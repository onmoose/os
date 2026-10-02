#!/usr/bin/env bash
# Build the test-only OS bundle the os-update and os-revert boots install (#563).
#
#   sudo -E dev/cloud/test/build-os-test-bundle.sh BOOT-PROOF.raw OUTDIR
#
# The bundle holds slot A of the boot-proof image, repacked with one change: a
# host-agent stamped one patch release above VERSION. So on slot B the box
# reports that version, and the update loop sees it as current. Nothing in it
# ever ships: it is signed with this checkout's throwaway key (dev/cloud/rauc.sh),
# which only a boot-proof image of the same build trusts.
#
# Kept cheap on purpose (the maintainer's call for #563): the slot is repacked
# with gzip at level 1, not xz, which GRUB also reads; the baked image tarballs
# under /var/lib/moose are left out, because a slot's /var/lib/moose is copied
# to the state partition only at a box's first boot and slot B never has one.
#
# Writes OUTDIR/os-test.raucb, OUTDIR/os-test.sha256 (the digest) and
# OUTDIR/os-test.version (the version in the bundle and in its host-agent).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# shellcheck source=dev/cloud/rauc.sh
. "${REPO_ROOT}/dev/cloud/rauc.sh"

image="$(realpath "${1:?usage: build-os-test-bundle.sh BOOT-PROOF.raw OUTDIR}")"
out="${2:?usage: build-os-test-bundle.sh BOOT-PROOF.raw OUTDIR}"
GO="${GO:-$(command -v go || true)}"
[ -n "$GO" ] || { echo "build-os-test-bundle: go not found (set GO)" >&2; exit 1; }
CALLER="${SUDO_USER:-}"
t0=$(date +%s)

rm -rf "$out"
mkdir -p "$out/work"
out="$(realpath "$out")"
[ -n "$CALLER" ] && chown -R "$CALLER" "$out"

base="$(tr -d '[:space:]' < "${REPO_ROOT}/VERSION")"
IFS=. read -r maj min pat <<<"$base"
next="${maj}.${min}.$((pat + 1))"

as_caller() { if [ -n "$CALLER" ]; then sudo -u "$CALLER" "$@"; else "$@"; fi; }

# The host-agent for the new slot, built the way stage-control-plane.sh builds
# the baked one, with the next patch number.
commit="$(as_caller git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
as_caller env CGO_ENABLED=1 CGO_CFLAGS=-D_GNU_SOURCE "$GO" build -C "$REPO_ROOT" -tags hosted \
    -ldflags "-X github.com/onmoose/os/internal/version.Version=${next} -X github.com/onmoose/os/internal/version.Commit=${commit}" \
    -o "$out/work/host-agent-real" ./cmd/host-agent-real/
as_caller "$GO" build -C "$REPO_ROOT" -o "$out/work/slotbudget" ./dev/cloud/slotbudget
"$out/work/slotbudget" -extract "$out/work/slot.img" "$image" >/dev/null
rm -f "$out/work/slotbudget"

rauc_throwaway_ca
cp "$RAUC_THROWAWAY_DIR/root-ca.pem" "$RAUC_THROWAWAY_DIR/signer.pem" "$RAUC_THROWAWAY_DIR/signer.key" "$out/work/"

rauc_run "$out/work" '
    s=$(date +%s)
    unsquashfs -q -n -d root slot.img
    rm -f slot.img
    echo "unsquashfs: $(( $(date +%s) - s )) s"
    install -m 0755 host-agent-real root/usr/lib/moose/host-agent-real
    # Left empty, not removed: state-setup binds onto these paths.
    find root/var/lib/moose -mindepth 1 -delete
    s=$(date +%s)
    mkdir -p bundle
    mksquashfs root bundle/rootfs.img -comp gzip -Xcompression-level 1 -noappend -quiet
    echo "mksquashfs (gzip level 1): $(( $(date +%s) - s )) s, $(stat -c %s bundle/rootfs.img) bytes"
    compatible="$(sed -n "s/^compatible=//p" root/etc/rauc/system.conf | head -n1)"
    cp root/etc/rauc/system.conf system.conf
    rm -rf root
    cat > bundle/manifest.raucm <<EOF
[update]
compatible=${compatible}
version='"$next"'
description=moose OS '"$next"' (boot-proof test bundle, never shipped)
build='"$commit"'

[bundle]
format=verity

[image.rootfs]
filename=rootfs.img
EOF
    rauc --conf=system.conf --keyring=root-ca.pem bundle --cert=signer.pem --key=signer.key bundle os-test.raucb
    rauc --conf=system.conf --keyring=root-ca.pem info os-test.raucb >/dev/null
'
mv "$out/work/os-test.raucb" "$out/os-test.raucb"
rm -rf "$out/work"
sha256sum "$out/os-test.raucb" | cut -d' ' -f1 > "$out/os-test.sha256"
echo "$next" > "$out/os-test.version"
[ -n "$CALLER" ] && chown -R "$CALLER":"$(id -gn "$CALLER")" "$out" 2>/dev/null || true
bytes="$(stat -c %s "$out/os-test.raucb")"
echo "os test bundle: ${next}, ${bytes} bytes, sha256 $(cat "$out/os-test.sha256"), built in $(( $(date +%s) - t0 )) s"
{
    echo "### OS test bundle (#563, boot-proof only)"
    echo ""
    echo "Version ${next}, $(awk -v a="$bytes" 'BEGIN { printf "%.1f MB", a / 1e6 }'), built in $(( $(date +%s) - t0 )) s. Slot repacked with gzip level 1; never shipped."
} >> "${GITHUB_STEP_SUMMARY:-/dev/null}"
