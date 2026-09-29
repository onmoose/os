# The `root_setup` manifest field and its admission rules, #528

- **Status:** done
- **Date:** 2026-09-29
- **Specs touched:** `APP_MANIFEST.md` # B (status only)

This is slice 3 of #523, after [remap-base-well-known.md](remap-base-well-known.md) (slice 2). It adds the manifest intent field that [userns-remap-spec.md](userns-remap-spec.md) defined for the caps tier, and the install checks that keep it off every app that runs in the host user namespace. It only models and checks the field. No tier acts on it yet: that is slice 4 (#529).

## What was done

- **`internal/manifest`.** `Manifest.RootSetup bool` (`root_setup`, `omitempty`). An optional field with a safe default, so `manifest_version` stays 1. `Parse` is not strict, so an older brain ignores the key, as `APP_MANIFEST.md` # B says.
- **`internal/admission.CheckManifest`** refuses `root_setup: true` together with a `folders` grant, `gpu: true`, any `devices`, or `service_user: true`. Each message names `root_setup` and the grant, and says what to do. The first three share the reason: those grants run the app in the host user namespace, where the five capabilities would be real root's. The last is its own: `service_user` pins a non-root user and `root_setup` removes the pin.
- **`moose manifest check`** now runs `admission.CheckManifest` after the schema lint and before the compose admission. Before this, `check` ran only the compose rules, so a manifest the brain refuses at install (for example `service_user` with `folders`) passed `check`. Catalog CI runs `check`, so it now refuses the same manifests at publish time. `lint` stays schema only, as its doc says.
- **Door-2** needs no change: `manifest.Synthesize` never sets the field, and the Door-2 permissions overlay only parses `permissions`, strictly, so a paste cannot set it either.
- **Tests:** parse (set and default), every refused pair and the accepted cases in `admission` (including that `gpu` or `devices` alone still pass), the same rules through `moose manifest check`, and a lifecycle test that a `root_setup` manifest renders the same override, byte for byte, as one without it, with no `cap_add`.

## How it maps to the specs

- Realizes the manifest half of `APP_MANIFEST.md` # B (`root_setup`): the field and the "Folderless only" rule. `APP_MANIFEST.md` gains a Status line: parsed and checked, not acted on.
- Not yet realized, and left to #529: the caps tier itself (`APP_ISOLATION.md` # User-namespace tiers) and the "Only on a remapped box" refusal.
- `docs/architecture.md` and `docs/dev/authoring-apps-with-an-agent.md` (what `check` enforces) are updated.

## Verification

- `make check`: green.
- Not run in `CI / Cloud image`: nothing on the booted path changes. No image or catalog app sets the field, and the override is unchanged (the lifecycle test above).

## Known gaps & deviations

- **Between this slice and #529, a `root_setup` app installs in the default tier** and fails at start the way it does today. That is the same result an older brain gives, which the spec already accepts. The catalog must not publish a `root_setup` app before #529 lands.
- **`moose manifest check` now also enforces the existing `service_user` rule.** That is a small widening beyond `root_setup`. It is the same function the brain runs at install, so it can only refuse manifests that would fail on a box anyway.

## What's next

1. #529: the brain tiers. It reads `remap_base`, checks `docker info`, maps `root_setup` to the caps tier, and refuses a `root_setup` install without the remap.
2. #530 (after #486) and #531: turn the remap on in both images, and a CI remap boot.
