# Shared confirm dialog

- **Status:** done
- **Date:** 2026-10-12
- **Specs touched:** none (`docs/dev/web-ui.md` only)

This follows [os-update-now.md](os-update-now.md). Its "Update now" confirmation was a native `<dialog>` written straight into `SystemSection.vue`. Other screens will need the same "are you sure?" step, so it is now a shared part.

## What was done

- New `web-ui/src/components/ui/ConfirmDialog.vue`. It takes a `title`, a `confirmLabel` (default "Confirm") and a `cancelLabel` (default "Cancel"), and puts its body in the default slot. The caller opens it with `open()` through a template ref. The confirm button closes the dialog, then emits `confirm`. Cancel and Esc only close it.
- It is still a native `<dialog>` opened with `showModal()`, so the browser blocks the page behind it, moves focus into it and closes it on Esc. The title id comes from Vue's `useId()`, so two dialogs on one page never share an id.
- `SystemSection.vue` uses it. The look, the text and the behavior are the same as before: the same title, the same two body sentences, "Cancel" and "Update now". A click outside the dialog still does nothing, as before.
- `docs/dev/web-ui.md` names the component and says to prefer it over a hand-built `<dialog>` or `window.confirm`.

## What's next

- Nothing else moves to it in this change. Screens that need a yes before an action can use it when they are next touched.

## Known gaps

- No component test: the web UI has no test runner. `make check-web` (typecheck and build) is the check.
