package alarm

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

func TestForPendingSilenceAndGrouping(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now().UTC()
	n := &rec{}
	ev := &Evaluator{Store: st, Notify: n, Now: func() time.Time { return now }}
	mk := func(name, svc string, forS int64) int64 {
		id, err := st.SaveAlarm(ctx, store.Alarm{Name: name, Enabled: true, Kind: store.AlarmLogCount, Service: svc, MinSeverity: 17,
			Op: ">", Threshold: 0, WindowS: 3600, ForS: forS, WebhookIDs: []int64{1}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	slow := mk("slow", "api", 120)
	mk("a", "api", 0)
	mk("b", "api", 0)
	st.InsertLogs(ctx, []store.LogRecord{{Time: now.Add(-time.Minute), Service: "api", Severity: 17, Body: "x"}})

	ev.Tick(ctx)
	if len(n.events) != 1 || len(n.events[0].Alarms) != 2 || !strings.Contains(n.events[0].Message, "2 alarms") {
		t.Fatalf("a and b should be one grouped notification: %+v", n.events)
	}
	if a, _ := st.GetAlarm(ctx, slow); a.State != store.AlarmPending {
		t.Fatalf("slow should be pending, is %s", a.State)
	}
	now = now.Add(3 * time.Minute)
	ev.Tick(ctx)
	if a, _ := st.GetAlarm(ctx, slow); a.State != store.AlarmFiring || len(n.events) != 2 || n.events[1].Alarm.Name != "slow" {
		t.Fatalf("slow should fire after its for: %s %+v", a.State, n.events)
	}

	// Silenced: state changes, nobody is told.
	st.SaveSilence(ctx, store.Silence{Service: "api", StartsAt: now.Add(-time.Minute), EndsAt: now.Add(2 * time.Hour)})
	now = now.Add(2 * time.Hour) // logs leave the window... and the silence ends exactly now
	now = now.Add(-time.Second)
	ev.Tick(ctx)
	if len(n.events) != 2 {
		t.Fatalf("silenced resolutions should not notify: %+v", n.events[2:])
	}
}

func TestNextState(t *testing.T) {
	now := time.Now()
	a := store.Alarm{ForS: 60, State: store.AlarmOK}
	if NextState(a, true, now) != store.AlarmPending {
		t.Fatal("first true with for: pending")
	}
	a.PendingSince = now.Add(-2 * time.Minute)
	if NextState(a, true, now) != store.AlarmFiring {
		t.Fatal("held long enough: firing")
	}
	if NextState(a, false, now) != store.AlarmOK {
		t.Fatal("false: ok")
	}
}

func TestSLOBurn(t *testing.T) {
	ctx := context.Background()
	st, _ := store.Open(filepath.Join(t.TempDir(), "a.db"))
	defer st.Close()
	now := time.Now().UTC().Truncate(time.Minute)
	var spans []store.Span
	for i := 0; i < 200; i++ {
		status := store.SpanOK
		if i%4 == 0 { // 25% errors in the last minutes
			status = store.SpanError
		}
		spans = append(spans, store.Span{TraceID: "t", SpanID: "s", Service: "api", Name: "GET /", Kind: "server",
			Start: now.Add(-time.Duration(i%4) * time.Minute), DurationUS: 1000, Status: status})
	}
	st.InsertSpans(ctx, spans)
	o := store.SLO{Name: "api avail", Kind: store.SLOAvailability, Service: "api", Objective: 99, WindowDays: 30}
	if err := ValidateSLO(o); err != nil {
		t.Fatal(err)
	}
	s, err := EvaluateSLO(ctx, st, o, now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if s.Total != 200 || s.SLI != 75 || s.BudgetRemaining >= 0 || SLOState(s) != store.SLOFastBurn {
		t.Fatalf("slo: %+v state %s", s, SLOState(s))
	}
	_ = notify.SLOBurn
}
