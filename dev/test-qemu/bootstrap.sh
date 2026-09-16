#!/usr/bin/env bash
# Prepare the medium-lane test image (slice 0021).
#
# Sequence:
#   1. Probe host for mkosi, swtpm, qemu-system-x86_64, OVMF firmware.
#      Exit with a clear install pointer if anything is missing — we
#      don't auto-apt-install host packages (these are user decisions).
#   2. Build the storage-verify binary statically.
#   3. Generate a test SSH keypair under .dev/qemu/ if absent.
#   4. Stage mkosi.extra/ with: dist/systemd/ units at their on-target
#      paths, the storage-verify binary at /usr/lib/moose/, root's
#      authorized_keys, and sshd config drop-in.
#   5. Invoke `mkosi build` (cached after first run).
#
# Idempotent via .dev/qemu/.moose-medium-ready (versioned content gate,
# same idiom as dev/test-nspawn/bootstrap.sh).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
TEST_DIR="${REPO_ROOT}/dev/test-qemu"
WORK="${REPO_ROOT}/.dev/qemu"
EXTRA="${TEST_DIR}/mkosi.extra"
CANARY="${WORK}/.moose-medium-ready"
CANARY_VERSION="v30"  # bump when mkosi.conf changes require a clean rebuild
PASSPHRASE_FILE="${TEST_DIR}/mkosi.passphrase"  # LUKS recovery key (slice 0023); gitignored
IMAGE_OUT="${WORK}/moose-medium.raw"
SSH_KEY="${WORK}/ssh-key"

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    echo "must run as root (mkosi + qemu+swtpm need privileged ops later)" >&2
    exit 1
fi

# Resolve invoking non-root user for file ownership of build artifacts.
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

# Sudo strips PATH (secure_path in sudoers); fold the caller's user-local
# bin dir back in so tools like mkosi installed via `pipx`/`pip --user`/
# symlinked into ~/.local/bin/ are visible to the preflight probe below.
if [ -n "$CALLER_HOME" ] && [ -d "$CALLER_HOME/.local/bin" ]; then
    PATH="$CALLER_HOME/.local/bin:$PATH"
fi

# --- 1. host preflight
preflight_missing=()
for tool in mkosi swtpm qemu-system-x86_64 qemu-img ssh ssh-keygen scp curl docker; do
    if ! command -v "$tool" >/dev/null 2>&1; then
        preflight_missing+=("$tool")
    fi
done

# host-agent-real is a CGO binary (PAM); building it (#164, brain bootstrap)
# needs the libpam development headers on the build host.
if [ ! -f /usr/include/security/pam_appl.h ]; then
    preflight_missing+=("libpam0g-dev (PAM headers for host-agent-real)")
fi

# OVMF firmware location varies by distro.
OVMF_CODE=""
for cand in /usr/share/OVMF/OVMF_CODE.fd \
            /usr/share/ovmf/OVMF.fd \
            /usr/share/OVMF/OVMF.fd \
            /usr/share/edk2-ovmf/x64/OVMF_CODE.fd; do
    if [ -r "$cand" ]; then OVMF_CODE="$cand"; break; fi
done
if [ -z "$OVMF_CODE" ]; then
    preflight_missing+=("ovmf (UEFI firmware — install package: ovmf)")
fi

if [ ${#preflight_missing[@]} -gt 0 ]; then
    cat >&2 <<EOF
medium-lane host preflight: missing tooling
  ${preflight_missing[*]}

Install (Ubuntu/Debian):
  sudo apt-get install -y qemu-system-x86 swtpm ovmf openssh-client libpam0g-dev
  # mkosi v22+: Ubuntu 20.04's apt has v9 (too old). Install via pipx:
  sudo apt-get install -y pipx
  pipx install mkosi
  # ensure ~/.local/bin is on PATH (or /root/.local/bin under sudo)

After installing, re-run \`sudo make test-medium-qemu\`.
EOF
    exit 1
fi

# mkosi version sanity (need >=22 for the config schema we use). Capture
# the whole output first to avoid SIGPIPE killing mkosi when `head -1`
# closes the pipe early (mkosi 27 surfaces this; `set -e` + `pipefail`
# would then abort us with rc=141).
mkosi_version_full="$(mkosi --version 2>&1 || true)"
mkosi_version_line="$(printf '%s\n' "$mkosi_version_full" | head -n1)"
mkosi_version="$(printf '%s' "$mkosi_version_line" | awk '{print $NF}' | tr -d v)"
# Strip a trailing suffix like `~devel`/`-dev`/`rc1` so `-lt` sees a pure int.
mkosi_major="${mkosi_version%%[!0-9]*}"
if [ -n "$mkosi_major" ] && [ "$mkosi_major" -lt 22 ] 2>/dev/null; then
    echo "mkosi version $mkosi_version is too old (need >=22). pipx install --upgrade mkosi" >&2
    exit 1
fi

# Resolve `go` for the static build below.
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

# Idempotency gate. Re-runs are cheap; mkosi's cache handles incremental
# rebuilds. Bump CANARY_VERSION when a downstream schema change requires
# a full rebuild.
mkdir -p "$WORK"
if [ -n "$CALLER" ]; then
    chown "$CALLER":"$(id -gn "$CALLER")" "$WORK"
fi

if [ -f "$CANARY" ] && [ "$(cat "$CANARY")" = "$CANARY_VERSION" ] \
   && [ -f "$IMAGE_OUT" ]; then
    echo "medium-lane image already built (${IMAGE_OUT}); skipping bootstrap"
    exit 0
fi

# --- 2. build storage-verify statically
VERIFY_BIN="${WORK}/moose-storage-verify"
if [ -n "$CALLER" ]; then
    sudo -u "$CALLER" env CGO_ENABLED=0 "$GO" build -o "$VERIFY_BIN" \
        "${REPO_ROOT}/cmd/moose-storage-verify/"
else
    CGO_ENABLED=0 "$GO" build -o "$VERIFY_BIN" \
        "${REPO_ROOT}/cmd/moose-storage-verify/"
fi

# network-verify (#130): drives the real netstate + avahipublisher packages
# against the VM's NetworkManager and avahi-daemon. CGO-free on purpose —
# host-agent-real needs libpam at build time, this doesn't.
NETVERIFY_BIN="${WORK}/moose-network-verify"
if [ -n "$CALLER" ]; then
    sudo -u "$CALLER" env CGO_ENABLED=0 "$GO" build -o "$NETVERIFY_BIN" \
        "${REPO_ROOT}/cmd/moose-network-verify/"
else
    CGO_ENABLED=0 "$GO" build -o "$NETVERIFY_BIN" \
        "${REPO_ROOT}/cmd/moose-network-verify/"
fi

# host-agent-real (#164): the production privileged binary, which now launches
# the brain container during its own startup. CGO on (PAM) + CGO_CFLAGS as the
# Makefile sets — a dynamic binary linking the build host's libpam/glibc, run on
# the Debian VM (which carries libpam0g in its base). Replaces the /bin/true
# stub staged below so the real brain bootstrap runs on boot.
HOSTAGENT_BIN="${WORK}/host-agent-real"
if [ -n "$CALLER" ]; then
    sudo -u "$CALLER" env CGO_ENABLED=1 CGO_CFLAGS=-D_GNU_SOURCE "$GO" build \
        -o "$HOSTAGENT_BIN" "${REPO_ROOT}/cmd/host-agent-real/"
else
    CGO_ENABLED=1 CGO_CFLAGS=-D_GNU_SOURCE "$GO" build \
        -o "$HOSTAGENT_BIN" "${REPO_ROOT}/cmd/host-agent-real/"
fi

# --- 3. SSH keypair
if [ ! -f "$SSH_KEY" ]; then
    if [ -n "$CALLER" ]; then
        sudo -u "$CALLER" ssh-keygen -t ed25519 -N "" -C "moose-medium-test" \
            -f "$SSH_KEY"
    else
        ssh-keygen -t ed25519 -N "" -C "moose-medium-test" -f "$SSH_KEY"
    fi
fi
chmod 0600 "$SSH_KEY"

# --- 3b. LUKS recovery passphrase (slice 0023).
# mkosi auto-detects dev/test-qemu/mkosi.passphrase (must be mode 0600,
# no trailing newline) and enrolls it as LUKS keyslot 0 on the encrypted
# root. This is the test-lane stand-in for STORAGE.md's installer-
# generated recovery passphrase. Generated once, persisted, gitignored.
# run-medium-tests.sh reads it back to build the first-boot SMBIOS
# credential; the enrollment service reads the staged copy at
# /etc/moose/secrets/luks-recovery.key. Single source of truth.
if [ ! -f "$PASSPHRASE_FILE" ]; then
    # 32 hex chars, no newline — matches the cryptsetup/crypttab keyfile format.
    head -c16 /dev/urandom | od -An -tx1 | tr -d ' \n' > "$PASSPHRASE_FILE"
fi
chmod 0600 "$PASSPHRASE_FILE"
if [ -n "$CALLER" ]; then
    chown "$CALLER":"$(id -gn "$CALLER")" "$PASSPHRASE_FILE"
fi

# --- 4. stage mkosi.extra/
rm -rf "$EXTRA"
mkdir -p "$EXTRA/etc/systemd/system" \
         "$EXTRA/usr/lib/moose" \
         "$EXTRA/root/.ssh" \
         "$EXTRA/etc/ssh/sshd_config.d" \
         "$EXTRA/etc/pam.d" \
         "$EXTRA/etc/moose/secrets" \
         "$EXTRA/etc/docker" \
         "$EXTRA/etc/systemd/system/docker.service.d" \
         "$EXTRA/usr/local/bin"

# Container logging. Docker's daemon-wide log driver must be journald
# (LOGGING.md # Docker daemon uses the `journald` log driver): host-agent-real's
# per-app log tail runs `journalctl CONTAINER_NAME=<container>`
# (internal/hostagent/journalsource), and CONTAINER_NAME is set only by that
# driver. Under Docker's json-file default the match returns nothing and the
# dashboard's Logs tab waits forever, for every app. Kept byte-identical to the
# hosted lane's committed dev/cloud/mkosi.extra/etc/docker/daemon.json — both
# real profiles run the same host-agent binary against the same expectation.
cat > "$EXTRA/etc/docker/daemon.json" <<'EOF'
{
  "log-driver": "journald"
}
EOF

# Docker is the deliberate exception to journald's per-unit rate limit
# (LOGGING.md # Tuning). journald enforces the limit against _SYSTEMD_UNIT, so
# with the driver above every container on the box shares ONE bucket attributed
# to docker.service; under the 10000-per-30s default a single chatty container
# silently starves every other container's lines. Disabled here rather than
# globally, so real system services keep their cap. Byte-identical to the
# hosted lane's dev/cloud/mkosi.extra/.../10-moose-logging.conf.
cat > "$EXTRA/etc/systemd/system/docker.service.d/10-moose-logging.conf" <<'EOF'
[Service]
LogRateLimitIntervalSec=0
LogRateLimitBurst=0
EOF

# Recovery keyfile baked at the production path STORAGE.md specifies
# (/etc/moose/secrets/luks-recovery.key, mode 0400, root-owned). The
# first-boot enrollment service reads it via systemd-cryptenroll
# --unlock-key-file to authorize adding the TPM2 keyslot — exactly what
# host-agent's first-run would do in production.
cp "$PASSPHRASE_FILE" "$EXTRA/etc/moose/secrets/luks-recovery.key"
chmod 0400 "$EXTRA/etc/moose/secrets/luks-recovery.key"

# Kernel cmdline for the encrypted root (slice 0023). systemd-repart
# derives the LUKS header UUID from the pinned root partition UUID (see
# mkosi.repart/10-root.conf) as v4(HMAC-SHA256(key=partuuid,
# msg="luks-uuid")). Compute it here and bake rd.luks.uuid into the UKI
# cmdline (mkosi prepends /etc/kernel/cmdline to KernelCommandLine).
# rd.luks.options=tpm2-device=auto makes the second boot try the TPM2
# token first; first boot falls back to the cryptsetup.passphrase
# credential. The post-build check below verifies the computed UUID
# against the real image before we ever boot it.
ROOT_PARTUUID="10101010-1010-4010-8010-101010101010"
LUKS_UUID="$(python3 - "$ROOT_PARTUUID" <<'PY'
import sys, hmac, hashlib, uuid
base = uuid.UUID(sys.argv[1]).bytes
d = bytearray(hmac.new(base, b"luks-uuid", hashlib.sha256).digest()[:16])
d[6] = (d[6] & 0x0f) | 0x40   # UUID version 4
d[8] = (d[8] & 0x3f) | 0x80   # RFC 4122 variant
print(uuid.UUID(bytes=bytes(d)))
PY
)"
[ -n "$LUKS_UUID" ] || { echo "failed to compute LUKS UUID" >&2; exit 1; }
mkdir -p "$EXTRA/etc/kernel"
printf 'rd.luks.uuid=%s rd.luks.options=tpm2-device=auto root=/dev/mapper/luks-%s\n' \
    "$LUKS_UUID" "$LUKS_UUID" > "$EXTRA/etc/kernel/cmdline"

# dist/systemd units. Same shape as 0020's staging but installed
# permanently into the image rather than bind-mounted at runtime.
cp "${REPO_ROOT}/dist/systemd/moose-storage-ready.target"   "$EXTRA/etc/systemd/system/"
cp "${REPO_ROOT}/dist/systemd/moose-storage-verify.service" "$EXTRA/etc/systemd/system/"
cp "${REPO_ROOT}/dist/systemd/moose-recovery.target"        "$EXTRA/etc/systemd/system/"

# host-agent.service runs the real host-agent-real (#164/#165): it binds the
# agent socket and, after Docker is ready, seeds the brain's Docker transport
# (ingress network + socket-proxy) and launches the brain container (the postinst
# enables the unit). The brain then reconciles Caddy + moose-ui from the staged
# control-plane compose (M1b). A medium-lane drop-in points the bootstrap at the
# bundle's dev-tagged images + tarballs and orders host-agent after the first-boot
# image load so every image is present when the bootstrap runs.
cp "${REPO_ROOT}/dist/systemd/host-agent.service" "$EXTRA/etc/systemd/system/"
cp "$HOSTAGENT_BIN" "$EXTRA/usr/lib/moose/host-agent-real"
chmod 0755 "$EXTRA/usr/lib/moose/host-agent-real"

mkdir -p "$EXTRA/etc/systemd/system/host-agent.service.d"
cat > "$EXTRA/etc/systemd/system/host-agent.service.d/10-moose-brain-image.conf" <<'EOF'
[Unit]
# The control-plane images are docker-loaded by the first-boot oneshot; order
# after it so the bootstrap finds them present rather than re-loading tarballs.
After=moose-load-images.service

[Service]
Environment=MOOSE_BRAIN_IMAGE=moose-brain:dev
Environment=MOOSE_BRAIN_IMAGE_TAR=/var/lib/moose/control-plane-images/moose-brain.tar
# M1b: the socket-proxy image + tarball, the staged control-plane compose dir,
# and the moose-ui dial target the brain installs the dashboard route with. The
# proxy tarball lives in the same bundle dir the first-boot loader reads.
Environment=MOOSE_PROXY_IMAGE=tecnativa/docker-socket-proxy:v0.4.2
Environment=MOOSE_PROXY_IMAGE_TAR=/var/lib/moose/control-plane-images/docker-socket-proxy.tar
Environment=MOOSE_CONTROL_PLANE_DIR=/var/lib/moose/control-plane
Environment=MOOSE_DASHBOARD_UI_UPSTREAM=moose-ui:80
# M2 (#167): the Door-1 store (cloud #62). The guest is air-gapped (restrict=on),
# so the brain can't reach the real control plane; a whoami snapshot is staged at
# build time (see below) and the brain seeds its store from that file at boot
# (internal/catalog/remote.go # loadSnapshotFile). A real box sets no
# MOOSE_CATALOG_FILE and keeps no catalog on disk — this is a test-lane seam. Point
# the catalog URL at an inert address (nothing listening) so the background sync
# fails fast and cleanly instead of hanging on DNS. Offline-install mode stays on so
# the brain trusts the catalog-promised digest of the docker-loaded whoami image
# instead of pulling (APP_LIFECYCLE.md # image digest pinning).
Environment=MOOSE_CATALOG_URL=http://127.0.0.1:9
Environment=MOOSE_CATALOG_FILE=/var/lib/moose/catalog-seed.json
Environment=MOOSE_OFFLINE_INSTALL=1
EOF

# PAM service for host-agent-real's verify-password (#166). pamverifier dials
# the "moose" PAM service; install the canonical stack (auth+account via
# pam_unix) so the headless first-run admin authenticates against /etc/shadow.
# Without it pam_start("moose") falls back to /etc/pam.d/other (deny) and /login
# 401s. The moose group + sudo group it needs are provisioned at build time
# (mkosi.postinst.chroot + the sudo package in mkosi.conf).
cp "${REPO_ROOT}/dev/pam/moose" "$EXTRA/etc/pam.d/moose"

# storage-verify binary.
cp "$VERIFY_BIN" "$EXTRA/usr/lib/moose/moose-storage-verify"
chmod 0755 "$EXTRA/usr/lib/moose/moose-storage-verify"

# network-verify binary (#130 in-VM driver).
cp "$NETVERIFY_BIN" "$EXTRA/usr/lib/moose/moose-network-verify"
chmod 0755 "$EXTRA/usr/lib/moose/moose-network-verify"

# First-boot TPM2 enrollment (slice 0023 Stage 2): the run-once unit +
# its enrollment script. The unit gates on a marker (run-once); the
# postinst wires its .wants symlink under moose-storage-ready.target.
# This is the test-lane stand-in for host-agent's first-run enrollment.
cp "${TEST_DIR}/moose-tpm-enroll.service" "$EXTRA/etc/systemd/system/"
cp "${TEST_DIR}/first-boot-tpm-enroll.sh" "$EXTRA/usr/lib/moose/first-boot-tpm-enroll.sh"
chmod 0755 "$EXTRA/usr/lib/moose/first-boot-tpm-enroll.sh"

# The appliance's own static config (#467): the SSH LAN/mesh scoping rule, its
# loader unit, and the sshd hardening drop-in. Checked in under appliance/ at
# their in-image paths and copied in whole — see appliance/README.md for why they
# are not in this script. On a real box the moose .deb ships them; the medium lane
# is the only thing that builds an appliance image today.
cp -a "${TEST_DIR}/appliance/etc/." "$EXTRA/etc/"

# sshd: allow root key-login, no passwords (TEST IMAGE ONLY).
#
# Named to sort FIRST in sshd_config.d/. sshd takes the first value it obtains for
# a keyword and reads the drop-ins in filename order, so moose-hardening.conf's
# `PermitRootLogin no` would otherwise win and shut the harness out of its own
# image — every in-VM assertion here runs over this root connection.
#
# The appliance lane deliberately does NOT exercise the per-account SSH toggle.
# Enabling an account makes host-agent render an AllowUsers naming that account and
# no other; AllowUsers is not additive across drop-in files, so root would stop
# being considered and the harness would cut its own connection mid-run. Turning
# the last account back off would stop sshd outright and do the same. The daemon
# lifecycle and the account toggle are proved in the cloud lane instead, which
# reaches its box over the serial console and has no connection to lose
# (dev/cloud/cloud-assertions.sh, the `ssh` boot).
cat >"$EXTRA/etc/ssh/sshd_config.d/00-medium-test.conf" <<'EOF'
PermitRootLogin prohibit-password
PasswordAuthentication no
PubkeyAuthentication yes
UseDNS no
EOF

# authorized_keys for root.
cp "${SSH_KEY}.pub" "$EXTRA/root/.ssh/authorized_keys"
chmod 0700 "$EXTRA/root/.ssh"
chmod 0600 "$EXTRA/root/.ssh/authorized_keys"

# Assertion script bundled into the image at a stable path. The host
# driver could scp it in at runtime, but baking it means the image is
# self-testable via `mkosi qemu` for debugging.
cp "${TEST_DIR}/medium-assertions.sh" "$EXTRA/usr/local/bin/medium-assertions.sh"
chmod 0755 "$EXTRA/usr/local/bin/medium-assertions.sh"

# --- 4b. control-plane image bundle + Docker apt repo (M0, #163)
# The medium-lane image runs Docker (baked from Docker's apt repo at build time)
# and bakes the control-plane image tarballs, docker-loading them at first boot.
# The VM has no network, so every image the brain's compose references must be a
# local tarball (TESTING.md # Full-stack control-plane integration).

# Build + save the four control-plane images. `docker save` writes to
# .dev/control-plane/*.tar; the docker layer cache makes re-runs cheap.
echo "building + saving control-plane image bundle (docker build)..."
make -C "$REPO_ROOT" control-plane-images
CP_BUNDLE="${REPO_ROOT}/.dev/control-plane"

# Stage the tarballs into the image at /var/lib/moose/control-plane-images/.
mkdir -p "$EXTRA/var/lib/moose/control-plane-images"
cp "$CP_BUNDLE"/*.tar "$EXTRA/var/lib/moose/control-plane-images/"

# --- 4c. app image + test catalog for the full-stack app-install lane (M2, #167)
# The full-stack lane installs a catalog app (whoami) end-to-end, air-gapped. The
# app image must be a local tarball too (the guest has no registry — the same
# offline-first requirement as the control-plane images), and the brain needs a
# catalog to install from (the brain container ships none). The first-boot loader
# globs *.tar in the images dir, so dropping whoami.tar there loads it alongside
# the control-plane images.
echo "saving whoami app image for the offline full-stack lane..."
# Pull by DIGEST, not the mutable tag: the test catalog promises exactly
# sha256:43a68d10b9…, and offline mode trusts that promise without re-deriving
# the manifest digest from the loaded image — so a tag that moved upstream would
# silently bake the wrong bytes, undetected until an online run. Re-tag so the
# loaded image carries the v1.10.3 tag the override references (offline-local
# pins the tag, not the digest — a save/load image has no RepoDigest).
WHOAMI_REF="traefik/whoami@sha256:43a68d10b9dfcfc3ffbfe4dd42100dc9aeaf29b3a5636c856337a5940f1b4f1c"
docker pull "$WHOAMI_REF"
docker tag "$WHOAMI_REF" traefik/whoami:v1.10.3
docker save traefik/whoami:v1.10.3 \
    -o "$EXTRA/var/lib/moose/control-plane-images/whoami.tar"

# Stage a catalog snapshot with a whoami app (cloud #62). No catalog/ directory is
# baked into the image and the guest is air-gapped, so the brain reads this file
# once at boot to seed its store (internal/catalog/remote.go # loadSnapshotFile)
# and installs whoami from it. The brain never writes the file back — a box keeps
# no catalog on disk. mkcatalog generates it from the dev/test-qemu whoami package
# (a copy of the shipping whoami plus a documents:write folder grant, so install
# exercises a real use-case-folder bind mount + content-survives-uninstall) and
# stamps the integrity digest the brain verifies. It rides the brain's
# /var/lib/moose mount.
mkdir -p "$EXTRA/var/lib/moose"
"$GO" -C "$REPO_ROOT" run ./dev/mkcatalog \
    -pkg "${TEST_DIR}/catalog/whoami" \
    -out "$EXTRA/var/lib/moose/catalog-seed.json"

# Stage the control-plane compose + caddy.json at /var/lib/moose/control-plane/
# (M1b): the brain runs `docker compose up` here, and Caddy bind-mounts caddy.json
# from this dir. It must be the SAME host path the brain container sees (the
# daemon resolves compose bind sources as host paths — socket-proxy-compose-
# validation.md), which host-agent's brain run-spec mounts same-path via
# /var/lib/moose. The proxy is intentionally absent from this compose; host-agent
# seeds it. These staged files + the new host-agent env are baked into the image,
# so CANARY_VERSION is bumped above to force a clean rebuild.
mkdir -p "$EXTRA/var/lib/moose/control-plane"
cp "${REPO_ROOT}/dev/control-plane/compose.yml" "$EXTRA/var/lib/moose/control-plane/"
cp "${REPO_ROOT}/dev/control-plane/caddy.json"   "$EXTRA/var/lib/moose/control-plane/"

# First-boot docker-load oneshot + its script (postinst wires the .wants symlink).
cp "${TEST_DIR}/moose-load-images.service" "$EXTRA/etc/systemd/system/"
cp "${TEST_DIR}/load-control-plane-images.sh" "$EXTRA/usr/lib/moose/load-control-plane-images.sh"
chmod 0755 "$EXTRA/usr/lib/moose/load-control-plane-images.sh"

# Order docker.service after storage assembly (BOOT.md; #163). Best-effort
# ordering, not a strict gate — Docker still starts if storage partially failed.
mkdir -p "$EXTRA/etc/systemd/system/docker.service.d"
cat > "$EXTRA/etc/systemd/system/docker.service.d/10-moose-storage.conf" <<'EOF'
[Unit]
After=moose-storage-ready.target
EOF

# Docker's apt repo for the image build (mkosi.conf # PackageManagerTrees).
# The GPG key is fetched here — the build host has network; the VM never does.
# bookworm pocket: the medium-lane image is Release=bookworm.
PKGMNGR="${TEST_DIR}/mkosi.pkgmngr"
rm -rf "$PKGMNGR"
mkdir -p "$PKGMNGR/etc/apt/keyrings" "$PKGMNGR/etc/apt/sources.list.d"
curl -fsSL https://download.docker.com/linux/debian/gpg \
    -o "$PKGMNGR/etc/apt/keyrings/docker.asc"
chmod a+r "$PKGMNGR/etc/apt/keyrings/docker.asc"
cat > "$PKGMNGR/etc/apt/sources.list.d/docker.list" <<'EOF'
deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable
EOF

# --- 5. mkosi build
# Run as caller for cache ownership. mkosi auto-sudos for the privileged
# ops it needs (loopback mount, etc.).
echo "building medium-lane image via mkosi (first run takes a few minutes)..."
cd "$TEST_DIR"
# The staging steps above run as root and leave files owned root:root,
# some at restrictive modes (0700 for /root/.ssh). mkosi runs as $CALLER
# and would fail to traverse them. Re-own the entire staged tree (and
# the work dir mkosi writes into) to the caller. mkosi preserves mode
# inside the image regardless of host ownership.
if [ -n "$CALLER" ]; then
    chown -R "$CALLER":"$(id -gn "$CALLER")" "$EXTRA" "$WORK" "$PKGMNGR" "$CP_BUNDLE"
fi
# Resolve absolute path so the nested `sudo -u` invocation finds mkosi
# even though sudo strips PATH (same idiom as $GO above).
MKOSI_BIN="$(command -v mkosi || true)"
if [ -z "$MKOSI_BIN" ]; then
    echo "mkosi disappeared from PATH between preflight and build" >&2
    exit 1
fi

# mkosi's launcher shim runs `python3` from its environment; >=3.10 is
# required. Probe explicitly so we can pass MKOSI_INTERPRETER through to
# the (sudo-stripped) build invocation. Required on focal where system
# python is 3.8 and deadsnakes no longer ships 3.10 (focal EOL); the
# caller's conda/miniconda python typically satisfies the bar.
MKOSI_INTERPRETER=""
for cand in python3.13 python3.12 python3.11 python3.10; do
    if path="$(command -v "$cand" 2>/dev/null)"; then
        MKOSI_INTERPRETER="$path"; break
    fi
done
if [ -z "$MKOSI_INTERPRETER" ] && [ -n "$CALLER_HOME" ]; then
    for cand in "$CALLER_HOME/anaconda3/bin/python3" \
                "$CALLER_HOME/miniconda3/bin/python3" \
                "$CALLER_HOME/.pyenv/shims/python3"; do
        if [ -x "$cand" ] && "$cand" -c \
            'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' \
            2>/dev/null; then
            MKOSI_INTERPRETER="$cand"; break
        fi
    done
fi
if [ -z "$MKOSI_INTERPRETER" ]; then
    cat >&2 <<'EOF'
mkosi requires Python >=3.10; none found on PATH or in caller's conda/miniconda.
Install options:
  - jammy/noble: apt-get install python3.10 python3.10-venv
  - focal:       deadsnakes no longer ships 3.10 (focal EOL). Use conda,
                 pyenv, or `uv python install 3.10`.
EOF
    exit 1
fi
echo "mkosi interpreter: $MKOSI_INTERPRETER"

# Ubuntu 24.04+ heads-up: mkosi builds rootless (as $CALLER, below) by unsharing
# a user namespace and dropping capabilities inside it. With
# kernel.apparmor_restrict_unprivileged_userns=1 (the 24.04 default) that userns
# has no CAP_SETPCAP, so mkosi's PR_CAPBSET_DROP EPERMs and the build dies at
# sandbox bring-up (#189). We don't relax a system sysctl unbidden, so warn with
# the remedy here — it lands right above mkosi's own (opaque) traceback if the
# build does fail. The cloud lane (dev/cloud/bootstrap.sh), which runs mkosi as
# the invoking user, hard-probes instead; here mkosi runs as $CALLER under sudo.
userns_knob=/proc/sys/kernel/apparmor_restrict_unprivileged_userns
if [ -r "$userns_knob" ] && [ "$(cat "$userns_knob")" = "1" ]; then
    cat >&2 <<EOF
note: kernel.apparmor_restrict_unprivileged_userns=1 (Ubuntu 24.04 default).
If the mkosi build below fails with PR_CAPBSET_DROP / "Operation not permitted",
relax it with: sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
See docs/dev/running-locally.md # Ubuntu 24.04: unprivileged user namespaces.
EOF
fi

if [ -n "$CALLER" ]; then
    sudo -u "$CALLER" env "MKOSI_INTERPRETER=$MKOSI_INTERPRETER" \
        "$MKOSI_BIN" --force build
else
    MKOSI_INTERPRETER="$MKOSI_INTERPRETER" "$MKOSI_BIN" --force build
fi

# mkosi writes to OutputDirectory=../../.dev/qemu/. Confirm the
# expected artifact exists (filename pattern depends on mkosi version
# and ImageId).
if [ ! -f "$IMAGE_OUT" ]; then
    # mkosi 22+ default extension is .raw; some versions emit
    # moose-medium.raw or moose-medium.
    for cand in "${WORK}/moose-medium.raw" "${WORK}/moose-medium"; do
        if [ -f "$cand" ]; then
            ln -sf "$(basename "$cand")" "$IMAGE_OUT" 2>/dev/null || \
                cp "$cand" "$IMAGE_OUT"
            break
        fi
    done
fi
if [ ! -f "$IMAGE_OUT" ]; then
    echo "mkosi build did not produce $IMAGE_OUT — check OutputDirectory" >&2
    ls -la "$WORK" >&2 || true
    exit 1
fi

# Verify the computed LUKS UUID matches the built image (slice 0023).
# Catches a wrong derive_uuid formula here — at build, with a clear
# message — instead of as a silent unlock failure 90s into a QEMU boot.
# losetup -P exposes the partitions; blkid reads the LUKS2 header UUID
# without unlocking. p2 is the root partition (p1 = ESP).
VLOOP="$(losetup -fP --show "$IMAGE_OUT")"
udevadm settle 2>/dev/null || sleep 0.5
ACTUAL_LUKS_UUID="$(blkid -s UUID -o value "${VLOOP}p2" 2>/dev/null || true)"
losetup -d "$VLOOP" 2>/dev/null || true
if [ "$ACTUAL_LUKS_UUID" != "$LUKS_UUID" ]; then
    cat >&2 <<EOF
LUKS UUID mismatch — the rd.luks.uuid baked into the cmdline won't match
the encrypted root, so the initrd will never unlock it.
  computed (in cmdline): $LUKS_UUID
  actual (image header): $ACTUAL_LUKS_UUID
Fix the derive_uuid computation in bootstrap.sh (or bake the actual value).
EOF
    exit 1
fi
echo "verified root LUKS UUID = $LUKS_UUID"

echo -n "$CANARY_VERSION" > "$CANARY"
echo "medium-lane image ready at $IMAGE_OUT"
