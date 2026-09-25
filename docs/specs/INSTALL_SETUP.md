# Install setup: the install page, provider accounts, and role-mapped settings

> **Status: draft, under discussion.** This is a working plan, not a locked spec. It records what we have decided so far and what is still open. When a part locks, its shape moves into the spec it belongs to (`DASHBOARD.md`, `APP_MANIFEST.md`, `SERVICE_PROVISIONING.md`, `BRAIN_UI_PROTOCOL.md`, `SETTINGS.md`), with a `DECISIONS.md` entry where a past decision flips. Items still open at that point move to `NEXT.md`.

## Goal

Installing an app should feel like picking options, not like filling in environment variables. Today the install is a modal (`web-ui/src/components/InstallDialog.vue`) that shows the app's raw `config:` fields, one text box per env var. For an AI app that means seven optional boxes (`ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENCLAW_CUSTOM_BASE_URL`, and so on) and no hint of which ones matter.

The new flow:

1. The user clicks Install. They go to a **setup page**, not a modal.
2. The setup page shows **provider logos**, for example AI providers and email providers. The user picks one and adds a key, or picks an account they already added. Moose fills in every setting the app needs from that choice.
3. The user clicks Install on the setup page. They go to a **progress page** that shows the steps: downloading, installing, starting.

Two ideas carry this:

- **Roles.** A manifest field says what it *means* (an AI key, a base URL, a model), not only which env var it sets. This is the mapping between a simple UI and the app's own environment.
- **Provider data.** For each provider, moose knows everything needed to fill the app's fields: base URLs and the model list.

**Out of scope: hidden keys.** Giving an app a fake key and routing its requests through an on-box proxy that adds the real key is deferred. It lives in `NEXT.md` # On-box credential broker. Nothing in this plan blocks it: the role vocabulary already has a `base_url` attribute, which is what the proxy needs, and the brain already fills the app's fields itself, which is where the proxy would plug in.

## Where we are today

- **The install flow.** The Install button (`web-ui/src/views/AppDetailView.vue`) opens `InstallDialog.vue`. State lives in `web-ui/src/useInstall.ts`: it fetches `GET /api/v1/catalog/:id/install-plan`, sends `POST /api/v1/apps`, and polls the job. The modal closes and progress shows only as a label on the Install button (`INSTALL_PHASES` in `AppDetailView.vue`). Nothing about the install is in the URL, so a page reload loses it.
- **Setup fields** (`APP_MANIFEST.md` # D4, `internal/manifest/manifest.go` `ConfigField`). Each field is one env var (`app_env`). Its value is set directly under that name in the compose override (`DECISIONS.md` 2026-06-26). The brain validates the values (`internal/api/appconfig.go`). Secret values are never returned by any read endpoint.
- **Email accounts** (`SERVICE_PROVISIONING.md` # BYO outgoing mail). An admin adds an account once, in Settings. They pick a preset and type only the key. Host, port and encryption come from `internal/mailpreset` (hardcoded in Go, `DECISIONS.md` 2026-08-27). At install the user picks an account. The brain writes `MOOSE_MAIL_*` into the app's `.env`, and the app's compose maps those to its own names. Adding an account is admin-only. Accounts belong to the box, not to a user: the `mail_providers` table has no owner column, and `label` is unique across the box. The install modal can only link to Settings.
- **What catalog apps declare.** In `onmoose/store`, 18 manifests use `config:` and 17 use `mail:`. AI keys are the biggest group: `OPENAI_API_KEY` (5 apps), `ANTHROPIC_API_KEY` (4), `OPENROUTER_API_KEY` (3), `GEMINI_API_KEY`, `GROQ_API_KEY`, and "custom OpenAI-compatible" triples such as `OPENCLAW_CUSTOM_BASE_URL` / `_MODEL` / `_API_KEY`. Other repeating groups: Google OAuth client id and secret (3 apps), Resend keys, S3-style object storage. openclaw and hermes-agent need "any one" provider, which the schema cannot say today.

## Decisions so far

| Date | Decision |
| --- | --- |
| 2026-09-25 | Install moves from a modal to its own page, followed by a progress page. |
| 2026-09-25 | The mapping from UI to env vars is **generic**: a role vocabulary on manifest fields, not something special for AI. AI is the first user of it. |
| 2026-09-25 | Store as much as possible per provider, including many models per provider. An app may take only a key, a key and a model, or a key, a model and a URL. The design must allow each combination. |
| 2026-09-25 | Models come in types (chat, embedding, and so on), and one app can be given several models at once so the user can switch between them inside the app. |
| 2026-09-25 | Every user can add accounts, not only admins. An account can be used only by the user who added it. Sharing an account with other users on the box comes later. Hosted boxes, the current focus, have one user. |
| 2026-09-25 | The UI shows about five featured providers, plus search over all of them. |
| 2026-09-25 | Provider and model data is not in the OS release and not fetched from each provider's API. It is published from `onmoose/store` and served by the catalog service, so it can change without an OS update. |
| 2026-09-25 | Hidden keys (fake key plus on-box proxy) are **deferred**. This plan covers the UI and the manifest change only. |
| 2026-09-25 | Work across repos is fine. In practice this plan touches `os` and `store`, and not `cloud`. |
| 2026-09-25 | Requirement rules are "at least one of" groups only, for now. |
| 2026-09-25 | The role vocabulary and the provider data shape stay open to change. A change that older boxes cannot read ships as a new catalog snapshot version served next to the old one, not as a bump in place. |
| 2026-09-25 | Step 1 (the setup page and the progress page) is built first, UI only, on its own branch. The layout uses Tailwind Plus patterns: a left-aligned description list for the page, horizontal link cards for the options, and a divider with a "More" button under each option list. |
| 2026-09-25 | The generic `openai_compatible` slot holds one provider. A model list that spans several providers is not supported for now. |
| 2026-09-25 | Models for an installed app are edited on the app's settings screen. A redesign of that screen comes later. |

## Design

### 1. Roles: the mapping between the UI and the app's env vars

A config field gets one new optional key, `role`. The value still lands under `app_env`, exactly as today, so D4 (`DECISIONS.md` 2026-06-26) holds and no compose file has to change.

```yaml
config:
  - app_env: ANTHROPIC_API_KEY
    role: ai.anthropic.api_key
  - app_env: OPENCLAW_CUSTOM_BASE_URL
    role: ai.openai_compatible.base_url
  - app_env: OPENCLAW_CUSTOM_API_KEY
    role: ai.openai_compatible.api_key
  - app_env: OPENCLAW_CUSTOM_MODEL
    role: ai.openai_compatible.model.chat
```

The vocabulary is `<kind>.<protocol>.<attribute>`:

- **kind** is the family: `ai` now. Later `oauth` (Google, GitHub client id and secret), `object_store` (S3-style), and maybe `mail`. It is generic, as decided.
- **protocol** is the API the app speaks: a native one (`anthropic`, `openai`, `gemini`, `openrouter`, `groq`) or the generic `openai_compatible`.
- **attribute** is the piece: `api_key`, `base_url`, `model.<type>`, `models.<type>` (section 2). The app declares only the attributes it reads. That gives the "key only", "key and model", and "key, model and URL" cases for free.

Fields that share `kind.protocol` form a **slot**. The setup page draws a slot as provider tiles, not as text boxes. When the user fills a slot, moose fills every attribute the app declared for it at once. A field without a `role` stays a plain form field, as today, so all current manifests keep working. Title and description stay required, because the post-install screen and the fallback still need them.

**Filling a slot.** When the user picks provider P for an app:

1. If the app has a native slot for P (for example `ai.anthropic.*` and P is Anthropic), fill that slot.
2. Else, if the app has an `ai.openai_compatible` slot and P offers an OpenAI-compatible endpoint, fill that slot with P's base URL, the key, and the chosen model or models.
3. Else P is not offered for this app, and its tile is hidden.

Most providers offer an OpenAI-compatible endpoint, including Anthropic and Gemini, so an app with only the generic slot can still use almost all of them. An app can fill several slots, for example Anthropic and OpenAI together in openclaw. The generic `openai_compatible` slot holds one provider, because it is one set of env vars.

### 2. Models

Models have **types**, and an app can take **one model or several** of a type.

- **Types.** `chat`, `embedding`, `image`, `speech_to_text`, `text_to_speech`, `rerank`. The provider data tags each model with its types and with capability flags (vision, tools, reasoning). The type in the role decides which models the picker offers.
- **One model.** `model.<type>`, for example `ai.openai_compatible.model.chat` or `ai.openai.model.embedding`. The UI shows a single picker.
- **Several models.** `models.<type>`, for apps that take a list in one env var and let the user switch inside the app (for example `OPENAI_MODELS=gpt-4o,gpt-4o-mini`). The UI shows a multi-select. The field declares how the list is joined with `separator` (default `,`).
- **No model field at all.** Many apps ask the provider for its model list themselves once they have a key and a base URL. Those apps declare no model attribute, and the user picks a model inside the app.
- **A model that is not in our list.** Providers ship new models often. The picker also accepts a typed model id, so our list never blocks the user.

### 3. Requirement rules

Two things cannot be said with a single field's `required` today:

- "At least one AI provider is required" (openclaw, hermes-agent).
- "Key A is required only if key B is not set", which is the same rule seen from one field.

Proposal: a `requires` list next to `config:`. Each entry is a `one_of` group, satisfied when at least one member is filled. A member is a kind (`ai`), a slot (`ai.anthropic`), or a plain field by its `app_env`.

```yaml
requires:
  - one_of: [ai]                                  # any AI provider
  - one_of: [GOOGLE_API_KEY, GOOGLE_CREDENTIALS]  # plain fields, no roles
```

The setup page keeps Install disabled until every group is satisfied, and says which group is missing. The brain checks the same rules on `POST /api/v1/apps` and answers 422, the same as for `required` today.

The third known case, "model is required when a custom base URL is set", goes away for role-tagged slots, because moose fills the whole slot at once. For plain fields it stays unsupported until an app needs it.

"At least one of" is the only rule for now. The holes we found, and how the design closes them:

- **The rules must hold after install too.** Editing the app's settings is checked against the same `requires` groups, not only the install. Deleting an account that an app depends on is the harder case. See Open questions 1.
- **A slot can need a model type that a provider lacks.** If a slot has `model.embedding`, a provider with no embedding models (Anthropic today) must not show as a tile for that slot. Otherwise the user picks it and the app fails. Tiles are filtered by the model types the slot declares.
- **`one_of: [ai]` is too coarse when an app needs two things.** An app that needs a chat provider *and* an embedding provider must list slots in two groups, not write `one_of: [ai]`. The authoring guide says so, and the lint warns when a kind-level group covers slots with different model types.
- **`required: true` inside a `one_of` group contradicts the group.** The lint rejects a field that is both `required` and a member of a group.

### 4. Provider data

Per provider, moose stores:

- id, display name, logo URL, a link to where the user gets a key, a short help text, and an optional key format hint (for example the `sk-ant-` prefix)
- which native protocol it speaks, and its OpenAI-compatible base URL if it has one
- whether it needs a key at all (a local Ollama or vLLM server needs only a URL, which the user types)
- a **model list**: id, display name, types, capability flags, and a "recommended default" per type
- a featured rank, so the UI can show the top five

The data is authored in `onmoose/store` and published by the catalog service. The brain fetches it at runtime and holds it in memory, the same way it holds the catalog today (`internal/catalog`). The brain needs it, not only the UI, because the brain fills the slot (section 5). Values are written into the app when it is installed, so a later outage of the catalog service does not affect a running app.

**Rollout to boxes on older versions.** The catalog wire format ignores unknown keys, but a box refuses the whole snapshot if `schema_version` is one it does not know (`internal/catalog/wire.go`). So the provider data is either added to the current snapshot without bumping `schema_version`, or the catalog service serves a **new snapshot version next to the old one**, and each box asks for the version it can read. Bumping the version in place is the one thing to avoid: every older box would lose its whole store.

**The shape is not frozen yet.** Roles, `requires` and the provider data shape may still change while we build this. Serving versions side by side is what keeps that cheap: a change the old shape cannot absorb goes into a new version, and older boxes keep reading the old one. The same goes for manifests: older boxes parse them leniently, so `role`, `separator` and `requires` are ignored there and the fields show as plain form fields, as today. Store manifests can be tagged before the whole fleet is updated.

Email presets stay in `internal/mailpreset` for now. They change "roughly never", which was the reason for `DECISIONS.md` 2026-08-27 D1. Moving them to the same source later is possible, but it is not part of this plan.

### 5. Provider accounts

A **provider account** is a saved key for one provider: provider id, a label, the key, and an optional base URL (for a self-hosted server).

- Any user can add one, from the setup page or from Settings. This lifts the admin-only rule. Email accounts get the same change.
- An account belongs to the user who added it, and only that user can use it. Sharing comes later. A household app installed by an admin uses that admin's accounts.
- Email accounts change the same way. `mail_providers` gets an owner column. Labels become unique per owner, not per box. Existing rows go to the first admin. On a hosted box that is the only user, so nothing visible changes.
- The setup page can add an account inline. The user never has to leave the install.
- `POST /api/v1/apps` carries a binding per slot: which account, and which model or models for each model attribute. The UI never sends or sees the key again.
- The **brain resolves** each binding into the role-tagged `app_env` values when it writes the compose override. It must be the brain, because secrets are never returned to the UI.
- When an account's key changes, the brain re-writes every app bound to it, the way `RebindMail` does for email.
- After install, the app's settings screen shows the account and model pickers for a role-tagged slot, not the raw fields. This is also where the user adds or removes models for an app that takes a list. A change restarts the app. How that screen is laid out overall is left for later.

### 6. The setup page and the progress page

- **Routes.** `/store/:id/install` for setup and `/store/:id/install/:jobId` for progress. `/store/custom` (`CustomInstallView.vue`) is the precedent for a full-page install form. The job id is in the URL, so a reload picks the job up again through `GET /api/v1/jobs/:id`.
- **Setup page.** It keeps everything the modal has today: permissions, folder sources, the storage estimate, the duplicate-install warning (409), and the inline field errors (422). It adds a section per slot (provider tiles: five featured, then search; then the model pickers) and an email section with inline "add account". `web-ui/src/mailProviderForm.ts` is already shared by two views, so the inline form becomes its third user. Plain role-less fields show below, as today.
- **Progress page.** The brain already reports a live `step` on the job. The page shows the phases as a checklist, not a button label, and ends with an Open button.
- `InstallDialog.vue` is removed once the page replaces it.

## Work by repo

- **os:** `role`, `separator` and `requires` in the manifest schema and its validation, plus the `moose manifest` lint; fetching and caching provider data; the provider account store and API with owners; the owner column on email accounts; slot resolution at install and rebind; the two UI pages; lifting the admin-only rule; the spec updates (`DASHBOARD.md`, `APP_MANIFEST.md` # D4, `BRAIN_UI_PROTOCOL.md`, `SERVICE_PROVISIONING.md`, `SETTINGS.md`, `APP_STORE.md`, `DECISIONS.md`).
- **store:** the provider and model data and its publishing through the catalog service; `role` and `requires` on the 18 manifests with `config:`; the authoring guide.
- **cloud:** nothing.

## Suggested order

1. **Setup page and progress page**, with today's fields. No schema change. It ships on its own and every later step builds on it. Until roles exist, the AI providers section uses a temporary provider list in the UI and a temporary lookup from today's env var names (`ANTHROPIC_API_KEY` and so on) to providers. Both sit in one module that step 4 replaces.
2. **Inline email accounts** on the setup page, with account owners and the lifted admin rule. It depends only on step 1.
3. **Provider data** in `store` and the catalog service, and the brain fetching it.
4. **Roles, models, requirement rules, AI provider accounts, and slot filling.** Then tag the store manifests.

## Open questions

1. **Deleting an account that apps depend on.** Email today lets the delete through and the bound apps become unbound. For an AI app that can mean the app stops working. Proposal: the delete screen lists the apps that use the account and warns. The delete goes through, the app's settings screen shows "needs a provider", and the same `requires` check blocks saving until one is picked.
