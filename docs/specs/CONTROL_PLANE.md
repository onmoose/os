# moose Control Plane

> Working spec for moose's control plane — the brain that orchestrates apps, routing, identity, storage, and the mesh. Companion to `SPEC.md`.
>
> **Topical specs that used to live here:**
> - App lifecycle (install, run, update, uninstall, reconciler, slug allocation, Caddy timing) → **`APP_LIFECYCLE.md`**
> - Brain ↔ UI API protocol (sync, jobs, SSE, errors, auth, versioning) → **`BRAIN_UI_PROTOCOL.md`**
> - Brain ↔ host-agent protocol → **`BRAIN_HOST_PROTOCOL.md`**
> - Web UI codebase, stack, deploy model → **`WEB_UI.md`**
>
> This doc stays at the architectural-overview level: what the control plane is, its layered shape, and the brain's deployment-time decisions.
>
> **Environment profiles.** The control plane described here is **Layer 1** — identical across the `appliance` and `hosted` profiles. Only Layer 2 (the base image and `host-agent`) diverges; the hosted profile ships a build-tagged slim cloud `host-agent` and leaves brain / Caddy / `moose-ui` / socket-proxy unchanged. See `ENVIRONMENT.md` # Two layers, treated differently.

## What the control plane is responsible for

- **App lifecycle** — install, start, stop, update, uninstall. Talks to Docker. Full spec in `APP_LIFECYCLE.md`.
- **Routing** — dynamically configures the reverse proxy as apps come and go (`photos.local` → container).
- **mDNS publishing** — registers each app's hostname with Avahi (via `host-agent`).
- **API + UI** — HTTP + SSE + (future) WebSocket API for the web UI; ships the UI. Wire spec in `BRAIN_UI_PROTOCOL.md`; client-side stack in `WEB_UI.md`.
- **Identity** — user accounts, sessions, admin/root account, sharing/ACLs. See `AUTH.md`.
- **Managed services** — runs shared Postgres / Redis / etc., provisions databases on app install, handles backups + upgrades. See `SERVICE_PROVISIONING.md`.
- **Storage** — manages the data drive (mergerfs union, ext4+LUKS+TPM unlock), bind mounts for `/home/` and `/var/lib/moose/`, app data directories. See `STORAGE.md`.
- **Mesh** — integrates with Headscale, registers the box, manages pairing tokens, surfaces people/devices UI. See `MOOSE_NETWORK.md`.
- **Backups** — orchestrates app backup hooks, schedules, restores.
- **Auto-updates** — apps, the OS itself, and the control plane. See `UPDATES.md`.

## Layered architecture

```
┌─────────────────────────────────────────────────────────┐
│                       Host (Debian)                     │
│                                                         │
│   ┌─────────────────┐                                   │
│   │ host-agent      │ ← tiny native binary, systemd     │
│   │ (Go, static)    │   • mounts disks / mergerfs pool  │
│   └────────┬────────┘   • bootstraps the brain          │
│            │            • OS-level updates              │
│            ▼                                            │
│   ┌──────────────────────────────────────────────┐      │
│   │           Docker (containerd under)          │      │
│   │                                              │      │
│   │  ┌──────────────┐  ┌──────────────────────┐  │      │
│   │  │ moose-brain  │  │ Caddy (reverse proxy)│  │      │
│   │  │ (Go daemon)  │◄─┤ config via admin API │  │      │
│   │  │  + SQLite    │  └──────────────────────┘  │      │
│   │  │              │  ┌──────────────────────┐  │      │
│   │  │              │  │  managed Postgres    │  │      │
│   │  │              │◄─┤  (per major version) │  │      │
│   │  │              │  └──────────────────────┘  │      │
│   │  │              │  ┌──────────────────────┐  │      │
│   │  │              │  │  managed Redis       │  │      │
│   │  │              │◄─┤                      │  │      │
│   │  │              │  └──────────────────────┘  │      │
│   │  │              │  ┌──────────────────────┐  │      │
│   │  │              │  │  user apps           │  │      │
│   │  │              │◄─┤  (Photos, Grocery…)  │  │      │
│   │  │              │  └──────────────────────┘  │      │
│   │  └──────┬───────┘                            │      │
│   │         │ Docker API (via socket proxy)      │      │
│   └─────────┼────────────────────────────────────┘      │
│             ▼                                           │
│   ┌─────────────┐  ┌─────────────┐                      │
│   │   Avahi     │  │  Headscale  │                      │
│   │ (mDNS host) │  │   client    │                      │
│   └─────────────┘  └─────────────┘                      │
└─────────────────────────────────────────────────────────┘
```

### Layer 1 — `host-agent` (native, tiny)

A small native Go binary running as a systemd service. **Does as little as possible.**

- Bootstrap the system at boot — unlock and mount the data drive(s), assemble the mergerfs union, check disks, start Docker.
- Pull and start the `moose-brain`.
- Apply OS-level updates (today: `apt`; tomorrow: A/B image swaps).
- Recover the brain if it crashes.

When we eventually move to A/B immutable updates, this is the only piece that's *part of* the immutable OS image. It's small, changes rarely. **This positioning makes the future immutable migration painless.**

For v1, host-agent could be a few hundred lines of Go. Deliberately boring.

The brain ↔ host-agent wire-level contract is specified in **`BRAIN_HOST_PROTOCOL.md`** (HTTP/JSON over UNIX socket, two API patterns, SSE for streams, lockstep versioning). Anything that crosses this boundary — mDNS publish/unpublish, OS updates, Tier-2 systemd/config ops — runs on that protocol.

### Layer 2 — `moose-brain` (where 95% of logic lives)

A single Go process holding all orchestration logic. Internal packages:
- App manager — install / lifecycle / updates (spec: `APP_LIFECYCLE.md`)
- Proxy manager — talks to Caddy admin API
- Mesh manager — talks to Headscale
- Service manager — provisions DBs in managed Postgres / Redis
- Storage manager — mergerfs, tiers, app data dirs
- Identity — accounts, sessions, ACLs
- API server — serves the dashboard API (spec: `BRAIN_UI_PROTOCOL.md`)
- Backup orchestrator
- **Health manager** — owns the typed set of active health issues, consults them to gate write/app/user operations, surfaces them via the API + SSE channel. The brain runs in *degraded mode* when any issue is active. Spec: `HEALTH.md`.

Persists its own state in **SQLite** (single file, no separate DB process for moose's own data; managed Postgres is for *apps*, SQLite is for *moose*).

**Why Go:** every adjacent tool is in Go (Docker, containerd, Headscale, Caddy, Traefik, Avahi bindings). Static binary, mature concurrency model, the lingua franca of the ecosystem.

**Why one binary, not microservices:** single-node home appliance, microservices are pure overhead. Internal modularity via clean Go packages is enough.

### Layer 3 — managed sidecars

- **Caddy** as the reverse proxy. Brain calls its admin API to add/remove routes when apps install. Built for dynamic config; native Let's Encrypt.
- **`moose-ui`** — the dashboard's static file server (`caddy:alpine` + the built UI bundle baked in). A brain-launched control-plane container; Caddy routes all non-API traffic to it. Stack and deploy model in `WEB_UI.md`.
- **Managed Postgres** — one shared instance per major version users depend on. Brain is the only DB creator. Apps get scoped credentials.
- **Managed Redis** similarly.
- **User apps** — each its own compose stack, brain manages lifecycle.

The brain runs *next to* these and orchestrates them — not inside them.

## Decisions

### Locked: brain runs as a container

- The `moose-brain` ships as a Docker image, run by Docker, supervised by `host-agent`.
- **Why:** atomic production updates (pull new tag, recreate container, trivial rollback by reverting tag). No partial-install failure modes the way `apt`-based deploys produce. Same image runs in dev, staging, prod. Future migration to A/B immutable OS updates becomes a host-agent change, not a brain change — the brain doesn't move.
- **Cost we accept:** marginally slower dev loop than `go run` on the host. Mitigated with bind-mount + `air` (or equivalent) hot-reload during development. Production wins are worth the dev tax.
- Performance is a non-factor — container overhead for a long-running daemon is negligible.

### Locked: Docker socket exposure mitigated by socket proxy

- The brain does **not** mount `/var/run/docker.sock` directly. It talks to a `tecnativa/docker-socket-proxy` container at `tcp://docker-proxy:2375`. Brain config: one env var (`DOCKER_HOST=tcp://docker-proxy:2375`).
- The proxy is configured with an allowlist of Docker API endpoint families the brain actually needs (`CONTAINERS=1`, `IMAGES=1`, `NETWORKS=1`, `VOLUMES=1`, `POST=1`, plus `PING`/`VERSION`/`INFO`). Every family is load-bearing — measured against the real brain paths in #430 — so the list is already as narrow as it can be.
- **The allowlist filters by URL prefix and method only; the proxy never reads request bodies.** So `EXEC` is genuinely denied — it is a URL family the allowlist omits. But the body of an allowed `POST /containers/create` is not inspected: `Privileged: true` and `Binds: ["/:/host"]` pass through, and `CONTAINERS=1` also permits `/containers/<id>/start` (the pinned `v0.4.2` has no separate `ALLOW_START`). Granting `CONTAINERS` + `POST` therefore lets any caller that can reach `:2375` start a host-root container — a full container escape, **measured on a real box in #430**. This is *not* closed by the allowlist (the brain genuinely needs `CONTAINERS`+`POST`); the only control is **network reachability** — nothing but the brain may reach the proxy. That is not yet enforced: the proxy shares `moose-ingress` with app `main_service` containers, so a compromised app can reach it today. **#187** (apps off `moose-ingress`, Caddy connects outward into per-app networks) is the fix, and it is the fix for this live escape, not just admin-port isolation. See `THREAT_MODEL.md` B2.
- **Why the proxy at all:** the brain has the largest attack surface (HTTP API exposed to the LAN, third-party app manifests we evaluate). If it's ever compromised, the proxy keeps `EXEC` and the raw socket file off its transport. It is defense in depth — one extra container, one env var, ~5MB RAM — **not** a body-level guard: it does not stop a compromised *brain* from asking Docker for a privileged container. What bounds that is who can reach the brain and the proxy, not the allowlist.
- **Operational rule:** when the brain needs a new Docker API endpoint family, that's an explicit config change to the proxy. Forces conscious thought about what privileges the brain holds.
- **host-agent seeds the proxy (M1b, #165).** The proxy is the brain's *only* path to Docker, and a process cannot bring up its own sole transport — so the proxy is **not** in the brain's control-plane compose. host-agent, which holds the raw socket, creates the `moose-ingress` network and launches the `docker-socket-proxy` (raw socket bind-mounted read-only, the one place it is exposed to a container) *before* launching the brain, then sets the brain's `DOCKER_HOST`. The proxy is brain transport *infrastructure*, seeded by host-agent; Caddy + `moose-ui` are *services* the brain owns and reconciles (`lifecycle.EnsureControlPlane`). The brain reaches the proxy by the `docker-proxy` network alias regardless of the container's own (`moose-`-prefixed) name. See `socket-proxy-compose-validation.md` and `DECISIONS.md` 2026-06-14.
- **`EXEC` stays denied; managed-DB provisioning runs in a one-shot container instead.** Managed databases (`internal/lifecycle/services.go`) provision per-app roles/databases by running the engine's client (`psql`/`mysql`/`valkey-cli`) in a throwaway `docker run --rm` container joined to the service network, connecting over TCP — *not* by `docker exec`'ing the long-running service container (`DECISIONS.md` 2026-06-15). This was the re-architecture the 2026-06-14 call gated managed-DB-in-production on: it keeps `EXEC` denied (the `docker run` family uses `POST /containers/create` + `/start` + `/attach`, all under the allowed `CONTAINERS`/`POST` families) and the brain off the service network (only the ephemeral container joins it). Readiness polls the service's compose healthcheck via `docker inspect` (`CONTAINERS`), not an exec'd probe. Managed DB therefore works under the proxy in production; dev (native raw socket) was never affected.

### Locked: host-agent runs under systemd as `Type=notify`

- Unit type **`Type=notify`**, not `simple`. host-agent calls `sd_notify(READY=1)` only after its UNIX socket is bound and accepting connections. Anything ordered `After=host-agent.service` (brain container, downstream services) sees a ready socket on first try — no startup races.
- **`Restart=always`**, `RestartSec=2s`, `StartLimitBurst=5` / `StartLimitIntervalSec=60s`. Crash → restart fast, but a sustained crashloop (5 in 60s) stops and surfaces failure. On stop, `OnFailure=moose-recovery.target` routes the box into recovery boot — host-agent itself broken means the brain can't run, so the dashboard isn't reachable and a separate rescue page is the only option (see `BOOT.md` # Failure → recovery target — the narrow cases).
- **Watchdog enabled.** `WatchdogSec=30s`; host-agent pings `sd_notify(WATCHDOG=1)` every ~10s from a dedicated goroutine. Converts a hung process into a restart. Conservative interval avoids false positives during long legitimate operations (backup tarball walks, large image pulls).
- **Ordering**: `After=moose-storage-ready.target docker.service network-online.target`, `Wants=network-online.target moose-storage-ready.target`, `Requires=docker.service`. host-agent starts even if storage assembly partially failed — it reads `/run/moose/health/storage.json` and forwards findings to the brain, which raises health issues per `HEALTH.md`. The brain is the single source of truth for "is it safe to write right now"; systemd ordering is best-effort, not strict-gate. Full boot-chain context in `BOOT.md`.
- **The runtime directory is preserved across a stop**: `RuntimeDirectory=moose` with **`RuntimeDirectoryPreserve=yes`**. The second line is load-bearing and not a tidiness choice. systemd deletes a `RuntimeDirectory=` when the unit stops and makes a **new inode** on start; the brain reaches the socket through a bind mount of that *directory*, and Docker resolves a bind mount when the container starts, not per call. So without `Preserve` a plain `systemctl restart host-agent` leaves the brain looking at the deleted inode with no `agent.sock` in it — every host-backed call fails, **login included** (the brain holds no password hash, `AUTH.md` # Identity primitive), and nothing closes the window, since the brain runs `restart=unless-stopped` and host-agent does not touch a running brain. The box then looks healthy from outside and nobody can log in until a reboot (#447). Preserving the directory also stops host-agent's own stop from deleting `/run/moose/health/storage.json`, which `moose-storage-verify` writes once at boot and never regenerates (`BOOT.md` # The storage-ready target). Nothing is leaked by keeping it: `/run` is a tmpfs, so a reboot clears it regardless, and host-agent unlinks a stale `agent.sock` before it binds.
- **Socket activation is deliberately deferred** — extra complexity with no win on an always-on appliance. Tracked in `NEXT.md` if we ever revisit. It is the canonical answer to "the socket must outlive the service", and it would additionally let connections queue in the kernel backlog through a restart rather than fail; `RuntimeDirectoryPreserve=yes` above buys the ~2s restart window instead of the permanent outage, which is the part that mattered (#447).

### Locked: host-agent hardening directives

host-agent is root, but its filesystem and kernel reach is constrained by systemd:

```
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
ReadWritePaths=/var/lib/moose /etc/moose /run/moose
PrivateTmp=true
ProtectKernelTunables=true
ProtectKernelModules=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
```

`ProtectHome=true` is load-bearing: **host-agent has no general filesystem-read API over `/home`.** Any operation that touches a user's home directory is a narrow, named operation in `BRAIN_HOST_PROTOCOL.md`, not a generic file-read primitive. This is a deliberate constraint, not an oversight.

Capability dropping (`CapabilityBoundingSet=`) is **not** used — host-agent's job is "do root things on the box" (mount, cryptsetup, useradd, systemctl). The filesystem and kernel constraints above are where the meaningful blast-radius reduction happens.

### Locked: host-agent launches the brain container

- During its own startup, after Docker is ready, host-agent pulls (if needed) and starts the brain container with Docker restart policy `unless-stopped`. After that, Docker keeps brain alive across host-agent restarts; host-agent does not actively supervise brain during steady-state operation.
- **One chain of custody.** host-agent owns every container on the box (apps, managed services, *and* brain). The reconciler pattern from `APP_LIFECYCLE.md` extends to brain naturally.
- **Lockstep version check happens at launch.** host-agent refuses to start a brain whose major protocol version it doesn't speak (per `BRAIN_HOST_PROTOCOL.md`). One actor owns both endpoints' lifecycles, so the check is a function call, not an out-of-band reconciliation. Concretely (#164): the brain image declares its wire-protocol major in the `moose.protocol.major` OCI label, and host-agent reads it (`docker image inspect`) and compares against its own `protocol.Major` before `docker run` — a mismatch, or a missing label, is a refused launch with a clear error, not a first-request failure.
- The alternative — a separate `moose-brain.service` systemd unit — was rejected because it splits the update flow (host-agent already owns the brain+UI update stream per `UPDATES.md`) and moves the lockstep version check out-of-band into first-request failure.

### Locked: Caddy is moose substrate, runs as a container

- Caddy is **not a Tier-2 OS integration**. It needs no NET_ADMIN, no external auth flow, has no user-facing settings page; it's moose's own machinery, in the same bucket as the brain itself.
- Runs as a container, started by the brain alongside other moose-managed containers. Joins the moose Docker network and publishes host ports 80 and 443.
- Configured via Caddy's **admin API on `:2019`**, reached by the brain over the Docker network — no Caddyfile on disk, no `caddy reload` shell-out. Atomic reloads, no file I/O. The admin endpoint binds `0.0.0.0:2019` (not `localhost`): the brain is a *separate* container, so it can't reach Caddy's loopback — the listener must be on the container's network interface. The host never publishes 2019 (only 80/443 are exposed), so it's reachable only from inside the Docker network. **Network-isolation invariant:** Caddy's admin API has no auth — its only protection is that nothing untrusted shares its network. So **app containers must never be attached to the network that carries the admin port.** Caddy reaches app upstreams by joining each app's own per-app network (Caddy connects outward); the control-plane network (brain, Caddy, `moose-ui`, the socket proxy) stays trusted-only. An app on the same network as `:2019` could rewrite the entire route table.
- **The certificate store persists; the config does not.** Caddy's `/data` is a named Docker volume (`moose-caddy-data`, declared in the control-plane compose), so the ACME account key and any issued certificate survive a container recreate. Config is the opposite by design: the brain re-asserts every route and the hosted TLS policy through the admin API on its own startup, so `/config` (Caddy's autosave of the last admin config, never read back — the container loads `caddy.json` explicitly and does not `--resume`) is a `tmpfs`. The split matters most on hosted, where a lost `/data` means a **new** Let's Encrypt order rather than a renewal: new orders get no ARI rate-limit exemption, and every hosted box lives under one registered domain (`onmoose.network`) whose "50 new certificates per 7 days" budget the whole fleet shares (#433). On the appliance `/data` holds only Caddy's local CA and instance id — no ACME runs there yet — so persisting it is free rather than load-bearing.
- **Catch-all 404 invariant.** Caddy's `moose` server always has a final route at the end of `routes[]` with `@id=moose-catchall` and no matcher, returning HTML 404 ("No app at this hostname"). The brain inserts dynamic per-app routes at index 0 (`POST /config/apps/http/servers/moose/routes/0`) so the catch-all stays last. On startup the brain calls `EnsureCatchAll` which re-installs the catch-all if missing — survives Caddy state loss, hand-edits, or config drift. Returning 200 empty for unmatched routes is a UX failure and breaks tests that can't distinguish "routed" from "no-match".
- **Updates ride the brain+UI stream**, not the Debian base stream — image tag in the release manifest, pulled and reconciled by host-agent + brain. Decouples Caddy version from Debian's release cadence.
- **Performance is a non-issue for the household workload.** Docker bridge networking adds microseconds per connection — invisible at household scale. The heaviest "big file" path (SMB transfers of media to/from laptops) bypasses Caddy entirely: SMB is a Tier-2 host service on port 445. DLNA likewise. Caddy carries HTTP app traffic only (Jellyfin/Plex/Immich streaming, dashboards, app UIs); even a household of 4K streamers is well below containerized Caddy's ceiling.
- **Memory overhead from containerization is negligible.** Containers are not VMs — same process, same RSS, no extra kernel or libc.
- **Escape hatch if needed:** `--network=host` recovers host-level networking at the cost of internal-DNS service-name routing. We don't expect to need it.

### Locked: the dashboard UI is a brain-launched container

- The dashboard ships as its own `moose-ui` container (`caddy:alpine` + the built UI bundle baked in — full deploy model in `WEB_UI.md`). It is **launched by the brain**, alongside Caddy and the docker-socket-proxy, as part of bringing up the control-plane stack — *not* by host-agent. host-agent's chain of custody stops at the brain (# Locked: host-agent launches the brain container); the brain owns every container downstream of itself, the UI included.
- **Why the brain and not host-agent:** the UI version is bound to the brain's API version (the bundle declares the API minor it requires; `WEB_UI.md` # deploy model), and brain+UI update as one stream (`UPDATES.md`). Keeping both launches under the brain means the version pairing is enforced by the actor that already owns it, not split across the host-agent boundary.
- The LAN-facing Caddy routes `/api/v1/*` (and the SSE/log streams) to `moose-brain` and everything else to `moose-ui` (`WEB_UI.md` # deploy model). Both are reconciled by the brain on startup the same way app containers are.

### Locked: control-plane container hardening

Every **app** container gets the same sandbox with no opt-out — `cap_drop: ALL`, `no-new-privileges`, a pinned `user:` — written by the brain into its override file (`APP_LIFECYCLE.md` # Locked: override file contents, `APP_ISOLATION.md` # Capabilities & privilege). The control plane's own containers had none of it until #431, which is backwards: Caddy terminates TLS, holds the hosted wildcard's private key, and carries an unauthenticated admin API. What each container runs now, and why:

- **`caddy` and `moose-ui`** (declared in `dev/control-plane/compose.yml`): `cap_drop: ALL` plus `cap_add: [NET_BIND_SERVICE]` — both are a Caddy binding `:80` (Caddy also `:443`), which is the one capability either needs — `security_opt: [no-new-privileges:true]`, and `read_only: true`. A read-only root costs Caddy nothing since #433 gave both its write paths somewhere to go: `/data` is the `moose-caddy-data` volume and `/config` a `tmpfs` (# Locked: Caddy is moose substrate). The certificate store still works under it — Caddy writes its keys to the volume, not the image layer.
- **`moose-docker-proxy`** (launched by host-agent, `internal/hostagent/brainlaunch`): `cap_drop: ALL` + `no-new-privileges`. It needs no capability at all — haproxy binds `:2375`, above the privileged range. This is the container holding the raw Docker socket, so it is where a bug is worth the most. `read_only` is **deliberately not set**: the upstream image's entrypoint writes its generated config to `/tmp` and haproxy writes `/run` and `/var/lib/haproxy`, so a read-only root would mean three `tmpfs` mounts pinned to another project's internal paths, which break silently on an image bump. Capability dropping is the part that carries the value here.
- **`moose-brain`**: no capability sandbox. The brain `chown`s app data directories to the uid it elects for an app (`APP_ISOLATION.md` # Runtime identity & data ownership), so `CAP_CHOWN` is load-bearing and `cap_drop: ALL` would break an install. Hardening it means naming the capabilities it does need and proving that set on a booted box. Until then the residual is recorded in `THREAT_MODEL.md` B2.

A container's capability set is fixed when the container is **created**, so declaring the sandbox is not enough to put an existing box under it — a restart does not re-apply the flags. Compose handles that for `caddy` and `moose-ui` (it recreates a service whose config changed). Nothing recreates the proxy, so `EnsureTransport` reads the existing container's capability set and **recreates it once** if it predates the sandbox, before the brain is launched. If that read fails, the box keeps the container it has and boots: a hardening check must not be the thing that stops a box starting. The recreate takes the proxy away before it can put the new one back, and nothing retries it later in the same boot, so the relaunch is retried a few times — a daemon that is briefly busy must not cost the box its only path to Docker.

What this does not do: it does not make a Caddy or haproxy bug harmless. It lowers what one is worth.

### Locked: implementation specifics

- **Language:** Go. Single binary. Static.
- **Internal structure:** clean packages (app manager, proxy manager, mesh manager, service manager, storage manager, identity, API server, backup orchestrator). Single process — no microservices.
- **State:** SQLite, single file in a persistent volume, for moose's own state. *Managed Postgres is for apps; SQLite is for moose.*
- **Reverse proxy:** Caddy, controlled via its admin API.
- **`host-agent` is and stays minimal** — bootstrap, brain supervision, OS-level updates. Anything that changes frequently lives in the brain, not here.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in the relevant topical doc) plus `DECISIONS.md` if they flip a position.
