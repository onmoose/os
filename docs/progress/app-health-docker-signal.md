# App health reads Docker's health status and does not follow redirects

- **Status:** done
- **Date:** 2026-10-13
- **Specs touched:** `HEALTH.md` # Detector catalog, `APP_MANIFEST.md` # B, `DECISIONS.md` 2026-10-13

This fixes #609. The `app-unresponsive` detector came from the per-app health probe ([`DECISIONS.md`](../specs/DECISIONS.md) 2026-06-02). On a hosted box it reported an owner-only app as healthy for 15 hours while the app's backend returned `502` and Docker said the container was `unhealthy`. Three things caused it: the probe followed redirects, the forward-auth gate's `302` to the box login counted as healthy under the default "any status < 500", and Docker's own health status was never read.

## What was done

- **Docker's health status is read.** `lifecycle.ManagedContainer` has a new `Health` field: `State.Health.Status` from the same `docker inspect` the detector already runs each tick. It is `starting`, `healthy` or `unhealthy`, and empty when the image has no `HEALTHCHECK`. Checked against real Docker: a container with a failing healthcheck prints `starting` then `unhealthy`, and one with none prints `none`, which the reader turns into the empty string.
- **Docker `unhealthy` fails the tick, for every app.** `appProbeDetector.check` now samples every steady-running instance, not only those with a `health_probe`. A tick fails when Docker says `unhealthy`, or when the app has a probe, is past its start-period grace, and the probe fails. The debounce is unchanged: raise on the 2nd failed tick, clear on 1 tick that does not fail. The issue text is unchanged, so the user sees one issue.
- **The probe does not follow redirects.** `newProbeClient` sets `CheckRedirect` to return `http.ErrUseLastResponse`, so `probeHealthy` classifies the app's own `3xx`.
- **The gate's login redirect is no sample.** On hosted, for a restricted app, a `302` whose `Location` is exactly the box login URL means the probe met the gate. `probe` now returns a three-way result, and that case is `probeNoSample`: it neither fails nor passes the tick, and Docker decides. The login URL moved into `profile.HostedLoginURL`, which the route builder in `internal/lifecycle` now uses too, so the two cannot drift.
- **Tests** (`cmd/brain/main_test.go`): Docker `unhealthy` raises on the 2nd tick and clears when it turns `healthy`, `starting` or no healthcheck, with and without a probe; `starting`, no healthcheck and `healthy` never raise; Docker `healthy` does not outvote a failing probe; Docker `unhealthy` counts inside the probe's grace; against an `httptest` server the probe does not follow a `302` and the target is never requested; the gate's login redirect never raises, does not hide Docker `unhealthy`, and does not count as good under the default healthy set; a `302` elsewhere on a gated route is still classified. Each new test fails with the matching part of the fix removed.

## How it maps to the specs

- `HEALTH.md` # Detector catalog: the issue row and the measurement row name both signals; three new paragraphs cover the Docker signal, the no-redirect rule, and the gate redirect.
- `APP_MANIFEST.md` # B: an app with no `health_probe` can still raise `app-unresponsive` from Docker; `healthy_status` sees the app's own `3xx`.
- `DECISIONS.md` 2026-10-13: the 2026-06-02 "no field, issue never raised" no longer holds.

## Known gaps & deviations

- **"No sample" clears, like a good sample.** The issue describes `starting`, no healthcheck and the gate redirect as "not samples". The detector has no hold state: a tick either fails or it does not, and one that does not fail clears an active issue. This is how the detector already treated the start-period grace and a stopped container, and it is what the issue's "clears on the first tick it is no longer `unhealthy`" asks for. The visible result: an owner-only app on hosted with no image healthcheck is never flagged by the probe. That is the honest state until #610 lets the probe past the gate.
- **The appliance path is unchanged** except for the Docker signal and the redirect rule. There is no gate on the appliance.
- **Not tried on a running box.** The `docker inspect` format was checked against real Docker; the detector itself is covered by unit tests with the fake Docker reader.

## What's next

- #610: let the probe reach past the forward-auth gate, so an owner-only app's probe is a real sample again.
