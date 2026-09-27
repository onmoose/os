package lifecycle

// AI slot bindings at install (INSTALL_SETUP.md # 5): the API resolves each
// binding into config values, and the install stores both. The config values
// reach the override like typed ones, the binding rows are kept, and both go
// with the instance.

import (
	"context"
	"testing"
	"time"

	"github.com/onmoose/os/internal/store"
)

// createAIAccount stores an account owned by u_admin, creating that user first.
func (e *testEnv) createAIAccount(t *testing.T, id string) {
	t.Helper()
	if err := e.store.CreateUser(store.User{
		ID: "u_admin", Username: "admin", DisplayName: "admin",
		Role: store.RoleAdmin, CreatedAt: time.Unix(1_600_000_000, 0),
	}); err != nil {
		t.Fatalf("create user: %v", err)
	}
	now := time.Unix(1_700_000_000, 0)
	if err := e.store.CreateAIAccount(store.AIAccount{
		ID: id, OwnerUserID: "u_admin", ProviderID: "openai", Label: "Work",
		APIKey: "sk-from-account", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("create ai account: %v", err)
	}
}

func TestInstallStoresAIBindings(t *testing.T) {
	e := newTestEnv(t)
	e.createAIAccount(t, "ai_1")
	e.writeCatalogApp(t, "configapp", configCompose, configManifest)
	e.docker.digests[testImage] = testDigest

	// What the API resolved the binding to.
	cfg := []store.InstanceConfig{
		{AppEnv: "OPENAI_API_KEY", Value: "sk-from-account", Secret: true},
		{AppEnv: "OPENAI_MODEL", Value: "gpt-4o"},
	}
	bindings := []store.AIBinding{{Slot: "ai.openai", AccountID: "ai_1", Models: map[string][]string{"model.chat": {"gpt-4o"}}}}
	inst, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "configapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", cfg, bindings, nil)
	if err != nil {
		t.Fatalf("install: %v", err)
	}

	app := readOverrideEnv(t, e, inst.ID).Services["app"].Environment
	if app["OPENAI_API_KEY"] != "sk-from-account" || app["OPENAI_MODEL"] != "gpt-4o" {
		t.Fatalf("override env = %v", app)
	}
	got, err := e.store.ListInstanceAIBindings(inst.ID)
	if err != nil || len(got) != 1 || got[0].AccountID != "ai_1" || got[0].Slot != "ai.openai" || got[0].Models["model.chat"][0] != "gpt-4o" {
		t.Fatalf("stored bindings = %+v (%v)", got, err)
	}

	if err := e.m.Uninstall(context.Background(), inst.ID); err != nil {
		t.Fatalf("uninstall: %v", err)
	}
	if got, _ := e.store.ListAIBindingsForAccount("ai_1"); len(got) != 0 {
		t.Fatalf("bindings must go with the instance: %+v", got)
	}
}

// An account deleted between the API's check and the install fails the
// foreign key, and the install rolls back.
func TestInstallMissingAIAccountRollsBack(t *testing.T) {
	e := newTestEnv(t)
	e.writeCatalogApp(t, "configapp", configCompose, configManifest)
	e.docker.digests[testImage] = testDigest
	cfg := []store.InstanceConfig{{AppEnv: "OPENAI_API_KEY", Value: "sk", Secret: true}}
	_, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "configapp"),
		Owner{UserID: "u_admin", Username: "admin"}, store.ScopeHousehold, nil, "", cfg,
		[]store.AIBinding{{Slot: "ai.openai", AccountID: "ai_ghost"}}, nil)
	if err == nil {
		t.Fatal("want error binding a missing account")
	}
	if list, _ := e.store.List(); len(list) != 0 {
		t.Fatalf("failed install must roll back the instance row, got: %v", list)
	}
}
