# moose Build & Boot Pipeline

> Working spec for how moose ships — from source to a USB stick to a running box. Companion to `SPEC.md`, `CONTROL_PLANE.md`, `FIRST_RUN.md`, `STORAGE.md`.

> **Environment profiles.** This doc describes building the `appliance` install ISO. The same `mkosi` builder also emits a lean **hosted** cloud VM image profile (no Avahi/Samba/NetworkManager/cryptsetup-TPM/mergerfs) paired with a build-tagged slim cloud `host-agent`. See `ENVIRONMENT.md` # How the profile is realized.

This doc is **draft / option-survey**. Most sections present alternatives with a recommendation; locked decisions are called out explicitly. The intent is to surface forks before committing.

## What this doc covers

- The Debian base — release, kernel, what's preinstalled.
- ISO composition — tooling, layout, online vs. offline.
- The installer — what runs between USB-boot and reboot-to-disk.
- `host-agent` packaging and how it lands on disk.
- `moose-brain` image build, distribution, first-boot pull.
- How third-party build inputs are pinned and how a pin gets bumped.
- Versioning and release artifacts.

What it does **not** cover: update mechanics post-install (separate doc), CI/CD specifics, signing infrastructure (deferred until we have a release to sign).

---

## 1. Debian base

### Release

- **Debian 13 "Trixie" (stable).** Current as of 2026, fresh enough kernel/userland for modern hardware.
- Tracking testing or unstable would buy newer packages at the cost of stability we cannot afford for a non-technical-user appliance.

**Locked: Debian stable.** Re-pin when the next stable cuts.

### Kernel

Two real options:

- **Stock stable kernel.** Whatever ships in Trixie. Conservative, well-tested, but a 2026 stable kernel will already be a year+ behind on hardware support — bad for BYO x86 where the user's NIC / Wi-Fi / GPU may be newer than the kernel knows about.
- **`linux-image-*-bpo` (backports kernel).** Newer kernel, same Debian packaging discipline. Standard answer for "I want broad hardware support on stable." Used by ProxmoxVE, many appliances.

**Recommendation: backports kernel.** BYO hardware is a stated pillar (`SPEC.md`); shipping a kernel that doesn't recognize last year's Wi-Fi chips defeats it. Cost is a slightly larger update surface — acceptable.

### Kernel cmdline

The installed GRUB config must set these kernel parameters (`GRUB_CMDLINE_LINUX`):

- **`psi=1`** — enables the Pressure Stall Information accounting (`/proc/pressure/*`) that the `ram-pressure` health detector (`HEALTH.md` # Detector catalog) reads. Debian builds the kernel with `CONFIG_PSI=y` but `CONFIG_PSI_DEFAULT_DISABLED=y`, so PSI returns no useful data at runtime unless `psi=1` is on the cmdline. Without it the detector silently reads zeros and never fires — a false all-clear. Cost is negligible (a few per-cgroup counters).

### Firmware

- Include `firmware-linux`, `firmware-iwlwifi`, `firmware-realtek`, `firmware-amd-graphics`, `firmware-misc-nonfree` and similar. Non-free firmware is now in Debian's official installer by default (since Bookworm); we follow suit. Without this, half of laptops won't have working Wi-Fi at first boot.

### Preinstalled packages

Minimum to be a moose box:

- `systemd`, `systemd-cryptenroll`, `cryptsetup` — boot, encryption, TPM auto-unlock (`STORAGE.md`).
- `docker-ce` (or `docker.io` from Debian; see below) — runtime for everything.
- `avahi-daemon` — mDNS publishing for `*.local` app hostnames and SMB service discovery (`_smb._tcp`).
- `caddy` — only if we ship it on host; if it runs as a container under the brain (per `CONTROL_PLANE.md`), skip on host.
- `moose-host-agent` — baked into the OS image (# 4).
- `openssh-server` — SSH daemon, scoped to LAN + mesh via nftables (see "SSH" below).
- `samba` — SMB file shares for cross-device access (`STORAGE.md` # Cross-device access).
- `mergerfs` — userspace union for data drives (`STORAGE.md` # Data drives). Activates whenever a data drive is present.
- `nftables` — firewall, scoping SSH and SMB to LAN + mesh.
- Standard base utilities (`curl`, `ca-certificates`, `tpm2-tools`, `lvm2`, `e2fsprogs`, `cryptsetup-initramfs`).

**Open: `docker-ce` (upstream Docker repo) vs. `docker.io` (Debian-packaged).** Upstream is fresher and what the Docker docs assume; Debian's package lags but integrates more cleanly with apt security updates. Lean toward `docker-ce` from Docker's own apt repo — most of our app authors test against upstream Docker.

### SSH

`openssh-server` is **installed but not enabled at boot** — sshd does not run and :22 is closed on a fresh box. **Both** images carry it now (#467): the appliance always did, and the hosted cut that once forbade the package was reversed. It is started when the first account enables SSH from Settings and stopped when the last one disables it, so a box nobody uses SSH on presents no port at all (`AUTH.md` # Device access; `DECISIONS.md` 2026-09-09).

Stopping the unit is an ordinary event here, not an administrator shutting a service down, and Debian's `ssh.service` is not written for that: it declares `RuntimeDirectory=sshd`, so systemd deletes `/run/sshd` on stop, and sshd then refuses to read any config ("Missing privilege separation directory"). Since host-agent validates every render with `sshd -t` before starting anything, the first disable would otherwise make SSH un-re-enableable until reboot. Both images carry a `RuntimeDirectoryPreserve=yes` drop-in. The first enable after a boot works either way — `/run/sshd` is created at boot by openssh's own tmpfiles rule — so only the second enable of a boot shows the problem, which is why the cloud lane's `ssh` boot enables, disables, and enables again.

Debian's `openssh-server` postinst enables `ssh.service` on install, so each image undoes that at build time — the hosted one in `dev/cloud/mkosi.postinst.chroot`, which drops the `multi-user.target.wants` link and adds a preset so a later `preset-all` cannot put it back. Debian trixie does not enable `ssh.socket`, so the service unit is the whole of the run state. The build also **deletes the host keys** the postinst generates, so boxes from one image do not share them, and ships `moose-sshd-keygen.service` to make per-box keys **at boot** — Debian's own `sshd-keygen.service` is `ConditionFirstBoot=yes` and pulled in only by `ssh.service`, both of which are wrong for a daemon that first starts weeks later. At boot rather than with the daemon, because host-agent validates the rendered config with `sshd -t` *before* it starts the unit, and that exits "no hostkeys available" when there are none. Keys on disk open nothing; the port is still the daemon's run state. The cloud lane's `ssh` boot watches :22 through the whole cycle on a booted box — closed at boot, open after the toggle, closed again after it is turned off (`dev/cloud/run-cloud-tests.sh`).

Why daemon-follows-the-enabled-set instead of daemon-on-with-an-empty-allowlist:

- **A closed port beats a port that answers and refuses.** The old posture left :22 open for the life of every box so that sshd could reject at auth-name resolution. That defends a weaker position rather than arguing against a stronger one, and it costs a permanently visible service on a machine most owners will never SSH into.
- **On hosted the daemon *is* the port control.** That profile runs no moose firewall and its provider attaches none, so nothing else can close :22 (`ENVIRONMENT.md` # Access & files).
- **The toggle is no less simple.** The user still flips one switch in Settings; the brain calls host-agent, which renders the config and starts or stops the unit. A `systemctl` call is no more visible to the user than a config edit was.
- `PermitRootLogin no`. `PasswordAuthentication yes` globally, because it is a prerequisite for the password half of any account's `AuthenticationMethods` — **it does not mean a password alone gets in.** These two live in a static drop-in of their own (`sshd_config.d/moose-hardening.conf`), separate from the generated file, so they hold while no account is enabled and nothing has been rendered. Per-account method policy is a `Match User` block, so an account whose mandatory factor is the key is `publickey`, and one that added the optional second lock is `publickey,password`.

The drop-in at `sshd_config.d/moose-allowed.conf` is **rendered whole from the enabled set**, never line-edited: a global `AllowUsers` plus one `Match User` block per enabled account. Each block names an `AuthorizedKeysFile` pair — moose's root-owned `/etc/ssh/moose-authorized-keys/<user>` first, the user's own `.ssh/authorized_keys` second (`AUTH.md` # Device access explains why moose's keys stay out of the home directory). host-agent validates the candidate on its own **before** installing it and the combined config **after**, restoring the previous file if the combined check fails, so a render sshd rejects never survives to break the next start.

**Network scope: LAN + mesh only, structurally.** An nftables rule on :22 default-denies and allows only:

- RFC1918 source ranges: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16` (the LAN), and their IPv6 counterparts `fe80::/10` (link-local) and `fc00::/7` (unique-local). Both families have to be spelled out: in an `inet` table an `ip saddr` match compiles to an nfproto==IPv4 test before the address compare, so an IPv4-only rule silently drops every IPv6 client — and a LAN client resolving the box over mDNS commonly gets an AAAA record. A globally-routable IPv6 address is **not** allowed even from the same LAN, because it is reachable from the public internet, which is the thing this rule exists to close. The dual-stack LAN whose peers only carry GUAs is an open item (`NEXT.md` # SSH scoping on a dual-stack LAN); link-local always exists on a LAN interface, so the path stays open meanwhile.
- The mesh interface (`tailscale0` / `headscale0`) when present — devices the user has paired via `MOOSE_NETWORK.md`.

**A container is not a LAN device.** Docker's default address pool is carved out of `172.17.0.0/16` upward, which sits inside the accepted `172.16.0.0/12`, so the private ranges alone would hand every app container the household's own reach to `:22` and put a compromised app in front of the box's authentication surface (`THREAT_MODEL.md`, adversary: compromised app at runtime). Traffic arriving on a Docker-managed bridge (`docker0`, or `br-<hex>` for a user-defined network) is dropped **before** the accepts. Matched by input interface, not by subnet: the interface is exact, while excluding `172.17.0.0/16` by address would lock out a household that genuinely numbers its LAN there. Same shape as the hosted metadata block, which separates container traffic from host traffic by the path it takes rather than the address it carries.

SSH from the public internet is **structurally blocked**, not relying on per-account opt-in alone. A port scan from outside sees a closed port, not a refused-auth banner. The path to "SSH to my box from outside" is "pair the device on the mesh" — same trust model the user already learns for the dashboard. Interface-agnostic by design (nftables on source IP, not `ListenAddress` on a NIC name), so changing NICs / adding Wi-Fi doesn't break it.

Implemented as a drop-in at `/etc/nftables.d/moose-ssh.conf`, owned by the OS image (no `.deb` is planned any more, # 4), and loaded on every boot by `moose-ssh-firewall.service` — a standing policy, not something that follows the daemon, so there is no window in which the port is reachable from off the LAN. The rule matches on source IP rather than a NIC name, so changing NICs or adding Wi-Fi does not break it, and `iif lo` keeps a client on the box itself working. Mesh interface name is templated at host-agent startup based on which mesh client is installed; **v1 installs no mesh client**, so the LAN half is the whole rule today and the interface clause lands with the mesh, not before. Today the file, its loader unit and the hardening drop-in are checked in under `dev/test-qemu/appliance/` and staged into the appliance image by that lane's `bootstrap.sh`. This scoping is **appliance-only and stays**: it is complementary to the daemon lifecycle above, not replaced by it. The daemon decides whether :22 answers at all; nftables decides who may reach it when it does. Hosted has neither a LAN nor a mesh to scope to, so there the daemon is the only control.

### User-namespace remap

Both images, hosted and appliance, configure Docker with a **daemon-wide `userns-remap`** (`APP_ISOLATION.md` # User-namespace tiers, `DECISIONS.md` 2026-09-29). Built in both images (#530, `../progress/userns-remap-on.md`), after it was proved on the hosted image (`../progress/userns-remap-ci-proofs.md`). It is image configuration, the same on every box, so it needs no per-box setting and the seed is not involved (`ENVIRONMENT.md` # Provisioning & first-boot). The build does four things:

- **`/etc/docker/daemon.json`** gains `"userns-remap": "moose-remap"` next to the journald log driver.
- **A `moose-remap` system account** (no home, `nologin` shell). Docker reads its range from `/etc/subuid` and `/etc/subgid`.
- **One range, `moose-remap:1000000:65536`**, in both `/etc/subuid` and `/etc/subgid`. Host ids 1000000 to 1065535 sit far above every moose band (the fixed identities 2000 and 2001, the app-service band from 2100, users from 3000). The build writes it before any user exists.
- **`SUB_UID_COUNT 0` and `SUB_GID_COUNT 0` in `/etc/login.defs`.** On Debian trixie `useradd` otherwise gives every new user a range of its own (`165536:65536` on a test box). A moose user has no use for one, and a large user count could climb toward the remap range, where `newuidmap` would let that user reach remapped files. With the count at 0, host-agent's `useradd` gives no range to anyone.

**With the remap on, Docker turns its containerd image store off** and uses the classic **`overlay2`** store, with its data under `/var/lib/docker/1000000.1000000`. Docker 29 does this by itself (moby#47377). The first-boot `docker load` of the baked control-plane images, the app pulls, and the control-plane update and revert all work on it. No buildkit build ran under the remap; the box builds no images today (admission refuses `build:`).

**The remap is fixed for the life of a box.** Turning it on moves Docker to a new data root, so every image and container made before is out of sight, and it changes who owns every app's data. So it is set when the image is built and never flipped on a running box. An existing box is not migrated. **Nothing replaces `/etc/docker/daemon.json` on a box today**: the image ships it, apt does not own it, and a box never updates its OS, because the image-based OS update is not built. So new boxes are built with the remap on, and boxes built before #530 keep it off. Both are safe, and the brain reads which one it is on (`APP_ISOLATION.md` # User-namespace tiers). **The A/B image keeps it that way:** `daemon.json`, `/etc/subuid`, `/etc/subgid` and `/etc/login.defs` are pinned at first boot and never follow a new image (# 1b, #486). Moving a box from off to on stays a separate design, as its own step that re-owns app data and recreates containers.

**Turning it on was the last step of the rollout.** The brain and host-agent changes landed first and are inert on a daemon without the remap (the brain reads the daemon, `APP_ISOLATION.md` # User-namespace tiers; `--userns=host` is a no-op there, `CONTROL_PLANE.md` # Locked: control-plane container hardening). The `daemon.json` change came after them, in both images at once (#530). The hosted image sets it in `dev/cloud/mkosi.extra/etc/docker/daemon.json` and `dev/cloud/mkosi.postinst.chroot`, and the appliance test image in `dev/test-qemu/bootstrap.sh` and `dev/test-qemu/mkosi.postinst.chroot`. Every boot of both QEMU lanes checks it on the booted box: `docker info` lists `name=userns` and the `overlay2` store, and the socket proxy and the brain run in the host user namespace while Caddy and `moose-ui` are remapped.

### What we deliberately do not preinstall

- Desktop environment, X/Wayland session manager (except as needed for the installer — see #3).
- Anything from `tasksel`'s "standard" set beyond what we explicitly list.

---

## 1b. The A/B OS image *(stream A)*

The box's OS is one image written into one of two slots (`UPDATES.md` # 1, `DECISIONS.md` 2026-10-01). This section is the image's shape: what is on the disk, how it boots, and where per-box state lives. The spike in #485 proved the shape on both firmwares (`../progress/ab-update-engine-spike.md`).

> **Status: the hosted image is built in this layout (#561) and updates itself (#563).** # As built (#561), for the hosted image below says what it does and lists the state inventory. The signed bundle is built too (#562, # The bundle), and `host-agent` installs it, switches, marks good and reverts (#563, `UPDATES.md` # 1 # As built). The appliance image (`dev/test-qemu/`) still builds a single root until #564. The OS package lock for the hosted image is built too (# The OS package lock, #560).

### Partition layout

The same on both profiles. The image carries the first three; first boot makes the rest with `systemd-repart`.

| # | Partition | Size | Holds |
|---|---|---|---|
| 1 | ESP | 128M | GRUB for UEFI, GRUB's modules for both firmwares, `grub.cfg` and the `grubenv` that holds the slot order. No kernel |
| 2 | BIOS boot | 1M | GRUB's core image for legacy BIOS (#277) |
| 3 | slot A | 1G | one whole root as a read-only squashfs (xz), kernel and initramfs included |
| 4 | slot B | 1G | made empty at first boot; the first update fills it |
| 5 | state | the rest | per-box state (below); made at first boot and grown on every boot when the disk grew |

On hosted, the state partition took over the job of `moose-grow-root` (`ENVIRONMENT.md` # Storage (hosted), #561): the root no longer grows, the state partition does. On the appliance this is the **OS drive's** layout. The data drive(s) and their mergerfs pool are unchanged (`STORAGE.md`).

**Slot size: 1 GiB of read-only squashfs-xz, fixed for the life of the box** (`DECISIONS.md` 2026-10-01). Slot B is made right after slot A at first boot, so a slot can never grow later without moving the state partition. A slot is a squashfs compressed with xz because GRUB 2.12 must read the kernel and initramfs from inside it, and that is the one compressed format GRUB reads: it has no erofs driver and no zstd decoder for squashfs. The box never writes a slot, so read-only costs nothing. **The build fails when the squashfs in slot A fills more than 60% of the slot** (# Disk budget).

### Disk budget

Every byte the OS reserves is taken from every box for life, so the budget is measured on the smallest hosted box: a Hetzner CX23 with a **40 GB** disk (40 x 10^9 bytes). Measured in `CI / Cloud image` run 36932849491: the slot on the image that ships, the ESP and the state partition on a booted box (the same under UEFI and legacy BIOS):

| # | Partition | Size | Share of 40 GB | Measured content | Headroom |
|---|---|---|---|---|---|
| 1 | ESP | 128 MiB (134.2 MB) | 0.34% | 18.9 MB used, 14% of the ESP: GRUB's EFI binary, its modules for both firmwares, fonts, `grub.cfg`, `grubenv` | 115.3 MB |
| 2 | BIOS boot | 1 MiB | 0.003% | GRUB's core image | none needed |
| 3 | slot A | 1 GiB (1.074 GB) | 2.68% | squashfs 435.3 MB, 40.5% of the slot | 209.0 MB to the 60% budget, 638.5 MB to full |
| 4 | slot B | 1 GiB (1.074 GB) | 2.68% | empty until the first OS update | the same budget as slot A |
| | **reserved for the OS** | **2177 MiB (2.283 GB)** | **5.71%** | | |
| 5 | state | the rest, about 37.7 GB | 94.3% | 1.30 GB used after a first boot of the boot-proof image (below) | the rest of the disk |

- **What is in the slot.** Uncompressed, the root is 1.13 GB: the control-plane image tarballs (383 MB), the kernel and initramfs (46.5 MB), and the rest of the OS (about 658 MB). Compressed it is 435 MB, and 314 MB without the tarballs (run 36909222361, a size probe). The tarballs stay baked in the slot, so a new box needs no registry at first boot (# 5).
- **The state partition after a first boot** holds 1.30 GB in the boot lane (run 36932849491, a 24G disk), and 1.83 GB after the remap boots installed five apps and a managed Postgres. That is the boot-proof image, which also bakes test-only images, so a shipped box uses somewhat less. It is mostly the control plane: the copied tarballs and the images loaded into Docker.
- **What the slot's content costs on the state partition.** Two baked trees are copied to the state partition once, at first boot (# As built, copy once): the tarballs under `/var/lib/moose/control-plane-images/` (383 MB, 0.96% of 40 GB) and the systemd state under `/var/lib/systemd`. The tarballs are then also loaded into Docker. So a new box holds the control plane three times: compressed in the slot, as tarballs on the state partition, and as images in Docker.
- **The budget is checked on every build.** `dev/cloud/slotbudget` reads the squashfs superblock in slot A of the built image and fails `dev/cloud/bootstrap.sh` when it is over 60% of the slot. It writes the table above (the partitions in the image) to the CI job summary. The boot-proof image (`dev/cloud/test/`) also bakes test-only images (postgres:16, a registry) and fills 53.7% of its slot; it is reported, not gated, because it never ships.
- **The ESP holds no kernel.** mkosi would copy every kernel it finds to the ESP, with its own initrd, and that is why it sizes an ESP at 512 MiB to 1 GiB by default. The image's kernel lives only in the slot, so 128 MiB is plenty.

### Boot: GRUB on both firmwares

GRUB boots the box under UEFI (`x86_64-efi`) and under legacy BIOS (`i386-pc`), from **one** `grub.cfg` and **one** `grubenv` on the ESP. This replaces the hosted image's split of systemd-boot for UEFI and GRUB for BIOS.

- The slot choice is RAUC's GRUB backend: `ORDER`, `<slot>_OK` and `<slot>_TRY` in `grubenv`. GRUB boots the first slot in `ORDER` that is good and not yet tried, and sets its `TRY` before booting it. `rauc status mark-good` clears it. A slot that never gets there is skipped on the next boot. **One attempt per update.**
- When no slot is left to try, GRUB falls back to the **known-good slot** rather than stopping at a prompt nobody will see: the last good slot in `ORDER`, because an install puts the new slot first and the slot it came from last. **Only the fallback slot's `TRY` is cleared.** A slot that failed keeps `TRY=1` and is never booted again until a new install into it resets its flag. As built (#563), the old slot's `host-agent` also marks a failed slot bad (`OK=0`), and the switch to a newly installed slot (`rauc status mark-active other`) sets its `OK=1 TRY=0`.
- The kernel and initramfs live **inside** each slot, where GRUB reads them from the slot's squashfs (`squash4` and `xzio`). A slot is one self-contained unit: the kernel always matches the root it boots.
- A slot that hangs instead of rebooting would never revert, so the image reboots on emergency and rescue, sets `panic=`, and runs a watchdog where the machine has one (`UPDATES.md` # 1).
- **Appliance, Secure Boot:** shim plus Debian's signed GRUB, which limits the modules and files GRUB may load. That this `grub.cfg` works under it is not proved yet (`NEXT.md` # A/B OS image).

### What a slot holds, and what the state partition holds

**A slot holds a whole root and is never written by the box.** It holds only what we publish, so it can be checked but needs no secrecy.

**Everything the box writes that must outlive a slot swap lives on the state partition**, set up early in boot (in the initrd), before systemd reads `/etc`:

- **`/etc` is an overlay.** The slot's `/etc` is the lower layer and `/etc` on the state partition is the upper layer. Users, groups, password hashes, SSH host keys, `machine-id`, the rendered sshd drop-ins and `/etc/moose/secrets/` all land in the upper layer and survive the swap. A file the box never touched still comes from the new slot, so image defaults keep updating.
- **Bind mounts from the state partition:** `/var/lib/docker`, `/var/lib/containerd`, `/var/lib/moose` (brain SQLite, `images.json`, the staged compose, the seed and its materialized state), `/srv/moose`, `/home` (from `srv/moose/home` on the state partition, so user files sit under `/srv/moose` as on the appliance), `/var/log` (so the journal of a slot that failed is still there to read), `/var/lib/systemd`, `/var/lib/sudo` and RAUC's own data directory. Caddy's cert store is a Docker volume, so it is under `/var/lib/docker`. **The databases always stay on the local disk**, each on a bind mount of its own (`/var/lib/moose`, `/var/lib/docker`), and are never under `/srv/moose` (`ENVIRONMENT.md` # Storage (hosted)). The full inventory of what a running box writes is in # As built (#561) below; anything else gets a regeneration rule there.
- **Image-owned:** `/usr` and the rest of the root, including `/var/lib/dpkg`, which must describe the slot it is in.

Three rules follow from the overlay, and every image change must respect them:

1. **A file the box changes stops following the image.** That is right for users and host keys, and wrong for a config file a later image wants to change. So the box writes as little as it can into `/etc`. `host-agent`'s own settings live under `/var/lib/moose`.
2. **A pinned file never updates.** On first boot four files are copied up on purpose and never follow the image again: `/etc/docker/daemon.json`, `/etc/subuid`, `/etc/subgid` and `/etc/login.defs`. This is what keeps a box's userns-remap fixed for life (# User-namespace remap). A later change to one of them needs its own migration step, because Docker has no drop-in directory for `daemon.json`.
3. **System users come from `sysusers.d`, never from `useradd` at build time.** The box's `/etc/passwd` is already in the upper layer, so a user a new image adds with `useradd` is hidden. The spike proved both halves.

**Appliance encryption.** The slots are not encrypted: they hold no per-box secret. Each slot gets dm-verity, set up in the initrd for the booted slot, so a changed byte fails to read. The state partition is LUKS with TPM unseal against PCR 7, as the OS root is today (`STORAGE.md` # Encryption posture). PCR 7 is the Secure Boot policy, not the kernel, so a slot swap does not break the unseal. Hosted keeps its custodian model (`ENVIRONMENT.md` # Storage (hosted)).

### As built (#561), for the hosted image

`dev/cloud/` builds this layout. Since #563 `host-agent` installs a bundle into the other slot, switches in the window and marks a slot good (`UPDATES.md` # 1 # As built); the rest of this list is as #561 built it.

- **Partitions.** The image holds the ESP (128M, label `esp`), the BIOS boot partition (1M) and slot A (1G, label `moose-slot-a`, PARTUUID `20202020-2020-4020-8020-202020202020`, the UUID the single root had before) (`dev/cloud/mkosi.repart/`), about 1.13 GiB in all. Slot A is `Format=squashfs` with `Compression=xz`. The image is built with 512-byte sectors (`SectorSize=512` in `mkosi.conf`) so the 128 MiB ESP can be FAT32: systemd-repart always formats an ESP as FAT32, and with its default 4096-byte filesystem sectors FAT32 needs about 260 MB, so at 128 MiB UEFI firmware saw no filesystem on it. With 512-byte sectors it needs about 36 MB. Hosted VM disks use 512-byte logical sectors; a disk with 4096-byte logical sectors could not read this ESP, which matters for the appliance (#564), not for hosted. The runtime definitions in `/usr/lib/repart.d/` add slot B (1G, empty, label `moose-slot-b`, PARTUUID `21212121-2121-4121-8121-212121212121`) and the state partition (label `moose-state`, ext4, the rest of the disk) at first boot. The stock `systemd-repart.service` is masked, so the initramfs is the one place repart runs. A test in `dev/cloud/slotbudget` checks that the build-time and runtime definitions agree on every size.
- **Boot.** GRUB for both firmwares (`Bootloader=grub`, `BiosBootloader=grub`), from `/grub/grub.cfg` and `/grub/grubenv` on the ESP. The first grubenv is `ORDER="A B" A_OK=1 A_TRY=0 B_OK=0 B_TRY=0`. Each menu entry loads `squash4` and `xzio` and boots `/usr/lib/moose/boot/vmlinuz` and `initrd.img` from its slot with `ro console=tty0 console=ttyS0 psi=1 panic=10 fsck.mode=skip rauc.slot=<A|B> root=PARTUUID=<slot>`. `fsck.mode=skip` is there because the slot has nothing to check, and state-setup checks the state partition itself. Since #563 `host-agent` marks the booted slot good at every start (a trial boot only once the brain is healthy), so `TRY` is back to 0 on every boot that came up. **The ESP holds no kernel.** The postinst moves the kernel from `/usr/lib/modules/<kver>/` into `/usr/lib/moose/boot/`, so mkosi (with `Bootable=auto`) finds no kernel to copy to the ESP and adds no menu entries; `grub.cfg` is the whole menu. The ESP holds GRUB's EFI binary, its modules for both firmwares, `grub.cfg` and `grubenv`.
- **Early setup, in the initramfs.** The slot's initramfs is Debian's initramfs-tools (`linux-image-amd64` already depends on it), built by `mkinitramfs` in `mkosi.postinst.chroot`. `squashfs`, `overlay` and `ext4` are added to its module list. Its `local-bottom` hook (`/etc/initramfs-tools/scripts/local-bottom/moose-state`) runs after the slot is mounted read-only and before systemd: it mounts `/proc`, `/sys`, `/dev` and a `/run` tmpfs into the slot and runs `/usr/lib/moose/state-setup` chrooted into it, so the tools are the slot's own. The script finds the boot disk (the parent disk of the device behind `/`, through sysfs), runs `systemd-repart` on that disk, and looks for the `moose-state` partition on that disk only: a partition with the label on any other disk is never touched, and no match on the boot disk is fatal. It then checks the state partition with `e2fsck -p`, mounts it at `/state`, grows its ext4 with `systemd-growfs`, mounts the `/etc` overlay (`upperdir=/state/etc/upper`), pins the four files on the first boot (marker `/state/etc/.moose-pinned`), and makes the bind mounts (`/home` from `srv/moose/home`). Any failure panics, and `panic=10` reboots, so a slot never comes up without its state.
- **Copy once.** One rule for every bind mount: when a state directory does not exist yet, the slot's directory is copied into it first (to a temporary name, then renamed), and then bound. So what the image baked under a bound path reaches a new box: the control-plane image tarballs, `control-plane/compose.yml` and `caddy.json` under `/var/lib/moose`, and the systemd state under `/var/lib/systemd`. The copy costs disk: the tarballs are 383 MB on the state partition (# Disk budget). **After the first boot the state copy is the one in use, and a newer slot's baked copy stays hidden.** That is right for the control plane: stream B owns it after first boot, and it updates through the control-plane update, not the OS. The work that bakes the last released control plane into the image (#566) must keep this rule in mind: a baked control plane only reaches boxes made from that image.
- **The slot is read-only** (a squashfs, `ro` on the command line, and no root line in `/etc/fstab`). The boot lane fails a boot that ends with a failed unit, or whose journal shows a host write that hit the read-only slot (`cloud-assertions.sh` # ok), so a write the inventory missed is found in CI, not on a box.
- **Users.** moose's own users and groups (`moose` 3000, `moose-app` 2000, `moose-shared` 2001, `moose-remap`) come from `/usr/lib/sysusers.d/moose.conf` (rule 3). Users the Debian packages make at build time stay in the slot's `/etc/passwd`, the overlay's lower layer.
- **A slot that hangs must still revert** (`UPDATES.md` # 1). `emergency.service` and `rescue.service` get a drop-in that reboots after 10 s, the initramfs reboots on a failure through `panic=10`, and `RuntimeWatchdogSec=60s` feeds a hardware watchdog where the machine has one (none in the QEMU lane).
- **RAUC.** `rauc` and `rauc-service` are installed with `/etc/rauc/system.conf`: the two slots by PARTUUID as `type=raw` (RAUC writes a squashfs image to the partition as it is), `bootloader=grub` with the ESP's grubenv, and `data-directory=/var/lib/rauc` on the state partition. Since #562 it also has a `[keyring]`: `/etc/rauc/keyring.pem` with `check-purpose=codesign` (# The bundle).

**The state inventory (#486 step 3).** Everything a running hosted box writes, and where it goes. It comes from a CI probe on the single-root image (run 36901760712: every boot, listing every path changed under `/` since boot) and was then proved on the read-only slot: every boot of run 36932849491, under both firmwares, ends with no failed unit and no host write that hit the slot.

| What the box writes | Home |
|---|---|
| Users, groups and password hashes (`/etc/passwd`, `group`, `shadow`, `gshadow` and their `-` backups), `machine-id`, the SSH host keys, `sshd_config.d/moose-allowed.conf`, `/etc/ssh/moose-authorized-keys/`, the `rc?.d` links `systemctl` writes when SSH is turned off, the time zone (`/etc/localtime`) | `/etc` overlay, upper layer on the state partition |
| `/etc/docker/daemon.json`, `/etc/subuid`, `/etc/subgid`, `/etc/login.defs` | pinned: copied into the upper layer on the first boot |
| `/home` (users' files) | bind mount from `srv/moose/home` on the state partition |
| `/var/lib/docker` (images, containers, volumes, Caddy's cert store in the `caddy_data` volume), `/var/lib/containerd` | bind mounts |
| `/var/lib/moose` (brain SQLite, `images.json`, the staged compose and its ledger, the seed and its materialized state, the catalog cache, the baked control-plane images) | bind mount, copied once from the slot |
| `/srv/moose` (the household shared tree host-agent makes on both profiles) | bind mount |
| `/var/log` (the journal when it is persistent, `wtmp`) | bind mount |
| `/var/lib/systemd` (timers, random seed, journal catalog, coredumps) | bind mount, copied once from the slot |
| `/var/lib/rauc` (RAUC's status) | bind mount |
| `/var/lib/sudo` (an admin's sudo lecture marks) | bind mount |
| `/var/tmp` | tmpfs from `/etc/fstab`; nothing in it must outlive a reboot |
| `/tmp`, `/run` | tmpfs, as before |
| `/var/lib/dbus/machine-id` | a link to `/etc/machine-id`, made at build time (dbus's tmpfiles rule wrote it at boot) |
| `/opt/containerd/bin`, `/opt/containerd/lib` | made empty at build time (containerd made them at start) |
| `/etc/ld.so.cache`, `/var/cache/ldconfig` | regeneration rule: none. `ldconfig.service` is masked, and the slot ships the cache that matches its libraries. A copy in the upper layer would outlive its slot and, after a revert, name the newer slot's libraries |
| `/etc/.updated`, `/var/.updated` | `systemd-update-done.service` is masked. The `ConditionNeedsUpdate=` jobs (`systemd-sysusers`, the journal catalog) then run on every boot, which an image that swaps under `/etc` needs anyway |
| `/root` | no home. Nothing on a hosted box writes it; an admin's root shell history is lost |

The boot lane's own writes moved with it: its SSH test key to `/run`, the remap boots' state to `/var/lib/moose/test/`, and the update boot's `host-agent` drop-in lands in the `/etc` upper layer like any box change.

### The bundle: what a box downloads

One **RAUC bundle** per OS release, in the `verity` format: the slot image in a squashfs plus a CMS signature over it. The box checks the signature against an X.509 CA baked into the image (`/etc/rauc/keyring.pem`), and on top of that checks the bundle's digest against the update target (`UPDATES.md` # 1, #563).

**As built (#562), for the hosted image.**

- **What is in it.** One image, `rootfs.img`: slot A of the image that ships, cut out of the GPT at the squashfs's own size rounded up to 4 KiB (`dev/cloud/slotbudget -extract`). So it is the same bytes as slot A in the published disk image. The manifest takes `compatible` from the slot's own `/etc/rauc/system.conf` (`moose-hosted-x86_64`), `version` from `VERSION`, `build` from the git commit, and has no hooks. No `adaptive=` and no streaming yet; those are for the installer (#563).
- **Size** (`CI / Cloud image` run 37010678846; the `.raw.xz` from the probe run 37007422220): the slot image is 435.2 MB, 40.5% of the 1 GiB slot. The bundle is 438.5 MB, 40.8% of the slot: the image, a 3.4 MB verity hash tree and the signature. With the disk image (`.raw.xz`, 435.8 MB) an OS release is about 874 MB to download, and the bundle is half of it. Each build writes the bundle's size to the job summary, and each publish the share of the release download.
- **How it is built.** `dev/cloud/build-bundle.sh`, in the build job of `ci-cloud-image.yml`, right after the image. RAUC runs in the pinned `debian:trixie-slim` (`BRAIN_RUNTIME_IMAGE` in `dev/control-plane/images.lock`), with `rauc` at the exact version of `dev/os-lock/cloud-packages.lock`, from `snapshot.debian.org` at the locked timestamp, so the build uses the same RAUC as the box. It adds about a minute (53 s in run 37010678846).
- **The keyring.** `/etc/rauc/keyring.pem` is staged into the generated wiring tree (`dev/cloud/rauc.sh`, `MOOSE_RAUC_KEYRING`). An image built to publish the OS line carries only the **release root CA**, `dev/release/rauc/release-ca.pem`. Every other build carries only a **throwaway root** made once per checkout under `.dev/rauc/throwaway/`. The key is never a file in the repo, and no image that ships trusts the throwaway. The boot lane checks that the keyring is there, that it comes from the slot and not the `/etc` upper layer (so a new image's keyring reaches the box), and that `system.conf` asks for `check-purpose=codesign`. Without that line RAUC wants the S/MIME purpose and refuses a code-signing cert.
- **Signing and its checks.** The build job always signs with the throwaway signer, and then checks the bundle against the `system.conf` and keyring read back out of the slot with `unsquashfs`, not against the copies in the repo: the slot carries the keyring the build staged; the bundle verifies against the throwaway root; with the throwaway keyring the image accepts it, and with the release keyring it must **refuse** it; and an unrelated CA (a wrong key) is refused. It also rehearses the `sign` job's re-signing with a throwaway "release" CA, so that script runs on every build. A **`sign` job** of its own, after every boot passed, alone re-signs the bundle with the release signer (`dev/release/sign-bundle.sh`, `rauc resign`; the payload stays the same bytes), but only a bundle whose sha256 matches the one the build job reported, and checks it against the image's own config and keyring. The publish job then attaches it, after checking the digest `sign` reported. So the signature vouches for what the build job produced; the box's check of the digest its update target names (#563) is the control for a bad build. It refuses when the signer secrets are empty, when the image keyring is not the committed release root, or when the image would accept the throwaway-signed bundle.
- **Key custody** (`DECISIONS.md` 2026-10-02): an offline root CA with the maintainer, and a signer it issued as the secrets `RAUC_SIGNING_CERT` and `RAUC_SIGNING_KEY` of the GitHub Environment `os-release` (only `main` and `v*` tags; only the `sign` job enters it). No CRLs. Making the root, issuing and rotating a signer and replacing the root are in `docs/dev/rauc-signing.md`. **Until `release-ca.pem` is committed, `release.yml` tags nothing for a merge that bumps `VERSION`** (on either line, even when the merge bumps `CONTROL_PLANE_VERSION` too), and a dispatch that publishes the OS fails at its first step. Until the secrets exist, the `sign` job refuses and nothing is published. The exact rule is in `docs/dev/contributing.md` # Release model.
- **Installed by `host-agent` since #563.** The `os-update` boot proves `rauc install` into slot B (RAUC's `raw` handler takes `rootfs.img`), the switch and the trial boot, under both firmwares; `os-revert` proves the revert (`docs/dev/hosted-boot-proof.md`). RAUC's install leaves the boot order alone (`activate-installed=false` in `system.conf`): `host-agent` switches with `rauc status mark-active other` only inside the window, and checks the grubenv reads `OK=1 TRY=0` for the new slot before it reboots (the stale try flag of #570).
- **The trial's safety net.** `moose-os-trial.timer` (`/etc/systemd/system/`, enabled in `timers.target`) runs `/usr/lib/moose/os-trial-check` 15 minutes after boot. If the booted slot still has its trial marker (`/var/lib/moose/os-update/trial-<slot>`) and the grubenv does not mark it good (`OK=1 TRY=0`), its `host-agent` never marked it good, so it marks the slot bad, leaves a note (`/var/lib/moose/os-update/safety-net-<slot>`, which the old slot's `host-agent` logs and removes) and reboots, and GRUB boots the other slot. The boot-proof image sets the timer to 90 s (`dev/cloud/test/bootstrap.sh`); the image that ships keeps 15 min.

### The OS package lock

With no `apt` on the box, every package version in the OS is chosen at build time, so the build pins them the way `package-lock.json` pins npm packages.

- **The lock is a Debian snapshot timestamp** in a checked-in file under `dev/os-lock/`. The image installs from `snapshot.debian.org` at that moment, `debian-security` included, so one commit always builds the same OS.
- **The resolved package list is committed next to it** (a normalized form of mkosi's JSON manifest), so a lock change shows up as a readable diff of package versions.
- **Third-party apt repos are not in the Debian snapshot.** `docker-ce` and its plugins come from `download.docker.com` (# Docker package source), and a Tier-2 package such as Tailscale comes from its vendor's repo. The lock names their exact versions, and the build installs those versions. Every curated Tier-2 package is in the image, with the run state its own spec gives it (`SERVICE_PROVISIONING.md` # Tier 2), because nothing can be installed on the host at runtime. A bump of any source goes through the same PR.
- **A scheduled CI job moves the timestamp forward.** When any package changed, it opens a PR that names the changes ("openssl 3.5.1-1 to 3.5.1-1+deb13u1"). CI builds the image and runs the boot proofs on it. **Merging it puts the fix on `dev`, not on any box.** It ships with the next OS release, cut like any release by bumping `VERSION` (# Versioning, `docs/dev/contributing.md` # Release model). A bump that changes a package from `trixie-security` is released within 7 days, as a hotfix from `main`; any other bump ships with the next normal release (`DECISIONS.md` 2026-10-01, the OS patch release entry).

**As built (#560), for the hosted image.** `snapshot.debian.org` is read only at build time; a box never talks to it.

- **Three files in `dev/os-lock/`.** `debian-snapshot` is one timestamp, such as `20261001T082322Z`. `third-party.lock` holds one `package=version` line for each of `docker-ce`, `docker-ce-cli`, `containerd.io` and `docker-compose-plugin`. `cloud-packages.lock` is the resolved list, one `name version architecture` line per package, made from mkosi's JSON manifest by `dev/os-lock/oslock` (sorted, with a header; never edited by hand).
- **How a build uses them.** `dev/os-lock/os-lock.sh` is shared by `dev/cloud/bootstrap.sh` (the lean image that ships) and `dev/cloud/test/bootstrap.sh` (the boot-proof image). It passes the timestamp to mkosi as `--snapshot`, so mkosi v26 installs the main archive and `debian-security` from `snapshot.debian.org/archive/{debian,debian-security}/<ts>`. mkosi leaves out `trixie-updates` in snapshot mode, so the helper adds it back from the same snapshot, and the image keeps the suites it had before the lock. Each Docker pin becomes an apt pin (`Pin-Priority: 1001`). `Acquire::Retries "5"` covers a slow mirror. mkosi already turns off apt's `Valid-Until` check, so an old timestamp still builds even though `trixie-security`'s Release file expires after 7 days.
- **The check.** After the build, each of the two images must resolve to exactly `cloud-packages.lock`, versions included, or the build fails and prints what moved. In CI that is two separate builds of one commit compared with one list. The resolved list is uploaded as the `cloud-packages-lock` artifact. A Docker pin whose version is gone from Docker's repo makes apt fall back to the newest version, and this check then fails, so it never ships quietly. The lean check (`dev/cloud/expected-packages.txt`, names only) stays: it is the reviewed set of packages, and the lock adds the versions.
- **No fallback.** When `snapshot.debian.org` or Docker's repo is down after the retries, the build fails. It never falls back to the live mirror, which would break the lock. A failed release is re-run (`docs/dev/contributing.md` # Release model). No copy of the `.deb` files is kept.
- **The bump** is `.github/workflows/os-lock-bump.yml`, daily. It takes the newest snapshot and the newest Docker version within each pin's current major (a new major is a manual edit), builds the lean image in record mode (`MOOSE_OS_LOCK_RECORD=1`), and does nothing when no package changed. Otherwise it commits the three files on `bot/os-lock` and names every change in the commit, the PR title and the PR body, marking the ones that come from `trixie-security`. The branch keeps human work: it is never deleted or force-pushed; the base is merged into it and a commit is added only when the lock files changed. The build and the push are separate jobs, and only the push job ever sees the bot token. A PR that touches `dev/os-lock/`, into `dev` or `hotfix/**`, runs the lock check and the full boot list in `CI / Cloud image`. How the PR gets opened, with or without the `OS_LOCK_BOT_TOKEN` secret, is in `docs/dev/contributing.md` # OS package lock bumps.
- **Not locked:**
  - The **appliance lane** (`dev/test-qemu/`, bookworm, local-only) is not locked yet. It gets the lock with its move to trixie and the A/B layout (#564).
  - The **mkosi tools tree** (the build tools mkosi runs, `ToolsTree=default`) still installs from the live trixie mirror: mkosi v26 does not pass `Snapshot=` to it. It decides the tools, not the packages in the image.
  - Docker's signing key is fetched at build time. The versions are pinned, not the key.

---

## 2. ISO build tooling

**Decided (2026-06-16): Option C — `mkosi`.** It is moose's single image builder, for the install ISO, the cloud VM image, and the QEMU test image alike. See "Decision" below; `DECISIONS.md` 2026-06-16 carries the delta. The four options below are kept as the record that produced the call.

The fork, as it stood — four real options:

### Option A — `live-build`

Debian's official meta-tool for building live + installer ISOs. Used by Kali, Tails, official Debian Live.

- **Pros:** Designed exactly for this. Handles squashfs, bootloader (GRUB/syslinux for BIOS+UEFI), hybrid ISO/USB, package selection, hooks for customization. Mature, well-documented, Debian-blessed.
- **Cons:** Configuration is a sprawl of directories and shell hooks. Debugging is "read the source." The tool is in maintenance mode — works fine, but few new features.

### Option B — `debian-installer` + preseed

The installer Debian ships on its own ISOs. Drive it via a preseed file (`preseed.cfg`).

- **Pros:** The most boring, well-trodden path. Massive deployments use it.
- **Cons:** Preseed is a key-value config file with awkward escape rules. Customizing the installer's *appearance* (moose branding, custom screens) means patching `cdebconf` themes and is genuinely painful. Conditional logic (e.g., "if no second disk, skip step 2") is a pre-script hack.
- **Verdict:** Right for a sysadmin tool, wrong for a consumer appliance.

### Option C — `mkosi`

systemd-team's modern image builder. Declarative TOML config, can produce disk images, ISOs, container images. Used by Fedora CoreOS-adjacent work and increasingly in the systemd ecosystem.

- **Pros:** Clean config. First-class support for the kind of immutable / A/B image we expect to migrate to (`SPEC.md` "OS update model"). Aligned with systemd, which we depend on heavily.
- **Cons:** Newer; less battle-tested for Debian specifically (better support for Fedora/Arch). Smaller community for "I'm building a Debian appliance" recipes.
- **Strategic angle:** if we're going to A/B immutable later anyway, picking mkosi now means one tool for both v1 and the future. live-build has no story for A/B images.

### Option D — Custom (`debootstrap` + `xorriso` + scripts)

Roll our own. What Ubuntu's modern installers do under the hood.

- **Pros:** Full control. No tool quirks to work around.
- **Cons:** We become the maintainers of an ISO builder. Many person-weeks to match what live-build gives for free.
- **Verdict:** Reject. Premature DIY for a problem with mature solutions.

### Decision (2026-06-16): Option C — `mkosi`

**Locked: mkosi is the single image builder** — for the install ISO, the cloud VM image, and the QEMU test image. This overturns the earlier "live-build-for-v1, migrate-to-mkosi-later" recommendation (`DECISIONS.md` 2026-06-16). One builder, one config, one artifact definition for every target.

Why mkosi-now rather than live-build-then-migrate:

- **The test lane is already mkosi, all the way up the stack.** `dev/test-qemu/` builds the full control plane (`host-agent → brain → Caddy + UI`), boots it under `mkosi qemu`, and `mkosi-repart` already produces a LUKS2+ext4 root that TPM-unseals and switch-roots (`TESTING.md` # Full-stack control-plane integration; `docs/progress/luks-tpm-enrollment.md`). Shipping live-build for the ISO would mean maintaining a *second* builder that must stay byte-identical with the test image to hold the "live fs == installed fs" invariant (# 3). mkosi makes that invariant trivially true — there is one artifact.
- **Systemd-native is the right substrate for moose.** We lean hard on systemd — `systemd-cryptenroll` + TPM unseal, UKI, `systemd-boot`, `cryptsetup-initramfs`. mkosi is the systemd team's own image tool, so partitioning / LUKS / TPM / UKI-signing are first-class rather than bolted on. (Umbrel, on the same Debian base, assembles a Docker-built rootfs + Rugix + Mender to get the equivalent; mkosi collapses that into one pipeline.)
- **One config emits every target.** The same mkosi definition produces the flashable install ISO **and** a cloud VM image (qcow2 / raw) for the hosted-in-cloud product. live-build has no cloud-image story; that would be a third path.
- **A/B-immutable is the stated future** (`SPEC.md` OS update model). live-build has no A/B story; mkosi's disk-image output A/B-swaps natively. We are **not** shipping A/B in v1 — v1 is mutable Debian + a flash-an-ISO install — but picking mkosi now means that future lands with no re-tooling.

What this decision **does not** settle (kept open — see `NEXT.md`):

- **The OTA update orchestrator.** Named on 2026-10-01: **RAUC**, with GRUB on both firmwares, not `systemd-sysupdate` (# 1b, `DECISIONS.md` 2026-10-01). mkosi builds the slot image; RAUC writes it and owns the slot switch.
- **The interactive installer is unchanged.** mkosi vs live-build is only *how the bootable artifact is assembled* — moose still ships the guided first-run installer of # 3 / `FIRST_RUN.md` Phase 1 (disk selection, recovery passphrase, confirm-wipe). The USB stick boots that installer, which writes the OS to the machine's internal disk. We are **not** adopting the competitors' direct-flash-the-image-onto-the-target model.

Knowingly accepted costs:

- mkosi's Debian support is thinner than live-build's (it is better-trodden for Fedora / Arch). The LUKS/TPM bring-up already paid down the riskiest part of that on a real Debian boot, but expect occasional sharp edges a live-build user would not hit.
- **A live installer ISO that boots a session is live-build's home turf** (the Tails / Kali pattern), and is the one part of mkosi's fit not yet proven in-repo — the test lane boots a *disk image*, not a live-session ISO carrying the kiosk installer (# 3). Validating mkosi's live-ISO output is a follow-up, not a reason to keep a second builder.
  - **⚠ Resolved 2026-06-17 (#199): mkosi emits no ISO — and moose no longer wants one.** Investigating this exact follow-up found mkosi 26's output formats are `{confext,cpio,directory,disk,esp,none,portable,sysext,tar,uki,oci,addon}` — there is **no `iso`/ISO9660 format** (and no `xorriso`/El-Torito code in the package). mkosi builds GPT *disk* images. The call (maintainer, 2026-06-17): **drop the literal `.iso` entirely; the bootable artifacts are disk images** — a `qcow2`/`raw` for the cloud VM and a `raw` `dd`'d to a USB stick for bare metal. Optical-media / CD-DVD boot is explicitly out of scope. The "live fs == installed fs" invariant (# 3) is unaffected — a `Format=disk` root is what gets booted/laid down — and "mkosi is the single builder" holds exactly (this is mkosi's native distribution model). The cloud VM image is the **priority** target; bare-metal USB follows (`#196` epic ordering). See `DECISIONS.md` 2026-06-17 and `progress/iso-mkosi-finding.md`.

---

## 3. The installer

`FIRST_RUN.md` Phase 1 specifies the installer's user-visible flow: hardware check → disk selection → recovery passphrase → confirm wipe → install → reboot. This section is *how* that flow runs.

### Three execution models

- **Model 1 — Custom TUI** (text-mode, ncurses-style). Lightweight, ugly, fine for tinkerers, wrong for the long-term audience.
- **Model 2 — Custom GUI in a minimal Xorg/Wayland session.** Boot a minimal desktop, run a moose-branded GTK/Qt app. Pretty, heavy on ISO size and dev work.
- **Model 3 — Web installer in a kiosk browser.** Boot a minimal compositor (`cage` or `weston --kiosk`), launch Chromium pointed at a local installer service (Go binary serving HTTP on `localhost`). The installer service does the actual work (partitioning, LUKS, TPM enrollment, file copy).

### Recommendation: Model 3 (kiosk web installer)

- Reuses our web stack — same TypeScript framework, same components, same designers as the post-install dashboard. Visual consistency from USB-boot to dashboard.
- The installer service is a sibling to `moose-brain` in shape: Go binary, HTTP API, but its job ends at first reboot. We can borrow patterns and even some packages.
- ISO cost: ~150–250 MB for compositor + Chromium. Acceptable on a multi-GB ISO.
- The same UI language carries forward — no jarring "install looks like a 90s setup, then suddenly it's a polished web app."

ZimaOS and a couple of other appliance OSes use this exact pattern. It's well-trodden.

### What the installer service does

1. Probe hardware (CPU, RAM, disks, UEFI, TPM2). Refuse with a clear message if any hard requirement (`FIRST_RUN.md`) fails.
2. Present disk picker + recovery-passphrase screen.
3. On confirm:
   a. Partition target disk(s) (GPT, ESP + LUKS-encrypted root).
   b. `cryptsetup luksFormat`, generate recovery passphrase, enroll TPM2 with `systemd-cryptenroll`.
   c. Lay down the OS image (squashfs → ext4 copy, or rsync from the live filesystem). The installer's *own* live environment is essentially the same image we lay on disk.
   d. Install GRUB to the ESP, configure for UEFI.
   e. Run `update-initramfs` so initramfs has TPM-unlock support.
4. Show recovery passphrase, require user confirmation.
5. Reboot.

### Decision: live filesystem == installed filesystem

The same root filesystem the live ISO boots from is what gets copied to disk. No separate "live image" vs. "installable image." Means everything we test in the live environment is what runs post-install. With one mkosi-built artifact (# 2) this invariant is structural rather than a discipline to maintain across two builders.

---

## 4. `host-agent` packaging

Three options:

- **A — Ship as `.deb` in our own apt repo.** ISO build pulls it during package selection. Updates ride apt. Standard Debian.
- **B — Bake the binary directly into the live filesystem at ISO build time** (no `.deb`, just a file + a systemd unit). Simpler, but no apt-managed update path.
- **C — Distribute as a container alongside the brain.** Inverts the architecture — host-agent is the *one* thing that should be on the host, not in a container (`CONTROL_PLANE.md`). Reject.

**Decision (2026-10-01): B, baked into the OS image.** With an A/B OS (# 1b), the image is the update unit, so `host-agent` needs no package of its own. It is a file plus a systemd unit in the slot, and it updates when the slot does (`UPDATES.md` # 2). The apt repo this section used to plan is not built. Option A's main argument was the apt update path, and that path retired with the A/B image (`DECISIONS.md` 2026-10-01).

- Native systemd unit, native logs, as before.
- Its version is the moose (OS) version (# Versioning).

---

## 5. `moose-brain` image

Per `CONTROL_PLANE.md`: brain runs as a container, supervised by host-agent.

### Build

- Multi-stage Dockerfile. Build stage compiles the Go binary (static, CGO disabled where possible). Runtime stage: **`debian:trixie-slim` with the `docker` CLI + Compose plugin bundled** (`docker-ce-cli` + `docker-compose-plugin` from Docker's official apt repo — the same trusted source as the host engine, per the Docker-package-source decision below). **Not distroless:** the brain orchestrates apps by shelling out to the `docker` / `docker compose` CLI (`internal/lifecycle/docker.go`), which a distroless runtime — no shell, no binaries — cannot host. Multi-stage already keeps the Go toolchain out of the final image; the bundled CLI is a runtime dependency it can't trim, putting the image at **~256 MB** (measured in M0, #163) — immaterial against the multi-GB app images the box pulls, and slim stays debuggable (it has a shell). See `DECISIONS.md` 2026-06-13 for the flip off distroless.
- Output is a single OCI image, tagged `vX.Y.Z` and `latest` (latest only on stable channel). `vX.Y.Z` is the **control-plane** version (`CONTROL_PLANE_VERSION`, # Versioning), not the moose (OS) version.

### Distribution — three options

- **A — Public registry (`ghcr.io/moose/brain` or Docker Hub).** Pull at first boot. Simple, no infra to run beyond a registry account. Requires internet at first boot.
- **B — Self-hosted registry (`registry.onmoose.io`).** Same as A but we own the namespace and don't depend on GitHub/Docker policies. Modest VPS cost.
- **C — Bundle the image in the ISO.** Image is loaded into Docker at install time via `docker load`. Works offline at first boot. ISO grows by the image size (~256 MB for the slim-with-CLI brain image, measured in M0 #163 — see the Build section above — still small against the multi-GB app images the box pulls).

### Recommendation: B + C combined

- **Bundle a pinned brain image in the ISO** so the box boots and is functional with zero internet.
- **Self-hosted registry for ongoing updates.** host-agent (or the brain itself) pulls newer tags from `registry.onmoose.io` when online.
- Self-hosted over public-registry-only because: (1) a `moose` namespace on Docker Hub is not guaranteed; (2) we already need `onmoose.io` infra for the mesh, adding a registry is incremental; (3) avoids dependency on a third party's pull-rate-limit policy.
- We can mirror to a public registry as a redundancy story, but it's not the source of truth.

### First-boot brain bootstrap

1. host-agent starts (systemd, after Docker).
2. host-agent checks `/var/lib/moose/brain-image.tar` (bundled in ISO) — if Docker doesn't already have the image, `docker load` it.
3. host-agent pulls the latest tag from `registry.onmoose.io` if online and a newer version exists. (Behavior on offline: keep the bundled version. Behavior on update failure: keep current. Never break boot.)
4. host-agent starts the brain container with the configured pin.
5. Brain takes over from there — Caddy, `moose-ui`, sidecars, etc. (`CONTROL_PLANE.md`).

---

## 5b. `moose-ui` image

The dashboard ships as a **second OCI image**, built and distributed the same way as the brain. `WEB_UI.md` owns the stack and deploy model; this section covers only how the image is built and lands on a box.

### Build

- Base Caddy (the same digest-pinned `CADDY_IMAGE` the proxy runs — # 5c), with the built UI bundle (`web-ui/dist`) baked in at `/srv/ui` and the trivial SPA Caddyfile (serve `/srv/ui`, fallback to `index.html`, gzip/brotli/ETag on by default). No build-stage Go compile — the bundle is produced by the UI's own `vite build` upstream of the image build (`WEB_UI.md`).
- Output is a single OCI image, tagged `vX.Y.Z` and `latest` (latest only on stable channel). It carries the same control-plane `vX.Y.Z` as the brain: the two images are one release line (# Versioning, below; `WEB_UI.md` # deploy + update flow).

### Distribution

Same as the brain (# 5 Distribution): **bundled in the ISO for offline first-boot** (`docker load` from a pinned tarball) **and** pulled from `registry.onmoose.io` for ongoing updates. Both images appear together in the release manifest (`RELEASE_MANIFEST.md`); the updater recreates only what changed (`WEB_UI.md` # deploy + update flow).

### Launch

`moose-ui` is **not** started by host-agent. The brain launches it as part of the control-plane stack, alongside Caddy (`CONTROL_PLANE.md` # Locked: the dashboard UI is a brain-launched container). host-agent's brain bootstrap (# First-boot brain bootstrap) ends at the brain; the brain brings up everything downstream.

---

## 5c. Pinned third-party build inputs

Most of what a moose build consumes is not ours. Two of the images a box runs are upstream outright: stock **Caddy** (the reverse proxy) and **`tecnativa/docker-socket-proxy`** (the container that fronts the raw Docker socket — `CONTROL_PLANE.md` # Docker socket exposure). The hosted profile also builds its own Caddy from two more upstream images, a `caddy:*-builder` and a `caddy:*-alpine` base (`dev/control-plane/caddy-acmedns/`), with one upstream Go module compiled in. And the two images that *are* ours are built on three more upstream bases (`golang:*`, `debian:*-slim`, `node:*-alpine`).

The two shipped images are pulled at **image-build** time and `docker save`d into the offline bundle, so a box never pulls them — it loads the tarballs. The bases and the module are consumed even earlier, while the images are being built. That makes a mutable tag a build problem, not a live-box problem, but a real one: `caddy:2-alpine` is rebuilt upstream whenever its base is patched, so two builds of the same moose commit weeks apart would contain different Caddy bytes with nothing recording which. It is the same reasoning the catalog already applies to every app image (`APP_LIFECYCLE.md` # Locked: image digest pinning).

**As-built (#432):** every third-party build input the control plane has is pinned in one checked-in file, [`dev/control-plane/images.lock`](../../dev/control-plane/images.lock) — the four images above by digest, the three base images `moose-brain` and `moose-ui` are built from by digest, and the one Go module compiled into the hosted Caddy by version.

- The file holds plain `NAME=name:tag@sha256:...` lines. The `Makefile` `include`s it and is the only reader; everything else that builds one of these images goes through a make target, `dev/cloud/stage-control-plane.sh` included. The plain form is also `source`-able, so a script can read a pin directly if one ever needs to. The tag half stays readable as a label; the digest decides the bytes. Each digest is the multi-arch **index** digest, so the pin does not assume an architecture.
- `make control-plane-images` pulls by digest, then re-tags to the plain tag before `docker save`. The saved tag matters: a box loads the tarball offline and the control-plane compose names the image by tag (`dev/control-plane/compose.yml`), so the digest cannot be its lookup key there.
- The hosted Caddy is built by `make caddy-acmedns-image`, which passes both pinned base images in as build args. Its Dockerfile carries **no default** for them — a default would be a second copy of the pin, free to drift, and a bare `docker build` would then quietly bake unpinned bytes.
- `internal/hostagent/controlplane/imagepins_test.go` fails if a pin loses its digest, or if a pinned tag stops matching the files that name that image by tag.
- **The two moose images take their bases as build args too.** `cmd/brain/Dockerfile` and `web-ui/Dockerfile` name no base directly; `make brain-image` / `make ui-image` feed them from the pin file, and `moose-ui`'s runtime base is the *same* `CADDY_IMAGE` the proxy runs, so the box never holds two different Caddys. This is **pinning, not reproducibility**: the brain's runtime stage still `apt-get`s `docker-ce-cli` from a live index, so two builds of one commit can still differ. It fixes the base bytes and records them, which is what a supply-chain question actually asks.
- **The hosted Caddy's plugin is version-pinned**, not only its two base images. `xcaddy build --with <module>` with no version takes the latest release that day, so the plugin could change under two frozen bases. Caddy's own version needs no argument — it comes from the pinned builder image (the shipped binary reports `v2.10.0`, matching `caddy:2.10.0-builder`).
- **Recording:** the file is checked in, so `git show v0.4.0:dev/control-plane/images.lock` answers "which Caddy was in v0.4.0?" from a version number alone. The digests are deliberately **not** added to the release manifest, which stays about the two images an update can move (`RELEASE_MANIFEST.md` # Fields); these bytes only change when someone edits the pin file.

**How to bump a pin.** Read the new digest, paste it into `images.lock`, and commit it on its own saying why:

```
docker buildx imagetools inspect caddy:2-alpine | awk '/^Digest:/{print $2; exit}'
```

The case that matters is an **upstream Caddy security release**: bump `CADDY_IMAGE` and both `CADDY_ACMEDNS_*_IMAGE` pins together, since they are the same upstream project. `CADDY_ACMEDNS_MODULE` is a Go module version, so it is read from the module's releases rather than from a registry. Nothing bumps a pin automatically — that is the point of a pin — so a security release is a normal PR like any other.

---

## 6. Artifacts and channels

### Per-release artifacts

**Two release lines** (# Versioning, below; `DECISIONS.md` 2026-10-01, built in #559). A moose release `vX.Y.Z` is the **OS release**: the disk images and the RAUC bundle below, named with the `VERSION` number. The control plane is released on its own line, git tag `control-plane-vX.Y.Z`: the brain and UI images, named with the `CONTROL_PLANE_VERSION` number. There is still no per-component tag to keep in sync.

- `moose-vX.Y.Z-amd64.qcow2` — the **cloud VM image** (priority target; the hosted product provisions tenants from it — `ENVIRONMENT.md` # Provisioning). Emitted by mkosi `Format=disk`.
- `moose-vX.Y.Z-amd64.raw` — the **bare-metal install medium**, `dd`'d / flashed to a USB stick (the "old laptop in the pantry" path). Same mkosi `Format=disk` rootfs; not optical media (no `.iso` — see # 2's 2026-06-17 resolution and `DECISIONS.md`).
- `moose-vX.Y.Z-amd64.raucb` + `.sha256` — the **OS update bundle** (# 1b # The bundle), signed by the release signer, what a running box downloads to update its OS. Built and attached since #562; a hosted box installs it since #563, once its update target names it. `host-agent` ships inside it and inside the disk images; there is no `.deb` and no apt repo (# 4).
- `registry.onmoose.io/moose/brain:vX.Y.Z` — the brain image, where `X.Y.Z` is the control-plane version. `latest` tag advances on stable channel. **The image tag has no `control-plane-` prefix**, only the git tag and the GitHub Release do: `vX.Y.Z` is the image tag shape every release before the split used, and the private control plane resolves digests by it.
- `registry.onmoose.io/moose/ui:vX.Y.Z` — the dashboard image. Same control-plane `vX.Y.Z` as the brain; both bundled in the ISO for offline first-boot.
- **The control-plane images are published publicly**, and `registry.onmoose.io` is a name we can point wherever later (the first realization is `ghcr.io/onmoose/…`, which costs nothing and has no egress bill for public packages). Public rather than private+credential because there is nothing to protect: the brain and UI are built from this public repo, and every secret a box holds is per-box and seeded at provision time (`ENVIRONMENT.md` # Provisioning), never baked into an image. A private registry would buy no confidentiality and would put a pull credential on every box — one more thing to seed, rotate, and fail at 03:00 on a machine nobody can SSH into. Boxes pull **by digest**, not by tag, using the same pinning the app installer already uses (`APP_LIFECYCLE.md`), so a public registry does not mean a mutable one.
- **As-built:** `CI / Cloud image` (`.github/workflows/ci-cloud-image.yml`) additionally attaches `moose-vX.Y.Z-amd64.raw.xz` + `moose-vX.Y.Z-amd64.raw.xz.sha256` to the tagged GitHub Release, gated on the same `SHOULD_PUBLISH` condition that used to gate the provider-snapshot upload. That Release asset is the only published **disk-image** artifact: #352 removed the provider-snapshot upload, so the lane holds no hosting-provider credential and a release publishes to no hosting provider. **As of #370 the same lane also pushes the two control-plane images to ghcr** (`ghcr.io/onmoose/brain` and `ghcr.io/onmoose/ui`, tagged `vX.Y.Z` + `latest`), gated on the same `SHOULD_PUBLISH` condition and running *after* the seeded-boot proof — so the images published are the exact local images baked into the disk image that just booted, not a rebuild of them. The push uses the job's own `GITHUB_TOKEN` (`packages: write` — granted at the `cloud-image` job in `release.yml` too, since a called reusable workflow can only narrow its caller's permissions, never widen them). The lane still holds no long-lived registry credential. Pushed digests are written to the job summary. The step runs **after** the Release-asset attach, so a registry failure cannot leave a release without its disk image. **The packages are public now.** ghcr makes a package private on first push. Turning the two packages public needed a one-time change in the package settings by an org admin, which the workflow cannot do. That change is done. `ghcr.io/onmoose/brain` and `ghcr.io/onmoose/ui` both answer an **anonymous** pull, and each carries `latest` plus every released tag from `v0.6.0` on. So the "published publicly" decision above is now real: a box with no registry login can pull the pair its update target names (`UPDATES.md` # 8.4). **As of #559 the lane has one publish switch per line.** The disk-image attach runs for an OS release, the ghcr push for a control-plane release, and both for a merge that bumps both files. A control-plane-only release still builds the disk image and runs every gate boot, then skips only the attach, so the images it pushes are still the ones that just booted. Neither publish step ever overwrites what is published. The attach treats the image and its checksum as one pair: both present are left, neither are uploaded, and exactly one refuses with the `gh release delete-asset` a person runs first. The ghcr push skips an image whose version tag is already there, and re-points that image's `latest` at it, byte for byte, so a push that died before `latest` is repaired. `latest` only moves forward: it is pushed or repaired only when this version is the newest `control-plane-v*` tag by semver, so re-running or dispatching an older release never points it back. With no `control-plane-vX.Y.Z` git tag, the push refuses outright if the brain or the UI tag is already published. Dispatch keeps `publish` (both lines) and adds `publish_os` and `publish_control_plane`, for re-publishing one line. `.raw.xz` names the actual shipped format — this lane's mkosi build produces `.raw` directly (xz-compressed for the upload), not a qcow2 conversion. **Since #562 an OS release attaches four files as one set:** the image, the bundle and their checksums (`dev/release/attach-image.sh`). The script detects and refuses a mixed set: all on the Release are left, none are uploaded in one call, and any other mix refuses, including a Release cut before the bundle existed. The `sign` job re-signs the bundle with the release signer first (# 1b # The bundle). **Since the CI speed-up (`../progress/ci-cloud-image-speedup.md`) the lane is four jobs** (the fourth, `sign`, since #562)**:** one build job makes both images and the bundle once, a matrix boots each boot group under each firmware at once, a `sign` job re-signs the bundle when the OS line publishes, and a separate publish job runs only after every boot (and `sign`) passed. The publish job loads the brain and UI tarballs the build job baked into the image and pushes those, so the published images are still the ones that just booted. It is the only job with write access.

### Channels

- **Stable** — what `mooseos.com/download` points at. Default for all installs.
- **Beta** — opt-in via Settings. Same artifacts, different repo / tag suffix.
- *(No nightly in v1. Internal CI builds exist but aren't a user-facing channel.)*

A box's channel determines which OS release and which control-plane release it is offered.

### Versioning

**Two version lines, one per update stream** (`DECISIONS.md` 2026-10-01, which flips 2026-07-16 in part):

- **moose `vMAJOR.MINOR.PATCH` is the OS release.** It names one A/B image: Debian at the locked snapshot (# 1b # The OS package lock), the kernel, firmware and `host-agent`. It keeps the repo-root **`VERSION`** file and the `vX.Y.Z` tags. A lock bump that changes packages is an OS patch release. **A patch release never changes an on-disk format**; only a minor release may, and a box never skips a minor (`UPDATES.md` # 1). `host-agent --version` prints it.
- **The control plane has its own `vMAJOR.MINOR.PATCH`**, for `moose-brain` and `moose-ui` together, in a repo-root **`CONTROL_PLANE_VERSION`** file, git-tagged `control-plane-vX.Y.Z` (its images are tagged `vX.Y.Z`, see # Per-release artifacts). `moose-brain --version` prints it. A control-plane release needs no OS release.
- **They meet at one check.** A control-plane build names the oldest moose release it runs on: `minimumAgentVersion` in `cmd/brain/main.go`, and `minimum_host_agent` in the release manifest. Both already compare against `host-agent`'s version, which is the moose version, so nothing on the wire changes.
- **The disk images bake the control plane current at build time**, for offline first boot only (# 5). After that a box follows the control-plane line, and an OS update never changes which control plane it runs (`UPDATES.md` # 3).

**As built (#559).** Two plain-text files at the repo root, one per line:

- **`VERSION`** holds the last released moose (OS) version. **`CONTROL_PLANE_VERSION`** holds the last released control-plane version. Each changes only in a release PR (`docs/dev/contributing.md` # Release model), and the bump **is** the release trigger for that line. No `-dev` suffix, no "next target" bookkeeping between releases. The control-plane line started at `0.15.0`, the number the brain already reported, so nothing a box shows jumped.
- **`release.yml` decides each line on its own** on every push to `main` (`dev/release/decide.sh`, tested by `dev/release/decide_test.go`). A `VERSION` bump tags `vX.Y.Z`, creates its GitHub Release and attaches the disk image. A `CONTROL_PLANE_VERSION` bump tags `control-plane-vX.Y.Z`, creates its GitHub Release with `--latest=false` (so GitHub's "Latest" stays on the release with the disk image), and pushes the brain and UI images to ghcr as `vX.Y.Z` with the control-plane number. A merge that bumps both cuts both from one cloud-image run. A merge that bumps neither is a green no-op. Bumping a file to a version tagged at another commit is a hard error, as before. A tag at the commit being released is a resume: an earlier run for the same push tagged it and then failed, so a re-run finishes the job instead of failing.
- **The control-plane line never overwrites a published image tag.** The image tag shape `vX.Y.Z` is shared with every release before the split, so a control-plane version whose image tag already exists on ghcr, for the brain or the UI, is refused before anything is tagged. So the line's first tag, `control-plane-v0.15.0`, is made once by hand at the `v0.15.0` commit, which is where the published `v0.15.0` images were built (`docs/dev/contributing.md` # Release model). Until that tag exists, `release.yml` refuses on every push to `main`.
- **Every build stamps two fields, not one:** its line's version and the git commit it was built from (`git rev-parse --short HEAD`), via `-ldflags -X` into `internal/version`. `host-agent` (real and fake) is stamped from `VERSION` and `host-agent --version` prints `moose 0.15.0 (g1a2b3c)`. The brain is stamped from `CONTROL_PLANE_VERSION` and `moose-brain --version` prints `moose control plane 0.15.0 (g1a2b3c)`, so its number is never read as the OS release. Both images also carry `org.opencontainers.image.version` (the control-plane version) and `org.opencontainers.image.revision` (the commit) labels. On a tagged release the commit is the tag's commit; on a dev build between releases it isn't, and that's visible without needing a suffix on the version string itself. The files (not `git describe`) are what CI asserts a pushed tag against, because the brain's container build and the mkosi cloud-image build both run from contexts without full `.git` history (the Dockerfile's build context excludes `.git` entirely, see `.dockerignore`).
- **The image inherits the moose (OS) SemVer, not CalVer:** `moose-vX.Y.Z-amd64.raw.xz` is named from `VERSION`.
- **The disk image bakes the brain and UI built from the same commit.** If `main` holds control-plane changes that were never released, an OS-only release bakes them under the last `CONTROL_PLANE_VERSION` number: the same number, different bytes. A box replaces them at its first control-plane update. Baking the last released control plane from ghcr by digest instead is #566.
- The image still carries a manifest listing the exact versions of every component it bundles (Debian base version, kernel, etc. — components genuinely external to this repo). With the OS lock (# 1b) that list is the committed package list.

---

## 7. Build pipeline shape (informational)

Not locking specifics, but the rough shape:

```
   Source (host-agent, brain, UI)
            │
            ▼
       CI (build, test)
            │
            ├──► host-agent binary ─► into the OS image (# 4)
            ├──► brain image ─────► registry.onmoose.io
            └──► ui image ────────► registry.onmoose.io  (caddy:alpine + bundle, see WEB_UI.md)
                                     │
                                     ▼
                          mkosi image assembly (Format=disk)
                                     │
                                     ▼
                  moose-vX.Y.Z-amd64.qcow2 (cloud VM, priority)
                  moose-vX.Y.Z-amd64.raw   (bare-metal USB)
                  moose-vX.Y.Z-amd64.raucb (OS update bundle, #562)
                                     │
                                     ▼
                              releases.onmoose.io
                                     │
                                     ▼
                        stable.json (+ minisig) — see RELEASE_MANIFEST.md
```

GitHub Actions or self-hosted CI — TBD, not architecturally interesting at this stage.

---

## Locked decisions

- **Base: Debian stable (currently Trixie / 13).**
- **Kernel: Debian backports kernel** for hardware support on BYO x86.
- **Non-free firmware bundled** for Wi-Fi and GPU support out of the box.
- **Image tooling: `mkosi`** (decided 2026-06-16, `DECISIONS.md`). One builder for the cloud VM image, the bare-metal USB install image, and the QEMU test lane; systemd-native, and A/B-ready for the immutable future. Overturns the earlier live-build-for-v1 recommendation. **(⚠ #199, 2026-06-17 resolved: mkosi has no ISO9660 output — it builds GPT *disk* images, and moose no longer ships a literal `.iso`. Artifacts are a `qcow2`/`raw` cloud image (priority) and a `raw` USB image; CD/DVD/optical boot is out of scope. See # 2's resolution + `DECISIONS.md` 2026-06-17.)**
- **Installer execution model: kiosk web installer.** Minimal compositor (`cage` / `weston --kiosk`) + Chromium pointed at a local installer service. Closest production reference: Fedora's Anaconda Web UI.
- **Docker package source: `docker-ce` from Docker's official apt repo.** Revisit if Docker Inc. policy changes; swap to `docker.io` is a one-line apt source change.
- **Docker runs with a daemon-wide `userns-remap`** on a `moose-remap` range (`1000000:65536`), `SUB_UID_COUNT 0` / `SUB_GID_COUNT 0` in `login.defs`, and the classic `overlay2` store that the remap implies. Both images; fixed for the life of a box (# User-namespace remap, `DECISIONS.md` 2026-09-29).
- **`host-agent` ships inside the OS image**, not as a Debian package and not as a container (# 4, `DECISIONS.md` 2026-10-01).
- **The OS is an A/B image** (# 1b): two 1 GiB whole-root slots of read-only squashfs-xz, held to a 60% budget, and a state partition, RAUC with GRUB on UEFI and legacy BIOS, an `/etc` overlay with a pinned list, a signed `verity` bundle per release, and a Debian snapshot lock. The hosted image is built in this layout (#561), the Debian snapshot lock is built (#560), and so is the signed bundle (#562), with an offline root CA and a CI-only signer; the update itself and the appliance image are not built yet (#563, #564).
- **`moose-brain` ships as an OCI image**, `debian:trixie-slim` runtime with the `docker` CLI + Compose plugin bundled (the brain shells out to them; distroless can't host them — `DECISIONS.md` 2026-06-13), from our own registry, also bundled in the ISO for offline first-boot.
- **`moose-ui` ships as a second OCI image** (`caddy:alpine` + baked UI bundle), from our own registry, also bundled in the ISO. Launched by the brain, not host-agent (`CONTROL_PLANE.md`).
- **Every third-party build input is pinned in one checked-in file** (`dev/control-plane/images.lock`, #432): upstream images by digest, base images by digest, the hosted Caddy's plugin by module version. Same reasoning as app images: a tag is not a lookup key. See # 5c for how to bump one.
- **Same root filesystem serves both the live (installer) environment and the installed system.**
- **SSH daemon installed but not enabled at boot; it follows the per-account opt-in** (# SSH, `AUTH.md` # Device access). Root login disabled. Appliance image only so far — hosted packaging is #467.
- **Channels: stable only in v1, no beta, no nightly.** Beta is additive when triggered (see `RELEASE_MANIFEST.md`).
- **Versioning: two lines, one per stream.** moose `vX.Y.Z` (repo-root `VERSION`) is the OS release, and the image inherits it; the control plane has its own semver (`CONTROL_PLANE_VERSION`, `control-plane-vX.Y.Z`). Every build additionally stamps the git commit. No per-component counters (`DECISIONS.md` 2026-10-01, flipping 2026-07-16 in part). Built in #559.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
