#!/usr/bin/env bash
# PROBE #564 (dropped before the PR). CI only. Builds the probe image, appends a
# dm-verity hash tree to slot A, copies the slot to B, writes both root hashes
# into the grubenv, and boots it once under OVMF with Secure Boot on (Microsoft
# keys enrolled) and a swtpm TPM. The guest drives three boots on its own
# (mkosi.extra/usr/lib/probe/probe.sh). Writes a report to $GITHUB_STEP_SUMMARY.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
PROBE_DIR="${REPO_ROOT}/dev/probe-564"
WORK="${REPO_ROOT}/.dev/probe-564"
SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/null}"
mkdir -p "$WORK"

sum() { echo "$*" | tee -a "$SUMMARY"; }

phase="${1:-all}"

build() {
    # Docker's repo and pins, and the locked Debian snapshot, as the hosted build.
    # shellcheck source=dev/os-lock/os-lock.sh
    . "${REPO_ROOT}/dev/os-lock/os-lock.sh"
    stage_os_lock_apt "${PROBE_DIR}/mkosi.pkgmngr"
    local ts t0 t1
    ts="$(os_lock_snapshot)"
    t0=$(date +%s)
    mkosi --directory "$PROBE_DIR" --snapshot "$ts" --force build
    t1=$(date +%s)
    sum "### Build"
    sum ""
    sum "mkosi build: $((t1 - t0)) s (snapshot $ts)"
}

verity() {
    local img="$WORK/moose-probe.raw"
    python3 - "$img" "$WORK" "$SUMMARY" <<'PY'
import json, os, re, struct, subprocess, sys
img, work, summary = sys.argv[1], sys.argv[2], sys.argv[3]
def say(s=""):
    print(s)
    with open(summary, "a") as f: f.write(s + "\n")
pt = json.loads(subprocess.check_output(["sfdisk", "-J", img]))["partitiontable"]
ss = pt.get("sectorsize", 512)
parts = {p.get("name"): p for p in pt["partitions"]}
def span(name):
    p = parts[name]; return p["start"] * ss, p["size"] * ss
a_off, a_size = span("moose-slot-a")
b_off, b_size = span("moose-slot-b")
e_off, e_size = span("esp")
with open(img, "rb") as f:
    f.seek(a_off); sb = f.read(96)
magic, = struct.unpack_from("<I", sb, 0)
assert magic == 0x73717368, hex(magic)
comp, = struct.unpack_from("<H", sb, 20)
used, = struct.unpack_from("<Q", sb, 40)
hoff = (used + 4095) // 4096 * 4096
blocks = hoff // 4096
slot = os.path.join(work, "slot.img")
subprocess.check_call(["dd", f"if={img}", f"of={slot}", "bs=4M", "iflag=skip_bytes,count_bytes",
                       f"skip={a_off}", f"count={a_size}", "conv=sparse", "status=none"])
out = subprocess.check_output(["veritysetup", "format", f"--hash-offset={hoff}",
                               f"--data-blocks={blocks}", slot, slot], text=True)
print(out)
roothash = re.search(r"Root hash:\s*([0-9a-f]+)", out).group(1)
# Hash tree size: the levels of 4 KiB hash blocks (128 sha256 per block) plus the superblock.
n, tree = blocks, 0
while n > 1:
    n = (n + 127) // 128; tree += n
tree_bytes = tree * 4096 + 4096
end = hoff + tree_bytes
assert end <= a_size, (end, a_size)
for off in (a_off, b_off):
    subprocess.check_call(["dd", f"if={slot}", f"of={img}", "bs=4M", "oflag=seek_bytes",
                           "iflag=count_bytes", f"seek={off}", f"count={end}", "conv=notrunc", "status=none"])
os.remove(slot)
# grubenv: the one the image baked, plus both root hashes (same bytes in A and B).
env = os.path.join(work, "grubenv")
subprocess.check_call(["mcopy", "-n", "-i", f"{img}@@{e_off}", "::/EFI/debian/grubenv", env])
subprocess.check_call(["grub-editenv", env, "set", f"A_ROOTHASH={roothash}", f"B_ROOTHASH={roothash}"])
assert os.path.getsize(env) == 1024
subprocess.check_call(["mcopy", "-o", "-i", f"{img}@@{e_off}", env, "::/EFI/debian/grubenv"])
print(subprocess.check_output(["grub-editenv", env, "list"], text=True))
GiB, MB = 1 << 30, 1e6
say("### Slot and verity sizes")
say("")
say(f"| What | Bytes | Share of a 1 GiB slot | Share of a 64 GB OS drive |")
say(f"|---|---|---|---|")
say(f"| squashfs (xz, compression id {comp}) | {used:,} ({used/MB:.1f} MB) | {100*used/GiB:.1f}% | {100*used/64e9:.2f}% |")
say(f"| verity hash tree + superblock | {tree_bytes:,} ({tree_bytes/MB:.2f} MB) | {100*tree_bytes/GiB:.2f}% | {100*tree_bytes/64e9:.3f}% |")
say(f"| squashfs + tree | {end:,} ({end/MB:.1f} MB) | {100*end/GiB:.1f}% | {100*end/64e9:.2f}% |")
say(f"| tree as a share of the squashfs | | {100*tree_bytes/used:.2f}% | |")
say(f"| ESP partition | {e_size:,} | | {100*e_size/64e9:.2f}% |")
say("")
say(f"Root hash: `{roothash}`; hash offset {hoff}; data blocks {blocks}.")
PY
}

report_packages() {
    # Installed sizes from the slot's own dpkg status, and what parts of the
    # tree cost once compressed with xz (each part made into its own squashfs).
    local img="$WORK/moose-probe.raw" tree="$WORK/tree" off
    off="$(sfdisk -J "$img" | python3 -c 'import json,sys; p=[x for x in json.load(sys.stdin)["partitiontable"]["partitions"] if x.get("name")=="moose-slot-a"][0]; print(p["start"]*512)')"
    rm -rf "$tree"
    sudo unsquashfs -q -n -d "$tree" -o "$off" "$img" >/dev/null
    python3 - "$tree/var/lib/dpkg/status" "$SUMMARY" <<'PY2'
import sys
status, summary = sys.argv[1], sys.argv[2]
pk = []
for para in open(status).read().split("\n\n"):
    f = dict(l.split(": ", 1) for l in para.splitlines() if ": " in l and not l.startswith(" "))
    if f.get("Package"): pk.append((f["Package"], int(f.get("Installed-Size", "0")) * 1024))
tot = sum(s for _, s in pk)
with open(summary, "a") as out:
    out.write(f"\n### Packages: {len(pk)}, installed size {tot/1e6:.1f} MB\n\n| Package | Installed MB |\n|---|---|\n")
    for n, s in sorted(pk, key=lambda x: -x[1])[:25]:
        out.write(f"| {n} | {s/1e6:.1f} |\n")
    fw = sum(s for n, s in pk if n.startswith("firmware-"))
    out.write(f"\nfirmware-* packages: {fw/1e6:.1f} MB installed\n")
PY2
    {
        echo ""
        echo "### Compressed (squashfs xz) cost of parts of the slot"
        echo ""
        echo "| Part | Uncompressed MB | squashfs xz MB |"
        echo "|---|---|---|"
        local d u c
        for d in usr/lib/firmware usr/lib/modules usr/lib/moose/boot usr/bin usr/libexec usr/lib/x86_64-linux-gnu usr/share usr/lib/python3; do
            [ -d "$tree/$d" ] || continue
            u="$(sudo du -sb "$tree/$d" | cut -f1)"
            sudo mksquashfs "$tree/$d" "$WORK/part.sqfs" -comp xz -noappend -quiet >/dev/null
            c="$(stat -c %s "$WORK/part.sqfs")"
            echo "| /$d | $(python3 -c "print(f'{$u/1e6:.1f}')") | $(python3 -c "print(f'{$c/1e6:.1f}')") |"
        done
        echo ""
        echo "firmware files: $(sudo find "$tree/usr/lib/firmware" -type f | wc -l), of which already compressed (.xz/.zst): $(sudo find "$tree/usr/lib/firmware" -type f \( -name '*.xz' -o -name '*.zst' \) | wc -l)"
    } | tee -a "$SUMMARY"
    sudo rm -rf "$tree" "$WORK/part.sqfs"
}

boot() {
    local img="$WORK/moose-probe.raw" run="$WORK/run" code vars accel
    rm -rf "$run"; mkdir -p "$run/tpm"
    code=/usr/share/OVMF/OVMF_CODE_4M.secboot.fd
    vars=/usr/share/OVMF/OVMF_VARS_4M.ms.fd
    [ -r "$code" ] && [ -r "$vars" ] || { echo "no Secure Boot OVMF pair ($code, $vars)" >&2; exit 1; }
    cp "$vars" "$run/vars.fd"
    swtpm socket --tpmstate "dir=$run/tpm" --ctrl "type=unixio,path=$run/tpm/sock" \
        --tpm2 --daemon --log "file=$run/tpm/log,level=1"
    for _ in $(seq 1 50); do [ -S "$run/tpm/sock" ] && break; sleep 0.1; done
    accel=kvm; [ -w /dev/kvm ] || accel=tcg
    local t0 t1 rc=0
    t0=$(date +%s)
    timeout 1200 qemu-system-x86_64 \
        -machine "q35,smm=on,accel=$accel" -cpu "$([ $accel = kvm ] && echo host || echo max)" \
        -m 2048 -smp 2 \
        -global driver=cfi.pflash01,property=secure,value=on \
        -global ICH9-LPC.disable_s3=1 \
        -drive "if=pflash,format=raw,unit=0,readonly=on,file=$code" \
        -drive "if=pflash,format=raw,unit=1,file=$run/vars.fd" \
        -chardev "socket,id=chrtpm,path=$run/tpm/sock" \
        -tpmdev emulator,id=tpm0,chardev=chrtpm -device tpm-tis,tpmdev=tpm0 \
        -drive "file=$img,format=raw,if=virtio" \
        -display none -serial "file:$run/serial.log" -monitor none -nic none || rc=$?
    t1=$(date +%s)
    {
        echo ""
        echo "### Boot (Secure Boot on: $code + $(basename "$vars"), swtpm, accel $accel)"
        echo ""
        echo "qemu exit $rc after $((t1 - t0)) s"
        echo ""
        echo '```'
        grep -aE 'moose-grub:|moose-verity:|PROBE|Secure boot|secureboot|Kernel is locked down|lockdown|device-mapper: verity|Kernel panic|prohibited|error:' "$run/serial.log" | cut -c1-400 | head -150
        echo '```'
    } | tee -a "$SUMMARY"
    grep -q 'PROBE-VERDICT: PASS' "$run/serial.log" || { echo "probe verdict is not PASS" >&2; tail -80 "$run/serial.log" >&2; exit 1; }
}

case "$phase" in
    build) build ;;
    verity) verity ;;
    packages) report_packages ;;
    boot) boot ;;
    all) build; verity; report_packages; boot ;;
    *) echo "usage: $0 [build|verity|packages|boot|all]" >&2; exit 2 ;;
esac
