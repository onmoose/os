package api

// GET /apps/{id}/secrets reveals an instance's owner-visible secrets (#152).
// Authorization mirrors stop/start (authorizeAppMutation): owner-or-admin, with
// a 404 leak-guard for a member peeking at someone else's personal app and a 403
// for a member on a household app. Built direct-handler (like system_test.go's
// host-dependent cases) because a real reveal needs an installed instance dir +
// store secrets the http harness's nil-driver lifecycle can't produce.

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/store"
)

const revealManifestYAML = `
id: revealapp
manifest_version: 1
name: Reveal App
version: "1.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [revealapp]
secrets:
  - name: setup_token
    show: true
  - name: db
`

// secretServer builds a Server over a temp store + lifecycle Manager and seeds
// one installed instance (owner ownerID, given scope) whose manifest declares an
// owner-visible `setup_token` and an internal `db`. Returns the server, the
// instance id, and its on-disk instance dir (so the 500 case can remove it).
func secretServer(t *testing.T, ownerID, scope string) (*Server, string, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "moose.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	life := lifecycle.NewManager(st, catalog.New(t.TempDir()), nil, nil, nil, events.NewBus(), dir)

	const id = "inst_reveal"
	if err := st.Create(store.Instance{
		ID: id, ManifestID: "revealapp", Name: "Reveal App", Slug: "revealapp",
		Version: "1.0", State: "running", OwnerUserID: ownerID, Scope: scope,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	instDir := filepath.Join(dir, "instances", id)
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatalf("mkdir instance dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "manifest.yml"), []byte(revealManifestYAML), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	if err := st.SetInstanceSecrets(id, []store.InstanceSecret{
		{Name: "setup_token", Value: "TOKENVAL"}, {Name: "db", Value: "DBVAL"},
	}); err != nil {
		t.Fatalf("seed secrets: %v", err)
	}
	return &Server{store: st, life: life}, id, instDir
}

func reveal(t *testing.T, s *Server, ctx context.Context, id string) (AppSecretsDTO, error) {
	t.Helper()
	out, err := s.appSecrets(ctx, &struct {
		ID string `path:"id"`
	}{ID: id})
	if err != nil {
		return AppSecretsDTO{}, err
	}
	return out.Body, nil
}

func memberCtx(userID string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{
		User: store.User{ID: userID, Role: store.RoleMember},
	})
}

func adminCtx(userID string) context.Context {
	return auth.WithIdentity(context.Background(), auth.Identity{
		User: store.User{ID: userID, Role: store.RoleAdmin},
	})
}

// assertStatus fails unless err is a huma StatusError with the wanted code.
func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	var se huma.StatusError
	if !errors.As(err, &se) || se.GetStatus() != want {
		t.Fatalf("want status %d, got %v", want, err)
	}
}

// TestAppSecrets_RequiresAuth: the route is registered and fenced — an
// unauthenticated request never reaches the handler. Exercised through the full
// router so the wiring (registerAppSecrets) is covered, not just the handler.
func TestAppSecrets_RequiresAuth(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/api/v1/apps/inst_x/secrets", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET secrets: want 401, got %d", resp.StatusCode)
	}
}

// TestAppSecrets_NoIdentity_401: the handler's own guard (via authorizeAppMutation)
// returns 401 when no identity rode the context.
func TestAppSecrets_NoIdentity_401(t *testing.T) {
	s, id, _ := secretServer(t, "u_owner", store.ScopePersonal)
	_, err := reveal(t, s, context.Background(), id)
	assertStatus(t, err, http.StatusUnauthorized)
}

// TestAppSecrets_OwnerSeesOnlyVisible: the owning member gets the `show: true`
// secret with its real value and never the internal `db`.
func TestAppSecrets_OwnerSeesOnlyVisible(t *testing.T) {
	s, id, _ := secretServer(t, "u_owner", store.ScopePersonal)
	body, err := reveal(t, s, memberCtx("u_owner"), id)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if len(body.Secrets) != 1 {
		t.Fatalf("want 1 revealed secret, got %+v", body.Secrets)
	}
	if body.Secrets[0].Name != "setup_token" || body.Secrets[0].Value != "TOKENVAL" {
		t.Fatalf("revealed wrong secret: %+v", body.Secrets[0])
	}
}

// TestAppSecrets_AdminSeesAny: an admin reveals a household app's secret (no
// single owner on a household instance, so admin is the authorized reader).
func TestAppSecrets_AdminSeesAny(t *testing.T) {
	s, id, _ := secretServer(t, "u_someone", store.ScopeHousehold)
	body, err := reveal(t, s, adminCtx("u_admin"), id)
	if err != nil {
		t.Fatalf("reveal: %v", err)
	}
	if len(body.Secrets) != 1 || body.Secrets[0].Name != "setup_token" {
		t.Fatalf("admin reveal: %+v", body.Secrets)
	}
}

// TestAppSecrets_OtherMember_404: a member asking for another user's personal
// app gets 404 (existence leak guard), not 403.
func TestAppSecrets_OtherMember_404(t *testing.T) {
	s, id, _ := secretServer(t, "u_owner", store.ScopePersonal)
	_, err := reveal(t, s, memberCtx("u_intruder"), id)
	assertStatus(t, err, http.StatusNotFound)
}

// TestAppSecrets_MemberHousehold_403: a household app is admin-only, so a member
// is forbidden (it's visible to them, hence 403 not 404).
func TestAppSecrets_MemberHousehold_403(t *testing.T) {
	s, id, _ := secretServer(t, "u_someone", store.ScopeHousehold)
	_, err := reveal(t, s, memberCtx("u_member"), id)
	assertStatus(t, err, http.StatusForbidden)
}

// TestAppSecrets_RevealError_500: a missing instance dir (manifest load fails)
// surfaces as 500 rather than an empty 200.
func TestAppSecrets_RevealError_500(t *testing.T) {
	s, id, instDir := secretServer(t, "u_owner", store.ScopePersonal)
	if err := os.RemoveAll(instDir); err != nil {
		t.Fatalf("remove instance dir: %v", err)
	}
	_, err := reveal(t, s, memberCtx("u_owner"), id)
	assertStatus(t, err, http.StatusInternalServerError)
}
