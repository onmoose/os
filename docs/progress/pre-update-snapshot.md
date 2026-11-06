# The pre-update snapshot, its restore and the 7-day cleanup

- **Status:** done
- **Date:** 2026-11-06
- **Specs touched:** `UPDATES.md` # Pre-update snapshot (new # As built), `APP_LIFECYCLE.md` # on-disk layout per instance and # update + rollback, `APP_MANIFEST.md` # C, `SERVICE_PROVISIONING.md` (implementation status, # At app update), `docs/architecture.md`, `docs/dev/authoring-apps-with-an-agent.md` step 6, `CLAUDE.md` (the `snapshot` log field)

Closes #347, step 1 of the app-update work in #630. `UPDATES.md` promises that a failed app update brings back the previous version, and `DECISIONS.md` 2026-05-17 says that promise needs the app's data back too, not only its image. This builds that safety net: a snapshot of an app's data and of its managed databases, a restore that puts both back, and a cleanup after 7 days. The update transaction that calls them is #631. The managed-service side builds on [managed-services-postgres.md](managed-services-postgres.md), [managed-services-mysql.md](managed-services-mysql.md) and [managed-services-redis.md](managed-services-redis.md).

## What was done

- **The manifest models `storage.data_volumes` and `storage.cache_volumes`** (`manifest.Storage`). `Parse` keeps them as written. `manifest.CleanStoragePath` is the one place a path is read: relative to the instance dir, cleaned, and refused when it is empty, absolute, uses a variable, leaves the app's folder, is the whole folder, or names a file the brain keeps there (`manifest.yml`, `compose.yml`, `compose.override.yml`, `compose.override.yml.prev`, `.env`, `snapshots`).
- **Admission checks them** (`admission.StoragePaths`, called from `admission.CheckManifest`, so the install and `moose manifest check` both refuse a bad list). A cache path may sit inside a data path. Every other overlap is refused, with a message that names the field and the entry: the same path twice, a data path inside a data path, a cache inside a cache, a data path inside a cache, a path in both lists.
- **`Manager.TakeSnapshot`** (`internal/lifecycle/snapshot.go`) writes, in `instances/<id>/snapshots/`, `pre-update-<version>.tar` (the data volumes minus the cache volumes inside them), one dump per Postgres, MySQL or MariaDB grant (`.<service>.pgdump` or `.<service>.sql`), and `pre-update-<version>.json` last, as the mark that the set is complete. The tar keeps numeric owners, modes and modification times, keeps symlinks as symlinks, and refuses a path whose parent folder is a symlink. A manifest with no `data_volumes` gets the best effort: all of `data/` plus every other relative bind dir of the compose. Before it writes, it re-checks the stored manifest's lists with `admission.StoragePaths`, so an instance installed before this rule is checked too.
- **The dumps** run through the existing one-shot client container, with a new `DockerDriver.RunOneOffStream` (`docker run --rm [-i]`) that streams the container's stdout to the file and the file back into its stdin on restore. The brain never joins a service network, and no bind mount is needed. Postgres: `pg_dump --format=custom`. MySQL and MariaDB: `mysqldump` / `mariadb-dump --single-transaction --routines --triggers --events --no-tablespaces --hex-blob`. `toolWrapper` in `services.go` is `clientWrapper` for these tools.
- **Valkey is skipped.** It has one keyspace shared by every app and no per-app database, so a per-app dump is not possible. The grant is listed under `skipped` in the record.
- **Failures are returned, and a half-written set is removed first.** A dump that fails, a write that fails, or a drive that would be more than 95% full after the tar (`ErrSnapshotNoSpace`) all come back to the caller, so #631 can refuse the update.
- **`Manager.RestoreSnapshot`** reads the record, checks every file and grant before it changes anything, empties each data volume except the cache paths inside it (never following a symlink), unpacks the tar with the owners, modes and times it holds, removes a data volume that did not exist at the snapshot, then drops, re-creates and refills each database from its dump. It can run again after a failure.
- **`Manager.PruneSnapshots`** removes sets whose record is older than 7 days, and any loose snapshot file that old that no complete set names. It skips an instance whose lock is held. `cmd/brain` runs it once at startup and then every hour (`snapshotPruneLoop`).
- **`statDiskSpace`** reads `statfs` on unix (`diskspace_unix.go`), with a build-tagged stub elsewhere, behind a `Manager.diskSpace` seam the tests set.

## How it maps to the specs

- `UPDATES.md` # Pre-update snapshot steps 1 to 3 are built, with the file names, the dump formats, Valkey, the space rule and the cleanup written down in the new # As built part.
- `APP_LIFECYCLE.md` # update + rollback step 4 and the restore on failure are built as functions; the transaction around them is #631.
- `APP_MANIFEST.md` # C: the nesting rule ("a cache may nest inside data, nothing else may overlap") is enforced, and the old note that v1 parses neither list is replaced.
- `APP_MANIFEST.md` # Custom container: the Door-2 best effort is the fallback, without writing a `data_volumes` list into the synthetic manifest.
- `DECISIONS.md` 2026-05-17: brute force, byte level, no hooks. Nothing assumes hooks exist.

## Design choices the specs did not settle

- **The app is stopped while the snapshot is taken.** The specs list the snapshot before the old container stops (`UPDATES.md` # 4 step 2, `APP_LIFECYCLE.md` step 4). A tar of files a running app writes is not consistent, so the functions expect the caller to have stopped the app. The specs now say so, and #631 must order its steps this way.
- **An empty `data_volumes: []` gets the same fallback as an absent one.** Some catalog apps write `data_volumes: []` (excalidraw, flipt). For a safety net, keeping more is the safe side, and the stored manifest drops an empty list anyway (`omitempty`).
- **The fallback also keeps bind dirs outside `data/`.** The spec says "everything under `data/`", but real composes bind `./config` and `./uploads` next to it (jotty, listmonk), and the synthetic manifest's own sketch is "all volumes from the compose".
- **Paths are relative to the instance dir, not to `data/`**, the way the compose writes its binds. Two catalog apps declare `./config` and `./uploads`, and requiring `./data/…` would have refused them.
- **A record file (`.json`) marks a complete set.** Restore needs to know what was kept without trusting whichever manifest is on disk by then, and a crash must never leave a set that looks complete.
- **The dump streams over stdout and stdin**, not through a bind mount, so the remapped client never needs to read or write the brain's `0600` files.
- **The space rule is the `disk-full` threshold (95%).** The tar size is measured ahead; the dumps are not, and a dump that fills the disk fails like any other dump.
- **Hourly cleanup**, once at startup too, as a plain loop like the notification prune. Hourly keeps a set at most an hour past its 7 days, and a pass reads one small folder per app.
- **Two sentinel errors**, `ErrSnapshotNoSpace` and `ErrNoSnapshot`, because #631 must tell "the drive is full" (the update is deferred and the user is told) from other failures, and #634 must tell "nothing to roll back to".

## Tests

- `internal/manifest`: `TestParseStorageVolumes` (both YAML list shapes, the round trip the brain makes into the instance dir), `TestCleanStoragePath`.
- `internal/admission`: `TestStoragePaths` (19 cases, each refusal also through `CheckManifest`).
- `cmd/moose`: two new rows in `TestCheck_RejectsManifestRules`.
- `internal/lifecycle` with the fakes (`snapshot_test.go`): data minus cache, then a restore after a changed, an added and a removed file (mode, time, symlink, owner through the chown recorder, a second restore); the fallback; a data volume absent at the snapshot; the Postgres dump and restore and the Valkey skip; a failed dump leaves nothing and restore then says `ErrNoSnapshot`; a full drive; symlinks (a linked parent refused, a link inside kept and not followed); a bad stored storage list; the prune (old set, loose file, locked instance); `isDumpOf`. `TestSnapshotRestoreKeepsOwnersAsRoot` gives files uids in the remap range and checks the restore puts them back, with setuid; it skips when not root.
- **Real Docker** (`snapshot_live_test.go`, build tag `dockerlive`): `go test ./internal/lifecycle/ -tags dockerlive -run TestLiveSnapshot -v -timeout 900s`. For Postgres 16, MySQL 8.0 and MariaDB 11.4: install a real app with a real managed database, write rows and files, stop the app, snapshot, then migrate (add a column, rewrite rows, add a table, change and add files, write the cache), restore, and check the rows, the column count, the table count, the files and the untouched cache, then start the app again. All three passed.
- **As root**: the lifecycle test binary, built with `CGO_ENABLED=0 go test -c`, was run in an `alpine` container as root (`-test.run 'Snapshot|Prune|IsDumpOf'`). All 11 passed, including `TestSnapshotRestoreKeepsOwnersAsRoot`.
- Every catalog manifest in a local copy of `onmoose/store` (82, of which 74 declare storage lists) passes the new `CheckManifest`, through a throwaway test that was not committed.
- `make fmt-check`, `make vet` and `make openapi-check` pass. The Go suite passes except two packages that fail the same way on a clean `dev` on this machine: `internal/hostagent/osupdate` (`TestTrialGood`, `TestOneSwitchPerNight`) and the PAM packages (old `libpam0g-dev` headers, `C.RTLD_NEXT`). No test was removed.

## Known gaps & deviations

- **No update takes a snapshot yet.** The functions have no caller but the cleanup; #631 builds the transaction.
- **Valkey data is not protected.** An app that keeps data it cannot lose only in Valkey loses whatever an update writes there. Apps use it for caches, sessions and queues, so this is the expected case, but it is not a guarantee.
- **Removing the previous image after 7 days is not built.** No previous generation exists until #631 keeps one; the image side of the cleanup belongs with #631 and #634.
- **The streamed dump was tried through the docker-socket proxy by hand, not in a test.** `tecnativa/docker-socket-proxy:v0.4.2` with the brain's allowlist (`brainlaunch`) passed 20 MB through `docker run --rm -i alpine cat` unchanged (same sha256 in and out), and still refused `docker exec` with a 403. No automated test runs the dump through the proxy; the hosted lane in CI will be the first one once #631 calls it.
- **Not kept by the tar:** extended attributes and ACLs, hard links (they come back as copies), sockets, devices and pipes (left out and logged), and symlink times.
- **The dump size is not checked ahead.** A dump that fills the drive fails its write and the set is removed, but the drive was full for a moment.
- **A restore that fails after dropping a database leaves the database empty** until the restore is run again. The dump is untouched, so a second run puts it back.
- **Instances installed before this change** have no storage lists in their stored manifest (the brain re-wrote it from a struct that did not model them), so their first snapshot is the best effort over all bind dirs.

## What's next

- #631: the update transaction calls `TakeSnapshot` after it stops the old version, refuses the update on any error (`ErrSnapshotNoSpace` gets its own message), and calls `RestoreSnapshot` on a failed update before it brings the old version back.
- #634: the rollback button reads the kept set within its 7 days, and the previous-image cleanup lands with the kept generation.
