// Package alarm evaluates user-defined alert rules over stored logs, metrics
// and service liveness, and notifies webhooks when an alarm changes state.
package alarm

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Evaluator runs alarms on an interval.
type Evaluator struct {
	Store    *store.Store
	Notify   notify.Notifier
	Log      *slog.Logger
	Now      func() time.Time
	Interval time.Duration // default 15s

	sloMu sync.Mutex
	sloAt time.Time
}

func (e *Evaluator) now() time.Time {
	if e.Now != nil {
		return e.Now().UTC()
	}
	return time.Now().UTC()
}

func (e *Evaluator) log() *slog.Logger {
	if e.Log != nil {
		return e.Log
	}
	return slog.Default()
}

// Run evaluates until ctx is done.
func (e *Evaluator) Run(ctx context.Context) {
	iv := e.Interval
	if iv <= 0 {
		iv = 15 * time.Second
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		e.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick evaluates every enabled alarm once. An alarm with For set goes
// through "pending" and fires only when its condition held that long.
// Alarms that change state in the same pass and notify the same webhooks
// are sent as one grouped notification; silenced alarms notify nobody.
func (e *Evaluator) Tick(ctx context.Context) {
	alarms, err := e.Store.ListAlarms(ctx)
	if err != nil {
		e.log().Error("list alarms", "err", err)
		return
	}
	now := e.now()
	var out []transition
	for _, a := range alarms {
		if !a.Enabled {
			continue
		}
		res, err := Evaluate(ctx, e.Store, a, now)
		if err != nil {
			e.log().Warn("alarm evaluation failed", "alarm", a.Name, "err", err)
			continue
		}
		state := NextState(a, res.Firing, now)
		if res.Firing && a.PendingSince.IsZero() {
			if err := e.Store.SetAlarmPending(ctx, a.ID, now); err != nil {
				e.log().Error("record alarm", "alarm", a.Name, "err", err)
			}
		} else if !res.Firing && !a.PendingSince.IsZero() {
			if err := e.Store.SetAlarmPending(ctx, a.ID, time.Time{}); err != nil {
				e.log().Error("record alarm", "alarm", a.Name, "err", err)
			}
		}
		changed := state != a.State
		msg := res.Message
		if state == store.AlarmPending {
			msg = "Pending — " + msg
		}
		if err := e.Store.RecordAlarmEval(ctx, a.ID, state, res.Value, changed, msg, now); err != nil {
			e.log().Error("record alarm", "alarm", a.Name, "err", err)
			continue
		}
		if !changed {
			continue
		}
		e.log().Info("alarm state changed", "alarm", a.Name, "state", state, "value", res.Value)
		// Only firing and recovering from firing are worth a message.
		if state != store.AlarmFiring && a.State != store.AlarmFiring {
			continue
		}
		if sl, ok, err := e.Store.Silenced(ctx, a.Service, a.ID, now); err == nil && ok {
			e.log().Info("alarm notification silenced", "alarm", a.Name, "silence", sl.ID)
			continue
		}
		out = append(out, transition{a: a, state: state, res: res})
	}
	if e.Notify != nil {
		for _, ev := range groupTransitions(out, now) {
			e.Notify.Notify(ev)
		}
	}
	e.tickSLOs(ctx, now)
}

// NextState is an alarm's state after an evaluation.
func NextState(a store.Alarm, condition bool, now time.Time) string {
	switch {
	case !condition:
		return store.AlarmOK
	case a.State == store.AlarmFiring || a.ForS <= 0:
		return store.AlarmFiring
	case !a.PendingSince.IsZero() && now.Sub(a.PendingSince) >= time.Duration(a.ForS)*time.Second:
		return store.AlarmFiring
	}
	return store.AlarmPending
}

type transition struct {
	a     store.Alarm
	state string
	res   Result
}

func alarmInfo(t transition) notify.AlarmInfo {
	return notify.AlarmInfo{ID: t.a.ID, Name: t.a.Name, State: t.state, Value: t.res.Value, Op: t.a.Op, Threshold: t.a.Threshold}
}

// groupTransitions builds one notification per set of target webhooks.
func groupTransitions(ts []transition, now time.Time) []notify.Event {
	byHooks := map[string][]transition{}
	var order []string
	for _, t := range ts {
		ids := append([]int64(nil), t.a.WebhookIDs...)
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		k := fmt.Sprint(ids)
		if _, ok := byHooks[k]; !ok {
			order = append(order, k)
		}
		byHooks[k] = append(byHooks[k], t)
	}
	var out []notify.Event
	for _, k := range order {
		g := byHooks[k]
		ids := g[0].a.WebhookIDs
		if ids == nil {
			ids = []int64{}
		}
		if len(g) == 1 {
			t := g[0]
			typ := notify.AlarmResolved
			if t.state == store.AlarmFiring {
				typ = notify.AlarmFiring
			}
			info := alarmInfo(t)
			out = append(out, notify.Event{Type: typ, Time: now, Service: t.a.Service, Message: t.res.Message, WebhookIDs: ids, Alarm: &info})
			continue
		}
		typ := notify.AlarmResolved
		var firing, resolved []string
		var infos []notify.AlarmInfo
		var lines []string
		for _, t := range g {
			infos = append(infos, alarmInfo(t))
			lines = append(lines, "• "+t.res.Message)
			if t.state == store.AlarmFiring {
				typ = notify.AlarmFiring
				firing = append(firing, t.a.Name)
			} else {
				resolved = append(resolved, t.a.Name)
			}
		}
		head := fmt.Sprintf("%d alarms changed state", len(g))
		if len(firing) > 0 {
			head += fmt.Sprintf(" — firing: %s", strings.Join(firing, ", "))
		}
		if len(resolved) > 0 {
			head += fmt.Sprintf(" — resolved: %s", strings.Join(resolved, ", "))
		}
		out = append(out, notify.Event{Type: typ, Time: now, Message: head + "\n" + strings.Join(lines, "\n"), WebhookIDs: ids, Alarms: infos})
	}
	return out
}

// Result of evaluating one alarm.
type Result struct {
	Value   float64 `json:"value"`
	Firing  bool    `json:"firing"`
	NoData  bool    `json:"no_data"`
	Message string  `json:"message"`
}

// Compare applies op.
func Compare(v float64, op string, threshold float64) bool {
	switch op {
	case ">":
		return v > threshold
	case ">=":
		return v >= threshold
	case "<":
		return v < threshold
	case "<=":
		return v <= threshold
	case "==":
		return v == threshold
	}
	return false
}

// Validate checks an alarm definition.
func Validate(a store.Alarm) error {
	if strings.TrimSpace(a.Name) == "" {
		return fmt.Errorf("name is required")
	}
	switch a.Kind {
	case store.AlarmLogCount:
	case store.AlarmMetric:
		if a.Metric == "" {
			return fmt.Errorf("metric is required")
		}
		switch a.Agg {
		case "avg", "sum", "max", "min", "count", "last", "p50", "p90", "p95", "p99":
		default:
			return fmt.Errorf("unknown aggregation %q", a.Agg)
		}
	case store.AlarmServiceDown:
	default:
		return fmt.Errorf("unknown alarm kind %q", a.Kind)
	}
	if a.Kind != store.AlarmServiceDown {
		switch a.Op {
		case ">", ">=", "<", "<=", "==":
		default:
			return fmt.Errorf("unknown comparison %q", a.Op)
		}
		if a.WindowS < 10 || a.WindowS > 7*24*3600 {
			return fmt.Errorf("window must be between 10s and 7d")
		}
		if a.ForS < 0 || a.ForS > 24*3600 {
			return fmt.Errorf("for must be between 0 and 24h")
		}
	}
	return nil
}

func window(s int64) string { return (time.Duration(s) * time.Second).String() }

// Evaluate computes an alarm's current value (also used for "preview").
func Evaluate(ctx context.Context, st *store.Store, a store.Alarm, now time.Time) (Result, error) {
	scope := "all services"
	if a.Service != "" {
		scope = a.Service
	}
	switch a.Kind {
	case store.AlarmLogCount:
		q := store.LogQuery{Q: a.Query, MinSeverity: a.MinSeverity, From: now.Add(-time.Duration(a.WindowS) * time.Second), To: now}
		if a.Service != "" {
			q.Services = []string{a.Service}
		}
		n, err := st.CountLogs(ctx, q)
		if err != nil {
			return Result{}, err
		}
		v := float64(n)
		what := "matching log lines"
		if a.Query != "" {
			what = fmt.Sprintf("log lines matching %q", a.Query)
		}
		r := Result{Value: v, Firing: Compare(v, a.Op, a.Threshold)}
		r.Message = fmt.Sprintf("%s: %d %s in %s over %s (threshold %s %g)", a.Name, n, what, scope, window(a.WindowS), a.Op, a.Threshold)
		if !r.Firing {
			r.Message = "Resolved — " + r.Message
		}
		return r, nil
	case store.AlarmMetric:
		v, ok, err := st.MetricAggregate(ctx, a.Service, a.Metric, a.Agg, now.Add(-time.Duration(a.WindowS)*time.Second), now)
		if err != nil {
			return Result{}, err
		}
		if !ok {
			return Result{NoData: true, Message: fmt.Sprintf("%s: no data for %s in %s over %s", a.Name, a.Metric, scope, window(a.WindowS))}, nil
		}
		r := Result{Value: v, Firing: Compare(v, a.Op, a.Threshold)}
		r.Message = fmt.Sprintf("%s: %s(%s) = %.4g in %s over %s (threshold %s %g)", a.Name, a.Agg, a.Metric, v, scope, window(a.WindowS), a.Op, a.Threshold)
		if !r.Firing {
			r.Message = "Resolved — " + r.Message
		}
		return r, nil
	case store.AlarmServiceDown:
		svcs, err := st.ListServices(ctx)
		if err != nil {
			return Result{}, err
		}
		var down []string
		for _, s := range svcs {
			if s.Down && (a.Service == "" || a.Service == s.Name) {
				down = append(down, s.Name)
			}
		}
		r := Result{Value: float64(len(down)), Firing: len(down) > 0}
		if r.Firing {
			r.Message = fmt.Sprintf("%s: down — %s", a.Name, strings.Join(down, ", "))
		} else {
			r.Message = fmt.Sprintf("Resolved — %s: %s is up", a.Name, scope)
		}
		return r, nil
	}
	return Result{}, fmt.Errorf("unknown alarm kind %q", a.Kind)
}
