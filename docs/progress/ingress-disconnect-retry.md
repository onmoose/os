# A failed disconnect from the control-plane network is tried again

- **Status:** done
- **Date:** 2026-11-08
- **Specs touched:** `CONTROL_PLANE.md` # Locked: Caddy is moose substrate; `THREAT_MODEL.md` B2

This closes a gap left by [apps-off-ingress.md](apps-off-ingress.md). Greptile found it on the release PR #650. `migrateOffIngress` wrote the cleaned `compose.override.yml` first and then disconnected the main container from `moose-ingress`. It returned early when the override had no `moose-ingress` in it. So if the disconnect failed, or the brain died between the write and the disconnect, the next start saw a clean file and never tried again. `Reconcile` does not recreate a running app, so the app stayed on the network where it can reach the Docker socket proxy (host root, `THREAT_MODEL.md` B2), with one warning in the log.

## What was done

- **The disconnect no longer depends on the override.** `migrateOffIngress` is now two steps, `dropIngressFromOverride` and `disconnectFromIngress`. The second runs at every brain start for every running, stopped, failed or `update_failed` app, whatever the file said. It also runs when the override cannot be read or written, because that says nothing about the container. The returned bool still means "the override changed", and the one caller ignores it.
- **It asks the container first.** `disconnectFromIngress` calls `ContainerNetworks` and disconnects only if `moose-ingress` is listed. So a normal box makes one `docker inspect` per app and logs nothing, and "app moved off the control-plane network" is logged only for a real disconnect. It asks the container and not `NetworkInspect`, because a network's list holds running containers only, and a stopped container that is still attached joins the network again when it starts. Checked on real Docker.
- **A missing container is now its own error.** `ContainerNetworks` returns `errNoSuchContainer` when `docker inspect` says "No such container", and the migration treats that as nothing to do (an app with no container). Any other failure to ask is returned, so `Reconcile` logs a warning with `instance_id` and the next start asks again. Other wording from a later Docker falls on this safe side. Other failures now carry Docker's stderr, not only "exit status 1". `EnsureCaddyNetworks` still treats every error as a failure, as before.
- **What Docker says, measured on Docker 28.1.1.** `docker network disconnect` for a missing container says `No such container: <name>`, also when the network is missing too (the container is checked first). A missing network with a real container says `network <name> not found`. A container not on the network says `is not connected to network`, or `is not connected to the network` for one never started. `notAttached` already covers all of these. `docker inspect --type container` on a missing container says `No such container: <name>` on stderr.
- **The container name is right.** `writeOverride` has pinned `container_name: moose-<id>-<main>` since #83 (June), and the first tagged release is from July, so no released box has a container with the compose `-1` suffix. In the code before #608, only the main service was attached to `moose-ingress`.
- **A manifest that cannot be read.** With a clean override there is no name to ask for. This is logged at debug level and returns no error, so it does not add a warning at every start. The next start asks again. For a running app `reapplyResourceLimits` already warns about the manifest. If the override was changed on this start, the error is returned as before.
- **Tests** (`internal/lifecycle/caddy_networks_test.go`, with a new `networkDisconnectErr` on the fake):
  - Clean override, container still attached: disconnected on this start.
  - Disconnect fails on the first start: the override is cleaned, the error is returned, and a second run disconnects it. This is the bug.
  - Clean override, container not attached: no error and no disconnect call.
  - Clean override, no container: no error and no disconnect call.
  - A failed inspect is an error, not clean.
  - Clean override, unreadable manifest: no error and no docker call.
  - Missing override, container attached: disconnected, and the error is returned.
  - `Reconcile` warns with `instance_id` and the cause when the disconnect fails.
  - `TestDockerNetworkErrorWording` covers the missing-container wording.
  - `TestReconcileMovesOldAppsOffIngress` now puts the container on `moose-ingress` in its fixture, because the code now asks before it disconnects. Its assertions did not change.
- **Real Docker.** `internal/lifecycle/ingress_live_test.go` (`-tags dockerlive`) runs the migration with the real CLI on real Docker 28.1.1: no container, a container not attached, a container attached but not running, and a container attached and running. Passed locally.
- **Cloud lane.** The `access` boot step 6a in `dev/cloud/cloud-assertions.sh` asserts that no managed container is on `moose-ingress`. That is still true, so it did not change.

## How it maps to the specs

- `CONTROL_PLANE.md` # Locked: Caddy is moose substrate: the disconnect is checked at every start, and a failed one is tried again.
- `THREAT_MODEL.md` B2, Brain↔Docker row: names the time an app can stay on the network when the disconnect fails.

## Known gaps & deviations

- **The retry is at the next brain start, not sooner.** That is the bar that was asked for. Until a later start succeeds, the app stays on the network, with one warning per start.
- **An unreadable manifest pauses the check.** If an app is still attached, its override is clean and its manifest cannot be read, the brain cannot name the container. It logs at debug level only, and the check resumes at the first start where the manifest can be read.
- **`noSuchContainer` matches Docker 28's words.** If a later Docker words it differently, an app with no container warns at each start. It does not count as clean.
- **Appliance medium lane and cloud boot not run.** This change has unit tests and the live test only.

## What's next

Nothing follows from this change.
