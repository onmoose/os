# moose App Store

> How moose publishes and serves the catalog of installable apps to every box. Companion to `APP_MANIFEST.md` (the contract being published), `APP_LIFECYCLE.md` (what the box does after fetching), `UPDATES.md` (the update flow that consumes catalog version bumps), and `RELEASE_MANIFEST.md` (the sibling doc this one borrows its publishing shape from).

The scope here is the **app catalog**: how apps reach a moose box, what the box trusts, and what infrastructure we run to publish it. Container images themselves are not hosted by us — they live in their authors' registries (Docker Hub, GHCR, …). What we publish is the metadata that tells the box which image bytes to trust for each app version.

> **Superseded — read this first (`DECISIONS.md` 2026-07-02, cloud `specs/CATALOG.md`).** The publish + trust model below (a static, minisign-**signed** `catalog.json` served from a CDN at `store.onmoose.network`, with per-app `manifest.yml`/`compose.yml` fetched on demand and hash-chained to a signed root) is **not what shipped.** As built (OS #62 / cloud #62, restructured in #434): the catalog is served by the **control plane's dynamic HTTP API**, on two seams. The box fetches **browse data** for its own surface in one request (`GET /catalog?env=<environment>`) — display records, the landing page, the category vocabulary, and an opaque `version` token — checks the schema version, **holds it in memory**, and projects the store locally. It fetches an app's **install payload** only when it installs that app, by following the `manifest_url` / `compose_url` on that app's record (`application/yaml`, the verbatim file). The box keeps **no copy of the browse data on disk**, so it always renders what the endpoint serves now; a box that has not synced shows an empty store. There is **no Ed25519/minisign signature and no integrity digest**: the box only ever fetches from the moose control plane over **TLS**, which authenticates the origin, and HTTP framing catches a truncated body. The digest that used to sit on the snapshot was doing cache work, not security work, and it made every new published field a flag day (# What the box models). No catalog is baked into the box image. The sections below are kept for the schema field semantics (`icon_glyph`, `footprint`, `images`, curation/`listed:`), which carry over; treat their signing/CDN mechanics as historical. The live wire shape is cloud `specs/CATALOG.md`; the box consumer is `../progress/catalog-remote-thin-client.md`.

## What the store is

A static, signed JSON catalog served from a CDN, backed by a git repo. Same shape as `RELEASE_MANIFEST.md`:

```
https://store.onmoose.network/catalog.json
https://store.onmoose.network/catalog.json.minisig
https://store.onmoose.network/apps/<id>/manifest.yml
https://store.onmoose.network/apps/<id>/docker-compose.yml
https://store.onmoose.network/apps/<id>/icon.png
https://store.onmoose.network/apps/<id>/screenshots/...
```

`catalog.json` is the **index** — one entry per app with the current published version, content hashes, and resolved image digests. Per-app `manifest.yml` and `docker-compose.yml` are fetched on demand at install time. The catalog itself is small even at scale (~300 bytes per entry — see Scaling below).

The brain polls the catalog on the same hourly cadence as the release manifest. When an app's version in the catalog advances past what's installed, the per-app update flow fires (`UPDATES.md` # 4). When the user opens "Browse store," the brain hands the UI the snapshot it holds in memory.

## Catalog schema (v1)

```json
{
  "manifest_version": 1,
  "generated_at": "2026-05-17T08:00:00Z",
  "apps": {
    "photoprism": {
      "version": "2.4.1",
      "name": "PhotoPrism",
      "categories": ["media", "photos"],
      "short_description": "Self-hosted photo library with AI tagging",
      "icon_url": "/apps/photoprism/icon.png",
      "icon_glyph": "image",
      "manifest_url": "/apps/photoprism/manifest.yml",
      "manifest_hash": "sha256:def456...",
      "compose_url": "/apps/photoprism/docker-compose.yml",
      "compose_hash": "sha256:fed321...",
      "images": {
        "photoprism/photoprism:2.4.1": { "digest": "sha256:abc123...", "download_bytes": 612000000, "disk_bytes": 1480000000 },
        "mariadb:11.0.2": { "digest": "sha256:789xyz...", "download_bytes": 118000000, "disk_bytes": 402000000 }
      },
      "footprint": {
        "image_download_bytes": 730000000,
        "image_disk_bytes": 1882000000,
        "estimated_state": "10GB"
      },
      "files_first_class": true
    },
    "immich": { ... }
  }
}
```

Fields per app:

- **`version`** — semver of the currently-promoted version. Bumping this is the publish action.
- **`name`, `categories`, `short_description`, `icon_url`** — for the browse UI. The browse view is rendered from these alone, without fetching individual manifests.
- **`icon_glyph`** — optional Lucide icon name (kebab-case, e.g. `notebook-pen`) the browse UI renders as the card/header icon when `icon_url` is absent, instead of a single generic glyph. Author-chosen fallback for apps that ship no logo; ignored when `icon_url` is present. The brain passes it through verbatim and only shape-validates it (kebab-case) — it can't confirm the name exists in the icon set, which lives in the UI, so an unknown-but-well-formed name degrades to the generic glyph client-side. Browse UI groups by category regardless of file shape; icon choice is likewise a UI concern.
- **`manifest_url` / `manifest_hash`** — content-addressed pointer to the full manifest. On install, brain fetches and verifies the hash matches.
- **`compose_url` / `compose_hash`** — same, for the compose file.
- **`images`** — map of `image:tag` (as referenced in the compose) → `{ digest, download_bytes, disk_bytes }`. CI resolves all three at catalog-build time from the registry: `digest` (the pinned bytes — see Trust below; the brain pulls by digest, not by tag), `download_bytes` (sum of the image's compressed layer sizes — the bandwidth/time cost), and `disk_bytes` (sum of its uncompressed layer sizes, deduping layers shared *within this app's own image set* — the on-disk cost). Sizes are **display-only and advisory** (# Trust model); only `digest` gates the pull.
- **`footprint`** — per-app summary so the **browse grid renders the size without fetching the full manifest**: `{ image_download_bytes, image_disk_bytes, estimated_state }`. CI computes the two image totals by summing the `images` entries and hoists `estimated_state` verbatim from the manifest's `storage.estimated_size` (`APP_MANIFEST.md` # Storage; absent if the manifest omits it). The image totals are an **upper bound** — they assume nothing is cached locally; the install dialog shows a sharper, box-specific number that subtracts already-present images (`BRAIN_UI_PROTOCOL.md` # GET /api/v1/catalog/:id/install-plan). `estimated_state` is the **measured app-state baseline at install** (`DECISIONS.md` 2026-06-09), not a usage projection — the same value on the card and in the dialog.
- **`files_first_class`** — true when the manifest declares `folders` and does not set `storage.app_managed_user_content`. Surfaces as a badge in the UI; not a gate.

Top-level fields:

- **`manifest_version`** — schema version. Brain ignores unknown fields so new optional fields land additively.
- **`generated_at`** — informational; not used for any gating.

Anything not in the schema is implicit (per-app file paths follow the URL convention) or out of scope (rollout pacing, telemetry — not in scope for the store).

## Trust model — what's signed, what's pinned

> **Superseded (`DECISIONS.md` 2026-07-02, #434).** There is no signature in the shipped design, and no integrity digest either — trust is **TLS to the control plane** (see the banner at the top). The minisign/pubkey-rotation mechanics in this section did not ship. The *digest pinning* of image bytes (below) carries over: the resolved `@sha256:…` digests live in each app's `images` block inside the verbatim manifest the box re-parses, and the brain still pulls by digest.

The catalog is **signed with minisign (Ed25519)** by the moose store key. Brain verifies on every fetch and refuses to act on an unsigned or invalidly-signed catalog.

- **Pubkey is baked into the brain image** at build time. Same forward-compat pattern as `RELEASE_MANIFEST.md`: verifier accepts a **list** of pubkeys, so rotation is dual-sign-then-drop without a flag day.
- **Store signing key is separate from the release-manifest signing key.** Different blast radius — a compromised store key lets an attacker publish a malicious app manifest; a compromised release key lets them ship a malicious brain. Separating them limits damage.

**Image bytes are pinned by digest in the catalog, not in the manifest.** Authors declare `image: photoprism/photoprism:2.4.1` (version, ergonomic). CI resolves the digest at catalog-build time and writes it into the `images` map. The brain pulls by `@sha256:...` derived from the catalog. The signed catalog is the binding from "the moose store promises version 2.4.1" to "these specific bytes."

Consequences:

- Tag mutation on an upstream registry (intentional or compromised) does not affect installed boxes — they pulled the digest the catalog promised.
- A new release of the app is a CI run that resolves the new digest and a PR that bumps `version` + `images` in the catalog.
- Authors never manage SHAs; manifest stays readable and portable (the same manifest still runs outside moose with normal `docker compose pull`).

**Image sizes are display-only, not part of the trust binding.** The same CI run that resolves a digest also records the image's `download_bytes` / `disk_bytes` (# Catalog schema). These exist purely to tell the user the on-disk footprint before they install; they gate nothing — a size that drifts from reality is a cosmetic bug, not an integrity failure. Only the digest binds bytes.

**What we don't sign:** individual manifests / compose files don't carry their own signature. Their integrity is bound to the catalog via the `manifest_hash` / `compose_hash` fields. One signed root, hash-chained leaves — same shape as the well-known package-manager pattern.

**What we don't host:** container images live wherever the author publishes them. We don't mirror Docker Hub. The "your app keeps working if the original developer disappears" pitch is delivered by the **running box's local image cache**, not by us re-hosting upstream artifacts. Mirroring is a Tier-3 future concern.

## Verification lives in the brain

`host-agent` verifies the release manifest because the release manifest controls *the brain itself* — verifying there avoids "brain verifying its own upgrade." The app catalog is a higher-frequency, lower-stakes feed about *apps the brain manages*. Verification belongs in the brain:

- The brain already speaks to Docker, owns the install transaction (`APP_LIFECYCLE.md`), and is the place per-app state lives.
- Keeps host-agent narrow — its job is host-level concerns, not app-catalog parsing.
- The store pubkey baked into the brain image cycles on the brain's release cadence, which is exactly where we want it.

`host-agent` stays the verifier for the release manifest. Two verifiers, two keys, two scopes — the small extra surface buys clear layering.

## Submission and promotion

The catalog source of truth is a git repo (`github.com/moose/store` or similar). Each app is a directory:

```
github.com/moose/store
├── apps/
│   ├── photoprism/
│   │   ├── manifest.yml
│   │   ├── docker-compose.yml
│   │   ├── icon.png
│   │   ├── screenshots/
│   │   └── CHANGELOG.md
│   ├── immich/
│   └── ...
├── catalog.json            ← generated by CI from the tree
└── catalog.json.minisig    ← signed by maintainer offline
```

A publish — new app or new version of an existing app — is a pull request that updates the app's directory. CI on the PR validates:

- Manifest parses against the schema (`APP_MANIFEST.md`).
- Compose passes the admission rules (`APP_LIFECYCLE.md` # admission policy) — no host port bindings, no `cap_add`, no host networking, etc.
- All images referenced in the compose are pullable from their registries.
- Image digests resolve and are recorded in `catalog.json`.
- Hashes recomputed; signature on `catalog.json.minisig` verifies.

The maintainer signs `catalog.json` offline (hardware token), commits `.minisig` alongside, opens the PR. On merge, the CDN syncs within seconds and boxes pick up the change on their next hourly poll.

For the v1 single-maintainer phase, self-merge is fine. Branch protection (require an additional reviewer) is a one-setting change with no doc impact.

## v1 catalog is hand-curated by moose

The first apps are written by us — manifests wrapping popular open-source projects (Immich, Paperless-ngx, Jellyfin, Navidrome, etc.). Authors aren't yet submitting their own manifests; the store repo is the catalog.

This shapes the v1 trust model intentionally:

- **Every manifest is signed-by-moose** because every manifest is *authored-by-moose*.
- **Curation policy is enforced by review**, not by automation — we set the bar (`files_first_class` preferred; `app_managed_user_content` rare and labeled; stdout logging; declared-vs-actual permission match).
- **Third-party authorship** lands later — when the catalog ecosystem matures, app authors will submit PRs against `moose/store` (still signed by us) before the model evolves further into per-store keys.

The data model below already accommodates additional catalogs from day one, so the transition is additive when it happens.

## Listed apps — pulling an entry without deleting it

Curation sometimes needs to **withdraw an app that can't currently ship** — an image that crash-loops under the sandbox until a platform gap closes, or one rejected for good — without throwing away the adaptation work (the rewritten compose, resolved digests, the manifest itself). The manifest's `listed:` field (`APP_MANIFEST.md` # A; default `true`) is that control: `listed: false` keeps the entry in the catalog directory but pulls it from the store.

The brain enforces it asymmetrically, by intent:

- **Store-facing paths filter it out.** The browse list (`GET /api/v1/catalog`) omits unlisted apps, the detail page (`GET /api/v1/catalog/:id`) returns 404, and both install paths (`/install-plan` and the `POST /api/v1/apps` install action) return 404 — so a stale store link or a scripted call can't install a deliberately-withdrawn app. To the store, the app simply doesn't exist.
- **By-id resolution stays honest.** Loading the manifest by id (for an already-installed instance's dashboard card, for reconciliation, for serving its icon, for `moose manifest lint`) ignores the flag. An app unlisted *after* someone installed it keeps working and stays manageable — withdrawal affects discovery and new installs, never a running instance.

This is a **curation control, not access control**: it's box-wide, not per-user or per-role, and there is no "show unlisted apps" path in v1. `listed: false` is the mechanism that enforces a `Blocked` or `Rejected` curation verdict — it pulls the app from the store while its adaptation work stays intact in the catalog.

## Multiple catalogs — data model only in v1

`SPEC.md` and `APP_MANIFEST.md` both commit to third-party stores as a long-term shape. v1 does **not** ship UI for adding them, but the brain's data model treats "the catalog" as one entry in a list:

```
catalogs:
  - id: moose
    name: moose
    url: https://store.onmoose.network/catalog.json
    pubkeys: [<minisign-pubkey>]
    builtin: true
```

A third-party catalog later is the same row with `builtin: false` and its own URL + pubkeys, added through a settings flow that doesn't exist yet. The brain's verify-fetch-install pipeline already operates per-catalog. Apps include their `catalog_id` in SQLite so "this app came from store X" is recorded from day one — avoiding a retrofit when the second catalog ships.

The UI in v1 shows one tab: the moose store. No settings affordance to add another.

## Scaling: when single-file catalog becomes too much

Numbers: each catalog entry is ~300 bytes JSON. 100 apps = ~30 KB. 1000 apps = ~300 KB. 10,000 apps = ~3 MB. Single-file is fine well past the realistic v1 horizon. Hourly fetch of a few hundred KB is unremarkable.

Migration when it eventually bites is additive because consumers already fetch through an index:

- Today: `catalog.json` contains all entries inline.
- Later: `catalog.json` becomes a shard index pointing to per-category files (`/shards/media.json`, `/shards/productivity.json`), each independently signed. Brain learns to follow shard pointers.

Older brains that don't know about shards keep working as long as we keep serving the flat form during the transition window. Defer until the catalog crosses ~1 MB or hourly fetch latency becomes user-visible.

Browse UI groups by category regardless of file shape — the grouping is a UI concern, not a transport concern.

## Failure modes

- **Box can't reach the control plane:** the store is empty until a sync lands. The brain holds the snapshot in memory only, so a box that has just restarted has nothing to browse, and a box that was already running keeps the snapshot it has until the process ends. The background sync retries; nothing else on the box is blocked. Icons and screenshots already fetched stay servable from the asset cache for up to 24 hours, so a store that does have a snapshot doesn't lose its artwork on a blip.

  Empty rather than stale is the deliberate choice. Browsing an old catalog was never the same as being able to act on it — installing an app pulls images over the same network that just failed — and a box pinned to an old copy can offer a manifest the store no longer publishes. The catalog is a separate distribution with its own release cadence, and the box is a thin client of it (`DECISIONS.md` 2026-08-17).
- **Browse payload fails its schema check:** the brain rejects it — a payload stamped with a format the box cannot project never becomes the read source. Whatever payload the process already holds stays in effect, and a box with none stays empty. This is the only refusal left on the browse path; unknown fields are dropped, not refused (# What the box models). A truncated body fails to parse, which is the same outcome.
- **Install payload can't be fetched:** the install fails with a plain error and writes no state. A `404` on the document route reads as "no such app" (the store no longer serves this app's payload); anything else reads as a reachability failure. Browsing is unaffected — it never touches those routes.
- **An installed app leaves the box's surface (or leaves the catalog):** the app keeps working. Its manifest and compose were written next to the installation at install time, so every routine path — the route builder, the mail picker, the resource limits — reads the box's own copy and never the catalog. What degrades is only the catalog-supplied display metadata: `GET /catalog?env=` no longer carries a record for that app, so its card falls back to the instance row's own name and version with no icon. This is the accepted trade of moving environment filtering to the server (#434); the alternative, persisting a copy of the display record too, buys a card icon and a second thing to keep fresh.
- **Image pull fails at install time:** standard install failure, surfaced per `APP_LIFECYCLE.md` # install transaction.
- **Image digest changes upstream between catalog publish and box pull:** the box pulls by digest, so the upstream's new bytes don't affect it. The box installs the bytes the catalog promised. If the digest was *deleted* from the upstream registry (rare — most registries keep digests addressable), the install fails with a registry-side error.

## What we run

As shipped (cloud #62; the live wire shape is cloud `specs/CATALOG.md`), end-to-end:

1. **Store git repo** (cloud-side) — the authoring source of truth: one directory per app with its `manifest.yml` + `compose.yml` + assets.
2. **CI on the repo** — schema lint, admission check, image-pullability check, digest resolution, and catalog regeneration (browse records + the per-app documents + assets).
3. **The control plane's catalog API**, served **over TLS** on the moose apex — `GET /catalog?env=<environment>` for browse data, `GET /catalog/apps/{id}/manifest` and `/compose` for an app's two install documents (`application/yaml`, the verbatim file, `Cache-Control: public, max-age=3600`), and `GET /catalog/assets/{id}/{path...}` for artwork. This is a real backend service (part of the moose cloud control plane), not a static CDN: the box is a thin HTTP client of it.

Trust is **TLS to the control plane** — there is no signing keypair, no pubkey baked into the brain image, and no integrity digest. The publish flow is git-driven (a store PR regenerates what the API serves); the box-side flow is fetch-project, plus one document fetch per install.

**Every URL on a browse record is opaque.** `icon_url`, `screenshot_urls`, `manifest_url` and `compose_url` are followed as given (resolved against the catalog base URL when relative); the box never assembles a path of its own. That is what lets documents or artwork move to object storage on another origin without a box-side change.

## Landing page

The store's front page — the box's landing view and the control plane's own store surface at `store.onmoose.network` alike — is authored whole in a curated `home.yml`, not derived from any app's own metadata: a single `spotlight:` app id rendered as a banner, plus an ordered list of `groups:` (a `category:` id from the catalog's category list and 1-4 app ids) rendered as packed rows below it. Editing the front page is editing that one file; importing a new app or reordering a manifest's `categories:` never reshuffles it, because the page's shape isn't computed from categories at all.

The control plane publishes the block **verbatim** on the snapshot (`CatalogFile.Home`) — carried, not derived, so the curation decision stays with the store curation source, not with a projection the control plane or a box could drift out of step with. The same one filter applies at serve time: an app the block names that isn't advertised on the requesting surface (`Environments`, # Catalog schema above) drops out of its slot — the spotlight goes unset, or the app is skipped within its group — and a group left with no advertised apps is dropped entirely rather than rendered empty. **The environment filter itself is the control plane's**, applied to the `?env=` the box sends (#434): a box receives only apps its surface may show, so it applies no second visibility pass. What the box still does locally is resolve the home block against the apps it received — an id the response does not carry drops out of its slot, and an emptied group is dropped — because it renders its landing from the payload it holds in memory (`internal/catalog/remote.go`).

The box derives nothing else from the block: it does not compute rank and does not re-sort groups. Group headings use the authored category label carried on the snapshot (# Category labels), not text derived from the id. If a synced snapshot carries no home block (an older control plane, or a curation publish with an empty `home.yml`), the box's landing has no spotlight and no groups; the view falls back to the flat curated Featured row, and if that's empty too, to a plain "pick a category or search" prompt — the landing is never blank, but an empty home block is not the same as "nothing curated."

## Category labels

An app's `categories:` holds ids (`developer-tools`, `ai`). Ids are not display text, so something has to turn one into a heading — and for a while every surface did that itself. They disagreed: the same category rendered as "Developer-tools" in one store and "developer tools" in the other, and `ai` rendered as "ai" in both. A stopgap made the two agree with each other (hyphens to spaces) without making either correct.

The fix is that the label is **authored once and carried**, never derived. The synced snapshot has a top-level `categories` block — the whole vocabulary, each entry an `id` and the display `label`, in authored order:

```json
"categories": [
  { "id": "productivity", "label": "Productivity" },
  { "id": "developer-tools", "label": "Developer tools" },
  { "id": "ai", "label": "AI" }
]
```

`AI` is the case that shows why deriving cannot work: no rule over the id produces it.

The box renders that label everywhere it names a category — the pills, the landing row headings, the category page heading, and the app detail page's info panel. Two consequences worth stating:

- **The pills follow authored order, not alphabetical.** The vocabulary is a curation artifact, like `home.yml`, so its order is a decision rather than an accident. The box shows only the categories its own surface actually has apps for, in that order.
- **The box never applies text transforms to a label.** No `capitalize`, no title-casing. "Developer tools" is what was authored, and "Developer Tools" is not.

Two fallbacks remain, and neither is the normal path. A category id the vocabulary does not carry still gets a pill — dropping it would hide a browsable app behind no entry point — labelled with a readable form of the id (`developer-tools` → "Developer tools"), appended after the authored entries in sorted order so the result is deterministic. A box that has never synced, or one reading a disk catalog, has no vocabulary at all and labels everything that way. The control plane holds the same fallback, so even the degraded modes agree.

The vocabulary is carried on the browse payload alongside the app records and the home block; nothing about it is derived on the box.

## What the box models, and what it drops

The box does not model the whole published catalog. `internal/catalog/wire.go` declares the fields the box surfaces, and it declares a **subset** on purpose.

**An unknown key is dropped, wherever it sits.** `encoding/json` ignores any key it has no field for, top-level or inside an app, so a field added upstream reaches the box and vanishes there — no error, no log line, nothing the box could act on even in principle. **A new field is not delivered when the control plane starts serving it. It is delivered when the box models it.** Adding one is a two-side change: the wire struct here, plus the projection into whatever the box surfaces.

The price is silent loss. What it buys is that the publish side can move first, without waiting for every deployed box.

### It used to be worse: the per-app flag day

Until #434 the two cases behaved in opposite ways, and the per-app one was a trap. `verify()` recomputed the snapshot's `index_sha256` by **re-marshalling the apps it had just parsed**. A box that did not model a per-app field dropped that key on parse, so it was absent from the re-marshal, so the digest could not reproduce — and the box rejected the **entire snapshot**, not the field and not that app. A running box went stale; a restarted one had an **empty store**. The signal was one log line about a digest mismatch, which read like corruption rather than a shape change.

This was not theoretical. It was measured on `external_costs`: a box at `e825e9f` fed a real published snapshot in which one app declared a cost returned `catalog index digest mismatch`, while the same snapshot verified on a box that modelled the field. Adding a per-app field was therefore a coordinated release — model it, ship it to the fleet, and only then publish data that used it.

The digest is gone. It was never doing security work — TLS authenticates the origin, HTTP framing catches truncation — it was doing cache work, and the `version` token does that job without making field order load-bearing. Adding a display field to the published catalog is now a publish-side change again.

**`wireSchemaVersion` (and the control plane's `SchemaVersion`) stays**, for the change a box genuinely cannot read: a format it could only half-project. That check is exact equality, so bump it *with* the change, never ahead of it — raising the version while nothing needs it would reject every payload on every deployed box immediately.

### The shape guard

What keeps the modelled shape reviewable is a **synthetic** pinned payload at `internal/catalog/testdata/snapshot.json`, plus `TestNoUnmodeledFields` (`internal/catalog/wire_test.go`). The fixture is hand-authored — fake apps in the published wire shape, not a copy of any catalog the control plane serves — and the test parses it into a generic map, failing on any top-level or per-app key the box's own types do not declare, including the nested per-app shapes (footprint, author, links).

Be clear about what that does and does not buy. It is no longer a correctness gate: an unmodelled key costs a feature now, not a working store. Both sides of the comparison live in this repo, so what the test holds in step is the box's types and the shape written down next to them, and it makes that shape reviewable as one file. It **cannot** tell you the published shape moved: the box does not hold a published payload to compare against. Noticing a newly published field the box does not model is a **publish-side** check, on the side that has the published payload. When the box starts modelling a field, add it to the fixture; when it deliberately does not model a top-level field, list it in `ignoredTopLevelKeys` with the reason. An explicit "we looked and said no", never silence.

Modelling a field is per-consumer work: this same subset-and-drop rule holds for any other surface that consumes the published shape, so a field one surface starts showing does not appear on another until that surface models it too.

## Locked decisions

_(Updated for the shipped design — `DECISIONS.md` 2026-07-02, cloud #62. The earlier signed-static-CDN calls are superseded; the digest-pinning and don't-host-images calls carry over.)_

- **The catalog is served by the control plane's HTTP API,** not a static CDN file. The box fetches **browse data** for its own surface in one request (`GET /catalog?env=`) and an app's **install payload** only when it installs that app (`GET /catalog/apps/{id}/manifest` and `/compose`). It is a **thin client that keeps no catalog on disk**. Not synced ⇒ empty store; only icons and screenshots are cached, with a 24-hour expiry (`DECISIONS.md` 2026-08-17).
- **Browse and install are separate fetches** (#434). Carrying every app's manifest, compose and resolved images inside the browse response made 77% of a 614KB payload install data for apps the box would never install, and it put the box's install path and its store grid behind one shape. Splitting them also killed the index digest, which is what made every published field a flag day.
- **Environment filtering is server-side.** The box sends `?env=<appliance|hosted>` and shows what it receives; it runs no second visibility pass. An installed app that leaves the box's surface keeps working (its manifest lives next to the installation) but loses its catalog-supplied card metadata (# Failure modes).
- **Every published URL is opaque.** The box follows `icon_url`, `screenshot_urls`, `manifest_url` and `compose_url` as given and assembles no paths of its own, so artwork or documents can move to another origin without a box-side change.
- **App manifests and compose files are authored in the store, not in `os`.** The `os` repo holds the schema, the admission policy, and the tooling; the artifacts live in `onmoose/store` and reach a box only through the published snapshot. Catalog fixtures in `os` are synthetic.
- **No signing, and no integrity digest.** Trust is **TLS to the control plane**, which authenticates the origin; HTTP framing catches a truncated body. A signature would re-authenticate bytes TLS already covers, for a key-distribution cost and no threat it closes. The digest was cache machinery wearing an integrity label, and it cost a flag day per published field (#434); the opaque `version` token does the cache job. The schema version stays as the one refusal.
- **Authors declare image versions; CI resolves digests into the published catalog.** The brain pulls by digest — the resolved `@sha256:…` lives in each app's `images:` block inside the verbatim manifest the box fetches and re-parses. Tag mutation on upstream registries can't ship malicious code to a box.
- **The manifest + compose are fetched per app, at install time,** and then **persisted next to the installation**. That is what keeps routine box operation off the catalog service and keeps an installed app's manifest alive after the app is unpublished. There are no per-app hash-chained files: the documents are plain `application/yaml` behind TLS.
- **Verification happens in the brain** (not host-agent). The brain owns app lifecycle and re-parses each manifest with its own `manifest.Parse`, staying the sole enforcer of the manifest contract.
- **We don't host container images.** Authors publish to their own registries. The box's local image cache delivers the "app keeps working if the developer disappears" property. Image mirroring is deferred.
- **v1 catalog is hand-curated by moose.** Every manifest is moose-authored. Third-party authorship (PRs against the store) lands later.
- **No baked catalog in the box image.** Every box — appliance and hosted — is a control-plane thin client (`DECISIONS.md` 2026-07-02).
- **Promotion is a PR against the store repo** with a regenerated snapshot. CI validates schema, admission rules, image reachability, and digests; merge is the publish action (the control plane then serves the new snapshot).
- **Category display text is authored, never derived.** The snapshot carries a `categories` vocabulary (id + `label`, in authored order); the box renders the label on pills, group headings, the category page, and the app detail panel (# Category labels). Deriving display text from the id is what made two store surfaces disagree about the same category.
- **The landing page (spotlight + category groups) is authored whole in a curated `home.yml`, carried verbatim on the snapshot, and filtered by environment at serve time** — never derived from an app's own `categories:` or computed by either consumer (# Landing page). Both the box and the control plane's own store surface render the same authored shape.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
