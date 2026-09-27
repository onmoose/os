# Install setup page and progress page

- **Status:** done, not yet tried in a browser (see Known gaps)
- **Date:** 2026-09-25
- **Specs touched:** docs/specs/DASHBOARD.md, docs/specs/SERVICE_PROVISIONING.md, docs/specs/BRAIN_UI_PROTOCOL.md

## What was done

Step 1 of the install setup plan (`INSTALL_SETUP.md` # Suggested order). Installing a catalog app used to open a modal (`InstallDialog.vue`) that listed the app's raw `config:` fields, one text box per env var, and showed progress only as a label on the Install button. A reload lost the install. This change replaces the modal with two routed pages. It is **UI only**: no Go change, no API change. It uses the endpoints that already exist: `GET /api/v1/catalog/:id/install-plan`, `POST /api/v1/apps`, `GET /api/v1/jobs/:id`, and `POST /api/v1/mail-providers`.

**Setup page, `/store/:id/install`** (`web-ui/src/views/InstallSetupView.vue`). The Install button on the app's detail page goes here, and the split button's household item adds `?scope=household`. The layout is the Tailwind Plus left-aligned description list: one row per section, label on the left, content on the right, stacked on a phone. The rows are Permissions, Folders, Email, AI providers, Settings, and Storage, each shown only when the app needs it. Everything the modal did is kept: the permission list with folder write access in red, folder sources and the subfolder prompt, the storage estimate and its not-enough-space warning, the required-field gate on Install, a 422 shown inline above the button, the 409 duplicate warning with "Install my own copy" (a retry with `confirm: true`), and `HealthGated` around the Install button. The form is seeded once per app from the first plan that arrives, so a later refetch of the plan does not wipe what the user typed.

**Email row** (`components/install/MailAccountSection.vue`). The accounts show as cards with the provider logo, plus None, which stays the default and always valid. An admin gets an "Add an account" card that opens the provider picker and the form inline, using `mailProviderForm.ts` (now its third consumer). The save goes through `withElevation`, and a dismissed prompt is a quiet no-op through the shared `errorMessage`. After a save the page refetches the install plan and picks the new account. `useMailPresets` gained an optional `enabled` ref, because `/mail-presets` is admin-only and a member's setup page must not call it.

**AI providers row** (`components/install/AIProviderSection.vue`, `web-ui/src/aiProviders.ts`). Provider tiles, five first and the rest behind More. Picking a tile opens an editor for the key, and for a model or a server address where the app has a field for it. Saving writes the values into the app's existing `config` fields. An app can take several providers, one per set of fields. The row shows only when the app has at least one field the lookup recognises; the recognised fields leave the Settings row.

**The provider list and the env-name lookup are temporary.** Both sit in `aiProviders.ts` and are marked so, to be replaced by manifest roles and published provider data in plan step 4. The lookup works like this:

- Exact names map to a provider and a part: `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `OPENAI_BASE_URL`, `OPENAI_MODEL`, `GEMINI_API_KEY`, `OPENROUTER_API_KEY`, `OPENROUTER_MODEL`, `GROQ_API_KEY`, `MISTRAL_API_KEY`, `DEEPSEEK_API_KEY`, `XAI_API_KEY`. Fields of one provider form a slot, which counts only if it has a key field.
- `<PREFIX>_CUSTOM_BASE_URL` / `_MODEL` / `_API_KEY` form a compatible slot, which counts only if it has the base URL field.
- `OPENAI_API_KEY` next to `OPENAI_BASE_URL` is also a compatible slot.
- A provider goes to its own slot if the app has one, else to a compatible slot (a custom triple first, so picking Groq never takes over the app's OpenAI key), else its tile is hidden. When a provider fills a slot that is not its own, the base URL field gets the provider's endpoint, or the address the user typed for Ollama and "Other".
- A compatible slot holds one provider. A second pick replaces the first, and the editor says so before the save.
- A custom triple's model is required, as openclaw's and hermes-agent's descriptions say. A native model field is optional, and "App default" leaves it blank.

Checked against the store's real field lists with a throwaway script: openclaw and hermes-agent get their four native slots plus the custom triple, and every other provider lands in the triple. cap gets Anthropic, OpenAI and Groq. open-seo gets OpenRouter with its model. firecrawl gets OpenAI only. osiris's `GEMINI_API_KEY_1` is not recognised and stays a plain field.

**Settings row** (`components/install/ConfigFieldInput.vue`). Every field the AI row did not claim, rendered as before (text, secret, enum, bool) with the Tailwind Plus input with label and help text, the native select, and the toggle with label and description. The "Sets `APP_ENV`" hint stays. The Install gate checks every required field in the final answer, whether it came from the Settings row or an AI choice, and the footer lists what is still needed.

**Progress page, `/store/:id/install/:jobId`** (`web-ui/src/views/InstallProgressView.vue`). A 202 from `POST /apps` replaces the setup URL with this one, so Back skips the finished form. It polls the job with `useQuery` and `refetchInterval` and shows four phases as the Tailwind Plus "circles with text" step list: Preparing, Downloading (`resolving_digests`, which pulls the images), Setting up, Starting. The phases follow the order the brain runs its steps, never go back, and an unknown step keeps the last known phase. The old button labels mapped `compose_up` to Downloading and `resolving_digests` to Downloading while the steps between them read Preparing, so the label went back and forth; the new mapping does not. The page ends with Open (the new instance's URL from `["apps"]`, found by the job's `result.instance_id`) and Go to Home, or with the error, Try again (the setup page), and Back to the app. A reload resumes the job. A 404 on the job, which is what the brain answers after a restart because jobs live in memory, stops polling and says the install is no longer tracked.

**Detail page and `useInstall.ts`.** The detail page's button now only navigates. It still reads "Installing…" while the caller's instance is in the `installing` state, which the brain sets at the start of the job, so it holds across a reload with no local state. `useInstall.ts` is reshaped into `useAppInstances` (which copies the caller has, and whether the household item is offered) and `useInstallSubmit` (the POST and its two error branches). The per-step labels on the button are gone; the progress page owns the phases. `InstallDialog.vue` is removed.

## How it maps to the specs

Realizes `INSTALL_SETUP.md` # 6 and step 1 of # Suggested order, with the layout the plan's 2026-09-25 decision names (description list, horizontal link cards, divider with a More button). `DASHBOARD.md` # Install authorization now describes the setup page, the Email row with inline add, the AI providers row, and the progress page in place of the consent dialog. `SERVICE_PROVISIONING.md` # BYO outgoing mail and `BRAIN_UI_PROTOCOL.md` # install-plan are updated where they named the dialog. `docs/dev/web-ui.md` has the new routes, components and the temporary module. `docs/architecture.md` did not name the modal and needed no change.

## Known gaps & deviations

- **Not tried in a browser.** `vue-tsc --noEmit` passes, and the AI lookup was run against the store's field lists. Nothing was clicked through: not the pages at phone width, not an install end to end, not the inline email add with the elevation prompt, not a reload on the progress page.
- **A bare `MODEL` is not recognised.** openmuse's `MODEL` wants `provider/model` (for example `anthropic/claude-sonnet-5`), and a name alone cannot say that format. It stays a plain required field in Settings, so an openmuse user picks a provider tile and still types `MODEL` by hand. openmuse's `GOOGLE_API_KEY` is also left plain, because the name is used for non-AI Google keys elsewhere; for openmuse a Gemini tile therefore lands in its OpenAI-compatible slot, which is not what that app expects. Roles fix both.
- **The provider data is a guess.** Base URLs, key links and the model lists in `aiProviders.ts` are hand-written and were not checked against each provider. Models can always be typed, so a stale list does not block anyone. No provider logos ship; tiles draw a Lucide icon.
- **Inline email add and the hosted owner.** For the hosted box owner, elevation is a full-page trip to the portal. The setup form is not kept across it, so after the trip the user fills in the form again and presses Add again. Keeping the form in `sessionStorage` like the SSH screen would mean storing API keys, so it was not done.
- **Try again after a failed install goes to a personal install.** The job does not say which scope it had, so a failed household install needs the household item again.
- **Adding an account is still admin-only.** The plan lifts that in step 2; members see no add card.
- **A typed model id goes through More.** The search box behind More is where a model that is not in the list is typed, so the model picker always shows More, even for a short list.

## What's next

- Try both pages in a browser, at phone width, with openclaw (AI row with the custom triple), cap (native slots and plain fields) and a mail-capable app (inline add).
- Plan step 2: inline email accounts with owners and the lifted admin rule.
- Plan steps 3 and 4: provider data from the catalog service, then manifest roles, which replace `aiProviders.ts`.
