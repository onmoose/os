# The fit is said on the app's page only, in a yellow alert, with Install disabled

- **Status:** done
- **Date:** 2026-10-14
- **Specs touched:** `APP_STORE.md` # Apps this box cannot run, `APP_ISOLATION.md` # Resource limits, `APP_MANIFEST.md` # B2, `BRAIN_UI_PROTOCOL.md` (catalog routes, # install-plan), `DECISIONS.md` 2026-10-13, `docs/architecture.md`

This follows [app-fit-check.md](app-fit-check.md) (#599). The maintainer tried it on the dev box and changed how the fit is shown. The changes below came out of that testing, plus a refactor the new layout made possible.

## What was done

- **No badge on store cards.** `StoreAppCard` no longer shows "Too big for this box" or "May not fit right now". The landing page already has no badges (`APP_STORE.md` # How the box store draws it).
- **A yellow alert above the price panel.** On the App page the fit notice is its own block at the top of the right column, above the price panel, with the column's normal spacing. It is a Tailwind UI "alert with description" in yellow (`WarningAlert.vue`, used by `FitNotice.vue`). Too big reads "This app can't run on this box as it is." as the title and "It requires 64.0 GB of memory, and this box only has 7.6 GB." under it; CPU reads "It requires 16 cores, and this box only has 8 cores.". Tight reads "This app may not fit next to the apps that are running.", then the free memory and the running apps in one line. The Upgrade link stays for a hosted owner. The install pages use the same alert.
- **The brain's refusal says the same words** (`lifecycle.TooBigError`), so a forced install fails with the page's text.
- **Install stays, disabled, for a too-big app**, on the App page and on a pack page. A pack page shows no sentence for it: each app installs on its own, and the reason is said on the App page only. `needs-remap` is unchanged.
- **A disk warning on the App page.** When the plan's footprint reaches 90% of the free space (`spaceTight`, the install pages' rule), the App page shows the same yellow alert, "This might not fit." with "It needs about X, and only about Y is free on your box. You can still install.". A warning only; Install stays enabled. The install pages keep their older wording.
- **Refactor.** With no fit in the lists, `Catalog.WithFit` walked every list shape for one caller. It is gone (`fitview.go` and its test). The detail handler computes the fit from `Detail.Recommended()`, and `Fit` moved from `Entry` to `Detail`, so the list routes have no always-empty field and do not read the box. The getter is on `Detail`, not `Entry`, because huma cannot build the schema link of a struct that embeds a type with methods. `useStartInstall` returns `tooBig`, which the App page and the pack row each worked out on their own.
- **Tests.** `TestCatalogCarriesFit` checks the detail and the plan agree and a too-big app is still listed. `TestRecommendedFromRecord` checks the record's `resources` reaches `Detail.Recommended`. The lifecycle tests pin the exact refusal text for memory and for CPU.

## How it maps to the specs

- `APP_STORE.md` # Apps this box cannot run: no badge in the lists; on the App page the Memory and CPU rows, the yellow alert above the price panel, the disabled Install, and the disk warning; a pack page disables Install with no sentence.
- `APP_ISOLATION.md` # Resource limits: the alert titles and the refusal text.
- `BRAIN_UI_PROTOCOL.md`: `fit` is a `Detail` field, absent on `Entry`.
- `DECISIONS.md` 2026-10-13 (#599) is reworded in place to drop the store card. It is from the day before and nothing was released between, so it is corrected rather than followed by a new entry.

## Known gaps & deviations

- **The issue's "store cards show the same state" no longer holds**, by the maintainer's choice after trying it.
- **The install pages' disk warning keeps its older wording and style.** Moving it to the shared alert was offered and not asked for.
- **The tight wording has not been seen on a box with real running apps.** Only the too-big alert was tried on the dev box.
- **One commit in this PR does not build alone**: the UI commit uses `tooBig` from `useStartInstall`, which the next commit adds. The branch as a whole builds.

## What's next

Nothing new. The items in [app-fit-check.md](app-fit-check.md) # What's next stand.
