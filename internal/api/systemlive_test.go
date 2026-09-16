package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/systemlive"
)

// constSampler is the canned host-resources source the api harness wires into
// its live hub. Counters climb one "second" per call so the hub can diff a real
// rate after the cold frame. It satisfies systemlive's (unexported) sampler.
type constSampler struct {
	mu sync.Mutex
	n  int64
}

func (s *constSampler) SystemResources(context.Context) (protocol.SystemResources, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	n := s.n
	return protocol.SystemResources{
		TsNs:    n * int64(time.Second),
		CPU:     protocol.CPUCounters{TotalJiffies: n * 400, IdleJiffies: n * 300},
		LoadAvg: [3]float64{0.42, 0.51, 0.48},
		Mem:     protocol.MemCounters{TotalBytes: 16728338432, AvailableBytes: 9214455808, UsedBytes: 7513882624},
		Net:     []protocol.NetCounters{{Iface: "eth0", RxBytes: n * 120000, TxBytes: n * 48000}},
		Disk:    []protocol.DiskCounters{{Dev: "sda", ReadBytes: n * 90000, WriteBytes: n * 14000}},
		UptimeS: n,
	}, nil
}

// The stream is not on the public allowlist, so the auth middleware rejects an
// unauthenticated request before the handler runs (BRAIN_UI_PROTOCOL.md: the
// moose_session cookie carries the SSE handshake).
func TestSystemLive_RequiresAuth(t *testing.T) {
	h := newHarness(t)
	resp := h.do("GET", "/api/v1/system/live", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated stream: want 401, got %d", resp.StatusCode)
	}
}

// The stream is available to every signed-in user — no role gate
// (LOCAL_ANALYTICS.md # Privacy model; DASHBOARD.md # the top bar).
func TestSystemLive_MemberCanStream(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "correct-horse-battery")
	h.addMember("u_member1", "bob", "bobpass")
	h.loginAs("bob", "bobpass")

	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/system/live", nil)
	for _, c := range h.jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream GET as member: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("member stream: want 200, got %d", resp.StatusCode)
	}
}

// An authenticated client opens the stream and sees `event: sample` frames whose
// JSON carries the live levels (Done-when, issue #5). The cold first frame's rate
// fields are null — we assert the levels, which are present from frame one.
func TestSystemLive_StreamsSampleFrames(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("admin", "correct-horse-battery") // captures the session cookie into the jar

	req, _ := http.NewRequest("GET", h.srv.URL+"/api/v1/system/live", nil)
	for _, c := range h.jar.Cookies(req.URL) {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type: want text/event-stream, got %q", ct)
	}

	type frame struct {
		data string
		ok   bool
	}
	got := make(chan frame, 1)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		sawEvent := false
		for sc.Scan() {
			line := sc.Text()
			if line == "event: sample" {
				sawEvent = true
				continue
			}
			if sawEvent && strings.HasPrefix(line, "data: ") {
				got <- frame{data: strings.TrimPrefix(line, "data: "), ok: true}
				return
			}
		}
		got <- frame{}
	}()

	select {
	case f := <-got:
		if !f.ok {
			t.Fatal("stream ended before an event: sample frame")
		}
		var s systemlive.Sample
		if err := json.Unmarshal([]byte(f.data), &s); err != nil {
			t.Fatalf("sample JSON: %v (data=%q)", err, f.data)
		}
		if s.Mem.TotalBytes == 0 || s.UptimeS == 0 {
			t.Errorf("sample levels should be populated on the first frame, got mem=%+v uptime=%d", s.Mem, s.UptimeS)
		}
		if len(s.Net) != 1 || s.Net[0].Iface != "eth0" {
			t.Errorf("sample net: want one eth0 entry, got %+v", s.Net)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for a sample frame")
	}
}
