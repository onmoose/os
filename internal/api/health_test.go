package api

import (
	"context"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/onmoose/moose/internal/audit"
	"github.com/onmoose/moose/internal/auth"
	"github.com/onmoose/moose/internal/catalog"
	"github.com/onmoose/moose/internal/events"
	"github.com/onmoose/moose/internal/health"
	"github.com/onmoose/moose/internal/hostagent"
	"github.com/onmoose/moose/internal/hostclient"
	"github.com/onmoose/moose/internal/protocol"
	"github.com/onmoose/moose/internal/store"
)

// healthHarness wires a real hostagent.Agent (with FakeHealthSource) behind a
// real unix socket, a real hostclient pointing at it, and a real
// health.Manager passed into api.NewServer. This is the production seam end
// to end — the test never reaches into agent internals or fakes the wire.
type healthHarness struct {
	*harness
	healthMgr *health.Manager
	healthSrc *hostagent.FakeHealthSource
	healthSvc *hostagent.FakeServiceReporter
	host      *hostclient.Client
}

func newHealthHarness(t *testing.T) *healthHarness {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "api.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Real hostagent.Agent on a unix socket. We wire stubVerifier-style
	// minimal handlers via the agent's own mux so the brain talks to the
	// production wire format (BRAIN_HOST_PROTOCOL.md), not a hand-rolled
	// JSON mock.
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	src := hostagent.NewFakeHealthSource()
	svc := hostagent.NewFakeServiceReporter()
	a := hostagent.New(
		&alwaysValidVerifier{}, // /setup needs verify-password to succeed
		hostagent.NewFakePublisher(".local"),
	)
	a.Health = src
	a.Services = svc
	mux := http.NewServeMux()
	a.Mount(mux)
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	host := hostclient.New(sock)
	cat := catalog.New(t.TempDir())
	bus := events.NewBus()
	authMgr := auth.NewManager(st)
	healthMgr := health.NewManager(nil)

	apiSrv := NewServer(st, cat, nil, bus, authMgr, host, audit.New(st), healthMgr, nil, nil)
	ts := httptest.NewServer(apiSrv.Handler())
	t.Cleanup(ts.Close)

	jar, _ := cookiejar.New(nil)
	return &healthHarness{
		harness: &harness{
			srv: ts, jar: jar, t: t, st: st,
		},
		healthMgr: healthMgr,
		healthSrc: src,
		healthSvc: svc,
		host:      host,
	}
}

// pull runs one reconciliation cycle (the same shape cmd/brain's
// pullSystemHealth runs at startup and every 60s). Tests call this after
// Set()ing findings on a fake source to advance the brain's view of the world.
// Each report category is reconciled independently — categories are disjoint,
// so iteration order doesn't matter.
func (h *healthHarness) pull() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	sh, err := h.host.SystemHealth(ctx)
	if err != nil {
		h.t.Fatalf("pull SystemHealth: %v", err)
	}
	for cat, findings := range sh.Categories {
		h.healthMgr.ApplyFindings(cat, findings)
	}
}

type alwaysValidVerifier struct{}

func (alwaysValidVerifier) Verify(_, _ string) (bool, error) { return true, nil }

// --- tests ---

func TestHealth_RequiresAuth(t *testing.T) {
	h := newHealthHarness(t)

	resp := h.do("GET", "/api/v1/health", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/v1/health: want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHealth_MemberGetsForbidden(t *testing.T) {
	h := newHealthHarness(t)
	h.setupAdmin("alice", "pass1")
	h.elevate("pass1")

	// Create a member and switch to that session.
	resp := h.do("POST", "/api/v1/users", map[string]any{
		"username": "bob", "password": "bobpass", "role": "member",
	})
	if resp.StatusCode >= 300 {
		body, _ := newCookieRespBody(resp)
		t.Fatalf("create member: %d %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// Log out alice, log in bob.
	h.do("POST", "/api/v1/logout", nil).Body.Close()
	h.do("POST", "/api/v1/login", map[string]string{"username": "bob", "password": "bobpass"}).Body.Close()

	resp = h.do("GET", "/api/v1/health", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("member GET /api/v1/health: want 403, got %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestHealth_AdminSeesEmptyByDefault(t *testing.T) {
	h := newHealthHarness(t)
	h.setupAdmin("alice", "pass1")

	resp := h.do("GET", "/api/v1/health", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("admin GET /api/v1/health: want 200, got %d", resp.StatusCode)
	}
	var out struct {
		Issues []health.Issue `json:"issues"`
	}
	out = decodeJSON[struct {
		Issues []health.Issue `json:"issues"`
	}](t, resp)
	if len(out.Issues) != 0 {
		t.Errorf("want empty issues on healthy box, got %v", out.Issues)
	}
}

func TestHealth_FindingsFlowFromHostAgentToAPI(t *testing.T) {
	h := newHealthHarness(t)
	h.setupAdmin("alice", "pass1")

	// Seed a finding on the host-agent side, then trigger one poll.
	h.healthSrc.Set([]protocol.Finding{
		{ID: "data-drive-missing", Details: "abc-123 not attached"},
	})
	h.pull()

	resp := h.do("GET", "/api/v1/health", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: %d", resp.StatusCode)
	}
	out := decodeJSON[struct {
		Issues []health.Issue `json:"issues"`
	}](t, resp)
	if len(out.Issues) != 1 || out.Issues[0].ID != "data-drive-missing" {
		t.Fatalf("issues: want [data-drive-missing], got %v", out.Issues)
	}
	got := out.Issues[0]
	if got.Severity != health.SeverityError {
		t.Errorf("severity: want error, got %s", got.Severity)
	}
	if !got.BlocksWrites || !got.BlocksApps || !got.BlocksUsers {
		t.Errorf("blocks_* flags must be set for data-drive-missing, got %+v", got)
	}
	if got.Details != "abc-123 not attached" {
		t.Errorf("details: want passthrough, got %q", got.Details)
	}
}

func TestHealth_ClearedFindingDisappearsOnNextPoll(t *testing.T) {
	h := newHealthHarness(t)
	h.setupAdmin("alice", "pass1")

	h.healthSrc.Set([]protocol.Finding{{ID: "data-drive-missing"}})
	h.pull()
	// Drive reattached.
	h.healthSrc.Set(nil)
	h.pull()

	out := decodeJSON[struct {
		Issues []health.Issue `json:"issues"`
	}](t, h.do("GET", "/api/v1/health", nil))
	if len(out.Issues) != 0 {
		t.Errorf("want empty after clear poll, got %v", out.Issues)
	}
}

// TestHealth_ServiceDownDebouncesThenSurfaces exercises the first cross-category
// locus-B detector end to end: a service-down finding from the services category
// debounces (one bad poll surfaces nothing), raises on the second consecutive
// bad poll as a state-category issue, and clears when the service recovers — all
// through the production wire (GET /v1/health/system → ApplyFindings → GET
// /api/v1/health).
func TestHealth_ServiceDownDebouncesThenSurfaces(t *testing.T) {
	h := newHealthHarness(t)
	h.setupAdmin("alice", "pass1")

	issues := func() []health.Issue {
		return decodeJSON[struct {
			Issues []health.Issue `json:"issues"`
		}](t, h.do("GET", "/api/v1/health", nil)).Issues
	}

	h.healthSvc.Set([]protocol.Finding{
		{ID: "service-down", InstanceKey: "docker.service", Details: "docker.service is failed"},
	})

	// First poll: debounced — nothing surfaces yet.
	h.pull()
	if got := issues(); len(got) != 0 {
		t.Fatalf("service-down must debounce — want 0 issues after one bad poll, got %v", got)
	}

	// Second consecutive bad poll: raises as a state-category issue.
	h.pull()
	got := issues()
	if len(got) != 1 || got[0].ID != "service-down" || got[0].InstanceKey != "docker.service" {
		t.Fatalf("want service-down/docker.service after two bad polls, got %v", got)
	}
	if got[0].Category != health.CategoryState {
		t.Errorf("service-down display category: want state, got %s", got[0].Category)
	}

	// Service recovers: clears on the next poll.
	h.healthSvc.Set(nil)
	h.pull()
	if got := issues(); len(got) != 0 {
		t.Errorf("want empty after service recovers, got %v", got)
	}
}

// newCookieRespBody drains resp.Body and returns it as a string, useful for
// surfacing API error payloads in test failures.
func newCookieRespBody(resp *http.Response) (string, error) {
	defer resp.Body.Close()
	b := make([]byte, 512)
	n, _ := resp.Body.Read(b)
	return string(b[:n]), nil
}
