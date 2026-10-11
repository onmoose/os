# SSH screen: turn SSH on, then add a key, then Save

- **Status:** done, pending a manual UX check on hosted
- **Date:** 2026-10-11
- **Specs touched:** docs/specs/AUTH.md (Device access flow, steps 1, 2 and 4)

## What was done

Follows [ssh-draft-save.md](ssh-draft-save.md), which made Settings → SSH a draft with Cancel and Save (#494). That screen kept three behaviours from the one before it. On a hosted box with no key, the top switch was locked until a key was added. The authentication methods showed while SSH was off, so the key card could be reached. And Save was hidden while the draft was invalid. The result was a screen with SSH off, a public key switch on and locked, and only "Add a key" to press. The intended order is the reverse: turn SSH on, add a key, save once.

The screen changes are in `web-ui/src/views/settings/SshSection.vue`. `PUT /me/ssh` did not change: it already takes "on, plus a new key" in one request and checks the guards on the end state. One route was added for the key check below.

- **The top switch always moves.** It is disabled only while a save runs. Turning it on changes only the draft, as before.
- **The methods show only while the switch is on** (`showMethods = enabled`). The two old exceptions (a hosted box with no key, and a draft with key changes) are gone.
- **Turning SSH on, on hosted with no key, opens the add-key form**, because that is the next step.
- **Turning SSH off drops the unsaved method changes**: new keys, removals, the password choice, and the add form. The methods are hidden then, and a hidden change must not be saved. Saved keys stay on the account, so turning SSH on again brings them back. The same rule (`dropHiddenChanges` in `web-ui/src/sshDraft.ts`) runs on the other paths to "off": a refetch after another tab turned SSH off, a draft restored from `sessionStorage`, and, as a last guard, `buildSaveBody`.
- **Save stays in view but disabled while the draft is invalid** (hosted, SSH on, no key), and the existing reason line moved from the key card to beside the buttons, in muted text. Text typed into the add form counts as a key, because Save adds it to the draft first. So the path is: on, paste, Save.
- **The top switch's line follows the saved state, not the draft.** It changes on Save. The "Once you save…" and "SSH turns off … when you save" lines are gone.
- **Copy.** The intro is "SSH gives you a command line on this box from another computer. It is off by default." The methods intro is "There are 2 authentication methods: your account password and / or a public SSH key." The key card has one line for every state and profile: "A key file on your computer that proves it is you when you connect over SSH."
- **One text per line, in every state and profile.** No line on the screen changes with the state any more. The switch line is always "Turn on SSH to access the box externally." (the "You can sign in … as `<username>`" line is gone). The Password card is always "Your account password. When both methods are on, SSH asks for both." (it had three versions: appliance, hosted member, hosted owner). The restore line no longer adds a second sentence when the box changed. The key list keeps its per-row status ("Added <date>", "Will be removed when you save", "Will be added when you save"), and Save keeps "Saving…".
- **A key is checked at "Add key", not at Save.** New route `POST /api/v1/me/ssh/check-key` (`internal/api/ssh.go`, `checkMySSHKey`) runs `parseSSHPublicKey`, the same check Save runs, and returns the key as it will be stored plus its fingerprint. It stores nothing, never calls the host, and is not audited (a pure read). The screen refuses a private key before sending anything, then asks the route; a refusal shows under the add form. The draft keeps the checked key and its fingerprint, so a new key's row shows the fingerprint like a saved one. Duplicates are still caught against the list on screen, now on the normalised key. Save still checks every new key. OpenAPI and the TS client are regenerated.
- **A check that comes back late is dropped.** Each check carries a token, and closing the form, Cancel or turning SSH off bumps it, so a key is never staged into a draft that moved on while it was being checked. An answer is also dropped when the key in the form changed while it was checked. While a check runs, the key and name fields are read-only, "Choose a .pub file" and Save are disabled, and a second Save or "Add key" does nothing. When a refetch shows SSH off, the add form is closed too, so its text cannot keep Save up for a hidden key.
- The locked switches stay as they are: the public key on hosted, the password on the appliance, and the hosted owner's password.

**Tests.** In `internal/api/ssh_test.go`: `TestCheckKeyValidatesWithoutStoring` (a valid key returns its stored form and fingerprint; empty text, junk, bad base64 and a private key are 422; nothing is stored and the host is not called), `TestCheckKeyDropsOptions` (authorized_keys options in a paste are not in the answer) and `TestCheckKeyNeedsASession` (401 without a session). `go test ./internal/api`, `make openapi-check` and `make check-web` pass. The web UI has no unit tests for this screen. The UX check is manual, in `make dev`.

## How it maps to the specs

`AUTH.md` # Device access, flow steps 1 and 4, now say the switch always moves, the methods show only while it is on, turning it off drops unsaved method changes, and Save is disabled (not hidden) with the reason beside it.

## Known gaps & deviations

- In `make dev`, switching a stack from appliance to hosted keeps the account's `require_password = 1`, which the appliance writes, so the hosted Password switch starts on. A real hosted account starts with it off (the column defaults to 0).
- The reason line beside Save reuses the old message ("Keep at least one key while SSH is on…"), which was written for the "removed every key" case.
- A self-review (sonnet agent) found the late-check race and the hidden-change paths above, both fixed here. Greptile then found a file picked during a check and an add form left open after a refetch turned SSH off, both fixed here.

## What's next

- Manual check of three paths in `make dev`: hosted owner, hosted member, appliance.
- #500, generate a key in the browser, plugs into the add-key form this change now opens.
