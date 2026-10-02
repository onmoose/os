#!/bin/bash
# Cloud boot-proof in-VM assertions (C2, #205; seed/gate scenarios C3a, #220).
# Baked into the boot-proof image at /usr/local/bin/cloud-assertions.sh and run on
# each boot by moose-cloud-assertions.service. Writes a single verdict line to the
# serial console for dev/cloud/run-cloud-tests.sh to grep:
#
#     MOOSE_CLOUD_ASSERTIONS: PASS
#     MOOSE_CLOUD_ASSERTIONS: FAIL: <reason>
#
# The cloud analogue of dev/test-qemu/medium-assertions.sh. Every boot first does
# the control-plane-up proof (systemd userspace up with no failed units, PSI live,
# the baked control-plane images loaded, the four containers running, the dashboard
# + /api answering through Caddy), then asserts the hosted portal-to-box SSO gate
# (#275; ENVIRONMENT.md # Admin bootstrap — as built) for the scenario the harness
# selected via the moose.assert credential. This box-only lane has no portal private
# key, so it asserts the gate's negative properties (the verifier is armed and
# refuses what it should); the positive owner-create + wizard path needs a real
# assertion and is the joint cloud on-ramp acceptance (cloud docs/ops/e2e-onramp.md):
#
#     unseeded         no seed → no verification key → GET /_moose/sso ⇒ 503;
#                      POST /setup ⇒ 403 (disabled on hosted)
#     seeded           seed on disk → key ingested → a bad/unsigned token on
#                      GET /_moose/sso ⇒ 401 (verifier armed); /setup ⇒ 403; the
#                      brain logged 'provisioning seed ingested'
#     frozen:<box-id>  reboot with a DIFFERENT seed → the dashboard + /api still
#                      serve under the ORIGINAL <box-id> (Caddy route unchanged ⇒
#                      identity frozen), and the brain does NOT re-ingest the seed
#     access           a valid owner assertion → session → per-app forward-auth
#                      access modes end-to-end through real Caddy (#308)
#     ssh              the per-account SSH opt-in against a REAL sshd (#467): :22
#                      closed at boot, a key-less enable refused, the port opening
#                      with the toggle, a real login that a key passes and a
#                      password alone does not, the optional second factor making
#                      the key alone insufficient, the port closing again, and a
#                      deleted account leaving no name and no key file behind
#     update           a real control-plane update and a real failed-update-then-
#                      revert, pulled by digest from a registry inside the guest
#                      (#382), then the same update again driven by an update-target
#                      source rather than an admin (#401) — including a refusal of an
#                      answer that is not pinned to a digest. The only scenario that
#                      changes the box's images.
#     remap            the user-namespace tiers (#531): one app per tier (default
#                      folderless, default service_user, caps root_setup, default
#                      on the managed Postgres, host folder app), each checked for
#                      its userns mode, capabilities and host data owner, and the
#                      caps-tier app's data kept across a container recreate
#     remap-reboot     the same disk booted again: every check of `remap` again
#                      on what the box brought back, and the caps-tier data kept
#
# On PASS the script powers the box off cleanly (the serial-only analogue of the
# medium lane's SSH `systemctl poweroff`) so the brain's SQLite box-id write flushes
# to the persisted overlay before the harness boots the next scenario.
#
# -u + pipefail but NOT -e: every check is `... || fail`. The unseeded/seeded/frozen
# scenarios only read or probe. The access, ssh and update scenarios do change the
# box — each on its own throwaway overlay — because the thing under test is a
# mutation: an owner session plus an app install (access), accounts and the sshd
# config (ssh), and the box's own control-plane images (update). The remap boots
# share one overlay of their own: the first installs the apps the second checks.
set -uo pipefail

SENTINEL=/dev/console
SEED=/var/lib/moose/seed.json
# Host the dashboard + /api + /setup are served under, resolved per scenario below
# (just before step 7, once json_str is defined). An UNPROVISIONED hosted box has no
# box-id yet, so the brain installs the route under the appliance-style "moose.local"
# apex; a SEEDED/FROZEN box installs it under "<box-id>.onmoose.io" — the apex of
# the box's wildcard cert (C3b, #207). The assertion is a Host-header route match over
# localhost — no DNS/mDNS involved. Default is the unprovisioned host.
DASH_HOST=moose.local
# Which scenario to assert — set by the harness over SMBIOS (ImportCredential=
# moose.assert in the unit). Absent/empty ⇒ unseeded (the bare boot-proof default).
MODE="$(tr -d '\r\n' < "${CREDENTIALS_DIRECTORY:-/nonexistent}/moose.assert" 2>/dev/null || true)"
[ -n "$MODE" ] || MODE=unseeded

emit() { echo "MOOSE_CLOUD_ASSERTIONS: $1" > "$SENTINEL" 2>/dev/null || true; }
# Dump control-plane state to the serial console on failure — the brain's
# EnsureControlPlane error lives in its container log, which isn't otherwise on
# the serial the harness captures (mirrors the medium lane's install_diag).
diag() {
    {
        echo "=== MOOSE_CLOUD_DIAG ==="
        echo "-- docker ps -a --"
        docker ps -a --format '{{.Names}}\t{{.Status}}\t{{.Image}}' 2>&1
        echo "-- docker network ls --"
        docker network ls 2>&1
        echo "-- moose-ingress containers --"
        docker network inspect moose-ingress --format '{{range .Containers}}{{.Name}}={{.IPv4Address}} {{end}}' 2>&1
        echo "-- brain networks --"
        docker inspect moose-brain --format '{{json .NetworkSettings.Networks}}' 2>&1
        echo "-- proxy networks --"
        docker inspect moose-docker-proxy --format '{{json .NetworkSettings.Networks}}' 2>&1
        echo "-- forwarding sysctls --"
        echo "ip_forward=$(cat /proc/sys/net/ipv4/ip_forward 2>&1) bridge-nf-call-iptables=$(cat /proc/sys/net/bridge/bridge-nf-call-iptables 2>/dev/null || echo '<module not loaded>')"
        echo "-- docker info (firewall backend / warnings) --"
        docker info 2>&1 | grep -iE "firewall|iptables|nftables|warning|cgroup version" | head
        echo "-- iptables-save (full ruleset) --"
        iptables-save 2>&1
        echo "-- brain netns -> proxy probe (route/neigh/tcp from inside the brain's network ns) --"
        bp="$(docker inspect -f '{{.State.Pid}}' moose-brain 2>/dev/null)"
        if [ -n "$bp" ]; then
            nsenter -t "$bp" -n ip route get 172.18.0.2 2>&1
            nsenter -t "$bp" -n ip neigh 2>&1
            nsenter -t "$bp" -n bash -c '(echo >/dev/tcp/172.18.0.2/2375) 2>&1 && echo "tcp 172.18.0.2:2375 OPEN" || echo "tcp 172.18.0.2:2375 FAIL"' 2>&1
        fi
        echo "-- proxy netns (eth0 up? ip? neigh?) --"
        pp="$(docker inspect -f '{{.State.Pid}}' moose-docker-proxy 2>/dev/null)"
        if [ -n "$pp" ]; then
            nsenter -t "$pp" -n ip -br addr 2>&1
            nsenter -t "$pp" -n ip -br link 2>&1
            nsenter -t "$pp" -n ip neigh 2>&1
            nsenter -t "$pp" -n bash -c '(echo >/dev/tcp/172.18.0.3/8080) 2>&1 && echo "proxy->brain:8080 OPEN" || echo "proxy->brain:8080 FAIL"' 2>&1
        fi
        echo "-- host bridge state (ports / fdb) --"
        ip -br link 2>&1 | grep -E 'br-|docker0|veth' || true
        bridge link show 2>&1 || true
        bridge fdb show 2>&1 | grep -E 'br-' | head -20 || true
        echo "-- networkd view of docker links (should be 'unmanaged') --"
        networkctl list 2>&1 | grep -iE 'docker|veth|br-|IDX' || true
        echo "-- loaded netfilter/bridge modules (/proc/modules) --"
        grep -iE 'br_netfilter|nf_conntrack|nf_nat|^bridge |^veth |iptable|nft|overlay' /proc/modules 2>&1 || echo "(none matched)"
        echo "-- proxy logs (tail 15) --"
        docker logs moose-docker-proxy 2>&1 | tail -15
        echo "-- moose-brain logs (tail 40) --"
        docker logs moose-brain 2>&1 | tail -40
        echo "-- moose-brain resolved profile (grep, not tail) --"
        docker logs moose-brain 2>&1 | grep -iE 'environment profile resolved|provisioning seed|SSO stays closed' || echo "(no profile line in brain log)"
        echo "-- moose-brain mounts (is /etc/moose/profile bind-mounted?) --"
        docker inspect moose-brain --format '{{range .Mounts}}{{.Source}} -> {{.Destination}}{{println}}{{end}}' 2>&1
        echo "-- host-agent journal (tail 15) --"
        journalctl -u host-agent.service -b --no-pager 2>&1 | tail -15
        # The update boot (#382) fails on state that lives in files and in
        # host-agent's log, neither of which the blocks above show. A red update
        # boot has to be diagnosable from this serial dump alone.
        echo "-- control-plane declaration (images.json) --"
        cat /var/lib/moose/control-plane/images.json 2>&1 || true
        echo "-- control-plane compose (image lines) --"
        grep -n 'image:' /var/lib/moose/control-plane/compose.yml 2>&1 || true
        echo "-- brain snapshots --"
        ls -la /var/lib/moose/brain-snapshots 2>&1 || true
        echo "-- host-agent update lines --"
        journalctl -u host-agent.service -b --no-pager 2>&1 | grep -iE 'system-update|control plane|revert|pull|snapshot' | tail -25
        echo "=== END MOOSE_CLOUD_DIAG ==="
    } > "$SENTINEL" 2>&1 || true
}
fail() {
    echo "cloud-assertions FAIL: $*" >&2
    diag
    emit "FAIL: $*"
    # No poweroff on failure: leave the VM up so run-cloud-tests.sh can scrape the
    # serial diag, then kill it and keep the run artifacts.
    exit 1
}
ok() {
    # Last gate, every boot (#561): the slot is read-only, so anything that
    # still writes to it fails. Catch it whether it failed a unit or only
    # logged. Container logs are left out: an app's own read-only filesystem
    # is not the slot.
    failed_units="$(systemctl list-units --state=failed --no-legend --plain 2>/dev/null | awk '{print $1}' | tr '\n' ' ')"
    [ -z "${failed_units// /}" ] || fail "failed units at the end of the boot: $failed_units"
    rofs="$(journalctl -b --no-pager -o json 2>/dev/null | grep -v '"CONTAINER_NAME"' | grep -i 'read-only file system' | head -n 5 | cut -c1-600)"
    [ -z "$rofs" ] || fail "something tried to write to the read-only slot: $rofs"
    emit "PASS"
    # Clean poweroff so the brain's SQLite writes (the persisted box-id) flush to
    # the qcow2 overlay before the harness boots the next scenario over it. --no-block
    # so this oneshot's ExecStart returns; systemd then runs an orderly shutdown.
    systemctl --no-block poweroff 2>/dev/null || true
    exit 0
}

echo "cloud-assertions: starting boot-proof checks (mode=${MODE})"

# The slot GRUB booted (#563). Every boot but the OS update boots runs on slot A;
# those two move the box between slots, one stage per boot, and keep the stage
# on the state partition.
BOOTED="$(sed -n 's/.*rauc\.slot=\([AB]\).*/\1/p' /proc/cmdline)"
OS_STATE_DIR=/var/lib/moose/test/os
os_stage() { cat "$OS_STATE_DIR/stage" 2>/dev/null || echo 1; }
set_os_stage() { mkdir -p "$OS_STATE_DIR" && echo "$1" > "$OS_STATE_DIR/stage" && sync; }
case "$MODE" in
os-update|os-revert) ;;
*) [ "$BOOTED" = A ] || fail "layout: booted slot '$BOOTED', want A (only the OS update boots leave slot A)" ;;
esac

# The broken slot of the os-revert boot (#563). host-agent is kept from starting
# on it (a drop-in the first stage planted), which is the case where nothing but
# the image's own safety net can take the box back. So this stage checks only
# that, and waits: every other check of this script would fail on a box with no
# host-agent, which is the point of the scenario. moose-os-trial.timer (3 min in
# this image) must reboot the box; the next stage runs on slot A.
if [ "$MODE" = os-revert ] && [ "$(os_stage)" = 2 ]; then
    [ "$BOOTED" = B ] || fail "os-revert: stage 2 should boot slot B, booted '$BOOTED' (did GRUB skip the new slot?)"
    [ -e /var/lib/moose/os-update/trial-B ] || fail "os-revert: no trial marker for slot B on its trial boot"
    for _ in $(seq 1 20); do systemctl is-active -q host-agent.service && break; sleep 1; done
    systemctl is-active -q host-agent.service && fail "os-revert: host-agent runs on the slot it was meant to be kept off"
    systemctl list-timers --all --no-pager 2>/dev/null | grep -q moose-os-trial.timer || fail "os-revert: moose-os-trial.timer is not scheduled on the trial boot"
    echo "cloud-assertions: os-revert: on slot B, host-agent cannot start, trial marker present; waiting for the safety net to reboot the box"
    set_os_stage 3
    sleep 900
    fail "os-revert: the safety net never rebooted the box off the broken slot"
fi

# --- 1. no control-plane unit has failed.
# NOTE: we deliberately do NOT gate on `systemctl is-system-running == running`:
# this script runs as a boot-transaction unit (WantedBy=multi-user.target), so
# the system stays 'starting' until the script itself finishes — gating on
# 'running' here would self-deadlock. The concrete per-unit / container / HTTP
# checks below are the real control-plane-up proof. This step is the early
# fast-fail: the unit is ordered After the control-plane units, so any that died
# during boot is already 'failed' by now.
failed="$(systemctl list-units --state=failed --no-legend --plain 2>/dev/null | awk '{print $1}')"
for u in docker.service systemd-networkd.service host-agent.service moose-load-images.service; do
    grep -qx "$u" <<<"$failed" && fail "control-plane unit failed: $u (failed: $(tr '\n' ' ' <<<"$failed"))"
done

# --- 1b. the A/B layout (#561, BUILD.md # 1b). Every boot, both firmwares.
# The image carries 3 partitions (ESP 128 MiB, BIOS boot, slot A: a 1 GiB
# squashfs); the initramfs (state-setup, via the
# initramfs-tools hook) made slot B and the state partition at first boot,
# grew the state partition to fill the disk (the harness gives the box a disk
# far bigger than the image), and set up the /etc overlay, the pinned files and
# the bind mounts before systemd started. The slot is read-only.
layout_fail() { fail "layout: $*"; }
root_src="$(findmnt -no SOURCE / 2>/dev/null)"
root_disk="/dev/$(lsblk -no PKNAME "$root_src" 2>/dev/null | head -n1)"
[ -b "$root_disk" ] || layout_fail "cannot find the disk under / (source '$root_src')"
nparts="$(lsblk -nr -o TYPE "$root_disk" | grep -c '^part$')"
[ "$nparts" = 5 ] || layout_fail "$root_disk has $nparts partitions, want 5 (ESP, BIOS boot, slot A, slot B, state): $(lsblk -nr -o NAME,SIZE,PARTLABEL "$root_disk" | tr '\n' ';')"
for pair in "1:esp" "3:moose-slot-a" "4:moose-slot-b" "5:moose-state"; do
    n="${pair%%:*}"; want="${pair#*:}"
    got="$(lsblk -nr -o PARTN,PARTLABEL "$root_disk" | awk -v n="$n" '$1==n{print $2}')"
    [ "$got" = "$want" ] || layout_fail "partition $n is labelled '$got', want '$want'"
done
esp_bytes="$(lsblk -bnr -o PARTN,SIZE "$root_disk" | awk '$1==1{print $2}')"
slot_a_bytes="$(lsblk -bnr -o PARTN,SIZE "$root_disk" | awk '$1==3{print $2}')"
slot_b_bytes="$(lsblk -bnr -o PARTN,SIZE "$root_disk" | awk '$1==4{print $2}')"
[ "$esp_bytes" = 134217728 ] || layout_fail "the ESP is $esp_bytes bytes, want 128 MiB"
[ "$slot_a_bytes" = 1073741824 ] && [ "$slot_b_bytes" = 1073741824 ] \
    || layout_fail "slots are A=$slot_a_bytes B=$slot_b_bytes bytes, want 1 GiB each"
if [ "$BOOTED" = B ]; then booted_uuid=21212121-2121-4121-8121-212121212121; booted_idx=1; else booted_uuid=20202020-2020-4020-8020-202020202020; booted_idx=0; fi
[ "$root_src" = "/dev/disk/by-partuuid/$booted_uuid" ] || [ "$(lsblk -no PARTUUID "$root_src")" = "$booted_uuid" ] \
    || layout_fail "/ is $root_src, not slot $BOOTED"
# Grown: the state partition reaches the end of the disk, and its ext4 fills it.
disk_bytes="$(lsblk -bdn -o SIZE "$root_disk")"
state_dev="$(findmnt -no SOURCE /state 2>/dev/null)"
[ -n "$state_dev" ] || layout_fail "/state is not mounted"
state_part_bytes="$(lsblk -bdn -o SIZE "$state_dev")"
state_fs_bytes="$(df -B1 --output=size /state | tail -n1 | tr -d ' ')"
# ESP 128 MiB + BIOS 1 MiB + two 1 GiB slots + GPT and alignment.
used_before_state=$(( (128 + 1 + 2048 + 4) * 1024 * 1024 ))
[ "$state_part_bytes" -ge $(( disk_bytes - used_before_state )) ] \
    || layout_fail "state partition is $state_part_bytes bytes on a $disk_bytes byte disk: it did not grow to the end"
# ext4 reserves its metadata and journal, so the filesystem is a little smaller.
[ "$state_fs_bytes" -ge $(( state_part_bytes * 95 / 100 )) ] \
    || layout_fail "state ext4 is $state_fs_bytes bytes in a $state_part_bytes byte partition: it was not grown"
echo "cloud-assertions: layout: $nparts partitions on $root_disk (${disk_bytes} bytes), ESP 128 MiB, slots 1 GiB each, state partition $state_dev ${state_part_bytes} bytes with ext4 ${state_fs_bytes} bytes"
# The slot is a read-only squashfs, and it is the slot GRUB chose.
findmnt -no OPTIONS / | tr ',' '\n' | grep -qx ro || layout_fail "/ is not mounted read-only: $(findmnt -no OPTIONS /)"
[ "$(findmnt -no FSTYPE /)" = squashfs ] || layout_fail "/ is $(findmnt -no FSTYPE /), want squashfs"
# A squashfs keeps file owners as the build saw them. If the build had mapped
# them wrong, sudo would be neither root's nor setuid.
[ "$(stat -c '%u %a' /usr/bin/sudo)" = "0 4755" ] || layout_fail "/usr/bin/sudo is $(stat -c '%u %a' /usr/bin/sudo), want root-owned 4755"
# The measured numbers for the disk budget (BUILD.md # 1b # Disk budget): the
# squashfs's own size from its superblock (bytes_used, offset 40), the ESP's
# use, and the state partition's use at this point of the boot.
slot_a_dev=/dev/disk/by-partuuid/20202020-2020-4020-8020-202020202020
sq_bytes="$(dd if="$slot_a_dev" bs=96 count=1 2>/dev/null | od -An -t u8 -j 40 -N 8 | tr -d ' ')"
[ -n "$sq_bytes" ] && [ "$sq_bytes" -gt 0 ] || layout_fail "cannot read the squashfs size from slot A's superblock"
esp_used="$(df -B1 --output=used /efi | tail -n1 | tr -d ' ')"
state_used="$(df -B1 --output=used /state | tail -n1 | tr -d ' ')"
echo "cloud-assertions: layout: measured: squashfs ${sq_bytes} bytes ($(( sq_bytes * 1000 / slot_a_bytes / 10 )).$(( sq_bytes * 1000 / slot_a_bytes % 10 ))% of the slot), ESP used ${esp_used} of ${esp_bytes} bytes, state partition used ${state_used} of ${state_fs_bytes} bytes"
cmdline="$(cat /proc/cmdline)"
for w in "rauc.slot=$BOOTED" panic=10 ro psi=1 BOOT_IMAGE=/usr/lib/moose/boot/vmlinuz; do
    grep -qw -- "$w" <<<"$cmdline" || layout_fail "kernel command line lacks '$w': $cmdline"
done
echo "cloud-assertions: layout: / is slot $BOOTED read-only, booted by GRUB from the slot's own kernel ($cmdline)"
# The slot's initramfs can find a provider's disk. Hetzner Cloud presents the
# boot disk as virtio-SCSI, which needs virtio_scsi and the SCSI disk driver
# sd_mod; without sd_mod no /dev/sda appears and the boot hangs in the
# initramfs (diagnosed once on a live Hetzner box, when mkosi built the
# initrd). Debian's initramfs-tools (MODULES=most) carries them; this lane
# boots virtio-blk, so only this check would notice them gone.
initrd_mods="$(lsinitramfs /usr/lib/moose/boot/initrd.img 2>/dev/null)" || layout_fail "lsinitramfs cannot read /usr/lib/moose/boot/initrd.img"
for m in sd_mod virtio_scsi virtio_blk squashfs overlay ext4; do
    grep -qE "/${m}\.ko(\.[a-z]+)?$" <<<"$initrd_mods" || layout_fail "the slot's initramfs has no ${m} module"
done
echo "cloud-assertions: layout: the slot's initramfs carries sd_mod, virtio_scsi, virtio_blk, squashfs, overlay and ext4"
# /etc is an overlay with its upper layer on the state partition.
etc_opts="$(findmnt -no FSTYPE,OPTIONS /etc)"
grep -q '^overlay' <<<"$etc_opts" && grep -q 'upperdir=/state/etc/upper' <<<"$etc_opts" \
    || layout_fail "/etc is not the overlay on /state: $etc_opts"
# Every bind mount is the state partition.
state_devno="$(stat -c %d /state)"
for d in /home /var/lib/docker /var/lib/containerd /var/lib/moose /var/log /var/lib/systemd /var/lib/rauc /var/lib/sudo /srv/moose; do
    [ "$(stat -c %d "$d")" = "$state_devno" ] || layout_fail "$d is not on the state partition ($(findmnt -no SOURCE,TARGET "$d" | tr '\n' ' '))"
done
# User files live under /srv/moose, as on the appliance; the databases have
# their own bind mounts and never sit under it (ENVIRONMENT.md # Storage
# (hosted)).
findmnt -no SOURCE /home | grep -q '\[/srv/moose/home\]$' || layout_fail "/home is not bound from the state partition's srv/moose/home: $(findmnt -no SOURCE /home)"
findmnt -no SOURCE /var/lib/moose | grep -q '\[/var/lib/moose\]$' || layout_fail "/var/lib/moose is not its own bind mount: $(findmnt -no SOURCE /var/lib/moose)"
findmnt -no FSTYPE /var/tmp | grep -qx tmpfs || layout_fail "/var/tmp is not a tmpfs"
echo "cloud-assertions: layout: /etc overlay and the bind mounts are on the state partition, /var/tmp is a tmpfs"
# The per-box state is on the state partition: the four pinned files (the
# userns-remap), the SSH host keys and machine-id, all in the upper layer.
up=/state/etc/upper
for f in docker/daemon.json subuid subgid login.defs; do
    [ -s "$up/$f" ] || layout_fail "pinned file /etc/$f is not in the upper layer"
done
grep -q '"userns-remap": "moose-remap"' "$up/docker/daemon.json" || layout_fail "pinned daemon.json lost the remap"
grep -q '^moose-remap:' "$up/subuid" && grep -q '^moose-remap:' "$up/subgid" || layout_fail "pinned subuid/subgid lost the moose-remap range"
[ -e /state/etc/.moose-pinned ] || layout_fail "no pin marker on the state partition"
for _ in $(seq 1 30); do [ -s "$up/ssh/ssh_host_ed25519_key" ] && break; sleep 1; done
for k in ssh_host_ed25519_key ssh_host_ecdsa_key ssh_host_rsa_key; do
    [ -s "$up/ssh/$k" ] || layout_fail "SSH host key $k is not on the state partition"
done
[ -s "$up/machine-id" ] && [ "$(cat "$up/machine-id")" = "$(cat /etc/machine-id)" ] \
    || layout_fail "machine-id is not on the state partition (upper '$(cat "$up/machine-id" 2>/dev/null)', /etc '$(cat /etc/machine-id)')"
echo "cloud-assertions: layout: pinned daemon.json/subuid/subgid/login.defs, SSH host keys and machine-id ($(cat /etc/machine-id)) are on the state partition"
# The bootloader side: RAUC reads the slot config and the grubenv, and the
# box reboots rather than waits in emergency or rescue mode.
rauc_out="$(rauc status 2>&1)" || layout_fail "rauc status failed: $(tail -n3 <<<"$rauc_out" | tr '\n' ' ')"
grep -qi "booted from: *rootfs.${booted_idx} (${BOOTED})" <<<"$rauc_out" || layout_fail "rauc does not see slot $BOOTED as booted: $(tr '\n' ' ' <<<"$rauc_out")"
# The keyring a bundle must chain to (#562) comes from the slot, never from the
# state partition: a new image's keyring must reach the box, so the box never
# writes it into the /etc upper layer.
[ -s /etc/rauc/keyring.pem ] || layout_fail "no /etc/rauc/keyring.pem: rauc install would refuse every bundle"
[ ! -e "$up/rauc/keyring.pem" ] || layout_fail "/etc/rauc/keyring.pem is in the /etc upper layer, so it no longer follows the image"
grep -qx 'check-purpose=codesign' /etc/rauc/system.conf || layout_fail "/etc/rauc/system.conf does not ask for check-purpose=codesign"
echo "cloud-assertions: layout: rauc keyring from the slot: $(openssl x509 -in /etc/rauc/keyring.pem -noout -subject 2>/dev/null || head -c 40 /etc/rauc/keyring.pem)"
grub-editenv /efi/grub/grubenv list | grep -qx "${BOOTED}_OK=1" || layout_fail "grubenv does not mark slot $BOOTED good: $(grub-editenv /efi/grub/grubenv list 2>&1 | tr '\n' ' ')"
for u in emergency.service rescue.service; do
    systemctl cat "$u" 2>/dev/null | grep -q 'systemctl --no-block reboot' || layout_fail "$u has no reboot drop-in"
done
dmesg 2>/dev/null | grep -q 'moose-state: bind mounts done' || journalctl -k -b --no-pager 2>/dev/null | grep -q 'moose-state: bind mounts done' \
    || layout_fail "no 'moose-state: bind mounts done' in the kernel log"
# state-setup looked for the state partition on the boot disk only.
journalctl -k -b --no-pager 2>/dev/null | grep -q "moose-state: boot disk is $root_disk " \
    || dmesg 2>/dev/null | grep -q "moose-state: boot disk is $root_disk " \
    || layout_fail "state-setup did not report $root_disk as the boot disk"
[ "$(lsblk -no PKNAME "$state_dev")" = "$(basename "$root_disk")" ] || layout_fail "the state partition $state_dev is not on the boot disk $root_disk"
echo "cloud-assertions: layout: rauc sees slot $BOOTED booted, grubenv has ${BOOTED}_OK=1, emergency and rescue reboot"

# --- 1c. the baked host-agent carries a real build stamp (BUILD.md # Versioning:
# "every build stamps two fields"). An unstamped build reports internal/version's
# "dev" default; the brain's minimumAgentVersion check hands that to semver, an
# unparseable core sorts before every valid version, and the box raises
# version-mismatch — blocking app installs on a box that is otherwise perfectly
# healthy. v0.4.0 shipped exactly that: stage-control-plane.sh built the agent
# without the Makefile's -ldflags, so the image's agent reported "dev".
#
# This has to be asserted on a BUILT IMAGE, which makes this lane the only place
# it can be caught. No unit test can: the stamp is applied by the build command,
# so a `go test` binary is unstamped by construction and asserting anything about
# version.Version in one only ever pins the default. The release workflow's
# tag-vs-VERSION assert doesn't reach it either — it checks the file, not what
# landed in the binary.
ha_version="$(/usr/lib/moose/host-agent-real --version 2>&1 || true)"
grep -qE '^moose [0-9]+\.[0-9]+\.[0-9]+ ' <<<"$ha_version" || \
    fail "baked host-agent is not version-stamped: --version reports '$ha_version' (want 'moose X.Y.Z (g<sha>)'; an unstamped 'dev' build raises version-mismatch and blocks app installs on a healthy box)"
echo "cloud-assertions: host-agent build stamp ok ($ha_version)"

# --- 2. PSI is live (BUILD.md # 1 — psi=1 on the cmdline). Without it the
# ram-pressure health detector silently reads zeros; a boot test must catch that.
# NB: read the CONTENT — /proc/pressure/memory reports st_size=0 like most proc
# files, so `test -s` always sees it as empty even when PSI is active. When PSI is
# OFF the file does not exist (the directory is absent), so cat fails / is empty.
psi_mem="$(cat /proc/pressure/memory 2>/dev/null || true)"
[ -n "$psi_mem" ] || fail "/proc/pressure/memory unreadable/empty — PSI not active (psi=1 missing?)"
grep -q '^some ' <<<"$psi_mem" || fail "/proc/pressure/memory malformed: $psi_mem"

# --- 3. the single NIC came up via systemd-networkd DHCP (no NetworkManager).
command -v nmcli >/dev/null 2>&1 && fail "NetworkManager present — hosted must bring the NIC up via networkd only"
nwd_state="$(systemctl is-active systemd-networkd.service 2>&1 || true)"
[ "$nwd_state" = active ] || fail "systemd-networkd is '$nwd_state' (want active)"

# --- 4. docker up and the four control-plane images loaded from the baked bundle.
docker_state="$(systemctl is-active docker.service 2>&1 || true)"
[ "$docker_state" = active ] || fail "docker.service is '$docker_state' (want active)"
for _i in $(seq 1 60); do
    [ -f /var/lib/moose/.control-plane-images-loaded ] && break
    systemctl is-failed --quiet moose-load-images.service && \
        fail "moose-load-images.service failed: $(journalctl -u moose-load-images.service -b --no-pager 2>/dev/null | tail -10)"
    sleep 1
done
[ -f /var/lib/moose/.control-plane-images-loaded ] || fail "control-plane image-load marker never appeared after 60s"
cp_images="$(docker images --format '{{.Repository}}' 2>&1 || true)"
# Hosted bakes the caddy-dns/acmedns Caddy build (moose-caddy-acmedns), not stock
# caddy:2-alpine — the wildcard cert needs the DNS-01 module (os #207/C3b).
for repo in moose-brain moose-ui moose-caddy-acmedns tecnativa/docker-socket-proxy; do
    grep -qx "$repo" <<<"$cp_images" || fail "baked image '$repo' not loaded (have: $(tr '\n' ' ' <<<"$cp_images"))"
done

# --- 5. the brain brought the control plane up: four containers running. The
# brain bootstrap + compose up race this unit, so poll.
want="moose-brain moose-caddy moose-ui moose-docker-proxy"
running=""
for _i in $(seq 1 120); do
    running="$(docker ps --format '{{.Names}}' 2>/dev/null | tr '\n' ' ')"
    miss=0
    for c in $want; do grep -qw "$c" <<<"$running" || miss=1; done
    [ "$miss" = 0 ] && break
    sleep 1
done
for c in $want; do
    grep -qw "$c" <<<"$running" || fail "control-plane container '$c' not running after 120s (have: $running)"
done

# --- 5b. container stdout is readable through journald by CONTAINER_NAME — the
# EXACT query host-agent-real's per-app log tail runs
# (internal/hostagent/journalsource: `journalctl CONTAINER_NAME=<container>`).
# This is deliberately not a `docker logs` read: `docker logs` works on every log
# driver, which is precisely why the driver being wrong went unnoticed and every
# app's Logs tab hung on "Waiting for log output…" on a real box. Assert the
# driver, then assert the query it exists to serve actually returns lines.
log_driver="$(docker info --format '{{.LoggingDriver}}' 2>/dev/null || true)"
[ "$log_driver" = journald ] || \
    fail "docker log driver is '$log_driver' (want journald) — the per-app Logs tab reads journalctl CONTAINER_NAME=, which only the journald driver populates"
# moose-brain is the safe probe: it is up by now (step 5) and always writes
# startup milestones to stdout. Poll — journald ingest can lag container start
# by a beat under a loaded TCG boot, same race wait_brain_log documents.
brain_journal=""
for _i in $(seq 1 60); do
    brain_journal="$(journalctl CONTAINER_NAME=moose-brain -b --no-pager -n 5 -o cat 2>/dev/null || true)"
    [ -n "$brain_journal" ] && break
    sleep 1
done
[ -n "$brain_journal" ] || \
    fail "journalctl CONTAINER_NAME=moose-brain returned nothing after 60s — container stdout is not reaching journald, so the per-app Logs tab will hang for every app"
echo "cloud-assertions: container logs readable via journalctl CONTAINER_NAME= (driver=journald)"

# --- 5c. the control-plane containers run the app sandbox (#431). Apps get
# cap_drop ALL + no-new-privileges from the brain's override, in code; the
# control plane declares the same posture by hand (compose for caddy + moose-ui,
# brainlaunch.proxyRunSpec for the proxy), so this checks what the box actually
# booted rather than what the file says. Caddy binds :80/:443, so it keeps
# CAP_NET_BIND_SERVICE and nothing else; the proxy needs no capability at all.
# The brain is knowingly absent from this list — it needs CAP_CHOWN for app data
# dirs (CONTROL_PLANE.md # Locked: control-plane container hardening).
for c in moose-caddy moose-ui moose-docker-proxy; do
    caps="$(docker inspect "$c" --format '{{json .HostConfig.CapDrop}}' 2>/dev/null || true)"
    [ "$caps" = '["ALL"]' ] || \
        fail "$c cap_drop is '${caps:-<nothing>}', want [\"ALL\"] (#431 — the control-plane sandbox is gone)"
    secopt="$(docker inspect "$c" --format '{{json .HostConfig.SecurityOpt}}' 2>/dev/null || true)"
    grep -q 'no-new-privileges:true' <<<"$secopt" || \
        fail "$c security_opt is '${secopt:-<nothing>}', want no-new-privileges:true (#431)"
done
for c in moose-caddy moose-ui; do
    ro="$(docker inspect "$c" --format '{{.HostConfig.ReadonlyRootfs}}' 2>/dev/null || true)"
    [ "$ro" = true ] || fail "$c does not have a read-only root filesystem (#431)"
    capadd="$(docker inspect "$c" --format '{{json .HostConfig.CapAdd}}' 2>/dev/null || true)"
    [ "$capadd" = '["NET_BIND_SERVICE"]' ] || [ "$capadd" = '["CAP_NET_BIND_SERVICE"]' ] || \
        fail "$c cap_add is '${capadd:-<nothing>}', want only NET_BIND_SERVICE (#431)"
done
echo "cloud-assertions: control-plane containers sandboxed — cap_drop ALL, no-new-privileges, read-only root on caddy + moose-ui (#431)"

# --- 5d. the daemon runs with the userns-remap (#530, BUILD.md # User-namespace
# remap), and the control plane splits across it. The image sets the remap, so
# every boot runs on it and every boot checks it. docker info must list
# name=userns and the classic overlay2 store (the remap turns the containerd
# store off). The socket proxy and the brain opt out with --userns=host
# (brainlaunch), so they run as real host root. Caddy and moose-ui keep the daemon
# default, the remap, so their root is host uid 1000000.
remap_base=1000000
# The host uid a container's init process runs as (the lean image has no procps).
host_uid_of() { awk '/^Uid:/{print $2}' "/proc/$(docker inspect -f '{{.State.Pid}}' "$1" 2>/dev/null)/status" 2>/dev/null; }
sec_opts="$(docker info --format '{{json .SecurityOptions}}' 2>/dev/null || true)"
store_driver="$(docker info --format '{{.Driver}}' 2>/dev/null || true)"
docker_root="$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)"
echo "cloud-assertions: docker info: security options: $sec_opts"
echo "cloud-assertions: docker info: storage driver: $store_driver, root dir: $docker_root"
grep -q 'name=userns' <<<"$sec_opts" || fail "userns-remap is not on: docker info security options are $sec_opts, want name=userns (#530)"
[ "$store_driver" = overlay2 ] || fail "docker storage driver is '$store_driver', want overlay2 (the store the remap uses, #530)"
[ "$docker_root" = "/var/lib/docker/${remap_base}.${remap_base}" ] \
    || fail "docker root dir is '$docker_root', want /var/lib/docker/${remap_base}.${remap_base} (the remapped root, #530)"
for f in /etc/subuid /etc/subgid; do
    grep -qx "moose-remap:${remap_base}:65536" "$f" || fail "$f has no moose-remap:${remap_base}:65536 line: $(cat "$f" 2>&1 | tr '\n' ' ')"
done
for k in SUB_UID_COUNT SUB_GID_COUNT; do
    grep -qE "^${k}[[:space:]]+0$" /etc/login.defs || fail "/etc/login.defs does not set $k 0: $(grep -E "$k" /etc/login.defs | tr '\n' ' ')"
done
for c in moose-docker-proxy moose-brain; do
    um="$(docker inspect "$c" --format '{{.HostConfig.UsernsMode}}' 2>/dev/null || true)"
    [ "$um" = host ] || fail "$c UsernsMode is '${um:-<empty>}', want host (#526)"
    puid="$(host_uid_of "$c")"
    [ "$puid" = 0 ] || fail "$c runs as host uid '${puid:-<none>}', want 0 (host userns, #526)"
done
for c in moose-caddy moose-ui; do
    um="$(docker inspect "$c" --format '{{.HostConfig.UsernsMode}}' 2>/dev/null || true)"
    [ -z "$um" ] || fail "$c UsernsMode is '$um', want the daemon default (remapped)"
    puid="$(host_uid_of "$c")"
    [ "$puid" = "$remap_base" ] || fail "$c runs as host uid '${puid:-<none>}', want $remap_base (remapped root)"
done
echo "cloud-assertions: userns-remap on (moose-remap:${remap_base}:65536, SUB_UID_COUNT 0); proxy + brain in the host userns (host uid 0), caddy + moose-ui remapped (host uid ${remap_base})"

# --- 6. proxy boundary: the brain reaches Docker only through the socket-proxy,
# never the raw socket (CONTROL_PLANE.md # Docker socket exposure).
brain_sock="$(docker inspect moose-brain --format '{{range .Mounts}}{{println .Source}}{{end}}' 2>/dev/null | grep -c 'docker.sock' || true)"
[ "$brain_sock" = 0 ] || fail "raw docker.sock mounted into moose-brain (proxy boundary breached)"

# --- 6b. metadata SSRF block (#251): forwarded / app-container egress to the cloud
# metadata endpoint (169.254.169.254) is dropped, while the host-root first-boot
# seed fetch (OUTPUT path) is not. The QEMU lane delivers the seed over SMBIOS, so
# there is no real 169.254.169.254 server to positively probe host reachability —
# instead assert the rule's SHAPE (a forward hook, never an output hook, matching
# the metadata IP) plus that a real container packet HITS the drop: probe from
# inside the brain's netns (a genuine forward-path source over moose-ingress) and
# require the drop counter to increment. Together: containers blocked, the host
# OUTPUT path structurally untouched (so the seed fetch still works).
fw_rules="$(nft list table inet moose_metadata 2>/dev/null)" || \
    fail "metadata firewall: nft table 'inet moose_metadata' absent — egress block not loaded (#251; moose-metadata-firewall.service is $(systemctl is-active moose-metadata-firewall.service 2>&1))"
grep -q 'hook forward' <<<"$fw_rules" || \
    fail "metadata firewall: drop chain is not a forward hook (#251) — rules: $(tr '\n' ' ' <<<"$fw_rules")"
grep -q 'hook output' <<<"$fw_rules" && \
    fail "metadata firewall: an output hook is present — would break the host-root first-boot seed fetch (#251)"
grep -q '169\.254\.169\.254' <<<"$fw_rules" || \
    fail "metadata firewall: no rule matches 169.254.169.254 (#251) — rules: $(tr '\n' ' ' <<<"$fw_rules")"

# Drop-counter probe: read packets matched before/after a container-origin connect.
md_packets() { nft list table inet moose_metadata 2>/dev/null | awk '/169\.254\.169\.254/{for(i=1;i<=NF;i++) if($i=="packets") print $(i+1)}' | head -1; }
md_pid="$(docker inspect -f '{{.State.Pid}}' moose-brain 2>/dev/null)"
[ -n "$md_pid" ] || fail "metadata firewall: moose-brain pid not found for the egress probe (#251)"
# The live drop-counter probe needs the HOST to have a route to the metadata IP, so
# the container's forwarded packet is actually routed (and so traverses the forward
# hook) rather than rejected at the routing stage. The host does on a real cloud (it
# reaches 169.254.169.254 to fetch the seed) and under QEMU slirp (DHCP hands out a
# default route that covers it). If a routeless lane ever lacks it, fall back to the
# shape assertions above (rule loaded + forward-only) rather than a false-fail.
if ip route get 169.254.169.254 >/dev/null 2>&1; then
    md_before="$(md_packets)"
    # A DROP gives no RST, so the connect would hang — bound it; the SYN is emitted
    # (and counted) immediately, so 3s is ample. The probe is EXPECTED not to connect.
    # stderr is NOT suppressed so nsenter infrastructure failures (stale PID, permission
    # denied) appear in the serial log and are distinguishable from "DROP working".
    timeout 3 nsenter -t "$md_pid" -n bash -c 'exec 3<>/dev/tcp/169.254.169.254/80' 2>&1 || true
    md_after="$(md_packets)"
    [ -n "$md_before" ] && [ -n "$md_after" ] || fail "metadata firewall: could not read the drop counter (#251)"
    [ "$md_after" -gt "$md_before" ] || \
        fail "metadata firewall: a container probe to 169.254.169.254 did NOT hit the forward DROP (counter $md_before -> $md_after) — SSRF still open (#251)"
    echo "cloud-assertions: metadata SSRF block (#251) — forward-hook DROP loaded; container egress to 169.254.169.254 dropped (counter $md_before -> $md_after)"
else
    echo "cloud-assertions: metadata SSRF block (#251) — forward-hook DROP loaded (shape verified); live drop-probe skipped — host has no route to 169.254.169.254 in this lane"
fi

# HTTP over Caddy :80 via bash /dev/tcp (no curl in the lean image). Same idiom
# as medium-assertions. Prints the status line; HTTP/1.0 + Connection: close so
# the server closes the stream.
http_status() { # PATH HOST -> status line
    exec 3<>/dev/tcp/127.0.0.1/80 || return 1
    printf 'GET %s HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n' "$1" "$2" >&3
    head -1 <&3
    exec 3>&- 3<&-
}
http_post_status() { # PATH HOST JSON -> status line
    local body="$3" len
    len="$(printf '%s' "$body" | wc -c | tr -d ' ')"
    exec 3<>/dev/tcp/127.0.0.1/80 || return 1
    printf 'POST %s HTTP/1.0\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \
        "$1" "$2" "$len" "$body" >&3
    head -1 <&3
    exec 3>&- 3<&-
}
# Full-response HTTP helpers (headers + body) over Caddy :80 — the status-only
# helpers above can't see Set-Cookie / Location / a response body. Used by the
# access boot (cookies, the whoami echo) and the update boot (the job id and the
# job's JSON status). ${N:-} keeps them safe under `set -u` when a cookie arg is
# omitted.
full_get() { # PATH HOST [COOKIE] -> full response
    exec 3<>/dev/tcp/127.0.0.1/80 || return 1
    if [ -n "${3:-}" ]; then
        printf 'GET %s HTTP/1.0\r\nHost: %s\r\nCookie: %s\r\nConnection: close\r\n\r\n' "$1" "$2" "$3" >&3
    else
        printf 'GET %s HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n' "$1" "$2" >&3
    fi
    cat <&3
    exec 3>&- 3<&-
}
# Like full_get, plus one arbitrary extra request header. The path-scoped
# exposure probes (#415) need to send a FORGED X-Moose-User and see what the app
# upstream received, which no cookie-only helper can do.
full_get_hdr() { # PATH HOST HEADER-LINE [COOKIE] -> full response
    exec 3<>/dev/tcp/127.0.0.1/80 || return 1
    if [ -n "${4:-}" ]; then
        printf 'GET %s HTTP/1.0\r\nHost: %s\r\n%s\r\nCookie: %s\r\nConnection: close\r\n\r\n' "$1" "$2" "$3" "$4" >&3
    else
        printf 'GET %s HTTP/1.0\r\nHost: %s\r\n%s\r\nConnection: close\r\n\r\n' "$1" "$2" "$3" >&3
    fi
    cat <&3
    exec 3>&- 3<&-
}
full_send() { # METHOD PATH HOST COOKIE JSON -> full response
    local len; len="$(printf '%s' "$5" | wc -c | tr -d ' ')"
    exec 3<>/dev/tcp/127.0.0.1/80 || return 1
    printf '%s %s HTTP/1.0\r\nHost: %s\r\nCookie: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \
        "$1" "$2" "$3" "$4" "$len" "$5" >&3
    cat <&3
    exec 3>&- 3<&-
}
status_of() { head -1 <<<"$1" | tr -d '\r'; }
# Extract NAME=VALUE from the first Set-Cookie carrying NAME (drops attributes).
cookie_val() { grep -i '^Set-Cookie:' <<<"$1" | grep -oE "$2=[^;[:space:]]+" | head -1; }
# The WHOLE raw Set-Cookie line for NAME, attributes included — cookie_val above
# deliberately drops them, but the Domain attribute is exactly what the two-cookie
# safety model rests on, so it has to be asserted, not just carried.
cookie_line() { grep -i '^Set-Cookie: *'"$2"'=' <<<"$1" | head -1 | tr -d '\r'; }

# Status line from an arbitrary address, not just Caddy on :80 — the update boot
# probes the brain container's own /healthz and the in-guest registry, neither of
# which is reachable through Caddy.
http_status_addr() { # IP PORT PATH -> status line
    exec 3<>"/dev/tcp/$1/$2" || return 1
    printf 'GET %s HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n' "$3" "$1" >&3
    head -1 <&3
    exec 3>&- 3<&-
}

# Extract a JSON string field's value from a compact one-line document. The seed
# the harness generates is compact and its fields (box_id, assertion_verification_key)
# are plain strings with no embedded quotes, so a targeted sed is sufficient (no
# jq in the lean image).
json_str() { # FILE KEY -> value
    sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" "$1" | head -1
}
# Same, over a document already in a variable — an HTTP response, headers and all.
# The update boot reads the job id out of the trigger's 202 body this way.
json_str_of() { # DOC KEY -> value
    sed -n "s/.*\"$2\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" <<<"$1" | head -1
}

# Wait for a line matching a fixed pattern in the brain's container log. The brain
# writes each milestone to stdout ONCE at startup, but `docker logs` reads the
# daemon's json-file, which buffers the container's stream before flushing to disk
# — under a loaded TCG boot that flush can lag the brain's own log timestamp by
# several seconds. A single-shot (or short) grep therefore loses a genuine race:
# the line is emitted but not yet readable (a seeded boot's milestone has been seen
# in the brain log 3s before the check that "failed" to find it). Poll generously.
# The lag is bounded (seconds), so the default 90s window makes a miss effectively
# impossible; the happy path breaks on the first read, so the wide window costs no
# real time. Callers pair this with a deterministic co-signal (serving under the
# box-id host, :443 bound) that already proves the milestone causally happened —
# this only pins that the exact code path logged it. Returns 0 on match.
wait_brain_log() { # pattern [timeout_s]
    local pat="$1" timeout="${2:-90}" _i
    for _i in $(seq 1 "$timeout"); do
        docker logs moose-brain 2>&1 | grep -qF "$pat" && return 0
        sleep 1
    done
    return 1
}

# Resolve the Host the brain actually serves the dashboard under for this scenario
# (see DASH_HOST above). A provisioned box (seeded/frozen) serves at its wildcard apex
# "<box-id>.onmoose.io", not "moose.local" — so steps 7–9 must probe that host or
# Caddy's catch-all answers 404. Seeded
# reads the box-id from the just-materialized seed; frozen uses the persisted identity
# carried in MODE (the brain ignores this boot's re-delivered seed, so the route stays
# under the original box-id).
case "$MODE" in
seeded)   DASH_HOST="$(json_str "$SEED" box_id).onmoose.io" ;;
frozen:*) DASH_HOST="${MODE#frozen:}.onmoose.io" ;;
access)   DASH_HOST="$(json_str "$SEED" box_id).onmoose.io" ;;
update)   DASH_HOST="$(json_str "$SEED" box_id).onmoose.io" ;;
ssh)      DASH_HOST="$(json_str "$SEED" box_id).onmoose.io" ;;
remap|remap-reboot) DASH_HOST="$(json_str "$SEED" box_id).onmoose.io" ;;
os-update|os-revert) DASH_HOST="$(json_str "$SEED" box_id).onmoose.io" ;;
esac
echo "cloud-assertions: probing control plane at Host=$DASH_HOST (mode=$MODE)"

# --- 7. the dashboard SPA answers through Caddy (the control-plane-up proof).
# The brain flips/installs the dashboard route a beat after Caddy comes up, so poll.
spa=""
for _i in $(seq 1 60); do
    spa="$(http_status / "$DASH_HOST" 2>/dev/null || true)"
    grep -q ' 200' <<<"$spa" && break
    sleep 1
done
grep -q ' 200' <<<"$spa" || fail "dashboard SPA not reachable through Caddy: status='$spa'"

# --- 8. /api routes to the brain (not the catch-all). /api/v1/me is a real brain
# route: 200 (with the setup flag) or 401. A 404 = catch-all swallowed it; a 502
# = route installed but the brain's listener isn't up yet, so poll.
api=""
for _i in $(seq 1 60); do
    api="$(http_status /api/v1/me "$DASH_HOST" 2>/dev/null || true)"
    grep -qE ' (200|401)' <<<"$api" && break
    sleep 1
done
grep -qE ' (200|401)' <<<"$api" || fail "/api not routed to the brain through Caddy: status='$api'"

# --- 9. the hosted portal-to-box SSO gate (#275; ENVIRONMENT.md # Admin bootstrap).
# The hosted box bootstraps its first admin through the portal-to-box SSO handshake,
# not a /setup secret. /setup is disabled on hosted, and GET /_moose/sso verifies a
# portal-signed ownership assertion against the seed-delivered verification key.
# For the unseeded/seeded/frozen boots this lane has no portal private key, so it
# asserts the *negative* gate properties (the verifier is armed and refuses every
# token it shouldn't accept); the positive path against the REAL production portal —
# owner auto-create → session → wizard — is the joint cloud on-ramp acceptance (cloud
# docs/ops/e2e-onramp.md), not this box-only boot lane. The `access` boot (#308) is
# the deliberate exception: it seeds a *test-portal* key whose private half the
# harness holds, so it drives the positive session path here to prove the per-app
# forward-auth access modes (see the access case below).

# /setup is disabled on every hosted boot (the owner uses SSO): 403, never the
# appliance's open empty-box 200/409. Proof the profile marker reached the container.
# Break only on a definitive brain answer (403, or the appliance-mode 409/200 we
# want to catch below) — NOT on a 502/503. Those are Caddy's "no ready upstream for
# /api" during the first second after the stack comes up (the brain's listener /
# dashboard route land a beat behind the container), a transient this poll must ride
# through exactly as the /api/v1/me poll above does. Breaking on a transient 503 was
# a latent race: the box is correct (the brain returns 403 once its upstream is
# ready), but a probe that caught the startup window failed the proof. A genuinely
# stuck /setup still fails — the loop exhausts its 30s window holding the last 503,
# and the 403 assertion below rejects it.
setup=""
for _i in $(seq 1 30); do
    setup="$(http_post_status /api/v1/setup "$DASH_HOST" \
        '{"display_name":"probe","password":"probe-pw-once"}' 2>/dev/null || true)"
    grep -qE ' (403|409|200)' <<<"$setup" && break
    sleep 1
done
grep -q ' 403' <<<"$setup" || fail "hosted /setup not disabled: status='$setup' (want 403; an appliance-mode brain would 409/200 — profile marker not reaching the container?)"
echo "cloud-assertions: hosted /setup disabled (403 — bootstrap is via SSO)"

case "$MODE" in
unseeded)
    # No seed ingested → no verification key → GET /_moose/sso returns 503, NOT a
    # redirect or a fall-through. Proof the SSO gate stays closed until a seed lands.
    sso="$(http_status '/_moose/sso?token=x.y' "$DASH_HOST" 2>/dev/null || true)"
    grep -q ' 503' <<<"$sso" || fail "unseeded /_moose/sso gate not armed: status='$sso' (want 503, unprovisioned)"
    echo "cloud-assertions: hosted SSO gate armed (503, unprovisioned)"
    ;;
seeded)
    [ -f "$SEED" ] || fail "seeded mode but $SEED absent (seed materializer did not run?)"
    box_id="$(json_str "$SEED" box_id)"
    key="$(json_str "$SEED" assertion_verification_key)"
    [ -n "$box_id" ] && [ -n "$key" ] || fail "could not read box_id/assertion_verification_key from $SEED"

    # The seed's verification key was ingested: GET /_moose/sso now runs the verifier
    # and a syntactically-valid-but-unsigned token fails the signature check → 401
    # (not 503). Proof the key loaded and the verifier is wired on this box. Poll:
    # the route is served (step 8 passed) but the verifier arms a beat behind the
    # listener, so a single-shot read can catch a transient 503 before the key loads.
    sso=""
    for _i in $(seq 1 30); do
        sso="$(http_status '/_moose/sso?token=ZmFrZQ.ZmFrZXNpZw' "$DASH_HOST" 2>/dev/null || true)"
        grep -q ' 401' <<<"$sso" && break
        sleep 1
    done
    grep -q ' 401' <<<"$sso" || fail "seeded /_moose/sso with a bad token: status='$sso' (want 401 — key loaded, signature rejected)"
    echo "cloud-assertions: hosted SSO verifier armed (bad token 401, key loaded from seed; box_id=$box_id)"

    # The synchronous seed ingestion ran before the brain served — in fact it ran
    # before steps 7-8 above could pass: the dashboard + /api answered under
    # DASH_HOST=<box_id>.onmoose.io, and the brain only installs that box-id route
    # AFTER reading the seed and learning its box-id (cmd/brain loadHostedEnvironment).
    # So the milestone has causally already been logged by now; this confirms the
    # exact line was emitted. Use the flush-lag-tolerant waiter — a single-shot grep
    # loses the docker json-log race even though the line is present moments later.
    wait_brain_log 'provisioning seed ingested' || \
        fail "brain did not log 'provisioning seed ingested' on the seeded boot"
    echo "cloud-assertions: seed ingested (box_id=$box_id persisted)"

    # The seed's complete acme-dns enrollment drives the brain's wildcard-TLS pass
    # (cmd/brain EnsureWildcardTLS): it configures Caddy's acme-dns DNS-01 issuer for
    # the apex + "*.$box_id.onmoose.io" and adds the :443 listener. Real issuance
    # can't run here — air-gapped (restrict=on), no reach to acme-dns or Let's Encrypt
    # — so no cert is obtained; what this asserts is that the brain REACHES and APPLIES
    # the config and :443 actually binds. That application is the exact step a booted
    # hosted box was failing (#278: box-id site unrouted, :443 never bound, no wildcard
    # cert), and the air-gapped lane never exercised it before — the prior seed carried
    # no enrollment, so EnsureWildcardTLS was skipped.

    # Two proofs the brain APPLIED the wildcard-TLS config. Order matters: assert the
    # deterministic socket signal FIRST, then the log line. EnsureWildcardTLS binds
    # :443 as part of phase 1 and logs "caddy: wildcard TLS configured" in the same
    # synchronous call, so once :443 is listening the milestone has already been
    # emitted — the log grep is then a same-call confirmation the daemon has had ample
    # time to flush, not a race we start cold.

    # (a) The :443 listener actually bound. A plain TCP connect to Caddy's HTTPS port
    # succeeds even with no cert (the TLS handshake would fail, but the socket is
    # listening) — the ":443 never binds" symptom from #278, asserted positively. Poll:
    # the listener is patched in a beat after the config PUT.
    bound=""
    for _i in $(seq 1 30); do
        if timeout 3 bash -c 'exec 3<>/dev/tcp/127.0.0.1/443' 2>/dev/null; then bound=1; break; fi
        sleep 1
    done
    [ -n "$bound" ] || fail "Caddy :443 listener not bound on the seeded boot (#278 — :443 never came up)"
    echo "cloud-assertions: Caddy :443 listener bound"

    # (b) The brain logged the wildcard-TLS milestone. Flush-lag-tolerant waiter: the
    # line is emitted once during the (now-proven-complete) phase-1 call, and a
    # single-shot grep can still lose the race to the docker json-log flush.
    wait_brain_log 'caddy: wildcard TLS configured' || \
        fail "brain did not configure wildcard TLS on the seeded boot (#278 — EnsureWildcardTLS not reached/applied)"
    echo "cloud-assertions: wildcard TLS configured (acme-dns DNS-01 issuer + :443 set for *.$box_id.onmoose.io)"

    # (c) Caddy's certificate store survives a container recreate (#433). The cert
    # this box would obtain lands in /data; on the writable layer it dies with the
    # container and the box has to place a NEW Let's Encrypt order — not a renewal,
    # so no ARI exemption, and against a "50 new certificates per 7 days" budget
    # that every hosted box shares because they are all under one registered domain
    # (onmoose.io). This lane is air-gapped, so it cannot watch for the absence
    # of an issuance; what it CAN prove is the property that absence rests on — the
    # store is on a named volume with a life of its own, not in the container.
    mount_name="$(docker inspect moose-caddy \
        --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Type}}:{{.Name}}{{end}}{{end}}' 2>/dev/null || true)"
    [ "$mount_name" = "volume:moose-caddy-data" ] || \
        fail "moose-caddy /data is not the moose-caddy-data volume: got '${mount_name:-<nothing>}' (#433 — a recreate would drop the wildcard cert)"
    docker volume inspect moose-caddy-data >/dev/null 2>&1 || \
        fail "docker volume moose-caddy-data does not exist (#433)"

    # The mount is live in both directions, not just declared: a file Caddy writes
    # under /data is visible to a SEPARATE container mounting the same volume, so it
    # outlives this container by construction. Reads the image moose-caddy runs, so
    # nothing is pulled in the air gap.
    caddy_img="$(docker inspect moose-caddy --format '{{.Config.Image}}' 2>/dev/null || true)"
    [ -n "$caddy_img" ] || fail "could not read the moose-caddy image ref (#433 probe)"
    docker exec moose-caddy sh -c 'echo moose-433 > /data/.moose-persist-probe' 2>/dev/null || \
        fail "could not write a probe into moose-caddy /data (#433)"
    probe="$(docker run --rm --entrypoint sh -v moose-caddy-data:/probe "$caddy_img" \
        -c 'cat /probe/.moose-persist-probe' 2>/dev/null || true)"
    docker exec moose-caddy rm -f /data/.moose-persist-probe 2>/dev/null || true
    [ "$probe" = "moose-433" ] || \
        fail "moose-caddy /data writes do not land in the moose-caddy-data volume: probe read back '${probe:-<nothing>}' (#433)"
    echo "cloud-assertions: Caddy cert store on the moose-caddy-data volume, survives a container recreate (#433)"
    ;;
frozen:*)
    expect="${MODE#frozen:}"
    [ -n "$expect" ] || fail "frozen mode missing the expected box-id (MODE='$MODE')"
    # A DIFFERENT seed was delivered this boot, but the brain's identity is frozen in
    # SQLite: it loads the persisted box-id and ignores the new seed. Two proofs that
    # need no admin session:
    #   1. The dashboard + /api checks above ran against DASH_HOST=<expect>.onmoose.io
    #      (the ORIGINAL box-id) and passed — if a re-delivered seed had re-keyed the
    #      box, Caddy's dashboard route would be under this boot's box-id and those
    #      probes would have 404'd. So serving under <expect> *is* the frozen-identity
    #      proof.
    #   2. This boot does NOT re-ingest: the brain loads the persisted identity and
    #      never logs 'provisioning seed ingested' (that line is first-boot-only).
    sso="$(http_status '/_moose/sso?token=ZmFrZQ.ZmFrZXNpZw' "$DASH_HOST" 2>/dev/null || true)"
    grep -q ' 401' <<<"$sso" || fail "frozen mode: /_moose/sso bad token status='$sso' (want 401 — verifier still armed from the persisted key)"
    if docker logs moose-brain 2>&1 | grep -q 'provisioning seed ingested'; then
        fail "frozen mode: brain re-ingested a seed — a re-delivered seed must be ignored on a frozen-identity boot"
    fi
    # Confirm the on-disk seed really is this boot's distinct seed (a no-op overwrite
    # would make the frozen assertion vacuous). A warning, not a failure: the identity
    # proof above is the real signal.
    if [ -f "$SEED" ]; then
        disk_box="$(json_str "$SEED" box_id)"
        [ -n "$disk_box" ] && [ "$disk_box" = "$expect" ] && \
            echo "cloud-assertions: WARN frozen seed.json box_id ($disk_box) == frozen identity — re-delivery not distinct" >&2
    fi
    echo "cloud-assertions: frozen identity held across reboot — served under box_id $expect, re-delivered seed ignored"
    ;;
access)
    # Per-app forward-auth access-mode proof (#308), the positive path the box-only
    # SSO gate above can't reach: it needs a real owner session, so this scenario is
    # seeded with a TEST-PORTAL key (the harness holds the matching private key —
    # dev/cloud/mkassertion) and the harness delivers a valid owner assertion over
    # the moose.sso_token credential. The box verifies it exactly as a real portal
    # assertion, auto-creates the owner, and mints both cookies. We then install a
    # real app and drive every access mode end-to-end through the box's own Caddy:
    #   - restricted (the hosted default): unauthenticated ⇒ 302 to the box login;
    #     the owner's forward-auth cookie ⇒ proxied through with no second login;
    #   - public (after the exposure toggle): reachable with no session;
    #   - moose_forward_auth never reaches the app upstream in EITHER mode, while an
    #     app's own cookie DOES (#335's per-cookie strip — the whole-header delete it
    #     replaced made every third-party app with a browser login unusable, #306).
    [ -f "$SEED" ] || fail "access mode but $SEED absent (seed materializer did not run?)"
    box_id="$(json_str "$SEED" box_id)"
    [ -n "$box_id" ] || fail "access mode: could not read box_id from $SEED"
    apex="${box_id}.onmoose.io"
    app_host="whoami.${apex}"

    # The signed owner assertion the harness minted with the test-portal private key,
    # delivered over SMBIOS (ImportCredential=moose.sso_token in the unit).
    sso_token="$(tr -d '\r\n' < "${CREDENTIALS_DIRECTORY:-/nonexistent}/moose.sso_token" 2>/dev/null || true)"
    [ -n "$sso_token" ] || fail "access mode: moose.sso_token credential missing (harness did not mint/deliver the owner assertion)"

    # The full-response HTTP + cookie helpers this scenario needs (full_get,
    # full_send, status_of, cookie_val, cookie_line) are defined once above, beside
    # the status-only helpers — the update boot (#382) drives the same SSO landing.

    # 1. portal-to-box SSO, driven ONCE (the jti is single-use — a retry replays and
    #    401s). Steps 7-9 already proved the control plane up + the verifier armed, so
    #    a valid token now lands the owner. Expect 303 + both cookies: the host-only
    #    session and the Domain-scoped forward-auth credential.
    sso_resp="$(full_get "/_moose/sso?token=${sso_token}" "$apex" 2>/dev/null || true)"
    sso_status="$(status_of "$sso_resp")"
    grep -q ' 303' <<<"$sso_status" \
        || fail "access: SSO landing did not 303 to the dashboard (owner auto-create failed?): status='$sso_status'"
    session_cookie="$(cookie_val "$sso_resp" moose_session)"
    fa_cookie="$(cookie_val "$sso_resp" moose_forward_auth)"
    [ -n "$session_cookie" ] || fail "access: no moose_session cookie from the SSO landing"
    [ -n "$fa_cookie" ] || fail "access: no moose_forward_auth cookie from the SSO landing"
    echo "cloud-assertions: SSO owner session established (session + forward-auth cookies minted; box_id=$box_id)"

    # 1a. THE TWO-COOKIE SAFETY MODEL, asserted on the wire (#304's headline claim).
    #     The whole design rests on the two cookies having DIFFERENT scopes, and until
    #     now that was only ever asserted structurally in unit tests — this lane
    #     captured the real Set-Cookie headers and then looked only at their values.
    #     Assert the attributes:
    #       - moose_session carries NO Domain ⇒ host-only, scoped to the dashboard host
    #         alone. A Domain here would send the ADMIN session to every app subdomain,
    #         where a third-party app could replay it as the owner. This is the single
    #         most dangerous regression in the whole epic and it is one attribute wide.
    #       - moose_forward_auth carries Domain=<box-id>.onmoose.io ⇒ deliberately
    #         domain-wide, which is what lets the browser present it to an app subdomain
    #         (and is why the app route must strip it — probed below).
    sess_line="$(cookie_line "$sso_resp" moose_session)"
    fa_line="$(cookie_line "$sso_resp" moose_forward_auth)"
    grep -qiE 'Domain=' <<<"$sess_line" \
        && fail "access: SESSION COOKIE IS DOMAIN-SCOPED — the dashboard session must be host-only or an app subdomain receives it and can replay it as the owner: $sess_line"
    grep -qiE "Domain=\.?${apex}(;|$)" <<<"$fa_line" \
        || fail "access: forward-auth cookie is not Domain-scoped to the box apex (${apex}); the browser would never present it to an app subdomain: $fa_line"
    echo "cloud-assertions: cookie scopes correct on the wire (moose_session host-only, moose_forward_auth Domain=${apex})"

    # 2. install whoami air-gapped: offline mode trusts the catalog-promised digest of
    #    the docker-loaded image (no pull). 202 starts the async install job.
    inst_status="$(status_of "$(full_send POST /api/v1/apps "$apex" "$session_cookie" '{"manifest_id":"whoami","scope":"personal"}' 2>/dev/null)")"
    case "$inst_status" in
        *" 202"*|*" 200"*) ;;
        *) fail "access: install whoami did not start: status='$inst_status' (offline bundle/catalog cache missing?)" ;;
    esac

    # 3. RESTRICTED (the hosted default), with the owner's forward-auth cookie ⇒ the
    #    app proxies through. Poll until whoami actually answers (install + compose up
    #    + the route flip from splash to app race this): a 200 whose body is the
    #    whoami echo (Hostname:) means the whole transaction converged AND the
    #    forward_auth verify let the owner through. Send an extra throwaway cookie:
    #    the strip assertion below proves the strip is PER-COOKIE (#335) — the probe
    #    must survive to the app upstream, and moose_forward_auth must not.
    a_resp=""; a_status=""
    for _i in $(seq 1 150); do
        a_resp="$(full_get / "$app_host" "${fa_cookie}; probe=leakcheck" 2>/dev/null || true)"
        a_status="$(status_of "$a_resp")"
        grep -q ' 200' <<<"$a_status" && grep -qi 'Hostname:' <<<"$a_resp" && break
        sleep 1
    done
    grep -q ' 200' <<<"$a_status" && grep -qi 'Hostname:' <<<"$a_resp" \
        || fail "access: restricted app with the owner forward-auth cookie never proxied through to whoami after 150s: status='$a_status'"
    grep -qiE '^X-Moose-User:' <<<"$a_resp" \
        || fail "access: forward-auth identity header X-Moose-User was not forwarded to the app upstream"
    grep -qiE '^Cookie:.*moose_forward_auth=' <<<"$a_resp" \
        && fail "access: COOKIE LEAK (restricted) — the app upstream received moose_forward_auth; the #335 per-cookie strip is broken"
    grep -qiE '^Cookie:.*probe=leakcheck' <<<"$a_resp" \
        || fail "access: restricted app upstream did not receive its own cookie (probe=leakcheck) — the strip is removing more than moose_forward_auth: $(grep -i '^Cookie:' <<<"$a_resp" | tr -d '\r')"
    echo "cloud-assertions: restricted app proxies the owner through with no second login (identity forwarded, only moose_forward_auth stripped)"

    # 3a. RESTRICTED, NO session ⇒ 302 to the box login. Now that the app has
    #     converged, an unauthenticated GET exercises the forward_auth gate's closed
    #     path: the brain verify 401s and Caddy turns it into a redirect to the box
    #     dashboard (https://<box-id>.onmoose.io/, the login).
    n_resp="$(full_get / "$app_host" 2>/dev/null || true)"
    n_status="$(status_of "$n_resp")"
    grep -q ' 302' <<<"$n_status" \
        || fail "access: restricted app without a session did not 302 to the box login: status='$n_status'"
    grep -iE "^Location: *https://${apex}/" <<<"$n_resp" >/dev/null \
        || fail "access: restricted-app 302 Location is not the box login: $(grep -i '^Location:' <<<"$n_resp" | tr -d '\r')"
    echo "cloud-assertions: restricted app gates an unauthenticated request (302 → box login)"

    # 3b. PATH-SCOPED EXPOSURE (#415). The app is still restricted, and its manifest
    #     declares access.public_paths ["/v1", "/v1/*"]. The claim: those paths
    #     answer anonymously (an external SDK can post to the API) while every other
    #     path keeps the box login in front of it. This is the half a unit test
    #     cannot prove — Caddy matches a normalized path, the app sees the original
    #     URI, and that gap is where this bug class lives.
    for p in /v1 /v1/traces "/v1/traces?x=1"; do
        pp_resp="$(full_get "$p" "$app_host" 2>/dev/null || true)"
        grep -q ' 200' <<<"$(status_of "$pp_resp")" && grep -qi 'Hostname:' <<<"$pp_resp" \
            || fail "access: declared public path $p did not reach the app anonymously: status='$(status_of "$pp_resp")'"
    done
    echo "cloud-assertions: declared public paths answer with no session (the token-authed API works while the UI stays gated)"

    # 3c. THE FORGERY GUARD, and the reason the scrub is unconditional. The gate does
    #     not run on a public path, so nothing there would overwrite a caller-supplied
    #     X-Moose-User. If the app got a brain-vouched header on one path and a forged
    #     one on another it could not tell them apart, and "moose says this is the
    #     owner" would become "anyone on the internet says so".
    fg_resp="$(full_get_hdr /v1/traces "$app_host" 'X-Moose-User: attacker' 2>/dev/null || true)"
    grep -qi 'Hostname:' <<<"$fg_resp" || fail "access: forged-header probe did not reach the app on a public path"
    grep -qiE '^X-Moose-User:' <<<"$fg_resp" \
        && fail "access: IDENTITY FORGERY — a client-supplied X-Moose-User survived to the app upstream on a public path: $(grep -i '^X-Moose-User:' <<<"$fg_resp" | tr -d '\r')"
    # Same forgery on the GATED path, with the owner's cookie: the app must receive
    # the brain's value, never the caller's.
    fg2_resp="$(full_get_hdr / "$app_host" 'X-Moose-User: attacker' "$fa_cookie" 2>/dev/null || true)"
    grep -qi 'Hostname:' <<<"$fg2_resp" || fail "access: forged-header probe did not reach the app on the gated path"
    grep -qiE '^X-Moose-User: *attacker' <<<"$fg2_resp" \
        && fail "access: IDENTITY FORGERY — a client-supplied X-Moose-User survived the gate: $(grep -i '^X-Moose-User:' <<<"$fg2_resp" | tr -d '\r')"
    grep -qiE '^X-Moose-User:' <<<"$fg2_resp" \
        || fail "access: the gated path lost the vouched X-Moose-User entirely (the scrub is deleting the brain's own value)"
    echo "cloud-assertions: identity headers scrubbed on both branches (forged X-Moose-User never reaches the app; the vouched one still does)"

    # 3d. The #335 per-cookie strip holds on the public branch too — it is the same
    #     proxy handler on both sides of the subroute, and this proves it.
    pc_resp="$(full_get /v1/traces "$app_host" "${fa_cookie}; probe=leakcheck" 2>/dev/null || true)"
    grep -qi 'Hostname:' <<<"$pc_resp" || fail "access: public-path cookie probe did not reach the app"
    grep -qiE '^Cookie:.*moose_forward_auth=' <<<"$pc_resp" \
        && fail "access: COOKIE LEAK (public path) — the app upstream received moose_forward_auth on a declared public path"
    grep -qiE '^Cookie:.*probe=leakcheck' <<<"$pc_resp" \
        || fail "access: public path lost the app's own cookie — the strip is removing more than moose_forward_auth"
    echo "cloud-assertions: public paths strip only moose_forward_auth (same proxy handler as the gated branch)"

    # 3e. THE BYPASS TABLE — the point of running this through real Caddy. Every
    #     entry is a request that must NOT be treated as a public path. The requests
    #     are written raw onto the socket (no client-side normalization), so what
    #     Caddy matches is exactly what is asserted here:
    #       /v1extra          "/v1/*" must not behave like a bare "/v1*" prefix, or
    #                         a sibling route would be exposed by accident;
    #       /v1/../ and //v1/ path traversal and slash-merging: Caddy matches the
    #                         cleaned path, so these resolve to the app root;
    #       /v1/%2e%2e/       the encoded form of the same, the case where a matcher
    #                         and an app can disagree about what the path is;
    #       /V1x, /admin      plain non-matches, the control.
    #     A 302 to the box login is the pass condition: the gate ran.
    #     The claim asserted for every entry is the one that matters: the app is
    #     NOT reached without a session. How the box says no differs by entry:
    #       gate  — the forward_auth gate ran and 302'd to the box login;
    #       merge — Caddy collapses the duplicate slash and 301's to the
    #               normalized path BEFORE matching, so the app is never reached
    #               and the redirect target is then judged on its own merits
    #               (`/v1/` is genuinely public, `//admin` normalizes to a gated
    #               `/admin`). This was the one real correction the first CI run
    #               produced: the probe is safe, the expectation was wrong.
    for probe in "/v1extra|gate" "/v1/../|gate" "//v1/|merge" "/v1/%2e%2e/|gate" "/V1x|gate" "/admin|gate"; do
        bad="${probe%|*}"; want="${probe#*|}"
        bp_resp="$(full_get "$bad" "$app_host" 2>/dev/null || true)"
        bp_status="$(status_of "$bp_resp")"
        grep -qi 'Hostname:' <<<"$bp_resp" \
            && fail "access: UNGATED PATH — '$bad' reached the app upstream with no session; it is not a declared public path"
        case "$want:$bp_status" in
            gate:*" 302"*) ;;
            merge:*" 301"*)
                grep -qiE '^Location:.*//v1/' <<<"$bp_resp" \
                    && fail "access: UNGATED PATH — '$bad' redirected without collapsing the duplicate slash: $(grep -i '^Location:' <<<"$bp_resp" | tr -d '\r')"
                ;;
            *) fail "access: UNGATED PATH — '$bad' answered '$bp_status', wanted $want; an undeclared path must never be served anonymously" ;;
        esac
    done
    echo "cloud-assertions: undeclared paths stay closed (prefix footgun, traversal, encoded traversal, double slash, case variant)"

    # 4. flip to PUBLIC via the exposure toggle (owner session; the endpoint is
    #    hosted-only + owner-or-admin). Resolve the instance id from the running
    #    container's moose.instance_id label (whoami is FROM-scratch — no shell to
    #    exec — so read it host-side, as the medium lane does).
    cname="$(docker ps --format '{{.Names}}' | grep -i whoami | head -1)"
    [ -n "$cname" ] || fail "access: no running whoami container to resolve the instance id (docker ps: $(docker ps --format '{{.Names}}' | tr '\n' ' '))"
    inst_id="$(docker inspect "$cname" --format '{{ index .Config.Labels "moose.instance_id" }}' 2>/dev/null)"
    [ -n "$inst_id" ] || fail "access: whoami container $cname has no moose.instance_id label"
    exp_status="$(status_of "$(full_send PUT "/api/v1/apps/${inst_id}/exposure" "$apex" "$session_cookie" '{"exposure":"public"}' 2>/dev/null)")"
    grep -q ' 200' <<<"$exp_status" || fail "access: exposure toggle to public failed: status='$exp_status'"

    # 4a. PUBLIC, NO session ⇒ reachable (200), no gate. The route flip from
    #     forward_auth to a bare proxy lands a beat after the PUT, so poll.
    p_resp=""; p_status=""
    for _i in $(seq 1 30); do
        p_resp="$(full_get / "$app_host" 2>/dev/null || true)"
        p_status="$(status_of "$p_resp")"
        grep -q ' 200' <<<"$p_status" && grep -qi 'Hostname:' <<<"$p_resp" && break
        sleep 1
    done
    grep -q ' 200' <<<"$p_status" && grep -qi 'Hostname:' <<<"$p_resp" \
        || fail "access: public app not reachable without a session after the toggle: status='$p_status'"
    echo "cloud-assertions: public app reachable with no session (200)"

    # 4b. PUBLIC + a forward-auth cookie ⇒ STILL stripped before the app upstream. A
    #     public app must never receive the Domain-scoped cookie, or it could replay
    #     it against the owner's restricted apps — the reason the route builder
    #     strips moose_forward_auth on every hosted route, public included (#335
    #     narrows this from #306's whole-header delete to just that one cookie; the
    #     probe cookie must still reach a public app, same as a restricted one).
    pl_resp="$(full_get / "$app_host" "${fa_cookie}; probe=leakcheck" 2>/dev/null || true)"
    grep -qi 'Hostname:' <<<"$pl_resp" || fail "access: public-app cookie-leak probe did not reach whoami"
    grep -qiE '^Cookie:.*moose_forward_auth=' <<<"$pl_resp" \
        && fail "access: COOKIE LEAK (public) — the app upstream received moose_forward_auth; the #335 per-cookie strip is broken"
    grep -qiE '^Cookie:.*probe=leakcheck' <<<"$pl_resp" \
        || fail "access: public app upstream did not receive its own cookie (probe=leakcheck) — the strip is removing more than moose_forward_auth: $(grep -i '^Cookie:' <<<"$pl_resp" | tr -d '\r')"
    echo "cloud-assertions: public app also strips only moose_forward_auth (no forward-auth cookie leaks to a public upstream, app's own cookie intact)"

    # 4c. FOLDER APPS ON BOTH SOURCES (#519). A folder app is the path the other
    #     steps never touch: before compose up the folder source must exist on
    #     the HOST, owned so the app's identity can write it. The brain runs in a
    #     container, so this only works if the shared tree is mounted into it
    #     (household) and host-agent prepares the home folder (personal). Until
    #     #519 a household install failed with "stat /srv/moose/shared: no such
    #     file or directory", and a personal folder was made inside the brain's
    #     own container. filedrop writes one file into its Documents folder at
    #     start, as the identity moose runs it as. The proof is that file, on the
    #     host, with the expected owner.
    brain_mounts="$(docker inspect moose-brain --format '{{range .Mounts}}{{.Source}}->{{.Destination}} {{end}}' 2>/dev/null)"
    grep -q '/srv/moose/shared->/srv/moose/shared' <<<"$brain_mounts" \
        || fail "folders: the brain does not mount the shared tree at the same path: $brain_mounts"
    grep -qE '(^| )/home->' <<<"$brain_mounts" \
        && fail "folders: the brain mounts /home; personal folders are host-agent's job and the brain must not hold every home: $brain_mounts"
    sr_stat="$(stat -c '%u:%g %a' /srv/moose/shared 2>/dev/null)"
    [ "$sr_stat" = "0:2001 2770" ] \
        || fail "folders: /srv/moose/shared is '$sr_stat', want '0:2001 2770' (root:moose-shared, setgid) before any app installs"
    echo "cloud-assertions: shared tree ready before the first install (root:moose-shared 2770) and mounted into the brain; /home is not"

    # filedrop_install BODY LABEL sets FD_ID to the new instance id, or fails. It
    # polls the install job so a red run names the brain's own error, not just a
    # timeout. These helpers set globals instead of printing: a fail inside $(...)
    # would only leave the subshell, and the scenario would carry on.
    filedrop_install() {
        local resp job st jr
        resp="$(full_send POST /api/v1/apps "$apex" "$session_cookie" "$1" 2>/dev/null)"
        grep -qE ' 20[02]' <<<"$(status_of "$resp")" \
            || fail "folders: install filedrop ($2) did not start: status='$(status_of "$resp")' body=$(tail -1 <<<"$resp" | cut -c1-400)"
        job="$(json_str_of "$resp" job_id)"
        [ -n "$job" ] || fail "folders: install filedrop ($2) returned no job id: $(tail -1 <<<"$resp" | cut -c1-400)"
        st=""; jr=""
        for _i in $(seq 1 240); do
            jr="$(full_get "/api/v1/jobs/${job}" "$apex" "$session_cookie" 2>/dev/null || true)"
            st="$(json_str_of "$jr" status)"
            case "$st" in completed|failed|cancelled) break ;; esac
            sleep 1
        done
        [ "$st" = "completed" ] \
            || fail "folders: install filedrop ($2) ended '$st': $(grep -o '"error":{[^}]*}' <<<"$jr" | cut -c1-600)"
        FD_ID="$(json_str_of "$jr" instance_id)"
        [ -n "$FD_ID" ] || fail "folders: install filedrop ($2) completed with no instance id: $(tail -1 <<<"$jr" | cut -c1-400)"
    }

    # filedrop_user INSTANCE_ID sets FD_USER to the running container's "uid:gid".
    filedrop_user() {
        local c=""
        for _i in $(seq 1 60); do
            c="$(docker ps --filter "label=moose.instance_id=$1" --format '{{.Names}}' | head -1)"
            [ -n "$c" ] && break
            sleep 1
        done
        [ -n "$c" ] || fail "folders: no running container for filedrop instance $1: $(docker ps -a --format '{{.Names}} {{.Status}}' | grep -i filedrop | tr '\n' ' ')"
        FD_USER="$(docker inspect "$c" --format '{{.Config.User}}')"
    }

    # wait_file PATH returns 0 once PATH exists (the app writes it at start).
    wait_file() {
        for _i in $(seq 1 30); do [ -f "$1" ] && return 0; sleep 1; done
        return 1
    }

    # Household: the admin default. Runs as moose-app (2000) with the moose-shared
    # group (2001) added, and binds /srv/moose/shared/Documents, which the brain
    # creates root:moose-shared 2770 through its mount.
    filedrop_install '{"manifest_id":"filedrop","scope":"household"}' household
    filedrop_user "$FD_ID"; hh_user="$FD_USER"
    [ "$hh_user" = "2000:2000" ] || fail "folders: household filedrop runs as '$hh_user', want moose-app 2000:2000"
    hh_file=/srv/moose/shared/Documents/filedrop.txt
    wait_file "$hh_file" \
        || fail "folders: household filedrop wrote no file at $hh_file on the host: $(ls -la /srv/moose/shared /srv/moose/shared/Documents 2>&1 | tr '\n' ' ')"
    hh_dir_stat="$(stat -c '%u:%g %a' /srv/moose/shared/Documents)"
    [ "$hh_dir_stat" = "0:2001 2770" ] \
        || fail "folders: /srv/moose/shared/Documents is '$hh_dir_stat', want '0:2001 2770' (made by the brain, root:moose-shared, setgid)"
    hh_file_owner="$(stat -c '%u:%g' "$hh_file")"
    [ "$hh_file_owner" = "2000:2001" ] \
        || fail "folders: $hh_file is owned '$hh_file_owner', want 2000:2001 (moose-app, group moose-shared by the setgid folder)"
    grep -q 'uid=2000' "$hh_file" || fail "folders: $hh_file does not say the app ran as 2000: $(cat "$hh_file")"
    echo "cloud-assertions: household folder app wrote $hh_file on the host as $hh_file_owner (Documents $hh_dir_stat)"

    # Personal: the owner's own copy (confirm, because the household copy above
    # triggers the duplicate warning). Runs as the owner, and binds ~/Documents,
    # which host-agent creates owned by the owner. The SSO owner is a new
    # account, so ~/Documents does not exist until this install asks for it.
    filedrop_install '{"manifest_id":"filedrop","scope":"personal","confirm":true}' personal
    filedrop_user "$FD_ID"; pp_user="$FD_USER"
    pp_uid="${pp_user%%:*}"; pp_gid="${pp_user##*:}"
    pp_home="$(getent passwd "$pp_uid" | cut -d: -f6)"
    # Not a uid range check: the hosted image sets no UID_MIN, so the SSO owner
    # gets the next free uid (2001 in CI), not one from the 3000+ range. What
    # matters here is that the app runs as a real login account with a home
    # under /home, not as root or the shared moose-app identity.
    case "$pp_uid" in ''|0|2000|*[!0-9]*) pp_home="" ;; esac
    case "$pp_home" in /home/?*) ;; *) pp_home="" ;; esac
    [ -n "$pp_home" ] \
        || fail "folders: personal filedrop runs as '$pp_user', want the owner's own account with a home under /home; getent: $(getent passwd "$pp_uid")"
    pp_file="$pp_home/Documents/filedrop.txt"
    wait_file "$pp_file" \
        || fail "folders: personal filedrop wrote no file at $pp_file on the host: $(ls -la "$pp_home" "$pp_home/Documents" 2>&1 | tr '\n' ' ')"
    pp_dir_owner="$(stat -c '%u:%g' "$pp_home/Documents")"
    [ "$pp_dir_owner" = "$pp_user" ] \
        || fail "folders: $pp_home/Documents is owned '$pp_dir_owner', want the owner $pp_user (made by host-agent, not root:root by Docker)"
    pp_file_owner="$(stat -c '%u:%g' "$pp_file")"
    [ "$pp_file_owner" = "$pp_uid:$pp_gid" ] \
        || fail "folders: $pp_file is owned '$pp_file_owner', want the owner $pp_uid:$pp_gid"
    echo "cloud-assertions: personal folder app wrote $pp_file on the host as $pp_file_owner (Documents owned by the owner)"

    # 4d. AN image_user APP RUNS AS ITS IMAGE'S OWN USER, REMAPPED (#537, #530).
    #     The image turns the userns remap on, so the brain puts this app in the
    #     image tier: no user: pin, no capability back, and the daemon's remap.
    #     Each service writes its id into its bind dir at start, as the user its
    #     image sets (1001 by number, app2 = 1002 by name). The proof is that
    #     file on the host, owned by base plus that uid. Until #530 this step
    #     checked the refusal on a box with no remap; that refusal is covered by
    #     the brain's tests (internal/lifecycle/userns_test.go) and no lane box
    #     runs without the remap any more.
    iu_resp="$(full_send POST /api/v1/apps "$apex" "$session_cookie" '{"manifest_id":"imageuser","scope":"household"}' 2>/dev/null)"
    iu_job="$(json_str_of "$iu_resp" job_id)"
    [ -n "$iu_job" ] || fail "image_user: install imageuser returned no job id: status='$(status_of "$iu_resp")' $(tail -1 <<<"$iu_resp" | cut -c1-400)"
    iu_st=""; iu_jr=""
    for _i in $(seq 1 240); do
        iu_jr="$(full_get "/api/v1/jobs/${iu_job}" "$apex" "$session_cookie" 2>/dev/null || true)"
        iu_st="$(json_str_of "$iu_jr" status)"
        case "$iu_st" in completed|failed|cancelled) break ;; esac
        sleep 1
    done
    [ "$iu_st" = completed ] \
        || fail "image_user: install imageuser on the remapped box ended '$iu_st', want completed: $(grep -o '"error":{[^}]*}' <<<"$iu_jr" | cut -c1-600)"
    iu_id="$(json_str_of "$iu_jr" instance_id)"
    [ -n "$iu_id" ] || fail "image_user: install imageuser completed with no instance id: $(tail -1 <<<"$iu_jr" | cut -c1-400)"
    for svc_dir_uid in imageuser:data:1001 named:work:1002; do
        svc="${svc_dir_uid%%:*}"; rest="${svc_dir_uid#*:}"; dir="${rest%%:*}"; want_uid="$((remap_base + ${rest#*:}))"
        c="$(docker ps --filter "label=moose.instance_id=$iu_id" --filter "label=com.docker.compose.service=$svc" --format '{{.Names}}' | head -1)"
        [ -n "$c" ] || fail "image_user: no running container for service $svc: $(docker ps -a --filter "label=moose.instance_id=$iu_id" --format '{{.Names}} {{.Status}}' | tr '\n' ' ')"
        c_um="$(docker inspect "$c" --format '{{.HostConfig.UsernsMode}}')"
        c_user="$(docker inspect "$c" --format '{{.Config.User}}')"
        c_capadd="$(docker inspect "$c" --format '{{json .HostConfig.CapAdd}}')"
        [ -z "$c_um" ] || fail "image_user: $svc UsernsMode is '$c_um', want the daemon default (remapped)"
        case "$c_capadd" in null|'[]') ;; *) fail "image_user: $svc cap_add is $c_capadd, want none (image tier)" ;; esac
        id_file="/var/lib/moose/state/instances/$iu_id/$dir/id.txt"
        wait_file "$id_file" || fail "image_user: $svc wrote no $id_file on the host: $(ls -lan "/var/lib/moose/state/instances/$iu_id" 2>&1 | tr '\n' ' ')"
        id_owner="$(stat -c '%u' "$id_file")"
        [ "$id_owner" = "$want_uid" ] || fail "image_user: $id_file is owned by host uid $id_owner, want $want_uid (base + the image's user)"
        echo "cloud-assertions: image_user $svc runs remapped as its image's user (Config.User='$c_user', cap_add=$c_capadd) and wrote $dir/id.txt as host uid $id_owner: $(cat "$id_file")"
    done

    # 5. THE HOSTED CONFIRM STEP (os#469). Destructive admin writes sit behind a
    #    re-auth gate, and until now a hosted owner could not pass it: the portal
    #    signs them in and the box gives their PAM account a random password nobody
    #    has seen, so every elevation-class action was unreachable on a hosted box.
    #    The fix makes a second portal round-trip the proof. This drives it with a
    #    REAL assertion (the harness's second token) against the REAL handshake, and
    #    ends in a real elevation-class write — the only proof that matters.
    sso_token2="$(tr -d '\r\n' < "${CREDENTIALS_DIRECTORY:-/nonexistent}/moose.sso_token2" 2>/dev/null || true)"
    [ -n "$sso_token2" ] || fail "access: moose.sso_token2 credential missing (harness did not mint/deliver the second owner assertion)"

    new_user_body='{"display_name":"tester","password":"moose-cloud-lane-tester-pw"}'

    # 5a. The plain owner session is admin but NOT elevated, so the write is refused.
    #     This is the state a hosted box could never leave before #469.
    cu_status="$(status_of "$(full_send POST /api/v1/users "$apex" "$session_cookie" "$new_user_body" 2>/dev/null)")"
    grep -q ' 403' <<<"$cu_status" \
        || fail "access: create-user on a signed-in-but-unconfirmed owner session answered '$cu_status'; wanted 403 (the re-auth gate)"

    # 5b. The dashboard mints a one-time confirm challenge. It is what a cross-site
    #     page cannot supply: minting it takes an authenticated POST to the box's own
    #     API, so a drive-by navigation to the portal's open-box route cannot arm the
    #     window on the victim's box.
    ch_resp="$(full_send POST /api/v1/auth/elevate/challenge "$apex" "$session_cookie" '{}' 2>/dev/null)"
    grep -q ' 200' <<<"$(status_of "$ch_resp")" \
        || fail "access: mint confirm challenge answered '$(status_of "$ch_resp")'; wanted 200"
    challenge="$(json_str_of "$ch_resp" challenge)"
    [ -n "$challenge" ] || fail "access: confirm challenge response carried no challenge: $(tail -1 <<<"$ch_resp")"

    # 5c. The portal round-trip: a fresh assertion plus the return path the dashboard
    #     asked for, URL-encoded exactly as the portal forwards it. The box must land
    #     the owner back on the page they came from, with the spent challenge stripped
    #     out of the URL.
    cf_resp="$(full_get "/_moose/sso?token=${sso_token2}&return=%2Fsettings%2Fusers%3Fconfirm%3D${challenge}" "$apex" 2>/dev/null || true)"
    grep -q ' 303' <<<"$(status_of "$cf_resp")" \
        || fail "access: confirm landing answered '$(status_of "$cf_resp")'; wanted 303"
    cf_loc="$(grep -i '^Location:' <<<"$cf_resp" | head -1 | tr -d '\r' | awk '{print $2}')"
    [ "$cf_loc" = "/settings/users" ] \
        || fail "access: confirm landing sent the owner to '$cf_loc'; wanted /settings/users with the confirm stripped"
    confirm_cookie="$(cookie_val "$cf_resp" moose_session)"
    [ -n "$confirm_cookie" ] || fail "access: confirm landing minted no session cookie"

    # 5d. The same write now passes, and it really reached the host: the Linux
    #     account exists. A 200 alone would only prove the brain let it through.
    cu2_status="$(status_of "$(full_send POST /api/v1/users "$apex" "$confirm_cookie" "$new_user_body" 2>/dev/null)")"
    grep -q ' 200' <<<"$cu2_status" \
        || fail "access: create-user after the portal confirm answered '$cu2_status'; wanted 200 — the hosted owner still cannot pass the re-auth gate"
    id tester >/dev/null 2>&1 \
        || fail "access: create-user returned 200 but no PAM account 'tester' exists; the elevation-class write never reached the host"
    echo "cloud-assertions: hosted confirm step opened the elevation window through a real portal round-trip (create-user 403 before, 200 after, PAM account created)"

    # 5e. A return path naming another host must never be honoured: the landing hands
    #     out a live session, so an open redirect here would hand it to somebody
    #     else's page. The owner still signs in and still lands on their own front
    #     page. Driven with the third assertion — every token is single-use, and a
    #     rejected token would 401 before the redirect is ever built, which would
    #     prove nothing about the return path. The other refused shapes are covered
    #     per-shape by the brain's unit tests (internal/api # TestReturnTarget).
    sso_token3="$(tr -d '\r\n' < "${CREDENTIALS_DIRECTORY:-/nonexistent}/moose.sso_token3" 2>/dev/null || true)"
    [ -n "$sso_token3" ] || fail "access: moose.sso_token3 credential missing (harness did not mint/deliver the third owner assertion)"
    or_resp="$(full_get "/_moose/sso?token=${sso_token3}&return=%2F%2Fevil.example%2Fsteal" "$apex" 2>/dev/null || true)"
    grep -q ' 303' <<<"$(status_of "$or_resp")" \
        || fail "access: landing with an off-box return answered '$(status_of "$or_resp")'; wanted 303 (sign-in still works)"
    or_loc="$(grep -i '^Location:' <<<"$or_resp" | head -1 | tr -d '\r' | awk '{print $2}')"
    [ "$or_loc" = "/" ] \
        || fail "access: OPEN REDIRECT — an off-box return path became Location '$or_loc'; wanted the box's own front page"
    [ -n "$(cookie_val "$or_resp" moose_session)" ] \
        || fail "access: the off-box-return landing minted no session; sign-in must still succeed"
    echo "cloud-assertions: an off-box return path is refused and the owner lands on the box's own front page"

    echo "cloud-assertions: hosted per-app access modes verified end-to-end (restricted gate + owner proxy-through, public reachability, per-cookie strip in both modes), and folder apps write to the host on both sources"
    ;;
ssh)
    # SSH end-to-end on a booted hosted box (#467). #464 built the whole path from
    # the brain's API down to the rendered sshd config, but every proof of it stopped
    # at a rendered string: no test had ever watched :22 open, watched a real sshd
    # accept a key and refuse a password, or watched the port close again. That is
    # what this scenario is for, and it is the acceptance condition #463 left unmet.
    #
    # Everything happens inside the guest. The box makes its own keypair with
    # ssh-keygen and connects to itself, so the air gap costs nothing. The owner
    # session comes from the same signed test-portal credential the access boot uses.
    [ -f "$SEED" ] || fail "ssh mode but $SEED absent (seed materializer did not run?)"
    box_id="$(json_str "$SEED" box_id)"
    [ -n "$box_id" ] || fail "ssh mode: could not read box_id from $SEED"
    apex="${box_id}.onmoose.io"

    DROPIN=/etc/ssh/sshd_config.d/moose-allowed.conf
    KEYSDIR=/etc/ssh/moose-authorized-keys

    # A TCP connect to :22, as the answer to "is the port open". /dev/tcp fails on a
    # closed port, which is exactly the signal — no ss/netstat parsing.
    port22_open() { (exec 3<>/dev/tcp/127.0.0.1/22) 2>/dev/null; }
    unit_active() { [ "$(systemctl is-active ssh.service 2>/dev/null)" = active ]; }
    # The daemon's run state and the port change a beat after the API returns (the
    # brain calls host-agent, which runs systemctl), so poll rather than sample.
    wait_port22() { # want=open|closed
        local want="$1" _i
        for _i in $(seq 1 30); do
            if [ "$want" = open ]; then port22_open && return 0
            else port22_open || return 0; fi
            sleep 1
        done
        return 1
    }

    # --- 1. at boot: nothing listening, nothing enabled.
    # On hosted the daemon IS the port control — no moose firewall, and the provider
    # attaches none — so a box that boots with sshd running has no control over :22
    # at all. Debian's openssh-server postinst enables ssh.service on install, so
    # this is a live check on the image's own wiring (dev/cloud/mkosi.postinst.chroot
    # undoes it), not a restatement of a config file.
    unit_active && fail "ssh: ssh.service is active at boot — the image enabled sshd; on hosted that leaves :22 open for the life of the box"
    port22_open && fail "ssh: :22 answers at boot with no account enabled"
    [ -f "$DROPIN" ] && fail "ssh: $DROPIN exists at boot — nothing should be rendered before an account opts in"

    # Host keys are this box's own, not the image's. Debian's postinst generates
    # them at IMAGE BUILD time, so every box provisioned from one image would share
    # them and any holder of the published image could impersonate a box to its
    # owner's ssh client. The build deletes them and moose-sshd-keygen.service
    # makes per-box ones at boot.
    #
    # Their presence is not the question — they have to be here, because host-agent
    # validates the rendered config with `sshd -t` before it ever starts the daemon
    # and that needs keys. The question is WHEN they were written. A key this box
    # generated has an mtime at or after this boot; a key baked into the image
    # carries the build's timestamp, hours or days earlier. The 300s slack absorbs
    # clock jitter and still separates the two cases by a wide margin.
    hostkey=/etc/ssh/ssh_host_ed25519_key
    [ -f "$hostkey" ] \
        || fail "ssh: no host keys at boot — moose-sshd-keygen.service did not run, so the first enable will fail 'sshd -t' with 'no hostkeys available'"
    btime="$(awk '/^btime /{print $2}' /proc/stat)"
    kmtime="$(stat -c %Y "$hostkey" 2>/dev/null || echo 0)"
    [ -n "$btime" ] && [ "$kmtime" -ge "$((btime - 300))" ] \
        || fail "ssh: HOST KEY WAS BAKED INTO THE IMAGE — written $((btime - kmtime))s before this boot, so every box from this image shares it"
    echo "cloud-assertions: :22 closed and ssh.service inactive at boot, nothing rendered, host keys generated by this box"

    # --- 2. owner session, then elevation.
    # Every SSH write is elevation-class (it changes who can get a shell on the box),
    # so the session has to pass the re-auth gate. Elevation re-verifies the account's
    # password through PAM, and a hosted owner's password is generated by the SSO
    # auto-create and thrown away — nobody, including this harness, knows it. So set
    # one here as host root, which is the only side that can. This is harness setup,
    # not a product path: the box is ours and PAM is the source of truth
    # (AUTH.md # Identity primitive), so chpasswd is the same write the brain would
    # make through host-agent. A real hosted owner has no way past this gate at all
    # today; that is #469, not something this scenario can fix.
    # The SSO landing runs first: it is what creates the owner's PAM account, so
    # there is nothing to set a password on before it. Driven ONCE — the jti is
    # single-use, so a retry replays and 401s.
    sso_token="$(tr -d '\r\n' < "${CREDENTIALS_DIRECTORY:-/nonexistent}/moose.sso_token" 2>/dev/null || true)"
    [ -n "$sso_token" ] || fail "ssh: moose.sso_token credential missing (harness did not mint/deliver the owner assertion)"
    sso_resp="$(full_get "/_moose/sso?token=${sso_token}" "$apex" 2>/dev/null || true)"
    grep -q ' 303' <<<"$(status_of "$sso_resp")" \
        || fail "ssh: SSO landing did not 303 to the dashboard: status='$(status_of "$sso_resp")'"
    owner_cookie="$(cookie_val "$sso_resp" moose_session)"
    [ -n "$owner_cookie" ] || fail "ssh: no moose_session cookie from the SSO landing"

    owner=owner
    OWNER_PW='moose-cloud-lane-owner-pw'
    id "$owner" >/dev/null 2>&1 \
        || fail "ssh: the SSO landing did not create the PAM account '$owner' (mkassertion's -email local-part)"
    printf '%s:%s\n' "$owner" "$OWNER_PW" | chpasswd || fail "ssh: could not set a known password for '$owner'"

    # Every SSH write below re-elevates first. The window is five minutes
    # (USERS_AND_GROUPS.md # Elevation in the UI) and this scenario drives three
    # sshd reloads, three real ssh connections and a user create + delete, each
    # waiting on a systemctl round-trip through host-agent — under CI's TCG-only
    # QEMU that can outlast the window, and an expired one fails as a 403 that says
    # nothing about SSH. Re-elevating is one PAM verify and removes the flake.
    elevate() { # HOST COOKIE PASSWORD -> 0 when the session is elevated
        local r
        r="$(full_send POST /api/v1/auth/elevate "$1" "$2" "{\"password\":\"$3\"}" 2>/dev/null)"
        grep -q ' 200' <<<"$(status_of "$r")"
    }
    elevate "$apex" "$owner_cookie" "$OWNER_PW" \
        || fail "ssh: elevate as the owner failed (PAM did not accept the password we just set?)"
    echo "cloud-assertions: owner session established and elevated"

    # --- 3. turning SSH on with no key is refused.
    # The key is the MANDATORY factor on hosted (DECISIONS.md 2026-09-09): a
    # household password on a port the open internet can reach is not a credential,
    # and sshd knows nothing of the login throttling the brain applies to that same
    # password. Enforced server-side, not only in the dashboard.
    elevate "$apex" "$owner_cookie" "$OWNER_PW" || fail "ssh: re-elevate before the key-less enable failed"
    # PUT /me/ssh takes the whole state, keys included (#501), so an empty key
    # list is sent on purpose: a body with no "keys" is refused for being
    # malformed, which would pass this check for the wrong reason.
    nokey="$(full_send PUT /api/v1/me/ssh "$apex" "$owner_cookie" '{"enabled":true,"keys":[]}' 2>/dev/null)"
    grep -q ' 422' <<<"$(status_of "$nokey")" \
        || fail "ssh: enabling SSH with no key was not refused: status='$(status_of "$nokey")' (want 422 — the key is mandatory on hosted)"
    port22_open && fail "ssh: :22 opened after a refused enable"
    echo "cloud-assertions: enabling SSH with no key refused (422), :22 still closed"

    # --- 4. add a key and turn SSH on, in one save.
    KEYFILE=/run/moose-ssh-lane  # /run, because the OS slot is read-only (BUILD.md # 1b)
    rm -f "$KEYFILE" "${KEYFILE}.pub"
    ssh-keygen -t ed25519 -N '' -C 'moose-cloud-lane' -f "$KEYFILE" >/dev/null 2>&1 \
        || fail "ssh: ssh-keygen failed (is openssh-client in the image?)"
    pubkey="$(tr -d '\n' < "${KEYFILE}.pub")"
    elevate "$apex" "$owner_cookie" "$OWNER_PW" || fail "ssh: re-elevate before turning SSH on failed"
    on="$(full_send PUT /api/v1/me/ssh "$apex" "$owner_cookie" \
        "{\"enabled\":true,\"keys\":[{\"public_key\":\"${pubkey}\",\"label\":\"cloud lane\"}]}" 2>/dev/null)"
    grep -q ' 200' <<<"$(status_of "$on")" \
        || fail "ssh: turning SSH on with a new key failed: status='$(status_of "$on")'"
    # The answer is the whole state; its one key is the one just added. Later
    # saves keep it by this id.
    key_id="$(json_str_of "$on" id)"
    [ -n "$key_id" ] || fail "ssh: could not read the new key's id out of the save response"
    wait_port22 open || fail "ssh: :22 never opened after the account was enabled"
    unit_active || fail "ssh: ssh.service is not active after the account was enabled"
    [ -f "$DROPIN" ] || fail "ssh: $DROPIN was not rendered after the account was enabled"
    grep -qE "^AllowUsers .*\b${owner}\b" "$DROPIN" \
        || fail "ssh: rendered drop-in does not name '$owner' in AllowUsers: $(cat "$DROPIN")"
    grep -qE "^Match User ${owner}\$" "$DROPIN" \
        || fail "ssh: rendered drop-in has no Match block for '$owner': $(cat "$DROPIN")"
    [ -f "${KEYSDIR}/${owner}" ] || fail "ssh: managed key file ${KEYSDIR}/${owner} was not written"
    echo "cloud-assertions: SSH on — ssh.service active, :22 listening, drop-in names $owner"

    # --- 5. a real connection: the key gets in, a password alone does not.
    # This is the step that makes the whole scenario worth booting a VM for. Up to
    # here everything is a rendered string; from here a real sshd decides.
    SSH_OPTS="-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 -o BatchMode=yes"
    # Poll: sshd was just started/reloaded and may not have finished binding.
    conn=""
    for _i in $(seq 1 30); do
        conn="$(ssh $SSH_OPTS -i "$KEYFILE" "${owner}@127.0.0.1" 'echo MOOSE_SSH_OK' 2>&1)"
        grep -q MOOSE_SSH_OK <<<"$conn" && break
        sleep 1
    done
    grep -q MOOSE_SSH_OK <<<"$conn" \
        || fail "ssh: key-based login as '$owner' failed: $conn"
    echo "cloud-assertions: real ssh login with the key succeeded, on host keys this box generated itself"

    # Password alone must not be a way in. Ask sshd directly rather than trying to
    # type a password without a terminal: a client that offers no key gets back the
    # server's list of methods that may continue, and with AuthenticationMethods
    # publickey that list is `publickey` — sshd saying, on the wire, that no password
    # is accepted here. The connection must also actually fail.
    # Normalise line endings before matching. The methods list is read out of ssh's
    # -v output, and matching it is where this assertion has actually gone wrong
    # before: an anchored pattern failed against a line that printed identically.
    # Match on what the line says, not on where it ends.
    pw_out="$(ssh $SSH_OPTS -v -o PubkeyAuthentication=no -o PreferredAuthentications=password \
        "${owner}@127.0.0.1" 'echo MOOSE_SSH_PW' 2>&1 | tr -d '\r')"
    grep -q MOOSE_SSH_PW <<<"$pw_out" \
        && fail "ssh: PASSWORD-ONLY LOGIN SUCCEEDED — the key is supposed to be the mandatory factor on hosted: $pw_out"
    pw_methods="$(grep -i 'Authentications that can continue' <<<"$pw_out" | tail -1)"
    [ -n "$pw_methods" ] \
        || fail "ssh: sshd never sent a methods list on the password-only attempt; cannot tell what it would accept: $pw_out"
    # The property, stated directly: password is not among the ways in. Asserting
    # the absence is what "a password alone does not get in" means — a list that
    # merely contains publickey would still be satisfied by publickey,password.
    grep -qi 'password' <<<"$pw_methods" \
        && fail "ssh: PASSWORD IS AN ACCEPTED METHOD — sshd offers '$pw_methods'; on hosted the key is the mandatory factor, not one of two doors"
    grep -qi 'publickey' <<<"$pw_methods" \
        || fail "ssh: sshd does not offer publickey either: '$pw_methods'"
    echo "cloud-assertions: password-only login refused — sshd offers '${pw_methods#*continue: }' and nothing else"

    # --- 6. the optional second factor makes the key alone insufficient.
    # The optional factor is a second lock, never a second door: AuthenticationMethods
    # becomes publickey,password, so sshd demands BOTH and neither alone gets in
    # (AUTH.md # Device access). The proof is the same key that just worked no longer
    # working, and sshd asking for a password after accepting it.
    elevate "$apex" "$owner_cookie" "$OWNER_PW" || fail "ssh: re-elevate before the second-factor toggle failed"
    both="$(full_send PUT /api/v1/me/ssh "$apex" "$owner_cookie" \
        "{\"enabled\":true,\"require_password\":true,\"keys\":[{\"id\":\"${key_id}\"}]}" 2>/dev/null)"
    grep -q ' 200' <<<"$(status_of "$both")" \
        || fail "ssh: turning on the optional second factor failed: status='$(status_of "$both")'"
    grep -qE '^ *AuthenticationMethods +publickey,password$' "$DROPIN" \
        || fail "ssh: drop-in does not require publickey,password after the second factor was turned on: $(cat "$DROPIN")"

    # BatchMode means the client can never supply a password, so a success here would
    # mean the key alone was enough. Poll the other way round: give the reload a
    # moment, but require every attempt in the window to fail.
    sleep 3
    keyonly="$(ssh $SSH_OPTS -v -i "$KEYFILE" "${owner}@127.0.0.1" 'echo MOOSE_SSH_KEYONLY' 2>&1 | tr -d '\r')"
    grep -q MOOSE_SSH_KEYONLY <<<"$keyonly" \
        && fail "ssh: THE KEY ALONE STILL GETS IN with the second factor on — publickey,password is not being enforced: $keyonly"
    # Partial success is the AND, in sshd's own words: the key was accepted and was
    # not enough. Stronger than reading the methods list, because it says the key
    # got through and the connection still did not.
    grep -qi 'partial success' <<<"$keyonly" \
        || fail "ssh: sshd did not report partial success, so the key was not even accepted: $(grep -iE 'can continue|denied' <<<"$keyonly" | tail -3)"
    key_methods="$(grep -i 'Authentications that can continue' <<<"$keyonly" | tail -1)"
    grep -qi 'password' <<<"$key_methods" \
        || fail "ssh: sshd did not demand a password after accepting the key: '$key_methods'"
    echo "cloud-assertions: second factor enforced — the key is accepted, then a password is still demanded"

    # --- 7. a save that leaves SSH on with no key is refused.
    # The guard checks the state the save ends in (#501), so a save that drops the
    # only key and adds its replacement goes through, and one that only drops it
    # is refused rather than silently turning SSH off.
    elevate "$apex" "$owner_cookie" "$OWNER_PW" || fail "ssh: re-elevate before the key removal failed"
    delk="$(full_send PUT /api/v1/me/ssh "$apex" "$owner_cookie" '{"enabled":true,"require_password":true,"keys":[]}' 2>/dev/null)"
    grep -q ' 422' <<<"$(status_of "$delk")" \
        || fail "ssh: removing the only key while SSH is on was not refused: status='$(status_of "$delk")' (want 422)"
    [ -f "${KEYSDIR}/${owner}" ] || fail "ssh: the managed key file disappeared on a refused delete"
    echo "cloud-assertions: removing the only key while SSH is on refused (422)"

    # --- 8. turn SSH off: the port closes and the key file goes.
    elevate "$apex" "$owner_cookie" "$OWNER_PW" || fail "ssh: re-elevate before turning SSH off failed"
    off="$(full_send PUT /api/v1/me/ssh "$apex" "$owner_cookie" \
        "{\"enabled\":false,\"keys\":[{\"id\":\"${key_id}\"}]}" 2>/dev/null)"
    grep -q ' 200' <<<"$(status_of "$off")" \
        || fail "ssh: turning SSH off failed: status='$(status_of "$off")'"
    wait_port22 closed || fail "ssh: :22 STILL ANSWERS after the last account turned SSH off — on hosted the daemon is the only port control"
    unit_active && fail "ssh: ssh.service is still active after the last account turned SSH off"
    [ -f "${KEYSDIR}/${owner}" ] \
        && fail "ssh: the managed key file ${KEYSDIR}/${owner} survived the account being turned off"
    echo "cloud-assertions: SSH off — ssh.service stopped, :22 closed, managed key file removed"

    # --- 9. deleting a user who had SSH on takes their access with them.
    # This cleanup landed in #464 and nothing outside a booted box can check it: a
    # name left in AllowUsers or a key file left in ${KEYSDIR} is a credential for an
    # account that no longer exists. Needs a second account, because the owner cannot
    # delete itself.
    GONE_USER=sshgone
    GONE_PW='moose-cloud-lane-gone-pw'
    elevate "$apex" "$owner_cookie" "$OWNER_PW" || fail "ssh: re-elevate before creating the second account failed"
    mk="$(full_send POST /api/v1/users "$apex" "$owner_cookie" \
        "{\"display_name\":\"${GONE_USER}\",\"password\":\"${GONE_PW}\",\"role\":\"member\"}" 2>/dev/null)"
    grep -qE ' (200|201)' <<<"$(status_of "$mk")" \
        || fail "ssh: could not create the second account: status='$(status_of "$mk")'"
    gone_id="$(json_str_of "$mk" id)"
    [ -n "$gone_id" ] || fail "ssh: could not read the new user's id out of the create response"

    # That account turns its own SSH on — /me/ssh is self-service, so it needs its own
    # session and its own elevation.
    glogin="$(full_send POST /api/v1/login "$apex" 'probe=login' \
        "{\"username\":\"${GONE_USER}\",\"password\":\"${GONE_PW}\"}" 2>/dev/null)"
    grep -q ' 200' <<<"$(status_of "$glogin")" \
        || fail "ssh: '$GONE_USER' could not log in: status='$(status_of "$glogin")'"
    gone_cookie="$(cookie_val "$glogin" moose_session)"
    [ -n "$gone_cookie" ] || fail "ssh: no session cookie for '$GONE_USER'"
    elevate "$apex" "$gone_cookie" "$GONE_PW" || fail "ssh: '$GONE_USER' could not elevate"

    elevate "$apex" "$gone_cookie" "$GONE_PW" || fail "ssh: re-elevate as '$GONE_USER' before turning SSH on failed"
    # This enable is also the re-enable regression test. SSH was turned off in step
    # 8, which stops the unit — and Debian's ssh.service declares
    # RuntimeDirectory=sshd, so systemd deletes /run/sshd on that stop. sshd will
    # not read a config without it, and host-agent runs `sshd -t` before starting
    # anything, so without the RuntimeDirectoryPreserve drop-in no account can ever
    # turn SSH back on until the box reboots. Only the SECOND enable of a boot
    # catches it.
    gon="$(full_send PUT /api/v1/me/ssh "$apex" "$gone_cookie" \
        "{\"enabled\":true,\"keys\":[{\"public_key\":\"${pubkey}\",\"label\":\"cloud lane\"}]}" 2>/dev/null)"
    grep -q ' 200' <<<"$(status_of "$gon")" \
        || fail "ssh: '$GONE_USER' could not turn SSH on AFTER a previous account turned it off: status='$(status_of "$gon")' — if host-agent logged 'Missing privilege separation directory', the ssh.service RuntimeDirectoryPreserve drop-in is missing and SSH is one-shot per boot"
    wait_port22 open || fail "ssh: :22 never re-opened for '$GONE_USER'"
    grep -qE "^AllowUsers .*\b${GONE_USER}\b" "$DROPIN" \
        || fail "ssh: drop-in does not name '$GONE_USER' after they turned SSH on: $(cat "$DROPIN")"
    [ -f "${KEYSDIR}/${GONE_USER}" ] || fail "ssh: no managed key file for '$GONE_USER'"

    elevate "$apex" "$owner_cookie" "$OWNER_PW" || fail "ssh: re-elevate before deleting the second account failed"
    rmu="$(full_send DELETE "/api/v1/users/${gone_id}" "$apex" "$owner_cookie" '{}' 2>/dev/null)"
    grep -qE ' (204|200)' <<<"$(status_of "$rmu")" \
        || fail "ssh: deleting '$GONE_USER' failed: status='$(status_of "$rmu")'"
    id "$GONE_USER" >/dev/null 2>&1 \
        && fail "ssh: the Linux account '$GONE_USER' still exists after the user was deleted"
    [ -f "${KEYSDIR}/${GONE_USER}" ] \
        && fail "ssh: KEY LEFT BEHIND — ${KEYSDIR}/${GONE_USER} outlived the deleted account; it authenticates a name that could be re-created"
    if [ -f "$DROPIN" ]; then
        grep -qw "$GONE_USER" "$DROPIN" \
            && fail "ssh: the deleted account '$GONE_USER' is still named in $DROPIN: $(cat "$DROPIN")"
    fi
    wait_port22 closed \
        || fail "ssh: :22 still answers after the last enabled account was deleted"
    echo "cloud-assertions: deleting an account with SSH on removed its name, its key file, and closed :22"

    echo "cloud-assertions: hosted SSH verified end-to-end (port opens and closes with the toggle, key required, second factor enforced, deleted account leaves nothing behind)"
    ;;
update)
    # Control-plane update proof (#382): a REAL update and a REAL failed-update-
    # then-revert, on a booted box, driven through the real admin trigger
    # (POST /api/v1/system/update → host-agent's system-update job → internal/
    # hostagent/cpupdate). Everything under that endpoint was proven only against a
    # fake Docker: no real daemon, no real registry, no real brain restart, no real
    # revert. This scenario is where those meet.
    #
    # The riskiest step in the whole design is here: **host-agent recreates the
    # brain while the brain is what served the request that asked for it.** So the
    # happy-path assertions are written to make a failure there unmistakable — the
    # brain container id must change, the running brain must carry the marker label
    # only the new image has, and the box must answer again on the new pair.
    #
    # Pull-by-digest is proven, not simulated: the guest runs its own registry on
    # 127.0.0.1:5000, the target images are pushed into it and then DROPPED from the
    # local image store, so the updater's `docker pull <ref>@sha256:…` has to fetch
    # them back. Docker treats a localhost registry as insecure by default, so this
    # needs no daemon config.
    [ -f "$SEED" ] || fail "update mode but $SEED absent (seed materializer did not run?)"
    box_id="$(json_str "$SEED" box_id)"
    [ -n "$box_id" ] || fail "update mode: could not read box_id from $SEED"
    apex="${box_id}.onmoose.io"

    # 1. owner session. The trigger is admin-only, so this boot is seeded with the
    #    test-portal key and given a signed owner assertion, exactly as the access
    #    boot is (dev/cloud/mkassertion). Driven once — the jti is single-use.
    sso_token="$(tr -d '\r\n' < "${CREDENTIALS_DIRECTORY:-/nonexistent}/moose.sso_token" 2>/dev/null || true)"
    [ -n "$sso_token" ] || fail "update mode: moose.sso_token credential missing (harness did not mint/deliver the owner assertion)"
    sso_resp="$(full_get "/_moose/sso?token=${sso_token}" "$apex" 2>/dev/null || true)"
    grep -q ' 303' <<<"$(status_of "$sso_resp")" \
        || fail "update: SSO landing did not 303 to the dashboard: status='$(status_of "$sso_resp")'"
    session_cookie="$(cookie_val "$sso_resp" moose_session)"
    [ -n "$session_cookie" ] || fail "update: no moose_session cookie from the SSO landing"
    echo "cloud-assertions: update — owner session established (box_id=$box_id)"

    # 2. the in-guest registry. Loaded from the test-only tarball (the production
    #    image ships none of this) and run on the host loopback, where the Docker
    #    daemon that does the pulling can reach it.
    docker load -i /var/lib/moose/test-images/registry.tar >/dev/null 2>&1 \
        || fail "update: could not docker-load the test registry image (/var/lib/moose/test-images/registry.tar missing from the boot-proof image?)"
    docker rm -f moose-test-registry >/dev/null 2>&1 || true
    docker run -d --name moose-test-registry -p 127.0.0.1:5000:5000 registry:2 >/dev/null 2>&1 \
        || fail "update: could not start the in-guest registry container"
    reg=""
    for _i in $(seq 1 90); do
        reg="$(http_status_addr 127.0.0.1 5000 /v2/ 2>/dev/null || true)"
        grep -qE ' (200|401)' <<<"$reg" && break
        sleep 1
    done
    grep -qE ' (200|401)' <<<"$reg" || fail "update: in-guest registry never answered on 127.0.0.1:5000 (last status='$reg'): $(docker logs moose-test-registry 2>&1 | tail -5)"
    echo "cloud-assertions: update — in-guest registry serving on 127.0.0.1:5000"

    # 3. publish a new generation of an image and print the digest ref to update to.
    #    `docker commit`, not `docker build`: the guest is air-gapped and has no Go
    #    toolchain, and commit derives from the image the box is ALREADY running, so
    #    the new brain is the real brain plus one changed thing. Labels merge on
    #    commit, so the derived brain keeps moose.protocol.major and passes the
    #    lockstep guard the way a real release would.
    #
    #    Both local references are dropped after the push. That is what makes the
    #    updater's pull a genuine fetch instead of a no-op over an image that never
    #    left the box — and it is asserted, not assumed.
    publish_gen() { # BASE_REF REPO_TAG [dockerfile-change...] -> prints the digest ref
        local base="$1" repotag="$2"; shift 2
        local tmp=moose-cpupdate-src target="127.0.0.1:5000/${repotag}" args=(commit) c digest
        docker rm -f "$tmp" >/dev/null 2>&1 || true
        docker create --name "$tmp" "$base" >/dev/null 2>&1 || return 1
        for c in "$@"; do args+=(--change "$c"); done
        args+=("$tmp" "$target")
        docker "${args[@]}" >/dev/null 2>&1 || { docker rm -f "$tmp" >/dev/null 2>&1; return 1; }
        docker rm -f "$tmp" >/dev/null 2>&1 || true
        docker push "$target" >/dev/null 2>&1 || return 1
        digest="$(docker inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' "$target" 2>/dev/null | grep '^127\.0\.0\.1:5000/' | head -1)"
        [ -n "$digest" ] || return 1
        docker rmi "$target" >/dev/null 2>&1 || true
        docker rmi "$digest" >/dev/null 2>&1 || true
        printf '%s' "$digest"
    }

    brain_before_id="$(docker inspect -f '{{.Id}}' moose-brain 2>/dev/null || true)"
    brain_before_ref="$(docker inspect -f '{{.Config.Image}}' moose-brain 2>/dev/null || true)"
    ui_before_ref="$(docker inspect -f '{{.Config.Image}}' moose-ui 2>/dev/null || true)"
    [ -n "$brain_before_id" ] && [ -n "$brain_before_ref" ] && [ -n "$ui_before_ref" ] \
        || fail "update: could not read the running control-plane pair (brain id='$brain_before_id' brain='$brain_before_ref' ui='$ui_before_ref')"

    brain_v2="$(publish_gen "$brain_before_ref" moose-brain:v2 'LABEL moose.test.generation=v2')" \
        || fail "update: could not publish the gen-2 brain image to the in-guest registry"
    ui_v2="$(publish_gen "$ui_before_ref" moose-ui:v2 'LABEL moose.test.generation=v2')" \
        || fail "update: could not publish the gen-2 ui image to the in-guest registry"
    # The pull has to be real. If either image is still in the local store the
    # digest pull would be satisfied without the registry, and this whole scenario
    # would prove recreate/revert while quietly skipping what production does.
    for r in "$brain_v2" "$ui_v2"; do
        docker image inspect "$r" >/dev/null 2>&1 \
            && fail "update: $r is still in the local image store before the update — the updater's pull would not be a real registry fetch"
    done
    echo "cloud-assertions: update — gen-2 pair published by digest and dropped locally (brain=$brain_v2 ui=$ui_v2)"

    # Poll one update job to a terminal state. The brain is recreated in the middle
    # of this, so /api is briefly gone: ride through every non-200 rather than
    # treating it as a verdict. The job record lives in host-agent, which stays up,
    # which is the whole reason the job id is host-agent's and not the brain's.
    JOB_RESP=""
    poll_job() { # JOB_ID TIMEOUT_S -> 0 when the job reached completed/failed
        local id="$1" timeout="$2" _i resp=""
        for _i in $(seq 1 "$timeout"); do
            resp="$(full_get "/api/v1/system/update/${id}" "$apex" "$session_cookie" 2>/dev/null || true)"
            if grep -q ' 200' <<<"$(status_of "$resp")" \
                && grep -qE '"status"[[:space:]]*:[[:space:]]*"(completed|failed)"' <<<"$resp"; then
                JOB_RESP="$resp"
                return 0
            fi
            sleep 1
        done
        JOB_RESP="$resp"
        return 1
    }

    # 4. THE HAPPY PATH. Both refs move, so this is the coordinated ship: pull both,
    #    snapshot, declare, recreate both, health-check both, commit.
    up_resp="$(full_send POST /api/v1/system/update "$apex" "$session_cookie" \
        "{\"brain_image\":\"${brain_v2}\",\"ui_image\":\"${ui_v2}\"}" 2>/dev/null || true)"
    grep -q ' 202' <<<"$(status_of "$up_resp")" \
        || fail "update: POST /api/v1/system/update was not accepted: status='$(status_of "$up_resp")'"
    job_id="$(json_str_of "$up_resp" job_id)"
    [ -n "$job_id" ] || fail "update: no job_id in the accepted update response: $(tail -1 <<<"$up_resp")"
    echo "cloud-assertions: update — update job ${job_id} accepted (the brain is now replacing itself)"

    poll_job "$job_id" 420 || fail "update: job $job_id never reached a terminal state (last: $(status_of "$JOB_RESP")) — is the box serving at all after the brain was recreated? $(docker ps --format '{{.Names}} {{.Status}}' | tr '\n' ';')"
    grep -qE '"status"[[:space:]]*:[[:space:]]*"completed"' <<<"$JOB_RESP" \
        || fail "update: the happy-path job did not complete: $(tail -1 <<<"$JOB_RESP")"
    grep -qE '"brain_changed"[[:space:]]*:[[:space:]]*true' <<<"$JOB_RESP" \
        || fail "update: job reports brain_changed=false on a moved brain ref: $(tail -1 <<<"$JOB_RESP")"
    grep -qE '"ui_changed"[[:space:]]*:[[:space:]]*true' <<<"$JOB_RESP" \
        || fail "update: job reports ui_changed=false on a moved ui ref: $(tail -1 <<<"$JOB_RESP")"
    grep -qE '"reverted"[[:space:]]*:[[:space:]]*true' <<<"$JOB_RESP" \
        && fail "update: the happy-path update reverted: $(tail -1 <<<"$JOB_RESP")"

    # 4a. the brain really was replaced — not left running and merely re-declared.
    brain_after_id="$(docker inspect -f '{{.Id}}' moose-brain 2>/dev/null || true)"
    [ -n "$brain_after_id" ] || fail "update: no moose-brain container after the update"
    [ "$brain_after_id" != "$brain_before_id" ] \
        || fail "update: the brain container was NEVER recreated (same id $brain_before_id) — the update reported success without replacing the brain"
    brain_after_ref="$(docker inspect -f '{{.Config.Image}}' moose-brain 2>/dev/null || true)"
    [ "$brain_after_ref" = "$brain_v2" ] \
        || fail "update: the running brain is on '$brain_after_ref', not the target '$brain_v2'"
    gen="$(docker inspect -f '{{index .Config.Labels "moose.test.generation"}}' moose-brain 2>/dev/null || true)"
    [ "$gen" = v2 ] \
        || fail "update: the running brain does not carry the gen-2 marker label (got '$gen') — it is not the image this update targeted"
    ui_after_ref="$(docker inspect -f '{{.Config.Image}}' moose-ui 2>/dev/null || true)"
    [ "$ui_after_ref" = "$ui_v2" ] \
        || fail "update: the running ui is on '$ui_after_ref', not the target '$ui_v2'"
    echo "cloud-assertions: update — both containers recreated on the new pair (brain id $brain_before_id -> $brain_after_id)"

    # 4b. the declaration, in BOTH files (UPDATES.md # 8.3): images.json is what
    #     host-agent reads at the next boot, compose.yml is what the brain
    #     reconciles to. A box whose containers moved but whose declaration did not
    #     silently rolls back on its next reboot.
    ledger=/var/lib/moose/control-plane/images.json
    [ -f "$ledger" ] || fail "update: no ledger at $ledger after a successful update"
    grep -qF "$brain_v2" "$ledger" || fail "update: ledger does not name the new brain ref: $(tr -d '\n' < "$ledger")"
    grep -qF "$ui_v2" "$ledger" || fail "update: ledger does not name the new ui ref: $(tr -d '\n' < "$ledger")"
    grep -qF "$brain_before_ref" "$ledger" \
        || fail "update: ledger does not record the previous brain ref '$brain_before_ref' — there is nothing to roll back to: $(tr -d '\n' < "$ledger")"
    grep -qE "^[[:space:]]*image:[[:space:]]*${ui_v2}\$" /var/lib/moose/control-plane/compose.yml \
        || fail "update: the staged compose does not pin the new ui ref '$ui_v2': $(grep -n 'image:' /var/lib/moose/control-plane/compose.yml | tr '\n' ' ')"
    echo "cloud-assertions: update — declaration written in both files (images.json current+previous, compose.yml ui image)"

    # 4c. the new brain is really serving: /healthz on the container itself (the
    #     same probe the updater uses), and the box answering through Caddy again.
    brain_ip="$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}} {{end}}' moose-brain 2>/dev/null | awk '{print $1}')"
    [ -n "$brain_ip" ] || fail "update: the recreated brain has no address on the ingress network"
    hz=""
    for _i in $(seq 1 60); do
        hz="$(http_status_addr "$brain_ip" 8080 /healthz 2>/dev/null || true)"
        grep -q ' 200' <<<"$hz" && break
        sleep 1
    done
    grep -q ' 200' <<<"$hz" || fail "update: the updated brain does not answer /healthz on $brain_ip:8080 (status='$hz')"
    ver_resp="$(full_get /api/v1/system/version "$apex" "$session_cookie" 2>/dev/null || true)"
    grep -q ' 200' <<<"$(status_of "$ver_resp")" \
        || fail "update: GET /api/v1/system/version after the update: status='$(status_of "$ver_resp")'"
    grep -qF "$ui_v2" <<<"$ver_resp" \
        || fail "update: system/version does not report the new ui image '$ui_v2': $(tail -1 <<<"$ver_resp")"
    echo "cloud-assertions: update — HAPPY PATH OK (brain replaced itself, /healthz 200 on the new image, system/version reports the new pair)"

    # 5. THE REVERT. Point an update at a brain that starts but never serves, and
    #    make it do damage on the way: it truncates the brain's SQLite database and
    #    leaves a marker file. That turns two silent claims into observable facts —
    #    the bad brain really ran (marker present) and the snapshot really came back
    #    (the database is a valid SQLite file again, and the owner session still
    #    works). A revert that restored nothing would leave the clobbered file.
    broken_marker=/var/lib/moose/broken-brain-ran
    rm -f "$broken_marker"
    brain_bad="$(publish_gen "$brain_v2" moose-brain:bad \
        'ENTRYPOINT ["/bin/sh","-c","echo BROKEN > /var/lib/moose/state/moose.db; touch /var/lib/moose/broken-brain-ran; sleep 900"]')" \
        || fail "update: could not publish the deliberately-broken brain image"

    bad_resp="$(full_send POST /api/v1/system/update "$apex" "$session_cookie" \
        "{\"brain_image\":\"${brain_bad}\"}" 2>/dev/null || true)"
    grep -q ' 202' <<<"$(status_of "$bad_resp")" \
        || fail "update: POST of the failing update was not accepted: status='$(status_of "$bad_resp")'"
    bad_job="$(json_str_of "$bad_resp" job_id)"
    [ -n "$bad_job" ] || fail "update: no job_id for the failing update: $(tail -1 <<<"$bad_resp")"

    # The health wait is 60s (UPDATES.md # 3 step 3d) and the revert runs after it,
    # so this window is deliberately wide.
    poll_job "$bad_job" 420 || fail "update: the failing job $bad_job never reached a terminal state (last: $(status_of "$JOB_RESP")) — the box may not have come back from the revert: $(docker ps --format '{{.Names}} {{.Status}}' | tr '\n' ';')"
    grep -qE '"status"[[:space:]]*:[[:space:]]*"failed"' <<<"$JOB_RESP" \
        || fail "update: an update to a brain that never serves was reported as success: $(tail -1 <<<"$JOB_RESP")"
    grep -qE '"reverted"[[:space:]]*:[[:space:]]*true' <<<"$JOB_RESP" \
        || fail "update: the failed update did not revert: $(tail -1 <<<"$JOB_RESP")"
    grep -qE '"failure_mode"[[:space:]]*:[[:space:]]*"health"' <<<"$JOB_RESP" \
        || fail "update: the failed update blames the wrong step (want failure_mode=health): $(tail -1 <<<"$JOB_RESP")"
    grep -q '"revert_error"' <<<"$JOB_RESP" \
        && fail "update: the revert itself failed: $(tail -1 <<<"$JOB_RESP")"

    # 5a. the bad brain really ran. Without this the whole revert half could pass on
    #     a box where the broken image never started, which would prove nothing.
    [ -f "$broken_marker" ] \
        || fail "update: the broken brain never started (no $broken_marker) — the revert proof would be vacuous"

    # 5b. images and declaration are back on the good pair.
    brain_reverted_ref="$(docker inspect -f '{{.Config.Image}}' moose-brain 2>/dev/null || true)"
    [ "$brain_reverted_ref" = "$brain_v2" ] \
        || fail "update: after the revert the brain is on '$brain_reverted_ref', not the previous good ref '$brain_v2'"
    grep -qF "$brain_v2" "$ledger" \
        || fail "update: after the revert the ledger does not name the good brain ref again: $(tr -d '\n' < "$ledger")"
    grep -qF "$brain_bad" "$ledger" \
        && fail "update: after the revert the ledger still names the failed brain ref '$brain_bad' — the next boot would launch it: $(tr -d '\n' < "$ledger")"

    # 5c. the SQLite snapshot was restored over what the bad brain wrote.
    ls -d /var/lib/moose/brain-snapshots/* >/dev/null 2>&1 \
        || fail "update: no pre-update snapshot under /var/lib/moose/brain-snapshots (UPDATES.md # 3 step 3b)"
    db_head="$(head -c 15 /var/lib/moose/state/moose.db 2>/dev/null || true)"
    [ "$db_head" = "SQLite format 3" ] \
        || fail "update: the brain database was NOT restored after the revert (starts with '$db_head', the broken brain's write is still there)"

    # 5d. the box is serving again on the restored pair, with the SAME session — a
    #     restored database that could not answer a signed-in request would be a
    #     restore in name only.
    me=""
    for _i in $(seq 1 90); do
        me="$(status_of "$(full_get /api/v1/me "$apex" "$session_cookie" 2>/dev/null || true)")"
        grep -q ' 200' <<<"$me" && break
        sleep 1
    done
    grep -q ' 200' <<<"$me" \
        || fail "update: after the revert the box does not answer an authenticated /api/v1/me (status='$me') — the restored database or the restored brain is not serving"
    spa_after="$(http_status / "$DASH_HOST" 2>/dev/null || true)"
    grep -q ' 200' <<<"$spa_after" || fail "update: after the revert the dashboard does not answer: status='$spa_after'"
    echo "cloud-assertions: update — REVERT OK (health failure detected, both refs and the SQLite snapshot restored, box serving on the old pair with the same session)"

    # 6. THE TARGET-DRIVEN PATH (os#401). Everything above was driven by an admin
    #    POSTing two refs. On a hosted box nobody types them: host-agent reads an
    #    update-target source and applies what it is handed, in the update window,
    #    with no prompt (UPDATES.md # 8.4). This proves that loop against a real
    #    source, a real registry pull and the real transaction — and, first, proves
    #    it REFUSES an answer that is not pinned to a digest.
    #    The target URL itself is NOT set here. It arrives in this boot's seed
    #    (run-cloud-tests.sh, seed_cred_keyed's third argument), because the seed
    #    is the only channel a real box has for a per-box fact and an environment
    #    drop-in would prove a path production never takes (os#407). The address
    #    below must stay in step with the one that script seeds.
    target_dir=/var/lib/moose/test-target
    mkdir -p "$target_dir"

    write_target() { # BRAIN_REF UI_REF VERSION [WINDOW] -> the answer the box reads
        # The window is optional on the wire (os#408). Left out, the answer has
        # no opinion and the box keeps MOOSE_UPDATE_WINDOW; set, it wins.
        local window=""
        if [ -n "${4:-}" ]; then window=",\"window\":\"$4\""; fi
        printf '{"version":"%s","channel":"stable","brain_image":"%s","brain_digest":"%s","ui_image":"%s","ui_digest":"%s","published_at":"2026-01-01T00:00:00Z"%s,"an_unknown_field":true}\n' \
            "$3" "$1" "${1##*@}" "$2" "${2##*@}" "$window" > "$target_dir/target.json"
    }

    # Restarting host-agent is how a tick is forced: the loop polls every 15
    # minutes but ticks once immediately at startup, and a boot proof cannot wait
    # a quarter of an hour. The drop-in points the repository assertion at the
    # in-guest registry, which is the same configurability a box under test gets
    # in production. The target URL is not here — it comes from the seed.
    #
    # **The window in the drop-in is one minute wide and almost certainly shut.**
    # That is the point (os#408): the apply below can only happen if the window in
    # the ANSWER outranks this variable. A whole-day variable here would let the
    # apply pass whether the box read the answer's window or ignored it.
    mkdir -p /etc/systemd/system/host-agent.service.d
    cat > /etc/systemd/system/host-agent.service.d/30-update-target.conf <<EOF
[Service]
Environment=MOOSE_UPDATE_WINDOW=04:00-04:01
Environment=MOOSE_UPDATE_BRAIN_REPO=127.0.0.1:5000/moose-brain
Environment=MOOSE_UPDATE_UI_REPO=127.0.0.1:5000/moose-ui
EOF
    systemctl daemon-reload || fail "update-target: systemctl daemon-reload failed"

    # The source: a file server on the loopback, run from the Caddy image the box
    # already has, so this needs nothing the boot-proof image does not ship.
    caddy_image="$(docker inspect -f '{{.Config.Image}}' moose-caddy 2>/dev/null || true)"
    [ -n "$caddy_image" ] || fail "update-target: no moose-caddy container to borrow a file-server image from"
    docker rm -f moose-test-target >/dev/null 2>&1 || true
    write_target "127.0.0.1:5000/moose-brain:v3" "127.0.0.1:5000/moose-ui:v3" "v0.0.0-unpinned"
    docker run -d --name moose-test-target -p 127.0.0.1:5001:80 \
        -v "$target_dir":/srv:ro "$caddy_image" \
        caddy file-server --root /srv --listen :80 >/dev/null 2>&1 \
        || fail "update-target: could not start the in-guest update-target file server"
    tgt=""
    for _i in $(seq 1 60); do
        tgt="$(http_status_addr 127.0.0.1 5001 /target.json 2>/dev/null || true)"
        grep -q ' 200' <<<"$tgt" && break
        sleep 1
    done
    grep -q ' 200' <<<"$tgt" || fail "update-target: the in-guest source never answered on 127.0.0.1:5001 (last status='$tgt'): $(docker logs moose-test-target 2>&1 | tail -5)"

    # 6a. THE REFUSAL. The answer names TAGS. A box that pulled them would be
    #     trusting a movable label, so it must refuse and stay exactly where it is.
    brain_id_before_target="$(docker inspect -f '{{.Id}}' moose-brain 2>/dev/null || true)"
    # The baseline for the os#447 check below. Read a host-backed endpoint while
    # host-agent is still the process the brain has always talked to, so the
    # "after" read has something to be compared against — without this, a box
    # that never served this endpoint at all would pass by failing twice.
    target_before="$(status_of "$(full_get /api/v1/system/update-target "$apex" "$session_cookie" 2>/dev/null || true)")"
    grep -q ' 200' <<<"$target_before" \
        || fail "update-target: the brain did not serve a host-backed endpoint BEFORE the host-agent restart (status='$target_before')"
    systemctl restart host-agent.service || fail "update-target: could not restart host-agent"
    sleep 30
    # The channel, before the behaviour: host-agent must have taken this URL out of
    # the seed. Without this the apply below would still pass on a box that read the
    # target from anywhere at all, which is exactly how the credential mechanism
    # stayed green while doing nothing on a real box (os#404 → os#407).
    journalctl -u host-agent.service -b --no-pager 2>&1 | grep -q 'from=seed' \
        || fail "update-target: host-agent did not resolve its target from the seed: $(journalctl -u host-agent.service -b --no-pager 2>&1 | grep -i 'update target' | tail -5)"
    echo "cloud-assertions: update-target — SEED OK (the box took its update target from the provisioning seed)"
    # The box says which box it is. Without the id in the ask, the answer is the
    # same for every box and one box can never be moved on its own (os#408).
    journalctl -u host-agent.service -b --no-pager 2>&1 | grep -q "update target.*box_id=$box_id" \
        || fail "update-target: host-agent did not resolve a box_id to send with its ask: $(journalctl -u host-agent.service -b --no-pager 2>&1 | grep -i 'update target' | tail -5)"
    echo "cloud-assertions: update-target — IDENTITY OK (the box asks as box_id=$box_id)"
    journalctl -u host-agent.service -b --no-pager 2>&1 | grep -q "refusing the answer" \
        || fail "update-target: host-agent did not log a refusal for an unpinned answer: $(journalctl -u host-agent.service -b --no-pager 2>&1 | grep -i 'update target' | tail -5)"
    [ "$(docker inspect -f '{{.Id}}' moose-brain 2>/dev/null || true)" = "$brain_id_before_target" ] \
        || fail "update-target: the box acted on an UNPINNED answer — the brain container was replaced"
    echo "cloud-assertions: update-target — REFUSAL OK (a tagged answer was refused, box unchanged)"

    # 6a-bis. THE SOCKET SURVIVES A PLAIN RESTART (os#447). host-agent was
    #     restarted above. Before the fix, systemd deleted /run/moose when the
    #     unit stopped and made a fresh inode on start, while the brain's bind
    #     mount still pointed at the deleted one — so the brain saw an empty
    #     directory, never saw the new agent.sock, and every host-backed call
    #     answered 502. Nothing closed that window: the brain runs
    #     restart=unless-stopped and host-agent does not touch a running brain,
    #     so the box stayed unable to log ANYONE in until a reboot. The unit now
    #     carries RuntimeDirectoryPreserve=yes, which keeps the inode.
    #
    #     Two halves, and the second is what makes this an assertion rather than
    #     a coincidence: the endpoint answers 200 again, AND it is the same
    #     brain container. A recreate also produces a 200 — that is precisely
    #     how the box "recovers" today — so without the id check this would pass
    #     on the broken build. (6a asserts the same id for the refusal; repeated
    #     here so this check does not depend on that one staying put.)
    target_after=""
    for _i in $(seq 1 60); do
        target_after="$(status_of "$(full_get /api/v1/system/update-target "$apex" "$session_cookie" 2>/dev/null || true)")"
        grep -q ' 200' <<<"$target_after" && break
        sleep 1
    done
    grep -q ' 200' <<<"$target_after" \
        || fail "update-target: the brain cannot reach host-agent after a plain host-agent restart (status='$target_after') — os#447 regression. /run/moose inode now: $(stat -c %i /run/moose 2>&1); RuntimeDirectoryPreserve=$(systemctl show host-agent.service -p RuntimeDirectoryPreserve --value 2>&1); brain mounts: $(docker inspect -f '{{range .Mounts}}{{.Source}} {{end}}' moose-brain 2>&1)"
    [ "$(docker inspect -f '{{.Id}}' moose-brain 2>/dev/null || true)" = "$brain_id_before_target" ] \
        || fail "update-target: the brain container was replaced across the host-agent restart, so the 200 above says nothing about os#447"
    echo "cloud-assertions: update-target — SOCKET SURVIVES OK (a plain host-agent restart left the SAME brain container able to reach it)"

    # 6b. THE APPLY. A pinned gen-3 pair, published and dropped locally like the
    #     ones above, so the loop's apply is a real registry pull.
    brain_v3="$(publish_gen "$(docker inspect -f '{{.Config.Image}}' moose-brain)" moose-brain:v3 'LABEL moose.test.generation=v3')" \
        || fail "update-target: could not publish the gen-3 brain image"
    ui_v3="$(publish_gen "$(docker inspect -f '{{.Config.Image}}' moose-ui)" moose-ui:v3 'LABEL moose.test.generation=v3')" \
        || fail "update-target: could not publish the gen-3 ui image"
    # The answer carries a whole-day window. The drop-in above says 04:00-04:01,
    # so the apply below is only possible if the box took the window from here.
    write_target "$brain_v3" "$ui_v3" "v0.0.0-target-test" "00:00-23:59"
    systemctl restart host-agent.service || fail "update-target: could not restart host-agent for the pinned answer"

    # The window, before the apply it enables. from=answer is the whole claim:
    # where the box got the hour from, not just that it updated.
    window_taken=""
    for _i in $(seq 1 60); do
        journalctl -u host-agent.service -b --no-pager 2>&1 \
            | grep -q 'taking the update window from the answer.*from=answer' && { window_taken=yes; break; }
        sleep 1
    done
    [ -n "$window_taken" ] \
        || fail "update-target: host-agent did not take the update window from the answer: $(journalctl -u host-agent.service -b --no-pager 2>&1 | grep -i 'update target\|window' | tail -5)"
    echo "cloud-assertions: update-target — WINDOW OK (the answer's window outranked MOOSE_UPDATE_WINDOW)"

    applied=""
    for _i in $(seq 1 420); do
        if grep -qF "$brain_v3" "$ledger" 2>/dev/null && grep -qF "$ui_v3" "$ledger" 2>/dev/null; then
            applied=yes
            break
        fi
        sleep 1
    done
    [ -n "$applied" ] \
        || fail "update-target: nothing applied the pinned target within 420s (ledger: $(tr -d '\n' < "$ledger")): $(journalctl -u host-agent.service -b --no-pager 2>&1 | grep -i 'update target\|system-update' | tail -10)"

    # The containers really moved, not just the declaration.
    gen3=""
    for _i in $(seq 1 120); do
        gen3="$(docker inspect -f '{{index .Config.Labels "moose.test.generation"}}' moose-brain 2>/dev/null || true)"
        [ "$gen3" = v3 ] && break
        sleep 1
    done
    [ "$gen3" = v3 ] \
        || fail "update-target: the running brain does not carry the gen-3 marker (got '$gen3') — the ledger moved but the container did not"
    me_after=""
    for _i in $(seq 1 90); do
        me_after="$(status_of "$(full_get /api/v1/me "$apex" "$session_cookie" 2>/dev/null || true)")"
        grep -q ' 200' <<<"$me_after" && break
        sleep 1
    done
    grep -q ' 200' <<<"$me_after" \
        || fail "update-target: after the target-driven update the box does not answer an authenticated /api/v1/me (status='$me_after')"
    echo "cloud-assertions: update-target — APPLY OK (the box read its target, pulled the pinned pair and applied it with no prompt)"

    # 6c. THE READ (os#443). Everything above is journal lines and container
    #     state. The dashboard reads neither, so the same facts have to come
    #     back through the brain. Asserted after the apply so the read covers the
    #     pair the box actually moved to. (It no longer has to be here: os#447 is
    #     fixed, and 6a-bis asserts the brain reaches host-agent across a plain
    #     restart, with no recreate to lean on.)
    #
    #     The claim is end-to-end: the pair the in-guest control plane served is
    #     the pair the brain names. The box is on that pair now, so the state is
    #     `current`; `available` is accepted too, for the tick that has not
    #     re-read the ledger yet.
    target_read=""
    for _i in $(seq 1 120); do
        target_read="$(full_get /api/v1/system/update-target "$apex" "$session_cookie" 2>/dev/null || true)"
        grep -qE '"state":"(available|current)"' <<<"$target_read" && break
        sleep 1
    done
    grep -q ' 200' <<<"$(status_of "$target_read")" \
        || fail "update-target: the brain did not serve /api/v1/system/update-target (status='$(status_of "$target_read")'): $(docker ps --format '{{.Names}} {{.Status}}' 2>&1 | tr '\n' '; ')"
    grep -qF "$brain_v3" <<<"$target_read" && grep -qF "$ui_v3" <<<"$target_read" \
        || fail "update-target: the read does not name the pinned pair the source served: $(tail -1 <<<"$target_read")"
    # The channel, through the read: from=seed is how an operator sees that this
    # box is not following the fleet without opening the journal (os#407).
    grep -q '"from":"seed"' <<<"$target_read" \
        || fail "update-target: the read does not name the seed as the target's source: $(tail -1 <<<"$target_read")"
    # The window came from the answer, and it is a separate field from the one
    # above - two settings whose values only look alike (os#443).
    grep -q '"window_from":"answer"' <<<"$target_read" \
        || fail "update-target: the read does not say the window came from the answer: $(tail -1 <<<"$target_read")"
    # A caller with no session must not learn what this box is being moved to.
    anon_read="$(status_of "$(full_get /api/v1/system/update-target "$apex" "" 2>/dev/null || true)")"
    grep -qE ' (401|403)' <<<"$anon_read" \
        || fail "update-target: the read answered an unauthenticated caller (status='$anon_read')"
    echo "cloud-assertions: update-target — READ OK (the brain reports the pinned pair the in-guest source served, from=seed, window from the answer, admin-only)"
    ;;
remap|remap-reboot)
    # The user-namespace tiers on the booted image (#531, APP_ISOLATION.md #
    # User-namespace tiers). Step 5d above has already checked the daemon side
    # on this boot: name=userns and overlay2, the proxy and the brain in the
    # host userns, Caddy and moose-ui remapped. This scenario checks the apps.
    #
    # `remap` signs the owner in and installs one app per tier: remapdrop
    # (folderless, default tier, root inside), svcdrop (service_user, default
    # tier), rootsetup (root_setup, caps tier), pgnote (default tier, on the
    # managed Postgres) and filedrop household (a folder app, host tier). It
    # checks each one, recreates the caps-tier container and checks that its
    # data is kept. `remap-reboot` boots the same disk again and runs the same
    # checks on what came back. The ids the second boot needs are left in
    # REMAP_STATE on the overlay. Each boot signs in with its own single-use
    # owner assertion, so no session crosses the reboot.
    REMAP_STATE=/var/lib/moose/test/remap-state  # on the state partition, so it survives the reboot
    [ -f "$SEED" ] || fail "remap: $MODE mode but $SEED absent (seed materializer did not run?)"
    box_id="$(json_str "$SEED" box_id)"
    [ -n "$box_id" ] || fail "remap: could not read box_id from $SEED"
    apex="${box_id}.onmoose.io"

    sso_token="$(tr -d '\r\n' < "${CREDENTIALS_DIRECTORY:-/nonexistent}/moose.sso_token" 2>/dev/null || true)"
    [ -n "$sso_token" ] || fail "remap: moose.sso_token credential missing (harness did not mint/deliver the owner assertion)"
    sso_resp="$(full_get "/_moose/sso?token=${sso_token}" "$apex" 2>/dev/null || true)"
    grep -q ' 303' <<<"$(status_of "$sso_resp")" \
        || fail "remap: SSO landing did not 303: status='$(status_of "$sso_resp")'"
    session_cookie="$(cookie_val "$sso_resp" moose_session)"
    fa_cookie="$(cookie_val "$sso_resp" moose_forward_auth)"
    [ -n "$session_cookie" ] && [ -n "$fa_cookie" ] || fail "remap: the SSO landing minted no session or forward-auth cookie"

    # A new SSO owner gets no subordinate id range. The image sets
    # SUB_UID_COUNT 0 and SUB_GID_COUNT 0 before any user exists, so useradd
    # gives the owner none, and no account can use newuidmap to reach the
    # remap range (BUILD.md # User-namespace remap). Checked on both boots: the
    # owner is created on the first.
    owner="$(json_str_of "$(full_get /api/v1/me "$apex" "$session_cookie" 2>/dev/null || true)" username)"
    [ -n "$owner" ] || fail "remap: /api/v1/me named no username for the SSO owner"
    owner_uid="$(id -u "$owner" 2>/dev/null)" || fail "remap: the SSO owner '$owner' has no host account"
    for f in /etc/subuid /etc/subgid; do
        grep -q "^${owner}:" "$f" && fail "remap: the SSO owner '$owner' has a subordinate id range in $f: $(grep "^${owner}:" "$f")"
        f_lines="$(grep -cv '^[[:space:]]*$' "$f")"
        [ "$f_lines" = 1 ] || fail "remap: $f has $f_lines entries, want only moose-remap:${remap_base}:65536: $(tr '\n' ' ' < "$f")"
    done
    echo "cloud-assertions: remap: the SSO owner '$owner' (host uid $owner_uid) has no /etc/subuid or /etc/subgid range; each file holds only moose-remap:${remap_base}:65536"

    # remap_install MANIFEST SCOPE sets R_ID and R_SLUG, or fails with the
    # brain's own error. These helpers set globals instead of printing: a fail
    # inside $(...) would only leave the subshell.
    remap_install() {
        local resp job st="" jr=""
        resp="$(full_send POST /api/v1/apps "$apex" "$session_cookie" "{\"manifest_id\":\"$1\",\"scope\":\"$2\"}" 2>/dev/null)"
        job="$(json_str_of "$resp" job_id)"
        [ -n "$job" ] || fail "remap: install $1 did not start: status='$(status_of "$resp")' body=$(tail -1 <<<"$resp" | cut -c1-400)"
        for _i in $(seq 1 300); do
            jr="$(full_get "/api/v1/jobs/${job}" "$apex" "$session_cookie" 2>/dev/null || true)"
            st="$(json_str_of "$jr" status)"
            case "$st" in completed|failed|cancelled|stalled) break ;; esac
            sleep 1
        done
        [ "$st" = completed ] || fail "remap: install $1 ended '$st': $(grep -o '"error":{[^}]*}' <<<"$jr" | cut -c1-600)"
        R_ID="$(json_str_of "$jr" instance_id)"
        R_SLUG="$(json_str_of "$jr" slug)"
        [ -n "$R_ID" ] && [ -n "$R_SLUG" ] || fail "remap: install $1 completed with no instance id or slug: $(tail -1 <<<"$jr" | cut -c1-400)"
    }
    # remap_ctr INSTANCE_ID SERVICE sets R_CTR to that service's running
    # container, waiting for it (an app comes back a while after a reboot).
    remap_ctr() {
        R_CTR=""
        for _i in $(seq 1 180); do
            R_CTR="$(docker ps --filter "label=moose.instance_id=$1" --filter "label=com.docker.compose.service=$2" --filter status=running --format '{{.Names}}' | head -1)"
            [ -n "$R_CTR" ] && return 0
            sleep 1
        done
        fail "remap: no running container for $2 (instance $1): $(docker ps -a --filter "label=moose.instance_id=$1" --format '{{.Names}} {{.Status}}' | tr '\n' ' ')"
    }
    # remap_get SLUG PATH sets R_BODY to the body of a 200 from the app,
    # through Caddy with the owner's forward-auth cookie.
    remap_get() {
        local r=""
        for _i in $(seq 1 180); do
            r="$(full_get "$2" "$1.$apex" "$fa_cookie" 2>/dev/null || true)"
            if grep -q ' 200' <<<"$(status_of "$r")"; then
                R_BODY="$(sed '1,/^\r*$/d' <<<"$r" | tr -d '\r')"
                return 0
            fi
            sleep 1
        done
        fail "remap: GET $2 on $1.$apex through Caddy never answered 200: status='$(status_of "$r")'; caddy route ids: $(docker exec moose-caddy wget -qO- http://localhost:2019/config/apps/http/servers/moose/routes 2>&1 | grep -o '"@id":"[^"]*"' | tr '\n' ' ')"
    }
    remap_wait_file() { for _i in $(seq 1 60); do [ -s "$1" ] && return 0; sleep 1; done; return 1; }
    remap_owner() { stat -c '%u:%g' "$1" 2>/dev/null || echo "<missing $1>"; }
    remap_nocaps() { case "$1" in null|'[]') return 0 ;; *) return 1 ;; esac; }
    remap_inst() { echo "/var/lib/moose/state/instances/$1"; }

    # remap_checks runs every per-tier check. Both boots run it: after the
    # install, and after the reboot on what the box brought back.
    remap_checks() {
        local c um user capadd puid dir owner f_owner want

        # 1. Folderless, default tier: remapped, root inside, so the process
        #    and its data are host uid base.
        remap_ctr "$RD_ID" remapdrop; c="$R_CTR"
        um="$(docker inspect "$c" --format '{{.HostConfig.UsernsMode}}')"
        user="$(docker inspect "$c" --format '{{.Config.User}}')"
        capadd="$(docker inspect "$c" --format '{{json .HostConfig.CapAdd}}')"
        [ -z "$um" ] || fail "remap: remapdrop UsernsMode is '$um', want the daemon default (remapped)"
        [ "$user" = "0:0" ] || fail "remap: remapdrop runs as '$user', want 0:0 (the brain's own uid)"
        remap_nocaps "$capadd" || fail "remap: remapdrop cap_add is $capadd, want none (default tier)"
        puid="$(host_uid_of "$c")"
        [ "$puid" = "$remap_base" ] || fail "remap: remapdrop runs as host uid '$puid', want $remap_base"
        dir="$(remap_inst "$RD_ID")/data"
        owner="$(remap_owner "$dir")"
        [ "$owner" = "$remap_base:$remap_base" ] || fail "remap: remapdrop data dir is owned $owner, want $remap_base:$remap_base"
        remap_wait_file "$dir/id.txt" || fail "remap: remapdrop wrote no $dir/id.txt"
        f_owner="$(remap_owner "$dir/id.txt")"
        [ "$f_owner" = "$remap_base:$remap_base" ] || fail "remap: remapdrop id.txt is owned $f_owner, want $remap_base:$remap_base"
        remap_get "$RD_SLUG" /id.txt
        grep -q 'uid=0(root)' <<<"$R_BODY" || fail "remap: remapdrop /id.txt through Caddy says '$R_BODY', want uid=0(root)"
        echo "cloud-assertions: remap [$MODE]: default tier, folderless: remapdrop UsernsMode='' user=$user cap_add=$capadd host uid $puid; data dir $owner, id.txt $f_owner; through Caddy: $R_BODY"

        # 2. service_user, default tier: its own identity from the app-service
        #    band, shifted by base on the host.
        remap_ctr "$SD_ID" svcdrop; c="$R_CTR"
        um="$(docker inspect "$c" --format '{{.HostConfig.UsernsMode}}')"
        user="$(docker inspect "$c" --format '{{.Config.User}}')"
        capadd="$(docker inspect "$c" --format '{{json .HostConfig.CapAdd}}')"
        local su_uid="${user%%:*}" su_gid="${user#*:}"
        [ -z "$um" ] || fail "remap: svcdrop UsernsMode is '$um', want the daemon default (remapped)"
        case "$su_uid:$su_gid" in *[!0-9:]*|:*|*:) fail "remap: svcdrop user is '$user', want a numeric uid:gid" ;; esac
        [ "$su_uid" -ge 2100 ] && [ "$su_uid" -lt 3000 ] || fail "remap: svcdrop runs as uid $su_uid, want one from the app-service band (2100 to 2999)"
        remap_nocaps "$capadd" || fail "remap: svcdrop cap_add is $capadd, want none (default tier)"
        puid="$(host_uid_of "$c")"
        [ "$puid" = "$((remap_base + su_uid))" ] || fail "remap: svcdrop runs as host uid '$puid', want $((remap_base + su_uid))"
        dir="$(remap_inst "$SD_ID")/data"
        want="$((remap_base + su_uid)):$((remap_base + su_gid))"
        owner="$(remap_owner "$dir")"
        [ "$owner" = "$want" ] || fail "remap: svcdrop data dir is owned $owner, want $want (base + its uid:gid)"
        remap_wait_file "$dir/id.txt" || fail "remap: svcdrop wrote no $dir/id.txt"
        f_owner="$(remap_owner "$dir/id.txt")"
        [ "$f_owner" = "$want" ] || fail "remap: svcdrop id.txt is owned $f_owner, want $want"
        remap_get "$SD_SLUG" /id.txt
        grep -q "uid=${su_uid}" <<<"$R_BODY" || fail "remap: svcdrop /id.txt through Caddy says '$R_BODY', want uid=$su_uid"
        echo "cloud-assertions: remap [$MODE]: default tier, service_user: svcdrop UsernsMode='' user=$user cap_add=$capadd host uid $puid; data dir $owner, id.txt $f_owner; through Caddy: $R_BODY"

        # 3. root_setup, caps tier: remapped, no user: pin, the five
        #    capabilities back and nothing more. Its start chowns its data to
        #    www-data (33) as root, then drops, so the running process is host
        #    uid base+33 and its data lands there.
        remap_ctr "$RS_ID" rootsetup; c="$R_CTR"
        um="$(docker inspect "$c" --format '{{.HostConfig.UsernsMode}}')"
        user="$(docker inspect "$c" --format '{{.Config.User}}')"
        capadd="$(docker inspect "$c" --format '{{json .HostConfig.CapAdd}}')"
        local caps capdrop secopt
        caps="$(tr -d '[]" ' <<<"$capadd" | tr ',' '\n' | sed 's/^CAP_//' | sort | tr '\n' ' ')"
        capdrop="$(docker inspect "$c" --format '{{json .HostConfig.CapDrop}}')"
        secopt="$(docker inspect "$c" --format '{{json .HostConfig.SecurityOpt}}')"
        [ -z "$um" ] || fail "remap: rootsetup UsernsMode is '$um', want the daemon default (remapped); the caps tier must never be in the host userns"
        [ -z "$user" ] || fail "remap: rootsetup runs with user '$user', want no user: pin (caps tier)"
        [ "$caps" = "CHOWN DAC_OVERRIDE FOWNER SETGID SETUID " ] || fail "remap: rootsetup cap_add is $capadd, want exactly CHOWN SETUID SETGID DAC_OVERRIDE FOWNER"
        grep -q '"ALL"' <<<"$capdrop" || fail "remap: rootsetup cap_drop is $capdrop, want ALL"
        grep -q 'no-new-privileges' <<<"$secopt" || fail "remap: rootsetup security_opt is $secopt, want no-new-privileges"
        puid="$(host_uid_of "$c")"
        [ "$puid" = "$((remap_base + 33))" ] || fail "remap: rootsetup runs as host uid '$puid', want $((remap_base + 33)) (www-data, after the root start)"
        dir="$(remap_inst "$RS_ID")/data"
        owner="$(remap_owner "$dir")"
        [ "$owner" = "$remap_base:$remap_base" ] || fail "remap: rootsetup data dir is owned $owner, want $remap_base:$remap_base (the container's root, caps tier)"
        want="$((remap_base + 33)):$((remap_base + 33))"
        f_owner="$(remap_owner "$dir/db")"
        [ "$f_owner" = "$want" ] || fail "remap: rootsetup data/db is owned $f_owner, want $want (chowned by its root start)"
        remap_wait_file "$dir/db/token" || fail "remap: rootsetup wrote no $dir/db/token"
        [ "$(remap_owner "$dir/db/token")" = "$want" ] || fail "remap: rootsetup token is owned $(remap_owner "$dir/db/token"), want $want"
        remap_get "$RS_SLUG" /token
        RS_TOKEN_NOW="$R_BODY"
        [ -n "$RS_TOKEN_NOW" ] && [ "$RS_TOKEN_NOW" = "$(cat "$dir/db/token")" ] \
            || fail "remap: rootsetup /token through Caddy says '$RS_TOKEN_NOW', the file says '$(cat "$dir/db/token")'"
        echo "cloud-assertions: remap [$MODE]: caps tier, root_setup: rootsetup UsernsMode='' user='$user' cap_add=$capadd cap_drop=$capdrop security_opt=$secopt host uid $puid; data dir $owner, data/db $f_owner; token through Caddy: $RS_TOKEN_NOW"

        # 4. Managed Postgres, default tier: the service runs remapped and its
        #    entrypoint gives PGDATA to postgres (999), so host uid base+999.
        #    pgnote, a folderless app on it, runs remapped as root inside and
        #    its row is in its own database.
        remap_ctr "$PG_ID" pgnote; c="$R_CTR"
        um="$(docker inspect "$c" --format '{{.HostConfig.UsernsMode}}')"
        user="$(docker inspect "$c" --format '{{.Config.User}}')"
        [ -z "$um" ] || fail "remap: pgnote UsernsMode is '$um', want the daemon default (remapped)"
        [ "$user" = "0:0" ] || fail "remap: pgnote runs as '$user', want 0:0"
        puid="$(host_uid_of "$c")"
        [ "$puid" = "$remap_base" ] || fail "remap: pgnote runs as host uid '$puid', want $remap_base"
        local pg=moose-svc-postgres-16 pg_um pg_puid pg_owner pg_db pg_row=""
        for _i in $(seq 1 120); do
            [ "$(docker inspect "$pg" --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ] && break
            sleep 1
        done
        [ "$(docker inspect "$pg" --format '{{.State.Health.Status}}' 2>/dev/null)" = healthy ] \
            || fail "remap: $pg never became healthy: $(docker ps -a --filter "name=$pg" --format '{{.Status}}') log: $(docker logs --tail 5 "$pg" 2>&1 | tr '\n' ' ')"
        pg_um="$(docker inspect "$pg" --format '{{.HostConfig.UsernsMode}}' 2>/dev/null)"
        [ -z "$pg_um" ] || fail "remap: $pg UsernsMode is '$pg_um', want the daemon default (remapped)"
        pg_puid="$(host_uid_of "$pg")"
        [ "$pg_puid" = "$((remap_base + 999))" ] || fail "remap: $pg runs as host uid '$pg_puid', want $((remap_base + 999))"
        pg_owner="$(remap_owner /var/lib/moose/state/services/postgres-16/data)"
        [ "${pg_owner%%:*}" = "$((remap_base + 999))" ] || fail "remap: postgres-16 data dir is owned $pg_owner, want uid $((remap_base + 999))"
        pg_db="$(sed -n 's/^MOOSE_SERVICE_DATABASE_NAME=//p' "$(remap_inst "$PG_ID")/.env")"
        [ -n "$pg_db" ] || fail "remap: pgnote .env names no MOOSE_SERVICE_DATABASE_NAME"
        for _i in $(seq 1 120); do
            pg_row="$(docker exec "$pg" psql -U postgres -d "$pg_db" -tAc 'select v from moose_notes' 2>&1)"
            [ "$pg_row" = moose-531-note ] && break
            sleep 1
        done
        [ "$pg_row" = moose-531-note ] || fail "remap: pgnote's row is not in database $pg_db: '$pg_row'; pgnote log: $(docker logs --tail 5 "$c" 2>&1 | tr '\n' ' ')"
        echo "cloud-assertions: remap [$MODE]: managed Postgres, default tier: $pg UsernsMode='' host uid $pg_puid, data dir $pg_owner; pgnote UsernsMode='' user=$user host uid $puid; its row in database $pg_db: $pg_row"

        # 5. A folder app, host tier: it acts as the real moose-app identity on
        #    the shared tree, so no remap and no capability.
        remap_ctr "$FD_ID" filedrop; c="$R_CTR"
        um="$(docker inspect "$c" --format '{{.HostConfig.UsernsMode}}')"
        user="$(docker inspect "$c" --format '{{.Config.User}}')"
        capadd="$(docker inspect "$c" --format '{{json .HostConfig.CapAdd}}')"
        [ "$um" = host ] || fail "remap: filedrop (household) UsernsMode is '${um:-<empty>}', want host (folder app)"
        [ "$user" = "2000:2000" ] || fail "remap: filedrop (household) runs as '$user', want moose-app 2000:2000"
        remap_nocaps "$capadd" || fail "remap: filedrop cap_add is $capadd, want none (host tier)"
        puid="$(host_uid_of "$c")"
        [ "$puid" = 2000 ] || fail "remap: filedrop runs as host uid '$puid', want the real 2000"
        remap_wait_file /srv/moose/shared/Documents/filedrop.txt || fail "remap: filedrop wrote no /srv/moose/shared/Documents/filedrop.txt"
        f_owner="$(remap_owner /srv/moose/shared/Documents/filedrop.txt)"
        [ "$f_owner" = "2000:2001" ] || fail "remap: /srv/moose/shared/Documents/filedrop.txt is owned $f_owner, want the real 2000:2001"
        echo "cloud-assertions: remap [$MODE]: host tier, folder app: filedrop UsernsMode=host user=$user cap_add=$capadd host uid $puid; /srv/moose/shared/Documents/filedrop.txt $f_owner"
    }

    if [ "$MODE" = remap ]; then
        # The managed service and pgnote run postgres:16. It is test-only and
        # outside the first-boot loader's dir, so only this boot loads it.
        docker load -i /var/lib/moose/test-images/postgres-16.tar >/dev/null 2>&1 \
            || fail "remap: docker load postgres-16.tar failed: $(docker load -i /var/lib/moose/test-images/postgres-16.tar 2>&1 | tail -2)"

        remap_install remapdrop personal; RD_ID="$R_ID"; RD_SLUG="$R_SLUG"
        remap_install svcdrop personal; SD_ID="$R_ID"; SD_SLUG="$R_SLUG"
        remap_install rootsetup personal; RS_ID="$R_ID"; RS_SLUG="$R_SLUG"
        remap_install pgnote personal; PG_ID="$R_ID"
        remap_install filedrop household; FD_ID="$R_ID"
        echo "cloud-assertions: remap: installed remapdrop, svcdrop, rootsetup, pgnote and filedrop (household)"

        remap_checks
        # A restart loop would mean the root start failed at least once.
        rs_restarts="$(docker inspect "$(docker ps --filter "label=moose.instance_id=$RS_ID" --format '{{.Names}}' | head -1)" --format '{{.RestartCount}}')"
        [ "$rs_restarts" = 0 ] || fail "remap: rootsetup restarted $rs_restarts times; its root start should work the first time in the caps tier"
        rs_token="$RS_TOKEN_NOW"

        # The caps-tier app keeps its data across a recreate. Recreate it the
        # way the brain runs compose, so the container is new and its start
        # runs as root again on the data it left.
        remap_ctr "$RS_ID" rootsetup; old_cid="$(docker inspect "$R_CTR" --format '{{.Id}}')"
        rc_out="$(cd "$(remap_inst "$RS_ID")" && docker compose -f compose.yml -f compose.override.yml --env-file .env -p "moose-$RS_ID" up -d --force-recreate 2>&1)" \
            || fail "remap: recreating rootsetup failed: $(tail -3 <<<"$rc_out" | tr '\n' ' ')"
        remap_ctr "$RS_ID" rootsetup; new_cid="$(docker inspect "$R_CTR" --format '{{.Id}}')"
        [ -n "$new_cid" ] && [ "$new_cid" != "$old_cid" ] || fail "remap: rootsetup was not recreated (${old_cid:0:12} -> ${new_cid:0:12})"
        remap_checks
        [ "$RS_TOKEN_NOW" = "$rs_token" ] || fail "remap: rootsetup lost its data on a recreate: token '$rs_token' became '$RS_TOKEN_NOW'"
        echo "cloud-assertions: remap: the caps-tier app kept its data across a container recreate (${old_cid:0:12} -> ${new_cid:0:12}), token $rs_token"

        mkdir -p "$(dirname "$REMAP_STATE")" && chmod 700 "$(dirname "$REMAP_STATE")"
        printf 'RD_ID=%q\nRD_SLUG=%q\nSD_ID=%q\nSD_SLUG=%q\nRS_ID=%q\nRS_SLUG=%q\nPG_ID=%q\nFD_ID=%q\nrs_token=%q\n' \
            "$RD_ID" "$RD_SLUG" "$SD_ID" "$SD_SLUG" "$RS_ID" "$RS_SLUG" "$PG_ID" "$FD_ID" "$rs_token" > "$REMAP_STATE"
        sync
        echo "cloud-assertions: remap: every tier checked on the first boot; ids left for the reboot"
    else
        [ -f "$REMAP_STATE" ] || fail "remap-reboot: no $REMAP_STATE from the first boot (did the remap boot run on this disk?)"
        # shellcheck disable=SC1090
        . "$REMAP_STATE"
        remap_checks
        [ "$RS_TOKEN_NOW" = "$rs_token" ] || fail "remap-reboot: rootsetup lost its data on a reboot: token '$rs_token' became '$RS_TOKEN_NOW'"
        echo "cloud-assertions: remap-reboot: every tier checked again after a real reboot of the same disk; the caps-tier app kept its data (token $rs_token)"
    fi
    ;;
os-update|os-revert)
    # THE A/B OS UPDATE (#563, UPDATES.md # 1). host-agent reads an OS target
    # from its update-target source, downloads the bundle, checks its sha256
    # against the target, has RAUC install it into slot B, switches in the
    # window, and on the next boot keeps or gives up the new slot. The bundle
    # is the boot-proof slot repacked with a host-agent one patch release up
    # (dev/cloud/test/build-os-test-bundle.sh), on a read-only second disk, and
    # served to host-agent by a file server inside the guest. One stage per
    # boot; the stage is on the state partition, so it survives the reboots.
    #   os-update: 1 (slot A) wrong digest refused, install ahead of the
    #              window, switch; 2 (slot B) marked good, everything intact.
    #   os-revert: 1 (slot A) install and switch, host-agent kept off slot B;
    #              2 (slot B) handled at the top of this script; 3 (slot A)
    #              reverted on its own, everything intact.
    [ -f "$SEED" ] || fail "$MODE mode but $SEED absent"
    box_id="$(json_str "$SEED" box_id)"
    apex="${box_id}.onmoose.io"
    os_cred="$(tr -d '\r\n' < "${CREDENTIALS_DIRECTORY:-/nonexistent}/moose.os_test" 2>/dev/null || true)"
    IFS=: read -r os_mode os_ver os_sum <<<"$os_cred"
    [ -n "$os_ver" ] && [ -n "$os_sum" ] || fail "$MODE: moose.os_test credential missing or malformed ('$os_cred')"
    base_ver="$(/usr/lib/moose/host-agent-real --version | awk '{print $2}')"
    stage="$(os_stage)"
    target_dir=/var/lib/moose/test-target
    ha_log() { journalctl -u host-agent.service -b --no-pager -o short-unix 2>&1; }
    wait_ha() { # PATTERN SECONDS WHAT
        for _i in $(seq 1 "$2"); do ha_log | grep -q -- "$1" && return 0; sleep 1; done
        fail "$MODE: $3 within $2 s: $(ha_log | grep -i 'os update\|update target' | tail -8 | cut -c1-400 | tr '\n' ' ')"
    }
    # The admin read of what host-agent decided (BRAIN_UI_PROTOCOL: admin-only).
    os_read() { sed '1,/^\r*$/d' <<<"$(full_get /api/v1/system/update-target "$apex" "$session_cookie" 2>/dev/null || true)" | tr -d '\r'; }
    os_field() { grep -o "\"os\":{.*" <<<"$1" | json_str_of "$(cat)" "$2"; }
    rauc_slot_version() { rauc status --detailed --output-format=json 2>/dev/null | grep -o "\"bootname\":\"$1\"[^]]*" | grep -o '"version":"[^"]*"' | head -1 | cut -d'"' -f4; }
    grubvar() { grub-editenv /efi/grub/grubenv list | sed -n "s/^$1=//p"; }
    boot_gate() {
        local fu ro
        fu="$(systemctl list-units --state=failed --no-legend --plain 2>/dev/null | awk '{print $1}' | tr '\n' ' ')"
        [ -z "${fu// /}" ] || fail "$MODE: failed units before the switch: $fu"
        ro="$(journalctl -b --no-pager -o json 2>/dev/null | grep -v '"CONTAINER_NAME"' | grep -i 'read-only file system' | head -n 3 | cut -c1-400)"
        [ -z "$ro" ] || fail "$MODE: something tried to write to the read-only slot: $ro"
    }
    write_os_target() { # SHA256 WINDOW
        printf '{"version":"os-test","window":"%s","os":[{"version":"%s","bundle_url":"http://127.0.0.1:5001/bundles/os-test.raucb","bundle_sha256":"%s"}]}\n' \
            "$2" "$os_ver" "$1" > "$target_dir/target.json"
    }
    facts() { # the per-box state an OS update must not touch
        echo "shadow=$(grep "^${owner}:" /etc/shadow | cut -d: -f1-2)"
        echo "hostkey=$(ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub 2>/dev/null | awk '{print $2}')"
        echo "machineid=$(cat /etc/machine-id)"
        echo "data=$(cat "/home/${owner}/os-update-data.txt" 2>/dev/null)"
    }

    if [ "$stage" = 1 ]; then
        [ "$BOOTED" = A ] || fail "$MODE: the first boot is on slot '$BOOTED', want A"
        sso_token="$(tr -d '\r\n' < "${CREDENTIALS_DIRECTORY:-/nonexistent}/moose.sso_token" 2>/dev/null || true)"
        [ -n "$sso_token" ] || fail "$MODE: moose.sso_token credential missing"
        sso_resp="$(full_get "/_moose/sso?token=${sso_token}" "$apex" 2>/dev/null || true)"
        grep -q ' 303' <<<"$(status_of "$sso_resp")" || fail "$MODE: SSO landing did not 303: status='$(status_of "$sso_resp")'"
        session_cookie="$(cookie_val "$sso_resp" moose_session)"
        [ -n "$session_cookie" ] || fail "$MODE: no moose_session cookie from the SSO landing"
        owner="$(json_str_of "$(full_get /api/v1/me "$apex" "$session_cookie" 2>/dev/null || true)" username)"
        [ -n "$owner" ] && id -u "$owner" >/dev/null 2>&1 || fail "$MODE: the SSO owner '$owner' has no host account"
        head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n' > "/home/${owner}/os-update-data.txt"
        chown "$owner" "/home/${owner}/os-update-data.txt"
        mkdir -p "$OS_STATE_DIR" && chmod 700 "$OS_STATE_DIR"
        app_id=""
        if [ "$MODE" = os-update ] && [ "$os_mode" = full ]; then
            # An app, so "apps intact" means a container that comes back.
            resp="$(full_send POST /api/v1/apps "$apex" "$session_cookie" '{"manifest_id":"whoami","scope":"personal"}' 2>/dev/null)"
            job="$(json_str_of "$resp" job_id)"
            [ -n "$job" ] || fail "$MODE: install whoami did not start: status='$(status_of "$resp")'"
            st=""; jr=""
            for _i in $(seq 1 300); do
                jr="$(full_get "/api/v1/jobs/${job}" "$apex" "$session_cookie" 2>/dev/null || true)"
                st="$(json_str_of "$jr" status)"
                case "$st" in completed|failed|cancelled|stalled) break ;; esac
                sleep 1
            done
            [ "$st" = completed ] || fail "$MODE: install whoami ended '$st'"
            app_id="$(json_str_of "$jr" instance_id)"
            docker ps --filter "label=moose.instance_id=${app_id}" --filter status=running --format '{{.Names}}' | grep -q . \
                || fail "$MODE: whoami has no running container"
        fi
        printf 'session_cookie=%q\nowner=%q\napp_id=%q\nbase_ver=%q\n' "$session_cookie" "$owner" "$app_id" "$base_ver" > "$OS_STATE_DIR/before"
        facts > "$OS_STATE_DIR/facts"
        echo "cloud-assertions: $MODE: owner '$owner' signed in, data written${app_id:+, whoami installed ($app_id)}; running OS $base_ver on slot A"

        # The bundle disk, the in-guest source, and host-agent pointed at it.
        mkdir -p "$target_dir/bundles"
        bdev="$(blkid -L moose-os-test 2>/dev/null || true)"
        [ -n "$bdev" ] || fail "$MODE: no disk labelled moose-os-test (the harness's bundle disk)"
        mount -o ro "$bdev" "$target_dir/bundles" || fail "$MODE: cannot mount the bundle disk $bdev"
        [ "$(sha256sum "$target_dir/bundles/os-test.raucb" | cut -d' ' -f1)" = "$os_sum" ] || fail "$MODE: the bundle on the disk is not the one the harness named"
        mkdir -p /etc/systemd/system/host-agent.service.d
        cat > /etc/systemd/system/host-agent.service.d/30-os-update-test.conf <<'UNIT'
[Service]
Environment=MOOSE_UPDATE_OS_URL_PREFIX=http://127.0.0.1:5001/
UNIT
        if [ "$MODE" = os-revert ]; then
            # The broken slot: host-agent is kept from starting on slot B
            # only, so the old slot still runs it after the revert.
            cat > /etc/systemd/system/host-agent.service.d/90-os-revert-test.conf <<'UNIT'
[Service]
ExecStartPre=/bin/sh -c 'if grep -qw rauc.slot=B /proc/cmdline; then echo "os-revert test: host-agent is kept off slot B"; exit 1; fi'
UNIT
        fi
        systemctl daemon-reload
        caddy_image="$(docker inspect -f '{{.Config.Image}}' moose-caddy 2>/dev/null || true)"
        docker rm -f moose-test-target >/dev/null 2>&1 || true
        # A shut window: twelve hours from now, one minute wide.
        shut="$(printf '%02d:00-%02d:01' $(( (10#$(date +%H) + 12) % 24 )) $(( (10#$(date +%H) + 12) % 24 )))"
        write_os_target "$(printf '%064d' 0)" "$shut"
        docker run -d --name moose-test-target -p 127.0.0.1:5001:80 -v "$target_dir":/srv:ro "$caddy_image" \
            caddy file-server --root /srv --listen :80 >/dev/null 2>&1 || fail "$MODE: could not start the in-guest file server"
        for _i in $(seq 1 60); do grep -q ' 200' <<<"$(http_status_addr 127.0.0.1 5001 /target.json 2>/dev/null)" && break; sleep 1; done

        if [ "$MODE" = os-update ]; then
            # 1a. A WRONG DIGEST. The signature would pass; the digest the
            #     target names does not, so RAUC never sees the bundle.
            systemctl restart host-agent.service || fail "$MODE: could not restart host-agent"
            wait_ha "update target names; refusing it" 180 "host-agent did not refuse a bundle whose digest the target does not name"
            [ -z "$(rauc_slot_version B)" ] || fail "$MODE: slot B holds '$(rauc_slot_version B)' after a refused download"
            st="$(os_field "$(os_read)" state)"
            [ "$st" = failed ] || fail "$MODE: the update-target read says os.state '$st' after a wrong digest, want failed"
            echo "cloud-assertions: os-update: WRONG DIGEST OK (refused before RAUC saw it; slot B untouched; os.state=failed)"

            if [ "$os_mode" = refuse ]; then
                # 1b-refuse. A run that publishes the OS: the image trusts only
                #     the release root, so the throwaway-signed bundle must be
                #     refused by RAUC even with the right digest.
                write_os_target "$os_sum" "$shut"
                systemctl restart host-agent.service
                wait_ha 'install failed.*rauc install' 300 "RAUC did not refuse the throwaway-signed bundle"
                [ -z "$(rauc_slot_version B)" ] || fail "$MODE: an image with the release keyring INSTALLED a throwaway-signed bundle"
                echo "cloud-assertions: os-update: RELEASE KEYRING OK (the right digest, but RAUC refused the throwaway signature; slot B untouched)"
                boot_gate
                ok
            fi
        fi

        # 1b. INSTALL AHEAD OF THE WINDOW. The right digest and a shut
        #     window: the bundle goes into slot B, and the boot order stays.
        write_os_target "$os_sum" "$shut"
        systemctl restart host-agent.service
        wait_ha "installed into the other slot" 300 "the bundle was not installed into slot B"
        [ "$(rauc_slot_version B)" = "$os_ver" ] || fail "$MODE: slot B holds '$(rauc_slot_version B)', want $os_ver"
        [ "$(grubvar ORDER)" = "A B" ] || fail "$MODE: the boot order moved outside the window: ORDER='$(grubvar ORDER)'"
        for _i in $(seq 1 30); do [ "$(os_field "$(os_read)" state)" = installed ] && break; sleep 1; done
        [ "$(os_field "$(os_read)" state)" = installed ] || fail "$MODE: os.state is '$(os_field "$(os_read)" state)', want installed"
        t_dl="$(ha_log | grep 'downloading the bundle' | tail -1 | awk '{print $1}')"
        t_got="$(ha_log | grep 'bundle downloaded and its digest matches' | tail -1 | awk '{print $1}')"
        t_in="$(ha_log | grep 'installed into the other slot' | tail -1 | awk '{print $1}')"
        echo "cloud-assertions: $MODE: INSTALL AHEAD OK (slot B holds $os_ver, ORDER still 'A B', os.state=installed); measured: download+digest $(awk -v a="$t_dl" -v b="$t_got" 'BEGIN{printf "%.1f", b-a}') s, rauc install $(awk -v a="$t_got" -v b="$t_in" 'BEGIN{printf "%.1f", b-a}') s, bundle $(stat -c %s "$target_dir/bundles/os-test.raucb") bytes"

        # 1c. THE SWITCH. An open window: host-agent puts slot B first and
        #     reboots. The next stage runs on slot B.
        boot_gate
        date +%s > "$OS_STATE_DIR/switch-at"
        set_os_stage 2
        write_os_target "$os_sum" "00:00-23:59"
        systemctl restart host-agent.service
        echo "cloud-assertions: $MODE: window open; waiting for host-agent to switch to slot B and reboot"
        sleep 900
        fail "$MODE: host-agent never switched slots and rebooted: $(ha_log | grep -i 'os update' | tail -6 | tr '\n' ' ')"
    fi

    # The last stage: back on a slot that must be healthy and marked good.
    # shellcheck disable=SC1090
    . "$OS_STATE_DIR/before"
    if [ "$MODE" = os-update ]; then
        [ "$stage" = 2 ] && [ "$BOOTED" = B ] || fail "os-update: stage $stage booted slot '$BOOTED', want stage 2 on slot B"
        wait_ha "the new slot is healthy and marked good" 300 "host-agent did not mark the new slot good"
        [ ! -e /var/lib/moose/os-update/trial-B ] || fail "os-update: the trial marker for slot B is still there"
        [ "$(grubvar ORDER)" = "B A" ] && [ "$(grubvar B_OK)" = 1 ] && [ "$(grubvar B_TRY)" = 0 ] \
            || fail "os-update: grubenv after mark-good: $(grub-editenv /efi/grub/grubenv list | tr '\n' ' ')"
        [ "$(/usr/lib/moose/host-agent-real --version | awk '{print $2}')" = "$os_ver" ] || fail "os-update: slot B's host-agent is not $os_ver"
        want_outcome=good; want_state=current; want_ver="$os_ver"
        t_good="$(ha_log | grep 'the new slot is healthy and marked good' | tail -1 | awk '{print $1}')"
        echo "cloud-assertions: os-update: SWITCH OK (booted slot B, marked good, grubenv ORDER='B A' B_OK=1 B_TRY=0); measured: switch to marked good $(awk -v a="$(cat "$OS_STATE_DIR/switch-at")" -v b="$t_good" 'BEGIN{printf "%.0f", b-a}') s, the reboot included"
        note_pat="updated its system to $os_ver"
    else
        [ "$stage" = 3 ] && [ "$BOOTED" = A ] || fail "os-revert: stage $stage booted slot '$BOOTED', want stage 3 back on slot A"
        wait_ha "the box went back to the old slot" 120 "host-agent did not record the revert"
        [ ! -e /var/lib/moose/os-update/trial-B ] || fail "os-revert: the trial marker for slot B is still there"
        [ "$(grubvar B_OK)" = 0 ] && [ "$(grubvar A_OK)" = 1 ] || fail "os-revert: grubenv after the revert: $(grub-editenv /efi/grub/grubenv list | tr '\n' ' ')"
        [ "$(/usr/lib/moose/host-agent-real --version | awk '{print $2}')" = "$base_ver" ] || fail "os-revert: back on slot A, host-agent is not $base_ver"
        want_outcome=reverted; want_state=held; want_ver="$base_ver"
        echo "cloud-assertions: os-revert: REVERT OK (slot B never came up; back on slot A, B marked bad, A good); measured: switch to back on slot A $(( $(date +%s) - $(cat "$OS_STATE_DIR/switch-at") )) s, two reboots and the safety net included"
        note_pat="did not work, so moose went back"
    fi
    # What the brain reports, and what the box kept.
    me=""
    for _i in $(seq 1 90); do me="$(status_of "$(full_get /api/v1/me "$apex" "$session_cookie" 2>/dev/null || true)")"; grep -q ' 200' <<<"$me" && break; sleep 1; done
    grep -q ' 200' <<<"$me" || fail "$MODE: the owner's session does not work after the OS move (status '$me')"
    ver_body="$(full_get /api/v1/system/version "$apex" "$session_cookie" 2>/dev/null || true)"
    [ "$(json_str_of "$ver_body" os_version)" = "$want_ver" ] && [ "$(json_str_of "$ver_body" os_slot)" = "$BOOTED" ] \
        || fail "$MODE: /api/v1/system/version reports os_version '$(json_str_of "$ver_body" os_version)' os_slot '$(json_str_of "$ver_body" os_slot)', want $want_ver $BOOTED"
    rd=""; for _i in $(seq 1 60); do rd="$(os_read)"; [ "$(os_field "$rd" state)" = "$want_state" ] && break; sleep 2; done
    [ "$(os_field "$rd" state)" = "$want_state" ] && [ "$(os_field "$rd" outcome)" = "$want_outcome" ] \
        || fail "$MODE: the update-target read says os.state '$(os_field "$rd" state)' outcome '$(os_field "$rd" outcome)', want $want_state $want_outcome: $(grep -o '"os":{.*' <<<"$rd" | cut -c1-400)"
    now_facts="$(facts)"
    [ "$now_facts" = "$(cat "$OS_STATE_DIR/facts")" ] \
        || fail "$MODE: per-box state changed across the OS move: before [$(tr '\n' ' ' < "$OS_STATE_DIR/facts")] now [$(tr '\n' ' ' <<<"$now_facts")]"
    if [ -n "$app_id" ]; then
        for _i in $(seq 1 180); do docker ps --filter "label=moose.instance_id=${app_id}" --filter status=running --format '{{.Names}}' | grep -q . && break; sleep 1; done
        docker ps --filter "label=moose.instance_id=${app_id}" --filter status=running --format '{{.Names}}' | grep -q . \
            || fail "$MODE: the whoami app did not come back after the OS move"
    fi
    echo "cloud-assertions: $MODE: INTACT OK (owner '$owner', session, password hash, SSH host key, machine-id, data file${app_id:+ and the whoami app} unchanged; os_version $want_ver on slot $BOOTED; os.state=$want_state, last outcome $want_outcome)"
    nb=""
    for _i in $(seq 1 150); do nb="$(full_get /api/v1/notifications "$apex" "$session_cookie" 2>/dev/null || true)"; grep -q "$note_pat" <<<"$nb" && break; sleep 2; done
    grep -q "$note_pat" <<<"$nb" || fail "$MODE: no admin notification '$note_pat': $(sed '1,/^\r*$/d' <<<"$nb" | cut -c1-400)"
    echo "cloud-assertions: $MODE: NOTIFY OK (admins were told: '$note_pat')"
    ;;
*)
    fail "unknown assert mode '$MODE'"
    ;;
esac

echo "cloud-assertions: control plane up, dashboard + /api served through Caddy; gate scenario '$MODE' OK"
ok
