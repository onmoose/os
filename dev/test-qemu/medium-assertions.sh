#!/usr/bin/env bash
# Medium-lane in-VM assertions, baked into the image at
# /usr/local/bin/medium-assertions.sh by dev/test-qemu/bootstrap.sh.
#
# Driven over SSH by dev/test-qemu/run-medium-tests.sh after the VM
# boots. Writes the verdict to /var/lib/moose-medium-result; the host
# driver scp's it back.
#
# Takes a phase argument (slice 0023 Stages 2/3); the harness runs the
# image through two boots of the same disk and asserts a different thing
# on each:
#   first-boot   common checks + the run-once systemd-cryptenroll landed
#                a PCR-7-bound systemd-tpm2 token in the LUKS header.
#   second-boot  common checks + the token persisted, AND this boot got
#                here at all — the harness withheld the passphrase
#                credential, so reaching multi-user proves the initrd
#                unsealed root unattended via that TPM2 token. Then the
#                network-state slice (#130): NM LAN set, avahi
#                allow-interfaces sync, per-interface announcement,
#                interface-removal rewrite, IP-change replay.
#   combined     (default) common checks only — for `mkosi qemu` manual
#                debugging where no harness phase is supplied.
#
# -u + pipefail but deliberately NOT -e: every assertion is `... || fail
# "..."`. The EXIT trap upgrades the STARTED sentinel to a generic FAIL
# if anything falls through without ok/fail. Same posture as slice 0020.
set -uo pipefail

PHASE="${1:-combined}"
RESULT=/var/lib/moose-medium-result

# Sentinel non-matching the host's ^(PASS|FAIL:) regex so the driver
# doesn't tear us down before assertions complete.
echo "STARTED" > "$RESULT"

fail() { echo "FAIL: $*" > "$RESULT"; sync; exit 1; }
ok()   { echo "PASS"     > "$RESULT"; sync; exit 0; }

trap '
    if ! grep -qE "^(PASS|FAIL:)" "$RESULT" 2>/dev/null; then
        echo "FAIL: assertion script aborted before reaching ok/fail" > "$RESULT"
    fi
    sync
' EXIT

echo "medium-assertions phase=$PHASE"

# --- 1. real systemd userspace reached multi-user.
# Poll, don't sample once: on first boot the run-once
# moose-tpm-enroll.service is part of the boot transaction
# (WantedBy=moose-storage-ready.target) and its Argon2 keyslot operation
# takes a couple of seconds, during which is-system-running legitimately
# reports 'starting'. sshd has no ordering against the enroll, so it can
# win the race and let us in before the transaction settles — sampling
# once here would spuriously fail (and the driver's follow-up poweroff
# would then SIGTERM the enroll mid-operation). Wait for the transaction
# to converge; a failed enroll still converges (Wants=, not Requires=) to
# 'degraded', which the marker-wait below then diagnoses precisely.
state=""
for _i in $(seq 1 120); do
    state="$(systemctl is-system-running 2>&1 || true)"
    case "$state" in
        running|degraded) break ;;
    esac
    sleep 1
done
case "$state" in
    running|degraded) ;;
    *) fail "system state is '$state' after 120s (expected running or degraded)" ;;
esac

# --- 2. storage-verify ran end-to-end
verify_state="$(systemctl is-active moose-storage-verify.service 2>&1 || true)"
[ "$verify_state" = "active" ] \
    || fail "moose-storage-verify.service is '$verify_state' (expected active)"

# --- 3. reporter output exists and is shaped correctly
test -s /run/moose/health/storage.json \
    || fail "/run/moose/health/storage.json missing or empty"

payload="$(cat /run/moose/health/storage.json)"
compact="$(tr -d ' \n\t' <<<"$payload")"
grep -q '"checked_at"' <<<"$compact" \
    || fail "storage.json missing checked_at: $payload"
grep -q '"findings"' <<<"$compact" \
    || fail "storage.json missing findings: $payload"

# Level-0 VM has no data drive enrolled — expect empty findings.
case "$compact" in
    *'"findings":null'*|*'"findings":[]'*) ;;
    *) fail "expected empty findings on Level-0 VM, got: $payload" ;;
esac
grep -q '"id":' <<<"$compact" \
    && fail "spurious finding on clean Level-0 VM: $payload"

# --- 4. TPM plumbing is live
# swtpm via QEMU exposes /dev/tpm0 and /dev/tpmrm0. tpm2_pcrread is the
# cheapest test that the device is wired and responding. PCR 7 (Secure
# Boot policy state per STORAGE.md # Encryption posture) is the bank our
# keyslot policy binds to.
test -c /dev/tpmrm0 \
    || fail "/dev/tpmrm0 not present (TPM device not exposed to VM?)"

pcr_out="$(tpm2_pcrread sha256:7 2>&1)" \
    || fail "tpm2_pcrread sha256:7 failed: $pcr_out"
grep -q '7 *:' <<<"$pcr_out" \
    || fail "tpm2_pcrread output unexpected shape: $pcr_out"

# --- 5. root actually sits on a LUKS/dm-crypt device (slice 0023 Stage 1)
# The whole point of 0023: prove we booted the *encrypted* root, not a
# silent plaintext fallback. Reaching here means systemd-cryptsetup
# unlocked the LUKS container in the initrd (via the passphrase credential
# on first boot, via the TPM2 token on second boot) and switch-root landed
# on the mapper device. Assert the mount source is the dm-crypt device.
root_src="$(findmnt -no SOURCE / 2>/dev/null || true)"
case "$root_src" in
    /dev/mapper/luks-*|/dev/dm-*) ;;
    *) fail "root is not on a dm-crypt device (source='$root_src'); encrypted-root path not exercised" ;;
esac
# Belt-and-suspenders: confirm it's a live LUKS mapping, not a same-named
# mapper device of some other dm target.
cryptsetup status "$root_src" 2>/dev/null | grep -qiE 'type:[[:space:]]*LUKS' \
    || fail "root device '$root_src' is not a live LUKS mapping"

# Resolve the LUKS backing partition (where the header + tokens live).
# Needed for the token assertions below (the header is on the partition,
# not the mapper). awk on the `device:` line of cryptsetup status.
luks_backing() {
    cryptsetup status "$root_src" 2>/dev/null \
        | awk '/^[[:space:]]*device:/ {print $2}'
}

# Assert the LUKS header carries a systemd-tpm2 token bound to PCR 7.
# Parsed from the JSON metadata with grep/sed only — the guest image has
# no jq or python3. Compact the JSON, find a systemd-tpm2 token, isolate
# its tpm2-pcrs array (hyphenated key in the on-disk token JSON), and
# check 7 is a member (comma-wrap so "7" can't match inside "17").
assert_tpm2_pcr7_token() {
    local backing json jcompact arr
    backing="$(luks_backing)"
    [ -n "$backing" ] || fail "could not resolve LUKS backing device for $root_src"

    json="$(cryptsetup luksDump --dump-json-metadata "$backing" 2>/dev/null || true)"
    [ -n "$json" ] || fail "cryptsetup luksDump --dump-json-metadata $backing produced no output"
    jcompact="$(tr -d ' \n\t' <<<"$json")"

    grep -q '"type":"systemd-tpm2"' <<<"$jcompact" \
        || fail "no systemd-tpm2 token in LUKS header of $backing (json: $jcompact)"

    arr="$(grep -oE '"tpm2-pcrs":\[[0-9,]*\]' <<<"$jcompact" | head -1 \
        | sed -E 's/.*\[//; s/\].*//')"
    grep -q ',7,' <<<",${arr}," \
        || fail "systemd-tpm2 token not bound to PCR 7 (tpm2-pcrs=[$arr]) on $backing"
}

# --- network-state slice (#130) -----------------------------------------
# Second boot only (the steady-state boot, after the LUKS/TPM checks).
# Drives /usr/lib/moose/moose-network-verify — the same netstate +
# avahipublisher packages cmd/host-agent-real wires, minus PAM — against
# the VM's real NetworkManager and avahi-daemon. The SSH NIC (MAC pinned
# in run-medium-tests.sh, NM-unmanaged) is never touched.
SSH_NIC_MAC="52:54:00:6d:6c:01"
MNV=/usr/lib/moose/moose-network-verify
AVAHI_CONF=/etc/avahi/avahi-daemon.conf
MNV_LOG=/var/log/moose-network-verify.log

nic_ipv4() {
    ip -o -4 addr show dev "$1" 2>/dev/null | awk '{print $4}' | cut -d/ -f1 | head -1
}

assert_network_state() {
    command -v nmcli >/dev/null || fail "nmcli not in image"
    command -v avahi-resolve >/dev/null || fail "avahi-resolve not in image"
    [ -x "$MNV" ] || fail "$MNV missing from image"

    # Both NM NICs must reach 'connected'. DHCP on the isolated usernets
    # is quick but has no ordering against sshd, so poll.
    local nics=""
    for _i in $(seq 1 60); do
        nics="$(nmcli -t -f DEVICE,TYPE,STATE device 2>/dev/null \
            | awk -F: '$2 == "ethernet" && $3 == "connected" {print $1}' | sort)"
        [ "$(grep -c . <<<"$nics")" -eq 2 ] && break
        sleep 1
    done
    [ "$(grep -c . <<<"$nics")" -eq 2 ] \
        || fail "want 2 NM-connected ethernet NICs, have: $(nmcli device 2>&1)"
    local nic_a nic_b
    nic_a="$(sed -n 1p <<<"$nics")"
    nic_b="$(sed -n 2p <<<"$nics")"

    # The SSH NIC must be NM-unmanaged — its absence below proves the LAN
    # set is "active NM connections", not "all kernel interfaces".
    local ssh_nic
    ssh_nic="$(ip -o link | awk -v mac="$SSH_NIC_MAC" \
        'tolower($0) ~ mac {sub(/:$/, "", $2); print $2; exit}')"
    [ -n "$ssh_nic" ] || fail "no interface with SSH NIC MAC $SSH_NIC_MAC"
    grep -qx "$ssh_nic" <<<"$nics" \
        && fail "SSH NIC $ssh_nic leaked into the NM-connected set"

    # 1. netstate's LAN set == the NM set, SSH NIC excluded.
    local lan lan_names
    lan="$("$MNV" lan 2>&1)" || fail "moose-network-verify lan: $lan"
    lan_names="$(grep -oE '"Name":"[^"]*"' <<<"$lan" | cut -d'"' -f4 | sort)"
    [ "$lan_names" = "$nics" ] \
        || fail "netstate LAN set [$lan_names] != NM set [$nics] (raw: $lan)"

    # 2. serve: the conf ships with no allow-interfaces, so the startup
    # sync exercises conf-change -> daemon restart -> republish; the
    # published name must then resolve to one of the LAN addresses.
    "$MNV" serve -slug moosetest >"$MNV_LOG" 2>&1 &
    local mnv_pid=$!
    local want_allow="allow-interfaces=${nic_a},${nic_b}"
    local got=""
    for _i in $(seq 1 30); do
        grep -qx "$want_allow" "$AVAHI_CONF" 2>/dev/null && { got=1; break; }
        kill -0 "$mnv_pid" 2>/dev/null || break
        sleep 1
    done
    [ -n "$got" ] || fail "allowlist never became '$want_allow': conf=$(grep allow-interfaces "$AVAHI_CONF" 2>/dev/null) log=$(tail -5 "$MNV_LOG" 2>/dev/null)"

    local addr_a addr_b resolved=""
    addr_a="$(nic_ipv4 "$nic_a")"
    addr_b="$(nic_ipv4 "$nic_b")"
    for _i in $(seq 1 30); do
        resolved="$(avahi-resolve -4 -n moosetest.local 2>/dev/null | awk '{print $2}')"
        [ -n "$resolved" ] && break
        sleep 1
    done
    [ -n "$resolved" ] || fail "moosetest.local never resolved: $(tail -5 "$MNV_LOG" 2>/dev/null)"
    case "$resolved" in
        "$addr_a"|"$addr_b") ;;
        *) fail "moosetest.local resolved to $resolved, want $addr_a or $addr_b" ;;
    esac

    # 3. interface removal: disconnecting nic_b must rewrite the allowlist
    # to nic_a alone (second conf-change -> restart -> republish round).
    nmcli device disconnect "$nic_b" >/dev/null 2>&1 \
        || fail "nmcli device disconnect $nic_b failed"
    got=""
    for _i in $(seq 1 30); do
        grep -qx "allow-interfaces=${nic_a}" "$AVAHI_CONF" 2>/dev/null && { got=1; break; }
        sleep 1
    done
    [ -n "$got" ] || fail "allowlist not rewritten after $nic_b disconnect: conf=$(grep allow-interfaces "$AVAHI_CONF" 2>/dev/null) log=$(tail -5 "$MNV_LOG" 2>/dev/null)"

    # 4. IP-change replay: committed entry groups hold the literal old
    # address, so only watcher -> RepublishAll makes the new one
    # resolvable. Conf is unchanged by an IP-only change (no restart).
    local conn
    conn="$(nmcli -t -f GENERAL.CONNECTION device show "$nic_a" 2>/dev/null | cut -d: -f2-)"
    [ -n "$conn" ] || fail "no NM connection on $nic_a"
    nmcli connection modify "$conn" ipv4.method manual \
        ipv4.addresses 10.0.9.99/24 ipv4.gateway "" ipv4.dns "" \
        || fail "nmcli connection modify '$conn' failed"
    nmcli connection up "$conn" >/dev/null 2>&1 \
        || fail "nmcli connection up '$conn' failed"
    resolved=""
    for _i in $(seq 1 30); do
        resolved="$(avahi-resolve -4 -n moosetest.local 2>/dev/null | awk '{print $2}')"
        [ "$resolved" = "10.0.9.99" ] && break
        sleep 1
    done
    [ "$resolved" = "10.0.9.99" ] \
        || fail "replay never re-announced 10.0.9.99 (last resolve: '$resolved'): $(tail -10 "$MNV_LOG" 2>/dev/null)"

    kill -0 "$mnv_pid" 2>/dev/null \
        || fail "moose-network-verify serve died mid-test: $(tail -10 "$MNV_LOG" 2>/dev/null)"
    kill "$mnv_pid" 2>/dev/null
}

# --- M2 full-stack app install (#167) -----------------------------------
# Installs the catalog app whoami end-to-end with NO guest internet (the netdevs
# run restrict=on — run-medium-tests.sh), proving: the offline image bundle is
# complete (a pull would hard-fail; the brain trusts the catalog-promised digest
# of the docker-loaded image — MOOSE_OFFLINE_INSTALL), the container runs,
# whoami.local resolves, the app's route returns its page through Caddy, a real
# use-case-folder bind mount lands, and content under it survives uninstall
# (STORAGE.md # Files are first-class). The socket-proxy boundary is asserted in
# the M1b block above. Driven over Caddy :80, same /dev/tcp idiom as M1b/M1c.
#
# scope=personal, not household: a level-0 VM has no data drive, so the shared
# tree (/srv/moose/shared) household would force doesn't exist; personal binds
# the admin's own ~/Documents, which the brain creates at install.
assert_app_install() {
    # Locals (not top-level): ADMIN_DOCS interpolates $SETUP_USER, which the M1c
    # block sets — and under `set -u` an unbound reference aborts the whole
    # script. This function only runs in second-boot, after that block, so the
    # var is bound here; defining these at top level fires before it is set.
    local APP_SLUG=whoami
    local ADMIN_DOCS="/home/${SETUP_USER}/Documents"
    local MARKER="${ADMIN_DOCS}/survives-uninstall.txt"

    # 0. authenticate. M1c proved /login returns 200; here we keep the session
    # cookie (install authorizes on the session — no elevation needed for
    # install). Set-Cookie: moose_session=<tok>; …  → "moose_session=<tok>".
    local login_resp cookie
    login_resp="$(http_post /api/v1/login moose.local "$setup_body" 2>/dev/null || true)"
    cookie="$(grep -i '^Set-Cookie:' <<<"$login_resp" \
        | sed -E 's/^[Ss]et-[Cc]ookie:[[:space:]]*([^;]*).*/\1/' | tr -d '\r' | head -1)"
    [ -n "$cookie" ] \
        || fail "no session cookie from /login: $(head -1 <<<"$login_resp" | tr -d '\r')"

    # authenticated request helpers carrying the session cookie.
    app_post() { # PATH JSON -> full response
        local body="$2" len
        # Byte length, not ${#body} (shell char count): a multi-byte char would
        # mis-size Content-Length and the server would truncate/reject the body.
        len="$(printf '%s' "$body" | wc -c | tr -d ' ')"
        exec 3<>/dev/tcp/127.0.0.1/80 || return 1
        printf 'POST %s HTTP/1.0\r\nHost: moose.local\r\nCookie: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \
            "$1" "$cookie" "$len" "$body" >&3
        cat <&3
        exec 3>&- 3<&-
    }
    app_delete() { # PATH -> status line
        exec 3<>/dev/tcp/127.0.0.1/80 || return 1
        printf 'DELETE %s HTTP/1.0\r\nHost: moose.local\r\nCookie: %s\r\nConnection: close\r\n\r\n' "$1" "$cookie" >&3
        head -1 <&3 | tr -d '\r'
        exec 3>&- 3<&-
    }
    # GET an arbitrary Host through Caddy (the app route) -> full response.
    host_get() { # PATH HOST -> full response
        exec 3<>/dev/tcp/127.0.0.1/80 || return 1
        printf 'GET %s HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n' "$1" "$2" >&3
        cat <&3
        exec 3>&- 3<&-
    }

    # 1. install. 202 Accepted starts the install job; the brain pulls (fails,
    # air-gapped) then trusts the catalog digest of the loaded image, brings the
    # container up, publishes whoami.local, flips the route to the app.
    local resp status
    resp="$(app_post /api/v1/apps '{"manifest_id":"whoami","scope":"personal"}' 2>/dev/null || true)"
    status="$(head -1 <<<"$resp" | tr -d '\r')"
    case "$status" in
        *" 202"*|*" 200"*) ;;
        *) fail "install whoami did not start: status='$status' body=$(sed -n '/^\r\{0,1\}$/,$p' <<<"$resp" | tr -d '\r' | tail -2 | tr '\n' ' ')" ;;
    esac

    # On any install failure below, dump diagnostics to stdout (the harness
    # surfaces the assertion script's stdout in the make output): the brain's
    # install/rollback log carries the actual cause, and docker ps -a shows
    # whether the app container was created or exited.
    install_diag() {
        echo "--- M2 install diagnostics ---"
        echo "install response (first lines):"; head -3 <<<"$resp" | tr -d '\r'
        echo "docker ps -a:"; docker ps -a --format '{{.Names}}\t{{.Status}}\t{{.Image}}' 2>&1
        echo "GET /apps:"; http_get_auth /api/v1/apps 2>/dev/null | sed -n '/^\r\{0,1\}$/,$p' | tr -d '\r' | tail -3
        echo "brain log (tail 40):"; docker logs moose-brain 2>&1 | tail -40
        echo "--- end diagnostics ---"
    }
    # http_get_auth: authenticated GET (used by install_diag + below).
    http_get_auth() {
        exec 3<>/dev/tcp/127.0.0.1/80 || return 1
        printf 'GET %s HTTP/1.0\r\nHost: moose.local\r\nCookie: %s\r\nConnection: close\r\n\r\n' "$1" "$cookie" >&3
        cat <&3
        exec 3>&- 3<&-
    }

    # 2. wait for the app route to serve through Caddy. The brain flips the route
    # from splash to the app upstream only after main_service is healthy (whoami
    # has no healthcheck → "none" counts as healthy), so a 200 here means the
    # whole install transaction converged: image trusted offline, compose up,
    # route flipped. whoami echoes the request — assert its body, not just 200,
    # so we know we reached whoami and not a stale splash/catch-all.
    local app_resp app_status=""
    for _i in $(seq 1 120); do
        app_resp="$(host_get / "${APP_SLUG}.local" 2>/dev/null || true)"
        app_status="$(head -1 <<<"$app_resp" | tr -d '\r')"
        grep -q ' 200' <<<"$app_status" && grep -qi 'Hostname:' <<<"$app_resp" && break
        sleep 1
    done
    grep -q ' 200' <<<"$app_status" \
        || { install_diag; fail "whoami route not serving through Caddy after 120s: status='$app_status'"; }
    grep -qi 'Hostname:' <<<"$app_resp" \
        || fail "whoami.local route did not return the whoami echo page: $(head -1 <<<"$app_status")"

    # 3. whoami.local resolves in-guest via avahi (the per-app mDNS record the
    # brain published, on the NM LAN interfaces). Use avahi-resolve-host-name
    # (avahi-utils) — no libnss-mdns / getent dependency. Poll: publish races the
    # route flip slightly.
    local resolved=""
    for _i in $(seq 1 30); do
        resolved="$(avahi-resolve-host-name -4 "${APP_SLUG}.local" 2>/dev/null | awk '{print $2}')"
        [ -n "$resolved" ] && break
        sleep 1
    done
    [ -n "$resolved" ] \
        || fail "${APP_SLUG}.local did not resolve via avahi-resolve-host-name"

    # 4. a real use-case-folder bind mount landed. whoami is a FROM-scratch image
    # (no shell → no docker exec), so assert host-side: the running container's
    # Mounts carry /moose/documents bound from the admin's personal ~/Documents
    # (scope=personal). Resolve the container by moose's instance-id label.
    local cname inst_id mounts
    cname="$(docker ps --format '{{.Names}}' | grep -E "^moose-.*-${APP_SLUG}\$" | head -1)"
    [ -n "$cname" ] || fail "no running whoami container (docker ps: $(docker ps --format '{{.Names}}' | tr '\n' ' '))"
    inst_id="$(docker inspect "$cname" --format '{{ index .Config.Labels "moose.instance_id" }}' 2>/dev/null)"
    [ -n "$inst_id" ] || fail "whoami container $cname has no moose.instance_id label"
    mounts="$(docker inspect "$cname" --format '{{range .Mounts}}{{.Destination}}={{.Source}}{{"\n"}}{{end}}' 2>/dev/null)"
    grep -qx "/moose/documents=${ADMIN_DOCS}" <<<"$mounts" \
        || fail "documents bind mount missing/wrong (want /moose/documents=${ADMIN_DOCS}); mounts: $(tr '\n' ' ' <<<"$mounts")"

    # 5. content survives uninstall. Write a marker into the bound host folder
    # (the brain created + chowned it at install), uninstall the app, and assert
    # the file outlives it — uninstalling never deletes user content.
    echo "moose-m2-survives" > "$MARKER" || fail "could not write marker $MARKER"
    local del_status
    del_status="$(app_delete "/api/v1/apps/${inst_id}" 2>/dev/null || true)"
    case "$del_status" in
        *" 202"*|*" 200"*) ;;
        *) fail "uninstall whoami did not start: status='$del_status'" ;;
    esac
    # Wait for the container to be gone (uninstall = compose down -v + teardown).
    local gone=""
    for _i in $(seq 1 60); do
        docker ps --format '{{.Names}}' | grep -qE "^moose-.*-${APP_SLUG}\$" || { gone=1; break; }
        sleep 1
    done
    [ -n "$gone" ] || fail "whoami container still running 60s after uninstall"
    test -f "$MARKER" \
        || fail "user content $MARKER did NOT survive uninstall (files-are-first-class violated)"

    echo "control-plane M2: whoami installed air-gapped, ${APP_SLUG}.local resolved, route + bind verified, content survived uninstall"
}

# --- TEMPORARY (M0, #163): control-plane images baked + loaded.
# This is NOT a permanent medium-lane assertion — the automated control-plane
# checks belong to the full-stack lane (M2). It is here to verify the M0
# "Done when": docker.service is active and the four bundled images are present
# after first boot. Remove (or migrate to the full-stack lane) once that lands.
docker_state="$(systemctl is-active docker.service 2>&1 || true)"
[ "$docker_state" = "active" ] \
    || fail "docker.service is '$docker_state' (expected active)"
# moose-load-images.service runs once at first boot (WantedBy=multi-user.target);
# SSH can beat its docker-load, so poll for the success marker before listing.
for _i in $(seq 1 60); do
    [ -f /var/lib/moose/.control-plane-images-loaded ] && break
    if systemctl is-failed --quiet moose-load-images.service; then
        fail "moose-load-images.service failed: $(journalctl -u moose-load-images.service -b --no-pager 2>/dev/null | tail -20)"
    fi
    sleep 1
done
[ -f /var/lib/moose/.control-plane-images-loaded ] \
    || fail "control-plane image-load marker never appeared after 60s"
cp_images="$(docker images --format '{{.Repository}}' 2>&1 || true)"
for repo in moose-brain moose-ui caddy tecnativa/docker-socket-proxy; do
    grep -qx "$repo" <<<"$cp_images" \
        || fail "control-plane image '$repo' not loaded (have: $(tr '\n' ' ' <<<"$cp_images"))"
done
echo "control-plane: docker.service active, 4 bundled images loaded"

# --- M1b (#165): the brain brings up the control-plane stack, reaching Docker
# only through the host-agent-seeded socket-proxy. Proves the M1b "Done when":
# Caddy + moose-ui + socket-proxy launched, dashboard SPA loads through Caddy,
# the raw socket is not mounted into the brain. The thorough app-install
# assertions are M2 (#167); the headless /setup round-trip is M1c (#166).

# 1. host-agent seeds the proxy; the brain reconciles caddy + ui. The brain
# bootstrap + compose up run after Docker is ready and race sshd, so poll.
want_containers="moose-brain moose-caddy moose-ui moose-docker-proxy"
running=""
for _i in $(seq 1 120); do
    running="$(docker ps --format '{{.Names}}' 2>/dev/null | tr '\n' ' ')"
    ok_all=1
    for c in $want_containers; do
        grep -qw "$c" <<<"$running" || ok_all=0
    done
    [ "$ok_all" = 1 ] && break
    sleep 1
done
for c in $want_containers; do
    grep -qw "$c" <<<"$running" \
        || fail "control-plane container '$c' not running after 120s (have: $running)"
done

# 2. proxy boundary: the brain must NOT have the raw Docker socket mounted — it
# reaches Docker only via the proxy (CONTROL_PLANE.md # Docker socket exposure).
brain_sock="$(docker inspect moose-brain \
    --format '{{range .Mounts}}{{println .Source}}{{end}}' 2>/dev/null \
    | grep -c 'docker.sock' || true)"
[ "$brain_sock" = 0 ] \
    || fail "raw docker.sock is mounted into moose-brain (proxy boundary breached)"
brain_dockerhost="$(docker inspect moose-brain \
    --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
    | sed -n 's/^DOCKER_HOST=//p')"
[ "$brain_dockerhost" = "tcp://docker-proxy:2375" ] \
    || fail "brain DOCKER_HOST='$brain_dockerhost', want tcp://docker-proxy:2375"

# 3. dashboard loads through Caddy. Caddy publishes :80 on the host; the dashboard
# host route serves the SPA from moose-ui and proxies /api to the brain. No curl
# in the image — use bash /dev/tcp. Poll: the brain configures the route a beat
# after Caddy comes up.
http_status() { # $1 path, $2 host -> prints the HTTP status line
    exec 3<>/dev/tcp/127.0.0.1/80 || return 1
    printf 'GET %s HTTP/1.0\r\nHost: %s\r\nConnection: close\r\n\r\n' "$1" "$2" >&3
    head -1 <&3
    exec 3>&- 3<&-
}
spa_status=""
for _i in $(seq 1 60); do
    spa_status="$(http_status / moose.local 2>/dev/null || true)"
    grep -q ' 200' <<<"$spa_status" && break
    sleep 1
done
grep -q ' 200' <<<"$spa_status" \
    || fail "dashboard SPA not reachable through Caddy: status='$spa_status'"

# 4. the /api leg routes to the brain, not the catch-all 404. /api/v1/me is a
# real brain route (200 with the setup flag, or 401) — either proves Caddy
# proxied /api to the brain. A 404 would mean the catch-all swallowed it; a 502
# means the route is installed but the brain's HTTP listener isn't up yet.
# Poll: the brain installs the dashboard route (which is why /api isn't a 404)
# before it finishes its synchronous startup host-calls and reaches
# ListenAndServe, so Caddy can briefly 502 the brain leg while the SPA leg
# (a separate container) already serves 200. Same poll shape as the SPA above.
api_status=""
for _i in $(seq 1 60); do
    api_status="$(http_status /api/v1/me moose.local 2>/dev/null || true)"
    grep -qE ' (200|401)' <<<"$api_status" && break
    sleep 1
done
grep -qE ' (200|401)' <<<"$api_status" \
    || fail "/api not routed to the brain through Caddy: status='$api_status'"

echo "control-plane M1b: stack up, proxy boundary held, dashboard + /api reachable"

# --- managed-DB provisioning transport (#185): the brain provisions per-app
# databases/roles by running the engine's client in a one-shot `docker run --rm`
# container (output capture = a container *attach*), NOT a `docker exec` into the
# long-running service container — because the socket-proxy denies the EXEC
# family (CONTROL_PLANE.md # Docker socket exposure; DECISIONS.md 2026-06-15).
# Exercise that exact capability against the *real* booted proxy, from the
# brain's own vantage (its DOCKER_HOST is the proxy): a one-shot run+attach+rm
# must succeed and a `docker exec` must still be refused. The full provision/
# drop/readiness round-trip is covered by the dockerlive suite and is the M2
# app-install lane in the VM; this is the transport-capability check #185 turns
# on. The moose-brain image is reused as both the docker-client context (it ships
# the docker CLI) and the throwaway run-image (it ships /bin/sh) — no extra image
# is bundled.
oneshot="$(docker exec moose-brain docker run --rm --entrypoint sh moose-brain -c 'echo MOOSE_ONESHOT_OK' 2>&1 || true)"
grep -q 'MOOSE_ONESHOT_OK' <<<"$oneshot" \
    || fail "one-shot 'docker run --rm' (managed-DB provisioning transport) failed through the proxy: $oneshot"
# EXEC must stay denied — the proxy boundary the provisioning re-architecture was
# built to respect rather than widen.
execout="$(docker exec moose-brain docker exec moose-caddy true 2>&1 || true)"
grep -qE '403|[Ff]orbidden|denied' <<<"$execout" \
    || fail "docker exec is NOT denied through the proxy (EXEC boundary breached): $execout"
echo "managed-DB #185: one-shot run+attach permitted through the proxy, EXEC still denied"

# --- M1c (#166): headless first-run. POST /setup creates the admin through the
# real useradd/chpasswd/gpasswd path; POST /login then authenticates that account
# against /etc/shadow via host-agent verify-password (the brain holds no password
# hash — AUTH.md). Both go through Caddy on :80, the same scriptable HTTP path the
# QEMU harness drives with no browser. No curl/jq in the image — hand-build the
# request over bash /dev/tcp, same idiom as http_status above.
SETUP_USER=mooseadmin
SETUP_PASS=moosefirstrunpw1
setup_body="{\"username\":\"$SETUP_USER\",\"password\":\"$SETUP_PASS\"}"

# http_post PATH HOST JSON -> prints the full HTTP response (status line + headers
# + body). HTTP/1.0 + Connection: close so the server closes the stream and `cat`
# returns. Content-Length is the BYTE length (wc -c), not ${#body}'s shell char
# count — a multi-byte char (e.g. an accented username) would otherwise mis-size
# the body and the server would truncate/reject it.
http_post() {
    local body="$3" len
    len="$(printf '%s' "$body" | wc -c | tr -d ' ')"
    exec 3<>/dev/tcp/127.0.0.1/80 || return 1
    printf 'POST %s HTTP/1.0\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s' \
        "$1" "$2" "$len" "$body" >&3
    cat <&3
    exec 3>&- 3<&-
}

# 1. /setup. 200 on a fresh box; 409 ("setup has already completed") when this
# disk has already been through first-run — the medium lane reuses one disk
# across the first-boot and second-boot phases, so the admin created on boot 1
# (and the brain's SQLite row with it) is still present on boot 2. A 502 means
# the host path failed (missing moose/sudo group, useradd/chpasswd error); poll
# briefly to ride out brain-not-ready, then surface the body for diagnosis.
setup_status=""
setup_resp=""
for _i in $(seq 1 20); do
    setup_resp="$(http_post /api/v1/setup moose.local "$setup_body" 2>/dev/null || true)"
    setup_status="$(head -1 <<<"$setup_resp" | tr -d '\r')"
    case "$setup_status" in
        *" 200"*|*" 409"*) break ;;
    esac
    sleep 1
done
case "$setup_status" in
    *" 200"*|*" 409"*) ;;
    *) fail "/setup did not complete: status='$setup_status' body=$(tr -d '\r' <<<"$setup_resp" | tail -2 | tr '\n' ' ')" ;;
esac

# 2. The account is a real Linux user: primary group moose (useradd --gid moose)
# and a member of sudo (the first admin is added to sudo at creation —
# USERS_AND_GROUPS.md # Roles). Proves SetPassword + SetRole hit the real system.
id "$SETUP_USER" >/dev/null 2>&1 \
    || fail "admin '$SETUP_USER' not in /etc/passwd after /setup"
id -nG "$SETUP_USER" 2>/dev/null | grep -qw sudo \
    || fail "admin '$SETUP_USER' not in sudo group (groups: $(id -nG "$SETUP_USER" 2>/dev/null))"
id -ng "$SETUP_USER" 2>/dev/null | grep -qx moose \
    || fail "admin '$SETUP_USER' primary group is '$(id -ng "$SETUP_USER" 2>/dev/null)', want moose"

# 3. /login authenticates the account against /etc/shadow via host-agent
# verify-password (PAM pam_unix, service "moose"). This is the M1c "Done when".
# Single attempt: by here useradd+chpasswd have completed (200/409 above), so a
# correct password logs in first try — and the brain rate-limits failed logins
# (AUTH.md # Rate limiting), so retrying a real failure would only lock us out.
# On second-boot the 409 above means the admin and its /etc/shadow entry survived
# the encrypted-root reboot, so the same credentials authenticate here too.
login_status="$(http_post /api/v1/login moose.local "$setup_body" 2>/dev/null | head -1 | tr -d '\r')"
case "$login_status" in
    *" 200"*) ;;
    *) fail "/login (verify-password against /etc/shadow) failed: status='$login_status'" ;;
esac

echo "control-plane M1c: /setup created the admin, verify-password authenticated it against /etc/shadow"

# --- SSH posture on the appliance (#467; BUILD.md # SSH) ------------------
# The two files the appliance must ship, and the rule actually being loaded.
# Both halves matter separately: a file that lands but is never loaded scopes
# nothing, and a rule loaded from something not checked in is not a shipped
# posture.
#
# What this does NOT do is flip the per-account toggle. Enabling an account makes
# host-agent render an AllowUsers naming that account and no other, and disabling
# the last one stops sshd — either would cut this very connection, which is how
# every assertion in this file reaches the box. The daemon lifecycle and the
# toggle are proved in the cloud lane, over a serial console with nothing to lose
# (dev/cloud/cloud-assertions.sh, the `ssh` boot).
assert_ssh_posture() {
    [ -f /etc/ssh/sshd_config.d/moose-hardening.conf ] \
        || fail "sshd hardening drop-in missing from the image (/etc/ssh/sshd_config.d/moose-hardening.conf)"
    grep -qE '^PermitRootLogin +no$' /etc/ssh/sshd_config.d/moose-hardening.conf \
        || fail "hardening drop-in does not set PermitRootLogin no"
    grep -qE '^PasswordAuthentication +yes$' /etc/ssh/sshd_config.d/moose-hardening.conf \
        || fail "hardening drop-in does not set PasswordAuthentication yes (the prerequisite for the password half of AuthenticationMethods)"

    [ -f /etc/nftables.d/moose-ssh.conf ] \
        || fail "SSH firewall rule missing from the image (/etc/nftables.d/moose-ssh.conf)"

    # The loader ran and the rule is live in the kernel, not just on disk.
    systemctl is-active --quiet moose-ssh-firewall.service \
        || fail "moose-ssh-firewall.service is not active: $(systemctl status moose-ssh-firewall.service --no-pager 2>&1 | tail -10)"
    rules="$(nft list table inet moose_ssh 2>&1)" \
        || fail "nftables table inet moose_ssh not loaded: $rules"

    # Default-deny plus the three private ranges. Assert the drop and each allow
    # separately — a rule set that dropped everything would pass a "drop exists"
    # check while locking the whole LAN out, and one that allowed everything
    # would pass an "allow exists" check while scoping nothing.
    grep -qE 'tcp dport 22 .*drop' <<<"$rules" \
        || fail "no default drop on :22 in the moose_ssh table: $rules"
    for range in 10.0.0.0/8 172.16.0.0/12 192.168.0.0/16; do
        grep -qF "$range" <<<"$rules" \
            || fail "private range $range is not allowed to reach :22: $rules"
    done

    # Both families, and this is the easy one to lose. In an inet table an
    # `ip saddr` match compiles to an nfproto==IPv4 test first, so it can never
    # match an IPv6 packet — drop the v6 accepts and every IPv6 connection falls
    # into the final drop instead. A LAN client resolving the box over mDNS
    # commonly gets an AAAA record, so that would hang `ssh moose.local` while the
    # rule still read as if it allowed the LAN.
    for range in fe80::/10 fc00::/7; do
        grep -qF "$range" <<<"$rules" \
            || fail "IPv6 LAN range $range is not allowed to reach :22, so every IPv6 client hits the drop: $rules"
    done

    # Containers are excluded, and the ORDER is what makes that true: Docker's
    # bridges are numbered inside 172.16.0.0/12, so a drop placed after the
    # RFC1918 accept would never be reached and a compromised app container would
    # sit in front of the box's own sshd.
    for iface in docker0 'br-*'; do
        grep -qF "iifname \"$iface\"" <<<"$rules" \
            || fail "container interface $iface is not excluded from the :22 allow, so an app container can reach sshd: $rules"
    done
    docker_line="$(grep -n 'iifname "docker0"' <<<"$rules" | head -1 | cut -d: -f1)"
    lan_line="$(grep -n '172\.16\.0\.0/12' <<<"$rules" | head -1 | cut -d: -f1)"
    [ -n "$docker_line" ] && [ -n "$lan_line" ] && [ "$docker_line" -lt "$lan_line" ] \
        || fail "the container drop does not come before the RFC1918 accept, so it is dead code (docker=$docker_line lan=$lan_line): $rules"

    # The harness's own drop-in still sorts first, so root login survives every
    # rebuild. This is the check that catches someone renaming it back.
    [ -f /etc/ssh/sshd_config.d/00-medium-test.conf ] \
        || fail "harness sshd drop-in is not named to sort before moose-*.conf; root login would be shut off by the hardening drop-in"
    echo "appliance SSH posture: hardening drop-in + LAN-scoped :22 rule shipped and loaded"
}
assert_ssh_posture

case "$PHASE" in
    first-boot)
        # The run-once enrollment unit (moose-tpm-enroll.service) is
        # ordered Before=moose-storage-ready.target but ssh.service has no
        # ordering against it, so SSH can win the race and we may arrive
        # before enrollment finishes. Wait for the marker (written only on
        # a successful enroll); fail fast if the unit itself failed.
        for _i in $(seq 1 120); do
            [ -f /var/lib/moose/.luks-tpm-enrolled ] && break
            if systemctl is-failed --quiet moose-tpm-enroll.service; then
                fail "moose-tpm-enroll.service failed: $(journalctl -u moose-tpm-enroll.service -b --no-pager 2>/dev/null | tail -20)"
            fi
            sleep 1
        done
        [ -f /var/lib/moose/.luks-tpm-enrolled ] \
            || fail "enrollment marker never appeared after 120s (enroll service stuck?)"
        assert_tpm2_pcr7_token
        ;;
    second-boot)
        # Reaching here is itself the unattended-unseal proof: the harness
        # launched this boot's QEMU with no cryptsetup.passphrase
        # credential and the console is serial-only, so the recovery
        # keyslot was unusable — the only way root unlocked is the
        # PCR-7-bound TPM2 token enrolled on the first boot. Confirm the
        # token persisted and the enrollment marker survived the reboot.
        [ -f /var/lib/moose/.luks-tpm-enrolled ] \
            || fail "enrollment marker missing on second boot (did first-boot enrollment not persist?)"
        assert_tpm2_pcr7_token
        # Best-effort, non-fatal: surface the initrd TPM2-unlock line into
        # the serial log for debugging. Wording varies across systemd
        # versions, so this is a hint, not an assertion — the structural
        # proof above is load-bearing.
        journalctl -b --no-pager 2>/dev/null \
            | grep -iE 'tpm2|cryptsetup' | tail -5 || true
        # M2 (#167): the full-stack app install. Runs before assert_network_state
        # so the network test's interface disconnect / IP renumber can't disturb
        # whoami's mDNS resolution mid-assertion.
        assert_app_install
        assert_network_state
        ;;
    combined) ;;
    *) fail "unknown phase '$PHASE'" ;;
esac

ok
