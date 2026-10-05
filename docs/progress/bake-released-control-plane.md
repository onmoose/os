# OS releases bake the last released control plane

- **Status:** done
- **Date:** 2026-10-05
- **Specs touched:** `docs/specs/BUILD.md`, `docs/specs/UPDATES.md`, `docs/architecture.md`, `docs/dev/contributing.md`, `docs/dev/hosted-boot-proof.md`

Closes #566, a slice of #486. It closes the "same number, different bytes in the disk image" gap of [control-plane-version-line.md](control-plane-version-line.md) (#559). Since #559 the OS and the control plane are two release lines, but the disk image still baked the brain and UI built from its own commit. So an OS-only release made while `main` held unreleased brain or UI changes baked them under the last released control-plane number. Now an OS-only release bakes the last released control plane, pulled from ghcr by digest, and its boot proof runs against those images.

## What was done

### Two sources for the brain and UI

`dev/cloud/stage-control-plane.sh` takes the brain and UI from `MOOSE_CONTROL_PLANE_SOURCE`:

- **`released`**: `make control-plane-released`, which runs `dev/control-plane/pull-released.sh`. It resolves `ghcr.io/onmoose/brain` and `ghcr.io/onmoose/ui` at image tag `v<CONTROL_PLANE_VERSION>` to their digests once (`dev/release/ghcr-resolve.sh`, an anonymous registry read), pulls each by digest, tags it `moose-brain:dev` and `moose-ui:dev` (the names the brain drop-in and the control-plane compose use), and saves it. It then checks that each tarball holds the image config ghcr names, and fails otherwise.
- **`local`** (the default): `make control-plane-images`, a build of the commit, as before.

Both make targets share a new `make control-plane-third-party` for the Caddy and socket-proxy tarballs. `ghcr-resolve.sh` hashes the bytes the registry served and checks them against the registry's digest header. For an index it takes the `linux/amd64` entry (the moose images are single manifests today, but a Docker that pushes an index would not break it). A missing tag, any other HTTP answer, a manifest with no config digest and an index with no amd64 image all fail the build.

### The rule (`CI / Cloud image`)

The workflow sets `MOOSE_CONTROL_PLANE_SOURCE` from the publish switches, in this order:

1. **The control plane publishes** (a control-plane release, or a merge that bumps both files): `local`. Its images are not on ghcr yet, and the publish job pushes the very images the boots ran. A merge that bumps both files therefore bakes the control plane it releases beside the OS.
2. **Only the OS publishes** (an OS-only release, or a manual `v*` tag push): `released`.
3. **Nothing publishes** (a PR, a lock bump, a dispatch): `local`, so a PR boots the brain of its own commit. A dispatch can ask for `released` with the new `control_plane` input, to see before an OS release that the OS boots with the control plane it will ship.

A dispatch that asks for `released` while it publishes the control plane fails at once instead of being quietly overridden. The publish job's tarball hand-over checks the bundle record says `local`, so a pulled older release can never be pushed under a new number.

### The record, and the boot check

- **`dev/control-plane/bundle-record.sh`** writes `control-plane.env` beside the tarballs: the source, the control-plane version, the two pinned refs (for `released`), and the image ID each tarball loads as. The ID is the image config digest, read from the tarball's own `manifest.json` (both the Docker 25+ and the older layout), so it does not depend on the build host's image store.
- **Reuse.** The staging reuses a bundle only when its record matches what is asked for (the same source, and for `released` the same version). A bundle with no record is rebuilt once.
- **In the image.** The record is baked into the slot as `/usr/lib/moose/control-plane.env`.
- **Same bytes in both images.** The build job keeps the shipped image's record and fails when the boot-proof image records another pair. The job summary shows the record.
- **Every boot** (`cloud-assertions.sh` step 5a) checks that `moose-brain:dev` and `moose-ui:dev` loaded as the recorded IDs, and that the running `moose-brain` and `moose-ui` containers run them. It prints `control plane baked: <source> <version>`, and for `released` the two refs. On the box Docker uses its classic store (the remap turns the containerd store off), where the image ID is the config digest.

## Numbers

- **Build time.** The released pull took 7 s in CI (run 37345392679: resolve, pull and save both images), against about 68 s to build the brain and UI (`ci-cloud-image-speedup.md`). So an OS-only release builds about 1 min faster. Other runs are unchanged.
- **Disk.** The record adds under 1 KB to the slot. The brain and UI tarballs are the released images instead of a local build of the commit, so their size follows the release.
- **CI.** No new job and no new boot. One new dispatch input.

## How it was verified

All in CI with every publish input false. Nothing was built or booted locally. `ghcr-resolve.sh` and `pull-released.sh` were also run by hand against the real ghcr (read only: a pull of the public `v0.15.0` images and a multi-arch public image for the index path).

| Run | What | Result |
|---|---|---|
| 37345392679 | the full list, `-f control_plane=released` | The build baked control plane 0.15.0 from ghcr (`brain@sha256:cefc9592...`, `ui@sha256:2bbf0ee2...`), both images recorded the same pair, and every boot printed `control plane baked: released 0.15.0`. **10 of 14 jobs green**: `unseeded seeded access update ssh remap` under both firmwares. The 4 `os-update` and `os-revert` jobs are red, as expected: they read the OS update state and the OS notification through the brain, and the 0.15.0 brain is older than #563, so its `/api/v1/system/update-target` has no `os` object. host-agent itself did the right thing (it refused the wrong digest in the log). See # Known gaps. |
| **37347252464** | **the full list, default (`local`), head `2e16ca4` (later commits change only docs)** | **green, all 14 jobs: 7 boot groups under UEFI and under legacy BIOS, 14.0 min wall. Every boot printed `control plane baked: local 0.15.0`, the `os-update` boot on slot A and again on slot B** |

- **Tests.** `dev/release/controlplane_test.go`: `ghcr-resolve.sh` with a stub registry (a single manifest, an index with the amd64 entry, an index with none, a digest header that does not match the bytes, a missing tag, a manifest with no config), and `bundle-record.sh` (a released and a local record, both tarball layouts, and the refusals). `make check` green; `actionlint` clean.

## How it maps to the specs

- `BUILD.md` # Versioning: the "same commit" bullet is replaced by the as-built rule and the record. The design bullet now says the disk images bake a released control plane. # 1b # As built (copy once) and # 5c and # 6 follow.
- `UPDATES.md` # 3: a new box starts on a released pair.
- `docs/dev/contributing.md` # Release model: what an OS-only release bakes, and how to check ahead.

## Known gaps & deviations

- **The next OS release must come with, or after, a control-plane release.** The last released control plane is 0.15.0 (`control-plane-v0.15.0`, the `v0.15.0` commit), which is older than #563. Run 37345392679 shows an OS-only release today would fail the `os-update` and `os-revert` boots, because those boots check the OS update through the brain. The fix is the rule itself: bump `CONTROL_PLANE_VERSION` in the same release PR, or release the control plane first. After one control-plane release with #563's brain half, OS-only releases pass again, until a later boot check needs unreleased brain code. The boots were kept strict on purpose: an OS release whose baked brain cannot show the OS update state is worth stopping. Reading host-agent directly in those boots would let such a release through.
- **The full released path is not green yet, so "Done when" is met only in part.** The disk image bakes the digests of `control-plane-v0.15.0` and the boot proof ran against them, but the two OS update boots cannot pass until a control-plane release carries #563. A real OS-only release first runs after that.
- **A box still updates its control plane once after first boot.** The baked reference is `moose-brain:dev`, and the update loop compares it as a string with the target's digest refs. So even a box whose baked pair equals its target re-pulls it by digest (the layers are already there). That was true before this change, and it is not in this slice.
- **The record is checked against the image ID, not the manifest digest.** A box that loads a tarball keeps no manifest digest. The config digest pins the same bytes; the manifest digest is in the record as the ref.
- **`release.yml` does not check before it tags** that `v<CONTROL_PLANE_VERSION>` is on ghcr. An OS-only release with a missing image tag fails early in the build, before mkosi runs, and re-running `release.yml` resumes once the control-plane release is finished.
- **The PR trigger does not watch `dev/control-plane/`.** A change only to `pull-released.sh` or `bundle-record.sh` does not start `CI / Cloud image` on its own; the release run and a dispatch still exercise them.

## What's next

1. Release the control plane with or before the next OS release (above), then an OS-only release can bake it and pass every boot.
2. The rest of #486, as in [host-agent-os-update.md](host-agent-os-update.md): the `os` part of the private update-target answer, the proof on a provisioned box, and #564.
