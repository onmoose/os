// Package version holds moose's build identity: the repo SemVer and the git
// commit a binary was built from. There is one version for the whole
// monorepo (BUILD.md # Versioning) — brain, UI, and host-agent all ship from
// one commit, so there is no per-component counter to track here.
//
// Both vars are stamped at build time via -ldflags -X (see the Makefile's
// LDFLAGS); this package holds them, it does not compute them. The
// zero-value defaults below are what an unstamped build (`go run`, `go
// test`, an editor's "run" button) shows, so a stamped build is visibly
// different from one that isn't.
package version

// Version is the moose repo version — the contents of the root VERSION file
// at build time. VERSION holds the last *released* version and only changes
// in the dev->main release PR, so a dev build between releases reports the
// same Version as the last release; Commit (below) is what distinguishes it.
var Version = "dev"

// Commit is the short git commit sha (`git rev-parse --short HEAD`) the
// binary was built from. On a tagged release this is the tag's commit; on a
// dev build it isn't, and that distinction is the point of tracking it
// separately from Version rather than folding it into a "-dev" suffix.
var Commit = "unknown"

// String is the "moose 0.4.0 (g1a2b3c)" display form used by --version flags
// and startup logs.
func String() string {
	return "moose " + Version + " (g" + Commit + ")"
}
