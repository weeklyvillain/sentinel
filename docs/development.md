# Development

## Building

```bash
npm --prefix web ci && npm --prefix web run build   # only after changing the UI; the build is committed
go build -o bin/sentinel ./cmd/sentinel
GITHUB_APP_ID=123456 GITHUB_APP_PRIVATE_KEY_FILE=sentinel.pem \
GITHUB_APP_CLIENT_ID=Iv1.abc GITHUB_APP_CLIENT_SECRET=... \
PUBLIC_URL=https://sentinel.example.com ALLOWED_ORG=acme \
ANTHROPIC_API_KEY=sk-ant-... ./bin/sentinel
```

## Local demo

Try it locally without GitHub, sign-in or an AI key, with demo traffic:

```bash
SENTINEL_HTTP_ADDR=127.0.0.1:8080 DRY_RUN=true DASHBOARD_AUTH=none AI_PROVIDER=none ./bin/sentinel
```

```bash
go run ./cmd/sentinel-demo -endpoint http://localhost:4318
```

Then open http://localhost:8080. `cmd/sentinel-demo` sends logs (with a
backfilled history), traces (checkout calling billing, an N+1 on
`/api/products`) and metrics for four fake services; one of them stops after
90s to show outage detection, and halfway through checkout ships release
1.5.0, which is slower.

For UI work, `npm --prefix web run dev` serves the React app with hot reload
and proxies `/api` to a Sentinel on :8080 (`SENTINEL_URL` to change).

## End-to-end check

```bash
./scripts/e2e-dryrun.sh
```

Builds the binary, runs it in `DRY_RUN`, POSTs a burst of 50 OTLP/JSON error
logs with different order ids, emails, IPs, durations and secrets, and checks
that they became one group and one (logged) issue, that all 50 lines are
searchable through the dashboard API, and that no secret is in the output or
the stored logs. Use `PORT=14318 HTTP_PORT=18090` if the defaults are taken.

## Tests and layout

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
