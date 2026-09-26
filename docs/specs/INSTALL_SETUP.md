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
| 2026-09-25 | The provider data shape is the one in # 4: a top-level `ai_providers` list on the catalog snapshot, one entry per provider with id, name, logos, key link, help, key prefix, native protocol, OpenAI-compatible base URL, a checked date, per-type defaults, and the models with their types and flags. |
| 2026-09-25 | Display order is file order. The setup page shows the first five and the rest behind "More". There is no separate featured rank. |
| 2026-09-25 | "Other (OpenAI-compatible)" is a UI tile, not provider data. It has no URL and no models; the user types the address, and the key is optional. |
| 2026-09-25 | `needs_key` is dropped until a keyless provider is in the data. Today only "Other" may take no key, and the UI owns it. |
| 2026-09-25 | A provider may carry a second logo for dark backgrounds, `logo_dark_url`. |
| 2026-09-25 | The box reads the data leniently: an unknown model type or flag is dropped, not refused, so the store can add one before the fleet understands it. Nothing in `ai_providers` can make a box refuse the snapshot. |
| 2026-09-25 | Logos are proxied through the box, like app icons. The UI never loads them from the asset origin. |
| 2026-09-25 | The snapshot version covers `ai_providers`. `onmoose/store#133` made it cover the home page and the categories; the store change for step 3 adds `ai_providers` to it. |
| 2026-09-26 | Role vocabulary: `<kind>.<protocol>.<attribute>`, lowercase segments. Kind is a closed list in Go, `ai` only for now: a kind is added when moose can fill it (email and Google OAuth are the likely next ones). Protocol is open: `openai_compatible` is reserved, and any other value is a native protocol matched against a provider's `native_protocol`. Attribute is closed: `api_key`, `base_url`, `model.<type>`, `models.<type>`, with the model types of # 4. |
| 2026-09-26 | `separator` is valid only on a `models.<type>` field. Default `,`, 1 to 4 printable characters, no newline, `=` or quote. |
| 2026-09-26 | A `requires` member is a kind (`ai`), a slot (`ai.anthropic`) or a plain field by its `app_env`, told apart by case. A group is satisfied when at least one matched field has a value after the brain resolves the install. Model fields do not count. The check needs no knowledge of the vocabulary. |
| 2026-09-26 | A native `base_url` is filled only from the account's own base URL. Otherwise it is left blank and the app uses its built-in default. |
| 2026-09-26 | `manifest.Parse` stays lenient: every new rule is in the `moose manifest` lint, never in `Parse`. The box shows a field it cannot fill as a plain field, drops a `requires` member that matches no field, and drops a group left empty. It never refuses a manifest over `role`, `separator` or `requires`. |
| 2026-09-26 | `manifest_version` stays 1 and the snapshot `schema_version` stays 2. The new keys are optional, and older boxes ignore them. |
| 2026-09-26 | openclaw and hermes-agent get `requires: one_of: [ai]`, so their install is gated on a provider. Today they install with none. |
| 2026-09-26 | Editing an installed app's settings may not make a satisfied `requires` group unsatisfied. A group that already fails (an app installed before `requires`, or after its account was deleted) does not block unrelated edits. |
| 2026-09-26 | `moose manifest lint` takes an optional `--ai-providers <path>` and then warns about a native protocol that no provider offers. |
| 2026-09-26 | Known limits, deferred to a later version: a field that says which provider to use (openmuse `MODEL=provider/model`) stays a plain field, and an app gets one slot per protocol, so separate OpenAI-compatible endpoints per job (upstream open-webui) cannot be expressed. |

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

**As built (roles in the schema).** `role` and `separator` are on `ConfigField`, and the vocabulary is in `internal/manifest/roles.go` (`ParseRole`, `FillableRoles`, `EffectiveSeparator`). The box reads them leniently and the `moose manifest` lint strictly (`internal/manifest/lint.go`); `APP_MANIFEST.md` # D4 # Roles and requires has the schema. The install plan and the config endpoint send each fillable field's `role` and `separator`. Filling a slot from a provider account, and matching a native slot against provider data, are not built yet (step 4, later pieces).

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

**As built (requires).** `requires` is on the manifest, read leniently (`Manifest.EffectiveRequires`, `GroupFields`, `GroupSatisfied` in `internal/manifest/roles.go`). The brain checks it on `POST /api/v1/apps` and, as "no worse", on `PUT /api/v1/apps/{id}/config` (`internal/api/appconfig.go`). The install plan and the config endpoint send the groups. The lint rules and warnings above are in `internal/manifest/lint.go`.

### 4. Provider data

The catalog snapshot (`GET /catalog?env=`) carries a top-level `ai_providers` list. The list is in display order: the setup page shows the first five providers and the rest behind "More". There is no separate featured rank. Each entry:

```json
{
  "id": "anthropic",
  "name": "Anthropic",
  "logo_url": "https://assets.example/ai-providers/anthropic/logo.svg",
  "logo_dark_url": "https://assets.example/ai-providers/anthropic/logo-dark.svg",
  "key_url": "https://platform.claude.com/settings/keys",
  "help": "Make a key in the Claude Console, under Settings, API keys.",
  "key_prefix": "sk-ant-",
  "native_protocol": "anthropic",
  "openai_base_url": "https://api.anthropic.com/v1/",
  "checked": "2026-09-25",
  "defaults": { "chat": "claude-sonnet-5" },
  "models": [
    { "id": "claude-sonnet-5", "name": "Claude Sonnet 5", "types": ["chat"], "flags": ["vision", "tools", "reasoning"] }
  ]
}
```

- **Required:** `id`, `name`, and per model `id`, `name` and `types`. Everything else is optional.
- **`logo_url`, `logo_dark_url`** are absolute URLs on the asset origin, followed as given (a relative one resolves against the catalog base, like an app icon). The dark one is for dark backgrounds.
- **`key_url`** is the provider's page for making a key, and **`help`** a short text shown next to it.
- **`key_prefix`** is a hint. A key that does not start with it gets a soft warning on the setup page, never a block.
- **`native_protocol`** is the API an app's native slot speaks (`anthropic`, `openai`, `gemini`, ...). A native slot matches a provider by this field, not by the provider id. A provider without it is reachable only through an OpenAI-compatible slot.
- **`openai_base_url`** is the provider's OpenAI-compatible endpoint, if it has one.
- **`checked`** is the date the entry was last checked against the provider's own docs.
- **`defaults`** maps a model type to the model to suggest first for that type.
- **Model `types`** come from a closed list: `chat`, `embedding`, `image`, `speech_to_text`, `text_to_speech`, `rerank`. **`flags`** come from `vision`, `tools`, `reasoning`.

"Other (OpenAI-compatible)" is not in the data. It is a tile the UI owns: no URL, no models, the user types the server's address, and the key is optional. A `needs_key` field is left out until a provider that needs no key is in the data.

**How the box reads it** (`internal/catalog/aiproviders.go`). The box reads leniently, so the store can grow the data without a new snapshot version. An unknown model type or flag is dropped. A model left with no known type is dropped. A `defaults` entry that names a missing model, a model without that type, or an unknown type is dropped. A provider with no id or name is dropped, and a repeated provider id keeps the first. The box logs what it dropped once per load. Nothing in `ai_providers` can make the box refuse the snapshot: a value it cannot read at all is an empty list. A box on an older catalog, or on a snapshot without the field, has an empty list too.

**How the box serves it.** `GET /api/v1/ai-providers` returns the list in order to any signed-in user (`BRAIN_UI_PROTOCOL.md` # Pattern A). Logo URLs in the answer point at the box, `GET /api/v1/ai-providers/{id}/logo` and `/logo-dark`, which proxy and cache the image like an app icon. When the list is empty, the setup page draws no AI section and shows the AI fields as plain fields, so an install is never blocked by missing provider data.

The data is authored in `onmoose/store` and published by the catalog service. The brain fetches it at runtime and holds it in memory with the rest of the snapshot (`internal/catalog`). The brain needs it, not only the UI, because the brain fills the slot (section 5). Values are written into the app when it is installed, so a later outage of the catalog service does not affect a running app.

**Rollout to boxes on older versions.** The catalog wire format ignores unknown keys, but a box refuses the whole snapshot if `schema_version` is one it does not know (`internal/catalog/wire.go`). So the provider data is either added to the current snapshot without bumping `schema_version`, or the catalog service serves a **new snapshot version next to the old one**, and each box asks for the version it can read. Bumping the version in place is the one thing to avoid: every older box would lose its whole store. **As built, `ai_providers` is added to the current snapshot, and `schema_version` stays 2.** An older box ignores the key, and a newer box reads it leniently, so neither refuses the snapshot over it.

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
