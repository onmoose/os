# userns-remap on in both images, #530

- **Status:** done, hosted lane proved; appliance lane not run yet (see Known gaps)
- **Date:** 2026-09-29
- **Specs touched:** `BUILD.md` # User-namespace remap, `ENVIRONMENT.md` # How the profile is realized, `CONTROL_PLANE.md` # Locked: control-plane container hardening, `APP_ISOLATION.md` # User-namespace tiers, `APP_MANIFEST.md` # B (`root_setup`, `image_user` status), `TESTING.md` # Medium lane and the hosted lane paragraph, `docs/dev/hosted-boot-proof.md`, `docs/architecture.md`

Slice 5 of #523, and the last slice that changes a box. It follows [image-user-tier.md](image-user-tier.md) (#537) and [brain-userns-tiers.md](brain-userns-tiers.md) (#529), whose brain and host-agent halves were inert until an image turned the remap on. This change turns it on, in the hosted image and in the appliance test image. It writes properly what the throwaway branch in [userns-remap-ci-proofs.md](userns-remap-ci-proofs.md) proved on the hosted image.

## What was done

### Both images

- **`daemon.json`** gains `"userns-remap": "moose-remap"` next to the journald log driver. Hosted: `dev/cloud/mkosi.extra/etc/docker/daemon.json`. Appliance: the heredoc in `dev/test-qemu/bootstrap.sh`, still byte for byte the same as the hosted file.
- **`mkosi.postinst.chroot`** (both `dev/cloud/` and `dev/test-qemu/`) makes the `moose-remap` system account (`useradd -r -U`, no home, `nologin`), writes `moose-remap:1000000:65536` to `/etc/subuid` and `/etc/subgid`, and sets `SUB_UID_COUNT 0` and `SUB_GID_COUNT 0` in `/etc/login.defs`. It runs before any user exists, and it is idempotent. The block is the same in both files, as the `moose-app` block already was.
- **No package was added.** The lean check passed: "lean check passed, manifest matches expected-packages.txt exactly (162 packages)".
- The appliance lane's `CANARY_VERSION` goes from `v30` to `v31`, so a cached appliance image is rebuilt with the remap.

### The checks on the booted box

- **Every cloud boot** now checks the remap in `cloud-assertions.sh` step 5d, before its scenario. It prints the two `docker info` lines, then fails unless the security options list `name=userns`, the storage driver is `overlay2`, the root dir is `/var/lib/docker/1000000.1000000`, both range files carry the `moose-remap` line, `login.defs` sets both counts to 0, the proxy and the brain run with `UsernsMode=host` as host uid 0, and Caddy and `moose-ui` run remapped as host uid 1000000. The host uid is read from `/proc/<pid>/status`, since the lean image has no procps.
- **The appliance medium lane** runs the same check in `medium-assertions.sh` step 2b, on both of its boots.
- **The access boot's `image_user` step changed.** It used to check that a box with no remap refuses the `imageuser` fixture. No lane box runs without the remap now, so that step would fail. It now installs the fixture and checks that each service runs remapped, with no `cap_add`, and writes its bind dir as host uid `1000000` plus its image's user. The refusal without the remap is still covered by the brain's tests (`internal/lifecycle/userns_test.go`, `imageuser_test.go`). The comments in the fixture and in `dev/cloud/test/bootstrap.sh` say this now.

### CI run

`CI / Cloud image`, `publish=false`, on commit 8a5b4f0: https://github.com/onmoose/os/actions/runs/36636692363. **All six boots pass** (`unseeded seeded bios access update ssh`) on the remapped image. Each boot printed:

```
cloud-assertions: docker info: security options: ["name=apparmor,profile=default","name=seccomp,profile=builtin","name=userns","name=cgroupns"]
cloud-assertions: docker info: storage driver: overlay2, root dir: /var/lib/docker/1000000.1000000
cloud-assertions: userns-remap on (moose-remap:1000000:65536, SUB_UID_COUNT 0); proxy + brain in the host userns (host uid 0), caddy + moose-ui remapped (host uid 1000000)
```

The access boot also printed "image_user imageuser runs remapped as its image's user (Config.User='1001', cap_add=null) and wrote data/id.txt as host uid 1001001" and the same for `named` at host uid 1001002. The update boot's happy path and revert both passed on the remapped store, and so did the household and personal folder installs (host tier).

### Docs

`BUILD.md` # User-namespace remap now says the remap is built, and gives the #486 answer in place of "must not ship the remap before that is settled": nothing replaces `daemon.json` on a box today, so new boxes get the remap and boxes built before this keep it off, and both are safe. The rule for the OS update (keep each box's setting, or move a box over on purpose) lives on #486. `ENVIRONMENT.md`, `CONTROL_PLANE.md`, `APP_ISOLATION.md`, `APP_MANIFEST.md` and `architecture.md` no longer say that no image turns the remap on. `TESTING.md` and `hosted-boot-proof.md` describe the new per-boot check and the changed access step.

## How it maps to the specs

- `BUILD.md` # User-namespace remap: the four build steps, in both images, as written.
- `ENVIRONMENT.md` # How the profile is realized: the same daemon settings on both profiles, image configuration only, nothing in the seed.
- `APP_ISOLATION.md` # User-namespace tiers: on a new box the brain now sees the remap and picks the tiers. The access boot shows the image tier and the host tier on the booted image.
- `DECISIONS.md` 2026-09-29: no decision flips. The #486 answer is a clarification of "new boxes only", so it is written in `BUILD.md` and on #486, not as a new entry.

## Known gaps & deviations

- **The appliance medium lane did not run.** `make test-medium-qemu` built the tools tree and the image root, then failed while writing the encrypted disk image: "Failed to copy bytes to partition: No space left on device". The machine's root filesystem has 13 GB free, and the LUKS image needs an 8.5 GB raw file plus the same again for its staging partition. Nothing in the lane or the change is at fault, but the result the issue asks for is still missing. It needs space freed on the build machine (Docker's build cache holds about 30 GB) and one more run.
- **No remap-off box in the lane.** The six boots all run the remap now, so the brain's no-remap path (refusals for `root_setup` and `image_user`, byte-for-byte overrides) is proved only by unit and golden tests, not on a booted box.
- **Existing boxes keep the remap off.** That is by design, per the maintainer: no migration is built, and a box built before this change keeps working as it did.
- **Not in this change:** the dedicated remap boots (`remap`, `remap-reboot`) with poznote, memos, managed Postgres and a reboot. That is #531.

## What's next

1. **Run the appliance medium lane once** on this image (`make test-medium-qemu`) after freeing disk space, and record the result on #530.
2. **#531:** add the remap boots to `CI / Cloud image`. The per-boot remap check (step 5d) and `remap_base` in `cloud-assertions.sh` are ready to use. The access boot already covers the `imageuser` fixture on the remapped box, so #531 does not need to repeat it.
3. **#486:** the image-based OS update must keep each box's remap setting, or move a box over on purpose.
4. **`capabilities.yml`:** the `userns-remap` capability now exists on new boxes. Adding it is the signal that re-screens blocked apps such as plunk, so it should land with the first catalog app that is proven on it.
