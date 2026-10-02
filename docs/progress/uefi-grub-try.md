# GRUB's try flag survives under UEFI

- **Status:** done
- **Date:** 2026-10-02
- **Specs touched:** `docs/specs/BUILD.md`, `docs/specs/UPDATES.md`, `docs/specs/TESTING.md`, `docs/architecture.md`, `docs/dev/hosted-boot-proof.md`

Closes #575, a slice of #486. It follows [host-agent-os-update.md](host-agent-os-update.md) (#563), whose `os-revert` boot found that under UEFI the grubenv never showed the booted slot's `TRY=1`, so a new slot that panics before userspace would be booted again and again. The cause was the boot lane, not GRUB and not the image: the harness ran OVMF with no writable VARS store, and OVMF then wrote its variables to the ESP and lost GRUB's write.

## What was done

### The root cause

Three probe runs, each on its own trace commit (dropped before the PR):

- **GRUB's write works.** Run 37061482623 turned on GRUB's `efidisk` and `disk` debug around `save_env` under UEFI. GRUB wrote 2 sectors (0xa84c, 0xa84d) of `hd0`, `save_env` returned 0, and a `load_env` right after read back `A_TRY=1`.
- **The disk never had it.** Runs 37063254219 and 37064755329 read those two sectors from Linux. In the initramfs, 3 s after boot, before `systemd-repart` and before anything mounts the ESP, the sectors held `A_TRY=0` under UEFI and `A_TRY=1` under BIOS. So the write was lost between GRUB and Linux, below GRUB.
- **The firmware writes the ESP.** The same runs listed the ESP: under UEFI it had a file `NvVars`, written at every boot. The harness picked `/usr/share/ovmf/OVMF.fd` (the combined 4 MB image) read-only, and found no VARS template, because Ubuntu 24.04 names them `OVMF_CODE_4M.fd` and `OVMF_VARS_4M.fd`. OVMF with no flash for its variables keeps them in memory and saves them to `NvVars` on the first FAT filesystem, through its own FAT driver. GRUB's raw write did not survive that. The exact way it is lost (most likely the FAT driver writing back sectors it cached before GRUB ran) was not traced further: with a VARS store there is no `NvVars` write, and the flag stays.

A real UEFI keeps its variables in flash and does not write the ESP, so this is a boot-lane bug. Nothing changes in the image that ships, and no disk is added.

### The fix (`dev/cloud/run-cloud-tests.sh`, `dev/cloud/test/bootstrap.sh`)

- The harness picks OVMF as a **pair**: a CODE image and the VARS template that matches it (Ubuntu 24.04's `_4M` pair first, then the older Debian/Ubuntu pair, Arch and Fedora paths). The VARS store is now **required**: without one the harness stops, rather than boot a firmware that writes the ESP. It prints `OVMF: code ..., vars from ...`. The same check is in the bootstrap preflight.
- `moose-test-grubenv.service` gains `RequiresMountsFor=/efi`. The ESP is `nofail` in fstab, so `local-fs.target` does not wait for it, and one probe run read the grubenv before the mount and logged nothing.

### The checks (`dev/cloud/cloud-assertions.sh`)

- **Every boot, both firmwares:** the grubenv GRUB left for this boot has `<booted slot>_TRY=1`, and the ESP has no `NvVars`. The line is `layout: GRUB saved A_TRY=1 before booting slot A; no NvVars on the ESP`. The `os-revert` trial boot of slot B checks `B_TRY=1` too.
- **A slot that panics before userspace is skipped** (the second half of "Done when"). `os-revert` gains a last stage. After the revert, on slot A, the guest waits until slot A is marked good, plants `/state/moose-test/panic-slot-B`, runs `rauc status mark-active other` (slot B first, `B_OK=1 B_TRY=0`) and reboots. A test-only initramfs hook, `moose-test-panic` (local-bottom, after `moose-state`, so the state partition is mounted), sees the file on slot B, leaves a note, and crashes the kernel with sysrq `c`. `panic=10` reboots. Stage 4 must come up on slot A with `B_OK=1 B_TRY=1` and the note there. The harness also checks the serial log has the hook's line and `Kernel panic`. Without the fix this stage never ends under UEFI: slot B boots and panics for ever, and the boot times out.
- The hook is written by `dev/cloud/test/bootstrap.sh` into the boot-proof image only, so it is in the test bundle (the bundle is that image's slot) and never in the image that ships.
- The `A_OK=1` layout check now reads the grubenv once and then matches. `grub-editenv list | grep -qx` under `pipefail` failed once when `grep` exited first (run 37063254219, BIOS).

### The safety net stays on the marker

The second dismissed Greptile point on #574 asked whether `os-trial-check` should use the grubenv as a second signal, now that `TRY` holds. It does not. The only use would be "the marker is there but the grubenv says `TRY=0`, so the slot is already good, do not reboot". That trusts a flag that GRUB writes through the firmware, and this slice shows a firmware can lose that write without a trace. On such a firmware the safety net would then leave a hung slot running for ever. The marker is on the state partition and only `host-agent` writes and removes it, so it stays the one signal. Its comment, and two comments and one log line in `internal/hostagent/osupdate`, no longer say UEFI loses the flag; they say a firmware can.

## Numbers

| What | Value |
|------|-------|
| Disk cost on a box | none: no image change, the ESP is unchanged |
| `os-revert` boot job, before (run 37058171441) | 3.4 min UEFI, 3.6 min BIOS |
| `os-revert` boot job, with the panic stage (run 37067400755) | 4.9 min UEFI, 4.0 min BIOS |
| Switch to back on slot A, the first revert (run 37067400755) | 116 s UEFI, 111 s BIOS, as before |

## How it was verified

All in `CI / Cloud image` with `publish=false`; nothing was built or booted locally.

| Run | Boots | What it showed |
|-----|-------|----------------|
| 37061482623 | `unseeded`, GRUB trace | GRUB writes sectors 0xa84c and 0xa84d under UEFI, `save_env` returns 0, read-back gives `A_TRY=1`; Linux sees `A_TRY=0` |
| 37063254219 | `unseeded`, Linux trace | the raw sectors on `/dev/vda` hold `A_TRY=0`; the ESP has an `NvVars` written during boot (BIOS red on the `pipefail` race above) |
| 37064755329 | `unseeded`, initramfs trace | the initramfs, before anything else touches the disk, reads `A_TRY=0` under UEFI and `A_TRY=1` under BIOS |
| 37066652030 | `unseeded os-revert` | red in the build: the test postinst runs before the initramfs exists, so a check placed there could not work; dropped |
| 37067400755 | `unseeded os-revert` | green, all four jobs: `A_TRY=1` at boot under UEFI and BIOS, no `NvVars`, `B_TRY=1` on the trial boot, `PANIC SKIP OK` under both firmwares |
| FULL_RUN_ID | the full list, final head | FULL_RUN_RESULT |

`make check` is green (the Go change is comments and one log line).

## How it maps to the specs

- `BUILD.md` # 1b # Boot: "a slot that never gets there is skipped on the next boot" now holds under UEFI in the boot lane, and the spec says what it depends on (a firmware that does not write the ESP) and how the lane checks it.
- `UPDATES.md` # 1: the trial timer goes by the marker only, with the reason.
- `TESTING.md` and `hosted-boot-proof.md`: the new checks, the `os-revert` panic stage, and how to read a red one.

## Known gaps & deviations

- **QEMU only.** A real Hetzner CPX (UEFI) has not been checked. If its firmware has no persistent variable store and saves `NvVars` to the ESP the way OVMF did here, GRUB's try flag would be lost there too. The first box on a CPX should run the image's boot once and read `grub-editenv /efi/grub/grubenv list` and `ls /efi` for an `NvVars`.
- **The panic is in the initramfs**, not in a broken kernel image. It is a real kernel panic before systemd, through the same `panic=10` path. A kernel that does not boot at all (GRUB cannot load it) is a different case and is not covered by this proof.
- The test-lane OVMF varstore is shared by every boot of one firmware run, as before.

## What's next

- **For #564 (appliance), about the ESP:** GRUB's `save_env` is a raw write of the grubenv's sectors, done through the firmware's disk driver. Anything that also writes the ESP from the firmware side can lose it: an OVMF with no VARS store (`NvVars`), and possibly a real UEFI that falls back to the same. The appliance medium lane (`dev/test-qemu/run-medium-tests.sh`, local-only) still falls back to the combined `OVMF.fd` with no VARS store when it does not find the old `OVMF_CODE.fd`/`OVMF_VARS.fd` names, the same trap this slice removed from the hosted lane. It should take the same pair list before it relies on the grubenv. The 512-byte sector decision for the appliance ESP does not change any of this: the grubenv is 2 sectors at 512 bytes and would be 1 sector at 4096.
- Check one real UEFI provider box for `NvVars` (the gap above).
