# check=skip=InvalidDefaultArgInFrom
# (The two FROM args are deliberately default-less — see the pin note below.
# A BuildKit check directive has to be the first line of the file.)
# Custom Caddy with the acme-dns DNS provider (caddy-dns/acmedns) compiled in.
#
# The hosted profile serves every app at "<slug>.<box-id>.onmoose.io" over a
# real Let's Encrypt wildcard cert obtained via ACME DNS-01 against the box's
# per-box acme-dns account (ENVIRONMENT.md # Networking & discovery; os #207 /
# C3b). DNS-01 needs a DNS-provider module in the Caddy binary, and the stock
# caddy:2-alpine image ships none — hence this xcaddy build.
#
# Only the hosted image points MOOSE_CADDY_IMAGE at this build (see
# ../compose.yml). Appliance keeps riding stock caddy:2-alpine: it does no ACME
# (plain-HTTP ".local"), so adding the module there would be unused surface and
# would drop it off the upstream official image. When the appliance secure-URLs
# toggle ships, flipping it onto this same image is a one-line change.
#
# Build (from the repo root):
#   make caddy-acmedns-image
# The real DNS-01 issuance path is exercised end-to-end in the cloud on-ramp
# (cloud #6 / CL6), not the inner loop (no real ACME/DNS locally).
# Both base images and the plugin module are pinned in dev/control-plane/images.lock
# and passed in as build args (#432; BUILD.md # 5c). They have NO
# default on purpose: a default would be a second copy of the pin, free to drift,
# and a bare `docker build` here would then quietly bake unpinned bytes. Build it
# with `make caddy-acmedns-image`, which reads the pin file.
#
# The pins are held at Caddy 2.10.0: its builder ships Go 1.24.x, caddy-dns/acmedns
# >=0.7.0 requires Go >=1.24, and the builder pins GOTOOLCHAIN=local (no toolchain
# auto-download), so an older Caddy (2.8.4 → Go 1.23) fails `xcaddy build` with a
# "requires go >= 1.24" error.
ARG CADDY_ACMEDNS_BUILDER_IMAGE
ARG CADDY_ACMEDNS_BASE_IMAGE
FROM ${CADDY_ACMEDNS_BUILDER_IMAGE} AS builder
# The module is version-pinned too: without a version xcaddy takes whatever is
# latest that day, so the plugin could change while both base images stayed put.
# Caddy's own version needs no argument here — it comes from the builder image.
ARG CADDY_ACMEDNS_MODULE
RUN xcaddy build --with ${CADDY_ACMEDNS_MODULE}

FROM ${CADDY_ACMEDNS_BASE_IMAGE}
COPY --from=builder /usr/bin/caddy /usr/bin/caddy
