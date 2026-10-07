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

// A cancelled install stops waiting and returns the last pull error.
func TestPullWithRetryStopsOnCancel(t *testing.T) {
	d := newFakeDocker()
	d.pullFails = 100
	d.pullFailErr = errRateLimited
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := pullWithRetry(ctx, d, "ghcr.io/x/y@sha256:abc"); !errors.Is(err, errRateLimited) {
		t.Fatalf("err = %v, want the rate-limit error", err)
	}
	if got := len(d.pulled()); got != 1 {
		t.Fatalf("Pull called %d times, want 1", got)
	}
}

func TestIsRateLimited(t *testing.T) {
	for msg, want := range map[string]bool{
		"toomanyrequests: retry-after: 25.51µs":                    true,
		"TOOMANYREQUESTS: You have reached your pull rate limit":   true,
		"unexpected status code 429 Too Many Requests":             true,
		"dial tcp: lookup ghcr.io: no such host":                   false,
		"manifest unknown: manifest tagged by \"v1\" is not found": false,
	} {
		if got := isRateLimited(errors.New(msg)); got != want {
			t.Errorf("isRateLimited(%q) = %v, want %v", msg, got, want)
		}
	}
}
