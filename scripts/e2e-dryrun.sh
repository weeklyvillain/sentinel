#!/usr/bin/env bash
# End-to-end check: run sentinel in DRY_RUN, POST a burst of OTLP/JSON error
# logs (with varying ids), and assert they become exactly one group and one
# (dry-run) issue.
#
#   ./scripts/e2e-dryrun.sh            # uses port 4318
#   PORT=14318 ./scripts/e2e-dryrun.sh
set -euo pipefail

cd "$(dirname "$0")/.."
PORT="${PORT:-4318}"
BURST="${BURST:-50}"
WORK="$(mktemp -d)"
BIN="$WORK/sentinel"
[[ "$(go env GOOS)" == "windows" ]] && BIN="$BIN.exe"
LOG="$WORK/sentinel.log"

go build -o "$BIN" ./cmd/sentinel

cleanup() { [[ -n "${PID:-}" ]] && kill "$PID" 2>/dev/null || true; }
trap cleanup EXIT

DRY_RUN=true AI_PROVIDER=none \
SENTINEL_OTLP_ADDR="127.0.0.1:$PORT" SENTINEL_DB="$WORK/sentinel.db" \
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

GROUPS_JSON=$(curl -fsS "http://127.0.0.1:$PORT/groups")
echo "$GROUPS_JSON" | head -40

ngroups=$(echo "$GROUPS_JSON" | grep -c '"fingerprint"' || true)
count=$(echo "$GROUPS_JSON" | grep -o '"count": [0-9]*' | head -1 | grep -o '[0-9]*')
nissues=$(grep -c 'DRY_RUN create issue' "$LOG" || true)

echo
echo "groups=$ngroups count=$count dry-run issues=$nissues"
if grep -q 'sk_live_' "$LOG"; then echo "FAIL: secret leaked into output"; exit 1; fi
if [[ "$ngroups" == 1 && "$count" == "$BURST" && "$nissues" == 1 ]]; then
  echo "--- dry-run issue (from sentinel log) ---"
  grep 'DRY_RUN create issue' "$LOG" | head -1 | cut -c1-1500
  echo
  echo "PASS: $BURST events -> 1 group -> 1 issue"
else
  echo "FAIL"; cat "$LOG"; exit 1
fi
