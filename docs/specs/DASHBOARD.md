# Dashboard — logged-in product surface

> Working spec for what the moose dashboard **is** once you're logged in: the home screen, the app model the home screen renders, global navigation, and the top bar. Companion to `WEB_UI.md` (which owns the stack, container, and deploy model — *not* the information architecture), `AUTH.md` (role-gated nav), `APP_LIFECYCLE.md` (instances, slugs, routing), `STORAGE.md` (per-user folders), `DISCOVERY.md` (the `.local` names this scheme publishes), and `NOTIFICATIONS.md` (the bell).
>
> `WEB_UI.md` answers "how is the dashboard built and shipped." This doc answers "what does the user see and touch." Every other subsystem *feeds* this surface; nothing else owns it.

## North star for this surface

A **calm launcher**, not a control panel. The home screen is the apps the household runs, with breathing room — not a wall of gauges and widgets. This is a deliberate position *against* the Umbrel/CasaOS "dashboard of widgets" shape and toward the Synology/ZimaOS "the apps are the product, the chrome gets out of the way" shape. Problems surface when they exist and stay invisible when they don't.

---

## Locked: the apps model — instances are owner-scoped

This is the load-bearing decision, and it's the one that reshaped `SPEC.md` (see `DECISIONS.md` 2026-05-29 # App instances are owner-scoped). It is also moose's clearest app-layer differentiator — see # Why this is a differentiator below.

Every app instance has an **owner**. There are two kinds:

- **Household (shared) instance** — owned by an admin, on behalf of the whole household. One running instance; every household member who has permission sees and opens the *same* instance. The app's own internal multi-user (Jellyfin profiles, Immich accounts, Home Assistant users) handles per-person separation *inside* the one instance. This is the right shape for genuinely-shared apps: Home Assistant, a VPN, a household media library, a shared grocery list.
- **Personal (per-user) instance** — owned by a single user. Its own instance id, data dir, slug, route, managed-service database, and folder bindings (it binds the *owner's* `~/Photos`, `~/Documents`, etc.). This is the right shape for personal-data apps: Immich (my photo backup ≠ my partner's), a password vault, personal notes.

Mechanically, a personal instance is exactly the Tier-3 shape already locked in `APP_LIFECYCLE.md` # "an app instance is a Docker Compose project": *N independent compose projects pointing at the same manifest+compose, each with its own instance id, data dir, and slug.* The control plane was already built for this — per-instance databases (`SERVICE_PROVISIONING.md`: each instance gets its own DB + role inside the shared Postgres *server*), per-instance on-disk layout, and a reconciler that publishes one name per instance. Owner-scoping is the model that uses machinery we already have, not new machinery.

### Install authorization

| Actor | Can install | Result |
|---|---|---|
| **Admin** | Yes | Chooses **Household** (shared, admin-owned) or **Just for me** (a personal instance owned by the admin). |
| **Member** | Yes | Always a **personal** instance, owned by that member. Members cannot create household instances. |

A member installing an app binds *their own* user folders into *their own* instance — which is why per-user instances **resolve** the "files are first-class" tension rather than create it. A single shared Immich would have to read every user's `~/Photos`, violating the per-user `0750` isolation in `STORAGE.md`. A personal Immich reads only its owner's `~/Photos`. Owner-scoping and per-user folders fit each other.

**Folder source is a per-folder install choice.** For each folder an app declares (`APP_MANIFEST.md` # `folders`), the install screen resolves a *source*: a personal instance offers the owner's folder (default) or the household Shared folder per folder; a household instance always uses Shared. The author declares only the folder + mode, never the source — "my own Jellyfin on my movies" vs "on the family library" is the installer's call, not the author's (`DECISIONS.md` 2026-05-30). This is the only folder-related input on the otherwise all-or-nothing consent screen, alongside the `pick-subfolder` prompt.

**The consent screen is a page, followed by a progress page.** Install on the app's detail page goes to a setup page at `/store/:id/install` (`?scope=household` for the household install). It is laid out as a list of rows, one per section, label on the left and content on the right, stacked on a phone: Permissions, Folders, Email, AI providers, Settings, Storage, each shown only when the app needs it. Its Install button starts the job and moves to `/store/:id/install/:jobId`, which shows the job as four phases (Preparing, Downloading, Setting up, Starting) and ends with an **Open** button, or with the error and a way back. The job id is in the URL, so a reload picks the job up again. The brain keeps jobs in memory only, so after a brain restart the page says the install is no longer tracked and points to Home. The detail page's button reads "Installing…" while the instance row is in the `installing` state. The plan for this flow, and the parts still to come, is `INSTALL_SETUP.md`.

**The consent screen is driven by `GET /api/v1/catalog/:id/install-plan`** (`BRAIN_UI_PROTOCOL.md` # GET /api/v1/catalog/:id/install-plan). The brain computes the screen's inputs from the parsed manifest and the caller's role: the declared permissions and a per-folder per-scope source menu (household → Shared only; personal → owner's folder or Shared). The endpoint is read-only and advisory — it makes no host calls and mutates nothing; the user's elections are validated and stamped into the compose override only at `POST /api/v1/apps` install time.

**The setup page shows the app's size on disk.** Alongside the permission and folder-source rows, a **Size** row shows one figure: the disk space the app's images take ("About 1.5 GB"), from `image_disk_bytes` in the install-plan `footprint` (`BRAIN_UI_PROTOCOL.md` # GET /api/v1/catalog/:id/install-plan). The brain returns raw bytes, and the UI rounds them to plain units. The download size and the "grows as you use it" estimate are not shown: the user only cares how much space the app takes. The row is skipped when the manifest carries no sizes. When `image_disk_bytes + estimated_state_bytes` approaches `free_bytes`, the row shows a **not-enough-space warning** (same disk-pressure surface as the storage pill and `HEALTH.md` # `disk-full`) — surfaced, not a hard block, matching the resource-pressure posture in # What stays deferred. The store row/`AppTile` shows the coarse catalog `footprint` (`APP_STORE.md` # Catalog schema) as a small size on the card before the user ever opens the setup page.

**Mail-capable apps add an Email row to the setup page.** When the manifest declares `mail:` (`APP_MANIFEST.md` # D3), the install plan carries the installer's own email accounts (id, label and provider) and the page shows them as cards with the provider's logo, plus **None** (the default: the app installs with email features off). The user picks one. A sole account is preselected as the obvious intent. About five cards show at first, and a **More** button shows the rest with a search box. Any user can **add an account right there**: a last card opens the same provider picker and form as Settings → Outgoing email (`POST /api/v1/mail-providers`), and the new account is then picked. The account belongs to the user who adds it, and adding needs no password re-prompt, so a hosted owner is not sent to the portal and does not lose the form (`DECISIONS.md` 2026-09-25). A household app installed by an admin sends through that admin's account. The binding is changeable later from the app's detail page, with the caller's own accounts in the picker (another user's account shows as "Someone else's account" until replaced), where a rebind re-stamps the env and recreates the containers (`SERVICE_PROVISIONING.md` # BYO outgoing mail).

**Apps with `config:` add a Settings row to the setup page.** When the manifest declares user-supplied configuration (`APP_MANIFEST.md` # D4), the install plan carries each field's schema (`app_env`, `title`, `description`, `secret`, `required`, `type`, `options`, `default`) and the page renders a small form: each field shows its `title` and `description` prominently with the `app_env` as a monospace hint beneath ("Sets `OPENAI_API_KEY`") so a user reading the app's own docs can confirm the match. Secret fields render as masked inputs; `enum` as a select, `bool` as a toggle, `text` as a single-line input. **Required fields gate the Install button** — it stays disabled until every required field has a value, so the app never installs into a guaranteed crash-loop; optional fields left blank are simply not injected (the app keeps its own default). The answers ride the same `POST /api/v1/apps` `config` body as the folder/scope elections and are stamped into the compose override (`BRAIN_UI_PROTOCOL.md` # POST /api/v1/apps). Values are editable later from the app's detail page (# Installed apps).

**AI keys get an AI providers row instead of text boxes.** Fields the page recognises as an AI provider's key, base URL, or model leave the Settings row and show as provider tiles (Anthropic, OpenAI, Gemini, OpenRouter, Groq first, the rest behind **More**). Picking a tile asks for the key, and for a model or a server address where the app has a field for it, then fills those fields. An app can take several providers at once, one per set of fields. A provider without its own fields in the app uses the app's OpenAI-compatible fields when it has them (`<APP>_CUSTOM_BASE_URL` / `_MODEL` / `_API_KEY`, or `OPENAI_API_KEY` with `OPENAI_BASE_URL`), filled with that provider's endpoint. Those fields hold one provider, so a second pick replaces the first, and the page says so. A tile the app cannot use is hidden, and the row is hidden when no field is recognised. The values ride the same `fields` map as the Settings row. For now the provider list and the recognition by env var name live in the dashboard and are temporary. Manifest roles and published provider data replace them (`INSTALL_SETUP.md`).

**Scope is selected by the Install button, not on the setup page.** The setup page has no scope picker; scope is pre-decided by which button variant the user clicked. In the store row, admins on a multi-user box see a split-button: the primary **Install** action installs as personal (just for them); the chevron dropdown offers **Install for the whole household**. Members and single-user-mode admins see a plain Install button (personal, no choice needed). See # Single-user simplification below.

### Warn, don't block, on duplicate install

When a user goes to install an app that already has an instance on the box (household or another user's personal), we **warn and offer, never block**:

> *Jellyfin is already installed as a household app. You can open it, or install your own copy.*

Rationale: two people may want genuinely different things from the same app — different media libraries, different photo backups, different notes. Blocking the second install assumes the first install serves everyone; it often doesn't. This is the one behavior that requires real multi-instance to be live (you can't fake it by disallowing duplicates), and we accept that cost because it *is* the differentiator.

### What stays deferred (don't scope-creep here)

- **Resource limits / quotas per user.** N personal Immichs = N container stacks on a pantry laptop. This is a real runtime cost, but it's a *product-acceptance* reality, not a structural one — the box owner self-limits. We surface it as a **warning when resources are tight** (ties into `HEALTH.md` `ram-pressure` / `disk-full`), not a hard cap. Quotas remain a `NEXT.md` Tier-4 item.
- **Granular post-install permission revocation.** Install-time consent is all-or-nothing (accept the declared permissions or cancel); changing a grant after install — turning off a folder, downgrading write→read — is the separate per-app permissions screen deferred to `NEXT.md` Tier-3. (Cross-user *shared-folder access* for personal instances is no longer deferred — it's now the install-time source election below; `DECISIONS.md` 2026-05-30.)
- **SSO.** The long-term door (mentioned in `SPEC.md` # Accounts & users): if/when moose gets SSO, *shared* instances become the *encouraged* path for apps that support internal multi-user, with personal instances as the escape hatch for apps that don't (or for users who want hard data isolation). Warn-don't-block keeps both doors open in the meantime.

---

## Locked: Door-2 custom container install flow

Everything above is the **Door-1** (store) install. **Door 2** — pasting a raw `docker-compose.yml` — is a separate, **admin-only** flow that produces a first-class instance under the *identical* sandbox as a store app (admission is door-symmetric; `APP_ISOLATION.md` # Trust tiers, `DECISIONS.md` 2026-06-02). This section locks its IA and screen UX; `APP_MANIFEST.md` # Custom container — synthetic manifest owns the manifest the flow produces.

### Where it lives

Admin-only, and tucked away — not a dock item, not in the Store browse grid. An **"Install a custom container"** affordance sits at the **bottom of the Store**, below the catalog, visible only to admins (members never see Door 2 — `DECISIONS.md` 2026-06-02). It opens a dedicated full-screen form, **not** the catalog consent dialog: the two are different shapes. A store install *elects folder sources* off a known manifest; a custom install *authors* the manifest from a paste. The calm-launcher posture holds — the non-technical primary audience never trips over a "paste YAML" box, while the tinkerer who wants it finds it where power-user affordances live.

### The form — what we ask vs. autodetect

One screen, top to bottom:

1. **Paste or upload the compose file.** A large textarea (file-picker as the alternative) is the primary input. The compose is held **verbatim** — moose never rewrites it (`APP_MANIFEST.md`). This is *the user's document*; it stays in its own textarea throughout (see # Form is a projection of the synthetic manifest below).
2. **App name.** A friendly display name; the slug derives from it. The form previews the resulting URL (`<slug>.local`) live as the name is typed.
3. **Main service** — *autodetected* when the compose has exactly one service; a **required dropdown** of the compose's services when it has several (`manifest.Synthesize`).
4. **Main port** — the *container-internal* port Caddy routes to. **Best-effort inferred** from every signal the compose carries — a single `expose:` value, or the *container side* of a published `ports:` mapping (`8080:80` ⇒ `80`, mined out before the mapping itself is rejected) — and **asked** only when the compose is silent, since moose can't read the image's `EXPOSE` without pulling it. Always editable, always required, with help text ("the port your app listens on *inside* the container — check the image's docs"). A published `ports:` mapping is still an admission rejection (Caddy fronts every app); we read its container side for the prefill, we don't honor the host binding.
5. **Permissions.** The admin elects the app's moose-native permissions — this is where the synthetic manifest's `permissions` block is authored (`APP_MANIFEST.md` # Custom container):
   - **Internet** — default **on** (the custom-app default), with a one-line explanation.
   - **LAN / mDNS** — default **off**.
   - **GPU** — default **off**; a single toggle. On ⇒ the synthetic manifest sets `gpu: true` (platform GPU runtime; `APP_MANIFEST.md` # gpu). No-GPU boxes surface the same capacity-check failure as a store app.
   - **Folder access** — **optional, empty by default** (most pasted containers touch no user content). An "add a folder" control adds **two-input rows**: **Source** (a picker over the fixed use-case folders — Photos, Documents, Movies, Music, Notes, Downloads) on the left, **Destination** (a free-text in-container path the admin types) on the right, plus a read/write choice. Each row becomes one folder grant in the synthetic manifest. The destination is hand-typed and Door-2-specific — see # Folder grants carry an explicit destination path below.
   - **Devices and managed `services`** are deliberately **not** given dedicated controls — they're the long tail. A power user reaches them through the **Edit as YAML** toggle (next), not a form field.
6. **Scope** — even though Door 2 is admin-only, the admin still chooses **Household** vs **Just for me**, via the same button convention as the store row (silent personal on a single-user box; # Single-user simplification).

### Form is a projection of the synthetic manifest (with a YAML escape hatch)

The form fields in steps 2–5 are a **friendly projection of the synthetic manifest** — the overlay moose wraps around the pasted compose (`APP_MANIFEST.md` # Custom container). An **"Edit as YAML"** toggle flips that overlay between the form and a **raw manifest editor**, so the power user who needs a field the form doesn't surface (`devices`, managed `services`, a `health_probe`) hand-authors it without us building a control for every key. This is the Door-1/Door-2 split recursed one level: the form is the calm path, the YAML view is the escape hatch.

Two boundaries keep it honest:

- **The toggle edits the *manifest overlay*, not the compose.** The pasted compose is the user's verbatim document and keeps its own textarea (step 1); the YAML view never merges the two. Two documents, two roles — flipping to YAML never threatens the "compose held verbatim" guarantee.
- **Admission gates every path identically.** Whether a permission was toggled, a folder row filled in, or the manifest hand-edited as YAML, submitting runs `Synthesize` + `admission.Check` (# Validation below). The escape hatch escapes the *form*, not the *sandbox* — a YAML-editing admin still can't smuggle `privileged` or a host mount past the door (`APP_ISOLATION.md` # Forbidden for both doors).

This is **install-time authoring** of a not-yet-installed app — distinct from the deferred *graduate-in-place* path (`NEXT.md`), which edits an already-installed instance's manifest (re-render, restart, reconcile, audit). Editing the overlay before the instance exists has none of that lifecycle surface.

### Folder grants carry an explicit destination path

A store app's folder grant declares no in-container path: the brain mounts every folder at a fixed `/moose/<folder>` and injects `MOOSE_FOLDER_<NAME>`, and the *author* maps that variable to the image's library path (`APP_MANIFEST.md` # Locked: folders mount at a fixed path). A Door-2 paste has no author to adapt — the verbatim third-party compose already hardcodes where it wants data (PhotoPrism reads `/photoprism/originals`, not a moose env var). So a **Door-2 folder grant carries an explicit `target`** — the destination path the admin types — and the brain binds the elected source straight there. Store apps keep the fixed-path + env-var convention; the explicit `target` is an additive, Door-2-only field (`APP_MANIFEST.md` # Custom container, `DECISIONS.md` 2026-06-02). The source side stays a **picker, not free text** — it must resolve to a real use-case folder, keeping folder access inside the files-first-class model and out of "bind any host path" territory (which admission rejects anyway).

### Validation: coach the paste into the sandbox

This is the load-bearing UX call. The common Door-2 input is a copy-pasted forum snippet that **will** trip the door-symmetric admission rules — `ports:`, host-path bind mounts, `privileged`, `cap_add`, `build:`, host namespaces (`APP_ISOLATION.md` # Forbidden for both doors). Door 2's job is to **explain and coach**, not just reject:

- **Two-stage, synchronous.** The client parses the YAML for instant structural feedback; submitting calls `POST /api/v1/apps/custom`, which runs `Synthesize` + `admission.Check` as **synchronous pre-checks** and returns `422` with the exact field-named message *before* any install job starts (implemented). A bad paste never leaves a half-built instance.
- **Errors are inline and actionable.** Each admission rejection already carries its remedy in the message ("service X declares host ports — remove the ports mapping"; "use a relative bind mount like ./data/… instead"); the form surfaces it against the offending input, not as an opaque toast. This turns the sandbox from a wall into a guided rail.
- **Image pinning is surfaced honestly.** The form notes that moose pins the **exact image it pulls now** (TOFU digest; `APP_MANIFEST.md`) and that a custom app **does not auto-update** — there is no catalog tracking its versions. The admin updates it by re-pasting a newer tag.

### Name / slug collisions

A custom install **never** triggers the duplicate-install warning: `Synthesize` mints a fresh manifest id with random entropy on every paste, so two custom apps can't collide on identity (`BRAIN_UI_PROTOCOL.md` # the two install endpoints are intentionally asymmetric). What *can* collide is the **slug** — the routable name — against an existing instance; that's resolved by the same first-come rule as everything else: bare `<slug>`, then `--<user>` (personal) or `-2` (household) on collision (# instance naming above). The form previews the preferred `<slug>.local`; the completed install reports the final, possibly-suffixed URL.

### Edit-after-install is deferred (v1 is install-only)

There is **no** in-product editor for an *installed* custom app in v1. The **Edit as YAML** toggle (# Form is a projection above) authors the manifest **before install**, while the form is open and no instance exists yet — that is not the deferred feature. To change an app *after* it's installed — a new image tag, a refined volume, a managed DB — the admin **uninstalls and re-pastes**. The "graduate the synthetic manifest in place" path (`APP_MANIFEST.md` # one model, two doors) — editing a *live* instance's manifest, then re-rendering, restarting, and reconciling — is real but **not v1**; it's parked in `NEXT.md`. This keeps Door 2's post-install surface to the one thing it must do well — get a pasted compose safely installed and routed — and matches the broader v1 posture that even store-app permission *revocation* is deferred (# What stays deferred above).

---

## Locked: instance naming / routing — first-come bare slug, `--<user>` on collision

Every instance needs a stable, unique, routable name — it's the LAN `.local` record (`DISCOVERY.md` # Per-app A records), the `.onmoose.io` subdomain (`MOOSE_NETWORK.md`), and the Caddy site block, all keyed on the instance slug.

**The scheme:**

| Scenario | Slug | LAN | Public |
|---|---|---|---|
| First install of any scope (no conflict) | `<slug>` | `immich.local` | `immich.<box-id>.onmoose.io` |
| Personal install when bare slug is taken | `<slug>--<user>` | `immich--alex.local` | `immich--alex.<box-id>.onmoose.io` |
| Household install when bare slug is taken | `<slug>-2` | `immich-2.local` | `immich-2.<box-id>.onmoose.io` |

- **The bare slug is first-come, any scope.** The first instance of any app installed — whether household or personal — wins the clean name. On a collision, a personal instance appends the owner (`--<user>`); a household instance without an owner to name gets a numeric suffix (`-2`, `-3`). Scope is an attribute shown in the dashboard (Household / Yours grouping, owner label on the tile), not encoded in the hostname.
- **Double dash (`--`) as the separator.** App slugs are kebab-case and can contain single hyphens (`home-assistant`), so a single `-` is ambiguous — `home-assistant-alex` can't be parsed into slug + user, but `home-assistant--alex` can.
- **`<slug>` leads, `<user>` trails** (not `<user>--<slug>`) so an app's instances sort together by app identity rather than collide-sorting under each user.

### Why this shape, and not the prettier dotted one

The obvious alternative — `<user>.<slug>.<box-id>.onmoose.io` (`alex.immich.…`) — reads better but **breaks the cert architecture**, which is the decisive constraint:

- `MOOSE_NETWORK.md` (lines 27, 138–139) locks **one** wildcard DNS record `*.<box-id>.onmoose.io` and **one** wildcard Let's Encrypt cert `*.<box-id>.onmoose.io`, renewed quietly every ~60 days via ACME DNS-01.
- **A TLS wildcard spans exactly one label — it does not cross dots.** `immich--alex.<box-id>.onmoose.io` is one label → covered. `alex.immich.<box-id>.onmoose.io` is *two* labels → **not** covered by `*.<box-id>…`. The dotted form would force a *separate* wildcard cert (`*.immich.<box-id>…`) issued per app, a new ACME round and DNS record on every install — destroying the "one cert, renew quietly" model.
- The LAN side agrees: Avahi publishes each instance as a flat, single-label A record `<slug>.local` (`DISCOVERY.md`). A single-label name resolves on every mDNS client; a multi-label `.local` name (the dotted `alex.immich.local`, or the old `<slug>.moose.local` infix shape) is **rejected outright by Linux's `nss-mdns`** and handled inconsistently by Android/Windows mDNS stacks. This — not just the cert architecture — is why both dimensions (app and user) collapse into one `--`-joined label. See `DISCOVERY.md` # Per-app A records.

So both transports independently force **flat, single-label**. The dotted form was rejected not on taste but on the cert and mDNS constraints. The aesthetic cost is small in practice: these hostnames are clicked from dashboard tiles, rarely typed or read raw.

### Pros / cons of the chosen scheme

**Pros**
- One wildcard cert covers every app and every personal instance, forever — no per-install ACME churn.
- Works on every mDNS client (flat single label).
- On a single-user box every app gets the clean bare slug; the `--<user>` suffix appears only when a second instance of the same app actually exists and disambiguation is necessary.
- Reuses the existing per-instance slug field (`APP_LIFECYCLE.md` # instance is a compose project) — the slug is just *derived* differently on collision.

**Cons (accepted)**
- `immich--alex` is less elegant than `alex.immich`. Mitigated: rarely seen raw.
- The `--` separator must be reserved: catalog slugs and usernames may not contain `--`, and neither may produce an `xn--` label prefix (reserved for IDN/punycode). We control both the catalog and username validation, so this is a validation rule, not a real limit. Documented as a constraint on `APP_STORE.md` slug validation and `USERS_AND_GROUPS.md` username rules.
- **A collision-triggered `--<user>` hostname leaks the `username ↔ app` mapping to the LAN.** `immich--alex.local` is a published mDNS record (`DISCOVERY.md`), so any device on the network can observe which user triggered a disambiguation. Bare names (the common case on a lightly loaded box) reveal nothing about scope or ownership; the leak occurs only when two instances of the same app coexist. Net improvement over the old scheme where every personal instance was always suffixed. Accepted for the same reasons: closed-by-default, single-household LAN (`THREAT_MODEL.md` treats the LAN as semi-trusted), and the record must exist for routing regardless. Revisit if moose ever targets shared/untrusted LANs.

---

## Locked: the home screen is the app launcher

Home = a grid of app tiles. No widgets (see below). The grid is grouped:

- **Household** — shared instances the current user has permission to open.
- **Yours** — the current user's personal instances.

At v1's app counts there's no scale problem, so the groups are simply **rows/sections** on one screen. The longer-term shape is **swipeable pages** (think iOS home screens) once a household accumulates enough apps to justify paging — reserved, not built.

A member sees their **Yours** group plus the **Household** apps they're permitted to open; they never see other members' personal instances. An admin additionally sees management affordances (install-as-household, the gear routes below). The grid itself is the same component; the *contents* are scoped per user.

### Tile

A tile shows: icon, app name, and a category/role label. The **clickable affordance is the logo square only, and only to open a running app** — a stopped or failed tile is inert. Starting, stopping, and retrying a service all happen from the tile's **quick menu** (see # Quick menu), opened by a small **menu button** pinned to the right edge of the tile beside the name and shown only to a viewer who may control the app. In the calm default the tile carries **no status decoration**. State surfaces only when it's not nominal:

- **Failed** — an instance whose install or start transaction ended in `failed` (`APP_LIFECYCLE.md` # install transaction, # stop, start, uninstall). The tile takes a **light amber/warning tint** — deliberately distinct from the gray *stopped* tile — and keeps the corner **alert mark**: failed is trouble, not an intended state. The tile shows a "Failed" hover caption. A viewer who may control the app (admin for any app; the owner for their own personal app) **retries from the quick menu** (the Retry action runs the **same Start transaction** as a stopped-app start — `APP_LIFECYCLE.md` # stop, start, uninstall — including the `<slug>.local` mDNS re-publish, so on success the app is `running` with its name and route restored) and also gets a **"View details" link** to the app page (`/settings/apps/<id>`), where the failure reason / logs live — so a persistent failure is diagnosed rather than blindly retried. A viewer who can't control the app sees the amber alert tile without the menu button or the link.
- **Other trouble (down / crash-looping / interrupted)** — any other non-nominal state (a crash-looping container's `needs-attention` surface, `HEALTH.md` `container-restart-loop`; an interrupted install/update) grays the tile out with the corner alert mark and **no inline action** — the health surface owns the detail and, for a crash loop, Docker's `restart: unless-stopped` is already retrying.
- **Stopped** — a *deliberately* stopped app (`APP_LIFECYCLE.md` # stop, start, uninstall) also grays out, but carries **no alert mark** — it's an intended state, not trouble. The tile shows a **"Service stopped"** hover caption and is otherwise inert; a viewer who may control the app starts it again from the **quick menu** (see # Quick menu). (The rest of the *updating* / *starting* visual treatment is implementation-time UX, not spec.)

On a hosted box a tile also carries a **public marker**: a small globe in the bottom corner of the logo square. It has two forms, because an app can be open in two different degrees. A **whole globe** means the access mode is **Public** — anyone with the link opens the app (`ENVIRONMENT.md` # Per-app owner-only access, #306). The **same globe faded** (30% opacity) means the app is **Only me** but its manifest declares `access.public_paths` (`APP_MANIFEST.md` # E2, #415), so part of it still answers anonymously. One drawing carries both states on purpose: a partly open app reads as a fainter version of an open one, not as a second symbol to learn. An app that is closed end to end carries **no marker**: the quiet default needs no mark. Both markers are read-only signals — the launcher has no way to say "anyone with this link can open this" otherwise, and the toggle itself stays on the app's settings page, where the same pair of facts is written out in words. The marker is **hosted-only**: the appliance has no public app subdomains, so every tile there would carry it and it would mean nothing.

### Quick menu

The menu button beside the name opens a small popup — a lighter-weight surface than the full Settings → Installed apps detail page, reachable without leaving the launcher. It shows the app's **logo, name, and short description**, then two full-width actions: the **service control** (Stop a running app / Start a stopped one / Retry a failed one — the same Start/Stop transaction the logo click and the detail page run) and **App settings**, a link to the per-app detail page (`/settings/apps/<id>`). The short description is the catalog's `short_description`; a Door-2 custom app with no catalog entry simply shows logo + name with no blurb. The button — and therefore the menu — is shown **only to a viewer who may control the app** (admin for any app; the owner for their own personal app); the brain re-checks authorization on every Stop/Start regardless. The menu's visuals are implementation-time UX; the affordance set and the role gating are the spec.

### Open-app interaction

Clicking a **running** tile's **logo opens the app in a new browser tab** at its own host (`<slug>.local` or `<slug>--<user>.local` depending on whether disambiguation was needed, or the `.onmoose.io` host when the remote toggle is on). The app runs on its own origin — that's the whole point of subdomain routing (`SPEC.md`: browser same-origin isolation). The dashboard is the launcher, not a frame/proxy around apps. A stopped or failed tile's logo does nothing — starting the service is a quick-menu action (see # Tile above).

### First arrival / empty state

(Folds in the former `NEXT.md` Tier-2 "Dashboard at first arrival.") A box with no apps yet shows an empty **Your apps** state that points at the Store rather than a wall of suggestions or a forced starter bundle. The calm posture applies from the first second: invite, don't shove. Concrete copy and whether to offer a light "get started" nudge is implementation-time UX.

---

## Locked: global navigation — a four-item dock

A floating bottom dock with exactly four destinations:

| Item | What it is |
|---|---|
| **Home** | The app launcher (above). |
| **Files** | The in-dashboard file browser over the user's use-case folders and `~/Shared/`. Owned by its own spec (`FILES.md`); appears here as a top-level destination because "files are first-class." |
| **Store** | Browse/install apps. Install respects the authorization table above. |
| **Settings** | Box + account settings, and the **home for gated routes**. |

**Activity (audit log) and Users live *under Settings* as gated routes**, not as top-level dock items — administrative surface, not daily-use. Role gating per `AUTH.md`: **Users is admin-only.** **Activity is open to every signed-in user but scoped server-side** — a member sees only events where they are the actor or target, an admin sees the full box-wide feed (`LOGGING.md` # Visibility rules; the brain enforces the split, the UI renders whatever it returns). Admins additionally see the system/storage/network panels. (Activity's all-user visibility was settled by issue #11 and `LOGGING.md`; this supersedes an earlier "admin-surface" framing — see `DECISIONS.md` 2026-06-05.)

Settings is itself a **left-nav shell**: a sidebar of sections on the left (collapsing to a horizontal tab strip on narrow screens) and the active section's content filling the rest. Each section is its own nested route under `/settings`, so sections deep-link and the avatar-menu links land directly on them; `/settings` redirects to the Account section. The nav is grouped: **You** holds what the signed-in user owns (Account, Installed apps, then Outgoing email, the user's own email accounts), **System** holds the box-wide items (Users, Notifications, Activity, About) — Notifications sits directly above Activity because both answer "what happened on this box". The section set as built: **Account** (the signed-in user's identity + self-service password change), **Installed apps** (a list of installed instances, one card with a row per app: the app's logo (or `AppGlyph` fallback) in a rounded square, its name, and its short catalog description truncated to one line — empty for a Door-2 custom app with no catalog entry, rather than a blank line. A row's right side carries a colored status dot with a plain word ("Running" green, "Stopped" gray, "Failed" amber, any other state written out amber) — never the raw lowercase state string — and swaps to amber "Needs attention" for a running app with an open `container-restart-loop` or `app-unresponsive` health issue against that instance. Under the status, small muted text joins whatever applies with " · ": the owner label in multi-user mode ("Shared" or the username), the version as `v<version>`, and the access mode on hosted boxes only (same three-way label as the Access control below, so the two can't disagree). The status stays visible at phone width; the second line may hide there. Clicking anywhere on the row opens a per-app detail page at `/settings/apps/<id>` — logo, name, description, the app's **URL written out as a link** so it can be opened in a new tab or copied with a right-click (the **Open** button next to it is the one-click path, but it hides the address and only exists while the app runs; there is no copy button because `navigator.clipboard` is unavailable on the HTTP-only `.local` origin), the **Stop service** / **Start service** control, **Uninstall**, an **Access** toggle on hosted boxes (**Only me** / **Public** — the per-app access mode of `ENVIRONMENT.md` # Per-app owner-only access, #306/#307; hidden on the appliance, which has no public app subdomains, and switching re-writes the app's Caddy route via `PUT /api/v1/apps/<id>/exposure`). An app whose manifest declares `access.public_paths` (`APP_MANIFEST.md` # E2) must say so on that control instead of a bare "Only you can open it": the toggle still reads **Only me**, and the line under it adds that some app paths are open to the public, because a label that hides that would claim a narrower app than the box is serving. It says *that* some paths are open, not which ones: the list is manifest detail nobody acts on from this page. The check is made against the instance's own manifest copy, the same file the route was built from, so the label cannot drift from the route (#415), an **outgoing-email** picker for mail-capable apps, a **Setup secrets** reveal for apps that declare an owner-visible secret (`APP_MANIFEST.md` # D2 — the per-instance bootstrap credential, masked until revealed, with a best-effort copy that degrades to select-on-screen since `.local` is HTTP-only), a **Settings** editor for apps that declare `config:` (`APP_MANIFEST.md` # D4 — the same fields the install form collected; non-secret values shown and editable, secret values shown as "set" with a **replace** affordance rather than revealed, and saving rewrites the override and restarts the app), and the app's logs. Control authorization mirrors install/uninstall: admins for any app, the owner for their own personal app — the same gate guards the secret reveal and the config editor), **Users** (admin-only; the nav item is hidden from members and the section also redirects), **Outgoing email** (every user, their own accounts only), **Notifications** (per-category bell mutes), **Activity**, and **About** (product identity; grows to show version/box-name once the brain exposes them). The admin **Storage / Network / System** panels are reserved sections, not yet built. The sidebar visuals are implementation-time UX; the section set and the role gating are the spec.

**Global cross-surface search** (one box that spans apps + files) is deferred. At v1 app counts the home grid is scannable; that search earns its place when a household's app + file corpus outgrows the eye. Reserved, not built. The **Store** is the exception, and a scoped one: as the catalog corpus grows it carries its own in-page browse filters — a page-wide search over name + short description, and a row of category pills (the union of the catalog's own `categories`, "All" first; `APP_STORE.md` # Catalog schema, "Browse UI groups by category"). These filter the browse grid in place and don't reach beyond the Store.

---

## Locked: the top bar

Four elements, top corners, quiet by default:

- **Storage pill** — a small always-present capacity readout (e.g. `1.2 / 4 TB`). Present but never loud; clicking it goes to Settings → Storage. It turns insistent only under disk pressure (`HEALTH.md` `disk-full`).
- **Live-resources chevron** — a small chevron next to the avatar menu opens a compact, live-updating panel (CPU / RAM / net in-out / disk IO) streamed over `GET /api/v1/system/live`. Available to **every** signed-in user — host-level state isn't per-user data. Opening the panel opens the SSE stream; closing it closes the stream (`LOCAL_ANALYTICS.md` # Real-time system resources). Added to the locked set 2026-05-31 (`DECISIONS.md`).
- **Avatar / account menu** — the current user; the menu is the path to account settings, sign-out, and (for admins) the gated routes that also live under Settings.
- **Notification bell** — the in-product notification center (`NOTIFICATIONS.md`). A small dot indicates unread count. This is the dashboard-only v1 transport; off-box transports (email, push) are the deferred seam in `NOTIFICATIONS.md`.

The greeting/status line and any ambient "everything's fine" prose are **implementation-time UX**, not spec — explicitly out of scope here.

---

## Locked: no home-screen widgets in v1

Umbrel ships app-contributed home-screen widgets as a first-class `umbreld` module. moose does **not** have a widget concept in v1, and the home screen shows no cards on a healthy box. Reasons:

- It's the calm-launcher position: apps get the breathing room; the chrome stays quiet.
- Widgets are an app-author contract (a manifest surface, a render sandbox, a security boundary) that we are not opening in v1.
- The information widgets would carry (health, resources, storage) already has homes: the storage pill, the bell, the live-resources surface (`LOCAL_ANALYTICS.md`), and the degraded-state cards that appear *only* when something's wrong.

This is a pin-a-no decision, recorded so a future "add widgets" PR is a deliberate reopening, not a drift. If widgets ever return, they'd be an app-manifest feature designed alongside the security model — out of scope now.

---

## Locked: single-user simplification

When `single_user_mode` is true (the box has exactly one registered user), the household/personal distinction is meaningless — suppress it everywhere. The UI should read as a simple, personal launcher with no multi-user vocabulary.

**Home grid:** the Household and Yours section headers are hidden. All apps render in a flat grid; sections still render only when non-empty, so the layout is unchanged, just unlabeled.

**Install button:** a plain **Install** button with no chevron. Scope is silently personal. The split-button (with the household dropdown) only appears when `role == admin && !single_user_mode`.

**App tiles:** the "Shared" / "Personal" scope label is hidden. The tile shows name only.

**Settings manage-apps list:** the scope/owner label (e.g. "Shared" or the owner's username) is hidden.

**Folder source labels on the install setup page:** "The household's shared X" is relabeled to "Shared X (accessible from your other devices)" — the Samba angle is real and valid even solo, but "household" is confusing with one user.

**Transition:** `single_user_mode` is recomputed on every session-bearing response (`/login`, `/setup`, `/me`). When a second user is created and the admin next logs in or refreshes, `single_user_mode` becomes false and all suppressed UI reappears. No migration of existing app instances needed — scope and owner metadata is always stored; it just wasn't surfaced.

---

## Why this is a differentiator

A scan of the neighbors (May 2026) shows everyone separates *files* per user but **nobody makes app *instances* a per-user, self-service concept**:

- **Umbrel** — explicitly single-user; multi-user is a years-old, still-unbuilt feature request.
- **ZimaOS** — multi-user at the SMB/folder layer only; apps bind to the owner account ("supports owner accounts only"), so a member logging in still hits the main user's app data.
- **TrueNAS SCALE** — per-user separation is ZFS datasets + ACLs; running two instances of an app is a *manual admin* chart deployment with no notion of "this instance belongs to Alex."
- **Synology DSM** — real multi-user OS, and you *can* run multiple Docker instances, but it's manual (separate containers, manual ports + reverse-proxy rules); Package Center packages are single-instance.

The universal fallback is "one shared instance + the app's own internal multi-user (if it has any), layered on shared files" — which is exactly the no-SSO tension in `SPEC.md`, and exactly what breaks for personal-data apps. The reason no one ships identity-driven per-user instances is the plumbing (per-instance routing, certs, databases). moose already has that plumbing, so it can ship the thing the neighbors punt on. This is squarely on the "app ecosystem is the strongest pillar" thesis.

---

## Relationship to other docs

- `WEB_UI.md` — stack, container, deploy, API-version handshake. Unchanged by this doc; this doc is its IA complement.
- `APP_LIFECYCLE.md` — owns the instance-as-compose-project model and the slug field this scheme derives. The first-come + collision-suffix derivation rule is recorded there too.
- `DISCOVERY.md` — publishes the per-instance `.local` name; "slug" there now means "the (possibly suffixed) instance slug."
- `AUTH.md` — the role gating behind the dock, Settings routes, and install authorization.
- `STORAGE.md` — per-user `~/` folders that personal instances bind; the model owner-scoping is designed to respect.
- `NOTIFICATIONS.md` / `LOCAL_ANALYTICS.md` — the bell and the (non-widget) live-resources surface.
- `FILES.md` — the Files destination in the dock; the in-dashboard file manager over the user's home + the Shared tree.

Open items that touch this surface (per-app tile state vocabulary beyond down/stopped, search design, swipe-paging, first-arrival nudge copy) live in `NEXT.md`, not here.
