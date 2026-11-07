#!/usr/bin/env bash
# End-to-end check: run sentinel in DRY_RUN, POST a burst of OTLP/JSON error
# logs (with varying ids), and assert they become exactly one group and one
# (dry-run) issue, and that every line is stored, searchable and redacted.
#
#   ./scripts/e2e-dryrun.sh                         # OTLP on 4318, dashboard API on 18089
#   PORT=14318 HTTP_PORT=18090 ./scripts/e2e-dryrun.sh
set -euo pipefail

cd "$(dirname "$0")/.."
PORT="${PORT:-4318}"
HTTP_PORT="${HTTP_PORT:-18089}"
BURST="${BURST:-50}"
WORK="$(mktemp -d)"
BIN="$WORK/sentinel"
[[ "$(go env GOOS)" == "windows" ]] && BIN="$BIN.exe"
LOG="$WORK/sentinel.log"

go build -o "$BIN" ./cmd/sentinel

cleanup() { [[ -n "${PID:-}" ]] && kill "$PID" 2>/dev/null || true; }
trap cleanup EXIT

DRY_RUN=true AI_PROVIDER=none DASHBOARD_AUTH=none \
SENTINEL_OTLP_ADDR="127.0.0.1:$PORT" SENTINEL_HTTP_ADDR="127.0.0.1:$HTTP_PORT" SENTINEL_DB="$WORK/sentinel.db" \
SETTLE_WINDOW=2s TICK_INTERVAL=1s MIN_EVENTS=3 DOWN_AFTER=0 \
  "$BIN" >"$LOG" 2>&1 &
PID=$!

for _ in $(seq 50); do
  curl -fsS "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break
  sleep 0.2
done

echo "POSTing $BURST error logs to /v1/logs ..."
for i in $(seq 1 "$BURST"); do
  trace=$(printf '%032x' "$i")
  body=$(cat <<JSON
{"resourceLogs":[{"resource":{"attributes":[
  {"key":"service.name","value":{"stringValue":"checkout"}},
  {"key":"deployment.environment.name","value":{"stringValue":"prod"}}]},
 "scopeLogs":[{"logRecords":[{
  "severityNumber":17,"severityText":"ERROR",
  "traceId":"$trace","spanId":"00000000000000$(printf '%02x' $((i % 256)))",
  "body":{"stringValue":"failed to charge order $((1000 + i)) for user$i@example.com: upstream 10.0.0.$((i % 250)):443 timed out after $((100 + i))ms (api_key=sk_live_$i$i$i$i$i$i)"},
  "attributes":[{"key":"exception.type","value":{"stringValue":"PaymentTimeout"}}]
 }]}]}]}
JSON
)
  curl -fsS -X POST "http://127.0.0.1:$PORT/v1/logs" -H 'Content-Type: application/json' -d "$body" >/dev/null
done

echo "waiting for the settle window + a reporter tick ..."
sleep 4

API="http://127.0.0.1:$HTTP_PORT/api"
GROUPS_JSON=$(curl -fsS "$API/groups")
echo "$GROUPS_JSON" | head -c 1200; echo

ngroups=$(echo "$GROUPS_JSON" | grep -o '"fingerprint"' | wc -l | tr -d ' ')
count=$(echo "$GROUPS_JSON" | grep -o '"count":[0-9]*' | head -1 | grep -o '[0-9]*')
nissues=$(grep -c 'DRY_RUN create issue' "$LOG" || true)
# Every log line is also stored and searchable.
searchable=$(curl -fsS "$API/logs?q=charge%20order&range=1h&limit=1" | grep -o '"total":[0-9]*' | grep -o '[0-9]*')

echo
echo "groups=$ngroups count=$count dry-run issues=$nissues searchable_logs=$searchable"
if grep -q 'sk_live_' "$LOG"; then echo "FAIL: secret leaked into output"; exit 1; fi
if curl -fsS "$API/logs?range=1h&limit=1000" | grep -q 'sk_live_'; then echo "FAIL: secret stored in logs"; exit 1; fi
if [[ "$ngroups" == 1 && "$count" == "$BURST" && "$nissues" == 1 && "$searchable" == "$BURST" ]]; then
  echo "--- dry-run issue (from sentinel log) ---"
  grep 'DRY_RUN create issue' "$LOG" | head -1 | cut -c1-1500
  echo
  echo "PASS: $BURST events -> 1 group -> 1 issue; $BURST searchable, redacted log lines"
else
  echo "FAIL"; cat "$LOG"; exit 1
fi
