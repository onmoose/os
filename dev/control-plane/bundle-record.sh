#!/usr/bin/env bash
# Write the record of a control-plane image bundle (#566): where the brain and
# UI tarballs came from, and the image ID each one loads as.
#
#   bundle-record.sh <dir> local <version>
#   bundle-record.sh <dir> released <version> <brain ref> <ui ref>
#
# <dir> holds moose-brain.tar and moose-ui.tar (`make control-plane-images` or
# `make control-plane-released`). The record goes to <dir>/control-plane.env.
# dev/cloud/stage-control-plane.sh reads it to decide whether the bundle can be
# reused, and bakes it into the image as /usr/lib/moose/control-plane.env,
# where the boot lane checks the running brain and UI against it.
#
# The image ID is the sha256 of the image config, read from the tarball's own
# manifest.json, so it does not depend on which image store the build host's
# Docker uses. A box loads the tarball into Docker's classic store (the
# user-namespace remap turns the containerd store off, BUILD.md # 1), where the
# image ID is exactly that digest.
set -euo pipefail

usage() { echo "usage: bundle-record.sh <dir> local <version> | <dir> released <version> <brain ref> <ui ref>" >&2; exit 2; }
[ "$#" -ge 3 ] || usage
dir="$1"; source="$2"; version="$3"
brain_ref=""; ui_ref=""
case "$source" in
    local) [ "$#" -eq 3 ] || usage ;;
    released) [ "$#" -eq 5 ] || usage; brain_ref="$4"; ui_ref="$5" ;;
    *) usage ;;
esac
case "$version" in
    [0-9]*.[0-9]*.[0-9]*) ;;
    *) echo "bundle-record: '${version}' is not a version" >&2; exit 1 ;;
esac

# config_id TAR: "sha256:<hex>" of the one image in TAR. Docker 25+ writes
# "Config": "blobs/sha256/<hex>", older Docker "<hex>.json".
config_id() {
    tar -xOf "$1" manifest.json | python3 -c '
import json, re, sys
m = json.load(sys.stdin)
if len(m) != 1:
    sys.exit("want one image in the tarball, found %d" % len(m))
c = m[0]["Config"]
h = re.fullmatch(r"(?:blobs/sha256/)?([0-9a-f]{64})(?:\.json)?", c)
if not h:
    sys.exit("cannot read a config digest from %r" % c)
print("sha256:" + h.group(1))'
}

brain_id="$(config_id "$dir/moose-brain.tar")"
ui_id="$(config_id "$dir/moose-ui.tar")"

tmp="$dir/.control-plane.env.tmp"
cat > "$tmp" <<EOF
# The control plane this image bakes (#566, BUILD.md # Versioning).
# source=released: the brain and UI of release ${version}, pulled from ghcr by digest.
# source=local: built from this commit, labelled with the last released number.
MOOSE_BAKED_CP_SOURCE=${source}
MOOSE_BAKED_CP_VERSION=${version}
MOOSE_BAKED_BRAIN_REF=${brain_ref}
MOOSE_BAKED_UI_REF=${ui_ref}
MOOSE_BAKED_BRAIN_ID=${brain_id}
MOOSE_BAKED_UI_ID=${ui_id}
EOF
mv "$tmp" "$dir/control-plane.env"
echo "control-plane bundle: ${source} ${version}, brain ${brain_id}, ui ${ui_id}"
