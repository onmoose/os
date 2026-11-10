# Folders the user chooses: spec

- **Status:** done (spec only)
- **Date:** 2026-11-10
- **Specs touched:** `docs/specs/APP_MANIFEST.md`, `docs/specs/APP_ISOLATION.md`, `docs/specs/STORAGE.md`, `docs/specs/DASHBOARD.md`, `docs/specs/INSTALL_STEPS.md`, `docs/specs/BRAIN_UI_PROTOCOL.md`, `docs/specs/BRAIN_HOST_PROTOCOL.md`, `docs/specs/UPDATES.md`, `docs/specs/APP_LIFECYCLE.md`, `docs/specs/LOGGING.md`, `docs/specs/USERS_AND_GROUPS.md`, `docs/specs/DECISIONS.md`, `docs/specs/NEXT.md`, `docs/specs/TERMINAL_APPS.md`

Closes #655. Follows [terminal-apps-design.md](terminal-apps-design.md), which left the folder model and six open questions in the draft `TERMINAL_APPS.md`. This entry answers those questions and specs item 1 of its planned work: folders the user chooses, for every app. No code changed.

## What was done

- **The manifest names the folder, the user chooses where it points** (`APP_MANIFEST.md` # `folders`). `folders:` keeps its `- folder:` entries (a map keyed by folder was weighed and turned down, so every existing manifest keeps its meaning). There are seven names: the six use-case folders and `projects`. `default:` is the suggested subfolder on any entry, `optional: true` lets the user skip a folder, and `scope` is gone (still parsed, does nothing). `extra_folders: read | write` lets the user add more folders, at `/moose/extra/<Name>`, with `MOOSE_FOLDER_EXTRA` naming their parent. The app still sees `/moose/<folder>` and `MOOSE_FOLDER_<NAME>`.
- **The folder limit** (`APP_ISOLATION.md` # User content). A personal folder is in the owner's own home, never the home itself, under a use-case folder or a folder made through moose, with no dot level and no symlinked level. moose never makes a folder named `bin` or starting with a dot: Debian's `~/.profile` puts `~/bin` on `PATH`, and on hosted the owner's shell is root (`DECISIONS.md` 2026-10-11). The `APP_ISOLATION.md` invariant from #603 is rewritten to this limit, which is the "own decision first" it asked for.
- **`Projects` and folders made through moose** (`STORAGE.md`). `Projects` is made the first time an app asks for it. The brain keeps a row for each top-level folder it made for a user, and only those, the use-case folders and folders inside them can be given to an app.
- **The folder chooser** (`DASHBOARD.md` # Folder chooser, `INSTALL_STEPS.md` # 5b). One small component, folders only, one level at a time, with New folder and Choose this folder. It reads `GET /api/v1/me/folders`, which reads a new narrow host-agent op, `GET /v1/users/{username}/folders` (`BRAIN_HOST_PROTOCOL.md`). A new folder is made by `prepare-folder`, whose host-side check widens to the parts of the limit host-agent can see.
- **Changing folders after install** (`DASHBOARD.md` # Changing folders after install). A Folders section on the app's settings page, saved with `PUT /api/v1/apps/{id}/folders` (a job that recreates the containers, audited as `app.folders_change`). Pulled forward from the deferred permissions screen; taking a grant away stays deferred (`NEXT.md`). Refused for an app with no folders, because its tier is fixed.
- **Updates.** A new required folder in an update is bound with its suggestion, a new optional one is left off, and a chosen folder never moves (`UPDATES.md`, `APP_LIFECYCLE.md`).
- **Terminal apps' open questions answered** (`TERMINAL_APPS.md`, `DECISIONS.md` 2026-11-10): the images are built in `onmoose/app-images`; Claude Code is installed on the box at first start with Anthropic's native installer, under Anthropic's Commercial Terms, never baked into a published image; no session cap in v1; the Git integration moves after v1 (`gh auth login` in the terminal covers v1); hosted only.

## How it maps to the specs

- Realizes "Files are first-class, apps are windows" further: the user, not the app author, decides which of their folders an app sees.
- Keeps the owner-UID invariant (`APP_ISOLATION.md`) true under a wider bind surface, and keeps host-agent's home access a narrow named op (`CONTROL_PLANE.md` # Locked: host-agent hardening directives).
- Keeps the manifest's two-major back-compat: an old manifest means the same thing. A newer manifest on an older brain is covered in `APP_MANIFEST.md` # `folders` (Older brains reading a newer manifest).

## Known gaps & deviations

- **Spec only.** Nothing is built. Every new section is marked "planned".
- **#604 is still open,** and the wider bind surface makes it worse. It is item 2 of `TERMINAL_APPS.md` # Planned work and must land before the first terminal app.
- **The first-level rule lives in the brain only.** host-agent cannot see the brain's rows, so a brain bug could ask it to prepare a hand-made top-level folder. host-agent still refuses the home itself, dot levels, `bin` and symlinks, which are the cases that reach the owner's shell.
- **An older brain refuses `projects`,** and a `default` without `scope: pick-subfolder`. The catalog has to keep `scope: pick-subfolder` next to `default`, and hold back an app that uses `projects`, until every box reads the new form. Nothing enforces this; it is a rule for catalog authors.
- **The old doc example `default: Notes/Obsidian` did not match the code,** which joins `default` under the folder (`internal/lifecycle/lifecycle.go` `hostSource`). The spec now says `default` is relative to the folder (`default: Obsidian`). The `BRAIN_UI_PROTOCOL.md` example `subfolder_default` is changed to match. Whether any store manifest wrote the folder name into `default` was not checked.

- **Changing folders asks for no password again,** like install, which also widens what an app reaches without one. The review agent asked whether it belongs under the 5-minute re-prompt for destructive operations (`USERS_AND_GROUPS.md`). Kept as is: it moves no files and deletes nothing.
- **Nothing lints the back-compat rule for catalog authors.** A `moose manifest` check that warns on `default` without `scope: pick-subfolder` would catch it; it goes in the first code issue.

## What's next

1. The three code issues of `TERMINAL_APPS.md` # Planned work item 1, in order: #657 (manifest, admission and install), #658 (the folder chooser), #659 (changing folders after install).
2. #604.
3. The `moose-terminal` base image, onmoose/app-images#46, starting with that repo's two rule changes.
4. Locked access mode, personal-only and hosted-only for terminal apps, #660.
5. The first apps, Claude Code and OpenCode, as catalog app issues in `onmoose/store`.
