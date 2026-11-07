// Package ingest implements an OTLP/HTTP receiver (logs, traces, metrics) that
// turns error telemetry into normalized events.
//
// The Export*ServiceRequest messages are wire-identical to LogsData,
// TracesData and MetricsData (a single repeated field 1), so those are decoded
// directly; this avoids pulling in the gRPC collector packages.
package ingest

import (
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/weeklyvillain/sentinel/internal/event"
)

// Sink receives what the receiver extracts.
type Sink interface {
	// Heartbeat is called once per service per request, for every signal.
	Heartbeat(ctx context.Context, service, env string, t time.Time)
	// Event is called for every detected error.
	Event(ctx context.Context, e event.Event)
}

// Receiver is the OTLP/HTTP handler.
type Receiver struct {
	Sink Sink
	// MinSeverity is the lowest severity_number treated as an error (17 = ERROR).
	MinSeverity  int32
	MaxBodyBytes int64
	Log          *slog.Logger
	Now          func() time.Time
}

// Register mounts the OTLP endpoints on mux.
func (r *Receiver) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/logs", r.handle(r.logs))
	mux.HandleFunc("POST /v1/traces", r.handle(r.traces))
	mux.HandleFunc("POST /v1/metrics", r.handle(r.metrics))
}

type decodeFn func(ctx context.Context, body []byte, isJSON bool) error

var errUnsupported = errors.New("unsupported content type")

func (r *Receiver) handle(fn decodeFn) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ct, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
		var isJSON bool
		switch ct {
		case "application/x-protobuf", "application/protobuf":
		case "application/json":
			isJSON = true
		default:
			http.Error(w, errUnsupported.Error()+": "+ct, http.StatusUnsupportedMediaType)
			return
		}
		limit := r.MaxBodyBytes
		if limit <= 0 {
			limit = 32 << 20
		}
		var rd io.Reader = http.MaxBytesReader(w, req.Body, limit)
		switch strings.ToLower(req.Header.Get("Content-Encoding")) {
		case "", "identity":
		case "gzip":
			gz, err := gzip.NewReader(rd)
			if err != nil {
				writeStatus(w, isJSON, http.StatusBadRequest, "bad gzip body")
				return
			}
			defer gz.Close()
			rd = io.LimitReader(gz, limit)
		default:
			writeStatus(w, isJSON, http.StatusUnsupportedMediaType, "unsupported content-encoding")
			return
		}
		body, err := io.ReadAll(rd)
		if err != nil {
			writeStatus(w, isJSON, http.StatusBadRequest, "read body: "+err.Error())
			return
		}
		if err := fn(req.Context(), body, isJSON); err != nil {
			r.log().Warn("otlp decode failed", "path", req.URL.Path, "err", err)
			writeStatus(w, isJSON, http.StatusBadRequest, err.Error())
			return
		}
		// Every Export*ServiceResponse is empty on full success.
		if isJSON {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte("{}"))
		} else {
			w.Header().Set("Content-Type", "application/x-protobuf")
			w.WriteHeader(http.StatusOK)
		}
	}
}

// writeStatus writes an OTLP google.rpc.Status-shaped error.
func writeStatus(w http.ResponseWriter, isJSON bool, code int, msg string) {
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(map[string]any{"code": 3, "message": msg})
		return
	}
	http.Error(w, msg, code)
}

func (r *Receiver) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.Default()
}

func (r *Receiver) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func unmarshal(body []byte, isJSON bool, m proto.Message) error {
	if isJSON {
		fixed, err := fixJSONIDs(body)
		if err != nil {
			return err
		}
		return protojson.UnmarshalOptions{DiscardUnknown: true}.Unmarshal(fixed, m)
	}
	return proto.Unmarshal(body, m)
}

// fixJSONIDs converts OTLP/JSON's hex-encoded traceId/spanId/parentSpanId into
// the base64 that protojson expects for bytes fields.
func fixJSONIDs(body []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, fmt.Errorf("invalid JSON: %w", err)
	}
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, val := range t {
				switch k {
				case "traceId", "spanId", "parentSpanId", "trace_id", "span_id", "parent_span_id":
					if s, ok := val.(string); ok && s != "" {
						if b, err := hex.DecodeString(s); err == nil {
							t[k] = base64.StdEncoding.EncodeToString(b)
						}
					}
				default:
					walk(val)
				}
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return json.Marshal(v)
}

type resInfo struct {
	service, env string
	attrs        map[string]string
}

func resourceInfo(res *resourcepb.Resource) resInfo {
	ri := resInfo{attrs: map[string]string{}}
	if res != nil {
		putAttrs(ri.attrs, res.GetAttributes())
	}
	ri.service = ri.attrs["service.name"]
	if ri.service == "" {
		ri.service = "unknown_service"
	}
	ri.env = ri.attrs["deployment.environment.name"]
	if ri.env == "" {
		ri.env = ri.attrs["deployment.environment"]
	}
	return ri
}

func (r *Receiver) heartbeat(ctx context.Context, seen map[string]bool, ri resInfo) {
	if seen[ri.service] {
		return
	}
	seen[ri.service] = true
	r.Sink.Heartbeat(ctx, ri.service, ri.env, r.now())
}

var errorWords = map[string]bool{
	"error": true, "err": true, "fatal": true, "critical": true, "crit": true,
	"alert": true, "emerg": true, "emergency": true, "panic": true, "severe": true,
}

func isErrorText(s string) bool {
	s = strings.ToLower(strings.TrimSpace(s))
	if errorWords[s] {
		return true
	}
	// e.g. "ERROR2", "Error-level"
	for w := range errorWords {
		if len(w) > 3 && strings.HasPrefix(s, w) {
			return true
		}
	}
	return false
}

func (r *Receiver) logs(ctx context.Context, body []byte, isJSON bool) error {
	var req logspb.LogsData
	if err := unmarshal(body, isJSON, &req); err != nil {
		return err
	}
	minSev := r.MinSeverity
	if minSev <= 0 {
		minSev = int32(logspb.SeverityNumber_SEVERITY_NUMBER_ERROR)
	}
	seen := map[string]bool{}
	for _, rl := range req.GetResourceLogs() {
		ri := resourceInfo(rl.GetResource())
		r.heartbeat(ctx, seen, ri)
		for _, sl := range rl.GetScopeLogs() {
			for _, lr := range sl.GetLogRecords() {
				attrs := map[string]string{}
				putAttrs(attrs, lr.GetAttributes())
				_, hasExc := attrs["exception.type"]
				if !hasExc {
					_, hasExc = attrs["exception.message"]
				}
				if !hasExc {
					_, hasExc = attrs["exception.stacktrace"]
				}
				isErr := int32(lr.GetSeverityNumber()) >= minSev || isErrorText(lr.GetSeverityText()) || hasExc
				if !isErr {
					continue
				}
				merged := mergeAttrs(ri.attrs, attrs)
				e := event.Event{
					Kind:        event.KindLog,
					Service:     ri.service,
					Environment: ri.env,
					Time:        r.pickTime(lr.GetTimeUnixNano(), lr.GetObservedTimeUnixNano()),
					Severity:    severity(lr),
					Message:     anyString(lr.GetBody()),
					ExcType:     attrs["exception.type"],
					ExcMessage:  attrs["exception.message"],
					Stacktrace:  attrs["exception.stacktrace"],
					TraceID:     hexID(lr.GetTraceId()),
					SpanID:      hexID(lr.GetSpanId()),
					Attributes:  merged,
				}
				r.Sink.Event(ctx, e)
			}
		}
	}
	return nil
}

func severity(lr *logspb.LogRecord) string {
	if lr.GetSeverityText() != "" {
		return lr.GetSeverityText()
	}
	if lr.GetSeverityNumber() != 0 {
		return strings.TrimPrefix(lr.GetSeverityNumber().String(), "SEVERITY_NUMBER_")
	}
	return ""
}

func (r *Receiver) traces(ctx context.Context, body []byte, isJSON bool) error {
	var req tracepb.TracesData
	if err := unmarshal(body, isJSON, &req); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, rs := range req.GetResourceSpans() {
		ri := resourceInfo(rs.GetResource())
		r.heartbeat(ctx, seen, ri)
		for _, ss := range rs.GetScopeSpans() {
			for _, sp := range ss.GetSpans() {
				var excEv *tracepb.Span_Event
				for _, ev := range sp.GetEvents() {
					if ev.GetName() == "exception" {
						excEv = ev
						break
					}
				}
				isErr := sp.GetStatus().GetCode() == tracepb.Status_STATUS_CODE_ERROR
				if !isErr && excEv == nil {
					continue
				}
				attrs := map[string]string{}
				putAttrs(attrs, sp.GetAttributes())
				exc := map[string]string{}
				if excEv != nil {
					putAttrs(exc, excEv.GetAttributes())
				}
				e := event.Event{
					Kind:        event.KindSpan,
					Service:     ri.service,
					Environment: ri.env,
					Time:        r.pickTime(sp.GetEndTimeUnixNano(), sp.GetStartTimeUnixNano()),
					Severity:    "ERROR",
					Message:     sp.GetStatus().GetMessage(),
					ExcType:     exc["exception.type"],
					ExcMessage:  exc["exception.message"],
					Stacktrace:  exc["exception.stacktrace"],
					SpanName:    sp.GetName(),
					TraceID:     hexID(sp.GetTraceId()),
					SpanID:      hexID(sp.GetSpanId()),
					Attributes:  mergeAttrs(ri.attrs, attrs),
				}
				r.Sink.Event(ctx, e)
			}
		}
	}
	return nil
}

func (r *Receiver) metrics(ctx context.Context, body []byte, isJSON bool) error {
	var req metricspb.MetricsData
	if err := unmarshal(body, isJSON, &req); err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, rm := range req.GetResourceMetrics() {
		r.heartbeat(ctx, seen, resourceInfo(rm.GetResource()))
	}
	return nil
}

func (r *Receiver) pickTime(ts ...uint64) time.Time {
	for _, t := range ts {
		if t > 0 {
			return time.Unix(0, int64(t)).UTC()
		}
	}
	return r.now().UTC()
}

func hexID(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	for _, c := range b {
		if c != 0 {
			return hex.EncodeToString(b)
		}
	}
	return ""
}

const maxAttrLen = 8192

func putAttrs(dst map[string]string, kvs []*commonpb.KeyValue) {
	for _, kv := range kvs {
		v := anyString(kv.GetValue())
		if len(v) > maxAttrLen {
			v = v[:maxAttrLen]
		}
		dst[kv.GetKey()] = v
	}
}

func mergeAttrs(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func anyString(v *commonpb.AnyValue) string {
	if v == nil {
		return ""
	}
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return x.StringValue
	case *commonpb.AnyValue_BoolValue:
		return fmt.Sprint(x.BoolValue)
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprint(x.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprint(x.DoubleValue)
	case *commonpb.AnyValue_BytesValue:
		return base64.StdEncoding.EncodeToString(x.BytesValue)
	case *commonpb.AnyValue_ArrayValue, *commonpb.AnyValue_KvlistValue:
		b, err := json.Marshal(toPlain(v))
		if err != nil {
			return ""
		}
		return string(b)
	}
	return ""
}

func toPlain(v *commonpb.AnyValue) any {
	switch x := v.GetValue().(type) {
	case *commonpb.AnyValue_ArrayValue:
		out := []any{}
		for _, e := range x.ArrayValue.GetValues() {
			out = append(out, toPlain(e))
		}
		return out
	case *commonpb.AnyValue_KvlistValue:
		out := map[string]any{}
		for _, kv := range x.KvlistValue.GetValues() {
			out[kv.GetKey()] = toPlain(kv.GetValue())
		}
		return out
	default:
		return anyString(v)
	}
}
