package store

import "fmt"

// schemaV2 adds the dashboard tables: searchable logs, metrics, GitHub App
// state, sessions, alarms and webhooks.
const schemaV2 = `
CREATE TABLE IF NOT EXISTS logs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts INTEGER NOT NULL,
	service TEXT NOT NULL,
	env TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT 'log',
	severity INTEGER NOT NULL DEFAULT 0,
	severity_text TEXT NOT NULL DEFAULT '',
	body TEXT NOT NULL DEFAULT '',
	trace_id TEXT NOT NULL DEFAULT '',
	span_id TEXT NOT NULL DEFAULT '',
	attrs TEXT NOT NULL DEFAULT '{}',
	fingerprint TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS logs_ts ON logs(ts);
CREATE INDEX IF NOT EXISTS logs_service_ts ON logs(service, ts);
CREATE INDEX IF NOT EXISTS logs_trace ON logs(trace_id) WHERE trace_id <> '';
CREATE INDEX IF NOT EXISTS logs_fp ON logs(fingerprint) WHERE fingerprint <> '';
CREATE VIRTUAL TABLE IF NOT EXISTS logs_fts USING fts5(body, attrs, content='logs', content_rowid='id');
CREATE TRIGGER IF NOT EXISTS logs_ai AFTER INSERT ON logs BEGIN
	INSERT INTO logs_fts(rowid, body, attrs) VALUES (new.id, new.body, new.attrs);
END;
CREATE TRIGGER IF NOT EXISTS logs_ad AFTER DELETE ON logs BEGIN
	INSERT INTO logs_fts(logs_fts, rowid, body, attrs) VALUES ('delete', old.id, old.body, old.attrs);
END;

CREATE TABLE IF NOT EXISTS metric_points (
	ts INTEGER NOT NULL,
	service TEXT NOT NULL,
	name TEXT NOT NULL,
	attrs TEXT NOT NULL DEFAULT '',
	value REAL NOT NULL
);
CREATE INDEX IF NOT EXISTS metric_points_series ON metric_points(service, name, ts);
CREATE INDEX IF NOT EXISTS metric_points_ts ON metric_points(ts);
CREATE TABLE IF NOT EXISTS metric_series (
	service TEXT NOT NULL,
	name TEXT NOT NULL,
	unit TEXT NOT NULL DEFAULT '',
	kind TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	last_seen INTEGER NOT NULL,
	PRIMARY KEY (service, name)
);

CREATE TABLE IF NOT EXISTS gh_installations (
	id INTEGER PRIMARY KEY,
	account TEXT NOT NULL,
	account_type TEXT NOT NULL DEFAULT '',
	repo_selection TEXT NOT NULL DEFAULT '',
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS gh_repos (
	full_name TEXT PRIMARY KEY,
	installation_id INTEGER NOT NULL,
	private INTEGER NOT NULL DEFAULT 0,
	allowed INTEGER NOT NULL DEFAULT 0,
	updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	login TEXT NOT NULL,
	name TEXT NOT NULL DEFAULT '',
	avatar_url TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS webhooks (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	url TEXT NOT NULL,
	secret TEXT NOT NULL DEFAULT '',
	format TEXT NOT NULL DEFAULT 'json',
	events TEXT NOT NULL DEFAULT '[]',
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS webhook_deliveries (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	webhook_id INTEGER NOT NULL REFERENCES webhooks(id) ON DELETE CASCADE,
	event TEXT NOT NULL,
	ts INTEGER NOT NULL,
	status INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL DEFAULT 0,
	attempt INTEGER NOT NULL DEFAULT 1,
	payload TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS webhook_deliveries_hook ON webhook_deliveries(webhook_id, id);

CREATE TABLE IF NOT EXISTS alarms (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	kind TEXT NOT NULL,
	service TEXT NOT NULL DEFAULT '',
	query TEXT NOT NULL DEFAULT '',
	min_severity INTEGER NOT NULL DEFAULT 0,
	metric TEXT NOT NULL DEFAULT '',
	agg TEXT NOT NULL DEFAULT 'avg',
	op TEXT NOT NULL DEFAULT '>',
	threshold REAL NOT NULL DEFAULT 0,
	window_s INTEGER NOT NULL DEFAULT 300,
	webhook_ids TEXT NOT NULL DEFAULT '[]',
	state TEXT NOT NULL DEFAULT 'ok',
	last_value REAL NOT NULL DEFAULT 0,
	last_eval INTEGER NOT NULL DEFAULT 0,
	last_changed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS alarm_events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	alarm_id INTEGER NOT NULL REFERENCES alarms(id) ON DELETE CASCADE,
	ts INTEGER NOT NULL,
	state TEXT NOT NULL,
	value REAL NOT NULL,
	message TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS alarm_events_alarm ON alarm_events(alarm_id, id);
`

func (s *Store) migrate() error {
	if _, err := s.db.Exec(schemaV2); err != nil {
		return err
	}
	if _, err := s.db.Exec(schemaChecks); err != nil {
		return err
	}
	if _, err := s.db.Exec(schemaAnalyses); err != nil {
		return err
	}
	if _, err := s.db.Exec(schemaDashboards); err != nil {
		return err
	}
	for _, sch := range []string{schemaTraces, schemaGroupsExt, schemaAlerting, schemaMetricHist, schemaFixerRuns} {
		if _, err := s.db.Exec(sch); err != nil {
			return err
		}
	}
	cols := []struct{ table, col, def string }{
		{"groups", "repo", "TEXT NOT NULL DEFAULT ''"},
		{"groups", "block_reason", "TEXT NOT NULL DEFAULT ''"},
		{"services", "repo", "TEXT NOT NULL DEFAULT ''"},
		{"services", "labels", "TEXT NOT NULL DEFAULT ''"},
		{"services", "managed", "INTEGER NOT NULL DEFAULT 0"},
		{"services", "description", "TEXT NOT NULL DEFAULT ''"},
		// issues: file GitHub issues for this service. watch: flag the service
		// as down when it goes quiet (opt-in, from the Uptime page).
		{"services", "issues", "INTEGER NOT NULL DEFAULT 1"},
		{"services", "watch", "INTEGER NOT NULL DEFAULT 0"},
		{"groups", "first_release", "TEXT NOT NULL DEFAULT ''"},
		{"groups", "last_release", "TEXT NOT NULL DEFAULT ''"},
		{"groups", "regressed_release", "TEXT NOT NULL DEFAULT ''"},
		{"groups", "snooze_until", "INTEGER NOT NULL DEFAULT 0"},
		{"groups", "snooze_count", "INTEGER NOT NULL DEFAULT 0"},
		{"groups", "snooze_release", "TEXT NOT NULL DEFAULT ''"},
		// The fingerprint the event computed to, so merged groups can be split again.
		{"samples", "fingerprint", "TEXT NOT NULL DEFAULT ''"},
		// Alarms: how long the condition must hold before firing.
		{"alarms", "for_s", "INTEGER NOT NULL DEFAULT 0"},
		{"alarms", "pending_since", "INTEGER NOT NULL DEFAULT 0"},
		// Automated fixes.
		{"services", "autofix", "INTEGER NOT NULL DEFAULT 0"},
		{"groups", "fix_state", "TEXT NOT NULL DEFAULT ''"},
		{"groups", "fix_requested_at", "INTEGER NOT NULL DEFAULT 0"},
		{"groups", "fix_attempts", "INTEGER NOT NULL DEFAULT 0"},
		{"groups", "fix_pr_number", "INTEGER NOT NULL DEFAULT 0"},
		{"groups", "fix_pr_url", "TEXT NOT NULL DEFAULT ''"},
		{"groups", "fix_checked_at", "INTEGER NOT NULL DEFAULT 0"},
		{"groups", "fix_started_at", "INTEGER NOT NULL DEFAULT 0"},
		{"groups", "fix_summary", "TEXT NOT NULL DEFAULT ''"},
		// Every pull request of a fix across repositories (JSON).
		{"groups", "fix_prs", "TEXT NOT NULL DEFAULT '[]'"},
		{"groups", "triage_verdict", "TEXT NOT NULL DEFAULT ''"},
		{"groups", "triage_severity", "TEXT NOT NULL DEFAULT ''"},
		{"groups", "triage_reason", "TEXT NOT NULL DEFAULT ''"},
	}
	for _, c := range cols {
		if err := s.addColumn(c.table, c.col, c.def); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addColumn(table, col, def string) error {
	rows, err := s.db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == col {
			return nil
		}
	}
	rows.Close()
	_, err = s.db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, col, def))
	return err
}
