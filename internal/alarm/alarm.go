// Package alarm evaluates user-defined alert rules over stored logs, metrics
// and service liveness, and notifies webhooks when an alarm changes state.
package alarm

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
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

// Tick evaluates every enabled alarm once.
func (e *Evaluator) Tick(ctx context.Context) {
	alarms, err := e.Store.ListAlarms(ctx)
	if err != nil {
		e.log().Error("list alarms", "err", err)
		return
	}
	now := e.now()
	for _, a := range alarms {
		if !a.Enabled {
			continue
		}
		res, err := Evaluate(ctx, e.Store, a, now)
		if err != nil {
			e.log().Warn("alarm evaluation failed", "alarm", a.Name, "err", err)
			continue
		}
		state := store.AlarmOK
		if res.Firing {
			state = store.AlarmFiring
		}
		changed := state != a.State
		if err := e.Store.RecordAlarmEval(ctx, a.ID, state, res.Value, changed, res.Message, now); err != nil {
			e.log().Error("record alarm", "alarm", a.Name, "err", err)
			continue
		}
		if !changed {
			continue
		}
		typ := notify.AlarmResolved
		if res.Firing {
			typ = notify.AlarmFiring
		}
		e.log().Info("alarm state changed", "alarm", a.Name, "state", state, "value", res.Value)
		if e.Notify != nil {
			ids := a.WebhookIDs
			if ids == nil {
				ids = []int64{}
			}
			e.Notify.Notify(notify.Event{
				Type: typ, Time: now, Service: a.Service, Message: res.Message, WebhookIDs: ids,
				Alarm: &notify.AlarmInfo{ID: a.ID, Name: a.Name, State: state, Value: res.Value, Op: a.Op, Threshold: a.Threshold},
			})
		}
	}
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
		case "avg", "sum", "max", "min", "count", "last":
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
