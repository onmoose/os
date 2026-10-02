#!/bin/bash
# PROBE #564, in the guest. One QEMU run, three boots, driven by a stage file
# on the ESP:
#   1. slot A: check Secure Boot, lockdown, the verity root, that GRUB saved
#      A_TRY=1; then corrupt one block of the canary file in slot B, make B
#      the first slot (B_OK=1 B_TRY=0) and reboot.
#   2. slot B: GRUB must have saved B_TRY=1. Read the canary: dm-verity must
#      panic the kernel (panic=10 reboots).
#   3. slot A again: GRUB skipped B (B_TRY=1). PCR 7 is the same as on boot 1.
# Prints PROBE-VERDICT: PASS or FAIL and powers off.
set -u
say() { echo "PROBE: $*" > /dev/console; }
fail() { say "FAIL: $*"; echo "PROBE-VERDICT: FAIL" > /dev/console; sync; systemctl poweroff; exit 1; }

mountpoint -q /efi || mount -t vfat /dev/disk/by-partlabel/esp /efi || fail "cannot mount the ESP"
ls /efi >/dev/null
ENV=/efi/EFI/debian/grubenv
slot="$(sed -n 's/.*rauc\.slot=\([AB]\).*/\1/p' /proc/cmdline)"
stage="$(cat /efi/probe-stage 2>/dev/null || echo none)"
envlist="$(grub-editenv "$ENV" list | grep -v ROOTHASH | tr '\n' ' ')"
say "boot: slot=$slot stage=$stage"
say "cmdline: $(cat /proc/cmdline)"
say "grubenv at boot: $envlist"
sbvar=/sys/firmware/efi/efivars/SecureBoot-8be4df61-93ca-11d2-aa0d-00e098032b8c
sb="$(od -An -t u1 "$sbvar" 2>/dev/null | awk '{print $NF}')"
say "SecureBoot efivar: ${sb:-missing}"
say "kernel: $(dmesg | grep -iE 'secure ?boot|lockdown' | head -5 | tr '\n' '|')"
lockdown="$(cat /sys/kernel/security/lockdown 2>/dev/null || echo unknown)"
say "lockdown: $lockdown"
say "root: $(findmnt -no SOURCE,FSTYPE,OPTIONS /)"
say "verity: $(veritysetup status moose-root 2>&1 | tr -s ' ' | tr '\n' ';')"
pcr7="$(cat /sys/class/tpm/tpm0/pcr-sha256/7 2>/dev/null || echo none)"
say "pcr7: $pcr7"
say "tpm event log: $(ls /sys/kernel/security/tpm0/ 2>/dev/null | tr '\n' ' ')"

case "$slot:$stage" in
A:none)
    say "sizes: esp used $(du -sb /efi | cut -f1) bytes; $(df -B1 --output=size,used /efi | tail -1)"
    say "sizes: kernel $(stat -c %s /usr/lib/moose/boot/vmlinuz), initrd $(stat -c %s /usr/lib/moose/boot/initrd.img)"
    say "sizes: root uncompressed $(du -sbx / | cut -f1) bytes"
    say "sizes: firmware $(du -sb /usr/lib/firmware | cut -f1) bytes"
    say "efi files: $(cd /efi && find . -type f -printf '%p=%s ' )"
    [ "$sb" = "1" ] || fail "Secure Boot is not on (efivar=$sb)"
    case "$lockdown" in *"[integrity]"*|*"[confidentiality]"*) ;; *) fail "kernel not locked down: $lockdown" ;; esac
    case "$(findmnt -no SOURCE /)" in /dev/mapper/moose-root) ;; *) fail "/ is not the verity device" ;; esac
    veritysetup status moose-root | grep -q 'status:.*verified' || say "note: veritysetup status has no 'verified' line"
    echo "$envlist" | grep -q 'A_TRY=1' || fail "GRUB did not save A_TRY=1 (grubenv: $envlist)"
    echo "$pcr7" > /efi/probe-pcr7
    m="MOOSE-PROBE-"; m="${m}CANARY-7f3a9c"
    part=/dev/disk/by-partlabel/moose-slot-b
    off="$(grep -obUaF -m1 "$m" "$part" | head -1 | cut -d: -f1)"
    [ -n "$off" ] || fail "canary marker not found in slot B"
    blk=$(( (off / 4096 + 2) * 4096 ))
    dd if=/dev/urandom of="$part" bs=4096 seek=$((blk / 4096)) count=1 conv=notrunc,fsync status=none || fail "dd into slot B failed"
    say "corrupted slot B at byte $blk (canary marker at $off)"
    grub-editenv "$ENV" set ORDER="B A" A_OK=1 A_TRY=0 B_OK=1 B_TRY=0 || fail "grub-editenv set failed"
    echo tried-B > /efi/probe-stage
    sync
    say "stage 1 done: slot B first, rebooting"
    systemctl reboot
    ;;
B:tried-B)
    echo "$envlist" | grep -q 'B_TRY=1' || say "WARN: GRUB did not save B_TRY=1 (grubenv: $envlist)"
    echo "$envlist" | grep -q 'B_TRY=1' && echo b-try-ok > /efi/probe-b-try
    echo booted > /efi/probe-b-booted
    sync
    say "on slot B: reading the canary, dm-verity must panic"
    cat /usr/lib/probe/canary > /dev/null
    sleep 20
    fail "read the corrupted canary on slot B with no panic"
    ;;
A:tried-B)
    [ -e /efi/probe-b-booted ] || fail "slot B never booted"
    [ -e /efi/probe-b-try ] || fail "GRUB did not save B_TRY=1 before booting slot B"
    echo "$envlist" | grep -q 'B_TRY=1' || fail "B_TRY is not 1 after the revert (grubenv: $envlist)"
    echo "$envlist" | grep -q 'A_TRY=1' || fail "GRUB did not save A_TRY=1 on the revert boot (grubenv: $envlist)"
    first="$(cat /efi/probe-pcr7)"
    [ "$first" = "$pcr7" ] || fail "PCR 7 changed across boots: $first then $pcr7"
    say "back on slot A after slot B's verity panic; PCR 7 stable ($pcr7)"
    echo "PROBE-VERDICT: PASS" > /dev/console
    sync
    systemctl poweroff
    ;;
*)
    fail "unexpected slot/stage $slot:$stage"
    ;;
esac
