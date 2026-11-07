# Move an app's managed database to a new major version during an update

- **Status:** done
- **Date:** 2026-11-07
- **Specs touched:** `UPDATES.md` (new # 4 As built (#635), # As built (#631) The classes, # As built (#633), # Update mechanics per app steps 4 and 7, # 7 Rollback summary), `SERVICE_PROVISIONING.md` (# Catalog, # Provisioning protocol status, # At app update, different major version), `BRAIN_UI_PROTOCOL.md` (the `status` values), `NEXT.md` (# Managed-service lifecycle gaps), `DECISIONS.md` (2026-11-07, the old database stays 7 days), `docs/architecture.md`, `CLAUDE.md` (`update_class`)

Closes #635, step 6 and the last child of #630. [update-results.md](update-results.md) (#634) added the rollback by hand for 7 days. Until now an update whose new version asks for a newer major of a managed database (Postgres 15 to 18, for one) was classified `service_major` and never applied, and the app's page said moose could not move the data yet. New manifests should declare Postgres 18 (`SERVICE_PROVISIONING.md` # Catalog), so this was common. This moves the data as part of the update, as `SERVICE_PROVISIONING.md` # At app update, different major version designs, and keeps the old database for the rollback.

## What was done

- **The move** (`internal/lifecycle/servicemove.go`). Inside the #631 transaction, after the snapshot and with the old version stopped, as the first part of writing the new generation (step `moving_databases`):
  1. Start the new major's shared instance if it is not running (`ensureServiceInstance`, lazy spinup).
  2. Make the app's role (Postgres) or user (MySQL, MariaDB) on it, with the same name and password.
  3. Make the database with the same name and fill it from the snapshot's dump, with the new major's own `pg_restore` or client (`restoreDatabase`, the same as a snapshot restore). It runs through the one-shot client container, so the brain never joins a service network.
  4. Point the grant at the new major and keep the old one as retired, in one store write (`store.MoveServiceGrant`). The `.env` the update writes next names the new host.
- **Why the same names.** A MySQL-family dump names its user in the `DEFINER` of each view, routine and trigger. A Postgres dump makes the role the owner of each object. With the same names the dump restores as it is, the way the same-major restore already does.
- **A name another grant holds on the new major is refused** (`store.ServiceNamesTaken`, over the current and the retired grants of every instance). It can only happen for two installs of the same app whose random suffixes match (1 in 65,536). The update fails with a plain sentence and rolls back. The record marks a move as started only after this check, and a rollback drops a database on the new major only for a started move, so the move never drops another app's database.
- **Free space.** The new database takes about as much room as the old one, which stays. The move measures the old database (`dumpEstimate`) and refuses to start when the drive would be more than 95% full, with a plain sentence.
- **Refused when classified** (`serviceMoves`, called by `classifyUpdate`; class `blocked`): another engine ("This version keeps its data in MariaDB, and the app has it in MySQL. moose cannot move the data from one database engine to another, so it does not apply this version"), an older major ("This version needs an older Postgres (15) than the app has (16)..."), and a Valkey version change. Versions are compared part by part as numbers (`compareVersions`), so 10.11 is older than 11.4.
- **The rollback** (`moveServicesBack`, the first step of `undoUpdate`). It drops the database on the new major and points the grant back (`store.MoveServiceGrantBack`), then the snapshot is restored as for any failed update. The old database was never changed. `update.json` lists each move (`MovedServices`) before anything is made on the new major, so the startup pass after a crash does the same.
- **The old database stays for 7 days** (`service_grants_retired`, a new table, additive). The spec dropped it at once (step 5). That would break the rollback by hand of #634, so it now goes with the 7-day cleanup, and the spec line says so (`DECISIONS.md` 2026-11-07).
  - **The rollback by hand** reads the moves from `update.prev.json`, refuses ("no earlier version") when an old database is gone, and otherwise does the same undo: drop on the new major, point back, restore the snapshot into the old database, start the old version.
  - **The prune** (`pruneRetiredGrants`, called by `PruneSnapshots` for each instance it locks) drops an old database 7 days after the move, or as soon as the instance has no earlier version to roll back to.
  - **Uninstall** drops the old databases with the current ones.
- **No more `service_major` class.** An update that moves a database and asks for no more access is `available`, and the nightly window applies it like any other. One that also asks for more access waits for the owner's yes (#633), which closes #639's known gap. The store constant stays, because rows written by an older brain hold it: `CheckUpdates` classifies such a row again. The API enum keeps the value for the same rows.

## How it maps to the specs

- `SERVICE_PROVISIONING.md` # At app update, different major version: steps 1 to 4 are built. Step 1's backup is the pre-update snapshot's dump. Step 5 now keeps the old database for 7 days.
- `UPDATES.md` # 4 Update mechanics per app, step 4 (spin up the new major and restore) and step 7 (on failure, back to the previous major and its data): built.
- `UPDATES.md` # 4 Auto-apply unless permissions expand: "cross-major managed-service migrations are infrastructure, not user-facing trust events". A move asks nothing of the owner.
- `UPDATES.md` # 7 Rollback summary, the managed service major migration row: automatic on a failed update, and by hand for 7 days.

## Design choices the specs did not settle

- **Same names and password on the new major**, for the dump reasons above, and so pointing back is one store write.
- **A taken name is refused, not renamed.** Renaming would break every `DEFINER` in a MySQL dump. The chance is small, and the refusal changes nothing.
- **The old database is dropped early** when the instance has no earlier version to roll back to (for one, after a rollback by hand to a later version, or once the previous generation is pruned). Nothing can use it then.
- **Valkey version changes are refused.** Valkey has one version, and its data is a shared keyspace with no per-app dump.

## Tests

- `internal/store` (`updates_test.go`): `TestMoveServiceGrant` (the move, the retired row, a missing grant, the move back twice, the taken check on both versions, the delete, the cascade).
- `internal/lifecycle` with the fakes (`servicemove_test.go`): `TestUpdateMovesTheDatabase` (the class is `available`, the step, the grant on 17 with the same names, the retired 16 row, the `.env` host, the role made and the 16 dump fed to `pg_restore` on 17, nothing dropped on 16), `TestUpdateAfterTheMoveRollsBack`, `TestUpdateMoveRestoreFails` (the plain sentence, the old version up again), `TestUpdateMoveRefusesATakenName` (no `DROP` on 17), `TestRollbackByHandAfterAMove`, `TestRollbackByHandNeedsTheOldDatabase`, `TestReconcileMovesTheDatabaseBack`, `TestPruneDropsTheOldDatabaseAfter7Days`, `TestUninstallDropsTheOldDatabase`, `TestCheckClassifiesALegacyServiceMajor`, `TestCompareVersions`. `TestUpdateRefusals` lost its two `service_major` cases and gained an older major, an older major that also asks for more, an engine change, and a newer major that asks for more (now `needs_approval`). `TestClassifyUpdateWithApproval` changed the same way. No test was removed.
- **Real Docker** (`servicemove_live_test.go`, build tag `dockerlive`): `go test ./internal/lifecycle/ -tags dockerlive -run TestLiveMove -v -timeout 1800s`. `TestLiveMovePostgres` (15 to 18), `TestLiveMoveMySQL` (8.0 to 8.4) and `TestLiveMoveMariaDB` (10.11 to 11.4) all pass. Each one: 1.0 on the old major with rows, plus a `pgcrypto` extension and a serial (Postgres) or a view and a trigger whose `DEFINER` is the app's user (MySQL family). A 2.0 on the new major that never becomes healthy leaves 1.0 on the old major with its rows and nothing of it on the new one. A good 2.1 runs on the new major (checked with `version()`) with every row, the extension works, the serial and the trigger go on, and the old database is still there. The rollback by hand puts 1.0 back on the old major and drops the new database. 2.1 again, then the prune 7 days later drops the old database while 2.1 keeps its rows. The earlier live tests (`TestLiveRollback`, `TestLiveUpdate`, `TestLiveApproved`, `TestLiveAutoUpdateTurnsInternetOff`, `TestLiveSnapshot`) pass too.
- `make check`: see the PR. The known failures in `internal/hostagent/osupdate` (`TestTrialGood`, `TestOneSwitchPerNight`) are on `dev` too, and this branch does not touch that package.

## Known gaps & deviations

- **"In `update_failed`"** in the issue's Done when: a failed update whose rollback works ends `running` on the old version with the failure recorded, as for every update since #631 (`DECISIONS.md` 2026-11-06). `update_failed` is only for a rollback that could not bring the old version back.
- **The old major keeps running** after its last app moved off. The grace shutdown is not built (`NEXT.md` # Managed-service lifecycle gaps).
- **No whole-box proof.** The real-Docker tests ran without the socket proxy and with no remap, like #631's. The new major's data dir gets the remap owner through the same `ensureServiceInstance` as an install.
- **The free-space check is an estimate**: the database's size on the old major. A restore that fills the drive fails and rolls back.
- **A move that failed half way can leave the new major's role behind** when its drop fails during the rollback. It is logged, holds no data the app uses, and the next move of the same app drops it first.

## What's next

- #630's own Done when: the OS-and-app night in the QEMU lane, and a check on a running box that a newer catalog version is applied in the window.
