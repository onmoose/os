#!/usr/bin/env bash
# Does a ghcr image tag exist? Anonymous, read-only: the moose images are public
# (BUILD.md # 6), so no login is needed and none is used.
#
#   ghcr-tag-exists.sh <owner/name> <tag>
#
# Exit 0: the tag exists. Exit 1: it does not (a definite 404). Exit 2: any
# other answer. The caller must treat 2 as "unknown", never as "missing", or a
# registry outage would read as permission to publish over a released tag.
set -euo pipefail

repo="${1:?usage: ghcr-tag-exists.sh <owner/name> <tag>}"
tag="${2:?usage: ghcr-tag-exists.sh <owner/name> <tag>}"

token="$(curl -fsS "https://ghcr.io/token?scope=repository:${repo}:pull" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])')" || {
    echo "ghcr-tag-exists: could not get an anonymous token for ${repo}" >&2
    exit 2
}

accept="application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json"
status="$(curl -sS -o /dev/null -w '%{http_code}' -I \
    -H "Authorization: Bearer ${token}" -H "Accept: ${accept}" \
    "https://ghcr.io/v2/${repo}/manifests/${tag}")" || status="000"

case "$status" in
    200) exit 0 ;;
    404) exit 1 ;;
    *) echo "ghcr-tag-exists: ${repo}:${tag} lookup returned HTTP ${status}" >&2; exit 2 ;;
esac
