package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/fixjob"
	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// The fixer container (cmd/sentinel-fixer) runs Claude Code next to
// Sentinel. In this mode Sentinel queues fix requests instead of labelling
// the issue; the fixer claims them over HTTP, gets a token that can push to
// that one repository, and reports back the pull request it opened.

// Fix modes.
const (
	FixModeActions = "actions" // label the issue; the repository's workflow fixes it
	FixModeFixer   = "fixer"   // queue it for the sentinel-fixer container
)

// fixRunTimeout: a running job with no result by then is given up (the fixer
// restarted or died); it can be requested again.
const fixRunTimeout = time.Hour

// ErrStaleJob is returned for a result of a job that is no longer running.
var ErrStaleJob = errors.New("this fix job is no longer running")

// FixMode is how fixes run now: Cfg.FixMode (AUTOFIX_MODE) until the
// dashboard sets one.
func (r *Reporter) FixMode() string {
	if m := r.fixMode.Load(); m != nil {
		return *m
	}
	return r.Cfg.FixMode
}

// SetFixMode switches between FixModeActions and FixModeFixer.
func (r *Reporter) SetFixMode(m string) { r.fixMode.Store(&m) }

func (r *Reporter) fixerMode() bool { return r.FixMode() == FixModeFixer }

// queueFix puts a group in the fixer's queue.
func (r *Reporter) queueFix(ctx context.Context, gh github.API, g store.Group, why string) error {
	body := fmt.Sprintf("🤖 Automated fix requested %s: queued for Sentinel's fixer, which runs Claude Code on this repository "+
		"and opens a pull request that says `Fixes #%d` if it finds a fix.", why, g.IssueNumber)
	if err := gh.Comment(ctx, g.IssueNumber, body); err != nil {
		return err
	}
	if err := r.Store.SetFixRequested(ctx, g.ID, store.FixQueued, r.now()); err != nil {
		return err
	}
	r.Log.Info("fix queued", "issue", g.IssueNumber, "repo", g.Repo, "service", g.Service, "why", why)
	r.notify(notify.Event{Type: notify.FixRequested, Service: g.Service, Group: groupInfo(g),
		Issue:   &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL},
		Message: fmt.Sprintf("Automated fix queued for %s in %s", g.Title, g.Service)})
	return nil
}

// ClaimFixJob gives the fixer its next job, or nil when there is none:
// investigations first (issues wait for them), then fixes.
func (r *Reporter) ClaimFixJob(ctx context.Context) (*fixjob.Job, error) {
	if remoteAI(r.AI) {
		job, err := r.claimAnalysis(ctx)
		if err != nil || job != nil {
			return job, err
		}
	}
	pa, ok := r.Router.(PushAccessor)
	if !ok {
		return nil, fmt.Errorf("%w: this GitHub connection cannot hand out push access", ErrCannotFix)
	}
	for i := 0; i < 5; i++ {
		g, found, err := r.Store.ClaimFix(ctx, r.now())
		if err != nil || !found {
			return nil, err
		}
		access, err := pa.PushAccess(ctx, g.Repo, true)
		if err != nil {
			r.Log.Warn("fix job: no push access", "repo", g.Repo, "err", err)
			if err := r.Store.SetFixOutcome(ctx, g.ID, store.FixFailed, 0, "", err.Error(), r.now()); err != nil {
				return nil, err
			}
			continue
		}
		samples, err := r.Store.Samples(ctx, g.ID)
		if err != nil {
			return nil, err
		}
		brief := FixBrief(g, samples, r.issueExtra(ctx, g, samples))
		if a, err := r.Store.LatestAnalysis(ctx, g.ID); err == nil && a.Status == store.AnalysisDone {
			var d ai.Draft
			if json.Unmarshal([]byte(a.Draft), &d) == nil && d.LikelyCause != "" {
				brief += "\n## Earlier investigation (may be wrong)\n\n" + d.Summary + "\n\nLikely cause: " + d.LikelyCause + "\n"
			}
		}
		job := &fixjob.Job{
			Kind: fixjob.KindFix, ID: g.ID, Attempt: g.FixAttempts, Repo: g.Repo, IssueNumber: g.IssueNumber, IssueURL: g.IssueURL,
			Title: g.Title, Brief: brief,
			Branch: fmt.Sprintf("sentinel-fix-%d-%d", g.IssueNumber, g.FixAttempts), CloneURL: access.CloneURL, APIURL: access.APIURL,
			Token: access.Token, DryRun: access.DryRun || dryRun(g),
			Related: r.relatedRepos(ctx, g, samples, pa),
		}
		r.Log.Info("fix job claimed", "group", g.ID, "issue", g.IssueNumber, "repo", g.Repo, "attempt", g.FixAttempts, "related", len(job.Related))
		return job, nil
	}
	return nil, nil
}

// maxRelatedRepos bounds how many other repositories one fix may touch.
const maxRelatedRepos = 3

// relatedRepos lists the repositories of the other services in the error's
// traces (a frontend sending what the backend rejects, a caller and the
// service it calls), with push access to each: a fix may need to change
// more than one of them.
func (r *Reporter) relatedRepos(ctx context.Context, g store.Group, samples []store.Sample, pa PushAccessor) []fixjob.RelatedRepo {
	traces := map[string]bool{}
	for _, s := range samples {
		if id := s.Event.TraceID; id != "" && len(traces) < 10 {
			traces[id] = true
		}
	}
	if len(traces) == 0 {
		return nil
	}
	services := map[string]bool{}
	for id := range traces {
		spans, err := r.Store.GetTrace(ctx, id)
		if err != nil {
			continue
		}
		for _, sp := range spans {
			if sp.Service != g.Service {
				services[sp.Service] = true
			}
		}
	}
	if len(services) == 0 {
		return nil
	}
	all, err := r.Store.ListServices(ctx)
	if err != nil {
		return nil
	}
	repoOf := map[string]string{}
	for _, s := range all {
		if s.Repo != "" {
			repoOf[s.Name] = s.Repo
		}
	}
	names := make([]string, 0, len(services))
	for s := range services {
		names = append(names, s)
	}
	sort.Strings(names)
	seen := map[string]bool{strings.ToLower(g.Repo): true}
	var out []fixjob.RelatedRepo
	for _, svc := range names {
		repo := repoOf[svc]
		if repo == "" || seen[strings.ToLower(repo)] {
			continue
		}
		seen[strings.ToLower(repo)] = true
		access, err := pa.PushAccess(ctx, repo, true)
		if err != nil {
			r.Log.Info("fix job: related repository left out", "repo", repo, "service", svc, "err", err)
			continue
		}
		out = append(out, fixjob.RelatedRepo{Repo: repo, Service: svc, CloneURL: access.CloneURL, Token: access.Token})
		if len(out) == maxRelatedRepos {
			break
		}
	}
	return out
}

// prLinks lists pull requests as markdown links.
func prLinks(prs []fixjob.PullRequest) string {
	var b strings.Builder
	for _, p := range prs {
		fmt.Fprintf(&b, "\n- %s#%d: %s", p.Repo, p.Number, p.URL)
	}
	return b.String()
}

// FixJobResult records what the fixer did and tells the issue.
func (r *Reporter) FixJobResult(ctx context.Context, id int64, res fixjob.Result) (store.Group, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	g, err := r.Store.GetGroupByID(ctx, id)
	if err != nil {
		return g, err
	}
	if g.FixState != store.FixRunning || res.Attempt != g.FixAttempts {
		return g, ErrStaleJob
	}
	now := r.now()
	info := &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL}
	var state, comment string
	switch res.Status {
	case fixjob.StatusPR:
		if res.PRURL == "" || res.PRNumber == 0 {
			return g, fmt.Errorf("a pull request result needs pr_number and pr_url")
		}
		state = store.FixPROpen
		comment = fmt.Sprintf("🛠️ Sentinel's fixer opened %s with a proposed fix.", res.PRURL)
		if len(res.PRs) > 1 {
			comment = fmt.Sprintf("🛠️ Sentinel's fixer proposes a fix across %d repositories. Review and merge them together:%s", len(res.PRs), prLinks(res.PRs))
		}
		r.notify(notify.Event{Type: notify.FixOpened, Service: g.Service, Group: groupInfo(g), Issue: info,
			Message: fmt.Sprintf("Fix proposed for %s in %s: %s", g.Title, g.Service, res.PRURL)})
	case fixjob.StatusNoFix:
		state = store.FixNoFix
		comment = "🤖 Sentinel's fixer looked into this with Claude Code but did not find a fix it was confident about."
		if s := neutralize(res.Summary); s != "" {
			comment += "\n\n" + s
		}
	case fixjob.StatusDryRun:
		state = store.FixNoFix
		res.Summary = "Dry run: a fix was made but not pushed.\n\n" + res.Diffstat + "\n\n" + res.Summary
	case fixjob.StatusFailed:
		state = store.FixFailed
		res.Summary = res.Error
		r.Log.Warn("fix job failed", "group", g.ID, "issue", g.IssueNumber, "err", res.Error)
	default:
		return g, fmt.Errorf("unknown status %q", res.Status)
	}
	if err := r.Store.SetFixOutcome(ctx, g.ID, state, res.PRNumber, res.PRURL, res.Summary, now); err != nil {
		return g, err
	}
	var prs []store.FixPR
	if res.Status == fixjob.StatusPR && len(res.PRs) > 1 {
		for _, p := range res.PRs {
			prs = append(prs, store.FixPR{Repo: p.Repo, Number: p.Number, URL: p.URL, State: "open"})
		}
	}
	if err := r.Store.SetFixPRs(ctx, g.ID, prs); err != nil {
		return g, err
	}
	if comment != "" && !dryRun(g) {
		if gh, _, err := r.clientFor(ctx, g); err == nil {
			if err := gh.Comment(ctx, g.IssueNumber, comment); err != nil {
				r.Log.Warn("fix result comment", "issue", g.IssueNumber, "err", err)
			}
		}
	}
	r.Log.Info("fix job done", "group", g.ID, "issue", g.IssueNumber, "status", res.Status, "pr", res.PRURL)
	return r.Store.GetGroupByID(ctx, id)
}

// failStaleFixes gives up on jobs the fixer never finished.
func (r *Reporter) failStaleFixes(ctx context.Context) error {
	stale, err := r.Store.StaleFixes(ctx, r.now().Add(-fixRunTimeout))
	if err != nil {
		return err
	}
	for _, g := range stale {
		r.Log.Warn("fix job timed out", "group", g.ID, "issue", g.IssueNumber)
		if err := r.Store.SetFixOutcome(ctx, g.ID, store.FixFailed, 0, "", "the fixer did not report back within an hour", r.now()); err != nil {
			return err
		}
	}
	return nil
}

// FixBrief describes a group for the fixing agent: what failed, where, the
// samples with stack traces, tags and breadcrumbs. All of it is redacted
// telemetry, and the agent is told to treat it as data.
func FixBrief(g store.Group, samples []store.Sample, x IssueExtra) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", g.Title)
	fmt.Fprintf(&b, "- Service: %s\n- Environment: %s\n- Kind: %s\n- Occurrences: %d (first %s, last %s)\n",
		g.Service, orDash(g.Env), g.Kind, g.Count, ts(g.FirstSeen), ts(g.LastSeen))
	if g.FirstRelease != "" {
		fmt.Fprintf(&b, "- First seen in release: %s (latest: %s)\n", g.FirstRelease, orDash(g.LastRelease))
	}
	if g.RegressedRelease != "" {
		fmt.Fprintf(&b, "- Came back after being fixed, in release %s\n", g.RegressedRelease)
	}
	if g.IssueURL != "" {
		fmt.Fprintf(&b, "- GitHub issue: %s\n", g.IssueURL)
	}
	if tags := tagLines(x.Tags); tags != "" {
		b.WriteString("\n## Tags\n\n" + tags)
	}
	if len(samples) > 0 {
		last := samples[len(samples)-1].Event
		b.WriteString("\n## Latest occurrence\n\n")
		b.WriteString(sampleBlock(last))
		if len(last.Attributes) > 0 {
			keys := make([]string, 0, len(last.Attributes))
			for k := range last.Attributes {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			b.WriteString("\nAttributes:\n\n")
			for _, k := range keys {
				fmt.Fprintf(&b, "- %s: %s\n", k, cell(last.Attributes[k]))
			}
		}
		if len(samples) > 1 {
			b.WriteString("\n## First occurrence\n\n")
			b.WriteString(sampleBlock(samples[0].Event))
		}
	}
	if len(x.Breadcrumbs) > 1 {
		b.WriteString("\n## Log lines before the latest occurrence\n\n")
		var lines strings.Builder
		for _, l := range x.Breadcrumbs {
			fmt.Fprintf(&lines, "%s %s %s %s\n", l.Time.UTC().Format("15:04:05.000"), l.SeverityText, l.Service, strings.ReplaceAll(l.Body, "\n", " "))
		}
		b.WriteString(codeBlock(strings.TrimRight(lines.String(), "\n")))
	}
	return b.String()
}
