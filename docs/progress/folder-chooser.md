# Folders the user chooses: the folder chooser

- **Status:** done
- **Date:** 2026-11-11
- **Specs touched:** `docs/specs/BRAIN_UI_PROTOCOL.md`, `docs/specs/BRAIN_HOST_PROTOCOL.md`, `docs/specs/DASHBOARD.md`, `docs/specs/INSTALL_STEPS.md`, `docs/specs/STORAGE.md`

Closes #658. Follows [folder-choice-install.md](folder-choice-install.md), which built the manifest and install half of folders the user chooses (#657) and left two gaps this entry closes: the dashboard still sent the old form, and a top-level folder the user made by hand was recorded as "made through moose" when an install asked for it with `new: true`.

## What was done

- **host-agent lists folders** (`GET /v1/users/{username}/folders?path=`). The real op (`usermgr.listUnder`) opens each level relative to the one before with `O_NOFOLLOW`, as `prepare-folder` does, and keeps only real directories with no leading dot, sorted, at most 1000 (`truncated`). A missing level is `404 not-found`, a file or a symlink on the way is `409 not-a-directory`, a bad path is `400 bad-path`. The fake lists the dev operator's home with `internal/folderlist`, a new small package that the brain also uses for the shared tree.
- **`prepare-folder` says what it made.** The answer is `{ "made_top": bool }`: true only when this call made the first level of the path. The real walk reports a first level it found (even one made by something else a moment before) as false. The fake checks with `Lstat` before it makes the path.
- **`GET /api/v1/me/folders?source=personal|shared&path=`** (`internal/api/folders.go`). The caller's own home through host-agent, with no username parameter. The top level is the seven use-case folders (`missing: true` when not there) and the folders made through moose that are still there (`made_by_moose: true`); a hand-made folder, a row whose folder is gone, dot folders and files are not offered. A path whose first level is outside that set is a `422` on `path`. The shared tree is listed by the brain itself. Not audited.
- **A new folder may not take a name already in the home** (issue item 4). When a personal election or extra folder asks for `new: true`, the install lists the top of the owner's home first and refuses a name that is there with a `422` on that folder's row ("a folder named "scripts" is already in your home, and moose can only use a new folder it made itself"). In the job, a new folder whose first level host-agent did not make (`made_top: false`) fails the install with `lifecycle.ErrFolderAlreadyThere` and writes no `made_folders` row. Two folders of one install under the same new name are fine: the first one made it.
- **The dashboard.** A new `FolderChooser` component: one level at a time, Back, New folder (with name checks), Choose this folder, a "New" mark on a missing use-case folder, and Your folders / Shared folders buttons for an extra folder on a personal app. The install's folder step (`FolderChoices`) shows each folder as "Notes / Obsidian" with Change, "Don't use a folder" as a choice for an optional folder, and More folders with Add a folder and Remove for `extra_folders`. It sends `path`, `new`, `skip` and `config.extra_folders`. A `422` on `config.folders.<folder>` or `config.extra_folders[<i>]` shows under that row. The step shows for an app that declares only `extra_folders`. The tab's draft keeps the choices.
- **Tests.** `folderlist` (filters, symlinks, a missing root, the 1000 cut); `usermgr` (`made_top` on a new, a second and a hand-made folder; `listUnder` with dot names, files and symlinks at two levels); the host-agent handler (error mapping, a bad path never reaches the user manager, `made_top` sent); lifecycle (a new folder found there fails with no row; two folders under one new name write one row); the API (`/me/folders` top filter, a deeper level, 422/404/409, the shared top with no tree; a `new: true` on a hand-made name is a 422 on the folder row and on an extra folder row, with no row written). The API test harness's host-agent mock lists a real temp tree with `folderlist`.

## How it maps to the specs

- `BRAIN_UI_PROTOCOL.md` # Folder chooser and # Folder elections after #655: built, with an as-built note; "taken" now includes a hand-made folder.
- `BRAIN_HOST_PROTOCOL.md` # Listing folders for the folder chooser: built; a new paragraph for `made_top`.
- `DASHBOARD.md` # Folder chooser and `INSTALL_STEPS.md` # 5b: built, with an as-built note on where the chooser opens and the source buttons for extra folders.
- `STORAGE.md` # Folders made through moose: the as-built paragraph says "taken" is now checked against the home.

## Known gaps & deviations

- **Tried in a browser on `make dev`.** With a copy of `files-demo` that also has an optional `notes` folder and `extra_folders: read`, a fresh state dir and `HOME` set to a scratch folder for the fake host-agent: Documents pointed at the `Taxes` subfolder chosen in the chooser, and `Recipes` made with New folder as an extra folder. The install finished, the override bound `Documents/Taxes`, `Notes/Demo` and `Recipes`, and `made_folders` held `Recipes`. Not run against the real host-agent on a box; the real walk is covered by its Linux tests.
- **A host-agent from before #658** has neither the listing op nor `made_top`. On such a box the chooser cannot list the home (Change shows the error), and an install that asks for a new folder fails with a 502 at the listing; one that skips the listing check would still be refused in the job, because `{}` reads as `made_top: false`. Installs that keep the suggestions work as before. `minimumAgentVersion` is not raised: the brain still works with an older host-agent for everything else. The control-plane release that carries this should go out with, or after, the OS release that carries the new host-agent.
- **An extra folder may come from the shared tree on a personal app.** The chooser offers Your folders and Shared folders for it, because an extra folder has no source radios of its own. The brain allowed this already (#657). The spec did not say how the source is picked; written into `INSTALL_STEPS.md` # 5b.
- **The brain's shared listing is not race-free** the way host-agent's `O_NOFOLLOW` walk is. It checks each level with `Lstat` inside an `os.Root`, so it never reads outside the shared tree; a level swapped for a symlink in between could at worst show the names of another folder inside the tree. Only names are read.
- **The chooser lists Shared as a choice only for extra folders.** A declared folder's source is still picked with the radios above it, as before; Change then opens the chooser in that tree.

## What's next

1. #659, changing folders after install (`PUT /api/v1/apps/{id}/folders`), which reuses the chooser and `FolderChoices`.
2. #604, before the first terminal app. No `dev` to `main` release before it lands.
