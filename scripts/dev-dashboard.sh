#!/usr/bin/env bash
# Run sentinel locally for dashboard development: DRY_RUN, no auth, fast timers.
# Usage: scripts/dev-dashboard.sh   (then: go run ./cmd/sentinel-demo -endpoint http://localhost:${OTLP_PORT:-14318})
set -euo pipefail
cd "$(dirname "$0")/.."
export DRY_RUN=true DASHBOARD_AUTH=none AI_PROVIDER=none
export SENTINEL_OTLP_ADDR="127.0.0.1:${OTLP_PORT:-14318}"
export SENTINEL_HTTP_ADDR="127.0.0.1:${HTTP_PORT:-18080}"
export PUBLIC_URL="http://localhost:${HTTP_PORT:-18080}"
export SENTINEL_DB="${SENTINEL_DB:-sentinel-dev.db}"   # gitignored
export SETTLE_WINDOW=10s TICK_INTERVAL=3s MIN_EVENTS=2 DOWN_AFTER=60s
exec go run ./cmd/sentinel
