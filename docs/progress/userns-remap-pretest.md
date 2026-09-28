# userns-remap pre-test: Step 0 of #516

- **Status:** done
- **Date:** 2026-09-28
- **Specs touched:** none. The spec changes a go needs are listed under What's next. `docs/dev/spikes/per-app-userns-remap.md` gets a dated correction note.

This is Step 0 of #516. Before we trust any spike code, it asks two questions on a real Debian box. Does the catalog still boot when Docker runs with a daemon-wide `userns-remap`? And do the containers that opt out with `userns_mode: host` break on image files owned by the remapped range? It follows the June spike, [`../dev/spikes/per-app-userns-remap.md`](../dev/spikes/per-app-userns-remap.md), and corrects its sysbox claim.

Short answer: **go.** No app failed only under the remap. poznote and mealie, which crash-loop today, pass under the remap with the five capabilities given back. Every other failure happens in all modes and has a cause outside the remap.

## What was done

### The run

- **Box:** GCE `e2-standard-4` (4 vCPU, 16 GB RAM, 50 GB disk), Debian 13, kernel `6.12.107+deb13-cloud-amd64`, Docker `29.8.1`. It was built with the store repo's curation-box tooling: no service account, IAP-only SSH, idle shutdown.
- **Harness:** `dev/spike-userns-remap/` and `internal/lifecycle/spike_userns.go` on branch `test/516-userns-remap-harness`, commit `ce9c83e`. That branch is never merged. It runs the **real brain as root**, as in production, with the **fake host-agent** as the operator, so folder apps still run as a real non-root user.
- **Modes:**
  - **A:** Docker 29 default, containerd image store, no remap.
  - **B:** classic `overlay2` store, no remap.
  - **C:** classic `overlay2` with `userns-remap` on host UIDs 1000000 to 1065535, owned by a `moose-remap` system account. In C the brain uses the three tiers from #516. Apps are remapped with `cap_drop: ALL` by default. poznote (and mealie in one extra pass) gets `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE`, `FOWNER` back. Folder and GPU apps get `userns_mode: host`.
- **Apps:** 58 listed store apps plus a poznote fixture. Each app is pulled cold, installed through the API, watched for one minute, measured, then uninstalled.
- **Reruns:** apps that were refused for missing install config got plain test values and ran again in C, then in A. memos ran again in A and C with the merged package from onmoose/store#141 (`service_user: true`). forgejo and langfuse were retried. mealie ran once more in C with the caps tier. B was not rerun, so those apps show `refused` in B. When a mode has two lines for one app, the later line is the one shown below.
- **Raw results:** `A.jsonl`, `B.jsonl`, `C.jsonl`, `probes.txt` and the brain logs stay on the box under `~/remap-results/`. The task's local copy is in the session scratchpad. None of it is checked in.

### Result by class

59 apps in the list.

| Class | Count | Apps |
|---|---|---|
| Pass in A and C | 47 | Every row below marked `pass` in both A and C |
| Fail in A and C, not the remap | 9 | cap, forgejo, jotty, langfuse, mealie (default tier), nocodb, open-seo, plane, trigger-dev |
| **Fail only in C** (a remap regression) | **0** | none |
| Pass only in C | 2 | poznote and mealie, both in the caps tier |
| Fail only in A (store switch, not the remap) | 1 | twenty, a timing edge |
| Not testable | 1 | windmill |

Of the 47, three booted with values that are not real: openmuse used placeholder OpenAI and CopilotKit keys, and hermes-agent and openclaw used a custom model URL with nothing behind it. They prove the containers start under the remap. They do not prove the AI features work. Six more (chatto, coneshare, cube, databag, openmausbot, uptimepage) only needed plain test values, like an owner email.

### Per-app table

`C data owner` is the host owner of `<instance>/data` after install. It only means something where the app binds `./data`. `0:0` means the brain did not chown that directory, because the app binds other paths or is a folder app. `C userns` is what Docker used for the app's containers.

| App | A | B | C | C data owner | C userns | Note |
|---|---|---|---|---|---|---|
| actual-budget | pass | pass | pass | `1001000:1001003` | remapped |  |
| appsmith | pass | pass | pass | `1001000:1001003` | remapped |  |
| audiobookshelf | pass | pass | pass | `0:0` | host |  |
| baserow | pass | pass | pass | `0:0` | remapped |  |
| blinko | pass | pass | pass | `1000000:1000000` | remapped |  |
| calnode | pass | pass | pass | `1001000:1001003` | remapped |  |
| campfire | pass | pass | pass | `0:0` | remapped |  |
| cap | fail | refused | fail |  |  | fail both: its compose pulls `quay.io/minio/minio`, which now needs a login. Ran with a placeholder Resend key |
| chatto | pass | refused | pass | `0:0` | remapped | needed install config; B not rerun |
| coneshare | pass | refused | pass | `0:0` | remapped | needed install config; B not rerun |
| cube | pass | refused | pass | `0:0` | remapped | needed install config; B not rerun |
| databag | pass | refused | pass | `1001000:1001003` | remapped | needed install config; B not rerun |
| docuseal | pass | pass | pass | `0:0` | remapped |  |
| excalidraw | pass | pass | pass | `0:0` | remapped |  |
| files-demo | pass | pass | pass | `0:0` | host |  |
| firecrawl | pass | pass | pass | `0:0` | remapped |  |
| flipt | pass | pass | pass | `0:0` | remapped |  |
| forgejo | fail | fail | fail |  |  | fail both: Codeberg serves the pinned index, but its amd64 manifest answers 404 |
| freellmapi | pass | pass | pass | `1001000:1001003` | remapped |  |
| gitea | pass | pass | pass | `0:0` | remapped |  |
| gods-eye-view | pass | pass | pass | `0:0` | remapped |  |
| hermes-agent | pass | refused | pass | `1001000:1001003` | remapped | boot only: a custom model URL with no model behind it |
| immich | pass | pass | pass | `0:0` | host |  |
| jellyfin | pass | pass | pass | `0:0` | host |  |
| jotty | fail | fail | fail |  |  | fail both: admission refuses its `user: "0:0"` |
| jupyter | pass | fail | pass | `1000:1003` | host | one unhealthy start in B only |
| kan | pass | pass | pass | `0:0` | remapped |  |
| kimai | pass | pass | pass | `1000000:1000000` | remapped |  |
| laminar | pass | pass | pass | `0:0` | remapped |  |
| langfuse | fail | fail | fail |  |  | fail both: `quay.io/minio/minio` needs a login |
| lanraragi | pass | pass | pass | `0:0` | host |  |
| listmonk | pass | pass | pass | `0:0` | remapped |  |
| mealie | fail | fail | pass | `1000911:1000911` | remapped | fail both in the default tier: its root entrypoint chowns and drops to uid 911, which `cap_drop: ALL` refuses. **Passes in C with the caps tier** |
| memos | pass | fail | pass | `1001000:1001003` | remapped | merged package (`service_user: true`). B is the old package, not rerun |
| navidrome | pass | pass | pass | `0:0` | host |  |
| nextcloud | pass | pass | pass | `1001000:1001003` | remapped |  |
| nocodb | fail | fail | fail |  |  | fail both: `nocodb` is reported unhealthy about 30 s after start, so its worker never starts. Cause not found |
| novu | pass | pass | pass | `0:0` | remapped |  |
| open-seo | fail | fail | fail |  |  | fail both: not healthy inside the 2-minute health wait. It was healthy when looked at later |
| open-webui | pass | pass | pass | `1000000:1000000` | remapped |  |
| openclaw | pass | refused | pass | `0:0` | remapped | boot only: a custom model URL with no model behind it |
| opendray | pass | pass | pass | `0:0` | remapped |  |
| openmausbot | pass | refused | pass | `0:0` | remapped | needed install config; B not rerun |
| openmuse | pass | refused | pass | `0:0` | remapped | boot only: placeholder OpenAI and CopilotKit keys |
| osiris | pass | pass | pass | `0:0` | remapped |  |
| paperless-ngx | pass | pass | pass | `0:0` | host |  |
| penpot | pass | pass | pass | `0:0` | remapped |  |
| plane | fail | fail | fail |  |  | fail both: `quay.io/minio/minio` needs a login |
| pocket-id | pass | pass | pass | `1001000:1001003` | remapped |  |
| postiz | pass | pass | pass | `0:0` | remapped |  |
| poznote | fail | fail | pass | `1000082:1000082` | remapped | **pass only in C** (caps tier), the point of #516. Crash-loops in A and B |
| searxng | pass | pass | pass | `0:0` | remapped |  |
| trigger-dev | fail | fail | fail |  |  | fail both: `quay.io/minio/minio` needs a login |
| twenty | fail | pass | pass | `0:0` | remapped | fail in A only: `compose up` hit its 2-minute limit. It needs 121 to 124 s in B and C too |
| unleash | pass | pass | pass | `0:0` | remapped |  |
| uptimepage | pass | refused | pass | `0:0` | remapped | needed install config; B not rerun |
| vaultwarden | pass | pass | pass | `1000000:1000000` | remapped |  |
| whoami | pass | pass | pass | `0:0` | remapped |  |
| windmill | refused | refused | refused |  |  | not testable: unlisted in the store, so not in the seeded catalog |

### What the owners show

- **Remapped root lands at the base.** Folderless apps that run as the brain's euid (root) own their data at `1000000`, not `0`: blinko, kimai, open-webui, vaultwarden.
- **`service_user` apps own their data at base+UID** (#516 proof 7, in part): actual-budget, appsmith, calnode, databag, freellmapi, hermes-agent, memos, nextcloud and pocket-id all show `1001000:1001003`. That is base+1000, because the fake host-agent hands every `service_user` app the operator's own uid (1000). So this run shows the offset works. It does **not** show that two apps get two different identities. That needs host-agent-real, which allocates from 2100 up.
- **Managed services own their data at base+UID** (#516 proof 6, in part). In C, the data of `postgres-15`, `postgres-16`, `postgres-18`, `mariadb-11-4`, `mysql-8-4` and `valkey-8` is owned by `1000999` on the host. In B the same Postgres data is owned by `999`. Every app bound to them passed.
- **poznote owns its data at base+82** (`1000082`), as #516 proof 2 expects. mealie owns its data at base+911 (`1000911`).
- **Folder apps run with host userns.** All eight folder apps (audiobookshelf, files-demo, immich, jellyfin, jupyter, lanraragi, navidrome, paperless-ngx) show `userns_mode=host` on every container. Every other app shows the daemon default, which is the remap. None broke on image files owned by the remapped range.

### Probes (mode C, plain Docker)

From `probes.txt`, 2026-09-28, Docker 29.8.1:

| Probe | Result |
|---|---|
| Proof 8: remapped root **with** the five capabilities cannot read, write or chown a real-root `0700` bind | PASS |
| Container uid 0 is host 1000000, container uid 82 is host 1000082 | PASS |
| plunk image: its own USER (1001) writes its baked `/app` and `/etc/nginx`, remapped, `cap_drop: ALL`, no capabilities back | PASS |
| formbricks image: its own USER (1001) recreates its baked migrations dir, remapped, `cap_drop: ALL`, no capabilities back | PASS |
| Proof 1 shape: a host-userns root container can use `docker.sock` (the socket-proxy shape) | PASS |
| A host-userns container sees its image files owned by uid 1000000 | INFO: image layers stay remapped, as Docker documents |
| Proof 4 shape: a host-userns non-root container (folder-app shape) can read its image files | PASS |
| Proof 1 shape: a host-userns root container with default capabilities can write its own image files (brain shape) | PASS |
| A host-userns root container with `cap_drop: ALL` writing its own image files | INFO: `Permission denied` |
| `/etc/subuid`: Debian `useradd` gave a new user `165536:65536` (`SUB_UID_MIN=100000`, `SUB_UID_COUNT=65536`) | INFO |
| That new range does not overlap the remap range | PASS |

Two of these matter for the next step:

- **plunk and formbricks passed as their own USER under the remap alone.** They had no capabilities back. Under the remap, the image's baked owner (1001) and the running user are the same remapped uid, so the writes work. That means **Option A, the install-time re-own layer, may not be needed** for this class. It also means those two images may want the default tier, not the caps tier. The CI lane should confirm this through the brain, not only with plain Docker.
- **A host-userns container that runs as root with `cap_drop: ALL` cannot write its own image files.** Those files are owned by 1000000, and with no `DAC_OVERRIDE` real root is just another uid. The brain keeps its default capabilities, so it is fine. The socket proxy and any other host-userns container that runs as root with `cap_drop: ALL` must be checked in proof 1.

### Profile

These numbers cover the 36 apps that passed in all three modes in the first matrix run (the reruns did not include B). Medians and p90, from `run-app.sh`:

| Measure | A median | A p90 | B median | B p90 | C median | C p90 |
|---|---|---|---|---|---|---|
| Cold pull (s) | 13.5 | 87.5 | 21.7 | 98.4 | 21.4 | 102.8 |
| Time to running (s) | 17.1 | 70.1 | 14.8 | 68.2 | 14.6 | 69.7 |
| Idle memory (MiB) | 116.5 | 2246 | 118 | 2236 | 114 | 2193 |
| Image store growth (MiB) | 0 | 4069 | 464 | 3871 | 606 | 3871 |

- **B vs C, the cost of the remap: none that we can measure.** Pull, time to running and memory are the same within noise. On the median, store growth is higher in C. But app by app, B and C are equal for 31 of the 36, and the differences go both ways (baserow, blinko, kimai and nextcloud are smaller in C, while laminar has a negative B value from pruning). The median moves because of how layers left over from the app before are shared. It is not a cost of the remap.
- **A vs B, the cost of the store switch: not measured cleanly.** In A, `docker image prune -af` did not free the containerd content store, so 23 of the 37 A passes show 0 bytes of growth, and many A pulls were warm. That makes A's pull and store numbers not usable. For the 12 apps that were cold in A too, the median pull was 48.5 s in A, 61.9 s in B and 61.0 s in C. The median time to running was 48.3 s, 45.0 s and 47.2 s. So the classic store may pull a bit slower. This run is not enough to say for sure.
- **Large C vs B differences, app by app.** novu took 10.5 s longer to become ready in C (47.7 to 58.2 s). whoami took 6.8 s longer (2.3 to 9.1 s). blinko was 19 s faster in C (25.3 to 6.4 s). gods-eye-view pulled 13 s slower. None of these repeats in a pattern, and none of them passes in one mode but not the other. No app restarted in the minute after it started, in any mode.

### Pre-existing bugs found

None of these comes from the remap. Each one fails in every mode it ran in.

- **memos** crash-looped under any root brain (`su-exec: setgroups(10001): Operation not permitted`), and the CI cloud lane showed the same on the hosted image. It is fixed in onmoose/store#140 / #141 (`service_user: true`). The merged package passes in A and C here.
- **mealie** has the same bug as memos. It is `state: full`, listed, and it crash-loops under a root brain in A, B and C. Its log shows `chown: changing ownership of '/app/data': Operation not permitted`, then `error: failed switching to "911": operation not permitted`. Its entrypoint runs as root, chowns, then drops to uid 911, and `cap_drop: ALL` refuses both steps. It passes in C with the caps tier. Without the remap, the likely fix is the same as for memos: `service_user: true`, if the image accepts a runtime user. A real box hits this today.
- **quay.io/minio/minio now needs a login.** An anonymous token gets `UNAUTHORIZED` for the pinned tag and digest. cap, langfuse, plane and trigger-dev all pin that image, so none of them can install on any box.
- **forgejo:** Codeberg serves the pinned image index (`sha256:23ccc1…`), but the amd64 manifest it names (`sha256:5b1d65…`) answers `MANIFEST_UNKNOWN`. It failed the same way on all four tries across three hours, so it looks like a registry fault and not a short outage.
- **jotty:** admission refuses it in every mode, because its store compose sets `user: "0:0"`. It is recorded here and not fixed.
- **nocodb:** in every mode, `compose up` fails about 30 s after start with `dependency failed to start: container ... nocodb is unhealthy`. Its logs stop at `completed configure`. The health check (30 s interval, 5 retries, 30 s start period) should not fail that early, so the cause is not clear. It needs a look on the curation box.
- **open-seo:** it does not become healthy inside the brain's 2-minute health wait in any mode. It was healthy when we looked later. So on a 4-vCPU box its first start takes longer than the wait.
- **twenty** is right at the 2-minute `compose up` limit in every mode (121 to 124 s in B and C), and in A it went over twice. It is a timing edge, not a remap issue.
- **windmill** is in the test list but not listed in the store, so the seeded catalog does not have it (404). It is not a bug.

### Answers to the #516 questions, as far as Step 0 goes

- **`/etc/subuid` safety.** Yes: on Debian trixie, `useradd` gives a new user a subuid range (`165536:65536`). It did not overlap the remap range here. But moose users have no use for a range, and a larger user count could climb toward the remap band. The simple fix is `SUB_UID_COUNT 0` and `SUB_GID_COUNT 0` in `/etc/login.defs` on the image, plus writing the `moose-remap` entry before any user is made. This run did not check if shadow's allocator skips ranges already listed in `/etc/subuid`.
- **Image store.** The box runs docker-ce 29.8.1. Mode C turns the containerd snapshotter off itself, and Docker then reports `overlay2`. This run did not try remap with the snapshotter left on, and it did not run a buildkit build under the remap. Both stay open.
- **Opt-out list.** In this run the only containers that needed host userns were folder apps. Multi-service apps with their own databases or brokers (novu with mongodb and redis, firecrawl with rabbitmq and postgres, laminar with clickhouse) ran remapped. So did the managed Postgres, MariaDB, MySQL and Valkey, and the dev Caddy container, which the brain configured for every install. No store app uses host networking, because admission refuses it. GPU apps, Tier-2 services and the real control plane were not in this run.
- **Threat model delta.** Not decided here. See What's next.

## How it maps to the specs

- `APP_ISOLATION.md` # Runtime identity & data ownership and # Not in v1: this run tests the "user namespace remap breaks too many images" reason. On this catalog it broke none.
- `APP_ISOLATION.md` # Runtime identity & data ownership: the "app declares intent, never a UID" rule holds. The tiers are picked from the manifest (folders, GPU, a hardcoded caps list), never from a manifest UID.
- `THREAT_MODEL.md`, container escape row: proof 8 shows that remapped root, even with five capabilities back, has no power over real-root files.
- `docs/dev/catalog-import-gaps.md` # nonroot-data-ownership and # privilege-drop-denied: poznote, mealie, plunk and formbricks are the images this class names.

## Known gaps & deviations

- **Fake host-agent.** `service_user` identities are all the operator's uid, so proof 7's "two apps, two identities" is not shown. host-agent-real, the real control-plane launch, `.local` routing through Avahi and GPU apps are not covered. Those stay with the #516 proofs in the CI lane.
- **Boot only.** An app passes if it reaches `running`, is healthy where it has a health check, and does not restart for one minute. No app was used in a browser. poznote did not create or read back a note, and no app was recreated or rebooted.
- **B was not rerun** for the config-refused apps or for memos. Their B rows show the first run.
- **mealie's C row is the caps-tier pass.** Its default-tier C line (fail) is in `C.jsonl` too.
- **Store growth in A is not usable** (see Profile). Growth also counts layers that the prune after the last app left behind, so single values are noisy.
- **Harness leak.** `run-app.sh` never uninstalls an instance whose job failed after the instance was made (a health-wait failure keeps the instance as `failed`). So mealie and open-seo kept running during later C installs until the reruns removed them. mealie's crash loop added some background load. It does not change any verdict.
- **Proof 9** (the generator never emits `userns_mode: host` with the restored capabilities) was not tested. The spike code refuses that combination at install time, but nothing ran it.
- **Placeholder values** for openmuse, hermes-agent and openclaw prove only that the containers start.
- **The box was left in mode A** with the merged memos package in `~/store/apps/memos` (the old copy is `~/memos-old`). It shuts itself down when idle.

## What's next

1. **Go: build the #516 proofs in the CI lane** (`CI / Cloud image`, `publish=false`) with the daemon.json change in the cloud image and the spike branch in `writeOverride`.
   - Proof 1: host-agent-real launches the socket proxy and the brain with `userns_mode: host`, and Caddy and `moose-ui` remapped. Check the socket proxy in particular. If it runs as root with `cap_drop: ALL`, it cannot write its own image files under the remap.
   - Proofs 2 and 3 on a booted image: poznote creates a note and reads it back, and the note survives a container recreate and a reboot. Rerun the whole catalog.
   - Proof 4 with a `moose-shared` folder grant, which this run did not check.
   - Proofs 6 and 7 with host-agent-real, so two `service_user` apps show base+2100 and base+2101.
   - Proof 9 as a unit test over the generator.
   - Proof 5 (GPU) on a real box with a GPU, or marked "not tested".
2. **Try plunk and formbricks through the brain in the default tier**, remapped with no capabilities back. If they pass, Option A (the install-time re-own layer) can be dropped, and the caps tier is only for images that chown or drop privileges in their entrypoint (poznote, mealie).
3. **Decide the image-store question:** remap with the containerd snapshotter left on, and a buildkit build under the remap.
4. **Store fixes, in onmoose/store:** mealie needs `service_user: true` or the caps tier. cap, langfuse, plane and trigger-dev need a MinIO image that can be pulled without a login. forgejo's pinned digest needs checking. nocodb needs a look.
5. **Spec changes, only after the CI proofs pass:**
   - `APP_ISOLATION.md`: # Not in v1 drops "User namespace remap. Breaks too many images". # Runtime identity & data ownership gains the three tiers and says the brain picks host userns, never a manifest. The caps tier needs a manifest intent field.
   - `THREAT_MODEL.md`: the container escape row says every remapped app shares one range, so a breakout from one remapped app reaches the files of the others, but not real host root. The caps tier gives capabilities inside the namespace. Folder, GPU and control-plane containers keep today's exposure.
   - `DECISIONS.md`: a new entry that turns on daemon-wide remap and records why not sysbox CE (one shared range too, plus two root daemons).
   - `BUILD.md` and `ENVIRONMENT.md`: the `daemon.json` change, the `moose-remap` subuid entry, and `SUB_UID_COUNT 0` in `login.defs`.
   - `NEXT.md` # User-namespace remap for hardcoded-internal-UID app images: close it or narrow it to what is left.
