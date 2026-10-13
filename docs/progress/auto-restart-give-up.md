# The automatic restart gives up after one hour

- **Status:** done
- **Date:** 2026-10-13
- **Specs touched:** `HEALTH.md` # Automatic restart; `APP_LIFECYCLE.md` # Locked: automatic restart of an unresponsive app; `LOGGING.md` (`app.restart.auto`); `BRAIN_UI_PROTOCOL.md`; `DECISIONS.md` 2026-10-13; `docs/architecture.md`

This closes #620, the known gap that [auto-restart-unresponsive.md](auto-restart-unresponsive.md) (#598) left. There, an app that stays broken got at most 3 automatic restarts in any rolling hour, so about 3 every hour for as long as it stayed broken, each one audited, while the issue text said the brain "stopped trying". The count was in memory, so a brain restart started it again. The maintainer's decision: the brain tries for one hour, then gives up for good. [caddy-config-restore.md](caddy-config-restore.md) (#613) is not touched: the restore still runs before the probe on the same tick.

## What was done

- **The count is in the app's row.** Two additive columns on `instances`: `auto_restarts` (default 0) and `auto_restart_last_at` (unix seconds, default 0), read into `store.Instance.AutoRestarts` and `LastAutoRestart`. `lifecycle.Manager.BeginRestart(id, at)` now counts the restart (`Store.RecordInstanceAutoRestart`) under the app's lock, after its checks and before it returns the work, so a refused restart is not counted and a brain that dies halfway keeps the count. A failed write refuses the restart.
- **Giving up** (`cmd/brain/autorestart.go`). The brain gives up on an app when its stored count reaches 3. The waits stay 5, 10 and 20 minutes, now doubled per restart in the stored count (or per restart in the last hour, whichever is more), so a brain restart in the middle keeps the backoff. Once given up, `maybeRestart` never starts a restart, so no `app.restart.auto` record is written.
- **What ends it, each with a fresh hour.**
  - A real healthy sample: the probe answers good, or, for an app with no `health_probe`, Docker reports `healthy`. The detector (`appProbeDetector.check`) now tells this apart from a tick that does not fail only because it has no sample (the forward-auth gate, the no-route mark, a dead probe socket, an app Caddy is off the network of, the start-period grace, a container that is not running). For an app with a probe, Docker's `healthy` does not count, because the probe is what flagged it. `autoRestarter.recovered` then writes the count back to 0 (`Store.ResetInstanceAutoRestarts(id, false)`), only when it is above 0, so a healthy app costs no write.
  - A person's Start, which covers Stop then Start: `lifecycle.Manager.Start` resets the count and clears the last time under the app's lock (`ResetInstanceAutoRestarts(id, true)`), before it starts the app. Stop alone does not reset it.
  - A change of the Automatic restart switch: `Store.SetInstanceAutoRestart` resets the count and clears the last time in the same `UPDATE` when the value changes. Writing the value the app already has resets nothing.
- **The #598 hourly cap stays for an app that recovers and breaks again.** A real healthy sample resets the stored count, so such an app is never given up on. Without a cap it would be restarted every 8 minutes forever, with a raise and a notification each time. So the in-memory history from #598 still caps it at 3 restarts in any hour. A person's action clears the last restart time in the row, and the brain drops its in-memory history for an app whose row has none, so a person's Start or a switch change really gives a fresh hour; a real recovery keeps the time and so keeps the cap.
- **The issue text.** Once given up, the details read: "moose restarted it 3 times, the last time at HH:MM UTC. That did not help, so moose will not restart it again by itself. To try again, stop the app and start it. Its logs may show what is wrong." Before that, they name the last restart as before, now from the row. No new UI.
- **Audit metadata.** `attempt` is now 1 to 3, counted since the app last worked or a person last acted (`LOGGING.md`).
- **Tests.**
  - `cmd/brain/autorestart_test.go`, through the real detector with a fake lifecycle and a real SQLite store (the detector lists from it, the fake `BeginRestart` counts in it, the reset writes to it): an app that stays broken is restarted at minutes 6, 17 and 38 and never again in the next 5 hours, with the given-up text and exactly 3 audit records; a new detector and restarter on the same store (a brain restart) restarts nothing for 3 hours and shows the text again; a brain restart after 2 restarts makes the third wait 20 minutes, not 5; a real healthy answer resets the count, and the next failure is restarted after the 5-minute wait; ticks with no sample (Caddy off the network, the container not running) clear the issue but keep the count at 3 and restart nothing; Stop keeps the count, and a Start (the row reset `lifecycle.Start` does) leads to a new restart; the switch off and on through the real store leads to a new restart on the next tick, without the given-up text; an app that recovers after each restart and breaks again is never given up on and is still restarted at most 3 times in any hour. The #598 tests still pass. Run with `-race`.
  - `internal/lifecycle/lifecycle_restart_test.go`: a begun restart is counted in the row with its time before it runs; a refused one (busy, opted out) is not; turning the setting off resets the count; a Start from `stopped` and from `failed` resets the count and clears the time, and Stop alone does not.
  - `internal/store/store_test.go`: both columns round-trip through `Get` and `List`; `ResetInstanceAutoRestarts` keeps or clears the time; a change of the setting resets, writing the same value does not; a missing app is `ErrNotFound`.
- **Cloud lane: not changed.** Proving the give-up on the lane needs about 45 minutes of a broken app, and the `access` boot has no cheap step that shows the stored count (the API does not expose it). The rules are proved by the unit tests against the real detector and a real store.

## How it maps to the specs

- `HEALTH.md` # Automatic restart: the "Bounded" bullet now gives up for good after 3 restarts, a new bullet lists what ends it and that it survives a brain restart, and a third keeps the hourly cap for an app that recovers. The sentence that the history is in memory is gone.
- `APP_LIFECYCLE.md` # Locked: automatic restart: the restart is counted in the row before it runs, and a person's Start resets it.
- `DECISIONS.md` 2026-10-13 records the change from a rolling-hour limit to giving up.

## Known gaps & deviations

- **"One hour" is three restarts, not a clock.** The brain gives up after the third restart that did not help. With the waits, that is at about minute 38 after the issue is raised. If restarts are delayed (a busy app, ticks with no sample that start the wait again), the three restarts take longer than an hour, and the brain still gives up only after the third.
- **The hourly cap for an app that recovers is still in memory.** A brain restart gives such an app up to 3 more restarts in that hour. The given-up state, which is what the decision is about, is stored.
- **An app that recovers and breaks again is never given up on.** Each real healthy answer gives it a fresh hour, as decided; the cap keeps it at 3 restarts an hour, each audited.
- **A reset after a real healthy sample is not audited or announced.** It is a log line (`auto-restart: app answered its health check; restart count reset`).
- **The text says "stop the app and start it".** The switch off and on works too, but the text names the one action every user who can see the app's controls understands.

- **Review.** The agent review found no Block. Its notes are left as they are: the switch is written without the app's lock, so a change during a restart in flight resets the count while the restart runs, which matches "a change of the setting ends giving up"; and an app with no probe and no image healthcheck can never give a real healthy sample, but it can never be flagged either, so its count never rises. Greptile found one real race, fixed: a person who turns the switch off and on (or stops and starts the app) after the tick read the row but before the restart began left the brain's in-memory history in place, and the restart's new time hid the reset from later ticks, so the old history blocked the next restarts for up to an hour. `BeginRestart` now also returns the row it read under the lock, before the count, and the brain forgets its history when that row has no last restart time and takes the attempt number from it. A detector test drives that race and fails without the fix.

## What's next

- Nothing follows from this change.
