package version

import "testing"

// The two display forms name their own line. host-agent's form is grepped by
// the cloud boot proof (`^moose X.Y.Z `), so it must not change shape, and the
// brain's must never match that grep, or its number reads as the OS release.
func TestDisplayFormsNameTheirLine(t *testing.T) {
	oldV, oldC := Version, Commit
	t.Cleanup(func() { Version, Commit = oldV, oldC })
	Version, Commit = "0.15.0", "abc1234"

	if got, want := String(), "moose 0.15.0 (gabc1234)"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	if got, want := ControlPlaneString(), "moose control plane 0.15.0 (gabc1234)"; got != want {
		t.Errorf("ControlPlaneString() = %q, want %q", got, want)
	}
}
