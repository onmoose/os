package lifecycle

// The owner's approval of an update that asks for more access (#633,
// UPDATES.md # 4 and # 6): the structured diff, the approval rule in
// classifyUpdate, ApproveUpdate's refusals, the guard that keeps an update
// that is not approved from being applied, and what an approved update binds
// and turns on.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

func accessMan(p manifest.Permissions, publicPaths ...string) *manifest.Manifest {
	return &manifest.Manifest{Version: "2.0", Permissions: p, Access: manifest.Access{PublicPaths: publicPaths}}
}

func TestWidenedAccess(t *testing.T) {
	had := grantedAccess{
		Permissions: manifest.Permissions{
			Folders: []manifest.Folder{
				{Folder: "photos", Mode: "read", Scope: "whole"},
				{Folder: "documents", Mode: "read", Scope: "pick-subfolder", Default: "Work"},
				// The user skipped notes at install: it has no bind in sources.
				{Folder: "notes", Mode: "read", Optional: true},
			},
			ExtraFolders: "read",
			Devices:      []string{"/dev/ttyUSB0"},
		},
		PublicPaths: []string{"/api/"},
	}
	sources := map[string]string{"photos": sourceShared, "documents": sourcePersonal}
	same := had.Permissions
	with := func(f func(p *manifest.Permissions)) manifest.Permissions {
		p := same
		p.Folders = slices.Clone(same.Folders)
		p.Devices = slices.Clone(same.Devices)
		f(&p)
		return p
	}
	for name, tc := range map[string]struct {
		man   *manifest.Manifest
		scope string
		want  []store.AccessWant
	}{
		"same":     {man: accessMan(same, "/api/")},
		"narrower": {man: accessMan(manifest.Permissions{Folders: same.Folders[:1]})},
		"internet": {
			man:  accessMan(with(func(p *manifest.Permissions) { p.Internet = true }), "/api/"),
			want: []store.AccessWant{{Kind: store.WantInternet}},
		},
		"lan": {
			man:  accessMan(with(func(p *manifest.Permissions) { p.LAN = true }), "/api/"),
			want: []store.AccessWant{{Kind: store.WantLAN}},
		},
		"gpu": {
			man:  accessMan(with(func(p *manifest.Permissions) { p.GPU = true }), "/api/"),
			want: []store.AccessWant{{Kind: store.WantGPU}},
		},
		"device": {
			man:  accessMan(with(func(p *manifest.Permissions) { p.Devices = append(p.Devices, "/dev/dri") }), "/api/"),
			want: []store.AccessWant{{Kind: store.WantDevice, Device: "/dev/dri"}},
		},
		"new folder, personal app": {
			man: accessMan(with(func(p *manifest.Permissions) {
				p.Folders = append(p.Folders, manifest.Folder{Folder: "movies", Mode: "write"})
			}), "/api/"),
			scope: store.ScopePersonal,
			want:  []store.AccessWant{{Kind: store.WantFolder, Folder: "movies", Mode: "write", Change: store.FolderNew, Source: sourcePersonal}},
		},
		"new folder, household app": {
			man: accessMan(with(func(p *manifest.Permissions) {
				p.Folders = append(p.Folders, manifest.Folder{Folder: "movies"})
			}), "/api/"),
			scope: store.ScopeHousehold,
			want:  []store.AccessWant{{Kind: store.WantFolder, Folder: "movies", Mode: "read", Change: store.FolderNew, Source: sourceShared}},
		},
		"read to write": {
			man:  accessMan(with(func(p *manifest.Permissions) { p.Folders[0].Mode = "write" }), "/api/"),
			want: []store.AccessWant{{Kind: store.WantFolder, Folder: "photos", Mode: "write", Change: store.FolderWrite, Source: sourceShared}},
		},
		// scope means nothing since #655: the folder the user chose stays.
		"subfolder to whole is no more access": {
			man: accessMan(with(func(p *manifest.Permissions) { p.Folders[1].Scope = "whole" }), "/api/"),
		},
		"new optional folder is left off": {
			man: accessMan(with(func(p *manifest.Permissions) {
				p.Folders = append(p.Folders, manifest.Folder{Folder: "movies", Mode: "write", Optional: true})
			}), "/api/"),
		},
		"skipped folder becomes required": {
			man:  accessMan(with(func(p *manifest.Permissions) { p.Folders[2].Optional = false }), "/api/"),
			want: []store.AccessWant{{Kind: store.WantFolder, Folder: "notes", Mode: "read", Change: store.FolderNew, Source: sourcePersonal}},
		},
		"skipped folder that stays optional gets write": {
			man: accessMan(with(func(p *manifest.Permissions) { p.Folders[2].Mode = "write" }), "/api/"),
		},
		"required folder becomes optional": {
			man: accessMan(with(func(p *manifest.Permissions) { p.Folders[0].Optional = true }), "/api/"),
		},
		"no extra folder added: read to write is no more access": {
			man: accessMan(with(func(p *manifest.Permissions) { p.ExtraFolders = "write" }), "/api/"),
		},
		"write to read is narrower": {
			man: accessMan(with(func(p *manifest.Permissions) { p.Folders[0].Mode = "read" }), "/api/"),
		},
		"new public path": {
			man:  accessMan(same, "/api/", "/share/"),
			want: []store.AccessWant{{Kind: store.WantPublicPath, Path: "/share/"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			scope := tc.scope
			if scope == "" {
				scope = store.ScopePersonal
			}
			if got := widenedAccess(had, tc.man, sources, scope); !slices.Equal(got, tc.want) {
				t.Fatalf("widenedAccess = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// extra_folders asks for more only when it goes from read to write and the
// app has an extra folder: then one item covers all of them (#655).
func TestWidenedAccess_ExtraFolders(t *testing.T) {
	had := grantedAccess{Permissions: manifest.Permissions{ExtraFolders: "read"}}
	extras := map[string]string{"extra/Taxes": sourceShared, "extra/Recipes": sourcePersonal}
	got := widenedAccess(had, accessMan(manifest.Permissions{ExtraFolders: "write"}), extras, store.ScopePersonal)
	want := []store.AccessWant{{Kind: store.WantFolder, Folder: "extra", Mode: "write", Change: store.FolderWrite, Source: sourcePersonal}}
	if !slices.Equal(got, want) {
		t.Fatalf("read to write with extra folders = %+v, want %+v", got, want)
	}
	none := grantedAccess{}
	if got := widenedAccess(none, accessMan(manifest.Permissions{ExtraFolders: "write"}), nil, store.ScopePersonal); len(got) != 0 {
		t.Fatalf("a new extra_folders grants nothing yet, got %+v", got)
	}
}

// approvalFor is the approval an owner gives for man.
func approvalFor(t *testing.T, man *manifest.Manifest) store.UpdateApproval {
	t.Helper()
	b, err := json.Marshal(accessOf(man))
	if err != nil {
		t.Fatal(err)
	}
	return store.UpdateApproval{Version: man.Version, Access: string(b), By: "u_alex"}
}

func TestClassifyUpdateWithApproval(t *testing.T) {
	oldMan := &manifest.Manifest{Version: "1.0"}
	v2 := &manifest.Manifest{Version: "2.0", Permissions: manifest.Permissions{Internet: true}}
	inst := store.Instance{ID: "i", Scope: store.ScopePersonal, UsernsTier: store.UsernsTierHost}

	if c := classifyUpdate(inst, oldMan, v2, nil, nil, 0, nil); c.Status != store.UpdateNeedsApproval || !strings.Contains(c.Reason, "its owner allows it") {
		t.Fatalf("no approval: %+v", c)
	}
	inst.UpdateApproval = approvalFor(t, v2)
	c := classifyUpdate(inst, oldMan, v2, nil, nil, 0, nil)
	if c.Status != store.UpdateAvailable || !slices.Equal(c.Wants, []store.AccessWant{{Kind: store.WantInternet}}) {
		t.Fatalf("covered: %+v, want available with the list kept", c)
	}

	// v3 asks for no more than v2 did (v2 was allowed and rolled back): no
	// second question.
	v3 := &manifest.Manifest{Version: "3.0", Permissions: manifest.Permissions{Internet: true}}
	if c := classifyUpdate(inst, oldMan, v3, nil, nil, 0, nil); c.Status != store.UpdateAvailable {
		t.Fatalf("within the approval: %+v", c)
	}
	// v4 asks for more than was allowed: it asks again.
	v4 := &manifest.Manifest{Version: "4.0", Permissions: manifest.Permissions{Internet: true, LAN: true}}
	if c := classifyUpdate(inst, oldMan, v4, nil, nil, 0, nil); c.Status != store.UpdateNeedsApproval {
		t.Fatalf("beyond the approval: %+v", c)
	}

	// A managed database's major move is no class of its own (#635): an
	// update that moves it and asks for more access waits for the yes, and
	// one that is refused (an older major) still stores the list.
	grants := []store.ServiceGrant{{LogicalName: "database", Kind: "postgres", Version: "16"}}
	v5 := &manifest.Manifest{Version: "5.0", Permissions: manifest.Permissions{LAN: true},
		Services: map[string]manifest.ServiceDep{"database": {Type: "postgres", Version: "17"}}}
	if c := classifyUpdate(inst, oldMan, v5, grants, nil, 0, nil); c.Status != store.UpdateNeedsApproval || !slices.Equal(c.Wants, []store.AccessWant{{Kind: store.WantLAN}}) {
		t.Fatalf("service major and wider: %+v", c)
	}
	v6 := &manifest.Manifest{Version: "6.0", Permissions: manifest.Permissions{LAN: true},
		Services: map[string]manifest.ServiceDep{"database": {Type: "postgres", Version: "15"}}}
	if c := classifyUpdate(inst, oldMan, v6, grants, nil, 0, nil); c.Status != store.UpdateBlocked || !slices.Equal(c.Wants, []store.AccessWant{{Kind: store.WantLAN}}) {
		t.Fatalf("older major and wider: %+v", c)
	}

	// A household app waits for an admin.
	hh := store.Instance{ID: "h", Scope: store.ScopeHousehold, UsernsTier: store.UsernsTierHost}
	if c := classifyUpdate(hh, oldMan, v2, nil, nil, 0, nil); !strings.Contains(c.Reason, "an admin allows it") {
		t.Fatalf("household reason: %q", c.Reason)
	}
}

const internetOn = "  internet: true\n"

// v2Internet publishes 2.0 of the test app with the internet turned on.
func (e *testEnv) v2Internet(t *testing.T) {
	t.Helper()
	e.writeCatalogApp(t, "upapp", upCompose(upImage2), strings.Replace(upManifest("2.0", upImage2, upDigest2, ""), "internet: false", "internet: true", 1))
}

func TestApproveUpdateRefusals(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := installUp(t, e, "")

	// No update at all.
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_admin"); !errors.Is(err, ErrNothingToApprove) {
		t.Fatalf("no update: %v", err)
	}
	// An update that asks for nothing more needs no approval.
	e.publish(t, "2.0", upImage2, upDigest2, "")
	e.m.CheckUpdates(ctx)
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_admin"); !errors.Is(err, ErrNothingToApprove) {
		t.Fatalf("available: %v", err)
	}

	e.v2Internet(t)
	if err := e.store.SetInstanceUpdateCheck(inst.ID, store.UpdateCheck{}); err != nil {
		t.Fatal(err)
	}
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateNeedsApproval {
		t.Fatalf("check = %+v", r.Update)
	}
	// The person saw another version.
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "1.5", "u_admin"); !errors.Is(err, ErrApproveStale) {
		t.Fatalf("wrong version: %v", err)
	}
	// A stopped app.
	if err := e.store.SetState(inst.ID, "stopped"); err != nil {
		t.Fatal(err)
	}
	var refusal *UpdateRefusal
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_admin"); !errors.As(err, &refusal) || !strings.Contains(err.Error(), "Start the app") {
		t.Fatalf("stopped: %v", err)
	}
	if err := e.store.SetState(inst.ID, "running"); err != nil {
		t.Fatal(err)
	}
	// The app store changed what 2.0 asks for: refused, and the new list is
	// stored so the next look shows it.
	e.writeCatalogApp(t, "upapp", upCompose(upImage2), strings.Replace(upManifest("2.0", upImage2, upDigest2, ""), "  internet: false\n  lan: false\n", internetOn+"  lan: true\n", 1))
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_admin"); !errors.Is(err, ErrApproveChanged) {
		t.Fatalf("changed manifest: %v", err)
	}
	r := e.row(t, inst.ID)
	if r.UpdateApproval != (store.UpdateApproval{}) || len(r.Update.Wants) != 2 {
		t.Fatalf("after the refusal: approval %+v wants %+v", r.UpdateApproval, r.Update.Wants)
	}
	// Now it is the same as what the person sees: approved, and available.
	wants, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_admin")
	if err != nil || len(wants) != 2 {
		t.Fatalf("approve: %v %+v", err, wants)
	}
	r = e.row(t, inst.ID)
	if r.UpdateApproval.Version != "2.0" || r.UpdateApproval.By != "u_admin" || r.Update.Status != store.UpdateAvailable {
		t.Fatalf("after the approval: approval %+v update %+v", r.UpdateApproval, r.Update)
	}
	// A later check does not undo it: the version is classified already.
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateAvailable {
		t.Fatalf("a later check moved it to %+v", r.Update)
	}
}

// An update that asks for more access is never applied without the
// approval: not by a person's Update, and not by the nightly window.
func TestUnapprovedUpdateIsNeverApplied(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.v2Internet(t)
	e.m.CheckUpdates(ctx)
	pulls := len(e.docker.pulled())
	if err := e.m.UpdateByPerson(ctx, inst.ID, nil); err == nil || !strings.Contains(err.Error(), "asks for more access") {
		t.Fatalf("a person's Update: %v", err)
	}
	// Even a row that says available (written by hand, or by a check
	// before the catalog changed) is classified again under the lock.
	if err := e.store.SetInstanceUpdateCheck(inst.ID, store.UpdateCheck{Version: "2.0", Status: store.UpdateAvailable}); err != nil {
		t.Fatal(err)
	}
	if err := e.m.Update(ctx, inst.ID, nil); err == nil {
		t.Fatal("an update that asks for more access was applied")
	}
	if len(e.docker.pulled()) != pulls {
		t.Fatal("a refused update pulled images")
	}
	n, host, now := realNight(t, e)
	if onNight(n, host, now, 7) {
		t.Fatal("the nightly window tried an update that waits for an approval")
	}
	if r := e.row(t, inst.ID); r.Version != "1.0" || r.Update.Status != store.UpdateNeedsApproval {
		t.Fatalf("row: version %q update %+v", r.Version, r.Update)
	}
}

// After an approval, a failed update keeps the approval, and the nightly
// window tries it again under the 3-failures rule. A good update clears it.
func TestApprovedUpdateFailsThenNightlyRetries(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	e.newVersionMigrates(t, false)
	e.v2Internet(t)
	e.m.CheckUpdates(ctx)
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_admin"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := e.m.UpdateByPerson(ctx, inst.ID, nil); err == nil {
		t.Fatal("2.0 updated, want it to fail")
	}
	r := e.row(t, inst.ID)
	if r.Version != "1.0" || r.UpdateApproval.Version != "2.0" || r.Update.Status != store.UpdateAvailable {
		t.Fatalf("after the failure: version %q approval %+v update %+v", r.Version, r.UpdateApproval, r.Update)
	}
	n, host, now := realNight(t, e)
	work, err := n.work("2026-11-07")
	if err != nil || len(work) != 1 || work[0].ID != inst.ID {
		t.Fatalf("work tonight = %v, %v; want the approved update", work, err)
	}

	e.newVersionMigrates(t, true)
	if !onNight(n, host, now, 7) {
		t.Fatal("the window did not try the approved update")
	}
	r = e.row(t, inst.ID)
	if r.Version != "2.0" || r.UpdateApproval != (store.UpdateApproval{}) || r.UpdateDeclined != "" {
		t.Fatalf("after a good update: version %q approval %+v declined %q", r.Version, r.UpdateApproval, r.UpdateDeclined)
	}
}

// internal is the test app network's internal flag.
func (e *testEnv) internal(t *testing.T, id string) bool {
	t.Helper()
	internal, ok := e.docker.networkInternal(appNetwork(id))
	if !ok {
		t.Fatalf("the app network of %s does not exist", id)
	}
	return internal
}

// An approved update that turns the internet on makes the app's network
// again without --internal and the containers again on it; a rollback puts
// the old flag back. A narrowing update, applied on its own, turns it off.
func TestUpdateFollowsTheInternetPermission(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := installUp(t, e, "")
	if !e.internal(t, inst.ID) {
		t.Fatal("the app network has internet before the update")
	}
	e.docker.attached[caddyContainer] = map[string]bool{appNetwork(inst.ID): true}

	// 2.0 never becomes healthy: the rollback makes the network internal again.
	e.newVersionMigrates(t, false)
	e.v2Internet(t)
	e.m.CheckUpdates(ctx)
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_admin"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := e.m.Update(ctx, inst.ID, nil); err == nil {
		t.Fatal("2.0 updated, want a rollback")
	}
	if !e.internal(t, inst.ID) {
		t.Fatal("after the rollback the app network has internet")
	}
	if creates := strings.Count(strings.Join(e.docker.methods(), " "), "ComposeCreate"); creates != 2 {
		t.Fatalf("ComposeCreate ran %d times, want 2 (the update and the rollback)", creates)
	}
	if !e.docker.attached[caddyContainer][appNetwork(inst.ID)] {
		t.Fatal("Caddy is not back on the app network")
	}

	e.newVersionMigrates(t, true)
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if e.internal(t, inst.ID) {
		t.Fatal("after the update the app network is still internal")
	}

	// 3.0 turns the internet off: narrower, applied without a question, and
	// the network is internal again (the #631 update kept the old network).
	e.writeCatalogApp(t, "upapp", upCompose(upImage1), upManifest("3.0", upImage1, upDigest1, ""))
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateAvailable {
		t.Fatalf("narrower: %+v", r.Update)
	}
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update to 3.0: %v", err)
	}
	if !e.internal(t, inst.ID) {
		t.Fatal("after the narrowing update the app network has internet")
	}
}

const upFolders = "  folders:\n    - folder: documents\n      mode: read\n      scope: pick-subfolder\n      default: Work\n"

// overrideVolumes is the main service's volumes in the override on disk.
func (e *testEnv) overrideVolumes(t *testing.T, id string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(e.m.instanceDir(id), "compose.override.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, ":/moose/") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(l), "- ")))
		}
	}
	return out
}

// An approved update that adds a folder binds the install default (the
// owner's own folder, made ready by host-agent), and a mode upgrade binds
// read-write. Since #655 the folder the user chose stays where it is, even
// when scope goes from pick-subfolder to whole.
func TestApprovedUpdateBindsNewFolders(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	if err := e.store.CreateUser(store.User{ID: "u_alex", Username: "alex", DisplayName: "Alex", Role: store.RoleMember}); err != nil {
		t.Fatal(err)
	}
	e.publish(t, "1.0", upImage1, upDigest1, upFolders)
	inst, err := e.m.Install(ctx, mustLoadApp(t, e.m, "upapp"), Owner{UserID: "u_alex", Username: "alex"}, store.ScopePersonal,
		[]FolderMount{{Folder: "documents", Source: sourcePersonal, Path: "Documents/Notebooks"}}, "", nil, nil, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	home := filepath.Join(e.host.homeRoot, "alex")
	if got := e.overrideVolumes(t, inst.ID); !slices.Equal(got, []string{home + "/Documents/Notebooks:/moose/documents:ro"}) {
		t.Fatalf("1.0 binds %v", got)
	}

	// 2.0: scope: whole on Documents, read-write, and a new Movies folder.
	v2 := strings.Replace(upFolders, "scope: pick-subfolder\n      default: Work\n", "scope: whole\n", 1)
	v2 = strings.Replace(v2, "mode: read", "mode: write", 1) + "    - folder: movies\n      mode: read\n"
	e.publish(t, "2.0", upImage2, upDigest2, v2)
	e.m.CheckUpdates(ctx)
	r := e.row(t, inst.ID)
	want := []store.AccessWant{
		{Kind: store.WantFolder, Folder: "documents", Mode: "write", Change: store.FolderWrite, Source: sourcePersonal},
		{Kind: store.WantFolder, Folder: "movies", Mode: "read", Change: store.FolderNew, Source: sourcePersonal},
	}
	if r.Update.Status != store.UpdateNeedsApproval || !slices.Equal(r.Update.Wants, want) {
		t.Fatalf("check = %+v", r.Update)
	}
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_alex"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	got := e.overrideVolumes(t, inst.ID)
	wantVols := []string{home + "/Documents/Notebooks:/moose/documents:rw", home + "/Movies:/moose/movies:ro"}
	slices.Sort(got)
	if !slices.Equal(got, wantVols) {
		t.Fatalf("2.0 binds %v, want %v", got, wantVols)
	}
	if !slices.ContainsFunc(e.host.calls, func(c call) bool { return c.String() == "PrepareUserFolder(alex,Movies)" }) {
		t.Errorf("the new Movies folder was not made ready; host calls %v", e.host.calls)
	}
}

// A widened public path takes effect with the update: the route the update
// writes carries it.
func TestApprovedUpdateOpensNewPublicPaths(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installUp(t, e, "access:\n  public_paths: [/api/]\n")
	e.newVersionMigrates(t, true)
	e.publish(t, "2.0", upImage2, upDigest2, "access:\n  public_paths: [/api/, /share/]\n")
	e.m.CheckUpdates(ctx)
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_admin"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := e.caddy.config(inst.ID).PublicPaths; !slices.Equal(got, []string{"/api/", "/share/"}) {
		t.Fatalf("route public paths = %v", got)
	}
}

// A GPU or a device on an app that has one keeps the tier, so it asks for an
// approval; and the override of the approved update has the new device.
func TestApprovedUpdateAddsADevice(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	inst := installUp(t, e, "  devices: [/dev/ttyUSB0]\n")
	e.newVersionMigrates(t, true)
	e.publish(t, "2.0", upImage2, upDigest2, "  devices: [/dev/ttyUSB0, /dev/ttyACM0]\n")
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateNeedsApproval ||
		!slices.Equal(r.Update.Wants, []store.AccessWant{{Kind: store.WantDevice, Device: "/dev/ttyACM0"}}) {
		t.Fatalf("check = %+v", r.Update)
	}
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_admin"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if err := e.m.Update(ctx, inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(e.m.instanceDir(inst.ID), "compose.override.yml"))
	if err != nil || !strings.Contains(string(b), "/dev/ttyACM0") {
		t.Fatalf("the override has no new device: %v\n%s", err, b)
	}
}

// A first folder on an app with none changes the tier on a remapped box, so
// it is blocked, not something an owner can approve.
func TestFirstFolderOnARemappedBoxIsBlocked(t *testing.T) {
	ctx := context.Background()
	e := newTestEnv(t)
	e.remapped()
	inst := installUp(t, e, "")
	e.publish(t, "2.0", upImage2, upDigest2, "  folders:\n    - folder: photos\n      mode: read\n")
	e.m.CheckUpdates(ctx)
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateBlocked {
		t.Fatalf("check = %+v, want blocked", r.Update)
	}
	if _, err := e.m.ApproveUpdate(ctx, inst.ID, "2.0", "u_admin"); !errors.Is(err, ErrNothingToApprove) {
		t.Fatalf("approve a blocked update: %v", err)
	}
}
