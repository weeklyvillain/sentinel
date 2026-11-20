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
	// NPlusOne is how many identical queries under one span make an N+1
	// performance issue (0 = off).
	NPlusOne int
}

// Release records the version a service is sending from.
func (p *Processor) Release(ctx context.Context, service, env, version string, t time.Time) {
	if err := p.Store.TouchRelease(ctx, service, env, version, t); err != nil {
		p.log().Error("touch release", "service", service, "err", err)
	}
}

// Spans redacts and stores spans and looks for N+1 queries.
func (p *Processor) Spans(ctx context.Context, spans []store.Span) {
	for i := range spans {
		sp := &spans[i]
		sp.Name = redact.String(sp.Name)
		sp.StatusMsg = redact.String(sp.StatusMsg)
		redact.Attrs(sp.Attrs)
		for j := range sp.Events {
			redact.Attrs(sp.Events[j].Attrs)
		}
	}
	if err := p.Store.InsertSpans(ctx, spans); err != nil {
		p.log().Error("insert spans", "count", len(spans), "err", err)
	}
	if p.NPlusOne > 0 {
		for _, e := range DetectNPlusOne(spans, p.NPlusOne) {
			p.Event(ctx, e)
		}
	}
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
			// The log line keeps the fingerprint it computed to (not the
			// group's, which differs after a merge) so merges can be undone.
			_, fp, err := p.record(ctx, *r.Error)
			if err != nil {
				p.log().Error("record event", "service", r.Error.Service, "err", err)
			} else {
				l.Fingerprint = fp
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
	g, _, err := p.record(ctx, e)
	return g, err
}

func (p *Processor) record(ctx context.Context, e event.Event) (store.Group, string, error) {
	if e.Time.IsZero() {
		e.Time = time.Now().UTC()
	}
	redact.Event(&e)
	fp := fingerprint.Compute(&e)
	g, err := p.Store.RecordEvent(ctx, fp, e)
	return g, fp, err
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
