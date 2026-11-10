# moose terminal apps

> **Draft design, not built.** Working spec for running terminal-based apps (AI coding agents such as Claude Code, Codex CLI, Grok CLI and OpenCode) as moose apps that open in a browser tab. Captures a design discussion held on 2026-10-06, with its open questions answered on 2026-11-10 (`DECISIONS.md` 2026-11-10). Nothing here is built yet. The folder part is specced in the docs that own it (#655). Companion to `APP_MANIFEST.md` (folders, needs), `STORAGE.md` (use-case folders), `DASHBOARD.md` (install flow, Integrations), `SERVICE_PROVISIONING.md` (per-user accounts), `ENVIRONMENT.md` (hosted forward-auth, per-app access mode) and `APP_ISOLATION.md` (the sandbox these apps run in).

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

## Folders, chosen by the user

Specced in #655 for every app, not only terminal apps. The owners are `APP_MANIFEST.md` # `folders` (the manifest), `APP_ISOLATION.md` # User content (the limit), `STORAGE.md` (`Projects`, folders made through moose) and `DASHBOARD.md` (# Folder chooser, # Changing folders after install). In short:

- **The manifest names the folder, the user chooses where it points.** `folders:` keeps its `- folder:` entries. The name is what the app sees (`/moose/projects`, `MOOSE_FOLDER_PROJECTS`), and the folder of the same name is the suggestion. `default:` suggests a subfolder, and `optional: true` lets the user skip it. `scope` is gone.
- **"Required" means "point this at some folder"**, not "this exact folder". An empty new folder is fine.
- **Extra folders.** `extra_folders: write` lets the user add any number of further folders, at `/moose/extra/<Name>`. Terminal apps declare it, so the user can give an agent their folder of Markdown notes.
- **The limit.** A personal folder is in the user's own home, never the home itself, under a use-case folder or a folder made through moose, with no dot level and no symlink. moose never makes a folder named `bin` or starting with a dot. The limit protects the user from an app (an agent with `~/.ssh` has the user's keys, and on hosted the owner's shell is root), not from themselves.
- **`Projects`** is the seventh folder name, the suggestion for terminal apps, made the first time an app asks for it.
- **Changing folders after install** is on the app's settings page. It recreates the container, which ends running terminal sessions, and the page warns about that.

A terminal app's manifest:

```yaml
permissions:
  folders:
    - folder: projects
      mode: write
  extra_folders: write
```

## Git integration (after v1)

**Not in v1** (`DECISIONS.md` 2026-11-10). The agent works on the box, so it is useful with local git alone: commits, branches and history all work. A user who wants to push runs `gh auth login` in the terminal. `gh` shows its own device code, and keeps the login in the app's home folder, which survives restarts. `git` and `gh` are in the base image for this. What the integration would add is one place to manage accounts and a faster setup. The sketch below is kept for when it is built. If it is, the `onmoose` org owns the moose GitHub App, and its public client id ships in the brain.

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

One image that every CLI app builds on. It is built and published in `onmoose/app-images`, with the CLI images on top of it (`DECISIONS.md` 2026-11-10). That repo already probes every image under moose's sandbox, signs it, and lets Renovate bump it weekly, which a CLI that releases every week needs. Two of its rules change first: a folder may hold our own source (the project page and the start script), and an upstream may be an npm package or a binary release, not only a git commit. A CLI app is then a small Dockerfile: `FROM moose-terminal`, install the CLI, set the command.

**Contents:**

- ttyd (serves xterm.js and connects it to a command over WebSocket), tmux (sessions survive a closed tab).
- git, `gh`, ripgrep, curl, Node, Python.
- `mise`, so the agent or user can install other languages (Go, Rust, ...) into the persisted home without root. There is no `apt`: no root and `cap_drop: [ALL]`.
- The project page and start script (below).

### Opening the app (decided)

Opening the app opens **a new tab at `<slug>.<box>/`**. That page is a **simple HTML page served by the base image**, not the dashboard. It runs next to tmux, so it can see running sessions.

**The project page lists:**

- One level of the `projects` folder: each subfolder is a project.
- Each extra folder as its own entry.
- Per row: name, when last used, a dot if a session is running, and a "stop session" button.

**Actions:** New folder, and Clone (a pasted URL; a private repo uses the login the user made in the terminal with `gh auth login`). No delete or rename: Files does that, and the agent page is the wrong place to lose a folder.

**URL contract:**

| Path | What it does |
|---|---|
| `/` | The project page. |
| `/p/<folder>/<path>` | The terminal, in that folder, attached to that folder's tmux session. |
| `/clone?repo=<url>&into=<folder>` | Clones inside the app with the user's own terminal login, then opens the new folder. |

The start script checks every path itself: it must be inside a mounted folder (`/moose/projects` or `/moose/extra/<Name>`), with no `..` and no symlink that leads out. The URL is never trusted.

**Sessions:**

- One tmux session per folder. Two tabs on the same project show the same session.
- Closing a tab leaves the agent running. Reopening the URL reattaches. Project URLs can be bookmarked.
- **No cap on sessions in v1.** Each agent uses roughly 200 to 400 MB, but sessions run only when the user starts one, the page has a stop button, and the box already warns on RAM pressure (#597). A later change can refuse a new session when `MemAvailable` in `/proc/meminfo` is low; it needs no brain work.

**The terminal page:** a "Back to projects" link, and a key bar for Esc, Ctrl, Tab and arrows on phones.

**Later:** "Open with <app>" on folders in the dashboard's Files page (`FILES.md`), as a plain link to `<slug>.<box>/p/<folder>/<path>`.

## The agent CLIs

- **Logging in to the AI (decided: both).** If the user binds an account from the existing **AI services** integration at install, the brain injects the key (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, ...) and no login is needed. Without one, the first run shows the CLI's own login, which covers subscriptions (Claude Pro/Max, ChatGPT plans). The AI step is optional.
- **The CLI's home folder** (`~/.claude`, `~/.codex`: logins, settings, history) lives in the instance state dir. It survives restarts and is deleted on uninstall. Project files never live there.
- **Updates (decided: catalog releases).** Self-update is turned off. New CLI versions ship as catalog releases, so versions are pinned and update notes show in the dashboard. `onmoose/app-images`'s weekly Renovate bumps rebuild the images, and the catalog picks up the new digest.
- **Previewing a dev server** the agent starts (`npm run dev` on port 3000): needed, parked for v2. It needs per-port routes (for example `3000-claude.<box>`) through Caddy and forward-auth.

## Security and access

- **Personal-only.** Terminal apps cannot be installed as household instances: git ownership (a repo and the agent must have the same owner, or git refuses with "dubious ownership") and per-person credentials both need it. A personal instance runs as the owner's UID.
- **Hosted only.** The brain refuses a terminal app on the appliance (# Open questions, 6).
- **v1: access locked to "Only me".** On hosted, a public terminal app is an open shell on the internet with the user's API key and git token. The manifest needs a flag that locks the access mode, and the brain must refuse to change it (not only hide the switch).
- **v2: sharing a session.** The user wants to share a session on purpose. Planned shape: a share link with a unique, unguessable URL that does not contain the project name, scoped to one session, and revocable. Not designed yet.
- **CI proof.** The hosted `access` boot proves ttyd's WebSocket works through forward-auth and that the forward-auth cookie is still stripped.

## Images: public or private

**Recommendation: public**, on the same reasoning as the brain and UI images (`BUILD.md` # 6): there is nothing secret in them, every secret is per-box and injected at runtime, and a private registry would put a pull credential on every box.

- **`moose-terminal` base image:** public. Only open-source tools (ttyd MIT, tmux ISC, git GPL-2.0 from Debian packages with their sources available).
- **Codex, OpenCode, Grok CLI images:** public. Their licenses (Apache-2.0, MIT, MIT) allow redistribution.
- **Claude Code: installed on the box, not baked in** (decided 2026-11-10). Its license is "© Anthropic PBC. All rights reserved." Anthropic's legal page allows "preinstalling or running Claude Code in your products or services (e.g. in hosted sandboxes or other agent infrastructure)" under its Commercial Terms, which moose accepts for the hosted service, on these conditions: the binary is not modified, no built-in login method is removed or disabled, each user signs in with their own API key or Claude plan, and we never pay for or resell their usage. Both logins above qualify. The Claude Code app image holds only the base image and a pinned version number. On first start the box installs that exact version with Anthropic's native installer (`install.sh` with the version; npm is deprecated) into the persisted home, with `DISABLE_UPDATES=1`, which Anthropic documents for distributors who ship their own versions. So no image we publish contains Anthropic's binary, the catalog still pins the version, and the user's box downloads it from Anthropic. We may say the app "runs Claude Code"; using Anthropic's logo as the catalog icon needs their written permission, so the icon is plain until then.

## Open questions

Answered on 2026-11-10 (`DECISIONS.md` 2026-11-10):

1. **Where the image is built:** `onmoose/app-images` (# The `moose-terminal` base image).
2. **Claude Code redistribution:** installed on the box at first start, under Anthropic's Commercial Terms (# Images).
3. **Session cap:** none in v1 (# Sessions).
4. **Manifest field names:** the folder keys are in `APP_MANIFEST.md` # `folders` (#655). The locked access mode, personal-only and hosted-only keys are named in their own PR (# Planned work, item 4).
5. **The moose GitHub App:** owned by the `onmoose` org, if the Git integration is built after v1.

Still open:

6. **Appliance:** a login (forward-auth or another) in front of apps, before terminal apps can ship there. Until then the brain refuses them on the appliance.

## Planned work (proposed order)

1. **Spec: folders the user chooses** (#655, this change). Code, as separate issues in this order:
   1. #657: the manifest and admission changes (`optional`, `extra_folders`, `projects`, `scope` ignored), the new folder elections on install, and the wider `prepare-folder` check.
   2. #658: the folder chooser: host-agent's folder list op, `GET /api/v1/me/folders`, the component, and the install's folder step.
   3. #659: changing folders after install: `PUT /api/v1/apps/{id}/folders` and the app page's Folders section.
2. **Fix #604** (a folder source swapped for a symlink between two starts). It must land before item 5.
3. **`moose-terminal` base image** in `onmoose/app-images` (onmoose/app-images#46): the two rule changes there first, then ttyd, tmux, tools, the project page, the start script, the key bar.
4. **Locked access mode, personal-only, hosted-only** for terminal apps (#660) (manifest keys, brain refusals, the store hiding them where they cannot run, the `access` boot proving the WebSocket through forward-auth).
5. **First apps:** Claude Code on the base image, OpenCode on its own image (`ghcr.io/anomalyco/opencode`, `opencode web`). Catalog app issues in `onmoose/store`.
6. **Later:** the Git integration, "Open with" in Files, dev-server previews, session sharing, a free-memory check before a new session, the appliance.

## Sources

- ttyd: https://github.com/tsl0922/ttyd
- Web terminal comparison: https://www.pistack.xyz/posts/self-hosted-terminal-sharing-tmate-ttyd-wetty-2026/
- OpenCode web: https://opencode.ai/docs/web/
- Codex app server: https://developers.openai.com/codex/app-server.md
- Claude Code Remote Control: https://simonwillison.net/2026/Feb/25/claude-code-remote-control/
- Claude Code license status: https://fast.io/resources/claude-code-open-source-status-guide/
- grok-cli: https://www.superagent.sh/open-source/grok-cli
- Zellij web client (an alternative to ttyd + tmux): https://zellij.dev/documentation/web-client
