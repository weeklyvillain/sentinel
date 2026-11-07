# Sentinel

Sentinel receives OpenTelemetry (OTLP/HTTP) logs, traces and metrics, finds
errors and service outages, groups them deterministically, and files **one
GitHub issue per distinct problem** instead of one per occurrence. A small AI
model writes the issue title and analysis; every fact in the issue (counts,
timestamps, trace IDs, samples) is assembled by Sentinel itself.

```
app ──OTLP──▶ Collector ──otlphttp──▶ Sentinel :4318 ──▶ SQLite ──▶ reporter loop ──▶ GitHub issues
                                                              ├──▶ alarms ──▶ webhooks
                                                              └──▶ dashboard :8080
```

## How it works

1. **Ingest** – `POST /v1/logs`, `/v1/traces`, `/v1/metrics` accept protobuf
   (`application/x-protobuf`) or JSON (`application/json`), optionally gzip
   compressed, and reply with an empty `Export*ServiceResponse`.
   - Error **logs**: `severity_number >= MIN_SEVERITY` (default 17 = ERROR), an
     error-like `severity_text` (`error`, `fatal`, `critical`, `panic`, …), or any
     `exception.*` attribute.
   - Error **spans**: status `ERROR`, or an `exception` span event.
   - Every log line is stored (redacted) for search; error spans are stored as
     log lines too. **Metrics** are stored for charts and alarms. Every signal
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

## Dashboard

Sentinel serves a dashboard on `SENTINEL_HTTP_ADDR` (default `:8080`, separate
from the OTLP port):

- **Overview** – services up/down, open issues, errors by service, log volume
  by level, firing alarms.
- **Logs** – full-text search over every log line (and error span), filters
  for service, level, time range, trace id, attributes and "same error";
  a clickable volume histogram, live tail and pagination. Query syntax: words
  are prefix-matched and ANDed, `"exact phrase"`, `-exclude`.
- **Issues** – error groups by status with their GitHub issue, occurrence
  chart, first/latest samples with stack traces, ignore / un-ignore, and the
  reason a group is still pending (e.g. no repository configured).
- **Metrics** – charts for every OTLP metric a service reports (gauges as-is,
  cumulative counters as per-interval deltas, histograms as interval means),
  with avg/sum/max/min/count and per-attribute split.
- **Services** – discovered automatically from `service.name`; add a service
  ahead of time, choose which repository its issues go to and extra labels.
- **Alarms** – rules evaluated every 15s: *log count* (matching text, level,
  service, window), *metric threshold* (aggregate over a window) and *service
  down*. A preview shows the current value; state changes are kept as history.
- **Uptime** – HTTP checks run from the Sentinel server (see below).
- **Webhooks** – Slack, Discord or generic JSON targets for `issue.created`,
  `issue.regression`, `issue.updated`, `service.down`, `service.recovered`,
  `check.down`, `check.recovered`
  (alarm notifications go to the webhooks chosen on each alarm). JSON
  deliveries carry `X-Sentinel-Event` and, with a secret,
  `X-Sentinel-Signature: sha256=<HMAC-SHA256(secret, body)>`. Failures (network,
  429, 5xx) are retried twice and every delivery is logged in the UI.
- **GitHub** – install the GitHub App, sync repositories, authorize the
  repositories Sentinel may create issues in, and pick a default repository.

Logs and metric points are kept for `LOG_RETENTION` / `METRIC_RETENTION`
(default 7 days) and pruned every 10 minutes. Everything is redacted before
it is stored.

### Signing in

`DASHBOARD_AUTH=github` (the default) uses "Sign in with GitHub" through the
GitHub App's OAuth credentials. Only users in `ALLOWED_USERS` or active members
of `ALLOWED_ORG` get in; one of the two is required, so Sentinel fails closed.
Sessions are HttpOnly cookies (7 days). State-changing API calls require an
`X-Sentinel-Request` header, which cross-site pages cannot send.
`DASHBOARD_AUTH=none` disables sign-in and is for local use only.

### GitHub App setup

1. Create a GitHub App (Settings → Developer settings → GitHub Apps → New
   GitHub App) on your user or organization.
2. **Callback URL**: `$PUBLIC_URL/auth/callback`. **Setup URL**:
   `$PUBLIC_URL/github/setup` with "Redirect on update" ticked. Webhook: not
   needed (untick "Active").
3. Permissions. Repository: **Issues: Read and write**, **Metadata: Read-only**.
   Organization: **Members: Read-only** (only needed for `ALLOWED_ORG`).
4. Generate a private key and a client secret, then start Sentinel with
   `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY_FILE`, `GITHUB_APP_CLIENT_ID`,
   `GITHUB_APP_CLIENT_SECRET` and `PUBLIC_URL`.
5. In the dashboard: **GitHub → Install on GitHub**, choose repositories,
   come back, and switch on the repositories Sentinel may file issues in.
   Then set a default repository and/or a repository per service.

Issues are routed: service repository → dashboard default → `GITHUB_REPO`.
With the app, the repository must be visible to an installation *and*
authorized in the dashboard; otherwise the group stays pending and the Issues
page says why. Installation tokens are short-lived and minted on demand.

`GITHUB_TOKEN` + `GITHUB_REPO` (a personal access token) still work when no
app is configured; the dashboard then cannot restrict repositories.

## Running

```bash
npm --prefix web ci && npm --prefix web run build   # only after changing the UI; the build is committed
go build -o bin/sentinel ./cmd/sentinel
GITHUB_APP_ID=123456 GITHUB_APP_PRIVATE_KEY_FILE=sentinel.pem \
GITHUB_APP_CLIENT_ID=Iv1.abc GITHUB_APP_CLIENT_SECRET=... \
PUBLIC_URL=https://sentinel.example.com ALLOWED_ORG=acme \
ANTHROPIC_API_KEY=sk-ant-... ./bin/sentinel
```

Try it locally without GitHub, sign-in or an AI key, with demo traffic:

```bash
DRY_RUN=true DASHBOARD_AUTH=none AI_PROVIDER=none ./bin/sentinel
```

```bash
go run ./cmd/sentinel-demo -endpoint http://localhost:4318
```

Then open http://localhost:8080. `cmd/sentinel-demo` sends logs (with a
backfilled history), error spans and metrics for four fake services; one of
them stops after 90s to show outage detection.

For UI work, `npm --prefix web run dev` serves the React app with hot reload
and proxies `/api` to a Sentinel on :8080 (`SENTINEL_URL` to change).

### End-to-end check

```bash
./scripts/e2e-dryrun.sh
```

Builds the binary, runs it in `DRY_RUN`, POSTs a burst of 50 OTLP/JSON error
logs with different order ids, emails, IPs, durations and secrets, and checks
that they became one group and one (logged) issue, that all 50 lines are
searchable through the dashboard API, and that no secret is in the output or
the stored logs. Use `PORT=14318 HTTP_PORT=18090` if the defaults are taken.

## Deploying on a separate server

Run Sentinel on its **own** machine, not on the servers it watches. Then an
app server dying, rebooting or losing its network shows up in Sentinel (as a
service going quiet, failing uptime checks, an issue and webhooks) instead of
taking the monitoring down with it.

```
 app server(s)                                     Sentinel server
 ┌──────────────────────────────┐   HTTPS + token   ┌──────────────────────────────┐
 │ apps ─OTLP─▶ otelcol-contrib │ ────────────────▶ │ Caddy :443 ─▶ sentinel       │
 │   (disk-backed retry queue,  │   /v1/logs …      │   /v1/*  → OTLP :4318        │
 │    host metrics)             │                   │   /*     → dashboard :8080   │
 └──────────────────────────────┘  ◀── uptime ───── │   uptime checks, GitHub,     │
                                     checks (HTTP)  │   webhooks (outbound)        │
                                                    └──────────────────────────────┘
```

### 1. Sentinel server (Docker Compose)

```bash
git clone <this repo> sentinel && cd sentinel/deploy
cp .env.example .env            # set SENTINEL_DOMAIN, INGEST_TOKEN, GitHub App, ...
openssl rand -hex 32            # a good INGEST_TOKEN
cp ~/Downloads/your-app.private-key.pem secrets/github-app.pem
docker compose up -d --build
```

- Point DNS for `SENTINEL_DOMAIN` at the server and open ports **80** and
  **443** (Caddy gets a Let's Encrypt certificate automatically). Ports 4318
  and 8080 are not published; everything goes through Caddy.
- Caddy routes `/v1/*` to the OTLP receiver and everything else to the
  dashboard, so collectors and browsers use the same `https://SENTINEL_DOMAIN`.
- `INGEST_TOKEN` is required by the Compose file: collectors must send
  `Authorization: Bearer <token>`. Several comma-separated tokens are accepted,
  so you can rotate without downtime.
- Data lives in the `sentinel-data` volume (`/data/sentinel.db`). Back it up
  with e.g. `docker compose exec` + `sqlite3 .backup`, or snapshot the volume.
- Upgrade: `git pull && docker compose up -d --build`. The schema migrates
  itself on start.
- The image has a `HEALTHCHECK` (`sentinel healthcheck`), so `docker ps` shows
  whether it is healthy.
- Sentinel needs outbound HTTPS for GitHub, the AI provider, webhooks and
  uptime checks.

#### When another proxy already owns 80/443

If the host already runs a reverse proxy, use
`docker-compose.behind-proxy.yml` instead. It runs Sentinel alone and
publishes OTLP on `127.0.0.1:4318` and the dashboard on `127.0.0.1:18080`
(reach it with `ssh -L 18080:localhost:18080 host`). To serve it publicly,
paste [`deploy/Caddyfile.shared-proxy`](deploy/Caddyfile.shared-proxy) into the
existing Caddyfile, set `PROXY_NETWORK` to that proxy's Docker network, and
add `-f docker-compose.proxy-network.yml`. Configure dashboard sign-in first.

### 2. Every app server (OpenTelemetry Collector)

Install **otelcol-contrib** and use
[`deploy/otel-collector-app-server.yaml`](deploy/otel-collector-app-server.yaml)
with:

```bash
SENTINEL_ENDPOINT=https://sentinel.example.com
SENTINEL_INGEST_TOKEN=<the INGEST_TOKEN>
HOST_SERVICE_NAME=host-app-1
```

It:

- receives OTLP from the apps on that machine (`localhost:4317/4318`),
- forwards logs, traces and metrics to Sentinel over HTTPS with gzip,
- keeps a **disk-backed retry queue**, so data produced while Sentinel or the
  network is unreachable is delivered when it comes back,
- sends **host metrics** (CPU, memory, disk, network, load) as a service named
  `HOST_SERVICE_NAME`. Sentinel then shows the server's own charts and flags
  the whole machine as down when it goes quiet.

### 3. Uptime checks

On **Uptime** in the dashboard, add the public health URLs of your apps (e.g.
`https://shop.example.com/healthz`). Sentinel probes them from its own server
on an interval, with an expected status (`2xx`, `200,204`, …) and optional
body text. After N consecutive failures the check is **down**: Sentinel files
(or reopens) an issue for it, in the repository of the service you link it to,
and sends `check.down` to webhooks. When it passes again the issue is closed
with the outage length and `check.recovered` is sent.

Uptime checks and telemetry tell you different things. A failing check with
telemetry still arriving points at DNS, TLS, the proxy or the network.
Telemetry gone *and* checks failing means the server or app is down.
Telemetry gone with checks passing means the collector or network path to
Sentinel broke.

### Who watches Sentinel?

Sentinel cannot report its own outage. Point an external uptime service (or
another Sentinel) at `https://SENTINEL_DOMAIN/healthz`.

## Configuration

All configuration is via environment variables. Durations accept Go syntax
(`90s`, `5m`, `168h`) or plain seconds.

| Variable | Default | Meaning |
|---|---|---|
| `SENTINEL_OTLP_ADDR` | `:4318` | OTLP/HTTP listen address |
| `SENTINEL_HTTP_ADDR` | `:8080` | Dashboard + API listen address |
| `INGEST_TOKEN` | – | Comma-separated bearer tokens required on `/v1/*` (set it whenever the OTLP port is reachable from other machines) |
| `PUBLIC_URL` | `http://localhost:8080` | External dashboard URL (OAuth and setup callbacks) |
| `SENTINEL_DB` | `sentinel.db` | SQLite database path |
| `DASHBOARD_AUTH` | `github` | `github` (sign in with GitHub) or `none` (local only) |
| `ALLOWED_USERS` | – | Comma-separated GitHub logins allowed to sign in |
| `ALLOWED_ORG` | – | GitHub organization whose members may sign in |
| `LOG_RETENTION` | `168h` | How long log lines are kept |
| `METRIC_RETENTION` | `168h` | How long metric points are kept |
| `MIN_SEVERITY` | `17` | Lowest log `severity_number` treated as an error |
| `TICK_INTERVAL` | `10s` | Reporter loop interval |
| `SETTLE_WINDOW` | `60s` | Wait after a group's first event before filing |
| `MIN_EVENTS` | `1` | Minimum events before a group is filed |
| `UPDATE_INTERVAL` | `30m` | At most one update comment per issue per interval |
| `MAX_ISSUES_PER_HOUR` | `10` | Cap on new issues per rolling hour (`-1` = unlimited) |
| `DOWN_AFTER` | `5m` | Silence before a service counts as down; `0` disables |
| `GITHUB_APP_ID` | – | GitHub App id (enables app mode) |
| `GITHUB_APP_PRIVATE_KEY_FILE` / `GITHUB_APP_PRIVATE_KEY` | – | App private key (PEM file, or PEM text with `\n`) |
| `GITHUB_APP_CLIENT_ID` / `GITHUB_APP_CLIENT_SECRET` | – | App OAuth credentials for dashboard sign-in |
| `GITHUB_APP_SLUG` | looked up | App slug for the install link |
| `GITHUB_TOKEN` | – | Personal access token (used only without an app) |
| `GITHUB_REPO` | – | Fallback `owner/name` for issues |
| `GITHUB_LABELS` | – | Comma-separated labels for every new issue, e.g. `sentinel,bug` |
| `GITHUB_API_URL` / `GITHUB_WEB_URL` | `https://api.github.com` / `https://github.com` | For GitHub Enterprise |
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

## Development

```bash
go test ./...
go vet ./...
```

Layout: `internal/ingest` (OTLP receiver, metric conversion), `internal/redact`,
`internal/fingerprint`, `internal/store` (SQLite: one writer connection plus a
read pool in WAL mode, FTS5 log index), `internal/reporter` (issue lifecycle,
down detection, repository routing), `internal/ai` (Anthropic,
OpenAI-compatible, template), `internal/github` (REST client, GitHub App auth,
dry-run), `internal/alarm`, `internal/notify` (webhooks), `internal/uptime`
(HTTP checks), `internal/server`
(dashboard API and auth), `internal/web` (embedded build of `web/`, the React
app), `cmd/sentinel`, `cmd/sentinel-demo`.

The UI build in `internal/web/dist` is committed so `go build` works without
Node; rebuild it with `npm --prefix web run build` after changing `web/`.
