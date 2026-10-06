# moose terminal apps

> **Draft design, not built.** Working spec for running terminal-based apps (AI coding agents such as Claude Code, Codex CLI, Grok CLI and OpenCode) as moose apps that open in a browser tab. Captures a design discussion held on 2026-10-06. Nothing here is built yet, and the decisions below are not yet in `DECISIONS.md`. Companion to `APP_MANIFEST.md` (folders, needs), `STORAGE.md` (use-case folders), `DASHBOARD.md` (install flow, Integrations), `SERVICE_PROVISIONING.md` (per-user accounts), `ENVIRONMENT.md` (hosted forward-auth, per-app access mode) and `APP_ISOLATION.md` (the sandbox these apps run in).

## Goal

A user can install a terminal app (for example Claude Code) from the catalog, open it in a new browser tab, pick a project folder, and work with the agent in a real terminal. The agent works on the user's own files, can use git with the user's own accounts, and keeps running when the tab closes.

**Scope for now: the hosted profile only.** The appliance is out of scope. On the appliance there is no forward-auth in front of apps, so an app at `<slug>.local` is reachable by anyone on the LAN. A terminal app there would be an open shell. That must be solved before terminal apps ship on the appliance.

## What this is not

This is **not** a host shell. `SETTINGS.md` says moose ships no web terminal, and `NEXT.md` # Web terminal in the dashboard keeps a root-capable host shell as a separate, unsolved design. A terminal app runs inside its own app container, with the normal sandbox (`cap_drop: [ALL]`, userns remap, runs as a UID, no host access). It sees only the folders the user gave it. So it does not conflict with "SSH is rescue-only".

## Options considered

| Option | What it is | Verdict |
|---|---|---|
| A | Each app is a catalog app whose image runs ttyd + tmux + the CLI. | **Chosen, as a shared base image** (below). |
| B | A manifest field (`terminal: { command: ... }`) and moose injects the wrapper. | Not now. The base image is B's design delivered as an image. Move to B only if we need something an image cannot do. |
| C | Use each tool's own web UI. | **Chosen where an official one exists** (OpenCode: `opencode web`). Community web UIs for Codex or Claude (codexapp, codex-pocket, CloudCLI) are not used: third-party, change often, ours to maintain. |
| D | A web terminal in the dashboard (host shell). | Rejected for this. Root on the host, conflicts with "SSH is rescue-only", needs the unsolved root-PTY design. |

**The rule:** an app with a good official web UI uses it. Every other CLI app uses the `moose-terminal` base image.

What each tool offers today (October 2026):

- **OpenCode:** `opencode web`, a full web UI (file tree, diffs, sessions), with a password option (`OPENCODE_SERVER_PASSWORD`). MIT.
- **Codex CLI:** `codex app-server` speaks JSON-RPC over WebSocket. Only community web UIs sit on top of it. Apache-2.0. Uses the base image.
- **Claude Code:** no official local web UI. "Remote Control" (`claude --remote`) drives a session from claude.ai through Anthropic's servers, which is not what we want here. Proprietary license (see # Images). Uses the base image.
- **Grok:** the common `grok-cli` is a community tool (MIT); xAI's own is "Grok Build". No web UI. Uses the base image.

## Folders: slots, chosen by the user

The fixed list of six use-case folders (`photos`, `music`, `movies`, `documents`, `notes`, `downloads`, enforced in `internal/manifest/manifest.go`) becomes a **starting list, not a limit**. This changes how `permissions.folders` works for every app, not just terminal apps.

- **The manifest declares slots, not folders.** A slot is a named need (`workspace`, `photos`, `notes`). Each slot has a *suggested* folder, a mode (`read` or `write`), and whether the app needs it to run.
- **The user decides where each slot points**, at install: one of the default folders, a subfolder of one, or a brand new folder they name on the spot.
- **"Required" means "point this at some folder"**, not "this exact folder". Immich needs a photos folder to work, so its slot is required, but the user picks which folder, and an empty new one is fine. An optional slot can be skipped.
- **The app sees a stable path and env var per slot**: `/moose/<slot>` and `MOOSE_FOLDER_<SLOT>`, as today. Only the host source changes. The brain already resolves the source at install (personal or shared, plus a subfolder), so this extends the existing mechanism.
- **Extra folders.** A manifest flag (working name `extra_folders: true`) lets the user add any number of further folders. They mount at `/moose/extra/<FolderName>`. Terminal apps and file managers use this, so the user can give an agent their folder of Markdown notes. Apps like Immich do not.
- **Back-compat.** Existing manifests keep working: the six current `folder:` values become slots with that folder as the suggestion (two-major back-compat rule, `APP_MANIFEST.md` # Format).
- **Changing folders after install: yes, in v1** (decided). A minimal "add or remove a folder" control on the app's detail page, pulled forward from the deferred per-app permissions screen (`DASHBOARD.md`, `NEXT.md` Tier 3). It recreates the container, which ends running terminal sessions, and the UI must warn about that.

### The safety limit (decided)

The user can point a slot at any folder, within these rules:

- Inside the user's own home or the household shared tree (`/srv/moose/shared/`).
- No hidden folders (`~/.ssh`, `~/.config`, any path part starting with `.`).
- No symlink that leads out of the tree.
- Never another user's home, never `/var/lib/moose`.

Today host-agent's `prepare-folder` requires the path to start with a use-case folder (`BRAIN_HOST_PROTOCOL.md` # prepare-folder). That rule becomes the list above. The limit protects the user from an app (an agent with `~/.ssh` has the user's keys), not from themselves.

### The `Projects` folder (decided)

- A new suggested folder, `Projects/`, the default for terminal apps' `workspace` slot.
- It is created **the first time an app asks for it**, not at user creation for everyone. Most users never code, and an empty folder in their home is noise.
- Needs a `STORAGE.md` change.

## Git integration (decided)

A new section **Settings → Integrations → Git**, next to AI services and Email, following the same pattern: a grid of providers, each asking only what it needs.

- **GitHub:**
  - **Connect with GitHub** (recommended): device flow (the user types a short code at github.com/login/device) through a **moose GitHub App**, so the user chooses which repos moose can reach. Device flow needs no callback URL and no client secret on the box.
  - **Use a token:** paste a fine-grained personal access token, with short steps for making one.
- **GitLab:** a token first (GitLab.com or self-hosted, server URL prefilled and editable). OAuth later.
- **Codeberg, Gitea, Forgejo:** server URL plus a token.
- **Other:** server URL plus username and token, for any HTTPS git host.
- Each account carries the commit name and email (editable).

**Ownership.** An account belongs to the user who added it, exactly like email accounts (`SERVICE_PROVISIONING.md` # BYO outgoing mail). Not shared with other users. Each account lists the apps that use it. Revoke in one place and every app loses it.

**How an app gets it.**

- The manifest declares a git need (working name `git: true`), like it declares AI or mail needs. The install flow shows a git step listing the user's saved accounts.
- An app can bind several accounts, one per host (GitHub and GitLab both).
- The brain gives the container a read-only credentials file, a git config that points git's credential helper at it, `GH_TOKEN` for the `gh` CLI, and `user.name` / `user.email`. So `git push` and `gh pr create` work with no login in the terminal.
- **Token refresh.** GitHub App user tokens expire after 8 hours and come with a refresh token. The brain refreshes them and rewrites the credentials file while the app runs. Git's credential helper reads the file on each use, so no restart is needed.
- **At rest** tokens are plaintext in SQLite, like email passwords today. Accepted for v1; covered by `NEXT.md` # App-secret injection hardening.

**Risk, stated plainly.** An agent holding the token can push anything the token allows. Choosing repos per GitHub App account is the real limit.

## Why git (and other tools) live in the image, not the OS

- A container's filesystem is its image. It cannot see host programs unless they are bind-mounted.
- Bind-mounting the host's git does not work in practice: git needs the host's shared libraries (glibc, libcurl, OpenSSL, PCRE), so you would mount half of the host's `/usr/lib` into the app. That ends the isolation, couples apps to the host's versions, and A/B OS updates would change tools under running apps. `DECISIONS.md` turned down the same idea for the Docker CLI in the brain (option c, "the brain image would couple to the host's Docker version").
- It is not wasteful. Docker stores a shared layer once. Every CLI app builds `FROM moose-terminal`, so git, ripgrep and Node are stored and downloaded once per box (git plus its libraries is roughly 20 to 40 MB), however many terminal apps are installed. Apps that do not need git never carry it.

## The `moose-terminal` base image

One image that every CLI app builds on. A CLI app is then a small Dockerfile: `FROM moose-terminal`, install the CLI, set the command.

**Contents:**

- ttyd (serves xterm.js and connects it to a command over WebSocket), tmux (sessions survive a closed tab).
- git, `gh`, ripgrep, curl, Node, Python.
- `mise`, so the agent or user can install other languages (Go, Rust, ...) into the persisted home without root. There is no `apt`: no root and `cap_drop: [ALL]`.
- The project page and start script (below).

### Opening the app (decided)

Opening the app opens **a new tab at `<slug>.<box>/`**. That page is a **simple HTML page served by the base image**, not the dashboard. It runs next to tmux, so it can see running sessions.

**The project page lists:**

- One level of the `workspace` slot: each subfolder is a project.
- Each extra folder as its own entry.
- Per row: name, when last used, a dot if a session is running, and a "stop session" button.

**Actions:** New folder, and Clone (repos from the bound git account, or a pasted URL). No delete or rename: Files does that, and the agent page is the wrong place to lose a folder.

**URL contract:**

| Path | What it does |
|---|---|
| `/` | The project page. |
| `/p/<slot>/<path>` | The terminal, in that folder, attached to that folder's tmux session. |
| `/clone?repo=<url>&into=<slot>` | Clones inside the app with its injected credentials, then opens the new folder. |

The start script checks every path itself: it must be inside a mounted slot, with no `..` and no symlink that leads out. The URL is never trusted.

**Sessions:**

- One tmux session per folder. Two tabs on the same project show the same session.
- Closing a tab leaves the agent running. Reopening the URL reattaches. Project URLs can be bookmarked.
- A cap on sessions running at once. Each agent is a Node process using roughly 200 to 400 MB, and a box can be an old laptop. The number is not set yet.

**The terminal page:** a "Back to projects" link, and a key bar for Esc, Ctrl, Tab and arrows on phones.

**Later:** "Open with <app>" on folders in the dashboard's Files page (`FILES.md`), as a plain link to `<slug>.<box>/p/<slot>/<path>`.

## The agent CLIs

- **Logging in to the AI (decided: both).** If the user binds an account from the existing **AI services** integration at install, the brain injects the key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, ...) and no login is needed. Without one, the first run shows the CLI's own login, which covers subscriptions (Claude Pro/Max, ChatGPT plans). The AI step is optional.
- **The CLI's home folder** (`~/.claude`, `~/.codex`: logins, settings, history) lives in the instance state dir. It survives restarts and is deleted on uninstall. Project files never live there.
- **Updates (decided: catalog releases).** Self-update is turned off. New CLI versions ship as catalog releases, so versions are pinned and update notes show in the dashboard. This needs an automatic rebuild pipeline in `onmoose/store`, because Claude Code releases about weekly.
- **Previewing a dev server** the agent starts (`npm run dev` on port 3000): needed, parked for v2. It needs per-port routes (for example `3000-claude.<box>`) through Caddy and forward-auth.

## Security and access

- **Personal-only.** Terminal apps cannot be installed as household instances: git ownership (a repo and the agent must have the same owner, or git refuses with "dubious ownership") and per-person credentials both need it. A personal instance runs as the owner's UID.
- **v1: access locked to "Only me".** On hosted, a public terminal app is an open shell on the internet with the user's API key and git token. The manifest needs a flag that locks the access mode, and the brain must refuse to change it (not only hide the switch).
- **v2: sharing a session.** The user wants to share a session on purpose. Planned shape: a share link with a unique, unguessable URL that does not contain the project name, scoped to one session, and revocable. Not designed yet.
- **CI proof.** The hosted `access` boot proves ttyd's WebSocket works through forward-auth and that the forward-auth cookie is still stripped.

## Images: public or private

**Recommendation: public**, on the same reasoning as the brain and UI images (`BUILD.md` # 6): there is nothing secret in them, every secret is per-box and injected at runtime, and a private registry would put a pull credential on every box.

- **`moose-terminal` base image:** public. Only open-source tools (ttyd MIT, tmux ISC, git GPL-2.0 from Debian packages with their sources available).
- **Codex, OpenCode, Grok CLI images:** public. Their licenses (Apache-2.0, MIT, MIT) allow redistribution.
- **Claude Code: open legal question.** Its license is "© Anthropic PBC. All rights reserved. Use is subject to Anthropic's Commercial Terms of Service." Baking it into a public image we publish is redistribution, which those terms may not allow. A private image does not fix that: we would still be distributing it to every box. A likely way out: the Claude Code app image contains only the base image plus a pinned version number, and the box installs that exact version from npm on first start into the persisted home. The version stays pinned by the catalog (so "updates are catalog releases" still holds), and the user's box downloads it from Anthropic directly. Needs a check of Anthropic's terms before we choose.

## Open questions

1. **Where the `moose-terminal` image is built and published.** `onmoose/store` (with the catalog apps) or its own repo. Unknown whether store builds images today.
2. **Claude Code redistribution** (above).
3. **Session cap** number, and what the page shows when it is hit.
4. **Manifest field names**: slots, `extra_folders`, the git need, the locked-access flag. All are working names.
5. **Ownership of the moose GitHub App**: the account, its name, where its client id lives.
6. **Appliance**: forward-auth (or another login) in front of apps before terminal apps ship there.

## Planned work (proposed order)

1. Spec PR: folder slots, user-chosen folders and the safety limit, `Projects`, folders changeable after install (`APP_MANIFEST.md`, `STORAGE.md`, `DASHBOARD.md`, `BRAIN_HOST_PROTOCOL.md`, `DECISIONS.md`).
2. Git integration (`SERVICE_PROVISIONING.md`, `DASHBOARD.md`, `APP_MANIFEST.md`).
3. `moose-terminal` base image: ttyd, tmux, tools, project page, start script, key bar.
4. Locked access mode for terminal apps, and personal-only.
5. First apps: Claude Code on the base image, OpenCode on its own web UI.
6. Later: "Open with" in Files, dev-server previews, session sharing.

## Sources

- ttyd: https://github.com/tsl0922/ttyd
- Web terminal comparison: https://www.pistack.xyz/posts/self-hosted-terminal-sharing-tmate-ttyd-wetty-2026/
- OpenCode web: https://opencode.ai/docs/web/
- Codex app server: https://developers.openai.com/codex/app-server.md
- Claude Code Remote Control: https://simonwillison.net/2026/Feb/25/claude-code-remote-control/
- Claude Code license status: https://fast.io/resources/claude-code-open-source-status-guide/
- grok-cli: https://www.superagent.sh/open-source/grok-cli
- Zellij web client (an alternative to ttyd + tmux): https://zellij.dev/documentation/web-client
