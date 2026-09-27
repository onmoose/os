# Email accounts per user

- **Status:** done, not yet tried in a browser (see Known gaps)
- **Date:** 2026-09-25
- **Specs touched:** docs/specs/SERVICE_PROVISIONING.md, docs/specs/SETTINGS.md, docs/specs/BRAIN_UI_PROTOCOL.md, docs/specs/DASHBOARD.md, docs/specs/DECISIONS.md

## What was done

Step 2 of the install setup plan (`INSTALL_SETUP.md` # 5 and # Suggested order). It follows [install-setup-page.md](install-setup-page.md), which built the setup page with an inline "Add an account" card that only an admin saw and that went through the elevation prompt. On a hosted box that prompt is a full-page trip to the portal, and the setup form was lost on the way back. This change makes email accounts belong to a user, lets any user add them, and drops the re-prompt for add and edit. `DECISIONS.md` 2026-09-25 has the why.

**Store.** `mail_providers` gets `owner_user_id` (FK to `users`, `ON DELETE CASCADE`), and the box-wide `UNIQUE (label)` becomes `UNIQUE (owner_user_id, label)`. SQLite cannot drop a UNIQUE constraint, so `migrateMailProviderOwners` (`internal/store/mail.go`) rebuilds the table with the documented copy, drop and rename steps. It runs once, when the table has no `owner_user_id` column, after the ALTER loop so a very old table has its `provider_type` by then. Details:

- Every existing row goes to the founding admin: the oldest admin by `created_at`, then `id`, the same rule the instances owner backfill uses. If there is no admin it falls back to the oldest user of any role. If there is no user at all, the rows and their bindings are dropped with a warning, because nobody could ever sign in to use them. Neither fallback can happen on a real box: only an admin could add an account, and the last admin cannot be deleted.
- Foreign keys are switched off for the rebuild, on one pinned connection, because with them on the `DROP TABLE` would cascade `instance_mail_bindings` away and unbind every app. Before commit it runs `PRAGMA foreign_key_check` on the two tables it touched and fails on any broken row. Foreign keys go back on afterwards; a test proves the cascade works after the migration.
- One DDL function (`mailProvidersDDL`) makes both the fresh table and the rebuilt one, so the two schemas cannot drift. The `mail_providers` CREATE moved out of the big schema string for that reason.
- Store methods are owner-scoped: `ListMailProviders(owner)`, `UpdateMailProvider` matches `id` and `owner_user_id`, `DeleteMailProvider(id, owner)`. `ListAllMailProviders` exists for tests and whole-box checks. `GetMailProvider(id)` stays unscoped; the API compares the owner. `ListMailBindingsForOwner` serves the user-delete rollback below.

**Deleting a user** deletes their accounts, and `instance_mail_bindings` cascades from there, so an app bound to one falls back to unbound. That is the same thing that already happened when an account was deleted: the app's `.env` keeps the old values until its next write. `deleteUser` reads the user's accounts and bindings before the delete, the way it already reads SSH keys, and if the host step fails and the user row is put back, the accounts and bindings are put back too.

**API** (`internal/api/mail.go`). Every mail endpoint is open to any signed-in user and scoped to the caller:

- `GET /mail-providers`, `GET /mail-providers/options` and the install plan's `mail.providers` list only the caller's accounts.
- `POST /mail-providers` sets the owner to the caller. No admin check, no elevation.
- `PUT /mail-providers/{id}` needs no elevation. Another user's id is 404, and it audits a failure, since that is the "someone tried to change X" case.
- `DELETE /mail-providers/{id}` keeps elevation. Another user's id is 404 and audits a failure.
- `POST /mail-providers/{id}/test` is 404 for another user's id.
- `POST /mail-providers/verify` and `GET /mail-presets` need only a session.
- `POST /api/v1/apps` and `PUT /apps/{id}/mail-binding` accept only an account the caller owns, whoever owns the app. Another user's id gets the same `422 no such mail provider` as a missing id, and audits a failure. The one owner check is `ownMailProvider`, which maps "not yours" to `store.ErrNotFound`.
- The 500 paths on the update read, the rebind lookup and the install lookup now audit a failure too.

The 409 text for a duplicate label is now "you already have an account with that name". OpenAPI summaries changed; the spec and `web-ui/src/generated/openapi.ts` are regenerated.

**UI.** The setup page's Email row shows "Add an account" to every user and posts without `withElevation`. Settings → Outgoing email moves from the System group to You, is open to every user, and reads as "Your accounts". Its add and edit post without `withElevation`; delete keeps it. The admin redirects in both Outgoing email views are gone. On an installed app's settings screen the picker already listed the options endpoint, which is now the caller's own accounts. When the app is bound to an account the caller does not own, the picker reads "Someone else's account" instead of "Unknown account".

**Try again keeps the scope.** The progress URL now carries `?scope=household` for a household install (`useInstall.ts` reads it from the request it just sent), and the progress page's Try again link passes it back to the setup page.

**AI provider list** (`web-ui/src/aiProviders.ts`, still temporary). Checked against each provider's own docs on 2026-09-25 and corrected:

- Verified model ids: Anthropic (`claude-sonnet-5`, `claude-opus-5-5`, `claude-haiku-4-5`, `claude-fable-5-1`), OpenAI (`gpt-6-sol`, `gpt-6-astra`, `gpt-6-luna`), Gemini (`gemini-3.8-flash`, `gemini-3.7-flash`, `gemini-3.5-flash-lite`, `gemini-2.5-pro`), Groq (`openai/gpt-oss-120b`, `llama-3.3-70b-versatile`, `openai/gpt-oss-20b`, `llama-3.1-8b-instant`), Mistral (`mistral-large-latest`, `mistral-small-latest`), DeepSeek (`deepseek-flash`, `deepseek-v4-pro`), xAI (`grok-4.7`, `grok-4.6`), Cerebras (`gpt-oss-120b`, `qwen-3.8-27b`). OpenRouter: only `~openai/gpt-sol-latest`, from its quickstart.
- Verified base URLs: Anthropic `https://api.anthropic.com/v1/`, Gemini `https://generativelanguage.googleapis.com/v1beta/openai/`, OpenRouter `https://openrouter.ai/api/v1`, Mistral `https://api.mistral.ai/v1`, DeepSeek `https://api.deepseek.com` (was `/v1`), xAI `https://api.x.ai/v1`.
- Verified key pages: Anthropic `https://platform.claude.com/settings/keys` (was the old console URL), DeepSeek `https://platform.deepseek.com/api_keys`.

**Setup page fixes found while testing step 1:**

- "More" under a list of options shows only when there are more options than the first five. Before, a model list always had "More", because the search behind it was the only place to type a model id. For a provider with two models, "More" opened onto nothing.
- A model field for typing any model id now sits under the model cards at all times. `OptionCards` no longer takes a typed value.
- The Ollama tile is gone. Ollama runs on the user's own computer, which a hosted box cannot reach. "Other (OpenAI-compatible)" covers a server the box can reach, since the user types its address.

## How it maps to the specs

Realizes `INSTALL_SETUP.md` # 5 for email and step 2 of # Suggested order. `SERVICE_PROVISIONING.md` # BYO outgoing mail now describes per-user accounts, the owner rule for binding, the re-prompt on delete only, what a user delete does, and the migration. `SETTINGS.md` moves the Outgoing email row to My account, role "any user". `BRAIN_UI_PROTOCOL.md` # install-plan says the mail picker is the caller's own accounts. `DASHBOARD.md` # Install authorization and the Settings nav paragraph follow. `docs/dev/web-ui.md` drops the "admin-only" notes. `AUTH.md` and `USERS_AND_GROUPS.md` did not list the mail operations, and `docs/architecture.md` says nothing that became wrong.

## Known gaps & deviations

- **Not tried in a browser, and the migration was not run on a real database.** `make test-nopam` and `vue-tsc --noEmit` pass. The migration is covered by store tests that recreate the old table (with and without `provider_type`), but not by opening a real box's `moose.db`.
- **The AI list is only partly checked.** Not checked: the OpenAI, Gemini, Groq, Mistral, xAI and Cerebras key pages; the OpenAI, Groq and Cerebras base URLs; OpenRouter models other than the quickstart one (`anthropic/claude-sonnet-5` and `google/gemini-3.8-flash` follow its naming but are guesses); the Together and Fireworks entries, which are unchanged. The docs were read through a summarising fetch, so a model id could be wrong where the page was long. Models can always be typed, so a stale entry blocks nobody.
- **Query cache across users.** The dashboard does not clear the TanStack Query cache on logout, so on a shared browser the next user can see the previous user's cached account list for a moment before the refetch. This is not new (the apps list behaves the same), but the email list is now per user.
- **An app bound to another user's account** keeps sending through it. Only the owner or an admin can control the app, and the picker says "Someone else's account"; picking another account replaces it. Nothing moves the binding when an admin rebinds to their own account, by design.
- **Nav move.** Outgoing email moved from the System group to You. The brief asked only that every user see it; the move follows the nav's own rule that You holds what the user owns.

## What's next

- Try it: the setup page as a member (inline add, no prompt), Settings → Outgoing email as a member, delete (prompt still shown), and a box with existing accounts to watch the migration.
- Plan steps 3 and 4: provider data from the catalog service, then manifest roles and AI provider accounts with the same owner model, which replace `aiProviders.ts`.
- Sharing an account with other users, when a household asks for it.
