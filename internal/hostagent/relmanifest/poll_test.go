package relmanifest

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// cdn is a stand-in for releases.onmoose.io: it serves whatever bytes the
// test puts in it, and can be made to fail the way a real host does.
type cdn struct {
	mu       sync.Mutex
	manifest string
	sig      string
	status   int
	hits     int
}

func newCDN(manifest, sig string) *cdn {
	return &cdn{manifest: manifest, sig: sig, status: http.StatusOK}
}

func (c *cdn) set(manifest, sig string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manifest, c.sig = manifest, sig
}

func (c *cdn) setStatus(code int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status = code
}

func (c *cdn) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits
}

func (c *cdn) server(t *testing.T) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.hits++
		if c.status != http.StatusOK {
			w.WriteHeader(c.status)
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, ".minisig"):
			_, _ = w.Write([]byte(c.sig))
		case strings.HasSuffix(r.URL.Path, ".json"):
			_, _ = w.Write([]byte(c.manifest))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// poller wires a Poller against a test CDN, a real temp state dir, and the
// signer's key.
func newTestPoller(t *testing.T, s signer, c *cdn) *Poller {
	t.Helper()
	srv := c.server(t)
	return &Poller{
		HTTP:     srv.Client(),
		Verifier: NewVerifier(s.publicKey(t)),
		BaseURL:  srv.URL,
		StateDir: t.TempDir(),
	}
}

func TestPollFetchesVerifiesAndCaches(t *testing.T) {
	s := newSigner(t)
	c := newCDN(goodManifest, s.sign([]byte(goodManifest), "t", true))
	p := newTestPoller(t, s, c)

	m, err := p.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	if m.Brain != "1.4.2" {
		t.Fatalf("brain = %q", m.Brain)
	}
	// The cache is what an offline box acts on, so a successful poll must have
	// written it — and it must verify on its own.
	cached, ok, err := Load(p.StateDir, p.Verifier)
	if err != nil || !ok {
		t.Fatalf("cache after poll: ok=%v err=%v", ok, err)
	}
	if cached.Manifest.Brain != "1.4.2" {
		t.Fatalf("cached brain = %q", cached.Manifest.Brain)
	}
	st := p.State()
	if !st.HasManifest || st.LastError != nil || st.LastSuccessAt.IsZero() {
		t.Fatalf("state after a good poll: %+v", st)
	}
}

// The core promise of RELEASE_MANIFEST.md # Failure modes: a bad poll leaves
// the previous valid manifest in effect. Each case below first caches a good
// manifest, then breaks the CDN in one way, and asserts the cache survived.
func TestAFailedPollKeepsTheLastGoodManifest(t *testing.T) {
	cases := map[string]func(t *testing.T, c *cdn, s signer){
		"http error":        func(t *testing.T, c *cdn, s signer) { c.setStatus(http.StatusInternalServerError) },
		"empty body":        func(t *testing.T, c *cdn, s signer) { c.set("", "") },
		"truncated json":    func(t *testing.T, c *cdn, s signer) { c.set(goodManifest[:20], c.sig) },
		"tampered manifest": func(t *testing.T, c *cdn, s signer) { c.set(strings.Replace(goodManifest, "1.4.2", "9.9.9", 1), c.sig) },
		"signature junk":    func(t *testing.T, c *cdn, s signer) { c.set(goodManifest, "not a signature") },
		// A perfectly formed manifest signed by a key this box does not accept
		// — the shape a compromised or wrong publishing path would produce.
		"other key": func(t *testing.T, c *cdn, s signer) {
			evil := newSigner(t)
			body := strings.Replace(goodManifest, "1.4.2", "9.9.9", 1)
			c.set(body, evil.sign([]byte(body), "t", true))
		},
		"wrong schema": func(t *testing.T, c *cdn, s signer) {
			body := strings.Replace(goodManifest, `"manifest_version": 1`, `"manifest_version": 7`, 1)
			c.set(body, s.sign([]byte(body), "t", true))
		},
	}
	for name, breakIt := range cases {
		t.Run(name, func(t *testing.T) {
			s := newSigner(t)
			c := newCDN(goodManifest, s.sign([]byte(goodManifest), "t", true))
			p := newTestPoller(t, s, c)
			if _, err := p.Poll(context.Background()); err != nil {
				t.Fatalf("first poll: %v", err)
			}

			breakIt(t, c, s)
			if _, err := p.Poll(context.Background()); err == nil {
				t.Fatal("second poll succeeded; the CDN was serving something bad")
			}

			cached, ok, err := Load(p.StateDir, p.Verifier)
			if err != nil || !ok {
				t.Fatalf("cache was lost by a failed poll: ok=%v err=%v", ok, err)
			}
			if cached.Manifest.Brain != "1.4.2" {
				t.Fatalf("cache changed on a failed poll: brain = %q", cached.Manifest.Brain)
			}
			st := p.State()
			if st.LastError == nil {
				t.Fatal("state.LastError is nil after a failed poll")
			}
			if !st.HasManifest || st.Manifest.Brain != "1.4.2" {
				t.Fatalf("state dropped the good manifest on a failed poll: %+v", st)
			}
		})
	}
}

// The oversized body is a REAL manifest — correctly signed, valid schema, and
// padded past the cap with an unknown field the parser would otherwise ignore.
// Junk bytes would fail verification anyway and prove nothing about the cap.
func TestPollRefusesAnOversizedBody(t *testing.T) {
	s := newSigner(t)
	padding := strings.Repeat("x", maxBodyBytes)
	huge := strings.Replace(goodManifest, `"channel": "stable",`, `"channel": "stable", "notes": "`+padding+`",`, 1)
	if len(huge) <= maxBodyBytes {
		t.Fatalf("test bug: body is %d bytes, not over the %d cap", len(huge), maxBodyBytes)
	}
	// Sanity: without the cap this body is perfectly acceptable, so the cap is
	// the only thing this test can be failing on.
	if _, err := Parse([]byte(huge)); err != nil {
		t.Fatalf("test bug: the padded manifest does not parse: %v", err)
	}
	c := newCDN(huge, s.sign([]byte(huge), "t", true))
	p := newTestPoller(t, s, c)
	_, err := p.Poll(context.Background())
	if err == nil {
		t.Fatal("Poll accepted a body larger than the cap")
	}
	// The error has to SAY it was too big. Without the explicit size check the
	// read is still truncated, so the poll fails anyway — but as "signature does
	// not verify", which sends whoever reads that log hunting a key problem that
	// does not exist.
	if !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("err = %v, want it to name the size limit", err)
	}
}

func TestPollRefusesAnUnreachableHost(t *testing.T) {
	s := newSigner(t)
	p := &Poller{
		HTTP:     http.DefaultClient,
		Verifier: NewVerifier(s.publicKey(t)),
		// Reserved TEST-NET-1 address; nothing answers, and the fetch context
		// bounds how long that takes.
		BaseURL:  "http://192.0.2.1:9",
		StateDir: t.TempDir(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := p.Poll(ctx); err == nil {
		t.Fatal("Poll succeeded against an unreachable host")
	}
}

// A box that boots offline still knows which release it last heard about.
func TestLoadCacheFillsStateOnBoot(t *testing.T) {
	s := newSigner(t)
	dir := t.TempDir()
	if err := Save(dir, []byte(goodManifest), s.sign([]byte(goodManifest), "t", true)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	p := &Poller{Verifier: NewVerifier(s.publicKey(t)), StateDir: dir}
	p.LoadCache()
	if st := p.State(); !st.HasManifest || st.Manifest.Brain != "1.4.2" {
		t.Fatalf("state after LoadCache: %+v", st)
	}
}

func TestLoadCacheIgnoresACacheItCannotVerify(t *testing.T) {
	s, other := newSigner(t), newSigner(t)
	dir := t.TempDir()
	if err := Save(dir, []byte(goodManifest), s.sign([]byte(goodManifest), "t", true)); err != nil {
		t.Fatalf("Save: %v", err)
	}
	p := &Poller{Verifier: NewVerifier(other.publicKey(t)), StateDir: dir}
	p.LoadCache()
	if st := p.State(); st.HasManifest {
		t.Fatal("LoadCache accepted a cache signed by a key this box does not trust")
	}
}

// Run polls immediately and then on the interval. The clock is injected, so a
// year of polling takes microseconds and no test waits an hour.
func TestRunPollsOnceImmediatelyThenOnTheInterval(t *testing.T) {
	s := newSigner(t)
	c := newCDN(goodManifest, s.sign([]byte(goodManifest), "t", true))
	p := newTestPoller(t, s, c)

	ticks := make(chan time.Time)
	p.After = func(time.Duration) <-chan time.Time { return ticks }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()

	waitFor(t, func() bool { return c.calls() >= 2 }) // manifest + signature
	ticks <- time.Now()
	waitFor(t, func() bool { return c.calls() >= 4 })

	cancel()
	// Unblock a loop that is already waiting on the tick channel.
	select {
	case ticks <- time.Now():
	default:
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// A build with no baked key refuses every manifest, so the loop must not run at
// all — a polling loop that can only ever fail looks like "no releases yet"
// instead of "this build trusts nobody".
func TestRunDoesNothingWithNoKeys(t *testing.T) {
	s := newSigner(t)
	c := newCDN(goodManifest, s.sign([]byte(goodManifest), "t", true))
	p := newTestPoller(t, s, c)
	p.Verifier = NewVerifier()

	done := make(chan struct{})
	go func() { p.Run(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run kept polling with no keys")
	}
	if c.calls() != 0 {
		t.Fatalf("CDN was called %d times by a keyless build", c.calls())
	}
}

// Poll is exported, so a zero-value Poller must refuse rather than panic.
func TestPollWithNoVerifierRefusesRatherThanPanics(t *testing.T) {
	p := &Poller{StateDir: t.TempDir()}
	if _, err := p.Poll(context.Background()); !errors.Is(err, ErrNoKeys) {
		t.Fatalf("err = %v, want ErrNoKeys", err)
	}
}

func TestNextDelayStaysPositiveForShortIntervals(t *testing.T) {
	p := &Poller{Interval: time.Second}
	for i := 0; i < 200; i++ {
		if d := p.nextDelay(); d <= 0 {
			t.Fatalf("nextDelay = %v", d)
		}
	}
}

func TestVerifierFromBakedKeys(t *testing.T) {
	old := BakedKeys
	t.Cleanup(func() { BakedKeys = old })

	a, b := newSigner(t), newSigner(t)
	keyLine := func(s signer) string {
		lines := strings.Split(strings.TrimSpace(s.pubKeyFile()), "\n")
		return lines[len(lines)-1]
	}

	BakedKeys = ""
	if got := VerifierFromBakedKeys().Keys(); got != 0 {
		t.Fatalf("unstamped build accepted %d keys, want 0", got)
	}

	BakedKeys = keyLine(a) + "," + keyLine(b)
	if got := VerifierFromBakedKeys().Keys(); got != 2 {
		t.Fatalf("two stamped keys parsed as %d", got)
	}

	// One broken entry must not take the good one down with it.
	BakedKeys = keyLine(a) + ",not-a-key"
	if got := VerifierFromBakedKeys().Keys(); got != 1 {
		t.Fatalf("one good + one broken key parsed as %d, want 1", got)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}
