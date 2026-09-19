#!/usr/bin/env bash
# Medium-lane test driver: QEMU + swtpm + SSH-driven assertions.
#
# Boots the LUKS-encrypted image through TWO sequential QEMU processes
# against one shared disk overlay + OVMF vars + swtpm state (slice 0023
# Stages 2-3):
#   boot 1  recovery-passphrase credential supplied → initrd unlocks the
#           TPM-less root → run-once unit enrolls a PCR-7 TPM2 keyslot.
#   boot 2  no credential → root can only unlock via that TPM2 token, so
#           reaching multi-user proves unattended PCR-7 unseal.
# Each boot SSHes in, runs /usr/local/bin/medium-assertions.sh <phase>
# (baked into the image at build time), scp's the verdict back, powers
# off. See docs/specs/TESTING.md # Medium lane and
# docs/progress/0023-luks-tpm-enrollment.md.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="${REPO_ROOT}/.dev/qemu"
IMAGE_OUT="${WORK}/moose-medium.raw"
SSH_KEY="${WORK}/ssh-key"
PASSPHRASE_FILE="${REPO_ROOT}/dev/test-qemu/mkosi.passphrase"

# Per-run ephemeral state.
RUN_DIR="$(mktemp -d -t moose-medium.XXXXXX)"
SWTPM_DIR="${RUN_DIR}/swtpm"
SWTPM_SOCK="${SWTPM_DIR}/sock"
RESULT_FILE="${RUN_DIR}/result"
QEMU_SERIAL="${RUN_DIR}/serial.log"
QEMU_PID=""
SWTPM_PID=""

# QEMU writes serial.log as root (this script runs under sudo). Resolve
# the invoking user so we can hand diagnostics back caller-readable —
# otherwise the kept log is root-owned and undebuggable without sudo.
CALLER="${SUDO_USER:-}"
if [ -z "$CALLER" ] || [ "$CALLER" = "root" ]; then
    CALLER="$(logname 2>/dev/null || true)"
fi

# Copy the serial log somewhere the caller can read, and surface the
# lines that actually explain a boot failure. The kernel/systemd
# dependency-failure cascade is downstream noise; the load-bearing line
# is the systemd-cryptsetup ExecStart error just above it. tail alone
# cuts it off, so grep the unlock path explicitly with context.
dump_serial() {
    [ -r "$QEMU_SERIAL" ] || return 0
    local saved="${WORK}/last-serial.log"
    cp "$QEMU_SERIAL" "$saved" 2>/dev/null || true
    if [ -n "$CALLER" ]; then
        chown "$CALLER":"$(id -gn "$CALLER" 2>/dev/null || echo "$CALLER")" \
            "$saved" 2>/dev/null || true
    fi
    echo "--- serial: unlock path (cryptsetup/luks/credential/tpm2) ---" >&2
    grep -niE 'cryptsetup|luks|credential|passphrase|tpm2|unlock|sysroot|switch-root' \
        "$QEMU_SERIAL" 2>/dev/null | tail -40 >&2 || true
    echo "--- serial: tail 30 ---" >&2
    tail -30 "$QEMU_SERIAL" >&2 || true
    echo "--- full serial log saved (caller-readable): ${saved} ---" >&2
}

cleanup() {
    local rc=$?
    if [ -n "$QEMU_PID" ] && kill -0 "$QEMU_PID" 2>/dev/null; then
        kill -KILL "$QEMU_PID" 2>/dev/null || true
    fi
    if [ -n "$SWTPM_PID" ] && kill -0 "$SWTPM_PID" 2>/dev/null; then
        kill -TERM "$SWTPM_PID" 2>/dev/null || true
    fi
    # Keep RUN_DIR on failure for debugging; clean only on success.
    if [ "$rc" -eq 0 ]; then
        rm -rf "$RUN_DIR"
    else
        echo "run artifacts kept at $RUN_DIR (serial log: $QEMU_SERIAL)" >&2
    fi
    return "$rc"
}
trap cleanup EXIT

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    echo "must run as root (QEMU+KVM, mkosi build)" >&2
    exit 1
fi

# --- 1. always invoke bootstrap; it has its own canary-version gate
# and exits fast when the cached image is current. Skipping bootstrap
# here would make `CANARY_VERSION` bumps invisible to the driver.
"${REPO_ROOT}/dev/test-qemu/bootstrap.sh"

# --- 2. resolve OVMF firmware (varies by distro; bootstrap already
# confirmed one exists, repeat the probe here for hermeticity).
OVMF_CODE=""
OVMF_VARS_TEMPLATE=""
for cand in /usr/share/OVMF/OVMF_CODE.fd \
            /usr/share/ovmf/OVMF.fd \
            /usr/share/OVMF/OVMF.fd \
            /usr/share/edk2-ovmf/x64/OVMF_CODE.fd; do
    if [ -r "$cand" ]; then OVMF_CODE="$cand"; break; fi
done
for cand in /usr/share/OVMF/OVMF_VARS.fd \
            /usr/share/ovmf/OVMF_VARS.fd \
            /usr/share/edk2-ovmf/x64/OVMF_VARS.fd; do
    if [ -r "$cand" ]; then OVMF_VARS_TEMPLATE="$cand"; break; fi
done
[ -n "$OVMF_CODE" ] || { echo "OVMF code firmware not found" >&2; exit 1; }

# Per-run VARS copy (firmware variables — writable).
OVMF_VARS="${RUN_DIR}/OVMF_VARS.fd"
if [ -n "$OVMF_VARS_TEMPLATE" ]; then
    cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"
else
    # Some distros ship OVMF as a single file (combined CODE+VARS).
    # Fall back to using OVMF_CODE for both, with snapshot.
    OVMF_VARS=""
fi

# --- 3. allocate a free TCP port for SSH forward
SSH_PORT="$(python3 -c 'import socket; s=socket.socket(); s.bind(("",0)); print(s.getsockname()[1]); s.close()' 2>/dev/null \
    || echo 0)"
if [ "$SSH_PORT" = "0" ] || [ -z "$SSH_PORT" ]; then
    # Fallback: scan for a free port in the 20000-40000 range.
    for p in $(seq 20000 40000); do
        if ! ss -ltn "sport = :$p" 2>/dev/null | grep -q LISTEN; then
            SSH_PORT="$p"; break
        fi
    done
fi
[ -n "$SSH_PORT" ] || { echo "could not allocate ssh forward port" >&2; exit 1; }
echo "ssh forward: 127.0.0.1:${SSH_PORT} -> guest:22"

# --- 4. swtpm lifecycle: one emulator instance *per boot*, both against
# the same persistent --tpmstate dir. swtpm exits when its QEMU client
# disconnects, so a single shared instance can't span the two sequential
# QEMU processes (the first boot's poweroff tears it down, leaving the
# second boot with no socket to connect to). Relaunching per boot is also
# the faithful model: it's a TPM *power cycle* — volatile PCRs reset and
# get re-measured to identical values by the (identical) firmware, while
# the persistent SRK + NVRAM live in the state dir, so the PCR-7-sealed
# keyslot enrolled in boot 1 unseals in boot 2.
mkdir -p "$SWTPM_DIR"

start_swtpm() {
    local phase="$1"
    # Per-phase log so a boot-2 failure doesn't clobber boot-1's TPM log
    # (the enroll-vs-unseal interaction is exactly what you'd diff).
    local log="${SWTPM_DIR}/log-${phase}"
    swtpm socket \
        --tpmstate "dir=$SWTPM_DIR" \
        --ctrl "type=unixio,path=$SWTPM_SOCK" \
        --tpm2 \
        --daemon \
        --pid "file=${SWTPM_DIR}/pid" \
        --log "file=${log},level=20"
    # swtpm --daemon backgrounds and writes the pid file.
    SWTPM_PID=""
    sleep 0.2
    [ -f "${SWTPM_DIR}/pid" ] && SWTPM_PID="$(cat "${SWTPM_DIR}/pid")"
    # Wait (bounded) for the control socket before QEMU tries to connect.
    for _i in $(seq 1 50); do
        [ -S "$SWTPM_SOCK" ] && break
        sleep 0.1
    done
    # Fail fast if swtpm never came up — otherwise QEMU fails to connect
    # and the driver mis-reports it as "qemu died before sshd", masking
    # the real cause (the swtpm log holds it).
    [ -S "$SWTPM_SOCK" ] || {
        echo "swtpm control socket never appeared; see ${log}" >&2
        return 1
    }
}

stop_swtpm() {
    [ -n "$SWTPM_PID" ] || return 0
    kill -TERM "$SWTPM_PID" 2>/dev/null || true
    for _i in $(seq 1 20); do
        kill -0 "$SWTPM_PID" 2>/dev/null || break
        sleep 0.1
    done
    kill -KILL "$SWTPM_PID" 2>/dev/null || true
    SWTPM_PID=""
    rm -f "$SWTPM_SOCK"
}

# --- 5. launch QEMU
ACCEL=tcg
if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
    ACCEL=kvm
fi

# LUKS recovery passphrase (slice 0023). Supplied to the *first* boot as
# a systemd credential over SMBIOS type 11 — the mkosi-initrd's
# systemd-cryptsetup@.service ImportCredential drop-in consumes
# `cryptsetup.passphrase` to unlock the root before any TPM2 token
# exists. The second boot deliberately omits it so the only way in is
# the PCR-7-bound TPM2 keyslot enrolled on the first boot.
[ -r "$PASSPHRASE_FILE" ] || { echo "missing $PASSPHRASE_FILE (run bootstrap)" >&2; exit 1; }
LUKS_PASS="$(cat "$PASSPHRASE_FILE")"
LUKS_CRED="type=11,value=io.systemd.credential:cryptsetup.passphrase=${LUKS_PASS}"

# Writable per-run disk overlay. The image must persist mutations
# *between* the two boots (the first-boot TPM2 enrollment writes a new
# keyslot into the on-disk LUKS header that the second boot reads back),
# so snapshot=on (which discards on exit, and resets between QEMU
# processes) won't do. A qcow2 overlay backed by the read-only golden
# raw keeps the golden image clean while persisting writes for this run.
OVERLAY="${RUN_DIR}/disk.qcow2"
qemu-img create -q -f qcow2 -F raw -b "$IMAGE_OUT" "$OVERLAY"

# Base QEMU args shared by both boots. The disk overlay, OVMF vars, and
# swtpm state dir all persist across the two QEMU processes so the TPM
# seal enrolled in boot 1 unseals in boot 2 (identical firmware + PCRs).
qemu_base_args() {
    QEMU_ARGS=(
        -machine "q35,accel=${ACCEL}"
        # -cpu host requires KVM (QEMU rejects it under TCG with "kvm
        # required by -cpu host"); fall back to -cpu max on the no-KVM
        # software-emulation path so the harness still launches.
        -cpu "$([ "$ACCEL" = kvm ] && echo host || echo max)"
        # 2G, not 1G: the LUKS2 recovery keyslot uses Argon2id, whose
        # memory cost systemd-repart calibrated on the big-RAM *build
        # host* (libcryptsetup default caps it near 1 GiB). Unlocking
        # re-allocates that buffer, and a 1 GiB guest — minus the
        # initramfs unpacked into RAM and the kernel — can't, so the
        # unlock died with "Not enough available memory to open a
        # keyslot" / "Cannot allocate memory". 2 GiB clears it with
        # headroom, and is also more representative of a real moose box
        # (an old laptop, never 1 GiB). Harness-only change — the built
        # image is untouched, so no rebuild/canary bump needed.
        -m 2G
        -smp 2
        -nographic
        -serial "file:${QEMU_SERIAL}"
        -monitor none
        -drive "file=${OVERLAY},if=virtio,format=qcow2"
        -drive "if=pflash,format=raw,readonly=on,file=${OVMF_CODE}"
        -chardev "socket,id=chrtpm,path=${SWTPM_SOCK}"
        -tpmdev "emulator,id=tpm0,chardev=chrtpm"
        -device tpm-crb,tpmdev=tpm0
        # Three NICs (#130 network-state slice). NIC1 carries SSH exactly
        # as before (systemd-networkd DHCP; NM-unmanaged via its MAC — see
        # mkosi.postinst.chroot). NIC2/NIC3 are NetworkManager-managed LAN
        # interfaces on isolated usernets with distinct subnets so their
        # DHCP addresses differ. MACs are pinned because the in-image
        # config partitions networkd vs NM by MAC, not by the
        # slot-dependent predictable interface name.
        #
        # restrict=on AIR-GAPS the guest (#167): SLIRP routes no guest packets
        # to the host or the outside, so a missing bundled image hard-fails
        # instead of silently pulling from Docker Hub — proving the offline
        # image bundle is complete (TESTING.md # Full-stack control-plane
        # integration). It does NOT affect explicit forwarding rules (the SSH
        # hostfwd still works) or SLIRP's own DHCP (the NM LAN NICs still get
        # leases), so the network-state and SSH assertions are unaffected.
        # NOTE: this is a permanent fixture — hermeticity is the point. A future
        # assertion that genuinely needs outbound (e.g. an online registry-pull /
        # update-path test) would hard-fail SILENTLY under restrict=on; such a
        # test must drop restrict on its own netdev (or run in a separate,
        # non-air-gapped lane), not relax it here.
        -netdev "user,id=mn1,restrict=on,hostfwd=tcp::${SSH_PORT}-:22"
        -device "virtio-net-pci,netdev=mn1,mac=52:54:00:6d:6c:01"
        -netdev "user,id=mn2,restrict=on,net=10.0.3.0/24"
        -device "virtio-net-pci,netdev=mn2,mac=52:54:00:6d:6c:02"
        -netdev "user,id=mn3,restrict=on,net=10.0.4.0/24"
        -device "virtio-net-pci,netdev=mn3,mac=52:54:00:6d:6c:03"
        -no-reboot
    )
    if [ -n "$OVMF_VARS" ]; then
        QEMU_ARGS+=( -drive "if=pflash,format=raw,file=${OVMF_VARS}" )
    fi
}

# --- 6. SSH options shared by both boots. The hostfwd port is reused
# across boots — they run strictly sequentially (boot 1 fully exits
# before boot 2 launches), so there's no contention.
SSH_OPTS=(
    -p "$SSH_PORT"
    -i "$SSH_KEY"
    -o StrictHostKeyChecking=no
    -o UserKnownHostsFile=/dev/null
    -o LogLevel=ERROR
    -o ConnectTimeout=2
    -o BatchMode=yes
)

# Bounded wait for the current QEMU to exit (clean poweroff), SIGKILL
# fallback after 15s — same pattern as the fast lane's container kill.
# Clears QEMU_PID so the EXIT trap doesn't double-kill.
kill_qemu() {
    [ -n "$QEMU_PID" ] || return 0
    for _i in $(seq 1 15); do
        kill -0 "$QEMU_PID" 2>/dev/null || break
        sleep 1
    done
    if kill -0 "$QEMU_PID" 2>/dev/null; then
        kill -KILL "$QEMU_PID" 2>/dev/null || true
    fi
    wait "$QEMU_PID" 2>/dev/null || true
    QEMU_PID=""
    # Tear down this boot's TPM emulator too — swtpm is relaunched per
    # boot against the shared state dir (see start_swtpm).
    stop_swtpm
}

# Boot the image once and run one phase of in-VM assertions against it.
# The disk overlay, OVMF vars, and swtpm state all persist across the two
# calls (created/launched once, above), so the TPM2 keyslot the first
# boot enrolls into the on-disk LUKS header unseals on the second boot
# under an identical firmware + PCR-7 measurement. Sets the global VERDICT
# to the phase's PASS / FAIL string; returns non-zero on any failure.
#
#   run_boot <phase> [extra qemu args...]
run_boot() {
    local phase="$1"; shift
    QEMU_SERIAL="${RUN_DIR}/serial-${phase}.log"
    QEMU_PID=""

    qemu_base_args
    QEMU_ARGS+=( "$@" )

    echo "=== boot phase=${phase} (accel=${ACCEL}) ==="
    # Fresh TPM emulator for this boot (shared persistent state dir).
    if ! start_swtpm "$phase"; then
        VERDICT="FAIL: swtpm did not start (phase ${phase})"
        return 1
    fi
    qemu-system-x86_64 "${QEMU_ARGS[@]}" &
    QEMU_PID=$!

    echo "waiting for sshd..."
    local up=""
    for _i in $(seq 1 90); do
        if ssh "${SSH_OPTS[@]}" "root@127.0.0.1" true 2>/dev/null; then
            echo "ssh up (${_i}s)"; up=1; break
        fi
        if ! kill -0 "$QEMU_PID" 2>/dev/null; then
            echo "qemu (phase=${phase}) died before sshd came up. serial:" >&2
            dump_serial
            stop_swtpm
            VERDICT="FAIL: qemu died before sshd (phase ${phase})"
            return 1
        fi
        sleep 1
    done
    if [ -z "$up" ]; then
        echo "ssh never reachable after 90s (phase=${phase}). serial:" >&2
        dump_serial
        kill_qemu
        VERDICT="FAIL: ssh never reachable (phase ${phase})"
        return 1
    fi

    echo "running in-VM assertions (phase=${phase})..."
    ssh "${SSH_OPTS[@]}" "root@127.0.0.1" \
        "/usr/local/bin/medium-assertions.sh ${phase}" || true
    # scp the verdict back (note: scp uses -P for port, not -p).
    scp -P "$SSH_PORT" -i "$SSH_KEY" \
        -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o LogLevel=ERROR -o ConnectTimeout=2 -o BatchMode=yes \
        "root@127.0.0.1:/var/lib/moose-medium-result" \
        "$RESULT_FILE" 2>/dev/null || true
    VERDICT="$(cat "$RESULT_FILE" 2>/dev/null || true)"

    # Clean shutdown so the LUKS-header writes (first-boot enrollment)
    # flush to the overlay before the next boot reads them back.
    ssh "${SSH_OPTS[@]}" "root@127.0.0.1" "systemctl poweroff" 2>/dev/null || true
    kill_qemu

    if [ -z "$VERDICT" ]; then
        echo "no verdict written (phase=${phase}). serial:" >&2
        dump_serial
        VERDICT="FAIL: no verdict (phase ${phase})"
        return 1
    fi
    if [ "$VERDICT" != "PASS" ]; then
        dump_serial
        return 1
    fi
    return 0
}

# --- 7. two-boot cycle (slice 0023 Stages 2 + 3).
#
# Boot 1 (enrollment): supply the recovery-passphrase credential over
# SMBIOS so the initrd can unlock the still-TPM-less root; the run-once
# moose-tpm-enroll.service then adds the PCR-7-bound TPM2 keyslot.
if ! run_boot "first-boot" -smbios "$LUKS_CRED"; then
    echo "medium-lane test: ${VERDICT}" >&2
    exit 1
fi
echo "first boot OK — TPM2 keyslot enrolled (PCR 7)"

# Boot 2 (unseal): deliberately NO credential. The recovery keyslot is
# unreachable (no passphrase, serial-only console), so the only way root
# unlocks is the TPM2 token enrolled above — booting to multi-user is the
# unattended-unseal proof. Same overlay + swtpm + OVMF vars as boot 1, so
# PCR 7 reproduces and the sealed keyslot opens.
if ! run_boot "second-boot"; then
    echo "medium-lane test: ${VERDICT}" >&2
    exit 1
fi
echo "second boot OK — unattended PCR-7 TPM2 unseal"

echo "medium-lane test: PASS"
exit 0
