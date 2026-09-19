#!/usr/bin/env bash
# The Go half of `make dev`: run the fake host-agent and the brain, and rebuild
# both when a .go file changes.
#
# Why this exists: Vite watches the UI and reloads it, but nothing watched the
# Go side, so a brain edit only reached the running process on the next
# `make dev`. The symptom is never obvious — a route added in this session
# answers 405, a new response field silently arrives undefined — and it costs
# more time to diagnose than the rebuild costs to run.
#
# Two choices worth knowing:
#
#   * The debounce is long (10s by default) because most edits here arrive from
#     a coding agent, which writes a burst of files over several seconds. A
#     short debounce would rebuild and restart the brain in the middle of that
#     burst, repeatedly. Set MOOSE_DEV_DEBOUNCE=2 when editing by hand.
#   * The build runs BEFORE the running processes are stopped, into a staging
#     path. A build that fails leaves the old brain up and serving, so a typo
#     never costs you the stack. (Staging is also what avoids "text file busy":
#     Linux refuses to write over a running executable.)
#
# A restart drops open SSE streams — the dashboard reconnects on its own — and
# kills any install job in flight.
set -uo pipefail

GO=${GO:-go}
DEV_DIR=${DEV_DIR:-.dev}
LDFLAGS=${LDFLAGS:-}
WATCH_DIRS=${MOOSE_DEV_WATCH_DIRS:-cmd internal}
DEBOUNCE=${MOOSE_DEV_DEBOUNCE:-10}
POLL=${MOOSE_DEV_POLL:-2}

STAGE="$DEV_DIR/next"
STAMP="$DEV_DIR/.dev-go-stamp"
agent_pid=""
brain_pid=""

log() { printf '[watch] %s\n' "$*"; }

start_procs() {
  MOOSE_DEV_AVAHI=1 "$DEV_DIR/host-agent" > >(sed -u 's/^/[agent] /') 2>&1 &
  agent_pid=$!
  "$DEV_DIR/brain" > >(sed -u 's/^/[brain] /') 2>&1 &
  brain_pid=$!
}

# SIGTERM, then SIGKILL after a few seconds. The wait is bounded on purpose: a
# child that ignores TERM (a wedged brain, a slow flush) would otherwise hang
# the Ctrl-C that got you here.
stop_procs() {
  local pid alive waited=0
  # Unquoted on purpose: an empty pid drops out by word splitting.
  for pid in $brain_pid $agent_pid; do kill "$pid" 2>/dev/null; done
  while [ "$waited" -lt 5 ]; do
    alive=0
    for pid in $brain_pid $agent_pid; do kill -0 "$pid" 2>/dev/null && alive=1; done
    [ "$alive" = 0 ] && break
    sleep 1
    waited=$((waited + 1))
  done
  for pid in $brain_pid $agent_pid; do
    kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null
    wait "$pid" 2>/dev/null
  done
  brain_pid=""
  agent_pid=""
}

trap 'stop_procs; exit 0' INT TERM EXIT

# Anything modified since the last build started.
changed() {
  [ -n "$(find $WATCH_DIRS -name '*.go' -newer "$STAMP" -print -quit 2>/dev/null)" ]
}

# Anything modified inside the debounce window, i.e. the burst is still running.
settling() {
  [ -n "$(find $WATCH_DIRS -name '*.go' -newermt "-$DEBOUNCE seconds" -print -quit 2>/dev/null)" ]
}

# Build into the staging path. Both binaries, because a change in internal/ can
# belong to either one and working out which is not worth the seconds it saves.
build_go() {
  mkdir -p "$STAGE"
  touch "$STAMP"
  $GO build -ldflags "$LDFLAGS" -o "$STAGE/host-agent" ./cmd/host-agent || return 1
  $GO build -ldflags "$LDFLAGS" -o "$STAGE/brain" ./cmd/brain || return 1
}

# -newermt with a relative time is a GNU find extension. Without it the loop
# still runs, it just does not watch — better than refusing to start.
if ! find . -maxdepth 0 -newermt '-1 seconds' >/dev/null 2>&1; then
  log "this find has no -newermt, so Go changes will not rebuild; restart make dev after editing Go"
  start_procs
  wait
  exit 0
fi

touch "$STAMP"
start_procs

while true; do
  sleep "$POLL"
  changed || continue
  log "Go files changed, waiting ${DEBOUNCE}s for the edits to settle"
  while settling; do sleep "$POLL"; done
  log "rebuilding"
  if build_go; then
    stop_procs
    mv -f "$STAGE/host-agent" "$DEV_DIR/host-agent"
    mv -f "$STAGE/brain" "$DEV_DIR/brain"
    start_procs
    log "host-agent and brain restarted"
  else
    log "build failed, so the running brain was left alone; fix and save again"
  fi
done
