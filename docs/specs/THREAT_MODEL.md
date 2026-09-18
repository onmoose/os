# moose Threat Model

> The security lens for the whole spec. `AUTH.md`, `APP_ISOLATION.md`, `STORAGE.md`, `MOOSE_NETWORK.md`, and `USERS_AND_GROUPS.md` each defend against an attacker; this doc writes down *who that attacker is*, *what we protect*, and — most importantly — *what we deliberately don't defend against*. It is a checking framework and a place to point when arguing edge cases.

> **Environment profiles.** This doc's posture is the `appliance` one: the user owns the hardware, and the box is closed-by-default. The **hosted** profile (cloud VM) differs on two load-bearing points — moose-operated infra is inside the trust boundary (an honest convenience tier, not operator-blind), and the exposure posture inverts to public-by-default / auth-gated. See `ENVIRONMENT.md` # Threat model (hosted) and # Public-by-default, auth-gated.

## Stance — this doc owns no mitigations

Every "how we defend X" lives in the doc that owns X. This document is an **index of what we defend and what we don't**; the other docs are the *how*. If reading this makes us want to change a defense, that's the threat model doing its job — but the change lands in the owning doc, and an entry goes in `DECISIONS.md`. The threat model documents; it does not decide.

**Scope: v1 only — closed-by-default, LAN-first.** The deferred mesh / remote-access surface (`MOOSE_NETWORK.md` # Deferred) changes the internet-facing boundary materially; it is **out of scope here** and gets its own pass when it ships (see # When this model changes). Everything below assumes the v1 posture: no app or service publicly exposed, cloud is DNS + certs only.

## The trust model (read this first)

moose is a **household appliance**, and almost every security decision follows from one assumption:

> **Members of a household broadly trust each other. moose is not hardening one member against another at kernel grade. The real adversaries are (1) the network — LAN and internet, (2) a compromised app, and (3) physical possession of a drive that left the building.**

This single paragraph justifies a large fraction of the spec, and naming it stops reviewers from relitigating decisions against the wrong adversary:

- **No cross-app SSO and no member-vs-member kernel sandbox** because members aren't the threat (`AUTH.md` # No SSO; `APP_ISOLATION.md` # Multi-user runtime).
- **Admin/root can read every user's files in v1** — accepted, because the admin is a trusted household member, not an adversary; per-user admin-resistant encryption (fscrypt) is a roadmap upgrade, not a v1 gap (`STORAGE.md` # Future: per-user encryption).
- **Demotion doesn't kill a live `sudo` session**, the **login screen lists who lives here**, **temporary passwords travel verbally** — all fine under household trust (`USERS_AND_GROUPS.md` # Known gaps, `AUTH.md` # Login screen UX).
- **Catalog apps are filtered by curation, not by sandbox-proof isolation** — we trust the review process to keep host-rooting apps out, and isolate to bound the blast radius of an *honest* app that gets compromised, not to contain a deliberately malicious author (`APP_ISOLATION.md` # Trust tiers).

Where the trust model does **not** extend: the network (treated as hostile), app *runtime* (assume any app can be compromised), and a drive that physically leaves (assume it falls into hostile hands).

## Assets — what we protect

| Asset | Where it lives | Primary threat |
|---|---|---|
| User content (Photos, Documents, …) | `/home/<user>/` (`STORAGE.md`) | Cross-user read; drive theft; ransomware-via-app |
| PAM credentials | `/etc/shadow` (`AUTH.md`) | Credential theft, offline cracking |
| App state + per-user managed-service DBs | `/var/lib/moose/state/instances/<id>/` (`APP_ISOLATION.md`) | Cross-app / cross-user access |
| Brain SQLite (accounts, sessions, audit log) | `/var/lib/moose/state/moose.db` (+ `-wal`) | Tamper (esp. audit log), session theft |
| LUKS keys / TPM seal | OS drive + TPM (`STORAGE.md`) | Whole-box theft, Secure-Boot subversion |
| Box network position | nftables, closed-by-default (`MOOSE_NETWORK.md`) | Remote exploitation, lateral movement |
| Privacy metadata (who-runs-what, audit trail, cloud DNS queries) | brain SQLite + cloud (`LOGGING.md`, `MOOSE_NETWORK.md`) | Disclosure / correlation |

## Actors / principals

| Principal | Capability assumption |
|---|---|
| **Anonymous LAN device** | On the same L2 network; can reach any LAN-exposed port; can attempt mDNS spoofing, ARP tricks. Present and untrusted. |
| **Authenticated member** | Valid moose password; unprivileged Linux user; owns their own data + per-user app instances. Trusted (household). |
| **Admin** | Member + `sudo` + host mutation via the dashboard/host-agent. Trusted; a compromised admin is high-impact by design. |
| **App container** | Runs code we didn't write. **Assume it can be compromised.** Confined to its declared permissions. |
| **Catalog app author** | Submits manifests. Trusted via curation, not via sandbox. A *deliberately* malicious author is out of scope (curation's job). |
| **moose cloud** | DNS resolver + ACME helper. Sees box-ids and query metadata; never sees traffic or data. Honest-but-curious. |
| **Possessor of a removed drive** | Has a drive that left the box. Hostile. Defended by LUKS. |
| **Possessor of the whole box** | Physical theft of the running/poweroff box incl. TPM. **Out of scope beyond at-rest encryption.** |
| **Internet attacker** | Off-LAN, no mesh credential. In v1 has **no reachable surface** (closed-by-default). |

## Trust boundaries

The analysis is organized by boundary, because that's where threats live. Each table: the threat, the doc that owns the mitigation, and the residual risk we knowingly carry.

### B1 — LAN/mesh ↔ public internet

The box presents **no surface to the public internet in v1.** SSH/SMB are firewalled to RFC1918 + the mesh interface; store apps don't bind host ports; the cloud never carries traffic.

| Threat | Mitigation (owner) | Residual |
|---|---|---|
| Remote exploitation of an exposed service | Closed-by-default; SSH/SMB scoped to RFC1918+mesh via nftables (`AUTH.md` # Device access, `BUILD.md` # SSH); no public-exposure toggle (`MOOSE_NETWORK.md` # closed by default) | BYO-domain + explicit advanced exposure is the user's consciously-accepted risk |
| Apps reaching the internet uninvited | `internet: false` → `internal: true` bridge, kernel-level (no NAT route) (`APP_ISOLATION.md` # internet) | — (host ports are admission-rejected for both doors; nothing binds to the host) |
| Cloud-mediated LAN access concerns | Cloud resolves names + sets ACME TXT only; no proxy/tunnel (`MOOSE_NETWORK.md`) | Cloud learns box-id exists and who queries it (metadata, not content) |

### B2 — App container ↔ host (assume breach)

The richest boundary. The right question is not "can an app be compromised" (assume yes) but "what's the blast radius when one is?"

| Threat | Mitigation (owner) | Residual |
|---|---|---|
| Container escape to host root | `cap_drop: ALL`; `privileged`, docker socket, `SYS_ADMIN` admission-rejected for **both doors**; Docker default seccomp/AppArmor (`APP_ISOLATION.md` # Forbidden for both doors, # Capabilities) | No userns remap, no custom seccomp in v1 (Docker defaults deemed sufficient). **Admission blocks escape via an app's *own* compose only — it does nothing about reaching the socket-proxy over the network at runtime; see the Brain↔Docker row, a live gap until #187.** |
| App lies about declared permissions | Enforced at kernel/Docker layer, not metadata — violations silently fail + log (`APP_ISOLATION.md` # Failure mode) | — |
| Compromised app reads beyond its scope | Per-app bridge (no inter-app traffic); bind-mounts limited to the declared `folders` at their elected source (one user's home, or the household-shared tree the owner can already reach); other homes not on its filesystem (`APP_ISOLATION.md` # Filesystem) | Blast radius = the app's declared permissions + its mounted folders + its own managed DB. An app *can* read everything its grants allow — that's the grant, not a leak |
| Brain↔Docker control-plane abuse | Brain talks to Docker via socket-proxy, not raw socket (`CONTROL_PLANE.md` # Locked: Docker socket exposure) | **Live gap, pending #187.** The proxy is body-blind — an allowed `POST /containers/create` can carry `Privileged:true` + `Binds:["/:/host"]`, then `/start`, and run as host root. Measured on a real box (#430). The proxy sits on `moose-ingress`, which app `main_service` containers also join, so any compromised app reaches `docker-proxy:2375` with `curl`. Not closable by the allowlist (the brain needs `CONTAINERS`+`POST`); the fix is network isolation — #187 takes apps off `moose-ingress`, leaving only the brain able to reach `:2375` |
| Compromised control-plane container (Caddy, `moose-ui`, socket-proxy) | Same sandbox app containers get: `cap_drop: ALL` (Caddy keeps only `NET_BIND_SERVICE`), `no-new-privileges`, read-only root on Caddy + `moose-ui` (`CONTROL_PLANE.md` # Locked: control-plane container hardening, #431) | The **brain** container is not sandboxed — it needs `CAP_CHOWN` to own app data dirs, so its capability set has to be named and proven on a booted box first. The socket-proxy runs with a writable root (its image writes `/tmp`, `/run`, `/var/lib/haproxy`) |
| Privileged Door-2 app | **Rejected** — admission is door-symmetric; `privileged`/socket/`cap_add`/host-ports/host-namespaces are refused for custom compose exactly as for store apps, because a container escape on a multi-user box hits every member, not just the (admin-only) installer (`APP_ISOLATION.md` # Trust tiers, `DECISIONS.md` 2026-06-02) | An admin who needs such a container runs it over SSH (`AUTH.md` # SSH is rescue) — deliberate, not one-paste; that residual is the box owner's own root access |

**Blast-radius summary:** one compromised store app reaches its own data, its user's declared folders, and the internet (if granted) — **not** host root, other users' homes, or other apps. That containment is the security claim; preventing the compromise itself is curation's job, not the sandbox's. **One caveat holds this open today:** the socket-proxy is reachable from app containers on `moose-ingress`, and reaching it is host root (Brain↔Docker row). The claim is true once #187 closes that network path; until then it is the box's most severe open gap.

### B3 — Member ↔ admin

| Threat | Mitigation (owner) | Residual |
|---|---|---|
| Member performs admin action | Role checked server-side in the brain every request; routes grouped by role at router level; UI hiding is defense-in-depth only (`AUTH.md` # Roles) | — |
| Stolen elevated session | 5-minute elevation window; enrollment-class ops (add/eject drive) require a fresh password every time (`USERS_AND_GROUPS.md` # Elevation, `AUTH.md`) | A forgotten open tab is bounded to 5 min of elevation |
| Demoted admin retains power | `gpasswd -d` flips group membership (`USERS_AND_GROUPS.md`) | Live `sudo`/SSH session keeps capability until logout — accepted under household trust |
| Compromised admin SSH | SSH off-by-account-by-default; admin must opt in (`AUTH.md` # Device access) | A compromised admin shell is root — accepted; marginal, since the admin can already mutate the host via host-agent |

### B4 — Box ↔ moose cloud

| Threat | Mitigation (owner) | Residual |
|---|---|---|
| Cloud compromise injects bad data | Cloud is DNS + ACME-helper only; per-box keypair auth; enrollment opt-in (`MOOSE_NETWORK.md`) | Compromised cloud could mis-resolve a box-id; cannot decrypt traffic or reach data |
| Cloud sees user activity | No traffic ever traverses cloud servers (`MOOSE_NETWORK.md` # What cloud actually does) | Cloud sees box-ids and which devices query them — disclosed; privacy-doc surface |
| Privacy-strict user wants zero cloud | Enrollment is opt-in; box never contacts cloud if declined; BYO-domain alternative (`MOOSE_NETWORK.md`) | — |

### B5 — At-rest disk ↔ removed drive / stolen box

| Threat | Mitigation (owner) | Residual |
|---|---|---|
| Drive theft / RMA / removal | LUKS on every drive; separate keyslots; one recovery passphrase (`STORAGE.md` # Encryption posture) | **Defended** — a drive that leaves is unreadable |
| Whole-box theft | — | **Out of scope.** TPM is in the box; PCR-7 auto-unlock means a thief with the box boots into decrypted storage. Future PIN-on-boot "high-security mode" is the planned answer (`STORAGE.md`) |
| Attacker subverts Secure Boot | TPM seal against PCR 7 (`STORAGE.md`) | PCR-7-only is weaker than PCR 7+11; trade made for kernel-update-survivable unattended boot. Stricter sealing is a non-destructive upgrade (`NEXT.md`) |
| Doubly-lost (no backup + lost passphrase) | — | **Out of scope.** Data gone. Honest, same shape as losing both copies of any single-copy secret |

### B6 — Per-user isolation (member ↔ member)

| Threat | Mitigation (owner) | Residual |
|---|---|---|
| One user reads another's files via FS | `/home/<user>/` `0750`, owned by user; per-user container runs as user UID, sees only own folders (`STORAGE.md` # Permissions, `APP_ISOLATION.md`) | — |
| Cross-user managed-DB access | Per-(user,app) Postgres on that instance's private network — impossible by construction (`APP_ISOLATION.md` # Managed services placement) | — |
| Admin/root reads another user's data | — | **Out of scope in v1.** LUKS-only encrypts the disk, not users from each other; once unlocked, root reads all. Per-user fscrypt is roadmap. v1 features touching user data are built *as if* fscrypt were on (per-user-keyed backup, no admin cross-user indexing) so the upgrade is data-only (`STORAGE.md` # Future) |
| Login screen reveals household members | User-list login (`AUTH.md` # Login screen UX) | Accepted under household trust; privacy-conscious users flip to blank-form login |

### B7 — Credential / authentication surface

| Threat | Mitigation (owner) | Residual |
|---|---|---|
| Session cookie leaks to app origins | Cookie host-scoped, `Domain` unset — never broadened to `.local` (`AUTH.md` # Sessions). **Load-bearing for origin isolation** | — |
| Credential stuffing / brute force | Per-username exponential backoff + per-IP token bucket (`AUTH.md` # Rate limiting) | Brain's own login endpoint throttling is the floor; finer lockout tuning in `NEXT.md` |
| CSRF | `SameSite=Lax`; no state-changing GETs (`AUTH.md` # CSRF) | Future third-party API needs explicit CSRF tokens — out of scope until then |
| Offline hash cracking | yescrypt/argon2id via PAM; brain stores no password hash (`AUTH.md` # Identity primitive) | Requires the drive to already be decrypted (B5) |
| Recovery-code abuse | argon2id hash, show-once, single-use, no physical-access reset (`AUTH.md` # The recovery code) | Phone-photo of code lands in user's cloud backup — accepted convenience trade; lost-code + forgotten-password = unrecoverable |
| Weak / no second factor | Password-only in v1; HIBP non-blocking warning (`AUTH.md`) | No 2FA/TOTP in v1 — designed to coexist later (TOTP is origin-independent) |

### B8 — Brain ↔ host-agent

The most-trusted internal boundary: host-agent runs as root and trusts the brain completely.

| Threat | Mitigation (owner) | Residual |
|---|---|---|
| Unauthorized process drives host-agent | UNIX socket access gated by the `moose` group, kernel-enforced; **exactly one member** (brain's runtime UID), CI-asserted (`USERS_AND_GROUPS.md` # Group reference, `AUTH.md` # Test invariant) | The CI invariant *is* the entire authz model here — if group membership is wrong, the boundary is broken |
| Brain compromise | — | A compromised brain = host compromise, by design. The brain is the trusted control plane; isolating it from host-agent would defeat its purpose. Defense is keeping the brain small + the `moose`-group invariant tight |

## Out of scope (named loudly)

These are **not defended against in v1**, by deliberate choice:

- **Whole-box physical theft** beyond at-rest encryption (TPM is in the box). Future PIN mode.
- **Member-vs-member at admin/root level** — no fscrypt in v1; root reads all. Roadmap.
- **A deliberately malicious catalog author** — curation filters this; the sandbox bounds blast radius but is not claimed to contain a determined hostile author who slips through review.
- **Nation-state / targeted physical-evil-maid / hardware implants.**
- **Side-channel & timing attacks** (cache, power, EM).
- **Supply-chain compromise** of Debian, Docker, or upstream app images. Partially *reduced* by catalog digest-pinning (`APP_STORE.md`) and signed release manifests (`RELEASE_MANIFEST.md`), but not modeled as a defended adversary.
- **Denial of service.** No hard cgroup enforcement in v1 (`APP_ISOLATION.md` # Resource limits); a runaway app can starve the box. Home-scale, low-priority; OOM hints later.
- **The doubly-lost data scenario** (no backup + lost secret).

## Residual risks we knowingly accept (consolidated)

The honest list, gathered from across the spec so it lives in one place:

1. A compromised app reads everything its granted permissions allow (the grant is the boundary).
2. Admin/root reads any user's data until fscrypt ships.
3. Compromised admin SSH session = root; demotion doesn't kill a live session.
4. Whole-box theft defeats at-rest encryption (PCR-7 auto-unlock).
5. PCR-7-only sealing is weaker against Secure-Boot subversion than PCR 7+11.
6. Cloud DNS sees box-ids + query metadata.
7. Recovery-code photo can land in a user's cloud backup.
8. Lost recovery code + forgotten password = unrecoverable account; lost secret + no backup = lost data.
9. No 2FA in v1.
10. No DoS protection (no hard resource caps) in v1.
11. A compromised brain compromises the host.

Each is defensible under the household trust model and the v1 scope; each has a named future upgrade where one exists.

One item on this list is **not** knowingly accepted — it is a live gap with a fix in flight, listed here so a reader scanning this section does not miss the box's most severe current exposure:

- **A compromised *app* can escape to host root via the socket-proxy, until #187.** The proxy is body-blind and shares `moose-ingress` with app `main_service` containers, so any compromised app can reach `docker-proxy:2375` and start a privileged, host-bind-mounted container (measured, #430; B2 Brain↔Docker row). This is item 11 widened from "the brain" to "any app," and it closes when #187 takes apps off that network — not a residual we accept, a bug we are fixing.

## Methodology note

The per-boundary tables above are the deliverable. **STRIDE** (Spoofing, Tampering, Repudiation, Information disclosure, Denial of service, Elevation of privilege) was walked across each boundary as a behind-the-scenes checklist to avoid missing a category — not rendered as a cell-by-cell matrix, which reads worse as a spec doc. Tampering of the **audit log** specifically is addressed by append-only SQLite triggers (`LOGGING.md` # Tamper-evidence); Repudiation by the audit trail itself (`LOGGING.md`); the hash-chain integrity guarantee is deferred (`NEXT.md`). A light **privacy pass** (LINDDUN-flavored) on the metadata assets is covered by the closed-by-default posture, telemetry-off-by-default (`TELEMETRY.md`), and local-analytics-never-leave-the-box (`LOCAL_ANALYTICS.md`).

## When this model changes

This is a **living document**, revisited when a trust boundary moves. The known future trigger: **remote access via the mesh** (`MOOSE_NETWORK.md` # Deferred). When it ships, B1 changes shape — `.onmoose.io` names become reachable off-LAN, scoped pairing introduces a new principal (a paired-but-non-household device, e.g. "grandma sees Photos"), and the closed-by-default claim narrows to "closed except to identity-paired devices." That warrants a dedicated boundary pass and `DECISIONS.md` entries; it is explicitly not modeled here.

## Locked decisions

- **This doc is a lens, not a source of mitigations.** Every defense is owned by another doc; this one indexes and points.
- **Household trust model is the root assumption.** Members trust each other; adversaries are the network, a compromised app, and a removed drive.
- **Assume any app can be compromised.** The sandbox bounds blast radius; curation prevents the compromise. Neither claims to contain a deliberately malicious author.
- **Scope is v1 closed-by-default.** The deferred mesh surface is explicitly out of scope and gets its own pass.
- **Named non-goals are first-class:** whole-box theft, member-vs-member-at-root, malicious author past curation, nation-state, side-channels, supply-chain-as-adversary, DoS, doubly-lost.
- **Boundary-narrative + tables is the format;** STRIDE is a checklist, not a matrix.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
