package api

// The owner's yes and no to an app update that asks for more access, and the
// prompt's close for a session (#633). The server here has a real lifecycle
// over a catalog that offers 2.0 of the seeded app with the internet turned
// on. The update job that a yes starts fails at the pull (the fake Docker has
// no images), which is enough to see the job and its audit record; the
// update transaction itself is tested in internal/lifecycle.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/admission"
	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

const approveV2Manifest = `
id: cfgapp
manifest_version: 1
name: Cfg App
version: "2.0"
compose_file: compose.yml
main_service: app
main_port: 8080
preferred_slugs: [cfgapp]
permissions:
  internet: true
`

const approveV2Compose = "services:\n  app:\n    image: example/cfgapp:2\n"

// approveHost answers the identity read; nothing else is reached.
type approveHost struct{ lifecycle.HostDriver }

func (approveHost) WellKnownIdentity(context.Context) (protocol.WellKnownIdentityResponse, error) {
	return protocol.WellKnownIdentityResponse{MooseAppUID: 2000, MooseAppGID: 2000, MooseSharedGID: 2001}, nil
}

// approveDocker runs no remap and has no images, so the update job stops at
// its pull with the old version still running.
type approveDocker struct{ lifecycle.DockerDriver }

func (approveDocker) UsernsRemap(context.Context) (bool, error) { return false, nil }
func (approveDocker) ImageInspect(context.Context, string) (lifecycle.RepoDigests, error) {
	return lifecycle.RepoDigests{}, errors.New("no such image")
}
func (approveDocker) Pull(context.Context, string) error        { return errors.New("no registry here") }
func (approveDocker) RemoveImage(context.Context, string) error { return nil }

// approveServer is configServer with a lifecycle that can approve: the
// catalog offers 2.0, which asks for the internet, and the row says so.
func approveServer(t *testing.T, ownerID, scope string) (*Server, string) {
	t.Helper()
	s, id, instDir := configServer(t, ownerID, scope, "running", nil)
	catDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(catDir, "cfgapp"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"manifest.yml": approveV2Manifest, "compose.yml": approveV2Compose} {
		if err := os.WriteFile(filepath.Join(catDir, "cfgapp", name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(instDir, "compose.yml"), []byte("services:\n  app:\n    image: example/cfgapp:1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s.bus = events.NewBus()
	s.catalog = catalog.New(catDir)
	s.life = lifecycle.NewManager(s.store, s.catalog, approveHost{}, nil, approveDocker{}, s.bus, filepath.Dir(filepath.Dir(instDir)))
	s.life.SetAdmitter(admission.CheckStructure)
	s.life.CheckUpdates(context.Background())
	row, err := s.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Update.Status != store.UpdateNeedsApproval || len(row.Update.Wants) != 1 {
		t.Fatalf("seeded check = %+v, want 2.0 waiting for an approval", row.Update)
	}
	return s, id
}

func postApprove(s *Server, ctx context.Context, id, version string) (*struct{ Body Job }, error) {
	return s.approveAppUpdate(ctx, &struct {
		ID   string `path:"id"`
		Body UpdateVersionBody
	}{ID: id, Body: UpdateVersionBody{Version: version}})
}

func postDecline(s *Server, ctx context.Context, id, version string) error {
	_, err := s.declineAppUpdate(ctx, &struct {
		ID   string `path:"id"`
		Body UpdateVersionBody
	}{ID: id, Body: UpdateVersionBody{Version: version}})
	return err
}

func auditsOf(t *testing.T, s *Server, action string) []store.AuditEvent {
	t.Helper()
	rows, err := s.store.ListAuditEvents(store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var out []store.AuditEvent
	for _, r := range rows {
		if r.Action == action {
			out = append(out, r)
		}
	}
	return out
}

// waitJob waits for a job to end and returns its status and error.
func waitJob(t *testing.T, s *Server, id string) (string, *JobError) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		j, ok := s.jobs.get(id)
		if !ok {
			t.Fatal("job vanished")
		}
		snap := j.snapshot()
		if snap.Status != "running" {
			return snap.Status, snap.Error
		}
		if time.Now().After(deadline) {
			t.Fatal("the job did not end")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestApproveAppUpdate_OwnerRunsTheUpdateAndAudits(t *testing.T) {
	s, id := approveServer(t, "u_owner", store.ScopePersonal)
	out, err := postApprove(s, memberCtx("u_owner"), id, "2.0")
	if err != nil {
		t.Fatalf("approve: %v", err)
	}
	if out.Body.Kind != "app-update" {
		t.Fatalf("job kind %q", out.Body.Kind)
	}
	if status, jobErr := waitJob(t, s, out.Body.ID); status != "failed" || jobErr == nil || !strings.Contains(jobErr.Message, "could not download") {
		t.Fatalf("job = %s %+v, want the pull to fail", status, jobErr)
	}
	rows := auditsOf(t, s, audit.ActionAppUpdateApprove)
	if len(rows) != 1 || !rows[0].Success || rows[0].ActorUserID != "u_owner" || rows[0].TargetID != id {
		t.Fatalf("approve audit = %+v, want one success by the owner", rows)
	}
	var meta struct {
		Version    string          `json:"version"`
		NewVersion string          `json:"new_version"`
		Wants      []AppAccessWant `json:"wants"`
	}
	if err := json.Unmarshal([]byte(rows[0].Metadata), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.Version != "1.0" || meta.NewVersion != "2.0" || len(meta.Wants) != 1 || meta.Wants[0].Kind != "internet" {
		t.Fatalf("approve audit metadata = %s", rows[0].Metadata)
	}
	if upd := auditsOf(t, s, audit.ActionAppUpdate); len(upd) != 1 || upd[0].Success {
		t.Fatalf("update audit = %+v, want the failed job", upd)
	}
	row, _ := s.store.Get(id)
	if row.UpdateApproval.Version != "2.0" || row.UpdateApproval.By != "u_owner" || row.Update.Status != store.UpdateAvailable {
		t.Fatalf("after the yes: approval %+v update %+v", row.UpdateApproval, row.Update)
	}
}

func TestApproveAppUpdate_Gates(t *testing.T) {
	// An admin cannot allow it for the owner of a personal app.
	s, id := approveServer(t, "u_owner", store.ScopePersonal)
	_, err := postApprove(s, adminCtx("u_admin"), id, "2.0")
	assertStatus(t, err, http.StatusForbidden)
	if rows := auditsOf(t, s, audit.ActionAppUpdateApprove); len(rows) != 1 || rows[0].Success || rows[0].ActorUserID != "u_admin" {
		t.Fatalf("the admin's 403 must audit a failure: %+v", rows)
	}
	// A member asking about someone else's app learns nothing.
	_, err = postApprove(s, memberCtx("u_other"), id, "2.0")
	assertStatus(t, err, http.StatusNotFound)
	_, err = postApprove(s, context.Background(), id, "2.0")
	assertStatus(t, err, http.StatusUnauthorized)
	if row, _ := s.store.Get(id); row.UpdateApproval != (store.UpdateApproval{}) {
		t.Fatalf("a refused yes stored an approval: %+v", row.UpdateApproval)
	}

	// A household app is an admin's to allow, not a member's.
	h, hid := approveServer(t, "u_owner", store.ScopeHousehold)
	_, err = postApprove(h, memberCtx("u_owner"), hid, "2.0")
	assertStatus(t, err, http.StatusForbidden)
	out, err := postApprove(h, adminCtx("u_admin"), hid, "2.0")
	if err != nil {
		t.Fatalf("admin on a household app: %v", err)
	}
	waitJob(t, h, out.Body.ID)
	rows := auditsOf(t, h, audit.ActionAppUpdateApprove)
	if len(rows) != 2 || rows[0].Success == rows[1].Success {
		t.Fatalf("household audit = %+v, want one failure and one success", rows)
	}
}

func TestApproveAppUpdate_Refusals409AreAudited(t *testing.T) {
	s, id := approveServer(t, "u_owner", store.ScopePersonal)
	_, err := postApprove(s, memberCtx("u_owner"), id, "1.5")
	assertStatus(t, err, http.StatusConflict)
	if !strings.Contains(err.Error(), "Look again") {
		t.Errorf("409 message %q", err.Error())
	}
	if err := s.store.SetState(id, "stopped"); err != nil {
		t.Fatal(err)
	}
	_, err = postApprove(s, memberCtx("u_owner"), id, "2.0")
	assertStatus(t, err, http.StatusConflict)
	if !strings.Contains(err.Error(), "Start the app") {
		t.Errorf("409 message %q", err.Error())
	}
	rows := auditsOf(t, s, audit.ActionAppUpdateApprove)
	if len(rows) != 2 || rows[0].Success || rows[1].Success {
		t.Fatalf("audit = %+v, want two failures", rows)
	}
}

func TestDeclineAppUpdate(t *testing.T) {
	s, id := approveServer(t, "u_owner", store.ScopePersonal)
	assertStatus(t, postDecline(s, adminCtx("u_admin"), id, "2.0"), http.StatusForbidden)
	assertStatus(t, postDecline(s, memberCtx("u_owner"), id, "1.5"), http.StatusConflict)
	if err := postDecline(s, memberCtx("u_owner"), id, "2.0"); err != nil {
		t.Fatalf("decline: %v", err)
	}
	row, _ := s.store.Get(id)
	if row.UpdateDeclined != "2.0" || row.Version != "1.0" {
		t.Fatalf("after the no: declined %q version %q", row.UpdateDeclined, row.Version)
	}
	ctx := memberCtx("u_owner")
	dto := s.toDTO(ctx, row, "u_owner", nil)
	if !dto.Update.Declined || !dto.Update.CanApprove {
		t.Fatalf("DTO after the no: %+v; want declined and still allowed from the settings page", dto.Update)
	}
	if rows := auditsOf(t, s, audit.ActionAppUpdateApprove); len(rows) != 0 {
		t.Fatalf("a no is not audited: %+v", rows)
	}
	// A yes after the no clears it.
	out, err := postApprove(s, ctx, id, "2.0")
	if err != nil {
		t.Fatalf("approve after a decline: %v", err)
	}
	waitJob(t, s, out.Body.ID)
	if row, _ := s.store.Get(id); row.UpdateDeclined != "" {
		t.Fatalf("the yes left the decline %q", row.UpdateDeclined)
	}
}

func TestAppUpdateDTO_CanApprove(t *testing.T) {
	owner := auth.Identity{User: store.User{ID: "u_owner", Role: store.RoleMember}}
	admin := auth.Identity{User: store.User{ID: "u_admin", Role: store.RoleAdmin}}
	waiting := store.UpdateCheck{Version: "2.0", Status: store.UpdateNeedsApproval, Wants: []store.AccessWant{{Kind: store.WantLAN}}}
	personal := store.Instance{State: "running", Scope: store.ScopePersonal, OwnerUserID: "u_owner", Version: "1.0", Update: waiting}
	household := personal
	household.Scope = store.ScopeHousehold
	stopped := personal
	stopped.State = "stopped"
	available := personal
	available.Update.Status = store.UpdateAvailable
	for name, tc := range map[string]struct {
		actor auth.Identity
		inst  store.Instance
		want  bool
	}{
		"owner, personal":         {owner, personal, true},
		"admin, not the owner":    {admin, personal, false},
		"admin, household":        {admin, household, true},
		"member, household":       {owner, household, false},
		"owner, stopped":          {owner, stopped, false},
		"owner, nothing to allow": {owner, available, false},
	} {
		t.Run(name, func(t *testing.T) {
			d := appUpdateDTO(tc.actor, tc.inst)
			if d.CanApprove != tc.want {
				t.Fatalf("can_approve = %v, want %v", d.CanApprove, tc.want)
			}
			if tc.inst.Update.Status == store.UpdateNeedsApproval && (len(d.Wants) != 1 || d.Wants[0].Kind != "lan") {
				t.Fatalf("wants = %+v", d.Wants)
			}
		})
	}
	if d := appUpdateDTO(owner, store.Instance{}); d.Wants == nil {
		t.Fatal("wants must be an empty list, not null")
	}
}

func TestDismissUpdatePrompt_IsPerSession(t *testing.T) {
	s, _, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
	now := time.Now()
	for _, tok := range []string{"first", "second"} {
		sess := store.Session{Token: tok, UserID: "u_owner", CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}
		if err := s.store.CreateSession(sess); err != nil {
			t.Fatal(err)
		}
	}
	user, err := s.store.GetUser("u_owner")
	if err != nil {
		t.Fatal(err)
	}
	ctxOf := func(tok string) context.Context {
		sess, err := s.store.GetSession(tok)
		if err != nil {
			t.Fatal(err)
		}
		return auth.WithIdentity(context.Background(), auth.Identity{User: user, Session: sess})
	}
	if _, err := s.dismissUpdatePrompt(ctxOf("first"), &struct{}{}); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	me, err := s.me(ctxOf("first"), &struct{}{})
	if err != nil || !me.Body.UpdatePromptDismissed {
		t.Fatalf("/me of the session that closed it: %+v, %v", me, err)
	}
	me, err = s.me(ctxOf("second"), &struct{}{})
	if err != nil || me.Body.UpdatePromptDismissed {
		t.Fatalf("/me of another session (a new sign-in): %+v, %v", me, err)
	}
	_, err = s.dismissUpdatePrompt(context.Background(), &struct{}{})
	assertStatus(t, err, http.StatusUnauthorized)
}
