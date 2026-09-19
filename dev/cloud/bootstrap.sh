#!/usr/bin/env bash
# Build the hosted cloud-VM image (C1b #203; first-boot wiring #242) and assert it
# is genuinely lean. This is the image `deploy/hetzner-image/build-and-upload.sh`
# (cloud repo) snapshots and provisioning uploads as MOOSE_HETZNER_IMAGE.
#
# C1b shipped this as a lean Debian + docker host with NO moose wiring — it booted
# to an empty, network-less docker host on a real cloud (#242). This script now
# bakes the first-boot RUNTIME wiring (the same wiring the boot-proof test lane
# validates) so a provisioned box self-bootstraps: networkd brings the NIC up via
# DHCP, the baked control-plane images load, host-agent launches the brain, and the
# seed materializer + /setup gate run. The wiring adds no apt packages, so the lean
# check still passes (it asserts the appliance package cuts against the manifest).
#
# Sequence:
#   1. Preflight: root, mkosi (>=22), curl, python3, docker, go, libpam headers.
#   2. Stage the first-boot wiring into mkosi.extra.wiring/ via the shared
#      dev/cloud/stage-control-plane.sh (slim host-agent + units + control-plane
#      bundle + seed materializer; the SAME staging the test lane runs).
#   3. Stage Docker's apt repo (trixie pocket) into mkosi.pkgmngr/ so docker-ce
#      resolves at build time (build-host network only; the VM never apt-installs).
#   4. `mkosi build` → a raw GPT disk image under .dev/cloud/.
#   5. Assert the built package manifest matches the checked-in expected set
#      (expected-packages.txt) exactly — any off-list package (bloat, e.g.
#      recommends drift) or unexpectedly-dropped package fails the build, not
#      just a hardcoded appliance cut list (#238). Then source-sanity-check the
#      committed ExtraTrees marker reads `hosted`.
#
# Needs root: it builds the control-plane image bundle (docker) and chowns build
# artifacts back to the caller; mkosi itself runs as the caller (it auto-escalates
# for the disk ops). `make build-cloud-image` runs it under `sudo -E`.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
CLOUD_DIR="${REPO_ROOT}/dev/cloud"
WORK="${REPO_ROOT}/.dev/cloud"
WIRING="${CLOUD_DIR}/mkosi.extra.wiring"
PKGMNGR="${CLOUD_DIR}/mkosi.pkgmngr"
CP_BUNDLE="${REPO_ROOT}/.dev/control-plane"

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    echo "must run as root (control-plane image build + mkosi disk ops). Use: sudo make build-cloud-image" >&2
    exit 1
fi

# Resolve the invoking non-root user for build-artifact ownership + the unprivileged
# go/mkosi sub-builds (same pattern as the test lane).
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

# --- 1. preflight
missing=()
for tool in mkosi curl python3 docker; do
    command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
done
# host-agent-real is a CGO binary (PAM verify is kept in hosted); needs the headers.
[ -f /usr/include/security/pam_appl.h ] || missing+=("libpam0g-dev (PAM headers for host-agent-real)")
if [ ${#missing[@]} -gt 0 ]; then
    cat >&2 <<EOF
cloud-image build preflight: missing tooling
  ${missing[*]}

Install (Ubuntu/Debian):
  sudo apt-get install -y curl python3 docker.io libpam0g-dev
  sudo apt-get install -y pipx && pipx install mkosi   # need v22+; ensure ~/.local/bin on PATH

After installing, re-run \`sudo make build-cloud-image\`.
EOF
    exit 1
fi

# mkosi version sanity (need >=22 for the config schema we use). Capture the whole
# output first so `head` closing the pipe early can't SIGPIPE mkosi 27 (pipefail
# would then abort us with rc=141) — same guard as the test lane.
mkosi_version_full="$(mkosi --version 2>&1 || true)"
mkosi_version="$(printf '%s\n' "$mkosi_version_full" | head -n1 | awk '{print $NF}' | tr -d v)"
mkosi_major="${mkosi_version%%[!0-9]*}"
if [ -n "$mkosi_major" ] && [ "$mkosi_major" -lt 22 ] 2>/dev/null; then
    echo "mkosi version $mkosi_version is too old (need >=22). pipx install --upgrade mkosi" >&2
    exit 1
fi

# Resolve go for the slim-agent build (sudo strips it from PATH).
if [ -z "${GO:-}" ]; then GO="$(command -v go 2>/dev/null || true)"; fi
if [ -z "${GO:-}" ] && [ -n "$CALLER_HOME" ]; then
    for cand in "${CALLER_HOME}/.local/go/bin/go" "/usr/local/go/bin/go"; do
        [ -x "$cand" ] && { GO="$cand"; break; }
    done
fi
if ! { [ -n "${GO:-}" ] && [ -x "$GO" ]; }; then
    cat >&2 <<EOF
cloud-image build preflight: go binary not found (GO=${GO:-}).
  Install (Ubuntu/Debian): sudo apt-get install -y golang-go
  Or download from https://go.dev/dl/ and ensure the binary is on PATH.
  Set GO=/path/to/go to override discovery.

After installing, re-run \`sudo make build-cloud-image\`.
EOF
    exit 1
fi

# --- 1b. unprivileged user-namespace preflight (Ubuntu 24.04+)
# mkosi builds rootless: it unshares a user namespace and drops capabilities
# inside it. Ubuntu 24.04 ships kernel.apparmor_restrict_unprivileged_userns=1,
# which hands an unconfined process a userns with no CAP_SETPCAP, so mkosi's
# PR_CAPBSET_DROP EPERMs and the build dies at sandbox bring-up before doing any
# work (#189). Probe mkosi's real sandbox from a config-less dir (so a healthy
# host doesn't provision a tools tree here) and, on failure, point at the fix
# rather than leaving the caller with mkosi's opaque ctypes traceback.
# Probe AS THE USER THAT ACTUALLY RUNS MKOSI ($CALLER, see the build step below) —
# not as root. Two reasons the root probe false-negatives even though the real
# `sudo -u "$CALLER" mkosi build` succeeds: (1) mkosi's sandbox is rootless and
# behaves differently under root; (2) `sudo -u` resets PATH to the system python
# 3.8, but mkosi needs >=3.10 — so the probe must pass MKOSI_INTERPRETER just like
# the build step does.
probe_py=""
for cand in python3.13 python3.12 python3.11 python3.10 \
            "$CALLER_HOME/anaconda3/bin/python3" "$CALLER_HOME/miniconda3/bin/python3"; do
    p="$(command -v "$cand" 2>/dev/null || true)"
    [ -n "$p" ] && [ -x "$p" ] && "$p" -c 'import sys; sys.exit(0 if sys.version_info >= (3,10) else 1)' 2>/dev/null \
        && { probe_py="$p"; break; }
done
probe_dir="$(mktemp -d)"
[ -n "$CALLER" ] && chown "$CALLER" "$probe_dir" 2>/dev/null || true
userns_ok=1
if [ -n "$CALLER" ]; then
    ( cd "$probe_dir" && sudo -u "$CALLER" env "MKOSI_INTERPRETER=$probe_py" "$(command -v mkosi)" sandbox -- true ) >/dev/null 2>&1 || userns_ok=0
else
    ( cd "$probe_dir" && MKOSI_INTERPRETER="$probe_py" mkosi sandbox -- true ) >/dev/null 2>&1 || userns_ok=0
fi
rm -rf "$probe_dir"
if [ "$userns_ok" -ne 1 ]; then
    knob=/proc/sys/kernel/apparmor_restrict_unprivileged_userns
    knob_val=$(cat "$knob" 2>/dev/null || echo "absent")
    if [ "$knob_val" = "1" ]; then
        cat >&2 <<EOF
mkosi's build sandbox can't start: creating an unprivileged user namespace
failed (PR_CAPBSET_DROP EPERM). kernel.apparmor_restrict_unprivileged_userns=1
(Ubuntu 24.04 default) — relax it:
  - this shell / CI:  sudo sysctl -w kernel.apparmor_restrict_unprivileged_userns=0
  - persist it:       echo 'kernel.apparmor_restrict_unprivileged_userns=0' | \\
                      sudo tee /etc/sysctl.d/99-mkosi-userns.conf && sudo sysctl --system
See docs/dev/running-locally.md # Ubuntu 24.04: unprivileged user namespaces.
EOF
    else
        cat >&2 <<EOF
mkosi's build sandbox can't start (sandbox probe failed; knob=${knob_val} — not the AppArmor restriction).
Check that uidmap is installed and ${CALLER:-root} appears in /etc/subuid and /etc/subgid:
  grep "^${CALLER:-root}:" /etc/subuid /etc/subgid
EOF
    fi
    exit 1
fi

mkdir -p "$WORK"
[ -n "$CALLER" ] && chown "$CALLER":"$(id -gn "$CALLER")" "$WORK"

# --- 2. stage the first-boot wiring (shared with the test lane).
# shellcheck source=dev/cloud/stage-control-plane.sh
. "${CLOUD_DIR}/stage-control-plane.sh"
stage_control_plane

# --- 3. Docker apt repo for the build's package manager.# The build host has network; the VM never apt-installs Docker (baked). trixie
# pocket — the cloud image is Release=trixie (the test lane uses bookworm).
rm -rf "$PKGMNGR"
mkdir -p "$PKGMNGR/etc/apt/keyrings" "$PKGMNGR/etc/apt/sources.list.d"
curl -fsSL https://download.docker.com/linux/debian/gpg \
    -o "$PKGMNGR/etc/apt/keyrings/docker.asc"
chmod a+r "$PKGMNGR/etc/apt/keyrings/docker.asc"
cat > "$PKGMNGR/etc/apt/sources.list.d/docker.list" <<'EOF'
deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian trixie stable
EOF

# --- 4. build. mkosi runs as the caller (it auto-sudos for privileged disk ops);
# re-own the staged trees first. NOT $CP_BUNDLE (shared with the medium lane; mkosi
# reads the tarball copies under $WIRING, never the bundle itself).
echo "building hosted cloud image via mkosi (first run takes a few minutes)..."
if [ -n "$CALLER" ]; then
    chown -R "$CALLER":"$(id -gn "$CALLER")" "$WIRING" "$WORK" "$PKGMNGR"
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
        "$MKOSI_BIN" --directory "$CLOUD_DIR" --force build
else
    MKOSI_INTERPRETER="$MKOSI_INTERPRETER" "$MKOSI_BIN" --directory "$CLOUD_DIR" --force build
fi

# --- 5. assert lean
MANIFEST="$(ls -1 "$WORK"/*.manifest 2>/dev/null | head -n1 || true)"
if [ -z "$MANIFEST" ]; then
    echo "no package manifest under $WORK (expected ManifestFormat=json output)" >&2
    ls -la "$WORK" >&2 || true
    exit 1
fi

# Lean guard (#238): assert the built manifest's package set matches the
# checked-in expected set (expected-packages.txt) exactly. This supersedes the
# old hardcoded appliance cut list — any off-list package (recommends drift like
# #237, or any other bloat) fails, not only a named handful, and an
# unexpectedly-dropped package fails too. The expected set is a lockfile-style
# snapshot of the lean image (ENVIRONMENT.md # How the profile is realized);
# names only, so version bumps don't churn it. Regenerate it by pasting the
# reported sets when a package change is intended (see expected-packages.txt).
EXPECTED="${CLOUD_DIR}/expected-packages.txt"
python3 - "$MANIFEST" "$EXPECTED" <<'PY'
import json, re, sys

manifest_path, expected_path = sys.argv[1], sys.argv[2]

# The concrete kernel-image package carries the full ABI version IN its name
# (e.g. linux-image-6.12.94+deb13-amd64), so its name changes on every kernel
# bump — that is not bloat. It is pulled by the stable meta-package
# linux-image-amd64, which stays in the expected set and still flags a real
# kernel removal. Drop the versioned one from both sides so an ABI bump is not a
# false "unexpected package". This is the one package whose name is not version-
# stable; everything else is compared by name only.
versioned_kernel = re.compile(r"^linux-image-\d")

with open(manifest_path) as f:
    data = json.load(f)
installed = {p["name"] for p in data.get("packages", [])
             if p.get("name") and not versioned_kernel.match(p["name"])}

expected = set()
with open(expected_path) as f:
    for line in f:
        name = line.split("#", 1)[0].strip()
        if name and not versioned_kernel.match(name):
            expected.add(name)

unexpected = sorted(installed - expected)
missing = sorted(expected - installed)

print(f"lean check: {len(installed)} packages compared, {len(expected)} expected")

if unexpected or missing:
    print("LEAN CHECK FAILED — built image does not match the expected package "
          "set (dev/cloud/expected-packages.txt).", file=sys.stderr)
    if unexpected:
        print(f"\nUNEXPECTED — in the image but not expected ({len(unexpected)}); "
              "if intended, add these to expected-packages.txt:", file=sys.stderr)
        for name in unexpected:
            print(name, file=sys.stderr)
    if missing:
        print(f"\nMISSING — expected but not in the image ({len(missing)}); "
              "if intended, drop these from expected-packages.txt:", file=sys.stderr)
        for name in missing:
            print(name, file=sys.stderr)
    sys.exit(1)

print(f"lean check passed — manifest matches expected-packages.txt exactly "
      f"({len(installed)} packages)")
PY

# Source-sanity check: verify the committed ExtraTrees source file reads `hosted`
# so a stale or accidentally blanked file fails fast before the next build.
MARKER="${CLOUD_DIR}/mkosi.extra/etc/moose/profile"
if [ "$(tr -d '[:space:]' < "$MARKER")" != "hosted" ]; then
    echo "source-sanity check failed: $MARKER does not read 'hosted'" >&2
    exit 1
fi
echo "source-sanity check passed: ExtraTrees source $MARKER reads 'hosted'"

IMAGE_OUT="$(ls -1 "$WORK"/*.raw 2>/dev/null | head -n1 || true)"
[ -n "$CALLER" ] && [ -n "$IMAGE_OUT" ] && \
    chown "$CALLER":"$(id -gn "$CALLER")" "$IMAGE_OUT" 2>/dev/null || true
echo "hosted cloud image built: ${IMAGE_OUT:-<see $WORK>}"
