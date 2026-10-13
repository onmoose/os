# ram-pressure reads `full avg300` and available memory

- **Status:** done
- **Date:** 2026-10-13
- **Specs touched:** `HEALTH.md` # Capacity & informational, # Where detectors run, # Catalog — locus B (host-agent periodic)

This closes #597. [ram-pressure-detector.md](ram-pressure-detector.md) (#38) built the detector with a placeholder threshold, PSI `some avg60` above 20%, to be tuned at first soak. It never fired in the field. A hosted box with 3.8 GB of RAM and no swap ran two apps whose recommended memory added up to about 6.5 GB, and sat at about 200 MB available for 15 hours, reading `some avg60=4.88` and `full avg300=3.66`. One app took 109 s to start, and another app's backend wedged during boot and never served. This entry is that tuning.

## What was done

- **New raise signal: PSI `full avg300` at 2% or more** (`internal/hostagent/rampressure`, `fullAvg300Threshold`). `full` counts time when every runnable task stalled on memory at once, so it measures the box stalling, not one task waiting. The 5-minute average ignores short spikes such as an image pull. `some` is no longer a raise signal; its `avg300` stays in the finding's details.
- **Second raise signal: `MemAvailable` under 5% of `MemTotal`** (`availableFloor`), read from `/proc/meminfo`. The finding is raised when either signal is bad, and clears when both are back under their thresholds. The brain's debounce is unchanged: 2 bad samples raise, 1 good one clears.
- **Each signal fails open on its own.** A missing or unparseable `/proc/pressure/memory` (no `psi=1`) no longer silences the detector; the memory signal still works, and the other way round.
- **Details text.** For example: "memory stall 3.7% over the last 5 minutes (all tasks), 4.1% (some tasks); 1.6 GB free of 3.8 GB". A part whose file could not be read is left out.
- **Summary text** (`internal/health`): "The box is low on memory and is slowing down. Stop an app you don't use to free some." It used to say "Check which app is using the most", but there is no place on the box to check that. Naming the heavy apps is #623.
- **`/proc/meminfo` is parsed in `rampressure`**, not shared with `procsource`. procsource is Linux-only by build tag and rampressure is not, so sharing would make this package Linux-only for about a dozen lines.
- **Tests** (`rampressure_test.go`), against fixture files in `testdata/` in the kernel's format: the stalled box from #597 raises; the same box after recovery (`full avg300` under 1%) does not; a spike that lifts `avg10` but not `avg300` does not; an idle box does not; low available memory raises with no PSI stall. Boundaries: `full avg300` at exactly 2.00 raises and 1.99 does not; `MemAvailable` at exactly 5% does not raise and one kB under does. Parse tests for both files, the exact details text, and fail-open for each signal on its own.

## How it maps to the specs

- `HEALTH.md` # Catalog — locus B: the `ram-pressure` row names both files and both thresholds. A new note under the table records the #597 numbers as the first soak data point and says why `full avg300` was picked.
- `HEALTH.md` # Capacity & informational and # Where detectors run: the row text and the locus-B "Built" note follow the new signal.

## Known gaps & deviations

- **No hysteresis band.** `HEALTH.md` # Cross-cutting detector policy asks for different raise and clear thresholds on threshold issues. This row overrides it, as the issue asked, and the note in `HEALTH.md` says so: a 5-minute average moves slowly, and the debounce does the rest. A band would need state in the reporter, which is stateless today.
- **Only one soak data point.** The 2% and 5% values are set from one box. The constants stay one-line changes.
- **The issue does not name the heavy apps.** #597 asked for it. Nothing on the box measures memory per app, and the banner does not show the issue's details. The maintainer chose to rewrite the summary and file #623.
- **Not run on a real box.** The change is proved by unit tests against fixture files with the field numbers. No cloud lane step reads the detector, and no lane was run.
- On the #597 box, 200 MB available was just over the 5% line (about 195 MB), so the memory signal alone would not have fired there. PSI does.

## What's next

- #623: name the apps that use the most memory.
- Collect more soak readings and tune the two constants if needed.
