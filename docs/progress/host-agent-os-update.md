# host-agent applies OS updates

- **Status:** done
- **Date:** 2026-10-02
- **Specs touched:** `docs/specs/UPDATES.md`, `docs/specs/BRAIN_HOST_PROTOCOL.md`, `docs/specs/BUILD.md`, `docs/specs/ENVIRONMENT.md`, `docs/specs/NOTIFICATIONS.md`, `docs/specs/TESTING.md`, `docs/architecture.md`, `docs/dev/hosted-boot-proof.md`, `docs/dev/contributing.md`, `CLAUDE.md` (three log fields, approved by the maintainer)

Closes #563, the fifth slice of #486. It follows [hosted-ab-layout.md](hosted-ab-layout.md) (#561), which built the hosted image in the A/B layout with RAUC and nothing that marks a slot good, and [rauc-bundle.md](rauc-bundle.md) (#562), which builds a signed bundle per OS release that nothing installed. Now `host-agent` installs that bundle into the other slot, switches inside the update window, keeps the new slot once the brain is healthy, and goes back to the old slot on its own when it is not. This is #486 steps 4 to 6 on the hosted profile. The appliance gets it with #564.

## What was done

### The answer's OS part (`internal/hostagent/updatetarget`)

- **Wire.** The update-target answer gains an optional `os` list, oldest first, each entry `{version, bundle_url, bundle_sha256}`. The last entry is the target. Left out, the answer has no opinion about the OS.
- **Checks** (`os.go`, `ValidateOS`): a plain `X.Y.Z` version, ascending order, a 64-hex lowercase sha256, an absolute http or https URL, and a URL that starts with the expected prefix (`MOOSE_UPDATE_OS_URL_PREFIX`, default `https://github.com/onmoose/os/releases/download/`, the same kind of check as the expected image repositories).
- **Which release** (`PickOS`), as `UPDATES.md` # 1 says: on a later minor, the first entry above the box's own minor (a box never skips a minor); on its own minor or one back, the target; further back, refused. A list that leaves a minor out is refused (the step must be the minor right after the box's own). A release below the running control plane's floor is refused too. The brain writes `minimumAgentVersion` to `/var/lib/moose/state/minimum-host-agent` at start for this; a box that cannot read the file refuses every move to an older release.
- **Each stream is judged on its own.** A bad OS part, or one of the wrong JSON shape (decoded apart from the rest), refuses stream A only. A refused control-plane part, or a box that cannot read its running pair, still lets stream A go on. The test answer of the new boots carries only an OS part, which proves it.
- **Order in the window.** The loop runs stream B first. When stream B starts an update in this tick, or cannot because a job runs, stream A waits for the next tick.

### The transaction (`internal/hostagent/osupdate`, new)

- **Install, ahead of the window** (`os-install` job). Download to `/var/lib/moose/os-update/bundle.raucb.part` on the state partition, hashing as it writes. Only a file whose sha256 is the one the answer names is renamed and handed to `rauc install`, so RAUC never sees a bundle the answer did not name. RAUC then checks the signature against the image's keyring. `system.conf` now has `activate-installed=false`, so the install leaves the boot order alone. The file is deleted afterwards. A failed attempt is not repeated for the same release and digest the same night.
- **Switch, inside the window** (`os-switch` job). Check that RAUC reports the other slot holds the release, write the record, write the trial marker `/var/lib/moose/os-update/trial-<slot>`, run `rauc status mark-active other`, and check the grubenv reads `ORDER="<new> <old>"`, `<new>_OK=1`, `<new>_TRY=0` before rebooting. RAUC 1.13's GRUB backend already resets `TRY` there (its `grub_set_primary`, read in the source), so the #570 gap is now a check, and a failing check puts the booted slot first again and does not reboot. A reboot that is not accepted undoes the switch. **One OS switch per night**, kept in the record across the reboot, so a box several minors behind takes one step per window. **Nothing installs or switches while the booted slot is on trial**: the other slot is then the way back.
- **The trial, at the next start of `host-agent`** (`Boot`). With the booted slot's marker: wait for the brain's `/healthz` up to `MOOSE_OS_TRIAL_TIMEOUT` (10 min); healthy means `mark-good`, the marker goes, the outcome is `good`; not healthy means `mark-bad booted` and a reboot. Without a marker: `mark-good` at once. With the other slot's marker: that slot failed, so the outcome is `reverted`, the slot is marked bad, the marker goes. **Only the first boot after a switch is on trial** (the maintainer's call).
- **The safety net in the image.** `moose-os-trial.timer` runs `/usr/lib/moose/os-trial-check` 15 minutes after boot. If the booted slot still has its marker and the grubenv does not already mark it good (a marker host-agent failed to remove), its `host-agent` never got there, so the script marks the slot bad, leaves a note `safety-net-<slot>` and reboots. The old slot's `host-agent` logs from the note that the safety net made the revert, which the `os-revert` boot checks. The boot-proof image sets it to 90 s.
- **Jobs.** `hostagent.Agent.StartJob` runs any job under the one lock `system-update` takes. The OS jobs carry no result.
- **Record.** `state.json`: what was installed where, the last install attempt, the last switch, the last outcome with an id. Fields only grow, because either release may read it.

### Reporting

- `GET /v1/system/status` and `GET /api/v1/system/version` carry `os_version` and `os_slot`. The OS version is `host-agent`'s own version, which is the version of the slot it ships in.
- `GET /v1/system/update-target`, and `GET /api/v1/system/update-target`, carry an `os` object: `state` (`unsupported`, `none`, `refused`, `current`, `installing`, `installed`, `waiting`, `rebooting`, `held`, `failed`), `running`, `slot`, `target`, `detail` and `last`. The loop decides once per tick, so between ticks `host-agent` refreshes the job states from the job and the record (`Peek`).
- **Admin notification.** A brain loop on the health-poll cadence reads `os.last` and raises one notification per outcome id: info "moose updated its system to X", warning "A system update did not work, so moose went back". It checks the store for the dedup key first, because a re-raise would mark a read notification unread again, and skips outcomes older than 7 days, so a pruned row is not raised again.
- The fake `host-agent` reports `unsupported`, or a canned state from `MOOSE_FAKE_OS_UPDATE`. The appliance build has no OS applier and reports `unsupported`.
- `CLAUDE.md` gains `os`, `slot` and `digest` in the standard log fields.

### The boots (`os-update`, `os-revert`)

- **The test bundle** (`dev/cloud/test/build-os-test-bundle.sh`, in the build job only when one of the two boots runs): slot A of the boot-proof image, repacked with a `host-agent` stamped one patch release above `VERSION`, the baked tarballs under `/var/lib/moose` left out (a slot's copy is read only at a box's first boot), squashfs gzip at level 1 (GRUB reads it), signed with the throwaway key. One bundle serves both boots.
- **The harness** puts it on an ext4 image attached as a second read-only disk, gives each boot its own overlay, box-id, test-portal key and owner assertion, and does not pass `-no-reboot`, so the box reboots between slots inside one QEMU run. The in-guest script keeps its stage on the state partition.
- **`os-update`**: on slot A, owner sign-in, a data file and the `whoami` app; a wrong digest is refused and slot B stays empty; the right digest with a shut window installs into slot B and leaves `ORDER="A B"`; an open window switches and reboots. On slot B: marked good, `ORDER="B A" B_OK=1 B_TRY=0`, `host-agent` at the new version, the version read and the `os` read right, the owner's session, password hash, SSH host key, `machine-id`, the file and the app unchanged, and the admin notification there.
- **`os-revert`**: the same install, with a drop-in that keeps `host-agent` off slot B. On slot B the script only checks that and waits. The safety net reboots the box, GRUB skips the slot still on trial, and on slot A `host-agent` records the revert and marks B bad. Then the same checks as `os-update`, with the `held` state and the warning notification.
- **On a run that publishes the OS**, `os-update` runs only its refusal half (the wrong digest, then RAUC refusing the throwaway-signed bundle with the right digest), and `os-revert` is left out of the matrix (the maintainer's call).
- **Both are in every full run**, so lock bumps and releases run them. A PR that touches the code still boots only `update` (the maintainer's call).

## The private-side change this needs (for the maintainer)

The box side is done. **No production box moves its OS until the control plane sends the OS part.** The exact wire change to `GET /api/updates/target`:

- Add an optional top-level field `os`: a JSON array, oldest first, of objects `{"version": "X.Y.Z", "bundle_url": "<url>", "bundle_sha256": "<64 lowercase hex>"}`.
- **Which entries:** the newest patch of each minor, from the oldest minor still supported up to the box's OS target, with the target last. For a box that should stay on its OS, either leave `os` out or send a list whose last entry is the release it runs.
- **`bundle_url`:** the Release asset, `https://github.com/onmoose/os/releases/download/vX.Y.Z/moose-vX.Y.Z-amd64.raucb`. Anything else must also start with that prefix, or the box refuses it.
- **`bundle_sha256`:** the content of the release's `moose-vX.Y.Z-amd64.raucb.sha256` asset (the hex digest only), read once when the OS release is recorded and stored with it. Never computed per request, and never "latest".
- **Leave it out entirely** when there is no OS target. An entry with a missing or malformed digest makes the box refuse stream A and log it; stream B is not affected.
- It is a per-box fact like the control-plane target, so per-box pinning and staged rollout work the same way. The control-plane part and the OS part are independent: a box may get either, both, or neither.

## Numbers

From runs on this branch (the final run is in # How it was verified):

| | Value |
|---|---|
| Download per update (a real release) | 438.5 MB, one bundle (`rauc-bundle.md`) |
| Disk the box needs while it installs | 438.5 MB on the state partition, 1.1% of a 40 GB disk, deleted after the install |
| Time to apply, measured in the guest | NUMBERS-APPLY |
| Test bundle in CI | 366.4 MB, built in 27 s (unsquashfs 4 s, mksquashfs gzip 4 s), uploaded in 5 s |
| CI cost of the two boots | NUMBERS-CI |

## How it was verified

All in CI, every publish input false. Never built or booted locally.

NUMBERS-RUNS

- **Tests.** `internal/hostagent/updatetarget/os_test.go` (the checks, every pick rule, the loop: install outside the window and switch inside it, stream B first, a bad OS part refused with stream B still applying, an answer with only an OS part, current, none, unsupported). `internal/hostagent/osupdate/osupdate_test.go` against a fake two-slot RAUC (a normal boot marked good at once, install then switch, a wrong digest never reaching RAUC and not retried the same night, a stale `TRY` stopping the switch and putting the booted slot back, a busy lock, a good trial, a failed trial rebooting and the old slot recording the revert and holding, the floor file, the JSON and grubenv parsers, `Peek`). The report's `os` part, the brain's pass-through, the notification, the store lookup and the brain's outcome check have tests too. `make check` green.

## How it maps to the specs

- `UPDATES.md` # 1: steps 1 to 6 of the transaction, built on hosted. The section has an "As built (#563)" part and the trial rule. # 8.4 step 4 names the wire. The rollback table says built on hosted.
- `BRAIN_HOST_PROTOCOL.md`: `os_version`/`os_slot` on the status read, the `os` object on the update-target read, the two new job kinds under the one lock, and how rule D (drain before the reboot) is met.
- `BUILD.md` # 1b: the status, `activate-installed=false`, the trial timer, the install proof.
- `DECISIONS.md` 2026-10-02 ("a leaked signer alone cannot push an update"): realized by the digest check before RAUC.

## Review

- **The fresh review agent** found one Block and two Shoulds, all fixed: the loop could install into the old slot, the way back, while the new slot was still on trial, and a multi-step update could switch twice in one night (now: nothing moves during a trial, one switch per night); a failed `rauc status` at start left a trial to the image timer (now retried for about 30 s); a failed read of the record could be written over (now never). Its nits are fixed too: two log field names, a "notified" line for an outcome that raised nothing, and a 2 GiB cap on the download.
- **Greptile** found seven, all fixed: an `os` part of the wrong JSON shape failed the whole answer (now decoded apart, stream A only); a list that left a minor out let the box skip it (now refused); the trial-overwrite case above; a failed reboot after `mark-active` left the new slot first (now undone); a stale marker on a slot already marked good would make the timer revert it (now the script checks the grubenv first); a missing floor allowed a downgrade (now every downgrade needs the floor); and a plain local `make test-cloud-qemu` had no test bundle (now the harness builds it from the local image).

## Known gaps & deviations

- **No production box moves its OS yet.** The private side has to send the `os` part (above).
- **QEMU only**, as the rest of #486. #486's "Done when" still needs the same proof on a provisioned box.
- **The test bundle is not a release bundle.** It is the boot-proof slot with one binary changed, gzip-compressed, signed with the throwaway key. The release bundle (xz, release signer) is checked by #562's build checks, not installed here. A release run installs nothing.
- **The trial checks the brain's `/healthz` only.** It does not check Caddy, the UI or the apps. The spec asks for `host-agent` and the brain.
- **A `held` release after a revert waits for the next night**, and the bundle is downloaded again then. The fleet halt that would stop a broken release (`UPDATES.md` # 8.5) is deferred.
- **No report back to the cloud.** `UPDATES.md` # 8.4 step 5 still waits for box authentication.
- **The floor needs a brain from this change.** An older brain writes no floor file, and `host-agent` then refuses every OS downgrade on that box. Upgrades are not affected.
- **An `http` update target URL carries the digest unprotected**, as it already carries the control-plane digests. RAUC's signature check still applies. Production uses https.
- **Same-night key.** One attempt per night uses the window's calendar night. A test run that crosses midnight UTC between its stages would see a new night.
- **The version read degrades.** When the source cannot be read after a reboot, `os.state` is `none` with the last outcome still there; it does not say "unreachable" for stream A.

## What's next

1. The maintainer adds the `os` part to the control plane's answer (above), then #486's proof on a provisioned box under both firmwares.
2. #564: the appliance image in the A/B layout, which then gets this applier.
3. Box authentication, for the report back and the fleet halt (`UPDATES.md` # 8.4, # 8.5).
