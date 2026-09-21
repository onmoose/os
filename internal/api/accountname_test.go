package api

import "testing"

// free is a taken-check for a host where nothing exists, so derivation always
// resolves on its first candidate.
func free(string) (bool, error) { return false, nil }

// occupied returns a taken-check that reports the given names as already used.
func occupied(names ...string) func(string) (bool, error) {
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	return func(n string) (bool, error) { return set[n], nil }
}

func TestAccountNameBase(t *testing.T) {
	cases := map[string]string{
		"José Smith":   "josesmith", // the issue's worked example
		"Cindy":        "cindy",
		"Anna-Maria":   "annamaria", // separators are removed, not replaced
		"Anna Maria":   "annamaria", // so these two collide, by design
		"Søren":        "soren",     // not decomposable: needs the fold table
		"Straße":       "strasse",
		"Łukasz":       "lukasz",
		"Ægir":         "aegir",
		"Nguyễn":       "nguyen", // Vietnamese: NFD does the work
		"Müller":       "muller",
		"  spaced  ":   "spaced",
		"O'Brien":      "obrien",
		"李":            "user",  // no Latin letters survive: the fallback
		"2024":         "u2024", // useradd refuses an all-numeric name
		"7Up":          "u7up",
		"mixedCASE123": "mixedcase123",
	}
	for in, want := range cases {
		if got := accountNameBase(in); got != want {
			t.Errorf("accountNameBase(%q) = %q; want %q", in, got, want)
		}
	}
}

func TestAccountNameBaseIsAlwaysUsable(t *testing.T) {
	// Whatever goes in, what comes out must be a name useradd accepts and must
	// keep the two reservations the <slug>--<user> scheme depends on.
	for _, in := range []string{
		"José Smith", "李", "2024", "----", "xn--bcher-kva", "a--b",
		"ThisNameIsFarTooLongToBeALinuxAccountNameByAnyMeasureAtAll", "!!!", "ß",
	} {
		got := accountNameBase(in)
		if got == "" {
			t.Errorf("accountNameBase(%q) = empty", in)
		}
		if len(got) > maxAccountNameLen {
			t.Errorf("accountNameBase(%q) = %q, over %d chars", in, got, maxAccountNameLen)
		}
		if got[0] < 'a' || got[0] > 'z' {
			t.Errorf("accountNameBase(%q) = %q, must start with a letter", in, got)
		}
		if err := validateUsername(got); err != nil {
			t.Errorf("accountNameBase(%q) = %q, fails validateUsername: %v", in, got, err)
		}
	}
}

func TestDeriveAccountNameWalksPastCollisions(t *testing.T) {
	// A second José. The display names differ (uniqueness only folds case and
	// whitespace, not accents), so both are allowed and they collide here.
	got, err := deriveAccountName("Jose Smith", occupied("josesmith"))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got != "josesmith1" {
		t.Errorf("second José = %q; want josesmith1", got)
	}

	// Third one walks again.
	got, err = deriveAccountName("Jose Smith", occupied("josesmith", "josesmith1"))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got != "josesmith2" {
		t.Errorf("third José = %q; want josesmith2", got)
	}
}

func TestDeriveAccountNameRefusesReserved(t *testing.T) {
	// Somebody called "Root" is a perfectly good display name, but must not be
	// handed the root account.
	for _, in := range []string{"root", "Root", "admin", "Postgres", "systemd", "systemd-resolve"} {
		// systemd-resolve derives to "systemdresolve", which is not reserved and
		// not a real account; the host probe is what would catch it if it were.
		got, err := deriveAccountName(in, free)
		if err != nil {
			t.Fatalf("derive(%q): %v", in, err)
		}
		if reservedAccountNames[got] {
			t.Errorf("derive(%q) = %q, which is reserved", in, got)
		}
	}
	if got, _ := deriveAccountName("root", free); got != "root1" {
		t.Errorf("derive(root) = %q; want root1", got)
	}
}

func TestDeriveAccountNameRespectsTheHost(t *testing.T) {
	// The collision that matters: a daemon account an app package added. Without
	// this walk, set-password's upsert would hand the person that account.
	got, err := deriveAccountName("Plex", occupied("plex"))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if got != "plex1" {
		t.Errorf("derive(Plex) with plex on the host = %q; want plex1", got)
	}
}

func TestDeriveAccountNameSuffixStaysWithinLengthCap(t *testing.T) {
	long := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 44 chars
	base := accountNameBase(long)
	got, err := deriveAccountName(long, occupied(base))
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if len(got) > maxAccountNameLen {
		t.Errorf("derive(long) = %q (%d chars); want at most %d", got, len(got), maxAccountNameLen)
	}
	if got == base {
		t.Errorf("derive(long) = %q, same as the taken base", got)
	}
}

func TestDeriveAccountNameGivesUpRatherThanSpinning(t *testing.T) {
	// A host-agent that answers "exists" to everything must not hang the brain.
	_, err := deriveAccountName("Cindy", func(string) (bool, error) { return true, nil })
	if err == nil {
		t.Fatal("derive with everything taken returned no error")
	}
}

func TestDeriveAccountNamePropagatesHostErrors(t *testing.T) {
	// A broken probe must not be read as "the name is free". That is how a
	// person ends up owning a system account.
	boom := errTest
	_, err := deriveAccountName("Cindy", func(string) (bool, error) { return false, boom })
	if err != boom {
		t.Fatalf("derive with a failing probe = %v; want the probe's error", err)
	}
}

func TestFoldDisplayNameFoldsCaseAndSpaceOnly(t *testing.T) {
	same := [][2]string{
		{"Cindy", "cindy"},
		{"Cindy", "CINDY"},
		{"Anna Maria", "  Anna   Maria "},
	}
	for _, p := range same {
		if foldDisplayName(p[0]) != foldDisplayName(p[1]) {
			t.Errorf("%q and %q should fold together", p[0], p[1])
		}
	}
	// Accents are NOT folded: José and Jose are two different people.
	if foldDisplayName("José") == foldDisplayName("Jose") {
		t.Error(`"José" and "Jose" should not fold together`)
	}
}

func TestValidateDisplayName(t *testing.T) {
	if err := validateDisplayName("José Smith"); err != nil {
		t.Errorf("valid name rejected: %v", err)
	}
	for _, bad := range []string{"", "a\u0000b", string(make([]rune, maxDisplayNameLen+1))} {
		if err := validateDisplayName(bad); err == nil {
			t.Errorf("validateDisplayName(%q) accepted", bad)
		}
	}
}

// errTest is a sentinel for the probe-failure case above.
var errTest = errSentinel("probe exploded")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
