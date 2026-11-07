# Sentinel

Sentinel receives OpenTelemetry (OTLP/HTTP) logs, traces and metrics, finds
errors and service outages, groups them deterministically, and files **one
GitHub issue per distinct problem** instead of one per occurrence. A small AI
model writes the issue title and analysis; every fact in the issue (counts,
timestamps, trace IDs, samples) is assembled by Sentinel itself.

```
app ──OTLP──▶ Collector ──otlphttp──▶ Sentinel :4318 ──▶ SQLite ──▶ reporter loop ──▶ GitHub issues
```

## How it works

1. **Ingest** – `POST /v1/logs`, `/v1/traces`, `/v1/metrics` accept protobuf
   (`application/x-protobuf`) or JSON (`application/json`), optionally gzip
   compressed, and reply with an empty `Export*ServiceResponse`.
   - Error **logs**: `severity_number >= MIN_SEVERITY` (default 17 = ERROR), an
     error-like `severity_text` (`error`, `fatal`, `critical`, `panic`, …), or any
     `exception.*` attribute.
   - Error **spans**: status `ERROR`, or an `exception` span event.
   - **Metrics** only count as heartbeats (for outage detection). Every signal
     is a heartbeat for its `service.name`.
2. **Redaction** – bearer/basic tokens, `password=` / `secret=` / `api_key=` /
   `token=` style values, AWS keys, JWTs, GitHub/Slack tokens and URL
   credentials are replaced with `[REDACTED]` *before* anything is stored, sent
   to a model or posted to GitHub. Attributes whose key looks secret
   (`authorization`, `cookie`, `*password*`, …) are dropped wholesale.
3. **Grouping** – `fingerprint = sha256(service, kind, exception.type, X)`
   where X is the top 5 stack frames with line numbers, hex addresses,
   goroutine ids and Go argument lists stripped, or (with no stack) the message
   with UUIDs, hex ids, IPs, emails, numbers and quoted strings replaced by
   placeholders. Set the attribute `sentinel.fingerprint` on a log/span to
   choose the group yourself.
4. **Reporter** (every `TICK_INTERVAL`):
   - A *pending* group whose first event is older than `SETTLE_WINDOW` and that
     has at least `MIN_EVENTS` events gets an issue. Before creating it, GitHub
     is searched for the hidden marker `<!-- sentinel:fingerprint=… -->`, so a
     lost database does not create duplicates.
   - Open groups with new events get **at most one** update comment per
     `UPDATE_INTERVAL` (new count, total, latest sample).
   - If a human closed the issue as *completed* and the error comes back, it is
     reopened with a **Regression** comment. Closed as *not planned* → the group
     is ignored and Sentinel stays quiet.
   - At most `MAX_ISSUES_PER_HOUR` new issues per rolling hour; the rest stay
     pending until there is room.
5. **Service down** – a service that was seen and then sends nothing for
   `DOWN_AFTER` gets a synthetic `down` event that goes through the same
   grouping (down groups skip the settle window and `MIN_EVENTS`). After a
   Sentinel restart there is a grace period of `DOWN_AFTER` before anything can
   be flagged. When the service sends again, Sentinel comments the outage
   length and closes the issue; a later outage reopens the same issue.

## Running

```bash
go build -o bin/sentinel ./cmd/sentinel
GITHUB_TOKEN=ghp_... GITHUB_REPO=acme/ops ANTHROPIC_API_KEY=sk-ant-... ./bin/sentinel
```

Try it without GitHub or an AI key:

```bash
DRY_RUN=true AI_PROVIDER=none ./bin/sentinel
```

Debug endpoints (on the OTLP port unless `SENTINEL_HTTP_ADDR` is set):
`GET /healthz`, `GET /groups` (JSON view of groups and services).

### End-to-end check

```bash
./scripts/e2e-dryrun.sh
```

Builds the binary, runs it in `DRY_RUN`, POSTs a burst of 50 OTLP/JSON error
logs with different order ids, emails, IPs, durations and secrets, and checks
that they became one group and one (logged) issue with no secret in the
output. Use `PORT=14318` if 4318 is taken.

## Configuration

All configuration is via environment variables. Durations accept Go syntax
(`90s`, `5m`) or plain seconds.

| Variable | Default | Meaning |
|---|---|---|
| `SENTINEL_OTLP_ADDR` | `:4318` | OTLP/HTTP listen address |
| `SENTINEL_HTTP_ADDR` | *(empty)* | Separate address for `/healthz` and `/groups`; empty = same as OTLP |
| `SENTINEL_DB` | `sentinel.db` | SQLite database path |
| `MIN_SEVERITY` | `17` | Lowest log `severity_number` treated as an error |
| `TICK_INTERVAL` | `10s` | Reporter loop interval |
| `SETTLE_WINDOW` | `60s` | Wait after a group's first event before filing |
| `MIN_EVENTS` | `1` | Minimum events before a group is filed |
| `UPDATE_INTERVAL` | `30m` | At most one update comment per issue per interval |
| `MAX_ISSUES_PER_HOUR` | `10` | Cap on new issues per rolling hour (`-1` = unlimited) |
| `DOWN_AFTER` | `5m` | Silence before a service counts as down; `0` disables |
| `GITHUB_TOKEN` | – | Token with `issues: write` on the repo (required unless `DRY_RUN`) |
| `GITHUB_REPO` | – | `owner/name` (required unless `DRY_RUN`) |
| `GITHUB_LABELS` | – | Comma-separated labels for new issues, e.g. `sentinel,bug` |
| `GITHUB_API_URL` | `https://api.github.com` | For GitHub Enterprise |
| `DRY_RUN` | `false` | Log GitHub operations instead of performing them |
| `AI_PROVIDER` | `anthropic` | `anthropic`, `openai` (any OpenAI-compatible server) or `none` |
| `AI_MODEL` | `claude-haiku-5-5` for anthropic | Model name; required for `openai` |
| `ANTHROPIC_API_KEY` | – | Anthropic key (the SDK's other credential sources also work) |
| `OPENAI_BASE_URL` | `http://localhost:11434/v1` | OpenAI-compatible base URL (Ollama default) |
| `OPENAI_API_KEY` | – | Optional bearer token for the OpenAI-compatible server |
| `AI_TIMEOUT` | `60s` | Per-draft timeout |

### AI providers

- **anthropic** (default): Claude Haiku 5.5 via the official Go SDK, using
  structured JSON output and low effort. Refusals and API errors fall back to
  the template.
- **openai**: any `/v1/chat/completions` server with JSON mode, e.g.
  Ollama (`AI_PROVIDER=openai AI_MODEL=llama3.1`) or llama.cpp
  (`OPENAI_BASE_URL=http://localhost:8080/v1`).
- **none**: a deterministic template, no model calls.

The model only writes the title, summary, likely cause and next steps. Its
prompt marks all telemetry as untrusted data, and its output is redacted and
has `@mentions` neutralized before it is posted.

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
    endpoint: http://sentinel:4318   # the exporter appends /v1/logs, /v1/traces, /v1/metrics
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

## Development

```bash
go test ./...
go vet ./...
```

Layout: `internal/ingest` (OTLP receiver), `internal/redact`,
`internal/fingerprint`, `internal/store` (SQLite, WAL, single connection),
`internal/reporter` (issue lifecycle and down detection), `internal/ai`
(Anthropic, OpenAI-compatible, template), `internal/github` (REST client and
dry-run), `cmd/sentinel`.
