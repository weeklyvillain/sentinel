package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

const schemaAlerting = `
-- Maintenance windows: notifications matching them are not sent.
CREATE TABLE IF NOT EXISTS silences (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	service TEXT NOT NULL DEFAULT '',
	alarm_id INTEGER NOT NULL DEFAULT 0,
	starts_at INTEGER NOT NULL,
	ends_at INTEGER NOT NULL,
	comment TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL
);

-- Service level objectives over request statistics or uptime checks.
CREATE TABLE IF NOT EXISTS slos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	name TEXT NOT NULL,
	kind TEXT NOT NULL,
	service TEXT NOT NULL DEFAULT '',
	operation TEXT NOT NULL DEFAULT '',
	check_id INTEGER NOT NULL DEFAULT 0,
	threshold_ms REAL NOT NULL DEFAULT 0,
	objective REAL NOT NULL,
	window_days INTEGER NOT NULL DEFAULT 30,
	webhook_ids TEXT NOT NULL DEFAULT '[]',
	state TEXT NOT NULL DEFAULT 'ok',
	last_eval INTEGER NOT NULL DEFAULT 0,
	last_changed INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);
`

// Silence mutes notifications for a service and/or alarm for a while.
type Silence struct {
	ID        int64     `json:"id"`
	Service   string    `json:"service"`  // "" = every service
	AlarmID   int64     `json:"alarm_id"` // 0 = any alarm or event
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	Comment   string    `json:"comment"`
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Active reports whether the silence is in effect at t.
func (s Silence) Active(t time.Time) bool { return !t.Before(s.StartsAt) && t.Before(s.EndsAt) }

// Matches reports whether the silence covers a notification.
func (s Silence) Matches(service string, alarmID int64) bool {
	if s.Service != "" && s.Service != service {
		return false
	}
	return s.AlarmID == 0 || s.AlarmID == alarmID
}

// ListSilences returns silences that have not ended before since, newest first.
func (s *Store) ListSilences(ctx context.Context, since time.Time) ([]Silence, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, service, alarm_id, starts_at, ends_at, comment, created_by, created_at FROM silences
		WHERE ends_at >= ? ORDER BY starts_at DESC`, ms(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Silence{}
	for rows.Next() {
		var sl Silence
		var st, en, ca int64
		if err := rows.Scan(&sl.ID, &sl.Service, &sl.AlarmID, &st, &en, &sl.Comment, &sl.CreatedBy, &ca); err != nil {
			return nil, err
		}
		sl.StartsAt, sl.EndsAt, sl.CreatedAt = fromMs(st), fromMs(en), fromMs(ca)
		out = append(out, sl)
	}
	return out, rows.Err()
}

// SaveSilence inserts (ID == 0) or updates a silence.
func (s *Store) SaveSilence(ctx context.Context, sl Silence) (int64, error) {
	if sl.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO silences (service, alarm_id, starts_at, ends_at, comment, created_by, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			sl.Service, sl.AlarmID, ms(sl.StartsAt), ms(sl.EndsAt), sl.Comment, sl.CreatedBy, ms(time.Now()))
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE silences SET service = ?, alarm_id = ?, starts_at = ?, ends_at = ?, comment = ? WHERE id = ?`,
		sl.Service, sl.AlarmID, ms(sl.StartsAt), ms(sl.EndsAt), sl.Comment, sl.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return sl.ID, nil
}

// DeleteSilence removes a silence.
func (s *Store) DeleteSilence(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM silences WHERE id = ?`, id)
	return err
}

// Silenced returns the active silence covering a notification, if any.
func (s *Store) Silenced(ctx context.Context, service string, alarmID int64, now time.Time) (Silence, bool, error) {
	list, err := s.ListSilences(ctx, now)
	if err != nil {
		return Silence{}, false, err
	}
	for _, sl := range list {
		if sl.Active(now) && sl.Matches(service, alarmID) {
			return sl, true, nil
		}
	}
	return Silence{}, false, nil
}

// SLO kinds.
const (
	SLOLatency      = "latency"      // requests faster than threshold_ms (and not failed)
	SLOAvailability = "availability" // requests that did not fail
	SLOUptime       = "uptime"       // successful uptime check probes
)

// SLO states, by burn rate (how fast the error budget is used).
const (
	SLOOk       = "ok"
	SLOSlowBurn = "slow_burn" // 6× over 6h and 30m
	SLOFastBurn = "fast_burn" // 14.4× over 1h and 5m
)

// SLO is a service level objective.
type SLO struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Service     string    `json:"service"`
	Operation   string    `json:"operation"` // entry span name; "" = all
	CheckID     int64     `json:"check_id"`
	ThresholdMS float64   `json:"threshold_ms"`
	Objective   float64   `json:"objective"` // percent, e.g. 99.9
	WindowDays  int       `json:"window_days"`
	WebhookIDs  []int64   `json:"webhook_ids"`
	State       string    `json:"state"`
	LastEval    time.Time `json:"last_eval,omitzero"`
	LastChanged time.Time `json:"last_changed,omitzero"`
	CreatedAt   time.Time `json:"created_at"`
}

const sloCols = `SELECT id, name, kind, service, operation, check_id, threshold_ms, objective, window_days, webhook_ids, state, last_eval, last_changed, created_at FROM slos`

func scanSLO(r scanner) (SLO, error) {
	var o SLO
	var hooks string
	var le, lc, ca int64
	if err := r.Scan(&o.ID, &o.Name, &o.Kind, &o.Service, &o.Operation, &o.CheckID, &o.ThresholdMS, &o.Objective, &o.WindowDays, &hooks,
		&o.State, &le, &lc, &ca); err != nil {
		return o, err
	}
	json.Unmarshal([]byte(hooks), &o.WebhookIDs)
	if o.WebhookIDs == nil {
		o.WebhookIDs = []int64{}
	}
	o.LastEval, o.LastChanged, o.CreatedAt = fromMs(le), fromMs(lc), fromMs(ca)
	return o, nil
}

// ListSLOs returns all SLOs.
func (s *Store) ListSLOs(ctx context.Context) ([]SLO, error) {
	rows, err := s.db.QueryContext(ctx, sloCols+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []SLO{}
	for rows.Next() {
		o, err := scanSLO(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// GetSLO returns one SLO.
func (s *Store) GetSLO(ctx context.Context, id int64) (SLO, error) {
	o, err := scanSLO(s.db.QueryRowContext(ctx, sloCols+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return o, ErrNotFound
	}
	return o, err
}

// SaveSLO inserts (ID == 0) or updates an SLO definition.
func (s *Store) SaveSLO(ctx context.Context, o SLO) (int64, error) {
	hooks, _ := json.Marshal(o.WebhookIDs)
	if o.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO slos (name, kind, service, operation, check_id, threshold_ms, objective, window_days, webhook_ids, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, o.Name, o.Kind, o.Service, o.Operation, o.CheckID, o.ThresholdMS, o.Objective, o.WindowDays, string(hooks), ms(time.Now()))
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE slos SET name = ?, kind = ?, service = ?, operation = ?, check_id = ?, threshold_ms = ?, objective = ?, window_days = ?, webhook_ids = ? WHERE id = ?`,
		o.Name, o.Kind, o.Service, o.Operation, o.CheckID, o.ThresholdMS, o.Objective, o.WindowDays, string(hooks), o.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return o.ID, nil
}

// DeleteSLO removes an SLO.
func (s *Store) DeleteSLO(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM slos WHERE id = ?`, id)
	return err
}

// RecordSLOEval stores an SLO's latest state.
func (s *Store) RecordSLOEval(ctx context.Context, id int64, state string, changed bool, now time.Time) error {
	if changed {
		_, err := s.db.ExecContext(ctx, `UPDATE slos SET state = ?, last_eval = ?, last_changed = ? WHERE id = ?`, state, ms(now), ms(now), id)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE slos SET last_eval = ? WHERE id = ?`, ms(now), id)
	return err
}

// CheckTally counts an uptime check's probes over [from, to).
func (s *Store) CheckTally(ctx context.Context, checkID int64, from, to time.Time) (total, ok int64, err error) {
	err = s.rdb.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(ok), 0) FROM check_results WHERE check_id = ? AND ts >= ? AND ts < ?`,
		checkID, ms(from), ms(to)).Scan(&total, &ok)
	return
}
