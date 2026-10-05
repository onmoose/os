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
CANARY_VERSION="v30"  # bump when staging/mkosi.conf/repart changes require a clean rebuild
# A change to the OS package lock (#560) must rebuild too, so all three lock
# files are part of the canary. The resolved list is in it as well: a re-run
# after only the list changed must not exit early and skip os_lock_check below.
# The keyring mode is in it too (#562): a build for release bakes another
# /etc/rauc/keyring.pem.
CANARY_VERSION="${CANARY_VERSION}-rauc-${MOOSE_RAUC_KEYRING:-throwaway}"
# And where the brain and UI come from (#566): a build of this commit, or the
# last released pair from ghcr, at which control-plane version.
CANARY_VERSION="${CANARY_VERSION}-cp-${MOOSE_CONTROL_PLANE_SOURCE:-local}-$(tr -d '[:space:]' < "${REPO_ROOT}/CONTROL_PLANE_VERSION")"
CANARY_VERSION="${CANARY_VERSION}-lock-$(cat "${REPO_ROOT}/dev/os-lock/debian-snapshot" "${REPO_ROOT}/dev/os-lock/third-party.lock" "${REPO_ROOT}/dev/os-lock/cloud-packages.lock" | sha256sum | cut -c1-12)"
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
# OVMF (UEFI firmware): a CODE image and its VARS template, as a pair, from the
# list in dev/cloud/ovmf.sh, which run-cloud-tests.sh uses too (#575). Only
# when UEFI is one of the firmwares the boots run under.
# shellcheck source=dev/cloud/ovmf.sh
. "${CLOUD_DIR}/ovmf.sh"
if ovmf_wanted && ! ovmf_find; then
    missing+=("ovmf (UEFI firmware with its VARS template, package: ovmf)")
fi
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
# What this lane writes into /etc at run time: host-agent drop-ins that point
# it at the in-guest update targets. They go on a keep list of their own, so a
# Debian-major tidy-up (BUILD.md # 1b, rule 4) keeps them across the os-update
# boot's faked major, and the end-of-boot check of the upper layer accepts them.
mkdir -p "$EXTRA/usr/lib/moose/etc-keep.d"
cat > "$EXTRA/usr/lib/moose/etc-keep.d/boot-lane.list" <<'EOF'
# The boot lane's own writes into /etc (dev/cloud/test/bootstrap.sh).
systemd/system/host-agent.service.d
EOF

# The OS update trial's safety net fires after 90 s here instead of 15 min
# (#563), so the os-revert boot does not sit out a quarter of an hour. A
# healthy trial boot marks its slot good about 17 s after the switch in CI
# (run 37031756819), which the os-update boot proves under the same setting.
# The image that ships keeps 15 min.
# What GRUB left in the grubenv for this boot, logged before host-agent can
# mark anything (#563): GRUB sets the booted slot's TRY=1, and the boot lane
# checks it did, under both firmwares (#575). RequiresMountsFor: the ESP is
# `nofail` in fstab, so local-fs.target does not wait for it, and a read
# before the mount found no grubenv (run 37063254219).
cat > "$EXTRA/etc/systemd/system/moose-test-grubenv.service" <<'EOF'
[Unit]
Description=moose test: log the grubenv GRUB left for this boot
Before=host-agent.service
After=local-fs.target
RequiresMountsFor=/efi
DefaultDependencies=no

[Service]
Type=oneshot
ExecStart=/bin/sh -c 'echo "grubenv at boot: $(grub-editenv /efi/grub/grubenv list | tr "\n" " ")"'

[Install]
WantedBy=multi-user.target
EOF
# A slot that dies before userspace (#575), for the last stage of the
# os-revert boot. This initramfs hook runs right after moose-state, so the
# state partition is mounted at ${rootmnt}/state. When the state partition
# holds moose-test/panic-slot-<slot> for the booted slot, it leaves a note and
# crashes the kernel: a real kernel panic, before systemd, which panic=10
# turns into a reboot. GRUB must then skip the slot, because it saved the
# slot's TRY=1 before it booted it. The hook is in the boot-proof image only,
# so in the test bundle too (it is this image's slot), never in the image
# that ships. mkosi copies the extra trees before any postinst runs, and
# dev/cloud/mkosi.postinst.chroot builds the initramfs, so the hook is in it.
# If it ever is not, slot B boots in full and stage 4 fails on the slot it
# booted.
mkdir -p "$EXTRA/etc/initramfs-tools/scripts/local-bottom"
cat > "$EXTRA/etc/initramfs-tools/scripts/local-bottom/moose-test-panic" <<'EOF'
#!/bin/sh
PREREQ="moose-state"
prereqs() { echo "$PREREQ"; }
case "$1" in
    prereqs) prereqs; exit 0 ;;
esac
. /scripts/functions
slot=""
for arg in $(cat /proc/cmdline); do
    case "$arg" in rauc.slot=*) slot="${arg#rauc.slot=}" ;; esac
done
[ -n "$slot" ] || exit 0
[ -e "${rootmnt}/state/moose-test/panic-slot-${slot}" ] || exit 0
echo "moose-test: slot ${slot} panics before userspace, as the os-revert boot asked" > /dev/kmsg
echo "${slot}" > "${rootmnt}/state/moose-test/panicked-slot-${slot}"
echo s > /proc/sysrq-trigger
sleep 2
echo c > /proc/sysrq-trigger
# Not reached when the crash works. If it did not, fail the initramfs instead:
# panic= reboots that too.
panic "moose-test: slot ${slot}: the kernel crash did not happen"
EOF
chmod 0755 "$EXTRA/etc/initramfs-tools/scripts/local-bottom/moose-test-panic"

mkdir -p "$EXTRA/etc/systemd/system/moose-os-trial.timer.d"
cat > "$EXTRA/etc/systemd/system/moose-os-trial.timer.d/10-cloud-test.conf" <<'EOF'
[Timer]
OnBootSec=
OnBootSec=90s
EOF

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

# filedrop image (#519): busybox, pinned by the same index digest its manifest
# promises and re-tagged to the tag its compose names. The access boot installs
# filedrop twice, household and personal, to prove both folder sources on a
# booted box. It is small (about 2 MB), so it rides the first-boot loader like
# whoami instead of a test-only dir.
BUSYBOX_REF="busybox@sha256:bdf57e528e45e4433820e045b29b4597825a1c9e38353532d90a01445013f82e"
docker pull "$BUSYBOX_REF"
docker tag "$BUSYBOX_REF" busybox:1.37.0
docker save busybox:1.37.0 -o "$EXTRA/var/lib/moose/control-plane-images/filedrop.tar"

# imageuser images (#537): two synthetic images built here from the same
# busybox, each with its own USER and a /baked dir owned by that user. One
# names the user by number (1001), the other by name (app2, uid 1002 in its
# /etc/passwd). The imageuser fixture declares image_user: true. The image
# runs the userns remap (#530), and the access boot checks that the app runs
# remapped as its images' users. Built with the classic builder so no buildx or
# registry is needed:
# the FROM image is already local. Together they add about 2 MB, since the
# layers they share with busybox are saved once.
IMAGEUSER_CTX="${WORK}/imageuser-build"
rm -rf "$IMAGEUSER_CTX"
mkdir -p "$IMAGEUSER_CTX"
cat > "$IMAGEUSER_CTX/Dockerfile" <<'EOF'
FROM busybox:1.37.0
ARG IMAGE_USER
RUN addgroup -g 1001 app1 && adduser -D -H -u 1001 -G app1 app1 \
 && addgroup -g 1002 app2 && adduser -D -H -u 1002 -G app2 app2 \
 && mkdir /baked && chown "${IMAGE_USER}:${IMAGE_USER}" /baked && chmod 0755 /baked
USER ${IMAGE_USER}
EOF
DOCKER_BUILDKIT=0 docker build -q --build-arg IMAGE_USER=1001 -t moose-test/imageuser:1 "$IMAGEUSER_CTX"
DOCKER_BUILDKIT=0 docker build -q --build-arg IMAGE_USER=app2 -t moose-test/imageuser-named:1 "$IMAGEUSER_CTX"
docker save moose-test/imageuser:1 moose-test/imageuser-named:1 \
    -o "$EXTRA/var/lib/moose/control-plane-images/imageuser.tar"

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
    -pkg "${TEST_DIR}/catalog/filedrop" \
    -pkg "${TEST_DIR}/catalog/imageuser" \
    -pkg "${TEST_DIR}/catalog/remapdrop" \
    -pkg "${TEST_DIR}/catalog/svcdrop" \
    -pkg "${TEST_DIR}/catalog/rootsetup" \
    -pkg "${TEST_DIR}/catalog/pgnote" \
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

# --- 3d. postgres:16 for the remap boots (#531), TEST-LANE ONLY. The remap boot
# installs pgnote, a synthetic app on the managed Postgres 16, and the managed
# service runs this image. pgnote uses the same image for its psql client. It is
# about 150 MB, so it goes to the test-only dir like the registry: only the remap
# boot loads it, and the other boots never pay for it. The remapdrop, svcdrop
# and rootsetup fixtures use the busybox image the first-boot loader already
# loads. Pinned by digest because `postgres:16` is a moving tag.
echo "baking postgres:16 for the userns-remap boots (#531)..."
POSTGRES_REF="postgres@sha256:1a6ab3f5345eb6dbe04a1349529caabdb0ab09293a09590fad07b2246bfa4b54"
docker pull "$POSTGRES_REF"
docker tag "$POSTGRES_REF" postgres:16
docker save postgres:16 -o "$EXTRA/var/lib/moose/test-images/postgres-16.tar"

# --- 4. the OS package lock (#560): the same apt sources, snapshot and pins as
# the lean build (dev/os-lock/os-lock.sh), so this image installs the versions
# the shipped image does. Build-host network only; the VM never apt-installs.
# shellcheck source=dev/os-lock/os-lock.sh
. "${REPO_ROOT}/dev/os-lock/os-lock.sh"
stage_os_lock_apt "$PKGMNGR"
OS_LOCK_SNAPSHOT="$(os_lock_snapshot)"

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

# MOOSE_MKOSI_TOOLS_TREE (opt-in): use this tools tree instead of building the
# default one. CI sets it to dev/cloud/mkosi.tools, which the lean build made a
# minute earlier in the same job from the same settings, and saves about 40 s.
# Off by default: a local tree from an older build is not checked for being
# current when it is named by path.
tools_tree_args=()
if [ -n "${MOOSE_MKOSI_TOOLS_TREE:-}" ]; then
    [ -d "$MOOSE_MKOSI_TOOLS_TREE" ] || { echo "MOOSE_MKOSI_TOOLS_TREE='${MOOSE_MKOSI_TOOLS_TREE}' is not a directory" >&2; exit 1; }
    echo "using the tools tree ${MOOSE_MKOSI_TOOLS_TREE} (MOOSE_MKOSI_TOOLS_TREE)"
    tools_tree_args=(--tools-tree "$MOOSE_MKOSI_TOOLS_TREE")
fi

if [ -n "$CALLER" ]; then
    sudo -u "$CALLER" env "MKOSI_INTERPRETER=$MKOSI_INTERPRETER" \
        "$MKOSI_BIN" --directory "$TEST_DIR" "${tools_tree_args[@]}" --snapshot "$OS_LOCK_SNAPSHOT" --force build
else
    MKOSI_INTERPRETER="$MKOSI_INTERPRETER" "$MKOSI_BIN" --directory "$TEST_DIR" "${tools_tree_args[@]}" --snapshot "$OS_LOCK_SNAPSHOT" --force build
fi

# The boot-proof image is a second, separate build of the same package set, so
# it must resolve to the same committed lock as the lean one. In CI this is the
# "two builds of one commit install the same versions" proof (#560). Never in
# record mode: only the lean build records.
TEST_MANIFEST="$(ls -1 "$WORK"/*.manifest 2>/dev/null | head -n1 || true)"
[ -n "$TEST_MANIFEST" ] || { echo "no package manifest under $WORK" >&2; exit 1; }
MOOSE_OS_LOCK_RECORD= os_lock_check "$TEST_MANIFEST" "$WORK/cloud-packages.lock"

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

# The slot budget, reported but not gated (#561, dev/cloud/slot-budget.sh). The
# 60% gate is on the image that ships (dev/cloud/bootstrap.sh); this one also
# carries test-only images, so it only has to fit its 1 GiB slot, which
# systemd-repart already enforces when it builds the image.
# shellcheck source=dev/cloud/slot-budget.sh
. "${CLOUD_DIR}/slot-budget.sh"
slot_budget_check "$IMAGE_OUT" 100 "OS slot use (the boot-proof image, with test-only images; not gated)"

echo -n "$CANARY_VERSION" > "$CANARY"
[ -n "$CALLER" ] && chown "$CALLER":"$(id -gn "$CALLER")" "$CANARY" "$IMAGE_OUT" 2>/dev/null || true
echo "cloud boot-proof image ready at $IMAGE_OUT"
