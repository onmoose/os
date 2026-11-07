# The self-review is a Sonnet agent with a brief in code-review.md

- **Status:** done
- **Date:** 2026-11-07
- **Specs touched:** none. Dev docs: `docs/dev/code-review.md` (new # The review agent), `docs/dev/contributing.md` (# Step 7, the checklist), `CLAUDE.md` (# Developing, # Working style).

Follows [app-update-boot.md](app-update-boot.md) (#643). Step 7 of `contributing.md` and `CLAUDE.md` said to run the self-review with `/code-review low`, and said that this starts a fresh Sonnet agent with no conversation history. Neither is true. The skill runs as a fork of the session, so it carries the whole conversation and runs on the session's model, whatever model is asked for. At low effort it also skips tests, fixtures and markdown. On #643 (https://github.com/onmoose/os/pull/643), a PR of test-lane code, fixtures, CI and docs, it made two tool calls and reported nothing. A Sonnet agent started by hand with a brief then found a Block: the night's cut-off could come before host-agent's first tick in the window, which would have turned about a third of the runs red. The green run of #643 shows it: its window opened at 17:02 and the update came at 17:13, after the old window's cut-off (the `app-update` job log of https://github.com/onmoose/os/actions/runs/42392801634).

## What was done

- **`code-review.md` # The review agent.** What the self-review agent must be (a new agent with a fresh context, on Sonnet), and the brief to start it with, filled in with the PR number and the progress entry. The brief reads `code-review.md` first and follows it, reads the existing review comments, reviews the whole diff with tests, fixtures, CI and docs included, checks the diff's claims against the code, and checks the docs rules.
- **`contributing.md` # Step 7 and the checklist, and `CLAUDE.md`,** point at that section, and say not to use the `/code-review` skill, and why.
- **The brief lives in the docs, not in a tool's config** (such as a `.claude/agents/` file), so every contributor and every tool reads the same rule (`CLAUDE.md` # Documentation discipline).

## Known gaps

- Older progress entries say `/code-review low` was the self-review. They are frozen and stay as written.

## What's next

- Nothing.
