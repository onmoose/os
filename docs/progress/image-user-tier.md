# image_user: the image tier, #537

- **Status:** done
- **Date:** 2026-09-29
- **Specs touched:** `APP_MANIFEST.md` # B (`image_user`) and # Locked decisions, `APP_ISOLATION.md` # Runtime identity & data ownership, # User-namespace tiers, # What this does not cover, # High-level toggles, `APP_LIFECYCLE.md` # Locked: override file contents and # Locked: install transaction, `THREAT_MODEL.md` B2 and residual 12, `DECISIONS.md` 2026-09-29 (image_user), `NEXT.md` ("After the user-namespace remap", topic 1 closed)

This follows [brain-userns-tiers.md](brain-userns-tiers.md) (#529), whose What's next item 2 asked for a product call on images that must run as their own baked user. The maintainer chose a separate intent over reusing `root_setup`, because it is least privilege. This is that intent, as part of #523. No image turns the remap on yet (#530), so on every box today an `image_user` install is refused, and nothing else changes.

## What was done

- **The field.** `manifest.Manifest.ImageUser` (`image_user`, bool, `omitempty`), so `manifest_version` stays 1. I kept the name the issue proposed: it sits next to `service_user` and `root_setup` and says whose user runs. `own_user`, `keep_image_user` and `baked_user` were weighed (`APP_MANIFEST.md` # B).
- **Admission.** `admission.CheckManifest` refuses `image_user` with `folders`, `gpu: true`, `devices`, `service_user: true` or `root_setup: true`, each with a plain message. A new `admission.CheckManifestCompose` refuses a `user:` on any service of an `image_user` app, since that would replace the image's own user (a numeric `user:` was already refused for every app; this catches a name). Both run at install and in `moose manifest check`.
- **The image tier** (`internal/lifecycle/userns.go`, `store.UsernsTierImage = "image"`). `pickTier` picks it for an `image_user` app on a remapped daemon, after the host check, and refuses it with no remap (`ErrImageUserNeedsRemap`, a plain message, before any state). `applyTier` removes the `user:` pin and adds nothing: `cap_drop: [ALL]` and `no-new-privileges` stay. It now also refuses to write `userns_mode: host` without a `user:` pin, the same kind of guard as the one against `cap_add`.
- **The image user** (`internal/lifecycle/imageuser.go`). After the pull (step 5 of the code, before the bind dirs and the override), `resolveImageUsers` reads each service's `Config.User` (`DockerDriver.ImageUser`, `docker image inspect`). It resolves it the way Docker does at start:
  - `""` is `0:0`, and `uid:gid` with two numbers is used as it is. No file is read.
  - A name, a bare number (for its gid) or a group name is looked up in the image's own `/etc/passwd` and `/etc/group`. `DockerDriver.ImageUserFiles` gets them with `docker create --pull never --network none` and a made-up entrypoint, then `docker cp <id>:/etc/passwd -` and the same for `/etc/group`, then `docker rm -f -v`. The image is never started. Only regular files count (a symlinked `passwd` is treated as missing), and each file is capped at 1 MiB. A file the image does not have is treated as missing, from the daemon's "Could not find the file" answer.
  - A name not in the file, a missing file for a name, or an id at or above 65536 refuses the install with a plain message, and the rollback removes the row and what the pull did.
- **Why names are resolved, not refused.** The issue allowed a name only with a clearly safe way. Both apps that asked for this use one: plunk's image sets `USER plunk` and formbricks' `USER nextjs`, so refusing names would have left them out. It is safe because the result is only an offset inside the remap range, and an id past it is refused, so a name can give nothing a numeric `USER` could not give already. The files are the ones Docker reads at start, so the owner matches the process. No image code runs. It is written down in `APP_ISOLATION.md` # User-namespace tiers and `DECISIONS.md`.
- **Bind dir owners.** `bindDirsByService` is `relativeBindDirs` per service, and `isolation.bindDirOwners` gives each dir its owner. In every other tier that is `bindOwner`, as before. In the image tier a dir goes to `base+uid`:`base+gid` of the image user of the service that binds it, and a dir that two services with different users both bind is refused. Managed-service data stays at `base`.
- **Store.** The CHECK on a fresh database now allows `image`, and `Create` accepts it.
- **Log fields.** `image user resolved` logs `image_user` (the image's own spec) and `uid`/`gid`. `CLAUDE.md` gains `gid` and `image_user`, and the `tier` values gain `image`.
- **A permanent fixture for the lane.** `dev/cloud/test/catalog/imageuser` has two services on two synthetic images that `bootstrap.sh` builds from busybox: `moose-test/imageuser:1` (`USER 1001`) and `moose-test/imageuser-named:1` (`USER app2`, uid 1002 in its passwd), each with a `/baked` dir owned by its user. Each writes its id into its baked dir and its bind dir at start. The access boot now checks that a box with no remap refuses it with the plain message and makes no container. The future remap boot (#531) can run it as it is.

## Tests

- **`TestUsernsNeverHostWithCaps`** now runs every shape with four intents: none, `root_setup`, `image_user` and both, on a daemon with and without the remap. It also checks that `userns_mode: host` never comes without a `user:` pin, that the image tier has no `user:`, no `cap_add` and no `userns_mode`, and which shapes admission and the tier pick refuse.
  - **Two deliberate breaks fail it.** (1) `cap_add` added in the image-tier branch of `applyTier`: "app: rendered tier caps, want image" for the two folderless shapes with `image_user`. (2) `userns_mode: host` added there and its new guard turned off: "app: userns_mode host with NO user: pin". Both were reverted.
- **`TestOverrideUnchangedWithoutRemap`** still passes against the golden files from before the tiers, so every existing manifest gets the same override on a daemon with no remap. An `image_user` app is refused there, so it has no golden in that set.
- **`TestOverrideRemappedTiers`** (new) pins one override per tier on a remapped daemon: default (folderless and `service_user`), caps, image and host. These golden files were written by this change, with their own flag (`-update-golden-remap`), so the old set is never rewritten by accident.
- `TestResolveImageUser` (20 cases: numbers, names, groups, the first passwd line winning, missing names and groups, ids past the range), `TestReadUserFile` (a symlinked passwd, a file over the cap, an empty stream), `TestCapReader`, `TestBindDirOwners`.
- Install tests through the fakes: refused with no remap before any state; refused with a `user:` in the compose before any state; the image tier for `1001`, `1001:1001`, `plunk`, `""` and `0`, with both bind dirs at the right owner and the override remapped with no `user:` and no `cap_add`; an unknown name, a missing passwd and a failed probe each roll back with no row and no `compose up`; two services with different users get their own owners, and a shared dir is refused.
- `checkTierKept` gains four cases (image kept, image dropped, image to `root_setup`, default to image) and the no-remap refusal.
- Admission, `moose manifest check`, the manifest parse and the store round trip each gain cases.
- **`TestLiveImageUserProbe`** (build tag `dockerlive`) ran the real CLI against a real `busybox:1.37.0` on this machine's Docker: `ImageUser` gave `""`, `ImageUserFiles` gave the real passwd and group, `nobody:www-data` resolved to `65534:33`, and no probe container was left.

## Verification

- `make check`: green, the full suite with the PAM package.
- `CI / Cloud image` with `publish=false` on this branch (the six usual boots): run 36609935235, all six pass (`unseeded seeded bios access update ssh`). The access boot now includes the refusal: "image_user app refused on a box with no remap, with the plain message, and no container made".

### The remapped half, on the booted image

A throwaway branch, `test/537-remap-proof` off this branch, turned the remap on in the hosted image the way #529 did (`daemon.json`, the `moose-remap` account and range, `SUB_UID_COUNT 0` and `SUB_GID_COUNT 0`), with a `remap` boot on a 40 GB disk and 8 GB of memory. It added two test-only fixtures: `imageuser-ghost` (an image whose `USER ghost` is not in its passwd) and `plunk-iu` (plunk with `image_user: true`, on the managed Postgres and Valkey). It was never merged and is deleted from origin. Run: https://github.com/onmoose/os/actions/runs/36609452121 (`boots=remap`).

It passed, with part 1 all green. Quotes are from the run's serial log. `base` is 1000000.

| Check | Result |
|---|---|
| The daemon is remapped, and the proxy and the brain are `UsernsMode=host` | "security options ... name=userns ...; proxy and brain UsernsMode=host" |
| The override pins no `user:` and adds no `cap_add` or `userns_mode` | "override has no user:, no cap_add and no userns_mode" |
| The brain resolved both users, the number and the name | "image user resolved ... service=imageuser image=moose-test/imageuser:1 image_user=1001 uid=1001 gid=1001" and "service=named image=moose-test/imageuser-named:1 image_user=app2 uid=1002 gid=1002" |
| `USER 1001`: remapped, no capability, process and bind dir at `base+1001`, both writes work | "imageuser user='1001' UsernsMode='' CapAdd=null CapDrop=["ALL"] SecurityOpt=["no-new-privileges:true"] host uid:gid 1001001:1001001; bind dir 1001001:1001001 file 1001001:1001001 says 'uid=1001(app1) ...'; baked /baked/id.txt says 'uid=1001(app1) ...'" |
| `USER app2` (a name): the same at `base+1002` | "named user='app2' UsernsMode='' CapAdd=null CapDrop=["ALL"] ... host uid:gid 1001002:1001002; bind dir 1001002:1001002 file 1001002:1001002 says 'uid=1002(app2) ...'; baked /baked/id.txt says 'uid=1002(app2) ...'" |
| A name the image does not list is refused plainly, and nothing is left | "the image of service \"ghost\" runs as the user \"ghost\", and that user is not in the image's own user list, so moose cannot tell who should own its data. This app cannot be installed"; no app container and no probe container left |

**plunk with `image_user: true`** (recorded only). It ran as its own baked user, remapped, with no capability: "user='plunk' capadd=null host uid 1001001" (`base+1001`), and the override had no `user:`. The baked write that stopped it in the default tier ("Can't write to /app/node_modules/@prisma/engines") did not happen. It then stopped where it stopped in the caps tier in #529: Prisma tried to download its schema engine from `binaries.prisma.sh`, which the air-gapped lane cannot reach ("getaddrinfo EAI_AGAIN"), so its migrations failed and the install ended "did not become healthy". So plunk gets as far with no capability as it did with five. Running it for real needs a lane with a network and a catalog package with `internet: true`.

**Without the remap** the same `imageuser` install is refused with the plain message, on the access boot of the six-boot run on this branch (above).

## Known gaps & deviations

- **The update path is not built**, so neither `checkTierKept` nor a check that the new image runs as the same user has a caller. `checkTierKept` covers a tier change. In the image tier the owner also follows the image's user, so the update path must also resolve the new images' users and refuse a change. The spec and the code comment say so.
- **A database created by a dev build between #536 and this change** has the old CHECK (`'default','caps','host'`), and SQLite cannot change a CHECK without rebuilding the table. On such a database an `image_user` install fails at the row write, before any other state. #536 never reached `main`, and such a box has no remap anyway (no image turns it on), so it would refuse the install earlier, in `pickTier`. A remapped box is always a new box (`DECISIONS.md` 2026-09-29, "New boxes only"), so its database is created by this code. No migration is written for it.
- **A missing file is found from the daemon's text** ("Could not find the file"). If Docker changes that text, an image without `/etc/passwd` gets a wrapped Docker error instead, which still refuses the install.
- **The probe's `docker rm` does not use the install's context**, so that it still runs after a cancel. A wedged daemon would hold that one call.
- **An image picks which uid in the range it runs as.** After a container escape it can reach the files of remapped containers that run as that uid, such as a `service_user` app's data or a managed service's. Most remapped containers already share `base`, so this is a small widening, after an escape only. `THREAT_MODEL.md` B2 and residual 12 say so.
- **A relative bind over `/etc/passwd` in the compose** would make Docker read a different file at start than the one the brain read from the image. It is only a wrong owner for the app's own dirs, inside the range, never a host uid. The compose is reviewed, and no known app does it.
- **The tier is per app.** An app that bundles a service needing root work at start (formbricks' own Postgres) cannot use `image_user`; it needs `root_setup` for the whole app, or a managed service instead.
- **A probe container is left behind** only if the brain stops between `docker create` and `docker rm`. It carries the `moose.image_user_probe` label; nothing removes it automatically.
- **The install progress screen has no new step.** The image user is read inside the step that already runs there.

## Review

- **Fresh Sonnet agent:** no Block findings. Two Notes, both kept as they are. (1) The probe's `docker rm` runs without the request context, so a wedged daemon would hold that one call. That is on purpose: the cleanup must still run when the install's context is cancelled. (2) A file the image does not have is found from the daemon's "Could not find the file" text. If that text ever changes, such an image gets a wrapped Docker error instead of "missing", which still refuses the install; it cannot give a wrong owner. Both are in Known gaps.
- **Greptile P1, "Existing databases reject image tier":** dismissed, and kept in Known gaps. It is true that `CREATE TABLE IF NOT EXISTS` leaves the old CHECK on a database created between #536 and this change. But the image tier only exists on a remapped box, and the remap is for new boxes only (`DECISIONS.md` 2026-09-29, "New boxes only": an existing box is re-provisioned, not migrated). A remapped box therefore always starts with a database this code created. #536 never reached `main`, and a box with such a database has no remap, so `pickTier` refuses the install before the row write. A table rebuild to change a CHECK is a bigger risk than the case it would cover.
- **Greptile P2, "Probe volumes remain after cleanup":** confirmed and fixed. An image with a `VOLUME` gets an anonymous volume for the probe container too, so the probe is now removed with `docker rm -f -v`.
- **Greptile P2, "Unrelated files block user resolution":** confirmed and fixed. The probe copied all of `/etc` under one 64 MiB cap, so a large unrelated file there could refuse an install. It now copies `/etc/passwd` and `/etc/group` one by one (`copyUserFile`), each capped at 1 MiB. `TestLiveImageUserProbe` passed again against real Docker, and the remap proof was run again on the new code (below).

## What's next

1. #530 and #531: turn the remap on in both images, and give `CI / Cloud image` a remap boot. The `imageuser` fixture is ready for it.
2. Real catalog packages: plunk with `image_user: true` and `internet: true` (its Prisma engines), formbricks with `root_setup` or a managed Postgres, and SpiceDB.
3. The app update path must call `checkTierKept` and compare the image users.
