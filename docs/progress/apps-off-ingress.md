# Apps off the control-plane network, Caddy admin on a unix socket

- **Status:** done
- **Date:** 2026-10-13
- **Specs touched:** `CONTROL_PLANE.md` # Locked: Docker socket exposure, # Locked: Caddy is moose substrate; `THREAT_MODEL.md` B2 and the accepted-risk list; `APP_ISOLATION.md` # Inter-app traffic; `DECISIONS.md` 2026-10-13; `docs/architecture.md`

This closes #608. Until now every app's `main_service` joined `moose-ingress`, the network of the brain, Caddy, `moose-ui` and the Docker socket proxy. So a compromised app could reach `docker-proxy:2375`, which is host root because the proxy does not read request bodies (measured in #430, the box's most severe open gap in `THREAT_MODEL.md` B2), and also Caddy's unauthenticated admin API on `:2019`, the brain's API on `:8080`, and other apps directly. #187 named the fix and was closed in favour of #608.

## What was done

- **Apps leave `moose-ingress`.** `writeOverride` no longer attaches any service to it. The main service's `moose-<id>-<main>` alias moves to the app's own network, `moose-app-<id>`, so route upstreams are unchanged. The override goldens show the change and nothing else.
- **The brain puts Caddy on each app's network.** New `DockerDriver` methods `NetworkConnect`, `NetworkDisconnect` and `ContainerNetworks` (the proxy already allows `NETWORKS` + `POST`). Install connects Caddy right after creating the network, before `compose up`. Start connects it again, since Caddy may have been recreated while the app was stopped. Teardown (uninstall, install rollback, orphan teardown) disconnects Caddy first, because Docker will not remove a network a container is still on. "Already attached" and "already gone" count as success, matched against Docker 28's words.
- **A recreated Caddy gets its networks back.** `Manager.EnsureCaddyNetworks` reads Caddy's networks with one `docker inspect` and connects every running app it is not on. It runs at the end of the startup reconcile and first on every health-poll tick (`appRuntimeHealthLoop`). It never runs compose. It skips an instance whose lock is held (`tryLockInstance`): the install, start or uninstall holding it handles the network, and connecting during an uninstall would keep the network from being removed.
- **Caddy's admin API is a unix socket on a new box.** The staged `caddy.json` listens on `unix//run/moose-caddy/admin.sock`, and the control-plane compose binds `./caddy/` there (`protocol.CaddySocketDir`, `CaddySocketMount`). `caddy.New` accepts a `unix://` address and dials the socket, sending `Host: 127.0.0.1`.
- **host-agent creates the socket dir** on every start, before the brain launch: `brainlaunch.EnsureCaddySocketDir` makes `/var/lib/moose/control-plane/caddy/` mode `0700`, owned by the moose-remap base when `docker info` says the remap is on, else 0, and repairs owner and mode on an existing dir. Checked first with a throwaway Caddy in the production sandbox (`--read-only`, `cap_drop ALL`): with a dir it does not own, Caddy fails with "starting caddy administration endpoint: permission denied"; with one it owns, it makes the socket at mode `0200`, replaces a stale socket after a recreate, accepts any Host, and a non-root user on the host cannot reach it.
- **The brain reads the admin address from the staged `caddy.json`** (`lifecycle.ControlPlaneCaddyAdmin`), not from `MOOSE_CADDY_ADMIN`. This departs from the issue, which had host-agent pass the socket in `MOOSE_CADDY_ADMIN`. The reason: the staged files are copied once, at first boot, and an OS update never replaces them, and `brainlaunch.Launch` leaves an existing brain container and its env alone. So no env value can be right for every box. The file is the one fact that matches the Caddy the box runs. The maintainer chose this over migrating old boxes in this change (`DECISIONS.md` 2026-10-13). host-agent still sets the TCP value, for a brain from before #608.
- **Dev keeps TCP `:2019`** (`dev/caddy.json` is unchanged): a unix socket does not cross a Docker Desktop bind mount on macOS. The brain attaches the dev Caddy to app networks the same way.
- **Tests.**
  - `internal/lifecycle/caddy_networks_test.go`: install connects after the network exists and before compose up; a failed connect does not fail the install; uninstall disconnects before compose down and network remove; start reconnects; the reconnect pass reconnects a running app, does nothing when Caddy is on the network, skips stopped apps and locked instances, and connects nothing when the inspect fails; the inspect parser; Docker's error wording.
  - `internal/lifecycle/controlplane_test.go`: the admin address for a socket, a socket with a mode, TCP, no listen, a socket outside the mount, and an unreadable file. `TestStagedControlPlaneUsesAdminSocket` reads the real `dev/control-plane/` files and checks that `caddy.json` and `compose.yml` agree.
  - `internal/caddy/unix_test.go`: a client on a real unix socket, with the Host header Caddy needs.
  - `internal/hostagent/brainlaunch/caddysocket_test.go`: create, repair the mode without touching contents, and refuse a file or a symlink.
  - `internal/lifecycle/caddynet_live_test.go` (`-tags dockerlive`, needs `make caddy`): a real install against real Docker and the dev Caddy. The app is on its own network and not on `moose-ingress`, Caddy reaches it by its alias, a dropped network comes back through `EnsureCaddyNetworks`, and uninstall removes the network. Passed locally.
- **Cloud lane, `access` boot, new step 6** (`dev/cloud/cloud-assertions.sh`): no managed container is on `moose-ingress`; Caddy is on the app network; from inside whoami's network namespace, `moose-caddy:2019`, `docker-proxy:2375` and `moose-brain:8080` refuse, by name and by their `moose-ingress` address, while `moose-caddy:80` answers as the positive control; the socket dir is `700` owned by the remap base; the admin API answers on the socket and not on TCP; the brain logged the socket address; after a compose recreate of Caddy, Caddy is back on the app network within 90s; after a brain restart, whoami answers through the recreated Caddy; uninstall removes the app network. The `remap` boot's route diagnostic now reads the admin API over the socket with a small perl-base helper, since the image has no curl. `VERDICT_TIMEOUT` for the access boot goes from 960 to 1290.

## How it maps to the specs

- `CONTROL_PLANE.md` # Locked: Caddy is moose substrate already said apps never join the network that carries the admin port and Caddy connects outward. It now says how that is built, including the socket, the dir owner, the address read from `caddy.json`, the reconnect pass and the dev difference.
- `CONTROL_PLANE.md` # Locked: Docker socket exposure: the escape is closed by network reach.
- `THREAT_MODEL.md` B2: the Brain↔Docker row is no longer a live gap; the blast-radius caveat and the accepted-risk list now name only the older-box residual.
- `APP_ISOLATION.md` # Inter-app traffic: how the networks enforce "denied".

## Known gaps & deviations

- **Hosted boxes made before #608 keep a TCP admin API.** Their apps are off `moose-ingress` too, so the proxy and the brain are closed there. But Caddy now sits on each app's network, so an app on such a box can reach `moose-caddy:2019` as it could before. Moving those boxes over is #612.
- **"After Caddy is recreated, the app answers again within one tick" is not met.** The brain puts Caddy back on the network within one tick, which the boot proves. But a recreated Caddy also lost every route, and the brain writes routes only at startup. The boot restarts the brain after the recreate, as host-agent's updater does. Writing the routes again while the brain runs is #613.
- **Release order.** A box image with this change must bake a brain with it, or the brain on a new box dials TCP `:2019`, which no longer exists. The next OS release must bump `CONTROL_PLANE_VERSION` too, as the issue said. A brain with this change on an older OS is fine: it reads `caddy.json` and stays on TCP.
- **Not built locally:** `cmd/host-agent-real` does not compile on this machine (its PAM headers are too old for the `pam` module), so CI is the first compile of `ensureCaddySocketDir`.
- **Appliance medium lane not run** (local-only). It stages the same `dev/control-plane/` files and runs host-agent-real, so it takes the same path.

## What's next

- #612: move older hosted boxes to the socket.
- #613: write Caddy's config again when it was recreated while the brain runs.
- #596 can add its brain-only Caddy listener as a second socket in the same `caddy/` dir.
