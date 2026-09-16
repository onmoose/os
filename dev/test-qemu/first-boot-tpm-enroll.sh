#!/usr/bin/env bash
# First-boot LUKS TPM2 enrollment (slice 0023 Stage 2).
#
# Test-lane stand-in for host-agent's first-run enrollment step
# (STORAGE.md # First-run flow step 4). Adds a TPM2 keyslot to the root
# LUKS device bound to PCR 7, authorizing the new keyslot by unlocking an
# existing one with the recovery keyfile baked at
# /etc/moose/secrets/luks-recovery.key.
#
# The command is exactly STORAGE.md's (systemd-cryptenroll
# --tpm2-device=auto --tpm2-pcrs=7); only its caller differs — a oneshot
# unit here, host-agent first-run in production. Replace this with the
# real caller when host-agent's first-run lands.
#
# Run once: the unit gates on the marker (ConditionPathExists=!), and we
# write it only after a successful enroll, so a failed enroll re-runs on
# the next boot rather than silently staying un-enrolled.
set -euo pipefail

KEYFILE=/etc/moose/secrets/luks-recovery.key
MARKER=/var/lib/moose/.luks-tpm-enrolled

# Resolve the LUKS backing device from the mounted root. The root is
# /dev/mapper/luks-<uuid>; systemd-cryptenroll operates on the backing
# partition (where the LUKS header lives), not the mapper device.
root_src="$(findmnt -no SOURCE / 2>/dev/null || true)"
case "$root_src" in
    /dev/mapper/*|/dev/dm-*) ;;
    *) echo "root '$root_src' is not a dm-crypt device; refusing to enroll" >&2; exit 1 ;;
esac
backing="$(cryptsetup status "$root_src" 2>/dev/null \
    | awk '/^[[:space:]]*device:/ {print $2}')"
[ -n "$backing" ] || { echo "could not resolve LUKS backing device for $root_src" >&2; exit 1; }
[ -r "$KEYFILE" ] || { echo "recovery keyfile $KEYFILE missing/unreadable" >&2; exit 1; }

echo "enrolling TPM2 keyslot (PCR 7) on $backing (backing $root_src)"
# --unlock-key-file authorizes the new keyslot by opening the existing
# recovery keyslot; --tpm2-pcrs=7 binds the new keyslot's policy to PCR 7
# (Secure Boot state — stable across reboots under our plain-OVMF lane),
# so the next boot can unseal unattended with no passphrase present.
systemd-cryptenroll \
    --unlock-key-file="$KEYFILE" \
    --tpm2-device=auto \
    --tpm2-pcrs=7 \
    "$backing"

mkdir -p "$(dirname "$MARKER")"
date -u +%FT%TZ > "$MARKER"
echo "TPM2 enrollment complete; marker written to $MARKER"
