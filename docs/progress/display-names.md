# Display names: people type a name, the box derives the account

- **Status:** done
- **Date:** 2026-09-21
- **Specs touched:** `FIRST_RUN.md`, `USERS_AND_GROUPS.md`, `AUTH.md`, `BRAIN_HOST_PROTOCOL.md`

**Closes #468.** `FIRST_RUN.md` # Identity & display names has said since the first draft that a person types a first name and the box derives a stable Linux account name from it. None of it was built. Both account-creating paths took the account name straight from the request, a third path (the hosted SSO handshake) had a rule of its own, and there was no display name in the database at all, so there was nothing to rename.

## What was done

**One derivation, in one place.** `internal/api/accountname.go` holds the whole of it, and the three paths that create an account all call it: `POST /api/v1/setup`, `POST /api/v1/users`, and `createSSOOwner`. None of them accepts an account name from the caller any more. The old `ssoUsername` is gone.

The derivation lowercases, decomposes, drops combining marks, folds the Latin letters decomposition does not split (`ø`, `ß`, `ł`, `æ`, `đ`, `þ`), keeps `[a-z0-9]`, caps at 32 characters, and gives a digit-initial result a leading `u` because `useradd` refuses an all-numeric name. Then it walks: reserved list, host probe, `1`, `2`, and so on. `José Smith` becomes `josesmith`.

**A display name on the user row.** Additive column with a default, backfilled from `username` so a box that has been running since before this change shows a name rather than a blank. A `UNIQUE ... COLLATE NOCASE` index sits under it, and `CreateUser` and `CreateFirstAdmin` refuse a row with no display name, so a caller that forgets one gets an error on the first insert rather than a conflict on the second.

**Uniqueness is checked in the brain.** `NOCASE` only folds ASCII, so it would let `José` and `JOSÉ` both through. The real comparison folds case and whitespace, not accents, and the index is the backstop for the race between the check and the insert. The refusal names the person already on the box and suggests a fix, the way the spec does.

**A new host op.** `GET /v1/users/{username}/exists`, a boolean probe for one name. host-agent's `set-password` is an upsert, so a derived name landing on an existing account would set a password on it instead of creating one: a person called Plex on a box running Plex would be handed the Plex daemon's account. The static reserved list only holds names somebody thought of; this is what covers the rest.

**A rename route.** `POST /api/v1/users/{id}/name` writes the display name and nothing else. Anyone may rename themselves; an admin renaming somebody else needs the elevation window, like every other admin change to another account in that section. It audits success and failure.

**The dashboard.** The login picker, top bar, account screen and activity feed show the display name. The account name is shown in exactly two places, Settings → SSH (where it is the name you type to `ssh`) and the admin user list (where an admin needs it to help somebody with SSH). The first-run wizard asks for "Your first name" and its known-gap note is gone. The users list gained a rename control.

**Two callers outside the Go tree.** Four shell scripts post to `/setup` or `POST /users` across the dev, medium and cloud lanes; all four send `display_name` now. The request schema is strict, so a caller still sending `username` gets a 422 rather than having it quietly ignored, asserted in a test.

## How it maps to the specs

Realizes `FIRST_RUN.md` # Identity & display names, which was spec-only until now, and the "no username field" promise in # Step 2. `FIRST_RUN.md` gained the as-built derivation, the non-Latin fallback, why step 4 asks the host, and where the slug is shown. `USERS_AND_GROUPS.md` # Identity model gained the rename route and the no-caller-supplied-slug rule. `AUTH.md` # Login screen UX now says the picker payload carries both names and that `/login` still authenticates on the account name. `BRAIN_HOST_PROTOCOL.md` documents the probe and the three properties that should not be relaxed.

Two spec sentences were corrected rather than implemented as written. The derivation removes characters outside `[a-z0-9]` instead of collapsing runs of them, which is what produces `josesmith` and is what makes the `--` and `xn--` reservations true by construction rather than by argument. And `systemd*` is matched as the bare name, not as a prefix: every systemd account Debian creates is hyphenated and so already unreachable, while a prefix match would also reject `systemd1`, `systemd2` and every other name the collision walk could fall back to, leaving a person called Systemd with no name at all. A test found that one.

## What the self-review caught

Three real bugs, all found by the fresh review agent on the opened PR, all fixed on the branch with a test that fails against the code as it was.

- **The migration would have stopped a box from starting.** Usernames are unique case-*sensitively*, and the old `validateUsername` rejected only `--` and an `xn--` prefix, so a box can be holding both `Bob` and `bob`. Backfilling display names from them hands the `NOCASE` index two rows it treats as one, so the index fails to build, `migrate` returns an error, and the brain does not come up after the upgrade. `dedupeDisplayNames` now resolves the clashes first, oldest account keeping its name and the next becoming `Bob 2`. It is idempotent, and only a migrating box can need it.
- **The SSO wedge was still there, one line further along.** Moving the adopt path off name-derivation was right but not sufficient: `newAccount` runs the display-name uniqueness check, and the half-created admin already holds the name the assertion asks for, so the retry answered 409 and never reached `CreateFirstAdmin` or the adopt branch. The check for an existing user now comes first, before anything is derived. The `CreateFirstAdmin` conflict branch stays, because it is the atomic guard for two handshakes racing on a genuinely empty box.
- **The length cap ran before the digit prefix**, so a 32-digit name came back out at 33 characters wearing its new `u`, past the limit the cap exists to satisfy. Capping is last now. `TestAccountNameBaseIsAlwaysUsable` covered the invariant but had no long all-digit input to catch it.

## Known gaps & deviations

- **Non-Latin names get `user`, `user1`.** `FIRST_RUN.md` used to promise `李` → `li`. That needs a full transliteration table, which is a dependency and a lot of surface; the spec now states the fallback instead. The slug is invisible unless somebody uses SSH.
- **The SSO owner's name is the email local part** until the control plane starts signing a `name` claim. `assertion.Claims` reads the field when it is there; adding it to what the portal signs is a change on the other side of that seam, and nothing here can check it.
- **`adoptSSOOwner` no longer looks the owner up by name.** It could not: the derivation walks past names that are taken, so a retry after a partial create would derive a different name than the row it was looking for and wedge the box. It now adopts the box's sole admin, and errors if there is not exactly one. This was forced by the change, not asked for by the issue.
- **The recovery screen still asks for an account name.** `POST /api/v1/recover` takes a username and `RecoverView.vue` has a text field for it. A person who has only ever seen the login picker has never been shown their account name, so they cannot fill it in. Out of scope here and not fixed; it wants the same picker the login screen has.
- **`x/text` is a new direct dependency** (`unicode/norm`, for the decomposition). It was already in the module graph as an indirect dep at v0.19.0. Promoting it moved the indirect `x/mod` v0.16.0 → v0.17.0.
- **Nothing ran on a booted box.** The `useradd` half of the "Done when" list and the hosted owner path both need a real boot; the inner loop against the fake host-agent is as far as this went. The cloud lane's `access` boot exercises the hosted path on the branch.
- **No test covers two admins racing a create** onto the same display name. The unique index is the guard and a store test proves it rejects, but the HTTP-level race is not driven.

## What's next

- Give the recovery screen the login picker, so the account name is never something a person has to know.
- Send `name` in the signed assertion from the control plane, then drop the email-local-part fallback here.
- Decide whether the activity feed should record the name as it was at the time. It resolves at render time today, so history reads under the current name. That is the `NEXT.md` item "Display-name rename UX + audit log story", still open.
