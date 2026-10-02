# CI / Cloud image: build once, boot in parallel

- **Status:** done
- **Date:** 2026-10-02
- **Specs touched:** `docs/specs/BUILD.md`, `docs/dev/hosted-boot-proof.md`

A slice of #486, after [hosted-ab-layout.md](hosted-ab-layout.md). Since #561 every boot runs under UEFI and again under legacy BIOS, and the `CI / Cloud image` workflow ran them one after another in one job: a full run took about 27 min. This slice cuts that to 10.5 to 11.5 min (about 13 on a branch's first run, while the layer cache fills) without dropping any boot, any publish guard or any rule about which boots run.

## What was done

### Three jobs instead of one (`.github/workflows/ci-cloud-image.yml`)

- **`build`** runs every assert as before (another ref, a pull request, its own boot list, the tag against `VERSION`), then picks the boots, then builds the production image and the boot-proof image, and uploads the boot-proof image as one qcow2 artifact (`cloud-image-boot-qcow2`, about 570 MB, stored without zip compression because the squashfs inside is already xz). The boots are picked before the build now, so a typo in the `boots` input fails at once instead of after 10 min. The job has read access only: it runs mkosi as root with a lot of third-party code, and it publishes nothing.
- **`boot`** is a matrix: one job per boot group and firmware, all at once, each on its own runner. `unseeded seeded` is one group because those boots share one disk overlay, and `frozen` joins it when someone asks for it. Every other boot is its own group. So the full list is 10 jobs, a PR's `update` is 2, and `-f boots="remap"` is 2. `fail-fast` is off, so a red run shows every failing boot. Each job downloads the qcow2 and runs `make test-cloud-qemu` with `MOOSE_CLOUD_QCOW2` (new: boot this image, build nothing), `MOOSE_CLOUD_BOOTS` and `MOOSE_CLOUD_FIRMWARES` set to its group and firmware. Every boot job logged `accel=kvm`: matrix runners expose `/dev/kvm` the same way the single job did.
- **`publish`** runs only when a line publishes, and only after `build` and every `boot` job passed. It is the only job with `contents: write` and `packages: write`. The two publish steps are unchanged, guards included. For the control plane, `build` uploads the `moose-brain.tar` and `moose-ui.tar` it baked into both images, and `publish` loads them before the push. `docker load` keeps the image ID, so the job pushes the images the boots ran, not a rebuild. For the OS line, `build` uploads the production raw.

Why one job per boot and firmware: the longest single boot (`update`, about 2.7 min) sets the wall time. One job per firmware would take about 17 min, one job per boot running both firmwares about 13, this split about 11. Each job pays about a minute of setup and one image download.

The publish switches moved from the job's `env` to the workflow's `env`, so they are still computed once and every job reads the same values. `release.yml` and `os-lock-bump.yml` did not change. The "a PR boots only `update`" rule and the "a PR that touches `dev/os-lock/` runs the full list" rule are the same code as before, moved into the new matrix step.

### The boot-proof build reuses the tools tree

`dev/cloud/test/bootstrap.sh` takes an opt-in `MOOSE_MKOSI_TOOLS_TREE`, passed to mkosi as `--tools-tree`. CI sets it to `dev/cloud/mkosi.tools`, which the production build made a minute earlier from the same settings (mkosi v26's tools-tree cache manifest is the same for both configs). That saves the second tools-tree build, about 40 s. It is off by default: a tree named by path is not checked for being current, and a local tree can be old.

### The QEMU packages are downloaded once

In the first full run of this branch one boot job spent 6 min fetching QEMU (46 MB at 135 kB/s from the Ubuntu mirror), and with ten jobs at once the slowest one sets the wall time. So the build job now downloads the QEMU packages first, on a fresh runner, uploads them (`cloud-image-qemu-debs`), and every boot job installs them with `dpkg -i` in about 7 s, with no mirror. It is `dpkg`, not `apt`: given local files whose versions are also in the mirror, apt downloads them again (every job did, in run 36996912341). If the runner image moves between the build job and a boot job and the set no longer fits, the step falls back to the mirror with a warning.

### Layer cache for the image builds, only on runs that publish nothing

The `Makefile` builds the brain, UI and hosted Caddy images through `$(call docker_build,<scope>)`. With `MOOSE_BUILD_CACHE=gha` that is `docker buildx build --load` on a docker-container builder, reading and writing the GitHub Actions cache, one scope per image, and a failed cache write never fails the build. Otherwise it is the plain `docker build` it was. The workflow sets `MOOSE_BUILD_CACHE` from the same expression as `SHOULD_PUBLISH`, inverted: `gha` on a run that publishes nothing, `none` on any run where a line publishes. So a release reads no cache and builds what ships fresh, and no layer another run wrote can reach a release. `crazy-max/ghaction-github-runtime` (pinned by commit) gives BuildKit the cache token a `run` step does not see.

## Timings

Before: run 36941147838 (full list, the last run of #561). After: run 36999140120 (full list, commit 6588081, the last code change on this branch; the commit after it changes only docs and workflow comments). The layer-cache rows come from run 36992720567 (the first run on this branch, cache empty) and run 36994275441 (the next one, cache warm).

### Wall time

| Phase | Before | After |
|---|---|---|
| Setup (checkout, setup-mkosi, QEMU, go, builder) | 0.7 min | 1.0 min (now also downloads the QEMU packages once) |
| Control-plane images (brain, UI, hosted Caddy) | 2.2 min | 0.4 min (layer cache warm, and the same commit as the probe run before it; about 1.5 min on a new commit, see below) |
| Production mkosi build | 3.0 min | 2.6 min |
| Boot-proof image build (+ qcow2 + upload) | 3.8 min | 2.9 min (tools tree reused) |
| Boots | 17.5 min (14 boots in a row) | 3.6 min (10 jobs at once; the slowest job, `update` under BIOS, with its setup and download) |
| **Whole run** | **27.4 min** | **10.5 min** |

### Runner minutes

| Job | Before | After |
|---|---|---|
| Build | 27.4 min (one job did everything) | 6.8 min |
| Boot jobs | in the one job | 24.9 min (10 jobs, 1.5 to 3.5 min each) |
| **Total** | **27.4 min** | **31.8 min** |

Total runner minutes go up, from about 27 to about 32 (the plan said about 35), because each boot job pays its own setup (about 10 s for QEMU, a few seconds for go) and its own image download (6 to 35 s). That is acceptable: the repo is public, so standard GitHub-hosted runners cost nothing, and the wall time is what a person waits for.

### The layer cache

| Control-plane images step | Time |
|---|---|
| No cache (before, and every run that publishes) | 2.2 min |
| Cache empty, first run on a branch (run 36992720567) | 4.8 min |
| Cache warm, new commit (run 36994275441) | 1.5 min |
| Cache warm, same commit as an earlier run (run 36999140120) | 0.4 min |

So on a new commit the warm cache saves about 0.65 min; a re-run of the same commit saves about 1.8 min, because every layer hits. Most of it is the hosted Caddy image (62 s down to 9 s), whose inputs are all pinned. The brain and UI still rebuild their Go and Vite steps on every commit, because their build copies the whole tree. The first run on a branch pays about 2.6 min extra, almost all of it uploading the base image layers to the cache (`mode=max`). A branch reads only its own cache entries and those of `dev`, and nothing writes the cache on `dev`, so every new branch starts cold. See Known gaps.

## Known gaps

- **The layer cache only pays off from about the fifth run on a branch.** By the numbers above, a branch's first run is about 2.6 min slower (so about 13 min in all) and each later run on a new commit about 0.65 min faster (about 11.5 min in all). The cheapest fix is to cache only the hosted Caddy image, which has pinned inputs and gives most of the saving; another is to let a run on `dev` fill the cache, which branches can read. Neither is done here, because the maintainer chose this shape before these numbers existed.
- **No apt download cache, by decision.** The maintainer chose to drop it after these numbers: in run 36941147838 all downloads in one build took about 15 s (the mirrors served about 100 MB/s), and the boot-proof build already downloaded 0 B, because both builds share mkosi's package cache (`~/.cache/mkosi/debian~trixie~x86-64`, mkosi's `PackageCacheDirectory` default). `CacheDirectory=` in `dev/cloud/mkosi.conf` holds only incremental images, and `Incremental` is off, so it holds nothing worth keeping. A cache would have saved at most about 15 s and cost about as much to restore, plus a warm-up build on `dev`. If it is ever added, the pinned versions stay safe: apt uses a cached `.deb` only when its hash matches the signed snapshot index, and `os_lock_check` still compares every version with the lock.
- **The boot-proof image is still a second full mkosi build** (item 3 of the plan, skipped). Slot A is a read-only squashfs-xz in a fixed 1 GiB partition, so adding the test files as an overlay means unpacking the slot, adding files, squashing it again with options that match systemd-repart, and writing it back. The squash step costs about as much as the build it replaces (79 to 114 s), and the net saving after the tools-tree reuse is about a minute, for an image under test that is no longer made the way the shipped one is. A cleaner route for later: systemd 257 reads `systemd.extra-unit.*` and `systemd.unit-dropin.*` credentials, so the test harness could arrive over SMBIOS and a second disk with no change to the image at all.
- **The publish job has not run.** No run of this slice may publish. The publish steps are unchanged, but the hand-over is new: the `cloud-image-os-raw` and `cloud-image-control-plane` artifacts, and the `docker load` before the push. The first release after this merge is its first real run; read that run's publish job.
- **The image artifacts live for 3 days**, enough to re-run a failed boot or publish job. A re-run after that has to start from the build.

## What's next

- #562 and #563, the rest of #486: the signed bundle, then the A/B install and mark-good.
