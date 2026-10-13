# The fit is said on the app's page only, under Information, with Install disabled

- **Status:** done
- **Date:** 2026-10-13
- **Specs touched:** `APP_STORE.md` # Apps this box cannot run, `APP_ISOLATION.md` # Resource limits, `APP_MANIFEST.md` # B2, `BRAIN_UI_PROTOCOL.md` (catalog routes, # install-plan), `DECISIONS.md` 2026-10-13, `docs/architecture.md`

This follows [app-fit-check.md](app-fit-check.md) (#599), after the maintainer tried it on a running box. Three UI changes were asked for: no fit warning in the store, the notice in the right column under Information, and the Install button kept but disabled for an app too big for the box.

## What was done

- **No badge on store cards.** `StoreAppCard` no longer shows "Too big for this box" or "May not fit right now". The landing page already says there are no badges on it (`APP_STORE.md` # How the box store draws it). `fitBadge` is gone from `fit.ts`.
- **The store lists no longer carry `fit`.** With no badge, nothing reads it, so `Server.storeCatalog` no longer wraps the catalog in `WithFit` and a list request no longer reads host-agent or the running apps' manifests. The detail route and the install plan keep `fit`. `Catalog.WithFit` still fills every shape; only the detail route uses it.
- **The notice sits under Information.** On the App page, `FitNotice` moved from under the header to the right column, after the Information rows (Version, Category, Size, Memory, CPU, License) and before the links. It is next to the Memory and CPU rows it is about.
- **Install stays, disabled, for a too-big app.** On the App page and on a pack page, the Install button is shown and disabled when the plan says `unavailable: "too-big"`. The pack page shows no sentence for it, so the reason is said on the App page only. `needs-remap` is unchanged: its sentence still replaces the button. `useStartInstall.goInstall` now starts nothing when the plan has any `unavailable` code, as a guard for any other caller.
- **Tests.** `TestCatalogCarriesFit` now checks that the list routes carry no `fit` and still list a too-big app, and that the detail and the plan agree. `TestCatalogFitUnknown` checks the detail instead of the list.

## How it maps to the specs

- `APP_STORE.md` # Apps this box cannot run: the store lists show a too-big app as usual with no badge; the app page says it, and its Install is disabled; a pack page disables Install with no sentence.
- `APP_ISOLATION.md` # Resource limits and `APP_MANIFEST.md` # B2: the fit is shown on the app's page and in the install plan, not on the store card.
- `BRAIN_UI_PROTOCOL.md`: only the detail route's `Entry` carries `fit`; the list routes do not read the box.
- `DECISIONS.md` 2026-10-13 (#599) is reworded in place to drop the store card. The entry is from the same day and nothing was released between, so it is corrected rather than followed by a new entry.

## Known gaps & deviations

- **The issue's "store cards show the same state" no longer holds**, by the maintainer's choice after trying it.
- **`Entry.Fit` stays in the API schema** for the list routes, always absent there. Removing it would split `Entry` into two types for no gain.
- **Not looked at in a browser by me.** The maintainer is testing it on the dev stack; typecheck and the production build pass.

## What's next

Nothing new. The items in [app-fit-check.md](app-fit-check.md) # What's next stand.
