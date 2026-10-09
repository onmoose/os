# Draw the store landing from the catalog's sections

- **Status:** done
- **Date:** 2026-10-09
- **Specs touched:** `docs/specs/APP_STORE.md`, `docs/specs/BRAIN_UI_PROTOCOL.md`, `docs/specs/NEXT.md`

The box snapshot now carries the authored landing page as typed sections (`home.sections`) and pack `keywords` (onmoose/store#203). It already carried `packs`, which the box ignored. Until now the box store drew the older landing: a spotlight banner and category groups. This change moves the box store to the sectioned landing that the website's store already draws (#591).

## What was done

- **The wire models the new keys.** `internal/catalog/wire.go` gains `wireSection`, `wireSlide`, `wirePack`, `wireHomePage.Sections` and `catalogFile.Packs`. The pinned fixture was refreshed with `make catalog-fixture OS=../os` from the store branch of #203. `TestNoUnmodeledFields` now also checks the keys inside `home`, each section, each slide, each group and each pack, so a key added there later is caught too. It was checked by adding a fake key to a slide and a pack: the test failed on both.
- **The brain resolves the page** (`remote.go`). A slide whose app is missing, an app missing from a group, and a pack id that names no pack drop out of their slot. A pack with any missing app drops whole. An empty slide list, group or section drops whole, and so does a section of a type the box does not know. A search section always stays.
- **The remap filter** (`remapfilter.go`) drops an app that needs the remap from slides and groups, and a pack holding one from the pack list and every section. Such a pack's route returns 404 and search does not find it.
- **API.** `GET /api/v1/catalog/home` adds `sections`, with apps as full entries and packs as full pack records. `GET /api/v1/catalog/pack?id=` returns one pack, or 404. `GET /api/v1/catalog/search` now returns `{apps, packs}`: the apps of matching packs first, then the apps that match on their own text, each once, then the packs. A pack matches when the query starts a word of its title or a keyword, or holds a whole keyword, after both are normalized; a query under two characters matches no pack. This is the other store surface's rule.
- **Art goes through the brain.** Slide and pack illustrations are served by `GET /api/v1/catalog/illustration?key=`, where the key is a hash of the published URL, through the same 24-hour asset cache as icons. Only art the snapshot names can be fetched.
- **web-ui.** `StoreView` draws the sections in catalog order: the search section (a large search box and suggestion chips that fill it), `StoreDiscover` (hero and side slides, scroll-snap, dots, arrow keys), `StorePackCard` grids for intents and packs, and the categories through `packRows`. A search or category view (`StoreResults`) shows under the search box while the other sections hide, so the input is never remounted while typing. With no sections, the older landing is unchanged. The description line under "Store" is gone. The new pack page (`/store/packs/:id`, `StorePackView`) lists the pack's apps; each row (`StorePackApp`) has its own Install, Open, or the "cannot install here" sentence.
- **One install entry.** The logic behind the detail page's Install button (start at once for an app with no steps, otherwise open the install pages) moved from `AppDetailView` into `useStartInstall` in `useInstall.ts`, and the pack page rows use the same composable. The detail page behaves as before.
- **Docs.** `APP_STORE.md` # Landing page is rewritten (what the snapshot carries, what the box does, how the box store draws it); # Apps this box cannot run and # Locked decisions follow. `BRAIN_UI_PROTOCOL.md` lists the store routes. `NEXT.md` Tier 4 gains the open question of when the store may stop publishing the older spotlight and groups. `docs/architecture.md` and `docs/dev/web-ui.md` updated.

## Tests

- `internal/catalog/sections_test.go`: resolving (missing ids, a pack with a missing app, empty slide lists, groups and sections, an unknown type, labels, brain-side art URLs), the older landing with no sections, the art proxy by key and a 404 for an unknown key, the remap drop in sections, packs and the pack route, and pack search (prefix, a whole keyword in a longer query, a word inside a word, one character, apps-first order with no repeat).
- `internal/api/catalog_sections_test.go`: `/catalog/home` sections and art served through the brain, the pack route and its 404, the pack route and landing on a box with no remap, and search returning packs. The auth test covers the two new routes.
- Existing search tests follow the new return type.
- **Looked at in `make dev`** against the store's local catalog (`docker compose -f dev/docker-compose.yml up` in `../store`, on the #203 branch, 65 apps, 10 packs), with `MOOSE_CATALOG_URL=http://localhost:8090`, in headless Chrome at 1280 and 390 pixels wide. The five sections draw in order; the discover hero and side slides each have one Install; the intent and starter pack cards show their art through the brain's illustration route; the categories pack into rows; the "photo backup" chip fills the box and shows two apps under "Apps" and one pack under "Packs"; the pack page `/store/packs/family-cloud` lists its three apps, each with Install. At 390 pixels the page has no sideways scroll. No install was run from the pack page in this session; the button runs the same `useStartInstall` as the detail page.

## How it maps to the specs

`APP_STORE.md` # Landing page is the contract: the five section types, packs as a promise of all their apps, the drop rules, the art route, search order, and the pack page that installs one app at a time.

## Known gaps & deviations

- **The pack route is `/api/v1/catalog/pack?id=`, not `/api/v1/catalog/packs/{id}`.** Go's `net/http` mux refuses to register `/catalog/packs/{id}` next to `/catalog/{id}/install-plan`: both match `/catalog/packs/install-plan` and neither is more specific, so the brain would panic at start. The category route takes `?name=` for the same reason, and the art route takes `?key=`. The UI path is `/store/packs/:id` as planned.
- **The search box stays where the search section is, but a search or category view always shows under it.** If a catalog placed the search section after other sections, those sections hide during a search and the results still show under the search box.
- **The box searches every pack the snapshot carries,** not only the packs on the landing. The other store surface searches only the packs its page shows. Today every published pack is on the page.
- **App search does not match category labels,** only ids, as before. The other surface matches both.
- **A pack page with many apps fetches one install plan per app,** for its Install button. Packs hold a few apps.
- **An app whose id is `packs` cannot have its install page reached by URL,** because `/store/packs/install` opens the pack page. No catalog app has that id.
