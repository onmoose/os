# A signed RAUC bundle per OS release

- **Status:** done
- **Date:** 2026-10-02
- **Specs touched:** `docs/specs/BUILD.md`, `docs/specs/UPDATES.md`, `docs/specs/DECISIONS.md`, `docs/specs/NEXT.md`, `docs/architecture.md`, `docs/dev/hosted-boot-proof.md`, `docs/dev/contributing.md`, `docs/dev/rauc-signing.md` (new)

Closes #562, the fourth slice of #486. It follows [hosted-ab-layout.md](hosted-ab-layout.md) (#561), which built the hosted image in the A/B layout with RAUC and no keyring, and [ci-cloud-image-speedup.md](ci-cloud-image-speedup.md), which split `CI / Cloud image` into a build job, a boot matrix and a publish job. This slice adds the artifact a running box will download to update its OS: one signed RAUC bundle per OS release, attached beside the disk image. Nothing installs it yet; that is #563.

## What was done

### The bundle

- **What is in it.** One image, `rootfs.img`: slot A of the image that ships (the production build, the one whose `.raw.xz` is attached), cut out at the squashfs's own size rounded up to 4 KiB. A new `-extract` flag on `dev/cloud/slotbudget` does the cut, reusing its GPT and superblock reader. The manifest takes `compatible` from the slot's own `/etc/rauc/system.conf` (`moose-hosted-x86_64`), `version` from `VERSION` and `build` from the git commit. Format `verity`, no hooks, no `adaptive=`.
- **How it is built.** `dev/cloud/build-bundle.sh`, a new step in the build job right after the image build. RAUC runs in the pinned `debian:trixie-slim` (`BRAIN_RUNTIME_IMAGE`), with `rauc` at the version `dev/os-lock/cloud-packages.lock` names, installed from snapshot.debian.org at the locked timestamp, so the build's RAUC is the box's RAUC (1.13-3+deb13u1). The helpers live in `dev/cloud/rauc.sh`.
- **The keyring in the image.** `/etc/rauc/system.conf` gains `[keyring] path=keyring.pem check-purpose=codesign`. The keyring itself is staged into the generated wiring tree by `stage-control-plane.sh`, so the lean image and the boot-proof image carry the same one. `MOOSE_RAUC_KEYRING` picks it: `release` (the committed `dev/release/rauc/release-ca.pem`) on a run that publishes the OS line, `throwaway` (a root made once per checkout under `.dev/rauc/throwaway/`) on every other run and on local builds. No test key and no placeholder root is committed.
- **Checks in the build job,** against the `system.conf` and keyring read back out of the slot with `unsquashfs`, not the repo copies: (1) the slot carries the keyring the build staged; (2) the bundle verifies against the throwaway root; (3) the image's own config accepts it with the throwaway keyring, and must refuse it with the release keyring; (4) a wrong key, an unrelated CA, is refused; (5) a rehearsal of the `sign` job's `dev/release/sign-bundle.sh` with a throwaway "release" CA, so the publish-side script runs on every build and not first on a real release.
- **The boot lane** checks on every boot that `/etc/rauc/keyring.pem` is there, that it comes from the slot and not the `/etc` upper layer (so a new image's keyring reaches the box), and that `system.conf` asks for `check-purpose=codesign`. It names the keyring in a `layout: rauc keyring from the slot:` line.

### Signing and publishing

- **Key custody, decided by the maintainer** (`DECISIONS.md` 2026-10-02): an offline root CA, and a signer it issues, kept as the secrets `RAUC_SIGNING_CERT` and `RAUC_SIGNING_KEY` of a GitHub Environment `os-release`, limited to `main` and `v*` tags, with no required reviewer. No CRLs.
- **The build job signs with the throwaway key only**, and reports the bundle's sha256 as a job output.
- **A `sign` job of its own** re-signs it. It runs only when the OS line publishes, after every boot passed, with a plain `environment: os-release`, and it does nothing else. It signs only a bundle whose sha256 matches the build job's, with `dev/release/sign-bundle.sh` (`rauc resign`). Before it signs, that script refuses when a secret is empty, when the image keyring is not the committed release root, and when that keyring accepts the throwaway-signed input. It writes the bundle only after it verifies against the image's own config and keyring, and the job reports the signed bundle's sha256. So the release key never meets the job that runs mkosi as root, and the signature vouches for what the build job produced. The box's check of the digest its update target names (#563) is the control for a bad build.
- **The publish job** needs `sign` when the OS line publishes (`!cancelled()` lets it run when `sign` was skipped on a control-plane-only publish, so that path is as before). It checks the signed bundle against the digest `sign` reported before it attaches anything. A run that publishes is no longer cancelled by a later push (`cancel-in-progress` is false for it), so a Release upload is never cut off halfway.
- **No OS release is tagged before the key setup.** `release.yml` runs `dev/release/require-release-ca.sh` before it tags or creates any Release for a merge that bumps `VERSION`. Without a valid release root (present, a CA, not expired, not a throwaway or check CA) it stops, on both lines, so a merge that bumps both never tags two Releases and then publishes nothing. The OS line is never dropped quietly. A merge that bumps only `CONTROL_PLANE_VERSION` needs no root and no signer. A dispatch that publishes the OS runs the same check at its first step.
- **One set of four files.** `dev/release/attach-image.sh` now takes any number of files and treats them as one set: the image, its checksum, the bundle and its checksum. It detects and refuses a mixed set: all on the Release are left, none are uploaded in one call, and any other mix refuses with the `gh release delete-asset` lines to run. That covers a Release cut before this change too, which has the image pair and no bundle. A rebuilt bundle never joins an image from another build. (The upload of four files is not atomic; a cut-off upload is caught by the next run.)
- **The publish job's summary** lists both files and each one's share of the release download. The build job's summary lists the bundle's size and its share of the slot.

### For the maintainer

- **`dev/release/rauc-ca.sh`**: `root DIR` makes the root (EC P-256, key encrypted with AES-256, 20 years), `signer DIR NAME` issues a signer (codeSigning, 3 years, checked against the root), `show` prints a cert. It refuses to overwrite a root or a signer, and a run that fails part way removes the files it made, so it can be run again. CI uses it for the throwaway root, with short lifetimes and no passphrase.
- **`docs/dev/rauc-signing.md`**: make the root, issue a signer, create the environment and its two secrets, commit `release-ca.pem`, rotate the signer, replace the root in two releases.

## Numbers

From run 37010678846 (the build job's summary) and the probe run 37007422220 (the disk image's `.raw.xz`):

| | Bytes | Share |
|---|---|---|
| Slot image in the bundle | 435,179,520 (435.2 MB) | 40.5% of the 1 GiB slot |
| Bundle (`.raucb`: the image, a 3.4 MB verity tree, the signature) | 438,498,575 (438.5 MB) | 40.8% of the slot |
| Disk image (`.raw.xz`) | 435,828,888 (435.8 MB) | |
| An OS release download, both files | about 874 MB | the bundle is 50.2% |

Added CI time: the bundle step takes 24 to 53 s in the build job (the container and its apt install, `rauc bundle` 4 s, the rehearsal about 20 s), which is also the added wall time, since the boots wait for the build job. An OS release also runs the `sign` job (a runner, the 438 MB bundle artifact down and the signed one up, a re-sign of about 25 s: about 1.5 runner minutes, in series before publish) and uploads 438 MB more to the Release. So under 1 runner minute on every run, and about 3 on an OS release.

## How it was verified

All in CI, every publish input false. Never built or booted locally.

- **Probes, before the plan** (a temporary step, dropped from the branch): run 37004090875 found that the slim image has no CA store, so the snapshot is fetched over plain http (apt checks the signed index anyway); run 37004816613 that RAUC asks for the S/MIME purpose unless `check-purpose=codesign` is set, and refuses a code-signing cert; run 37005541036 that `rauc resign` on a verity bundle wants a loop device and exclusive access in a plain container; run 37006246355 that `rauc resign --no-verify` works and leaves the payload byte for byte the same, and that the old CA then refuses the result; run 37007422220 the sizes and timings.
- **The branch:** run 37009950553 (red: the slotbudget build ran as the caller into a root-owned directory), run 37010678846 (green with `-f boots="ssh"`: every check, the rehearsal, and the keyring line on the boots under both firmwares).
- **The final head:** the full list, see the PR.
- **Tests:** `dev/cloud/slotbudget` (`-extract` cuts at the rounded size, stops at the partition, and fails and leaves no file on an image cut short inside the slot), `dev/release/publish_test.go` (the four-file set: none, all, the image pair only, the bundle only, and a single file refused), `dev/release/rauc_test.go` (`rauc-ca.sh` makes a root and a codeSigning signer that chains to it, with private key modes, and refuses to overwrite; a failed `rauc-ca.sh` run leaves nothing and can be run again; `require-release-ca.sh` refuses a missing root, a throwaway root and a signer cert, and accepts a real root; `sign-bundle.sh` refuses with no secrets and with no committed root, or a keyring that is not it, before any container starts and without writing a bundle). `make check` green; `actionlint` clean.

## How it maps to the specs

- `BUILD.md` # 1b # The bundle: the verity bundle, the keyring at `/etc/rauc/keyring.pem`, and the digest check on top (#563), as designed. The section now has an "As built (#562)" part. `BUILD.md` # 6: the `.raucb` is a built artifact.
- `UPDATES.md` # 1 step 2: the signature check now has its keyring and its signer.
- `NEXT.md` # Build & distribution: the OS bundle half of the signing item is resolved; the release manifest's minisign custody stays open.

## Known gaps & deviations

- **The release paths have not run.** No run of this slice may publish, so the release keyring build, check 3's release branch, the `sign` job and its environment, the real `sign-bundle.sh` with the secrets, the publish job's new `needs` condition and the four-file attach first run on the first OS release after the maintainer's setup. The rehearsal runs `sign-bundle.sh` on every build, and the refusals are unit-tested. `release.yml`'s new check only runs on `main`; its script is unit-tested.
- **No `rauc install` proof.** That RAUC's `raw` handler takes `rootfs.img` into a slot is first exercised by #563's boot, by the maintainer's call.
- **The bundle is cut from the production image, not the boot-proof image.** That matches the disk image, which is also not the exact image the boots ran: the boot-proof image is a second build with the test harness added.
- **The first OS release after this needs the maintainer's setup first** (`docs/dev/rauc-signing.md`). Until `release-ca.pem` is committed, `release.yml` tags nothing for a merge that bumps `VERSION`, on either line, even when the merge bumps `CONTROL_PLANE_VERSION` too. A merge that bumps only `CONTROL_PLANE_VERSION` releases as before. With the root but without the secrets, the Releases are tagged and the `sign` job fails, so nothing is published until the secrets exist and `release.yml` is re-run.
- **Review (Greptile, dismissed):** the boot-proof canary in `dev/cloud/test/bootstrap.sh` names the keyring mode, not the root's bytes, so a local re-run after a root change could reuse an old boot-proof image. The canary only gates local re-runs; CI always builds from a fresh checkout, so it is left as it is.
- **The throwaway root is per checkout, not per run.** In CI every run is a fresh checkout. A local build reuses `.dev/rauc/throwaway/` until `make clean` or it expires (30 days); such an image never ships.
- **A leaked signer stays valid until it expires**, since there are no CRLs. The update target's bundle digest (#563) is what keeps it from reaching a box; `rauc-signing.md` says to rotate and release soon.
- **The bundle name has no profile in it.** The appliance (#564) will need its own `compatible` and a name that tells the two apart.

## What's next

1. The maintainer's setup: the offline root, a signer, the `os-release` environment and its secrets, and `release-ca.pem` committed (`docs/dev/rauc-signing.md`). Then read the first OS release's publish job.
2. #563: host-agent installs the bundle into the other slot, checks its digest against the update target, resets the slot's `TRY` flag, marks good and reverts. Its boot proves `rauc install`.
3. #564: the appliance image, with its own `compatible`.
