# The health probe reaches owner-only apps through a Caddy probe socket

- **Status:** done
- **Date:** 2026-10-13
- **Specs touched:** `HEALTH.md` # Detector catalog; `CONTROL_PLANE.md` # Locked: Caddy is moose substrate; `APP_MANIFEST.md` # B; `THREAT_MODEL.md` B2; `TESTING.md` # Hosted cloud variant; `DECISIONS.md` 2026-10-13; `docs/architecture.md`; `docs/dev/hosted-boot-proof.md`

This closes #610, the gap [app-health-docker-signal.md](app-health-docker-signal.md) left. Since #609 the `app-unresponsive` probe reads the forward-auth gate's `302` to the box login as no sample. That stopped the false "healthy", but it also meant the probe never saw an owner-only app on hosted, which is most hosted apps. It builds on [caddy-admin-socket-migration.md](caddy-admin-socket-migration.md) (#612), which binds Caddy's whole `caddy/` dir on every box, old and new.

## What was done

- **The release order was fixed in the issue first.** The issue planned to declare the probe server in `dev/control-plane/caddy.json`, on the OS line. That file is copied to a box once, at first boot, and no update replaces it (`BUILD.md` # As built), so the server would have reached new boxes only. The issue body now says the brain writes the server through the admin API, so it ships on the control-plane line alone and reaches every box with the brain update.
- **A second Caddy server** (`internal/caddy`). `EnableProbeServer(listen)` turns it on. Then:
  - `EnsureServer` writes the whole `moose-probe` server after it resets the moose routes: the one listener `unix//run/moose-caddy/probe.sock`, `automatic_https` off, and only a catch-all that answers `404` with `X-Moose-Probe: no-route`. POST creates or replaces it, so its routes are reset the same way.
  - Every route write (`AddRoute`, `AddSplashRoute`) writes the app route first, then the probe's copy with the id `moose-probe-<instance_id>`, the same `Host` match and upstream, and no forward-auth, no public-path split and no cookie strip. A splash is copied as it is. The copy's `reverse_proxy` deletes `X-Moose-Probe` from the app's answer, so an app cannot pass for "no route". The copy is written only when the app route landed, so it never runs ahead of what browsers get. A failed copy is logged and not returned: a caller must not fail an install over the probe.
  - When the copy's PATCH finds no route, the client checks for the probe catch-all (`GET /id/moose-probe-catchall`). If it is missing, the server is gone (a Caddy recreate), and it writes the whole server again before it inserts the copy. Caddy refuses a PUT into the routes of a server that does not exist (checked against `caddy:2-alpine`), so this step is needed.
  - `RemoveRoute` removes both.
- **The brain turns it on only when its admin API is on the socket** (`cmd/brain`): a box made since #608, or one #612 moved. Only then is `caddy/` bound into Caddy, and Caddy refuses a whole config with a listener it cannot open. A box still on TCP and the dev loop keep today's probe. The paths are `lifecycle.CaddyProbeListen` (Caddy's side) and `lifecycle.CaddyProbeSocket(dir)` (the host side, `/var/lib/moose/control-plane/caddy/probe.sock`).
- **The probe dials the socket** (`appProbeDetector.SetProbeSocket`). When the socket file exists, the probe sends its `GET` there with the app's `Host`; otherwise it goes to Caddy's `:80` as before, with #609's gate rule. Two answers on the socket are **no sample**: the catch-all's mark, and a connection the socket does not take (the dial error is wrapped in `errProbeSocketDial`). A timeout after the connection was made is still a failure: the app is slow, not Caddy.
- **Checked against a real Caddy** (`caddy:2-alpine` in local Docker, admin on TCP for the test): the server is written with its socket, a route answers `502` for a dead upstream, a host with no route gets the marked `404`, a restart loses the server, the next `AddRoute` writes it back with its copy, and `RemoveRoute` leaves the marked `404`. Caddy replaces a stale socket file when it listens again. The socket's default mode is `0200` owned by Caddy's root; the brain opens it as root, like the admin socket.
- **Tests.** `internal/caddy/probe_test.go`, against a fake admin with both route lists that answers like Caddy: the whole server from `EnsureServer`; nothing written with the server off; the copy of a gated route has no gate and deletes the mark; a splash is copied and replaced in place on the flip; a missing server is written again on the next route write; a failed copy does not fail the write; a failed app route skips the copy; a removal removes both. `cmd/brain/probesocket_test.go`, against a real unix socket server: a gated app at `502` raises on the 2nd tick and clears on a `200`, and Caddy's `:80` is never asked; the marked `404` is no sample even for an app whose healthy set holds `404`, and Docker `unhealthy` still raises then; a dead socket file is no sample; no socket file falls back to `:80`. `internal/lifecycle`: the probe socket sits next to the admin socket. The dead-socket and no-route tests fail with their check removed, and so does the restore test in `internal/caddy` without the restore.
- **Cloud lane, `access` boot** (`dev/cloud/cloud-assertions.sh`), with a new fixture `probewho`: whoami with `health_probe: {path: /health, start_period: 1s}`, installed owner-only. whoami answers `GET /health` with whatever status a `POST /health` set, so the app can answer `502` and `200` with its container up all along.
  - **4e** (new). `:80` answers `probewho`'s `/health` with the gate's `302`. The probe socket exists, the probe server's only listener is that socket, and no app container mounts the control-plane dir. Through the socket, `/` reaches `probewho` with no identity header, `/health` answers `200`, and a host with no route answers `404` with the mark. With the app healthy, no issue after 70s. With the app at `502`, the issue is raised, and not before 50s (so not on the first failed tick). Back at `200`, it clears within 90s.
  - **6d-1** (new, for #613). After the Caddy recreate the probe server is gone (`null` on the admin API). An exposure toggle of `probewho`, a normal route write, brings it back with `probewho`'s copy, while whoami's host still gets the no-route mark.
  - **6e and 6g-2.** After the brain restart and after the #612 move, the probe socket reaches the apps again.
  - `VERDICT_TIMEOUT` for the access boot goes from 1590 to 2190.

## How it maps to the specs

- `HEALTH.md` # Detector catalog: the measurement row names the probe socket and the two no-sample answers; a new paragraph explains the probe server; the gate paragraph now applies only to a box on TCP.
- `CONTROL_PLANE.md` # Locked: Caddy is moose substrate: a bullet for the probe server, why the brain writes it, and how it comes back after a recreate.
- `THREAT_MODEL.md` B2: the probe server serves every app with no gate, so it is a unix socket only the brain mounts, like the admin API.
- `DECISIONS.md` 2026-10-13: the server is written by the brain, not declared in `caddy.json`.

## Known gaps & deviations

- **A missing route is no sample, and no sample clears, like a good tick.** That is #609's rule (a tick that does not fail clears), kept on purpose. So on the lane a missing probe route cannot be told apart from a healthy app by the issue alone. The lane proves the probe server answers a missing route with the mark, and the unit tests prove the brain reads the mark as no sample.
- **After a Caddy recreate, only the apps whose route is written again get their copy back** until the brain restarts (#613). In between, those apps are no sample, not unhealthy. #613 itself is not fixed here: its fix should write the probe server too, which `EnsureServer` already does.
- **"Healthy through the socket" is proved by the sequence**, not by a log line: the app at `502` raises (which the gate path can never do) and the same probe then reports it clear and keeps it clear.
- **A box whose #612 move keeps failing** stays on TCP and gets no probe server. Its owner-only apps stay unprobed, as since #609.
- **Appliance boxes take the same path** (their admin API is on the socket too), but the appliance lane, which is local-only, was not run. There is no gate on the appliance, so the probe answers the same as before there.

## What's next

- #598: restart an app that stays unresponsive. The probe is now a real sample for owner-only apps. Note that a probe gap (no sample) clears the issue, so a sustained-window timer must restart only on an issue that stayed raised, and a Caddy recreate can reset that window.
- #613: write Caddy's config again when it was recreated while the brain runs, the probe server included.
