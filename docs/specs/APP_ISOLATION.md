# moose App Isolation

> Working spec for how the brain enforces what apps can do at runtime. Companion to `SPEC.md`, `CONTROL_PLANE.md`, `APP_MANIFEST.md`, `SERVICE_PROVISIONING.md`, `STORAGE.md`, `FIRST_RUN.md`.

The manifest declares *intent* (`internet: true`, `lan: false`, `folders: [{folder: photos}]`). This document describes what those declarations mean concretely — what Linux/Docker primitives back them, what the defaults are, and where the trust boundaries sit.

## Principles

- **The user owns the box.** Defaults are conservative, and an admin who genuinely needs an unsandboxable container has SSH (`AUTH.md` # SSH is rescue) — `docker run` over SSH is the escape hatch. We don't lock the user out of their own machine; we just don't put host-rooting one paste away in the UI.
- **Store apps carry a meaningful trust claim.** Things that can root the host are not allowed in the catalog. **Door-2 custom compose runs under the identical sandbox** — host-rooting primitives are refused for *both* doors. The reason is multi-user: moose's threat model names "a compromised app" as a top adversary, and a Door-2 app that roots the host doesn't just affect the (admin) installer — it exposes every other household member's data, none of whom consented. "The user owns the consequences" holds for a single-user box; on a multi-user box the consequences land on other principals, so the bright lines stay up in the UI path. (See `DECISIONS.md` 2026-06-02.)
- **Manifest declares intent. Brain enforces it.** Apps that lie about their permissions either fail (silently blocked at the kernel/Docker layer) or get pulled from the catalog.
- **Same enforcement everywhere.** Store apps and custom apps share the same runtime. Only the *defaults* and *catalog rules* differ.

## Trust tiers

| | Store apps (Door 1) | Custom apps (Door 2) |
|---|---|---|
| Who can install | Admin or member (members: personal scope only) | **Admin only** |
| Manifest source | Catalog repo, reviewed | User-supplied, synthesized from raw compose |
| Default permissions | Least-permissive (must declare what they need) | Permissive (`internet: true` on by default) |
| Image binding | Digest from the published catalog | TOFU — pull, pin the resolved digest |
| Forbidden primitives | `privileged`, docker socket, host ports, host bind mounts, `cap_add`, host namespaces | **Identical to store** — same admission policy runs for both |
| Enforcement mechanism | Identical to custom | Identical to store |

**What actually differs between the doors is the *manifest*, not the *sandbox*.** Door 2 is "you wrote the manifest instead of us, behind the same safety rails" — permissive defaults, a synthesized manifest, and TOFU digest pinning. The runtime envelope (`cap_drop: [ALL]`, no host access, Caddy-routed, runs as a UID) is byte-for-byte the same. An admin who needs a container that can't fit those rails (Portainer/Watchtower → docker socket; Tailscale/WireGuard → `NET_ADMIN` + host net) runs it over SSH, or it becomes a curated **Tier-2** OS integration (`SERVICE_PROVISIONING.md`). Relaxing the door asymmetrically is a future option, not v1 (`DECISIONS.md` 2026-06-02).

---

## Multi-user runtime

moose is multi-user (`FIRST_RUN.md`). Every user has private data; admins manage the box but cannot read other users' files through normal paths. App execution reflects this: **every app instance has an owner**, and a personal instance is scoped to its owner's data (`DASHBOARD.md` # instances are owner-scoped, `DECISIONS.md` 2026-05-29).

### Owner-scoped instances

Whether an instance is **household** (admin-owned, shared — one instance, the app's own internal multi-user separates people inside it) or **personal** (owned by one user, its own data, folders, and route) is **elected by the installing user**, not derived from a tier or declared in the manifest (`APP_MANIFEST.md` # G). Admins choose household or personal; members install personal only.

A personal instance is the per-owner compose-project shape locked in `APP_LIFECYCLE.md` # an app instance is a Docker Compose project: an independent project named `moose-<instance-id>`, with its own instance id, data dir, and slug (bare `<slug>` if it wins first-come, `<slug>--<user>` on collision — `DASHBOARD.md` # instance naming). If two users each install the same app personally, two independent instances run.

- A personal instance's main process runs as the **owner's Linux UID/GID** (assigned at user creation, in the moose-reserved 3000+ range). The brain enforces this via the compose `user:` field; we do not rely on `PUID`/`PGID` env-var conventions. (A household instance runs as a shared service identity, not a single member's UID.)
- The container sees only the bind-mounted use-case folders declared in the manifest (the owner's `/home/<user>/Photos/`, `Documents/`, etc., or a household `/srv/moose/shared/<Folder>/` when the installer elected the shared source — see `APP_MANIFEST.md` # `folders`), mounted at the fixed `/moose/<folder>` paths. Other users' homes are not bind-mounted; they're not even on the filesystem the container can reach.
- App authors write a single-user app. They do not need to know about users, sessions, or identity — moose runs one instance per owner.

### Tier-2 apps (always shared)

Tier-2 apps (Tailscale, SMB, DLNA, future entries — `SERVICE_PROVISIONING.md`) are inherently box-wide. One instance per box, admin-installed. They are not owner-scoped. Tier-2 ≠ Tier-3 from a runtime-isolation standpoint and most of the rules in this document apply differently. See `SERVICE_PROVISIONING.md` for their model.

### No `multi_user` field

Household-vs-personal is an install-time election, not a static property of the app, so **no manifest field expresses it** (an earlier draft's `multi_user.mode` was removed — `DECISIONS.md` 2026-05-29). Cross-user *awareness* inside a single instance (e.g., a future moose-built Photos app with its own sharing UI) is a separate, deferred concern, not a v1 manifest field.

### Routing per instance

Each instance has its own subdomain: `<slug>.local` for whichever instance won first-come; `<slug>--<user>.local` for a personal instance that collided with an existing bare name; `<slug>-2.local` for a household collision (`DASHBOARD.md` # instance naming). One wildcard cert covers every instance regardless of suffix. Scope and ownership are surfaced in the dashboard, not inferred from the hostname.

Consequence: a personal instance's URL is owner-specific — `immich--alex.local` is Alex's. Granular cross-user sharing (intentionally exposing one user's content to another) is a future feature; v1 sharing is "drop it in `~/Shared/`" only (`STORAGE.md`).

### User lifecycle

- **Login:** brain loads the user's session, ensures their per-user containers are running for the apps they have installed.
- **Logout:** containers stay running. Lifecycle is decoupled from session activity, so background work (sync, backups, scheduled jobs) keeps working.
- **User deletion:** admin is prompted. Default action is to **archive** the user's data — rename `/home/<slug>/` to `/home/.archived/<slug>-<date>/` (atomic rename within the same filesystem) and stop their app instances, not to delete. Admin can choose to delete instead. The archive can be cleaned up later from Settings.

### Privacy ceiling at v1

Per-user data lives at `/home/<user>/` with `0750` perms owned by the user (`STORAGE.md` # Permissions). Other moose users cannot read it through the filesystem. **The admin (or anyone with shell as root) can read everything**, because v1 only encrypts at the disk level (LUKS), not per user. Admin-resistant per-user encryption (fscrypt) is on the roadmap; see `STORAGE.md` "Future: per-user encryption" for the planned upgrade. v1 features that touch user data are designed as if that upgrade were already in place — backup is per-user-keyed, etc. — so the upgrade is data-only, not feature-redesign.

---

## Network model

### Per-app network

Every app gets its own Docker bridge network. Inter-container DNS works inside it (the app's own compose services resolve each other by name). Inter-*app* traffic is denied by default — apps live on separate networks.

The brain reaches the app's web port over this network for reverse-proxy routing. Apps **do not bind to host ports** — for *both* doors; the brain owns 80/443 for the subdomain proxy + TLS termination. Manifest declares `web.port: 8080` and the brain wires `myapp.local → container:8080`. A `ports:` host binding is an admission rejection regardless of door (`APP_LIFECYCLE.md` # admission policy).

### `internet: true` / `false`

- `true` (default for custom, opt-in for store): per-app bridge has NAT to the host's default route. Outbound works, inbound is blocked except via the reverse proxy.
- `false`: per-app bridge is created with `internal: true`. No NAT, no external route — kernel-level block, not iptables rules we maintain. External DNS fails. Internal DNS for inter-container resolution still works.

Applies to IPv4 and IPv6.

### `lan: true`

The container gets a **macvlan attachment** to the host's primary LAN interface in addition to its per-app bridge. From the LAN's perspective the container is its own device with its own IP and MAC.

This is the only mechanism that preserves the use cases that justify `lan: true`:

- mDNS / SSDP / Bonjour discovery (Home Assistant, Scrypted, Frigate, Chromecast, AirPlay, DLNA renderers)
- Direct unicast to LAN devices (smart bulbs, IP cameras, network printers)
- Pi-hole / AdGuard serving DNS to other LAN devices

Cost: each `lan: true` app burns one IP on the user's LAN. Tolerable — single-digit count of such apps on a typical box.

**Wi-Fi caveat.** Some Wi-Fi drivers reject macvlan. On Wi-Fi-only setups the brain falls back to bridge + LAN route, with a documented limitation: unicast to LAN devices works, multicast discovery does not. The first-run network wizard captures the primary LAN interface; if it's wireless, the user is warned at install time for `lan: true` apps.

### Inter-app traffic

Denied. Apps that need to share data go through:
- Managed services (`services: [postgres]`)
- A shared use-case folder (`folders: [...]` — two of the same user's apps pointed at the same source see the same `~/Photos/`, or the same `/srv/moose/shared/` tree)

A user who genuinely wants two apps wired directly together puts them in one Door-2 compose. Cross-app networking is not a v1 feature.

---

## Filesystem & devices

### Root filesystem

Writable by default (Docker default). Most existing images assume `/var/log`, `/var/cache`, `/run` are writable; enforcing read-only would break ~30% of catalog candidates and burn support cycles.

Opt-in hardening: `security.read_only_root: true` in the manifest enables read-only root + tmpfs for declared scratch paths. Recommended for security-sensitive apps; not the default.

The writable layer resets on container *recreation* (image update, uninstall/reinstall) but persists across restarts.

### Volumes

App state (indexes, configs, the app's own DB) lives under the instance dir at `/var/lib/moose/state/instances/<id>/data/`, via bind mounts only — no Docker named volumes (`APP_LIFECYCLE.md` # on-disk layout per instance). The author writes the bind mount against `${MOOSE_DATA_DIR}/foo:/foo` (or the relative `./data/foo:/foo`); the brain injects `MOOSE_DATA_DIR`, so authors reference a stable variable rather than a hardcoded host path.

`/tmp` is a size-capped tmpfs.

**Bind mounts to arbitrary host paths are forbidden for both doors.** Only relative bind mounts under the instance's `data/` dir are allowed; an absolute host source is an admission rejection (store or custom alike).

**Every instance runs as a resolved `user:`, and every private bind dir it declares is owned by that same UID.** A Tier-3 container has `cap_drop: [ALL]`, which removes `CAP_DAC_OVERRIDE` — so even a root-UID container can only write a bind dir when it is that dir's actual owner. The brain therefore pins `user:` on every instance and, before `compose up`, creates **and** chowns *every* declared relative (`./…`) bind source — not just the top-level `data/`, but each `./data/media`, `./config`, … the compose declares — to match. This is load-bearing: the docker daemon creates a missing bind source as **root:root** regardless of its parent's owner, so any private bind dir the brain doesn't pre-create is unwritable to the non-root runtime UID and crash-loops the app (the failure mode #147 fixed). Identity by app shape: a **folder app** runs as the owner's UID (personal) or the moose-app identity (household); a **folderless app** runs as the brain's own effective UID/GID, which is the creator and owner of the freshly-made bind dirs (root under the production brain, the operator's user under the native dev brain), or — when it declares `service_user: true` — a dedicated allocated non-root identity. Beyond the relative private bind dirs, the brain also prepares each elected **personal** use-case folder source (an absolute bind the override injects, e.g. a `pick-subfolder` subdir like `~/Documents/Notebooks`): it `MkdirAll`s + chowns it to the owner UID before `compose up`, for the same reason — an unprepared source would be docker-created root-owned and unwritable. This is safe because a personal source's runtime identity *is* the owner, so a pre-existing `~/Documents` chown is a no-op and only the new leaf is created. **Shared** sources (`/srv/moose/shared/…`) are prepared by a *different* rule, because that tree is group-owned, not UID-owned: the brain creates the elected `<Folder>[/<subfolder>]` beneath the shared root, owning each **newly created** level `root:moose-shared`, mode `02770` (setgid, so descendants inherit the group; `STORAGE.md` # user content) — it **never** chowns a shared dir to a runtime UID and **never** re-owns a pre-existing parent (a shared dir belongs to the storage setup, not to one install). The household `moose-app` container reaches it through its `moose-shared` `group_add`, so no per-UID ownership is needed. Because writing under `/srv/moose` needs root, this preparation runs **only under the production brain** (euid 0); the unprivileged native dev brain can't create the shared tree, so household shared-folder apps are out of the inner loop (#156 — a dev-rooted shared base via the host seam, mirroring the dev-rooted personal home, is the follow-up if dev parity is wanted). The full source-of-identity table and the `service_user` rule are in # Runtime identity & data ownership. This is why a folderless app does not "just run as root by default" — relying on the image's default user breaks the moment the brain is not itself root (the native inner loop) or the image's default user is non-root.

### User content (use-case folders)

Apps reach user content by **bind-mounting use-case folders** declared in the manifest (`APP_MANIFEST.md` # `folders`):

```yaml
permissions:
  folders:
    - { folder: photos, mode: write }
    - { folder: documents, mode: read }
```

Each declared folder is bind-mounted at a fixed in-container path `/moose/<folder>` and the absolute path injected as `MOOSE_FOLDER_<NAME>`; the app's compose maps that variable to its own library path (`APP_MANIFEST.md` # `folders`). `mode` defaults to `read` if unspecified — least privilege, and `write` is a deliberate choice the catalog reviewer notices.

Use-case folder taxonomy v1 (fixed): `photos`, `documents`, `movies`, `music`, `notes`, `downloads` — mapped to capitalized directories (`Photos/`, `Documents/`, etc., per `STORAGE.md`). User-defined folders deferred.

**The host source is the installer's per-folder election, not the manifest's.** A declared folder binds one of two sources, chosen at install:

- **Personal source** — the owner's `/home/<user>/<Folder>/`, owned by that user (UID in the moose 3000+ range), with `/home/<user>/` mode `0750` (`STORAGE.md` # Permissions). The container runs as the owner's UID and reaches no other user's home. This is the default offered for a personal instance.
- **Shared source** — `/srv/moose/shared/<Folder>/`, the household tree every member can already reach via the `moose-shared` group (`STORAGE.md`, `USERS_AND_GROUPS.md`). The brain adds the container to `moose-shared` (compose `group_add`) so it has exactly that group's access — no new privilege. When the elected `<Folder>[/<subfolder>]` doesn't pre-exist, the brain creates it `root:moose-shared` mode `02770` (setgid) before `compose up` — see the bind-source preparation rule under # Runtime identity & data ownership. Always used by a household instance; offered to a personal instance when the installer elects it.

A personal instance reading the shared tree (the "my own Jellyfin on the family library" case) is now a supported election — **this supersedes the earlier MVP carve-out** that forbade Tier-3 per-user apps from crossing the per-user/shared boundary (`DECISIONS.md` 2026-05-30). The boundary still holds in the one direction that matters: a household (shared) instance never binds a single member's private `~/`, because there is no one owner to scope it to.

The `PUID`/`PGID` env-var pattern from earlier drafts is gone — we set the container's runtime UID directly via the compose `user:` field, so file ownership lines up natively. Apps that hardcode an internal UID and ignore the runtime user override will hit permission errors and get pulled from the catalog.

### Devices

`permissions.devices` lists explicit device paths or shorthand categories:

```yaml
permissions:
  devices:
    - /dev/ttyUSB0          # Zigbee/Z-Wave dongle
    - /dev/video0           # webcam
```

Nothing else under `/dev` is exposed. The brain validates that requested devices exist before starting the app.

### GPU

Separate field, because driver wiring is platform-specific (NVIDIA runtime, Intel/AMD `/dev/dri`) and the brain has to do real work, not just pass a device through:

```yaml
permissions:
  gpu: true
```

The OS handles drivers, runtime selection, and device exposure. The app sees whatever GPU is present and can introspect model, memory, and capabilities through standard tooling (e.g., `nvidia-smi`, `/dev/dri`). App author requests access; the OS makes it work. The manifest stays **vendor-agnostic** — `gpu: true`, never a vendor or a device path — because vendor→runtime selection is the brain's job, not the author's.

**What the brain emits (Intel iGPU, v1).** The first slice targets the "old laptop in the pantry" — overwhelmingly an Intel integrated GPU exposed as `/dev/dri` (VA-API), which needs no proprietary driver or container toolkit. The brain learns the host's GPU through one query, `GET /v1/system/gpu` (`BRAIN_HOST_PROTOCOL.md` # GPU capability query: presence + vendor + the `render` group GID). When the manifest declares `gpu: true` and the host reports an Intel iGPU, the brain adds to the **main service** in the compose override: a `/dev/dri` device bind, and a `group_add` of the render GID from that query — the same `devices` / `group_add` machinery a shared-folder grant already uses, so a `cap_drop: [ALL]` container (no `CAP_DAC_OVERRIDE`) can still open the render node. **AMD** is a near-identical follow-on (also `/dev/dri`, integrated *and* discrete — only the image driver stack and vendor reporting differ); **NVIDIA** is structurally different (proprietary driver + NVIDIA Container Toolkit + `deploy.resources.reservations.devices`) and is a separate follow-on. The split is **DRI/VA-API path vs NVIDIA path**, not integrated-vs-discrete.

**No GPU is a hard refusal, not advice.** If `gpu: true` and the host reports no usable GPU (`present: false`), the brain refuses the install at capacity-check time, **before** generating the override — never a late `docker compose up` failure on a half-built instance. This is a **hard gate**, and the distinction matters: it is *not* the same posture as `resources.recommended`, which is advice the user can knowingly proceed past (`APP_MANIFEST.md` # B2 — recommended specs are never a cap, and the install-footprint free-space figure is likewise advisory, `BRAIN_HOST_PROTOCOL.md` # `GET /v1/system/status`). A declared GPU permission means the app's core function (transcoding, ML thumbnails) **cannot run at all** without the device, so the brain stops the install rather than silently dropping to CPU. Because an install is a Pattern-B job (`BRAIN_HOST_PROTOCOL.md` # Pattern B — Jobs), the refusal surfaces as a **failed install job** carrying a clear no-GPU message (a typed `lifecycle` error the brain raises before any Docker work); the advisory install-plan may additionally report GPU availability so the dashboard can warn before the user commits, the way it already surfaces the free-space figure.

The OS-image media stack (`/dev/dri` + mesa + `intel-media-va-driver`, the `render` group and its udev rules) and the real `/dev/dri` detection in `cmd/host-agent-real` ship separately and need on-hardware verification — tracked in issue #125. The brain-side override + capacity gate above are built and unit-tested against the fake host-agent.

---

## Runtime identity & data ownership

Every Tier-3 instance runs as a **resolved, moose-managed UID/GID** (the compose `user:` field), and every private bind dir it declares is created + chowned to match — the invariant that lets a `cap_drop: [ALL]` container, which has no `CAP_DAC_OVERRIDE`, write its own state (# Volumes). The identity is never the image's choice; it is one of four moose-owned sources:

| App shape | Runtime identity | Drawn from |
|---|---|---|
| Folder app, personal source | the **owner's** UID/GID | the moose user range (≥ 3000, `STORAGE.md` # Permissions) |
| Folder app, shared source | the **moose-app** shared service identity (+ `moose-shared` group) | fixed well-known (`/v1/identity/well-known`: 2000/2001) |
| Folderless app (default) | the **brain's own euid/gid** — root in production, the operator's user in the native dev brain | n/a (it is the creator of the bind dirs) |
| Folderless app, `service_user: true` | a **dedicated per-instance service identity** | the moose-reserved app-service band (below 3000, distinct from the fixed 2000/2001) |

### `service_user` — a dedicated non-root identity for folderless apps

The default folderless identity is the brain's euid, which is **root** in production. An image that writes its data as a **non-root** user — common in nginx+php-fpm and LinuxServer-style images — then cannot write that root-owned `data/` dir, and cannot chown it itself because `CAP_CHOWN` is stripped. `service_user: true` (`APP_MANIFEST.md` # B) is the author's declaration *"run me as a dedicated unprivileged account, and make my data writable by it."* When set, the brain:

- **allocates a UID/GID from a reserved app-service band** — below the 3000 user floor and above the fixed 2000/2001 well-known identities; host-agent owns the exact range and the allocation, a sibling of `/v1/identity/well-known`,
- **persists it on the instance row and reuses it across container recreations** — the identity is *stable for the life of the instance*, never re-rolled on update or restart (a transient UID would orphan the data it owns),
- **pins the container `user:` to it and chowns every declared private bind dir to it**, exactly as the folder cases already do.

**The app declares intent, never a UID.** There is deliberately **no manifest field naming a numeric UID/GID.** A manifest-named UID would be interpreted in the *host* namespace — moose runs no user-namespace remap (# Not in v1) — so it could alias a real host principal: a system service account, or a moose user in the 3000+ range. That would hand a compromised app that principal's filesystem identity, and `THREAT_MODEL.md` names a compromised app as a top adversary. moose owns the number; the manifest owns the *intent*. A numeric `user:`/UID in a submitted manifest is an admission rejection for both doors.

**`service_user` is meaningful only for folderless apps.** A folder app's identity is already a managed non-root UID (the owner, or the moose-app identity), so the field is redundant there; admission rejects `service_user: true` combined with a `folders` grant.

### What this does *not* cover

`service_user` works for images that **adopt the runtime `user:`** moose assigns. It does **not** rescue an image that **hardcodes a different internal UID** for its data writes and ignores the runtime user — e.g. a php-fpm pool pinned to `user = www-data` in baked config, or an entrypoint that runs as root and `setuid`-drops to a fixed service user (which additionally needs the stripped `CAP_SETUID`/`CAP_SETGID`). For those, moose cannot align ownership without either naming the image's host UID (refused, above) or user-namespace remapping (# Not in v1). Such images stay **curation-rejects** until userns-remap lands; the catalog-import ledger tracks the specific apps (`docs/dev/catalog-import-gaps.md` # nonroot-data-ownership) to revisit when it does. This also absorbs the **conflicting per-service-UID** shape (#193) — one app whose services appear to want *different* non-root identities: a sidecar needing merely *a* non-root UID is already served by `service_user`, so the only service that truly needs a *specific* identity is again a hardcoded-internal-UID image. A per-service distinct-UID mechanism wouldn't rescue it (and moose treats an app's services as one trust unit), so it folds into the same deferral rather than being a separate v1 mechanism.

### Foundation for future cross-app data grants

Keeping every runtime identity a **stable, moose-managed** principal (rather than a free-form or transient host UID) is also what keeps a future *"app A may read app B's data"* grant expressible: such a grant would be a bind mount of B's `data/` into A plus a group/ACL grant *referencing A's runtime UID* — the same shape as a `moose-shared` folder grant today. A free-form or re-rolled identity would make that grant point at an unmanaged principal. Whether cross-app data reads are ever sanctioned at all is a separate, open product question — today inter-app data sharing goes only through shared use-case folders and managed services (# Inter-app traffic, `THREAT_MODEL.md`).

---

## Capabilities & privilege

### Default

`cap_drop: ALL`. Linux capabilities are added back only for what the manifest declares.

### High-level toggles

The common case is expressed through high-level fields in the manifest, not raw cap names:

| Manifest field | Maps to |
|---|---|
| `internet: true` | NAT route on per-app bridge |
| `lan: true` | macvlan attachment + multicast |
| `devices: [...]` | device cgroup entries |
| `gpu: true` | platform-appropriate GPU runtime |
| `folders: [...]` (personal source) | bind mount of `/home/<user>/<Folder>/` at `/moose/<folder>` + injected `MOOSE_FOLDER_<NAME>` |
| `folders: [...]` (shared source) | bind mount of `/srv/moose/shared/<Folder>/` at `/moose/<folder>` + `moose-shared` group membership (`group_add`) + injected `MOOSE_FOLDER_<NAME>` |

App authors think "I need to control the network," not "I need `NET_ADMIN`." The brain does the translation.

### Escape hatch — not a v1 store field

There is **no `permissions.capabilities` list in the v1 store schema.** A store app gets `cap_drop: [ALL]` and adds nothing back; admission rejects any `cap_add` (`APP_LIFECYCLE.md` # admission policy, `APP_MANIFEST.md` # E). A reviewed-at-submission capability list for the rare legitimate case (`NET_ADMIN`, `SYS_TIME`) is a **deferred** schema addition — tracked in `NEXT.md`, not assumed by the catalog today.

The app that genuinely needs a capability, `privileged`, the Docker socket, or low-level hardware access does **not** get there through Door 2 — admission refuses those primitives for both doors. It runs as a curated OS integration (**Tier 2**, `SERVICE_PROVISIONING.md`), or the admin runs it directly over SSH (`AUTH.md` # SSH is rescue). The box owner keeps the power; it just isn't a one-paste UI action.

### Forbidden for both doors

These are container-escape primitives and are admission-rejected for store **and** custom apps alike:

- `privileged: true`
- Mounting `/var/run/docker.sock`
- any `cap_add` (`SYS_ADMIN` especially)

Neither door can request them through the UI. **Admission is deliberately door-symmetric** (`APP_LIFECYCLE.md` # admission policy) — Door-2 carries *permissive defaults*, not *relaxed enforcement* (`DECISIONS.md` 2026-06-02). The escape path for a genuinely-unsandboxable container is Tier-2 curation or the admin's SSH access, not a custom paste. A future door-asymmetric relaxation (and the related reviewed `permissions.capabilities` allowlist) is parked in `NEXT.md`, not assumed today.

### Not in v1

- **User namespace remap.** Breaks too many images. Revisit once the catalog is mature. It is also the blocker for supporting images that hardcode a non-root *internal* UID (# Runtime identity & data ownership — `service_user` covers images that adopt the runtime user, not that class).
- **Custom seccomp / AppArmor profiles.** Docker defaults are sufficient for the threat model.

---

## Resource limits

The manifest declares **recommended** specs only — never a ceiling:

```yaml
resources:
  recommended:
    memory: 512M
    cpu: 1.0
```

Used for:
- **Capacity check at install time.** "You have 800M free; this app wants 512M; fine."
- **Store display and sorting.**

**The manifest cannot impose a limit — there is no `limit` field, by design.** The author can't see the user's hardware: a baked-in `mem_limit` would OOM-kill an app during a legitimate usage peak (photo indexing, transcode, ML) on a box with gigabytes free. Limiting is the *user's* call ("the user owns the box"), not the author's. `recommended` is advice; it never throttles.

### Default runtime: burst freely

Apps run with **no cgroup cap by default** and may burst above their recommendation. Peaks should complete fast, not be throttled. CPU in particular is **never capped** — it's time-shared, so a CPU-hungry app simply runs slower and harms nothing; throttling it only makes it feel needlessly sluggish.

### Control-plane protection (structural, not a cap)

"Default unlimited" leaves one hole: a runaway app exhausting RAM could trip the kernel OOM-killer and take down the *brain*, removing the very UI the user needs to fix it. Closed structurally, independent of any app limit:

- **The brain and host-agent get a protected OOM score** (effectively last-to-be-killed under memory pressure). A genuine runaway app is killed before the control plane.

This is deliberately **OOM-priority, not a hard cap**: a cap bites even when RAM is free (degrading peaks); OOM scoring only acts when memory is *actually* exhausted, so peaks that fit are untouched and only a true runaway is reaped — and the offending app dies before the brain. It serves the no-degraded-peaks goal and the don't-starve-the-brain goal at once.

### Optional user-set cap (memory only)

For the rare app that *persistently* misbehaves, the user can set a per-app **memory** cap from the UI:

- **Default off.** Set only when a user chooses to clamp a specific hog.
- **Memory only** — no CPU cap (see above).
- Surfaced reactively: the per-app usage view (`LOCAL_ANALYTICS.md`) and the app-hog signal (`NOTIFICATIONS.md`) help the user identify *which* app to clamp.
- Setting it shows a plain-language warning: *"Limiting this app may cause it to slow down or restart during heavy use."* The user-caused OOM-during-peak risk is theirs, explicitly acknowledged.

When the user sets a cap, the brain applies the corresponding cgroup `mem_limit` on the container; clearing it returns the app to unlimited burst.

---

## Secrets & credentials

Injected as **environment variables** at container start. Managed-service credentials, API keys for moose-provided integrations, etc.

```
DATABASE_URL=postgres://app_xyz:...@managed-postgres:5432/app_xyz
REDIS_URL=redis://...
```

Universally supported by Docker images. The brain may surface these in the UI for advanced users (toggleable view, off by default).

Mounted-file secrets (Docker secrets style) deferred — only ~half of images support it, and the threat model on a single-user home server doesn't justify the compatibility cost.

---

## Managed services placement

When a per-user app declares `services: [postgres]`, the brain runs a Postgres container **co-located on that (user, app)'s per-app network**. Only that specific instance — Andrei's Photos, not Maria's — can reach it.

- Lifecycle tied to the (user, app) tuple. Uninstall Andrei's Photos → Andrei's Postgres goes away; data backed up first per `SERVICE_PROVISIONING.md`. Maria's Photos is untouched.
- Postgres data lives under the per-(user, app) instance dir (`/var/lib/moose/state/instances/<id>/managed/postgres/...`), owned by the user's UID with restrictive perms. Cross-user filesystem access is blocked the same way every other app-state dir is — POSIX ownership + the brain controlling the bind-mount surface. **Open:** when fscrypt lands for `/home/<user>/`, does it extend to `/var/lib/moose/state/instances/` for per-user app state? Tracked in `NEXT.md`.
- Network-layer isolation: cross-user, cross-app database access is impossible by construction.
- Cost: in the worst case, N users × M apps requesting Postgres = N×M Postgres instances. Realistic case (1–2 users, one heavy account running most apps) keeps this well within home-server budgets.

**Likely to change.** Once we have catalog data on duplication patterns, we may move to a shared service plane (one Postgres process, per-(user, app)-scoped databases and credentials). Schema and manifest stay the same; placement becomes an internal detail.

---

## Failure mode

When an app violates its declared permissions at runtime — tries to reach the LAN with `lan: false`, opens a raw socket without `NET_RAW`, writes to a `read`-mode folder — the action **silently fails at the kernel/Docker layer** and is **logged to the app's log stream** with a clear reason:

```
[moose-isolation] blocked outbound connection to 192.168.1.50:80 — app declares lan: false
```

No popup, no kill. The app sees a normal "connection refused" or "permission denied" and handles it (or doesn't). The brain sees the violation in the log stream and surfaces it in the app's troubleshooting view.

This matches the user's mental model: "the app says it doesn't need X, the OS makes sure it really doesn't get X." Loud failures would punish honest apps that probe for optional capabilities.

---

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
