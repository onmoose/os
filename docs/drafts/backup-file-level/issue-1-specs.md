TITLE: specs: backup + restore architecture, and the authenticated box→cloud channel

Size: M
Area: backend
Depends on: none

## Summary

Docs only, no code. Writes the backup/restore architecture malmo has been deferring behind `NEXT.md` # Backup architecture shape, and specs the authenticated box→cloud channel a hosted box needs before it can report anything. Base for the two build issues that follow.

## Spec / source of truth

New `docs/specs/BACKUP.md`; new # Box identity section in `docs/specs/ENVIRONMENT.md`. Read `STORAGE.md`, `APP_LIFECYCLE.md`, `APP_MANIFEST.md` # Storage, `SERVICE_PROVISIONING.md`, `AUTH.md`, `UPDATES.md` # Pre-update snapshot, and `ENVIRONMENT.md` end-to-end first — this pass sits on all of them.

## Do

### 1. New `docs/specs/BACKUP.md`

**Scope.** Scheduled off-site backup is **hosted-only** in v1 — that is where malmo runs the box and carries the custody obligation. State plainly that an appliance box therefore has **no backup at all** in v1: single drive, SnapRAID deferred, no destination. Add `NEXT.md` # Appliance backup destination rather than letting it read as an oversight. The appliance still inherits the backup set, the consistency rules, and the restore path, because the artifact is a logical bundle and is the same vehicle as the portability promise in `ENVIRONMENT.md` # Logical export / restore bundle.

**The set is roots minus an exclude list, never an enumerated include list.** This is the most important choice in the spec. `STORAGE.md` # What apps and users actually see says users **may rename, delete, or add folders**, so an include list naming the use-case folders silently omits anything a user creates — a backup that looks healthy and is missing data. An exclude list fails the other way: an unanticipated path costs storage, not data.

**Get the profile difference right — it is not one root on hosted.** `ENVIRONMENT.md` # Storage (hosted) collapses the appliance layout: no mergerfs, no add/eject, **one volume for everything**. So `/home` and `/var/lib/malmo` are real directories there, not views onto `/srv/malmo/`, and hosted has **three roots**: `/home/<user>/`, `/var/lib/malmo/`, and `/srv/malmo/shared/` (`internal/lifecycle/lifecycle.go` sets the shared root on both profiles). The appliance has the same three logical roots as one bind-mounted tree, where the rule is to back up the tree rather than the views so bytes are not traversed twice.

Consequently **the bundle records logical roots tagged by role, not absolute paths** — the same photo is `/home/cindy/Photos/…` on hosted and `/srv/malmo/home/cindy/Photos/…` on an appliance, and a bundle keyed to absolute paths can only restore onto the profile it came from, forfeiting the portability promise.

**A run must refuse to snapshot a broken box.** An empty or drastically incomplete capture recorded as a success is worse than a failure, because retention eventually replaces good snapshots with empty ones. Do **not** reach for the appliance's storage canary: `ENVIRONMENT.md` # Storage says hosted has "no mergerfs, no add/eject, no canary" — it detects a removed or wrong *physical* drive and has nothing to do on a cloud volume. Hosted needs its own precondition: every expected root present and non-empty, and no implausible shrink against the previous snapshot.

Excludes (relative to the state root): `instances/*/snapshots/`, every declared `cache_volumes` subtree, the **live data directories of managed services** (dumped instead), `catalog-cache/`, `seed.json`, the engine's local cache. The OS and Docker images fall outside the roots by construction.

**Verify these path names against the code, not against `STORAGE.md`.** Its layout diagram is stale — it shows `brain/state.db` and `managed-services/`, while `cmd/brain/main.go` opens `<stateDir>/malmo.db` and `internal/lifecycle/services.go` writes services under `<stateDir>/services/`. An exclude keyed to a name that no longer exists silently fails to exclude, which is exactly how a live database gets file-copied into a backup. Fix the diagram in this pass.

**Databases get dumped; files get copied.** Brain SQLite via `VACUUM INTO`. Managed services via a **per-app logical dump** through the throwaway one-shot client container the brain already uses to provision — `SERVICE_PROVISIONING.md` provisions them as brain-owned compose projects with data outside `/var/lib/malmo/instances/<id>/`, so copying only instance directories restores a database-backed app with its settings intact and its rows gone. `SERVICE_PROVISIONING.md` # At backup already states the intended shape and lists this as "gated on the backup design"; this spec is that gate.

**Credentials.** `AUTH.md` makes PAM the source of truth and the brain holds no password hash, so without this a restore brings back accounts nobody can log into. v1 backs up the **`/etc/shadow` entries** for malmo users, which is the whole credential set on hosted — `ENVIRONMENT.md` # Access & files ships **no Samba** there, so `tdbsam` is not in scope. Note that an appliance restore would need it too (it holds its own NT hashes, synced from `passwd` only on a live change, `AUTH.md` # Samba password backend), and leave room in the bundle schema rather than pretending the profiles match. Note honestly that this puts password hashes in the backup, and why that is a widening rather than a new concession (the repository is client-side encrypted and `ENVIRONMENT.md` # Threat model already places the operator inside the hosted trust boundary). Name the successor: once the box→cloud channel exists, the control plane can push a password to a restored box and keep hashes out entirely.

**Who executes a run.** The timer is a host unit, but `VACUUM INTO` acts on the brain's live SQLite and the service dumps need the socket-proxy Docker path (`CONTROL_PLANE.md` # Docker socket exposure) — neither is a host operation, and the brain runs containerized with `MALMO_STATE_DIR` so its paths and host paths differ. Spec the split (timer as clock, brain as executor) and flag the exact seam as the build issue's first decision, since it determines where restic runs and which side holds the repository credential.

**Consistency, tiered and stated:** managed-service data and brain state consistent by construction; everything else crash-consistent, including an app that bundles its own database inside `data/`. Hooks stay deferred — `DECISIONS.md` 2026-05-13 deferred them noting every use case was tied to managed services or backups, and backup arriving is that trigger, but the dump path covers the exposed population without asking any author to write anything. Filesystem freeze is **rejected**, not deferred: no snapshotting volume manager under the locked ext4 layout.

**The push agent:** box-initiated (nothing can reach into a box with no sshd), restic via `mkosi.extra` so the lean-image guard still passes, jittered timer, **write-and-list credentials only — the box never prunes**, keys escrowed. Spell out that every snapshot is logically full and physically incremental: no full/incremental chain, every snapshot restores standalone, retention can drop any snapshot without orphaning another. Operational edges are requirements, not polish: the first run is hours; a box powered off mid-run leaves a stale lock; disk pressure fails the backup exactly when it matters; a failed run is loud.

**Restore, including the part that bites.** Restore is its own entry path, not a replay of first-run: a restored box has users, so `/setup` returns 409 and no wizard appears — which is the desired outcome once credentials are restored.

The real hazard is that **restoring the brain's SQLite cannot be a file copy.** `box_meta` holds box-scoped identity and `cmd/brain/main.go` takes a frozen-identity path at boot — if a `box_id` is present the brain **ignores the seed entirely**. Restoring the old database wholesale onto a fresh box therefore overwrites the new identity with the destroyed box's, and the next boot serves the old DNS name with acme-dns credentials belonging to a box that no longer exists. Silent at restore, surfaces at the first certificate operation. Spec the split: **box-bound** (`box_id`, `assertion_verification_key`, `enrollment`) always from the new seed; **owner linkage** (`sso_owner_sub`, `sso_owner_user_id`) from the backup so the owner logs straight in; **portable** (users, apps, settings) from the backup. Note the consequence that app URLs change, since they are `<slug>.<box-id>.malmo.network`.

Then the transaction ordering: fresh box → brain state with identity re-pointed → credentials → managed services + dumps → instances via ordinary reconcile → content. Add the preconditions: the bundle's schema version is understood, and **the target disk is large enough** — hosted grows its root to fill the provider disk (`ENVIRONMENT.md` # Storage), so restoring onto a smaller SKU fails partway through, after the operator believes it is underway.

### 2. Fix `ENVIRONMENT.md` # Admin bootstrap — as built (prerequisite)

That section is **stale relative to the code**. It describes the seed as `{box_id, admin_bootstrap_secret, enrollment}` and a `/setup` gate taking a `bootstrap_secret` body field. `internal/profile/seed.go` shows `{box_id, assertion_verification_key, enrollment}`, and `internal/store/store.go` says the assertion key "replaces the prior one-time admin-bootstrap secret hash" — hosted admin bootstrap is portal SSO now. Correct it before adding the section below, which assumes the corrected text.

### 3. New # Box identity section in `ENVIRONMENT.md`

Single-use hours-TTL bootstrap token in the seed → box generates an Ed25519 keypair at first boot → spends the token once to register the public half → signs every later request over a canonical string. Rotation is a new key signed by the old; revocation is clearing the key. Ed25519 signed requests rather than mTLS, because certificate auth terminates where TLS does and splits the identity check from the authorization depending on it.

Note this is the **outbound** direction — `assertion_verification_key` already covers inbound portal assertions and is unaffected.

Box-side constraints to state: the canonical signing string is a **byte-exact wire contract** tested against fixed vectors (same class as the seed shape and the assertion token format); enrollment precedes anything needing the channel and retries through the first-boot DHCP race; enrollment is first-boot-once like seed ingestion; the private key is at-rest state with the same custody question as `NEXT.md` # Encrypt hosted enrollment credentials at rest; no secret ever reaches a log; a restored box enrolls fresh and is a new identity.

Also note the standalone security payoff: a single-use token makes the seed's long-lived acme-dns credential exposure self-limiting, and is the precondition for eventually removing it from the seed.

### 4. Doc-map and cross-reference upkeep

- `docs/README.md` — add the `BACKUP.md` row. A spec not listed there is a bug.
- `NEXT.md` — resolve # Backup architecture shape into the new spec; add # Appliance backup destination; keep the genuinely-open items rather than deleting them.
- `SERVICE_PROVISIONING.md` — point the deferred backup/restore note at the now-existing gate.
- `DECISIONS.md` — an entry for this pass, noting explicitly that it does **not** reopen 2026-05-13 (hooks).
- `ENVIRONMENT.md` — # Logical export / restore bundle becomes a pointer; resolve the "export/restore bundle home" open question.
- `STORAGE.md` — fix the stale layout diagram (`brain/state.db` → `malmo.db`, `managed-services/` → `services/`) so the exclude list can be keyed to real names.
- `MALMO_NETWORK.md:168` and `:324` list off-site backup among paid SKUs. That framing is appliance/mesh monetization and is left as-is; flag it in the progress entry so it is a decision rather than drift.
- A `docs/progress/` entry plus its index line.

## Touch

`docs/specs/BACKUP.md`, `docs/specs/ENVIRONMENT.md`, `docs/specs/NEXT.md`, `docs/specs/DECISIONS.md`, `docs/specs/SERVICE_PROVISIONING.md`, `docs/README.md`, `docs/progress/`. No code.

## Done when

`docs/specs/BACKUP.md` exists and answers without hedging: what the backup root is and what is subtracted from it, which parts are consistent and which are only crash-consistent, how a user logs in after a restore, and which brain state is portable versus box-bound. `NEXT.md` # Backup architecture shape is resolved rather than still reserved, and the appliance gap is a named item. `ENVIRONMENT.md` # Admin bootstrap — as built matches the code, and # Box identity specs the outbound channel including byte-exactness of the signing string. `docs/README.md` lists the new spec.
