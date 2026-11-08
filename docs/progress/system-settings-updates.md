# Settings → System: versions, the update schedule, the last result, and Update now

- **Status:** done
- **Date:** 2026-10-12
- **Specs touched:** `docs/specs/UPDATES.md` (# 6, # 8.2, # 8.4), `docs/specs/BRAIN_HOST_PROTOCOL.md`, `docs/specs/BRAIN_UI_PROTOCOL.md`, `docs/specs/TIME.md`, `docs/architecture.md`, `docs/dev/hosted-boot-proof.md`

Closes #488, the control-plane half (stream B) of the update screen. It builds on [update-target-read.md](update-target-read.md) (#443), which made the loop's decision readable, and [about-box-versions.md](about-box-versions.md) (#393), which showed the versions on About. The OS half (stream A, the reboot) is #509. Before starting, the issue was brought up to date with what landed since it was written: [host-agent-os-update.md](host-agent-os-update.md) (#563) built the OS update and its `os.last` record, and [control-plane-version-line.md](control-plane-version-line.md) (#559) made the brain's `version` the control-plane number. The maintainer decided three things then: keep the page name "System", show the OS and one control-plane version, and send no notification for a control-plane outcome.

## What was done

### The screen (`web-ui/src/views/settings/SystemSection.vue`)

- A new **Settings → System** item, next to About in the "System" group. The version card moved there from About, which now links to it, so there is one version display.
- **Versions, for every user.** The front shows "moose 0.16.1" (the OS release, `os_version`, falling back to `host_agent_version`) and "Control plane 0.17.0" (the brain's `version`). The brain's number is never labelled "moose" any more. Technical details: commit, host agent, OS slot, dashboard image.
- **Updates, for admins on a box that updates itself** (`auto_apply`, so hosted). The window in the box's zone ("between 03:00 and 04:00 (Europe/Stockholm)"), one sentence per state written by the dashboard with `detail` under a closed "Technical reason", "Last checked 10 minutes ago", and the last update ("Updated to 0.18.0 on 12 Oct at 03:04", or "The update to 0.18.0 failed, because the new version did not start properly. Your box went back to 0.17.0."). `unknown` never reads as up to date, `unreachable` says the box keeps running, and `disabled` says to contact support. On the appliance the section is hidden: its updates are parked (#400).
- **Update now**, shown when the state is `available`. The sentence before it and the confirmation dialog (a native `<dialog>`) say the apps keep working, the dashboard is away for up to a minute, and the box does not restart. The issue said "how long apps will be offline", but a stream B update does not stop the apps (Caddy stays up, `UPDATES.md` # 3), so the copy says what really happens.
- **While it runs.** The job id goes into sessionStorage, so a reload comes back to "Updating…". The page polls the job (`GET /api/v1/system/update/{job_id}`, which host-agent answers and so works again as soon as the new brain does) and ignores the failed requests while the brain restarts. When the job ends it reads `last`. When the update replaced the UI (`result.ui_changed`) the page reloads once, so it runs the new bundle. A 404 on the job (host-agent restarted) ends the wait and shows `last`. After ten minutes it says the update is taking longer than expected.
- **The `426` path: not built.** The issue left the choice open. The reload above is what stands in for it, on the tab that pressed the button; `BRAIN_UI_PROTOCOL.md` says so.

### The apply route

- **Brain:** `POST /api/v1/system/update-target/apply`, no body, admin-only, `202` with the job. Audited on success and on every refusal (`403`, both `409`s, `502`), with the existing `system.update` action and `trigger: update_now`.
- **host-agent:** `POST /v1/jobs/system-update-target`, no body. `409 job-running` when the lock is held (an update, or an OS install or switch), `409 nothing-to-apply` when the state is not `available` (and on the appliance), `501` when no loop is wired.
- **The loop** (`updatetarget.Loop.ApplyNow`) applies the target the last tick checked. It reads the running pair fresh, so an update that landed since the tick is not applied twice. It bypasses the one-attempt-per-night guard and marks the coming window's night as tried (`nextNight`), so a failed manual update is not repeated that night. `applyMu` guards the guard, held across the check, the start and the mark in both the tick and `ApplyNow`. A test races the two under `-race`.

### The record (`last`)

- host-agent writes `last-update.json` beside the ledger (`controlplane.LastUpdate`) for every stream B update, before it closes the job, and serves it as `last` on the update-target read. Shape: `id` (the job id), `outcome` (`good` or `failed`), `version`, `from`, `at`, `started_at`, `brain_changed`, `ui_changed`, `failure_mode`, `reverted`, `revert_error`. The first five match `os.last`. Scheduled updates, Update now and the explicit-refs route all write it; an update that changed nothing does not.
- `version` and `from` come from the images' `org.opencontainers.image.version` label (cpupdate reads the running image before the pull and the target after it). When the target never pulled, `version` falls back to the version the target named.

### The zone

- The loop used Go's `time.Local`, which is loaded once when host-agent starts. `set-timezone` does not refresh it, so after first-run set the zone, the window stayed in the old zone until the next reboot, and `time.Local` cannot name itself ("Local"). Now `Loop.Zone` (`updatetarget.SystemZone`) reads the `/etc/localtime` link on every tick, and the read reports the same name as `zone`. `TZ` wins when set, as it does for every program; a link that names no known zone falls back to `time.Local`.

### Fake host-agent

`MOOSE_FAKE_UPDATE_PROFILE=hosted` makes the report a hosted box, `MOOSE_FAKE_UPDATE_LAST=good|failed` serves a canned `last`, and Update now works: the fake takes the job lock, waits 8 s, then reports the box on the target with a `good` `last` under the job's id (`MOOSE_FAKE_UPDATE_APPLY=fail` gives a reverted failure). The zone is the dev machine's own. With `MOOSE_FAKE_UPDATE_TARGET` (existing), every sentence on the screen can be reached in the inner loop.

## How it was verified

- **Tests.** `internal/hostagent/updatetarget/applynow_test.go` (the apply outside the window, the night counting, the bypass, every refusal, the appliance, a lock refusal marking nothing, the race, `nextNight`, the zone link), `internal/hostagent/applytarget_test.go` (the route's codes, the outcome saved under the job id before the job closes, a failure, a no-op saving nothing), `internal/hostagent/cpupdate` (versions from the labels, UI-only, a pull failure), `internal/hostagent/controlplane/lastupdate_test.go`, and `internal/api/systemupdate_test.go` (the route's audit on success, 403, both 409s and 502, and the read passing `zone` and `last` through). `make check` green; `npm run build` (typecheck and build) green.
- **The fake over its real socket**: `available` → apply `202` → a second apply `409 job-running` → after 8 s the job `completed`, the state `current`, `last` good under the same id → a third apply `409 nothing-to-apply`.
- **The cloud lane's `update` boot** gains two checks (`docs/dev/hosted-boot-proof.md`): after the revert, the read reports that failed job as `last` from disk with a `zone` (`LAST OK`); and Update now applies a gen-4 target whose window is twelve hours away, the new brain reports that job as a good `last`, and a second apply is `409` (`update-now — APPLY OK`). This boot runs on the PR in CI.
- **The screen was not checked by looking at it.** It is covered by the typecheck and the build, and its data paths by the fake. The dashboard has no component tests.

## Review

- **The fresh review agent** found nothing at Block or Should level.
- **Greptile** found three P2s, all fixed: a poll in flight when the admin left the page could still finish and reload the page they moved to (the page now stops its poll work once it is left; the marker stays, so coming back resumes the wait); a hosted box with unusable update settings reports `auto_apply: false`, so its `disabled` sentence never showed (it now shows for a hosted admin); and `nextNight` added hours to midnight, so on the night the clocks go back an apply at the second 02:30 counted for the next night instead of that one (it now builds the wall-clock time, with a test across the change in Europe/Stockholm).
- **CI:** the cloud lane's `update` boot passed under both firmwares on the first push, with the new `LAST OK` and `update-now` checks.

## Known gaps & deviations

- **Other open tabs are not told** that the UI changed. Only the tab that pressed Update now reloads. A tab left open across a scheduled update keeps the old bundle until it is reloaded. API minors only add fields, so it keeps working.
- **No notification for a control-plane outcome.** The maintainer's call: not wanted. The OS update has one (#563).
- **A failed apply at noon still counts for the night**, as the issue asked. An admin who wants to retry the same day presses Update now again; the guard does not stop a manual apply.
- **The record keeps the last outcome only.** The journal keeps the history.
- **`from` can be empty** when the running image has no version label (a dev image, or an image built before the label existed). The sentence then says "went back to the version before it".

## What's next

1. #509: the OS on this screen (the restart sentence, `os.last`, and whether Update now also switches the OS).
2. The `426` path, if open tabs across a scheduled update turn out to matter.
