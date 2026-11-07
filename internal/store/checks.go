package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

const schemaChecks = `
CREATE TABLE IF NOT EXISTS checks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	url TEXT NOT NULL,
	method TEXT NOT NULL DEFAULT 'GET',
	interval_s INTEGER NOT NULL DEFAULT 60,
	timeout_s INTEGER NOT NULL DEFAULT 10,
	expect_status TEXT NOT NULL DEFAULT '2xx',
	body_contains TEXT NOT NULL DEFAULT '',
	service TEXT NOT NULL DEFAULT '',
	failures_before_down INTEGER NOT NULL DEFAULT 2,
	enabled INTEGER NOT NULL DEFAULT 1,
	state TEXT NOT NULL DEFAULT 'pending',
	consecutive_failures INTEGER NOT NULL DEFAULT 0,
	first_failure_at INTEGER NOT NULL DEFAULT 0,
	down_since INTEGER NOT NULL DEFAULT 0,
	last_checked INTEGER NOT NULL DEFAULT 0,
	last_status INTEGER NOT NULL DEFAULT 0,
	last_latency_ms INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS check_results (
	check_id INTEGER NOT NULL REFERENCES checks(id) ON DELETE CASCADE,
	ts INTEGER NOT NULL,
	ok INTEGER NOT NULL,
	status INTEGER NOT NULL DEFAULT 0,
	latency_ms INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS check_results_check_ts ON check_results(check_id, ts);
CREATE INDEX IF NOT EXISTS check_results_ts ON check_results(ts);
`

// Check states.
const (
	CheckPending = "pending" // not probed yet
	CheckUp      = "up"
	CheckDown    = "down"
)

// Check is an HTTP uptime probe run from the Sentinel server.
type Check struct {
	ID                 int64  `json:"id"`
	Name               string `json:"name"`
	URL                string `json:"url"`
	Method             string `json:"method"`        // GET | HEAD
	IntervalS          int64  `json:"interval_s"`    // seconds between probes
	TimeoutS           int64  `json:"timeout_s"`     // per-probe timeout
	ExpectStatus       string `json:"expect_status"` // "2xx", "200", "200,204", "2xx,3xx"
	BodyContains       string `json:"body_contains"` // optional substring
	Service            string `json:"service"`       // groups/routes issues like that service; default = check name
	FailuresBeforeDown int    `json:"failures_before_down"`
	Enabled            bool   `json:"enabled"`

	State               string    `json:"state"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	FirstFailureAt      time.Time `json:"first_failure_at,omitzero"`
	DownSince           time.Time `json:"down_since,omitzero"`
	LastChecked         time.Time `json:"last_checked,omitzero"`
	LastStatus          int       `json:"last_status"`
	LastLatencyMS       int64     `json:"last_latency_ms"`
	LastError           string    `json:"last_error,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
}

// GroupService is the service name a check's outages are filed under.
func (c Check) GroupService() string {
	if c.Service != "" {
		return c.Service
	}
	return c.Name
}

const checkCols = `SELECT id, name, url, method, interval_s, timeout_s, expect_status, body_contains, service, failures_before_down, enabled,
	state, consecutive_failures, first_failure_at, down_since, last_checked, last_status, last_latency_ms, last_error, created_at FROM checks`

func scanCheck(r scanner) (Check, error) {
	var c Check
	var enabled int
	var ff, ds, lc, ca int64
	err := r.Scan(&c.ID, &c.Name, &c.URL, &c.Method, &c.IntervalS, &c.TimeoutS, &c.ExpectStatus, &c.BodyContains, &c.Service,
		&c.FailuresBeforeDown, &enabled, &c.State, &c.ConsecutiveFailures, &ff, &ds, &lc, &c.LastStatus, &c.LastLatencyMS, &c.LastError, &ca)
	if err != nil {
		return c, err
	}
	c.Enabled = enabled == 1
	c.FirstFailureAt, c.DownSince, c.LastChecked, c.CreatedAt = fromMs(ff), fromMs(ds), fromMs(lc), fromMs(ca)
	return c, nil
}

// ListChecks returns all checks.
func (s *Store) ListChecks(ctx context.Context) ([]Check, error) {
	rows, err := s.db.QueryContext(ctx, checkCols+` ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Check{}
	for rows.Next() {
		c, err := scanCheck(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetCheck returns one check.
func (s *Store) GetCheck(ctx context.Context, id int64) (Check, error) {
	c, err := scanCheck(s.db.QueryRowContext(ctx, checkCols+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// SaveCheck inserts (ID == 0) or updates a check's definition. Editing a
// check does not reset its state.
func (s *Store) SaveCheck(ctx context.Context, c Check) (int64, error) {
	if c.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO checks (name, url, method, interval_s, timeout_s, expect_status, body_contains, service,
			failures_before_down, enabled, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			c.Name, c.URL, c.Method, c.IntervalS, c.TimeoutS, c.ExpectStatus, c.BodyContains, c.Service, c.FailuresBeforeDown, boolInt(c.Enabled), ms(time.Now()))
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE checks SET name = ?, url = ?, method = ?, interval_s = ?, timeout_s = ?, expect_status = ?,
		body_contains = ?, service = ?, failures_before_down = ?, enabled = ? WHERE id = ?`,
		c.Name, c.URL, c.Method, c.IntervalS, c.TimeoutS, c.ExpectStatus, c.BodyContains, c.Service, c.FailuresBeforeDown, boolInt(c.Enabled), c.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return c.ID, nil
}

// DeleteCheck removes a check and its results.
func (s *Store) DeleteCheck(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM checks WHERE id = ?`, id)
	return err
}

// CheckResult is one probe.
type CheckResult struct {
	Time      time.Time `json:"t"`
	OK        bool      `json:"ok"`
	Status    int       `json:"status"`
	LatencyMS int64     `json:"latency_ms"`
	Error     string    `json:"error,omitempty"`
}

// RecordCheckResult stores a probe and the check's new state.
func (s *Store) RecordCheckResult(ctx context.Context, c Check, r CheckResult) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO check_results (check_id, ts, ok, status, latency_ms, error) VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, ms(r.Time), boolInt(r.OK), r.Status, r.LatencyMS, r.Error); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE checks SET state = ?, consecutive_failures = ?, first_failure_at = ?, down_since = ?,
		last_checked = ?, last_status = ?, last_latency_ms = ?, last_error = ? WHERE id = ?`,
		c.State, c.ConsecutiveFailures, ms(c.FirstFailureAt), ms(c.DownSince), ms(r.Time), r.Status, r.LatencyMS, r.Error, c.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// ListCheckResults returns probes in [from, to), oldest first (at most limit).
func (s *Store) ListCheckResults(ctx context.Context, id int64, from, to time.Time, limit int) ([]CheckResult, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.rdb.QueryContext(ctx, `SELECT ts, ok, status, latency_ms, error FROM (
		SELECT ts, ok, status, latency_ms, error FROM check_results WHERE check_id = ? AND ts >= ? AND ts < ? ORDER BY ts DESC LIMIT ?
	) ORDER BY ts`, id, ms(from), ms(to), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []CheckResult{}
	for rows.Next() {
		var r CheckResult
		var ts int64
		var ok int
		if err := rows.Scan(&ts, &ok, &r.Status, &r.LatencyMS, &r.Error); err != nil {
			return nil, err
		}
		r.Time, r.OK = fromMs(ts), ok == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// CheckStats summarizes probes since a time.
type CheckStats struct {
	Probes       int64   `json:"probes"`
	Failures     int64   `json:"failures"`
	UptimePct    float64 `json:"uptime_pct"` // 100 when there are no probes
	AvgLatencyMS float64 `json:"avg_latency_ms"`
}

// CheckStatsSince returns uptime and latency for a check.
func (s *Store) CheckStatsSince(ctx context.Context, id int64, since time.Time) (CheckStats, error) {
	var st CheckStats
	var avg sql.NullFloat64
	err := s.rdb.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(1 - ok), 0), AVG(CASE WHEN ok = 1 THEN latency_ms END)
		FROM check_results WHERE check_id = ? AND ts >= ?`, id, ms(since)).Scan(&st.Probes, &st.Failures, &avg)
	if err != nil {
		return st, err
	}
	st.UptimePct = 100
	if st.Probes > 0 {
		st.UptimePct = 100 * float64(st.Probes-st.Failures) / float64(st.Probes)
	}
	st.AvgLatencyMS = avg.Float64
	return st, nil
}

// CheckLatencySeries buckets successful-probe latency (avg) and failures.
func (s *Store) CheckLatencySeries(ctx context.Context, id int64, from, to time.Time, bucket time.Duration) ([]Bucket, error) {
	b := bucket.Milliseconds()
	if b <= 0 {
		b = 60000
	}
	rows, err := s.rdb.QueryContext(ctx, fmt.Sprintf(`SELECT (ts / %d) * %d AS bk, 'latency_ms', CAST(COALESCE(AVG(CASE WHEN ok = 1 THEN latency_ms END), 0) AS INTEGER) FROM check_results
		WHERE check_id = ? AND ts >= ? AND ts < ? GROUP BY bk
		UNION ALL
		SELECT (ts / %d) * %d AS bk, 'failures', SUM(1 - ok) FROM check_results WHERE check_id = ? AND ts >= ? AND ts < ? GROUP BY bk`, b, b, b, b),
		id, ms(from), ms(to), id, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return fillBuckets(rows, from, to, time.Duration(b)*time.Millisecond)
}
