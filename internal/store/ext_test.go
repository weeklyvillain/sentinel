package store

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
)

func TestTagsReleasesAndUsers(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		rel := "1.0"
		if i >= 6 {
			rel = "1.1"
		}
		if _, err := s.RecordEvent(ctx, "fp", event.Event{Kind: event.KindLog, Service: "api", Environment: "prod", Release: rel,
			Time: t0.Add(time.Duration(i) * time.Second), Message: "boom",
			Attributes: map[string]string{"user.id": fmt.Sprint(i % 4), "http.route": "/x"}}); err != nil {
			t.Fatal(err)
		}
	}
	g, _ := s.GetGroup(ctx, "fp")
	if g.FirstRelease != "1.0" || g.LastRelease != "1.1" || g.Users != 4 {
		t.Fatalf("releases/users: %+v", g)
	}
	tags, err := s.GroupTags(ctx, g.ID, 10)
	if err != nil {
		t.Fatal(err)
	}
	byKey := map[string]TagSummary{}
	for _, tg := range tags {
		byKey[tg.Key] = tg
	}
	if r := byKey["release"]; r.Distinct != 2 || r.Total != 10 || r.Top[0].Value != "1.0" || r.Top[0].Count != 6 {
		t.Fatalf("release tag: %+v", r)
	}
	if byKey["route"].Top[0].Value != "/x" {
		t.Fatalf("route tag: %+v", byKey["route"])
	}
	for _, f := range []GroupFilter{{Release: "1.1"}, {FirstRelease: "1.0"}, {Env: "prod"}} {
		gs, err := s.QueryGroups(ctx, f)
		if err != nil || len(gs) != 1 {
			t.Fatalf("filter %+v: %v %d", f, err, len(gs))
		}
	}
	if gs, _ := s.QueryGroups(ctx, GroupFilter{FirstRelease: "1.1"}); len(gs) != 0 {
		t.Fatalf("first_release 1.1 should match nothing, got %d", len(gs))
	}
}

func TestMergeAndUnmerge(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	rec := func(fp string, n int, user string) {
		for i := 0; i < n; i++ {
			if _, err := s.RecordEvent(ctx, fp, event.Event{Kind: event.KindLog, Service: "api", Time: t0.Add(time.Duration(i) * time.Second),
				Message: fp, Attributes: map[string]string{"user.id": user}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rec("a", 3, "u1")
	rec("b", 2, "u2")
	a, _ := s.GetGroup(ctx, "a")
	b, _ := s.GetGroup(ctx, "b")
	if _, err := s.Merge(ctx, a.ID, []int64{b.ID}, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetGroup(ctx, "b"); err != ErrNotFound {
		t.Fatalf("source should be gone: %v", err)
	}
	rec("b", 1, "u3") // routed to a now
	a, _ = s.GetGroup(ctx, "a")
	if a.Count != 6 || a.Users != 3 || a.Merged != 1 {
		t.Fatalf("merged group: count=%d users=%d merged=%d", a.Count, a.Users, a.Merged)
	}
	fps, _ := s.Fingerprints(ctx, "a")
	if len(fps) != 2 {
		t.Fatalf("fingerprints: %v", fps)
	}
	nb, _, err := s.Unmerge(ctx, a.ID, "b", StatusPending)
	if err != nil {
		t.Fatal(err)
	}
	a, _ = s.GetGroup(ctx, "a")
	if nb.Count != 3 || a.Count != 3 || nb.Fingerprint != "b" {
		t.Fatalf("after unmerge: a=%d b=%d", a.Count, nb.Count)
	}
	smp, _ := s.Samples(ctx, nb.ID)
	if len(smp) != 3 {
		t.Fatalf("b samples: %d", len(smp))
	}
}

func TestSnoozeOver(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	g := Group{Status: StatusIgnored, Count: 10}
	if _, over := SnoozeOver(g, now); over {
		t.Fatal("plain ignore never ends")
	}
	g.SnoozeUntil = now.Add(time.Hour)
	if _, over := SnoozeOver(g, now); over {
		t.Fatal("not yet")
	}
	if _, over := SnoozeOver(g, now.Add(2*time.Hour)); !over {
		t.Fatal("time over")
	}
	g = Group{Status: StatusIgnored, Count: 20, SnoozeCount: 20}
	if _, over := SnoozeOver(g, now); !over {
		t.Fatal("count reached")
	}
	g = Group{Status: StatusIgnored, LastRelease: "2", SnoozeRelease: "1"}
	if _, over := SnoozeOver(g, now); !over {
		t.Fatal("new release")
	}
}

func TestSpanStatsAndQuantiles(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var spans []Span
	for i := 0; i < 100; i++ {
		st := SpanOK
		if i < 5 {
			st = SpanError
		}
		spans = append(spans, Span{TraceID: fmt.Sprintf("%032x", i), SpanID: fmt.Sprintf("%016x", i), Service: "api", Name: "GET /x", Kind: "server",
			Start: t0.Add(time.Duration(i) * time.Second), DurationUS: int64(i+1) * 1000, Status: st, Sampled: i%10 == 0})
		// A child span is stored but not counted as a request.
		spans = append(spans, Span{TraceID: fmt.Sprintf("%032x", i), SpanID: fmt.Sprintf("%016x", 1000+i), ParentID: fmt.Sprintf("%016x", i),
			Service: "api", Name: "SELECT", Kind: "client", Start: t0, DurationUS: 10, Sampled: i%10 == 0})
	}
	if err := s.InsertSpans(ctx, spans); err != nil {
		t.Fatal(err)
	}
	ops, err := s.Operations(ctx, "", t0, t0.Add(10*time.Minute))
	if err != nil || len(ops) != 1 {
		t.Fatalf("ops: %v %v", err, ops)
	}
	op := ops[0]
	if op.Count != 100 || op.Errors != 5 || math.Abs(op.P95MS-95) > 10 || math.Abs(op.P50MS-50) > 6 {
		t.Fatalf("stats: %+v", op)
	}
	tr, err := s.SearchTraces(ctx, TraceQuery{Service: "api", From: t0.Add(-time.Hour), To: t0.Add(time.Hour)})
	if err != nil || len(tr) != 10 {
		t.Fatalf("sampled traces: %v %d", err, len(tr))
	}
	got, _ := s.GetTrace(ctx, tr[0].TraceID)
	if len(got) != 2 || tr[0].RootName != "GET /x" {
		t.Fatalf("trace: %+v", tr[0])
	}
	series, err := s.RunSpanQuery(ctx, SpanQuery{Service: "api", Stat: "p95"}, t0, t0.Add(10*time.Minute), 10*time.Minute)
	if err != nil || len(series) != 1 || series[0].Points[0].Value == nil {
		t.Fatalf("span query: %v %+v", err, series)
	}
}

func TestBucketQuantile(t *testing.T) {
	bounds := []float64{1, 2, 4}
	counts := []uint64{10, 10, 0, 0}
	if v, _ := BucketQuantile(0.5, bounds, counts); v != 1 {
		t.Fatalf("p50 = %v", v)
	}
	if v, _ := BucketQuantile(0.75, bounds, counts); v != 1.5 {
		t.Fatalf("p75 = %v", v)
	}
	if _, ok := BucketQuantile(0.5, bounds, []uint64{0, 0, 0, 0}); ok {
		t.Fatal("empty should be no data")
	}
}

func TestHistogramMetricPercentile(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := s.InsertMetrics(ctx, []MetricPoint{{Time: t0, Service: "api", Name: "lat", Kind: "histogram", Value: 1,
		Bounds: []float64{0.1, 1}, Counts: []uint64{50, 50, 0}}}); err != nil {
		t.Fatal(err)
	}
	ser, err := s.RunMetricQuery(ctx, MetricQuery{Metric: "lat", Agg: "p90"}, t0, t0.Add(time.Minute), time.Minute)
	if err != nil || len(ser) != 1 || ser[0].Points[0].Value == nil {
		t.Fatalf("p90: %v %+v", err, ser)
	}
	if v := *ser[0].Points[0].Value; math.Abs(v-0.82) > 0.01 {
		t.Fatalf("p90 = %v", v)
	}
	v, ok, err := s.MetricAggregate(ctx, "", "lat", "p50", t0, t0.Add(time.Minute))
	if err != nil || !ok || math.Abs(v-0.1) > 1e-9 {
		t.Fatalf("aggregate p50 = %v %v %v", v, ok, err)
	}
	hm, err := s.RunMetricQuery(ctx, MetricQuery{Metric: "lat", Agg: "heatmap"}, t0, t0.Add(time.Minute), time.Minute)
	if err != nil || len(hm) != 2 {
		t.Fatalf("heatmap: %v %d", err, len(hm))
	}
}

func TestSilences(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now().UTC()
	if _, err := s.SaveSilence(ctx, Silence{Service: "api", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.Silenced(ctx, "api", 3, now); !ok {
		t.Fatal("api should be silenced")
	}
	if _, ok, _ := s.Silenced(ctx, "web", 3, now); ok {
		t.Fatal("web should not be silenced")
	}
	if _, ok, _ := s.Silenced(ctx, "api", 3, now.Add(2*time.Hour)); ok {
		t.Fatal("silence should have ended")
	}
}
