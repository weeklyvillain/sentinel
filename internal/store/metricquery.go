package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// AttrFilter keeps (=) or drops (!=) points by an attribute. Value may list
// alternatives separated by "|".
type AttrFilter struct {
	Key   string `json:"key"`
	Op    string `json:"op"` // = | !=
	Value string `json:"value"`
}

// MetricQuery selects and aggregates one metric for a dashboard panel.
//
// Aggregation happens in two steps, so gauges and counters both come out
// right: first each series (one attribute set of one service) is reduced
// per time bucket with Agg, then the series of a group are combined with
// Combine. "rate" is per-second change: for OTLP delta counters the bucket's
// sum divided by its length, for cumulative counters (Prometheus *_total
// scraped as gauges) the increase between points, tolerating resets.
type MetricQuery struct {
	Metric  string       `json:"metric"`
	Service string       `json:"service"`
	Filters []AttrFilter `json:"filters"`
	Agg     string       `json:"agg"`      // avg | min | max | sum | last | count | rate; histograms also p50 | p75 | p90 | p95 | p99 | heatmap
	Combine string       `json:"combine"`  // sum | avg | min | max; default sum for sum/count/rate, else avg
	GroupBy []string     `json:"group_by"` // attribute keys and/or "service"
	Limit   int          `json:"limit"`    // most significant groups kept (default 10)
}

var validAggs = map[string]bool{"avg": true, "min": true, "max": true, "sum": true, "last": true, "count": true, "rate": true}

// Validate checks the query.
func (q *MetricQuery) Validate() error {
	if q.Metric == "" {
		return fmt.Errorf("a metric is required")
	}
	if q.Agg == "" {
		q.Agg = "avg"
	}
	if !validAggs[q.Agg] && !IsHistAgg(q.Agg) {
		return fmt.Errorf("unknown aggregation %q", q.Agg)
	}
	if q.Combine == "" {
		q.Combine = "avg"
		if q.Agg == "sum" || q.Agg == "count" || q.Agg == "rate" {
			q.Combine = "sum"
		}
	}
	switch q.Combine {
	case "sum", "avg", "min", "max":
	default:
		return fmt.Errorf("unknown combine %q", q.Combine)
	}
	for _, f := range q.Filters {
		if f.Op != "=" && f.Op != "!=" {
			return fmt.Errorf("filter %q: op must be = or !=", f.Key)
		}
	}
	if q.Limit <= 0 || q.Limit > 50 {
		q.Limit = 10
	}
	return nil
}

// ParseAttrs splits a stored "k=v,k=v" attribute string.
func ParseAttrs(s string) map[string]string {
	m := map[string]string{}
	if s == "" {
		return m
	}
	for _, p := range strings.Split(s, ",") {
		if k, v, ok := strings.Cut(p, "="); ok {
			m[k] = v
		}
	}
	return m
}

func (f AttrFilter) match(attrs map[string]string) bool {
	v, has := attrs[f.Key]
	in := false
	if has {
		for _, alt := range strings.Split(f.Value, "|") {
			if v == alt {
				in = true
				break
			}
		}
	}
	if f.Op == "!=" {
		return !in
	}
	return in
}

type bucketAcc struct {
	sum, min, max, last float64
	lastT               int64
	n                   int
}

func (a *bucketAcc) add(t int64, v float64) {
	if a.n == 0 || v < a.min {
		a.min = v
	}
	if a.n == 0 || v > a.max {
		a.max = v
	}
	if a.n == 0 || t >= a.lastT {
		a.last, a.lastT = v, t
	}
	a.sum += v
	a.n++
}

func (a *bucketAcc) value(agg string, bucketSec float64) float64 {
	switch agg {
	case "min":
		return a.min
	case "max":
		return a.max
	case "sum":
		return a.sum
	case "last":
		return a.last
	case "count":
		return float64(a.n)
	case "rate":
		return a.sum / bucketSec
	}
	return a.sum / float64(a.n)
}

// RunMetricQuery evaluates q into one series per group over [from, to).
func (s *Store) RunMetricQuery(ctx context.Context, q MetricQuery, from, to time.Time, bucket time.Duration) ([]Series, error) {
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if bucket < time.Second {
		bucket = time.Minute
	}
	if IsHistAgg(q.Agg) {
		return s.runHistQuery(ctx, q, from, to, bucket)
	}
	b := bucket.Milliseconds()
	start := (ms(from) / b) * b
	n := int((ms(to) - start + b - 1) / b)
	if n > 2000 {
		n = 2000
	}

	// Cumulative counters need the point before the window for the first rate.
	qFrom := ms(from)
	cumulative := false
	if q.Agg == "rate" {
		var kind string
		kq := `SELECT kind FROM metric_series WHERE name = ?`
		ka := []any{q.Metric}
		if q.Service != "" {
			kq += ` AND service = ?`
			ka = append(ka, q.Service)
		}
		s.rdb.QueryRowContext(ctx, kq+` LIMIT 1`, ka...).Scan(&kind)
		cumulative = kind != "counter"
		if cumulative {
			qFrom -= b
		}
	}
	sqlq := `SELECT ts, service, attrs, value FROM metric_points WHERE name = ? AND ts >= ? AND ts < ?`
	args := []any{q.Metric, qFrom, ms(to)}
	if q.Service != "" {
		sqlq += ` AND service = ?`
		args = append(args, q.Service)
	}
	sqlq += ` ORDER BY ts`
	rows, err := s.rdb.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type seriesKey struct{ service, attrs string }
	type seriesState struct {
		group   string
		buckets map[int64]*bucketAcc
		prevT   int64
		prevV   float64
		hasPrev bool
	}
	all := map[seriesKey]*seriesState{}
	parsed := map[string]map[string]string{}
	for rows.Next() {
		var t int64
		var svc, attrs string
		var v float64
		if err := rows.Scan(&t, &svc, &attrs, &v); err != nil {
			return nil, err
		}
		k := seriesKey{svc, attrs}
		st, ok := all[k]
		if !ok {
			a, ok := parsed[attrs]
			if !ok {
				a = ParseAttrs(attrs)
				parsed[attrs] = a
			}
			keep := true
			for _, f := range q.Filters {
				if !f.match(a) {
					keep = false
					break
				}
			}
			if !keep {
				all[k] = nil
				continue
			}
			st = &seriesState{group: groupLabel(q, svc, a), buckets: map[int64]*bucketAcc{}}
			all[k] = st
		}
		if st == nil {
			continue
		}
		val := v
		if q.Agg == "rate" && cumulative {
			if !st.hasPrev {
				st.prevT, st.prevV, st.hasPrev = t, v, true
				continue
			}
			dt := float64(t-st.prevT) / 1000
			inc := v - st.prevV
			if inc < 0 { // counter reset
				inc = v
			}
			st.prevT, st.prevV = t, v
			if dt <= 0 {
				continue
			}
			val = inc / dt
		}
		if t < ms(from) {
			continue
		}
		bk := (t / b) * b
		acc := st.buckets[bk]
		if acc == nil {
			acc = &bucketAcc{}
			st.buckets[bk] = acc
		}
		acc.add(t, val)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Per series per bucket, then combined per group.
	agg := q.Agg
	if q.Agg == "rate" && cumulative {
		agg = "avg" // values are already per-second rates
	}
	bucketSec := float64(b) / 1000
	type groupAcc struct {
		vals   map[int64]*bucketAcc
		weight float64
	}
	groups := map[string]*groupAcc{}
	for _, st := range all {
		if st == nil {
			continue
		}
		g := groups[st.group]
		if g == nil {
			g = &groupAcc{vals: map[int64]*bucketAcc{}}
			groups[st.group] = g
		}
		for bk, acc := range st.buckets {
			v := acc.value(agg, bucketSec)
			ga := g.vals[bk]
			if ga == nil {
				ga = &bucketAcc{}
				g.vals[bk] = ga
			}
			ga.add(bk, v)
		}
	}
	type ranked struct {
		label  string
		weight float64
	}
	var order []ranked
	for label, g := range groups {
		w := 0.0
		for _, acc := range g.vals {
			w += math.Abs(acc.value(q.Combine, 1))
		}
		order = append(order, ranked{label, w})
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].weight != order[j].weight {
			return order[i].weight > order[j].weight
		}
		return order[i].label < order[j].label
	})
	if len(order) > q.Limit {
		order = order[:q.Limit]
	}
	out := []Series{}
	for _, r := range order {
		g := groups[r.label]
		label := r.label
		if label == "" {
			label = q.Metric
		}
		ser := Series{Label: label, Points: make([]SeriesPoint, 0, n)}
		for i := 0; i < n; i++ {
			t := start + int64(i)*b
			p := SeriesPoint{Time: time.UnixMilli(t).UTC()}
			if acc, ok := g.vals[t]; ok {
				v := acc.value(q.Combine, 1)
				p.Value = &v
			}
			ser.Points = append(ser.Points, p)
		}
		out = append(out, ser)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out, nil
}

func groupLabel(q MetricQuery, service string, attrs map[string]string) string {
	if len(q.GroupBy) == 0 {
		return ""
	}
	parts := make([]string, 0, len(q.GroupBy))
	for _, k := range q.GroupBy {
		if k == "service" {
			parts = append(parts, service)
			continue
		}
		v := attrs[k]
		if v == "" {
			v = "-"
		}
		parts = append(parts, v)
	}
	return strings.Join(parts, " ")
}

// MetricAttr is one attribute key of a metric with some of its values.
type MetricAttr struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// MetricAttrs lists the attribute keys (and up to 30 values each) a metric
// has, for building filters and group-bys.
func (s *Store) MetricAttrs(ctx context.Context, service, name string) ([]MetricAttr, error) {
	q := `SELECT DISTINCT attrs FROM metric_points WHERE name = ?`
	args := []any{name}
	if service != "" {
		q += ` AND service = ?`
		args = append(args, service)
	}
	rows, err := s.rdb.QueryContext(ctx, q+` LIMIT 2000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	vals := map[string]map[string]bool{}
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		for k, v := range ParseAttrs(a) {
			if vals[k] == nil {
				vals[k] = map[string]bool{}
			}
			if len(vals[k]) < 30 {
				vals[k][v] = true
			}
		}
	}
	out := []MetricAttr{}
	for k, vs := range vals {
		a := MetricAttr{Key: k, Values: []string{}}
		for v := range vs {
			a.Values = append(a.Values, v)
		}
		sort.Strings(a.Values)
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, rows.Err()
}
