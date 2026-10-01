#!/usr/bin/env bash
# Attach the OS disk image and its checksum to a GitHub Release, as ONE pair,
# and never overwrite a published asset.
#
#   attach-image.sh <tag> <image-file> <checksum-file>
#
# A rebuild is not byte-identical, so the checksum of this build only matches
# the image of this build:
#   - both on the Release: an earlier attempt finished; leave them (exit 0).
#   - neither: upload both in one call (exit 0).
#   - exactly one: an earlier attempt stopped halfway. Uploading the missing
#     half from this build would pair an old image with a new checksum, or the
#     other way round, so refuse (exit 1) and tell a person which stray asset to
#     delete before re-running. Nothing published is deleted here.
# No --clobber: if an asset appears between the check and the upload, the
# upload fails instead of overwriting it.
#
# Needs `gh` with GH_TOKEN; ci-cloud-image.yml calls it after making sure the
# Release exists.
set -euo pipefail

tag="${1:?usage: attach-image.sh <tag> <image-file> <checksum-file>}"
asset="${2:?usage: attach-image.sh <tag> <image-file> <checksum-file>}"
checksum="${3:?usage: attach-image.sh <tag> <image-file> <checksum-file>}"
a_name="$(basename -- "$asset")"
c_name="$(basename -- "$checksum")"

existing="$(gh release view "$tag" --json assets --jq '.assets[].name')"
has() { grep -qxF "$1" <<<"$existing"; }

if has "$a_name" && has "$c_name"; then
    echo "::notice::release ${tag} already has ${a_name} and ${c_name}; leaving them (delete both first to replace them)"
elif ! has "$a_name" && ! has "$c_name"; then
    gh release upload "$tag" "$asset" "$checksum"
    echo "attached ${a_name} + ${c_name} to release ${tag}"
else
    if has "$a_name"; then stray="$a_name"; else stray="$c_name"; fi
    echo "::error::release ${tag} has ${stray} but not its pair, so an earlier upload stopped halfway. This build cannot add the other half: a rebuild has different bytes, so the image and checksum would not match. Delete the stray asset and re-run: gh release delete-asset ${tag} ${stray}"
    exit 1
fi
