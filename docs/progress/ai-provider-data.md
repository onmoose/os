# AI provider data from the catalog

- **Status:** done, tried in a browser without logos (see Known gaps)
- **Date:** 2026-09-25
- **Specs touched:** docs/specs/INSTALL_SETUP.md, docs/specs/BRAIN_UI_PROTOCOL.md, docs/specs/APP_STORE.md

## What was done

Step 3 of the install setup plan (`INSTALL_SETUP.md` # 4 and # Suggested order), the `os` side only. It follows [install-setup-page.md](install-setup-page.md), which built the setup page with a hand-written AI provider list in the UI, and [email-accounts-per-user.md](email-accounts-per-user.md), which corrected that list against the providers' docs. The list now comes from the catalog, so it can change without an OS update. The env-name lookup in `aiProviders.ts` stays until step 4.

**Brain, reading** (`internal/catalog/wire.go`, `internal/catalog/aiproviders.go`). The snapshot has a new top-level `ai_providers` field, in the shape agreed in `INSTALL_SETUP.md` # 4. `schema_version` stays 2. The field is held as raw JSON on `catalogFile` and decoded leniently in `newSnapshot`, so it is read once per load and kept in memory with the rest of the snapshot:

- The value is decoded entry by entry. A value that is not a list is an empty list, and an entry that does not decode is dropped. **Nothing in `ai_providers` can make the box refuse the snapshot**, which a typed field could have done: a wrong JSON type anywhere in it would have failed the whole unmarshal.
- A provider with no id or name is dropped; a repeated provider id keeps the first.
- A model with no id is dropped, and a repeated model id keeps the first. An unknown type or flag is dropped. A model left with no known type is dropped. A model with no name shows its id.
- A `defaults` entry is dropped when its type is unknown, or its model is missing or does not have that type. The last rule goes a little past the brief (which named "missing model" and "unknown type"), because suggesting an embedding model as the chat default would put a wrong model first in the picker.
- The drops are logged once per load: one `slog.Info` line with the count and the first ten reasons.

The model types (`chat`, `embedding`, `image`, `speech_to_text`, `text_to_speech`, `rerank`) and flags (`vision`, `tools`, `reasoning`) are closed lists in `aiproviders.go`. The source interface gained `aiProviders()` and `aiProviderLogoPath()`; the disk source (tests only) has no data and answers an empty list and `ErrNotFound`. The facade exposes them as `Catalog.AIProviders()` and `Catalog.AIProviderLogoPath(id, dark)`.

**Brain, serving** (`internal/api/aiproviders.go`). `GET /api/v1/ai-providers` answers `{ "providers": [...] }` in authored order to any signed-in user, like the catalog browse routes. No data is 200 with an empty list, never `null`. Logo URLs in the answer are box routes:

- `GET /api/v1/ai-providers/{id}/logo` and `GET /api/v1/ai-providers/{id}/logo-dark`. Two fixed routes rather than a `?variant=` query: each variant is its own URL, so the browser caches them apart, and the handler has nothing to parse.
- Both go through `cachedAsset`, the same proxy and 24-hour cache as an app icon, and through `serveAsset` for the error mapping. The cache key is `ai-providers/<id>`, so a logo never lands in an app's cache directory.
- A logo URL is left out of the answer when the provider has none, so the UI never asks for a 404.

The OpenAPI spec and `web-ui/src/generated/openapi.ts` are regenerated. `AIModel.types` and `flags` carry their closed lists as enums, so the TS types are literal unions.

**Seed seams.** `MOOSE_CATALOG_FILE` needed no change: it goes through `parseSnapshot` and `newSnapshot` like a fetched payload. `BuildSnapshot` takes a provider list (`SnapshotAIProvider`, an alias of the wire type), and `dev/mkcatalog` gained `-ai-providers <file.json>`, a JSON list in the published shape. `make seed-catalog` and `make dev-app` pass it through as `AIPROVIDERS=<path>`. That is how the setup page can be tried before the catalog service serves the field.

**Fixture and shape guard.** `internal/catalog/testdata/snapshot.json` gained two invented providers: `alpha-ai` with every optional field, `beta-ai` with few. `TestNoUnmodeledFields` now also checks the keys of each provider and each model.

**UI** (`web-ui/src/aiProviders.ts`, `components/install/AIProviderSection.vue`, `views/InstallSetupView.vue`). The hardcoded `PROVIDERS` list and its Lucide icons are gone. The setup page fetches `GET /api/v1/ai-providers` with TanStack Query (key `["ai-providers"]`, 5 minutes stale, one retry, no refetch on focus) and passes the list to the AI section.

- **Slots match by protocol.** A native slot now carries the protocol its env names speak (`NATIVE_ENV` maps `ANTHROPIC_API_KEY` to the `anthropic` protocol, and so on), and a provider fills it when its `native_protocol` matches, whatever its id. A provider without `native_protocol` fills only a compatible slot, and only if it has `openai_base_url`.
- **"Other (OpenAI-compatible)"** stays a UI tile (`OTHER` in `aiProviders.ts`), always last. It has no URL and no models; the user types the address, and its key is optional. It is the only tile with an optional key, since `needs_key` is not in the data.
- **A native slot no provider can fill** goes back to the Settings row as plain fields (`fillableSlots`). Without this, an app with `GEMINI_API_KEY` on a catalog without a gemini provider would have a field the user could not reach.
- **Model picker.** It offers the provider's chat models, with `defaults.chat` first and marked "Suggested". A required model starts on that suggestion. The typed field for any model id is still there.
- **Selected tile.** A provider tile shows as selected while its editor is open, not only after it is saved, so the user sees which tile they are filling in.
- **Logos.** A tile draws `logo_url`, or a generic icon when there is none or the image fails to load (`Server` for Other, `Bot` for the rest). `logo_dark_url` is used when the dashboard is in dark mode, read as a `dark` class on `<html>`.
- **Key help.** Where the old editor showed only the key link, it now shows `help` followed by the "Get a key" link to `key_url`. A key that does not start with `key_prefix` gets a warning line under the field. Save is never blocked by it.
- **Fallback.** With an empty list, from either an empty answer or a failed fetch, no AI section is drawn and every AI field is a plain field in the Settings row. The page shows "Loading…" while the list is loading, but only for an app that has AI fields, so the fields do not jump from Settings to the AI row while the user types.

The header of `aiProviders.ts` now says that only the env-name lookup is temporary.

## How it maps to the specs

Realizes `INSTALL_SETUP.md` # 4 on the box side and step 3 of # Suggested order. That section now describes the agreed shape, how the box reads it, and how it serves it. Its Decisions table has the 2026-09-25 rows for the shape, file order as display order, "Other" being UI-owned, `needs_key` dropped, `logo_dark_url`, lenient reading, logos proxied through the box, and the snapshot version covering `ai_providers`. `BRAIN_UI_PROTOCOL.md` lists the three routes and has a new # AI provider data section. `APP_STORE.md` has a new # AI provider data section with the box-facing half of the contract, as `docs/dev/contributing.md` # Changing the published catalog shape asks. `docs/architecture.md` has the new catalog projection and the logos in the asset cache. `docs/dev/web-ui.md` describes the new data source and the fallback.

## Known gaps & deviations

- **Tried in a browser without logos.** `make check` and `vue-tsc --noEmit` pass. The setup page was tried under `make dev-app APPS="openclaw firecrawl" AIPROVIDERS=<file>` with a seeded providers file that has no logos: tile order, native and custom slots, the model picker, the key warning, and the selected tile. Not tried: real logos, a failed logo, and dark logos (the dashboard has no dark theme yet).
- **The store side is not built.** This is the `os` half. Until the store publishes `ai_providers` (the store PR for step 3, which also adds the field to what the snapshot version covers), a real box has an empty list and every AI field is a plain field. That is a step back from step 1 for now: the hardcoded tiles are gone before the published ones arrive.
- **The fixture was written by hand.** Since #491 the publisher writes `internal/catalog/testdata/snapshot.json` from its own wire types. The publisher does not have `ai_providers` yet, so the two synthetic providers were added by hand, in the agreed shape. The store PR should regenerate the fixture from the publisher; if its shape differs, `TestNoUnmodeledFields` will say so.
- **Dark logos are wired but never shown.** The dashboard has a light theme only (`style.css`), so there is no dark mode to switch on. The component reads a `dark` class on `<html>`, which is where a Tailwind dark theme usually puts it; if the dark-mode trigger lands another way, that one line changes. It does not follow the OS setting on purpose: on the light-only page that would draw a light logo on a light card.
- **The model picker shows chat models only.** Today the lookup knows only chat model fields (`OPENAI_MODEL`, `*_CUSTOM_MODEL`). Other types wait for roles (`model.<type>`, step 4).
- **The key warning checks `key_prefix` only.** No length or format check.
- **The snapshot's `version` token from `BuildSnapshot` does not cover providers.** It hashes the apps only (as it already did not cover home and categories). It matters only for the seed seam, which is read once at boot.

## What's next

- The store side of step 3: author the provider data, publish it as `ai_providers`, cover it with the snapshot version, and regenerate this repo's fixture from the publisher.
- Try the setup page with real provider data: openclaw (native slots and the custom triple), cap, and a catalog with no data (plain fields).
- Plan step 4: manifest roles, models by type, requirement rules, AI provider accounts, and slot filling in the brain. That replaces the env-name lookup in `aiProviders.ts`.
