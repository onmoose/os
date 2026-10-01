# Shared helpers for the OS package lock (BUILD.md # 1b # The OS package lock,
# #560). Sourced, not run, by dev/cloud/bootstrap.sh (the lean image that ships)
# and dev/cloud/test/bootstrap.sh (the boot-proof image), so both builds install
# the same versions from the same sources.
#
# Expects these set by the sourcing script: REPO_ROOT, CALLER (may be empty), GO.
#
# What the lock pins, and how:
#   - Debian: dev/os-lock/debian-snapshot is a snapshot.debian.org timestamp.
#     mkosi gets it as --snapshot, so the main archive and debian-security come
#     from snapshot.debian.org at that moment (mkosi v26 does both). mkosi leaves
#     out trixie-updates in snapshot mode, so it is added back here from the same
#     snapshot: the image keeps the suites it had before the lock.
#   - Docker: its repo is not in the snapshot. dev/os-lock/third-party.lock names
#     exact versions; each becomes an apt pin (Pin-Priority 1001).
#   - The result: dev/os-lock/cloud-packages.lock, checked after the build by
#     os_lock_check.
#
# There is no fallback to a live mirror when snapshot.debian.org is slow or down:
# that would break the lock. apt retries (Acquire::Retries), and then the build
# fails. A failed release is re-run (docs/dev/contributing.md # Release model).

OS_LOCK_DIR="${REPO_ROOT}/dev/os-lock"

# os_lock_snapshot prints the locked timestamp, or fails on a malformed file.
os_lock_snapshot() {
    local ts
    ts="$(tr -d '[:space:]' < "${OS_LOCK_DIR}/debian-snapshot")"
    if ! printf '%s' "$ts" | grep -Eq '^[0-9]{8}T[0-9]{6}Z$'; then
        echo "dev/os-lock/debian-snapshot does not hold a snapshot.debian.org timestamp (got '$ts')" >&2
        return 1
    fi
    printf '%s\n' "$ts"
}

# stage_os_lock_apt PKGMNGR_DIR: write the build's apt sources, pins and retry
# setting into an mkosi.pkgmngr tree (mkosi uses it as apt's /etc/apt). Replaces
# the tree, so nothing stale survives.
stage_os_lock_apt() {
    local pkgmngr="$1" ts
    ts="$(os_lock_snapshot)" || return 1
    rm -rf "$pkgmngr"
    mkdir -p "$pkgmngr/etc/apt/keyrings" "$pkgmngr/etc/apt/sources.list.d" \
        "$pkgmngr/etc/apt/preferences.d" "$pkgmngr/etc/apt/apt.conf.d"

    # Docker's repo, trixie pocket. Build-host network only; the VM never
    # apt-installs (BUILD.md # Docker package source).
    curl -fsSL --retry 5 https://download.docker.com/linux/debian/gpg \
        -o "$pkgmngr/etc/apt/keyrings/docker.asc"
    chmod a+r "$pkgmngr/etc/apt/keyrings/docker.asc"
    cat > "$pkgmngr/etc/apt/sources.list.d/docker.list" <<'EOF'
deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian trixie stable
EOF

    # trixie-updates from the same snapshot (mkosi drops it in snapshot mode).
    cat > "$pkgmngr/etc/apt/sources.list.d/debian-updates.list" <<EOF
deb [signed-by=/usr/share/keyrings/debian-archive-keyring.gpg] https://snapshot.debian.org/archive/debian/${ts} trixie-updates main
EOF

    # One exact pin per third-party package.
    local line name ver pref="$pkgmngr/etc/apt/preferences.d/moose-os-lock.pref"
    : > "$pref"
    while IFS= read -r line || [ -n "$line" ]; do
        line="${line%%#*}"
        line="$(printf '%s' "$line" | tr -d '[:space:]')"
        [ -n "$line" ] || continue
        name="${line%%=*}"
        ver="${line#*=}"
        if [ -z "$name" ] || [ -z "$ver" ] || [ "$name" = "$line" ]; then
            echo "dev/os-lock/third-party.lock: '$line' is not package=version" >&2
            return 1
        fi
        printf 'Package: %s\nPin: version %s\nPin-Priority: 1001\n\n' "$name" "$ver" >> "$pref"
    done < "${OS_LOCK_DIR}/third-party.lock"

    # snapshot.debian.org can be slow; retry a fetch before failing the build.
    cat > "$pkgmngr/etc/apt/apt.conf.d/80-moose-os-lock" <<'EOF'
Acquire::Retries "5";
EOF
    echo "OS lock: Debian snapshot ${ts}, $(grep -c '^Package:' "$pref") third-party pins"
}

# os_lock_tool OUT: build dev/os-lock/oslock to OUT, as the caller so root never
# owns the caller's Go build cache.
os_lock_tool() {
    local out="$1"
    if [ -n "${CALLER:-}" ]; then
        sudo -u "$CALLER" "$GO" build -C "$REPO_ROOT" -o "$out" ./dev/os-lock/oslock
    else
        "$GO" build -C "$REPO_ROOT" -o "$out" ./dev/os-lock/oslock
    fi
}

# os_lock_check MANIFEST OUT_LOCK: write the resolved list for MANIFEST to
# OUT_LOCK, then compare it with the committed dev/os-lock/cloud-packages.lock.
#
# With MOOSE_OS_LOCK_RECORD=1 the committed file is replaced instead of checked.
# Only the bump workflow sets it (.github/workflows/os-lock-bump.yml), right
# after it moved the snapshot or a pin; every other build checks.
os_lock_check() {
    local manifest="$1" out="$2" tool
    tool="$(dirname "$out")/oslock"
    os_lock_tool "$tool" || return 1
    "$tool" normalize "$manifest" > "$out" || return 1
    if [ "${MOOSE_OS_LOCK_RECORD:-}" = "1" ]; then
        cp "$out" "${OS_LOCK_DIR}/cloud-packages.lock"
        [ -n "${CALLER:-}" ] && chown "$CALLER":"$(id -gn "$CALLER")" "${OS_LOCK_DIR}/cloud-packages.lock" 2>/dev/null
        echo "OS lock: recorded $(grep -vc '^#' "$out") packages to dev/os-lock/cloud-packages.lock (MOOSE_OS_LOCK_RECORD=1)"
        return 0
    fi
    if [ ! -f "${OS_LOCK_DIR}/cloud-packages.lock" ]; then
        echo "dev/os-lock/cloud-packages.lock is missing. The resolved list is at $out." >&2
        return 1
    fi
    "$tool" check "$manifest" "${OS_LOCK_DIR}/cloud-packages.lock"
}
