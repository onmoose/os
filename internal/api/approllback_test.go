package api

// The rollback by hand, the banner's acknowledgement and the toast's "seen"
// mark (#634). The rollback transaction itself is tested at the lifecycle
// layer (rollback_test.go); here the instance has no files, so the job ends
// with "no earlier version", which is enough to see the job and its audit.

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/store"
)

func postRollback(s *Server, ctx context.Context, id string) (*struct{ Body Job }, error) {
	return s.rollbackApp(ctx, &struct {
		ID string `path:"id"`
	}{ID: id})
}

func rollbackAudits(t *testing.T, s *Server) []store.AuditEvent {
	t.Helper()
	rows, err := s.store.ListAuditEvents(store.AuditFilter{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var out []store.AuditEvent
	for _, r := range rows {
		if r.Action == audit.ActionAppUpdateRollback {
			out = append(out, r)
		}
	}
	return out
}

// updated gives the instance a good update from 1.0 at the given time.
func updated(t *testing.T, s *Server, id string, at time.Time) {
	t.Helper()
	if err := s.store.FinishInstanceUpdate(id, "2.0", "App", store.LastUpdate{From: "1.0", At: at}); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackApp_RefusalsAreAudited(t *testing.T) {
	t.Run("nothing to go back to", func(t *testing.T) {
		s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
		_, err := postRollback(s, memberCtx("u_owner"), id)
		assertStatus(t, err, http.StatusConflict)
		if !strings.Contains(err.Error(), "no earlier version") {
			t.Errorf("409 message %q", err.Error())
		}
		if rows := rollbackAudits(t, s); len(rows) != 1 || rows[0].Success {
			t.Fatalf("audit %+v", rows)
		}
	})
	t.Run("seven days over", func(t *testing.T) {
		s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
		updated(t, s, id, time.Now().Add(-8*24*time.Hour))
		_, err := postRollback(s, memberCtx("u_owner"), id)
		assertStatus(t, err, http.StatusConflict)
	})
	t.Run("busy", func(t *testing.T) {
		s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
		updated(t, s, id, time.Now())
		if err := s.store.SetState(id, "updating"); err != nil {
			t.Fatal(err)
		}
		_, err := postRollback(s, memberCtx("u_owner"), id)
		assertStatus(t, err, http.StatusConflict)
		if rows := rollbackAudits(t, s); len(rows) != 1 || rows[0].Success {
			t.Fatalf("audit %+v", rows)
		}
	})
	t.Run("member on a household app", func(t *testing.T) {
		s, id, _ := configServer(t, "u_owner", store.ScopeHousehold, "running", nil)
		updated(t, s, id, time.Now())
		_, err := postRollback(s, memberCtx("u_owner"), id)
		assertStatus(t, err, http.StatusForbidden)
		if rows := rollbackAudits(t, s); len(rows) != 1 || rows[0].Success {
			t.Fatalf("a 403 must audit a failure: %+v", rows)
		}
	})
	t.Run("another member's app", func(t *testing.T) {
		s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
		updated(t, s, id, time.Now())
		_, err := postRollback(s, memberCtx("u_other"), id)
		assertStatus(t, err, http.StatusNotFound)
		_, err = postRollback(s, context.Background(), id)
		assertStatus(t, err, http.StatusUnauthorized)
	})
}

func TestRollbackApp_RunsAJobAndAudits(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopeHousehold, "running", nil)
	updated(t, s, id, time.Now())
	out, err := postRollback(s, adminCtx("u_admin"), id)
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if out.Body.Kind != "app-rollback" || out.Body.ID == "" {
		t.Fatalf("job %q %q", out.Body.Kind, out.Body.ID)
	}
	deadline := time.Now().Add(5 * time.Second)
	var snap Job
	for {
		j, ok := s.jobs.get(out.Body.ID)
		if !ok {
			t.Fatal("job vanished")
		}
		snap = j.snapshot()
		if snap.Status != "running" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	// The instance has no kept files, so the rollback finds nothing.
	if snap.Status != "failed" || snap.Error == nil || !strings.Contains(snap.Error.Message, "no earlier version") {
		t.Fatalf("job %q error %+v", snap.Status, snap.Error)
	}
	rows := rollbackAudits(t, s)
	if len(rows) != 1 || rows[0].Success || rows[0].ActorUserID != "u_admin" {
		t.Fatalf("audit %+v, want one failure by the admin", rows)
	}
}

func TestAppUpdateDTO_Rollback(t *testing.T) {
	at := time.Now().Add(-time.Hour)
	i := store.Instance{
		Version: "2.0", State: "running", Scope: store.ScopePersonal, OwnerUserID: "u_owner",
		LastUpdate:    store.LastUpdate{From: "1.0", At: at, ByWindow: true},
		UpdateFailure: store.UpdateFailure{Version: "3.0", Kind: store.UpdateFailedHealth, ByWindow: true, Acked: true},
	}
	owner := auth.Identity{User: store.User{ID: "u_owner", Role: store.RoleMember}}
	d := appUpdateDTO(owner, i)
	if !d.CanRollback || d.UpdatedFrom != "1.0" || !d.UpdatedOvernight || d.UpdatedAt == nil || d.RollbackUntil == nil ||
		!d.RollbackUntil.Equal(at.Add(7*24*time.Hour)) || !d.FailedOvernight || !d.FailedAcked {
		t.Fatalf("owner's dto %+v", d)
	}
	admin := auth.Identity{User: store.User{ID: "u_admin", Role: store.RoleAdmin}}
	if !appUpdateDTO(admin, i).CanRollback {
		t.Error("an admin cannot roll back a personal app it may control")
	}
	i.Scope = store.ScopeHousehold
	if appUpdateDTO(owner, i).CanRollback {
		t.Error("a member can roll back a household app")
	}
	i.Scope, i.State = store.ScopePersonal, "updating"
	if d := appUpdateDTO(owner, i); d.CanRollback || d.UpdatedFrom != "1.0" {
		t.Errorf("a busy app: %+v, want the update shown and no rollback", d)
	}
	i.State, i.LastUpdate.At = "running", time.Now().Add(-8*24*time.Hour)
	if d := appUpdateDTO(owner, i); d.CanRollback || d.UpdatedFrom != "" || d.RollbackUntil != nil {
		t.Errorf("after 7 days: %+v, want nothing to roll back", d)
	}
}

func TestAcknowledgeUpdateFailure(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
	if err := s.store.RecordInstanceUpdateFailure(id, store.UpdateFailure{Version: "2.0", Kind: store.UpdateFailedHealth}); err != nil {
		t.Fatal(err)
	}
	ack := func(ctx context.Context) error {
		_, err := s.acknowledgeAppUpdateFailure(ctx, &struct {
			ID string `path:"id"`
		}{ID: id})
		return err
	}
	assertStatus(t, ack(memberCtx("u_other")), http.StatusNotFound)
	if err := ack(memberCtx("u_owner")); err != nil {
		t.Fatal(err)
	}
	if !mustGet(t, s, id).UpdateFailure.Acked {
		t.Fatal("not acknowledged")
	}
}

func TestMarkUpdatesSeen(t *testing.T) {
	s, _, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
	before := time.Now().Add(-time.Second)
	if _, err := s.markUpdatesSeen(memberCtx("u_owner"), &struct{}{}); err != nil {
		t.Fatal(err)
	}
	seen, err := s.store.UpdatesSeen("u_owner")
	if err != nil || seen.Before(before) {
		t.Fatalf("seen %v %v", seen, err)
	}
	_, err = s.markUpdatesSeen(context.Background(), &struct{}{})
	assertStatus(t, err, http.StatusUnauthorized)
}
