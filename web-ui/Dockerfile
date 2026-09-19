# check=skip=InvalidDefaultArgInFrom
# moose-ui control-plane image (BUILD.md # 5b; WEB_UI.md # deploy model).
# Build context is web-ui/, through `make ui-image` — that target feeds the two
# base images in from dev/control-plane/images.lock. They have no default here on
# purpose: a default is a second copy of a pin, free to drift, and a bare
# `docker build` would then bake unpinned bytes (#432; BUILD.md # 5c). The runtime
# base is the same CADDY_IMAGE pin the proxy runs, so both hold one Caddy.
#
#   build   — produce the static SPA bundle with Vite.
#   runtime — Caddy serving the bundle from /srv/ui with the SPA
#             Caddyfile (fallback to index.html, compression + ETag on).

ARG UI_BUILDER_IMAGE
ARG UI_RUNTIME_IMAGE
FROM ${UI_BUILDER_IMAGE} AS build
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci
COPY . .
# `vite build` directly, not `npm run build`: the image's job is to produce the
# bundle. Type-checking (vue-tsc) is a separate CI gate (make check-web), not a
# step in baking the artifact.
RUN npx vite build

FROM ${UI_RUNTIME_IMAGE} AS runtime
COPY Caddyfile /etc/caddy/Caddyfile
COPY --from=build /app/dist /srv/ui
