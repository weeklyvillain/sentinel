package reporter

import (
	"context"
	"fmt"
	"strings"

	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

func dryRun(g store.Group) bool { return strings.HasPrefix(g.IssueURL, "dry-run://") }

func byWho(by string) string {
	if by != "" {
		return fmt.Sprintf("by @%s from the Sentinel dashboard", by)
	}
	return "from the Sentinel dashboard"
}

// SnoozeGroup ignores a group until the snooze ends (see store.Snooze).
func (r *Reporter) SnoozeGroup(ctx context.Context, id int64, sn store.Snooze, by string) (store.Group, error) {
	var parts []string
	if !sn.Until.IsZero() {
		parts = append(parts, "until "+ts(sn.Until))
	}
	if sn.Count > 0 {
		parts = append(parts, fmt.Sprintf("until it happens %d more times", sn.Count))
	}
	if sn.Release {
		parts = append(parts, "until a new release sends it")
	}
	note := ""
	if len(parts) > 0 {
		note = "Snoozed " + strings.Join(parts, " or ") + "; Sentinel reopens it then."
	}
	g, err := r.setGroupStatus(ctx, id, ActionIgnore, by, note)
	if err != nil {
		return g, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.Store.SetSnooze(ctx, id, sn); err != nil {
		return g, err
	}
	return r.Store.GetGroupByID(ctx, id)
}

// endSnoozes brings back ignored groups whose snooze is over: reopened when
// they happened meanwhile, otherwise resolved (a later event reopens them).
func (r *Reporter) endSnoozes(ctx context.Context) error {
	groups, err := r.Store.ListGroups(ctx, store.StatusIgnored)
	if err != nil {
		return err
	}
	off, err := r.issuesOff(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	for _, g := range groups {
		why, over := store.SnoozeOver(g, now)
		if !over {
			continue
		}
		hasIssue := g.IssueNumber > 0 && !dryRun(g)
		if hasIssue && off[g.Service] {
			continue // nothing may change on GitHub; wait
		}
		fresh := g.NewSinceReport() > 0
		if hasIssue {
			gh, _, err := r.clientFor(ctx, g)
			if err != nil {
				r.Log.Error("github client", "repo", g.Repo, "err", err)
				continue
			}
			if fresh {
				if err := gh.Reopen(ctx, g.IssueNumber); err != nil {
					r.Log.Error("end snooze: reopen", "issue", g.IssueNumber, "err", err)
					continue
				}
				samples, _ := r.Store.Samples(ctx, g.ID)
				body := fmt.Sprintf("**Snooze ended** because %s. %d new occurrence(s) since it was snoozed (%d total), last seen %s.\n\n",
					why, g.NewSinceReport(), g.Count, ts(g.LastSeen))
				if len(samples) > 0 {
					body += "Latest sample:\n\n" + sampleBlock(samples[len(samples)-1].Event)
				}
				if err := gh.Comment(ctx, g.IssueNumber, body); err != nil {
					return err
				}
			} else {
				// Still closed; mark it completed so that the next event
				// reopens it as a regression instead of being ignored.
				if err := gh.Comment(ctx, g.IssueNumber, fmt.Sprintf("Snooze ended because %s, with no new occurrences. If it happens again this issue is reopened.", why)); err != nil {
					return err
				}
				if err := gh.Close(ctx, g.IssueNumber, "completed"); err != nil {
					return err
				}
			}
		}
		if err := r.Store.ClearSnooze(ctx, g.ID); err != nil {
			return err
		}
		status := store.StatusResolved
		switch {
		case fresh && g.IssueNumber > 0:
			status = store.StatusOpen
		case fresh:
			status = store.StatusPending
		}
		reported := g.Count
		if status == store.StatusPending {
			reported = -1
		}
		if err := r.Store.SetStatus(ctx, g.ID, status, reported); err != nil {
			return err
		}
		if status == store.StatusOpen {
			if err := r.Store.MarkReported(ctx, g.ID, g.Count, now); err != nil {
				return err
			}
			r.notify(notify.Event{Type: notify.IssueRegression, Service: g.Service, Group: groupInfo(g),
				Issue:   &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL},
				Message: fmt.Sprintf("Snooze ended for %s in %s: %s", g.Title, g.Service, why)})
		}
		r.Log.Info("snooze ended", "group", g.ID, "why", why, "status", status)
	}
	return nil
}

// MergeGroups merges sources into target. Their GitHub issues are closed as
// not planned with a pointer to the target.
func (r *Reporter) MergeGroups(ctx context.Context, targetID int64, sourceIDs []int64, by string) (store.Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	merged, err := r.Store.Merge(ctx, targetID, sourceIDs, r.now())
	if err != nil {
		return store.Group{}, err
	}
	target, err := r.Store.GetGroupByID(ctx, targetID)
	if err != nil {
		return target, err
	}
	where := fmt.Sprintf("Sentinel group %d (%s)", target.ID, cell(target.Title))
	if target.IssueNumber > 0 {
		where = fmt.Sprintf("%s#%d", target.Repo, target.IssueNumber)
	}
	var refs []string
	for _, src := range merged {
		if src.IssueNumber == 0 || dryRun(src) {
			continue
		}
		refs = append(refs, fmt.Sprintf("%s#%d", src.Repo, src.IssueNumber))
		gh, _, err := r.clientFor(ctx, src)
		if err != nil {
			r.Log.Warn("merge: github client", "repo", src.Repo, "err", err)
			continue
		}
		if err := gh.Comment(ctx, src.IssueNumber, fmt.Sprintf("Merged into %s %s. Sentinel tracks this error there now.", where, byWho(by))); err != nil {
			r.Log.Warn("merge: comment", "issue", src.IssueNumber, "err", err)
		}
		if src.Status == store.StatusOpen {
			if err := gh.Close(ctx, src.IssueNumber, "not_planned"); err != nil {
				r.Log.Warn("merge: close", "issue", src.IssueNumber, "err", err)
			}
		}
	}
	if target.IssueNumber > 0 && !dryRun(target) && len(merged) > 0 {
		if gh, _, err := r.clientFor(ctx, target); err == nil {
			msg := fmt.Sprintf("%d other error group(s) were merged into this issue %s", len(merged), byWho(by))
			if len(refs) > 0 {
				msg += ": " + strings.Join(refs, ", ")
			}
			if err := gh.Comment(ctx, target.IssueNumber, msg+fmt.Sprintf(". %d occurrences in total now.", target.Count)); err != nil {
				r.Log.Warn("merge: comment target", "issue", target.IssueNumber, "err", err)
			}
		}
	}
	r.Log.Info("merged groups", "target", targetID, "sources", len(merged), "by", by)
	return target, nil
}

// UnmergeGroup splits a merged fingerprint off again. If it had an issue,
// that issue is reopened for it.
func (r *Reporter) UnmergeGroup(ctx context.Context, groupID int64, fp, by string) (store.Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	aliases, err := r.Store.Aliases(ctx, groupID)
	if err != nil {
		return store.Group{}, err
	}
	var alias *store.Alias
	for i := range aliases {
		if aliases[i].Fingerprint == fp {
			alias = &aliases[i]
		}
	}
	if alias == nil {
		return store.Group{}, store.ErrNotFound
	}
	status := store.StatusPending
	if alias.IssueNumber > 0 {
		status = store.StatusOpen
	}
	g, _, err := r.Store.Unmerge(ctx, groupID, fp, status)
	if err != nil {
		return g, err
	}
	if g.IssueNumber > 0 && !dryRun(g) {
		gh, _, err := r.clientFor(ctx, g)
		if err == nil {
			if err := gh.Reopen(ctx, g.IssueNumber); err != nil {
				r.Log.Warn("unmerge: reopen", "issue", g.IssueNumber, "err", err)
			}
			if err := gh.Comment(ctx, g.IssueNumber, "Split off again into its own group "+byWho(by)+"; Sentinel tracks this issue again."); err != nil {
				r.Log.Warn("unmerge: comment", "issue", g.IssueNumber, "err", err)
			}
		}
		if err := r.Store.MarkChecked(ctx, g.ID, r.now()); err != nil {
			return g, err
		}
	}
	r.Log.Info("unmerged group", "from", groupID, "new", g.ID, "by", by)
	return g, nil
}

// issueExtra is the context added to new issues: tags and breadcrumbs.
func (r *Reporter) issueExtra(ctx context.Context, g store.Group, samples []store.Sample) IssueExtra {
	var x IssueExtra
	x.Tags, _ = r.Store.GroupTags(ctx, g.ID, 3)
	if len(samples) > 0 {
		x.Breadcrumbs, _, _ = r.Store.Breadcrumbs(ctx, samples[len(samples)-1].Event, 15)
	}
	return x
}
