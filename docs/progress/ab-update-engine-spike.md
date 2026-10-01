# Spike: pick the A/B OS update engine

- **Status:** done
- **Date:** 2026-10-01
- **Specs touched:** none (a spike; the findings feed the design issue)

This entry closes the spike in #485. It compares two engines for image-based OS updates (stream A, `UPDATES.md` # 1 and # 2): **RAUC** and **systemd-sysupdate**. Rugix was dropped before the spike started. It also answers the userns-remap requirement that #486 added: the update must never turn Docker's remap on or off under a box.

The spike code is throwaway. It lives only on the branch `test/485-ab-update-spike` (at `21895b3`, in `dev/spike-ab-update/`) and is never merged. Its CI workflow is `.github/workflows/spike-ab-update.yml` on that branch. The passing run is **36836466471**.

**Recommendation: RAUC, with GRUB on both firmwares.** The reasons and the trade-off are in # Recommendation.

## What was done

### The test box

One small trixie image, built with mkosi v26 in two profiles (`rauc`, `sysupdate`). It carries Docker (with the userns-remap on, as the hosted image has it), sqlite3 and sshd, and no control plane. Each engine builds it three times: v1, v2, and a broken v3.

The disk layout is the same for both engines, so the proofs compare like with like:

| # | Partition | Size | Where it comes from |
|---|---|---|---|
| 1 | ESP | 512M | in the image |
| 2 | BIOS boot | 1M | in the image |
| 3 | slot A | 3G | in the image, holds v1 |
| 4 | slot B | 3G | made by `systemd-repart` at first boot |
| 5 | data | the rest | made and grown by `systemd-repart` at first boot |

A slot holds a whole root. Everything a box writes that must outlive a slot swap lives on the data partition. In this entry "data partition" always means partition 5 of the **OS drive**. It is not the appliance's separate data drive (`STORAGE.md` # Data drive(s)).

- `/etc` is an overlay. The slot's `/etc` is the lower layer, and the upper layer is on the data partition. So users, passwords, SSH host keys and `machine-id` land on the data partition.
- On the first boot only, three files are copied up on purpose ("pinned"): `/etc/docker/daemon.json`, `/etc/subuid` and `/etc/subgid`. A pinned file never follows the image again. This is the #486 rule: the box keeps its remap setting for life.
- `/home`, `/var/lib/docker` and `/var/lib/moose` are bind mounts from the data partition.

A small PID 1 wrapper (`/usr/lib/spike/init`) sets this up before systemd starts. It is the cheapest way to prove the layout on both engines at once. A real build would do it in the initrd.

The health gate (`spike-health.service`) stands in for "host-agent and the brain are healthy". On success it marks the slot good: `rauc status mark-good` for RAUC, and `systemd-bless-boot` (through `boot-complete.target`) for sysupdate. On failure the unit has `FailureAction=reboot`. v3 always fails the gate.

### The proof run

The guest runs the whole sequence on its own, one stage per boot, with no NIC and nobody at the console. The update artifacts come in on a second, read-only disk. The sequence is: boot v1, seed data, install v2, reboot, check, install the broken v3, reboot, v3 fails its gate and reboots, back on v2, check again. Each run ends with a `SPIKE-VERDICT: PASS` or `FAIL` line on the serial console.

The data seeded on v1 and checked after the update and after the revert: a Docker volume and a stopped container, a SQLite file in `/var/lib/moose`, and a file in `/home` owned by a user made on the box. The per-box state checked: that user's `/etc/shadow` line, `machine-id`, the SSH ed25519 host key, the `moose-remap` line in `/etc/subuid`, Docker's data root (`/var/lib/docker/1000000.1000000`) and `name=userns` in `docker info`.

v2 changes the image on purpose, to test the per-box rules. Its `daemon.json` has the remap **off**. It also adds one system user through a `sysusers.d` file and another with a plain `useradd` at build time.

### Results

All three boot sets pass in run 36836466471.

| Proof | RAUC, UEFI | RAUC, legacy BIOS | sysupdate, UEFI | sysupdate, legacy BIOS |
|---|---|---|---|---|
| 1. Two slots and a data partition; first boot makes slot B and grows data | pass | pass | pass | fail: no native rollback (systemd-boot is UEFI-only) |
| 2. Update written to the other slot while running, then reboot into it | pass | pass | pass | fail: no native rollback (systemd-boot is UEFI-only) |
| 3. Broken update reverts on its own, no console | pass | pass | pass | fail: no native rollback (systemd-boot is UEFI-only) |
| 4. Docker volume, container, SQLite and `/home` intact after update and revert | pass | pass | pass | fail: no native rollback (systemd-boot is UEFI-only) |
| Extra: per-box state survives the swap (remap files, password hash, SSH host key, `machine-id`) | pass | pass | pass | fail: no native rollback (systemd-boot is UEFI-only) |

sysupdate was not run under BIOS. Its rollback is systemd-boot's boot counting, and systemd-boot does not exist under legacy BIOS. A GRUB boot counter built around sysupdate was ruled out before the spike, so that column is a recorded fail, not a test.

How each revert looked on the box:

- **RAUC.** GRUB keeps `ORDER`, `<slot>_OK` and `<slot>_TRY` in a `grubenv` on the ESP. It sets `TRY=1` before it boots a slot, and `mark-good` clears it. v3 kept `A_TRY=1`, so on the next boot GRUB skipped slot A and booted v2 in slot B. The same `grub.cfg` and `grubenv` work under both firmwares.
- **sysupdate.** v3's UKI was installed as `moose-spike_3+1-0.efi`. systemd-boot counted the attempt down to `+0-1`, and the gate failed, so it was never blessed. On the next boot systemd-boot sorted it last and booted `moose-spike_2.efi`.

The broken v3 booted once on each engine before the fallback. A full run of four boots took 73 s (RAUC) and 126 s (sysupdate) under KVM.

What the per-box rules showed, the same on both engines:

- **The pin works.** v2 ships `daemon.json` with the remap off. The box still runs v1's file, with `"userns-remap": "moose-remap"`, after the update and after the revert. Docker's data root and the apps' owners did not move.
- **A new system user must come from `sysusers.d`.** The `sysusers.d` user from v2 reached the box. The `useradd` user did not, because the box's `/etc/passwd` is already in the overlay's upper layer, so the new image's copy is hidden.

### Bugs found on the way

These cost CI runs and are worth knowing for the build.

- **Debian splits RAUC in two.** The `rauc` package is the client only. The D-Bus daemon that `rauc install` and `rauc status mark-good` talk to is in `rauc-service`. Without it, `mark-good` fails with `The name de.pengutronix.rauc was not provided by any .service files`.
- **trixie's `systemd-import` does not unpack zstd, and says it succeeded.** sysupdate hands the download to `systemd-import`. Given a `.raw.zst`, the 257 build writes the compressed bytes to the partition as they are, and exits 0. `systemd-fsck` then finds no filesystem and skips, and the initrd fails to mount the root. Checked by hand in a `debian:trixie` container: a `.zst` round trip gives a file that starts with the zstd magic (`28 b5 2f fd`); a `.xz` round trip gives the original bytes. The spike now ships `.raw.xz` and checks the new slot's filesystem type after install.
- **A slot that hangs never reverts.** In run 36791598772, sysupdate's v2 (with the zstd bug) stopped in the initrd's emergency mode. The root account is locked, so it sat at "Press Enter to continue" for the whole 25-minute timeout. Boot counting only helps a slot that reboots. RAUC's `TRY` flag has the same limit. See # Known gaps.
- Smaller ones, fixed earlier on the branch: sysupdate needs `UnifiedKernelImages=unsigned` in mkosi v26 to build UKIs without a key; Debian ships sysupdate's timers even with no binary, and mkosi's presets turn them on; PID 1 starts with no `PATH`; `docker-cli` is only a Recommends of `docker.io` on trixie.

### Building sysupdate ourselves

Debian builds systemd with `-Dsysupdate=disabled`, so trixie has no `systemd-sysupdate`. The spike builds it in a `debian:trixie` container from upstream `v257.13`, the same tag as trixie's systemd. systemd 257 has no standalone target for it, so the build script adds one to `src/sysupdate/meson.build`, modelled on `systemd-repart.standalone`. The binary links systemd's shared code statically, so it does not depend on Debian's private `libsystemd-shared-257.so`. The build took 50 to 59 s. sysupdate still runs Debian's `systemd-import` (from `systemd-container`) to write the slot, and that is where the zstd bug lives.

### Sizes

From `sizes.txt` in run 36836466471. The slot is a fixed 3G partition.

| | RAUC | sysupdate |
|---|---|---|
| Used in the slot | 682 MB | 602 MB |
| What the box downloads per update | one bundle, 350 MB (`.raucb`, verity format, signed) | root image 231 MB (`.raw.xz`) + UKI 47 MB + `SHA256SUMS` |

RAUC's slot is larger because the kernel and a Debian initramfs live inside it, where GRUB reads them. sysupdate's kernel and initrd are in the UKI on the ESP. Both images carry the same packages otherwise.

## Answers to the #485 questions

### Automatic rollback under GRUB / legacy BIOS

**RAUC gives it, natively.** Its GRUB backend is a supported upstream path, and the spike proves it under SeaBIOS and OVMF from one `grub.cfg`. The integration cost is small and all of it is ours to keep:

- about 70 lines of `grub.cfg`, close to RAUC's reference script (`contrib/grub.conf`), with one change: when no slot is left to try, it boots the first good slot instead of stopping at a prompt nobody sees;
- a `system.conf` that names the two slots and the `grubenv` path;
- the kernel and initramfs inside each slot, so a slot is one self-contained unit;
- `rauc` and `rauc-service` from Debian (1.13), plus `grub-pc-bin` and `grub-efi-amd64-bin`, which the hosted image already uses for dual-firmware boot (#277).

**sysupdate does not.** Its rollback is systemd-boot's boot counting, and systemd-boot is UEFI-only. The alternatives are a GRUB boot counter built around sysupdate (ruled out) or UEFI-only hosted server types (priced below).

### The UEFI-only fallback: what it costs on Hetzner

The hosted image boots under both firmwares today because the provider's server type decides the firmware (`ENVIRONMENT.md` # Boot (hosted), #277): CX (Intel) presents legacy BIOS, CPX (AMD) presents UEFI. A UEFI-only engine means hosted boxes must move to CPX.

Hetzner Cloud prices from 15 June 2026, per month, excluding VAT, Germany and Finland:

| Shape | CX (legacy BIOS) | CPX (UEFI) | CPX costs |
|---|---|---|---|
| 2 vCPU, 4 GB | CX23 €5.49 (40 GB disk) | CPX22 €19.49 (80 GB disk) | 3.5× |
| next size up | CX33 €8.49 | CPX32 €35.49 | 4.2× |
| next size up | CX43 €15.99 | CPX42 €69.49 | 4.3× |

So UEFI-only adds about €14 per box per month at the smallest size. CPX is the better machine (AMD Genoa, twice the disk), but moose does not need that to run.

Availability: CX is sold only in the EU locations. In the US (Ashburn, Hillsboro) and Singapore Hetzner offers only CPX and CCX, so boxes there are UEFI already, and CPX there is dearer still (CPX21 is $31.99 in the US). Older CX22 and CPX21 types can no longer be created.

One thing to check: Hetzner says CX Gen3 (CX23 and up) runs on a mix of older Intel and AMD hosts from the old CX and CPX lines. The "CX boots BIOS" finding (#277) came from the older generation. A CX23 may get either firmware depending on its host. That does not change the answer, because the image has to boot under both either way, but it is a cheap check on a real box before anyone plans around it.

**Other providers.** Most big clouds can boot UEFI now: AWS (UEFI boot mode on its Nitro types), Google Cloud and Azure Generation 2 VMs. Some cheap tiers still boot legacy BIOS only, and Azure Generation 1 VMs are BIOS. RAUC with GRUB works on both, so it does not tie moose to a provider's firmware choice.

### Slot unit

**Whole root per slot, plus an `/etc` overlay on the data partition, plus a short pinned list.** The spike proves this shape on both engines. `/usr` per slot with a persistent root (the ParticleOS shape) was not tried. It needs every package to work with an `/etc` made from factory defaults, and Debian's packages assume conffiles in `/etc`.

The overlay is what makes whole-root work. It also sets three rules the design must write down:

1. **A file the box changes stops following the image.** That is right for users, passwords and host keys. It is wrong for a config file the image wants to change later. So the box should write as little as it can into `/etc`, and host-agent's own config should live under `/var/lib/moose`.
2. **A pinned file never updates.** `daemon.json` is pinned whole, for the remap. If a later image needs to change another key in `daemon.json`, that change needs its own migration step. Docker has no drop-in directory for `daemon.json`, so the pin cannot be narrowed to the one key.
   The spike pinned three files. The real list needs a fourth: **`/etc/login.defs`**, which carries `SUB_UID_COUNT 0` and `SUB_GID_COUNT 0` (#530). If a later image dropped those lines, `useradd` would start giving new users subordinate ID ranges from 100000 up, and after enough users they would run into the `moose-remap` range at 1000000. Pinning it whole has the same cost as `daemon.json`. The design may prefer to pin only those two keys, by having host-agent check them at boot.
3. **New system users come from `sysusers.d`, never from `useradd` at build time.** The spike shows the `useradd` user does not reach an existing box.

For #486 this is the first of its two options: the remap stays as the box was built, across every slot swap. Moving a box from off to on stays a separate design.

### Slot size and headroom

The hosted image today is an 8 GiB root that uses 1.1 GB (`CI / Cloud image` run 36694063081: "size is 8.5G, consumes 1.1G"). That includes the ESP's kernel and the baked brain and UI image tarballs. The spike's slot uses 600 to 680 MB.

**Suggested: two 4 GiB slots.** That is about 3.5 times today's use. The 8 GiB is only the image's starting size: on a hosted box `moose-grow-root` grows the root to the whole provider disk, and Docker's data lives on that root. With A/B the OS slot is capped at 4 GiB, and the rest of the disk goes to the data partition instead, where `/var/lib/docker`, `/var/lib/moose` and `/home` live. On a CX23's 40 GB disk that is about 31 GB of data partition. So the space apps can use stays about the same, but anything the box writes to the root outside those bind mounts is now capped at the slot and is lost at each swap. `/var/log` (the journal) and `/var/cache` are the obvious ones. The design must decide which other parts of `/var` move to the data partition. If the brain and UI tarballs move out of the slot (they are stream B, and can be pulled to `/var/lib/docker`), the headroom grows further. Slot size is fixed for the life of a box, because slot B is made at first boot right after slot A, so this number should be chosen with care and generously.

### Artifact and signing

- **RAUC** downloads one bundle: a squashfs holding the slot image, plus a CMS signature over it. The spike's bundle is 350 MB. The box checks it against an X.509 CA in its keyring (`/etc/rauc/keyring.pem`). The `verity` bundle format lets RAUC stream an install over HTTP(S) and, with its adaptive mode, fetch only the changed blocks. Neither of those was tried here.
- **sysupdate** downloads one file per partition (a compressed root image, 231 MB as xz) and a UKI (47 MB). It trusts them through a `SHA256SUMS` file with a detached GPG signature. The spike ran with `Verify=no`.

Either way, this is a third signing scheme. The appliance release manifest uses minisign (Ed25519, `RELEASE_MANIFEST.md`). RAUC adds an X.509 CA, and sysupdate adds a GPG key. Nothing signs release artifacts today (`NEXT.md`), so key custody has to be designed before either ships. On hosted, the update target already carries pinned digests (`UPDATES.md` # 8). It could carry the bundle's digest too, so the box checks the download against the channel it already trusts.

### Appliance fit: LUKS with TPM PCR 7, or dm-verity

Firmware is not a problem on the appliance: `FIRST_RUN.md` already requires UEFI. The question is encryption.

With this layout the OS slots hold **no per-box secrets**. Passwords, SSH host keys, `machine-id`, the LUKS recovery key (`/etc/moose/secrets/luks-recovery.key`) and the box's own state are on the data partition, through the `/etc` overlay and the bind mounts. The slots hold only what we publish. So the slots need integrity, not secrecy.

That data partition is on the OS drive. So the recovery key stays where `STORAGE.md` puts it, on the encrypted OS drive, now in its data partition instead of its root. User content on the appliance stays on the separate data drive(s), as today. One thing changes: `STORAGE.md` says the OS drive holds no irreplaceable state. That is already not true of the recovery key, and the `/etc` upper layer adds the box's users and host keys. The design should say how a replaced OS drive gets them back.

**Suggested shape: OS slots unencrypted, each with dm-verity, and the OS drive's data partition LUKS with TPM unseal against PCR 7.** PCR 7 is the Secure Boot policy, not the kernel, so an A/B swap does not break the unseal, the same as a kernel update today.

- **RAUC** fits either way. A slot's `device=` is any block device, so an opened LUKS mapping should work as a slot (not tried here), at the cost of a TPM enrollment per slot. For verity, the bundle can carry a verity hash tree beside the image. The initrd must then set up `veritysetup` for the booted slot. That part is ours to build and was not tried.
- **sysupdate** writes slots by raw offset into the GPT partition. It cannot target a LUKS mapping, so encrypted slots do not fit it. It fits verity well: ParticleOS's shape is a signed UKI with a verity root hash on its command line. That is also the path to PCR 11 sealing, which `NEXT.md` lists as a later upgrade.

The trade-off is Secure Boot. GRUB under Secure Boot needs shim and Debian's signed GRUB, and Debian's signed GRUB limits which modules and files it loads. The spike's `grub.cfg` uses `regexp`, `loadenv` and `test`, and reads the kernel from the slot's ext4. Whether that works under Debian's signed GRUB was not tested.

## Recommendation

**RAUC, with GRUB on both firmwares, whole-root slots, an `/etc` overlay with a pinned list on the data partition, and verity-protected slots on the appliance.**

Why:

- It is the only candidate with native automatic rollback under legacy BIOS. That keeps hosted on CX, which costs a quarter to a third of CPX (€5.49 against €19.49 at the smallest size).
- It is a Debian package. sysupdate is not: moose would build it from source, patch systemd's build to get a standalone binary, and own it through every systemd security update. The spike already hit one silent failure in the part Debian does ship (`systemd-import` and zstd).
- One boot path on every box. The hosted image already uses GRUB for BIOS, and the same `grub.cfg` serves UEFI.
- It is what Home Assistant OS and ZimaOS run in production.

What it costs, stated plainly:

- **An extra tool beside mkosi**, and an X.509 signing CA to stand up and guard.
- **A GRUB script we own.** It is short and close to RAUC's reference, but a bug in it can stop every box from booting.
- **No UKI.** The kernel and initramfs are loose files in the slot. The signed-UKI, verity root-hash and PCR 11 path that systemd-boot offers is harder to reach from GRUB. Secure Boot with Debian's signed GRUB is the first thing the design issue should prove.

**When to look again:** if Debian starts shipping `systemd-sysupdate`, and hosted moves to UEFI-only types (for example because CX Gen3 turns out to be UEFI too), sysupdate becomes the better fit, mostly for the UKI and Secure Boot story. The slot layout and the `/etc` rules above carry over to either engine unchanged.

## How it maps to the specs

- `UPDATES.md` # 1 and # 2: stream A is "one atomic unit" realized by `apt` today and by an A/B image later. This spike picks the engine for "later". It changes no spec.
- `BUILD.md` # 2 left the OTA orchestrator open on purpose ("naming the orchestrator waits for the A/B work"). This entry names one; the spec changes with the design issue.
- `BUILD.md` # User-namespace remap and #486: the remap is fixed for the life of a box. The pinned list is how an A/B update keeps that true.
- `ENVIRONMENT.md` # Boot (hosted): the dual-firmware requirement is why legacy BIOS was the deciding question.
- `STORAGE.md`: TPM unseal against PCR 7, and the recovery key on the encrypted drive. The suggested appliance shape keeps both, on the data partition.

## Known gaps & deviations

- **QEMU only.** No proof on a provisioned Hetzner box or a real appliance. #485 did not require it; the design issue carries that condition.
- **A hanging slot does not revert, on either engine.** Boot counting and RAUC's `TRY` flag both assume the bad slot reboots. The spike's broken v3 fails cleanly and reboots. A slot that stops in the initrd's emergency mode, or hangs anywhere, waits for ever (seen in run 36791598772). The real design needs emergency and rescue to reboot, `panic=` on the kernel command line, and a watchdog. Whether Hetzner VMs expose a watchdog device was not checked.
- **One attempt per update.** RAUC's `TRY` flag (and the spike's `TriesLeft=1` for sysupdate) gives a new slot one boot. A power cut during that boot reverts a good update. The box tries it again later, so this is safe, but it is a choice to confirm.
- **Not tried:** dm-verity slots, LUKS slots, Secure Boot, RAUC streaming and adaptive updates, sysupdate's GPG verification, and `/usr` per slot. Each is named in the answers above.
- **The early layout runs as a PID 1 wrapper**, not in the initrd. It proves the layout, not where it should live.
- **Install times were not captured.** The serial console lost `proof.sh`'s lines between the install and the reboot. The four-boot run times are in # Results.
- **Hetzner prices are from Hetzner's price adjustment page** (15 June 2026). The CX Gen3 hardware note is from Hetzner's Gen3 launch notes, not from a box.

## What's next

1. **The A/B design issue**, starting from RAUC with GRUB. It should first prove Secure Boot with Debian's signed GRUB and the `grub.cfg` above, and a verity slot set up in the initrd.
2. **Pick the slot size** (4 GiB suggested) before the first A/B image ships, since it is fixed for the life of a box.
3. **Write the `/etc` rules into the spec** (`BUILD.md` or a new A/B section of `UPDATES.md`): the overlay, the pinned list (with `login.defs`), `sysusers.d` for system users, and which parts of `/var` live on the data partition.
4. **Design artifact signing** together with the release-signing item in `NEXT.md`, so the box does not end up with three unrelated keys.
5. **Check a CX23's firmware** on a real box, once, to settle whether CX Gen3 is BIOS or mixed.
