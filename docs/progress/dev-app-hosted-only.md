# make dev-app can boot a hosted_only app

- **Status:** done
- **Date:** 2026-11-12
- **Specs touched:** `docs/specs/APP_MANIFEST.md`, `docs/dev/running-locally.md`, `docs/dev/authoring-apps-with-an-agent.md`

Follows [terminal-app-flags.md](terminal-app-flags.md) (#660). That change made the brain refuse a `hosted_only` app off the hosted profile. `make dev-app` runs the appliance profile, and the store's curation box and its verify driver both boot apps with it. So no terminal app could be installed there, for the import or for a later version bump. Five terminal-app imports wait on it in `onmoose/store`.

## What was done

- **A brain switch, `MOOSE_DEV_ALLOW_HOSTED_ONLY`.** Off by default. `cmd/brain` reads it and logs a warning when it is on.
- **The API** (`Server.SetAllowHostedOnly`). With the switch, `checkPlacement` does not refuse a `hosted_only` install, and the install plan does not say `unavailable: "hosted-only"`. Both read one helper, `hostedOnlyRefused`.
- **The lifecycle** (`Manager.SetAllowHostedOnly`). The placement check reads `placementProfile()`, which is hosted when the switch is on. That covers the install backstop and the update check. Nothing else reads it: routes, forward-auth and the `owner_only` lock still follow the real profile.
- **`make dev-app`** exports `MOOSE_DEV_ALLOW_HOSTED_ONLY := 1`. Plain `make dev` does not.
- **Tests.** Lifecycle: with the switch, a household install is still `ErrPersonalOnly`, a personal install works, and its route has no forward-auth. An update that adds `hosted_only` is found as available and applied, through `CheckUpdates` and `Update` (each fails without the fix). API: with the switch, the plan offers the app, a household install is still a `409`, and a personal install is a `202`.

## Known gaps & deviations

- **Review.** The review agent found no Block. Its notes: the new `SetAllowHostedOnly` doc sat inside `SetEnvironment`'s comment (moved), and the first update test called `classifyUpdate` directly, so it did not guard the real update paths (now it drives `CheckUpdates` and `Update`). It also asked whether the API and the lifecycle should share one flag. Kept as two, like the profile itself, which both get from `cmd/brain`; the lifecycle refuses again if only the API is set.

- **The dev boot does not test the hosted gate.** `owner_only` means nothing off hosted, so the app's route has no box login in front of it. The cloud `access` boot already proves the WebSocket through forward-auth with `termwho`.
- **Only the placement rule moves**, and only where the switch is set. Nothing on a real box sets it.

## What's next

1. In `onmoose/store`: say in the curation docs that a terminal app boots on the curation box like any other app.
