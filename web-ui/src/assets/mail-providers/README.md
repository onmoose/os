# Outgoing-email provider logos

Drop a logo in here and it appears in Settings → Integrations → Email. No code change needed.

## Naming

**`<preset id>.<ext>`** — the id comes from `internal/mailpreset/mailpreset.go`:

| file | provider |
|---|---|
| `ses.*` | Amazon SES |
| `sendgrid.*` | SendGrid |
| `mailgun.*` | Mailgun |
| `postmark.*` | Postmark |
| `brevo.*` | Brevo |
| `resend.*` | Resend |
| `smtp2go.*` | SMTP2GO |
| `google_workspace.*` | Google Workspace |

`custom` is deliberately absent — "Custom SMTP server" is not a brand, so it draws a generic server glyph instead.

`.svg`, `.png`, `.jpg`, `.jpeg` and `.webp` all work. Vite picks the file up at build time by globbing this folder (`MailProviderLogo.vue`), so **adding a provider logo is just adding a file**. One file per provider: if both `brevo.svg` and `brevo.png` exist, which one wins is undefined — delete the one you don't want.

A provider with no file here falls back to a lettermark tile with its initials. That is a real fallback, not a bug, so a new preset never ships a broken image.

## What makes a good file

- **Prefer SVG.** It stays sharp at any size and is usually the smallest.
- **Prefer the square icon over the wordmark** where the brand has one. The provider's name is already printed under the logo, so a wordmark says it twice.
- **Any aspect ratio works.** The slot is a fixed-height box with `object-contain`, so a wide wordmark renders wide and short rather than stretched or cropped.
- **Transparent background.** The card behind it is olive, not white, so a white box around the mark will show.
- **Trim the padding.** Many downloads ship generous whitespace, which makes the mark look smaller than its neighbours.
- Around **128px** is plenty for a raster file; the logo renders at roughly 32px.

## Where these came from

Replace any of them with a better file — several are stand-ins:

- `ses.svg`, `sendgrid.svg`, `mailgun.svg`, `postmark.svg`, `smtp2go.svg` — [vectorlogo.zone](https://www.vectorlogo.zone/).
- `resend.svg`, `google_workspace.svg` — Wikimedia Commons. Both are **wordmarks**, not square icons, so they read wider than the rest.
- `brevo.png` — Brevo's own site favicon, upscaled from the largest frame. A vector version would be better.
- `ses.svg` is the **generic AWS mark**, not an SES-specific one; there is no public SES icon in these sources.

These are the providers' trademarks, used to identify the service the admin is choosing. They are bundled rather than hotlinked because a box may have no internet, and the dashboard must not call out to a CDN when it renders.
