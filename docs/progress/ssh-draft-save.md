# SSH screen as a draft with Cancel and Save

- **Status:** done, pending the hosted checks listed under Known gaps
- **Date:** 2026-09-22
- **Specs touched:** docs/specs/AUTH.md, docs/specs/SETTINGS.md, docs/specs/LOGGING.md

## What was done

**Closes #494.** Follows [ssh-settings-screen.md](ssh-settings-screen.md), which built Settings → SSH with one request per control. On a hosted box with no keys yet, that left the owner with three controls, two of which could not move, and a Password card explaining a lock they could never use. Rotating your only key was also impossible without a spare: the per-delete guard refused removing the last key while SSH was on, even when a replacement was about to be pasted.

**Backend: one whole-state write.** `PUT /api/v1/me/ssh` now takes `{ enabled, require_password, keys }`, where `keys` is the complete desired set: a kept key is `{ id }`, a new key is `{ public_key, label }` (`internal/api/ssh.go`).

- `keys` is required. An old client that sends only the flag gets a 422, instead of being read as "no keys" and wiping every key.
- The brain parses every new key first (same `parseSSHPublicKey`, so options like `command=` are still dropped), then diffs against the store (`planSSHKeys`) and writes the difference in one transaction (`store.ApplySSHChange`). Removals run before additions, so removing a key and pasting it back in one Save is not a duplicate.
- Guards apply to the end state. The hosted "no key while enabled" refusal stays, now checked on the final key set. The per-delete last-key guard is gone, because a valid end state can no longer trip it. The 10-key cap is also checked on the final set.
- A refused new key (bad paste, private key, duplicate) carries its index in the huma error `location` (`body.keys[N].public_key`), so the screen shows the message under the key at fault. `ApiError` gained an optional `location` field for this (`web-ui/src/api.ts`).
- A kept id the account no longer holds is a 409 ("your SSH keys changed somewhere else"), because the request was built from an old list.
- The host is pushed once, while SSH is on or on the way off. An account that stays off is not pushed, same as before for a key added while off.
- On a host failure, the same store call runs in reverse: old access row back, added keys out, removed keys in with their original `added_at` so the list keeps its order. `ListSSHKeys` now also orders by `rowid`, so keys added in one Save keep the order they were sent in.
- Audit: the three actions stay and all come from the one handler. `ssh.key.add` and `ssh.key.delete` are written once per key changed. `ssh.access.set` is written when the flag or the password choice moved, or when the Save changed nothing at all, so every request leaves a line. A refused Save (unelevated, guard, conflict, host 502, store 500) writes the same records with success false. The plan is worked out before the elevation check so an unelevated Save still names its keys.
- `POST /me/ssh/keys` and `DELETE /me/ssh/keys/{id}` are removed, and `store.DeleteSSHKey` with them. The SSH screen was their only caller. OpenAPI and the TS client are regenerated.

**Frontend: the draft** (`web-ui/src/views/settings/SshSection.vue`, new `web-ui/src/sshDraft.ts`).

- The draft is a set of changes on top of the box's state, not a copy: `enabled` and `requirePassword` are null while they follow the box, plus a list of removed key ids and a list of new keys. A refetch or a change from another tab shows up under the user's changes rather than being overwritten by them. A switch set back to the box's value stops being a change.
- Cancel and Save sit at the bottom and appear only while there is a change. They are hidden, not shown disabled, when the draft is clean. Save is also hidden in the one invalid state (hosted, SSH on, no key left), and the reason shows on the key card. Cancel stays so the user can back out. Both stay up while a save is in flight, so "Saving…" has somewhere to show.
- A removed key stays in the list, greyed and dashed, "Will be removed when you save", with Undo. A new key shows dashed with "Will be added when you save". Pasting back a key marked for removal just undoes the removal. Text left in the add form when Save is pressed is staged first.
- Turning the key method off on the appliance marks every key for removal instead of deleting them one by one behind a confirm. Nothing is lost until Save, and each key has Undo.
- Wording: the top switch is "Enable SSH for my account". The password card is "Password", with the three bodies from the issue (appliance, hosted non-owner, hosted owner). The screen never says "moose password".
- The draft is written to `sessionStorage` on every change while dirty, and removed when it goes clean. The entry is keyed by user id. It is cleared on a successful Save, on Cancel, on Discard, and on a confirmed leave. On load it is restored and fitted to fresh state: removals of keys that are gone are dropped, new keys the box now holds are dropped, and "Unsaved changes restored." shows with a Discard link. If the box's state moved since the draft was written, the line says so.
- A private key never enters the draft. The screen refuses a paste containing `PRIVATE KEY` before staging it, because the draft is written to `sessionStorage`. The add form's unstaged text is not stored either.
- Leaving through the app with unsaved changes asks first (`onBeforeRouteLeave`). Closing or reloading the tab gets the browser's warning, except on the hosted owner's portal confirm redirect: `elevate.ts` now sets a flag (`isLeavingForConfirm()`) before it sends the page away.

**Tests.** `internal/api/ssh_test.go` was rewritten for the new request shape. It covers both profiles, the missing-`keys` refusal, the final-state guard on hosted, rotation in one Save (one host call, one add and one delete record), an appliance Save that enables, adds and removes in one host call with exactly one audit record per change, remove-and-re-add, unknown ids, both duplicate shapes, a private key refused with its location, a full undo on host failure (order and `added_at` kept), per-key failure audits on an unelevated Save, and that the per-key routes are gone. Tests that only exercised the removed routes are deleted on purpose: `TestHostedLastKeyCannotBeRemovedWhileEnabled`, `TestLastKeyGuardIsAudited` and `TestFailedKeyDeleteIsAudited` are replaced by the final-state and unknown-id tests, and `TestHostFailureRestoresTheDeletedKey` by `TestHostFailureUndoesTheWholeSave`. The delete-user SSH tests are unchanged.

## How it maps to the specs

Realizes the flow in `AUTH.md` # Device access, now written as draft-then-save, with the end-state guard in place of the per-delete one. `LOGGING.md` describes the three SSH audit actions as coming from one handler, one record per key. `SETTINGS.md` # panel inventory notes the draft shape of the SSH row.

## Known gaps & deviations

- **Not checked in a browser this session.** `go test ./internal/api/ ./internal/store/` passes, and `vue-tsc --noEmit` is clean. The appliance "Done when" (one Save, one prompt, one request in the network tab; Cancel restores a removed key; rotation accepted) needs a manual pass in `make dev`.
- **Hosted checks are open.** The CI cloud image lane check (non-owner sees the live Password switch and two-locks wording, Save disabled with an empty key set) has not been run. The owner round-trip (paste a first key, Save, portal, back with the key and the restore line, second Save, storage entry gone) needs a real provisioned box and a real portal, as the issue says. It must be checked there before the issue closes.
- **The leave prompt is `window.confirm`.** The dashboard has no confirm dialog component yet. It is plain, but it works and it is the browser's own wording around ours.
- **Label is only read for new keys.** A held key keeps its label. There is no rename, as before.

## What's next

The hosted checks above.
