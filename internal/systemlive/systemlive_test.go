package systemlive

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

const sec = int64(time.Second) // 1e9 ns, the per-call ts spacing in rampSampler

// --- derive() rate math (pure, deterministic) ----------------------------

func TestDerive_ColdStartNullRates(t *testing.T) {
	cur := protocol.SystemResources{
		TsNs:    1 * sec,
		CPU:     protocol.CPUCounters{TotalJiffies: 400, IdleJiffies: 300},
		LoadAvg: [3]float64{0.42, 0.51, 0.48},
		Mem:     protocol.MemCounters{TotalBytes: 16, AvailableBytes: 9, UsedBytes: 7},
		Net:     []protocol.NetCounters{{Iface: "eth0", RxBytes: 120000, TxBytes: 48000}},
		Disk:    []protocol.DiskCounters{{Dev: "sda", ReadBytes: 90000, WriteBytes: 14000}},
		UptimeS: 1,
	}
	s := derive(nil, cur)

	if s.CPUPct != nil {
		t.Errorf("cold cpu_pct: want nil, got %v", *s.CPUPct)
	}
	if s.Net[0].RxBps != nil || s.Net[0].TxBps != nil {
		t.Errorf("cold net rates: want nil, got %+v", s.Net[0])
	}
	if s.Disk[0].ReadBps != nil || s.Disk[0].WriteBps != nil {
		t.Errorf("cold disk rates: want nil, got %+v", s.Disk[0])
	}
	// Levels and identity pass through even on the cold frame.
	if s.Mem.TotalBytes != 16 || s.Mem.UsedBytes != 7 || s.Mem.AvailableBytes != 9 {
		t.Errorf("mem level: want pass-through, got %+v", s.Mem)
	}
	if s.Load != [3]float64{0.42, 0.51, 0.48} {
		t.Errorf("load: want pass-through, got %v", s.Load)
	}
	if s.UptimeS != 1 {
		t.Errorf("uptime: want 1, got %d", s.UptimeS)
	}
	if s.Net[0].Iface != "eth0" || s.Disk[0].Dev != "sda" {
		t.Errorf("identity must survive a null rate: got net=%q disk=%q", s.Net[0].Iface, s.Disk[0].Dev)
	}
}

func TestDerive_RatesFromDelta(t *testing.T) {
	prev := protocol.SystemResources{
		TsNs: 1 * sec,
		CPU:  protocol.CPUCounters{TotalJiffies: 400, IdleJiffies: 300},
		Net:  []protocol.NetCounters{{Iface: "eth0", RxBytes: 120000, TxBytes: 48000}},
		Disk: []protocol.DiskCounters{{Dev: "sda", ReadBytes: 90000, WriteBytes: 14000}},
	}
	cur := protocol.SystemResources{
		TsNs: 2 * sec,
		CPU:  protocol.CPUCounters{TotalJiffies: 800, IdleJiffies: 600},
		Net:  []protocol.NetCounters{{Iface: "eth0", RxBytes: 240000, TxBytes: 96000}},
		Disk: []protocol.DiskCounters{{Dev: "sda", ReadBytes: 180000, WriteBytes: 28000}},
	}
	s := derive(&prev, cur)

	// busy = totalΔ(400) - idleΔ(300) = 100; 100/400 = 25%.
	if s.CPUPct == nil || *s.CPUPct != 25.0 {
		t.Errorf("cpu_pct: want 25.0, got %v", s.CPUPct)
	}
	// All counters doubled over a 1s window → delta == prev value.
	assertRate(t, "rx", s.Net[0].RxBps, 120000)
	assertRate(t, "tx", s.Net[0].TxBps, 48000)
	assertRate(t, "read", s.Disk[0].ReadBps, 90000)
	assertRate(t, "write", s.Disk[0].WriteBps, 14000)
}

// The rate denominator is the ts_ns delta, not an assumed 1-second tick.
func TestDerive_RateUsesTimestampDelta(t *testing.T) {
	prev := protocol.SystemResources{TsNs: 0, Net: []protocol.NetCounters{{Iface: "eth0"}}}
	cur := protocol.SystemResources{
		TsNs: sec / 2, // half a second elapsed
		Net:  []protocol.NetCounters{{Iface: "eth0", RxBytes: 60000}},
	}
	s := derive(&prev, cur)
	// 60000 bytes in 0.5s = 120000 B/s.
	assertRate(t, "rx", s.Net[0].RxBps, 120000)
}

func TestDerive_CounterResetYieldsNull(t *testing.T) {
	prev := protocol.SystemResources{TsNs: 1 * sec, Net: []protocol.NetCounters{{Iface: "eth0", RxBytes: 500000}}}
	cur := protocol.SystemResources{TsNs: 2 * sec, Net: []protocol.NetCounters{{Iface: "eth0", RxBytes: 1000}}}
	s := derive(&prev, cur)
	if s.Net[0].RxBps != nil {
		t.Errorf("a counter reset (negative delta) must yield nil, got %d", *s.Net[0].RxBps)
	}
}

func TestDerive_CPUCounterResetYieldsNull(t *testing.T) {
	prev := protocol.SystemResources{TsNs: 1 * sec, CPU: protocol.CPUCounters{TotalJiffies: 800, IdleJiffies: 600}}
	cur := protocol.SystemResources{TsNs: 2 * sec, CPU: protocol.CPUCounters{TotalJiffies: 400, IdleJiffies: 300}}
	if s := derive(&prev, cur); s.CPUPct != nil {
		t.Errorf("cpu total going backwards must yield nil, got %v", *s.CPUPct)
	}
}

func TestDerive_NewInterfaceNullUntilSecondSample(t *testing.T) {
	prev := protocol.SystemResources{TsNs: 1 * sec, Net: []protocol.NetCounters{{Iface: "eth0", RxBytes: 1000}}}
	cur := protocol.SystemResources{TsNs: 2 * sec, Net: []protocol.NetCounters{
		{Iface: "eth0", RxBytes: 3000},
		{Iface: "wg0", RxBytes: 5000}, // appeared this sample — no prior to diff
	}}
	s := derive(&prev, cur)
	if s.Net[0].RxBps == nil {
		t.Error("eth0 (present in prev) should have a rate")
	}
	if s.Net[1].Iface != "wg0" || s.Net[1].RxBps != nil {
		t.Errorf("a newly-appeared interface must read nil, got %+v", s.Net[1])
	}
}

func TestDerive_NonPositiveTimeDeltaYieldsNull(t *testing.T) {
	prev := protocol.SystemResources{TsNs: 2 * sec, Net: []protocol.NetCounters{{Iface: "eth0", RxBytes: 1000}}}
	cur := protocol.SystemResources{TsNs: 2 * sec, Net: []protocol.NetCounters{{Iface: "eth0", RxBytes: 9000}}}
	s := derive(&prev, cur)
	if s.CPUPct != nil || s.Net[0].RxBps != nil {
		t.Errorf("a zero time delta must null all rates, got cpu=%v rx=%v", s.CPUPct, s.Net[0].RxBps)
	}
}

// --- Hub ref-count, fan-out, cold-null, error skip -----------------------

// rampSampler returns monotonically-climbing counters, one "second" per call, so
// successive samples diff to a stable, known rate. It counts calls so a test can
// prove only one poller runs.
type rampSampler struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (s *rampSampler) SystemResources(context.Context) (protocol.SystemResources, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return protocol.SystemResources{}, s.err
	}
	s.calls++
	n := int64(s.calls)
	return protocol.SystemResources{
		TsNs:    n * sec,
		CPU:     protocol.CPUCounters{TotalJiffies: n * 400, IdleJiffies: n * 300},
		LoadAvg: [3]float64{0.42, 0.51, 0.48},
		Mem:     protocol.MemCounters{TotalBytes: 16, AvailableBytes: 9, UsedBytes: 7},
		Net:     []protocol.NetCounters{{Iface: "eth0", RxBytes: n * 120000, TxBytes: n * 48000}},
		Disk:    []protocol.DiskCounters{{Dev: "sda", ReadBytes: n * 90000, WriteBytes: n * 14000}},
		UptimeS: n,
	}, nil
}

func (s *rampSampler) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// isPolling / prevIsNil expose the ref-count bookkeeping for assertions.
func (h *Hub) isPolling() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cancel != nil
}

func (h *Hub) prevIsNil() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prev == nil
}

// prevTsNs reports the baseline's timestamp, or -1 when there is no baseline.
func (h *Hub) prevTsNs() int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.prev == nil {
		return -1
	}
	return h.prev.TsNs
}

// The first frame after a cold start carries null rates (no prior to diff); the
// next poll carries real rates. interval is 1h so the only automatic poll is the
// immediate cold one — the second poll is driven explicitly.
func TestHub_ColdFirstFrameThenRates(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &rampSampler{}
	h := New(ctx, s, time.Hour)

	ch, unsub := h.Subscribe()
	defer unsub()

	f1 := recv(t, ch)
	if f1.CPUPct != nil || f1.Net[0].RxBps != nil {
		t.Errorf("cold first frame must have null rates, got cpu=%v rx=%v", f1.CPUPct, f1.Net[0].RxBps)
	}
	if f1.UptimeS != 1 {
		t.Errorf("cold frame still carries levels: uptime want 1, got %d", f1.UptimeS)
	}

	h.poll(ctx) // second sample → real rates
	f2 := recv(t, ch)
	if f2.CPUPct == nil || *f2.CPUPct != 25.0 {
		t.Errorf("second frame cpu_pct: want 25.0, got %v", f2.CPUPct)
	}
	assertRate(t, "rx", f2.Net[0].RxBps, 120000)
}

func TestHub_RefCountStartsAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &rampSampler{}
	h := New(ctx, s, time.Hour)

	if h.isPolling() {
		t.Fatal("hub must not poll with zero subscribers")
	}

	ch1, unsub1 := h.Subscribe()
	recv(t, ch1) // drain the cold frame → the immediate poll has run (1 call)
	if !h.isPolling() {
		t.Fatal("first subscriber must start the poller")
	}

	_, unsub2 := h.Subscribe()
	if got := s.callCount(); got != 1 {
		t.Errorf("second subscriber must reuse the one poller, not start another: calls=%d, want 1", got)
	}

	unsub1()
	if !h.isPolling() {
		t.Error("poller must keep running while a subscriber remains")
	}

	unsub2()
	if h.isPolling() {
		t.Error("last unsubscribe must stop the poller")
	}
	if !h.prevIsNil() {
		t.Error("last unsubscribe must reset the rate baseline so the next cold start re-nulls")
	}
}

// One upstream poll fans out to every subscriber. A subscriber that joins a warm
// hub gets real rates immediately (it didn't trigger a cold start).
func TestHub_FanOutToAllSubscribers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &rampSampler{}
	h := New(ctx, s, time.Hour)

	ch1, unsub1 := h.Subscribe()
	defer unsub1()
	recv(t, ch1) // cold frame for sub1

	ch2, unsub2 := h.Subscribe()
	defer unsub2()

	h.poll(ctx) // single poll → broadcast to both
	f1 := recv(t, ch1)
	f2 := recv(t, ch2)

	if f1.CPUPct == nil || f2.CPUPct == nil {
		t.Fatalf("both subscribers should receive the same poll's rates, got %v / %v", f1.CPUPct, f2.CPUPct)
	}
	if *f1.CPUPct != *f2.CPUPct {
		t.Errorf("fan-out mismatch: %v vs %v", *f1.CPUPct, *f2.CPUPct)
	}
}

func TestHub_PollErrorSkipsBroadcast(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &rampSampler{err: errors.New("host-agent down")}
	h := New(ctx, s, time.Hour)

	ch, unsub := h.Subscribe()
	defer unsub()

	select {
	case f := <-ch:
		t.Fatalf("a failed poll must not broadcast, got %+v", f)
	case <-time.After(50 * time.Millisecond):
	}
	if !h.prevIsNil() {
		t.Error("an error poll must leave the baseline untouched (still nil after only errors)")
	}
}

// A poll whose generation was cancelled while its (slow) upstream read was in
// flight must be a no-op: it must neither advance the baseline nor broadcast.
// This is the close-then-reopen gesture — a viewer collapses the dropdown
// (unsubscribe → poller cancelled, prev nilled), a new viewer cold-starts a
// fresh generation, and only now does the stranded gen-1 read return. Without
// the ctx re-check in poll(), that late read would repopulate prev (handing the
// fresh cold start a stale baseline → a non-null first frame, against
// BRAIN_UI_PROTOCOL.md:179) and leak an extra frame into the live subscriber.
func TestHub_CancelledGenerationPollIsNoOp(t *testing.T) {
	parent := context.Background()
	s := &rampSampler{}
	h := New(parent, s, time.Hour)

	// Stand up the live generation: one subscriber whose cold poll has run, so
	// prev holds that generation's sample #1 (ts = 1*sec).
	ch, unsub := h.Subscribe()
	defer unsub()
	recv(t, ch)
	if got := h.prevTsNs(); got != 1*sec {
		t.Fatalf("setup: live generation baseline ts: want %d, got %d", 1*sec, got)
	}

	// A stranded poll from an already-cancelled generation: the read succeeds
	// (rampSampler ignores ctx) but the post-read guard must drop it.
	dead, deadCancel := context.WithCancel(parent)
	deadCancel()
	h.poll(dead)

	// No frame leaked to the live subscriber...
	select {
	case f := <-ch:
		t.Fatalf("a cancelled-generation poll must not broadcast, got %+v", f)
	case <-time.After(50 * time.Millisecond):
	}
	// ...and the baseline is untouched (still the live generation's sample #1).
	if got := h.prevTsNs(); got != 1*sec {
		t.Errorf("a cancelled-generation poll must not advance the baseline: want %d, got %d", 1*sec, got)
	}
}

// blockSampler blocks until its gate is closed, then returns one ramp sample.
// Used to hold a poll mid-read so the test can cancel the generation in flight.
type blockSampler struct {
	gate chan struct{}
	ramp rampSampler
}

func (s *blockSampler) SystemResources(ctx context.Context) (protocol.SystemResources, error) {
	<-s.gate
	return s.ramp.SystemResources(ctx)
}

// TestHub_MidFlightCancelIsNoOp covers the TOCTOU window between the sampler
// returning and poll() acquiring the lock. Before the fix, unsubscribe() could
// cancel ctx and nil prev between those two points, then the zombie poll would
// set prev to a stale value — giving the next cold start a non-null first frame.
// With ctx.Err() checked under the lock the window is closed.
func TestHub_MidFlightCancelIsNoOp(t *testing.T) {
	parent := context.Background()
	bs := &blockSampler{gate: make(chan struct{})}
	h := New(parent, bs, time.Hour)

	// Subscribe starts the loop goroutine, which immediately calls poll() and
	// blocks inside bs.SystemResources (gate not yet closed).
	_, unsub := h.Subscribe()

	// Cancel the generation while the read is in flight.
	unsub()
	if !h.prevIsNil() {
		t.Fatal("prev must be nil after last unsubscribe")
	}

	// Release the blocked read. poll() now has raw in hand, acquires the lock,
	// sees ctx.Err() != nil, and returns without touching prev.
	close(bs.gate)
	time.Sleep(20 * time.Millisecond) // let the goroutine finish

	if !h.prevIsNil() {
		t.Error("a poll cancelled mid-flight must not advance the baseline")
	}
}

func assertRate(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s_bps: want %d, got nil", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s_bps: want %d, got %d", name, want, *got)
	}
}

func recv(t *testing.T, ch <-chan Sample) Sample {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a sample")
		return Sample{}
	}
}
