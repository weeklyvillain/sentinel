package store

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// LogRecord is one stored (redacted) log line or error span.
type LogRecord struct {
	ID           int64             `json:"id"`
	Time         time.Time         `json:"time"`
	Service      string            `json:"service"`
	Env          string            `json:"env,omitempty"`
	Kind         string            `json:"kind"` // log | span
	Severity     int32             `json:"severity"`
	SeverityText string            `json:"severity_text,omitempty"`
	Body         string            `json:"body"`
	TraceID      string            `json:"trace_id,omitempty"`
	SpanID       string            `json:"span_id,omitempty"`
	Attrs        map[string]string `json:"attrs,omitempty"`
	Fingerprint  string            `json:"fingerprint,omitempty"`
}

// InsertLogs stores a batch of records in one transaction.
func (s *Store) InsertLogs(ctx context.Context, recs []LogRecord) error {
	if len(recs) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO logs (ts, service, env, kind, severity, severity_text, body, trace_id, span_id, attrs, fingerprint)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, r := range recs {
		attrs, _ := json.Marshal(r.Attrs)
		if r.Kind == "" {
			r.Kind = "log"
		}
		if _, err := stmt.ExecContext(ctx, ms(r.Time), r.Service, r.Env, r.Kind, r.Severity, r.SeverityText, r.Body,
			r.TraceID, r.SpanID, string(attrs), r.Fingerprint); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LogQuery filters logs. Zero values mean "no filter".
type LogQuery struct {
	Q           string    // search language, see ParseLogQuery
	Services    []string  // any of
	Levels      []string  // any of: debug, info, warn, error, fatal
	MinSeverity int32     // severity_number >=
	From, To    time.Time // [From, To)
	TraceID     string    //
	Fingerprint string    // logs belonging to a group
	// Fingerprints, if set, is used instead of Fingerprint: a group and the
	// fingerprints merged into it.
	Fingerprints []string
	Kind         string            // log | span
	Attrs        map[string]string // exact attribute matches
	// Cursor for pagination (results older than this record).
	BeforeTime time.Time
	BeforeID   int64
	Limit      int
}

func (q LogQuery) where() (string, []any, error) {
	var conds []string
	var args []any
	if strings.TrimSpace(q.Q) != "" {
		n, err := ParseLogQuery(q.Q)
		if err != nil {
			return "", nil, err
		}
		cond, a, err := compileLogQuery(n)
		if err != nil {
			return "", nil, err
		}
		if cond != "" {
			conds = append(conds, cond)
			args = append(args, a...)
		}
	}
	if len(q.Levels) > 0 {
		var ors []string
		for _, l := range q.Levels {
			if lvl, ok := normLevel(l); ok {
				ors = append(ors, levelCond(lvl))
			}
		}
		if len(ors) > 0 {
			conds = append(conds, "("+strings.Join(ors, " OR ")+")")
		}
	}
	if len(q.Services) > 0 {
		conds = append(conds, "service IN ("+placeholders(len(q.Services))+")")
		for _, s := range q.Services {
			args = append(args, s)
		}
	}
	if q.MinSeverity > 0 {
		conds = append(conds, "severity >= ?")
		args = append(args, q.MinSeverity)
	}
	if !q.From.IsZero() {
		conds = append(conds, "ts >= ?")
		args = append(args, ms(q.From))
	}
	if !q.To.IsZero() {
		conds = append(conds, "ts < ?")
		args = append(args, ms(q.To))
	}
	if q.TraceID != "" {
		conds = append(conds, "trace_id = ?")
		args = append(args, strings.ToLower(q.TraceID))
	}
	if len(q.Fingerprints) > 0 {
		conds = append(conds, "fingerprint IN ("+placeholders(len(q.Fingerprints))+")")
		for _, f := range q.Fingerprints {
			args = append(args, f)
		}
	} else if q.Fingerprint != "" {
		conds = append(conds, "fingerprint = ?")
		args = append(args, q.Fingerprint)
	}
	if q.Kind != "" {
		conds = append(conds, "kind = ?")
		args = append(args, q.Kind)
	}
	for k, v := range q.Attrs {
		conds = append(conds, "json_extract(attrs, ?) = ?")
		args = append(args, `$."`+strings.ReplaceAll(k, `"`, ``)+`"`, v)
	}
	if !q.BeforeTime.IsZero() {
		conds = append(conds, "(ts < ? OR (ts = ? AND id < ?))")
		args = append(args, ms(q.BeforeTime), ms(q.BeforeTime), q.BeforeID)
	}
	if len(conds) == 0 {
		return "", nil, nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

// SearchLogs returns matching logs, newest first.
func (s *Store) SearchLogs(ctx context.Context, q LogQuery) ([]LogRecord, error) {
	if q.Limit <= 0 || q.Limit > 1000 {
		q.Limit = 100
	}
	where, args, err := q.where()
	if err != nil {
		return nil, err
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, ts, service, env, kind, severity, severity_text, body, trace_id, span_id, attrs, fingerprint
		FROM logs`+where+` ORDER BY ts DESC, id DESC LIMIT ?`, append(args, q.Limit)...)
	if err != nil {
		return nil, fmt.Errorf("search logs: %w", err)
	}
	defer rows.Close()
	out := []LogRecord{}
	for rows.Next() {
		var r LogRecord
		var ts int64
		var attrs string
		if err := rows.Scan(&r.ID, &ts, &r.Service, &r.Env, &r.Kind, &r.Severity, &r.SeverityText, &r.Body, &r.TraceID, &r.SpanID, &attrs, &r.Fingerprint); err != nil {
			return nil, err
		}
		r.Time = fromMs(ts)
		json.Unmarshal([]byte(attrs), &r.Attrs)
		out = append(out, r)
	}
	return out, rows.Err()
}

// Severity levels used for charts.
var Levels = []string{"debug", "info", "warn", "error", "fatal"}

const levelExpr = `CASE WHEN severity >= 21 THEN 'fatal' WHEN severity >= 17 THEN 'error' WHEN severity >= 13 THEN 'warn' WHEN severity >= 9 OR severity = 0 THEN 'info' ELSE 'debug' END`

// Bucket is one time bucket of a histogram, keyed by series name.
type Bucket struct {
	Time   time.Time        `json:"t"`
	Counts map[string]int64 `json:"counts"`
}

// LogHistogram counts matching logs per time bucket, split by level
// (by="level") or by service (by="service").
func (s *Store) LogHistogram(ctx context.Context, q LogQuery, bucket time.Duration, by string) ([]Bucket, error) {
	if bucket < time.Second {
		bucket = time.Minute
	}
	key := levelExpr
	if by == "service" {
		key = "service"
	}
	q.BeforeTime = time.Time{}
	where, args, err := q.where()
	if err != nil {
		return nil, err
	}
	b := bucket.Milliseconds()
	rows, err := s.rdb.QueryContext(ctx, fmt.Sprintf(`SELECT (ts / %d) * %d AS b, %s AS k, COUNT(*) FROM logs%s GROUP BY b, k ORDER BY b`, b, b, key, where), args...)
	if err != nil {
		return nil, fmt.Errorf("log histogram: %w", err)
	}
	defer rows.Close()
	return fillBuckets(rows, q.From, q.To, bucket)
}

type rowScanner interface {
	Next() bool
	Scan(...any) error
	Err() error
}

// fillBuckets turns (bucket, key, count) rows into a dense series from..to.
func fillBuckets(rows rowScanner, from, to time.Time, bucket time.Duration) ([]Bucket, error) {
	byTime := map[int64]map[string]int64{}
	for rows.Next() {
		var t int64
		var k string
		var n int64
		if err := rows.Scan(&t, &k, &n); err != nil {
			return nil, err
		}
		if byTime[t] == nil {
			byTime[t] = map[string]int64{}
		}
		byTime[t][k] += n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	b := bucket.Milliseconds()
	out := []Bucket{}
	if from.IsZero() || to.IsZero() {
		for t, c := range byTime {
			out = append(out, Bucket{Time: fromMs(t), Counts: c})
		}
		sortBuckets(out)
		return out, nil
	}
	start := (ms(from) / b) * b
	for t := start; t < ms(to); t += b {
		c := byTime[t]
		if c == nil {
			c = map[string]int64{}
		}
		out = append(out, Bucket{Time: time.UnixMilli(t).UTC(), Counts: c})
		if len(out) > 2000 {
			break
		}
	}
	return out, nil
}

func sortBuckets(bs []Bucket) {
	for i := 1; i < len(bs); i++ {
		for j := i; j > 0 && bs[j].Time.Before(bs[j-1].Time); j-- {
			bs[j], bs[j-1] = bs[j-1], bs[j]
		}
	}
}

// CountLogs counts logs matching q.
func (s *Store) CountLogs(ctx context.Context, q LogQuery) (int64, error) {
	q.BeforeTime = time.Time{}
	where, args, err := q.where()
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.rdb.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs`+where, args...).Scan(&n)
	return n, err
}

// LogFacets returns attribute keys, and the fields of JSON log lines, seen
// in recent logs (for filter suggestions).
func (s *Store) LogFacets(ctx context.Context, since time.Time) ([]string, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT DISTINCT j.key FROM (SELECT attrs FROM logs WHERE ts >= ? ORDER BY id DESC LIMIT 2000) l, json_each(l.attrs) j WHERE j.key IS NOT NULL ORDER BY j.key LIMIT 300`, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keys, err := s.logJSONKeys(ctx, since)
	if err != nil {
		return out, nil // attribute keys are still useful on their own
	}
	return mergeKeys(out, keys), nil
}

// Prune deletes logs and metric points older than the given cutoffs, in
// chunks so the writer is never held for long. It returns rows deleted.
func (s *Store) Prune(ctx context.Context, logsBefore, metricsBefore time.Time) (int64, error) {
	var total int64
	for _, q := range []struct {
		sql string
		t   time.Time
	}{
		{`DELETE FROM logs WHERE id IN (SELECT id FROM logs WHERE ts < ? LIMIT 5000)`, logsBefore},
		{`DELETE FROM metric_points WHERE rowid IN (SELECT rowid FROM metric_points WHERE ts < ? LIMIT 20000)`, metricsBefore},
		{`DELETE FROM metric_hist WHERE rowid IN (SELECT rowid FROM metric_hist WHERE ts < ? LIMIT 20000)`, metricsBefore},
		{`DELETE FROM span_stats WHERE rowid IN (SELECT rowid FROM span_stats WHERE ts < ? LIMIT 20000)`, metricsBefore},
		{`DELETE FROM webhook_deliveries WHERE ts < ?`, logsBefore},
		{`DELETE FROM alarm_events WHERE ts < ?`, metricsBefore.Add(-30 * 24 * time.Hour)},
		{`DELETE FROM check_results WHERE rowid IN (SELECT rowid FROM check_results WHERE ts < ? LIMIT 20000)`, metricsBefore},
		{`DELETE FROM sessions WHERE expires_at < ?`, time.Now()},
	} {
		if q.t.IsZero() {
			continue
		}
		for {
			if ctx.Err() != nil {
				return total, ctx.Err()
			}
			res, err := s.db.ExecContext(ctx, q.sql, ms(q.t))
			if err != nil {
				return total, err
			}
			n, _ := res.RowsAffected()
			total += n
			if n < 5000 {
				break
			}
		}
	}
	return total, nil
}
