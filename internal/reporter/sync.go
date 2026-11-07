package reporter

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// GitHub is the source of truth for whether an issue is open. Sentinel
// mirrors it both ways:
//
//   - closed on GitHub as completed → resolved here (a later event reopens it
//     as a regression); closed as not planned → ignored here;
//     reopened on GitHub → open here.
//   - closed, ignored or reopened in the dashboard → the same on GitHub.

// syncIdle is how long after its last event a resolved or ignored group is
// still checked for being reopened on GitHub.
const syncIdle = 30 * 24 * time.Hour

// syncGitHub checks the GitHub state of every issue at most once per
// Cfg.StateCheck and mirrors changes made there.
func (r *Reporter) syncGitHub(ctx context.Context) error {
	groups, err := r.Store.ListGroups(ctx, store.StatusOpen, store.StatusResolved, store.StatusIgnored)
	if err != nil {
		return err
	}
	now := r.now()
	for _, g := range groups {
		if g.IssueNumber == 0 || now.Sub(g.LastCheckedAt) < r.Cfg.StateCheck {
			continue
		}
		if g.Status != store.StatusOpen && now.Sub(g.LastSeen) > syncIdle {
			continue
		}
		// Filed while DRY_RUN was on: there is no such issue on GitHub.
		if strings.HasPrefix(g.IssueURL, "dry-run://") {
			continue
		}
		// Checked (or tried) now: a failure waits for the next interval
		// instead of being retried every tick.
		if err := r.Store.MarkChecked(ctx, g.ID, now); err != nil {
			return err
		}
		gh, _, err := r.clientFor(ctx, g)
		if err != nil {
			r.Log.Warn("github sync: no client", "repo", g.Repo, "issue", g.IssueNumber, "err", err)
			continue
		}
		iss, err := gh.GetIssue(ctx, g.IssueNumber)
		if err != nil {
			r.Log.Warn("github sync: get issue", "repo", g.Repo, "issue", g.IssueNumber, "err", err)
			continue
		}
		switch {
		case g.Status == store.StatusOpen && iss.Closed():
			if err := r.closedOnGitHub(ctx, gh, g, iss, now); err != nil {
				r.Log.Error("sync closed issue", "issue", g.IssueNumber, "err", err)
			}
		case g.Status != store.StatusOpen && !iss.Closed():
			// Reopened by someone on GitHub: it is theirs to track again.
			if err := r.Store.SetStatus(ctx, g.ID, store.StatusOpen, g.Count); err != nil {
				return err
			}
			r.Log.Info("issue reopened on GitHub", "repo", g.Repo, "issue", g.IssueNumber, "service", g.Service)
		}
	}
	return nil
}

// closedOnGitHub handles an open group whose issue was closed on GitHub.
// Events after the close mean it happened again (a regression); events from
// before the close were seen by whoever closed it.
func (r *Reporter) closedOnGitHub(ctx context.Context, gh github.API, g store.Group, iss github.Issue, now time.Time) error {
	if iss.StateReason == "not_planned" {
		r.Log.Info("issue closed as not planned on GitHub; ignoring group", "repo", g.Repo, "issue", g.IssueNumber)
		return r.Store.SetStatus(ctx, g.ID, store.StatusIgnored, g.Count)
	}
	if iss.ClosedAt != nil && g.LastSeen.After(*iss.ClosedAt) {
		off, err := r.issuesOff(ctx)
		if err != nil {
			return err
		}
		if !off[g.Service] {
			samples, err := r.Store.Samples(ctx, g.ID)
			if err != nil {
				return err
			}
			return r.regress(ctx, gh, g, samples, now)
		}
	}
	r.Log.Info("issue closed on GitHub; resolved", "repo", g.Repo, "issue", g.IssueNumber, "service", g.Service)
	return r.Store.SetStatus(ctx, g.ID, store.StatusResolved, g.Count)
}

// Dashboard actions on a group.
const (
	ActionResolve = "resolve" // close the issue as completed
	ActionIgnore  = "ignore"  // close the issue as not planned; stay quiet
	ActionReopen  = "reopen"  // reopen the issue (or un-ignore)
)

// ErrBadAction is returned for an unknown action.
var ErrBadAction = errors.New(`action must be "resolve", "ignore" or "reopen"`)

// SetGroupStatus applies a dashboard action to a group and its GitHub issue.
// GitHub is changed first: if that fails, nothing changes here.
func (r *Reporter) SetGroupStatus(ctx context.Context, id int64, action, by string) (store.Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.Store.GetGroupByID(ctx, id)
	if err != nil {
		return g, err
	}
	now := r.now()
	who := "from the Sentinel dashboard"
	if by != "" {
		who = fmt.Sprintf("by @%s from the Sentinel dashboard", by)
	}
	var status string
	switch action {
	case ActionResolve:
		status = store.StatusResolved
	case ActionIgnore:
		status = store.StatusIgnored
	case ActionReopen:
		status = store.StatusPending
		if g.IssueNumber > 0 {
			status = store.StatusOpen
		}
	default:
		return g, ErrBadAction
	}

	// A dry-run issue exists only in the log: change the group, not GitHub.
	if g.IssueNumber > 0 && !strings.HasPrefix(g.IssueURL, "dry-run://") {
		gh, _, err := r.clientFor(ctx, g)
		if err != nil {
			return g, err
		}
		iss, err := gh.GetIssue(ctx, g.IssueNumber)
		if err != nil {
			return g, err
		}
		switch {
		case action == ActionReopen && iss.Closed():
			if err := gh.Reopen(ctx, g.IssueNumber); err != nil {
				return g, err
			}
			if err := gh.Comment(ctx, g.IssueNumber, "Reopened "+who+"."); err != nil {
				return g, err
			}
		case action != ActionReopen && !iss.Closed():
			reason, verb := "completed", "Closed"
			if action == ActionIgnore {
				reason, verb = "not_planned", "Closed as not planned (Sentinel will stay quiet about it)"
			}
			if err := gh.Comment(ctx, g.IssueNumber, verb+" "+who+"."); err != nil {
				return g, err
			}
			if err := gh.Close(ctx, g.IssueNumber, reason); err != nil {
				return g, err
			}
		}
	}
	if err := r.Store.SetStatus(ctx, g.ID, status, g.Count); err != nil {
		return g, err
	}
	if err := r.Store.MarkChecked(ctx, g.ID, now); err != nil {
		return g, err
	}
	r.Log.Info("group status changed in dashboard", "group", g.ID, "action", action, "by", by, "issue", g.IssueNumber)
	return r.Store.GetGroupByID(ctx, g.ID)
}
