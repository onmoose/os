# The brain's three user-namespace tiers, #529

- **Status:** done
- **Date:** 2026-09-29
- **Specs touched:** `APP_ISOLATION.md` # User-namespace tiers, `APP_LIFECYCLE.md` # Locked: install transaction, `APP_MANIFEST.md` # B (`root_setup` status), `BRAIN_HOST_PROTOCOL.md` # User info endpoints

This is slice 4 of #523, after [root-setup-manifest-field.md](root-setup-manifest-field.md) (slice 3). It uses what the first three slices gave the brain: the proxy and the brain already run with `--userns=host` ([brainlaunch-userns-host.md](brainlaunch-userns-host.md)), host-agent reports `remap_base` ([remap-base-well-known.md](remap-base-well-known.md)), and the manifest carries `root_setup`. The design is [userns-remap-spec.md](userns-remap-spec.md), proved in [userns-remap-ci-proofs.md](userns-remap-ci-proofs.md). No image turns the remap on yet (#530), so on every box today nothing changes, except that a `root_setup` install is refused.

## What was done

- **`internal/lifecycle/userns.go`** (new).
  - `hostIdentity` reads `remap_base` from host-agent and asks Docker (`DockerDriver.UsernsRemap`, `docker info --format '{{json .SecurityOptions}}'`, looking for `name=userns`). The base only comes from host-agent, never from an env var. When the two disagree, or either read fails, every install is refused (`ErrRemapMismatch`, a plain message; the detail goes to the log).
  - `pickTier` picks the tier from the manifest: with no remap, always `host`, and a `root_setup` app is refused (`ErrRootSetupNeedsRemap`). With the remap: `host` for `folders`, `gpu: true` or `devices`; `caps` for `root_setup`; `default` for the rest. The host check comes first.
  - `isolation.applyTier` edits each service's override entry: `userns_mode: host` for the host tier, `cap_add` of `CHOWN`, `SETUID`, `SETGID`, `DAC_OVERRIDE`, `FOWNER` and no `user:` for the caps tier. It writes nothing with no remap, refuses the caps tier with no remap, and refuses an entry that would carry both `userns_mode: host` and `cap_add`.
  - `isolation.bindOwner` gives the host owner of a bind dir: the real ids with no remap or in the host tier, `base:base` in the caps tier, `base+uid:base+gid` in the default tier (and an error for an id outside the 65536-id range).
  - `checkTierKept` is the "the tier is fixed" refusal (`ErrTierChange`). There is no app update path yet, so nothing calls it but its test. The update path must call it.
- **`internal/lifecycle/lifecycle.go`.** A new step 2c in `install`, after the GPU gate and before any state: read the remap, pick the tier, refuse. The tier goes on the row. The well-known identity read moved there, so every install now calls it (it used to be folder apps only). Bind dirs are chowned to `bindOwner`, through a new `Manager.chown` (`os.Lchown` in production; tests record the owners). `writeOverride` calls `applyTier` for every service. The `app installed` log line gains `tier`.
- **`internal/lifecycle/services.go`.** On a remapped daemon a new managed service's data dir and what is in it go to `base:base` (`writeServiceDir`). An unprivileged dev brain logs and goes on, like the bind dirs.
- **`internal/store`.** `instances.userns_tier` (`TEXT NOT NULL DEFAULT 'host'`, with a CHECK on a fresh database), `Instance.UsernsTier`, and the `UsernsTier*` constants. An empty tier at `Create` means `host`; an unknown one is refused.
- **Docs:** the specs above, `docs/architecture.md`, `docs/dev/running-locally.md` (a dev Docker with `userns-remap` now makes every install fail, since the fake host-agent reports no range), and `CLAUDE.md` gains the `tier` and `remap_base` log fields.

### The migration: what "today" means for an existing instance

Every instance on a running box was installed on a daemon with no remap. All its containers are in the host user namespace, and its data has real host owners. That is what the host tier means, so the column's default is `host`, and the brain also stores `host` for every new install on a daemon with no remap. The value is the truth about who owns the data, not only a label.

It follows that if the remap were turned on under a box that already has apps, those apps keep the host tier. An update that would move a folderless one into the remap is refused by `checkTierKept`, because its data is owned by real host ids. But nothing rewrites an existing override, so such an app would start remapped with host-owned data and fail. This slice does not repair that. It should not happen: a box's remap is set when its image is built, and #523 requires that an OS update never turns it on or off. `APP_ISOLATION.md` # User-namespace tiers now says so.

## Tests

- **`TestOverrideUnchangedWithoutRemap`** installs ten shapes through the real transaction (folderless, jobs, `service_user`, household and personal folders, GPU, devices, managed Postgres, and two Door-2 pastes) and compares each override with a golden file. **The golden files were written by the code on `dev` before any change here** (the first commit of the branch), so this shows the override is byte for byte the same on a daemon with no remap. Only the per-run ids and temp paths are replaced before the compare.
- **`TestUsernsNeverHostWithCaps`** is the spike's proof 9 as a real test. It runs the real `pickTier` and `writeOverride` over six shapes (folderless, `service_user`, folders, GPU, devices, folders and GPU), each with and without `root_setup`, with and without the remap. For every service it checks: never `userns_mode: host` with `cap_add`, `cap_drop: [ALL]` always, the expected tier, no `user:` in the caps tier, and which shapes admission and the tier pick refuse. It renders the shapes admission refuses too, so the generator is shown to hold on its own.
  - **A deliberate break fails it.** I added `cap_add` in the host-tier branch of `applyTier` and turned off its guard. Four subtests failed with "app: userns_mode host WITH cap_add [CHOWN SETUID SETGID DAC_OVERRIDE FOWNER]" (folders and GPU, with and without `root_setup`). The change was then reverted.
- Install tests through the fakes: `root_setup` refused with no remap, before any row or Docker work; the two disagreements and the two failed reads refuse every install; the default tier (data at `base+uid`), the caps tier (five caps, no `user:`, data at `base`), the host tier for a folder app; managed-service data at `base` with the remap and untouched without; `checkTierKept` for six cases, with and without the remap.
- The store: the round trip, the refusal of an unknown tier, and a migration from an `instances` table without the column.
- `TestRootSetupDoesNotChangeTheOverrideYet` (slice 3) is removed: it pinned the "not acted on yet" state this slice ends. The golden test covers the no-remap half of it, and the new install test the refusal.
- Two folderless tests no longer assert that `WellKnownIdentity` is not called, since every install calls it now. They still assert that no owner home is resolved.

## Verification

- `make check`: green, the full suite with the PAM package.
- `CI / Cloud image` with `publish=false` on this branch (the six usual boots): run 36595832496, all six pass (`unseeded seeded bios access update ssh`).

### The remapped half, on the booted image

A throwaway branch, `test/529-remap-proof` off this branch, turned the remap on in the hosted image the way the spike did (`daemon.json`, the `moose-remap` account, `moose-remap:1000000:65536` in `/etc/subuid` and `/etc/subgid`, `SUB_UID_COUNT 0` and `SUB_GID_COUNT 0` in `login.defs`). It added a `remap` boot with a 40 GB disk and 8 GB of memory, and test-only fixtures: `datadrop` (busybox writing to its `./data`), `memos` (`service_user`), `poznote` (`root_setup: true`), and plunk and formbricks, each once plain and once with `root_setup`. It was never merged and is deleted from origin. It ran twice with `publish=false`: run 36596370743, then run 36598956852 with the formbricks env filled in and failed installs found by label. Part 1 passed in both. Quotes are from 36598956852.

| Check | Result |
|---|---|
| The daemon is remapped, and the proxy and the brain are `UsernsMode=host` | "security options ... name=userns"; "proxy and brain UsernsMode=host" |
| A folderless app, default tier, root inside | "datadrop user=0:0 UsernsMode='' CapAdd=null host uid 1000000; data dir 1000000:1000000; file 1000000:1000000 says 'uid=0(root) ...'" |
| A `service_user` app, default tier: data at `base+uid` | "memos user=2100:2100 host uid 1002100; data dir 1002100:1002100; db 1002100:1002100" |
| A `root_setup` app, caps tier: five caps, no `user:`, remapped | "poznote CapAdd=[CAP_CHOWN, CAP_DAC_OVERRIDE, CAP_FOWNER, CAP_SETGID, CAP_SETUID] CapDrop=[ALL] User='' UsernsMode='' SecurityOpt=[no-new-privileges:true] host uid 1000000 restarts=0; data dir 1000082:1000082". The brain gave the dir to `base:base`, and poznote's entrypoint gave it to its own user |
| A folder app, host tier | "filedrop UsernsMode=host CapAdd=null user=2000:2000 host uid 2000; /srv/moose/shared/Documents/filedrop.txt is 2000:2001" |
| Managed services, default tier | "managed postgres-16 data owner 1000999:1000000 host uid 1000999", valkey-8 the same owner. The brain gave both dirs to `base:base` and the images took them from there |

### plunk and formbricks

Both are public images, used only on the throwaway branch, never added as catalog apps.

- **plunk, default tier** (`user: "0:0"` pinned): the same failure as today, on a remapped daemon: "Error: Can't write to /app/node_modules/@prisma/engines". Root inside is `base+0`, and the baked files are owned by `base+1001`; with no `DAC_OVERRIDE` root cannot write them.
- **plunk, caps tier** (`root_setup: true`): it ran as its own baked user ("user='plunk' ... host uid 1001001", `base+1001`) and got past that write. It then stopped on something else: Prisma tried to download its engine from `binaries.prisma.sh`, which the air-gapped lane cannot reach ("getaddrinfo EAI_AGAIN"). So the tier question is answered for plunk: the caps tier works, because it drops the `user:` pin. Whether plunk then runs needs a lane with a network, and a catalog package with `internet: true`.
- **formbricks, default tier:** the bundled `pgvector` Postgres could not start ("dependency postgres failed to start"): its root entrypoint must `chown` its data dir, which needs `CHOWN`. The main image was never started.
- **formbricks, caps tier:** the bundled Postgres started and owns its data at `base+999` (host uid 1000999). The main image ran as its own user (`nextjs`) but stopped at env checks before it touched its migrations dir: formbricks v6 needs SpiceDB (`AUTHZED_ENABLED=true`, `AUTHZED_ENDPOINT`, `AUTHZED_TOKEN` and more). So the baked-dir write that blocked it before was not reached. The run shows that the caps tier is the one its bundled Postgres needs, and that the main image then runs as its own user. It does not show the app working.

**What this means, for the maintainer.** Neither app needs a fourth tier. Both get further in the caps tier, but for a reason that is not the one `root_setup` names. plunk has no root work at start. It needs the image's own `user:` left in place, and the caps tier happens to do that. formbricks needs the caps tier for a real root entrypoint (its bundled Postgres), and for the same "leave my user" reason for the main image. Two things follow, and they are product calls, not code in this slice:

1. **Is `root_setup: true` the right field for "run as the image's own user"?** It works, but it gives five capabilities to an app that does not use them (a non-root process with `no-new-privileges` cannot raise them, so the cost is small), and it says "root work" where the truth is "my baked user". A separate intent, or a wider meaning for `root_setup`, would say it plainly. Both stay inside the three tiers.
2. **Bind dirs in the caps tier go to `base`, the container's root.** An image that runs as its own non-root user and also has a data bind cannot write that dir unless its entrypoint fixes the owner as root first. plunk has no data bind, so it does not hit this. A catalog package for such an image would need to know.

`NEXT.md` keeps the open item with these findings.

## Known gaps & deviations

- **No app update path, so the tier refusal has no caller yet.** `checkTierKept` is tested and documented as the check the update path must run.
- **Turning the remap on under existing apps is not repaired** (see the migration section). It relies on a box's remap being fixed for its life.
- **Every install now reads the well-known identity and `docker info`.** A host-agent or Docker that cannot answer now fails a folderless install that would have worked before. That is the spec's rule: without both answers the brain cannot know the owner of a bind dir.
- **Bind dirs are now chowned with `Lchown`, not `Chown`.** The brain creates them itself right before, so this only matters if a symlink were there, and then not following it is the safer choice.
- **The install progress screen has no step for the new check.** It runs inside the first phase ("Preparing"), which is where its refusals belong.
- **A managed service created before the remap keeps its old owner.** Only a new service dir is chowned. Same reason as the migration: a box's remap does not change.

## What's next

1. #530: turn the remap on in both images (after #486), and #531: a remap boot in `CI / Cloud image`, so the lane keeps proving this.
2. A product call on plunk and formbricks (above): keep using `root_setup` for "run as my own baked user", or give that its own intent. Then real catalog packages: plunk with `internet: true` and its Prisma engines, formbricks with SpiceDB.
