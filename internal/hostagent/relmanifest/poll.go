package relmanifest

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"time"
)

// DefaultBaseURL is where the appliance release manifest is published
// (RELEASE_MANIFEST.md # What the manifest is). The channel file name is
// appended to it, so a future beta channel is one more file at the same base.
const DefaultBaseURL = "https://releases.onmoose.network"

// PollInterval is the hourly cadence RELEASE_MANIFEST.md specifies.
const PollInterval = time.Hour

// pollJitter is how much the interval is randomly shortened or lengthened on
// each tick.
//
// Not in the spec, and deliberate: every box would otherwise poll on its own
// exact hour boundary from its own boot time, which is fine, until the fleet
// grows and boots cluster (a provider maintenance window, a power cut across a
// region, a coordinated release). Spreading each tick over a few minutes costs
// nothing and removes a self-inflicted spike on a static file host.
const pollJitter = 5 * time.Minute

// fetchTimeout bounds one poll's HTTP work. A CDN that hangs must not hold the
// poller open until the next tick, or two polls would overlap.
const fetchTimeout = 30 * time.Second

// Doer is the HTTP surface the poller needs. Consumer-side by CLAUDE.md's rule,
// and small enough that a test can drive it with httptest without a real
// network.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// maxBodyBytes caps what the poller will read from the CDN. The manifest is a
// few hundred bytes and the signature is one line; anything in megabytes is a
// misconfigured host or a hostile one, and neither deserves the box's memory.
const maxBodyBytes = 64 << 10

// State is what the poller knows right now. It is a snapshot, safe to copy.
//
// Manifest and HasManifest describe the last manifest that **verified** —
// which, after a failed poll, is still the previous good one. LastError
// describes the last attempt. Both matter: a box can be acting on a perfectly
// good manifest while its polls have been failing for a week, and an operator
// needs to see both facts at once.
type State struct {
	Manifest    Manifest
	HasManifest bool
	// LastPollAt is when a poll last completed, successfully or not.
	LastPollAt time.Time
	// LastSuccessAt is when a poll last ended with a verified manifest.
	LastSuccessAt time.Time
	// LastError is the last poll's failure, or nil when it succeeded.
	LastError error
}

// Poller fetches the release manifest on a timer, verifies it, and keeps the
// last good one on disk.
//
// It decides nothing. Decide answers "what should this box do", and the caller
// asks that with the box's own running versions, which the poller has no
// business knowing.
type Poller struct {
	HTTP     Doer
	Verifier *Verifier
	// BaseURL is the manifest host; empty means DefaultBaseURL.
	BaseURL string
	// StateDir is where the cache lives (/var/lib/moose in production).
	StateDir string
	// Interval between polls; zero means PollInterval.
	Interval time.Duration
	// Now and After exist so a test can drive a year of polling in
	// milliseconds. Both default to the real clock.
	Now   func() time.Time
	After func(time.Duration) <-chan time.Time

	mu    sync.Mutex
	state State
}

// State returns a snapshot of what the poller knows.
func (p *Poller) State() State {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *Poller) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *Poller) after(d time.Duration) <-chan time.Time {
	if p.After != nil {
		return p.After(d)
	}
	return time.After(d)
}

func (p *Poller) interval() time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}
	return PollInterval
}

// nextDelay is the interval with jitter applied, never below a second.
func (p *Poller) nextDelay() time.Duration {
	base := p.interval()
	j := pollJitter
	if j > base/2 {
		j = base / 2 // keep a short test interval from going negative
	}
	d := base + time.Duration(rand.Int63n(int64(2*j)+1)) - j
	if d < time.Second {
		d = time.Second
	}
	return d
}

// LoadCache reads the cached manifest into the poller's state, so a box that
// boots offline still knows which release it last heard about
// (RELEASE_MANIFEST.md # Failure modes). A missing or unverifiable cache leaves
// the state empty and is not fatal — the next successful poll fills it.
func (p *Poller) LoadCache() {
	c, ok, err := Load(p.StateDir, p.Verifier)
	if err != nil {
		slog.Warn("release manifest: cached manifest unusable", "err", err)
		return
	}
	if !ok {
		return
	}
	p.mu.Lock()
	p.state.Manifest, p.state.HasManifest = c.Manifest, true
	p.mu.Unlock()
	slog.Info("release manifest: loaded from cache", "brain", c.Manifest.Brain, "ui", c.Manifest.UI)
}

// Poll runs one cycle: fetch the manifest and its signature, verify, parse, and
// cache. It returns the manifest on success.
//
// **A failed poll changes nothing on disk.** An unreachable host, an HTTP
// error, a truncated body or a signature that does not verify all leave the
// cached manifest exactly as it was, because that manifest is what an offline
// box keeps acting on. Clearing it on a bad fetch would turn a network blip
// into a box that has forgotten which release it is on.
func (p *Poller) Poll(ctx context.Context) (Manifest, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	m, err := p.poll(ctx)

	p.mu.Lock()
	p.state.LastPollAt = p.now()
	p.state.LastError = err
	if err == nil {
		p.state.Manifest, p.state.HasManifest = m, true
		p.state.LastSuccessAt = p.state.LastPollAt
	}
	p.mu.Unlock()

	if err != nil {
		// Warn, not Error: an appliance behind a flaky link fails polls
		// routinely and the box is fine. The 24-hour "this keeps failing"
		// signal is a separate, deliberate escalation (# Failure modes).
		slog.Warn("release manifest poll failed; keeping the last known manifest", "err", err)
		return Manifest{}, err
	}
	slog.Info("release manifest polled", "brain", m.Brain, "ui", m.UI, "minimum_host_agent", m.MinimumHostAgent)
	return m, nil
}

func (p *Poller) poll(ctx context.Context) (Manifest, error) {
	// Run refuses to start without keys, but Poll is exported and callable on
	// its own. A zero-value Poller must fail the same way a keyless one does —
	// refusing every manifest — not panic on a nil verifier.
	if p.Verifier == nil {
		return Manifest{}, ErrNoKeys
	}
	base := strings.TrimSuffix(p.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	name := Channel + ".json"

	raw, err := p.fetch(ctx, base+"/"+name)
	if err != nil {
		return Manifest{}, err
	}
	sig, err := p.fetch(ctx, base+"/"+name+".minisig")
	if err != nil {
		return Manifest{}, err
	}
	// Verify before parse, so nothing unsigned ever reaches the fields a
	// decision is made from.
	if _, err := p.Verifier.Verify(raw, string(sig)); err != nil {
		return Manifest{}, err
	}
	m, err := Parse(raw)
	if err != nil {
		return Manifest{}, err
	}
	if err := Save(p.StateDir, raw, string(sig)); err != nil {
		// The manifest is good; only writing it down failed. Return it — the
		// box can act on this poll — and log the cache failure, which shows up
		// as "the box re-fetches from scratch after every reboot".
		slog.Error("release manifest: cache write failed", "err", err)
		return m, nil
	}
	return m, nil
}

func (p *Poller) fetch(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("relmanifest: build request for %s: %w", url, err)
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("relmanifest: fetch %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("relmanifest: fetch %s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("relmanifest: read %s: %w", url, err)
	}
	if len(b) > maxBodyBytes {
		return nil, fmt.Errorf("relmanifest: %s is larger than %d bytes", url, maxBodyBytes)
	}
	return b, nil
}

// Run polls until ctx is done: once at the start, then on the jittered
// interval. It never returns an error — a poll failure is a normal event on a
// box behind a flaky link, and the loop's job is to keep trying.
func (p *Poller) Run(ctx context.Context) {
	if p.Verifier == nil || p.Verifier.Keys() == 0 {
		// Say it once, loudly, at startup rather than on every poll. A box in
		// this state refuses every manifest by design (see ErrNoKeys), so a
		// silent loop would look like "no releases published" instead of "this
		// build trusts nobody".
		slog.Warn("release manifest: no signing keys baked in; polling would refuse every manifest, not starting")
		return
	}
	slog.Info("release manifest: polling", "interval", p.interval().String())
	for {
		_, _ = p.Poll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-p.after(p.nextDelay()):
		}
	}
}
