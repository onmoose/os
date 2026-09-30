#!/usr/bin/env bash
# SPIKE (#485), not for merge. Boots v1 of one engine under one firmware and
# lets the guest run the whole proof sequence on its own (proof.sh): update to
# v2, update to the broken v3, fall back to v2. One QEMU process for the whole
# run. The guest reboots itself between stages and powers off at the end.
#
#   dev/spike-ab-update/run-proofs.sh rauc|sysupdate uefi|bios
#
# Nobody touches the console. Air-gapped: no NIC at all.
# CI only, like build.sh.
set -euo pipefail

ENGINE="${1:?usage: run-proofs.sh <engine> uefi|bios}"
FW="${2:?usage: run-proofs.sh <engine> uefi|bios}"
SPIKE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$SPIKE/../.." && pwd)"
OUT="$REPO/.dev/spike-ab/$ENGINE"
W="$OUT/run-$FW"
rm -rf "$W"; mkdir -p "$W"
SERIAL="$W/serial.log"

cp --sparse=always "$OUT/v1/moose-spike_1.raw" "$W/disk.raw"
# A provider disk is far bigger than the image. The data partition must grow
# into the rest (proof 1).
truncate -s 16G "$W/disk.raw"

ACCEL=tcg
if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then ACCEL=kvm; fi

args=(
    -machine "q35,accel=$ACCEL"
    -cpu "$([ "$ACCEL" = kvm ] && echo host || echo max)"
    -m 2G -smp 2
    -display none -monitor none
    -serial "file:$SERIAL"
    -nic none
    -drive "file=$W/disk.raw,if=virtio,format=raw"
    -drive "file=$OUT/ctl.img,if=virtio,format=raw,readonly=on"
)
if [ "$FW" = uefi ]; then
    code=""; vars=""
    for c in /usr/share/OVMF/OVMF_CODE_4M.fd /usr/share/OVMF/OVMF_CODE.fd /usr/share/ovmf/OVMF.fd; do
        [ -r "$c" ] && { code="$c"; break; }
    done
    for v in /usr/share/OVMF/OVMF_VARS_4M.fd /usr/share/OVMF/OVMF_VARS.fd; do
        [ -r "$v" ] && { vars="$v"; break; }
    done
    [ -n "$code" ] && [ -n "$vars" ] || { echo "OVMF not found" >&2; exit 1; }
    cp "$vars" "$W/vars.fd"
    args+=( -drive "if=pflash,format=raw,unit=0,readonly=on,file=$code"
            -drive "if=pflash,format=raw,unit=1,file=$W/vars.fd" )
fi
# FW=bios attaches nothing: QEMU's built-in SeaBIOS, the firmware Hetzner CX
# presents (#277).

echo "=== run-proofs.sh: engine=$ENGINE firmware=$FW accel=$ACCEL"
t0=$(date +%s)
rc=0
timeout 1500 qemu-system-x86_64 "${args[@]}" || rc=$?
t1=$(date +%s)
echo "=== qemu exited rc=$rc after $((t1 - t0))s"

grep -a 'SPIKE' "$SERIAL" | tee "$W/spike-lines.txt" || true
verdict=$(grep -a -o 'SPIKE-VERDICT: .*' "$SERIAL" | tail -n1 | tr -d '\r' || true)
echo "=== verdict: ${verdict:-none (timeout or hang, see serial.log)}"
{
    echo "### $ENGINE / $FW: ${verdict:-no verdict}"
    echo '```'
    cat "$W/spike-lines.txt"
    echo '```'
} >> "${GITHUB_STEP_SUMMARY:-/dev/null}"

if [ "$verdict" != "SPIKE-VERDICT: PASS" ]; then
    echo "=== last 120 serial lines"
    tail -n 120 "$SERIAL" | tr -d '\r'
    exit 1
fi
