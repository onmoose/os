#!/usr/bin/env bash
# SPIKE (#485), not for merge. Builds one engine's three images and its
# update artifacts.
#
#   dev/spike-ab-update/build.sh rauc|sysupdate
#
# Output, under .dev/spike-ab/<engine>/:
#   v1/ v2/ v3/     mkosi output per version. v1's disk image is what boots.
#                   v3 is the broken one: its health gate always fails.
#   ctl.img         the air-gapped control disk (ext4, label SPIKECTL) that
#                   carries the v2 and v3 update artifacts into the guest
#   sizes.txt       artifact and slot sizes, for the write-up
#
# CI only (a GitHub-hosted runner with mkosi v26). Never run it on a laptop:
# moose never builds images locally.
set -euo pipefail

ENGINE="${1:?usage: build.sh rauc|sysupdate}"
case "$ENGINE" in rauc|sysupdate) ;; *) echo "unknown engine $ENGINE" >&2; exit 2 ;; esac

SPIKE="$(cd "$(dirname "$0")" && pwd)"
REPO="$(cd "$SPIKE/../.." && pwd)"
TOP="$REPO/.dev/spike-ab"
OUT="$TOP/$ENGINE"
PROFILE="$SPIKE/mkosi.profiles/$ENGINE"
mkdir -p "$OUT" "$TOP/keys"

log() { echo "=== build.sh [$ENGINE]: $*"; }

if [ "$ENGINE" = rauc ]; then
    # A throwaway signing key for the spike. RAUC signs bundles with X.509/CMS;
    # the box trusts whatever CA is in its keyring.
    if [ ! -s "$TOP/keys/cert.pem" ]; then
        openssl req -x509 -newkey rsa:4096 -nodes -days 30 \
            -keyout "$TOP/keys/key.pem" -out "$TOP/keys/cert.pem" \
            -subj "/O=moose spike/CN=moose-spike-485" 2>/dev/null
    fi
    install -D -m 0644 "$TOP/keys/cert.pem" "$PROFILE/mkosi.extra/etc/rauc/keyring.pem"
else
    bin="$TOP/sysupdate-bin/systemd-sysupdate"
    if [ ! -x "$bin" ]; then
        log "building systemd-sysupdate (Debian does not ship it)"
        t0=$(date +%s)
        "$SPIKE/sysupdate/build-sysupdate.sh" "$TOP/sysupdate-bin"
        echo "sysupdate build seconds: $(( $(date +%s) - t0 ))" > "$TOP/sysupdate-bin/build-time.txt"
    fi
    install -D -m 0755 "$bin" "$PROFILE/mkosi.extra/usr/lib/systemd/systemd-sysupdate"
fi

for V in 1 2 3; do
    broken=0; [ "$V" = 3 ] && broken=1
    extra=()
    if [ "$ENGINE" = sysupdate ]; then
        # The slot label carries the version. sysupdate matches moose-spike_@v,
        # and the UKI boots the partition with that label.
        cat > "$PROFILE/mkosi.repart/20-root.conf" <<EOF
# Written by build.sh. Slot A, which holds v$V in this build.
[Partition]
Type=root
Label=moose-spike_$V
UUID=a0a0a0a0-0000-4000-8000-00000000000a
Format=ext4
CopyFiles=/
ExcludeFiles=/efi/
SizeMinBytes=3G
SizeMaxBytes=3G
EOF
        extra+=( "--kernel-command-line=root=PARTLABEL=moose-spike_$V" )
    fi
    log "mkosi build v$V (broken=$broken)"
    t0=$(date +%s)
    mkosi --directory="$SPIKE" --profile="$ENGINE" \
        --image-version="$V" \
        --environment="SPIKE_VERSION=$V" --environment="SPIKE_BROKEN=$broken" \
        --output-directory="$OUT/v$V" --cache-directory="$TOP/cache-$ENGINE" \
        "${extra[@]}" -f build
    log "v$V built in $(( $(date +%s) - t0 ))s"
    ls -la "$OUT/v$V"
done

# The update artifacts, as a box would download them.
rm -rf "$OUT/ctl"
mkdir -p "$OUT/ctl"
for V in 2 3; do
    root="$OUT/v$V/moose-spike_$V.root-x86-64.raw"
    [ -s "$root" ] || { echo "missing split root partition $root" >&2; ls "$OUT/v$V" >&2; exit 1; }
    if [ "$ENGINE" = rauc ]; then
        b="$OUT/bundle-v$V"
        rm -rf "$b"; mkdir -p "$b"
        cp --sparse=always "$root" "$b/rootfs.ext4"
        cat > "$b/manifest.raucm" <<EOF
[update]
compatible=moose-spike
version=$V

[bundle]
format=verity

[image.rootfs]
filename=rootfs.ext4
EOF
        mkdir -p "$OUT/ctl/rauc"
        log "rauc bundle v$V"
        docker run --rm -v "$OUT:/out" -v "$TOP/keys:/keys:ro" debian:trixie bash -euc "
            apt-get update -qq >/dev/null
            apt-get install -y -qq --no-install-recommends rauc squashfs-tools >/dev/null
            rauc --version
            rauc bundle --cert=/keys/cert.pem --key=/keys/key.pem /out/bundle-v$V /out/ctl/rauc/v$V.raucb
            rauc info --keyring=/keys/cert.pem /out/ctl/rauc/v$V.raucb
            chown -R $(id -u):$(id -g) /out/ctl /out/bundle-v$V"
        rm -rf "$b"
    else
        d="$OUT/ctl/sysupdate/v$V"
        mkdir -p "$d"
        zstd -q -T0 -10 "$root" -o "$d/moose-spike_$V.root-x86-64.raw.zst"
        cp "$OUT/v$V/moose-spike_$V.efi" "$d/"
        (cd "$d" && sha256sum -- * > SHA256SUMS)
    fi
done

{
    echo "engine: $ENGINE"
    echo "slot size (fixed partition): 3G"
    for V in 1 2 3; do
        echo "v$V root ext4 image: apparent $(du -h --apparent-size "$OUT/v$V/moose-spike_$V.root-x86-64.raw" | cut -f1), allocated $(du -h "$OUT/v$V/moose-spike_$V.root-x86-64.raw" | cut -f1)"
    done
    echo "update artifacts a box downloads per update:"
    (cd "$OUT/ctl" && find . -type f -printf '  %p %s bytes\n' | sort)
    [ -f "$TOP/sysupdate-bin/build-time.txt" ] && [ "$ENGINE" = sysupdate ] && cat "$TOP/sysupdate-bin/build-time.txt"
} | tee "$OUT/sizes.txt"

# The control disk. mkfs.ext4 -d needs no root and no loop device.
rm -f "$OUT/ctl.img"
need_mb=$(( $(du -sm "$OUT/ctl" | cut -f1) + 512 ))
truncate -s "${need_mb}M" "$OUT/ctl.img"
mkfs.ext4 -q -L SPIKECTL -d "$OUT/ctl" "$OUT/ctl.img"
log "control disk ready: $OUT/ctl.img (${need_mb} MiB)"
