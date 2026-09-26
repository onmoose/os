# AI slot filling at install

- **Status:** done
- **Date:** 2026-09-26
- **Specs touched:** docs/specs/INSTALL_SETUP.md, docs/specs/BRAIN_UI_PROTOCOL.md, docs/specs/APP_MANIFEST.md, docs/specs/SERVICE_PROVISIONING.md, docs/specs/DASHBOARD.md

## What was done

The third piece of step 4 of the install setup plan (`INSTALL_SETUP.md` # 1, # 5 and # 6), after [ai-accounts.md](ai-accounts.md), which stored AI provider accounts. This piece fills an app's AI slots from those accounts at install. It follows the five 2026-09-26 decisions rows that start "Piece 3:", "The brain resolves a binding", "A slot is filled either", "The brain stores each binding" and "The setup page reads roles".

**Wire.** `POST /api/v1/apps` takes `config.ai_bindings`: a list of `{ slot, account_id, models }`. `slot` is `kind.protocol` from the manifest. `models` maps a model setting the slot declares (`model.chat`, `models.embedding`) to model ids. The install plan is unchanged: the UI works out the tiles from the roles it already sends and from `GET /api/v1/ai-providers`.

**Resolution** (`internal/api/aibindings.go`). `resolveAIBindings` takes the manifest, an account lookup and the provider list, so it is tested without a server. For each binding:

- The slot must be one the manifest declares with fillable roles, and appear once.
- The account must be the caller's (`ownAIAccount`). A missing id and another user's id get the same 422.
- The provider must fit. A native slot needs a listed provider whose `native_protocol` is the slot's protocol. The compatible slot needs an `openai_compatible` account or a listed provider with an `openai_base_url`. A provider that has left the provider data can fill only the compatible slot, and only when the account has its own base URL.
- `api_key` gets the account's key, or nothing for a keyless account. A compatible `base_url` gets the account's base URL, else the provider's `openai_base_url`. A native `base_url` gets the account's base URL or nothing.
- Each declared model setting gets the chosen ids, else the provider's default for its type (a listed provider only), else a 422. `model.<type>` takes exactly one id and `models.<type>` one or more. Ids are trimmed, must not be empty, hold no control characters and no duplicates, and in a list no id may contain the field's separator. The list is joined with the separator.

`resolveInstallWithAI` then refuses a non-empty typed value for any field of a bound slot, merges the resolved values with the typed ones, and runs the result through the existing `resolveInstallConfig`. So `required`, enum and `requires` checks see the filled slot, and the resolved values become normal config values with the field's secret flag. The handler reads the provider data only when the request has bindings. Every refusal audits `app.install` with `success=false`, like the other install checks. The full list of 422 messages is in `BRAIN_UI_PROTOCOL.md` # POST /api/v1/apps.

**Store** (`internal/store/aiaccounts.go`). A new table, `instance_ai_bindings`: `instance_id` (FK to `instances`, cascade), `slot`, `account_id` (FK to `ai_accounts`, cascade), `models` (JSON, `'{}'` when none), primary key `(instance_id, slot)`. Additive: created with `IF NOT EXISTS` after `ai_accounts`. Methods: `SetInstanceAIBindings` (replace an instance's set in one transaction), `PutAIBinding` (one row, for the user-delete rollback), `ListInstanceAIBindings`, `ListAIBindingsForAccount` (for piece 4). The stored model ids are the ones the app got, defaults included.

**Lifecycle** (`internal/lifecycle/lifecycle.go`). `Install` takes the bindings next to the config values and writes them in a new step 5e, `binding_ai_accounts`, right after the mail binding, inside the existing rollback. The brain commits first, and the FK catches an account deleted between the check and the write. Every existing `Install` call site gained a `nil`. The progress page maps the new step to "Setting up".

**User delete** (`internal/api/users.go`). `deleteUser` reads the bindings of the user's AI accounts before the delete, and puts them back with the accounts if the host step fails.

**UI.** `web-ui/src/aiProviders.ts` is no longer temporary: the env-name lookup (`NATIVE_ENV`, `CUSTOM_ENV`, the old `aiSlots`) is gone. Slots come from each field's `role` (kind `ai`, grouped by `kind.protocol`). A tile goes to the native slot for its `native_protocol` first, else to the compatible slot when it has an `openai_base_url`; Other always fits the compatible slot. A listed provider's tile is hidden for a slot when it has no model of a type the slot declares. A native slot that no provider fits stays plain fields. `AIProviderSection.vue` now lists the user's accounts for the picked tile (`GET /api/v1/ai-accounts`; Other lists `openai_compatible` accounts), a sole one preselected, with an inline add (name, key with the key link and the soft `key_prefix` warning, and the server address for Other) that posts `/api/v1/ai-accounts` without elevation and picks the new account. Then one model picker per declared setting: single for `model.<type>`, multi for `models.<type>`, the provider's models of that type with its default first and chosen, plus a typed id. The setup page sends `config.ai_bindings` and leaves a bound slot's fields out of `config.fields`. Install now also waits for the plan's `requires` groups, counting a bound slot as filled, and the "Still needed" line names what is missing ("an AI provider"). With no provider data every field stays a plain field, as before.

The OpenAPI spec and `web-ui/src/generated/openapi.ts` are regenerated. `api.ts` gains `AIAccount`, `AIAccountBody`, `AIBinding` and `RequiresGroup`.

**Tests.** `internal/api/aibindings_test.go`: resolution for native with a default, native with an account base URL and a typed model, compatible through a listed provider with lists joined, compatible through a native provider with defaults for two types, compatible with an account base URL override, Other with and without a key, a provider gone from the data with and without an account base URL, two slots at once; 21 table cases for the 422s; a separator inside a single-model id; `requires` met by a binding and typed role values still accepted; a lookup error as a 500; over HTTP, another user's account and a missing one both 422 `no such AI account` and audit a failure; a user-delete rollback restores the bindings. `internal/store/aiaccounts_test.go`: round trip, replace, list by account, FK on a missing account, cascade on account delete and on instance delete. `internal/lifecycle/lifecycle_ai_test.go`: an install stores the bindings and the config values it is given reach the override, uninstall removes the bindings, and a missing account rolls the install back. The web UI has no unit test runner, so none were added there.

## How it maps to the specs

Realizes the five piece 3 rows of the `INSTALL_SETUP.md` decisions table and the "Filling a slot" rule of # 1. As-built notes are added to `INSTALL_SETUP.md` # 1, # 5 and # 6 (the decisions table is unchanged). `BRAIN_UI_PROTOCOL.md` # POST /api/v1/apps documents `config.ai_bindings` and its 422s, and # AI provider accounts says what a delete does to bindings. `APP_MANIFEST.md` # D4 # Roles and requires says briefly how a slot is filled. `SERVICE_PROVISIONING.md` # AI provider accounts has a Bindings paragraph. `DASHBOARD.md` # Install authorization describes the new AI row and the `requires` gate. `docs/dev/web-ui.md` no longer calls `aiProviders.ts` temporary. `docs/architecture.md` has nothing that became wrong.

## Known gaps & deviations

- **Not tried in a browser.** Only `vue-tsc` checks the UI. The account pick, the inline add, the model pickers and the `requires` line have not been clicked through.
- **Not tried against a running brain.** `make check` passes. The API tests stop at the synchronous checks before the job; the lifecycle tests cover the stored rows and the override with fake Docker.
- **Post-install pickers and rebind are piece 4.** The app's settings screen still shows the raw fields. A key change on an account does not rewrite the apps bound to it.
- **Deleting an account leaves the app's stored values** (key, URL, models) in place until the app's settings are next written. Only the binding row goes.
- **The UI's `requires` gate is a close copy of the brain's rule, not the same code.** It counts every non-model field of a bound slot as filled. The brain counts only the values a binding really fills, so a slot that declares only model settings, or a keyless Other account on a slot with a key field and no base URL field, passes the UI and gets the brain's 422. No catalog app has either shape today.
- **A slot fits by protocol and model types only.** A listed provider without an `openai_base_url` never fits the compatible slot, even when its account has a base URL of its own, because the decision row names the provider's endpoint. Only a provider that has left the data falls back to the account's base URL.
- **Choices the brief left open.** The stored model ids include the defaults the brain applied, so piece 4 can show exactly what the app got. An empty typed value on a bound slot's field is treated like no value, not a clash. A present `models` key with no ids is a 422, not "use the default". No length limit on a model id beyond the request size.

## What's next

- Piece 4: the Settings screen for AI accounts, the account and model pickers on an installed app's settings screen, rewriting bound apps when a key changes, and what deleting an account in use does (`INSTALL_SETUP.md` # Open questions 1).
- Tag the store manifests with `role` and `requires` (`onmoose/store`).
