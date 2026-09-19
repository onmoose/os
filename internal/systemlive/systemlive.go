// Package systemlive produces the brain's live system-resources stream
// (BRAIN_UI_PROTOCOL.md Pattern C, stream 3; LOCAL_ANALYTICS.md # Real-time
// system resources). It owns a ref-counted upstream poller: the first SSE
// subscriber starts a 1 Hz poll of host-agent's raw cumulative counters, each
// poll is diffed against the previous one into rates and fanned out to every
// subscriber, and the last unsubscribe stops the poll — so there is zero idle
// cost on both the brain and host-agent when nobody is watching.
package systemlive

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// sampler is the brain's consumer-side slice of hostclient.Client: just the one
// raw-counter read the hub needs. *hostclient.Client satisfies it; tests pass a
// fake (CLAUDE.md: consumer-side interfaces).
type sampler interface {
	SystemResources(ctx context.Context) (protocol.SystemResources, error)
}

// Sample is the rate-derived payload sent to UI subscribers as `event: sample`
// (BRAIN_UI_PROTOCOL.md:169). The rate fields are pointers so the first sample
// after a cold start — when there is no prior counter to diff against — marshals
// them as JSON null, per the no-replay contract (BRAIN_UI_PROTOCOL.md:179). The
// levels (mem, load, uptime) are absolute and always present. Wire units are SI:
// *_bps are bytes/second, *_bytes are bytes, cpu_pct/load are floats; the UI
// does the human formatting.
type Sample struct {
	CPUPct  *float64   `json:"cpu_pct"`
	Load    [3]float64 `json:"load"`
	Mem     Mem        `json:"mem"`
	Net     []NetRate  `json:"net"`
	Disk    []DiskRate `json:"disk"`
	UptimeS int64      `json:"uptime_s"`
}

// Mem mirrors the host's instantaneous memory levels (bytes).
type Mem struct {
	UsedBytes      int64 `json:"used_bytes"`
	TotalBytes     int64 `json:"total_bytes"`
	AvailableBytes int64 `json:"available_bytes"`
}

// NetRate is one interface's derived throughput. RxBps/TxBps are nil on the
// cold-start frame and for an interface that wasn't in the previous sample.
type NetRate struct {
	Iface string `json:"iface"`
	RxBps *int64 `json:"rx_bps"`
	TxBps *int64 `json:"tx_bps"`
}

// DiskRate is one whole-disk device's derived throughput.
type DiskRate struct {
	Dev      string `json:"dev"`
	ReadBps  *int64 `json:"read_bps"`
	WriteBps *int64 `json:"write_bps"`
}

// Hub fans one upstream poll out to all connected SSE subscribers. Construct
// with New; the parent context (process lifetime) tears down any active poll on
// shutdown. Safe for concurrent use.
type Hub struct {
	sampler  sampler
	interval time.Duration
	parent   context.Context

	mu     sync.Mutex
	subs   map[int]chan Sample
	nextID int
	prev   *protocol.SystemResources // last raw sample for diffing; nil = cold
	cancel context.CancelFunc        // stops the poll goroutine; nil = not polling
}

// New returns a Hub that polls s every interval while ≥1 subscriber is
// connected. parent bounds any poll goroutine to the process lifetime.
func New(parent context.Context, s sampler, interval time.Duration) *Hub {
	return &Hub{
		sampler:  s,
		interval: interval,
		parent:   parent,
		subs:     map[int]chan Sample{},
	}
}

// Subscribe registers an SSE consumer. The returned channel delivers derived
// samples (~1/s); the returned func unsubscribes and must be called when the
// stream ends. The first subscriber starts the upstream poller; the last to
// leave stops it.
func (h *Hub) Subscribe() (<-chan Sample, func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	id := h.nextID
	h.nextID++
	ch := make(chan Sample, 4)
	h.subs[id] = ch
	if len(h.subs) == 1 {
		ctx, cancel := context.WithCancel(h.parent)
		h.cancel = cancel
		go h.loop(ctx)
	}
	return ch, func() { h.unsubscribe(id) }
}

func (h *Hub) unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.subs[id]
	if !ok {
		return
	}
	delete(h.subs, id)
	close(ch)
	if len(h.subs) == 0 {
		if h.cancel != nil {
			h.cancel()
			h.cancel = nil
		}
		// Reset the baseline so the next cold start re-nulls its first sample's
		// rates (BRAIN_UI_PROTOCOL.md:179) rather than diffing against a stale
		// pre-shutdown counter.
		h.prev = nil
	}
}

// loop polls immediately — establishing the cold-start baseline, whose
// broadcast carries null rates — then every interval until ctx is cancelled.
func (h *Hub) loop(ctx context.Context) {
	h.poll(ctx)
	t := time.NewTicker(h.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.poll(ctx)
		}
	}
}

// poll reads one raw sample, derives rates against the previous sample, and fans
// the result out to all subscribers. A failed read is logged and skipped (prev
// is kept) so the next tick retries — a transient host-agent hiccup must not
// clear the gauges or reset the rate baseline.
func (h *Hub) poll(ctx context.Context) {
	raw, err := h.sampler.SystemResources(ctx)
	if err != nil {
		slog.Warn("system-live: host-agent sample failed; skipping", "err", err)
		return
	}
	// The upstream read is the slow part (a host-agent round-trip), so this
	// poller's generation may have been cancelled while it was in flight — the
	// user collapsed the dropdown, the last subscriber left, unsubscribe()
	// nilled prev, and a new generation cold-started. Re-check under the lock
	// to close the TOCTOU window: without holding the lock, unsubscribe() could
	// cancel ctx and nil prev between the check and the lock, leaving a stale
	// baseline that would give the next cold start a non-null first frame
	// against BRAIN_UI_PROTOCOL.md:179.
	h.mu.Lock()
	if ctx.Err() != nil {
		h.mu.Unlock()
		return
	}
	sample := derive(h.prev, raw)
	h.prev = &raw
	// Non-blocking send under the lock: instant, and serialized against
	// unsubscribe's close() so we never send on a closed channel. A slow SSE
	// writer drops a frame rather than stalling the single poller.
	for _, ch := range h.subs {
		select {
		case ch <- sample:
		default:
		}
	}
	h.mu.Unlock()
}

// derive turns a raw counter sample into rates against the previous sample. With
// no previous sample (cold start) or a non-positive time delta, every rate field
// is nil (JSON null); the levels (mem/load/uptime) always pass through. Net and
// disk entries are joined to the previous sample by interface/device name, so a
// device that appears mid-stream reads nil until its second sample.
func derive(prev *protocol.SystemResources, cur protocol.SystemResources) Sample {
	s := Sample{
		Load: cur.LoadAvg,
		Mem: Mem{
			UsedBytes:      cur.Mem.UsedBytes,
			TotalBytes:     cur.Mem.TotalBytes,
			AvailableBytes: cur.Mem.AvailableBytes,
		},
		UptimeS: cur.UptimeS,
		Net:     make([]NetRate, len(cur.Net)),
		Disk:    make([]DiskRate, len(cur.Disk)),
	}

	haveRate := prev != nil && cur.TsNs > prev.TsNs
	var dtSec float64
	if haveRate {
		dtSec = float64(cur.TsNs-prev.TsNs) / 1e9
		s.CPUPct = cpuPct(prev.CPU, cur.CPU)
	}

	prevNet := map[string]protocol.NetCounters{}
	prevDisk := map[string]protocol.DiskCounters{}
	if prev != nil {
		for _, n := range prev.Net {
			prevNet[n.Iface] = n
		}
		for _, d := range prev.Disk {
			prevDisk[d.Dev] = d
		}
	}

	for i, n := range cur.Net {
		nr := NetRate{Iface: n.Iface}
		if haveRate {
			if p, ok := prevNet[n.Iface]; ok {
				nr.RxBps = rate(p.RxBytes, n.RxBytes, dtSec)
				nr.TxBps = rate(p.TxBytes, n.TxBytes, dtSec)
			}
		}
		s.Net[i] = nr
	}
	for i, d := range cur.Disk {
		dr := DiskRate{Dev: d.Dev}
		if haveRate {
			if p, ok := prevDisk[d.Dev]; ok {
				dr.ReadBps = rate(p.ReadBytes, d.ReadBytes, dtSec)
				dr.WriteBps = rate(p.WriteBytes, d.WriteBytes, dtSec)
			}
		}
		s.Disk[i] = dr
	}
	return s
}

// cpuPct returns busy/total*100 over the interval from jiffy deltas, or nil when
// the deltas are degenerate (counter reset, or no jiffies elapsed). The result
// is normalized across all cores by /proc/stat's aggregate line, so it stays in
// [0,100].
func cpuPct(prev, cur protocol.CPUCounters) *float64 {
	totalD := cur.TotalJiffies - prev.TotalJiffies
	idleD := cur.IdleJiffies - prev.IdleJiffies
	if totalD <= 0 || idleD < 0 {
		return nil
	}
	busy := totalD - idleD
	if busy < 0 {
		busy = 0
	}
	pct := 100 * float64(busy) / float64(totalD)
	return &pct
}

// rate is a bytes/second figure from two cumulative counters over dtSec. A
// negative delta (counter reset / device reattach) yields nil rather than a
// bogus spike. dtSec is guaranteed > 0 by the caller.
func rate(prev, cur int64, dtSec float64) *int64 {
	d := cur - prev
	if d < 0 {
		return nil
	}
	v := int64(float64(d) / dtSec)
	return &v
}
