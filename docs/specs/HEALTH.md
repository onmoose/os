# moose Health & Degraded Mode

> Working spec for how the brain represents and surfaces things-that-are-wrong without refusing to run. The model that lets the dashboard always come up, explain the problem in plain language, and walk the user through the fix.
>
> Companion to `BOOT.md` (the boot chain produces the conditions this doc handles), `CONTROL_PLANE.md` (the brain owns the health-issue state), `BRAIN_UI_PROTOCOL.md` (the wire shape for surfacing issues to the UI), `WEB_UI.md` (how banners and disabled actions render), `AUTH.md` (admin vs. member visibility of issues).

## Stance

moose's user is not a sysadmin. The failure-mode ranking for that audience is:

1. **Silent data corruption** — catastrophic, but rare, and they won't notice until backup-restore time.
2. **Box bricks (no UI, no obvious next step)** — also catastrophic, *and* they notice in 30 seconds. The "I'm returning it" moment.
3. **Box boots but something is degraded, with a banner explaining what** — annoying but survivable. They can act.

A naive hardening posture optimizes for (1) by halting at the systemd level on any anomaly. For moose's audience, that ranking is wrong: a brick costs more reputation than the rare corruption case it prevents, especially because most "corruption" risks are recoverable if caught early — the files are usually still *there*, just inaccessible or in the wrong place.

**The principle: if the brain can possibly run, it runs. Anomalies become health issues the user sees in the dashboard, not refusals to boot. The systemd-level recovery target exists only for the cases where the brain genuinely cannot start.**

This is closer to Synology's model than TrueNAS's: one UI, three states (healthy / degraded / failed), with remediation surfaced inline. Not "drop to a separate recovery interface."

## The health-issue model

The brain maintains a set of **health issues**. Each issue is a typed record the brain emits when a condition holds, removes when the condition clears, and exposes via the API so the dashboard can surface and act on it.

### Issue shape

```
{
  "id":              "data-drive-missing",   // stable string, single issue type
  "instance_key":    null,                   // optional — e.g. app id, drive id, user id
  "category":        "storage",              // storage | state | network | version | capacity
  "severity":        "error",                // warning | error | critical
  "tier":            2,                      // remediation tier (1, 2, or 3) — see below

  "blocks_writes":   true,                   // refuse writes to user data?
  "blocks_apps":     true,                   // refuse to start / install apps?
  "blocks_users":    true,                   // refuse to create / modify users?

  "summary":         "Your data drive isn't connected.",
  "details":         "moose expected the data drive enrolled on 2026-04-12 (UUID abc-123) but it isn't currently attached.",

  "actions": [
    { "label": "Try detecting it again",        "endpoint": "/api/v1/health/data-drive-missing/retry",       "tier": 1 },
    { "label": "Set up without this drive",     "endpoint": "/api/v1/health/data-drive-missing/forget",      "tier": 2, "destructive": true }
  ],

  "raised_at":       "2026-05-16T08:14:22Z",
  "last_checked_at": "2026-05-16T08:14:50Z"
}
```

The brain decides which flags apply per issue type. The UI does not infer; it reads.

### What the flags mean

- `blocks_writes` — the brain refuses any operation that would write user data (file uploads, share writes, app data writes). Reads stay available.
- `blocks_apps` — the brain refuses to start a stopped app, install a new app, or update an app. Already-running apps are not killed (that's a separate decision the user can take from the UI).
- `blocks_users` — the brain refuses to create new users, change passwords, change group membership.

A given issue may set zero, one, or several of these. The brain's operation gates are simply `!any(i.blocks_X for i in active_issues)`.

**Conservative-but-not-blunt:** when in doubt, block. But blocking everything is just "recovery mode with extra steps." The art is blocking the right things — see the taxonomy below.

## Remediation tiers

Every action attached to an issue declares a tier. Tiers describe **who does the work**:

- **Tier 1 — Physical action by the user, UI detects completion.** The UI explains what to do in plain language; the brain notices when the condition clears (typically via udev events, periodic checks, or push notifications from host-agent).
  Example: "Plug your data drive back in." → udev fires → storage re-verifies → issue clears, toast confirms reconnection.

- **Tier 2 — UI-driven remediation, one click.** The UI offers buttons; the brain (via host-agent) executes the fix. The user makes a decision; the box does the work.
  Example: "[Restore from backup (last good: 2 hours ago)]" → brain runs a job → on success, issue clears.

- **Tier 3 — SSH or console rescue.** Genuinely rare. These are the cases where the brain itself can't run, so there's no UI to drive a fix from. Tier 3 issues do **not** appear in the brain's health set (the brain isn't running); they're handled by `moose-recovery.target` (`BOOT.md` # Failure → recovery target — the narrow cases) or by physical console access for LUKS unlock.

**Rule: every issue in the taxonomy carries the lowest tier its remediation can be expressed at. If a new issue lands in Tier 3, that's a design red flag — figure out whether you can pull it back to Tier 2 with better tooling. Tier 3 stays tiny.**

**Rule: every banner has a primary action.** Even Tier 1 issues offer a no-op-but-helpful "Retry detection" button. A banner without an action is a status display the user can only watch, which makes the UI feel broken. Banners with actions feel like tools.

## Taxonomy

These are the issue types the brain knows about at v1. New issues are added by code change (each is a registered type in the brain), not declared in config. Adding new types is cheap; the categories and flag semantics are the contract.

### Storage

| ID | Severity | Blocks | Tier | Summary |
|---|---|---|---|---|
| `data-drive-missing` | error | writes, apps, users | 1 | Data drive enrolled but not currently attached. |
| `data-drive-wrong` | critical | writes, apps, users | 2 | A drive is attached, but its UUID doesn't match the enrolled drive. |
| `data-drive-readonly` | critical | writes, apps | 1 | Kernel mounted the data drive read-only (filesystem error suspected). |
| `disk-nearly-full` | warning | (new installs only) | 1 | A drive is above its usage warning threshold (90% data, 85% OS). |
| `disk-full` | critical | writes, apps | 1/2 | A drive crossed its hard threshold (95% on either drive). Tier-1 action: free space by removing files (links to Files / app instance breakdown); Tier-2: eject-and-replace flow (Settings → Storage → Eject) for upgrading to a bigger drive. |
| `canary-mismatch` | critical | writes, apps | 2 | Storage assembly succeeded but the canary check failed — almost certainly a moose bug. |
| `mergerfs-assembly-failed` | error | writes, apps | 2 | Mergerfs could not assemble the union across the data branches. |
| `disk-smart-failing` | error | (nothing) | 1 | SMART reports a drive is failing or growing bad sectors. **Deliberately does not block writes** — the data is still readable, and blocking would trap it on the dying drive (`DECISIONS.md` 2026-05-29). Loud banner + notification; Tier-1 action is "replace this drive" (links to the eject-and-replace flow). |
| `auto-unlock-degraded` | warning | (nothing) | 2 | The box booted, but TPM auto-unlock failed (PCR-7 changed) and it fell back to the recovery passphrase. It will prompt for the passphrase again on the next reboot until the TPM is re-enrolled. The *hard* case (can't boot at all) stays in `moose-recovery.target`, not here. See `STORAGE.md` # Encryption posture, `BOOT.md`. |

### State

| ID | Severity | Blocks | Tier | Summary |
|---|---|---|---|---|
| `brain-db-corrupt` | critical | nearly all ops | 2 | SQLite integrity check failed; brain is operating in a minimal-functionality mode. |
| `bootstrap-state-mismatch` | critical | nearly all ops | 2 | The box thinks it's bootstrapped but the brain database is missing — typically a data-drive swap. |
| `schema-migration-failed` | critical | apps, writes | 2 | A brain schema migration did not complete; the previous brain version is still callable for rollback. |
| `service-down` | error | (nothing) | 2 | A core system service isn't running. Host units are checked by host-agent via `systemctl` (locus B — profile-specific allowlist; host-agent itself is never on its own watchlist, as it can't report on itself); Caddy is a brain-managed container checked via the Docker + admin API (locus C) — see the detector catalog. Per-unit `instance_key`. Does **not** set block flags: operations that need the dead service fail naturally with their own errors, and blocking everything because Avahi died would be blunt. Tier-2 action restarts the unit. |

### Version

| ID | Severity | Blocks | Tier | Summary |
|---|---|---|---|---|
| `version-mismatch` | error | apps | 2 | host-agent's reported version is older than the minimum this brain requires — a compatible **range**, not an exact-match pair; a newer host-agent never raises this, since the brain declares a floor and not an exact pair. The floor can genuinely be crossed: stream B (the brain) updates **before** stream A (host-agent), so a new brain can land ahead of the host-agent it needs (`UPDATES.md` # Update ordering). The update itself parks rather than proceeding in that case (`UPDATES.md` # Compatibility matrix); this detector is what surfaces the state if a box reaches it anyway. |
| `app-image-partial` | warning | (that app only) | 2 | An app image download was interrupted and is incomplete. |
| `container-restart-loop` | warning | (nothing) | 2 | An app's container is crash-looping (restarted more than N times in a window). Per-app `instance_key`. The app is already failing; we surface it rather than block. Tier-2 action: view logs / stop the app. |
| `app-unresponsive` | warning | (nothing) | 2 | An app's container is running but its declared HTTP health-probe fails ("up but not responding"). Gated on the optional manifest `health_probe` field — apps that don't declare it are never probed. The app is reachable in Docker terms but not answering coherently; we surface it rather than block. Tier-2 action: view logs / restart the app. |

### Network

| ID | Severity | Blocks | Tier | Summary |
|---|---|---|---|---|
| `mdns-down` | warning | nothing | 1 | Avahi isn't publishing — the box may not appear as `moose.local`. |
| `hostname-conflict` | warning | nothing | 2 | Another device on the LAN already claims `moose.local`; Avahi renamed our host (e.g. `moose-2.local`). Admin picks a new hostname from Settings → System → Network. See `DISCOVERY.md`. |
| `lan-unreachable` | warning | nothing | 1 | network-online.target didn't reach in time; LAN reachability from other devices may be delayed. |
| `clock-not-synced` | warning | nothing | 2 | chrony hasn't synced in 6h or offset > 10s. Gates Let's Encrypt renewal within 7 days of expiry; surfaces in Settings → System → Time. See `TIME.md`. |

### Capacity & informational

Issues that exist to be visible without blocking anything. Same shape; all block flags false; surfaced as informational banners (warnings) or quiet cards (info).

| ID | Severity | Tier | Summary |
|---|---|---|---|
| `update-available` | info | 2 | A brain/UI, app, or OS update is ready to apply. See `UPDATES.md`. |
| `backup-overdue` | warning | 2 | No successful backup within the configured window. See `STORAGE.md` (backup architecture, deferred). |
| `tls-cert-near-expiry` | warning | 1/2 | A `.onmoose.network` certificate is within its renewal-failure window. See `MOOSE_NETWORK.md`. |
| `reboot-required` | info | 2 | A kernel or security update was applied that needs a reboot to take effect. Tier-2 action: reboot now / schedule. See `UPDATES.md`. |
| `ram-pressure` | warning | 1 | The box is under sustained memory pressure (swap thrashing). Informational — points the user at the per-container monitor to see what's heavy. See `LOCAL_ANALYTICS.md`. |
| `journal-disk-pressure` | warning | 2 | The persistent journal is near its size cap and competing for OS-drive space. Tier-2 action: vacuum the journal. See `LOGGING.md`. |

**The set is intentionally bounded.** ~25 well-named issues at v1, not 60. New failure modes map onto an existing issue or earn a new entry here; both paths go through a code change to the brain.

## Detector catalog

The taxonomy above says *what* the issues are. This section says *how each one is detected* — the missing half. Each issue type has exactly one detector; this is its contract.

### Where detectors run

The brain runs in a container behind the Docker socket-proxy (`CONTROL_PLANE.md`) and **cannot read host hardware** — no `statfs` of the host, no `smartctl`, no `systemctl is-active`, no `/proc/pressure`. That single fact shapes the whole catalog: every *physical* measurement is taken by **host-agent** and reported to the brain; the brain only runs detectors against state it owns directly. Four loci:

- **A — Boot-time reporters.** host-agent (and boot-chain units like `moose-storage-verify`, `BOOT.md`) write findings to `/run/moose/health/*.json`. The brain reads them at startup and reconciles. *(Built: storage assembly, canary, mergerfs — slices 0019/0024.)*
- **B — host-agent periodic reporters.** host-agent samples on a timer and the brain polls a health endpoint, reconciling findings the same way boot findings are reconciled. *(Built: `GET /v1/health/system` @ 60s — storage category from the boot reporter, the `service-down` services category via `systemctl is-active`, the `clock-not-synced` time category via `chronyc tracking` (re-queried ≤ every 5 min), the `ram-pressure` resources category via `/proc/pressure/memory` PSI (`some avg60`), and the `reboot-required` system category via `/var/run/reboot-required`; `ApplyFindings(category, …)` reconciles each domain independently.)* Everything host-physical and recurring lives here.
- **C — brain periodic checks.** Brain goroutine timers over brain-owned state: SQLite, the cert it serves, the version it negotiated, Docker events via the proxy.
- **D — reactive.** udev (drive add/remove), Docker container events, host-agent push events — no polling, the signal arrives.

**Transport decision (locus B):** the existing single-purpose `GET /v1/health/storage` generalizes to **one `GET /v1/health/system` report carrying findings across domains** (storage, drives, services, resources), not a proliferation of per-domain endpoints. The brain's `ApplyStorageFindings` reconcile (`internal/health`) generalizes to `ApplyFindings(category, findings)` — same clear-absent / raise-present / atomic-batch logic, scoped per category so a storage poll doesn't clear a service finding. This is a `BRAIN_HOST_PROTOCOL.md` knock-on; see `DECISIONS.md` 2026-05-29.

### Cross-cutting detector policy

These defaults apply to **every** detector unless its row overrides them. `HEALTH.md` previously deferred this to "inside each detector"; pinning a default stops every detector reinventing anti-flap.

- **Debounce — raise on 2 consecutive bad samples, clear on 1 good sample.** Asymmetric on purpose: slow to alarm (avoid transient-noise banners), fast to reassure. **Exception:** locus-A boot reporters and locus-D reactive signals (udev, Docker events) are authoritative and 1-shot — no debounce. Locus-C detectors that check a deterministic, non-noisy value (e.g. `version-mismatch`: a semver comparison of host-agent's reported version against the brain's `minimumAgentVersion` floor — DECISIONS.md 2026-07-16) are also 1-shot: raise/clear on the first definitive reading, no debounce needed.
- **Hysteresis on threshold issues.** Raise and clear thresholds differ so a value hovering at the boundary doesn't flap the banner: `disk-nearly-full` raises at 90%/85% but clears only below 88%/83%; `disk-full` raises at 95%, clears below 93%.
- **No severity escalation over time** (already locked) — a warning that's been up for a week is still a warning. Detectors may raise a *different, more severe issue* when the *evidence* worsens (e.g. SMART pre-fail vs. confirmed self-test FAIL could be two issues), but never escalate the same issue by age.
- **Last-checked is always fresh.** Every poll updates `last_checked_at` even when nothing transitions, so the dashboard can show "checked 30s ago" and a stale timestamp itself signals a dead detector.

### Catalog — locus B (host-agent periodic)

| Issue | Measurement | Cadence | Raise threshold | Clear |
|---|---|---|---|---|
| `data-drive-readonly` | `findmnt` mount flags on the data branch | 60s | flag `ro` present | flag `rw` |
| `disk-nearly-full` | `statfs` %used per drive | 5 min | ≥90% data / ≥85% OS | <88% / <83% |
| `disk-full` | `statfs` %used per drive | 5 min | ≥95% either drive | <93% |
| `disk-smart-failing` | `smartctl -H` + reallocated/pending/uncorrectable sector counts | 6h | SMART health FAIL **or** sector count >0 and growing | sticky — clears only when the drive is replaced (UUID change) |
| `service-down` (host units) | `systemctl is-active` over the **profile-specific** host-unit allowlist — appliance: `docker`, `avahi-daemon`, `chrony`, `smbd` (host-agent intentionally absent — a dead host-agent can't report on itself); hosted: `docker` only (the lean cloud image cuts Avahi/chrony/Samba, `ENVIRONMENT.md` # How the profile is realized). Caddy is **never** in either set — it is a brain-managed container, not a host unit (locus C, below). | 60s | `failed`/`inactive` | `active` |
| `ram-pressure` | `/proc/pressure/memory` (PSI `some avg60`) | 60s | sustained > threshold (tune at first soak) | below threshold |
| `clock-not-synced` | `chronyc tracking` — last sync age + offset | 5 min | >6h since sync **or** offset >10s | synced and offset <10s |
| `mdns-down` | `systemctl is-active avahi-daemon` + publish state | 60s | not publishing | publishing |
| `lan-unreachable` | `network-online.target` reached + primary-connection state | 60s + NM event | target not reached | reachable |
| `journal-disk-pressure` | journal directory size vs configured cap | 1h | within 10% of cap | below |
| `reboot-required` | presence of `/var/run/reboot-required` (+ `/var/run/reboot-required.pkgs` for the package list in the message) | 1h | file present | file absent (self-clears on reboot — `/run` is tmpfs) |

`hostname-conflict` is locus **D** (Avahi name-collision signal, not polled). `data-drive-missing` / `data-drive-wrong` are primarily locus D (udev) with a 60s locus-B backstop.

### Catalog — locus C (brain-owned state)

| Issue | Measurement | Cadence | Raise |
|---|---|---|---|
| `brain-db-corrupt` | `PRAGMA integrity_check` | boot + 6h | result ≠ `ok` *(built)* |
| `schema-migration-failed` | migration runner result | boot | migration aborted |
| `bootstrap-state-mismatch` | bootstrap marker present but DB absent | boot | mismatch |
| `version-mismatch` | host-agent's reported `agent_version` vs the brain's `minimumAgentVersion` floor, on handshake | each handshake | agent version older than the minimum *(built)* |
| `tls-cert-near-expiry` | NotAfter of the served `.onmoose.network` cert | daily | within renewal-failure window |
| `update-available` | release/catalog manifest vs installed | per refresh | newer version present |
| `backup-overdue` | last successful backup timestamp | hourly | older than window *(deferred with backup)* |
| `store-write-failed` | store write error (reactive, not timed) | on error | any persistent write failure *(built)* |
| `service-down` (Caddy) | Caddy container state via the Docker API + Caddy admin-API (`localhost:2019`) reachability | 60s | bounded self-heal exhausted (see below) — *deferred, see note* |
| `app-unresponsive` | HTTP GET **through Caddy** to the app's own route (`Host: <slug>`, path = manifest `health_probe.path`); response status vs the app's `healthy_status` set | 60s (health-poll tick) | status outside the healthy set, or timeout / connection failure — 2 consecutive bad samples, after the start-period grace. Clears on 1 good sample. |

**`brain-db-corrupt` is authoritative and 1-shot.** A `PRAGMA integrity_check` verdict is definitive, not a noisy sample, so this row overrides the cross-cutting debounce default (`# Cross-cutting detector policy`: raise on 2 consecutive bad samples) and raises/clears on the first reading — the same posture the policy grants locus-A/D signals. A query that fails to *run* (rather than returning a non-`ok` result) is inconclusive: no raise, no clear; corruption that breaks the query itself surfaces through `store-write-failed` instead.

**Why the `service-down` Caddy check lives at locus C, not B:** Caddy and the socket-proxy are **brain-managed containers, not host systemd units** (`CONTROL_PLANE.md` # Locked: Caddy is moose substrate, runs as a container) — there is no `caddy.service` for `systemctl is-active` to query. The brain already owns Docker access and Caddy's admin API, so it does a *better* check than systemctl could: container-running **and** actually serving (admin API answers / catch-all route present), which catches a wedged-but-not-exited Caddy that a process-liveness check would miss. The socket-proxy itself is not separately monitored — its failure manifests as the brain losing all Docker access, a self-evident condition surfaced through every Docker-backed operation failing at once.

**Detection feeds bounded self-heal, not a passive banner — and is deferred.** A fully-down Caddy means the dashboard is unreachable (Caddy fronts `moose.local`), so a banner has nobody to show it to. Instead, the brain restarts the Caddy container on failure, bounded like host-agent's `StartLimitBurst` (≈5 restarts / 60s); `service-down`(caddy) is raised only when that budget is **exhausted** (genuinely stuck), and the issue becomes a logged incident + post-recovery surface. This is **gated on the brain owning Caddy's container lifecycle** (start/stop/restart) — today the brain manages Caddy's *routes* (`EnsureServer`/`EnsureCatchAll`) but not its *container*, so the self-heal detector is **deferred** until that prerequisite lands. See `NEXT.md` # Caddy liveness self-heal and `DECISIONS.md` 2026-05-31.

**Per-reporter authority (reconcile rule).** `service-down` is one issue with a per-unit `instance_key`, but raised from two loci. Each reporter is authoritative **only over the `instance_key`s it reports** — the host-agent systemctl batch owns `{docker, avahi-daemon, chrony, smbd}` on appliance (or just `{docker}` on hosted); host-agent is never on its own watchlist (it can't report on itself); the brain locus-C check owns `{caddy}`. A reporter's batch clears only its own absent keys, never the other reporter's. This refines the "scoped per category" reconcile rule (`# Cross-cutting detector policy`) for the one issue that spans loci.

**Why `app-unresponsive` probes through Caddy, not by dialing the container.** The probe is opt-in per app (the manifest `health_probe` field, `APP_MANIFEST.md` # B); when an app declares it, the brain issues an HTTP `GET` on the health-poll tick. The probe goes **to Caddy, with `Host: <slug>`** — exactly the request a browser makes — never directly from the brain to the app container. The reason is the threat model: dialing the container would require the brain to join an app-facing Docker network, and Docker bridges are bidirectional, so that same membership would hand every app container (an **assumed-compromised** principal — `THREAT_MODEL.md` # B2) L3 reach to the brain's listening sockets, i.e. the control plane (`brain compromise = host compromise`, `THREAT_MODEL.md`). Probing through Caddy keeps the trusted control plane off every app-reachable network — the brain only ever talks to Caddy, which it already does for routing — and measures the user-visible truth: a request through the front door either gets a coherent answer or it doesn't (a dead upstream surfaces as Caddy's own `502`, which falls outside the healthy set naturally). See `DECISIONS.md` 2026-06-02.

**Healthy-status default and anti-flap.** Default healthy = **any status < 500** ("the app's HTTP server answered coherently"): an app that returns `401`/`403`/`404` on the probe path is *responding*; `5xx`, a timeout, or a connection failure is not. An author with a real `/healthz` can narrow this to `[200]`. The detector inherits the cross-cutting debounce (raise on 2 consecutive bad, clear on 1 good) and adds two guards: a **start-period grace** (default 60s after the container's `StartedAt`, overridable via `health_probe.start_period`) so a warming-up app doesn't raise on install/update, and **probing only steady-running containers** — a crash-looping app surfaces as `container-restart-loop`, not `app-unresponsive`, so the two detectors don't double-banner the same failure.

### Catalog — locus A (boot reporters) and D (reactive)

| Issue | Locus | Trigger |
|---|---|---|
| `canary-mismatch`, `mergerfs-assembly-failed` | A | `moose-storage-verify` finding at boot *(built)* |
| `health-report-malformed` | A | report file unparseable *(built)* |
| `auto-unlock-degraded` | A | boot-chain records TPM unseal fell back to passphrase |
| `data-drive-missing` / `data-drive-wrong` | D | udev add/remove; brain re-verifies enrolled UUID |
| `hostname-conflict` | D | Avahi name-collision callback |
| `app-image-partial` | D | image pull reports incomplete |
| `container-restart-loop` | D | Docker restart count > N within window *(built — brain polls `RestartCount`; N/window in the progress entry)* |

### What we deliberately do not check

moose is **closed-by-default, single-node, no email, no public DNS** except the opt-in `.onmoose.network` path. Several checks that a public-facing neighbor (Yunohost's `diagnosis`) treats as core are **non-goals** — written down so a future "parity" PR doesn't sleepwalk them in:

- **Email deliverability** — reverse-DNS, DNS blocklists, SMTP port reachability. We ship no mail stack (`NOTIFICATIONS.md` v1 is dashboard-only).
- **Public port exposure / open-port scans.** Closed-by-default means nothing is meant to be reachable from the internet; we don't probe for it.
- **Public DNS-record correctness / IPv6 reachability.** The `.onmoose.network` path owns its own cert/DNS health (`tls-cert-near-expiry`); there's no user-managed public DNS to validate.
- **fail2ban / intrusion-detection status.** Brute-force throttling on the login endpoint is its own item (`NEXT.md` Tier 4, `AUTH.md`), not a diagnosis check.
- **Kernel-panic / coredump capture.** Tracked as a `LOGGING.md`/`TELEMETRY.md` concern (`NEXT.md` Tier 4), not a health detector — by the time it'd raise, the box rebooted.

## Lifecycle of an issue

Issues are *facts the brain holds* — they're created by detection, kept until the underlying condition clears, and may have actions taken against them.

### Raise

Issues are raised by:

- **Boot-time reporters.** `moose-storage-verify` (`BOOT.md`) and similar boot-chain services write findings to a known path (e.g., `/run/moose/health/storage.json`). The brain reads this at startup and converts findings into issues.
- **Periodic checks.** The brain runs a small set of background checks (SQLite integrity on a timer, disk usage every N minutes, certificate expiry daily). Anomalies become issues.
- **Reactive signals.** udev events for drive add/remove, host-agent push events for service state, container events from Docker.

Detection is deliberately conservative: the brain does not raise an issue on transient noise (e.g., one failed health probe). A retry-with-backoff or N-of-M policy lives inside each detector.

### Display

The brain exposes the live set via `GET /api/v1/health` and streams transitions over the global SSE event channel (`BRAIN_UI_PROTOCOL.md`). The dashboard subscribes once at mount and surfaces:

- A **global banner** in the dashboard chrome when any `critical` or `error` issue is active. Click → the inline active-issues list on Home (`#health-issues`); a dedicated Issues route is a follow-up if that list grows unwieldy.
- **Inline cards** in the relevant section (Storage page for storage issues, Updates page for version issues, per-app card for `app-image-partial`, etc.).
- **Disabled action affordances** with explanatory tooltips wherever a blocked operation lives ("Install app" greyed out → tooltip: "Disabled because: data drive isn't connected").

The dashboard does not pop a modal for any issue. Issues surface in-place; the user reads them when they look.

### Clear

Each issue has a clearing condition the brain re-checks. For most issues this is the inverse of the raise condition (drive present → clear `data-drive-missing`; disk usage drops below threshold → clear `disk-nearly-full`). For issues where the user invoked a Tier-2 action, the action's success is the clearing event.

When an issue clears, the dashboard shows a brief toast ("Data drive reconnected") so the user *sees* the transition — silent auto-recovery is avoided. Toasts are the only place the brain interrupts the user; banners and inline cards wait to be looked at.

### Persistence

Active issues live in the brain's SQLite (`health_issues` table). They survive brain restarts. The history of raises and clears is in the `audit_events` table (`LOGGING.md`) — useful for diagnostic bundles and support flows.

## What the brain refuses to do

Across all active issues, the brain blocks operations whose preconditions don't hold:

- `blocks_writes=true` (any issue) → all write APIs to user-data paths return `409 Conflict` with `{code: "blocked-by-health-issue", issue_id: "...", message: "..."}`. SMB writes are refused at the share level (share present, ACL denies writes). Reads stay available.
- `blocks_apps=true` (any issue) → `POST /api/v1/apps`, `POST /api/v1/apps/:id/start`, `POST /api/v1/apps/:id/update` all return `409`. `POST /api/v1/apps/:id/stop` stays available — the user can always reduce activity.
- `blocks_users=true` (any issue) → `POST /api/v1/users`, password change, group membership changes return `409`. Login remains available.

**No override path on critical blocks.** The user cannot "install this app anyway" through a confirm dialog while `data-drive-missing` is active. The block is the protection; an override is an option the user will use, blame moose for, and remember. They'll resent the block once; they'll thank us the second time.

## What stays available in every degraded state

The non-negotiable invariant — the dashboard is the user's tool to recover the box, so it works no matter what:

- **The dashboard loads.** Always. Even with `brain-db-corrupt`, a minimal "your brain database is corrupt — here are your options" view comes up.
- **Login works** in all degraded states. PAM is independent of brain state (`AUTH.md`).
- **System logs are viewable** by admins. Required for any support flow.
- **A "diagnostic bundle" download** is available — zipped logs + state + issue set + storage view, for offline support.
- **The user's files** are reachable in whatever way is still safe. SMB stays up read-only if the data drive went read-only. Files-browsing in the UI stays up.
- **One-click actions for raised issues** are always callable (subject to the usual auth — admins only for system-level actions).

## What's deliberately out of scope here

- **The recovery dashboard UI itself.** Recovery mode (the static page served by `moose-recovery.target` when the brain can't run at all) is its own surface, specced in the deferred `RECOVERY.md` (`NEXT.md`). HEALTH.md owns the brain-is-running-but-unhappy story; RECOVERY.md owns the brain-can't-run story.
- **Auto-remediation.** The brain detects, blocks, and surfaces. It does not autonomously format, restore, or roll back. Every Tier-2 action is user-initiated. The single exception is host-agent's bounded restart policy (5 starts in 60s, then route to recovery) — that's a systemd-level safety net, not "remediation."
- **Severity escalation over time.** A warning doesn't become an error just because it's been raised for a week. Severities are properties of the condition, not of its age.
- **Cross-issue interactions.** Issues are independent. If two issues both set `blocks_apps`, the resulting block is the union; there's no priority among issues.

## Knock-ons to other docs

- **`BOOT.md`** — `moose-storage-ready.target` is *best-effort assembly*, not a strict gate. Individual storage-unit failures don't activate `moose-recovery.target`; they write findings to `/run/moose/health/` which the brain reads on startup. `moose-recovery.target` shrinks to two cases: TPM unseal failure on the root drive (brain can't start because the system can't boot), and host-agent itself broken (brain can't start because nothing launches it).
- **`STORAGE.md`** — the canary check and enrollment-marker mismatch are *reporters* now, not gatekeepers. Their findings become health issues, not boot failures.
- **`CONTROL_PLANE.md`** — host-agent's systemd ordering uses `After=moose-storage-ready.target Wants=moose-storage-ready.target`, not `Requires=`. host-agent always starts (so the brain always starts), reads storage findings, and acts accordingly.
- **`BRAIN_UI_PROTOCOL.md`** — new `/api/v1/health/issues` endpoint family and new event `kind`s (`health.issue_raised`, `health.issue_cleared`, `health.issue_updated`).
- **`BRAIN_HOST_PROTOCOL.md`** — locus-B detectors are fed by host-agent's `GET /v1/health/system` findings report (the brain can't measure host hardware itself). This doc owns *what* each detector measures; the protocol doc owns *how* findings cross the socket. See `DECISIONS.md` 2026-05-29.
- **`WEB_UI.md`** — banners, inline cards, disabled-action affordances. Active issues are server state held in the TanStack Query cache (not Pinia — `WEB_UI.md` # Health & degraded mode surfacing reconciled the earlier "Pinia store" phrasing to the locked *server-state-lives-in-Query* rule; issue #12); SSE invalidations wire updates; `useHealth()` composable.
- **`AUTH.md`** — admin-only vs. member-visible issues. v1: members see all banners (transparency), but Tier-2 actions on critical issues require admin elevation (existing 5-minute window).
- **`LOGGING.md`** — issue raises/clears land in `audit_events`, one record per issue (`action: health.issue.raised` / `health.issue.cleared`, `target_kind: health_issue`, `target_id` = the issue ID). Diagnostic-bundle endpoint includes the current issue set + recent transitions.
- **`NOTIFICATIONS.md`** — issue raise/clear is the primary trigger for the dashboard notification center. The issue-raise path additionally emits a notification for allowlisted issue types (storage + system criticals; box-wide criticals also emit a member-facing transparency variant). No change to the issue model.

## Locked decisions

- **The brain comes up if it can.** Degraded mode is the default response to anomalies; halting at the systemd level is reserved for the brain genuinely can't run.
- **Health issues are a typed, bounded set** registered in the brain code. Not config, not free-form.
- **Same dashboard in all states** — banners + inline cards + disabled actions, never a separate degraded UI.
- **Every banner has a primary action.** No actionless status displays.
- **Issues block operations via declared flags** (`blocks_writes`, `blocks_apps`, `blocks_users`). The brain consults these uniformly; no per-call ad-hoc gating.
- **No override path on critical blocks.** Users cannot click through `blocks_writes` or `blocks_apps` for critical issues.
- **No silent auto-recovery.** Cleared issues surface a toast.
- **Remediation tiers (1/2/3) are declared per action**, and Tier 3 is reserved for cases where the brain can't run.
- **Diagnostic-bundle download is always available**, regardless of which issues are active.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
