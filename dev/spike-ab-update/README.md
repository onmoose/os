# A/B update engine spike (#485)

Throwaway code for the spike in #485. It lives only on the branch `test/485-ab-update-spike` and is never merged. The findings go in `docs/progress/`.

It builds a small trixie box three times per engine (v1, v2, and a broken v3), then boots v1 in QEMU and lets the guest run the whole sequence on its own: update to v2, update to v3, fall back to v2. Nobody touches the console.

- `mkosi.conf`, `mkosi.extra/`, `mkosi.postinst.chroot`: the shared base. `mkosi.extra/usr/lib/spike/` holds the early init (data partition, `/etc` overlay, pinned files), the health gate and the proof runner.
- `mkosi.profiles/rauc/`: RAUC with GRUB on both UEFI and legacy BIOS.
- `mkosi.profiles/sysupdate/`: systemd-sysupdate with systemd-boot and UKIs, UEFI only. `sysupdate/build-sysupdate.sh` builds the binary, because Debian does not ship it.
- `build.sh <engine>` and `run-proofs.sh <engine> <firmware>` are CI only. The workflow is `.github/workflows/spike-ab-update.yml` and runs on every push to the branch.
