# AI provider accounts

- **Status:** done
- **Date:** 2026-09-26
- **Specs touched:** docs/specs/SERVICE_PROVISIONING.md, docs/specs/BRAIN_UI_PROTOCOL.md, docs/specs/LOGGING.md, docs/specs/INSTALL_SETUP.md

## What was done

The second piece of step 4 of the install setup plan (`INSTALL_SETUP.md` # 5 and # Suggested order), after [manifest-roles.md](manifest-roles.md), which added `role` and `requires` to the manifest schema. This piece stores AI provider accounts on the box and serves them over the API. It follows the 2026-09-26 rows of the `INSTALL_SETUP.md` decisions table, and copies the email account pattern from [email-accounts-per-user.md](email-accounts-per-user.md). It is brain only. Nothing reads or binds an account yet.

**Store** (`internal/store/aiaccounts.go`). A new table, `ai_accounts`: `id`, `owner_user_id` (FK to `users`, `ON DELETE CASCADE`), `provider_id`, `label`, `api_key`, `base_url` (`''` when unset), `created_at`, `updated_at`, and `UNIQUE (owner_user_id, label)`. It is created with `IF NOT EXISTS` next to `mail_providers` in `migrate`, so an existing box just gains it. `provider_id` has no CHECK and no foreign key, because the provider list is catalog data that can change. The key is plaintext at rest, like a mail password. Methods, owner-scoped like mail's: `CreateAIAccount`, `GetAIAccount` (unscoped; the API compares the owner), `ListAIAccounts(owner)`, `ListAllAIAccounts` (tests and whole-box checks), `UpdateAIAccount` (matches `id` and `owner_user_id`, keeps `created_at`), `DeleteAIAccount(id, owner)`. A duplicate label for one owner is `ErrConflict`.

**API** (`internal/api/aiaccounts.go`). Four routes, open to any signed-in user and scoped to the caller:

- `GET /api/v1/ai-accounts` lists the caller's own accounts as `{ "accounts": [...] }`, `[]` when there are none.
- `POST /api/v1/ai-accounts` adds one owned by the caller. No elevation.
- `PUT /api/v1/ai-accounts/{id}` edits one. No elevation. An empty or missing `api_key` keeps the stored key. Another user's id is `404 no such AI account` and audits a failure.
- `DELETE /api/v1/ai-accounts/{id}` needs elevation (`requireElevated`). Another user's id is 404 and audits a failure.

The DTO is `id`, `provider_id`, `label`, `base_url`, `key_set`, `created_at`, `updated_at`. The key is in no response, no audit row and no log line; tests check the raw JSON of every response and the audit metadata.

**Validation** (plain 422s, not audited, like mail's):

- `provider_id` must be an id in `catalog.AIProviders()` or `openai_compatible` (`manifest.ProtocolOpenAICompatible`). When the provider data is empty, a listed id is `the list of AI providers is not loaded yet. Try again in a few minutes, or add an OpenAI-compatible server`, and `openai_compatible` still works. An unknown id is `unknown AI provider`.
- A listed provider needs a key (`api_key is required for this provider`). `openai_compatible` needs a base URL (`base_url is required for an OpenAI-compatible server`) and no key.
- `base_url`: trimmed, an absolute `http` or `https` URL with a host, no user name or password, no query or fragment, at most 2048 characters.
- `api_key`: trimmed, no spaces, line breaks or control characters, at most 4096 characters.
- `label`: required after trimming, at most 100 characters, no line breaks or control characters. A label the owner already uses is `409 you already have an account with that name`.

On edit the key rule is checked on the final key (the stored one when none is sent), and `provider_id` is checked against the provider data only when it changes.

**Audit** (`internal/audit/audit.go`). `ai.account.create`, `ai.account.update` and `ai.account.delete`, target kind `ai_account`, metadata `label` and `provider_id`. Success and failure are audited on the same paths as the mail endpoints: 409, 500, and 404 on edit and delete. The three actions are in `LOGGING.md` # v1 action vocabulary and in the Activity screen's label map (`web-ui/src/views/settings/ActivitySection.vue`).

**Deleting a user** (`internal/api/users.go`). `deleteUser` reads the user's AI accounts before the delete, next to their email accounts. If the host step fails and the user row is put back, the AI accounts are put back too.

The OpenAPI spec and `web-ui/src/generated/openapi.ts` are regenerated.

## How it maps to the specs

Realizes the 2026-09-26 `INSTALL_SETUP.md` rows "AI provider accounts (piece 2)", "An AI account has", "The key is never returned" and "Piece 2 is brain only", and the account half of `INSTALL_SETUP.md` # 5, which gets a short as-built note. `SERVICE_PROVISIONING.md` has a new # AI provider accounts section next to # BYO outgoing mail. `BRAIN_UI_PROTOCOL.md` lists the routes under Pattern A and has a new # AI provider accounts part. `LOGGING.md` lists the three audit actions. `SETTINGS.md` is unchanged, because there is no Settings screen for AI accounts yet. `AUTH.md` and `USERS_AND_GROUPS.md` do not list per-feature elevation, and `docs/architecture.md` has nothing that became wrong.

## Known gaps & deviations

- **No UI yet.** The setup page adds accounts inline in piece 3. The Settings screen for AI accounts comes in piece 4.
- **No check-key call.** The box makes no call to the provider when an account is saved.
- **Nothing binds an app to an account yet**, so deleting an account changes no app. What a delete does to an app that uses the account is piece 4 (`INSTALL_SETUP.md` # Open questions 1).
- **A key cannot be cleared.** An empty `api_key` on edit keeps the stored key, like a mail password. So an `openai_compatible` account that has a key cannot be made keyless except by deleting it and adding it again.
- **Choices the brief left open.** The key also refuses inner spaces, not only line breaks and control characters. Mail has no label length limit, so 100 characters was picked. An edit checks `provider_id` against the provider data only when it changes, so a rename works while the catalog has not loaded. A delete refused for missing elevation does not audit, the same as mail's.
- **Not tried against a running brain.** `make check` passes. Only the API tests exercise the routes.

## What's next

- Piece 3: the setup page reads `role` and `requires`, adds AI accounts inline, and `POST /api/v1/apps` carries a binding per slot that the brain resolves into the app's fields.
- Piece 4: the Settings screen for AI accounts, rebinding apps when a key changes, and what deleting an account in use does.
