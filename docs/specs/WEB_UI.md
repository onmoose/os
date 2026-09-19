# Web UI

> Working spec for the moose dashboard — the web app that the browser talks to. Companion to `CONTROL_PLANE.md` (where the API server lives), `BRAIN_UI_PROTOCOL.md` (the contract the UI consumes), `AUTH.md` (session cookie), `UPDATES.md` (UI bundle update path). This doc owns the **stack and deploy model**; the **logged-in information architecture** (home screen, app model, navigation, top bar) lives in `DASHBOARD.md`.

## Locked: separate codebase, separate container

- UI lives in its **own repository**, developed independently of the brain. Vite for dev (HMR).
- Dev loop: Vite dev server proxies `/api/...` calls to the brain. Instant reload on UI changes. No brain rebuild needed for frontend work.
- **API contract is versioned and additive.** Brain serves under `/api/v1/...`. Minor versions are additive — fields are added, never removed or repurposed. UI bundle declares the API minor it requires in `version.json`. Brain returns `426 Upgrade Required` on mismatch. The 426 path is the in-tab safety net for "user had a tab open while the UI container updated" — not a transient-during-OS-update artifact. See `BRAIN_UI_PROTOCOL.md` # "API discipline" for the full versioning posture.
- **Why separate codebase + container:** UI iteration cadence is naturally faster than brain iteration cadence. A coupled ship model forces every CSS tweak through a brain release. Separating them with a stable API contract is the standard web-app shape; we keep it.

## Locked: deploy model — dedicated `moose-ui` container

- The UI ships as its own Docker image: **`caddy:alpine` base + UI bundle baked in at build time**.
- Caddyfile is trivial: serve `/srv/ui`, SPA fallback to `index.html`, gzip + brotli + ETag on by default.
- The existing LAN-facing Caddy (the reverse proxy, separate container, per `CONTROL_PLANE.md`) routes:
  - `/api/v1/*` → `moose-brain` container
  - `/api/v1/events`, `/api/v1/.../log` (SSE streams) → `moose-brain` (same as above, but with buffering disabled)
  - everything else → `moose-ui` container
- **No shared volumes.** UI bundle is baked into the image at CI build time. Each `moose-ui` image release is self-contained.
- **Security profile.** The UI container is a static file server. Read-only root filesystem, no secrets, no outbound network, no host privileges, no Docker socket access. Simplest container in the stack.

**Why `caddy:alpine` over alternatives:**

- **`nginx:alpine`** — equally competent at static serving, but nginx isn't otherwise in our stack. Using Caddy keeps "one HTTP server toolkit" across the appliance.
- **`scratch` + tiny Go static server** — smallest image (~5 MB), but reinvents brotli negotiation, ETag, conditional requests, and cache-control config. Cost > benefit at SPA scale.
- **Reusing the LAN Caddy container** with the UI bundle as a volume — couples UI updates to Caddy container updates, awkward semantics ("am I updating the reverse proxy or the UI?"). Rejected.

## Locked: deploy + update flow

- Two images are published per release: `moose-brain` and `moose-ui`. Both versions appear in the release manifest (`UPDATES.md`).
- The updater pulls and recreates **only what changed.** Most UI ships: only `moose-ui` recreates, brain keeps running. Most brain ships: only `moose-brain` recreates. UI keeps serving.
- **Coordinated ship.** When a release manifest bumps both versions in the same entry (e.g., a UI change that depends on a new brain endpoint), the updater treats them as one transaction — pull both, recreate both, verify both healthy, commit.
- **One user-facing toggle.** "Auto-update moose" governs the whole channel. The user never thinks about brain vs. UI.

## Locked: stack

| Layer | Pick | Note |
|---|---|---|
| Language | **TypeScript**, `strict` | `noUncheckedIndexedAccess: true`. |
| Framework | **Vue 3** | Composition API + `<script setup>` only. No Options API mixed in. |
| Build | **Vite 5+** | Already locked above. |
| Routing | **Vue Router 4** | History mode; Caddy serves `index.html` for unmatched routes. |
| Client state | **Pinia** | UI mode, form drafts, ephemeral client state only. |
| Server state | **`@tanstack/vue-query` v5** | All API data goes through Query. Single cache, single source of truth. |
| HTTP client | Native **`fetch`** + ~30 LOC wrapper | Prepends `/api/v1`, sends credentials, parses `{code,message}` errors into a typed `ApiError`. Swap for `openapi-fetch` when codegen lands. |
| SSE | Native **`EventSource`**, wrapped in `useEvents()` | One subscription at app mount; switches on `event.kind` and calls `queryClient.invalidateQueries(...)`. |
| Jobs | `useJob(jobId)` composable | Wraps `useQuery` with `refetchInterval` and `enabled: status not in terminal`. |
| Styling | **Tailwind CSS 4** | CSS-based config (`@theme`), no `tailwind.config.js`. The `@theme` block is the **Oatmeal** design system shared with cloud — the `--color-olive-50…950` OKLCH palette + Inter (`--font-sans`) / Instrument Serif (`--font-display`), lifted verbatim from cloud's `input.css`. The shadcn-vue semantic tokens (`background`, `foreground`, `accent`, …) are **repointed onto olive values** in that one block, so the whole app recolors from a single file; `accent` is monochrome dark olive; the four status tokens `destructive`/`warning`/`info`/`success` stay semantic (red/amber/blue/green), retuned to muted OKLCH so they read as olive-adjacent while keeping each notification + health severity distinguishable (`NOTIFICATIONS.md` # Severities). Fonts are **self-hosted** (`src/assets/fonts/`, declared in `fonts.css`), never the Google Fonts CDN — the `.local` LAN may be offline (`DECISIONS.md` 2026-07-01). |
| Components | **shadcn-vue** on **reka-ui** | Copy-paste components owned in our repo. reka-ui is the headless primitive layer (Vue port of Radix). |
| Icons | **lucide-vue** | shadcn ecosystem default. |
| Package manager | **pnpm** | Faster installs, smaller `node_modules`. |
| Lint / format | **ESLint** (`eslint-plugin-vue` + `@typescript-eslint`) + **Prettier** | Standard. |
| Node | Latest LTS | Pinned via `.nvmrc`. |

## Health & degraded mode surfacing

The brain may be running in *degraded mode* — one or more health issues active that block specific operations (see `HEALTH.md`). The dashboard surfaces this consistently:

- **`useHealth()` composable** wraps `useQuery(['health/issues'])` and reacts to `health.issue_raised` / `health.issue_cleared` / `health.issue_updated` events on the global SSE stream. Active issues are *server state*, so they live in the Query cache, **not** a Pinia store (per *Server state lives in Query* below — the locked rule); the one cached query is the shared store, and `useHealth()` derives `activeIssues` / `blocksApps` / `blocksWrites` / `blocksUsers` from it for every component. (Issue #12.)
- **Global banner** in the dashboard chrome whenever any `critical` or `error` issue is active. Click → the inline active-issues list on Home (`#health-issues`); a dedicated Issues route is a follow-up if that list grows unwieldy.
- **Inline cards** in the relevant section (Storage page for storage issues, Updates page for version issues, per-app card for `app-image-partial`, etc.). Each card carries the issue's primary action button.
- **Disabled action affordances.** Components that perform potentially-blocked operations consult the health store and render disabled with an explanatory tooltip ("Disabled because: data drive isn't connected"). A `<HealthGated :blocks="'apps'">` wrapper component standardizes this.
- **Toast on clear.** When an issue transitions to cleared, a brief toast confirms it. No modal, no interrupt — but the user *sees* the transition rather than the box silently auto-healing.
- **No modals for issues.** Banners and inline cards surface in-place; the user reads them when they look. Modals are reserved for user-initiated confirmations (Tier-2 destructive actions).
- **409 with `blocked-by-health-issue`** — the API client surfaces these as a toast with a "View" button that scrolls to the relevant banner.

## Architectural rules baked into the stack

- **Server state lives in Query, not Pinia.** Anything fetched from the brain — apps, users, settings, jobs — flows through `useQuery`. Pinia is reserved for genuinely client-side state. This prevents the standard pattern of "I have the data in two places and they're out of sync."
- **The SSE event stream is the cache-invalidation channel.** `useEvents()` subscribes once at app mount. Components don't subscribe to events directly; they read via `useQuery` and get fresh data when relevant events arrive. Push and pull share one cache.
- **`<script setup>` only.** No Options API, no `defineComponent({...})` blocks. Code-review enforced.
- **OpenAPI-codegen-ready.** The hand-rolled `apiClient` and the call-site shape mirror what `openapi-fetch` will produce. When codegen lands (`BRAIN_UI_PROTOCOL.md` # "API discipline"), it's a swap, not a rewrite.

Forms library, testing stack, dark-mode trigger, i18n, and accessibility cadence are downstream decisions tracked separately.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md).
