# Install steps: one question per page, built

- **Status:** done (steps 1 to 3 of the plan)
- **Date:** 2026-09-29
- **Specs touched:** `docs/specs/INSTALL_STEPS.md`, `docs/specs/DASHBOARD.md`, `docs/specs/BRAIN_UI_PROTOCOL.md`, `docs/specs/SERVICE_PROVISIONING.md`, `docs/specs/APP_MANIFEST.md`, `docs/specs/SETTINGS.md`, `docs/architecture.md`, `docs/dev/web-ui.md`, `docs/README.md`

This builds the design in [install-steps-design.md](install-steps-design.md): steps 1 to 3 of `INSTALL_STEPS.md` # Suggested order. The install setup page, which asked every question at once, is now a set of pages with one question each, then a last page that shows every answer with a Change link. Step 4 (`recommends`) is not part of it.

## What was done

**Build rules.** Before any code, `INSTALL_STEPS.md` got a # Build rules section with the maintainer's decisions: how the plan's flat fields and `requires` groups become pages, what counts as already answered, the key picked in advance, duplicate info in the install plan, the session-storage draft, and default models. An "As built" note per step records the choices made while building.

**Step 1: the pages, the last page and the first-page warnings.**

- `web-ui/src/installSteps.ts` is the pure part. `planNeeds` sorts the plan into pages: a required AI group, the optional AI row, one "<App> needs these to run" page for required plain fields and plain groups, and the Extra settings row for optional plain fields. A mixed group or a required field with a role becomes plain fields on the "needs these" page.
- `web-ui/src/views/InstallSetupView.vue` draws every page. A question page is `?step=<name>`, the last page has no step, and the scope stays in the query. Opening the last page while a required answer is missing redirects to the page that owns it. A step the app does not have goes to the last page.
- The draft (the first-time pages, the pages done, the answers without secret values) lives in `sessionStorage` under `moose.install.v1.<user>.<app>.<scope>`. It is removed after the install starts or on Cancel. The first-time pages are fixed when the draft is made, so "Step 2 of 4" does not change as pages are answered.
- The last page, "Ready to install <App>": the info box (`InstallInfoBox.vue`), then For (admin on a multi-user box, its own page), the AI service rows, Settings, one row per folder (one folders page, `FolderChoices.vue`), then the optional rows with Set up. Each page saves on Continue; optional pages keep a copy until then, so Back changes nothing.
- The brain's install plan carries `existing`, the copies of the app the caller can already see, from the same `visibleCopies` that the 409 check now uses. The first page warns, and the install is sent with `confirm: true`. Email accounts on the plan carry `created_at`, so the newest can be picked.
- The config and mail 422s now name the part of the request they blame in `errors[0].location` (`config.fields.<APP_ENV>`, `config.ai_bindings.<slot>`, `config.requires[<i>]`, `config.mail_provider_id`, `config.folders.<folder>`). The flow routes an error to the page that owns that part and shows the message there.
- The manifest lint refuses a `requires` group that mixes a kind or slot with a plain field.

**Step 2: the AI pages.** Each AI need (a required group, or the optional row) has three pages: `<need>` (the key list when the user has a usable key, else the service grid, else the key form when only one service fits), `<need>-service` (the grid) and `<need>-key` (the key form). `AccountList.vue` is the B layout, `ServiceGrid.vue` the grid (a radio group, three columns, the provider data's order, My own server last), and `AIKeyForm.vue` the form (numbered steps from `help` and `key_url`, the cost line, one password box with the prefix warning, the optional name "Anthropic key", "Anthropic key 2", and for My own server the address, an optional key and a model name). Continue on the form saves the account and picks it with the provider's default models. One service fills one need. `AISlotPicker.vue` is left only on the app's settings screen. The UI and the brain's messages now say "AI service"; Settings → Integrations → AI services is at `/settings/ai`, and `/settings/llm` redirects.

**Step 3: the email pages and the presets.** `email` shows the user's accounts (newest picked, "Don't send email", "Use a different email service"), else the grid; `email-service` is `MailServiceGrid.vue` (Gmail and iCloud with "For personal use", then "Sending services", then "Custom server"); `email-add` is `MailAddForm.vue`, which asks only what the preset needs, keeps "Test the settings first", and saves the account on Continue. In `internal/mailpreset` the Google Workspace preset is now "Gmail or Google Workspace" (id `google_workspace` kept), `icloud` is new (`smtp.mail.me.com`, 587, STARTTLS), and presets carry `Personal`, `AccountName`, `Steps` and `SetupURL`. The table order is the display order, in Settings too. `MailAccountSection.vue` is gone.

**After the user test (same branch, 2026-09-29).** The user tried the built UI, and the flow changed on this branch before it merged. What steps 1 to 3 above call "the last page" is gone: every need is now a step (AI needs, email, required settings, folders with a choice), each step with saved accounts lists them with the newest picked, and the last step's button is Install. Optional plain settings have no step and keep their defaults; they stay editable on the app's settings screen (`InstalledAppDetailSection.vue`). The scope comes only from the App page's button. An app with no steps (Memos) installs straight from the App page, whose right column now lists the app's permissions; with a duplicate or space warning it opens a page with only the warning. Other changes from the test: the step counter starts at 1, the info box is a titled list under the app name on the first step, the key and email forms put the password box first and fold the help at the bottom, the key form has no cost line, help steps name the box instead of "below", and Settings → AI services and Email use the same grids and forms. Two parts are built in their simplest form while the design is decided: the first view of an AI or email step for a user with no saved account, and the folder step's wording (`INSTALL_STEPS.md` # Build rules, rule 9).

**Tests.** Go: `TestInstallPlan_Existing`, `TestInstallPlan_NoExisting`, `TestInstall422Location` (six cases through `POST /api/v1/apps`), the mixed-group lint cases, and `TestICloudPreset`, `TestGmailPreset`, `TestPresetOrderAndPersonal`. `make check` and `make check-web` are green.

**Checked against a real brain.** A private stack (the stamped brain and fake host-agent from `make build`, Vite on another port, its own Caddy container) drove the flow in headless Chrome. Installed for real through the new pages: Excalidraw (no questions); a second Excalidraw past the first-page warning (the audit shows `confirm: true`, no 409); OpenClaw after the grid and the key form with a (fake) Anthropic key, and the stored binding is `ai.anthropic` on "Anthropic key"; Gitea after adding a Gmail account through the email grid and form, and its `.env` has `MOOSE_MAIL_HOST=smtp.gmail.com`. "Test the settings first" reached `smtp.gmail.com` and showed Google's refusal of the fake password on the form. Also checked: reload keeps the answers and asks for a typed secret again, Change and Continue, the grid's arrow keys, "Don't use an AI service" and "Don't send email", a single-service need (firecrawl), the scope change and "Folders reset", Cancel clears the draft.

## How it maps to the specs

- **`INSTALL_STEPS.md`**: # Build rules and one "As built" note per step. `DECISIONS.md` has the two 2026-09-29 entries (install from the App page; every need is a step). Its status now points at `DASHBOARD.md` as the source of truth for the built flow.
- **`DASHBOARD.md` # Install authorization**: rewritten for the steps, the email and AI steps, and the install from the App page.
- **`BRAIN_UI_PROTOCOL.md`**: `existing` and `created_at` on the install plan, and the 422 `location`.
- **`SERVICE_PROVISIONING.md` # BYO outgoing mail**: the Gmail and iCloud presets, first in the table.
- **`APP_MANIFEST.md`**: the mixed-group lint rule.
- **`SETTINGS.md`**: Integrations → AI services at `/settings/ai`.

## Known gaps & deviations

- **The key picked in advance is the newest usable account,** not the one another app used most recently. That needs a brain change (`INSTALL_STEPS.md` # Build rules, rule 3).
- **The real Gmail and iCloud sends are not tested.** A box cannot send with a fake password; the user tests both presets with a real account. The iCloud host, port and username rule come from Apple's support pages, not from a send.
- **`recommends` (step 4) is not started,** so a need is either required or optional, and no page has Skip.
- **The user test with non-technical people** (`INSTALL_STEPS.md` # Suggested order, item 5) has not happened.
- **There is no web-ui unit-test runner.** The page logic in `installSteps.ts` was checked with a one-off Node script and in the browser, not with checked-in tests.
- **Only the config and mail 422s carry a location.** Any other 422 has none, so it shows on the last step.
- **A provider with no default model for a type it offers** still gets its tile (rule 6). Every provider in the store has defaults today.
- **The iCloud logo** is a plain cloud drawn for moose, not Apple's mark.

## What's next

- The user tests a real send with Gmail and with iCloud, from a hosted box.
- A test of the built flow with 3 to 5 non-technical people.
- Step 4: `recommends` in the manifest schema, the lint and the install plan, then tagging the store apps.
- "Most recently used" for the key picked in advance.
