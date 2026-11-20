package reporter

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/store"
)

func TestSnoozeReopensWhenEventsArrive(t *testing.T) {
	h := filedHarness(t)
	id := h.group().ID
	g, err := h.r.SnoozeGroup(h.ctx, id, store.Snooze{Until: h.now.Add(time.Hour)}, "alice")
	if err != nil || g.Status != store.StatusIgnored || !g.Snoozed() {
		t.Fatalf("snooze: %v %+v", err, g)
	}
	iss := h.gh.issues[1]
	if iss.StateReason != "not_planned" || !strings.Contains(iss.comments[len(iss.comments)-1], "Snoozed until") {
		t.Fatalf("issue: %+v %v", iss.Issue, iss.comments)
	}
	h.emit(2, "boom %d")
	h.advance(30 * time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.Status != store.StatusIgnored {
		t.Fatalf("still snoozed, got %s", g.Status)
	}
	h.advance(31 * time.Minute)
	h.r.Tick(h.ctx)
	g = h.group()
	if g.Status != store.StatusOpen || g.Snoozed() || iss.State != "open" || !strings.Contains(iss.comments[len(iss.comments)-1], "Snooze ended") {
		t.Fatalf("snooze end: %s %+v %v", g.Status, iss.Issue, iss.comments)
	}
}

func TestSnoozeWithoutEventsResolves(t *testing.T) {
	h := filedHarness(t)
	id := h.group().ID
	if _, err := h.r.SnoozeGroup(h.ctx, id, store.Snooze{Until: h.now.Add(time.Hour)}, ""); err != nil {
		t.Fatal(err)
	}
	h.advance(2 * time.Hour)
	h.r.Tick(h.ctx)
	if g := h.group(); g.Status != store.StatusResolved || h.gh.issues[1].StateReason != "completed" {
		t.Fatalf("quiet snooze end: %s %+v", g.Status, h.gh.issues[1].Issue)
	}
	// A later event is a regression now, not ignored.
	h.emit(1, "boom %d")
	h.advance(time.Minute)
	h.r.Tick(h.ctx)
	if g := h.group(); g.Status != store.StatusOpen || h.gh.issues[1].State != "open" {
		t.Fatalf("regression after snooze: %s", g.Status)
	}
}

func TestMergeClosesSourceIssue(t *testing.T) {
	h := filedHarness(t)
	target := h.group()
	// A second, different error in the same service gets its own issue.
	h.r.Processor.Event(h.ctx, event.Event{Kind: event.KindLog, Service: "checkout", Time: h.now, ExcType: "OtherError", ExcMessage: "nope"})
	h.r.Tick(h.ctx)
	if h.gh.created != 2 {
		t.Fatalf("want 2 issues, got %d", h.gh.created)
	}
	gs, _ := h.st.ListGroups(h.ctx)
	var src store.Group
	for _, g := range gs {
		if g.ID != target.ID {
			src = g
		}
	}
	g, err := h.r.MergeGroups(h.ctx, target.ID, []int64{src.ID}, "bob")
	if err != nil || g.Count != target.Count+1 || g.Merged != 1 {
		t.Fatalf("merge: %v %+v", err, g)
	}
	si := h.gh.issues[src.IssueNumber]
	if si.State != "closed" || si.StateReason != "not_planned" || !strings.Contains(si.comments[0], fmt.Sprintf("#%d", target.IssueNumber)) {
		t.Fatalf("source issue: %+v %v", si.Issue, si.comments)
	}
	back, err := h.r.UnmergeGroup(h.ctx, target.ID, src.Fingerprint, "bob")
	if err != nil || back.IssueNumber != src.IssueNumber || back.Status != store.StatusOpen || si.State != "open" {
		t.Fatalf("unmerge: %v %+v %+v", err, back, si.Issue)
	}
}

func TestDetectNPlusOne(t *testing.T) {
	spans := []store.Span{{TraceID: "t", SpanID: "p", Service: "api", Name: "GET /products", Kind: "server"}}
	for i := 0; i < 12; i++ {
		spans = append(spans, store.Span{TraceID: "t", SpanID: fmt.Sprint(i), ParentID: "p", Service: "api", Name: "SELECT",
			Attrs: map[string]string{"db.statement": fmt.Sprintf("SELECT * FROM products WHERE id = %d", i)}})
	}
	evs := DetectNPlusOne(spans, 10)
	if len(evs) != 1 || evs[0].Kind != event.KindPerf || !strings.Contains(evs[0].Message, "12 times under GET /products") {
		t.Fatalf("n+1: %+v", evs)
	}
	if len(DetectNPlusOne(spans, 20)) != 0 {
		t.Fatal("12 < 20 should not fire")
	}
}

func TestSlowEndpointBecomesPerfIssue(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: 0, MinEvents: 1, PerfSlowP95: time.Second})
	var spans []store.Span
	for i := 0; i < 30; i++ {
		spans = append(spans, store.Span{TraceID: fmt.Sprintf("%032x", i), SpanID: "a", Service: "checkout", Name: "POST /pay", Kind: "server",
			Start: h.now.Add(-5 * time.Minute), DurationUS: 3_000_000, Sampled: true})
	}
	h.r.Processor.Spans(h.ctx, spans)
	h.r.Tick(h.ctx) // detects, groups
	h.advance(time.Second)
	h.r.Tick(h.ctx) // files
	g := h.group()
	if g.Kind != event.KindPerf || !strings.Contains(g.Title, "Slow endpoint: POST /pay") || g.IssueNumber == 0 {
		t.Fatalf("perf group: %+v", g)
	}
	smp, _ := h.st.Samples(h.ctx, g.ID)
	if smp[0].Event.TraceID == "" {
		t.Fatal("slow endpoint sample should link a trace")
	}
}

func TestIssueBodyHasReleasesAndTags(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: 0, MinEvents: 1})
	for i := 0; i < 4; i++ {
		h.r.Processor.Event(h.ctx, event.Event{Kind: event.KindLog, Service: "checkout", Release: "2.1.0", Time: h.now, ExcType: "E", ExcMessage: "x",
			Attributes: map[string]string{"user.id": fmt.Sprint(i), "http.route": "/pay"}})
	}
	h.r.Tick(h.ctx)
	body := h.gh.issues[1].body
	for _, want := range []string{"First seen in release | `2.1.0`", "Users affected | 4", "**route**: `/pay` 100%"} {
		if !strings.Contains(body, want) {
			t.Errorf("issue body lacks %q:\n%s", want, body)
		}
	}
}
