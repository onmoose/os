# moose Release Manifest

> How moose publishes a new brain + UI version to the fleet. Sibling to `UPDATES.md` (which covers the box-side update model) and `BUILD.md` (which covers ISO and `.deb` artifacts). This doc owns the **control-plane release manifest** — the JSON file that tells every box "the current moose is X.Y.Z."

The scope here is brain + UI only. Debian base updates flow through `unattended-upgrades`; `host-agent` flows through our apt repo; both are described in `UPDATES.md` and `BUILD.md` and do not use this manifest.

> **Appliance only.** Everything in this doc — the static JSON file, the CDN, the minisign signature, the hourly poll, and the `rollback_to` kill switch — applies to boxes moose does **not** operate. A **hosted** box (`ENVIRONMENT.md`) never fetches this manifest. Its target version is held per-box by the cloud control plane and the box asks for it directly, because on hosted we have an authenticated channel to each box and do not need a signed broadcast to reach machines we cannot otherwise address. See `UPDATES.md` # 8.1. The two profiles share the apply-and-rollback transaction; they differ only in what triggers it.

## What the manifest is

A static JSON file served at:

```
https://releases.onmoose.io/stable.json
https://releases.onmoose.io/stable.json.minisig
```

`host-agent` polls the manifest hourly. **As built**, each tick is jittered by up to five minutes — not in the original design, and added because a fleet whose boots cluster (a provider maintenance window, a region-wide power cut) would otherwise hit a static file host in a spike, for no benefit. Also as built: a host-agent binary with **no signing key baked in refuses every manifest and does not poll at all**, which is every build today (# Signing defers key custody until there is a release to sign). That is the deliberate safe state — a box with no way to check a manifest must ignore it, not trust it. When the manifest names a version pair newer than what's installed, the dashboard surfaces an "Update available" prompt (admin-prompted per `UPDATES.md`). When the admin clicks Update, host-agent pulls the named brain and UI images from `registry.onmoose.io` and runs the changed-only transaction described in `UPDATES.md`.

**As built, this is what stops the appliance updater (#401).** The box now has one update-target seam that both profiles read (`UPDATES.md` # 8.4), and it will only act on an image reference pinned to a digest — a box that resolved a tag itself would be trusting a movable label at update time. A manifest naming `1.4.2` cannot be turned into anything but a tag, so the appliance source reads the verified manifest and **refuses it as unpinned**, leaving the box where it is. Today that is academic (no signing key is baked into any build, so no manifest ever verifies), and closing it means adding optional pinned-reference fields alongside the versions — additive, no `manifest_version` bump, and tracked in `NEXT.md` because it belongs with the appliance release pipeline that does not exist yet.

The manifest names versions, not image URLs. The registry path is fixed (`registry.onmoose.io/moose/brain:vX.Y.Z`, `registry.onmoose.io/moose/ui:vX.Y.Z` — see `BUILD.md`). Keeping it implicit means we can move the registry later without re-cutting every prior manifest.

## Manifest schema (v1)

```json
{
  "manifest_version": 1,
  "channel": "stable",
  "brain": "1.4.2",
  "ui": "1.4.9",
  "minimum_host_agent": "1.0.0",
  "released_at": "2026-05-08T12:00:00Z",
  "rollback_to": null
}
```

Fields:

- **`manifest_version`** — schema version. Bumped only on breaking changes. host-agent ignores unknown fields, so additive evolution (new optional fields) does not bump this.
- **`channel`** — the channel this manifest applies to. v1 only ships `"stable"`. Included from day one to make a future `"beta"` channel additive rather than a flag day.
- **`brain`** — semver of the moose-brain image to run.
- **`ui`** — semver of the moose-ui image to run. **Note:** `BUILD.md` # Versioning moved to one repo version for the whole monorepo (DECISIONS.md 2026-07-16) — `brain` and `ui` are cut from the same commit and are always the same value in practice. They stay two fields here rather than being collapsed into one, since this schema is unbuilt and collapsing it is out of scope for that change; don't read the two fields as independently-versioned.
- **`minimum_host_agent`** — semver. If the box's host-agent is older, the manifest is ignored and the prompt does not surface. host-agent updates ride apt and roll out on their own cadence; this field is the safety belt.
- **`released_at`** — RFC 3339 timestamp. Informational; not used for any gating in v1. (Phased rollouts would use it; see "Future work" below.)
- **`rollback_to`** — the kill switch. `null` in steady state. When set to a prior `{"brain": "...", "ui": "..."}` pair, every box behaves as follows:
  - **Boxes that haven't yet applied the current version:** the prompt is silently retracted. They never saw a known-bad offer.
  - **Boxes that have already applied the current version:** the dashboard surfaces a "Downgrade recommended" prompt to revert to the named pair, using the previous-image snapshot retained for seven days per `UPDATES.md`.

The schema is intentionally small. Anything not in the schema is implicit (registry path) or out of scope (rollout pacing, cohort selection — see "Future work").

## Where it lives: git + CDN

The source of truth is a git repository (`github.com/moose/releases` or similar). The CDN at `releases.onmoose.io` serves the contents of `main` as static files. A commit on `main` becomes the live manifest within seconds of CDN sync.

Why git as the source rather than direct uploads to object storage:

- **Diffable history.** Every release is a commit. `git log stable.json` is the audit trail.
- **Free rollback.** Reverting a bad promotion is `git revert`.
- **Reviewable promotions.** A pull request is the unit of release; CI runs against it before merge.
- **Signature lives next to the artifact.** `stable.json.minisig` is committed alongside `stable.json`. The repo is self-contained.

The CDN is purely a delivery cache. It can be swapped (S3, R2, GitHub Pages, plain nginx) without affecting the publishing flow or the boxes.

## Signing: minisign / Ed25519

The manifest is signed with [minisign](https://jedisct1.github.io/minisign/) (Ed25519 signatures). host-agent verifies the signature on every fetch and refuses to act on an unsigned or invalidly-signed manifest.

Why minisign:

- Tiny verifier (one Ed25519 verify + a small header parse). ~100 LOC of Go in host-agent.
- Offline-friendly: signing happens on a maintainer's air-gapped or hardware-token-protected machine. The signing key is never on a server, never in CI secrets.
- No transparency log, no key directory infrastructure, no CA. The pubkey is baked into host-agent at build time.

**Verifier accepts a list of pubkeys, not a single constant.** This is the one design choice that prevents future pain. Key rotation (or migrating to a different signing scheme entirely) then follows the standard pattern:

1. Ship a host-agent release that accepts `{old_pubkey, new_pubkey}`.
2. Wait for the apt-driven host-agent rollout to reach the fleet.
3. Dual-sign manifests for a transition window.
4. Stop signing with the old key.
5. In a later host-agent release, drop the old key from the accepted list.

No flag day, no forced-update cliff. Without the list-from-day-one, a compromised or lost key would force a synchronized fleet update — an outage shape we deliberately avoid.

The signing key itself, key custody, and the rotation runbook are tracked separately (release-infra concern; deferred until we have a release to sign, per `BUILD.md`).

## Channels: one channel for v1

v1 ships a single `stable` channel. The `channel` field is present in the schema so adding `beta` later is additive — a new `beta.json` file alongside `stable.json`, an opt-in dashboard setting, no schema change.

We are not shipping a beta channel in v1 because:

- The early install base is small enough that a self-selected beta cohort would be effectively empty.
- Detection of bad releases at v1 scale will come from GitHub issues, support forum, and direct user reports — channels that work the same whether a beta exists or not.
- The risk a beta would mitigate (eager adopters acting as canaries) is already mitigated by admin-prompted updates: the first admins to click are self-selected eager adopters, functionally equivalent to a beta cohort.

When beta returns (driven by either auto-apply landing post-A/B-images, or fleet growth past the point where direct reports are timely), it slots in additively.

## Promotion: a GitHub release flips the manifest

Promoting a new version is a pull request that updates `stable.json` and `stable.json.minisig`. The flow:

1. Maintainer cuts a release tag for the brain and UI images in their respective repositories. CI builds, tests, and publishes the images to `registry.onmoose.io`.
2. Maintainer drafts the new manifest JSON locally, signs it with the offline key (`minisign -S -s ~/.moose/release.key -m stable.json`), and opens a PR against the `releases` repo with both files updated.
3. CI on the PR validates:
   - JSON parses against the schema (`manifest_version`, required fields, semver shape).
   - Signature verifies against the published pubkey.
   - Both image tags (`brain:vX.Y.Z`, `ui:vY.Y.Z`) exist in the registry and pass a manifest-pull check.
   - `minimum_host_agent` is satisfied by a host-agent version that already exists in the apt repo.
4. Maintainer merges. CDN picks up the new file on its next sync (seconds).
5. Boxes pick up the new manifest on their next hourly poll.

Promotion is therefore a one-line diff with verifiable preconditions. The discipline lives in the PR template ("did you dogfood on a non-production box for at least N hours? are there open critical issues against the prior release?"), not in the manifest format.

For the v1 single-maintainer phase, the PR can be self-merged. As the team grows, branch protection on `main` requiring an additional reviewer is a one-setting change with no doc impact.

## Kill switch: setting `rollback_to`

When a bad release is discovered post-promotion, the response is a follow-up PR that sets `rollback_to` to the prior version pair. Same signing + verification flow as a normal promotion. Once merged and CDN-synced (seconds), the offer for the bad version is retracted from every un-updated box on its next poll, and already-updated boxes see a downgrade recommendation.

This is the load-bearing protection in v1. It is independent of phased rollout — the kill switch works whether the rollout is staggered or instantaneous, and works whether or not telemetry is enabled. Its effectiveness is bounded by how fast we detect a bad release, which at v1 scale is "GitHub issues + forum + direct reports" — hours to days.

## Failure modes

- **Pointing a test box somewhere else.** `MOOSE_RELEASE_BASE_URL` replaces the `releases.onmoose.io` base, so a box under test can read a manifest from a local file server. This is safe because **the base URL is not what we trust — the signature is.** A manifest from any host must still verify against the baked keys. So this setting cannot feed a box an unsigned or wrongly signed manifest. Leave it empty for the real releases host.

- **Box can't reach `releases.onmoose.io`:** host-agent keeps the last-known manifest in `/var/lib/moose/manifest.json` (with its signature). Updates pause until connectivity returns. Consistent with `UPDATES.md`: an offline box stays current at its last-applied version.
  **As built** (`internal/hostagent/relmanifest`): the manifest and its signature share **one** file at that path, and the reason is a crash. Two files means two renames, and a power cut between them leaves the new manifest beside the old signature — a pair that cannot verify, with the previous good manifest already overwritten. The box would then have no usable cache at all, which is not "the previous valid manifest stays in effect". One file is one rename. The cache is also **re-verified when it is read**, so writing `/var/lib/moose` is not a way around the signature.
- **Signature verification fails:** host-agent logs and ignores the manifest. The previous valid manifest stays in effect. A persistent signature failure surfaces as a dashboard warning after 24 hours (operator should investigate; could indicate a CDN/storage corruption or, very rarely, a compromised publishing path).
- **`minimum_host_agent` not satisfied:** the manifest is honored as far as "this is the current release" but the prompt does not surface. The next host-agent update from apt resolves it; the prompt appears on the following poll.
- **Image tag missing from registry at update time:** the update fails health-check and rolls back per `UPDATES.md`. The release was malformed (CI should have caught this — the missing-image precondition exists for that reason).

## Future work — phased rollout / cohorts

Not in v1; documented here so the shape is known when we turn it on.

When auto-apply updates land (post-A/B-images), the natural "admins click at their own pace" rollout disappears, and we need an explicit pacing mechanism. The Ubuntu-style approach is additive to the current schema:

```json
{
  ...
  "rollout": [
    { "after": "0d",  "percent": 5  },
    { "after": "2d",  "percent": 25 },
    { "after": "4d",  "percent": 50 },
    { "after": "7d",  "percent": 100 }
  ]
}
```

Each box computes its bucket deterministically and locally: `bucket = hash(machine_id || canonical(brain, ui)) mod 100`. The box checks "is my bucket below the current percent for the elapsed time since `released_at`?" If yes, the offer (or the auto-apply) proceeds. If no, the box defers to the next poll.

Why hash over the version pair rather than over the manifest bytes: the rollout schedule itself may be edited mid-rollout (e.g., to extend the curve after a partial-trouble signal). Hashing over `{brain, ui}` keeps cohort identity stable across such edits. Canonical form is a fixed byte sequence — `brain="X";ui="Y"` or RFC 8785 JCS — pinned when this lands.

The trigger to add it:

- **Primary:** A/B immutable images land. Auto-apply becomes safe; admin-prompting no longer provides natural pacing. Cohorts then become the rollout's primary protection.
- **Secondary:** fleet scale outgrows direct-report detection. If we ship a bad release and the forum is too slow to surface it before the majority is affected, that's the empirical signal.
- **Either way:** cohorts are an additive field. host-agent code that already accepts unknown fields will silently ignore `rollout` from older builds; new host-agent versions honor it.

Beta channel reactivation, telemetry-gated halts, and per-region rollouts are independent of cohorts and tracked in `UPDATES.md` and `NEXT.md`.

## Locked decisions

- **This manifest is the appliance trigger only.** Hosted boxes get their target version per-box from the cloud control plane and never fetch `stable.json` (`UPDATES.md` # 8.1).
- **One static JSON manifest per channel**, served from a CDN backed by a git repo. v1 has one channel: `stable`.
- **Signed with minisign (Ed25519).** Pubkey baked into host-agent at build time. **Verifier accepts a list of pubkeys** for forward-compatible key rotation.
- **Schema is intentionally small** (`manifest_version`, `channel`, `brain`, `ui`, `minimum_host_agent`, `released_at`, `rollback_to`). No phased rollout, no cohort fields in v1.
- **`manifest_version: 1`** present from day one; host-agent ignores unknown fields so new fields land additively without flag days.
- **`rollback_to` is the kill switch.** Independent of rollout pacing. Retracts the offer from un-updated boxes and recommends downgrade on already-updated boxes.
- **Promotion is a PR against the `releases` repo.** CI validates JSON shape, signature, image existence, and host-agent minimums. Merge to `main` is the publish action.
- **No phased rollout in v1.** Every box that polls sees the new manifest immediately. Justification: admin-prompted updates already provide natural pacing at v1 scale.
- **No beta channel in v1.** Additive when the trigger (auto-apply or fleet scale) arrives.

## Open questions

Tracked centrally in [`NEXT.md`](NEXT.md). Resolutions land back here (or in `DECISIONS.md` if they flip a position).
