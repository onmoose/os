# A/B OS update: the design, and two version lines

- **Status:** done (design only; the build is split into #559 to #564)
- **Date:** 2026-10-01
- **Specs touched:** `UPDATES.md`, `BUILD.md`, `DECISIONS.md`, `NEXT.md`, `RELEASE_MANIFEST.md`, `ENVIRONMENT.md`, `STORAGE.md`, `SPEC.md`, `CONTROL_PLANE.md`, `docs/architecture.md`

This entry is the first slice of #486, "Decide and record". It follows [ab-update-engine-spike.md](ab-update-engine-spike.md), which picked RAUC with GRUB in #485. It adds no code. It writes the A/B OS image into the specs as the way stream A works, records two product calls the maintainer made while doing so, and splits the rest of #486 into six issues.

## What was done

### Three decisions (`DECISIONS.md` 2026-10-01)

1. **Stream A is an A/B OS image, built with RAUC and GRUB, on both profiles.** `apt` and `unattended-upgrades` leave the update path. `host-agent` ships inside the image, so its `.deb` and apt repo are not built. Nothing a user installs goes on the host.
2. **Appliance OS updates apply automatically and reboot in the window**, with no prompt, the same as hosted. This flips "reboots: never force". The maintainer chose it: an A/B update only takes effect after a reboot, so a box that waits for a click is never patched, and the automatic revert is the rollback `UPDATES.md` said auto-apply was waiting for.
3. **A moose release is the OS release, and the control plane has its own version.** The maintainer asked for this. moose `vX.Y.Z` (root `VERSION`) names the OS image. The brain and UI move to their own semver in `CONTROL_PLANE_VERSION`, tagged `control-plane-vX.Y.Z`, so they can ship often without an OS release. The two meet only at the control plane's compatibility floor, `minimumAgentVersion` and `minimum_host_agent`, which already compare against `host-agent`'s version and so need no wire change. This reverses the 2026-07-16 one-version decision in part.

### The design, in the specs

- **`UPDATES.md` # 1 and # 2** are rewritten. # 1 is the update transaction: learn the target through the one update-target seam, install into the inactive slot ahead of the window, switch and reboot in the window after stream B, mark good only when `host-agent` and the brain are healthy, revert on its own otherwise, one attempt per target per window. It states that the revert covers the OS and never the state partition, so on-disk formats must stay readable by the previous release, and that a slot which hangs must still revert. # 2 says `host-agent` ships in the image. # 3 says an OS update never changes which control plane a box runs. # 7 (ordering, reboots, compatibility, rollback table), # 8.4 step 4 and the locked decisions follow.
- **`BUILD.md` # 1b** is new. It is the image's shape: the partition table (ESP, BIOS boot, two 4 GiB slots, a state partition that grows), GRUB on both firmwares with RAUC's `grubenv` backend, what the state partition holds (the `/etc` overlay and the bind mounts) and what stays in the slot, the three overlay rules (changed files stop following the image; four pinned files, now with `login.defs`; system users from `sysusers.d`), appliance encryption (verity slots, LUKS state partition, TPM PCR 7), the signed bundle, and the **OS package lock**: a Debian snapshot timestamp plus the committed package list, Docker's packages pinned in the same lock, and a scheduled bot PR that bumps it.
- **`BUILD.md` # 4, # 6, # Versioning and the locked decisions** follow: `host-agent` baked into the image, a `.raucb` artifact, and the two version lines, marked as planned beside what is built today.
- **Review fixes:** the release manifest's `minimum_host_agent` floor gates only the brain and UI, never the planned `os` field, so a box below the floor can still update its OS. Curated Tier-2 packages are baked into the image (`SERVICE_PROVISIONING.md`). Only a minor OS release may change an on-disk format, a box never skips a minor, and an OS downgrade goes back at most one minor and never below the running control plane's floor (`UPDATES.md` # 1). Merging a lock bump puts a fix on `dev`, and it ships with the next `VERSION` release. `HEALTH.md`'s `reboot-required` and the `NOTIFICATIONS.md` reboot row are marked as retiring.
- **`ENVIRONMENT.md`**: on hosted, the state partition takes over from `moose-grow-root`, GRUB replaces systemd-boot for UEFI, and stream A rides the update-target answer. **`STORAGE.md` # OS drive**: the slots and the state partition on the appliance, with the recovery passphrase staying on the encrypted OS drive. **`RELEASE_MANIFEST.md`**: a planned optional `os` field, and apt references replaced. **`SPEC.md` # OS update model** and **`CONTROL_PLANE.md`**: apt retired.
- **`NEXT.md`**: "OS major-version upgrade commitment" is resolved (nobody reinstalls; a Debian major is a new image). "Reboot scheduling UX" narrows to how the reboot is shown. The phased-rollout topic notes that its main trigger is now designed. A new Tier 2 topic, **"A/B OS image: what to prove before it ships"**, holds six open points: Secure Boot with signed GRUB, verity in the initrd, a hanging slot and a watchdog on Hetzner, a replaced OS drive, a Debian major across the overlay, and how OS patch releases are cut.

### #486 split into six issues

| Issue | Slice | Depends on |
|---|---|---|
| #559 | The control plane gets its own version line | none |
| #560 | The OS package lock and its bot bump PRs | none |
| #561 | Hosted image in the A/B layout, with the state inventory (#486 step 3) | none |
| #562 | Build, sign and publish a RAUC bundle per OS release | #561 |
| #563 | `host-agent` applies OS updates (#486 steps 4 to 6) | #561, #562 |
| #564 | Appliance image in the A/B layout: verity, LUKS, Secure Boot | #561, `NEXT.md` points 1, 2, 4 |

All six are sub-issues of #486, which stays open as the umbrella until its "Done when" (a provisioned box, both firmwares) is met.

## How it maps to the specs

- `DECISIONS.md` 2026-08-11 called stream A "one atomic unit" with an A/B image as its end state. This makes that the v1 mechanism, and makes "a box is described by two numbers" literal: one OS version, one control-plane version.
- `BUILD.md` # User-namespace remap and #486's comment: the pinned list is how an OS update keeps each box's remap as it was built.
- `STORAGE.md` # Encryption posture: TPM unseal against PCR 7 stays, on the state partition instead of the root.

## Known gaps & deviations

- **Nothing is built.** Every section added is marked "designed, not built (#486)", and today's behavior (one `VERSION`, a single grown root, no OS updates) is still described as built where it is.
- **The version split is documented, not implemented.** The maintainer allowed that if it was too big for this change. It is #559, and it has no dependency, so it can go first.
- **Two calls in the design were not put to the maintainer**, because they follow from the spike: 4 GiB slots, and the state partition holding `/var/log` and `/var/lib/systemd` beside the paths #486 listed. Both are easy to change before #561 builds them, and hard to change after.
- **The appliance control plane stays admin-prompted.** `UPDATES.md` # 3 gave "no A/B rollback at the OS level" as one reason. That reason is gone, but the control plane has its own rollback and the question was not asked, so the policy is unchanged.

## What's next

1. #559 and #560, which have no dependencies, and #561, which unblocks the rest.
2. Decide `NEXT.md` # A/B OS image points 3 and 6 before either profile ships OS updates.
3. #562, then #563, then #486's own proof on a provisioned box under both firmwares.
4. #564 once the Secure Boot and verity points are proved.
