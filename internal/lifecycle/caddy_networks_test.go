package lifecycle

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
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

// An app installed before #608 has a saved override that attaches its main
// service to moose-ingress, and every later compose up reuses that file. The
// startup reconcile must take it off, in the file and on the running
// container, for running and stopped apps alike.
func TestReconcileMovesOldAppsOffIngress(t *testing.T) {
	for _, state := range []string{"running", "stopped"} {
		t.Run(state, func(t *testing.T) {
			e := newTestEnv(t)
			inst := installWhoami(t, e)
			if state == "stopped" {
				if err := e.m.Stop(context.Background(), inst.ID); err != nil {
					t.Fatalf("stop: %v", err)
				}
			}
			e.docker.psManaged[inst.ID] = state == "running"
			appNet := "moose-app-" + inst.ID
			alias := "moose-" + inst.ID + "-whoami"
			ovPath := filepath.Join(e.m.instanceDir(inst.ID), "compose.override.yml")
			// The override as writeOverride wrote it before #608.
			old := "networks:\n    " + appNet + ":\n        external: true\n    moose-ingress:\n        external: true\n" +
				"services:\n    whoami:\n        container_name: " + alias + "\n        networks:\n            " + appNet + ": null\n" +
				"            moose-ingress:\n                aliases:\n                    - " + alias + "\n"
			if err := os.WriteFile(ovPath, []byte(old), 0o644); err != nil {
				t.Fatal(err)
			}
			e.docker.calls = nil

			if err := e.m.Reconcile(context.Background()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			got, err := os.ReadFile(ovPath)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(got), "moose-ingress") {
				t.Fatalf("override still names moose-ingress:\n%s", got)
			}
			var doc struct {
				Services map[string]struct {
					Networks map[string]*struct {
						Aliases []string `yaml:"aliases"`
					} `yaml:"networks"`
				} `yaml:"services"`
			}
			if err := yaml.Unmarshal(got, &doc); err != nil {
				t.Fatal(err)
			}
			n := doc.Services["whoami"].Networks[appNet]
			if n == nil || len(n.Aliases) != 1 || n.Aliases[0] != alias {
				t.Fatalf("alias did not move to %s:\n%s", appNet, got)
			}
			calls := e.docker.Calls()
			if i := callIndex(calls, "NetworkDisconnect", ingressNetwork); i < 0 || calls[i].args[1] != alias {
				t.Fatalf("main container not disconnected from moose-ingress: %v", e.docker.methods())
			}

			// Already moved: the next pass changes nothing.
			e.docker.calls = nil
			if err := e.m.Reconcile(context.Background()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			if callIndex(e.docker.Calls(), "NetworkDisconnect", ingressNetwork) >= 0 {
				t.Fatalf("disconnected again on an override with no moose-ingress")
			}
		})
	}
}

// CaddyOffNetwork names the running apps the last pass left Caddy off, so the
// probe does not read their route's 502 as the app failing (#613).
func TestCaddyOffNetworkAfterAFailedConnect(t *testing.T) {
	e := newTestEnv(t)
	inst := installWhoami(t, e)
	if e.m.CaddyOffNetwork(inst.ID) {
		t.Fatal("off before any pass")
	}
	e.docker.attached[caddyContainer] = map[string]bool{}
	e.docker.networkConnectErr = errors.New("no such container: moose-caddy")
	e.m.EnsureCaddyNetworks(context.Background())
	if !e.m.CaddyOffNetwork(inst.ID) {
		t.Fatal("a failed connect does not count as off")
	}
	// The inspect fails: what the last pass found stays.
	e.docker.containerNetworksErr = errors.New("docker unreachable")
	e.m.EnsureCaddyNetworks(context.Background())
	if !e.m.CaddyOffNetwork(inst.ID) {
		t.Fatal("an unknown pass dropped what the last one found")
	}
	e.docker.containerNetworksErr = nil
	e.docker.networkConnectErr = nil
	e.m.EnsureCaddyNetworks(context.Background())
	if e.m.CaddyOffNetwork(inst.ID) {
		t.Fatal("still off after Caddy was put back on the network")
	}
	// A held app counts as off until a pass sees Caddy on its network.
	e.docker.attached[caddyContainer] = map[string]bool{}
	unlock := e.m.lockInstance(inst.ID)
	e.m.EnsureCaddyNetworks(context.Background())
	unlock()
	if !e.m.CaddyOffNetwork(inst.ID) {
		t.Fatal("a held app off Caddy's network does not count as off")
	}
}
