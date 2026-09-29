package brainlaunch

import (
	"context"
	"slices"
	"testing"
)

// The socket proxy runs in the host user namespace with a tmpfs on /run, and
// keeps its empty capability set: the remap opt-out gives nothing back
// (CONTROL_PLANE.md # Locked: control-plane container hardening).
func TestProxyRunsInTheHostUserNamespace(t *testing.T) {
	f := newFake()
	f.exists = false
	if err := EnsureTransport(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("EnsureTransport: %v", err)
	}
	s := f.lastRun
	if s.UsernsMode != "host" {
		t.Errorf("proxy userns = %q, want host", s.UsernsMode)
	}
	if !slices.Equal(s.Tmpfs, []string{"/run"}) {
		t.Errorf("proxy tmpfs = %v, want [/run]", s.Tmpfs)
	}
	if !slices.Equal(s.CapDrop, []string{"ALL"}) || !slices.Equal(s.SecurityOpt, []string{"no-new-privileges:true"}) {
		t.Errorf("proxy sandbox = cap_drop %v security_opt %v, want [ALL] and [no-new-privileges:true]", s.CapDrop, s.SecurityOpt)
	}
}

// A proxy the recreate path launches gets the same host namespace and tmpfs as
// a first launch, since both build it from proxyRunSpec.
func TestRecreatedProxyRunsInTheHostUserNamespace(t *testing.T) {
	f := newFake()
	f.exists = true
	f.sandboxed = false
	if err := EnsureTransport(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("EnsureTransport: %v", err)
	}
	if f.lastRun.UsernsMode != "host" || !slices.Equal(f.lastRun.Tmpfs, []string{"/run"}) {
		t.Errorf("recreated proxy = userns %q tmpfs %v, want host and [/run]", f.lastRun.UsernsMode, f.lastRun.Tmpfs)
	}
}

// The brain runs in the host user namespace, and gets no tmpfs and no
// capability change. RunSpecFor is what the control-plane update uses, so a
// brain an update recreates keeps it too.
func TestBrainRunsInTheHostUserNamespace(t *testing.T) {
	f := newFake()
	if err := Launch(context.Background(), f, testConfig()); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	for name, s := range map[string]RunSpec{"launch": f.lastRun, "RunSpecFor": RunSpecFor(testConfig())} {
		if s.UsernsMode != "host" {
			t.Errorf("%s: brain userns = %q, want host", name, s.UsernsMode)
		}
		if len(s.Tmpfs) != 0 || len(s.CapDrop) != 0 || len(s.SecurityOpt) != 0 {
			t.Errorf("%s: brain tmpfs %v cap_drop %v security_opt %v, want all empty", name, s.Tmpfs, s.CapDrop, s.SecurityOpt)
		}
	}
}

// runArgs passes the userns mode and each tmpfs to docker run, before the image.
func TestRunArgsUsernsAndTmpfs(t *testing.T) {
	args := runArgs(RunSpec{Name: "p", Image: "img", UsernsMode: "host", Tmpfs: []string{"/run", "/x"}})
	want := []string{"run", "-d", "--name", "p", "--userns", "host", "--tmpfs", "/run", "--tmpfs", "/x", "img"}
	if !slices.Equal(args, want) {
		t.Errorf("runArgs = %v, want %v", args, want)
	}
	// Empty fields add no flag, so a spec without them runs as before.
	args = runArgs(RunSpec{Name: "b", Image: "img"})
	if !slices.Equal(args, []string{"run", "-d", "--name", "b", "img"}) {
		t.Errorf("runArgs without userns/tmpfs = %v", args)
	}
}

// The full proxy spec renders to the flags the CI lane checks on a booted box.
func TestProxyRunArgs(t *testing.T) {
	args := runArgs(proxyRunSpec(testConfig()))
	for _, pair := range [][2]string{{"--userns", "host"}, {"--tmpfs", "/run"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges:true"}} {
		i := slices.Index(args, pair[0])
		if i < 0 || i+1 >= len(args) || args[i+1] != pair[1] {
			t.Errorf("proxy args %v: missing %s %s", args, pair[0], pair[1])
		}
	}
	if slices.Contains(args, "--cap-add") {
		t.Errorf("proxy args %v: must not add a capability", args)
	}
	if args[len(args)-1] != testConfig().ProxyImage {
		t.Errorf("proxy args %v: image must be last", args)
	}
}
