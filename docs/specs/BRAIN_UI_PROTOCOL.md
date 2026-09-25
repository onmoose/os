# Brain ↔ UI protocol

> The wire-level contract between the dashboard (browser TS client) and the brain. Sibling to `BRAIN_HOST_PROTOCOL.md` (which specs the brain ↔ host-agent boundary). Same four-pattern shape so engineers learn one model end-to-end.
>
> Companion to `CONTROL_PLANE.md` (architecture context), `AUTH.md` (session cookie), `WEB_UI.md` (client-side stack consuming this protocol).

## Scope

Everything the dashboard, a future CLI, a future third-party app store, or any external integrator does against the brain. This is moose's **public API surface** from day one — there is no separate internal route table.

## Transport

HTTPS via Caddy → brain. Browser-native fetch / EventSource / WebSocket. No bespoke client library required.

## Wire format

HTTP/1.1 + JSON. Versioned URL prefix `/api/v1/...`. The UI bundle declares its expected API version (`X-Moose-API-Version`); brain returns **426 Upgrade Required** on mismatch.

## API patterns

Four patterns, same rule as host-agent. Sync for short ops, jobs for anything that can exceed ~5 seconds, SSE for one-way streams, WebSocket reserved for future bidirectional needs.

### Pattern A — Sync request/response

For anything under ~5s and not needing progress.

```
GET  /api/v1/apps                          → list installed instances
GET  /api/v1/apps/:id                      → instance detail
GET  /api/v1/apps/:id/config               → user-supplied config values (APP_MANIFEST.md # D4; secret fields masked)
PUT  /api/v1/apps/:id/config               → update config values; rewrites override + restarts the app
POST /api/v1/users                         → create user
GET  /api/v1/settings/network              → current network config
GET  /api/v1/health                        → active health issues (see HEALTH.md; v1 path, in the OpenAPI spec as of issue #12)
POST /api/v1/health/:id/:act               → invoke a remediation action attached to an issue
GET  /api/v1/catalog                       → browse grid: id, name, version, short_description, categories, icon_url, icon_glyph, footprint
GET  /api/v1/catalog/:id                    → detail page: the browse fields plus long_description, screenshot_urls, author, license, links, changelog_url
GET  /api/v1/catalog/:id/icon               → app icon image bytes (raw; not JSON)
GET  /api/v1/catalog/:id/screenshots/:n     → n-th screenshot image bytes, manifest order, 0-based (raw; not JSON)
GET  /api/v1/catalog/:id/install-plan      → permission/scope plan for installing a catalog app (see below)
POST /api/v1/files/list                    → directory listing (see Files below)
POST /api/v1/files/mkdir | move | copy | delete  → file operations (see Files below)
```

Plain HTTP. Errors: HTTP status + `{ "code": "...", "message": "...", "details": {...} }`. Codes are stable strings; messages are human-readable, not contractual.

#### Catalog browse, detail, and assets

`GET /api/v1/catalog` returns the browse grid — one `Entry` per app with just what a card needs (`APP_STORE.md` # Catalog schema): id, name, version, `short_description`, `categories`, `icon_url`, the optional `icon_glyph` fallback, and the coarse `footprint`. `GET /api/v1/catalog/:id` returns the detail view: the same `Entry` fields embedded, plus `long_description` (markdown), `screenshot_urls`, `author`, `license`, `links`, and `changelog_url`. Both require an authenticated session (401 if absent); unknown id → 404; a malformed catalog entry → 500 (same integrity posture as install-plan).

`icon_url` and the `screenshot_urls` entries point at `GET /api/v1/catalog/:id/icon` and `/screenshots/:n` — they serve **raw image bytes**, not JSON, so the store loads them directly in `<img>` tags (and they stay out of the OpenAPI surface). `icon_url` is present only when the manifest declares an icon, so the store renders a glyph fallback without ever requesting a 404 — the manifest's optional `icon_glyph` (a Lucide name, returned as a plain JSON string on the `Entry`) picks that fallback glyph; absent ⇒ a generic glyph. **As built (#420) the brain proxies these; it does not read them off disk.** No catalog is baked into the box, and the box keeps none (`APP_STORE.md`). On the first request for an asset the brain fetches it from the URL the catalog published (today an object-storage origin) and caches the bytes under `MOOSE_CATALOG_CACHE_DIR`. That cache is per app and expires after 24 hours. It is not a catalog directory. So what the endpoint serves is the last good copy of a remote file, and a box that has never synced serves nothing. These all return 404: an unknown app, an app with no icon, an index that is out of range, and a non-numeric `:n`.

#### GET /api/v1/catalog/:id/install-plan

Returns everything the install-consent screen needs before the user confirms. Requires an authenticated session (401 if absent). An unknown catalog id → 404; a catalog entry that exists but fails to parse or is missing its compose file → 500 (an integrity problem a curated catalog should never ship, surfaced loudly rather than masked as a missing app).

```jsonc
{
  "manifest_id": "jellyfin",
  "name": "Jellyfin",
  "version": "10.9.6",
  "scope_options": ["household", "personal"],  // role-derived: admin gets both, member gets ["personal"] only
  "scope_default": "household",                 // admin → "household", member → "personal"
  "footprint": {
    "download_bytes": 612000000,                // images still to pull — excludes layers already on this box
    "image_disk_bytes": 1480000000,             // decompressed image cost incremental to this box
    "estimated_state_bytes": 10737418240,       // manifest storage.estimated_size, parsed to bytes (omitted if unset)
    "free_bytes": 412000000000                  // free space on the target data disk, for a not-enough-space warning
  },
  "permissions": {
    "internet": false,
    "lan": true,
    "gpu": false,
    "devices": ["/dev/dri/renderD128"],
    "folders": [
      {
        "folder": "movies",
        "mode": "write",              // "read" | "write"
        "scope": "pick-subfolder",    // "whole" | "pick-subfolder"
        "subfolder_default": "Movies/Family",  // omitted unless scope=pick-subfolder
        "sources": {
          "household": { "options": ["shared"],             "default": "shared" },
          "personal":  { "options": ["personal", "shared"], "default": "personal" }
        }
      }
    ]
  },
  "config": [                          // user-supplied config fields (APP_MANIFEST.md # D4); omitted when the manifest declares none
    {
      "app_env": "OPENAI_API_KEY",     // the app's own env var — also the form's monospace hint
      "title": "OpenAI API key",
      "description": "From your account at platform.openai.com; lets this app call OpenAI on your behalf.",
      "secret": true,                  // masked input; never echoed back
      "required": true,                // gates the Install button
      "type": "text"                   // "text" | "enum" | "bool"
    },
    {
      "app_env": "OPENAI_MODEL",
      "title": "Model",
      "description": "Which model the app uses for new chats.",
      "type": "enum",
      "options": ["gpt-4o", "gpt-4o-mini"],
      "default": "gpt-4o-mini"
    }
  ]
}
```

**Key properties of the response:**

- **Footprint is box-specific and incremental.** Unlike the coarse `footprint` summary in the catalog entry (`APP_STORE.md` # Catalog schema, used for the store grid), `download_bytes` and `image_disk_bytes` here are computed *for this box*: the brain inspects which of the app's images and layers are already present locally and subtracts them, so an app that reuses a base image you already have reads as cheaper. `estimated_state_bytes` is the manifest's `storage.estimated_size` parsed to bytes — the app's own working data measured at install, never user content and never a usage projection (`APP_MANIFEST.md` # Storage, `DECISIONS.md` 2026-06-09) — omitted when the manifest sets no estimate. `free_bytes` is the free space on the target data disk so the dialog can warn before an install that won't fit — same disk-pressure surface as `HEALTH.md` # `disk-full`. The UI owns all wording and unit rounding; the brain returns raw bytes. Like the rest of this endpoint, footprint is **advisory** — it makes no host mutation and the pull-time reality is authoritative.
- **Role-derived scope options.** `scope_options` and `scope_default` are computed from the caller's role. `POST /api/v1/apps` enforces the same rule (members are rejected on household scope). The scope is not selected on the install setup page. The split-button on the app's detail page sets it (see `DASHBOARD.md` # single-user simplification) and passes it to the page as `?scope=household`. `scope_options`/`scope_default` remain in the response for future use but the dashboard does not render a picker from them.
- **Per-scope source menus (Option A).** Each folder carries `sources.household` and `sources.personal`, each a `{options, default}` menu. The UI does zero policy derivation: pick a scope, look up `folder.sources[<scope>]`, render. A single-option menu (`household → ["shared"]`) renders as fixed/disabled. Both menus are always populated regardless of the caller's role — the household menu is unreachable for members (household scope isn't offered) but keeping the shape uniform means the UI doesn't branch on role when rendering a folder row.
- **Structured fields only, no copy.** The brain returns `mode`/`scope`/`subfolder_default` and source fields. The UI owns all wording ("can add, change & delete files in…", "Which folder should this app manage?"). This matches how the rest of the brain returns data, not sentences.
- **Config schema, not values.** `config` carries each declared field's *schema* so the dialog can render and validate the setup-fields form (`DASHBOARD.md` # Install authorization). It never carries a value — `secret` fields have none yet, and a `default`/`options` are part of the schema. The user's answers come back in the `POST /api/v1/apps` `config` body alongside the folder/scope elections, where the brain validates them (required present, enum within `options`, `app_env` recognized) and stamps each into the app's compose-override `environment:` under its `app_env` — directly, with no `MOOSE_*` indirection (`APP_MANIFEST.md` # D4, `SERVICE_PROVISIONING.md` # Env-var injection). After install, values are read/updated through `GET`/`PUT /api/v1/apps/{id}/config`.
- **Advisory, not authoritative.** The install-plan drives the consent screen. The authoritative validation + override stamping happen in slice 4 when `POST /api/v1/apps` receives the user's elections in its `config`. This endpoint makes no *mutating* host calls and changes nothing on the box — the footprint's read-only host (free space) and Docker (already-present images) queries are its only host contact, and a failure of either degrades to zeros rather than failing the plan.

**`single_user_mode` on session-bearing responses.** `GET /api/v1/me`, `POST /api/v1/login`, and `POST /api/v1/setup` all return a `UserDTO` that includes `"single_user_mode": true|false` — computed as `user_count == 1`. The flag is present on every session-establishing response (not just `/me`) so the UI has the correct value from the moment the session is created, without a follow-up fetch. Other endpoints that return `UserDTO` (user-management list/patch) omit it (`omitempty`). The dashboard uses this flag to: (a) show a plain Install button instead of a split-button, (b) suppress the Household/Yours section headers on the home grid, (c) hide the scope label on app tiles and in Settings, and (d) relabel the shared folder source from "The household's shared X" to "Shared X (accessible from your other devices)" in the consent dialog.

#### Files (`/api/v1/files/*`)

Back the in-dashboard file manager (`FILES.md`). Scoped per session to two roots — `home` (the caller's `/home/<user>/`) and `shared` (`/srv/moose/shared/`); there is no cross-user browse, for any role (`FILES.md` # Authorization). The brain validates session + root + path-containment (rejects `..`/absolute escapes) and forwards to host-agent's `/v1/files/*`, which does the real work **as the user's UID** (`BRAIN_HOST_PROTOCOL.md` # Files endpoints). `path` is `<relative>` within the named `root`.

- **Metadata ops are Pattern A:** `POST /api/v1/files/list` (returns `{entries:[{name, dir, size_bytes, mtime, hidden}]}`), `mkdir`, `move`, `copy`, `delete`. Standard `{code, message}` errors — `permission-denied`, `not-found`, `exists`, plus `blocked-by-health-issue` (`409`, when `data-drive-missing` blocks writes) and `507 Insufficient Storage` (ties to `disk-full`, `HEALTH.md`).
- **Content ops are streaming endpoints, not jobs:** `GET /api/v1/files/content?root=&path=` streams a download; `PUT /api/v1/files/content?root=&path=` streams an upload body. The brain pipes bytes to/from host-agent without buffering whole files. This is the **deliberate ">5s = job" exception** — transport-native progress, no server-side job state — mirroring the SSE log-tail exemption. File ops are **not** audited and do **not** trigger the elevation re-prompt (`FILES.md` # Audit & elevation).

### Pattern B — Jobs

For anything that can exceed ~5s or needs progress / cancel: app install, app update, mkfs, Tailscale enrollment, OS update, large config migrations.

```
POST /api/v1/apps
  { "manifest_id": "photoprism", "scope": "household", "confirm": false, "config": {...} }
→ 202 Accepted
  { "job_id": "j_a4f7b2", "kind": "app-install", "status": "running" }

GET  /api/v1/jobs/j_a4f7b2
→ 200 OK
  { "job_id": "j_a4f7b2", "kind": "app-install", "status": "running",
    "progress": 0.42, "step": "pulling_images", "started_at": "..." }

POST /api/v1/jobs/j_a4f7b2/cancel
→ 200 OK
  { "job_id": "j_a4f7b2", "status": "cancelling" }
```

Status values: `running`, `completed`, `failed`, `cancelled`, `cancelling`, `stalled` — same vocabulary as host-agent jobs. On `completed`, the response carries `result`. On `failed`, an `error` with `code` + `message`.

**Owner-scoping on install (`DASHBOARD.md` # the apps model).** `scope` is `"household"` or `"personal"`. Members may only install `personal` instances (a `household` request is `403`); admins choose, defaulting to `household` when omitted. The owner is always the calling user — there is no "install on behalf of" parameter. Installed instances carry `owner_user_id`, `owner_username`, and `scope` in the `GET /api/v1/apps` / `:id` DTO; `GET /api/v1/apps` is scoped to the caller (own personal + all household; admins see all), and `GET /api/v1/apps/:id` returns `404` (not `403`) for a personal instance the caller doesn't own, so existence isn't disclosed.

**The install `config` body carries the user's elections — folder sources, subfolders, mail binding, and user-supplied config values.** Setup-field answers (`APP_MANIFEST.md` # D4) arrive keyed by `app_env`, e.g. `"config": { ..., "fields": { "OPENAI_API_KEY": "sk-…", "OPENAI_MODEL": "gpt-4o-mini" } }`. The brain validates against the manifest (every `required` field present and non-empty, `enum` values within `options`, no unknown `app_env`) → `422` with per-field `errors` on failure; on success it stamps each value into the target service's `environment:` in the compose override, under the `app_env` verbatim, before the first `compose up`. Secret-field values are persisted like `MOOSE_SECRET_*` and never returned by any read endpoint.

**`GET`/`PUT /api/v1/apps/{id}/config` edit the values after install.** `GET` returns the field schema plus current non-secret values; a secret field reports `"set": true|false` but never its value. **`PUT` is a partial update, not a full replace** — this matters because the client can never read a secret back, so it cannot resubmit it. An `app_env` **omitted** from the request keeps its stored value; a field present with a non-empty value sets it; a field present with an explicit empty string (an optional field only) clears it. **`required` is validated against the resulting state, not the request** — a required field is satisfied by a value already stored, so editing one field never forces the user to re-enter a secret they can't see, and a required secret can only be *replaced* (by sending a new value), never accidentally blanked (an empty string on a required field is a `422`). The same per-field validation as install otherwise applies (enum within `options`, no unknown or reserved `app_env`). `PUT` rewrites the override and restarts the app so the new environment takes effect — a job, since `compose up` recreates containers. Both follow the app-control gate (admin for any app, owner for a personal app — the same gate as stop/start and the secret reveal); a config change is an elevation-class mutation and audits success and failure (`CONTROL_PLANE.md`).

**Warn, don't block, on duplicate install (`DASHBOARD.md` # warn, don't block).** A `POST /api/v1/apps` with `confirm` unset/false, when an instance of that manifest already exists that the caller can see (a household instance or their own personal one), returns `409 Conflict` with `code: "duplicate-install"` and an `errors` array summarizing the existing copies. The UI surfaces "open it" vs. "install your own copy"; the latter retries the same request with `confirm: true`, which skips the check.

**The two install endpoints are intentionally asymmetric.** `POST /api/v1/apps` (catalog, Door-1) takes `scope` **and** `confirm`. `POST /api/v1/apps/custom` (pasted compose, Door-2) takes `scope` but **not** `confirm` — a custom install synthesizes a fresh manifest id with a random suffix on every paste, so two custom installs can never collide and the duplicate warning never applies.

Some brain jobs internally delegate to host-agent jobs (an app install does brain-side work; a `mkfs` is essentially a passthrough). The brain owns its own job ID space; the host-agent job ID is an internal implementation detail.

**The rule:** if a route can exceed ~5 seconds, it's a job. Bias toward "make it a job" when uncertain.

### Pattern C — SSE (server → client streams)

Three distinct stream types:

**1. Per-resource log / progress tails.**

```
GET /api/v1/jobs/j_a4f7b2/log         — install/update/mkfs output
GET /api/v1/apps/:id/log              — container log tail (forwarded from Docker)
GET /api/v1/services/smbd/log         — Tier-2 service log (forwarded from host-agent journalctl)
```

For app and service logs, the brain is a transparent forwarder over the host-agent SSE stream (`BRAIN_HOST_PROTOCOL.md` Pattern C). Events flow end-to-end with no translation; the brain re-emits IDs from its own monotonic counter so dashboard `Last-Event-ID` replays work even across brain restarts.

**2. Global event stream — dashboard liveness.**

```
GET /api/v1/events
→ Content-Type: text/event-stream

  id: 1
  event: app.state_changed
  data: {"instance_id":"...","state":"running","prev":"installing"}

  id: 2
  event: update.available
  data: {"instance_id":"...","from":"2.4.1","to":"2.4.2"}

  id: 3
  event: drift.surfaced
  data: {"surface":"smbd","desired":"enabled","actual":"disabled"}

  id: 4
  event: health.issue_raised
  data: {"id":"data-drive-missing","severity":"error","summary":"Your data drive isn't connected."}

  id: 5
  event: health.issue_cleared
  data: {"id":"data-drive-missing"}
```

One long-lived stream per dashboard tab. Carries typed events for: app lifecycle transitions, updates available / applied / failed, drift surfaces, peer / mesh events (when mesh ships), Tier-2 service state changes, **health issues raised / cleared / updated** (see `HEALTH.md`), user notifications.

**Blocked-operation responses.** When a request is refused because a health issue's `blocks_writes` / `blocks_apps` / `blocks_users` flag is set, the brain returns `409 Conflict` with `{code: "blocked-by-health-issue", issue_id: "...", message: "..."}`. The UI uses `issue_id` to link the user from the failed action back to the banner explaining why.

**Event `kind` values are enumerated in the API schema.** No untyped `{type, data}` blobs. Adding a new event kind is an API-version-bumping change.

**Reconnect resilience.** Same as host-agent: monotonic event `id`, rolling buffer (~256 KB per stream), client sends `Last-Event-ID: <n>` on reconnect, brain replays from `n+1`. If the gap exceeds the buffer, brain emits one `{"lost": true}` event and resumes from current.

**Stream cap.** Brain enforces ≤16 concurrent SSE streams per session — backstop for buggy dashboards or many open tabs. Excess connections receive `429 Too Many Requests`.

**3. Live system-resources stream — on-demand, not persisted.**

```
GET /api/v1/system/live
→ Content-Type: text/event-stream

  event: sample
  data: {"cpu_pct":12.4,"load":[0.42,0.51,0.48],
         "mem":{"used_bytes":7513882624,"total_bytes":16728338432,"available_bytes":9214455808},
         "net":[{"iface":"enp3s0","rx_bps":812000,"tx_bps":143000}],
         "disk":[{"dev":"sda","read_bps":410000,"write_bps":92000}],
         "uptime_s":84021}
```

Available to **every** signed-in user — host-level state isn't per-user data (`LOCAL_ANALYTICS.md` # Privacy model). The brain polls host-agent's `GET /v1/system/resources` (`BRAIN_HOST_PROTOCOL.md`) once per second, diffs the raw counters into the rates above, and fans out to all subscribers from one upstream poller. **Wire units are SI:** `*_bps` are bytes/second, `*_bytes` are bytes, `cpu_pct` and `load` are floats; the UI does the human formatting (KB/s, GiB). The stream opens on the first subscriber and the brain stops polling when the last disconnects (zero idle cost).

**Storage fullness is a one-time poll, not this stream.** The same panel's Storage bars (how full each disk is) read `GET /api/v1/system/storage` — a plain huma JSON endpoint (in the OpenAPI spec, so its TS types are generated) returning `{ "disks": [ { "label", "free_bytes", "total_bytes" } ] }`, the brain's pass-through of host-agent's `SystemStatus.Disks` (`BRAIN_HOST_PROTOCOL.md` # GET /v1/system/status). Disk fullness doesn't move at the 1 Hz gauge cadence, so the panel fetches it once on open rather than subscribing — same one-shot read the install-plan dialog already does for free bytes. Every signed-in user, no role gate.

**What the box is running is a separate one-time poll.** `GET /api/v1/system/version` answers "which versions is this box on", across all three components rather than just the brain: `{ "version", "commit", "host_agent_version"?, "ui_image"? }`. `version` + `commit` are the brain's own build identity, compiled in (`BUILD.md` # Versioning); `host_agent_version` comes from the host round trip (`BRAIN_HOST_PROTOCOL.md` # GET /v1/system/status); `ui_image` is the image reference pinned in the staged control-plane compose — the same declaration the brain reconciles to, and the one the control-plane updater rewrites (`UPDATES.md` # 8.3). Every signed-in user, no role gate: build identity is not per-user data.

**Starting a control-plane update is an admin-only pair of routes.** `POST /api/v1/system/update` with `{ "brain_image"?, "ui_image"? }` starts the update (`UPDATES.md` # 3) and answers `202` with the job: `{job_id, kind, status, started_at}`. `GET /api/v1/system/update/{job_id}` polls it, and once it ends carries `error` `{code, message}` plus `result` `{brain_changed, ui_changed, reverted, failure_mode, revert_error}` — "it broke at the health check and the box was put back" is the answer the admin needs. Refusals: `403` for a member, `422` for no refs or a malformed one, `409` when an update is already running, `502` when host-agent cannot be reached. Starting an update is elevation-class, so the start and every refusal are audited; `success` on that record means "the update started", not "the update worked".

**The job id is host-agent's, not the brain's** (`BRAIN_HOST_PROTOCOL.md` # Pattern B, as built). The brain does not wrap this in one of its own Pattern B jobs, because the brain is one of the two containers being replaced — a brain-side record would die halfway through the operation it was tracking. Polling goes through to host-agent, which stays up, so the status read still works after the brain has been recreated.

**The target is two explicit image refs in the request.** There is no release-manifest poll and no cloud call behind this endpoint yet; the box↔cloud credential that would need is still undesigned (`NEXT.md`, Tier 1). There is no dashboard surface for it yet either — this is the API only.

**The two optional fields are omitted when unknown, and absent means "could not read it", never "not installed".** The endpoint deliberately degrades instead of failing whole — unlike `/system/storage`, where a host failure is a 502. The brain's own version is always available and is the one an updater needs first, so a dead host-agent or a dev box with no staged compose must not cost the caller the fields that are still true. A caller that needs to distinguish "unknown" from "old" checks for the field's presence, which is why these are `omitempty` rather than empty strings.

**No reconnect replay.** This channel is exempt from the `Last-Event-ID` buffer below — replaying stale samples is wrong for a live gauge. A reconnecting client resumes at the next live `sample`; the first event after any connect reports `cpu_pct`/`*_bps` rate fields as `null` (no prior sample to diff against), with real rates from the second sample on. It still counts against the ≤16-stream cap.

### Pattern D — WebSocket (future, reserved)

Reserved for the web terminal (`NEXT.md` Tier 3). HTTP upgrade on the same server. No v1 pre-design. The terminal has its own security implications (root PTY = root on host) that need separate design — `AUTH.md` already locks the gating gesture (re-type dashboard password for a root shell).

## API discipline

**Authentication.** Opaque `moose_session` cookie (per `AUTH.md`). No bearer tokens, no JWTs. The cookie carries the SSE handshake and the future WebSocket upgrade — no separate auth path. CSRF is handled by `SameSite=Strict` on the cookie plus an `Origin` check on state-changing requests.

**Versioning.** The API is **versioned and additive**, not lockstep. Brain serves under `/api/v1/...`. Minor versions are additive — fields are added, never removed or repurposed. The UI bundle declares the API minor it requires (in `version.json`); brain accepts any UI built against `v1.X` where `X ≤ current_minor`. Breaking changes go to `/api/v2`, which the brain serves alongside `/api/v1` during the deprecation window.

This is deliberately *not* lockstep with the brain version. The UI and brain ship as separate images on a shared release channel (`WEB_UI.md` # "deploy + update flow") and iterate at independent cadences. Most UI ships don't move the brain; most brain ships don't move the UI.

**The `426 Upgrade Required` path is the in-tab safety net.** When the user has a dashboard tab open and the UI container updates underneath them, the next API call from the stale tab may declare a `version` the brain no longer supports (or the inverse — the UI just updated to require an API minor the brain doesn't yet serve, during a coordinated ship between minor pull and brain restart). On `426`, the UI shows "moose updated — refresh to continue."

**Public-API posture.** The API the dashboard uses **is** the API a future CLI, third-party app store, or external tool will hit. Concretely:

- Stable URLs, stable error codes, stable event `kind` values.
- No hidden auth shortcuts the dashboard uses but external callers can't (e.g. no "internal" routes outside `/api/v1/`).
- The one carve-out is **operational probes**, which are not API at all (below).
- Rate-limit and abuse posture is locked below (# Rate limiting & abuse) — public-callable from day one means external callers (CLI, third-party stores) need a predictable throttling contract, not just the dashboard.

**Operational probes live outside the API.** `GET /healthz` is the brain's liveness probe: unauthenticated, no versioning promise, absent from the OpenAPI document and the generated TS client. It answers 200 as soon as the HTTP server is answering and checks **nothing** — not SQLite, not Docker, not Caddy, not host-agent. Its caller is the control-plane updater, which waits on it after recreating the brain and reverts both control-plane images if the wait times out (`UPDATES.md` # 3 step 3d), so a dependency check here would roll back a healthy new brain because some unrelated subsystem was sick — and roll it back to an old brain facing the same sick subsystem. Dependency health is a different question with its own surface: `GET /api/v1/health` (`HEALTH.md`), admin-gated and structured.

This does not weaken the public-API posture above. That rule bans routes the *dashboard* uses that an external caller cannot; the dashboard never calls this one. Reach is narrow by construction: Caddy proxies only `/api/*` and `/_moose/*` to the brain, so `/healthz` is not routable from the LAN — the callers who can reach it are on the box's Docker network or on the box itself.

**Codegen.** Split. **Server-side OpenAPI 3 emission lands from day one** — the brain is written using [`huma`](https://huma.rocks), a Go web library that produces an OpenAPI schema as a byproduct of handler registration. Typed request/response Go structs *are* the schema; there is no hand-maintained `openapi.yaml`. The schema is emitted reproducibly **without a running brain** by `make openapi` (`cmd/openapi-gen` → `api.OpenAPIDocument()`), committed at `api/openapi.{json,yaml}`, and held fresh by a CI gate (`make openapi-check`, mirroring `fmt-check`); it is also the substrate for the breaking-change CI below. **Client-side codegen landed in issue #52** — the UI's wire types are now generated from the committed spec via [`openapi-typescript`](https://openapi-ts.dev) (`web-ui`'s `npm run gen:api` → `src/generated/openapi.ts`), and `web-ui/src/api.ts` aliases them under the names the dashboard imports, so call sites are unchanged. The hand-written `fetch` wrapper **stays** — it owns the `ApiError` mapping + 401 handling; a typed `openapi-fetch` *client* (which would rewrite every call site's `{data,error}` handling) remains deferred. The raw SSE streams (`/api/v1/events`, `/api/v1/system/live`) bypass huma, so their event payloads aren't in the spec and their TS types stay hand-maintained (typed SSE is a separate follow-up).

### CI enforcement

The additive-minor discipline above is a *contract* — with in-flight UI tabs during a moose update, and with external callers (third-party stores, CLI, future tooling) per the public-API posture. The cost of breaking it is paid by callers, not the change author. Discipline-by-convention decays; CI is the mechanism that internalizes the cost so a breaking change can't merge silently.

**Mechanism: generated OpenAPI + `oasdiff breaking`.**

On every PR, CI:

1. Builds the brain and writes `openapi.json` (a `make openapi` target or a tiny Go binary that calls `api.OpenAPI()`).
2. Runs `oasdiff breaking origin/main:openapi.json HEAD:openapi.json`. Non-zero exit = breaking change detected = build fails.

[`oasdiff`](https://github.com/tufin/oasdiff) (Tufin, Apache-2.0) has a closed, rule-based notion of "breaking": response field removed; response field type changed or narrowed; enum value removed; endpoint removed; request field newly required; optional response field becoming nullable; error-code enum value removed; etc. The check is **structural, not heuristic** — it sees the declared schema, not a sampled set of responses. A field declared in a Go struct is in the schema whether any test exercises it or not; an enum value listed in a Go type is in the schema whether any test fires it or not.

**Two nudges to make this work cleanly:**

- **Event `kind` and error `code` are first-class Go enum types**, each registered in a single file (`events/kinds.go`, `errors/codes.go`). Each appears in OpenAPI as a named enum schema; oasdiff catches removals from either set the same way it catches field removals.
- **PR template includes a "does this change `/api/v1`?" checkbox.** If checked, the reviewer is on the hook for confirming the change is deliberate — mechanical CI catches the schema, the checkbox catches *intent* (e.g., a copy-paste accident that happens to produce a clean diff).

**Why generated, not hand-written OpenAPI.** A hand-written `openapi.yaml` becomes a second source of truth that drifts from the Go code — and the drift isn't caught until a PR breaks something real for a caller. Generated-from-types makes drift structurally impossible: the schema is the byproduct of the code that serves it.

**Why this over snapshot-testing responses.** Snapshot tests verify what they call. Coverage gaps — endpoint never tested, enum value never observed, error response shape, omitted optional fields — silently let breaking changes through. The schema diff is the contract diff; the snapshot diff is "did anything I happened to look at change?"

**No escape hatches.**

- No "let this one through" CI flag. Bypass is "move to `/api/v2`," not "skip the check."
- No grace period for newly-added enum values to be removed later. Once landed, additive forever.
- No "internal" routes outside `/api/v1` that escape the discipline. Public-API posture is from day one.

**Debuggability is a first-class design constraint** (inherited from `BRAIN_HOST_PROTOCOL.md`). Anything the dashboard does is reproducible with `curl` and a session cookie:

```
curl -b "moose_session=..." http://moose.local/api/v1/apps
curl -b "moose_session=..." -N http://moose.local/api/v1/events
```

Future changes that would make the protocol harder to debug from `curl` need explicit justification.

## Rate limiting & abuse

moose is closed-by-default, LAN + mesh only (`THREAT_MODEL.md` # B1) — there is no public-internet exposure in v1, so this is **not** internet-scale DoS defense (DoS is a named non-goal, `THREAT_MODEL.md` # Out of scope). The realistic threat is a **runaway or buggy client** — a dashboard tab in a reconnect loop, a CLI script polling tight, a third-party store with a bad retry config — or a compromised LAN device doing the same deliberately, grinding a modest single-node box (often an old laptop) into CPU/goroutine/memory pressure. The posture is **throttle, don't ban; in-memory state; resets on brain restart; log the source IP but never blacklist** — the same philosophy `AUTH.md` # Rate limiting set for the login path.

Three orthogonal planes, plus the login throttle that already exists:

1. **Per-session request rate** — a token bucket keyed on the `moose_session` token, governing all authenticated short requests (Pattern A, plus job create/poll/cancel). Default **120 req/min sustained, burst 60** — sized so a normal dashboard (TanStack Query + a few SSE streams + job polling) never trips it, but a runaway loop does. SSE streams and the streaming `/files/content` endpoints are **exempt** from this bucket — they are long-lived, not short requests.
2. **Per-IP request rate** — a token bucket keyed on client IP, governing the **unauthenticated** allowlist only (`/login`, `/setup`, version probe). Default **30 req/min/IP**. This sits *above* the login throttle: `/login` keeps its stricter per-username exponential backoff + per-IP bucket (`AUTH.md` # Rate limiting); this plane is the backstop so the *other* unauthenticated routes can't be hammered to burn DB/CPU work. Logs the IP, does not ban (LAN reality). **`GET /healthz` is exempt** (# API discipline — operational probes): the updater polls it once a second for up to 60s after recreating the brain, which is twice this budget, and a `429` would read as "the new brain is unhealthy" and revert a working update. The handler writes a fixed body and touches nothing, so serving it costs about what refusing it would.
3. **SSE-stream concurrency** — the ≤16 concurrent-streams-per-session cap (# Stream cap, above). This is a **separate budget from request rate**: opening a stream consumes one of the 16 slots but does *not* draw from plane 1's bucket, so a tight EventSource reconnect loop is bounded by the slot cap + reconnect interval, not by req/min.

**429 contract.** A throttled request gets HTTP `429` with the standard envelope `{ "code": "rate-limited", "message": "...", "details": { "scope": "session" | "ip", "retry_after_s": N } }` **and a `Retry-After` header** (seconds) so well-behaved external callers back off correctly. The `rate-limited` code is distinct from the login lockout's `login.lockout` (`AUTH.md`) — request throttling and account lockout are different events. The dashboard surfaces a non-alarming "moose is busy — retrying…" and auto-retries after `Retry-After` (TanStack Query backoff); this is a backstop, not a normal-operation banner.

**Mechanism.** A middleware in the brain's chain, ordered **after** auth resolves the session but **before** handlers — so it keys on the session token when authenticated, or falls back to client IP on the unauthenticated allowlist. In-memory, mutex-guarded buckets with periodic GC of idle entries; no persistence (resets on restart, like the login throttle). Plane 3 reuses the # Stream cap counter.

**What "client IP" means.** A per-IP bucket is only a throttle if the key is something the caller cannot choose. The brain reads `X-Forwarded-For` **only** when the request's immediate peer is a configured trusted proxy, and then takes the **last untrusted hop** in the chain rather than the first; from any other peer the header is ignored entirely and the peer address is the key. The trusted set defaults to loopback plus Docker's bridge pool (`127.0.0.0/8`, `::1/128`, `172.16.0.0/12`) — the brain container publishes no port, so every peer it can have is on the box's own Docker network — and is narrowed or widened with `MOOSE_TRUSTED_PROXIES`. It must stay narrower than "the private ranges": a trusted hop is skipped while walking the chain, so trusting the ranges LAN **clients** live in (`192.168.0.0/16`, `10.0.0.0/8`) would erase each client's own hop and key the whole household on Caddy's address — one device could then spend everyone's budget, the exact failure these planes exist to prevent. Caddy, as the box's edge, trusts **no** client: it declares an empty trusted-proxy set and so replaces an inbound `X-Forwarded-For` with the address it actually saw. The same rule and the same derived address govern the login throttle (`AUTH.md` # Rate limiting) and what the audit log records as a request's origin.

**Explicit non-goals (v1).** No IP banning (throttle only). No persistent or distributed limiter state (single node). No per-route custom budgets beyond the three planes — uniform until a specific route bites (no premature abstraction). No global whole-box request ceiling (per-session + per-IP suffices for 1–4 users). No per-session concurrent-jobs cap (job *creation* counts against plane 1; install/update are serialized by the lifecycle transaction owner). A per-session **file-transfer** concurrency cap for streaming `/files/content` is deliberately deferred to `NEXT.md` (those endpoints are ungoverned by all three planes; pin a counter when it bites).

## Locked decisions

- **Transport:** HTTPS via Caddy. No direct brain port exposure.
- **Wire format:** HTTP/1.1 + JSON, versioned URL prefix `/api/v1/...`.
- **API patterns:** sync request/response (A) for <5s; jobs (B) for longer or progress-reporting ops; SSE (C) for one-way streams; WebSocket (D) reserved for future bidirectional needs.
- **Authentication:** opaque `moose_session` cookie. No bearer tokens. SSE/WS auth via the same cookie.
- **CSRF:** `SameSite=Strict` cookie + `Origin` check on state-changing requests.
- **Versioning:** API-versioned, additive-minor. `/api/v1` minors only add fields; breaking changes go to `/api/v2`. UI and brain ship independently on a shared release channel (`WEB_UI.md`). UI declares `X-Moose-API-Version`; brain returns 426 if it can't serve that minor.
- **Additive-minor discipline.** Fields in `/api/v1` are never removed or repurposed. New fields are always optional. Event `kind` values are added, never removed (deprecation = stop emitting). **CI enforces via generated OpenAPI + `oasdiff breaking`** (see # CI enforcement). Bypass is `/api/v2`, not a skip flag.
- **Errors:** HTTP status + `{code, message, details?}` body. Codes are stable strings.
- **Event `kind` values are enumerated in the schema.** Adding a new kind is an API-version-bumping change.
- **SSE reconnect:** monotonic `id`, ~256 KB per-stream rolling buffer, `Last-Event-ID` replay, single `{"lost": true}` event on overflow.
- **Stream cap:** ≤16 concurrent SSE streams per session.
- **Rate limiting:** throttle-not-ban, in-memory, three orthogonal planes — per-session request rate (120/min, burst 60), per-IP request rate on the unauthenticated allowlist (30/min, with `GET /healthz` exempt), and the per-session SSE-stream concurrency cap (separate budget). `429` carries `code: "rate-limited"` + a `Retry-After` header. LAN-scoped runaway-client defense, not DoS protection (`THREAT_MODEL.md` non-goal). See # Rate limiting & abuse.
- **Codegen split.** Server-side: OpenAPI 3 emitted by the brain via [`huma`](https://huma.rocks) from day one (substrate for CI enforcement); reproducibly re-emitted server-lessly by `make openapi`, committed at `api/openapi.{json,yaml}`, freshness-gated by `make openapi-check`. Client-side: wire **types** generated from the committed spec via `openapi-typescript` (issue #52); the hand-written `fetch` wrapper stays (keeps `ApiError` + 401), so a typed `openapi-fetch` client is still deferred. SSE payload types stay hand-maintained (SSE bypasses huma).
- **Public-API posture from day one.** Dashboard uses the same routes any external caller will hit. No internal carve-outs.
- **Operational probes are not API.** `GET /healthz` sits outside `/api/v1`, outside the OpenAPI document, outside the generated client, and outside plane 2's throttle. Unauthenticated, liveness-only, checks no dependency — it exists so the control-plane updater can tell "the brain is serving" from "revert this update" (`UPDATES.md` # 3).
- **Debuggability is a first-class design constraint.** Future changes that hurt `curl`-debuggability need explicit justification.

## Knock-ons to other docs

- `CONTROL_PLANE.md` — points to this doc as the authoritative spec for the brain↔UI boundary.
- `BRAIN_HOST_PROTOCOL.md` — sibling protocol; SSE/jobs patterns deliberately identical.
- `AUTH.md` — `moose_session` cookie semantics live there.
- `WEB_UI.md` — client-side consumers of this protocol (`@tanstack/vue-query`, `useEvents()`, `useJob()`).
- `NEXT.md` — carries the OpenAPI codegen-timing follow-up and the deferred per-session file-transfer concurrency cap.
