#!/usr/bin/env bash
# Re-sign the OS update bundle with the release signer (#562,
# docs/dev/rauc-signing.md, DECISIONS.md 2026-10-02 key custody).
#
#   sign-bundle.sh IN.raucb IMAGE_ETC OUT.raucb
#
# Run by the publish job of ci-cloud-image.yml, the only job that sees the
# release signer: RAUC_SIGNING_CERT and RAUC_SIGNING_KEY, secrets of the
# `os-release` GitHub Environment, passed in as environment variables (PEM
# text). IN.raucb is the bundle the build job made and signed with its
# throwaway key. IMAGE_ETC holds system.conf and keyring.pem as the build job
# read them back out of the image's slot.
#
# It refuses, before it signs anything:
#   - when either secret is empty (the environment or its secrets are missing);
#   - when the image keyring is not dev/release/rauc/release-ca.pem, so the
#     image that ships trusts exactly the committed release root;
#   - when the image keyring ACCEPTS the throwaway-signed IN.raucb: an image
#     that ships may never trust the throwaway key.
# Then it replaces the signature (`rauc resign`; the payload stays byte for
# byte the same) and checks OUT.raucb against the image's own config and
# keyring. A bundle that does not verify is never written to OUT.
#
# `rauc resign --no-verify`: re-checking a verity payload needs a loop device
# and exclusive access, which the container has not got (CI run 37005541036).
# The signature of IN is still checked against the throwaway root by the build
# job, and OUT is fully checked here.
#
# MOOSE_RAUC_REHEARSAL=1 (dev/cloud/build-bundle.sh only): IMAGE_ETC holds a
# throwaway "release" CA, so the committed release CA is not compared.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
# shellcheck source=dev/cloud/rauc.sh
. "${REPO_ROOT}/dev/cloud/rauc.sh"

in="$(realpath "${1:?usage: sign-bundle.sh IN.raucb IMAGE_ETC OUT.raucb}")"
etc="$(realpath "${2:?usage: sign-bundle.sh IN.raucb IMAGE_ETC OUT.raucb}")"
dst="${3:?usage: sign-bundle.sh IN.raucb IMAGE_ETC OUT.raucb}"

if [ -z "${RAUC_SIGNING_CERT:-}" ] || [ -z "${RAUC_SIGNING_KEY:-}" ]; then
    echo "::error::no release signer: RAUC_SIGNING_CERT or RAUC_SIGNING_KEY is empty. They are secrets of the os-release GitHub Environment, which only main and v* tags may use. Refusing to publish a bundle signed with the throwaway key. See ${RAUC_HOWTO}." >&2
    exit 1
fi
[ -f "$etc/system.conf" ] && [ -f "$etc/keyring.pem" ] || { echo "sign-bundle: $etc must hold system.conf and keyring.pem" >&2; exit 1; }
if [ "${MOOSE_RAUC_REHEARSAL:-}" != "1" ]; then
    rauc_require_release_ca
    cmp -s "$etc/keyring.pem" "$RAUC_RELEASE_CA" || { echo "::error::the image's /etc/rauc/keyring.pem is not dev/release/rauc/release-ca.pem; this image was not built for release" >&2; exit 1; }
fi

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
chmod 0700 "$work"
mkdir "$work/etc"
cp "$etc/system.conf" "$etc/keyring.pem" "$work/etc/"
ln "$in" "$work/in.raucb" 2>/dev/null || cp "$in" "$work/in.raucb"
printf '%s\n' "$RAUC_SIGNING_CERT" > "$work/signer.pem"
( umask 077; printf '%s\n' "$RAUC_SIGNING_KEY" > "$work/signer.key" )

rauc_run "$work" '
    if rauc --conf=etc/system.conf info in.raucb >/dev/null 2>&1; then
        echo "::error::the image keyring accepts the throwaway-signed bundle; refusing to sign" >&2; exit 1
    fi
    echo "ok: the image keyring refuses the throwaway-signed input"
    rauc --conf=etc/system.conf resign --no-verify --cert=signer.pem --key=signer.key --signing-keyring=etc/keyring.pem in.raucb out.raucb
    rauc --conf=etc/system.conf info out.raucb > out-info.txt
    echo "ok: the re-signed bundle verifies against the image keyring"
    sed -n "/^Certificate Chain:/,\$p" out-info.txt
    rm -f signer.key
'
mv "$work/out.raucb" "$dst"
echo "signed bundle: $dst"
