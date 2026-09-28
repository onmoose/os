# userns-remap CI proofs: the booted cloud image, #516

- **Status:** done
- **Date:** 2026-09-28
- **Specs touched:** none. The spec changes a go needs are listed under What's next.

This follows Step 0 of #516, [`userns-remap-pretest.md`](userns-remap-pretest.md). Step 0 ran the catalog on a GCE box with a fake host-agent and a native brain, and said go. This entry is the next and harder check: the real hosted production image, booted in the `CI / Cloud image` QEMU lane, with host-agent-real, the containerized brain and the real control plane, on a Docker daemon with a daemon-wide `userns-remap`.

Short answer: **go.** Every proof the lane can run passed. The control plane needed two small changes in host-agent-real: `--userns=host` for the socket proxy and the brain, and a tmpfs on the proxy's `/run`. No capability was given back to anything. The six usual boots, including the control-plane update and revert, also pass on the remapped image. Two things the run found are not caused by the remap: a containerized brain cannot prepare a household shared folder, and one app lost its Caddy route once after a failed admin call.

## What was done

### The spike branch

All code is on the throwaway branch `test/516-userns-remap-ci-proofs`, last commit `78775a6`. It is never merged. A copy of the same commit, `test/516-userns-remap-ci-proofs-full`, ran the usual boots in parallel.

- **Brain:** the Step 0 spike code, brought over as is from `test/516-userns-remap-harness` (`internal/lifecycle/spike_userns.go` and the three hooks marked `SPIKE (#516)`). It is off unless `MOOSE_SPIKE_USERNS_BASE` is set. Folder and GPU apps get `userns_mode: host`. Apps in `MOOSE_SPIKE_USERNS_CAPS_APPS` get `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE` and `FOWNER` back and lose their `user:` pin. Everything else stays remapped with `cap_drop: ALL`. Bind dirs are chowned to base+uid, and managed-service data dirs to base.
- **Image** (`dev/cloud/`, so the production image, not only the test lane):
  - `daemon.json` gains `"userns-remap": "moose-remap"` next to the journald log driver.
  - `mkosi.postinst.chroot` creates a `moose-remap` system account, writes `moose-remap:1000000:65536` to `/etc/subuid` and `/etc/subgid`, and sets `SUB_UID_COUNT 0` and `SUB_GID_COUNT 0` in `/etc/login.defs`.
  - No package was added. The lean check still passes, with the manifest matching `expected-packages.txt` exactly at 162 packages (run 36490832011).
- **host-agent-real** (`internal/hostagent/brainlaunch`, `cmd/host-agent-real/main.go`):
  - `RunSpec` gains `UsernsMode` and `Tmpfs`, passed to `docker run` as `--userns` and `--tmpfs`.
  - The socket proxy and the brain always run with `--userns=host`. We did not detect the remap from `docker info` and did not add an image flag. `--userns=host` is a no-op on a daemon without remap (checked on Docker 28 locally), so the simplest honest choice is to always pass it. It cannot drift from `daemon.json`, and it needs no extra Docker call. The update path builds the brain from the same `RunSpecFor`, so a brain an update recreates keeps it too.
  - The proxy gets `--tmpfs /run`. See # The socket proxy.
  - Two spike-only inputs, both from the host-agent environment: `MOOSE_SPIKE_USERNS_BASE` and `MOOSE_SPIKE_USERNS_CAPS_APPS` are forwarded into the brain container, and `MOOSE_SPIKE_SHARED_ROOT` is bind-mounted into the brain at the same path when it exists on the host. The test lane sets them in a new `30-spike-userns.conf` drop-in beside the existing `20-` one.
- **Test lane** (`dev/cloud/test/`, `dev/cloud/cloud-assertions.sh`, `dev/cloud/run-cloud-tests.sh`):
  - Test-only catalog fixtures in the published wire shape: `poznote` (copied from the harness branch), `memos` and `memos-pg` (`service_user: true`, the second on managed Postgres 16), and `memos-shared` (a `documents` write grant). The lane is air-gapped, so the poznote, memos and `postgres:16` images are baked by digest into `/var/lib/moose/test-images/`. Only the remap boot loads them.
  - Every boot now checks the remap and the userns split of the control plane, at start and again after its scenario.
  - Two new boots, `remap` and `remap-reboot`, over one fresh overlay, like the `frozen` pattern. The first installs the proof apps. The second boots the same disk again.
  - The workflow gains a `boots` input for `workflow_dispatch`, so the branch could ask for `remap` alone.
- **Proof 9** is a table test, `TestUsernsSpikeNeverHostWithCaps` in `internal/lifecycle/spike_userns_test.go`.

### CI runs

All are `CI / Cloud image` with `publish=false`.

| Run | Boots | Result |
|---|---|---|
| 36490832011 | `unseeded seeded bios access update ssh` | pass, all six, on the remapped image |
| 36490829133 | `remap` | fail at proof 2: poznote had no Caddy route (see # Not caused by the remap) |
| 36492250153 | `remap` | fail at proof 6: the check wanted gid base+999 too, but Postgres only changes the owner uid. The check was wrong, not the box |
| 36493227634 | `remap remap-reboot` | pass |
| 36494198194 | `remap remap-reboot` | pass, with the full error text of the shared-tree install kept |

### The proofs

Proof numbers are the ones #516 uses. Quotes are serial lines from the runs named.

| # | Proof | Result | Evidence |
|---|---|---|---|
| 1 | Control plane up on a remapped daemon | **pass** | Every boot in 36490832011, 36493227634 and 36494198194: "security options: [... "name=userns" ...]" and "proxy + brain UsernsMode=host (host uid 0), caddy + moose-ui remapped (host uid 1000000); proxy tmpfs={"/run":""}". The dashboard, `/api` and SSO answer in every boot. "whoami installed and routed through Caddy (status HTTP/1.0 200 OK), remapped, host uid 1000000" (36494198194). |
| 2 | poznote end to end | **pass** | 36494198194: "CapAdd=["CAP_CHOWN","CAP_DAC_OVERRIDE","CAP_FOWNER","CAP_SETGID","CAP_SETUID"] User='' host uid 1000000 restarts=0", "note 2 created and read back over HTTP through Caddy", "host owner of poznote data=1000082:1000082 db=1000082:1000082", "note survives a container recreate (0603ec1bb4f2 -> 981ccdbe15a2)", and after the reboot: "note 2 read back after a real reboot, data owner 1000082:1000082". The recreate uses the brain's own `docker compose ... up -d --force-recreate`. |
| 3 | A small catalog set | **pass** | whoami (above), memos and memos-pg, each healthy through Caddy with a first user created over its API: "memos installed, healthy through Caddy, first user created; user=2100:2100 data owner=1002100:1002100" and "memos-pg ... user=2101:2101 data owner=1002101:1002101". The whole catalog was Step 0's job and is not rerun here. |
| 4 | Household `moose-shared` folder grant | **pass**, after a harness fix | "household folder app in host userns (user 2000:2000, group_add ["2001"], host uid 2000) wrote /srv/moose/shared/Documents/memos_prod.db as 2000:2001; Documents is 0:2001 2770". It needed the shared tree mounted into the brain. See # Not caused by the remap. |
| 5 | GPU app | **not tested** | No GPU in the QEMU lane. Out of scope here. |
| 6 | Managed Postgres at base+999 | **pass** | "managed postgres-16 remapped (UsernsMode='', host uid 1000999), data owner 1000999:1000000, memos-pg wrote its user row to db memos_pg_1c06". The row was read back with `psql` in the service container. The group stays base+0, the owner the brain hands it, because the Postgres entrypoint changes only the owner uid. |
| 7 | Two `service_user` apps, two identities | **pass** | "two service_user apps, two identities: memos=2100 (host 1002100), memos-pg=2101 (host 1002101)". The data dir owner and the host uid of the running process both match. |
| 8 | Host power is gone | **not rerun** | Plain-Docker probe, passed in Step 0. Nothing in this lane changes it. |
| 9 | Never host userns with restored caps | **pass** | `TestUsernsSpikeNeverHostWithCaps` runs the real `writeOverride` for folderless, `service_user`, folder, GPU and folder+GPU manifests, each in and out of the caps list, and checks every service: never `userns_mode: host` with `cap_add`, `cap_drop: [ALL]` in every tier, and the expected tier. It also checks that the install-time refusal fires for exactly the host-tier shapes in the caps list. A deliberate break (caps added in the host branch) makes it fail. |

### The socket proxy

We predicted the failure before the first CI run. We built a copy of `tecnativa/docker-socket-proxy:v0.4.2` with every file owned by uid 1000000, which is how a host-userns container sees its image on a remapped daemon, and ran it as root with `cap_drop: ALL`. haproxy exited with "[ALERT] (1) : [haproxy.main()] Cannot create pidfile /run/haproxy.pid". `/run` is `0755` and owned by the remapped root, and real root with no `DAC_OVERRIDE` is just another uid there. `/tmp` is `1777`, so the entrypoint's `/tmp/haproxy.cfg` still works, and haproxy only reads `/var/lib/haproxy`.

The fix is `--tmpfs /run`. A fresh tmpfs is owned by real root, so haproxy can write its pid file. The proxy keeps `cap_drop: ALL` and `no-new-privileges`, and gets no capability back. The same image with the tmpfs answered `_ping` locally, and on the booted image it ran in every boot. The serial log shows the reason for the fix on disk: "proxy image /run owner on disk: 1000000:1000000".

The brain needed no fix. It keeps Docker's default capabilities (#442 is the open item to narrow them), so the remapped owner of its image files does not stop it. It relaunched cleanly on the remapped daemon in the remap boot, and the update boot recreated it on new images and back again.

### The image store

- The image installs docker-ce 29.8.1. With the remap on, Docker turns the containerd image store off by itself. It reports `driver=overlay2`, with no snapshotter in `DriverStatus`, and `root=/var/lib/docker/1000000.1000000`.
- **Nothing in the control plane broke on the classic store.** The first-boot `docker load` of the baked control-plane images worked in every boot, and so did the `docker load` of the proof app images. The `update` boot passed on the remapped image: an in-guest registry, `docker commit` and `docker push` of a new pair, `docker rmi`, the updater's pull by digest, the recreate, the failed update with its revert, and the seed-driven target path. After it, "moose-brain is still UsernsMode=host (image 127.0.0.1:5000/moose-brain@sha256:6b1dd316...)" (36490832011).
- **Not tried:** a buildkit build under the remap, which Option A (the install-time re-own layer) would need, and remap with the containerd snapshotter forced on.

### Not caused by the remap

- **A containerized brain cannot prepare a household shared folder.** The first `memos-shared` install, as household, failed with "prepare shared source \"/srv/moose/shared/Documents\": shared root \"/srv/moose/shared\": stat /srv/moose/shared: no such file or directory" (36494198194). `prepareSharedSource` (#156) stats the shared root from inside the brain, and host-agent mounts only `/var/lib/moose`, the agent socket dir and the profile marker into the brain (the diag block lists those three). So a household or shared-source folder install fails on any box whose brain runs in a container, which is every real box, and household is the default scope for an admin. In this run the host had no shared tree either: the hosted image creates no `/srv/moose/shared`. The code and the mount list show that creating it on the host alone would not be enough. For the proof, the harness created the tree (`root:moose-shared`, `2770`), removed the brain container and restarted host-agent, so the brain came back with the tree mounted (`MOOSE_SPIKE_SHARED_ROOT`). The same install then passed.
- **One app lost its Caddy route once.** In 36490829133 the brain logged "caddy upstream flip failed (continuing) ... caddy admin unreachable: Put \"http://moose-caddy:2019/config/apps/http/servers/moose/routes/0\": read tcp 172.18.0.3:35212->172.18.0.4:2019: read: connection reset by peer", and poznote answered 404 from then on. `upsertRoute` removes the old route before it PUTs the new one and ignores the remove's error, so one failed PUT leaves the app with no route until something installs it again. It happened once in four remap boots. The access boot, the only other boot that installs an app, did not hit it. We cannot say from this run whether the remap makes it more likely. The later remap boots restart an app through the brain once if its host is still 404 after a minute, and log it. That never fired.

## How it maps to the specs

- `APP_ISOLATION.md` # Runtime identity & data ownership: the three tiers hold on a booted image with host-agent-real. The brain picks host userns from the manifest (folders, GPU), and the caps tier from a list, never from a manifest UID. The `service_user` band from host-agent-real gives distinct host identities under the remap (base+2100, base+2101).
- `APP_ISOLATION.md` # Not in v1: "User namespace remap. Breaks too many images" does not hold for the control plane or the proof set on the production image.
- `CONTROL_PLANE.md` # Locked: control-plane container hardening: the proxy keeps its full sandbox under the remap, with a tmpfs `/run`. The brain's residual (default capabilities) is unchanged and now also covers the remapped owner of its image files.
- `THREAT_MODEL.md` B2 and the container escape row: the proxy and the brain stay real root in the host user namespace. Everything else, Caddy and `moose-ui` included, runs as the remapped range.
- `ENVIRONMENT.md` # Provisioning & first-boot and `BUILD.md`: the remap is image configuration (`daemon.json`, `/etc/subuid`, `/etc/subgid`, `login.defs`). No per-box setting, so the seed is not involved.
- `UPDATES.md` # 8: the control-plane update and revert work on the classic store.

## Known gaps & deviations

- **QEMU lane only.** Proofs 2 and 3 on a provisioned hosted box, and proof 5 on a box with a GPU, are still open, as #516 asks.
- **The appliance image was not built.** Only the hosted image carries the remap. The appliance medium lane (`dev/test-qemu`, local only) was not run, and its image has no `moose-remap` account yet.
- **A small catalog set.** Four apps plus the managed Postgres, not the whole catalog. Step 0 ran the catalog under the remap with a native brain. No `group_add` catalog app was in this set, other than the `moose-shared` grant.
- **Proof 4 needed a harness fix** that is not a product change: the shared tree mounted into the brain, and the tree created by the test. See # Not caused by the remap.
- **The route-loss workaround** in the remap boot could hide a repeat of that bug. It logs when it fires, and it never did.
- **Always `--userns=host`** means `docker inspect` shows `UsernsMode=host` for the proxy and the brain even on a daemon without remap. That is correct but less telling than a detected value.
- **Proof 8 was not rerun**, and a buildkit build under the remap was not tried.
- **`/etc/subuid` after a user is made** was not read. host-agent created the owner account with `useradd` in every SSO boot, but the lane only checks that `login.defs` says `SUB_UID_COUNT 0` and that the `moose-remap` range is there, not that the owner got no range.

## What's next

1. **Go: write the implementation issue.** It starts from the model proven here and in Step 0. It should cover:
   - The `daemon.json` change, the `moose-remap` account and ranges, and `SUB_UID_COUNT 0` / `SUB_GID_COUNT 0`, in **both** images (hosted and appliance), with the appliance medium lane run once.
   - `--userns=host` for the proxy and the brain, and `--tmpfs /run` for the proxy, as real code in `brainlaunch`, not a spike.
   - The three tiers in `writeOverride`, with the host tier chosen by the brain from folders and GPU. The generator and the install check keep the rule that host userns never comes with restored capabilities, and the proof 9 test moves with them.
   - **A manifest intent field for the caps tier**, in place of the spike's id list. It is for folderless apps only: admission refuses it together with `folders` or `gpu: true`. It names an intent (the image's entrypoint must chown or drop privileges as root), never a UID or a capability list.
   - Bind-dir chown to base+uid and managed-service data to base, from one base the brain reads (not an env var set by hand).
   - A remap boot in `CI / Cloud image`, so the lane keeps proving this after the change.
2. **Fix the shared-folder bug on its own**, before or with the implementation, since it breaks household folder installs on every real box today, with or without the remap: the brain cannot see `/srv/moose/shared`, and the hosted image does not create it. Either host-agent prepares shared sources (as it does for Files), or the tree is mounted into the brain.
3. **Look at the route flip.** `upsertRoute` removes before it adds, so one failed admin call leaves an app with no route. It is rare, and not shown to be caused by the remap.
4. **Real-box proofs:** poznote and the proof set on a provisioned hosted box with the remapped image, and a GPU app on a box with a GPU (proof 5).
5. **Spec changes, with the implementation:**
   - `APP_ISOLATION.md`: # Not in v1 drops "User namespace remap. Breaks too many images". # Runtime identity & data ownership gains the three tiers, says the brain picks host userns and never a manifest, adds the caps-tier intent field, and states **the folder-app limit**: the remap only helps folderless apps, a folder app stays in the host tier on today's sandbox, and a folder app whose image needs root at start must be packaged to start without it. Mapping one container to one real user would need per-container ID maps, which stays in # Not in v1.
   - `APP_MANIFEST.md`: the caps-tier intent field, and its admission rule.
   - `CONTROL_PLANE.md` # Locked: control-plane container hardening: the proxy and the brain run with `--userns=host`, and the proxy gets a tmpfs `/run`, with the reason.
   - `THREAT_MODEL.md`: the container escape row says every remapped app shares one range, so a breakout from one remapped app reaches the files of the others but not real host root. The caps tier gives capabilities inside the namespace. Folder, GPU and control-plane containers keep today's exposure.
   - `DECISIONS.md`: a new entry that turns on daemon-wide remap and records why not sysbox CE.
   - `BUILD.md` and `ENVIRONMENT.md`: the `daemon.json` change, the `moose-remap` range, `SUB_UID_COUNT 0` in `login.defs`, and that the box runs Docker's classic `overlay2` store because the remap turns the containerd store off.
   - `NEXT.md` # User-namespace remap for hardcoded-internal-UID app images: close it, or narrow it to the real-box and GPU proofs if they are still open.
