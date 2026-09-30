# The fake host-agent reports a hand-made remap range, #548

- **Status:** done
- **Date:** 2026-09-30
- **Specs touched:** `BRAIN_HOST_PROTOCOL.md` # User info endpoints (the fake's `remap_base`), `docs/architecture.md`, `docs/dev/running-locally.md`, `docs/dev/authoring-apps-with-an-agent.md` # Which tier

This follows [userns-remap-capability.md](userns-remap-capability.md) (#545), whose authoring guide says `make dev-app` cannot boot a `root_setup` or `image_user` app, and [hide-remap-apps.md](hide-remap-apps.md) (#544). The fake host-agent never sent `remap_base`, so even on a machine where someone set up the remap by hand, Docker reported a remap, the fake reported none, and the brain refused every install as a disagreement. The store's re-test of the apps waiting on the remap needs the dev brain to boot them.

## What was done

- **`usermgr.ReadRemapBase(subUIDPath, subGIDPath)`** (`internal/hostagent/usermgr/remap.go`) is the real agent's `RemapBase` as a plain function; an empty path means `/etc/subuid` or `/etc/subgid`. `LinuxUserManager.RemapBase` now calls it, so there is one copy of the rules. The file has no build tag, so the fake still builds on macOS and Windows.
- **`Agent.DevRemapBase`** (`internal/hostagent/agent.go`): a new optional func the fake branch of `GET /v1/identity/well-known` calls. A range sets `remap_base`, no range leaves the field out, and an error answers 500 with the same code and message as the real agent. Nil (every existing test) keeps the old answer.
- **`cmd/host-agent`** wires it to `usermgr.ReadRemapBase("", "")`.
- The result, by machine:
  - A normal dev machine (no `moose-remap` line): no `remap_base`, exactly as before. With #544 the store keeps marking `root_setup` and `image_user` apps as not installable in `make dev`.
  - A machine with the remap set up by hand: `remap_base` from the lines, Docker agrees, and the brain picks the tiers as on a real box.
  - A bad line: an error, like the real agent, so the brain refuses every install until it is fixed.
- **Docs.** `running-locally.md` has a new # Booting apps that need the remap: the four setup steps the images do, and two warnings (it remaps the whole daemon; the unprivileged dev brain cannot give bind dirs to the range). The prerequisite bullet no longer says the fake never reports a range. The authoring guide's "Which tier" and step 10(c) point there. `BRAIN_HOST_PROTOCOL.md` and `docs/architecture.md` say what the fake sends now.

## How it maps to the specs

- `BRAIN_HOST_PROTOCOL.md` # User info endpoints: the wire shape is unchanged. The fake now follows the same rules as the real agent.

## Tests

- `TestWellKnownIdentity_FakeBranch_DevRemapBase` (`internal/hostagent`): a range is reported, no range leaves the key out of the JSON, and a bad line answers 500 with no detail in the body; the operator identity stays the same. `TestWellKnownIdentity_FakeBranch_OmitsRemapBase` still passes with no `DevRemapBase`.
- `TestReadRemapBase` (`internal/hostagent/usermgr`): the function reads the paths it is given (no files, a good pair, a range in one file only). `TestRemapBase` covers every rule through the method, which now calls the same function.
- The real binary: `cmd/host-agent` built and run on this machine (no `moose-remap` line in `/etc/subuid` or `/etc/subgid`) answered `{"moose_app_uid":1000,"moose_app_gid":1000,"moose_shared_gid":1000}`, with no `remap_base`.
- `make check`: green.

## Known gaps & deviations

- **Not run on a remapped dev machine.** I did not turn the remap on on this machine: it would remap every container on its Docker. The positive case is covered by the unit tests with real files, and the reading code is the one the real agent uses on the booted images.
- **An app with a bind dir needs the brain as root there.** On a remapped daemon each bind dir must go to an id in the range, and the unprivileged dev brain skips that chown (it logs `bind dir chown skipped under unprivileged brain`). The same goes for a managed service's data dir. Such an app then cannot write its data. `running-locally.md` says so. Only an app with no bind dir and no managed service is not affected. Making the dev brain do this without root is a bigger change (it is the same fidelity gap as `catalog-import-gaps.md` # dev-box-nonroot-folderless-identity).
- **The fake's operator identity is unchanged.** `moose_app_uid` is still the operator's uid, as before, which is only used by folder apps; they are in the host tier anyway.

## What's next

1. The store re-tests the apps waiting on `userns-remap` with the dev brain on a remapped machine.
