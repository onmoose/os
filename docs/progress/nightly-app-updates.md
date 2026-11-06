# Run app updates in the nightly window and hold the OS switch until they finish

- **Status:** done
- **Date:** 2026-11-06
- **Specs touched:** `UPDATES.md` (# 4 Update window and its new # As built (#632), # Per-app auto-update toggle, # 6 Failure signaling, # 7 Reboots, # 8.4), `BRAIN_HOST_PROTOCOL.md` (the two new read fields, the hold route, Update now under a hold), `BRAIN_UI_PROTOCOL.md` (`update.paused`, `update.failed_count`, `hold_until`), `DASHBOARD.md` (# Tile), `TESTING.md` (the `os-update` boot), `docs/architecture.md`, `docs/dev/hosted-boot-proof.md`, `CLAUDE.md` (the `until` and `night` log fields)

Closes #632, step 3 of the app-update work in #630. [app-update.md](app-update.md) (#631) built the update transaction, the classes and the per-app toggle, with nothing that updates an app on its own. This adds the nightly window: the brain updates the apps whose update it may apply, one at a time, after the control plane and before the OS switch. Since the OS switch reboots the box and only host-agent can start it, the brain now holds it with a lease while it has app work.

## What was done

- **The hold on host-agent** (`internal/hostagent/updatetarget/hold.go`). `Loop.SetHold` and `Loop.ClearHold`, served as `PUT` and `DELETE /v1/system/update-hold` (`internal/hostagent/updatehold.go`, wired in `cmd/host-agent-real`). host-agent cuts every hold to at most 10 minutes from now (`protocol.UpdateHoldLease`) and to the window's end minus 5 minutes (`protocol.UpdateHoldCapBeforeEnd`), and keeps it in memory. While it is live, `tickOS` passes the window as shut to the OS applier, so the install still runs and the switch waits, and `ApplyNow` puts an OS switch on the same waiting path as a switch behind stream B (`manualOS`, retried by `manualStep`). An OS-only Update now under a hold answers `202` with an empty job id. Clearing a hold wakes the loop, and while a hold is live `Run` sleeps no longer than until it ends or the window opens.
- **Stream B done for the night.** `Loop.ControlPlaneDone` says the night stream B is finished for: the window is open, the last tick ran inside this window, and nothing more will happen tonight (nothing to do, the appliance, or tonight's attempt is over and its job no longer runs; `Applier.JobRunning` reads the job). The read carries it as `control_plane_done_night`, always sent, `""` included, and carries `hold_until`. The OS detail under a hold says "the switch waits for app updates to finish".
- **A late control-plane target waits.** When stream B was already done tonight and a new target shows up while the brain holds the window, the loop leaves it for the next night (`settled`), because the control-plane update restarts the brain in the middle of an app update. Only a tick whose source answered settles the night, so a blip (unreachable, refused, no target) does not push a good target to the next night.
- **No hold, no app update.** A hold call that fails this time (a socket error, a 500) makes the brain wait a minute and try again. Only `hostclient.ErrHoldUnsupported` (404 or 405 from a host-agent before #632, 501 `hold-unsupported` from one with no update loop) lets the apps go without a hold.
- **The fake host-agent** keeps and reports a hold with the same cuts, reports `control_plane_done_night`, names a zone (UTC when the dev machine has no name for its own), and takes its window from `MOOSE_FAKE_UPDATE_WINDOW`.
- **The store** (`internal/store/updates.go`). Additive columns `update_tried_version`, `update_tried_night` and `update_failed_count`, all "never tried" by default. `RecordInstanceUpdateFailure` counts a failure in a row on the same version in one SQL statement; another version starts at 1; `FinishInstanceUpdate` and `ResetInstanceUpdateFailCount` set it to 0. `Instance.UpdatePaused` is true at 3 failures on the version the box offers.
- **The brain's loop** (`internal/lifecycle/nightly.go`, started from `cmd/brain/main.go`). It wakes every minute, reads the window and the zone from the read (with `time/tzdata` built into the brain), and falls back to 03:00-04:00 and UTC with one warning when either is missing. A night is the date its window opens on, built on the wall clock. From 15 minutes before the window, when any app has work, it renews the hold every minute, also during a long update. It runs `CheckUpdates` once per night, waits for `control_plane_done_night` (or, with an older host-agent, a read checked inside tonight's window with no control-plane update waiting), and updates one app at a time, oldest install first, through `Manager.Update`, writing the try first. No update starts in the last 20 minutes. It clears the hold as soon as no work is left, and at the cut-off.
- **A person's Update** goes through `Manager.UpdateByPerson`, which sets the count back to 0 first; it works on a paused app.
- **The API and the dashboard.** The app DTO gains `update.paused` and `update.failed_count`; the update-target DTO gains `hold_until`. The tile's update link reads "Update is failing repeatedly. Paused." and the app's settings page says the same under the last failure. OpenAPI and the TS client are regenerated.
- **The QEMU lane.** The `os-update` boot sets a hold through host-agent's socket before its Update now, checks for 35 s that the switch does not happen, that the read shows `hold_until` and the waiting detail, then clears the hold and lets the switch and the reboot go.

## How it maps to the specs

- `UPDATES.md` # 4 Update window: one window, one app at a time, the cut-off, one try per version per night, the pause at 3 and the reset by a person's Update are built and written down, with an As built (#632) note.
- `UPDATES.md` # 6 Failure signaling: the paused sentence is shown as built.
- `UPDATES.md` # 7 Update ordering and Reboots: control plane, then apps, then the OS switch, with the hand-off written as built: the lease, the cap, the wake, Update now waiting.
- `UPDATES.md` # 8.4: the new read field.
- `BRAIN_HOST_PROTOCOL.md`: the hold route and the two read fields.

## Design choices the specs did not settle

These came from the maintainer's brief for this issue and are written into the specs.

- **The cut-off is the window's end minus 20 minutes**, not 03:40: the window can come from the control plane or an env var.
- **The hold is a lease renewed every minute**, not one expiry, so the issue's "the cut-off plus the longest single update" needs no guess at how long one update takes. A live brain keeps the switch waiting while it works; a dead one lets it go within 10 minutes; nothing reaches past the cap.
- **The brain reads the zone from the read**, not from its own `time.Local`, which in its container is not the box's.
- **A new read field says when stream B is done**, because `available` looks the same before, during and after tonight's attempt.
- **Update now waits for the hold** rather than refusing, as # 7 says the switch comes after stream B.

And three of my own, small:

- **A control-plane target that shows up after stream B was done waits for the next night while the brain holds the window.** Otherwise the control-plane update would restart the brain in the middle of an app update and turn it into an `interrupted` failure. Without a hold it applies as before.
- **host-agent also cuts a hold to the lease**, not only to the cap, so a brain bug that asks for an hour cannot keep a dead brain's hold alive for an hour.
- **The brain's update-target DTO carries `hold_until`**, so an admin can see the hold through the dashboard's own read and the QEMU boot can check it.

No `DECISIONS.md` entry: no locked decision flips. The lease refines how # 7's "an app that is mid-update holds the reboot" is built.

## Tests

- `internal/hostagent/updatetarget` (`hold_test.go`): `TestHold_BlipDoesNotSettleTheNight`, `TestHold_KeepsTheSwitchButNotTheInstall`, `TestHold_ExpiredLetsItSwitch`, `TestHold_ClearWakesTheLoop` (through `Run`), `TestHold_IsCutToTheLeaseAndTheCap`, `TestHoldCap` (a wrapping window, outside the window), `TestApplyNow_WaitsForTheHold` (cleared and expired), `TestControlPlaneDone` (current, no target, unreachable, appliance, an update running then failed, not yet checked in this window, outside the window, checked last night), `TestHold_LateTargetWaitsWhileAppsUpdate` (with and without a hold), `TestHoldWake`.
- `internal/hostagent` (`updatehold_test.go`): `TestUpdateHold_PutSetsAndAnswersTheKeptEnd`, `TestUpdateHold_BadUntil_400`, `TestUpdateHold_Delete_204`, `TestUpdateHold_NoHolder_501`, `TestApplyTarget_SwitchWaitingForTheHold_202WithNoJob`, `TestJobRunning`.
- `cmd/host-agent-real`: `TestReport_HoldAndControlPlaneDone`, `TestReport_NoLoopIsDoneInTheWindow`, `TestHoldRoutesReachTheLoop` (the routes reach the real loop and its cut), and a new case in `TestReportOS` (the installed release under a hold).
- `internal/hostclient` (`updatehold_test.go`): `TestUpdateHold`, `TestUpdateHold_Unsupported`.
- `internal/store`: `TestNightlyColumnsMigrateForAnOlderDatabase` (a database with #631's columns and none of these), `TestUpdateFailCount`.
- `internal/lifecycle` (`nightly_test.go`, a fake clock and a fake host-agent, no sleeps in the loop): `TestNightly_NothingOutsideTheWindow`, `TestNightly_HoldsFromAQuarterBeforeOnlyWithWork`, `TestNightly_WaitsForTheControlPlane`, `TestNightly_OneAtATimeInOrder`, `TestNightly_StopsAtTheCutoff`, `TestNightly_Skips` (toggle off, stopped, Door-2, needs approval, service major, tried tonight, paused), `TestNightly_OneTryPerNightAcrossARestart`, `TestNightly_ChecksTheCatalogOncePerNight`, `TestNightly_ZoneAndWindowFromTheRead` (and the fallbacks), `TestNightly_DSTNights` (Stockholm's clocks going back before the window, New York's inside a 01:00-03:00 window), `TestNightly_OldHostAgent`, `TestNightly_HostAgentProblems`, `TestNightly_RenewsTheHoldDuringAnUpdate`, `TestNightly_HoldIsCappedBeforeTheEnd`, `TestNightWindow`; with the real update transaction, `TestNightly_AppliesAnUpdate` and `TestNightly_PausesAfterThreeFailures` (three nights, the pause, a person's Update, a new catalog version).
- `internal/api`: `TestAppUpdateDTO_Paused`, `TestUpdateApp_PausedCanBeUpdatedAndResetsTheCount`, `TestUpdateTarget_HoldUntil`.
- `make fmt-check`, `make vet`, `make openapi-check`, `make check-web` and `make test-nopam` pass on this machine (the full run included `internal/hostagent/osupdate` and `cmd/host-agent-real`). `go test -race` passes for `internal/hostagent/updatetarget` and the nightly tests in `internal/lifecycle`.
- Existing tests: none removed or weakened. The two test appliers in `internal/hostagent/updatetarget` gained a `JobRunning` method, which the `Applier` interface now has.
- The QEMU change runs in the `os-update` boot: `gh workflow run "CI / Cloud image" --ref feat/632-nightly-app-updates -f publish=false -f boots="os-update"`. It was not run for this entry.

## Known gaps & deviations

- **The QEMU lane shows the hold, not a whole night.** It proves that a hold keeps the OS switch from happening and that clearing it lets the switch go, on a real booted box. A night with a real app update before the OS switch, in the guest, was too big for this change: it needs a catalog app with two versions inside the air-gapped guest and the window open for both streams. The lifecycle tests cover the brain's side of that night with fakes.
- **Update now for the control plane is not held.** An admin who presses Update now while an app updates at night starts the control-plane update at once; it restarts the brain and the app update ends as `interrupted` (one failure). Only the OS switch waits for the hold, as the brief asked.
- **An app update that runs past the cap can be cut by the reboot.** The cap is the window's end minus 5 minutes; the startup pass puts the old version back and it counts as a failure.
- **With a host-agent from before #632**, a control-plane update that failed tonight leaves the read `available`, so the brain waits until the cut-off and the apps skip that night. Nothing holds the OS switch either; the brain logs that once.
- **A read that fails all night** means no app updates that night.
- **A failure that is not recorded on the row** (the catalog could not be read, host-agent did not answer) does not count towards the pause, but still uses that night's try.
- **The hold lives in host-agent's memory.** A host-agent restart drops it until the brain's next renewal, up to a minute later.
- **No audit record and no notification** for an update the window made; the log lines carry the outcome. Notifications and Settings → Updates are #634.
- **The fake host-agent's Update now ignores the hold**; it only keeps and reports it.
- **Settings → System does not show the hold**; it is on the read for #634 to use.
- **The brain parses the window itself** (`nightWindow`), a small copy of host-agent's parser, because the brain does not import host-agent's packages.
- **`control_plane_done_night` is a pointer in `protocol.UpdateTarget`**, so the brain can tell a host-agent that sends `""` from one that sends nothing.
- **Not tried in a browser or against real Docker for the loop.** `make check-web` passed. The update transaction itself was tried against real Docker in #631.

## What's next

- #633: the owner approves a `needs_approval` update; the window then applies it like any other.
- #634: Settings → Updates, the notifications (an update the window applied, one that failed, one that is paused), the "See logs" link, and the hold on Settings → System.
- #635: a managed database's major move during an update.
- Show a whole night in the QEMU lane once the guest can serve a second version of a catalog app.
