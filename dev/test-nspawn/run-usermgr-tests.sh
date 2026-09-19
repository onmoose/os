#!/usr/bin/env bash
# Run the usermgrtest-tagged integration tests inside systemd-nspawn.
#
# Why nspawn: the tests in internal/hostagent/usermgr/integration_test.go
# touch /etc/passwd, /etc/shadow and /etc/group via useradd/chpasswd/
# gpasswd/userdel — too destructive for a developer laptop, but cheap
# and disposable inside an ephemeral namespace. See docs/specs/TESTING.md
# # Fast lane.
#
# Invoke as root: `sudo make test-usermgr-nspawn`.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ROOTFS="${REPO_ROOT}/.dev/nspawn/rootfs"
TESTBIN="${REPO_ROOT}/.dev/nspawn/usermgr.test"

# Walk back to the invoking non-root user. SUDO_USER is unreliable
# under nested sudo (`sudo make` → recipe-internal `sudo -E ...` sets
# SUDO_USER=root); logname follows the controlling tty's login.
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
else
    echo "warning: cannot determine invoking user (logname/SUDO_USER both root or empty);" \
         "building test binary as root and Go's module/build cache will be root-owned" >&2
fi

# Ensure the build artifact dir exists and is owned by the invoking user
# BEFORE bootstrap.sh runs. Otherwise bootstrap creates .dev/nspawn/ as
# root and the subsequent `sudo -u $CALLER go test -c -o ...` aborts on a
# fresh clone. Idempotent: install -d on an existing dir just refreshes
# ownership.
BUILD_DIR="$(dirname "$TESTBIN")"
if [ -n "$CALLER" ]; then
    install -d -o "$CALLER" -g "$(id -gn "$CALLER")" "$BUILD_DIR"
else
    mkdir -p "$BUILD_DIR"
fi

# Resolve `go`. Under sudo, HOME is root's so a per-user Go install
# isn't on PATH.
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

# Bootstrap the rootfs if absent. bootstrap.sh requires root (it uses
# nspawn to provision packages inside the rootfs); inherit our caller's
# sudo by running it as the current EUID.
if [ ! -f "${ROOTFS}/.moose-nspawn-ready" ]; then
    "${REPO_ROOT}/dev/test-nspawn/bootstrap.sh"
fi

# Build the test binary statically so the rootfs doesn't need a Go
# toolchain. CGO_ENABLED=0 also sidesteps the global CGO_CFLAGS=-D_GNU_SOURCE
# we set for pamverifier — usermgr is pure Go. Drop to the invoking
# user so the build cache and module cache stay user-owned.
if [ -n "$CALLER" ]; then
    sudo -u "$CALLER" env CGO_ENABLED=0 "$GO" test -c \
        -tags usermgrtest \
        -o "$TESTBIN" \
        "${REPO_ROOT}/internal/hostagent/usermgr/"
else
    CGO_ENABLED=0 "$GO" test -c \
        -tags usermgrtest \
        -o "$TESTBIN" \
        "${REPO_ROOT}/internal/hostagent/usermgr/"
fi

# --ephemeral: never mutates the cached rootfs (each run gets a fresh
# overlay). --bind-ro: the test binary appears at /usermgr.test inside.
# -E PATH: chpasswd lives in /usr/sbin which isn't in the default
# nspawn env PATH.
exec systemd-nspawn \
    --quiet \
    --ephemeral \
    --directory="$ROOTFS" \
    --bind-ro="$TESTBIN:/usermgr.test" \
    -E PATH=/usr/sbin:/usr/bin:/sbin:/bin \
    /usermgr.test -test.v
