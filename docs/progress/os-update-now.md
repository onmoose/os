# Settings → System covers the OS update: the restart, the last result, and Update now

- **Status:** done
- **Date:** 2026-10-12
- **Specs touched:** `docs/specs/UPDATES.md` (# 1, # 6, # 7, # 8.2, # 8.4), `docs/specs/DECISIONS.md` (2026-10-12), `docs/specs/NEXT.md` (# Reboot scheduling UX, narrowed), `docs/specs/BRAIN_HOST_PROTOCOL.md`, `docs/specs/BRAIN_UI_PROTOCOL.md`, `docs/architecture.md`, `docs/dev/hosted-boot-proof.md`

Closes #509, the OS half of the update screen. It follows [system-settings-updates.md](system-settings-updates.md) (#488), which built Settings → System for the control plane, and [host-agent-os-update.md](host-agent-os-update.md) (#563), which built the OS update on hosted. Much of #509 was already there: #488 shows the OS version and slot, and #563 keeps `os.last` on disk across the restart. What was left was the screen's OS sentences and Update now for the OS. The maintainer chose that Update now covers the OS, so the box can restart outside the window (`DECISIONS.md` 2026-10-12).

## What was done

### host-agent

- **`osupdate.Applier.SwitchNow`**: the window's `os-switch` without its two per-night guards ("tried tonight" and "one switch per night"). What keeps the box safe stays: the release must already be installed in the other slot (nothing is downloaded), and nothing moves while the booted slot is on trial. The switch records the night it counts for, so the window does not switch to a release that just reverted.
- **`updatetarget.Loop.ApplyNow`** covers both streams in the window's order. Stream B starts at once when its target differs from what runs. When the OS release the loop picked is installed (read fresh through `Peek`, since the tick can be 15 minutes old), the box switches to it. With both, the switch waits for stream B's job: `ApplyNow` records it (`manualOS`, under the same lock as the night guard) and wakes `Run`, which then only retries the switch every 10 s until it starts or an hour passes, and then goes back to ticking. The switch goes ahead after a failed stream B too, as in the window.
- A release that is still downloading is not waited for: Update now covers what is ready. A held lock, or a boot on trial, is `409 job-running` (`updatetarget.ErrBusy`, mapped through the new `hostagent.ErrJobRunning`).

### The screen (`SystemSection.vue`)

- One sentence per OS state (`installing`, `installed`, `waiting`, `rebooting`, `held`, `failed`, `refused`, `current`), with `os.detail` under "Technical reason". Nothing for `unsupported` or `none`.
- **The restart is said before the click.** With an installed release: "Updating now restarts your box. Your apps and the dashboard are offline for a few minutes." The dialog says the box goes back on its own if the new system does not start, which can take up to 15 minutes, and its title names what is installed ("Install control plane 0.18.0 and moose 0.16.2 now?").
- **The wait survives the restart.** The sessionStorage marker now holds the stream B job (if any), the OS release (if any), the id of the last OS outcome before the click, and the time. Stream B is followed through its job as before; a reload after a new dashboard keeps the marker and goes on to the OS part. The OS part is followed through the update-target read: done when the box runs the new release, or when a new `os.last` names it (a revert). Requests that fail while the box is down only mean "ask again". "Taking longer than expected" comes after 25 minutes with an OS switch, 10 without.
- **Last updates** lists both records: the control plane's `last` and the OS's `os.last` ("System updated to moose 0.16.2 on 12 Oct at 14:05", or "... did not start properly ..., so your box went back to moose 0.16.1").

### Fake host-agent

With `MOOSE_FAKE_UPDATE_PROFILE=hosted` and `MOOSE_FAKE_OS_UPDATE=installed`, Update now plays both streams: the control plane first, then `os.state: rebooting` for 8 s, then the box on the new release with a good `os.last` (`MOOSE_FAKE_UPDATE_APPLY=fail` gives `held` and a reverted `os.last`).

## How it was verified

- **Tests.** `osupdate`: `SwitchNow` before the install does nothing, skips a "tried tonight" hold and reboots into B, waits on a held lock, never moves during a trial. `updatetarget`: an installed OS alone switches at once and counts for the coming night; both streams start B first and switch on the next manual step once the lock is free; a switch that never gets the lock is dropped after an hour; a release still installing is refused; a held lock is `ErrBusy`; `Run` wakes for a waiting switch instead of sleeping 15 minutes. `make check` and `npm run build` green.
- **The fake over its real socket**: one apply with both streams pending gave the control plane `good`, then `os.state: rebooting`, then `os.running` on the new release with a good `os.last` under an id built from the job.
- **The cloud lane's `os-update` boot** now switches through Update now: the window in the answer stays shut, the owner's `POST /api/v1/system/update-target/apply` is accepted, and the box switches to slot B and comes up marked good, with every check the boot already made. `os-revert` still switches through the window, so both ways in are proven. This runs in the full CI list, not on a PR's default `update` boot.
- **The screen was not checked by looking at it**, as for #488.

## Known gaps & deviations

- **Both streams in one click are not proven on a booted box.** The `os-update` boot's answer carries only an OS part (by design, to prove the streams are judged apart), so its Update now is the OS switch alone. The B-then-A order is covered by the loop's tests and the fake.
- **A release still downloading is not waited for.** The admin sees "is downloading" and no button until it is installed.
- **Pushing one night's restart back is still open** (`NEXT.md` # Reboot scheduling UX).
- **Appliance:** nothing here, as for #488 (#564, #400).

## What's next

1. #486's proof on a provisioned box, which can now use Update now instead of waiting for the window.
2. `NEXT.md` # Reboot scheduling UX: whether an admin can push a night's restart back.
