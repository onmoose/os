# A failed Caddy route update keeps the old route, #520

- **Status:** done
- **Date:** 2026-09-29
- **Specs touched:** `APP_LIFECYCLE.md`, `CONTROL_PLANE.md`

This closes #520. The bug was found by the #516 CI proofs, [userns-remap-ci-proofs.md](userns-remap-ci-proofs.md), under # Not caused by the remap. In run 36490829133 the brain logged "caddy upstream flip failed (continuing) ... caddy admin unreachable: Put \"http://moose-caddy:2019/config/apps/http/servers/moose/routes/0\": ... read: connection reset by peer", and poznote answered 404 from then on. That entry's What's next item 3 asked for this fix.

The cause was in `upsertRoute` (`internal/caddy/caddy.go`). It deleted the app's route by `@id`, ignored the result, and only then PUT the new route. When the PUT failed, the app had no route at all. Every request fell through to the catch-all 404 until something wrote the route again, and on a running box nothing did until the brain restarted.

## What was done

### Replace in place, in one call

`upsertRoute` and `EnsureDashboard` now share `upsertRouteByID`. It sends `PATCH /id/<id>` with the new route. Caddy loads a PATCH as one new config, so the call lands whole or changes nothing. When it fails, the old route keeps serving. The route also keeps its place in the list, so it stays ahead of the catch-all.

Only when Caddy answers 404 ("unknown object ID", so no route has the `@id` yet) does it insert the route with `PUT .../routes/0`, as before. That is the first write for an app, and every write after a brain restart, since the brain clears the route list on startup.

**Why not add the new route first and remove the old one after.** Caddy refuses a config that holds the same `@id` twice ("indexing config: duplicate ID"), checked against `caddy:2.11.4`. So the new route would need a second id. The swap would take two calls again, and the second id would have to be cleaned up if the brain died between them. The in-place PATCH is one call with nothing to clean up.

### The caller sees the error, and a lost answer is tried again

The error from Caddy still goes back to the caller, which logs it with `instance_id`, `host` and `upstream` as before. A call that got no HTTP answer at all (a reset or refused connection, like the CI case) is tried again, up to 3 tries, 500 ms apart. Both calls are safe to repeat: a retry PATCHes the route an earlier PUT may already have added. A call that Caddy answered with an error is not repeated, since Caddy would refuse the same config again. A call that timed out is not repeated either: Caddy is hanging, and each try would cost the full 5 s client timeout. The retry log line carries the route's `host`.

This matters because with the in-place fix a failed flip no longer shows the catch-all, but it does leave the app on its "starting" splash. Without the retry, one reset connection would keep a healthy app on that splash until the brain restarts.

### The startup pass repairs routes for stopped and failed apps too

The issue asked whether the reconciler repairs a missing route on its next pass. It runs once, at brain startup (`APP_LIFECYCLE.md` # Locked: reconciliation is imperative). For a **running** app it already did: `reassertRouting` writes the real upstream on every startup, whether or not the containers were up.

For a **stopped** or **failed** app it did not. The brain clears Caddy's routes on startup (`EnsureIngress` sends an empty route list), and nothing wrote the splash back. So after any brain restart a stopped or failed app answered with the catch-all 404 ("No app at this hostname") instead of its splash. The new `reassertSplash` writes the stopped or failed splash for those apps during the pass. It keys the route on the stored host and does not re-publish the mDNS name, the same as Stop.

The splashes are written **after** the loop over all instances, not in it. The whole pass runs under one 30 s startup deadline in `cmd/brain`, and instances come in install order. Without this, a slow Caddy could spend that deadline on older stopped apps' splashes and leave no time for a running app's route or compose up. Both reviewers found this (see # Review). The splashes then get their own 10 s budget (`splashBudget`), cut loose from the caller's deadline, so they still get a try when the work before them used it all.

### Tests

- `internal/caddy/upsert_test.go` adds `routeAdmin`, a fake admin API that keeps a real route list and answers PATCH, PUT and DELETE the way Caddy does, including 404 for an unknown id and 400 for a duplicate id. A call can be made to fail with a reset connection or a 500, and a failed call leaves the list as it was. It can also apply a call and then reset, which is a write Caddy applied whose answer was lost, and it can answer slowly.
  - `TestAddRouteFailedWriteKeepsOldRoute`: every call of the flip fails. The splash still serves, the caller gets the error, and no DELETE is sent.
  - `TestAddRouteOneFailedCallNeverLeavesNoRoute`: calls 1 to 4 of the flip fail one at a time. The host always serves the old splash (with an error) or the new app, never the catch-all. With the old code this fails at call 2, with the same "connection reset by peer" on the PUT that CI logged.
  - `TestAddRouteRetryAfterAnAppliedWrite`: a PATCH, and a first-write PUT, that landed but lost their answer. The retry succeeds and no duplicate route is added.
  - `TestAddRouteRetriesAWriteWithNoAnswer`, `TestAddRouteDoesNotRetryAnAnsweredError`, `TestAddRouteDoesNotRetryATimeout`, `TestAddRouteInsertsOnceThenReplacesInPlace`.
- `internal/lifecycle/lifecycle_test.go`: `TestReconcileRepairsRouteOfRunningInstance` (a running app left on its splash gets its upstream back) and `TestReconcileReassertsSplashForStoppedAndFailed`, and `TestReconcileWritesSplashesAfterRunningRoutes` (an older stopped app's splash is written after a newer running app's route), and `TestReconcileWritesSplashesAfterTheDeadline` (a stopped app's splash is still written when the caller's context is already done). The last three fail without the change.
- The existing request-shape tests in `caddy_test.go` now look for the PATCH instead of the PUT, and the dashboard test checks that no DELETE is sent.
- **Against a real Caddy.** A throwaway probe (not checked in) ran the client against `caddy:2.11.4` with `dev/caddy.json`: splash, flip to the app, stopped splash, a PATCH Caddy refuses (the stopped splash keeps serving and the 500 comes back), and two dashboard writes. The route list ended as `[moose-dashboard, moose-app-a1, moose-catchall]`, one route each with the catch-all last.

`make check` passes.

## How it maps to the specs

- `APP_LIFECYCLE.md` # Locked: Caddy route registration timing now says how a swap is done and why the old route is never removed first. # Locked: reconciliation is imperative lists the route re-write for every state.
- `CONTROL_PLANE.md` # Catch-all 404 invariant: routes are inserted at index 0 with PUT (the spec said POST, which appends) and changed only in place after that.
- `docs/architecture.md` brain → Caddy: the same, in one sentence.

## Review

- **Agent review (Block) and Greptile (P1), the same finding: splash replay could use up the startup deadline.** Confirmed. Before this change a stopped or failed app made no Caddy call in the pass, and a write was never retried. Fixed two ways: splashes are written after every running app's work, and a timed-out call is not retried, so a hanging Caddy costs one client timeout per write as before. A refused or reset connection still costs up to about 1 s per write. Covered by `TestReconcileWritesSplashesAfterRunningRoutes` and `TestAddRouteDoesNotRetryATimeout`.
- **Agent review (Note) and Greptile (P2), the same finding: no test for a write that landed but lost its answer.** Confirmed as a coverage gap, not a bug. Added `TestAddRouteRetryAfterAnAppliedWrite`.
- **Agent review (Note): the retry log line had no route identity.** Confirmed. It now logs `host`.
- **Greptile re-review (P1): with splashes last, they could start on an expired deadline and never be written.** Confirmed. The splash loop now runs on its own 10 s budget, apart from the caller's deadline. This can make startup up to 10 s longer, only when Caddy is slow. Covered by `TestReconcileWritesSplashesAfterTheDeadline`.
- **Greptile re-review (P2): the timeout test waited a fixed 250 ms for the slow fake to record its call.** Confirmed. The fake now records a call before its delay, and the test has no sleep.

## Known gaps & deviations

- **The startup pass is still the only repair.** If a flip fails on all 3 tries, a running app stays on its "starting" splash (or a stopped app keeps pointing at its stopped containers) until the user acts on it or the brain restarts. There is no periodic reconcile, by design (`APP_LIFECYCLE.md`). The failure is logged at warn level.
- **`EnsureDashboard` changed too.** It had the same remove-then-add shape. At startup the list is empty, so this changes nothing there, but it keeps one write path for every route.
- **Not run on a booted image.** The fix is inside the brain's Caddy client and the startup pass, and the unit tests plus the real-Caddy probe cover it. The `CI / Cloud image` lane was run on this branch; see the PR.

## What's next

Nothing new from this change.
