# web-ui code architecture

How the dashboard front-end is built, as it exists in the repo today. This is the **code-level map** for someone about to edit `web-ui/`. It is the third leg of three web-ui docs — read them in this order:

- **`docs/specs/WEB_UI.md`** — the design source of truth: stack picks, deploy model (`moose-ui` container), versioning posture, and the architectural rules ("server state lives in Query", "SSE is the cache-invalidation channel", "`<script setup>` only"). Read it first; this doc assumes those decisions.
- **`docs/architecture.md`** — the *external* contract: how the browser, web-ui, brain, and Caddy wire together (REST `/api/v1/*` + SSE, cookie auth).
- **This doc** — the *internal* shape: folder layout, the cross-cutting modules, state model, and recipes for adding a view/component/query.

For how to *run* it (Vite dev server, Node version, CI), see [`running-locally.md`](running-locally.md). For the API contract the UI consumes, see `docs/specs/BRAIN_UI_PROTOCOL.md`.

## Stack, as built

Vue 3 (Composition API, `<script setup>` only) + Vite 5 + TypeScript `strict` (with `noUncheckedIndexedAccess`). Server state through `@tanstack/vue-query` v5; routing through Vue Router 4 (history mode); styling through Tailwind CSS 4 (CSS-config via `@theme`, no `tailwind.config.js`). Icons via `lucide-vue-next`. `reka-ui` + the `cn()` helper (`clsx` + `tailwind-merge`) are present as the shadcn-vue scaffolding so components can be added via the shadcn CLI later. The first owned components live in `components/ui/` — `Button.vue` (the pill idiom, `primary`/`secondary`/`ghost` variants, and an `as` prop for the cases that must be a real `<a>` — an "Open in a new tab" affordance is a link, not a button, but it is the same pill) and `Heading.vue` (the `font-display` display idiom) — hand-written from the Oatmeal Tailwind patterns (not the shadcn CLI, not Oatmeal's `.tsx` source; #261) and consuming the olive tokens via `cn()`. Prefer `<Button>` / `<Heading>` over ad-hoc `<button>` / heading markup for the pill + display idioms; other surfaces still use plain elements with Tailwind classes and the design tokens in `style.css`.

`main.ts` is the whole bootstrap: `createApp(App)` with Pinia, the router, and `VueQueryPlugin`. That's it — twelve lines.

### Where the as-built diverges from `WEB_UI.md`

The spec is the target; a few picks haven't landed yet. Don't treat these as bugs — they're staged work:

- **Package manager is npm, not pnpm.** There's a `package-lock.json` and `running-locally.md`/CI use `npm ci`. The spec names pnpm; revisit if/when we switch.
- **No ESLint/Prettier yet.** The spec lists them; `package.json` has neither. `vue-tsc --noEmit` (run by `npm run build` and CI) is the only automated gate today.
- **Pinia is registered but unused.** The spec reserves Pinia for client-side state; in practice the cross-cutting client state (auth, toasts, elevation) is held as **module-singleton refs** (see below), which is simpler at this size. Pinia is wired in `main.ts` and ready when a real store appears (the spec's `useHealth()` health store is the likely first).
- **`useJob()` is `waitForJob()` for now.** The spec's `useJob(jobId)` composable (a `useQuery` with `refetchInterval`) isn't built as a shared composable; `api.ts` has a plain poll loop instead. The install progress page is the one place that already polls with `useQuery` and `refetchInterval`, inline.
- **`useHealth()` / `<HealthGated>` / degraded-mode banners** (WEB_UI.md # Health & degraded mode) are not built yet.

When you close one of these gaps, delete its bullet here in the same change.

## Folder layout

```
web-ui/
├── index.html              # Vite entry; mounts #app
├── vite.config.ts          # @ → src/ alias; dev proxy /api → brain (SSE-aware)
├── tsconfig.json           # strict + noUncheckedIndexedAccess; @/* path
├── package.json            # npm scripts: dev / build / preview / gen:api
└── src/
    ├── main.ts             # app bootstrap (Pinia + router + Vue Query)
    ├── App.vue             # auth-aware root: bootstrap → Setup | AppShell | Login
    ├── style.css           # Tailwind 4 entry + @theme design tokens
    ├── router.ts           # Vue Router 4 route table (lazy-imported views)
    │
    ├── api.ts              # fetch wrapper + ApiError + generated-type re-exports
    ├── auth.ts             # session lifecycle: bootstrap/login/setup/logout
    ├── useEvents.ts        # one SSE subscription → Query cache invalidation
    ├── toasts.ts           # app-wide ephemeral error-toast channel
    ├── elevate.ts          # 5-min elevation flow + withElevation() wrapper
    ├── useNotifications.ts # notification list/badge queries + mutations
    ├── useNotificationMutes.ts
    │
    ├── generated/
    │   └── openapi.ts      # GENERATED from api/openapi.json — do not hand-edit
    ├── lib/
    │   └── utils.ts        # cn() class-merge helper (shadcn convention)
    │
    ├── mailProviderForm.ts # outgoing-mail form shape + preset rules, shared by
    │                       #   the Settings add flow, the inline edit form, and
    │                       #   the install flow's email add form
    ├── sshDraft.ts         # the SSH screen's unsaved draft: the save body, and the
    │                       #   sessionStorage copy that survives a reload or the
    │                       #   hosted owner's portal confirm
    ├── useInstall.ts       # catalog-app install flow: which copies the caller has
    │                       #   (detail-page button state) and the POST with its
    │                       #   409/422 branches; see "Install flow" below
    ├── aiProviders.ts      # AI slots from manifest roles, provider tiles, model
    │                       #   pickers, bindings and the requires gate
    ├── installSteps.ts     # the install flow as pages: plan -> pages, AI needs,
    │                       #   the key picked in advance, the session-storage
    │                       #   draft, and the page that owns a 422
    │
    ├── views/              # one component per route (lazy-loaded)
    │   ├── HomeView.vue        # installed-app grid
    │   ├── StoreView.vue       # catalog browse grid (cards → detail page)
    │   ├── AppDetailView.vue   # /store/:id — app detail page; Install starts here
    │   ├── InstallSetupView.vue    # /store/:id/install?step=: the install flow's pages
    │   ├── InstallProgressView.vue # /store/:id/install/:jobId: install job progress
    │   ├── CustomInstallView.vue  # Door-2 custom-container form (admin-only)
    │   ├── FilesView.vue
    │   └── settings/           # Settings left-nav shell + its sections
    │       ├── SettingsLayout.vue        # sidebar + nested-route content pane
    │       ├── AccountSection.vue        # identity + self-service password change
    │       ├── SshSection.vue            # per-account SSH opt-in + public keys (draft + Save)
    │       ├── NotificationsSection.vue  # per-category bell mutes
    │       ├── InstalledAppsSection.vue  # manage/uninstall/logs list
    │       ├── ActivitySection.vue       # audit-log browser (all users)
    │       ├── UsersSection.vue          # admin-only user management
    │       ├── InstalledAppDetailSection.vue # one app: controls, email, secrets, settings + AI service pickers, logs
    │       ├── LLMProvidersSection.vue   # /settings/ai, Integrations → AI services: the user's own AI accounts
    │       ├── EmailSection.vue          # Integrations → Email: the user's own SMTP account list
    │       ├── EmailAddSection.vue       # /email/add + /email/add/:preset (/settings/mail/* redirect here)
    │       └── AboutSection.vue          # product identity
    │
    └── components/         # reusable chrome + dialogs
        ├── AppShell.vue        # signed-in chrome; mounts useEvents() once
        ├── TopBar.vue, Dock.vue
        ├── AppTile.vue         # dashboard launcher tile (opens the app)
        ├── StoreAppCard.vue    # store browse card (links to the detail page)
        ├── AppGlyph.vue        # icon-less fallback: manifest icon_glyph → Lucide icon, else AppWindow
        ├── MailProviderLogo.vue # provider mark from assets/mail-providers/, by preset id
        │                        #   (that folder's README is the how-to for adding one)
        ├── AIProviderLogo.vue   # AI service logo (box-proxied), icon fallback
        ├── AISlotPicker.vue     # AI service row: tiles, account pick + inline add,
        │                        #   model pickers; shared by the setup page and the
        │                        #   app's settings screen
        ├── SplitButton.vue
        ├── ElevateDialog.vue
        ├── ToastHost.vue
        └── install/            # the pages of the install flow
            ├── OptionCards.vue       # selectable card grid + "More" divider + search (AISlotPicker)
            ├── ConfigFieldInput.vue  # one config field (text / secret / enum / bool)
            ├── InstallInfoBox.vue    # the quiet info box: permissions, size, space warning
            ├── FolderChoices.vue     # the folders page: source and subfolder per folder
            ├── AccountList.vue       # the B layout: saved AI keys or email accounts
            ├── ServiceGrid.vue       # the AI service grid (radio group)
            ├── AIKeyForm.vue         # a new AI key, saved on the page's Continue
            ├── MailServiceGrid.vue   # the email service grid: personal, then sending services
            └── MailAddForm.vue       # a new email account, saved on the page's Continue
```

A handful of top-level `.vue` files (`Login.vue`, `Setup.vue`, `NotificationBell.vue`, `LiveResources.vue`) sit directly in `src/` rather than `components/` — they're the pre-shell / standalone surfaces. New reusable components go in `components/`; new routed screens go in `views/`.

## State model — three tiers, in order of preference

1. **Server state → TanStack Query.** Everything fetched from the brain (apps, catalog, users, notifications, jobs) goes through `useQuery`/`useMutation` keyed by a stable array (`["apps"]`, `["notifications"]`, …). One cache, one source of truth. This is the load-bearing rule from `WEB_UI.md` — never stash fetched data in a local `ref` that can drift.
2. **Client state → module-singleton refs** (today) / Pinia (when it grows). `auth.ts`, `toasts.ts`, and `elevate.ts` each export a module-level `ref`/`reactive` plus imperative functions and a `useX()` accessor returning computed views. Any module can import and mutate; components read reactively. This is deliberately not Pinia yet — see the divergence note above.
3. **Component-local `ref`** for form drafts and view-local UI toggles.

### The cache-invalidation channel

`useEvents.ts` opens **one** `EventSource("/api/v1/events")` and is called exactly once, in `AppShell.vue` (so it covers every signed-in view). It does not carry payloads into components — it listens for event kinds (`app.state_changed`, `app.installed`, `app.uninstalled`, `notification.created`, `notification.updated`) and calls `queryClient.invalidateQueries(...)`. Components stay pull-only via `useQuery` and re-render when the relevant query refetches. Push and pull share the one cache (WEB_UI.md, BRAIN_UI_PROTOCOL.md Pattern C). When you add a new live-updating resource, add its event kind(s) here rather than subscribing from the component.

## Cross-cutting modules

- **`api.ts`** — the ~30-LOC `fetch` wrapper. `api.get/post/put/patch/del` prepend `/api/v1`, send `credentials: "include"`, and normalize both error shapes the brain emits (huma's `{detail,title,errors}` and the jobs `{code,message}`) into a typed `ApiError(code, message, status)`. A 401 from *any* call fires the `onUnauthenticated` handler (registered by `auth.ts`) to drop the session. It also re-exports the **wire types** as friendly aliases (`User`, `Instance`, `CatalogEntry`, …) sourced from `generated/openapi.ts`, plus a few hand-maintained types for endpoints that bypass huma codegen (the Door-2 custom-install request/result types, and the `Scope` literal union the generator emits as a bare string).
- **`auth.ts`** — owns the session lifecycle and the `currentUser`/`hasUsers`/`booted` singletons that drive `App.vue`'s three-way branch. `bootstrap()` runs `GET /auth/state` → (`/me` | login | setup). `setup()`/`setupComplete()` are split intentionally so the Setup view stays mounted to show the one-time recovery code before flipping to the shell. Call `refreshCurrentUser()` in the `onSettled` of user-management mutations so `single_user_mode` stays accurate without a reload.
- **`elevate.ts`** — the 5-minute re-prompt window for destructive admin ops (`USERS_AND_GROUPS.md` # Elevation in the UI). Wrap a mutation in `withElevation(fn)`: it runs `fn`, and on a `403 elevation_required` it drives the single `ElevateDialog` (mounted in `AppShell`), elevates the session, and retries once. Inside a live window the prompt never shows. A user cancel rejects with `elevationCancelled` — map it to a no-op, not an error. For the hosted box owner the confirm step is a full-page portal round-trip, so the pending call never resumes. A screen that holds a draft keeps it in `sessionStorage` itself (the SSH screen does, via `sshDraft.ts`), and reads `isLeavingForConfirm()` to skip its unsaved-changes warning for that one leave.
- **`toasts.ts`** — app-wide ephemeral feedback. `pushErrorToast(message)` from anywhere; `<ToastHost>` (mounted in `AppShell`) renders the live list, auto-dismissing after 6s. Error-only today (the rollback feedback for optimistic notification mutations); success/confirm toasts extend this same channel when they land.

## Routing

`router.ts` is a flat lazy-imported table (history mode). Four primary destinations mirror the dock (`DASHBOARD.md` # global navigation): Home, Files, Store, Settings. Admin-only screens (`/store/custom`, `/settings/users`) **guard the role inside the view component** rather than via a router guard — follow the `CustomInstallView` pattern when adding another admin-only screen. Unknown paths redirect to `/` so the SPA never 404s its own chrome (production Caddy also serves `index.html` for unmatched routes).

The Store is a **browse → detail** pair: `/store` (`StoreView`) is a grid of `StoreAppCard`s (logo + name) — filterable by a page-wide search and category pills — that link to `/store/:id` (`AppDetailView`), the app-store-style detail page where the description, screenshots, and the Install flow live. `/store/custom` is declared before `/store/:id` (and Vue Router ranks the static segment higher anyway, so `custom` never matches the `:id` param). Installing a catalog app adds two routes under the detail page: `/store/:id/install` (`InstallSetupView`, `?scope=household` for the household install) and `/store/:id/install/:jobId` (`InstallProgressView`).

When an app has no raster icon (`icon_url`), both the card and the detail header fall back via **`AppGlyph`**, which renders the Lucide icon named by the manifest's `icon_glyph` (kebab-case) or the generic `AppWindow`. `AppGlyph` imports the Lucide set with a lazy `import("lucide-vue-next")`, so the ~900 KB icon library is split into its own chunk that loads only when a glyph fallback is actually rendered — never on the main bundle. In a curated catalog most apps ship a real logo, so that chunk rarely loads; if glyph-fallback usage ever becomes common, switch `AppGlyph` to per-icon dynamic imports so only the few used glyphs load.

## Install flow

A catalog install goes from the detail page's Install button (and the split button's household item) to the install flow at `/store/:id/install`, then to the progress page. The flow is one question per page (`docs/specs/INSTALL_STEPS.md`, `DASHBOARD.md` # Install authorization). `InstallSetupView.vue` fetches `GET /catalog/:id/install-plan` and draws every page: a question page is `?step=<name>`, the last page has no step. On Install it sends `POST /apps`, and a 202 replaces the URL with the progress page, which polls `GET /jobs/:id` with a `useQuery` `refetchInterval` until the job ends. Because the job id is in the URL, a reload resumes it. A 404 on the job (the brain restarted and forgot it) stops the polling and says so.

`src/installSteps.ts` is the pure part. `planNeeds` sorts the plan into pages by the build rules (a required AI group, the optional AI row, the "needs these to run" page, the Extra settings row); `aiNeeds`, `needOfStep`, `usableAccounts`, `choiceFor` and `pickInAdvance` handle the AI pages, one need at a time, one service per need; `stepForError` maps a 422's `location` to its page. The view keeps a draft (the first-time pages, the pages done, the answers) and writes it to `sessionStorage` under `moose.install.v1.<user>.<app>.<scope>`, without secret field values. The draft is removed after the install starts or on Cancel. The first-time pages are fixed when the draft is made, so the step counter does not change. Opening the last page while a required answer is missing redirects to the first page that owns it. Every page saves on Continue: a page-local copy for the settings, Extra settings, email, folders and For pages (the settings copies stay in memory only, never in the draft), and for a new AI key or email account the form component's `save()`, which the view calls from Continue (`AIKeyForm`, `MailAddForm`, exposed with `defineExpose`). No page has a Save of its own. The seeding watch sits at the end of the view's setup, because with the plan cached it runs at once and must see every value declared.

`src/useInstall.ts` holds the shared parts. `useAppInstances(manifestId)` finds the caller's household and own-personal copies in the `["apps"]` cache. It says whether the detail page shows Open, Install, or "Installing…", and whether the household item is offered. "Installing…" comes from the instance row being in the `installing` state, which the brain creates at the start of the job, so it needs no local state and holds across a reload. `useInstallSubmit(manifestId)` is the POST with its two error branches: 409 `duplicate-install` becomes a warn-don't-block banner with "Install my own copy" (a retry with `confirm: true`), and any other failure (a 422 election) shows inline above the Install button.

The progress page folds the brain's ~15 lifecycle steps into four phases in the order the brain runs them: Preparing, Downloading (`resolving_digests`, which pulls the images), Setting up, Starting. The phase never goes back, and an unknown step keeps the last known phase. The wording stays in the view.

The pages' parts live in `components/install/`. The AI pages read `GET /api/v1/ai-providers` (query key `["ai-providers"]`) and the user's accounts (`["ai-accounts"]`); the tile rule is still `aiProviders.ts` (`slotFor`, `fits`), and "My own server" is the UI's own tile, whose accounts carry provider id `openai_compatible`. An empty provider list (the catalog is not reachable, or serves none) means no AI pages: every field is a plain field, so an install is never blocked on provider data. The email pages read `GET /api/v1/mail-presets` through `useMailPresets` in `mailProviderForm.ts`; the preset table gives the order (Gmail and iCloud, marked `personal`, first) and the numbered steps. `AccountList` is shared by the AI key list and the email account list, with the logo in a slot. The grids are radio groups: one tile in the tab order, the arrow keys move the choice.

The same `AISlotPicker` draws the app's settings screen (`InstalledAppDetailSection.vue`, `INSTALL_SETUP.md` piece 4). There the choices start from the `ai_bindings` of `GET /apps/{id}/config` (`choiceFromBinding`), a slot with values and no binding stays raw fields under "set by hand" until the user asks to pick an account, and Save sends the changed fields plus the changed slots (`bindingChanges`: a new or changed binding, or `account_id: ""` for a removed one) in one `PUT`. A choice whose account is not in the caller's `["ai-accounts"]` list is another user's, and the picker says "Someone else's account". A binding whose provider has no tile (it left the provider data, or the data did not load) gets its own card with **Remove**, which sends the `account_id: ""` clear, and its fields are never raw inputs. The choices are seeded when the page opens an app and again only when nothing is unsaved or right after a save, so a background refetch keeps what the user picked. User-facing text says "AI service"; code and API names keep `ai`. An account edit or delete may answer with a `job_id` for the apps it restarts: the Settings screens follow it with `waitForJobOk` (`api.ts`) and then invalidate `["apps"]` and `["app-config"]`, so tiles and the settings screen pick up `needs_setup`.

## Styling

`style.css` is the Tailwind 4 entry: `@import "tailwindcss"` then an `@theme` block of design tokens named to the **shadcn-vue CSS-variable convention** (`--color-background`, `--color-card`, `--color-accent`, …) so shadcn components added later inherit the palette automatically. The block **is** the Oatmeal design system shared with cloud — the `--color-olive-50…950` OKLCH ramp + Inter / Instrument Serif — and the semantic tokens are repointed onto olive values, so the whole app recolors from this one file (`WEB_UI.md` # Styling; `DECISIONS.md` 2026-07-01). Light theme only for now (dark-mode trigger is deferred). Use the semantic token classes (`bg-background`, `text-muted-foreground`, `border-border`) rather than raw hex so the eventual dark theme is a token swap. Fonts are **self-hosted** in `src/assets/fonts/` (with OFL notices) and declared via `@font-face` in `src/assets/fonts/fonts.css`, imported from `main.ts` — kept out of `style.css` because Tailwind's Lightning CSS transform drops the first `@font-face` in the `@import "tailwindcss"` entry.

## OpenAPI codegen workflow

The brain's huma handler structs are the single source of truth for wire types. `src/generated/openapi.ts` is generated by `openapi-typescript` from `api/openapi.json`:

```
npm run gen:api      # openapi-typescript ../api/openapi.json -o src/generated/openapi.ts
```

Regenerate after any brain DTO change, and re-export the new schema as a friendly alias in `api.ts` (don't import `components["schemas"][...]` from call sites). CI's `make openapi-check` keeps the committed `api/openapi.json` in sync with the Go code, so the generated types can't silently drift. Never hand-edit `generated/openapi.ts`. See `docs/progress/openapi-codegen.md` for the why (including the `package.json` `overrides` pinning the codegen dependency closure to pre-May-2026 releases).

## Recipes

**Add a screen:** create `views/FooView.vue` (`<script setup>`), add a lazy route in `router.ts`, link it from `Dock.vue` or the relevant parent. Admin-only? Guard the role in the view like `CustomInstallView`/`UsersSection`.

**Add a Settings section:** create `views/settings/FooSection.vue`, add a nested child route under `/settings` in `router.ts`, and add a nav item to `SettingsLayout.vue` (`adminOnly: true` hides it from members). The section renders inside the shell's content pane — no breadcrumb or back-link; the sidebar is the navigation.

**Fetch brain data:** `useQuery({ queryKey: ["foo"], queryFn: () => api.get<Foo>("/foo") })`. If the brain emits an SSE event when `foo` changes, add that event kind to `useEvents.ts` to invalidate `["foo"]`. Never copy query results into a standalone `ref`.

**Mutate brain state:** `useMutation` calling `api.post/put/del`. Destructive/admin op? Wrap the mutation fn in `withElevation(...)`. On failure of an optimistic mutation, `pushErrorToast(...)` and roll back. Touches the current user's role/mode? `refreshCurrentUser()` in `onSettled`.

**Add a wire type:** change the Go DTO, regenerate (`npm run gen:api`), add the alias export in `api.ts`.
