#!/usr/bin/env bash
# Boot-chain assertions, run *inside* systemd-nspawn --boot.
#
# Driven by moose-boot-test.service (staged by run-boot-chain-tests.sh)
# after multi-user.target is reached. Writes PASS or FAIL: <detail> to
# /var/lib/moose-boot-result, which is bind-mounted from the host so the
# driver can read the verdict after `systemctl poweroff` exits the
# container.
#
# See docs/specs/TESTING.md # Fast lane for what we're trying to catch:
# unit dependency errors, drop-in overrides, synthetic-target shape.
# -u + pipefail but deliberately NOT -e: every assertion is `... || fail
# "..."`, and a hard -e would skip the descriptive failure message and
# exit silently. The EXIT trap below upgrades the STARTED sentinel to a
# generic FAIL if anything falls through without ok/fail.
set -uo pipefail

RESULT=/var/lib/moose-boot-result
# Sentinel for "service entered ExecStart but didn't reach ok/fail" —
# the trap below upgrades this to a proper FAIL: line on abort.
# The host driver polls for ^(PASS|FAIL:) and skips this sentinel so it
# doesn't tear us down before assertions complete.
echo "STARTED" > "$RESULT"

# Always shut down the container at the end, regardless of pass/fail.
# Signal PID 1 directly with SIGRTMIN+4 ("start poweroff.target") — this
# bypasses DBus and the shutdown.target negotiation path which stalls in
# our minimal rootfs (the bind-ro on /etc/systemd/system masks shutdown
# wants that bookworm normally ships). systemctl --no-block poweroff
# does NOT reliably trigger shutdown in this configuration; the direct
# signal does.
poweroff_and_exit() {
    sync
    /bin/kill -s SIGRTMIN+4 1
    # Belt-and-suspenders: also try systemctl in case kill fails.
    /bin/systemctl --no-block poweroff 2>/dev/null || true
    exit 0
}
fail() { echo "FAIL: $*" > "$RESULT"; poweroff_and_exit; }
ok()   { echo "PASS"     > "$RESULT"; poweroff_and_exit; }

# Belt-and-suspenders cleanup. Runs on every exit path:
#   - happy path: ok/fail wrote PASS/FAIL and called exit 0 → the
#     grep guard skips the upgrade, the second SIGRTMIN+4 to PID 1
#     is harmless (PID 1 already starting shutdown).
#   - abort path: set -u / unhandled fault → upgrade the STARTED
#     sentinel to a proper FAIL: line and still poweroff.
trap '
    # If we never reached fail/ok the result is still "STARTED" — upgrade
    # to a proper FAIL: so the host driver sees a real verdict.
    if ! grep -qE "^(PASS|FAIL:)" "$RESULT" 2>/dev/null; then
        echo "FAIL: assertion script aborted before reaching ok/fail" > "$RESULT"
    fi
    sync
    /bin/kill -s SIGRTMIN+4 1 2>/dev/null || /bin/systemctl --no-block poweroff
' EXIT

# --- 1. units parse + analyze verify
# Pass the stub docker/smbd/avahi-daemon units too — systemd-analyze
# loads their .d/ drop-ins automatically when the parent unit is named,
# so this catches malformed dist/systemd/dropins/<svc>.service.d/
# regressions that would otherwise only surface at boot.
verify_out="$(systemd-analyze verify \
    /etc/systemd/system/moose-storage-ready.target \
    /etc/systemd/system/moose-storage-verify.service \
    /etc/systemd/system/moose-recovery.target \
    /etc/systemd/system/host-agent.service \
    /etc/systemd/system/docker.service \
    /etc/systemd/system/smbd.service \
    /etc/systemd/system/avahi-daemon.service \
    2>&1)" \
    || fail "systemd-analyze verify rejected one or more units: ${verify_out}"

# --- 2. drop-ins applied to their parent units
# `systemctl cat <unit>` prints both the base unit and any drop-ins.
# We seeded stub parent units in the staging tree so the drop-ins have
# something to attach to — systemd-245+ does not surface orphan drop-ins.
for svc in docker smbd avahi-daemon; do
    out="$(systemctl cat "${svc}.service" 2>&1)" \
        || fail "systemctl cat ${svc}.service failed: $out"
    grep -q 'moose-storage-ready.target' <<<"$out" \
        || fail "drop-in for ${svc}.service does not reference moose-storage-ready.target"
done

# --- 3. synthetic target pulls in the verifier
deps="$(systemctl list-dependencies moose-storage-ready.target 2>&1)"
grep -q 'moose-storage-verify.service' <<<"$deps" \
    || fail "moose-storage-ready.target does not list moose-storage-verify.service in its deps tree"

# --- 4. verifier is ordered Before= the ready target
before="$(systemctl show moose-storage-verify.service -p Before --value 2>&1)"
grep -q 'moose-storage-ready.target' <<<"$before" \
    || fail "moose-storage-verify.service has Before='$before' (expected moose-storage-ready.target)"

# --- 5. host-agent ordering + OnFailure routing
after="$(systemctl show host-agent.service -p After --value 2>&1)"
grep -q 'moose-storage-ready.target' <<<"$after" \
    || fail "host-agent.service After='$after' missing moose-storage-ready.target"
grep -q 'docker.service' <<<"$after" \
    || fail "host-agent.service After='$after' missing docker.service"

onfail="$(systemctl show host-agent.service -p OnFailure --value 2>&1)"
grep -q 'moose-recovery.target' <<<"$onfail" \
    || fail "host-agent.service OnFailure='$onfail' (expected moose-recovery.target)"

# StartLimitBurst lives in [Unit]; systemd surfaces it under the unit name.
slburst="$(systemctl show host-agent.service -p StartLimitBurst --value 2>&1)"
[ "$slburst" = "5" ] \
    || fail "host-agent.service StartLimitBurst='$slburst' (expected 5)"

# --- 6. reporter actually runs end-to-end
# Start the verifier directly (not the target) so a transient docker.service
# / smbd.service stub failure can't mask the reporter result.
systemctl start moose-storage-verify.service \
    || fail "systemctl start moose-storage-verify.service failed: $(systemctl status --no-pager moose-storage-verify.service 2>&1 | tail -20)"

# Reporter exits 0 unconditionally per BOOT.md; check the artifact.
test -s /run/moose/health/storage.json \
    || fail "/run/moose/health/storage.json missing or empty after verifier ran"

# Payload shape: top-level object with checked_at + findings.
# Minimal bookworm rootfs has no python/jq, so we grep for the keys
# and the absence of findings entries on a Level-0 boot. The reporter
# emits pretty-printed JSON, so collapse whitespace before matching.
payload="$(cat /run/moose/health/storage.json)"
compact="$(tr -d ' \n\t' <<<"$payload")"
grep -q '"checked_at"' <<<"$compact" \
    || fail "storage.json missing checked_at: $payload"
grep -q '"findings"' <<<"$compact" \
    || fail "storage.json missing findings: $payload"

# Level-0 boot (no /etc/moose/data-drive.enrolled in the rootfs): expect
# either `"findings":null` (Go nil slice) or `"findings":[]` (empty
# slice). Anything containing `"id":` would be a spurious finding.
case "$compact" in
    *'"findings":null'*|*'"findings":[]'*) ;;
    *) fail "expected empty findings on Level-0 boot, got: $payload" ;;
esac
grep -q '"id":' <<<"$compact" \
    && fail "spurious finding emitted on clean Level-0 rootfs: $payload"

ok
