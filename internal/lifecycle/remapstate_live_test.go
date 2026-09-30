//go:build dockerlive

package lifecycle

// Real-system check for the store's remap read: the real docker CLI against
// the machine's daemon, with a host that reports no range, like the fake
// host-agent in the dev loop. Run with:
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveRemapState -v
//
// Needs a Docker daemon.

import (
	"context"
	"testing"
)

func TestLiveRemapState(t *testing.T) {
	ctx := context.Background()
	d := NewCLIDocker()
	remapped, err := d.UsernsRemap(ctx)
	if err != nil {
		t.Fatalf("docker info: %v", err)
	}
	m := &Manager{host: &fakeHost{}, docker: d}
	got := m.RemapState(ctx)
	// The fake host reports no range. A daemon with no remap agrees, so the
	// store knows the box runs none. A remapped daemon disagrees, so the
	// state is unknown and the store hides nothing.
	want := RemapOff
	if remapped {
		want = RemapUnknown
	}
	if got != want {
		t.Fatalf("RemapState = %v with a daemon that has remap=%v, want %v", got, remapped, want)
	}
	t.Logf("daemon remap=%v, RemapState=%v", remapped, got)
}
