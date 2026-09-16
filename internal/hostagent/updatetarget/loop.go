package updatetarget

import (
	"context"
	"errors"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"time"

	"github.com/onmoose/moose/internal/hostagent/controlplane"
)

// PollInterval is how often the box asks its source what it should be running.
//
// It is deliberately **shorter than the update window**. The window is an hour
// wide, so an hourly poll could land at 02:58 and again at 04:01 and step
// straight over it, and the box would then wait a full day for a target it
// already knew about. Four ticks per window is enough that jitter, a slow
// source, or a clock adjustment cannot swallow all of them.
const PollInterval = 15 * time.Minute

// pollJitter spreads each tick, for the same reason relmanifest's poll does:
// boxes cluster after a provider maintenance window or a regional power cut,
// and the whole fleet is pointed at one endpoint.
const pollJitter = 2 * time.Minute

// Applier starts the control-plane update transaction. Consumer-side interface
// (CLAUDE.md # Go code discipline); the provider is *hostagent.Agent, which
// starts the same job POST /v1/jobs/system-update starts.
//
// **It must go through host-agent's job lock, not straight to cpupdate.** An
// admin-triggered update and a target-driven one are the same dangerous
// operation, and two of them at once on one box is the thing that lock exists to
// prevent. A refusal here ("a job is already running") is an ordinary outcome,
// not a failure of this loop.
type Applier interface {
	// StartUpdate begins an update to the given refs and returns the job id. The
	// job runs in the background: a nil error means the transaction started, not
	// that it succeeded.
	StartUpdate(brainRef, uiRef string) (jobID string, err error)
}

// RunningPair reports the control-plane images the box is running right now, so
// the loop can tell "the target moved" from "nothing to do".
type RunningPair interface {
	Running() (brain, ui string, err error)
}

// LedgerPair reads the running pair from the box's own declaration: the ledger
// for the brain, the staged compose for the UI (UPDATES.md # 8.3 — the two files
// that carry the pair). It is the same pair cpupdate reverts to, read from the
// same place, so the loop's comparison and the transaction's cannot drift.
type LedgerPair struct {
	// Dir is MOOSE_CONTROL_PLANE_DIR.
	Dir string
	// BrainDefault is the ref this box shipped with (MOOSE_BRAIN_IMAGE), used
	// before the first update has written a ledger.
	BrainDefault string
}

// Running reports the declared pair.
func (l LedgerPair) Running() (brain, ui string, err error) {
	brain, _ = controlplane.ResolveBrainImage(l.Dir, l.BrainDefault)
	ui, err = controlplane.ReadUIImage(l.Dir)
	if err != nil {
		return "", "", err
	}
	return brain, ui, nil
}

// Loop is the **single consumer of the Source seam**: it asks, compares,
// validates, and applies inside the window. Both profiles run this one loop; all
// that differs is the Source they are given and whether they may apply without
// asking.
type Loop struct {
	// Source answers what this box should be running.
	Source Source
	// Current reports what it is running.
	Current RunningPair
	// Applier starts the transaction. Nil means the loop can only observe,
	// which is also what AutoApply=false means.
	Applier Applier
	// Repos is the expected home of each image; the zero value means
	// DefaultRepositories.
	Repos Repositories
	// Window is when an update may start, unless the source names one of its
	// own. The zero value means DefaultWindow.
	Window Window
	// WindowFrom says where Window came from, carried only so the logs can say
	// what an answer's window replaced. One of the `from` values in
	// UPDATES.md # 8.4.
	WindowFrom string
	// AutoApply is the # 8.2 difference between the profiles. Hosted is true:
	// we operate the box, so it patches itself and the tenant is told
	// afterwards. Appliance is false: the control plane is admin-prompted
	// (# 3), so the box learns its target and waits for a human.
	AutoApply bool
	// Profile is the environment profile this loop runs on (ENVIRONMENT.md),
	// carried only so the logs say which one made a decision.
	Profile string
	// Interval between polls; zero means PollInterval.
	Interval time.Duration
	// Now and After exist so a test can drive a week of polling in
	// milliseconds. Both default to the real clock.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time

	// lastQuiet dedupes the repeated messages. The overwhelmingly common tick is
	// "the target is what I am already running", and that must cost nothing and
	// say nothing. The same applies to a state that persists: a box whose source
	// has been down for a week, or whose appliance manifest can never be pinned,
	// says so once per change and not four times an hour forever.
	lastQuiet string
	// lastWindow dedupes the window lines. It is separate memory from
	// lastQuiet: the window and the target change independently, and a target
	// that moves every night must not keep re-announcing a window that never
	// moved.
	lastWindow string
	// attempted is the target version this loop last started an update for, and
	// the night it did it in. One attempt per night per version: a failed
	// update reverts the box, so the next tick sees the same difference again,
	// and without this the box would retry a bad target every quarter of an
	// hour until the window closed.
	//
	// **night, not the occurrence's exact start instant.** The window can now
	// change on every tick (an answer can move it at any poll, UPDATES.md #
	// 8.4), so comparing exact start timestamps breaks: a same-night change to
	// the window's start moves the occurrence's timestamp without moving its
	// calendar date, and comparing timestamps would read that as "a later
	// occurrence" and let the same version be retried twice in one night. The
	// date is stable across such a shift; it only advances on a genuinely new
	// night. See occurrenceNight.
	attempted struct {
		version string
		night   time.Time
	}

	// snap is what the last tick decided, for a reader outside the loop
	// (state.go). snapMu guards it: the writer is the tick, the reader is the
	// host socket's HTTP handler.
	snapMu sync.Mutex
	snap   Snapshot
}

// occurrenceNight is the calendar date (local midnight) of the window
// occurrence that contains t, using w's own wrap-past-midnight rollback
// (Window.Occurrence) so a window spanning local midnight (e.g. "23:30-00:30")
// still counts as one night, not two.
//
// This exists instead of comparing w.Occurrence(t) directly because w can
// change between ticks: the answer can move the window's start on any poll.
// The occurrence's exact start instant moves with it, but its calendar date
// does not, as long as the shift keeps the occurrence on the same night. That
// is what makes the one-attempt-per-night guard in Tick immune to a same-night
// window nudge while still opening back up on the next real night.
func occurrenceNight(w Window, t time.Time) time.Time {
	start := w.Occurrence(t)
	return time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *Loop) window() Window {
	if l.Window == (Window{}) {
		return DefaultWindow
	}
	return l.Window
}

func (l *Loop) repos() Repositories {
	if l.Repos == (Repositories{}) {
		return DefaultRepositories
	}
	return l.Repos
}

func (l *Loop) interval() time.Duration {
	if l.Interval > 0 {
		return l.Interval
	}
	return PollInterval
}

func (l *Loop) after(d time.Duration) <-chan time.Time {
	if l.After != nil {
		return l.After(d)
	}
	return time.After(d)
}

func (l *Loop) nextDelay() time.Duration {
	base := l.interval()
	j := pollJitter
	if j > base/2 {
		j = base / 2 // keep a short test interval from going negative
	}
	d := base + time.Duration(rand.Int63n(int64(2*j)+1)) - j
	if d < time.Second {
		d = time.Second
	}
	return d
}

// fromAnswer is the `from` value for a window the source named. The other
// values are host-agent's (cmd/host-agent-real/updateconfig.go); this one lives
// here because this package is where an answer's window is read.
const fromAnswer = "answer"

// fromDefault is what an unset WindowFrom reports. A loop wired without one
// (a test, or a caller that never read a setting) is on the built-in window, so
// that is what the snapshot says rather than an empty string.
const fromDefault = "default"

// windowFrom is WindowFrom with the built-in default filled in.
func (l *Loop) windowFrom() string {
	if l.WindowFrom == "" {
		return fromDefault
	}
	return l.WindowFrom
}

// windowFor picks the window for this tick.
//
// **The answer wins when it carries one.** When to update is per-box policy the
// control plane owns, and it has to be changeable while the box runs
// (UPDATES.md # 8.1). An answer with no window is "no opinion", not "use the
// default": treating it as the default would silently outrank the operator's own
// MOOSE_UPDATE_WINDOW.
//
// A window the box cannot read warns and falls back. That is deliberately unlike
// a bad target, which stops the loop: a wrong hour can only apply an update at
// the wrong time, while a wrong target sends the box to the wrong version.
func (l *Loop) windowFor(t Target) (Window, string) {
	if strings.TrimSpace(t.Window) == "" {
		return l.window(), l.windowFrom()
	}
	w, err := ParseWindow(t.Window)
	if err != nil {
		l.sayWindow("bad:"+t.Window, slog.LevelWarn,
			"update target: the window in the answer is not readable; keeping the configured one",
			"err", err, "window", l.window().String(), "from", l.windowFrom())
		return l.window(), l.windowFrom()
	}
	l.sayWindow("answer:"+w.String(), slog.LevelInfo,
		"update target: taking the update window from the answer",
		"window", w.String(), "from", fromAnswer)
	return w, fromAnswer
}

// sayWindow logs a change in the window, once per change.
func (l *Loop) sayWindow(key string, level slog.Level, msg string, args ...any) {
	if l.lastWindow == key {
		return
	}
	l.lastWindow = key
	slog.Log(context.Background(), level, msg, args...)
}

// quiet logs msg only when this tick's state differs from the last one.
func (l *Loop) quiet(level slog.Level, key, msg string, args ...any) {
	if l.lastQuiet == key {
		return
	}
	l.lastQuiet = key
	slog.Log(context.Background(), level, msg, args...)
}

// Tick runs one cycle: ask, compare, and apply if everything lines up. It never
// returns an error — there is no caller to hand one to, and none of the ways
// this can go wrong is a reason for the box to do anything but keep running what
// it runs.
func (l *Loop) Tick(ctx context.Context) {
	t, err := l.Source.Target(ctx)
	switch {
	case errors.Is(err, ErrNoTarget):
		// A source with nothing to offer is not a broken source, and it is
		// certainly not "there is nothing to run". Distinguishable from the
		// unreachable case below, which is the point.
		l.record(OutcomeNone, Target{}, l.window(), l.windowFrom(), nil)
		l.quiet(slog.LevelInfo, "no-target", "update target: the source has no target; staying on the current version")
		return
	case err != nil:
		// Unreachable, a 500, an unparseable answer, or an appliance manifest that
		// can never be pinned. Log it and carry on: a box must never degrade
		// because it could not ask.
		//
		// Deduped by the error text, because these states persist. A box behind a
		// dead link, or an appliance whose manifest names versions, would otherwise
		// write the same warning four times an hour for as long as it runs, and
		// bury the tick where something actually changed.
		l.record(OutcomeUnreachable, Target{}, l.window(), l.windowFrom(), err)
		l.quiet(slog.LevelWarn, "err:"+err.Error(),
			"update target: could not read the source; staying on the current version", "err", err)
		return
	}

	// Untrusted input, validated before anything is pulled. A refusal is loud
	// and repeated: an operator needs to see that a box has been declining its
	// target since Tuesday, and a source stuck on a bad answer is a fleet-wide
	// problem, not a quiet one.
	if err := t.Validate(l.repos()); err != nil {
		// Deduped on the answer, not silenced: a source stuck on a bad answer says
		// so once, and says so again the moment the answer changes. Error level,
		// because unlike an unreachable source this is a box being handed
		// something it must not act on.
		//
		// The refused answer is kept in the snapshot. Nothing acts on it — the
		// version and refs are what an operator needs to see to fix the source.
		l.record(OutcomeRefused, t, l.window(), l.windowFrom(), err)
		l.quiet(slog.LevelError, "refused:"+t.Version+":"+t.BrainImage+":"+t.UIImage,
			"update target: refusing the answer; nothing pulled, box unchanged",
			"err", err, "brain", t.Version, "image", t.BrainImage)
		return
	}

	// The window is resolved here, before the compare, not further down on the
	// apply path. An answer's window outranks the box's setting the moment the
	// answer is read (UPDATES.md # 8.4), so a box that is already current still
	// reports the hour it would update in. The log line is deduped, so resolving
	// it every tick costs nothing.
	w, windowFrom := l.windowFor(t)

	brain, ui, err := l.Current.Running()
	if err != nil {
		// The box could not read its own declaration, so it cannot tell whether
		// the answer is a change. The answer itself was fine, so the snapshot
		// keeps it and reports the read failure alongside.
		//
		// One tick writes one snapshot. Recording the good answer here and
		// overwriting it below would publish an intermediate "all fine" state
		// that a concurrent reader can catch.
		l.record(OutcomeUnreachable, t, w, windowFrom, err)
		l.quiet(slog.LevelWarn, "running-err:"+err.Error(),
			"update target: cannot read what this box is running", "err", err)
		return
	}
	l.record(OutcomeOK, t, w, windowFrom, nil)
	if brain == t.BrainImage && ui == t.UIImage {
		// The overwhelmingly common case. No pull, no work, and one line the
		// first time it is true.
		l.quiet(slog.LevelInfo, "current:"+t.Version, "update target: already on the target version", "brain", t.Version)
		return
	}

	if !l.AutoApply || l.Applier == nil {
		// Appliance: the box now knows its target and an admin decides
		// (UPDATES.md # 3). Surfacing that as a dashboard prompt is a separate
		// slice; this is where the fact enters the box.
		l.quiet(slog.LevelInfo, "available:"+t.Version, "update target: a different control plane is available",
			"brain", t.Version, "image", t.BrainImage)
		return
	}

	now := l.now()
	if !w.Contains(now) {
		l.quiet(slog.LevelInfo, "holding:"+t.Version, "update target: holding a new version for the update window",
			"brain", t.Version, "step", "waiting", "zone", now.Location().String())
		return
	}
	if night := occurrenceNight(w, now); l.attempted.version == t.Version && !night.After(l.attempted.night) {
		// Already tried this version tonight. If it failed it has already put
		// the box back, and the next window is soon enough to try again.
		return
	}

	l.attempted.version, l.attempted.night = t.Version, occurrenceNight(w, now)
	l.lastQuiet = ""
	jobID, err := l.Applier.StartUpdate(t.BrainImage, t.UIImage)
	if err != nil {
		// Includes the ordinary "a job is already running" refusal, which is why
		// this is a Warn and not an Error.
		slog.Warn("update target: could not start the update", "err", err, "brain", t.Version)
		return
	}
	slog.Info("update target: applying", "brain", t.Version, "image", t.BrainImage,
		"step", "accepted", "job_id", jobID)
}

// Run ticks until ctx is done: once at the start, then on the jittered interval.
//
// The first tick is immediate so a box that has been off for a week learns its
// target as soon as it is up, but it still only **applies** inside the window,
// so booting at noon does not restart the control plane at noon.
func (l *Loop) Run(ctx context.Context) {
	slog.Info("update target: polling", "interval", l.interval().String(), "profile", l.Profile)
	for {
		l.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-l.after(l.nextDelay()):
		}
	}
}
