# The pinned catalog fixture carries the remap intents

- **Status:** done
- **Date:** 2026-09-30
- **Specs touched:** none

This closes the fixture gap that [hide-remap-apps.md](hide-remap-apps.md) (#544) left: "the pinned fixture waits for the publisher's output". The catalog publisher now sends `root_setup` and `image_user` on each browse record, from the app's manifest.

## What was done

- `internal/catalog/testdata/snapshot.json` is the publisher's own output (`make catalog-fixture` on the publishing side), not a hand edit. Compared with the old file, only two things changed: the opaque `version` token, and `"root_setup": true, "image_user": true` on `alpha-notes`, the record that carries every optional field. A real manifest never sets both, because admission refuses the pair. The fixture only pins the shape.
- `TestNoUnmodeledFields` now sees both keys and finds them modelled in `wireApp`. So a rename on either side of the contract now fails here.
- `TestParseFixtureSnapshot` also checks that both fields reach the parsed record.

## How it maps to the specs

`APP_STORE.md` # Apps this box cannot run already says the box reads the two fields from the browse record. This makes that claim tested against the publisher's real output.

## Known gaps & deviations

- The live catalog sends the fields only after the store release that carries the publisher change.

## What's next

Nothing on this side.
