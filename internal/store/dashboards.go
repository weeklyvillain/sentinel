package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const schemaDashboards = `
CREATE TABLE IF NOT EXISTS dashboards (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	spec TEXT NOT NULL DEFAULT '{}',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
`

// Dashboard is a saved set of panels. Spec (panels, layout, default range)
// is owned by the UI and stored as JSON.
type Dashboard struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Spec        json.RawMessage `json:"spec"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

func scanDashboard(sc interface{ Scan(...any) error }) (Dashboard, error) {
	var d Dashboard
	var spec string
	var c, u int64
	err := sc.Scan(&d.ID, &d.Name, &d.Description, &spec, &c, &u)
	d.Spec = json.RawMessage(spec)
	d.CreatedAt, d.UpdatedAt = fromMs(c), fromMs(u)
	return d, err
}

// ListDashboards returns all dashboards by name.
func (s *Store) ListDashboards(ctx context.Context) ([]Dashboard, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, name, description, spec, created_at, updated_at FROM dashboards ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Dashboard{}
	for rows.Next() {
		d, err := scanDashboard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDashboard returns one dashboard.
func (s *Store) GetDashboard(ctx context.Context, id int64) (Dashboard, error) {
	d, err := scanDashboard(s.db.QueryRowContext(ctx, `SELECT id, name, description, spec, created_at, updated_at FROM dashboards WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return d, ErrNotFound
	}
	return d, err
}

// SaveDashboard creates (ID 0) or updates a dashboard.
func (s *Store) SaveDashboard(ctx context.Context, d Dashboard, now time.Time) (Dashboard, error) {
	if len(d.Spec) == 0 {
		d.Spec = json.RawMessage(`{}`)
	}
	if d.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO dashboards (name, description, spec, created_at, updated_at) VALUES (?, ?, ?, ?, ?)`,
			d.Name, d.Description, string(d.Spec), ms(now), ms(now))
		if err != nil {
			return d, err
		}
		d.ID, _ = res.LastInsertId()
	} else {
		res, err := s.db.ExecContext(ctx, `UPDATE dashboards SET name = ?, description = ?, spec = ?, updated_at = ? WHERE id = ?`,
			d.Name, d.Description, string(d.Spec), ms(now), d.ID)
		if err != nil {
			return d, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return d, ErrNotFound
		}
	}
	return s.GetDashboard(ctx, d.ID)
}

// DeleteDashboard removes a dashboard.
func (s *Store) DeleteDashboard(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM dashboards WHERE id = ?`, id)
	return err
}
