package api

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/onmoose/os/internal/catalog"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/protocol"
)

// remapHost answers the well-known identity with a fixed remap_base (nil: no
// remap, like the fake host-agent). Every other host call goes to the
// harness's host-agent mock.
type remapHost struct {
	lifecycle.HostDriver
	base *int
}

func (h remapHost) WellKnownIdentity(context.Context) (protocol.WellKnownIdentityResponse, error) {
	return protocol.WellKnownIdentityResponse{MooseAppUID: 2000, MooseAppGID: 2000, MooseSharedGID: 2001, RemapBase: h.base}, nil
}

// remapDocker reports whether the daemon is remapped. The store paths these
// tests reach call nothing else on Docker (the fixtures declare no images).
type remapDocker struct {
	lifecycle.DockerDriver
	on  bool
	err error
}

func (d remapDocker) UsernsRemap(context.Context) (bool, error) { return d.on, d.err }

// withRemap swaps in a lifecycle.Manager that reads the given remap state:
// on is a remapped box, off a box with none.
func withRemap(t *testing.T, on bool) func(*Server) {
	return withRemapDocker(t, on, remapDocker{on: on})
}

// withUnreadableRemap is a box whose docker info fails, so the remap state is
// unknown.
func withUnreadableRemap(t *testing.T) func(*Server) {
	return withRemapDocker(t, false, remapDocker{err: errors.New("docker is not answering")})
}

func withRemapDocker(t *testing.T, on bool, d remapDocker) func(*Server) {
	return func(s *Server) {
		var base *int
		if on {
			b := 1000000
			base = &b
		}
		s.life = lifecycle.NewManager(s.store, s.catalog, remapHost{HostDriver: s.host, base: base}, nil, d, s.bus, t.TempDir())
	}
}

const rootSetupManifestYML = `id: poz
manifest_version: 1
name: Poz
version: "1.0"
description:
  short: notes app
categories: [notes]
compose_file: compose.yml
main_service: app
main_port: 80
root_setup: true
`

const imageUserManifestYML = `id: plk
manifest_version: 1
name: Plk
version: "1.0"
description:
  short: mail app
categories: [mail]
compose_file: compose.yml
main_service: app
main_port: 80
image_user: true
`

const plainManifestYML = `id: plain
manifest_version: 1
name: Plain
version: "1.0"
description:
  short: a plain app
categories: [notes]
compose_file: compose.yml
main_service: app
main_port: 80
`

func remapHarness(t *testing.T, opts ...func(*Server)) *harness {
	h := newHarness(t, opts...)
	writeManifestFixture(t, h.catalogDir, "poz", rootSetupManifestYML)
	writeManifestFixture(t, h.catalogDir, "plk", imageUserManifestYML)
	writeManifestFixture(t, h.catalogDir, "plain", plainManifestYML)
	h.setupAdmin("alice", "pass1")
	return h
}

type appsBody struct {
	Apps []catalog.Entry `json:"apps"`
}

func entryIDs(es []catalog.Entry) []string {
	out := make([]string, 0, len(es))
	for _, e := range es {
		out = append(out, e.ID)
	}
	slices.Sort(out)
	return out
}

func (h *harness) getOK(path string) *http.Response {
	h.t.Helper()
	resp := h.do("GET", path, nil)
	if resp.StatusCode != http.StatusOK {
		h.t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
	}
	return resp
}

// On a box with no remap the root_setup and image_user apps are left out of
// every store list, and a direct link to one still loads with Install marked
// not possible.
func TestCatalogHidesRemapAppsWithoutRemap(t *testing.T) {
	h := remapHarness(t, withRemap(t, false))

	if got := entryIDs(decodeJSON[appsBody](t, h.getOK("/api/v1/catalog")).Apps); !slices.Equal(got, []string{"plain"}) {
		t.Errorf("GET /catalog = %v, want [plain]", got)
	}
	if got := entryIDs(decodeJSON[appsBody](t, h.getOK("/api/v1/catalog/search?q=app")).Apps); !slices.Equal(got, []string{"plain"}) {
		t.Errorf("search = %v, want [plain]", got)
	}
	home := decodeJSON[catalog.Home](t, h.getOK("/api/v1/catalog/home"))
	var cats []string
	for _, c := range home.Categories {
		cats = append(cats, c.ID)
	}
	if !slices.Equal(cats, []string{"notes"}) {
		t.Errorf("home categories = %v, want [notes] (mail had only the image_user app)", cats)
	}
	if resp := h.do("GET", "/api/v1/catalog/category?name=mail", nil); resp.StatusCode != http.StatusNotFound {
		t.Errorf("category mail = %d, want 404", resp.StatusCode)
	}
	page := decodeJSON[catalog.CategoryPage](t, h.getOK("/api/v1/catalog/category?name=notes"))
	if got := entryIDs(page.Apps); !slices.Equal(got, []string{"plain"}) {
		t.Errorf("category notes = %v, want [plain]", got)
	}

	for _, id := range []string{"poz", "plk"} {
		h.getOK("/api/v1/catalog/" + id).Body.Close()
		plan := decodeJSON[InstallPlanDTO](t, h.getOK("/api/v1/catalog/"+id+"/install-plan"))
		if plan.Unavailable != unavailableNeedsRemap {
			t.Errorf("%s install plan unavailable = %q, want %q", id, plan.Unavailable, unavailableNeedsRemap)
		}
	}
	plan := decodeJSON[InstallPlanDTO](t, h.getOK("/api/v1/catalog/plain/install-plan"))
	if plan.Unavailable != "" {
		t.Errorf("plain install plan unavailable = %q, want empty", plan.Unavailable)
	}
}

// On a remapped box, and when the remap cannot be read, nothing is hidden and
// Install is possible.
func TestCatalogShowsRemapApps(t *testing.T) {
	for name, opts := range map[string][]func(*Server){
		"remapped box":          {withRemap(t, true)},
		"remap state not known": {withUnreadableRemap(t)},
	} {
		t.Run(name, func(t *testing.T) {
			h := remapHarness(t, opts...)
			if got := entryIDs(decodeJSON[appsBody](t, h.getOK("/api/v1/catalog")).Apps); !slices.Equal(got, []string{"plain", "plk", "poz"}) {
				t.Errorf("GET /catalog = %v, want every app", got)
			}
			if got := entryIDs(decodeJSON[appsBody](t, h.getOK("/api/v1/catalog/search?q=app")).Apps); !slices.Equal(got, []string{"plain", "plk", "poz"}) {
				t.Errorf("search = %v, want every app", got)
			}
			for _, id := range []string{"poz", "plk"} {
				plan := decodeJSON[InstallPlanDTO](t, h.getOK("/api/v1/catalog/"+id+"/install-plan"))
				if plan.Unavailable != "" {
					t.Errorf("%s install plan unavailable = %q, want empty", id, plan.Unavailable)
				}
			}
		})
	}
}
