// Package clockhealth is host-agent's locus-B clock-not-synced detector
// (HEALTH.md # Detector catalog, the clock-not-synced row; TIME.md # Drift
// monitoring). It runs `chronyc tracking`, parses the last-sync time and the
// current offset, and emits a single `clock-not-synced` finding when chrony has
// not synced in over 6h OR the offset exceeds 10s. The brain reconciles it under
// the report's `time` category and debounces it (raise on 2 consecutive bad
// samples).
//
// chrony is queried at most once every 5 minutes (TIME.md: a relaxed cadence for
// a slow-moving signal); the brain polls /v1/health/system more often (60s), so
// Read returns the cached finding between samples. The detector reports the
// instantaneous state — debounce and clear-on-recover live in the brain
// (internal/health), not here.
package clockhealth

import (
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// issueClockNotSynced is the registered issue ID raised when the clock is off.
const issueClockNotSynced = "clock-not-synced"

// Raise thresholds, fixed by spec (TIME.md # Drift monitoring, HEALTH.md # Detector
// catalog): raise when the last sync is older than maxSyncAge OR the offset
// exceeds maxOffset. sampleInterval is the relaxed re-query cadence.
const (
	maxSyncAge     = 6 * time.Hour
	maxOffset      = 10 * time.Second
	sampleInterval = 5 * time.Minute
)

// Reporter implements hostagent.ClockReporter. runTracking and now are
// injectable so tests drive chrony state and the clock without a real chronyd.
type Reporter struct {
	mu          sync.Mutex
	now         func() time.Time
	runTracking func() (string, error)
	interval    time.Duration

	sampledAt  time.Time
	cached     []protocol.Finding
	haveSample bool
}

// New returns a Reporter backed by real `chronyc tracking`, re-querying chrony at
// most once every 5 minutes.
func New() *Reporter {
	return &Reporter{
		now:         time.Now,
		runTracking: runChronycTracking,
		interval:    sampleInterval,
	}
}

// Read returns the clock-not-synced finding (or nil when the clock is healthy).
// chrony is re-queried only when the cached sample is older than the interval;
// otherwise the cached result is returned, so the brain's 60s poll doesn't exec
// chronyc on every tick. It always returns a usable slice — an out-of-sync clock
// is data, not an error.
func (r *Reporter) Read() []protocol.Finding {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.now()
	if r.haveSample && now.Sub(r.sampledAt) < r.interval {
		return cloneFindings(r.cached)
	}

	out, err := r.runTracking()
	if err != nil {
		// chrony unreachable/uninstalled is service-down's job (chrony.service is
		// on its allowlist) — fail open here rather than raise a clock issue on a
		// tooling error. Keep any prior sample and don't advance the timer, so the
		// next poll retries.
		slog.Error("clockhealth: chronyc tracking failed", "err", err)
		return cloneFindings(r.cached)
	}

	t := parseTracking(out)
	if !t.ok {
		// Unparseable output should never happen for real chronyc (chrony answered,
		// but in a shape we don't understand — almost certainly a format change).
		// Hold the prior sample rather than asserting "healthy": discarding to nil
		// is the only path that would silently clear an active clock-not-synced on
		// one bad sample. Still advance the timer so a persistent bad parse backs
		// off to the 5-min cadence instead of hot-looping chronyc every poll.
		slog.Error("clockhealth: could not parse chronyc tracking output")
	} else {
		r.cached = evaluate(t, now)
	}
	r.sampledAt = now
	r.haveSample = true
	return cloneFindings(r.cached)
}

// tracking is the parsed subset of `chronyc tracking` the detector needs.
type tracking struct {
	refTime time.Time     // "Ref time (UTC)" — when chrony last updated from a source
	offset  time.Duration // |"Last offset"| as a duration
	ok      bool          // false when either field could not be parsed
}

// evaluate turns a parsed tracking into findings against the raise thresholds.
// Assumes t.ok.
func evaluate(t tracking, now time.Time) []protocol.Finding {
	age := now.Sub(t.refTime)
	if age <= maxSyncAge && t.offset <= maxOffset {
		return nil // synced recently and offset small → healthy
	}
	return []protocol.Finding{{
		ID:      issueClockNotSynced,
		Details: raiseReason(age, t.offset, t.refTime),
	}}
}

// raiseReason builds a short, plain-English advisory detail for the banner body
// (reaches the dashboard via GET /api/v1/health, so no Go-duration formatting).
func raiseReason(age, offset time.Duration, refTime time.Time) string {
	var parts []string
	switch {
	case refTime.Year() < 2000:
		parts = append(parts, "clock has never synced")
	case age > maxSyncAge:
		parts = append(parts, "last synced "+humanizeHoursAgo(age))
	}
	if offset > maxOffset {
		parts = append(parts, "off by about "+humanizeSeconds(offset))
	}
	if len(parts) == 0 {
		return "clock not synced"
	}
	return strings.Join(parts, "; ")
}

// humanizeHoursAgo renders an over-threshold sync age as plain English. The age
// only reaches here above the 6h threshold, so hours is the only unit needed.
func humanizeHoursAgo(age time.Duration) string {
	h := int(age.Round(time.Hour) / time.Hour)
	if h <= 1 {
		return "over an hour ago"
	}
	return fmt.Sprintf("about %d hours ago", h)
}

// humanizeSeconds renders an offset (always over the 10s threshold here) in whole
// seconds, e.g. "12 seconds".
func humanizeSeconds(offset time.Duration) string {
	s := int(offset.Round(time.Second) / time.Second)
	if s == 1 {
		return "1 second"
	}
	return fmt.Sprintf("%d seconds", s)
}

// parseTracking extracts the Ref time and Last offset from `chronyc tracking`
// output. chrony keys never contain a colon, so the first ':' on a line is the
// key/value separator (the time's own colons come after it). ok is true only
// when both fields parsed — anything else fails open at the call site.
func parseTracking(out string) tracking {
	var t tracking
	var foundRef, foundOffset bool
	for _, line := range strings.Split(out, "\n") {
		i := strings.IndexByte(line, ':')
		if i < 0 {
			continue
		}
		key := strings.TrimSpace(line[:i])
		val := strings.TrimSpace(line[i+1:])
		switch {
		case strings.HasPrefix(key, "Ref time"):
			if ts, err := parseRefTime(val); err == nil {
				t.refTime = ts
				foundRef = true
			}
		case key == "Last offset":
			if d, err := parseSecondsField(val); err == nil {
				t.offset = absDuration(d)
				foundOffset = true
			}
		}
	}
	t.ok = foundRef && foundOffset
	return t
}

// parseRefTime parses chrony's "Ref time" value, printed via strftime
// "%a %b %d %H:%M:%S %Y" in UTC, e.g. "Fri Jun 05 12:34:56 2026".
func parseRefTime(v string) (time.Time, error) {
	for _, layout := range []string{"Mon Jan 02 15:04:05 2006", "Mon Jan _2 15:04:05 2006"} {
		if ts, err := time.Parse(layout, v); err == nil {
			return ts.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("clockhealth: unrecognized ref time %q", v)
}

// parseSecondsField parses a "<float> seconds" value (e.g. "-0.000045 seconds")
// into a duration.
func parseSecondsField(v string) (time.Duration, error) {
	fields := strings.Fields(v)
	if len(fields) == 0 {
		return 0, fmt.Errorf("clockhealth: empty seconds field")
	}
	f, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, err
	}
	return time.Duration(f * float64(time.Second)), nil
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func cloneFindings(in []protocol.Finding) []protocol.Finding {
	if in == nil {
		return nil
	}
	out := make([]protocol.Finding, len(in))
	copy(out, in)
	return out
}

// runChronycTracking runs `chronyc tracking` and returns its stdout.
func runChronycTracking() (string, error) {
	out, err := exec.Command("chronyc", "tracking").Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}
