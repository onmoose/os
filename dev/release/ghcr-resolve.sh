#!/usr/bin/env bash
# Resolve a published ghcr image tag to its digests (#566). Anonymous and
# read-only: the moose images are public (BUILD.md # 6), so no login is needed
# and none is used.
#
#   ghcr-resolve.sh <owner/name> <tag>
#
# Prints one line: "<manifest digest> <config digest>". The manifest digest is
# what a box pulls by (`docker pull ghcr.io/<owner/name>@<manifest digest>`).
# The config digest is the image ID Docker gives that image once it is loaded,
# which is how the boot lane checks that a box runs exactly these bytes.
#
# The tag is read once, here, and never again: the build pulls by the digest
# this prints, so a tag that moves during the build cannot change what is
# baked. When the tag names an index (a multi-platform image), the linux/amd64
# entry is taken, because that is the only platform moose builds.
#
# The digest is computed from the bytes the registry served, not taken from a
# header, and must match the header when the registry sends one. Any HTTP
# answer other than 200 fails the script: a release build must never bake
# something it could not pin.
set -euo pipefail

[ "$#" -eq 2 ] || { echo "usage: ghcr-resolve.sh <owner/name> <tag>" >&2; exit 2; }
repo="$1"; ref="$2"

accept="application/vnd.oci.image.index.v1+json,application/vnd.docker.distribution.manifest.list.v2+json,application/vnd.docker.distribution.manifest.v2+json,application/vnd.oci.image.manifest.v1+json"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

token="$(curl -fsS "https://ghcr.io/token?scope=repository:${repo}:pull" \
    | python3 -c 'import sys,json; print(json.load(sys.stdin)["token"])' 2>/dev/null)" || {
    echo "ghcr-resolve: could not get an anonymous token for ${repo}" >&2
    exit 1
}

# fetch REF: the manifest at REF into $work/body, its headers into $work/head.
# Prints the sha256 of the body as "sha256:<hex>".
fetch() {
    local r="$1" status sum want
    status="$(curl -sS -o "$work/body" -D "$work/head" -w '%{http_code}' \
        -H "Authorization: Bearer ${token}" -H "Accept: ${accept}" \
        "https://ghcr.io/v2/${repo}/manifests/${r}")" || status="000"
    if [ "$status" != 200 ]; then
        echo "ghcr-resolve: ${repo}:${r} returned HTTP ${status}" >&2
        return 1
    fi
    sum="sha256:$(sha256sum "$work/body" | cut -d' ' -f1)"
    want="$(tr -d '\r' < "$work/head" | awk -F': ' 'tolower($1) == "docker-content-digest" {print $2}' | tail -n1)"
    if [ -n "$want" ] && [ "$want" != "$sum" ]; then
        echo "ghcr-resolve: ${repo}:${r} sent ${sum}, but the registry says ${want}" >&2
        return 1
    fi
    printf '%s\n' "$sum"
}

# kind: "index <amd64 digest>", "manifest <config digest>", or fails.
kind() {
    python3 - "$work/body" <<'PY'
import json, sys
d = json.load(open(sys.argv[1]))
mt = d.get("mediaType", "")
if "manifests" in d:
    for m in d["manifests"]:
        p = m.get("platform") or {}
        if p.get("os") == "linux" and p.get("architecture") == "amd64":
            print("index", m["digest"])
            sys.exit(0)
    sys.exit("no linux/amd64 image in the index")
cfg = (d.get("config") or {}).get("digest", "")
if not cfg.startswith("sha256:") or len(cfg) != 71:
    sys.exit("manifest has no config digest (mediaType %r)" % mt)
print("manifest", cfg)
PY
}

digest="$(fetch "$ref")"
read -r k v < <(kind) || { echo "ghcr-resolve: cannot read the manifest of ${repo}:${ref}" >&2; exit 1; }
if [ "$k" = index ]; then
    digest="$(fetch "$v")"
    [ "$digest" = "$v" ] || { echo "ghcr-resolve: ${repo}@${v} came back as ${digest}" >&2; exit 1; }
    read -r k v < <(kind) || { echo "ghcr-resolve: cannot read the manifest of ${repo}@${digest}" >&2; exit 1; }
    [ "$k" = manifest ] || { echo "ghcr-resolve: ${repo}@${digest} is an index inside an index" >&2; exit 1; }
fi
case "$digest" in sha256:????????????????????????????????????????????????????????????????) ;; *)
    echo "ghcr-resolve: bad digest '${digest}'" >&2; exit 1 ;;
esac
printf '%s %s\n' "$digest" "$v"
