package api

// Per-app automatic restart setting (#598). Driven over a real store, reusing
// configServer's seeded instance.

import (
	"context"
	"net/http"
	"testing"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/store"
)

func putAutoRestart(s *Server, ctx context.Context, id string, enabled bool) (*struct{ Body InstanceDTO }, error) {
	return s.setAppAutoRestart(ctx, &struct {
		ID   string `path:"id"`
		Body struct {
			Enabled bool `json:"enabled"`
		}
	}{ID: id, Body: struct {
		Enabled bool `json:"enabled"`
	}{Enabled: enabled}})
}

func TestSetAppAutoRestart_TogglesForTheOwner(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
	s.catalog = catalog.New(t.TempDir())

	// On by default.
	if row, _ := s.store.Get(id); row.AutoRestartOff {
		t.Fatal("a new app has the automatic restart off")
	}
	out, err := putAutoRestart(s, memberCtx("u_owner"), id, false)
	if err != nil {
		t.Fatalf("turn off: %v", err)
	}
	if out.Body.AutoRestart {
		t.Error("DTO auto_restart = true after turning it off")
	}
	if row, _ := s.store.Get(id); !row.AutoRestartOff {
		t.Error("stored setting is still on")
	}
	out, err = putAutoRestart(s, adminCtx("u_admin"), id, true)
	if err != nil {
		t.Fatalf("turn on: %v", err)
	}
	if !out.Body.AutoRestart {
		t.Error("DTO auto_restart = false after turning it on")
	}
	if row, _ := s.store.Get(id); row.AutoRestartOff {
		t.Error("stored setting is still off")
	}
}

func TestSetAppAutoRestart_NonAdminHousehold_403(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopeHousehold, "running", nil)
	_, err := putAutoRestart(s, memberCtx("u_owner"), id, false)
	assertStatus(t, err, http.StatusForbidden)
	if row, _ := s.store.Get(id); row.AutoRestartOff {
		t.Error("a refused request changed the setting")
	}
}

func TestSetAppAutoRestart_OtherMembersApp_404(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
	_, err := putAutoRestart(s, memberCtx("u_other"), id, false)
	assertStatus(t, err, http.StatusNotFound)
}

func TestSetAppAutoRestart_NoAuth_401(t *testing.T) {
	s, id, _ := configServer(t, "u_owner", store.ScopePersonal, "running", nil)
	_, err := putAutoRestart(s, context.Background(), id, false)
	assertStatus(t, err, http.StatusUnauthorized)
}
