# moose App Manifest

> Working spec for the `manifest.yml` schema — the contract between an app and the moose OS. Companion to `SPEC.md`, `CONTROL_PLANE.md`, and `APP_LIFECYCLE.md`.

## Core design principle: one model, two doors

The brain only ever knows about manifests. *Everything* installed on a moose box has one. The user-facing UX has two entry points:

- **Door 1 — App store.** App author wrote a complete `manifest.yml` + `docker-compose.yml`. One-click install. Full integration (managed services, backup hooks, declared permissions).
- **Door 2 — Custom container.** User pastes/uploads a raw `docker-compose.yml`. The brain **generates a synthetic manifest** with sensible defaults. The app is a first-class citizen — it gets a subdomain, shows in the dashboard, integrates as much as the synthetic manifest allows.

This unification matters because:
- The brain's data model stays simple — one type of thing.
- A power user can paste a compose file today; the synthetic manifest is designed to *graduate* later (add backup hooks, request a managed DB, refine volumes) into a richer manifest of the same schema. **In-product editing of a synthetic manifest is deferred past v1** (`NEXT.md`) — v1's custom flow is install-only, and changing a custom app means uninstall + re-paste. See `DASHBOARD.md` # Door-2 custom container install flow.
- Door-1 is just "we wrote the manifest for you."

## Author philosophy

App authors **adapt their app to run on moose.** This is an explicit design choice, not an accident.

- We provide thorough, friendly docs and examples.
- We expect authors to make small, well-defined changes — pointing env vars at moose's injected values, splitting cache from data volumes, declaring permissions honestly.
- We do **not** auto-rewrite the compose file or guess at things. The manifest is the author's contract; if it lies, the app misbehaves and it's on the author.
- For popular OSS apps that don't know moose exists, we maintain manifests ourselves in the official catalog repo. Same schema, same rules.

## Format

- **YAML.** Same mental space as `docker-compose.yml`. App authors are already in YAML when writing compose; one less context switch.
- **Schema-versioned from v1.** Top-level `manifest_version: 1`. We commit to backward compatibility for at least the previous two major versions. When we change semantics, old-version manifests keep working.
- **Public, versioned spec.** Third-party stores depend on this format. The schema is published, stable, and changes only in versioned increments.

## What's required, what's optional

**Required (the bare minimum to install an app):**
- `id`
- `manifest_version`
- `name`
- `version`
- `compose_file`
- `main_service`
- `main_port`

**Everything else is optional** with sensible defaults that do the right thing. A minimal valid manifest is ~7 lines.

## Field categories

### A. Identity and metadata

For the store and the dashboard UI. The brain mostly doesn't care; the store does.

```yaml
id: photoprism                    # globally unique slug
name: PhotoPrism                  # display
version: 2.4.1                    # app version
manifest_version: 1               # schema version
description:
  short: "Self-hosted photo library with AI tagging"
  long: |                         # markdown allowed
    PhotoPrism is an AI-powered app for browsing,
    organizing & sharing your photo collection...
icon: ./icon.png                  # bundled in the app package
icon_glyph: image                 # optional; Lucide icon name used as the store fallback when no `icon` is bundled (ignored when `icon` is set)
screenshots: [./shot1.png, ./shot2.png]
categories: [media, photos]
author:
  name: PhotoPrism Labs
  url: https://photoprism.app
license: AGPL-3.0
links:
  homepage: https://photoprism.app
  source: https://github.com/photoprism/photoprism
  support: https://docs.photoprism.app
changelog_url: https://github.com/photoprism/photoprism/releases  # optional; used by the "What's new" panel after an update
listed: true                      # optional, default true; `false` pulls the app from the store (hidden from browse + uninstallable) while keeping its manifest in the catalog
external_costs:                   # optional; money a THIRD PARTY charges to make the app useful
  - id: model-access              # kebab-case, unique within the app
    title: "Model access"         # 2-4 words, no vendor name
    description: "You bring your own provider key, and the provider bills you for what the assistant reads and writes."
    required: true                # the app's main job does not work without paying
    estimate: "a few dollars per million tokens (long agent runs use many times more than chat)"
    estimate_checked: 2026-08-10  # required whenever `estimate` is set
```

**`categories`** is an open-ended list of lowercase kebab-case tags. There is no fixed enum — authors introduce new values as needed (`marketing`, `developer-tools`, `food`, `books`, …). The brain does not validate category values; the store UI uses them for browse filters. Aim for reuse over novelty: check what existing catalog apps already declare before coining a new tag.

**`listed`** controls store visibility. Omitted (or `true`) ⇒ the app appears in the browse grid, has a detail page, and can be installed — the normal case. Setting `listed: false` **pulls the app from the store**: it's hidden from browse, its detail page and install paths return 404, but the manifest stays in the catalog directory — it still parses, lints, and serves its icons/screenshots, and an already-installed instance keeps its dashboard card and stays reconcilable (visibility is resolved by id, not via the filtered browse). This is how a `Blocked` or `Rejected` app is withdrawn without throwing away its adaptation work — e.g. an image that can't yet run under the sandbox, parked until the platform gap or upstream fix lands. It is a curation control, not a per-user or per-role one; there is no "show me unlisted apps" path in v1.

**`external_costs`** names money a **third party** charges to make the app useful: a model-provider API key the assistant cannot answer without, a mail provider an email app sends through. It is display metadata the store surfaces **before install**, so a user is never surprised by a bill from someone else. The brain does not act on it, and it never gates install — an app that will not *boot* without a paid thing is a curation verdict (`blocks-start`), not an external cost. Absent ⇒ the app costs nothing beyond the box.

It is deliberately **not** what moose charges for the app. That is a commercial decision which changes without the app changing, so it lives in the curation source next to `listed`/`environments` (store `status.yml` `price:`), not in this schema. Neither one is a limitation: a limitation is a broken feature, and paying for something is not a defect.

| field | required | notes |
|---|---|---|
| `id` | yes | kebab-case, unique within the app. A stable key a surface can hold across versions. |
| `title` | yes | Two to four words, sentence case, no vendor name (vendors change, the cost does not). |
| `description` | yes | Who charges, and why the app needs it. |
| `required` | yes | `true` when the app's main job does not work without paying. **No default** — an omitted key is rejected. Defaulting to `false` would let a forgotten line render a cost the app cannot work without as "optional", which is the surprise this whole block exists to prevent. |
| `estimate` | no | A short unit rate. **Omitting it is always valid** and is the right answer when no honest rate exists. Capped at 100 characters, counted in runes. A present-but-blank value is rejected: that is a slip, not the deliberate answer. |
| `estimate_checked` | with `estimate` | `YYYY-MM-DD`, the day the estimate was last confirmed. Rejected without an `estimate`. |

`estimate` is a **unit rate, never a monthly total** — a total depends on how one person uses the app, which nobody authoring a manifest can know. The date exists because a free-text price is the one number here that nothing can re-measure: every other figure comes from a real boot (resolved digests, measured storage), while this one is a market observation that goes stale in silence. The full authoring rules — wording, currency, why a per-token rate is never rewritten as per-word — live with the curation source that authors these files (store `docs/app-description.md` # External costs), which also enforces the house style this package does not (no em dashes, no vendor names).

### B. Runtime

The minimum to actually launch the thing.

```yaml
compose_file: docker-compose.yml      # standard compose; never modified by moose
main_service: photoprism              # which compose service is "the app"
main_port: 2342                       # port the main service listens on internally
preferred_slugs: [photos, photoprism] # subdomain priority list; OS picks first free
needs_secure_context: false           # optional; default false. See below.
timezone: system                      # optional; "system" (default) or "utc"
health_probe: /healthz                # optional; enables the "responding" check. See below.
service_user: false                   # optional; default false. Dedicated non-root identity for folderless apps. See below.
```

**`id` and `preferred_slugs`** must be strict kebab-case — lowercase alphanumerics joined by single internal hyphens (`home-assistant` ✓; `whoami-`, `-x`, `who--ami`, `xn--y`, `Foo` ✗). This keeps the `<slug>--<user>` personal-instance scheme parseable (`DASHBOARD.md` # instance naming): no leading/trailing hyphen and no `--` run (which would collide with the owner separator and also covers the reserved `xn--` prefix). Catalog CI and the manifest parser both reject violations.

**`timezone`** controls the container's TZ. Default `system` — the brain bind-mounts `/etc/localtime` and sets `TZ=<system_tz>`, so timestamps in app UIs match the user's wall clock. Set `utc` for apps that prefer UTC internally (databases, queues, anything that explicitly normalizes on UTC). Full model in `TIME.md`. Most apps should leave this unset.

The compose file is held **verbatim**. Authors test it with `docker compose up` and it behaves identically inside moose. Moose configures the surrounding environment; it does not edit the compose file.

**Image references in compose use version tags, not digests.** Authors write `image: photoprism/photoprism:2.4.1` — readable, portable, the same line that runs outside moose. For **store apps**, moose's catalog CI resolves each `image:tag` to a specific `sha256:` digest at publish time and writes it into the published catalog (`APP_STORE.md` # Trust model). The brain pulls by digest derived from the catalog — the version tag is the author's API, the digest is the bytes-binding. For **Door-2 custom apps**, the brain falls back to trust-on-first-use: pull, resolve digest, pin in the override.

**`needs_secure_context`** signals that the app relies on browser APIs gated on a [secure context](https://developer.mozilla.org/en-US/docs/Web/Security/Secure_Contexts) (camera, mic, clipboard, service workers, PWA install, secure cookies, WebAuthn). It's an **author-provided hint**, used by the brain to warn the user at install time — not a routing instruction.

- `needs_secure_context: false` (default): no special treatment.
- `needs_secure_context: true`: at install time, if the user's current URL scheme is `.local` (toggle off or not enrolled), the install dialog warns *"This app uses features that need HTTPS — they may not work at the `.local` URL. Turn on secure URLs in Settings → Network."* The user can install anyway.

The field is **never a routing override.** The URL each app gets is determined entirely by the global "Use secure URLs" toggle in Settings — see `MOOSE_NETWORK.md`. App authors should set this honestly: many apps work fine on HTTP and shouldn't set it; apps that genuinely depend on a secure-context API should.

Previously this field was named `requires_https` and gated install on un-enrolled boxes. Changed 2026-05-14 — see `DECISIONS.md`.

**`health_probe`** opts the app into moose's *"up but not responding"* detection. It is **not** Docker `HEALTHCHECK`: moose holds the compose file verbatim and cannot add a healthcheck for the author, so the probe is declared here, in moose's contract, and executed by the brain. Absent (the default), the app is never probed and the `app-unresponsive` health issue is never raised for it — least surprise for the bulk of the catalog. Shorthand `health_probe: /healthz` expands to `{ path: /healthz }`; the full form:

```yaml
health_probe:
  path: /healthz                    # HTTP path to GET (required when the block is present)
  healthy_status: [200]             # optional; default: any status < 500
  start_period: 60s                 # optional; grace after container start before probing (default 60s)
```

When set, the brain probes the app on its health-poll tick and raises the **non-blocking** `app-unresponsive` warning (`HEALTH.md` # Version, Tier-2 action: view logs / restart) when the probe fails. Three things to know, all owned by `HEALTH.md` # Detector catalog:

- **No `port` field.** The probe targets the app's existing route (which already points at `main_service:main_port`) — it goes *through Caddy* with `Host: <slug>`, exactly like a browser request, not by the brain dialing the container. This is a security call: it keeps the brain (the control plane) off every app-reachable Docker network. See `DECISIONS.md` 2026-06-02.
- **Default healthy = any status < 500**, i.e. "the server answered coherently." An app that returns `401`/`403`/`404` on the probe path is still *responding*; `5xx`, a timeout, or a connection failure (Caddy's `502`) is not. Authors with a real health endpoint can narrow to `[200]`.
- **`start_period`** is the grace after the container starts before the probe counts, so a warming-up app doesn't flap the banner on install/update.

Door-2 synthetic manifests omit it; a power user can add it later by editing the manifest, same as any other optional field.

**`service_user`** declares that the app writes its data as a **non-root** user and should run under a dedicated, moose-allocated service identity rather than the folderless default (the brain's euid — root in production). Set it for nginx+php-fpm / LinuxServer-style images whose processes drop to a service user and so can't write moose's root-owned `data/` dir. When `true`, the brain allocates a stable per-instance UID/GID from its reserved app-service band, pins the container `user:`, and chowns the instance `data/` dir to it (`APP_ISOLATION.md` # Runtime identity & data ownership).

The field is a **boolean intent, never a number** — you cannot name a UID; moose owns the value, precisely so a manifest can't alias a host principal (a numeric `user:` is an admission rejection). It is meaningful only for **folderless** apps: an app with `folders` already runs as a managed non-root identity (the owner, or the shared moose-app identity), so combining `service_user: true` with a folder grant is rejected. It does **not** help an image that hardcodes a *different* internal UID and ignores the runtime user (a php-fpm pool pinned to `www-data`, an entrypoint that `setuid`-drops to a fixed user) — that class waits on user-namespace remap (`APP_ISOLATION.md` # Not in v1).

### B2. Resources (recommended, never a limit)

The author declares **recommended** specs — advice only, never a ceiling.

```yaml
resources:
  recommended:
    memory: 512M
    cpu: 1.0
```

Used for the install-time capacity check ("you have 800M free; this wants 512M; fine") and store display/sorting. **There is deliberately no `limit` field** — the author can't see the user's hardware, so a manifest-imposed cap would throttle legitimate usage peaks. Apps run with no cgroup cap by default and burst freely; limiting is the *user's* call (an optional per-app **memory** cap in the UI, default off), and the brain protects its own control plane via OOM priority rather than caps. Full runtime model — including why CPU is never capped and how the brain stays alive under memory pressure — is owned by `APP_ISOLATION.md` # Resource limits.

### C. Storage

Two distinct kinds of storage in every app — user content and app state. See `STORAGE.md` # Files are first-class.

**User content** is what the user owns — photos, music, notes, documents. Lives at `/home/<user>/Photos/`, `~/Music/`, etc. Apps reach it by **bind-mounting use-case folders**, declared in `permissions.folders` (next section). Survives app uninstall.

**App state** is the app's own working data — indexes, caches, databases, configs. Lives at `/var/lib/moose/state/instances/<id>/data/`. Opaque to the user. Deleted on uninstall (or archived if the user picks "keep data").

The `storage:` block configures app state only.

```yaml
storage:
  data_volumes:                       # app state to back up (indexes, configs, app DB)
    - ./data/index
    - ./data/config
  cache_volumes:                      # transient app state → excluded from backup
    - ./data/cache
    - ./data/thumbnails
  tier: fast                          # fast | normal | any  (default: any)
  estimated_size: 10GB                # measured app-state on disk right after install
  app_managed_user_content: false     # opt-in; see "Apps that manage their own content tree"
```

**`data_volumes` vs `cache_volumes`** — the backup system uses this. Cache is regeneratable; data isn't. Without this distinction, we'd back up thumbnail caches. A `cache_volumes` path **may be nested inside a `data_volume`** — the common shape is a single `./data` bind with a `cache/` subdirectory (e.g. a downloaded model or an embeddings store the app re-fetches on demand): list `./data` under `data_volumes` and `./data/cache` under `cache_volumes`, and backup is the data tree **minus** the cache subtrees. Paths that don't nest must not otherwise overlap. (Note both lists are author-grade declarations: v1 parses neither into the Go struct yet — see `docs/dev/authoring-apps-with-an-agent.md` — so they document intent for the backup system that will consume them.)

**`estimated_size` is the *app-state baseline at install* — measured, not a usage projection.** It is the size of the app's own working data (indexes, databases, configs) under `/var/lib/moose/state/instances/<id>/data/` **as it stands the moment install completes** (the main service first reports healthy), on a clean install. It is deliberately *not* a guess at how big the app might grow with use: if the app later downloads another model or the user uploads a 2 GB library, that growth is **not** counted here — that's a runtime disk-pressure concern (`HEALTH.md` # `disk-full`), not a pre-install figure. The goal is a number close to the real on-disk cost of *having installed* the app; undercounting (a first-boot download still in flight when the health probe passes) is acceptable, overcounting by speculating about use is not. It is **not** the container-image size and **not** the user's content (Photos/Music/Documents the app bind-mounts — that is first-class, unbounded, and survives uninstall, so it is never attributed to the app). Image size is **not** author-declared: the catalog build resolves it from the actual pinned images at publish time (`APP_STORE.md` # Catalog schema). The brain combines the two — image size + `estimated_size` — into the **on-disk footprint** it shows before install (store card + consent dialog; `BRAIN_UI_PROTOCOL.md` # GET /api/v1/catalog/:id/install-plan, `DASHBOARD.md` # Install authorization). It stays advisory: warn on a tight disk, never block.

**How to measure it (authoring).** Don't estimate — measure. The import smoke-test already boots the app on a clean install and waits for the health probe; at that point `du -sb` the instance's `data/` volumes and record the result. Apps with a live-boot test can assert the figure against drift on version bumps. Full per-app CI measurement isn't required (some apps need managed services to boot); the author-time measurement during import is the source.

**`app_managed_user_content: true`** is the opt-in for apps that genuinely can't expose user content via use-case folders (legacy apps with opaque libraries). Triggers an install-time warning to the user: *"This app stores your files in its own folder, not your moose Photos/Music/Documents. You'll need this app to access them."* The moose store prefers apps that don't set this; curation policy may reject third-party manifests that do (TBD, `NEXT.md`).

**Bind mounts only — no Docker named volumes.** All app state lives under the instance's `data/` directory via bind mounts. Compose uses `${MOOSE_DATA_DIR}/foo:/foo` (absolute) or `./data/foo:/foo` (relative to the project dir). One backup root, one disk-usage view, one mental model. See `APP_LIFECYCLE.md` # on-disk layout per instance.

### D. Managed services

The "OS as platform" bet made concrete. Apps declare what infra they need; the brain provisions it.

```yaml
services:
  database:
    type: postgres
    version: "18"                     # version pin
    name: photoprism_db               # logical name within this app
  cache:
    type: valkey
    version: "8"
```

The brain provisions the resource (e.g., creates a database in the shared Postgres-18 instance with a scoped user) and **injects credentials as environment variables**.

Available types and versions (`SERVICE_PROVISIONING.md` # Catalog (v1)): `postgres` (15, 16, 17, 18 — new manifests should declare **18**; 15 is deprecated but still accepted), `mysql` (8.0, 8.4), `mariadb` (10.11, 11.4), `valkey` (8). `redis` (7) is accepted as a **compatibility alias for `valkey`** — it always provisions the BSD-3 Valkey engine underneath, never upstream Redis (`DECISIONS.md` 2026-06-13); new manifests should prefer `valkey`. A type/version outside this set is rejected at manifest parse time. The MySQL family injects port 3306 and a `mysql://` DSN for both engines (one wire protocol); Valkey injects port 6379 and a `redis://` DSN (the universal RESP scheme). There is no `extensions:` key: an app installs trusted Postgres extensions itself (its role owns its database), and anything needing an untrusted or third-party extension bundles its own engine — `SERVICE_PROVISIONING.md` # Database extensions.

**Naming convention: app-defined.** The moose brain exposes the credentials under stable, documented variable names (e.g., `MOOSE_SERVICE_DATABASE_HOST`, `MOOSE_SERVICE_DATABASE_USER`, `MOOSE_SERVICE_DATABASE_PASSWORD`, `MOOSE_SERVICE_DATABASE_NAME`, `MOOSE_SERVICE_DATABASE_DSN`). The app's compose file maps these to whatever variables the app actually expects:

```yaml
# inside the app's docker-compose.yml
environment:
  PHOTOPRISM_DATABASE_DSN: ${MOOSE_SERVICE_DATABASE_DSN}
```

This means the app is the one doing the wiring. The app remains portable (still runs outside moose with a manually-set env var). It's a small adaptation — well-documented, explicit, no magic.

Apps that don't trust managed services can simply ship their own database in their own compose file. **Both paths work**; the manifest path is encouraged but not enforced.

### D2. Generated secrets

Many apps require a random, app-specific secret to sign auth tokens, sessions, or cookies — `BETTER_AUTH_SECRET` (Better Auth), `SECRET_KEY_BASE` (Rails), a JWT/HMAC signing key. The author can't ship a value (a public catalog secret signs nothing securely), and the non-technical user can't be asked to generate one. So the manifest *declares* the need and the brain generates the value.

```yaml
secrets:
  - name: auth          # → injected as MOOSE_SECRET_AUTH
  - name: session_key
    bytes: 64           # entropy drawn before encoding; default 32, floor 16
```

At install the brain draws each secret from a CSPRNG, base64url-encodes it (32 bytes → a 43-char string, past the "32+ char" bar most libraries want), and injects it as `MOOSE_SECRET_<NAME>` (uppercased). The app's compose maps that to whatever variable the app actually expects — the same app-defined wiring as `MOOSE_SERVICE_*` and `MOOSE_FOLDER_*`:

```yaml
# inside the app's docker-compose.yml
environment:
  BETTER_AUTH_SECRET: ${MOOSE_SECRET_AUTH}
```

**The value is generated once and stays stable** for the life of the instance — it is persisted and re-emitted on every restart, never re-rolled, because a token-signing secret that changed underneath the app would invalidate every live session. `name` is lowercase snake_case (so the uppercased env-var suffix is unambiguous); names are unique within a manifest. See `SERVICE_PROVISIONING.md` # Env-var injection.

**`show: true` surfaces a secret's value to the instance owner** (and admins) on the app detail page (`DASHBOARD.md` # Installed apps), gated to the same owner-or-admin rule as the app's controls. Set it for a *bootstrap* credential the user must read to finish first sign-in — a self-authenticating app's setup token — so the manifest never has to ship a published constant as its fallback. Omitted (the default) keeps a secret internal: a managed-service password the app consumes but the user never needs is never revealed, so a single reveal can't expose every injected credential.

```yaml
secrets:
  - name: setup_token   # owner reads this once to set a password
    show: true
  - name: auth          # internal: signs sessions, never shown
```

### D3. Outgoing mail

Apps that can *send* email — password resets, reminders, invites — declare it, and the admin decides at install (or later) which of the box's registered SMTP providers the app sends through (`SERVICE_PROVISIONING.md` # BYO outgoing mail).

```yaml
mail:
  optional: true       # the app must run fine unbound (email features off)
```

When the instance is bound to a provider, the brain injects `MOOSE_MAIL_HOST/_PORT/_USER/_PASSWORD/_FROM/_ENCRYPTION` plus a Symfony-style `MOOSE_MAIL_DSN`; unbound, **nothing is injected**. The compose maps the vars app-defined as usual, with a default for the unbound case so absence degrades to the app's own "email off" mode:

```yaml
# inside the app's docker-compose.yml (Kimai)
environment:
  MAILER_URL: "${MOOSE_MAIL_DSN:-null://null}"
  MAILER_FROM: "${MOOSE_MAIL_FROM:-kimai@example.com}"
```

v1 admits only `optional: true` — an app that *can't* run unbound (`optional: false` or a bare `mail: {}`) is rejected at parse, because a box with no registered providers couldn't install it. Required-mail semantics (blocking install until a provider is picked) is a possible later loosening; declare-and-degrade is the v1 contract.

### D4. User-supplied configuration

Some apps need a value only the *user* can provide — an API token for a third-party provider (`OPENAI_API_KEY`, a PlanetScale auth token), an external account's connection string, a region or model selector. The app already reads these from environment variables, but the author can't ship a value (it's the user's own credential) and the brain can't generate one (unlike a `secret:`, it has external meaning). Without a surface for it the app installs into a useless or crash-looping state — this is the single largest class of apps the catalog rejects today, tracked as the `operator-env-config` gap (`docs/dev/catalog-import-gaps.md`). The `config:` block is that surface: the manifest declares the fields, the brain renders a form, and the user's answers are injected into the app's environment.

```yaml
config:
  - app_env: OPENAI_API_KEY          # the app's own env var — the field's identifier
    title: "OpenAI API key"
    description: "From your account at platform.openai.com; lets this app call OpenAI on your behalf."
    secret: true                     # masked input; stored + handled like a secret, never logged
    required: true                   # blocks install until provided
  - app_env: OPENAI_MODEL
    title: "Model"
    description: "Which model the app uses for new chats."
    type: enum                       # text (default) | enum | bool
    options: ["gpt-4o", "gpt-4o-mini"]
    default: "gpt-4o-mini"
```

**The value is injected under the app's own variable name — there is no `MOOSE_*` indirection.** `app_env` is *both* the form's technical hint and the variable the brain sets: with `app_env: OPENAI_API_KEY`, the brain stamps `OPENAI_API_KEY=<the user's answer>` straight into the app's environment. No compose mapping line is needed, and nothing in the compose has to change. This is a deliberate divergence from the rest of the injected family (`MOOSE_SERVICE_*`, `MOOSE_SECRET_*`, `MOOSE_FOLDER_*`, `MOOSE_MAIL_*`), which exists *because* the brain owns those values and the app must adapt to receive them. A user-supplied config value is not moose's to own — it is the app's own native variable that the user would set by hand when running the app standalone — so the indirection buys no portability here, and injecting `app_env` directly means the field the user reads in the app's upstream docs (`OPENAI_API_KEY`) is exactly the name shown on the form and exactly the name set in the container. See `DECISIONS.md` 2026-06-26 and `SERVICE_PROVISIONING.md` # Env-var injection.

**Where it lands.** The brain writes each provided value into the target service's `environment:` in the compose override it already stamps (the same override that pins `user:` and `cap_drop`), not through the interpolation `.env` the `MOOSE_*` family uses. The target is `main_service` by default; a field may name a different service (or one shared by a worker and a web tier) with `service:`. Compose merges override `environment:` over the base, so a value the manifest sets wins over any placeholder in the author's compose.

**Reserved names — the collision rule.** Because the value lands directly under `app_env` and the override *wins* over the base compose, `app_env` is a security boundary: an unconstrained name could overwrite a runtime-critical or brain-owned variable. So `app_env` is rejected at manifest parse (and re-checked at admission, defense in depth) when it (a) begins with the **`MOOSE_`** prefix — that namespace is the brain's injected family (`MOOSE_SERVICE_*`, `MOOSE_SECRET_*`, `MOOSE_FOLDER_*`, `MOOSE_MAIL_*`, `MOOSE_APP_URL`, `MOOSE_DATA_DIR`, `MOOSE_INSTANCE_ID`), never user-settable — or (b) matches a reserved **loader/runtime denylist** (`PATH`, `HOME`, `USER`, `SHELL`, `HOSTNAME`, `IFS`, and the dynamic-linker set `LD_PRELOAD` / `LD_LIBRARY_PATH` / `LD_AUDIT`). A user-config field names an *app's own* configuration variable; it never reaches process or platform internals. The denylist lives with the validator and grows if a new injected family or sensitive loader var appears (`THREAT_MODEL.md`).

**Field shape.**

- **`app_env`** (required) — the app's environment-variable name. Unique within the manifest, a valid env-var identifier (`[A-Z_][A-Z0-9_]*` by convention), and not a reserved name (# Reserved names above). It is the storage key, the form hint, and the injected variable.
- **`title`** (required) — the human label on the form ("OpenAI API key"). The user may not recognize `OPENAI_API_KEY`; the title + description bridge to it, and the monospace `app_env` is shown beneath so a user reading the app's own docs can confirm the match.
- **`description`** (required) — one or two sentences: what it is, and where to get it.
- **`secret`** (default `false`) — when set, the input is masked, the value is stored and handled like a `MOOSE_SECRET_*` value (never logged, never echoed into compose output; folds into `NEXT.md` # App-secret injection hardening), and the post-install editor shows "set" with a **replace** affordance rather than revealing it. A `secret` field may not carry a `default` — a published default for a credential defeats the point.
- **`required`** (default `false`) — a required field blocks the **install button** until the user supplies it, so the app never installs into a guaranteed crash-loop. An optional field left blank injects **nothing** — the app falls back to its own compose default and degrades gracefully, the same declare-and-degrade contract as `mail: {optional: true}`.
- **`type`** (default `text`) — `text` | `enum` | `bool`. `enum` requires `options` (a non-empty list of allowed strings, rendered as a select); `bool` renders a toggle and injects `true`/`false`.
- **`options`** (enum only), **`default`** (prefill / value-when-blank; not allowed on `secret`), **`service`** (target compose service; default `main_service`).
- **`role`** (optional): what the field means, so the setup page can fill it from a provider account instead of showing a text box (# Roles and requires below).
- **`separator`** (optional): how a `models.<type>` list is joined (# Roles and requires below).

**Roles and requires.** A field can say what it *means*, not only which variable it sets. A manifest can also say that the app needs at least one of several fields. The design is in `INSTALL_SETUP.md` # 1 to # 3; this is the schema.

```yaml
config:
  - app_env: ANTHROPIC_API_KEY
    title: "Anthropic API key"
    description: "..."
    secret: true
    role: ai.anthropic.api_key
  - app_env: CUSTOM_BASE_URL
    title: "Custom provider base URL"
    description: "..."
    role: ai.openai_compatible.base_url
  - app_env: CUSTOM_MODELS
    title: "Custom provider models"
    description: "..."
    role: ai.openai_compatible.models.chat
    separator: ";"
requires:
  - one_of: [ai]                                  # any AI provider
  - one_of: [GOOGLE_API_KEY, GOOGLE_CREDENTIALS]  # plain fields, by app_env
```

- **`role`** is `<kind>.<protocol>.<attribute>`, in lowercase segments (`[a-z][a-z0-9_]*`). **kind** is a closed list: only `ai` today. A kind is added when moose can fill it. **protocol** is open: `openai_compatible` is reserved for the generic slot that any provider with an OpenAI-compatible base URL can fill, and any other value is a native protocol, matched against a provider's `native_protocol`. **attribute** is a closed list: `api_key`, `base_url`, `model.<type>` or `models.<type>`, where the type is one of `chat`, `embedding`, `image`, `speech_to_text`, `text_to_speech`, `rerank`. The value still lands under `app_env`, so no compose file changes. Fields that share `kind.protocol` form a **slot**.
- **`separator`** is valid only on a `models.<type>` field. Default `,`. It is 1 to 4 printable characters, with no newline, no `=` and no quote.
- **`requires`** is a list of `one_of` groups. A group is met when at least one of its fields has a value after the brain resolves the install. A member is a kind (`ai`, one lowercase segment), a slot (`ai.anthropic`, two segments) or a plain field by its `app_env` (uppercase). A kind or slot member matches every field whose role starts with it. Model fields (`model.*`, `models.*`) never count, because a model alone does not reach a provider. "At least one of" is the only rule for now.
- **How a slot is filled.** On the setup page the user picks a provider tile and one of their AI accounts for it, then the models. The install sends that as a binding (`BRAIN_UI_PROTOCOL.md` # POST /api/v1/apps `config.ai_bindings`), and the brain writes every field of the slot from it: the key, the base URL (for the compatible slot, the provider's OpenAI-compatible address unless the account has its own), and the model ids, a `models.<type>` list joined with `separator`. A native slot takes a provider whose `native_protocol` is the slot's protocol. The compatible slot takes an "Other" server or any provider with an OpenAI-compatible address. A slot that no provider can fill shows its fields as plain fields, and plain values for role fields are still accepted.
- **The brain enforces `requires`.** `POST /api/v1/apps` answers 422 while a group is unmet. Editing an installed app's settings may not make a met group unmet; a group that already fails (an app installed before `requires`) does not block an unrelated edit (`BRAIN_UI_PROTOCOL.md` # Pattern B).

**Leniency: the box never refuses a manifest over these keys.** `manifest.Parse` is the only manifest parser, and the brain runs it on every catalog manifest and on every installed app's stored manifest. So none of the rules above are in `Parse`. The box reads the keys leniently (`internal/manifest/roles.go`): a field whose role it cannot fill (malformed, unknown kind, attribute or model type, a separator that is invalid or not on a `models` field, a key field that is not `secret`, a field that is not `type: text`, or a role repeated from an earlier field) is shown as a plain field. A `requires` member that matches no field is dropped, and so is a group left empty or an entry it cannot read. The catalog logs what it dropped when it loads the manifest. The strict rules are in the lint (`internal/manifest/lint.go`), which only `moose manifest lint` and `check` run: they fail on a bad role or separator, a repeated role, a key field without `secret: true`, a role field with a type other than text or with `options`, `default` or `required: true`, a slot with only model fields, an `openai_compatible` slot without `base_url`, a `requires` member that matches no field or names a role, a repeated member, an empty group, a plain field that is both `required: true` and a group member, and a role-tagged field named by its `app_env` in a group. They warn, without failing, when a kind-level group covers slots that need different model types, when a group has a single plain field (use `required: true`), and, with `--ai-providers <path>`, when no provider offers a native protocol.

**`manifest_version` stays 1.** The keys are optional, and older boxes ignore them: `yaml.Unmarshal` is non-strict, so an older box shows every field as a plain field and checks no groups, as before.

**Lifecycle — two entry points, both required.** Config is collected in the install consent form (`DASHBOARD.md` # Install authorization) *before* the first `compose up`, because several apps exit immediately without their token. It is also editable after install on the app detail page (`DASHBOARD.md` # Installed apps), owner-or-admin gated like every other app control; saving rewrites the override and restarts the app. Tokens expire and rotate, so post-install editing is not optional.

**Door 2 (custom paste) has no `config:` block.** A pasted third-party compose already carries (or hardcodes) its own env; the admin sets values directly in the compose via the install form's **Edit as YAML** path (# Custom container — synthetic manifest). `config:` is a Door-1 authored convenience, the same way `target` on folder grants is Door-2-only in the other direction.

### E. Permissions and capabilities

What the app is allowed to touch. Default is "very little"; manifest opts in to specific things.

**Granularity: medium.** Not coarse-grained-only (leaves real attack surface), not fine-grained Kubernetes-style (rabbit hole non-technical users would never understand).

```yaml
permissions:
  internet: true                      # outbound internet allowed
  lan: false                          # can talk to LAN devices (macvlan; see APP_ISOLATION.md)
  folders:                            # access to use-case content folders (see below)
    - { folder: photos, mode: write }
    - { folder: movies, mode: read }
  devices: [/dev/ttyUSB0]             # explicit device paths (Zigbee/Z-Wave dongles, webcams)
  gpu: true                           # platform-appropriate GPU runtime (NVIDIA / Intel / AMD)
  network_isolation: per_app          # per_app | shared
```

Permissions are **declared and enforced.** Not metadata — the brain actually configures Docker networks, bind mounts, and devices to match. Apps cannot reach what they didn't declare. The concrete Linux/Docker primitive behind each field is owned by `APP_ISOLATION.md` # Capabilities & privilege; this doc is the schema, that doc is the enforcement.

Store review checks the declared permissions match the app's actual usage.

**`devices` and `gpu`.** `devices` lists explicit `/dev/...` paths the app needs passed through (a Zigbee dongle, a webcam); the brain validates each exists before start. `gpu: true` is **separate from `devices`** because driver wiring is platform-specific — the OS selects the right runtime (NVIDIA container runtime, Intel/AMD `/dev/dri`) and the app introspects what's present via standard tooling; if no GPU exists the install fails at the capacity check.

**No added Linux capabilities for store apps.** The brain's override is `cap_drop: [ALL]` and adds none; admission rejects any `cap_add` (`APP_LIFECYCLE.md` # admission policy). Apps that genuinely need a capability, `privileged`, or the Docker socket do **not** get there through Door 2 — admission is door-symmetric and refuses those for custom compose exactly as for store apps (`APP_ISOLATION.md` # Trust tiers, `DECISIONS.md` 2026-06-02). They run as curated OS integrations (**Tier 2**, `SERVICE_PROVISIONING.md`) or the admin runs them over SSH. A raw-capability escape hatch *in a store manifest* is intentionally absent. (`APP_ISOLATION.md` sketches a reviewed `permissions.capabilities` list; that is not part of the v1 store schema and is tracked as an open item — see `NEXT.md`.)

#### `folders` — access to use-case content

How an app reads/writes content in the use-case folders (`STORAGE.md` # What apps and users actually see). For each folder, declare the folder name, mode, and subfolder scope:

```yaml
permissions:
  folders:
    - folder: photos                  # photos | music | movies | documents | notes | downloads
      mode: write                     # read | write  (default: read)
      scope: whole                    # whole | pick-subfolder  (default: whole)
    - folder: notes
      mode: write
      scope: pick-subfolder           # user picks the subfolder at install
      default: Notes/Obsidian         # default subfolder; user can override
```

- **`mode`** defaults to **`read`** when unspecified — least privilege, and `write` is a deliberate choice the catalog reviewer notices. `mode: write` shows up on the install screen as "this app can ADD, CHANGE, AND DELETE files in your X folder" — read-only declarations are visibly different.
- **`scope: whole`** (default) — brain bind-mounts the entire folder (e.g., all of the chosen `Photos/`) into the container.
- **`scope: pick-subfolder`** — install screen prompts the user: "Which folder should this app manage?" Default is the manifest's `default` (auto-created if absent), user can choose any path under the folder. Used for notes apps (one vault per "context"), media apps that should manage a subset of a library, etc.

**Source is the installer's choice, not the author's.** The manifest declares *what* content the app touches and *how* (`mode`/`scope`); it deliberately does **not** declare whether the folder is the user's **personal** `~/<Folder>/` or the **household-shared** `/srv/moose/shared/<Folder>/`. The author can't know a given household's intent — "I want *my own* Jellyfin on *my* movies" and "I want it on the *family* library" are both valid and the app code is identical. So source is elected per folder at install (`DASHBOARD.md` # install authorization, `DECISIONS.md` 2026-05-30):

- **Personal instance** — the install screen offers, per folder, **your `<Folder>`** (default) or the **household Shared `<Folder>`**. Choosing shared adds the container to the `moose-shared` group; it reaches exactly what the owner can already reach as a household member.
- **Household instance** — always the household Shared `<Folder>` (a shared instance has no single owner whose `~/` it could bind). No per-folder toggle.

This supersedes the earlier `user_folders` / `shared_folders` split, where the author picked the source by choosing the key.

**How it's mounted — fixed path + injected env var.** The brain bind-mounts each declared folder at a stable, documented path — `/moose/<folder>` (e.g. `/moose/photos`) — and injects the absolute path as `MOOSE_FOLDER_<NAME>` (e.g. `MOOSE_FOLDER_PHOTOS=/moose/photos`). The app's compose maps that variable to whatever the app actually expects:

```yaml
# inside the app's docker-compose.yml
environment:
  PHOTOPRISM_ORIGINALS_PATH: ${MOOSE_FOLDER_PHOTOS}
```

This is the same injection convention as managed services (`MOOSE_SERVICE_*`) and `MOOSE_DATA_DIR` — the manifest stays declarative about *intent*, the app does the wiring, and the app stays portable. The in-container mount path and the env var are stable regardless of the elected source or subfolder; only the **host source** varies (personal `~/<Folder>/` vs shared `/srv/moose/shared/<Folder>/`, narrowed further by a `pick-subfolder` choice). The source side is resolved by the brain at install time — it learns the owner's home path and UID from host-agent (`BRAIN_HOST_PROTOCOL.md`), never declared by the author.

#### External-storage convention for popular apps

The moose-tuned manifest for an app whose upstream supports external libraries (Immich, Photoprism, Jellyfin, Nextcloud, Navidrome, ...) declares `folders` and configures the app via env vars or post-install steps to **point its internal "library path" at the bind-mounted use-case folder**. The user's files stay at `~/Photos/`, the app indexes them there, uninstalling the app keeps the files. This is the path that earns the manifest a "files first-class" badge in the store.

Apps that don't support external libraries fall back to `storage.app_managed_user_content: true` (`STORAGE.md` # Files are first-class). For v1, the store catalog is hand-curated by moose — we write manifests that follow the external-storage pattern wherever upstream supports it.

**No `cap_add` for store (Tier-3) apps.** The brain's override drops ALL capabilities and adds none. Apps that genuinely need Linux capabilities (VPN clients, FUSE mounts, raw sockets) belong in Tier 2 — OS integrations curated by moose with a separate install path. See `SERVICE_PROVISIONING.md`. If a Tier-3 compose declares `cap_add`, the brain refuses to install it.

### E2. Access — path-scoped exceptions to the box login

Hosted boxes put a **box login in front of each app** by default, and the owner flips a per-app toggle between "Only me" and "Public" (`ENVIRONMENT.md` # Per-app owner-only access). That is a whole-app decision, and some apps do not fit it: a developer tool often pairs a **token-authed API** with a **session-authed UI**, so letting an external SDK reach the API means making the whole app public, which drops the box login in front of the UI too. Langfuse hit this wall first; Laminar hit it worse, because its self-hosted UI signs in any email with no password.

The manifest may therefore declare paths that stay open even while the app is owner-only:

```yaml
access:
  public_paths: ["/v1", "/v1/*"]   # served anonymously; the rest keeps the box login
```

- **It is an exception, not a second exposure switch.** The app stays "Only me". Only the listed paths skip the gate, and only on hosted — an appliance app is public anyway, so the field changes nothing there.
- **Two shapes, nothing else:** an exact path (`/v1`) or a path and everything under it (`/v1/*`). `/`, `/*` and `/**` are rejected outright: they would make the whole app public while the dashboard still offers the owner an access toggle for it. A mid-path or bare-suffix wildcard is rejected too, because `/v1*` also matches `/v1admin` — an author reaching for "everything under /v1" would open a sibling path they never read. At most 16 entries, 128 characters each.
- **Declare the plain, decoded path.** `%`, `?`, `#`, `\`, `//` and `..` are rejected. The proxy matches a cleaned, decoded path while the app sees the original URI, so a declaration containing any of those means two different things on the two sides.
- **Matching is case-insensitive**, so `/v1/*` also opens `/V1/x`. Entries that differ only by case are duplicates.
- **`/v1/*` does not match `/v1` itself.** Declare both when the API answers on the bare prefix.
- **The author does not get identity from a public path.** moose's vouched identity headers are stripped from every inbound request on every hosted app route, so a caller can never forge them — see `ENVIRONMENT.md` # Per-app owner-only access.
- **The trust boundary is catalog review**, not this validation. These paths ship in the catalog, which moose curates; the rules above catch an author's mistake, and a manifest that wanted to be hostile already chooses the app's images. A reviewer should ask one question: is every declared path authenticated by the app itself?
- **The dashboard says so.** An app that declares public paths shows them on its access control instead of a bare "Only you can open it" (`DASHBOARD.md` # Settings).

### F. Lifecycle hooks — deferred from MVP

The `hooks:` block is **not part of v1.** Apps already run their own migrations on container start; the brain's **pre-update snapshot** (`UPDATES.md` # Pre-update snapshot) is the v1 safety net for migrations that go wrong.

When hooks return, they will be designed as **one-shot container images** rather than in-container scripts:

```yaml
# Sketch, not v1 syntax
hooks:
  pre_update:          { image: photoprism/migrator:2.4.1 }
  post_update_rollback: { image: photoprism/migrator:2.4.1, args: ["rollback"] }
```

The brain will run the hook image as a transient container with the app's volumes attached. This respects closed-source images (no shell-in-app-container required) and gives commercial vendors a clean integration path. `pre_update`, when supplied, replaces the brain's brute-force tar for that app — the snapshot remains the default for apps without a hook. `post_update_rollback` fires only when the update fails after the new container started; it's the right shape for apps with bespoke recovery (e.g., a destructive schema migration that needs explicit reversal). Tracked in `APP_LIFECYCLE.md` # "Deferred: lifecycle hooks".

### G. Multi-user behavior — not a manifest concern

**The manifest does not decide how an app is shared across accounts.** Whether an instance is *household* (one shared instance, app-internal multi-user separates people inside it) or *personal* (one instance per owner, binding only the owner's folders) is **elected by the installing user**, not declared by the author (`DASHBOARD.md` # instances are owner-scoped, `DECISIONS.md` 2026-05-29):

- An **admin** chooses Household or "Just for me" at install.
- A **member** can only create a **personal** instance.
- Every user sees their own personal instances plus the household instances they're permitted to open.

There is deliberately **no `multi_user.mode` field** — an earlier draft had `shared | per_user`, removed because scope is a runtime election, not a static property of the app. The brain realizes a personal instance as the per-owner compose-project shape already locked in `APP_LIFECYCLE.md` # an app instance is a Docker Compose project; folder bindings resolve to the owner's `~/<Folder>/`.

Mesh-guest sharing (`guest_shareable`) and per-app household visibility (which members may open a given household instance) are **deferred** — guest sharing rides on the mesh (deferred), and the visibility/authorization model is owned by `AUTH.md` / `DASHBOARD.md`, not the manifest. Neither is a v1 manifest field.

## Complete sample manifest

PhotoPrism, end-to-end:

```yaml
id: photoprism
manifest_version: 1
name: PhotoPrism
version: 2.4.1
description:
  short: "Self-hosted photo library with AI tagging"
  long: |
    PhotoPrism is an AI-powered app for browsing, organizing, and sharing your photo collection without giving up control. It indexes your existing files, generates thumbnails, and uses TensorFlow to tag people, places, and subjects automatically. Works great alongside the Files app — your originals stay in your Photos folder and are never locked inside PhotoPrism.
icon: ./icon.png
categories: [media, photos]
author: { name: "PhotoPrism Labs", url: "https://photoprism.app" }
license: AGPL-3.0

compose_file: docker-compose.yml
main_service: photoprism
main_port: 2342
preferred_slugs: [photos, photoprism]

resources:
  recommended: { memory: 1G, cpu: 2.0 }  # advice only; never a cap

storage:
  data_volumes: [./index, ./sidecar]    # app's own index/metadata
  cache_volumes: [./cache, ./thumbs]
  tier: fast
  estimated_size: 10GB

services:
  database: { type: postgres, version: "18" }

permissions:
  internet: true
  folders:
    - { folder: photos, mode: write }   # PhotoPrism reads/writes the chosen Photos folder
  gpu: true                             # hardware-accelerated thumbnails / transcode
```

~28 lines for a real-world app. The compose file is what the author already had. Whether this installs as a household or personal instance is the installer's choice, not declared here (# G).

## Custom container — synthetic manifest

User pastes a compose file, names the app, picks the main port, and **elects the app's permissions** in the install form (`DASHBOARD.md` # Door-2 custom container install flow). The brain generates:

```yaml
id: my-thing-x4f7                     # auto: name + entropy
manifest_version: 1
name: my-thing
version: custom
compose_file: docker-compose.yml      # the user's pasted file
main_service: <inferred or asked>
main_port: <inferred or asked>
preferred_slugs: [my-thing]

storage:
  data_volumes: [<all volumes from the compose>]
  # cache_volumes: empty by default — best-effort backup of everything
permissions:
  internet: true                      # default-on for custom apps; form toggle
  lan: false                          # form toggle
  gpu: false                          # form toggle
  folders:                            # empty by default; one entry per form row
    - folder: photos
      mode: read
      target: /photoprism/originals   # Door-2-only: explicit in-container path
```

**The `permissions` block is admin-elected in the form, not hardcoded.** `internet` (default on), `lan`, `gpu`, and any `folders` rows are authored through the install screen's permission controls; `devices` and managed `services` are the long tail, reached through the form's **Edit as YAML** escape hatch rather than dedicated fields (`DASHBOARD.md` # Form is a projection of the synthetic manifest). The form is a friendly projection of *this* manifest; the YAML toggle edits the same overlay raw. No managed services by default; best-effort backup of all volumes (we can't tell cache from data without the author's input); scope (household vs. personal) is the installer's election, not a manifest field (# G). The richer-manifest *graduate-in-place* path — editing an already-installed instance's manifest — is the intended future shape but **deferred past v1** (`DASHBOARD.md` # Edit-after-install is deferred); install-time authoring (form + YAML toggle) is not that deferred feature.

**Door-2 folder grants carry an explicit `target`.** Store-app folder grants declare no in-container path — the brain mounts each at `/moose/<folder>` + injects `MOOSE_FOLDER_<NAME>`, and the author maps that env var (# Locked: folders mount at a fixed path). A Door-2 paste has no author to adapt: the verbatim third-party compose hardcodes its data path, so the synthetic manifest's folder entry carries an explicit `target` (the destination the admin typed) and the brain binds the elected source straight there. The `target` field is **Door-2-only** — store manifests omit it and keep the fixed-path + env-var convention (`DECISIONS.md` 2026-06-02). The *source* (personal vs. household) stays the installer's per-folder election, exactly as for store apps.

**What the brain infers vs. asks (Door-2 paste).** `main_service` is **autodetected** when the compose has exactly one service, and **asked** otherwise (a dropdown of the compose's services). `main_port` is the *container-internal* port Caddy routes to — **best-effort inferred** from every signal the compose carries: a single `expose:` value, or the *container side* of a published `ports:` mapping (`8080:80` ⇒ `80`), mined out for the prefill before the mapping itself is rejected. It is **asked** only when the compose is silent (moose can't read the image's `EXPOSE` without pulling it) and is always editable. A published `ports:` is never *honored* (it's an admission rejection — Caddy fronts every app on internal networks); its container side is only read to prefill `main_port`. The full screen UX — where the flow lives, the permission controls and YAML escape hatch, inline admission-error coaching, the live URL preview, and the deferred edit-after-install path — is locked in `DASHBOARD.md` # Door-2 custom container install flow.

**Custom apps may request managed services.** Allowed, not encouraged. A power user pasting compose can manually add `services: { database: { type: postgres, version: "18" } }` and gets the same managed Postgres treatment. We document the path; we don't gate it.

## Locked decisions

- **Format: YAML.** `manifest.yml`.
- **Schema versioned from day one.** `manifest_version: 1`. Backward compatible for at least the previous two majors.
- **Most fields optional with sensible defaults.** Required: `id`, `manifest_version`, `name`, `version`, `compose_file`, `main_service`, `main_port`.
- **Compose file is verbatim.** Moose doesn't rewrite it.
- **`resources.recommended` is advice, never a cap.** No `limit` field exists in the manifest; authors can't see the user's hardware. Default runtime is uncapped burst; user-set memory caps and control-plane OOM protection live in `APP_ISOLATION.md` # Resource limits.
- **Permissions are declared and enforced.** Not just metadata.
- **User content vs. app state are separate stores.** User content (`/home/<user>/Photos/`, etc.) accessed by manifest-declared bind mounts of use-case folders; app state in `/var/lib/moose/state/instances/<id>/data/`. Apps reach user content by reference, never by copy.
- **`scope: pick-subfolder`** for `folders` — install-time prompt for apps that should manage a subset (notes apps, media subsets). Default is provided by the manifest; user can override.
- **Folder source (personal vs household-shared) is installer-elected, not a manifest field.** The manifest declares only the folder + `mode` + `scope`; whether it binds the owner's `~/<Folder>/` or the household `/srv/moose/shared/<Folder>/` is the installer's per-folder choice (personal instances pick, defaulting to personal; household instances are always shared). Replaces the old `user_folders` / `shared_folders` keys. See `DECISIONS.md` 2026-05-30.
- **`folders` mount at a fixed path + injected env var (store apps).** A store manifest declares folder + `mode` + `scope` but no in-container path; the brain mounts each at `/moose/<folder>` and injects `MOOSE_FOLDER_<NAME>`. The app's compose maps that variable to its own library path. `mode` defaults to `read`. Same injection pattern as `MOOSE_SERVICE_*` / `MOOSE_DATA_DIR`. **Door-2 custom apps diverge:** their verbatim compose has no author to map the env var, so a Door-2 folder grant carries an explicit `target` (the destination path the admin types) and the brain binds straight there. `target` is Door-2-only; store grants omit it (# Custom container — synthetic manifest, `DECISIONS.md` 2026-06-02).
- **`gpu` is its own field, separate from `devices`.** `devices` passes through explicit `/dev/...` paths; `gpu: true` selects the platform GPU runtime. No-GPU box fails at the capacity check.
- **`app_managed_user_content: true`** is the opt-in for apps that don't expose user content via use-case folders. Triggers an install-time warning. Curated store prefers apps without it.
- **`service_user: true`** opts a *folderless* app into a dedicated, moose-allocated non-root runtime identity — stable per instance, drawn from a reserved app-service band below the 3000 user floor; the brain pins `user:` and chowns `data/` to it. The manifest declares **intent only — no numeric UID is namable** (a host-namespace UID could alias a real principal under moose's no-userns-remap model); a numeric `user:`, or `service_user: true` alongside a `folders` grant, is an admission rejection. Does not cover images that hardcode a *different* non-root internal UID (deferred to userns-remap, `NEXT.md`). `APP_ISOLATION.md` # Runtime identity & data ownership, `DECISIONS.md` 2026-06-10.
- **Scope (household vs. personal) is installer-elected, not a manifest field.** No `multi_user.mode`. Admins choose household or personal; members install personal only (`DASHBOARD.md`, `DECISIONS.md` 2026-05-29). Guest-sharing and household visibility are deferred and not manifest fields.
- **No added Linux capabilities for store apps.** Override is `cap_drop: [ALL]`, adds none; admission rejects `cap_add`. Capability / `privileged` / Docker-socket needs go through Door-2 or Tier 2. A reviewed `permissions.capabilities` escape hatch is not in the v1 store schema (open in `NEXT.md`).
- **Bind mounts only — no Docker named volumes for app data.** All data lives under the instance's `data/` dir.
- **Hooks deferred from MVP.** When reintroduced, they will be one-shot container images, not in-container scripts.
- **`health_probe` is opt-in and moose-executed, not Docker `HEALTHCHECK`.** Optional `path` (+ `healthy_status`, `start_period`); the brain probes the app *through its Caddy route* on the health-poll tick and raises the non-blocking `app-unresponsive` warning (`HEALTH.md`) when it fails. Absent → no probe, issue never raised. Default healthy = any status < 500. Probing through Caddy (not by dialing the container) keeps the control plane off app-reachable networks. See `DECISIONS.md` 2026-06-02.
- **`needs_secure_context` is an install-time warning, not a routing override or install block.** Apps declare it honestly; the brain warns the user if the current URL scheme is HTTP. The URL each app uses is determined by the global toggle in Settings, not the manifest.
- **Public, versioned spec.** Third-party stores depend on it.
- **Env-var injection: app-defined naming.** App's compose maps moose's stable `MOOSE_SERVICE_*` variables to whatever names the app expects. No auto-rewrite. Authors adapt; we document.
- **Generated secrets are declared, brain-generated, and stable.** A manifest declares `secrets: [{name, bytes?, show?}]`; the brain draws each from a CSPRNG once at install, persists it, and injects it as `MOOSE_SECRET_<NAME>` — re-emitted verbatim on every restart so token-signing secrets don't rotate underneath live sessions. Same app-defined wiring as `MOOSE_SERVICE_*` (# D2). `show: true` makes one owner-visible on the app detail page (so a self-auth app's bootstrap token can be per-instance random, not a published constant — #152); omitted keeps it internal. Security hardening (delivery surface, at-rest, rotation) is tracked open in `NEXT.md` # App-secret injection hardening.
- **User-supplied config is declared, form-collected, and injected under the app's own var name — no `MOOSE_*` indirection.** A manifest declares `config: [{app_env, title, description, secret?, required?, type?, options?, default?, service?}]` (# D4); the brain renders a form, collects answers at install (required ones gate the install button) and on the app detail page after, and stamps each value into the target service's `environment:` override under `app_env` verbatim. Unlike `MOOSE_SERVICE_*`/`MOOSE_SECRET_*`/`MOOSE_FOLDER_*`/`MOOSE_MAIL_*`, the value is the app's own native variable the user would set by hand, not a moose-owned value the app must adapt to — so there is no indirection and no compose mapping line. Because the override wins over the base compose, `app_env` is reserved-name checked at parse: never the `MOOSE_` prefix and never a loader/runtime-critical var (`PATH`, `LD_PRELOAD`, …), so user input can't clobber brain-owned or process-critical environment (# D4 # Reserved names). `secret: true` masks + protects the value like a generated secret (folds into `NEXT.md` # App-secret injection hardening). Closes the `operator-env-config` catalog gap. Door-2 paste has no `config:` (the admin edits env in the YAML escape hatch). See `DECISIONS.md` 2026-06-26 (#264). A field may carry a `role` and the manifest a `requires` list, read leniently by the box and strictly by the lint (# D4 # Roles and requires, `INSTALL_SETUP.md`).
- **Outgoing mail is declared optional-only (`mail: {optional: true}`).** The declaration unlocks the install-time provider picker and per-instance `MOOSE_MAIL_*` injection (# D3, `SERVICE_PROVISIONING.md` # BYO outgoing mail); unbound apps get nothing injected and must run with email off. `optional: false` (and a bare `mail: {}`) is rejected at parse in v1.
- **Permissions granularity: medium for v1.** Internet, LAN, shared storage, devices, privileged, network isolation. Not coarse-only, not fine-grained Kubernetes-style.
- **Custom apps can request managed services.** Allowed, not encouraged.
- **No inter-app dependencies in v1.** Apps are self-contained. If they need multiple services, they go in the same compose. Cross-app sharing only via shared use-case folders (two of the same user's apps both binding the same `folders` entry; the installer points each at the same personal or shared source).
- **Manifest can live in-repo or in moose's catalog repo.** Both patterns supported indefinitely. Schema is identical in both cases. We bootstrap by writing manifests for popular apps; over time, upstreams ship their own.
- **Image references use version tags; the store catalog resolves digests.** Authors write `image: foo/bar:1.2.3`; moose's CI pins the bytes via a `sha256:` digest in the published catalog (`APP_STORE.md`). Door-2 custom apps fall back to TOFU digest pinning in the brain.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
