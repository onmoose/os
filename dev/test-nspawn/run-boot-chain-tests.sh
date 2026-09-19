#!/usr/bin/env bash
# Boot-chain fast-lane tests — systemd-nspawn --boot edition.
#
# Slice 0019 shipped systemd units in dist/systemd/ + the
# moose-storage-verify reporter; nothing booted them in CI. This script
# closes that gap per docs/specs/TESTING.md # Fast lane.
#
# Sequence:
#   1. bootstrap-if-absent the cached rootfs (.dev/nspawn/rootfs)
#   2. build moose-storage-verify statically
#   3. stage units into .dev/nspawn/boot-stage/etc/systemd/system/
#      with stub parent units for docker/smbd/avahi-daemon (so drop-ins
#      have something to attach to — systemd does not surface orphans)
#   4. boot the rootfs ephemerally, bind the staging tree onto
#      /etc/systemd/system, bind the binary at /usr/lib/moose/...,
#      and drop in a moose-boot-test.service oneshot that runs
#      boot-assertions.sh after multi-user.target
#   5. read the PASS|FAIL verdict from a bind-mounted result file
#
# Invoke as root: `sudo make test-boot-chain-nspawn`.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ROOTFS="${REPO_ROOT}/.dev/nspawn/rootfs"
WORK="${REPO_ROOT}/.dev/nspawn"
STAGE="${WORK}/boot-stage"
VERIFY_BIN="${WORK}/moose-storage-verify"
RESULT_FILE="$(mktemp -t moose-boot-result.XXXXXX)"
trap 'rm -f "$RESULT_FILE"' EXIT

# Same caller-resolution dance as run-usermgr-tests.sh — under
# `sudo make`, SUDO_USER may be root. logname follows the controlling
# tty's login.
CALLER="$(logname 2>/dev/null || true)"
if [ -z "$CALLER" ] || [ "$CALLER" = "root" ]; then
    CALLER="${SUDO_USER:-}"
fi
if [ -z "$CALLER" ] || [ "$CALLER" = "root" ]; then
    CALLER=""
fi
CALLER_HOME=""
if [ -n "$CALLER" ]; then
    CALLER_HOME="$(getent passwd "$CALLER" | cut -d: -f6)"
fi

# Ensure the artifact dir is owned by the invoking user before bootstrap
# touches it.
if [ -n "$CALLER" ]; then
    install -d -o "$CALLER" -g "$(id -gn "$CALLER")" "$WORK"
else
    mkdir -p "$WORK"
fi

# Resolve `go`. Under sudo, HOME points at root and a per-user Go install
# may not be on PATH.
if [ -z "${GO:-}" ]; then
    GO="$(command -v go 2>/dev/null || true)"
fi
if [ -z "${GO:-}" ] && [ -n "$CALLER_HOME" ]; then
    for cand in "${CALLER_HOME}/.local/go/bin/go" "/usr/local/go/bin/go"; do
        if [ -x "$cand" ]; then GO="$cand"; break; fi
    done
fi
if [ -z "${GO:-}" ] || [ ! -x "$GO" ]; then
    echo "go binary not found (\$GO=${GO:-}, CALLER=${CALLER:-}, PATH=$PATH)" >&2
    exit 1
fi

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    echo "must run as root (systemd-nspawn requires CAP_SYS_ADMIN)" >&2
    exit 1
fi

# --- 1. bootstrap
if [ ! -f "${ROOTFS}/.moose-nspawn-ready" ] \
   || [ "$(cat "${ROOTFS}/.moose-nspawn-ready" 2>/dev/null || true)" != "v2" ]; then
    "${REPO_ROOT}/dev/test-nspawn/bootstrap.sh"
fi

# --- 2. build verifier statically (no Go toolchain in the rootfs)
if [ -n "$CALLER" ]; then
    sudo -u "$CALLER" env CGO_ENABLED=0 "$GO" build -o "$VERIFY_BIN" \
        "${REPO_ROOT}/cmd/moose-storage-verify/"
else
    CGO_ENABLED=0 "$GO" build -o "$VERIFY_BIN" \
        "${REPO_ROOT}/cmd/moose-storage-verify/"
fi

# --- 3. stage units
# Layout matches dist/systemd/README.md # Layout:
#   /etc/systemd/system/<unit>
#   /etc/systemd/system/<unit>.d/moose.conf  (drop-ins)
# Plus stub parent units for docker/smbd/avahi-daemon so drop-ins attach,
# plus the test-driver service that runs the assertions.
rm -rf "$STAGE"
mkdir -p "$STAGE/etc/systemd/system"
SYS="$STAGE/etc/systemd/system"

# Real units + targets from dist/systemd/.
cp "${REPO_ROOT}/dist/systemd/moose-storage-ready.target"   "$SYS/"
cp "${REPO_ROOT}/dist/systemd/moose-storage-verify.service" "$SYS/"
cp "${REPO_ROOT}/dist/systemd/moose-recovery.target"        "$SYS/"
cp "${REPO_ROOT}/dist/systemd/host-agent.service"           "$SYS/"

# Drop-ins. dist/systemd/dropins/<unit>.service.d/moose.conf
# → /etc/systemd/system/<unit>.service.d/moose.conf
for d in "${REPO_ROOT}/dist/systemd/dropins"/*.service.d; do
    unit_dir="$(basename "$d")"   # e.g. docker.service.d
    mkdir -p "$SYS/$unit_dir"
    cp "$d/moose.conf" "$SYS/$unit_dir/"
done

# Stub parent units. systemd-nspawn rootfs has no docker/smbd/avahi
# installed; a drop-in without a parent unit is silently ignored by
# `systemctl cat`. The stub satisfies the lookup; ExecStart=/bin/true
# means even if something pulls them in, they exit clean.
for svc in docker smbd avahi-daemon; do
    cat >"$SYS/${svc}.service" <<EOF
[Unit]
Description=Stub ${svc}.service for boot-chain test rootfs (slice 0020)
# Real unit not installed in the nspawn rootfs; this stub exists only
# so dist/systemd/dropins/${svc}.service.d/moose.conf has a parent to
# attach to. Do not copy to production.

[Service]
Type=oneshot
ExecStart=/bin/true
RemainAfterExit=yes
EOF
done

# Test driver: oneshot that runs the assertion script after the system
# settles, then powers off. RemainAfterExit=yes ensures `systemctl
# start` waits for the assertions to finish. ExecStopPost=poweroff
# --force skips the second-stage shutdown that can hang under nspawn.
cat >"$SYS/moose-boot-test.service" <<'EOF'
[Unit]
Description=moose boot-chain assertion driver (slice 0020)
After=basic.target
Wants=basic.target
# Avoid waiting for multi-user.target — in a minimal rootfs that target
# may hang on getty/console services. basic.target is enough for the
# systemctl operations the assertion script performs.

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStartPre=/bin/sh -c 'echo "FAIL: ExecStart never reached" > /var/lib/moose-boot-result'
ExecStart=/usr/local/bin/boot-assertions.sh
# The script itself execs poweroff after writing the verdict. This
# ExecStopPost is the belt-and-suspenders fallback if the script exits
# without doing so (e.g. the trap fires before reaching the script's
# tail).
ExecStopPost=/bin/systemctl --no-block poweroff
StandardOutput=journal+console
StandardError=journal+console

[Install]
WantedBy=basic.target
EOF
# Enable via symlink (no systemctl available at staging time).
mkdir -p "$SYS/basic.target.wants"
ln -sf ../moose-boot-test.service "$SYS/basic.target.wants/moose-boot-test.service"

# Our --bind-ro replaces /etc/systemd/system wholesale, which drops the
# rootfs's default.target symlink. Without a default.target, systemd
# falls back to /lib/systemd/system/default.target which in bookworm
# points at graphical.target — that hangs forever waiting for a display
# manager. Pin default.target → multi-user.target so boot completes.
ln -sf /lib/systemd/system/multi-user.target "$SYS/default.target"

# Drop-in for host-agent.service: in the nspawn rootfs, docker.service
# is a stub that succeeds. Requires=docker.service should resolve fine.
# We do NOT want host-agent.service to actually start (its ExecStart is
# stubbed and would race the assertions), so we prevent it: rely on the
# fact that host-agent.service has [Install] WantedBy=multi-user.target
# only — no .wants symlink is created at staging time, so the unit is
# present but inactive. The assertions query it via systemctl show, not
# is-active.

# Make staging readable inside nspawn (which runs with mapped uids by
# default; readable-by-other is the simplest guarantee).
chmod -R a+rX "$STAGE"

# Result file: bind-mounted RW so the assertions can write back.
: > "$RESULT_FILE"
chmod a+rw "$RESULT_FILE"

# --- 4. boot
# --bind-ro on the staging tree replaces the rootfs's
# /etc/systemd/system. --bind on the result file is RW; we use a path
# under /var/lib/ because systemd remounts /run, /tmp as fresh tmpfs at
# PID 1 setup and would mask any bind there.
# --bind-ro for the verifier binary + assertions script.
# /bin/true stubs host-agent-real (its ExecStart path must exist for
# the unit to load, even though we never start the unit).
# Launch nspawn in the background; the host driver polls $RESULT_FILE and
# tears down the container as soon as a verdict appears. The container's
# in-container shutdown path (kill -SIGRTMIN+4 1 / systemctl poweroff) is
# unreliable in our minimal rootfs — graceful shutdown stalls on a
# StopJob timeout. Polling + SIGKILL keeps the fast lane fast.
echo "starting nspawn boot test..."
systemd-nspawn \
    --quiet \
    --ephemeral \
    --private-network \
    --register=no \
    --directory="$ROOTFS" \
    --bind-ro="$STAGE/etc/systemd/system:/etc/systemd/system" \
    --bind-ro="$VERIFY_BIN:/usr/lib/moose/moose-storage-verify" \
    --bind-ro=/bin/true:/usr/lib/moose/host-agent-real \
    --bind-ro="${REPO_ROOT}/dev/test-nspawn/boot-assertions.sh:/usr/local/bin/boot-assertions.sh" \
    --bind="$RESULT_FILE:/var/lib/moose-boot-result" \
    --boot &
NSPAWN_PID=$!

# Poll up to 30s for a verdict. Boot to assertions runs in 3-5s typically.
for _i in $(seq 1 300); do
    if [ -s "$RESULT_FILE" ] \
       && grep -qE '^(PASS|FAIL:)' "$RESULT_FILE" 2>/dev/null; then
        break
    fi
    if ! kill -0 "$NSPAWN_PID" 2>/dev/null; then
        break
    fi
    sleep 0.1
done

# Tear down nspawn aggressively — graceful shutdown is unreliable here
# and we already have the verdict. SIGKILL the systemd-nspawn supervisor
# which in turn kills the container's PID 1 and reaps the namespace.
if kill -0 "$NSPAWN_PID" 2>/dev/null; then
    kill -KILL "$NSPAWN_PID" 2>/dev/null || true
fi
# Capture the rc without aborting under set -e. We expect rc=137
# (SIGKILL) in the happy path because we explicitly killed the container
# after harvesting the verdict.
NSPAWN_RC=0
wait "$NSPAWN_PID" 2>/dev/null || NSPAWN_RC=$?
# rc=137 (SIGKILL) is expected and benign — we explicitly killed the
# container after capturing the verdict. Quiet success path; surface only
# truly anomalous codes.
if [ "$NSPAWN_RC" -ne 0 ] && [ "$NSPAWN_RC" -ne 137 ] && [ "$NSPAWN_RC" -ne 143 ]; then
    echo "warning: nspawn exited rc=$NSPAWN_RC" >&2
fi

# --- 5. verdict
VERDICT="$(cat "$RESULT_FILE" 2>/dev/null || true)"
if [ -z "$VERDICT" ]; then
    echo "FAIL: no verdict written to result file (nspawn rc=$NSPAWN_RC)" >&2
    exit 1
fi
if [ "$VERDICT" = "PASS" ]; then
    echo "boot-chain tests: PASS"
    exit 0
fi
echo "boot-chain tests: $VERDICT" >&2
exit 1
