package store

import (
	"context"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

func joinFloats(fs []float64) string {
	parts := make([]string, len(fs))
	for i, f := range fs {
		parts[i] = strconv.FormatFloat(f, 'g', -1, 64)
	}
	return strings.Join(parts, ",")
}

func joinUints(us []uint64) string {
	parts := make([]string, len(us))
	for i, u := range us {
		parts[i] = strconv.FormatUint(u, 10)
	}
	return strings.Join(parts, ",")
}

func splitFloats(s string) []float64 {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]float64, len(parts))
	for i, p := range parts {
		out[i], _ = strconv.ParseFloat(p, 64)
	}
	return out
}

func splitUints(s string) []uint64 {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]uint64, len(parts))
	for i, p := range parts {
		out[i], _ = strconv.ParseUint(p, 10, 64)
	}
	return out
}

// histAggs are aggregations computed from histogram buckets.
var histAggs = map[string]float64{"p50": 0.5, "p75": 0.75, "p90": 0.9, "p95": 0.95, "p99": 0.99, "heatmap": -1}

// IsHistAgg reports whether agg needs histogram buckets.
func IsHistAgg(agg string) bool { _, ok := histAggs[agg]; return ok }

// BucketQuantile estimates a quantile from explicit-bound bucket counts like
// Prometheus' histogram_quantile: linear interpolation within the bucket,
// the +Inf bucket answers with the highest finite bound.
func BucketQuantile(q float64, bounds []float64, counts []uint64) (float64, bool) {
	var total uint64
	for _, c := range counts {
		total += c
	}
	if total == 0 || len(bounds) == 0 {
		return 0, false
	}
	rank := q * float64(total)
	var cum uint64
	for i, c := range counts {
		if float64(cum+c) >= rank && c > 0 {
			if i >= len(bounds) {
				return bounds[len(bounds)-1], true
			}
			lo := 0.0
			if i > 0 {
				lo = bounds[i-1]
			} else if bounds[0] < 0 {
				lo = bounds[0]
			}
			hi := bounds[i]
			return lo + (hi-lo)*(rank-float64(cum))/float64(c), true
		}
		cum += c
	}
	return bounds[len(bounds)-1], true
}

type histAcc struct {
	bounds []float64
	counts []uint64
}

func (h *histAcc) add(bounds []float64, counts []uint64) {
	if h.counts == nil {
		h.bounds = bounds
		h.counts = make([]uint64, len(counts))
	}
	if len(counts) != len(h.counts) {
		return // other bucket layout: skipped
	}
	for i, c := range counts {
		h.counts[i] += c
	}
}

// runHistQuery answers percentile and heatmap queries from metric_hist.
// Percentiles give one series per group; a heatmap gives one series per
// bucket (labelled by its upper bound, lowest first) with counts.
func (s *Store) runHistQuery(ctx context.Context, q MetricQuery, from, to time.Time, bucket time.Duration) ([]Series, error) {
	b := bucket.Milliseconds()
	start := (ms(from) / b) * b
	n := int((ms(to) - start + b - 1) / b)
	if n > 2000 {
		n = 2000
	}
	sqlq := `SELECT ts, service, attrs, bounds, counts FROM metric_hist WHERE name = ? AND ts >= ? AND ts < ?`
	args := []any{q.Metric, ms(from), ms(to)}
	if q.Service != "" {
		sqlq += ` AND service = ?`
		args = append(args, q.Service)
	}
	rows, err := s.rdb.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	groups := map[string]map[int64]*histAcc{}
	keep := map[string]bool{}
	labels := map[string]string{}
	for rows.Next() {
		var t int64
		var svc, attrs, bounds, counts string
		if err := rows.Scan(&t, &svc, &attrs, &bounds, &counts); err != nil {
			return nil, err
		}
		k := svc + "\x00" + attrs
		ok, seen := keep[k]
		if !seen {
			a := ParseAttrs(attrs)
			ok = true
			for _, f := range q.Filters {
				if !f.match(a) {
					ok = false
					break
				}
			}
			keep[k] = ok
			labels[k] = groupLabel(q, svc, a)
		}
		if !ok {
			continue
		}
		label := labels[k]
		if q.Agg == "heatmap" {
			label = ""
		}
		g := groups[label]
		if g == nil {
			g = map[int64]*histAcc{}
			groups[label] = g
		}
		bk := (t / b) * b
		acc := g[bk]
		if acc == nil {
			acc = &histAcc{}
			g[bk] = acc
		}
		acc.add(splitFloats(bounds), splitUints(counts))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if q.Agg == "heatmap" {
		return heatmapSeries(groups[""], start, b, n), nil
	}
	qv := histAggs[q.Agg]
	out := []Series{}
	for label, g := range groups {
		if label == "" {
			label = q.Agg + " " + q.Metric
		}
		ser := Series{Label: label, Points: make([]SeriesPoint, 0, n)}
		for i := 0; i < n; i++ {
			t := start + int64(i)*b
			p := SeriesPoint{Time: time.UnixMilli(t).UTC()}
			if acc, ok := g[t]; ok {
				if v, ok := BucketQuantile(qv, acc.bounds, acc.counts); ok {
					p.Value = &v
				}
			}
			ser.Points = append(ser.Points, p)
		}
		out = append(out, ser)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	if len(out) > q.Limit && q.Limit > 0 {
		out = out[:q.Limit]
	}
	return out, nil
}

// heatmapSeries turns per-time-bucket histograms into one series per value
// bucket, dropping buckets that are empty in the whole range.
func heatmapSeries(g map[int64]*histAcc, start, b int64, n int) []Series {
	var bounds []float64
	for _, acc := range g {
		if len(acc.bounds) > len(bounds) {
			bounds = acc.bounds
		}
	}
	if len(bounds) == 0 {
		return []Series{}
	}
	out := []Series{}
	for bi := 0; bi <= len(bounds); bi++ {
		label := "+Inf"
		if bi < len(bounds) {
			label = "≤ " + strconv.FormatFloat(bounds[bi], 'g', 4, 64)
		}
		ser := Series{Label: label, Points: make([]SeriesPoint, 0, n)}
		any := false
		for i := 0; i < n; i++ {
			t := start + int64(i)*b
			p := SeriesPoint{Time: time.UnixMilli(t).UTC()}
			if acc, ok := g[t]; ok && len(acc.counts) == len(bounds)+1 {
				v := float64(acc.counts[bi])
				p.Value = &v
				if v > 0 {
					any = true
				}
			}
			ser.Points = append(ser.Points, p)
		}
		if any {
			out = append(out, ser)
		}
	}
	return out
}

// SpanHeatmap returns request latency (ms) as a heatmap: one series per
// latency bucket with request counts.
func (s *Store) SpanHeatmap(ctx context.Context, service, name string, from, to time.Time, bucket time.Duration) ([]Series, error) {
	if bucket < time.Minute {
		bucket = time.Minute
	}
	b := bucket.Milliseconds()
	start := (ms(from) / b) * b
	n := int((ms(to) - start + b - 1) / b)
	if n > 2000 {
		n = 2000
	}
	g := map[int64]*histAcc{}
	boundsMS := make([]float64, len(histBounds))
	for i, v := range histBounds {
		boundsMS[i] = math.Round(v/10) / 100
	}
	err := s.spanStatRows(ctx, service, name, from, to, func(ts int64, _, _ string, _, _, _ int64, h Hist) {
		bk := (ts / b) * b
		acc := g[bk]
		if acc == nil {
			acc = &histAcc{}
			g[bk] = acc
		}
		c := make([]uint64, len(h))
		for i, v := range h {
			c[i] = uint64(v)
		}
		acc.add(boundsMS, c)
	})
	if err != nil {
		return nil, err
	}
	return heatmapSeries(g, start, b, n), nil
}

// histAggregate is a quantile of a histogram metric over a window (alarms).
func (s *Store) histAggregate(ctx context.Context, service, name string, q float64, from, to time.Time) (float64, bool, error) {
	sqlq := `SELECT bounds, counts FROM metric_hist WHERE name = ? AND ts >= ? AND ts < ?`
	args := []any{name, ms(from), ms(to)}
	if service != "" {
		sqlq += ` AND service = ?`
		args = append(args, service)
	}
	rows, err := s.rdb.QueryContext(ctx, sqlq, args...)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	acc := &histAcc{}
	for rows.Next() {
		var b, c string
		if err := rows.Scan(&b, &c); err != nil {
			return 0, false, err
		}
		acc.add(splitFloats(b), splitUints(c))
	}
	if err := rows.Err(); err != nil {
		return 0, false, err
	}
	v, ok := BucketQuantile(q, acc.bounds, acc.counts)
	return v, ok, nil
}
