package updatetarget

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// The snapshot exists so something other than the journal can say what the loop
// decided (#443). These tests pin the one property that matters: each of the
// four outcomes is reachable and stays distinct from the others. A source that
// is down and a source stuck on a bad answer must never read the same.

func TestSnapshot_NoTickYet(t *testing.T) {
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: brainRef, ui: uiRef}, &fakeApplier{}, ptr(at(12, 3, 30)))
	s := l.Snapshot()
	if s.Outcome != "" || !s.CheckedAt.IsZero() {
		t.Fatalf("snapshot before the first tick = %+v, want the zero value", s)
	}
}

func TestSnapshot_Outcomes(t *testing.T) {
	unpinned := goodTarget()
	unpinned.BrainImage = "ghcr.io/onmoose/brain:v0.7.0"

	cases := []struct {
		name    string
		src     *fakeSource
		want    Outcome
		wantErr string // a substring of the recorded reason; "" means none
	}{
		{"a target", &fakeSource{target: goodTarget()}, OutcomeOK, ""},
		{"nothing on offer", &fakeSource{err: ErrNoTarget}, OutcomeNone, ""},
		{"source down", &fakeSource{err: errors.New("dial tcp: connection refused")}, OutcomeUnreachable, "connection refused"},
		{"a tagged answer", &fakeSource{target: unpinned}, OutcomeRefused, "not pinned"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			now := at(12, 3, 30)
			l := newLoop(c.src, fakeRunning{brain: brainRef, ui: uiRef}, &fakeApplier{}, &now)
			l.Tick(context.Background())

			s := l.Snapshot()
			if s.Outcome != c.want {
				t.Fatalf("outcome = %q, want %q", s.Outcome, c.want)
			}
			if !s.CheckedAt.Equal(now) {
				t.Errorf("checked at %v, want %v", s.CheckedAt, now)
			}
			if c.wantErr == "" {
				if s.Err != "" {
					t.Errorf("recorded reason %q, want none", s.Err)
				}
			} else if !strings.Contains(s.Err, c.wantErr) {
				t.Errorf("recorded reason %q, want it to mention %q", s.Err, c.wantErr)
			}
		})
	}
}

// A refused answer is kept, not dropped: an operator fixing a broken source
// needs to see what that source is actually serving.
func TestSnapshot_KeepsTheRefusedAnswer(t *testing.T) {
	unpinned := goodTarget()
	unpinned.BrainImage = "ghcr.io/onmoose/brain:v0.7.0"
	l := newLoop(&fakeSource{target: unpinned}, fakeRunning{brain: oldBrain, ui: oldUI}, &fakeApplier{}, ptr(at(12, 3, 30)))
	l.Tick(context.Background())

	if got := l.Snapshot().Target.BrainImage; got != unpinned.BrainImage {
		t.Fatalf("kept brain image %q, want the refused %q", got, unpinned.BrainImage)
	}
}

// The window in the snapshot is the one that would actually be used, so an
// answer that names its own outranks the box's setting here too (UPDATES.md
// # 8.4). Without this the report would show an hour the box no longer uses.
func TestSnapshot_WindowComesFromTheAnswer(t *testing.T) {
	tgt := goodTarget()
	tgt.Window = "05:00-06:00"
	l := newLoop(&fakeSource{target: tgt}, fakeRunning{brain: brainRef, ui: uiRef}, &fakeApplier{}, ptr(at(12, 3, 30)))
	l.Window, l.WindowFrom = Window{Start: 3 * time.Hour, End: 4 * time.Hour}, "env"
	l.Tick(context.Background())

	s := l.Snapshot()
	if got := s.Window.String(); got != "05:00-06:00" {
		t.Errorf("window %q, want the answer's 05:00-06:00", got)
	}
	if s.WindowFrom != fromAnswer {
		t.Errorf("window from %q, want %q", s.WindowFrom, fromAnswer)
	}
}

// A box with no answer still reports the window it is configured with, and where
// that came from. The dashboard says "installs tonight at ..." from this.
func TestSnapshot_WindowFallsBackToTheSetting(t *testing.T) {
	l := newLoop(&fakeSource{err: ErrNoTarget}, fakeRunning{brain: brainRef, ui: uiRef}, &fakeApplier{}, ptr(at(12, 3, 30)))
	l.Window, l.WindowFrom = Window{Start: 5 * time.Hour, End: 6 * time.Hour}, "env"
	l.Tick(context.Background())

	s := l.Snapshot()
	if got := s.Window.String(); got != "05:00-06:00" {
		t.Errorf("window %q, want the configured 05:00-06:00", got)
	}
	if s.WindowFrom != "env" {
		t.Errorf("window from %q, want %q", s.WindowFrom, "env")
	}
}

// An unset WindowFrom reports the built-in default rather than an empty string:
// the wire field is read by a person, and "" says nothing.
func TestSnapshot_WindowFromDefaults(t *testing.T) {
	l := newLoop(&fakeSource{err: ErrNoTarget}, fakeRunning{brain: brainRef, ui: uiRef}, &fakeApplier{}, ptr(at(12, 3, 30)))
	l.Tick(context.Background())
	if got := l.Snapshot().WindowFrom; got != fromDefault {
		t.Fatalf("window from %q, want %q", got, fromDefault)
	}
}

// The writer is the tick and the reader is an HTTP handler, so the two run at
// once on a real box. Under -race this is the test that says so.
func TestSnapshot_ConcurrentReadAndTick(t *testing.T) {
	now := at(12, 3, 30)
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: brainRef, ui: uiRef}, &fakeApplier{}, &now)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			l.Tick(context.Background())
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			_ = l.Snapshot()
		}
	}()
	wg.Wait()
}

// The update-target URL is operator-settable, so it can carry credentials, and
// the errors that name it now reach an API response as `detail` (#443). None of
// them may carry the password.
func TestRedactURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://user:secret@moose.example/target", "https://redacted@moose.example/target"},
		// A secret hides in the query as readily as in the userinfo, and this
		// is the likelier shape: a box pointed at a signed or tokenized source.
		{"https://moose.example/target?token=secret", "https://moose.example/target?redacted"},
		// The query is replaced, not deleted: "this box asks with parameters"
		// and "this box asks with none" are different situations.
		{"https://moose.example/target", "https://moose.example/target"},
		{"https://moose.example/target#secret", "https://moose.example/target"},
		{"", ""},
		{"://nope", "(unreadable URL)"},
	}
	for _, c := range cases {
		if got := RedactURL(c.in); got != c.want {
			t.Errorf("RedactURL(%q) = %q, want %q", c.in, got, c.want)
		}
		if strings.Contains(RedactURL(c.in), "secret") {
			t.Errorf("RedactURL(%q) leaked a secret", c.in)
		}
	}
}

// CauseOf is what stops the raw URL riding in on the wrapped error. Both
// standard-library paths carry one, and neither is safe: http.Client.Do strips
// the password but keeps the whole query, and url.Parse keeps everything.
func TestCauseOf(t *testing.T) {
	inner := errors.New("connection refused")
	wrapped := &url.Error{Op: "Get", URL: "https://user:secret@host/x?token=secret", Err: inner}
	if got := CauseOf(wrapped); got != inner {
		t.Fatalf("CauseOf = %v, want the inner cause", got)
	}
	if strings.Contains(CauseOf(wrapped).Error(), "secret") {
		t.Error("the unwrapped cause still carries a secret")
	}
	if got := CauseOf(inner); got != inner {
		t.Errorf("CauseOf(plain error) = %v, want it unchanged", got)
	}
}

// The redaction has to hold on the path that actually reaches the report: a
// source that cannot be reached, whose error names the URL it failed on.
func TestSnapshot_UnreachableDetailCarriesNoPassword(t *testing.T) {
	src := HTTPSource{
		URL:  "https://user:secret@127.0.0.1:1/target?token=querysecret",
		HTTP: &http.Client{Timeout: time.Second},
	}
	l := newLoop(src, fakeRunning{brain: brainRef, ui: uiRef}, &fakeApplier{}, ptr(at(12, 3, 30)))
	l.Tick(context.Background())

	s := l.Snapshot()
	if s.Outcome != OutcomeUnreachable {
		t.Fatalf("outcome = %q, want %q", s.Outcome, OutcomeUnreachable)
	}
	// Both hiding places, and the box id the source appends to the query.
	for _, leak := range []string{"secret", "querysecret"} {
		if strings.Contains(s.Err, leak) {
			t.Fatalf("the recorded reason leaks %q: %q", leak, s.Err)
		}
	}
	if !strings.Contains(s.Err, "redacted@127.0.0.1:1") {
		t.Errorf("the recorded reason = %q, want it to still name the redacted URL", s.Err)
	}
	if !strings.Contains(s.Err, "connection refused") {
		t.Errorf("the recorded reason = %q, want it to still say what went wrong", s.Err)
	}
}
