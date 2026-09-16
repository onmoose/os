package controlplane

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLedgerRoundTrip(t *testing.T) {
	dir := t.TempDir()
	applied := time.Now().UTC().Truncate(time.Second)
	want := Ledger{
		Current:  Pair{Brain: "ghcr.io/onmoose/moose-brain@sha256:new", UI: "ghcr.io/onmoose/moose-ui@sha256:new", AppliedAt: applied},
		Previous: &Pair{Brain: "moose-brain:latest", UI: "moose-ui:dev", AppliedAt: applied.Add(-24 * time.Hour)},
	}
	if err := WriteLedger(dir, want); err != nil {
		t.Fatalf("WriteLedger: %v", err)
	}

	got, ok, err := ReadLedger(dir)
	if err != nil || !ok {
		t.Fatalf("ReadLedger: ok=%v err=%v", ok, err)
	}
	if got.Current != want.Current {
		t.Errorf("current = %+v, want %+v", got.Current, want.Current)
	}
	if got.Previous == nil || *got.Previous != *want.Previous {
		t.Errorf("previous = %+v, want %+v", got.Previous, want.Previous)
	}

	// An atomic write that leaves its temp file behind fills the control-plane
	// dir with debris on every update.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != ledgerFile {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("dir contains %v, want only %s", names, ledgerFile)
	}
}

// Absent is the normal state of every box that has never updated — including
// every box shipping today — so it must not read as a failure. Unparseable is
// the opposite: something wrote garbage where the box's identity lives.
func TestReadLedgerDistinguishesAbsentFromCorrupt(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := ReadLedger(dir); ok || err != nil {
		t.Errorf("missing ledger: ok=%v err=%v, want ok=false and no error", ok, err)
	}

	if err := os.WriteFile(LedgerPath(dir), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("write corrupt ledger: %v", err)
	}
	_, _, err := ReadLedger(dir)
	if err == nil {
		t.Error("corrupt ledger read cleanly, want an error")
	}
}

// The ledger records what the box last *applied*; the env default records what
// it last *shipped with*. Once an update lands the second is older by
// definition, so the ledger has to win — otherwise a box that loses its brain
// container silently rolls back to the baked image.
func TestResolveBrainImage(t *testing.T) {
	const env = "moose-brain:latest"

	t.Run("ledger wins when it names a brain", func(t *testing.T) {
		dir := t.TempDir()
		if err := WriteLedger(dir, Ledger{Current: Pair{Brain: "ghcr.io/onmoose/moose-brain@sha256:new", UI: "ui"}}); err != nil {
			t.Fatalf("WriteLedger: %v", err)
		}
		ref, fromLedger := ResolveBrainImage(dir, env)
		if ref != "ghcr.io/onmoose/moose-brain@sha256:new" || !fromLedger {
			t.Errorf("ref=%q fromLedger=%v, want the ledger's ref", ref, fromLedger)
		}
	})

	// Every failure falls back rather than refusing to launch: the brain is how
	// anyone finds out what is wrong with the box, so it has to come up.
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"no ledger yet", func(*testing.T, string) {}},
		{"corrupt ledger", func(t *testing.T, dir string) {
			if err := os.WriteFile(LedgerPath(dir), []byte("{"), 0o644); err != nil {
				t.Fatalf("write: %v", err)
			}
		}},
		{"ledger with an empty brain ref", func(t *testing.T, dir string) {
			if err := WriteLedger(dir, Ledger{Current: Pair{UI: "ui"}}); err != nil {
				t.Fatalf("WriteLedger: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			tc.setup(t, dir)
			ref, fromLedger := ResolveBrainImage(dir, env)
			if ref != env || fromLedger {
				t.Errorf("ref=%q fromLedger=%v, want the env default", ref, fromLedger)
			}
		})
	}

	t.Run("no control-plane dir at all", func(t *testing.T) {
		ref, fromLedger := ResolveBrainImage("", env)
		if ref != env || fromLedger {
			t.Errorf("ref=%q fromLedger=%v, want the env default", ref, fromLedger)
		}
	})
}

func TestPreviousExpired(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name string
		l    Ledger
		want bool
	}{
		{"no previous pair", Ledger{}, false},
		{"applied an hour ago", Ledger{Previous: &Pair{AppliedAt: now.Add(-time.Hour)}}, false},
		{"applied six days ago", Ledger{Previous: &Pair{AppliedAt: now.Add(-6 * 24 * time.Hour)}}, false},
		{"applied eight days ago", Ledger{Previous: &Pair{AppliedAt: now.Add(-8 * 24 * time.Hour)}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.l.PreviousExpired(now); got != tc.want {
				t.Errorf("PreviousExpired = %v, want %v", got, tc.want)
			}
		})
	}
}

// A rewrite must replace the file, not append to or truncate it in place: the
// reader is a boot path, and a ledger that is briefly a mix of two generations
// would launch a brain that was never applied.
func TestWriteLedgerReplacesTheWholeFile(t *testing.T) {
	dir := t.TempDir()
	long := Ledger{Current: Pair{Brain: strings.Repeat("a", 200), UI: strings.Repeat("b", 200)}}
	if err := WriteLedger(dir, long); err != nil {
		t.Fatalf("WriteLedger long: %v", err)
	}
	short := Ledger{Current: Pair{Brain: "b", UI: "u"}}
	if err := WriteLedger(dir, short); err != nil {
		t.Fatalf("WriteLedger short: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(dir, ledgerFile))
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if strings.Contains(string(b), "aaaa") {
		t.Errorf("the previous, longer ledger is still partly on disk:\n%s", b)
	}
	got, _, err := ReadLedger(dir)
	if err != nil {
		t.Fatalf("ReadLedger: %v", err)
	}
	if got.Current.Brain != "b" {
		t.Errorf("brain = %q, want b", got.Current.Brain)
	}
}
