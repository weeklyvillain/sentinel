package ingest

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/weeklyvillain/sentinel/internal/event"
	"github.com/weeklyvillain/sentinel/internal/store"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
)

// Client endpoints: telemetry from apps and browsers, authenticated with a
// service's public client key (the same key a Sentry DSN carries) in the
// URL, so they work from browsers and navigator.sendBeacon:
//
//	POST /ingest/{project}/{key}/logs     JSON logs: an object, an array or one object per line
//	POST /ingest/{project}/{key}/datadog  the Datadog Browser SDK's `proxy` (same parser)
//	POST /ingest/{project}/{key}/faro     a Grafana Faro collector
//
// A key with allowed origins only accepts browsers on those sites.

// RegisterClient mounts the client endpoints on mux.
func (r *Receiver) RegisterClient(mux *http.ServeMux, lookup SentryLookup) {
	for kind, h := range map[string]func(SentryProject, []byte, time.Time) (clientBatch, error){
		"logs": parseLogsBody, "datadog": parseLogsBody, "faro": parseFaro,
	} {
		p := "/ingest/{project}/{key}/" + kind
		mux.HandleFunc("POST "+p, r.client(lookup, h))
		mux.HandleFunc("OPTIONS "+p, r.clientPreflight(lookup))
	}
}

// originAllowed: no list allows every origin; otherwise the request's
// Origin must match an entry ("https://shop.example.com" or a
// "https://*.example.com" wildcard). Requests without an Origin (servers,
// sendBeacon from the same site sends one) are allowed.
func originAllowed(p SentryProject, origin string) bool {
	if len(p.Origins) == 0 || origin == "" {
		return true
	}
	o := strings.ToLower(strings.TrimRight(origin, "/"))
	for _, a := range p.Origins {
		a = strings.ToLower(strings.TrimRight(strings.TrimSpace(a), "/"))
		if a == "*" || a == o {
			return true
		}
		if scheme, host, ok := strings.Cut(a, "://*."); ok {
			if rest, ok := strings.CutPrefix(o, scheme+"://"); ok && strings.HasSuffix(rest, "."+host) {
				return true
			}
		}
	}
	return false
}

func setClientCORS(w http.ResponseWriter, req *http.Request, p SentryProject, origin string) {
	h := w.Header()
	if len(p.Origins) == 0 || origin == "" {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Add("Vary", "Origin")
	}
	h.Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", allowHeaders(req, "Content-Type, Content-Encoding, X-Faro-Session-Id, X-Api-Key, Authorization, Idempotency-Key"))
	h.Set("Access-Control-Max-Age", "86400")
}

// allowHeaders allows the headers a preflight asks for: SDKs add their own
// (Faro sends Idempotency-Key), and these endpoints never use cookies.
func allowHeaders(req *http.Request, base string) string {
	if asked := strings.TrimSpace(req.Header.Get("Access-Control-Request-Headers")); asked != "" && len(asked) < 1000 {
		return base + ", " + asked
	}
	return base
}

func (r *Receiver) authClient(w http.ResponseWriter, req *http.Request, lookup SentryLookup) (SentryProject, bool) {
	if lookup == nil {
		sentryErr(w, http.StatusNotFound, "client keys are not set up")
		return SentryProject{}, false
	}
	p, ok := lookup(req.PathValue("project"))
	origin := req.Header.Get("Origin")
	if !ok || subtle.ConstantTimeCompare([]byte(req.PathValue("key")), []byte(p.Key)) != 1 {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		sentryErr(w, http.StatusUnauthorized, "unknown client key: copy the URL again from Settings → Integrations")
		return p, false
	}
	setClientCORS(w, req, p, origin)
	if !originAllowed(p, origin) {
		sentryErr(w, http.StatusForbidden, "origin "+origin+" is not allowed for this key")
		return p, false
	}
	return p, true
}

func (r *Receiver) clientPreflight(lookup SentryLookup) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if _, ok := r.authClient(w, req, lookup); ok {
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

// clientBatch is what a client request converts into.
type clientBatch struct {
	records []Record
	metrics []store.MetricPoint
	env     string
	release string
	// traces is OTLP JSON (Faro's tracing), with attributes for every span.
	traces     json.RawMessage
	traceAttrs map[string]string
}

func (r *Receiver) client(lookup SentryLookup, parse func(SentryProject, []byte, time.Time) (clientBatch, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		p, ok := r.authClient(w, req, lookup)
		if !ok {
			return
		}
		body, err := readSentryBody(req)
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, errUnsupportedEncoding) {
				code = http.StatusUnsupportedMediaType
			}
			sentryErr(w, code, err.Error())
			return
		}
		b, err := parse(p, body, r.now())
		if err != nil {
			sentryErr(w, http.StatusBadRequest, err.Error())
			return
		}
		ctx := req.Context()
		env := b.env
		if env == "" {
			env = p.Environment
		}
		r.Sink.Heartbeat(ctx, p.Service, env, r.now())
		if rs, ok := r.Sink.(ReleaseSink); ok && b.release != "" {
			rs.Release(ctx, p.Service, env, b.release, r.now())
		}
		if len(b.records) > 0 {
			r.Sink.Records(ctx, b.records)
		}
		if len(b.metrics) > 0 {
			r.Sink.Metrics(ctx, b.metrics)
		}
		if len(b.traces) > 0 && string(b.traces) != "null" {
			if err := r.clientTraces(ctx, b.traces, true, p, env, b.release, b.traceAttrs); err != nil {
				r.log().Warn("client traces unreadable", "service", p.Service, "err", err)
			}
		}
		w.WriteHeader(http.StatusAccepted)
	}
}

// clientTraces stores OTLP spans sent with a client key, under the key's
// service; extra attributes (session, user, browser) go on every span.
func (r *Receiver) clientTraces(ctx context.Context, body []byte, isJSON bool, p SentryProject, env, release string, extra map[string]string) error {
	var req tracepb.TracesData
	if err := unmarshal(body, isJSON, &req); err != nil {
		return err
	}
	return r.tracesData(ctx, &req, func(ri *resInfo) {
		ri.service = p.Service
		if ri.env == "" {
			ri.env = env
		}
		if ri.env == "" {
			ri.env = p.Environment
		}
		if ri.version == "" {
			ri.version = release
		}
		for k, v := range extra {
			if _, ok := ri.attrs[k]; !ok {
				ri.attrs[k] = v
			}
		}
	})
}

// traceIDs normalizes a trace id to 32 lowercase hex characters (accepting
// UUID-style dashes) and a span id to 16; anything else is dropped.
func traceIDs(trace, span string) (string, string) {
	norm := func(s string, n int) string {
		s = strings.ToLower(strings.ReplaceAll(strings.TrimSpace(s), "-", ""))
		if len(s) != n {
			return ""
		}
		for _, c := range s {
			if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
				return ""
			}
		}
		if strings.Trim(s, "0") == "" {
			return ""
		}
		return s
	}
	return norm(trace, 32), norm(span, 16)
}

// fromTraceparent reads a W3C traceparent ("00-<trace>-<span>-<flags>").
func fromTraceparent(tp string) (string, string) {
	parts := strings.Split(strings.TrimSpace(tp), "-")
	if len(parts) != 4 {
		return "", ""
	}
	return traceIDs(parts[1], parts[2])
}

// --- JSON logs (and the Datadog Browser SDK) -------------------------------

const maxClientEntries = 5000

// jsonEntries reads an object, an array of objects, or one object per line.
func jsonEntries(body []byte) ([]map[string]any, error) {
	t := bytes.TrimSpace(body)
	if len(t) == 0 {
		return nil, nil
	}
	if t[0] == '[' {
		var arr []map[string]any
		if err := json.Unmarshal(t, &arr); err != nil {
			return nil, fmt.Errorf("not a JSON array of objects: %v", err)
		}
		return arr, nil
	}
	var out []map[string]any
	dec := json.NewDecoder(bytes.NewReader(t))
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			return out, fmt.Errorf("not JSON objects: %v", err)
		}
		out = append(out, m)
		if len(out) > maxClientEntries {
			return nil, fmt.Errorf("more than %d entries in one request", maxClientEntries)
		}
	}
	return out, nil
}

func pick(m map[string]any, keys ...string) (string, string) {
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s := anyText(v); s != "" {
				return k, s
			}
		}
	}
	return "", ""
}

// clientTime reads a timestamp: RFC 3339, or epoch milliseconds/seconds.
func clientTime(v any, now time.Time) time.Time {
	switch x := v.(type) {
	case float64:
		switch {
		case x > 1e17: // nanoseconds
			return time.Unix(0, int64(x)).UTC()
		case x > 1e11: // milliseconds
			return time.UnixMilli(int64(x)).UTC()
		case x > 0:
			sec := int64(x)
			return time.Unix(sec, int64((x-float64(sec))*1e9)).UTC()
		}
	case string:
		if t, err := time.Parse(time.RFC3339Nano, x); err == nil {
			return t.UTC()
		}
		if f, err := strconv.ParseFloat(x, 64); err == nil {
			return clientTime(f, now)
		}
	}
	return now.UTC()
}

// flattenAttrs turns nested objects into dot keys, at most 3 levels deep;
// deeper values and arrays stay JSON.
func flattenAttrs(out map[string]string, prefix string, m map[string]any, depth int) {
	for k, v := range m {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		if sub, ok := v.(map[string]any); ok && depth < 3 {
			flattenAttrs(out, key, sub, depth+1)
			continue
		}
		if s := anyText(v); s != "" && len(out) < 200 {
			if len(s) > 4000 {
				s = s[:4000]
			}
			out[key] = s
		}
	}
}

var messageKeys = []string{"message", "msg", "body", "text", "log", "event"}
var levelKeys = []string{"level", "severity", "status", "lvl", "loglevel", "severity_text"}
var timeKeys = []string{"timestamp", "time", "ts", "date", "@timestamp"}

// parseLogsBody reads JSON log entries: the usual message/level/time field
// names, an `error` object or `stack` for errors, everything else as
// attributes. It is also what the Datadog Browser SDK sends to its proxy.
func parseLogsBody(p SentryProject, body []byte, now time.Time) (clientBatch, error) {
	entries, err := jsonEntries(body)
	if err != nil {
		return clientBatch{}, err
	}
	var b clientBatch
	for _, m := range entries {
		msgKey, msg := pick(m, messageKeys...)
		lvlKey, lvl := pick(m, levelKeys...)
		var ts time.Time
		timeKey := ""
		for _, k := range timeKeys {
			if v, ok := m[k]; ok {
				ts, timeKey = clientTime(v, now), k
				break
			}
		}
		if ts.IsZero() {
			ts = now.UTC()
		}
		_, env := pick(m, "environment", "env", "deployment.environment")
		_, release := pick(m, "release", "version", "service.version")
		_, traceID := pick(m, "trace_id", "traceId", "traceID", "trace.id")
		_, spanID := pick(m, "span_id", "spanId", "spanID", "span.id")
		traceID, spanID = traceIDs(traceID, spanID)
		if _, tp := pick(m, "traceparent"); tp != "" && traceID == "" {
			traceID, spanID = fromTraceparent(tp)
		}
		if env != "" {
			b.env = env
		}
		if release != "" {
			b.release = release
		}
		attrs := map[string]string{}
		rest := map[string]any{}
		for k, v := range m {
			switch k {
			case msgKey, lvlKey, timeKey, "service":
			default:
				rest[k] = v
			}
		}
		flattenAttrs(attrs, "", rest, 0)
		sev, sevText := sentrySeverity(lvl)
		if lvl == "" {
			sev, sevText = 9, "INFO"
		}
		// Errors: an error object (Datadog: error.kind/message/stack) or a stack.
		var excType, excMsg, stack string
		if e, ok := m["error"].(map[string]any); ok {
			_, excType = pick(e, "kind", "type", "name")
			_, excMsg = pick(e, "message", "msg")
			_, stack = pick(e, "stack", "stacktrace")
		} else if s, ok := m["error"].(string); ok {
			excMsg = s
		}
		if stack == "" {
			_, stack = pick(m, "stack", "stacktrace", "exception.stacktrace")
		}
		if msg == "" {
			msg = excMsg
		}
		if msg == "" {
			raw, _ := json.Marshal(m)
			msg = string(raw)
		}
		env2 := env
		if env2 == "" {
			env2 = p.Environment
		}
		rec := Record{Log: store.LogRecord{Time: ts, Service: p.Service, Env: env2, Kind: "log", Severity: sev, SeverityText: sevText,
			Body: msg, TraceID: traceID, SpanID: spanID, Attrs: attrs}}
		if sev >= 17 || excType != "" || stack != "" {
			if rec.Log.Severity < 17 {
				rec.Log.Severity, rec.Log.SeverityText, sevText = 17, "ERROR", "ERROR"
			}
			rec.Error = &event.Event{Kind: event.KindLog, Service: p.Service, Environment: env2, Release: release, Time: ts,
				Severity: sevText, Message: msg, ExcType: excType, ExcMessage: excMsg, Stacktrace: stack,
				TraceID: traceID, SpanID: spanID, Attributes: attrs}
		}
		b.records = append(b.records, rec)
	}
	return b, nil
}

// --- Grafana Faro ------------------------------------------------------------

type faroPayload struct {
	Meta struct {
		App struct {
			Name        string `json:"name"`
			Version     string `json:"version"`
			Release     string `json:"release"`
			Environment string `json:"environment"`
		} `json:"app"`
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
		User struct {
			ID       string `json:"id"`
			Email    string `json:"email"`
			Username string `json:"username"`
		} `json:"user"`
		Page struct {
			URL string `json:"url"`
		} `json:"page"`
		View struct {
			Name string `json:"name"`
		} `json:"view"`
		Browser struct {
			Name    string `json:"name"`
			Version string `json:"version"`
			OS      string `json:"os"`
			Mobile  bool   `json:"mobile"`
		} `json:"browser"`
	} `json:"meta"`
	Logs []struct {
		Message   string         `json:"message"`
		Level     string         `json:"level"`
		Timestamp string         `json:"timestamp"`
		Context   map[string]any `json:"context"`
		Trace     *faroTrace     `json:"trace"`
	} `json:"logs"`
	Exceptions []struct {
		Type       string `json:"type"`
		Value      string `json:"value"`
		Timestamp  string `json:"timestamp"`
		Stacktrace *struct {
			Frames []struct {
				Filename string `json:"filename"`
				Function string `json:"function"`
				Lineno   int    `json:"lineno"`
				Colno    int    `json:"colno"`
			} `json:"frames"`
		} `json:"stacktrace"`
		Context map[string]any `json:"context"`
		Trace   *faroTrace     `json:"trace"`
	} `json:"exceptions"`
	Measurements []struct {
		Type      string             `json:"type"`
		Values    map[string]float64 `json:"values"`
		Timestamp string             `json:"timestamp"`
	} `json:"measurements"`
	Events []struct {
		Name       string         `json:"name"`
		Domain     string         `json:"domain"`
		Attributes map[string]any `json:"attributes"`
		Timestamp  string         `json:"timestamp"`
	} `json:"events"`
	// Traces is OTLP JSON from @grafana/faro-web-tracing.
	Traces json.RawMessage `json:"traces"`
}

type faroTrace struct {
	TraceID string `json:"trace_id"`
	SpanID  string `json:"span_id"`
}

// faroLevels: Faro also uses "log" and "trace".
func faroSeverity(level string) (int32, string) {
	switch strings.ToLower(level) {
	case "log", "":
		return 9, "INFO"
	case "warn":
		return 13, "WARN"
	}
	return sentrySeverity(level)
}

// pagePath is the page without its query (cardinality, and query strings
// can carry tokens).
func pagePath(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return ""
	}
	return u.Path
}

// parseFaro converts a Faro payload: logs and exceptions into log lines
// (exceptions into issues), measurements (Web Vitals) into metrics, events
// into debug log lines.
func parseFaro(p SentryProject, body []byte, now time.Time) (clientBatch, error) {
	var f faroPayload
	if err := json.Unmarshal(body, &f); err != nil {
		return clientBatch{}, fmt.Errorf("not a Faro payload: %v", err)
	}
	var b clientBatch
	env := f.Meta.App.Environment
	if env == "" {
		env = p.Environment
	}
	b.env = env
	b.release = f.Meta.App.Version
	if b.release == "" {
		b.release = f.Meta.App.Release
	}
	base := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			base[k] = v
		}
	}
	set("app.name", f.Meta.App.Name)
	set("session.id", f.Meta.Session.ID)
	set("user.id", f.Meta.User.ID)
	set("user.email", f.Meta.User.Email)
	set("user.username", f.Meta.User.Username)
	set("page.path", pagePath(f.Meta.Page.URL))
	set("view.name", f.Meta.View.Name)
	set("browser.name", f.Meta.Browser.Name)
	set("browser.version", f.Meta.Browser.Version)
	set("browser.os", f.Meta.Browser.OS)
	if f.Meta.Browser.Mobile {
		base["browser.mobile"] = "true"
	}
	withCtx := func(ctx map[string]any) map[string]string {
		a := mergeAttrs(base)
		flattenAttrs(a, "context", ctx, 1)
		return a
	}
	ids := func(t *faroTrace) (string, string) {
		if t == nil {
			return "", ""
		}
		return t.TraceID, t.SpanID
	}
	for _, l := range f.Logs {
		sev, sevText := faroSeverity(l.Level)
		ts := clientTime(l.Timestamp, now)
		tid, sid := ids(l.Trace)
		attrs := withCtx(l.Context)
		rec := Record{Log: store.LogRecord{Time: ts, Service: p.Service, Env: env, Kind: "log", Severity: sev, SeverityText: sevText,
			Body: l.Message, TraceID: tid, SpanID: sid, Attrs: attrs}}
		if sev >= 17 {
			rec.Error = &event.Event{Kind: event.KindLog, Service: p.Service, Environment: env, Release: b.release, Time: ts,
				Severity: sevText, Message: l.Message, TraceID: tid, SpanID: sid, Attributes: attrs}
		}
		b.records = append(b.records, rec)
	}
	for _, x := range f.Exceptions {
		ts := clientTime(x.Timestamp, now)
		tid, sid := ids(x.Trace)
		var sb strings.Builder
		if x.Stacktrace != nil {
			// Faro lists frames as the browser's stack does: newest first.
			for _, fr := range x.Stacktrace.Frames {
				fn := fr.Function
				if fn == "" || fn == "?" {
					fn = "<anonymous>"
				}
				loc := fr.Filename
				if fr.Lineno > 0 {
					loc += ":" + strconv.Itoa(fr.Lineno)
					if fr.Colno > 0 {
						loc += ":" + strconv.Itoa(fr.Colno)
					}
				}
				fmt.Fprintf(&sb, "at %s (%s)\n", fn, loc)
			}
		}
		stack := strings.TrimRight(sb.String(), "\n")
		attrs := withCtx(x.Context)
		attrs["exception.type"], attrs["exception.message"] = x.Type, x.Value
		if stack != "" {
			attrs["exception.stacktrace"] = stack
		}
		e := event.Event{Kind: event.KindLog, Service: p.Service, Environment: env, Release: b.release, Time: ts, Severity: "ERROR",
			ExcType: x.Type, ExcMessage: x.Value, Stacktrace: stack, TraceID: tid, SpanID: sid, Attributes: attrs}
		b.records = append(b.records, Record{Error: &e, Log: store.LogRecord{Time: ts, Service: p.Service, Env: env, Kind: "log",
			Severity: 17, SeverityText: "ERROR", Body: e.Summary(), TraceID: tid, SpanID: sid, Attrs: attrs}})
	}
	// Spans (page loads, fetches) join the backend's spans through the W3C
	// traceparent header the tracing instrumentation adds to requests.
	b.traces, b.traceAttrs = f.Traces, base
	page := pagePath(f.Meta.Page.URL)
	for _, m := range f.Measurements {
		ts := clientTime(m.Timestamp, now)
		names := make([]string, 0, len(m.Values))
		for k := range m.Values {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			if strings.EqualFold(k, "delta") {
				continue // the change since the last report of the same vital
			}
			typ := m.Type
			if typ == "" {
				typ = "measurement"
			}
			unit := ""
			switch lk := strings.ToLower(k); {
			case lk == "lcp" || lk == "fcp" || lk == "fid" || lk == "inp" || lk == "ttfb" || strings.HasSuffix(lk, "_duration") || strings.HasSuffix(lk, "_time"):
				unit = "ms"
			}
			attrs := ""
			if page != "" {
				attrs = "page=" + strings.ReplaceAll(page, ",", "%2C")
			}
			b.metrics = append(b.metrics, store.MetricPoint{Time: ts, Service: p.Service, Name: strings.ReplaceAll(typ, "-", "_") + "." + strings.ToLower(k),
				Unit: unit, Kind: "gauge", Attrs: attrs, Value: m.Values[k]})
		}
	}
	for _, e := range f.Events {
		attrs := mergeAttrs(base)
		flattenAttrs(attrs, "event", e.Attributes, 1)
		set := e.Domain
		if set != "" {
			attrs["event.domain"] = set
		}
		b.records = append(b.records, Record{Log: store.LogRecord{Time: clientTime(e.Timestamp, now), Service: p.Service, Env: env, Kind: "log",
			Severity: 5, SeverityText: "DEBUG", Body: "event: " + e.Name, Attrs: attrs}})
	}
	return b, nil
}
