#!/usr/bin/env bash
# Attach an OS release's files to its GitHub Release as ONE set, and never
# overwrite a published asset.
#
#   attach-image.sh <tag> <file> <file> [<file>...]
#
# ci-cloud-image.yml passes four files: the disk image, its checksum, the RAUC
# bundle and its checksum (#562). A rebuild is not byte-identical, so the files
# of one build only belong with each other: a checksum only matches its own
# file, and the bundle carries the same slot A as the image of its build.
#   - all on the Release: an earlier attempt finished; leave them (exit 0).
#   - none: upload all of them in one call (exit 0).
#   - only some: an earlier attempt stopped halfway, or the Release was cut
#     before the set grew (a Release with the image but no bundle). Uploading
#     the rest from this build would mix two builds, so refuse (exit 1) and tell
#     a person which assets to delete before re-running. Nothing published is
#     deleted here.
# No --clobber: if an asset appears between the check and the upload, the
# upload fails instead of overwriting it.
#
# Needs `gh` with GH_TOKEN; ci-cloud-image.yml calls it after making sure the
# Release exists.
set -euo pipefail

usage="usage: attach-image.sh <tag> <file> <file> [<file>...]"
tag="${1:?$usage}"
shift
[ "$#" -ge 2 ] || { echo "$usage" >&2; exit 2; }

existing="$(gh release view "$tag" --json assets --jq '.assets[].name')"
has() { grep -qxF "$1" <<<"$existing"; }

present=()
missing=()
for f in "$@"; do
    name="$(basename -- "$f")"
    if has "$name"; then present+=("$name"); else missing+=("$name"); fi
done

if [ "${#missing[@]}" -eq 0 ]; then
    echo "::notice::release ${tag} already has ${present[*]}; leaving them (delete them all first to replace them)"
elif [ "${#present[@]}" -eq 0 ]; then
    gh release upload "$tag" "$@"
    echo "attached ${missing[*]} to release ${tag}"
else
    deletes=""
    for name in "${present[@]}"; do deletes="${deletes}gh release delete-asset ${tag} ${name} -y; "; done
    echo "::error::release ${tag} has ${present[*]} but not ${missing[*]}, so an earlier upload stopped halfway (or the Release was cut before this set grew). This build cannot add the rest: a rebuild has different bytes, so the files would not belong together. Delete the stray assets and re-run: ${deletes}"
    exit 1
fi
