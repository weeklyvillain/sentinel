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
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Sink receives what the receiver extracts.
type Sink interface {
	// Heartbeat is called once per service per request, for every signal.
	Heartbeat(ctx context.Context, service, env string, t time.Time)
	// Records is called once per request with every log record (and error
	// span); errors carry a normalized Event.
	Records(ctx context.Context, recs []Record)
	// Metrics is called with converted metric points.
	Metrics(ctx context.Context, pts []store.MetricPoint)
}

// Receiver is the OTLP/HTTP handler.
type Receiver struct {
	Sink Sink
	// MinSeverity is the lowest severity_number treated as an error (17 = ERROR).
	MinSeverity  int32
	MaxBodyBytes int64
	// Tokens, if non-empty, are the accepted bearer tokens
	// ("Authorization: Bearer <token>"). Empty disables authentication.
	Tokens []string
	// TokenSource, when set, replaces Tokens (a token can change at runtime).
	TokenSource func() []string
	Log         *slog.Logger
	Now         func() time.Time
	// SpanSampleRate is the share of traces stored for the trace view (0-1).
	// Error spans are always stored; request statistics count every span.
	SpanSampleRate float64

	deltaOnce sync.Once
	delta     *deltaTracker
}

func (r *Receiver) deltas() *deltaTracker {
	r.deltaOnce.Do(func() { r.delta = newDeltaTracker() })
	return r.delta
}

// Register mounts the OTLP endpoints on mux.
func (r *Receiver) Register(mux *http.ServeMux) {
	mux.HandleFunc("POST /v1/logs", r.handle(r.logs))
	mux.HandleFunc("POST /v1/traces", r.handle(r.traces))
	mux.HandleFunc("POST /v1/metrics", r.handle(r.metrics))
}

type decodeFn func(ctx context.Context, body []byte, isJSON bool) error

var errUnsupported = errors.New("unsupported content type")

// authorized checks the bearer token in constant time.
func (r *Receiver) authorized(req *http.Request) bool {
	tokens := r.Tokens
	if r.TokenSource != nil {
		tokens = r.TokenSource()
	}
	if len(tokens) == 0 {
		return true
	}
	h := req.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(h, "Bearer ")
	if !ok {
		tok, ok = strings.CutPrefix(h, "bearer ")
	}
	if !ok || tok == "" {
		return false
	}
	match := 0
	for _, t := range tokens {
		match |= subtle.ConstantTimeCompare([]byte(tok), []byte(t))
	}
	return match == 1
}

func (r *Receiver) handle(fn decodeFn) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if !r.authorized(req) {
			ct, _, _ := mime.ParseMediaType(req.Header.Get("Content-Type"))
			w.Header().Set("WWW-Authenticate", `Bearer realm="sentinel-otlp"`)
			writeStatus(w, ct == "application/json", http.StatusUnauthorized, "missing or invalid ingest token")
			return
		}
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
func writeStatus(w http.ResponseWriter, isJSON bool, status int, msg string) {
	if isJSON {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		code := 3 // INVALID_ARGUMENT
		if status == http.StatusUnauthorized {
			code = 16 // UNAUTHENTICATED
		}
		json.NewEncoder(w).Encode(map[string]any{"code": code, "message": msg})
		return
	}
	http.Error(w, msg, status)
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
	service, env, version string
	attrs                 map[string]string
}

// key identifies the sending process by all its resource attributes.
func (ri resInfo) key() string {
	parts := make([]string, 0, len(ri.attrs))
	for k, v := range ri.attrs {
		parts = append(parts, k+"="+v)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x1f")
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
	ri.version = ri.attrs["service.version"]
	return ri
}

func (r *Receiver) heartbeat(ctx context.Context, seen map[string]bool, ri resInfo) {
	if seen[ri.service] {
		return
	}
	seen[ri.service] = true
	r.Sink.Heartbeat(ctx, ri.service, ri.env, r.now())
	if rs, ok := r.Sink.(ReleaseSink); ok && ri.version != "" {
		rs.Release(ctx, ri.service, ri.env, ri.version, r.now())
	}
}

// ReleaseSink is optionally implemented by a Sink to learn which version
// (service.version) of a service is sending, for release tracking.
type ReleaseSink interface {
	Release(ctx context.Context, service, env, version string, t time.Time)
}

// SpanSink is optionally implemented by a Sink to store every span (not
// only errors) for the trace view and performance data.
type SpanSink interface {
	Spans(ctx context.Context, spans []store.Span)
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

// Record is one stored log line (or error span). Error is set when the record
// is an error that should be grouped.
type Record struct {
	Log   store.LogRecord
	Error *event.Event
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
	var batch []Record
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
				sevNum := int32(lr.GetSeverityNumber())
				if sevNum == 0 {
					sevNum = severityFromText(lr.GetSeverityText())
				}
				isErr := int32(lr.GetSeverityNumber()) >= minSev || isErrorText(lr.GetSeverityText()) || hasExc
				ts := r.pickTime(lr.GetTimeUnixNano(), lr.GetObservedTimeUnixNano())
				rec := Record{Log: store.LogRecord{
					Time:         ts,
					Service:      ri.service,
					Env:          ri.env,
					Kind:         "log",
					Severity:     sevNum,
					SeverityText: severity(lr),
					Body:         anyString(lr.GetBody()),
					TraceID:      hexID(lr.GetTraceId()),
					SpanID:       hexID(lr.GetSpanId()),
					Attrs:        attrs,
				}}
				if isErr {
					if rec.Log.Severity < 17 {
						rec.Log.Severity = 17
					}
					rec.Error = &event.Event{
						Kind:        event.KindLog,
						Service:     ri.service,
						Environment: ri.env,
						Release:     ri.version,
						Time:        ts,
						Severity:    severity(lr),
						Message:     rec.Log.Body,
						ExcType:     attrs["exception.type"],
						ExcMessage:  attrs["exception.message"],
						Stacktrace:  attrs["exception.stacktrace"],
						TraceID:     rec.Log.TraceID,
						SpanID:      rec.Log.SpanID,
						Attributes:  mergeAttrs(ri.attrs, attrs),
					}
				}
				batch = append(batch, rec)
			}
		}
	}
	r.Sink.Records(ctx, batch)
	return nil
}

// severityFromText maps a severity_text to a severity_number when the
// producer only set the text.
func severityFromText(s string) int32 {
	s = strings.ToLower(strings.TrimSpace(s))
	switch {
	case s == "":
		return 0
	case strings.HasPrefix(s, "trace"):
		return 1
	case strings.HasPrefix(s, "debug"):
		return 5
	case strings.HasPrefix(s, "info"), s == "notice":
		return 9
	case strings.HasPrefix(s, "warn"):
		return 13
	case s == "fatal" || s == "emerg" || s == "emergency" || s == "panic" || s == "alert":
		return 21
	case isErrorText(s):
		return 17
	}
	return 0
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
	return r.tracesData(ctx, &req, nil)
}

// tracesData stores spans and files error spans. override, when set,
// adjusts each resource (client keys put spans under their own service).
func (r *Receiver) tracesData(ctx context.Context, req *tracepb.TracesData, override func(*resInfo)) error {
	seen := map[string]bool{}
	var batch []Record
	spanSink, keepSpans := r.Sink.(SpanSink)
	var spans []store.Span
	for _, rs := range req.GetResourceSpans() {
		ri := resourceInfo(rs.GetResource())
		if override != nil {
			override(&ri)
		}
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
				attrs := map[string]string{}
				putAttrs(attrs, sp.GetAttributes())
				if keepSpans {
					spans = append(spans, r.storeSpan(ri, sp, attrs, isErr || excEv != nil))
				}
				if !isErr && excEv == nil {
					continue
				}
				exc := map[string]string{}
				if excEv != nil {
					putAttrs(exc, excEv.GetAttributes())
				}
				e := event.Event{
					Kind:        event.KindSpan,
					Service:     ri.service,
					Environment: ri.env,
					Release:     ri.version,
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
				// Error spans are also stored as searchable log lines.
				logAttrs := mergeAttrs(attrs, exc)
				logAttrs["span.name"] = sp.GetName()
				body := e.Summary()
				batch = append(batch, Record{Error: &e, Log: store.LogRecord{
					Time: e.Time, Service: e.Service, Env: e.Environment, Kind: "span", Severity: 17, SeverityText: "ERROR",
					Body: body, TraceID: e.TraceID, SpanID: e.SpanID, Attrs: logAttrs,
				}})
			}
		}
	}
	if len(spans) > 0 {
		spanSink.Spans(ctx, spans)
	}
	r.Sink.Records(ctx, batch)
	return nil
}

var spanKinds = map[tracepb.Span_SpanKind]string{
	tracepb.Span_SPAN_KIND_INTERNAL: "internal", tracepb.Span_SPAN_KIND_SERVER: "server", tracepb.Span_SPAN_KIND_CLIENT: "client",
	tracepb.Span_SPAN_KIND_PRODUCER: "producer", tracepb.Span_SPAN_KIND_CONSUMER: "consumer",
}

// storeSpan converts a span for storage. Spans of errors are always kept;
// other traces are kept with probability SpanSampleRate, decided by trace id
// so a trace is kept or dropped as a whole.
func (r *Receiver) storeSpan(ri resInfo, sp *tracepb.Span, attrs map[string]string, isErr bool) store.Span {
	start := r.pickTime(sp.GetStartTimeUnixNano(), sp.GetEndTimeUnixNano())
	dur := int64(0)
	if sp.GetEndTimeUnixNano() > sp.GetStartTimeUnixNano() && sp.GetStartTimeUnixNano() > 0 {
		dur = int64(sp.GetEndTimeUnixNano()-sp.GetStartTimeUnixNano()) / 1000
	}
	out := store.Span{
		TraceID: hexID(sp.GetTraceId()), SpanID: hexID(sp.GetSpanId()), ParentID: hexID(sp.GetParentSpanId()),
		Service: ri.service, Env: ri.env, Version: ri.version, Name: sp.GetName(), Kind: spanKinds[sp.GetKind()],
		Start: start, DurationUS: dur, Status: int(sp.GetStatus().GetCode()), StatusMsg: sp.GetStatus().GetMessage(),
		Attrs: attrs,
	}
	if out.Kind == "" {
		out.Kind = "internal"
	}
	for i, ev := range sp.GetEvents() {
		if i == 32 {
			break
		}
		ea := map[string]string{}
		putAttrs(ea, ev.GetAttributes())
		out.Events = append(out.Events, store.SpanEvent{Time: r.pickTime(ev.GetTimeUnixNano()), Name: ev.GetName(), Attrs: ea})
	}
	out.Sampled = isErr || sampled(out.TraceID, r.SpanSampleRate)
	return out
}

// sampled decides from the trace id (its last 8 bytes are random).
func sampled(traceID string, rate float64) bool {
	if rate >= 1 {
		return true
	}
	if rate <= 0 || len(traceID) < 16 {
		return false
	}
	v, err := strconv.ParseUint(traceID[len(traceID)-16:], 16, 64)
	if err != nil {
		return false
	}
	return float64(v) < rate*float64(math.MaxUint64)
}

func (r *Receiver) metrics(ctx context.Context, body []byte, isJSON bool) error {
	var req metricspb.MetricsData
	if err := unmarshal(body, isJSON, &req); err != nil {
		return err
	}
	seen := map[string]bool{}
	var pts []store.MetricPoint
	for _, rm := range req.GetResourceMetrics() {
		ri := resourceInfo(rm.GetResource())
		r.heartbeat(ctx, seen, ri)
		for _, sm := range rm.GetScopeMetrics() {
			for _, m := range sm.GetMetrics() {
				pts = append(pts, r.deltas().convert(ri.service, ri.key(), m, r.now())...)
			}
		}
	}
	if len(pts) > 0 {
		r.Sink.Metrics(ctx, pts)
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
