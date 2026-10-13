package api

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/fit"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

// fitHost answers SystemResources with a fixed box, or an error. Every other
// host call goes to the harness's host-agent mock.
type fitHost struct {
	lifecycle.HostDriver
	res protocol.SystemResources
	err error
}

func (h fitHost) SystemResources(context.Context) (protocol.SystemResources, error) {
	return h.res, h.err
}

// withFitHost swaps in a lifecycle.Manager that reads the given box. It uses
// the harness's state dir, so seedInstanceManifest reaches it. It runs before
// the first request.
func (h *harness) withFitHost(host fitHost) {
	s := h.apiSrv
	host.HostDriver = s.host
	s.life = lifecycle.NewManager(s.store, s.catalog, host, nil, remapDocker{on: true}, s.bus, h.stateDir)
}

func fitManifest(id, resources string) string {
	return `id: ` + id + `
manifest_version: 1
name: ` + id + `
version: "1.0"
description:
  short: an app
categories: [notes]
compose_file: compose.yml
main_service: app
main_port: 80
` + resources
}

func fitHarness(t *testing.T, host fitHost) *harness {
	h := newHarness(t)
	h.withFitHost(host)
	writeManifestFixture(t, h.catalogDir, "small", fitManifest("small", "resources:\n  recommended: { memory: 512M, cpu: 1 }\n"))
	writeManifestFixture(t, h.catalogDir, "big", fitManifest("big", "resources:\n  recommended: { memory: 8G }\n"))
	writeManifestFixture(t, h.catalogDir, "cores", fitManifest("cores", "resources:\n  recommended: { cpu: 4 }\n"))
	writeManifestFixture(t, h.catalogDir, "none", fitManifest("none", ""))
	h.setupAdmin("alice", "pass1")
	return h
}

// The store lists, the detail page and the install plan all carry the same
// fit, and a too-big app is still listed but cannot be installed (#599).
func TestCatalogCarriesFit(t *testing.T) {
	h := fitHarness(t, fitHost{res: protocol.SystemResources{
		Mem: protocol.MemCounters{TotalBytes: 4 << 30, UsedBytes: 1 << 30},
		CPU: protocol.CPUCounters{Cores: 2},
	}})
	want := map[string]string{"small": fit.Fits, "big": fit.TooBig, "cores": fit.TooBig, "none": fit.Fits}

	got := map[string]string{}
	for _, e := range decodeJSON[appsBody](t, h.getOK("/api/v1/catalog")).Apps {
		if e.Fit == nil {
			t.Fatalf("%s carries no fit", e.ID)
		}
		got[e.ID] = e.Fit.State
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("list: %s = %q, want %q", id, got[id], w)
		}
		d := decodeJSON[catalog.Detail](t, h.getOK("/api/v1/catalog/"+id))
		if d.Fit == nil || d.Fit.State != w {
			t.Errorf("detail: %s fit = %+v, want %q", id, d.Fit, w)
		}
		plan := decodeJSON[InstallPlanDTO](t, h.getOK("/api/v1/catalog/"+id+"/install-plan"))
		if plan.Fit == nil || plan.Fit.State != w {
			t.Errorf("plan: %s fit = %+v, want %q", id, plan.Fit, w)
		}
		wantUnavailable := ""
		if w == fit.TooBig {
			wantUnavailable = unavailableTooBig
		}
		if plan.Unavailable != wantUnavailable {
			t.Errorf("plan: %s unavailable = %q, want %q", id, plan.Unavailable, wantUnavailable)
		}
	}
	d := decodeJSON[catalog.Detail](t, h.getOK("/api/v1/catalog/small"))
	if d.Fit.RecommendedMemoryBytes != 512<<20 || d.Fit.RecommendedCPU != 1 || d.Fit.BoxCores != 2 {
		t.Errorf("small fit figures = %+v", d.Fit)
	}
}

// A box that cannot read its own memory shows no fit and refuses nothing.
func TestCatalogFitUnknown(t *testing.T) {
	h := fitHarness(t, fitHost{err: errors.New("host-agent unreachable")})
	for _, e := range decodeJSON[appsBody](t, h.getOK("/api/v1/catalog")).Apps {
		if e.Fit != nil {
			t.Errorf("%s carries a fit with no box reading: %+v", e.ID, e.Fit)
		}
	}
	plan := decodeJSON[InstallPlanDTO](t, h.getOK("/api/v1/catalog/big/install-plan"))
	if plan.Fit != nil || plan.Unavailable != "" {
		t.Errorf("plan = fit %+v, unavailable %q; want neither", plan.Fit, plan.Unavailable)
	}
}

// A member sees a tight app's running apps only where canSee lets them: the
// household app is named, another user's personal app is only counted. Both
// still take memory, so the app is tight either way (Greptile on #625).
func TestCatalogFitHidesOthersPersonalApps(t *testing.T) {
	h := fitHarness(t, fitHost{res: protocol.SystemResources{
		Mem: protocol.MemCounters{TotalBytes: 4 << 30, UsedBytes: 1 << 30},
		CPU: protocol.CPUCounters{Cores: 4},
	}})
	h.addMember("u_bob", "bob", "pw-bob")
	for _, i := range []store.Instance{
		{ID: "i_house", ManifestID: "big", Name: "House App", Slug: "house", Version: "1.0", State: "running", OwnerUserID: "u_alice", Scope: store.ScopeHousehold, CreatedAt: time.Now()},
		{ID: "i_secret", ManifestID: "big", Name: "Alice Secret", Slug: "secret--alice", Version: "1.0", State: "running", OwnerUserID: "u_alice", Scope: store.ScopePersonal, CreatedAt: time.Now()},
	} {
		if err := h.st.Create(i); err != nil {
			t.Fatal(err)
		}
		h.seedInstanceManifest(i.ID, fitManifest("big", "resources:\n  recommended: { memory: 1G }\n"))
	}

	check := func(who string, fitOf func() *fit.AppFit, wantNamed []string, wantOthers int) {
		t.Helper()
		f := fitOf()
		if f == nil || f.State != fit.Tight {
			t.Fatalf("%s: fit = %+v, want tight", who, f)
		}
		var named []string
		for _, r := range f.Running {
			named = append(named, r.Name)
		}
		if !slices.Equal(named, wantNamed) || f.OtherRunning != wantOthers {
			t.Errorf("%s: named %v, others %d; want %v, %d", who, named, f.OtherRunning, wantNamed, wantOthers)
		}
	}
	detail := func() *fit.AppFit { return decodeJSON[catalog.Detail](t, h.getOK("/api/v1/catalog/small")).Fit }
	plan := func() *fit.AppFit {
		return decodeJSON[InstallPlanDTO](t, h.getOK("/api/v1/catalog/small/install-plan")).Fit
	}
	// small recommends 512M; 4 GiB minus the 2 GiB the two apps recommend
	// leaves room, so make it tight with a 3G app instead.
	writeManifestFixture(t, h.catalogDir, "small", fitManifest("small", "resources:\n  recommended: { memory: 3G }\n"))

	h.loginAs("bob", "pw-bob")
	check("member detail", detail, []string{"House App"}, 1)
	check("member plan", plan, []string{"House App"}, 1)
	h.loginAs("alice", "pass1")
	check("admin detail", detail, []string{"House App", "Alice Secret"}, 0)
}
