# The catalog is its own service, and the docs said it was the control plane

- **Status:** done
- **Date:** 2026-09-18
- **Specs touched:** `docs/specs/APP_STORE.md`, `docs/specs/BRAIN_UI_PROTOCOL.md`, `docs/specs/NEXT.md`, `docs/architecture.md`, `CLAUDE.md`, `README.md`, `docs/dev/contributing.md`, `docs/dev/authoring-apps-with-an-agent.md`, `docs/dev/running-locally.md`

Rides on [rename-to-moose.md](rename-to-moose.md), which is the change that points `MOOSE_CATALOG_URL` at `https://catalog.onmoose.io`. That URL is the last step of a move the box side never wrote down: the catalog is no longer part of the control plane. It is published and served from `onmoose/store`, on its own host and its own deploy, and its artwork is served from an object-storage origin rather than from the catalog host at all.

Nothing in the box's behaviour changes here. The code was already right, for a reason worth keeping: `resolveURL` treats every published URL as opaque and follows it as given, with a comment saying assets may move to object storage. They did. What was wrong was about twenty sentences that named the wrong operator, and one procedure that was missing.

## What was done

**The vocabulary is now "the catalog service".** Every place that said the control plane serves, publishes, or filters the catalog now names the service instead. That is `APP_STORE.md` (the superseded banner, # Failure modes, # What we run, # Landing page, # Category labels, # What the box models, and the locked-calls list), `docs/architecture.md` (the `catalog` package row and the App store bullet), `BRAIN_UI_PROTOCOL.md` # the asset routes, `NEXT.md` # catalog content policy, `CLAUDE.md`, `README.md`, and three files in `docs/dev/`. The same pass went through the code comments that carry the claim: `internal/catalog/wire.go`, `disk.go`, `remote.go`, and the catalog wiring in `cmd/brain/main.go`. One log message changed with them, `catalog: remote control-plane source` to `catalog: remote source`; nothing reads it.

**`CLAUDE.md`'s seam list said three things come from the control plane.** Two do. The catalog is the third and it does not, so the section is now "What a box is sent from outside it" and the catalog row says plainly that it is a different operator on a different deploy, so a catalog edit never touches the control plane.

**Artwork is called out where it was implied.** `APP_STORE.md` # What we run listed `GET /catalog/assets/{id}/{path...}` as a route of the same API. It is not one any more: a record carries absolute `icon_url` and `screenshot_urls` on an object-storage origin. The opaque-URL paragraph that predicted this now says it has happened, and the `catalog` row in `docs/architecture.md`, the asset-route text in `BRAIN_UI_PROTOCOL.md` and the `IconURL` comment in `wire.go` say the same.

**References to the private repo are gone from these paths.** `cloud #62`, `cloud specs/CATALOG.md` and "part of the moose cloud control plane" appeared in the spec, in `cmd/brain/main.go` and in `internal/catalog`. The issue numbers are this repo's, and the wire shape a box can actually read is `internal/catalog/wire.go`, so both now point at something a reader here can open.

**`docs/dev/contributing.md` has the procedure it was already linked to.** A new # Changing the published catalog shape section states the quiet property of the seam (a field the box does not model is dropped with no error and no log line, so a field is delivered when the box models it) and the four steps: model it or record the decision not to in `ignoredTopLevelKeys`, hand-edit `internal/catalog/testdata/snapshot.json`, restate the box-facing half in `APP_STORE.md`, and bump `wireSchemaVersion` only for a format the box cannot half-read.

**One test was added, because the claim the docs now make had no coverage.** Documents on another origin were already covered (`TestRemoteLoadFollowsAbsoluteDocumentURL`, which asserts the catalog origin served zero of them). Assets were not: the fixture published relative icon and screenshot URLs, so `TestRemoteAssetProxyAndCache` fetched artwork from the catalog origin itself, which is no longer how production works. `TestRemoteAssetFollowsAbsoluteURL` is the artwork half: it stands up a second server as the bucket, publishes absolute URLs at it, and asserts the bytes came from there, the extension survived into the cache name, and the catalog origin served zero assets.

**One stale command was removed while writing that.** Step 4 told the reader to run `go test ./internal/catalog -run TestVerifyFixtureSnapshot -update` to re-stamp the fixture's digest. That test does not exist and neither does the flag: `go test -run TestVerifyFixtureSnapshot` reports "no tests to run" and passes, which is why nobody noticed. It is a leftover from before #434 removed the index digest, and there is now no digest to re-stamp, since `version` is an opaque token the box never recomputes.

## What was checked

`gofmt` clean, `go vet` clean on the touched packages, and `make test-nopam` green, including the new test. The fixture rule is asserted by the tests that already exist: `TestNoUnmodeledFields` and `TestParseFixtureSnapshot` still pass against the hand-written snapshot, which is the point of keeping it hand-written.

The live endpoint was read rather than assumed: `GET https://catalog.onmoose.io/catalog?env=hosted` answers 200 and its records carry `icon_url` on `storage.googleapis.com`, which is the fact that made the asset sentences wrong.

## Known gaps

- **No lane asserts the operator split.** The box cannot tell who runs the host it fetches from, and should not, so this is documentation accuracy with no test behind it. What is tested is the property that makes the split safe, a published URL on another origin being followed as given, and both halves of it are covered now (# What was done).
- **`APP_STORE.md`'s superseded banner is getting long.** It now carries three layers of "not what shipped" (the signed-CDN design, the #434 restructure, and the operator move). A rewrite that states the shipped design first and keeps the history below it would read better, and is not this change.
- **The store side still points at a procedure it describes differently.** Whoever maintains both surfaces should reconcile the store's own wire reminder with the section added here, in particular the hand-written fixture rule (#420). Nothing on this side can check that.
