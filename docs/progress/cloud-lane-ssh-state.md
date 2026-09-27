# Cloud lane: the SSH boot uses the whole-state SSH save

- **Status:** done
- **Date:** 2026-09-27
- **Specs touched:** none (test script and dev docs only)

## What was done

The v0.14.0 release boot proof failed on its `ssh` boot (run 36341323935). The other five boots passed. The box was right and the test was stale. [ssh-draft-save.md](ssh-draft-save.md) (#501) made `PUT /api/v1/me/ssh` take the whole SSH state, keys included, and removed `POST /api/v1/me/ssh/keys` and `DELETE /api/v1/me/ssh/keys/{id}`. `dev/cloud/cloud-assertions.sh` still called the removed routes, and sent `PUT` bodies with no `keys`. That entry's known gaps already said the cloud lane check was still open, and this is what it would have found.

The `ssh` boot now drives the same request the Settings screen sends:

- **Step 3** sends `"keys": []` on purpose. A body with no `keys` is refused as malformed, so the old body would have passed the "no key is refused" check for the wrong reason.
- **Step 4** adds the key and turns SSH on in one save, and reads the key's id out of the answer.
- **Steps 6 and 8** keep that key by its id.
- **Step 7** checks the end-state guard: a save that leaves SSH on with no key is a 422 and the key file stays. The per-delete guard it tested before is gone.
- **Step 9**: the second account adds its key and turns SSH on in one save, which is still the second enable of the boot, so the re-enable regression it guards is still covered.

`docs/dev/hosted-boot-proof.md` and `CLAUDE.md` listed five boots, and the lane runs six, so both now name `ssh`.

## Known gaps & deviations

- The script has no unit test. The proof is the `CI / Cloud image` run on this branch (`publish=false`): run 36342315360, all six boots passed, `ssh` included.

## What's next

- Re-run the boot proof on `dev` for v0.14.0, then open the release PR.
