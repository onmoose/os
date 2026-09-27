# Install setup: close the doc gaps, and clear a deleted user's values from apps

- **Status:** done
- **Date:** 2026-09-27
- **Specs touched:** docs/specs/INSTALL_SETUP.md, docs/specs/BRAIN_UI_PROTOCOL.md, docs/specs/SERVICE_PROVISIONING.md, docs/specs/SETTINGS.md, docs/specs/DASHBOARD.md, docs/specs/NEXT.md, docs/specs/APP_STORE.md, docs/specs/APP_MANIFEST.md, docs/specs/MOOSE_NETWORK.md

## What was done

A doc audit of the install setup plan after its `os` side was built: [install-setup-page.md](install-setup-page.md), [email-accounts-per-user.md](email-accounts-per-user.md), [ai-provider-data.md](ai-provider-data.md), [manifest-roles.md](manifest-roles.md), [ai-accounts.md](ai-accounts.md), [ai-slot-filling.md](ai-slot-filling.md) and [ai-bindings-after-install.md](ai-bindings-after-install.md) (#503 to #508). Four read-only passes compared the docs with the code: the API and its audit surface, the brain internals and app authoring, the dashboard, and the plan with the doc maps. Most of it was already right. The API spec, `LOGGING.md`, `docs/architecture.md`, `APP_MANIFEST.md` # D4 and `docs/dev/web-ui.md` all match the code, and the OpenAPI files were fresh. The audit found one code bug and a set of doc gaps. This change fixes both.

**The bug: a user delete left the user's AI and email values in apps.** [ai-bindings-after-install.md](ai-bindings-after-install.md) made an account delete clear the account's values from the apps that used it and restart them. A user delete was left on the old path: `deleteUser` removed the user row, and the accounts and bindings went by cascade, but `instance_config` kept the AI values and nothing rewrote the apps. A household app that used the deleted user's key kept running with it, and never showed "needs setup", because the values were still there. `SERVICE_PROVISIONING.md` said the two paths behave the same, so fixing only the doc would have written the bug down as intended.

- **Store.** `store.DeleteUserAndAIValues` clears, in the same transaction as the user row, the values each binding to one of the user's AI accounts gave its app. It shares the loop with `DeleteAIAccountAndValues` (`clearBindingValues`), so both follow one rule: the bindings are read inside the transaction, a binding clears the fields it recorded, and only an old binding with none recorded asks the manifest copy. If that fails, nothing is deleted (`*SlotFieldsError`). It returns the values it cleared, and `store.RestoreConfigValues` puts them back. A value written since is kept (`INSERT OR IGNORE`).
- **API.** `deleteUser` calls it in place of `store.DeleteUser`. A refused lookup is the same 500 as the account delete, naming the app, and now says "The user was not deleted". It goes through `restoreSSH` like every failure after the SSH revoke. The "name the slot's fields" closure and the refusal answer moved into `bindingSlotFields` and `slotFieldsRefused` in `internal/api/aiaccounts.go`, so the two deletes cannot drift apart. A failed host step now also restores the cleared values, after the accounts and bindings. On success a `user-delete` job runs `lifecycle.RestampConfig` for the AI apps and `lifecycle.RestampMail` for the apps bound to the user's email accounts. The answer is `200 {job_id}` when an app was reached and `204` when not, the same shape as the account deletes. The route declares both answers and its errors in OpenAPI, for the reason [ai-bindings-after-install.md](ai-bindings-after-install.md) gives: a custom 204 stops huma adding its default error. The UI's delete call ignores the body, so it needed no change.
- **Tests.** `TestDeleteUserAndAIValues` (store: another user's values untouched, recorded fields skip the manifest, a refused lookup deletes nothing, restore keeps a newer value). `TestDeleteUserClearsTheirAccountsFromApps` (API: values gone at once, then the job drops the key from the override and the `MOOSE_MAIL_*` lines from the `.env` of an app owned by someone else). `TestDeleteUserRestoresAIValues` (a 502 puts the values back). `TestDeleteUserRefusedWhenManifestUnreadable` (the refusal is audited and deletes nothing). The first and last fail against the old handler. Two existing tests changed with the behaviour: `TestDeleteUserMailAccounts` expects 200 with a job for a user whose account an app used, and `TestDeleteUserRestoresAIBindings` seeds a manifest copy, because a delete now refuses without one.

**The doc gaps.**

- **`INSTALL_SETUP.md` was still "draft, under discussion"** with no open questions and every step built on the `os` side. Its status now says it is the record of the plan, names the spec that owns each built part, and says the spec wins if they disagree. "Where we are today" is now "Where we started (2026-09-25)" and says it is history. A pointer to a closed "Open questions 1" now points at the decisions table. Two 2026-09-27 rows record the base URL rule and the user delete. `docs/README.md` describes it the same way.
- **The base URL removal rule** from the last review round of #508 was only in `BRAIN_UI_PROTOCOL.md`. It is now in the plan's decisions table and in `SERVICE_PROVISIONING.md` # AI provider accounts.
- **"Install dialog"** in `APP_STORE.md` (the footprint estimate), `APP_MANIFEST.md` and `MOOSE_NETWORK.md` (the HTTPS warning, not built yet), and `NEXT.md` (the folder permission block) now says "install setup page".
- **`SETTINGS.md`**'s panel inventory had no Installed apps row, and `DASHBOARD.md`'s Settings nav description left SSH out of the System group. Both now match `SettingsLayout.vue`.
- **`docs/dev/authoring-apps-with-an-agent.md`** said nothing about `role`, `separator`, `requires` or `--ai-providers`. The validator section now says what the roles lint catches and why it is the only place a bad role shows. The prompt's env var step says how to tag AI provider settings, and the validate step passes `--ai-providers ../store/ai_providers.yml`. The advice was checked by running the lint on a sample manifest, with a good protocol and a misspelled one.
- **`NEXT.md`** gains # Install setup: deferred limits: the provider-choosing field, one slot per protocol, and account sharing. Its Outgoing mail entry no longer says the accounts are admin-registered.
- **`docs/progress/README.md`**'s Up next list gains the two follow-ups of [ai-bindings-after-install.md](ai-bindings-after-install.md), and its issue link points at `onmoose/os` rather than the pre-rename repo.
- **`catalog-import-gaps.md`** `conditional-config-requirement — flipt` notes that `requires` shipped and why it does not close that class.

## How it maps to the specs

- `SERVICE_PROVISIONING.md` # BYO outgoing mail and # AI provider accounts: a user delete now does what an account delete does to the apps the accounts reached, as the spec already claimed.
- `BRAIN_UI_PROTOCOL.md` # Deleting a user is new, and Pattern A lists `DELETE /api/v1/users/:id`.
- CLAUDE.md # Brain commits first: the values are cleared in the brain's transaction, and a host failure rolls them back with the user row.

## Known gaps & deviations

- **`capabilities.yml` is unchanged, on purpose.** One audit pass proposed an entry for AI slot filling. The ledger's contract (`CAPABILITIES.md` # The append-only discipline) says an id appears only when a `catalog-import-gaps.md` gap-class closes, and no gap-class is about AI settings. Flipt's `conditional-config-requirement` is related but not closed (see above). An entry would break the rule the file exists to keep.
- **An app reached by both an AI and an email account of the deleted user restarts twice**, once per re-stamp. Folding the two into one lifecycle call was not worth a new lifecycle method for a rare admin action.
- **The job outcome is not audited**, the same as the account deletes ([ai-bindings-after-install.md](ai-bindings-after-install.md) # Known gaps). A failed re-stamp is logged, and the job answers with the apps it could not update, but the Users screen does not watch the job.
- **`INSTALL_SETUP.md` still holds a full copy of the built shape** in its "As built" notes. The status line now says the specs win. Cutting the copies was left out: they are the record of how each piece was built, and removing them is a rewrite, not a gap fix.
- **Not tried on a running brain or in a browser.** Go tests only. `make check` cannot run here (the PAM headers fail to compile), so `make test-nopam`, `gofmt`, `go vet`, the OpenAPI freshness check and `make check-web` were run instead. CI runs the full gate.

## What's next

- Tag the store manifests with `role` and `requires` (`onmoose/store`), so real apps show the pickers. This is the last step of the plan.
- Try the settings screen, the LLM providers screen, a key change and a user delete against a running brain with a real app, in a browser.
