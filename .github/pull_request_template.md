<!-- See docs/dev/contributing.md # Step 6 + Definition of done. Fill every section; delete the comments. -->
<!-- Branch should be named <area>/<N>-<short-slug> (e.g. feat/12-health-banners). One PR closes exactly one issue. -->

## What & why

<!-- One or two lines: what this slice does and the behavior change. -->

## Spec(s) touched

<!-- Which docs/specs/*.md this realizes or diverges from. "None" if pure tooling/infra. Add a DECISIONS.md entry if a locked decision flipped. -->

## What was tested

<!-- How, and against what. Be concrete: `make check` green? real Docker? a VM boot (nspawn / QEMU+swtpm)? Unit tests alone are not enough for slices that integrate with a real external system. -->

## Known gaps & deviations

<!-- Be honest. Every "handled" claim must be verifiable in the diff — don't assume symmetry between similar code paths. "None" if truly none. -->

## Platform gaps

<!-- Catalog PRs only. If the app shipped with a feature that moose can't fully support, list each gap here: gap-class tag, severity (degrades / blocks-start), trigger, what breaks for the user, and why moose can't satisfy it. Omit this section (or write "None") for non-catalog PRs. -->

## Definition of done

- [ ] Behavior works in the inner loop (`make dev`), and integration-tested against the real system if it touches one.
- [ ] Tests added at the right layer; `make check` green (frontend changes: `make check-web` too).
- [ ] No test deleted or newly skipped: checked the deletion count on `*_test.go` in the diff, and confirmed any new environment-gated test actually runs in CI (it is not in the job summary's skip list). A green suite cannot tell you this — deleted and skipped tests both report green.
- [ ] Progress entry written (`docs/progress/NNNN-…`); indexes in both READMEs updated; Up-next re-ordered if needed.
- [ ] Catalog change: if this touches the box's catalog wire (`internal/catalog/wire.go`) or the store views, said whether moose's other store surface needs the same change (`docs/specs/APP_STORE.md` # What the box models, and what it drops).
- [ ] Catalog change, **new published field**: a box drops any key it does not model, top-level or per-app alike (#434 removed the index digest that used to make a per-app field reject the whole payload), so publishing ahead of the fleet is safe and the field simply does not appear until boxes model it. Bump the schema version only for a format a box could not project, and bump it *with* the data, never ahead of it.
- [ ] Spec doc updated if behavior realized/diverged; `DECISIONS.md` entry if a locked decision flipped.
- [ ] No section-sign symbol (write `#` instead), no hard-wrapped markdown, `log/slog` only, conventions per `CLAUDE.md`.
- [ ] Branch off `dev`, PR into `dev` with `Closes #<N>` (do not delete this line — GitHub will not auto-close the issue otherwise); any dependent issue un-`blocked`.

<!-- ⚠️ REQUIRED: keep the Closes line below — do not delete it. Without it GitHub will NOT auto-close the issue on merge and it will remain open. Replace <N> with the issue number. -->
Closes #<N>