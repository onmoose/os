# Install steps: the design for one question per page

- **Status:** done
- **Date:** 2026-09-28
- **Specs touched:** docs/specs/INSTALL_STEPS.md (new), docs/specs/DECISIONS.md, docs/specs/DASHBOARD.md, docs/specs/APP_STORE.md, docs/specs/SERVICE_PROVISIONING.md, docs/specs/INSTALL_SETUP.md, docs/specs/NEXT.md, docs/README.md

This is a design entry, with no code. It follows [install-setup-doc-gaps.md](install-setup-doc-gaps.md), which closed the install setup plan (`INSTALL_SETUP.md`).

## What was done

- A UX review of the install setup page (`InstallSetupView.vue`, `AISlotPicker.vue`, `MailAccountSection.vue`) against every manifest in `onmoose/store` (112 apps). About 90 need nothing, 17 take optional email, 2 require AI, and 5 take optional AI. The main problems found: every question shows at once, and the Save inside the AI row is a trap that leaves Install disabled.
- **`INSTALL_STEPS.md`**, the new plan: one question per page, a last page that shows every answer with Change, and repeat installs going straight to that last page. It covers the "saved keys first" layout for AI and email, the service grid in popularity order with no Recommended badge, the key form with a default name, no model choice during the install, three need levels with a proposed `recommends` manifest list, the quiet info box on the first and last pages, early warnings, and error routing.
- **Two `DECISIONS.md` entries:** the install flow, and Gmail and iCloud as the first email presets.
- **A correction in `SERVICE_PROVISIONING.md`:** iCloud was excluded as 465-only, but Apple's settings page gives port 587 with STARTTLS. Gmail was checked too: SMTP with an app password still works, and the 2026 Gmailify and POP change is about fetching mail, not sending it.
- Pointers from `DASHBOARD.md`, `APP_STORE.md` and `INSTALL_SETUP.md`, and a new `NEXT.md` item for the two ideas left out (checking an AI key, and starting the download early).

## How it maps to the specs

It keeps the north star (simplicity for non-technical users) and the "UI owns all wording" rule. It builds on the role and binding model of `INSTALL_SETUP.md` without changing the wire shapes, except the proposed `recommends` list. Declare-and-degrade mail (`APP_MANIFEST.md` # D3) stays: `recommends` makes a need louder, never required.

## Known gaps & deviations

- Nothing is built. `DASHBOARD.md` # Install authorization still describes the as-built page.
- The `recommends` shape, a one-line description per service tile, and the source of the popularity order are open questions in `INSTALL_STEPS.md`.
- The Gmail and iCloud presets are checked against the providers' docs only. A real send from a hosted box is needed before they ship.

## What's next

1. A clickable prototype tested with 3 to 5 non-technical people, above all to see whether anyone reads the info box.
2. Build in the order of `INSTALL_STEPS.md` # Suggested order: the last page and early warnings first, then the AI pages, the email pages with the new presets, and `recommends`.
