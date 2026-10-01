// Package version holds a moose binary's build identity: the version of the
// release line it belongs to, and the git commit it was built from.
//
// moose has two version lines, one per update stream (BUILD.md # Versioning,
// DECISIONS.md 2026-10-01). host-agent belongs to the OS line and is stamped
// from the repo-root VERSION file. moose-brain belongs to the control-plane
// line and is stamped from CONTROL_PLANE_VERSION. Each binary carries only its
// own line, so this package keeps one Version var and the build decides which
// file fills it. There is still no per-component counter.
//
// Both vars are stamped at build time via -ldflags -X (see the Makefile's
// LDFLAGS and BRAIN_LDFLAGS, and cmd/brain/Dockerfile); this package holds
// them, it does not compute them. The zero-value defaults below are what an
// unstamped build (`go run`, `go test`, an editor's "run" button) shows, so a
// stamped build is visibly different from one that isn't.
package version

// Version is the version of this binary's release line: the contents of
// VERSION (host-agent) or CONTROL_PLANE_VERSION (brain) at build time. Each
// file holds the last *released* version of its line and only changes in a
// release PR, so a dev build between releases reports the same Version as the
// last release; Commit (below) is what distinguishes it.
var Version = "dev"

// Commit is the short git commit sha (`git rev-parse --short HEAD`) the
// binary was built from. On a tagged release this is the tag's commit; on a
// dev build it isn't, and that distinction is the point of tracking it
// separately from Version rather than folding it into a "-dev" suffix.
var Commit = "unknown"

// String is the OS-line display form, "moose 0.4.0 (g1a2b3c)", used by
// host-agent's --version flag. The cloud boot proof greps for the leading
// "moose X.Y.Z " (dev/cloud/cloud-assertions.sh 1c), so keep that shape.
func String() string {
	return "moose " + Version + " (g" + Commit + ")"
}

// ControlPlaneString is the control-plane display form, "moose control plane
// 0.4.0 (g1a2b3c)", used by the brain's --version flag. It names its line so
// the brain's number is never read as the moose (OS) release.
func ControlPlaneString() string {
	return "moose control plane " + Version + " (g" + Commit + ")"
}
