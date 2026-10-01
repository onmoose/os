#!/bin/bash
# SPIKE (#485), not for merge. The in-guest proof runner.
#
# The guest is air-gapped. The update artifacts arrive on a second disk with
# the filesystem label SPIKECTL. One stage runs per boot, and the stage lives
# on the data partition, so the sequence survives the reboots:
#
#   start          v1 in slot A. Proof 1 (layout). Seed the data. Install v2.
#   expect-2       v2 booted in the other slot. Proof 2. Proof 4 (data) and the
#                  per-box state check. Install the broken v3.
#   (broken boot)  v3 fails its health check and reboots. Nothing runs here.
#   expect-revert  back on v2 with nobody at a console. Proof 3. Proof 4 again.
#
# Every line the host harness reads starts with "SPIKE:". The last one is
# "SPIKE-VERDICT: PASS" or "SPIKE-VERDICT: FAIL <reason>", then the guest
# powers off.
set -u

ENGINE=$(cat /usr/lib/spike/engine)
STATE=/data/spike
CTL=/mnt/spike-ctl
mkdir -p "$STATE"

say() { echo "SPIKE: $*"; }
fail() {
    echo "SPIKE-VERDICT: FAIL $*"
    diag
    systemctl poweroff
    exit 1
}
pass() {
    echo "SPIKE-VERDICT: PASS"
    systemctl poweroff
    exit 0
}
diag() {
    say "diag: lsblk"; lsblk -o NAME,PARTLABEL,PARTUUID,SIZE,FSTYPE,MOUNTPOINTS | sed 's/^/SPIKE:   /'
    say "diag: boot state"; bootstate | sed 's/^/SPIKE:   /'
    say "diag: failed units: $(systemctl --failed --no-legend --plain | awk '{print $1}' | tr '\n' ' ')"
}

version() { . /etc/os-release; echo "${IMAGE_VERSION:-unknown}"; }

# --- engine hooks ------------------------------------------------------------

slot() {
    if [ "$ENGINE" = rauc ]; then
        sed -n 's/.*rauc\.slot=\([^ ]*\).*/\1/p' /proc/cmdline
    else
        blkid -s PARTLABEL -o value "$(findmnt -no SOURCE /)"
    fi
}

bootstate() {
    if [ "$ENGINE" = rauc ]; then
        grub-editenv /efi/grub/grubenv list
    else
        ls -1 /efi/EFI/Linux/
    fi
}

install_version() {
    local v=$1 t0 t1
    t0=$(date +%s)
    if [ "$ENGINE" = rauc ]; then
        say "rauc install $CTL/rauc/v$v.raucb"
        rauc install "$CTL/rauc/v$v.raucb" 2>&1 | sed 's/^/SPIKE:   rauc: /'
        [ "${PIPESTATUS[0]}" -eq 0 ] || fail "rauc install of v$v failed"
    else
        mkdir -p /run/spike-src
        mountpoint -q /run/spike-src && umount /run/spike-src
        mount --bind "$CTL/sysupdate/v$v" /run/spike-src
        say "systemd-sysupdate update $v"
        /usr/lib/systemd/systemd-sysupdate --no-pager list 2>&1 | sed 's/^/SPIKE:   sysupdate: /'
        /usr/lib/systemd/systemd-sysupdate --no-pager update "$v" 2>&1 | sed 's/^/SPIKE:   sysupdate: /'
        [ "${PIPESTATUS[0]}" -eq 0 ] || fail "systemd-sysupdate update $v failed"
        # sysupdate reports success even when systemd-import wrote the bytes
        # through without decompressing them (it does that for zstd), so check
        # the slot holds a filesystem before trusting the reboot.
        udevadm settle
        local fstype
        fstype=$(blkid -s TYPE -o value "/dev/disk/by-partlabel/moose-spike_$v" 2>/dev/null)
        [ "$fstype" = ext4 ] || fail "the new slot moose-spike_$v holds '$fstype', not ext4"
        say "the new slot moose-spike_$v holds ext4"
    fi
    t1=$(date +%s)
    say "install of v$v took $((t1 - t0))s"
    say "boot state after install:"; bootstate | sed 's/^/SPIKE:   /'
}

# --- data and per-box state ----------------------------------------------------

seed_data() {
    tar -C /usr/lib/spike/tinyroot -c . | docker import - spike/tiny:1 >/dev/null || fail "docker import failed"
    docker volume create spikevol >/dev/null || fail "docker volume create failed"
    docker run --name spike-keep -v spikevol:/v spike/tiny:1 /bin/sh -c 'echo hello-volume > /v/f' \
        || fail "docker run writing the volume failed"
    sqlite3 /var/lib/moose/spike.db "create table t(v text); insert into t values('hello-sqlite');" \
        || fail "sqlite write failed"
    useradd -m -s /bin/bash spikeuser || fail "useradd failed"
    echo 'spikeuser:Spike-pass-1' | chpasswd || fail "chpasswd failed"
    echo hello-home > /home/spikeuser/f
    chown spikeuser: /home/spikeuser/f
    sync
    say "seeded: docker volume spikevol, container spike-keep, /var/lib/moose/spike.db, /home/spikeuser/f"
}

check_data() {
    local where=$1 got
    got=$(docker run --rm -v spikevol:/v spike/tiny:1 /bin/cat /v/f 2>&1) || fail "$where: docker volume read failed: $got"
    [ "$got" = hello-volume ] || fail "$where: docker volume holds '$got'"
    docker container inspect spike-keep >/dev/null 2>&1 || fail "$where: container spike-keep is gone"
    got=$(sqlite3 /var/lib/moose/spike.db "pragma integrity_check; select v from t;" 2>&1 | tr '\n' ' ')
    [ "$got" = "ok hello-sqlite " ] || fail "$where: sqlite says '$got'"
    got=$(cat /home/spikeuser/f 2>&1)
    [ "$got" = hello-home ] || fail "$where: /home file holds '$got'"
    [ "$(stat -c %U /home/spikeuser/f)" = spikeuser ] || fail "$where: /home file lost its owner"
    say "$where: proof 4 data intact (docker volume + container, sqlite, /home)"
}

identity() {
    echo "shadow=$(grep '^spikeuser:' /etc/shadow | sha256sum | cut -c1-16)"
    echo "machine_id=$(cat /etc/machine-id)"
    echo "ssh_ed25519=$(ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub | awk '{print $2}')"
    echo "subuid=$(grep '^moose-remap:' /etc/subuid)"
    echo "docker_root=$(docker info --format '{{.DockerRootDir}}')"
    echo "docker_userns=$(docker info --format '{{.SecurityOptions}}' | grep -o 'name=userns' || echo off)"
}

check_identity() {
    local where=$1
    identity > "$STATE/identity.now"
    if ! diff -u "$STATE/identity.v1" "$STATE/identity.now" | sed 's/^/SPIKE:   /'; then
        fail "$where: per-box state changed across the slot swap"
    fi
    say "$where: per-box state intact: $(tr '\n' ' ' < "$STATE/identity.now")"
    say "$where: this image ships daemon.json as: $(tr -d ' \n' < /usr/lib/spike/daemon.json.image)"
    say "$where: the box runs daemon.json as:     $(tr -d ' \n' < /etc/docker/daemon.json)"
    # Information, not a pass/fail proof: how a new image's users reach a box
    # whose /etc/passwd is already in the overlay's upper layer.
    say "$where: info: sysusers.d user spikev2 from the new image: $(getent passwd spikev2 >/dev/null && echo present || echo missing)"
    say "$where: info: useradd-in-build user spikev2old from the new image: $(getent passwd spikev2old >/dev/null && echo present || echo missing)"
}

# --- the stages ----------------------------------------------------------------

boots=$(( $(cat "$STATE/boots" 2>/dev/null || echo 0) + 1 ))
echo "$boots" > "$STATE/boots"
stage=$(cat "$STATE/stage" 2>/dev/null || echo start)
v=$(version)
say "boot $boots: engine=$ENGINE version=$v slot=$(slot) stage=$stage firmware=$([ -d /sys/firmware/efi ] && echo uefi || echo bios)"

[ "$boots" -le 8 ] || fail "more than 8 boots, the sequence is looping"

if [ -e /usr/lib/spike/broken ]; then
    say "broken image booted in slot $(slot); the health gate will fail it"
    exit 0
fi

mkdir -p "$CTL"
mountpoint -q "$CTL" || mount -o ro "$(blkid -L SPIKECTL)" "$CTL" || fail "no SPIKECTL disk"

case "$stage" in
start)
    [ "$v" = 1 ] || fail "first boot runs version $v, not 1"
    parts=$(lsblk -lno PARTLABEL "$(lsblk -no PKNAME "$(findmnt -no SOURCE /)" | sed 's|^|/dev/|')" | grep -c .)
    data_bytes=$(blockdev --getsize64 "$(findmnt -no SOURCE /data)")
    say "proof 1: $parts partitions, data partition $((data_bytes / 1024 / 1024)) MiB"
    [ "$parts" -eq 5 ] || fail "proof 1: expected 5 partitions after first boot, found $parts"
    [ "$data_bytes" -gt $((8 * 1024 * 1024 * 1024)) ] || fail "proof 1: data partition did not grow"
    say "proof 1: slot use: $(df -BM --output=used,size / | tail -1) (used, size)"
    diag
    say "proof 1 PASS: slot B and the data partition were made at first boot"
    seed_data
    identity > "$STATE/identity.v1"
    say "per-box state at v1: $(tr '\n' ' ' < "$STATE/identity.v1")"
    echo "$(slot)" > "$STATE/slot.v1"
    install_version 2
    echo expect-2 > "$STATE/stage"
    sync
    say "rebooting into v2"
    systemctl reboot
    ;;
expect-2)
    [ "$v" = 2 ] || fail "proof 2: expected version 2 after the update, running $v"
    [ "$(slot)" != "$(cat "$STATE/slot.v1")" ] || fail "proof 2: v2 runs in the same slot as v1"
    say "proof 2 PASS: v2 runs from slot $(slot)"
    check_data "after update"
    check_identity "after update"
    say "boot state with v2 good:"; bootstate | sed 's/^/SPIKE:   /'
    echo "$(slot)" > "$STATE/slot.v2"
    install_version 3
    echo expect-revert > "$STATE/stage"
    sync
    say "rebooting into the broken v3"
    systemctl reboot
    ;;
expect-revert)
    [ "$v" = 2 ] || fail "proof 3: expected the revert to v2, running $v"
    [ "$(slot)" = "$(cat "$STATE/slot.v2")" ] || fail "proof 3: running v2 from an unexpected slot $(slot)"
    [ -s "$STATE/broken-boots" ] || fail "proof 3: the broken v3 never booted"
    say "the broken v3 booted $(wc -l < "$STATE/broken-boots") time(s) before the fallback"
    say "boot state after the revert:"; bootstate | sed 's/^/SPIKE:   /'
    say "proof 3 PASS: the broken v3 booted, failed its health check, and the box came back to v2 on its own"
    check_data "after revert"
    check_identity "after revert"
    pass
    ;;
*)
    fail "unknown stage $stage"
    ;;
esac
