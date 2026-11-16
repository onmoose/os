DRAFT — new section for docs/specs/ENVIRONMENT.md, placed immediately after "### Admin bootstrap — as built" and before "## Networking & discovery (hosted v1)".

PREREQUISITE FIX, same PR: "### Admin bootstrap — as built" is stale relative to the code. It describes the seed as `{box_id, admin_bootstrap_secret, enrollment}` and a `/setup` gate taking a `bootstrap_secret` body field. `internal/profile/seed.go` shows the seed is actually `{box_id, assertion_verification_key, enrollment}`, and `internal/store/store.go` states the assertion key "replaces the prior one-time admin-bootstrap secret hash" — hosted admin bootstrap is portal SSO now. Correct that section before adding this one; the text below assumes the corrected version.

---

## Box identity — the authenticated box→cloud channel

Everything a hosted box says to the hosted control plane today is a special case. Certificate renewal authenticates with the acme-dns credential, which is scoped to DNS and meaningless outside it. Catalog sync is unauthenticated, because it fetches a public snapshot. Neither generalizes, so the next thing a box needs to say — a backup outcome (`BACKUP.md` # Box-initiated push) — has nothing to say it with.

That is not a one-off gap. Fleet health, update status, disk pressure, and the credential delivery backup itself depends on are all the same shape: **the box asserts who it is, and the control plane answers with desired state.** The channel is built once.

It also has a security payoff independent of any feature that uses it. `seed.json` carries `enrollment.password`, the long-lived acme-dns credential this doc already names as the worst-case box secret — worst-case because it can rewrite `_acme-challenge.<box-id>` and so mint or MITM the box's wildcard certificate. The seed is delivered over a metadata endpoint that stays readable for the server's whole life, which is why the image carries a standing rule blocking container egress to it (# Provisioning & first-boot, #251). A **single-use, short-lived** bootstrap token makes that exposure self-limiting, and is the precondition for eventually removing the long-lived credential from the seed entirely.

The seed's other two fields are unaffected: `box_id` stays the box's frozen identity, and `assertion_verification_key` keeps doing its own job (verifying portal SSO assertions inbound to the box). This section adds the **outbound** direction, which nothing covers today.

### Shape

- **The seed carries a bootstrap token**, single-use with a TTL measured in hours, alongside the fields # Admin bootstrap — as built already defines.
- **The box generates an Ed25519 keypair locally at first boot.** The private half never leaves the box and is never transmitted.
- **The box spends the token exactly once** to register its public half. The exchange is atomic control-plane-side: a replayed token is refused, and a box that already enrolled does not re-enroll.
- **Every subsequent request is signed** over a canonical string of method, path, body digest, timestamp, and nonce.
- **Rotation** is a new public key signed by the old one. **Revocation** is clearing the registered key.

**Ed25519 signed requests rather than mTLS.** Client-certificate authentication terminates wherever TLS terminates, which on any proxied surface means the identity check happens in one process and the authorization that depends on it in another, joined by a trusted header. Signing at the request layer keeps the two together. `MALMO_NETWORK.md` made the same call for assertions in the other direction.

### Constraints the box side must honor

- **The canonical signing string is a wire contract**, in the same class as the seed's JSON shape and the assertion token format: the two sides meet at bytes, not at a shared type. Canonicalization ambiguity is the classic source of interop bugs that appear only for certain payloads — an empty body, a query string, a non-ASCII path — so it is specified byte-exactly and tested against fixed vectors on both sides.
- **Enrollment precedes anything that needs the channel** in boot order, and retries: first boot races DHCP exactly the way the seed fetch does (# Provisioning & first-boot).
- **Enrollment is first-boot-once, like seed ingestion.** The registered key is the box's frozen credential; re-delivering a spent token cannot re-key a provisioned box.
- **The private key is at-rest state on the box**, with the same custody question as the enrollment credential it is intended to eventually replace (`NEXT.md` # Encrypt hosted enrollment credentials at rest).
- **No secret reaches a log.** Not the bootstrap token, not the private key, not a vended credential — through seed assembly, through structured `slog` fields, or through an error string.
- **A restore is a new identity.** A box restored from backup enrolls fresh and gets a new keypair; it is not the old box (`BACKUP.md` # Restore). Any authorization to read the previous box's backup therefore cannot be anchored to box identity.

**On `appliance`, none of this exists.** There is no control plane to report to.
