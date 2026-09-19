package updatetarget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// DefaultURL is where a hosted box asks what it should be running. The control
// plane serves it publicly and unauthenticated, because "what is the newest
// moose" is not a secret — which is also why this can ship before the box↔cloud
// credential exists (UPDATES.md # 8.1 parks that in NEXT.md).
//
// It is a **default, not a constant**: the URL is configuration so a box can be
// pointed at an alternative source to prove a release before the fleet gets it.
// Keeping it configurable also keeps the seam honest — the box knows there is an
// update-target source, not who operates it.
const DefaultURL = "https://api.onmoose.io/api/updates/target"

// fetchTimeout bounds one read. The answer is a few hundred bytes off a cached
// endpoint; anything slower than this is a source that is effectively down, and
// an unreachable source is a no-op.
const fetchTimeout = 30 * time.Second

// maxBodyBytes caps what will be read from the source. The answer is one small
// JSON object. Anything in megabytes is a misconfigured source or a hostile one,
// and neither deserves the box's memory.
const maxBodyBytes = 64 << 10

// RedactURL reduces a URL to the part that is safe to write where a person will
// read it — a log line, or the diagnostic this box serves at
// GET /v1/system/update-target. It keeps the scheme, host and path, and drops
// **both** places a secret can hide.
//
// The update-target URL is operator-settable (a seed field, or
// MOOSE_UPDATE_TARGET_URL), so nothing stops a box being pointed at
// `https://user:secret@host/target` or at `https://host/target?token=secret`.
// The errors below name the URL they failed on, which is what makes a broken
// source fixable — and, unredacted, is what would carry that secret into an API
// response.
//
// Measured, because the defaults are not obvious: `http.Client.Do` returns a
// *url.Error that strips the password (`user:***`) and keeps the whole query,
// and `url.Parse` returns one that keeps everything, password included. Neither
// is safe to pass through, which is why callers pair this with CauseOf below.
//
// A query is replaced rather than deleted, so a diagnostic still says one was
// there — a box asking `?channel=candidate` and a box asking nothing are
// different situations, and the difference is worth keeping.
//
// A URL that will not parse is not passed through at all: the bytes we could
// not read are exactly the bytes we cannot prove are safe. The caller's own
// message says which setting was at fault, which is the part that fixes a typo.
func RedactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unreadable URL)"
	}
	if u.User != nil {
		u.User = url.User("redacted")
	}
	if u.RawQuery != "" {
		u.RawQuery = "redacted"
	}
	u.Fragment = ""
	return u.String()
}

// CauseOf unwraps a *url.Error to the failure underneath it, dropping the URL
// that type carries. Callers name the URL themselves, through RedactURL; this
// is what stops the raw one riding in alongside it.
func CauseOf(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		return ue.Err
	}
	return err
}

// Doer is the HTTP surface this source needs. Consumer-side (CLAUDE.md # Go code
// discipline), and small enough that a test drives it with httptest.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// HTTPSource reads the target from an update-target URL. This is the **hosted**
// implementation of Source.
type HTTPSource struct {
	// URL is the update-target endpoint; empty means DefaultURL.
	URL string
	// BoxID is this box's identity, sent as the box_id query parameter so the
	// control plane can answer for this box and not for the fleet (UPDATES.md
	// # 8.1). Empty means "no identity", and then nothing is sent.
	BoxID string
	// HTTP is the client; nil means a plain client with fetchTimeout.
	HTTP Doer
}

// requestURL is the exact URL this box asks on.
//
// **A box with no identity sends no parameter at all.** An empty `box_id=` is a
// different statement: it names a box called nothing, and the control plane
// would have to guess what to do with it. An appliance box, and a hosted box
// with no seed, both take this path, so their request stays byte-for-byte what
// it was before boxes said who they are.
//
// The parameter is merged into whatever query the configured URL already has,
// so a box pointed at `…/target?channel=candidate` keeps its channel.
func (s HTTPSource) requestURL() (string, error) {
	raw := s.URL
	if raw == "" {
		raw = DefaultURL
	}
	if s.BoxID == "" {
		return raw, nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("updatetarget: %s is not a URL: %w", RedactURL(raw), CauseOf(err))
	}
	q := u.Query()
	q.Set("box_id", s.BoxID)
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// wireTarget is the answer as it arrives. It is a CONTRACT WITH THE CONTROL
// PLANE: encoding/json silently drops what it does not model, so a rename on
// either side is a two-repo change, not a refactor.
//
// **Unknown fields are ignored on purpose.** The answer may carry more than the
// box models — that is how the sender ships a new optional field without a fleet
// update — but the fields below are modelled rather than passed through
// opaquely, because they are what the box validates and acts on.
type wireTarget struct {
	Version     string    `json:"version"`
	BrainImage  string    `json:"brain_image"`
	BrainDigest string    `json:"brain_digest"`
	UIImage     string    `json:"ui_image"`
	UIDigest    string    `json:"ui_digest"`
	PublishedAt time.Time `json:"published_at"`
	// Window is optional. An answer that leaves it out has no opinion about
	// when this box may update, and the box then keeps its own setting.
	Window string `json:"window"`
}

// Target reads the update-target URL.
//
// A **404 is ErrNoTarget**, not an error: the source is up and saying "nothing
// is published on this channel". The control plane answers 404 rather than an
// empty 200 for exactly this reason — "no target" and "a target with empty
// fields" must never be the same response, or a box could read an unpinned
// answer as an instruction.
//
// Everything else that goes wrong — DNS, a refused connection, a 500, a body
// that is not JSON — is an error, and the loop treats it as a no-op that keeps
// the box on its current version.
func (s HTTPSource) Target(ctx context.Context) (Target, error) {
	url, err := s.requestURL()
	if err != nil {
		return Target{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Target{}, fmt.Errorf("updatetarget: build request for %s: %w", RedactURL(url), CauseOf(err))
	}
	client := s.HTTP
	if client == nil {
		client = &http.Client{Timeout: fetchTimeout}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Target{}, fmt.Errorf("updatetarget: fetch %s: %w", RedactURL(url), CauseOf(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return Target{}, ErrNoTarget
	}
	if resp.StatusCode != http.StatusOK {
		return Target{}, fmt.Errorf("updatetarget: fetch %s: HTTP %d", RedactURL(url), resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return Target{}, fmt.Errorf("updatetarget: read %s: %w", RedactURL(url), err)
	}
	if len(b) > maxBodyBytes {
		return Target{}, fmt.Errorf("updatetarget: %s answered more than %d bytes", RedactURL(url), maxBodyBytes)
	}
	var w wireTarget
	if err := json.Unmarshal(b, &w); err != nil {
		return Target{}, fmt.Errorf("updatetarget: parse the answer from %s: %w", RedactURL(url), err)
	}
	return Target{
		Version:     w.Version,
		BrainImage:  w.BrainImage,
		BrainDigest: w.BrainDigest,
		UIImage:     w.UIImage,
		UIDigest:    w.UIDigest,
		PublishedAt: w.PublishedAt,
		Window:      w.Window,
	}, nil
}
