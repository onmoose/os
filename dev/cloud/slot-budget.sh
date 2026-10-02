# shellcheck shell=bash
# The OS slot budget (#561, BUILD.md # 1b # Disk budget). Sourced by
# dev/cloud/bootstrap.sh (the image that ships), which fails when the squashfs
# in slot A fills more than 60% of its fixed 1 GiB partition, and by
# dev/cloud/test/bootstrap.sh (the boot-proof image), which only reports: that
# image also bakes test-only images (postgres:16, a registry), which never ship.
#
# Expects these set by the sourcing script: REPO_ROOT, CALLER (may be empty), GO.

# slot_budget_check IMAGE.raw [MAX_PERCENT] [TITLE]: build dev/cloud/slotbudget
# as the caller (so root never owns the caller's Go build cache) and run it on
# IMAGE. MAX_PERCENT defaults to the budget, 60. In CI the table also goes to
# the job summary, under TITLE.
#
# With MOOSE_OS_LOCK_RECORD=1 (the lock bump's record build) an over-budget
# image does not stop the build, the same as the lean check: the bump still
# needs its resolved list, and the bump PR's own CI run fails on it.
slot_budget_check() {
    local image="$1" max="${2:-60}" title="${3:-OS slot budget (the image that ships)}" tool rc=0
    [ -f "$image" ] || { echo "slot budget: no image at '$image'" >&2; return 1; }
    tool="$(dirname "$image")/slotbudget"
    if [ -n "${CALLER:-}" ]; then
        sudo -u "$CALLER" "$GO" build -C "$REPO_ROOT" -o "$tool" ./dev/cloud/slotbudget || return 1
    else
        "$GO" build -C "$REPO_ROOT" -o "$tool" ./dev/cloud/slotbudget || return 1
    fi
    "$tool" -max-percent "$max" -title "$title" ${GITHUB_STEP_SUMMARY:+-summary "$GITHUB_STEP_SUMMARY"} "$image" || rc=$?
    if [ "$rc" -ne 0 ]; then
        if [ "$rc" -eq 1 ] && [ "${MOOSE_OS_LOCK_RECORD:-}" = "1" ]; then
            echo "slot budget failed; going on because MOOSE_OS_LOCK_RECORD=1" >&2
            return 0
        fi
        return "$rc"
    fi
}
