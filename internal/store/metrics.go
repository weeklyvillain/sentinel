package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MetricPoint is one stored data point. Counters are stored as deltas,
// histograms/summaries as the mean of the interval.
type MetricPoint struct {
	Time    time.Time
	Service string
	Name    string
	Unit    string
	Kind    string // gauge | counter | histogram | summary
	Desc    string
	Attrs   string // canonical "k=v,k=v"
	Value   float64
	// Histograms: bucket upper bounds and the counts of this interval
	// (len(Counts) == len(Bounds)+1, the last bucket is +Inf).
	Bounds []float64
	Counts []uint64
}

const schemaMetricHist = `
CREATE TABLE IF NOT EXISTS metric_hist (
	ts INTEGER NOT NULL,
	service TEXT NOT NULL,
	name TEXT NOT NULL,
	attrs TEXT NOT NULL DEFAULT '',
	bounds TEXT NOT NULL,
	counts TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS metric_hist_series ON metric_hist(name, ts);
CREATE INDEX IF NOT EXISTS metric_hist_ts ON metric_hist(ts);
`

// MetricSeries describes a metric a service reports.
type MetricSeries struct {
	Service     string    `json:"service"`
	Name        string    `json:"name"`
	Unit        string    `json:"unit,omitempty"`
	Kind        string    `json:"kind"`
	Description string    `json:"description,omitempty"`
	LastSeen    time.Time `json:"last_seen"`
}

// InsertMetrics stores a batch of points and updates the series catalog.
func (s *Store) InsertMetrics(ctx context.Context, pts []MetricPoint) error {
	if len(pts) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.PrepareContext(ctx, `INSERT INTO metric_points (ts, service, name, attrs, value) VALUES (?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer ins.Close()
	type key struct{ s, n string }
	seen := map[key]MetricPoint{}
	var hist *sql.Stmt
	for _, p := range pts {
		if _, err := ins.ExecContext(ctx, ms(p.Time), p.Service, p.Name, p.Attrs, p.Value); err != nil {
			return err
		}
		if len(p.Counts) > 0 {
			if hist == nil {
				if hist, err = tx.PrepareContext(ctx, `INSERT INTO metric_hist (ts, service, name, attrs, bounds, counts) VALUES (?, ?, ?, ?, ?, ?)`); err != nil {
					return err
				}
				defer hist.Close()
			}
			if _, err := hist.ExecContext(ctx, ms(p.Time), p.Service, p.Name, p.Attrs, joinFloats(p.Bounds), joinUints(p.Counts)); err != nil {
				return err
			}
		}
		k := key{p.Service, p.Name}
		if prev, ok := seen[k]; !ok || p.Time.After(prev.Time) {
			seen[k] = p
		}
	}
	for _, p := range seen {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metric_series (service, name, unit, kind, description, last_seen) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT(service, name) DO UPDATE SET unit = excluded.unit, kind = excluded.kind, description = excluded.description,
			last_seen = MAX(last_seen, excluded.last_seen)`, p.Service, p.Name, p.Unit, p.Kind, p.Desc, ms(p.Time)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ListMetricSeries returns known metrics, optionally for one service.
func (s *Store) ListMetricSeries(ctx context.Context, service string) ([]MetricSeries, error) {
	q := `SELECT service, name, unit, kind, description, last_seen FROM metric_series`
	var args []any
	if service != "" {
		q += ` WHERE service = ?`
		args = append(args, service)
	}
	q += ` ORDER BY service, name`
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []MetricSeries{}
	for rows.Next() {
		var m MetricSeries
		var ls int64
		if err := rows.Scan(&m.Service, &m.Name, &m.Unit, &m.Kind, &m.Description, &ls); err != nil {
			return nil, err
		}
		m.LastSeen = fromMs(ls)
		out = append(out, m)
	}
	return out, rows.Err()
}

// SeriesPoint is one aggregated bucket.
type SeriesPoint struct {
	Time  time.Time `json:"t"`
	Value *float64  `json:"v"` // nil = no data
}

// Series is one line on a chart.
type Series struct {
	Label  string        `json:"label"`
	Points []SeriesPoint `json:"points"`
}

var aggs = map[string]string{"avg": "AVG(value)", "sum": "SUM(value)", "max": "MAX(value)", "min": "MIN(value)", "count": "COUNT(*)", "last": "MAX(value)"}

// MetricSeriesQuery aggregates a metric into buckets. With split=true it
// returns one series per attribute set (top 8 by volume), else one total.
// service may be empty for all services.
func (s *Store) MetricSeriesQuery(ctx context.Context, service, name, agg string, from, to time.Time, bucket time.Duration, split bool) ([]Series, error) {
	expr, ok := aggs[agg]
	if !ok {
		return nil, fmt.Errorf("unknown aggregation %q", agg)
	}
	if bucket < time.Second {
		bucket = time.Minute
	}
	b := bucket.Milliseconds()
	cond := `name = ? AND ts >= ? AND ts < ?`
	args := []any{name, ms(from), ms(to)}
	if service != "" {
		cond = `service = ? AND ` + cond
		args = append([]any{service}, args...)
	}
	labelExpr := `''`
	if split {
		labelExpr = `attrs`
		if service == "" {
			labelExpr = `service || CASE WHEN attrs <> '' THEN ' ' || attrs ELSE '' END`
		}
	}
	var labels []string
	if split {
		rows, err := s.rdb.QueryContext(ctx, fmt.Sprintf(`SELECT %s AS l FROM metric_points WHERE %s GROUP BY l ORDER BY COUNT(*) DESC LIMIT 8`, labelExpr, cond), args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var l string
			rows.Scan(&l)
			labels = append(labels, l)
		}
		rows.Close()
	} else {
		labels = []string{""}
	}
	start := (ms(from) / b) * b
	n := int((ms(to) - start + b - 1) / b)
	if n > 2000 {
		n = 2000
	}
	out := []Series{}
	for _, l := range labels {
		q := fmt.Sprintf(`SELECT (ts / %d) * %d AS bk, %s FROM metric_points WHERE %s`, b, b, expr, cond)
		a := append([]any{}, args...)
		if split {
			q += fmt.Sprintf(` AND %s = ?`, labelExpr)
			a = append(a, l)
		}
		q += ` GROUP BY bk`
		rows, err := s.rdb.QueryContext(ctx, q, a...)
		if err != nil {
			return nil, err
		}
		vals := map[int64]float64{}
		for rows.Next() {
			var t int64
			var v float64
			if err := rows.Scan(&t, &v); err != nil {
				rows.Close()
				return nil, err
			}
			vals[t] = v
		}
		rows.Close()
		ser := Series{Label: l, Points: make([]SeriesPoint, 0, n)}
		if ser.Label == "" {
			ser.Label = name
		}
		for i := 0; i < n; i++ {
			t := start + int64(i)*b
			p := SeriesPoint{Time: time.UnixMilli(t).UTC()}
			if v, ok := vals[t]; ok {
				v := v
				p.Value = &v
			}
			ser.Points = append(ser.Points, p)
		}
		out = append(out, ser)
	}
	return out, nil
}

// MetricAggregate returns agg(value) over a window (for alarms). ok=false when there is no data.
func (s *Store) MetricAggregate(ctx context.Context, service, name, agg string, from, to time.Time) (float64, bool, error) {
	if q, ok := histAggs[agg]; ok && q > 0 {
		return s.histAggregate(ctx, service, name, q, from, to)
	}
	expr, okA := aggs[agg]
	if !okA {
		return 0, false, fmt.Errorf("unknown aggregation %q", agg)
	}
	q := fmt.Sprintf(`SELECT %s, COUNT(*) FROM metric_points WHERE name = ? AND ts >= ? AND ts < ?`, expr)
	args := []any{name, ms(from), ms(to)}
	if service != "" {
		q += ` AND service = ?`
		args = append(args, service)
	}
	if agg == "last" {
		q = `SELECT value, 1 FROM metric_points WHERE name = ? AND ts >= ? AND ts < ?`
		if service != "" {
			q += ` AND service = ?`
		}
		q += ` ORDER BY ts DESC LIMIT 1`
	}
	var v *float64
	var n int64
	err := s.rdb.QueryRowContext(ctx, q, args...).Scan(&v, &n)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	if v == nil || n == 0 {
		return 0, false, nil
	}
	return *v, true, nil
}
