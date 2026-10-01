#!/usr/bin/env bash
# Point one ghcr tag at the manifest another tag already names, byte for byte,
# so both tags carry the same digest. Nothing is rebuilt or re-pushed: the
# manifest is read and written back under the new tag.
#
#   GHCR_USER=<actor> GHCR_TOKEN=<token> ghcr-retag.sh <owner/name> <src-tag> <dst-tag>
#
# ci-cloud-image.yml uses it to repair `latest` when a run that pushed `vX.Y.Z`
# died before `latest`. It prints the digest on stdout and checks it: after the
# write, <dst-tag> must answer with the same digest as <src-tag>, or it exits 1.
set -euo pipefail

repo="${1:?usage: ghcr-retag.sh <owner/name> <src-tag> <dst-tag>}"
src="${2:?usage: ghcr-retag.sh <owner/name> <src-tag> <dst-tag>}"
dst="${3:?usage: ghcr-retag.sh <owner/name> <src-tag> <dst-tag>}"
: "${GHCR_USER:?GHCR_USER is required}" "${GHCR_TOKEN:?GHCR_TOKEN is required}"

accept="application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json"
api="https://ghcr.io/v2/${repo}/manifests"

token="$(curl -fsS -u "${GHCR_USER}:${GHCR_TOKEN}" "https://ghcr.io/token?scope=repository:${repo}:pull,push" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])' 2>/dev/null)" || {
    echo "ghcr-retag: could not get a push token for ${repo}" >&2
    exit 1
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsS -H "Authorization: Bearer ${token}" -H "Accept: ${accept}" \
    -D "$tmp/headers" -o "$tmp/manifest" "${api}/${src}" || {
    echo "ghcr-retag: could not read ${repo}:${src}" >&2
    exit 1
}
ctype="$(grep -i '^content-type:' "$tmp/headers" | tail -n1 | cut -d: -f2- | tr -d ' \r')"
[ -n "$ctype" ] || { echo "ghcr-retag: ${repo}:${src} answered with no content type" >&2; exit 1; }
digest="sha256:$(sha256sum "$tmp/manifest" | cut -d' ' -f1)"

curl -fsS -X PUT -H "Authorization: Bearer ${token}" -H "Content-Type: ${ctype}" \
    --data-binary "@$tmp/manifest" -o /dev/null "${api}/${dst}" || {
    echo "ghcr-retag: could not write ${repo}:${dst}" >&2
    exit 1
}

# Read it back: the registry's own digest for <dst-tag> must be the one we wrote.
got="$(curl -fsS -I -H "Authorization: Bearer ${token}" -H "Accept: ${accept}" "${api}/${dst}" \
    | grep -i '^docker-content-digest:' | tail -n1 | cut -d: -f2- | tr -d ' \r')" || got=""
if [ "$got" != "$digest" ]; then
    echo "ghcr-retag: ${repo}:${dst} answers '${got:-nothing}', want ${digest}" >&2
    exit 1
fi
echo "$digest"
