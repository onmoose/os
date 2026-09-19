# onmoose.io

> The cloud-side surface that supports the moose OS. Companion to `SPEC.md`. Owns DNS, certs, enrollment, and (deferred) the identity-based mesh for remote access.

> **Environment profiles.** This doc describes the `appliance` profile, where `.local` is the foundation and `<slug>.<box-id>.onmoose.io` HTTPS is an opt-in toggle. In the **hosted** profile (cloud VM) there is no `.local` and no toggle: `<slug>.<box-id>.onmoose.io` public HTTPS is always-on and the only scheme, enrolled automatically at provision. The wildcard-cert ACME DNS-01 mechanism is the same. See `ENVIRONMENT.md` # Networking & discovery (hosted v1).

## Scope

onmoose.io is a small set of cloud services run by moose, used by every box that opts in. It is **not** a control plane for boxes — it never sees app traffic or app data, only DNS lookups and cert-renewal metadata.

What's in the **MVP slice** of onmoose.io:

- A moose-owned apex domain (e.g. `onmoose.io`).
- An authoritative DNS service for that apex.
- An enrollment API: box gets a per-box subdomain at first-run, plus credentials to drive ACME DNS-01.
- Caddy on each box renews real Let's Encrypt certs against this DNS.

What's **deferred** (mesh / remote access via Headscale + DERP, device pairing, ACL UI, mobile clients) is documented below for continuity but is not in v1.

## Locked: one URL scheme at a time, governed by a global toggle

Every app is **always reachable at `<slug>.local`** over plain HTTP. That route is the foundation; it works with no internet, no enrollment, no cloud dependency. The LAN name is single-label by necessity — `DISCOVERY.md` # Per-app A records explains why it is not `<slug>.moose.local`. The two schemes deliberately differ in shape: the flat `.local` mDNS namespace can't carry the `<box-id>` hierarchy, while the unicast `.onmoose.io` side needs it for the per-box wildcard cert. The `<slug>` prefix stays consistent across both.

If the user enrolls with onmoose.io and turns on the **"Use secure (HTTPS) URLs for my apps"** toggle, the brain additionally registers `<slug>.<box-id>.onmoose.io` for every app and the dashboard switches to surfacing those URLs everywhere. The toggle is **all-or-nothing**: every app shows the same scheme, no per-app routing override.

| URL                                       | Scheme | Resolution                      | Cert                      | When active                                |
|-------------------------------------------|--------|---------------------------------|---------------------------|--------------------------------------------|
| `<slug>.local`                            | HTTP   | mDNS on the LAN (Avahi)         | none                      | Always (the foundation)                    |
| `<slug>.<box-id>.onmoose.io`           | HTTPS  | moose cloud DNS → box's LAN IP  | Let's Encrypt wildcard    | Enrolled + toggle on; surfaces in the UI   |

When the toggle is on, the `.local` routes remain installed in Caddy as a fallback (so power users can still type `photos.local` directly) — but the dashboard, tile clicks, copy-link buttons, and bookmarks all show `.onmoose.io`.

See `DECISIONS.md` (2026-05-14) for why we collapsed from the earlier "two URLs always visible" model to this one.

### Why hybrid and not one or the other

- **HTTP-only on `.local`** alone is cheap but cripples apps that need HTTPS-gated browser APIs (camera, mic, clipboard, PWA install, service workers, secure cookies) and trains users to ignore browser warnings. Bad long-term posture.
- **Internal CA** alone forces every device — phone, laptop, family member's phone — through a "install moose's root cert and trust it" flow. iOS profile install is the single worst UX moment we could ship. Side-steps the cloud entirely but at a UX cost we don't want.
- **HTTPS-only via the cloud URL** alone is what HexOS tried; users pushed back hard on the "I'm on my own LAN, why does this need your servers?" feel. Even though moose's cloud would be DNS-only (never a traffic proxy), the optics of cloud-mediated LAN access aren't worth the principle.
- **Hybrid** keeps `.local` HTTP as the always-works fallback (no internet, no cloud, no per-device setup) and offers `.onmoose.io` HTTPS for users who want real certs and the modern web's full feature set. Privacy-strict users can decline enrollment and never touch the cloud; the box remains fully functional.

### `.local` is a desktop URL scheme — Android needs the cloud path

`.local` resolution requires the OS to wire mDNS into `getaddrinfo`. macOS, iOS, Linux (with `nss-mdns`), and Windows (with Bonjour installed) all do this. **Android does not**, and there is no app-layer workaround — Android's NSD is an API for apps that want to *browse* for services, not a system resolver browsers can use. A user typing `photos.local` into Chrome on an Android phone gets NXDOMAIN.

This makes the secure-URLs toggle the **Android compatibility path** in practice. The label doesn't say that, but the first-run wizard asks about it directly ("Will anyone in your household use this from an Android phone?") and flips the toggle pre-emptively for yes. See `FIRST_RUN.md` and `DISCOVERY.md` for the full client-compatibility matrix and gotchas (AP isolation, `.local` AD collisions, hostname conflict).

### What cloud actually does in this model

- Resolves `<box-id>.onmoose.io` to the box's LAN IP. **No traffic ever traverses moose's servers.**
- Sets DNS TXT records on demand to satisfy Let's Encrypt's ACME DNS-01 challenge, so the box can renew real certs without exposing port 80/443 to the internet.
- That's it. Cloud is a name-resolver and a cert-issuance helper. It's not a proxy, not a tunnel, not a relay (mesh relays come later, separate piece).

## The toggle

The toggle lives in **Settings → Network** and is the single control that determines what URL scheme the user sees.

**Label:** *"Use secure (HTTPS) URLs for my apps"*
**Sub-line:** *"Required by some apps (cameras, password managers, PWAs). Also the foundation for remote access (coming later)."*

We deliberately don't call this "make apps available outside your network." That phrasing was considered and rejected — in MVP `.onmoose.io` only resolves to the box's LAN IP, so it doesn't grant remote reach yet, and over-promising on this label is the exact failure mode HexOS hit. Honest framing primes the user for the real remote-access feature when it ships.

**State table:**

| Enrolled? | Toggle | Effect                                                                                              |
|-----------|--------|-----------------------------------------------------------------------------------------------------|
| No        | n/a    | Toggle disabled. Inline "Enroll your box to enable HTTPS URLs" with a one-click enroll button.      |
| Yes       | Off    | Dashboard shows `.local` URLs. `.onmoose.io` certs renew quietly in the background.              |
| Yes       | On     | Dashboard shows `.onmoose.io` URLs everywhere. `.local` still works if typed directly.           |

Flipping the toggle is **the only origin transition** a normal user encounters. They re-authenticate to the dashboard once on the new scheme; same for each app on next visit. Accepted as a deliberate "switch modes" action — see `DECISIONS.md` (2026-05-14) for why we dropped the cross-scheme session-handoff idea.

### Apps that need a secure context

Some apps need real HTTPS to function — typically because they use browser APIs gated on a secure context (camera, mic, clipboard, service workers, PWA install, secure cookies, WebAuthn). The manifest declares this with `needs_secure_context: true` (see `APP_MANIFEST.md`).

The brain uses this field as a **warning trigger at install time**, not a routing override:

- Toggle on: install proceeds silently. The app's URL is HTTPS, the app works.
- Toggle off (or not enrolled): install dialog shows a warning — *"This app uses features that need HTTPS to work fully (camera, etc.). It may not work correctly at its `.local` URL. Turn on secure URLs in Settings → Network."* The user can install anyway. Some apps degrade gracefully on HTTP; that's the app's call to make, not moose's.

No install hard-block. We respect user agency; the warning is informative, not gating.

### Where remote-access discovery will live (deferred)

When the mesh ships and `.onmoose.io` URLs *do* become reachable remotely, discoverability is its own feature surface — **not** a clever URL substitution. Concretely:

- A **"Remote access" section in Settings** that lists every URL the user can reach from outside, plus which devices/people are paired.
- A **"Open from outside home" affordance per app** (button or menu item) that copies the `.onmoose.io` URL with a tooltip explaining it only works on paired devices.
- The **sharing flow** generates the remote URL as part of pairing — the recipient gets a link that works for them because pairing happens in the same act.

This is the path Tailscale, Plex, and Synology all use. URL switching in the address bar is not the right discovery mechanism — a real feature with a real button is.

### Rejected: showing both URLs in the dashboard

Considered listing both URLs per app, labeled "On this network" and "Secure URL". Rejected because:

- It surfaces `.onmoose.io` as a user-facing URL without any of the affordances that would justify it (no remote reach in MVP, no clear "this is for HTTPS apps" framing).
- Doubles the cognitive cost of every app tile.
- The "two URLs to share with my partner — which one?" decision lands on the user every time.

### Rejected: context-aware URL switching

Considered showing `.local` when the dashboard is accessed via `.local`, and `.onmoose.io` when accessed via `.onmoose.io`. Rejected because:

- **Bootstrap problem.** First-time user is on LAN, sees only `.local` URLs, never learns `.onmoose.io` exists. They leave home, can't reach the dashboard at all (MVP `.network` resolves to LAN IP), and there's no remote URL to fall back to that they ever saw.
- **In MVP there is no remote case.** `.onmoose.io` is only ever accessed from LAN today. The "switch on remote context" branch has nothing to switch to.
- **Same-house cross-device confusion.** Phone on cellular at the kitchen table vs. laptop on wifi see different URLs for the same app. Sharing between devices becomes "wait, which URL did I copy?"
- **Discovery should live in a feature, not in URL machinery.** When remote access exists, it gets its own surface (above). Until then, the simpler model is correct.

### Rejected: per-app routing override (transparent HTTPS for `needs_secure_context` apps)

A previous version of this doc had the brain silently send `requires_https` apps to `.onmoose.io` while keeping every other app on `.local`. Rejected because:

- The user lands on a different origin without knowing it — different cookies, different session, different sharing semantics from the neighboring app tile, no UI cue.
- Made the dashboard's URL story unpredictable: "why does this app have a different domain than that one?"
- The brain doesn't actually know what the user wants. A global toggle puts the decision in the user's hands explicitly.

See `DECISIONS.md` (2026-05-14).

### Rejected: hard-blocking install of `needs_secure_context` apps on un-enrolled / toggle-off boxes

Previously specified as "the app cannot be installed until the user enrolls." Rejected because:

- Paternalistic. Many `needs_secure_context` apps degrade gracefully on HTTP (e.g., a notes app uses clipboard API for "copy to share" but is otherwise fine); the user can judge.
- The warning at install time communicates the constraint clearly without taking the choice away.

## Enrollment flow (first-run)

Enrollment is opt-in. The first-run wizard surfaces it with plain-language framing:

> *Enrolling gives every app a secure URL like `photos.cindy-fox.onmoose.io`. Your data never goes through moose's servers — only DNS lookups do. You can skip this and access your apps at `photos.local` instead.*

If the user enrolls:

1. Box generates a keypair, sends an enrollment request to the enrollment API.
2. Wizard shows a **"Name your box"** screen with a base-name text field (e.g. `cindy`) and a system-assigned suffix shown as static text next to it (e.g. `-fox`). A reshuffle die (`🎲`) picks a different suffix; reshuffles are unlimited.
3. API checks availability of the `(base, suffix)` pair. On collision, auto-rerolls the suffix and shows the new combo. Reserved bases (moose-internal slugs, crude words) are rejected with a clear message; the user types another.
4. Once accepted, API returns the box-id (`<base>-<suffix>`) and an API token. Box persists both.
5. Box phones the API to set an A record: `*.<box-id>.onmoose.io` → box's LAN IP.
6. Caddy on the box uses ACME DNS-01 (via a moose-provided plugin or generic API) to obtain a wildcard cert for `*.<box-id>.onmoose.io`. Renewal every ~60 days.

If the user declines:

- mDNS publishing for `.local` still happens.
- The box never contacts onmoose.io for anything.
- The "Use secure URLs" toggle in Settings → Network is disabled, with an inline "Enroll your box to enable" affordance.
- Apps that declare `needs_secure_context: true` (see `APP_MANIFEST.md`) install fine but show a warning that some features may not work over HTTP.
- The user can enroll later from Settings → Network at any time.

### Locked: box-id is base + curated suffix, joined by a dash

The box-id is two parts joined by a dash: a **user-chosen base name** and a **system-assigned suffix** drawn from a small curated list. Examples: `cindy-fox`, `the-perez-family-pine`, `larry-raven`.

**The base** is what the user types. Validation is permissive: lowercase letters and digits, single internal dashes, not starting or ending with a dash, reasonable length cap. A **reserved-base blocklist** rejects names that would collide with moose-internal slugs (`photos`, `home`, `mail`, etc.) and a small set of crude words that combine poorly with any suffix. Not a generic profanity filter — a targeted list, kept small.

**The suffix** is system-assigned at enrollment, never typed. It's drawn from a hand-curated list themed around **Nordic nature** — concrete nouns only, no adjectives, no verbs: animals (`fox`, `elk`, `owl`, `raven`, `wolf`), plants (`pine`, `oak`, `birch`, `moss`, `fern`), geography (`bay`, `cove`, `fjord`, `lake`, `dune`). 3–5 characters. **~50 entries at launch.** The semantic neutrality is load-bearing: a concrete-noun suffix can't combine with the base to form an unintended phrase, which is the failure mode of free-typed suffixes and adjective-noun pairs.

The wizard renders the suffix as static text next to the base field, with a **🎲 reshuffle** affordance that picks a different one. **No cap on reshuffles** — the pool is curated to be semantically safe, so grinding produces nothing worth grinding for.

The actual word list lives outside this spec (data, not design). Curation policy: concrete nouns only, Nordic-nature theme, 3–5 chars, no homophones with crude words, no trademarked terms, no ASCII-confusables.

**Why this shape:**

- **Dash, not dot.** Industry precedent for `<name>-<random>` PaaS URLs (Vercel, Netlify, Heroku, Render, Fly) is uniformly dash. Dot is reserved for hierarchically distinct labels (e.g. `<worker>.<account>.workers.dev`). Our suffix is anti-collision plumbing, not a namespace.
- **Curated, not generated.** A pronounceable-nonsense generator (`cindy-zoki`) would also work and avoids list maintenance — but curated suffixes feel intentional, verbalize cleanly, and quietly carry moose's Nordic identity. ~50 entries at launch is an afternoon; growth is on the order of ~10/year.
- **System-assigned, not typed.** Letting users type the suffix re-opens squatting (`bob-001`...`bob-999`) and re-introduces the second naming negotiation we removed by adding the suffix in the first place. Reshuffle gives aesthetic choice without giving targetable strings.
- **No paid "drop the suffix" tier.** Considered (`larry.onmoose.io` as an upgrade). Rejected — willingness-to-pay is low, the suffix doesn't bleed enough to upsell, and pay-gating cosmetics doesn't fit the monetization shape (paid SKUs are off-site backup, relay bandwidth, paid apps).

**Collision capacity.** ~50 suffixes × any base means each (base, suffix) pair is unique within the namespace; the brain rejects pair-level collisions at enrollment and auto-rerolls. Even at 100k-box scale, the most popular base name is expected to see low-thousand assignments — each suffix shoulders a manageable share. The list grows if real assignments outpace forecasts.

### Locked: pick the name at enrollment, no rename afterward

The user names their box at first-run enrollment. After that, the box-id is frozen for the life of the install.

**Why pick at enrollment:** the moment the user cares is when they first see the suggestion. Giving them a chance to type something memorable then is cheap and meaningful. Tailscale handles tailnet naming the same way.

**Why no rename afterward:** every alternative we considered has real costs.
- DNS TTL propagation means the old name keeps resolving for the TTL window.
- Cert reissuance for the new wildcard adds a renewal cycle.
- Bookmarks and previously-shared links break silently.
- Audit/identity surfaces ("this device was registered under `cindy-zx9`") need historical mapping.

If a user truly needs a different name, the supported path is **re-enrollment**: pick a new name, old subdomain decommissions, the box is told its old URLs no longer work. Same operational cost as a fresh box, no half-state. Locking the name out of the rename surface removes a class of "I changed the name and now nothing works" tickets we'd otherwise own.

### Failure modes — explicit

Chosen specifically so that none of these break the box:

- **Cloud is down.** `.local` HTTP keeps working. Cached certs serve `.onmoose.io` for up to ~30 days past last successful renewal.
- **Internet is down.** Same as above. mDNS doesn't need internet.
- **Box LAN IP changes (DHCP).** Box detects, re-registers via the enrollment API. <1-minute DNS staleness window for already-cached resolvers.
- **Cert expires (long offline period).** Caddy keeps serving the expired cert (browsers will warn) — or we transparently redirect the cloud URL to `.local` until renewal succeeds. Decided when we implement.
- **User wants out.** Deleting the enrollment from the box revokes the DNS record. Optional "wipe my box-id" call to the API.

### Honest sharp edges

- **Toggle-flip re-auth.** Flipping "Use secure URLs" changes the origin of every app and the dashboard. The user re-authenticates to the dashboard once on the new scheme, and to each app on next visit. Acceptable: it's a deliberate mode switch, not a per-click cost. No session handoff in v1 — see `DECISIONS.md`.
- **`.onmoose.io` is not remote access in MVP.** It resolves to a LAN IP, unreachable when you leave home. The toggle's sub-line copy is honest about this; the underlying name will become remotely reachable when the mesh ships.
- **Cloud DNS sees metadata.** We learn box-ids exist and which devices query them. Standard for any cloud-resolved name. Privacy doc must be explicit; we don't see content, payloads, or traffic.
- **Roaming network = stale DNS.** If the box moves networks (LAN IP changes), brief window of "site not found" on devices with cached old IP. Acceptable.
- **Caddy config doubles when toggle is on.** Two route blocks per app (`.local` HTTP fallback + `.onmoose.io` HTTPS active). Marginal overhead; the brain manages both.
- **`needs_secure_context` apps on a toggle-off box may misbehave.** User was warned at install time. The app loads, the broken feature degrades visibly. No runtime indicator beyond what the app itself shows.

### Knock-on to other docs

- `APP_LIFECYCLE.md` # "Caddy route registration timing" — Caddy registration step always creates the `.local` HTTP route; adds the `.onmoose.io` HTTPS route if the box is enrolled. The dashboard URL surfaced per app is determined by the global toggle, not per-app manifest fields.
- `FIRST_RUN.md` — wizard's enrollment step pairs naming the box with turning on secure URLs. For new users they are effectively the same choice.
- `APP_MANIFEST.md` — adds `needs_secure_context: bool` (optional, default false). The field triggers a warning at install time when the user is on `.local`; it is not a routing override.

### Bring your own domain

Supported as a first-class alternative to the onmoose.io subdomain.

- User points `home.theirdomain.com` at the box.
- Box runs ACME (HTTP-01 if a port is exposed, DNS-01 with a supported provider) for cert issuance.
- For users who want to avoid any moose cloud dependency entirely.
- Treated as another "secure URL scheme" for the toggle: when the user has a custom domain, the toggle surfaces `<slug>.<custom-domain>` instead of `.onmoose.io`. `.local` remains the off-state.

---

## Deferred: remote access via mesh

Everything below is **not in MVP.** Captured here so the design is centralized and we don't relitigate when we pick it up. The DNS+certs slice above is independent and ships first.

### Security posture — closed by default

A defining principle for moose; worth being loud about as a product position when remote access ships.

- **No app or service is publicly exposed.** Period. There is no "share this app with the world" toggle in v1.
- **Access is granted to specific devices and users**, Tailscale-style. The user enrolls each phone/laptop they own into a private mesh tied to their moose box. Only enrolled devices can reach the box.
- **Sharing with other people** (show grandma your photos): grandma installs the moose client on her phone, gets invited to the mesh with **scoped permissions** — only the apps she's been granted, time-limited, revocable.
- **Pairing flow is the central remote-access primitive.** Scan QR / enter code; the device joins the mesh.
- **Public-internet exposure** (open ports, anyone with the URL can hit the login page) is **not a default option.** It's available only as an explicit advanced setting for users who BYO domain and consciously accept the risk.

**Why this matters:** the "open a port to the internet so my partner can see the photos" pattern is how home servers get owned in 2026. A single CVE in any self-hosted app exposes the whole box. Identity-based mesh access flips the model — attackers can't even attempt a connection unless they're already on the user's mesh. Reduces the attack surface to ~zero for the default user.

### Why a relay is needed despite having DNS

The onmoose.io DNS gives boxes a *name*. It does not give them *reachability* — those are two separate problems, and the second is much harder. A relay (or NAT-traversal mesh) is needed because for a meaningful share of users, the box is simply unreachable from the public internet, no matter what DNS says.

Reasons direct connections fail:

1. **CGNAT (Carrier-Grade NAT).** Many ISPs — almost all mobile/cellular, an increasing share of European and Asian residential broadband, much of LATAM — put thousands of customers behind a *shared* public IP. The "public IP" isn't actually the user's. There's no port-forward path because no port is theirs. From outside the ISP, the box is unreachable, full stop.
2. **No port forward configured.** Even on a real public IP, the user's router blocks unsolicited inbound traffic by default. Non-technical users will not configure port forwards. Many ISPs disable router admin entirely.
3. **Symmetric NAT.** Some routers (mostly enterprise / mobile carrier) refuse hole-punching. WireGuard / STUN / ICE can't get through.
4. **IPv4/IPv6 mismatches.** Visitor on IPv6-only, box on IPv4-only (or vice versa). DNS returns an address neither side can reach.
5. **Dynamic IP rotation.** ISP changes the box's public IP; DDNS is updating; for a few minutes the DNS record is stale.

### How the mesh works

Architecture is Tailscale-shaped:

1. Both peers (visitor's phone, moose box) connect *outbound* to a coordination server. Outbound works through almost any NAT.
2. The coordination server tells each peer the other's address candidates.
3. Both peers attempt **direct UDP hole-punching** simultaneously. ~85–95% of the time this works and traffic flows direct, peer-to-peer, never touching moose's servers.
4. If hole-punching fails, both peers fall back to a **relay**. The relay forwards encrypted packets between them — it can't read the contents, just the wrapper.

Important properties:

- **Relay is fallback, not default.** Most traffic never touches it.
- **Relay traffic is encrypted end-to-end.** The relay sees noise.
- **Relay is the only way to reach CGNAT users at all.** Without it, those users have no remote access — period.
- **The mesh is identity-based.** Relay or direct, only enrolled devices can establish a connection. The relay is just a packet shuttler between two already-authenticated peers; it doesn't expand attack surface.

### Mesh stack — Headscale + WireGuard

We need a coordination service to make the identity-based mesh work — keys exchanged, devices registered, ACLs enforced, NAT-traversal hints distributed. Tailscale itself is the obvious reference, but their coordinator is closed-source SaaS and we don't want product/brand dependency on another company. We **self-host the coordinator** instead.

**License survey:**
- **Tailscale.** WireGuard core, Linux/iOS/Android client cores, and the DERP relay protocol/server are open source. The **coordination server is proprietary**, as are the Windows/macOS GUI apps. We can use Tailscale as SaaS but cannot host it. Rejected on product-identity grounds.
- **Headscale.** Open-source re-implementation of Tailscale's coordination server. **BSD-3 license, no commercial restrictions.** Mature, actively maintained, originally written by an ex-Tailscale engineer. Speaks Tailscale's wire protocol — official Tailscale clients work against it, useful as a fallback during dev.
- **NetBird.** Full-stack Tailscale alternative. Client is BSD-3, **server components (management, signal, relay) are AGPLv3.** AGPLv3 forces us to publish any modifications we serve over the network — meaningful friction for a commercial product where we'll iterate on the management server. Viable but worse fit than Headscale.

**Decision (deferred but locked-in-direction): Headscale + DERP, both BSD-3.** Clean license, mature, no surprises.

**What we'd run on onmoose.io for remote access:**
- **Headscale coordinator.** Single Go binary, SQLite or Postgres backend. Small VPS. ~$10/mo can serve many thousands of boxes at this layer.
- **DERP relay fleet.** A few cheap geographically-distributed VPSes running Tailscale's open-source DERP server. Used only when peer-to-peer hole-punching fails (~5–15% of connections).
- **Enrollment API extension** — the MVP enrollment API gets extended to also issue Headscale pre-auth keys when a box opts into remote access.

**What we'd build:**
- **moose client apps** (iOS, Android, later desktop) wrapping `wireguard-go` or `boringtun` (both BSD) for the data plane, speaking Headscale's API for the control plane.
- **Pairing UX** — QR codes, magic links, scoped invitations.
- **ACL UI** — Headscale supports ACLs via YAML config; we build the friendly "who can see what" interface on top.

**Trust model:**
- Coordinator is online but **blind to app traffic.** It sees public keys, addresses, ACL config — never the data flowing between peers (WireGuard-encrypted end-to-end).
- DERP relays are **even more blind** — they forward encrypted UDP packets without keys to read them.
- **Box ↔ coordinator auth** is a per-box keypair, generated at first boot. Compromising the coordinator could let an attacker add bogus devices to a tailnet, but cannot decrypt existing traffic.
- Pairing tokens and magic links are **short-lived and single-use** to limit interception risk.

### Device pairing flows

#### Pairing your own device (phone, laptop)

1. On the moose web UI (LAN), user clicks **"Add a device"**. A modal shows a QR code containing a single-use pairing token (5-minute TTL).
2. User installs the **moose app**, taps **"Pair with my moose"**, scans the QR.
3. The phone sends the token to the onmoose.io coordinator. Coordinator validates, registers the phone's public key under the user's tailnet, and returns the box's address candidates plus an ACL granting access to the user's apps.
4. Phone establishes a WireGuard tunnel — direct via hole-punching if possible, via DERP relay otherwise.
5. Done. `photos.cindy-fox.onmoose.io` now resolves and is reachable from anywhere with a network connection.

#### Sharing with another person (e.g. grandma sees Photos)

1. In Photos, user clicks **"Share with someone"**, enters grandma's name, picks scope (just Photos / Photos + Grocery / etc.) and optionally an expiration.
2. moose generates a **magic link** encoding a scoped, time-limited, revocable invitation token.
3. User sends the link via any channel (SMS, email, Signal — moose doesn't care).
4. Grandma installs the moose app, taps the link, completes pairing.
5. Grandma's device joins the user's tailnet under a **restricted ACL** — she can reach only the apps she was granted; she's invisible to other tailnet members; her permissions can be revoked from the **"People with access"** UI at any time.

### Build vs. buy — honest scope for remote access

The hard part of this stack isn't the coordinator. **It's the client apps**, especially mobile. Native-feeling apps for pairing, ACL display, and "what's reachable right now" take real engineering — Tailscale has spent years polishing theirs. A few realistic scope reductions for the first remote-access release:

- **Mobile-first.** Ship iOS + Android moose clients. Cover 90% of remote-access use cases.
- **Defer native desktop clients.** Power users can install official Tailscale clients pointed at our Headscale (works, but rough UX). A polished moose desktop client comes later.
- **Web fallback for casual access.** Even without a moose client, an enrolled-and-shared user can reach apps via a web interface served by the box (proxied through the relay). No app install needed for "just let me see the photos once."
- **Use battle-tested primitives.** `wireguard-go` and `boringtun` are stable. Don't reinvent the data plane.

---

## Cost analysis

For the free baseline (DNS + certs in MVP, mesh later). Off-site backup is paid and not part of this layer.

**DNS + cert issuance (MVP):**
- `onmoose.io` domain: ~$15/year, one-time.
- DNS hosting on Cloudflare's free tier: $0 (unlimited records, free API at home-server scale). Self-hosted PowerDNS/CoreDNS on a small VPS as alternative: ~$5/mo.
- Enrollment API (mints subdomains, authenticates boxes via per-box keypair, sets TXT records for Let's Encrypt DNS-01 renewal): small Go service on a $5–10/mo VPS, or Cloudflare Workers (effectively free at this scale).
- Cert renewal cadence: every 60 days per box, fully automated.
- **Total at 10k boxes: ~$30–60/month all-in.** Per-user cost: well under $0.10/year. Treat as fixed overhead.

**NAT-traversal mesh (deferred):**
- ~85% of traffic: direct peer-to-peer, costs us nothing.
- ~15% of traffic: relayed, costs ~$0.005–0.01/GB on commodity bandwidth.
- Typical home-server usage (photo browsing, file access, dashboard checks) is light; average per-user relay traffic is small.
- **Tailscale operates their global DERP fleet for a multi-million-user free tier on what's likely low five figures/month.** At our scale, expect bandwidth in the **low hundreds/month** for many thousands of users. Manageable as brand investment.

---

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
