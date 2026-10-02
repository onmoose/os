# shellcheck shell=bash
# Shared RAUC helpers for the OS update bundle (#562, BUILD.md # 1b # The
# bundle, docs/dev/rauc-signing.md). Sourced, not run, by
# dev/cloud/stage-control-plane.sh (the keyring both images bake),
# dev/cloud/build-bundle.sh (the build job) and dev/release/sign-bundle.sh (the
# publish job).
#
# Expects REPO_ROOT set by the sourcing script.
#
# Which keyring an image bakes, as /etc/rauc/keyring.pem, is set by
# MOOSE_RAUC_KEYRING:
#   throwaway (the default) the root of a throwaway CA, made once per checkout
#             under .dev/rauc/throwaway/ with dev/release/rauc-ca.sh. A box
#             from such an image trusts only bundles this checkout signed. Every
#             run that publishes no OS image uses it, and so does a local build.
#   release   dev/release/rauc/release-ca.pem, the public cert of the offline
#             root CA. ci-cloud-image.yml sets it on every run that publishes
#             the OS line. It fails when the file is not committed yet.

RAUC_RELEASE_CA="${REPO_ROOT}/dev/release/rauc/release-ca.pem"
RAUC_THROWAWAY_DIR="${REPO_ROOT}/.dev/rauc/throwaway"
RAUC_HOWTO="docs/dev/rauc-signing.md"

# rauc_keyring_mode prints throwaway or release, or fails on any other value.
rauc_keyring_mode() {
    case "${MOOSE_RAUC_KEYRING:-throwaway}" in
        throwaway|release) printf '%s\n' "${MOOSE_RAUC_KEYRING:-throwaway}" ;;
        *) echo "MOOSE_RAUC_KEYRING must be throwaway or release, got '${MOOSE_RAUC_KEYRING}'" >&2; return 1 ;;
    esac
}

# rauc_require_release_ca fails, naming the how-to, when the release root CA is
# not committed or is not a CA certificate.
rauc_require_release_ca() {
    if [ ! -f "$RAUC_RELEASE_CA" ]; then
        echo "no release root CA: dev/release/rauc/release-ca.pem is not committed yet, so this run cannot build an OS image to publish. The maintainer makes the root offline and commits its public cert; see ${RAUC_HOWTO}." >&2
        return 1
    fi
    if ! openssl x509 -in "$RAUC_RELEASE_CA" -noout -ext basicConstraints 2>/dev/null | grep -q 'CA:TRUE'; then
        echo "dev/release/rauc/release-ca.pem is not a CA certificate; see ${RAUC_HOWTO}." >&2
        return 1
    fi
}

# rauc_throwaway_ca makes the throwaway CA and its signer once, under
# .dev/rauc/throwaway/, and reuses them after that, so the lean image and the
# boot-proof image of one build bake the same keyring. Short lifetimes: a
# throwaway is never meant to sign anything a week later.
rauc_throwaway_ca() {
    local d="$RAUC_THROWAWAY_DIR"
    if [ -f "$d/root-ca.pem" ] && [ -f "$d/signer.pem" ] && [ -f "$d/signer.key" ]; then
        return 0
    fi
    rm -rf "$d"
    mkdir -p "$(dirname "$d")"
    RAUC_CA_NAME="moose THROWAWAY (not for release)" RAUC_CA_ROOT_DAYS=30 RAUC_CA_SIGNER_DAYS=30 RAUC_CA_NO_PASSPHRASE=1 \
        "${REPO_ROOT}/dev/release/rauc-ca.sh" root "$d" >/dev/null
    RAUC_CA_NAME="moose THROWAWAY (not for release)" RAUC_CA_ROOT_DAYS=30 RAUC_CA_SIGNER_DAYS=30 \
        "${REPO_ROOT}/dev/release/rauc-ca.sh" signer "$d" signer >/dev/null
    if [ -n "${CALLER:-}" ]; then chown -R "$CALLER":"$(id -gn "$CALLER")" "${REPO_ROOT}/.dev/rauc" 2>/dev/null || true; fi
}

# rauc_stage_keyring WIRING: write the keyring the image bakes to
# WIRING/etc/rauc/keyring.pem (/etc/rauc/system.conf names it).
rauc_stage_keyring() {
    local wiring="$1" mode
    mode="$(rauc_keyring_mode)" || return 1
    mkdir -p "$wiring/etc/rauc"
    if [ "$mode" = "release" ]; then
        rauc_require_release_ca || return 1
        cp "$RAUC_RELEASE_CA" "$wiring/etc/rauc/keyring.pem"
    else
        rauc_throwaway_ca || return 1
        cp "$RAUC_THROWAWAY_DIR/root-ca.pem" "$wiring/etc/rauc/keyring.pem"
    fi
    chmod 0644 "$wiring/etc/rauc/keyring.pem"
    echo "rauc keyring: ${mode} ($(openssl x509 -in "$wiring/etc/rauc/keyring.pem" -noout -subject))"
}

# rauc_run WORKDIR SCRIPT: run SCRIPT with bash in a throwaway container that
# holds the same RAUC the image ships: the pinned debian:trixie-slim
# (BRAIN_RUNTIME_IMAGE in dev/control-plane/images.lock), with rauc at the
# version dev/os-lock/cloud-packages.lock names, from snapshot.debian.org at
# the locked timestamp, and squashfs-tools. WORKDIR is mounted at /w.
#
# The snapshot is fetched over plain http: the slim image has no CA store, and
# apt checks every index against Debian's archive key, which the image does
# carry, so the transport needs no TLS for integrity.
rauc_run() {
    local work="$1" script="$2" img ts rv
    img="$(awk -F= '$1=="BRAIN_RUNTIME_IMAGE"{print $2}' "${REPO_ROOT}/dev/control-plane/images.lock")"
    ts="$(tr -d '[:space:]' < "${REPO_ROOT}/dev/os-lock/debian-snapshot")"
    rv="$(awk '$1=="rauc"{print $2}' "${REPO_ROOT}/dev/os-lock/cloud-packages.lock")"
    [ -n "$img" ] && [ -n "$ts" ] && [ -n "$rv" ] || { echo "rauc_run: could not read the base image, snapshot or rauc version from the locks" >&2; return 1; }
    docker run --rm -v "$work:/w" -w /w -e TS="$ts" -e RV="$rv" "$img" bash -euo pipefail -c '
        rm -f /etc/apt/sources.list.d/*
        cat > /etc/apt/sources.list <<EOF
deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${TS} trixie main
deb [check-valid-until=no] http://snapshot.debian.org/archive/debian/${TS} trixie-updates main
deb [check-valid-until=no] http://snapshot.debian.org/archive/debian-security/${TS} trixie-security main
EOF
        apt-get -o Acquire::Retries=5 update -qq
        DEBIAN_FRONTEND=noninteractive apt-get -o Acquire::Retries=5 install -y -qq --no-install-recommends "rauc=${RV}" squashfs-tools >/dev/null
        echo "in the container: $(rauc --version)"
        '"$script"
}
