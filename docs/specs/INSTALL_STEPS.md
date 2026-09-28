# Install steps: one question per page

> **Status: designed 2026-09-28, not built.** This is the plan for the next shape of the install flow. It follows `INSTALL_SETUP.md`, which built the setup page, provider accounts and role-mapped settings. Until this plan is built, the as-built flow is the one in `DASHBOARD.md` # Install authorization. When a part of this plan is built, its spec (`DASHBOARD.md`, `APP_MANIFEST.md`, `APP_STORE.md`, `SERVICE_PROVISIONING.md`) becomes the source of truth for it, and this doc gets an "As built" note, the same way `INSTALL_SETUP.md` did.

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
| One AI service per install. An app that can use several (openclaw) gets its second one from its settings screen. |
| Needs have three levels: **required** (a page, no Skip), **recommended** (a page with Skip and one sentence on what the user misses), **optional** (a row on the last page with "Set up"). Optional email and AI stay inside the install flow. The user never has to go to Settings to add them. |
| The info box (permissions, size, not-enough-space) is quiet, has no controls, and sits under the app name on the first page and the last page only. The pages in between show one line: icon, name, and "Step 2 of 4". |
| The duplicate-install and not-enough-space warnings show on the first page, before any question, not on the last. |
| Gmail and iCloud are the first two email presets. The Google Workspace preset becomes "Gmail or Google Workspace", and iCloud is added. The sending services (SES, SendGrid and the rest) follow under their own heading. |
| Out of scope for this plan: checking a key against the provider before saving it, and starting the image download before the user presses Install. Both are in `NEXT.md`. |

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

**Routes.** The last page is `/store/:id/install`, as today. A question page adds `?step=<name>` (for example `?step=ai-key`), so Back, browser Back and reload all work. A query parameter keeps the step names clear of the progress route's `:jobId`. The answers ride in the URL and in the tab's session storage, so a trip to the provider's site in another tab loses nothing. A key typed but not yet saved is never written to storage.

**Step counter.** "Step 2 of 4" counts only the pages this app shows to this user.

### 2. The last page

- **Header:** app icon and name, then the info box. The box is grey, uses smaller text and has no controls. It lists what the app can do, as plain sentences (write access to a folder stays in red, because that is the one line people must notice), and its size ("Takes about 1.2 GB"). It adds the not-enough-space warning when it applies.
- **For** (admin on a multi-user box only): "For: just you · Change", or "For: everyone at home · Change". This is the only place that tells an admin that their key pays for the whole household.
- **One row per answered need:** "AI service: Anthropic, key 'Anthropic key' · Change", "Email: Gmail, family@gmail.com · Change".
- **Folders:** "Photos: your Photos · Change". A folder source always has a default, so it is a row here, never its own question page. Change opens the choice (yours or the household's), and "Change subfolder" opens the text box.
- **Optional rows:** "Email: not set up · Set up". Set up opens the same pages as a recommended need, then comes back here.
- **Extra settings:** optional plain fields (postiz, osiris) are one row, "Extra settings · Set up", which opens one page with all of them.
- **Install.** Disabled only when a required answer is missing, which can happen only after a Change removed one. The line next to it names what is missing.

**Change comes back here.** A Change link opens that one question, and Continue returns to the last page. It does not walk the user through the other steps again (the GOV.UK "Check your answers" pattern).

**Errors.** A 422 from `POST /api/v1/apps` sends the user to the page that owns the field, with the error on that page. It is not shown as red text on the last page. A 409 duplicate never reaches this point, because the first page already warned.

### 3. AI pages

**Which services show.** The existing slot rule decides it (`INSTALL_SETUP.md` # 1): a service shows when it can fill one of the app's slots and has a model of every type the slot asks for. When only one service can serve the app (firecrawl: OpenAI; open-seo: OpenRouter), there is no grid. The key page names the service instead: "Firecrawl works with OpenAI."

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

- **Gmail or Google Workspace:** `smtp.gmail.com`, 587, username is the full address, the credential is a 16-character app password. It is the existing `google_workspace` preset renamed, with help text that covers personal Gmail too. Password-only sign-in for apps ended in 2025, and app passwords still work for SMTP. Google pushes OAuth instead and could remove app passwords later, and a Workspace admin can turn them off.
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
- The shape is a proposal: see # Open questions.

### 6. Other rules

- **Accessibility.** A tile grid is a radio group: arrow keys move between tiles, and each tile's name is text. On each step, focus moves to the page heading, and the page title changes to match.
- **Words.** The UI says "AI service", not "LLM provider", on these pages and in Settings (Settings → Integrations → AI services).
- **No key check, no early download.** Both are out of scope (`NEXT.md` # Install steps: deferred).

## Work by repo

- **os:**
  - web-ui: the question pages and the last page, `?step=` routing, the B layout for AI and email, the key form with a default name, the three-level handling, the info box, and moving the warnings to the first page.
  - brain: `recommends` in the manifest schema and lint, and in the install plan; the Gmail rename and the iCloud preset in `internal/mailpreset`; and a way to tell which account an app used most recently.
- **store:** the popularity order in `ai_providers.yml`, a default model for every model type each provider offers, and `recommends` on the apps that degrade badly without email or AI.
- **cloud:** nothing.

## Suggested order

1. **The last page and the first-page warnings,** with today's pickers moved behind Change. This is the biggest win on its own.
2. **AI pages** (B layout, grid, key form).
3. **Email pages** and the Gmail and iCloud presets. Test a real send from a hosted box for both presets before they ship.
4. **`recommends`,** then tagging the store.
5. **A test with 3 to 5 non-technical people** on a clickable prototype, before step 1 is built if possible. Watch in particular whether anyone reads the info box.

## Open questions

1. **The `recommends` shape.** `mail` as a `one_of` member, and the key names `recommends` and `without`, are a proposal.
2. **A one-line description per service tile** ("Makes the Claude models", "One key for many AI services"). It would help a user tell Groq from Fireworks, and it ranks nothing. It needs a `summary` field in the provider data. Not decided.
3. **Who keeps the popularity order, and from what source.** Boxes send no usage data, so today it is a fixed order in `ai_providers.yml`, kept by hand from public market share. The rule should be written down in `APP_STORE.md` # AI provider data when it is decided.
