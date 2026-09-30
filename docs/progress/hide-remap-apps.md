# Hide the apps a box cannot run: root_setup and image_user without the remap, #544

- **Status:** done
- **Date:** 2026-09-30
- **Specs touched:** `APP_STORE.md` (new # Apps this box cannot run, # Landing page, # Locked decisions), `APP_ISOLATION.md` # User-namespace tiers, `BRAIN_UI_PROTOCOL.md` # Catalog and # install-plan, `DECISIONS.md` 2026-09-30, `docs/architecture.md`, `docs/dev/web-ui.md`

This follows [userns-remap-close.md](userns-remap-close.md), which closed the remap work (#523). Two manifest fields need a remapped Docker daemon: `root_setup` (caps tier) and `image_user` (image tier). On a box with no remap the brain refuses such an install, but the app still showed in the store, so a user could pick it and only learn at the end that it cannot be installed. A box built before #530 has no remap, and neither has the native dev loop.

## What was done

- **The remap state, cached** (`internal/lifecycle/remapstate.go`). `Manager.RemapState(ctx)` returns `RemapOff`, `RemapOn` or `RemapUnknown`. It makes the install path's own check (`hostIdentity`): host-agent's `remap_base` and Docker's `name=userns` must agree. A failed read or a disagreement is unknown. A known answer is kept for ten minutes, an unknown one for thirty seconds, so a store request does not call host-agent and `docker info` each time. One read runs at a time, and the lock is not held during it: while a refresh is in flight other callers get the last answer at once, and only before the first answer do they wait, stopping when their own request goes away. The read ignores the caller's cancel (with a five-second limit of its own), so one request that goes away cannot leave an unknown in the cache. The install path does not use the cache: it still reads the remap on every install.
- **The catalog view** (`internal/catalog/remapfilter.go`). `Catalog.WithoutRemapApps()` wraps the source and drops the apps that need the remap from `List` and so from `Home`, `Category` and `Search`, plus the featured row, the spotlight and the home groups. An emptied group is dropped, and a category left with no app loses its pill, because `Home` derives the pills from `List`. Every by-id lookup (`Entry`, `Detail`, `Load`, the asset paths) passes through, so a direct link still loads. `Entry` has an unexported `needsRemap`, set from the browse record or, for the disk source, from the manifest.
- **The browse record** (`internal/catalog/wire.go`). `wireApp` gains two optional booleans, `root_setup` and `image_user`, copied from the manifest. `dev/mkcatalog` writes them for a seeded snapshot.
- **The API** (`internal/api/api.go`). `storeCatalog(ctx)` returns the filtered view only when `RemapState` is a known `RemapOff`. The four list routes use it. The detail route and the install plan keep the full catalog.
- **The install plan** (`internal/api/install_plan.go`). A new optional `unavailable` field, a reason code: `needs-remap` when the manifest sets `root_setup` or `image_user` and the box is a known `RemapOff`. It reads the manifest itself, so it holds even when the browse record does not carry the two fields. It is a code, not a sentence, because `BRAIN_UI_PROTOCOL.md` # install-plan says the UI owns all wording. The install API is not changed: it refuses as before.
- **The UI.** `unavailableText(plan)` in `web-ui/src/installSteps.ts` gives the sentence: "This app can't be installed on this box, because it needs a safety feature that only boxes set up with a newer version of moose have." It names no mechanism. The detail page shows it under the header in place of the Install button. The install pages (a direct link to `/store/:id/install`) show it with only a Back button, so no step is asked. No new screen and no badge.
- OpenAPI and the TS client regenerated.

### Why "unknown" shows every app

Hiding on a guess would take apps away from a box that may be remapped. When the state is unknown the install path already refuses every install with its own plain message (`ErrRemapMismatch`, or the failed read), so showing the apps costs nothing that is not already true. So only a known "no remap" hides anything. This is written in `APP_STORE.md` # Apps this box cannot run.

## How it maps to the specs

- `APP_STORE.md` # Apps this box cannot run is new and describes all of the above in box terms. The locked decision "Environment filtering is server-side" now says the one thing the box hides itself, and `DECISIONS.md` 2026-09-30 says why: the remap is a fact about one box, not about a surface.
- `APP_ISOLATION.md` # User-namespace tiers: the "No remap, no caps tier" rule now says the store hides these apps too.
- `BRAIN_UI_PROTOCOL.md`: the list routes' filter, and `unavailable` on the install plan.

## Tests

- `internal/lifecycle/remapstate_test.go`: the six states (no remap, remap on both sides, both disagreements, each failed read), the cache (five calls, one `docker info`; a known answer held until it runs out; an unknown one read again after its shorter wait), a cancelled caller still gets `RemapOff` (through a Docker fake that fails on a done context), a caller during a held refresh gets the last answer at once, and before the first answer a caller waits, or gets unknown if it goes away. Run with `-race`.
- `internal/lifecycle/remapstate_live_test.go` (build tag `dockerlive`): the real CLI against this machine's Docker (no remap) and a host with no range, the dev loop's shape. It gave `RemapOff`: "daemon remap=false, RemapState=1".
- `internal/catalog/remapfilter_test.go`: through a synced remote source, a `root_setup` app (featured, the spotlight, the only app of one group) and an `image_user` app (the only app of one category) are gone from `List`, the spotlight, the featured row, the groups, the category pills, `Category` and `Search`, while the unfiltered catalog still has them, and `Detail` and `Entry` still resolve. The disk source reads the two fields from the manifest.
- `internal/api/catalog_remap_test.go`: over HTTP, with a lifecycle manager whose host and Docker report no remap, the two apps are missing from `/catalog`, `/catalog/search`, `/catalog/home` (the `mail` pill is gone) and `/catalog/category` (`mail` is 404), the detail route answers 200, and the install plan carries `needs-remap` for both and nothing for a plain app. With the remap on, and with an unreadable Docker, every app shows and no plan carries it.
- The refusal of the install itself is covered by the existing `TestInstallRootSetupRefusedWithoutRemap` and the `image_user` equivalent in `imageuser_test.go`, and by `TestUsernsNeverHostWithCaps`. Nothing here changes it.

## Verification

- `make check`: green, the full suite with the PAM package.
- `make check-web`: green.
- The UI was checked through the types and the build, not with screenshots.

## Known gaps & deviations

- **The list hiding needs the catalog publisher.** The published browse record does not carry `root_setup` or `image_user` today: I read the live `GET /catalog?env=hosted` on 2026-09-30, and no record has either key, and no published manifest sets either field yet. So until the publisher adds the two booleans to each browse record, the lists hide nothing, and only the install plan (which reads the manifest) says an app cannot be installed. That half works for every app now. The box-side half has to ship first anyway, since a field is delivered when the box models it (`APP_STORE.md` # What the box models). It is a change on the publishing side, outside this repo.
- **The pinned fixture does not have the two keys.** `internal/catalog/testdata/snapshot.json` is written by the publisher from its own wire types (`contributing.md` # Changing the published catalog shape), and the publisher does not emit them yet, so I did not hand-edit it. `TestNoUnmodeledFields` still passes, because it only fails on a fixture key the box does not model. Refresh the fixture from the publisher's output once it sends them.
- **A cached "off" for up to ten minutes.** A box's remap is fixed when its image is built, so this only matters if someone changes the daemon by hand, and then the install path reads the real state anyway.
- **The dev loop hides these apps.** Local Docker has no remap and the fake host-agent sends no `remap_base`, so `make dev` never shows a `root_setup` or `image_user` app in the lists. That is the true state of that box.

## Review

- **Fresh Sonnet agent:** no Block findings. Three Notes, the same three Greptile raised, handled below.
- **Greptile P1, "Uninstallable apps stay listed":** true, and dismissed as a code finding. It is the first known gap above: the lists hide an app only once the catalog publisher sends `root_setup` and `image_user` on its browse record, and that change is outside this repo. The box has to model the fields first, and the install plan covers the direct-link case for every app now. The PR body says so.
- **Greptile P2, "Back reopens blocked install":** confirmed and fixed. The install pages' Back button for an unavailable app called `cancel`, which pushes the App page, so the browser's Back returned to the blocked page. It now calls `goBack`, the history-aware Back every other page uses.
- **Greptile P2, "Store requests wait together":** confirmed and fixed. `RemapState` held its lock through the read, so an expired cache and a slow Docker made every store request wait up to five seconds. The lock is no longer held during the read. While a refresh is in flight, other callers get the last answer at once; only before the first answer do they wait, and a caller whose request goes away stops waiting with unknown, which hides nothing. Two tests hold a refresh inside `docker info` and check both cases, under `-race`.

## What's next

1. The catalog publisher sends `root_setup` and `image_user` on each browse record, and the pinned fixture is refreshed from its output.
2. #545: record the `userns-remap` capability, update the gap ledger and the authoring guide.
