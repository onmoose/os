package updatetarget

import "time"

// Outcome is what one tick produced. The four values are the three ways the
// Source contract can end (target, ErrNoTarget, any other error) plus the one
// the box itself produces: an answer it read and then refused.
//
// They stay apart because they are four different things to tell an operator. A
// source that is down and a source stuck on a bad answer look the same if this
// is flattened, and only one of them is a fleet-wide problem.
type Outcome string

const (
	// OutcomeOK is an answer that was read and passed Validate.
	OutcomeOK Outcome = "ok"
	// OutcomeNone is ErrNoTarget: the source is fine and has nothing to offer.
	OutcomeNone Outcome = "none"
	// OutcomeUnreachable is any other Source error, plus the case where the box
	// could not read its own running pair. The box could not ask, so it stays
	// where it is.
	OutcomeUnreachable Outcome = "unreachable"
	// OutcomeRefused is an answer Validate rejected. Nothing was pulled.
	OutcomeRefused Outcome = "refused"
)

// Snapshot is what the last tick decided, for a reader outside the loop.
//
// The loop used to keep all of this to itself and write it only to the journal
// (UPDATES.md # 8.4). That is enough for a hosted box, which applies its own
// target, and not enough for an appliance, where # 3 promises an admin prompt
// that had nothing to read. This is what the host socket serves.
//
// **The running pair is deliberately not in here.** A tick that ends early —
// no target, an unreachable source, a refusal — never reads it, and host-agent
// outlives a control-plane update, so a pair stored here would be stale exactly
// after an apply. The reader reads it fresh instead.
type Snapshot struct {
	// Outcome is the last tick's result. The zero value means no tick has
	// finished yet.
	Outcome Outcome
	// Target is the answer, set only when Outcome is OutcomeOK. It is also kept
	// for OutcomeRefused, where it is the answer that was rejected — the
	// version and refs in it are what an operator needs to see to fix the
	// source, and nothing acts on them.
	Target Target
	// Err is why the tick produced no usable target: the source error for
	// OutcomeUnreachable, the validation error for OutcomeRefused. Empty
	// otherwise.
	Err string
	// CheckedAt is when the tick ran. Zero until the first one finishes.
	CheckedAt time.Time
	// Window is the update window that was in force for this tick, and
	// WindowFrom where it came from (UPDATES.md # 8.4 — "answer", "env" or
	// "default"). An answer that names a window outranks the box's own setting,
	// so this is the resolved value, not the configured one.
	Window     Window
	WindowFrom string
	// OS is stream A's last decision. It is kept across a tick that ends
	// before the OS part is read (an unreachable source), so it is the last
	// decision, not this tick's.
	OS OSSnapshot
}

// OSSnapshot is stream A's last decision. State is one of the
// protocol.OSUpdate* values; the zero value means stream A has not been
// decided yet.
type OSSnapshot struct {
	State  string
	Detail string
	// Target is the release the box picked, when it picked one.
	Target *OSRelease
}

// recordOS stores stream A's decision for this tick.
func (l *Loop) recordOS(o OSSnapshot) {
	l.snapMu.Lock()
	defer l.snapMu.Unlock()
	l.snap.OS = o
}

// Snapshot returns the last tick's decision. Safe to call from another
// goroutine: the writer is the tick, the reader is an HTTP handler.
func (l *Loop) Snapshot() Snapshot {
	l.snapMu.Lock()
	defer l.snapMu.Unlock()
	return l.snap
}

// record stores this tick's decision. err may be nil.
func (l *Loop) record(o Outcome, t Target, w Window, from string, err error) {
	s := Snapshot{Outcome: o, Target: t, CheckedAt: l.now(), Window: w, WindowFrom: from}
	if err != nil {
		s.Err = err.Error()
	}
	l.snapMu.Lock()
	defer l.snapMu.Unlock()
	s.OS = l.snap.OS
	l.snap = s
}
