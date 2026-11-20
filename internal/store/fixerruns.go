package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Fixer runs: every job the fixer container claims (a fix or an
// investigation), with what Claude Code did (the log) and what it cost
// (tokens, turns, time). The dashboard lists them under Issues → Fixer.
const schemaFixerRuns = `
CREATE TABLE IF NOT EXISTS fixer_runs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	kind TEXT NOT NULL,
	group_id INTEGER NOT NULL DEFAULT 0,
	analysis_id INTEGER NOT NULL DEFAULT 0,
	repo TEXT NOT NULL DEFAULT '',
	issue_number INTEGER NOT NULL DEFAULT 0,
	attempt INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL DEFAULT 'running',
	started_at INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	auth TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	turns INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cache_read_tokens INTEGER NOT NULL DEFAULT 0,
	cache_write_tokens INTEGER NOT NULL DEFAULT 0,
	cost_usd REAL NOT NULL DEFAULT 0,
	pr_url TEXT NOT NULL DEFAULT '',
	summary TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	log_lines INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS fixer_runs_started ON fixer_runs(started_at);
CREATE INDEX IF NOT EXISTS fixer_runs_group ON fixer_runs(group_id, id);
CREATE TABLE IF NOT EXISTS fixer_run_logs (
	run_id INTEGER NOT NULL REFERENCES fixer_runs(id) ON DELETE CASCADE,
	seq INTEGER NOT NULL,
	at INTEGER NOT NULL,
	kind TEXT NOT NULL,
	text TEXT NOT NULL,
	PRIMARY KEY (run_id, seq)
) WITHOUT ROWID;
`

// Fixer run statuses: running, then the job's result status (pr, no_fix,
// failed, dry_run, done), or lost when the fixer stopped without one.
const (
	FixerRunRunning = "running"
	FixerRunLost    = "lost"
)

// Log limits per run: a runaway agent must not fill the database.
const (
	MaxFixerLogLines = 3000
	MaxFixerLogText  = 8000
)

// FixerUsage is what Claude Code reported for a run.
type FixerUsage struct {
	Model            string  `json:"model,omitempty"`
	Turns            int     `json:"turns"`
	DurationMS       int64   `json:"duration_ms"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	CostUSD          float64 `json:"cost_usd"`
}

// FixerRun is one job the fixer worked on.
type FixerRun struct {
	ID          int64     `json:"id"`
	Kind        string    `json:"kind"` // fix | analyze
	GroupID     int64     `json:"group_id"`
	AnalysisID  int64     `json:"analysis_id,omitempty"`
	Repo        string    `json:"repo"`
	IssueNumber int       `json:"issue_number,omitempty"`
	Attempt     int       `json:"attempt"`
	Status      string    `json:"status"`
	StartedAt   time.Time `json:"started_at"`
	FinishedAt  time.Time `json:"finished_at,omitzero"`
	Auth        string    `json:"auth,omitempty"` // subscription | api_key
	FixerUsage
	PRURL    string `json:"pr_url,omitempty"`
	Summary  string `json:"summary,omitempty"`
	Error    string `json:"error,omitempty"`
	LogLines int    `json:"log_lines"`

	// Joined from the group.
	Service    string `json:"service,omitempty"`
	GroupTitle string `json:"group_title,omitempty"`
	IssueURL   string `json:"issue_url,omitempty"`
}

// FixerLogLine is one step of a run: the fixer's own (step, error) or
// Claude Code's (text, tool, tool_result, tool_error).
type FixerLogLine struct {
	Seq  int       `json:"seq"`
	At   time.Time `json:"at"`
	Kind string    `json:"kind"`
	Text string    `json:"text"`
}

// StartFixerRun records a job the fixer just claimed. The fixer works on one
// job at a time, so runs it left running are lost.
func (s *Store) StartFixerRun(ctx context.Context, r FixerRun, now time.Time) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `UPDATE fixer_runs SET status = ?, finished_at = ?,
		error = CASE WHEN error = '' THEN 'the fixer stopped before reporting a result' ELSE error END
		WHERE status = ?`, FixerRunLost, ms(now), FixerRunRunning); err != nil {
		return 0, err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO fixer_runs (kind, group_id, analysis_id, repo, issue_number, attempt, status, started_at, auth, model)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Kind, r.GroupID, r.AnalysisID, r.Repo, r.IssueNumber, r.Attempt, FixerRunRunning, ms(now), r.Auth, r.Model)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AppendFixerLog adds lines to a running run's log, up to MaxFixerLogLines.
func (s *Store) AppendFixerLog(ctx context.Context, runID int64, lines []FixerLogLine) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var n int
	var status string
	err = tx.QueryRowContext(ctx, `SELECT log_lines, status FROM fixer_runs WHERE id = ?`, runID).Scan(&n, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status != FixerRunRunning {
		return ErrNotFound
	}
	added := 0
	for _, l := range lines {
		if n+added >= MaxFixerLogLines {
			break
		}
		text := l.Text
		if len(text) > MaxFixerLogText {
			text = strings.ToValidUTF8(text[:MaxFixerLogText], "") + "…"
		}
		if n+added == MaxFixerLogLines-1 {
			l.Kind, text = "step", "(log truncated: the rest of this run is not stored)"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO fixer_run_logs (run_id, seq, at, kind, text) VALUES (?, ?, ?, ?, ?)`,
			runID, n+added+1, ms(l.At), l.Kind, text); err != nil {
			return err
		}
		added++
	}
	if added > 0 {
		if _, err := tx.ExecContext(ctx, `UPDATE fixer_runs SET log_lines = log_lines + ? WHERE id = ?`, added, runID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// FinishFixerRun records how a run ended. Only a running run is updated.
func (s *Store) FinishFixerRun(ctx context.Context, runID int64, status string, u FixerUsage, prURL, summary, errText string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE fixer_runs SET status = ?, finished_at = ?,
		model = CASE WHEN ? != '' THEN ? ELSE model END, turns = ?, duration_ms = ?,
		input_tokens = ?, output_tokens = ?, cache_read_tokens = ?, cache_write_tokens = ?, cost_usd = ?,
		pr_url = ?, summary = ?, error = ?
		WHERE id = ? AND status = ?`,
		status, ms(now), u.Model, u.Model, u.Turns, u.DurationMS,
		u.InputTokens, u.OutputTokens, u.CacheReadTokens, u.CacheWriteTokens, u.CostUSD,
		prURL, summary, errText, runID, FixerRunRunning)
	return err
}

const fixerRunCols = `r.id, r.kind, r.group_id, r.analysis_id, r.repo, r.issue_number, r.attempt, r.status, r.started_at, r.finished_at,
	r.auth, r.model, r.turns, r.duration_ms, r.input_tokens, r.output_tokens, r.cache_read_tokens, r.cache_write_tokens, r.cost_usd,
	r.pr_url, r.summary, r.error, r.log_lines, COALESCE(g.service, ''), COALESCE(g.title, ''), COALESCE(g.issue_url, '')`

func scanFixerRun(sc interface{ Scan(...any) error }) (FixerRun, error) {
	var r FixerRun
	var st, fin int64
	err := sc.Scan(&r.ID, &r.Kind, &r.GroupID, &r.AnalysisID, &r.Repo, &r.IssueNumber, &r.Attempt, &r.Status, &st, &fin,
		&r.Auth, &r.Model, &r.Turns, &r.DurationMS, &r.InputTokens, &r.OutputTokens, &r.CacheReadTokens, &r.CacheWriteTokens, &r.CostUSD,
		&r.PRURL, &r.Summary, &r.Error, &r.LogLines, &r.Service, &r.GroupTitle, &r.IssueURL)
	r.StartedAt, r.FinishedAt = fromMs(st), fromMs(fin)
	return r, err
}

// FixerRunFilter narrows ListFixerRuns.
type FixerRunFilter struct {
	Kind    string // fix | analyze | "" (both)
	Status  string
	GroupID int64
	Limit   int
}

// ListFixerRuns returns runs, newest first.
func (s *Store) ListFixerRuns(ctx context.Context, f FixerRunFilter) ([]FixerRun, error) {
	q := `SELECT ` + fixerRunCols + ` FROM fixer_runs r LEFT JOIN groups g ON g.id = r.group_id WHERE 1=1`
	var args []any
	if f.Kind != "" {
		q += ` AND r.kind = ?`
		args = append(args, f.Kind)
	}
	if f.Status != "" {
		q += ` AND r.status = ?`
		args = append(args, f.Status)
	}
	if f.GroupID != 0 {
		q += ` AND r.group_id = ?`
		args = append(args, f.GroupID)
	}
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 100
	}
	q += ` ORDER BY r.id DESC LIMIT ?`
	args = append(args, f.Limit)
	rows, err := s.rdb.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FixerRun{}
	for rows.Next() {
		r, err := scanFixerRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetFixerRun returns one run, or ErrNotFound.
func (s *Store) GetFixerRun(ctx context.Context, id int64) (FixerRun, error) {
	r, err := scanFixerRun(s.rdb.QueryRowContext(ctx, `SELECT `+fixerRunCols+` FROM fixer_runs r LEFT JOIN groups g ON g.id = r.group_id WHERE r.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

// FixerLog returns a run's log lines after seq (0 = from the start).
func (s *Store) FixerLog(ctx context.Context, runID int64, after int) ([]FixerLogLine, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT seq, at, kind, text FROM fixer_run_logs WHERE run_id = ? AND seq > ? ORDER BY seq`, runID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FixerLogLine{}
	for rows.Next() {
		var l FixerLogLine
		var at int64
		if err := rows.Scan(&l.Seq, &at, &l.Kind, &l.Text); err != nil {
			return nil, err
		}
		l.At = fromMs(at)
		out = append(out, l)
	}
	return out, rows.Err()
}

// FixerStats sums the runs started since a time.
type FixerStats struct {
	Runs             int            `json:"runs"`
	ByStatus         map[string]int `json:"by_status"`
	Fixes            int            `json:"fixes"`
	Investigations   int            `json:"investigations"`
	Turns            int64          `json:"turns"`
	DurationMS       int64          `json:"duration_ms"`
	InputTokens      int64          `json:"input_tokens"`
	OutputTokens     int64          `json:"output_tokens"`
	CacheReadTokens  int64          `json:"cache_read_tokens"`
	CacheWriteTokens int64          `json:"cache_write_tokens"`
	CostUSD          float64        `json:"cost_usd"`
}

// FixerRunStats sums the runs started at or after since.
func (s *Store) FixerRunStats(ctx context.Context, since time.Time) (FixerStats, error) {
	st := FixerStats{ByStatus: map[string]int{}}
	rows, err := s.rdb.QueryContext(ctx, `SELECT kind, status, COUNT(*), SUM(turns), SUM(duration_ms), SUM(input_tokens), SUM(output_tokens),
		SUM(cache_read_tokens), SUM(cache_write_tokens), SUM(cost_usd)
		FROM fixer_runs WHERE started_at >= ? GROUP BY kind, status`, ms(since))
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var kind, status string
		var n int
		var turns, dur, in, out, cr, cw int64
		var cost float64
		if err := rows.Scan(&kind, &status, &n, &turns, &dur, &in, &out, &cr, &cw, &cost); err != nil {
			return st, err
		}
		st.Runs += n
		st.ByStatus[status] += n
		if kind == "analyze" {
			st.Investigations += n
		} else {
			st.Fixes += n
		}
		st.Turns += turns
		st.DurationMS += dur
		st.InputTokens += in
		st.OutputTokens += out
		st.CacheReadTokens += cr
		st.CacheWriteTokens += cw
		st.CostUSD += cost
	}
	return st, rows.Err()
}

// PruneFixerRuns drops logs of runs started before logsBefore, and runs
// started before runsBefore.
func (s *Store) PruneFixerRuns(ctx context.Context, logsBefore, runsBefore time.Time) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM fixer_run_logs WHERE run_id IN (SELECT id FROM fixer_runs WHERE started_at < ?)`, ms(logsBefore)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM fixer_runs WHERE started_at < ?`, ms(runsBefore))
	return err
}
