package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/store"
)

type sink struct {
	mu      sync.Mutex
	events  []event.Event
	logs    []store.LogRecord
	metrics []store.MetricPoint
	beats   []string
}

func (s *sink) Heartbeat(_ context.Context, svc, env string, _ time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beats = append(s.beats, svc+"/"+env)
}
func (s *sink) Records(_ context.Context, recs []Record) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range recs {
		s.logs = append(s.logs, r.Log)
		if r.Error != nil {
			s.events = append(s.events, *r.Error)
		}
	}
}
func (s *sink) Metrics(_ context.Context, pts []store.MetricPoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = append(s.metrics, pts...)
}

func server(t *testing.T) (*httptest.Server, *sink) {
	s := &sink{}
	mux := http.NewServeMux()
	(&Receiver{Sink: s}).Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, s
}

const logsJSON = `{"resourceLogs":[{"resource":{"attributes":[
  {"key":"service.name","value":{"stringValue":"checkout"}},
  {"key":"deployment.environment.name","value":{"stringValue":"prod"}}]},
 "scopeLogs":[{"logRecords":[
  {"timeUnixNano":"1767225600000000000","severityNumber":17,"severityText":"ERROR","body":{"stringValue":"db down"},
   "traceId":"5b8efff798038103d269b633813fc60c","spanId":"eee19b7ec3c1b174",
   "attributes":[{"key":"exception.type","value":{"stringValue":"ConnError"}}]},
  {"severityNumber":9,"body":{"stringValue":"just info"}},
  {"severityText":"fatal","body":{"stringValue":"by text"}},
  {"severityNumber":9,"body":{"stringValue":"info with exception"},"attributes":[{"key":"exception.message","value":{"stringValue":"x"}}]}
 ]}]}]}`

func TestLogsJSON(t *testing.T) {
	srv, s := server(t)
	resp, err := http.Post(srv.URL+"/v1/logs", "application/json", strings.NewReader(logsJSON))
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	body.ReadFrom(resp.Body)
	if resp.StatusCode != 200 || strings.TrimSpace(body.String()) != "{}" || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("bad response %d %q", resp.StatusCode, body.String())
	}
	if len(s.events) != 3 {
		t.Fatalf("want 3 error events, got %d: %+v", len(s.events), s.events)
	}
	e := s.events[0]
	if e.Service != "checkout" || e.Environment != "prod" || e.Message != "db down" || e.ExcType != "ConnError" ||
		e.TraceID != "5b8efff798038103d269b633813fc60c" || e.SpanID != "eee19b7ec3c1b174" || e.Kind != event.KindLog {
		t.Fatalf("bad event: %+v", e)
	}
	if !e.Time.Equal(time.Unix(1767225600, 0)) {
		t.Errorf("time: %v", e.Time)
	}
	if len(s.beats) != 1 || s.beats[0] != "checkout/prod" {
		t.Errorf("heartbeats: %v", s.beats)
	}
	if len(s.logs) != 4 || s.logs[1].Severity != 9 || s.logs[2].Severity != 21 || s.logs[0].Severity != 17 {
		t.Errorf("want all 4 logs stored with severities 17/9/21/17, got %+v", s.logs)
	}
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func TestTracesProtobufGzip(t *testing.T) {
	srv, s := server(t)
	req := &tracepb.TracesData{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", "api"), str("deployment.environment", "staging")}},
		ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{
			{Name: "ok", TraceId: bytes.Repeat([]byte{1}, 16)},
			{Name: "GET /x", TraceId: bytes.Repeat([]byte{2}, 16), Status: &tracepb.Status{Code: tracepb.Status_STATUS_CODE_ERROR, Message: "500"}},
			{Name: "with-exc", Events: []*tracepb.Span_Event{{Name: "exception", Attributes: []*commonpb.KeyValue{
				str("exception.type", "ValueError"), str("exception.message", "bad"), str("exception.stacktrace", "Traceback...")}}}},
		}}},
	}}}
	raw, _ := proto.Marshal(req)
	var gz bytes.Buffer
	w := gzip.NewWriter(&gz)
	w.Write(raw)
	w.Close()
	hr, _ := http.NewRequest("POST", srv.URL+"/v1/traces", &gz)
	hr.Header.Set("Content-Type", "application/x-protobuf")
	hr.Header.Set("Content-Encoding", "gzip")
	resp, err := http.DefaultClient.Do(hr)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-protobuf" {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if len(s.events) != 2 {
		t.Fatalf("want 2 error spans, got %d", len(s.events))
	}
	if s.events[0].SpanName != "GET /x" || s.events[0].Message != "500" || s.events[0].Environment != "staging" || s.events[0].TraceID != strings.Repeat("02", 16) {
		t.Errorf("bad span event: %+v", s.events[0])
	}
	if s.events[1].ExcType != "ValueError" || s.events[1].Stacktrace != "Traceback..." || s.events[1].Kind != event.KindSpan {
		t.Errorf("bad exception span: %+v", s.events[1])
	}
}

func TestMetricsHeartbeatAndBadInput(t *testing.T) {
	srv, s := server(t)
	resp, _ := http.Post(srv.URL+"/v1/metrics", "application/json",
		strings.NewReader(`{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"worker"}}]}}]}`))
	if resp.StatusCode != 200 || len(s.beats) != 1 || s.beats[0] != "worker/" || len(s.events) != 0 {
		t.Fatalf("metrics: status=%d beats=%v events=%d", resp.StatusCode, s.beats, len(s.events))
	}
	resp, _ = http.Post(srv.URL+"/v1/logs", "application/json", strings.NewReader(`{not json`))
	if resp.StatusCode != 400 {
		t.Errorf("bad json: want 400 got %d", resp.StatusCode)
	}
	resp, _ = http.Post(srv.URL+"/v1/logs", "text/plain", strings.NewReader(`x`))
	if resp.StatusCode != 415 {
		t.Errorf("bad content type: want 415 got %d", resp.StatusCode)
	}
	// protobuf logs with no service.name
	raw, _ := proto.Marshal(&logspb.LogsData{ResourceLogs: []*logspb.ResourceLogs{{ScopeLogs: []*logspb.ScopeLogs{{LogRecords: []*logspb.LogRecord{
		{SeverityNumber: logspb.SeverityNumber_SEVERITY_NUMBER_FATAL, Body: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: "x"}}}}}}}}})
	resp, _ = http.Post(srv.URL+"/v1/logs", "application/x-protobuf", bytes.NewReader(raw))
	if resp.StatusCode != 200 || len(s.events) != 1 || s.events[0].Service != "unknown_service" {
		t.Errorf("protobuf logs: %d %+v", resp.StatusCode, s.events)
	}
}

func TestMetricsConversion(t *testing.T) {
	srv, s := server(t)
	post := func(cum1, cum2 int) {
		body := `{"resourceMetrics":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"api"}}]},
		 "scopeMetrics":[{"metrics":[
		  {"name":"cpu","unit":"1","gauge":{"dataPoints":[{"asDouble":0.5,"timeUnixNano":"1767225600000000000"}]}},
		  {"name":"requests","sum":{"isMonotonic":true,"aggregationTemporality":2,"dataPoints":[
		    {"asInt":"` + itoa(cum1) + `","startTimeUnixNano":"1","timeUnixNano":"1767225600000000000","attributes":[{"key":"route","value":{"stringValue":"/a"}}]}]}},
		  {"name":"latency","histogram":{"aggregationTemporality":1,"dataPoints":[{"count":"4","sum":2.0,"timeUnixNano":"1767225600000000000"}]}}
		 ]}]}]}`
		_ = cum2
		resp, err := http.Post(srv.URL+"/v1/metrics", "application/json", strings.NewReader(body))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("post: %v %v", err, resp)
		}
	}
	post(100, 0)
	post(130, 0)
	byName := map[string][]store.MetricPoint{}
	for _, p := range s.metrics {
		byName[p.Name] = append(byName[p.Name], p)
	}
	if len(byName["cpu"]) != 2 || byName["cpu"][0].Value != 0.5 || byName["cpu"][0].Kind != "gauge" {
		t.Errorf("gauge: %+v", byName["cpu"])
	}
	// First cumulative point is the baseline; the second is the delta.
	if len(byName["requests"]) != 1 || byName["requests"][0].Value != 30 || byName["requests"][0].Attrs != "route=/a" {
		t.Errorf("counter: %+v", byName["requests"])
	}
	if len(byName["latency"]) != 2 || byName["latency"][0].Value != 0.5 {
		t.Errorf("histogram mean: %+v", byName["latency"])
	}
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestIngestToken(t *testing.T) {
	s := &sink{}
	mux := http.NewServeMux()
	(&Receiver{Sink: s, Tokens: []string{"tok-a", "tok-b"}}).Register(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	send := func(auth string) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/logs", strings.NewReader(logsJSON))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for auth, want := range map[string]int{"": 401, "Bearer wrong": 401, "tok-a": 401, "Bearer tok-a": 200, "Bearer tok-b": 200} {
		if got := send(auth); got != want {
			t.Errorf("auth %q: got %d want %d", auth, got, want)
		}
	}
	if len(s.events) != 6 { // two accepted requests x 3 errors
		t.Errorf("events: %d", len(s.events))
	}
}
