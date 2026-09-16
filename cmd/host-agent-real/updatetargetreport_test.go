package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/moose/internal/hostagent/updatetarget"
	"github.com/onmoose/moose/internal/protocol"
)

// The report is the whole point of #443: the loop's decision, readable by
// something other than the journal. These tests pin the seven states, because
// the states are the contract — a dashboard that cannot tell "nothing on offer"
// from "we could not ask" tells the user the wrong thing in the case that
// matters.

// The four references these tests move between, in the shape a real answer
// carries: a repository, then a full 64-hex digest.
var (
	runningBrain = "ghcr.io/onmoose/brain@" + digest("a")
	runningUI    = "ghcr.io/onmoose/ui@" + digest("b")
	targetBrain  = "ghcr.io/onmoose/brain@" + digest("c")
	targetUI     = "ghcr.io/onmoose/ui@" + digest("d")
)

func digest(hexDigit string) string { return "sha256:" + strings.Repeat(hexDigit, 64) }

type stubRunning struct {
	brain, ui string
	err       error
}

func (s stubRunning) Running() (string, string, error) { return s.brain, s.ui, s.err }

type stubSource struct {
	target updatetarget.Target
	err    error
}

func (s stubSource) Target(context.Context) (updatetarget.Target, error) { return s.target, s.err }

// tickedLoop returns a loop that has run exactly one tick against src.
func tickedLoop(t *testing.T, src updatetarget.Source, running updatetarget.RunningPair) *updatetarget.Loop {
	t.Helper()
	l := &updatetarget.Loop{Source: src, Current: running, Profile: "hosted"}
	l.Tick(context.Background())
	return l
}

func goodOffer() updatetarget.Target {
	return updatetarget.Target{
		Version:     "v0.8.0",
		BrainImage:  targetBrain,
		UIImage:     targetUI,
		PublishedAt: time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC),
	}
}

func TestReport_NoLoopIsDisabled(t *testing.T) {
	r := updateTargetReport{
		disabledErr: `seed update_target_url has no host: "moose.example"`,
		running:     stubRunning{brain: runningBrain, ui: runningUI},
		window:      updatetarget.DefaultWindow,
		windowFrom:  "default",
	}
	got := r.Read()

	// The distinction this state exists for: a box that will never update must
	// not look like a box that has not checked yet.
	if got.State != protocol.UpdateTargetDisabled {
		t.Fatalf("state = %q, want %q", got.State, protocol.UpdateTargetDisabled)
	}
	if !strings.Contains(got.Detail, "no host") {
		t.Errorf("detail = %q, want the reason the target was refused", got.Detail)
	}
	// A box that will not update itself still says what it is running.
	if got.Running.Brain != runningBrain {
		t.Errorf("running brain = %q, want %q", got.Running.Brain, runningBrain)
	}
	if got.CheckedAt != "" {
		t.Errorf("checked_at = %q, want empty — no check ever ran", got.CheckedAt)
	}
}

func TestReport_LoopWithNoTickYetIsUnknown(t *testing.T) {
	l := &updatetarget.Loop{Source: stubSource{err: updatetarget.ErrNoTarget}}
	r := updateTargetReport{loop: l, running: stubRunning{brain: runningBrain, ui: runningUI}}

	if got := r.Read().State; got != protocol.UpdateTargetUnknown {
		t.Fatalf("state = %q, want %q", got, protocol.UpdateTargetUnknown)
	}
}

func TestReport_States(t *testing.T) {
	unpinned := goodOffer()
	unpinned.BrainImage = "ghcr.io/onmoose/brain:v0.8.0"

	cases := []struct {
		name    string
		src     updatetarget.Source
		running updatetarget.RunningPair
		want    string
	}{
		{
			name:    "a target the box is not on",
			src:     stubSource{target: goodOffer()},
			running: stubRunning{brain: runningBrain, ui: runningUI},
			want:    protocol.UpdateTargetAvailable,
		},
		{
			name:    "a target the box is already on",
			src:     stubSource{target: goodOffer()},
			running: stubRunning{brain: targetBrain, ui: targetUI},
			want:    protocol.UpdateTargetCurrent,
		},
		{
			name:    "nothing on offer",
			src:     stubSource{err: updatetarget.ErrNoTarget},
			running: stubRunning{brain: runningBrain, ui: runningUI},
			want:    protocol.UpdateTargetNone,
		},
		{
			name:    "a source that is down",
			src:     stubSource{err: errors.New("dial tcp: connection refused")},
			running: stubRunning{brain: runningBrain, ui: runningUI},
			want:    protocol.UpdateTargetUnreachable,
		},
		{
			name:    "an answer the box refused",
			src:     stubSource{target: unpinned},
			running: stubRunning{brain: runningBrain, ui: runningUI},
			want:    protocol.UpdateTargetRefused,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := updateTargetReport{loop: tickedLoop(t, c.src, c.running), running: c.running}
			got := r.Read()
			if got.State != c.want {
				t.Fatalf("state = %q, want %q (detail %q)", got.State, c.want, got.Detail)
			}
			if got.CheckedAt == "" {
				t.Error("checked_at is empty after a tick")
			}
		})
	}
}

// The offer is on the wire for the two states that have one, and it is always
// the pinned reference — never a version string turned back into a tag.
func TestReport_AvailableCarriesThePinnedPair(t *testing.T) {
	running := stubRunning{brain: runningBrain, ui: runningUI}
	r := updateTargetReport{loop: tickedLoop(t, stubSource{target: goodOffer()}, running), running: running}
	got := r.Read()

	if got.Target == nil {
		t.Fatal("no target on an available report")
	}
	if got.Target.BrainImage != targetBrain || got.Target.UIImage != targetUI {
		t.Errorf("target pair = %q / %q, want %q / %q", got.Target.BrainImage, got.Target.UIImage, targetBrain, targetUI)
	}
	if got.Target.Version != "v0.8.0" {
		t.Errorf("version = %q, want v0.8.0", got.Target.Version)
	}
	if got.Target.PublishedAt != "2026-09-01T10:00:00Z" {
		t.Errorf("published_at = %q, want RFC3339 UTC", got.Target.PublishedAt)
	}
}

// A source with nothing to offer names no images, so the report carries no
// target at all. An empty offer object would read as "a target with no images".
func TestReport_NoneCarriesNoTarget(t *testing.T) {
	running := stubRunning{brain: runningBrain, ui: runningUI}
	r := updateTargetReport{loop: tickedLoop(t, stubSource{err: updatetarget.ErrNoTarget}, running), running: running}
	if got := r.Read(); got.Target != nil {
		t.Fatalf("target = %+v, want none", got.Target)
	}
}

// The running pair is read when the request arrives, not carried from the tick.
// A tick that ended early never read it, and host-agent outlives an update, so a
// stored pair would be stale exactly after one.
func TestReport_RunningIsReadPerRequest(t *testing.T) {
	running := &movingPair{brain: runningBrain, ui: runningUI}
	// The tick sees the old pair and offers a different one.
	r := updateTargetReport{loop: tickedLoop(t, stubSource{target: goodOffer()}, running), running: running}
	if got := r.Read().State; got != protocol.UpdateTargetAvailable {
		t.Fatalf("state = %q, want %q before the update lands", got, protocol.UpdateTargetAvailable)
	}
	// The update lands. No new tick has run, and the report must still be right.
	running.brain, running.ui = targetBrain, targetUI
	if got := r.Read().State; got != protocol.UpdateTargetCurrent {
		t.Fatalf("state = %q, want %q once the box is on the target", got, protocol.UpdateTargetCurrent)
	}
}

type movingPair struct {
	brain, ui string
}

func (p *movingPair) Running() (string, string, error) { return p.brain, p.ui, nil }

// A box that cannot read its own declaration cannot say whether a target is a
// change, so it says so rather than guessing.
func TestReport_UnreadableRunningPair(t *testing.T) {
	running := stubRunning{err: errors.New("read control-plane compose: no such file")}
	r := updateTargetReport{loop: tickedLoop(t, stubSource{target: goodOffer()}, running), running: running}
	got := r.Read()

	if got.State != protocol.UpdateTargetUnreachable {
		t.Fatalf("state = %q, want %q", got.State, protocol.UpdateTargetUnreachable)
	}
	if !strings.Contains(got.Detail, "no such file") {
		t.Errorf("detail = %q, want the read failure", got.Detail)
	}
}

// The two "from" settings are separate facts with lookalike values, and merging
// them is the mistake this asserts against: a URL can come from the seed and a
// window never can, a window can come from the answer and a URL never can.
func TestReport_TheTwoFromsStayApart(t *testing.T) {
	tgt := goodOffer()
	tgt.Window = "05:00-06:00"
	running := stubRunning{brain: runningBrain, ui: runningUI}
	l := &updatetarget.Loop{
		Source: stubSource{target: tgt}, Current: running,
		Window: updatetarget.DefaultWindow, WindowFrom: "env",
	}
	l.Tick(context.Background())

	got := updateTargetReport{
		loop: l, running: running, from: "seed",
		window: updatetarget.DefaultWindow, windowFrom: "env",
		profile: "hosted", autoApply: true,
	}.Read()

	if got.From != "seed" {
		t.Errorf("from = %q, want the target URL's source %q", got.From, "seed")
	}
	if got.WindowFrom != "answer" {
		t.Errorf("window_from = %q, want the answer's %q", got.WindowFrom, "answer")
	}
	if got.Window != "05:00-06:00" {
		t.Errorf("window = %q, want the answer's 05:00-06:00", got.Window)
	}
	if !got.AutoApply || got.Profile != "hosted" {
		t.Errorf("auto_apply/profile = %v/%q, want true/hosted", got.AutoApply, got.Profile)
	}
}

// A box can have two faults at once: a seeded target it refused at startup, and
// a declaration it cannot read. Only the first is why this box will never
// update, so it wins the state — reporting the read failure instead would hide
// a permanent fault behind one that looks transient. The second is not dropped.
func TestReport_DisabledOutranksAnUnreadablePair(t *testing.T) {
	got := updateTargetReport{
		disabledErr: `seed update_target_url has no host: "moose.example"`,
		running:     stubRunning{err: errors.New("read control-plane compose: no such file")},
	}.Read()

	if got.State != protocol.UpdateTargetDisabled {
		t.Fatalf("state = %q, want %q", got.State, protocol.UpdateTargetDisabled)
	}
	if !strings.Contains(got.Detail, "no host") {
		t.Errorf("detail = %q, want the reason the box has no update loop", got.Detail)
	}
	if !strings.Contains(got.Detail, "no such file") {
		t.Errorf("detail = %q, want the read failure carried along too", got.Detail)
	}
}

// The refused answer is on the wire, and it may name a tag — naming one is a
// way to get refused. State is what says whether a target is applicable.
func TestReport_RefusedTargetIsNotApplicable(t *testing.T) {
	unpinned := goodOffer()
	unpinned.BrainImage = "ghcr.io/onmoose/brain:v0.8.0"
	running := stubRunning{brain: runningBrain, ui: runningUI}
	got := updateTargetReport{loop: tickedLoop(t, stubSource{target: unpinned}, running), running: running}.Read()

	if got.State != protocol.UpdateTargetRefused {
		t.Fatalf("state = %q, want %q", got.State, protocol.UpdateTargetRefused)
	}
	if got.Target == nil || got.Target.BrainImage != unpinned.BrainImage {
		t.Fatalf("target = %+v, want the refused answer verbatim", got.Target)
	}
}
