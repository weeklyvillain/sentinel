# Following a request through every service

A trace is every span with the same trace id, from any service, whichever
way it arrived (OTLP, a Sentry DSN, Faro). The trace view shows them as one
waterfall with the log lines that carry that trace id, and the Traces page
filters by any span attribute (`session.id=…`, `user.id=…`, `order.id=…`),
which finds every trace of a browser session or user; the log drawer and
span details link there ("traces"). Sampling is decided from the trace id,
so every service keeps or drops the same traces.

Services hand the trace on in HTTP headers. What each SDK sends and reads
(checked in their source):

| SDK | Sends | Continues from |
|---|---|---|
| OpenTelemetry (every language) | W3C `traceparent` | W3C `traceparent` |
| Faro (`@grafana/faro-web-tracing`) | `traceparent` (cross-origin only to `propagateTraceHeaderCorsUrls`) | (starts traces in the browser) |
| Sentry JavaScript | `sentry-trace` + `baggage`; also `traceparent` with `propagateTraceparent: true` | `sentry-trace` |
| Sentry Python | `sentry-trace` + `baggage` | `sentry-trace` |

So a chain links up when each hop speaks the next one's header:
OpenTelemetry to OpenTelemetry always; Sentry to Sentry always; a browser
with Faro tracing, or Sentry JS with `propagateTraceparent: true`, into
OpenTelemetry backends. A Sentry Python backend only joins a trace that
arrives as `sentry-trace`. Mixed chains cannot be stitched afterwards:
two trace ids stay two traces.

Checklist:

- Browser to your API: list the API in `propagateTraceHeaderCorsUrls`
  (Faro) or `tracePropagationTargets` (Sentry), and let the API's CORS allow
  the `traceparent`, `tracestate`, `sentry-trace` and `baggage` headers.
- Backends: use the OpenTelemetry instrumentation for your HTTP server and
  client (or the Sentry SDK's), which read and forward the headers. Reading
  them by hand, match header names case-insensitively.
- Logs: OpenTelemetry log bridges (Python's `LoggingHandler`, Go's
  `otelslog`, …) stamp each log line with the active span, so the trace view
  shows them. JSON logs can send `trace_id`/`span_id` or a `traceparent`.
- Sentry SDKs in OpenTelemetry mode (Python's `OTLPIntegration`) export
  their OTLP traces to the DSN's `…/integration/otlp/v1/traces`, which
  Sentinel serves too.

Tested end to end: a browser with Faro tracing calling two services using
the OpenTelemetry Python SDK showed up as one trace of five spans across
the three services, with the log lines of both backends.
