package ingest

import (
	"strings"
	"testing"
)

const alloyLine = `ts=2026-01-15T09:30:00.000000000Z level=error msg="Exporting failed. Dropping data." component_path=/ component_id=otelcol.exporter.otlphttp.sentinel error="not retryable error: Permanent error: rpc error: code = Unauthenticated desc = error exporting items, request to https://sentinel.contoso.dev/v1/logs responded with HTTP Status Code 401" dropped_items=3`

func TestParseLogfmt(t *testing.T) {
	pairs, ok := ParseLogfmt(alloyLine)
	if !ok || len(pairs) != 7 {
		t.Fatalf("alloy line: %v %+v", ok, pairs)
	}
	m := map[string]string{}
	for _, kv := range pairs {
		m[kv[0]] = kv[1]
	}
	if m["level"] != "error" || m["msg"] != "Exporting failed. Dropping data." || m["component_path"] != "/" || m["dropped_items"] != "3" ||
		!strings.HasPrefix(m["error"], "not retryable error") || !strings.HasSuffix(m["error"], "HTTP Status Code 401") {
		t.Fatalf("fields: %+v", m)
	}
	if p, ok := ParseLogfmt(`msg="say \"hi\"\nnow" n=1`); !ok || p[0][1] != "say \"hi\"\nnow" {
		t.Fatalf("escapes: %v %+v", ok, p)
	}
	for _, notLogfmt := range []string{
		"plain text line: cache warmed in 120ms",
		"user logged in with id=7",      // text with one pair
		"GET /cart?x=1&y=2 200",         // a URL
		`level=error msg="unterminated`, // broken quoting
		`a=1 "quoted" b=2`,              // stray quote
	} {
		if _, ok := ParseLogfmt(notLogfmt); ok {
			t.Errorf("%q parsed as logfmt", notLogfmt)
		}
	}
}

func TestBodyLevel(t *testing.T) {
	for body, want := range map[string]string{
		alloyLine:                               "error",
		`time=x lvl=warn msg=retrying`:          "warn",
		`{"level":"error","msg":"boom"}`:        "error",
		`{"severity":"WARNING","message":"x"}`:  "WARNING",
		`plain text with level=error inside it`: "",
		`{"msg":"no level"}`:                    "",
	} {
		if got := BodyLevel(body); got != want {
			t.Errorf("%q: %q, want %q", body, got, want)
		}
	}
}

// An OTLP record without severity whose logfmt body says level=error is an
// error; one with its own severity keeps it.
func TestLogfmtLevelAtIngest(t *testing.T) {
	srv, s := server(t)
	body := func(sev, text, line string) string {
		return `{"resourceLogs":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"alloy"}}]},"scopeLogs":[{"logRecords":[{"timeUnixNano":"1768469400000000000",` +
			sev + `"body":{"stringValue":` + text + `}}]}]}]}`
	}
	esc := strings.NewReplacer(`\`, `\`, `"`, `\"`)
	post(t, srv.URL+"/v1/logs", []byte(body("", `"`+esc.Replace(alloyLine)+`"`, "")), map[string]string{"Content-Type": "application/json"})
	if len(s.logs) != 1 || s.logs[0].Severity != 17 || s.logs[0].SeverityText != "ERROR" || len(s.events) != 1 {
		t.Fatalf("logfmt error: %+v events %d", s.logs, len(s.events))
	}
	// The issue is about msg; the rest are attributes; the stored line is unchanged.
	if e := s.events[0]; e.Summary() != "Exporting failed. Dropping data." || e.Attributes["component_id"] != "otelcol.exporter.otlphttp.sentinel" ||
		!strings.HasSuffix(e.Attributes["error"], "HTTP Status Code 401") || s.logs[0].Body != alloyLine {
		t.Fatalf("event: %q %+v", e.Summary(), e.Attributes)
	}
	post(t, srv.URL+"/v1/logs", []byte(body(`"severityNumber":9,"severityText":"INFO",`, `"`+esc.Replace(alloyLine)+`"`, "")), map[string]string{"Content-Type": "application/json"})
	if len(s.logs) != 2 || s.logs[1].Severity != 9 || len(s.events) != 1 {
		t.Fatalf("record severity must win: %+v events %d", s.logs[1], len(s.events))
	}
}
