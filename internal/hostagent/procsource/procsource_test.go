//go:build linux

package procsource

import (
	"os"
	"path/filepath"
	"testing"
)

// fixtureRoot writes the given kernel files into a temp tree and returns a
// Sampler rooted there, so tests parse known content and never touch /proc.
func fixtureRoot(t *testing.T, files map[string]string) *Sampler {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &Sampler{root: root}
}

// allFixtures is a coherent kernel-file set: one physical NIC among virtual
// noise, one whole disk with a partition, ten-field cpu line.
func allFixtures() map[string]string {
	return map[string]string{
		// user nice system idle iowait irq softirq steal guest guest_nice
		"proc/stat":    "cpu  4705 150 1120 16250 520 29 35 8 999 111\ncpu0 4705 150 1120 16250 520 29 35 8 999 111\nintr 0\n",
		"proc/meminfo": "MemTotal:       16336268 kB\nMemFree:         1198320 kB\nMemAvailable:    8998492 kB\nBuffers:          517352 kB\n",
		"proc/loadavg": "0.42 0.51 0.48 2/1234 56789\n",
		"proc/net/dev": `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 9999999     100    0    0    0     0          0         0  9999999     100    0    0    0     0       0          0
 enp3s0: 99201234   81234    0    0    0     0          0        12 41200934   52341    0    0    0     0       0          0
docker0:  555555     200    0    0    0     0          0         0   666666     300    0    0    0     0       0          0
veth12ab:  111111     50    0    0    0     0          0         0   222222     60    0    0    0     0       0          0
br-9f8e7d:  333333     70    0    0    0     0          0         0   444444     80    0    0    0     0       0          0
tailscale0:  777777    90    0    0    0     0          0         0   888888     95    0    0    0     0       0          0
`,
		"proc/diskstats": ` 8       0 sda 13927 6573 1244154 7757 31477 28167 2402892 53737 0 26684 61494 0 0 0 0
 8       1 sda1 13694 6519 1240522 7701 28196 28167 2402892 52157 0 25075 59858 0 0 0 0
 259     0 nvme0n1 50000 0 8000000 1000 60000 0 9000000 2000 0 3000 3000 0 0 0 0
 259     1 nvme0n1p1 100 0 5000 10 200 0 7000 20 0 30 30 0 0 0 0
   7     0 loop0 52 0 2122 12 0 0 0 0 0 16 12 0 0 0 0
 253     0 dm-0 1000 0 80000 100 2000 0 90000 200 0 300 300 0 0 0 0
  11     0 sr0 10 0 80 1 0 0 0 0 0 1 1 0 0 0 0
`,
		"proc/uptime": "84021.43 668000.12\n",
	}
}

func TestSample_FullFixture(t *testing.T) {
	s := fixtureRoot(t, allFixtures())
	res, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}

	// cpu: sum of user..steal (first 8 fields), guest/guest_nice excluded.
	if want := int64(4705 + 150 + 1120 + 16250 + 520 + 29 + 35 + 8); res.CPU.TotalJiffies != want {
		t.Errorf("TotalJiffies = %d, want %d (guest fields must not be summed)", res.CPU.TotalJiffies, want)
	}
	if want := int64(16250 + 520); res.CPU.IdleJiffies != want {
		t.Errorf("IdleJiffies = %d, want %d (idle + iowait)", res.CPU.IdleJiffies, want)
	}
	if res.CPU.Cores != 1 {
		t.Errorf("Cores = %d, want 1 (one cpuN line)", res.CPU.Cores)
	}

	// mem: kB → bytes; used = total − available.
	if want := int64(16336268) * 1024; res.Mem.TotalBytes != want {
		t.Errorf("TotalBytes = %d, want %d", res.Mem.TotalBytes, want)
	}
	if want := int64(8998492) * 1024; res.Mem.AvailableBytes != want {
		t.Errorf("AvailableBytes = %d, want %d", res.Mem.AvailableBytes, want)
	}
	if want := int64(16336268-8998492) * 1024; res.Mem.UsedBytes != want {
		t.Errorf("UsedBytes = %d, want %d", res.Mem.UsedBytes, want)
	}

	if res.LoadAvg != [3]float64{0.42, 0.51, 0.48} {
		t.Errorf("LoadAvg = %v", res.LoadAvg)
	}

	// net: lo/docker0/veth*/br-* filtered out; enp3s0 + tailscale0 (mesh) kept,
	// sorted by name; rx is the 1st counter, tx the 9th.
	if len(res.Net) != 2 || res.Net[0].Iface != "enp3s0" || res.Net[1].Iface != "tailscale0" {
		t.Fatalf("Net ifaces = %+v, want [enp3s0 tailscale0]", res.Net)
	}
	if res.Net[0].RxBytes != 99201234 || res.Net[0].TxBytes != 41200934 {
		t.Errorf("enp3s0 counters = %+v", res.Net[0])
	}

	// disk: whole disks only, sorted; sectors × 512.
	if len(res.Disk) != 2 || res.Disk[0].Dev != "nvme0n1" || res.Disk[1].Dev != "sda" {
		t.Fatalf("Disk devs = %+v, want [nvme0n1 sda]", res.Disk)
	}
	if res.Disk[1].ReadBytes != 1244154*512 || res.Disk[1].WriteBytes != 2402892*512 {
		t.Errorf("sda counters = %+v", res.Disk[1])
	}

	if res.UptimeS != 84021 {
		t.Errorf("UptimeS = %d, want 84021", res.UptimeS)
	}
	if res.TsNs <= 0 {
		t.Errorf("TsNs = %d, want positive", res.TsNs)
	}
}

func TestIncludeIface(t *testing.T) {
	cases := map[string]bool{
		"lo":              false,
		"docker0":         false,
		"docker_gwbridge": false,
		"br-9f8e7d6c":     false,
		"veth12ab":        false,
		"eth0":            true,
		"enp3s0":          true,
		"wlan0":           true,
		"wlp2s0":          true,
		"tailscale0":      true, // the mesh interface is reported (LOCAL_ANALYTICS.md)
	}
	for name, want := range cases {
		if got := includeIface(name); got != want {
			t.Errorf("includeIface(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestWholeDisk(t *testing.T) {
	cases := map[string]bool{
		"sda":       true,
		"sdab":      true,
		"hda":       true,
		"vda":       true,
		"xvda":      true,
		"nvme0n1":   true,
		"mmcblk0":   true,
		"sda1":      false,
		"nvme0n1p2": false,
		"mmcblk0p1": false,
		"loop0":     false,
		"dm-0":      false,
		"md0":       false,
		"zram0":     false,
		"sr0":       false,
		"ram0":      false,
	}
	for dev, want := range cases {
		if got := wholeDisk.MatchString(dev); got != want {
			t.Errorf("wholeDisk(%q) = %v, want %v", dev, got, want)
		}
	}
}

// Older kernels emit fewer cpu fields; whatever is present is summed.
func TestParseCPU_ShortLine(t *testing.T) {
	c, err := parseCPU("cpu  100 0 50 800 25\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.TotalJiffies != 975 {
		t.Errorf("TotalJiffies = %d, want 975", c.TotalJiffies)
	}
	if c.IdleJiffies != 825 {
		t.Errorf("IdleJiffies = %d, want 825 (idle + iowait)", c.IdleJiffies)
	}
}

// Cores counts every cpuN line, and the aggregate line is read even when it is
// not first.
func TestParseCPU_Cores(t *testing.T) {
	stat := "cpu0 1 0 1 10 0\ncpu  4 0 4 40 0\ncpu1 1 0 1 10 0\ncpu2 1 0 1 10 0\ncpu3 1 0 1 10 0\nintr 0\n"
	c, err := parseCPU(stat)
	if err != nil {
		t.Fatal(err)
	}
	if c.Cores != 4 {
		t.Errorf("Cores = %d, want 4", c.Cores)
	}
	if c.TotalJiffies != 48 {
		t.Errorf("TotalJiffies = %d, want 48 (the aggregate line only)", c.TotalJiffies)
	}
}

func TestSample_Errors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{"missing file", func(f map[string]string) { delete(f, "proc/meminfo") }},
		{"no cpu line", func(f map[string]string) { f["proc/stat"] = "intr 0\n" }},
		{"meminfo missing MemAvailable", func(f map[string]string) { f["proc/meminfo"] = "MemTotal: 100 kB\n" }},
		{"garbled loadavg", func(f map[string]string) { f["proc/loadavg"] = "x y z\n" }},
		{"truncated net/dev line", func(f map[string]string) { f["proc/net/dev"] = "h1\nh2\n eth0: 1 2 3\n" }},
		{"empty uptime", func(f map[string]string) { f["proc/uptime"] = "\n" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := allFixtures()
			tc.mutate(files)
			if _, err := fixtureRoot(t, files).Sample(); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

// TestSample_RealProc exercises the real kernel files (this package is
// Linux-only, so /proc is always there): two samples must read coherently and
// the cumulative counters must not go backwards — the monotonicity the brain's
// rate derivation depends on.
func TestSample_RealProc(t *testing.T) {
	s := New()
	first, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Sample()
	if err != nil {
		t.Fatal(err)
	}
	if first.Mem.TotalBytes <= 0 || first.Mem.AvailableBytes <= 0 {
		t.Errorf("mem not populated: %+v", first.Mem)
	}
	if first.UptimeS <= 0 {
		t.Errorf("UptimeS = %d, want positive", first.UptimeS)
	}
	if second.CPU.TotalJiffies < first.CPU.TotalJiffies || second.CPU.IdleJiffies < first.CPU.IdleJiffies {
		t.Errorf("cpu jiffies went backwards: %+v then %+v", first.CPU, second.CPU)
	}
	if second.TsNs <= first.TsNs {
		t.Errorf("ts_ns not increasing: %d then %d", first.TsNs, second.TsNs)
	}
	for _, n := range first.Net {
		if !includeIface(n.Iface) {
			t.Errorf("excluded iface %q leaked into the sample", n.Iface)
		}
	}
	for _, d := range first.Disk {
		if !wholeDisk.MatchString(d.Dev) {
			t.Errorf("non-whole-disk device %q leaked into the sample", d.Dev)
		}
	}
}
