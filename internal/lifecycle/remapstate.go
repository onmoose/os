package lifecycle

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// RemapState is what the brain knows about the Docker userns-remap on this
// box, for the store. The install path does not use it: it reads the remap
// itself on every install (hostIdentity), so a stale value here can never let
// an install through.
type RemapState int

const (
	// RemapUnknown: host-agent or Docker could not be read, or they disagree.
	// The store then hides nothing, since it must not hide apps on a guess.
	// The install path refuses every install on its own in that case.
	RemapUnknown RemapState = iota
	// RemapOff: both agree the box runs no remap. A root_setup or image_user
	// app cannot be installed here.
	RemapOff
	// RemapOn: both agree the box runs the remap.
	RemapOn
)

// How long a read is kept. A box's remap is fixed when its image is built
// (APP_ISOLATION.md # User-namespace tiers), so a known answer can be kept a
// long time. An unknown one is read again soon, so a Docker that was still
// starting is seen once it answers.
const (
	remapKnownTTL   = 10 * time.Minute
	remapUnknownTTL = 30 * time.Second
	remapReadWait   = 5 * time.Second
)

type remapCache struct {
	mu    sync.Mutex
	state RemapState
	until time.Time
	// now is time.Now in production; tests move it.
	now func() time.Time
}

// RemapState returns the cached remap state, and reads it again when the
// cache has run out. Concurrent callers wait for one read rather than each
// making their own.
func (m *Manager) RemapState(ctx context.Context) RemapState {
	c := &m.remap
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	if !c.until.IsZero() && now().Before(c.until) {
		return c.state
	}
	c.state = m.readRemapState(ctx)
	ttl := remapKnownTTL
	if c.state == RemapUnknown {
		ttl = remapUnknownTTL
	}
	c.until = now().Add(ttl)
	return c.state
}

// readRemapState makes the same check as the install path (hostIdentity):
// host-agent's remap_base and Docker's security options must agree.
func (m *Manager) readRemapState(ctx context.Context) RemapState {
	if m.host == nil || m.docker == nil {
		return RemapUnknown
	}
	// The result is shared by every caller for a while, so one request that
	// goes away must not cut the read short and leave an unknown in the cache.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), remapReadWait)
	defer cancel()
	_, base, err := m.hostIdentity(ctx)
	if err != nil {
		// hostIdentity logs a disagreement itself; a failed read is logged
		// here. Either way the store shows every app.
		if !errors.Is(err, ErrRemapMismatch) {
			slog.Warn("could not read the userns remap for the store; hiding no app", "err", err)
		}
		return RemapUnknown
	}
	if base > 0 {
		return RemapOn
	}
	return RemapOff
}
