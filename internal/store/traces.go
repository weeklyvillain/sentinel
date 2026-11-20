package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

const schemaTraces = `
CREATE TABLE IF NOT EXISTS spans (
	trace_id TEXT NOT NULL,
	span_id TEXT NOT NULL,
	parent_id TEXT NOT NULL DEFAULT '',
	service TEXT NOT NULL,
	env TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	start_us INTEGER NOT NULL,
	dur_us INTEGER NOT NULL,
	status INTEGER NOT NULL DEFAULT 0,
	status_msg TEXT NOT NULL DEFAULT '',
	attrs TEXT NOT NULL DEFAULT '{}',
	events TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS spans_trace ON spans(trace_id);
CREATE INDEX IF NOT EXISTS spans_start ON spans(start_us);
CREATE INDEX IF NOT EXISTS spans_service_start ON spans(service, start_us);

-- Per-minute request statistics of entry spans (server, consumer and root
-- spans), kept for every span even when traces are sampled.
CREATE TABLE IF NOT EXISTS span_stats (
	ts INTEGER NOT NULL,
	service TEXT NOT NULL,
	name TEXT NOT NULL,
	count INTEGER NOT NULL,
	errors INTEGER NOT NULL,
	sum_us INTEGER NOT NULL,
	hist TEXT NOT NULL,
	PRIMARY KEY (ts, service, name)
);
CREATE INDEX IF NOT EXISTS span_stats_service ON span_stats(service, name, ts);
`

// Span status codes (as in OTLP).
const (
	SpanUnset = 0
	SpanOK    = 1
	SpanError = 2
)

// SpanEvent is an event recorded on a span (an exception, a log).
type SpanEvent struct {
	Time  time.Time         `json:"time"`
	Name  string            `json:"name"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// Span is one stored span.
type Span struct {
	TraceID    string            `json:"trace_id"`
	SpanID     string            `json:"span_id"`
	ParentID   string            `json:"parent_id,omitempty"`
	Service    string            `json:"service"`
	Env        string            `json:"env,omitempty"`
	Version    string            `json:"version,omitempty"`
	Name       string            `json:"name"`
	Kind       string            `json:"kind"` // server | client | internal | producer | consumer
	Start      time.Time         `json:"start"`
	DurationUS int64             `json:"duration_us"`
	Status     int               `json:"status"`
	StatusMsg  string            `json:"status_message,omitempty"`
	Attrs      map[string]string `json:"attrs,omitempty"`
	Events     []SpanEvent       `json:"events,omitempty"`
	// Sampled: store the span (not only count it in the statistics).
	Sampled bool `json:"-"`
}

// Entry reports whether the span is where a request enters a service.
func (s Span) Entry() bool {
	return s.Kind == "server" || s.Kind == "consumer" || s.ParentID == ""
}

// --- latency histogram --------------------------------------------------

// Histogram buckets: upper bounds growing by √2 from 0.1 ms, up to about an
// hour; the last bucket is open.
const histBuckets = 52

var histBounds = func() []float64 {
	b := make([]float64, histBuckets-1)
	for i := range b {
		b[i] = 100 * math.Pow(math.Sqrt2, float64(i)) // µs
	}
	return b
}()

func histIndex(us int64) int {
	v := float64(us)
	i := sort.SearchFloat64s(histBounds, v)
	if i >= len(histBounds) {
		return histBuckets - 1
	}
	return i
}

// Hist is a latency histogram over the fixed buckets.
type Hist []int64

func newHist() Hist { return make(Hist, histBuckets) }

func parseHist(s string) Hist {
	h := newHist()
	for i, p := range strings.Split(s, ",") {
		if i >= histBuckets {
			break
		}
		var n int64
		fmt.Sscan(p, &n)
		h[i] = n
	}
	return h
}

func (h Hist) String() string {
	parts := make([]string, len(h))
	for i, n := range h {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ",")
}

func (h Hist) add(o Hist) {
	for i := range h {
		if i < len(o) {
			h[i] += o[i]
		}
	}
}

// Quantile estimates the q-quantile (0-1) in microseconds by linear
// interpolation inside the bucket. ok=false when the histogram is empty.
func (h Hist) Quantile(q float64) (float64, bool) {
	var total int64
	for _, n := range h {
		total += n
	}
	if total == 0 {
		return 0, false
	}
	rank := q * float64(total)
	var cum int64
	for i, n := range h {
		if n == 0 {
			continue
		}
		if float64(cum+n) >= rank {
			lo := 0.0
			if i > 0 {
				lo = histBounds[i-1]
			}
			hi := lo * math.Sqrt2
			if i < len(histBounds) {
				hi = histBounds[i]
			}
			frac := (rank - float64(cum)) / float64(n)
			return lo + (hi-lo)*frac, true
		}
		cum += n
	}
	return histBounds[len(histBounds)-1], true
}

// Below estimates how many observations are at or below us microseconds.
func (h Hist) Below(us float64) float64 {
	var n float64
	for i, c := range h {
		if c == 0 {
			continue
		}
		lo := 0.0
		if i > 0 {
			lo = histBounds[i-1]
		}
		hi := lo * math.Sqrt2
		if i < len(histBounds) {
			hi = histBounds[i]
		}
		switch {
		case us >= hi:
			n += float64(c)
		case us > lo:
			n += float64(c) * (us - lo) / (hi - lo)
		}
	}
	return n
}

// --- writes ---------------------------------------------------------------

type statKey struct {
	ts            int64
	service, name string
}

type statAcc struct {
	count, errors, sumUS int64
	hist                 Hist
}

// InsertSpans stores the sampled spans and adds every entry span to the
// per-minute statistics.
func (s *Store) InsertSpans(ctx context.Context, spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	stats := map[statKey]*statAcc{}
	for _, sp := range spans {
		if !sp.Entry() {
			continue
		}
		k := statKey{(sp.Start.UnixMilli() / 60000) * 60000, sp.Service, truncate(sp.Name, 200)}
		a := stats[k]
		if a == nil {
			a = &statAcc{hist: newHist()}
			stats[k] = a
		}
		a.count++
		if sp.Status == SpanError {
			a.errors++
		}
		a.sumUS += sp.DurationUS
		a.hist[histIndex(sp.DurationUS)]++
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.PrepareContext(ctx, `INSERT INTO spans (trace_id, span_id, parent_id, service, env, version, name, kind, start_us, dur_us, status, status_msg, attrs, events)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, sp := range spans {
		if !sp.Sampled {
			continue
		}
		attrs, _ := json.Marshal(sp.Attrs)
		evs := sp.Events
		if evs == nil {
			evs = []SpanEvent{}
		}
		events, _ := json.Marshal(evs)
		if _, err := ins.ExecContext(ctx, sp.TraceID, sp.SpanID, sp.ParentID, sp.Service, sp.Env, sp.Version, sp.Name, sp.Kind,
			sp.Start.UnixMicro(), sp.DurationUS, sp.Status, sp.StatusMsg, string(attrs), string(events)); err != nil {
			return err
		}
	}
	for k, a := range stats {
		var hist string
		var count, errs, sum int64
		err := tx.QueryRowContext(ctx, `SELECT count, errors, sum_us, hist FROM span_stats WHERE ts = ? AND service = ? AND name = ?`, k.ts, k.service, k.name).
			Scan(&count, &errs, &sum, &hist)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.ExecContext(ctx, `INSERT INTO span_stats (ts, service, name, count, errors, sum_us, hist) VALUES (?, ?, ?, ?, ?, ?, ?)`,
				k.ts, k.service, k.name, a.count, a.errors, a.sumUS, a.hist.String()); err != nil {
				return err
			}
		case err != nil:
			return err
		default:
			h := parseHist(hist)
			h.add(a.hist)
			if _, err := tx.ExecContext(ctx, `UPDATE span_stats SET count = ?, errors = ?, sum_us = ?, hist = ? WHERE ts = ? AND service = ? AND name = ?`,
				count+a.count, errs+a.errors, sum+a.sumUS, h.String(), k.ts, k.service, k.name); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// PruneTraces deletes spans that started before t.
func (s *Store) PruneTraces(ctx context.Context, before time.Time) (int64, error) {
	var total int64
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM spans WHERE rowid IN (SELECT rowid FROM spans WHERE start_us < ? LIMIT 5000)`, before.UnixMicro())
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 5000 || ctx.Err() != nil {
			return total, ctx.Err()
		}
	}
}

// PruneSpanStats deletes request statistics older than t.
func (s *Store) PruneSpanStats(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM span_stats WHERE ts < ?`, ms(before))
	return err
}

// --- traces ---------------------------------------------------------------

const spanCols = `SELECT trace_id, span_id, parent_id, service, env, version, name, kind, start_us, dur_us, status, status_msg, attrs, events FROM spans`

func scanSpan(r scanner) (Span, error) {
	var sp Span
	var start int64
	var attrs, events string
	if err := r.Scan(&sp.TraceID, &sp.SpanID, &sp.ParentID, &sp.Service, &sp.Env, &sp.Version, &sp.Name, &sp.Kind, &start, &sp.DurationUS,
		&sp.Status, &sp.StatusMsg, &attrs, &events); err != nil {
		return sp, err
	}
	sp.Start = time.UnixMicro(start).UTC()
	json.Unmarshal([]byte(attrs), &sp.Attrs)
	json.Unmarshal([]byte(events), &sp.Events)
	return sp, nil
}

// GetTrace returns the stored spans of a trace, earliest first.
func (s *Store) GetTrace(ctx context.Context, traceID string) ([]Span, error) {
	rows, err := s.rdb.QueryContext(ctx, spanCols+` WHERE trace_id = ? ORDER BY start_us, span_id LIMIT 5000`, strings.ToLower(traceID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Span{}
	for rows.Next() {
		sp, err := scanSpan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// TraceQuery finds traces.
type TraceQuery struct {
	Service     string
	Name        string // span name (exact)
	MinDuration time.Duration
	ErrorsOnly  bool
	Attrs       map[string]string
	From, To    time.Time
	Limit       int
}

// TraceSummary is one row of a trace search.
type TraceSummary struct {
	TraceID     string    `json:"trace_id"`
	RootService string    `json:"root_service"`
	RootName    string    `json:"root_name"`
	Start       time.Time `json:"start"`
	DurationUS  int64     `json:"duration_us"`
	Spans       int       `json:"spans"`
	Errors      int       `json:"errors"`
	Services    []string  `json:"services"`
}

// SearchTraces returns the newest traces with a span matching q.
func (s *Store) SearchTraces(ctx context.Context, q TraceQuery) ([]TraceSummary, error) {
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 50
	}
	conds := []string{"start_us >= ?", "start_us < ?"}
	args := []any{q.From.UnixMicro(), q.To.UnixMicro()}
	if q.Service != "" {
		conds = append(conds, "service = ?")
		args = append(args, q.Service)
	}
	if q.Name != "" {
		conds = append(conds, "name = ?")
		args = append(args, q.Name)
	}
	if q.MinDuration > 0 {
		conds = append(conds, "dur_us >= ?")
		args = append(args, q.MinDuration.Microseconds())
	}
	if q.ErrorsOnly {
		conds = append(conds, "status = 2")
	}
	for k, v := range q.Attrs {
		conds = append(conds, "json_extract(attrs, ?) = ?")
		args = append(args, `$."`+strings.ReplaceAll(k, `"`, ``)+`"`, v)
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT trace_id, MAX(start_us) AS st FROM spans WHERE `+strings.Join(conds, " AND ")+
		` GROUP BY trace_id ORDER BY st DESC LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		var st int64
		if err := rows.Scan(&id, &st); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []TraceSummary{}
	for _, id := range ids {
		spans, err := s.GetTrace(ctx, id)
		if err != nil {
			return nil, err
		}
		if len(spans) == 0 {
			continue
		}
		out = append(out, Summarize(spans))
	}
	return out, nil
}

// Summarize describes a trace by its root span.
func Summarize(spans []Span) TraceSummary {
	ids := map[string]bool{}
	for _, sp := range spans {
		ids[sp.SpanID] = true
	}
	t := TraceSummary{TraceID: spans[0].TraceID, Spans: len(spans)}
	var root *Span
	var start, end int64
	svcs := map[string]bool{}
	for i, sp := range spans {
		st := sp.Start.UnixMicro()
		if i == 0 || st < start {
			start = st
		}
		if e := st + sp.DurationUS; e > end {
			end = e
		}
		if sp.Status == SpanError {
			t.Errors++
		}
		if !svcs[sp.Service] {
			svcs[sp.Service] = true
			t.Services = append(t.Services, sp.Service)
		}
		// The root has no parent, or a parent that was not stored.
		if root == nil && (sp.ParentID == "" || !ids[sp.ParentID]) {
			root = &spans[i]
		}
	}
	if root == nil {
		root = &spans[0]
	}
	t.RootService, t.RootName = root.Service, root.Name
	t.Start = time.UnixMicro(start).UTC()
	t.DurationUS = end - start
	return t
}

// --- request statistics ---------------------------------------------------

// OpStats summarizes requests to one operation (entry span name) of a service.
type OpStats struct {
	Service   string  `json:"service"`
	Name      string  `json:"name"`
	Count     int64   `json:"count"`
	Errors    int64   `json:"errors"`
	RatePerS  float64 `json:"rate_per_s"`
	ErrorRate float64 `json:"error_rate"` // 0-1
	AvgMS     float64 `json:"avg_ms"`
	P50MS     float64 `json:"p50_ms"`
	P95MS     float64 `json:"p95_ms"`
	P99MS     float64 `json:"p99_ms"`
}

type opAcc struct {
	count, errors, sum int64
	hist               Hist
}

func (s *Store) spanStatRows(ctx context.Context, service, name string, from, to time.Time, fn func(ts int64, service, name string, count, errs, sum int64, h Hist)) error {
	q := `SELECT ts, service, name, count, errors, sum_us, hist FROM span_stats WHERE ts >= ? AND ts < ?`
	args := []any{ms(from), ms(to)}
	if service != "" {
		q += ` AND service = ?`
		args = append(args, service)
	}
	if name != "" {
		q += ` AND name = ?`
		args = append(args, name)
	}
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ts, count, errs, sum int64
		var svc, n, hist string
		if err := rows.Scan(&ts, &svc, &n, &count, &errs, &sum, &hist); err != nil {
			return err
		}
		fn(ts, svc, n, count, errs, sum, parseHist(hist))
	}
	return rows.Err()
}

// Operations returns request statistics per operation over [from, to),
// busiest first.
func (s *Store) Operations(ctx context.Context, service string, from, to time.Time) ([]OpStats, error) {
	type key struct{ s, n string }
	accs := map[key]*opAcc{}
	err := s.spanStatRows(ctx, service, "", from, to, func(_ int64, svc, name string, count, errs, sum int64, h Hist) {
		k := key{svc, name}
		a := accs[k]
		if a == nil {
			a = &opAcc{hist: newHist()}
			accs[k] = a
		}
		a.count += count
		a.errors += errs
		a.sum += sum
		a.hist.add(h)
	})
	if err != nil {
		return nil, err
	}
	secs := to.Sub(from).Seconds()
	out := []OpStats{}
	for k, a := range accs {
		out = append(out, opStats(k.s, k.n, a, secs))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Service+out[i].Name < out[j].Service+out[j].Name
	})
	return out, nil
}

func opStats(service, name string, a *opAcc, secs float64) OpStats {
	o := OpStats{Service: service, Name: name, Count: a.count, Errors: a.errors}
	if secs > 0 {
		o.RatePerS = float64(a.count) / secs
	}
	if a.count > 0 {
		o.ErrorRate = float64(a.errors) / float64(a.count)
		o.AvgMS = float64(a.sum) / float64(a.count) / 1000
	}
	p50, _ := a.hist.Quantile(0.5)
	p95, _ := a.hist.Quantile(0.95)
	p99, _ := a.hist.Quantile(0.99)
	o.P50MS, o.P95MS, o.P99MS = p50/1000, p95/1000, p99/1000
	return o
}

// OpWindow returns the statistics of one operation (or every operation of a
// service when name is empty) over [from, to), plus its histogram.
func (s *Store) OpWindow(ctx context.Context, service, name string, from, to time.Time) (OpStats, Hist, error) {
	a := &opAcc{hist: newHist()}
	err := s.spanStatRows(ctx, service, name, from, to, func(_ int64, _, _ string, count, errs, sum int64, h Hist) {
		a.count += count
		a.errors += errs
		a.sum += sum
		a.hist.add(h)
	})
	return opStats(service, name, a, to.Sub(from).Seconds()), a.hist, err
}

// SpanMetrics are the series a "spans" dashboard query can chart.
var SpanMetrics = map[string]bool{"rate": true, "count": true, "errors": true, "error_rate": true, "avg": true, "p50": true, "p90": true, "p95": true, "p99": true}

// SpanQuery charts request statistics: rate, errors or latency of entry spans.
type SpanQuery struct {
	Service string   `json:"service"`
	Name    string   `json:"operation"`
	Stat    string   `json:"stat"`     // see SpanMetrics; latencies are in ms
	GroupBy []string `json:"group_by"` // "service" and/or "operation"
	Limit   int      `json:"limit"`
}

// RunSpanQuery evaluates q into one series per group.
func (s *Store) RunSpanQuery(ctx context.Context, q SpanQuery, from, to time.Time, bucket time.Duration) ([]Series, error) {
	if q.Stat == "" {
		q.Stat = "rate"
	}
	if !SpanMetrics[q.Stat] {
		return nil, fmt.Errorf("unknown span statistic %q", q.Stat)
	}
	if q.Limit <= 0 || q.Limit > 50 {
		q.Limit = 10
	}
	if bucket < time.Minute {
		bucket = time.Minute
	}
	b := bucket.Milliseconds()
	start := (ms(from) / b) * b
	n := int((ms(to) - start + b - 1) / b)
	if n > 2000 {
		n = 2000
	}
	groups := map[string]map[int64]*opAcc{}
	err := s.spanStatRows(ctx, q.Service, q.Name, from, to, func(ts int64, svc, name string, count, errs, sum int64, h Hist) {
		var parts []string
		for _, g := range q.GroupBy {
			switch g {
			case "service":
				parts = append(parts, svc)
			case "operation", "name":
				parts = append(parts, name)
			}
		}
		label := strings.Join(parts, " ")
		g := groups[label]
		if g == nil {
			g = map[int64]*opAcc{}
			groups[label] = g
		}
		bk := (ts / b) * b
		a := g[bk]
		if a == nil {
			a = &opAcc{hist: newHist()}
			g[bk] = a
		}
		a.count += count
		a.errors += errs
		a.sum += sum
		a.hist.add(h)
	})
	if err != nil {
		return nil, err
	}
	value := func(a *opAcc) (float64, bool) {
		switch q.Stat {
		case "rate":
			return float64(a.count) / (float64(b) / 1000), true
		case "count":
			return float64(a.count), true
		case "errors":
			return float64(a.errors), true
		case "error_rate":
			if a.count == 0 {
				return 0, false
			}
			return 100 * float64(a.errors) / float64(a.count), true
		case "avg":
			if a.count == 0 {
				return 0, false
			}
			return float64(a.sum) / float64(a.count) / 1000, true
		}
		qv := map[string]float64{"p50": 0.5, "p90": 0.9, "p95": 0.95, "p99": 0.99}[q.Stat]
		v, ok := a.hist.Quantile(qv)
		return v / 1000, ok
	}
	type ranked struct {
		label string
		total int64
	}
	var order []ranked
	for l, g := range groups {
		var t int64
		for _, a := range g {
			t += a.count
		}
		order = append(order, ranked{l, t})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].total != order[j].total {
			return order[i].total > order[j].total
		}
		return order[i].label < order[j].label
	})
	if len(order) > q.Limit {
		order = order[:q.Limit]
	}
	out := []Series{}
	for _, r := range order {
		label := r.label
		if label == "" {
			label = q.Stat
		}
		ser := Series{Label: label, Points: make([]SeriesPoint, 0, n)}
		for i := 0; i < n; i++ {
			t := start + int64(i)*b
			p := SeriesPoint{Time: time.UnixMilli(t).UTC()}
			if a, ok := groups[r.label][t]; ok {
				if v, ok := value(a); ok {
					p.Value = &v
				}
			} else if q.Stat == "rate" || q.Stat == "count" || q.Stat == "errors" {
				z := 0.0
				p.Value = &z
			}
			ser.Points = append(ser.Points, p)
		}
		out = append(out, ser)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}
