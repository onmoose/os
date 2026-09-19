# moose Update Model

> Working spec for how a running moose box stays current. Companion to `SPEC.md`, `CONTROL_PLANE.md`, `BUILD.md`, `SERVICE_PROVISIONING.md`, `APP_MANIFEST.md`.

This doc is **draft / option-survey**. Most sections present alternatives with a recommendation; locked decisions are pulled out at the bottom. The intent is to surface forks before committing.

## What this doc covers

A box has **two update streams**. The split is not by component, it is by **what the thing runs on**:

- **Stream A — the box.** Debian base, kernel, firmware, and `host-agent`. This is the machine itself. It is slow, it sometimes needs a reboot, and it is **one atomic unit**: you do not get to have a new kernel with an old `host-agent`. Today that unit is realized by `apt` (# 1, # 2); the end state is an A/B image, deferred to v2 (`SPEC.md`).
- **Stream B — the containers.** `moose-brain`, `moose-ui`, apps, and managed services. These are images. They are frequent, they need no reboot, and each one already carries its own rollback story, because "keep the old image" is what a container registry is for.

Within stream B the components still have their own policies — the control plane is admin-triggered or cloud-pushed, apps auto-apply unless permissions expand, managed services are invisible infrastructure. Those policies are in # 3, # 4, and # 5 below and are unchanged by the two-stream framing. What the framing fixes is the **unit of testing and the unit of rollback**: two streams means we ship and verify two combinations, not the cross-product of five.

This doc spells out the policy for each component, plus the cross-cutting concerns: scheduling, rollback, dependency ordering, failure handling. Sections # 1 to # 7 describe the **appliance** profile — a box moose does not operate. The **hosted** profile (`ENVIRONMENT.md`) keeps both streams and all the apply/rollback mechanics, but moves the update *decision* from the box admin to the cloud control plane. That delta is # 8.

It does **not** cover the eventual A/B immutable migration mechanics — that's a v2 design once the product has traction (`SPEC.md`).

### Why two and not five

This doc used to describe five independent streams (Debian base, `host-agent`, control plane, apps, managed services). The flip to two is recorded in `DECISIONS.md` 2026-08-11. The short reason: five independent streams means the fleet holds a cross-product of versions that we can never enumerate, let alone test. Two streams means a box is described by two numbers.

The comparable products agree, and the one that disagrees is the cautionary tale. umbrelOS ships whole-system A/B images (Rugix for the image, Mender for the OTA), with apps updating separately from a git-backed store. ZimaOS ships A/B slots via RAUC. Both converged on "the box is one atomic thing, the apps are not." CasaOS is the counter-example: a shell script that downloads per-component binaries (Gateway, MessageBus, UserService, LocalStorage, AppManagement, CLI) from GitHub releases, independently — and per-component drift is a recurring source of broken installs.

We are not copying their mechanism (they are image-based appliances; our stream B is containers, which are already atomic and already versioned). We are copying the **boundary**.

---

## 1. Debian base *(stream A — the box)*

The OS underneath us — kernel, libc, OpenSSL, firmware, Docker itself.

### Options

- **A — `unattended-upgrades`, security-only.** Debian's stock auto-update. Pulls patches from `*-security` only. Conservative.
- **B — `unattended-upgrades`, full stable.** Same mechanism, broader scope (`stable`, `stable-updates`, `stable-security`). More fixes, more change surface.
- **C — Manual / admin-triggered only.** Settings → System → "Check for OS updates." User decides.
- **D — No automatic OS updates at all.** Lock to whatever shipped with the ISO; users reinstall to get a newer base.

### Recommendation: A — security-only auto-updates

- The "pantry laptop that just works" pitch (`SPEC.md`) requires security updates to apply without intervention. Most non-technical users will never click an update button.
- Full-stable auto-updates is the territory where `apt` actually breaks things. We deliberately scope to `*-security` to minimize that risk while still keeping the box patched.
- Larger upgrades (Debian point releases, dist-upgrade) stay manual / admin-triggered until A/B images land.

Pros:
- Standard Debian mechanism, well-understood, audited.
- Security floor without admin attention.

Cons:
- A bad security update can still brick boot. SPEC.md already accepts this as a v1 risk we cure with A/B images later.
- `unattended-upgrades` has corner cases (kernel updates leave old initrd, disk-full mid-upgrade) — Debian-standard problems with Debian-standard mitigations.

---

## 2. `host-agent` *(stream A — the box)*

Tiny native binary, supervises the brain (`CONTROL_PLANE.md`). Updates are rare — anything that changes often lives in the brain instead.

### Options

- **A — Auto-update via `unattended-upgrades` from our apt repo.** Same mechanism as the Debian base, just one more source list.
- **B — Brain orchestrates host-agent updates.** Brain detects a new version on `apt.onmoose.io`, downloads, calls a host-agent self-update endpoint.
- **C — Admin-triggered only.** Settings → "Update moose system."

### Recommendation: A — `unattended-upgrades` from our apt repo

- Boring, native, exactly what the apt machinery is for.
- host-agent shouldn't be updating itself while running — apt's preinst/postinst handle the systemd-unit restart cleanly.
- Brain orchestrating its own supervisor is a layering inversion we don't want.

Pros:
- Same plumbing as #1 — no new mechanism.
- apt's transactional model means partial-failure states are rare.

Cons:
- Coupled to apt cron schedule (typically nightly). New host-agent versions take up to 24h to roll out. Acceptable — it changes rarely.

---

## 3. Control plane — `moose-brain` + `moose-ui` *(stream B — containers)*

The control plane ships as **two container images** on **one release manifest**: `moose-brain` (the daemon) and `moose-ui` (the dashboard, per `WEB_UI.md`). Most weeks the UI moves and the brain doesn't; occasionally the brain moves and the UI doesn't; occasionally they move together (coordinated change requiring a new brain endpoint that the UI consumes).

This is the most user-visible update stream because the brain + UI together *are* moose from the user's perspective.

**One channel, two artifacts.** The user sees a single "auto-update moose" affordance. The updater pulls and recreates only what changed — UI-only ship recreates only `moose-ui`; brain-only ship recreates only `moose-brain`; coordinated ship recreates both as one transaction (pull both, recreate both, verify both healthy, commit; on failure, revert both).

### Options on update trigger

- **A — Auto-pull `latest` tag continuously.** Box polls registry, pulls when tag advances.
- **B — Release manifest.** Box polls a JSON manifest at `releases.onmoose.io/stable.json` that lists the *current* stable version. Gives us a kill switch (retract a bad release) and a place to gate rollouts if we later need pacing. See `RELEASE_MANIFEST.md` for the full schema + publishing pipeline.
- **C — Periodic prompt.** Box checks for updates, surfaces "moose X.Y.Z available — update now?" in the UI.
- **D — Fully manual.** Admin clicks update.

### Decision: B — release manifest, admin-prompted

- Release manifest (not raw `latest` tag) because we need a kill switch. If we ship a bad version and 5% of boxes start crashlooping, we want to flip the manifest back and stop *availability* of that version *now*, before more boxes prompt their admin to install it.
- Admin-prompted (not auto-applied) because v1 has no A/B rollback at the OS level. Phone-OS-style auto-apply assumes hardware-backed rollback we don't have until A/B images land. Surfacing "moose X.Y.Z is available" and waiting for the admin is the honest posture.
- The manifest names both `brain` and `ui` versions, plus `minimum_host_agent` and `rollback_to`. Full schema, signing (minisign / Ed25519), and publishing pipeline live in `RELEASE_MANIFEST.md`. v1 ships a single `stable` channel with no phased rollout — admin-prompting provides natural pacing at v1 scale.

The updater compares each named version against what's currently installed:

- `brain` unchanged + `ui` advanced → recreate only `moose-ui`. Brain keeps running; no API interruption.
- `brain` advanced + `ui` unchanged → recreate only `moose-brain`. UI keeps serving; brief API gap during brain restart (the in-tab `426` safety net per `BRAIN_UI_PROTOCOL.md` covers stale tabs).
- Both advanced → coordinated transaction: pull both, recreate both, verify both healthy, commit. On failure of either, revert both to the previous pair.

`rollback_to` is a *paired* rollback (brain + UI both revert together when fired), since brain/UI version pairs are tested together before publication. If set, the offer for the bad version is retracted from all boxes that haven't yet applied it; already-updated boxes see a "downgrade available" prompt that recommends reverting (using the kept-for-7-days snapshot). Cheap insurance. Full rollback semantics in `RELEASE_MANIFEST.md`.

For the silent (telemetry-off) population, our visibility comes from the same channels Ubuntu and Debian have always used: GitHub issues, support forum, direct reports. Slower than real-time metrics, sufficient for the appliance's risk profile at v1 scale.

### Phased rollout and beta channel — deferred

Both are deferred from v1 with explicit triggers documented in `RELEASE_MANIFEST.md` # "Future work" and # "Channels":

- **Phased rollout / cohorts** activates when A/B immutable images land and auto-apply becomes safe — admin-prompting no longer provides natural pacing. Schema is additive (`rollout` array + deterministic `hash(machine_id || canonical(brain, ui))` bucket).
- **Beta channel** reactivates when fleet growth outpaces direct-report detection, or when auto-apply lands. Additive — a new `beta.json` alongside `stable.json`, opt-in setting, no schema change.

### Update mechanics

1. host-agent polls the release manifest hourly.
2. If a newer manifest applies to this box (channel, host-agent compat), host-agent surfaces a "moose update available — vX.Y.Z" notification in the dashboard. Current versions keep running.
3. When the admin clicks **Update**, host-agent runs the changed-only transaction:
   a. Pull each image whose version moved (`moose-brain`, `moose-ui`, or both).
   b. **If brain moved:** snapshot the brain's SQLite database to `/var/lib/moose/brain-snapshots/<old-version>.db`. Cheap (SQLite is one file, single-digit MB at v1 scale).
   c. Recreate the changed containers in order: **UI first (if changed), then the brain**. Brain restart is fast (~5–10s); UI container restart is faster.

   This order used to read "brain first, then UI", and that was wrong — a race, not a preference. The brain runs `docker compose up -d` on the **same** control-plane project at every startup, so a brain started before host-agent's compose finishes runs a second compose on that project at the same time. The two then interleave the rename dance compose does to recreate a service, collide on the backup name, and leave the box with **no `moose-ui` container at all**; the updater's own health check then cannot resolve the UI and reverts a change that was otherwise fine. Starting the brain last removes the concurrency: host-agent's compose runs while no brain exists, and the brain boots into a stack that already matches the declaration, so its reconcile is a no-op. The # 8.3 handoff makes the two actors agree on *what* should be running; it says nothing about *when* each may act, and concurrent compose on one project is what falls out of that gap. Found by the booted-box proof in `docs/progress/cloud-update-boot-proof.md` — no fake-Docker test could see it.

   **The rule the order rests on: the brain reconciles the control-plane project once, at startup, and never again.** Ordering removes the overlap only while that holds. A UI-only update still runs compose with the brain up, so if the brain's reconcile ever became periodic — a timer, a retry loop, a watchdog — the same two-compose collision comes back, and the recreate order does not fix that shape of it. Two actors share one compose project, and the collision needs **both to run compose at the same moment**. host-agent's compose is safe against a brain that is already up — that brain will not touch the project again — but not against a brain that is itself starting, which is why a brain-changed update starts the brain last. Anything that makes the brain call compose again after startup reopens the window, and closing it again would need a real lock between the two actors, which the # 8.3 handoff does not give.
   d. Wait up to 60s for `/healthz` on the brain and a simple HTTP probe on the UI.
4. **On health-check failure of either:** host-agent reverts **both** to the previous pair (revert images, restore SQLite snapshot if brain was changed), restarts. Surfaces the failure in the UI with a "rollback succeeded" status.
5. **On three consecutive failed update attempts to the same manifest:** host-agent pins to the last-known-good pair and stops re-prompting until the release manifest advances past the failing version (or `rollback_to` retracts it).

Keep the previous brain/UI image pair and SQLite snapshot for 7 days, then GC.

If the release manifest's `rollback_to` field retracts the currently-offered version before the admin has applied it, the prompt silently disappears. This is the kill switch in action — admin never sees an offer for a known-bad release.

### Update window

Control-plane updates are admin-triggered, so they apply when the admin clicks. There is no fixed window. Apps and managed-service patches still serialize to the 03:00–04:00 window (#4, #5).

**Hosted is the exception, and it is the same window** (# 8.4, as built): nobody clicks on a box we operate, so the target-driven update waits for 03:00–04:00 local instead of applying the moment it is published.

Impact at apply time depends on what moved:

- **UI only:** ~1s of dashboard unavailability while the UI container restarts. Open tabs hit the in-tab `426` path on the next request and prompt the user to refresh.
- **Brain only:** ~5–10s of API unavailability. App routing continues (Caddy stays up; only the brain's API endpoints are briefly absent). Open tabs see a brief network error and recover on retry.
- **Coordinated (both):** ~10–15s. Admin sees a "this will take ~30s" notice before confirming.

---

## 4. Apps *(stream B — containers)*

`SPEC.md` locked: **automatic by default, per-app toggle off.** This section spells out what "automatic" means concretely and the one carve-out where we prompt.

### Auto-apply unless permissions expand

The trigger for prompting is **permission expansion**, not version bumps. Concretely, the brain diffs the new manifest's `permissions:` block against the running version's:

- New permission key (e.g., `devices` newly present) → prompt.
- Widened value (`internet: false → true`, new entry in `folders`, new entry in `devices`, `gpu: false → true`, mode upgrade `read → write` on an existing folder, etc.) → prompt.
- A new or widened `access.public_paths` entry (`APP_MANIFEST.md` # E2) → prompt. It exposes more of the app to anonymous callers, which is the same kind of trust event as a new folder grant, even though it sits outside the `permissions:` block. A narrower or unchanged list → no prompt.
- Same or narrower permissions → auto-apply, no prompt.

The `public_paths` rule is **specified, not yet built** — the permission diff itself is unimplemented (no app-update transaction reads it yet), so this says what the diff must cover when it lands.

This means a Photos `1.4 → 2.0` bump that doesn't touch permissions auto-applies. A Photos `1.4 → 1.5` bump that adds `devices: [/dev/dri]` for hardware-accelerated thumbnails prompts.

Reasoning:
- New permissions are a trust event. Auto-granting `lan: true` because an app's `2.0` manifest declares it is a security regression. The user opted into the *app at the trust level it had*, not into a permission expansion.
- Tying the prompt to a concrete manifest diff (rather than a fuzzy "major" judgment by the author) means the policy is enforceable without catalog reviewers having to relitigate what counts as a major bump.
- Cross-major managed-service migrations (Postgres 15 → 16, per `SERVICE_PROVISIONING.md`) are *infrastructure*, not user-facing trust events — they happen transparently in the update window. The pre-migration backup is the safety net.

### Who gets prompted

**The user who owns the instance**, not the admin (unless they're the same person).

Tier-3 apps run as per-user instances (`APP_ISOLATION.md`). Each instance is the property of one user — their data, their network exposure, their managed-service credentials. The permission-expansion prompt goes to that user the next time they log in. The admin has no special claim over another user's instance and is not notified.

Consequence: **two users on the same box can run different versions of the same app for a while.** Maria has accepted the `lan: true` expansion in Photos 2.0; Andrei hasn't, so his instance is still on 1.4. This is fine — instances are already fully isolated (separate containers, separate volumes, separate managed-service DBs per `APP_ISOLATION.md` "Managed services placement"). There is no coordination required.

A user who declines the prompt stays pinned to their current version. Their instance keeps running. The prompt re-surfaces if they dismiss without choosing; they can also accept later from the app's Settings page.

Tier-2 apps (Tailscale, SMB, DLNA) are box-wide and admin-installed. Permission changes for Tier-2 prompt the admin. Tier-2 update flow is otherwise covered in `SERVICE_PROVISIONING.md`.

### Update window

Same as brain: **03:00–04:00 local** by default. App updates serialize one at a time within the window — never two concurrent app updates.

### Update mechanics per app

1. Pull new image.
2. **Snapshot the app's state** — see "Pre-update snapshot" below.
3. Run `pre_update` hook (`APP_MANIFEST.md`) — deferred from MVP; the snapshot is the v1 safety net.
4. If managed-service major version changed: take pre-migration backup (per `SERVICE_PROVISIONING.md`), spin up new major, `pg_dump | pg_restore`.
5. Stop old container, start new container with the same volumes.
6. Wait up to 120s for the app to respond on `main_port`.
7. **On failure:** restore from the pre-update snapshot, revert to previous image. If managed-service was migrated, revert to the previous major and restore from the pre-migration dump. Notify admin.

**Keep the previous image and snapshot for 7 days.** "App is broken since last night" is the realistic complaint and we want the rollback button to actually work.

### Pre-update snapshot

The single biggest gap in image-only rollback is **app-managed schema migrations**. A new app version starts up, alters tables / rewrites data-volume files as part of its boot migration, then fails health check. Image rollback alone leaves the *old* code running against *migrated* data — broken in a way restoring the image doesn't fix.

Until lifecycle hooks return (`APP_MANIFEST.md` # F, `APP_LIFECYCLE.md` # Deferred: lifecycle hooks), the brain takes a brute-force snapshot before every app update:

1. **Tar the manifest's declared `data_volumes`** to `/var/lib/moose/state/instances/<id>/snapshots/pre-update-<old-version>.tar`. `cache_volumes` are excluded — that's literally what the data/cache split is for (`APP_MANIFEST.md` # C).
2. **If the app uses a managed service**, `pg_dump` (or equivalent for the service type) the app's logical database into the same snapshot dir. Cheap, well-bounded, runs in the 03:00 window when nothing else is going on. Applies whether or not the service version moved — protects against app-driven schema changes inside the same major.
3. **Retain alongside the previous image for 7 days**, then GC.

On health-check failure of the new container, the brain stops the new container, restores the tar (and the logical DB dump if present), and starts the previous image. Single-generation rollback — enough for the one-step-back UX, no n-deep history in v1.

Cost is bounded: `data_volumes` are author-declared and typically small (indexes, configs, app DBs); the bulk of app state usually lives in `cache_volumes` and is excluded. Snapshot happens during the 03:00–04:00 window when nothing else is running. Disk pressure surfaces as a `disk-full` health issue per `HEALTH.md`; if the box is too full to take a snapshot, the update is deferred and the user is told.

When hooks return, `pre_update` (author-provided, app-aware) replaces the tar for apps that ship one. The brain's snapshot stays the safety net for apps that don't.

**App-side rollback hooks are deferred.** A `post_update_rollback` hook fired only when `post_update` fails is the right long-term shape for apps that need bespoke recovery, but it pushes complexity onto every author for a case the snapshot already handles. Sketched in `APP_MANIFEST.md` # F; not in v1.

### Per-app auto-update toggle

- Default ON (`SPEC.md`).
- Off means: never auto-update. Admin sees "X update available" badge, clicks to apply.
- Off does **not** mean "freeze the version" — security-classified updates (we'll need a flag in the manifest) still apply auto if the catalog marks them critical. Open question: do we ship that flag in v1, or honor "off" strictly? Lean strict in v1 — fewer surprises.

---

## 5. Managed services *(stream B — containers)*

Postgres, Redis, etc. Per `SERVICE_PROVISIONING.md`, brain owns lifecycle.

### What triggers a managed-service update

- **Patch within a major** (Postgres 15.4 → 15.5): brain pulls and restarts on its own update window. App-transparent.
- **New major requested** (an app's manifest now wants Postgres 16, brain only has 15 running): triggered by app update, follows the cross-major migration path in `SERVICE_PROVISIONING.md`.
- **Major retired** (last app on Postgres 15 is uninstalled): grace period, then shutdown.

### Update mechanics

Patch updates serialize per major-version instance. Brain stops the container, pulls new image, starts it. App connections drop and reconnect — handled by client retry logic in the apps.

Cross-major migrations are an app-update mechanic, not a managed-service-update mechanic. They live in #4.

**No user-visible toggle for managed-service updates.** The user didn't install Postgres; they installed Photos. Postgres patch updates are infrastructure, not a user concern.

---

## 6. Dashboard update UX

The mechanics above describe *what* happens. This section describes how the dashboard surfaces it.

### Where updates appear

Three surfaces, one mental model:

- **Per-app tile.** A small badge on the app's tile in the dashboard when its version moved (auto-applied overnight, or pending user decision). Click → "What's new" panel with the upstream changelog (sourced from the manifest's `links.support` or a `changelog_url` field — small additive field).
- **Settings → Updates.** Single aggregate view: "X apps updated last night, Y waiting on you, Z failed." This is where the rollback affordance lives (using the kept-for-7-days image + snapshot per # 4).
- **No global "update now" button.** Auto-updates serialize in the 03:00–04:00 window. The dashboard does not pretend the user controls cadence beyond the per-app toggle and the permission-expansion accept (below).

### The permission-expansion prompt

The only case the box asks. Surfaces **on next login of the instance owner**, as a modal on first dashboard load — not a dismissible banner. The app stays on its current version until the user decides.

- **Diff shown in plain language.** "Photos wants new access: **read & write your Movies folder**." Same vocabulary as the install screen (`APP_MANIFEST.md` # E), so the user recognizes it.
- **Two buttons: Allow & update / Keep current version.** No third "remind me later" — closing the modal is dismissal, and the prompt re-surfaces on the *next* login (not every page load).
- **Allow & update applies immediately**, not at 03:00. The user just made a deliberate decision; making them wait until tomorrow morning is confusing. The ~minute of app unavailability is the cost of the choice they explicitly initiated.
- **Accept later** is available from the app's Settings page.

Consequence (already noted in # 4): two users on the same box may run different versions of the same Tier-3 per-user app for a while. By design — instances are already per-user isolated.

### Admin visibility into other users' versions

The instance-owner prompt model means an admin can't directly see *why* Cindy's Photos is on a stale version. They can wonder why disk usage diverges, or why behavior differs across accounts.

Settings → Users → `cindy` exposes "Apps Cindy hasn't accepted updates for: Photos 2.0 (pending permission: read & write Movies)." Read-only — the admin sees the fact, but cannot accept on Cindy's behalf. Tier-3 per-user instances are Cindy's to authorize.

### Failure signaling

Auto-rollback already happens (# 4). The dashboard surfaces it:

- **Per-app tile banner**, persistent until acknowledged: "Photos couldn't update to 2.0 last night. Rolled back to 1.4."
- **Settings → Updates** lists the failure with mode (image pull / health check / hook / snapshot restore) and a "view logs" link to the diagnostic bundle (`LOGGING.md`).
- **After 3 consecutive failures** to the same manifest (# 4 mechanics), the prompt stops and the banner changes to "Update is failing repeatedly. Paused. See logs." The retry button is still available; the box just stops trying on its own.

### Post-update toast

A small, auto-dismissing toast on the next dashboard visit after an overnight update batch: "3 apps updated overnight" → click for the list with per-app "what's new" snippets. Not modal, not blocking. Auto-dismisses after one view.

### Notification center

The update outcomes surfaced here also fan out to the dashboard notification center (`NOTIFICATIONS.md`), routed per the actionability + ownership rule: **OS / host-agent / brain+UI updates → admins only**; **app auto-update / permission-approval-pending / failed-rollback → the instance owner** (box-wide Tier-2 apps → admins), never broadcast to all users. The per-app tile badge, Settings → Updates view, and post-update toast remain the in-context surfaces; the notification is the durable, read-stateful copy for the user who wasn't looking when it happened.

### What lives in `APP_MANIFEST.md`

Additive fields the manifest grows to support this UX:

- **`changelog_url`** — optional pointer to a per-version changelog. If absent, the dashboard links to `links.support`.

No other UX-driven manifest fields in v1.

---

## 7. Cross-cutting concerns

### Update ordering

**Stream B before stream A**, and within stream B, control plane before apps:

```
stream B:  moose-brain + moose-ui  →  apps & managed services
stream A:  Debian base + host-agent  (last; may reboot)
```

Reasoning:
- Brain must support the manifest_version of any app coming next, so the control plane moves before the apps that depend on it.
- Stream A goes last because it often wants a reboot, and we'd rather reboot once at the end of the window than mid-flight.
- Stream A is internally ordered by `apt`, not by us. Debian base and `host-agent` are one transaction (# What this doc covers); asking which of the two goes first is asking about apt's dependency solver, not about moose policy.

The one ordering constraint that crosses the streams is the **`host-agent` ↔ brain compat floor**: a brain build declares the oldest `host-agent` it will talk to (`minimumAgentVersion` in `cmd/brain/main.go`, surfaced as a health issue per `HEALTH.md`). If a stream-B update would land a brain that needs a newer `host-agent` than stream A has delivered, the update parks with a clear reason rather than proceeding — see # Compatibility matrix.

### Reboots

Debian base updates set `/var/run/reboot-required` when applicable. Policy:

- **Reboot opportunistically in the update window** if the marker is set and no app is mid-update.
- **Otherwise wait.** Don't reboot during the day.
- After 7 days of a pending reboot, surface "your moose needs to restart" in the dashboard, but never force.

Reboot at v1 means roughly 30–60s of full unavailability. Acceptable nightly, hostile mid-day.

### Compatibility matrix

The release manifest (#3) carries `minimum_host_agent`. The brain carries `minimum_manifest_version` and `maximum_manifest_version` for apps. host-agent carries `minimum_brain_version`.

If an app update wants a manifest_version newer than the running brain supports, the brain refuses the update and surfaces "moose needs to update first" in the UI. The next brain update should resolve it; if it doesn't, the app stays pinned.

This means a misalignment never silently breaks something — it parks the update with a clear reason.

### Network requirements

All update streams require internet. **An offline box stays on its current versions indefinitely.** This is correct behavior, not a bug — local-first is a design property (`SPEC.md`).

When the box reconnects after an offline stretch, updates resume on the next scheduled window. We do **not** rush an immediate update on reconnect (avoids "I just plugged it in, why is it updating?").

### Telemetry and rollout health

When telemetry is enabled (`FIRST_RUN.md` opt-in), boxes report:
- Successful update completion per stream.
- Update failures with the failure mode (image pull, health check, hook).
- Crash counts per brain version.

Telemetry is a **signal that accelerates our reaction time**, not a gate. Boxes with telemetry off get the same updates and the same protection (manifest applies to everyone; `rollback_to` retracts a bad release fleet-wide) — they just don't contribute signal. When phased rollout activates post-v1, the schedule will be time-based — telemetry will let us halt or trigger `rollback_to` faster than we'd otherwise notice, not gate advancement.

### Rollback summary

| Stream | Component | Rollback mechanism |
|---|---|---|
| A — the box | Debian base | None in v1; A/B images later |
| A — the box | `host-agent` | apt revert (manual, rare path) |
| B — containers | `moose-brain` + `moose-ui` | Previous image pair + SQLite snapshot; revert as a pair, automatic on health-check fail of either |
| B — containers | App | Previous image + pre-update tar of `data_volumes` (+ `pg_dump` of managed-service DB if any), automatic on health-check fail; keep 7 days |
| B — containers | Managed service (patch) | Previous image; data is shared so this is a tag-flip |
| B — containers | Managed service (major migration) | Pre-migration dump, automatic on app-update fail |

The table shows the asymmetry that motivates the two-stream split: **every rollback in stream B is automatic and mechanical, and neither rollback in stream A is.** Containers keep their previous image by construction; a box that has run `apt upgrade` has no previous state to return to. Stream A's "no real rollback" is the v1 hole we accept, and A/B images are how it closes.

---

## 8. Hosted boxes

On the `hosted` profile (`ENVIRONMENT.md`) moose operates the box. That single fact changes who decides and how the box finds out. It changes almost nothing about what actually happens on disk.

**What is identical to appliance:** both streams, the apply transaction, the pre-update SQLite snapshot, the per-app `data_volumes` tar, the health-check-then-revert path, the 7-day retention, the 03:00–04:00 window, and the stream-B-before-stream-A ordering. All of # 1 to # 7 still applies. The expensive half of the update machinery is shared, and we only build it once.

**What is different:** three things, below.

### 8.1 The target version lives in the cloud, per box

The cloud control plane (`onmoose/cloud`, private) holds a **target version per `box_id`**. It does not serve `stable.json`, and a hosted box does not poll `releases.onmoose.io` or verify a minisign signature. That whole mechanism (`RELEASE_MANIFEST.md`) is appliance-only.

Why per-box rather than one fleet-wide value, or reusing the signed manifest:

- **We already have the identity.** Every hosted box was provisioned with a `box_id` and enrollment credentials (`ENVIRONMENT.md` # Provisioning), and the brain persists the `box_id` as frozen identity. A per-box target version is one more column next to facts the control plane already keeps.
- **It gives us staged rollout and pinning for free**, with no schema. `RELEASE_MANIFEST.md` # Future work defers cohorts behind a deterministic `hash(machine_id || version)` bucket precisely because a static file cannot address one box. A per-box target can: move 5 boxes, watch, move the rest; pin one troubled tenant to an old version while we debug; halt everything by not advancing.
- **The kill switch gets cheaper.** On appliance, `rollback_to` exists because we cannot reach the boxes. On hosted we can. A bad release is fixed by setting a new target, and it lands on the next window rather than needing a retraction protocol.

**The box polls the cloud; the cloud never connects in.** The box asks "what should I be running?" on its existing outbound path. No inbound port, no listener, nothing new in the firewall. This matches the rest of the hosted posture — outbound-only is why `moose-metadata-firewall.service` can be as strict as it is (`ENVIRONMENT.md` # Provisioning, #251).

**As built (#408), the box says which box is asking.** It sends its `box_id` as a query parameter — `GET <target-url>?box_id=<id>` — so the control plane can answer for that one box. The id comes from the provisioning seed (#407), read on the same pass as the update target. #401 shipped the fleet-wide read first, because one answer for everyone needs no identity at all; this is what makes "move 5 boxes, watch, move the rest" possible. A box with **no** `box_id` — an appliance box, or a hosted box with no seed — sends no parameter and gets whatever the endpoint serves everyone, exactly as before.

**The identity is a bare `box_id` on an unauthenticated endpoint, and it is deliberately weak.** Anyone who learns a box-id can read what that box is told to run, and can claim to be that box when asking. We accept that for now, for three reasons: the ask is a **read**, so nothing is mutated by it; the answer is version information, not tenant data; and the operator path that sets a box's target sits behind the control plane's own auth, so a forged ask cannot change what any box is told. The real costs are that a third party could watch one box's rollout, and that the control plane cannot trust the id it is handed. **Nothing that mutates state may be built on this identity.** The box's report-back (# 8.4 step 5) and the fleet-side auto-halt (# 8.5) both write, so they wait for real authentication.

That authentication is the credential below. **The concrete auth for this channel is not designed yet** — the enrollment credentials in `seed.json` today are scoped to acme-dns, not to a general box↔cloud API. Parked in `NEXT.md`.

### 8.2 We push; the tenant is told, not asked

There is no "Update available — click to install" prompt on hosted. The update applies in the window and the tenant admin gets a notification afterwards (`NOTIFICATIONS.md`, routed to admins).

The reasoning is ownership, not convenience. We run the machine, so an unpatched hosted box is our liability, not the tenant's oversight. A box sitting on a known-bad version because nobody logged in to click is a failure mode we would be choosing on purpose. Every hosted product patches this way.

**The one carve-out that survives: the app permission-expansion prompt (# 4) still prompts the instance owner.** We push *infrastructure* — a new brain, a new UI, a patched Debian. We do not push a *new grant of access to someone's data*. If Photos 2.0 wants read-write on the owner's Movies folder, that is a trust decision belonging to the person whose folder it is, and hosting their box does not transfer it to us. So on hosted, the box updates itself without asking, and an app that wants more permission still waits for its owner. That asymmetry is deliberate.

### 8.3 `host-agent` applies both brain and UI

One actor runs the stream-B control-plane transaction: `host-agent` pulls and recreates **both** `moose-brain` and `moose-ui`. One transaction, one health check, one rollback. The brain cannot recreate itself, and splitting the job so that host-agent does the brain and the brain does the UI would mean two actors, two rollback paths, and a coordinated brain+UI ship that needs them to agree mid-flight.

This has a real cost worth naming: `CONTROL_PLANE.md` locks the **brain** as the launcher of `moose-ui` and Caddy, so `host-agent` is here reaching past the brain to containers the brain reconciles. Left alone, the brain would come back up and reconcile the UI straight back to the version it knew about.

**The staged control-plane compose file is the handoff point.** `host-agent` writes the new UI image reference into the staged compose (`MOOSE_CONTROL_PLANE_DIR`, the same file `lifecycle.EnsureControlPlane` reconciles from) *before* recreating anything. The brain restarts, reads that file, and reconciles to the version already running. The two actors never disagree because they are reading the same declaration — host-agent is the one that writes it, the brain is the one that maintains it. Rollback writes the old reference back the same way.

**The brain's own reference lives in a second file, because the brain is not in that compose.** It cannot be: a process cannot bring itself up, so `host-agent` launches the brain with `docker run` (`CONTROL_PLANE.md` # host-agent launches the brain container) rather than as a compose service. So the box declares its pair across two files in the same directory, both written before anything is recreated:

- **`compose.yml`** — the UI's image. Read by the **brain**, which reconciles the stack to it.
- **`images.json`** — a small ledger: the pair the box should be running, the pair it was running before, and when each was applied. Read by **host-agent** at boot to decide which brain to launch, and by the update transaction to know what to revert to.

The ledger is not bookkeeping. `host-agent` leaves an existing brain container alone, so it only chooses an image when there is no container to leave — a first boot, or a box whose brain container was removed or pruned. Without the ledger that second case relaunches the reference baked into the disk image at build time, and an applied update silently goes backwards. The ledger is also where the previous pair is recorded, which is what the revert path and the 7-day retention window (# 3) both read. Every failure to read it falls back to the baked reference rather than refusing to launch: the brain is how anyone finds out what is wrong with the box, so it has to come up.

### 8.4 Mechanics

1. `host-agent` polls the cloud for this box's target version.
2. If the target differs from what is running, it applies in the next window — no prompt.

**As built (#401): steps 1 and 2 are one seam, shared with appliance.** `internal/hostagent/updatetarget` defines a single `Source` ("what should this box be running?") with an implementation per profile — hosted reads a configurable update-target URL over the box's existing outbound path, appliance reads the signed release manifest — and **one** loop consumes it: compare, validate, hold for the window, apply. Neither profile has its own copy of the poll, the compare or the apply. The details that matter:

- **The answer names pinned image references, never tags.** `…@sha256:<64 hex>` for both the brain and the UI, resolved once by the sender (see `RELEASE_MANIFEST.md` for the appliance publisher and the cloud's `GET /api/updates/target` for hosted). The box **refuses** an answer that is unpinned, that names only one of the two images, that points at an unexpected repository, or whose carried digest disagrees with its own reference. A refusal is logged and changes nothing — the box stays on its current version, and nothing is pulled.
- **Which repositories count as expected is configuration, not a fixed value.** The default pair is `ghcr.io/onmoose/brain` and `ghcr.io/onmoose/ui` (`BUILD.md` # 6 — public, pulled by digest). Each can be changed on its own with `MOOSE_UPDATE_BRAIN_REPO` and `MOOSE_UPDATE_UI_REPO`. They exist for two reasons: the CI boot proof serves both images from a registry **inside the guest**, and a box under test may point at another registry. The repository is not what pins the bytes — the digest does that. It is the check that stops a well-formed answer from sending a box to someone else's image. Changing it on a production box is as deliberate an act as pointing that box at a non-default target.
- **An unreachable source and a "no target" answer are both no-ops**, and are distinct in the logs. A box never degrades, refuses to serve, or rolls back because it could not ask.
- **The window is the hosted apply gate**: 03:00–04:00 local by default (see the window bullet below for where it can come from), which is why the poll is every 15 minutes rather than hourly — an hourly poll can step over an hour-wide window. Within one window a box makes **one** attempt per target version: a failed update has already reverted the box, and retrying it every 15 minutes would be a loop, so the next attempt is the next night.
- **The apply goes through host-agent's job lock**, the same one `POST /v1/jobs/system-update` takes, so a target-driven update and an admin-triggered one can never run at once.
- **Appliance learns but does not apply.** The seam is shared; # 8.2 is not. On appliance the control plane stays admin-prompted (# 3), so the loop stops at "a different control plane is available".
- **As built (#443), what the loop decided is readable, not just logged.** The loop keeps its last tick — the outcome, the target, when it checked and why it produced nothing — and host-agent serves it at `GET /v1/system/update-target`, which the brain re-serves admin-only at `GET /api/v1/system/update-target` (`BRAIN_HOST_PROTOCOL.md` has the payload and the seven `state` values). This is the read the dashboard prompt consumes; until it existed, "a different control plane is available" was a journal line and the appliance prompt # 3 promises had nothing to read. Three things it settles:
  - **The running pair is read on request**, from `images.json` and the staged compose, never carried from the last tick. A tick that ends early never reads it, and host-agent outlives an update, so a stored pair would be stale exactly after one.
  - **A box with no loop says so.** When a seeded `update_target_url` is unusable, host-agent refuses it and starts no loop (above). That box reports `disabled` with the reason, not "nothing to offer" and not "not checked yet" — "this box will never update" and "this box has not checked yet" are different facts, and only the first one needs a human.
  - **The two `from` settings stay apart on the wire.** `from` is where the target URL came from (`seed`, `env`, `default`); `window_from` is where the window came from (`answer`, `env`, `default`). They are different settings with lookalike values, and one field for both would report a window source as a URL source.
- **As built (#407), the target is a field in the provisioning seed: `update_target_url`.** This is what makes per-box pinning possible at all: a hosted box has no SSH (`ENVIRONMENT.md` # Access & files), so without it there is no way to point an already-provisioned box at `?channel=candidate`. The seed is the home because it is **the only channel a real box has for a per-box fact**, and because it is written once when the VM is created — which fits a fact fixed for the life of the box, like which control plane it belongs to. The precedence is **seed, then `MOOSE_UPDATE_TARGET_URL`, then the built-in default**: the seed is the per-box fact from the provisioner, and the environment variable stays below it as the local hand-edit an operator drop-in or the CI boot proof uses. A box seeded without the field behaves exactly as it did before. Which source won is logged once at startup, and a box on a non-default target logs it at **warn** level, because "this box is not following the fleet" is the first thing to know when one box behaves unlike the rest. host-agent reads the seed file itself rather than through the brain's reader, so a box seeded only for updates and a box with no seed at all both work.
  - **#404 built this as two `ImportCredential=` lines on `host-agent.service`, and they reached no real box.** The claim it was built on — that credentials are "the same path `moose.seed` already arrives on" — was wrong: `moose.seed` is a credential only in the QEMU lane, and a real box gets its per-box facts as metadata user-data, which does not feed systemd's credential store (`ENVIRONMENT.md` # Provisioning & first-boot — the write-once channel bullet). #407 deleted both lines and the code behind them; there is no credential path left to fall back to.
  - **The window did not move to the seed, and will not.** A window has to be changeable while the box runs, and the seed can never be rewritten.
- **As built (#408), the window comes from the answer first: the answer, then `MOOSE_UPDATE_WINDOW`, then the built-in default.** The answer carries an optional `window` field in `HH:MM-HH:MM` form, and that is the home the bullet above was waiting for — the control plane can now move one box's hour while the box is running, the same way it moves its version. **An answer that leaves the field out has no opinion**, and the box keeps what it is configured with. That is not the same as "use the default": reading an absent field as the default would silently outrank an operator's `MOOSE_UPDATE_WINDOW`, and it is what lets this ship before the control plane sends a window at all. A window the box cannot read **warns and falls back** to the setting below it, for the same reason as always — a wrong hour only applies an update at the wrong time. Where the window came from is in the log as `from`, one of `answer`, `env` or `default`.
- **An unusable target is refused, not resolved away (#404, #407).** If the seed's `update_target_url` is set but is not an absolute `http` or `https` URL with a host, host-agent logs an error and **does not start the update loop at all**. It does not fall back to the fleet endpoint: a box that was deliberately pinned to a candidate must never quietly join stable because of a typo. The box keeps serving whatever it is already running. **A seed that will not parse is refused the same way**, because the bytes we could not read might have carried a target and we cannot tell; a seed that is simply absent is not an error and falls through, since that is the appliance and the un-steered hosted box. An unusable **window** is not treated like any of this and falls back to 03:00-04:00 with a warning, because a wrong hour can only apply an update at the wrong time, while a wrong target sends the box to the wrong version.
- **The loop applies whatever the target names, in either direction. A box running something newer than its target rolls back to it.** The compare is "does the running pair differ from the target pair", with no notion of "forward", so setting a box's target to an older release is how a deliberate downgrade is performed, and pointing a box at a stale target is how one happens by accident. This is correct (per-box pinning wants deliberate downgrades) and it is surprising, so it is written here rather than left to be discovered. It nearly bit us the day the loop shipped: a box provisioned from the v0.7.0 image would have rolled itself back to v0.6.0 overnight had `stable` not been promoted the same hour. **Before pointing a box at a target, check which way it moves that box.**
3. **Stream B:** pull by digest, snapshot the brain's SQLite, write the staged compose, recreate the changed containers, health-check, revert both on failure of either (# 3).
4. **Stream A:** unchanged from # 1 and # 2 — `unattended-upgrades` security-only plus our apt repo, last in the window, reboot opportunistically. A hosted VM reboot is cheaper than an appliance one: no user is physically waiting, and the window is ours.
5. The box reports the outcome back to the cloud: version now running, success or failure, and the failure mode if it rolled back.

Step 5 is what the whole design is for. On appliance our visibility into a bad release is "GitHub issues and the forum, hours to days" (# 3). On hosted it is a fleet view that tells us a version is failing before the second box tries it.

### 8.5 Deferred on hosted

- **Per-box targeting UI and cohort management** are cloud-side concerns (`onmoose/cloud`), not box-side. This doc specifies what the box asks for and obeys; how an operator sets it is the other repo's problem.
- **Auto-rollback triggered by the fleet**, rather than by the box's own health check. If three boxes fail the same target, the cloud should stop handing that target out. Sensible, cloud-side, not v1.

---

## Locked decisions

- **Two update streams, split by what the thing runs on:** stream A is **the box** (Debian base + kernel + firmware + `host-agent`) — one atomic unit, may reboot, realized by `apt` today and by an A/B image later. Stream B is **the containers** (brain, UI, apps, managed services) — per-image, no reboot, rollback by keeping the previous image. Flipped from the earlier five-stream model in `DECISIONS.md` 2026-08-11.
- **Two-track posture, modeled after Android:** silent auto-apply for security patches; admin-prompted for anything that changes meaningful surface (brain, app permissions, OS major upgrades).
- **Debian base: `unattended-upgrades` security-only.** Full upgrades and Debian point-releases stay admin-triggered until A/B images.
- **`host-agent`: `unattended-upgrades` from our apt repo.**
- **Control plane (`moose-brain` + `moose-ui`): release-manifest-driven, admin-prompted.** Manifest carries `brain`, `ui`, `minimum_host_agent`, and `rollback_to` (full schema + signing + publishing pipeline in `RELEASE_MANIFEST.md`). v1 ships a single `stable` channel; phased rollout and beta channel are deferred (additive when triggers fire — see `RELEASE_MANIFEST.md` # Future work). Telemetry is a halt-fast signal, not a rollout gate. Updater recreates only what changed; brain+UI revert as a pair on failure.
- **Apps: auto-update by default** (per `SPEC.md`); **prompt the instance owner only when the manifest's `permissions:` block expands** (new key or widened value). Permission-neutral updates of any size auto-apply. Different users on the same box may temporarily run different versions of the same app — by design, since instances are already per-user isolated. Tier-2 apps prompt the admin (box-wide).
- **Pre-update snapshot of `data_volumes` (plus `pg_dump` of any managed-service DB)** is taken before every app update. Restored on health-check failure alongside the image revert. Hooks remain deferred; the snapshot is the v1 safety net for app-driven schema migrations. Kept 7 days.
- **Permission-expansion prompt surfaces on next login of the instance owner**, modal on first dashboard load, two buttons (Allow & update / Keep current version). Accept applies immediately, not at 03:00. Admin sees per-user pending-update facts in Settings → Users; cannot accept on another user's behalf.
- **Update surfaces in the dashboard:** per-app tile badge for available/applied/failed; Settings → Updates for the aggregate view and rollback affordance; auto-dismissing toast for overnight batches.
- **Managed services: brain-owned, no user toggle.** Patches in update window; cross-major migrations triggered transparently by app updates with a pre-migration backup.
- **Update window: 03:00–04:00 local** for apps, managed services, Debian base, reboots. Configurable, advanced setting. Brain has no fixed window — it's admin-triggered.
- **Update ordering: stream B before stream A** — brain + UI → apps & managed services → the box (which may reboot). Stream A's internal order is apt's business, not ours.
- **Reboots: opportunistic in window only.** Surface a dashboard nag after 7 days; never force.
- **Rollback: previous image + state snapshot kept for 7 days** for brain, apps, and managed-service patches. Debian base has no rollback in v1.
- **All updates require internet; offline boxes stay current at their last-applied versions.**
- **Hosted: the target version is per-box, held by the cloud control plane** (# 8.1). No `stable.json`, no minisign, no hourly manifest poll — that path is appliance-only (`RELEASE_MANIFEST.md`). The box polls outbound; the cloud never connects in.
- **Hosted: updates are pushed, not prompted** (# 8.2). They apply in the window and the tenant admin is notified afterwards. **The app permission-expansion prompt is the one carve-out** — it still goes to the instance owner, because widening access to a user's data is their decision even on a box we operate.
- **Hosted and appliance share the apply/rollback mechanics** (# 8). Only the trigger differs. The signed-manifest trigger and the cloud-target trigger are two thin paths onto one transaction.
- **`host-agent` recreates both `moose-brain` and `moose-ui`** as one transaction (# 8.3). It writes the new image references first, so the brain reconciles to the same versions on restart instead of fighting them back. **Two files carry that declaration**, both in `MOOSE_CONTROL_PLANE_DIR`: the staged `compose.yml` holds the UI's reference (the brain reads it), and `images.json` — a ledger of the current pair, the previous pair, and when each was applied — holds the brain's (host-agent reads it at boot). The brain is not in the compose because a process cannot bring itself up; without the ledger, a box that lost its brain container would relaunch the reference baked at build time and silently undo an applied update.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
