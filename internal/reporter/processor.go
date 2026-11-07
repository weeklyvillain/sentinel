// Package reporter turns stored groups into GitHub issues and keeps them up to
// date, and detects services that stopped sending telemetry.
package reporter

import (
	"context"
	"log/slog"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/fingerprint"
	"github.com/weeklyvillain/sentinel/internal/ingest"
	"github.com/weeklyvillain/sentinel/internal/redact"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Processor is the ingest.Sink: it redacts, fingerprints and stores events.
type Processor struct {
	Store *store.Store
	Log   *slog.Logger
}

func (p *Processor) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

// Heartbeat records service liveness.
func (p *Processor) Heartbeat(ctx context.Context, service, env string, t time.Time) {
	if err := p.Store.TouchService(ctx, service, env, t); err != nil {
		p.log().Error("touch service", "service", service, "err", err)
	}
}

// Event redacts, groups and stores e.
func (p *Processor) Event(ctx context.Context, e event.Event) {
	if _, err := p.Record(ctx, e); err != nil {
		p.log().Error("record event", "service", e.Service, "err", err)
	}
}

// Records redacts and stores a batch of log records; errors are also grouped.
func (p *Processor) Records(ctx context.Context, recs []ingest.Record) {
	logs := make([]store.LogRecord, 0, len(recs))
	for _, r := range recs {
		l := r.Log
		l.Body = redact.String(l.Body)
		redact.Attrs(l.Attrs)
		if r.Error != nil {
			g, err := p.Record(ctx, *r.Error)
			if err != nil {
				p.log().Error("record event", "service", r.Error.Service, "err", err)
			} else {
				l.Fingerprint = g.Fingerprint
			}
		}
		logs = append(logs, l)
	}
	if err := p.Store.InsertLogs(ctx, logs); err != nil {
		p.log().Error("insert logs", "count", len(logs), "err", err)
	}
}

// Metrics stores metric points.
func (p *Processor) Metrics(ctx context.Context, pts []store.MetricPoint) {
	if err := p.Store.InsertMetrics(ctx, pts); err != nil {
		p.log().Error("insert metrics", "count", len(pts), "err", err)
	}
}

// Record is Event with the resulting group returned.
func (p *Processor) Record(ctx context.Context, e event.Event) (store.Group, error) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	redact.Event(&e)
	fp := fingerprint.Compute(&e)
	return p.Store.RecordEvent(ctx, fp, e)
}

// DownMessage is the stable message of synthetic down events.
const DownMessage = "service stopped sending telemetry"

// DownEvent builds the synthetic event for a service outage.
func DownEvent(service, env string, lastSeen, now time.Time) event.Event {
	return event.Event{
		Kind:        event.KindDown,
		Service:     service,
		Environment: env,
		Time:        now,
		Severity:    "ERROR",
		Message:     DownMessage,
		Attributes: map[string]string{
			"sentinel.last_seen": lastSeen.UTC().Format(time.RFC3339),
		},
	}
}

// DownFingerprint is the fingerprint of a service's down group.
func DownFingerprint(service string) string {
	e := DownEvent(service, "", time.Time{}, time.Time{})
	return fingerprint.Compute(&e)
}
