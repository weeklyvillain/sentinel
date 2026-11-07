package reporter

import (
	"context"
	"log/slog"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/github"
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
	GitHub    github.API
	Log       *slog.Logger
	Now       func() time.Time
	StartedAt time.Time

	capLogged time.Time
}

// New builds a reporter. StartedAt defaults to now.
func New(cfg Config, st *store.Store, provider ai.Provider, gh github.API, log *slog.Logger) *Reporter {
	cfg.defaults()
	if log == nil {
		log = slog.Default()
	}
	r := &Reporter{Cfg: cfg, Store: st, AI: provider, GitHub: gh, Log: log, Now: time.Now}
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
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"down-detection", r.detectDown},
		{"recoveries", r.handleRecoveries},
		{"new-issues", r.createIssues},
		{"updates", r.updateOpen},
		{"resolved-regressions", r.reopenResolved},
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
		if sv.Down || now.Sub(sv.LastSeen) < r.Cfg.DownAfter {
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
		if _, err := r.Processor.Record(ctx, DownEvent(sv.Name, sv.Env, sv.LastSeen, now)); err != nil {
			return err
		}
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
				body := recoveryComment(sv, outage)
				if err := r.GitHub.Comment(ctx, g.IssueNumber, body); err != nil {
					return err
				}
				if err := r.GitHub.Close(ctx, g.IssueNumber, "completed"); err != nil {
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
		if err := r.Store.ClearRecovery(ctx, sv.Name); err != nil {
			return err
		}
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
	for _, g := range groups {
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
		if err := r.fileGroup(ctx, g, now); err != nil {
			r.Log.Error("file issue", "fingerprint", g.Fingerprint, "err", err)
		}
	}
	return nil
}

func (r *Reporter) fileGroup(ctx context.Context, g store.Group, now time.Time) error {
	samples, err := r.Store.Samples(ctx, g.ID)
	if err != nil {
		return err
	}
	marker := Marker(g.Fingerprint)

	// Dedupe against GitHub in case the DB was lost.
	existing, err := r.GitHub.FindByMarker(ctx, marker)
	if err != nil {
		return err
	}
	if existing != nil {
		if err := r.Store.MarkIssueCreated(ctx, g.ID, existing.Number, existing.HTMLURL, g.CountAtLastReport, now, false); err != nil {
			return err
		}
		r.Log.Info("adopted existing issue", "fingerprint", g.Fingerprint, "issue", existing.Number)
		g.Status, g.IssueNumber, g.IssueURL = store.StatusOpen, existing.Number, existing.HTMLURL
		return r.syncOpen(ctx, g, samples, now, true)
	}

	draft, err := r.AI.Draft(ctx, inputFor(g, samples))
	if err != nil {
		r.Log.Warn("AI draft failed, using template", "err", err)
		draft, _ = ai.Template{}.Draft(ctx, inputFor(g, samples))
		draft.Provider = "template (fallback after provider error)"
	}
	provider := r.AI.Name()
	if draft.Provider != "" {
		provider = draft.Provider
	}
	draft = sanitizeDraft(draft)
	body := IssueBody(g, draft, samples, provider)
	iss, err := r.GitHub.CreateIssue(ctx, draft.Title, body)
	if err != nil {
		return err
	}
	if err := r.Store.MarkIssueCreated(ctx, g.ID, iss.Number, iss.HTMLURL, g.Count, now, true); err != nil {
		return err
	}
	r.Log.Info("created issue", "issue", iss.Number, "url", iss.HTMLURL, "service", g.Service, "count", g.Count, "title", draft.Title)
	return nil
}

func (r *Reporter) updateOpen(ctx context.Context) error {
	groups, err := r.Store.ListGroups(ctx, store.StatusOpen)
	if err != nil {
		return err
	}
	now := r.now()
	for _, g := range groups {
		if g.IssueNumber == 0 || g.NewSinceReport() <= 0 {
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
		if err := r.syncOpen(ctx, g, samples, now, due); err != nil {
			r.Log.Error("update issue", "issue", g.IssueNumber, "err", err)
		}
	}
	return nil
}

// syncOpen reconciles an open group with its issue: regression if the issue
// was closed as completed, ignore if not_planned, otherwise an update comment
// when due.
func (r *Reporter) syncOpen(ctx context.Context, g store.Group, samples []store.Sample, now time.Time, due bool) error {
	iss, err := r.GitHub.GetIssue(ctx, g.IssueNumber)
	if err != nil {
		return err
	}
	if iss.Closed() {
		if iss.StateReason == "not_planned" {
			r.Log.Info("issue closed as not planned; ignoring group", "issue", g.IssueNumber)
			return r.Store.SetStatus(ctx, g.ID, store.StatusIgnored, g.Count)
		}
		if g.NewSinceReport() <= 0 {
			// Closed and nothing new: treat as resolved; a later event reopens it.
			return r.Store.SetStatus(ctx, g.ID, store.StatusResolved, g.Count)
		}
		return r.regress(ctx, g, samples, now)
	}
	if !due || g.NewSinceReport() <= 0 {
		return r.Store.MarkChecked(ctx, g.ID, now)
	}
	if err := r.GitHub.Comment(ctx, g.IssueNumber, UpdateComment(g, samples)); err != nil {
		return err
	}
	r.Log.Info("posted update", "issue", g.IssueNumber, "new", g.NewSinceReport(), "total", g.Count)
	return r.Store.MarkReported(ctx, g.ID, g.Count, now)
}

func (r *Reporter) regress(ctx context.Context, g store.Group, samples []store.Sample, now time.Time) error {
	if err := r.GitHub.Reopen(ctx, g.IssueNumber); err != nil {
		return err
	}
	if err := r.GitHub.Comment(ctx, g.IssueNumber, RegressionComment(g, samples)); err != nil {
		return err
	}
	if err := r.Store.SetStatus(ctx, g.ID, store.StatusOpen, -1); err != nil {
		return err
	}
	r.Log.Info("regression: reopened issue", "issue", g.IssueNumber, "service", g.Service)
	return r.Store.MarkReported(ctx, g.ID, g.Count, now)
}

// reopenResolved handles groups we closed ourselves (e.g. a recovered outage)
// that received new events.
func (r *Reporter) reopenResolved(ctx context.Context) error {
	groups, err := r.Store.ListGroups(ctx, store.StatusResolved)
	if err != nil {
		return err
	}
	now := r.now()
	for _, g := range groups {
		if g.IssueNumber == 0 || g.NewSinceReport() <= 0 {
			continue
		}
		iss, err := r.GitHub.GetIssue(ctx, g.IssueNumber)
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
		if err := r.regress(ctx, g, samples, now); err != nil {
			r.Log.Error("reopen issue", "issue", g.IssueNumber, "err", err)
		}
	}
	return nil
}

func inputFor(g store.Group, samples []store.Sample) ai.Input {
	in := ai.Input{Service: g.Service, Environment: g.Env, Kind: g.Kind, Count: g.Count, FirstSeen: g.FirstSeen, LastSeen: g.LastSeen}
	for _, s := range samples {
		in.Samples = append(in.Samples, s.Event)
	}
	return in
}
