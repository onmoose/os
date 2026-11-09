# moose Update Model

> Working spec for how a running moose box stays current. Companion to `SPEC.md`, `CONTROL_PLANE.md`, `BUILD.md`, `SERVICE_PROVISIONING.md`, `APP_MANIFEST.md`.

This doc is **draft / option-survey**. Most sections present alternatives with a recommendation; locked decisions are pulled out at the bottom. The intent is to surface forks before committing.

## What this doc covers

A box has **two update streams**. The split is not by component, it is by **what the thing runs on**:

- **Stream A — the box.** Debian base, kernel, firmware, and `host-agent`. This is the machine itself. It is slow, it needs a reboot, and it is **one atomic unit**: you do not get to have a new kernel with an old `host-agent`. That unit is an **A/B OS image** (# 1, # 2; `DECISIONS.md` 2026-10-01), built on hosted (#561 to #563) and not yet on the appliance (#564). Its version is the moose release.
- **Stream B — the containers.** `moose-brain`, `moose-ui`, apps, and managed services. These are images. They are frequent, they need no reboot, and each one already carries its own rollback story, because "keep the old image" is what a container registry is for.

Within stream B the components still have their own policies — the control plane is admin-triggered or cloud-pushed, apps auto-apply unless permissions expand, managed services are invisible infrastructure. Those policies are in # 3, # 4, and # 5 below and are unchanged by the two-stream framing. What the framing fixes is the **unit of testing and the unit of rollback**: two streams means we ship and verify two combinations, not the cross-product of five.

This doc spells out the policy for each component, plus the cross-cutting concerns: scheduling, rollback, dependency ordering, failure handling. Sections # 1 to # 7 describe the **appliance** profile — a box moose does not operate. The **hosted** profile (`ENVIRONMENT.md`) keeps both streams and all the apply/rollback mechanics, but moves the update *decision* from the box admin to the cloud control plane. That delta is # 8.

It covers the A/B OS image's update policy (# 1). The image's layout and build are in `BUILD.md` # 1b.

### Why two and not five

This doc used to describe five independent streams (Debian base, `host-agent`, control plane, apps, managed services). The flip to two is recorded in `DECISIONS.md` 2026-08-11. The short reason: five independent streams means the fleet holds a cross-product of versions that we can never enumerate, let alone test. Two streams means a box is described by two numbers.

The comparable products agree, and the one that disagrees is the cautionary tale. umbrelOS ships whole-system A/B images (Rugix for the image, Mender for the OTA), with apps updating separately from a git-backed store. ZimaOS ships A/B slots via RAUC. Both converged on "the box is one atomic thing, the apps are not." CasaOS is the counter-example: a shell script that downloads per-component binaries (Gateway, MessageBus, UserService, LocalStorage, AppManagement, CLI) from GitHub releases, independently — and per-component drift is a recurring source of broken installs.

We are not copying their mechanism (they are image-based appliances; our stream B is containers, which are already atomic and already versioned). We are copying the **boundary**.

---

## 1. The OS image *(stream A: the box)*

The OS underneath us: kernel, libc, OpenSSL, firmware, Docker itself, and `host-agent` (# 2). Since `DECISIONS.md` 2026-10-01 this is **one A/B image**, on both profiles. There is no `apt` on the update path and no `unattended-upgrades`. The image layout, the per-box state rules and the build are in `BUILD.md` # 1b; this section is the update policy.

> **Status: built on hosted (#563); the appliance waits for #564.** The hosted image is in the A/B layout (#561), every OS release publishes a signed bundle (#562, `BUILD.md` # 1b # The bundle), and `host-agent` applies it (#563, # As built below). The appliance image is not A/B yet, so its `host-agent` has no OS applier and reports stream A as `unsupported`. The engine was chosen by the spike in #485 (`../progress/ab-update-engine-spike.md`). Existing boxes are not migrated to the A/B layout: they are replaced. The hosted update-target answer carries the OS part (# As built), and a box with no OS part in its answer stays on its OS.

### The update transaction

1. **The box learns the target OS version** through the one update-target seam (# 8.4): the cloud's answer on hosted, the signed release manifest on appliance (`RELEASE_MANIFEST.md`). The target names OS releases with the digests of their bundles: one per minor on the way, so a box that missed a minor can step through it (below). The box never resolves "latest" for the OS, for the same reason it never resolves a tag for the brain.
2. **Download and install ahead of the window.** host-agent hands the bundle to RAUC, which checks its signature against the image's `/etc/rauc/keyring.pem` (the offline release root CA, `docs/dev/rauc-signing.md`) and writes it into the **inactive slot** while the box keeps serving. host-agent also checks the bundle's digest against the target, so a signature alone never makes a box install a bundle. A failure here changes nothing: the running slot was never touched.
3. **Switch and reboot inside the window.** The new slot is set to boot next with **one attempt**. Stream A goes last in the window (# 7), after stream B is done.
4. **Mark good only when the box is healthy.** After the reboot, host-agent marks the slot good once `host-agent` itself and the brain answer their health checks. Until then the slot stays on trial.
5. **Revert on its own.** A slot that is not marked good is not booted again: the next boot goes back to the previous slot. A failed health check reboots on purpose, so the revert happens with nobody at a console, under UEFI and under legacy BIOS alike.
6. **Report.** Admins get a notification afterwards (`NOTIFICATIONS.md`, admins only). On hosted the outcome also goes back to the cloud (# 8.4 step 5).

**One attempt per target per window**, as stream B does. A failed OS update is tried again the next night, not in a loop.

**The revert covers the OS, never the data.** The state partition is shared by both slots, so the previous slot boots against whatever the new one wrote. Two rules follow, and they bind every release: the brain's SQLite and every on-disk format must stay readable by the previous release, and `host-agent` must accept state written by the next one.

**Formats change only in minor releases, and a box never skips a minor.** Here a "minor" is a `MAJOR.MINOR` line, and lines are ordered as versions, not by the minor number alone: 2.0 is the line right after 1.9, a major release is the next minor like any other, and "one minor back" from 2.0 is 1.9. A box can revert, on its own or by target, to whatever is in its other slot, so "the previous release" must mean every release it could land on. Three rules make that true:

- **A patch release never changes an on-disk format.** An OS patch release is a lock bump or a fix (`BUILD.md` # Versioning). So any patch of a minor reads what any other patch of it wrote.
- **A minor release may change a format, and must still read and write the format of the minor before it.** 1.4 is held to 1.3's format, not to 1.2's.
- **A box never skips a minor.** Told to move from 1.2.x to 1.4.y, it installs the newest 1.3 patch first, and goes on to 1.4.y in a later window once 1.3 is marked good. **So the OS part of a target is a list, not one release:** the newest patch of each minor from the oldest one still supported up to the target, each with its bundle digest. The **last entry is the target**, and the box picks what to install from where the target sits: if the target's minor is above its own, it installs the first entry whose minor is above its own (the next step); if the target is in its own minor, or one minor below, it installs the target itself (a patch move, or a deliberate one-minor downgrade); if the target is further back, it refuses (below). The hosted answer and the appliance manifest's `os` field both carry this list (`RELEASE_MANIFEST.md`). So the release in its other slot is always the same minor or the one before, and both can read its state.

**An OS downgrade by target follows the same reach.** The update loop applies whatever the target names, in either direction (# 8.4). For the OS, the box refuses a target that is more than one minor back (from 1.4.y: any 1.4 or 1.3 patch, never 1.2), and a target below the running control plane's floor (`minimumAgentVersion`, # 7 Compatibility matrix), since that would boot a `host-agent` the brain refuses to work with. A refusal is logged and changes nothing, like any other refused target. This is the same discipline the brain's own rollback already needs (`NEXT.md` # Brain state-migration framework).

**Which boots are on trial** (the maintainer's call, #563). Only the **first boot after an OS switch** is on trial: it is marked good once the brain answers, or given up. Every other boot is marked good as soon as `host-agent` starts. So a problem that has nothing to do with the OS (a broken control-plane update, a slow app) never moves a box back to its older OS, and a box with only one good slot never loops through reboots.

**A slot that hangs must still revert.** Boot counting only helps a slot that reboots. The spike saw a slot stop in the initrd's emergency shell and wait for ever. So the image reboots on emergency and rescue, sets `panic=` on the kernel command line, and has systemd feed a hardware watchdog. Hetzner Cloud VMs have one, and a hung box resets about 2 minutes after systemd last fed it. A hang in GRUB or the initramfs, before systemd, is not covered: only a crash there reboots (`BUILD.md` # 1b # As built).

### As built (#563), on hosted

`internal/hostagent/osupdate` is the transaction, `internal/hostagent/updatetarget` decides which release and when. Both run in `host-agent`; the brain only reports.

- **The answer's OS part** is an optional `os` list, oldest first, each entry `{version, bundle_url, bundle_sha256}` (# 8.4 has the wire). The box refuses an entry with no 64-hex sha256, a version that is not a plain `X.Y.Z`, a list out of order, or a `bundle_url` that does not start with the expected prefix (`MOOSE_UPDATE_OS_URL_PREFIX`, default `https://github.com/onmoose/os/releases/download/`). It then picks as # The update transaction says: the next minor's entry, the target in its own or the previous minor, or a refusal. It also refuses a list whose next step is not the minor right after the box's own (a list that leaves a minor out), and a release below the running control plane's floor: the brain writes `minimumAgentVersion` to `/var/lib/moose/state/minimum-host-agent` at start. **A box that cannot read that file refuses every move to an older release.** An `os` part of the wrong JSON shape is refused like a bad entry. **Each stream is judged on its own**: a bad OS part holds back stream A only, and a bad control-plane part holds back stream B only. The running OS release is `host-agent`'s own version, which is the version of the slot it ships in.
- **Install, ahead of the window** (an `os-install` job, under the same lock as `system-update`). The bundle is downloaded to `/var/lib/moose/os-update/` on the state partition and its sha256 checked against the answer before RAUC sees it. Then `rauc install`, which checks the signature and writes the other slot. `system.conf` has `activate-installed=false`, so the boot order does not move. The file is deleted afterwards. A failed attempt is not repeated the same night for the same release and digest.
- **Switch, inside the window** (an `os-switch` job), and only when stream B does not hold the window (it did not just start an update, and no job runs). `rauc status mark-active other` puts the new slot first with `OK=1 TRY=0`; RAUC's GRUB backend resets the slot's try flag there, and `host-agent` checks the grubenv says so before it reboots. It writes a trial marker for the new slot (`/var/lib/moose/os-update/trial-<slot>`) and the record (`state.json`), then reboots. A reboot that is not accepted undoes the switch. An undo removes the trial marker only after the booted slot is first again; if that fails, the marker stays, so a new slot that still boots next boots on trial. **One OS switch per night**, so a box several minors behind takes one step per window. **Nothing installs or switches while the booted slot is on trial**, because the other slot is then the way back.
- **The trial.** On the new slot, `host-agent` waits for the brain's `/healthz` for up to `MOOSE_OS_TRIAL_TIMEOUT` (10 min, at most 14), and never past 13 min after boot: the image's timer counts 15 min from boot, and the 2 min between are for the final RAUC mark, so `host-agent` always decides first. Healthy: `mark-good`, the marker goes, the outcome is `good`. Not healthy: `mark-bad booted` and a reboot, and GRUB boots the old slot. **A `host-agent` that never starts** cannot do either, so the image carries `moose-os-trial.timer` (15 min after boot): if the booted slot still has its marker, it marks the slot bad, leaves a note (`safety-net-<slot>`) and reboots. The timer goes by the marker only, never by the grubenv's `TRY` flag: GRUB writes that flag through the firmware, and a firmware can lose the write (#575), so a hung slot that looked good would never be rebooted. On the old slot, `host-agent` finds the new slot's marker, records the outcome `reverted`, marks that slot bad, logs whether the safety net made the revert, and stays. The same release is tried again the next night. Only a switch that took effect counts: the record's `activated` flag is written after `mark-active` succeeds, and a slot that gives up its trial leaves a note (`trial-failed-<slot>` from `host-agent`, `safety-net-<slot>` from the timer). So a power cut before `mark-active` never reads as a revert, and a power cut right after it still records one. A new switch clears the old notes of its slot. When RAUC cannot mark the slot, the trial does not loop: a slot it cannot mark bad is rebooted once, a slot it cannot mark good is not rebooted, and the image's timer reboots a slot at most once; after that the box stays up for a person. A download left by a kill or a power cut is deleted at the next start.
- **Report.** `GET /api/v1/system/version` carries `os_version` and `os_slot`; `GET /api/v1/system/update-target` carries an `os` object beside stream B's fields (`BRAIN_HOST_PROTOCOL.md`). The brain raises one admin notification per outcome (`NOTIFICATIONS.md` # Updates). Reporting back to the cloud (# 8.4 step 5) still waits for real box authentication.
- **What it costs a box** (CI, `../progress/host-agent-os-update.md`): one bundle download per update (438.5 MB for a real release), the same space on the state partition while it installs (1.1% of a 40 GB disk), and one reboot.

### Policy: automatic on both profiles

**The OS update applies by itself and reboots in the window, on appliance and on hosted** (`DECISIONS.md` 2026-10-01). An A/B update only takes effect after a reboot, so a box that waited for a click would never be patched. The automatic revert is what makes this safe; it is the rollback this doc used to say auto-apply was waiting for.

- Security fixes and every other change to the OS ship the same way: as a new OS release (`BUILD.md` # Versioning). There is no separate "security only" track, because there is no package manager on the box to pick packages with.
- A Debian major upgrade (13 to 14) is also just a new OS image. Nobody reinstalls (`NEXT.md` # OS major-version upgrade commitment, resolved).
- What the admin sees about the reboot (#509): Settings → System says beforehand that tonight's window restarts the box, and an admin can press **Update now** to switch and restart now instead (# 7, # 8.4). Pushing one night's restart back is still open (`NEXT.md` # Reboot scheduling UX).

---

## 2. `host-agent` *(stream A: the box)*

Tiny native binary, supervises the brain (`CONTROL_PLANE.md`). **It ships inside the OS image** and updates with it (# 1). It has no update path of its own: no `.deb` in an apt repo, no self-update.

- It is the one moose component on the host, so it belongs to the host's atomic unit. A new `host-agent` and a new kernel arrive in one image, tested together.
- Its version is the moose (OS) version (`BUILD.md` # Versioning). The control plane's compatibility floor reads it (# 7 Compatibility matrix).
- The brain never updates it. Brain orchestrating its own supervisor would be a layering inversion.

The apt-based options this section used to weigh (`unattended-upgrades` from our repo, brain-orchestrated, admin-triggered) were retired by `DECISIONS.md` 2026-10-01.

---

## 3. Control plane — `moose-brain` + `moose-ui` *(stream B — containers)*

The control plane ships as **two container images** on **one release manifest**: `moose-brain` (the daemon) and `moose-ui` (the dashboard, per `WEB_UI.md`). Most weeks the UI moves and the brain doesn't; occasionally the brain moves and the UI doesn't; occasionally they move together (coordinated change requiring a new brain endpoint that the UI consumes).

This is the most user-visible update stream because the brain + UI together *are* moose from the user's perspective.

**The control plane has its own version line** (`DECISIONS.md` 2026-10-01, `BUILD.md` # Versioning). A control-plane release never needs an OS release, and an OS release never changes which control plane a box runs: the ledger (# 8.3) keeps naming the pair the box was on, so a new slot launches the same brain and UI. The control plane only ever waits for the OS through its compatibility floor (# 7). A new box starts on a released pair too: the disk image bakes a control-plane release, never unreleased code under a released number (`BUILD.md` # Versioning, #566).

**One channel, two artifacts.** The user sees a single "auto-update moose" affordance. The updater pulls and recreates only what changed — UI-only ship recreates only `moose-ui`; brain-only ship recreates only `moose-brain`; coordinated ship recreates both as one transaction (pull both, recreate both, verify both healthy, commit; on failure, revert both).

### Options on update trigger

- **A — Auto-pull `latest` tag continuously.** Box polls registry, pulls when tag advances.
- **B — Release manifest.** Box polls a JSON manifest at `releases.onmoose.io/stable.json` that lists the *current* stable version. Gives us a kill switch (retract a bad release) and a place to gate rollouts if we later need pacing. See `RELEASE_MANIFEST.md` for the full schema + publishing pipeline.
- **C — Periodic prompt.** Box checks for updates, surfaces "moose X.Y.Z available — update now?" in the UI.
- **D — Fully manual.** Admin clicks update.

### Decision: B — release manifest, admin-prompted

- Release manifest (not raw `latest` tag) because we need a kill switch. If we ship a bad version and 5% of boxes start crashlooping, we want to flip the manifest back and stop *availability* of that version *now*, before more boxes prompt their admin to install it.
- Admin-prompted (not auto-applied) because v1 has no A/B rollback at the OS level. *(That reason no longer holds once the A/B OS image ships (# 1). The control plane stays admin-prompted on appliance for now: it has its own rollback, and the policy was not reopened with `DECISIONS.md` 2026-10-01.)* Phone-OS-style auto-apply assumes hardware-backed rollback we don't have until A/B images land. Surfacing "moose X.Y.Z is available" and waiting for the admin is the honest posture.
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
2. If a newer manifest applies to this box (channel, host-agent compat), host-agent surfaces a "moose update available — vX.Y.Z" notification in the dashboard. With the two version lines (`BUILD.md` # Versioning), the version shown here is the control-plane release, not the moose (OS) release. Current versions keep running.
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

The diff is built (#631, `lifecycle.widenedAccess`): the update check reads it, the `public_paths` rule included, and an update that widens access is never applied until its owner allows it. The owner's yes, the prompt and the admin's view are built (#633, # Who gets prompted # As built (#633), # 6).

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

#### As built (#633)

- **The diff is data.** `lifecycle.widenedAccess` returns a list of items, not sentences: `internet`, `lan`, `gpu`, `device` (with the path), `folder` (with the folder, the new mode, the change: `new`, `write` for read to write, `whole` for a pick-subfolder folder that becomes the whole folder, and the source the update binds) and `public_path` (with the path). The dashboard says each item in the install screen's words (`APP_MANIFEST.md` # E), through one function (`wantLines` in `web-ui/src/installSteps.ts`, next to `permissionLines`), so the prompt, the app's settings page and Settings → Users say the same thing. The list is worked out for every new version and stored on the row (`instances.update_wants`), whatever the class, so an update that is refused for another reason and also asks for more access says both. A row classified by a brain from before #633 has no list, and the next check classifies it again.
- **Who may say yes.** The owner of a personal app, and an admin for a household app. This is stricter than who may press Update: an admin can control another person's personal app, but cannot allow new access to it (`approverOK` in `internal/api/appupdate.go`). The API answers that admin `403`, and audits it.
- **The yes is stored per instance** and names the version the owner saw and the access they accepted: the new manifest's `permissions` and `access.public_paths`, as JSON (`instances.update_approved_version`, `update_approved_access`, `update_approved_by`, `update_approved_at`). An update is **covered** when it asks for no more than the accepted access. A covered update is class `available`. So a later version that asks for even more asks again, and one that asks for no more does not: v2 allowed, v2 failed and was rolled back, v3 with v2's access is applied with no second question. The nightly window may then try a covered update that failed, under the 3-failures rule (# Update window). A good update clears the yes.
- **The guard is the class.** `Manager.Update` classifies the update again under the instance's lock, with the manifest it applies and the yes on the row. A person's Update, the nightly window and the yes all go through it, so an update that asks for more than was allowed is never applied, whoever starts it.
- **Allow & update** (`POST /api/v1/apps/{id}/update/approve`, with the version the person saw) stores the yes and runs the update at once, as the same job as Update, whether the automatic update is on or off. Under the instance's lock it refuses a stopped app, an update that does not wait for a yes, a version that is not the one the box offers now, and a manifest that now asks for something else under the same version (it stores the new list, so the next look shows it). It lets the lock go before the update takes it; an update that starts in between is classified again and is covered only for the access the person allowed.
- **Keep current version** (`POST /api/v1/apps/{id}/update/decline`) stores the version on the row (`instances.update_declined_version`). The prompt does not ask about that version again. The app's settings page still offers Allow & update, and a new version that asks for more asks again. A yes clears it. It grants nothing, so it is not audited.
- **Closing the prompt without choosing** is stored on the session (`sessions.update_prompt_dismissed`, `POST /api/v1/me/update-prompt/dismiss`, read through `GET /api/v1/me`). A new sign-in is a new session, so the prompt comes back then, and not on the next page load or in another tab (`DECISIONS.md` 2026-11-06, "Keep current version" is a no for that version).
- **What an allowed update turns on.** The update transaction of #631 was written for updates that ask for no more, so it gained these: a folder the new version adds is bound with the install default (the owner's own folder for a personal app, the household's shared folder for a household app, and the manifest's `default` subfolder for a `pick-subfolder` folder), made ready the way install does it; a `pick-subfolder` folder that becomes `whole` binds the whole folder from the same source; and the app's own network follows the new `internet` value in both directions (`APP_LIFECYCLE.md` # update + rollback). The last one also fixes a narrowing update, which #631 applied without a question and which kept the old network with internet. `devices`, `gpu` and a read to write mode follow the new manifest when the override is written, and a new public path is in the route the update writes. `lan` is not enforced anywhere yet (there is no macvlan), so allowing it changes nothing at run time.
- **A first folder, a first device or the GPU** on an app that had none moves the app to the host tier on a remapped box, so the update is `blocked` (# As built (#631)), not something an owner can allow.
- **On hosted** the prompt is the same (# 8.2): the owner allows it, not the control plane.

### Update window

Same as brain: **03:00–04:00 local** by default. App updates serialize one at a time within the window — never two concurrent app updates.

The rules of a night, for every app whose update the box may apply on its own (the toggle on, # Per-app auto-update toggle, and the class `available`, # As built (#631)):

- **The window is the box's one update window**, the same window and the same time zone as the control plane and the OS (# 8.4). It can come from the control plane's answer, from `MOOSE_UPDATE_WINDOW`, or from the default; the apps never have a window of their own.
- **The apps go after the control plane** (# 7 Update ordering) and before the OS switch, which waits for them (# 7 Reboots).
- **One app at a time**, oldest install first.
- **A cut-off: the window's end minus 20 minutes** (03:40 for the default window). No new app update starts after it; what is left waits for the next night. The 20 minutes leave an update that started just before the cut-off its health wait and a rollback, and leave the OS switch time to reboot inside the window.
- **One try per version per night.** A failed update is tried again the next night, not in a loop.
- **After 3 failures in a row on the same version, the box stops trying** that version on its own, and the app is paused (# 6 Failure signaling). A new catalog version starts the count again. The Update button still works on a paused app, and a person's Update sets the count back to 0 before it runs, so a failure then counts 1 and the window tries again the next night.

#### As built (#632)

`internal/lifecycle/nightly.go` is the loop. `cmd/brain` starts it at boot, beside the other loops. Everything it needs to remember across a night is in SQLite, because the control-plane update restarts the brain in the same window.

- **It wakes every minute** and reads host-agent's update-target read (`GET /v1/system/update-target`, `BRAIN_HOST_PROTOCOL.md`). It takes the window from `window` and the zone from `zone`. The brain runs in a container whose local zone is not the box's, so it never uses its own `time.Local`; the zone database is built into the brain (`time/tzdata`), so the image needs no zone files. A read with no window, or one the brain cannot parse, uses 03:00-04:00; a read with no zone, or a zone the brain does not know, uses UTC. Each is logged once at warn.
- **A night is named by the date its window opens on** (`YYYY-MM-DD`). Every time in it (the start, the end, the cut-off) is built on the wall clock, the way host-agent builds its night, so a night the clocks change on keeps its hours.
- **Work tonight** is an instance that is `running`, not a Door-2 app, has its automatic update on, has a stored update of class `available`, was not tried tonight in that version, and is not paused. Before the first update of a night the loop runs the update check once more (`CheckUpdates`), so it acts on the latest catalog.
- **The hold.** From 15 minutes before the window opens (02:45 for the default window), while there is work, the loop holds the OS switch on host-agent and renews the hold every minute (# 7 Reboots). As soon as there is no work left, or at the cut-off, it clears the hold.
- **Waiting for the control plane.** No app update starts until host-agent says stream B is done for tonight: its read's `control_plane_done_night` names tonight (# 8.4). A host-agent from before #632 does not send that field; the brain then goes when the read was checked inside tonight's window and no control-plane update waits to apply (`state` is not `available`, or `auto_apply` is false), and otherwise waits until the cut-off.
- **The update** is the same transaction as a person's Update (`Manager.Update`, # As built (#631)), with the same instance lock and the same limit of 3. The loop writes the try (the version and the night, `instances.update_tried_version` and `update_tried_night`) **before** the update runs, so a brain that stops in the middle of an update does not try the same version again that night. After an update the loop goes on to the next app at once, without waiting a minute.
- **The count** is `instances.update_failed_count`: one more for each recorded failure of the same version (`RecordInstanceUpdateFailure`), 1 for a failure of another version, 0 after a good update and before a person's Update (`Manager.UpdateByPerson`). An app is paused when the count is 3 or more and the failed version is the version the box offers (`store.Instance.UpdatePaused`). A failure that is not recorded on the row (the catalog could not be read, the app was stopped) does not count, but still uses tonight's try.
- **Proven on a booted box** by the `app-update` cloud boot (#630, `TESTING.md` # Hosted cloud variant): the hold before the window, the app update inside it, and the OS switch only after it. host-agent's loop is not woken by a new hold (only a cleared one wakes it), so its first tick in the window can come up to one poll (15 minutes, plus its jitter) after its last tick. Waking when the window opens (`holdWake`) needs a tick that already sees the hold, before the window. The poll can sleep up to 17 minutes and the brain holds from 15 minutes before the window, so that tick can be missed: the first tick in the window then comes up to about 2 minutes after it opens. That is well before the cut-off, 20 minutes before the window ends.
- **Each outcome is logged** with `instance_id`, `name`, `version`, `new_version` and `night`, a failure with `count` too, and a pause with its own line. Each outcome is also told to the owner (# 6 Notification center, # As built (#634)), and a good update and a recorded failure are marked as the window's (`instances.update_last_auto`, `update_failed_auto`), which the toast and the tile's "last night" read.

### Update mechanics per app

1. Pull new image.
2. **Snapshot the app's state** — see "Pre-update snapshot" below.
3. Run `pre_update` hook (`APP_MANIFEST.md`) — deferred from MVP; the snapshot is the v1 safety net.
4. If managed-service major version changed: take pre-migration backup (per `SERVICE_PROVISIONING.md`), spin up new major, `pg_dump | pg_restore`. As built (#635): the backup is the snapshot's dump of step 2, restored into the new major after the old version stops (# As built (#635)).
5. Stop old container, start new container with the same volumes.
6. Wait for the new version to become healthy, for up to `max(120s, health_probe.start_period + 60s)`. `start_period` is the manifest's when it declares a `health_probe`, else the 60s default (`APP_MANIFEST.md` # B), so with the defaults this is 120s, the same as install. A slow app is not rolled back for being slow.
7. **On failure:** restore from the pre-update snapshot, revert to previous image. If managed-service was migrated, revert to the previous major and restore from the pre-migration dump. Notify admin. As built (#635): the database on the new major is dropped and the app points at the old one again, which the update never changed.

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

#### As built (#347)

The snapshot, its restore and the 7-day cleanup are built as functions on `lifecycle.Manager` (`internal/lifecycle/snapshot.go`). The update transaction (#631, # As built (#631) below) calls them.

- **The app is stopped while the snapshot is taken.** A tar of files the app is still writing is not a copy you can trust, so the caller holds the instance's lock and has stopped the app's containers. The functions take no lock themselves, so the update transaction can call them inside its own. This means the update stops the old version before the snapshot, not after it (#631 orders the steps).
- **What the tar keeps.** The declared `data_volumes`, minus every `cache_volumes` path inside them. A manifest that declares no `data_volumes` (a Door-2 app, or a catalog app that leaves the list out or empty) gets the best effort: all of `data/` and every other relative bind dir of the compose (`./config` next to `./data`), minus any declared cache. An instance installed before #347 has a stored manifest with neither list, so its first snapshot is also the best effort. The tar keeps each entry's numeric owner, mode and modification time, and keeps a symlink as a symlink, never following it. A path whose parent folder is a symlink is refused. Sockets, devices and pipes are left out and logged. A hard link comes back as a second copy of the file.
- **The files.** All in `snapshots/`, named after the version the instance ran: `pre-update-<version>.tar`, one dump per database (`pre-update-<version>.<service>.pgdump` for Postgres, `.sql` for MySQL and MariaDB), and `pre-update-<version>.json` last. The `.json` record lists what the set holds and is the mark that it is complete. Every file is mode `0600`. A version character that is not safe in a file name becomes `_`.
- **The dumps** run through the same throwaway client container as provisioning (`SERVICE_PROVISIONING.md` # Provisioning protocol), so the brain never joins a service network. The dump streams out of the container's stdout into the file, and back into its stdin on restore (`docker run --rm -i`), so no bind mount is needed and the client's remapped user never touches the brain's files. Postgres uses `pg_dump --format=custom`. MySQL and MariaDB use `mysqldump` (`mariadb-dump`) in one transaction, with routines, triggers and events.
- **Valkey has no per-app dump.** Every app shares one Valkey keyspace and has no database of its own (`SERVICE_PROVISIONING.md` # Per-app isolation in shared instances), so there is nothing per app to dump or put back. A Valkey grant is listed as skipped in the record. Apps use Valkey for caches, sessions and queues, which a rollback can lose; an app that keeps data it cannot lose only in Valkey is not protected.
- **No snapshot, no update.** Every failure is returned to the caller, and a half-written set is removed first. The brain refuses a snapshot that would leave the drive more than 95% full (the `disk-full` threshold in `HEALTH.md`), and returns `ErrSnapshotNoSpace`. The need is measured before anything is written: the tar's size, plus each database's own size (`pg_database_size` for Postgres, whose compressed dump is smaller, and twice the size of the tables and indexes in `information_schema` for MySQL and MariaDB, whose dump is text and writes binary as hex). The free space comes from `statfs` on the snapshots folder. The estimate can still be low, so the drive is checked again after each dump. A write that fails on a drive past the threshold is also `ErrSnapshotNoSpace`, because a dump that fills the drive fails inside the client container and reaches the brain as the container's error, not as "no space left".
- **Restore** puts every data volume back exactly as the tar holds it: files added since are removed, a data volume that did not exist then is removed, owners, modes and times are set from the tar, and the cache volumes are left as they are. Each dumped database is dropped (`WITH (FORCE)` on Postgres), made again with the same owner, and filled from its dump, so tables and rows added since are gone. Before it clears anything, the restore reads the whole tar once, so a tar damaged on disk fails while the data is still in place. A restore can run again after a failure: it reads the same files and never changes them.
- **Cleanup.** The brain removes snapshots older than 7 days once at startup and then every hour (`lifecycle.Manager.PruneSnapshots`, started from `cmd/brain/main.go`). The brain has no scheduler (`NEXT.md` # Container image cleanup), so this is a plain loop like the notification prune. It skips an app whose lock is held, so it never removes a snapshot an update or a rollback is using. The same pass removes the previous generation and its images when the rollback by hand to it is over (# As built (#634)).

**App-side rollback hooks are deferred.** A `post_update_rollback` hook fired only when `post_update` fails is the right long-term shape for apps that need bespoke recovery, but it pushes complexity onto every author for a case the snapshot already handles. Sketched in `APP_MANIFEST.md` # F; not in v1.

#### As built (#631)

The box finds an update, sorts it into a class, and updates one instance when a person asks, with the rollback below. The nightly window is #632 (# Update window, # As built (#632)), the owner's approval #633 (# Who gets prompted # As built (#633)), the update results, the notifications and the rollback button #634 (# As built (#634)), and a managed database's major move #635 (# As built (#635)).

- **Detection is by catalog version only.** After every catalog sync that reached the catalog service (a 304 included, so a check that failed is tried again), and once at brain startup, the brain compares each instance's version with the catalog entry's `version` (`lifecycle.Manager.CheckUpdates`; the catalog calls back through `catalog.OnSync`, since lifecycle reads the catalog and not the other way round). It never compares image digests: the browse payload carries none, and a switch from an index digest to the `amd64` one must not read as a new version (`APP_STORE.md`). For each new version it loads the new manifest once and stores the class on the instance, so a sync that changes nothing does not fetch it again. The check takes no lock while it loads a manifest, so it stores its result under the instance's lock, after reading the row again, and stores nothing when an update finished in the meantime. A Door-2 app (version `custom`, no catalog entry) never gets an update. An app the catalog does not list keeps what was stored, because a box that has not synced yet has an empty catalog.
- **The classes.** `available`: the box can apply it. `needs_approval`: it widens `permissions:` or `access.public_paths` (# Auto-apply unless permissions expand) more than the owner allowed; never applied until the owner allows it (#633), and then it is `available`. A managed database that moves to a newer major of the same engine is no class of its own: the update moves the data (# As built (#635)). A managed service the old version did not have is not a block either: the update provisions it, as install does, and a rollback drops it. `needs_newer_moose`: a `manifest_version` this brain cannot read (# 7 Compatibility matrix). `blocked`: the update would change the instance's user-namespace tier or turn `service_user` on or off (its data would need a new owner, `APP_ISOLATION.md` # User-namespace tiers), or it adds a required config field with no default that the instance has no value for (the box cannot invent it), or it moves a managed database to another engine (MySQL to MariaDB or the reverse) or to an older major (a dump from a newer major may not restore into an older one, # As built (#635)). `service_major` was the class of a database major move before #635; no brain writes it now, and a row that holds it is classified again by the next check. Each class but `available` comes with a plain sentence that the app's page shows.
- **Update now.** `POST /api/v1/apps/{id}/update` runs the transaction at once, as a job, whether the toggle is on or off (`BRAIN_UI_PROTOCOL.md`). The owner can call it, or an admin for a household app. It answers `409` with the plain sentence for an app with no update the box can apply, a stopped app and a Door-2 app. It is elevation-class, like install: success, every failure and every refusal are audited as `app.update` (`LOGGING.md`).
- **The order of the steps** is set by the snapshot, which needs the app stopped and reads the manifest on disk: pull the new images while the old version runs, then (for the image tier) check that the new images run as the same users, keep the previous generation, stop the old version, take the snapshot, and only then write the new manifest, compose, override and `.env` and start the new version (`APP_LIFECYCLE.md` # update + rollback). So the downtime is the switch, not the download.
- **No snapshot, no update.** When the snapshot cannot be taken (`ErrSnapshotNoSpace`, or a dump fails), nothing new is written, the old version is started again, and the reason is stored as the last failure, so the tile shows it.
- **After a rollback the app is `running`**, not `update_failed`: the old version runs on its old data, and every part of the brain that treats `running` as "the containers are up" (the startup pass, Stop, the health and restart loops, the dashboard) keeps working. The failure is stored on the instance: the version that failed, the step (`pull`, `snapshot`, `start`, `health`, `refused`, `snapshot_restore`, `interrupted`), a short message and the time. It drives the tile badge and is what #634 lists; a later good update clears it. `update_failed` is the state when the rollback itself could not bring the old version back. It is treated like `failed`: the dashboard offers Retry, and Start first finishes the rollback if it did not finish (`DECISIONS.md` 2026-11-06).
- **A rollback changes nothing until no container of the app runs.** The undo clears data folders and rebuilds databases, so it must not run under a live app. It first runs `compose stop`. That reads the files on disk, which can be the new version's broken files, so a failed stop says nothing about the containers. So after a failed stop the brain asks Docker for the running containers of the project `moose-<id>`, by the compose project label, and stops them by id without the compose files. When Docker says none run, by the first answer or after the direct stop, the rollback goes on. When some still run, or Docker does not answer, the rollback stops before it moves a database or touches data: the row is `update_failed`, `update.json` stays, and Start tries again. The same holds for a failed update, the rollback by hand (# As built (#634)) and the startup pass (`Manager.stopForUndo`).
- **A brain that stops mid-update** is covered by the startup pass: an instance left in `updating` is put back to the old version, from the snapshot and the previous generation when the update got that far, and the failure is recorded as `interrupted`. No row stays `updating`.
- **The previous generation** is kept as files next to the current ones (`manifest.yml.prev`, `compose.yml.prev`, `compose.override.yml.prev`, `.env.prev`) and as pins in `instance_images_prev`. Uninstall removes those images too. They go after 7 days with the snapshot, and an update that does not go through keeps the generation before it (# As built (#634)).
- **New secrets** a version declares are generated; the ones the instance has are kept as they are.
- **Concurrency.** At most 3 installs and updates run at once, one shared limit (`APP_LIFECYCLE.md` # concurrency), and an update holds the instance's lock for its whole run.

#### As built (#634)

The rollback button and the cleanup that ends it.

- **The rollback by hand.** For 7 days after a good update, the owner (or an admin for a household app) can put the app back on the version it ran before, with the data it had just before the update (`POST /api/v1/apps/{id}/rollback`, a job; `lifecycle.Manager.Rollback` in `internal/lifecycle/rollback.go`). It uses what the update kept: the snapshot of the old version, the previous generation, and the update's own record, now kept as `update.prev.json` next to the `.prev` files, which says what the update added (managed services, secrets, config defaults, a remade network). The rollback is the undo of a failed update run later: stop, restore the snapshot, put the previous generation back, drop what the update added, start the old version. It writes `update.json` with `rollback: true` first, so a brain that stops in the middle finishes it at the next start, and a rollback that cannot finish leaves the app `update_failed`, which Start finishes. Before it starts the old version, it writes the override's environment and the `.env`'s mail lines again from the store, because the `.prev` files are from before the update: a setting changed, or an AI or email account deleted or moved, in those days must not come back (`restampFromStore`). Once the undo is done the record says so (`undo_done`), so a brain that stops after it never runs the undo again over files that already moved on. It records no update failure. It works on a running, stopped or failed app and ends with the app running (or `failed`, with Retry, when the old version does not start).
- **Data written since the update is lost**, because the snapshot is from before it. The dashboard says so in the confirm dialog, with the date of the snapshot.
- **After a rollback the app's automatic update is off** (`DECISIONS.md` 2026-11-07), so the window does not apply the same version again the next night. The version it left is stored as declined, so the prompt does not ask about it again. The update check runs after the rollback, so the app's page offers the catalog's version again by hand.
- **The 7 days start when the snapshot was taken.** A good update stores the version before it, that time, and whether the window made it (`instances.update_last_from`, `update_last_at`, `update_last_auto`). The app DTO says `updated_from`, `updated_at`, `updated_overnight`, `rollback_until` and `can_rollback`. Who may roll back is who may press Update. It is elevation-class: success, every failure and every refusal are audited as `app.update_rollback` (`LOGGING.md`).
- **One generation, kept through a failed update.** A good update replaces the previous generation. An update that does not go through no longer drops it: before it writes over the `.prev` files, the update copies them (and `update.prev.json`) to `<name>.old`, and a failure puts them back with the previous pins, so 1.0 to 2.0 good, then 2.0 to 3.0 failed, still lets a person go back to 1.0. A good update removes the copy. After a rollback by hand there is nothing more to go back to.
- **Cleanup.** The hourly prune (`PruneSnapshots`) removes, in the same pass as the snapshot, the previous generation (the `.prev` files, `update.prev.json`, the pins) and its images when nothing else uses them, once the 7 days are over or the snapshot is gone. So the button and what it needs go together. An app with `update.json` is left alone, snapshot included, because an update or a rollback that did not finish needs both. A previous generation from a brain before #634, with no time on the row, goes by the age of its `manifest.yml.prev`.

#### As built (#635)

A managed database that the new version wants on a newer major of the same engine (Postgres 15 to 18, MySQL 8.0 to 8.4, MariaDB 10.11 to 11.4) is moved during the update, as `SERVICE_PROVISIONING.md` # At app update, different major version designs (`internal/lifecycle/servicemove.go`).

- **When.** After the snapshot (# As built (#347)), while the old version is stopped, as the first part of writing the new generation (step `moving_databases`). The snapshot already holds a dump of the database from the old major, so the move takes no second dump.
- **How.** The new major's shared instance starts if it is not running (lazy spinup). The app's role (Postgres) or user (MySQL, MariaDB) is made on it with the same name and password, and the database with the same name, filled from the snapshot's dump with the new major's own `pg_restore` or client, through the same one-shot client container. Then the grant points at the new major, and the new `.env` names the new host (`MOOSE_SERVICE_<NAME>_HOST`, `_DSN`). The names stay the same because a MySQL-family dump names its user in the `DEFINER` of each view, routine and trigger, and a Postgres dump makes the role the owner of each object: the dump restores as it is only for the same names.
- **A name another grant holds** on the new major (a 1 in 65,536 chance for two installs of the same app) is refused: the update fails with a plain sentence and rolls back, and nothing on the new major is dropped. The move never drops or shares another app's database. From the check until the update ends, the brain also holds the names in memory, so two updates that run at once cannot both take them.
- **Free space.** The new database takes about as much room as the old one, which stays. The move refuses to start when that would leave the drive more than 95% full, with the same rule as the snapshot.
- **Refused when classified** (class `blocked`, with a plain sentence): another engine (MySQL to MariaDB, or the reverse, is not a major move), an older major (a dump from a newer major may not restore into an older one), and a Valkey version change (Valkey has one version and no per-app data).
- **A failed update** drops the database on the new major and points the grant back, then restores the snapshot as for any failed update. The old database was never changed. The app ends `running` on the old version, with the failure recorded (# As built (#631)). The record (`update.json`) lists each move before anything is made on the new major, and marks it started once the names are known to be free, so the startup pass after a crash does the same, and drops only what this update made.
- **The old database stays for 7 days**, so the rollback by hand (# As built (#634)) can point the app back at it (`service_grants_retired`). A rollback by hand drops the database on the new major, points the app back, restores the snapshot into the old database and starts the old version. It refuses ("no earlier version") once the old database is gone. The hourly prune drops the old database 7 days after the move, or as soon as there is no earlier version to roll back to. Uninstall drops it with the rest.
- **The update class.** A move is no class of its own: an update that moves a database and asks for no more access is `available` and is applied in the nightly window like any other. One that also asks for more access waits for the owner's yes (#633).

### Per-app auto-update toggle

- Default ON (`SPEC.md`).
- Off means: never auto-update. Admin sees "X update available" badge, clicks to apply.
- **As built (#634):** a rollback by hand turns the toggle off (`DECISIONS.md` 2026-11-07).
- Off does **not** mean "freeze the version" — security-classified updates (we'll need a flag in the manifest) still apply auto if the catalog marks them critical. Open question: do we ship that flag in v1, or honor "off" strictly? Lean strict in v1 — fewer surprises.
- **As built (#631):** the setting is stored per instance (`instances.auto_update_off`, on for every instance, new and old) and set on the app's settings page, next to the automatic restart switch, through `PUT /api/v1/apps/{id}/auto-update` (owner, or an admin for a household app). The nightly window reads it (#632, # Update window): an app with the toggle off is never updated by the box. The badge and the Update button work whether it is on or off.

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
  - **As built (#631):** the tile shows a small arrow in its top-left corner when the app store has a new version, in amber when the last update did not go through, with an "Update available" or "Update failed" link to the app's settings page. There, an **Updates** section shows the version, the new version or the reason it is not applied, an **Update to <version>** button when the brain can apply it (the `can_apply` field of the app), and the last failed update with its message. The "What's new" panel and the changelog link are not built.
  - **As built (#634):** the tile no longer shows that a new version is available: no arrow and no "Update available" link (`DECISIONS.md` 2026-11-07). The tile speaks only when an update failed, with the banner of # Failure signaling. The app's settings page still shows the new version and the Update button.
- **Settings → Updates.** Single aggregate view: "X apps updated last night, Y waiting on you, Z failed." This is where the rollback affordance lives (using the kept-for-7-days image + snapshot per # 4).
  - **Not built (#634, `DECISIONS.md` 2026-11-07):** there is no Settings → Updates page. Each app's settings page (Settings → Installed apps → the app) already shows what this view would list for it, under **Updates**: the version, a new one and why it is or is not applied, the update that waits for a yes with Allow & update, the last good update with **Go back to <version>** for its 7 days, and the last failed update with the step that broke and View logs. The notifications and the toast link to that page. `/settings/updates` redirects to the installed apps list.
- **No global "update now" button for apps.** Auto-updates serialize in the 03:00–04:00 window. The dashboard does not pretend the user controls cadence beyond the per-app toggle and the permission-expansion accept (below). **The system has one** (#488): on a hosted box, Settings → System shows the versions the box runs, when it updates itself, what the last control-plane update did, and an **Update now** button for admins when a control-plane update is waiting (# 8.2, # 8.4). It moves that one update earlier; it is not a way to update apps. Since #509 it also covers an OS release that is installed and waiting for the window, and then the box restarts (# 7).

### The permission-expansion prompt

The only case the box asks. Surfaces **on next login of the instance owner**, as a modal on first dashboard load — not a dismissible banner. The app stays on its current version until the user decides.

- **Diff shown in plain language.** "Photos wants new access: **read & write your Movies folder**." Same vocabulary as the install screen (`APP_MANIFEST.md` # E), so the user recognizes it.
- **Two buttons: Allow & update / Keep current version.** No third "remind me later" — closing the modal is dismissal, and the prompt re-surfaces on the *next* login (not every page load).
- **Allow & update applies immediately**, not at 03:00. The user just made a deliberate decision; making them wait until tomorrow morning is confusing. The ~minute of app unavailability is the cost of the choice they explicitly initiated.
- **Accept later** is available from the app's Settings page.
- **As built (#633):** the modal is mounted with the signed-in app (`UpdatePrompt.vue` in `AppShell`) and opens once per page load, when the session has not closed it. Its apps are drawn from the apps list the dashboard already loads: the ones whose update waits for a yes, that the caller may allow (`update.can_approve`: their own personal apps, and household apps for an admin) and that the caller did not say "Keep current version" to for that version (`update.declined`). One entry per app: "<App> <version> wants new access:", then the lines in the install screen's words, with write access in red, and the two buttons. **Allow & update** runs the update job and shows "Updating…" until it ends. **Keep current version** stores a decline for that version. When every app is decided, the modal closes. Closing it without choosing (the X, Escape, a click outside) is a dismissal: it is stored on the session, so the modal comes back at the next sign-in. The app's settings page shows the same lines under the version and an **Allow & update** button for someone who may allow it; someone who cannot (an admin on another person's app) reads whose yes it waits for. The notification of a pending yes goes to whoever may give it (# Notification center, #634).

Consequence (already noted in # 4): two users on the same box may run different versions of the same Tier-3 per-user app for a while. By design — instances are already per-user isolated.

### Admin visibility into other users' versions

The instance-owner prompt model means an admin can't directly see *why* Cindy's Photos is on a stale version. They can wonder why disk usage diverges, or why behavior differs across accounts.

Settings → Users → `cindy` exposes "Apps Cindy hasn't accepted updates for: Photos 2.0 (pending permission: read & write Movies)." Read-only — the admin sees the fact, but cannot accept on Cindy's behalf. Tier-3 per-user instances are Cindy's to authorize.

**As built (#633):** under each person in Settings → Users, "Updates that wait for <name> to allow them:", then each of their personal apps whose update waits for a yes (the ones they said "Keep" to as well, marked so), with the version and the lines in the install screen's words, and "Only <name> can allow these updates." There is no button. The admin's apps list holds every app, so the view is a filter by owner in the dashboard, and the API refuses an admin's yes on another person's personal app (`403`, audited).

### Failure signaling

Auto-rollback already happens (# 4). The dashboard surfaces it:

- **Per-app tile banner**, persistent until acknowledged: "Photos couldn't update to 2.0 last night. Rolled back to 1.4."
- **Settings → Updates** lists the failure with mode (image pull / health check / hook / snapshot restore) and a "view logs" link to the diagnostic bundle (`LOGGING.md`). As built (#634), this is on the app's settings page instead (# Where updates appear).
- **After 3 consecutive failures** to the same manifest (# 4 mechanics), the prompt stops and the banner changes to "Update is failing repeatedly. Paused. See logs." The retry button is still available; the box just stops trying on its own.
  - **As built (#632):** the app DTO carries `update.paused` and `update.failed_count`. The tile's update link reads "Update is failing repeatedly. Paused." in amber, and the app's settings page adds the same sentence under the last failure, with the Update button still there.
  - **As built (#634):** under the tile, until a controller closes it, a banner says "Photos couldn't update to 2.0 last night. Rolled back to 1.4." ("last night" only when the nightly window made the update), or "Update is failing repeatedly. Paused." Closing it is `POST /api/v1/apps/{id}/update/acknowledge`, stored on the app (`instances.update_failed_acked`), so it stays closed on every device; a new failure opens it again. The banner links to the app's settings page. An app whose old version did not start again shows as failed instead. The app's settings page shows the failure with the step that broke, in words (the image download, the copy of the data, the start, the health check, the safety checks, putting the data back, an update that was cut off), the message, the paused text and **View logs**, which opens the Logs section of the same page (`/settings/apps/{id}#logs`). The diagnostic bundle of `LOGGING.md` is not built, so View logs goes to the app's own logs.

### Post-update toast

A small, auto-dismissing toast on the next dashboard visit after an overnight update batch: "3 apps updated overnight" → click for the list with per-app "what's new" snippets. Not modal, not blocking. Auto-dismisses after one view.

**As built (#634):** `UpdatesToast.vue` in `AppShell`. On the first dashboard load after the window updated apps, it shows "3 apps updated overnight" (or "Photos updated overnight") with a link to that app's settings page (or, for several apps, to the installed apps list), for 6 seconds like every toast. It counts the apps the window updated (`update.updated_overnight`) after the person last saw the toast, and in the last day at most, among their own personal apps and the household apps. Once shown, `POST /api/v1/me/updates-seen` stores the time on the person (`users.updates_seen_at`, read back on `GET /me` as `updates_seen_at`), so it shows once, on any device. The "what's new" snippets are not built, as the changelog is not.

### Notification center

The update outcomes surfaced here also fan out to the dashboard notification center (`NOTIFICATIONS.md`), routed per the actionability + ownership rule: **OS / host-agent / brain+UI updates → admins only**; **app auto-update / permission-approval-pending / failed-rollback → the instance owner** (box-wide Tier-2 apps → admins), never broadcast to all users. The per-app tile badge, Settings → Updates view, and post-update toast remain the in-context surfaces; the notification is the durable, read-stateful copy for the user who wasn't looking when it happened.

**As built (#634):** `notify.Notifier` has `AppUpdateApplied`, `AppUpdateFailed` and `AppUpdateNeedsApproval`, in the `updates` category, linking to the app's settings page. A personal app's go to its owner (`audience: user`), a household app's to the admins. The nightly loop tells the window's good update (info), its failure (error: rolled back, or the old version did not start again) and the third failure in a row (error, "update is paused"). Only a failure the update recorded on the row is told. The update check tells a version that newly waits for a yes (warning), to whoever may give it; the same version found again is not told again. Each outcome of each version has its own key (`app-update:<instance>:<kind>:<version>`), so a second failure of the same version refreshes the row instead of adding one, and a good update resolves the failures and the wait for a yes of the version it applied. A person's own Update, Allow & update and rollback are not told: they see the result on the page. Any good update, whoever started it, resolves the failures and the wait for a yes of the version it installed (`AppUpdateDone`).

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
stream A:  the OS image, host-agent inside it  (last; reboots)
```

Reasoning:
- Brain must support the manifest_version of any app coming next, so the control plane moves before the apps that depend on it.
- Stream A goes last because it always needs a reboot, and we'd rather reboot once at the end of the window than mid-flight.
- Stream A has no internal order. Debian base and `host-agent` are one image, swapped as one slot (# 1).

The one ordering constraint that crosses the streams is the **`host-agent` ↔ brain compat floor**: a brain build declares the oldest `host-agent` it will talk to (`minimumAgentVersion` in `cmd/brain/main.go`, surfaced as a health issue per `HEALTH.md`). If a stream-B update would land a brain that needs a newer `host-agent` than stream A has delivered, the update parks with a clear reason rather than proceeding — see # Compatibility matrix.

### Reboots

An OS update always reboots, and that reboot is part of the update (# 1): it happens inside the window, with no prompt, on both profiles (`DECISIONS.md` 2026-10-01). Nothing else reboots the box on its own.

- **Only in the window**, and only after stream B has finished. An app that is mid-update holds the reboot until it is done or the window closes; a reboot that misses the window waits for the next night. **The one exception is an admin's Update now** (#509, `DECISIONS.md` 2026-10-12): an OS release already installed in the other slot is switched to and the box restarts at once, still after stream B. The admin was told before the click that the box restarts and for how long.

**The hand-off between the apps and the OS switch (as built, #632).** Only the brain calls host-agent, never the other way round, and only host-agent can switch the OS. So the brain holds the switch:

- **The hold is a lease.** While the brain has app work for tonight, from 15 minutes before the window opens, it sends host-agent `PUT /v1/system/update-hold` (`BRAIN_HOST_PROTOCOL.md`) about every minute, each time with `until = min(now + 10 minutes, the window's end minus 5 minutes)`. When it has no work left, it clears the hold at once (`DELETE`). host-agent keeps the hold in memory.
- **host-agent enforces both limits itself**: it cuts every hold to at most 10 minutes from now (`protocol.UpdateHoldLease`) and to the window's end minus 5 minutes (`protocol.UpdateHoldCapBeforeEnd`). So a brain bug cannot block the OS switch past the cap, and a brain that dies stops renewing and its hold ends within 10 minutes: the OS switch still goes that night.
- **Why a lease, and not one expiry.** The issue asked for an expiry of at most "the cut-off plus the longest single update". Nobody knows how long the longest update takes: a large image pull alone can take longer than the window. A short lease renewed while the work lasts needs no such guess. A live brain keeps the switch waiting as long as its work runs, and never past the cap. A dead one lets it go within one lease. The cap leaves the switch 5 minutes to start inside the window, so an app update that runs past the cap can be cut short by the reboot; the startup pass then puts its old version back (`interrupted`, # 4 As built (#631)), and it counts as a failure.
- **What the hold stops.** While it is live, host-agent's loop treats the window as shut for the switch only: the download and the install into the other slot still run, because they do not touch the running slot. An admin's **Update now** waits too: the switch is accepted and retried, the way it already waits for a control-plane update, and goes when the hold ends. It is not refused.
- **No missed window.** Clearing a hold wakes host-agent's loop, so the switch does not wait up to 15 minutes for the next tick. While a hold is live the loop also ticks when the hold would end on its own, and when the window opens.
- **A failed read keeps the hold.** When the brain cannot read the update target, it starts no app update, and it renews a hold it already has, read against the last window and zone it read. It never clears the hold on a failed read, so a blip between two app updates does not let the OS switch reboot the box with apps left to update.
- **No hold, no app update.** If host-agent can hold the switch but the hold call fails (a socket error, a 500), the brain starts no app update on that wake and tries again a minute later. Only a host-agent that cannot hold at all (one from before #632, or one with no update loop, which has no switch to hold) lets the apps go without a hold.
- **The control plane does not cut an app update short either.** When stream B was already done tonight (current, or tried) and a new control-plane target shows up later that night while the brain holds the window, host-agent leaves it for the next night. Only a tick whose source answered counts as done: an unreachable source, a refused answer or no target says nothing about tonight, so one blip at 03:00 does not push a good target a tick later to the next night. Once the read has told the brain that stream B is done tonight, though, the night counts as done whatever the outcome, because the brain may already be updating an app on that answer. The reason it waits: the control-plane update restarts the brain, which would interrupt the app update that is running. Without a hold it applies as before.
- **Who sees it.** The update-target read carries `hold_until`, and an installed OS release that waits for the hold says "the switch waits for app updates to finish" in `os.detail` (# 8.4, `BRAIN_HOST_PROTOCOL.md`). The brain re-serves `hold_until` to admins.
- **The appliance** has no OS update yet (#564) and its control plane never applies in the window, so there the apps start as soon as the window opens. The hold works the same way and has nothing to hold.
- Reboot means roughly 30 to 60 s of full unavailability. Acceptable nightly, hostile mid-day.
- `/var/run/reboot-required` has no meaning any more: no package manager writes it.

### Compatibility matrix

The release manifest (#3) carries `minimum_host_agent`. Since `host-agent` ships inside the OS image and carries the moose (OS) version (# 2), that floor reads as **the oldest moose release this control plane runs on**. The field name and the brain's `minimumAgentVersion` stay as they are. The brain carries `minimum_manifest_version` and `maximum_manifest_version` for apps. host-agent carries `minimum_brain_version`.

If an app update wants a manifest_version newer than the running brain supports, the brain refuses the update and surfaces "moose needs to update first" in the UI. The next brain update should resolve it; if it doesn't, the app stays pinned. As built (#631): `manifest.Parse` reads `manifest_version` before anything else and returns `manifest.ErrUnsupportedManifestVersion`, so a newer manifest whose other fields changed shape is still known as newer; the update check stores the class `needs_newer_moose`, and the app's page says "This version needs a newer moose". The check runs again when the catalog moves, and every brain start runs it, so a brain update that can read the manifest offers the update.

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
| A — the box | The OS image, `host-agent` inside it | The previous slot, automatic when the new slot is not marked good (# 1). Built on hosted (#563); the appliance with #564 |
| B — containers | `moose-brain` + `moose-ui` | Previous image pair + SQLite snapshot; revert as a pair, automatic on health-check fail of either |
| B — containers | App | Previous image + pre-update tar of `data_volumes` (+ `pg_dump` of managed-service DB if any), automatic on health-check fail; keep 7 days |
| B — containers | Managed service (patch) | Previous image; data is shared so this is a tag-flip |
| B — containers | Managed service (major migration) | The old database, untouched by the move and kept 7 days, plus the pre-update dump; automatic on app-update fail, and by hand for 7 days (#635) |

The two streams roll back in different ways, and both do it on their own: stream B keeps the previous image, stream A keeps the previous slot. On the appliance, until its image is A/B (#564), stream A has no rollback because it has no update at all (# 1 Status).

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

**An admin may also apply a pending update early (#488).** Settings → System says when the box will update itself and what the last update did, and an admin can press **Update now** instead of waiting for the window. That changes nothing about who decides: there is still no "a new version is available, do you want it?" prompt and no switch to turn updates off. The button only moves the update the box already has earlier. It covers the control plane (stream B) and, since #509, an OS release that is already installed in the other slot (stream A), whose switch restarts the box. An OS release that is still downloading is not waited for.

**The one carve-out that survives: the app permission-expansion prompt (# 4) still prompts the instance owner.** We push *infrastructure* — a new brain, a new UI, a patched Debian. We do not push a *new grant of access to someone's data*. If Photos 2.0 wants read-write on the owner's Movies folder, that is a trust decision belonging to the person whose folder it is, and hosting their box does not transfer it to us. So on hosted, the box updates itself without asking, and an app that wants more permission still waits for its owner. That asymmetry is deliberate.

### 8.3 `host-agent` applies both brain and UI

One actor runs the stream-B control-plane transaction: `host-agent` pulls and recreates **both** `moose-brain` and `moose-ui`. One transaction, one health check, one rollback. The brain cannot recreate itself, and splitting the job so that host-agent does the brain and the brain does the UI would mean two actors, two rollback paths, and a coordinated brain+UI ship that needs them to agree mid-flight.

This has a real cost worth naming: `CONTROL_PLANE.md` locks the **brain** as the launcher of `moose-ui` and Caddy, so `host-agent` is here reaching past the brain to containers the brain reconciles. Left alone, the brain would come back up and reconcile the UI straight back to the version it knew about.

**The staged control-plane compose file is the handoff point.** `host-agent` writes the new UI image reference into the staged compose (`MOOSE_CONTROL_PLANE_DIR`, the same file `lifecycle.EnsureControlPlane` reconciles from) *before* recreating anything. The brain restarts, reads that file, and reconciles to the version already running. The two actors never disagree because they are reading the same declaration — host-agent is the one that writes it, the brain is the one that maintains it. Rollback writes the old reference back the same way.

**One narrow exception: the brain writes one line, once, for the Caddy admin socket (#612, `DECISIONS.md` 2026-10-13).** A box made before #608 kept a staged `caddy.json` with Caddy's admin API on TCP and a `compose.yml` with no bind for `caddy/`, because the staged files are copied once (`BUILD.md` # As built). At its own startup, before its compose up, a brain that finds TCP in `caddy.json` moves the box to the socket (`lifecycle.MigrateCaddyAdminSocket`): it makes `caddy/` for Caddy's remapped root, keeps copies of both files as `caddy.json.tcp-admin` and `compose.yml.tcp-admin`, adds the `./caddy:/run/moose-caddy` line to the `caddy` service, points `admin.listen` at the socket (that file last), force-recreates Caddy alone (`docker compose up -d --force-recreate --no-deps caddy`, since Caddy reads `caddy.json` only when it starts), and waits for the admin API on the socket. If it does not answer, the brain puts both files back, recreates Caddy again so it is back on TCP, and tries again at the next start. Three things keep this inside the rules above:

- **It is a different line from the updater's.** host-agent writes the `moose-ui` image line; the brain adds one line to the `caddy` service. Each edit is a single line checked by a YAML parse, never a marshal.
- **It runs only at brain startup, next to the compose run the brain already owns** (# 3 step 3c), and before it. host-agent doing the edit was rejected: at boot it would race the brain's startup compose on the same project. The one remaining overlap is a revert that times out while a new brain is still in this step; both writes are atomic, but one could undo the other's line, and the next start repairs it either way.
- **It does not wait for the revert window.** The ledger keeps the previous brain for 7 days, and a revert to a brain from before #608 on a moved box gives a brain that dials TCP `:2019`, which Caddy no longer has. The maintainer accepted that: apps keep the routes Caddy holds, but new installs get no route and a stop or start does not flip one until the next control-plane update, whose brain reads the socket from `caddy.json`. Waiting would leave every recently updated box open to route rewrites from its apps for up to 7 days.

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
- **As built (#488), an admin can apply the target now.** `POST /api/v1/system/update-target/apply` (host-agent's `POST /v1/jobs/system-update-target`) takes no body: it applies the target the loop already fetched and checked, the same one the window would apply. Two rules about the night's one attempt: **a manual apply bypasses it**, because an admin who presses Update now after a failed night is choosing to retry and a failure puts the box back anyway; and **a manual apply counts as the attempt for the coming window**, so a failed update at noon is not tried again by the window that night (the night after tries again). The guard now has two writers, the tick and the host socket's handler, so the loop holds a lock across the check, the start and the mark (`Loop.ApplyNow`). It covers both streams, in the window's order (#509): stream B starts at once, and when the OS release the loop picked is installed in the other slot, the box then switches to it and restarts (`os-switch`, the window's switch without its two per-night guards; the trial guard stays, so nothing moves while the booted slot is on trial). With both in one click, the switch waits for stream B's job to free the lock: the loop retries it every 10 s for up to an hour, then leaves it to the window. A switch from Update now counts as the coming night's switch, so the window does not switch to a release that just reverted. The appliance refuses it, because it does not apply its own target (# 8.2, parked in #400).
- **As built (#488), the window follows the box's time zone.** The loop reads the zone from the `/etc/localtime` link on every tick. Before this it used Go's `time.Local`, which is loaded once when host-agent starts, so a zone set by first-run after that only took effect at the next reboot. The update-target read reports the zone as `zone`, so the dashboard names the same zone the loop uses.
- **As built (#488), the last control-plane update is kept on disk.** host-agent writes the outcome of every stream B update (good or failed, the step that broke, whether the box went back, the versions) to `last-update.json` beside the ledger and serves it as `last` on the update-target read. The job record lives in memory and the brain is replaced by the update, so this is what lets the dashboard show "Updated to X" or "the update failed, your box went back to Y" once the new brain answers.
- **Appliance learns but does not apply.** The seam is shared; # 8.2 is not. On appliance the control plane stays admin-prompted (# 3), so the loop stops at "a different control plane is available".
- **As built (#632), the read says when stream B is done for the night.** The `available` state looks the same whether an update is about to start, is running, or already failed tonight (one attempt per night), so the brain could not tell from it when its app updates may start (# 7). host-agent now computes `control_plane_done_night`: the night stream B is finished for (`YYYY-MM-DD`, the date the window opened on), or `""`. It names tonight when the window is open, the loop checked its source inside this window, and stream B will do nothing more tonight: there was nothing to do (`current`, `none`, `refused`, `unreachable`, `disabled`, or the appliance, which never applies in the window), or tonight's one attempt is over and its update job no longer runs. The running pair and the job are read when the request arrives, so a finished update is seen at once. A host-agent with the field always sends it, `""` included, so the brain can tell it from an older one. The same read carries `hold_until` while the brain holds the OS switch (# 7 Reboots).
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
4. **Stream A:** the A/B transaction of # 1, last in the window. **As built (#563)**, the answer carries an optional `os` list beside the brain and UI references: `"os": [{"version": "0.15.3", "bundle_url": "https://github.com/onmoose/os/releases/download/v0.15.3/moose-v0.15.3-amd64.raucb", "bundle_sha256": "<64 hex>"}, …]`, the newest patch of each minor from the oldest one still supported up to the target, which is last. Left out, the answer has no opinion about the OS and the box stays on its OS. The sha256 is the release's own `.raucb.sha256` asset. First proven on a provisioned hosted box on 2026-11-09, moving from 0.16.0 to 0.17.0 on the `cx23` type. A hosted VM reboot is cheaper than an appliance one: no user is physically waiting, and the window is ours.
5. The box reports the outcome back to the cloud: version now running, success or failure, and the failure mode if it rolled back.

Step 5 is what the whole design is for. On appliance our visibility into a bad release is "GitHub issues and the forum, hours to days" (# 3). On hosted it is a fleet view that tells us a version is failing before the second box tries it.

### 8.5 Deferred on hosted

- **Per-box targeting UI and cohort management** are cloud-side concerns (`onmoose/cloud`), not box-side. This doc specifies what the box asks for and obeys; how an operator sets it is the other repo's problem.
- **Auto-rollback triggered by the fleet**, rather than by the box's own health check. If three boxes fail the same target, the cloud should stop handing that target out. Sensible, cloud-side, not v1.

---

## Locked decisions

- **Two update streams, split by what the thing runs on:** stream A is **the box** (Debian base + kernel + firmware + `host-agent`): one atomic unit, reboots, realized as an A/B OS image (`DECISIONS.md` 2026-10-01). Stream B is **the containers** (brain, UI, apps, managed services) — per-image, no reboot, rollback by keeping the previous image. Flipped from the earlier five-stream model in `DECISIONS.md` 2026-08-11.
- **Two-track posture, modeled after Android:** silent auto-apply for the OS (every OS release, security or major, with an automatic revert) and for apps whose permissions do not grow; admin-prompted for anything that changes meaningful surface (the brain on appliance, app permissions).
- **The OS: an A/B image, built with RAUC and GRUB on both firmwares.** No `apt` on the update path. Downloaded ahead of the window, switched and rebooted inside it with no prompt on both profiles, marked good only once `host-agent` and the brain are healthy, reverted to the previous slot on its own otherwise. One attempt per target per window. The revert covers the OS, never the state partition (# 1).
- **`host-agent` ships inside the OS image** and has no update path of its own (# 2).
- **Two version lines:** moose `X.Y.Z` is the OS release; the control plane has its own semver. They meet only at the control plane's compatibility floor (`BUILD.md` # Versioning).
- **Control plane (`moose-brain` + `moose-ui`): release-manifest-driven, admin-prompted.** Manifest carries `brain`, `ui`, `minimum_host_agent`, and `rollback_to` (full schema + signing + publishing pipeline in `RELEASE_MANIFEST.md`). v1 ships a single `stable` channel; phased rollout and beta channel are deferred (additive when triggers fire — see `RELEASE_MANIFEST.md` # Future work). Telemetry is a halt-fast signal, not a rollout gate. Updater recreates only what changed; brain+UI revert as a pair on failure.
- **Apps: auto-update by default** (per `SPEC.md`); **prompt the instance owner only when the manifest's `permissions:` block expands** (new key or widened value). Permission-neutral updates of any size auto-apply. Different users on the same box may temporarily run different versions of the same app — by design, since instances are already per-user isolated. Tier-2 apps prompt the admin (box-wide).
- **Pre-update snapshot of `data_volumes` (plus `pg_dump` of any managed-service DB)** is taken before every app update. Restored on health-check failure alongside the image revert. Hooks remain deferred; the snapshot is the v1 safety net for app-driven schema migrations. Kept 7 days.
- **Permission-expansion prompt surfaces on next login of the instance owner**, modal on first dashboard load, two buttons (Allow & update / Keep current version). Accept applies immediately, not at 03:00. Admin sees per-user pending-update facts in Settings → Users; cannot accept on another user's behalf.
- **Update surfaces in the dashboard:** the app's settings page for the version, the update, a failure and the rollback affordance; a tile banner for a failed update only; auto-dismissing toast for overnight batches; no Settings → Updates page and no tile badge for an available version (`DECISIONS.md` 2026-11-07).
- **Managed services: brain-owned, no user toggle.** Patches in update window; cross-major migrations triggered transparently by app updates with a pre-migration backup.
- **Update window: 03:00–04:00 local** for apps, managed services, the OS image and its reboot. Configurable, advanced setting. Brain has no fixed window — it's admin-triggered.
- **Update ordering: stream B before stream A:** brain + UI → apps & managed services → the OS image (which reboots).
- **Reboots: only as part of an OS update, only in the window, with no prompt** (`DECISIONS.md` 2026-10-01).
- **Rollback: previous image + state snapshot kept for 7 days** for brain, apps, and managed-service patches. The OS rolls back to its previous slot.
- **All updates require internet; offline boxes stay current at their last-applied versions.**
- **Hosted: the target version is per-box, held by the cloud control plane** (# 8.1). No `stable.json`, no minisign, no hourly manifest poll — that path is appliance-only (`RELEASE_MANIFEST.md`). The box polls outbound; the cloud never connects in.
- **Hosted: updates are pushed, not prompted** (# 8.2). They apply in the window and the tenant admin is notified afterwards. **The app permission-expansion prompt is the one carve-out** — it still goes to the instance owner, because widening access to a user's data is their decision even on a box we operate.
- **Hosted and appliance share the apply/rollback mechanics** (# 8). Only the trigger differs. The signed-manifest trigger and the cloud-target trigger are two thin paths onto one transaction.
- **`host-agent` recreates both `moose-brain` and `moose-ui`** as one transaction (# 8.3). It writes the new image references first, so the brain reconciles to the same versions on restart instead of fighting them back. **Two files carry that declaration**, both in `MOOSE_CONTROL_PLANE_DIR`: the staged `compose.yml` holds the UI's reference (the brain reads it), and `images.json` — a ledger of the current pair, the previous pair, and when each was applied — holds the brain's (host-agent reads it at boot). The brain is not in the compose because a process cannot bring itself up; without the ledger, a box that lost its brain container would relaunch the reference baked at build time and silently undo an applied update. **The one exception to host-agent writing the compose:** the brain adds the Caddy socket-dir line once, at its own startup, on a box made before #608 (# 8.3, #612).

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
