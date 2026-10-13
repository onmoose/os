package lifecycle

import (
	"context"
	"errors"
	"testing"
)

// Caddy reaches an app by joining the app's own network, and no app joins
// moose-ingress (#608). These tests cover the brain's side of that: the
// connect on install and start, the disconnect before the network is removed,
// and the pass that puts a recreated Caddy back on every running app.

// callIndex is the position of the first call to method whose first argument
// is arg, or -1.
func callIndex(calls []call, method string, arg any) int {
	for i, c := range calls {
		if c.method == method && len(c.args) > 0 && c.args[0] == arg {
			return i
		}
	}
	return -1
}

func TestInstallConnectsCaddyToAppNetwork(t *testing.T) {
	e := newTestEnv(t)
	inst := installWhoami(t, e)

	appNet := "moose-app-" + inst.ID
	if !e.docker.attached[caddyContainer][appNet] {
		t.Fatalf("caddy is not on %s after install: %v", appNet, e.docker.attached)
	}
	calls := e.docker.Calls()
	// The network must exist before Caddy can join it, and Caddy must be on it
	// before the route flips to the real upstream.
	created, connected, up := callIndex(calls, "NetworkCreate", appNet), callIndex(calls, "NetworkConnect", appNet), callIndex(calls, "ComposeUp", e.m.instanceDir(inst.ID))
	if created < 0 || connected < 0 || connected < created {
		t.Fatalf("want NetworkCreate then NetworkConnect for %s, got %v", appNet, e.docker.methods())
	}
	if up >= 0 && connected > up {
		t.Fatalf("caddy joined the app network after compose up: %v", e.docker.methods())
	}
}

func TestInstallSurvivesCaddyConnectFailure(t *testing.T) {
	e := newTestEnv(t)
	e.docker.networkConnectErr = errors.New("no such container: moose-caddy")
	inst := installWhoami(t, e)
	if inst.State != "running" {
		t.Fatalf("a failed caddy connect must not fail the install (the health tick retries), state=%q", inst.State)
	}
}

func TestUninstallDisconnectsCaddyBeforeNetworkRemove(t *testing.T) {
	e := newTestEnv(t)
	inst := installWhoami(t, e)
	e.docker.calls = nil

	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	appNet := "moose-app-" + inst.ID
	calls := e.docker.Calls()
	disc, down, rm := callIndex(calls, "NetworkDisconnect", appNet), callIndex(calls, "ComposeDown", e.m.instanceDir(inst.ID)), callIndex(calls, "NetworkRemove", appNet)
	if disc < 0 || rm < 0 || disc > rm {
		t.Fatalf("want NetworkDisconnect before NetworkRemove for %s, got %v", appNet, e.docker.methods())
	}
	if down >= 0 && disc > down {
		t.Fatalf("want caddy off the app network before compose down, got %v", e.docker.methods())
	}
	if e.docker.attached[caddyContainer][appNet] {
		t.Fatalf("caddy still on %s after uninstall", appNet)
	}
}

func TestStartReconnectsCaddy(t *testing.T) {
	e := newTestEnv(t)
	inst := installWhoami(t, e)
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	// Caddy was recreated while the app was stopped, so it lost the network.
	delete(e.docker.attached[caddyContainer], "moose-app-"+inst.ID)

	if err := e.m.Start(context.Background(), inst.ID); err != nil {
		t.Fatalf("start: %v", err)
	}
	if !e.docker.attached[caddyContainer]["moose-app-"+inst.ID] {
		t.Fatalf("start did not put caddy back on the app network")
	}
}

func TestEnsureCaddyNetworksReconnectsRunningApps(t *testing.T) {
	e := newTestEnv(t)
	inst := installWhoami(t, e)
	appNet := "moose-app-" + inst.ID

	// Caddy recreated: it is on no app network any more.
	e.docker.attached[caddyContainer] = map[string]bool{ingressNetwork: true}
	e.docker.calls = nil
	e.m.EnsureCaddyNetworks(context.Background())
	if !e.docker.attached[caddyContainer][appNet] {
		t.Fatalf("caddy not reconnected to %s: %v", appNet, e.docker.methods())
	}

	// Already on it: one inspect, no connect.
	e.docker.calls = nil
	e.m.EnsureCaddyNetworks(context.Background())
	if callIndex(e.docker.Calls(), "NetworkConnect", appNet) >= 0 {
		t.Fatalf("connected again although caddy is on %s: %v", appNet, e.docker.methods())
	}
}

func TestEnsureCaddyNetworksSkipsStoppedApps(t *testing.T) {
	e := newTestEnv(t)
	inst := installWhoami(t, e)
	if err := e.m.Stop(context.Background(), inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	e.docker.attached[caddyContainer] = map[string]bool{}
	e.docker.calls = nil
	e.m.EnsureCaddyNetworks(context.Background())
	if len(e.docker.Calls()) != 0 {
		t.Fatalf("a stopped app needs no upstream; want no docker calls, got %v", e.docker.methods())
	}
}

func TestEnsureCaddyNetworksSkipsLockedInstance(t *testing.T) {
	e := newTestEnv(t)
	inst := installWhoami(t, e)
	e.docker.attached[caddyContainer] = map[string]bool{}

	// An uninstall holds the lock: connecting now would keep the network from
	// being removed.
	unlock := e.m.lockInstance(inst.ID)
	e.m.EnsureCaddyNetworks(context.Background())
	unlock()
	if e.docker.attached[caddyContainer]["moose-app-"+inst.ID] {
		t.Fatalf("connected caddy to an instance another path holds")
	}
}

func TestEnsureCaddyNetworksInspectFailure(t *testing.T) {
	e := newTestEnv(t)
	inst := installWhoami(t, e)
	e.docker.attached[caddyContainer] = map[string]bool{}
	e.docker.containerNetworksErr = errors.New("no such container: moose-caddy")
	e.docker.calls = nil
	e.m.EnsureCaddyNetworks(context.Background())
	if callIndex(e.docker.Calls(), "NetworkConnect", "moose-app-"+inst.ID) >= 0 {
		t.Fatalf("connected without knowing caddy's networks: %v", e.docker.methods())
	}
}

func TestParseContainerNetworks(t *testing.T) {
	got, err := parseContainerNetworks([]byte(`{"moose-ingress":{"IPAddress":"172.18.0.3"},"moose-app-x":{"Aliases":null}}` + "\n"))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got) != 2 || !got["moose-ingress"] || !got["moose-app-x"] {
		t.Fatalf("got %v", got)
	}
	if _, err := parseContainerNetworks([]byte("not json")); err == nil {
		t.Fatalf("want an error for unparseable output")
	}
}

func TestDockerNetworkErrorWording(t *testing.T) {
	// Docker 28's words, captured from a real daemon.
	if !alreadyAttached("Error response from daemon: endpoint with name moose-caddy already exists in network moose-app-x") {
		t.Fatalf("already-attached wording not recognized")
	}
	for _, out := range []string{
		"Error response from daemon: container 421b is not connected to network moose-app-x",
		"Error response from daemon: network moose-app-x not found",
		"Error response from daemon: No such container: moose-caddy",
	} {
		if !notAttached(out) {
			t.Fatalf("not-attached wording not recognized: %q", out)
		}
	}
	if notAttached("Error response from daemon: permission denied") {
		t.Fatalf("a real failure read as not attached")
	}
}
