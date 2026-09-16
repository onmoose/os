// Package rampressure is host-agent's locus-B ram-pressure detector (HEALTH.md
// # Detector catalog, the ram-pressure row). It samples the kernel's Pressure
// Stall Information for memory (/proc/pressure/memory, the `some avg60` field)
// and emits a single box-wide `ram-pressure` finding when memory-stall pressure
// is sustained above the threshold. The brain reconciles it under the report's
// `resources` category and debounces it (raise on 2 consecutive bad samples).
//
// Unlike the slower-cadence clock detector, this reporter holds no cache: the
// ram-pressure cadence is 60s (HEALTH.md), which is exactly the brain's
// /v1/health/system poll interval, so every Read re-reads /proc. The detector
// reports the instantaneous PSI reading; debounce and clear-on-recover live in
// the brain (internal/health), not here.
//
// Threshold. `some avg60` is the share of the last 60s during which at least one
// task stalled waiting on memory (reclaim / swap-in / refault). On a box with
// adequate RAM it stays near zero even under heavy CPU or IO — it climbs only
// when tasks actually stall on memory. pressureThreshold = 20% is a deliberately
// conservative default: HEALTH.md says "tune at first soak", so this is a single
// named constant (one-line tuning). Combined with the brain's two-bad-sample
// debounce it raises only on ~2 minutes of sustained, real contention — the
// "swap thrashing" the issue summary describes — not on a brief reclaim spike.
//
// PSI requires `psi=1` on the kernel cmdline (BUILD.md # Kernel cmdline). Without
// it /proc/pressure/memory is absent or reads zeros and the detector silently
// never fires — that fail-open is intentional: a tooling/availability gap is not
// a health finding (chrony being down is service-down's job, a missing PSI is a
// build-config gap, neither is "the box is under memory pressure").
package rampressure

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/onmoose/moose/internal/protocol"
)

// issueRAMPressure is the registered issue ID raised under sustained pressure.
const issueRAMPressure = "ram-pressure"

// pressureThreshold is the raise threshold on PSI memory `some avg60` (percent).
// Conservative default per HEALTH.md ("sustained > threshold (tune at first
// soak)"); a single constant so first-soak tuning is one line. See the package
// doc for the rationale.
const pressureThreshold = 20.0

// psiMemoryPath is the kernel PSI memory file (requires psi=1 on the cmdline).
const psiMemoryPath = "/proc/pressure/memory"

// Reporter implements hostagent.RAMReporter. read is injectable so tests drive
// PSI state without a real /proc.
type Reporter struct {
	read func() (string, error)
}

// New returns a Reporter backed by /proc/pressure/memory.
func New() *Reporter {
	return &Reporter{read: readPSIMemory}
}

// Read returns the ram-pressure finding (or nil when pressure is at or below the
// threshold). It re-reads /proc on every call — the brain's 60s poll IS the
// sampling cadence (HEALTH.md). It always returns a usable slice; a missing or
// unparseable PSI file fails open (nil), since PSI unavailability is a kernel
// cmdline gap, not a health finding.
func (r *Reporter) Read() []protocol.Finding {
	out, err := r.read()
	if err != nil {
		// /proc/pressure/memory absent (psi=1 not set) or unreadable — fail open.
		// The detector silently never fires rather than raising on a tooling gap.
		slog.Error("rampressure: reading /proc/pressure/memory failed", "err", err)
		return nil
	}
	avg60, ok := parseSomeAvg60(out)
	if !ok {
		// Unparseable PSI should never happen on a real host; fail open (treat as
		// healthy) rather than raise on a parse error.
		slog.Error("rampressure: could not parse PSI some avg60")
		return nil
	}
	if avg60 <= pressureThreshold {
		return nil
	}
	return []protocol.Finding{{
		ID:      issueRAMPressure,
		Details: fmt.Sprintf("memory stall %.0f%% over the last 60s", avg60),
	}}
}

// parseSomeAvg60 extracts the avg60 value from the `some` line of
// /proc/pressure/memory. Format (Linux PSI):
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// We want the `some` line (any task stalled) rather than `full` (all tasks
// stalled) — the more sensitive of the two, matching HEALTH.md's `some avg60`.
// ok is false when the some/avg60 field is absent or unparseable; the caller
// fails open.
func parseSomeAvg60(out string) (float64, bool) {
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || fields[0] != "some" {
			continue
		}
		for _, f := range fields[1:] {
			if v, ok := strings.CutPrefix(f, "avg60="); ok {
				n, err := strconv.ParseFloat(v, 64)
				if err != nil {
					return 0, false
				}
				return n, true
			}
		}
	}
	return 0, false
}

// readPSIMemory reads /proc/pressure/memory.
func readPSIMemory() (string, error) {
	b, err := os.ReadFile(psiMemoryPath)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
