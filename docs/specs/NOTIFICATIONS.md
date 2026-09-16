# moose Notifications

> Working spec for the dashboard notification center — the in-product bell that tells the user what happened on their box while they weren't looking. Companion to `HEALTH.md` (most notifications are derived from health issues), `UPDATES.md` (update outcomes), `LOGGING.md` (security-relevant audit events), `AUTH.md` (admin vs. member visibility), `WEB_UI.md` (how the bell and inbox render), `BRAIN_UI_PROTOCOL.md` (the wire shape for the live feed).

## Stance

Every surface moose has today assumes the user is *looking at the dashboard*. HEALTH issues are banners and inline cards — "the user reads them when they look." UPDATES are tile badges and auto-dismissing toasts. The Activity (audit) view is pull-only. None of this reaches the box sitting unattended in a pantry.

Notifications is the layer that aggregates and **persists** what happened, so that when the user next opens the dashboard they see a timeline — "here's everything since you were last here" — instead of only the current-state banners. It is the missing **history + read-state** layer, not a new event source.

**The notification center is not the out-of-band path, and v1 does not pretend to be.** A bell in the dashboard chrome closes the "I missed it because it was a transient banner / I wasn't on that page" gap. It does **not** close the "nobody is looking at the dashboard" gap — that needs a transport that pushes off the box (email, mobile push). Those are deliberately **out of scope for v1** (no email-on-file, no mobile app yet), and the model below is built so they slot in later as additional sinks without rework.

This is the TrueNAS model deliberately scoped down: TrueNAS drives an always-on in-UI alert bell *and* opt-in transports (email, Slack, …) off one typed alert taxonomy. We ship the bell, leave the transport list empty, and reuse our existing typed event sets (HEALTH issues, audit actions, update outcomes) as the taxonomy. The bell-alone configuration is a complete, useful product — TrueNAS with no alert services configured still works — which is what makes "center only" a legitimate v1 rather than a half-feature.

## Not a new event source

A notification is **derived** from an event that already exists elsewhere:

- a HEALTH issue being **raised** or **cleared** (`HEALTH.md`),
- an **update outcome** (app auto-updated, update failed/rolled back, system update available) (`UPDATES.md`),
- a small allowlisted set of **audit actions** (login from a new device, recovery-code use, role change) (`LOGGING.md`).

The brain does not invent a parallel taxonomy. Notifications are a **routing + persistence + read-state** layer on top of these. The design discipline mirrors HEALTH's bounded issue set: the set of events that produce a notification is a small, curated allowlist (below), registered in brain code — *not* "every audit event," which would bury the signal.

## Where it sits among the existing surfaces

Four surfaces, four jobs. Drawing the boundaries crisply is the most important clarity this doc provides:

| Surface | Lifetime | Job |
|---|---|---|
| **Toast** | Seconds, in-tab, ephemeral | "This just happened while you're watching" (issue cleared, update applied). Never persisted. |
| **HEALTH banner / inline card** | As long as the condition holds | "This is wrong *right now*, and here's the fix in context." State, not history. |
| **Notification center (this doc)** | Until read + dismissed | "Here's what happened while you were away," persisted, with read/unread and a link to the relevant surface. |
| **Activity view (audit)** | Forever, append-only | "The complete who-did-what record," queryable, for security review. |

A disk filling up produces: a HEALTH `disk-full` banner (state, with the fix), **and** a notification (so the admin sees it next login even if they don't visit the Storage page), **and** an audit row if a user action was involved. The notification *links to* the HEALTH banner; it does not replace it.

## The notification model

A notification is a typed record in the brain's SQLite. Shape:

```
{
  "id":          4821,                       // sequence, never reused
  "ts":          "2026-05-22T03:14:00Z",
  "category":    "storage",                  // storage | system | updates | security | account | app
  "severity":    "critical",                 // info | warning | error | critical
  "source_kind": "health_issue",             // health_issue | update | audit | app_lifecycle
  "source_id":   "data-drive-missing",       // links back to the originating event/issue type
  "dedup_key":   "health:data-drive-missing",// coalescing key — see Lifecycle

  "audience":    "admins",                   // admins | members | user
  "user_id":     null,                       // set when audience = user
  "variant":     "actionable",               // actionable | transparency (see Routing)

  "summary":     "Your data drive isn't connected.",
  "body":        "moose can't find the data drive enrolled on 2026-04-12. Saving and apps are paused until it's reconnected.",
  "action":      { "label": "Open Storage", "route": "/settings/storage" },  // or null

  "read_at":      null,                      // per-recipient; see Read state
  "dismissed_at": null
}
```

**Storage note — mutable, unlike audit.** Notifications live in their own table (`notifications`), *not* in `audit_events`. The audit table is append-only and forever (`LOGGING.md`); notifications carry mutable `read_at` / `dismissed_at` and are subject to retention (`store.PruneNotifications`, run at boot and hourly: a 90-day age cap, then a 1000-row ceiling that trims the oldest excess resolved-rows-first; `notification_reads` children cascade). Conflating them would fight both invariants. A notification may *reference* an audit row via `source_kind: "audit"` + `source_id`, but it is a separate, prunable record.

### Severity

Reuses HEALTH's vocabulary plus `info`:

- **info** — nothing wrong; something the user may want to know (app updated overnight, user created).
- **warning** — attention warranted, not urgent (disk nearly full, login from new device, permission approval pending).
- **error** — something failed (app update rolled back, schema migration failed).
- **critical** — box function is impaired (drive missing, disk full, brain DB corrupt).

Severity is a property of the **source event** (HEALTH issues already carry it), not assigned independently here.

## Routing — actionability + ownership

A notification goes to whoever can **act** on it, and/or whoever it is personally **about**:

- **System-level / box-wide** (storage, OS/system updates, brain state, box security) → **admins**. They hold `sudo` and are the only ones who can fix it (`USERS_AND_GROUPS.md`).
- **Personal / per-instance** (your app, your account, your login) → **the owning user**. This matches the per-user-instance model: Tier-3 app instances are owned by one user, and the permission-expansion prompt already targets the instance owner, not the admin or "everyone" (`UPDATES.md`).

Recipients:

- **Admin** — all admins.
- **Members** — all non-admin users; the broadcast audience for the transparency variant below.
- **Owner** — the member who owns the specific app instance.
- **Self** — the account the event is about.

### Member transparency variant

When a **box-wide critical** issue blocks something a member uses (e.g. `disk-full` → uploads paused, `data-drive-missing` → saving paused), the member receives an **info-only `transparency` variant**: a non-actionable notice ("Saving is paused — your admin has been notified") so the "why can't I upload?" question answers itself. The **actionable** variant — with the fix link — goes to admins. This mirrors HEALTH's existing stance that members see degraded-state banners for transparency while Tier-2 remediation stays admin-gated.

The transparency variant has no `action` and is purely informational; it clears (or flips to an "all clear" info notification) when the underlying issue clears.

**Delivery (as implemented).** The transparency notice is one row with `audience: members` — a class broadcast, the mirror of `admins`, read/dismissed per-recipient via the same `notification_reads` join. Which issues emit it is a curated property of the notification allowlist (the storage drive issues the table below marks "+ member transparency"), *not* a function of an issue's `blocks_writes` flag: `canary-mismatch` and `mergerfs-assembly-failed` also block writes but stay admin-only, because they are System/state plumbing, not the member-legible "your saving is paused" condition. The notice is `info` severity regardless of the originating issue's severity (the member's experience is the same whether the drive is missing or wrong). On clear, the member notice resolves and a member "all clear" (`info`) is emitted; on a flap (re-raise after clear) the now-false all-clear is retracted.

## The notification list (v1 source allowlist)

The curated set of events that produce a notification. New entries are added by brain code change, same discipline as HEALTH's taxonomy. Everything not on this list stays in HEALTH banners / Activity / Logs only.

**Storage** — source: HEALTH issues; all to Admin (members get the transparency variant on box-blocking criticals)

| Event | Severity | To |
|---|---|---|
| Drive failing / SMART warning (when the check lands) | error | Admin |
| `data-drive-readonly` / `data-drive-wrong` | critical | Admin (+ member transparency) |
| `data-drive-missing` | error | Admin (+ member transparency) |
| `disk-full` | critical | Admin (+ member transparency) |
| `disk-nearly-full` | warning | Admin |

**System / state** — source: HEALTH issues; Admin

| Event | Severity | To |
|---|---|---|
| `brain-db-corrupt`, `canary-mismatch` | critical | Admin |
| `mergerfs-assembly-failed` | error | Admin |
| `schema-migration-failed`, `version-mismatch` | error | Admin |
| Box rebooted / recovered after downtime | info | Admin |

**Updates** — source: UPDATES outcomes

| Event | Severity | To |
|---|---|---|
| OS / host-agent / brain+UI update available | info | **Admin only** |
| System update applied; reboot pending > 7 days | info | Admin |
| App auto-updated overnight | info | **Owner** (Tier-2 → Admin) |
| App update needs permission approval | warning | **Owner** (mirrors the existing on-login modal) |
| App update failed / rolled back | error | **Owner** (Tier-2 → Admin) |

**Security / account** — source: allowlisted audit actions

| Event | Severity | To |
|---|---|---|
| Login from a new device | warning | **Self** |
| Repeated failed logins on an account | warning | Self + Admin |
| Admin reset your password / changed your role | info | **Self** (affected member) |
| Recovery code used | warning | Self + Admin |
| SSH / SMB auth failures from a new source | warning | Admin |
| `sudo` / `su` invoked outside the UI elevation window | warning | Admin |
| User created / deleted | info | Admin |

**App lifecycle** — source: app reconciler; Owner

| Event | Severity | To |
|---|---|---|
| Your app finished installing / failed to install | info / error | **Owner** |
| Your app is crash-looping | error | **Owner** (Tier-2 → Admin) |

### Deliberately not on the bell in v1

**All network issues** (`mdns-down`, `lan-unreachable`, `hostname-conflict`, `clock-not-synced`) stay as in-context HEALTH banners only. They are low-actionability or self-resolving, and pinging the bell for them is noise. `hostname-conflict` and `clock-not-synced` are the admin-actionable borderline cases; revisit if support traffic shows users miss them (`NEXT.md`).

Most audit events also never notify — the Activity view is their home. Only the security-relevant subset above is promoted.

## Lifecycle — coalescing and read state

The "don't cry wolf" rules; this is where notification systems live or die.

### One notification per raise

For HEALTH-derived notifications, the brain emits **one notification when an issue is raised** and (optionally) **one when it clears** — never one per re-check. The `dedup_key` (e.g. `health:data-drive-missing`) means a flapping drive does not produce 40 notifications: while a notification with that key is unread, a re-raise of the same issue updates the existing record's timestamp rather than creating a new one. This rides HEALTH's existing raise/clear issue lifecycle directly.

### Clears

When the underlying HEALTH issue clears, the brain marks the notification resolved and emits a brief **info** "resolved" notification (e.g. "Data drive reconnected") — consistent with HEALTH's "no silent auto-recovery; cleared issues surface a toast" rule. The original critical notification is marked resolved, not deleted, so the timeline stays honest.

### Read / unread / dismiss

- **Per-recipient read state.** A notification addressed to `admins` is read/unread *per admin* — one admin reading it doesn't clear the badge for another. (Implementation: a `notification_reads` join table keyed on `(notification_id, user_id)` carries read/dismiss state for **every** recipient uniformly — `audience: user` rows take the same join, not a per-row fast path; the notification record itself stays free of per-recipient state. The uniform join was chosen over a row-column shortcut for `audience: user` because one code path is simpler than two and the read query joins regardless.)
- **Unread badge** on the bell shows the count of unread notifications visible to the current user.
- **Dismiss** removes a notification from the active inbox; dismissed notifications are retained (subject to retention policy) but out of the default view. Dismissing is not the same as resolving the underlying condition — a dismissed `disk-full` notification does not clear the HEALTH issue or unblock writes.

## Surfaces (dashboard)

- **Bell in the dashboard chrome**, always present, with an unread-count badge. Scoped to the current user per the routing rules above.
- **Dropdown inbox** on click: reverse-chronological list, severity-colored, grouped by read/unread. Each row: summary, relative time, and the `action` link if present (deep-links to the relevant surface — Storage page, app card, Settings → Updates, Activity view).
- **Live updates** over the existing global SSE channel (`BRAIN_UI_PROTOCOL.md`) — new notifications appear without a refresh; the badge updates in place. New `event kind`s: `notification.created`, `notification.updated` (read/resolve/dismiss).
- **No modal, no forced interrupt.** Consistent with HEALTH — the bell waits to be looked at. The one pre-existing exception stays where it is: the app permission-expansion **modal on next login** (`UPDATES.md`) is a separate, deliberate interrupt; the bell additionally carries a persistent `warning` notification for the same event so it isn't lost if the modal is dismissed.
- **Relationship to toasts:** an in-the-moment toast and a persisted notification can describe the same event (e.g. "app updated"). The toast is fire-and-forget; the notification is the durable copy. The brain decides per source whether to also toast; the dashboard does not synthesize toasts from notifications.

## Configuration

Minimal in v1:

- **Per-user, per-category mute.** A user can mute a category (e.g. an admin mutes `updates` info-level chatter). Defaults: everything **on**.
- Severity is not user-tunable in v1 (it's a property of the source event).
- No quiet hours / snooze in v1 (`NEXT.md`) — low value without an off-box transport that would otherwise wake someone.

**Delivery (as implemented).** A mute is a row in `notification_mutes` keyed `(user_id, category)` — presence means muted, absence means on, so a new user has no rows and sees everything ("everything on by default"); unmute is a DELETE. It is a **read-time filter** applied uniformly to the three aggregate surfaces (the inbox list, the unread badge count, and mark-all-read), *never* emit-time suppression: a box-wide `admins`/`members` notification is one row shared by many recipients, so it cannot be withheld per-user at write time. The per-id read/dismiss path is deliberately mute-agnostic — a user can still act on a specific notification in a muted category (e.g. one they read before muting). Mark-all-read honors the filter so a muted category is left untouched and reappears in its true unread state on unmute. Mutating a mute is **not audited** (a personal view preference, not an elevation-class action — `CLAUDE.md`). The wire surface is `GET /api/v1/notifications/mutes` (the caller's muted categories), `PUT`/`DELETE /api/v1/notifications/mutes/{category}` (mute/unmute, idempotent, 422 on an unknown category validated against the full `notify.Categories` taxonomy). Muting a category currently hides *all* its severities including criticals; whether criticals should ring through a mute is an open question (`NEXT.md`).

**Surface (as implemented).** The toggles live in **Settings → Notifications** as a per-category on/off list — one row per `notify.Categories` entry, *on* = receiving, *off* = muted, so a fresh account shows everything on. The dashboard owns the category labels (the brain owns only the taxonomy); flipping a row is optimistic (the switch responds immediately, rolls back on error) and then reconciles the mute set plus the now-refiltered inbox/badge against the server, riding the same `notification.updated` SSE invalidation as the bell.

## The transport-agnostic seam (why this doesn't need a rewrite later)

The model above is built so off-box transports slot in without reshaping it:

- The **routing layer** (which recipients, which variant) is computed once, independent of delivery.
- The **notification center is the first sink.** A future `email` / `push` / `webhook` sink subscribes to the same routed notification stream and applies its own per-user, per-category delivery preferences.
- Severity is already the natural dial for "which transports fire" (e.g. critical → all sinks; info → in-product only) when transports exist.

When email-on-file lands (its own `NEXT.md` Tier-2 item) and/or the mobile app ships (deferred with the mesh, `MOOSE_NETWORK.md`), they become additional sinks behind this seam. **No off-box transport, no SMTP relay, no `onmoose.network` dependency, and no email-on-file requirement in v1** — the notification center is fully local and self-contained.

## Knock-ons to other docs

- **`HEALTH.md`** — issue raise/clear becomes the primary notification trigger. No change to the issue model; the brain's issue-raise path additionally emits a notification for allowlisted issue types. Worth a one-line cross-reference there.
- **`UPDATES.md`** — the update outcomes it already surfaces (tile badges, Settings → Updates, toasts) additionally produce notifications per the list. The post-update toast and the notification are the durable/ephemeral pair described above.
- **`LOGGING.md`** — the allowlisted security/account audit actions fan out to a notification in addition to the `audit_events` row. The audit row stays the system of record; the notification is the prunable, read-stateful copy.
- **`BRAIN_UI_PROTOCOL.md`** — new `/api/v1/notifications` endpoint family (list, mark-read, dismiss, per-category mute) and new SSE `kind`s (`notification.created`, `notification.updated`).
- **`WEB_UI.md`** — bell + dropdown inbox in the chrome; `useNotifications()` composable wrapping the list + unread-count `useQuery`s (notifications are server state → TanStack Query, not Pinia, per `WEB_UI.md`'s "server state lives in Query" rule; the dropdown open-state is component-local ephemeral state); SSE subscription invalidates those queries; unread-badge logic.
- **`AUTH.md` / `USERS_AND_GROUPS.md`** — recipient resolution (Admin / Owner / Self) reuses the existing role and instance-ownership model; no new concepts.

## Locked decisions

- **In-product dashboard bell only in v1.** No email, no mobile push, no webhook. The notification center is the always-available floor; off-box transports are deferred and slot in behind a transport-agnostic seam.
- **Notifications are derived from existing events** (HEALTH issues, update outcomes, allowlisted audit actions) — not a new event source, not a parallel taxonomy.
- **Curated source allowlist**, registered in brain code, bounded like HEALTH's issue set. Everything else stays in banners / Activity / Logs.
- **Routing = actionability + ownership.** System/box-wide → admins; personal/per-instance → owning user. OS/system updates are **admin-only**; app updates go to the **instance owner**, never broadcast to all users.
- **Member transparency variant** — box-wide criticals that block a member-visible function emit an info-only, non-actionable notice to members; the actionable copy goes to admins.
- **All network issues stay off the bell in v1** (HEALTH banners only).
- **Stored in a separate, mutable, prunable `notifications` table** — distinct from the append-only forever `audit_events` table.
- **Retention is age-primary with a row ceiling**: prune deletes rows older than 90 days, then (if still over) trims the oldest excess with resolved rows dropped before active ones — a cleared issue is history, a live one is not. Runs at boot and hourly (`MOOSE_NOTIFY_PRUNE`). The 1000-row cap is a runaway ceiling, not a hard quota.
- **One notification per issue raise**, coalesced by `dedup_key`; resolved (not deleted) on clear. No per-flap spam.
- **Per-recipient read state**; unread badge; dismiss ≠ resolve. No modal interrupt (the existing permission-expansion login modal is the sole pre-existing exception, with a persistent notification mirror).
- **Per-user, per-category mute; everything on by default.** No quiet hours / severity tuning in v1.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
