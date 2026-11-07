package alarm

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

type rec struct{ events []notify.Event }

func (r *rec) Notify(e notify.Event) { r.events = append(r.events, e) }

func TestLogCountAlarmFiresAndResolves(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	n := &rec{}
	ev := &Evaluator{Store: st, Notify: n, Now: func() time.Time { return now }}
	id, err := st.SaveAlarm(ctx, store.Alarm{Name: "checkout errors", Enabled: true, Kind: store.AlarmLogCount, Service: "checkout",
		MinSeverity: 17, Op: ">", Threshold: 3, WindowS: 300, WebhookIDs: []int64{5}})
	if err != nil {
		t.Fatal(err)
	}
	ev.Tick(ctx)
	if len(n.events) != 0 {
		t.Fatal("fired with no data")
	}
	var logs []store.LogRecord
	for i := 0; i < 5; i++ {
		logs = append(logs, store.LogRecord{Time: now.Add(-time.Minute), Service: "checkout", Severity: 17, Body: "boom"})
	}
	logs = append(logs, store.LogRecord{Time: now.Add(-time.Minute), Service: "billing", Severity: 17, Body: "boom"})
	st.InsertLogs(ctx, logs)
	ev.Tick(ctx)
	ev.Tick(ctx) // no duplicate notification while still firing
	if len(n.events) != 1 || n.events[0].Type != notify.AlarmFiring || n.events[0].Alarm.Value != 5 || n.events[0].WebhookIDs[0] != 5 {
		t.Fatalf("firing: %+v", n.events)
	}
	a, _ := st.GetAlarm(ctx, id)
	if a.State != store.AlarmFiring {
		t.Errorf("state %s", a.State)
	}
	now = now.Add(10 * time.Minute) // errors leave the window
	ev.Tick(ctx)
	if len(n.events) != 2 || n.events[1].Type != notify.AlarmResolved {
		t.Fatalf("resolve: %+v", n.events)
	}
	hist, _ := st.ListAlarmEvents(ctx, id, 10)
	if len(hist) != 2 {
		t.Errorf("history: %d", len(hist))
	}
}

func TestServiceDownAndMetricAlarm(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(filepath.Join(t.TempDir(), "a.db"))
	defer st.Close()
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	st.TouchService(ctx, "api", "", now.Add(-time.Hour))
	st.MarkServiceDown(ctx, "api", now.Add(-time.Hour))
	r, err := Evaluate(ctx, st, store.Alarm{Name: "down", Kind: store.AlarmServiceDown}, now)
	if err != nil || !r.Firing || r.Value != 1 {
		t.Fatalf("down: %+v %v", r, err)
	}
	st.InsertMetrics(ctx, []store.MetricPoint{{Time: now.Add(-time.Minute), Service: "api", Name: "queue.depth", Value: 250}})
	r, _ = Evaluate(ctx, st, store.Alarm{Name: "q", Kind: store.AlarmMetric, Metric: "queue.depth", Agg: "max", Op: ">=", Threshold: 200, WindowS: 300}, now)
	if !r.Firing || r.Value != 250 {
		t.Fatalf("metric: %+v", r)
	}
	r, _ = Evaluate(ctx, st, store.Alarm{Name: "q", Kind: store.AlarmMetric, Metric: "missing", Agg: "avg", Op: "<", Threshold: 1, WindowS: 300}, now)
	if r.Firing || !r.NoData {
		t.Fatalf("no data should not fire: %+v", r)
	}
	if err := Validate(store.Alarm{Name: "x", Kind: store.AlarmMetric, Agg: "avg", Op: ">", WindowS: 60}); err == nil {
		t.Error("metric alarm without metric should be invalid")
	}
}
