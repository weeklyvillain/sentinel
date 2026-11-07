package reporter

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/ai"
	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/github"
	"github.com/weeklyvillain/sentinel/internal/store"
)

type fakeAI struct {
	calls int
	fail  bool
}

func (f *fakeAI) Name() string { return "fake" }
func (f *fakeAI) Draft(_ context.Context, in ai.Input) (ai.Draft, error) {
	f.calls++
	if f.fail {
		return ai.Draft{}, errors.New("boom")
	}
	return ai.Draft{Title: "AI: " + in.Service + " broke", Summary: "it broke", LikelyCause: "cosmic rays; ping @alice", NextSteps: []string{"fix it"}}, nil
}

type fakeIssue struct {
	github.Issue
	body     string
	comments []string
}

type fakeGH struct {
	mu      sync.Mutex
	issues  map[int]*fakeIssue
	created int
	reopens int
	closes  int
}

func newFakeGH() *fakeGH { return &fakeGH{issues: map[int]*fakeIssue{}} }

func (f *fakeGH) CreateIssue(_ context.Context, title, body string) (github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created++
	n := len(f.issues) + 1
	iss := &fakeIssue{Issue: github.Issue{Number: n, HTMLURL: fmt.Sprintf("https://gh/%d", n), State: "open", Title: title}, body: body}
	f.issues[n] = iss
	return iss.Issue, nil
}
func (f *fakeGH) Comment(_ context.Context, n int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[n].comments = append(f.issues[n].comments, body)
	return nil
}
func (f *fakeGH) GetIssue(_ context.Context, n int) (github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.issues[n].Issue, nil
}
func (f *fakeGH) Reopen(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reopens++
	f.issues[n].State, f.issues[n].StateReason = "open", "reopened"
	return nil
}
func (f *fakeGH) Close(_ context.Context, n int, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	f.issues[n].State, f.issues[n].StateReason = "closed", reason
	return nil
}
func (f *fakeGH) FindByMarker(_ context.Context, marker string) (*github.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, iss := range f.issues {
		if strings.Contains(iss.body, marker) {
			i := iss.Issue
			return &i, nil
		}
	}
	return nil, nil
}

// userClose simulates a human closing the issue on GitHub.
func (f *fakeGH) userClose(n int, reason string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[n].State, f.issues[n].StateReason = "closed", reason
}

type harness struct {
	t   *testing.T
	ctx context.Context
	now time.Time
	st  *store.Store
	gh  *fakeGH
	ai  *fakeAI
	r   *Reporter
}

func newHarness(t *testing.T, cfg Config) *harness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	h := &harness{t: t, ctx: context.Background(), now: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC), st: st, gh: newFakeGH(), ai: &fakeAI{}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h.r = New(cfg, st, h.ai, StaticRouter{Repo: "acme/app", API: h.gh}, log)
	h.r.Now = func() time.Time { return h.now }
	h.r.StartedAt = h.now
	return h
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *harness) emit(n int, msg string) {
	h.t.Helper()
	for i := 0; i < n; i++ {
		e := event.Event{
			Kind: event.KindLog, Service: "checkout", Environment: "prod", Time: h.now,
			ExcType: "db.ConnError", ExcMessage: fmt.Sprintf(msg, 1000+i),
			TraceID: fmt.Sprintf("%032x", i+1),
			Message: "token=supersecretvalue123 lookup failed",
		}
		h.r.Processor.Event(h.ctx, e)
	}
}

func (h *harness) group() store.Group {
	h.t.Helper()
	gs, err := h.st.ListGroups(h.ctx)
	if err != nil {
		h.t.Fatal(err)
	}
	if len(gs) != 1 {
		h.t.Fatalf("want exactly 1 group, got %d", len(gs))
	}
	return gs[0]
}

func TestFiftyIdenticalEventsOneIssueThenOneComment(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: time.Minute, MinEvents: 3, UpdateInterval: 30 * time.Minute, DownAfter: 0})

	h.emit(50, "failed to load user %d: connection refused")
	h.r.Tick(h.ctx)
	if h.gh.created != 0 {
		t.Fatal("issue filed before settle window elapsed")
	}

	h.advance(61 * time.Second)
	h.r.Tick(h.ctx)
	h.r.Tick(h.ctx)
	if h.gh.created != 1 || h.ai.calls != 1 {
		t.Fatalf("want 1 issue / 1 AI call, got %d / %d", h.gh.created, h.ai.calls)
	}
	g := h.group()
	if g.Count != 50 || g.Status != store.StatusOpen || g.IssueNumber != 1 {
		t.Fatalf("unexpected group: %+v", g)
	}
	iss := h.gh.issues[1]
	for _, want := range []string{"AI: checkout broke", "| Occurrences | 50 |", Marker(g.Fingerprint), "## Trace IDs", "db.ConnError", "[REDACTED]"} {
		if !strings.Contains(iss.Title+iss.body, want) {
			t.Errorf("issue missing %q:\n%s", want, iss.body)
		}
	}
	if strings.Contains(iss.body, "supersecretvalue123") {
		t.Error("secret leaked into issue")
	}
	if strings.Contains(iss.body, "@alice") {
		t.Error("AI text can @mention users")
	}

	// More events: no comment until the update interval has passed.
	h.advance(time.Minute)
	h.emit(7, "failed to load user %d: connection refused")
	h.advance(5 * time.Minute) // past the state-check interval, still before the update interval
	h.r.Tick(h.ctx)
	if len(iss.comments) != 0 {
		t.Fatalf("commented before update interval: %v", iss.comments)
	}
	h.advance(30 * time.Minute)
	h.r.Tick(h.ctx)
	h.r.Tick(h.ctx)
	if len(iss.comments) != 1 {
		t.Fatalf("want 1 update comment, got %d", len(iss.comments))
	}
	if !strings.Contains(iss.comments[0], "**7 new occurrence(s)**") || !strings.Contains(iss.comments[0], "57 total") {
		t.Errorf("unexpected comment: %s", iss.comments[0])
	}
	if h.gh.created != 1 {
		t.Fatalf("duplicate issue created")
	}
}

func TestRecurrenceAfterCloseReopens(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: 0, MinEvents: 1, DownAfter: 0})
	h.emit(3, "boom %d")
	h.r.Tick(h.ctx)
	if h.gh.created != 1 {
		t.Fatal("no issue")
	}
	h.gh.userClose(1, "completed")

	// No new events: nothing happens.
	h.advance(time.Hour)
	h.r.Tick(h.ctx)
	if h.gh.reopens != 0 {
		t.Fatal("reopened without new events")
	}

	h.emit(2, "boom %d")
	h.advance(3 * time.Minute)
	h.r.Tick(h.ctx)
	iss := h.gh.issues[1]
	if h.gh.reopens != 1 || iss.State != "open" {
		t.Fatalf("want reopened issue, reopens=%d state=%s", h.gh.reopens, iss.State)
	}
	if len(iss.comments) != 1 || !strings.Contains(iss.comments[0], "Regression") {
		t.Fatalf("want regression comment, got %v", iss.comments)
	}
	if h.gh.created != 1 {
		t.Fatal("a new issue was created instead of reopening")
	}
	if g := h.group(); g.Status != store.StatusOpen || g.NewSinceReport() != 0 {
		t.Fatalf("unexpected group after regression: %+v", g)
	}
}

func TestClosedNotPlannedIsIgnored(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: 0, DownAfter: 0})
	h.emit(1, "boom %d")
	h.r.Tick(h.ctx)
	h.gh.userClose(1, "not_planned")
	h.emit(5, "boom %d")
	h.advance(time.Hour)
	h.r.Tick(h.ctx)
	h.r.Tick(h.ctx)
	if h.gh.reopens != 0 || len(h.gh.issues[1].comments) != 0 {
		t.Fatal("not_planned issue should stay quiet")
	}
	if g := h.group(); g.Status != store.StatusIgnored {
		t.Fatalf("want ignored, got %s", g.Status)
	}
	h.emit(5, "boom %d")
	h.advance(time.Hour)
	h.r.Tick(h.ctx)
	if h.gh.reopens != 0 || len(h.gh.issues[1].comments) != 0 || h.gh.created != 1 {
		t.Fatal("ignored group produced activity")
	}
}

func (h *harness) setFlags(service string, issues, watch *bool) {
	h.t.Helper()
	if err := h.st.SetServiceFlags(h.ctx, service, store.ServiceFlags{Issues: issues, Watch: watch}); err != nil {
		h.t.Fatal(err)
	}
}

func ptr(b bool) *bool { return &b }

func TestDownThenRecoveryClosesAndLaterReopens(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: time.Minute, MinEvents: 5, DownAfter: 5 * time.Minute})
	h.r.Processor.Heartbeat(h.ctx, "payments", "prod", h.now)
	h.setFlags("payments", nil, ptr(true))

	// Within the startup grace period nothing is flagged even if silent.
	h.advance(4 * time.Minute)
	h.r.Tick(h.ctx)
	h.advance(2 * time.Minute) // 6m since last seen, past grace
	h.r.Tick(h.ctx)
	if h.gh.created != 1 {
		t.Fatalf("want down issue, created=%d", h.gh.created)
	}
	g := h.group()
	if g.Kind != event.KindDown || g.Status != store.StatusOpen {
		t.Fatalf("unexpected group: %+v", g)
	}
	if !strings.Contains(h.gh.issues[1].body, "No telemetry received since") {
		t.Errorf("down issue body missing outage info:\n%s", h.gh.issues[1].body)
	}
	// Still silent: no duplicate events / issues.
	h.advance(10 * time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.Count != 1 || h.gh.created != 1 {
		t.Fatalf("down flagged twice: count=%d created=%d", g.Count, h.gh.created)
	}

	// Recovery.
	h.r.Processor.Heartbeat(h.ctx, "payments", "prod", h.now)
	h.r.Tick(h.ctx)
	iss := h.gh.issues[1]
	if iss.State != "closed" || iss.StateReason != "completed" || len(iss.comments) != 1 {
		t.Fatalf("want closed with recovery comment, got state=%s comments=%v", iss.State, iss.comments)
	}
	if !strings.Contains(iss.comments[0], "Outage lasted about **16m0s**") {
		t.Errorf("unexpected recovery comment: %s", iss.comments[0])
	}
	if g := h.group(); g.Status != store.StatusResolved {
		t.Fatalf("want resolved, got %s", g.Status)
	}

	// A later outage reopens the same issue.
	h.advance(6 * time.Minute)
	h.r.Tick(h.ctx)
	if h.gh.created != 1 || h.gh.reopens != 1 || iss.State != "open" {
		t.Fatalf("want same issue reopened: created=%d reopens=%d state=%s", h.gh.created, h.gh.reopens, iss.State)
	}
	h.r.Processor.Heartbeat(h.ctx, "payments", "prod", h.now)
	h.r.Tick(h.ctx)
	if iss.State != "closed" || h.gh.closes != 2 {
		t.Fatalf("second recovery should close again: state=%s closes=%d", iss.State, h.gh.closes)
	}
}

// Going quiet only counts as down for services watched from the Uptime page.
func TestUnwatchedServiceIsNeverDown(t *testing.T) {
	h := newHarness(t, Config{DownAfter: 5 * time.Minute})
	h.r.Processor.Heartbeat(h.ctx, "batch-job", "prod", h.now)
	h.advance(time.Hour)
	h.r.Tick(h.ctx)
	if h.gh.created != 0 {
		t.Fatal("an unwatched service was flagged down")
	}
	if svcs, _ := h.st.ListServices(h.ctx); svcs[0].Down {
		t.Fatal("unwatched service marked down")
	}

	// Watching it starts the clock from the last time it was seen.
	h.setFlags("batch-job", nil, ptr(true))
	h.r.Tick(h.ctx)
	if h.gh.created != 1 {
		t.Fatalf("watched silent service should be down, created=%d", h.gh.created)
	}
	// Unwatching clears the down state without a recovery comment.
	h.setFlags("batch-job", nil, ptr(false))
	h.r.Tick(h.ctx)
	if svcs, _ := h.st.ListServices(h.ctx); svcs[0].Down || !svcs[0].RecoveredAt.IsZero() {
		t.Fatalf("unwatched service still down: %+v", svcs[0])
	}
	if len(h.gh.issues[1].comments) != 0 {
		t.Fatal("unwatching should not post a recovery comment")
	}
}

// With issue creation off, errors are still grouped but nothing reaches GitHub
// until it is switched back on.
func TestIssuesOffForService(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: 0, MinEvents: 1, UpdateInterval: time.Minute, DownAfter: 0})
	h.r.Processor.Heartbeat(h.ctx, "checkout", "prod", h.now)
	h.setFlags("checkout", ptr(false), nil)
	h.emit(5, "boom %d")
	h.r.Tick(h.ctx)
	g := h.group()
	if h.gh.created != 0 || g.Status != store.StatusPending || !strings.Contains(g.BlockReason, "turned off") {
		t.Fatalf("muted service filed an issue: created=%d %+v", h.gh.created, g)
	}

	h.setFlags("checkout", ptr(true), nil)
	h.r.Tick(h.ctx)
	if h.gh.created != 1 {
		t.Fatalf("switching issues back on should file it, created=%d", h.gh.created)
	}

	// Muting again stops updates to the open issue too.
	h.setFlags("checkout", ptr(false), nil)
	h.emit(5, "boom %d")
	h.advance(time.Hour)
	h.r.Tick(h.ctx)
	if n := len(h.gh.issues[1].comments); n != 0 {
		t.Fatalf("muted service got %d update comment(s)", n)
	}
}

func TestDownDisabled(t *testing.T) {
	h := newHarness(t, Config{DownAfter: 0})
	h.r.Processor.Heartbeat(h.ctx, "payments", "", h.now)
	h.advance(time.Hour)
	h.r.Tick(h.ctx)
	if h.gh.created != 0 {
		t.Fatal("DOWN_AFTER=0 should disable down detection")
	}
}

func TestIssueCapAndAIFallback(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: 0, MaxIssuesPerHour: 2, DownAfter: 0})
	h.ai.fail = true
	for i := 0; i < 4; i++ {
		h.r.Processor.Event(h.ctx, event.Event{Kind: event.KindLog, Service: fmt.Sprintf("svc%d", i), Time: h.now, Message: "boom"})
	}
	h.r.Tick(h.ctx)
	if h.gh.created != 2 {
		t.Fatalf("cap: want 2 issues, got %d", h.gh.created)
	}
	if !strings.Contains(h.gh.issues[1].body, "template (fallback") {
		t.Error("fallback draft should be labeled")
	}
	pending, _ := h.st.ListGroups(h.ctx, store.StatusPending)
	if len(pending) != 2 {
		t.Fatalf("want 2 pending, got %d", len(pending))
	}
	h.advance(61 * time.Minute)
	h.r.Tick(h.ctx)
	if h.gh.created != 4 {
		t.Fatalf("after an hour the rest should be filed, got %d", h.gh.created)
	}
}

func TestMarkerDedupeAfterDBLoss(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: 0, DownAfter: 0})
	h.emit(2, "boom %d")
	h.r.Tick(h.ctx)
	// Simulate a lost DB: fresh store, same GitHub.
	st2, err := store.Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	r2 := New(Config{SettleWindow: 0, DownAfter: 0}, st2, h.ai, StaticRouter{Repo: "acme/app", API: h.gh}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	r2.Now = func() time.Time { return h.now }
	r2.Processor.Event(h.ctx, event.Event{Kind: event.KindLog, Service: "checkout", Environment: "prod", Time: h.now, ExcType: "db.ConnError", ExcMessage: "boom 1"})
	r2.Tick(h.ctx)
	if h.gh.created != 1 {
		t.Fatalf("duplicate issue after DB loss: %d", h.gh.created)
	}
	gs, _ := st2.ListGroups(h.ctx)
	if len(gs) != 1 || gs[0].IssueNumber != 1 || gs[0].Status != store.StatusOpen {
		t.Fatalf("group not linked to existing issue: %+v", gs)
	}
	if len(h.gh.issues[1].comments) != 1 {
		t.Fatalf("adopted issue should get an update comment, got %d", len(h.gh.issues[1].comments))
	}
}
