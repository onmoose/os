//go:build dockerlive

package lifecycle

// Real-system check for pulling from a backup source (#588): the real
// `docker` CLI, a real local registry as the source, and an upstream that
// cannot be reached. Run with:
//
//	go test ./internal/lifecycle/ -tags dockerlive -run TestLiveInstallFromSource -v -timeout 300s
//
// Needs a Docker daemon and network access to pull registry:2 and
// traefik/whoami. Excluded from the default suite by the build tag.

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/store"
)

func liveRun(t *testing.T, args ...string) string {
	t.Helper()
	var stderr strings.Builder
	cmd := exec.Command("docker", args...)
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s%s", strings.Join(args, " "), err, out, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func liveRepoDigests(t *testing.T, ref string) []string {
	t.Helper()
	var rd []string
	if err := json.Unmarshal([]byte(liveRun(t, "image", "inspect", "--format", "{{json .RepoDigests}}", ref)), &rd); err != nil {
		t.Fatalf("parse RepoDigests: %v", err)
	}
	return rd
}

func TestLiveInstallFromSource(t *testing.T) {
	ctx := context.Background()

	// The source: a local registry holding the image under another name.
	reg := liveRun(t, "run", "-d", "-p", "127.0.0.1::5000", "registry:2")
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", reg).Run() })
	port := liveRun(t, "port", reg, "5000/tcp")
	port = port[strings.LastIndex(port, ":")+1:]
	source := "127.0.0.1:" + port + "/mirror/traefik/whoami"
	liveRun(t, "pull", "traefik/whoami:v1.10.3")
	liveRun(t, "tag", "traefik/whoami:v1.10.3", source+":v1.10.3")
	liveRun(t, "push", source+":v1.10.3")
	var digest string
	for _, rd := range liveRepoDigests(t, source+":v1.10.3") {
		if name, d, ok := strings.Cut(rd, "@"); ok && name == source {
			digest = d
		}
	}
	if digest == "" {
		t.Fatalf("no digest for %s after push", source)
	}
	sourceRef := source + "@" + digest
	// Not in the local store before the install: only a pull can bring it.
	liveRun(t, "rmi", source+":v1.10.3")
	if out, err := exec.Command("docker", "image", "inspect", sourceRef).CombinedOutput(); err == nil {
		t.Fatalf("%s still present before the install: %s", sourceRef, out)
	}

	// Upstream: a registry that refuses every connection.
	upstreamImage := "127.0.0.1:9/traefik/whoami:v1.10.3"
	compose := "services:\n  app:\n    image: " + upstreamImage + "\n"
	man := fmt.Sprintf(`
id: srcapp
manifest_version: 1
name: Source App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 80
preferred_slugs: [srcapp]
permissions:
  internet: false
  lan: false
images:
  %s:
    digest: %s
    sources:
      - ref: %s
`, upstreamImage, digest, source)

	stateDir := t.TempDir()
	catDir := t.TempDir()
	s, err := store.Open(filepath.Join(stateDir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	docker := NewCLIDocker()
	m := NewManager(s, catalog.New(catDir), newFakeHost(), newFakeCaddy(), docker, events.NewBus(), stateDir)
	if err := docker.NetworkCreate(ctx, ingressNetwork, false); err != nil {
		t.Fatalf("ingress net: %v", err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", ingressNetwork).Run() })
	writeLiveCatalogApp(t, catDir, "srcapp", compose, man)

	inst, err := m.Install(ctx, mustLoadApp(t, m, "srcapp"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = m.Uninstall(context.Background(), inst.ID) })
	if inst.State != "running" {
		t.Fatalf("state = %q, want running", inst.State)
	}
	if got := overridePin(t, stateDir, inst.ID, "app"); got != sourceRef {
		t.Fatalf("override image = %q, want %q", got, sourceRef)
	}
	t.Logf("installed from the source; RepoDigests of %s: %v", sourceRef, liveRepoDigests(t, sourceRef))
	running := liveRun(t, "inspect", "--format", "{{.Config.Image}} {{.State.Status}}", "moose-"+inst.ID+"-app")
	if running != sourceRef+" running" {
		t.Fatalf("container = %q, want %q running", running, sourceRef)
	}

	// The image is removed while the app is stopped. Start pulls it again
	// through the brain, since compose may not pull by itself.
	if err := m.Stop(ctx, inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	liveRun(t, "rm", "-f", "moose-"+inst.ID+"-app")
	liveRun(t, "rmi", sourceRef)
	if err := m.Start(ctx, inst.ID); err != nil {
		t.Fatalf("start after the image was removed: %v", err)
	}
	if got := overridePin(t, stateDir, inst.ID, "app"); got != sourceRef {
		t.Fatalf("override image after start = %q, want %q", got, sourceRef)
	}
	t.Logf("start pulled the removed image from the source again")

	// Uninstall removes the image under the source's name.
	if err := m.Uninstall(ctx, inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if out, err := exec.Command("docker", "image", "inspect", sourceRef).CombinedOutput(); err == nil {
		t.Fatalf("%s survived uninstall: %s", sourceRef, out)
	}
	t.Logf("uninstall removed %s", sourceRef)
}
