// Package store persists error groups, samples and service liveness in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver

	"github.com/weeklyvillain/sentinel/internal/event"
)

// Group statuses.
const (
	StatusPending  = "pending"
	StatusOpen     = "open"
	StatusResolved = "resolved"
	StatusIgnored  = "ignored"
)

// KeepLatestSamples is how many recent samples are kept per group, in
// addition to the very first one.
const KeepLatestSamples = 9

// Group is one deduplicated error.
type Group struct {
	ID                int64      `json:"id"`
	Fingerprint       string     `json:"fingerprint"`
	Service           string     `json:"service"`
	Env               string     `json:"env,omitempty"`
	Kind              event.Kind `json:"kind"`
	Title             string     `json:"title"`
	FirstSeen         time.Time  `json:"first_seen"`
	LastSeen          time.Time  `json:"last_seen"`
	Count             int64      `json:"count"`
	CountAtLastReport int64      `json:"count_at_last_report"`
	Status            string     `json:"status"`
	IssueNumber       int        `json:"issue_number,omitempty"`
	IssueURL          string     `json:"issue_url,omitempty"`
	LastReportedAt    time.Time  `json:"last_reported_at,omitzero"`
	LastCheckedAt     time.Time  `json:"last_checked_at,omitzero"`
	IssueCreatedAt    time.Time  `json:"issue_created_at,omitzero"`
	Repo              string     `json:"repo,omitempty"`
	BlockReason       string     `json:"block_reason,omitempty"`
}

// NewSinceReport is the number of events since the last issue/comment.
func (g *Group) NewSinceReport() int64 { return g.Count - g.CountAtLastReport }

// Sample is a stored example event of a group.
type Sample struct {
	ID    int64       `json:"id"`
	Event event.Event `json:"event"`
}

// Service tracks liveness of a telemetry source.
type Service struct {
	Name        string    `json:"name"`
	Env         string    `json:"env,omitempty"`
	LastSeen    time.Time `json:"last_seen"`
	Down        bool      `json:"down"`
	DownSince   time.Time `json:"down_since,omitzero"`
	RecoveredAt time.Time `json:"recovered_at,omitzero"`
	Repo        string    `json:"repo,omitempty"`
	Labels      []string  `json:"labels,omitempty"`
	Managed     bool      `json:"managed"`
	Description string    `json:"description,omitempty"`
	// Issues: file GitHub issues for this service (default on).
	Issues bool `json:"issues"`
	// Watch: flag the service as down when it stops sending telemetry for
	// DOWN_AFTER (opt-in).
	Watch bool `json:"watch"`
}

// Store wraps the SQLite database.
type Store struct {
	db  *sql.DB // single writer connection
	rdb *sql.DB // read pool (same as db for :memory:)
}

const schema = `
CREATE TABLE IF NOT EXISTS groups (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	fingerprint TEXT NOT NULL UNIQUE,
	service TEXT NOT NULL,
	env TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL,
	count INTEGER NOT NULL DEFAULT 0,
	count_at_last_report INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'pending',
	issue_number INTEGER NOT NULL DEFAULT 0,
	issue_url TEXT NOT NULL DEFAULT '',
	last_reported_at INTEGER NOT NULL DEFAULT 0,
	last_checked_at INTEGER NOT NULL DEFAULT 0,
	issue_created_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS groups_status ON groups(status);
CREATE TABLE IF NOT EXISTS samples (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	ts INTEGER NOT NULL,
	data TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS samples_group ON samples(group_id, id);
CREATE TABLE IF NOT EXISTS services (
	name TEXT PRIMARY KEY,
	env TEXT NOT NULL DEFAULT '',
	last_seen INTEGER NOT NULL,
	down INTEGER NOT NULL DEFAULT 0,
	down_since INTEGER NOT NULL DEFAULT 0,
	recovered_at INTEGER NOT NULL DEFAULT 0
);
`

// Open opens (creating if needed) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		dsn = "file::memory:?_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// A single connection serializes writers and keeps :memory: databases shared.
	db.SetMaxOpenConns(1)
	db.SetConnMaxLifetime(0)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	s := &Store{db: db, rdb: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	if path != ":memory:" {
		// WAL lets readers run alongside the single writer; dashboard queries
		// (log search, charts) use this pool so they never block ingest.
		rdb, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)")
		if err != nil {
			db.Close()
			return nil, err
		}
		rdb.SetMaxOpenConns(4)
		s.rdb = rdb
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error {
	if s.rdb != s.db {
		s.rdb.Close()
	}
	return s.db.Close()
}

func ms(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMs(v int64) time.Time {
	if v == 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

// RecordEvent adds e to the group identified by fp, creating it if needed,
// stores it as a sample and returns the updated group.
func (s *Store) RecordEvent(ctx context.Context, fp string, e event.Event) (Group, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Group{}, err
	}
	defer tx.Rollback()

	now := ms(e.Time)
	title := e.Summary()
	if len(title) > 200 {
		title = title[:200]
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO groups (fingerprint, service, env, kind, title, first_seen, last_seen, count, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, 1, 'pending')
		ON CONFLICT(fingerprint) DO UPDATE SET
			count = count + 1,
			last_seen = MAX(last_seen, excluded.last_seen),
			env = CASE WHEN env = '' THEN excluded.env ELSE env END,
			-- a group resolved before any issue existed starts over as pending
			status = CASE WHEN status = 'resolved' AND issue_number = 0 THEN 'pending' ELSE status END`,
		fp, e.Service, e.Environment, string(e.Kind), title, now, now)
	if err != nil {
		return Group{}, fmt.Errorf("upsert group: %w", err)
	}
	g, err := scanGroup(tx.QueryRowContext(ctx, groupCols+` WHERE fingerprint = ?`, fp))
	if err != nil {
		return Group{}, err
	}
	data, err := json.Marshal(e)
	if err != nil {
		return Group{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO samples (group_id, ts, data) VALUES (?, ?, ?)`, g.ID, now, string(data)); err != nil {
		return Group{}, fmt.Errorf("insert sample: %w", err)
	}
	// Keep the first sample plus the latest KeepLatestSamples.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM samples WHERE group_id = ?1
		AND id <> (SELECT MIN(id) FROM samples WHERE group_id = ?1)
		AND id NOT IN (SELECT id FROM samples WHERE group_id = ?1 ORDER BY id DESC LIMIT ?2)`,
		g.ID, KeepLatestSamples); err != nil {
		return Group{}, fmt.Errorf("trim samples: %w", err)
	}
	return g, tx.Commit()
}

const groupCols = `SELECT id, fingerprint, service, env, kind, title, first_seen, last_seen, count,
	count_at_last_report, status, issue_number, issue_url, last_reported_at, last_checked_at, issue_created_at, repo, block_reason FROM groups`

type scanner interface{ Scan(...any) error }

func scanGroup(r scanner) (Group, error) {
	var g Group
	var kind string
	var first, last, rep, chk, created int64
	err := r.Scan(&g.ID, &g.Fingerprint, &g.Service, &g.Env, &kind, &g.Title, &first, &last, &g.Count,
		&g.CountAtLastReport, &g.Status, &g.IssueNumber, &g.IssueURL, &rep, &chk, &created, &g.Repo, &g.BlockReason)
	if err != nil {
		return g, err
	}
	g.Kind = event.Kind(kind)
	g.FirstSeen, g.LastSeen = fromMs(first), fromMs(last)
	g.LastReportedAt, g.LastCheckedAt, g.IssueCreatedAt = fromMs(rep), fromMs(chk), fromMs(created)
	return g, nil
}

// ErrNotFound is returned when a group does not exist.
var ErrNotFound = errors.New("not found")

// GetGroup returns a group by fingerprint.
func (s *Store) GetGroup(ctx context.Context, fp string) (Group, error) {
	g, err := scanGroup(s.db.QueryRowContext(ctx, groupCols+` WHERE fingerprint = ?`, fp))
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	return g, err
}

// ListGroups returns groups, optionally filtered by status, newest activity first.
func (s *Store) ListGroups(ctx context.Context, statuses ...string) ([]Group, error) {
	q := groupCols
	args := []any{}
	if len(statuses) > 0 {
		q += ` WHERE status IN (`
		for i, st := range statuses {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, st)
		}
		q += `)`
	}
	q += ` ORDER BY last_seen DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		g, err := scanGroup(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// Samples returns the stored samples of a group, oldest first.
func (s *Store) Samples(ctx context.Context, groupID int64) ([]Sample, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, data FROM samples WHERE group_id = ? ORDER BY id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Sample{}
	for rows.Next() {
		var sm Sample
		var data string
		if err := rows.Scan(&sm.ID, &data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &sm.Event); err != nil {
			return nil, err
		}
		out = append(out, sm)
	}
	return out, rows.Err()
}

// MarkIssueCreated links a group to a newly created (or adopted) issue.
// reportedCount is the group count the report was based on. created=false
// (an adopted existing issue) does not count toward the hourly cap.
func (s *Store) MarkIssueCreated(ctx context.Context, id int64, repo string, number int, url string, reportedCount int64, now time.Time, created bool) error {
	createdAt := int64(0)
	if created {
		createdAt = ms(now)
	}
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET status = 'open', repo = ?, issue_number = ?, issue_url = ?, block_reason = '',
		count_at_last_report = ?, last_reported_at = ?, last_checked_at = ?, issue_created_at = ? WHERE id = ?`,
		repo, number, url, reportedCount, ms(now), ms(now), createdAt, id)
	return err
}

// MarkReported records that a comment covering events up to reportedCount was posted.
func (s *Store) MarkReported(ctx context.Context, id int64, reportedCount int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET count_at_last_report = ?, last_reported_at = ?, last_checked_at = ? WHERE id = ?`,
		reportedCount, ms(now), ms(now), id)
	return err
}

// MarkChecked records that the issue state was checked on GitHub.
func (s *Store) MarkChecked(ctx context.Context, id int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET last_checked_at = ? WHERE id = ?`, ms(now), id)
	return err
}

// SetStatus changes a group's status. If reportedCount >= 0 it also sets
// count_at_last_report (so events up to it are considered handled).
func (s *Store) SetStatus(ctx context.Context, id int64, status string, reportedCount int64) error {
	if reportedCount >= 0 {
		_, err := s.db.ExecContext(ctx, `UPDATE groups SET status = ?, count_at_last_report = ? WHERE id = ?`, status, reportedCount, id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET status = ? WHERE id = ?`, status, id)
	return err
}

// IssuesCreatedSince counts groups whose issue was created at or after t.
func (s *Store) IssuesCreatedSince(ctx context.Context, t time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM groups WHERE issue_created_at >= ?`, ms(t)).Scan(&n)
	return n, err
}

// TouchService records that service sent telemetry at now. If the service was
// marked down it is flagged as recovered (handled later by the reporter).
func (s *Store) TouchService(ctx context.Context, name, env string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO services (name, env, last_seen) VALUES (?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			last_seen = MAX(last_seen, excluded.last_seen),
			env = CASE WHEN excluded.env <> '' THEN excluded.env ELSE env END,
			recovered_at = CASE WHEN down = 1 AND recovered_at = 0 THEN excluded.last_seen ELSE recovered_at END,
			down = 0`,
		name, env, ms(now))
	return err
}

// ListServices returns all known services.
func (s *Store) ListServices(ctx context.Context) ([]Service, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, env, last_seen, down, down_since, recovered_at, repo, labels, managed, description, issues, watch FROM services ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Service{}
	for rows.Next() {
		var sv Service
		var last, since, rec int64
		var down, managed, issues, watch int
		var labels string
		if err := rows.Scan(&sv.Name, &sv.Env, &last, &down, &since, &rec, &sv.Repo, &labels, &managed, &sv.Description, &issues, &watch); err != nil {
			return nil, err
		}
		sv.Issues, sv.Watch = issues == 1, watch == 1
		sv.Labels = splitLabels(labels)
		sv.Managed = managed == 1
		sv.LastSeen, sv.DownSince, sv.RecoveredAt = fromMs(last), fromMs(since), fromMs(rec)
		sv.Down = down == 1
		out = append(out, sv)
	}
	return out, rows.Err()
}

// MarkServiceDown flags a service as down since the given time. It only
// succeeds (returns true) if the service is watched, not already down and has
// not been seen after since.
func (s *Store) MarkServiceDown(ctx context.Context, name string, since time.Time) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE services SET down = 1, down_since = ?, recovered_at = 0
		WHERE name = ? AND watch = 1 AND down = 0 AND last_seen > 0 AND last_seen <= ?`, ms(since), name, ms(since))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ClearRecovery resets the recovery marker after the reporter handled it.
func (s *Store) ClearRecovery(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE services SET recovered_at = 0, down_since = 0 WHERE name = ? AND down = 0`, name)
	return err
}

// GetGroupByID returns a group by id.
func (s *Store) GetGroupByID(ctx context.Context, id int64) (Group, error) {
	g, err := scanGroup(s.rdb.QueryRowContext(ctx, groupCols+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return g, ErrNotFound
	}
	return g, err
}

// SetBlockReason records why a pending group cannot be filed (shown in the UI).
func (s *Store) SetBlockReason(ctx context.Context, id int64, reason string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE groups SET block_reason = ? WHERE id = ? AND block_reason <> ?`, reason, id, reason)
	return err
}

// GetService returns one service.
func (s *Store) GetService(ctx context.Context, name string) (Service, error) {
	all, err := s.ListServices(ctx)
	if err != nil {
		return Service{}, err
	}
	for _, sv := range all {
		if sv.Name == name {
			return sv, nil
		}
	}
	return Service{}, ErrNotFound
}

// ServiceConfig is the user-editable part of a service.
type ServiceConfig struct {
	Name        string   `json:"name"`
	Repo        string   `json:"repo"`
	Labels      []string `json:"labels"`
	Description string   `json:"description"`
}

// UpsertServiceConfig adds (or edits) a service from the dashboard. A service
// added before it ever sent telemetry has last_seen = 0 and is never flagged down.
func (s *Store) UpsertServiceConfig(ctx context.Context, c ServiceConfig) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO services (name, last_seen, repo, labels, managed, description) VALUES (?, 0, ?, ?, 1, ?)
		ON CONFLICT(name) DO UPDATE SET repo = excluded.repo, labels = excluded.labels, managed = 1, description = excluded.description`,
		c.Name, c.Repo, joinLabels(c.Labels), c.Description)
	return err
}

// ServiceFlags are the per-service switches; nil leaves a flag unchanged.
type ServiceFlags struct {
	Issues *bool `json:"issues"`
	Watch  *bool `json:"watch"`
}

// SetServiceFlags turns issue creation and down detection on or off. A
// service that is no longer watched also stops being down: nobody asked to
// know, so there is nothing to recover from.
func (s *Store) SetServiceFlags(ctx context.Context, name string, f ServiceFlags) error {
	b := func(p *bool, col string) string {
		if p == nil {
			return col
		}
		if *p {
			return "1"
		}
		return "0"
	}
	res, err := s.db.ExecContext(ctx, `UPDATE services SET issues = `+b(f.Issues, "issues")+`, watch = `+b(f.Watch, "watch")+` WHERE name = ?`, name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return s.ClearUnwatchedDown(ctx)
}

// ClearUnwatchedDown resets the down state of services that are not watched
// (for example ones flagged before watching became opt-in).
func (s *Store) ClearUnwatchedDown(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE services SET down = 0, down_since = 0, recovered_at = 0 WHERE watch = 0 AND (down = 1 OR recovered_at > 0)`)
	return err
}

// DeleteService forgets a service. If it sends telemetry again it reappears as discovered.
func (s *Store) DeleteService(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM services WHERE name = ?`, name)
	return err
}

func splitLabels(s string) []string {
	out := []string{}
	for _, l := range strings.Split(s, ",") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func joinLabels(ls []string) string {
	out := []string{}
	for _, l := range ls {
		if l = strings.TrimSpace(l); l != "" && !strings.Contains(l, ",") {
			out = append(out, l)
		}
	}
	return strings.Join(out, ",")
}
