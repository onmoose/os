# The startup reconcile waits for Docker, #540

- **Status:** done
- **Date:** 2026-09-30
- **Specs touched:** `APP_LIFECYCLE.md` # Locked: reconciliation is imperative, with a startup pass, `docs/architecture.md`

Closes #540, a bug the new `remap-reboot` boot of #531 found on its first run. After a reboot every app answered Caddy's catch-all 404, and stayed that way until someone stopped and started it.

## What was done

- **The cause.** The brain reaches Docker only through the socket proxy. On a reboot Docker starts the proxy and the brain together, from their restart policy, and host-agent starts after both ("brain container already present; leaving it to Docker"). In run https://github.com/onmoose/os/actions/runs/36642566707 the brain was up one second before the proxy answered. Its startup reconcile logged "Cannot connect to the Docker daemon at tcp://docker-proxy:2375" and then "startup reconcile failed" with "reconcile: list actual: exit status 1". `EnsureIngress` had already reset Caddy's route list, and the reconcile is the only thing that puts the app routes back, so no app had a route. The dashboard route was fine, because it is added separately.
- **`lifecycle.Manager.WaitDocker(ctx, poll)`** (new). It calls `PSManaged`, the `docker ps` the reconcile lists containers with, until it works or `ctx` ends, and then returns the last error.
- **`cmd/brain/main.go`** calls it once, before any startup Docker work (the control-plane compose, `EnsureIngress`, the reconcile), with a 15s budget (`dockerReadyTimeout`). On a first boot or an update the proxy is already up and the wait returns at once. If Docker never answers the brain logs "docker not reachable; startup reconcile may be incomplete" and goes on as before. 15s on its own stays well under the 60s host-agent gives a recreated brain to answer `/healthz`. The other startup budgets (the control-plane compose, the Caddy wait, the reconcile) come after it, so a box where several of them are stuck at once can still miss that window, as it could before.
- The reconcile itself still runs once. `cmd/brain` says why the control-plane compose must not gain a retry loop; this change only waits before the first try.

## Tests

- `TestWaitDockerThenReconcileReaddsRoutesAfterReboot`: an installed app whose containers run and whose route is gone, with the fake Docker failing its first calls. A reconcile run into one failure returns an error and adds no route, which is the bug. After `WaitDocker` rides out three failures, the reconcile adds the route.
- `TestWaitDockerGivesUpWhenCtxEnds`: with Docker never answering, the wait ends with the budget and returns "docker not ready" with the last Docker error.
- The fake Docker gains `psManagedFails`, which fails the next N `PSManaged` calls.
- `make check`: green.

## Verification

The `remap-reboot` boot of #531 is the proof on a booted box. It runs on the #531 branch after this change lands on `dev`.

## Known gaps & deviations

- **Only Docker is waited for.** Host-agent was also not up yet in that run ("host-agent not reachable"). The reconcile does not need it on a hosted box, and on an appliance a failed mDNS publish is logged and the route is still written, so this change does not wait for it.
- **A Docker that is down for more than 15s at start** still leaves the apps with no route until the next brain start or a stop and start. That is the old behaviour, for a rarer case.

## Review

- **Fresh Sonnet agent:** no Block findings. One Question: the "stays under the 60s" claim read as a guarantee, but the other startup budgets add up after the wait. Confirmed and reworded in the code comment and above: the claim holds for the wait alone.
- **Greptile P1, "Docker wait can trigger rollback":** partly confirmed. On an update host-agent recreates only the brain, and the proxy stays up, so the wait returns at once there. It could only eat into the `/healthz` window if the proxy were also slow during an update. The budget went from 30s to 15s anyway: in the failing run the proxy answered about a second after the brain started, so 15s is plenty for a reboot and leaves more of the 60s for the rest of startup.

## What's next

1. #531: the remap boots, whose `remap-reboot` boot checks this on every run.
