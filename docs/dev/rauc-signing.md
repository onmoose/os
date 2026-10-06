# Signing OS update bundles

Every OS release ships a RAUC bundle, `moose-vX.Y.Z-amd64.raucb`, next to the disk image (`../specs/BUILD.md` # 1b # The bundle). A box installs a bundle only if its signer chains to the root CA in the box's own `/etc/rauc/keyring.pem`. This page is the maintainer's how-to for the keys. The decision behind it is `../specs/DECISIONS.md` 2026-10-02 (key custody for the OS bundle).

## The shape

- **The root CA lives offline**, with the maintainer. Its private key never touches CI, a server or this repo. Its public cert is committed as `dev/release/rauc/release-ca.pem`, and every image built to publish bakes it as `/etc/rauc/keyring.pem`.
- **A signer, issued by the root, lives in CI.** Its cert and key are the secrets `RAUC_SIGNING_CERT` and `RAUC_SIGNING_KEY` of the GitHub Environment `os-release`. Only `main` and `v*` tags may use that environment, and only the `sign` job of `CI / Cloud image` enters it. That job does nothing else: it runs after every boot passed, checks the bundle, re-signs it and hands it to the publish job. The build job, which runs mkosi as root, never sees the key.
- **What the signature vouches for.** The build job builds the bundle, signs it with the throwaway key and reports its sha256. The `sign` job re-signs only a bundle with that digest. So the release signature says "this is what the build job of a release run produced", and no more: a build job that was made to produce a bad bundle would get it signed. The control for that is on the box: it installs only the bundle whose digest its update target names (`../specs/UPDATES.md` # 1, built by #563), and that digest comes from the cloud control plane (hosted) or the minisign-signed release manifest (appliance), not from this CI.
- **Runs that publish no OS image use a throwaway root.** It is made once per checkout under `.dev/rauc/throwaway/` and never stored anywhere else. Such an image trusts only bundles signed in that checkout, so a stray CI artifact trusts nothing real.
- **There are no CRLs.** A CRL expires, and a box that was offline past its expiry could then never update again. A leaked signer is handled by rotation (below). On top of the signature, a box only installs the bundle whose digest its update target names (`../specs/UPDATES.md` # 1), so a signer key on its own cannot push an update to a box.

All keys are EC P-256. A signer carries the codeSigning purpose, and the image's `/etc/rauc/system.conf` asks for it (`check-purpose=codesign`).

**What happens before the setup is done** (`dev/release/require-release-ca.sh` checks that the root is present, is a CA, has not expired and is not one of the throwaway or check CAs CI makes):

- **A merge that bumps `VERSION` while `release-ca.pem` is missing tags nothing.** `release.yml` stops before it tags or creates any Release, on either line, even when the same merge bumps `CONTROL_PLANE_VERSION` too. The OS line is never dropped quietly. Commit the root first (# 4), or release only the control plane by bumping only `CONTROL_PLANE_VERSION`.
- **A merge that bumps only `CONTROL_PLANE_VERSION` needs neither the root nor the signer.**
- **The root committed, but a secret missing:** `release.yml` tags and creates the Releases, then the `sign` job fails with "no release signer" and the publish job does not run, so nothing is published on either line. Add the secrets (# 3) and re-run `release.yml`; it resumes.
- **A dispatch** with `publish_os` (or a hand-pushed `v*` tag) fails at its first step without the root, and in `sign` without the secrets.

## 1. Make the root CA (once, offline)

On a machine that is not CI, with this repo checked out and `openssl` installed:

```bash
dev/release/rauc-ca.sh root ~/moose-rauc-ca
```

openssl asks for a passphrase twice. It encrypts `root-ca.key` with it (AES-256). The root is valid for 20 years. You get:

- `~/moose-rauc-ca/root-ca.key`: **secret**. Keep it offline, with a backup (for example a second encrypted USB stick in another place), and the passphrase in your password manager. Losing it means the next root change needs two releases instead of one (# 5).
- `~/moose-rauc-ca/root-ca.pem`: public.

## 2. Issue a signer (once now, then at each rotation)

```bash
dev/release/rauc-ca.sh signer ~/moose-rauc-ca signer-2026
```

openssl asks for the root's passphrase. The signer is valid for 3 years. Give each signer a new name. The script checks that it chains to the root and carries the codeSigning purpose. `dev/release/rauc-ca.sh show ~/moose-rauc-ca/signer-2026.pem` prints its dates.

## 3. Create the environment and its two secrets

In GitHub: **Settings > Environments > New environment**, named `os-release`.

- **Deployment branches and tags:** "Selected branches and tags", with the branch rule `main` and the tag rule `v*`. No required reviewer: the merged release PR is the approval.
- **Environment secrets:** `RAUC_SIGNING_CERT` holds the whole text of `signer-2026.pem`, and `RAUC_SIGNING_KEY` the whole text of `signer-2026.key`, both with their `-----BEGIN` and `-----END` lines.

Or with the CLI:

```bash
gh api -X PUT repos/onmoose/os/environments/os-release \
  -F deployment_branch_policy[protected_branches]=false \
  -F deployment_branch_policy[custom_branch_policies]=true
gh api -X POST repos/onmoose/os/environments/os-release/deployment-branch-policies -f name=main -f type=branch
gh api -X POST repos/onmoose/os/environments/os-release/deployment-branch-policies -f name='v*' -f type=tag
gh secret set RAUC_SIGNING_CERT --env os-release < ~/moose-rauc-ca/signer-2026.pem
gh secret set RAUC_SIGNING_KEY  --env os-release < ~/moose-rauc-ca/signer-2026.key
```

Then delete the local copy of the signer's key: `shred -u ~/moose-rauc-ca/signer-2026.key`. A new signer is cheap to issue; a key that lingers on a laptop is not.

Create the environment **before** the first OS release. GitHub creates an environment that a job names but that does not exist yet, with no branch rule, the first time such a job runs.

## 4. Commit the root's public cert

```bash
mkdir -p dev/release/rauc
cp ~/moose-rauc-ca/root-ca.pem dev/release/rauc/release-ca.pem
```

Commit it through a normal PR into `dev`. From the next OS release on, the image bakes it and the bundle is signed by the release signer.

## 5. Rotate or replace

- **Rotate the signer** (every year or two, and at once if the key may have leaked): issue a new one (# 2) and replace both secrets (# 3). Boxes change nothing, because they trust the root. The old signer's cert stays valid until it expires, so if it leaked, the update-target digest is what keeps it from reaching a box; ship the next release soon so the fleet moves on.
- **Replace the root** (it leaked, or it nears its 20-year end). Two OS releases:
  1. Make the new root (# 1, a new directory) and put **both** roots into `dev/release/rauc/release-ca.pem` (old first, then new; RAUC trusts every CA in the file). Release it. This release is still signed by the signer in CI, which the old root issued: it needs no use of either root's key. Wait until the fleet runs this release.
  2. Issue a signer from the new root, replace the secrets, drop the old root from the file, and release again.

  **If the old root's key is lost,** this still works, as long as the signer in CI is still valid: step 1 uses only that signer. What a lost root key blocks is issuing a new signer from it. So if the old root's key is lost and the current signer has also expired or leaked, no bundle the fleet trusts can be signed any more, and every box needs a new image (re-provisioning on hosted). Keep the backup, and start a root change well before the signer expires.
- **A box that missed a release** still trusts what its own slot carries. So a root change must reach the whole fleet before the old root leaves the file.

## Where it is in the code

- `dev/release/rauc-ca.sh`: the root and signer commands above. CI uses it for the throwaway root.
- `dev/cloud/rauc.sh`: which keyring an image bakes (`MOOSE_RAUC_KEYRING`, set by `ci-cloud-image.yml`), and the pinned RAUC container.
- `dev/cloud/build-bundle.sh`: the build job. It makes the bundle with the throwaway signer and checks it against the keyring read back out of the slot.
- `dev/release/require-release-ca.sh`: the check `release.yml` runs before it tags an OS release, and every OS publish run first.
- `dev/release/sign-bundle.sh`: the `sign` job. It refuses without the secrets, without the committed root, or when the image would trust the throwaway signer, then re-signs and checks the result.
- `dev/release/attach-image.sh`: the publish job attaches the image, the bundle and their checksums. It detects and refuses a mixed set (some of the four already on the Release) and never overwrites one.
