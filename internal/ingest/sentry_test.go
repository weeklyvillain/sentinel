package ingest

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/fingerprint"
)

func sentryServer(t *testing.T) (*httptest.Server, *spanSink) {
	s := &spanSink{}
	mux := http.NewServeMux()
	projects := map[string]SentryProject{"42": {ID: "42", Key: "abc123", Service: "shop", Environment: "production"}}
	(&Receiver{Sink: s, SpanSampleRate: 1}).RegisterSentry(mux, func(id string) (SentryProject, bool) {
		p, ok := projects[id]
		return p, ok
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

func post(t *testing.T, url string, body []byte, headers map[string]string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

const auth = "Sentry sentry_version=7, sentry_client=sentry.python/2.19.0, sentry_key=abc123"

// An envelope as the Python SDK sends it: an error event with a stack
// trace, tags, user, release and trace context, plus a session item, gzip
// compressed.
const pythonEnvelope = `{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","sent_at":"2026-11-20T12:00:01.000Z","sdk":{"name":"sentry.python","version":"2.19.0"}}
{"type":"event","content_type":"application/json"}
{"event_id":"9ec79c33ec9942ab8353589fcb2e04dc","timestamp":"2026-11-20T12:00:00.123456Z","level":"error","platform":"python","release":"shop@1.4.2","environment":"staging","server_name":"web-1","transaction":"/checkout","tags":{"region":"eu"},"user":{"id":"7","email":"a@example.com"},"contexts":{"trace":{"trace_id":"4bf92f3577b34da6a3ce929d0e0e4736","span_id":"00f067aa0ba902b7","op":"http.server"}},"exception":{"values":[{"type":"KeyError","value":"'sku'","module":null,"stacktrace":{"frames":[{"filename":"django/core/handlers/base.py","function":"_get_response","lineno":197,"in_app":false},{"filename":"shop/views.py","function":"checkout","lineno":42,"in_app":true},{"filename":"shop/cart.py","function":"price","lineno":17,"in_app":true}]}}]}}
{"type":"session","length":70}
{"sid":"1","init":true,"started":"2026-11-20T12:00:00Z","status":"ok"}
`

func gz(s string) []byte {
	var b bytes.Buffer
	w := gzip.NewWriter(&b)
	w.Write([]byte(s))
	w.Close()
	return b.Bytes()
}

func TestSentryPythonEnvelope(t *testing.T) {
	srv, s := sentryServer(t)
	code, body := post(t, srv.URL+"/sentry/api/42/envelope/", gz(pythonEnvelope), map[string]string{"X-Sentry-Auth": auth, "Content-Encoding": "gzip",
		"Content-Type": "application/x-sentry-envelope"})
	if code != 200 || !strings.Contains(body, "9ec79c33ec9942ab8353589fcb2e04dc") {
		t.Fatalf("envelope: %d %s", code, body)
	}
	if len(s.events) != 1 || len(s.logs) != 1 {
		t.Fatalf("events %d, logs %d", len(s.events), len(s.logs))
	}
	e := s.events[0]
	if e.Service != "shop" || e.Environment != "staging" || e.Release != "shop@1.4.2" || e.ExcType != "KeyError" || e.ExcMessage != "'sku'" ||
		e.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || e.Attributes["region"] != "eu" || e.Attributes["user.email"] != "a@example.com" {
		t.Fatalf("event: %+v", e)
	}
	// Newest frame first, only in-app frames.
	if e.Stacktrace != "at price (shop/cart.py:17)\nat checkout (shop/views.py:42)" {
		t.Fatalf("stack: %q", e.Stacktrace)
	}
	if f := fingerprint.Frames(e.Stacktrace); len(f) != 2 {
		t.Fatalf("grouping does not read the frames: %v", f)
	}
	if s.beats[0] != "shop/staging" {
		t.Fatalf("heartbeat: %v", s.beats)
	}
}

// The JavaScript SDK without compression, and the older /store/ endpoint
// with the key in the query string.
func TestSentryStoreAndMessages(t *testing.T) {
	srv, s := sentryServer(t)
	ev := `{"event_id":"a1","timestamp":1763640000.5,"level":"info","platform":"javascript","message":{"formatted":"user clicked buy"},"fingerprint":["{{ default }}"]}`
	if code, body := post(t, srv.URL+"/sentry/api/42/store/?sentry_key=abc123&sentry_version=7", []byte(ev), nil); code != 200 {
		t.Fatalf("store: %d %s", code, body)
	}
	if len(s.logs) != 1 || len(s.events) != 0 || s.logs[0].Body != "user clicked buy" || s.logs[0].Severity != 9 || s.logs[0].Env != "production" {
		t.Fatalf("info message is a log line, not an error: logs %+v events %d", s.logs, len(s.events))
	}
	// A custom fingerprint groups explicitly.
	ev = `{"event_id":"a2","level":"error","message":"payment declined","fingerprint":["payments","declined"]}`
	post(t, srv.URL+"/sentry/api/42/store/?sentry_key=abc123", []byte(ev), nil)
	if len(s.events) != 1 || s.events[0].FingerprintOverride() != "payments|declined" {
		t.Fatalf("fingerprint: %+v", s.events)
	}
}

// A broken item later in the envelope does not lose the ones before it.
func TestSentryBrokenItemKeepsEarlierOnes(t *testing.T) {
	srv, s := sentryServer(t)
	env := `{"event_id":"d1"}
{"type":"event"}
{"level":"error","message":"kept"}
not json
`
	if code, body := post(t, srv.URL+"/sentry/api/42/envelope/", []byte(env), map[string]string{"X-Sentry-Auth": auth}); code != 200 || len(s.events) != 1 {
		t.Fatalf("%d %s, events %d", code, body, len(s.events))
	}
}

func TestSentryAuth(t *testing.T) {
	srv, s := sentryServer(t)
	env := "{\"event_id\":\"b1\"}\n{\"type\":\"event\"}\n{\"level\":\"error\",\"message\":\"x\"}\n"
	if code, _ := post(t, srv.URL+"/sentry/api/42/envelope/", []byte(env), map[string]string{"X-Sentry-Auth": "Sentry sentry_key=wrong"}); code != 401 {
		t.Fatalf("wrong key: %d", code)
	}
	if code, _ := post(t, srv.URL+"/sentry/api/99/envelope/", []byte(env), map[string]string{"X-Sentry-Auth": auth}); code != 401 {
		t.Fatalf("unknown project: %d", code)
	}
	// Browser SDKs may put the DSN in the envelope header instead.
	withDSN := "{\"event_id\":\"b2\",\"dsn\":\"https://abc123@sentinel.example.com/sentry/42\"}\n{\"type\":\"event\"}\n{\"level\":\"error\",\"message\":\"y\"}\n"
	if code, body := post(t, srv.URL+"/sentry/api/42/envelope/", []byte(withDSN), nil); code != 200 {
		t.Fatalf("dsn in header: %d %s", code, body)
	}
	if len(s.events) != 1 {
		t.Fatalf("events: %d", len(s.events))
	}
	if code, _ := post(t, srv.URL+"/sentry/api/42/envelope/", []byte(env), map[string]string{"X-Sentry-Auth": auth, "Content-Encoding": "br"}); code != 415 {
		t.Fatalf("brotli: %d", code)
	}
	// CORS preflight for browser SDKs.
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/sentry/api/42/envelope/", nil)
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: %d %v", resp.StatusCode, resp.Header)
	}
}

// Transactions become spans; Sentry logs become log lines (errors too).
func TestSentryTransactionsAndLogs(t *testing.T) {
	srv, s := sentryServer(t)
	tx := `{"type":"transaction","event_id":"c1","transaction":"GET /api/cart","start_timestamp":1763640000.0,"timestamp":1763640000.25,"release":"shop@1.4.2",
"contexts":{"trace":{"trace_id":"5bf92f3577b34da6a3ce929d0e0e4736","span_id":"10f067aa0ba902b7","op":"http.server","status":"ok"}},
"spans":[{"span_id":"20f067aa0ba902b7","parent_span_id":"10f067aa0ba902b7","op":"db.query","description":"SELECT * FROM carts WHERE id = $1","start_timestamp":1763640000.01,"timestamp":1763640000.2,"status":"internal_error"}]}`
	logs := `{"items":[{"timestamp":1763640001.0,"level":"info","body":"cart loaded","trace_id":"5bf92f3577b34da6a3ce929d0e0e4736","attributes":{"cart.items":{"value":3,"type":"integer"},"sentry.environment":{"value":"staging","type":"string"}}},
{"timestamp":1763640002.0,"level":"error","body":"cart write failed","attributes":{}}]}`
	env := "{\"event_id\":\"c1\"}\n{\"type\":\"transaction\",\"length\":" + strconv.Itoa(len(tx)) + "}\n" + tx + "\n{\"type\":\"log\",\"item_count\":2,\"content_type\":\"application/vnd.sentry.items.log+json\"}\n" +
		strings.ReplaceAll(logs, "\n", "") + "\n"
	if code, body := post(t, srv.URL+"/sentry/api/42/envelope/", []byte(env), map[string]string{"X-Sentry-Auth": auth}); code != 200 {
		t.Fatalf("envelope: %d %s", code, body)
	}
	if len(s.spans) != 2 {
		t.Fatalf("spans: %+v", s.spans)
	}
	root, child := s.spans[0], s.spans[1]
	if root.Name != "GET /api/cart" || root.Kind != "server" || root.DurationUS != 250000 || root.ParentID != "" || root.Version != "shop@1.4.2" {
		t.Fatalf("root: %+v", root)
	}
	if child.Kind != "client" || child.Status != 2 || child.ParentID != root.SpanID || child.TraceID != root.TraceID || child.DurationUS < 189000 || child.DurationUS > 191000 {
		t.Fatalf("child: %+v", child)
	}
	if len(s.logs) != 2 || s.logs[0].Body != "cart loaded" || s.logs[0].Attrs["cart.items"] != "3" || s.logs[0].Env != "staging" {
		t.Fatalf("logs: %+v", s.logs)
	}
	if len(s.events) != 1 || s.events[0].Message != "cart write failed" || s.events[0].Kind != event.KindLog {
		t.Fatalf("error log: %+v", s.events)
	}
}

// sentry_sdk's OTLPIntegration exports OTLP traces to a path from the DSN.
func TestSentryOTLPTraces(t *testing.T) {
	srv, s := sentryServer(t)
	body := `{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"unknown_service"}}]},"scopeSpans":[{"spans":[
{"traceId":"5bf92f3577b34da6a3ce929d0e0e4736","spanId":"10f067aa0ba902b7","name":"GET /cart","kind":2,"startTimeUnixNano":"1763640000000000000","endTimeUnixNano":"1763640000050000000","status":{}}]}]}]}`
	if code, resp := post(t, srv.URL+"/sentry/api/42/integration/otlp/v1/traces", []byte(body), map[string]string{"X-Sentry-Auth": auth, "Content-Type": "application/json"}); code != 200 {
		t.Fatalf("otlp: %d %s", code, resp)
	}
	if len(s.spans) != 1 || s.spans[0].Service != "shop" || s.spans[0].Env != "production" || s.spans[0].Kind != "server" {
		t.Fatalf("spans: %+v", s.spans)
	}
	if code, _ := post(t, srv.URL+"/sentry/api/42/integration/otlp/v1/traces", []byte(body), map[string]string{"X-Sentry-Auth": "Sentry sentry_key=nope"}); code != 401 {
		t.Fatalf("wrong key: %d", code)
	}
}
