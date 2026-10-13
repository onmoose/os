//go:build dockerlive

package lifecycle

// Real-system check for #608: Caddy reaches an app on the app's own network,
// and the app is not on moose-ingress. Drives the real Manager against the
// real docker CLI and the dev Caddy container (`make caddy`, which is the
// moose-caddy every dev brain drives). Run with:
//
//	make caddy
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveCaddyAppNetwork -v
//
// Needs network access to pull traefik/whoami. Host and Caddy admin are fakes:
// the route is not what this checks, the network path is.

import (
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/admission"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/store"
)

func TestLiveCaddyAppNetwork(t *testing.T) {
	ctx := context.Background()
	if out, err := exec.Command("docker", "inspect", "-f", "{{.State.Running}}", caddyContainer).Output(); err != nil || strings.TrimSpace(string(out)) != "true" {
		t.Fatalf("this test drives the dev Caddy container %s; start it with `make caddy` (inspect: %v %s)", caddyContainer, err, out)
	}
	stateDir := t.TempDir()
	catDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	docker := NewCLIDocker()
	m := NewManager(s, catalog.New(catDir), newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)
	m.SetAdmitter(admission.Check)

	writeLiveCatalogApp(t, catDir, "netapp", `
services:
  app:
    image: traefik/whoami:v1.10.3
`, `
id: netapp
manifest_version: 1
name: Net App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
preferred_slugs: [netapp]
permissions:
  internet: false
  lan: false
`)
	inst, err := m.Install(ctx, mustLoadApp(t, m, "netapp"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	appNet := "moose-app-" + inst.ID
	appCtr := "moose-" + inst.ID + "-app"
	t.Cleanup(func() { _ = m.Uninstall(context.Background(), inst.ID) })

	// The app is on its own network only, not on moose-ingress.
	nets, err := docker.ContainerNetworks(ctx, appCtr)
	if err != nil {
		t.Fatalf("app networks: %v", err)
	}
	if nets[ingressNetwork] || !nets[appNet] {
		t.Fatalf("app networks = %v, want %s and not %s", nets, appNet, ingressNetwork)
	}

	// Caddy reaches the app by the alias the route upstream uses.
	caddyGet := func() error {
		return exec.Command("docker", "exec", caddyContainer, "wget", "-q", "-T", "3", "-O", "/dev/null", "http://"+appCtr+":80/").Run()
	}
	if err := caddyGet(); err != nil {
		t.Fatalf("caddy cannot reach %s on %s: %v", appCtr, appNet, err)
	}

	// Caddy loses the network (what a recreate does); the health-tick pass
	// puts it back.
	if err := docker.NetworkDisconnect(ctx, appNet, caddyContainer); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if caddyGet() == nil {
		t.Fatalf("caddy still reaches the app with no shared network; the check proves nothing")
	}
	m.EnsureCaddyNetworks(ctx)
	if err := caddyGet(); err != nil {
		t.Fatalf("caddy not back on %s after EnsureCaddyNetworks: %v", appNet, err)
	}

	// Uninstall takes Caddy off and removes the network.
	if err := m.Uninstall(ctx, inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if err := exec.Command("docker", "network", "inspect", appNet).Run(); err == nil {
		t.Fatalf("%s survived uninstall", appNet)
	}
	if nets, _ := docker.ContainerNetworks(ctx, caddyContainer); nets[appNet] {
		t.Fatalf("caddy still lists %s", appNet)
	}
}
