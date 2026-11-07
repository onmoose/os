# The app-update cloud boot: app updates proven on a booted box

- **Status:** done
- **Date:** 2026-11-07
- **Specs touched:** `TESTING.md` (# Hosted cloud variant, the new app-update boot), `docs/dev/hosted-boot-proof.md` (the scenario table, # The app-update boot, # How to run it)

Closes #630, the parent of six child issues that all merged: [pre-update-snapshot.md](pre-update-snapshot.md) (#347), [app-update.md](app-update.md) (#631), [nightly-app-updates.md](nightly-app-updates.md) (#632), [update-approval.md](update-approval.md) (#633), [update-results.md](update-results.md) (#634) and [service-major-move.md](service-major-move.md) (#635). Each of them listed the same gap: no whole-box proof. Their real-Docker tests ran without the socket proxy and without the userns remap, and the QEMU lane showed only the OS hold, not a whole night. [service-major-move.md](service-major-move.md) left #630's own Done when as the next step: the OS-and-app night in the QEMU lane, and a check on a running box that a newer catalog version is applied in the window. This entry adds that proof as a new boot of the hosted cloud lane, `app-update`. It changes no product code.

## What was done

- **A test app with five versions**, `upnote` (`dev/cloud/test/catalog/upnote` for 1.0, `dev/cloud/test/catalog-updates/upnote-<version>` for the rest). It is a folderless app on the managed Postgres 16, on the `postgres:16` image the remap boot already bakes. Each version adds the row `row-<version>` to its database once, and its version to `data/starts.txt` at every start, so the boot can read which versions ran on which data. 3.0 reports unhealthy after a 20 s start period, so it writes first and then fails. 3.1 asks for `internet`. All five pass `moose manifest check`.
- **One catalog file per version** (`dev/cloud/test/bootstrap.sh`). The seed holds 1.0. `mkcatalog` writes `/var/lib/moose/test-catalogs/upnote-<version>.json` for each later version. A new version reaches the box when the boot copies one of them over `catalog-seed.json` and restarts the brain: the brain reads that file only at startup, and checks for updates there (`cmd/brain/main.go`). The update loads the new version from the file (the documents are inline) and resolves its image with the offline fallback, like an install.
- **The boot** (`dev/cloud/cloud-assertions.sh`, mode `app-update`). It runs through the harness's `os_boot` (`dev/cloud/run-cloud-tests.sh`): its own overlay and box-id (`lark-cedar`), a test-portal owner assertion, the OS test bundle disk, and a 45-minute verdict limit. Stage 1, on slot A:
  - **Setup.** The in-guest update source from the OS boots serves an answer with a window 12 hours away, so nothing updates on its own whatever the hour of the run. With the OS bundle in the answer, host-agent installs it into slot B ahead of the window, as on a real night. The owner installs upnote 1.0 and turns its automatic updates off.
  - **A, an update by hand.** 1.0 to 2.0 is classified `available`. After Update, 2.0 runs with both rows and both lines, still remapped, and `snapshots/pre-update-1.0` holds the tar, the dump and the record.
  - **B1, a broken update.** While the update to 3.0 runs, the boot watches 3.0 write its row and its line. The update must then fail, and the box ends on 2.0 with neither of them, and with the failure recorded. This is the snapshot restore of the files and of the database, through the socket proxy.
  - **B2, an update that asks for more.** 3.1 is classified `needs_approval`. A plain Update is refused, the owner gets the "3.1 wants new access" notification, and after the yes (`update/approve`) 3.1 runs.
  - **B3, Go back.** `can_rollback` is offered. After it, the app is on 2.0 with the data from just before 3.1 (no `row-3.1`), and its automatic updates are off.
  - **C, the night.** The answer gets a window that opens in about six minutes. The catalog moves to 4.0, and the owner turns automatic updates on. The brain must hold the OS switch before the window opens (the read shows `hold_until`). Without the hold, host-agent's first tick in the window would switch at once, because slot B is installed. The window then updates the app to 4.0, and only after that does the box switch to slot B and reboot.
- **Stage 2, on slot B.** host-agent marks the slot good, the owner's session still works, upnote 4.0 runs on rows 1.0, 2.0 and 4.0, the app says `updated_overnight` from 2.0, and the owner has the "updated to 4.0" notification. An app update cut by the reboot would be put back to 2.0 by the startup pass, so this stage also proves the order.
- **On a run that publishes the OS** (`os_mode=refuse`), RAUC refuses the test bundle. The answer then has no OS part, and stage 1 ends after the night: the app is on 4.0 and the hold is cleared. An answer with only a version and a window goes through host-agent's refused control-plane path, which still settles the night, so the brain's wait for the control plane ends as on the OS boots.
- **CI** (`.github/workflows/ci-cloud-image.yml`). `app-update` is in the full list, so every release and every full manual run has it, including a run that publishes the OS. The PR path filter now also has the app update code (`internal/lifecycle/{update,nightly,snapshot,rollback,servicemove,pinning}*.go`, `internal/api/appupdate*.go`). A PR that touches those files, host-agent's update target or hold, the upnote catalogs, or the two lane scripts runs `update app-update`. Any other PR still runs only `update`. The boot gets the OS test bundle, so a run with `app-update` builds it.

## How it maps to the specs

- `UPDATES.md` # 4 Update mechanics per app: steps 1 to 7 on a booted box, with the socket proxy and the remap.
- `UPDATES.md` # Pre-update snapshot: taken before the update, and restored for the failed one, files and database.
- `UPDATES.md` # 4 Auto-apply unless permissions expand, and # Who gets prompted: an update that asks for `internet` waits for the owner.
- `UPDATES.md` # 7 Rollback summary: automatic on failure, and by hand.
- `UPDATES.md` # 4 Update window and # 7 Reboots: the app update runs in the window and the OS switch waits for it.
- `TESTING.md` # Hosted cloud variant: one more boot, on its own overlay like the OS update boots.

## Known gaps & deviations

- **Three failures in a row and the pause are not in the boot.** They need three nights. The lifecycle tests cover them.
- **No database major move.** It needs a second Postgres image baked in, which makes the boot-proof image bigger. The snapshot dump and restore already go through the socket proxy here, and the move itself was proven on real Docker in [service-major-move.md](service-major-move.md).
- **The night's failure notification is not in the boot.** Phase B's failure is a person's Update, which is not told (`UPDATES.md` # As built (#634)). The nightly loop's own failure path is covered by the lifecycle tests.
- **The dashboard is not in the boot.** The approval modal, Go back, the banner and the toast are only type-checked and built. Testing them in a browser is still to do.
- **A new catalog version comes from a brain restart**, not a catalog sync. The lane has no catalog service, and the brain reads the staged file only at startup. The update check runs at startup too, so the classification is the same code a sync runs.
- **The boot waits about six minutes for its window**, so it is the longest boot of the lane.
- **The PR rule runs `app-update` for any change to `cloud-assertions.sh` or `run-cloud-tests.sh`**, also when the change is for another boot. That costs a parallel job, not wall time, and it means a change to this boot is always run.
- **Results:** see the PR. The first CI run with only this boot is [run 42387076736](https://github.com/onmoose/os/actions/runs/42387076736).

## What's next

- Test the update screens in a browser with `make dev`: the approval modal, Allow & update, Go back, the banner after a failure, and the overnight toast.
