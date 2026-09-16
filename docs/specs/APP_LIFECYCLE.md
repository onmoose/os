# App lifecycle

> How the brain actually controls apps — install, run, update, uninstall — on top of Docker. Companion to `CONTROL_PLANE.md` (architecture context), `APP_MANIFEST.md` (the contract being installed), `APP_ISOLATION.md` (the runtime enforcement of permissions), `BRAIN_HOST_PROTOCOL.md` (the reconciler pattern's host-side surface), and `MOOSE_NETWORK.md` (Caddy route registration for `.onmoose.network`).

## Locked: an app instance is a Docker Compose project

The brain's unit of management is a **compose project**, not an individual container.

- Project name: `moose-<instance-id>`.
- Every container the brain manages carries labels: `moose.managed=true`, `moose.instance_id=<id>`, `moose.manifest_id=<id>`.
- `main_service` (from the manifest) is the service the brain routes to and watches for health. Other services in the compose are siblings managed by the same lifecycle ops.
- Tier-3 per-user apps are N independent compose projects pointing at the same manifest+compose. Each has its own instance id, data dir, and slug.
- **Slug derivation (locked, see `DASHBOARD.md` # instance naming):** the bare manifest slug `<slug>` is first-come for any scope — the first instance installed wins the clean name. On a collision, a personal instance appends the owner (`<slug>--<user>`, double-dash separator); a household instance (no owner to name) gets a numeric suffix (`<slug>-2`, `<slug>-3`). Flat and single-label so it fits the one wildcard cert `*.<box-id>.onmoose.network` and resolves cleanly over mDNS. Slugs and usernames may not contain `--` or produce an `xn--` label prefix.

Door-1 (store) and Door-2 (custom compose) converge here: both produce a manifest+compose pair that the brain installs identically. The Door-2 path just synthesizes the manifest first.

## Locked: Docker driver is the `docker compose` CLI

Brain shells out to `docker compose` for lifecycle ops (`up`, `down`, `pull`, `stop`, `start`, `config`). The `compose` plugin binary ships in the brain image.

The Docker HTTP API (via the socket proxy) is used directly only for things the CLI handles poorly:

- `/events` stream for crash detection and health changes.
- Log streaming to the UI WebSocket.
- Image GC.
- Network creation (`moose-ingress` at brain startup, per-app `moose-app-<id>` on install).

**Why CLI for v1:** fastest path to a working system. Compose semantics stay upstream's problem; we don't reimplement them. The dev-loop and behavior match what app authors run locally with `docker compose up`. We can swap in the SDK later for hot paths if profiling demands it.

## Locked: on-disk layout per instance

```
/var/lib/moose/state/instances/<instance-id>/
├── manifest.yml              # author's, verbatim
├── compose.yml               # author's, verbatim
├── compose.override.yml      # moose-generated, regenerated on update
├── .env                      # moose-injected variables
├── data/                     # bind-mount root for all app data
│   └── ...                   # subdirs per the compose's bind mounts
└── snapshots/                # pre-update tar(s) of data_volumes + managed-service DB dumps
    └── pre-update-<old-version>.tar  # see UPDATES.md # Pre-update snapshot
```

The brain's install command is literally:

```
docker compose \
  -f compose.yml -f compose.override.yml \
  --env-file .env \
  -p moose-<instance-id> \
  up -d
```

All app data lives under `data/` via bind mounts. **No Docker named volumes for app data.** Authors write `${MOOSE_DATA_DIR}/pgdata:/var/lib/postgresql/data` (or the equivalent relative path). One backup root, one disk-usage view, one mental model.

## Locked: reconciliation is imperative, with a startup pass

The brain does **not** run a k8s-style reconciler loop. Each user action is a sequenced set of idempotent ops with explicit rollback. SQLite is the desired-state source of truth.

On brain startup, a reconciliation pass walks SQLite, lists containers with `moose.managed=true`, and fixes drift:
- `state=running` but no containers: `docker compose up -d`.
- `state=running` with containers up but the instance is flagged *pending-recreate* (a config/mail edit committed the override/.env, then its `compose up` failed): `docker compose up -d` to apply the committed env, then clear the flag (#268). The brain sets the flag at the failed edit (brain-commits-first), so a container left running on stale env converges on the next startup pass instead of waiting for the user to retry.
- `state=stopped` but containers running: `docker compose stop`.
- Orphan containers (labeled but no SQLite row, e.g. crash mid-install): tear them down.

**Why imperative:** single-node appliance, one user clicking at a time. A reconciler is overkill. The startup pass plus per-step rollback covers every realistic failure mode.

## Locked: same reconciler pattern extends to all host-managed state

The startup-pass + idempotent-ops pattern above applies to apps (Docker containers). It applies **identically** to everything the brain manages on the host via `host-agent`:

- Tier-2 services (Tailscale enable/disable + sign-in state, Samba enable + share config, DLNA on/off).
- mDNS publications.
- Per-user SSH-access state and `authorized_keys`.
- Network configuration (DHCP vs. static IP, primary interface).

Each of these has a row (or rows) in SQLite expressing **desired state**. host-agent exposes `GET /v1/state/summary` returning **actual state** for all the same things. Brain reconciles at three triggers:

1. **On brain startup** — full reconcile pass before serving requests (same as the app pass above).
2. **On a 60-second heartbeat** — brain polls the summary, diffs against desired, surfaces or fixes mismatches.
3. **After every state-changing op** — verify the change took effect; if not, the op is `failed`, not `completed`.

**Drift policy — two cases, two responses:**

| Disagreement                                                              | Response                                                                                       |
|---------------------------------------------------------------------------|------------------------------------------------------------------------------------------------|
| Desired says X, actual says Y, **brain made the last change** (crash mid-step) | **Auto-reconcile.** Re-apply desired. Handles "brain crashed halfway through enrolling Tailscale" cleanly. |
| Desired says X, actual says Y, **something else changed state** (drift)   | **Surface, don't auto-fix.** Settings shows "SMB is disabled — your settings say it should be on. [Fix] [Update settings]" |

How brain distinguishes: every state-changing op records "I am about to apply this change" before issuing it. If reconciliation sees a mismatch where the last-recorded intent matches desired, the user (or another process) drifted it. If it matches neither desired nor actual, an in-flight op was interrupted — re-apply.

**Dangerous ops are excluded from auto-reconcile.** A `mkfs` or `apt dist-upgrade` interrupted halfway is not safe to re-run; the UI surfaces "an operation was interrupted while doing X — verify state before retrying." The `Dangerous` flag comes from the job-kind declaration (`BRAIN_HOST_PROTOCOL.md` # Failure semantics).

**Why same pattern for everything:** the discipline cost of "model desired state, make ops idempotent" is paid once per state-managing surface. Two reconciliation models would double the surface area for bugs without adding power.

## Locked: per-instance state machine

```
absent → installing → running ⇄ stopped
              ↓          ↓
            failed     updating → running
                          ↓
                       update_failed
                          ↓
                          → running (after rollback)
```

`failed` and `update_failed` are user-visible recoverable states with a "retry" / "view logs" / "uninstall" action — not dead ends. **`failed → running` is a retry**: the same Start op (# stop, start, uninstall), reachable straight from the dashboard tile (`DASHBOARD.md` # Tile, issue #154), with no uninstall/reinstall or SQLite surgery; `update_failed` recovers via the rollback path below. The state is written to SQLite *before* the corresponding Docker op begins, so a crash mid-op leaves a known-recoverable state.

## Locked: override file contents

The brain-generated `compose.override.yml` applies isolation and appliance behavior. **Locked contents, for every service in the compose:**

- Networks: attach to `moose-app-<instance-id>` (per-app bridge). `main_service` additionally attaches to `moose-ingress` so Caddy can reach it.
- `container_name: moose-<instance-id>-<main-service>` pinned on `main_service` **only**. Without the pin compose names the running container with a replica suffix (`…-1`), and Docker's journald driver tags log lines with that suffixed name — breaking the per-app Logs tail's exact `CONTAINER_NAME` match (`LOGGING.md` # Per-app logs) while the network alias kept Caddy working. The pin makes the running container's name the same `moose-<id>-<service>` stem the brain already computes for the Caddy upstream and ingress alias. An explicit `container_name` makes the service unscalable; the main service is single-replica by design, and sidecars stay unpinned so the constraint never lands on an author's scalable workers. Same pattern as the managed services' fixed exec handle.
- `cap_drop: [ALL]`. No `cap_add` for Tier-3 apps, ever.
- `security_opt: [no-new-privileges:true]`.
- `restart: unless-stopped` (forced — this is a home appliance, apps must survive reboots), **except for author-declared terminating jobs**, whose `restart:` is preserved verbatim. A "terminating job" is detected from the union of two signals: (a) the author set `restart: "no"` or `restart: "on-failure"` on the service, or (b) the service is the target of another service's `depends_on: {condition: service_completed_successfully}` — which catches the common case where the author omitted `restart:` entirely (Compose's default is `no`) and signal (a) alone would miss. Forcing `unless-stopped` on a one-shot init/migrate/seed job is catastrophic: the job exits 0, Docker restarts it, it never reaches the "completed" terminal state, and a completion-gate `depends_on` blocks `docker compose up -d` forever (`DECISIONS.md` 2026-06-05). **`main_service` is always forced long-running** regardless of these signals — a paranoid or buggy author can't accidentally exempt the actual app. Every service not detected as a job still gets forced `unless-stopped`.
- `devices:` mirroring `permissions.devices` from the manifest. Empty if not declared.
- The `moose-app-<id>` network is created with `internal: true` if `permissions.internet: false`.

The override **does not** touch images, env, command, entrypoint, healthcheck, ports, depends_on. It is purely additive-isolation, not a rewrite.

## Locked: admission policy

Before install, brain runs `docker compose config` against the author's compose and rejects on any of:

- `ports:` host bindings on any service. (Routing is via Caddy on internal networks; nothing binds to the host.)
- `privileged: true` on any service. (Tier-3 only — Tier-2 OS-integration apps use a separate install path.)
- `cap_add` on any service.
- `network_mode: host | container:* | none`.
- `pid: host`, `ipc: host`, `userns_mode: host`.
- `volumes:` with absolute host paths. Only bind mounts under the instance's `data/` dir are allowed.
- `build:` — apps ship images, not Dockerfiles.
- `extends:` referencing files outside the manifest package.
- `deploy.replicas` greater than 1 on any service. moose is a single-node appliance — a second replica buys no availability, Caddy routes to one upstream alias per instance, and the main service's `container_name` is pinned for the per-app Logs tail (# Locked: override file contents), which compose refuses to scale. Rejecting at admission names the field instead of failing opaquely at `up`.

Rejection messages name the exact field that failed. Catalog CI runs the same checks before publish.

**The policy is door-symmetric** — it runs identically for Door-1 (catalog) and Door-2 (custom paste). Door 2 differs only in its *manifest* (permissive defaults, synthesized, TOFU-pinned image), never in its *sandbox*; the host-rooting primitives above are refused for both, because on a multi-user box a container escape hits every member, not just the installer (`APP_ISOLATION.md` # Trust tiers, `DECISIONS.md` 2026-06-02). **Door 2 is admin-only**: `POST /api/v1/apps/custom` requires admin (members install store apps, personal scope only). A future door-asymmetric relaxation is parked in `NEXT.md`.

## Locked: env-var injection

Brain writes a `.env` file in the instance dir; compose auto-loads it. MVP variable set:

- `MOOSE_INSTANCE_ID` — opaque instance identifier.
- `MOOSE_APP_URL` — the routable URL for this instance (e.g. `http://photos.local`).
- `MOOSE_DATA_DIR` — absolute path to the instance's `data/` dir, for compose to reference in bind mounts.

The app's compose maps these to whatever variable names the app expects. No auto-rewrite.

Managed-service variables (`MOOSE_SERVICE_*`) and per-instance secrets (`MOOSE_SECRET_*`) are deferred — see `SERVICE_PROVISIONING.md`.

## Locked: image digest pinning

All app installs end up running against a `compose.override.yml` that pins every image as `image: name@sha256:...`. From the second `up` onward, the compose is byte-deterministic.

**For store (Door-1) apps, the digest comes from the published catalog** (`APP_STORE.md` # Trust model — catalog's `images` map). The promised digest is the *address the brain pulls*: it fetches `name@sha256:…` and never consults the tag. That is the binding from "the moose store promised this version" to specific bytes — a content-addressed pull cannot return anything else, so there is nothing to compare afterwards.

The tag in the manifest is a human-readable label, not a lookup key. This matters because tags are mutable in practice, not just in theory: a registry rebuilds the same version tag whenever the image's base is patched, so `nextcloud:34.0.1-apache` can name different bytes on different days. Pulling the tag would make the bytes a box runs depend on *when* it installed, and would turn every routine upstream rebuild into a failed install. Pulling the promised digest makes an upstream rebuild a non-event, and makes two boxes installing the same catalog version byte-identical. Whether a promise should be updated to newer upstream bytes is a curation decision, made where the catalog is built — never by the box, and never implicitly by the clock.

The one hard failure left on this path is the catalog contradicting *itself*: a compose pinned to `name@sha256:…` whose digest disagrees with the promise in the `images` map. That is a curation bug with no safe pick between the two, so the install fails and rolls back.

**For custom (Door-2) apps, the brain falls back to TOFU**: pull, resolve the digest via `docker inspect`, write it into the override. No external authority to compare against; the user pasted the compose themselves.

Updates re-resolve (catalog for Door-1, fresh inspect for Door-2). The previous digest is kept in SQLite to power one-generation rollback.

**Offline (air-gapped) installs trust the catalog promise directly.** A baked box with no registry — the first-boot bootstrap, and the air-gapped QEMU full-stack lane (`TESTING.md` # Full-stack control-plane integration) — `docker load`s every image from the offline bundle and cannot `docker pull`. In this mode (the brain's `MOOSE_OFFLINE_INSTALL`), a pull failure is not fatal: if the image is already present locally, the **catalog-promised digest is the pin** — the offline bundle stands in for the registry as the trust anchor (a `docker save`/`load` image carries no `RepoDigest`, so the normal inspect-against-registry path can't resolve one). Two cases still hard-fail, to keep the air-gap honest: an image that is genuinely absent (the bundle is incomplete — the missing-image hard-fail the lane exists to catch), and a Door-2 install (no catalog promise to trust offline). Off by default: a box with a reachable registry pulls the promised digest from it as usual.

## Locked: install transaction

```
1.  Parse + validate manifest                 (no state)
2.  Parse + admit compose                     (no state)
3.  Allocate slug, write SQLite row           (state: installing)
4.  Create instance dir tree
5.  Generate override + .env
6.  Pull images, resolve digests, rewrite override
7.  Create per-app network
8.  Register Caddy route (splash) + publish mDNS
9.  docker compose up -d                      (bounded by the health-wait budget)
10. Wait for main_service healthy             (default 120s; manifest may override)
11. Flip Caddy upstream from splash to main_service
12. Mark state: running
```

Failure handling:
- **Steps 1–2:** clean fail, no state written.
- **Steps 3–9:** full rollback — unpublish mDNS, drop Caddy route, `compose down -v`, drop network, remove instance dir, delete SQLite row. Step 9's `compose up -d` runs under a context bounded by the health-wait budget (the same default 120s as step 10), so a pathological app whose completion gate never completes **fails the install cleanly** instead of wedging the brain indefinitely — a containment backstop independent of the terminating-job detection above.
- **Steps 10–11:** keep the instance dir (so the user can inspect logs). Caddy route stays registered but in "failed" splash mode. State: `failed`. The UI surfaces the failing step and last 50 lines of logs.

## Locked: update + rollback

```
1. Fetch new package (new manifest + compose)
2. Re-run admission on new compose
3. Save current override → compose.override.yml.prev; current digest → SQLite
4. Snapshot data_volumes + managed-service DB → snapshots/pre-update-<old-version>.tar
   (see UPDATES.md # Pre-update snapshot)
5. Generate new override
6. docker compose pull
7. docker compose up -d                       (recreates changed services)
8. Wait healthy
9. Mark state: running
```

If step 7 or 8 fails: stop new container, **restore the snapshot from step 4** (untar data_volumes; restore managed-service DB from dump), swap `.prev` override back, `up -d` again with the pinned previous digest, mark `update_failed`. One generation of rollback history — sufficient for the one-step-back UX, no n-deep history in v1. Snapshot + previous image are GC'd after 7 days.

## Locked: stop, start, uninstall

- **Stop:** `docker compose -p moose-<id> stop` (never `down` — containers, per-app network, Caddy route, and mDNS name all stay in place; only the running processes halt, freeing CPU/RAM). The Caddy route flips to the moose-styled "this app is stopped" splash. State: `stopped`. Legal only from `running`; any other state is a 409. Note this frees the *app's* footprint only — a shared Tier-1 managed service (Postgres/MySQL) the app uses stays running, since it's brain-owned and shared (`SERVICE_PROVISIONING.md`).
- **Start:** `docker compose -p moose-<id> up -d`, **not** `compose start` (`DECISIONS.md` 2026-06-10). `start` only restarts the existing stopped containers — it re-runs already-completed one-shot jobs and ignores `depends_on` ordering; `up -d` is the same op the reconcile pass uses, so dependency ordering and completion-gate jobs (# override file contents) behave exactly as on install, and it's idempotent. State is written `running` **before** the docker op (brain-commits-first), so a crash mid-start leaves a `running` row the reconcile pass finishes — the same recovery path a reboot takes. Start also **re-asserts the mDNS name, not just the Caddy route** (the same idempotent `host.Publish` install and the reconcile pass use), so an app recovered via Start after its `<slug>.local` went dark — a mid-life host-agent restart that dropped the process-local Avahi entry group, or a prior install that failed before publishing — resolves by name again without waiting for a brain reboot; the published name keys the starting splash, the real upstream, and the failed splash alike. Stop, by contrast, never re-publishes — the stopped splash keeps resolving on the install-time announcement, which is intended. The route flips to the "starting" splash, then to the real upstream once `main_service` is healthy; a start that comes up but never goes healthy lands in `failed` with the "failed" splash, exactly like an install health-timeout. Legal from `stopped` (start) **or `failed`** (the click-to-retry recovery, `DASHBOARD.md` # Tile, issue #154); any other state is a 409. Retrying a failed app is *exactly* a Start — same `compose up -d`, health-wait, splash→upstream flip, and mDNS re-publish — so it lands in `running` on success or back in `failed` (via the same failed-splash path) if it still won't come healthy, giving a `failed` instance a UI recovery path that needs neither uninstall/reinstall nor hand-editing SQLite.
- **Uninstall:** confirm dialog with a "keep data" checkbox (default: delete).
  - **Delete:** `compose down -v`, remove route + mDNS, `rm -rf` instance dir, then **reclaim images** — `docker rmi` each `repo@sha256:…` the instance ran, skipping any image another installed instance still references (cross-checked against the `instance_images` table *after* the SQLite row is deleted, so "still referenced" is just every remaining row). Best-effort: an image docker refuses to drop (held by another tag or a stopped container) is logged, never fatal — images stay pinned-and-tagged, so a plain `docker image prune` would not reclaim them, which is why the targeted `rmi`-by-digest is needed. Scope is the *uninstall* case only; a recurring sweep for **update**-orphaned images (the previous image kept 7 days under # update + rollback, `UPDATES.md`) needs a scheduler seam the brain lacks and stays deferred (`NEXT.md` # Container image cleanup).
  - **Keep:** same, but move instance dir to `/var/lib/moose/archive/<id>-<timestamp>/` first. SQLite retains a tombstone row. Re-import path is a follow-up.

## Locked: crash detection

Docker's own `restart: unless-stopped` (forced by the override) handles transient crashes — the brain does not intervene.

The brain subscribes to Docker's `/events` stream filtered by `moose.managed=true`. Container die / health_status / restart events update the per-instance health view. If `main_service` dies more than 5 times in 60 seconds, the brain marks the instance `unhealthy` in the UI (state stays `running` — Docker will keep trying — but the dashboard shows a "needs attention" badge with a logs link).

## Locked: reboot behavior

Host reboots → Docker starts → containers with the forced `restart: unless-stopped` come back → brain starts → reconciliation pass corrects any drift. Apps that were `running` before reboot are `running` after; apps that were `stopped` get stopped again by the reconciliation pass if Docker brought them up.

## Locked: slug allocation

The manifest lists `preferred_slugs` in priority order. The brain picks the first one that's free, where "free" means: not used by another installed instance, and not in the reserved list.

Reserved (cannot be used by apps): `api`, `admin`, `dashboard`, `moose`, `host`, `setup`, plus any name the brain itself serves.

If every preferred slug is taken (rare — two apps both wanting `photos`), append a numeric suffix: `photos-2`, `photos-3`, … The chosen slug is persisted in SQLite and never changes for the lifetime of the instance.

Per-user disambiguation (when `multi_user: per_user` apps ship) is deferred until that feature is built.

## Locked: mDNS ownership — host-agent owns Avahi

Avahi runs on the host as a normal Debian service (`avahi-daemon`). It needs host networking for multicast on UDP 5353; running it in a container is awkward.

The `host-agent` owns the Avahi integration and exposes a narrow `publish/unpublish hostname` RPC over the host-agent ↔ brain channel. The brain calls this RPC during install (publish) and uninstall (unpublish). The brain never talks to Avahi directly.

**Mechanism:** `publish` calls Avahi's DBus API (`EntryGroupNew` → `AddAddress` → `Commit`) to announce an A record for `<slug>.local` pointing at the box's LAN IP; on a name collision it retries once with the box-qualified `<slug>-<box>.local` and returns whichever name won. `unpublish` frees the entry group. (An earlier static-service-file approach was abandoned — Avahi static files announce *services*, not bare A-record aliases; see `DISCOVERY.md` # Per-app A records and `DECISIONS.md` 2026-05-24.) The brain uses the **returned** name for both the Caddy route and the displayed URL, so the two never disagree. The full mechanism (record types, interface scoping, conflict handling, single-label rationale) lives in `DISCOVERY.md`.

**Readiness gate:** `publish` is a Pattern-B job (`BRAIN_HOST_PROTOCOL.md`) — host-agent writes the file, then waits on Avahi's DBus for `EntryGroup.StateChanged → ESTABLISHED` (typically <1s, but a real propagation step) before returning success. The brain does not mark the app `ready` until publish completes; an unannounced name behind a live Caddy route is a confusing "browser timeout" failure mode we structurally prevent.

**Why:** matches the layering principle — host-agent owns host resources, brain owns app logic. The alternative (bind-mounting Avahi's D-Bus socket into the brain container) would expand the brain's host attack surface for no real benefit. The brain has the most code and the broadest exposure; keeping it sandboxed from host services is worth a small RPC.

Side benefit: host-agent already needs a brain-facing channel for OS updates and storage management. Avahi publish/unpublish is one more method on the same interface, not a new dependency.

## Locked: Caddy route registration timing — register early, with a splash

When the install transaction reaches step 8, the brain registers the Caddy route immediately, pointing at a moose-served splash page ("Photos is starting up…", auto-refreshing). Once `main_service` is healthy, the brain flips the Caddy upstream from the splash to the real container (step 11).

**Why:** registering only after the container is healthy leaves `photos.local` returning *connection refused* (or NXDOMAIN, since mDNS isn't published yet) for up to 120 seconds after the user clicks install. That looks broken; the splash looks intentional.

The same splash machinery serves three user-visible states with consistent vocabulary:

- **Starting** — during install or first start.
- **Stopped** — for manually stopped apps.
- **Failed** — install or update failure, with a "view logs" link.

Mechanically: the brain owns two route variants in Caddy's config per instance and swaps between them on state transitions. mDNS publish happens at the same moment as the splash registration — both make the hostname reachable.

If the box is enrolled with onmoose.network, the brain registers **two hostnames** per app (a `.local` HTTP route and a `<slug>.<box-id>.onmoose.network` HTTPS route). Both go through the same splash → real-upstream flip. Dashboard tile-clicks default to the `.local` URL; apps with `requires_https: true` in the manifest open the `.onmoose.network` URL instead. See `MOOSE_NETWORK.md` for why `.local` is the canonical user-facing URL.

## Locked: concurrency

- **Per-instance mutex:** one lifecycle operation at a time per instance.
- **Global semaphore:** at most 3 concurrent installs or updates, to bound system load on small boxes.
- **Image pulls:** deduplicated by the Docker daemon itself; the brain doesn't coordinate.

## Deferred: lifecycle hooks

The manifest's `hooks:` block (`post_install`, `pre_update`, `pre_backup`, etc.) is **not in MVP.** Apps run their own migrations on container start; the **pre-update snapshot** (`UPDATES.md` # Pre-update snapshot) is the v1 safety net for app-driven schema migrations that go wrong.

When hooks return, they will be designed as **one-shot container images**, not in-container scripts. The manifest will reference a hook image (e.g. `pre_update: { image: photoprism/migrator:2.4.1 }`); the brain runs it as a transient container with the app's volumes attached. This respects closed-source images natively and removes the "main_service needs a shell" constraint.

The author-provided `pre_update` hook, when it returns, *replaces* the brute-force tar for apps that ship one. The brain's snapshot stays the safety net for apps that don't. A `post_update_rollback` hook fired only when `post_update` fails is sketched in `APP_MANIFEST.md` # F for apps that need bespoke recovery — also deferred.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md).
