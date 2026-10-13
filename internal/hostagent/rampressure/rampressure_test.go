package rampressure

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The fixtures in testdata are in the exact format the kernel prints.
//
//   - psi-stalled.txt: the averages are the ones read on a hosted box with
//     3.8 GB of RAM that sat stalled on memory for 15 hours (#597). Only the
//     total= counters are invented.
//   - psi-recovered.txt: the same box after one app was stopped. The `some`
//     averages are the ones read there; the `full` line is invented to match.
//   - psi-spike.txt: a short spike, such as an image pull. avg10 is high, avg300
//     is not.
//   - psi-idle.txt: a box with ample memory. Every average is zero.
//   - meminfo-healthy.txt / meminfo-low.txt: 3.8 GB of RAM with 1.6 GB and
//     150 MB available.

func fixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func reporter(psi, meminfo string) *Reporter {
	return &Reporter{
		readPSI:     func() (string, error) { return psi, nil },
		readMeminfo: func() (string, error) { return meminfo, nil },
	}
}

func TestParsePSI(t *testing.T) {
	cases := []struct {
		file       string
		some, full float64
	}{
		{"psi-idle.txt", 0, 0},
		{"psi-stalled.txt", 4.11, 3.66},
		{"psi-recovered.txt", 1.52, 0.71},
		{"psi-spike.txt", 1.95, 1.58},
	}
	for _, c := range cases {
		p, ok := parsePSI(fixture(t, c.file))
		if !ok {
			t.Errorf("%s: want ok", c.file)
			continue
		}
		if p.someAvg300 != c.some || p.fullAvg300 != c.full {
			t.Errorf("%s: got some=%v full=%v, want some=%v full=%v", c.file, p.someAvg300, p.fullAvg300, c.some, c.full)
		}
	}
}

func TestParsePSI_Rejects(t *testing.T) {
	for name, in := range map[string]string{
		"garbage":      "not a psi file at all\n",
		"some only":    "some avg10=5.0 avg60=5.0 avg300=5.0 total=1\n",
		"full only":    "full avg10=5.0 avg60=5.0 avg300=5.0 total=1\n",
		"no avg300":    "some avg10=5.0 avg60=5.0 total=1\nfull avg10=5.0 avg60=5.0 total=1\n",
		"bad avg300":   "some avg300=x total=1\nfull avg300=1.0 total=1\n",
		"empty string": "",
	} {
		if _, ok := parsePSI(in); ok {
			t.Errorf("%s: want ok=false", name)
		}
	}
}

func TestParseMeminfo(t *testing.T) {
	m, ok := parseMeminfo(fixture(t, "meminfo-low.txt"))
	if !ok || m.totalKB != 3984588 || m.availableKB != 153600 {
		t.Fatalf("got %+v ok=%v", m, ok)
	}
	for name, in := range map[string]string{
		"no available": "MemTotal: 100 kB\n",
		"no total":     "MemAvailable: 100 kB\n",
		"zero total":   "MemTotal: 0 kB\nMemAvailable: 0 kB\n",
		"bad number":   "MemTotal: x kB\nMemAvailable: 100 kB\n",
	} {
		if _, ok := parseMeminfo(in); ok {
			t.Errorf("%s: want ok=false", name)
		}
	}
}

// TestRead_Thresholds is the "Done when" list of #597, plus the MemAvailable
// signal and the clear condition.
func TestRead_Thresholds(t *testing.T) {
	cases := []struct {
		name, psi, meminfo string
		raise              bool
	}{
		{"stalled box from #597 raises", "psi-stalled.txt", "meminfo-healthy.txt", true},
		{"full avg300 under 1% with normal memory stays clear", "psi-recovered.txt", "meminfo-healthy.txt", false},
		{"short spike in avg10 only stays clear", "psi-spike.txt", "meminfo-healthy.txt", false},
		{"idle box stays clear", "psi-idle.txt", "meminfo-healthy.txt", false},
		{"low available memory raises without PSI stall", "psi-idle.txt", "meminfo-low.txt", true},
		{"both signals bad raises", "psi-stalled.txt", "meminfo-low.txt", true},
	}
	for _, c := range cases {
		got := reporter(fixture(t, c.psi), fixture(t, c.meminfo)).Read()
		if c.raise {
			if len(got) != 1 || got[0].ID != "ram-pressure" || got[0].InstanceKey != "" {
				t.Errorf("%s: want one box-wide ram-pressure finding, got %v", c.name, got)
			}
		} else if got != nil {
			t.Errorf("%s: want nil, got %v", c.name, got)
		}
	}
}

func TestRead_Boundaries(t *testing.T) {
	healthy := fixture(t, "meminfo-healthy.txt")
	idle := fixture(t, "psi-idle.txt")
	atFull := "some avg10=0.00 avg60=0.00 avg300=2.50 total=1\nfull avg10=0.00 avg60=0.00 avg300=2.00 total=1\n"
	if got := reporter(atFull, healthy).Read(); len(got) != 1 {
		t.Errorf("full avg300 exactly at the threshold must raise, got %v", got)
	}
	underFull := "some avg10=0.00 avg60=0.00 avg300=2.50 total=1\nfull avg10=0.00 avg60=0.00 avg300=1.99 total=1\n"
	if got := reporter(underFull, healthy).Read(); got != nil {
		t.Errorf("full avg300 just under the threshold must not raise, got %v", got)
	}
	// 5% of 2000000 kB is 100000 kB: exactly at the floor is healthy.
	if got := reporter(idle, "MemTotal: 2000000 kB\nMemAvailable: 100000 kB\n").Read(); got != nil {
		t.Errorf("MemAvailable exactly at the floor must not raise, got %v", got)
	}
	if got := reporter(idle, "MemTotal: 2000000 kB\nMemAvailable: 99999 kB\n").Read(); len(got) != 1 {
		t.Errorf("MemAvailable just under the floor must raise, got %v", got)
	}
}

func TestRead_Details(t *testing.T) {
	got := reporter(fixture(t, "psi-stalled.txt"), fixture(t, "meminfo-healthy.txt")).Read()
	if len(got) != 1 {
		t.Fatalf("want a finding, got %v", got)
	}
	want := "memory stall 3.7% over the last 5 minutes (all tasks), 4.1% (some tasks); 1.6 GB free of 3.8 GB"
	if got[0].Details != want {
		t.Errorf("details:\n got %q\nwant %q", got[0].Details, want)
	}
}

// TestRead_FailOpen checks that each signal fails open on its own: a file that
// cannot be read or parsed never raises, and never hides the other signal.
func TestRead_FailOpen(t *testing.T) {
	readErr := func() (string, error) { return "", errors.New("no such file") }
	ok := func(s string) func() (string, error) { return func() (string, error) { return s, nil } }

	stalled := fixture(t, "psi-stalled.txt")
	low := fixture(t, "meminfo-low.txt")
	healthy := fixture(t, "meminfo-healthy.txt")

	cases := []struct {
		name        string
		psi, mem    func() (string, error)
		raise       bool
		wantDetails string // a substring the details must hold when raised
	}{
		{"both unreadable", readErr, readErr, false, ""},
		{"both unparseable", ok("garbage"), ok("garbage"), false, ""},
		{"PSI missing, memory healthy", readErr, ok(healthy), false, ""},
		{"PSI missing, memory low", readErr, ok(low), true, "150 MB free"},
		{"PSI garbage, memory low", ok("garbage"), ok(low), true, "150 MB free"},
		{"meminfo missing, PSI stalled", ok(stalled), readErr, true, "memory stall 3.7%"},
	}
	for _, c := range cases {
		r := &Reporter{readPSI: c.psi, readMeminfo: c.mem}
		got := r.Read()
		if !c.raise {
			if got != nil {
				t.Errorf("%s: want nil, got %v", c.name, got)
			}
			continue
		}
		if len(got) != 1 || !strings.Contains(got[0].Details, c.wantDetails) {
			t.Errorf("%s: want one finding with %q in details, got %v", c.name, c.wantDetails, got)
		}
	}
}
