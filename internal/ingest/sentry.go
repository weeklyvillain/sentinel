package ingest

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/store"
)

// Sentry compatibility: Sentry SDKs keep working, pointed at a Sentinel DSN
// (https://<key>@<host>/sentry/<project>) instead of Sentry's. The SDK
// posts envelopes to /sentry/api/<project>/envelope/ (or events to the
// older /store/); errors and messages become log lines and issues,
// transactions become traces, Sentry logs become log lines. Everything
// else in an envelope (sessions, client reports, replays, profiles,
// attachments) is accepted and dropped.
// https://develop.sentry.dev/sdk/data-model/envelopes/

// SentryProject is what a DSN's project id and key stand for.
type SentryProject struct {
	ID          string
	Key         string
	Service     string
	Environment string // used when an event has none
	// Origins, when set, are the only sites browsers may send from.
	Origins []string
}

// SentryLookup finds a project by id; nil when Sentry ingestion is off.
type SentryLookup func(projectID string) (SentryProject, bool)

const maxSentryBody = 20 << 20

// RegisterSentry mounts the Sentry endpoints on mux.
func (r *Receiver) RegisterSentry(mux *http.ServeMux, lookup SentryLookup) {
	for _, p := range []string{"/sentry/api/{project}/envelope/", "/sentry/api/{project}/store/"} {
		mux.HandleFunc("POST "+p+"{$}", r.sentry(lookup))
		mux.HandleFunc("OPTIONS "+p+"{$}", sentryCORS)
	}
	// Sentry SDKs in OpenTelemetry mode (sentry_sdk's OTLPIntegration) export
	// OTLP traces to a path derived from the DSN.
	for _, p := range []string{"/sentry/api/{project}/integration/otlp/v1/traces", "/sentry/api/{project}/integration/otlp/v1/traces/"} {
		mux.HandleFunc("POST "+p, r.sentryOTLP(lookup))
		mux.HandleFunc("OPTIONS "+p, sentryCORS)
	}
}

func (r *Receiver) sentryOTLP(lookup SentryLookup) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		setSentryCORS(w, req)
		if lookup == nil {
			sentryErr(w, http.StatusNotFound, "Sentry ingestion is not set up")
			return
		}
		proj, ok := lookup(req.PathValue("project"))
		key := sentryKey(req)
		if !ok || key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(proj.Key)) != 1 {
			sentryErr(w, http.StatusUnauthorized, "unknown DSN: copy it again from Settings → Integrations")
			return
		}
		body, err := readSentryBody(req)
		if err != nil {
			sentryErr(w, http.StatusBadRequest, err.Error())
			return
		}
		isJSON := strings.HasPrefix(req.Header.Get("Content-Type"), "application/json")
		if err := r.clientTraces(req.Context(), body, isJSON, proj, "", "", nil); err != nil {
			sentryErr(w, http.StatusBadRequest, err.Error())
			return
		}
		w.Header().Set("Content-Type", req.Header.Get("Content-Type"))
		w.WriteHeader(http.StatusOK)
	}
}

// Browser SDKs post across origins.
func setSentryCORS(w http.ResponseWriter, req *http.Request) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", allowHeaders(req, "Content-Type, X-Sentry-Auth, Content-Encoding, Authorization, sentry-trace, baggage"))
	h.Set("Access-Control-Max-Age", "86400")
}

func sentryCORS(w http.ResponseWriter, req *http.Request) {
	setSentryCORS(w, req)
	w.WriteHeader(http.StatusNoContent)
}

func sentryErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"detail": msg})
}

// sentryKey reads the public key: X-Sentry-Auth, ?sentry_key=, or a Bearer
// Authorization header as some proxies send.
func sentryKey(req *http.Request) string {
	if k := req.URL.Query().Get("sentry_key"); k != "" {
		return k
	}
	for _, h := range []string{req.Header.Get("X-Sentry-Auth"), req.Header.Get("Authorization")} {
		h = strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(h, "Sentry "), "sentry "))
		for _, part := range strings.Split(h, ",") {
			if k, ok := strings.CutPrefix(strings.TrimSpace(part), "sentry_key="); ok {
				return k
			}
		}
	}
	return ""
}

func (r *Receiver) sentry(lookup SentryLookup) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		setSentryCORS(w, req)
		if lookup == nil {
			sentryErr(w, http.StatusNotFound, "Sentry ingestion is not set up")
			return
		}
		proj, ok := lookup(req.PathValue("project"))
		body, err := readSentryBody(req)
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errUnsupportedEncoding) {
				code = http.StatusUnsupportedMediaType
			}
			sentryErr(w, code, err.Error())
			return
		}
		key := sentryKey(req)
		if key == "" && strings.HasSuffix(req.URL.Path, "/envelope/") {
			key = envelopeDSNKey(body)
		}
		if !ok || key == "" || subtle.ConstantTimeCompare([]byte(key), []byte(proj.Key)) != 1 {
			sentryErr(w, http.StatusUnauthorized, "unknown DSN: copy it again from Settings → Integrations")
			return
		}
		if origin := req.Header.Get("Origin"); !originAllowed(proj, origin) {
			sentryErr(w, http.StatusForbidden, "origin "+origin+" is not allowed for this DSN")
			return
		}
		ctx := req.Context()
		var res sentryBatch
		if strings.HasSuffix(req.URL.Path, "/store/") {
			res.event(proj, body, r.now())
		} else if err := res.envelope(proj, body, r.now()); err != nil {
			sentryErr(w, http.StatusBadRequest, err.Error())
			return
		}
		r.Sink.Heartbeat(ctx, proj.Service, res.env(proj), r.now())
		if rs, ok := r.Sink.(ReleaseSink); ok && res.release != "" {
			rs.Release(ctx, proj.Service, res.env(proj), res.release, r.now())
		}
		if len(res.spans) > 0 {
			if ss, ok := r.Sink.(SpanSink); ok {
				for i := range res.spans {
					res.spans[i].Sampled = res.spans[i].Status == 2 || sampled(res.spans[i].TraceID, r.SpanSampleRate)
				}
				ss.Spans(ctx, res.spans)
			}
		}
		if len(res.records) > 0 {
			r.Sink.Records(ctx, res.records)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"id": res.eventID})
	}
}

var errUnsupportedEncoding = errors.New("unsupported Content-Encoding")

func readSentryBody(req *http.Request) ([]byte, error) {
	var rd io.Reader = http.MaxBytesReader(nil, req.Body, maxSentryBody)
	switch enc := strings.ToLower(strings.TrimSpace(req.Header.Get("Content-Encoding"))); enc {
	case "", "identity":
	case "gzip", "x-gzip":
		gz, err := gzip.NewReader(rd)
		if err != nil {
			return nil, err
		}
		rd = gz
	case "deflate":
		// Usually zlib-wrapped; some clients send raw deflate.
		raw, err := io.ReadAll(rd)
		if err != nil {
			return nil, err
		}
		if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			rd = zr
		} else {
			rd = flate.NewReader(bytes.NewReader(raw))
		}
	default:
		return nil, fmt.Errorf("%w %q: use gzip (the Python SDK compresses with brotli when the brotli package is installed; set transport_compression_algo to gzip)", errUnsupportedEncoding, enc)
	}
	b, err := io.ReadAll(io.LimitReader(rd, maxSentryBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxSentryBody {
		return nil, errors.New("envelope too large")
	}
	return b, nil
}

// envelopeDSNKey reads the key from the envelope header's dsn.
func envelopeDSNKey(body []byte) string {
	line, _, _ := bytes.Cut(body, []byte("\n"))
	var h struct {
		DSN string `json:"dsn"`
	}
	if json.Unmarshal(line, &h) != nil || h.DSN == "" {
		return ""
	}
	at := strings.Index(h.DSN, "@")
	start := strings.Index(h.DSN, "://")
	if at < 0 || start < 0 || at < start {
		return ""
	}
	key, _, _ := strings.Cut(h.DSN[start+3:at], ":")
	return key
}

// sentryBatch collects what one request converts into.
type sentryBatch struct {
	eventID string
	release string
	envSeen string
	records []Record
	spans   []store.Span
}

func (b *sentryBatch) env(p SentryProject) string {
	if b.envSeen != "" {
		return b.envSeen
	}
	return p.Environment
}

// envelope walks the items: a header line, then per item a header line and
// a payload of `length` bytes (or up to the next newline).
func (b *sentryBatch) envelope(p SentryProject, body []byte, now time.Time) error {
	rd := bufio.NewReaderSize(bytes.NewReader(body), 64<<10)
	head, err := readLine(rd)
	if err != nil {
		return errors.New("empty envelope")
	}
	var eh struct {
		EventID string `json:"event_id"`
	}
	json.Unmarshal(head, &eh)
	b.eventID = eh.EventID
	for {
		ih, err := readLine(rd)
		if err != nil {
			return nil // end of envelope
		}
		if len(bytes.TrimSpace(ih)) == 0 {
			continue
		}
		var item struct {
			Type   string `json:"type"`
			Length *int   `json:"length"`
		}
		if json.Unmarshal(ih, &item) != nil {
			return nil // keep the items read so far
		}
		var payload []byte
		if item.Length != nil {
			if *item.Length < 0 || *item.Length > maxSentryBody {
				return nil
			}
			payload = make([]byte, *item.Length)
			if _, err := io.ReadFull(rd, payload); err != nil {
				return nil
			}
			if c, err := rd.Peek(1); err == nil && c[0] == '\n' {
				rd.ReadByte()
			}
		} else {
			payload, _ = readLine(rd)
		}
		switch item.Type {
		case "event":
			b.event(p, payload, now)
		case "transaction":
			b.transaction(p, payload, now)
		case "log":
			b.logs(p, payload, now)
		}
	}
}

func readLine(rd *bufio.Reader) ([]byte, error) {
	line, err := rd.ReadBytes('\n')
	if len(line) == 0 && err != nil {
		return nil, err
	}
	return bytes.TrimRight(line, "\r\n"), nil
}

// sentryTime reads a timestamp: seconds (float) or RFC 3339.
type sentryTime struct{ t time.Time }

func (s *sentryTime) UnmarshalJSON(b []byte) error {
	var f float64
	if json.Unmarshal(b, &f) == nil {
		if f > 0 {
			sec := int64(f)
			s.t = time.Unix(sec, int64((f-float64(sec))*1e9)).UTC()
		}
		return nil
	}
	var str string
	if json.Unmarshal(b, &str) == nil && str != "" {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999", "2006-01-02T15:04:05"} {
			if t, err := time.Parse(layout, str); err == nil {
				s.t = t.UTC()
				return nil
			}
		}
	}
	return nil
}

func (s sentryTime) or(now time.Time) time.Time {
	if s.t.IsZero() || s.t.After(now.Add(time.Hour)) {
		return now.UTC()
	}
	return s.t
}

type sentryFrame struct {
	Filename string `json:"filename"`
	AbsPath  string `json:"abs_path"`
	Module   string `json:"module"`
	Function string `json:"function"`
	Lineno   int    `json:"lineno"`
	Colno    int    `json:"colno"`
	InApp    *bool  `json:"in_app"`
}

type sentryException struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	Module     string `json:"module"`
	Stacktrace *struct {
		Frames []sentryFrame `json:"frames"`
	} `json:"stacktrace"`
}

// list accepts {"values": [...]} or a bare array (older SDKs).
type sentryList[T any] struct{ Values []T }

func (l *sentryList[T]) UnmarshalJSON(b []byte) error {
	var wrapped struct {
		Values []T `json:"values"`
	}
	if json.Unmarshal(b, &wrapped) == nil && wrapped.Values != nil {
		l.Values = wrapped.Values
		return nil
	}
	return json.Unmarshal(b, &l.Values)
}

// sentryMessage is a string or {"formatted"/"message", "params"}.
type sentryMessage struct{ Text string }

func (m *sentryMessage) UnmarshalJSON(b []byte) error {
	if json.Unmarshal(b, &m.Text) == nil {
		return nil
	}
	var o struct {
		Formatted string `json:"formatted"`
		Message   string `json:"message"`
	}
	json.Unmarshal(b, &o)
	m.Text = o.Formatted
	if m.Text == "" {
		m.Text = o.Message
	}
	return nil
}

// sentryTags is an object or a list of [key, value] pairs.
type sentryTags map[string]string

func (t *sentryTags) UnmarshalJSON(b []byte) error {
	*t = sentryTags{}
	var m map[string]any
	if json.Unmarshal(b, &m) == nil {
		for k, v := range m {
			(*t)[k] = anyText(v)
		}
		return nil
	}
	var pairs [][]any
	if json.Unmarshal(b, &pairs) == nil {
		for _, p := range pairs {
			if len(p) == 2 {
				(*t)[anyText(p[0])] = anyText(p[1])
			}
		}
	}
	return nil
}

func anyText(v any) string {
	switch x := v.(type) {
	case nil:
		return ""
	case string:
		return x
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(x)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

type sentryEvent struct {
	EventID     string                       `json:"event_id"`
	Timestamp   sentryTime                   `json:"timestamp"`
	Level       string                       `json:"level"`
	Platform    string                       `json:"platform"`
	Logger      string                       `json:"logger"`
	Transaction string                       `json:"transaction"`
	ServerName  string                       `json:"server_name"`
	Release     string                       `json:"release"`
	Environment string                       `json:"environment"`
	Message     *sentryMessage               `json:"message"`
	Logentry    *sentryMessage               `json:"logentry"`
	Exception   *sentryList[sentryException] `json:"exception"`
	Tags        sentryTags                   `json:"tags"`
	Extra       map[string]any               `json:"extra"`
	User        map[string]any               `json:"user"`
	Fingerprint []string                     `json:"fingerprint"`
	Request     *struct {
		URL    string `json:"url"`
		Method string `json:"method"`
	} `json:"request"`
	Contexts map[string]json.RawMessage `json:"contexts"`
	SDK      *struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	} `json:"sdk"`
	// Transactions.
	StartTimestamp sentryTime   `json:"start_timestamp"`
	Spans          []sentrySpan `json:"spans"`
}

type sentryTrace struct {
	TraceID      string         `json:"trace_id"`
	SpanID       string         `json:"span_id"`
	ParentSpanID string         `json:"parent_span_id"`
	Op           string         `json:"op"`
	Status       string         `json:"status"`
	Data         map[string]any `json:"data"`
}

func (e *sentryEvent) trace() sentryTrace {
	var t sentryTrace
	if raw, ok := e.Contexts["trace"]; ok {
		json.Unmarshal(raw, &t)
	}
	return t
}

var sentryLevels = map[string]int32{"fatal": 21, "critical": 21, "error": 17, "warning": 13, "warn": 13, "info": 9, "log": 9, "debug": 5, "trace": 1}

func sentrySeverity(level string) (int32, string) {
	l := strings.ToLower(level)
	if l == "" {
		l = "error"
	}
	n, ok := sentryLevels[l]
	if !ok {
		n = 17
	}
	return n, strings.ToUpper(l)
}

// stack writes Sentry frames (oldest first) as "at fn (file:line)" lines,
// newest first, which grouping reads like a JavaScript or Java trace. With
// in-app frames, only those.
func stack(frames []sentryFrame) string {
	inApp := false
	for _, f := range frames {
		if f.InApp != nil && *f.InApp {
			inApp = true
			break
		}
	}
	var sb strings.Builder
	for i := len(frames) - 1; i >= 0; i-- {
		f := frames[i]
		if inApp && (f.InApp == nil || !*f.InApp) {
			continue
		}
		file := f.Filename
		if file == "" {
			file = f.AbsPath
		}
		if file == "" {
			file = f.Module
		}
		fn := f.Function
		if fn == "" {
			fn = "<anonymous>"
		}
		loc := file
		if f.Lineno > 0 {
			loc += ":" + strconv.Itoa(f.Lineno)
			if f.Colno > 0 {
				loc += ":" + strconv.Itoa(f.Colno)
			}
		}
		fmt.Fprintf(&sb, "at %s (%s)\n", fn, loc)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// event converts an error or message event.
func (b *sentryBatch) event(p SentryProject, payload []byte, now time.Time) {
	var e sentryEvent
	if json.Unmarshal(payload, &e) != nil {
		return
	}
	if b.eventID == "" {
		b.eventID = e.EventID
	}
	env := e.Environment
	if env == "" {
		env = p.Environment
	}
	if env != "" {
		b.envSeen = env
	}
	if e.Release != "" {
		b.release = e.Release
	}
	sev, sevText := sentrySeverity(e.Level)
	ts := e.Timestamp.or(now)
	tr := e.trace()

	attrs := map[string]string{}
	for k, v := range e.Tags {
		attrs[k] = v
	}
	set := func(k, v string) {
		if v != "" {
			attrs[k] = v
		}
	}
	set("sentry.event_id", e.EventID)
	set("sentry.platform", e.Platform)
	set("logger", e.Logger)
	set("transaction", e.Transaction)
	set("server.name", e.ServerName)
	if e.SDK != nil {
		set("sentry.sdk", strings.TrimSpace(e.SDK.Name+" "+e.SDK.Version))
	}
	if e.Request != nil {
		set("http.method", e.Request.Method)
		set("url.full", e.Request.URL)
	}
	for _, k := range []string{"id", "username", "email", "ip_address"} {
		if v, ok := e.User[k]; ok {
			set("user."+k, anyText(v))
		}
	}
	for k, v := range e.Extra {
		set("extra."+k, anyText(v))
	}
	if len(e.Fingerprint) > 0 && !(len(e.Fingerprint) == 1 && e.Fingerprint[0] == "{{ default }}") {
		attrs[event.FingerprintAttr] = strings.Join(e.Fingerprint, "|")
	}

	msg := ""
	if e.Logentry != nil {
		msg = e.Logentry.Text
	}
	if msg == "" && e.Message != nil {
		msg = e.Message.Text
	}
	var exc *sentryException
	if e.Exception != nil && len(e.Exception.Values) > 0 {
		// The last value is the one that was raised; earlier ones are causes.
		exc = &e.Exception.Values[len(e.Exception.Values)-1]
	}
	ev := event.Event{Kind: event.KindLog, Service: p.Service, Environment: env, Release: e.Release, Time: ts,
		Severity: sevText, Message: msg, TraceID: tr.TraceID, SpanID: tr.SpanID, Attributes: attrs}
	if exc != nil {
		ev.ExcType, ev.ExcMessage = exc.Type, exc.Value
		if exc.Module != "" && !strings.Contains(exc.Type, ".") {
			attrs["exception.module"] = exc.Module
		}
		if exc.Stacktrace != nil {
			ev.Stacktrace = stack(exc.Stacktrace.Frames)
		}
		set("exception.type", ev.ExcType)
		set("exception.message", ev.ExcMessage)
		set("exception.stacktrace", ev.Stacktrace)
	}
	body := msg
	if body == "" {
		body = ev.Summary()
	}
	rec := Record{Log: store.LogRecord{Time: ts, Service: p.Service, Env: env, Kind: "log", Severity: sev, SeverityText: sevText,
		Body: body, TraceID: tr.TraceID, SpanID: tr.SpanID, Attrs: attrs}}
	// Exceptions and error-level messages are errors; info and warning
	// messages are log lines.
	if exc != nil || sev >= 17 {
		if rec.Log.Severity < 17 {
			rec.Log.Severity = 17
		}
		rec.Error = &ev
	}
	b.records = append(b.records, rec)
}

type sentrySpan struct {
	SpanID         string         `json:"span_id"`
	ParentSpanID   string         `json:"parent_span_id"`
	TraceID        string         `json:"trace_id"`
	Op             string         `json:"op"`
	Description    string         `json:"description"`
	Status         string         `json:"status"`
	StartTimestamp sentryTime     `json:"start_timestamp"`
	Timestamp      sentryTime     `json:"timestamp"`
	Tags           sentryTags     `json:"tags"`
	Data           map[string]any `json:"data"`
}

func sentrySpanKind(op string) string {
	switch {
	case op == "http.server" || strings.HasPrefix(op, "http.server") || op == "pageload" || op == "navigation":
		return "server"
	case strings.HasPrefix(op, "http.client") || strings.HasPrefix(op, "db") || strings.HasPrefix(op, "cache") || op == "http":
		return "client"
	case strings.HasPrefix(op, "queue.process"):
		return "consumer"
	case strings.HasPrefix(op, "queue.publish"):
		return "producer"
	}
	return "internal"
}

// sentryStatus: ok and unset are fine; anything else is an error (2).
func sentryStatus(s string) int {
	if s == "" || s == "ok" {
		return 0
	}
	return 2
}

func durationUS(start, end time.Time) int64 {
	if end.After(start) {
		return end.Sub(start).Microseconds()
	}
	return 0
}

// transaction converts a transaction and its spans into stored spans.
func (b *sentryBatch) transaction(p SentryProject, payload []byte, now time.Time) {
	var e sentryEvent
	if json.Unmarshal(payload, &e) != nil {
		return
	}
	tr := e.trace()
	if tr.TraceID == "" || tr.SpanID == "" {
		return
	}
	env := e.Environment
	if env == "" {
		env = p.Environment
	}
	if env != "" {
		b.envSeen = env
	}
	if e.Release != "" {
		b.release = e.Release
	}
	start, end := e.StartTimestamp.or(now), e.Timestamp.or(now)
	attrs := map[string]string{}
	for k, v := range e.Tags {
		attrs[k] = v
	}
	for k, v := range tr.Data {
		attrs[k] = anyText(v)
	}
	if tr.Op != "" {
		attrs["sentry.op"] = tr.Op
	}
	name := e.Transaction
	if name == "" {
		name = tr.Op
	}
	b.spans = append(b.spans, store.Span{TraceID: tr.TraceID, SpanID: tr.SpanID, ParentID: tr.ParentSpanID, Service: p.Service, Env: env,
		Version: e.Release, Name: name, Kind: sentrySpanKind(tr.Op), Start: start, DurationUS: durationUS(start, end),
		Status: sentryStatus(tr.Status), StatusMsg: statusMsg(tr.Status), Attrs: attrs})
	for i, s := range e.Spans {
		if i >= 1000 {
			break
		}
		if s.SpanID == "" {
			continue
		}
		ss, se := s.StartTimestamp.or(start), s.Timestamp.or(end)
		sa := map[string]string{}
		for k, v := range s.Tags {
			sa[k] = v
		}
		for k, v := range s.Data {
			sa[k] = anyText(v)
		}
		if s.Op != "" {
			sa["sentry.op"] = s.Op
		}
		name := s.Description
		if name == "" {
			name = s.Op
		}
		if len(name) > 300 {
			name = name[:300]
		}
		trace := s.TraceID
		if trace == "" {
			trace = tr.TraceID
		}
		b.spans = append(b.spans, store.Span{TraceID: trace, SpanID: s.SpanID, ParentID: s.ParentSpanID, Service: p.Service, Env: env,
			Version: e.Release, Name: name, Kind: sentrySpanKind(s.Op), Start: ss, DurationUS: durationUS(ss, se),
			Status: sentryStatus(s.Status), StatusMsg: statusMsg(s.Status), Attrs: sa})
	}
}

func statusMsg(s string) string {
	if sentryStatus(s) == 0 {
		return ""
	}
	return s
}

// logs converts a Sentry "log" item: {"items": [{timestamp, level, body,
// trace_id, attributes: {k: {value, type}}}]}.
func (b *sentryBatch) logs(p SentryProject, payload []byte, now time.Time) {
	var in struct {
		Items []struct {
			Timestamp  sentryTime                 `json:"timestamp"`
			Level      string                     `json:"level"`
			Body       string                     `json:"body"`
			TraceID    string                     `json:"trace_id"`
			Attributes map[string]json.RawMessage `json:"attributes"`
		} `json:"items"`
	}
	if json.Unmarshal(payload, &in) != nil {
		return
	}
	for _, it := range in.Items {
		attrs := map[string]string{}
		keys := make([]string, 0, len(it.Attributes))
		for k := range it.Attributes {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			var v struct {
				Value any `json:"value"`
			}
			if json.Unmarshal(it.Attributes[k], &v) == nil && v.Value != nil {
				attrs[k] = anyText(v.Value)
			} else {
				attrs[k] = string(it.Attributes[k])
			}
		}
		env := attrs["sentry.environment"]
		if env == "" {
			env = p.Environment
		}
		if env != "" {
			b.envSeen = env
		}
		if r := attrs["sentry.release"]; r != "" {
			b.release = r
		}
		sev, sevText := sentrySeverity(it.Level)
		ts := it.Timestamp.or(now)
		rec := Record{Log: store.LogRecord{Time: ts, Service: p.Service, Env: env, Kind: "log", Severity: sev, SeverityText: sevText,
			Body: it.Body, TraceID: it.TraceID, Attrs: attrs}}
		if sev >= 17 {
			rec.Error = &event.Event{Kind: event.KindLog, Service: p.Service, Environment: env, Release: attrs["sentry.release"], Time: ts,
				Severity: sevText, Message: it.Body, TraceID: it.TraceID, Attributes: attrs}
		}
		b.records = append(b.records, rec)
	}
}
