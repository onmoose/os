#!/usr/bin/env bash
# Build the OS update bundle from the image that ships (#562, BUILD.md # 1b
# # The bundle), check it, and rehearse the publish job's signing.
#
#   sudo -E dev/cloud/build-bundle.sh IMAGE.raw OUTDIR
#
# IMAGE.raw is the production image dev/cloud/bootstrap.sh built. The bundle is
# one RAUC bundle in the verity format, holding slot A of that image: the same
# bytes as slot A in the published disk image, cut to the squashfs's own size
# (dev/cloud/slotbudget -extract). Its manifest takes `compatible` from the
# image's own /etc/rauc/system.conf, `version` from VERSION and `build` from the
# git commit.
#
# It is always signed here with the throwaway signer (dev/cloud/rauc.sh). A run
# that publishes the OS re-signs it with the release signer in the publish job
# (dev/release/sign-bundle.sh), so the release key never meets this build,
# which runs mkosi as root with a lot of third-party code.
#
# Checks, all against the keyring and system.conf read back OUT OF the slot
# (unsquashfs), not the copies in the repo:
#   1. the keyring the slot carries is the one this build staged
#      (MOOSE_RAUC_KEYRING: the throwaway root, or dev/release/rauc/release-ca.pem);
#   2. the bundle verifies against the throwaway root;
#   3. with the throwaway keyring, the image's own config accepts the bundle;
#      with the release keyring, it must REFUSE it: an image that ships may
#      never trust the throwaway signer;
#   4. a wrong key: a bundle-shaped check against an unrelated CA is refused;
#   5. a rehearsal of dev/release/sign-bundle.sh with a throwaway "release" CA,
#      so the publish job's script runs on every build, not only on a release.
#
# Writes to OUTDIR: moose-cloud.raucb, image-etc/{system.conf,keyring.pem} and
# bundle-info.txt, and a size table to $GITHUB_STEP_SUMMARY when set.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# shellcheck source=dev/cloud/rauc.sh
. "${REPO_ROOT}/dev/cloud/rauc.sh"

image="$(realpath "${1:?usage: build-bundle.sh IMAGE.raw OUTDIR}")"
out="${2:?usage: build-bundle.sh IMAGE.raw OUTDIR}"
mode="$(rauc_keyring_mode)"
GO="${GO:-$(command -v go || true)}"
[ -n "$GO" ] || { echo "build-bundle: go not found (set GO)" >&2; exit 1; }
CALLER="${SUDO_USER:-}"

rm -rf "$out"
mkdir -p "$out/bundle" "$out/image-etc"
out="$(realpath "$out")"

# The throwaway signer signs every bundle here. The image build already made
# it; this only makes it when the image was built elsewhere.
rauc_throwaway_ca
cp "$RAUC_THROWAWAY_DIR/root-ca.pem" "$out/throwaway-root.pem"
cp "$RAUC_THROWAWAY_DIR/signer.pem" "$out/throwaway-signer.pem"
cp "$RAUC_THROWAWAY_DIR/signer.key" "$out/throwaway-signer.key"

# The keyring this build staged, for check 1.
if [ "$mode" = "release" ]; then
    rauc_require_release_ca
    staged="$RAUC_RELEASE_CA"
else
    staged="$RAUC_THROWAWAY_DIR/root-ca.pem"
fi

# The wrong key for check 4, and a throwaway "release" CA for the rehearsal.
RAUC_CA_NAME="moose WRONG KEY (check)" RAUC_CA_ROOT_DAYS=2 RAUC_CA_NO_PASSPHRASE=1 \
    "${REPO_ROOT}/dev/release/rauc-ca.sh" root "$out/wrong" >/dev/null
RAUC_CA_NAME="moose REHEARSAL release (check)" RAUC_CA_ROOT_DAYS=2 RAUC_CA_SIGNER_DAYS=2 RAUC_CA_NO_PASSPHRASE=1 \
    "${REPO_ROOT}/dev/release/rauc-ca.sh" root "$out/rehearsal" >/dev/null
RAUC_CA_NAME="moose REHEARSAL release (check)" RAUC_CA_SIGNER_DAYS=2 \
    "${REPO_ROOT}/dev/release/rauc-ca.sh" signer "$out/rehearsal" signer >/dev/null

# The slot image.
# Built as the caller, so root never owns the caller's Go build cache, into a
# directory the caller owns.
tooldir="$(mktemp -d)"
tool="$tooldir/slotbudget"
if [ -n "$CALLER" ]; then
    chown "$CALLER" "$tooldir"
    sudo -u "$CALLER" "$GO" build -C "$REPO_ROOT" -o "$tool" ./dev/cloud/slotbudget
else
    "$GO" build -C "$REPO_ROOT" -o "$tool" ./dev/cloud/slotbudget
fi
"$tool" -extract "$out/bundle/rootfs.img" "$image" >/dev/null
rm -rf "$tooldir"

version="$(tr -d '[:space:]' < "${REPO_ROOT}/VERSION")"
if [ -n "$CALLER" ]; then
    build="$(sudo -u "$CALLER" git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
else
    build="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
fi

t0=$(date +%s)
rauc_run "$out" '
    # The image'"'"'s own RAUC config and keyring, read back out of the slot.
    unsquashfs -cat bundle/rootfs.img etc/rauc/system.conf > image-etc/system.conf
    unsquashfs -cat bundle/rootfs.img etc/rauc/keyring.pem > image-etc/keyring.pem
    compatible="$(sed -n "s/^compatible=//p" image-etc/system.conf | head -n1)"
    [ -n "$compatible" ] || { echo "the slot'"'"'s /etc/rauc/system.conf names no compatible" >&2; exit 1; }
    grep -qx "check-purpose=codesign" image-etc/system.conf || { echo "the slot'"'"'s system.conf does not ask for check-purpose=codesign" >&2; exit 1; }
    cat > bundle/manifest.raucm <<EOF
[update]
compatible=${compatible}
version='"$version"'
description=moose OS '"$version"' (hosted)
build='"$build"'

[bundle]
format=verity

[image.rootfs]
filename=rootfs.img
EOF
    conf=image-etc/system.conf

    s=$(date +%s)
    rauc --conf=$conf --keyring=throwaway-root.pem bundle --cert=throwaway-signer.pem --key=throwaway-signer.key bundle moose-cloud.raucb
    echo "rauc bundle took $(( $(date +%s) - s )) s"

    echo "check 2: the bundle verifies against the throwaway root"
    rauc --conf=$conf --keyring=throwaway-root.pem info moose-cloud.raucb > bundle-info.txt
    cat bundle-info.txt

    echo "check 3: the image'"'"'s own config and keyring ('"$mode"')"
    if [ "'"$mode"'" = "release" ]; then
        if rauc --conf=$conf info moose-cloud.raucb >/dev/null 2>&1; then
            echo "an image built for release ACCEPTS a bundle signed with the throwaway key; refusing" >&2; exit 1
        fi
        echo "ok: the release keyring refuses the throwaway-signed bundle"
    else
        rauc --conf=$conf info moose-cloud.raucb >/dev/null
        echo "ok: the image keyring accepts the bundle"
    fi

    echo "check 4: a wrong key is refused"
    if rauc --conf=$conf --keyring=wrong/root-ca.pem info moose-cloud.raucb >/dev/null 2>&1; then
        echo "a CA that never signed this bundle accepted it" >&2; exit 1
    fi
    echo "ok: the wrong key is refused"
'
echo "bundle container: $(( $(date +%s) - t0 )) s"

echo "check 1: the slot carries the keyring this build staged (${mode})"
cmp -s "$out/image-etc/keyring.pem" "$staged" || { echo "the slot's /etc/rauc/keyring.pem is not the ${mode} keyring this build staged" >&2; exit 1; }
echo "ok: $(openssl x509 -in "$out/image-etc/keyring.pem" -noout -subject)"

echo "check 5: rehearse the publish job's signing with a throwaway release CA"
mkdir -p "$out/rehearsal/image-etc"
sed 's/^path=.*/path=keyring.pem/' "$out/image-etc/system.conf" > "$out/rehearsal/image-etc/system.conf"
cp "$out/rehearsal/root-ca.pem" "$out/rehearsal/image-etc/keyring.pem"
RAUC_SIGNING_CERT="$(cat "$out/rehearsal/signer.pem")" RAUC_SIGNING_KEY="$(cat "$out/rehearsal/signer.key")" \
    MOOSE_RAUC_REHEARSAL=1 \
    "${REPO_ROOT}/dev/release/sign-bundle.sh" "$out/moose-cloud.raucb" "$out/rehearsal/image-etc" "$out/rehearsal/signed.raucb"
echo "ok: the rehearsal signed and verified the bundle"

# Only what the next jobs need stays.
rm -rf "$out/wrong" "$out/rehearsal" "$out/throwaway-signer.key" "$out/throwaway-signer.pem"
rm -f "$out/bundle/rootfs.img"

slot=$((1 << 30))
img_bytes="$(sed -n 's/.*Size: .*(\([0-9]*\) bytes).*/\1/p' "$out/bundle-info.txt" | head -n1)"
b_bytes="$(stat -c %s "$out/moose-cloud.raucb")"
pct() { awk -v a="$1" -v b="$2" 'BEGIN { printf "%.1f%%", a * 100 / b }'; }
mb() { awk -v a="$1" 'BEGIN { printf "%.1f MB", a / 1e6 }'; }
{
    echo "### OS update bundle (#562)"
    echo ""
    echo "| | Bytes | Size | Share of the 1 GiB slot |"
    echo "|---|---|---|---|"
    echo "| Slot image in the bundle | ${img_bytes} | $(mb "$img_bytes") | $(pct "$img_bytes" "$slot") |"
    echo "| Bundle (\`.raucb\`, verity, signed) | ${b_bytes} | $(mb "$b_bytes") | $(pct "$b_bytes" "$slot") |"
    echo ""
    echo "Keyring in the image: \`${mode}\`. Signed here with the throwaway key; a release re-signs it in the publish job."
} | tee -a "${GITHUB_STEP_SUMMARY:-/dev/null}"
[ -n "$CALLER" ] && chown -R "$CALLER":"$(id -gn "$CALLER")" "$out" 2>/dev/null || true
echo "bundle: $out/moose-cloud.raucb"
