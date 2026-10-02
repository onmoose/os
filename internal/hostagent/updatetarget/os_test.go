package updatetarget

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

func rel(v string) OSRelease {
	return OSRelease{
		Version:      v,
		BundleURL:    DefaultOSURLPrefix + "v" + v + "/moose-v" + v + "-amd64.raucb",
		BundleSHA256: strings.Repeat("a", 64),
	}
}

func TestValidateOS(t *testing.T) {
	ok := []OSRelease{rel("1.3.4"), rel("1.4.2")}
	if err := ValidateOS(ok, DefaultOSURLPrefix); err != nil {
		t.Fatalf("a good list was refused: %v", err)
	}
	cases := map[string][]OSRelease{
		"no digest":       {{Version: "1.4.2", BundleURL: rel("1.4.2").BundleURL}},
		"short digest":    {{Version: "1.4.2", BundleURL: rel("1.4.2").BundleURL, BundleSHA256: "abc"}},
		"upper digest":    {{Version: "1.4.2", BundleURL: rel("1.4.2").BundleURL, BundleSHA256: strings.Repeat("A", 64)}},
		"not a version":   {{Version: "latest", BundleURL: rel("1.4.2").BundleURL, BundleSHA256: strings.Repeat("a", 64)}},
		"pre-release":     {{Version: "1.4.2-rc1", BundleURL: rel("1.4.2").BundleURL, BundleSHA256: strings.Repeat("a", 64)}},
		"other host":      {{Version: "1.4.2", BundleURL: "https://evil.example/b.raucb", BundleSHA256: strings.Repeat("a", 64)}},
		"not a URL":       {{Version: "1.4.2", BundleURL: "/b.raucb", BundleSHA256: strings.Repeat("a", 64)}},
		"out of order":    {rel("1.4.2"), rel("1.3.4")},
		"duplicate entry": {rel("1.4.2"), rel("1.4.2")},
	}
	for name, list := range cases {
		if err := ValidateOS(list, DefaultOSURLPrefix); !errors.Is(err, ErrOSRefused) {
			t.Errorf("%s: want a refusal, got %v", name, err)
		}
	}
	// An empty prefix accepts any http(s) URL: the in-guest source of the boot lane.
	if err := ValidateOS([]OSRelease{{Version: "1.4.2", BundleURL: "http://127.0.0.1:5001/b.raucb", BundleSHA256: strings.Repeat("a", 64)}}, ""); err != nil {
		t.Errorf("an empty prefix refused a plain URL: %v", err)
	}
}

func TestPickOS(t *testing.T) {
	cases := []struct {
		name, running, floor string
		list                 []string
		want                 string
		current, refused     bool
	}{
		{name: "current", running: "1.4.2", list: []string{"1.3.9", "1.4.2"}, want: "1.4.2", current: true},
		{name: "patch up", running: "1.4.0", list: []string{"1.4.2"}, want: "1.4.2"},
		{name: "patch down", running: "1.4.2", list: []string{"1.4.0"}, want: "1.4.0"},
		{name: "next minor", running: "1.3.1", list: []string{"1.3.9", "1.4.2"}, want: "1.4.2"},
		{name: "never skips a minor", running: "1.2.0", list: []string{"1.2.5", "1.3.9", "1.4.2"}, want: "1.3.9"},
		{name: "across a major", running: "1.9.3", list: []string{"1.9.4", "2.0.1"}, want: "2.0.1"},
		{name: "one minor back", running: "1.4.2", list: []string{"1.3.9"}, want: "1.3.9"},
		{name: "two minors back", running: "1.4.2", list: []string{"1.2.9"}, refused: true},
		{name: "one minor back across a major", running: "2.0.1", list: []string{"1.9.4"}, want: "1.9.4"},
		{name: "not the last line of the major", running: "2.0.1", list: []string{"1.8.4", "1.9.4"}, refused: false, want: "1.9.4"},
		{name: "a later line of the old major exists", running: "2.0.1", list: []string{"1.8.4", "1.9.0", "1.8.9"}, refused: true},
		{name: "below the floor", running: "1.4.2", floor: "1.4.0", list: []string{"1.3.9"}, refused: true},
		{name: "above the floor", running: "1.4.2", floor: "1.3.0", list: []string{"1.3.9"}, want: "1.3.9"},
		{name: "running is not a version", running: "dev", list: []string{"1.4.2"}, refused: true},
	}
	for _, c := range cases {
		var list []OSRelease
		for _, v := range c.list {
			list = append(list, rel(v))
		}
		got, current, err := PickOS(c.running, c.floor, list)
		if c.refused {
			if err == nil {
				t.Errorf("%s: want a refusal, got %s", c.name, got.Version)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got.Version != c.want || current != c.current {
			t.Errorf("%s: got %s current=%v, want %s current=%v", c.name, got.Version, current, c.want, c.current)
		}
	}
}

// fakeOS records what the loop asked of stream A.
type fakeOS struct {
	running string
	calls   []bool // the open flag of each Apply
	rel     OSRelease
}

func (f *fakeOS) Running() (string, string) { return f.running, "A" }
func (f *fakeOS) Floor() string             { return "" }
func (f *fakeOS) Apply(r OSRelease, open bool, _ time.Time) OSDecision {
	f.calls = append(f.calls, open)
	f.rel = r
	if open {
		return OSDecision{State: protocol.OSUpdateRebooting}
	}
	return OSDecision{State: protocol.OSUpdateInstalled}
}

func osLoop(t Target, running string, w Window, now time.Time) (*Loop, *fakeOS, *fakeApplier) {
	f := &fakeOS{running: running}
	ap := &fakeApplier{}
	l := &Loop{
		Source:    &fakeSource{target: t},
		Current:   fakeRunning{brain: t.BrainImage, ui: t.UIImage},
		Applier:   ap,
		AutoApply: true,
		Window:    w,
		OS:        f,
		Now:       func() time.Time { return now },
	}
	return l, f, ap
}

func TestTickOS(t *testing.T) {
	w, _ := ParseWindow("03:00-04:00")
	in := time.Date(2026, 10, 3, 3, 10, 0, 0, time.Local)
	out := time.Date(2026, 10, 3, 12, 0, 0, 0, time.Local)
	base := Target{Version: "v0.16.0", BrainImage: brainRef, UIImage: uiRef}
	base.OS = []OSRelease{rel("0.15.1")}

	t.Run("installs ahead of the window, switches inside it", func(t *testing.T) {
		l, f, _ := osLoop(base, "0.15.0", w, out)
		l.Tick(context.Background())
		if len(f.calls) != 1 || f.calls[0] {
			t.Fatalf("outside the window: want one Apply with open=false, got %v", f.calls)
		}
		if s := l.Snapshot().OS; s.State != protocol.OSUpdateInstalled || s.Target == nil || s.Target.Version != "0.15.1" {
			t.Fatalf("snapshot: %+v", s)
		}
		l.Now = func() time.Time { return in }
		l.Tick(context.Background())
		if len(f.calls) != 2 || !f.calls[1] {
			t.Fatalf("inside the window: want open=true, got %v", f.calls)
		}
	})

	t.Run("stream B goes first in the window", func(t *testing.T) {
		tgt := base
		l, f, ap := osLoop(tgt, "0.15.0", w, in)
		l.Current = fakeRunning{brain: oldBrain, ui: tgt.UIImage}
		l.Tick(context.Background())
		if len(ap.calls) != 1 {
			t.Fatalf("want the control-plane update started, got %d calls", len(ap.calls))
		}
		if len(f.calls) != 1 || f.calls[0] {
			t.Fatalf("the OS must not switch while stream B holds the window, got %v", f.calls)
		}
	})

	t.Run("a bad OS part refuses stream A only", func(t *testing.T) {
		tgt := base
		tgt.OS = []OSRelease{{Version: "0.15.1", BundleURL: rel("0.15.1").BundleURL}}
		l, f, _ := osLoop(tgt, "0.15.0", w, in)
		l.Current = fakeRunning{brain: oldBrain, ui: tgt.UIImage}
		ap := l.Applier.(*fakeApplier)
		l.Tick(context.Background())
		if len(f.calls) != 0 {
			t.Fatalf("a refused OS part reached the applier")
		}
		if s := l.Snapshot().OS; s.State != protocol.OSUpdateRefused {
			t.Fatalf("want refused, got %+v", s)
		}
		if len(ap.calls) != 1 {
			t.Fatalf("the control plane must still update, got %d calls", len(ap.calls))
		}
	})

	t.Run("an answer with only an OS part still moves the OS", func(t *testing.T) {
		tgt := Target{OS: []OSRelease{rel("0.15.1")}, Window: "00:00-23:59"}
		l, f, ap := osLoop(tgt, "0.15.0", w, out)
		l.Tick(context.Background())
		if l.Snapshot().Outcome != OutcomeRefused || len(ap.calls) != 0 {
			t.Fatalf("the control-plane part must be refused: %+v, %d calls", l.Snapshot(), len(ap.calls))
		}
		if len(f.calls) != 1 || !f.calls[0] {
			t.Fatalf("stream A must still be judged, in the answer's window: %v", f.calls)
		}
	})

	t.Run("current and none", func(t *testing.T) {
		l, f, _ := osLoop(base, "0.15.1", w, in)
		l.Tick(context.Background())
		if len(f.calls) != 0 || l.Snapshot().OS.State != protocol.OSUpdateCurrent {
			t.Fatalf("current: calls %v, snapshot %+v", f.calls, l.Snapshot().OS)
		}
		tgt := base
		tgt.OS = nil
		l, _, _ = osLoop(tgt, "0.15.0", w, in)
		l.Tick(context.Background())
		if l.Snapshot().OS.State != protocol.OSUpdateNone {
			t.Fatalf("none: %+v", l.Snapshot().OS)
		}
	})

	t.Run("no applier is unsupported", func(t *testing.T) {
		l, _, _ := osLoop(base, "0.15.0", w, in)
		l.OS = nil
		l.Tick(context.Background())
		if l.Snapshot().OS.State != protocol.OSUpdateUnsupported {
			t.Fatalf("got %+v", l.Snapshot().OS)
		}
	})
}
