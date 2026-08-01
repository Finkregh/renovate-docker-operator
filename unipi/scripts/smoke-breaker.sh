#!/usr/bin/env bash
set -euo pipefail

# Smoke test for the breaker/metrics API surface.
# Non-destructive when the breaker is already closed.
#
# Usage:
#   BASE=http://localhost:8081 ./unipi/scripts/smoke-breaker.sh

BASE="${BASE:-http://localhost:8080}"

fail() {
  echo "FAIL: $1" >&2
  exit 1
}

echo "=== step 1: GET /api/v1/breaker/state ==="
RESP=$(curl -sf "${BASE}/api/v1/breaker/state") || fail "breaker/state returned non-200"
echo "$RESP" | jq -e '.state' >/dev/null 2>&1 || fail "response missing .state field"
echo "OK — state=$(echo "$RESP" | jq -r '.state')"

echo ""
echo "=== step 2: GET /metrics ==="
METRICS=$(curl -sf "${BASE}/metrics") || fail "/metrics returned non-200"
echo "$METRICS" | grep -q "renovate_breaker_state" || fail "/metrics missing renovate_breaker_state"
echo "OK — renovate_breaker_state found"

echo ""
echo "=== step 3: POST /api/v1/breaker/bypass/smoke/example ==="
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" -X POST "${BASE}/api/v1/breaker/bypass/smoke/example")
if [ "$HTTP_CODE" != "204" ]; then
  fail "bypass returned HTTP $HTTP_CODE, expected 204"
fi
echo "OK — bypass returned 204"

echo ""
echo "=== step 4: POST /api/v1/breaker/reset ==="
RESP=$(curl -sf -X POST "${BASE}/api/v1/breaker/reset") || fail "breaker/reset returned non-200"
echo "$RESP" | jq -e '.previousState' >/dev/null 2>&1 || fail "response missing .previousState"
echo "$RESP" | jq -e '.clearedBackoffs' >/dev/null 2>&1 || fail "response missing .clearedBackoffs"
echo "$RESP" | jq -e '.replayedProjects' >/dev/null 2>&1 || fail "response missing .replayedProjects"
echo "OK — reset response has all expected fields"

echo ""
echo "=== ALL STEPS PASSED ==="
exit 0
