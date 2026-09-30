# Widen the "Skip small issues" rule in CLAUDE.md

- **Status:** done
- **Date:** 2026-09-30
- **Specs touched:** none (`CLAUDE.md`, # Working style)

The "Skip small issues" rule covered only filing follow-up issues, and it still allowed a small finding "in one line at most". During the userns-remap close-out a coding agent kept asking the maintainer, reply after reply, to approve a change that only adds a health warning. The rule did not clearly forbid that.

## What was done

The bullet in `CLAUDE.md` # Working style now:

- covers proposals and questions, not only issues;
- names what counts: a big improvement, a breaking change for users, a real bug, or a security risk;
- drops the "one line at most" allowance, so a small item is left out completely;
- forbids repeating a small open question across replies, and says an item the user says to ignore is dropped for good.

The old text had an em dash, which the no-em-dash rule in the same file forbids. The new text has none.

## How it maps to the specs

No spec changes. `CLAUDE.md` is the working-style source for every contributor and coding agent.

## Known gaps & deviations

None.

## What's next

Nothing.
