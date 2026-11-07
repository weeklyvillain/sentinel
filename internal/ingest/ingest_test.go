package ingest

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
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
)

type sink struct {
	mu     sync.Mutex
	events []event.Event
	beats  []string
}

func (s *sink) Heartbeat(_ context.Context, svc, env string, _ time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beats = append(s.beats, svc+"/"+env)
}
func (s *sink) Event(_ context.Context, e event.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, e)
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
