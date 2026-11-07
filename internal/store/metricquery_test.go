package store

import (
	"context"
	"math"
	"testing"
	"time"
)

func near(t *testing.T, what string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: no value, want %v", what, want)
	}
	if math.Abs(*got-want) > 1e-9 {
		t.Fatalf("%s: got %v, want %v", what, *got, want)
	}
}

func TestRunMetricQuery(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	var pts []MetricPoint
	// A cumulative CPU counter scraped every 15s (Prometheus *_total arrives
	// as a gauge), two CPUs, two modes. user grows 3/s per cpu, idle 1/s.
	for i := 0; i <= 8; i++ {
		ts := t0.Add(time.Duration(i) * 15 * time.Second)
		for _, cpu := range []string{"0", "1"} {
			pts = append(pts,
				MetricPoint{Time: ts, Service: "host", Name: "cpu_total", Kind: "gauge", Attrs: "cpu=" + cpu + ",mode=user", Value: float64(i) * 45},
				MetricPoint{Time: ts, Service: "host", Name: "cpu_total", Kind: "gauge", Attrs: "cpu=" + cpu + ",mode=idle", Value: 1000 + float64(i)*15},
			)
		}
		// A gauge per mountpoint.
		pts = append(pts,
			MetricPoint{Time: ts, Service: "host", Name: "fs_avail", Kind: "gauge", Attrs: "mountpoint=/", Value: 40},
			MetricPoint{Time: ts, Service: "host", Name: "fs_avail", Kind: "gauge", Attrs: "mountpoint=/data", Value: 10},
		)
	}
	// OTLP delta counter: 6 requests per 10s for 2 minutes.
	for i := 0; i < 12; i++ {
		pts = append(pts, MetricPoint{Time: t0.Add(time.Duration(i) * 10 * time.Second), Service: "api", Name: "requests", Kind: "counter", Attrs: "status=2xx", Value: 6})
	}
	if err := s.InsertMetrics(ctx, pts); err != nil {
		t.Fatal(err)
	}
	from, to := t0, t0.Add(2*time.Minute)

	// Rate of a cumulative counter, grouped by mode, summed over CPUs.
	res, err := s.RunMetricQuery(ctx, MetricQuery{Metric: "cpu_total", Agg: "rate", GroupBy: []string{"mode"}}, from, to, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 || res[0].Label != "idle" || res[1].Label != "user" {
		t.Fatalf("groups: %+v", res)
	}
	near(t, "idle rate", res[0].Points[1].Value, 2) // 1/s on each of 2 CPUs
	near(t, "user rate", res[1].Points[1].Value, 6)

	// Filter: everything except idle.
	res, _ = s.RunMetricQuery(ctx, MetricQuery{Metric: "cpu_total", Agg: "rate", Filters: []AttrFilter{{Key: "mode", Op: "!=", Value: "idle"}}}, from, to, time.Minute)
	if len(res) != 1 {
		t.Fatalf("filtered: %+v", res)
	}
	near(t, "non-idle", res[0].Points[1].Value, 6)

	// Delta counter rate: 6 per 10s = 0.6/s.
	res, _ = s.RunMetricQuery(ctx, MetricQuery{Metric: "requests", Service: "api", Agg: "rate"}, from, to, time.Minute)
	near(t, "request rate", res[0].Points[0].Value, 0.6)

	// Gauges: avg per series, then summed across mountpoints (not summed
	// over the four scrapes in a bucket).
	res, _ = s.RunMetricQuery(ctx, MetricQuery{Metric: "fs_avail", Agg: "avg", Combine: "sum"}, from, to, time.Minute)
	near(t, "total avail", res[0].Points[0].Value, 50)
	res, _ = s.RunMetricQuery(ctx, MetricQuery{Metric: "fs_avail", Agg: "last", GroupBy: []string{"mountpoint"}}, from, to, time.Minute)
	if len(res) != 2 || res[0].Label != "/" {
		t.Fatalf("by mountpoint: %+v", res)
	}
	near(t, "/ avail", res[0].Points[0].Value, 40)

	// Limit keeps the largest groups.
	res, _ = s.RunMetricQuery(ctx, MetricQuery{Metric: "fs_avail", GroupBy: []string{"mountpoint"}, Limit: 1}, from, to, time.Minute)
	if len(res) != 1 || res[0].Label != "/" {
		t.Fatalf("limit: %+v", res)
	}

	attrs, err := s.MetricAttrs(ctx, "host", "cpu_total")
	if err != nil || len(attrs) != 2 || attrs[1].Key != "mode" || len(attrs[1].Values) != 2 {
		t.Fatalf("attrs: %+v %v", attrs, err)
	}
	if _, err := s.RunMetricQuery(ctx, MetricQuery{Metric: "x", Agg: "median"}, from, to, time.Minute); err == nil {
		t.Fatal("unknown aggregation accepted")
	}
}

func TestDashboardsCRUD(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	d, err := s.SaveDashboard(ctx, Dashboard{Name: "Host", Spec: []byte(`{"panels":[]}`)}, now)
	if err != nil || d.ID == 0 || string(d.Spec) != `{"panels":[]}` {
		t.Fatalf("create: %+v %v", d, err)
	}
	d.Name = "Host overview"
	if d, err = s.SaveDashboard(ctx, d, now); err != nil || d.Name != "Host overview" {
		t.Fatalf("update: %+v %v", d, err)
	}
	if list, _ := s.ListDashboards(ctx); len(list) != 1 {
		t.Fatalf("list: %d", len(list))
	}
	s.DeleteDashboard(ctx, d.ID)
	if _, err := s.GetDashboard(ctx, d.ID); err != ErrNotFound {
		t.Fatalf("after delete: %v", err)
	}
}
