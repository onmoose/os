# moose LAN Discovery & mDNS

> Working spec for how moose boxes and their apps are *found* on the local network — hostname resolution, service advertisement, and the publisher daemon. Touches `MOOSE_NETWORK.md` (URL schemes, "Use secure URLs" toggle), `APP_LIFECYCLE.md` (reconciler owns Avahi state alongside Caddy), `STORAGE.md` (Samba/SMB advertisement), `FIRST_RUN.md` (Android-household nudge toward secure URLs), `BOOT.md` (Avahi unit ordering).

> **Environment profiles.** This entire doc is `appliance`-only. The **hosted** profile (cloud VM) has no LAN, no Avahi, and no mDNS — apps resolve through public DNS at `<slug>.<box-id>.onmoose.network`. See `ENVIRONMENT.md` # Networking & discovery (hosted v1).

## Stance

Discovery is the bridge between "the box is on the LAN" and "the user can type a URL and reach an app." For the `.local` URL scheme committed in `MOOSE_NETWORK.md` (`photos.local`, `notes.local`, …) to work, every app slug must be a name the LAN can resolve *before* HTTP routing happens. There is no shortcut: routers don't host our zones, browsers don't synthesize subdomains, and TLS termination at Caddy is moot if the name never resolves.

moose's posture is **Avahi as the LAN nameserver, per-app records published by the reconciler, no client-side magic required.** Discoverability is owned by the brain in the same lifecycle as Caddy site config — install an app, two things get published; uninstall, two things get torn down.

We also accept up-front that **`.local` is a desktop story.** Android browsers do not resolve `.local` at the OS level (see below). The `<box-id>.onmoose.network` HTTPS path in `MOOSE_NETWORK.md` exists primarily to serve that audience; it is the compatibility path, not a premium feature.

## Locked: Avahi as the publisher

`avahi-daemon`, not `systemd-resolved`'s mDNS responder. The reasons:

- Avahi publishes service records (`_http._tcp`, `_smb._tcp`, `_device-info._tcp`) — the things Finder, Windows Explorer, and Linux file managers key off when populating "Network" or "Locations" sidebars. systemd-resolved's mDNS is hostname-only.
- Samba (SMB shares from `STORAGE.md`) integrates with Avahi out of the box for share advertisement, including TimeMachine. Adopting a second responder for hostnames would mean two implementations multicasting on the same socket — at best wasteful, at worst they fight over claim/defend semantics.
- It's what every neighbor in `CLAUDE.md` (Umbrel, CasaOS, Synology under the hood, TrueNAS) runs, for the same reasons. The interop surface is well-trodden.
- ~5 MB resident. Not a footprint concern.

`systemd-resolved` keeps its DNS-stub role; its mDNS responder is disabled at image-build time.

## What we publish

Three categories of records:

### 1. The host record

`moose.local A <lan-ip>` — the box itself. This is avahi-daemon's native per-interface host record, driven by the system hostname — not published by moose code. Avahi announces it at startup and re-announces on link-up and IP change. The dashboard at `https://moose.local` (or `http://moose.local` pre-toggle) resolves through this.

### 2. Per-app A records

For each installed app instance with slug `<slug>`: `<slug>.local A <lan-ip>`. The slug is the *instance* slug: the bare `<base>` is first-come for any scope — so the first Immich installed, household or personal, gets `immich.local`. A personal instance that collides with an existing bare name trails the owner (`<base>--<user>`, e.g. `immich--alex.local`); a colliding household instance gets a numeric suffix (`immich-2.local`). See `DASHBOARD.md` # instance naming and `APP_LIFECYCLE.md` # slug derivation.

**Single-label, on purpose.** The name is `<slug>.local`, *not* `<slug>.moose.local`. The `--` in the slug keeps the user dimension within one label; there is no `.moose` infix. This is load-bearing: a name with a dot before `.local` is *multi-label*, and `nss-mdns` (the Linux mDNS resolver) rejects multi-label `.local` names outright — it never queries the network, so `getaddrinfo` (and therefore `curl`, browsers, every normal client) returns NXDOMAIN. `systemd-resolved`'s mDNS behaves the same way. The earlier `<slug>.moose.local` shape was multi-label and so never resolved on Linux at all. The `.moose` infix also bought nothing: mDNS (RFC 6762) has no zones, delegation, or wildcards (see # Why subdomains can't be wildcarded), so `<slug>.moose.local` was never a *subdomain* of `moose.local` — just a flat name that happened to contain dots, published individually like any other. Single-label `<slug>.local` resolves on every mDNS client (verified: a single-label name published by Avahi resolves through the same Linux `getaddrinfo` path that rejects the multi-label form). The box's own name stays `moose.local`; the `.onmoose.network` HTTPS scheme keeps its hierarchical `<slug>.<box-id>.onmoose.network` shape with its wildcard cert — the two namespaces are resolved by entirely different mechanisms and need not match. See `DECISIONS.md` (2026-05-31).

**Collision fallback.** Single-label names share the flat `.local` namespace with every other device on the LAN, so `photos.local` could clash with, say, a printer. On an Avahi name collision the publisher retries once with a box-qualified name `<slug>-<box>.local` (e.g. `photos-moose.local`, where `<box>` is the box's hostname label). The publish call returns the name that actually won; the reconciler uses *that* returned name for both the Caddy route and the URL shown in the dashboard, so the route and the announcement never disagree. If both the primary and the fallback collide, publish fails and the install surfaces the error (rare; same class as the box `hostname-conflict` issue below).

**Mechanism: Avahi DBus `EntryGroup.AddAddress`, per LAN interface.** The install reconciler calls `org.freedesktop.Avahi.Server.EntryGroupNew`, then `org.freedesktop.Avahi.EntryGroup.AddAddress` once per LAN interface — each call scoped to that interface's index and carrying that interface's own IPv4 (the set comes from NetworkManager; see # Interface scoping) — then `Commit`. Announcing each interface's address on that interface mirrors what avahi-daemon does natively for the box's own host record, and makes "announced a bridge or mesh IP on the LAN" structurally impossible. Uninstall calls `EntryGroup.Free`, which withdraws the announcement. When NetworkManager is unavailable (the dev inner loop), the publisher falls back to a single all-interfaces announcement with one route-probed IPv4.

Static service files (`/etc/avahi/services/*.service`) were the original plan
but were verified not to work for this use case on 2026-05-24: Avahi static
files announce *services*, not bare A-record aliases. Even with the corrected
XML (`<host-name>` inside `<service>`), `avahi-resolve -n <slug>.local`
timed out — Avahi will not publish a standalone A record from a static file.
DBus `EntryGroup.AddAddress` is the only programmatic path. See
`DECISIONS.md` entry 2026-05-24 and `docs/progress/avahi-dbus-publisher.md`.

**Restart durability.** DBus entry groups are process-local: they are lost when
host-agent restarts. The brain re-publishes all running instances at startup via
the existing startup reconcile (`lifecycle.Reconcile`), which already calls
`host.Publish` for every instance in the `running` state. This covers both
"brain restart while host-agent was running" and "both restart together."

**Known gap:** mid-life host-agent restart while the brain is running is not
covered. The brain does not currently detect that host-agent restarted (only
that it is reachable). A future mitigation is to poll `GET /v1/system/status`
for `uptime_s` decreasing and replay on detection. Tracked in
`docs/progress/avahi-dbus-publisher.md`.

### 3. Service records (Bonjour browsing)

For the box itself: `_device-info._tcp` with `model=moose` so it appears with a recognizable icon in Finder. Samba publishes its own `_smb._tcp` and TimeMachine records (`STORAGE.md`).

For individual apps: **not in v1.** We do not publish per-app `_http._tcp` service records. The browse-the-network experience for apps is the dashboard, not the OS file manager. Revisit if compelling use cases appear.

## The reconciler owns Avahi state

This is the load-bearing rule. Install transaction (`APP_LIFECYCLE.md`):

1. Write compose project.
2. Write Caddy site block.
3. Call Avahi DBus `EntryGroup.AddAddress` + `Commit`.
4. Reload Caddy; Avahi multicasts immediately on Commit.
5. Wait for both to be live (Caddy reload returns; Avahi announcement multicasts).
6. Mark app `ready`.

Uninstall does the inverse. Slug rename touches both. If either step fails, the install rolls back both — never leave a Caddy block routing for a name nobody announces, or an Avahi record for a Caddy block that doesn't exist. The two are siblings, lockstep.

**Install latency note.** Avahi announcement takes <1 second on a healthy LAN, but it's a real step — other devices need to multicast-cache the answer before resolution succeeds. The dashboard should not mark an app `ready` until the announcement has been emitted (Avahi's DBus `EntryGroup.StateChanged` → `ESTABLISHED`). Spec'd in `APP_LIFECYCLE.md`'s install-transaction section.

**Caddy upstream Host-header note.** Subdomain routing depends on the original client `Host` (`photos.local`) reaching the app — that's what lets an app distinguish itself from its neighbors. Today this is automatic: the brain builds routes as JSON via the Caddy admin API (`internal/caddy/caddy.go`) and dials plain-HTTP `host:port` upstreams, so Caddy forwards the client `Host` unchanged. Caddy 2.11 (Feb 2026) changed the *default* so that **HTTPS** upstreams instead get the upstream's own `host:port` as the `Host` header. We don't proxy to HTTPS upstreams today, so the change doesn't touch us. If a future app forces an in-container TLS upstream, the route's `reverse_proxy` handler must explicitly set the `Host` header back to `{http.request.host}`, or that app will see the wrong hostname and subdomain routing will break for it.

## Interface scoping

By default Avahi advertises on every interface. We restrict it to **LAN interfaces only** — ethernet and WiFi managed by NetworkManager, not the Headscale mesh interface (where MagicDNS handles naming) and not Docker bridge networks.

Configured in `/etc/avahi/avahi-daemon.conf`:

```
allow-interfaces=eno1,wlp2s0  # actual names resolved at boot
```

host-agent computes the allow-list from NetworkManager's connection state (active ethernet/WiFi connections) at boot and re-computes it on interface add/remove. Avahi has no conf.d include mechanism, so host-agent rewrites the `allow-interfaces` key inside the main conf in place (atomic temp-file + rename; every other line preserved). An allowlist already implies deny-everything-else, so no `deny-interfaces` is written. avahi-daemon reads its conf only at startup — SIGHUP/`--reload` re-reads only static service files — so an allowlist change requires a daemon restart, and a restart destroys every committed DBus entry group, so host-agent always re-publishes all app names afterwards. The allowlist is complementary to per-interface `AddAddress` (see # Per-app A records), not redundant: it also keeps avahi-daemon's *native* host record and the SMB advertisement off the mesh and Docker bridges, which per-app entry groups can't influence. Spec'd in `BRAIN_HOST_PROTOCOL.md` as part of the network-state surface.

## Why subdomains can't be wildcarded — and what we do instead

mDNS (RFC 6762) has no central server and no zone file. Each device announces *exactly* the names it owns; resolvers ask "who has this exact name?" and accept the first answer. There is no wildcard synthesis. RFC 6762 §12 doesn't define wildcard semantics, and no major resolver implements them.

Three options were considered:

- **Per-app A records, published on install** (option A, chosen). Works on every mDNS-capable client. Symmetric with Caddy reconciliation. Scales easily to ~100 apps; multicast announcement traffic is negligible at that scale.
- **CNAME `<slug>.local` → `moose.local`** (option B, rejected). CNAME-following over mDNS is under-specified. Apple's mDNSResponder follows them, Windows Bonjour mostly does, systemd-resolved has had bugs, and several Linux NSS implementations don't. Compatibility loss with no upside — the reconciler still has to publish each CNAME, identical work to publishing A records.
- **Single A record + Host-header routing only** (option C, rejected). Requires the browser to resolve the subdomain before it can send a `Host:` header. mDNS doesn't resolve names that weren't announced, so the request never leaves the client. The model assumes resolution is solved; on the LAN, it isn't.

## Client compatibility

| Client | `.local` resolution | Notes |
|---|---|---|
| macOS (Safari, Chrome, Firefox, Finder) | ✅ Native | mDNSResponder built-in; this is the gold path. |
| iOS (Safari, Chrome, Files.app) | ✅ Native | Same stack as macOS. |
| Windows 10/11 (Edge, Chrome, Explorer) | ⚠️ With Bonjour | Native support is partial and version-dependent. Bonjour Print Services (free Apple download, also shipped by iTunes/Adobe) is the reliable path. **First-run dashboard should detect Windows without Bonjour and link to the installer.** |
| Linux desktop | ✅ With `nss-mdns` | Resolves single-label `.local` names; `nss-mdns` is almost universally installed. **Caveat that drove the naming:** it rejects *multi-label* `.local` (e.g. `x.moose.local`) outright — instant NXDOMAIN, no network query — which is exactly why app names are single-label `<slug>.local`. See # Per-app A records. |
| **Android (any browser)** | ❌ Not at OS level | NSD is an app-API, not a system resolver. Browsers don't use it. `.local` URLs return NXDOMAIN. **No workaround at moose's layer.** |
| Chromecast / smart TVs / IoT | Varies | Out of scope for v1 user-facing URLs. |

## The Android problem, explicitly

Android does not wire mDNS into `getaddrinfo`. A browser query for `photos.local` is sent to the configured unicast DNS server (the router), which returns NXDOMAIN. There is no fallback. This is by design — Google has battery, multicast-on-WiFi-cost, and security reasons, and their preferred discovery model is cloud-mediated (Cast, Nearby).

Implication for moose: **households with Android users need `<box-id>.onmoose.network` HTTPS URLs** (`MOOSE_NETWORK.md`), where resolution goes through public DNS. The "Use secure URLs" toggle is, in practical user-facing terms, the *"my household has Android devices"* toggle.

Two follow-ups:

- **First-run nudge** (`FIRST_RUN.md`): the wizard asks "Will anyone in your household use this from an Android phone?" If yes, flip the secure-URLs toggle and explain why. Avoids the "works on my laptop, broken on my partner's phone" support ticket.
- **Dashboard hint**: when a member is added and their first dashboard session comes from an Android User-Agent while secure-URLs is off, surface a one-time banner to the admin: "Your household includes an Android device. Enable secure URLs so apps work everywhere."

## Failure modes & known gotchas

These are not bugs we can fix in moose's code, but support-load realities to anticipate. Each one wants an entry in the diagnostic bundle (`LOGGING.md`) when relevant.

- **AP isolation / client isolation** on consumer routers silently blocks multicast between clients. Common on guest WiFi, occasional on default home configs. Symptom: box pings fine by IP, `.local` doesn't resolve from any client. Diagnostic bundle should include a "multicast probe" — broadcast a known query, count responses; zero responses with the box on the same subnet strongly implies AP isolation.
- **`.local` collision with Active Directory.** Some SOHO/corporate networks use `.local` as an internal AD domain. Their unicast resolver claims `.local` queries and Avahi never gets asked. Rare in our audience but real. No fix; document and let the user switch to secure URLs.
- **VLAN segmentation.** Multicast doesn't cross VLAN boundaries by default. Box on one VLAN, clients on another, no discovery. User-network-design issue; out of scope for v1 to detect.
- **IPv6 link-local.** Avahi publishes both A and AAAA by default. Some older clients get confused. We leave the default on; if reports surface, we have the `use-ipv6=no` knob.
- **Multiple `.local` responders on the LAN.** A second moose box, a Synology, a printer — all happily coexist (each owns its own names). The only collision case is two devices claiming `moose.local`; Avahi's RFC 6762 §9 conflict-resolution renames the loser to `moose-2.local`. This is correct behavior but produces a confusing URL change. The dashboard surfaces a typed health issue (`hostname-conflict`) when Avahi reports a rename; admin can pick a different hostname from Settings → System → Network.
- **IP change without link-down.** Rare (manual DHCP server change, router swap). Avahi's announce-on-link-up doesn't fire, and committed DBus entry groups hold the literal old address — `avahi-daemon --reload` fixes neither (it only re-reads static service files). host-agent watches NetworkManager over DBus and re-publishes every entry group with the current per-interface addresses on any network change (debounced ~2s).

## What we explicitly don't do

- **No wildcard records.** Doesn't exist in mDNS; not worth trying to fake.
- **No private CA for `.local` HTTPS.** `.local` + Let's Encrypt is impossible (no public DNS). Installing a private CA on every client device is a non-starter for our audience. `.local` is HTTP-only by definition; HTTPS goes via `<box-id>.onmoose.network`. Stated here to forestall the "just ship a private CA" suggestion.
- **No DNS-SD service catalog for apps.** Dashboard is the browse surface, not Finder's "Network" sidebar. We may revisit if a use case emerges.
- **No LLMNR.** Microsoft's old multicast-name protocol. Modern Windows leans on mDNS-with-Bonjour; LLMNR doesn't carry service records and is being phased out for security reasons. Not worth supporting.

## Cross-references

- `MOOSE_NETWORK.md` — URL schemes, secure-URL toggle (the Android compatibility path).
- `APP_LIFECYCLE.md` — install/uninstall transaction includes Avahi state alongside Caddy.
- `STORAGE.md` — Samba/SMB advertisement via Avahi; TimeMachine records.
- `FIRST_RUN.md` — Android-household nudge during wizard; Windows-Bonjour link.
- `BOOT.md` — `avahi-daemon.service` ordering; not on the critical path for dashboard availability.
- `HEALTH.md` — `hostname-conflict` typed issue when Avahi renames our host.
- `BRAIN_HOST_PROTOCOL.md` — host-agent computes the Avahi interface allow-list from NetworkManager state.
- `LOGGING.md` — multicast probe in diagnostic bundle.

## Open

Tracked in `NEXT.md`. The notable ones:

- **Multicast probe in the diagnostic bundle** — exact shape, what we measure, how we present the AP-isolation suspicion to admins.
- **Per-app `_http._tcp` service records** — whether listing apps in Finder/Explorer Network sidebars is worth the complexity. Default no; revisit if requests appear.
- **Hostname rename UX** — when Avahi conflict-resolves us to `moose-2.local`, how aggressively the dashboard prompts for a fix vs. lets it ride.
- **Windows Bonjour detection** in first-run — User-Agent isn't reliable; consider a JS-side mDNS probe instead.
