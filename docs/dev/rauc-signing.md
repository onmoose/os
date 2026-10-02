# Signing OS update bundles

Every OS release ships a RAUC bundle, `moose-vX.Y.Z-amd64.raucb`, next to the disk image (`../specs/BUILD.md` # 1b # The bundle). A box installs a bundle only if its signer chains to the root CA in the box's own `/etc/rauc/keyring.pem`. This page is the maintainer's how-to for the keys. The decision behind it is `../specs/DECISIONS.md` 2026-10-02 (key custody for the OS bundle).

## The shape

- **The root CA lives offline**, with the maintainer. Its private key never touches CI, a server or this repo. Its public cert is committed as `dev/release/rauc/release-ca.pem`, and every image built to publish bakes it as `/etc/rauc/keyring.pem`.
- **A signer, issued by the root, lives in CI.** Its cert and key are the secrets `RAUC_SIGNING_CERT` and `RAUC_SIGNING_KEY` of the GitHub Environment `os-release`. Only `main` and `v*` tags may use that environment, and only the publish job of `CI / Cloud image` enters it. The build job, which runs mkosi as root, never sees the key.
- **Runs that publish no OS image use a throwaway root.** It is made once per checkout under `.dev/rauc/throwaway/` and never stored anywhere else. Such an image trusts only bundles signed in that checkout, so a stray CI artifact trusts nothing real.
- **There are no CRLs.** A CRL expires, and a box that was offline past its expiry could then never update again. A leaked signer is handled by rotation (below). On top of the signature, a box only installs the bundle whose digest its update target names (`../specs/UPDATES.md` # 1), so a signer key on its own cannot push an update to a box.

All keys are EC P-256. A signer carries the codeSigning purpose, and the image's `/etc/rauc/system.conf` asks for it (`check-purpose=codesign`).

Until `dev/release/rauc/release-ca.pem` is committed, every run that publishes the OS line fails at once with "release-ca.pem is not committed yet". Until the two secrets exist, it fails in the publish job with "no release signer". In both cases nothing is published.

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
  1. Make the new root (# 1, a new directory) and put **both** roots into `dev/release/rauc/release-ca.pem` (old first, then new; RAUC trusts every CA in the file). Release it, still signed by a signer of the old root. Wait until the fleet runs this release.
  2. Issue a signer from the new root, replace the secrets, drop the old root from the file, and release again.

  If the old root's key is lost, step 1 cannot be signed by it: every box then needs a new image (re-provisioning on hosted). Keep the backup.
- **A box that missed a release** still trusts what its own slot carries. So a root change must reach the whole fleet before the old root leaves the file.

## Where it is in the code

- `dev/release/rauc-ca.sh`: the root and signer commands above. CI uses it for the throwaway root.
- `dev/cloud/rauc.sh`: which keyring an image bakes (`MOOSE_RAUC_KEYRING`, set by `ci-cloud-image.yml`), and the pinned RAUC container.
- `dev/cloud/build-bundle.sh`: the build job. It makes the bundle with the throwaway signer and checks it against the keyring read back out of the slot.
- `dev/release/sign-bundle.sh`: the publish job. It refuses without the secrets, without the committed root, or when the image would trust the throwaway signer, then re-signs and checks the result.
