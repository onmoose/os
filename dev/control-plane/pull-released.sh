#!/usr/bin/env bash
# Pull the released control plane into the image bundle (#566).
#
#   pull-released.sh <dir> <version> [<owner>]
#
# Resolves ghcr.io/<owner>/brain and ghcr.io/<owner>/ui at image tag v<version>
# to their digests ONCE (dev/release/ghcr-resolve.sh), pulls each by digest,
# tags it with the local name the box runs (moose-brain:dev, moose-ui:dev: the
# brain drop-in and the control-plane compose name these, and a box never
# pulls them), and docker-saves it into <dir>. Then it checks that each
# tarball holds the image config the registry named, and writes the bundle
# record (bundle-record.sh) with the two pinned refs.
#
# <owner> defaults to onmoose. Called by `make control-plane-released`, which
# passes CONTROL_PLANE_VERSION: the last released control plane, so an OS
# release bakes a pair that really is a control-plane release (BUILD.md
# # Versioning). Fails when the tag is not on ghcr, which is the case when the
# control-plane release of that number did not publish: finish it first.
set -euo pipefail

[ "$#" -ge 2 ] && [ "$#" -le 3 ] || { echo "usage: pull-released.sh <dir> <version> [<owner>]" >&2; exit 2; }
dir="$1"; version="$2"; owner="${3:-onmoose}"
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
resolve="${here}/../release/ghcr-resolve.sh"

mkdir -p "$dir"
# A failed pull must not leave a record that names what is no longer there.
rm -f "$dir/control-plane.env"

declare -A ref want
for name in brain ui; do
    read -r digest config < <("$resolve" "${owner}/${name}" "v${version}") || {
        echo "pull-released: cannot resolve ghcr.io/${owner}/${name}:v${version}; is control-plane ${version} published?" >&2
        exit 1
    }
    remote="ghcr.io/${owner}/${name}@${digest}"
    echo "control plane ${version}: ${name} is ${remote} (config ${config})"
    docker pull --platform linux/amd64 "$remote"
    docker tag "$remote" "moose-${name}:dev"
    docker save "moose-${name}:dev" -o "$dir/moose-${name}.tar"
    ref[$name]="$remote"
    want[$name]="$config"
done

bash "${here}/bundle-record.sh" "$dir" released "$version" "${ref[brain]}" "${ref[ui]}"

# The tarballs must hold the configs the registry named, or the record (and
# the boot check that reads it) would describe other bytes than ghcr serves.
# shellcheck disable=SC1091
. "$dir/control-plane.env"
for pair in "brain:${MOOSE_BAKED_BRAIN_ID}" "ui:${MOOSE_BAKED_UI_ID}"; do
    name="${pair%%:*}"; got="${pair#*:}"
    if [ "$got" != "${want[$name]}" ]; then
        rm -f "$dir/control-plane.env"
        echo "pull-released: moose-${name}.tar holds config ${got}, but ghcr names ${want[$name]}" >&2
        exit 1
    fi
done
echo "pull-released: the bundle holds control plane ${version} as published"
