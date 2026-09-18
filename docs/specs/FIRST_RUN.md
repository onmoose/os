# moose First-Run Experience

> Working spec for what happens between "user downloads the ISO" and "user is on the dashboard." Companion to `SPEC.md`, `CONTROL_PLANE.md`, `APP_MANIFEST.md`, `SERVICE_PROVISIONING.md`, `APP_ISOLATION.md`, `STORAGE.md`.

> **Environment profiles.** This doc describes the `appliance` profile (BYO box, USB install). In the **hosted** profile (moose-operated cloud VM) there is no installer — the box is provisioned from a cloud image and configured at first boot, and the wizard is trimmed to **time zone and telemetry consent** — the network, storage, and enrollment steps are gone (NIC from cloud metadata, no disk selection, enrollment done at provision), and so is the first-admin step: the founding admin is created by the portal sign-in handshake before the wizard runs (# Step 2, hosted note). See `ENVIRONMENT.md` # Provisioning & first-boot (hosted).

## Phases

First-run is three distinct surfaces:

1. **Installer** (boots from USB, runs once) — hardware checks, disk selection, recovery passphrase, install, reboot.
2. **Setup wizard** (first boot of the installed system) — network, first admin account, optional cloud mention, telemetry, time zone, done.
3. **Steady state** — user opens browser at `moose.local`, lands on dashboard.

Phase 0 (download ISO, flash to USB) is a website concern, not moose's runtime.

## Hardware floor

Published as "will it run moose?" at the install page. Hard requirements, checked by the installer before any disk action:

- **CPU:** x86-64, 2 cores
- **RAM:** 4 GB minimum, 8 GB recommended
- **Disk:** 64 GB minimum on the OS drive
- **UEFI** boot (legacy BIOS not supported)
- **TPM 2.0** present and enabled

The TPM requirement is non-negotiable — it backs encryption-at-rest auto-unlock (see `STORAGE.md`). Most x86 hardware from ~2016+ has TPM2 (Intel PTT / AMD fTPM, often firmware-based, free, but sometimes disabled in BIOS). The installer's hardware-check screen surfaces this clearly with a "check your BIOS for fTPM/PTT" link.

If any hard requirement fails the installer stops with a clear message. No partial installs.

## Phase 1 — Installer

Linear, mostly mechanical:

1. **Hardware check.** Pass/fail per item from the list above.
2. **Disk selection.**
   - User picks the **OS drive** (default: smallest non-removable disk, typically the NVMe).
   - If a second non-removable disk is present, user is offered "use this to expand your storage" — yes by default, picks the largest remaining disk. Single-drive boxes skip the question.
3. **Confirm wipe.** "These disks will be erased: /dev/nvme0n1, /dev/sda. Continue?"
4. **Install runs.** OS image copied, one LUKS recovery passphrase generated and silently stored at `/etc/moose/secrets/luks-recovery.key`, both drives LUKS-formatted with that passphrase, both TPM2-enrolled with `systemd-cryptenroll` against PCR 7, bootloader installed. The passphrase is **not displayed** — it lives on the box and is surfaced later from Settings → Storage → Advanced if needed. See `STORAGE.md` # Encryption posture for the reasoning.
5. **Reboot.** USB removed, box boots from disk, auto-unlocks via TPM.

No language picker (English-only v1). No license screen (TBD with legal but not blocking). No "create user" step — the wizard does that.

## Phase 2 — Setup wizard

First boot lands on a local web UI at `moose.local` (mDNS) or the box's LAN IP. The wizard is one-shot, a few steps, and disappears once done.

**Per-profile step set.** The steps below are tagged by where they run. **Appliance:** all of them. **Hosted:** Step 3 (time zone), Step 4 (telemetry), Step 6 (done); Step 1 (network) and Step 5 (secure URLs / enrollment) are gone (`ENVIRONMENT.md` # Provisioning), and **Step 2 (first admin, incl. 2a recovery) is gone too** — on hosted the admin already exists when the wizard loads, created by the portal sign-in handshake, so the shell starts at time zone (# Step 2, hosted note). The wizard renders from a data-driven step list keyed on the profile, so the shared shell serves both. The completion marker that ends the wizard is brain-side (`box_meta.first_run_complete`), set by Step 6 and read by the dashboard via `GET /auth/state` — the wizard runs until that marker is set, so an interrupted wizard resumes rather than dropping the user onto the dashboard.

### Step 1 — Network *(appliance only)*

The wizard branches on what the box has:

- **Ethernet link detected and DHCP succeeded:** the step is silent — connection name, IP, and gateway are shown for confirmation, "Continue" advances. No prompt; the common case is a no-op.
- **No ethernet carrier (cable unplugged or WiFi-only box):** the wizard runs a WiFi scan and presents an SSID picker. The user taps a network and enters the password. Hidden networks are reachable from a "Join other network…" affordance that takes SSID + password + security type. Enterprise WPA (802.1X) is deferred — out of scope for v1.
- **Ethernet present but the user wants WiFi anyway** (laptop near the router but they'd rather not run a cable): a secondary "Use WiFi instead" link from the ethernet-detected screen reveals the same SSID picker.

After WiFi password entry, the wizard tests the connection and waits for DHCP. On failure it surfaces a clear retry inline (`wrong password`, `couldn't reach DHCP`, `no signal`). On success the WiFi connection is saved (NM's connection store) and pinned as the **primary connection** (`BOOT.md` # NetworkManager).

**Ethernet is recommended, not required.** Discovery-based smart-home apps (`lan: true`, macvlan-attached — `APP_ISOLATION.md`) depend on reliable multicast, which most consumer APs rate-limit or filter; guest-network "client isolation" blocks peer discovery entirely; macvlan on WiFi is unreliable across drivers. The wizard surfaces this once, on the WiFi-confirmation screen: *"You're on WiFi. Most apps work fine — but smart-home apps that find devices on your network (Home Assistant, camera apps) work much better over an Ethernet cable. You can switch later from Settings → Network."* Not a blocker; just honest.

Other Step 1 details:

- **DHCP by default; static-IP option** for power users, available on both ethernet and WiFi connections (Settings → Network later, or expanded "Advanced" in the wizard).
- The detected primary interface is what `lan: true` apps will macvlan-attach to. On a WiFi-primary box, `lan: true` apps install but are flagged with a `discovery-on-wifi-degraded` warning in the dashboard (`HEALTH.md`-shaped, but per-app rather than global).
- **IPv6 opportunistic** — if the network provides it, NetworkManager picks it up; we don't ask.
- **Captive-portal networks** (hotels, dorms) are explicitly out of scope. The wizard can't navigate a captive portal page; the failure mode is a stuck "couldn't reach DHCP" with a hint to use a real home network.

**Cross-device note.** If the user is running the wizard from another device on a wired network and changes the box to a different WiFi, the wizard warns that the dashboard URL may move to a new IP, finishes the WiFi switch last, and surfaces the new LAN IP on the "done" screen so the user knows where to reconnect.

### Step 2 — First admin

> **Hosted.** On the appliance, the trust that lets *this* person create the founding admin is physical presence at the box during first boot. A hosted cloud VM has no such gate, and **this whole step does not run there**. The owner already has a portal account at `mooseos.com`, so the portal signs a short-lived ownership assertion and sends the owner's browser to the box; the box checks it against the key in its provisioning seed and creates the founding admin from it, with a username derived from the owner's email and a random password the owner never sees (`ENVIRONMENT.md` # Owner sign-in & seed ingestion — as built). There is no name-and-password form, no recovery code (the portal account is the way back in), and no hosted `/setup` — `POST /setup` returns 403 on hosted, and an unauthenticated visitor is sent to the portal instead of a setup page. Until the box has ingested a seed it has no key, and sign-in returns "not provisioned".

- Two fields: **first name** + **password**. That's it.
- The first user created is automatically an admin. Admins can create more users (admins or members) later in Settings. Admins are added to the Linux `sudo` group (rescue path when the dashboard is broken); members are unprivileged. See `USERS_AND_GROUPS.md`.
- No username field — the display name is what the user types and what the UI shows everywhere. See "Identity & display names" below for how this maps to the underlying OS user.
- The password set here is the user's **moose password** — used for the dashboard, and for SSH and SMB file shares if/when those are enabled per-account from Settings. One credential across all surfaces; what changes per-protocol is whether the account is allowed to use it. See `AUTH.md` # Device access (SSH + SMB).

#### Step 2a — Save a recovery code (recommended)

Immediately after the password is set, the wizard surfaces a recovery-code step. The toggle is **on by default**.

> ☑ **Save a recovery code** (Recommended)
>
> *If you forget your dashboard password, this code is the only way back in. Without it, you'd need to reinstall and restore from backup. Take a photo of the code with your phone — it'll back up automatically to your photos, and you'll have it when you need it.*

If the user proceeds: the brain generates a 16-character code formatted as `XXXX-XXXX-XXXX-XXXX`, shows it once full-screen with a copy button, and requires an "I have saved this" checkbox before moving on. The code is hashed (argon2id) in the brain's SQLite and is single-use; plaintext is never persisted.

If the user toggles off: an explicit confirmation appears — *"You won't be able to recover your account if you forget your password. Continue without a recovery code?"* — forcing acknowledgment of the tradeoff.

The recovery code is **separate from the LUKS recovery passphrase** (`STORAGE.md` # Encryption posture). The LUKS passphrase is generated silently and stored on the box; the dashboard recovery code is the only thing the user is asked to save at first-run. Don't combine — see `AUTH.md` for the threat-model reasoning.

**Redemption flow** (when the user later logs in via the recovery code): the brain validates the code, then forces a set-new-password screen (no skip), then generates and shows a fresh recovery code once (single-use semantics — the old code is consumed). Full mechanics in `AUTH.md` # Using the recovery code.

### Step 3 — Time zone

- **Auto-detected via IP geolocation.** No prompt if successful.
- If the box has no internet at first-run, the wizard falls back to asking the user to pick from a list.
- Always overridable from Settings later.

Full time / NTP model in `TIME.md`. NTP itself (chrony with NTS sources) is up by the time the wizard runs — no user-facing NTP configuration at first-run.

### Step 4 — Telemetry

- One unchecked checkbox: *"Send anonymous usage statistics and crash reports to help improve moose."*
- Off by default. Plain language. No dark patterns.
- Inline "What does this collect?" disclosure expands to show the field allowlist.
- Toggleable from Settings → Privacy later — an admin-only Box panel, since the toggle is box-wide (`SETTINGS.md` # panel inventory). The first-run prompt is the founding admin making that box-level choice once.

Full spec: `TELEMETRY.md`. The one toggle covers both the usage stream and the crash stream; both go to `telemetry.onmoose.io` (a moose-controlled endpoint, not a third-party SaaS).

### Step 5 — Secure URLs & enrollment (optional) *(appliance only)*

This step is two coupled choices presented as one. Turning on **"Use secure (HTTPS) URLs for my apps"** requires enrolling the box with onmoose.io (to get the subdomain + Let's Encrypt cert). They're the same decision for a new user, so the wizard frames them together.

**Framing on the screen:**

> *"Use secure (HTTPS) URLs for your apps?"*
>
> *Some apps need HTTPS to work fully — cameras, password managers, app-like installs on your phone. We'll give your moose a name like `cindy-zx9.onmoose.io` and a real certificate, so your apps are reachable at HTTPS URLs on your home network. Your data never leaves your box; only DNS lookups go through our servers.*
>
> *Tip: **if anyone in your household uses an Android phone, you'll want this on.** Android can't open the default `.local` URLs from a browser; the secure URLs work everywhere.*
>
> *You can also do this later from Settings.*

The Android line is not decorative — it's the load-bearing reason this toggle exists for many households. See `DISCOVERY.md` for the full client-compatibility picture; the short version is that Android does not wire mDNS into the system resolver and there is no workaround at moose's layer.

Two buttons: **"Yes, set it up"** and **"Skip for now"**.

If the user proceeds:

1. Wizard shows a "Name your moose" field with a suggested name (e.g. `cindy-zx9`). Editable; user can accept the suggestion or type their own (e.g. `the-perez-family`).
2. Availability is checked live against the enrollment API. On collision, the wizard offers alternatives or invites the user to try again. Reserved names (single dictionary words, moose-internal slugs) are rejected with a clear message.
3. On confirm: box is enrolled, the API token is persisted, cert issuance starts in the background, and the **"Use secure URLs" toggle is set to ON**. The wizard moves on.

The name is **frozen at this step** for the life of the install — changing it later requires re-enrollment, which decommissions the old subdomain. See `MOOSE_NETWORK.md` for the rationale ("Locked: pick the name at enrollment, no rename afterward").

If the user skips:

- mDNS publishing for `.local` still happens; dashboard, all apps, and SSH work normally over HTTP.
- The "Use secure URLs" toggle in Settings → Network appears disabled, with a one-click "Enroll your box to enable" affordance next to it.
- Apps that declare `needs_secure_context: true` (camera/mic/PWA-dependent apps; see `APP_MANIFEST.md`) can still be installed but show a warning that some features may not work without HTTPS.
- The user can run this same flow later from Settings → Network at any time.

This step replaces the older "cloud features mention" placeholder. It is still **not** a moose account sign-in — there is no moose cloud account at v1. The enrollment is per-box, anonymous beyond the box-id, and revocable.

> History note: an earlier version of this wizard step presented `.onmoose.io` as an always-on "secure URL channel" exposed per-app via a `requires_https` manifest flag. Replaced 2026-05-14 by the global-toggle model. See `DECISIONS.md`.

### Step 6 — Done

Land on dashboard. Wizard is over.

## Identity & display names

### What the user sees

The user types a first name and a password. The first name is shown everywhere — login screen, dashboard, "Cindy installed Photos," sharing UIs, audit log. There is no "username" concept exposed.

### What the system stores

The display name is slugified to a stable Linux user ID:

1. Transliterate to ASCII (`José` → `jose`, `李` → `li`).
2. Lowercase, strip to `[a-z0-9]`, collapse runs.
3. Empty result falls back to `user`.
4. Check against a reserved-slug list: `root`, `admin`, `daemon`, `postgres`, `redis`, `mysql`, `nobody`, `www-data`, `sshd`, `systemd*`, `moose`, plus standard system. On hit, append `1`, `2`, ... until free.
5. Display-name uniqueness is enforced at creation, so collisions in step 4 are the rare path.

The `[a-z0-9]`-strip-and-collapse in step 2 already guarantees the two reservations the `<slug>--<user>` personal-instance scheme depends on (`DASHBOARD.md` # instance naming): a username slug can never contain `--` (runs collapse to a single `-`) nor start with `xn--`. The brain also enforces both as an explicit guard at the user-creation boundary.

UID assigned from the moose-reserved range (3000+). Home directory: `/home/<slug>/`. Use-case folders created at account setup: `Photos/`, `Documents/`, `Movies/`, `Music/`, `Notes/`, `Downloads/` (`STORAGE.md` # What apps and users actually see).

### Why this shape

- **No prefix on the slug.** `cindy`, not `moose-cindy`. SSH (`ssh cindy@box.local`), paths (`/home/cindy/Photos/`), and Samba usernames stay clean. Collision risk is real but small, and the reserved list + UID-range filtering covers it without per-day verbosity cost.
- **Display name is mutable, slug is stable.** A user renames "Cindy" → "Cynthia" — display flips, slug stays `cindy`, paths and ownership untouched. Renaming a Linux user is a destructive operation we don't expose.
- **Display-name uniqueness enforced.** Two "Cindys" on one box is rejected at admin-create time with a "use Cindy K. or Cindy 2" prompt. Edge case for the 1–2-user installs we expect.

### Roles

Two roles in v1:

| | Admin | Member |
|---|---|---|
| Manage users | ✅ create, promote, demote, delete | ❌ |
| Configure box | ✅ network, storage, telemetry, etc. | ❌ |
| Install Tier-2 apps (Tailscale, SMB, DLNA) | ✅ | ❌ |
| Install Tier-3 apps | ✅ | ✅ — for themselves |
| Use Tier-3 apps | ✅ | ✅ — their own per-user instance |

The Tier-3 install permission is per-user: when a member installs Photos, their per-user Photos instance spins up against their `~/Photos/`. Other users are unaffected. They can also install Photos for themselves (separate instance, separate data) or not.

Kid-account restrictions (limit which apps a member can install, content filters, time-of-day rules) are explicitly **deferred** — useful for the family-box scenario, not v1.

## Phase 3 — Steady state

Once the wizard is done the box is fully usable. The dashboard is the user's primary surface; the wizard never reappears.

What's *on* the dashboard at first arrival is an open question (see below) — empty, suggested apps, or a starter bundle.

### Reaching the dashboard from other devices

`moose.local` resolves out of the box on macOS, iOS, and Linux (with `nss-mdns`, almost universal). Two cases need help:

- **Windows clients** need Apple's Bonjour service. Most Windows 10/11 installs do not have it. The "Add another device" / share-link surface in the dashboard detects a Windows User-Agent visiting for the first time and links to the Bonjour Print Services installer with a one-line explanation. If the household is using the secure-URL path, this is moot — `cindy-zx9.onmoose.io` resolves via public DNS on every OS.
- **Android browsers** do not resolve `.local` at all (see Step 5 above and `DISCOVERY.md`). The only path that works is the secure-URL scheme; the same share surface surfaces this for Android visitors when secure URLs are off.

## What v1 does not include

- Cloud account / sign-in. Box is local-only at v1; cloud features mentioned but not installed.
- Multi-user role hierarchy beyond admin/member. Kid accounts, app-level visibility, scheduled access — all deferred.
- App-level cross-user sharing primitives. v1 sharing is "drop it in `shared/`" only.
- Email on file for users. No "forgot password" flow at v1; admins reset member passwords manually.
- Avatars / profile pictures. Cosmetic; deferred.
- Multi-language UI. English only.
- License acceptance screen. Pending legal, not blocking the design.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
