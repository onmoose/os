# host-agent's hardening lines, and the mount namespace it must keep

- **Status:** done
- **Date:** 2026-11-13
- **Specs touched:** `docs/specs/CONTROL_PLANE.md`, `docs/specs/DECISIONS.md`

Closes #667. Follows [folder-source-pins.md](folder-source-pins.md) (#604), which made host-agent bind-mount every folder source at `/run/moose/folder-pins/`. `CONTROL_PLANE.md` # Locked: host-agent hardening directives locked ten systemd lines, and `dist/systemd/host-agent.service` had none of them. Most of the ten would also have broken the pins without any error.

## What was done

- **The unit sets two lines:** `NoNewPrivileges=yes` and `LockPersonality=yes` (`dist/systemd/host-agent.service`). Neither makes a mount namespace. The image installs no AppArmor profiles, so `NoNewPrivileges=` blocks no profile change on exec. host-agent is root already, so a setuid binary gives it nothing.
- **Eight lines are dropped from the locked list, each with its reason** (`CONTROL_PLANE.md`). Seven of them (`ProtectSystem=`, `ProtectHome=`, `ReadWritePaths=`, `PrivateTmp=`, `ProtectKernelTunables=`, `ProtectKernelModules=`, `ProtectControlGroups=`) give the service its own mount namespace. That turns off mount propagation to the host, so Docker would bind an empty pin path and the app would start on an empty folder. The pin's own inode check would pass, because it runs in the same namespace. `RestrictSUIDSGID=` refuses the setgid bit that `brainlaunch.EnsureSharedTree` puts on `/srv/moose/shared` (`02770`).
- **The spec says host-agent must mount in the host namespace, and why**, next to the list. The narrow `/home` rule stays, held by the protocol, since `ProtectHome=` can no longer be named as what enforces it.
- **`DECISIONS.md` 2026-11-13** records the flip and the rejected alternative: keeping the namespace lines and moving the pin mounts into a helper outside the unit. host-agent can start any unit through systemd, so the lines only limit how far a bug reaches, which does not pay for splitting one root process into two.
- **A unit test** (`cmd/host-agent-real/unit_test.go`): fails if `host-agent.service` or `moose-folder-pins.service` (which restores the pins at boot with the same binary) sets any line that makes a mount namespace. It also fails on `RestrictSUIDSGID=` in `host-agent.service`, and pins the two lines that ship. Checked by adding `ProtectHome=true` to the unit: the test failed with the #667 message.
- **Boot checks** (`dev/cloud/cloud-assertions.sh`):
  - Every boot, step 1a: `systemctl show` reports both lines as `yes`, the running process has `NoNewPrivs: 1`, and its mount namespace is PID 1's. So PAM verify, `useradd`, the control-plane update and the OS update through RAUC all run under the new lines in CI.
  - The `access` boot, step 4c-2: the pin is a mount in the host namespace (the script's own `/proc/self/mountinfo`), and the app reads a file the host wrote into the folder, through `docker exec`. Before, the step proved only that the app's own file landed.
- **CI.** The full boot list passed on both firmwares (`CI / Cloud image` run 43457582560, `publish=false`). The `access` boot logged both new checks: host-agent in the host mount namespace with the two lines on, and the pin a host mount that the app reads through.

## Known gaps & deviations

- **Review.** The review agent found no Block. Its one note: `moose-folder-pins.service` also makes pins (at boot, with the same binary), and nothing stopped a namespacing line there. The unit test now reads that unit too. Its other note, that the unit test sees no drop-ins, is covered by step 1a, which checks the running process. Greptile found nothing.
- **`cryptsetup` is not checked under the new lines.** LUKS is not wired in host-agent yet. `NoNewPrivileges=` does not limit a root process's capabilities, so nothing here is expected to change, but no boot has run it.
- **The appliance medium lane did not run.** It copies the same unit (`dev/test-qemu/bootstrap.sh`), but it stays local-only.
- **`Type=notify` and the watchdog** are still not in the unit. They are a separate locked section and out of scope here.

## What's next

Nothing from this change.
