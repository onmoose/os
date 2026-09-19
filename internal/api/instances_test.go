package api

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/store"
)

// seedInstance writes an instance row directly, bypassing the install
// transaction (the api harness builds the server with life=nil).
func (h *harness) seedInstance(id, manifestID, slug, ownerID, scope string) {
	h.t.Helper()
	if err := h.st.Create(store.Instance{
		ID: id, ManifestID: manifestID, Name: manifestID, Slug: slug,
		Version: "1", State: "running",
		OwnerUserID: ownerID, Scope: scope, CreatedAt: time.Now(),
	}); err != nil {
		h.t.Fatalf("seed instance %q: %v", id, err)
	}
}

// seedInstanceManifest writes an instance's persisted manifest copy, the one the
// installer writes next to the installation (lifecycle writeInstanceDir). Every
// brain path that needs an INSTALLED app's manifest reads this file rather than
// the catalog (#434), so a test that exercises one has to stage it.
func (h *harness) seedInstanceManifest(id, manifestYML string) {
	h.t.Helper()
	dir := filepath.Join(h.stateDir, "instances", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		h.t.Fatalf("mkdir instance dir %q: %v", id, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yml"), []byte(manifestYML), 0o644); err != nil {
		h.t.Fatalf("write instance manifest %q: %v", id, err)
	}
}

func identity(u store.User) auth.Identity {
	return auth.Identity{User: u}
}

// --- install authorization ------------------------------------------------

func TestInstallMemberCannotChooseHousehold(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_m", "mara", "pw123456")
	h.loginAs("mara", "pw123456")

	resp := h.do("POST", "/api/v1/apps", map[string]any{
		"manifest_id": "immich", "scope": "household",
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member household install = %d, want 403", resp.StatusCode)
	}
}

func TestInstallRejectsIllegalElectionAndAudits(t *testing.T) {
	h := newHarness(t)
	writeManifestFixture(t, h.catalogDir, "jellyfin", foldersManifestYML)
	h.setupAdmin("admin", "hunter2")

	// Household install electing a personal folder source — illegal (household
	// forces shared). Must reject synchronously, before the job runs.
	resp := h.do("POST", "/api/v1/apps", map[string]any{
		"manifest_id": "jellyfin",
		"scope":       "household",
		"config": map[string]any{
			"folders": []map[string]any{{"folder": "movies", "source": "personal"}},
		},
	})
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("illegal election install = %d, want 422", resp.StatusCode)
	}

	// The rejection is an elevation-class mutation failure → audited success=false.
	ar := h.do("GET", "/api/v1/audit", nil)
	defer ar.Body.Close()
	body := decodeJSON[struct {
		Events []AuditEventDTO `json:"events"`
	}](t, ar)
	found := false
	for _, e := range body.Events {
		if e.Action == "app.install" && !e.Success {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("want an app.install success=false audit record for the rejected election")
	}
}

func TestResolveOwnerScope(t *testing.T) {
	h := newHarness(t)
	admin := store.User{ID: "u_a", Username: "andrei", Role: store.RoleAdmin}
	member := store.User{ID: "u_m", Username: "mara", Role: store.RoleMember}

	cases := []struct {
		name      string
		user      store.User
		requested string
		wantScope string
		wantOwner string
		wantErr   bool
	}{
		{"admin defaults to household", admin, "", store.ScopeHousehold, "u_a", false},
		{"admin chooses personal", admin, "personal", store.ScopePersonal, "u_a", false},
		{"admin chooses household", admin, "household", store.ScopeHousehold, "u_a", false},
		{"member forced personal", member, "", store.ScopePersonal, "u_m", false},
		{"member explicit personal", member, "personal", store.ScopePersonal, "u_m", false},
		{"member household rejected", member, "household", "", "", true},
		{"invalid scope rejected", admin, "bogus", "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auth.WithIdentity(context.Background(), identity(tc.user))
			owner, scope, err := h.srvServer().resolveOwnerScope(ctx, tc.requested, "app.install", map[string]any{"manifest_id": "immich"})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got owner=%+v scope=%q", owner, scope)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if scope != tc.wantScope {
				t.Fatalf("scope=%q, want %q", scope, tc.wantScope)
			}
			if owner.UserID != tc.wantOwner {
				t.Fatalf("owner=%q, want %q", owner.UserID, tc.wantOwner)
			}
			if scope == store.ScopePersonal && owner.Username != tc.user.Username {
				t.Fatalf("personal owner username=%q, want %q", owner.Username, tc.user.Username)
			}
		})
	}
}

// --- warn-don't-block -----------------------------------------------------

func TestInstallDuplicateWarnsThenConfirms(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	// An existing household instance of "immich".
	h.seedInstance("i1", "immich", "immich", "u_admin", store.ScopeHousehold)

	srv := h.srvServer()
	ctx := auth.WithIdentity(context.Background(), identity(store.User{ID: "u_admin", Username: "admin", Role: store.RoleAdmin}))

	// Without confirm: 409 duplicate-install.
	if err := srv.checkDuplicate(ctx, "immich", false, "app.install"); err == nil {
		t.Fatal("checkDuplicate without confirm: want 409, got nil")
	}
	// With confirm: proceeds.
	if err := srv.checkDuplicate(ctx, "immich", true, "app.install"); err != nil {
		t.Fatalf("checkDuplicate with confirm: %v", err)
	}
	// No existing instance: proceeds.
	if err := srv.checkDuplicate(ctx, "jellyfin", false, "app.install"); err != nil {
		t.Fatalf("checkDuplicate no duplicate: %v", err)
	}
}

func TestCheckDuplicateIgnoresOtherMembersPersonal(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_alex", "alex", "pw123456")
	h.addMember("u_mara", "mara", "pw123456")
	// alex has a personal immich; mara should not be warned about it.
	h.seedInstance("i1", "immich", "immich--alex", "u_alex", store.ScopePersonal)

	srv := h.srvServer()
	ctx := auth.WithIdentity(context.Background(), identity(store.User{ID: "u_mara", Username: "mara", Role: store.RoleMember}))
	if err := srv.checkDuplicate(ctx, "immich", false, "app.install"); err != nil {
		t.Fatalf("mara should not see alex's personal immich: %v", err)
	}
}

// --- read scoping ---------------------------------------------------------

func TestListAppsScopedToCaller(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_alex", "alex", "pw123456")
	h.addMember("u_mara", "mara", "pw123456")
	h.seedInstance("h1", "jellyfin", "jellyfin", "u_admin", store.ScopeHousehold)
	h.seedInstance("p1", "immich", "immich--alex", "u_alex", store.ScopePersonal)
	h.seedInstance("p2", "immich", "immich--mara", "u_mara", store.ScopePersonal)

	h.loginAs("alex", "pw123456")
	resp := h.do("GET", "/api/v1/apps", nil)
	body := decodeJSON[struct {
		Apps []InstanceDTO `json:"apps"`
	}](t, resp)

	got := map[string]bool{}
	for _, a := range body.Apps {
		got[a.ID] = true
	}
	if !got["h1"] || !got["p1"] {
		t.Fatalf("alex should see household + own personal, got %v", got)
	}
	if got["p2"] {
		t.Fatal("alex must not see mara's personal instance")
	}
}

// TestVisibilityPredicatesAgree is the tripwire for the canSee (Go) /
// ListVisibleTo (SQL) duplication called out in review: for every user, the
// SQL-scoped list must equal the full list filtered through canSee. When
// per-app member grants land and one side changes, this fails loudly.
func TestVisibilityPredicatesAgree(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_alex", "alex", "pw123456")
	h.addMember("u_mara", "mara", "pw123456")
	h.seedInstance("h1", "jellyfin", "jellyfin", "u_admin", store.ScopeHousehold)
	h.seedInstance("p1", "immich", "immich--alex", "u_alex", store.ScopePersonal)
	h.seedInstance("p2", "immich", "immich--mara", "u_mara", store.ScopePersonal)

	all, err := h.st.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	users := []struct {
		id      string
		isAdmin bool
	}{{"u_admin", true}, {"u_alex", false}, {"u_mara", false}}

	for _, u := range users {
		visible, err := h.st.ListVisibleTo(u.id, u.isAdmin)
		if err != nil {
			t.Fatalf("ListVisibleTo(%s): %v", u.id, err)
		}
		sqlSet := map[string]bool{}
		for _, i := range visible {
			sqlSet[i.ID] = true
		}
		ident := identity(store.User{ID: u.id, Role: roleFor(u.isAdmin)})
		for _, i := range all {
			want := canSee(ident, i)
			if want != sqlSet[i.ID] {
				t.Fatalf("user %s instance %s: canSee=%v but ListVisibleTo=%v", u.id, i.ID, want, sqlSet[i.ID])
			}
		}
	}
}

func roleFor(isAdmin bool) string {
	if isAdmin {
		return store.RoleAdmin
	}
	return store.RoleMember
}

func TestGetAppLeakGuard(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "hunter2")
	h.addMember("u_alex", "alex", "pw123456")
	h.addMember("u_mara", "mara", "pw123456")
	h.seedInstance("p1", "immich", "immich--alex", "u_alex", store.ScopePersonal)

	h.loginAs("mara", "pw123456")
	resp := h.do("GET", "/api/v1/apps/p1", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("mara GET alex's personal app = %d, want 404", resp.StatusCode)
	}
}
