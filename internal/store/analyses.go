package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// The analysis queue: before a group becomes an issue, the AI provider writes
// its title and analysis. That can take minutes with a local model, so it runs
// in the background, one job at a time, and the reporter files the issue once
// the job is done (or failed, in which case it uses the template).
const schemaAnalyses = `
CREATE TABLE IF NOT EXISTS analyses (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	group_id INTEGER NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	status TEXT NOT NULL DEFAULT 'queued',
	provider TEXT NOT NULL DEFAULT '',
	queued_at INTEGER NOT NULL,
	started_at INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	error TEXT NOT NULL DEFAULT '',
	draft TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS analyses_group ON analyses(group_id, id);
CREATE INDEX IF NOT EXISTS analyses_status ON analyses(status, id);
`

// Analysis statuses.
const (
	AnalysisQueued  = "queued"
	AnalysisRunning = "running"
	AnalysisDone    = "done"
	AnalysisFailed  = "failed"
)

// Analysis is one AI drafting job for a group.
type Analysis struct {
	ID         int64     `json:"id"`
	GroupID    int64     `json:"group_id"`
	Status     string    `json:"status"`
	Provider   string    `json:"provider,omitempty"`
	QueuedAt   time.Time `json:"queued_at"`
	StartedAt  time.Time `json:"started_at,omitzero"`
	FinishedAt time.Time `json:"finished_at,omitzero"`
	Error      string    `json:"error,omitempty"`
	// Draft is the provider's JSON output (title, summary, ...), set when done.
	Draft string `json:"draft,omitempty"`

	// Joined from the group, for the queue view.
	Service     string `json:"service"`
	GroupTitle  string `json:"group_title"`
	GroupStatus string `json:"group_status"`
	IssueURL    string `json:"issue_url,omitempty"`
}

const analysisCols = `a.id, a.group_id, a.status, a.provider, a.queued_at, a.started_at, a.finished_at, a.error, a.draft,
	g.service, g.title, g.status, g.issue_url`

func scanAnalysis(sc interface{ Scan(...any) error }) (Analysis, error) {
	var a Analysis
	var q, s, f int64
	err := sc.Scan(&a.ID, &a.GroupID, &a.Status, &a.Provider, &q, &s, &f, &a.Error, &a.Draft,
		&a.Service, &a.GroupTitle, &a.GroupStatus, &a.IssueURL)
	a.QueuedAt, a.StartedAt, a.FinishedAt = fromMs(q), fromMs(s), fromMs(f)
	return a, err
}

// LatestAnalysis returns the newest analysis for a group, or ErrNotFound.
func (s *Store) LatestAnalysis(ctx context.Context, groupID int64) (Analysis, error) {
	a, err := scanAnalysis(s.db.QueryRowContext(ctx, `SELECT `+analysisCols+`
		FROM analyses a JOIN groups g ON g.id = a.group_id WHERE a.group_id = ? ORDER BY a.id DESC LIMIT 1`, groupID))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// GetAnalysis returns one analysis.
func (s *Store) GetAnalysis(ctx context.Context, id int64) (Analysis, error) {
	a, err := scanAnalysis(s.db.QueryRowContext(ctx, `SELECT `+analysisCols+`
		FROM analyses a JOIN groups g ON g.id = a.group_id WHERE a.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// EnqueueAnalysis queues a job for a group unless one is already queued or running.
func (s *Store) EnqueueAnalysis(ctx context.Context, groupID int64, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO analyses (group_id, status, queued_at)
		SELECT ?, 'queued', ? WHERE NOT EXISTS (
			SELECT 1 FROM analyses WHERE group_id = ? AND status IN ('queued', 'running'))`, groupID, ms(now), groupID)
	return err
}

// ClaimAnalysis marks the oldest queued job running and returns it. ok is
// false when the queue is empty.
func (s *Store) ClaimAnalysis(ctx context.Context, provider string, now time.Time) (Analysis, bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `UPDATE analyses SET status = 'running', started_at = ?, provider = ?
		WHERE id = (SELECT id FROM analyses WHERE status = 'queued' ORDER BY id LIMIT 1) RETURNING id`, ms(now), provider).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return Analysis{}, false, nil
	}
	if err != nil {
		return Analysis{}, false, err
	}
	a, err := s.GetAnalysis(ctx, id)
	return a, err == nil, err
}

// FinishAnalysis stores the result of a running job: the draft JSON on
// success, or the error.
func (s *Store) FinishAnalysis(ctx context.Context, id int64, draft, errMsg string, now time.Time) error {
	status := AnalysisDone
	if errMsg != "" {
		status = AnalysisFailed
	}
	_, err := s.db.ExecContext(ctx, `UPDATE analyses SET status = ?, draft = ?, error = ?, finished_at = ? WHERE id = ?`,
		status, draft, errMsg, ms(now), id)
	return err
}

// RetryAnalysis queues a failed job again.
func (s *Store) RetryAnalysis(ctx context.Context, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE analyses SET status = 'queued', queued_at = ?, started_at = 0, finished_at = 0, error = '', draft = '', provider = ''
		WHERE id = ? AND status = 'failed'`, ms(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RequeueRunning puts jobs that were running when Sentinel stopped back in
// the queue.
func (s *Store) RequeueRunning(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE analyses SET status = 'queued', started_at = 0, provider = '' WHERE status = 'running'`)
	return err
}

// ListAnalyses returns all queued and running jobs plus the most recent
// finished ones (newest first).
func (s *Store) ListAnalyses(ctx context.Context, recent int) ([]Analysis, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT `+analysisCols+` FROM analyses a JOIN groups g ON g.id = a.group_id
		WHERE a.status IN ('queued', 'running')
		   OR a.id IN (SELECT id FROM analyses WHERE status IN ('done', 'failed') ORDER BY id DESC LIMIT ?)
		ORDER BY CASE a.status WHEN 'running' THEN 0 WHEN 'queued' THEN 1 ELSE 2 END,
		         CASE WHEN a.status = 'queued' THEN a.id ELSE -a.id END`, recent)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Analysis{}
	for rows.Next() {
		a, err := scanAnalysis(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PruneAnalyses keeps the newest n finished jobs.
func (s *Store) PruneAnalyses(ctx context.Context, keep int) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM analyses WHERE status IN ('done', 'failed') AND id NOT IN (
		SELECT id FROM analyses WHERE status IN ('done', 'failed') ORDER BY id DESC LIMIT ?)
		AND id NOT IN (SELECT MAX(id) FROM analyses GROUP BY group_id)`, keep)
	return err
}
