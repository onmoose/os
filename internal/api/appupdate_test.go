package api

// The app update endpoint and the auto-update setting (#631). Driven over a
// real store with configServer's seeded instance. The update transaction
// itself is tested at the lifecycle layer (update_test.go); here the job runs
// against an empty catalog, so it ends with "no update", which is enough to
// see the job and its audit record.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/store"
)

func postUpdate(s *Server, ctx context.Context, id string) (*struct{ Body Job }, error) {
	return s.updateApp(ctx, &struct {
		ID string `path:"id"`
	}{ID: id})
}

func putAutoUpdate(s *Server, ctx context.Context, id string, enabled bool) (*struct{ Body InstanceDTO }, error) {
	return s.setAppAutoUpdate(ctx, &struct {
		ID   string `path:"id"`
		Body struct {
			Enabled bool `json:"enabled"`
		}
	}{ID: id, Body: struct {
		Enabled bool `json:"enabled"`
	}{Enabled: enabled}})
}

func updateAudits(t *testing.T, s *Server) []store.AuditEvent {
	t.Helper()
	rows, err := s.store.ListAuditEvents(store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatalf("list audit: %v", err)
	}
	var out []store.AuditEvent
	for _, r := range rows {
		if r.Action == audit.ActionAppUpdate {
			out = append(out, r)
		}
	}
	return out
}

func TestUpdateApp_Refusals409AreAudited(t *testing.T) {
	for name, tc := range map[string]struct {
		state   string
		version string
		check   store.UpdateCheck
		want    string
	}{
		"no update":      {state: "running", want: "no update"},
		"needs approval": {state: "running", check: store.UpdateCheck{Version: "2.0", Status: store.UpdateNeedsApproval, Reason: "This version asks for more access: the internet"}, want: "more access"},
		"service major":  {state: "running", check: store.UpdateCheck{Version: "2.0", Status: store.UpdateServiceMajor, Reason: "This version needs postgres 17"}, want: "postgres 17"},
		"newer moose":    {state: "running", check: store.UpdateCheck{Version: "2.0", Status: store.UpdateNeedsNewerMoose, Reason: "This version needs a newer moose"}, want: "newer moose"},
		"not running":    {state: "stopped", check: store.UpdateCheck{Version: "2.0", Status: store.UpdateAvailable}, want: "Start the app"},
		"door-2 app":     {state: "running", version: manifest.CustomVersion, check: store.UpdateCheck{Version: "2.0", Status: store.UpdateAvailable}, want: "added by hand"},
	} {
		t.Run(name, func(t *testing.T) {
			s, id, _ := configServer(t, "u_owner", store.ScopePersonal, tc.state, nil)
			if tc.version != "" {
				id = "inst_custom"
				if err := s.store.Create(store.Instance{
					ID: id, ManifestID: "custom-x", Name: "Pasted", Slug: "pasted", Version: tc.version,
					State: tc.state, OwnerUserID: "u_owner", Scope: store.ScopePersonal, CreatedAt: time.Now(),
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.store.SetInstanceUpdateCheck(id, tc.check); err != nil {
				t.Fatal(err)
			}
			_, err := postUpdate(s, memberCtx("u_owner"), id)
			assertStatus(t, err, http.StatusConflict)
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("409 message %q, want %q", err.Error(), tc.want)
			}
			rows := updateAudits(t, s)
			if len(rows) != 1 || rows[0].Success || rows[0].TargetID != id {
				t.Fatalf("audit = %+v, want one failure for %s", rows, id)
			}
		})
	}
}

func TestUpdateApp_Gates(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopeHousehold, "running", nil)
	if err := s.store.SetInstanceUpdateCheck(id, store.UpdateCheck{Version: "2.0", Status: store.UpdateAvailable}); err != nil {
		t.Fatal(err)
	}
	_, err := postUpdate(s, memberCtx("u_owner"), id)
	assertStatus(t, err, http.StatusForbidden)
	if rows := updateAudits(t, s); len(rows) != 1 || rows[0].Success {
		t.Fatalf("a 403 must audit a failure: %+v", rows)
	}
	_, err = postUpdate(s, context.Background(), id)
	assertStatus(t, err, http.StatusUnauthorized)

	p, pid, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
	_, err = postUpdate(p, memberCtx("u_other"), pid)
	assertStatus(t, err, http.StatusNotFound)
	_, err = postUpdate(p, adminCtx("u_admin"), "ghost")
	assertStatus(t, err, http.StatusNotFound)
}

// TestUpdateApp_RunsAJobAndAudits: an available update starts a job; the job's
// end is audited. The catalog here no longer has the app, so the job fails
// with "no update" and clears the stale check.
func TestUpdateApp_RunsAJobAndAudits(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopeHousehold, "running", nil)
	s.catalog = catalog.New(t.TempDir())
	if err := s.store.SetInstanceUpdateCheck(id, store.UpdateCheck{Version: "2.0", Status: store.UpdateAvailable}); err != nil {
		t.Fatal(err)
	}
	out, err := postUpdate(s, adminCtx("u_admin"), id)
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if out.Body.Kind != "app-update" || out.Body.ID == "" {
		t.Fatalf("job kind %q id %q", out.Body.Kind, out.Body.ID)
	}
	deadline := time.Now().Add(5 * time.Second)
	var status string
	var jobErr *JobError
	for {
		j, ok := s.jobs.get(out.Body.ID)
		if !ok {
			t.Fatal("job vanished")
		}
		snap := j.snapshot()
		status, jobErr = snap.Status, snap.Error
		if status != "running" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if status != "failed" || jobErr == nil || !strings.Contains(jobErr.Message, "no update") {
		t.Fatalf("job status %q error %+v", status, jobErr)
	}
	rows := updateAudits(t, s)
	if len(rows) != 1 || rows[0].Success || rows[0].ActorUserID != "u_admin" {
		t.Fatalf("audit = %+v, want one failure by the admin", rows)
	}
	if row, _ := s.store.Get(id); row.Update != (store.UpdateCheck{}) {
		t.Errorf("stale check left: %+v", row.Update)
	}
}

func TestSetAppAutoUpdate(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
	s.catalog = catalog.New(t.TempDir())
	if row, _ := s.store.Get(id); row.AutoUpdateOff {
		t.Fatal("a new app has automatic update off")
	}
	if dto := s.toDTO(mustGet(t, s, id), "", nil); !dto.AutoUpdate {
		t.Fatal("DTO auto_update = false for a new app")
	}
	out, err := putAutoUpdate(s, memberCtx("u_owner"), id, false)
	if err != nil {
		t.Fatalf("turn off: %v", err)
	}
	if out.Body.AutoUpdate {
		t.Error("DTO auto_update = true after turning it off")
	}
	if row, _ := s.store.Get(id); !row.AutoUpdateOff {
		t.Error("stored setting is still on")
	}
	if out, err = putAutoUpdate(s, adminCtx("u_admin"), id, true); err != nil || !out.Body.AutoUpdate {
		t.Fatalf("turn on: %v, %+v", err, out)
	}
	_, err = putAutoUpdate(s, memberCtx("u_other"), id, false)
	assertStatus(t, err, http.StatusNotFound)

	h, hid, _ := configServer(t, "u_owner", store.ScopeHousehold, "running", nil)
	_, err = putAutoUpdate(h, memberCtx("u_owner"), hid, false)
	assertStatus(t, err, http.StatusForbidden)
}

func TestAppUpdateDTO(t *testing.T) {
	at := time.Unix(1_700_000_000, 0)
	i := store.Instance{
		Version: "1.0", State: "running",
		Update:        store.UpdateCheck{Version: "2.0", Status: store.UpdateAvailable},
		UpdateFailure: store.UpdateFailure{Version: "1.9", Kind: store.UpdateFailedHealth, Message: "did not start", At: at},
	}
	d := appUpdateDTO(i)
	if !d.CanApply || d.AvailableVersion != "2.0" || d.FailedKind != "health" || d.FailedAt == nil || !d.FailedAt.Equal(at) {
		t.Fatalf("dto = %+v", d)
	}
	i.State = "stopped"
	if appUpdateDTO(i).CanApply {
		t.Error("a stopped app can apply an update")
	}
	i.State, i.Update.Status = "running", store.UpdateNeedsApproval
	if appUpdateDTO(i).CanApply {
		t.Error("an update that needs approval can be applied")
	}
}

func mustGet(t *testing.T, s *Server, id string) store.Instance {
	t.Helper()
	r, err := s.store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
