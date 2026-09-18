# moose — What's Next

> The single, prioritized list of design topics we still need to cover. Replaces the "Open questions" tail of every other doc. Companion to `DECISIONS.md` (what we figured out, and why).

## How to use this doc

- **Tier 1** = blocks other design work or implementation. Tackle these first.
- **Tier 2** = shapes UX or developer surface; not strictly blocking but compounds if delayed.
- **Tier 3** = can be deferred without retrofitting risk, but pinning the shape now is cheap insurance.
- **Tier 4** = bikesheds, point-decisions, or small open items. Park here so they aren't forgotten; pull into a higher tier when they bite.

Each entry: one-sentence shape, the doc it touches, and *why this tier*. The doc is the source of context — read it before opening the topic.

**This is a *design* backlog, not an implementation backlog.** An entry means "we haven't decided the shape yet." The moment a topic's design is **locked** — its shape written into the relevant spec (+ a `DECISIONS.md` entry if it flipped a position) — it leaves this doc. Opening the contributor issue (or queuing it in `../progress/README.md` # Up next) is part of that **same change**: lock the spec, remove the entry here, file the issue, together. Do **not** defer the removal to the implementing PR — that leaves NEXT.md claiming a design is open for the whole time the issue sits in the backlog, which is exactly the staleness this doc must not accumulate. If an issue covers only *part* of an entry, scope the entry down to what remains (see the image-cleanup and lint-tool entries for the pattern) rather than deleting it.

---

## Tier 1 — Blocking

*(Last audit: 2026-05-31 — Tier 1 is **clear of product-surface gaps**. The dashboard-shell gap was resolved by `DASHBOARD.md` (logged-in IA + owner-scoped apps model; `DECISIONS.md` 2026-05-29), and the in-dashboard file-manager gap is now resolved by `FILES.md` (ops execute as the user's UID in host-agent; own + Shared scope; `DECISIONS.md` 2026-05-31). The infrastructure spine (boot/storage/health/updates/auth) is well-specified. Remaining work is largely implementation, not design — slice queue lives in [`../progress/README.md`](../progress/README.md) # Up next. Promote an item here into Tier 1 if a new blocking design gap appears. **2026-08-11:** one did — the hosted update path (`UPDATES.md` # 8) landed with a designed trigger and no designed credential to authenticate it; see below.)*

*(Items resolved out of this tier are recorded in `DECISIONS.md` — including the 2026-06-17 #199 resolution that mkosi emits no `.iso` and moose ships disk images instead, cloud VM image first.)*

### Box ↔ cloud API authentication (hosted)

`UPDATES.md` # 8 locks the hosted update trigger: the box polls the cloud control plane for its per-box target version, outbound-only. **What authenticates that call is undesigned.** The `enrollment` block the box receives in `seed.json` (`ENVIRONMENT.md` # Owner sign-in & seed ingestion — as built) is an acme-dns account — scoped to writing one DNS TXT record, not to a general control-plane API. So the hosted update path has a designed trigger and no credential to make the call with.

**Where this stands as built (#408).** A hosted box now says which box it is when it asks for its update target: `GET <target-url>?box_id=<id>` (`UPDATES.md` # 8.1). That endpoint is public and needs no login, so the identity is a claim, not proof. Anyone who learns a box-id can read what that box is told to run, and can ask as that box.

We accept that for now, for two reasons. The ask is only a **read** of version information. And the operator path that sets a target sits behind the control plane's own login.

It stops being enough as soon as the box **writes**. Two things need that: the report-back in `UPDATES.md` # 8.4 step 5 ("this box now runs v0.7.0", or "it failed and rolled back"), and the fleet auto-halt in # 8.5. Both need the control plane to trust who is talking. A fleet view built on an unauthenticated report is one that anyone can lie to. **So build nothing that changes state on top of a bare `box_id`.**

Shape to decide: does the box get a second, separate credential at provision time (simple, one more secret to seed and rotate), or does the `box_id` + a control-plane-issued token become a general-purpose box identity that later features (metering, suspend/restore, fleet management — `ENVIRONMENT.md` # Deferred) also use? The second is more work now and is almost certainly what we end up needing, which is the argument for not designing the first one twice. Also open: rotation, and what a box does when its credential is rejected (keep running and keep serving, is the obvious answer — an auth failure must never take a tenant's apps down).

The **seed is how it gets there.** It is the only per-box channel a real box has, and it is written once (`ENVIRONMENT.md` # Provisioning & first-boot). So the shape to design is a long-lived per-box secret, plus a way to rotate it on the box.

**Context:** `UPDATES.md` # 8.1, `ENVIRONMENT.md` # Updates (hosted) + # Provisioning, `onmoose/cloud` (the other half lives there).
**Why Tier 1:** it now blocks the **write** half, not the read half. The trigger itself shipped in #402/#408 on a bare `box_id`, and apply/rollback shipped before it. What cannot be built until this is designed is the box reporting its outcome back (`UPDATES.md` # 8.4 step 5) and the fleet auto-halt that reads those reports (# 8.5). Those are the point of the hosted update path, so the work stops here.

---

## Tier 2 — UX-shaping

### Off-box notification transports (`NOTIFICATIONS.md` # transport-agnostic seam)

The in-product notification center (the dashboard bell) is **decided and specced in `NOTIFICATIONS.md`** — v1 is dashboard-only. What remains open is the *off-box* delivery that actually reaches the user who isn't looking at the dashboard (the pantry-box case the bell deliberately doesn't solve): **email** and **mobile push**. Both slot in behind the transport-agnostic seam already designed in `NOTIFICATIONS.md` — no model rework, just new sinks + per-user/per-category/per-severity delivery preferences. Email is gated on **email-on-file** (separate Tier-2 item below); push is gated on the mobile app (deferred with the mesh). Also still open: quiet-hours / snooze and daily-digest, which only earn their keep once an off-box transport exists.

**Context:** `NOTIFICATIONS.md` (the model + seam), email-on-file entry below, `MOOSE_NETWORK.md` (SMTP relay — own vs. transactional; residential-IP deliverability), mobile app (deferred, `MOOSE_NETWORK.md` # mesh).
**Why Tier 2:** the bell closes the "I missed a transient banner" gap; it does **not** close the "nobody's looking" gap. Until an off-box transport lands, a degraded drive in a pantry goes unseen until someone next opens the dashboard.
**Prior art:** Synology DSM (push/email/SMS center) is the target shape; TrueNAS pairs an always-on in-UI bell with opt-in transport services — exactly the seam we built. v1 = bell only (Umbrel's level, done well); off-box transports move us toward Synology-lite.

### UPS, clean shutdown, power-event handling

The box is plugged into a wall and lives 24/7 in a pantry. Power blip → unclean LUKS shutdown → degraded mode at boot. v1 minimum: graceful shutdown on power-button press, a "Shut down / Restart" affordance in the dashboard, and (if a USB UPS is attached) low-battery hook via NUT. Affects `BOOT.md` (clean-shutdown path), `HEALTH.md` (a "running on battery" issue type), and the install/wizard story (do we detect a UPS at first run?).

**Context:** `BOOT.md`, `HEALTH.md`, `FIRST_RUN.md`, `AUTH.md` (who can shut down the box from the UI).
**Why Tier 2:** Synology/TrueNAS treat this as table stakes. Most boxes will never see a UPS, but "shut down from the dashboard" and "don't corrupt on power blip" are universal.

### i18n posture (v1 + schema door)

Two-part decision: (a) is v1 English-only? (very probably yes); (b) does the manifest schema *allow* localized fields (`name`, `description`, `category`) so a future translation pass doesn't require a breaking schema bump? Yunohost is fully multilingual and we'll get the ask. Decide the door now; defer the translations.

**Context:** `APP_MANIFEST.md`, `APP_STORE.md`, `WEB_UI.md`.
**Why Tier 2:** schema-shaping; literally one or two field-shape decisions in `APP_MANIFEST.md`.

### Store catalog curation policy

The publish *mechanism* is settled (`DECISIONS.md` 2026-07-02): the catalog service serves a CI-validated catalog over its HTTP API (`GET /catalog?env=` for browse data, per-app document routes for the install payload), the box fetches it as a thin client and keeps no browse copy on disk, and trust is TLS — no signing and no integrity digest (`DECISIONS.md` 2026-09-04). What's still open is the **content policy** the maintainer enforces in review: do we reject manifests that set `storage.app_managed_user_content: true`, or only label them with the absence of the `files_first_class` badge? Do we require apps to log to stdout/stderr (no `logging.driver:` overrides, no in-`command:` file redirects) so the dashboard Logs tab works (`LOGGING.md` # Apps are expected to log to stdout)? What other criteria gate inclusion (license, upstream maintenance signals, declared-vs-actual permission audit)?

**Decided — app-bundled databases (incl. copyleft engines like MongoDB/SSPL).** An app that bundles its own database engine in its compose (the Umbrel/CasaOS pattern) is **accepted**, including engines moose will not offer as a managed Tier-1 service. The test is *who serves the database*: an app that uses the engine **internally for its own data** (Rocket.Chat → its bundled MongoDB) is fine — the app offers its own functionality, not the database's, so no copyleft service obligation (SSPL §13 / AGPL) reaches moose or the user. **Reject** only an app whose *own purpose is to serve a generic database to third parties* (a DBaaS panel, a hosted-Mongo admin app) — there the bundling doesn't launder the service trigger. This is the curation-side corollary of "moose will not offer a managed `type: mongodb`" (`DECISIONS.md` 2026-06-25, `docs/progress/mongodb-compat-spike.md`): a Mongo-needing app is **not** rejected for the absence of a managed Mongo service — it bundles its own. The bundled engine's data is app-state, not files-first-class user content, so such an app does not earn the `files_first_class` badge; that is expected, not a defect. One operational note for whoever ships offline bundles: baking a Mongo-bundling app into an air-gapped bundle redistributes the SSPL bytes, carrying the standard source-offer duty (`docs/progress/mongodb-compat-spike.md` # The decision).

**Context:** `APP_STORE.md` # v1 catalog is hand-curated by moose, `APP_MANIFEST.md` # External-storage convention, `STORAGE.md` # Files are first-class, `LOGGING.md`, `DECISIONS.md` 2026-06-25 (managed MongoDB declined).
**Why Tier 2:** the "files are first-class" principle only holds if curation defends it. Decided lazily, the catalog fills with opaque-library apps and the principle erodes. The mechanism is in place; the bar isn't written down.
**Prior art:** Yunohost's app integration-level (0–9) rating is a useful model for surfacing curation outcomes as a small badge set (e.g. `Files-first-class`, `Backup-aware`, `Multi-instance`, `Stdout-clean`) — a single number is too coarse, but the underlying idea of "show users the integration-quality signal without exposing manifest internals" maps well.

### Shared folder management UX

Who can add/rename subfolders under `~/Shared/`? How does an admin see what's in there vs. per-user? Does each user get a per-user view filtered to "things I have access to," or a flat view? Group management for `moose-shared` (kick a user off shared content) — Settings surface.

**Context:** `STORAGE.md` # Permissions, `AUTH.md`.
**Why Tier 2:** the on-disk mechanics are pinned; the dashboard UX isn't. Households without this can still use Shared/ via SMB, but the in-dashboard view needs design before public release.

### First-run restore / migrate-from-old-box fork

`FIRST_RUN.md` is greenfield-only today: wipe → wizard → dashboard. There is no branch for "I'm replacing my old box" or "restore from backup." Even though off-site backup itself is deferred (Tier-3 "Backup architecture shape"), the *fork* in the wizard shapes first-run now — a "Set up as new" vs. "Restore from backup / migrate from another box" choice early in the flow. Reserve it now or retrofit the wizard later. Umbrel ships a data-export / migration path; cross-box migration is also tracked under Tier-4 "Backup & migration."

**Context:** `FIRST_RUN.md`, Tier-3 "Backup architecture shape," Tier-4 "Cross-box migration."
**Why Tier 2:** it's a structural branch in the first-run flow; cheap to reserve the fork now, expensive to wedge in after the happy-path wizard is built.

### Email-on-file for users

Required for password recovery, product comms, and any future cloud-account linking. Currently sidestepped because all three are deferred — but the *decision* of whether to collect at first-run shapes the wizard.

**Context:** `FIRST_RUN.md`.
**Why Tier 2:** decided now or it becomes a forced retrofit later. Likely answer: optional field at user creation, used for recovery only.

### App-secret injection hardening (`SERVICE_PROVISIONING.md` # Env-var injection)

The *mechanism* — a manifest `secrets:` declaration → brain generates a CSPRNG value once, persists it, injects `MOOSE_SECRET_<NAME>`, re-emits it stably — is **shipped and specced** (`APP_MANIFEST.md` # D2, `DECISIONS.md` 2026-06-05). What's deliberately deferred is the security hardening around it; the v1 implementation is correct but not yet hardened, and these were reviewed and parked, not missed:

- **`.env` file permissions.** The instance `.env` is written world-readable (`0o644`). Now that it holds a signing secret, a non-admin local account can read every app's secret. Decide `0o600` + root-owned, and whether that's the answer for *all* injected vars or just secrets.
- **Env-var delivery surface.** A value in the container environment is visible via `docker inspect`, `/proc/<pid>/environ`, and child-process inheritance — the classic leak is an app's own crash reporter shipping `process.env` off-box (and these apps tend to declare `internet: true`). The safer shape is the Docker-secret / `_FILE` convention (mount the value as a file, app reads `*_FILE`), but it needs per-app support. **Superseded in principle by # On-box credential broker below** — a broker keeps the real value out of the container entirely, which `_FILE` does not; do not build `_FILE` first. What stays open here is only whether env-var is the accepted trade-off until the broker lands.
- **At-rest encryption.** The value is stored plaintext in SQLite and on disk in `.env`. `SERVICE_PROVISIONING.md` promises managed-service creds "encrypted at rest" — unbuilt, and the same decision covers secrets and the outgoing-mail provider passwords (`mail_providers.password`, # BYO outgoing mail — the one credential that unlocks an *external* account, not just a box-local DB). Relationship to LUKS (covers a powered-off stolen drive) vs. row-level encryption (covers a live box / a leaked DB) needs to be drawn explicitly.
- **Backups.** A signing secret must travel in the app's backup archive (a restored app has to keep validating old tokens), which makes backup-archive encryption load-bearing the moment secrets exist. Gate with the backup design (# Backup architecture shape).
- **Rotation + log hygiene.** Env-injected secrets can't rotate without restarting the container and invalidating live tokens — no recovery story beyond reset. And moose's own logs/audit/compose-output must never surface the value (one watch point: `ComposeUp` returns `CombinedOutput()` into install errors).

**Context:** `SERVICE_PROVISIONING.md`, `APP_MANIFEST.md` # D2, `THREAT_MODEL.md` (adversaries: compromised app at runtime, stolen drive), `STORAGE.md` # LUKS, the backup entry below.
**Why Tier 2:** the leak surface compounds — every app installed before `.env` is locked down ships an exposed secret on disk, so the longer it waits the larger the retrofit. Not strictly blocking (household trust + skeleton status make the current shape tolerable), but it shouldn't ride to v1 unhardened.

### On-box credential broker — apps get fake keys, moose holds the real ones

Today every injected credential — a mail provider's password, a user-supplied API key (`APP_MANIFEST.md` # D4), a managed-service password — reaches the app as its **real value** in the container environment. The entry above lists what that costs and proposes the Docker-secret `_FILE` convention as the mitigation. **A broker is the better answer, and it supersedes that bullet:** the app is injected with a *fake* credential that only moose can resolve, and an on-box proxy swaps it for the real one on the way out. The real value never enters the container in any form, so there is nothing for `docker inspect`, `/proc/<pid>/environ`, or the app's own crash reporter to leak. **Do not build `_FILE` first** — it is a weaker fix to the same problem and the two do not compose.

The scope is wider than mail. It is the general answer for any credential an app must *use* but should not *hold*: AI provider keys (the case that motivates it — an app calling OpenAI or Anthropic on the user's key), third-party API tokens, and outgoing mail. Rough shape:

- **API keys first, mail second.** API keys are HTTP — intercept the request, swap an auth header, forward. Mail is SMTP, which means running a submission server plus MAIL FROM validation, queueing, bounces, and abuse handling. Same machine, much harder protocol. Prove the mechanism on the easy one.
- **Mail on the broker reopens `DECISIONS.md` 2026-06-12** (no moose relay). That rejection was reasoned on *residential-IP deliverability*, which is an appliance fact; it does not hold for a hosted box relaying through a real provider. Reopen it deliberately when mail moves, rather than routing around it.
- **The provider type is the join key.** The broker has to know which provider a fake credential maps to. `mail_providers.provider_type` (added with the provider presets) is that identity for mail; the equivalent is needed for API-key config fields.
- **Open:** where the proxy runs (host-agent, a brain-managed container, a sidecar per app), how TLS interception is avoided or handled, what an app sees when the real credential is rejected upstream, and the abuse/rate-limit posture once moose is in the request path.

**Context:** # App-secret injection hardening above, `SERVICE_PROVISIONING.md` # Env-var injection + # BYO outgoing mail, `APP_MANIFEST.md` # D4, `THREAT_MODEL.md` (compromised app at runtime), `DECISIONS.md` 2026-06-12.
**Why Tier 2:** it changes what a credential *is* from the app's point of view, so the longer the fleet runs on real-value injection the more credentials need rotating when it lands. It also decides whether `_FILE` gets built at all, which is live work today.

### Where a control-plane update's outcome lives after it finishes

`POST /api/v1/system/update` returns a job id, and `GET /v1/jobs/{id}` answers from an **in-memory** record in host-agent. Lose the id — the admin closed the tab, the dashboard reloaded — or restart host-agent, and the outcome is gone: it survives only as the pair named in the ledger (`images.json`) and whatever went to the journal. Nothing can answer "did last night's update work, and if not, why."

Three committed surfaces need that answer, so the shape has to be decided before any of them is built:

- **Settings → Updates** (`UPDATES.md` # 6) promises an aggregate view — what updated, what is waiting, what failed — with the rollback affordance on it.
- **The notification** (`UPDATES.md` # 8.2, `NOTIFICATIONS.md`) is the durable, read-stateful copy for an admin who was not watching; a notification whose detail is already gone is not much of a record.
- **The hosted report back to the cloud** (`UPDATES.md` # 8.4 step 5) is "version now running, success or failure, and the failure mode if it rolled back" — a box that reboots mid-window must still be able to send it.

The open question is **what persists and where**, not whether. Options worth weighing: a `system_update` row in the brain's SQLite (natural home for anything the dashboard reads, but the brain is the thing being replaced — it cannot record its own last moments); a small host-agent-side file next to the ledger (survives the brain, but adds a second store to the one `images.json` already is); or extending the ledger itself with the outcome of the transition it already records (fewest moving parts, but turns a declaration into a log). Whichever wins also decides whether job records ever need persistence at all, which is why it is worth settling before a second job kind arrives.

**Context:** `UPDATES.md` # 3 (failure signalling), # 6, # 8.2, # 8.4; `NOTIFICATIONS.md`; `internal/hostagent/jobs.go`; `internal/hostagent/controlplane` (the ledger); `docs/progress/control-plane-update-trigger.md` # Known gaps.
**Why Tier 2:** nothing is blocked today — the transaction reverts correctly whether or not anyone reads the result — but all three surfaces above are already promised in locked specs, and each would otherwise invent its own answer. Deciding once is cheap; discovering three incompatible records later is not.

### The release manifest names versions, so an appliance can never apply one

The box has one update-target seam (`UPDATES.md` # 8.4, as built in #401) and it only acts on an image reference **pinned to a digest**: a box that resolved a tag itself would be trusting a movable label at update time, and two boxes told "1.4.2" a week apart could end up running different bytes. The signed release manifest names versions — `"brain": "1.4.2"` — with the registry path left implicit (`RELEASE_MANIFEST.md` # What the manifest is), and a version can only be turned into a tag. So the appliance source reads a verified manifest and **refuses it as unpinned**. The seam is real on both profiles; only the hosted one can currently produce a target.

The obvious fix is optional pinned-reference fields (`brain_image`, `ui_image`) alongside the versions — additive, no `manifest_version` bump, since unknown fields are already ignored. What makes it a decision rather than a patch is that `RELEASE_MANIFEST.md` deliberately keeps image URLs out of the file so the registry can move without re-cutting every signed manifest, and a pinned reference bakes the registry host into every one of them. Weigh that against inventing a publisher-side resolve step, or a separate signed digest map beside the manifest.

**Context:** `RELEASE_MANIFEST.md` # What the manifest is; `UPDATES.md` # 8.4; `internal/hostagent/updatetarget/manifest.go`; `docs/progress/update-target-source.md`.
**Why Tier 2:** nothing is blocked — no signing key is baked into any build, so no appliance verifies a manifest at all today, and the appliance updater is inert for that reason first. It has to be settled with the appliance release pipeline, before the first signed manifest is published, because the schema is easier to widen before there are files in the wild than after.

---

## Tier 3 — Defer-able, but pin the shape

### Align the QEMU test lane to Trixie (currently Bookworm)

`BUILD.md` # 1 locks Debian Trixie (13) as the product base, and the cloud-image profile (#196 C1b) targets Trixie. But the existing medium lane (`dev/test-qemu/mkosi.conf`) is still `Release=bookworm`. C1b carries a "fall back to Bookworm if Trixie fights mkosi" escape hatch, so the cloud image could silently land on Bookworm and diverge from the product spec with no tracking. Once the cloud image validates Trixie, align the test lane to Trixie too (or, if Trixie proves unworkable, record the Bookworm decision in `DECISIONS.md` rather than leaving it implicit in one issue).

**Context:** `BUILD.md` # 1, `dev/test-qemu/mkosi.conf`, #196 C1b (#203).
**Why Tier 3:** doesn't block cloud bring-up (either base boots); the risk is a silent spec/reality drift on the base release. Pin the resolution when C1b/C2 confirm Trixie.

### In-guest `nftables` default-deny for hosted (belt-and-suspenders)

Hosted ships **one** moose-owned in-guest nftables rule: a standing forward-hook DROP of app-container egress to the cloud metadata endpoint (`169.254.169.254`), loaded at boot by `moose-metadata-firewall.service` before docker or host-agent start any container (`DECISIONS.md` 2026-06-25, `ENVIRONMENT.md` # Provisioning & first-boot, #251). That targeted SSRF control is realized and not what this entry is about.

What remains open is a **general** default-deny backstop: L3/L4 filtering is the cloud provider's security groups / VPC firewall, recorded as an explicit operator requirement (`DECISIONS.md` 2026-06-19, #203). That is the right default for a provider-fronted VM, but it leaves the Docker-publishes-to-`0.0.0.0` behavior acceptable *only* because the provider firewall is assumed present. If a hosted target ever lacks a security-group layer (a bare provider, an on-prem "hosted-style" deploy), a minimal in-guest `nftables` default-deny (admit 443 + the ACME/redirect 80, drop the rest) would be the in-VM backstop. The `nftables` package and the `inet moose_metadata` table are already in the image — the general backstop is a broader ruleset + a host-agent seam that owns and reconciles it, not a new package. Pin the ruleset shape and the host-agent seam that would own it before that scenario is real.

**Context:** `DECISIONS.md` 2026-06-19 and 2026-06-25, `ENVIRONMENT.md` # Public-by-default, `BUILD.md` # SSH (the appliance `nftables` posture it would *not* reuse — different subjects).
**Why Tier 3:** not needed for the provider-fronted v1 (the security group covers it); it is a hardening option for a provider posture v1 doesn't target. No bring-up dependency.

### Factory reset / repurpose / "start over"

Explicitly undocumented today — `USERS_AND_GROUPS.md` # Known gaps admits "we don't have a clean 'reset everything except user content' story yet; treat reinstall + restore from backup as the floor." This is an end-to-end lifecycle gap: resale, household handoff, "I broke it and want a clean slate." It has a security dimension beyond UX — securely destroying LUKS keyslots so the outgoing drive is unreadable — so it's not purely a dashboard flow. Both Synology and Umbrel treat reset/repurpose as standard. Open: scope (full wipe vs. reset-config-keep-content vs. reset-keep-nothing), where it lives in the UI (Settings → Advanced, gated by fresh password), and the key-destruction mechanics.

**Context:** `USERS_AND_GROUPS.md` # Known gaps, `STORAGE.md` (LUKS keyslot destruction), `AUTH.md` (elevation gate), `FIRST_RUN.md` (post-reset re-onboarding), `BUILD.md` (relationship to reinstall).
**Why Tier 3:** reinstall-from-ISO is the v1 floor, so this doesn't block bring-up; bites the first time someone resells or hands off a box. Pin the shape (esp. key destruction) before public release.

### Brain state-migration framework (SQLite schema + on-disk layout)

App-level and managed-service migration are well-specced (`SERVICE_PROVISIONING.md` cross-version auto-migrate, `UPDATES.md` pre-update snapshot). The **brain's own** SQLite schema + on-disk instance-layout migration across auto-updates is only *referenced* (LOGGING.md mentions "buggy migrations" defensively) — no doc owns the mechanism. For an auto-updating fleet this is load-bearing: a bad brain migration bricks boxes with no UI left to recover from. Umbrel carries dedicated `migration` and `startup-migrations` modules for exactly this. Pin the shape: versioned, forward-only, transactional, run-before-serving, tested in the boot lanes, and how it interacts with brain-version rollback (`RELEASE_MANIFEST.md` `rollback_to`).

**Context:** `CONTROL_PLANE.md` (brain lifecycle), `UPDATES.md` (brain+UI stream, rollback), `RELEASE_MANIFEST.md` (`rollback_to`), `LOGGING.md` (append-only audit triggers vs. migrations), `TESTING.md` (boot-lane coverage).
**Why Tier 3:** the brain schema is small and stable today, so it doesn't bite at current scale; becomes a fleet-bricking risk the moment a migration ships wrong. Pin the discipline before the schema grows.

### OS major-version upgrade commitment

`UPDATES.md` covers both streams under one Debian release. What about Debian 12 → 13? Options: in-place `do-release-upgrade` (Debian's blessed path, sometimes brittle), image-based A/B (HexOS / ChromeOS shape — clean rollback, doubles OS-drive footprint), or "reinstall + import data" (cheap to ship, terrible UX). The *commitment* (will we ever expect users to reinstall to get a new Debian major?) is a position to take now; the mechanism can wait.

**Context:** `UPDATES.md`, `STORAGE.md` (system dataset vs. data drive split makes image-based A/B more feasible), `BUILD.md`.
**Why Tier 3:** doesn't bite until Debian cuts the next stable (~2027). Pin the commitment now so design choices don't accidentally foreclose A/B.

### Outgoing mail — what stays deferred past BYO (`SERVICE_PROVISIONING.md` # BYO outgoing mail)

The v1 shape is shipped (#122): admin-registered SMTP providers, per-app bindings, `MOOSE_MAIL_*` direct injection, no moose relay. Deliberately deferred, in rough order of likely demand:

- **A box-default provider.** Today every mail-capable app is bound explicitly; a "use for new apps automatically" default would remove a picker step once a box has exactly one provider it always uses.
- **Brain-sent email riding the same providers.** Notification email digests, password-recovery mail (`# Email-on-file for users` above) — the brain becoming a *consumer* of the provider registry rather than just an injector. This is the promotion trigger: the moment email goes cross-cutting (brain + apps), the `SERVICE_PROVISIONING.md` section graduates to its own `OUTGOING_MAIL.md`.
- **Re-stamp-on-edit.** A provider edit currently reaches bound apps only at their next rebind/recreate. If edit-propagation demand materializes, the answer is an explicit "apply to N bound apps now" action (visible restarts), not a silent fleet recreate.
- **A moose-provided sending identity.** Every hosted box already has a free domain (`<box-id>.onmoose.io`) whose DNS moose controls — which means moose, and only moose, can publish the SPF/DKIM/DMARC records that every transactional provider requires. Publish them once and a box could send as `noreply@<box-id>.onmoose.io` with zero user configuration, covering the apps that only mail their own users (Kimai invites, Gitea resets, Paperless alerts). Not v1: v1 is bring-your-own-key. The costs to weigh first are shared-zone reputation (one abusive box hurts every other), per-box quotas, and the fact that an ugly auto-generated sender is wrong for the apps that mail *strangers* (Ghost newsletters, DocuSeal signing requests), which need the user's own domain regardless.
- **Relay/smarthost.** Stays rejected, not deferred — residential IPs can't deliver mail, so a box-local relay is a queue plus a deliverability support burden in front of the user's real provider (`DECISIONS.md` 2026-06-12).

**Context:** `SERVICE_PROVISIONING.md` # BYO outgoing mail, `APP_MANIFEST.md` # D3, `DECISIONS.md` 2026-06-12. Password at-rest hardening folds into # App-secret injection hardening above.
**Why Tier 3:** the BYO shape is complete for app demand today; each deferral has a clean additive path that doesn't reshape the v1 contract.

### `moosectl` — on-box CLI

A `moosectl` for admins on the host: rescue operations, scripting hooks, listing apps, tailing logs, triggering updates. Today the host story is "SSH in and... do what?" — there are no commands beyond raw Docker. Either we ship one CLI that wraps the brain↔host-agent surface, or we declare that there is none and SSH is bash + journalctl + docker.

**Context:** `AUTH.md` (SSH as rescue), `BRAIN_HOST_PROTOCOL.md`, `CONTROL_PLANE.md`.
**Why Tier 3:** tinkerers will ask for it on day one; nothing on the v1 critical path depends on it.

### API tokens vs. cookie-only auth

Third-party stores, external tooling, and (later) `moosectl` need non-interactive auth. `AUTH.md` ships cookie-only in v1. Open: do we add long-lived API tokens (user-scoped, listable, revocable in Settings), or push everything through a service-account model? Affects `BRAIN_UI_PROTOCOL.md` (header vs. cookie auth path) and the rate-limit posture (Tier 2).

**Context:** `AUTH.md`, `BRAIN_UI_PROTOCOL.md`, `APP_STORE.md` (third-party stores).
**Why Tier 3:** v1 ships without it; pin the shape before the first third-party integrator forces an ad-hoc answer.

### Permission revocation after install

A user granted Immich access to `~/Photos/`. Six months later they want to revoke it without uninstalling. Today the manifest declares permissions at install; nothing covers the *change* path. UX: per-app permissions screen mirroring iOS/Android's, with consequences spelled out ("Immich will no longer be able to see your photos"). Brain side: re-render compose, restart instance, audit-event.

**Context:** `APP_MANIFEST.md` (`permissions.folders`), `APP_ISOLATION.md`, `APP_LIFECYCLE.md`, `LOGGING.md` (audit).
**Why Tier 3:** install-time grant works for v1; revocation is the second-order feature users will ask for once they've lived with the box for a while.

### Hostname / box-name rename

Can the user change the box's display name after first-run? Cascades into mDNS hostname (`moose.local` → `kitchen.local`?), Let's Encrypt cert SANs on the `MOOSE_NETWORK` side, audit log historical naming, SMB advertisement. Easy to forbid; easy to allow with caveats; expensive to retrofit if we wire the name into too many places. Also the forced-rename path: when Avahi conflict-resolves us to `moose-2.local` (`HEALTH.md` `hostname-conflict`), how aggressively does the dashboard prompt for a real fix vs. let the user ride it out?

**Context:** `FIRST_RUN.md`, `MOOSE_NETWORK.md`, `STORAGE.md` (Samba), `LOGGING.md`, `DISCOVERY.md`, `HEALTH.md`.
**Why Tier 3:** not a v1 feature; pin the architectural separation between "box-id" (stable) and "display name" (mutable) before too much code depends on conflating them.

### Per-app Bonjour service records (`_http._tcp`)

`DISCOVERY.md` ships v1 with per-app A records only — apps don't appear in Finder's "Network" sidebar or Windows Explorer's network view as individually browseable services. Adding `_http._tcp` advertisements per app would surface them there, plus enable some "discover devices on your network" flows in other apps. Easy to add; the open question is whether the dashboard isn't already the right browse surface.

**Context:** `DISCOVERY.md`.
**Why Tier 3:** additive, can land any time. Default position: don't add it; revisit if real demand appears.

### URL-scheme unification

Two URL schemes, two access models in users' heads: `.local` HTTP on the LAN, `<box-id>.onmoose.io` HTTPS via the toggle. The current model accepts the cognitive cost because the alternatives (always cloud, always `.local`, private CA + cert install on every device) each impose worse failure modes. Worth revisiting once we have real first-run analytics: how many users flip the toggle, how many are confused by the scheme switch, do Android households self-select toward enrolled boxes.

**Context:** `MOOSE_NETWORK.md`, `DISCOVERY.md`, `FIRST_RUN.md`.
**Why Tier 3:** unification is a v2 question — needs operational data we don't have yet.

### Documentation surface

Where do user docs and app-author docs live? In-product help drawer, `docs.mooseos.com` (separate Mkdocs/Astro site), `README` files in the catalog repo, all of the above? Yunohost has extensive in-product help; Umbrel docs live on a separate site; TrueNAS has both. Affects the dashboard codebase (`WEB_UI.md`) and the catalog repo layout (`APP_STORE.md`).

**Context:** `WEB_UI.md`, `APP_STORE.md`, `SPEC.md`.
**Why Tier 3:** can ship v1 with a thin docs site and add in-product help later, but the *split* between the two needs deciding before either is written at scale.

### Managed-service lifecycle gaps (Tier-1, post-Postgres-slice)

Postgres provisioning shipped (`docs/progress/managed-services-postgres.md`, `DECISIONS.md` 2026-06-05), then the MySQL family (`DECISIONS.md` 2026-06-09) and Redis (`docs/progress/managed-services-redis.md`, `DECISIONS.md` 2026-06-13 — isolation model resolved: a per-app ACL user with full keyspace, the credential being the boundary, over the logical-DB-number split). Of the `SERVICE_PROVISIONING.md` # Tier 1 spec pieces **deliberately deferred** out of the Postgres slice, three remain, each its own follow-up:

- **Grace-shutdown.** Lazy spinup is built; the symmetric "stop a service version 12h after its last consumer uninstalls" (`SERVICE_PROVISIONING.md` # At app uninstall, # Versioning) is not — services stay running. Needs a timer/GC and reconcile integration that checks remaining `service_grants` for the kind+version.
- **Cross-version migration.** Auto-migrate on a major-version bump (`pg_dump` old → `pg_restore` new, pre-migration backup as rollback) is unbuilt; gated on the backup design below.
- **At-rest encryption of service credentials.** The superuser password (`service_instances`) and per-app passwords (`service_grants`) are plaintext in SQLite + the service `.env`, exactly the gap tracked for `MOOSE_SECRET_*` — folds into **App-secret injection hardening** above; the decision should cover both.

**Context:** `SERVICE_PROVISIONING.md` # Tier 1, the App-secret injection hardening entry above, the Backup architecture entry below.
**Why deferred, not dropped:** the Postgres slice was scoped to unblock kan; these extend the same spec without changing its shape. Pin them so the next managed-service touch picks the right one off the top.

### Managed-service per-app key isolation

The shared Valkey instance (the engine behind both `type: valkey` and the `type: redis` alias, `DECISIONS.md` 2026-06-13) gives each app its own ACL credential, not its own keyspace. The Valkey slice (#159) closed cross-app **destruction** — `+@all -@admin -flushall -flushdb -swapdb` means no app can wipe the shared keyspace — but cross-app **confidentiality** is still open: because every app holds `~*`, app A can read or overwrite app B's keys with ordinary `GET`/`SET`/`SCAN`. Command ACLs can't fix this (those are core commands), and per-app key-pattern ACLs (`~app:*`) only work if the app prefixes every key itself, which most don't and moose can't force. The clean form is the `isolated: true` manifest field already sketched in `SERVICE_PROVISIONING.md` # Per-app isolation — a dedicated instance per app for the apps that need a hard boundary, shared-by-default otherwise. The cost is runtime RAM (one Valkey process per opted-in app), not code, so the decision is about *when an app warrants the dedicated instance*, not how to build it. Same shape would extend to the SQL families if a regulatory/security app ever needs it. No catalog app requires this today; pin it so the next isolation-sensitive app picks it up.

**Context:** `SERVICE_PROVISIONING.md` # Per-app isolation in shared instances, `DECISIONS.md` 2026-06-13, `docs/progress/managed-services-redis.md`.
**Why deferred, not dropped:** single-tenant home server, all apps installed by the same owner — the credential boundary (no unauthenticated access, no cross-app *destruction*) is enough for v1; cross-app key *reads* matter only once a genuinely untrusted or regulated app lands.

### Backup architecture shape

Off-site backup is paid + post-MVP, but the *interfaces* — manifest hints (data vs. cache volumes), restore path, bind-mount-only constraint, managed-service dump path — should be sketched now to avoid retrofitting once we ship.

**Context:** `APP_MANIFEST.md` (`storage.data_volumes` / `cache_volumes`), `SERVICE_PROVISIONING.md` (managed-service backups), `APP_LIFECYCLE.md`.
**Prior art:** Yunohost ships per-app `backup`/`restore` scripts in each package — a useful reference for how the manifest declares "what's data vs. what's reconstructable" and where app-author logic lives in the dump/restore path.

### Display-name rename UX + audit log story

Slug is stable; the rename mechanic is straightforward but "who is `cindy` if she renamed to `cynthia`?" needs design across audit log, sharing, and any future identity-bearing UI.

**Context:** `FIRST_RUN.md` ("Identity & display names").

### App-facing background-job service (Tier-1)

A managed queue + worker that apps can fire background work into (overnight re-encoding, ML indexing, etc.). Apps declare `services.jobs: { type: moose-jobs }` in the manifest; brain provisions credentials + queue URL. Probable implementation: Redis Streams or NATS JetStream as the queue, a moose-managed worker pool runs jobs during a configured idle window.

**Context:** `SERVICE_PROVISIONING.md` (Tier-1 catalog — would extend it), `APP_MANIFEST.md` (`services:` block).
**Why Tier 3:** completely separate from brain↔host-agent jobs (which are OS-level). This is an app-platform feature; the bet is that "apps can offload async work to moose" is a real differentiator vs. Umbrel/CasaOS. Pin the shape now so we don't accidentally make decisions in `SERVICE_PROVISIONING.md` that close it off; full design post-MVP.

### Web terminal in the dashboard

`SPEC.md` # Local access promises a "virtual terminal in the web UI" — a shell without leaving the browser. Needs design across:
- Protocol: WebSocket over the brain↔host-agent UNIX socket via HTTP upgrade (already locked in `BRAIN_HOST_PROTOCOL.md` Pattern D as the future shape).
- Auth: root PTY = root on the host. Default to dropping to the dashboard-user's Linux account; explicit "open a root shell" gesture gated by re-typing the dashboard password.
- UX: where it lives in the dashboard, history persistence, multi-session behavior.

**Context:** `BRAIN_HOST_PROTOCOL.md`, `AUTH.md`, `SPEC.md`.
**Why Tier 3:** load-bearing affordance for tinkerers, but not on the v1 critical path. Pinning shape now keeps the protocol's WebSocket reservation honest.

### Hooks — concrete shape for return

Decided in principle: when hooks return, they're **one-shot container images**, not in-container scripts (`DECISIONS.md` 2026-05-13). The concrete schema, timeout/failure handling, log surfacing, and brain-side execution model aren't specced.

**Context:** `APP_MANIFEST.md` # F, `APP_LIFECYCLE.md` # "Deferred: lifecycle hooks".

### Cert-expired UX

When a box has been offline long enough that `.onmoose.io` certs expired: serve the expired cert with browser warning, transparently redirect to `.local`, or surface a banner in the dashboard. `DISCOVERY.md` makes the `.local` fallback well-defined (per-app records keep working without cloud reachability), so "redirect to `.local` + banner" is the leading option for desktop households — but it doesn't work for Android households, where `.local` URLs are unreachable. The open question is whether to special-case that audience (e.g., a static "your cert expired, plug in for an hour" page served on the LAN IP).

**Context:** `MOOSE_NETWORK.md` ("Failure modes"), `DISCOVERY.md`.

### Phased rollout / cohort + beta channel activation

Both deferred from v1 (`DECISIONS.md` 2026-05-15). Shape is pinned in `RELEASE_MANIFEST.md` # Future work + # Channels — schema is additive, hash formula is `hash(machine_id || canonical(brain, ui))`, beta is a sibling `beta.json` file. What's still open: the **trigger conditions** in concrete terms (what fleet-size threshold, what auto-apply milestone, what bad-release detection latency forces our hand). Pre-decide so we don't dither when one of them fires.

**Narrowed to appliance (2026-08-11).** Hosted no longer needs any of this: a per-box target version in the cloud control plane gives staged rollout, per-box pinning, and halt without a cohort hash or a second channel file (`UPDATES.md` # 8.1). This entry is now only about boxes we cannot address individually.

**Context:** `RELEASE_MANIFEST.md`, `UPDATES.md` # 3, # 8.1.

### Settings → Storage UX (Level-1 walk-through, design pass)

The architecture and the install/wizard/add-drive/eject mechanics are locked (`STORAGE.md`, `FIRST_RUN.md`, `AUTH.md`, `BRAIN_HOST_PROTOCOL.md`, `HEALTH.md` # `disk-full`). What remains is design-time copy + screen-layout: card shape for OS drive vs. data drive at Level 0/1, where the "Show recovery passphrase" affordance lives under Advanced, eject-drive confirmation copy, disk-pressure banner copy + top-space-hogs enumeration, single-drive "add a data drive later" dashboard hint, and the file-access permission block on the app-install dialog ("Photos will read and write your Photos folder").

**Context:** `STORAGE.md`, `FIRST_RUN.md`, `HEALTH.md`, `APP_MANIFEST.md` # `permissions.folders`.
**Why Tier 3:** doesn't block bring-up — the brain endpoints and health-issue flags exist. UX iteration belongs with the designer and the first user-test pass, not the spec.

### Reboot scheduling UX

"Reboot tonight at 3am OK?" prompt vs. silent within window. Surface only when blocked vs. always.

**Context:** `UPDATES.md`.

### Member-visible Tier-1 app logs (manifest opt-in)

`LOGGING.md` defaults Tier-1 shared app logs to admin-only — stdout commonly leaks per-user behavioral signal (request paths, search queries). For apps whose stdout is genuinely uninteresting (a periodic sync daemon, an indexer), a manifest field `logs.member_visible: true` would let authors opt the app's logs into the per-member Logs tab. Default off; off remains the safe choice for the bulk of the catalog.

**Context:** `LOGGING.md` # Per-app logs, `APP_MANIFEST.md`.
**Why Tier 3:** doesn't block v1; the day a member can't debug a shared-app issue without flagging down an admin, this lands.

### Audit-log schema details

`LOGGING.md` pins the `audit_events` table shape, write path, and v1 action vocabulary. Open: the typed metadata schema per action type (free-form JSON works for v1 but becomes a UI rendering pain at scale), and whether to add a hash-chain / sequence-number integrity guarantee on top of the append-only invariant. Neither blocks v1; both become tech debt if left unresolved as the catalog grows.

**Context:** `LOGGING.md`, `AUTH.md`, `APP_LIFECYCLE.md`.
**Why Tier 3:** the UI works against free-form JSON in v1, but every new event type adds rendering work. Pin the metadata schema before the catalog grows.

### Recovery dashboard spec (`RECOVERY.md`)

`moose-recovery.target` shrunk to two triggers in `DECISIONS.md` 2026-05-16: **TPM2 unseal failure on the root drive** (LUKS recovery passphrase at console — needs a printed-on-the-box / wizard-shown story), and **host-agent crashloop past `StartLimitBurst`** (static page on port 80 with one-click "roll back host-agent"). The page's actual content + UX, the rollback mechanism, and the mDNS discoverability story (`moose-recovery.local`) are not specced. Most failure modes that the old strict-gate model routed here now flow through degraded mode (`HEALTH.md`) — recovery is now an honestly small surface.

**Context:** `BOOT.md` # Failure → recovery target — the narrow cases, `HEALTH.md` (the rest of what used to live here), `STORAGE.md` # Encryption posture, `AUTH.md` (recovery code vs. LUKS recovery passphrase distinction).
**Why Tier 3:** doesn't block v1 happy-path development; bites the moment a user hits one of the two genuinely-unrecoverable-from-UI cases. Pin the shape before public release.

### Threat-model re-pass when the mesh ships

`THREAT_MODEL.md` is scoped to v1 closed-by-default and explicitly names the trigger for a re-pass: remote access via the mesh (`MOOSE_NETWORK.md` # Deferred) reshapes boundary **B1** (off-LAN reachability), introduces a new principal (a paired-but-non-household device — "grandma sees Photos"), and narrows "closed-by-default" to "closed except to identity-paired devices." When the mesh is picked up, the threat model gets a dedicated boundary pass + `DECISIONS.md` entries.

**Context:** `THREAT_MODEL.md` # When this model changes, `MOOSE_NETWORK.md` # Deferred: remote access via mesh.
**Why Tier 3:** rides the mesh work, which is itself deferred. Pinned here so the "living document" claim isn't hollow.

### fscrypt rollout plan

Per-user encryption is deferred but on the roadmap. Key-loading model (Model A vs. B in `STORAGE.md`), interaction with background app work (`APP_ISOLATION.md`), password-recovery escape hatch.

**Context:** `STORAGE.md`, `APP_ISOLATION.md`.

### Caddy liveness self-heal (gated on brain-owned Caddy container lifecycle)

The `service-down`(caddy) detector (`HEALTH.md` # Detector catalog, locus C) can't be a passive banner — Caddy fronts `moose.local`, so when it's down there's no dashboard to show the banner. The decided shape (`DECISIONS.md` 2026-05-31) is **bounded self-heal**: the brain restarts the Caddy container on failure, capped like host-agent's `StartLimitBurst` (≈5/60s), raising the issue only when the budget is exhausted. The blocker is that **the brain does not yet own Caddy's container lifecycle** — it manages Caddy's *routes* (`EnsureServer`/`EnsureCatchAll`, `internal/lifecycle`) but never starts/stops/restarts the Caddy *container*; in prod "brain-managed Caddy" is intent, not implementation (`dev/docker-compose.yml` runs it standalone). The real prerequisite is a brain-owned Caddy container lifecycle (start/stop/restart via the socket-proxy), which is partly host-integrated and needs the VM outer loop. Once that lands, the self-heal detector is a thin layer on top (probe = Docker container-state + admin-API reachability; restart-budget; raise-on-exhaustion; reuse `internal/caddy.Client`).

**Context:** `HEALTH.md` # Detector catalog (locus-C Caddy row), `CONTROL_PLANE.md` # Locked: Caddy runs as a container, `DECISIONS.md` 2026-05-31.
**Why Tier 3:** doesn't block v1 happy-path; a fully-down Caddy is already visibly broken (dashboard unreachable). Pin the self-heal shape now (done); build it after the brain owns Caddy's container lifecycle.

### Hosted profile — the commercial control plane and the deferred network model

`ENVIRONMENT.md` specs the OS-adaptation layer of the `hosted` profile (the lean image, the slim host-agent, provisioning, networking, storage, the export bundle). It deliberately defers everything *outside* the tenant VM. Pin the shape before the hosted product is real: (1) the **provisioning / control API** that creates, configures, resizes, suspends, and tears down tenant VMs; (2) **resource metering → billing** (wiring the brain's existing `/proc`+disk sampling to a billing pipeline, plus pricing tiers — the per-instance cgroup-limit *mechanism* is in `ENVIRONMENT.md`, the metering is not); (3) **fleet management** (centralized OS/brain updates across tenants, abuse handling); (4) the **deferred hosted network model** — the identity-based WireGuard mesh for hosted, a central shared ingress with "no per-VM public IP" routing, and per-app public-vs-login exposure controls (v1 is a plain public auth-gated endpoint). This is net-new infrastructure with no appliance analogue, and where the operational weight of being a data custodian lives.

**Context:** `ENVIRONMENT.md` (the OS-side it builds on), `CONTROL_PLANE.md` (the per-tenant brain it provisions), `MOOSE_NETWORK.md` # Deferred (the mesh design it would extend), `APP_LIFECYCLE.md` (where per-instance limits attach).
**Why Tier 3:** the OS runs in a hosted VM without any of it (v1 is a public endpoint per app); none of it blocks bringing the hosted OS up. It blocks *operating a paid fleet*, so pin the shape before commercial launch.

### Hosted profile — open OS-level questions

Surfaced by `ENVIRONMENT.md`, smaller than the commercial layer but real: (1) **at-rest key custody** — provider volume encryption vs. LUKS keyed from a hosted KMS (vTPM exists on some hypervisors but is pointless under a custodian model); (2) the **headless recovery surface** that replaces the appliance's console-served `moose-recovery.target` page; (3) whether the **logical export/restore bundle** stays in `ENVIRONMENT.md` or graduates to its own spec, plus its concrete format; (4) the final **profile names** (`appliance`/`hosted` vs alternatives).

**Context:** `ENVIRONMENT.md` # Open questions, `STORAGE.md` (key custody), `BOOT.md` (recovery target).
**Why Tier 3:** each picks a concrete mechanism for a section `ENVIRONMENT.md` currently leaves open; none blocks the design, but they block implementation of that part of the hosted profile.

---

### Encrypt hosted enrollment credentials at rest (box-side)

The brain reads per-box acme-dns credentials from the seed and stores them as plain text in `box_meta` (`store.BoxMetaEnrollment`). That matches what the cloud side does today. The risk is someone getting the brain's SQLite file: a leaked subdomain, username and password lets an attacker renew certs for that one box. It does not get them anything more. Encrypt these at rest before the box's database lives on shared or backed-up infrastructure. The cloud side tracks the matching item for its own `boxes` table (`onmoose/cloud` NEXT.md).

**Context:** `ENVIRONMENT.md` # Provisioning & first-boot, `THREAT_MODEL.md`.
**Why Tier 3:** the damage is limited to one box's certs, and the fix is a box-side change on its own. The open question is where the box keeps its own key, since it has no secret store yet. Settle that before the database moves anywhere shared.

### Per-app disk quota for hosted tenants

`ENVIRONMENT.md` # Per-instance resource limits names a per-app **disk quota** as the third dimension the hosted control plane needs to bound a paying tenant. The memory + CPU cgroup limits landed with #211, but disk quota was deferred: the locked storage stack (`ext4` + Docker's `overlay2`, `STORAGE.md`) cannot enforce a per-container write-layer quota portably. Docker's `--storage-opt size=` works only on `xfs`-with-`pquota` or the `devicemapper`/`btrfs` drivers — none of which the appliance or the cloud image use.

The likely path is **XFS project quotas** on the data tree, driven through host-agent. That is a privileged op: set or clear a project ID and a hard limit on an app's `/var/lib/moose/state/instances/<id>/` subtree and the use-case folders it binds. It needs three things: a verb in `BRAIN_HOST_PROTOCOL.md`, an implementation in `host-agent-real`, and the same store-backed policy and reconcile seam the cgroup limits already use (`internal/store` `instance_resource_limits` would gain a `disk_bytes` column). In practice this is hosted-only.

**Context:** `ENVIRONMENT.md` # Per-instance resource limits, `STORAGE.md`, `BRAIN_HOST_PROTOCOL.md`. Deferred from #211; tracked in #221.
**Why Tier 3:** it is out of scope until the cloud image's storage layout is settled, and that choice decides whether XFS project quotas are available at all. Settle the shape together with the layout, not after it.

## Tier 4 — Smaller open items

Loose ends. Each is parked until it bites or a higher-tier topic pulls it in.

**Manifests & catalog**
- Exact `MOOSE_SERVICE_*` variable schema per service type — `APP_MANIFEST.md`, `SERVICE_PROVISIONING.md`.
- `permissions.devices` syntax — paths vs. categories (`webcam`, etc.). `APP_MANIFEST.md`. *(GPU split out into its own `gpu: true` field — `DECISIONS.md` 2026-05-30; this item now covers only non-GPU device shorthand.)*
- **Store `permissions.capabilities` escape hatch (deferred).** A reviewed-at-submission list (`NET_ADMIN`, `SYS_TIME`) for the rare store app that legitimately needs one capability. Cut from the v1 schema (`DECISIONS.md` 2026-05-30 — store apps get `cap_drop: [ALL]`, no `cap_add`); capability needs go through Door-2 / Tier 2 today. Revisit if a curated app genuinely can't fit either path. `APP_MANIFEST.md`, `APP_ISOLATION.md`.
- **User-namespace remap for hardcoded-internal-UID app images (deferred).** `service_user` (`APP_ISOLATION.md` # Runtime identity & data ownership, `DECISIONS.md` 2026-06-10) gives folderless apps a dedicated allocated non-root identity, but only covers images that *adopt* moose's runtime `user:`. It does not cover images that hardcode a *different* non-root internal UID — php-fpm pools pinned to `www-data`, entrypoints that `setuid`-drop to a fixed service user (also needing the stripped `CAP_SETUID`/`SETGID`). Supporting that class safely needs per-app user-namespace remapping (an in-container UID maps to a meaningless host subuid, so naming it can't alias a host principal), which `APP_ISOLATION.md` # Not in v1 defers until the catalog is mature. **Trigger to pick up:** a 3rd/4th app of this class in the import ledger — poznote, postiz, and formbricks (#182/#193) now make three (kimai a secondary finding), so the count-trigger is met; the cost stays gated on the feasibility spike below, so it remains deferred. **Per-service UID is not a separate escape:** #193 framed formbricks as needing distinct per-service identities (a bundled `pgvector` Postgres wanting non-root *and* the main app wanting its baked uid 1001), but a bundled Postgres adopts any non-root UID fine under `service_user`, so only the main app's hardcoded-1001 writes block — this same class. The services that would need *distinct* moose-managed UIDs are themselves hardcoded-internal-UID images, so per-service-UID assignment wouldn't unblock any real app and folds into this deferral rather than standing alone. **This is not implementation-ready and must not be filed as an implementation issue until specced** — two questions gate the whole approach and the first is a feasibility spike, not a design choice:
  1. **Feasibility spike first — can our runtime even do per-app userns?** Classic Docker's `userns-remap` is **daemon-global** (one mapping for *every* container — brain, managed Postgres, Caddy, all apps), not per-app; the per-app *distinct* mapping this needs is a rootless-Podman strength, not something `docker compose` gives cleanly. Answer this (rootless Docker? `--userns`? a different runtime? daemon-global only?) before any design — it can reshape or kill the approach.
  2. **Collision with the folder-identity model.** Folder apps run as the **owner's real host UID (≥ 3000)** precisely for native `/home/<user>/` access (`APP_ISOLATION.md` # Runtime identity & data ownership). A remap shifts in-container UIDs into a subordinate range, so a remapped container can no longer write the owner's home as the owner; every bind-chown gains remap-offset math. Reconciling "remap for isolation" with "run as the real owner UID for folder access" is the hard, unsolved part.
  When picked up, revisit the apps parked in `docs/dev/catalog-import-gaps.md` # nonroot-data-ownership (poznote #90, postiz #128, formbricks #182/#193, kimai #89 secondary finding). Carries the related open **product** question — whether cross-app data reads are ever sanctioned (today: only shared use-case folders + managed services). `APP_ISOLATION.md`, `THREAT_MODEL.md`.
- **Door-2 asymmetric admission relaxation (deferred).** v1 holds the line — admission is door-symmetric, Door-2 carries permissive *defaults* but the identical *sandbox* (`DECISIONS.md` 2026-06-02). If real demand appears, revisit relaxing the *app-bounded* primitives for Door-2 (host ports, arbitrary bind mounts minus the socket/host-root paths) while keeping the host-rooting ones (`privileged`, socket, host namespaces, near-root caps) refused — pairs with the deferred reviewed `permissions.capabilities` allowlist. `APP_ISOLATION.md` # Trust tiers, `APP_LIFECYCLE.md` # admission policy.
- Manifest signing / provenance for third-party stores. `APP_MANIFEST.md`.
- App icon & screenshot handling — bundled vs. URL. `APP_MANIFEST.md`.
- **On-disk footprint before install.** *(Resolved 2026-06-03 — widened from "image download size" to the full app footprint and locked. Footprint = container image(s) + app-state estimate; user content is excluded; image sizes are CI-derived (not the rejected `storage.image_size` manifest field); the store card shows a coarse catalog upper-bound and the install dialog a box-specific incremental number with a free-space warning. See `APP_STORE.md` # Catalog schema, `BRAIN_UI_PROTOCOL.md` # GET /api/v1/catalog/:id/install-plan, `DASHBOARD.md` # Install authorization, `DECISIONS.md` 2026-06-03. Implementation tracked on the issue board.)* Residual design-time copy/UX: whether the small store card shows **disk size or download size** as its single figure. `DASHBOARD.md`, `WEB_UI.md`. *(The "how to present an estimate that grows over time" residual is resolved 2026-06-09: `estimated_size` is now the measured app-state baseline at install, not a usage projection, so the manifest figure never represents growth — runtime disk-pressure (`HEALTH.md` # `disk-full`) covers "app filled the disk later." See `DECISIONS.md` 2026-06-09, `APP_MANIFEST.md` # Storage.)*
- Update-strategy declarations (in-place vs. needs-migration). `APP_MANIFEST.md` (folds into hooks).
- Typed install-time questions in the manifest (prior art: Yunohost's pre-install question schema — typed prompts for admin/domain/language captured at install time). Door 2 now authors the `permissions` block (internet/lan/gpu/folders) in the form plus a raw-YAML escape hatch (`DECISIONS.md` 2026-06-02), but there's still no *store-app-declared* typed-question schema for arbitrary install-time config beyond env-var passthrough; revisit when a curated app needs it. `APP_MANIFEST.md`.
- **Door-2 synthetic-manifest *graduate-in-place* path (deferred).** Editing an **already-installed** custom app's manifest in place to *graduate* it (refine volumes, add a managed DB / backup hooks, classify cache vs. data) without reinstalling. *Install-time* authoring is built into the form (permission controls + Edit-as-YAML toggle, `DECISIONS.md` 2026-06-02); the deferred remainder is the **post-install** editor specifically — its larger surface (re-render, restart, reconcile a live instance, audit) is why it waits. v1 change-path stays uninstall + re-paste. Pairs with the re-import path for archived "keep data" instances (Control-plane Tier item). `APP_MANIFEST.md` # one model, two doors, `DASHBOARD.md` # Edit-after-install is deferred.
- App categories / tags taxonomy for the store browse UX. `APP_STORE.md`, `WEB_UI.md`. *(The Store browse grid already filters by category — pills built from the raw union of the catalog's own `categories` strings, `progress/store-browse-filters.md`. Still open is the curated **taxonomy** behind those strings: a canonical set, ordering, display names, localization.)*
- Per-app cron / scheduled tasks declared in manifest (distinct from the Tier-3 background-jobs service in `SERVICE_PROVISIONING.md`). Cron-on-host vs. a per-instance scheduler container. `APP_MANIFEST.md`.
- Per-app kill switch in `catalog.json` (distinct from `RELEASE_MANIFEST.md`'s `rollback_to`, which targets brain/UI versions). For "CVE dropped in app X, stop it everywhere on next catalog refresh." `APP_STORE.md`, `APP_LIFECYCLE.md`.
- Catalog removal / delisted-app behavior — installed instances when an app is pulled (keep-running-with-warning vs. force-uninstall vs. read-only). `APP_STORE.md`, `APP_LIFECYCLE.md`.
- App dependency model — pin "no" (managed services are the answer) so authors don't build the assumption. `APP_MANIFEST.md`.
- *(Resolved 2026-05-29 — user-driven multi-instance is **yes**: duplicate installs warn but don't block (`DASHBOARD.md` # warn, don't block). Each becomes a personal instance with its own owner/slug/data. The remaining sub-question — two instances owned by the *same* user — folds into the same machinery; pin it if it ever bites.)*
- Same-user repeat-install slug cap + error UX. `allocateSlug` (`internal/lifecycle`) tries `<slug>` (bare, first-come), `<slug>--<user>` (personal collision), `<slug>-2`, `<slug>-3`, then fails *inside the install job* with an opaque "no free slug" error. Two open bits: (a) the cap of effectively three slugs before exhaustion is arbitrary and tight for power users who install the same app multiple times; (b) exhaustion should surface as a clear pre-job `422`/`409`, not a mid-job failure. `APP_LIFECYCLE.md`, `DASHBOARD.md`.
- App publisher identity / verified-author badge surface (the *mechanism* folds into manifest signing above; this is the catalog-side UX). `APP_STORE.md`.
- Container vulnerability scanning at catalog publish (Trivy/Grype in CI on every PR). `APP_STORE.md`.

**Networking & cloud**
- **SSH scoping on a dual-stack LAN.** `BUILD.md` # SSH scopes `:22` by source range, and the ranges it names are RFC1918. IPv6 has no RFC1918. As built (#467) the rule allows link-local `fe80::/10` and unique-local `fc00::/7` and drops everything else, which is the honest translation — those two cannot be routed in from outside — but it does not cover the common home network whose LAN peers carry only ISP-delegated **global** addresses. Those are reachable from the public internet, so allowing them would undo the property the rule exists for: a scan from outside sees a closed port. Link-local always exists on a LAN interface, so the path is not shut today; what needs deciding is whether "the LAN" gets a better v6 definition (the box's own delegated prefix, learned at runtime? interface-scoped accepts after all?) or whether v6 SSH stays link-local-and-ULA-only and the mesh is the answer for everything else. `BUILD.md` # SSH, `DISCOVERY.md` (mDNS hands clients AAAA records, which is how a client ends up on v6 in the first place).
- `box-id` allocation scheme — word-pair vs. random hex + check digit. `MOOSE_NETWORK.md`.
- DNS provider for the apex — Cloudflare free tier vs. self-hosted PowerDNS. `MOOSE_NETWORK.md`.
- ACME DNS-01 plugin path — Caddy generic vs. moose-specific plugin. `MOOSE_NETWORK.md`.
- Privacy doc surface — what we log (DNS queries, enrollment metadata) and retention. `MOOSE_NETWORK.md`.
- **Multicast / discovery diagnostic-bundle probe — exact shape.** `LOGGING.md` and `DISCOVERY.md` commit to including a multicast self-test in the diagnostic bundle; the precise measurements (which queries we send, on which interfaces, how we present "responses: 0" to a support tech) need a pass. `LOGGING.md`, `DISCOVERY.md`.
- **Windows Bonjour detection in first-run.** `FIRST_RUN.md` points Windows users at the Bonjour installer; the trigger today is User-Agent, which is unreliable. Consider a JS-side mDNS probe (does the browser actually resolve `moose.local` *from this client*?) so the prompt only fires on clients that need it. `FIRST_RUN.md`, `DISCOVERY.md`.
- Custom domain on the LAN — user owns `home.example.com` and wants the dashboard there. Caddy + ACME DNS-01 with their provider, or accept-cert-warning. `MOOSE_NETWORK.md`.
- Local DNS resolver shape — host runs dnsmasq (container resolution + free Pi-hole-shape ad-blocking as a side effect) vs. pure systemd-resolved. `APP_ISOLATION.md`, `MOOSE_NETWORK.md`.
- UPnP / port-forwarding stance — closed-by-default implies "no"; state it explicitly so a future "convenience" PR doesn't sleepwalk into it. `MOOSE_NETWORK.md`, `SPEC.md`.
- `status.onmoose.io` outage-comms surface — boxes show a banner from a cached status JSON when cloud is down. `MOOSE_NETWORK.md`.
- Anti-clone check at enrollment — two boxes with the same `box-id` (cloned ISO) must not both enroll. `MOOSE_NETWORK.md`.
- **Live-installer WiFi step.** A WiFi-only laptop has no ethernet, so the live ISO itself needs an SSID-picker before "Install to disk" (or be fully offline-installable). Also: WiFi credentials entered in the installer must survive into the installed system's NetworkManager config, not just the live environment. Driver coverage (Realtek/Broadcom non-free firmware) is the connected build-side concern. `BUILD.md`, `FIRST_RUN.md` # Step 1.
- **Dashboard Settings → Network panel UX.** The plumbing (NM-backed endpoints) is in `BRAIN_HOST_PROTOCOL.md`; the UX details (saved-networks list, signal/security indicators, switch-network "you may briefly lose this page" confirmation, static-IP form, multi-NIC priority controls) belong to `WEB_UI.md`. `BRAIN_HOST_PROTOCOL.md` # Network endpoints, `WEB_UI.md`.

**Isolation & runtime**
- **GPU + device capacity enforcement.** `permissions.gpu` and `permissions.devices` are parsed and (for devices) passed through, but the spec's "refuse at capacity check if the GPU/device is absent" (`APP_ISOLATION.md` # GPU, # Devices) is not honored — the brain has no host hardware-capability query, so an absent GPU/device currently fails at `docker compose up` instead of giving the specced capacity error, and `gpu: true` emits no runtime stanza at all. Needs a host capability endpoint (sibling of `/v1/identity/well-known`) the install transaction checks before generating the override. Deferred from the folder-enforcement slice (`docs/progress/install-permissions-enforcement.md`). `APP_ISOLATION.md`, `BRAIN_HOST_PROTOCOL.md`.
- **Ingress topology vs. "no inter-app traffic."** `APP_ISOLATION.md` # Inter-app traffic and `THREAT_MODEL.md` # B2 commit to **per-app bridges with no inter-app traffic** — the reverse proxy reaches each app over its own network. The implementation instead joins every app's `main_service` to a **shared `moose-ingress`** network alongside Caddy (`internal/lifecycle`), which lets app main-services reach each other and thus diverges from the stated mitigation. The design (per-app bridge) is already locked; the open sub-question is the *mechanism* to preserve it — Caddy joins each per-app network (matches the spec literally), or the shared ingress stays but inter-app traffic is cut another way (ICC off / nftables on the bridge). Pin which, then converge the impl. The per-app HTTP health-probe (`DECISIONS.md` 2026-06-02) deliberately routes its probe through Caddy partly so it doesn't depend on this resolving. `APP_ISOLATION.md` # Inter-app traffic, `THREAT_MODEL.md` # B2, `CONTROL_PLANE.md`.
- GPU sharing across apps (MIG / time-slice / exclusive). `APP_ISOLATION.md`.
- macvlan on bonded / bridged host interfaces. `APP_ISOLATION.md`.
- Read-only root rollout as a catalog requirement. `APP_ISOLATION.md`.
- Egress allowlist for `internet: true`. `APP_ISOLATION.md`.
- Per-app firewall rules (apps as L4 endpoints). `APP_ISOLATION.md`.
- Author-declared default/hint for folder source (e.g. an `allow_shared`-style flag) so a manifest can bias the install-time personal-vs-shared toggle without removing the installer's choice. Resolved-for-now as fully installer-elected (`DECISIONS.md` 2026-05-30); revisit if catalog demand appears. `APP_MANIFEST.md` # `folders`.
- fscrypt coverage for per-user app state under `/var/lib/moose/state/instances/<id>/`. When per-home fscrypt lands, does it extend to managed-service data (per-user Postgres, etc.)? `APP_ISOLATION.md` # Managed services placement, `STORAGE.md` # Future: per-user encryption.

**Storage & first-run**
- UTF-8 filename normalization (NFC vs. NFD) across SMB clients — macOS uses NFD on the wire, Linux stores bytes verbatim; "files-first-class" makes this user-visible. `STORAGE.md`.
- Data-import flows — **browser upload is now the v1 in-product path (`FILES.md`)**; what remains is *bulk* import from a USB stick / network share plugged into the box ("copy everything off this drive into ~/Photos"), which rides removable-drive auto-mount (below). `STORAGE.md`, `FIRST_RUN.md`, `FILES.md`.
- Boot-PIN high-security mode. `STORAGE.md`.
- Stronger TPM2 sealing policy (PCR 7+11 with signed PCR policy, or PCR 7+14). v1 seals against PCR 7 only — works across kernel updates, weaker against an attacker who can subvert Secure Boot. Upgrade is non-destructive (additional LUKS slot, re-enroll). `STORAGE.md`, `BOOT.md`.
- Recovery-passphrase storage assistance ("email this to me", USB shard). `STORAGE.md`, `FIRST_RUN.md`.
- Removable drives auto-mount UX. `STORAGE.md`.
- Filesystem on extra drives (ext4 vs. accept existing NTFS/exFAT). `STORAGE.md`.
- OS-drive-only swap with data drive intact. `STORAGE.md`.
- TPM-less hardware fallback. `FIRST_RUN.md`.
- First-run on a box with pre-existing moose data. `FIRST_RUN.md`.

**Users & groups**
- TPM-fail-and-admin-forgot-password rescue path. With `PermitRootLogin no` and no console root password, the LUKS recovery passphrase boots the box but leaves no clear next step. `USERS_AND_GROUPS.md` # Known gaps, `STORAGE.md`.
- Demotion doesn't kill live `sudo` capability — existing SSH sessions retain group membership until logout. Acceptable for the household trust model; revisit if threat model changes. `USERS_AND_GROUPS.md` # Known gaps.
- `moose-shared` membership management UI — how an admin removes a user from household-shared content without deleting the account. Folds into shared-folder management UX (Tier 2). `USERS_AND_GROUPS.md`, `STORAGE.md`.
- Account deletion flow — what happens to `/home/<user>/` and per-user Tier-3 instances when an account is removed. (Audit-row handling is settled: FK `SET NULL` on `audit_events.actor_user_id` keeps history with a null actor.) `USERS_AND_GROUPS.md`, `AUTH.md`.
- Account suspension — disable login without deleting data (kid grounded, ex-roommate archived). `AUTH.md`, `USERS_AND_GROUPS.md`.
- Multi-admin invitation flow — UI affordance for "make a second admin." Today implicit (admin creates a member then promotes them). `AUTH.md`, `USERS_AND_GROUPS.md`.
- **Recovery-code hash algorithm — bcrypt (shipped) vs. argon2id (original spec intent).** The brain hashes the one-time recovery code with **bcrypt** (`newRecoveryCode`), but `AUTH.md` originally specced **argon2id**; the spec was made hash-agnostic in PR #80 (account-ui) rather than blessing either. Decide which we actually want: bcrypt is fine for a 24-hex-char code (well under bcrypt's 72-byte input limit) and is already a dependency, but argon2id is the stronger modern default and matches the `pam_argon2` posture for the OS password. If we standardize on argon2id, it's a backend change (re-hash on next successful recovery, or a one-shot migration) — low urgency since the code is single-use and high-entropy. Pin the choice and write it back into `AUTH.md`. `AUTH.md` # The recovery code.

**Runtime & host**
- Periodic image / layer cleanup policy — a recurring `docker image prune -a` sweep (cadence + retention) for images orphaned by *updates*, not uninstalls. Post-uninstall reclaim is handled by targeted `rmi`-by-digest (issue #9); this remaining item needs a scheduler/timer seam the brain doesn't have yet, plus a retention rule for update-orphaned layers. `APP_LIFECYCLE.md`, `UPDATES.md`.
- Container runtime version pinning — which Docker engine version we ship, how it tracks Debian-base updates vs. upstream `docker-ce`. `BUILD.md`, `UPDATES.md`.
- Host kernel panic / coredump capture policy — what we keep, where, retention. Brain & host-agent process panics are covered by `TELEMETRY.md` (structured crash events when opt-in is on). Kernel panics are the remaining gap. `LOGGING.md`, `HEALTH.md`.
- Log rotation for non-journald files (Caddy access logs, anything that escapes the journal). `LOGGING.md`.

**Observability (user-facing)**
- Per-category mute vs. criticals — mute is implemented as a full read-time category filter (`NOTIFICATIONS.md` # Configuration), so muting `storage` also hides a `data-drive-readonly` critical. Spec-faithful and the user's explicit choice (defaults are everything-on), but a future "critical always rings through, mute only quiets info/warning" carve-out is plausible if support traffic shows muted criticals get missed. Deferred, not decided. `NOTIFICATIONS.md` # Configuration, # Severity.
- Role-filter the mute settings list — the Settings → Notifications toggle list (`NOTIFICATIONS.md` # Configuration, Surface) shows all of `notify.Categories` to every user, but per # Routing a member never *receives* the admin-only categories (`storage`, `system`, admin `updates`), so a member muting "Storage" writes a dead row. Faithful to the backend (which validates all categories regardless of role) and harmless, but the list could be trimmed to the categories a user can actually receive. Open question is whether per-role filtering is worth the role→category map it requires. Deferred, not decided. `NOTIFICATIONS.md` # Configuration, # Routing.
- Settings → System deep-view (admin route) — full 60-second graphs of CPU/RAM/net/disk with all interfaces and drives broken out, over the same `GET /api/v1/system/live` stream that powers the all-users top-bar dropdown. The dropdown ships first; this is the deeper admin surface. `LOCAL_ANALYTICS.md` # UI surfaces, `WEB_UI.md`.
- Per-container live monitor ("Activity Monitor" view) — sortable table of all containers with live CPU/RAM/net/disk. Host-level live view is specced (top-bar dropdown); per-container live is the deferred surface. Mechanism same as system-resources SSE. `LOCAL_ANALYTICS.md`, `WEB_UI.md`.
- App-level network bandwidth accounting (per-container veth stats). Useful for "which app is hammering my ISP" but expensive. `LOCAL_ANALYTICS.md`.
- Storage growth attribution — what *kind* of data grew ("Photos +50 GB this month, mostly RAW files"). Compound on top of the per-app storage trend already specced. `LOCAL_ANALYTICS.md`, `STORAGE.md`.
- "What's eating my disk" explorer — top-N folders/apps under `/srv/moose`. Folds into Settings → Storage UX (Tier 3). `STORAGE.md`, `WEB_UI.md`.

**Time**
- Captive-network NTP fallback — reconsider `time.onmoose.io` if user reports surface (networks that block external NTP). `TIME.md`.
- Per-user display TZ — browser-side `Intl.DateTimeFormat` covers the traveler case in v1; revisit if box-time-regardless requests appear. `TIME.md`.
- `last-known-time` rollback prevention — persist last-shutdown wall-clock so first-boot-no-network doesn't render 1970 in logs. Polish. `TIME.md`.

**Telemetry (project-side)**
- GeoIP source for country bucketing on the telemetry edge — which DB, refresh cadence, unresolved-IP placeholder. `TELEMETRY.md`.
- Crash stream split — v1 uses PostHog Cloud for both usage + crashes; split to self-hosted Sentry later if crash volume justifies it. `TELEMETRY.md`.
- `events` vs `audit_events` table unification in brain SQLite — single table with `category` discriminator, or two tables. Implementation choice; spec treats them as one logical store. `LOCAL_ANALYTICS.md`, `LOGGING.md`.

**Backup & migration**
- On-box / local backup to external USB drive — pre-cloud, v1-shaped: snapshot scheduling, retention, restore UI. Distinct from the off-site architecture entry above. `STORAGE.md`, `APP_LIFECYCLE.md`.
- Cross-box migration — "I bought a new laptop, move my stuff." Same plumbing as restore-from-backup but a different flow (pair source + destination, switch identity). `STORAGE.md`, `MOOSE_NETWORK.md`.
- Backup verification / restore-test cadence — untested backups aren't backups. `STORAGE.md`.

**Developer / app-author surface**
- Local dev/test subcommands beyond lint/check/resolve (`moose install --local`, etc.) — let authors run a manifest on their own box before a catalog PR. `moose manifest lint` (schema + sibling-compose validation, issue #7), `moose manifest check` (lint + the compose admission policy in one pass, so authors never hand-eyeball `admission.go`), and `moose manifest resolve` (registry digest + download/disk size resolution into the object-form `images` map, issue #69) shipped and own the `cmd/moose` skeleton; this remaining item is the heavier "actually install it locally" surface. `APP_MANIFEST.md`, `APP_STORE.md`.
- `moose catalog scaffold --compose <path>` — deterministic Phase-2 rewrite of an upstream compose into a Door-1 skeleton (drop `ports:`, named volume → `./data/` bind, flag — never silently strip — forbidden directives, emit a `manifest.yml`/`compose.yml` draft with TODOs). Cuts the mechanical keystrokes out of the agent-authoring loop (`docs/dev/authoring-apps-with-an-agent.md`). Deliberately takes a compose the author already located, NOT a repo URL: gathering a compose (it may live in a linked repo, under `docs/`, or only as a `docker run` line) and the ADAPT-DON'T-FORCE bail decision both stay model-side — a tool that crawls or auto-strips gets them wrong. **Trigger: revisit after ~10 hand-authored apps, and build only against the rewrite patterns actually observed across them** — the catalog is two apps deep today, so the common transforms aren't known yet (no premature abstraction, CLAUDE.md). `APP_MANIFEST.md`, `APP_LIFECYCLE.md`.
- Catalog PR template + author-facing docs surface (subset of the Tier-3 "Documentation surface" entry). `APP_STORE.md`.
- **`moose manifest resolve` — size `disk_bytes` without a Docker daemon.** #120 fixed the containerd-store bug by streaming `docker save` and unpacking layer blobs locally. It works, and it works on any store, but it is slow: a multi-GB image like open-webui takes about a minute just to count bytes. The cleaner path is to read the image manifest straight from the registry — no local pull, no `docker save`. Walk the layer descriptors, fetch each compressed blob, unpack it as it arrives, and add up the bytes. Same number, and no Docker daemon needed. Catalog CI wants that too, since no daemon runs there. The registry-client work was scoped in [`../progress/catalog-image-footprint.md`](../progress/catalog-image-footprint.md). `APP_STORE.md` # Catalog schema.
- Manifest changelog discipline — when schema v1 → v2 ships, how authors find out. Revisit once we have a `v2` candidate. `APP_MANIFEST.md`.

**Web UI**
- Browser support matrix — minimum Chromium/Firefox/Safari versions, mobile-browser commitment level (works / works well / PWA-grade). `WEB_UI.md`.
- Accessibility target — WCAG AA, keyboard navigation, screen-reader pass, reduced-motion. State the position so it's in the build pipeline from day one, not bolted on. `WEB_UI.md`.

**Updates**
- Critical-security flag override for auto-update-off apps. `UPDATES.md`.
- Metered-connection mode for app updates. `UPDATES.md`.
- Concurrent emergency updates across streams. `UPDATES.md`.
- Per-region / per-cohort rollouts. `UPDATES.md`.
- Concrete "stable" promotion criteria. `UPDATES.md`.
- CI signature-verification check on every `releases` PR (covered in `RELEASE_MANIFEST.md` # Promotion; tracked here so the implementation isn't forgotten when CI is stood up). `RELEASE_MANIFEST.md`.
- Signing-key custody + rotation runbook (deferred per `RELEASE_MANIFEST.md` # Signing — "until we have a release to sign"). `RELEASE_MANIFEST.md`, `BUILD.md`.

**Services**
- Per-app DB resource quotas. `SERVICE_PROVISIONING.md`.
- Backup frequency / retention defaults for managed-service dumps. `SERVICE_PROVISIONING.md`.
- Restore-to-different-version semantics for managed services. `SERVICE_PROVISIONING.md`.
- Whether DLNA stays in v1 or gets cut. `SERVICE_PROVISIONING.md`.

**Control plane**
- *(Resolved 2026-05-29/31 — instance naming is first-come bare `<slug>` for any scope; `<slug>--<user>` on personal collision, `<slug>-2` on household collision. Flat single-label forced by wildcard-cert + mDNS constraints. See `DASHBOARD.md` # instance naming and `DECISIONS.md` 2026-05-29 + 2026-05-31.)*
- Re-import path for archived ("keep data") instances after uninstall. `APP_LIFECYCLE.md`.
- Per-session concurrent file-transfer cap for the streaming `GET`/`PUT /api/v1/files/content` endpoints. These are streaming (not jobs, not SSE), so neither the per-session request-rate bucket nor the SSE-stream concurrency cap governs them (`BRAIN_UI_PROTOCOL.md` # Rate limiting & abuse — deliberately left out of the v1 posture). A small concurrency counter (same shape as the ≤16 SSE cap) is the obvious backstop for a buggy uploader; pin it when file-transfer abuse actually bites. `BRAIN_UI_PROTOCOL.md`, `FILES.md`.

**Build & distribution**
- Signing infrastructure for apt repo, registry images, ISO. `BUILD.md`.
- ISO size budget. `BUILD.md`.
- Installer shares code with `moose-brain` vs. clean-sheet. `BUILD.md`.
- Kiosk-installer failure-mode UX ("stuck at 73%"). `BUILD.md`.
- Hardware-compatibility list process. `BUILD.md`.

**Testing**
- Boot-test assertion harness language — Go (matches the codebase) vs. Python (richer QEMU/swtpm tooling). Either works; pick before the harness is built. `TESTING.md`.
- *(Resolved 2026-06-16 — `mkosi` chosen as the single image builder for the install ISO + cloud VM image + test lane; `live-build` rejected. The `mkosi qemu` test-story weight noted here was a material factor. See `DECISIONS.md` 2026-06-16 and `BUILD.md` # 2.)*
- **Web-UI (Vue) unit/component test harness — none exists yet.** `web-ui/` ships build tooling only (`vue-tsc --noEmit && vite build`), so all dashboard behavior is verified by type-check + manual run, never an automated assertion. This first bit when the account-ui slice (#13) shipped a 401-handling bug (`changeMyPassword` bouncing the user to login on a wrong current password) that lived entirely in an untested path; the fix added `api.ts`'s `suppressAuthHandler` opt-out with no regression test to pin it. Open decisions before standing one up: runner + environment (vitest + jsdom is the vite-native default) and — load-bearing in this repo — the **dependency-closure / supply-chain question** (the repo deliberately pins transitive deps to pre-May-2026 releases; a new test-runner closure must clear that bar). Once locked, backfill auth-flow regressions first (the `suppressAuthHandler` 401 path; recovery/redemption error surfaces). `WEB_UI.md`, `TESTING.md`.

**Health**
- *(Resolved 2026-05-29 — v1 health-check enumeration is now specced as the `HEALTH.md` # Detector catalog; see `DECISIONS.md` 2026-05-29. Disk SMART, journal-disk-pressure, container-restart-loop, service-down, ram-pressure, reboot-required, auto-unlock-degraded landed in the taxonomy; per-app HTTP health-probe remains deferred — see the manifest item below. Implementation follows as brain `internal/health` registry additions + host-agent locus-B reporters + the generalized `/v1/health/system` report.)*

**Top-level**
- Redundancy implementation when Level 2 storage ships (btrfs vs. ZFS vs. mdadm). `SPEC.md`.
- ARM64 timeline. `SPEC.md`, `BUILD.md`.
- License for the OS itself. `SPEC.md`.
