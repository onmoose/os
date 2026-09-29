# Install steps: one question per page

> **Status: designed 2026-09-28. Steps 1 to 3 of # Suggested order built 2026-09-29 (`docs/progress/install-steps.md`); step 4 (`recommends`) and the user test are not done.** This doc follows `INSTALL_SETUP.md`, which built the setup page, provider accounts and role-mapped settings. The built flow is described in `DASHBOARD.md` # Install authorization, which is its source of truth; the wire shapes are in `BRAIN_UI_PROTOCOL.md` and the email presets in `SERVICE_PROVISIONING.md` # BYO outgoing mail. This doc keeps the design, the build rules and an "As built" note per step, the same way `INSTALL_SETUP.md` did. The parts not built yet (need levels with `recommends`, the most recently used key) are still planned here.

## Goal

The install setup page (`/store/:id/install`) shows every question at once: permissions, folder sources, a subfolder box, email, LLM providers, plain settings, size, the duplicate warning and errors. The AI row alone has provider tiles, account cards, an add form, model pickers, and its own Save, Remove and Cancel. For a non-technical user it is hard to tell what they must answer and what is there only to read.

The worst trap is the nested save. "Add account" sits inside the provider editor's "Add" or "Save", which sits inside "Install". A user who adds a key and forgets the editor's Save links nothing. Install stays disabled and says "Still needed".

The new flow asks one question per page, shows the pages only when the app needs them, and lets a user who already has everything set up confirm each saved choice with one click.

## What apps ask for today

Counted from the `onmoose/store` manifests on 2026-09-28 (112 apps).

| Case | Apps | Today |
|---|---|---|
| Needs nothing | about 90 | Permissions, folders, size. |
| Email, optional | 17 (Paperless, Gitea, Forgejo, Plane, Penpot, NocoDB, …) | An Email row with None and account cards. All use `mail: {optional: true}`, the only form v1 allows. |
| AI, required (any provider) | openclaw, hermes-agent | `requires: [{one_of: [ai]}]`. Tiles for several native slots plus the compatible slot. |
| AI, optional | cap, osiris, firecrawl, open-seo, openmuse | The same picker, next to plain fields. firecrawl takes only OpenAI, and open-seo only OpenRouter. |
| AI with a model setting | open-seo (`ai.openrouter.model.chat`), the compatible slot of openclaw and hermes-agent | Model cards and a typed model box. |
| Plain keys with no role | cap (Resend, Google sign-in, AssemblyAI), osiris (8 keys), postiz (18), openmuse (CopilotKit, `MODEL`) | Text boxes, some required. |

## Decisions (2026-09-28)

| Decision |
| --- |
| One question per page. Each page has one Continue button, and Continue saves that page's answer. There is no Save button inside a page. |
| Every need is a step, and the last step's primary button is Install (2026-09-29; this replaces a last page that showed every answer with a Change link). A step with saved accounts lists them with one picked, so nothing is picked without the user seeing it, and a user with a saved key still sees the step. |
| A saved answer is picked in advance but never hidden: the step shows the saved choice selected, and the user can pick another or add a new one. |
| Changing the AI key opens a list of the user's saved keys first, each with its service's logo, with "Use a different AI service" at the bottom (the "B" layout below). The service grid shows only when the user has no usable key, or asks for another service. |
| The service grid shows every service the app can use, three columns wide on a computer. No search, no More button, no Recommended badge. |
| Services are in **popularity order**. Moose never marks a service as recommended and never moves one up for its own reasons. If moose offers its own AI service one day, it takes the place the same rule gives it. |
| Models are not asked during the install. The install uses the provider's default model for each model setting. The user changes the model later in the app's settings screen in moose, or inside the app. The one exception is "My own server", which has no model list: its key page has a model name box. |
| One AI service per need. An app that can use several for the same need (openclaw) gets its second one from its settings screen. An app with two separate AI needs (two `requires` groups on different slots, for example chat and embeddings) gets one set of AI pages per need. |
| Needs have three levels: **required** (a step, no "Don't use" choice), **recommended** (a step with Skip and one sentence on what the user misses; not built yet), **optional** (a step with a "Don't use" choice: "Don't use an AI service", "Don't send email"). Optional email and AI stay inside the install flow. Optional plain fields (Extra settings) have no step: they keep their defaults, and the user changes them later on the app's settings screen. |
| The info box (permissions, size, not-enough-space) is quiet, has no controls, and sits under the app name on the first step only. The other steps show one line: icon, name, and "Step 2 of 3". |
| The duplicate-install and not-enough-space warnings show at the top of the first step, before any question. An app with no steps but a warning opens one page with only the warning, "Install my own copy" (or Install, for space) and Cancel. |
| An optional need is used only when the user leaves a saved account picked on its step, or adds one: no app starts sending email or spending on AI without the user seeing the step. The step's "Don't use" choice leaves it unused. With no saved account, an optional step is a small offer: "Not now" (picked in advance) or "Set up email" / "Set up an AI service", which opens the service grid. A required need never gets "Not now". |
| The folder step is the consent screen for folder access. It shows for every app that uses a folder, even with only one possible source, with one line per folder saying what the app can do there (write access in red). |
| Gmail and iCloud are the first two email presets. The Google Workspace preset becomes "Gmail or Google Workspace", and iCloud is added. The sending services (SES, SendGrid and the rest) follow under their own heading. |
| (2026-09-29) An app that needs no input from the user shows no install pages. Install on the App page starts the install and goes straight to the progress page. The App page lists the permissions the app needs in a Permissions group in its right column. This holds only when there is nothing to warn about (no existing copy, enough space); otherwise a page with the warning opens. |
| Out of scope for this plan: checking a key against the provider before saving it, and starting the image download before the user presses Install. Both are in `NEXT.md`. |

## Build rules (2026-09-29)

These rules were set when the build started (steps 1 to 3 of # Suggested order). They fill gaps in the design above. They can change on the build branch as we learn, and this doc changes with the code.

1. **From the install plan to steps.** The install plan sends flat `config` fields and `requires` groups. The UI turns them into steps with this rule (as built 2026-09-29, after the user test):

   | What the plan has | Where it goes |
   |---|---|
   | A `requires` group with `ai` in it | A required AI step (key list, or service grid then key form) |
   | AI role fields with no group (cap, firecrawl) | An optional AI step, with "Don't use an AI service" |
   | `mail` block (always optional in v1) | The email step, with "Don't send email" |
   | Required plain fields (no role) | One "<App> needs these to run" step |
   | Any folder | One folder step for all of them: the consent screen for folder access |
   | Optional plain fields (no role) | No step: they keep their defaults |

   The order is: AI needs, then email, then required plain fields, then folders. Until `recommends` exists (step 4), a need is either required or optional. The schema allows two shapes that no store app uses: a mixed group (for example `one_of: [ai, SOME_ENV]`) and a required field with a role. The manifest lint (`internal/manifest/lint.go`) rejects both. The box is lenient, so when it meets one anyway, it shows the fields of that group or slot as plain fields on the "needs these to run" step. A group of plain fields only (`one_of: [A, B]`) goes on that step too, and its Continue waits until the group is met.
2. **Saved answers.** Every step shows, even with a saved answer. AI: the saved accounts that can fill the need, the newest picked. Email: the saved accounts, the newest picked, and "Don't send email". With no saved account, an optional AI or email step is the offer: "Not now", picked in advance, or "Set up …", which opens the grid; the heading is "<App> can send email" or "<App> can use an AI service", with the quiet line "You can set it up later in the app's settings." (the `recommends.without` sentence goes there once step 4 exists). A folder has a default, picked on the folder step.
3. **The key picked in advance** is the newest usable account for now. "The one another app used most recently" needs a brain change and comes later.
4. **Duplicate info in the install plan.** The brain adds the copies of the app the caller can already see to the install plan, so the first step warns before any question. The draft records that the warning was shown, and only then is the install sent with `confirm: true`. A user who opened a later step directly never saw it, so they get the 409 on the last step, above its Install button, which is also where a 409 for a copy that appears later (a second tab, a race) shows.
5. **Session storage.** The draft is kept in the tab's session storage under one key per user, app and scope. It is removed after a successful install or on Cancel. Only non-secret answers are stored: picked account ids, folder choices, and non-secret plain fields. A `secret: true` field and a key that is typed but not saved stay in the page's memory only. The URL carries only the step and the scope, not the answers.
6. **Default models.** A provider with no default model for a type it offers still gets its tile for now. The stricter rule in # 3 (hide such a tile) waits for the store lint that makes every provider carry a default for every type it offers. On 2026-09-29 every provider in `onmoose/store` `ai_providers.yml` has a default for every type it offers, so no tile is affected today.

7. **After the user test of the built UI (2026-09-29).** The step counter starts at 1 on the first page after Install. On every page the order is: the app's icon and name, the info box right under it (first step only), then the question as the heading of the choice under it. The info box starts with the size as its own line, then a titled list of permissions (or one line saying the app needs no special permissions). The key form and the email form read, top to bottom: the password box, the optional name (an always-visible input with only a bottom border, no hint), the household line (key form, household install only; there is no cost line), then the steps folded into "Where do I find my API key?" or "Where do I find my app password?" at the bottom, above "Test the settings first" (email form) and Continue. Neither form says "Saved on your box".

8. **No install pages for an app with no steps (2026-09-29).** `needsNoPages` in `installSteps.ts` decides it, from the plan alone: no field with an AI role, no required plain field or plain group, no `mail`, and no folder. Optional plain fields do not count; the install sends their defaults. `installWarnings` stops the skip when the plan lists an existing copy or the space is tight, and the warning page opens. Then Install on the App page (either scope) sends the install at once, with no confirm, and the 202 goes to the progress page. While the plan loads, Install waits; if the plan cannot load, Install opens the install pages. A 409 (a copy made after the plan was read) or any other error opens the install pages with it shown on the last step (for an app with no steps, the warning-only page).
9. **Every need is a step (2026-09-29, after the user test).** There is no summary page (# 2). `stepList` in `installSteps.ts` gives the steps in order. The last step's primary button is Install; on the other steps it is Continue. Adding a new account is a sub-flow of its step (the service grid, then the key or account form) whose bottom bar has only **Back** and **Continue**: on the grid, Continue opens the chosen service's form; on the form, Continue saves the account and goes back to the step's list with it picked, and the step's own button (Continue, or Install on the last step) then moves on. So the form's button is never Install, and a first-time openclaw user sees grid, form, list with the new key picked, Install. Back goes one page back, as the browser's Back does: form, grid, the page the user came from (the step's list or offer); when the grid is the step's first view (a required AI need with no saved key), the previous step, or the App page from the first step. "Don't use an AI service" and "Don't send email" are only choices in the step's list or offer, for optional needs, never buttons. Every page has two buttons at the bottom, as in # 1 Buttons: the left one is **Cancel** (clears the draft and ends the install) on the first step's own page and on the warning-only page, and **Back** everywhere else. There is no Cancel in the header. Settings → Add account has Back and Continue. The bare path opens the first step; opening a step after a required step with no answer opens that step instead. A 422 opens the step that owns it (`stepForError`); an error no step owns, and a 409, show on the last step above Install. The two parts that were held for a decision are built as decided: the offer for an optional step with no saved account (`OptionalOffer.vue`, chosen by `emailMode` and `aiMode` in `InstallSetupView.vue`), and the folder step as the consent screen (`FolderChoices.vue`): shown for every app that uses a folder, headed "<App> will use your <Folder>" (or "<App> will use these folders"), with one line per folder in the permission lines' words (`folderAccess`), write access in red, the radios only when there is a real choice, and the subfolder box as before.

**As built (the steps, 2026-09-29).** The step logic is `web-ui/src/installSteps.ts` and the steps are drawn by `web-ui/src/views/InstallSetupView.vue`. The choices made:

- **Step names.** Each AI need has a name: `ai`, `ai-2`, … for the required groups in `requires` order, and `ai-optional` for the optional one. The need's first page is its name, its service grid is `<name>-service` and its key form `<name>-key`. The other steps are `email` (with `email-service` and `email-add`), `settings` and `folders`. A page the app does not have goes to its step, and a key page with no service picked yet goes to its need's first page.
- **Folders are one step,** with every folder the app uses: what the app can do there, the source (yours or the household's) when there is a choice, and the subfolder, the default picked.
- **Every step saves on Continue.** The settings, email and folder steps keep a copy while the page is open, so leaving with Back changes nothing. A reload drops an edit not yet saved with Continue. If the email service list cannot load, the page says so, with Try again, and "Don't send email" still works. A saved email service that no longer exists sends the user to the grid.

**As built (AI steps, 2026-09-29).** The AI pages are `components/install/AccountList.vue` (the key list, shared with email), `ServiceGrid.vue`, `AIKeyForm.vue` and `OptionalOffer.vue`, and the need logic is in `installSteps.ts` (# AI needs). `AISlotPicker.vue` is now used only by the app's settings screen. The choices made:

- **A need's first page** shows the key list when the user has a key the app can use. With none, an optional need shows the offer ("Not now" or "Set up an AI service"), and a required one the service grid, or the key form when only one service fits (firecrawl, open-seo). A first-time openclaw user sees the grid, the key form, then the list with the new key picked and Install.
- **My own server** has no model list, so its account is usable once the model names for the slot are known. The names typed on its form are kept per account in the draft (`serverModels`). A saved server account is always in the list; when it is picked and its names for this slot are not known, the name boxes show under the list, required, before Continue.
- **When one service fits,** the key list offers "Add another key" instead of "Use a different AI service", since there is no grid to go to.
- **An optional need's key list** ends with "Don't use an AI service", which leaves the need unused. The draft remembers the choice (`declined`), so a return to the step shows it again.
- **Continue on the key form** saves the account (`POST /api/v1/ai-accounts`) and picks it, with the provider's default models, then goes back to the key list. A model is asked only for My own server, one box per model setting of the slot.
- **The default name** is "<Service> key", then "<Service> key 2", and for My own server "My server".
- **Words.** The Settings screen is Settings → Integrations → AI services at `/settings/ai`, and `/settings/llm` redirects there. Its Add account uses the same `ServiceGrid` and `AIKeyForm` (without an app, so no model box and no household line), then goes back to the list. "My own server" is also the tile's name in Settings. The brain's messages the user can read (the `missing` sentence and the 422s) say "AI service" too.

**As built (email step, 2026-09-29).** The email pages are `components/install/MailServiceGrid.vue` and `MailAddForm.vue`, and the account list is the same `AccountList.vue` as the AI key list. `MailAccountSection.vue` is gone. The presets are in `internal/mailpreset`. The choices made:

- **Pages.** `email` shows the user's accounts when they have one (newest picked, "Don't send email", "Use a different email service"), else the offer ("Not now" or "Set up email"). `email-service` is the grid and `email-add` the add form for the service picked there; saving goes back to `email` with the new account picked.
- **The preset table carries the order and the words.** The table lists Gmail and iCloud first with `personal: true`, then the sending services, then "Custom server" (renamed from "Custom SMTP server"). A personal preset has `steps` (the numbered steps, the first one about 2-Step Verification or two-factor authentication), a `setup_url` for the "Open your Google account" or "Open your Apple account" button, and `account_name` ("Gmail"). Settings → Integrations → Email reads the same table and uses the same `MailServiceGrid` and `MailAddForm` for Add account. In Settings the form also keeps a preset's server settings behind a closed "Server settings" section. Its add form asks iCloud's address once, but shows the username for Gmail or Google Workspace (prefilled from the address), so a Workspace alias can still be saved there.
- **The add form** asks only what the preset needs: the address (for Gmail and iCloud it is also the username), the region for SES and Mailgun, the username for the presets whose user supplies it (SES, Mailgun, Brevo, SMTP2GO, Custom server), the credential, and the server settings for Custom server only. For Custom server the username and password are optional, because a relay on the home network may need no login; the brain allows that too. A password with no username is refused on the form, because the brain signs in only when a username is set. Every preset needs its credential. The name is optional and defaults to "Gmail", then "Gmail 2". "Test the settings first" is on by default, as in Settings.
- **The iCloud logo** in `assets/mail-providers/icloud.svg` is a plain cloud drawn for moose, not Apple's mark.

## Design

### 1. The steps

```
App page ──Install──▶ step 1 ──▶ step 2 ──▶ … ──▶ last step ──Install──▶ Progress
```

1. **App page.** The Install button, and for an admin on a multi-user box, the split button with "Install for the whole household", as today. The scope comes from the button pressed; there is no scope step. The App page reads the install plan to decide whether the install steps are needed. From the same plan, its right column has a **Permissions** group between Extra costs and Information: one line per permission with a small icon, in the same words as the info box (`permissionLines` in `installSteps.ts`), folder write access in red. The group is hidden for an app with no permissions and while the plan loads. Nothing else on the App page changes; the Size row stays in Information. For an app with no steps and nothing to warn about, it is the only page before the progress page.
2. **Steps,** one per need, in the order of # Build rules, rule 1: AI needs, email, the required plain fields ("Cap needs these to run", which share one step because they usually come from one outside service), then the folders. The folder step is the consent screen for folder access, so it shows whenever the app uses a folder. The AI and email steps are in # 3 and # 4 below. A step with saved accounts lists them with the newest picked; a user with a saved key still sees the step.
3. **The last step's** primary button is Install. Above it show a 409 (a copy made after the plan was read) with "Install my own copy", and any error no step owns.
4. **Progress page,** as today (`/store/:id/install/:jobId`).

An app with no steps (no AI, no email, no required settings, no folder) and nothing to warn about goes from the App page straight to the progress page. With a warning, it opens one page with only the warning, "Install my own copy" (or Install, for space) and Cancel.

**Header.** App icon and name, then, on the first step only, the info box right under the name, because it belongs to the app. The box is grey, uses smaller text and has no controls. It starts with the size as its own line ("Takes about 1.2 GB"), and the not-enough-space warning when it applies. Then a title, "Openclaw needs these permissions:", and one line per permission as a list of nouns ("Internet access", "The Documents folder (can add, change, and delete files)") (write access to a folder stays in red, because that is the one line people must notice). The title shows even with one permission. An app with no permissions shows "<App> needs no special permissions." instead of an empty list. The size line is left out when the app's images are already on the box (the size is then 0), so that sentence keeps the box from being empty. The duplicate warning shows under the box on the first step. The step's question comes after the header, as the heading of the choice under it. The household line on the AI key form ("Your key pays for everyone at home who uses Openclaw.") tells an admin that their key pays for the whole household.

**Examples**

- **openclaw, user already has an Anthropic key.** App page, then one AI step with the key picked, whose button is Install. Two pages.
- **openclaw, user has no key.** App page, service grid, key form (Continue saves the key), the AI step's list with the new key picked, Install.
- **Paperless, user has an email account.** App page, email step (the account picked, "Don't send email" as a choice), folder step ("Paperless-ngx will use your Documents") with Install.
- **Paperless, no email account.** App page, email step ("Paperless-ngx can send email": "Not now" picked, or "Set up email"), folder step with Install.
- **Memos, no steps.** App page, then the progress page.
- **Immich, one Photos folder.** App page, then the folder step ("Immich will use your Photos") with Install.

**Buttons.** Every page has two buttons, both in the bottom bar, and none in the header:

| Page | Left button | Right button |
|---|---|---|
| The first step's own page (its list, offer, a grid that is its first view, settings, folders) | Cancel: clears the draft and goes to the App page | Continue, or Install if it is the only step |
| A later step's own page | Back: to the previous step's own page | Continue, or Install on the last step |
| An add-a-service sub-page (a grid opened from a list or offer, and the key or account form) | Back: one page back, form, grid, the step's list or offer | Continue |
| The warning-only page (no steps, a duplicate or space warning) | Cancel | Install my own copy, or Install |

The browser's Back does the same as the Back button. The 409 box above Install has its own Cancel, which only closes the box.

**Routes.** A step is `/store/:id/install?step=<name>` (for example `?step=ai-key`), so Back, browser Back and reload all work. A query parameter keeps the step names clear of the progress route's `:jobId`. The scope (`?scope=household`) stays in the query string on every page. The bare `/store/:id/install` opens the first step. Opening a step while an earlier required step has no answer (a bookmark, a reload of a fresh tab) opens that earlier step. The URL carries only `?step=` and `?scope=`, never an answer. The answers are kept in the tab's session storage, so a reload or a trip to the provider's site in another tab loses nothing. Only non-secret answers are kept: a picked account id, a folder, a non-secret plain field. A field with `secret: true` (a plain key with no role, such as cap's Resend key) and an AI or email key typed but not yet saved never go into storage. They stay in the page's memory, and a reload asks for them again.

**Step counter.** "Step 1 of 3" counts only the steps this app shows. The App page is not a step, so the first page after Install is step 1. A step's sub-pages (the service grid and the form) have the step's number. With only one step there is no counter, because "Step 1 of 1" says nothing.

**Errors.** A 422 from `POST /api/v1/apps` opens the step that owns the part of the request it blames, with the error on that step. One no step owns shows on the last step above Install. The first step already warned about a duplicate, from duplicate info the install plan carries. A 409 can still come back (a second tab, a race), so the last step shows it with "Install my own copy" and Cancel.

### 2. The last page (removed 2026-09-29)

A last page, "Ready to install Openclaw", used to show every answer with a Change link (the GOV.UK "Check your answers" pattern), the optional rows with Set up, a "For" row for the scope, and Install. A user whose needs all had saved answers went straight to it. It was removed after the user test: the user wants to see and confirm the account in one place, the step, and a summary page added a click without adding information (`DECISIONS.md` 2026-09-29).

### 3. AI pages

**Which services show.** The existing slot rule (`INSTALL_SETUP.md` # 1), made stricter by one condition: a service shows when it can fill one of the app's slots, has a model of every type the slot asks for, **and has a default model for each of those types**. The last part is new, because with no model page the default is the only way to choose (see Models below). When only one service can serve the app (firecrawl: OpenAI; open-seo: OpenRouter), there is no grid. The key page names the service instead: "Firecrawl works with OpenAI."

**Which key should Openclaw use?** (the AI step, when the user has usable keys)

```
Which key should Openclaw use?
● [A] Anthropic key
○ [A] Work key
○ [O] OpenAI key
─────────────
Use a different AI service →
```

- It lists only keys this app can use. Picking a key also picks the service.
- The key picked in advance is the one another app used most recently, or else the newest.
- "Use a different AI service" opens the grid, then the key form.

**Which AI service should Openclaw use?** (no usable key yet)

- Every service the app can use, in popularity order, three columns on a computer, two or one on a phone. The service's name is text on the tile, not only a logo.
- "My own server" is the last tile.
- A tile does not say "Recommended" and is not larger than the others.
- Required AI has Continue only. Recommended AI adds Skip.

**Your Anthropic key** (the new-key form)

- Top to bottom: the password box (the main thing on the page, with the prefix warning), the name, the household line when it applies, and at the bottom, above Continue, a closed section "Where do I find my API key?". It holds the numbered steps, with an "Open Anthropic" button that opens a new tab. The user opens it only if they need it. The steps come from the provider data's `help` and `key_url`, and the last one names the box by its label ("paste it into the Anthropic key box"), never by where it is, so a change of layout cannot make it wrong. The email presets' steps follow the same rule.
- There is no cost line. What a service costs depends too much on the model for the form to say anything useful, and the app's own cost estimates stay on the App page.
- One password box. The prefix warning stays.
- The name is an input that is always there, right under the key box, with only a bottom border and the placeholder "Name this key (optional)". Left empty, the key gets the default name without a word on the page. The default name is the service name plus "key" ("Anthropic key"), then "Anthropic key 2" and so on, so a name is never needed to go on.
- Continue saves the key as the user's account, picks it, and goes back to the step's key list, where the step's own button moves on. The form's bottom bar has only Back and Continue. If the user then leaves the install, the key stays saved and shows in Settings. That is on purpose, because they can reuse it.
- **My own server:** the address box, the key marked optional, and a model name box when the app's slot has a model setting.
- **Household install:** the page adds "Your key pays for everyone at home who uses Openclaw."

**Models.** The binding is sent with the provider's default model for each model setting, and the brain already fills in defaults (`INSTALL_SETUP.md` # 5). A provider with no default for a type the slot needs cannot fill that slot, so its tile is hidden for that app. The store should give every provider a default for every model type it offers. The model pickers stay on the app's settings screen (`AISlotPicker.vue`), where the user can change the model later.

### 4. Email pages

**Paperless can send email** (no saved account yet; optional email)

- Two choices: "Not now", picked in advance, and "Set up email →". Under them: "You can set it up later in the app's settings."
- "Set up email" and Continue open the service grid below, then the add form. Saving goes back to the email step, now the account list with the new account picked. Back on the grid returns to this offer.
- A required email need (none exists in v1) starts at the grid, with no "Not now".

**Which email should Paperless send from?** (the service grid)

- The presets as tiles in two groups. First, the two most common accounts people already have: **Gmail** and **iCloud**. Then, under the heading "Sending services", SES, SendGrid, Mailgun, Postmark, Brevo, Resend and SMTP2GO. "Custom server" comes last.
- Recommended email adds Skip and the app's sentence, for example: "Without email, you can't reset a forgotten password."

**Which email account?** (the email step, when the user has accounts)

- The same B layout as AI: saved accounts with their service's logo, the one used most recently picked in advance, then "Use a different email service". For an optional or recommended need it also has "Don't send email".

**Your Gmail account** (the add form)

- It asks only what the preset needs: the address, and the app password. For SES and Mailgun it also asks the region and the username. "Server settings" appear only for Custom.
- The account name is optional, with a default like the AI key's ("Gmail").
- Gmail and iCloud get numbered steps for making an app password, in a closed section "Where do I find my app password?" at the bottom of the form, above "Test the settings first" and Continue. The name input is right under the password box. The first step is turning on 2-Step Verification (Google) or two-factor authentication (Apple), because that is where people get stuck. The account name is the same quiet input as the key's name.
- The "Test the settings first" check stays as it is today.

**Gmail and iCloud presets.** Both send over SMTP on port 587 with STARTTLS and an app password. Port 587 is open from hosted boxes (`SERVICE_PROVISIONING.md` # BYO outgoing mail).

- **Gmail or Google Workspace:** `smtp.gmail.com`, 587, username is the full address, the credential is a 16-character app password. It is the existing `google_workspace` preset with a new label and help text that covers personal Gmail too. Only the label changes: the id `google_workspace` stays, because saved accounts and the logo map use it. Password-only sign-in for apps ended in 2025, and app passwords still work for SMTP. Google pushes OAuth instead and could remove app passwords later, and a Workspace admin can turn them off.
- **iCloud:** `smtp.mail.me.com`, 587, username is the full iCloud address, the credential is an app-specific password from appleid.apple.com (Sign-In and Security). Apple's support page gives port 587. The earlier exclusion ("465-only in practice") was wrong.
- Both are for low volume: Gmail allows about 500 emails a day. Password resets and reminders for a household fit easily. The tile says "For personal use".

### 5. Need levels in the manifest

Today a manifest can say *required* (`required: true`, `requires`) or leave a need optional. It cannot say "the app works, but badly, without this". The plan adds a `recommends` list next to `requires`, with the same `one_of` groups, plus `mail` as a member, and a required `without` sentence:

```yaml
mail:
  optional: true
recommends:
  - one_of: [mail]
    without: "You can't reset a forgotten password."
  - one_of: [ai]
    without: "Cap can't write summaries of your recordings."
```

- A recommended need gets its own step with Skip. The `without` sentence shows on that step.
- A need that is neither required nor recommended is optional: a step with a "Don't use" choice.
- The box reads `recommends` leniently, like `requires`. An older box ignores it, and the need shows as optional.
- The lint rejects a need that is both required and recommended, and a `without` sentence that is empty.
- `mail` is a special member. Today a `one_of` member is a kind (only `ai`), a slot, or a plain `app_env`, and a member that matches no field is dropped. `mail` matches no field, so it must be resolved from the manifest's `mail:` block instead. The lenient reader drops a `mail` member when the manifest has no `mail:` block, and logs it; the lint rejects that case.
- The shape is a proposal: see # Open questions.

### 6. Other rules

- **Accessibility.** A tile grid is a radio group: arrow keys move between tiles, and each tile's name is text. On each step, focus moves to the page heading, and the page title changes to match.
- **Words.** The UI says "AI service", not "LLM provider", on these pages and in Settings (Settings → Integrations → AI services).
- **No key check, no early download.** Both are out of scope (`NEXT.md` # Install steps: deferred).

## Work by repo

- **os:**
  - web-ui: the steps and `?step=` routing, the B layout for AI and email, the key form with a default name, the three-level handling, the info box, and the warnings on the first step.
  - brain: `recommends` in the manifest schema and lint, and in the install plan; duplicate info in the install plan, so the first step can warn before any question (today it is only a 409 from `POST /api/v1/apps`); the Gmail rename and the iCloud preset in `internal/mailpreset`; and a way to tell which account an app used most recently.
- **store:** the popularity order in `ai_providers.yml`, a default model for every model type each provider offers (with a lint that fails when one is missing), and `recommends` on the apps that degrade badly without email or AI.
- **cloud:** nothing.

## Suggested order

1. **The steps and the first-step warnings.** (Built first as pages with a summary page at the end; the summary page was removed after the user test, # 2.)
2. **AI pages** (B layout, grid, key form).
3. **Email pages** and the Gmail and iCloud presets. Test a real send from a hosted box for both presets before they ship.
4. **`recommends`,** then tagging the store.
5. **A test with 3 to 5 non-technical people** on the built UI, after steps 1 to 3 are built. There is no separate prototype. Watch in particular whether anyone reads the info box.

## Open questions

1. **The `recommends` shape.** `mail` as a special `one_of` member resolved from the `mail:` block, and the key names `recommends` and `without`, are a proposal.
2. **A one-line description per service tile** ("Makes the Claude models", "One key for many AI services"). It would help a user tell Groq from Fireworks, and it ranks nothing. It needs a `summary` field in the provider data. Not decided.
3. **Who keeps the popularity order, and from what source.** Boxes send no usage data, so today it is a fixed order in `ai_providers.yml`, kept by hand from public market share. The rule should be written down in `APP_STORE.md` # AI provider data when it is decided.
