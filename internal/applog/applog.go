// Package applog is the brain's authoritative per-app log fan-out
// (BRAIN_UI_PROTOCOL.md Pattern C; LOGGING.md # Per-app logs). It sits between
// host-agent's per-app log tail (one upstream follow per instance) and the
// dashboard's many SSE readers, and owns the user-facing reconnect contract the
// host-agent deliberately does not: a rolling ~256 KiB ring buffer, replay from
// a reader's Last-Event-ID, a single {"lost":true} marker when a reconnect's
// position has been evicted, and a brief linger so a quick reader reconnect
// reuses the warm buffer instead of cold-starting host-agent.
//
// One Hub per instance, ref-counted like systemlive.Hub: the first reader of an
// instance opens the upstream follow, the last to leave stops it (after the
// linger), so there is zero idle cost when nobody is watching. Each line read
// from upstream is stamped with the brain's own monotonic event id — host-agent's
// per-connection ids are not forwarded — so ids are stable across the brain's
// own upstream reconnects.
package applog

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// follower is the consumer-side slice of hostclient.Client the registry needs:
// just the per-app log tail. *hostclient.Client satisfies it; tests pass a fake
// (CLAUDE.md: consumer-side interfaces).
type follower interface {
	JournalFollow(ctx context.Context, container string) (<-chan protocol.JournalLine, error)
}

// Line is one fan-out frame: the brain's monotonic event id (the SSE `id:`) and
// the host-agent payload (the `data:`). A Line whose Data.Lost is true is the
// gap marker — it carries no log text.
type Line struct {
	ID   uint64
	Data protocol.JournalLine
}

const (
	defaultMaxBytes = 256 * 1024
	defaultLinger   = 3 * time.Second
	// subBuffer sizes each reader's live channel. A reader that falls this far
	// behind is closed (forcing an EventSource reconnect + ring replay) rather
	// than having a line silently dropped — an unmarked gap would violate the
	// reconnect contract.
	subBuffer = 256
)

// Registry owns one Hub per instance id. Construct with NewRegistry; the parent
// context (process lifetime) tears down every upstream follow on shutdown.
type Registry struct {
	follower follower
	parent   context.Context
	linger   time.Duration
	maxBytes int

	mu   sync.Mutex
	hubs map[string]*hub
}

// NewRegistry returns a Registry whose hubs follow f, with the production ring
// size and linger.
func NewRegistry(parent context.Context, f follower) *Registry {
	return newRegistry(parent, f, defaultLinger, defaultMaxBytes)
}

// newRegistry is the test seam: it lets tests pick a tiny linger (or zero) and a
// small ring without touching the package defaults.
func newRegistry(parent context.Context, f follower, linger time.Duration, maxBytes int) *Registry {
	return &Registry{
		follower: f,
		parent:   parent,
		linger:   linger,
		maxBytes: maxBytes,
		hubs:     map[string]*hub{},
	}
}

// Subscribe registers a reader of instanceID's logs, opening the upstream follow
// of container if this is the instance's first reader. It returns the replay
// backlog to write before going live (the ring tail from lastID, possibly led by
// a {lost} marker), the live channel of subsequent lines, and a release func the
// caller must invoke when the stream ends.
//
// lastID is the reader's Last-Event-ID (0 for a fresh reader, which receives the
// whole current backlog). It never returns an error: an upstream connect failure
// surfaces as the live channel closing immediately, which an EventSource retries
// — and a non-200 status is unreadable by EventSource anyway.
func (r *Registry) Subscribe(instanceID, container string, lastID uint64) (replay []Line, live <-chan Line, release func()) {
	for {
		r.mu.Lock()
		h := r.hubs[instanceID]
		if h == nil {
			h = r.startHub(instanceID, container)
			r.hubs[instanceID] = h
		}
		r.mu.Unlock()

		rep, ch, rel, ok := h.subscribe(lastID)
		if ok {
			return rep, ch, rel
		}
		// The hub was tearing down between our lookup and subscribe (its linger
		// fired). Drop it if it's still mapped and retry with a fresh one.
		r.mu.Lock()
		if r.hubs[instanceID] == h {
			delete(r.hubs, instanceID)
		}
		r.mu.Unlock()
	}
}

func (r *Registry) startHub(instanceID, container string) *hub {
	ctx, cancel := context.WithCancel(r.parent)
	h := &hub{
		registry:   r,
		follower:   r.follower,
		instanceID: instanceID,
		container:  container,
		linger:     r.linger,
		maxBytes:   r.maxBytes,
		subs:       map[int]chan Line{},
		cancel:     cancel,
	}
	go h.run(ctx)
	return h
}

func (r *Registry) remove(instanceID string, h *hub) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.hubs[instanceID] == h {
		delete(r.hubs, instanceID)
	}
}

// hub fans one instance's upstream follow out to all its readers and owns that
// instance's ring buffer + monotonic id counter.
type hub struct {
	registry   *Registry
	follower   follower
	instanceID string
	container  string
	linger     time.Duration
	maxBytes   int
	cancel     context.CancelFunc // stops run(); the upstream follow is bound to its ctx

	mu       sync.Mutex
	subs     map[int]chan Line
	nextSub  int
	buf      []Line // ring, oldest at front; ids strictly increasing
	bufBytes int
	nextID   uint64      // last assigned brain event id
	lingerT  *time.Timer // pending idle teardown; nil when readers are present
	stopped  bool        // teardown has begun; subscribe must refuse
}

// run opens the upstream follow and pumps lines into the ring + fan-out until
// the context is cancelled (linger teardown / process shutdown) or host-agent
// ends the stream. teardown always runs, dropping the hub from the registry.
func (h *hub) run(ctx context.Context) {
	defer h.teardown()
	ch, err := h.follower.JournalFollow(ctx, h.container)
	if err != nil {
		slog.Warn("applog: upstream follow failed", "instance_id", h.instanceID, "err", err)
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-ch:
			if !ok {
				slog.Info("applog: upstream stream ended", "instance_id", h.instanceID)
				return
			}
			h.append(data)
		}
	}
}

// append stamps a line with the next brain id, pushes it into the ring (evicting
// the oldest while over the byte budget), and fans it out. A reader whose buffer
// is full is closed rather than silently skipped — it will reconnect and replay
// from the ring.
func (h *hub) append(data protocol.JournalLine) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	ln := Line{ID: h.nextID, Data: data}
	h.buf = append(h.buf, ln)
	h.bufBytes += sizeOf(data)
	for len(h.buf) > 1 && h.bufBytes > h.maxBytes {
		h.bufBytes -= sizeOf(h.buf[0].Data)
		h.buf = h.buf[1:]
	}
	for id, sub := range h.subs {
		select {
		case sub <- ln:
		default:
			delete(h.subs, id)
			close(sub)
		}
	}
}

// subscribe registers a reader and computes its replay backlog atomically with
// registration, so no line can slip between the backlog snapshot and the live
// stream. ok is false if the hub is already tearing down (caller retries).
func (h *hub) subscribe(lastID uint64) (replay []Line, live <-chan Line, release func(), ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.stopped {
		return nil, nil, nil, false
	}
	if h.lingerT != nil {
		h.lingerT.Stop()
		h.lingerT = nil
	}
	replay = h.buildReplay(lastID)
	ch := make(chan Line, subBuffer)
	id := h.nextSub
	h.nextSub++
	h.subs[id] = ch
	return replay, ch, func() { h.unsubscribe(id) }, true
}

func (h *hub) unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.subs[id]
	if !ok {
		return // already closed by an append overflow
	}
	delete(h.subs, id)
	close(ch)
	if len(h.subs) == 0 && !h.stopped {
		if h.lingerT != nil {
			h.lingerT.Stop()
		}
		h.lingerT = time.AfterFunc(h.linger, h.maybeStop)
	}
}

// maybeStop is the linger callback: tear down only if still idle (a reader may
// have rejoined during the linger).
//
// Timing note: subscribe() stops the linger timer before adding a subscriber,
// but time.Timer.Stop() returns false when the callback is already running —
// meaning maybeStop may read idle=false only after cancel() and before a fresh
// subscriber's channel is closed by teardown(). That subscriber sees its channel
// close, the SSE handler returns, and EventSource reconnects (getting a fresh hub
// from the retry loop in Registry.Subscribe). It's a brief reconnect cycle, not
// data loss — the {lost} replay on reconnect handles the gap.
func (h *hub) maybeStop() {
	h.mu.Lock()
	idle := len(h.subs) == 0
	h.mu.Unlock()
	if idle {
		h.cancel() // → run returns → teardown
	}
}

func (h *hub) teardown() {
	h.mu.Lock()
	h.stopped = true
	subs := h.subs
	h.subs = map[int]chan Line{}
	if h.lingerT != nil {
		h.lingerT.Stop()
		h.lingerT = nil
	}
	h.mu.Unlock()
	for _, sub := range subs {
		close(sub)
	}
	h.registry.remove(h.instanceID, h)
}

// buildReplay returns the frames a reconnecting reader must receive before the
// live stream, given its Last-Event-ID. Called under h.mu.
//
//   - lastID == 0: a fresh reader — hand over the whole current backlog so the
//     Logs tab opens with recent history. Nothing was lost.
//   - empty ring with a prior position: a cold hub after teardown; continuity
//     can't be proven, so emit one {lost}.
//   - caught up (lastID ≥ newest): nothing to replay.
//   - next expected line still in the ring: replay the tail, no gap.
//   - next expected line evicted: emit {lost}, then replay everything left.
func (h *hub) buildReplay(lastID uint64) []Line {
	if lastID == 0 {
		return append([]Line(nil), h.buf...)
	}
	if len(h.buf) == 0 {
		return []Line{h.lostFrame()}
	}
	oldest := h.buf[0].ID
	newest := h.buf[len(h.buf)-1].ID
	if lastID >= newest {
		return nil
	}
	if lastID+1 >= oldest {
		out := make([]Line, 0, len(h.buf))
		for _, ln := range h.buf {
			if ln.ID > lastID {
				out = append(out, ln)
			}
		}
		return out
	}
	return append([]Line{h.lostFrame()}, h.buf...)
}

// lostFrame mints a fresh-id {lost} marker. Called under h.mu.
func (h *hub) lostFrame() Line {
	h.nextID++
	return Line{ID: h.nextID, Data: protocol.JournalLine{Lost: true}}
}

// sizeOf approximates a line's ring footprint for the byte budget. Exactness
// isn't required — the 256 KiB cap is a soft rolling window — so a small fixed
// per-entry overhead plus the field lengths is enough.
func sizeOf(d protocol.JournalLine) int {
	return len(d.Line) + len(d.Ts) + len(d.Stream) + 32
}
