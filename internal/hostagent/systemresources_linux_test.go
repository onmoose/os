//go:build linux

package hostagent

import (
	"net/http"
	"testing"

	"github.com/onmoose/os/internal/hostagent/procsource"
	"github.com/onmoose/os/internal/protocol"
)

// End-to-end smoke for the real sampler through the HTTP handler: the same
// wiring cmd/host-agent-real does (a.System = procsource.New()), served over
// GET /v1/system/resources, must carry this box's actual kernel counters —
// the inner-loop equivalent of opening the live-resources panel on a real
// install (issue #115 done-when).
func TestSystemResources_RealSamplerEndToEnd(t *testing.T) {
	a, mux := newTestAgent(&stubVerifier{})
	a.System = procsource.New()

	w := get(t, mux, "/v1/system/resources")
	if w.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", w.Code, w.Body.String())
	}
	s := decodeBody[protocol.SystemResources](t, w)
	if s.CPU.TotalJiffies <= 0 || s.CPU.IdleJiffies <= 0 {
		t.Errorf("cpu counters not real: %+v", s.CPU)
	}
	if s.Mem.TotalBytes <= 0 || s.Mem.AvailableBytes <= 0 || s.Mem.UsedBytes <= 0 {
		t.Errorf("mem levels not real: %+v", s.Mem)
	}
	if s.UptimeS <= 0 || s.TsNs <= 0 {
		t.Errorf("uptime/ts not real: uptime=%d ts=%d", s.UptimeS, s.TsNs)
	}
	// The synthetic fallback's signature values must be gone: it always reports
	// exactly one iface "eth0" and one disk "sda" with load [0.42 0.51 0.48].
	if s.LoadAvg == [3]float64{0.42, 0.51, 0.48} {
		t.Errorf("loadavg is the synthetic constant — sampler not in the path")
	}
}
