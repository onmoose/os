# moose Authentication & Sessions

> How the moose dashboard authenticates users, how sessions are managed, and how admin vs. member roles are enforced. Companion to `FIRST_RUN.md`, `MOOSE_NETWORK.md`, `CONTROL_PLANE.md`, `SERVICE_PROVISIONING.md`.

> **Environment profiles.** The dashboard auth/session model here is Layer 1 — identical across profiles, and identity stays PAM-sourced in both. What differs in the **hosted** profile: SSH and Samba (the appliance's other PAM consumers) are absent, and the exposure posture is public-by-default / auth-gated. See `ENVIRONMENT.md` # Access & files (hosted).

## Scope

The moose session governs **moose's own surfaces only**:

- The dashboard (install apps, see system status, browse the catalog).
- Settings (network, storage, users, telemetry).
- Tier-2 admin UIs (Tailscale, Samba, DLNA) — served *inside* the dashboard at `/settings/<service>/*`. Same origin as the dashboard, same session.

The moose session does **not** govern:

- **Tier-3 apps** (`photos.local`, etc.). Each app has its own auth — explicit no-SSO call (`SPEC.md` # Accounts & users). Subdomain isolation is load-bearing for security; the moose cookie is scoped to the dashboard host and never reaches app subdomains.
- **Device access (SSH + SMB).** Linux PAM + Samba directly. These services authenticate against the same password the user uses for the dashboard (PAM is the source of truth), but the brain's session cookie does not apply to them. Each protocol is opt-in per user — see "Device access (SSH + SMB)" below.

## Identity primitive: password

**One factor in v1: a password.** No passkeys, no TOTP, no email-based recovery (we have no email on file). Password is the floor; everything else is layered later without breaking changes.

**Why not passkeys in v1:**

- Passkeys are origin-bound by design. A passkey on `moose.local` doesn't work on `cindy-zx9.onmoose.io`. With the toggle that flips schemes, users would re-enroll per origin — terrible UX.
- No email = no fallback recovery for a lost passkey. Password recovery still has to exist anyway.
- WebAuthn ceremony + attestation + recovery flows is real complexity for a v1 audience that's tinkerers-then-households.

**Password storage: PAM is the source of truth.** The password lives in `/etc/shadow` (hashed via yescrypt or `pam_argon2`, whichever Debian ships configured), managed by Linux PAM. The brain does not store a password hash in its SQLite — it asks host-agent to verify on every login attempt.

**Why PAM, not brain SQLite:** the same password authenticates the dashboard, SSH, and SMB. PAM is the only credential store all three services can read uniformly. Keeping a copy in brain SQLite would create two sources of truth and a sync problem; routing dashboard verification through PAM keeps it to one. The per-login round-trip is brain → host-agent UNIX socket → PAM `authenticate()` — sub-millisecond, dwarfed by argon2id/yescrypt's deliberate cost.

The host-agent exposes a `verify_password(user, password)` endpoint to the brain. See `BRAIN_HOST_PROTOCOL.md` for the wire surface. The brain still owns sessions, roles, recovery codes — everything except the credential itself.

**Password rules:** minimum 8 characters, no upper bound, no composition rules. Surface the haveibeenpwned k-anonymity API check as a non-blocking warning ("this password has appeared in known breaches") if we have internet during account creation. Don't enforce.

## Sessions: server-side opaque cookie

The brain has a `sessions` table in SQLite. Login mints a row, returns a 256-bit random session ID as a cookie. The cookie is meaningless on its own — the brain looks it up server-side on every authenticated request.

**Cookie shape:**

| Attribute   | Value                                                                 |
|-------------|-----------------------------------------------------------------------|
| Name        | `moose_session`                                                       |
| Value       | 256 bits of CSPRNG entropy, base64url-encoded                         |
| `HttpOnly`  | yes                                                                   |
| `Secure`    | yes on `.onmoose.io`, no on `moose.local` (HTTP-only there)        |
| `SameSite`  | `Lax`                                                                 |
| `Domain`    | *unset* — scoped to the exact host (`moose.local`). Critical: do NOT set a broader `Domain` such as `.local`, which would leak the session to every `.local` host on the LAN — all app origins (`<slug>.local`) and any other `.local` device — defeating origin isolation. Apps are single-label siblings (`<slug>.local`), not subdomains of `moose.local`, so host-scoping keeps the dashboard session off every app origin. |
| `Path`      | `/`                                                                   |

**Why opaque cookies over JWTs:** JWTs win when multiple services need to verify without a roundtrip. We have one backend (the brain). JWTs would just give us non-revocable tokens with bigger payloads. Opaque cookies give us instant server-side revocation (logout, password change, "sign out everywhere"), tiny client cookies, and no JWT-key rotation theater. The DB hit per request is negligible at home-server scale.

**Sessions table:**

```
sessions:
  id              text primary key
  user_id         integer not null
  created_at      timestamp
  expires_at      timestamp     -- absolute hard cap, see "Lifetime"
  last_seen_at    timestamp     -- refreshed on each request
  client_hint     text          -- user agent string, for the "active sessions" UI
  origin          text          -- "local" or "network", informational only
```

**Lifetime:**

- **30-day rolling.** `last_seen_at` updates on every authenticated request; the brain treats the session as valid for 30 days past `last_seen_at`.
- **90-day hard cap** via `expires_at`. Even if the user is constantly active, force re-login at 90 days.
- **Rationale:** home server you barely log into. A 24-hour expiry would feel like Synology — annoying. Browsers and password managers handle a real password gracefully; the long rolling window keeps day-to-day friction at zero.

**Invalidation:**

- Logout: delete the session row.
- "Sign out everywhere" in Settings: delete all rows for the user.
- Password change: delete all rows for the user, force fresh login.
- Admin force-logout member: delete that user's rows.

## Cross-origin behavior (the toggle)

The cookie is scoped to the exact host. So:

- On `moose.local`, the user has a `moose_session` cookie for `moose.local`.
- On `cindy-zx9.onmoose.io`, they have a separate `moose_session` cookie for `cindy-zx9.onmoose.io`. Different cookie, different session row server-side.

When the user flips the "Use secure URLs" toggle in Settings → Network (see `MOOSE_NETWORK.md`):

1. Brain marks the current session's `origin` as the *old* one.
2. Brain proactively expires all sessions whose `origin` matches the off-mode (so a stale `.local` session doesn't linger after the user committed to `.network`).
3. User is redirected to the new origin's login screen and re-authenticates.

This is the **only** routine cross-origin transition in normal use. Re-auth here is accepted (`DECISIONS.md` 2026-05-14). Browsers and password managers handle it; the user types their password once.

## Login screen UX

**User-list style, like macOS / Plex / Synology DSM.**

The login page lists every account on the box — first name + colored letter glyph (avatars are deferred per `FIRST_RUN.md`). Click your name → password field appears → submit.

The payload behind it (`GET /api/v1/auth/users`) carries both names per account: the display name, which is the only one the screen renders, and the account name, which is what the browser posts back to `POST /api/v1/login`. `/login` still authenticates on the account name, so nothing about it changes and the person never has to know theirs. The glyph colour is keyed on the account name, which is stable, so a rename does not change the colour somebody recognises.

**Why a user list, not a username field:**

- Household device. 1–4 users. Set is small and known to people physically near the box.
- "Enter your username" is friction. Users remember first names; they forget exact slugs.
- Matches consumer multi-user OS UX (macOS login, Plex profile picker).

**Tradeoff:** anyone who reaches the dashboard URL sees the user list. Acceptable in the household trust model — the security boundary is "you're authenticated to moose," not "you don't know who lives here." Tinkerers who want stricter posture can flip a Settings toggle to switch to a blank username + password form.

**Appliance only.** That tradeoff assumes the reader is already on the LAN or the mesh, which is what the appliance's network posture guarantees. A hosted box has no such perimeter — it answers on the public internet at `<box-id>.onmoose.io` (`ENVIRONMENT.md` # Networking & discovery) — so the same list is a tenant roster any scanner can read, and the box-id labels needed to find it are already public in certificate transparency logs. On hosted the picker's source, `GET /api/v1/auth/users`, returns **404**, the same way `/setup` is disabled there. Nothing is lost: a hosted box never renders this screen. An unauthenticated visitor is redirected to the portal and bootstraps through the portal-to-box SSO handshake (`ENVIRONMENT.md` # Access & files, box side in `internal/api/sso.go`), so the dashboard never asks for the list. The refusal is 404 rather than 403 because the route does not exist on that profile, mirroring how the SSO landing hides itself on the appliance.

## Rate limiting

- **Per-username:** exponential backoff after failed attempts. 3 fails → 1s; 5 → 10s; 10 → 60s; 20 → account temporarily locked for 15 minutes.
- **Per-IP:** simple token bucket, 10 attempts per minute. Logs the IP but doesn't ban — most boxes only see LAN IPs.
- **Admin override:** an admin can clear a lock from the user-management UI ("Cindy is locked out, unlock her").
- All failed attempts logged. Audit log surfacing comes later.

**Which IP.** "Per-IP" means the address the box can actually attribute the request to, never one the caller chose for itself. The brain reads `X-Forwarded-For` only when the request's immediate peer is a **trusted proxy** — in moose's topology that is the box's own Caddy, which the brain container is reachable from and effectively nothing else — and then takes the **last untrusted hop** in the chain, not the first. From any other peer the header is ignored and the peer address is the key. Caddy is configured with an empty trusted-proxy set (it is the edge: its clients are LAN devices, and on hosted the open internet), so it replaces an inbound `X-Forwarded-For` with the address it actually saw rather than trusting the client's. Both halves are needed: without the Caddy half a forged chain reaches the brain intact; without the brain half the brain reads the wrong end of it. Reading the first hop is only ever correct if every proxy in the chain is trusted to have sanitized it, and on the login path getting this wrong means an attacker rotating a header gets a fresh 10-attempts-per-minute budget per request — the throttle stops existing for exactly the adversary it is for.

This section governs the **login path only** — the deliberately-expensive PAM round-trip is the asset it protects. *General* request throttling for the rest of the public API (per-session and per-IP request-rate buckets, the `429`/`Retry-After` contract, SSE-stream concurrency) lives in `BRAIN_UI_PROTOCOL.md` # Rate limiting & abuse. The two don't overlap: `/login` keeps its stricter per-username backoff here; the general per-IP plane there is the backstop for the *other* unauthenticated routes.

## CSRF

`SameSite=Lax` blocks cross-site POSTs to the dashboard origin. For v1 that's sufficient — we don't accept GET requests for state-changing operations.

When we add an API for third-party tools (NEXT.md Tier 1), we'll either issue an explicit CSRF token at login and require it as a header on writes, or require Origin/Referer checks. Out of scope until then.

## Roles

Two roles in v1 (`FIRST_RUN.md` # Identity):

| Capability                                  | Admin | Member |
|---------------------------------------------|:-----:|:------:|
| Manage users (create/promote/demote/delete) | ✅    | ❌     |
| Configure box (network, storage, telemetry) | ✅    | ❌     |
| Install Tier-2 apps                         | ✅    | ❌     |
| Access Tier-2 admin UIs                     | ✅    | ❌     |
| Install Tier-3 apps as **household** (shared, admin-owned) | ✅ | ❌ |
| Install Tier-3 apps as **personal** (per-user instance)    | ✅ | ✅ |
| Use Tier-3 apps                             | ✅    | ✅ (own + permitted household) |

App instances are **owner-scoped** (`DASHBOARD.md` # the apps model): an admin elects household vs. personal at install; a member can only create personal instances they own. Duplicate installs warn but don't block. This is an authorization rule, enforced server-side like the rest of this table.

**Enforcement:** role is checked **server-side in the brain** on every authenticated request. The UI also hides admin-only sections from members — defense in depth, not the security boundary.

Routes are grouped by role at the router level (e.g., `/api/admin/*` requires admin; `/api/me/*` requires any authenticated user). Hard to introduce a bypass by missing a check on one handler.

**On top of the role check, destructive Settings operations re-prompt for the password** with a 5-minute elevation window per session (sudo-in-UI pattern). Full mechanics in `USERS_AND_GROUPS.md` # Elevation in the UI.

**On a hosted box the owner confirms through the portal, not with a password** (issue #469, as built). The portal signs the owner in and the box gives their PAM account a random password that is generated and thrown away (`ENVIRONMENT.md` # Owner sign-in & seed ingestion), so there is no password for them to re-type and every elevation-class action was unreachable. Their confirm step is a second portal round-trip instead: the dashboard mints a one-time challenge (`POST /api/v1/auth/elevate/challenge`, hosted-only), sends the browser to the portal's open-box route with the current page as the return path and the challenge inside it, and the portal comes back to the box's SSO landing with a fresh ownership assertion. The landing verifies the assertion, spends the challenge, and marks the session elevated for the same five minutes a password confirm buys. Three rules make that safe, and each is enforced box-side:

- **No challenge, no elevation.** A plain sign-in landing elevates nothing. The portal's open-box route is a GET with a `SameSite=Lax` cookie, so a cross-site page can drive that navigation; without this rule it could silently arm a privileged window on the victim's own box. The challenge is the part such a page cannot supply, and the reason is worth stating precisely, because the obvious version of it is wrong. **The protection is that a cross-origin page cannot read the reply, not that it cannot send the request.** `POST /api/v1/auth/elevate/challenge` takes no body and requires no JSON content type, so it is a *simple* request in CORS terms: a same-site app page can send it with the owner's cookie and the brain will mint a challenge. What the page never gets is the response. The brain serves no CORS headers at all (`internal/api` # Handler), so the browser refuses to hand the body back, and a challenge nobody can read is inert — it is single-use, bound to its user, and expires in five minutes unspent. This matters most on hosted, where apps are `<slug>.<box-id>.onmoose.io` and the dashboard is `<box-id>.onmoose.io`. `onmoose.io` is not on the Public Suffix List (`ENVIRONMENT.md` # Certs, where the same fact sets the Let's Encrypt budget), so those hosts are **same-site** and `SameSite=Lax` does not stop a fetch between them. A reflected `Origin` with `Access-Control-Allow-Credentials` would hand the app the challenge value and with it the owner's elevation, which is exactly the boundary this rule exists to hold. Issue #475 fixed a layer that did reflect one.
- **The challenge is single-use and short-lived**, bound to the user it was minted for, and spent before the session is elevated. A replayed return URL signs the owner in and opens nothing.
- **The return path must be a relative path on this box.** The landing hands out a live session, so an open redirect there would hand it to someone else's page. Anything with a scheme, a host, a backslash, or a leading `//` is refused and the owner lands on the box's front page.

Elevation lives on the box session only. The Domain-scoped forward-auth cookie (`ENVIRONMENT.md` # Public-by-default) proves a live session to app subdomains and carries no elevation of its own. The appliance is untouched: the password prompt stays the only confirm step there, and a box user the hosted owner creates does have a password, so they keep the prompt too.

**Enrollment-class operations bypass the elevation window.** Add-drive and eject-drive (`STORAGE.md` # Adding a data drive, # Ejecting a data drive) require a fresh password prompt every time, regardless of recent elevation. These operations extend the box's LUKS keyslot set or remove a physically-attached drive; they're rare, deliberate, and not safely batched. The 5-minute window covers user-management batch work, not enrollment.

**Roles map to Linux groups on the host.** Members are unprivileged Linux users; admins are in the `sudo` group and can `sudo` over SSH for rescue work. See `USERS_AND_GROUPS.md` for the full posture, group reference table, and rescue path.

## Tier-2 admin surface lives in the dashboard

Critical architectural decision — separate doc-section because it shapes auth heavily.

Tier-2 apps (Tailscale, Samba, DLNA) install as **native Debian packages under systemd**, not Docker containers. Their admin UIs are **not exposed at their own subdomain**. Instead, the moose dashboard surfaces a hand-curated UI for each Tier-2 service at `/settings/<service>/*` (e.g., `/settings/tailscale`, `/settings/shares`).

The brain edits config files (`/etc/samba/smb.conf`) and toggles systemd units (`systemctl restart smbd`) via host-agent. The user never sees the upstream admin UI; they see moose's UI talking about the same underlying knobs.

**Why this collapses the auth problem:** Tier-2 routes are same-origin as the dashboard. The `moose_session` cookie just works. No forward-auth, no per-app subdomain, no embedded iframes, no Authelia-style central-login redirect dance.

**Tier-2 vs. Tier-3 in one sentence:** Tier-2 is *moose's UI for things it manages on the host*; Tier-3 is *third-party apps moose runs in containers with their own UIs at their own subdomains*. Different shapes, different auth stories.

See `SERVICE_PROVISIONING.md` for the full Tier-2 architecture.

## Password lifecycle

### Setting a password

- **First admin:** Step 2 of first-run sets it (`FIRST_RUN.md`).
- **New member:** admin creates the account from Settings → Users, sets a temporary password, communicates it to the member out of band (verbal, messenger). On first login, the member is forced to change it before they can do anything else.
- **Self-service change:** `Settings → My account → Change password`. Requires current password.

### Forgetting a password

- **Member forgets:** an admin resets it from Settings → Users → Reset password. Generates a temporary; member is forced to change at next login. No email needed — this is a household device, members are physically reachable.
- **Admin forgets:** the recovery code path (below). If there are multiple admins, another admin can also reset.

### The recovery code (admin-only, opt-in)

Admins can opt into a one-time recovery code at account creation. The toggle is **on by default** and labelled as recommended.

**First-run framing (Step 2 of `FIRST_RUN.md`):**

After the admin sets their password, the wizard shows:

> ☑ **Save a recovery code** (Recommended)
>
> *If you forget your dashboard password, this code is the only way back in. Without it, you'd need to reinstall and restore from backup. Take a photo of the code with your phone — it'll back up automatically to your photos, and you'll have it when you need it.*

If the user proceeds, the brain generates a recovery code (24 hexadecimal characters — 12 random bytes, shown raw with no separator mask) and displays it once full-screen with a copy button and explicit "I have saved this" checkbox. The UI shows the code verbatim rather than a `XXXX-XXXX`-style mask: the real code is a continuous hex string, so a mask would reject a pasted code. Hash stored in the brain's SQLite, on the user row. Plaintext is **never persisted** — show-once is the floor.

If the user toggles it off, an explicit confirmation: *"You won't be able to recover your account if you forget your password. Continue without a recovery code?"* Forces acknowledgment of the tradeoff.

**Same flow runs when an admin is added later** (admin creates a second admin account; the second admin sees the recovery-code step on first login).

### Using the recovery code

Login screen has a "Forgot password" link. It asks for the recovery code; the brain validates it against the stored hash in SQLite. On match, the brain serves a **forced** "set new password" screen — no skip, no "I'll do this later." The new password is sent to host-agent → `passwd <user>` + `smbpasswd -a` sync → PAM accepts the new password for dashboard, SSH, and SMB. The brain then invalidates all existing sessions for that user and consumes the old recovery code.

Because the user now has no recovery code (single-use semantics), the next screen **generates and displays a fresh recovery code once**, with the same "I have saved this" checkbox as first-run. Reissue is **mandatory on the recovery path** — unlike first-run, there is no opt-out toggle here: `POST /api/v1/recover` always returns a fresh code, and for the non-technical target audience "recovery stays on" is the safer default (a user can never exit the recovery flow with no way back in). A user who genuinely wants no recovery code declines at first-run, not here.

**Order-of-operations rule:** the brain checks host-agent reachability *before* consuming the recovery code. If host-agent is unreachable (rare — both run on the same box), the password change can't be applied, so the recovery code must survive. Otherwise a single-use code burns without effect.

The user lands on the dashboard with a fresh password and a fresh recovery code. From here, every downstream feature that requires an admin password — including add-drive and eject-drive — works normally; there is no "logged in via recovery code, now wants to do X" carve-out because the recovery-code login terminates in a real password by construction.

### Threat model

- **Lost code, forgotten password = no recovery.** Same as LUKS recovery passphrase semantics. Honest.
- **Phone-photo of code lands in iCloud/Google Photos.** Worth being explicit about in the privacy doc. Threat trade is "I forget my password" (likely) vs. "cloud photo backup is breached AND attacker correlates it to my moose box AND reaches my box on LAN" (extremely unlikely). For the household audience, convenience wins. Tinkerers who care write it down instead.
- **No physical-access reset.** Box gets stolen → TPM auto-unlocks LUKS → if "physical access = admin reset" were a path, the thief would become admin of a now-decrypted system. Rejected for this reason — see `DECISIONS.md` 2026-05-14.

### Separate from LUKS recovery passphrase

The LUKS recovery passphrase (shown at install, see `STORAGE.md`) recovers **disk decryption** when the TPM seal breaks (motherboard swap, firmware update). The dashboard recovery code recovers **account access**. Different things, different moments. Don't combine them onto one sheet — conflating them confuses threat models.

## Device access (SSH + SMB)

**One password for everything.** Dashboard, SSH, and SMB all authenticate against the same Linux account password — the one the user set at account creation, stored in PAM (`/etc/shadow`). Setting up SSH or mounting an SMB share uses the password the user already knows. **On hosted, SSH is the exception**: the password is not enough there on its own, and a public key is required — see the profile table below.

**What's per-protocol is the *access*, not the *credential*.** The password is set when the account is created; what changes when the user opts in is which services accept that password for that account.

**The mandatory factor is set by the profile.** SSH is the one place the one-password rule does not travel, and `DECISIONS.md` 2026-09-09 records why: that rule was a decision about a LAN, and hosted has none.

| Profile | Mandatory | Optional second factor | Refused |
|---|---|---|---|
| Appliance | The moose password | A public key | — |
| Hosted | A public key | The moose password | Enabling with no key |

The optional factor is a **second lock, never a second door**. Choosing it renders `AuthenticationMethods publickey,password` for that account, so sshd demands both and neither alone authenticates. Offering the other factor as an *alternative* would set the account's security by its weaker branch, which on hosted would discard the whole point of requiring a key.

**Both rows are enforced in the brain, not in host-agent and not in the UI.** host-agent renders what it is told and deliberately does not know the profile, so it cannot be the place that decides which factor is mandatory. The brain refuses a hosted enable that has no key, and resolves the appliance's password to required no matter what the caller sent — so adding a key on the appliance adds a lock rather than swapping one out. Issue #477 fixed the appliance half, which the table described and nothing enforced.

**Losing a key is not a lockout.** The dashboard is reached through the portal on hosted and through the login screen on the appliance, never through SSH. A user who loses their key signs in as usual and pastes a new one. That is what makes the strict hosted posture affordable for a non-technical owner.

**Default posture: nothing is listening.** Samba is enabled at boot with an empty `valid users`. **sshd is not running at all** until an account opts in:

- `sshd_config.d/moose-allowed.conf` is rendered from the enabled set — a global `AllowUsers` plus one `Match User` block per account carrying that account's `AuthenticationMethods`. Empty at install.
- sshd is **started when the first account enables SSH and stopped when the last one disables it**, so a box nobody uses SSH on has no open port rather than an open port that refuses (`BUILD.md` # SSH, and `DECISIONS.md` 2026-09-09 for why this replaces daemon-on-but-no-account).
- `smb.conf` carries a `valid users` directive per share. Empty at install — Samba rejects every account.

**Flow (per protocol):**

The SSH screen is a draft with Cancel and Save (#494). Steps 1 to 3 change the draft only. Nothing reaches the box until Save.

1. Settings → SSH → switch on "Enable SSH for my account." (The screen carries SSH alone until SMB has an API; `SETTINGS.md` # panel inventory has the naming.) SMB gets "Enable file shares (SMB)" on the same screen when it ships.
2. Add a public key. Required on hosted, optional on the appliance. The user can **upload a `.pub` file or paste the text**; both reach the same validation. Several keys per account is normal: a laptop and a desktop. A pasted **private** key is refused in plain English and never stored, not even in the browser's copy of the draft. A key the user removes stays in the list, greyed and marked "will be removed when you save", with Undo.
3. Optionally turn on the second factor, described to the user as an extra lock rather than another way in. The card is called "Password" and never "SSH password": it is the one password the account already has.
4. Save. The screen sends the **whole desired state** in one `PUT /api/v1/me/ssh`: the on/off flag, the password choice, and the complete key set, with kept keys named by id and new keys carrying their text. The brain diffs that against what it holds and writes the difference in one transaction. The profile's guards apply to the **state the Save ends in**, not to each step on the way, so "remove my only key, add its replacement" is one valid Save on hosted.
5. Confirm dashboard password (re-auth gate, prevents stolen-session abuse). Asked once per Save, not once per control. The hosted box owner has no box password, so their confirm is a portal round-trip; the screen keeps its draft in `sessionStorage` across that trip (and across a reload), keyed by user id, and shows "Unsaved changes restored." with a Discard link when it comes back. The user then presses Save again. That is not a replay of the write: the user submits the restored form themselves.
6. Brain calls host-agent → renders the sshd drop-in from the enabled set, writes the account's keys to a **root-owned file outside the user's home** (`/etc/ssh/moose-authorized-keys/<user>`), validates with `sshd -t`, reloads, and starts or stops the daemon as the enabled set requires. The per-account `Match` block points sshd at that file **and** at the user's own `~/.ssh/authorized_keys`, so keys a user added from their shell keep working and moose never touches that file. Keeping moose's keys out of the home directory is a security requirement, not tidiness: host-agent runs as root and `~/.ssh` is a path the account controls, so writing there as root can be redirected by a symlink the user swaps in. SMB is the `valid users` allowlist plus a Samba reload, unchanged.

**Why one password instead of two:**

- Same threat model. SSH and SMB are non-browser access to the same box; the dashboard is browser access. Whatever the user types into a credential field, the security ceiling is the password's strength.
- Realistic user behavior: with two passwords, users reuse the same string anyway. We'd be enforcing a separation that exists only on paper while paying the UX cost of explaining it.
- Fewer credentials to manage means fewer credentials forgotten, written on Post-its, or stored in unsafe places.

**Samba password backend:** Samba historically wants its own password DB (`tdbsam`), which doesn't share storage with `/etc/shadow`. We use Samba's PAM passdb backend (`passdb backend = tdbsam` with `unix password sync = yes` + `pam password change = yes`) so a password change via `passwd` automatically updates Samba. host-agent does the change atomically (`passwd` + Samba sync as one operation) so drift doesn't occur in practice.

**Network scope (appliance):** SSH on :22 and SMB on :445 are firewalled to RFC1918 + the mesh interface — see `BUILD.md` # SSH. Both are structurally blocked from the public internet; both work from a paired mesh device. Pair the device to access the box remotely. That scoping is what lets the appliance keep the password as its mandatory factor.

**Network scope (hosted):** there is no LAN and no mesh, so there is nothing to scope to, and no SMB at all (`ENVIRONMENT.md` # Access & files). The box is its own perimeter — it runs no moose firewall and its provider attaches none — so the only control over :22 is whether sshd is running, which is exactly why the daemon follows the enabled set. Reachability is therefore binary and the credential has to carry the weight, which is the key requirement.

## Brain ↔ host-agent in the auth path

For the operations that need host privilege:

- **Dashboard login (every login):** brain → host-agent `verify_password(user, password)` → PAM `authenticate()` → yes/no. The brain mints a session on yes; rate-limits on no. The PAM service name is `moose`; the stack lives at `/etc/pam.d/moose`.
- **Password change** (Settings → My account → password, or recovery-code flow): brain → host-agent → `passwd <user>` + Samba sync (one atomic operation).
- **SSH/SMB opt-in toggles:** brain → host-agent → add user to `AllowUsers` / `valid users` allowlist + service reload. Optional `authorized_keys` write.
- **Tier-2 admin operations:** brain → host-agent → edit config, `systemctl` restart.

The brain's session middleware reaches host-agent for *credential verification* on each login (PAM is the credential store, not brain SQLite). After a session is established, role + ACL checks stay inside the brain — no per-request roundtrip. Host-agent trusts the brain because brain ↔ host-agent communication is over a private channel: a UNIX socket whose access is kernel-enforced via group membership. See `BRAIN_HOST_PROTOCOL.md` for the full protocol.

**Test invariant (CI must assert):** the `moose` group on the running system contains exactly one member — the brain's container runtime UID. Any additional member is a configuration error and fails the test. This is the entire authn/authz model for the brain↔host-agent boundary; if group membership is wrong, the security boundary is broken.

## Sharp edges

- **Cookie isn't shared across the toggle flip.** Re-auth on `.local` ↔ `.network` is the cost. Accepted; happens once per deliberate mode switch.
- **Member's temporary password travels out-of-band.** No email = admin tells the member verbally. For a household, fine. For a future use case (small office), revisit.
- **A "forgotten admin" with no other admin and no recovery code is unrecoverable.** Honest position. Reinstall + restore from off-site backup. The toggle defaults to ON specifically to make this rare.
- **Recovery-code photo backup carries the code into the user's cloud.** Privacy doc covers this honestly.
- **No 2FA / TOTP in v1.** Mentioned to set expectations. Will add post-MVP, designed to coexist with the toggle (TOTP is origin-independent, unlike passkeys).

## Locked decisions

- **Identity primitive: password only in v1.** No passkeys, no TOTP, no email-based recovery.
- **Password storage: PAM (`/etc/shadow`) is the source of truth.** Brain verifies via host-agent's `verify_password`. Brain SQLite carries no password hash.
- **One password for dashboard, SSH, and SMB.** The user has a single moose password; service access (SSH, SMB) is gated per-protocol via allowlists, not separate credentials.
- **Session shape: server-side opaque cookie**, 256-bit random ID, `HttpOnly`, `SameSite=Lax`, host-scoped (no `Domain` attribute), `Secure` on HTTPS origins.
- **Session lifetime: 30-day rolling, 90-day hard cap.**
- **Login UX: user-list style** with first name + letter glyph. Settings toggle to switch to a blank-form login for privacy-conscious users.
- **Roles enforced server-side in the brain.** UI hiding is defense in depth.
- **Tier-2 admin surface lives in the dashboard at `/settings/<service>/*`.** Same origin, same session, no forward-auth.
- **SSH and SMB are off-by-account-by-default.** Per-user allowlists are empty until the user opts in via Settings. Samba runs from boot; **sshd does not** — it follows the enabled set, so :22 is closed on a box where nobody uses SSH.
- **The mandatory SSH factor is the profile's.** A public key on hosted, the moose password on the appliance. The other factor is available as a required *second* method (`AuthenticationMethods publickey,password`), never as an alternative. Hosted refuses to enable an account that has no key. See `DECISIONS.md` 2026-09-09.
- **Admin recovery code: opt-in toggle, default on.** Shown once, hashed (stored in brain SQLite), single-use, no physical-access reset path. Validating the code triggers a password change through host-agent → PAM.
- **No SSO into Tier-3 apps.** Locked already in `SPEC.md`; reiterated here.
- **Cross-origin re-auth on toggle flip is accepted.** No session handoff in v1.

## Knock-on to other docs

- `FIRST_RUN.md` — Step 2 adds the recovery-code sub-step. Per-account SSH/SMB opt-in explicitly out of the first-run flow (it's a post-install Settings toggle, not a wizard step).
- `SERVICE_PROVISIONING.md` — Tier-2 implementation locked as "native Debian + systemd; UI in the dashboard." Previously left as "container or host service — implementation detail."
- `CONTROL_PLANE.md` — host-agent scope expands to include Tier-2 systemd/config management, user-credential verification (`verify_password`), and credential mutations (`passwd`, `authorized_keys`, sshd/Samba allowlists).
- `MOOSE_NETWORK.md` — "toggle-flip re-auth" sharp edge points here for the concrete mechanism.
- `SPEC.md` — "No moose SSO into apps" still correct; this doc covers what the moose session *does* govern.
