# host-agent reports `remap_base` on the well-known endpoint, #527

- **Status:** done
- **Date:** 2026-09-29
- **Specs touched:** `BRAIN_HOST_PROTOCOL.md` # User info endpoints

This is slice 2 of #523, after [brainlaunch-userns-host.md](brainlaunch-userns-host.md) (slice 1). It adds the number the brain will need in slice 4 (#529) to give a remapped bind dir the right host owner: the first host id of the `moose-remap` range. The spec is [userns-remap-spec.md](userns-remap-spec.md). The brain does not read the field yet, and no image has a `moose-remap` range yet, so nothing changes on a box.

## What was done

- **`internal/protocol`.** `WellKnownIdentityResponse` gains `RemapBase *int` (`remap_base`, `omitempty`). A pointer, so "absent" (no remap) is not the same as 0. It is additive: an older brain ignores the key, and a brain reading an older host-agent sees nil.
- **`internal/hostagent/usermgr`** (`remap.go`). `LinuxUserManager.RemapBase` reads the `moose-remap` line of `/etc/subuid` and of `/etc/subgid`:
  - both name the same start: that start is the base;
  - neither file has the line, or a file is missing: no base (the box runs no remap);
  - only one file has it, the two ranges differ (start or count), the range holds fewer than 65536 ids, a line is not `name:start:count`, the start or count is not above zero, or a file has two lines for `moose-remap`: an error.
  - Lines for other accounts are skipped, so a Debian user's own range (for example `alice:100000:65536`) changes nothing. A start of 0 is refused because it would map a container's root to real root.
- **`internal/hostagent`.** `UserManager` gains `RemapBase`. The real branch of `GET /v1/identity/well-known` adds the field when there is a base, and answers 500 `well-known-identity-failed` on an error, with the detail only in the log. The fake branch never sends the field, which matches the dev loop's Docker.
- **`internal/hostclient`** already decodes the whole response, so the brain gets `RemapBase` with no code change there. Its test now checks that the field is absent from a host-agent that does not send it.
- **Tests:** the parser (13 cases), `RemapBase` over real temp files (10 cases plus an unreadable file), the handler (range, no range, disagreement, the fake leaves it out), and the wire shape (absent decodes to nil, nil encodes with no key).

## How it maps to the specs

- Realizes `BRAIN_HOST_PROTOCOL.md` # User info endpoints, "The remap base". The heading lost "(specced, not built yet)", and the section now also says what counts as a malformed line and that the brain does not read the field yet.
- `APP_ISOLATION.md` # User-namespace tiers ("Data ownership follows the tier") names this field as the brain's source for `base`. That is slice 4.

## Verification

- `make check`: green.
- The hosted variant (`-tags hosted`) builds, since both wirings use `LinuxUserManager`.
- Not run in `CI / Cloud image`: the booted image has no `moose-remap` line, so the lane would only see the field absent. The unit tests cover that case over real files.

## Known gaps & deviations

- **A broken range now fails the whole well-known call.** Folder installs use this endpoint today, so on a box whose subuid and subgid disagree about `moose-remap`, a folder install fails too, not only a remapped one. That is what the spec asks for ("an error rather than a guess"). Today no image writes the line, so no box can hit it.
- **The spec's error rule is stricter here in three small ways,** all safe: two `moose-remap` lines in one file, a start or count of 0, and a range smaller than 65536 ids are errors, and the counts must match as well as the starts. The spec said only "the two must name the same start". The section now says all of it.

## Review

- **Fresh Sonnet agent:** no Block or Note findings. One Question, which only confirmed that the stricter error rules are written down in the spec and here.
- **Greptile P2, "Range counts are discarded":** confirmed and fixed. Two ranges with the same start but different counts were accepted, and a short range would leave a `base+uid` owner the container cannot map. The counts must now match, and the range must hold at least 65536 ids (`minRemapCount`). Tests cover both.

## What's next

1. #528: the `root_setup` manifest field and its admission rules.
2. #529: the brain reads `remap_base`, checks `docker info` for `name=userns`, and picks the tiers.
