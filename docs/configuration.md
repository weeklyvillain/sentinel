# Configuration

All configuration is via environment variables. Durations accept Go syntax
(`90s`, `5m`, `168h`) or plain seconds.

| Variable | Default | Meaning |
|---|---|---|
| `SENTINEL_OTLP_ADDR` | `:4318` | OTLP/HTTP listen address |
| `SENTINEL_HTTP_ADDR` | `:8080` | Dashboard + API listen address |
| `INGEST_TOKEN` | generated | Comma-separated bearer tokens required on `/v1/*`; without it Sentinel generates one (Settings → Access) |
| `PUBLIC_URL` | `http://localhost:8080` | External dashboard URL (OAuth and setup callbacks) |
| `SENTINEL_DB` | `sentinel.db` | SQLite database path |
| `DASHBOARD_AUTH` | `github` | `github`, or `none` for local development (only with a localhost `SENTINEL_HTTP_ADDR`) |
| `ALLOWED_USERS` | – | GitHub logins allowed to sign in, besides Settings → Access |
| `ALLOWED_ORG` | – | GitHub organization whose members may sign in, besides Settings → Access |
| `LOG_RETENTION` | `168h` | How long log lines are kept |
| `METRIC_RETENTION` | `168h` | How long metric points (and request statistics, uptime results) are kept |
| `TRACE_RETENTION` | `72h` | How long stored spans are kept |
| `TRACE_SAMPLE_RATE` | `1` | Share of traces stored (0–1, decided per trace id); spans with errors are always stored, request statistics count every span |
| `PERF_SLOW_P95` | `2s` | p95 of an operation that files a slow-endpoint issue; `0` disables |
| `AUTOFIX_MODE` | `actions` | Where Claude Code fixes issues until one is picked in the dashboard: `actions` (a workflow in each repository) or `fixer` (the fixer container) |
| `AUTOFIX_LABEL` | `sentinel-autofix` | Label that starts the fix workflow in the repository (`actions` mode) |
| `FIXER_TOKEN` | generated | Shared secret of Sentinel and the fixer container. Without it Sentinel generates one and writes it to `FIXER_TOKEN_FILE` for the fixer |
| `FIXER_TOKEN_FILE` | – | Where Sentinel writes the fixer's token (`/fixer-auth/token` in the Compose files, a volume both containers mount) |
| `PERF_N_PLUS_ONE` | `10` | Identical database calls under one span that file an N+1 issue; `0` disables |
| `MIN_SEVERITY` | `17` | Lowest log `severity_number` treated as an error |
| `TICK_INTERVAL` | `10s` | Reporter loop interval |
| `SETTLE_WINDOW` | `60s` | Wait after a group's first event before filing |
| `MIN_EVENTS` | `1` | Minimum events before a group is filed |
| `UPDATE_INTERVAL` | `30m` | At most one update comment per issue per interval |
| `MAX_ISSUES_PER_HOUR` | `10` | Cap on new issues per rolling hour (`-1` = unlimited) |
| `DOWN_AFTER` | `5m` | Silence before a watched service counts as down; `0` disables |
| `GITHUB_APP_ID` | – | GitHub App id (enables app mode) |
| `GITHUB_APP_PRIVATE_KEY_FILE` / `GITHUB_APP_PRIVATE_KEY` | – | App private key (PEM file, or PEM text with `\n`) |
| `GITHUB_APP_CLIENT_ID` / `GITHUB_APP_CLIENT_SECRET` | – | App OAuth credentials for dashboard sign-in |
| `GITHUB_APP_SLUG` | looked up | App slug for the install link |
| `GITHUB_TOKEN` | – | Personal access token (used only without an app) |
| `GITHUB_REPO` | – | Fallback `owner/name` for issues |
| `GITHUB_LABELS` | – | Comma-separated labels for every new issue, e.g. `sentinel,bug` |
| `GITHUB_API_URL` / `GITHUB_WEB_URL` | `https://api.github.com` / `https://github.com` | For GitHub Enterprise |
| `DRY_RUN` | `false` | Log GitHub operations instead of performing them |
| `AI_PROVIDER` | `anthropic` | `anthropic`, `openai` (any OpenAI-compatible server), `claude-code` (the fixer container, see [Automated fixes](automated-fixes.md)) or `none` |
| `AI_MODEL` | `claude-haiku-5-5` for anthropic | Model name; required for `openai` |
| `ANTHROPIC_API_KEY` | – | Anthropic key (the SDK's other credential sources also work) |
| `OPENAI_BASE_URL` | `http://localhost:11434/v1` | OpenAI-compatible base URL (Ollama default) |
| `OPENAI_API_KEY` | – | Optional bearer token for the OpenAI-compatible server |
| `AI_TIMEOUT` | `60s` | Per-draft timeout |

## AI providers

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
