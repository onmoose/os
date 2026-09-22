# Installed apps: card with logo, description, status and details per row

- **Status:** done
- **Date:** 2026-09-22
- **Specs touched:** docs/specs/DASHBOARD.md

## What was done

**Closes #487.** Settings → Installed apps used to be a plain list of bordered rows showing only the app name, owner, and the raw lowercase state string (`running`, `stopped`, …). Rebuilt on the Tailwind Plus "In card with links" stacked list (`web-ui/src/views/settings/InstalledAppsSection.vue`):

- One card (`ul` with dividers, rounded from `sm:` up), one `li` per app, the whole row clickable to `/settings/apps/<id>` via the absolute-span trick.
- **Left:** the app logo in a rounded square (`icon_url`, falling back to `AppGlyph` with `icon_glyph`, same as `AppTile`), the name, and the short description truncated to one line — nothing shown when there is none.
- **Right, first line:** a colored status dot plus a plain word — green "Running", gray "Stopped", amber "Failed", amber with the state written out for anything else. A running app with an open `container-restart-loop` or `app-unresponsive` health issue against its instance shows amber "Needs attention" instead of "Running", read from the health issues the UI already loads (`useHealth.ts`, `["health/issues"]`) — no new request.
- **Right, second line:** small muted text, " · "-joined, skipping parts that don't apply: the owner label in multi-user mode ("Shared" or the username), the version as `v<version>`, and the access mode on hosted boxes only ("Only me" / "Partly public" / "Public" — same three-way rule as `AppTile`'s globe badge and the detail page's Access control).
- Mobile: the status line stays visible below `sm:`; the second line hides.
- Uses malmo's tokens (`bg-card`, `border-border`/`divide-border`, `hover:bg-muted`, `text-foreground`, `text-muted-foreground`) instead of the component's grays, so dark mode works through the tokens with no `dark:` classes.

**Brain side:** `InstanceDTO` gained `short_description` (`internal/api/api.go`), filled from the same catalog entry lookup that already sets `icon_url`/`icon_glyph` in `toDTO`. A Door-2 custom app has no catalog entry, so the field stays empty and is omitted (`omitempty`). This avoids a per-row `/catalog/<id>` fetch just to get the tagline. OpenAPI regenerated (`make openapi` + `npm run gen:api`).

## How it maps to the specs

Realizes `DASHBOARD.md` # Settings' description of what an Installed-apps row shows (added in this change — the section previously only described the detail page a row opens, not the row itself) and the state vocabulary in `DASHBOARD.md` # Tile (never the raw state string).

## Known gaps & deviations

- **No live browser smoke test this session.** `go test ./internal/api/...` passes and `vue-tsc --noEmit` type-checks clean, but no browser-automation tool was available in this session to click through light/dark mode and phone width. The component logic mirrors `AppTile.vue`'s already-shipped status/access rules closely, but this should still get a manual look in `make dev` before merge.
- `TestToDTO_ShortDescriptionFromCatalogEntry` covers the DTO fill and the Door-2 empty case; no new test was added for the health-issue "Needs attention" mapping in the Vue component itself, since the project has no component-test harness for `web-ui` yet (existing settings views have none either).

## What's next

Nothing follow-up identified.
