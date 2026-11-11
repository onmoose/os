# Terminal apps: draft design

- **Status:** in progress (design only)
- **Date:** 2026-10-06
- **Specs touched:** `docs/specs/TERMINAL_APPS.md` (new, draft), `docs/specs/NEXT.md`, `docs/README.md`

A design discussion about running terminal apps (AI coding agents such as Claude Code, Codex CLI, Grok CLI and OpenCode) on moose, opened in a browser tab. No code changed. This entry records where the discussion stopped so a later session can pick it up.

## What was done

- Researched web terminal options (ttyd, GoTTY, WeTTY, zellij web) and the browser paths each agent offers.
- Wrote `TERMINAL_APPS.md` as a draft spec with every decision made so far: hosted only, native web UI where official (OpenCode) and a shared `moose-terminal` base image (ttyd + tmux) for the rest, folder slots the user points anywhere within a safety limit, a `Projects` folder created on first use, folders changeable after install, a Git integration with several providers, a project page served by the base image in a new tab, AI login by bound key or by the CLI's own login, CLI updates as catalog releases, personal-only, access locked to "Only me" in v1.
- Added the draft to the spec map and an entry to `NEXT.md`.

## Known gaps & deviations

- None of the decisions are in `DECISIONS.md` yet. They go there with the spec PRs that make them real.
- The folder slot model changes a locked part of `APP_MANIFEST.md` for every app, not only terminal apps.

## What's next

- Answer the open questions in `TERMINAL_APPS.md` # Open questions, starting with the Claude Code redistribution question and where the base image is built.
- Open the issues in the order of `TERMINAL_APPS.md` # Planned work.
