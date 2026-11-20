package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Analyzer works through the analysis queue: one job at a time, because a
// local model on a small server can only do one draft at a time anyway. It
// runs apart from the reporter, so a slow model never holds up updates,
// outages or uptime checks.
type Analyzer struct {
	Store *store.Store
	AI    ai.Provider
	Log   *slog.Logger
	Now   func() time.Time

	wake chan struct{}
}

func (a *Analyzer) now() time.Time {
	if a.Now != nil {
		return a.Now().UTC()
	}
	return time.Now().UTC()
}

func (a *Analyzer) log() *slog.Logger {
	if a.Log != nil {
		return a.Log
	}
	return slog.Default()
}

// Wake makes Run look at the queue now instead of at its next poll.
func (a *Analyzer) Wake() {
	if a.wake == nil {
		return
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// Run processes jobs until ctx is done.
func (a *Analyzer) Run(ctx context.Context) {
	a.wake = make(chan struct{}, 1)
	if err := a.Store.RequeueRunning(ctx); err != nil {
		a.log().Error("requeue analyses", "err", err)
	}
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		for ctx.Err() == nil {
			worked, err := a.Step(ctx)
			if err != nil {
				a.log().Error("analysis", "err", err)
				break
			}
			if !worked {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
		case <-t.C:
		}
	}
}

// Step runs the oldest queued job, if any. It reports whether there was one.
// With the claude-code provider the fixer container does the jobs; Step only
// gives up on the ones it does not get to.
func (a *Analyzer) Step(ctx context.Context) (bool, error) {
	if remoteAI(a.AI) {
		now := a.now()
		if n, err := a.Store.ExpireAnalyses(ctx, now.Add(-analysisQueueWait), now.Add(-analysisRunWait), now); err != nil {
			return false, err
		} else if n > 0 {
			a.log().Warn("analyses the fixer did not finish in time; their issues use the template", "count", n)
		}
		return false, nil
	}
	job, ok, err := a.Store.ClaimAnalysis(ctx, a.AI.Name(), a.now())
	if err != nil || !ok {
		return false, err
	}
	g, err := a.Store.GetGroupByID(ctx, job.GroupID)
	if err != nil {
		return true, a.Store.FinishAnalysis(ctx, job.ID, "", "group: "+err.Error(), a.now())
	}
	samples, err := a.Store.Samples(ctx, g.ID)
	if err != nil {
		return true, err
	}
	start := time.Now()
	d, err := a.AI.Draft(ctx, inputFor(g, samples))
	took := time.Since(start).Round(100 * time.Millisecond)
	if err != nil {
		if errors.Is(err, context.Canceled) && ctx.Err() != nil {
			return true, nil // shutting down; RequeueRunning picks it up next start
		}
		a.log().Warn("AI analysis failed; the issue will use the template", "group", g.ID, "service", g.Service, "took", took, "err", err)
		return true, a.Store.FinishAnalysis(ctx, job.ID, "", err.Error(), a.now())
	}
	b, _ := json.Marshal(d)
	a.log().Info("AI analysis done", "group", g.ID, "service", g.Service, "provider", job.Provider, "took", took, "title", d.Title)
	return true, a.Store.FinishAnalysis(ctx, job.ID, string(b), "", a.now())
}

// waitingReason is shown on a group whose analysis has not finished.
const waitingReason = "waiting for AI analysis"

// queuedDraft returns the finished draft for a group, queueing an analysis
// first if there is none. ready is false while the analysis is pending.
func (r *Reporter) queuedDraft(ctx context.Context, g store.Group, now time.Time) (ai.Draft, bool, error) {
	a, err := r.Store.LatestAnalysis(ctx, g.ID)
	if errors.Is(err, store.ErrNotFound) {
		if err := r.Store.EnqueueAnalysis(ctx, g.ID, now); err != nil {
			return ai.Draft{}, false, err
		}
		r.Analyzer.Wake()
		return ai.Draft{}, false, r.Store.SetBlockReason(ctx, g.ID, waitingReason)
	}
	if err != nil {
		return ai.Draft{}, false, err
	}
	switch a.Status {
	case store.AnalysisDone:
		var d ai.Draft
		if err := json.Unmarshal([]byte(a.Draft), &d); err == nil && d.Title != "" {
			d.Provider = a.Provider
			return d, true, nil
		}
		fallthrough
	case store.AnalysisFailed:
		samples, err := r.Store.Samples(ctx, g.ID)
		if err != nil {
			return ai.Draft{}, false, err
		}
		d, _ := ai.Template{}.Draft(ctx, inputFor(g, samples))
		d.Provider = "template (AI analysis failed)"
		return d, true, nil
	default: // queued or running
		return ai.Draft{}, false, r.Store.SetBlockReason(ctx, g.ID, waitingReason)
	}
}
