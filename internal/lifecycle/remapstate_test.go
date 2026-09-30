package lifecycle

import (
	"context"
	"errors"
	"testing"
	"time"
)

// dockerReads counts the docker info reads the fake saw.
func dockerReads(d *fakeDocker) int {
	n := 0
	for _, m := range d.methods() {
		if m == "UsernsRemap" {
			n++
		}
	}
	return n
}

// RemapState makes the install path's own check: host-agent and Docker must
// agree. A failed read or a disagreement is unknown, never off, so the store
// does not hide apps on a guess.
func TestRemapState(t *testing.T) {
	base := testRemapBase
	for name, tc := range map[string]struct {
		set  func(e *testEnv)
		want RemapState
	}{
		"no remap, like the fake host-agent": {func(e *testEnv) {}, RemapOff},
		"remap on both sides": {func(e *testEnv) {
			e.docker.usernsRemap = true
			e.host.remapBase = &base
		}, RemapOn},
		"docker remapped, host has no range": {func(e *testEnv) { e.docker.usernsRemap = true }, RemapUnknown},
		"host has a range, docker not":       {func(e *testEnv) { e.host.remapBase = &base }, RemapUnknown},
		"docker info fails":                  {func(e *testEnv) { e.docker.usernsRemapErr = errors.New("boom") }, RemapUnknown},
		"well-known fails":                   {func(e *testEnv) { e.host.wellKnownErr = errors.New("boom") }, RemapUnknown},
	} {
		t.Run(name, func(t *testing.T) {
			e := newTestEnv(t)
			tc.set(e)
			if got := e.m.RemapState(context.Background()); got != tc.want {
				t.Fatalf("RemapState = %v, want %v", got, tc.want)
			}
		})
	}
}

// A known answer is kept, so the store lists do not read docker info on every
// request. An unknown one is kept for a shorter time and then read again.
func TestRemapStateCached(t *testing.T) {
	e := newTestEnv(t)
	now := time.Unix(1_000_000, 0)
	e.m.remap.now = func() time.Time { return now }
	ctx := context.Background()

	for range 5 {
		if got := e.m.RemapState(ctx); got != RemapOff {
			t.Fatalf("RemapState = %v, want off", got)
		}
	}
	if n := dockerReads(e.docker); n != 1 {
		t.Fatalf("docker info read %d times for five calls, want 1", n)
	}

	// Docker stops answering. The known answer holds until it runs out.
	e.docker.usernsRemapErr = errors.New("boom")
	now = now.Add(remapKnownTTL - time.Second)
	if got := e.m.RemapState(ctx); got != RemapOff {
		t.Fatalf("RemapState before the cache ran out = %v, want off", got)
	}
	now = now.Add(2 * time.Second)
	if got := e.m.RemapState(ctx); got != RemapUnknown {
		t.Fatalf("RemapState after the cache ran out = %v, want unknown", got)
	}

	// Docker answers again: the unknown is read again after its shorter wait.
	e.docker.usernsRemapErr = nil
	now = now.Add(remapUnknownTTL - time.Second)
	if got := e.m.RemapState(ctx); got != RemapUnknown {
		t.Fatalf("RemapState inside the unknown wait = %v, want unknown", got)
	}
	now = now.Add(2 * time.Second)
	if got := e.m.RemapState(ctx); got != RemapOff {
		t.Fatalf("RemapState after the unknown wait = %v, want off", got)
	}
	if n := dockerReads(e.docker); n != 3 {
		t.Fatalf("docker info read %d times, want 3", n)
	}
}

// ctxDocker fails UsernsRemap when its context is done, as the real CLI does.
type ctxDocker struct{ *fakeDocker }

func (d ctxDocker) UsernsRemap(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return d.fakeDocker.UsernsRemap(ctx)
}

// A request that goes away must not leave an unknown in the cache.
func TestRemapStateIgnoresCallerCancel(t *testing.T) {
	e := newTestEnv(t)
	e.m.docker = ctxDocker{e.docker}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := e.m.RemapState(ctx); got != RemapOff {
		t.Fatalf("RemapState with a cancelled caller = %v, want off", got)
	}
}
