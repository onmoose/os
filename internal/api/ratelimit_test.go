package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/store"
)

// --- token-bucket unit tests (deterministic, fake clock) -----------------

// fixedClock returns a clock the test advances by hand, so refill/reap timing
// is exact instead of racing wall-clock.
type fixedClock struct{ t time.Time }

func (c *fixedClock) now() time.Time      { return c.t }
func (c *fixedClock) add(d time.Duration) { c.t = c.t.Add(d) }

func newFixedClock() *fixedClock {
	return &fixedClock{t: time.Unix(1_600_000_000, 0)}
}

func (s *bucketSet) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buckets)
}

func TestBucketSet_BurstThenThrottle(t *testing.T) {
	clk := newFixedClock()
	bs := newBucketSet(3, 1, clk.now) // burst 3, 1 token/s

	for i := 0; i < 3; i++ {
		if ok, _ := bs.allow("k"); !ok {
			t.Fatalf("request %d within the burst: want allowed", i)
		}
	}
	ok, retry := bs.allow("k")
	if ok {
		t.Fatal("4th request past the burst: want throttled")
	}
	if retry <= 0 {
		t.Fatalf("throttled request: want a positive retry, got %s", retry)
	}
}

func TestBucketSet_RefillsOverTime(t *testing.T) {
	clk := newFixedClock()
	bs := newBucketSet(2, 1, clk.now) // burst 2, 1 token/s

	// Drain the bucket.
	bs.allow("k")
	bs.allow("k")
	if ok, _ := bs.allow("k"); ok {
		t.Fatal("bucket should be empty")
	}

	// One second refills exactly one token.
	clk.add(time.Second)
	if ok, _ := bs.allow("k"); !ok {
		t.Fatal("after 1s: want one refilled token")
	}
	if ok, _ := bs.allow("k"); ok {
		t.Fatal("after 1s only one token should have refilled")
	}

	// A long idle refills only up to capacity, never beyond.
	clk.add(time.Hour)
	for i := 0; i < 2; i++ {
		if ok, _ := bs.allow("k"); !ok {
			t.Fatalf("refill should be capped at burst: request %d want allowed", i)
		}
	}
	if ok, _ := bs.allow("k"); ok {
		t.Fatal("refill exceeded capacity")
	}
}

func TestBucketSet_PerKeyIsolation(t *testing.T) {
	clk := newFixedClock()
	bs := newBucketSet(1, 1, clk.now)

	if ok, _ := bs.allow("a"); !ok {
		t.Fatal("a's first request should be allowed")
	}
	if ok, _ := bs.allow("a"); ok {
		t.Fatal("a is at its limit")
	}
	if ok, _ := bs.allow("b"); !ok {
		t.Fatal("b has its own independent budget")
	}
}

func TestBucketSet_RetryAfterReflectsRefillRate(t *testing.T) {
	clk := newFixedClock()
	bs := newBucketSet(2, 0.5, clk.now) // 0.5 token/s, like the IP plane

	bs.allow("k")
	bs.allow("k")
	ok, retry := bs.allow("k")
	if ok {
		t.Fatal("bucket should be drained")
	}
	// Empty bucket at 0.5 token/s needs 2s for the next whole token.
	if got := retryAfterSeconds(retry); got != 2 {
		t.Fatalf("retry-after at 0.5 token/s: want 2s, got %ds (%s)", got, retry)
	}
}

func TestRetryAfterSeconds_FlooredAtOne(t *testing.T) {
	// A sub-second refill still tells the caller to wait a whole second.
	if got := retryAfterSeconds(100 * time.Millisecond); got != 1 {
		t.Fatalf("sub-second retry: want floored to 1, got %d", got)
	}
	if got := retryAfterSeconds(0); got != 1 {
		t.Fatalf("zero retry: want floored to 1, got %d", got)
	}
	if got := retryAfterSeconds(1500 * time.Millisecond); got != 2 {
		t.Fatalf("1.5s retry: want rounded up to 2, got %d", got)
	}
}

func TestBucketSet_ReapsIdleEntries(t *testing.T) {
	clk := newFixedClock()
	bs := newBucketSet(5, 1, clk.now)

	bs.allow("idle")
	if bs.size() != 1 {
		t.Fatalf("after one request: want 1 tracked bucket, got %d", bs.size())
	}

	// Past the reap interval AND the idle TTL, the next request on a different
	// key sweeps the dormant one.
	clk.add(rateLimitReapEvery + rateLimitIdleTTL + time.Second)
	bs.allow("fresh")
	if bs.size() != 1 {
		t.Fatalf("idle bucket should have been reaped, leaving only the fresh one; got %d", bs.size())
	}
}

func TestBucketSet_ReapKeepsActiveEntries(t *testing.T) {
	clk := newFixedClock()
	bs := newBucketSet(5, 1, clk.now)

	bs.allow("active")
	bs.allow("dormant") // both last-touched at T0

	// Refresh "active" just before the idle window closes, leaving "dormant"
	// untouched.
	clk.add(rateLimitIdleTTL - time.Minute)
	bs.allow("active") // active.last is now recent; no sweep yet (< reapEvery)

	// Cross both the reap interval and "dormant"'s idle TTL. The next request
	// sweeps: "dormant" (idle > TTL) is dropped, "active" (idle < TTL) is kept,
	// "trigger" is created — selective, not indiscriminate.
	clk.add(2 * time.Minute)
	bs.allow("trigger")
	if bs.size() != 2 {
		t.Fatalf("reap should drop only the dormant bucket, keeping active + trigger; got %d", bs.size())
	}
}

// TestRateLimit_ProductionPlaneParams pins the spec-locked thresholds so a
// careless edit to the constants fails loudly (BRAIN_UI_PROTOCOL.md # Rate
// limiting & abuse: per-session 120/min burst 60; per-IP 30/min).
func TestRateLimit_ProductionPlaneParams(t *testing.T) {
	if sessionRatePerMin != 120 || sessionRateBurst != 60 {
		t.Fatalf("per-session plane locked at 120/min burst 60; got %d/min burst %d", sessionRatePerMin, sessionRateBurst)
	}
	if ipRatePerMin != 30 {
		t.Fatalf("per-IP plane locked at 30/min; got %d/min", ipRatePerMin)
	}
}

// --- middleware tests (httptest, fake clock, limiter-only Server) ---------

// probeLimiterServer builds a Server carrying only the limiter — enough to drive
// the rateLimit middleware in isolation. Custom plane params keep the tests fast
// and deterministic.
func probeLimiterServer(sessionCap, ipCap float64, now func() time.Time) *Server {
	return &Server{limiter: &rateLimiter{
		session: newBucketSet(sessionCap, 1, now),
		ip:      newBucketSet(ipCap, 1, now),
	}}
}

// authedReq builds a request already carrying a resolved session identity, as
// authMiddleware would have attached upstream of the limiter.
func authedReq(path, userID, token string) *http.Request {
	r := httptest.NewRequest("GET", path, nil)
	id := auth.Identity{User: store.User{ID: userID}, Session: store.Session{Token: token}}
	return r.WithContext(auth.WithIdentity(r.Context(), id))
}

// countingNext is a terminal handler that records how many requests reached it
// and answers 200.
func countingNext(hits *int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		*hits++
		w.WriteHeader(http.StatusOK)
	})
}

func TestRateLimitMiddleware_SessionThrottle(t *testing.T) {
	clk := newFixedClock()
	srv := probeLimiterServer(2, 5, clk.now)
	var hits int
	h := srv.rateLimit(countingNext(&hits))

	// First two requests on the session pass; the third is throttled.
	for i := 0; i < 2; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, authedReq("/api/v1/me", "u1", "sess1"))
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d: want 200, got %d", i, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("/api/v1/me", "u1", "sess1"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("over-limit request: want 429, got %d", rr.Code)
	}
	if hits != 2 {
		t.Fatalf("throttled request should not reach the handler: want 2 hits, got %d", hits)
	}

	body := decodeJSON[rateLimitedBody](t, rr.Result())
	if body.Code != "rate-limited" {
		t.Fatalf("429 code: want rate-limited, got %q", body.Code)
	}
	if body.Details.Scope != "session" {
		t.Fatalf("429 scope: want session, got %q", body.Details.Scope)
	}
	if body.Details.RetryAfterS < 1 {
		t.Fatalf("429 retry_after_s: want >=1, got %d", body.Details.RetryAfterS)
	}
	if got := rr.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After header: want 1, got %q", got)
	}
}

func TestRateLimitMiddleware_PerSessionIsolation(t *testing.T) {
	clk := newFixedClock()
	srv := probeLimiterServer(1, 5, clk.now)
	var hits int
	h := srv.rateLimit(countingNext(&hits))

	// Drain session 1.
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("/api/v1/me", "u1", "sess1"))
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("/api/v1/me", "u1", "sess1"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("session 1 over limit: want 429, got %d", rr.Code)
	}

	// Session 2 has its own budget.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, authedReq("/api/v1/me", "u2", "sess2"))
	if rr.Code != http.StatusOK {
		t.Fatalf("session 2 first request: want 200, got %d", rr.Code)
	}
}

func TestRateLimitMiddleware_IPPlaneOnAllowlist(t *testing.T) {
	clk := newFixedClock()
	srv := probeLimiterServer(5, 1, clk.now)
	var hits int
	h := srv.rateLimit(countingNext(&hits))

	// Unauthenticated request (no identity in context) → per-IP plane, keyed on
	// RemoteAddr. httptest.NewRequest defaults RemoteAddr to 192.0.2.1.
	pub := func(remote string) *http.Request {
		r := httptest.NewRequest("GET", "/api/v1/auth/state", nil)
		r.RemoteAddr = remote
		return r
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, pub("1.2.3.4:5555"))
	if rr.Code != http.StatusOK {
		t.Fatalf("first IP request: want 200, got %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, pub("1.2.3.4:6666"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second request from same IP: want 429, got %d", rr.Code)
	}
	if body := decodeJSON[rateLimitedBody](t, rr.Result()); body.Details.Scope != "ip" {
		t.Fatalf("429 scope: want ip, got %q", body.Details.Scope)
	}

	// A different IP keeps its own budget.
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, pub("5.6.7.8:5555"))
	if rr.Code != http.StatusOK {
		t.Fatalf("different IP: want 200, got %d", rr.Code)
	}
}

func TestRateLimitMiddleware_StreamingExempt(t *testing.T) {
	clk := newFixedClock()
	srv := probeLimiterServer(1, 1, clk.now) // burst 1 — would throttle on the 2nd hit
	var hits int
	h := srv.rateLimit(countingNext(&hits))

	for _, path := range []string{
		"/api/v1/events",
		"/api/v1/system/live",
		"/api/v1/files/content?path=/x",
	} {
		for i := 0; i < 3; i++ {
			rr := httptest.NewRecorder()
			h.ServeHTTP(rr, authedReq(path, "u1", "sess1"))
			if rr.Code != http.StatusOK {
				t.Fatalf("%s request %d: streams are exempt, want 200, got %d", path, i, rr.Code)
			}
		}
	}
	if hits != 9 {
		t.Fatalf("all streaming requests should reach the handler: want 9 hits, got %d", hits)
	}
	// The exempt path must never have touched the session bucket.
	if n := srv.limiter.session.size(); n != 0 {
		t.Fatalf("streaming requests must not consume the session bucket; got %d tracked", n)
	}
}

// --- end-to-end wiring (real chain through Handler) -----------------------

// TestRateLimit_WiredIntoChain proves the limiter actually sits in the served
// chain and keys on the resolved session: an authenticated route returns the
// locked 429 once the per-session budget is spent. The harness swaps in a
// fixed-clock limiter so refill never masks the cap.
func TestRateLimit_WiredIntoChain(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "correct-horse-battery") // jars an authenticated session

	// Swap to a tiny, fixed-clock limiter so exactly `cap` authenticated
	// requests pass before a 429. Safe post-construction: no request is in
	// flight (setup ran synchronously over h.do).
	clk := newFixedClock()
	const cap = 3
	h.srvServer().limiter = &rateLimiter{
		session: newBucketSet(cap, 2, clk.now),
		ip:      newBucketSet(30, 0.5, clk.now),
	}

	for i := 0; i < cap; i++ {
		resp := h.do("GET", "/api/v1/me", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("authenticated request %d within budget: want 200, got %d", i, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp := h.do("GET", "/api/v1/me", nil)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over-budget request: want 429, got %d", resp.StatusCode)
	}
	if ra := resp.Header.Get("Retry-After"); ra == "" {
		t.Fatal("429 response is missing the Retry-After header")
	}
	body := decodeJSON[rateLimitedBody](t, resp)
	if body.Code != "rate-limited" || body.Details.Scope != "session" {
		t.Fatalf("429 body: want code=rate-limited scope=session, got code=%q scope=%q", body.Code, body.Details.Scope)
	}
}

// --- forged X-Forwarded-For (#329) ----------------------------------------

// TestRateLimit_ForgedForwardedForSharesOneBucket is the regression test for
// #329: the per-IP plane keyed on the *first* X-Forwarded-For hop, which the
// caller writes, so rotating the header minted a fresh bucket per request and
// the throttle protected nothing. Driving the real chain (authMiddleware →
// rateLimit) with N distinct forged values from one peer must still exhaust one
// bucket.
func TestRateLimit_ForgedForwardedForSharesOneBucket(t *testing.T) {
	clk := newFixedClock()
	const cap = 3
	srv := probeLimiterServer(5, cap, clk.now)
	var hits int
	h := srv.authMiddleware(srv.rateLimit(countingNext(&hits)))

	forged := func(n int) *http.Request {
		r := httptest.NewRequest("GET", "/api/v1/auth/state", nil)
		r.RemoteAddr = "203.0.113.7:5555"
		r.Header.Set("X-Forwarded-For", fmt.Sprintf("10.9.%d.%d", n/256, n%256))
		return r
	}
	for i := 0; i < cap; i++ {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, forged(i))
		if rr.Code != http.StatusOK {
			t.Fatalf("request %d within one peer's budget: want 200, got %d", i, rr.Code)
		}
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, forged(cap))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("a fresh forged X-Forwarded-For minted a new bucket: want 429, got %d", rr.Code)
	}
	if hits != cap {
		t.Fatalf("throttled request reached the handler: want %d hits, got %d", cap, hits)
	}
}

// TestRateLimit_TrustedProxyForwardedForIsHonoured is the other half of #329:
// once the peer *is* the box's Caddy, the header it appends is the only thing
// that can distinguish two LAN clients, so each still gets its own bucket. A fix
// that simply ignored X-Forwarded-For would pool the whole LAN behind Caddy's
// address and let one attacker throttle everyone.
func TestRateLimit_TrustedProxyForwardedForIsHonoured(t *testing.T) {
	clk := newFixedClock()
	srv := probeLimiterServer(5, 1, clk.now)
	srv.SetTrustedProxies(DefaultTrustedProxies()) // the shipped set, not a set tailored to this test
	var hits int
	h := srv.authMiddleware(srv.rateLimit(countingNext(&hits)))

	viaCaddy := func(client string) *http.Request {
		r := httptest.NewRequest("GET", "/api/v1/auth/state", nil)
		r.RemoteAddr = "172.18.0.3:5555"
		r.Header.Set("X-Forwarded-For", client)
		return r
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, viaCaddy("192.168.1.20"))
	if rr.Code != http.StatusOK {
		t.Fatalf("first request from LAN client: want 200, got %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, viaCaddy("192.168.1.20"))
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("second request from the same LAN client: want 429, got %d", rr.Code)
	}
	rr = httptest.NewRecorder()
	h.ServeHTTP(rr, viaCaddy("192.168.1.21"))
	if rr.Code != http.StatusOK {
		t.Fatalf("a different LAN client behind the same proxy must keep its own budget: got %d", rr.Code)
	}
}
