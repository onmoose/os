# Install steps: one question per page

> **Status: designed 2026-09-28, being built (steps 1 to 3) from 2026-09-29.** This is the plan for the next shape of the install flow. It follows `INSTALL_SETUP.md`, which built the setup page, provider accounts and role-mapped settings. Until this plan is built, the as-built flow is the one in `DASHBOARD.md` # Install authorization. When a part of this plan is built, its spec (`DASHBOARD.md`, `APP_MANIFEST.md`, `APP_STORE.md`, `SERVICE_PROVISIONING.md`) becomes the source of truth for it, and this doc gets an "As built" note, the same way `INSTALL_SETUP.md` did.

## Goal

The install setup page (`/store/:id/install`) shows every question at once: permissions, folder sources, a subfolder box, email, LLM providers, plain settings, size, the duplicate warning and errors. The AI row alone has provider tiles, account cards, an add form, model pickers, and its own Save, Remove and Cancel. For a non-technical user it is hard to tell what they must answer and what is there only to read.

The worst trap is the nested save. "Add account" sits inside the provider editor's "Add" or "Save", which sits inside "Install". A user who adds a key and forgets the editor's Save links nothing. Install stays disabled and says "Still needed".

The new flow asks one question per page, shows the pages only when the app needs them, and sends a user who already has everything set up straight to the last page.

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
| A user whose every need already has a saved answer goes straight to the last page. The last page shows every answer in plain view with a Change link, so nothing is picked without the user seeing it. |
| A saved answer is picked in advance but never hidden. When the user opens a question, its page shows the saved choice selected, and they can pick another or add a new one. |
| Changing the AI key opens a list of the user's saved keys first, each with its service's logo, with "Use a different AI service" at the bottom (the "B" layout below). The service grid shows only when the user has no usable key, or asks for another service. |
| The service grid shows every service the app can use, three columns wide on a computer. No search, no More button, no Recommended badge. |
| Services are in **popularity order**. Moose never marks a service as recommended and never moves one up for its own reasons. If moose offers its own AI service one day, it takes the place the same rule gives it. |
| Models are not asked during the install. The install uses the provider's default model for each model setting. The user changes the model later in the app's settings screen in moose, or inside the app. The one exception is "My own server", which has no model list: its key page has a model name box. |
| One AI service per need. An app that can use several for the same need (openclaw) gets its second one from its settings screen. An app with two separate AI needs (two `requires` groups on different slots, for example chat and embeddings) gets one set of AI pages per need. |
| Needs have three levels: **required** (a page, no Skip), **recommended** (a page with Skip and one sentence on what the user misses), **optional** (a row on the last page with "Set up"). Optional email and AI stay inside the install flow. The user never has to go to Settings to add them. |
| The info box (permissions, size, not-enough-space) is quiet, has no controls, and sits under the app name on the first page and the last page only. The pages in between show one line: icon, name, and "Step 2 of 4". |
| The duplicate-install and not-enough-space warnings show on the first page after the App page, before any question. When the flow goes straight to the last page (no questions, or every answer saved), that first page is the last page, so the warnings show at its top. The last page's info box keeps the not-enough-space line in every case. |
| A saved account is used on its own only for a required or recommended need. An optional need stays "not set up" until the user presses Set up, even when they have a saved account, so no app starts sending email or spending on AI without an explicit act. Set up then opens with the saved account picked. |
| Gmail and iCloud are the first two email presets. The Google Workspace preset becomes "Gmail or Google Workspace", and iCloud is added. The sending services (SES, SendGrid and the rest) follow under their own heading. |
| Out of scope for this plan: checking a key against the provider before saving it, and starting the image download before the user presses Install. Both are in `NEXT.md`. |

## Build rules (2026-09-29)

These rules were set when the build started (steps 1 to 3 of # Suggested order). They fill gaps in the design above. They can change on the build branch as we learn, and this doc changes with the code.

1. **From the install plan to pages.** The install plan sends flat `config` fields and `requires` groups. The UI turns them into pages with this rule:

   | What the plan has | Where it goes |
   |---|---|
   | A `requires` group with `ai` in it | Required AI pages (key list or service grid, then key form) |
   | AI role fields with no group (cap, firecrawl) | Optional "AI service · Set up" row on the last page |
   | `mail` block (always optional in v1) | Optional "Email · Set up" row on the last page |
   | Required plain fields (no role) | One "<App> needs these to run" page |
   | Optional plain fields (no role) | One "Extra settings · Set up" row on the last page |

   The order is: required AI, then required plain fields, then the last page. Until `recommends` exists (step 4), a need is either required or optional. The schema allows two shapes that no store app uses: a mixed group (for example `one_of: [ai, SOME_ENV]`) and a required field with a role. The manifest lint (`internal/manifest/lint.go`) rejects both. The box is lenient, so when it meets one anyway, it shows the fields of that group or slot as plain fields on the "needs these to run" page. A group of plain fields only (`one_of: [A, B]`) goes on that page too, and the page's Continue waits until the group is met.
2. **What counts as already answered.** AI: a saved account that can fill the slot. Email: a saved account, but an optional need stays "not set up" until the user presses Set up (# Decisions). Required plain fields always get a page. A folder always has a default, so it is a row on the last page, never a question.
3. **The key picked in advance** is the newest usable account for now. "The one another app used most recently" needs a brain change and comes later.
4. **Duplicate info in the install plan.** The brain adds the copies of the app the caller can already see to the install plan, so the first page warns before any question. When the plan lists a copy, the user has seen the warning, so the install is sent with `confirm: true`. The last page keeps the 409 handling for a copy that appears later (a second tab, a race).
5. **Session storage.** The draft is kept in the tab's session storage under one key per user, app and scope. It is removed after a successful install or on Cancel. Only non-secret answers are stored: picked account ids, folder choices, and non-secret plain fields. A `secret: true` field and a key that is typed but not saved stay in the page's memory only. The URL carries only the step and the scope, not the answers.
6. **Default models.** A provider with no default model for a type it offers still gets its tile for now. The stricter rule in # 3 (hide such a tile) waits for the store lint that makes every provider carry a default for every type it offers.

## Design

### 1. The pages

```
App page ──Install──▶ [first-time questions] ──▶ Last page ──Install──▶ Progress
                         one per page                ▲
                                                     └── Change on any row opens that page, then comes back here
```

1. **App page.** The Install button, and for an admin on a multi-user box, the split button with "Install for the whole household", as today.
2. **Question pages,** only for a need that has no saved answer. Required needs come first, then recommended ones. The AI and email pages are in # 3 and # 4 below. Required plain fields with no role (cap's Resend key and domain, openmuse's CopilotKit key) share one page, "Cap needs these to run", because they usually come from one outside service and splitting them per page helps no one.
3. **Last page: "Ready to install Openclaw".** The full info box, then one row per answer with a Change link, then the optional rows, then Install.
4. **Progress page,** as today (`/store/:id/install/:jobId`).

An app with no questions goes from the App page straight to the last page.

**Examples**

- **openclaw, user already has an Anthropic key.** App page, then the last page: "AI service: Anthropic, key 'Anthropic key' · Change". Two pages.
- **openclaw, user has no key.** App page, service grid, key form, last page. Four pages.
- **Paperless, email marked recommended, user has no email account.** App page, email service grid (with Skip), email account form, last page.
- **Immich, needs nothing.** App page, last page.

**Routes.** The last page is `/store/:id/install`, as today. A question page adds `?step=<name>` (for example `?step=ai-key`), so Back, browser Back and reload all work. A query parameter keeps the step names clear of the progress route's `:jobId`. The scope (`?scope=household`) stays in the query string on every page. Opening `/store/:id/install` while a required or recommended need has no answer (a first-time user, a bookmark, a reload of a fresh tab) redirects to the first unanswered `?step=`, so the last page is only ever reached with its answers in place. The answers ride in the URL and in the tab's session storage, so a trip to the provider's site in another tab loses nothing. Only non-secret answers do: a picked account id, a folder, a Skip, a non-secret plain field. A field with `secret: true` (a plain key with no role, such as cap's Resend key) and an AI or email key typed but not yet saved never go into the URL or storage. They stay in the page's memory, and a reload asks for them again.

**Step counter.** "Step 2 of 4" counts only the pages this app shows to this user.

### 2. The last page

- **Header:** app icon and name, then the info box. The box is grey, uses smaller text and has no controls. It lists what the app can do, as plain sentences (write access to a folder stays in red, because that is the one line people must notice), and its size ("Takes about 1.2 GB"). It adds the not-enough-space warning when it applies.
- **For** (admin on a multi-user box only): "For: just you · Change", or "For: everyone at home · Change". Together with the line on the AI key page, it tells an admin that their key pays for the whole household. Changing it resets the folder rows to the new scope's defaults, and the page says so ("Folders reset for everyone at home"), because folder sources differ per scope.
- **One row per answered need:** "AI service: Anthropic, key 'Anthropic key' · Change", "Email: Gmail, family@gmail.com · Change".
- **Folders:** "Photos: your Photos · Change". A folder source always has a default, so it is a row here, never its own question page. Change opens the choice (yours or the household's), and "Change subfolder" opens the text box.
- **Optional rows:** "Email: not set up · Set up". Set up opens the same pages as a recommended need, then comes back here.
- **Extra settings:** optional plain fields (postiz, osiris) are one row, "Extra settings · Set up", which opens one page with all of them.
- **Install.** Disabled only when a required answer is missing, which, with the redirect rule in # 1, can happen only after a Change removed one. The line next to it names what is missing.

**Change comes back here.** A Change link opens that one question, and Continue returns to the last page. It does not walk the user through the other steps again (the GOV.UK "Check your answers" pattern).

**Errors.** A 422 from `POST /api/v1/apps` sends the user to the page that owns the field, with the error on that page. It is not shown as red text on the last page. The first page already warned about a duplicate, from duplicate info the install plan will carry (see # Work by repo). A 409 can still come back (a second tab, a race), so the last page keeps today's handling: the warning with "Install my own copy" and Cancel.

### 3. AI pages

**Which services show.** The existing slot rule (`INSTALL_SETUP.md` # 1), made stricter by one condition: a service shows when it can fill one of the app's slots, has a model of every type the slot asks for, **and has a default model for each of those types**. The last part is new, because with no model page the default is the only way to choose (see Models below). When only one service can serve the app (firecrawl: OpenAI; open-seo: OpenRouter), there is no grid. The key page names the service instead: "Firecrawl works with OpenAI."

**Which key should Openclaw use?** (Change on the last page, when the user has usable keys)

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

- Numbered steps, with an "Open Anthropic" button that opens a new tab. The steps come from the provider data's `help` and `key_url`. A plain line says that the service charges for use: "Anthropic charges for use. You need a card on file."
- One password box. The prefix warning stays.
- "Name this key (optional)" is a link that opens a name box. The default name is the service name plus "key" ("Anthropic key"), then "Anthropic key 2" and so on, so a name is never needed to go on.
- Under the box: "Saved on your box. Your other apps can use it too."
- Continue saves the key as the user's account and picks it. If the user then leaves the install, the key stays saved and shows in Settings. That is on purpose, because they can reuse it.
- **My own server:** the address box, the key marked optional, and a model name box when the app's slot has a model setting.
- **Household install:** the page adds "Your key pays for everyone at home who uses Openclaw."

**Models.** The binding is sent with the provider's default model for each model setting, and the brain already fills in defaults (`INSTALL_SETUP.md` # 5). A provider with no default for a type the slot needs cannot fill that slot, so its tile is hidden for that app. The store should give every provider a default for every model type it offers. The model pickers stay on the app's settings screen (`AISlotPicker.vue`), where the user can change the model later.

### 4. Email pages

**Which email should Paperless send from?** (no saved account yet)

- The presets as tiles in two groups. First, the two most common accounts people already have: **Gmail** and **iCloud**. Then, under the heading "Sending services", SES, SendGrid, Mailgun, Postmark, Brevo, Resend and SMTP2GO. "Custom server" comes last.
- Recommended email adds Skip and the app's sentence, for example: "Without email, you can't reset a forgotten password."

**Which email account?** (Change on the last page, when the user has accounts)

- The same B layout as AI: saved accounts with their service's logo, the one used most recently picked in advance, then "Use a different email service". For an optional or recommended need it also has "Don't send email".

**Your Gmail account** (the add form)

- It asks only what the preset needs: the address, and the app password. For SES and Mailgun it also asks the region and the username. "Server settings" appear only for Custom.
- The account name is optional, with a default like the AI key's ("Gmail").
- Gmail and iCloud get numbered steps for making an app password. The first step is turning on 2-Step Verification (Google) or two-factor authentication (Apple), because that is where people get stuck.
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

- A recommended need gets its own page with Skip. The `without` sentence shows on that page and on the last page's row when it was skipped.
- A need that is neither required nor recommended is optional: a row on the last page.
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
  - web-ui: the question pages and the last page, `?step=` routing, the B layout for AI and email, the key form with a default name, the three-level handling, the info box, and moving the warnings to the first page.
  - brain: `recommends` in the manifest schema and lint, and in the install plan; duplicate info in the install plan, so the first page can warn before any question (today it is only a 409 from `POST /api/v1/apps`); the Gmail rename and the iCloud preset in `internal/mailpreset`; and a way to tell which account an app used most recently.
- **store:** the popularity order in `ai_providers.yml`, a default model for every model type each provider offers (with a lint that fails when one is missing), and `recommends` on the apps that degrade badly without email or AI.
- **cloud:** nothing.

## Suggested order

1. **The last page and the first-page warnings,** with today's pickers moved behind Change. This is the biggest win on its own.
2. **AI pages** (B layout, grid, key form).
3. **Email pages** and the Gmail and iCloud presets. Test a real send from a hosted box for both presets before they ship.
4. **`recommends`,** then tagging the store.
5. **A test with 3 to 5 non-technical people** on the built UI, after steps 1 to 3 are built. There is no separate prototype. Watch in particular whether anyone reads the info box.

## Open questions

1. **The `recommends` shape.** `mail` as a special `one_of` member resolved from the `mail:` block, and the key names `recommends` and `without`, are a proposal.
2. **A one-line description per service tile** ("Makes the Claude models", "One key for many AI services"). It would help a user tell Groq from Fireworks, and it ranks nothing. It needs a `summary` field in the provider data. Not decided.
3. **Who keeps the popularity order, and from what source.** Boxes send no usage data, so today it is a fixed order in `ai_providers.yml`, kept by hand from public market share. The rule should be written down in `APP_STORE.md` # AI provider data when it is decided.
