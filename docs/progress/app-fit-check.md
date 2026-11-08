# The store says whether an app fits the box, and the install refuses one that is too big

- **Status:** done
- **Date:** 2026-10-13
- **Specs touched:** `APP_MANIFEST.md` # B2 and # Locked decisions, `APP_ISOLATION.md` # GPU and # Resource limits, `APP_STORE.md` # Apps this box cannot run, `BRAIN_UI_PROTOCOL.md` (catalog routes, # install-plan), `BRAIN_HOST_PROTOCOL.md` (`GET /v1/system/resources`), `DECISIONS.md` 2026-10-13, `docs/architecture.md`

This closes #599. A hosted box with 3.8 GB of memory ran two apps whose manifests recommended 4 GB and 2.5 GB. Nothing in the store or the install said so, the box ran out of memory and one app wedged ([ram-pressure-full-avg300.md](ram-pressure-full-avg300.md) has the numbers). `resources.recommended` was not even parsed: the specs described an install-time capacity check that was never built. This entry builds it, as a fit the store shows before Install and a refusal at install.

## What was done

- **The manifest models `resources.recommended`** (`internal/manifest`, `Resources`, `Recommended`). `memory` is a size string in the binary units of `storage.estimated_size`, read through `MemoryBytes`; `cpu` is a number of cores. `Parse` refuses a malformed size or a negative CPU, so `moose manifest lint` does too. All 124 store manifests pass `moose manifest lint` with the new parser (79 declare the block), and `memory: lots` is refused.
- **host-agent reports the core count** (`protocol.CPUCounters.Cores`, `cores` on `GET /v1/system/resources`). The real sampler counts the `cpuN` lines of `/proc/stat` (`procsource.parseCPU`), and the fake answers 4. An older host-agent sends none, which the brain reads as unknown.
- **New leaf package `internal/fit`.** `Check(box, memBytes, cpu)` returns an `AppFit`: `too-big` when the app recommends more memory than MemTotal rounded up to a whole GiB, or more cores than the box has; `tight` when it recommends more memory than the headroom, MemTotal minus the larger of (the running apps' recommendations, added up) and (the memory in use); `fits` otherwise, and always for an app that recommends nothing. A tight result names the running apps, largest first. CPU is never tight, and is not checked when the core count is unknown.
- **The rounding up to a whole GiB** was the maintainer's call during this work. A VM sold as "4 GB" reports about 3.8 GiB of MemTotal, and nine catalog apps recommend `4G`. Without the rounding, the issue's rule would refuse all of them on a 4 GB box even with nothing else running. With it, they are tight there and need "Install anyway".
- **`lifecycle.FitBox`** reads the box once: memory and cores from `SystemResources`, and the recommended memory of every `running` instance from the manifest saved next to it. A stopped app counts for nothing. When host-agent cannot report memory it returns not-ok, and nothing is shown or refused. `lifecycle.CheckFit` runs `fit.Check` for one app's recommendation.
- **The install refuses a too-big app** (`lifecycle.TooBigError`), in the install transaction right after the mail check and before admission, the GPU gate, the instance row or any Docker work. Its text is the failed job's message: "This box can't run this app as it is. It recommends 12.0 GB of memory, and this box has 7.6 GB."
- **The catalog carries the recommendation and the fit.** The browse record's optional `resources` block is modelled on `wireApp` (the store side is below) and kept on `Entry` as a private field. `Catalog.WithFit` is a view, like `WithoutRemapApps`, that fills `Entry.Fit` in every list and by-id shape: `List` (so home, category and search), the featured row, the spotlight, the groups, the packs, the landing sections, `Entry` and `Detail`. It hides nothing. The API builds it from one `FitBox` reading per request (`Server.withFit`), for the store lists and the detail route.
- **A member never sees another user's personal app by name** (Greptile on #625). `Server.fitForCaller` keeps in `running` only the instances `canSee` allows and counts the rest in `other_running`; the headroom still counts every running app. The notice then says "1 app that someone else on this box uses". It applies on the store routes and the plan.
- **The install plan carries `fit`**, and `unavailable: "too-big"` for a too-big app. `needs-remap` wins when both apply.
- **`dev/mkcatalog` copies `resources` onto the seed record**, so `make dev-app` shows the fit as a real catalog would.
- **UI** (`web-ui`). Wording in one place, `fit.ts`. The store card shows a small badge for tight ("May not fit right now") and too big ("Too big for this box"). The App page shows Memory and CPU rows next to Size, and `FitNotice` under the header: tight names the running apps with their memory and says to stop or uninstall one, too big says why this box can't run it. On a hosted box the notice carries **Upgrade your box**, a link to `https://mooseos.com/dashboard/boxes/<box-id>` (`upgradeBoxURL`). A tight app always opens the install pages (`installWarnings`), the info box repeats the notice, and the last button reads **Install anyway**. `useEvents` now refetches `["catalog"]` and `["install-plan"]` on `app.state_changed`, `app.installed` and `app.uninstalled`, so stopping an app moves another from tight to fits without a reload.
- **Tests.** `internal/fit`: every state, the 4G-on-3.8-GiB case, the larger-of headroom, the largest-first list, which resource is too big. `internal/manifest`: parse, absent block, refusals. `procsource`: core count, aggregate line not first. `internal/lifecycle`: a too-big install leaves no row and makes no Docker call; tight and unknown install; `FitBox` counts a running instance and stops counting it once it is stopped; a host error is unknown. `internal/catalog`: the pinned fixture carries `resources`; `WithFit` fills the list, the spotlight, the featured row, the groups and the detail from the record. `internal/api`: list, detail and plan agree for fits and too big on memory and on CPU, a host error gives no fit and no refusal, and a member sees a household app named and an admin's personal app only counted while the admin sees both.

## The store side

The published browse record did not carry `resources`, so the box could not show a fit before it fetched a manifest. The store publisher now lifts `resources.recommended` onto each detail record, with the memory string verbatim, the same way `root_setup` and `image_user` were added, and its synthetic fixture sets it on `alpha-notes`. `internal/catalog/testdata/snapshot.json` here is that fixture's output (`make catalog-fixture` in the store repo), not hand-edited. The store change is its own PR in `onmoose/store`. Until it is published, cards show no badge and the App page shows no Memory or CPU rows. The install plan and the refusal read the manifest, so they work from this PR alone.

## How it maps to the specs

- `APP_MANIFEST.md` # B2 and `APP_ISOLATION.md` # Resource limits described `recommended` as advice only, with a capacity check that was never built. Both now describe the fit and the too-big refusal. `DECISIONS.md` 2026-10-13 records the flip, next to the GPU hard gate's reasoning, which `APP_ISOLATION.md` # GPU now points to.
- `APP_STORE.md` # Apps this box cannot run says a too-big app is shown with its state, not hidden, and describes the `resources` block in box terms.
- `BRAIN_UI_PROTOCOL.md` documents `fit` on catalog entries and on the plan, and the `too-big` code. `BRAIN_HOST_PROTOCOL.md` documents `cpu.cores`.

## Verified against a running brain

The real brain and the fake host-agent, on a fresh state dir, with a seed of three store apps built by `dev/mkcatalog`. On Linux the fake host-agent samples the real `/proc`, so this also ran the new core count (8 cores, 7.6 GiB). The list and the plan carried `fit` for every app. With the seed changed to a 12G app and a 16-core app, both read `too-big` (memory and CPU), the plan said `unavailable: "too-big"`, and `POST /api/v1/apps` for the 12G app failed its job with the message above, leaving no instance and no state dir.

Not run against a real box: the tight state with real running apps (it needs real installs; the lifecycle test covers it with a real store and saved manifests), the UI in a browser, and the Upgrade link against the portal.

## Known gaps & deviations

- **"Install anyway" is a UI rule only.** The brain does not refuse a tight install that skipped the confirm; the issue asks for the dialog to need it, and the brain enforces only too big. A tight app with a visible duplicate keeps the label "Install my own copy", which is also an explicit confirm, with the notice on the page.
- **The Upgrade link is shown to the hosted box's owner only.** The issue says hosted only; a box user the owner created cannot change the box on the portal, so the link would lead nowhere for them. The portal side (`/dashboard/boxes/<box-id>` and its sign-in round trip) belongs to the private control plane and is not checked here.
- **The card and page fit come from the catalog's copy of the block, the plan's from the manifest.** Both are copies of the same manifest field, so they agree once the store publishes the field. Before that, the page uses the plan's fit (it loads with the page), and cards show nothing.
- **One host call per store request.** Each list, detail and plan request reads host-agent's `/proc` sample and the saved manifests of the running apps. There is no cache; the reads are local and small.
- **"Running" is the instance state in the brain's store**, not Docker's live state, the same source the rest of the dashboard uses. An app mid-install does not count yet.

## What's next

- Merge and publish the store PR, so cards and the App page get the figures.
- A box that is already over committed shows tight for most apps. Naming the heavy apps in the `ram-pressure` issue is #623.
