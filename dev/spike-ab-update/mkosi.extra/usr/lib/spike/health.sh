#!/bin/bash
# SPIKE (#485), not for merge. The health gate. It stands in for "host-agent
# and the brain are healthy" in the real design.
#
# On success the slot is marked good, the engine's way:
#   rauc       rauc status mark-good (sets <slot>_OK=1 <slot>_TRY=0 in grubenv)
#   sysupdate  nothing here: this unit is RequiredBy=boot-complete.target, and
#              systemd-bless-boot marks the UKI good once that target is reached
#
# On failure the unit has FailureAction=reboot, so a slot that never gets
# healthy reboots on its own, with nobody at a console. The boot loader then
# falls back to the other slot.
set -u
say() { echo "SPIKE: health: $*" > /dev/ttyS0; }

engine=$(cat /usr/lib/spike/engine)

if [ -e /usr/lib/spike/broken ]; then
    say "this is the broken image, failing the health check (reboot follows)"
    mkdir -p /data/spike && echo "$(date +%s)" >> /data/spike/broken-boots && sync
    sleep 5
    exit 1
fi

for _ in $(seq 1 60); do
    docker info >/dev/null 2>&1 && break
    sleep 1
done
if ! docker info >/dev/null 2>&1; then
    say "docker is not answering"
    exit 1
fi

if [ "$engine" = rauc ]; then
    if ! rauc status mark-good >/dev/null 2>&1; then
        say "rauc status mark-good failed"
        exit 1
    fi
    say "healthy, rauc marked the booted slot good"
else
    say "healthy, systemd-bless-boot will mark the UKI good"
fi
