#!/usr/bin/env bash
# Cloud-lane end-to-end: boot proof + hosted /setup gate + first-run wizard (C2
# #205; C3a cloud-lane #220; C5 #209): build the hosted cloud image, convert it to
# the qcow2 cloud artifact, and boot it in QEMU to prove the control plane comes up,
# the hosted first-boot provisioning seed + admin-bootstrap gate work, AND the new
# admin can drive the trimmed first-run wizard to completion — the box becomes a
# working, admin-owned, served, first-run-complete moose. The cloud analogue of
# dev/test-qemu/run-medium-tests.sh, MINUS swtpm + LUKS (no TPM/disk encryption in
# hosted — "the disk IS the installed system", ENVIRONMENT.md # Provisioning), PLUS
# the seed delivery + wizard the medium lane has no analogue for.
#
# Every boot runs twice, once under UEFI (OVMF) and once under legacy BIOS
# (SeaBIOS), each firmware on its own overlays (#561; #277 is why BIOS matters).
# Per firmware: three sequential boots over ONE persisted qcow2 overlay (so the
# brain's box-id + first admin carry boot→boot), then the boots that each take
# their own fresh overlay. One virtio NIC with restrict=on (air-gapped: the seed
# arrives over SMBIOS, never the network), serial-log capture per boot. The in-VM
# self-check (cloud-assertions.sh, run by moose-cloud-assertions.
# service) reads which scenario to assert from a `moose.assert` SMBIOS credential,
# writes its verdict to the serial console, and powers the box off cleanly on PASS
# (no SSH in hosted — ENVIRONMENT.md # Access & files). This driver greps the verdict:
#
#   boot 1  un-seeded   no seed → GET /_moose/sso ⇒ 503; /setup ⇒ 403 (gate armed)
#   boot 2  seeded      seed A over SMBIOS (with a complete acme-dns enrollment) →
#                       assertion key ingested → a bad token on GET /_moose/sso ⇒ 401
#                       (verifier armed); /setup ⇒ 403; brain logged 'provisioning
#                       seed ingested' under box_id A; the brain APPLIES the wildcard-
#                       TLS config (acme-dns DNS-01 issuer + :443 bound) — no real cert
#                       (air-gapped), just that the box reaches and binds it (#278)
#   boot 3  frozen      a DIFFERENT seed B delivered, same overlay → the brain ignores
#                       it (identity frozen in SQLite); the dashboard + /api still serve
#                       under box_id A and the brain does not re-ingest
#   access              own overlay + box-id, seeded with a TEST-PORTAL key the harness
#                       holds → a real owner session, then the per-app forward-auth
#                       access modes end-to-end through real Caddy (#308), and the
#                       hosted confirm step (#469): a real elevation-class write that
#                       is refused before the portal round-trip and passes after it
#   update              own overlay + box-id, same test-portal key → a real control-
#                       plane update (pull by digest from a registry inside the guest,
#                       brain recreates itself, both files declared), a real
#                       failed-update-then-revert (#382), and the target-driven path:
#                       host-agent reads an update-target source and applies it with
#                       no prompt, refusing an unpinned answer (#401)
#   remap, remap-reboot own overlay + box-id, same test-portal key → one app per
#                       userns tier installed and checked (#531), then a real reboot
#                       of the same disk and every check again
#   ssh                 own overlay + box-id, same test-portal key → the per-account
#                       SSH opt-in against a REAL sshd (#467): :22 closed at boot,
#                       the port opening and closing with the toggle, a real login a
#                       key passes and a password alone does not, and a deleted
#                       account leaving no key behind
#
# The positive SSO path (a valid portal assertion → owner auto-create → box session →
# first-run wizard) needs the portal's private signing key, so it is the joint cloud
# on-ramp acceptance (cloud docs/ops/e2e-onramp.md), not this box-only boot lane.
#
# The seed is delivered as a systemd credential over SMBIOS type 11 (the same
# mechanism the medium lane uses for the LUKS passphrase; on a real cloud the same
# seed.json arrives via cloud-init). moose-seed.service materializes it to
# /var/lib/moose/seed.json before host-agent launches the brain.
#
# See docs/specs/TESTING.md # Full-stack control-plane integration,
# docs/progress/cloud-vm-boot-proof.md, docs/progress/cloud-seed-delivery.md, and
# docs/progress/cloud-e2e-test.md.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="${REPO_ROOT}/.dev/cloud-boot"
IMAGE_OUT="${WORK}/moose-cloud.raw"
VERSION="$(git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
QCOW2="${WORK}/moose-${VERSION}-amd64.qcow2"

RUN_DIR="$(mktemp -d -t moose-cloud.XXXXXX)"
OVERLAY=""   # set per firmware and per boot below (new_overlay)
QEMU_SERIAL="${RUN_DIR}/serial.log"  # set per-phase by run_boot
QEMU_PID=""
VERDICT=""

# The seed's two box-ids. Boot 2 provisions A; boot 3 re-delivers B and must be
# ignored, so /login still reports A.
BOX_ID_A=cindy-fox
BOX_ID_B=rusty-hawk
# The access boot (#308) provisions its OWN box on a fresh overlay, seeded with a
# test-portal key so it can mint a real owner session — separate identity from A/B.
BOX_ID_ACCESS=owl-harbor
# The update boot (#382) also provisions its OWN box on a fresh overlay: it is the
# one scenario that replaces the box's control-plane images, so it must never run
# over an overlay another boot depends on.
BOX_ID_UPDATE=pine-otter
# The ssh boot (#467) provisions its OWN box on a fresh overlay too: it creates and
# deletes accounts and rewrites the box's sshd config, so it must never run over an
# overlay another boot depends on.
BOX_ID_SSH=heron-birch
# The remap boots (#531) share ONE overlay of their own and one box-id: the
# second boot is a reboot of the disk the first one installed apps on.
BOX_ID_REMAP=moss-lynx
# The OS update boots (#563) each provision their OWN box on a fresh overlay:
# they write slot B and reboot the box between slots, so no other boot may see
# that disk.
BOX_ID_OS_UPDATE=fern-stoat
BOX_ID_OS_REVERT=wren-maple

# Which boots to run, space-separated (unseeded seeded frozen access update ssh remap).
# Default: all.
# A subset lets a caller run only the boots it needs — notably the cloud-image
# publish gate, which runs "unseeded seeded access update ssh remap" to prove the built image's
# brain accepts the current seed schema (the regression that gate exists for) and
# leaves out the frozen-identity boot. Frozen is orthogonal to that gate (it
# checks that a re-delivered seed is IGNORED on a later boot) and has shown a
# flaky false "re-ingested" verdict in CI whose root cause is still open — so
# it is kept in the default full run (where it can be triaged) but out of the
# publish gate. Order still holds: frozen reuses the overlay seeded leaves
# behind, so run seeded whenever frozen runs.
#
# These boots stand outside that ordering, each on its OWN fresh overlay:
#   - `access` (#308, #469) proves the per-app forward-auth access modes end-to-end
#     (restricted gate, owner proxy-through, public, whole-Cookie-header strip) and
#     the hosted confirm step that opens the re-auth window without a password. It
#     is ALSO in the publish gate: the gate it proves is on by default for every
#     hosted app (DECISIONS.md 2026-07-08), and this is its only real-Caddy net, so a
#     box that leaks its forward-auth cookie to an app upstream must fail publish.
#   - `ssh` (#467) proves the SSH daemon lifecycle and the per-account opt-in on a
#     real sshd. It is in the gate too: on hosted the daemon's run state is the ONLY
#     control over :22, so an image that boots with sshd running, or that cannot
#     close the port again, must not publish. It creates and deletes accounts and
#     rewrites the box's sshd config, so it takes its own overlay.
#   - `update` (#382, #401) proves the control-plane updater against a real Docker
#     daemon, a real registry inside the guest, a real brain restart, a real revert,
#     and the update-target loop that drives all of it on a hosted box. It
#     REPLACES the box's control-plane images, so its own overlay is not a
#     convenience — sharing one would leave every later boot on images this scenario
#     built.
#   - `remap` (#531) proves the user-namespace tiers on the booted image. It is
#     TWO boots over one fresh overlay of its own: the first installs one app per
#     tier and checks each, the second is a real reboot of the same disk that
#     checks them all again. One name runs both, because the second needs what
#     the first left on the disk.
# All of these are in the gate: see ci-cloud-image.yml.
#   - `os-update` and `os-revert` (#563) prove the A/B OS update on the booted
#     image. `os-update`: a wrong digest is refused, the bundle installs into
#     slot B ahead of the window, the box switches in the window, boots slot B
#     and marks it good, with the owner, an app, the SSH host keys and the data
#     intact. `os-revert`: the same bundle, but host-agent cannot start on
#     slot B, so the image's safety-net timer reboots the box and it comes back
#     on slot A on its own; then slot B is made active again and crashes its
#     kernel in the initramfs, and GRUB must skip it (#575). Each reboots its
#     box inside one QEMU run, so
#     neither passes -no-reboot. Both need the test bundle from
#     dev/cloud/test/build-os-test-bundle.sh (MOOSE_CLOUD_OS_BUNDLE_DIR).
BOOTS="${MOOSE_CLOUD_BOOTS:-unseeded seeded frozen access update ssh remap os-update os-revert}"
# Which firmwares run the boots (#561): by default every boot runs under UEFI and
# again under legacy BIOS. `bios` used to be a boot of its own (one un-seeded
# boot under SeaBIOS); it is a firmware now.
FIRMWARES="${MOOSE_CLOUD_FIRMWARES:-uefi bios}"
for f in $FIRMWARES; do
    case "$f" in uefi|bios) ;; *) echo "unknown firmware '$f' in MOOSE_CLOUD_FIRMWARES='$FIRMWARES' (known: uefi bios)" >&2; exit 1 ;; esac
done
should_run() { case " $BOOTS " in *" $1 "*) return 0 ;; *) return 1 ;; esac; }
# Refuse a name this script does not know. The workflow lets a person type the
# list (its `boots` input), and a typo would otherwise run nothing and pass.
# A list of only spaces splits into no names at all, and would run nothing.
boot_count=0
for b in $BOOTS; do
    boot_count=$((boot_count + 1))
    case "$b" in
        unseeded|seeded|frozen|access|update|ssh|remap|os-update|os-revert) ;;
        bios) echo "'bios' is no longer a boot: every boot runs under both firmwares (MOOSE_CLOUD_FIRMWARES, default 'uefi bios')" >&2; exit 1 ;;
        *) echo "unknown boot '$b' in MOOSE_CLOUD_BOOTS='$BOOTS' (known: unseeded seeded frozen access update ssh remap os-update os-revert)" >&2; exit 1 ;;
    esac
done
[ "$boot_count" -gt 0 ] || { echo "MOOSE_CLOUD_BOOTS='$BOOTS' names no boot" >&2; exit 1; }

# QEMU writes serial logs as root (this script runs under sudo). Resolve the
# invoking user so kept diagnostics are caller-readable.
CALLER="${SUDO_USER:-}"
if [ -z "$CALLER" ] || [ "$CALLER" = "root" ]; then CALLER="$(logname 2>/dev/null || true)"; fi
if [ "$CALLER" = "root" ]; then CALLER=""; fi  # root-shell edge case: no caller to chown back to

dump_serial() {
    [ -r "$QEMU_SERIAL" ] || return 0
    local saved="${WORK}/last-serial.log"
    cp "$QEMU_SERIAL" "$saved" 2>/dev/null || true
    [ -n "$CALLER" ] && chown "$CALLER":"$(id -gn "$CALLER" 2>/dev/null || echo "$CALLER")" "$saved" 2>/dev/null || true
    echo "--- serial: control-plane / assertion lines ---" >&2
    grep -niE 'cloud-assertions|moose|docker|caddy|brain|host-agent|networkd|fail' "$QEMU_SERIAL" 2>/dev/null | tail -40 >&2 || true
    # The whole diag block too. The tail above keeps only 40 lines, and a red
    # boot with many steps (the remap boots, #531) cuts the brain log out of it.
    # GRUB's own messages (OVMF mirrors the EFI console to the serial port).
    echo "--- serial: GRUB errors ---" >&2
    grep -aE '^error:|grub.*error|save_env|grubenv' "$QEMU_SERIAL" 2>/dev/null | head -20 >&2 || true
    echo "--- serial: diag block ---" >&2
    sed -n '/=== MOOSE_CLOUD_DIAG ===/,/=== END MOOSE_CLOUD_DIAG ===/p' "$QEMU_SERIAL" 2>/dev/null | grep -v '^-A \|^:\|^\*' | cut -c1-2000 >&2 || true
    echo "--- serial: tail 30 ---" >&2
    tail -30 "$QEMU_SERIAL" >&2 || true
    echo "--- full serial log saved (caller-readable): ${saved} ---" >&2
}

kill_qemu() {
    if [ -n "$QEMU_PID" ] && kill -0 "$QEMU_PID" 2>/dev/null; then
        kill -KILL "$QEMU_PID" 2>/dev/null || true
        wait "$QEMU_PID" 2>/dev/null || true
    fi
    QEMU_PID=""
}

cleanup() {
    local rc=$?
    kill_qemu
    if [ "$rc" -eq 0 ]; then rm -rf "$RUN_DIR"; else
        echo "run artifacts kept at $RUN_DIR" >&2
    fi
    return "$rc"
}
trap cleanup EXIT

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
    echo "must run as root (QEMU+KVM, mkosi build)" >&2
    exit 1
fi

# --- 1 + 2. The image to boot. MOOSE_CLOUD_QCOW2 names a qcow2 that is already
# built, and then this script builds nothing. CI uses it: one job builds the
# boot-proof image once, and each boot job downloads it (ci-cloud-image.yml).
# Without it, this script builds the image and converts it, as below.
if [ -n "${MOOSE_CLOUD_QCOW2:-}" ]; then
    [ -f "$MOOSE_CLOUD_QCOW2" ] || { echo "MOOSE_CLOUD_QCOW2='${MOOSE_CLOUD_QCOW2}' is not a file" >&2; exit 1; }
    QCOW2="$(cd "$(dirname "$MOOSE_CLOUD_QCOW2")" && pwd)/$(basename "$MOOSE_CLOUD_QCOW2")"
    mkdir -p "$WORK"
    echo "booting the prebuilt image ${QCOW2} (MOOSE_CLOUD_QCOW2); not building"
else
# --- 1. build (own canary gate; fast when current).
"${REPO_ROOT}/dev/cloud/test/bootstrap.sh"

# --- 2. qcow2 cloud artifact (BUILD.md # 6: qemu-img convert raw -> qcow2). This
# stays the pristine deliverable; the boots write to a throwaway overlay over it.
echo "converting raw -> qcow2 cloud artifact: $(basename "$QCOW2")"
qemu-img convert -f raw -O qcow2 "$IMAGE_OUT" "$QCOW2"
[ -n "$CALLER" ] && chown "$CALLER":"$(id -gn "$CALLER" 2>/dev/null || echo "$CALLER")" "$QCOW2" 2>/dev/null || true
fi

# Writable overlays backed by the pristine artifact; the base is never written.
# Each is DISK_SIZE, far bigger than the image (about 1.13 GiB: the 128 MiB ESP,
# the BIOS boot partition and the 1 GiB slot A), the way a provider disk is.
# First boot fills the rest with slot B (1 GiB) and the state partition, and
# the layout check (cloud-assertions.sh 1b) proves the state partition grew to
# the end of it.
DISK_SIZE="${MOOSE_CLOUD_DISK_SIZE:-24G}"
new_overlay() { # PATH
    qemu-img create -f qcow2 -b "$QCOW2" -F qcow2 "$1" "$DISK_SIZE" >/dev/null
}


# --- 3. resolve OVMF firmware (varies by distro): a CODE image and the VARS
# template that matches it, as a pair. One VARS copy, reused across boots so
# the EFI state persists like a real machine power-cycle.
#
# The VARS store is required (#575). Without a writable one (the combined
# /usr/share/ovmf/OVMF.fd attached read-only, which is what this lane used to
# pick on Ubuntu 24.04), OVMF keeps its variables in memory and saves them to
# an `NvVars` file on the ESP through its own FAT driver at every boot. GRUB's
# save of the TRY flag did not survive that: GRUB read its write back, but
# the initramfs found the grubenv sectors as they were before GRUB ran (runs
# 37061482623 and 37064755329). With a VARS store there is no NvVars, and the
# TRY flag stays. A real UEFI keeps
# its variables in flash and never writes the ESP. layout checks there is no
# NvVars on the ESP (cloud-assertions.sh), so this cannot come back silently.
OVMF_CODE=""
OVMF_VARS_TEMPLATE=""
for pair in /usr/share/OVMF/OVMF_CODE_4M.fd:/usr/share/OVMF/OVMF_VARS_4M.fd \
            /usr/share/OVMF/OVMF_CODE.fd:/usr/share/OVMF/OVMF_VARS.fd \
            /usr/share/edk2/x64/OVMF_CODE.4m.fd:/usr/share/edk2/x64/OVMF_VARS.4m.fd \
            /usr/share/edk2/ovmf/OVMF_CODE.fd:/usr/share/edk2/ovmf/OVMF_VARS.fd \
            /usr/share/edk2-ovmf/x64/OVMF_CODE.fd:/usr/share/edk2-ovmf/x64/OVMF_VARS.fd; do
    if [ -r "${pair%%:*}" ] && [ -r "${pair#*:}" ]; then
        OVMF_CODE="${pair%%:*}"; OVMF_VARS_TEMPLATE="${pair#*:}"; break
    fi
done
[ -n "$OVMF_CODE" ] || { echo "OVMF firmware not found: need a CODE image and its VARS template (package: ovmf)" >&2; exit 1; }
OVMF_VARS="${RUN_DIR}/OVMF_VARS.fd"
cp "$OVMF_VARS_TEMPLATE" "$OVMF_VARS"
echo "OVMF: code ${OVMF_CODE}, vars from ${OVMF_VARS_TEMPLATE}"

ACCEL=tcg
if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then ACCEL=kvm; fi

# Build a compact seed JSON for a box-id + a given verification key, and base64-
# encode it for an SMBIOS binary credential. Two parts mirror a real cloud seed:
#   - assertion_verification_key: a standard-base64 32-byte Ed25519 public key — the
#     wire shape of a real portal key — so the box loads its SSO verifier. The
#     unseeded/seeded/frozen boots pass a RANDOM value (no matching private key ⇒
#     only the verifier's rejection path runs); the access boot passes a real
#     TEST-PORTAL public key whose private half the harness holds (dev/cloud/
#     mkassertion), so a valid owner assertion verifies and the positive session
#     path runs.
#   - enrollment: a COMPLETE acme-dns credential block, so the brain runs its
#     wildcard-TLS pass (cmd/brain EnsureWildcardTLS) — configures Caddy's acme-dns
#     DNS-01 issuer for "*.<box-id>.onmoose.io" and binds :443. The values are
#     inert here: air-gapped (restrict=on) the box never reaches acme-dns/Let's
#     Encrypt, so no real cert issues — the lane asserts the brain APPLIES the
#     config and :443 comes up (the #278 regression class), not that a cert exists.
#     A seed with no enrollment (the prior shape) skipped that pass entirely, which
#     is exactly why CI never caught a hosted box failing to bind :443.
#   - update_target_url: OPTIONAL third argument, and the only boot that passes it
#     is the update boot (os#407). The seed is the only channel a real box has for
#     a per-box fact, so this is the one place the lane can prove that channel;
#     an environment drop-in would prove a path production never takes. Left out
#     everywhere else, which is what an un-steered box receives — those boots keep
#     the dead-port MOOSE_UPDATE_TARGET_URL from bootstrap.sh.
# Prints `io.systemd.credential.binary:moose.seed=<base64>`.
seed_cred_keyed() { # box_id key_b64 [update_target_url] -> SMBIOS value string
    local box_id="$1" key="$2" target="${3:-}" json target_field=""
    # An `if`, not `[ … ] && …`: under `set -e` a false test as the whole
    # statement takes the script down with it.
    if [ -n "$target" ]; then
        target_field="$(printf '"update_target_url":"%s",' "$target")"
    fi
    json="$(printf '{"box_id":"%s","assertion_verification_key":"%s",%s"enrollment":{"subdomain":"%s","username":"%s","password":"%s"}}' \
        "$box_id" "$key" "$target_field" "cloud-lane-acmedns-subdomain" "cloud-lane-acmedns-user" "cloud-lane-acmedns-pass")"
    printf 'io.systemd.credential.binary:moose.seed=%s' "$(printf '%s' "$json" | base64 -w0)"
}
# The unkeyed boots: a random 32-byte key (this box holds no matching private key).
seed_cred() { seed_cred_keyed "$1" "$(head -c 32 /dev/urandom | base64 -w0)"; }

# Resolve go for the access boot's assertion mint. Sudo strips PATH, so a caller's
# pipx/asdf-style go under ~/.local may be invisible; fall back to common locations.
# Only the access boot needs it — resolution failure is surfaced there, not here.
GO="${GO:-$(command -v go 2>/dev/null || true)}"
if [ -z "$GO" ] && [ -n "$CALLER" ]; then
    CALLER_HOME="$(getent passwd "$CALLER" | cut -d: -f6 2>/dev/null || true)"
    for cand in "${CALLER_HOME}/.local/go/bin/go" /usr/local/go/bin/go; do
        [ -x "$cand" ] && { GO="$cand"; break; }
    done
fi

# Mint the access boot's test-portal keypair + one or more valid owner assertions.
# Prints the seed public key, then one signed token per requested assertion
# (dev/cloud/mkassertion). Runs as the invoking caller under sudo so it uses their
# warm Go build cache (root's is cold and offline-hostile); the private key stays on
# the host — only the public key (into the seed) and the signed tokens (into the VM)
# cross into the box, exactly as a real portal would hand them over.
#
# A scenario that needs two tokens needs two mints: the box spends a jti on first
# use, so one token cannot serve both a sign-in and a later portal round-trip. The
# access boot asks for two — the second drives the hosted confirm step (os#469).
mint_owner_assertion() { # box_id [count] -> "<pubkey_b64>\n<token>[\n<token>...]"
    if [ -n "$CALLER" ]; then
        sudo -u "$CALLER" env "HOME=$(getent passwd "$CALLER" | cut -d: -f6)" \
            "$GO" -C "$REPO_ROOT" run ./dev/cloud/mkassertion -box "$1" -tokens "${2:-1}"
    else
        "$GO" -C "$REPO_ROOT" run ./dev/cloud/mkassertion -box "$1" -tokens "${2:-1}"
    fi
}

# Boot the overlay once and run one scenario of in-VM assertions. The assert mode
# is delivered as a text SMBIOS credential; any extra args (the seed credential)
# are appended. Sets VERDICT; returns non-zero on failure.
#   run_boot <phase> <mode> [extra -smbios args...]
run_boot() {
    local phase="$1" mode="$2"; shift 2
    QEMU_SERIAL="${RUN_DIR}/serial-${phase}.log"
    QEMU_PID=""
    VERDICT=""

    local firmware="${FIRMWARE:-uefi}"
    local qemu_args=(
        -machine "q35,accel=${ACCEL}"
        -cpu "$([ "$ACCEL" = kvm ] && echo host || echo max)"
        -m 2G
        -smp 2
        -nographic
        -serial "file:${QEMU_SERIAL}"
        -monitor none
        -drive "file=${OVERLAY},if=virtio,format=qcow2"
    )
    # UEFI attaches OVMF; BIOS (#277) attaches nothing so QEMU uses its built-in
    # SeaBIOS — the legacy-BIOS firmware a Hetzner CX (Intel) VM presents, under
    # which a UEFI-only image hangs at "Booting from Hard Disk". This is what
    # exercises the grub BIOS boot path; the OVMF-only lane could never catch #277.
    if [ "$firmware" = uefi ]; then
        qemu_args+=( -drive "if=pflash,format=raw,readonly=on,file=${OVMF_CODE}" )
    fi
    qemu_args+=(
        -netdev "user,id=n0,restrict=on"
        -device "virtio-net-pci,netdev=n0,mac=52:54:00:c1:0d:01"
        -smbios "type=11,value=io.systemd.credential:moose.assert=${mode}"
        "$@"
    )
    # A boot that reboots its own box (the OS update boots) keeps QEMU up
    # across the reboot; every other boot ends QEMU on a reboot.
    if [ -z "${KEEP_REBOOTS:-}" ]; then qemu_args+=( -no-reboot ); fi
    if [ "$firmware" = uefi ]; then
        qemu_args+=( -drive "if=pflash,format=raw,file=${OVMF_VARS}" )
    fi

    echo "=== boot phase=${phase} mode=${mode} firmware=${firmware} (accel=${ACCEL}, air-gapped) ==="
    qemu-system-x86_64 "${qemu_args[@]}" &
    QEMU_PID=$!

    # Wait for the verdict on the serial console. First boot does docker load +
    # brain bootstrap + compose up, so allow a generous window; cloud-assertions.sh
    # polls the stack up internally. 480s (not 360): the in-VM assertion widened its
    # flush-lag-tolerant log waits, so this outer budget must exceed the sum of the
    # guest's internal polls — otherwise a slow TCG boot times out here as a false
    # "no verdict" before the guest can emit PASS/FAIL. VERDICT_TIMEOUT lets a boot
    # that does MORE in-guest work raise it: the access boot adds SSO + an app install
    # + the exposure toggle on top of the shared prechecks, so its worst-case internal
    # poll sum is ~180s higher and it runs with a wider ceiling.
    local timeout="${VERDICT_TIMEOUT:-480}"
    local v=""
    for _i in $(seq 1 "$timeout"); do
        if grep -q 'MOOSE_CLOUD_ASSERTIONS:' "$QEMU_SERIAL" 2>/dev/null; then
            v="$(grep -o 'MOOSE_CLOUD_ASSERTIONS:.*' "$QEMU_SERIAL" | tail -1 | tr -d '\r')"
            break
        fi
        if ! kill -0 "$QEMU_PID" 2>/dev/null; then
            echo "qemu (phase=${phase}) exited before a verdict. serial:" >&2
            dump_serial
            VERDICT="FAIL: qemu died before verdict (phase ${phase})"
            return 1
        fi
        sleep 1
    done
    VERDICT="$v"
    if [ -z "$v" ]; then
        echo "no verdict on the serial console after ${timeout}s (phase=${phase}). serial:" >&2
        dump_serial
        kill_qemu
        VERDICT="FAIL: no verdict (phase ${phase}, timeout)"
        return 1
    fi
    echo "phase=${phase} verdict: ${v}"
    # Match the verdict EXACTLY, not as a substring. The guest emits either
    # "MOOSE_CLOUD_ASSERTIONS: PASS" or "MOOSE_CLOUD_ASSERTIONS: FAIL: <reason>",
    # and the old `*PASS*` glob read any failure whose REASON happened to contain
    # the letters "pass" as a pass. That is not hypothetical: a new assertion
    # failing with "PATH GATE BYPASS — ..." turned a genuinely red access boot
    # into a green CI run, printing "boot access OK" under a FAIL verdict it had
    # just echoed (#415). Any word like bypass/passphrase/password in a failure
    # message re-opens it, so anchor on the verdict word itself.
    case "$v" in
        "MOOSE_CLOUD_ASSERTIONS: PASS"*)
            # Print what the guest proved, not only that it passed. A PASS used to
            # discard every `cloud-assertions:` line (dump_serial runs on failure
            # only), so a scenario that silently stopped asserting — a section
            # skipped, a proof that never ran — was indistinguishable from one that
            # held. These lines are the evidence, and they belong in the CI log.
            grep -o 'cloud-assertions:.*' "$QEMU_SERIAL" 2>/dev/null | tr -d '\r' | sed 's/^/  /' || true
            # GRUB's own error lines, if any (#563: under UEFI GRUB does not
            # save the try flag; this is where it would say why).
            grep -aE '^error:|save_env|grubenv' "$QEMU_SERIAL" 2>/dev/null | tr -d '\r' | head -10 | sed 's/^/  grub: /' || true
            # On PASS the guest powers itself off (cloud-assertions.sh ok()); wait
            # for QEMU to exit so the overlay write (box-id + admin) flushes before
            # the next boot reads it. Bounded — kill if the clean shutdown hangs.
            for _i in $(seq 1 60); do
                kill -0 "$QEMU_PID" 2>/dev/null || break
                sleep 1
            done
            kill_qemu
            return 0
            ;;
        *)
            dump_serial
            kill_qemu
            return 1
            ;;
    esac
}

# --- 4. every boot, once per firmware (#561). The provider's server type decides
# the firmware (ENVIRONMENT.md # Boot (hosted)), so the whole list runs under UEFI
# (OVMF) and again under legacy BIOS (SeaBIOS, QEMU's own firmware, as Hetzner CX
# presents it). Each firmware gets its own overlays, so the two never share a disk.
for FIRMWARE in $FIRMWARES; do
OVERLAY="${RUN_DIR}/overlay-${FIRMWARE}.qcow2"
new_overlay "$OVERLAY"
echo "=== firmware ${FIRMWARE}: boots ${BOOTS} ==="

# --- 4. boot 1: un-seeded. No seed credential → the brain stays unprovisioned and
# GET /_moose/sso returns 503 and /setup returns 403 (the SSO gate is armed but
# closed — never the appliance's open empty-box behavior). Also the standalone C2
# control-plane-up proof.
if should_run unseeded; then
if ! run_boot "${FIRMWARE}-unseeded" "unseeded"; then
    echo "cloud gate proof: ${VERDICT}" >&2
    exit 1
fi
echo "boot 1 OK — control plane up, hosted SSO gate armed (503, unprovisioned)"
fi

# --- 5. boot 2: seeded. Deliver seed A → the brain ingests the assertion key; a
# bad/unsigned token on /_moose/sso is 401 (the verifier is armed) and /setup is 403
# (disabled on hosted). The ingested box-id A persists on the overlay. The positive
# owner-create + wizard path needs the portal private key (cloud on-ramp).
if should_run seeded; then
if ! run_boot "${FIRMWARE}-seeded" "seeded" -smbios "type=11,value=$(seed_cred "$BOX_ID_A")"; then
    echo "cloud gate proof: ${VERDICT}" >&2
    exit 1
fi
echo "boot 2 OK — seed ingested (assertion key loaded, bad-token 401), box_id=${BOX_ID_A}"
fi

# --- 6. boot 3: frozen identity. Re-deliver a DIFFERENT seed B over the SAME
# overlay. The brain loads its persisted box-id A from SQLite and ignores the new
# seed; the dashboard + /api still serve under box_id A and the brain does not
# re-ingest. Proves a re-delivered or changed seed cannot re-key a provisioned box
# (MOOSE_NETWORK.md frozen identity).
if should_run frozen; then
if ! run_boot "${FIRMWARE}-frozen" "frozen:${BOX_ID_A}" -smbios "type=11,value=$(seed_cred "$BOX_ID_B")"; then
    echo "cloud gate proof: ${VERDICT}" >&2
    exit 1
fi
echo "boot 3 OK — frozen identity held across reboot (re-delivered seed B ignored, box_id still ${BOX_ID_A})"
fi

# --- 8. access boot: per-app forward-auth access modes end-to-end (#308). Its OWN
# fresh overlay + box-id, seeded with a TEST-PORTAL key so the box can mint a real
# owner session (the positive path the box-only SSO gate above can't reach). The
# harness holds the matching private key and mints a valid owner assertion, delivered
# over a second credential (moose.sso_token); the in-VM cloud-assertions.sh drives
# SSO → installs whoami air-gapped → proves the restricted gate (302 without a
# session, proxied-through WITH the owner's forward-auth cookie), the public toggle
# (reachable with no session), and the Cookie-strip invariant (the app upstream never
# receives the cookie) in both modes. The same app declares access.public_paths, so
# the boot also proves the path-scoped carve-out (#415) through real Caddy: declared
# paths answer anonymously, forged identity headers never reach the app, and the
# path-matcher bypass table stays gated.
if should_run access; then
    [ -n "$GO" ] && [ -x "$GO" ] || {
        echo "access boot needs go to mint the owner assertion; none found (\$GO='${GO:-}')" >&2
        exit 1
    }
    # Three tokens, because each is single-use: one signs the owner in, one drives
    # the hosted confirm round-trip, and one drives the open-redirect probe that the
    # confirm step's return path opens up (os#469).
    mapfile -t access_mint < <(mint_owner_assertion "$BOX_ID_ACCESS" 3) || true
    ACCESS_KEY="${access_mint[0]:-}"
    ACCESS_TOKEN="${access_mint[1]:-}"
    ACCESS_TOKEN2="${access_mint[2]:-}"
    ACCESS_TOKEN3="${access_mint[3]:-}"
    [ -n "$ACCESS_KEY" ] && [ -n "$ACCESS_TOKEN" ] && [ -n "$ACCESS_TOKEN2" ] && [ -n "$ACCESS_TOKEN3" ] || {
        echo "access boot: failed to mint the owner assertions (go run ./dev/cloud/mkassertion)" >&2
        exit 1
    }

    # Fresh overlay: this box provisions with the test-portal key from its first boot,
    # independent of the A/B identity the shared overlay carries. OVERLAY and FIRMWARE
    # are run_boot's globals, and the bios boot above leaves them pointing at ITS
    # overlay under SeaBIOS — so set both explicitly here rather than inheriting them.
    ACCESS_OVERLAY="${RUN_DIR}/overlay-${FIRMWARE}-access.qcow2"
    new_overlay "$ACCESS_OVERLAY"
    OVERLAY="$ACCESS_OVERLAY"

    # Wider outer ceiling: the access scenario's in-guest work (SSO + app install +
    # exposure toggle) adds ~180s of worst-case internal poll time over the shared
    # prechecks, which alone can approach the 480s default under CI's TCG-only QEMU.
    # The two folder-app installs (#519) add up to ~240s more.
    VERDICT_TIMEOUT=960
    if ! run_boot "${FIRMWARE}-access" "access" \
        -smbios "type=11,value=$(seed_cred_keyed "$BOX_ID_ACCESS" "$ACCESS_KEY")" \
        -smbios "type=11,value=io.systemd.credential.binary:moose.sso_token=$(printf '%s' "$ACCESS_TOKEN" | base64 -w0)" \
        -smbios "type=11,value=io.systemd.credential.binary:moose.sso_token2=$(printf '%s' "$ACCESS_TOKEN2" | base64 -w0)" \
        -smbios "type=11,value=io.systemd.credential.binary:moose.sso_token3=$(printf '%s' "$ACCESS_TOKEN3" | base64 -w0)"; then
        echo "cloud gate proof: ${VERDICT}" >&2
        exit 1
    fi
    echo "boot access OK — per-app forward-auth access modes verified end-to-end (restricted gate + owner proxy-through, public, Cookie strip), household and personal folder apps wrote to the host, and the hosted confirm step opened the elevation window, box_id=${BOX_ID_ACCESS}"
fi

# --- 9. update boot: the control-plane updater, for real (#382). Its OWN fresh
# overlay + box-id, seeded with a TEST-PORTAL key like the access boot — the update
# trigger is admin-only, so the box needs a real owner session before it can be asked
# to update itself. Inside the guest, cloud-assertions.sh starts a registry on
# 127.0.0.1:5000, publishes a new brain/UI pair into it by digest, drops both from the
# local image store, and drives POST /api/v1/system/update. It then does it again with
# a brain that starts but never serves, to prove the revert. Everything below that
# endpoint had only ever met a fake Docker.
#
# It is also the boot that proves the seeded update target (os#407): its seed names
# the in-guest source below, and cloud-assertions.sh asserts host-agent resolved the
# target `from=seed`. Nothing else exercises the one channel a real box has.
if should_run update; then
    [ -n "$GO" ] && [ -x "$GO" ] || {
        echo "update boot needs go to mint the owner assertion; none found (\$GO='${GO:-}')" >&2
        exit 1
    }
    mapfile -t update_mint < <(mint_owner_assertion "$BOX_ID_UPDATE") || true
    UPDATE_KEY="${update_mint[0]:-}"
    UPDATE_TOKEN="${update_mint[1]:-}"
    [ -n "$UPDATE_KEY" ] && [ -n "$UPDATE_TOKEN" ] || {
        echo "update boot: failed to mint the owner assertion (go run ./dev/cloud/mkassertion)" >&2
        exit 1
    }

    # The update-target source the seed points this box at. It must match the
    # address cloud-assertions.sh serves target.json on, inside the guest: the
    # seed is delivered here, the file server is started there, and nothing
    # cross-checks the two but a failing boot.
    UPDATE_TARGET_URL=http://127.0.0.1:5001/target.json

    # Same explicit-globals reasoning as the access boot: OVERLAY and FIRMWARE are
    # run_boot's globals and the boots above leave them pointing elsewhere.
    UPDATE_OVERLAY="${RUN_DIR}/overlay-${FIRMWARE}-update.qcow2"
    new_overlay "$UPDATE_OVERLAY"
    OVERLAY="$UPDATE_OVERLAY"

    # The widest ceiling in the lane, and it is not padding: on top of the shared
    # prechecks this boot loads and starts a registry, commits and pushes two images,
    # then runs TWO full update transactions — the second of which spends a 60s health
    # wait failing on purpose before it reverts. Under CI's TCG-only QEMU that adds up.
    VERDICT_TIMEOUT=1500
    if ! run_boot "${FIRMWARE}-update" "update" \
        -smbios "type=11,value=$(seed_cred_keyed "$BOX_ID_UPDATE" "$UPDATE_KEY" "$UPDATE_TARGET_URL")" \
        -smbios "type=11,value=io.systemd.credential.binary:moose.sso_token=$(printf '%s' "$UPDATE_TOKEN" | base64 -w0)"; then
        echo "cloud update proof: ${VERDICT}" >&2
        exit 1
    fi
    echo "boot update OK — control-plane update applied and a failed update reverted, both for real (box_id=${BOX_ID_UPDATE})"
fi

# --- 10. ssh boot: the per-account SSH opt-in against a real sshd (#467). Its OWN
# fresh overlay + box-id, seeded with a TEST-PORTAL key like the access and update
# boots — every SSH write is elevation-class, so the box needs a real owner session
# before it can be asked to turn SSH on. Everything else happens inside the guest:
# cloud-assertions.sh makes a keypair with ssh-keygen and connects the box to
# itself, so nothing crosses the air gap.
#
# This is the only place the daemon lifecycle is observable. #464 built the whole
# path down to the rendered sshd config, but a rendered string cannot tell you that
# :22 opened, that a real sshd took the key and refused the password, or that the
# port closed again — and on hosted that port is controlled by nothing else.
if should_run ssh; then
    [ -n "$GO" ] && [ -x "$GO" ] || {
        echo "ssh boot needs go to mint the owner assertion; none found (\$GO='${GO:-}')" >&2
        exit 1
    }
    mapfile -t ssh_mint < <(mint_owner_assertion "$BOX_ID_SSH") || true
    SSH_KEY_B64="${ssh_mint[0]:-}"
    SSH_TOKEN="${ssh_mint[1]:-}"
    [ -n "$SSH_KEY_B64" ] && [ -n "$SSH_TOKEN" ] || {
        echo "ssh boot: failed to mint the owner assertion (go run ./dev/cloud/mkassertion)" >&2
        exit 1
    }

    # Same explicit-globals reasoning as the access and update boots: OVERLAY and
    # FIRMWARE are run_boot's globals and the boots above leave them pointing
    # elsewhere.
    SSH_OVERLAY="${RUN_DIR}/overlay-${FIRMWARE}-ssh.qcow2"
    new_overlay "$SSH_OVERLAY"
    OVERLAY="$SSH_OVERLAY"

    # Wider than the default for the same reason as the access boot: on top of the
    # shared prechecks this one drives SSO, an elevation, a key add, three sshd
    # reloads, three real ssh connections and a user create + delete. Each waits on
    # a systemctl round-trip through host-agent, and under CI's TCG-only QEMU those
    # add up well past the 480s default.
    VERDICT_TIMEOUT=900
    if ! run_boot "${FIRMWARE}-ssh" "ssh" \
        -smbios "type=11,value=$(seed_cred_keyed "$BOX_ID_SSH" "$SSH_KEY_B64")" \
        -smbios "type=11,value=io.systemd.credential.binary:moose.sso_token=$(printf '%s' "$SSH_TOKEN" | base64 -w0)"; then
        echo "cloud ssh proof: ${VERDICT}" >&2
        exit 1
    fi
    echo "boot ssh OK — :22 closed at boot, opened by the toggle and closed again; key accepted, password alone refused (box_id=${BOX_ID_SSH})"
fi

# --- 11. remap boots: the user-namespace tiers (#531). Their OWN fresh overlay +
# box-id, seeded with a TEST-PORTAL key like the access boot, since installing
# an app needs an owner session. Two boots over that one overlay: `remap`
# installs one app per tier and checks each (userns mode, capabilities, the
# host owner of its data), recreates the caps-tier container and checks its
# data is kept; `remap-reboot` is a real reboot of the same disk that runs the
# same checks on what the box brought back. Each boot gets its own owner
# assertion, because the box spends a jti on first use. Step 5d of
# cloud-assertions.sh checks the daemon side on both boots, as on every boot.
if should_run remap; then
    [ -n "$GO" ] && [ -x "$GO" ] || {
        echo "remap boots need go to mint the owner assertions; none found (\$GO='${GO:-}')" >&2
        exit 1
    }
    mapfile -t remap_mint < <(mint_owner_assertion "$BOX_ID_REMAP" 2) || true
    REMAP_KEY="${remap_mint[0]:-}"
    REMAP_TOKEN="${remap_mint[1]:-}"
    REMAP_TOKEN2="${remap_mint[2]:-}"
    [ -n "$REMAP_KEY" ] && [ -n "$REMAP_TOKEN" ] && [ -n "$REMAP_TOKEN2" ] || {
        echo "remap boots: failed to mint the owner assertions (go run ./dev/cloud/mkassertion)" >&2
        exit 1
    }

    # Same explicit-globals reasoning as the access boot: OVERLAY and FIRMWARE
    # are run_boot's globals and the boots above leave them pointing elsewhere.
    REMAP_OVERLAY="${RUN_DIR}/overlay-${FIRMWARE}-remap.qcow2"
    new_overlay "$REMAP_OVERLAY"
    OVERLAY="$REMAP_OVERLAY"

    # The first boot loads postgres:16 and runs five installs, one of them with
    # a managed Postgres spin-up, then a recreate and a second round of checks.
    # Each install polls up to 300s and each check up to 180s in the worst case,
    # so it gets the update boot's ceiling. The reboot only waits for the apps
    # to come back.
    VERDICT_TIMEOUT=1500
    if ! run_boot "${FIRMWARE}-remap" "remap" \
        -smbios "type=11,value=$(seed_cred_keyed "$BOX_ID_REMAP" "$REMAP_KEY")" \
        -smbios "type=11,value=io.systemd.credential.binary:moose.sso_token=$(printf '%s' "$REMAP_TOKEN" | base64 -w0)"; then
        echo "cloud remap proof: ${VERDICT}" >&2
        exit 1
    fi
    echo "boot remap OK: one app per userns tier installed and checked, the caps-tier data kept across a recreate (box_id=${BOX_ID_REMAP})"
    VERDICT_TIMEOUT=900
    if ! run_boot "${FIRMWARE}-remap-reboot" "remap-reboot" \
        -smbios "type=11,value=$(seed_cred_keyed "$BOX_ID_REMAP" "$REMAP_KEY")" \
        -smbios "type=11,value=io.systemd.credential.binary:moose.sso_token=$(printf '%s' "$REMAP_TOKEN2" | base64 -w0)"; then
        echo "cloud remap reboot proof: ${VERDICT}" >&2
        exit 1
    fi
    echo "boot remap-reboot OK: every tier checked again after a real reboot of the same disk (box_id=${BOX_ID_REMAP})"
fi

# --- 12. the OS update boots (#563). Each takes its own fresh overlay and
# box-id, a test-portal key and one owner assertion (the update-target read is
# admin-only), and a second, read-only disk holding the test bundle: the box is
# air-gapped, so the bundle reaches it as an ext4 image that the guest mounts
# and serves to host-agent from a file server inside the guest. The disk is
# made here, from MOOSE_CLOUD_OS_BUNDLE_DIR, with mke2fs -d (no root needed for
# that part). The guest reboots between slots inside one QEMU run.
#
# MOOSE_CLOUD_OS_REFUSE_ONLY=true runs only the refusal half of os-update: on
# a run that publishes the OS, the boot-proof image trusts only the release
# root, so the throwaway-signed test bundle must be refused (the maintainer's
# call for #563).
os_boot() { # NAME BOX_ID
    local name="$1" box="$2" dir="${MOOSE_CLOUD_OS_BUNDLE_DIR:-}" mint key token disk sum ver mode
    # A local run that built the image here makes the bundle from it too, once.
    if [ -z "$dir" ] && [ -f "$IMAGE_OUT" ]; then
        dir="${WORK}/os-test"
        [ "$dir/os-test.raucb" -nt "$IMAGE_OUT" ] || GO="$GO" "${REPO_ROOT}/dev/cloud/test/build-os-test-bundle.sh" "$IMAGE_OUT" "$dir"
        MOOSE_CLOUD_OS_BUNDLE_DIR="$dir"
    fi
    [ -n "$GO" ] && [ -x "$GO" ] || { echo "$name boot needs go to mint the owner assertion; none found (\$GO='${GO:-}')" >&2; exit 1; }
    [ -n "$dir" ] && [ -f "$dir/os-test.raucb" ] && [ -f "$dir/os-test.sha256" ] && [ -f "$dir/os-test.version" ] || {
        echo "$name boot needs the test bundle: set MOOSE_CLOUD_OS_BUNDLE_DIR to the output of dev/cloud/test/build-os-test-bundle.sh" >&2
        exit 1
    }
    mapfile -t mint < <(mint_owner_assertion "$box") || true
    key="${mint[0]:-}"; token="${mint[1]:-}"
    [ -n "$key" ] && [ -n "$token" ] || { echo "$name boot: failed to mint the owner assertion" >&2; exit 1; }
    disk="${RUN_DIR}/os-bundle-${FIRMWARE}-${name}.img"
    local bytes; bytes="$(stat -c %s "$dir/os-test.raucb")"
    mke2fs -q -t ext4 -L moose-os-test -d "$dir" "$disk" "$(( bytes / 1048576 + 64 ))M"
    sum="$(tr -d '[:space:]' < "$dir/os-test.sha256")"
    ver="$(tr -d '[:space:]' < "$dir/os-test.version")"
    mode=full
    [ "${MOOSE_CLOUD_OS_REFUSE_ONLY:-false}" = true ] && mode=refuse

    OVERLAY="${RUN_DIR}/overlay-${FIRMWARE}-${name}.qcow2"
    new_overlay "$OVERLAY"
    VERDICT_TIMEOUT=1500
    KEEP_REBOOTS=1
    if ! run_boot "${FIRMWARE}-${name}" "$name" \
        -drive "file=${disk},if=virtio,format=raw,readonly=on" \
        -smbios "type=11,value=$(seed_cred_keyed "$box" "$key" "http://127.0.0.1:5001/target.json")" \
        -smbios "type=11,value=io.systemd.credential.binary:moose.sso_token=$(printf '%s' "$token" | base64 -w0)" \
        -smbios "type=11,value=io.systemd.credential:moose.os_test=${mode}:${ver}:${sum}"; then
        KEEP_REBOOTS=""
        echo "cloud ${name} proof: ${VERDICT}" >&2
        exit 1
    fi
    KEEP_REBOOTS=""
}
if should_run os-update; then
    os_boot os-update "$BOX_ID_OS_UPDATE"
    echo "boot os-update OK: the OS update installed into slot B, the box switched, booted slot B and marked it good, with its owner, app, host keys and data intact (box_id=${BOX_ID_OS_UPDATE})"
fi
if should_run os-revert; then
    os_boot os-revert "$BOX_ID_OS_REVERT"
    # The last stage (#575): slot B crashed in its initramfs. The guest checks
    # it came back on slot A; the serial log shows the crash itself.
    grep -aq 'moose-test: slot B panics before userspace' "$QEMU_SERIAL" && grep -aq 'Kernel panic' "$QEMU_SERIAL" \
        || { echo "cloud os-revert proof: no kernel panic of slot B on the serial console" >&2; exit 1; }
    echo "boot os-revert OK: slot B never came up, the safety net rebooted the box and it went back to slot A on its own; then slot B panicked before userspace and GRUB skipped it (box_id=${BOX_ID_OS_REVERT})"
fi

echo "firmware ${FIRMWARE}: every boot OK (${BOOTS})"
done

echo "cloud end-to-end: PASS (boots: ${BOOTS}; firmwares: ${FIRMWARES})"
echo "qcow2 cloud artifact: ${QCOW2}"
exit 0
