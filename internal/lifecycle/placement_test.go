package lifecycle

// The three flags of a terminal app (#660, TERMINAL_APPS.md # Security and
// access): personal_only, hosted_only and access.owner_only, enforced in the
// transaction owner whatever the API or the dashboard did.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/store"
)

const terminalFlags = "personal_only: true\nhosted_only: true\naccess:\n  owner_only: true\n"

func installFlagged(t *testing.T, e *testEnv, extra, scope string) (store.Instance, error) {
	t.Helper()
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest)+extra)
	e.docker.digests[testImage] = testDigest
	return e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, scope, nil, "", nil, nil, nil)
}

func TestInstall_HostedOnlyRefusedOnAppliance(t *testing.T) {
	e := newTestEnv(t)
	if _, err := installFlagged(t, e, "hosted_only: true\n", store.ScopePersonal); !errors.Is(err, ErrHostedOnly) {
		t.Fatalf("install on the appliance: %v, want ErrHostedOnly", err)
	}
	if rows, _ := e.store.List(); len(rows) != 0 {
		t.Fatalf("a refused install left %d rows", len(rows))
	}
}

// make dev-app's switch lets the appliance install a hosted_only app, and
// changes nothing else: personal_only still refuses, the route stays plain
// (no forward-auth off hosted), and an update is not blocked as hosted-only.
func TestInstall_HostedOnlyDevSwitch(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetAllowHostedOnly(true)
	if _, err := installFlagged(t, e, terminalFlags, store.ScopeHousehold); !errors.Is(err, ErrPersonalOnly) {
		t.Fatalf("household install with the dev switch: %v, want ErrPersonalOnly", err)
	}
	inst, err := installFlagged(t, e, terminalFlags, store.ScopePersonal)
	if err != nil {
		t.Fatalf("personal install with the dev switch: %v", err)
	}
	if cfg := e.caddy.config(inst.ID); cfg.ForwardAuth != nil {
		t.Fatalf("the dev switch must not turn on the hosted gate, got %+v", cfg)
	}
}

// With the dev switch, an update that adds hosted_only on the appliance is
// found as available and is applied, through the real check and update paths.
func TestUpdate_HostedOnlyDevSwitch(t *testing.T) {
	e := newTestEnv(t)
	e.m.diskSpace = func(string) (uint64, uint64, error) { return 1 << 40, 2 << 40, nil }
	e.m.SetAllowHostedOnly(true)
	inst := installUp(t, e, "")
	e.publish(t, "2.0", upImage2, upDigest2, "hosted_only: true\n")
	e.m.CheckUpdates(context.Background())
	if r := e.row(t, inst.ID); r.Update.Status == store.UpdateBlocked {
		t.Fatalf("check with the dev switch: %+v, want not blocked", r.Update)
	}
	if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("update with the dev switch: %v", err)
	}
	if r := e.row(t, inst.ID); r.Version != "2.0" {
		t.Fatalf("version %q after the update, want 2.0", r.Version)
	}
}

func TestInstall_PersonalOnlyRefusesHousehold(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	if _, err := installFlagged(t, e, "personal_only: true\n", store.ScopeHousehold); !errors.Is(err, ErrPersonalOnly) {
		t.Fatalf("household install: %v, want ErrPersonalOnly", err)
	}
	if rows, _ := e.store.List(); len(rows) != 0 {
		t.Fatalf("a refused install left %d rows", len(rows))
	}
}

// All three flags on hosted: a personal install goes through, starts Only me
// and stays Only me.
func TestInstall_TerminalFlagsHostedPersonal(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst, err := installFlagged(t, e, terminalFlags, store.ScopePersonal)
	if err != nil {
		t.Fatalf("personal install on hosted: %v", err)
	}
	if row, _ := e.store.Get(inst.ID); row.Exposure != store.ExposureRestricted || row.Scope != store.ScopePersonal {
		t.Fatalf("row exposure %q scope %q, want restricted personal", row.Exposure, row.Scope)
	}
	if err := e.m.SetExposure(context.Background(), inst.ID, store.ExposurePublic); !errors.Is(err, ErrExposureLocked) {
		t.Fatalf("set public: %v, want ErrExposureLocked", err)
	}
	if row, _ := e.store.Get(inst.ID); row.Exposure != store.ExposureRestricted {
		t.Fatalf("a refused toggle changed the row to %q", row.Exposure)
	}
	if e.caddy.config(inst.ID).ForwardAuth == nil {
		t.Fatal("an owner_only app must be gated")
	}
	// Setting it to what it already is stays allowed.
	if err := e.m.SetExposure(context.Background(), inst.ID, store.ExposureRestricted); err != nil {
		t.Fatalf("set restricted: %v", err)
	}
}

// The route builder is the safety boundary: an owner_only app is gated even
// when its row says public, however the row got there.
func TestRoute_OwnerOnlyGatedWhateverTheRow(t *testing.T) {
	e := newTestEnv(t)
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst, err := installFlagged(t, e, "access:\n  owner_only: true\n", store.ScopeHousehold)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if err := e.store.SetInstanceExposure(inst.ID, store.ExposurePublic); err != nil {
		t.Fatal(err)
	}
	e.docker.psManaged = map[string]bool{}
	e.caddy.calls = nil
	if err := e.m.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if e.caddy.config(inst.ID).ForwardAuth == nil {
		t.Fatal("an owner_only app with a public row was served without the gate")
	}
}

// The appliance has no gate, so owner_only changes nothing there: the route
// stays the plain reverse_proxy and the toggle is not the brain's to refuse.
func TestOwnerOnly_ApplianceUnchanged(t *testing.T) {
	e := newTestEnv(t)
	inst, err := installFlagged(t, e, "access:\n  owner_only: true\n", store.ScopeHousehold)
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	if cfg := e.caddy.config(inst.ID); cfg.ForwardAuth != nil || cfg.StripCookieName != "" {
		t.Fatalf("appliance route must stay plain, got %+v", cfg)
	}
}

func TestClassifyUpdate_Placement(t *testing.T) {
	old := &manifest.Manifest{Version: "1.0"}
	household := store.Instance{Scope: store.ScopeHousehold, UsernsTier: store.UsernsTierHost}
	personal := store.Instance{Scope: store.ScopePersonal, UsernsTier: store.UsernsTierHost}
	cases := []struct {
		name string
		inst store.Instance
		new  *manifest.Manifest
		prof profile.Profile
		want string
	}{
		{"personal_only on household", household, &manifest.Manifest{Version: "2.0", PersonalOnly: true}, profile.Hosted, store.UpdateBlocked},
		{"personal_only on personal", personal, &manifest.Manifest{Version: "2.0", PersonalOnly: true}, profile.Hosted, store.UpdateAvailable},
		{"hosted_only on appliance", personal, &manifest.Manifest{Version: "2.0", HostedOnly: true}, profile.Appliance, store.UpdateBlocked},
		{"hosted_only on hosted", personal, &manifest.Manifest{Version: "2.0", HostedOnly: true}, profile.Hosted, store.UpdateAvailable},
	}
	for _, c := range cases {
		got := classifyUpdate(c.inst, old, c.new, nil, nil, 0, nil, c.prof)
		if got.Status != c.want {
			t.Errorf("%s: %s (%q), want %s", c.name, got.Status, got.Reason, c.want)
		}
		if got.Status == store.UpdateBlocked && (got.Reason == "" || strings.HasSuffix(got.Reason, ".")) {
			t.Errorf("%s: reason %q, want one sentence with no full stop (ApproveUpdate adds it)", c.name, got.Reason)
		}
	}
}

// An update that adds owner_only puts a public app back to Only me.
func TestUpdate_AddsOwnerOnlyRestricts(t *testing.T) {
	e := newTestEnv(t)
	e.m.diskSpace = func(string) (uint64, uint64, error) { return 1 << 40, 2 << 40, nil }
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installUp(t, e, "")
	if err := e.m.SetExposure(context.Background(), inst.ID, store.ExposurePublic); err != nil {
		t.Fatalf("set public: %v", err)
	}
	e.newVersionMigrates(t, true)
	e.publish(t, "2.0", upImage2, upDigest2, "access:\n  owner_only: true\n")
	if err := e.m.Update(context.Background(), inst.ID, nil); err != nil {
		t.Fatalf("update: %v", err)
	}
	if r := e.row(t, inst.ID); r.Version != "2.0" || r.Exposure != store.ExposureRestricted {
		t.Fatalf("after the update: version %q exposure %q, want 2.0 restricted", r.Version, r.Exposure)
	}
	if e.caddy.config(inst.ID).ForwardAuth == nil {
		t.Fatal("the new version's route is not gated")
	}
}

// An update that makes a household app personal_only waits as blocked.
func TestUpdate_PersonalOnlyOnHouseholdBlocked(t *testing.T) {
	e := newTestEnv(t)
	e.m.diskSpace = func(string) (uint64, uint64, error) { return 1 << 40, 2 << 40, nil }
	inst := installUp(t, e, "")
	e.publish(t, "2.0", upImage2, upDigest2, "personal_only: true\n")
	e.m.CheckUpdates(context.Background())
	if r := e.row(t, inst.ID); r.Update.Status != store.UpdateBlocked {
		t.Fatalf("check: %+v, want blocked", r.Update)
	}
	if err := e.m.Update(context.Background(), inst.ID, nil); err == nil {
		t.Fatal("a blocked update was applied")
	}
	if r := e.row(t, inst.ID); r.Version != "1.0" {
		t.Fatalf("version %q after a refused update", r.Version)
	}
}

// Greptile on #668: with no "updating" splash, the old public route stays,
// and a version that adds owner_only would answer anyone while it comes up.
// Such an update stops before the old version stops.
func TestUpdate_AddsOwnerOnlyStopsWithoutSplash(t *testing.T) {
	e := newTestEnv(t)
	e.m.diskSpace = func(string) (uint64, uint64, error) { return 1 << 40, 2 << 40, nil }
	e.m.SetEnvironment(profile.Hosted, "cindy-fox")
	inst := installUp(t, e, "")
	if err := e.m.SetExposure(context.Background(), inst.ID, store.ExposurePublic); err != nil {
		t.Fatalf("set public: %v", err)
	}
	e.publish(t, "2.0", upImage2, upDigest2, "access:\n  owner_only: true\n")
	e.caddy.writeErr = errors.New("caddy is down")
	e.docker.calls = nil
	if err := e.m.Update(context.Background(), inst.ID, nil); err == nil {
		t.Fatal("the update went on with no splash")
	}
	if e.docker.called("ComposeStop") || e.docker.called("ComposeUp") {
		t.Fatal("the old version was stopped or the new one started")
	}
	if r := e.row(t, inst.ID); r.Version != "1.0" || r.State != "running" {
		t.Fatalf("after the refused update: version %q state %q, want 1.0 running", r.Version, r.State)
	}
}
