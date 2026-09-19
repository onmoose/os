// Package cloud holds no Go production code — it exists only to host this test,
// which exercises the real-cloud seed-fetch logic of moose-seed-materialize.sh
// (#246) against a local mock metadata server. The shell script is the artifact
// baked into the hosted cloud image; testing its fetch/parse/retry/404 paths here
// (in `make check` / ci-go, on every PR) covers the genuinely novel bash so the
// CL6 live boot only has to prove the real Hetzner endpoint + DHCP timing, not the
// shell logic. The SMBIOS short-circuit and the root-owned `install` write are
// unchanged from the #220 path and stay covered by the QEMU cloud lane.
package cloud

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const script = "moose-seed-materialize.sh"

// runFn sources the materializer and runs one shell snippet against it, returning
// stdout and the exit code. The main-guard (BASH_SOURCE != $0) keeps `main` from
// running when sourced, so only the named functions execute.
func runFn(t *testing.T, snippet string, args ...string) (string, int) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("materializer uses bash /dev/tcp + timeout; Linux-only")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available")
	}
	if _, err := exec.LookPath("timeout"); err != nil {
		t.Skip("timeout(1) not available")
	}
	cmdArgs := append([]string{"-c", "source ./" + script + "\n" + snippet, "_"}, args...)
	cmd := exec.Command("bash", cmdArgs...)
	// Keep the tests fast: a 1s per-attempt connect cap and a 1s retry window so
	// the "endpoint never comes up" path gives up in ~2s instead of the 60s default.
	cmd.Env = append(cmd.Environ(),
		"MOOSE_SEED_FETCH_TIMEOUT=1",
		"MOOSE_SEED_FETCH_INTERVAL=1",
		"MOOSE_SEED_FETCH_DEADLINE=1",
	)
	out, err := cmd.Output()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("running snippet %q: %v", snippet, err)
	}
	return string(out), code
}

func TestParseURL(t *testing.T) {
	cases := []struct{ url, want string }{
		{"http://169.254.169.254/hetzner/v1/userdata", "169.254.169.254 80 /hetzner/v1/userdata"},
		{"http://127.0.0.1:8080/userdata", "127.0.0.1 8080 /userdata"},
		{"http://example.test", "example.test 80 /"},
	}
	for _, c := range cases {
		got, code := runFn(t, `parse_url "$1"; printf '%s %s %s' "$host" "$port" "$path"`, c.url)
		if code != 0 {
			t.Errorf("parse_url %q: exit %d", c.url, code)
		}
		if got != c.want {
			t.Errorf("parse_url %q = %q, want %q", c.url, got, c.want)
		}
	}
}

func TestFetchSeed200WritesBodyVerbatim(t *testing.T) {
	const seed = `{"box_id":"cindy-fox","assertion_verification_key":"a2V5"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hetzner/v1/userdata" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(seed))
	}))
	defer srv.Close()

	out, code := runFn(t, `fetch_seed "$1"`, srv.URL+"/hetzner/v1/userdata")
	if code != 0 {
		t.Fatalf("fetch_seed exit %d, want 0", code)
	}
	if out != seed {
		t.Errorf("fetch_seed body = %q, want %q", out, seed)
	}
}

func TestFetchSeed200KeepAliveSocketStillLandsSeed(t *testing.T) {
	// Regression for the cloud#6 live-run bug: Hetzner's metadata server ignores
	// our "Connection: close" and holds the socket open, so the inner `cat <&3`
	// never sees EOF and `timeout` kills it (exit 124) *after* the full 200 was
	// already captured. http_get must decide on the captured bytes, not that exit
	// code, and still hand back the body. TestFetchSeed200WritesBodyVerbatim uses a
	// polite httptest server that closes the socket, so it never exercised this —
	// this test drives a raw listener that deliberately keeps the connection open.
	//
	// Timing invariant (not a hazard): runFn caps the inner read at
	// MOOSE_SEED_FETCH_TIMEOUT=1s. The full seed is written synchronously over
	// loopback (microseconds) before that deadline, so `cat` always captures the
	// whole response and `timeout` only fires on the trailing keep-alive wait —
	// which is exactly the case under test.
	if runtime.GOOS != "linux" {
		t.Skip("materializer uses bash /dev/tcp + timeout; Linux-only")
	}
	const seed = `{"box_id":"keepalive-otter","assertion_verification_key":"a2V5"}`
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed at test end
			}
			go func(c net.Conn) {
				defer c.Close()
				// Drain the request line + headers (read up to the blank line).
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil || line == "\r\n" || line == "\n" {
						break
					}
				}
				_, _ = io.WriteString(c, "HTTP/1.0 200 OK\r\nContent-Length: "+
					strconv.Itoa(len(seed))+"\r\nContent-Type: text/plain\r\n\r\n"+seed)
				// Do NOT close after writing — mirror the Hetzner metadata server
				// holding the socket open. Block until the client (killed by
				// `timeout`) drops its end, then the deferred Close runs.
				_, _ = io.Copy(io.Discard, c)
			}(conn)
		}
	}()

	out, code := runFn(t, `fetch_seed "$1"`, "http://"+ln.Addr().String()+"/hetzner/v1/userdata")
	if code != 0 {
		t.Fatalf("fetch_seed against a keep-alive metadata socket exit %d, want 0", code)
	}
	if out != seed {
		t.Errorf("fetch_seed body = %q, want %q", out, seed)
	}
}

func TestFetchSeed404IsCleanNoSeed(t *testing.T) {
	// A 404 is the un-seeded real-cloud case (no user-data set) — a definitive
	// "no seed", returned fast without exhausting the retry window.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	out, code := runFn(t, `fetch_seed "$1"`, srv.URL+"/hetzner/v1/userdata")
	if code != 1 {
		t.Fatalf("fetch_seed on 404 exit %d, want 1", code)
	}
	if out != "" {
		t.Errorf("fetch_seed on 404 wrote %q, want empty", out)
	}
}

func TestFetchSeedTransientGivesUp(t *testing.T) {
	// Endpoint never reachable (closed port): http_get returns transient, the
	// bounded retry loop rides the deadline and then reports no seed — never hangs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL + "/hetzner/v1/userdata"
	srv.Close() // nothing is listening now -> connection refused

	out, code := runFn(t, `fetch_seed "$1"`, url)
	if code != 1 {
		t.Fatalf("fetch_seed on unreachable endpoint exit %d, want 1", code)
	}
	if out != "" {
		t.Errorf("fetch_seed on unreachable endpoint wrote %q, want empty", out)
	}
}

func TestHTTPGetTransientStatusRetries(t *testing.T) {
	// A non-200/404 status (e.g. a 500 from a flaky metadata proxy) is transient:
	// http_get returns 2 so fetch_seed keeps trying rather than treating it as a
	// definitive answer.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	host := strings.TrimPrefix(srv.URL, "http://")

	_, code := runFn(t, `http_get "${1%%:*}" "${1##*:}" /hetzner/v1/userdata`, host)
	if code != 2 {
		t.Fatalf("http_get on 500 exit %d, want 2 (transient)", code)
	}
}
