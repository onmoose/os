#!/usr/bin/env bash
# End-to-end smoke test for the DBus-based Avahi publisher in host-agent-real.
#
# What this verifies:
#   1. host-agent-real builds and starts
#   2. POST /v1/discovery/publish causes Avahi to announce <slug>.local
#   3. avahi-resolve actually returns an A record for that name
#   4. POST /v1/discovery/unpublish withdraws the announcement
#
# Requirements:
#   - avahi-daemon running (systemctl is-active avahi-daemon)
#   - libpam0g-dev installed (to build host-agent-real)
#   - No sudo: Avahi's default DBus policy allows any user to send to
#     org.freedesktop.Avahi, so the publisher works unprivileged. (Production
#     host-agent-real runs as root for PAM, not for this.)
#   - curl, avahi-resolve, jq (jq optional for pretty output)
#
# Usage:
#   dev/test-avahi-publisher.sh [slug]
#
# Default slug is "moosetest". Tests against ${slug}.local.
#
# Cleanup: kills host-agent-real and removes the socket on exit, even on
# failure. Avahi withdraws the name automatically when the DBus connection
# closes.

set -euo pipefail

SLUG="${1:-moosetest}"
NAME="${SLUG}.local"
SOCK="/tmp/moose-agent-test.sock"
BIN=".dev/host-agent-real"

cleanup() {
  if [[ -n "${PID:-}" ]]; then
    kill "$PID" 2>/dev/null || true
    wait "$PID" 2>/dev/null || true
  fi
  rm -f "$SOCK"
}
trap cleanup EXIT

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

# --- preflight --------------------------------------------------------------

systemctl is-active --quiet avahi-daemon || fail "avahi-daemon not running"
command -v avahi-resolve >/dev/null || fail "avahi-resolve not installed (apt install avahi-utils)"
command -v curl >/dev/null || fail "curl not installed"

# --- build ------------------------------------------------------------------

echo "==> Building host-agent-real..."
make host-agent-real

# --- start ------------------------------------------------------------------

echo "==> Starting host-agent-real on $SOCK..."
rm -f "$SOCK"
MOOSE_AGENT_SOCK="$SOCK" "./$BIN" &
PID=$!

# Wait for socket
for i in {1..20}; do
  [[ -S "$SOCK" ]] && break
  sleep 0.1
done
[[ -S "$SOCK" ]] || fail "socket $SOCK never appeared"

# --- pre-check --------------------------------------------------------------

echo "==> Pre-check: $NAME should NOT resolve yet..."
if avahi-resolve -n "$NAME" 2>/dev/null | grep -q "$NAME"; then
  fail "$NAME already resolves before publish — stale entry from a previous run?"
fi
echo "    OK: $NAME does not resolve"

# --- publish ----------------------------------------------------------------

echo "==> POST /v1/discovery/publish slug=$SLUG"
PUBLISH=$(curl --unix-socket "$SOCK" -sf -X POST http://localhost/v1/discovery/publish \
  -H 'Content-Type: application/json' \
  -d "{\"slug\":\"$SLUG\"}") || fail "publish returned non-2xx"
echo "    response: $PUBLISH"

# Give Avahi up to ~3 seconds to multicast
echo "==> Waiting for Avahi to announce..."
RESOLVED=""
for i in {1..15}; do
  if RESOLVED=$(avahi-resolve -n "$NAME" 2>/dev/null) && [[ -n "$RESOLVED" ]]; then
    break
  fi
  sleep 0.2
done
[[ -n "$RESOLVED" ]] || fail "$NAME never resolved (waited ~3s)"
echo "    resolved: $RESOLVED"

# --- unpublish --------------------------------------------------------------

echo "==> POST /v1/discovery/unpublish slug=$SLUG"
curl --unix-socket "$SOCK" -sf -X POST http://localhost/v1/discovery/unpublish \
  -H 'Content-Type: application/json' \
  -d "{\"slug\":\"$SLUG\"}" > /dev/null || fail "unpublish returned non-2xx"

echo "==> Verifying withdrawal..."
sleep 1
if avahi-resolve -n "$NAME" 2>/dev/null | grep -q "$NAME"; then
  fail "$NAME still resolves after unpublish"
fi
echo "    OK: $NAME withdrawn"

echo ""
echo "==> ALL CHECKS PASSED"
