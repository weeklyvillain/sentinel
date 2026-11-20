package reporter

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/fingerprint"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Performance issues are found in traces and go through the same grouping
// and issue pipeline as errors (kind "perf"):
//
//   - N+1 queries: one span with many identical database calls under it
//     (found as spans arrive).
//   - Slow endpoints: an entry operation whose p95 over 15 minutes is above
//     PERF_SLOW_P95.
//   - Latency regressions: an operation's p95 over the last hour is more than
//     twice its p95 over the day before.

// PerfAttr names the kind of performance problem on perf events.
const PerfAttr = "sentinel.perf"

func perfEvent(service, env, release, key, msg string, t time.Time, attrs map[string]string) event.Event {
	a := map[string]string{event.FingerprintAttr: key}
	for k, v := range attrs {
		a[k] = v
	}
	return event.Event{Kind: event.KindPerf, Service: service, Environment: env, Release: release, Time: t, Severity: "WARN", Message: msg, Attributes: a}
}

// dbStatement is what identifies a database call.
func dbStatement(sp store.Span) (string, bool) {
	for _, k := range []string{"db.query.text", "db.statement"} {
		if v := sp.Attrs[k]; v != "" {
			return v, true
		}
	}
	if sp.Attrs["db.system"] != "" || sp.Attrs["db.system.name"] != "" {
		return sp.Name, true
	}
	return "", false
}

// DetectNPlusOne finds spans with at least n identical database calls as
// direct children, and returns one perf event per (parent, statement).
func DetectNPlusOne(spans []store.Span, n int) []event.Event {
	if n <= 0 {
		return nil
	}
	byID := map[string]*store.Span{}
	for i := range spans {
		byID[spans[i].TraceID+"/"+spans[i].SpanID] = &spans[i]
	}
	type key struct{ trace, parent, stmt string }
	counts := map[key]int{}
	first := map[key]store.Span{}
	for _, sp := range spans {
		if sp.ParentID == "" {
			continue
		}
		stmt, ok := dbStatement(sp)
		if !ok {
			continue
		}
		k := key{sp.TraceID, sp.ParentID, fingerprint.NormalizeMessage(stmt)}
		if counts[k] == 0 {
			first[k] = sp
		}
		counts[k]++
	}
	var out []event.Event
	for k, c := range counts {
		if c < n {
			continue
		}
		sp := first[k]
		parentName := "(unknown span)"
		if p := byID[k.trace+"/"+k.parent]; p != nil {
			parentName = p.Name
		}
		e := perfEvent(sp.Service, sp.Env, sp.Version, "n+1|"+parentName+"|"+k.stmt,
			fmt.Sprintf("N+1 query: %q ran %d times under %s", truncateStr(k.stmt, 200), c, parentName), sp.Start,
			map[string]string{PerfAttr: "n_plus_one", "db.statement": k.stmt, "sentinel.repeats": strconv.Itoa(c), "sentinel.parent_span": parentName})
		e.TraceID, e.SpanID, e.SpanName = k.trace, k.parent, parentName
		out = append(out, e)
	}
	return out
}

func truncateStr(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

const perfEvery = 5 * time.Minute

// detectPerf looks for slow endpoints and latency regressions every few minutes.
func (r *Reporter) detectPerf(ctx context.Context) error {
	now := r.now()
	if now.Sub(r.perfAt) < perfEvery {
		return nil
	}
	r.perfAt = now
	recent, err := r.Store.Operations(ctx, "", now.Add(-15*time.Minute), now)
	if err != nil {
		return err
	}
	for _, op := range recent {
		if r.Cfg.PerfSlowP95 <= 0 || op.Count < 20 || op.P95MS < float64(r.Cfg.PerfSlowP95.Milliseconds()) {
			continue
		}
		e := perfEvent(op.Service, "", "", "slow|"+op.Name,
			fmt.Sprintf("Slow endpoint: %s has a p95 of %s (%d requests in 15 min, limit %s)", op.Name, fmtMS(op.P95MS), op.Count, r.Cfg.PerfSlowP95),
			now, map[string]string{PerfAttr: "slow_endpoint", "sentinel.operation": op.Name, "sentinel.p95_ms": fmt.Sprintf("%.0f", op.P95MS),
				"sentinel.p50_ms": fmt.Sprintf("%.0f", op.P50MS), "sentinel.requests": strconv.FormatInt(op.Count, 10)})
		r.attachSlowTrace(ctx, &e, op, r.Cfg.PerfSlowP95, now.Add(-15*time.Minute), now)
		if _, err := r.Processor.Record(ctx, e); err != nil {
			return err
		}
	}
	hour, err := r.Store.Operations(ctx, "", now.Add(-time.Hour), now)
	if err != nil {
		return err
	}
	for _, op := range hour {
		if op.Count < 50 {
			continue
		}
		base, _, err := r.Store.OpWindow(ctx, op.Service, op.Name, now.Add(-25*time.Hour), now.Add(-time.Hour))
		if err != nil {
			return err
		}
		if base.Count < 200 || op.P95MS < 2*base.P95MS || op.P95MS-base.P95MS < 100 {
			continue
		}
		e := perfEvent(op.Service, "", "", "latency-regression|"+op.Name,
			fmt.Sprintf("Latency regression: p95 of %s went from %s to %s", op.Name, fmtMS(base.P95MS), fmtMS(op.P95MS)),
			now, map[string]string{PerfAttr: "latency_regression", "sentinel.operation": op.Name,
				"sentinel.p95_ms": fmt.Sprintf("%.0f", op.P95MS), "sentinel.baseline_p95_ms": fmt.Sprintf("%.0f", base.P95MS)})
		r.attachSlowTrace(ctx, &e, op, time.Duration(base.P95MS*float64(time.Millisecond)), now.Add(-time.Hour), now)
		if _, err := r.Processor.Record(ctx, e); err != nil {
			return err
		}
	}
	return nil
}

// attachSlowTrace links an example trace slower than min.
func (r *Reporter) attachSlowTrace(ctx context.Context, e *event.Event, op store.OpStats, min time.Duration, from, to time.Time) {
	tr, err := r.Store.SearchTraces(ctx, store.TraceQuery{Service: op.Service, Name: op.Name, MinDuration: min, From: from, To: to, Limit: 1})
	if err == nil && len(tr) > 0 {
		e.TraceID = tr[0].TraceID
		e.SpanName = op.Name
	}
}

func fmtMS(ms float64) string {
	if ms >= 1000 {
		return fmt.Sprintf("%.2f s", ms/1000)
	}
	return fmt.Sprintf("%.0f ms", ms)
}
