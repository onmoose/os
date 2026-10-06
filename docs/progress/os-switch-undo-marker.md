# A failed switch undo keeps the trial marker

- **Status:** done
- **Date:** 2026-10-06
- **Specs touched:** `docs/specs/UPDATES.md`

A fix to [host-agent-os-update.md](host-agent-os-update.md) (#563), part of #486. Greptile found it on the release PR for 0.16.0 (#583).

## What was done

- **The bug.** When an `os-switch` job stops after `rauc status mark-active other` (for example, the grubenv check finds `TRY=1`, or the record cannot be saved), it undoes the switch: it puts the booted slot first again and removes the new slot's trial marker. It removed the marker even when putting the booted slot first failed. Then the new slot could still boot next, with no trial marker, so no safety net and no revert, and host-agent would mark it good.
- **The fix** (`internal/hostagent/osupdate/osupdate.go`, `doSwitch`): the marker is removed only after the booted slot is first again. If that fails, the marker stays and host-agent logs it. If the old slot boots after all, `Boot` already removes a marker for a switch that never took effect (`TestMarkerWithoutActivationIsNoRevert`).
- **Test:** `TestFailedUndoKeepsTheTrialMarker` makes the grubenv check fail and the undo's mark-active fail, and wants the marker kept. It fails without the fix.
- `UPDATES.md` # 1 says it in the switch step.

## What's next

Nothing new. The release of 0.16.0 (#583) goes on after this lands.

## Known gaps

- Only unit-tested: making two RAUC calls fail in a row in a booted image needs a test hook that the boot lane does not have, and it is not worth one for this path.
