# Hosted HTTPS bring-up & the cloud boot-proof — how it works, how to debug

The hosted box gets its `*.<box-id>.onmoose.io` wildcard HTTPS with **no toggle**: it happens on every boot, driven by the brain. This page is the "where we are now" view for that path and its air-gapped test lane — read it before chasing a red cloud boot-proof or a "`:443` doesn't bind" report. **As of the entry that added this doc, the path works and the lane is green**; the notes below are the map for when it isn't.

The design source of truth is `../specs/ENVIRONMENT.md` # Networking & discovery and # Provisioning & first-boot; the lane's shape is `../specs/TESTING.md` # Hosted cloud variant. This doc is the operational companion — the flow, the log lines, and the failure-mode → where-to-look table.

## The happy path (what a healthy boot does)

1. **Seed lands.** `moose-seed.service` materializes `/var/lib/moose/seed.json` (delivered by the provider's cloud-init on a real box, or by an SMBIOS credential in the QEMU lane) *before* host-agent launches the brain. The seed is `{box_id, assertion_verification_key, enrollment, update_target_url}`; `assertion_verification_key` is the portal's Ed25519 public key (base64) the box verifies owner sign-in assertions against, `enrollment` is the per-box acme-dns account `{subdomain, username, password}`, and `update_target_url` is optional and read by host-agent only.
2. **Brain ingests it.** On the first hosted boot `cmd/brain`'s `loadHostedEnvironment` reads the seed, persists the assertion key + enrollment + box-id (box-id last, as the commit marker), and logs **`hosted: provisioning seed ingested`**. On every later boot it loads the *persisted* identity and ignores any re-delivered seed (the identity is frozen in SQLite). An absent/unreadable seed logs `hosted box has no provisioning seed …` and the box stays pre-provisioned (no box-id, no wildcard pass) rather than crashing.
3. **Wildcard TLS is applied.** If `profile == hosted` **and** `enrollment.Complete()`, the brain calls `caddy.EnsureWildcardTLS`, which:
   - PUTs Caddy's `tls` app: the wildcard `*.<box-id>.onmoose.io` in **`certificates.automate`** (the "what to obtain") plus an automation policy pinning it to the acme-dns **DNS-01** issuer (the "how"). Both are required — a policy without an automate entry never places the order (this was the #278/#301 bug; see `../progress/hosted-wildcard-cert-automate.md`).
   - PATCHes the server's `listen` to `[":80", ":443"]`, binding `:443`.
   - Logs **`caddy: wildcard TLS configured`**. This is synchronous config only — Caddy obtains the cert in the background on its own schedule, so a slow/unreachable ACME never blocks startup and `:443` binds regardless of whether a cert exists yet.
4. **Two-path cert model.** The **wildcard** `*.<box-id>` is obtained via acme-dns DNS-01 (challenge at the delegated `_acme-challenge.<box-id>`). The **apex** `<box-id>.onmoose.io` (the dashboard host) is *not* routed through acme-dns — Caddy's default issuer gets it over tls-alpn-01/http-01 once the dashboard route names it. So exactly one name touches acme-dns; there is no order-vs-order race to manage.

Net: a provisioned box logs both milestones, binds `:443`, and serves every `<slug>.<box-id>.onmoose.io` from the one wildcard cert.

## The boot-proof lane

`dev/cloud/run-cloud-tests.sh` (`make test-cloud-qemu`) boots the real production image under QEMU, **air-gapped** (`restrict=on` — the seed arrives over SMBIOS, never the network), and greps a serial-console verdict written by `dev/cloud/cloud-assertions.sh`. Three UEFI scenarios over one persisted overlay, plus three boots that each get their own fresh overlay — the legacy-BIOS smoke boot, and the two that change the box (`access`, `update`):

| Scenario | What it asserts (box-facing) |
|----------|------------------------------|
| `unseeded` | No seed → `GET /_moose/sso` ⇒ 503 (gate armed, closed); `POST /setup` ⇒ 403 (disabled on hosted — the owner bootstraps via SSO, not a secret). |
| `seeded` | Seed with a **complete** enrollment → brain logs `provisioning seed ingested`; a bad token on `/_moose/sso` ⇒ 401 (verifier armed); **`:443` binds** and the brain logs `caddy: wildcard TLS configured`. |
| `frozen` | A *different* seed re-delivered on a later boot is ignored; the box still serves under the original box-id and does **not** re-ingest. |
| `access` | A valid owner assertion (minted by the harness, which holds the test-portal private key) → owner session → an app installed air-gapped, then the per-app forward-auth access modes end-to-end through real Caddy: restricted gates an anonymous request and proxies the owner through, public is reachable with no session, and `moose_forward_auth` never reaches the app upstream in either mode (#308/#335). The same app declares `access.public_paths`, so the boot also proves path-scoped exposure (#415): the declared paths answer anonymously, a forged `X-Moose-User` never reaches the app on either branch, and a bypass table (prefix footgun, traversal, encoded traversal, double slash) stays gated. It then proves the **hosted confirm step** (#469): with the plain owner session a create-user is refused with 403, the dashboard mints a one-time confirm challenge, a second real assertion lands with that challenge on its return path, and the same create-user passes and leaves a real PAM account — and a third assertion with an off-box return path still lands on the box's own front page. Own fresh overlay + box-id. |
| `update` | A **real control-plane update and a real failed-update-then-revert** (#382) — see the section below. Own fresh overlay + box-id; the only scenario that replaces the box's own images. |
| `bios` | The **same image booted under legacy BIOS** (SeaBIOS — QEMU's default firmware, no OVMF pflash) instead of UEFI → the control plane comes up and the SSO gate is armed. Proves the dual-firmware image (systemd-boot UEFI + GRUB BIOS, `ENVIRONMENT.md` # Boot) boots where a UEFI-only image hung on Hetzner CX/Intel (#277). Runs on its own fresh overlay, so it's independent of the provisioning sequence above. |

**Air-gapped means config-apply, not a real cert.** The lane cannot reach acme-dns or Let's Encrypt, so the `seeded` scenario proves the brain *applies* the issuer + binds `:443`, not that a cert was obtained. Real DNS-01 issuance + a live `*.<box-id>` cert are verified on a real provider box on a real network — deliberately outside this lane (the whole "config that looks right but never issues" class is invisible air-gapped; the `certificates.automate` unit test in `internal/caddy/caddy_test.go` is the closest static guard).

## The `update` boot (the control-plane updater proof)

The `update` scenario is the only place the updater meets a real Docker daemon, a real registry, a real brain restart and a real revert. Everything under `POST /api/v1/system/update` — the ledger, the transaction, the trigger — is otherwise proven against a fake Docker. Read `../progress/cloud-update-boot-proof.md` for why it is shaped this way.

What the boot does, in order:

1. **Owner session.** The boot is seeded with a test-portal key and given a signed owner assertion over SMBIOS, like the `access` boot. The update trigger is admin-only, so without this the box cannot be asked to update itself.
2. **A registry inside the guest.** `docker load` of a test-only tarball (`/var/lib/moose/test-images/registry.tar`), then a container on `127.0.0.1:5000`. Docker treats a localhost registry as insecure by default, so no daemon config is involved.
3. **A new pair, published by digest.** `docker commit` derives a new brain and a new UI from the images the box is running, adding one marker label. Both are pushed and then **removed from the local image store**, and their absence is asserted — that is what makes the updater's pull a real fetch instead of a no-op.
4. **The happy path.** `POST /api/v1/system/update` with both refs, then poll the job. Asserted: the job completes with both `*_changed` true and `reverted` false; the brain **container id changed**; the running brain is on the target ref and carries the marker label; `images.json` names the new pair and records the previous one; `compose.yml` pins the new UI ref; `/healthz` answers on the new container's own address; `GET /api/v1/system/version` reports the new UI image.
5. **The revert.** A second update points at a brain that starts, truncates `/var/lib/moose/state/moose.db`, drops a marker file and never serves. Asserted: the job is `failed` with `failure_mode: health` and `reverted: true` and no `revert_error`; the marker file exists (so the bad brain really ran); the brain is back on the previous good ref and the ledger no longer names the failed one; `moose.db` is a valid SQLite file again; and the box answers an authenticated `/api/v1/me` with the **same** session cookie.
6. **The target-driven path** (#401, #408, #443). The same transaction, with nobody typing refs. An in-guest file server on `127.0.0.1:5001` plays the control plane, and the boot's **seed** points the box at it — not a drop-in, because the seed is the only channel a real box has for a per-box fact. Asserted in two halves: a **tagged** answer is refused (the journal says so, the brain's `GET /api/v1/system/update-target` reports `state: refused` and `from: seed`, and the brain container is untouched), then a **pinned** answer is applied on its own (the answer's `window` outranks a one-minute `MOOSE_UPDATE_WINDOW`, the read names the same pinned pair the source served, and the ledger and the running container both move to it). The read is also checked to refuse an unauthenticated caller, and is taken **before** the apply lands — once it does, the state is `current` by definition and proves nothing about what the source served.

Reading a red one:

| Symptom | Most likely cause | Where to look |
|---------|-------------------|---------------|
| `in-guest registry never answered on 127.0.0.1:5000` | The registry tarball is missing from the image, or the container could not start. | Was the image rebuilt after `dev/cloud/test/bootstrap.sh` changed? The canary (`.dev/cloud-boot/.cloud-boot-ready`) gates the rebuild — bump `CANARY_VERSION` when staging changes. |
| `… is still in the local image store before the update` | The post-push `docker rmi` did not remove the image. The scenario refuses to continue, because the pull it is about to prove would not be a real fetch. | Something else references the image — check `docker ps -a` in the serial diag. |
| `the brain container was NEVER recreated (same id …)` | The update reported success without replacing the brain. This is the single most important assertion in the boot. | host-agent's journal lines in the diag block (`system-update`, `control plane`), then `images.json`. |
| job never reaches a terminal state | The box did not come back after the brain was recreated — most likely Caddy cannot reach the new brain, or the brain did not re-install its routes. | The diag block's `docker ps -a` and brain log tail; the job record lives in host-agent, so `journalctl -u host-agent` is the ground truth. |
| job fails with `resolve ui address: docker inspect moose-ui: exit status 1` | **Two `docker compose up` running on the `moose-control-plane` project at once**, which can leave the box with no `moose-ui` container. This is what the first run of this boot found (see `../progress/cloud-update-boot-proof.md`). The fix was to recreate the UI **before** starting the brain, so host-agent's compose runs while no brain exists. | The brain's `control-plane stack up failed; continuing` line in the serial log — a `Conflict. The container name "…_moose-ui" is already in use` there is the signature. Then check the recreate order in `internal/hostagent/cpupdate/update.go`. |
| revert asserted but `moose.db` still broken | `restoreBrainDB` did not run or restored nothing. | The `brain snapshots` listing in the diag block: an empty snapshot dir means the snapshot step, not the restore, is the failure. |

The diag block dumped on failure carries `images.json`, the compose image lines, the snapshot dir listing and host-agent's update log lines, so a red update boot should be diagnosable from the serial log alone.

## When it's red: where to look

The serial log is the primary artifact: `.dev/cloud-boot/last-serial.log` locally, or the "Seeded-boot test" step log in CI. On failure `cloud-assertions.sh` dumps a `=== MOOSE_CLOUD_DIAG ===` block (docker ps, networks, iptables, and **`moose-brain` log tail**). Grep it for the milestone lines above.

| Symptom | Most likely cause | Where to look |
|---------|-------------------|---------------|
| `:443` never binds / connection-refused | `EnsureWildcardTLS` was **skipped** — this is nearly always `boxID == ""` (seed not ingested) or an incomplete enrollment, *not* a bug in the bind/apply path. | Is `provisioning seed ingested` in the brain log? Is `/var/lib/moose/seed.json` present and does it carry an `enrollment` block? If both are present and `:443` still doesn't bind, then it's a genuine apply-path regression — check `caddy: wildcard TLS configured` is absent and read the Caddy admin PUT/PATCH errors. |
| `:443` binds but no cert (real box) | DNS/ACME reachability — acme-dns delegation, the box's resolver, or Let's Encrypt reach. Not reproducible air-gapped. | A real provider box only; read the box's Caddy log (via provider rescue mode — hosted ships no SSH). Not a boot-proof concern. |
| `hosted /setup not disabled: … 503` (unseeded) | **Startup race, not a bug.** A 503 there is Caddy answering "no ready upstream for `/api`" in the first second after the stack comes up. | The assertion now rides through transient 502/503 and only fails on a stuck window (`cloud-assertions.sh`, the `/setup` poll). If it recurs, confirm the brain reached `caddy: dashboard route installed` and `moose-brain listening`. |
| Brain baked old but image labeled new | Stale `.dev/` control-plane cache reused across a build. | Build on a fresh checkout (the CI job does — no `.dev/` cache), which rebuilds the control-plane images from source. |

A **broken image build** presents as several of these at once (e.g. `:443` refused *and* seed-ingest missing) — check the build step succeeded and the control-plane images were rebuilt before diagnosing the box logic.

## How to run it

- **It can also start on its own.** A PR into `dev` or `main` that touches the updater — `internal/hostagent/**`, `cmd/host-agent-real/**`, `internal/lifecycle/controlplane*.go` or `lifecycle.go`, `internal/api/system*.go`, `dev/cloud/**`, or the workflow itself — runs `CI / Cloud image` automatically and boots **only** the `update` scenario (#389). It publishes nothing (the run asserts that before it builds). So a PR in those paths can turn this lane red without anyone asking for it; read the run the same way as a manual one. Expect ~12–15 min, most of it the image build. Draft PRs are skipped — marking the PR ready starts the run. A PR that needs the other boots can still run them by hand with the command below.
- **CI (preferred — no local root/KVM, no image push):** `gh workflow run "CI / Cloud image" --ref <branch> -f publish=false`. Builds the image, then runs the `unseeded seeded bios access update` boots under QEMU. `publish=true` (the default) additionally **attaches the compressed image + checksum to the tagged GitHub Release and pushes the brain + UI images to ghcr** (#352 removed the old provider-snapshot upload, so the lane holds no hosting-provider credential) — only do that deliberately. Runtime ~10 min for the build, plus the boots (the `update` boot is the longest — it runs two full update transactions, one of which spends a 60s health wait failing on purpose).
- **Local:** `sudo make test-cloud-qemu` (needs root + `/dev/kvm`). Scope boots with `MOOSE_CLOUD_BOOTS="seeded"` to reproduce the wildcard path alone, `"bios"` for the legacy-BIOS boot alone, `"update"` for the updater proof alone, or the default `"unseeded seeded frozen bios access update"` for the full run.

## Related history (frozen snapshots — background, not the current view)

- `../progress/hosted-wildcard-cert-automate.md` — the `certificates.automate` fix (the real #278/#301 root cause).
- `../progress/cloud-wildcard-tls-assertion.md` — how the seeded lane came to assert the `:443` bind + apply.
- `../progress/cloud-image-live-onramp-fixes.md` — the two real-box fixes (seed-fetch keep-alive, static resolver) that a green real box depends on.
