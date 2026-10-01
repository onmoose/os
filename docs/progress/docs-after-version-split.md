# Docs that went stale after the version split

- **Status:** done
- **Date:** 2026-10-01
- **Specs touched:** `BRAIN_HOST_PROTOCOL.md`, `CLAUDE.md`, `docs/progress/README.md`

A docs-only sweep after [os-package-lock.md](os-package-lock.md) (#560), which closed the second slice of #486. It fixes three statements that #559 and #560 made wrong. Each sits in a doc the next slices (#561, #563) read first.

## What was done

- **`CLAUDE.md`.** The host-agent bullet listed apt as "not wired yet"; apt is no longer planned, so it now names the A/B OS update (#486) instead. The cloud-image paragraph gains one sentence on the two release lines from #559: dispatch takes `publish_os` and `publish_control_plane` to publish one line, and nothing published is overwritten.
- **`BRAIN_HOST_PROTOCOL.md` # Versioning** was "lockstep with OS release: brain version N talks to host-agent version N". After #559 the two are on separate lines, so the section now describes what holds: the brain's `minimumAgentVersion` floor, with no negotiation. The scope list no longer names apt as a host-agent job, and the boundary reads "the OS slots" instead of "apt".
- **`docs/progress/README.md` # Up next** gains the A/B OS update as item 1, with #561 as the next slice.

## How it maps to the specs

`DECISIONS.md` 2026-10-01 (the version split, stream A as an A/B image). No decision changes.

## Known gaps & deviations

- The "designed, not built (#486)" sections in `UPDATES.md`, `BUILD.md` # 1b, `ENVIRONMENT.md` and `STORAGE.md` are left as they are on purpose: each slice flips its own part to "as built".

## What's next

1. #561, the hosted image in the A/B layout.
