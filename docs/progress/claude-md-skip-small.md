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
- forbids repeating a small open question across replies, and says an item the user says to ignore is dropped for good;
- is scoped to suggestions nobody asked for. A design decision the task cannot go on without, and the findings of a requested code review, must still be raised. Both reviewers flagged that the first draft would have banned them too (see # Review).

The old text had an em dash, which the no-em-dash rule in the same file forbids. The new text has none.

## How it maps to the specs

No spec changes. `CLAUDE.md` is the working-style source for every contributor and coding agent.

## Review

- **Fresh Sonnet agent:** one Block, confirmed and fixed. The first draft said to raise a question only for a big improvement, a breaking change, a real bug or a security risk. That would have banned the design questions that `docs/dev/contributing.md` # Step 1 and the next CLAUDE.md bullet require. One Should, also fixed: it would have banned the Notes and Questions a code review must list. The bullet now says it covers only suggestions nobody asked for.
- **Greptile:** the same finding as the Block (P1), fixed the same way.

## Known gaps & deviations

None.

## What's next

Nothing.
