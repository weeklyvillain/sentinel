package store

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// LogMetric is a metric computed from the logs matching a search: how many
// there are, or a statistic of a numeric field (an attribute, or a field of a
// JSON log line), per time bucket, optionally split by a field.
type LogMetric struct {
	Agg   string // count | rate | unique | sum | avg | min | max | p50 | p90 | p95 | p99
	Field string // the numeric (or, for unique, any) field; not for count/rate
	By    string // "" | level | service | env | kind | any attribute
	Limit int    // most series when split (default 10)
}

// LogMetricAggs lists the aggregations, in menu order.
var LogMetricAggs = []string{"count", "rate", "unique", "sum", "avg", "min", "max", "p50", "p90", "p95", "p99"}

var percentiles = map[string]float64{"p50": 50, "p90": 90, "p95": 95, "p99": 99}

// maxMetricRows bounds the rows read for a percentile or a split.
const maxMetricRows = 200000

// LogMetricSeries runs m over the logs matching q, bucketed from q.From to q.To.
func (s *Store) LogMetricSeries(ctx context.Context, q LogQuery, m LogMetric, bucket time.Duration) ([]Series, error) {
	if bucket < time.Second {
		bucket = time.Minute
	}
	if m.Agg == "" {
		m.Agg = "count"
	}
	known := false
	for _, a := range LogMetricAggs {
		known = known || a == m.Agg
	}
	if !known {
		return nil, fmt.Errorf("unknown aggregation %q", m.Agg)
	}
	needsField := m.Agg != "count" && m.Agg != "rate"
	m.Field = strings.TrimSpace(m.Field)
	if needsField && m.Field == "" {
		return nil, fmt.Errorf("%s needs a field, e.g. duration_ms", m.Agg)
	}
	if m.Limit <= 0 || m.Limit > 50 {
		m.Limit = 10
	}
	q.BeforeTime = time.Time{}
	where, args, err := q.where()
	if err != nil {
		return nil, err
	}

	field := strings.TrimPrefix(m.Field, "@")
	// keyExpr and valExpr return SQL and append their arguments; the SELECT
	// list comes before WHERE, so its arguments go first.
	keyExpr := func(a *[]any) string {
		switch strings.ToLower(m.By) {
		case "":
			return "''"
		case "level":
			return levelExpr
		case "service", "env", "kind":
			return strings.ToLower(m.By)
		}
		return "IFNULL(CAST(" + AttrExpr(strings.TrimPrefix(m.By, "@"), a) + " AS TEXT), '')"
	}
	valExpr := func(a *[]any) string { return AttrExpr(field, a) }
	if needsField {
		if where == "" {
			where = " WHERE "
		} else {
			where += " AND "
		}
		where += valExpr(&args) + " IS NOT NULL"
	}
	b := bucket.Milliseconds()

	type cell struct {
		v float64
		n int64
	}
	data := map[string]map[int64]cell{} // key -> bucket -> value
	totals := map[string]int64{}
	put := func(k string, t int64, v float64, n int64) {
		if data[k] == nil {
			data[k] = map[int64]cell{}
		}
		data[k][t] = cell{v, n}
		totals[k] += n
	}

	if p, ok := percentiles[m.Agg]; ok {
		var sel []any
		key := keyExpr(&sel)
		val := valExpr(&sel)
		rows, err := s.rdb.QueryContext(ctx, fmt.Sprintf(`SELECT (ts / %d) * %d AS b, %s AS k, CAST(%s AS REAL) AS v FROM logs%s LIMIT %d`, b, b, key, val, where, maxMetricRows),
			append(sel, args...)...)
		if err != nil {
			return nil, fmt.Errorf("log metric: %w", err)
		}
		defer rows.Close()
		vals := map[string]map[int64][]float64{}
		for rows.Next() {
			var t int64
			var k string
			var v float64
			if err := rows.Scan(&t, &k, &v); err != nil {
				return nil, err
			}
			if vals[k] == nil {
				vals[k] = map[int64][]float64{}
			}
			vals[k][t] = append(vals[k][t], v)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for k, byT := range vals {
			for t, vs := range byT {
				put(k, t, percentile(vs, p), int64(len(vs)))
			}
		}
	} else {
		agg := map[string]string{
			"count": "COUNT(*)", "rate": "COUNT(*)",
			"unique": "COUNT(DISTINCT CAST(%[1]s AS TEXT))",
			"sum":    "SUM(CAST(%[1]s AS REAL))", "avg": "AVG(CAST(%[1]s AS REAL))",
			"min": "MIN(CAST(%[1]s AS REAL))", "max": "MAX(CAST(%[1]s AS REAL))",
		}[m.Agg]
		var sel []any
		key := keyExpr(&sel)
		aggSQL := agg
		if needsField {
			aggSQL = fmt.Sprintf(agg, valExpr(&sel))
		}
		all := append(sel, args...)
		rows, err := s.rdb.QueryContext(ctx, fmt.Sprintf(`SELECT (ts / %d) * %d AS b, %s AS k, %s, COUNT(*) FROM logs%s GROUP BY b, k LIMIT %d`, b, b, key, aggSQL, where, maxMetricRows), all...)
		if err != nil {
			return nil, fmt.Errorf("log metric: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			var t, n int64
			var k string
			var v *float64
			if err := rows.Scan(&t, &k, &v, &n); err != nil {
				return nil, err
			}
			if v == nil {
				continue
			}
			x := *v
			if m.Agg == "rate" {
				x /= bucket.Seconds()
			}
			put(k, t, x, n)
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}

	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if totals[keys[i]] != totals[keys[j]] {
			return totals[keys[i]] > totals[keys[j]]
		}
		return keys[i] < keys[j]
	})
	if len(keys) > m.Limit {
		keys = keys[:m.Limit]
	}
	if m.By == "" && len(keys) == 0 {
		keys = []string{""}
	}
	zero := m.Agg == "count" || m.Agg == "rate" || m.Agg == "unique"
	label := m.Agg
	if needsField {
		label = fmt.Sprintf("%s(%s)", m.Agg, m.Field)
	}
	start, end := (ms(q.From)/b)*b, ms(q.To)
	if q.From.IsZero() || q.To.IsZero() {
		return nil, fmt.Errorf("a time range is required")
	}
	if (end-start)/b > 2000 {
		return nil, fmt.Errorf("too many buckets; use a larger bucket")
	}
	out := []Series{}
	for _, k := range keys {
		ser := Series{Label: label}
		if m.By != "" {
			ser.Label = k
			if k == "" {
				ser.Label = "(none)"
			}
		}
		for t := start; t < end; t += b {
			var v *float64
			if c, ok := data[k][t]; ok {
				x := c.v
				v = &x
			} else if zero {
				x := 0.0
				v = &x
			}
			ser.Points = append(ser.Points, SeriesPoint{Time: time.UnixMilli(t).UTC(), Value: v})
		}
		out = append(out, ser)
	}
	return out, nil
}

// percentile returns the p-th percentile (0-100) of vs by linear interpolation.
func percentile(vs []float64, p float64) float64 {
	sort.Float64s(vs)
	if len(vs) == 1 {
		return vs[0]
	}
	r := p / 100 * float64(len(vs)-1)
	lo := int(math.Floor(r))
	hi := int(math.Ceil(r))
	return vs[lo] + (vs[hi]-vs[lo])*(r-float64(lo))
}
