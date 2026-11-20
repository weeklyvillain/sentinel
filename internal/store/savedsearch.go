package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

const schemaSavedSearches = `
CREATE TABLE IF NOT EXISTS saved_searches (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	params TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	uses INTEGER NOT NULL DEFAULT 0,
	last_used INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
`

// SavedSearch is a named Logs page search, shared by everyone. Params is the
// page's URL query (search, levels, services, chart), owned by the UI.
type SavedSearch struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	Params    string     `json:"params"`
	CreatedBy string     `json:"created_by,omitempty"`
	Uses      int64      `json:"uses"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

const savedSearchCols = `SELECT id, name, params, created_by, uses, last_used, created_at FROM saved_searches`

func scanSavedSearch(sc interface{ Scan(...any) error }) (SavedSearch, error) {
	var x SavedSearch
	var last, created int64
	err := sc.Scan(&x.ID, &x.Name, &x.Params, &x.CreatedBy, &x.Uses, &last, &created)
	if last > 0 {
		t := fromMs(last)
		x.LastUsed = &t
	}
	x.CreatedAt = fromMs(created)
	return x, err
}

// ListSavedSearches returns saved searches, most used first.
func (s *Store) ListSavedSearches(ctx context.Context) ([]SavedSearch, error) {
	rows, err := s.rdb.QueryContext(ctx, savedSearchCols+` ORDER BY uses DESC, last_used DESC, name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SavedSearch{}
	for rows.Next() {
		x, err := scanSavedSearch(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// GetSavedSearch returns one saved search.
func (s *Store) GetSavedSearch(ctx context.Context, id int64) (SavedSearch, error) {
	x, err := scanSavedSearch(s.db.QueryRowContext(ctx, savedSearchCols+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return x, ErrNotFound
	}
	return x, err
}

// SaveSavedSearch creates (ID 0) or updates (name and params) a saved search.
func (s *Store) SaveSavedSearch(ctx context.Context, x SavedSearch, now time.Time) (SavedSearch, error) {
	if x.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO saved_searches (name, params, created_by, created_at) VALUES (?, ?, ?, ?)`,
			x.Name, x.Params, x.CreatedBy, ms(now))
		if err != nil {
			return x, err
		}
		x.ID, _ = res.LastInsertId()
	} else {
		res, err := s.db.ExecContext(ctx, `UPDATE saved_searches SET name = ?, params = ? WHERE id = ?`, x.Name, x.Params, x.ID)
		if err != nil {
			return x, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return x, ErrNotFound
		}
	}
	return s.GetSavedSearch(ctx, x.ID)
}

// UseSavedSearch counts a use, so the ones run often come first.
func (s *Store) UseSavedSearch(ctx context.Context, id int64, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE saved_searches SET uses = uses + 1, last_used = ? WHERE id = ?`, ms(now), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteSavedSearch removes a saved search.
func (s *Store) DeleteSavedSearch(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM saved_searches WHERE id = ?`, id)
	return err
}
