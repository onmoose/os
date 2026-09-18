<div align="center">

# moose

**A home-server OS for people who want to own their data, not become sysadmins.**

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL_v3-blue.svg)](LICENSE)

[What is moose?](#what-is-moose) · [Principles](#principles) · [Two profiles](#two-profiles-appliance-and-hosted) · [Status](#status) · [Architecture](#architecture) · [Quickstart](#quickstart-local-dev-no-vm) · [Documentation](#documentation) · [Contributing](#contributing) · [Contributors](#contributors)

</div>

---

## What is moose

moose is a home-server OS in the same category as **Umbrel / ZimaOS / CasaOS**. Its north star is **simplicity for non-technical users**.

Install it on an old laptop or PC, leave it running in the pantry, and run the apps you use daily (photos, notes, files, a shared grocery list) on hardware you own, with data you own. Apps are Docker containers installed from a catalog or pasted in as a compose file. If the original app developer disappears, your app keeps working, and uninstalling an app never deletes your content.

> See [`docs/specs/SPEC.md`](docs/specs/SPEC.md) for the full vision.

## Principles

- **Files are first-class, apps are windows.** Your content lives in `~/Photos/`, not inside an app's opaque library. Uninstalling an app never deletes your photos.
- **We are not a NAS.** Storage is plumbing in service of apps. No pools, no vdevs, no parity-as-first-class, and no NAS vocabulary in the UI.
- **The UI is the path.** Every privileged operation has a UI path. SSH is rescue-only, never required for daily use, and one moose password (PAM-backed) covers the dashboard, SSH, and SMB.
- **Closed by default (on the appliance).** No public exposure for the home box in v1. Identity-based mesh only, opt-in.
- **One control plane, two environments.** The same brain, dashboard, app model, and auth run whether moose is the box in your pantry or a moose-operated cloud VM. The profile only changes the boring host layer underneath.

## Two profiles: appliance and hosted

moose is built for two environments from one codebase. The split is a build-and-config **profile**, not a fork. The control plane (roughly 95% of moose's logic) is identical across both; only the base image and the privileged host layer diverge. Full design in [`docs/specs/ENVIRONMENT.md`](docs/specs/ENVIRONMENT.md).

| | `appliance` (default) | `hosted` |
|---|---|---|
| **What it is** | bring-your-own x86 box on your LAN | moose-operated cloud VM, one per tenant |
| **Audience** | households and families | small and medium businesses |
| **Reachability** | `<slug>.local` on the LAN; `.onmoose.io` HTTPS opt-in | `<slug>.<box-id>.onmoose.io` public HTTPS, always on |
| **Install** | USB installer, wipe disk | provisioned from a cloud image, cloud-init-style first boot |
| **Storage** | physical OS + data drives, LUKS+TPM, mergerfs | virtual block volume(s), provider/KMS encryption |
| **Network** | NetworkManager (ethernet + WiFi), Avahi/mDNS, Samba/SMB | single virtual NIC, no mDNS, no SMB |
| **Posture** | closed by default | public-by-default, authentication is the gate |

## Status

**A working control plane, with the host layer and the hosted cloud image actively coming online.** This is well past the original walking skeleton.

What runs today (mostly in the native inner loop, see [Quickstart](#quickstart-local-dev-no-vm)):

- **App lifecycle.** Install from the catalog (door 1) or paste a compose file (door 2, admin-only), with an admission policy, image digest pinning, a reconcile pass, health-wait, splash-to-real-upstream routing, and stop / start / uninstall. Per-app logs, a consent-and-permissions flow (folders, devices, GPU), generated app secrets, and per-instance resource limits.
- **Managed databases.** Apps declare `services:` and the brain lazily provisions shared Postgres, MySQL, MariaDB, and Redis/Valkey on an internal network, injecting per-app credentials.
- **App store UI.** Browse grid with search and category filters, app detail pages (screenshots, markdown descriptions), install footprint and storage estimates, and glyph fallbacks for icon-less apps.
- **Users and auth.** First-admin setup, PAM-backed login (the host's `/etc/shadow` is the source of truth), opaque cookie sessions, roles mapped to Linux groups, a 5-minute elevation window for destructive ops, recovery-code redemption, rate-limiting and lockout, and an append-only audit log surfaced as an Activity view.
- **Health and notifications.** A catalog of detectors (service-down, container restart-loop, app-unresponsive, clock-not-synced, RAM pressure, reboot-required, version-mismatch, DB-corrupt) feeding dashboard banners and a notification inbox with per-category mute. Plus live system-resource readouts and disk-usage bars.
- **Real host-agent (`host-agent-real`).** PAM verify, user create / delete / role / password, real `/proc` sampling, disk and RAM reporting, journal streaming, per-LAN-interface Avahi discovery, and first-boot brain launch (Docker socket proxy + brain container). LUKS/TPM enrollment and the boot-chain units exist and are exercised in the QEMU test lane.
- **Hosted cloud profile (coming online).** A slim, build-tagged cloud `host-agent`; a lean `mkosi` cloud image with self-bootstrapping first boot; real Let's Encrypt wildcard certs over ACME DNS-01 for `*.<box-id>.onmoose.io` (the `auth.onmoose.io` acme-dns face is live); an app-container egress block for the cloud metadata endpoint; and a portal-to-box SSO handshake so the owner reaches the box through their existing `mooseos.com` login. The image builds, boots, and provisions on a real cloud provider; a CI lane plus a cloud QEMU lane drive the seed → first-run → served-dashboard arc, with full end-to-end acceptance still being hardened.

What is **not** built yet, so nobody reads this as a finished product: the appliance storage subsystem (`/srv/moose`, mergerfs, and the LUKS unlock at boot beyond the QEMU lane), the production install medium, stream A of updates (the apt and `unattended-upgrades` half), and WiFi/NetworkManager setup in the agent. Stream B is the box updating its own brain and UI. On `hosted` that is built and proven on a booted box. On `appliance` the box can read a signed release manifest, but there is no signing key and no release host yet, so it does nothing on purpose. The authoritative as-built map is [`docs/architecture.md`](docs/architecture.md) (# What is not built yet); per-change history is in [`docs/progress/`](docs/progress/).

## Architecture

A running moose is five processes/artifacts. Three are Go, one is JavaScript, one is a container we don't write. [`docs/architecture.md`](docs/architecture.md) is the live, as-built map.

- **`moose-brain`** (`cmd/brain/`, `internal/`) is the control-plane daemon: one Go binary owning SQLite state, the REST+SSE API, the app lifecycle, and the Caddy config. It drives Docker via the `docker compose` CLI.
- **`host-agent`** is the privileged side. `cmd/host-agent/` is a fake (real wire protocol, in-memory ops) used in the inner loop; `cmd/host-agent-real/` is the real binary, with a build-tagged slim `hosted` variant for the cloud image.
- **`web-ui`** (`web-ui/`) is the Vue 3 + Vite + TanStack Query dashboard. It talks only to the brain.
- **Caddy** (`dev/`) is the reverse proxy. It terminates `*.local` (appliance) or `*.<box-id>.onmoose.io` HTTPS (hosted) and routes to app containers and the brain, configured live via Caddy's admin API. Routing is per-subdomain, never path-based (browser same-origin policy is the reason).
- **SQLite** is the brain's only persistent store (`internal/store/`).

```
browser → web-ui → brain → docker compose (Docker daemon)
                         → Caddy admin API (Caddy → app containers)
                         → host-agent (HTTP/JSON over a UNIX socket)
```

## Repository layout

| Path | What lives here |
|---|---|
| `cmd/` | Go entrypoints: `brain`, `host-agent` (fake), `host-agent-real`, plus small tools (`moose`, `moose-storage-verify`, `moose-network-verify`, `openapi-gen`) |
| `internal/` | brain packages: `api`, `lifecycle`, `store`, `catalog`, `manifest`, `admission`, `caddy`, `hostclient`, `protocol`, `auth`, `audit`, `events`, `profile`, `assertion`, `version`, the health/observability set (`health`, `notify`, `applog`, `systemlive`, `storageverify`), and `internal/hostagent/…` for the host side |
| `api/` | the generated OpenAPI document (`make openapi`); `make check` fails if it is stale |
| `web-ui/` | Vue 3 + Vite dashboard |
| `dist/` | systemd units and drop-ins shipped onto a real box |
| `dev/` | local dev orchestration (Caddy container, config, image trees, test lanes) |
| `docs/` | all documentation (specs, progress, architecture, dev guides) |
| `Makefile` | dev workflow, run `make help` |

## Quickstart (local dev, no VM)

About 90% of development happens in an all-native inner loop: the brain and dashboard run directly on your machine against the local Docker socket, and the host-agent is the fake that speaks the real protocol but stubs host ops. It works on macOS, Windows (WSL2), or Linux with no platform-specific setup.

**Requirements:** Docker + `docker compose`, Node 20+, Go 1.23+, host port `:80` free, and `avahi-daemon` running on Linux so `.local` names resolve. Full guide: [`docs/dev/running-locally.md`](docs/dev/running-locally.md).

```bash
make dev          # the whole inner-loop stack in one terminal:
                  # Caddy (container) + fake host-agent + brain + Vite
```

Then open <http://localhost:5173> and install **Whoami** from the catalog. (The catalog is not in this repo — the brain syncs it from the control plane at run time. To work against a specific store app instead, use `make dev-app APP=<id>` with a `onmoose/store` checkout.) `make dev` also publishes each app's `<slug>.local` name over real Avahi, so installed apps are reachable by their portless `.local` URL from this box and other LAN devices (Android browsers don't resolve `.local`). Ctrl-C stops everything.

Prefer separate terminals? Run the pieces individually:

```bash
make caddy        # dev reverse proxy (container; apps on :80, admin :2019)
make run-agent    # fake host-agent (UNIX socket)
make run-brain    # moose-brain (:8080, native Go)
make ui           # dashboard (Vite, :5173)
```

Before opening a PR:

```bash
make check        # pre-PR gate: gofmt + vet + OpenAPI freshness + full Go test suite
make check-web    # pre-PR gate for web-ui changes: typecheck + production build
```

The host-integrated parts (boot ordering, LUKS/TPM, systemd, the cloud image) are the outer loop and run in a QEMU VM. See [`docs/specs/TESTING.md`](docs/specs/TESTING.md) and `make help` for the test lanes.

## Documentation

| Link | What it covers |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | The as-built map: components, wiring, per-package table, what's not built |
| [`docs/README.md`](docs/README.md) | The map of every spec, one line each |
| [`docs/specs/SPEC.md`](docs/specs/SPEC.md) | Top-level vision and architecture |
| [`docs/specs/ENVIRONMENT.md`](docs/specs/ENVIRONMENT.md) | The appliance / hosted profile split (home for all hosted design) |
| [`docs/specs/`](docs/specs/) | All design specs (source of truth) |
| [`docs/progress/`](docs/progress/) | Per-change implementation history (what's built, what's next) |
| [`docs/dev/`](docs/dev/) | Developer how-to (running locally, code-level architecture) |
| [`docs/dev/contributing.md`](docs/dev/contributing.md) | The contributor workflow, end to end |
| [`CLAUDE.md`](CLAUDE.md) | Working conventions for this repo |

## Contributing

New contributor (or pointing a coding agent at the repo)? Start with [`docs/dev/contributing.md`](docs/dev/contributing.md), the end-to-end loop (orient → pick a task → branch → build → test → document → PR).

- Open implementation tasks live in [GitHub Issues](https://github.com/onmoose/os/issues) (`gh issue list --label P1`).
- All work happens on a branch off latest `dev` and lands via a PR into `dev`. Never commit straight to `dev` or `main`. A PR from `dev` into `main` is how the maintainer cuts a release — contributors don't target `main` directly.
- Link the issue your PR closes with `Closes #<N>`.
- **Every change ships with documentation.** A code change is not complete until its docs are written in the same change.

## License

moose is licensed under the [GNU Affero General Public License v3.0](LICENSE) (AGPL-3.0-only). By contributing, you agree to the [Contributor License Agreement](CLA.md).

## Contributors

<table>
  <tr>
    <td align="center">
      <a href="https://github.com/onel">
        <img src="https://github.com/onel.png" width="100" height="100" alt="onel" style="border-radius:50%"><br>
        <sub><b>onel</b></sub>
      </a>
    </td>
    <td align="center">
      <a href="https://github.com/bogdanpydev">
        <img src="https://github.com/bogdanpydev.png" width="100" height="100" alt="bogdanpydev" style="border-radius:50%"><br>
        <sub><b>bogdanpydev</b></sub>
      </a>
    </td>
  </tr>
</table>
</content>
</invoke>
