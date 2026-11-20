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

func TestFTSTerm(t *testing.T) {
	cases := map[[2]string]string{
		{"connection", ""}:          `"connection"*`,
		{"db.ConnError", ""}:        `"db ConnError"`,
		{"connection refused", "p"}: `"connection refused"`,
		{"***", ""}:                 ``,
	}
	for in, want := range cases {
		if got := ftsTerm(in[0], in[1] == "p"); got != want {
			t.Errorf("ftsTerm(%q) = %q, want %q", in[0], got, want)
		}
	}
}

func TestParseLogQueryErrors(t *testing.T) {
	for _, in := range []string{`(timeout`, `(a (b)`, `level:loud`, `duration_ms>fast`, `severity:high`} {
		n, err := ParseLogQuery(in)
		if err == nil {
			_, _, err = compileLogQuery(n)
		}
		if err == nil {
			t.Errorf("%q: expected an error", in)
		}
	}
	for _, in := range []string{``, `   `, `*** "" -`, `it's "bad`, `http://example.com/x`, `Error: boom`, `timeout)`} {
		n, err := ParseLogQuery(in)
		if err == nil {
			_, _, err = compileLogQuery(n)
		}
		if err != nil {
			t.Errorf("%q: %v", in, err)
		}
	}
}

func TestAdvancedSearch(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	seedLogs(t, s, t0)
	// JSON lines carry their own fields.
	if err := s.InsertLogs(ctx, []LogRecord{
		{Time: t0.Add(31 * time.Minute), Service: "api", Severity: 5, Body: `{"msg":"slow query","duration_ms":900,"ok":true,"db":{"table":"orders"}}`},
		{Time: t0.Add(32 * time.Minute), Service: "api", Severity: 5, Body: `{"msg":"fast query","duration_ms":12,"ok":false}`},
	}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]int{
		`service:checkout`:                    15,
		`-service:checkout`:                   17,
		`service:check*`:                      15,
		`level:error`:                         6,
		`level>=warn`:                         6,
		`level:debug`:                         2,
		`level<info`:                          2,
		`refused OR level:debug`:              8,
		`(refused OR query) service:api`:      2,
		`user.id:2`:                           10,
		`user.id!=2`:                          22,
		`-user.id:2`:                          22,
		`has:user.id`:                         30,
		`-has:user.id`:                        2,
		`http.route:/orders/*`:                30,
		`duration_ms>500`:                     1,
		`duration_ms:<=900`:                   2,
		`ok:true`:                             1,
		`db.table:orders`:                     1,
		`message:slow`:                        1,
		`connection refused -service:billing`: 3,
		`NOT refused`:                         26,
		`-refused`:                            26,
	}
	for in, want := range cases {
		got, err := s.CountLogs(ctx, LogQuery{Q: in})
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got != int64(want) {
			t.Errorf("%q: %d matches, want %d", in, got, want)
		}
	}
	n, _ := s.CountLogs(ctx, LogQuery{Levels: []string{"debug", "error"}})
	if n != 8 {
		t.Errorf("levels debug+error: %d", n)
	}
}

func TestLogMetricSeries(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	t0 := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)
	var recs []LogRecord
	for i := 0; i < 10; i++ {
		svc := "a"
		if i >= 5 {
			svc = "b"
		}
		recs = append(recs, LogRecord{Time: t0.Add(time.Duration(i) * time.Minute), Service: svc, Severity: 9, Body: "req",
			Attrs: map[string]string{"duration_ms": fmt.Sprint((i + 1) * 10), "user": fmt.Sprint(i % 3)}})
	}
	if err := s.InsertLogs(ctx, recs); err != nil {
		t.Fatal(err)
	}
	q := LogQuery{From: t0, To: t0.Add(10 * time.Minute)}
	ser, err := s.LogMetricSeries(ctx, q, LogMetric{Agg: "count"}, 10*time.Minute)
	if err != nil || len(ser) != 1 || *ser[0].Points[0].Value != 10 {
		t.Fatalf("count: %v %+v", err, ser)
	}
	ser, _ = s.LogMetricSeries(ctx, q, LogMetric{Agg: "avg", Field: "duration_ms", By: "service"}, 10*time.Minute)
	got := map[string]float64{}
	for _, x := range ser {
		got[x.Label] = *x.Points[0].Value
	}
	if got["a"] != 30 || got["b"] != 80 {
		t.Errorf("avg by service: %v", got)
	}
	ser, _ = s.LogMetricSeries(ctx, q, LogMetric{Agg: "p50", Field: "duration_ms"}, 10*time.Minute)
	if v := *ser[0].Points[0].Value; v != 55 {
		t.Errorf("p50: %v", v)
	}
	ser, _ = s.LogMetricSeries(ctx, q, LogMetric{Agg: "unique", Field: "user"}, 10*time.Minute)
	if v := *ser[0].Points[0].Value; v != 3 {
		t.Errorf("unique: %v", v)
	}
	ser, _ = s.LogMetricSeries(ctx, q, LogMetric{Agg: "count", By: "user"}, 5*time.Minute)
	if len(ser) != 3 || len(ser[0].Points) != 2 {
		t.Errorf("count by attr: %+v", ser)
	}
	ser, _ = s.LogMetricSeries(ctx, LogQuery{Q: "service:a", From: q.From, To: q.To}, LogMetric{Agg: "max", Field: "duration_ms"}, time.Minute)
	if v := ser[0].Points[9].Value; v != nil {
		t.Errorf("max: empty buckets should be nil, got %v", *v)
	}
	if _, err := s.LogMetricSeries(ctx, q, LogMetric{Agg: "avg"}, time.Minute); err == nil {
		t.Error("avg without a field should fail")
	}
}

func TestSavedSearches(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	now := time.Now()
	a, err := s.SaveSavedSearch(ctx, SavedSearch{Name: "errors", Params: "level=error"}, now)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := s.SaveSavedSearch(ctx, SavedSearch{Name: "slow", Params: "q=duration_ms>500"}, now)
	s.UseSavedSearch(ctx, b.ID, now)
	list, _ := s.ListSavedSearches(ctx)
	if len(list) != 2 || list[0].Name != "slow" || list[0].Uses != 1 {
		t.Fatalf("list: %+v", list)
	}
	a.Name = "all errors"
	if a, err = s.SaveSavedSearch(ctx, a, now); err != nil || a.Name != "all errors" {
		t.Fatalf("update: %v %+v", err, a)
	}
	s.DeleteSavedSearch(ctx, a.ID)
	if _, err := s.GetSavedSearch(ctx, a.ID); err != ErrNotFound {
		t.Errorf("deleted: %v", err)
	}
	if err := s.UseSavedSearch(ctx, a.ID, now); err != ErrNotFound {
		t.Errorf("use deleted: %v", err)
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
