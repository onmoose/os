//go:build dockerlive

package lifecycle

// Real-system check for migrateOffIngress: what the real docker CLI does for a
// missing container, and for a container that is attached but not running. Run
// with:
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveMigrateOffIngress -v
//
// Needs network access to pull traefik/whoami. It makes moose-ingress if the
// box has none, and then removes it.

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/store"
)

func TestLiveMigrateOffIngress(t *testing.T) {
	ctx := context.Background()
	stateDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	docker := NewCLIDocker()
	m := NewManager(s, catalog.New(t.TempDir()), newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)

	const id, image = "ingresslivetest", "traefik/whoami:v1.10.3"
	dir := m.instanceDir(id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yml"), []byte("main_service: app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.override.yml"), []byte("services:\n    app:\n        networks: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// No container at all: a quiet no-op.
	if _, err := m.migrateOffIngress(ctx, id); err != nil {
		t.Fatalf("no container: %v", err)
	}

	if err := docker.Pull(ctx, image); err != nil {
		t.Fatalf("pull: %v", err)
	}
	info, err := docker.NetworkInspect(ctx, ingressNetwork)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Exists {
		if err := docker.NetworkCreate(ctx, ingressNetwork, false); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = docker.NetworkRemove(context.Background(), ingressNetwork) })
	}
	container := "moose-" + id + "-app"
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })

	// Not attached.
	if out, err := exec.Command("docker", "create", "--name", container, image).CombinedOutput(); err != nil {
		t.Fatalf("create: %v %s", err, out)
	}
	if _, err := m.migrateOffIngress(ctx, id); err != nil {
		t.Fatalf("not attached: %v", err)
	}

	// Attached and not running, then attached and running.
	for _, running := range []bool{false, true} {
		if err := docker.NetworkConnect(ctx, ingressNetwork, container); err != nil {
			t.Fatal(err)
		}
		if running {
			if out, err := exec.Command("docker", "start", container).CombinedOutput(); err != nil {
				t.Fatalf("start: %v %s", err, out)
			}
		}
		if _, err := m.migrateOffIngress(ctx, id); err != nil {
			t.Fatalf("attached, running=%v: %v", running, err)
		}
		nets, err := docker.ContainerNetworks(ctx, container)
		if err != nil {
			t.Fatal(err)
		}
		if nets[ingressNetwork] {
			t.Fatalf("still on %s, running=%v: %v", ingressNetwork, running, nets)
		}
	}
}
