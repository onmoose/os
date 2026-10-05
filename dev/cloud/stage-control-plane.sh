#!/usr/bin/env bash
# Shared hosted-cloud control-plane staging (#242). SOURCED by both the
# production lean build (dev/cloud/bootstrap.sh) and the boot-proof test lane
# (dev/cloud/test/bootstrap.sh) so the runtime wiring is staged identically for
# both — the test image must validate the real production wiring, not a divergent
# copy (ENVIRONMENT.md # How the profile is realized).
#
# It builds + stages everything a hosted tenant box needs to self-bootstrap on
# first boot, into dev/cloud/mkosi.extra.wiring/ (an ExtraTree of dev/cloud/, so
# it lands in BOTH the production image and the test image, which Include=.. it):
#   - the slim host-agent (-tags hosted, #204) + its unit + the cloud brain drop-in
#   - the PAM stack host-agent-real's verify-password needs
#   - the control-plane image bundle (brain/ui/proxy + the acmedns Caddy) + the
#     first-boot loader unit (reused verbatim from the medium lane). The brain
#     and UI are a build of this commit, or the last released pair from ghcr
#     (MOOSE_CONTROL_PLANE_SOURCE, #566), recorded in /usr/lib/moose/control-plane.env
#   - the control-plane compose + caddy.json at the same host path the brain sees
#   - the first-boot provisioning-seed materializer + its unit
#
# Deliberately NOT staged here: the serial-driven self-check (cloud-assertions.sh
# + its unit) — that is test-lane-only and stays in dev/cloud/test/.
#
# Expects these set by the sourcing script: REPO_ROOT, CALLER (may be empty), GO,
# CP_BUNDLE, WIRING (the dev/cloud/mkosi.extra.wiring/ target dir), WORK (a build
# scratch dir for the intermediate host-agent binary).
# Sourced, not executed — inherits the caller's shell options. Both callers have
# set -euo pipefail; mirror it here so sourcing from a non-strict script still fails fast.
set -euo pipefail

# Build identity (BUILD.md # Versioning: "every build stamps two fields") — the
# same -ldflags the Makefile's LDFLAGS apply, recomputed here because they cannot
# be inherited: bootstrap.sh is invoked as `sudo -E ./dev/cloud/bootstrap.sh`, so
# make's MOOSE_VERSION/MOOSE_COMMIT are plain make variables that never reach this
# script. An unstamped build is silent — it ships internal/version's "dev" default,
# the brain's minimumAgentVersion check can't parse it as semver, an unparseable
# core sorts before every valid version, and a correctly-built box raises
# version-mismatch and blocks app installs. That is what v0.4.0 shipped.
#
# git runs as CALLER because under sudo the repo is owned by the invoking user and
# git refuses a dubious-ownership repo as root; the commit falls back to "unknown"
# exactly as the Makefile's does rather than failing a ~40min image build over it.
# Only Commit can degrade — Version comes from the file, so the health check the
# stamp exists for passes either way.
stage_version_ldflags() {
    local v c
    v="$(cat "${REPO_ROOT}/VERSION")"
    if [ -n "$CALLER" ]; then
        c="$(sudo -u "$CALLER" git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
    else
        c="$(git -C "$REPO_ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)"
    fi
    printf -- '-X github.com/onmoose/os/internal/version.Version=%s -X github.com/onmoose/os/internal/version.Commit=%s' "$v" "$c"
}

# Build a Go binary, as the invoking user when running under sudo so the caller
# owns the build cache. CGO on (PAM verify is kept in hosted) + CGO_CFLAGS as the
# Makefile sets — dynamic against the build host's libpam, run on the Debian VM.
stage_build_go() { # OUT PKG [extra go-build args...]
    local out="$1" pkg="$2"; shift 2
    local ldflags; ldflags="$(stage_version_ldflags)"
    if [ -n "$CALLER" ]; then
        sudo -u "$CALLER" env CGO_ENABLED=1 CGO_CFLAGS=-D_GNU_SOURCE "$GO" build -ldflags "$ldflags" "$@" -o "$out" "$pkg"
    else
        CGO_ENABLED=1 CGO_CFLAGS=-D_GNU_SOURCE "$GO" build -ldflags "$ldflags" "$@" -o "$out" "$pkg"
    fi
}

stage_control_plane() {
    local hostagent_bin="${WORK}/host-agent-real-hosted"

    # --- slim host-agent (-tags hosted, #204).
    stage_build_go "$hostagent_bin" "${REPO_ROOT}/cmd/host-agent-real/" -tags hosted

    # --- control-plane image bundle (#566, BUILD.md # Versioning). Which brain
    # and UI the image bakes is MOOSE_CONTROL_PLANE_SOURCE:
    #   local    (default) a build of this commit: `make control-plane-images`.
    #            Every run that publishes nothing, so a PR boots its own brain,
    #            and a control-plane release, whose images are not on ghcr yet
    #            (the publish job pushes the very ones the boots ran).
    #   released the last released control plane, CONTROL_PLANE_VERSION, pulled
    #            from ghcr by digest: `make control-plane-released`. An OS-only
    #            release bakes this, so the pair in the image is a real
    #            control-plane release and not unreleased code under its number.
    # The bundle records where it came from (control-plane.env). Reuse it only
    # when that matches what is asked for, or MOOSE_REBUILD_CP=1 forces it:
    # `make control-plane-images` re-runs the brain (Go) + UI (Vue) docker
    # builds, regenerating ~13 GB of BuildKit cache each time, and the images
    # don't change while iterating on the boot wiring. Both bootstraps of one CI
    # run stage from the same bundle, so the image that ships and the image the
    # boots run bake the same bytes.
    local cp_source="${MOOSE_CONTROL_PLANE_SOURCE:-local}" cp_version have=""
    cp_version="$(tr -d '[:space:]' < "${REPO_ROOT}/CONTROL_PLANE_VERSION")"
    case "$cp_source" in local|released) ;; *)
        echo "MOOSE_CONTROL_PLANE_SOURCE='${cp_source}' (want local or released)" >&2; return 1 ;;
    esac
    if [ -f "$CP_BUNDLE/control-plane.env" ]; then
        have="$(. "$CP_BUNDLE/control-plane.env" && echo "${MOOSE_BAKED_CP_SOURCE:-} ${MOOSE_BAKED_CP_VERSION:-}")"
    fi
    # The same source AND the same version: a local build stamps the brain
    # with CONTROL_PLANE_VERSION, so a bump must rebuild it too.
    local want="${cp_source} ${cp_version}"
    if [ "${MOOSE_REBUILD_CP:-0}" = "1" ] || ! ls "$CP_BUNDLE"/moose-brain.tar "$CP_BUNDLE"/moose-ui.tar \
            "$CP_BUNDLE"/caddy.tar "$CP_BUNDLE"/docker-socket-proxy.tar >/dev/null 2>&1 \
            || [ "$have" != "$want" ]; then
        if [ "$cp_source" = released ]; then
            echo "pulling the released control plane ${cp_version} from ghcr by digest..."
            make -C "$REPO_ROOT" control-plane-released
        else
            echo "building + saving control-plane image bundle (docker)..."
            make -C "$REPO_ROOT" control-plane-images
        fi
    else
        echo "reusing existing control-plane image bundle (${have}; set MOOSE_REBUILD_CP=1 to force)"
    fi
    echo "control plane baked into the image:"
    grep '^MOOSE_BAKED_' "$CP_BUNDLE/control-plane.env"

    # --- stage mkosi.extra.wiring/ (generated; gitignored).
    rm -rf "$WIRING"
    mkdir -p "$WIRING/etc/systemd/system/host-agent.service.d" \
             "$WIRING/usr/lib/moose" \
             "$WIRING/usr/local/bin" \
             "$WIRING/etc/pam.d" \
             "$WIRING/var/lib/moose/control-plane-images" \
             "$WIRING/var/lib/moose/control-plane"

    # Slim host-agent at the production path host-agent.service ExecStarts.
    cp "$hostagent_bin" "$WIRING/usr/lib/moose/host-agent-real"
    chmod 0755 "$WIRING/usr/lib/moose/host-agent-real"
    cp "${REPO_ROOT}/dist/systemd/host-agent.service" "$WIRING/etc/systemd/system/"

    # host-agent bootstrap drop-in: point the brain bootstrap at the baked
    # dev-tagged images + tarballs + the staged control-plane dir, and order after
    # the first-boot image load so every image is present when the bootstrap runs.
    # The brain reads /etc/moose/profile (mounted from the host by host-agent —
    # brainlaunch ProfileMarkerPath) to resolve profile=hosted; no env needed.
    cat > "$WIRING/etc/systemd/system/host-agent.service.d/10-cloud-brain.conf" <<'EOF'
[Unit]
After=moose-load-images.service

[Service]
Environment=MOOSE_BRAIN_IMAGE=moose-brain:dev
Environment=MOOSE_BRAIN_IMAGE_TAR=/var/lib/moose/control-plane-images/moose-brain.tar
Environment=MOOSE_PROXY_IMAGE=tecnativa/docker-socket-proxy:v0.4.2
Environment=MOOSE_PROXY_IMAGE_TAR=/var/lib/moose/control-plane-images/docker-socket-proxy.tar
Environment=MOOSE_CONTROL_PLANE_DIR=/var/lib/moose/control-plane
Environment=MOOSE_DASHBOARD_UI_UPSTREAM=moose-ui:80
Environment=MOOSE_CADDY_IMAGE=moose-caddy-acmedns:dev
EOF

    # PAM stack for host-agent-real's verify-password (kept in hosted). Without it
    # pam_start("moose") falls back to /etc/pam.d/other (deny). The moose group is
    # provisioned by the postinst.
    cp "${REPO_ROOT}/dev/pam/moose" "$WIRING/etc/pam.d/moose"

    # Control-plane image bundle + first-boot loader (reused verbatim from the
    # medium lane — same offline-first mechanism, TESTING.md # Full-stack control-
    # plane integration). A tenant box is air-gapped at boot, so every image is a
    # local tarball; the VM never pulls.
    cp "$CP_BUNDLE"/*.tar "$WIRING/var/lib/moose/control-plane-images/"
    # The bundle's record, in the slot: which control plane this image bakes
    # and the image ID each one loads as. The boot lane checks the running
    # brain and UI against it (cloud-assertions.sh step 5a).
    cp "$CP_BUNDLE/control-plane.env" "$WIRING/usr/lib/moose/control-plane.env"
    # Hosted-only Caddy swap: the wildcard cert needs the caddy-dns/acmedns module
    # (ACME DNS-01 — os #207/C3b), which stock caddy:2-alpine lacks. Build the
    # xcaddy recipe and docker-save it OVER the *staged* caddy.tar — not the shared
    # $CP_BUNDLE copy, which the appliance/medium lane keeps on stock caddy (it does
    # no ACME). The drop-in above sets MOOSE_CADDY_IMAGE so the brain's control-plane
    # compose runs this image; load-control-plane-images.sh loads it from the tar
    # regardless of filename (build-host network only; the VM never pulls).
    local caddy_acmedns_image="moose-caddy-acmedns:dev"
    echo "building hosted Caddy with the caddy-dns/acmedns module (xcaddy)..."
    # Through make, not `docker build`: the target feeds the Dockerfile its two
    # digest-pinned base images from dev/control-plane/images.lock (#432).
    make -C "$REPO_ROOT" caddy-acmedns-image CADDY_ACMEDNS_IMAGE="$caddy_acmedns_image"
    docker save "$caddy_acmedns_image" -o "$WIRING/var/lib/moose/control-plane-images/caddy.tar"
    cp "${REPO_ROOT}/dev/test-qemu/load-control-plane-images.sh" "$WIRING/usr/lib/moose/"
    chmod 0755 "$WIRING/usr/lib/moose/load-control-plane-images.sh"
    cp "${REPO_ROOT}/dev/test-qemu/moose-load-images.service" "$WIRING/etc/systemd/system/"

    # Control-plane compose + caddy.json staged at the SAME host path the brain
    # container sees (same-path bind constraint — socket-proxy-compose-validation.md).
    cp "${REPO_ROOT}/dev/control-plane/compose.yml" "$WIRING/var/lib/moose/control-plane/"
    cp "${REPO_ROOT}/dev/control-plane/caddy.json"   "$WIRING/var/lib/moose/control-plane/"

    # No catalog is baked into the image (cloud #62). The brain syncs the store from
    # the control plane's public-read catalog API (GET /catalog, MOOSE_CATALOG_URL
    # default the apex) and holds it in memory; only proxied icons and screenshots are
    # cached, under /var/lib/moose/catalog-cache. A box that cannot reach the control
    # plane shows an empty store (the documented, accepted behavior — installing an app
    # needs internet regardless). This lane installs no app, so an empty store is fine
    # here.

    # First-boot provisioning-seed materializer + its oneshot (C3a cloud-lane, #220).
    # Lands the delivered seed at /var/lib/moose/seed.json before host-agent launches
    # the brain; the postinst enables the unit. The materializer reads the SMBIOS
    # systemd-credential channel (the test lane + clouds that deliver via fw_cfg)
    # first, falling back to the real-cloud metadata/user-data endpoint when absent
    # (Hetzner — #246).
    cp "${REPO_ROOT}/dev/cloud/moose-seed-materialize.sh" "$WIRING/usr/local/bin/moose-seed-materialize.sh"
    chmod 0755 "$WIRING/usr/local/bin/moose-seed-materialize.sh"
    cp "${REPO_ROOT}/dev/cloud/moose-seed.service" "$WIRING/etc/systemd/system/"

    # The RAUC keyring, /etc/rauc/keyring.pem (#562): the release root on a
    # build that publishes the OS, a throwaway root otherwise
    # (MOOSE_RAUC_KEYRING, dev/cloud/rauc.sh). A release build without the
    # committed release root fails here, before mkosi runs.
    # shellcheck source=dev/cloud/rauc.sh
    . "${REPO_ROOT}/dev/cloud/rauc.sh"
    rauc_stage_keyring "$WIRING"
}
