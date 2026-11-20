# Sending telemetry

Sentinel accepts OTLP from any OpenTelemetry SDK or Collector, events from Sentry SDKs, and logs from Grafana Faro, Datadog Browser Logs or plain JSON.

## Pointing an OpenTelemetry Collector at Sentinel

Add Sentinel as an extra `otlphttp` exporter next to your existing backend:

```yaml
receivers:
  otlp:
    protocols:
      grpc:
      http:

processors:
  batch:

exporters:
  otlphttp/sentinel:
    endpoint: https://sentinel.example.com   # the exporter appends /v1/logs, /v1/traces, /v1/metrics
    headers:
      Authorization: "Bearer ${env:SENTINEL_INGEST_TOKEN}"
    compression: gzip
    # encoding: json                 # protobuf (default) and json both work

service:
  pipelines:
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/sentinel]
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/sentinel]
    metrics:                          # optional: heartbeats for outage detection
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/sentinel]
```

Make sure your services set `service.name` (and ideally
`deployment.environment.name`) as resource attributes. To keep Sentinel's
input small you can filter to errors in the Collector (e.g. a `filter`
processor on `severity_number < SEVERITY_NUMBER_ERROR`), but then also send
metrics so outage detection still sees a heartbeat.

SDKs can also export straight to Sentinel with
`OTEL_EXPORTER_OTLP_ENDPOINT=http://sentinel:4318` and
`OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf`.


## Structured log lines (JSON and logfmt)

Log lines whose body is a JSON object or **logfmt** (`key=value` pairs with
quoted values, as Grafana Alloy, Loki, Prometheus, Go's slog text handler
and logrus write) are read as fields:

- When the record has no severity of its own, the line's `level=` (or
  `"level"`, `severity`, `lvl`) sets it, so `level=error` lines count as
  errors.
- The issue for such an error is titled by its `msg`/`message`; the other
  fields (`error`, ids, counts, timestamps) become its attributes, so lines
  that differ only in those group together. The stored line is unchanged.
- On the Logs page they show their message field (chosen per service, or
  the usual names), and the log drawer lists every field.

## Sentry SDKs (drop-in)

Apps that already use a Sentry SDK keep it: create a DSN for the service
under **Settings → Integrations** and put it where the Sentry DSN was
(`https://<key>@SENTINEL_DOMAIN/sentry/<id>`). Nothing else changes. The SDK
posts to `/sentry/api/<id>/envelope/` (or the older `/store/`), which
Sentinel serves on the dashboard port (so it works through Caddy unchanged)
and on the OTLP port.

- **Errors** (exceptions, and messages at level error/fatal) become issues
  and log lines: exception type and value, the stack trace (in-app frames,
  newest first), tags, user, request, release, environment, trace id and a
  custom `fingerprint` are kept. Info and warning messages become log lines.
- **Transactions** become traces (the transaction and its spans), so
  performance, slow-endpoint and N+1 detection work too.
- **Sentry logs** (`enable_logs`) become log lines with their attributes.
- Sessions, client reports, replays, profiles, check-ins and attachments are
  accepted and dropped. Bodies may be gzip or deflate compressed; the Python
  SDK uses brotli when the `brotli` package is installed, so set
  `transport_compression_algo` to `gzip` there.

Each DSN belongs to one service; an optional default environment is used
for events without one. Replacing a DSN's key or deleting it refuses apps
with the old one. Tested with the official Python SDK (2.19 and 2.75).

## Frontend and app logs

The same per-service key a Sentry DSN carries (Settings → Integrations →
App and browser keys) also works for:

- **Grafana Faro** (`@grafana/faro-web-sdk`): set
  `url: "https://SENTINEL_DOMAIN/ingest/<id>/<key>/faro"`. Console logs,
  uncaught errors and rejections become log lines and issues (with browser,
  page path, session and user); Web Vitals become metrics per page path;
  page and session events become debug log lines.
- **Datadog Browser Logs** (`@datadog/browser-logs`): keep the SDK and set
  `proxy: "https://SENTINEL_DOMAIN/ingest/<id>/<key>/datadog"`
  (`clientToken` is ignored). Logs, console and network errors arrive with
  their view, user and error details.
- **Plain JSON** at `/ingest/<id>/<key>/logs`: one object, an array, or one
  object per line, from `fetch`, `navigator.sendBeacon`, logger transports,
  Vector or Fluent Bit. The usual field names are understood (`message` or
  `msg`, `level`/`severity`/`status`, `timestamp`/`time`/`ts` as RFC 3339 or
  epoch); an `error` object (`name`/`kind`, `message`, `stack`) or a `stack`
  makes it an issue; everything else becomes attributes.

Keys are public (they ship in frontend code) and can only send telemetry.
Set **allowed origins** on a key (`https://shop.example.com`,
`https://*.example.com`) so browsers on other sites are refused; requests
without an `Origin` (servers) are still accepted. Tested with Faro 2.13 and
Datadog Browser Logs 7.17 in Chrome.
