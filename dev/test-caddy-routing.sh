#!/usr/bin/env bash
# End-to-end verification for Caddy Host-header-based subdomain routing.
#
# What this verifies:
#   0. Catch-all 404: unmatched hostname → HTTP 404 + "No app at this hostname"
#   1. Brain is reachable and Caddy is up (proxy + admin)
#   2. Installing whoami registers a subdomain route in Caddy
#   3. GET http://whoami.local:80/ (via Host header) → 2xx with whoami body
#   3b. A forged X-Forwarded-For is replaced by Caddy, not passed upstream (#329)
#   4. GET http://localhost:80/whoami/   (path-based)       → exactly 404 (subdomain-only routing)
#   5. GET http://localhost:80/ with Host: nobody.local → exactly 404
#   6. Uninstalling whoami withdraws the route (same Host → exactly 404)
#
# Requirements:
#   - `make dev` running: brain on :8080, Caddy admin on :2019, Caddy proxy on :80
#   - curl, jq
#
# On first run this script will bootstrap a test user (caddytest / caddytest-pw).
# On re-runs it logs in with those credentials. To reset: make clean && make dev.
#
# Usage:
#   dev/test-caddy-routing.sh

set -euo pipefail

BRAIN="http://localhost:8080"
CADDY_ADMIN="http://localhost:2019"
CADDY_PROXY="http://localhost:80"

TEST_USER="caddytest"
TEST_PASS="caddytest-pw"

COOKIE_JAR=""

cleanup() {
  [[ -n "${COOKIE_JAR:-}" ]] && rm -f "$COOKIE_JAR"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# --- helpers ----------------------------------------------------------------

# poll_job <job_id> <timeout_s> — polls GET /api/v1/jobs/<id> until
# status=completed or status=failed/cancelled/stalled. Returns 0 on completed.
poll_job() {
  local job_id="$1"
  local timeout_s="$2"
  local deadline=$(( $(date +%s) + timeout_s ))
  local state resp

  while true; do
    resp=$(curl -sf -b "$COOKIE_JAR" "$BRAIN/api/v1/jobs/$job_id") \
      || fail "job poll for $job_id returned non-2xx"
    state=$(printf '%s' "$resp" | jq -r '.status // empty')
    case "$state" in
      completed) return 0 ;;
      failed|cancelled|stalled) echo "    job $state — response: $resp" >&2; return 1 ;;
    esac
    [[ $(date +%s) -lt $deadline ]] || fail "timed out waiting for job $job_id after ${timeout_s}s"
    sleep 1
  done
}

# --- preflight --------------------------------------------------------------

echo "==> Preflight: checking brain, Caddy admin, Caddy proxy..."

command -v curl >/dev/null || fail "curl not installed"
command -v jq   >/dev/null || fail "jq not installed (apt install jq)"

# Brain: retry up to 5 seconds (it may still be starting).
BRAIN_UP=0
for i in {1..10}; do
  if curl -sf "$BRAIN/api/v1/auth/state" -o /dev/null 2>/dev/null; then
    BRAIN_UP=1; break
  fi
  sleep 0.5
done
[[ $BRAIN_UP -eq 1 ]] || fail "brain not reachable at $BRAIN — run: make dev"

# Caddy admin.
curl -sf "$CADDY_ADMIN/config/" -o /dev/null 2>/dev/null \
  || fail "Caddy admin not reachable at $CADDY_ADMIN — run: make dev"

# Caddy proxy (any response, even 404, means it's up).
HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" "$CADDY_PROXY/" 2>/dev/null) || true
[[ "$HTTP_CODE" != "000" ]] \
  || fail "Caddy proxy not reachable at $CADDY_PROXY — run: make dev"

echo "    OK: brain, Caddy admin, Caddy proxy all reachable"

# --- defensive cleanup: remove stale moose-app-* routes from prior failed runs --

echo "==> Cleanup: removing stale moose-app-* routes from prior failed runs..."
existing_ids=$(curl -sf "$CADDY_ADMIN/config/apps/http/servers/moose/routes" \
  | jq -r '.[] | select(.["@id"]? | test("^moose-app-")) | .["@id"]') || true
if [[ -n "$existing_ids" ]]; then
  for id in $existing_ids; do
    curl -sf -X DELETE "$CADDY_ADMIN/id/$id" >/dev/null \
      && echo "    removed stale route: $id" || true
  done
else
  echo "    no stale routes found"
fi

# --- Test 0: catch-all 404 with zero apps installed --------------------------

echo "==> [TEST 0] Catch-all: unmatched hostname → expect HTTP 404 + catch-all body"
T0_CODE=$(curl -s -o /tmp/moose-catchall-body.txt -w "%{http_code}" \
  -H "Host: nobody.local" "http://localhost:80/") || true
T0_BODY=$(cat /tmp/moose-catchall-body.txt)
if [[ "$T0_CODE" != "404" ]]; then
  fail "Expected HTTP 404 for unmatched host, got $T0_CODE"
fi
printf '%s' "$T0_BODY" | grep -q "No app at this hostname" \
  || fail "Catch-all body missing 'No app at this hostname' — got: $T0_BODY"
echo "    PASS: unmatched hostname returned 404 with catch-all body"

# --- setup / login ----------------------------------------------------------

COOKIE_JAR=$(mktemp /tmp/moose-caddy-test-cookies.XXXXXX)

echo "==> Auth: setup or login..."

AUTH_STATE=$(curl -sf "$BRAIN/api/v1/auth/state") \
  || fail "GET /api/v1/auth/state returned non-2xx"
HAS_USERS=$(printf '%s' "$AUTH_STATE" | jq -r '.has_users')

if [[ "$HAS_USERS" == "false" ]]; then
  echo "    No users yet — bootstrapping test admin ($TEST_USER)..."
  SETUP_RESP=$(curl -sf -c "$COOKIE_JAR" -b "$COOKIE_JAR" \
    -X POST "$BRAIN/api/v1/setup" \
    -H 'Content-Type: application/json' \
    -d "{\"display_name\":\"$TEST_USER\",\"password\":\"$TEST_PASS\"}") \
    || fail "POST /api/v1/setup failed"
  echo "    Setup OK (recovery code: $(printf '%s' "$SETUP_RESP" | jq -r '.recovery_code'))"
else
  echo "    Users exist — logging in as $TEST_USER..."
  LOGIN_CODE=$(curl -s -o /dev/null -w "%{http_code}" \
    -c "$COOKIE_JAR" -b "$COOKIE_JAR" \
    -X POST "$BRAIN/api/v1/login" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"$TEST_USER\",\"password\":\"$TEST_PASS\"}")
  if [[ "$LOGIN_CODE" != "200" ]]; then
    echo "    Login returned HTTP $LOGIN_CODE." >&2
    echo "    Hint: run \`make clean && make dev\` to reset, or create the $TEST_USER user manually." >&2
    exit 1
  fi
  echo "    Login OK"
fi

# --- install whoami ---------------------------------------------------------

echo "==> Installing whoami..."

INSTALL_RESP=$(curl -sf -c "$COOKIE_JAR" -b "$COOKIE_JAR" \
  -X POST "$BRAIN/api/v1/apps" \
  -H 'Content-Type: application/json' \
  -d '{"manifest_id":"whoami"}') \
  || fail "POST /api/v1/apps returned non-2xx"

JOB_ID=$(printf '%s' "$INSTALL_RESP" | jq -r '.job_id // empty')
[[ -n "$JOB_ID" ]] || fail "install response had no job id: $INSTALL_RESP"
echo "    install job: $JOB_ID — polling (timeout 60s)..."

poll_job "$JOB_ID" 60 || fail "whoami install job failed — check brain logs. If the image pull failed, ensure docker can reach the internet."
echo "    install done"

# --- extract slug + instance id ---------------------------------------------

echo "==> Fetching installed instance..."
APPS=$(curl -sf -b "$COOKIE_JAR" "$BRAIN/api/v1/apps") \
  || fail "GET /api/v1/apps returned non-2xx"

INSTANCE_ID=$(printf '%s' "$APPS" | jq -r '[.apps[] | select(.manifest_id=="whoami")] | first | .id // empty')
SLUG=$(printf '%s' "$APPS" | jq -r '[.apps[] | select(.manifest_id=="whoami")] | first | .slug // empty')

[[ -n "$INSTANCE_ID" ]] || fail "could not find whoami instance in app list: $APPS"
[[ -n "$SLUG" ]]        || fail "whoami instance has no slug"
echo "    instance_id=$INSTANCE_ID  slug=$SLUG"

# --- wait for container to be ready ----------------------------------------
# Caddy adds the route during install, but the container needs a moment to come up.

echo "==> Waiting for $SLUG.local to respond (up to 10s)..."
APP_OK=0
for i in {1..20}; do
  HTTP=$(curl -s -o /dev/null -w "%{http_code}" \
    --resolve "$SLUG.local:80:127.0.0.1" \
    "http://$SLUG.local:80/" 2>/dev/null) || true
  if [[ "$HTTP" == "200" ]]; then
    APP_OK=1; break
  fi
  sleep 0.5
done

# --- positive Host-based test -----------------------------------------------

echo "==> [TEST 1] Positive: Host-based routing → expect 2xx with whoami body"
if [[ $APP_OK -eq 0 ]]; then
  fail "Host: $SLUG.local on :80 never returned 200 (last HTTP code: $HTTP)"
fi

BODY=$(curl -sf \
  --resolve "$SLUG.local:80:127.0.0.1" \
  "http://$SLUG.local:80/") \
  || fail "curl with Host: $SLUG.local returned non-2xx"

printf '%s' "$BODY" | grep -qi "Hostname:" \
  || fail "whoami body did not contain 'Hostname:' — unexpected response: $BODY"
echo "    PASS: subdomain route works, body contains 'Hostname:'"

# --- forged X-Forwarded-For does not survive the Caddy hop -------------------

echo "==> [TEST 1b] Forged X-Forwarded-For → expect Caddy to replace it with the real peer"
# The brain's per-IP throttles key on the client IP Caddy reports (#329), so a
# client-supplied X-Forwarded-For must never reach an upstream. whoami echoes the
# request headers it received, which makes this observable end to end against the
# real Caddy rather than a fake admin API.
XFF_BODY=$(curl -sf \
  --resolve "$SLUG.local:80:127.0.0.1" \
  -H "X-Forwarded-For: 203.0.113.9, 198.51.100.7" \
  "http://$SLUG.local:80/") \
  || fail "curl with a forged X-Forwarded-For returned non-2xx"

XFF_SEEN=$(printf '%s' "$XFF_BODY" | grep -i "^X-Forwarded-For:" || true)
[[ -n "$XFF_SEEN" ]] \
  || fail "whoami did not echo an X-Forwarded-For header — cannot verify the trust boundary. Body: $XFF_BODY"
for forged in 203.0.113.9 198.51.100.7; do
  if printf '%s' "$XFF_SEEN" | grep -q "$forged"; then
    fail "TRUST BOUNDARY BROKEN: forged X-Forwarded-For hop $forged survived the Caddy hop — got: $XFF_SEEN"
  fi
done
echo "    PASS: Caddy replaced the forged chain ($XFF_SEEN)"

# --- negative path-based test -----------------------------------------------

echo "==> [TEST 2] Negative: path-based routing → expect exactly 404 with catch-all body"
PATH_BODY=$(curl -s -o /tmp/moose-path-body.txt -w "%{http_code}" \
  "http://localhost:80/$SLUG/") || true
PATH_CODE="$PATH_BODY"
PATH_BODY=$(cat /tmp/moose-path-body.txt)
if [[ "$PATH_CODE" == "200" ]]; then
  fail "ROUTING BUG: path-based request http://localhost:80/$SLUG/ returned 200 — subdomain-only routing is broken"
fi
if [[ "$PATH_CODE" != "404" ]]; then
  fail "Expected 404 for path-based request, got $PATH_CODE"
fi
printf '%s' "$PATH_BODY" | grep -q "No app at this hostname" \
  || fail "Path-based 404 body missing catch-all text — got: $PATH_BODY"
echo "    PASS: path-based request returned 404 with catch-all body — no path routing"

# --- negative unrelated-Host test -------------------------------------------

echo "==> [TEST 3] Negative: wrong Host header → expect exactly 404 with catch-all body"
WRONG_BODY=$(curl -s -o /tmp/moose-wrong-body.txt -w "%{http_code}" \
  -H "Host: nobody.local" "http://localhost:80/") || true
WRONG_CODE="$WRONG_BODY"
WRONG_BODY=$(cat /tmp/moose-wrong-body.txt)
if [[ "$WRONG_CODE" == "200" ]]; then
  fail "Wrong Host: nobody.local returned 200 — catch-all routing is leaking"
fi
if [[ "$WRONG_CODE" != "404" ]]; then
  fail "Expected 404 for unregistered host, got $WRONG_CODE"
fi
printf '%s' "$WRONG_BODY" | grep -q "No app at this hostname" \
  || fail "Unregistered host 404 body missing catch-all text — got: $WRONG_BODY"
echo "    PASS: unregistered host returned 404 with catch-all body"

# --- uninstall --------------------------------------------------------------

echo "==> Uninstalling $SLUG (instance_id=$INSTANCE_ID)..."
UNINSTALL_RESP=$(curl -sf -b "$COOKIE_JAR" \
  -X DELETE "$BRAIN/api/v1/apps/$INSTANCE_ID") \
  || fail "DELETE /api/v1/apps/$INSTANCE_ID returned non-2xx"

UJOB_ID=$(printf '%s' "$UNINSTALL_RESP" | jq -r '.job_id // empty')
[[ -n "$UJOB_ID" ]] || fail "uninstall response had no job id: $UNINSTALL_RESP"
echo "    uninstall job: $UJOB_ID — polling (timeout 60s)..."

poll_job "$UJOB_ID" 60 || fail "whoami uninstall job failed — check brain logs"
echo "    uninstall done"

# --- verify route is withdrawn ----------------------------------------------

echo "==> [TEST 4] Verify route withdrawn after uninstall → expect exactly 404 with catch-all body"
# Give Caddy a moment to process the route removal.
sleep 1
GONE_BODY=$(curl -s -o /tmp/moose-gone-body.txt -w "%{http_code}" \
  --resolve "$SLUG.local:80:127.0.0.1" \
  "http://$SLUG.local:80/") || true
GONE_CODE="$GONE_BODY"
GONE_BODY=$(cat /tmp/moose-gone-body.txt)
if [[ "$GONE_CODE" == "200" ]]; then
  fail "Route for $SLUG.local still returns 200 after uninstall — Caddy route not removed"
fi
if [[ "$GONE_CODE" != "404" ]]; then
  fail "Expected 404 after uninstall, got $GONE_CODE"
fi
printf '%s' "$GONE_BODY" | grep -q "No app at this hostname" \
  || fail "Post-uninstall 404 body missing catch-all text — got: $GONE_BODY"
echo "    PASS: $SLUG.local returned 404 with catch-all body after uninstall (route gone)"

echo ""
echo "==> ALL CHECKS PASSED"
