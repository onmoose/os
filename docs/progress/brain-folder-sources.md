# Folder sources for a containerized brain, #519

- **Status:** done
- **Date:** 2026-09-29
- **Specs touched:** `CONTROL_PLANE.md`, `BRAIN_HOST_PROTOCOL.md`, `APP_ISOLATION.md`, `STORAGE.md`, `THREAT_MODEL.md`, `TESTING.md`, `DECISIONS.md`

This closes #519. The bug was found by the #516 CI proofs, [userns-remap-ci-proofs.md](userns-remap-ci-proofs.md), under # Not caused by the remap: a containerized brain cannot prepare a household shared folder, because it cannot see `/srv/moose/shared`. That entry's What's next item 2 asked for this fix on its own.

The bug had two halves. On a real box the brain runs in a container that host-agent launches. The container mounted only the socket dir, `/var/lib/moose` and the profile marker.

- **Household.** `prepareSharedSource` stats `/srv/moose/shared` from inside the brain, finds nothing, and the install fails: "prepare shared source \"/srv/moose/shared/Documents\": shared root \"/srv/moose/shared\": stat /srv/moose/shared: no such file or directory". The hosted image never created that tree on the host either. Household is the default scope for an admin, so this hit users.
- **Personal.** The brain ran `MkdirAll` and `chown` on `/home/<user>/Documents`. With no `/home` mount, that made the folder in the container's own filesystem. On the host, Docker then created the bind source `root:root`, and the app, running as the owner with `cap_drop: ALL`, could not write it. host-agent does not create use-case folders at user creation (`usermgr`), so on a new account the folder never existed.

Neither half showed up before because every lane that installed a folder app ran a native brain, and the cloud lane's only app, whoami, has no folder grant.

## What was done

### The choice: mount the shared tree, but not `/home`

The issue asked to mount the shared tree, and to choose, for `/home`, between a mount and moving the prep to host-agent. **The shared tree is mounted into the brain. `/home` is not: host-agent prepares personal folders.**

- The shared tree is household space. Every member can already reach it through `moose-shared`, and the brain already owned its preparation (#156, `STORAGE.md` # Permissions). A mount keeps that code where it is.
- A home is one user's `0750` space. `BRAIN_HOST_PROTOCOL.md` already said the containerized brain cannot touch `/home`, and `CONTROL_PLANE.md` # Locked: host-agent hardening directives says any home operation is "a narrow, named operation", never a general file primitive. The Files endpoints follow the same rule.
- A compromised brain is host compromise anyway (`THREAT_MODEL.md` B8), so this is not a breach control. It is about the everyday case: a bug or a bad path in the large, LAN-facing brain should not be able to create, chown or read inside every user's home.

`DECISIONS.md` 2026-09-29 records it.

### host-agent: the shared tree and the brain mount

- `protocol.SharedRoot` (`/srv/moose/shared`) is now the one constant. The brain's `defaultSharedRoot` uses it.
- `brainlaunch.EnsureSharedTree(root, gid)` runs on every host-agent start, before the launch, on both profiles. A missing tree is created (and its parent). An existing one gets its group and mode set back to `moose-shared` `02770`. Only the root is touched, never anything inside it. A symlink or a file at that path is an error and nothing is changed.
- `cmd/host-agent-real` looks up the `moose-shared` group, calls it, and sets `Config.SharedRoot` only if it worked. Otherwise it logs an error and the brain launches without the mount, so Docker never creates the tree `root:root` `0755`.
- `runSpec` mounts `SharedRoot` read-write at the same path. The control-plane update builds the brain from the same `RunSpecFor`, so an updated brain keeps the mount.

### host-agent: `POST /v1/users/{username}/prepare-folder`

- New protocol type `PrepareUserFolderRequest{Path}`, and `protocol.UseCaseFolderDirs`.
- The handler (`internal/hostagent/agent.go`) checks the path with `ValidUserFolderPath`: relative, no empty, `.` or `..` level, and the first level must be a use-case folder. So the op can never reach `~/.ssh` or anything else in a home. Errors: 400 `bad-path`, 404 `unknown-user`, 409 `not-a-directory`.
- The real op (`usermgr.PrepareFolder`, `folder_linux.go`) resolves the home from `/etc/passwd`, never from the request. It then walks the path one level at a time with `openat(O_NOFOLLOW|O_DIRECTORY)`, starting from an fd on the home. A missing level is created and owned by the user. The last level is always owned by the user, which also repairs a `~/Documents` that an old Docker bind left `root:root`. Other existing levels are left alone. A symlink or a file on the way is `ErrNotADirectory`. The user owns the home and can put a symlink anywhere in it, so a root process that followed `~/Documents -> /etc` would hand `/etc` to them. `folder_other.go` is a stub so the package still builds off Linux.
- The fake branch (`make dev`) creates the path under the operator's own home, the one the fake `home` route hands out, with no chown. That is what the native brain did before.

### Brain

- `HostDriver` gains `PrepareUserFolder`, and `hostclient` implements it.
- `lifecycle.Install` asks host-agent for each personal folder (`Documents`, or `Documents/<subfolder>`) instead of `MkdirAll` + `chown`. A failure rolls the install back. There is no longer a warn-and-skip branch for an unprivileged brain: the fake host-agent handles dev.
- `prepareSharedSource` now uses `Lstat` and refuses a level that is a symlink or a file. The mount makes this code reachable on a real box for the first time, and any household member can write the shared tree. Docker follows a symlink in a bind source on the host, so `Documents -> /var/lib/moose` would have handed the brain's own state to a household app.

### CI lane

- A new synthetic fixture, `dev/cloud/test/catalog/filedrop`: busybox, a `documents` write grant, and a command that writes `id` into `/moose/documents/filedrop.txt` and then serves the folder so the container stays up. If the write fails the container exits and the install fails.
- `dev/cloud/test/bootstrap.sh` bakes `busybox:1.37.0` by digest next to whoami and adds filedrop to the catalog snapshot. The canary goes to v23.
- The `access` boot (`dev/cloud/cloud-assertions.sh` step 4c) checks that the brain mounts `/srv/moose/shared` and not `/home`, and that the tree is `0:2001 2770` before any install. It then installs filedrop household and checks `/srv/moose/shared/Documents` is `0:2001 2770` and `filedrop.txt` is `2000:2001`. Then it installs filedrop personal (with `confirm`) and checks that the container runs as the owner, and that `~/Documents` and `~/Documents/filedrop.txt` are owned by the owner. The install job is polled, so a red run names the brain's own error. The boot's outer timeout goes from 720s to 960s.

### Tests

- `usermgr`: `prepareUnder` creates every missing level, is idempotent, keeps existing content and mode, and refuses a symlink at the first, a deeper and the last level without writing through it, a file in the way, and bad input.
- `hostagent`: `ValidUserFolderPath`, delegation, the error mapping, a bad path never reaching the user manager, and the fake branch creating under `$HOME`.
- `hostclient`: the route and body on the wire.
- `lifecycle`: a personal install asks host-agent for `Documents/Work` for `alex` and no longer creates the folder itself; a host refusal rolls the install back; a household install asks for no home folder; `prepareSharedSource` refuses a symlink and a file at the leaf.
- `brainlaunch`: the shared-tree mount is there, read-write, and survives `RunSpecFor`; no `/home` mount; no mount when `SharedRoot` is empty; `EnsureSharedTree` creates, repairs only the root, and refuses a symlink or a file.
- `make check` passes.
- `CI / Cloud image` with `publish=false`: see # CI runs.

### CI runs

| Run | Result |
|---|---|
| 36564503254 | pending |

## How it maps to the specs

- `CONTROL_PLANE.md` # Locked: host-agent launches the brain container now lists what the brain mounts and why `/home` is not one of them.
- `BRAIN_HOST_PROTOCOL.md` # User info endpoints gains the prepare-folder op. The Files paragraph no longer says the brain cannot touch `/srv/moose`.
- `APP_ISOLATION.md` # Volumes: personal sources are prepared by host-agent; shared sources by the brain through the mount, refusing symlinks.
- `STORAGE.md` # Permissions: host-agent makes the shared tree exist before the brain launches.
- `THREAT_MODEL.md` B2: the brain's residual now includes read-write access to the shared tree.
- `TESTING.md` and `docs/dev/hosted-boot-proof.md`: the access boot's folder-app step.

## Known gaps & deviations

- **An existing brain container keeps its old mounts.** host-agent leaves a brain container that exists alone, and a container's mounts are fixed at create. So a box picks up the shared-tree mount when its brain is next created: a first boot, or a control-plane update, which recreates the brain from the new host-agent's `RunSpecFor`. A box that gets the new host-agent but no brain update stays broken for household folder installs until then. The proxy has a recreate-once check for its sandbox (#431). The brain does not get one here, because recreating a running brain at host-agent start can cut an install in half.
- **Lockstep.** A brain with this change calls a route an older host-agent does not have, so a personal folder install on it fails with a 404. It was already broken on a containerized brain. The release's `minimum_host_agent` should move with it.
- **Mount propagation.** The brain's bind of `/srv/moose/shared` is taken when the brain starts. If a data drive is later mounted over `/srv/moose`, the brain would keep seeing the old directory underneath. Drive enrollment is not built yet. When it lands, it has to restart or recreate the brain.
- **`EnsureSharedTree` does not check storage health.** On an appliance whose data drive failed to mount, it would create the tree on the OS drive. The data drive is not built on the appliance yet, and the brain blocks installs on `data-drive-missing` anyway.
- **The brain's shared prep is check-then-create**, not an fd walk like the host-agent op, so a symlink swapped in between the `Lstat` and the `Mkdir` is not caught. And Docker follows symlinks when it binds, so a symlink planted after install is not covered at all. Both need a household member with a shell, and the symlink refusal closes the easy case.
- **The appliance medium lane was not run.** It is local-only. Its whoami install is personal, so it now goes through the host-agent op.
- **The personal op sets the owner of the last level even if it already exists**, like the old brain code did. It does not repair a pre-existing parent, such as a `root:root` `~/Documents` with a new subfolder below it.

## What's next

1. If existing boxes should get the mount without waiting for a control-plane update, add a recreate-once check for the brain container, like the proxy's, run at a safe moment.
2. Run the appliance medium lane (`make test-medium-qemu`) once with this change.
3. The userns-remap implementation issue from [userns-remap-ci-proofs.md](userns-remap-ci-proofs.md) What's next item 1. Its folder-app proof no longer needs the harness fix.
