# moose Service Provisioning

> Working spec for how moose provides shared infrastructure to apps and how OS-level integrations are exposed. Companion to `SPEC.md`, `CONTROL_PLANE.md`, `APP_MANIFEST.md`.

## The three tiers

Every "service" on a moose box falls into one of three tiers, distinguished by **how deeply it integrates with the OS**. The tier determines the design.

### Tier 1 — Managed data services

Pure containers with persistent state: **Postgres, MySQL, MariaDB, Valkey** (v1; `redis` is a compatibility alias for Valkey). Later: MongoDB, others as demand justifies.

- Run as containers, fully isolated.
- Brain owns lifecycle, schema, credentials.
- Apps consume via `services:` declarations in the manifest; brain injects credentials as env vars.
- Backups are first-class — brain dumps state at backup time, restores symmetrically.
- Invisible to the user.

### Tier 2 — OS integrations

Things that need **deep system integration** and affect the whole box's behavior: **Tailscale, Samba/SMB, DLNA/UPnP** (v1 candidates), plus future entries like Pi-hole-style ad-blocking, exit-node VPNs, network printer sharing.

These cannot be regular apps because:
- They need privileged capabilities (`NET_ADMIN`, `/dev/net/tun`, raw sockets, host networking).
- They affect *other* apps' network/storage/visibility, not just themselves.
- Misconfiguration has system-wide blast radius.
- They have authentication flows external to moose (e.g., Tailscale's browser auth).
- Their updates belong with the OS, not the app store.

**Home: moose Settings UI**, not the App Store. **Curated by us** — no third-party Tier-2 modules in v1.

**Implementation is locked: native Debian packages, managed under systemd, with admin UIs surfaced inside the moose dashboard.** No Tier-2 service runs in a Docker container; no Tier-2 service exposes its upstream admin UI at its own subdomain. The user-facing surface is "Settings → Tailscale" (a moose-built UI on `moose.local/settings/tailscale`), not "Install Tailscale from store" or "open the Tailscale admin UI at tailscale.local." See `DECISIONS.md` 2026-05-14 and `AUTH.md` for the reasoning chain.

### Tier 3 — Regular apps

Everything else. Run with whatever permissions they declared in their manifest. The 99% case.

## Mental model: how do I install software on moose?

1. **Standard case:** install from app store (Tier 3).
2. **Need Postgres / Valkey:** the manifest just declares it. Brain provides it (Tier 1, invisible).
3. **Need Tailscale / SMB / VPN exit:** toggle in Settings (Tier 2, curated).
4. **Power user with a custom container:** paste a compose file (Tier 3 custom door).
5. **Genuinely advanced:** SSH in, do whatever — but it's not the supported path.

No `apt`, no terminal-by-default. The OS surface area is the UI.

---

## Tier 1 — Managed data services

### Catalog (v1)

- **Postgres** — versions 15, 16, 17 and 18. Most modern self-hosted apps target Postgres. **New manifests should declare 18**: apps have started depending on 18-only built-ins (`uuidv7()` is the first one the catalog hit), and a shim can't stand in for a built-in used as a column `DEFAULT`. **15 is deprecated** — still accepted so existing installs keep working, but not for new manifests. It stays accepted rather than being removed because there is no cross-version migration path yet (see the deferred list below), so dropping it would strand a box already running it.
- **MySQL** — versions 8.0 and 8.4 (upstream LTS series; 8.0 is past Oracle EOL but kept because Ghost pins it specifically). Some apps speak only the MySQL dialect — Ghost, Kimai.
- **MariaDB** — versions 10.11 and 11.4 (upstream LTS series). Some apps require it specifically — Nextcloud, WordPress.
- **Valkey** — version 8. Caching, sessions, queues. Provided by [Valkey](https://valkey.io), the Linux Foundation BSD-3-Clause fork of Redis 7.2.4 — RESP/ACL-compatible. New manifests should declare `type: valkey`.
- **Redis** — version 7, a **compatibility alias** for Valkey. moose never runs the upstream Redis image (Redis 7.4+ is RSALv2/SSPLv1, Redis 8+ is AGPLv3 — both on moose's avoid-list); a `type: redis, version: "7"` declaration always provisions the Valkey engine underneath (normalized to `valkey: "8"`), so a `redis:7` app and a `valkey:8` app share one instance. Kept so existing manifests and the broad Redis ecosystem keep working (`DECISIONS.md` 2026-06-13).

MySQL and MariaDB share one wire protocol and SQL dialect, so they are two `type` values backed by one provisioning path; the per-engine deltas are the image pin, the client binary names, the root-password env var, and the readiness probe (`DECISIONS.md` 2026-06-09). `valkey` and `redis` go further: they are two `type` values backed by **one engine** (Valkey) — `redis` normalizes to `valkey` before anything provisions, so there is no upstream-Redis code path at all (`DECISIONS.md` 2026-06-13).

We add new types when **3+ store apps actually want them**, not before. Each new type is real ongoing operational complexity (backup integration, version management, schema isolation).

Plausible v2+ additions: see # Post-v1 candidates. (MongoDB was evaluated and **declined** as a managed type — `DECISIONS.md` 2026-06-25; Mongo apps bundle their own engine.)

### Database extensions

There is no `extensions:` key in the manifest and no brain involvement in extensions. What an app can install is decided entirely by what the engine image ships and what its own credential is allowed to do, which splits Postgres extensions into three cases:

- **Trusted contrib extensions work today, unchanged** — `pgcrypto`, `uuid-ossp`, `citext`, `hstore`, `pg_trgm`, `ltree`, `unaccent`, `btree_gin`/`btree_gist`, `tablefunc` and the rest of the trusted set. They ship in the official `postgres` image, and since PG13 a non-superuser **database owner** may `CREATE EXTENSION` them. moose's per-app role owns its database (`CREATE DATABASE <db> OWNER <role>`), so an app just runs the statement in its own migrations. This is the case that covers most apps that "need an extension"; it needs nothing from the platform.
- **Untrusted contrib extensions are not available** — `pg_stat_statements`, `plpython3u`, `file_fdw` and friends are in the image but require superuser, which no app credential has. An app needing one bundles its own engine.
- **Third-party extensions are a different image, not a version** — `pgvector`, `postgis`, `timescaledb` are absent from the official image entirely, so no grant can help; they would need `pgvector/pgvector` or `postgis/postgis` as a distinct managed **type**, subject to the same "3+ store apps actually want it" bar as any other type (#355 holds that decision). Until then, an app needing one bundles its own engine (the `services:`-routing checklist's "not yet provisioned" case).

The same rule holds for the MySQL family and Valkey: whatever the upstream image ships is what an app gets.

### Provisioning protocol — end to end

> **Implementation status (v1, 2026-06-05 — `docs/progress/managed-services-postgres.md`; MySQL family 2026-06-09 — `docs/progress/managed-services-mysql.md`; Valkey/Redis 2026-06-13 — `docs/progress/managed-services-redis.md`).** Built: Postgres, MySQL, MariaDB, and Valkey provisioning end-to-end (lazy spinup, per-app credential, `MOOSE_SERVICE_<NAME>_*` injection, drop-on-uninstall). The brain provisions by running **the service's own client** (`psql` / `mysql` / `mariadb` / `valkey-cli`) in a **throwaway one-shot container** — `docker run --rm --network moose-svc-<k>-<v> --env-file <serviceDir>/.env <serviceImage> <client …>` — not a client connection of its own, so the control plane never joins the service network: only the `--rm` container does (`DECISIONS.md` 2026-06-02, 2026-06-15). This replaced an earlier `docker exec` into the long-running service container, which the docker-socket-proxy denies (`CONTROL_PLANE.md` # Docker socket exposure; `DECISIONS.md` 2026-06-14, 2026-06-15). Each service is a brain-owned compose project with a fixed `container_name` (`moose-svc-postgres-15`, the readiness-inspect handle) and an in-network DNS alias (`postgres-15.moose.internal`, the DSN host *and* the host the one-shot client connects to over TCP); dots in a version fold to dashes in every derived name (`moose-svc-mysql-8-0`, `mysql-8-0.moose.internal`) because compose project names reject dots. MySQL-family DSNs are `mysql://…:3306/…` for both engines (one wire protocol). **Valkey** (the engine behind both `type: valkey` and the `type: redis` alias — `redis:7` normalizes to `valkey:8`, so both land on `moose-svc-valkey-8`) has no database: the per-app credential is an **ACL user with full keyspace** (`redis://…valkey-8.moose.internal:6379`, no DB path; the universal RESP scheme; the credential is the isolation boundary, `DECISIONS.md` 2026-06-13), provisioned by `ACL SETUSER` and persisted to an external aclfile on the data volume (`ACL SAVE`) so it survives a restart — Valkey ACLs are config, not keyspace. **Deferred:** the grace-shutdown timer (services stay running after the last consumer uninstalls), backup/restore + cross-version migration (gated on the backup design), and at-rest encryption of the stored superuser + per-app passwords (plaintext today — folds into `NEXT.md` # App-secret injection hardening). See `NEXT.md` for each.

#### At app install

1. Brain reads the manifest, sees `services.database: { type: postgres, version: "15" }`.
2. Brain checks if a Postgres-15 instance is already running.
   - **Lazy spinup:** if not, start one now. Idle versions don't run.
3. Brain connects to that Postgres-15 instance as superuser and creates:
   - a database named `<app-id>_<random_suffix>` (e.g., `photoprism_a4f7`)
   - a role with a randomly-generated password
   - grants the role full privileges on that database, *only* that database
4. Brain stores the credentials in its SQLite, encrypted at rest.
5. At app start, brain injects env vars into the app's container:
   ```
   MOOSE_SERVICE_DATABASE_HOST=postgres-15.moose.internal
   MOOSE_SERVICE_DATABASE_PORT=5432
   MOOSE_SERVICE_DATABASE_NAME=photoprism_a4f7
   MOOSE_SERVICE_DATABASE_USER=photoprism_a4f7
   MOOSE_SERVICE_DATABASE_PASSWORD=<generated>
   MOOSE_SERVICE_DATABASE_DSN=postgres://photoprism_a4f7:...@postgres-15.moose.internal:5432/photoprism_a4f7
   ```
6. The app's compose maps these to whatever variables the app actually expects (per `APP_MANIFEST.md` — naming convention is **app-defined**).

### Env-var injection — the full family

Managed-service credentials are one of four `MOOSE_*` injection mechanisms the brain stamps into an app's environment, all sharing the same contract: **the brain owns a stable variable name; the app's compose maps it to whatever it expects.** The app never hardcodes a moose-specific value and stays portable.

| Variable | Source | Stability |
|---|---|---|
| `MOOSE_SERVICE_*` | Managed-service credentials (this doc) | Per provisioning; rotates only on re-provision |
| `MOOSE_FOLDER_<NAME>` | In-container path of a bound use-case folder (`APP_MANIFEST.md` # folders) | Fixed (`/moose/<folder>`) |
| `MOOSE_DATA_DIR`, `MOOSE_APP_URL`, `MOOSE_INSTANCE_ID` | Per-instance facts the brain knows (data root, routed URL, id) | Fixed for the instance |
| `MOOSE_SECRET_<NAME>` | A per-app random secret the brain **generates** from a manifest `secrets:` declaration | **Generated once at install, persisted, re-emitted verbatim** — never re-rolled |
| `MOOSE_MAIL_*` | The outgoing-mail provider the instance is bound to (this doc # BYO outgoing mail) | Stamped at install / rebind; absent when unbound |

**User-supplied config (`config:`, `APP_MANIFEST.md` # D4) is the one injected family that breaks this contract on purpose.** Its values aren't moose's to own — they're the app's own native variables the user supplies (`OPENAI_API_KEY`, a provider token) — so there is no `MOOSE_*` name and no app-side mapping: the brain injects the value **directly under the `app_env` name** the manifest declares, into the target service's `environment:` in the compose override (default `main_service`, overridable per field), rather than into the interpolation `.env`. The form shows that same `app_env` as the technical hint, so the variable the user reads in the app's upstream docs is the variable shown, supplied, and set. A `config` field marked `secret: true` is stored and protected exactly like a `MOOSE_SECRET_*` value (never logged, never echoed into compose output) — the same at-rest hardening surface (`NEXT.md` # App-secret injection hardening). See `DECISIONS.md` 2026-06-26.

The secret case is the only one the brain *creates* rather than *reflects*: for each `secrets: [{name, bytes?, show?}]` entry (`APP_MANIFEST.md` # D2) it draws `bytes` (default 32, floor 16) from a CSPRNG, base64url-encodes them, and persists the value alongside the instance. Stability is load-bearing: a token-signing secret (e.g. `BETTER_AUTH_SECRET`) that changed on restart would invalidate every live session, so the value is read back from storage on every `.env` rewrite, not regenerated. Security hardening of this path (env-var delivery surface, at-rest encryption, rotation) is open — `NEXT.md` # App-secret injection hardening.

A secret declared `show: true` is **owner-visible**: the brain serves its value to the instance owner (and admins) at `GET /apps/{id}/secrets`, surfaced on the app detail page (`DASHBOARD.md` # Installed apps). The read follows the app's control authorization — admins for any app, the owner for their own personal app, the same gate as stop/start — so a member can't read another user's secret and a household app stays admin-only; the response lists only the `show` secrets, never the internal ones. This exists so a *self-authenticating* app (one whose login is gated by a token, not by moose's session) can use a **per-instance random** bootstrap secret the owner reads once to set a password, instead of shipping a published constant whose fail-open case is a permanent LAN backdoor (#152, the Jupyter setup-token case). Revealing is a pure read, so it does not audit (only elevation-class mutations do, `CONTROL_PLANE.md`).

#### At app uninstall

1. Run app's `pre_uninstall` hook (e.g., final dump if it wants).
2. Drop the database, drop the role. Clean slate — no leftover schemas.
3. If this was the last app using the Postgres-15 instance, mark it for shutdown after a grace period (e.g., 12 hours). Avoids spin-up churn if the user is reinstalling.

#### At backup

1. Run app's `pre_backup` hook (let it quiesce / dump app-internal state).
2. Brain runs `pg_dump --format=custom` on that app's database.
3. Dump is included in the app's backup archive alongside its data volumes.
4. Run app's `post_backup` hook.

Per-app dumps mean per-app restore — restoring one app doesn't disturb others sharing the same Postgres instance.

#### At restore

1. Brain ensures the right Postgres major version is running.
2. Brain recreates the app's database and role with the credentials from the backup.
3. Brain pipes `pg_restore` from the dump.
4. App starts, reads injected env vars (which now point to the restored DB), resumes.

#### At app update — same major version

Nothing special. Same DB, same credentials.

#### At app update — different major version (cross-version migration)

When the new app version's manifest declares a different major (e.g., now needs Postgres 16 instead of 15):

1. Brain auto-takes a backup of the app's current DB before doing anything.
2. Brain spins up the new major version instance if not already running.
3. Brain `pg_dump` from old → `pg_restore` into new.
4. App starts pointed at the new instance.
5. Old DB and role on the previous version are dropped.

**Auto-migrate is the policy** — happens transparently as part of the app update. The pre-migration backup is the safety net; if the migration fails, moose rolls back to the old version + restored DB and surfaces the failure to the user. We accept the responsibility of getting this right; the alternative (force every cross-version app update through a manual user prompt) creates worse UX for the non-technical audience.

### Network architecture

- Each managed service instance runs on a dedicated internal Docker network: `moose-svc-postgres-15`, `moose-svc-valkey-8`, etc.
- Apps that declared a service in their manifest are attached to the matching network at start time.
- Internal DNS: `postgres-15.moose.internal` resolves **only on networks where that service is reachable**.
- Apps **cannot reach managed services they didn't declare**. Network membership is the enforcement mechanism, not a software allowlist.

### Per-app isolation in shared instances

One Postgres-15 instance serves many apps. Each app sees only:
- Its own database.
- Its own role with privileges on that database only.

Enforcement is via standard Postgres role/grant mechanics — not separate instances. Cleaner resource use, simpler operations.

**Valkey** (the engine behind both `type: valkey` and the `type: redis` alias) has no database to scope a role to, so the per-app unit is an **ACL user with full keyspace** (`ACL SETUSER … ~* &* +@all -@admin -flushall -flushdb -swapdb`): each app gets its own credential — revocable on uninstall, and unable to touch the ACL system, `CONFIG`, `SHUTDOWN`, or replication (`-@admin`) — but the keyspace is shared. The isolation boundary is the credential (no unauthenticated access; an app with no Valkey declaration never joins the network), not a key partition. This is a stronger boundary than a logical-DB-number split, which has no auth boundary between apps (`DECISIONS.md` 2026-06-13). `-flushall -flushdb -swapdb` removes the keyspace-destruction commands by name (rather than the blunt `-@dangerous`, which would also strip `INFO`/`KEYS`/`SORT` that ordinary clients call) so a single compromised or buggy app can't wipe the shared keyspace every other app reads from. The shared keyspace means one app can still *read* another's keys; per-app key **confidentiality** is the deferred isolation hardening (`NEXT.md` # Managed-service per-app key isolation), whose clean form is the `isolated: true` dedicated-instance escape hatch below.

If we later need stronger isolation (security-sensitive app, regulatory requirement), we can add a `services.database.isolated: true` manifest field that forces a dedicated instance for that app. **Not in v1.**

### Versioning

- Multiple major versions can coexist (Postgres 15 and 16 running side-by-side).
- Brain spins up versions only when an app actually requests them.
- Brain shuts down versions when the last app using them is uninstalled, after a grace period.

### Storage tier for managed services

- The shared Postgres / Valkey data lives on **fast tier** if available, falling back to normal.
- Apps don't get a say — this is OS-policy, not per-app config.

---

## BYO outgoing mail (`MOOSE_MAIL_*`)

> Implemented 2026-06-12 (`docs/progress/byo-outgoing-mail.md`, issue #122).

Many apps want to *send* email — password resets, reminders, invites. moose does not run a mail server, relay, or smarthost: **each user brings their own SMTP account** (Fastmail, a Gmail app password, the ISP's smarthost), and the brain injects its credentials into the apps that user chooses. The app dials the provider itself over its declared `internet` permission; no mail traffic flows through moose infrastructure.

**Providers** (the UI says "email accounts") belong to one user: the user who added it (`owner_user_id`, `DECISIONS.md` 2026-09-25). Any signed-in user can add them, in Settings → Outgoing email or inline on the install setup page, and each user sees and uses only their own. Another user's account is invisible on every read and answers 404 on every write, the same as an id that does not exist, so ids do not leak. Adding and editing your own account need no password re-prompt; delete keeps it, because it cannot be undone and it unbinds every app that uses the account (`USERS_AND_GROUPS.md` # Elevation in the UI). Sharing an account with other users is not built yet. Labels are unique per owner, not per box. When a user is deleted, their accounts go with them, and apps bound to one fall back to unbound, the same as when the account itself is deleted. Boxes from before owners moved every existing account to the founding admin (the oldest admin), which on a hosted box is its only user. A provider has a label, SMTP host + port, optional username/password, a from address, and an encryption mode (`none` | `starttls` | `tls`). A synchronous test-send (`POST /mail-providers/{id}/test`) validates a provider end to end — dial, TLS, auth, one delivered message — before any app depends on it. A second, weaker check runs **before** a provider is saved: `POST /mail-providers/verify` takes an unsaved provider body, connects and authenticates, and hangs up without sending anything, so a config that cannot connect never becomes an account. The add form runs it by default ("Test configuration when adding"). The two are deliberately different strengths — only a delivered message proves the provider accepts the from address, which is why the test-send stays, and why it needs a recipient the user chooses rather than an address moose picks. Passwords are write-only at the API: requests carry them, responses never echo them. (At-rest they are plaintext in the brain's SQLite today, same status as managed-service credentials — folds into `NEXT.md` # App-secret injection hardening.)

**Binding** is per-instance: a mail-capable app (manifest `mail:` block, `APP_MANIFEST.md` # D3) is bound to at most one provider. The install setup page offers the picker with the installer's own accounts (None is the default and always valid; a sole account is preselected), and any user can add an account there without leaving the install (`DASHBOARD.md` # Install authorization). The binding is changeable later from the app's settings screen: by admins for any app, by a member for their own personal instances, same authorization as stop/start. **An app can only be bound to the caller's own account**, at install and on a rebind, whoever owns the app. So a household app installed by an admin sends through that admin's account, and another admin who rebinds it picks from their own. `POST /api/v1/apps` and `PUT /api/v1/apps/{id}/mail-binding` answer another user's account id with the same 422 as a missing one. Unbound means **nothing is injected**: the app must run with email features off, which is why v1 admits only `optional: true` manifests.

A bound instance's `.env` carries the discrete fields plus a Symfony-style DSN, since apps differ in what they consume:

```
MOOSE_MAIL_HOST=smtp.fastmail.com
MOOSE_MAIL_PORT=465
MOOSE_MAIL_USER=box@example.com
MOOSE_MAIL_PASSWORD=<stored>
MOOSE_MAIL_FROM=box@example.com
MOOSE_MAIL_ENCRYPTION=tls
MOOSE_MAIL_USE_TLS=false
MOOSE_MAIL_USE_SSL=true
MOOSE_MAIL_DSN=smtps://box%40example.com:...@smtp.fastmail.com:465
```

The DSN scheme is `smtps://` for implicit TLS and `smtp://` otherwise (SMTP-URL consumers negotiate STARTTLS opportunistically; an app needing the exact mode reads `MOOSE_MAIL_ENCRYPTION`). `MOOSE_MAIL_USE_TLS` / `MOOSE_MAIL_USE_SSL` are boolean projections of that mode (STARTTLS vs implicit TLS, at most one true) for apps that take two separate flags — e.g. Django's `EMAIL_USE_TLS` / `EMAIL_USE_SSL`, which Paperless surfaces — since a compose file can't derive a boolean from the encryption string. Credentials are URL-escaped. The app's compose maps the vars to whatever it expects, per the family contract above — with a compose default for the unbound case so absence degrades cleanly (Kimai: `MAILER_URL: "${MOOSE_MAIL_DSN:-null://null}"`).

**Port 587 is the only submission port that works on hosted.** Measured 2026-08-27 from a `cx23` in `hel1` on the production Hetzner account: outbound **25 and 465 are both blocked** (no SYN-ACK), while **587 and 2525 are open**. The same-host comparison is what makes this port-based rather than a routing artifact — `smtp.sendgrid.net` and `smtp.gmail.com` each answered on 587 and timed out on 465. So on hosted a provider registered with `encryption: tls` (implicit TLS, port 465) never delivers, and its test-send fails as a bare connection timeout; every provider in the list below offers 587 with STARTTLS, which is the mode to register. This is a **hosted-profile constraint only** — an appliance box on a home LAN reaches 465 normally, and some providers (Fastmail, iCloud) prefer it, so `tls` stays a valid mode and the UI warns rather than forbids. Hetzner will lift the 25/465 block on request; if that is ever filed, this paragraph is what needs re-measuring.

**Providers are picked from a list, not typed from scratch.** For every provider worth presetting the host, port and encryption are fixed constants, and often the username is too; what is genuinely per-user is the credential, the from address, and for two providers a region. So Settings → Outgoing email is a two-step add: pick the provider, then fill in only what is left. Host, port and encryption are prefilled behind an **Advanced settings** disclosure, editable, so a non-standard endpoint is never trapped. `Custom SMTP server` is that same disclosure, open, with nothing prefilled — the old form. The brain persists which preset the user picked as `provider_type`; it does **not** re-derive the other fields from it, so an advanced override survives (`DECISIONS.md` 2026-08-27, D1–D4). The table lives in `internal/mailpreset` and is served at `GET /api/v1/mail-presets` (any signed-in user).

| id | Label | Host | Port | Username |
|---|---|---|---|---|
| `ses` | Amazon SES | `email-smtp.<region>.amazonaws.com` | 587 | admin's SES SMTP username |
| `sendgrid` | SendGrid | `smtp.sendgrid.net` | 587 | fixed `apikey` |
| `mailgun` | Mailgun | `smtp.mailgun.org` (US) / `smtp.eu.mailgun.org` (EU) | 587 | prefilled `postmaster@` |
| `postmark` | Postmark | `smtp.postmarkapp.com` | 587 | the Server API token, same as the password |
| `brevo` | Brevo | `smtp-relay.brevo.com` | 587 | the SMTP login, `xxx@smtp-brevo.com` |
| `resend` | Resend | `smtp.resend.com` | 587 | fixed `resend` |
| `smtp2go` | SMTP2GO | `mail.smtp2go.com` | 2525 | admin's SMTP user |
| `google_workspace` | Google Workspace | `smtp.gmail.com` | 587 | the full address |
| `custom` | Custom SMTP server | — | 587 | admin's |

Every preset is **STARTTLS**, and every port is one a hosted box can reach (see the port paragraph above). SMTP2GO is 2525 because that is the port it recommends as open in the most places. Only SES and Mailgun carry a region, and each region option names the host it resolves to rather than substituting a code into a template — Mailgun's EU host is a prefix, so one substitution rule could not serve both.

**Three providers are excluded on purpose.** **Microsoft 365**: SMTP AUTH is off by default on new tenants, and a preset that leads to a failed test-send is worse than no preset. **Proton**: needs Bridge, so there is no SMTP endpoint to point at. **Fastmail and iCloud**: 465-only in practice, which hosted blocks — revisit with the appliance.

**Propagation:** env is read at container create, so a rebind re-stamps the `.env` and recreates the instance's containers immediately (stopped instances pick it up at next start). Editing or deleting a *provider* does not re-stamp bound apps: they keep the previously injected values until their next rebind or reinstall — v1 accepts this lag, and the Settings UI says so. Deleting a provider unbinds its apps in the brain's state (the next rebind of each app drops the vars).

**Explicitly not in v1** (deferral, not rejection — `NEXT.md` # Outgoing mail): no moose-run relay/smarthost, no per-app rate limiting or queue, no inbound mail anything. If email grows more surface (a default box-wide provider, brain-sent notification email riding the same providers), this section promotes to its own `OUTGOING_MAIL.md`.

---

## Tier 2 — OS integrations (v1)

Three integrations targeted for v1. All three have clear demand, established implementations, and bounded scope.

### Tailscale

- Settings → Network → Tailscale.
- Installed as the upstream `tailscale` Debian package. `tailscaled` runs under systemd on the host.
- Moose's UI at `/settings/tailscale` is a thin wrapper over host-agent operations (`tailscale up`, `tailscale status`, etc.).
- User clicks "Sign in"; brain triggers `tailscale up` via host-agent, which prints a one-time auth URL. The dashboard surfaces the URL as a button that opens Tailscale's standard browser auth flow.
- Once joined, the box is on the user's tailnet.
- Apps that declare `permissions.tailscale: true` (manifest perm) are reachable via Tailscale's MagicDNS from any device on the user's tailnet.
- **Coexists with moose's built-in mesh.** Two separate networks. A user might use the moose mesh for "people I share photos with" and personal Tailscale for "all my own machines."
- Tailscale account is between the user and Tailscale Inc. — moose doesn't broker auth.

### Samba / SMB

- Settings → Sharing → Network shares.
- Installed as the upstream `samba` Debian package. `smbd` and `nmbd` run under systemd on the host.
- Exposes two share shapes over SMB so Windows / macOS / Linux clients can mount them as network drives: per-user home (`\\moose\<user>` → `/home/<user>/`) and household-shared (`\\moose\shared` → `/srv/moose/shared/`). See `STORAGE.md` # Cross-device access (SMB).
- Moose's UI at `/settings/shares` lets each user opt in/out of their own SMB share (off-by-account-by-default per `AUTH.md`). Brain edits `/etc/samba/smb.conf` (specifically `valid users` allowlists) and asks host-agent to `systemctl reload smbd`. Credentials are the user's moose password — no per-share password.
- Critical for the "I plug in moose and want it as a NAS for my laptop" use case.

### DLNA / UPnP media streaming

- Settings → Sharing → Media streaming.
- Exposes media in the household `Shared/Photos/` and `Shared/Movies/` folders over DLNA so smart TVs and game consoles can browse and play them.
- Often folded into specific apps (Plex, Jellyfin) but a lightweight built-in option covers the "I just want to play videos on my TV" non-app case.
- May be deprioritized if Jellyfin coverage in the app store is solid at launch; revisit closer to v1.

### What makes something a Tier-2 candidate

- Privileged capabilities required.
- System-wide effect.
- External authentication flow.
- Curated by us — committed to maintenance.
- **Available as a Debian package (or packageable by us as a `.deb`).** If a Tier-2-shaped integration only ships as a Docker container, we either (a) don't support it in v1, or (b) accept a one-off Docker-with-extra-caps path for that specific case. Most viable Tier-2 candidates have first-class Debian packaging from upstream.

We don't accept third-party Tier-2 contributions in v1. Adding a new Tier-2 integration is an OS feature, not an app submission.

---

## Post-v1 candidates

Ideas explicitly out of scope for v1, kept here so we don't lose them. Nothing in this section is committed — each entry would need a separate design pass before becoming a locked decision.

The bar for promotion is the same we apply to new Tier-1 types: (1) 5+ apps would actually use it, (2) sharing creates real benefit beyond convenience (security patching, ops integration, user-visible UX), (3) does not require app upstreams to redesign themselves around moose, (4) bounded API surface.

### Scheduled / deferred jobs

A unified facility for apps to declare periodic or constraint-based background work. Cron-style schedules ("re-index every 24h") and constraint-based dispatch ("run when the box is idle, on AC power") in one shape — Android's `WorkManager` is the model. Apps declare jobs in the manifest; moose arbitrates execution.

Value:
- Single observability surface — Activity view can show *why your box is loud at 3am* (Immich indexing, Paperless OCR'ing). Synology-tier ops visibility.
- Resource arbitration — don't let five apps kick off heavy jobs simultaneously.
- Power-aware — defer expensive jobs when the laptop-in-the-pantry is on battery.

Caveat: apps with framework-embedded schedulers (Sidekiq-cron, APScheduler, etc.) won't fully migrate; moose's scheduler covers what apps choose to declare, not all background work.

### Additional managed data services (Tier-1 catalog growth)

Extend the catalog as concrete app demand justifies. We **host the substrates apps already use** rather than inventing new APIs — same shape as Postgres/Valkey today.

Plausible additions:
- **MongoDB** — common in modern self-hosted apps, but **declined as a managed type, not deferred** (#253, `docs/progress/mongodb-compat-spike.md`, `DECISIONS.md` 2026-06-25). The license-clean engine (FerretDB v2, the redis→valkey substitution for SSPL Mongo) lacks change streams / oplog / replica sets / transactions (blocks Rocket.Chat et al.) and, decisively, enforces no per-database authorization, so the # Per-app isolation contract can't be met on a shared instance; real MongoDB can't be the managed engine either, because moose *serving* the database is the SSPL trigger. There is deliberately no `mongodb`→FerretDB alias (FerretDB is not a drop-in, unlike Valkey for Redis). **Mongo-needing apps bundle their own engine** (the Umbrel/CasaOS pattern, which ships real MongoDB and raises no SSPL obligation when the app uses it internally); curation accepts them per `NEXT.md` # Store catalog curation policy.
- **Kafka, RabbitMQ** — queue/streaming *substrates*, if app demand emerges.

(MariaDB graduated from this list to the v1 catalog alongside MySQL — `DECISIONS.md` 2026-06-09.)

We host queue substrates, not queue libraries. Sidekiq (Ruby), BullMQ (Node), Celery (Python), RQ — these are libraries that run *inside the app's own container*, pointed at a substrate we provide (already-managed Valkey for most; potentially Kafka or RabbitMQ later). Moose does not build or expose a queue API of its own.

### Cross-box services (federated state)

The biggest and most differentiated post-v1 idea. None of the home-server OS competitors have a story here.

User-facing pitch: "your grocery list syncs with your partner's box; your photos sync with your parents' box" — without either app author writing networking code.

The shape is unresolved — three plausible technical models, each implying a different developer experience:

- **Master-master replication** (CouchDB / PouchDB). Each box holds a full DB copy; bidirectional eventually-consistent sync. Mature but dated DX; apps work with conflict-resolution documents.
- **CRDT-as-library** (Automerge, Yjs). No central "server" — apps work with CRDT documents directly, sync is peer-to-peer. Modern DX, arguably better-aligned with the "files are first-class" instinct elsewhere in the spec, but storage story is less mature.
- **Local-first SQLite with sync** (cr-sqlite, Turso embedded replicas). Apps see a normal SQL DB; sync at the storage layer. Most familiar API, youngest ecosystem.

These are not interchangeable; the choice constrains what apps can be built on top. Not locked.

The bigger unresolved piece is **cross-box identity and consent**, which is the load-bearing part of any federation story and likely needs its own design doc (sketch: `FEDERATION.md`) before this gets serious. Components: how a user is named across boxes (box-ID + username? email-shape?), how box-B verifies that box-A's claim of "I'm Alice" is real (tied to the mesh's identity model, or separate?), the consent surface in the dashboard, granularity (per-app? per-document?), and revocation — which is genuinely hard once data has propagated. Each of these is its own design problem.

Why this is post-v1:
- Scope is large across multiple unsettled axes (sync model, identity, consent, revocation).
- No concrete apps demand it in 2026 — the self-hosted ecosystem is single-box-shaped. Risk of building rails for users who don't exist.
- Ecosystem seeding: this only pays off if apps adopt the pattern, which likely requires moose shipping reference apps to demonstrate it.

Locked now: **the moose mesh is the intended transport for future cross-box services.** Whatever we build later rides on the same Headscale/DERP substrate we ship for personal device access, not a separate network plane.

---

## Locked decisions

- **Three-tier model:** managed data services / OS integrations / regular apps.
- **v1 Tier-1 catalog:** Postgres (15, 16), MySQL (8.0, 8.4), MariaDB (10.11, 11.4), and Valkey (8; `redis: "7"` is a compatibility alias for it — moose never runs upstream Redis). Add types only when 3+ store apps justify it.
- **v1 Tier-2 list:** Tailscale, Samba/SMB, DLNA/UPnP (DLNA possibly deprioritized).
- **Shared instances for Tier 1.** One Postgres-15 instance serves many apps; isolation via Postgres roles/DBs.
- **Lazy spinup.** Tier-1 instances start when first needed, shut down with a grace period after the last app using them is uninstalled. *(v1: lazy spinup built; grace-shutdown deferred — services stay running.)*
- **Provisioning via a one-shot client container, not a brain SQL client.** The brain runs the service's own client (`psql` / `mysql` / `mariadb` / `valkey-cli`) in a throwaway `docker run --rm` container joined to the service network to create per-app databases/roles, so the brain itself never joins that network — same principle as probing through Caddy (`DECISIONS.md` 2026-06-15, superseding the original `docker exec` approach which the socket-proxy denies — 2026-06-14). The client connects over TCP to the service's in-network DNS alias (the DSN host); the service container's fixed `container_name` is the brain's readiness-inspect handle.
- **Cross-version migration: auto-migrate** with an automatic pre-migration backup as the rollback safety net. No prompts.
- **Network isolation:** apps reach Tier-1 services only via dedicated Docker networks; no manifest declaration → no network membership → no reachability.
- **Env-var injection:** stable `MOOSE_SERVICE_*` names; app maps them in its compose to whatever it actually expects (per `APP_MANIFEST.md`). Same contract for the rest of the `MOOSE_*` family (`MOOSE_FOLDER_*`, `MOOSE_APP_URL`, `MOOSE_SECRET_*`). **Exception:** user-supplied `config:` (`APP_MANIFEST.md` # D4) is injected **directly under the app's own `app_env` name** with no `MOOSE_*` indirection and no mapping line — the value is the app's native variable, not a moose-owned value the app must adapt to.
- **Generated secrets (`MOOSE_SECRET_*`):** a manifest `secrets:` declaration makes the brain generate a CSPRNG value once at install, persist it, and re-emit it stably across restarts. The only injected variable moose creates rather than reflects. A secret marked `show: true` is owner-visible at `GET /apps/{id}/secrets` (owner-or-admin, surfaced on the app detail page) so a self-auth app's bootstrap token can be per-instance random instead of a published constant (#152); unmarked secrets stay internal. Security hardening is open (`NEXT.md` # App-secret injection hardening).
- **Outgoing mail is BYO (`MOOSE_MAIL_*`), not a moose relay.** Each user adds their own external SMTP accounts; the brain injects the bound provider's credentials per instance and the app dials the provider itself. No smarthost, no queue, no inbound mail in v1; unbound apps get nothing injected and must run with email off (`mail: optional: true` is the only admitted shape).
- **Tier 2 is curated, not open.** No third-party Tier-2 in v1.
- **Tier 2 runs as native Debian packages under systemd**, not as Docker containers. The admin UI lives in the moose dashboard at `/settings/<service>/*` — no upstream admin UI is exposed at its own subdomain. Tier 2 updates ride apt.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
