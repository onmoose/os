# The remap boots in CI / Cloud image, #531

- **Status:** done
- **Date:** 2026-09-30
- **Specs touched:** `TESTING.md` # Hosted cloud variant, `docs/dev/hosted-boot-proof.md`, `docs/architecture.md`, `CLAUDE.md` (the boot list)

Slice 6 of #523, the last one. It follows [userns-remap-on.md](userns-remap-on.md) (#530), which turned the remap on in both images and left the dedicated remap boots to this slice. It writes properly what the throwaway branch in [userns-remap-ci-proofs.md](userns-remap-ci-proofs.md) proved with its `remap` and `remap-reboot` boots. The spike branch was read, not merged.

## What was done

### Two new boots

`dev/cloud/run-cloud-tests.sh` gains the `remap` and `remap-reboot` boots, on one fresh overlay and one box-id (`moss-lynx`) of their own, seeded with a test-portal key like the access boot. One boot name, `remap`, runs both, because the second needs the disk the first left. The harness mints two owner assertions, one per boot, since the box spends a jti on first use. So no session crosses the reboot: each boot signs in again. The first boot gets a 1500s ceiling, the reboot 900s.

`dev/cloud/cloud-assertions.sh` gains the `remap|remap-reboot` scenario. Step 5d, which runs on every boot, already checks the daemon side (`name=userns`, `overlay2`, the proxy and the brain with `UsernsMode=host` as host uid 0, Caddy and `moose-ui` remapped as host uid 1000000), so the scenario does not repeat it. It uses the `remap_base` and `host_uid_of` that step 5d sets.

- **Both boots:** sign in, then check that the new SSO owner has no line in `/etc/subuid` or `/etc/subgid`, and that each file holds only the `moose-remap` line. Then `remap_checks`, the same function on both boots, checks every tier (below).
- **`remap`:** loads `postgres:16` from the test-only images dir, installs the five apps, runs `remap_checks`, checks the caps-tier container never restarted, then recreates it with the brain's own compose invocation (`compose -f compose.yml -f compose.override.yml --env-file .env -p moose-<id> up -d --force-recreate`), checks the container id changed, runs `remap_checks` again and checks the token is the same. It leaves the ids and the token in `/var/lib/moose-remap-test/state` for the reboot.
- **`remap-reboot`:** a real reboot of the same disk. It reads the state, runs `remap_checks` on what the box brought back and checks the token is still the same.

What `remap_checks` checks, per app. `base` is 1000000.

| App | Tier | Checks |
|---|---|---|
| `remapdrop` | default, folderless | `UsernsMode` empty, `Config.User` `0:0`, no `cap_add`, process at host uid `base`, data dir and `id.txt` at `base:base`, `id.txt` read through Caddy says `uid=0(root)` |
| `svcdrop` | default, `service_user` | `UsernsMode` empty, `Config.User` a uid from the 2100 to 2999 band, no `cap_add`, process at `base+uid`, data dir and `id.txt` at `base+uid:base+gid`, `id.txt` through Caddy says that uid |
| `rootsetup` | caps, `root_setup` | `UsernsMode` empty (never the host userns), no `user:`, `cap_add` exactly the five, `cap_drop` `ALL`, `no-new-privileges`, process at `base+33` (after its root start drops to `www-data`), data dir at `base:base`, `data/db` and its token at `base+33`, and the token through Caddy is the one in the file |
| `pgnote` and the managed `postgres-16` | default | `pgnote` remapped as `0:0`, host uid `base`. `moose-svc-postgres-16` remapped, process at `base+999`, data dir owned by uid `base+999`. pgnote's row is in its own database |
| `filedrop` household | host, folder app | `UsernsMode=host`, `Config.User` `2000:2000`, no `cap_add`, process at the real 2000, `/srv/moose/shared/Documents/filedrop.txt` at the real `2000:2001` |

### Synthetic fixtures

Four new test-only catalog apps in `dev/cloud/test/catalog/`, in the published wire shape, all checked with `moose manifest check` and packaged by `mkcatalog`:

- `remapdrop` and `svcdrop`: busybox, write `id` into `./data`, serve it. `svcdrop` sets `service_user: true`.
- `rootsetup`: busybox with `root_setup: true`. Its start does what a root entrypoint like poznote's does: as root it makes `data/db`, chowns it to `33:33` and then drops with `setuidgid www-data` to write a token once and serve it. In the default tier the chown fails and the container exits, so a running container shows the tier gave the capabilities. I ran the script against the local busybox image (no image built) with the default tier's flags and the caps tier's flags: it failed with the first and worked twice with the second, keeping its token. So no public image was needed for the `root_setup` app.
- `pgnote`: the `postgres:16` image, used only for its `psql`, on the managed Postgres 16. It writes one row into its own database once, then idles.

The folder app is the existing `filedrop`. The access boot keeps the `imageuser` fixture, so the image tier is not repeated here. `dev/cloud/test/bootstrap.sh` adds the four packages to the catalog snapshot, bakes `postgres:16` (pinned by digest) into the test-only `test-images` dir that only the remap boot loads, and bumps `CANARY_VERSION` to `v25`. The three busybox fixtures reuse the image the first-boot loader already loads.

### Running only some boots

`ci-cloud-image.yml` gains a `boots` input on `workflow_dispatch`: a space-separated list of boot names, empty for the full gate list. It reaches the script as the `MOOSE_CLOUD_BOOTS` env var, not as shell text. `run-cloud-tests.sh` now refuses a boot name it does not know, so a typo fails the run instead of running nothing and passing. A list of only spaces is refused too. A run with its own list must set `publish=false`: a new step fails the run at once otherwise, before the build, since publishing rests on the full gate list. The full gate list, for a dispatch, a tag push and the release call, is now `unseeded seeded bios access update ssh remap`. A pull request run still boots only `update`.

When a boot fails, `dump_serial` now also prints the whole diag block (without the `iptables-save` lines), because the 40-line tail cut the brain log out of it on the first red run below. A failed `remap_get` also prints the Caddy route ids.

## A bug the reboot boot found: #540

The first run (`boots=remap`, https://github.com/onmoose/os/actions/runs/36642566707) passed the whole first boot, and every app answered 404 through Caddy after the reboot. The brain was up a second before the Docker socket proxy answered, its one startup reconcile failed at `docker ps` ("startup reconcile failed ... reconcile: list actual: exit status 1"), and that pass is the only thing that puts app routes back. So any box that reboots loses all its app routes until someone stops and starts each app. The spike's reboot boot passed anyway, most likely because its wait restarted an app through the brain when its route stayed missing for a minute, which puts the route back.

This is a real bug outside this issue, so it has its own issue and PR: #540, fixed in #541 (the brain waits up to 15s for Docker before its startup Docker work, `startup-wait-docker.md`). This branch has `dev` merged in with that fix. A run on a throwaway branch with both changes (`test/531-with-540`, https://github.com/onmoose/os/actions/runs/36644428131) passed both remap boots.

## CI runs

| Run | Boots | Result |
|---|---|---|
| https://github.com/onmoose/os/actions/runs/36642566707 | `remap` | first boot passed, `remap-reboot` failed: every app 404 after the reboot (#540) |
| https://github.com/onmoose/os/actions/runs/36644428131 | `remap` | pass, on a throwaway branch with the #540 fix merged in |
| https://github.com/onmoose/os/actions/runs/36647641394 | all eight (`unseeded seeded bios access update ssh remap`) | pass, on this branch with `dev` (and #541) merged in |
| https://github.com/onmoose/os/actions/runs/36648928304 | all eight | pass, on commit 8a86efa, the code this PR merges (only this table changed after it) |

Quotes from the serial log of 36647641394. The first boot:

```
cloud-assertions: remap: the SSO owner 'owner' (host uid 2001) has no /etc/subuid or /etc/subgid range; each file holds only moose-remap:1000000:65536
cloud-assertions: remap [remap]: default tier, folderless: remapdrop UsernsMode='' user=0:0 cap_add=null host uid 1000000; data dir 1000000:1000000, id.txt 1000000:1000000; through Caddy: uid=0(root) gid=0(root) groups=0(root)
cloud-assertions: remap [remap]: default tier, service_user: svcdrop UsernsMode='' user=2100:2100 cap_add=null host uid 1002100; data dir 1002100:1002100, id.txt 1002100:1002100; through Caddy: uid=2100 gid=2100 groups=2100
cloud-assertions: remap [remap]: caps tier, root_setup: rootsetup UsernsMode='' user='' cap_add=["CAP_CHOWN","CAP_DAC_OVERRIDE","CAP_FOWNER","CAP_SETGID","CAP_SETUID"] cap_drop=["ALL"] security_opt=["no-new-privileges:true"] host uid 1000033; data dir 1000000:1000000, data/db 1000033:1000033; token through Caddy: cf7ffbde-5a94-4999-896a-4b083ffe90ec
cloud-assertions: remap [remap]: managed Postgres, default tier: moose-svc-postgres-16 UsernsMode='' host uid 1000999, data dir 1000999:1000000; pgnote UsernsMode='' user=0:0 host uid 1000000; its row in database pgnote_af6b: moose-531-note
cloud-assertions: remap [remap]: host tier, folder app: filedrop UsernsMode=host user=2000:2000 cap_add=null host uid 2000; /srv/moose/shared/Documents/filedrop.txt 2000:2001
cloud-assertions: remap: the caps-tier app kept its data across a container recreate (ef0367bb186a -> 03708c120f79), token cf7ffbde-5a94-4999-896a-4b083ffe90ec
```

The reboot printed the same five tier lines under `remap [remap-reboot]`, the same subordinate-range line, and:

```
cloud-assertions: remap-reboot: every tier checked again after a real reboot of the same disk; the caps-tier app kept its data (token cf7ffbde-5a94-4999-896a-4b083ffe90ec)
```

Every boot, the two remap boots included, printed step 5d's "userns-remap on (moose-remap:1000000:65536, SUB_UID_COUNT 0); proxy + brain in the host userns (host uid 0), caddy + moose-ui remapped (host uid 1000000)".

**Time.** In 36647641394 the two remap boots took 86 seconds (00:07:17 to 00:08:43), and the boot step 9m23s. The last six-boot run before this change (36636692363) had a boot step of 8m00s. So the new boots add about a minute and a half, plus a few seconds of build to bake the `postgres:16` tar.

## Review

- **Fresh Sonnet agent:** two Block findings, both also raised by Greptile as P1, both confirmed and fixed.
  1. A manual run with `-f boots=remap` and the default `publish=true` would have published an image proven by only two boots. A new step, "Assert a run with its own boot list publishes nothing", fails such a run before the build. The docs that show `-f boots=remap` now also show `-f publish=false`.
  2. A `boots` value of only spaces split into no names, so the name check never ran, no boot ran, and the script printed PASS. The script now counts the names and refuses a list with none.
  - Two Notes. (3) The CI runs section was still a placeholder (also Greptile P2): filled in above. (4) The managed Postgres health wait fell through with no message when it timed out: it now fails with the container status and its log.
- **Greptile:** the same three findings as above (two P1, one P2), handled as above.

## How it maps to the specs

- `APP_ISOLATION.md` # User-namespace tiers: each of the four tiers now has a check on a booted box, on every full run of the lane. The default, caps and host tiers in these boots, the image tier in the access boot. So does "Data ownership follows the tier" for each tier and for managed services, and "Capabilities never come with the host user namespace" for the caps-tier app.
- `BUILD.md` # User-namespace remap: `SUB_UID_COUNT 0` is shown to work for a real new account, the SSO owner.
- `TESTING.md` # Hosted cloud variant: the new boots, written up there.

## Known gaps & deviations

- **A pull request run does not boot `remap`.** It still boots only `update`, to keep the PR cost near the build cost. Every dispatch, tag push and release call runs it. A PR that changes the tiers can run it with `-f boots=remap`.
- **The recreate is done by the lane, not by the brain.** No brain path recreates one app's container today (the app update path is not built). The lane runs the same compose command the brain runs.
- **The no-remap refusals are still only unit-tested.** Every lane box runs the remap, as [userns-remap-on.md](userns-remap-on.md) says.
- **Not proved here:** a real provisioned box (a real provider VM with a real seed), and the GPU and `devices` host-tier cases, which need hardware the lane does not have.

## What's next

1. The rest of #523: the remap on a real provisioned box, and the GPU host tier on real hardware.
2. The full appliance medium lane on a machine other than the one used for #530.
3. `capabilities.yml`: the `userns-remap` capability, with the first catalog app proven on it.
