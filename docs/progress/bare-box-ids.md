# The box-id is the name the owner chose

- **Status:** done
- **Date:** 2026-10-06
- **Specs touched:** `docs/specs/MOOSE_NETWORK.md`, `docs/specs/FIRST_RUN.md`, `docs/specs/NEXT.md`, `docs/specs/DECISIONS.md`

The box-id contract changed. It was a typed base plus a system-assigned suffix from a curated word list (`cindy-fox`). It is now the name the owner chose, used as given (`andrei`, so `andrei.onmoose.io` and `<slug>.andrei.onmoose.io`). The service that issues the id allocates it, checks the rules and keeps names unique, and a hosted box gets it in the seed as before. This entry brings the box side's docs, comments and tests in line. The box's behavior does not change.

## What was done

- **`MOOSE_NETWORK.md`.** The section "Locked: box-id is base + curated suffix, joined by a dash" is replaced by "Locked: the box-id is the name the owner chose". It states the rules for a new id (4 to 30 characters, `^[a-z0-9]+(-[a-z0-9]+)*$`, not reserved, not taken), that a taken name is refused with no suffix, reroll or reshuffle, and that the reserved list must cover every name with its own record under the fleet domain, because a bare box-id is a direct sibling of those records. It adds the one-year hold on a destroyed box's name, says older dashed ids stay valid and unchanged, and says who checks what: the issuing service checks, the box treats `box_id` as an opaque label. The "why" keeps what still holds (one DNS label, the reserved list, a name picked once with no rename) and drops the suffix material: the reshuffle, the word-list curation policy, the collision capacity sums, "Dash, not dot" and "No paid drop-the-suffix tier". Enrollment flow steps 2 to 4 and the examples (`photos.andrei.onmoose.io`) follow. The rename section's "first see the suggestion" wording no longer had a suggestion to point at, so it says "first set the box up".
- **`FIRST_RUN.md`** Step 5: the naming field no longer offers a generated suggestion (`cindy-zx9`) or rejects single dictionary words. The typed name is the whole box-id, and a taken, reserved or held name is refused.
- **`NEXT.md`**: the open item "`box-id` allocation scheme" is marked resolved.
- **`DECISIONS.md`** 2026-10-06 records the flip of a locked spec decision.
- **Code comments** that described the shape: `internal/profile/seed.go` (`Seed.BoxID`) and `internal/profile/appurl.go` (`NetworkApex`). No other Go comment stated the shape. Seed parsing is unchanged ("non-empty after trim"), and the box still does not check the id's shape, so it keeps accepting older dashed ids and whatever the issuing service sends.
- **Tests** for a bare id, end to end on the box side:
  - `internal/profile/appurl_test.go`: `TestHostedHostsAndURLs` and `TestCertSubjects` are now table tests over `andrei` and `cindy-fox` (dashboard host, app host, app URL, both cert subjects).
  - `internal/profile/seed_test.go`: a `"box_id":"andrei"` case in `TestReadSeed`, and `TestSeed_BareBoxIDRoundTrip`, which marshals a `Seed`, reads it back with `ReadSeed` and checks the derived hosts.
  - `internal/caddy/caddy_test.go`: a bare-id case in `TestSplitCertSubjects`.
  - `internal/auth/forwardauth_test.go`: `TestIssueForwardAuthBareBoxIDDomain` checks the forward-auth cookie is scoped to `andrei.onmoose.io`.
  - The existing `cindy-fox` fixtures stay. They now cover the older shape.

## How it maps to the specs

- `MOOSE_NETWORK.md` # Locked: pick the name at enrollment, no rename afterward still stands: the box-id is fixed for the life of the box.
- `ENVIRONMENT.md` # Provisioning & first-boot is unchanged: `box_id` is still a required, opaque seed field. `ENVIRONMENT.md` and `APP_STORE.md` held no statement of the old shape, so they needed no edit.

## Known gaps & deviations

- The reserved list itself, the uniqueness check and the one-year hold live with the service that issues ids, not on the box, so nothing in this repo enforces them. That is by design: the box takes the id it is given.

## What's next

Nothing on the box side.
