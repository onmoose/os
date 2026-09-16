# Rename the project from malmo to moose

- **Status:** done
- **Date:** 2026-09-16
- **Specs touched:** every file in `docs/specs/`; `MALMO_NETWORK.md` renamed to `MOOSE_NETWORK.md`; new `DECISIONS.md` entry (2026-09-16)
- **Closes:** #489

The project is now called **moose**. The hosted apex moves from `malmo.network` to `onmoose.network`, the product site from `malmo.com` to `mooseos.com`, and the code from `github.com/malmoos/malmo` to `github.com/onmoose/moose`. `DECISIONS.md` 2026-09-16 records the two calls inside the rename: a clean break with no compatibility layer, and an app-facing contract renamed in lockstep with `onmoose/store`.

## What was done

One mechanical pass over 371 files, about 4,950 mentions, then the file and directory renames, then a hand pass over the seams below. The replacement ran most-specific-first (`malmo.network`, then `malmo.com`, then `malmoos`, then `malmo`/`Malmo`/`MALMO`) so no name was rewritten twice.

**Module and binaries.** `go.mod` and 471 import lines. `cmd/malmo` becomes `cmd/moose`, plus `cmd/moose-storage-verify` and `cmd/moose-network-verify`, with the `Makefile` and `.gitignore` following.

**On-disk names.** `/var/lib/moose`, `/srv/moose` (with `shared/` and the `.canary`), `/etc/moose` (the profile marker, secrets, `metadata-firewall.nft`, `data-drive.enrolled`), `/run/moose` (`agent.sock`, `health/`), `/usr/lib/moose`, and the seed at `/var/lib/moose/seed.json` delivered as the `moose.seed` credential.

**Host accounts.** The `moose-app` user, the `moose-shared` group, the `moose-svc-<uid>` service-account prefix, the `moose` socket group, and the PAM service at `/etc/pam.d/moose` (`moose-test` in the nspawn lane).

**systemd.** `moose-storage-ready.target`, `moose-recovery.target`, `moose-storage-verify.service`, `moose-seed.service` with `moose-seed-materialize.sh`, `moose-grow-root`, `moose-metadata-firewall`, `moose-sshd-keygen`, `moose-tpm-enroll`, `moose-load-images`, `moose-ssh-firewall`, the `10-moose-*.conf` drop-ins, `nftables.d/moose-ssh.conf`, and `repart.d/50-moose-grow-root.conf`.

**Brain runtime.** The store file `moose.db`, the containers `moose-brain`, `moose-ui`, `moose-caddy` and `moose-docker-proxy`, the `moose-ingress` network, the `moose-control-plane` compose project, the `moose-caddy-data` and `moose-caddy-acmedns` volumes, the labels `moose.managed`, `moose.instance_id`, `moose.manifest_id` and the OCI label `moose.protocol.major`, and the Caddy route IDs `moose-app-*`, `moose-dashboard` and `moose-catchall`.

**HTTP surface.** The cookies `moose_session` and `moose_forward_auth`, the `moose.sso_token` value, the headers `X-Moose-User` and `X-Moose-User-Id` with the Caddy strip rules that guard them, and the paths `/_moose/sso` and `/_moose/forward-auth/verify`. The OpenAPI output was regenerated and is fresh.

**Env vars.** About 60 brain and host-agent names move to `MOOSE_*`. The app-facing set moves with them: `MOOSE_APP_URL`, `MOOSE_INSTANCE_ID`, `MOOSE_DATA_DIR`, `MOOSE_SECRET_*`, `MOOSE_SERVICE_*`, `MOOSE_MAIL_*`, `MOOSE_FOLDER_*`.

**Dashboard.** The package name, the page title, the headings on Login, Setup and Recover, the portal URL `https://onmoose.network`, the About link, and the copy in the telemetry step, the store, the app detail, custom install, outgoing email and SSH screens ("moose password").

**Docs.** `README.md`, `CLAUDE.md`, `docs/architecture.md`, `docs/README.md`, all live specs, all of `docs/dev/`, the issue and PR templates, and the workflows. The frozen history keeps the old name: `docs/progress/` entries and the older `DECISIONS.md` entries are untouched, so `MALMO_NETWORK.md` still appears in their prose as the name of a doc that has since been renamed. No markdown link in a frozen entry pointed at that file, so no frozen entry needed a link fix.

## Verification

- `make test-nopam` green, all packages. The PAM package is not built here because `github.com/msteinert/pam/v2` does not compile on this machine, which is true on `dev` as well and unrelated to the rename. CI covers it.
- `gofmt` clean, and `make openapi-check` reports the spec fresh.
- `git grep -i malmo -- ':!docs/progress' ':!docs/specs/DECISIONS.md'` returns nothing.

## What's next

- **The two-repo seams must land together.** `onmoose/store` renames the env vars and the header in every app compose. The private control plane owns the apex and `auth.` host, the assertion `iss`, the SSO landing link, the seed path and credential name, the ghcr image refs in its update-target answer, the image asset name it downloads, and the catalog endpoint host.
- **The cloud image lane is the real proof.** `CI / Cloud image` with `publish=false` boots the renamed paths, units and seed, and runs the control-plane update. A green unit suite does not cover any of that.
- **The org and packages are an owner action.** GitHub redirects a renamed repo; ghcr does not redirect image names, so `ghcr.io/onmoose/brain` and `ghcr.io/onmoose/ui` have to exist before a publish run.
- **Existing boxes are re-provisioned, not upgraded.** Nothing in the tree reads an old path, label or env name, so a box built from an older image cannot update into this brain.

## Known gaps

- Nothing here ran on a booted box. The claims about paths, units, accounts and the seed rest on the unit suite and on reading the diff, until the cloud image lane runs on the branch.
- The appliance medium lane (swtpm and LUKS) is local-only and was not run, so the renamed LUKS-era unit graph and the `moose-tpm-enroll` path are unproven.
- `make dev` was not exercised end to end in this session, so the renamed dev Caddy, the `moose-ingress` network and `moose.local` are unproven in the inner loop.
- The gitignored mkosi staging trees under `dev/cloud/` and `dev/test-qemu/` still hold old-name files on this machine. They are build output and are rebuilt, but a stale local tree can confuse a local image build until it is cleaned.
