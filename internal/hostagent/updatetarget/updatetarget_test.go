package updatetarget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/hostagent/controlplane"
	"github.com/onmoose/os/internal/hostagent/relmanifest"
)

// The four references these tests move between. Written out through digest()
// so the shape — repository, then a full 64-hex digest — is the same one a real
// answer carries.
var (
	brainRef = "ghcr.io/onmoose/brain@" + digest("a")
	uiRef    = "ghcr.io/onmoose/ui@" + digest("b")
	oldBrain = "ghcr.io/onmoose/brain@" + digest("c")
	oldUI    = "ghcr.io/onmoose/ui@" + digest("d")
)

func digest(hexDigit string) string { return "sha256:" + strings.Repeat(hexDigit, 64) }

// --- fakes -----------------------------------------------------------------

type fakeSource struct {
	target Target
	err    error
	calls  int
}

func (s *fakeSource) Target(context.Context) (Target, error) {
	s.calls++
	return s.target, s.err
}

type fakeRunning struct {
	brain, ui string
	err       error
}

func (r fakeRunning) Running() (string, string, error) { return r.brain, r.ui, r.err }

// fakeApplier records what it was asked to apply. It does NOT change what
// fakeRunning reports: the transaction is asynchronous on a real box, so the
// loop must be correct without assuming the pair moved by the next tick.
type fakeApplier struct {
	calls [][2]string
	err   error
}

func (a *fakeApplier) StartUpdate(brainRef, uiRef string) (string, error) {
	a.calls = append(a.calls, [2]string{brainRef, uiRef})
	if a.err != nil {
		return "", a.err
	}
	return fmt.Sprintf("j_%d", len(a.calls)), nil
}

type fakePoller struct{ state relmanifest.State }

func (p fakePoller) State() relmanifest.State { return p.state }

// goodTarget is a well-formed answer: pinned, both images, digests agreeing.
func goodTarget() Target {
	return Target{
		Version:     "v0.7.0",
		BrainImage:  brainRef,
		BrainDigest: digest("a"),
		UIImage:     uiRef,
		UIDigest:    digest("b"),
		PublishedAt: time.Date(2026, 8, 12, 10, 0, 0, 0, time.UTC),
	}
}

func ptr(t time.Time) *time.Time { return &t }

// at builds a local time inside or outside the default 03:00–04:00 window.
func at(day, hour, min int) time.Time {
	return time.Date(2026, 8, day, hour, min, 0, 0, time.Local)
}

// newLoop wires a loop with the given source and running pair, in auto-apply
// mode. The clock is read through the pointer, so a test can move time between
// ticks on one loop — which is the only way to exercise "held now, applied at
// 03:00" and "already tried tonight".
func newLoop(src Source, running RunningPair, ap *fakeApplier, now *time.Time) *Loop {
	return &Loop{
		Source:    src,
		Current:   running,
		Applier:   ap,
		AutoApply: true,
		Profile:   "hosted",
		Now:       func() time.Time { return *now },
	}
}

// --- the loop --------------------------------------------------------------

func TestTick_NoChangeAppliesNothing(t *testing.T) {
	ap := &fakeApplier{}
	// The box is already running exactly what the source names.
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: brainRef, ui: uiRef}, ap, ptr(at(12, 3, 30)))
	l.Tick(context.Background())
	l.Tick(context.Background())
	if len(ap.calls) != 0 {
		t.Fatalf("applied %v, want nothing on a box already at the target", ap.calls)
	}
}

func TestTick_ChangeIsApplied(t *testing.T) {
	ap := &fakeApplier{}
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, ptr(at(12, 3, 30)))
	l.Tick(context.Background())
	if len(ap.calls) != 1 {
		t.Fatalf("applied %v, want exactly one update", ap.calls)
	}
	// The pinned references are handed over verbatim: nothing composes a ref of
	// its own, and nothing resolves a tag.
	if got, want := ap.calls[0], [2]string{brainRef, uiRef}; got != want {
		t.Errorf("applied %v, want %v", got, want)
	}
}

func TestTick_UnreachableSourceIsANoOp(t *testing.T) {
	ap := &fakeApplier{}
	src := &fakeSource{err: errors.New("dial tcp: connection refused")}
	l := newLoop(src, fakeRunning{brain: oldBrain, ui: oldUI}, ap, ptr(at(12, 3, 30)))
	l.Tick(context.Background())
	if len(ap.calls) != 0 {
		t.Fatalf("applied %v, want nothing when the source cannot be reached", ap.calls)
	}
}

func TestTick_NoTargetIsANoOp(t *testing.T) {
	ap := &fakeApplier{}
	l := newLoop(&fakeSource{err: ErrNoTarget}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, ptr(at(12, 3, 30)))
	l.Tick(context.Background())
	if len(ap.calls) != 0 {
		t.Fatalf("applied %v, want nothing when the source has no target", ap.calls)
	}
}

// A bad answer must not reach a pull. Each case here differs from goodTarget in
// exactly one way.
func TestTick_RefusesABadAnswer(t *testing.T) {
	tests := []struct {
		name  string
		mutet func(*Target)
	}{
		{"a tag instead of a digest", func(x *Target) {
			x.BrainImage, x.BrainDigest = "ghcr.io/onmoose/brain:v0.7.0", ""
		}},
		{"a truncated digest", func(x *Target) {
			x.BrainImage, x.BrainDigest = "ghcr.io/onmoose/brain@sha256:670b07b1", ""
		}},
		{"only the brain, no ui", func(x *Target) { x.UIImage, x.UIDigest = "", "" }},
		{"only the ui, no brain", func(x *Target) { x.BrainImage, x.BrainDigest = "", "" }},
		{"another repository", func(x *Target) {
			x.BrainImage = "ghcr.io/attacker/brain@" + digest("a")
		}},
		{"a digest that contradicts its own reference", func(x *Target) {
			x.BrainDigest = digest("e")
		}},
		{"an uppercase digest", func(x *Target) {
			x.BrainImage, x.BrainDigest = "ghcr.io/onmoose/brain@"+digest("A"), ""
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			target := goodTarget()
			tc.mutet(&target)
			ap := &fakeApplier{}
			l := newLoop(&fakeSource{target: target}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, ptr(at(12, 3, 30)))
			l.Tick(context.Background())
			if len(ap.calls) != 0 {
				t.Fatalf("applied %v, want a refusal with nothing pulled", ap.calls)
			}
		})
	}
}

func TestTick_HoldsUntilTheWindow(t *testing.T) {
	ap := &fakeApplier{}
	now := at(12, 14, 0) // the middle of the afternoon
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	l.Tick(context.Background())
	if len(ap.calls) != 0 {
		t.Fatalf("applied %v outside the window, want the box held until 03:00", ap.calls)
	}
	// 03:10 the next morning.
	now = at(13, 3, 10)
	l.Tick(context.Background())
	if len(ap.calls) != 1 {
		t.Fatalf("applied %v, want the held update to start once the window opened", ap.calls)
	}
}

// --- where the window comes from -------------------------------------------

// configured stands in for the box's own MOOSE_UPDATE_WINDOW.
func configured(l *Loop, s string) {
	w, err := ParseWindow(s)
	if err != nil {
		panic(err)
	}
	l.Window, l.WindowFrom = w, "env"
}

// The answer wins. When to update is per-box policy the control plane owns and
// can change while the box runs (UPDATES.md # 8.1).
func TestTick_TheWindowInTheAnswerWins(t *testing.T) {
	ap := &fakeApplier{}
	tgt := goodTarget()
	tgt.Window = "12:00-13:00"
	// It is half past noon, and the box is set to update in the small hours.
	l := newLoop(&fakeSource{target: tgt}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, ptr(at(12, 12, 30)))
	configured(l, "03:00-04:00")
	l.Tick(context.Background())
	if len(ap.calls) != 1 {
		t.Fatalf("applied %v, want one update — the answer's window is now open", ap.calls)
	}
}

// An answer with no window is "no opinion", never "use the default". If it were
// read as the default it would silently outrank the operator's own setting.
func TestTick_NoWindowInTheAnswerKeepsTheConfiguredOne(t *testing.T) {
	ap := &fakeApplier{}
	now := ptr(at(12, 12, 30))
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, now)
	configured(l, "12:45-13:00")
	l.Tick(context.Background())
	if len(ap.calls) != 0 {
		t.Fatalf("applied %v at 12:30, want nothing — the configured window is still shut", ap.calls)
	}
	*now = at(12, 12, 50)
	l.Tick(context.Background())
	if len(ap.calls) != 1 {
		t.Fatalf("applied %v at 12:50, want one update inside the configured window", ap.calls)
	}
}

// A window the box cannot read warns and falls back. It is not fatal: a wrong
// hour can only apply an update at the wrong time, while a wrong target sends the
// box to the wrong version.
func TestTick_AnUnreadableWindowInTheAnswerFallsBack(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	ap := &fakeApplier{}
	tgt := goodTarget()
	tgt.Window = "tonight-ish"
	l := newLoop(&fakeSource{target: tgt}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, ptr(at(12, 12, 30)))
	// The box's own window is open now. The built-in default is not, so an apply
	// proves the fall-back went to the configured window and not to the default.
	configured(l, "12:00-13:00")
	for range 3 {
		l.Tick(context.Background())
	}
	if len(ap.calls) != 1 {
		t.Fatalf("applied %v, want one update — a bad window must not stop the box updating", ap.calls)
	}
	if got := strings.Count(buf.String(), "window in the answer is not readable"); got != 1 {
		t.Errorf("warned %d times about the same bad window, want 1:\n%s", got, buf.String())
	}
}

func TestTick_OneAttemptPerWindow(t *testing.T) {
	ap := &fakeApplier{}
	now := at(12, 3, 5)
	// The pair never moves: this is a box whose update failed and was reverted,
	// so every later tick sees the same difference again.
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	for _, m := range []int{5, 20, 35, 50} {
		now = at(12, 3, m)
		l.Tick(context.Background())
	}
	if len(ap.calls) != 1 {
		t.Fatalf("started %d updates in one window, want 1", len(ap.calls))
	}
	// The next night it tries again — the failure may have been a network blip.
	now = at(13, 3, 5)
	l.Tick(context.Background())
	if len(ap.calls) != 2 {
		t.Fatalf("started %d updates over two windows, want 2", len(ap.calls))
	}
}

// A window that moves within the same night must not look like a new night.
// The answer can move a box's window on any poll (UPDATES.md # 8.4), so the
// one-attempt-per-night guard has to survive that: the same version, still
// failing, still reverted, must not be retried twice before the sun comes up
// just because the control plane nudged the hour in between.
func TestTick_AWindowThatMovesWithinTheNightDoesNotDoubleApply(t *testing.T) {
	ap := &fakeApplier{}
	tgt := goodTarget()
	tgt.Window = "12:00-13:00"
	now := at(12, 12, 5)
	src := &fakeSource{target: tgt}
	l := newLoop(src, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	l.Tick(context.Background())
	if len(ap.calls) != 1 {
		t.Fatalf("started %d updates on the first tick, want 1", len(ap.calls))
	}

	// Same version, same night, later start — a plausible control-plane nudge,
	// not a new day.
	later := goodTarget()
	later.Window = "12:10-13:00"
	src.target = later
	now = at(12, 12, 20)
	l.Tick(context.Background())
	if len(ap.calls) != 1 {
		t.Fatalf("started %d updates after the window moved within the same night, want still 1", len(ap.calls))
	}
}

// The companion case: a version that is still failing does get retried the
// next night, exactly as TestTick_OneAttemptPerWindow already proves for a
// window that never changes. This one keeps a moved window in play across the
// day boundary, so a fix for the case above cannot solve it by making the
// guard too sticky to ever open again.
func TestTick_SameVersionNextNightStillAppliesWithAMovedWindow(t *testing.T) {
	ap := &fakeApplier{}
	tgt := goodTarget()
	tgt.Window = "12:00-13:00"
	now := at(12, 12, 5)
	src := &fakeSource{target: tgt}
	l := newLoop(src, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	l.Tick(context.Background())
	if len(ap.calls) != 1 {
		t.Fatalf("started %d updates on the first tick, want 1", len(ap.calls))
	}

	// A different night, and the window has moved again since. Still the same
	// version: the guard must open back up regardless.
	later := goodTarget()
	later.Window = "12:10-13:00"
	src.target = later
	now = at(13, 12, 20)
	l.Tick(context.Background())
	if len(ap.calls) != 2 {
		t.Fatalf("started %d updates the next night, want 2", len(ap.calls))
	}
}

func TestTick_ANewVersionIsTriedInTheSameWindow(t *testing.T) {
	ap := &fakeApplier{}
	src := &fakeSource{target: goodTarget()}
	now := at(12, 3, 5)
	l := newLoop(src, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	l.Tick(context.Background())

	// The fleet is moved to a fixed version half an hour later; the box must not
	// sit out the rest of the window because of the version that failed.
	next := goodTarget()
	next.Version = "v0.7.1"
	next.BrainImage = "ghcr.io/onmoose/brain@" + digest("f")
	next.BrainDigest = digest("f")
	src.target = next
	now = at(12, 3, 35)
	l.Tick(context.Background())

	if len(ap.calls) != 2 {
		t.Fatalf("started %d updates, want the second version tried in the same window", len(ap.calls))
	}
}

func TestTick_ApplianceNeverAppliesOnItsOwn(t *testing.T) {
	ap := &fakeApplier{}
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, ptr(at(12, 3, 30)))
	// UPDATES.md # 3: on appliance the control plane is admin-prompted.
	l.AutoApply = false
	l.Profile = "appliance"
	l.Tick(context.Background())
	if len(ap.calls) != 0 {
		t.Fatalf("applied %v, want an appliance to wait for an admin", ap.calls)
	}
}

func TestTick_UnreadableRunningPairIsANoOp(t *testing.T) {
	ap := &fakeApplier{}
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{err: errors.New("no compose file")}, ap, ptr(at(12, 3, 30)))
	l.Tick(context.Background())
	if len(ap.calls) != 0 {
		t.Fatalf("applied %v, want nothing when the box cannot read what it runs", ap.calls)
	}
}

func TestTick_ARefusedStartIsRetriedTheNextNight(t *testing.T) {
	ap := &fakeApplier{err: errors.New("a job is already running")}
	now := at(12, 3, 30)
	l := newLoop(&fakeSource{target: goodTarget()}, fakeRunning{brain: oldBrain, ui: oldUI}, ap, &now)
	l.Tick(context.Background())
	now = at(13, 3, 30)
	l.Tick(context.Background())
	if len(ap.calls) != 2 {
		t.Fatalf("attempted %d starts, want one per window even after a refusal", len(ap.calls))
	}
}

// --- the hosted source -----------------------------------------------------

func TestHTTPSource_ReadsTheAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Unknown fields are part of the case: the sender may carry more than
		// the box models, and that must not break the read.
		fmt.Fprintf(w, `{
		  "version": "v0.6.0",
		  "channel": "stable",
		  "brain_image": %q,
		  "brain_digest": %q,
		  "ui_image": %q,
		  "ui_digest": %q,
		  "published_at": "2026-08-12T10:00:00Z",
		  "something_we_have_never_heard_of": {"nested": true}
		}`, brainRef, digest("a"), uiRef, digest("b"))
	}))
	defer srv.Close()

	got, err := HTTPSource{URL: srv.URL}.Target(context.Background())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if got.Version != "v0.6.0" || got.BrainImage != brainRef || got.UIImage != uiRef {
		t.Fatalf("Target = %+v", got)
	}
	if err := got.Validate(DefaultRepositories); err != nil {
		t.Errorf("Validate: %v", err)
	}
}

// The box says who it is, so the control plane can answer for this box and not
// for the fleet (UPDATES.md # 8.1).
func TestHTTPSource_SendsTheBoxID(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.RawQuery
		fmt.Fprintf(w, `{"version":"v1","brain_image":%q,"ui_image":%q}`, brainRef, uiRef)
	}))
	defer srv.Close()

	if _, err := (HTTPSource{URL: srv.URL, BoxID: "cindy-fox"}).Target(context.Background()); err != nil {
		t.Fatalf("Target: %v", err)
	}
	if got != "box_id=cindy-fox" {
		t.Fatalf("query = %q, want box_id=cindy-fox", got)
	}
}

// A box with no identity sends NO parameter, not an empty one. An empty
// box_id= names a box called nothing, and an appliance box must ask exactly what
// it asked before boxes had an identity at all.
func TestHTTPSource_NoBoxIDSendsNoParameter(t *testing.T) {
	var raw string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw = r.URL.String()
		fmt.Fprintf(w, `{"version":"v1","brain_image":%q,"ui_image":%q}`, brainRef, uiRef)
	}))
	defer srv.Close()

	if _, err := (HTTPSource{URL: srv.URL + "/target"}).Target(context.Background()); err != nil {
		t.Fatalf("Target: %v", err)
	}
	if raw != "/target" {
		t.Fatalf("asked %q, want a bare /target with no query at all", raw)
	}
}

// A box pointed at a URL that already carries a query keeps it.
func TestHTTPSource_BoxIDJoinsAnExistingQuery(t *testing.T) {
	var q url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.Query()
		fmt.Fprintf(w, `{"version":"v1","brain_image":%q,"ui_image":%q}`, brainRef, uiRef)
	}))
	defer srv.Close()

	src := HTTPSource{URL: srv.URL + "/target?channel=candidate", BoxID: "cindy-fox"}
	if _, err := src.Target(context.Background()); err != nil {
		t.Fatalf("Target: %v", err)
	}
	if q.Get("channel") != "candidate" || q.Get("box_id") != "cindy-fox" {
		t.Fatalf("query = %v, want both channel and box_id", q)
	}
}

// The window is optional on the wire. Present, it is carried through raw; absent,
// it stays empty, which the loop reads as "no opinion".
func TestHTTPSource_ReadsTheWindow(t *testing.T) {
	body := `{"version":"v1","brain_image":%q,"ui_image":%q%s}`
	for _, tc := range []struct{ name, extra, want string }{
		{"a window in the answer", `,"window":"01:00-02:00"`, "01:00-02:00"},
		{"no window field", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, body, brainRef, uiRef, tc.extra)
			}))
			defer srv.Close()
			got, err := HTTPSource{URL: srv.URL}.Target(context.Background())
			if err != nil {
				t.Fatalf("Target: %v", err)
			}
			if got.Window != tc.want {
				t.Fatalf("Window = %q, want %q", got.Window, tc.want)
			}
		})
	}
}

func TestHTTPSource_404IsNoTarget(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"no release on this channel"}`, http.StatusNotFound)
	}))
	defer srv.Close()

	_, err := HTTPSource{URL: srv.URL}.Target(context.Background())
	if !errors.Is(err, ErrNoTarget) {
		t.Fatalf("err = %v, want ErrNoTarget — an empty channel is not a failure", err)
	}
}

func TestHTTPSource_ServerErrorsAndJunk(t *testing.T) {
	tests := []struct {
		name    string
		handler http.HandlerFunc
	}{
		{"a 500", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusInternalServerError) }},
		{"not json", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>proxy error</html>") }},
		{"a body larger than the cap", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, strings.Repeat("x", maxBodyBytes+1))
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(tc.handler)
			defer srv.Close()
			_, err := HTTPSource{URL: srv.URL}.Target(context.Background())
			if err == nil {
				t.Fatal("Target: want an error")
			}
			if errors.Is(err, ErrNoTarget) {
				t.Fatalf("err = %v, want a failure distinguishable from ErrNoTarget", err)
			}
		})
	}
}

func TestHTTPSource_UnreachableHost(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing is listening now

	_, err := HTTPSource{URL: url}.Target(context.Background())
	if err == nil || errors.Is(err, ErrNoTarget) {
		t.Fatalf("err = %v, want a plain failure", err)
	}
}

// --- the appliance source --------------------------------------------------

func TestManifestSource(t *testing.T) {
	t.Run("no poller is no target", func(t *testing.T) {
		_, err := ManifestSource{}.Target(context.Background())
		if !errors.Is(err, ErrNoTarget) {
			t.Fatalf("err = %v, want ErrNoTarget", err)
		}
	})
	t.Run("no verified manifest is no target", func(t *testing.T) {
		_, err := ManifestSource{Poller: fakePoller{}}.Target(context.Background())
		if !errors.Is(err, ErrNoTarget) {
			t.Fatalf("err = %v, want ErrNoTarget", err)
		}
	})
	t.Run("a manifest names versions, which is a refusal", func(t *testing.T) {
		p := fakePoller{state: relmanifest.State{
			HasManifest: true,
			Manifest:    relmanifest.Manifest{Brain: "1.4.2", UI: "1.4.2"},
		}}
		_, err := ManifestSource{Poller: p}.Target(context.Background())
		if !errors.Is(err, ErrNotPinned) {
			t.Fatalf("err = %v, want ErrNotPinned — the manifest carries no digest", err)
		}
	})
}

// --- validation ------------------------------------------------------------

func TestValidate_AcceptsAPinnedPair(t *testing.T) {
	if err := goodTarget().Validate(DefaultRepositories); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_EmptyRepositoryMeansNoRepositoryCheck(t *testing.T) {
	// A box pointed at a test registry (the boot proof) still gets the digest
	// check; only the repository assertion is relaxed.
	target := goodTarget()
	target.BrainImage = "localhost:5000/brain@" + digest("a")
	if err := target.Validate(Repositories{UI: DefaultRepositories.UI}); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// --- the running pair ------------------------------------------------------

func TestLedgerPair(t *testing.T) {
	dir := t.TempDir()
	compose := "services:\n  " + controlplane.UIServiceName + ":\n    image: " + oldUI + "\n"
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}

	// Before the first update there is no ledger, so the brain is whatever this
	// box shipped with.
	brain, ui, err := LedgerPair{Dir: dir, BrainDefault: oldBrain}.Running()
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if brain != oldBrain || ui != oldUI {
		t.Fatalf("Running = %q, %q; want the shipped brain and the compose UI", brain, ui)
	}

	// After one, the ledger wins: it records what the box applied.
	if err := controlplane.WriteLedger(dir, controlplane.Ledger{Current: controlplane.Pair{Brain: brainRef, UI: uiRef}}); err != nil {
		t.Fatal(err)
	}
	brain, _, err = LedgerPair{Dir: dir, BrainDefault: oldBrain}.Running()
	if err != nil {
		t.Fatalf("Running: %v", err)
	}
	if brain != brainRef {
		t.Fatalf("Running brain = %q, want the applied ref %q", brain, brainRef)
	}
}

// --- the window ------------------------------------------------------------

func TestParseWindow(t *testing.T) {
	tests := []struct {
		in      string
		want    Window
		wantErr bool
	}{
		{in: "", want: DefaultWindow},
		{in: "03:00-04:00", want: Window{Start: 3 * time.Hour, End: 4 * time.Hour}},
		{in: "23:30-00:30", want: Window{Start: 23*time.Hour + 30*time.Minute, End: 30 * time.Minute}},
		{in: "3-4", wantErr: true},
		{in: "03:00", wantErr: true},
		{in: "25:00-26:00", wantErr: true},
		{in: "03:00-03:00", wantErr: true},
	}
	for _, tc := range tests {
		got, err := ParseWindow(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseWindow(%q) = %v, want an error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ParseWindow(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

func TestWindowContains(t *testing.T) {
	w := DefaultWindow
	for _, tc := range []struct {
		hour, min int
		want      bool
	}{
		{2, 59, false}, {3, 0, true}, {3, 59, true}, {4, 0, false}, {14, 0, false},
	} {
		if got := w.Contains(at(12, tc.hour, tc.min)); got != tc.want {
			t.Errorf("Contains(%02d:%02d) = %v, want %v", tc.hour, tc.min, got, tc.want)
		}
	}

	wrap := Window{Start: 23 * time.Hour, End: time.Hour}
	for _, tc := range []struct {
		hour int
		want bool
	}{
		{22, false}, {23, true}, {0, true}, {1, false},
	} {
		if got := wrap.Contains(at(12, tc.hour, 0)); got != tc.want {
			t.Errorf("wrapping Contains(%02d:00) = %v, want %v", tc.hour, got, tc.want)
		}
	}
}

func TestWindowOccurrence(t *testing.T) {
	w := DefaultWindow
	if a, b := w.Occurrence(at(12, 3, 5)), w.Occurrence(at(12, 3, 55)); !a.Equal(b) {
		t.Errorf("two times in one window gave %v and %v, want one occurrence", a, b)
	}
	if a, b := w.Occurrence(at(12, 3, 5)), w.Occurrence(at(13, 3, 5)); !b.After(a) {
		t.Errorf("tonight %v is not after last night %v", b, a)
	}

	// A wrapping window that opened before midnight is still one occurrence.
	wrap := Window{Start: 23 * time.Hour, End: time.Hour}
	if a, b := wrap.Occurrence(at(12, 23, 30)), wrap.Occurrence(at(13, 0, 30)); !a.Equal(b) {
		t.Errorf("across midnight gave %v and %v, want one occurrence", a, b)
	}
}

// --- logging ---------------------------------------------------------------

// A state that persists must be said once, not on every tick. An appliance whose
// manifest can never be pinned, or a box behind a dead link, ticks four times an
// hour for as long as it runs.
func TestTick_APersistentFailureIsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	src := &fakeSource{err: errors.New("dial tcp: connection refused")}
	l := newLoop(src, fakeRunning{brain: oldBrain, ui: oldUI}, &fakeApplier{}, ptr(at(12, 3, 30)))
	for range 5 {
		l.Tick(context.Background())
	}
	if got := strings.Count(buf.String(), "could not read the source"); got != 1 {
		t.Errorf("logged the same unreachable source %d times, want 1", got)
	}

	// A different failure is news, and says so.
	src.err = errors.New("HTTP 500")
	l.Tick(context.Background())
	if got := strings.Count(buf.String(), "could not read the source"); got != 2 {
		t.Errorf("a changed failure logged %d lines in total, want 2", got)
	}
}

func TestTick_ARefusalIsLoggedPerAnswer(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(prev)

	bad := goodTarget()
	bad.BrainImage, bad.BrainDigest = "ghcr.io/onmoose/brain:v0.7.0", ""
	src := &fakeSource{target: bad}
	l := newLoop(src, fakeRunning{brain: oldBrain, ui: oldUI}, &fakeApplier{}, ptr(at(12, 3, 30)))
	for range 3 {
		l.Tick(context.Background())
	}
	if got := strings.Count(buf.String(), "refusing the answer"); got != 1 {
		t.Errorf("logged the same refusal %d times, want 1", got)
	}

	// The source is fixed. The box must notice on the very next tick.
	src.target = goodTarget()
	l.Tick(context.Background())
	if !strings.Contains(buf.String(), "applying") {
		t.Errorf("a corrected answer was not applied after a refusal:\n%s", buf.String())
	}
}
