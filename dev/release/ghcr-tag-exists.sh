#!/usr/bin/env bash
# Does a ghcr image tag exist, in any of the given repositories? Anonymous,
# read-only: the moose images are public (BUILD.md # 6), so no login is needed
# and none is used.
#
#   ghcr-tag-exists.sh <owner/name> [<owner/name>...] <tag>
#
# The tag comes last, so dev/release/decide.sh can append it to a configured
# command. release.yml passes the brain and the UI in one call, because the two
# are always pushed together and either one already published is a reason to
# stop.
#
# Exit 0: the tag exists in at least one repository. Exit 1: it is missing from
# all of them (a definite 404 each). Exit 2: no "exists", and at least one
# repository gave some other answer. The caller must treat 2 as "unknown",
# never as "missing", or a registry outage would read as permission to publish
# over a released tag.
set -euo pipefail

[ "$#" -ge 2 ] || { echo "usage: ghcr-tag-exists.sh <owner/name> [<owner/name>...] <tag>" >&2; exit 2; }
tag="${!#}"
repos=("${@:1:$#-1}")

accept="application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json"

# one REPO -> 0 exists, 1 missing, 2 unknown
one() {
    local repo="$1" token status
    token="$(curl -fsS "https://ghcr.io/token?scope=repository:${repo}:pull" \
        | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])' 2>/dev/null)" || {
        echo "ghcr-tag-exists: could not get an anonymous token for ${repo}" >&2
        return 2
    }
    status="$(curl -sS -o /dev/null -w '%{http_code}' -I \
        -H "Authorization: Bearer ${token}" -H "Accept: ${accept}" \
        "https://ghcr.io/v2/${repo}/manifests/${tag}")" || status="000"
    case "$status" in
        200) echo "ghcr-tag-exists: ${repo}:${tag} is published" >&2; return 0 ;;
        404) return 1 ;;
        *) echo "ghcr-tag-exists: ${repo}:${tag} lookup returned HTTP ${status}" >&2; return 2 ;;
    esac
}

result=1
for repo in "${repos[@]}"; do
    rc=0
    one "$repo" || rc=$?
    case "$rc" in
        0) exit 0 ;;
        1) ;;
        *) result=2 ;;
    esac
done
exit "$result"
