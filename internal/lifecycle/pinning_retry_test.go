package lifecycle

// The rate-limit retry around an image pull (#586).

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/onmoose/os/internal/store"
)

// errRateLimited is the error the CLI driver returns for ghcr's 429.
var errRateLimited = errors.New("pull ghcr.io/x/y@sha256:abc: exit status 1\ntoomanyrequests: retry-after: 25.51µs, allowed: 44000/minute")

func fastPullRetries(t *testing.T) {
	t.Helper()
	old := pullRetryDelays
	pullRetryDelays = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { pullRetryDelays = old })
}

// A registry that rate-limits a pull twice and then serves it installs the app.
func TestInstallRetriesRateLimitedPull(t *testing.T) {
	fastPullRetries(t)
	e := newTestEnv(t)
	e.writeCatalogApp(t, "whoami", whoamiCompose, whoamiManifest(testDigest))
	e.docker.pullFails = 2
	e.docker.pullFailErr = errRateLimited

	if _, err := e.m.Install(context.Background(), mustLoadApp(t, e.m, "whoami"), Owner{UserID: "u_admin", Username: "admin"}, store.ScopePersonal, nil, "", nil, nil, nil); err != nil {
		t.Fatalf("install: %v", err)
	}
	if got := len(e.docker.pulled()); got != 3 {
		t.Fatalf("Pull called %d times, want 3 (two rate-limited, one served)", got)
	}
}

// A pull that stays rate-limited fails with the registry's error after the
// last retry.
func TestPullWithRetryGivesUp(t *testing.T) {
	fastPullRetries(t)
	d := newFakeDocker()
	d.pullFails = 100
	d.pullFailErr = errRateLimited

	err := pullWithRetry(context.Background(), d, "ghcr.io/x/y@sha256:abc")
	if !errors.Is(err, errRateLimited) {
		t.Fatalf("err = %v, want the rate-limit error", err)
	}
	if got, want := len(d.pulled()), len(pullRetryDelays)+1; got != want {
		t.Fatalf("Pull called %d times, want %d", got, want)
	}
}

// Any other pull error returns at once, so an unreachable registry still fails
// fast and the offline fallback still engages without waiting.
func TestPullWithRetrySkipsOtherErrors(t *testing.T) {
	fastPullRetries(t)
	d := newFakeDocker()
	d.pullErrAll = fmt.Errorf("dial tcp: registry unreachable")

	if err := pullWithRetry(context.Background(), d, "ghcr.io/x/y@sha256:abc"); err == nil {
		t.Fatalf("want the pull error")
	}
	if got := len(d.pulled()); got != 1 {
		t.Fatalf("Pull called %d times, want 1", got)
	}
}

// A cancelled install stops waiting, and its error says it was cancelled as
// well as carrying the last pull error.
func TestPullWithRetryStopsOnCancel(t *testing.T) {
	d := newFakeDocker()
	d.pullFails = 100
	d.pullFailErr = errRateLimited
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := pullWithRetry(ctx, d, "ghcr.io/x/y@sha256:abc")
	if !errors.Is(err, context.Canceled) || !errors.Is(err, errRateLimited) {
		t.Fatalf("err = %v, want both context.Canceled and the rate-limit error", err)
	}
	if got := len(d.pulled()); got != 1 {
		t.Fatalf("Pull called %d times, want 1", got)
	}
}

func TestIsRateLimited(t *testing.T) {
	for _, tc := range []struct {
		ref, msg string
		want     bool
	}{
		// The real ghcr failure (#586), and Docker Hub's form.
		{"ghcr.io/x/y@sha256:abc", "pull ghcr.io/x/y@sha256:abc: exit status 1\nghcr.io/x/y@sha256:abc: Pulling from x/y\ntoomanyrequests: retry-after: 25.51µs, allowed: 44000/minute", true},
		{"nginx:1", "pull nginx:1: exit status 1\nError response from daemon: toomanyrequests: You have reached your pull rate limit", true},
		{"ghcr.io/x/y:1", "pull ghcr.io/x/y:1: exit status 1\nunexpected status code 429 Too Many Requests", true},
		// A short image name that is part of the error text still matches.
		{"requests", "pull requests: exit status 1\nError response from daemon: toomanyrequests: You have reached your pull rate limit", true},
		{"many", "pull many: exit status 1\nunexpected status code 429 Too Many Requests", true},
		// Other errors, and an image whose name is the error code.
		{"ghcr.io/x/y:1", "pull ghcr.io/x/y:1: exit status 1\ndial tcp: lookup ghcr.io: no such host", false},
		{"ghcr.io/org/toomanyrequests:latest", "pull ghcr.io/org/toomanyrequests:latest: exit status 1\nmanifest unknown", false},
		{"toomanyrequests", "pull toomanyrequests: exit status 1\nError response from daemon: pull access denied for toomanyrequests", false},
	} {
		if got := isRateLimited(errors.New(tc.msg), tc.ref); got != tc.want {
			t.Errorf("isRateLimited(%q, %q) = %v, want %v", tc.msg, tc.ref, got, tc.want)
		}
	}
}

// Cancelling the install during a backoff wait ends the wait at once, with no
// further pull.
func TestPullWithRetryCancelDuringWait(t *testing.T) {
	old := pullRetryDelays
	pullRetryDelays = []time.Duration{time.Hour}
	t.Cleanup(func() { pullRetryDelays = old })
	d := newFakeDocker()
	d.pullFails = 100
	d.pullFailErr = errRateLimited
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- pullWithRetry(ctx, d, "ghcr.io/x/y@sha256:abc") }()
	time.Sleep(20 * time.Millisecond) // let the first pull fail and the wait start
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("pullWithRetry did not stop when the context was cancelled")
	}
	if got := len(d.pulled()); got != 1 {
		t.Fatalf("Pull called %d times, want 1", got)
	}
}
