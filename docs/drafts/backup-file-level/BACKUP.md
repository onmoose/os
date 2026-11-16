# malmo Backup

> Working spec. What a hosted box backs up, how it is captured consistently, and how it is restored onto a fresh box. Resolves `NEXT.md` # Backup architecture shape and absorbs `ENVIRONMENT.md` # Logical export / restore bundle, which named this doc as its likely home. Companion to `STORAGE.md` (the on-disk layout this reads), `APP_LIFECYCLE.md` (the per-instance directory and the no-named-volumes rule), `APP_MANIFEST.md` (`storage.data_volumes` / `cache_volumes`), `SERVICE_PROVISIONING.md` (managed-service dumps), `AUTH.md` (credentials), and `ENVIRONMENT.md` (the hosted profile this ships in).

## Scope

**Scheduled off-site backup is a `hosted` capability.** On hosted, malmo runs the box, so malmo carries the custody obligation: a tenant hands over their photos, documents, and app state and gives up the hardware-ownership half of the pitch (`ENVIRONMENT.md` # Threat model). Custody without off-site backup is the worst version of that trade — one failed volume and the data is gone.

**On `appliance` there is no scheduled backup in v1.** No destination, no credentials, no timer. An appliance box with a single drive and no parity (SnapRAID is deferred, `STORAGE.md` # Levels of complexity) has no protection against drive failure, and the spec says so rather than leaving it to be inferred. A user-configured destination is tracked in `NEXT.md` # Appliance backup destination — it is a real design problem (whose credentials, entered where, in a UI built for non-technical users) and not a subset of this work.

What the appliance does inherit is everything below that is not the push agent: the backup set, the consistency rules, and the restore transaction. Because the restored artifact is a **logical bundle** rather than a disk image, it is also the vehicle for the portability promise — "it's your OS, move it home whenever you want" (`ENVIRONMENT.md` # Logical export / restore bundle). A disk image cannot serve that: the two profiles' images differ (LUKS, LAN, mDNS present on one, absent on the other), so a clone of a hosted VM will not boot on a laptop. **Restoring onto an appliance is not a v1 path**, but the bundle is designed so it can become one without a format change.

## What is backed up

### Roots, minus an exclude list

The set is defined as **roots with exclusions**, not as an enumerated list of paths. This is the most important choice in the spec.

An enumerated include list would have to name the use-case folders, and `STORAGE.md` # What apps and users actually see states that users **may rename, delete, or add folders**. An include list therefore silently omits anything a user creates — producing a backup that looks healthy and is missing data, discovered only at restore. An exclude list fails in the opposite direction: an unanticipated path is backed up, costing storage rather than data.

**The rule: take the roots and subtract. Anything not explicitly excluded is included.**

**On `hosted` there are three roots**, because the profile has no bind-mount tree. `ENVIRONMENT.md` # Storage (hosted) collapses the appliance layout: no mergerfs, no add/eject, and deliberately **one volume for everything** — so `/home` and `/var/lib/malmo` are real directories on the root filesystem, not views onto `/srv/malmo/`.

| Root | Holds |
|---|---|
| `/home/<user>/` | user content |
| `/var/lib/malmo/` | brain SQLite, app instances, managed-service state |
| `/srv/malmo/shared/` | household-shared content (`internal/lifecycle/lifecycle.go` sets this root on both profiles) |

**On `appliance` the same three logical roots exist as one tree** — `/srv/malmo/{home,state,shared}`, bind-mounted outward (`STORAGE.md` # Data drive(s)). There, the rule is to back up the underlying tree rather than the bind-mount views, or the same bytes are traversed twice under two names.

**The bundle therefore records logical roots, not absolute paths.** The same photo is `/home/cindy/Photos/…` on hosted and `/srv/malmo/home/cindy/Photos/…` on an appliance. A bundle keyed to absolute paths can only ever restore onto the profile it came from, which forecloses the portability promise. Tagging each root by role — `user-content`, `state`, `shared` — costs nothing now and keeps the appliance path open.

### The run must refuse to snapshot a broken box

A run that captures an empty or drastically incomplete tree and records it as a success is worse than a failed run: with retention, good snapshots are eventually replaced by empty ones.

The appliance has a purpose-built mechanism for the version of this caused by bind mounts racing their parent — `STORAGE.md` # Storage canary. **It does not exist on hosted**, which has no bind mounts to race and no physical drive to lose (`ENVIRONMENT.md` # Storage: "no mergerfs, no add/eject, no canary"). Hosted needs its own precondition, checked before anything is written to the repository:

- every expected root is present and non-empty;
- the captured set is not implausibly smaller than the previous snapshot.

Failure aborts the run and reports, rather than committing a snapshot.

### Excluded, and why

Paths below are given relative to the state root (`/var/lib/malmo/` on hosted). **Verify every one against the code before implementing** — `STORAGE.md`'s layout diagram is stale: it shows `brain/state.db` and `managed-services/`, while `cmd/brain/main.go` opens `<stateDir>/malmo.db` and `internal/lifecycle/services.go` writes services under `<stateDir>/services/`. An exclude keyed to a name that no longer exists silently fails to exclude, which is how live databases end up file-copied into a backup.

| Excluded | Why |
|---|---|
| `instances/*/snapshots/` | pre-update tars (`UPDATES.md` # Pre-update snapshot), regeneratable and by definition superseded |
| every declared `cache_volumes` subtree | regenerable by construction — thumbnails, transcodes, downloaded models. The rule is the declared data tree **minus** the declared cache subtrees, since a cache commonly nests inside a data volume |
| the **live data directories of managed services** (under `services/`) | captured as logical dumps instead. A file copy of a running database is a corrupt copy |
| `catalog-cache/` | re-fetchable from the catalog |
| `seed.json` | carries the enrollment credential; a restore target is issued a fresh seed anyway, so including it only widens the blast radius of a leaked repository key |
| the restic local cache | reconstructable from the repository |
| `lost+found`, sockets, device nodes | not data |

The OS and Docker images are excluded by construction, since neither lives under a root. The OS is a reproducible `mkosi` image and the image layers are re-pullable by digest, with the digests authoritative off-box in the catalog (`APP_STORE.md`).

### Added from outside the roots

Three things must be captured that live outside the roots:

- **Logical dumps of every managed service**, per app (below).
- **Credentials** — the `/etc/shadow` entries for malmo users (# Credentials).
- **A manifest of the run itself** — the roots captured and their roles, the OS image identity, and the bundle's schema version, so a restore can refuse a bundle it does not understand rather than half-applying it.

## Databases get dumped; files get copied

A backup that walks a filesystem is not atomic. Anything live and database-shaped is captured as a logical dump instead of a file read. This applies to two things today and to anything database-shaped added later.

### The brain's SQLite

A live SQLite file copied while the brain is writing is a corrupt copy. It is captured with `VACUUM INTO` to a staging path, and the staged file is what enters the backup.

### Managed services

`SERVICE_PROVISIONING.md` provisions shared managed services as **brain-owned compose projects** (`malmo-svc-postgres-15`, `malmo-svc-mysql-8-0`, `malmo-svc-valkey-8`), each with its own data volume shared across every app that asked for that engine. Postgres, MySQL, MariaDB, and Valkey are built and shipped.

Their data does **not** live under `/var/lib/malmo/instances/<id>/`. An app using a managed Postgres keeps its config and bind-mounted files in the instance directory and its rows in the shared service. Copying only instance directories restores such an app with its settings intact and its database empty — a backup that looks complete and silently is not, with nothing signalling it until a restore.

`SERVICE_PROVISIONING.md` # At backup already states the intended shape: "Dump is included in the app's backup archive alongside its data volumes." Concretely:

- A **logical dump**, not a file copy of the service's data volume.
- **Per app, not per engine**, so a restore can be scoped to one app without touching another's rows.
- Run through the **same throwaway one-shot client container the brain already uses to provision** — `docker run --rm --network malmo-svc-<k>-<v> --env-file <serviceDir>/.env <serviceImage> <client …>` — so the brain still never joins a service network (`DECISIONS.md` 2026-06-02, 2026-06-15).

`SERVICE_PROVISIONING.md` lists managed-service backup/restore as deferred and **gated on the backup design**. This spec is that gate.

## Credentials

`AUTH.md` makes PAM the source of truth for passwords: the brain holds no password hash and delegates verification to the host-agent. The hashes live in `/etc/shadow`, outside the backup root. Without them, a restored box brings back every user account in a state where nobody can log in.

**v1 backs up the `/etc/shadow` entries for malmo users.** That is the whole credential set on hosted.

- The user logs in after a restore with the password they already had. Nothing to communicate, nothing to reset, no admin handling other people's passwords.
- **Samba is not part of this on hosted.** `ENVIRONMENT.md` # Access & files states SMB is not shipped there — no LAN to serve. Samba keeps its own NT hashes in `tdbsam`, synced from `passwd` only on a live change (`AUTH.md` # Samba password backend), so a bundle restored onto an **appliance** would need it captured too or SMB breaks while dashboard and SSH work. Appliance restore is not a v1 path; the bundle schema should leave room for the field rather than pretending the profiles match.
- **This puts password hashes in the backup**, a real widening of exposure but not a new trust concession: the repository is client-side encrypted, and `ENVIRONMENT.md` # Threat model already places malmo-operated infrastructure inside the hosted trust boundary with operator-escrowed keys.
- It couples the bundle to a host file format, which is the argument for revisiting it once the successor below is available.

**The named alternative, for when the box→cloud channel exists:** the user authenticates at malmo.network, so the control plane can push a password to a restored box over that channel and keep hashes out of the backup entirely. It is strictly better and it is not v1, because it depends on a second workstream landing and adds a credential-push operation with its own authorization rules.

## Consistency: what is guaranteed and what is not

Three tiers, labelled honestly rather than implied:

1. **Managed-service data is consistent by construction.** A logical dump is a point-in-time snapshot of an app's rows. This covers the population most exposed to tearing.
2. **The brain's own state is consistent by construction**, via `VACUUM INTO`.
3. **Everything else is crash-consistent.** User content and an app's bind-mounted `data/` are captured as they lie. For user content this is close to harmless — media files are written once and rarely rewritten in place. For an app that **bundles its own database** inside `data/`, which malmo cannot see into, a restore is equivalent to restoring from a machine that lost power at that instant. Well-built databases recover from exactly that; not every bundled one will.

**Lifecycle hooks stay deferred.** `DECISIONS.md` 2026-05-13 deferred the manifest's `hooks:` block noting that every use case named was tied to managed services or backups. Backup arriving is that trigger, and the answer is still no for v1: the managed-service dump path removes the tearing risk for the apps that matter most without asking any author to write anything, which is the better trade for a catalog whose authors are mostly upstream projects that have never heard of malmo. When hooks return as one-shot container images (`NEXT.md` # Hooks — concrete shape for return), a `pre_backup` hook refines tier 3 for apps that ship one — the same relationship `pre_update` has to the pre-update snapshot.

**A filesystem freeze is rejected, not deferred.** It needs a snapshotting volume manager the locked ext4 layout does not provide (`DECISIONS.md` — ext4 + LUKS, not ZFS).

The measure of this feature is successful **restores**, not successful uploads.

## The push agent (hosted)

### Box-initiated

Nothing can reach into a hosted box: the lean image installs no `openssh-server`, and the control plane only talks to a box at provision time (`ENVIRONMENT.md` # How the profile is realized). Backup is therefore box-initiated push on a `malmo-backup.timer` in the quiet window, jittered so a fleet does not stampede its destination, with the outcome reported back so a silently-failing backup is visible rather than found at restore (`ENVIRONMENT.md` # Box identity).

### Who actually runs a backup

The timer is a host unit, but two of the three capture steps are **not host operations**, so the agent cannot be a standalone host script:

- `VACUUM INTO` acts on the brain's live SQLite, which the brain owns.
- Managed-service dumps need the service `.env`, the service network, and `docker run --rm` — and the brain reaches Docker through the socket-proxy (`CONTROL_PLANE.md` # Docker socket exposure), which is the only sanctioned path. A host script bypassing it re-opens a boundary `DECISIONS.md` 2026-06-14 closed deliberately.

The brain also runs containerized with `MALMO_STATE_DIR`, so brain-visible paths and host paths are not the same strings — a detail that has to be explicit wherever the two meet.

The workable split is **the timer as the clock, the brain as the executor**: the host unit triggers a run, the brain performs the dumps and hands back a staged, consistent set, and the push happens against paths the host can read. The exact seam — a host-agent job, a brain endpoint, or a brain-internal scheduler with the timer only as a wakeup — is the first thing the build issue must settle, because it determines where the restic invocation lives and which side holds the repository credential.

### Engine: restic

- Client-side AES-256, so nothing readable leaves the box.
- Content-addressed deduplication, so a daily run over a mostly-unchanged home directory transfers almost nothing.
- A single static binary, shipped via `mkosi.extra` so it adds no apt package and the lean-image manifest guard still passes (`ENVIRONMENT.md` # How the profile is realized).

**Every snapshot is logically full and physically incremental.** A run walks the whole set and stores only chunks the repository does not already hold, but the snapshot it records is a complete view of the box at that moment. There is no full/incremental chain and no base to reconstruct from, which has three consequences worth stating: every snapshot restores standalone, so one damaged snapshot never invalidates later ones; retention can drop any snapshot without orphaning another, because chunks are refcounted rather than chained; and there is no weekly-full schedule to design. The first run is the only expensive one.

### The box appends; it never prunes

The box holds a credential that can write to its own repository. If that credential could also delete, compromising a box would destroy its backups too.

- The box's credential is **write-and-list**, scoped so a box can never see or touch another box's repository.
- `forget` and `prune` are **not box operations**. Retention is enforced from the hosted control plane with a separate credential.
- Credential and repository-key rotation arrive over the same authenticated channel as the status report.

### Repository keys

Generated per box and **escrowed by the hosted control plane**. This is not operator-blind backup and the spec does not pretend otherwise — `ENVIRONMENT.md` # Threat model already states that at-rest encryption in the hosted profile defends against a co-tenant, a leaked disk image, and an idle volume, not against the operator. Escrow is also what makes operator-driven restore possible for someone who has lost their box entirely.

### Operational edges

- **The first run is hours, not minutes** on a large home directory, and per-box jitter does not help one slow box. It needs its own bound; a timeout sized for the steady-state incremental will kill it.
- **A box powered off mid-run leaves a stale repository lock** that refuses the next run. Hosted suspend does exactly this. Unlock handling is explicit, not discovered as a box that quietly stopped backing up.
- **Disk pressure fails the backup exactly when it matters.** The staging dumps and the local cache need free space. Insufficient space is a reported failure with a `HEALTH.md` issue, never a silent skip.
- **A failed run is loud.** A box that stops reporting is visible operator-side; "backed up daily" is a claim that has to stay continuously true.

## Restore

A restore path that has never been exercised is a hope, not a backup. The acceptance for any work in this spec is a real restore.

### Restore is its own entry path, not a replay of first-run

A restored box has users in its brain state, so the ordinary `/setup` empty-box guard returns 409 and no wizard appears. That is the desired outcome — with credentials restored, the owner simply logs in — but it means restore is a distinct path rather than a variant of provisioning.

### Brain state splits into portable and box-bound

**Restoring the brain's SQLite is not a file copy.** The `box_meta` table holds box-scoped identity, and `cmd/brain/main.go` takes a frozen-identity path at boot: if a `box_id` is already present, the brain **ignores the seed entirely**.

So restoring the old database wholesale onto a freshly provisioned box overwrites the new box's identity with the destroyed box's, and the next boot adopts it — serving the old DNS name with acme-dns credentials belonging to a box that no longer exists. Certificate issuance fails, and the box is unreachable at the name the user was actually given. The failure is silent at restore time and surfaces at the first certificate operation.

| Class | Rows | Source on restore |
|---|---|---|
| **Box-bound** | `box_id`, `assertion_verification_key`, `enrollment` | the **new** box's seed, never the backup |
| **Owner linkage** | `sso_owner_sub`, `sso_owner_user_id` | the backup — the same portal account owns the restored box, and carrying them is what lets the owner log straight in |
| **Portable** | users, installed apps, settings, telemetry consent, first-run completion | the backup |

Consequently, **app URLs change**: they are `<slug>.<box-id>.malmo.network`, so a restored box's apps are reachable at a new hostname and anything holding the old one is re-rendered on the reconcile pass.

### The transaction

**Preconditions, checked before anything is written:** the bundle's schema version is understood, and the target's disk is large enough for the captured set. Hosted grows its root to fill the provider disk at boot (`ENVIRONMENT.md` # Storage), so restoring onto a smaller SKU than the destroyed box fails partway through — after the box exists and the operator believes the restore is underway. It is a cheap check and an expensive omission.

1. **A fresh, current box** is provisioned and ingests its own seed. It has a new identity; it is not a resurrection.
2. **Brain SQLite** is restored with the box-bound rows re-pointed per the table above — first, because it is the index of what everything else means.
3. **Credentials** are restored, so users can log in.
4. **Managed services** are provisioned for the engines the restored app set requires, and each app's dump is loaded into its own database with its own credential re-established.
5. **Instance directories** are restored, then apps are brought up by the ordinary reconcile path, pulling images by pinned digest.
6. **User and shared content** is restored under the root, reconciled against the users the restored brain state defines.

**Restore is operator-triggered, by design.** The flow is: a box becomes unhealthy or dies, the customer contacts support, and an operator runs the restore. There is no self-serve restore surface, no customer-facing button, and no first-run "restore from backup" branch — `FIRST_RUN.md` stays greenfield-only, and the wizard fork it would otherwise need is not part of this work.

**One part of it must still be a tool, not a runbook step.** Re-pointing the box-bound rows above is the step where a hand-run restore silently adopts the destroyed box's identity and breaks certificate issuance — with the failure surfacing later, at the first renewal, rather than during the restore. An operator working through an incident at speed should not be hand-editing identity rows in SQLite. The re-point is a command with the split encoded in it; the runbook calls that command.

Self-serve file-level restore stays a later possibility and would live in the `FILES.md` surface; it is explicitly out of scope here.

## Locked decisions

- **Scheduled off-site backup is hosted-only in v1.** The appliance inherits the set, the consistency rules, and the restore path, but no destination.
- **The set is roots minus an exclude list**, never an enumerated include list — users may add and rename folders, and omission is the failure mode that hides until restore. Hosted has three roots; the appliance has the same three as one bind-mounted tree.
- **The bundle records logical roots, not absolute paths**, so it is not welded to the profile it came from.
- **A run refuses to snapshot a broken box.** Roots present and non-empty, and no implausible shrink against the previous snapshot. An empty snapshot must never be recorded as a success. The appliance canary is not this mechanism and does not exist on hosted.
- **The brain executes the capture; the timer is only the clock.** Dumps require the brain's SQLite and the socket-proxy Docker path.
- **Databases get dumped; files get copied.** Brain SQLite and every managed service, and anything database-shaped added later.
- **Per-app managed-service dumps**, not per-engine volume copies.
- **Consistency is tiered and stated**, not implied. Hooks stay deferred; filesystem freeze is rejected.
- **`/etc/shadow` entries are backed up** so a restore is transparent to the user; the control-plane password push is the named successor. Samba's store is an appliance-only concern, since hosted ships no SMB.
- **Brain state splits into portable and box-bound.** Box identity always comes from the new box's seed.
- **The box appends; it never prunes.** Deletion authority never lives on the box being protected.
- **Repository keys are escrowed on hosted**, consistent with the trust boundary already stated, and never claimed to be otherwise.

## Open items

Tracked in `NEXT.md`, not here.
