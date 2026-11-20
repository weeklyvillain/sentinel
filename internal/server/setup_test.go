package server

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
)

func TestSetupGuide(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "")
	get := func() (map[string]SetupStep, int) {
		resp, body := h.do(http.MethodGet, "/api/setup", "", false)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("setup: %d %s", resp.StatusCode, body)
		}
		var out struct {
			Steps     []SetupStep `json:"steps"`
			Remaining int         `json:"remaining"`
		}
		json.Unmarshal([]byte(body), &out)
		m := map[string]SetupStep{}
		for _, s := range out.Steps {
			m[s.ID] = s
		}
		return m, out.Remaining
	}
	steps, before := get()
	if steps["signin"].State != stepWarn || steps["telemetry"].State != stepTodo || steps["github"].State != stepTodo || steps["fixer"].State != stepTodo {
		t.Fatalf("fresh install: %+v", steps)
	}
	// Telemetry arrives: that step is done, the service still needs a repository.
	h.st.RecordEvent(context.Background(), "fp", event.Event{Kind: event.KindLog, Service: "checkout", Severity: "ERROR", Message: "x", Time: time.Now()})
	h.st.TouchService(context.Background(), "checkout", "prod", time.Now())
	steps, after := get()
	if steps["telemetry"].State != stepDone || steps["services"].State != stepTodo || after != before-1 {
		t.Fatalf("after telemetry (%d → %d): %+v %+v", before, after, steps["telemetry"], steps["services"])
	}
	// Skipping takes a step off the count; undoing puts it back.
	if resp, _ := h.do(http.MethodPost, "/api/setup/skip", `{"id":"uptime","skip":true}`, true); resp.StatusCode != http.StatusOK {
		t.Fatalf("skip: %d", resp.StatusCode)
	}
	if steps, n := get(); !steps["uptime"].Skipped || n != after-1 {
		t.Fatalf("skipped: %+v, %d remaining", steps["uptime"], n)
	}
	h.do(http.MethodPost, "/api/setup/skip", `{"id":"uptime","skip":false}`, true)
	if steps, n := get(); steps["uptime"].Skipped || n != after {
		t.Fatalf("unskipped: %+v, %d remaining", steps["uptime"], n)
	}
}
