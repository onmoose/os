# Folder source pins: a folder app binds an inode, not a path

- **Status:** done
- **Date:** 2026-11-11
- **Specs touched:** `docs/specs/APP_ISOLATION.md`, `docs/specs/BRAIN_HOST_PROTOCOL.md`, `docs/specs/APP_LIFECYCLE.md`, `docs/specs/HEALTH.md`, `docs/specs/BOOT.md`, `docs/specs/DECISIONS.md`

Closes #604. A folder app used to keep the real host path in `compose.override.yml`, and Docker resolved that path at every container start. A check at install or update could not stop a terminal app from swapping a subfolder for a symlink between two starts, so the next start could bind `~/.ssh`. The fix pins the folder's inode, so the later resolution has nothing to race.

## What was done

- **host-agent pin op** (`POST /v1/folders/pin`, `internal/hostagent`, `internal/hostagent/usermgr/folder_linux.go`). The request carries the username, whether the source is shared, and the path relative to the home or `SharedRoot`. The real op walks the source one level at a time with `O_NOFOLLOW`, refuses a file, a symlink or a missing level, bind-mounts the walked directory at `/run/moose/folder-pins/<dev>-<ino>`, and checks the mount's device and inode against the walked fd. A pin that is already the same inode is reused. The fake op walks with `Lstat`, refuses the same cases, and returns the real path, because the dev loop cannot mount and does not need to.
- **Protocol and client** (`internal/protocol/host.go`, `internal/hostclient`). `PinFolderRequest`, `PinFolderResponse`, `FolderPinRequest`, `FolderPinRecord`, and `SystemStatus.FolderPins`. The client maps 404 to `ErrPinUnsupported` and 409 to `ErrFolderSourceSwapped`.
- **Brain pins file** (`internal/lifecycle/folderpins.go`). `refreshFolderPins` reads the instance's override, classifies each volume source as personal (under the owner's home) or shared (under the shared root), pins each distinct source, and writes `compose.pins.yml` and `folder-pins.json` atomically. The pins file replaces only the folder volumes; compose merges it after the override and replaces by container target. `composeRun` adds it when it exists. It runs before every `compose up` and `compose create`: `composeUpInstance`, install, `writeNewGeneration`, and `undoUpdate` after the previous generation is back. The override is never rewritten, so update, rollback and source detection keep reading the real path.
- **Refusal** (`internal/lifecycle`). A swapped or missing source returns a `folderRefusal` with a plain sentence: the app cannot start, the folder is not a folder any more, a file or a link is in its place, put it back and start again. Start shows it in the job and sets the row `failed` with the failed splash. A boot reconcile retries the up for an app whose pin the restorer could not mount, and marks it `failed` only when the source is still bad. Install and update roll back with the same sentence.
- **Old host-agent** (`fallbackFolderPins`). A 404 removes the pins files and re-runs the old `prepare-folder` / `prepareSharedSource` check, so folder apps keep working. The `folder-pin-unsupported` warning says the box needs an OS update.
- **Health** (`cmd/brain/main.go`, `internal/health/health.go`). The existing locus-C status read now also reconciles `folder-pin-unsupported` against `SystemStatus.FolderPins`, 1-shot like `version-mismatch`. A failed status read changes nothing.
- **Boot restorer** (`dist/systemd/moose-folder-pins.service`, `cmd/host-agent-real --restore-folder-pins`). The pin is a bind mount under `/run`, so a reboot clears it. The unit runs after the storage target and before `docker.service`, reads each instance's `folder-pins.json`, walks each source again, mounts only when the walked inode still matches the pin's name (the device can change when a data drive is remounted), and prunes pins no record references. A swapped source is left unpinned: Docker then gets an empty pin path, not the real folder, and the restorer writes `folder-pins.failed` beside the record. The brain's startup pass reads that marker, stops the container Docker started, and marks the app failed with the same plain refusal a Start uses.
- **Tests.** Lifecycle: install writes the pins file and record; a start after the folder is swapped for a link is refused, no compose up runs, and the row is `failed` (personal and shared); an old host-agent falls back and still starts; a reconcile marks a swapped running instance failed. hostclient: 200, 404 and 409 mapping. host-agent: the handler delegates, maps errors, refuses a bad path, and the fake refuses a link. usermgr (Linux, root): the walk refuses a link and the mount is the same inode, with a write through the pin landing in the source.
- **Cloud lane** (`dev/cloud`). A new `filedropsub` fixture with `default: Notebooks`. The `access` boot installs it personal and household, writes a file through the subfolder, stops the app, swaps the subfolder for `~/.ssh` (personal) or a shared secret folder (household), and asserts the next start fails with the reason, the row is `failed`, no container runs, and the secret folder got no file. It restores the folder, starts again, asserts the container's source is a `/run/moose/folder-pins/` pin whose device and inode match the folder, and proves `moose-folder-pins.service` restores a detached pin.

## How it maps to the specs

- `APP_ISOLATION.md` # Folder source pins replaces the old "Known hole, #604" paragraph.
- `BRAIN_HOST_PROTOCOL.md` # Folder source pins and # Boot restore document the op and the restorer.
- `APP_LIFECYCLE.md` lists `compose.pins.yml` and `folder-pins.json` in the instance layout and says the pins file merges after the override.
- `HEALTH.md` # Version adds `folder-pin-unsupported`.
- `BOOT.md` adds the restorer to the boot chain and explains why it runs before Docker.
- `DECISIONS.md` 2026-11-11 records the inode-pin decision and the rejected alternatives.

## Known gaps & deviations

- `minimumAgentVersion` stays at `0.4.0`. The release that ships the new host-agent bumps it. Until then an older agent keeps the fallback and the warning.
- An uninstall leaves its pins mounted until the next reboot or the restorer's boot prune. The mount holds the source directory's inode, so a folder the user deletes after an uninstall keeps its contents on disk until then. The restorer prunes every pin no record references.
- The fake host-agent returns the real path as the pin, so the unit tests exercise the brain's plumbing and the refusal, not a real mount. The cloud lane and the `usermgrtest` lane cover the real mount.
- A rollback that cannot re-pin the old generation reports the update's generic "could not put the old version back" and logs the folder reason. A plain Start shows the reason directly.
