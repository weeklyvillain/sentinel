package reporter

import (
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

func withQueue(h *harness) *Analyzer {
	a := &Analyzer{Store: h.st, AI: h.ai, Log: h.r.Log, Now: func() time.Time { return h.now }}
	h.r.Analyzer = a
	return a
}

func TestQueuedAnalysisThenIssue(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: time.Minute, MinEvents: 3, DownAfter: 0})
	a := withQueue(h)
	h.emit(10, "failed to load user %d")
	h.advance(2 * time.Minute)

	// The first tick queues the analysis and files nothing.
	h.r.Tick(h.ctx)
	if h.gh.created != 0 || h.ai.calls != 0 {
		t.Fatalf("filed before analysis: created=%d calls=%d", h.gh.created, h.ai.calls)
	}
	g := h.group()
	if g.BlockReason != waitingReason {
		t.Fatalf("block reason = %q", g.BlockReason)
	}
	// Another tick does not queue it twice.
	h.r.Tick(h.ctx)
	if list, _ := h.st.ListAnalyses(h.ctx, 10); len(list) != 1 || list[0].Status != store.AnalysisQueued {
		t.Fatalf("queue: %+v", list)
	}

	if worked, err := a.Step(h.ctx); !worked || err != nil {
		t.Fatalf("step: %v %v", worked, err)
	}
	if worked, _ := a.Step(h.ctx); worked {
		t.Fatal("queue should be empty")
	}
	h.r.Tick(h.ctx)
	if h.gh.created != 1 || !strings.Contains(h.gh.issues[1].Title, "AI: checkout broke") {
		t.Fatalf("want issue from the AI draft, created=%d title=%q", h.gh.created, h.gh.issues[1].Title)
	}
	if !strings.Contains(h.gh.issues[1].body, "fake") {
		t.Errorf("issue should credit the provider that wrote it:\n%s", h.gh.issues[1].body)
	}
	if h.ai.calls != 1 {
		t.Fatalf("AI called %d times", h.ai.calls)
	}
}

func TestFailedAnalysisFallsBackToTemplateAndCanRetry(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: 0, MinEvents: 1, DownAfter: 0})
	a := withQueue(h)
	h.ai.fail = true
	h.emit(3, "boom %d")
	h.r.Tick(h.ctx)
	a.Step(h.ctx)
	list, _ := h.st.ListAnalyses(h.ctx, 10)
	if len(list) != 1 || list[0].Status != store.AnalysisFailed || list[0].Error == "" {
		t.Fatalf("want failed analysis, got %+v", list)
	}
	// Retry is possible while the group is still pending.
	if err := h.st.RetryAnalysis(h.ctx, list[0].ID, h.now); err != nil {
		t.Fatal(err)
	}
	a.Step(h.ctx) // fails again
	h.r.Tick(h.ctx)
	if h.gh.created != 1 || !strings.Contains(h.gh.issues[1].body, "template (AI analysis failed)") {
		t.Fatalf("want template issue after failed analysis, created=%d", h.gh.created)
	}
}

func TestOutagesSkipTheQueue(t *testing.T) {
	h := newHarness(t, Config{DownAfter: 5 * time.Minute})
	withQueue(h)
	h.r.Processor.Heartbeat(h.ctx, "payments", "prod", h.now)
	h.setFlags("payments", nil, ptr(true))
	h.advance(10 * time.Minute)
	h.r.Tick(h.ctx)
	if h.gh.created != 1 {
		t.Fatalf("outage should be filed without waiting for analysis, created=%d", h.gh.created)
	}
	if list, _ := h.st.ListAnalyses(h.ctx, 10); len(list) != 0 {
		t.Fatalf("outage was queued: %+v", list)
	}
}

func TestRequeueRunningAfterRestart(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: 0, MinEvents: 1, DownAfter: 0})
	withQueue(h)
	h.emit(1, "boom %d")
	h.r.Tick(h.ctx)
	if _, ok, _ := h.st.ClaimAnalysis(h.ctx, "x", h.now); !ok {
		t.Fatal("claim")
	}
	if err := h.st.RequeueRunning(h.ctx); err != nil {
		t.Fatal(err)
	}
	if list, _ := h.st.ListAnalyses(h.ctx, 10); list[0].Status != store.AnalysisQueued {
		t.Fatalf("want queued again, got %s", list[0].Status)
	}
}
