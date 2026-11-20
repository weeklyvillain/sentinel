package reporter

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Automated fixes run in the issue's own repository: Sentinel adds the fix
// label to the issue, the repository's workflow (Claude Code in GitHub
// Actions, see deploy/github-workflow-sentinel-autofix.yml) reacts to it,
// fixes the error and opens a pull request that says "Fixes #N". Sentinel
// follows that pull request through the issue's timeline; when it is merged
// GitHub closes the issue, which resolves the group as usual.

// DefaultFixLabel starts the fix workflow.
const DefaultFixLabel = "sentinel-autofix"

const (
	// fixNoPRAfter: without a pull request by then, the attempt counts as
	// failed (the workflow is missing, failed, or decided not to change code).
	fixNoPRAfter = 2 * time.Hour
	// fixCheckEvery: how often pending attempts are looked up on GitHub.
	fixCheckEvery = 2 * time.Minute
)

// ErrCannotFix is returned for groups that cannot get a fix attempt.
var ErrCannotFix = errors.New("cannot request a fix")

// fixable reports whether a group's kind is a code error worth fixing.
func fixable(g store.Group) bool { return g.Kind == event.KindLog || g.Kind == event.KindSpan }

func (r *Reporter) fixLabel() string {
	if r.Cfg.FixLabel != "" {
		return r.Cfg.FixLabel
	}
	return DefaultFixLabel
}

// autofixOn returns the services with automatic fixing switched on.
func (r *Reporter) autofixOn(ctx context.Context) (map[string]bool, error) {
	svcs, err := r.Store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	on := map[string]bool{}
	for _, sv := range svcs {
		if sv.Autofix {
			on[sv.Name] = true
		}
	}
	return on, nil
}

// RequestFix asks the fix workflow to fix a group's issue (the dashboard's
// "Fix it" button).
func (r *Reporter) RequestFix(ctx context.Context, id int64, by string) (store.Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.Store.GetGroupByID(ctx, id)
	if err != nil {
		return g, err
	}
	switch {
	case !fixable(g):
		return g, fmt.Errorf("%w: only errors can be fixed automatically, not %s issues", ErrCannotFix, g.Kind)
	case g.IssueNumber == 0:
		return g, fmt.Errorf("%w: no GitHub issue has been filed for it yet", ErrCannotFix)
	case g.Status != store.StatusOpen:
		return g, fmt.Errorf("%w: the issue is %s", ErrCannotFix, g.Status)
	case g.FixState == store.FixRequested || g.FixState == store.FixPROpen || g.FixState == store.FixQueued || g.FixState == store.FixRunning:
		return g, fmt.Errorf("%w: a fix is already in progress", ErrCannotFix)
	}
	gh, _, err := r.clientFor(ctx, g)
	if err != nil {
		return g, err
	}
	if err := r.requestFix(ctx, gh, g, byWho(by)); err != nil {
		return g, err
	}
	return r.Store.GetGroupByID(ctx, id)
}

// requestFix labels the issue so the repository's workflow starts.
func (r *Reporter) requestFix(ctx context.Context, gh github.API, g store.Group, why string) error {
	if r.fixerMode() {
		return r.queueFix(ctx, gh, g, why)
	}
	fx, ok := gh.(github.FixAPI)
	if !ok {
		return fmt.Errorf("%w: this GitHub connection cannot label issues", ErrCannotFix)
	}
	label := r.fixLabel()
	if err := fx.Relabel(ctx, g.IssueNumber, label); err != nil {
		return err
	}
	attempt := ""
	if g.FixAttempts > 0 {
		attempt = fmt.Sprintf(" (attempt %d)", g.FixAttempts+1)
	}
	body := fmt.Sprintf("🤖 Automated fix requested %s%s: added the `%s` label, which starts the fix workflow in this repository. "+
		"It should open a pull request that says `Fixes #%d`; Sentinel links it here.", why, attempt, label, g.IssueNumber)
	if err := gh.Comment(ctx, g.IssueNumber, body); err != nil {
		return err
	}
	now := r.now()
	if err := r.Store.SetFixRequested(ctx, g.ID, store.FixRequested, now); err != nil {
		return err
	}
	r.Log.Info("fix requested", "issue", g.IssueNumber, "repo", g.Repo, "service", g.Service, "why", why)
	r.notify(notify.Event{Type: notify.FixRequested, Service: g.Service, Group: groupInfo(g),
		Issue:   &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL},
		Message: fmt.Sprintf("Automated fix requested for %s in %s", g.Title, g.Service)})
	return nil
}

// autofixNew requests a fix for a newly filed issue if its service has
// automatic fixing on. Failures are logged; the issue stays filed.
func (r *Reporter) autofixNew(ctx context.Context, gh github.API, g store.Group, why string) {
	if !fixable(g) {
		return
	}
	// An investigation that judged the error not worth fixing (or not
	// fixable in code) saves the fix attempt.
	if g.TriageVerdict != "" && g.TriageVerdict != ai.VerdictFix {
		r.Log.Info("no automatic fix: the investigation's verdict is "+g.TriageVerdict, "issue", g.IssueNumber)
		return
	}
	on, err := r.autofixOn(ctx)
	if err != nil || !on[g.Service] {
		return
	}
	if err := r.requestFix(ctx, gh, g, why); err != nil {
		r.Log.Warn("automatic fix request failed", "issue", g.IssueNumber, "err", err)
	}
}

// trackFixes follows pending fix attempts: the pull request the workflow
// opened, and whether it was merged.
func (r *Reporter) trackFixes(ctx context.Context) error {
	if err := r.failStaleFixes(ctx); err != nil {
		return err
	}
	groups, err := r.Store.FixPending(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	for _, g := range groups {
		if now.Sub(g.FixCheckedAt) < fixCheckEvery || dryRun(g) {
			if dryRun(g) && g.FixState == store.FixRequested && now.Sub(g.FixRequestedAt) > fixNoPRAfter {
				r.Store.SetFixState(ctx, g.ID, store.FixNoPR, 0, "", now)
			}
			continue
		}
		if err := r.Store.MarkFixChecked(ctx, g.ID, now); err != nil {
			return err
		}
		gh, _, err := r.clientFor(ctx, g)
		if err != nil {
			r.Log.Warn("fix tracking: github client", "repo", g.Repo, "err", err)
			continue
		}
		fx, ok := gh.(github.FixAPI)
		if !ok {
			continue
		}
		prs, err := fx.LinkedPRs(ctx, g.IssueNumber)
		if err != nil {
			r.Log.Warn("fix tracking: linked pull requests", "repo", g.Repo, "issue", g.IssueNumber, "err", err)
			continue
		}
		if err := r.updateFix(ctx, g, prs, now); err != nil {
			return err
		}
	}
	return nil
}

// updateFix moves a fix attempt along from the issue's linked pull requests.
// Only pull requests can count that were opened for this attempt, i.e. are
// not the one an earlier attempt already recorded.
func (r *Reporter) updateFix(ctx context.Context, g store.Group, prs []github.PullRef, now time.Time) error {
	var pr *github.PullRef
	for i := range prs {
		p := prs[i]
		// The newest wins; a merged one wins over an open one.
		if pr == nil || p.Merged || (!pr.Merged && p.Number > pr.Number) {
			pr = &prs[i]
		}
	}
	info := &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL}
	switch {
	case pr == nil:
		if now.Sub(g.FixRequestedAt) > fixNoPRAfter {
			r.Log.Info("no fix pull request appeared", "issue", g.IssueNumber, "repo", g.Repo)
			return r.Store.SetFixState(ctx, g.ID, store.FixNoPR, 0, "", now)
		}
		return nil
	case pr.Merged:
		if g.FixState != store.FixMerged {
			r.Log.Info("fix merged", "issue", g.IssueNumber, "pr", pr.Number)
			r.notify(notify.Event{Type: notify.FixMerged, Service: g.Service, Group: groupInfo(g), Issue: info,
				Message: fmt.Sprintf("Fix for %s in %s was merged: %s", g.Title, g.Service, pr.URL)})
		}
		return r.Store.SetFixState(ctx, g.ID, store.FixMerged, pr.Number, pr.URL, now)
	case pr.State == "closed":
		return r.Store.SetFixState(ctx, g.ID, store.FixPRClosed, pr.Number, pr.URL, now)
	default:
		if g.FixState != store.FixPROpen || g.FixPRNumber != pr.Number {
			r.Log.Info("fix pull request opened", "issue", g.IssueNumber, "pr", pr.Number)
			r.notify(notify.Event{Type: notify.FixOpened, Service: g.Service, Group: groupInfo(g), Issue: info,
				Message: fmt.Sprintf("Fix proposed for %s in %s: %s", g.Title, g.Service, pr.URL)})
		}
		return r.Store.SetFixState(ctx, g.ID, store.FixPROpen, pr.Number, pr.URL, now)
	}
}
