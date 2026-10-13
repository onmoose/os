package api

import (
	"context"
	"errors"
	"testing"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/fit"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/protocol"
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

func withFitHost(t *testing.T, host fitHost) func(*Server) {
	return func(s *Server) {
		host.HostDriver = s.host
		s.life = lifecycle.NewManager(s.store, s.catalog, host, nil, remapDocker{on: true}, s.bus, t.TempDir())
	}
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
	h := newHarness(t, withFitHost(t, host))
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
