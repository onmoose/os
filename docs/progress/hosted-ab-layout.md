# Hosted image in the A/B layout

- **Status:** done
- **Date:** 2026-10-01
- **Specs touched:** `docs/specs/BUILD.md`, `docs/specs/ENVIRONMENT.md`, `docs/specs/DECISIONS.md`, `docs/specs/NEXT.md`, `docs/specs/TESTING.md`, `docs/architecture.md`, `docs/dev/hosted-boot-proof.md`

Closes #561, the third slice of #486, after [os-package-lock.md](os-package-lock.md) and [docs-after-version-split.md](docs-after-version-split.md). It builds the hosted image in the A/B layout that [ab-os-update-design.md](ab-os-update-design.md) designed, with the state inventory (#486 step 3). No box updates its OS yet: this slice makes a box that can be updated. The bundle is #562 and the update itself #563.

## What was done

### The layout, and the maintainer's change to it

The design had two 4 GiB ext4 slots and a 512 MiB ESP: 9.1 GB, 23% of the smallest hosted box's 40 GB disk. The maintainer rejected that while this slice was in progress. The approved layout (`DECISIONS.md` 2026-10-01, the slot entry):

| # | Partition | Size | Share of 40 GB | Where it comes from |
|---|---|---|---|---|
| 1 | ESP (`esp`) | 128 MiB | 0.34% | the image |
| 2 | BIOS boot | 1 MiB | 0.003% | the image |
| 3 | slot A (`moose-slot-a`) | 1 GiB, read-only squashfs-xz | 2.68% | the image |
| 4 | slot B (`moose-slot-b`) | 1 GiB, empty | 2.68% | `systemd-repart` at first boot |
| 5 | state (`moose-state`) | the rest, ext4, mounted at `/state` | 94.3% | `systemd-repart` at first boot, grown every boot |

So the OS reserves 2177 MiB (2.28 GB), **5.7% of a 40 GB disk**. squashfs-xz is the one compressed format GRUB 2.12 reads, so the kernel and initramfs stay inside the slot. The maintainer considered and rejected dropping slot B and keeping slots as files.

### The image (`dev/cloud/`)

- **Partitions.** `mkosi.repart/` builds the first three. Slot A is `Format=squashfs`, `Compression=xz`, 1G, PARTUUID `20202020-...` as before. The runtime definitions in `mkosi.extra/usr/lib/repart.d/` match them and add slot B and the state partition. `systemd-repart.service` is masked: the initramfs runs repart on every boot, so the state partition still grows to fill the disk, and `systemd-growfs` grows its ext4.
- **The ESP holds no kernel.** The postinst moves the kernel from `/usr/lib/modules/<kver>/` into the slot's `/usr/lib/moose/boot/`. With `Bootable=auto`, mkosi then finds no kernel to copy to the ESP and writes no menu entries of its own (checked in mkosi v26's source: `install_kernel` and `gen_kernel_images`). The ESP holds GRUB's EFI binary, GRUB's modules for both firmwares, `grub.cfg` and `grubenv`. `mkosi.repart/00-esp.conf` copies only `/efi`.
- **The ESP's FAT.** systemd-repart always formats an ESP as FAT32, and for an image file it uses 4096-byte filesystem sectors. FAT32 needs at least 65525 clusters, which 4K sectors only reach at about 260 MB, so the 128 MiB ESP had no valid FAT32 and OVMF fell through to its shell (run 36925538434). One sector per cluster (`SYSTEMD_REPART_MKFS_OPTIONS_VFAT=-s1`) could not fix that (run 36927908164). `SectorSize=512` in `mkosi.conf` does: with 512-byte sectors FAT32 needs about 36 MB, the figure mkosi's own ESP sizing uses. The GPT already used 512-byte sectors.
- **GRUB on both firmwares**, from one `grub.cfg` and one `grubenv` on the ESP (RAUC's GRUB backend). systemd-boot is gone. `grub.cfg` loads `squash4` and `xzio` and boots the slot's own kernel and initramfs with `ro psi=1 panic=10 fsck.mode=skip rauc.slot=<A|B>`.
- **Early setup in the initramfs.** Debian's initramfs-tools, built in the postinst, with `squashfs`, `overlay` and `ext4` added to its modules. Its `local-bottom` hook runs `/usr/lib/moose/state-setup` chrooted into the slot: repart, `e2fsck -p`, mount `/state`, `systemd-growfs`, the `/etc` overlay, the four pinned files on first boot, and the bind mounts. Baked files under a bound directory are copied to the state partition once, at first boot. Any failure panics, and `panic=10` reboots.
- **Where data sits.** The databases stay on the local disk, each on a bind mount of its own: `/var/lib/moose` (the brain's SQLite, `state/instances`, `state/services`) and `/var/lib/docker`. `/home` is bound from `/state/srv/moose/home`, so user files sit under `/srv/moose` as on the appliance. A later pool at `/srv/moose` (for example with a Hetzner Volume) can then take in the users' files without moving any database. Nothing builds that pool.
- **The read-only slot.** `/var/lib/containerd` and `/srv/moose` are bind mounts, `/var/tmp` is a tmpfs from `/etc/fstab`, and `/opt/containerd/{bin,lib}` are made at build time. `ldconfig.service` and `systemd-update-done.service` are masked.
- **Users** come from `/usr/lib/sysusers.d/moose.conf`, not `useradd` at build time.
- **A slot that hangs.** `emergency.service` and `rescue.service` reboot after 10 s, `panic=10` reboots a failed initramfs or kernel, and `RuntimeWatchdogSec=60s` feeds a hardware watchdog where there is one.
- **RAUC.** `rauc` and `rauc-service` with `/etc/rauc/system.conf`: the two slots by PARTUUID as `type=raw`, the GRUB backend with the ESP's grubenv, and `data-directory=/var/lib/rauc` on the state partition. No keyring, so `rauc install` refuses every bundle until #562. Nothing marks a slot good yet (#563).
- **Packages.** `grub-efi-amd64-bin`, `rauc`, `rauc-service` and `e2fsprogs` in; `systemd-boot` out. RAUC pulls in about 25 libraries (curl, GnuTLS, GLib and their dependencies). The lean check has 191 names, and `dev/os-lock/cloud-packages.lock` is the resolved list from CI. `mksquashfs` comes from mkosi's own tools tree, which carries `squashfs-tools`, so nothing is added for it.

### The slot budget (`dev/cloud/slotbudget`)

A small Go tool that reads the built raw image: the GPT, then the squashfs superblock at the start of `moose-slot-a`. It fails the build when the squashfs fills more than 60% of the slot, and also when the slot is not a squashfs or not compressed with xz, since GRUB could not boot it. It prints the partition table with each partition's share of 40 GB, and writes it to the CI job summary. `dev/cloud/slot-budget.sh` runs it from both builds. The lean build (the image that ships) is gated. The boot-proof image only reports (`-max-percent 100`), because it bakes test-only images such as postgres:16. Its tests build synthetic GPT images, and one checks that the build-time and runtime repart files agree on every size. `BUILD.md` # 1b # Disk budget has the table.

### Host-agent

The hosted build reports the state partition as its one "System" volume, measured through `/var/lib/moose` (`diskusage.NewHosted`), not the 1 GiB slot at `/`.

### The boot lane

Every boot now runs under UEFI and again under legacy BIOS, each firmware on its own overlays; `bios` is no longer a boot name. Every boot first checks the layout (`cloud-assertions.sh` step 1b): 5 partitions with the sizes above, the state partition grown to the end of the 24G disk, `/` a read-only squashfs from slot A with root-owned files, the `/etc` overlay and every bind mount on the state partition (`/home` from `srv/moose/home`), the pinned files, SSH host keys and `machine-id` in the upper layer, `rauc status`, the grubenv and the reboot drop-ins. It prints a `layout: measured:` line. Every boot ends with a gate: no failed unit, and no host write that hit the read-only slot.

## How it was verified

All in CI, with every publish input false. Never built or booted locally.

Runs on `feat/561-hosted-ab-layout`, in order:

| Run | Boots | Result |
|---|---|---|
| 36925538434 | `unseeded` | red: UEFI found no filesystem on the 128 MiB ESP and fell through to its shell |
| 36927908164 | `unseeded` | red, the same: `-s1` cannot help with 4K sectors |
| 36930171023 | `unseeded` | red: GRUB booted the squashfs slot and the initramfs hook ran, then `systemd-repart` could not make a temporary file on the read-only slot. The box panicked and rebooted, as designed |
| 36931341902 | `unseeded` | green under UEFI and legacy BIOS |
| 36935891502 | `unseeded`, after the initramfs module check | green under UEFI and legacy BIOS |
| 36937301690 | the full list, the PR's first head (`fab2d8e`) | green, all 14 boots |
| 36940136412 | the full list, after the two review fixes | red at the first boot: the boot-disk lookup used `lsblk`, which needs the udev database the initramfs chroot does not have |
| **36941147838** | **the full list, on the final image head (`5e21891`), after the review fixes** | **green, all 14 boots: 7 under UEFI, 7 under legacy BIOS** |
| **36932849491** | the full list: `unseeded seeded access update ssh remap` (7 boots) | **green, all 14 boots: 7 under UEFI, 7 under legacy BIOS** (boot step 21 min) |

One check was added after run 36932849491 (the initramfs carries `sd_mod` and `virtio_scsi`), and run 36935891502 booted `unseeded` with it under both firmwares: green. After the review fixes (the GRUB fallback and the boot-disk lookup), **run 36941147838** booted the full list on head `5e21891` under both firmwares: all 14 boots green. Only this docs entry changed after it.

**Measured** (run 36932849491; `BUILD.md` # 1b # Disk budget):

- **Slot A on the image that ships:** squashfs 435,278,427 bytes (435.3 MB), **40.5% of the 1 GiB slot**. 209.0 MB to the 60% budget, 638.5 MB to full. The boot-proof image, with its test-only images, is 576,809,844 bytes, 53.7%.
- **The image:** 1.13 GiB (1,209,008,128 bytes), 3.02% of 40 GB.
- **ESP use on a booted box:** 18.9 MB of 128 MiB (18,940,416 bytes under UEFI, 18,928,640 under BIOS), 14%. That is more than the 4 to 7 MB estimate: GRUB's modules for two firmwares, its EFI binary and fonts.
- **State partition on boot:** on the 24G test disk the partition is 23,485,984,768 bytes and its ext4 22,937,014,272 bytes, so it reached the end of the disk and the filesystem was grown. 1.30 GB used after a first boot, 1.83 GB after the remap boots' five apps and a managed Postgres.
- **Reserved for the OS** (ESP, BIOS boot, two slots): 2177 MiB, 2.28 GB, **5.71% of a 40 GB disk**.

Also: `make check` green, with the new `dev/cloud/slotbudget` tests. The lean check (191 names) and the OS lock check passed on both builds in every run.

## How it maps to the specs

- `BUILD.md` # 1b: the partition layout, GRUB on both firmwares, the `/etc` overlay with four pinned files, `sysusers.d`, and the state partition, as designed, with the new slot size. The section now has a # Disk budget and an # As built part with the state inventory.
- `UPDATES.md` # 1: "a slot that hangs must still revert" is covered by the reboot drop-ins, `panic=10` and the watchdog setting. The update transaction is not built.
- `ENVIRONMENT.md` # Storage (hosted) and # Boot (hosted): the state partition replaces `moose-grow-root`, GRUB replaces systemd-boot for UEFI, and the two data rules (databases local, `/home` under `/srv/moose`).
- `DECISIONS.md` 2026-10-01: the new slot entry. It changes the slot size the design had, which was never a locked decision.

## Known gaps & deviations

- **Review fixes (Greptile, confirmed by the code review).** The GRUB fallback, used when no slot is left to try, now clears only the try flag of the slot it falls back to: the last good slot in `ORDER`, slot A today. A failed slot keeps `TRY=1`, so it is never booted again; **#563 must reset that slot's flags when it installs into it.** And `state-setup` now resolves the boot disk from the device behind `/` and runs repart and the `moose-state` lookup on that disk only, with no fallback to another disk. The boot lane checks both the disk it reported and that the state partition is on it.
- **No OS update yet.** No keyring (`rauc install` refuses every bundle), no bundle (#562), and nothing marks a slot good (#563). Until then GRUB sets `A_TRY=1` on one boot and its fallback resets it on the next, so it writes the grubenv on every boot and always boots slot A.
- **QEMU only.** No boot on a provisioned Hetzner box. The QEMU lane boots a virtio-blk disk, Hetzner a virtio-SCSI one; the lane checks that the initramfs carries `sd_mod` and `virtio_scsi`, but does not boot that path.
- **Two-repo seam: the private smoke test needs a disk of 20 GB or more.** The state partition takes the disk less 2.28 GB, and host-agent's hosted build now reports the state partition as its one "System" volume. This cannot be checked from this repo.
- **The control plane sits on a new box three times:** compressed in the slot, as tarballs copied to the state partition at first boot (383 MB, 0.96% of 40 GB), and as images in Docker. The copy follows the approved copy-once rule. Pointing the loader and host-agent at the slot's own copy would save the 383 MB; it touches the appliance lane's shared loader, so it is not in this slice.
- **Copy once hides a newer slot's baked files.** After the first boot the state copy of `/var/lib/moose` and `/var/lib/systemd` is the one in use. That is right for the control plane (stream B owns it), and #566 must keep it in mind.
- **The ESP needs 512-byte logical sectors.** Hosted VM disks have them. A disk with 4096-byte logical sectors could not read this ESP; that is for the appliance (#564) to decide.
- **mkosi still builds its own default initrd**, which nothing uses, because the image lists a kernel package. It costs build time only.
- **A hang in a running system** that neither panics nor reaches emergency mode is covered only by `RuntimeWatchdogSec`, and the QEMU lane has no watchdog device. Whether Hetzner VMs have one is still open (`NEXT.md` # A/B OS image, point 3).
- **The read-only gate reads only this boot's journal**, and the `frozen` boot is still outside the gate, as before.
- **Not in this slice, by the maintainer's call:** any Hetzner Volume or pool at `/srv/moose`. The layout only keeps the way open.

## What's next

1. #562: build, sign and publish a RAUC bundle per OS release. It needs the keyring this slice leaves out.
2. #563: host-agent applies OS updates, marks a slot good, and reverts.
3. #564: the appliance image in the A/B layout.
