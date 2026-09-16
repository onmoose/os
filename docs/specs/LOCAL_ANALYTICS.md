# moose Local Analytics

> Working spec for the analytics view of a moose box, surfaced to its owner. **Nothing in this doc leaves the box.** Sibling to `TELEMETRY.md` (project-side telemetry, opt-in, sent off-box). Touches `LOGGING.md` (audit events overlap), `WEB_UI.md` (dashboard surfaces), `AUTH.md` (per-user visibility rules), `CONTROL_PLANE.md` (brain is the producer and the store).

## Stance

Two distinct user-facing analytics surfaces, both local-only:

1. **Per-user historical analytics.** "What's happened on my box over time" — app usage, sign-ins, install/update history, per-app storage trends, drive-fill forecast. Persisted in the brain's SQLite. Surfaced as per-user dashboards plus an admin aggregate.
2. **Real-time system resources.** "What's my box doing right now" — host CPU, RAM, network, disk IO. Live SSE stream, **only while a UI is watching**, nothing persisted. Available to every user via a top-bar dropdown.

Both are product features the user gets for free. No opt-in, no toggle — it's data about their own hardware on their own hardware.

## Principle: data about your box stays on your box

The bright line between this doc and `TELEMETRY.md` is mechanical: nothing in `LOCAL_ANALYTICS.md` ships any byte off the box. If a future feature wants to send any of this data out, it lands in `TELEMETRY.md` and gets the opt-in toggle. We don't blur the line.

## Privacy model: per-user only

The household-trust threat model is "I share a box with my partner and kids; I don't want to see what they're doing." That shapes the visibility rules:

| Surface | Member sees | Admin sees |
|---|---|---|
| Per-app open events | Their own | Aggregate only ("Photos: 40× this week") — not who opened it |
| Sign-in history | Their own sign-ins | All users' sign-ins (security-relevant) |
| Install / uninstall events | Apps they installed | All install/uninstall events with actor (admin domain) |
| Per-app storage | Their own Tier-3 instances | All apps + actor on shared apps |
| Per-user storage | Their own (`/home/<self>/`) | Aggregate per user ("Anna: 180 GB") |
| Drive-fill trend & forecast | Visible | Visible |
| Update history (per stream) | Visible | Visible |
| System resources (top-bar dropdown) | Visible | Visible |

The exception worth naming: **sign-in history is admin-visible for all users**. It's the security feature, not the surveillance feature — "did someone unauthorized access my box" is the third use case in `LOGGING.md` # Stance, and answering it requires admins seeing all logins.

The Mac mental model: admin can see system logs and who logged in; can't peek at which apps another user opened.

## Data stores

Two tables in the brain's SQLite (`/var/lib/moose/state/moose.db`). No second database, no Prometheus, no InfluxDB.

### `events` table

Append-only, one row per discrete event. Sibling to `audit_events` from `LOGGING.md` — same shape, possibly the same table with a `category` discriminator. (Implementation decision; spec treats them as one logical store.)

| Event kind | Subject | Actor | Notes |
|---|---|---|---|
| `app_opened` | app instance ID | user | Debounced: one row per (user, app) per 30 min. Source: Caddy hook on first request of a session. |
| `signin_succeeded` / `signin_failed` | user (or attempted username on fail) | n/a | Source: brain login endpoint, SSH (via PAM hook), SMB. `source` field disambiguates. LAN IP recorded. |
| `app_installed` / `app_uninstalled` | app slug + version | user | Already audit-event-shaped per `LOGGING.md`. |
| `app_update_applied` | app slug | user | |
| `stream_update_applied` | stream name | n/a (system) | `box` or `containers` — the two streams from `UPDATES.md`. Carries the component that moved (`debian`, `host-agent`, `brain`, `ui`, managed-service name), from→to version, and outcome. App updates have their own row above. |
| `health_issue_opened` / `health_issue_closed` | issue kind | n/a (system) | From `HEALTH.md` typed set. |
| `backup_completed` / `backup_failed` | n/a | n/a | Deferred — pairs with the backup feature in `NEXT.md`. |

Retention: forever. Volume is small (a 10-user box generating ~50 events/day × 10 years × 250 B per row ≈ 45 MB). Lives in backups via brain.db.

### `metrics` table

Time-series rollups. Shape: `(timestamp, kind, subject, value)`.

| Metric kind | Source | Cadence (full-res) |
|---|---|---|
| `app_storage_bytes` | `du` of `/var/lib/moose/state/instances/<id>/` | Hourly |
| `user_storage_bytes` | `du` of `/home/<user>/` | Hourly |
| `drive_used_bytes` | `statvfs` per mounted drive (OS drive, data drive) | 10 min |
| `app_cpu_percent` | Docker stats API, per container | 60 s |
| `app_ram_bytes` | Docker stats API, per container | 60 s |

Cost: `du` on a few hundred GB takes seconds; running it hourly is fine. Docker stats requires keeping a stats subscription open per container; the brain runs one combined subscriber and samples the latest values every 60 s rather than holding a per-container stream.

#### Retention via downsampling

| Age | Resolution |
|---|---|
| 0–24 h | full (60 s for CPU/RAM, 10 min for storage) |
| 24 h – 7 d | 5-min averages |
| 7 d – 90 d | hourly averages |
| 90 d – forever | daily averages |

Brain runs a downsampler nightly. Approximate footprint after 5 years on a 10-app box: <200 MB. Comfortable.

## Real-time system resources

Different beast. Not stored, not periodic — a live stream that only runs while someone is looking.

### Source

`/proc/stat`, `/proc/meminfo`, `/proc/loadavg`, `/proc/net/dev`, `/proc/diskstats`, `/proc/uptime`. No Docker stats subscription — this view is box-level, not per-container. (`/proc/diskstats` carries the same per-device sector counters as `/sys/block/<dev>/stat` in a single read — the implementation reads it instead of one file per device.)

**These are host reads, so host-agent owns them, not the brain.** The brain runs in a container and cannot read host `/proc`/`/sys` directly — the same constraint that puts physical health detection in locus B (`BRAIN_HOST_PROTOCOL.md` # Health findings report). host-agent exposes a single raw-counter sample endpoint; the brain polls it and derives the rates. See # Mechanism.

**Interface and device selection.** The network section reports **physical LAN NICs + the mesh interface only** — the same allowlist `DISCOVERY.md` uses for Avahi. `lo`, the Docker bridge (`docker0`), per-container `veth*`, and any `br-*` bridges are excluded; summing `veth*` would double-count container traffic. The disk-IO section reports the **whole-disk block devices** backing the OS drive and the data drive (e.g. `sda`, not `sda1`); when no data drive is present, only the OS drive appears. host-agent applies this allowlist so the brain never sees container-bridge noise.

### Mechanism

SSE stream from brain → UI on `GET /api/v1/system/live` (`BRAIN_UI_PROTOCOL.md` # Pattern C). The data flow is two legs:

- **host-agent → brain:** `GET /v1/system/resources` (`BRAIN_HOST_PROTOCOL.md`), Pattern A. Returns the **raw cumulative counters** from the sources above plus a host monotonic timestamp. host-agent is stateless — it reads on request and holds nothing.
- **brain → UI:** the brain polls that endpoint once per second **while ≥1 UI subscriber is connected**, diffs successive samples (rate denominator = host timestamp delta), and emits the computed rates/levels to all subscribers from a single poller.

**The poll is ref-counted by connected SSE subscribers: it starts on the first subscriber and stops when the last disconnects.** One upstream poller fans out to N browser tabs — never one host-agent poll per tab. Zero idle cost on both sides: no subscribers → brain doesn't poll → host-agent does nothing.

**No replay.** Unlike the global event stream, this channel is **exempt from `Last-Event-ID` reconnect replay** — stale CPU/RAM samples are worse than useless on a live gauge. A (re)connecting client gets the next live sample; the first event after any connect reports rate fields as `null` (no prior sample to diff against) until the second sample arrives one second later. The channel still counts against the brain's ≤16-concurrent-SSE-per-session cap (`BRAIN_UI_PROTOCOL.md` # Stream cap).

The view is available to all users — host-level resource state isn't per-user data, and "the box feels slow, what's happening" is a question every household member can legitimately ask.

### What it shows

- **CPU:** total %, load averages (1 m / 5 m / 15 m).
- **RAM:** used / total, `MemAvailable` (the honest "how much can apps actually grow into" number — accounts for reclaimable cache).
- **Network:** per-interface in/out (KB/s or MB/s). LAN NIC + mesh interface (when Headscale lands).
- **Disk IO:** read/write KB/s per drive (OS drive, data drive).
- **Storage:** how full each disk is — used / total bytes per volume, as a bar (System always, Data when a data drive is present). Unlike the rest of this view, storage fullness is **not** part of the live SSE stream — it doesn't move at the 1 Hz gauge cadence, so it's a **one-time poll on panel open** at `GET /api/v1/system/storage` (the brain proxies host-agent's `SystemStatus.Disks`, the same `GET /v1/system/status` the install-plan dialog reads for free bytes). The panel shows a collapsed aggregate bar (used / total across all volumes) that expands to one bar per disk ("412 GiB free of 1 TiB"). See `BRAIN_HOST_PROTOCOL.md` # GET /v1/system/status and `DECISIONS.md` 2026-06-13.
- **Uptime:** since last boot.

Out of scope for v1: per-core CPU, CPU temperature, fan speeds, per-container live stats (that's the deferred "per-app Activity Monitor" view in `NEXT.md`).

### UI surfaces

- **Top-bar dropdown.** Small chevron next to the user menu opens a compact panel: CPU%, RAM used/total, a collapsible Storage section (per-disk fullness), net in/out, disk IO. Live-updating gauges (Storage is a one-time poll, see # What it shows). Available to every signed-in user. Opening the dropdown opens the SSE stream and fires the storage poll; closing it closes the stream. This is the **fourth locked top-bar element** in `DASHBOARD.md` # the top bar (added alongside the storage pill, avatar menu, and bell — see `DECISIONS.md` 2026-05-31).
- **Settings → System page** (admin route, deeper view) — same stream, full graphs over the last 60 seconds, all interfaces and drives broken out. Useful when "the box is slow right now" turns into "let me actually look at what's going on." **Deferred** to a follow-up; the all-users dropdown ships first (`NEXT.md`).

The top-bar dropdown is the primary discoverability surface. The deferred Settings page exists for admins who want to stare at it.

## Dashboard surfaces (historical)

The `events` and `metrics` tables drive these views. None of them stream — they're SQLite queries on page render, refreshed on user interaction.

- **Per-app page.** "Opened 12× this week. Last: yesterday. 4.2 GB storage (+200 MB this month). 7-day storage sparkline."
- **Per-user page** (self, or admin viewing a member). Last sign-in, sign-in history, storage used, most-used apps (self-view only — admin sees the user's aggregate without per-app personal usage).
- **Box overview** (the dashboard home). Drive-fill forecast widget ("at current rate, your data drive fills in ~6 weeks"), recently-opened apps strip, uptime.
- **Activity tab** (already in `LOGGING.md` # Activity view). Chronological timeline of events. Filtering and per-user scoping respect the visibility rules above.
- **Tidy-up suggestions** (passive, opt-out). "These apps haven't been opened in 90 days." Surfaced as a soft banner on the Apps page; never a notification. Specifically *suggests* uninstall, never auto-acts.

## Forecasting (drive-fill)

The killer feature for the long-term audience. Simple linear regression on `drive_used_bytes` over the last 30 days → projected days until 95% full. Surfaces as:

- **Calm state** (>90 days): nothing shown, or a small "Storage: healthy" indicator.
- **Warning** (30–90 days): banner on the box overview, "At current usage, your data drive will be full around <date>. Consider adding a second drive or cleaning up."
- **Critical** (<30 days): persistent banner + actionable suggestions (top-N largest apps, top-N largest users, link to storage settings). Pairs with `HEALTH.md` `disk-full-warning` once it's typed in.

Out of scope: storage growth attribution ("Photos grew 50 GB this month, mostly RAW files"). Useful but compound; defer to v2.

## What this doc does *not* cover

- **Per-container live CPU/RAM monitor.** Tracked in `NEXT.md` as a deferred "Activity Monitor"-shaped view. The infrastructure (on-demand SSE, Docker stats source) is the same; just a different surface.
- **App-level network bandwidth.** Useful for "which app is hammering my ISP" but expensive to collect cleanly (requires per-container veth accounting). Tracked in `NEXT.md`.
- **Per-device LAN activity** ("which phone talked to which app"). Privacy-heavy, requires arp/dhcp snooping, not in scope.
- **Push notifications based on analytics** ("Anna's storage exceeded 200 GB"). Notification surface not specced yet; pairs with whatever shape that takes.

## Cross-references

- `TELEMETRY.md` — the sibling. Anything that *leaves* the box belongs there, with an opt-in.
- `LOGGING.md` # Activity view — audit events overlap with the `events` table; possibly the same physical table.
- `HEALTH.md` — typed health issues drive `health_issue_opened` events; drive-fill forecast feeds `disk-full-warning`.
- `AUTH.md` — sign-in events sourced from dashboard / SSH / SMB; per-user visibility rules enforced via session role.
- `WEB_UI.md` — top-bar dropdown, per-app page widgets, box overview composition.
- `BRAIN_UI_PROTOCOL.md` — `/api/v1/system/live` SSE channel (no-replay, counts against the per-session stream cap); standard REST for historical queries.
- `BRAIN_HOST_PROTOCOL.md` — `GET /v1/system/resources`, the host-agent raw-counter sample the brain polls and diffs for the live stream.
- `STORAGE.md` — `/var/lib/moose/state/moose.db` location, `/var/lib/moose/state/instances/<id>/` for app storage, `/home/<user>/` for user storage.

## Open

Tracked in `NEXT.md`. The notable ones:

- **`events` vs `audit_events` table unification.** Implementation choice; spec treats them as one logical store. Decide when brain schema lands.
- **Per-container live monitor** ("Activity Monitor" view). Mechanism is described above; surface is deferred.
- **App-level network bandwidth accounting.**
- **Storage growth attribution** (what *kind* of data grew).
