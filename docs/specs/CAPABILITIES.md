# moose Capabilities Manifest

> The machine-readable list of platform capabilities moose has shipped, so catalog curation stops depending on someone remembering. Sibling to the human gap ledger [`../dev/catalog-import-gaps.md`](../dev/catalog-import-gaps.md) (the prose this graduates from) and [`APP_STORE.md`](APP_STORE.md) (the store this keeps fresh). The manifest itself is [`../dev/capabilities.yml`](../dev/capabilities.yml).

## Why this exists

An app is `blocked` or `degraded` in the store *because moose lacks a feature* — user-namespace remap, a per-app operator-config surface, managed MongoDB, a headless-tool category. When that feature ships, the app can graduate. Today the only thing that connects "moose shipped capability X" to "these apps were waiting on X" is a human remembering to grep the gap ledger. The ledger's own instructions say as much: flip an entry to `implemented` "so the next person can grep this ledger for every app that was waiting on that gap and revisit them." That is a manual, forgettable step.

This manifest makes the connection mechanical. It is a versioned list of the capabilities moose has shipped, each keyed by the gap-class it closes. A curation record that names the gap-classes it waits on can then be cross-referenced against this list automatically: the moment every gap-class an app waits on appears here, the app surfaces on a re-screen list. The signal is a data lookup, not a memory.

## What it is

`../dev/capabilities.yml`: a top-level `version` and a list of `capabilities`, each with:

- **`id`** — the capability. **The id is the gap-class tag** from `catalog-import-gaps.md`, reused verbatim (`operator-env-config`, `managed-mysql`, `service-user`, …). Reusing the gap-class means the manifest, the ledger, and a curation record's blocker reference all speak one vocabulary with no separate taxonomy to keep in sync.
- **`since`** — the brain version that introduced it, always a quoted string (`"1.2.0"`, not `1.2.0`) — an unquoted value parses as a YAML float or integer, not a string. No brain version is cut yet (`RELEASE_MANIFEST.md`), so pre-1.0 this is the placeholder `"0.x"` and `ref` carries the real provenance; backfill the semver once releases are tagged.
- **`ref`** — the PR / `DECISIONS.md` date / spec section that shipped it, for traceability.
- **`summary`** — one line on what the capability is.

`version` is bumped whenever the list changes. It is a monotonic content version, not a schema version: a consumer stamps the `version` it validated against, so staleness (a downstream that validated against an older capabilities set than the one now in the tree) is visible.

## The append-only discipline

A shipped capability does not un-ship. **Append; never remove or renumber an id.** When a `catalog-import-gaps.md` entry flips to `implemented` or `resolved`, add its gap-class to `capabilities.yml` in the same change and bump `version`. That single edit is what fires the re-screen signal for every app waiting on the gap — so it is not optional bookkeeping, it is the mechanism.

One nuance the ledger already carries: a gap-class can be *partially* closed. `nonroot-data-ownership` shipped `service-user` (an app that adopts the runtime `user:` is unblocked) but the hardcoded-internal-UID facet still waits on the separate, unshipped `userns-remap`. List only the capability that actually shipped (`service-user`), under its own id — never the broader unclosed gap-class. A curation record waiting on the unshipped facet references the unshipped id (`userns-remap`), so it correctly stays blocked.

A partial closure's id is, by definition, **not** the gap-class tag verbatim — it is coined for the shipped facet. That breaks the single-vocabulary guarantee unless the bridge is written down: the `catalog-import-gaps.md` entry that names the shipped facet must state which `capabilities.yml` id it maps to (e.g. the `nonroot-data-ownership — poznote` entry names `service-user` as the mechanism moose now ships). Without that bridge, someone starting from the ledger's gap-class tag has no way to find the narrower id in the manifest. Coining a sub-capability id with no ledger cross-reference is a doc bug, not a valid partial closure.

## Who consumes it

The catalog curation records live outside this repo; a re-screen cross-check reads this manifest and those records and reports every app whose blockers are now all shipped. This repo owns only the manifest and the discipline that keeps it honest — publishing the capability, not consuming it. Keep the reference one-directional: this file names capabilities in OS terms; it does not know or name its consumers.
