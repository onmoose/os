# Folders the user chooses: changing them after install

- **Status:** done
- **Date:** 2026-11-12
- **Specs touched:** `docs/specs/BRAIN_UI_PROTOCOL.md`, `docs/specs/DASHBOARD.md`, `docs/specs/LOGGING.md`, `docs/specs/TERMINAL_APPS.md`

Closes #659. Follows [folder-chooser.md](folder-chooser.md) (#658), which built the folder chooser and the install's folder step, and [folder-source-pins.md](folder-source-pins.md) (#604), which pins every folder source by inode before each compose up. This entry builds the last code issue of `TERMINAL_APPS.md` # Planned work item 1: the user can change where an installed app's folders point.

## What was done

- **`GET /api/v1/apps/{id}/folders`** (`internal/api/appfolders.go`). App-control gate. Each declared folder in the install plan's shape plus its current `source`, `path` or `skipped`, the extra folders with their `/moose/extra/<Name>` names, the manifest's `extra_folders` mode, and `can_change` (the `PUT` gate). An app with no folders, and a Door-2 app, answers empty lists.
- **`PUT /api/v1/apps/{id}/folders`**. The gate of an update's yes (`approverOK`): the owner for a personal app, an admin for a household app; an admin on another person's personal app gets `403`, audited, and a member on another person's app `404`. A full replace in the install's shape, checked by the same `resolveElections`, with the same `422` locations. A new folder lists the top of the owner's home first, as at install. `409` (audited) for an app with no folders, a Door-2 app, or an app that is installing, updating or being removed. `204` and no job when nothing changes. Otherwise a job, `app-folders`, audited as `app.folders_change` with `changes: [{folder, old, new}]`.
- **Lifecycle** (`internal/lifecycle/folders.go`). `InstanceFolders` reads the current choice back from the override (`folderBinds`), relative to the owner's home or the shared tree. `SetFolders`, under the app's lock: prepares only the sources the app does not bind yet (`prepareFolderSources`, so a new top-level folder gets its `made_folders` row and a folder made by hand in between fails with `ErrFolderAlreadyThere`), rewrites every service's folder volumes and the shared group in the override, rewrites `.env` (`MOOSE_FOLDER_<NAME>` and `MOOSE_FOLDER_EXTRA` follow the new set), and for a running app recreates it and waits for it to be healthy. The recreate goes through `composeUpInstance`, so the new folders are pinned by inode (#604). A pin refusal is returned with its plain words. Brain commits first: a failed compose up leaves the app marked pending-recreate, as for a config edit. A stopped or failed app gets the new folders at its next start.
- **Kept as they are:** a top-level folder the app binds now counts as allowed in the check, so a folder chosen under an older rule can stay. An extra folder keeps its `/moose/extra/<Name>` when another is removed; a new one takes a free name.
- **Dashboard** (`web-ui/src/components/AppFolders.vue`). A Folders section on the app's page, above Logs, for the owner or an admin. It reuses the install's `FolderChoices` (source radios, path with Change and the folder chooser, Don't use a folder, Add a folder and Remove), warns "Saving restarts <App>. Anything running in it stops.", and has Save folders and Cancel. A 422 shows on its row; a failed job under the button. Read-only list for a caller who cannot change them. Activity labels the record "App folders changed".
- **Tests.** API: the Done when (GET, a PUT that moves projects, turns notes on and adds a new extra folder, the override, `.env`, the `made_folders` row and the audit record; the same body again is 204 with no record), the gate (admin 403 audited, member on a household app 403, member on another person's app 404), the refusals (409 audited, two 422s not audited, override unchanged), keeping a current hand-made top, and `keepExtraNames`. Lifecycle: a running app gets the binds, the shared group and `.env`, is recreated once, and its pins record names the new folder; a second identical set does nothing; dropping the shared folder drops the group; a stopped app is not started; a pin refusal at the recreate. The API harness's host-agent mock gained `home`, `prepare-folder` and the well-known ids.

## How it maps to the specs

- `BRAIN_UI_PROTOCOL.md`: the `GET`/`PUT /api/v1/apps/{id}/folders` paragraph is now "built in #659", with an as-built paragraph for the shapes, the gates and the job.
- `DASHBOARD.md` # Changing folders after install: built, with an as-built bullet.
- `LOGGING.md` `app.folders_change`: built.
- `TERMINAL_APPS.md` # Planned work item 1.3: done.

## Known gaps & deviations

- **Tried in a browser on `make dev`.** With a copy of `files-demo` that also has an optional `notes` folder (`default: Demo`, write) and `extra_folders: read`, a scratch `HOME` and a fresh state dir: installed with notes skipped, then on the app's page turned notes on and added a new folder named `scripts`, which was already in the home. The save showed the 422 under that row. Removed it, added a new `Recipes`, saved: the container restarted with `/moose/notes` (rw) and `/moose/extra/Recipes` (ro), `compose.pins.yml` named them, and Activity showed "App folders changed". The browser test found one bug, fixed before the PR: the section counted as changed before its first load, so the paths showed empty. Not run against the real host-agent on a box, and no cloud boot was run for this change.
- **The spec's "Choose a folder" button for a skipped folder** is the "Don't use a folder" radio pair of the install step instead, so the two screens read the same. Written into `DASHBOARD.md`.
- **A Door-2 app gets no Folders section and a `409` on `PUT`.** The spec does not say. Its folders are grants with a target the admin typed, which this request shape cannot express.
- **A 422 is not audited.** `LOGGING.md` lists 403, 409 and host-agent failures, and CLAUDE.md says validation 422s do not audit. The install path does audit its 422s; that was left as it is.
- **An app that does not come up healthy after the change keeps the new folders.** The job fails and says to check the logs. The folders are the user's choice, and the old ones are one more save away.
- **`make check` fails on this machine in 73 update tests,** all at the pre-update snapshot step, because the disk is 97% full and the snapshot refuses past its threshold. They do not touch this change; CI runs them.

## What's next

1. #660 (locked access, personal-only, hosted-only) and onmoose/app-images#46 (the `moose-terminal` base image).
2. The Claude Code and OpenCode catalog app issues in `onmoose/store`.
