# Record the userns-remap capability, the ledger and the authoring guide, #545

- **Status:** done
- **Date:** 2026-09-30
- **Specs touched:** `CAPABILITIES.md` # The append-only discipline, `APP_ISOLATION.md` # What this does not cover

This follows [userns-remap-close.md](userns-remap-close.md), which closed the remap work (#523), and runs next to [hide-remap-apps.md](hide-remap-apps.md) (#544). The remap shipped with two manifest fields, `root_setup` and `image_user`, but the docs that app authors and the store's re-screen read had not caught up. No code changes here.

## What was done

- **`docs/dev/capabilities.yml`**: a new id, `userns-remap` (`since: "0.x"`, `ref` #523, `DECISIONS.md` 2026-09-29, `APP_ISOLATION.md` # User-namespace tiers). The summary names `root_setup` (caps tier) and `image_user` (image tier), says folder apps are not covered, and says it applies only to boxes built with the remap on (after #530), since an existing box never gets it turned on. `version` goes from 1 to 2. The `service-user` summary no longer calls the remap "unshipped"; only its text changed, not its id. This is the edit that fires the store's re-screen for every record that waits on `userns-remap` (plunk records `unblock.os_capabilities: [userns-remap]`).
- **`docs/specs/CAPABILITIES.md`**: the partial-closure example said the remap was unshipped. It now says it shipped later under its own id, and that the gap-class tag itself is still not listed, because folder apps are not covered.
- **`docs/dev/catalog-import-gaps.md`**: every entry that waited on the remap has a dated update line. No `### ` heading changed.
  - poznote: `implemented` for poznote, caps tier (`root_setup: true`). The gap-class stays partly open for folder apps.
  - formbricks: `implemented`, caps tier for the whole app as packaged (its bundled `pgvector` Postgres chowns as root). Still blocked on SpiceDB, which is not this gap.
  - plunk: `implemented`, image tier (`image_user: true`). A package still needs `internet: true` and required SES fields.
  - nextcloud (both entries): the remap shipped, but nextcloud is a folder app, so they stay `open`.
  - penpot: the remap shipped, and `root_setup` could replace a wrapper for a folderless app, but only on a remapped box; penpot keeps its wrapper, which runs on every box. Stays `open`.
  - postiz was already `resolved`, and jotty and appsmith do not wait on the remap, so they are unchanged.
  - Each changed status line keeps the old status word after "Before that it was:", so the history stays readable.
- **`docs/dev/authoring-apps-with-an-agent.md`**: a new "Which tier" section: `root_setup` for a root entrypoint that chowns or drops privileges, `image_user` for an image that must run as its own baked `USER`, both folderless only, never with `service_user`, `folders`, `gpu`, `devices` or each other, and no compose `user:` with `image_user`. It says plainly that `make dev-app` cannot boot such an app, because the inner loop has no remap, and that the boot must happen on a box with the remap on, per the store's remote curation box doc. The prompt gains a step 3 bullet for these images, step 10(c) says where to boot them, and the DO NOT list no longer reads as "never any capability" and warns against taking the dev refusal as a verdict.
- **`docs/specs/APP_ISOLATION.md`** # What this does not cover said "Until the remap ships, such images stay curation-rejects". It now says a folder app of this class stays a reject, and a folderless one can be packaged now for a box built with the remap on.
- `NEXT.md` and `APP_MANIFEST.md` were checked: neither says these apps cannot be packaged, so neither changed.

## How it maps to the specs

- `CAPABILITIES.md` # The append-only discipline: one id appended, none removed or renamed, `version` bumped, and the ledger entries that name the shipped facet say which id it maps to (`userns-remap`).

## Verification

- `capabilities.yml` parses as YAML, with `version: 2` and the eight ids in order.
- `git diff` shows no changed `### ` line in the ledger.
- `make check`: green.

## Known gaps & deviations

- **The remote curation box doc is on the store side.** The guide points at it by name only. If it does not yet say how to boot a remapped app, that is a store-side change.
- **The dev loop still cannot boot these apps.** A later change could let the fake host-agent report a remap range the developer set up by hand.

## What's next

1. The store's re-screen picks up the records that wait on `userns-remap` (plunk, and poznote and formbricks if their records name it).
