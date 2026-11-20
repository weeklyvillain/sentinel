# Deploying on a separate server

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

## 1. Sentinel server (Docker Compose)

```bash
git clone https://github.com/weeklyvillain/sentinel.git && sentinel/deploy/setup.sh
```

[`setup.sh`](../deploy/setup.sh) checks Docker, asks for the domain, writes
`deploy/.env`, starts Sentinel behind Caddy and prints the address and the
setup code. Run it again to upgrade. By hand, it is:

```bash
git clone https://github.com/weeklyvillain/sentinel.git sentinel && cd sentinel/deploy
echo "SENTINEL_DOMAIN=sentinel.example.com" > .env
docker compose up -d --build
docker compose logs sentinel | grep "setup code"
```

Then open `https://SENTINEL_DOMAIN` and follow [First start](setup.md). That is
all the configuration Sentinel needs; everything else is in the dashboard.

- Point DNS for `SENTINEL_DOMAIN` at the server and open ports **80** and
  **443** (Caddy gets a Let's Encrypt certificate automatically). Ports 4318
  and 8080 are not published; everything goes through Caddy.
- Caddy routes `/v1/*` to the OTLP receiver and everything else to the
  dashboard, so collectors and browsers use the same `https://SENTINEL_DOMAIN`.
- Collectors must send `Authorization: Bearer <token>`. Sentinel generates
  the token; copy it from Settings → Access (where it can also be replaced).
  `INGEST_TOKEN` in `.env` overrides it and accepts several comma-separated
  tokens, for rotating without downtime.
- Data lives in the `sentinel-data` volume (`/data/sentinel.db`). Back it up
  with e.g. `docker compose exec` + `sqlite3 .backup`, or snapshot the volume.
- Upgrade: `git pull && docker compose up -d --build`. The schema migrates
  itself on start.
- The image has a `HEALTHCHECK` (`sentinel healthcheck`), so `docker ps` shows
  whether it is healthy.
- Sentinel needs outbound HTTPS for GitHub, the AI provider, webhooks and
  uptime checks.

### When another proxy already owns 80/443

If the host already runs a reverse proxy, use
`docker-compose.behind-proxy.yml` instead. It runs Sentinel alone and
publishes OTLP on `127.0.0.1:4318` and the dashboard on `127.0.0.1:18080`
(reach it with `ssh -L 18080:localhost:18080 host`). To serve it publicly,
paste [`deploy/Caddyfile.shared-proxy`](../deploy/Caddyfile.shared-proxy) into the
existing Caddyfile, set `PROXY_NETWORK` to that proxy's Docker network, and
add `-f docker-compose.proxy-network.yml`. Configure dashboard sign-in first.

## 2. Every app server (OpenTelemetry Collector)

With Docker, copy the `deploy/` folder (or clone the repository) to the app
server and run:

```bash
export SENTINEL_ENDPOINT=https://sentinel.example.com
export SENTINEL_INGEST_TOKEN=<the ingest token from Settings → Access>
export HOST_SERVICE_NAME=host-$(hostname)
docker compose -f docker-compose.collector.yml up -d
```

[`docker-compose.collector.yml`](../deploy/docker-compose.collector.yml) runs
otelcol-contrib on the host network with the host filesystem mounted
read-only for disk metrics. Without Docker, install **otelcol-contrib** and use
[`deploy/otel-collector-app-server.yaml`](../deploy/otel-collector-app-server.yaml)
with the same three environment variables.

It:

- receives OTLP from the apps on that machine (`localhost:4317/4318`),
- forwards logs, traces and metrics to Sentinel over HTTPS with gzip,
- keeps a **disk-backed retry queue**, so data produced while Sentinel or the
  network is unreachable is delivered when it comes back,
- sends **host metrics** (CPU, memory, disk, network, load) as a service named
  `HOST_SERVICE_NAME`. Sentinel then shows the server's own charts, and, once
  you switch on **Watch** for that service under Settings → Services, flags the whole machine as down
  when it goes quiet.

## 3. Uptime checks

On **Alerts → Uptime** in the dashboard, add the public health URLs of your apps (e.g.
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

## Who watches Sentinel?

Sentinel cannot report its own outage. Point an external uptime service (or
another Sentinel) at `https://SENTINEL_DOMAIN/healthz`.
