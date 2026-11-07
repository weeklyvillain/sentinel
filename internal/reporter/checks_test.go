package reporter

import (
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/notify"
	"github.com/weeklyvillain/sentinel/internal/store"
)

func TestCheckOutageLifecycle(t *testing.T) {
	h := newHarness(t, Config{SettleWindow: time.Hour, MinEvents: 50, DownAfter: 0})
	n := &recorder{}
	h.r.Notify = n
	c := store.Check{ID: 7, Name: "public api", URL: "https://api.example.com/healthz", Service: "api"}
	down := func() {
		c.DownSince = h.now
		h.r.CheckDown(h.ctx, c, store.CheckResult{Time: h.now, Error: "dial tcp: connection refused"})
	}
	up := func() {
		h.r.CheckUp(h.ctx, c, c.DownSince, store.CheckResult{Time: h.now, OK: true, Status: 200, LatencyMS: 42})
	}

	down()
	h.r.Tick(h.ctx) // outages skip the settle window and MIN_EVENTS
	if h.gh.created != 1 {
		t.Fatalf("want outage issue, got %d", h.gh.created)
	}
	iss := h.gh.issues[1]
	for _, want := range []string{"public api", "https://api.example.com/healthz", "connection refused"} {
		if !strings.Contains(iss.Title+iss.body, want) {
			t.Errorf("issue missing %q:\n%s\n%s", want, iss.Title, iss.body)
		}
	}
	if strings.Contains(iss.body, "No telemetry received") {
		t.Error("check outage described as telemetry silence")
	}

	h.advance(7 * time.Minute)
	up()
	if iss.State != "closed" || len(iss.comments) != 1 || !strings.Contains(iss.comments[0], "Outage lasted about **7m0s**") {
		t.Fatalf("recovery: state=%s comments=%v", iss.State, iss.comments)
	}

	h.advance(time.Hour)
	down()
	h.r.Tick(h.ctx)
	if h.gh.created != 1 || h.gh.reopens != 1 || iss.State != "open" || !strings.Contains(iss.comments[1], "Down again") {
		t.Fatalf("second outage should reopen the same issue: created=%d reopens=%d comments=%v", h.gh.created, h.gh.reopens, iss.comments)
	}
	var types []string
	for _, e := range n.events {
		types = append(types, e.Type)
	}
	want := []string{notify.CheckDown, notify.IssueCreated, notify.CheckRecovered, notify.CheckDown, notify.IssueRegression}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Errorf("notifications: %v want %v", types, want)
	}
}

func TestCheckRecoveredBeforeFiling(t *testing.T) {
	h := newHarness(t, Config{DownAfter: 0, MaxIssuesPerHour: 1})
	// Use up the hourly cap so the outage stays pending.
	h.emit(1, "boom %d")
	h.r.Tick(h.ctx)
	c := store.Check{ID: 1, Name: "web", URL: "https://example.com"}
	h.r.CheckDown(h.ctx, c, store.CheckResult{Time: h.now, Error: "timeout"})
	h.r.Tick(h.ctx)
	h.r.CheckUp(h.ctx, c, h.now, store.CheckResult{Time: h.now.Add(time.Minute), OK: true})
	g, err := h.st.GetGroup(h.ctx, CheckFingerprint(c))
	if err != nil || g.Status != store.StatusResolved || g.IssueNumber != 0 {
		t.Fatalf("pending outage should resolve quietly: %+v %v", g, err)
	}
}
