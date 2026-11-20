package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// Webhook is an outgoing notification target.
type Webhook struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	URL       string    `json:"url"`
	Secret    string    `json:"secret,omitempty"`
	Format    string    `json:"format"` // json | slack | discord
	Events    []string  `json:"events"` // subscribed event types (alarm events are routed per alarm)
	Enabled   bool      `json:"enabled"`
	Style     Style     `json:"style"` // message customization for slack/discord
	CreatedAt time.Time `json:"created_at"`
}

// Style customizes how a chat (Slack/Discord) webhook message looks. The
// zero value renders the classic one-line text message.
type Style struct {
	// Rich sends a card (Discord embed / Slack attachment) instead of a line of text.
	Rich bool `json:"rich"`
	// Template is the message text (plain) or card description (rich), with
	// {placeholders}; empty uses the default.
	Template string `json:"template,omitempty"`
	// Username and AvatarURL override the poster's name and picture.
	Username  string `json:"username,omitempty"`
	AvatarURL string `json:"avatar_url,omitempty"`
	// IconEmoji (Slack only, e.g. ":rotating_light:") overrides the avatar.
	IconEmoji string `json:"icon_emoji,omitempty"`
	// Mention is prepended to the message ("<@&role>", "<!channel>", …).
	Mention string `json:"mention,omitempty"`
	// MentionOn picks which events mention: "problems" (default) or "all".
	MentionOn string `json:"mention_on,omitempty"`
	// Card colors (#rrggbb) by kind of event; empty uses the default.
	ColorProblem  string `json:"color_problem,omitempty"`
	ColorRecovery string `json:"color_recovery,omitempty"`
	ColorInfo     string `json:"color_info,omitempty"`
	// HideFields drops the detail fields (service, issue, values) from cards.
	HideFields bool `json:"hide_fields,omitempty"`
	// Footer replaces the "Sentinel" card footer.
	Footer string `json:"footer,omitempty"`
}

// Subscribed reports whether the webhook wants event type ev.
func (w Webhook) Subscribed(ev string) bool {
	for _, e := range w.Events {
		if e == ev || e == "*" {
			return true
		}
	}
	return false
}

const webhookCols = `SELECT id, name, url, secret, format, events, enabled, style, created_at FROM webhooks`

func scanWebhook(r scanner) (Webhook, error) {
	var w Webhook
	var events, style string
	var enabled int
	var created int64
	err := r.Scan(&w.ID, &w.Name, &w.URL, &w.Secret, &w.Format, &events, &enabled, &style, &created)
	if err != nil {
		return w, err
	}
	json.Unmarshal([]byte(events), &w.Events)
	json.Unmarshal([]byte(style), &w.Style)
	if w.Events == nil {
		w.Events = []string{}
	}
	w.Enabled = enabled == 1
	w.CreatedAt = fromMs(created)
	return w, nil
}

// ListWebhooks returns all webhooks.
func (s *Store) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := s.db.QueryContext(ctx, webhookCols+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Webhook{}
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetWebhook returns one webhook.
func (s *Store) GetWebhook(ctx context.Context, id int64) (Webhook, error) {
	w, err := scanWebhook(s.db.QueryRowContext(ctx, webhookCols+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	return w, err
}

// SaveWebhook inserts (ID == 0) or updates a webhook and returns its id.
func (s *Store) SaveWebhook(ctx context.Context, w Webhook) (int64, error) {
	ev, _ := json.Marshal(w.Events)
	st, _ := json.Marshal(w.Style)
	if w.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO webhooks (name, url, secret, format, events, enabled, style, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			w.Name, w.URL, w.Secret, w.Format, string(ev), boolInt(w.Enabled), string(st), ms(time.Now()))
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE webhooks SET name = ?, url = ?, secret = ?, format = ?, events = ?, enabled = ?, style = ? WHERE id = ?`,
		w.Name, w.URL, w.Secret, w.Format, string(ev), boolInt(w.Enabled), string(st), w.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return w.ID, nil
}

// DeleteWebhook removes a webhook and its delivery log.
func (s *Store) DeleteWebhook(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id)
	return err
}

// Delivery is one attempt to call a webhook.
type Delivery struct {
	ID         int64     `json:"id"`
	WebhookID  int64     `json:"webhook_id"`
	Event      string    `json:"event"`
	Time       time.Time `json:"time"`
	Status     int       `json:"status"`
	Error      string    `json:"error,omitempty"`
	DurationMS int64     `json:"duration_ms"`
	Attempt    int       `json:"attempt"`
	Payload    string    `json:"payload,omitempty"`
}

// RecordDelivery logs a delivery and keeps the latest 200 per webhook.
func (s *Store) RecordDelivery(ctx context.Context, d Delivery) error {
	if _, err := s.db.ExecContext(ctx, `INSERT INTO webhook_deliveries (webhook_id, event, ts, status, error, duration_ms, attempt, payload) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		d.WebhookID, d.Event, ms(d.Time), d.Status, d.Error, d.DurationMS, d.Attempt, d.Payload); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM webhook_deliveries WHERE webhook_id = ?1 AND id NOT IN
		(SELECT id FROM webhook_deliveries WHERE webhook_id = ?1 ORDER BY id DESC LIMIT 200)`, d.WebhookID)
	return err
}

// ListDeliveries returns recent deliveries for a webhook, newest first.
func (s *Store) ListDeliveries(ctx context.Context, webhookID int64, limit int) ([]Delivery, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id, webhook_id, event, ts, status, error, duration_ms, attempt, payload FROM webhook_deliveries
		WHERE webhook_id = ? ORDER BY id DESC LIMIT ?`, webhookID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Delivery{}
	for rows.Next() {
		var d Delivery
		var ts int64
		if err := rows.Scan(&d.ID, &d.WebhookID, &d.Event, &ts, &d.Status, &d.Error, &d.DurationMS, &d.Attempt, &d.Payload); err != nil {
			return nil, err
		}
		d.Time = fromMs(ts)
		out = append(out, d)
	}
	return out, rows.Err()
}

// Alarm kinds.
const (
	AlarmLogCount    = "log_count"    // matching log lines in window
	AlarmMetric      = "metric"       // agg(metric) over window
	AlarmServiceDown = "service_down" // service(s) down
)

// Alarm states.
const (
	AlarmOK     = "ok"
	AlarmFiring = "firing"
	// AlarmPending: the condition holds, but not yet for the alarm's For.
	AlarmPending = "pending"
)

// Alarm is a user-defined alert rule.
type Alarm struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Enabled     bool    `json:"enabled"`
	Kind        string  `json:"kind"`
	Service     string  `json:"service"`
	Query       string  `json:"query"`
	MinSeverity int32   `json:"min_severity"`
	Metric      string  `json:"metric"`
	Agg         string  `json:"agg"`
	Op          string  `json:"op"` // > >= < <=
	Threshold   float64 `json:"threshold"`
	WindowS     int64   `json:"window_s"`
	// ForS: the condition must hold this long before the alarm fires.
	ForS         int64     `json:"for_s"`
	PendingSince time.Time `json:"pending_since,omitzero"`
	WebhookIDs   []int64   `json:"webhook_ids"`
	State        string    `json:"state"`
	LastValue    float64   `json:"last_value"`
	LastEval     time.Time `json:"last_eval,omitzero"`
	LastChanged  time.Time `json:"last_changed,omitzero"`
	CreatedAt    time.Time `json:"created_at"`
}

const alarmCols = `SELECT id, name, enabled, kind, service, query, min_severity, metric, agg, op, threshold, window_s, webhook_ids,
	state, last_value, last_eval, last_changed, created_at, for_s, pending_since FROM alarms`

func scanAlarm(r scanner) (Alarm, error) {
	var a Alarm
	var enabled int
	var hooks string
	var le, lc, ca, ps int64
	err := r.Scan(&a.ID, &a.Name, &enabled, &a.Kind, &a.Service, &a.Query, &a.MinSeverity, &a.Metric, &a.Agg, &a.Op, &a.Threshold,
		&a.WindowS, &hooks, &a.State, &a.LastValue, &le, &lc, &ca, &a.ForS, &ps)
	if err != nil {
		return a, err
	}
	a.PendingSince = fromMs(ps)
	a.Enabled = enabled == 1
	json.Unmarshal([]byte(hooks), &a.WebhookIDs)
	if a.WebhookIDs == nil {
		a.WebhookIDs = []int64{}
	}
	a.LastEval, a.LastChanged, a.CreatedAt = fromMs(le), fromMs(lc), fromMs(ca)
	return a, nil
}

// ListAlarms returns all alarms.
func (s *Store) ListAlarms(ctx context.Context) ([]Alarm, error) {
	rows, err := s.db.QueryContext(ctx, alarmCols+` ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Alarm{}
	for rows.Next() {
		a, err := scanAlarm(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetAlarm returns one alarm.
func (s *Store) GetAlarm(ctx context.Context, id int64) (Alarm, error) {
	a, err := scanAlarm(s.db.QueryRowContext(ctx, alarmCols+` WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return a, ErrNotFound
	}
	return a, err
}

// SaveAlarm inserts (ID == 0) or updates an alarm's definition.
func (s *Store) SaveAlarm(ctx context.Context, a Alarm) (int64, error) {
	hooks, _ := json.Marshal(a.WebhookIDs)
	if a.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO alarms (name, enabled, kind, service, query, min_severity, metric, agg, op, threshold, window_s, webhook_ids, state, created_at, for_s)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'ok', ?, ?)`,
			a.Name, boolInt(a.Enabled), a.Kind, a.Service, a.Query, a.MinSeverity, a.Metric, a.Agg, a.Op, a.Threshold, a.WindowS, string(hooks), ms(time.Now()), a.ForS)
		if err != nil {
			return 0, err
		}
		return res.LastInsertId()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE alarms SET name = ?, enabled = ?, kind = ?, service = ?, query = ?, min_severity = ?, metric = ?, agg = ?,
		op = ?, threshold = ?, window_s = ?, webhook_ids = ?, for_s = ? WHERE id = ?`,
		a.Name, boolInt(a.Enabled), a.Kind, a.Service, a.Query, a.MinSeverity, a.Metric, a.Agg, a.Op, a.Threshold, a.WindowS, string(hooks), a.ForS, a.ID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return a.ID, nil
}

// DeleteAlarm removes an alarm and its history.
func (s *Store) DeleteAlarm(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM alarms WHERE id = ?`, id)
	return err
}

// RecordAlarmEval stores the latest evaluation; if the state changed it also
// appends a history entry.
func (s *Store) RecordAlarmEval(ctx context.Context, id int64, state string, value float64, changed bool, msg string, now time.Time) error {
	if changed {
		if _, err := s.db.ExecContext(ctx, `UPDATE alarms SET state = ?, last_value = ?, last_eval = ?, last_changed = ? WHERE id = ?`,
			state, value, ms(now), ms(now), id); err != nil {
			return err
		}
		_, err := s.db.ExecContext(ctx, `INSERT INTO alarm_events (alarm_id, ts, state, value, message) VALUES (?, ?, ?, ?, ?)`, id, ms(now), state, value, msg)
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE alarms SET last_value = ?, last_eval = ? WHERE id = ?`, value, ms(now), id)
	return err
}

// SetAlarmPending records since when an alarm's condition holds (zero = it
// does not).
func (s *Store) SetAlarmPending(ctx context.Context, id int64, since time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alarms SET pending_since = ? WHERE id = ?`, ms(since), id)
	return err
}

// AlarmEventsBetween returns transitions in [from, to), oldest first (for annotations).
func (s *Store) AlarmEventsBetween(ctx context.Context, from, to time.Time) ([]AlarmEvent, error) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT id, alarm_id, ts, state, value, message FROM alarm_events WHERE ts >= ? AND ts < ? ORDER BY ts LIMIT 500`, ms(from), ms(to))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlarmEvent{}
	for rows.Next() {
		var e AlarmEvent
		var ts int64
		if err := rows.Scan(&e.ID, &e.AlarmID, &ts, &e.State, &e.Value, &e.Message); err != nil {
			return nil, err
		}
		e.Time = fromMs(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// AlarmEvent is one state transition.
type AlarmEvent struct {
	ID      int64     `json:"id"`
	AlarmID int64     `json:"alarm_id"`
	Time    time.Time `json:"time"`
	State   string    `json:"state"`
	Value   float64   `json:"value"`
	Message string    `json:"message"`
}

// ListAlarmEvents returns recent transitions (alarmID 0 = all alarms).
func (s *Store) ListAlarmEvents(ctx context.Context, alarmID int64, limit int) ([]AlarmEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	q := `SELECT id, alarm_id, ts, state, value, message FROM alarm_events`
	var args []any
	if alarmID > 0 {
		q += ` WHERE alarm_id = ?`
		args = append(args, alarmID)
	}
	q += ` ORDER BY id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []AlarmEvent{}
	for rows.Next() {
		var e AlarmEvent
		var ts int64
		if err := rows.Scan(&e.ID, &e.AlarmID, &ts, &e.State, &e.Value, &e.Message); err != nil {
			return nil, err
		}
		e.Time = fromMs(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}
