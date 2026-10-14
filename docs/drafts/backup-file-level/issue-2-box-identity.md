TITLE: Box identity: Ed25519 enrollment and request signing for the box→cloud channel

Size: M
Area: backend
Depends on: the specs issue (#N)

## Summary

A hosted box has no general-purpose way to authenticate itself to the hosted control plane. Certificate renewal uses the acme-dns credential, which is scoped to DNS; catalog sync is unauthenticated because it fetches a public snapshot. Neither generalizes, so the first thing a box needs to *report* has nothing to report with. This builds the channel once: a single-use bootstrap token in the seed, an Ed25519 keypair generated on the box, and signed requests thereafter.

## Spec / source of truth

`docs/specs/ENVIRONMENT.md` # Box identity (written by the specs issue). Read # Provisioning & first-boot and # Admin bootstrap — as built end-to-end first — this extends the seed contract they define, and reuses their first-boot-once ingestion pattern deliberately.

## Do

- **Extend seed ingestion** with the bootstrap token field, following the existing shape in `internal/` where `seed.json` is read. The seed's JSON shape is a wire contract mirrored byte-for-byte on the other side; the two repos meet at the format, not a shared Go type.
- **Generate an Ed25519 keypair at first boot** and persist it. The private half never leaves the box and is never transmitted. Persist with the same care the bootstrap-secret hash gets today: the write ordering must make a crash mid-write re-runnable next boot rather than stranding a half-state.
- **Enrollment call**, spending the token exactly once to register the public half. It must:
  - run **before anything that needs the channel**, in boot order;
  - **retry through the first-boot DHCP race**, the way the seed metadata fetch does (bounded, never blocking forever, exiting cleanly on a definitive negative);
  - be **first-boot-once** — a box that has enrolled ignores a re-delivered token, mirroring how a persisted `box_id` makes identity frozen.
- **Request signer** producing the canonical string over method, path, body digest, timestamp, and nonce. This is the delicate part: canonicalization ambiguity produces interop bugs that appear only for certain payloads (empty body, query string, non-ASCII path, repeated header). Implement against the spec's byte-exact definition and **test against fixed vectors**, not against a round-trip with our own signer — a round-trip passes happily when both sides are wrong in the same way.
- **Rotation**: a new key signed by the old one, on a schedule.
- **Secrets never logged.** Not the token, not the private key, not a vended credential — through seed assembly, `slog` fields, or an error string. Add the negative test.

## Touch

`internal/` (seed ingestion and a new box-identity package), `cmd/brain/`, `dev/cloud/` if the first-boot ordering needs a unit change. Cross-platform: the signing and enrollment logic must compile everywhere and stay inner-loop testable; only host wiring is Linux-only.

## Done when

A box boots un-enrolled, generates a keypair, spends its token, and signs a subsequent request that verifies against the canonical string — proven in the QEMU cloud lane across a multi-boot sequence, the way the seed gate is (un-enrolled → enrolled → reboot keeps the same identity). A replayed token is refused. Fixed-vector tests cover the canonical string including the awkward payloads. No secret appears in any log at any level.

## Notes

The canonical signing string is co-owned with the other side of the channel and cannot be changed unilaterally once either side ships.
