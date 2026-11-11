package lifecycle

// Folder source pin scenarios (#604): the brain writes compose.pins.yml before
// every compose up, refuses a start when the folder is now a link, and falls
// back to the prepare-folder re-check on an old host-agent.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

// installPinnedApp installs filesapp with one folder mount and returns the
// instance plus the real source path. It creates the source directory, which
// the fake host-agent does not create (the real prepare-folder does).
func installPinnedApp(t *testing.T, e *testEnv, scope string, owner Owner, folder FolderMount) (store.Instance, string) {
	t.Helper()
	e.writeCatalogApp(t, "filesapp", foldersCompose, foldersManifest("write", "whole"))
	e.docker.digests[testImage] = testDigest
	root := e.m.sharedRoot
	if scope == store.ScopePersonal {
		root = filepath.Join(e.host.homeRoot, owner.Username)
		// A start has only the row, so the owner must be a real user row for
		// refreshFolderPins to resolve the username.
		if err := e.store.CreateUser(store.User{ID: owner.UserID, Username: owner.Username, DisplayName: owner.Username, Role: store.RoleMember}); err != nil {
			t.Fatalf("create the owner: %v", err)
		}
	}
	src := filepath.Join(root, filepath.FromSlash(folder.relPath()))
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatalf("make the source: %v", err)
	}
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "filesapp"), owner, scope, []FolderMount{folder}, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return inst, src
}

func pinsFile(e *testEnv, id string) string {
	return filepath.Join(e.stateDir, "instances", id, folderPinsFile)
}

func pinRecord(e *testEnv, id string) protocol.FolderPinRecord {
	raw, err := os.ReadFile(filepath.Join(e.stateDir, "instances", id, folderPinRecord))
	if err != nil {
		return protocol.FolderPinRecord{}
	}
	var rec protocol.FolderPinRecord
	_ = json.Unmarshal(raw, &rec)
	return rec
}

// TestInstallPinsFolderSource: the install writes the pin file with the pin
// path as the volume source, keeps the target and mode, and records what the
// boot restorer needs.
func TestInstallPinsFolderSource(t *testing.T) {
	e := newTestEnv(t)
	e.host.pinPath = "/run/moose/folder-pins/8-4242"
	inst, src := installPinnedApp(t, e, store.ScopePersonal,
		Owner{UserID: "u_alex", Username: "alex"},
		FolderMount{Folder: "documents", Source: sourcePersonal, Path: "Documents/Notebooks"})

	raw, err := os.ReadFile(pinsFile(e, inst.ID))
	if err != nil {
		t.Fatalf("read compose.pins.yml: %v", err)
	}
	want := e.host.pinPath + ":/moose/documents:rw"
	if !strings.Contains(string(raw), want) {
		t.Errorf("compose.pins.yml: want %q, got:\n%s", want, raw)
	}
	if strings.Contains(string(raw), src+":/moose/documents") {
		t.Errorf("compose.pins.yml still binds the real path %q:\n%s", src, raw)
	}

	rec := pinRecord(e, inst.ID)
	if len(rec.Pins) != 1 {
		t.Fatalf("folder-pins.json: want one pin, got %+v", rec)
	}
	got := rec.Pins[0]
	if got.Pin != e.host.pinPath || got.Shared || got.Username != "alex" || got.Path != "Documents/Notebooks" {
		t.Errorf("folder-pins.json: got %+v", got)
	}
}

// TestStartRefusesSwappedFolderSource is the issue's exact test: the app is
// stopped, the folder is swapped for a symlink to the owner's ~/.ssh, and the
// next start refuses instead of binding the link.
func TestStartRefusesSwappedFolderSource(t *testing.T) {
	e := newTestEnv(t)
	inst, src := installPinnedApp(t, e, store.ScopePersonal,
		Owner{UserID: "u_alex", Username: "alex"},
		FolderMount{Folder: "documents", Source: sourcePersonal, Path: "Documents/Notebooks"})

	ctx := context.Background()
	if err := e.m.Stop(ctx, inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	ssh := filepath.Join(e.host.homeRoot, "alex", ".ssh")
	if err := os.MkdirAll(ssh, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, src+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(ssh, src); err != nil {
		t.Fatal(err)
	}

	e.docker.mu.Lock()
	e.docker.calls = nil
	e.docker.mu.Unlock()

	err := e.m.Start(ctx, inst.ID)
	if err == nil {
		t.Fatal("want a refusal when the folder source is a symlink")
	}
	if !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("refusal should say the folder is not a folder any more, got %q", err)
	}
	if e.docker.called("ComposeUp") {
		t.Error("want no compose up after the refusal")
	}
	row, gerr := e.store.Get(inst.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if row.State != "failed" {
		t.Errorf("state after the refusal = %q, want failed", row.State)
	}
}

// TestStartRefusesSwappedSharedFolderSource is the same refusal on the
// household tree, which host-agent walks from SharedRoot.
func TestStartRefusesSwappedSharedFolderSource(t *testing.T) {
	e := newTestEnv(t)
	inst, src := installPinnedApp(t, e, store.ScopeHousehold,
		Owner{UserID: "u_admin", Username: "admin"},
		FolderMount{Folder: "documents", Source: sourceShared, Path: "Documents/Notebooks"})

	ctx := context.Background()
	if err := e.m.Stop(ctx, inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := os.Rename(src, src+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.m.sharedRoot, "Secret"), src); err != nil {
		t.Fatal(err)
	}

	err := e.m.Start(ctx, inst.ID)
	if err == nil {
		t.Fatal("want a refusal when the shared folder source is a symlink")
	}
	if !strings.Contains(err.Error(), "not a folder") {
		t.Errorf("refusal should say the folder is not a folder any more, got %q", err)
	}
}

// TestStartFallsBackOnOldHostAgent: a host-agent with no pin op keeps the app
// running through the prepare-folder re-check, and the pins file is removed so
// compose binds the real path as before.
func TestStartFallsBackOnOldHostAgent(t *testing.T) {
	e := newTestEnv(t)
	e.host.pinUnsupported = true
	inst, _ := installPinnedApp(t, e, store.ScopePersonal,
		Owner{UserID: "u_alex", Username: "alex"},
		FolderMount{Folder: "documents", Source: sourcePersonal, Path: "Documents/Notebooks"})

	if _, err := os.Stat(pinsFile(e, inst.ID)); !os.IsNotExist(err) {
		t.Fatalf("want no compose.pins.yml on the fallback path, stat err = %v", err)
	}

	ctx := context.Background()
	if err := e.m.Stop(ctx, inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := e.m.Start(ctx, inst.ID); err != nil {
		t.Fatalf("start on the old-agent fallback: %v", err)
	}
	if _, err := os.Stat(pinsFile(e, inst.ID)); !os.IsNotExist(err) {
		t.Fatalf("want no compose.pins.yml after a fallback start, stat err = %v", err)
	}
	prepares := 0
	for _, c := range e.host.calls {
		if c.method == "PrepareUserFolder" {
			prepares++
		}
	}
	if prepares < 2 {
		t.Errorf("want PrepareUserFolder again at start (install + start), got %d calls", prepares)
	}
}

// TestReconcileMarksSwappedFolderFailed: a brain restart sees a running row
// with no containers, tries to bring it up, and marks it failed when the
// folder is a link instead of retrying it every pass.
func TestReconcileMarksSwappedFolderFailed(t *testing.T) {
	e := newTestEnv(t)
	inst, src := installPinnedApp(t, e, store.ScopePersonal,
		Owner{UserID: "u_alex", Username: "alex"},
		FolderMount{Folder: "documents", Source: sourcePersonal, Path: "Documents/Notebooks"})

	ctx := context.Background()
	if err := e.m.Stop(ctx, inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := e.store.SetState(inst.ID, "running"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, src+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.host.homeRoot, "alex", ".ssh"), src); err != nil {
		t.Fatal(err)
	}
	e.docker.psManaged = map[string]bool{}

	if err := e.m.Reconcile(ctx); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	row, err := e.store.Get(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "failed" {
		t.Errorf("state after reconcile = %q, want failed", row.State)
	}
}

// TestReconcileStopsAppWithPinFailureMarker: the boot restorer could not
// restore a pin (the source was swapped while the box was off) and wrote its
// marker. Docker has the container up on an empty pin path. The startup pass
// must stop it and mark the row failed, not leave it running.
func TestReconcileStopsAppWithPinFailureMarker(t *testing.T) {
	e := newTestEnv(t)
	inst, src := installPinnedApp(t, e, store.ScopePersonal,
		Owner{UserID: "u_alex", Username: "alex"},
		FolderMount{Folder: "documents", Source: sourcePersonal, Path: "Documents/Notebooks"})

	// The source is still swapped, so the retry refuses.
	if err := os.Rename(src, src+".real"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(e.host.homeRoot, "alex", ".ssh"), src); err != nil {
		t.Fatal(err)
	}
	// The container is up, so the normal running path would leave it alone.
	e.docker.psManaged = map[string]bool{inst.ID: true}
	marker := filepath.Join(e.stateDir, "instances", inst.ID, protocol.FolderPinFailedFile)
	b, err := json.Marshal(protocol.FolderPinFailure{Path: "Documents/Notebooks", Reason: "the source is not a real folder"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, b, 0o644); err != nil {
		t.Fatal(err)
	}

	e.docker.mu.Lock()
	e.docker.calls = nil
	e.docker.mu.Unlock()

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	row, err := e.store.Get(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "failed" {
		t.Errorf("state after a pin-failure marker = %q, want failed", row.State)
	}
	if !e.docker.called("ComposeStop") {
		t.Error("want a compose stop for the app Docker started on an empty pin path")
	}
}

// TestReconcileClearsPinFailureWhenFolderIsBack: a marker left by the boot
// restorer is retried. If the folder is good again, the up pins it, recreates
// the container and removes the marker, so the app comes back by itself.
func TestReconcileClearsPinFailureWhenFolderIsBack(t *testing.T) {
	e := newTestEnv(t)
	inst, _ := installPinnedApp(t, e, store.ScopePersonal,
		Owner{UserID: "u_alex", Username: "alex"},
		FolderMount{Folder: "documents", Source: sourcePersonal, Path: "Documents/Notebooks"})

	e.docker.psManaged = map[string]bool{inst.ID: true}
	marker := filepath.Join(e.stateDir, "instances", inst.ID, protocol.FolderPinFailedFile)
	b, err := json.Marshal(protocol.FolderPinFailure{Path: "Documents/Notebooks", Reason: "the source is not a real folder"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, b, 0o644); err != nil {
		t.Fatal(err)
	}

	e.docker.mu.Lock()
	e.docker.calls = nil
	e.docker.mu.Unlock()

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	row, err := e.store.Get(inst.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "running" {
		t.Errorf("state after the retry = %q, want running", row.State)
	}
	if !e.docker.called("ComposeStop") || !e.docker.called("ComposeUp") {
		t.Error("want a compose stop then up, so the container takes the restored pin")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("want the pin-failure marker removed, stat err = %v", err)
	}
}

// TestFolderPinsErrorIsTyped keeps the plain refusal checkable by the start and
// update callers, not just a string.
func TestFolderPinsErrorIsTyped(t *testing.T) {
	var err error = &folderRefusal{msg: "the folder is gone"}
	var refusal *folderRefusal
	if !errors.As(err, &refusal) {
		t.Fatal("want folderRefusal to be reachable with errors.As")
	}
}

// TestStartRefusesABindOutsideTheFolders: an absolute bind in the override
// that is under neither the owner's home, the shared tree nor the app's own
// dir would be bound by its real path, unpinned. The start refuses it (fail
// closed) and runs no compose up.
func TestStartRefusesABindOutsideTheFolders(t *testing.T) {
	e := newTestEnv(t)
	inst, _ := installPinnedApp(t, e, store.ScopePersonal,
		Owner{UserID: "u_alex", Username: "alex"},
		FolderMount{Folder: "documents", Source: sourcePersonal, Path: "Documents/Notebooks"})
	ctx := context.Background()
	if err := e.m.Stop(ctx, inst.ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	ovPath := filepath.Join(e.stateDir, "instances", inst.ID, "compose.override.yml")
	raw, err := os.ReadFile(ovPath)
	if err != nil {
		t.Fatal(err)
	}
	// Add a second bind with an absolute source outside every allowed root,
	// next to the folder bind the override already has.
	ov := strings.Replace(string(raw), ":/moose/documents:rw", ":/moose/documents:rw\n            - /etc:/moose/etc:ro", 1)
	if ov == string(raw) {
		t.Fatalf("could not find the folder bind in the override:\n%s", raw)
	}
	if err := os.WriteFile(ovPath, []byte(ov), 0o644); err != nil {
		t.Fatal(err)
	}

	e.docker.mu.Lock()
	e.docker.calls = nil
	e.docker.mu.Unlock()

	err = e.m.Start(ctx, inst.ID)
	var refusal *folderRefusal
	if !errors.As(err, &refusal) {
		t.Fatalf("start = %v; want a folder refusal for a bind outside the folders", err)
	}
	if e.docker.called("ComposeUp") {
		t.Error("want no compose up after the refusal")
	}
}

// TestReconcilePinRetryUpFailureBringsAppUpAgain: a pin-failure marker is
// retried with a stop and an up. When that up fails for a reason that is not a
// folder refusal, the containers are down, so the running path brings the app
// up once more in the same pass instead of trusting the container list read
// before the stop (it would otherwise leave the app stopped with its row
// saying running).
func TestReconcilePinRetryUpFailureBringsAppUpAgain(t *testing.T) {
	e := newTestEnv(t)
	inst, _ := installPinnedApp(t, e, store.ScopePersonal,
		Owner{UserID: "u_alex", Username: "alex"},
		FolderMount{Folder: "documents", Source: sourcePersonal, Path: "Documents/Notebooks"})

	e.docker.psManaged = map[string]bool{inst.ID: true}
	marker := filepath.Join(e.stateDir, "instances", inst.ID, protocol.FolderPinFailedFile)
	b, err := json.Marshal(protocol.FolderPinFailure{Path: "Documents/Notebooks", Reason: "the source is not a real folder"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, b, 0o644); err != nil {
		t.Fatal(err)
	}
	ups := 0
	e.docker.composeUp = func(context.Context, string, string) (string, error) {
		ups++
		if ups == 1 {
			return "boom", errors.New("compose up failed")
		}
		return "", nil
	}

	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if ups != 2 {
		t.Errorf("compose up ran %d times, want 2 (the marker retry, then the running path)", ups)
	}
	if row, err := e.store.Get(inst.ID); err != nil || row.State != "running" {
		t.Errorf("row = %q, %v; want running", row.State, err)
	}
}
