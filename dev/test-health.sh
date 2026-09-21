#!/usr/bin/env bash
# End-to-end demo of the storage-health pipeline (slice 0019).
#
# What this proves:
#   1. moose-storage-verify reads a marker + canary tree and writes a
#      protocol.StorageHealth payload to /run/moose/health/storage.json
#      (here pointed at a tempdir).
#   2. The fake host-agent, wired with MOOSE_HEALTH_PATH, serves that file's
#      findings in the storage category of GET /v1/health/system.
#   3. The brain polls host-agent on a short interval, reconciles findings
#      into health.Manager, and surfaces typed issues at GET /api/v1/health.
#   4. Findings raise on the next poll after the fixture changes; cleared
#      findings disappear on the next poll after they're removed.
#
# Requirements:
#   - Go toolchain on PATH
#   - curl, jq
#   - Free TCP ports 8090 (this script does not collide with `make dev`'s 8080)
#
# Usage:
#   dev/test-health.sh

set -euo pipefail

# --- workspace -----------------------------------------------------------

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
WORK="$(mktemp -d -t moose-health-XXXXXX)"
echo "workspace: $WORK"

SOCK="$WORK/agent.sock"
HEALTH_FILE="$WORK/storage.json"
FIXTURE_ROOT="$WORK/fixture"        # MOOSE_VERIFY_ROOT
STATE_DIR="$WORK/state"
CATALOG_CACHE_DIR="$WORK/catalog-cache"
COOKIE_JAR="$WORK/cookies"
BRAIN_LOG="$WORK/brain.log"
AGENT_LOG="$WORK/agent.log"

BRAIN_PORT=8090
BRAIN="http://127.0.0.1:${BRAIN_PORT}"
TEST_USER="healthtest"
TEST_PASS="healthtest-pw"

mkdir -p "$FIXTURE_ROOT/etc/moose" "$FIXTURE_ROOT/srv/moose" "$FIXTURE_ROOT/var/lib/moose" \
         "$STATE_DIR" "$CATALOG_CACHE_DIR"

# --- cleanup -------------------------------------------------------------

BRAIN_PID=""
AGENT_PID=""
cleanup() {
  local rc=$?
  set +e
  [[ -n "$BRAIN_PID" ]] && kill "$BRAIN_PID" 2>/dev/null
  [[ -n "$AGENT_PID" ]] && kill "$AGENT_PID" 2>/dev/null
  wait 2>/dev/null
  if (( rc != 0 )); then
    echo
    echo "--- last 40 lines of brain.log ---"
    tail -n 40 "$BRAIN_LOG" 2>/dev/null
    echo "--- last 40 lines of agent.log ---"
    tail -n 40 "$AGENT_LOG" 2>/dev/null
    echo
    echo "workspace preserved at: $WORK"
  else
    rm -rf "$WORK"
  fi
}
trap cleanup EXIT

# --- helpers -------------------------------------------------------------

fail() { echo "FAIL: $*" >&2; exit 1; }
step() { echo; echo "=== $* ==="; }

wait_for() {
  local label="$1"; shift
  for _ in $(seq 1 50); do
    if "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.1
  done
  fail "timeout waiting for: $label"
}

reporter() {
  MOOSE_VERIFY_ROOT="$FIXTURE_ROOT" \
  MOOSE_VERIFY_OUT="$HEALTH_FILE" \
    "$ROOT/moose-storage-verify"
}

# Fetch /api/v1/health and return the JSON.
issues() {
  curl -sS --cookie "$COOKIE_JAR" "$BRAIN/api/v1/health"
}

# Wait until the issues list matches the expected ID set (space-separated,
# sorted). Empty argument means "expect zero issues."
wait_for_issues() {
  local want="$1"
  for _ in $(seq 1 30); do
    local got
    got=$(issues | jq -r '.issues | map(.id) | sort | join(" ")')
    if [[ "$got" == "$want" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "want: '$want'"
  echo "got:  '$(issues | jq -c .)'"
  fail "issues did not converge"
}

# --- build ---------------------------------------------------------------

step "building binaries"
( cd "$ROOT" && go build -o ./moose-storage-verify ./cmd/moose-storage-verify )
( cd "$ROOT" && go build -o ./host-agent          ./cmd/host-agent )
( cd "$ROOT" && go build -o ./brain               ./cmd/brain )

# --- launch host-agent ---------------------------------------------------

step "launching fake host-agent (MOOSE_HEALTH_PATH=$HEALTH_FILE)"
# Seed an initial empty findings file so host-agent has something to serve
# before the first reporter run.
echo '{"checked_at":"1970-01-01T00:00:00Z","findings":[]}' > "$HEALTH_FILE"
MOOSE_AGENT_SOCK="$SOCK" \
MOOSE_HEALTH_PATH="$HEALTH_FILE" \
  "$ROOT/host-agent" >"$AGENT_LOG" 2>&1 &
AGENT_PID=$!
wait_for "host-agent socket" test -S "$SOCK"

# --- launch brain --------------------------------------------------------

step "launching brain (MOOSE_HEALTH_POLL=500ms)"
MOOSE_LISTEN=":${BRAIN_PORT}" \
MOOSE_STATE_DIR="$STATE_DIR" \
MOOSE_CATALOG_URL="http://127.0.0.1:1" \
MOOSE_CATALOG_CACHE_DIR="$CATALOG_CACHE_DIR" \
MOOSE_AGENT_SOCK="$SOCK" \
MOOSE_HEALTH_POLL="500ms" \
MOOSE_CADDY_ADMIN="http://127.0.0.1:1" \
MOOSE_LOG_LEVEL="info" \
  "$ROOT/brain" >"$BRAIN_LOG" 2>&1 &
BRAIN_PID=$!
wait_for "brain HTTP" curl -sS "$BRAIN/api/v1/auth/state"

# --- bootstrap admin -----------------------------------------------------

step "bootstrapping admin user"
curl -sS -c "$COOKIE_JAR" -b "$COOKIE_JAR" \
  -H 'Content-Type: application/json' \
  -d "{\"display_name\":\"$TEST_USER\",\"password\":\"$TEST_PASS\"}" \
  "$BRAIN/api/v1/setup" >/dev/null
curl -sS -c "$COOKIE_JAR" -b "$COOKIE_JAR" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$TEST_USER\",\"password\":\"$TEST_PASS\"}" \
  "$BRAIN/api/v1/login" >/dev/null

# --- assertion 1: healthy box, empty issues -----------------------------

step "case A — Level-0 box (no marker): empty findings"
reporter
wait_for_issues ""
echo "PASS: GET /api/v1/health → issues = []"

# --- assertion 2: drive enrolled but missing ---------------------------

step "case B — marker present, drive absent: data-drive-missing"
cat >"$FIXTURE_ROOT/etc/moose/data-drive.enrolled" <<EOF
{"uuid":"abc-123","enrolled_at":"2026-04-12T08:00:00Z"}
EOF
reporter
wait_for_issues "data-drive-missing"
got=$(issues)
sev=$(echo "$got" | jq -r '.issues[0].severity')
[[ "$sev" == "error" ]] || fail "severity: want error, got $sev"
blocks=$(echo "$got" | jq -c '{w: .issues[0].blocks_writes, a: .issues[0].blocks_apps, u: .issues[0].blocks_users}')
[[ "$blocks" == '{"w":true,"a":true,"u":true}' ]] || fail "blocks_* flags: $blocks"
echo "PASS: data-drive-missing raised with severity=error, blocks_writes/apps/users=true"

# --- assertion 3: drive reattached, healthy ----------------------------

step "case C — drive reattached + canaries match: issue clears"
echo abc-123 > "$FIXTURE_ROOT/srv/moose/.canary"
echo abc-123 > "$FIXTURE_ROOT/var/lib/moose/.canary"
reporter
wait_for_issues ""
echo "PASS: data-drive-missing cleared on next poll"

# --- assertion 4: bind landed on wrong fs ------------------------------

step "case D — bind canary differs from data-drive canary: canary-mismatch"
echo stale-uuid > "$FIXTURE_ROOT/var/lib/moose/.canary"
reporter
wait_for_issues "canary-mismatch"
sev=$(issues | jq -r '.issues[0].severity')
[[ "$sev" == "critical" ]] || fail "severity: want critical, got $sev"
echo "PASS: canary-mismatch raised with severity=critical"

# --- assertion 5: wrong drive plugged in -------------------------------

step "case E — canary UUID does not match marker: data-drive-wrong"
echo xyz-999 > "$FIXTURE_ROOT/srv/moose/.canary"
echo xyz-999 > "$FIXTURE_ROOT/var/lib/moose/.canary"
reporter
wait_for_issues "data-drive-wrong"
sev=$(issues | jq -r '.issues[0].severity')
[[ "$sev" == "critical" ]] || fail "severity: want critical, got $sev"
echo "PASS: data-drive-wrong raised with severity=critical"

# --- assertion 6: admin-only enforcement -------------------------------

step "case F — unauthenticated GET is 401"
code=$(curl -sS -o /dev/null -w '%{http_code}' "$BRAIN/api/v1/health")
[[ "$code" == "401" ]] || fail "want 401 unauthenticated, got $code"
echo "PASS: GET /api/v1/health without cookie → 401"

echo
echo "ALL CASES PASSED"
