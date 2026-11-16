# A hosted box in the inner loop

- **Status:** done
- **Date:** 2026-11-16
- **Specs touched:** none (dev how-to only: `docs/dev/running-locally.md`, root `README.md`)

Closes #673, after [dev-app-hosted-only.md](dev-app-hosted-only.md). That entry let `make dev-app` install a `hosted_only` app on the appliance profile, but its gap was that the dev boot does not test the hosted gate. A terminal app is meant to be tried as the owner of a hosted box: portal sign-in, then the app behind the forward-auth gate. Doing that locally took a long hand recipe, and even then four things broke under `make dev` (listed in #673).

## What was done

- **`make dev HOSTED=1`.** It uses its own state dir, `.dev/state-hosted`. On the first run it writes the `hosted` profile marker and a seed (`box_id`, `assertion_verification_key`) under `hosted/`, and checks that the dashboard name and each `APP` name resolve to this machine, printing the `/etc/hosts` line when one does not. It exports `MOOSE_PROFILE_PATH`, `MOOSE_SEED_PATH`, `MOOSE_DEV_HOME`, `MOOSE_UI_HOST` and `MOOSE_DEV_PLAIN_HTTP`. When the stack starts it sets both dashboard upstreams to this machine, so the brain installs the dashboard route and the forward-auth check reaches the brain. `BOX=` names the box (default `devbox`). A state dir made for another box is refused.
- **`make dev-login`.** It signs a fresh one-time token with the kept key and prints the `/_moose/sso` URL. It always reads the hosted state dir.
- **`mkassertion -key <file>`.** It reads the private key from the file, or makes one and writes it with mode `0600`. `-tokens 0` prints only the public key. Without `-key` the cloud lane works as before.
- **Caddy dials the `moose-ingress` gateway on Linux**, read at start, and `host.docker.internal` on Docker Desktop. The first try used `host.docker.internal` through `host-gateway` on Linux too. That is the `docker0` address, and once Caddy has joined an app network its traffic to it leaves through that app's bridge. So a firewall rule for one bridge did not cover the app routes. Traffic to the `moose-ingress` gateway always leaves on `moose-ingress`.
- **Vite** listens on `MOOSE_UI_HOST` when set and accepts `*.onmoose.io` host names.
- **The fake host-agent's home** is `Agent.DevHome`, set from `MOOSE_DEV_HOME`. Every fake identity call reads it. Empty means `$HOME`, as before.
- **`MOOSE_DEV_PLAIN_HTTP`.** `profile.UseHTTPForDev` makes `HostedAppURL` and `HostedLoginURL` plain `http`. The install event's URL now comes from `HostedAppURL` too, so it follows. Nothing on a box sets it.
- **`make dev APP=<id>`** seeds the apps like `make dev-app`. `dev-app` stays. Off hosted it still sets `MOOSE_DEV_ALLOW_HOSTED_ONLY`; on hosted it is not needed.
- **`make clean`** wipes `$(STATE_DIR)`, so `make clean HOSTED=1` resets the hosted box.
- **Tests.** `mkassertion`: the key file is written `0600`, read back as the same key, and a bad file is refused. host-agent: with `DevHome`, resolve-home reports it and prepare-folder makes `Projects` there and not in `$HOME`.

## How it was checked

Booted `make dev HOSTED=1 APP=opencode` against the store's `catalog/opencode` branch (onmoose/store#309). The dashboard loaded through Caddy, `make dev-login` signed in as `owner` (both cookies set, a replayed token gets `401`), OpenCode installed with its Projects folder bound from `.dev/state-hosted/home/Projects`, and the maintainer opened the dashboard and the app in a browser. That run had this machine's firewall in place (`INPUT` policy `DROP`) with a rule for the `moose-ingress` bridge only, and Caddy had joined OpenCode's app network, so the app routes went through the gateway address as described.

## Known gaps & deviations

- **Review.** The review agent found no Block. Its note that `getent` is missing on macOS, so the hosts check could never pass there, is fixed: without `getent` the check reads `/etc/hosts`. Its other notes were dropped as small: no test for `UseHTTPForDev`, and an `APP` set in the shell environment now changes plain `make dev`. Its question about the gateway routing is answered by the boot above.

- **A host firewall can block it.** On a machine whose `INPUT` policy is `DROP`, Caddy cannot reach the brain or Vite and every page is a `502`. `running-locally.md` gives the `iptables` rule for the `moose-ingress` bridge. Nothing detects it.
- **One `/etc/hosts` line per app**, and the check assumes the slug is the app id. A slug that differs (taken, or a later `preferred_slugs` entry) needs its own line.
- **Plain http only.** The wildcard cert and `:443` are not part of the dev loop. The cloud boots cover them.
- **Vite listens on every address** in this mode, so on a machine without a firewall the dev dashboard is reachable from the LAN, as the brain on `:8080` already is.

## What's next

Nothing queued.
