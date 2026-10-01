# OS package lock: Debian snapshot, Docker pins, daily bump

- **Status:** done
- **Date:** 2026-10-01
- **Specs touched:** docs/specs/BUILD.md, docs/specs/NEXT.md, docs/specs/DECISIONS.md, docs/dev/contributing.md, docs/architecture.md

Closes #560, a slice of #486, after [control-plane-version-line.md](control-plane-version-line.md). It builds the OS package lock that [ab-os-update-design.md](ab-os-update-design.md) designed in `BUILD.md` # 1b. With no `apt` on the box, this lock is how a Debian security fix reaches an image.

## What was done

- **A feasibility probe first, in CI** (run 36867301586, a temporary workflow, removed before the PR). mkosi v26's `Snapshot=` works for Debian trixie. Two timestamps gave two `openssl` versions, and the newer one came from `snapshot.debian.org/archive/debian-security/<ts>`. mkosi leaves out `trixie-updates` in snapshot mode, never passes the snapshot to its tools tree, and always turns off apt's `Valid-Until` check, so an old timestamp still builds. An apt pin to an exact Docker version installs that version.
- **`dev/os-lock/`**, three files. `debian-snapshot` holds the timestamp (`20261001T082322Z`). `third-party.lock` holds the four Docker packages as `package=version` lines. `cloud-packages.lock` is the resolved list, 163 packages as `name version architecture`, made from the first locked CI build.
- **`dev/os-lock/oslock`**, a small Go tool with tests. It normalizes mkosi's JSON manifest, checks it against the committed list, writes the PR title and a Markdown table of the changes (marking a version that is in `trixie-security`), and moves each Docker pin to the newest version within its major, with dpkg's version order. One test checks the committed files fit together: a valid timestamp, a pin for each Docker package `dev/cloud/mkosi.conf` installs, each pin in the resolved list, and the list in the exact form the tool writes.
- **`dev/os-lock/os-lock.sh`**, sourced by both cloud builds. It writes the build's apt tree: Docker's repo, `trixie-updates` from the same snapshot, one `Pin-Priority: 1001` pin per Docker package, and `Acquire::Retries "5"`. The builds pass the timestamp to mkosi as `--snapshot`.
- **The check, in both cloud builds.** `dev/cloud/bootstrap.sh` (the shipped image) and `dev/cloud/test/bootstrap.sh` (the boot-proof image) each compare their manifest with `cloud-packages.lock` and fail on any difference. In CI that is two separate builds of one commit against one list. The lean check on names stays as it was. The boot-proof image's rebuild canary now includes all three lock files, the resolved list too, so a local re-run after any lock change rebuilds and runs the check instead of exiting early. The lean build has no early exit.
- **`CI / Cloud image`** uses a new shared setup action (`.github/actions/setup-mkosi`), uploads the resolved list as the `cloud-packages-lock` artifact (on failure too), runs on a PR that touches `dev/os-lock/`, and gives such a PR the full boot list instead of only `update`. A new `ref` call input lets the bump boot its branch; a run with a `ref` asserts it publishes nothing. The PR's file list comes from the compare API, because the PR files API needs a permission that `release.yml`'s call does not grant.
- **`.github/workflows/os-lock-bump.yml`**, daily at 05:17 UTC and on dispatch. Two jobs. `resolve` (no secret, read only) starts from `bot/os-lock` with the base merged in when the branch exists, or from the base when it does not. It moves the snapshot and the pins, builds the lean image in record mode, and does nothing when no package changed. Otherwise it hands only the three lock files and the PR title and body to `handover` as an artifact. `handover` builds nothing: it merges the base into the branch the same way, applies the files, adds one commit, and pushes without force, so a person's commits on the branch and a PR opened by hand survive. A merge conflict or a moved branch stops the run with a summary. With a working `OS_LOCK_BOT_TOKEN` it opens or updates one PR. Without one, or when the token is set but rejected (checked with an API call, and again if its push is refused), it pushes with `GITHUB_TOKEN`, says "renew it" when the token was rejected, boots the branch by calling `ci-cloud-image.yml`, and writes the diff and an "open a PR" link (or the open PR's number) to the job summary and to one tracking issue, then comments the boot result there. `base=hotfix/X.Y.Z` runs it for an OS patch release; `CI / Go` and `CI / Cloud image` now also run on PRs into `hotfix/**`, and `CI / Go` on PRs that touch `dev/os-lock/`, so the lock consistency test runs on every bump PR.
- **Release process decided** (maintainer call): a lock-only OS patch release is cut from `main` as `hotfix/X.Y.Z`, the one PR into `main` that does not come from `dev`. A bump that changes a `trixie-security` package is released within 7 days; others ship with the next normal release. Written into `docs/dev/contributing.md` # Release model, `DECISIONS.md` 2026-10-01 and `NEXT.md` # A/B OS image point 6.

## How it was verified

All in CI, never locally, with every publish input false.

- **Snapshot feasibility:** run 36867301586 (temporary probe, described above).
- **First locked build:** run 36873363298 built the image at the lock and failed on purpose, because `cloud-packages.lock` did not exist yet. Its `cloud-packages-lock` artifact is the committed list: 163 packages, the four Docker pins at their pinned versions, the lean check still exact at 162 names.
- **Two builds, one list, full boots:** run 36874269703 (`CI / Cloud image`, dispatch). The lean image and the boot-proof image each logged `package lock check passed: 163 packages`, then all of `unseeded seeded bios access update ssh remap` passed.
- **The bump's no-token path, end to end:** run 36875320798, a temporary copy of `os-lock-bump.yml` on this branch with the token forced empty and the old lock edited so the change path runs. It resolved the snapshot, built in record mode, wrote the title (`OS lock (security): openssl 3.5.7-1~deb13u1 to 3.5.7-1~deb13u3, tzdata ...`) and the body, pushed `bot/os-lock-test-560` with `GITHUB_TOKEN`, and the called `ci-cloud-image.yml` built that ref with the full boot list and published nothing. The tracking-issue step was left out of the test to keep the repo free of a test issue. The test branch and the temporary workflow are removed.
- **Review fixes, the reworked bump (no-token path, a rejected token, a kept branch):** two runs of a temporary copy of the new `os-lock-bump.yml`, with a fake token value instead of the secret and the baseline edited so the change path runs. The tracking-issue step was left out.
  - Run 36882113674: the branch did not exist. The `handover` job's API check rejected the fake token and warned "OS_LOCK_BOT_TOKEN is set but rejected; renew it", then pushed a new branch from the base with `GITHUB_TOKEN`. (Its boots were cancelled to make room for run 2.)
  - A person's commit was then pushed to the branch by hand.
  - Run 36883124055: the branch existed. `resolve` and `handover` both merged the base into it, one commit was added on top, and the push was a plain fast-forward: the person's commit is still in the history. The called `ci-cloud-image.yml` booted the branch with the full list and published nothing.
  - The test branch and the temporary workflow are removed. The merge-conflict stop and the workflow-file refusal were not triggered in a test.
- **Final branch:** the PR's own `CI / Cloud image` and `CI / Go` runs on the final head (see the PR).
- `make check` green locally, with PAM headers; `actionlint` clean on every workflow.

## How it maps to the specs

- `BUILD.md` # 1b # The OS package lock: the timestamp, the committed resolved list, exact pins for the third-party repo and the scheduled bump PR, all as designed. The section now carries an "As built" part.
- `BUILD.md` # Versioning: "a lock bump that changes packages is an OS patch release" now has its process.
- Maintainer defaults: `expected-packages.txt` stays as the reviewed name set; `trixie-updates` comes back from the snapshot; Docker bumps stay within the current major; no fallback to the live mirror and no `.deb` cache or archive.

## Known gaps & deviations

- **The token path is verified only after merge.** The maintainer created `OS_LOCK_BOT_TOKEN` while this PR was open. It was not used from the feature branch on purpose: a bump branch made from an unmerged branch would open a PR into `dev` that carries the unmerged changes. The first real bump is a dispatch after merge, `gh workflow run "OS lock bump" --ref dev`, and that run is the end-to-end check of the token path (one PR into `dev` from `bot/os-lock`, whose `CI / Cloud image` runs the full boot list). The no-token path was run before merge from a temporary copy of the workflow on this branch, with the token forced empty (# How it was verified).
- **A push that brings in a workflow change is refused.** When `dev` changed a file under `.github/workflows/` since the last bump, merging `dev` into the bot branch brings that change in, and GitHub refuses the push unless the token may write workflows. `GITHUB_TOKEN` never may. The run stops and says so; the fix is a token with **Workflows: read and write**, or merging `dev` into the branch by hand. The workflow and `contributing.md` both say so.
- **The appliance lane (`dev/test-qemu/`) is not locked.** It is bookworm and local-only. It gets the lock with its move to trixie and the A/B layout (#564).
- **The mkosi tools tree is not locked.** mkosi v26 does not pass the snapshot to it. It decides the build tools, not the image's packages.
- **The kernel's versioned package** (`linux-image-6.12.111+deb13-amd64`) is in the lock; only the lean check drops it, as before.
- **Docker's signing key** is still fetched at build time. Versions are pinned, the key is not.
- **The brain's image** still `apt-get`s `docker-ce-cli` from a live index (`BUILD.md` # 5c). That is the control plane, not the OS image, and out of this slice.

## What's next

1. After merge, run the first bump by hand: `gh workflow run "OS lock bump" --ref dev`. Check that it opens one PR into `dev` from `bot/os-lock` (or, when no package moved since `20261001T082322Z`, that it ends with "No package changed"), and that the PR's `CI / Cloud image` run boots the full list.
2. Renew `OS_LOCK_BOT_TOKEN` before it expires, or replace it with a GitHub App token (`docs/dev/contributing.md` # OS package lock bumps).
3. Lock the appliance lane with #564.
