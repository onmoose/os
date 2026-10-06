#!/usr/bin/env bash
# Is <version> the newest release on a line, by semver, among the line's tags?
#
#   is-newest.sh <tag-prefix> <X.Y.Z>
#
# ci-cloud-image.yml asks this before it moves the control-plane images'
# `latest` tag, so re-running or dispatching an older control-plane release can
# never point `latest` back at older images:
#
#   is-newest.sh control-plane-v 0.16.0
#
# Exit 0: no tag on the line is newer (equal is fine: that is this release's own
# tag). Exit 1: a newer tag exists. Exit 2: the tag list could not be read, or
# <version> is not X.Y.Z. The caller treats 2 as "do not touch latest", never as
# "newest". Tags of another shape on the line (a suffix, a typo) are ignored, so
# they can neither block nor move `latest`.
#
# Tags are read from RELEASE_REMOTE (default: origin). The repo is public, so
# this needs no credentials.
set -euo pipefail

prefix="${1:?usage: is-newest.sh <tag-prefix> <X.Y.Z>}"
ver="${2:?usage: is-newest.sh <tag-prefix> <X.Y.Z>}"
remote="${RELEASE_REMOTE:-origin}"

[[ "$ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "is-newest: '$ver' is not X.Y.Z" >&2; exit 2; }

list="$(git ls-remote --tags --refs "$remote" "refs/tags/${prefix}*")" || {
    echo "is-newest: could not read the ${prefix}* tags from ${remote}" >&2
    exit 2
}

newest="$(printf '%s\n' "$list" \
    | sed -n "s|.*refs/tags/${prefix}\([0-9]*\.[0-9]*\.[0-9]*\)$|\1|p" \
    | grep -E '^[0-9]+\.[0-9]+\.[0-9]+$' \
    | sort -V | tail -n1 || true)"

if [ -z "$newest" ]; then
    exit 0
fi
top="$(printf '%s\n%s\n' "$newest" "$ver" | sort -V | tail -n1)"
if [ "$top" = "$ver" ]; then
    exit 0
fi
echo "is-newest: ${prefix}${newest} is newer than ${prefix}${ver}" >&2
exit 1
