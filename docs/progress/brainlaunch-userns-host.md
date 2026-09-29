# The socket proxy and the brain run with `--userns=host`, #526

- **Status:** done
- **Date:** 2026-09-29
- **Specs touched:** `CONTROL_PLANE.md` (status only)

This is slice 1 of #523, the build of the daemon-wide `userns-remap` that [userns-remap-spec.md](userns-remap-spec.md) wrote down. It takes the host-agent half of the spike in [userns-remap-ci-proofs.md](userns-remap-ci-proofs.md) and writes it as real code. The remap itself is not turned on: that is the last slice (#530).

## What was done

- **`internal/hostagent/brainlaunch`.** `RunSpec` gains `UsernsMode` and `Tmpfs`. `CLIDocker.Run` passes them to `docker run` as `--userns` and `--tmpfs`. The argument list is now built by a small function, `runArgs`, so a test can check the flags without a Docker daemon.
- **The socket proxy** (`proxyRunSpec`) gets `--userns=host` and `--tmpfs /run`. It keeps `cap_drop: ALL` and `no-new-privileges`. No capability is given back.
- **The brain** (`runSpec`, and so `RunSpecFor`) gets `--userns=host`. Nothing else changes. The control-plane update (`internal/hostagent/cpupdate`) builds the brain from `RunSpecFor`, so a brain an update recreates keeps the flag.
- Both flags are **always passed**. host-agent does not ask Docker whether the remap is on. On a daemon without the remap `--userns=host` changes nothing, so it cannot drift from `daemon.json`. The `/run` tmpfs is harmless there too: haproxy writes only its pid file in it.
- **Tests** (`userns_test.go`): the first-launch proxy, the recreated proxy, the brain from `Launch` and from `RunSpecFor`, and the rendered `docker run` arguments, including that the proxy gets no `--cap-add`.

## How it maps to the specs

- Realizes the first two bullets of `CONTROL_PLANE.md` # Locked: control-plane container hardening, "Under the daemon-wide userns-remap". That section said "specced, not built yet". It now says the two flags are built and the remap is not on yet.
- `APP_ISOLATION.md` # User-namespace tiers puts the proxy and the brain in the host tier, picked by host-agent. This is that.
- `docs/architecture.md` gains a line on what of the remap is built.

## Verification

- `make check`: green.
- A local Docker 28 without the remap runs `--userns host --tmpfs /run --cap-drop ALL --security-opt no-new-privileges:true` as expected: root in the host namespace and a tmpfs on `/run`.
- `CI / Cloud image` with `publish=false` on this branch, on today's image (no remap): see the PR for the run. The spike already ran the same two flags through the six usual boots on a remapped image (run 36490832011).

## Known gaps & deviations

- **An existing proxy or brain container is not recreated to get the flag.** `EnsureTransport` recreates a proxy only when it lacks the capability sandbox (#431), and `Launch` leaves an existing brain alone. That is fine here: on a daemon without the remap the flag does nothing, and turning the remap on moves Docker to a new data root (`BUILD.md` # User-namespace remap), so every container made before is out of sight and host-agent creates both again, with the flags. A control-plane update also recreates the brain with it.
- **Not proved on a remapped daemon in this PR.** The image has no remap yet. The spike proved these exact flags on one (`userns-remap-ci-proofs.md` proof 1), and slice 6 (#531) adds a remap boot that keeps proving it.

## What's next

1. #527: `remap_base` on `GET /v1/identity/well-known`.
2. #528: the `root_setup` manifest field and its admission rules.
3. #529, #530, #531: the brain tiers, turning the remap on in both images (after #486), and the CI remap boot.
