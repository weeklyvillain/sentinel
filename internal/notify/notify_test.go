package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

type receiver struct {
	mu     sync.Mutex
	bodies []string
	heads  []http.Header
	fail   int // fail the first N requests with 503
}

func (rc *receiver) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.bodies = append(rc.bodies, string(b))
	rc.heads = append(rc.heads, r.Header.Clone())
	if rc.fail > 0 {
		rc.fail--
		w.WriteHeader(503)
	}
}

func setup(t *testing.T) (*store.Store, *Dispatcher) {
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, &Dispatcher{Store: st, Backoff: []time.Duration{time.Millisecond, time.Millisecond}}
}

func TestRoutingSigningAndRetry(t *testing.T) {
	ctx := context.Background()
	st, d := setup(t)
	issues, alarms, slack := &receiver{fail: 1}, &receiver{}, &receiver{}
	for _, rc := range []*receiver{issues, alarms, slack} {
		srv := httptest.NewServer(rc)
		t.Cleanup(srv.Close)
		rc.mu.Lock()
		rc.bodies = nil
		rc.mu.Unlock()
		defer func(u string) { _ = u }(srv.URL)
		switch rc {
		case issues:
			st.SaveWebhook(ctx, store.Webhook{Name: "issues", URL: srv.URL, Secret: "s3cret", Format: "json", Events: []string{IssueCreated}, Enabled: true})
		case alarms:
			st.SaveWebhook(ctx, store.Webhook{Name: "alarms", URL: srv.URL, Format: "json", Events: []string{}, Enabled: true})
		case slack:
			st.SaveWebhook(ctx, store.Webhook{Name: "slack", URL: srv.URL, Format: "slack", Events: []string{"*"}, Enabled: true})
		}
	}
	d.Notify(Event{Type: IssueCreated, Service: "api", Message: "New issue for api",
		Issue: &IssueInfo{Repo: "acme/api", Number: 7, URL: "https://github.com/acme/api/issues/7"}})
	d.Notify(Event{Type: AlarmFiring, Message: "errors > 10", WebhookIDs: []int64{2}})
	d.Drain(ctx)

	if len(issues.bodies) != 2 { // one 503, one retry
		t.Fatalf("issues hook: want 2 attempts, got %d", len(issues.bodies))
	}
	var ev map[string]any
	json.Unmarshal([]byte(issues.bodies[1]), &ev)
	if ev["event"] != IssueCreated || ev["issue"].(map[string]any)["number"].(float64) != 7 {
		t.Errorf("payload: %v", ev)
	}
	if got := issues.heads[1].Get("X-Sentinel-Signature"); got != "sha256="+Sign("s3cret", []byte(issues.bodies[1])) {
		t.Errorf("signature %q", got)
	}
	if issues.heads[1].Get("X-Sentinel-Event") != IssueCreated {
		t.Error("missing event header")
	}
	if len(alarms.bodies) != 1 || !strings.Contains(alarms.bodies[0], AlarmFiring) {
		t.Errorf("alarm hook got %v", alarms.bodies)
	}
	// "*" subscribes to routed events but not to alarms targeted elsewhere.
	if len(slack.bodies) != 1 || !strings.Contains(slack.bodies[0], `"text"`) || !strings.Contains(slack.bodies[0], "<https://github.com/acme/api/issues/7|acme/api#7>") {
		t.Errorf("slack hook got %v", slack.bodies)
	}
	dels, _ := st.ListDeliveries(ctx, 1, 10)
	if len(dels) != 1 || dels[0].Status != 200 || dels[0].Attempt != 2 {
		t.Errorf("deliveries: %+v", dels)
	}
}

func TestDisabledAndPermanentFailure(t *testing.T) {
	ctx := context.Background()
	st, d := setup(t)
	rc := &receiver{fail: 100}
	srv := httptest.NewServer(rc)
	defer srv.Close()
	st.SaveWebhook(ctx, store.Webhook{Name: "off", URL: srv.URL, Events: []string{"*"}, Enabled: false})
	st.SaveWebhook(ctx, store.Webhook{Name: "broken", URL: srv.URL, Events: []string{"*"}, Enabled: true})
	d.Notify(Event{Type: ServiceDown, Message: "x"})
	d.Drain(ctx)
	if len(rc.bodies) != 3 {
		t.Fatalf("want 3 attempts to the enabled hook only, got %d", len(rc.bodies))
	}
	dels, _ := st.ListDeliveries(ctx, 2, 10)
	if len(dels) != 1 || dels[0].Error != "HTTP 503" || dels[0].Attempt != 3 {
		t.Errorf("deliveries: %+v", dels)
	}
}

func TestDiscordPayload(t *testing.T) {
	b, err := Payload("discord", Event{Type: ServiceRecovered, Message: "api is back"})
	if err != nil || !strings.Contains(string(b), `"content"`) || !strings.Contains(string(b), "api is back") {
		t.Fatalf("%s %v", b, err)
	}
	if _, err := Payload("xml", Event{}); err == nil {
		t.Error("unknown format should error")
	}
}
