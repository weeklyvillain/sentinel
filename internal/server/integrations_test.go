package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestSentryProjects(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "")
	h.s.PublicURL = "https://sentinel.example.com"
	type view struct {
		Sentry []struct {
			ID      int    `json:"id"`
			Key     string `json:"key"`
			Service string `json:"service"`
			DSN     string `json:"dsn"`
		} `json:"sentry"`
	}
	add := func(body string) (int, view) {
		resp, b := h.do(http.MethodPost, "/api/integrations/sentry", body, true)
		var v view
		json.Unmarshal([]byte(b), &v)
		return resp.StatusCode, v
	}
	if code, _ := add(`{"service":""}`); code != http.StatusBadRequest {
		t.Fatalf("no service: %d", code)
	}
	code, v := add(`{"service":"shop","environment":"production"}`)
	if code != http.StatusOK || len(v.Sentry) != 1 {
		t.Fatalf("add: %d %+v", code, v)
	}
	p := v.Sentry[0]
	if p.ID != 1 || len(p.Key) != 32 || p.DSN != "https://"+p.Key+"@sentinel.example.com/sentry/1" {
		t.Fatalf("project: %+v", p)
	}
	_, v = add(`{"service":"api"}`)
	if len(v.Sentry) != 2 || v.Sentry[1].ID != 2 {
		t.Fatalf("second: %+v", v)
	}
	got, ok := h.s.SentryLookup("1")
	if !ok || got.Service != "shop" || got.Key != p.Key || got.Environment != "production" {
		t.Fatalf("lookup: %+v %v", got, ok)
	}
	if _, ok := h.s.SentryLookup("nope"); ok {
		t.Fatal("non-numeric id found")
	}
	// A new key invalidates the old DSN.
	_, b := h.do(http.MethodPost, "/api/integrations/sentry/1/rotate", "", true)
	if strings.Contains(b, p.Key) {
		t.Fatal("key not replaced")
	}
	if got, _ := h.s.SentryLookup("1"); got.Key == p.Key {
		t.Fatal("lookup still has the old key")
	}
	// Projects survive a restart; deleting one stops it.
	h.s.integrations.sentry = nil
	h.s.LoadIntegrations(t.Context())
	if _, ok := h.s.SentryLookup("2"); !ok {
		t.Fatal("not reloaded")
	}
	h.do(http.MethodDelete, "/api/integrations/sentry/2", "", true)
	if _, ok := h.s.SentryLookup("2"); ok {
		t.Fatal("deleted project still accepted")
	}
	if resp, _ := h.do(http.MethodDelete, "/api/integrations/sentry/99", "", true); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown: %d", resp.StatusCode)
	}
}

func TestClientKeyOrigins(t *testing.T) {
	h := newHarness(t, AuthConfig{Mode: "none"}, "")
	h.do(http.MethodPost, "/api/integrations/sentry", `{"service":"web"}`, true)
	if resp, body := h.do(http.MethodPut, "/api/integrations/sentry/1", `{"origins":["https://shop.example.com/","https://*.preview.example.com",""],"environment":"prod"}`, true); resp.StatusCode != http.StatusOK ||
		!strings.Contains(body, `"origins":["https://shop.example.com","https://*.preview.example.com"]`) {
		t.Fatalf("set: %d %s", resp.StatusCode, body)
	}
	if p, _ := h.s.SentryLookup("1"); len(p.Origins) != 2 || p.Environment != "prod" {
		t.Fatalf("lookup: %+v", p)
	}
	for _, bad := range []string{`shop.example.com`, `ftp://x.com`, `https://x.com/path`} {
		if resp, _ := h.do(http.MethodPut, "/api/integrations/sentry/1", `{"origins":["`+bad+`"]}`, true); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%q accepted", bad)
		}
	}
	h.do(http.MethodPut, "/api/integrations/sentry/1", `{"origins":[]}`, true)
	if p, _ := h.s.SentryLookup("1"); len(p.Origins) != 0 || p.Environment != "prod" {
		t.Fatalf("cleared: %+v", p)
	}
}
