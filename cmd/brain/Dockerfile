# check=skip=InvalidDefaultArgInFrom
# moose-brain control-plane image (BUILD.md # 5; DECISIONS.md 2026-06-13).
# Build context is the repo root, through `make brain-image` — that target feeds
# the two base images in from dev/control-plane/images.lock. They have no default
# here on purpose: a default is a second copy of a pin, free to drift, and a bare
# `docker build` would then bake unpinned bytes (#432; BUILD.md # 5c).
#
#   build   — compile the brain as a static binary. CGO is off: the brain's only
#             sqlite is modernc.org/sqlite (pure Go), so nothing links libc.
#   runtime — a debian slim base with the docker CLI + compose plugin bundled.
#             NOT distroless: the brain orchestrates apps by shelling out to
#             `docker` / `docker compose` (internal/lifecycle/docker.go), which a
#             shell-less distroless runtime cannot host (DECISIONS.md 2026-06-13).

ARG BRAIN_BUILDER_IMAGE
ARG BRAIN_RUNTIME_IMAGE
FROM ${BRAIN_BUILDER_IMAGE} AS build
WORKDIR /src
# Resolve the module graph before copying the tree so the download layer caches
# across source-only changes.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build identity (BUILD.md # Versioning): VERSION is in the build context (repo
# root), but .git is excluded (.dockerignore — never ship dev/VCS state into the
# image), so the commit can't be resolved with `git rev-parse` in here. The
# Makefile's brain-image target passes it in as a build-arg instead.
ARG MOOSE_COMMIT=unknown
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w \
      -X github.com/onmoose/moose/internal/version.Version=$(cat VERSION) \
      -X github.com/onmoose/moose/internal/version.Commit=${MOOSE_COMMIT}" \
      -o /out/moose-brain ./cmd/brain

FROM ${BRAIN_RUNTIME_IMAGE} AS runtime
# docker-ce-cli + docker-compose-plugin from Docker's official apt repo — the
# same trusted source as the host engine (BUILD.md # Docker package source). The
# repo codename is read from /etc/os-release so this tracks the base image.
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends ca-certificates curl; \
    install -m 0755 -d /etc/apt/keyrings; \
    curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc; \
    chmod a+r /etc/apt/keyrings/docker.asc; \
    . /etc/os-release; \
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian ${VERSION_CODENAME} stable" \
        > /etc/apt/sources.list.d/docker.list; \
    apt-get update; \
    apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin; \
    apt-get purge -y --auto-remove curl; \
    rm -rf /var/lib/apt/lists/*
# Declare the wire-protocol major host-agent checks before launch (lockstep
# guard — CONTROL_PLANE.md # Locked: host-agent launches the brain container).
# Must equal internal/protocol.Major; bump both together with the /v<N> URL
# prefix.
LABEL moose.protocol.major="1"
COPY --from=build /out/moose-brain /usr/local/bin/moose-brain
# State dir and catalog are bind-mounts the supervisor provides at runtime; the
# brain reads its config from MOOSE_* env (cmd/brain/main.go # loadConfig).
ENTRYPOINT ["/usr/local/bin/moose-brain"]
