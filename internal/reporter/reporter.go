package reporter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Config controls reporting behavior.
type Config struct {
	Interval         time.Duration // tick interval (default 10s)
	SettleWindow     time.Duration // wait this long after first_seen before filing (default 60s)
	MinEvents        int64         // min events before filing (default 1)
	UpdateInterval   time.Duration // at most one update comment per group per interval (default 30m)
	StateCheck       time.Duration // how often to poll an issue's state while events arrive (default 2m)
	MaxIssuesPerHour int           // cap on new issues per rolling hour (default 10; <0 = unlimited)
	DownAfter        time.Duration // silence before a service is down; 0 disables
	PerfSlowP95      time.Duration // p95 that makes an endpoint a performance issue; 0 disables
	FixLabel         string        // label that starts the repository's fix workflow (default sentinel-autofix)
	FixMode          string        // actions (label the issue) | fixer (the sentinel-fixer container)
}

func (c *Config) defaults() {
	if c.Interval <= 0 {
		c.Interval = 10 * time.Second
	}
	if c.SettleWindow < 0 {
		c.SettleWindow = 0
	}
	if c.MinEvents <= 0 {
		c.MinEvents = 1
	}
	if c.UpdateInterval <= 0 {
		c.UpdateInterval = 30 * time.Minute
	}
	if c.StateCheck <= 0 {
		c.StateCheck = 2 * time.Minute
	}
	if c.MaxIssuesPerHour == 0 {
		c.MaxIssuesPerHour = 10
	}
}

// Reporter is the background loop.
type Reporter struct {
	Cfg       Config
	Store     *store.Store
	Processor *Processor
	AI        ai.Provider
	// Analyzer, when set, drafts issue text in the background (the analysis
	// queue); issues are filed once their analysis finished. Without it the
	// reporter drafts inline (tests, simple setups).
	Analyzer  *Analyzer
	Router    Router
	Notify    notify.Notifier
	Log       *slog.Logger
	Now       func() time.Time
	StartedAt time.Time

	capLogged time.Time
	perfAt    time.Time
	// mu serializes Tick with uptime-check transitions.
	mu sync.Mutex
	// fixMode, once set from the dashboard, replaces Cfg.FixMode.
	fixMode atomic.Pointer[string]
}

// New builds a reporter. StartedAt defaults to now.
func New(cfg Config, st *store.Store, provider ai.Provider, router Router, log *slog.Logger) *Reporter {
	cfg.defaults()
	if log == nil {
		log = slog.Default()
	}
	r := &Reporter{Cfg: cfg, Store: st, AI: provider, Router: router, Notify: notify.Nop{}, Log: log, Now: time.Now}
	r.Processor = &Processor{Store: st, Log: log}
	r.StartedAt = r.Now()
	return r
}

func (r *Reporter) now() time.Time { return r.Now().UTC() }

// Run ticks until ctx is done.
func (r *Reporter) Run(ctx context.Context) {
	t := time.NewTicker(r.Cfg.Interval)
	defer t.Stop()
	for {
		r.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick runs one reporting pass.
func (r *Reporter) Tick(ctx context.Context) {
	r.mu.Lock()
	defer r.mu.Unlock()
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"down-detection", r.detectDown},
		{"recoveries", r.handleRecoveries},
		{"github-sync", r.syncGitHub},
		{"new-issues", r.createIssues},
		{"updates", r.updateOpen},
		{"resolved-regressions", r.reopenResolved},
		{"snoozes", r.endSnoozes},
		{"fixes", r.trackFixes},
		{"performance", r.detectPerf},
	}
	for _, s := range steps {
		if ctx.Err() != nil {
			return
		}
		if err := s.fn(ctx); err != nil {
			r.Log.Error("reporter step failed", "step", s.name, "err", err)
		}
	}
}

func (r *Reporter) detectDown(ctx context.Context) error {
	// Before the early returns: a service that is not watched (any more) must
	// not stay down, also when detection is off or still in its grace period.
	if err := r.Store.ClearUnwatchedDown(ctx); err != nil {
		return err
	}
	if r.Cfg.DownAfter <= 0 {
		return nil
	}
	now := r.now()
	// Grace period after startup: we may simply not have heard from anyone yet.
	if now.Sub(r.StartedAt) < r.Cfg.DownAfter {
		return nil
	}
	svcs, err := r.Store.ListServices(ctx)
	if err != nil {
		return err
	}
	for _, sv := range svcs {
		// Only services added on the Uptime page are watched for silence:
		// plenty of senders (batch jobs, quiet agents) go minutes without a
		// line and are not down.
		if !sv.Watch || sv.Down || now.Sub(sv.LastSeen) < r.Cfg.DownAfter {
			continue
		}
		ok, err := r.Store.MarkServiceDown(ctx, sv.Name, sv.LastSeen)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		r.Log.Warn("service down", "service", sv.Name, "last_seen", sv.LastSeen)
		if err := r.Store.RecordOutageStart(ctx, sv.Name, sv.LastSeen); err != nil {
			return err
		}
		g, err := r.Processor.Record(ctx, DownEvent(sv.Name, sv.Env, sv.LastSeen, now))
		if err != nil {
			return err
		}
		ev := notify.Event{Type: notify.ServiceDown, Service: sv.Name, Group: groupInfo(g),
			Message: fmt.Sprintf("%s stopped sending telemetry (last seen %s)", sv.Name, ts(sv.LastSeen))}
		if g.IssueNumber > 0 {
			ev.Issue = &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL}
		}
		r.notify(ev)
	}
	return nil
}

func (r *Reporter) handleRecoveries(ctx context.Context) error {
	svcs, err := r.Store.ListServices(ctx)
	if err != nil {
		return err
	}
	for _, sv := range svcs {
		if sv.Down || sv.RecoveredAt.IsZero() {
			continue
		}
		g, err := r.Store.GetGroup(ctx, DownFingerprint(sv.Name))
		if err != nil && err != store.ErrNotFound {
			return err
		}
		if err == nil {
			outage := sv.RecoveredAt.Sub(sv.DownSince).Round(time.Second)
			switch {
			case g.Status == store.StatusOpen && g.IssueNumber > 0:
				gh, _, err := r.clientFor(ctx, g)
				if err != nil {
					return err
				}
				body := recoveryComment(sv, outage)
				if err := gh.Comment(ctx, g.IssueNumber, body); err != nil {
					return err
				}
				if err := gh.Close(ctx, g.IssueNumber, "completed"); err != nil {
					return err
				}
				if err := r.Store.SetStatus(ctx, g.ID, store.StatusResolved, g.Count); err != nil {
					return err
				}
				r.Log.Info("service recovered; closed issue", "service", sv.Name, "issue", g.IssueNumber, "outage", outage)
			case g.Status == store.StatusPending:
				// Never filed (e.g. capped): nothing to close.
				if err := r.Store.SetStatus(ctx, g.ID, store.StatusResolved, g.Count); err != nil {
					return err
				}
				r.Log.Info("service recovered before an issue was filed", "service", sv.Name, "outage", outage)
			}
		}
		if err := r.Store.RecordOutageEnd(ctx, sv.Name, sv.RecoveredAt); err != nil {
			return err
		}
		if err := r.Store.ClearRecovery(ctx, sv.Name); err != nil {
			return err
		}
		ev := notify.Event{Type: notify.ServiceRecovered, Service: sv.Name,
			Message: fmt.Sprintf("%s is sending telemetry again after %s", sv.Name, sv.RecoveredAt.Sub(sv.DownSince).Round(time.Second))}
		if g, err := r.Store.GetGroup(ctx, DownFingerprint(sv.Name)); err == nil && g.IssueNumber > 0 {
			ev.Group = groupInfo(g)
			ev.Issue = &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL}
		}
		r.notify(ev)
	}
	return nil
}

func (r *Reporter) createIssues(ctx context.Context) error {
	groups, err := r.Store.ListGroups(ctx, store.StatusPending)
	if err != nil {
		return err
	}
	now := r.now()
	// Oldest first so a cap doesn't starve old groups.
	for i, j := 0, len(groups)-1; i < j; i, j = i+1, j-1 {
		groups[i], groups[j] = groups[j], groups[i]
	}
	off, err := r.issuesOff(ctx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		if off[g.Service] {
			if err := r.Store.SetBlockReason(ctx, g.ID, "issue creation is turned off for service "+g.Service); err != nil {
				return err
			}
			continue
		}
		if g.Kind != event.KindDown {
			if now.Sub(g.FirstSeen) < r.Cfg.SettleWindow || g.Count < r.Cfg.MinEvents {
				continue
			}
		}
		if r.Cfg.MaxIssuesPerHour > 0 {
			n, err := r.Store.IssuesCreatedSince(ctx, now.Add(-time.Hour))
			if err != nil {
				return err
			}
			if n >= r.Cfg.MaxIssuesPerHour {
				if now.Sub(r.capLogged) > 5*time.Minute {
					r.Log.Warn("hourly issue cap reached; groups stay pending", "cap", r.Cfg.MaxIssuesPerHour)
					r.capLogged = now
				}
				return nil
			}
		}
		var pre *ai.Draft
		// Outages skip the queue: they must be filed now, and the template
		// describes them well.
		if r.Analyzer != nil && g.Kind != event.KindDown {
			d, ready, err := r.queuedDraft(ctx, g, now)
			if err != nil {
				return err
			}
			if !ready {
				continue
			}
			pre = &d
		}
		if pre != nil && pre.Triage != nil {
			t := pre.Triage
			if err := r.Store.SetTriage(ctx, g.ID, t.Verdict, t.Severity, t.Reason); err != nil {
				return err
			}
			g.TriageVerdict = t.Verdict
			if t.Verdict == ai.VerdictIgnore && skipNoise(r.AI) {
				r.Log.Info("not filed: the investigation says it is not worth fixing", "group", g.ID, "service", g.Service, "reason", t.Reason)
				if err := r.Store.SetStatus(ctx, g.ID, store.StatusIgnored, g.Count); err != nil {
					return err
				}
				continue
			}
		}
		if err := r.fileGroup(ctx, g, now, pre); err != nil {
			r.Log.Error("file issue", "fingerprint", g.Fingerprint, "err", err)
		}
	}
	return nil
}

func (r *Reporter) fileGroup(ctx context.Context, g store.Group, now time.Time, pre *ai.Draft) error {
	samples, err := r.Store.Samples(ctx, g.ID)
	if err != nil {
		return err
	}
	marker := Marker(g.Fingerprint)

	repo, err := r.Router.RepoFor(ctx, g.Service)
	if err != nil {
		if errors.Is(err, ErrNoRepo) {
			return r.Store.SetBlockReason(ctx, g.ID, err.Error())
		}
		return err
	}
	gh, err := r.Router.Client(ctx, repo, g.Service)
	if err != nil {
		return err
	}

	// Dedupe against GitHub in case the DB was lost.
	existing, err := gh.FindByMarker(ctx, marker)
	if err != nil {
		return err
	}
	if existing != nil {
		if err := r.Store.MarkIssueCreated(ctx, g.ID, repo, existing.Number, existing.HTMLURL, g.CountAtLastReport, now, false); err != nil {
			return err
		}
		r.Log.Info("adopted existing issue", "fingerprint", g.Fingerprint, "issue", existing.Number)
		g.Status, g.IssueNumber, g.IssueURL, g.Repo = store.StatusOpen, existing.Number, existing.HTMLURL, repo
		return r.syncOpen(ctx, gh, g, samples, now, true)
	}

	var draft ai.Draft
	if pre != nil {
		draft = *pre
	} else if draft, err = r.AI.Draft(ctx, inputFor(g, samples)); err != nil {
		r.Log.Warn("AI draft failed, using template", "err", err)
		draft, _ = ai.Template{}.Draft(ctx, inputFor(g, samples))
		draft.Provider = "template (fallback after provider error)"
	}
	provider := r.AI.Name()
	if draft.Provider != "" {
		provider = draft.Provider
	}
	draft = sanitizeDraft(draft)
	body := IssueBody(g, draft, samples, provider, r.issueExtra(ctx, g, samples))
	iss, err := gh.CreateIssue(ctx, draft.Title, body)
	if err != nil {
		return err
	}
	if err := r.Store.MarkIssueCreated(ctx, g.ID, repo, iss.Number, iss.HTMLURL, g.Count, now, true); err != nil {
		return err
	}
	r.Log.Info("created issue", "repo", repo, "issue", iss.Number, "url", iss.HTMLURL, "service", g.Service, "count", g.Count, "title", draft.Title)
	g.Repo, g.IssueNumber, g.IssueURL, g.Status = repo, iss.Number, iss.HTMLURL, store.StatusOpen
	r.autofixNew(ctx, gh, g, "automatically (auto-fix is on for "+g.Service+")")
	r.notify(notify.Event{Type: notify.IssueCreated, Service: g.Service, Group: groupInfo(g),
		Issue:   &notify.IssueInfo{Repo: repo, Number: iss.Number, URL: iss.HTMLURL, Title: draft.Title},
		Message: fmt.Sprintf("New issue for %s: %s (%d occurrences)", g.Service, draft.Title, g.Count)})
	return nil
}

func (r *Reporter) updateOpen(ctx context.Context) error {
	groups, err := r.Store.ListGroups(ctx, store.StatusOpen)
	if err != nil {
		return err
	}
	off, err := r.issuesOff(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	for _, g := range groups {
		if g.IssueNumber == 0 || g.NewSinceReport() <= 0 || off[g.Service] {
			continue
		}
		due := now.Sub(g.LastReportedAt) >= r.Cfg.UpdateInterval
		check := now.Sub(g.LastCheckedAt) >= r.Cfg.StateCheck
		if !due && !check {
			continue
		}
		samples, err := r.Store.Samples(ctx, g.ID)
		if err != nil {
			return err
		}
		gh, _, err := r.clientFor(ctx, g)
		if err != nil {
			r.Log.Error("github client", "repo", g.Repo, "err", err)
			continue
		}
		if err := r.syncOpen(ctx, gh, g, samples, now, due); err != nil {
			r.Log.Error("update issue", "issue", g.IssueNumber, "err", err)
		}
	}
	return nil
}

// syncOpen reconciles an open group with its issue: regression if the issue
// was closed as completed, ignore if not_planned, otherwise an update comment
// when due.
func (r *Reporter) syncOpen(ctx context.Context, gh github.API, g store.Group, samples []store.Sample, now time.Time, due bool) error {
	iss, err := gh.GetIssue(ctx, g.IssueNumber)
	if err != nil {
		return err
	}
	if iss.Closed() {
		return r.closedOnGitHub(ctx, gh, g, iss, now)
	}
	if !due || g.NewSinceReport() <= 0 {
		return r.Store.MarkChecked(ctx, g.ID, now)
	}
	if err := gh.Comment(ctx, g.IssueNumber, UpdateComment(g, samples)); err != nil {
		return err
	}
	r.Log.Info("posted update", "issue", g.IssueNumber, "new", g.NewSinceReport(), "total", g.Count)
	r.notify(notify.Event{Type: notify.IssueUpdated, Service: g.Service, Group: groupInfo(g),
		Issue:   &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL},
		Message: fmt.Sprintf("%d new occurrence(s) of %s in %s (%d total)", g.NewSinceReport(), g.Title, g.Service, g.Count)})
	return r.Store.MarkReported(ctx, g.ID, g.Count, now)
}

func (r *Reporter) regress(ctx context.Context, gh github.API, g store.Group, samples []store.Sample, now time.Time) error {
	if err := gh.Reopen(ctx, g.IssueNumber); err != nil {
		return err
	}
	if err := gh.Comment(ctx, g.IssueNumber, RegressionComment(g, samples)); err != nil {
		return err
	}
	if err := r.Store.SetStatus(ctx, g.ID, store.StatusOpen, -1); err != nil {
		return err
	}
	if err := r.Store.SetRegressedRelease(ctx, g.ID, g.LastRelease); err != nil {
		return err
	}
	// A merged fix that did not hold: try again (if auto-fix is on).
	if g.FixState == store.FixMerged {
		r.autofixNew(ctx, gh, g, "again, because the error came back after the fix was merged")
	}
	r.Log.Info("regression: reopened issue", "issue", g.IssueNumber, "service", g.Service)
	r.notify(notify.Event{Type: notify.IssueRegression, Service: g.Service, Group: groupInfo(g),
		Issue:   &notify.IssueInfo{Repo: g.Repo, Number: g.IssueNumber, URL: g.IssueURL},
		Message: fmt.Sprintf("Regression in %s: %s happened again", g.Service, g.Title)})
	return r.Store.MarkReported(ctx, g.ID, g.Count, now)
}

// reopenResolved handles groups we closed ourselves (e.g. a recovered outage)
// that received new events.
func (r *Reporter) reopenResolved(ctx context.Context) error {
	groups, err := r.Store.ListGroups(ctx, store.StatusResolved)
	if err != nil {
		return err
	}
	off, err := r.issuesOff(ctx)
	if err != nil {
		return err
	}
	now := r.now()
	for _, g := range groups {
		if g.IssueNumber == 0 && g.NewSinceReport() > 0 {
			// Resolved before it was ever filed, and it happened again.
			if err := r.Store.SetStatus(ctx, g.ID, store.StatusPending, -1); err != nil {
				return err
			}
			continue
		}
		if g.IssueNumber == 0 || g.NewSinceReport() <= 0 || off[g.Service] {
			continue
		}
		gh, _, err := r.clientFor(ctx, g)
		if err != nil {
			r.Log.Error("github client", "repo", g.Repo, "err", err)
			continue
		}
		iss, err := gh.GetIssue(ctx, g.IssueNumber)
		if err != nil {
			r.Log.Error("get issue", "issue", g.IssueNumber, "err", err)
			continue
		}
		if iss.Closed() && iss.StateReason == "not_planned" {
			if err := r.Store.SetStatus(ctx, g.ID, store.StatusIgnored, g.Count); err != nil {
				return err
			}
			continue
		}
		samples, err := r.Store.Samples(ctx, g.ID)
		if err != nil {
			return err
		}
		if err := r.regress(ctx, gh, g, samples, now); err != nil {
			r.Log.Error("reopen issue", "issue", g.IssueNumber, "err", err)
		}
	}
	return nil
}

// issuesOff returns the services whose issue creation is switched off. For
// those Sentinel still records and groups everything, but files, updates and
// reopens nothing on GitHub.
func (r *Reporter) issuesOff(ctx context.Context) (map[string]bool, error) {
	svcs, err := r.Store.ListServices(ctx)
	if err != nil {
		return nil, err
	}
	off := map[string]bool{}
	for _, sv := range svcs {
		if !sv.Issues {
			off[sv.Name] = true
		}
	}
	return off, nil
}

func inputFor(g store.Group, samples []store.Sample) ai.Input {
	in := ai.Input{Service: g.Service, Environment: g.Env, Kind: g.Kind, Count: g.Count, FirstSeen: g.FirstSeen, LastSeen: g.LastSeen}
	for _, s := range samples {
		in.Samples = append(in.Samples, s.Event)
	}
	return in
}

// clientFor returns the issue client for a group's existing issue.
func (r *Reporter) clientFor(ctx context.Context, g store.Group) (github.API, string, error) {
	repo := g.Repo
	if repo == "" {
		var err error
		if repo, err = r.Router.RepoFor(ctx, g.Service); err != nil {
			return nil, "", err
		}
	}
	gh, err := r.Router.Client(ctx, repo, g.Service)
	return gh, repo, err
}

func (r *Reporter) notify(ev notify.Event) {
	if r.Notify != nil {
		r.Notify.Notify(ev)
	}
}

func groupInfo(g store.Group) *notify.GroupInfo {
	return &notify.GroupInfo{ID: g.ID, Fingerprint: g.Fingerprint, Kind: string(g.Kind), Title: g.Title, Count: g.Count, Env: g.Env}
}
