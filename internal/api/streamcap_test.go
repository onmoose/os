package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/onmoose/moose/internal/auth"
)

// --- streamCap unit tests (deterministic, no HTTP) -----------------------

// snapshot returns the live stream count for token and whether the map still
// tracks it — an in-package accessor for assertions about the cap's bookkeeping.
func (c *streamCap) snapshot(token string) (count int, tracked bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	count, tracked = c.count[token]
	return
}

func TestStreamCap_AcquireUpToLimitThenReject(t *testing.T) {
	c := newStreamCap(3)
	releases := make([]func(), 0, 3)
	for i := 0; i < 3; i++ {
		rel, ok := c.acquire("tok")
		if !ok {
			t.Fatalf("acquire %d within the limit: want ok, got rejected", i)
		}
		releases = append(releases, rel)
	}
	if _, ok := c.acquire("tok"); ok {
		t.Fatal("acquire past the limit: want rejected, got ok")
	}

	// Releasing one frees exactly one slot — and only one.
	releases[0]()
	rel, ok := c.acquire("tok")
	if !ok {
		t.Fatal("acquire after a release: want ok, got rejected")
	}
	_ = rel
	if _, ok := c.acquire("tok"); ok {
		t.Fatal("acquire past the limit again: want rejected")
	}
}

// The HTTP tests shrink the cap for speed, so the production value is otherwise
// only review-visible. Pin it to the spec-locked number (BRAIN_UI_PROTOCOL.md #
// Stream cap: ≤16 concurrent SSE streams per session).
func TestStreamCap_ProductionLimitIsSixteen(t *testing.T) {
	if maxStreamsPerSession != 16 {
		t.Fatalf("BRAIN_UI_PROTOCOL.md locks the per-session cap at 16; got %d", maxStreamsPerSession)
	}
}

func TestStreamCap_PerToken(t *testing.T) {
	c := newStreamCap(1)
	if _, ok := c.acquire("a"); !ok {
		t.Fatal("token a's first acquire should succeed")
	}
	if _, ok := c.acquire("a"); ok {
		t.Fatal("token a is at its limit of 1")
	}
	if _, ok := c.acquire("b"); !ok {
		t.Fatal("token b has its own independent budget")
	}
}

func TestStreamCap_EntryDeletedAtZero(t *testing.T) {
	c := newStreamCap(2)
	r1, _ := c.acquire("tok")
	r2, _ := c.acquire("tok")

	r1()
	if n, _ := c.snapshot("tok"); n != 1 {
		t.Fatalf("after one release: want count 1, got %d", n)
	}
	r2()
	n, tracked := c.snapshot("tok")
	if n != 0 {
		t.Fatalf("after the last release: want count 0, got %d", n)
	}
	if tracked {
		t.Fatal("the map entry should be dropped when a session's last stream closes")
	}
}

// --- end-to-end over HTTP (Done-when, issue #47) -------------------------

// issueSession mints a valid session token for userID, so a test can drive
// several independent sessions for one user without re-logging-in.
func (h *harness) issueSession(userID string) string {
	h.t.Helper()
	sess, err := auth.NewManager(h.st).Issue(userID)
	if err != nil {
		h.t.Fatalf("issue session: %v", err)
	}
	return sess.Token
}

// openStream starts an SSE request carrying token and returns the response (with
// status already available — the handler flushes headers immediately) plus a
// cancel that disconnects the stream. The body is left open so the caller can
// hold the slot; the caller must cancel + close when done.
func openStream(t *testing.T, url, token string) (*http.Response, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		cancel()
		t.Fatalf("new request: %v", err)
	}
	req.AddCookie(&http.Cookie{Name: auth.CookieName, Value: token})
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		cancel()
		t.Fatalf("open stream %s: %v", url, err)
	}
	return resp, cancel
}

// waitStreamCount polls the cap until token's live count reaches want — a
// disconnect frees its slot asynchronously (the server observes the closed
// connection, then the deferred release runs), so the test waits for it.
func waitStreamCount(t *testing.T, c *streamCap, token string, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := c.snapshot(token); n == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	n, _ := c.snapshot(token)
	t.Fatalf("stream count for token did not reach %d (got %d)", want, n)
}

// The cap is per-session and shared by both raw SSE endpoints: filling a
// session's budget across /api/v1/events and /api/v1/system/live refuses the
// next stream with 429, a second session keeps its own budget, and closing a
// stream frees a slot. Exercises all four Done-when criteria of issue #47.
func TestStreamCap_EnforcedAcrossEndpoints(t *testing.T) {
	h := newHarness(t)
	admin := h.setupAdmin("admin", "correct-horse-battery")

	// Shrink the cap so the test opens a handful of streams, not 17. Safe to
	// swap the pointer here: no SSE request is in flight yet (setup ran over
	// h.do, which completes synchronously).
	const limit = 3
	h.srvServer().streams = newStreamCap(limit)
	sc := h.srvServer().streams

	tokenA := h.issueSession(admin.ID)
	tokenB := h.issueSession(admin.ID)
	base := h.srv.URL

	// Track every open stream so cleanup tears them down regardless of where an
	// assertion fails.
	var closers []context.CancelFunc
	t.Cleanup(func() {
		for _, c := range closers {
			c()
		}
	})
	hold := func(resp *http.Response, cancel context.CancelFunc) {
		closers = append(closers, func() { cancel(); resp.Body.Close() })
	}

	// Fill session A's budget across BOTH endpoints — proving they share one
	// per-session count, not one each.
	endpoints := []string{"/api/v1/events", "/api/v1/system/live", "/api/v1/events"}
	for i, ep := range endpoints {
		resp, cancel := openStream(t, base+ep, tokenA)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("stream %d (%s): want 200, got %d", i, ep, resp.StatusCode)
		}
		hold(resp, cancel)
	}

	// The (limit+1)th stream on session A is refused with 429 — on either
	// endpoint.
	over, overCancel := openStream(t, base+"/api/v1/system/live", tokenA)
	if over.StatusCode != http.StatusTooManyRequests {
		t.Errorf("over-cap stream: want 429, got %d", over.StatusCode)
	}
	overCancel()
	over.Body.Close()

	// Per-session: session B has its own full budget even while A is capped.
	respB, cancelB := openStream(t, base+"/api/v1/events", tokenB)
	if respB.StatusCode != http.StatusOK {
		t.Errorf("second session's stream: want 200, got %d", respB.StatusCode)
	}
	hold(respB, cancelB)

	// Releasing one of A's streams frees exactly one slot for A.
	closers[0]() // cancel + close A's first stream
	closers = closers[1:]
	waitStreamCount(t, sc, tokenA, limit-1)

	reopened, reopenCancel := openStream(t, base+"/api/v1/events", tokenA)
	if reopened.StatusCode != http.StatusOK {
		t.Errorf("after a release, a new stream on A: want 200, got %d", reopened.StatusCode)
	}
	hold(reopened, reopenCancel)
}
