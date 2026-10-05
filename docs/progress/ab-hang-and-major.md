# A slot that hangs, and a Debian major across the /etc overlay

- **Status:** done
- **Date:** 2026-10-05
- **Specs touched:** `docs/specs/BUILD.md`, `docs/specs/UPDATES.md`, `docs/specs/NEXT.md`, `docs/specs/DECISIONS.md`, `docs/architecture.md`, `docs/dev/hosted-boot-proof.md`

A slice of #486. It settles points 3 and 5 of `NEXT.md` # A/B OS image, which [ab-os-update-design.md](ab-os-update-design.md) opened and [hosted-ab-layout.md](hosted-ab-layout.md) narrowed: whether a Hetzner VM has a watchdog, so a hung slot still reboots and reverts, and what happens to the `/etc` upper layer when a box moves to another Debian major.

## What was done

### Point 3: Hetzner Cloud has a hardware watchdog

Checked on real Hetzner Cloud VMs, not in QEMU. Three `cx23` servers (the smallest box), stock Debian 13, two locations on AMD EPYC-Rome hosts and one on an Intel Skylake host, all booting legacy BIOS. Each lived a few minutes and was deleted with its SSH key; the API then listed no server, key, primary IP or snapshot made by the probe. The cost was about €0.035 (three started hours of a cx23).

- **The device.** The VM is QEMU's `q35` machine. Its ICH9 chipset (`8086:2918`, the LPC bridge at `00:1f.0`) has a TCO watchdog timer. There is no `i6300esb` and no ACPI `WDAT` table. The same chipset is there on the Intel and the AMD hosts.
- **Stock Debian shows nothing.** Hetzner's Debian image runs the `-cloud` kernel, which has no `iTCO_wdt` module: no `/dev/watchdog`, `wdctl` finds no device, `WatchdogDevice=` is empty.
- **The moose kernel drives it.** With `linux-image-amd64`, the kernel moose ships, `lpc_ich` and `iTCO_wdt` load on their own: `Found a ICH9 TCO device (Version=2, TCOBASE=0x0660)`, `initialized. heartbeat=30 sec (nowayout=0)`, `/dev/watchdog0` with identity `iTCO_wdt`. With `RuntimeWatchdogSec=60s`, as the image sets, systemd logs `Using hardware watchdog 'iTCO_wdt', version 2, device /dev/watchdog0` and `Watchdog running with a hardware timeout of 1min.` The box stayed up 150 s with systemd feeding it.
- **It fires.** Opened once and never fed (`echo 1 > /dev/watchdog`, 30 s timeout, `watchdog did not stop!`): the Intel VM reset after about 50 to 65 s, the AMD VM after 59.5 s. The journal of the boot that reset ends with no shutdown.
- **A real hang is covered.** With systemd feeding it at 60 s, PID 1 was frozen with `ptrace` (`PTRACE_SEIZE` and `PTRACE_INTERRUPT`, state `t (tracing stop)`, no sysrq). The VM reset 118.3 s after the freeze, with no shutdown in the journal.
- **The boot lane has it too.** It is `q35` as well. A probe run (37358065628, `unseeded`, both firmwares) logged the same `iTCO_wdt` lines, so `BUILD.md`'s "none in the QEMU lane" was wrong.

What changed:

- **No image change.** The kernel already has the driver and `10-moose-watchdog.conf` already sets 60 s. Only its comment changed.
- **Every boot checks it** (`cloud-assertions.sh`, layout): `WatchdogDevice` set, `/sys/class/watchdog/watchdog0` there, `RuntimeWatchdogUSec=1min`, and systemd's `Using hardware watchdog` line in the boot's log. systemd logs that line before journald runs, so it is in the kernel log, not under `_PID=1`.
- **No hang proof in CI** (the maintainer's call). The Hetzner probe is the proof; the boot lane only checks the device is fed.
- **Two gaps, in `BUILD.md` # 1b # As built.** The reset comes after about twice the timeout (the TCO timer fires on its second expiry), so about 2 minutes on a box. A hang in GRUB or the initramfs is not covered, only a crash through `panic=10`; the watchdog is not started earlier because a long `e2fsck` or `systemd-growfs` of a big state partition would reset the box again and again.
- **The appliance fallback** is recorded for #564: `softdog` only where a machine has no hardware watchdog. It covers a hung PID 1, not a kernel locked up with interrupts off, and nothing before systemd.

### Point 5: a Debian major tidies the /etc upper layer

The maintainer chose option A (`DECISIONS.md` 2026-10-05).

- **`state-setup` step 3a** (`dev/cloud/mkosi.extra/usr/lib/moose/state-setup`). It reads the slot's Debian major from `/usr/lib/os-release` and the box's from `/state/etc/.moose-debian-major`. When they differ, before `/etc` is mounted, it builds a new upper layer beside the old one:
  - the files on `/usr/lib/moose/etc-keep.list` (and any `etc-keep.d/*.list`), copied with `cp -a --parents`, whiteouts included;
  - `passwd`, `group`, `shadow`, `gshadow` merged by `awk`: the slot's lines in order, then the box's lines the slot lacks. In `shadow` and `gshadow` the box's password wins; in `group` and `gshadow` the members are joined. An id that differs between box and slot is logged;
  - `daemon.json` from the slot when its `userns-remap` is the box's, `login.defs` from the slot when it still sets `SUB_UID_COUNT 0` and `SUB_GID_COUNT 0`; the box's copy otherwise, with a log line.
  Then the old upper layer is renamed into `/state/etc/attic/<time>-debian-<from>-to-<to>/`, the new one takes its place, the overlay work dir is emptied, and the major is recorded last. A reboot between the two renames is finished on the next boot; one before them starts again. A box with no record records the slot's major and tidies nothing. It runs the same in both directions. **A tidy-up that fails does not stop the boot:** it logs, leaves the upper layer as it was, keeps the old major recorded and tries again next boot. A failed swap puts the old layer back, and stops the boot only when even that fails (an empty upper layer would boot a box with no users or host keys); the next boot then moves the new layer into place. A slot with no `VERSION_ID` (Debian testing or sid) skips the step, and a record the full state partition cannot take is logged. The first CI runs showed why: a bug in it panicked slot B, GRUB fell back to slot A, and slot A, which runs the same step against the same record, panicked too, for ever.
- **The keep list** (`etc-keep.list`): `machine-id`, `ssh/ssh_host_*`, `ssh/sshd_config.d/moose-allowed.conf`, `ssh/moose-authorized-keys`, `localtime`, `subuid`, `subgid`, and for the appliance `moose/secrets`, `moose/data-drive.enrolled`, `NetworkManager/system-connections`.
- **sshd after a major.** The enable links of `ssh.service` are not kept, so a renamed unit in a new major does not leave a dead link. When `ssh.service` was enabled in the old layer, a swap leaves `/state/etc/.moose-major-tidied`; when an admin had turned sshd off by hand, it leaves none (Greptile, round 2). At its next start `host-agent` sees it, calls `sshaccess.Manager.EnsureOnAtStart` (when the drop-in names an account and the unit is not enabled, `systemctl enable --now`), and removes the marker. Only then: on any other start it leaves sshd alone, so an admin's `systemctl disable ssh` stands, which matters on hosted, where sshd's run state is the only control over :22. It never turns sshd off.
- **Accounts keep their ids** (rule 3). `mkosi.postinst.chroot` writes every account, group and membership of the image into `/usr/lib/sysusers.d/moose-image-accounts.conf`, so `systemd-sysusers`, which runs on every boot, adds one a later image made at build time. `build-bundle.sh` check 6 reads that file out of the slot and fails when it differs from `dev/os-lock/cloud-accounts.lock` (48 groups and 21 accounts today), which is edited by hand.
- **The checks.** At the end of every boot (`ok()`), every file in the upper layer must be covered by a keep list, an account file, a pinned file or an sshd link, and every account in the image's file must be on the box with the same id. The boot lane's own host-agent drop-ins are on its own list (`etc-keep.d/boot-lane.list`, written by `dev/cloud/test/bootstrap.sh`). The `os-update` boot fakes a major: before the switch it turns SSH on for the owner, records Debian 12 and plants an admin's edit in `/etc`. On slot B it wants the tidy-up's kernel log line, the major recorded as 13, the edit in the attic and gone from `/etc`, and the owner's password hash, SSH host key, `machine-id` and groups unchanged (`MAJOR TIDY OK`). It also wants the drop-in still naming the owner, `ssh.service` enabled again by host-agent, its log line, and the marker gone (`SSHD BACK OK`). The `os-revert` boot proves the revert direction: on slot B, before the safety net reboots it, it records Debian 14; slot A must tidy 14 to 13 on the way back (attic entry, record back to 13, the identity facts unchanged). There the owner has SSH on in the drop-in but sshd was turned off by hand, so the tidy-up must leave no marker and sshd must stay off (`MAJOR TIDY BACK OK`).

## Numbers

| What | Value |
|------|-------|
| Disk cost on a box, point 3 | none: no image change |
| Disk cost on a box, point 5 | the attic: one copy of the old upper layer per major crossed, a few hundred KB (under 0.001% of the 40 GB smallest box); the generated sysusers file in the slot, a few KB |
| Hetzner watchdog reset, 30 s timeout, not fed | about 50 to 65 s (Intel), 59.5 s (AMD) |
| Hetzner watchdog reset, 60 s timeout, PID 1 frozen | 118.3 s |
| Hetzner probe cost | about €0.035: three cx23 VMs, a few minutes each |
| CI time | no new boot. `os-update` took 208 s (UEFI) and 187 s (BIOS) in run 37378140993, against 220 s and 207 s before (run 37349532308): the faked major adds nothing measurable. The full list ran in 14.6 min wall |

## How it was verified

The Hetzner probe above, and `CI / Cloud image` with `publish=false`; nothing was built or booted locally.

| Run | Boots | What it showed |
|-----|-------|----------------|
| 37358065628 | `unseeded`, a log-only probe | the boot lane has the same `iTCO_wdt` watchdog and systemd feeds it |
| 37364028120 | `os-update` | red: the first boot of a new box failed in `state-setup` (the major was written before `/state/etc` existed), a panic loop; the build printed the image's accounts for the lock |
| 37370270487 | `os-update` | red: the tidy-up on slot B tried to copy a plain path the box did not have, panicked, and GRUB's fallback slot A ran the same step and panicked too. Hence the rule that a failed tidy-up never stops a boot |
| 37374562648 | `os-update` | red only in the check: the tidy-up worked (the major was recorded as 13), but the journal's kernel log did not have its line; the check now reads `dmesg` first |
| 37376508079 | `os-update` | green under both firmwares: `MAJOR TIDY OK`, kept 16 files, the admin's edit in the attic, the owner intact; upper-layer and account checks green; check 6 green |
| **37378140993** | **the full list, head `f7108c7`** | **green, all 14 jobs: 7 boot groups under UEFI and under legacy BIOS, 14.6 min wall. Every boot checked the watchdog, the upper layer (17 files on the `os-update` boot) and the 69 image accounts** |

After review round 1 (a slot with no `VERSION_ID`, a failed swap, a full state partition, sshd re-enabled only after a tidy-up, and an SSH proof across the faked major):

| Run | Boots | What it showed |
|-----|-------|----------------|
| 37380583165 | `unseeded os-update` | green under both firmwares: `SSH on for 'owner'` before the switch, `SSHD BACK OK` and `MAJOR TIDY OK` (kept 18 files) on slot B |
| **37380588141** | **the full list (the PR's own run), head `2304072`** | **green, all 14 jobs, 14.2 min wall** |

`make test-nopam` is green; `sshaccess` has a new test for `EnsureOnAtStart`.

## How it maps to the specs

- `NEXT.md` # A/B OS image: point 3 resolved for hosted, point 5 resolved. # OS major-version upgrade commitment now points at the tidy-up.
- `BUILD.md` # 1b: four overlay rules instead of three (rule 3 gains the generated sysusers file and the id lock, rule 4 is new); # As built: the watchdog, its two gaps and the appliance fallback; the inventory row for the sshd links.
- `UPDATES.md` # 1: the hang sentence.
- `DECISIONS.md` 2026-10-05: the tidy-up, and why not a migration step.

## Known gaps

- **The tidy-up must ship in a Debian 13 release before the first Debian 14 release,** or a revert from 14 to 13 is not tidied. Boxes never skip a minor, so any 13 release before 14.0 is enough.
- **A real major was not run.** The `os-update` boot fakes one by changing the record; both slots are Debian 13. The day a 14 image exists, its account ids will likely move (Debian gives system ids in install order), and check 6 will fail until the build pins them.
- **A slot that disagrees with the box on an id** keeps the slot's id in the merged files, and the tidy-up only logs it. Check 6 is what keeps that from happening.
- **A hang before systemd is not covered,** and the reset takes about 2 minutes (`BUILD.md` # 1b # As built).
- **The appliance** (#564) gets the tidy-up and the keep list as they are, but not proved there yet, and needs `softdog` where it has no hardware watchdog.

## What's next

- #564: the appliance image in the A/B layout, with the same `state-setup`, and `softdog` as the fallback watchdog.
- Before the first Debian 14 image: pin the system account ids in its build so check 6 passes.
