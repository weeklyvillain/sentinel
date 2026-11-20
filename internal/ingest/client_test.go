package ingest

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/weeklyvillain/sentinel/internal/store"
)

func clientServer(t *testing.T, origins ...string) (*httptest.Server, *sink) {
	s := &sink{}
	mux := http.NewServeMux()
	projects := map[string]SentryProject{"7": {ID: "7", Key: "pubkey", Service: "web", Environment: "production", Origins: origins}}
	(&Receiver{Sink: s, Now: func() time.Time { return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC) }}).RegisterClient(mux, func(id string) (SentryProject, bool) {
		p, ok := projects[id]
		return p, ok
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func TestClientJSONLogs(t *testing.T) {
	srv, s := clientServer(t)
	url := srv.URL + "/ingest/7/pubkey/logs"
	// One object, as fetch or sendBeacon would send it (text/plain).
	if code, body := post(t, url, []byte(`{"msg":"cart opened","level":"info","ts":1763640000123,"user":{"id":7},"page":"/cart"}`),
		map[string]string{"Content-Type": "text/plain;charset=UTF-8"}); code != 202 {
		t.Fatalf("object: %d %s", code, body)
	}
	l := s.logs[0]
	if l.Body != "cart opened" || l.Severity != 9 || l.Service != "web" || l.Env != "production" || l.Attrs["user.id"] != "7" || l.Attrs["page"] != "/cart" ||
		!l.Time.Equal(time.UnixMilli(1763640000123).UTC()) {
		t.Fatalf("log: %+v", l)
	}
	// An array and one object per line.
	post(t, url, []byte(`[{"message":"a","level":"debug"},{"message":"b","level":"warn","env":"staging"}]`), nil)
	post(t, url, []byte("{\"message\":\"c\"}\n{\"message\":\"d\",\"timestamp\":\"2026-11-20T11:00:00Z\"}\n"), nil)
	if len(s.logs) != 5 || s.logs[2].Env != "staging" || s.logs[2].Severity != 13 || s.logs[4].Time.Hour() != 11 {
		t.Fatalf("batches: %+v", s.logs)
	}
	// Errors: an error object with a stack becomes an issue.
	post(t, url, []byte(`{"message":"checkout failed","level":"error","error":{"name":"TypeError","message":"x is undefined","stack":"TypeError: x is undefined\n    at pay (https://shop/app.js:10:5)\n    at onClick (https://shop/app.js:3:1)"}}`), nil)
	if len(s.events) != 1 || s.events[0].ExcType != "TypeError" || !strings.Contains(s.events[0].Stacktrace, "at pay") {
		t.Fatalf("error: %+v", s.events)
	}
	if code, _ := post(t, url, []byte(`not json`), nil); code != 400 {
		t.Fatalf("bad body: %d", code)
	}
	if code, _ := post(t, srv.URL+"/ingest/7/wrong/logs", []byte(`{"msg":"x"}`), nil); code != 401 {
		t.Fatalf("wrong key: %d", code)
	}
}

// What @datadog/browser-logs sends to its proxy: one JSON object per line,
// with ?ddforward=… on the URL.
func TestClientDatadogBrowserLogs(t *testing.T) {
	srv, s := clientServer(t)
	body := `{"date":1763640000500,"message":"Button clicked","status":"info","service":"web","origin":"logger","view":{"url":"https://shop.example.com/cart?x=1","referrer":""},"session_id":"s1","usr":{"id":"u9"}}
{"date":1763640001000,"message":"Uncaught TypeError: Cannot read properties of undefined","status":"error","origin":"source","error":{"kind":"TypeError","message":"Cannot read properties of undefined","stack":"TypeError: Cannot read properties of undefined\n    at render (https://shop.example.com/app.js:42:7)"},"view":{"url":"https://shop.example.com/cart"}}`
	code, resp := post(t, srv.URL+"/ingest/7/pubkey/datadog?ddforward=%2Fapi%2Fv2%2Flogs%3Fddsource%3Dbrowser", []byte(body), map[string]string{"Content-Type": "text/plain;charset=UTF-8"})
	if code != 202 {
		t.Fatalf("datadog: %d %s", code, resp)
	}
	if len(s.logs) != 2 || s.logs[0].Body != "Button clicked" || s.logs[0].Attrs["usr.id"] != "u9" || s.logs[0].Attrs["view.url"] == "" {
		t.Fatalf("logs: %+v", s.logs)
	}
	if len(s.events) != 1 || s.events[0].ExcType != "TypeError" || !strings.Contains(s.events[0].Stacktrace, "at render") {
		t.Fatalf("error: %+v", s.events)
	}
}

// A Grafana Faro payload: logs, an exception, Web Vitals and an event.
func TestClientFaro(t *testing.T) {
	m := &metricRecorder{}
	mux := http.NewServeMux()
	(&Receiver{Sink: m}).RegisterClient(mux, func(id string) (SentryProject, bool) {
		return SentryProject{ID: "7", Key: "pubkey", Service: "web"}, id == "7"
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	body := `{"meta":{"sdk":{"name":"@grafana/faro-web-sdk","version":"1.12.0"},"app":{"name":"shop-frontend","version":"3.1.0","environment":"production"},
"session":{"id":"sess-1"},"user":{"id":"u1"},"page":{"url":"https://shop.example.com/checkout?token=secret"},"browser":{"name":"Chrome","version":"131","os":"Mac OS","mobile":false}},
"logs":[{"message":"checkout opened","level":"info","timestamp":"2026-11-20T11:59:58.000Z","context":{"step":"1"}}],
"exceptions":[{"type":"Error","value":"payment form missing","timestamp":"2026-11-20T11:59:59.000Z","stacktrace":{"frames":[{"filename":"https://shop.example.com/assets/checkout.js","function":"mountForm","lineno":120,"colno":9},{"filename":"https://shop.example.com/assets/vendor.js","function":"render","lineno":5,"colno":1}]}}],
"measurements":[{"type":"web-vitals","values":{"lcp":2350.5,"cls":0.08},"timestamp":"2026-11-20T11:59:59.500Z"}],
"events":[{"name":"session_start","domain":"browser","attributes":{},"timestamp":"2026-11-20T11:59:57.000Z"}]}`
	code, resp := post(t, srv.URL+"/ingest/7/pubkey/faro", []byte(body), map[string]string{"Content-Type": "application/json", "Origin": "https://shop.example.com"})
	if code != 202 {
		t.Fatalf("faro: %d %s", code, resp)
	}
	if len(m.logs) != 3 || m.logs[0].Body != "checkout opened" || m.logs[0].Attrs["context.step"] != "1" || m.logs[0].Attrs["page.path"] != "/checkout" ||
		m.logs[0].Env != "production" || m.logs[2].Severity != 5 {
		t.Fatalf("logs: %+v", m.logs)
	}
	for _, l := range m.logs {
		for _, v := range l.Attrs {
			if strings.Contains(v, "secret") {
				t.Fatalf("query string kept: %+v", l.Attrs)
			}
		}
	}
	if len(m.events) != 1 || m.events[0].ExcMessage != "payment form missing" || m.events[0].Release != "3.1.0" ||
		!strings.HasPrefix(m.events[0].Stacktrace, "at mountForm (https://shop.example.com/assets/checkout.js:120:9)") {
		t.Fatalf("exception: %+v", m.events)
	}
	if len(m.points) != 2 || m.points[1].Name != "web_vitals.lcp" || m.points[1].Value != 2350.5 || m.points[1].Unit != "ms" || m.points[1].Attrs != "page=/checkout" {
		t.Fatalf("metrics: %+v", m.points)
	}
}

func TestClientOrigins(t *testing.T) {
	srv, s := clientServer(t, "https://shop.example.com", "https://*.preview.example.com")
	url := srv.URL + "/ingest/7/pubkey/logs"
	for origin, want := range map[string]int{
		"https://shop.example.com":          202,
		"https://pr-12.preview.example.com": 202,
		"https://evil.example.net":          403,
		"":                                  202, // servers send no Origin
	} {
		h := map[string]string{}
		if origin != "" {
			h["Origin"] = origin
		}
		if code, _ := post(t, url, []byte(`{"msg":"x"}`), h); code != want {
			t.Errorf("%q: %d, want %d", origin, code, want)
		}
	}
	if len(s.logs) != 3 {
		t.Fatalf("logs: %d", len(s.logs))
	}
	req, _ := http.NewRequest(http.MethodOptions, url, nil)
	req.Header.Set("Origin", "https://shop.example.com")
	req.Header.Set("Access-Control-Request-Headers", "content-type,idempotency-key")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "https://shop.example.com" ||
		!strings.Contains(resp.Header.Get("Access-Control-Allow-Headers"), "idempotency-key") {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
}

type metricRecorder struct {
	sink
	points []store.MetricPoint
}

func (m *metricRecorder) Metrics(_ context.Context, pts []store.MetricPoint) {
	m.points = append(m.points, pts...)
}

// Faro's tracing sends OTLP JSON spans in the payload: they are stored
// under the key's service with the session and user.
func TestClientFaroTraces(t *testing.T) {
	s := &spanSink{}
	mux := http.NewServeMux()
	(&Receiver{Sink: s, SpanSampleRate: 1}).RegisterClient(mux, func(id string) (SentryProject, bool) {
		return SentryProject{ID: "7", Key: "pubkey", Service: "web"}, id == "7"
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	body := `{"meta":{"app":{"name":"shop-frontend","version":"3.1.0","environment":"production"},"session":{"id":"sess-9"},"user":{"id":"u1"}},
"traces":{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"shop-frontend"}}]},
"scopeSpans":[{"scope":{"name":"@opentelemetry/instrumentation-fetch"},"spans":[
{"traceId":"4bf92f3577b34da6a3ce929d0e0e4736","spanId":"00f067aa0ba902b7","name":"HTTP POST","kind":3,"startTimeUnixNano":"1763640000000000000","endTimeUnixNano":"1763640000120000000",
"attributes":[{"key":"http.url","value":{"stringValue":"https://api.example.com/checkout"}}],"status":{}}]}]}]}}`
	if code, resp := post(t, srv.URL+"/ingest/7/pubkey/faro", []byte(body), nil); code != 202 {
		t.Fatalf("faro: %d %s", code, resp)
	}
	if len(s.spans) != 1 {
		t.Fatalf("spans: %+v", s.spans)
	}
	sp := s.spans[0]
	if sp.Service != "web" || sp.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || sp.Kind != "client" || sp.DurationUS != 120000 ||
		sp.Env != "production" || sp.Version != "3.1.0" || sp.Attrs["http.url"] == "" {
		t.Fatalf("span: %+v", sp)
	}
}

// JSON logs join their trace: trace ids are normalized, and a W3C
// traceparent field works too.
func TestClientLogsTraceIDs(t *testing.T) {
	srv, s := clientServer(t)
	url := srv.URL + "/ingest/7/pubkey/logs"
	post(t, url, []byte(`{"msg":"a","trace_id":"4BF92F35-77B3-4DA6-A3CE-929D0E0E4736","span_id":"00F067AA0BA902B7"}`), nil)
	post(t, url, []byte(`{"msg":"b","traceparent":"00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}`), nil)
	post(t, url, []byte(`{"msg":"c","trace_id":"not-a-trace"}`), nil)
	if s.logs[0].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || s.logs[0].SpanID != "00f067aa0ba902b7" ||
		s.logs[1].TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || s.logs[1].SpanID != "00f067aa0ba902b7" || s.logs[2].TraceID != "" {
		t.Fatalf("trace ids: %q %q %q", s.logs[0].TraceID, s.logs[1].TraceID, s.logs[2].TraceID)
	}
}
