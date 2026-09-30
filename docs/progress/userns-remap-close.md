# Close the userns-remap feature, #516 and #523

- **Status:** done
- **Date:** 2026-09-30
- **Specs touched:** `NEXT.md` (the remap sentence in the per-container ID maps item)

This entry closes the daemon-wide Docker `userns-remap` work, #523, and with it #516. It follows [userns-remap-boots.md](userns-remap-boots.md) (#531), the last slice. It adds no code. It records what shipped, the run on `dev` that proves it, what the next release needs, and the gaps left open on purpose.

## What was done

### What shipped

The work ran from Step 0 to the last CI boot. In order:

| Entry | PR | What |
|---|---|---|
| [userns-remap-pretest.md](userns-remap-pretest.md) | #517 | Step 0 on a GCE box: no app failed only under the remap |
| [userns-remap-ci-proofs.md](userns-remap-ci-proofs.md) | #518 | the #516 proofs on the booted hosted image, from a throwaway branch |
| [brain-folder-sources.md](brain-folder-sources.md) | #522 | the shared tree mounted into the brain, and `prepare-folder` for personal folders (#519, found by the proofs) |
| [userns-remap-spec.md](userns-remap-spec.md) | #524 | the spec: the tiers, `root_setup`, the `moose-remap` range |
| [brainlaunch-userns-host.md](brainlaunch-userns-host.md) | #532 | slice 1: the proxy and the brain with `--userns=host` |
| [remap-base-well-known.md](remap-base-well-known.md) | #534 | slice 2: `remap_base` on the well-known endpoint |
| [root-setup-manifest-field.md](root-setup-manifest-field.md) | #535 | slice 3: `root_setup` and its admission rules |
| [caddy-route-swap.md](caddy-route-swap.md) | #533 | a failed route write keeps the old route (#520, found by the proofs) |
| [brain-userns-tiers.md](brain-userns-tiers.md) | #536 | slice 4: the brain's tiers, stored on the instance |
| [image-user-tier.md](image-user-tier.md) | #538 | the image tier, `image_user` (#537) |
| [userns-remap-on.md](userns-remap-on.md) | #539 | slice 5: the remap on in both images |
| [startup-wait-docker.md](startup-wait-docker.md) | #541 | the brain waits for Docker before its startup reconcile (#540, found by the reboot boot) |
| [userns-remap-boots.md](userns-remap-boots.md) | #542 | slice 6: the `remap` and `remap-reboot` boots |

### The proof on `dev`

`CI / Cloud image` on `dev` as merged (commit `cfc1491`), with `publish=false` and the full gate list:

https://github.com/onmoose/os/actions/runs/36694063081 **passed**. All eight boots passed: `unseeded`, `seeded`, `bios`, `access`, `update`, `ssh`, `remap` and `remap-reboot`. The log ends with `cloud end-to-end: PASS (boots: unseeded seeded bios access update ssh remap)`. From the two remap boots:

```
cloud-assertions: remap: the caps-tier app kept its data across a container recreate (5abd96fe2d58 -> 39e8236ccef4), token fc45ba55-d0dc-4fd5-8740-bed75b7d7012
cloud-assertions: remap-reboot: every tier checked again after a real reboot of the same disk; the caps-tier app kept its data (token fc45ba55-d0dc-4fd5-8740-bed75b7d7012)
```

Nothing was published: the Release and ghcr steps were skipped.

### Product call: no test on a real hosted box

#523's "Done when" asked for the checks on a provisioned hosted box. The maintainer decided on 2026-09-30 that the QEMU lane and the Step 0 GCE run are enough, so no real-box test was built. The lane boots the production image with host-agent-real and the real control plane. What it does not have is a real provider VM with a real seed.

### What the next release needs

The brain on `dev` calls `POST /v1/users/{username}/prepare-folder` (#522) and reads `remap_base` (#534). No tag has either, so the next release must raise the host-agent floor to its own version. Today the only floor in code is `minimumAgentVersion` in `cmd/brain/main.go`, still `"0.4.0"`. It must be raised to the new version in the release PR that bumps `VERSION`, not before. The appliance manifest's `minimum_host_agent` is not published by anything yet. The floor only reports a mismatch as a health issue; it does not stop an update, so the per-box update target is the guard for older hosted boxes. The details are in the comment on #523: https://github.com/onmoose/os/issues/523#issuecomment-5907996850

### Cleanup

The spike branch `test/516-userns-remap-ci-proofs` is deleted from `origin`. Its commit `78775a6` is named in [userns-remap-ci-proofs.md](userns-remap-ci-proofs.md) and is no longer reachable from a branch. The agent worktrees of the slices are removed.

### Docs

`NEXT.md` said the build of the remap, with proofs still to run, was #523. It now says the build is done and names this entry.

## How it maps to the specs

- `APP_ISOLATION.md` # User-namespace tiers: every tier is built and has a check on a booted box, on every full run of the lane.
- `BUILD.md` # User-namespace remap: on in both images.

## Known gaps & deviations

- **The full appliance medium lane has not passed.** It has no CI job and must run on a machine other than the one used for #530 ([userns-remap-on.md](userns-remap-on.md)).
- **The OS update must keep each box's remap setting (#486).** Nothing replaces `daemon.json` today, so it is safe now. The rule lives on #486.
- **`checkTierKept` has no caller.** The app update path is not built. When it is, it must call the check ([brain-userns-tiers.md](brain-userns-tiers.md)).
- **No run on a real provisioned hosted box**, by the product call above.

## What's next

1. Raise the host-agent floor in the next release PR, as above.
2. The three gaps above, each on its own issue or in its own entry when it is picked up.
