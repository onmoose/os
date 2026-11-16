TITLE: Daily off-site backup for hosted boxes: data/cache parsing, managed-service dumps, and the backup agent

Size: L
Area: backend
Depends on: the specs issue (#N); the status report depends on box identity (#N)

## Summary

Builds the backup set `BACKUP.md` defines and pushes it off-box daily on hosted. Two of its three parts are prerequisites that other already-specced-but-unbuilt work is also waiting on, so they are worth doing here rather than as backup-private plumbing.

Consider splitting at the marked seam if this is too large for one slice — the first two sections stand alone and have value without the agent.

## Spec / source of truth

`docs/specs/BACKUP.md` (written by the specs issue). Also `ENVIRONMENT.md` # Storage (hosted) and # How the profile is realized — read these before `STORAGE.md`, since hosted collapses most of the appliance storage model. Then `STORAGE.md` # What apps and users actually see, `APP_MANIFEST.md` # Storage, `SERVICE_PROVISIONING.md` # At backup, `UPDATES.md` # Pre-update snapshot, `CONTROL_PLANE.md` # Docker socket exposure.

## Do

### 1. Parse and validate `data_volumes` / `cache_volumes`

`internal/manifest/manifest.go` currently reads only `storage.estimated_size` and says so in a comment: the other storage keys "live in the compose, not parsed here." Nothing consumes the data/cache split, which is why two specced features are unbuilt.

- Parse both lists into `manifest.Storage`.
- **Enforce the rules `APP_MANIFEST.md` states but nothing checks**: a `cache_volumes` path may nest inside a `data_volumes` path (the common shape is `./data` with `./data/cache`), but paths that do not nest must not otherwise overlap. Admission rejects a violation. This is the substance of the work — an unvalidated declaration silently excludes a data tree from backup, and that surfaces at restore.
- Door-2 synthetic manifests declare no `cache_volumes`; the documented fallback is best-effort backup of everything under `data/`.

**Why this is not backup-private:** `UPDATES.md` # Pre-update snapshot and `APP_LIFECYCLE.md` # Update transaction spec a pre-update tar of declared `data_volumes` as the **v1 rollback safety net** for app-driven schema migrations — a locked decision (`DECISIONS.md` 2026-05-17) unbuilt for exactly this missing field. Same field, two consumers.

### 2. Per-app managed-service dump path

`SERVICE_PROVISIONING.md` provisions shared services outside the instance directory, so without this an app using a managed database backs up its config and loses its rows.

- A **logical dump per app per service** (not a file copy of the service volume, not per-engine), so a restore can be scoped to one app.
- Run through the **existing throwaway one-shot client container** — `docker run --rm --network malmo-svc-<k>-<v> --env-file <serviceDir>/.env <serviceImage> <client …>` — so the brain still never joins a service network (`DECISIONS.md` 2026-06-02, 2026-06-15). `internal/lifecycle/services.go` already does this for provisioning.
- Cover the shipped engines: Postgres, MySQL, MariaDB, Valkey.
- The matching **restore** direction, since a dump nobody has restored is not a backup.

**Also not backup-private:** `UPDATES.md` and `APP_LIFECYCLE.md` both spec a managed-service dump alongside the pre-update tar, and `SERVICE_PROVISIONING.md` lists cross-version migration as gated on the same dump/restore path.

--- seam: everything above stands alone ---

### 3. The backup agent (hosted)

**Settle the execution model first — it determines everything below.** The timer is a host unit, but `VACUUM INTO` acts on the brain's live SQLite and the service dumps need the service `.env`, the service network, and `docker run --rm` through the socket-proxy (`CONTROL_PLANE.md` # Docker socket exposure) — a host script bypassing it reopens a boundary `DECISIONS.md` 2026-06-14 closed. The brain also runs containerized with `MALMO_STATE_DIR`, so brain paths and host paths are different strings. The shape is timer-as-clock, brain-as-executor; the seam (host-agent job, brain endpoint, or brain-internal scheduler woken by the timer) is this issue's first decision, because it fixes where restic runs and which side holds the repository credential.

**The set is roots minus excludes.** On hosted there are **three**: `/home/<user>/`, `/var/lib/malmo/`, and `/srv/malmo/shared/`. There is no `/srv/malmo` bind-mount tree here — `ENVIRONMENT.md` # Storage (hosted) has no mergerfs and one volume for everything, so `/home` and `/var/lib/malmo` are real directories. Do not enumerate user folders; users rename, delete, and add them, so an include list silently omits data.

Excludes, relative to the state root: `instances/*/snapshots/`, declared `cache_volumes` subtrees, the live managed-service data directories under `services/` (dumped instead), `catalog-cache/`, `seed.json`, the engine cache. **Verify these names against the code** — `STORAGE.md`'s diagram is stale (`brain/state.db`, `managed-services/`) versus `cmd/brain/main.go` (`<stateDir>/malmo.db`) and `internal/lifecycle/services.go` (`<stateDir>/services/`). An exclude keyed to a dead name silently fails to exclude, which is how a live database gets file-copied in.

**Record logical roots tagged by role, not absolute paths**, so the bundle is not welded to the profile it came from.

**Refuse to snapshot a broken box.** Every expected root present and non-empty, and no implausible shrink versus the previous snapshot, checked before anything is written. Note this is **not** the appliance storage canary, which does not exist on hosted (`ENVIRONMENT.md` # Storage: "no mergerfs, no add/eject, no canary") — it detects a wrong or removed physical drive and has no analogue on a cloud volume.

**Capture what lives outside the roots:** the brain SQLite via `VACUUM INTO` to a staging path (never a live file copy), the per-app service dumps from section 2, and the **`/etc/shadow` entries** for malmo users. Samba's `tdbsam` is *not* in scope — hosted ships no SMB (`ENVIRONMENT.md` # Access & files) — but leave room for it in the bundle schema, since an appliance restore would need it. Also record a bundle manifest: roots and their roles, the OS image identity, a schema version, so a restore can refuse a bundle it does not understand rather than half-applying it.

**The agent:**

- **restic in the hosted image**, shipped via `mkosi.extra` as a static binary so it adds no apt package and the lean-image manifest guard still passes.
- **`malmo-backup.timer` + service**, in the quiet window, **jittered** so a fleet does not stampede.
- **Backup configuration from the seed**; **status reported** on the authenticated box→cloud channel.
- **The box appends and never prunes.** No `forget`, no `prune`, no delete path in box-side code — retention is enforced elsewhere with a different credential, and that separation is the ransomware posture.
- **Edges are requirements, not polish:** the **first run** is hours on a large home directory and needs its own bound; a box powered off mid-run leaves a **stale repository lock** that must be handled explicitly; **insufficient disk** for staging dumps or the cache is a reported failure with a `HEALTH.md` issue, never a silent skip; a **failed run is loud**.

### 4. Restore

**Restore is operator-triggered**: a box dies, the customer contacts support, an operator runs the process. No self-serve surface, no first-run restore branch. But the identity re-point below is a **command**, not a documented SQL sequence — it is the step that silently breaks a restored box, and an operator mid-incident should not be hand-editing identity rows.

**Restoring the brain SQLite is not a file copy.** `box_meta` holds box-scoped identity, and `cmd/brain/main.go:397` takes a frozen-identity path at boot: if a `box_id` is present, the brain **ignores the seed entirely**. Restoring the old database wholesale onto a fresh box adopts the destroyed box's identity, then serves the old DNS name with acme-dns credentials for a box that no longer exists — silent at restore, surfacing at the first certificate operation.

- **Box-bound** (`box_id`, `assertion_verification_key`, `enrollment`): always from the new box's seed.
- **Owner linkage** (`sso_owner_sub`, `sso_owner_user_id`): from the backup, so the owner logs straight in.
- **Portable** (users, apps, settings, telemetry consent): from the backup.

Restore is its own entry path — a restored box has users, so `/setup` returns 409 and no wizard appears. App URLs change, since they are `<slug>.<box-id>.malmo.network`; anything holding the old hostname is re-rendered on reconcile.

Order: fresh box → brain state with identity re-pointed → credentials → services + dumps → instances via ordinary reconcile → content.

Preconditions before writing anything: the bundle schema version is understood, and the **target disk is large enough** for the captured set. Hosted grows its root to fill the provider disk at boot, so restoring onto a smaller SKU fails partway through — after the box exists and the operator believes the restore is running.

## Touch

`internal/manifest/`, `internal/admission/`, `internal/lifecycle/services.go` plus a new backup package, `dev/cloud/` (image + units). Sections 1 and 2 are cross-platform and inner-loop testable; only the agent's host wiring is hosted/Linux-only.

## Done when

**A real restore, not a green upload.** Back up a box with a database-backed app installed, provision a fresh box, restore, and confirm: the app comes back with its **rows**, the user logs in with their **existing password**, and the restored box serves its **own** DNS name and gets a certificate — not the destroyed box's.

Plus: a manifest with overlapping non-nesting paths is rejected at admission; a `./data` + `./data/cache` declaration backs up the data tree minus the cache subtree; a run **aborts rather than snapshotting** when a root is missing or empty; a file placed in a newly-created user folder (not one of the standard use-case folders) is present in the restore, proving the exclude-list model; the hosted image still passes the lean package-manifest check with restic present; a box interrupted mid-run backs up successfully on its next timer firing.
