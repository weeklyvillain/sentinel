package store

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func seedLogs(t *testing.T, s *Store, t0 time.Time) {
	t.Helper()
	var recs []LogRecord
	for i := 0; i < 30; i++ {
		sev := int32(9)
		body := fmt.Sprintf("GET /orders/%d ok", i)
		if i%5 == 0 {
			sev, body = 17, fmt.Sprintf("db.ConnError: connection refused for order %d", i)
		}
		svc := "checkout"
		if i%2 == 1 {
			svc = "billing"
		}
		recs = append(recs, LogRecord{Time: t0.Add(time.Duration(i) * time.Minute), Service: svc, Severity: sev, Body: body,
			TraceID: fmt.Sprintf("%032x", i), Attrs: map[string]string{"http.route": "/orders/:id", "user.id": fmt.Sprint(i % 3)}})
	}
	if err := s.InsertLogs(context.Background(), recs); err != nil {
		t.Fatal(err)
	}
}

func TestFTSQuery(t *testing.T) {
	cases := map[string]string{
		`connection refused`:    `"connection"* AND "refused"*`,
		`"connection refused"`:  `"connection refused"`,
		`db.ConnError -timeout`: `"db ConnError" NOT "timeout"*`,
		`it's "bad`:             `"it s" AND "bad"`,
		`   `:                   ``,
		`*** "" -`:              ``,
	}
	for in, want := range cases {
		if got, _ := FTSQuery(in); got != want {
			t.Errorf("FTSQuery(%q) = %q, want %q", in, got, want)
		}
	}
	if m, neg := FTSQuery(`-noise`); !neg || m != `"noise"*` {
		t.Errorf("negative-only: %q %v", m, neg)
	}
}

func TestSearchLogs(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	seedLogs(t, s, t0)

	got, err := s.SearchLogs(ctx, LogQuery{Q: "connection refused"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 6 || got[0].Body != "db.ConnError: connection refused for order 25" {
		t.Fatalf("fts: %d results, first %+v", len(got), got)
	}
	if got[0].Attrs["http.route"] != "/orders/:id" {
		t.Errorf("attrs not decoded: %v", got[0].Attrs)
	}
	got, _ = s.SearchLogs(ctx, LogQuery{Q: "ConnErr", Services: []string{"checkout"}, MinSeverity: 17})
	if len(got) != 3 { // orders 0, 10, 20
		t.Errorf("prefix + filters: %d", len(got))
	}
	got, _ = s.SearchLogs(ctx, LogQuery{Q: "-refused"})
	if len(got) != 24 {
		t.Errorf("negative: %d", len(got))
	}
	got, _ = s.SearchLogs(ctx, LogQuery{TraceID: fmt.Sprintf("%032X", 7)})
	if len(got) != 1 || got[0].Service != "billing" {
		t.Errorf("trace (case-insensitive): %+v", got)
	}
	got, _ = s.SearchLogs(ctx, LogQuery{Attrs: map[string]string{"user.id": "2"}, From: t0, To: t0.Add(10 * time.Minute)})
	if len(got) != 3 { // i = 2, 5, 8
		t.Errorf("attr+time: %d", len(got))
	}
	// Pagination returns every row exactly once.
	seen := map[int64]bool{}
	q := LogQuery{Limit: 7}
	for page := 0; page < 10; page++ {
		res, err := s.SearchLogs(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range res {
			if seen[r.ID] {
				t.Fatalf("duplicate row %d", r.ID)
			}
			seen[r.ID] = true
		}
		if len(res) < q.Limit {
			break
		}
		q.BeforeTime, q.BeforeID = res[len(res)-1].Time, res[len(res)-1].ID
	}
	if len(seen) != 30 {
		t.Errorf("pagination saw %d rows", len(seen))
	}
	n, _ := s.CountLogs(ctx, LogQuery{MinSeverity: 17})
	if n != 6 {
		t.Errorf("count: %d", n)
	}
}

func TestLogHistogramAndPrune(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	seedLogs(t, s, t0)
	h, err := s.LogHistogram(ctx, LogQuery{From: t0, To: t0.Add(30 * time.Minute)}, 10*time.Minute, "level")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 3 || h[0].Counts["info"] != 8 || h[0].Counts["error"] != 2 {
		t.Fatalf("histogram: %+v", h)
	}
	hs, _ := s.LogHistogram(ctx, LogQuery{From: t0, To: t0.Add(30 * time.Minute), MinSeverity: 17}, 30*time.Minute, "service")
	if len(hs) != 1 || hs[0].Counts["checkout"] != 3 || hs[0].Counts["billing"] != 3 {
		t.Fatalf("by service: %+v", hs)
	}
	n, err := s.Prune(ctx, t0.Add(15*time.Minute), time.Time{})
	if err != nil || n != 15 {
		t.Fatalf("prune: %d %v", n, err)
	}
	// FTS index stays in sync after deletes.
	got, _ := s.SearchLogs(ctx, LogQuery{Q: "refused"})
	if len(got) != 3 {
		t.Errorf("after prune fts: %d", len(got))
	}
}

func TestMetrics(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	var pts []MetricPoint
	for i := 0; i < 10; i++ {
		pts = append(pts,
			MetricPoint{Time: t0.Add(time.Duration(i) * time.Minute), Service: "api", Name: "latency", Kind: "histogram", Unit: "s", Attrs: "route=/a", Value: float64(i)},
			MetricPoint{Time: t0.Add(time.Duration(i) * time.Minute), Service: "api", Name: "latency", Kind: "histogram", Unit: "s", Attrs: "route=/b", Value: 100})
	}
	if err := s.InsertMetrics(ctx, pts); err != nil {
		t.Fatal(err)
	}
	list, _ := s.ListMetricSeries(ctx, "api")
	if len(list) != 1 || list[0].Unit != "s" || !list[0].LastSeen.Equal(t0.Add(9*time.Minute)) {
		t.Fatalf("series list: %+v", list)
	}
	ser, err := s.MetricSeriesQuery(ctx, "api", "latency", "max", t0, t0.Add(10*time.Minute), 5*time.Minute, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(ser) != 1 || len(ser[0].Points) != 2 || *ser[0].Points[0].Value != 100 {
		t.Fatalf("total: %+v", ser)
	}
	split, _ := s.MetricSeriesQuery(ctx, "api", "latency", "avg", t0, t0.Add(10*time.Minute), 5*time.Minute, true)
	if len(split) != 2 {
		t.Fatalf("split: %d", len(split))
	}
	for _, sr := range split {
		if sr.Label == "route=/a" && *sr.Points[0].Value != 2 {
			t.Errorf("avg /a: %v", *sr.Points[0].Value)
		}
	}
	v, ok, _ := s.MetricAggregate(ctx, "api", "latency", "avg", t0, t0.Add(time.Hour))
	if !ok || v != 52.25 {
		t.Errorf("aggregate: %v %v", v, ok)
	}
	if _, ok, _ := s.MetricAggregate(ctx, "api", "nope", "avg", t0, t0.Add(time.Hour)); ok {
		t.Error("no data should be ok=false")
	}
}

func TestReposAndSettings(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	inst := Installation{ID: 42, Account: "acme", AccountType: "Organization", RepoSelection: "selected"}
	s.SyncInstallation(ctx, inst, []Repo{{FullName: "acme/api"}, {FullName: "acme/web", Private: true}})
	if err := s.SetRepoAllowed(ctx, "ACME/api", true); err != nil {
		t.Fatal(err)
	}
	// Re-sync keeps the allowed flag and drops removed repos.
	s.SyncInstallation(ctx, inst, []Repo{{FullName: "acme/api"}, {FullName: "acme/new"}})
	repos, _ := s.ListRepos(ctx)
	if len(repos) != 2 || repos[0].FullName != "acme/api" || !repos[0].Allowed || repos[1].Allowed {
		t.Fatalf("repos: %+v", repos)
	}
	if err := s.SetRepoAllowed(ctx, "acme/web", true); err != ErrNotFound {
		t.Errorf("removed repo: %v", err)
	}
	s.SetSetting(ctx, "default_repo", "acme/api")
	if v, _ := s.Setting(ctx, "default_repo"); v != "acme/api" {
		t.Errorf("setting: %q", v)
	}
	// A service added in the dashboard before it sends is never flagged down.
	s.UpsertServiceConfig(ctx, ServiceConfig{Name: "new-svc", Repo: "acme/api", Labels: []string{"team-a", " "}})
	if ok, _ := s.MarkServiceDown(ctx, "new-svc", time.Now()); ok {
		t.Error("never-seen service marked down")
	}
	sv, _ := s.GetService(ctx, "new-svc")
	if !sv.Managed || sv.Repo != "acme/api" || len(sv.Labels) != 1 {
		t.Errorf("service config: %+v", sv)
	}
}
