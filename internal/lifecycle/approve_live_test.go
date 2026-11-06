//go:build dockerlive

package lifecycle

// Real-system proof for an approved update that asks for more access, and for
// the network flag on any update (#633, UPDATES.md # As built (#633)): a new
// folder is bound from the right source, a pick-subfolder folder that widens
// to the whole folder binds the whole folder, the internet turned on reaches
// Docker and a rollback turns it off again, and an update that turns the
// internet off makes the network internal. Run with:
//
//	go test -count=1 ./internal/lifecycle/ -tags dockerlive -run 'TestLiveApproved|TestLiveAutoUpdateTurnsInternetOff' -v -timeout 900s
//
// Needs a Docker daemon and the alpine:3.21 image (pulled when missing).

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/store"
)

// liveNetworkInternal reads the app network's internal flag from Docker.
func liveNetworkInternal(t *testing.T, id string) bool {
	t.Helper()
	out, err := exec.Command("docker", "network", "inspect", "-f", "{{.Internal}}", appNetwork(id)).Output()
	if err != nil {
		t.Fatalf("inspect the app network: %v", err)
	}
	return strings.TrimSpace(string(out)) == "true"
}

// liveOnNetwork reports whether the app's main container is on its own
// network, the one that exists now.
func liveOnNetwork(t *testing.T, id string) bool {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{range $k, $v := .NetworkSettings.Networks}}{{$k}}={{$v.NetworkID}} {{end}}", "moose-"+id+"-app").Output()
	if err != nil {
		t.Fatalf("inspect the app container: %v", err)
	}
	netID, err := exec.Command("docker", "network", "inspect", "-f", "{{.Id}}", appNetwork(id)).Output()
	if err != nil {
		t.Fatalf("inspect the app network: %v", err)
	}
	return strings.Contains(string(out), appNetwork(id)+"="+strings.TrimSpace(string(netID)))
}

// liveExec runs a command in the app's main container.
func liveExec(t *testing.T, id string, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("docker", append([]string{"exec", "moose-" + id + "-app"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

const liveImage = "alpine:3.21"

// installLivePersonal installs 1.0 of a personal app for alex with the given
// permissions and folder mounts.
func installLivePersonal(t *testing.T, e *liveUpdateEnv, id, perms string, mounts []FolderMount) store.Instance {
	t.Helper()
	ctx := context.Background()
	if err := e.st.CreateUser(store.User{ID: "u_alex", Username: "alex", DisplayName: "Alex", Role: store.RoleMember}); err != nil {
		t.Fatal(err)
	}
	e.publish(t, id, liveAppManifest(id, "1.0", perms), liveAppCompose(liveImage, "true", false, ""))
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, id), Owner{UserID: "u_alex", Username: "alex"}, store.ScopePersonal, mounts, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = e.m.Uninstall(context.Background(), inst.ID) })
	return inst
}

// approveAndUpdate approves the version the box offers and runs the update.
func approveAndUpdate(t *testing.T, e *liveUpdateEnv, id, version string) error {
	t.Helper()
	ctx := context.Background()
	e.m.CheckUpdates(ctx)
	row, _ := e.st.Get(id)
	if row.Update.Status != store.UpdateNeedsApproval {
		t.Fatalf("check = %+v, want an update that waits for an approval", row.Update)
	}
	if _, err := e.m.ApproveUpdate(ctx, id, version, "u_alex"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	return e.m.Update(ctx, id, func(s string) { t.Logf("step %s", s) })
}

func mkFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestLiveApprovedUpdateAddsAFolder: 2.0 adds a Movies folder. The update
// binds the owner's own Movies folder, the install default, and the new
// version reads it at /moose/movies.
func TestLiveApprovedUpdateAddsAFolder(t *testing.T) {
	e := newLiveUpdateEnv(t)
	const id = "liveapprovefolder"
	home := filepath.Join(e.host.homeRoot, "alex")
	mkFile(t, filepath.Join(home, "Photos", "cat.jpg"), "a photo")
	mkFile(t, filepath.Join(home, "Movies", "film.txt"), "a film")
	photos := "  folders:\n    - folder: photos\n      mode: read\n"
	inst := installLivePersonal(t, e, id, photos, []FolderMount{{Folder: "photos", Source: sourcePersonal}})
	if _, err := liveExec(t, inst.ID, "ls", "/moose/movies"); err == nil {
		t.Fatal("1.0 sees a Movies folder")
	}

	e.publish(t, id, liveAppManifest(id, "2.0", photos+"    - folder: movies\n      mode: read\n"), liveAppCompose(liveImage, "true", false, ""))
	if err := approveAndUpdate(t, e, inst.ID, "2.0"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, err := liveExec(t, inst.ID, "cat", "/moose/movies/film.txt"); err != nil || got != "a film" {
		t.Fatalf("2.0 reads /moose/movies/film.txt: %q, %v", got, err)
	}
	if got, err := liveExec(t, inst.ID, "cat", "/moose/photos/cat.jpg"); err != nil || got != "a photo" {
		t.Fatalf("2.0 lost the Photos folder: %q, %v", got, err)
	}
	t.Log("2.0 reads the owner's own Movies folder")
}

// TestLiveApprovedUpdateWholeFolder: 1.0 binds one folder inside Documents
// (pick-subfolder); 2.0 wants all of Documents. The update binds the whole
// folder, from the same source.
func TestLiveApprovedUpdateWholeFolder(t *testing.T) {
	e := newLiveUpdateEnv(t)
	const id = "liveapprovewhole"
	home := filepath.Join(e.host.homeRoot, "alex")
	mkFile(t, filepath.Join(home, "Documents", "Notebooks", "nb.txt"), "in the subfolder")
	mkFile(t, filepath.Join(home, "Documents", "top.txt"), "at the top")
	sub := "  folders:\n    - folder: documents\n      mode: read\n      scope: pick-subfolder\n      default: Notebooks\n"
	inst := installLivePersonal(t, e, id, sub, []FolderMount{{Folder: "documents", Source: sourcePersonal, Subfolder: "Notebooks"}})
	if got, _ := liveExec(t, inst.ID, "cat", "/moose/documents/nb.txt"); got != "in the subfolder" {
		t.Fatalf("1.0 does not see its subfolder: %q", got)
	}

	whole := "  folders:\n    - folder: documents\n      mode: read\n"
	e.publish(t, id, liveAppManifest(id, "2.0", whole), liveAppCompose(liveImage, "true", false, ""))
	if err := approveAndUpdate(t, e, inst.ID, "2.0"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got, err := liveExec(t, inst.ID, "cat", "/moose/documents/top.txt"); err != nil || got != "at the top" {
		t.Fatalf("2.0 does not see the whole Documents folder: %q, %v", got, err)
	}
	if got, err := liveExec(t, inst.ID, "cat", "/moose/documents/Notebooks/nb.txt"); err != nil || got != "in the subfolder" {
		t.Fatalf("2.0 does not see the subfolder inside it: %q, %v", got, err)
	}
	t.Log("2.0 binds all of Documents")
}

// TestLiveApprovedUpdateTurnsInternetOn: 2.0 turns the internet on. A 2.0
// that never becomes healthy is rolled back, and the network is internal
// again; a good 2.0 leaves it with a route out, with the app on it.
func TestLiveApprovedUpdateTurnsInternetOn(t *testing.T) {
	e := newLiveUpdateEnv(t)
	const id = "liveapprovenet"
	e.publish(t, id, liveAppManifest(id, "1.0", ""), liveAppCompose(liveImage, "true", false, ""))
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, id), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = e.m.Uninstall(context.Background(), inst.ID) })
	if !liveNetworkInternal(t, inst.ID) {
		t.Fatal("1.0's network has a route out")
	}
	on := func(m string) string { return strings.Replace(m, "internet: false", "internet: true", 1) }

	e.publish(t, id, on(liveAppManifest(id, "2.0", "")), liveAppCompose(liveImage, "true", true, ""))
	if err := approveAndUpdate(t, e, inst.ID, "2.0"); err == nil {
		t.Fatal("a 2.0 that never becomes healthy went through")
	}
	if !liveNetworkInternal(t, inst.ID) {
		t.Fatal("after the rollback the network has a route out")
	}
	if !liveOnNetwork(t, inst.ID) {
		t.Fatal("after the rollback 1.0 is not on its network")
	}
	if running, _, err := e.m.docker.Inspect(context.Background(), inst.ID, "app"); err != nil || !running {
		t.Fatalf("1.0 does not run after the rollback: %v %v", running, err)
	}
	t.Log("the rollback put the network back to internal")

	// The same 2.0, now healthy. The approval is still there and covers it.
	e.publish(t, id, on(liveAppManifest(id, "2.0", "")), liveAppCompose(liveImage, "true", false, ""))
	if err := e.m.Update(context.Background(), inst.ID, func(s string) { t.Logf("step %s", s) }); err != nil {
		t.Fatalf("update: %v", err)
	}
	if liveNetworkInternal(t, inst.ID) {
		t.Fatal("after the update the network is still internal")
	}
	if !liveOnNetwork(t, inst.ID) {
		t.Fatal("2.0 is not on the new network")
	}
	t.Log("2.0 runs on a network with a route out")
}

// TestLiveAutoUpdateTurnsInternetOff: 2.0 turns the internet off. That asks
// for less, so it is applied with no question, and the network becomes
// internal (the #631 update kept the old network).
func TestLiveAutoUpdateTurnsInternetOff(t *testing.T) {
	ctx := context.Background()
	e := newLiveUpdateEnv(t)
	const id = "liveautonetoff"
	e.publish(t, id, strings.Replace(liveAppManifest(id, "1.0", ""), "internet: false", "internet: true", 1), liveAppCompose(liveImage, "true", false, ""))
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, id), Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	t.Cleanup(func() { _ = e.m.Uninstall(context.Background(), inst.ID) })
	if liveNetworkInternal(t, inst.ID) {
		t.Fatal("1.0's network has no route out")
	}
	e.publish(t, id, liveAppManifest(id, "2.0", ""), liveAppCompose(liveImage, "true", false, ""))
	e.m.CheckUpdates(ctx)
	if row, _ := e.st.Get(inst.ID); row.Update.Status != store.UpdateAvailable {
		t.Fatalf("check = %+v, want available", row.Update)
	}
	if err := e.m.Update(ctx, inst.ID, func(s string) { t.Logf("step %s", s) }); err != nil {
		t.Fatalf("update: %v", err)
	}
	if !liveNetworkInternal(t, inst.ID) {
		t.Fatal("after the update the network still has a route out")
	}
	if !liveOnNetwork(t, inst.ID) {
		t.Fatal("2.0 is not on the new network")
	}
	t.Log("2.0 runs on an internal network")
}
