# moose Time, Timezone & NTP

> Working spec for how moose keeps the clock right, what timezone the box runs in, how containers inherit it, and what happens when sync fails. Touches `FIRST_RUN.md` (TZ auto-detect), `BOOT.md` (sync ordering), `HEALTH.md` (`clock-not-synced` issue), `MOOSE_NETWORK.md` (Let's Encrypt depends on correct time), `APP_MANIFEST.md` (per-app TZ opt-out), `LOGGING.md` (audit-event ordering), `UPDATES.md` (cron windows).

## Stance

Time is one of those load-bearing primitives that's invisible when it works and breaks half the product when it doesn't. The failure modes we care about: Let's Encrypt refuses to issue or validate certificates on a skewed clock; cron-based maintenance windows fire at the wrong wall-clock hour; audit-event ordering goes sideways; signed-release-manifest replay protection weakens; future TOTP would silently fail.

moose's posture is **modern NTP done right, by default**: authenticated time sources (NTS) where available, a real NTP daemon (not just an SNTP client), and a typed health issue when sync degrades. Cheap to get right at v1; expensive to retrofit.

## Locked: chrony as the NTP daemon

`chrony`, not `systemd-timesyncd`. The reasons:

- `systemd-timesyncd` is an SNTP client only — no NTS support, no server mode, weaker algorithm during long offsets or sleep/wake cycles.
- `chrony` handles laptop-shaped boxes correctly: closed lid for a week, dead RTC battery, flaky home wifi. It's what every Linux appliance project (TrueNAS, OpenWRT, Synology under the hood) settles on, for the same reasons.
- ~5 MB resident; not a footprint concern.

`systemd-timesyncd` is disabled at image-build time; chrony is enabled.

## Locked: NTP source list

```
server time.cloudflare.com iburst nts
pool 2.debian.pool.ntp.org iburst
pool 3.debian.pool.ntp.org iburst
server nts.netnod.se iburst nts
```

Two NTS-authenticated sources (Cloudflare primary, NETNOD as backup) and two Debian-pool fallbacks. chrony picks the majority and falls off bad sources automatically.

**Why NTS matters.** An attacker who can MITM your NTP can step the clock backwards and break Let's Encrypt validity windows (presenting an "old" valid certificate), invalidate TOTP (future), or weaken signed-manifest replay protection. NTS authenticates the time-server response. Free to enable, eliminates the attack class.

**Why no `time.onmoose.io` in v1.** Adds infra cost without obvious user need; the captive-network case (ISP blocks public NTP) usually blocks our endpoint too. Revisit if the case turns up.

**User override** lives in Settings → Advanced → Time. Editing the source list writes a fragment to `/etc/chrony/conf.d/`, then `systemctl reload chrony`. Most users will never touch it.

## Locked: RTC in UTC, always

`timedatectl set-local-rtc 0` — Linux default, enforced at image-build. Never set the RTC to local time; it breaks across DST transitions in ways that are hard to debug.

Boxes with a dead RTC battery boot with the clock at firmware default (often 1970 or 2017-ish). chrony's `makestep 1.0 3` rule steps the clock if offset > 1s during the first 3 updates, then slews thereafter. The 30–60 seconds between boot and first NTP sync produces wrong log timestamps; we accept this.

Boxes with no working RTC at all (rare on x86, possible on future ARM ports) are the same situation, just permanently. `chrony -s` (writes RTC after sync) is a no-op there.

## Locked: timezone — three layers

### System TZ

`/etc/localtime` symlinked via `timedatectl set-timezone <zone>`. Affects journald display, cron, anything reading `time.Local`.

- **First-run:** auto-detected via IP geolocation in the setup wizard (already specced in `FIRST_RUN.md` Step 3). If no internet at first-run, fall back to picking from a list.
- **Override:** Settings → System → Time. Always available.

### Container TZ

Docker containers default to UTC unless told otherwise. App authors usually want the user's TZ for display (Photos timestamps, Notes), occasionally don't care (databases, queues).

**Convention:** the brain bind-mounts `/etc/localtime` read-only and sets `TZ=<system_tz>` env on every managed container by default. App authors can opt out per-container in the manifest:

```yaml
timezone: utc   # default: system
```

Spec'd in `APP_MANIFEST.md`. Apps that need a different TZ from the system are vanishingly rare; the opt-out exists for completeness, not because we expect it to be used much.

### Per-user display TZ

**Skipped in v1.** The brain stores timestamps as UTC; the dashboard renders in the *browser's* TZ via `Intl.DateTimeFormat`. This handles the traveler case ("I'm in Berlin but the box is in Stockholm") for free without a per-user preference. A box-time-regardless-of-where-I-am toggle is a v2 nicety; tracked in `NEXT.md`.

## Boot ordering

`systemd-time-wait-sync.service` (ships with chrony) blocks until chrony reports the clock is synchronized. Services that **require** correct time depend on this unit:

- Let's Encrypt renewal (`MOOSE_NETWORK.md`) — validation refuses bad clocks.
- Backup scheduler (when backups land).
- Anything cron-based whose semantics depend on wall-clock hour.

Services that **don't** require correct time — and explicitly must *not* gate on it:

- `moose-brain.service`. The dashboard must come up regardless of NTP state. Users would otherwise lose UI access during ISP NTP outages. The brain runs in a "clock not synced" mode (see below) and refuses the specific operations that need it.
- `host-agent.service`. Same reasoning.
- `moose-storage-ready.target`. Storage assembly is time-independent.

This is the same pattern as `HEALTH.md` # Stance — degraded mode over hard failure, every time.

## Drift monitoring and the `clock-not-synced` health issue

**host-agent** samples `chronyc tracking` every 5 minutes and reports the result; the brain reconciles it into the `clock-not-synced` issue. Two values are parsed: time-since-last-sync and current offset estimate. This is a **locus-B (host-agent periodic) detector** — the brain runs in a container behind the Docker socket-proxy and cannot exec `chronyc` on the host, so every `chronyc` interaction (this poll and the "Force sync now" action below) runs in host-agent and is driven by the brain over `BRAIN_HOST_PROTOCOL.md` (`HEALTH.md` # Detector catalog, # Where detectors run).

**Raise `clock-not-synced` when:**

- No successful sync in the last 6 hours, **OR**
- Reported offset > 10 seconds.

**Shape** (per `HEALTH.md` typed-issue model):

| Field | Value |
|---|---|
| `id` | `clock-not-synced` |
| `severity` | `warning` |
| `blocks_writes` | `false` |
| `blocks_apps` | `false` |
| `blocks_users` | `false` |
| `tier` | 2 (UI-driven — admin can check sources, edit config, force resync from Settings → System → Time) |
| `summary` | "The box's clock isn't being kept accurate. Some features (HTTPS certificates, scheduled backups) may stop working." |

It's a **warning, not a halt** — the box stays usable. But it gates:

- **Let's Encrypt renewal** refuses to attempt if `clock-not-synced` is active AND the current cert is < 7 days from expiry. Would fail at validation anyway; surface honestly instead of fight-looping. The renewal job logs why and surfaces it on the cert status page.

Added to `HEALTH.md` # Network in the same change.

## What stays available when the clock is wrong

The brain operates in degraded mode, with the typed issue surfaced as a banner. Explicitly available:

- Dashboard login, app access, file browsing, all reads and writes.
- Manual sync trigger from the UI ("Force sync now" button in Settings → System → Time).
- Source-list editing.

Explicitly refused, with the issue cited:

- New Let's Encrypt issuance (would fail).
- Renewal of expiring certs (per the 7-day rule).

## Settings → System → Time

Surface for admins. Shows:

- Current system TZ + change action.
- Current time on the box (live).
- Sync status: synced / not synced, last sync, source currently in use, current offset estimate.
- "Force sync now" button → `chronyc -a makestep`.
- Source list (read-only by default; "Edit" reveals the config fragment editor, with a "Restore defaults" affordance).

Member view (Settings → System for non-admins) shows the first three (TZ, current time, sync status) but not the source editor or force-sync controls.

## Cross-references

- `FIRST_RUN.md` # Step 3 — TZ auto-detect at first-run.
- `BOOT.md` — `time-wait-sync` gate vs. the brain's independence from it.
- `HEALTH.md` # Network — `clock-not-synced` typed issue.
- `MOOSE_NETWORK.md` — Let's Encrypt renewal interaction.
- `APP_MANIFEST.md` — `timezone:` field for per-container opt-out.
- `LOGGING.md` — audit-event timestamps assume monotonic-ish wall-clock; degrades silently if clock jumps backward.
- `UPDATES.md` — maintenance windows are wall-clock-anchored.

## Open

Tracked in `NEXT.md`. The notable ones:

- **Captive-network NTP fallback.** If user reports surface, reconsider `time.onmoose.io`.
- **Per-user display TZ.** Browser-side rendering covers the traveler case in v1; revisit if box-time-regardless requests appear.
- **`last-known-time` rollback prevention.** Persisting last-shutdown time so first-boot-no-network doesn't show 1970 in logs — polish, not v1.
