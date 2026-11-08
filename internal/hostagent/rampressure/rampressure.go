// Package rampressure is host-agent's locus-B ram-pressure detector (HEALTH.md
// # Detector catalog, the ram-pressure row). It reads two kernel files on each
// call and emits a single box-wide `ram-pressure` finding when either signal is
// over its threshold. The brain reconciles it under the report's `resources`
// category and debounces it (raise on 2 consecutive bad samples, clear on 1
// good one).
//
// This reporter holds no cache: the ram-pressure cadence is 60s (HEALTH.md),
// which is exactly the brain's /v1/health/system poll interval, so every Read
// re-reads /proc. The detector reports the reading as it is now; debounce and
// clear-on-recover live in the brain (internal/health), not here.
//
// The two signals (#597):
//
//   - PSI `full avg300` from /proc/pressure/memory, at or above
//     fullAvg300Threshold. `full` is the share of time in which every runnable
//     task was stalled on memory at once, so it measures the box itself
//     stalling, not one task waiting. The 5-minute average ignores short
//     spikes such as an image pull or a backup. The first version read
//     `some avg60` against 20% and never fired on a box that was stalled for
//     15 hours at `full avg300` 3.66% (HEALTH.md # Detector catalog has the
//     numbers). `some` stays in the finding's details only.
//   - MemAvailable under availableFloor of MemTotal, from /proc/meminfo. This
//     catches a box that is close to the edge before it starts to stall.
//
// The finding clears when both signals are back under their thresholds. The
// thresholds are named constants, so tuning after more soak data is one line.
//
// Each signal fails open on its own. PSI needs `psi=1` on the kernel cmdline
// (BUILD.md # Kernel cmdline); without it /proc/pressure/memory is absent or
// reads zeros and only the MemAvailable signal can fire. A file that cannot be
// read or parsed is a tooling gap, not a health finding, so it never raises.
package rampressure

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"

	"github.com/onmoose/os/internal/protocol"
)

// issueRAMPressure is the registered issue ID raised under sustained pressure.
const issueRAMPressure = "ram-pressure"

// fullAvg300Threshold is the raise threshold on PSI memory `full avg300`
// (percent). Raise is at or above it.
const fullAvg300Threshold = 2.0

// availableFloor is the raise threshold on MemAvailable as a share of MemTotal.
// Raise is strictly under it.
const availableFloor = 0.05

const (
	psiMemoryPath = "/proc/pressure/memory" // requires psi=1 on the cmdline
	meminfoPath   = "/proc/meminfo"
)

// Reporter implements hostagent.RAMReporter. The read functions are injectable
// so tests drive the kernel files without a real /proc.
type Reporter struct {
	readPSI     func() (string, error)
	readMeminfo func() (string, error)
}

// New returns a Reporter backed by /proc/pressure/memory and /proc/meminfo.
func New() *Reporter {
	return &Reporter{
		readPSI:     func() (string, error) { return readFile(psiMemoryPath) },
		readMeminfo: func() (string, error) { return readFile(meminfoPath) },
	}
}

// Read returns the ram-pressure finding, or nil when both signals are healthy
// or could not be read. It always returns a usable slice.
func (r *Reporter) Read() []protocol.Finding {
	psi, psiOK := r.psi()
	mem, memOK := r.meminfo()

	stalled := psiOK && psi.fullAvg300 >= fullAvg300Threshold
	low := memOK && float64(mem.availableKB) < availableFloor*float64(mem.totalKB)
	if !stalled && !low {
		return nil
	}

	var parts []string
	if psiOK {
		parts = append(parts, fmt.Sprintf("memory stall %.1f%% over the last 5 minutes (all tasks), %.1f%% (some tasks)", psi.fullAvg300, psi.someAvg300))
	}
	if memOK {
		parts = append(parts, fmt.Sprintf("%s free of %s", formatKB(mem.availableKB), formatKB(mem.totalKB)))
	}
	return []protocol.Finding{{
		ID:      issueRAMPressure,
		Details: strings.Join(parts, "; "),
	}}
}

// psi reads and parses /proc/pressure/memory. ok is false when the file is
// missing or unparseable, and the signal then fails open.
func (r *Reporter) psi() (psiMemory, bool) {
	out, err := r.readPSI()
	if err != nil {
		// Absent when psi=1 is not set: a build-config gap, not a finding.
		slog.Error("rampressure: reading /proc/pressure/memory failed", "err", err)
		return psiMemory{}, false
	}
	p, ok := parsePSI(out)
	if !ok {
		slog.Error("rampressure: could not parse /proc/pressure/memory")
	}
	return p, ok
}

// meminfo reads and parses /proc/meminfo. ok is false when the file is missing
// or unparseable, and the signal then fails open.
func (r *Reporter) meminfo() (memInfo, bool) {
	out, err := r.readMeminfo()
	if err != nil {
		slog.Error("rampressure: reading /proc/meminfo failed", "err", err)
		return memInfo{}, false
	}
	m, ok := parseMeminfo(out)
	if !ok {
		slog.Error("rampressure: could not parse MemTotal and MemAvailable from /proc/meminfo")
	}
	return m, ok
}

// psiMemory holds the fields of /proc/pressure/memory the detector uses.
type psiMemory struct {
	someAvg300 float64 // reported detail only
	fullAvg300 float64 // the raise signal
}

// parsePSI extracts `some avg300` and `full avg300` from /proc/pressure/memory.
// Format (Linux PSI):
//
//	some avg10=0.00 avg60=0.00 avg300=0.00 total=0
//	full avg10=0.00 avg60=0.00 avg300=0.00 total=0
//
// ok is false unless both lines carry a parseable avg300.
func parsePSI(out string) (psiMemory, bool) {
	var p psiMemory
	var haveSome, haveFull bool
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		var dst *float64
		switch fields[0] {
		case "some":
			dst, haveSome = &p.someAvg300, true
		case "full":
			dst, haveFull = &p.fullAvg300, true
		default:
			continue
		}
		v, ok := fieldValue(fields[1:], "avg300=")
		if !ok {
			return psiMemory{}, false
		}
		*dst = v
	}
	return p, haveSome && haveFull
}

// fieldValue finds key=value among fields and parses the value as a float.
func fieldValue(fields []string, key string) (float64, bool) {
	for _, f := range fields {
		if v, ok := strings.CutPrefix(f, key); ok {
			n, err := strconv.ParseFloat(v, 64)
			return n, err == nil
		}
	}
	return 0, false
}

// memInfo holds the two /proc/meminfo lines the detector uses, in kB.
type memInfo struct {
	totalKB     int64
	availableKB int64
}

// parseMeminfo reads MemTotal and MemAvailable from /proc/meminfo. procsource
// parses the same two lines, but it is Linux-only by build tag and this package
// is not, so the dozen lines are repeated here rather than tie the two
// together. ok is false unless both are present, parseable, and MemTotal is
// above zero.
func parseMeminfo(out string) (memInfo, bool) {
	var m memInfo
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		var dst *int64
		switch fields[0] {
		case "MemTotal:":
			dst, haveTotal = &m.totalKB, true
		case "MemAvailable:":
			dst, haveAvail = &m.availableKB, true
		default:
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return memInfo{}, false
		}
		*dst = v
	}
	return m, haveTotal && haveAvail && m.totalKB > 0
}

// formatKB renders a /proc/meminfo kB figure as MB or GB for the details text.
func formatKB(kb int64) string {
	mb := float64(kb) / 1024
	if mb < 1024 {
		return fmt.Sprintf("%.0f MB", mb)
	}
	return fmt.Sprintf("%.1f GB", mb/1024)
}

func readFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
