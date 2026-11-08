package caddy

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// On a box the admin API is a unix socket (#608). The client must dial the
// socket, and send a Host header Caddy accepts on one.
func TestNewDialsUnixSocket(t *testing.T) {
	// A short directory: a unix socket path is limited to about 100 bytes,
	// and a test's own temp dir can be longer than that.
	dir, err := os.MkdirTemp("/tmp", "moose-caddy")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "admin.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var gotHost, gotPath string
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotHost, gotPath = r.Host, r.URL.Path
			w.WriteHeader(http.StatusOK)
		}),
		ReadHeaderTimeout: time.Second,
	}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })

	c := New("unix://" + sock)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.WaitReady(ctx); err != nil {
		t.Fatalf("WaitReady over the socket: %v", err)
	}
	if gotPath != "/config/" {
		t.Fatalf("path = %q, want /config/", gotPath)
	}
	if gotHost != "127.0.0.1" {
		t.Fatalf("Host = %q, want 127.0.0.1 (Caddy refuses other hosts on a unix socket)", gotHost)
	}
}

func TestNewKeepsHTTPAddress(t *testing.T) {
	c := New("http://localhost:2019")
	if c.admin != "http://localhost:2019" || c.http.Transport != nil {
		t.Fatalf("a TCP admin address must be used as given: admin=%q transport=%v", c.admin, c.http.Transport)
	}
}
