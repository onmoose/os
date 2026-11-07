# Show what app updates did, tell the owner, and roll back by hand for 7 days

- **Status:** done
- **Date:** 2026-11-07
- **Specs touched:** `UPDATES.md` (# As built (#632), # As built (#347), # As built (#631), new # 4 As built (#634), # Per-app auto-update toggle, # 6 Where updates appear, The permission-expansion prompt, Failure signaling, Post-update toast, Notification center), `APP_LIFECYCLE.md` (# update + rollback), `NOTIFICATIONS.md` (# Updates), `BRAIN_UI_PROTOCOL.md` (the three routes and the new `update` and `/me` fields), `LOGGING.md` (`app.update_rollback`), `DECISIONS.md` (2026-11-07), `docs/architecture.md`

Closes #634, step 5 of the app-update work in #630. [update-approval.md](update-approval.md) (#633) let the owner allow an update that asks for more access. The box could now update apps on its own, but it said nothing about it outside the log, and an update that passed its health check and broke something the next day had no way back. This adds the way back (the rollback button for 7 days), the cleanup that ends it, and the four places that show what the updates did: Settings → Updates, the tile banner, the overnight toast and the notifications.

## What was done

- **The rollback by hand** (`internal/lifecycle/rollback.go`, `Manager.Rollback`). For 7 days after a good update it puts the app back on the version before, with the data from just before the update. It is the undo of a failed update (`undoUpdate`), run later from what the update kept: the snapshot (#347), the previous generation (#631), and the update's own record, which a good update now keeps as `update.prev.json` instead of removing it. That record says what the update added (managed services, secrets, config defaults, a remade network), so the rollback drops them again. The rollback writes `update.json` with `rollback: true` before it changes anything, so the startup pass (`recoverRollback`) and a Start from `update_failed` (`finishRollback`) finish it. It records no update failure. It works on a running, stopped or failed app and ends with it running, or `failed` when the old version does not start. After it, the update check runs once the lock is let go, so the app's page offers the catalog's version again.
- **After a rollback the automatic update is off** and the version it left is stored as declined (`store.FinishInstanceRollback`), so the window does not apply it again tonight and the prompt does not ask about it again. This was the maintainer's call (`DECISIONS.md` 2026-11-07).
- **A failed update keeps the way back.** #631's known gap: a failed 2.0 to 3.0 dropped the 1.0 generation. Now, before an update writes over the `.prev` files, it copies them and `update.prev.json` to `<name>.old` (`keepOlder`, `updateRecord.OlderKept`), and a failure puts them back with the previous pins (`endPrevious`, `putOlderBack`) and removes only the new version's images. A good update removes the copy (`keepGoodRecord`). This covers a failure before the stop, after it, and an update the startup pass undoes.
- **The cleanup** (`pruneRollbackPoint`, called by `PruneSnapshots` for each app it locks). Once the 7 days are over or the snapshot is gone, the hourly pass removes the previous generation (the `.prev` files, the record, the pins), its images when nothing else uses them, and the row's rollback point. The 7 days start at the snapshot's `created_at`, which the good update stores as `update_last_at`, so the snapshot, the files, the images and the button go at the same time. An app with `update.json` is skipped. A previous generation from a brain before #634 (no time on the row) goes by the age of `manifest.yml.prev`.
- **The store** (`internal/store`). Additive columns with defaults: on `instances`, `update_last_from`, `update_last_at`, `update_last_auto` (the rollback point and whether the window made the update), `update_failed_auto` and `update_failed_acked` (the failure came from the window; the banner was closed); on `users`, `updates_seen_at` (the toast). `FinishInstanceUpdate` takes the rollback point. New: `FinishInstanceRollback`, `ClearInstanceLastUpdate`, `MarkInstanceUpdateByWindow`, `AckInstanceUpdateFailure`, `UpdatesSeen`, `SetUpdatesSeen`. A new failure opens the banner again.
- **The notifications** (`internal/notify`). `AppUpdateApplied` (info), `AppUpdateFailed` (error: rolled back, the old version did not start again, or the third failure in a row as "update is paused") and `AppUpdateNeedsApproval` (warning), in the `updates` category, linking to Settings → Updates. A personal app's go to its owner, a household app's to the admins, never to everyone. One key per outcome per version (`app-update:<instance>:<kind>:<version>`), so a second failure of the same version refreshes its row. A good update resolves the failures and the wait for a yes of that version. Lifecycle calls them through a consumer-side `UpdateNotifier` (`nightly.go`); `cmd/brain` sets the notification center. The nightly report tells the window's outcomes and marks them as the window's; it tells a failure only when this update recorded it on the row. `setUpdateCheck` tells a version that newly waits for a yes.
- **The API** (`internal/api/appupdate.go`). `POST /api/v1/apps/{id}/rollback` (202, an `app-rollback` job; the Update gate: owner, or an admin for a household app; `409` with a plain sentence for nothing to go back to or a busy app; audited as `app.update_rollback`, success, every failure and every refusal). `POST /api/v1/apps/{id}/update/acknowledge` (204, the app-control gate, not audited). `POST /api/v1/me/updates-seen` (204). The `update` DTO gains `failed_overnight`, `failed_acked`, `updated_from`, `updated_at`, `updated_overnight`, `rollback_until` and `can_rollback`; `GET /me` gains `updates_seen_at`. OpenAPI and the TS client are regenerated.
- **The dashboard.**
  - **Settings → Updates** (`UpdatesSection.vue`, `/settings/updates`, in the "You" group): "X apps updated last night, Y waiting on you, Z failed.", then Updated recently (with Go back to <version>), Waiting for you (Allow & update, or Update for an app whose automatic update is off) and Did not update (the step in words, the message, the paused text, View logs). Drawn from the apps list, for the apps the person controls.
  - **The rollback button** (`RollbackButton.vue`), on Settings → Updates and on the app's settings page under Updates, with a confirm dialog that says the data saved since the update is lost and that moose then stops updating the app on its own.
  - **The tile banner**: "Photos couldn't update to 2.0 last night. Rolled back to 1.4.", or the paused text, until a controller closes it.
  - **The toast** (`UpdatesToast.vue` in `AppShell`): "3 apps updated overnight" with a link to the list, once per person, for the window's updates of their own and the household's apps in the last day.
  - **View logs** opens the app's logs: `#logs` on the app's settings page opens the Logs section.
  - The wording lives in one module (`updates.ts`). Activity names `app.update_rollback`. Toasts can carry a link.

## How it maps to the specs

- `UPDATES.md` # 4 Update mechanics per app: "keep the previous image and snapshot for 7 days ... we want the rollback button to actually work". Built, with the cleanup that ends it.
- `UPDATES.md` # 6 Where updates appear: Settings → Updates, the aggregate line and the rollback affordance.
- `UPDATES.md` # 6 Failure signaling: the tile banner until acknowledged, the list with the mode and View logs, the paused text.
- `UPDATES.md` # 6 Post-update toast and Notification center; `NOTIFICATIONS.md` # Updates: the three app rows, routed to the owner and to the admins for household apps.

## Design choices the specs did not settle

- **A rollback by hand turns the automatic update off** (`DECISIONS.md` 2026-11-07). The maintainer chose it over skipping only the rolled-back version.
- **The version left is stored as declined.** Without it, a rolled-back update that asked for more access would bring the prompt back at the next sign-in, right after the person went back.
- **The 7 days start at the snapshot's time**, not when the update finished, so the snapshot, the previous generation and the button share one clock. They are minutes apart.
- **"Box-wide" is the household scope.** That is who may approve and update such an app already (#631, #633).
- **Only the nightly window's outcomes are notified.** A person who presses Update, Allow & update or Go back sees the result on the page. The issue lists applied, failed and paused, which are the window's words.
- **The banner shows to controllers only** (the owner, or an admin for a household app), because only they can close it, and closing it is stored on the app. A member sees the household app's tile without it.
- **The toast counts the person's own personal apps and the household apps**, matching who gets the notifications, but also for members on household apps: it is a note, not an action.
- **The rollback has no elevation prompt.** It loses data like uninstall does, and uninstall has none; both use a confirm step.
- **View logs goes to the app's logs**, since the diagnostic bundle of `LOGGING.md` is not built.

## Tests

- `internal/store` (`updates_test.go`): `TestLastUpdateAndRollbackColumns`, `TestFailureByWindowAndAck`, `TestUpdatesSeen`, `TestLastUpdateColumnsMigrateOnAnOlderDatabase`. Three existing calls pass the new `FinishInstanceUpdate` argument.
- `internal/notify`: `TestAppUpdateRouting` (owner, admins, an ownerless app dropped, the keys, the resolves), `TestAppUpdateFailedWording`.
- `internal/lifecycle` with the fakes (`rollback_test.go`): `TestGoodUpdateKeepsARollbackPoint`, `TestRollbackByHand` (data, files, pins, images, route, toggle off, declined, the check run again, nothing more to go back to), `TestFailedUpdateKeepsTheOlderRollback` (1.0 to 2.0 good, 2.0 to 3.0 failed, then back to 1.0), `TestRollbackRefusals` (never updated, 7 days over, snapshot gone, busy), `TestRollbackOfAStoppedApp`, `TestRollbackRestoreFailsThenStartFinishesIt`, `TestReconcileFinishesAnInterruptedRollback`, `TestPruneRemovesTheRollbackPoint`, `TestPruneLeavesAnUnfinishedRollback`, `TestNightlyNotifiesAndMarksTheWindow` (applied, failed, paused on the third night), `TestCheckTellsANewApprovalOnce`. Two existing calls pass the new `FinishInstanceUpdate` argument.
- `internal/api` (`approllback_test.go`): `TestRollbackApp_RefusalsAreAudited` (none, 7 days over, busy, member on a household app 403, another member's app 404, 401), `TestRollbackApp_RunsAJobAndAudits`, `TestAppUpdateDTO_Rollback`, `TestAcknowledgeUpdateFailure`, `TestMarkUpdatesSeen`.
- **Real Docker** (`rollback_live_test.go`, build tag `dockerlive`): `go test ./internal/lifecycle/ -tags dockerlive -run TestLiveRollback -v -timeout 900s`. `TestLiveRollbackFiles`: alpine 3.20 at 1.0, a good update to 3.21 at 2.0, the user writes on 2.0, a 3.0 that never becomes healthy is rolled back, then the rollback by hand: 1.0 runs again on its pinned 3.20 digest, on the file from 1.0, and the file written on 2.0 is gone. `TestLiveRollbackPostgres`: with a managed Postgres 16, rows changed and a table added on 2.0 are back to what they were before the update. Both pass on this machine, and so do the six existing `TestLiveUpdate*`, `TestLiveApproved*` and `TestLiveAutoUpdateTurnsInternetOff`.
- `make check`: gofmt, vet and the OpenAPI check pass, and every package passes but `internal/hostagent/osupdate`, whose `TestTrialGood` and `TestOneSwitchPerNight` fail on this machine with or without this change (the package is the same as on `dev`, and this branch does not touch it). The web-ui typecheck and build pass. No test was removed.

## Known gaps & deviations

- **Not tried in a browser.** The UI was type-checked and built, not clicked through: Settings → Updates, the banner, the toast and the dialog.
- **The rollback has no whole-box proof.** The real-Docker tests ran without the socket proxy and on a daemon with no remap, like #631's.
- **A crash right after a good update's row is written** and before `update.prev.json` is written leaves a row that offers a rollback the files cannot back. The rollback then answers "no earlier version" and logs why, and the hourly prune clears the point when the 7 days end.
- **A managed service the update added is dropped by the rollback**, with its data. The dialog says data since the update is lost, but does not name the service.
- **The toast and the banner count on the window's mark**, which is written right after the update ends. A brain that stops between the two leaves the update unmarked: no toast, and the banner says no "last night".
- **The "what's new" snippets** in the toast and the changelog link are not built (no `changelog_url`).
- **The hold on Settings → System** ([nightly-app-updates.md](nightly-app-updates.md) listed it for this issue) is not built: the issue does not ask for it.
- **Review findings dismissed** (the fresh review agent):
  - A missing `compose.override.yml.prev` breaking `keepOlder`: install and every update always write the override and `.env`, and `keepPrevious` already needs all four files, so a previous generation always has them.
  - `recoverRollback` marking an app running when `compose up` fails: the row is `running` after `FinishInstanceRollback`, and the startup pass then runs `compose up -d` for every `running` row with no containers, as it does after `recoverUpdates`.
  - The job's audit using the request context: `audit.Record` only reads values from it (actor, client IP), which outlive a cancel, the same as `runUpdateJob`.
- **Greptile's findings, all five fixed in this PR:**
  - A failed update's undo could run again after a crash and copy the older generation (already back in `.prev`) over the current files. The record now says `undo_done` before the older copy is put back, and recovery skips the undo then (`TestRecoveryAfterTheUndoDoesNotRunItAgain`).
  - A rollback put back the old override and `.env`, so a setting or an AI or email account changed or deleted since the update came back. The rollback now writes the environment and the mail lines again from the store (`restampFromStore`, `TestRollbackTakesTheCurrentAccountValues`).
  - The prune removed the snapshot of an unfinished rollback past the 7 days. It now skips an app with `update.json` before the snapshot too (`TestPruneKeepsTheSnapshotOfAnUnfinishedRollback`). This also closes #631's gap that an `update_failed` app lost its snapshot after 7 days.
  - Allow & update left the approval notification open. Every good update now resolves it (`AppUpdateDone`, `TestAPersonsUpdateResolvesItsNotifications`).
  - The window's mark was written after the update's last event. The nightly report now sends `app.update_changed` again after the mark.
  - Each of the two new tests for the first two fails with its fix turned off.
  - Greptile's second pass: when `undo_done` cannot be written, the older copy is no longer put back, so the window it named cannot open. The way back to that version is then lost, and the log says so (`TestNoUndoMarkNoOlderCopy`).
- **One generation only.** After a rollback by hand there is nothing more to go back to, as the spec says.

## What's next

- #635: a managed database's major move during an update. Its rollback by hand needs the pre-migration dump as well as the snapshot.
- #630 closes after #635.
