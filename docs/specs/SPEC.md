# moose — Home Server OS

> Working spec. Living document. Codename `moose` (placeholder, may stick).

## Vision

A home server OS that helps ordinary people take back ownership of their data and the apps they use every day. Same category as Umbrel, ZimaOS, CasaOS — explicitly **not** targeting Unraid / TrueNAS power users.

**North star:** simplicity and ease of use, above all else.

**The pitch:** install on an old laptop or PC, leave it running in the pantry, and run the apps you use daily — a grocery list shared with your partner, photos, notes, files — on hardware you own, with data you own. If the original developer disappears, your app keeps working.

## Audience

- **Long-term:** non-technical users who want to own their data and apps.
- **Initial:** tinkerers as early adopters — they're patient with rough edges and bring the app ecosystem with them.
- The product roadmap always optimizes toward the long-term audience; tinkerers are a stepping stone, not the destination.

## Differentiators (vs Umbrel / ZimaOS / CasaOS / Runtipi / Yunohost)

Combination, not a single axis:
- Branding & positioning
- Hardware openness (BYO, broad compatibility)
- Ease of install and daily use
- **App ecosystem — the strongest pillar**
- Developer-friendliness for app authors

## Distribution & Hardware

- **Bootable ISO**, takes over the whole disk. Single-purpose appliance.
- **Bring-your-own-hardware**, broad x86 support. ARM TBD.
- No hardware lock-in. Selling reference hardware is a possible future SKU but never a requirement.

## Architecture (initial decisions)

- **Base:** Debian.
- **Single-node only.** No clustering, no multi-machine federation. One box.
- **Multi-disk storage.** During install, the user picks from a small set of presets (ZimaOS-style radio buttons) with plain-language guidance and warnings:
  - **Maximize storage (no redundancy)** — all disks pooled with mergerfs, every byte usable. If a disk dies, only the data on that disk is lost. *Default starting point.*
  - **Protect my data (redundancy)** — mirrored / parity setup, less usable space. Implementation TBD (likely btrfs/ZFS down the line).
  - Possibly a third option later (e.g. single-disk-only / manual).
  - Rule: never more than ~3 options, no jargon. Redundancy framed as "protection," not as RAID levels.
  - Off-site backup (paid add-on) is the *primary* answer to data safety; on-disk redundancy is a secondary nice-to-have.
- **Apps are the product.** Functionality comes from apps, not from the OS chrome.
- **All apps are Docker-based.** One-click install.
- **App packaging = standard `docker-compose.yml` + a small `manifest.yml`.**
  - The compose file is the runtime — exactly what app authors already write.
  - The manifest holds moose-specific metadata: name, icon, category, description, exposed ports, permissions, backup hooks, post-install hooks, dependencies.
  - Goal: 90% of what an app author knows from Docker carries over; only the 10% that is moose-specific is new.
- **We bootstrap the ecosystem** by writing manifests for popular open-source projects ourselves.

## Monetization

- Paid cloud add-ons: off-site backup, DDNS, possibly remote access.
- App store revenue from paid apps.
- Core OS remains free / open.

## Accounts & users

- **Multi-user from day one.** Designed for the household: one account per family member.
- **First account created = admin (root).** Owns the box, can manage other accounts and the system.
- **App instances are owned by an account.** Every instance is either a **household (shared)** instance — admin-owned, one instance the whole household opens — or a **personal (per-user)** instance, owned by one user with its own data, route, and folder bindings. Admins choose which at install; members can create personal instances only. Duplicate installs warn, never block (you can run your own copy of an app someone already installed). See `DASHBOARD.md` # the apps model — the load-bearing decision recorded in `DECISIONS.md` 2026-05-29.
- **User content stays shareable** independent of app instancing — a shared Photos library under `~/Shared/`, per-user folders under `~/`. Personal instances bind only their owner's folders, which keeps "files are first-class" per-user isolation intact (`STORAGE.md`).
- **No moose SSO into apps (for now).** Each app keeps its own authentication. The moose login gates the device; in-app accounts are separate.

> Open tension: with no SSO, a *household* app still forces each member to create an in-app account. Owner-scoping softens this (a member who wants their own data can install a personal instance instead of sharing), but the shared-instance case remains. Revisit if/when SSO lands — at which point shared instances become the *encouraged* path for multi-user-capable apps.

## OS-provided managed services

A core architectural bet: **the OS offers shared infrastructure services that apps can request, instead of every app shipping its own.**

- Apps declare in their manifest that they need, e.g., a Postgres database.
- moose provisions a database in a centrally-managed Postgres instance for that app, hands the app credentials, and handles backups, upgrades, and maintenance — like a managed cloud DB.
- Catalog will likely include: Postgres (multiple major versions), and over time others (Redis, MariaDB/MySQL, etc.).
- Benefits: smaller / simpler apps, one well-tuned DB instead of dozens of half-configured ones, dedup of resources, OS-level backup of all app data through one path.
- Implication: app authors give up version pinning freedom in exchange for not running their own database.

This is a meaningful differentiator vs Umbrel / CasaOS, where every app bundles its own database. Worth treating as a first-class platform feature, not an afterthought.

## App–OS contract (early sketch — schema deferred)

The full `manifest.yml` schema is **deliberately deferred** until we understand the platform better. Known requirements so far:

- **Ports.** App declares its internal port. **The OS chooses the external port** — apps cannot demand a specific external port (since multiple apps would all want 8080 / 80 / 443).
- **Storage tiers.** Apps can request **fast storage (SSD)** or **normal storage (HDD)** for their data, depending on workload. The OS honors the request only if the hardware actually has that tier; otherwise it falls back gracefully.
- **Managed services.** Apps declare which OS-provided services they need (see above).
- *(Permissions, lifecycle hooks, dependencies between apps — too early to define.)*

## App store

- **Curated by us, open submissions.** Anyone can submit; we review.
- **Open store API.** The moose store is built on the same public API anyone else can build on. Users can add **third-party stores** alongside (or instead of) the official one. No preferential treatment for the official store at the OS level.
  - Implication: the manifest format and store API must be public, stable, and versioned from early on.
- **Automatic app updates by default.** Users can turn auto-updates off per-app.
- *(Paid app mechanics — open question, deferred.)*

## Access model

### Local access

1. **Web UI at `moose.local`** (mDNS) — primary interface. Manage the instance, browse and install apps, configure the system.
2. **Virtual terminal in the web UI** — shell without leaving the browser.
3. **SSH** — direct shell access for tinkerers / debugging. **LAN-only**, never exposed remotely.

### LAN routing — subdomain-based

- Each installed app is reachable at its own single-label `.local` name: `photos.local`, `grocery.local`, etc. (Single-label, not `photos.moose.local` — the `.moose` infix made the name multi-label, which Linux resolvers reject; see `DISCOVERY.md` # Per-app A records and `DECISIONS.md` 2026-05-31.)
- App authors **suggest preferred slugs** in priority order in their manifest; the OS picks the first one that's free.
- The OS publishes each app's hostname via mDNS (Avahi) at install time.
- LAN traffic is plain HTTP; HTTPS is available on the same apps via the opt-in `<box-id>.onmoose.io` subdomain (see `MOOSE_NETWORK.md`).

#### Why subdomain — and why we explicitly rejected path-based routing

We considered path-based routing (`moose.local/photos`, `moose.local/grocery`) and rejected it. Capturing the rationale because it's load-bearing for the security model and we don't want to relitigate.

**Path-based looked attractive at first:**
- Single mental model — everything under one domain.
- One bookmark covers the whole box.
- Works fine over plain HTTP on mDNS.
- Simpler reverse proxy config in the trivial case.

**Why we rejected it — two real costs:**

1. **Browser security model breaks down.** The browser same-origin policy keys on `scheme://host:port`. Path-based means *every app shares one origin*, which means the browser can't isolate them. Concretely:
   - **`localStorage` and `IndexedDB` are shared across all apps.** A buggy or malicious app can read another app's session tokens. This is how the web is specified — there is no fix.
   - **Cookies bleed across apps** unless every app sets `Path` perfectly (most don't).
   - **CSRF protections weaken.** Apps can issue authenticated requests against each other.
   - **CORS is a non-event.** Apps can `fetch()` each other freely, including credentials.

   Given that we allow **third-party app stores**, this is unacceptable. One bad app on a path-based moose could quietly pillage every other app's tokens. The blast radius is the entire box.

2. **~30–50% of self-hosted OSS apps assume they live at `/`.** They emit absolute URLs (`<a href="/login">`, `<script src="/static/app.js">`). Some accept a `BASE_URL` env var; many don't. WebSocket paths, OAuth callbacks, third-party widgets all carry root-path assumptions. Path-based forces ongoing per-app patching, directly increasing the cost of our strongest pillar (the curated app catalog).

**Subdomain wins on both axes:**
- Each app gets its own origin → real browser-enforced isolation. Cookies, localStorage, CORS all properly scoped without per-app effort.
- Apps work as upstream authors designed them — they assume root path of their own domain. Far less per-app patching.
- Maps cleanly to remote access (`photos.cindy.zx9.onmoose.io`) — same pattern everywhere.
- Industry-standard for multi-tenant hosting (Vercel, Netlify, Heroku, Railway).

**Subdomain costs we accept:**
- mDNS doesn't do wildcards — the OS has to publish each hostname individually via Avahi at install time. Plumbing, but well-trodden.
- The names must be **single-label** (`photos.local`, not `photos.moose.local`): Linux's `nss-mdns` rejects multi-label `.local` outright, so a `.moose` infix breaks resolution there entirely (not just on edge-case hardware). This is the single-label-`.local` decision in `DISCOVERY.md` / `DECISIONS.md` (2026-05-31).
- Some old/cheap routers and edge cases (certain iOS quirks, corporate networks) have flaky multi-name mDNS resolution even for single-label names. We mitigate with the optional port-based fallback below.

### Optional port-based routing

- For setups where mDNS multi-name resolution is unreliable (cheap routers, corporate networks, edge-case devices).
- The OS offers an **opt-in fallback:** route apps by port instead of subdomain (`moose.local:8431`, `moose.local:8432`).
- **Isolation is preserved:** different ports are different origins under the same-origin policy, so the security guarantees of subdomain routing carry over to port-based routing.
- Tradeoff: uglier URLs, no friendly names, harder to remember. Strictly opt-in, advanced setting. Default remains subdomain.

### TLS and remote access — see `MOOSE_NETWORK.md`

Networking concerns beyond the LAN — TLS, the onmoose.io apex, cloud DNS, cert issuance, the mesh, device pairing, sharing — live in `MOOSE_NETWORK.md`. The MVP slice is the **hybrid access model**: every app is reachable both at `<slug>.local` (HTTP, mDNS, no cloud) and at `<slug>.<box-id>.onmoose.io` (HTTPS, real Let's Encrypt cert via opt-in cloud enrollment). Mesh / identity-based remote access is captured in the same doc but deferred from v1.

## OS update model

- **v1: plain Debian + apt.** Ship fastest, accept that bad updates can brick the box.
- **Future:** migrate to A/B immutable updates (likely Debian + RAUC/mender, or a switch to an ostree-based base) once the product has traction and the support load justifies the plumbing investment.
- This is a **conscious tradeoff** — we know the pantry-laptop use case is fragile under apt; we're betting that early users tolerate it and that we can migrate before the audience broadens to fully non-technical users.

## Non-goals (for now)

- Multi-node / clustering.
- Power-user storage features (ZFS pools, parity arrays as first-class concepts).
- Replacing Unraid / TrueNAS.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
