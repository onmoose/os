# Draft: file-level backup and restore — NOT ADOPTED, DO NOT MERGE

**Status: parked design work. Nothing here is decided, implemented, or scheduled.**

These files are a design pass from 2026-07-19 that was **not taken forward**. The hosted product went with a simpler mechanism for v1 (a control-plane-side daily provider snapshot per box, which needs no OS-side code at all). This branch exists so the work and — more importantly — the corrections that came out of it are not lost when the design is revisited.

**Do not merge this branch.** `docs/drafts/` is not one of the three doc homes (`docs/specs/`, `docs/progress/`, `docs/dev/`) and is not a convention this repo keeps. If this design is adopted, the files move to their real homes: `BACKUP.md` to `docs/specs/`, the box-identity section into `docs/specs/ENVIRONMENT.md`, and the issue drafts into GitHub issues.

## What is here

| File | What it is |
|---|---|
| `BACKUP.md` | the spec draft: backup set, consistency posture, credentials, restore transaction |
| `ENVIRONMENT-box-identity-section.md` | a proposed `ENVIRONMENT.md` section for the authenticated box→cloud channel |
| `issue-1-specs.md` | draft issue: the docs-only pass |
| `issue-2-box-identity.md` | draft issue: enrollment + request signing |
| `issue-3-backup-agent.md` | draft issue: manifest parsing, dumps, the agent |

## The design in one paragraph

A box captures its own data — user content, shared content, app instances, brain state — deduplicated and encrypted client-side, and pushes it off-box daily, because nothing can reach into a hosted box. Databases are dumped rather than file-copied. Restore lands the bundle on a fresh, current box. The artifact is a logical bundle rather than a disk image, which is what lets the same mechanism serve both disaster recovery and the "move it home" migration promise.

## Why it was parked, and what would bring it back

The v1 mechanism is bounded by the provider's **account-wide cap of 30 snapshots** (not per server, not per project), and it is whole-disk, crash-consistent, one-day-of-history, hosted-only. This design has none of those limits: it scales without a support ticket, restores a single file or a single app, costs in proportion to data rather than disk, and is the only option that could ever serve the appliance.

It comes back when any of those limits starts to bite — most likely the snapshot cap, or the first customer who wants a single deleted file back.

## Findings worth keeping even if the design is never adopted

These came out of checking the drafts against the code and are true regardless of which backup mechanism ships:

1. **Two live doc/code drifts.** `ENVIRONMENT.md` # Admin bootstrap — as built describes a seed carrying `admin_bootstrap_secret` and a `/setup` gate taking a `bootstrap_secret` field; `internal/profile/seed.go` shows `{box_id, assertion_verification_key, enrollment}` and `internal/store/store.go` records that the assertion key *replaced* that secret. Separately, `STORAGE.md`'s layout diagram shows `brain/state.db` and `managed-services/` where `cmd/brain/main.go` opens `<stateDir>/malmo.db` and `internal/lifecycle/services.go` writes under `<stateDir>/services/`. Both are worth fixing on their own.

2. **`data_volumes` / `cache_volumes` are authored but unparsed**, and `APP_MANIFEST.md` states nesting/overlap rules nothing enforces. This blocks more than backup: `UPDATES.md` # Pre-update snapshot and `APP_LIFECYCLE.md` # Update transaction spec a pre-update tar of declared `data_volumes` as the **v1 rollback safety net** for app-driven schema migrations — a locked decision (`DECISIONS.md` 2026-05-17) that is unbuilt for exactly this missing field.

3. **Managed-service data lives outside the instance directories.** Any backup that copies only `/var/lib/malmo/instances/<id>/` restores a database-backed app with its config intact and its rows gone — the failure that looks like success until a restore.

4. **The hosted storage model is not the appliance's, and reading `STORAGE.md` first produces confident wrong answers.** `ENVIRONMENT.md` # Storage (hosted) removes mergerfs, the add/eject flows, and the storage canary, and puts everything on one volume — so there is no `/srv/malmo/{home,state,shared}` bind-mount tree there, and `/home` and `/var/lib/malmo` are real directories.

5. **An include list of user folders is unsafe.** `STORAGE.md` says users may rename, delete, and add folders, so enumerating the use-case folders silently omits anything a user creates. Any future backup work should subtract from roots rather than enumerate paths.

6. **Restoring the brain's SQLite cannot be a file copy.** `box_meta` holds box-scoped identity and `cmd/brain/main.go` ignores the seed entirely when a `box_id` is already present — so a wholesale restore makes a fresh box adopt the destroyed box's identity, and it fails silently until the first certificate operation.

7. **Password hashes are not in any backup set by default.** PAM is the source of truth and the brain holds no hash, so a restore without `/etc/shadow` brings back accounts nobody can log into. Hosted ships no Samba, so `tdbsam` is an appliance-only concern.
