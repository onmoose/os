#!/usr/bin/env bash
# Bootstrap the base rootfs for the nspawn test lane.
#
# Produces a minimal Debian rootfs at .dev/nspawn/rootfs/ with the
# user/group tooling we exercise (useradd, chpasswd, gpasswd, userdel)
# plus the `moose` and `sudo` groups pre-provisioned — matching the
# layout the real moose OS will ship.
#
# Source = `docker export debian:bookworm`. We use docker (already a
# project dep for Caddy) rather than mmdebstrap/debootstrap because
# bootstrapping Debian from an Ubuntu host fights with archive keyrings,
# subuid mappings, and other cross-distro friction that doesn't matter
# in CI. Output is a plain directory tree; nspawn doesn't care how it
# was assembled.
#
# Must run as root: systemd-nspawn needs CAP_SYS_ADMIN to apply the
# in-rootfs apt-get step. The rootfs ends up root-owned as a result;
# `sudo rm -rf .dev/nspawn/rootfs` is the cleanup path.
#
# Idempotent: if the rootfs already has the canary file, exit 0.
set -euo pipefail

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    echo "must run as root (systemd-nspawn requires CAP_SYS_ADMIN)" >&2
    exit 1
fi

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
ROOTFS="${REPO_ROOT}/.dev/nspawn/rootfs"
CANARY="${ROOTFS}/.moose-nspawn-ready"
IMAGE="${MOOSE_NSPAWN_IMAGE:-debian:bookworm}"

# Bumped to v2 when slice 0020 added systemd-sysv (needed for --boot in the
# boot-chain lane). Older v1 rootfs lacks /sbin/init; force a rebuild rather
# than silently failing inside the new lane.
CANARY_VERSION="v2"

if [ -f "$CANARY" ] && [ "$(cat "$CANARY")" = "$CANARY_VERSION" ]; then
    echo "nspawn rootfs already bootstrapped at $ROOTFS (sudo rm -rf to rebuild)"
    exit 0
fi
if [ -f "$CANARY" ]; then
    echo "nspawn rootfs at $ROOTFS is stale (canary != $CANARY_VERSION); rebuilding"
fi

for tool in docker tar systemd-nspawn; do
    if ! command -v "$tool" >/dev/null; then
        echo "$tool not installed" >&2
        exit 1
    fi
done

mkdir -p "$(dirname "$ROOTFS")"
rm -rf "$ROOTFS"
mkdir -p "$ROOTFS"

echo "pulling $IMAGE"
docker pull --quiet "$IMAGE" >/dev/null

CID="$(docker create "$IMAGE" /bin/true)"
trap 'docker rm -f "$CID" >/dev/null 2>&1 || true' EXIT
docker export "$CID" | tar -x -C "$ROOTFS"

# debian:bookworm ships with passwd + login but no sudo group. Add it
# (so SetRole's "admin → sudo" path is real), pre-provision the `moose`
# primary group at GID 3000 per FIRST_RUN.md # Identity & display names,
# and install libpam-modules so chpasswd can update /etc/shadow via PAM.
#
# systemd-sysv is needed by the boot-chain lane (slice 0020): it provides
# /sbin/init so `systemd-nspawn --boot` can find a PID 1. Cheap (~30 MB)
# and harmless for the no-boot lanes.
#
# Running apt-get under nspawn rather than `docker run` keeps the seam
# clean: the rootfs is only ever mutated through the same tool that
# runs the tests.
systemd-nspawn --quiet -D "$ROOTFS" --pipe \
    /bin/bash -c '
        set -e
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y -qq --no-install-recommends \
            sudo libpam-modules systemd-sysv
        getent group moose >/dev/null || groupadd -g 3000 moose
        apt-get clean
        rm -rf /var/lib/apt/lists/*
    '

echo -n "$CANARY_VERSION" > "$CANARY"
echo "nspawn rootfs ready at $ROOTFS"
