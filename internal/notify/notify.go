// Package notify delivers Sentinel events (issues created, outages, alarms)
// to user-configured webhooks.
package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

// Event types.
const (
	IssueCreated     = "issue.created"
	IssueRegression  = "issue.regression"
	IssueUpdated     = "issue.updated"
	ServiceDown      = "service.down"
	ServiceRecovered = "service.recovered"
	CheckDown        = "check.down"
	CheckRecovered   = "check.recovered"
	AlarmFiring      = "alarm.firing"
	AlarmResolved    = "alarm.resolved"
	FixRequested     = "fix.requested"
	FixOpened        = "fix.opened"
	FixMerged        = "fix.merged"
	SLOBurn          = "slo.burn"
	SLORecovered     = "slo.recovered"
	Test             = "test"
)

// EventTypes lists the subscribable event types (alarm events go to the
// webhooks chosen on each alarm).
var EventTypes = []string{IssueCreated, IssueRegression, IssueUpdated, FixRequested, FixOpened, FixMerged, ServiceDown, ServiceRecovered, CheckDown, CheckRecovered}

// GroupInfo summarizes an error group.
type GroupInfo struct {
	ID          int64  `json:"id"`
	Fingerprint string `json:"fingerprint"`
	Kind        string `json:"kind"`
	Title       string `json:"title"`
	Count       int64  `json:"count"`
	Env         string `json:"env,omitempty"`
}

// IssueInfo identifies a GitHub issue.
type IssueInfo struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url"`
	Title  string `json:"title,omitempty"`
}

// AlarmInfo describes an alarm transition.
type AlarmInfo struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	State     string  `json:"state"`
	Value     float64 `json:"value"`
	Op        string  `json:"op"`
	Threshold float64 `json:"threshold"`
}

// SLOInfo describes an SLO state change.
type SLOInfo struct {
	ID              int64   `json:"id"`
	Name            string  `json:"name"`
	State           string  `json:"state"`
	Objective       float64 `json:"objective"`
	SLI             float64 `json:"sli"`
	BudgetRemaining float64 `json:"budget_remaining"`
	BurnRate        float64 `json:"burn_rate"`
}

// Event is the webhook payload (format "json").
type Event struct {
	Type    string     `json:"event"`
	Time    time.Time  `json:"timestamp"`
	Service string     `json:"service,omitempty"`
	Message string     `json:"message"`
	Group   *GroupInfo `json:"group,omitempty"`
	Issue   *IssueInfo `json:"issue,omitempty"`
	Alarm   *AlarmInfo `json:"alarm,omitempty"`
	// Alarms lists every alarm of a grouped notification (several alarms
	// that changed state in the same evaluation, sent as one message).
	Alarms []AlarmInfo `json:"alarms,omitempty"`
	SLO    *SLOInfo    `json:"slo,omitempty"`
	// WebhookIDs, when non-nil, targets these webhooks instead of routing by subscription.
	WebhookIDs []int64 `json:"-"`
}

// Notifier accepts events.
type Notifier interface{ Notify(Event) }

// Nop discards events.
type Nop struct{}

func (Nop) Notify(Event) {}

// Dispatcher queues events and delivers them asynchronously with retries.
type Dispatcher struct {
	Store   *store.Store
	HTTP    *http.Client
	Log     *slog.Logger
	Backoff []time.Duration // delays between attempts; default 2s, 10s

	queue chan Event
	once  sync.Once
	wg    sync.WaitGroup
}

func (d *Dispatcher) init() {
	d.once.Do(func() {
		d.queue = make(chan Event, 1000)
		if d.HTTP == nil {
			d.HTTP = &http.Client{Timeout: 10 * time.Second}
		}
		if d.Log == nil {
			d.Log = slog.Default()
		}
		if d.Backoff == nil {
			d.Backoff = []time.Duration{2 * time.Second, 10 * time.Second}
		}
	})
}

// Notify enqueues an event; it never blocks (drops when the queue is full).
func (d *Dispatcher) Notify(ev Event) {
	d.init()
	if ev.Time.IsZero() {
		ev.Time = time.Now().UTC()
	}
	select {
	case d.queue <- ev:
	default:
		d.Log.Warn("webhook queue full; dropping event", "event", ev.Type)
	}
}

// Run delivers queued events until ctx is done.
func (d *Dispatcher) Run(ctx context.Context) {
	d.init()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-d.queue:
			d.dispatch(ctx, ev)
		}
	}
}

// Drain delivers everything currently queued (used by tests).
func (d *Dispatcher) Drain(ctx context.Context) {
	d.init()
	for {
		select {
		case ev := <-d.queue:
			d.dispatch(ctx, ev)
		default:
			d.wg.Wait()
			return
		}
	}
}

func (d *Dispatcher) dispatch(ctx context.Context, ev Event) {
	// Alarms and SLOs check silences themselves (per rule, before grouping).
	if ev.Type != Test && ev.Alarm == nil && ev.Alarms == nil && ev.SLO == nil {
		if sl, ok, err := d.Store.Silenced(ctx, ev.Service, 0, time.Now()); err == nil && ok {
			d.Log.Info("notification silenced", "event", ev.Type, "service", ev.Service, "silence", sl.ID)
			return
		}
	}
	hooks, err := d.Store.ListWebhooks(ctx)
	if err != nil {
		d.Log.Error("list webhooks", "err", err)
		return
	}
	for _, h := range hooks {
		if !h.Enabled || !targets(h, ev) {
			continue
		}
		d.wg.Add(1)
		go func(h store.Webhook) {
			defer d.wg.Done()
			d.deliver(ctx, h, ev)
		}(h)
	}
}

func targets(h store.Webhook, ev Event) bool {
	if ev.WebhookIDs != nil {
		for _, id := range ev.WebhookIDs {
			if id == h.ID {
				return true
			}
		}
		return false
	}
	return h.Subscribed(ev.Type)
}

func (d *Dispatcher) deliver(ctx context.Context, h store.Webhook, ev Event) store.Delivery {
	var last store.Delivery
	for attempt := 1; attempt <= len(d.Backoff)+1; attempt++ {
		last = d.SendOnce(ctx, h, ev)
		last.Attempt = attempt
		if last.Error == "" || !retryable(last.Status) || attempt > len(d.Backoff) {
			break
		}
		select {
		case <-ctx.Done():
			return last
		case <-time.After(d.Backoff[attempt-1]):
		}
	}
	if err := d.Store.RecordDelivery(context.WithoutCancel(ctx), last); err != nil {
		d.Log.Error("record delivery", "err", err)
	}
	if last.Error != "" {
		d.Log.Warn("webhook delivery failed", "webhook", h.Name, "event", ev.Type, "status", last.Status, "err", last.Error)
	}
	return last
}

func retryable(status int) bool { return status == 0 || status == 429 || status >= 500 }

// SendOnce performs a single delivery attempt (also used for "Send test").
func (d *Dispatcher) SendOnce(ctx context.Context, h store.Webhook, ev Event) store.Delivery {
	d.init()
	body, err := Payload(h.Format, ev)
	del := store.Delivery{WebhookID: h.ID, Event: ev.Type, Time: time.Now().UTC(), Payload: string(body)}
	if err != nil {
		del.Error = err.Error()
		return del
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(body))
	if err != nil {
		del.Error = err.Error()
		return del
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "sentinel-webhook/1")
	req.Header.Set("X-Sentinel-Event", ev.Type)
	req.Header.Set("X-Sentinel-Delivery", randomID())
	if h.Secret != "" {
		req.Header.Set("X-Sentinel-Signature", "sha256="+Sign(h.Secret, body))
	}
	start := time.Now()
	resp, err := d.HTTP.Do(req)
	del.DurationMS = time.Since(start).Milliseconds()
	if err != nil {
		del.Error = err.Error()
		return del
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	del.Status = resp.StatusCode
	if resp.StatusCode/100 != 2 {
		del.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return del
}

// Sign returns hex(HMAC-SHA256(secret, body)).
func Sign(secret string, body []byte) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write(body)
	return hex.EncodeToString(m.Sum(nil))
}

func randomID() string {
	b := make([]byte, 12)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Payload renders an event for a webhook format.
func Payload(format string, ev Event) ([]byte, error) {
	switch format {
	case "", "json":
		return marshal(ev)
	case "slack":
		return marshal(map[string]string{"text": chatText(ev, func(url, label string) string { return "<" + url + "|" + label + ">" })})
	case "discord":
		return marshal(map[string]string{"content": chatText(ev, func(url, label string) string { return "[" + label + "](" + url + ")" })})
	}
	return nil, fmt.Errorf("unknown webhook format %q", format)
}

func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func chatText(ev Event, link func(url, label string) string) string {
	icon := map[string]string{
		IssueCreated: "🆕", IssueRegression: "🔁", IssueUpdated: "📈", ServiceDown: "🔴", ServiceRecovered: "🟢", CheckDown: "🔴", CheckRecovered: "🟢",
		AlarmFiring: "🚨", AlarmResolved: "✅", FixRequested: "🤖", FixOpened: "🛠️", FixMerged: "✅", SLOBurn: "🔥", SLORecovered: "✅", Test: "🧪",
	}[ev.Type]
	var b strings.Builder
	fmt.Fprintf(&b, "%s *Sentinel* — %s", icon, ev.Message)
	if ev.Issue != nil && ev.Issue.URL != "" {
		fmt.Fprintf(&b, " %s", link(ev.Issue.URL, fmt.Sprintf("%s#%d", ev.Issue.Repo, ev.Issue.Number)))
	}
	return b.String()
}
