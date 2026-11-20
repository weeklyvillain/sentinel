# Sentinel

**Self-hosted error tracking and monitoring for OpenTelemetry that files one
GitHub issue per real problem, not one per log line.**

Send Sentinel your logs, traces and metrics: over OpenTelemetry, from an
existing Sentry SDK, or from the browser. It finds errors, outages and slow
endpoints, groups them deterministically and keeps a single GitHub issue up
to date for each distinct problem. A small AI model writes the issue's title
and analysis. Every fact in the issue (counts, timestamps, trace IDs,
samples) comes from Sentinel itself, not from the model.

It ships as one Go binary with an embedded dashboard and a SQLite database.
You don't need Kafka, ClickHouse or Elasticsearch, and it sets itself up from
the browser.

```
app ──OTLP──▶ Collector ──otlphttp──▶ Sentinel :4318 ──▶ SQLite ──▶ reporter ──▶ GitHub issues
                                                              ├──▶ alarms ──▶ Slack / Discord / webhooks
                                                              └──▶ dashboard :8080
```

## Features

- **Issues that stay quiet.** Errors are fingerprinted by service, exception
  type and normalized stack or message, so 10,000 occurrences make one issue.
  It gets at most one update comment per interval, is reopened with a
  *Regression* note if a fixed error comes back, and stays closed if you
  marked it *not planned*.
- **Outage detection.** Watched services that go quiet, and failing HTTP
  uptime checks, open an issue and notify your webhooks. When the service
  recovers, Sentinel comments how long the outage lasted and closes the issue.
- **Performance issues** from traces: N+1 queries, slow endpoints (p95) and
  latency regressions between releases.
- **Many ways in.** OTLP from any OpenTelemetry SDK or Collector, Sentry SDKs
  as a drop-in (just swap the DSN), Grafana Faro, Datadog Browser Logs and
  plain JSON. Structured JSON and logfmt lines are parsed into fields.
- **Secrets never leave the box.** Tokens, passwords, API keys, JWTs and URL
  credentials are redacted *before* anything is stored, sent to a model or
  posted to GitHub.
- **A full dashboard:** log search with live tail, trace waterfalls across
  services, per-operation latency, custom metric dashboards, alarms, uptime
  checks, SLOs with burn-rate alerts, and maintenance windows.
- **Releases and tags.** `service.version` is tracked per issue (first seen,
  last seen, regressed in), along with users affected and a breakdown by
  environment, host, route, browser and more.
- **Optional automated fixes.** Hand an issue to Claude Code, which opens a
  pull request that says `Fixes #N`, even across several repositories. It
  can run in your repository's GitHub Actions or in a fixer container next
  to Sentinel.
- **Choice of AI provider:** Anthropic (default), any OpenAI-compatible
  server such as Ollama or llama.cpp, Claude Code in the fixer container, or
  none at all (a deterministic template).

## Quick start

Try it on your machine in a minute, with no GitHub, sign-in or AI key needed.
You need Go 1.26+.

```bash
git clone https://github.com/weeklyvillain/sentinel.git && cd sentinel
go build -o bin/sentinel ./cmd/sentinel
SENTINEL_HTTP_ADDR=127.0.0.1:8080 DRY_RUN=true DASHBOARD_AUTH=none AI_PROVIDER=none ./bin/sentinel
```

In a second terminal, send it some demo traffic:

```bash
go run ./cmd/sentinel-demo -endpoint http://localhost:4318
```

Open <http://localhost:8080>. The demo simulates four services with logs,
traces and metrics. One of them stops after 90 seconds so you can watch
outage detection, and halfway through, the checkout service ships a slower
release. In `DRY_RUN` mode, GitHub operations are logged instead of performed.

## Running it for real

Run Sentinel on its **own** server, not on the machines it watches, so it can
still report when they go down. You only need a domain name pointing at the
server and ports 80 and 443 open:

```bash
git clone https://github.com/weeklyvillain/sentinel.git && cd sentinel/deploy
echo "SENTINEL_DOMAIN=sentinel.example.com" > .env
docker compose up -d --build                     # Sentinel behind Caddy with automatic HTTPS
docker compose logs sentinel | grep "setup code"
```

Open `https://SENTINEL_DOMAIN` and enter the setup code. Sentinel then
creates its own GitHub App through GitHub's manifest flow, so there are no
keys to copy. You install the app, sign in, and pick repositories. From
there, **Setup** in the sidebar walks you through the rest: telemetry,
services, AI, fixes, webhooks and uptime checks.

Point your collectors at it with the
[ready-made collector config](deploy/otel-collector-app-server.yaml) and the
ingest token from *Settings → Access*. The [deployment guide](docs/deployment.md)
has the full details, including running behind an existing reverse proxy.

## Documentation

| | |
|---|---|
| [How it works](docs/how-it-works.md) | Ingest, redaction, grouping, the issue lifecycle, releases, performance and outage detection |
| [First start and sign-in](docs/setup.md) | The setup code, the generated GitHub App, who may sign in, repositories, the setup guide |
| [Deployment](docs/deployment.md) | Docker Compose, running behind an existing proxy, collectors on app servers, uptime checks |
| [Sending telemetry](docs/sending-telemetry.md) | OpenTelemetry Collector config, structured logs, Sentry SDKs, Faro, Datadog and JSON logs |
| [Tracing across services](docs/tracing.md) | How traces link up across OpenTelemetry, Sentry and browser SDKs |
| [Dashboard](docs/dashboard.md) | Logs, traces, issues, dashboards, alarms, uptime, SLOs, webhooks |
| [Automated fixes](docs/automated-fixes.md) | Claude Code in GitHub Actions or the fixer container, cross-repository fixes, "worth fixing?" investigations |
| [Configuration](docs/configuration.md) | Every environment variable, AI providers |
| [Development](docs/development.md) | Building, the UI dev server, tests, end-to-end check, code layout |

## Security

- Ingest endpoints take a bearer token, which Sentinel generates for you. You
  can replace it under *Settings → Access*, or set several in `INGEST_TOKEN`
  to rotate them without downtime.
- Dashboard sign-in goes through GitHub and **fails closed**: only the users
  and organization allowed under *Settings → Access* get in, and you can't
  save a list that locks yourself out. A fresh install can only be claimed by
  someone who can read the server's log.
- Redaction runs before storage, AI calls and GitHub. The model's prompt treats
  all telemetry as untrusted data, and its output is redacted again, with
  `@mentions` neutralized, before it is posted.
- Webhook deliveries can be signed with HMAC-SHA256.

If you find a vulnerability, please report it privately through
[GitHub security advisories](https://github.com/weeklyvillain/sentinel/security/advisories/new)
rather than in a public issue.

## Contributing

Issues and pull requests are welcome. Run the tests before you open a PR:

```bash
go test ./... && go vet ./...
```

The built dashboard UI in `internal/web/dist` is committed, so `go build` works
without Node. If you change `web/`, rebuild it with
`npm --prefix web ci && npm --prefix web run build`. See
[Development](docs/development.md) for the hot-reload dev server and the
end-to-end check.

## License

Sentinel is released under the [MIT License](LICENSE).
