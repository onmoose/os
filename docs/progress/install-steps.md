# Install steps: one need per step, built

- **Status:** done (steps 1 to 3 of the plan)
- **Date:** 2026-09-29
- **Specs touched:** `docs/specs/INSTALL_STEPS.md`, `docs/specs/DASHBOARD.md`, `docs/specs/BRAIN_UI_PROTOCOL.md`, `docs/specs/SERVICE_PROVISIONING.md`, `docs/specs/APP_MANIFEST.md`, `docs/specs/SETTINGS.md`, `docs/specs/DECISIONS.md`, `docs/architecture.md`, `docs/dev/web-ui.md`, `docs/README.md`

This builds the design in [install-steps-design.md](install-steps-design.md): steps 1 to 3 of `INSTALL_STEPS.md` # Suggested order. The install setup page, which asked every question at once, is now a set of steps, one need per step, and the last step installs. The design changed during the user test on this branch; the section at the end lists how. Step 4 (`recommends`) is not part of it.

## What was done

**The steps.** `web-ui/src/installSteps.ts` turns the install plan (flat `config` fields and `requires` groups) into needs and steps (`planNeeds`, `stepList`): each AI need (a required `requires` group, or one optional step for the AI fields in no group), email, one "<App> needs these to run" step for the required plain fields and plain groups, and the folder step. Optional plain fields have no step; the install sends their defaults, and the app's settings screen edits them later. The scope comes only from the App page's button. `web-ui/src/views/InstallSetupView.vue` draws every step at `/store/:id/install?step=<name>`. A step with saved accounts lists them with the newest picked, so the user sees and confirms the account there. The last step's button is Install; the other steps say Continue. Every page has two buttons at the bottom: Cancel (on the first step and the warning-only page) or Back on the left, Continue or Install on the right. Opening a step after a required step with no answer opens that step.

**AI steps.** `AccountList.vue` is the key list (the B layout, shared with email), `ServiceGrid.vue` the grid (a radio group, the provider data's order, My own server last), `AIKeyForm.vue` the form (the key box first, the optional name, the household line, the help folded under "Where do I find my API key?"), and `OptionalOffer.vue` the "Not now / Set up an AI service" offer for an optional need with no saved key. Adding a key is a sub-flow with only Back and Continue; saving goes back to the step's list with the new key picked. The binding uses the provider's default models. A My own server account has no model list, so its model names are saved on the account (a new `models` field, `openai_compatible` only, stored in `ai_accounts.models`): the server's form asks them once, the brain fills a binding's missing models from them, and no later install asks again. A server account saved before that opens its own form to add a missing name. One service fills one need. `AISlotPicker.vue` is left only on the app's settings screen. The UI and the brain's messages say "AI service"; Settings → Integrations → AI services is at `/settings/ai`, and `/settings/llm` redirects.

**Email step.** The same shape: the account list with "Don't send email", or the offer "Not now / Set up email", then `MailServiceGrid.vue` (Gmail and iCloud "For personal use", then "Sending services", then "Custom server") and `MailAddForm.vue`, which asks only what the preset needs and keeps "Test the settings first". In `internal/mailpreset` the Google Workspace preset is now "Gmail or Google Workspace" (id `google_workspace` kept), `icloud` is new (`smtp.mail.me.com`, 587, STARTTLS), and presets carry `Personal`, `AccountName`, `Steps` and `SetupURL`. The steps name the box ("paste it into the App password box") instead of a direction. Settings → Add account for AI and email uses the same grids and forms.

**Folder step.** The consent screen for folder access, for every app that uses a folder: "<App> will use your <Folder>", one line per folder saying what the app can do there (write access in red, in the same words as the permission lines), the radios only when there is a real choice, and the subfolder box.

**The App page.** It fetches the install plan. An app with no steps (Memos) and nothing to warn about installs straight from there (`needsNoPages`, `installWarnings`); with a duplicate or space warning it opens a page with only the warning. The right column has a Permissions group from the same plan (`permissionLines`).

**Brain.** AI accounts carry `models` for `openai_compatible` (validated like model ids, refused with a `body.models` location for a listed provider), and a binding that names no model for a type takes the account's (`slotModels`). The install plan carries `existing`, the copies of the app the caller can already see, from the same `visibleCopies` that the 409 check uses, so the first step warns and the install is sent with `confirm: true` only when the warning was shown. Email accounts on the plan carry `created_at`. The config and mail 422s name the part of the request they blame in `errors[0].location` (`config.fields.<APP_ENV>`, `config.ai_bindings.<slot>`, `config.requires[<i>]`, `config.mail_provider_id`, `config.folders.<folder>`), and the flow opens the step that owns it. The manifest lint refuses a `requires` group that mixes a kind or slot with a plain field.

**Tests.** Go: `TestInstallPlan_Existing`, `TestInstallPlan_NoExisting`, `TestInstall422Location` (six cases through `POST /api/v1/apps`), the mixed-group lint cases, and `TestICloudPreset`, `TestGmailPreset`, `TestPresetOrderAndPersonal` (which also keeps the steps free of "below" and "above"), `TestAIAccountModelsRoundTrip` (store), `TestAIAccountModels` (create, update, keep, and the 422 locations) and `TestResolveAIBindings_AccountModels` (a binding with no models takes the account's). `make check` and `make check-web` are green. The web-ui has no test runner; the step logic was checked with a one-off Node script over the compiled `installSteps.ts` (plan-to-needs for openclaw, cap and edge shapes, the 422 routing, and the My own server cases: listed with no model names, not a complete choice until names are known, a choice with typed names, a listed key needing none).

**Checked against a real brain.** A private stack (the stamped brain and fake host-agent from `make build`, Vite on another port, its own Caddy container) drove the flow in headless Chrome, checked through routes, DOM text and the install request. Installed for real: Excalidraw and Memos with no steps, a second copy past the warning (the audit shows `confirm: true`), OpenClaw with a key, and Gitea with a Gmail account (its `.env` has `MOOSE_MAIL_HOST=smtp.gmail.com`). "Test the settings first" reached `smtp.gmail.com` and showed Google's refusal of the fake password on the form. Also checked: Back at each point of the grid and form, browser Back and Forward, the header Cancel from a sub-page, the offer, the folder step for Immich as a single user and on a multi-user box, and a household install from the split button.

## How the design changed during the user test

- The last page ("Ready to install", every answer with a Change link, optional rows with Set up, a "For" row) was removed: every need is a step, and the last step installs (`DECISIONS.md` 2026-09-29).
- Saved accounts are listed on each step with the newest picked, instead of the step being skipped.
- An optional need with no saved account is a small offer, "Not now" or "Set up …".
- The folder step became the consent screen for folder access, shown for every app that uses a folder.
- Adding an account became a sub-flow with only Back and Continue, returning to the step's list. A Cancel link in the header was tried and removed: Cancel is now the bottom bar's left button on the first step and the warning-only page, and Back everywhere else.
- An app with no steps installs from the App page, and the App page lists the app's permissions.
- Smaller changes: the step counter starts at 1 and hides with one step, the info box is a titled list under the app name on the first step, the forms put the password box first and fold the help at the bottom, and the key form has no cost line.

## How it maps to the specs

- **`INSTALL_STEPS.md`**: # Build rules (rules 1 to 9) and "As built" notes. `DECISIONS.md` has the two 2026-09-29 entries (install from the App page; every need is a step).
- **`DASHBOARD.md` # Install authorization**: the steps, the AI and email steps, the folder step, and the install from the App page.
- **`BRAIN_UI_PROTOCOL.md`**: `existing` and `created_at` on the install plan, and the 422 `location`.
- **`SERVICE_PROVISIONING.md` # BYO outgoing mail**: the Gmail and iCloud presets, first in the table.
- **`APP_MANIFEST.md`**: the mixed-group lint rule.
- **`SETTINGS.md`**: Integrations → AI services at `/settings/ai`, and the Add account flows.

## Known gaps & deviations

- **The key picked in advance is the newest usable account,** not the one another app used most recently. That needs a brain change (`INSTALL_STEPS.md` # Build rules, rule 3).
- **The real Gmail and iCloud sends are not tested.** A box cannot send with a fake password; the user tests both presets with a real account. The iCloud host, port and username rule come from Apple's support pages, not from a send.
- **`recommends` (step 4) is not started,** so a need is either required or optional, and no step has Skip.
- **The user test with non-technical people** (`INSTALL_STEPS.md` # Suggested order, item 5) has not happened.
- **There is no web-ui unit-test runner.** The step logic was checked with a one-off Node script and in the browser, not with checked-in tests.
- **Only the config and mail 422s carry a location.** Any other 422 has none, so it shows on the last step.
- **A provider with no default model for a type it offers** still gets its tile (rule 6). Every provider in the store has defaults today.
- **The iCloud logo** is a plain cloud drawn for moose, not Apple's mark.

## What's next

- The user tests a real send with Gmail and with iCloud, from a hosted box.
- A test of the built flow with 3 to 5 non-technical people.
- Step 4: `recommends` in the manifest schema, the lint and the install plan, then tagging the store apps.
- "Most recently used" for the key picked in advance.
