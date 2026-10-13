package fit

import "testing"

const mib = int64(1) << 20

func TestCheck(t *testing.T) {
	// The box from #599: 3.8 GiB of MemTotal, two cores.
	box := Box{MemTotalBytes: 3891 * mib, MemUsedBytes: 600 * mib, Cores: 2}
	withBig := box
	withBig.Running = []RunningApp{
		{InstanceID: "a", Name: "Small", RecommendedMemoryBytes: 256 * mib},
		{InstanceID: "b", Name: "Big", RecommendedMemoryBytes: 2560 * mib},
		{InstanceID: "c", Name: "None"},
	}

	cases := []struct {
		name  string
		box   Box
		mem   int64
		cpu   float64
		state string
	}{
		{"recommends nothing", withBig, 0, 0, Fits},
		{"fits next to what is in use", box, 1024 * mib, 1, Fits},
		{"4G on a 3.8 GiB box is tight, not too big", box, 4096 * mib, 2, Tight},
		{"tight next to a running app", withBig, 1536 * mib, 1, Tight},
		{"more than the box rounded up", box, 5120 * mib, 0, TooBig},
		{"more cores than the box", box, 0, 4, TooBig},
		{"cores unknown skips the CPU check", Box{MemTotalBytes: 8 << 30}, 0, 64, Fits},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Check(c.box, c.mem, c.cpu); got.State != c.state {
				t.Errorf("state = %s, want %s (%+v)", got.State, c.state, got)
			}
		})
	}
}

// The headroom subtracts the larger of the running recommendations and the
// memory in use, so an understated manifest cannot hide a full box.
func TestCheckHeadroomUsesTheLarger(t *testing.T) {
	box := Box{MemTotalBytes: 4096 * mib, MemUsedBytes: 3000 * mib, Running: []RunningApp{{InstanceID: "a", RecommendedMemoryBytes: 512 * mib}}}
	if got := Check(box, 1, 0).FreeMemoryBytes; got != 1096*mib {
		t.Errorf("free = %d MiB, want 1096 MiB (in use is larger)", got/mib)
	}
	box.MemUsedBytes = 100 * mib
	if got := Check(box, 1, 0).FreeMemoryBytes; got != 3584*mib {
		t.Errorf("free = %d MiB, want 3584 MiB (recommendations are larger)", got/mib)
	}
}

func TestCheckTightNamesRunningAppsLargestFirst(t *testing.T) {
	box := Box{MemTotalBytes: 4096 * mib, Running: []RunningApp{
		{InstanceID: "a", RecommendedMemoryBytes: 256 * mib},
		{InstanceID: "z"},
		{InstanceID: "b", RecommendedMemoryBytes: 2048 * mib},
	}}
	got := Check(box, 2048*mib, 0)
	if got.State != Tight {
		t.Fatalf("state = %s, want tight", got.State)
	}
	if len(got.Running) != 2 || got.Running[0].InstanceID != "b" || got.Running[1].InstanceID != "a" {
		t.Errorf("running = %+v, want b then a, without z", got.Running)
	}
	if fits := Check(box, 0, 0); fits.Running != nil {
		t.Errorf("a fitting app names running apps: %+v", fits.Running)
	}
}

func TestCheckTooBigSaysWhich(t *testing.T) {
	got := Check(Box{MemTotalBytes: 2 << 30, Cores: 1}, 4<<30, 2)
	if got.State != TooBig || !got.TooBigMemory || !got.TooBigCPU {
		t.Errorf("got %+v, want too big on both", got)
	}
}
