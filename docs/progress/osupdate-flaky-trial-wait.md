# The OS update tests wait for the end of the trial

- **Status:** done
- **Date:** 2026-11-08
- **Specs touched:** none. Test code only: `internal/hostagent/osupdate/osupdate_test.go`.

Follows [rauc-apt-https.md](rauc-apt-https.md) (#645), whose `make check` runs hit this. Two tests in `internal/hostagent/osupdate` failed now and then, also on plain `dev` (#647): `TestTrialGood` with `the trial marker is still there`, and `TestOneSwitchPerNight` with `install of the next step: {State:waiting ...}`. On one machine it failed in about 2 runs out of 3.

When a trial passes, `trial()` saves the good outcome first and removes the trial marker after. The order is on purpose: if the removal fails, the next boot finds the good outcome and only removes the marker. The trial runs in a goroutine. Both tests waited only for `Last() != nil`, so the wait could end between the save and the removal. The test then found the marker, or `Apply` still saw a trial and answered `waiting`.

## What was done

- **A `waitTrialPassed` helper** in the test file. It waits until the outcome is saved and the marker is gone. `TestTrialGood` and `TestOneSwitchPerNight` use it.
- **Two other tests already waited for both,** in their own way (`TestStaleMarkerOnAGoodSlot` and `TestTrialDeadlineCountsFromBoot`). They now use the helper too, so the next test copies the right wait.
- **Checked:** a `time.Sleep(50ms)` put between the save and `removeMarker` for one run made both tests fail every time, with the two messages above. With the helper, the same sleep passes. The sleep was not committed.
- **No product change.** In production the gap is harmless: `Apply` answers `waiting` and the update loop tries again on its next pass.

## Known gaps

- None.

## What's next

- Nothing.
