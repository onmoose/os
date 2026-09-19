#!/bin/bash
# First-boot loader for the bundled control-plane images (#163). The medium-lane
# image bakes moose-brain, moose-ui, caddy and docker-socket-proxy as `docker
# save` tarballs (offline-first — TESTING.md # Full-stack control-plane
# integration); this docker-loads them into the guest's /var/lib/docker so the
# brain's `docker compose up` finds them locally with no network. Run once,
# gated by moose-load-images.service on the marker written on success.
set -euo pipefail

IMG_DIR=/var/lib/moose/control-plane-images
MARKER=/var/lib/moose/.control-plane-images-loaded

shopt -s nullglob
tarballs=("$IMG_DIR"/*.tar)
if [ ${#tarballs[@]} -eq 0 ]; then
    echo "no image tarballs in $IMG_DIR" >&2
    exit 1
fi

for tar in "${tarballs[@]}"; do
    echo "loading $tar"
    docker load -i "$tar"
done

touch "$MARKER"
echo "loaded ${#tarballs[@]} control-plane image(s)"
