#!/usr/bin/env bash
# Build the hosted cloud BOOT-PROOF image (C2 #205; restructured #242): the
# production hosted image PLUS the serial-driven self-check. Since #242 promoted the
# first-boot runtime wiring (slim host-agent, networkd config, control-plane bundle,
# seed materializer) into the production image (dev/cloud/), this lane now adds ONLY
# the test harness on top — it boots the real production image, not a test-only
# superset. The cloud analogue of dev/test-qemu/bootstrap.sh, minus swtpm/LUKS/SSH
# (hosted cuts). dev/cloud/run-cloud-tests.sh calls this, then converts the raw to
# qcow2 and boots it in QEMU.
#
# The boot-proof image = the lean production image (Include=.. of dev/cloud/, which
# auto-detects dev/cloud/mkosi.postinst.chroot + dev/cloud/mkosi.extra.wiring/ for
# this lane too) + this lane's assertions extra + assertions-only postinst.
#
# Sequence:
#   1. Host preflight (mkosi v22+, qemu, qemu-img, OVMF, docker, go, libpam).
#   2. Stage the production first-boot wiring via the shared
#      dev/cloud/stage-control-plane.sh (the SAME staging the lean build runs).
#   3. Stage this lane's assertions (cloud-assertions.sh + its unit) into
#      dev/cloud/test/mkosi.extra/; the test postinst enables the unit.
#   4. Stage Docker's apt repo (trixie) so docker-ce resolves at build time.
#   5. `mkosi build` from dev/cloud/test/ → .dev/cloud-boot/moose-cloud.raw.
#
# Idempotent via .dev/cloud-boot/.cloud-boot-ready (versioned content gate).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../../.." && pwd)"
CLOUD_DIR="${REPO_ROOT}/dev/cloud"
TEST_DIR="${CLOUD_DIR}/test"
WORK="${REPO_ROOT}/.dev/cloud-boot"
EXTRA="${TEST_DIR}/mkosi.extra"          # this lane's assertions only
WIRING="${CLOUD_DIR}/mkosi.extra.wiring" # shared production wiring (ExtraTree of dev/cloud/)
PKGMNGR="${TEST_DIR}/mkosi.pkgmngr"
CP_BUNDLE="${REPO_ROOT}/.dev/control-plane"
CANARY="${WORK}/.cloud-boot-ready"
CANARY_VERSION="v22"  # bump when staging/mkosi.conf/repart changes require a clean rebuild
IMAGE_OUT="${WORK}/moose-cloud.raw"

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    echo "must run as root (mkosi escalates; QEMU+KVM later)" >&2
    exit 1
fi

# Resolve the invoking non-root user for build-artifact ownership.
CALLER="$(logname 2>/dev/null || true)"
if [ -z "$CALLER" ] || [ "$CALLER" = "root" ]; then CALLER="${SUDO_USER:-}"; fi
if [ "$CALLER" = "root" ]; then CALLER=""; fi
CALLER_HOME=""
[ -n "$CALLER" ] && CALLER_HOME="$(getent passwd "$CALLER" | cut -d: -f6)"
# Sudo strips PATH; fold the caller's ~/.local/bin back in so a pipx-installed
# mkosi is visible to the preflight probe.
if [ -n "$CALLER_HOME" ] && [ -d "$CALLER_HOME/.local/bin" ]; then
    PATH="$CALLER_HOME/.local/bin:$PATH"
fi

# --- 1. host preflight
missing=()
for tool in mkosi qemu-system-x86_64 qemu-img curl python3 docker; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
done
# host-agent-real is a CGO binary (PAM verify is kept in hosted); needs the headers.
[ -f /usr/include/security/pam_appl.h ] || missing+=("libpam0g-dev (PAM headers for host-agent-real)")
# OVMF (UEFI firmware) — location varies by distro.
OVMF_CODE=""
for cand in /usr/share/OVMF/OVMF_CODE.fd /usr/share/ovmf/OVMF.fd \
            /usr/share/OVMF/OVMF.fd /usr/share/edk2-ovmf/x64/OVMF_CODE.fd; do
    [ -r "$cand" ] && { OVMF_CODE="$cand"; break; }
done
[ -n "$OVMF_CODE" ] || missing+=("ovmf (UEFI firmware — package: ovmf)")
if [ ${#missing[@]} -gt 0 ]; then
    cat >&2 <<EOF
cloud boot-proof preflight: missing tooling
  ${missing[*]}

Install (Ubuntu/Debian):
  sudo apt-get install -y qemu-system-x86 ovmf curl python3 libpam0g-dev
  sudo apt-get install -y pipx && pipx install mkosi   # need v22+; ensure ~/.local/bin on PATH

After installing, re-run \`sudo make test-cloud-qemu\`.
EOF
    exit 1
fi

# mkosi version sanity (need >=22). Capture full output first so a SIGPIPE from
# head can't abort mkosi under pipefail (same guard as the lean/medium lanes).
mkosi_version_full="$(mkosi --version 2>&1 || true)"
mkosi_version="$(printf '%s\n' "$mkosi_version_full" | head -n1 | awk '{print $NF}' | tr -d v)"
mkosi_major="${mkosi_version%%[!0-9]*}"
if [ -n "$mkosi_major" ] && [ "$mkosi_major" -lt 22 ] 2>/dev/null; then
    echo "mkosi $mkosi_version too old (need >=22). pipx install --upgrade mkosi" >&2
    exit 1
fi

# Resolve go for the slim-agent build.
if [ -z "${GO:-}" ]; then GO="$(command -v go 2>/dev/null || true)"; fi
if [ -z "${GO:-}" ] && [ -n "$CALLER_HOME" ]; then
    for cand in "${CALLER_HOME}/.local/go/bin/go" "/usr/local/go/bin/go"; do
        [ -x "$cand" ] && { GO="$cand"; break; }
    done
fi
[ -n "${GO:-}" ] && [ -x "$GO" ] || { echo "go binary not found (\$GO=${GO:-})" >&2; exit 1; }

mkdir -p "$WORK"
[ -n "$CALLER" ] && chown "$CALLER":"$(id -gn "$CALLER")" "$WORK"

# Idempotency gate (re-runs cheap; mkosi caches incremental rebuilds).
if [ -f "$CANARY" ] && [ "$(cat "$CANARY")" = "$CANARY_VERSION" ] && [ -f "$IMAGE_OUT" ]; then
    echo "cloud boot-proof image already built ($IMAGE_OUT); skipping bootstrap"
    exit 0
fi

# --- 2. stage the production first-boot wiring (shared with the lean build) into
# dev/cloud/mkosi.extra.wiring/ — the ExtraTree of dev/cloud/ this lane inherits.
# shellcheck source=dev/cloud/stage-control-plane.sh
. "${CLOUD_DIR}/stage-control-plane.sh"
stage_control_plane

# --- 3. stage this lane's assertions on top of the production wiring. The
# serial-driven boot self-check + its oneshot are the ONLY test-only additions; the
# test postinst (dev/cloud/test/mkosi.postinst.chroot) enables the unit.
rm -rf "$EXTRA"
mkdir -p "$EXTRA/usr/local/bin" "$EXTRA/etc/systemd/system"
cp "${CLOUD_DIR}/cloud-assertions.sh" "$EXTRA/usr/local/bin/cloud-assertions.sh"
chmod 0755 "$EXTRA/usr/local/bin/cloud-assertions.sh"
cp "${TEST_DIR}/moose-cloud-assertions.service" "$EXTRA/etc/systemd/system/"

# --- 3b. app-install fixtures for the access-mode e2e (#308) — TEST-LANE ONLY. The
# access boot installs whoami air-gapped and drives the per-app forward-auth access
# modes through real Caddy. These land in the TEST ExtraTree (dev/cloud/test/
# mkosi.extra), NOT the shared production wiring ($WIRING) — the lean image ships no
# app and no offline-install mode. mkosi overlays both trees onto one rootfs, so the
# whoami tar sits alongside the control-plane bundle and the first-boot loader (which
# globs *.tar) docker-loads it too.
echo "baking whoami app image + catalog snapshot for the access-mode e2e (#308)..."
mkdir -p "$EXTRA/var/lib/moose/control-plane-images" \
         "$EXTRA/var/lib/moose" \
         "$EXTRA/etc/systemd/system/host-agent.service.d"

# whoami image: pull by DIGEST (not the mutable tag), re-tag to the v1.10.3 the
# compose + catalog reference — a save/load image carries no RepoDigest, so offline
# mode pins the tag. Same image + digest the medium lane bakes.
WHOAMI_REF="traefik/whoami@sha256:43a68d10b9dfcfc3ffbfe4dd42100dc9aeaf29b3a5636c856337a5940f1b4f1c"
docker pull "$WHOAMI_REF"
docker tag "$WHOAMI_REF" traefik/whoami:v1.10.3
docker save traefik/whoami:v1.10.3 -o "$EXTRA/var/lib/moose/control-plane-images/whoami.tar"

# Stage a local catalog snapshot with a whoami app: the lane is air-gapped, so there
# is no control plane to sync from, and the brain reads this file once at boot to
# seed its store (internal/catalog/remote.go # loadSnapshotFile, MOOSE_CATALOG_FILE).
# It is an input the brain never writes back — a box keeps no catalog on disk.
# mkcatalog generates it from the minimal hosted whoami package (pure routing, no
# folder grant — the access proof is the gate + strip, not bind mounts) and stamps
# the integrity digest the brain verifies. Built as the caller (warm Go cache) via
# stage_build_go, then run as root.
MKCATALOG_BIN="${WORK}/mkcatalog"
stage_build_go "$MKCATALOG_BIN" "${REPO_ROOT}/dev/mkcatalog/"
"$MKCATALOG_BIN" \
    -pkg "${TEST_DIR}/catalog/whoami" \
    -out "$EXTRA/var/lib/moose/catalog-seed.json"

# Offline-install env, layered over the shared 10-cloud-brain.conf drop-in (20- sorts
# after, so these win). host-agent-real forwards them into the brain container
# (cmd/host-agent-real/main.go → brainlaunch): OFFLINE_INSTALL trusts the docker-
# loaded image's catalog-promised digest instead of pulling, and the inert catalog
# URL makes the background sync fail fast so the staged snapshot stands. A real
# tenant box keeps the production default (pulls from the control plane, and sets no
# MOOSE_CATALOG_FILE at all) — these overrides exist only in the boot-proof image.
#
# MOOSE_UPDATE_TARGET_URL is inert here for a sharper reason (os#401): a hosted box
# applies its control-plane target WITHOUT a prompt, so a boot proof left pointing at
# the real control plane would, if the boot happened to land inside the 03:00-04:00
# window, pull the live fleet images and update the box under test mid-assertion.
# Pointing it at a dead port makes every boot's source unreachable, which the loop
# treats as a no-op — the behaviour the box owes an offline network anyway. The update
# boot overrides this from its SEED, not from a drop-in (os#407): the seed is the only
# channel a real box has for a per-box fact, and it outranks this variable.
cat > "$EXTRA/etc/systemd/system/host-agent.service.d/20-cloud-test-catalog.conf" <<'EOF'
[Service]
Environment=MOOSE_CATALOG_URL=http://127.0.0.1:9
Environment=MOOSE_CATALOG_FILE=/var/lib/moose/catalog-seed.json
Environment=MOOSE_OFFLINE_INSTALL=1
Environment=MOOSE_UPDATE_TARGET_URL=http://127.0.0.1:9
EOF

# --- 3c. registry image for the control-plane update proof (#382) — TEST-LANE ONLY.
# The update boot needs a real registry to pull FROM: the lane is air-gapped, and a
# `docker load` + retag would prove recreate and revert while quietly skipping the one
# step production always takes, `docker pull <ref>@sha256:...` (BUILD.md # 6 — boxes
# pull by digest). So the guest runs its own registry on 127.0.0.1:5000, pushes the
# target images into it, drops them from the local image store, and lets the updater
# pull them back by digest.
#
# It lands in a TEST-ONLY path, not /var/lib/moose/control-plane-images/: the first-boot
# loader globs that dir and would then load the registry on every boot of every
# scenario. The update assertion docker-loads this tar itself, so the other four boots
# never touch it. It is in the test ExtraTree, so the published production image does
# not carry it (that image is what `make build-cloud-image` lean-checks).
#
# Pinned by digest for the same reason whoami is: `registry:2` is a moving tag.
echo "baking the registry image for the control-plane update proof (#382)..."
mkdir -p "$EXTRA/var/lib/moose/test-images"
REGISTRY_REF="registry@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"
docker pull "$REGISTRY_REF"
docker tag "$REGISTRY_REF" registry:2
docker save registry:2 -o "$EXTRA/var/lib/moose/test-images/registry.tar"

# --- 4. Docker apt repo for the build's package manager (trixie pocket — the
# cloud image is Release=trixie). Build-host network only; the VM never apt-installs.
rm -rf "$PKGMNGR"
mkdir -p "$PKGMNGR/etc/apt/keyrings" "$PKGMNGR/etc/apt/sources.list.d"
curl -fsSL https://download.docker.com/linux/debian/gpg -o "$PKGMNGR/etc/apt/keyrings/docker.asc"
chmod a+r "$PKGMNGR/etc/apt/keyrings/docker.asc"
cat > "$PKGMNGR/etc/apt/sources.list.d/docker.list" <<'EOF'
deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian trixie stable
EOF

# --- 5. mkosi build (from dev/cloud/test; Include=.. pulls in the production base
# + its wiring). Re-own the staged trees + work dir to the caller; mkosi runs as
# $CALLER and auto-sudos for privileged ops. NOT $CP_BUNDLE (mkosi reads the tarball
# copies under $WIRING, and it is shared with the medium lane — leave it alone).
echo "building cloud boot-proof image via mkosi (first run takes a few minutes)..."
if [ -n "$CALLER" ]; then
    chown -R "$CALLER":"$(id -gn "$CALLER")" "$EXTRA" "$WIRING" "$WORK" "$PKGMNGR"
fi
MKOSI_BIN="$(command -v mkosi || true)"
[ -n "$MKOSI_BIN" ] || { echo "mkosi disappeared from PATH" >&2; exit 1; }

# mkosi's launcher needs python >=3.10; pass it through the sudo-stripped env.
MKOSI_INTERPRETER=""
for cand in python3.13 python3.12 python3.11 python3.10; do
    if path="$(command -v "$cand" 2>/dev/null)"; then MKOSI_INTERPRETER="$path"; break; fi
done
if [ -z "$MKOSI_INTERPRETER" ] && [ -n "$CALLER_HOME" ]; then
    for cand in "$CALLER_HOME/anaconda3/bin/python3" "$CALLER_HOME/miniconda3/bin/python3" \
                "$CALLER_HOME/.pyenv/shims/python3"; do
        if [ -x "$cand" ] && "$cand" -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null; then
            MKOSI_INTERPRETER="$cand"; break
        fi
    done
fi
[ -n "$MKOSI_INTERPRETER" ] || { echo "mkosi needs python >=3.10; none found" >&2; exit 1; }

if [ -n "$CALLER" ]; then
    sudo -u "$CALLER" env "MKOSI_INTERPRETER=$MKOSI_INTERPRETER" \
        "$MKOSI_BIN" --directory "$TEST_DIR" --force build
else
    MKOSI_INTERPRETER="$MKOSI_INTERPRETER" "$MKOSI_BIN" --directory "$TEST_DIR" --force build
fi

# mkosi writes to OutputDirectory=.dev/cloud-boot. Confirm the raw exists.
if [ ! -f "$IMAGE_OUT" ]; then
    for cand in "${WORK}/moose-cloud.raw" "${WORK}/moose-cloud"; do
        [ -f "$cand" ] && { ln -sf "$(basename "$cand")" "$IMAGE_OUT" 2>/dev/null || cp "$cand" "$IMAGE_OUT"; break; }
    done
fi
if [ ! -f "$IMAGE_OUT" ]; then
    echo "mkosi build did not produce $IMAGE_OUT" >&2
    ls -la "$WORK" >&2 || true
    exit 1
fi

echo -n "$CANARY_VERSION" > "$CANARY"
[ -n "$CALLER" ] && chown "$CALLER":"$(id -gn "$CALLER")" "$CANARY" "$IMAGE_OUT" 2>/dev/null || true
echo "cloud boot-proof image ready at $IMAGE_OUT"
