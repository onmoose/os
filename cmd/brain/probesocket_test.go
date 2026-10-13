package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/caddy"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/profile"
)

// --- app-unresponsive through Caddy's probe socket (issue #610) ----------

// probeSocketServer stands in for Caddy's moose-probe server on a unix socket.
// It answers every request with status, and with mark in caddy.ProbeHeader
// when mark is set, and records the Host and path it was asked for.
type probeSocketServer struct {
	path string

	mu       sync.Mutex
	status   int
	mark     string
	gotHost  string
	gotPath  string
	requests int
}

func (s *probeSocketServer) set(status int, mark string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status, s.mark = status, mark
}

func (s *probeSocketServer) seen() (host, path string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.gotHost, s.gotPath, s.requests
}

// shortSocketDir is a temp dir short enough for a unix socket path (about 100
// bytes at most), which a test's own temp dir may not be.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "moose-probe")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func startProbeSocket(t *testing.T, status int) *probeSocketServer {
	t.Helper()
	s := &probeSocketServer{path: filepath.Join(shortSocketDir(t), "probe.sock"), status: status}
	ln, err := net.Listen("unix", s.path)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			s.mu.Lock()
			s.gotHost, s.gotPath = r.Host, r.URL.Path
			s.requests++
			status, mark := s.status, s.mark
			s.mu.Unlock()
			if mark != "" {
				w.Header().Set(caddy.ProbeHeader, mark)
			}
			w.WriteHeader(status)
		}),
		ReadHeaderTimeout: time.Second,
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return s
}

// The #610 case: an owner-only app on hosted, whose upstream answers 502. On
// Caddy's site listener the gate answers the probe with its login redirect,
// which is no sample, so the app could never be flagged. Through the probe
// socket the probe reaches the app: it raises on the 2nd failed tick and
// clears on the first good one. The site listener is never asked.
func TestAppProbe_SocketReachesGatedApp(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	d, mgr, rt, _ := gatedScenario(clk, "", nil)
	sock := startProbeSocket(t, http.StatusBadGateway)
	d.SetProbeSocket(sock.path)

	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("first failed tick must not raise, got %v", mgr.List())
	}
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 1 {
		t.Fatalf("a 502 through the probe socket must raise on the 2nd tick, got %v", mgr.List())
	}
	host, path, _ := sock.seen()
	if want := profile.HostedAppHost("cindy-fox", "app"); host != want || path != "/healthz" {
		t.Errorf("probe socket got Host %q path %q, want %q /healthz", host, path, want)
	}
	if rt.last != nil {
		t.Errorf("the probe went to Caddy's site listener (Host %q); with the socket there it must not", rt.last.Host)
	}

	sock.set(http.StatusOK, "")
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("a good answer through the socket must clear, got %v", mgr.List())
	}
}

// The probe server's catch-all (no route for this host) is no sample, not a
// good answer, even for an app whose healthy set holds its 404. And it does
// not hide Docker's own verdict.
func TestAppProbe_SocketNoRouteIsNoSample(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	d, mgr, _, reader := gatedScenario(clk, "", []int{http.StatusNotFound})
	sock := startProbeSocket(t, http.StatusNotFound)
	sock.set(http.StatusNotFound, caddy.ProbeNoRoute)
	d.SetProbeSocket(sock.path)

	p := &manifest.HealthProbe{Path: "/healthz", HealthyStatus: []int{http.StatusNotFound}}
	if got := d.probe(context.Background(), "app.cindy-fox.onmoose.io", true, p); got != probeNoSample {
		t.Fatalf("the no-route answer = %v, want probeNoSample", got)
	}
	// The same 404 without the mark is the app's own answer.
	sock.set(http.StatusNotFound, "")
	if got := d.probe(context.Background(), "app.cindy-fox.onmoose.io", true, p); got != probeGood {
		t.Fatalf("an app's own 404 in its healthy set = %v, want probeGood", got)
	}

	sock.set(http.StatusNotFound, caddy.ProbeNoRoute)
	reader.containers[0].Health = "unhealthy"
	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 1 {
		t.Fatalf("Docker unhealthy must still raise when the probe route is missing, got %v", mgr.List())
	}
}

// A socket file that nothing listens on (Caddy was recreated and its probe
// server is not back yet, #613) is no sample: Caddy is missing, not the app.
func TestAppProbe_DeadSocketIsNoSample(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	d, mgr, rt, _ := gatedScenario(clk, "", nil)
	path := filepath.Join(shortSocketDir(t), "probe.sock")
	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.SetUnlinkOnClose(false)
	ln.Close() // the file stays; a connect is refused
	d.SetProbeSocket(path)

	p := &manifest.HealthProbe{Path: "/healthz"}
	if got := d.probe(context.Background(), "app.cindy-fox.onmoose.io", true, p); got != probeNoSample {
		t.Fatalf("a refused probe socket = %v, want probeNoSample", got)
	}
	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("a dead probe socket must not raise, got %v", mgr.List())
	}
	if rt.last != nil {
		t.Error("with the socket file there, the probe must not fall back to the site listener")
	}
}

// With no socket file (the probe server was never written), the probe keeps
// Caddy's site listener and #609's gate rule.
func TestAppProbe_NoSocketFileKeepsSiteListener(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	d, mgr, rt, _ := gatedScenario(clk, "", nil)
	d.SetProbeSocket(filepath.Join(shortSocketDir(t), "probe.sock"))

	d.check(context.Background())
	d.check(context.Background())
	if rt.last == nil {
		t.Fatal("with no socket file the probe must go to Caddy's site listener")
	}
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("the gate's login redirect must stay no sample, got %v", mgr.List())
	}
}

// The copy of a splash answers 200 whatever the app does, so it is marked and
// read as no sample. A splash copy left behind by a failed flip to the app
// must not make a down app look healthy.
func TestAppProbe_SocketSplashIsNoSample(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	d, _, _, _ := gatedScenario(clk, "", []int{http.StatusOK})
	sock := startProbeSocket(t, http.StatusOK)
	sock.set(http.StatusOK, caddy.ProbeSplash)
	d.SetProbeSocket(sock.path)

	p := &manifest.HealthProbe{Path: "/healthz", HealthyStatus: []int{http.StatusOK}}
	if got := d.probe(context.Background(), "app.cindy-fox.onmoose.io", true, p); got != probeNoSample {
		t.Fatalf("a splash answer = %v, want probeNoSample", got)
	}
}
