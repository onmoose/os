package applog

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

// fakeFollower hands out one channel per Follow call so a test can drive lines
// into the hub and prove how many upstream follows were opened.
type fakeFollower struct {
	mu  sync.Mutex
	chs []chan protocol.JournalLine
}

func newFakeFollower() *fakeFollower { return &fakeFollower{} }

func (f *fakeFollower) JournalFollow(_ context.Context, _ string) (<-chan protocol.JournalLine, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan protocol.JournalLine, 256)
	f.chs = append(f.chs, ch)
	return ch, nil
}

func (f *fakeFollower) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.chs)
}

// channel returns the channel from the callNo-th (1-based) Follow call, waiting
// for that call to happen — run() opens the upstream asynchronously.
func (f *fakeFollower) channel(t *testing.T, callNo int) chan protocol.JournalLine {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		f.mu.Lock()
		if len(f.chs) >= callNo {
			ch := f.chs[callNo-1]
			f.mu.Unlock()
			return ch
		}
		f.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("Follow call #%d never happened", callNo)
		}
		time.Sleep(time.Millisecond)
	}
}

func recvLine(t *testing.T, ch <-chan Line) Line {
	t.Helper()
	select {
	case l := <-ch:
		return l
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a line")
		return Line{}
	}
}

// A reader joining a warm hub receives the whole current backlog as replay
// (lastID 0 = fresh reader, nothing lost), then live lines.
func TestHub_LateJoinerGetsBacklog(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFakeFollower()
	r := newRegistry(ctx, f, time.Hour, 256*1024)

	rep1, ch1, rel1 := r.Subscribe("inst", "c", 0)
	defer rel1()
	if len(rep1) != 0 {
		t.Fatalf("first reader, empty ring: want no replay, got %d", len(rep1))
	}

	up := f.channel(t, 1)
	up <- protocol.JournalLine{Line: "a"}
	up <- protocol.JournalLine{Line: "b"}
	up <- protocol.JournalLine{Line: "c"}
	for i := 0; i < 3; i++ { // drain so all three are in the ring before the join
		recvLine(t, ch1)
	}

	rep2, _, rel2 := r.Subscribe("inst", "c", 0)
	defer rel2()
	if len(rep2) != 3 {
		t.Fatalf("late joiner backlog: want 3, got %d", len(rep2))
	}
	if rep2[0].Data.Line != "a" || rep2[2].Data.Line != "c" {
		t.Errorf("backlog order/content wrong: %+v", rep2)
	}
}

// Two readers of one instance share a single upstream follow.
func TestHub_SharesOneUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFakeFollower()
	r := newRegistry(ctx, f, time.Hour, 256*1024)

	_, ch1, rel1 := r.Subscribe("inst", "c", 0)
	defer rel1()
	f.channel(t, 1) <- protocol.JournalLine{Line: "x"}
	recvLine(t, ch1) // confirms the upstream is open and pumping

	_, _, rel2 := r.Subscribe("inst", "c", 0)
	defer rel2()
	if got := f.callCount(); got != 1 {
		t.Errorf("second reader must reuse the one follow: calls=%d, want 1", got)
	}
}

// A reconnect whose Last-Event-ID is still in the ring replays the tail with no
// gap; a reconnect whose position was evicted leads with a {lost} marker.
func TestHub_ReplayTailVsLostOnEviction(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFakeFollower()
	// Tiny ring so early ids evict. Each line is ~34 bytes (len + 32 overhead).
	r := newRegistry(ctx, f, time.Hour, 120)

	_, ch1, rel1 := r.Subscribe("inst", "c", 0)
	defer rel1()
	up := f.channel(t, 1)
	const n = 20
	for i := 0; i < n; i++ {
		up <- protocol.JournalLine{Line: "L"}
	}
	for i := 0; i < n; i++ {
		recvLine(t, ch1) // ids 1..20 now produced; only the last few remain in the ring
	}

	// lastID just behind the newest is still buffered → tail replay, no lost.
	repTail, _, relT := r.Subscribe("inst", "c", n-1)
	defer relT()
	for _, l := range repTail {
		if l.Data.Lost {
			t.Fatalf("a Last-Event-ID still in the ring must not emit lost: %+v", repTail)
		}
	}
	if len(repTail) != 1 || repTail[0].ID != n {
		t.Errorf("tail replay: want just id %d, got %+v", n, repTail)
	}

	// lastID 1 was long evicted → gap marker leads the replay.
	repLost, _, relL := r.Subscribe("inst", "c", 1)
	defer relL()
	if len(repLost) == 0 || !repLost[0].Data.Lost {
		t.Fatalf("an evicted Last-Event-ID must lead with a lost marker, got %+v", repLost)
	}
}

// After the last reader leaves and the linger elapses, the upstream follow is
// stopped; a fresh reader reopens it.
func TestHub_LingerTeardownReopensUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newFakeFollower()
	r := newRegistry(ctx, f, 10*time.Millisecond, 256*1024)

	_, ch1, rel1 := r.Subscribe("inst", "c", 0)
	f.channel(t, 1) <- protocol.JournalLine{Line: "x"}
	recvLine(t, ch1) // upstream #1 open
	rel1()           // last leave → linger begins

	// Wait out the linger; run() exits and the hub is dropped from the registry.
	deadline := time.Now().Add(time.Second)
	for r.hubCount() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("hub was not torn down after linger")
		}
		time.Sleep(time.Millisecond)
	}

	_, ch2, rel2 := r.Subscribe("inst", "c", 0)
	defer rel2()
	f.channel(t, 2) <- protocol.JournalLine{Line: "y"}
	recvLine(t, ch2)
	if got := f.callCount(); got != 2 {
		t.Errorf("after linger teardown, a fresh reader must reopen the follow: calls=%d, want 2", got)
	}
}

// hubCount exposes the live hub count for teardown assertions.
func (r *Registry) hubCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.hubs)
}
