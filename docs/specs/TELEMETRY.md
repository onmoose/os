# moose Telemetry

> Working spec for what moose (the project) collects from running boxes, how the user opts in, what's transmitted, and where it goes. Sibling to `LOCAL_ANALYTICS.md` (the user's view of their own box, never leaves the box). Touches `FIRST_RUN.md` (the opt-in screen), `LOGGING.md` (crash stream source), `UPDATES.md` (telemetry as halt-fast signal), `MOOSE_NETWORK.md` (endpoint surface).

## Stance

moose's product pitch is "own your data and apps on hardware you control." Telemetry is the part of the product where we ask the user to send us *something*. Two principles fall out:

1. **Off by default.** No toggle is friendly enough to justify defaulting on for a product whose whole pitch is data sovereignty. We accept the resulting low opt-in rate (peer projects land in the 5–15% range) as the price of the principle.
2. **One endpoint we control.** Every byte the box sends goes to `telemetry.onmoose.network`, terminated on infrastructure moose operates. No third-party SaaS endpoints. The backend behind that endpoint is an infra implementation detail (see "Backend choice" below) — but the box never knows the difference.

Telemetry is a **signal that accelerates our reaction time**, not a gate (`UPDATES.md` # Telemetry and rollout health). Boxes with telemetry off get the same updates, the same kill-switch protection, the same release timeline. They just don't contribute fleet signal.

## Locked: off by default, single first-run toggle

`FIRST_RUN.md` Step 4 owns the screen. The decisions:

- **One unchecked checkbox**, plain language: *"Send anonymous usage statistics and crash reports to help improve moose."*
- **An expandable "What does this collect?" link** directly below the checkbox, inline-disclosing the field list (below). No dark patterns; no separate "Learn more" page that nobody reads.
- **Toggleable from Settings later** — same checkbox, same disclosure. Off → on takes effect immediately; on → off stops the next transmission and is final (no rolling buffer that drains).
- **One toggle covers both streams** (usage analytics and crash reports). Splitting them creates a "I want to help with crashes but not usage" cohort that's hard to interpret and small to start with.

## What's collected — the allowlist

The spec lists fields. **Anything not on this list is not collected.** Adding a field requires editing this doc and bumping the telemetry schema version.

### Usage stream

Emitted at well-defined moments, batched, posted hourly.

| Event | Fields |
|---|---|
| `box_started` (once per boot) | `moose_version`, `host_agent_version`, `brain_version`, `country`, `cpu_arch`, `cpu_cores`, `ram_gb` (bucketed: 4/8/16/32/64+), `os_drive_gb` (bucketed), `data_drive_present` (bool), `boot_age_seconds` (since last boot) |
| `install_succeeded` / `install_failed` | `moose_version`, `reason_code` (failed only, coarse — e.g. `tpm_unseal`, `disk_too_small`, `network_unreachable`), `step` (failed only) |
| `app_installed` / `app_uninstalled` | `app_slug` (store apps only — see exclusion below), `app_version`, `catalog_id` |
| `app_update_succeeded` / `app_update_failed` | `app_slug`, `from_version`, `to_version`, `reason_code` (failed only) |
| `stream_update_succeeded` / `stream_update_failed` | `stream` (one of: `debian`, `host_agent`, `brain_ui`, `apps`, `managed_services`), `from_version`, `to_version`, `reason_code` (failed only) |
| `health_issue_raised` / `health_issue_cleared` | `issue_kind` (from the `HEALTH.md` typed set — e.g. `data_drive_missing`, `disk_full`, `update_pending`), `duration_seconds` (on clear only) |

That's the entire usage stream. Notably absent:

- **No per-app open events.** "How many people opened Immich today" is a local-analytics question (`LOCAL_ANALYTICS.md`); shipping it would mean shipping behavioral data about household members.
- **No user accounts or per-user data.** No user count, no role distribution, no login times.
- **No app names for user-pasted compose apps.** Only store-catalog apps (where the slug is already public) are reported. User-pasted compose manifests can have private names (`my-family-wiki`) and pointing at private catalog URLs — both stay on-box.
- **No file paths, hostnames, IPs, manifest URLs, Tailscale identity, network topology.**

### Crash stream

Emitted on the event, posted immediately if the box is online, queued otherwise.

| Event | Fields |
|---|---|
| `brain_panic` | `brain_version`, `panic_site` (file:line), `goroutine_stack` (scrubbed — paths under `/home/`, `/var/lib/moose/`, and `/srv/moose/` replaced with placeholders), `recent_request_path` (route template, not concrete URL — `/api/apps/:slug/start` not `/api/apps/photos-anna/start`) |
| `host_agent_panic` | `host_agent_version`, `panic_site`, `goroutine_stack` (same scrubbing) |
| `update_rollback` | `stream`, `from_version`, `to_version`, `trigger` (e.g. `crashloop`, `manual`) |

Crash *bundles* (full diagnostic dumps with logs, configs, container state) are **never automatically uploaded**. That's locked in `LOGGING.md` # Diagnostic bundles. Bundles are user-initiated and downloaded to the requesting admin's device; the user decides what to do with them.

## What identifies a box

**Rotating install ID.** A random 16-byte ID, generated locally, rotated every Monday 00:00 UTC. Persisted under the brain's state dir (`/var/lib/moose/state/`, `STORAGE.md` # mount layout) — there is no separate `/var/lib/moose-state` root. The previous week's ID is *not* retained — there is no client-side bridge from week N to week N+1.

Consequences:

- Cross-week retention curves are impossible to compute. We accept this.
- Reinstall → fresh ID (a reinstall happens to land mid-week, the install ID rotates out anyway by next Monday).
- An attacker with backend access cannot say "this box from January is the same box as this one from June" without correlating other signals (which we don't ship).

**No IP storage.** TLS terminates at the moose-controlled edge; the edge buckets the IP to ISO country code (e.g. `SE`, `US`) and drops the IP before writing to the analytics backend. No subdivision (state/region), no city, no AS number.

## Transport

- Box POSTs JSON to `https://telemetry.onmoose.network/v1/events` and `https://telemetry.onmoose.network/v1/crashes`.
- Batched: usage events buffer up to 1 hour or 64 KB, whichever comes first. Crashes post immediately (or queue if offline; retry on next boot).
- Payload is plain JSON, gzipped. No additional encryption layer beyond TLS.
- **Strict allowlist on the receiver.** Unknown event types or unknown fields are dropped at the edge with a `telemetry_schema_drift` counter — protects against accidental over-sharing if a future client ships a new field before this spec is updated.
- Failure to transmit is silent (no UI banner, no log noise). Telemetry is best-effort by design.

## Backend choice

The endpoint is fixed (`telemetry.onmoose.network`). The box always POSTs there; the backend is an implementation detail behind it.

**v1: PostHog Cloud.** Chosen for time-to-ship over self-hosted ops cost. This must be disclosed in the first-run expandable disclosure — the wording cannot say "stays with moose" because it doesn't:

> *Anonymous usage data and crash reports are sent to moose, processed via PostHog (a third-party analytics provider). No identifying information is included — see the field list below.*

Revisit if (a) box population grows large enough that PostHog Cloud cost outweighs ops cost of self-hosting, or (b) positioning pressure makes the third-party leg untenable. The box-side schema, endpoint URL, and disclosure copy are the only things that change in a future swap.

Crash stream: same backend in v1 (PostHog supports exception ingestion). Self-hosted Sentry is a possible future split if crash volume justifies the dedicated tool.

## Retention (server-side)

- Usage events: 24 months at full fidelity, rolled to monthly aggregates thereafter.
- Crash events: 24 months at full fidelity (we need to be able to look back at "is this regression new?").
- Country and rotating install ID are kept; nothing else identifying is stored.

## Settings UI

A single page under Settings → Privacy — an admin-only Box panel, since the toggle is box-wide (`SETTINGS.md` # panel inventory owns the IA placement; this section owns the page's behavior):

- The same checkbox as first-run, same disclosure.
- "Last transmission: <timestamp>" — so the user can verify nothing's leaving when the toggle is off.
- "View what was sent" — a chronological feed of the last 30 days of transmissions, decoded into the same English the first-run disclosure uses. Synology-shaped: nothing hidden, user can audit.

## Cross-references

- `FIRST_RUN.md` # Step 4 — the screen itself.
- `LOGGING.md` # Diagnostic bundles — why crash *bundles* never auto-upload.
- `UPDATES.md` # Telemetry and rollout health — telemetry as halt-fast signal, not gate.
- `RELEASE_MANIFEST.md` # Kill switch — works regardless of telemetry state; telemetry just speeds detection.
- `LOCAL_ANALYTICS.md` — the user-facing analytics that *never leave the box*. Distinct concern, distinct doc.
- `MOOSE_NETWORK.md` — `telemetry.onmoose.network` lives in the cloud surface alongside `releases.`, `store.`, and DNS.

## Open

Tracked in `NEXT.md`. The notable ones:

- **Country bucketing source.** GeoIP DB on the edge — which one, refresh cadence, what to do for unresolvable IPs (`XX`?).
- **Crash stream split.** v1 uses PostHog Cloud for both streams. If crash volume justifies it, split to self-hosted Sentry later (transparent to the box).
