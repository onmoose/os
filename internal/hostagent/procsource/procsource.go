//go:build linux

// Package procsource is host-agent's real /proc sampler behind GET
// /v1/system/resources — the leaf of the live system-resources chain
// (LOCAL_ANALYTICS.md # Real-time system resources). It reads six kernel files
// (/proc/stat, /proc/meminfo, /proc/loadavg, /proc/net/dev, /proc/diskstats,
// /proc/uptime) into one protocol.SystemResources of **raw cumulative
// counters**; all rate derivation stays in the brain (BRAIN_HOST_PROTOCOL.md
// # Pattern A), so the sampler is stateless — it reads on request and holds
// nothing.
//
// Two selection rules are applied here so the brain never sees container
// noise (LOCAL_ANALYTICS.md # Interface and device selection):
//
//   - Network: every interface except lo and the container-side virtual ones
//     (docker*, br-*, veth*). Physical LAN NICs and the future mesh interface
//     pass through — the spec reports "physical LAN NICs + the mesh interface
//     only", and summing veth* would double-count container traffic already
//     counted on the bridge side.
//   - Disk: whole-disk block devices only (sd*/hd*/vd*/xvd* letters-only,
//     nvme<n>n<n>, mmcblk<n>) — a partition would double-count its parent
//     disk, and loop/dm/md/zram devices are views over the same physical IO.
//
// Jiffy accounting: TotalJiffies sums user..steal, the first eight fields of
// the aggregate /proc/stat cpu line. guest and guest_nice are excluded because
// the kernel already accounts guest time inside user/nice — summing all ten
// would double-count it and understate CPU%. IdleJiffies is idle + iowait
// (iowait is "CPU had nothing to run", the same as idle from the gauge's
// point of view).
package procsource

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/onmoose/moose/internal/protocol"
)

// Sampler reads one protocol.SystemResources from the kernel files under root.
// root is "/" in production; tests point it at a fixture tree so they never
// touch the real /proc.
type Sampler struct {
	root string
}

// New returns a Sampler reading the live kernel files under /.
func New() *Sampler { return &Sampler{root: "/"} }

// Sample reads all six kernel files into one raw-counter snapshot. Any read or
// parse failure fails the whole sample — the brain logs and skips the tick,
// keeping its previous baseline, so a partial snapshot is worth less than none.
func (s *Sampler) Sample() (protocol.SystemResources, error) {
	var res protocol.SystemResources

	stat, err := s.read("proc/stat")
	if err != nil {
		return res, err
	}
	if res.CPU, err = parseCPU(stat); err != nil {
		return res, err
	}

	meminfo, err := s.read("proc/meminfo")
	if err != nil {
		return res, err
	}
	if res.Mem, err = parseMeminfo(meminfo); err != nil {
		return res, err
	}

	loadavg, err := s.read("proc/loadavg")
	if err != nil {
		return res, err
	}
	if res.LoadAvg, err = parseLoadavg(loadavg); err != nil {
		return res, err
	}

	netdev, err := s.read("proc/net/dev")
	if err != nil {
		return res, err
	}
	if res.Net, err = parseNetDev(netdev); err != nil {
		return res, err
	}

	diskstats, err := s.read("proc/diskstats")
	if err != nil {
		return res, err
	}
	if res.Disk, err = parseDiskstats(diskstats); err != nil {
		return res, err
	}

	uptime, err := s.read("proc/uptime")
	if err != nil {
		return res, err
	}
	if res.UptimeS, err = parseUptime(uptime); err != nil {
		return res, err
	}

	res.TsNs = time.Now().UnixNano()
	return res, nil
}

func (s *Sampler) read(rel string) (string, error) {
	data, err := os.ReadFile(filepath.Join(s.root, rel))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", rel, err)
	}
	return string(data), nil
}

// parseCPU reads the aggregate `cpu` line of /proc/stat into cumulative jiffy
// counters: user nice system idle iowait irq softirq steal [guest guest_nice].
// Total sums the first eight (see the package doc for why guest is excluded);
// older kernels emit fewer fields, in which case whatever is present is summed.
func parseCPU(stat string) (protocol.CPUCounters, error) {
	for _, line := range strings.Split(stat, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 || fields[0] != "cpu" {
			continue
		}
		var c protocol.CPUCounters
		for i, f := range fields[1:] {
			if i >= 8 {
				break
			}
			v, err := strconv.ParseInt(f, 10, 64)
			if err != nil {
				return protocol.CPUCounters{}, fmt.Errorf("proc/stat cpu field %d: %w", i, err)
			}
			c.TotalJiffies += v
			if i == 3 || i == 4 { // idle + iowait
				c.IdleJiffies += v
			}
		}
		return c, nil
	}
	return protocol.CPUCounters{}, fmt.Errorf("proc/stat has no aggregate cpu line")
}

// parseMeminfo reads MemTotal and MemAvailable (kB → bytes). Used is
// Total − Available — the honest "in use" figure that already credits
// reclaimable cache back, matching the synthetic handler's field meaning.
func parseMeminfo(meminfo string) (protocol.MemCounters, error) {
	var m protocol.MemCounters
	var haveTotal, haveAvail bool
	for _, line := range strings.Split(meminfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		switch fields[0] {
		case "MemTotal:", "MemAvailable:":
			v, err := strconv.ParseInt(fields[1], 10, 64)
			if err != nil {
				return protocol.MemCounters{}, fmt.Errorf("proc/meminfo %s: %w", fields[0], err)
			}
			if fields[0] == "MemTotal:" {
				m.TotalBytes = v * 1024
				haveTotal = true
			} else {
				m.AvailableBytes = v * 1024
				haveAvail = true
			}
		}
	}
	if !haveTotal || !haveAvail {
		return protocol.MemCounters{}, fmt.Errorf("proc/meminfo missing MemTotal or MemAvailable")
	}
	m.UsedBytes = m.TotalBytes - m.AvailableBytes
	return m, nil
}

// parseLoadavg reads the three load averages from /proc/loadavg.
func parseLoadavg(loadavg string) ([3]float64, error) {
	var l [3]float64
	fields := strings.Fields(loadavg)
	if len(fields) < 3 {
		return l, fmt.Errorf("proc/loadavg has %d fields, want ≥3", len(fields))
	}
	for i := 0; i < 3; i++ {
		v, err := strconv.ParseFloat(fields[i], 64)
		if err != nil {
			return l, fmt.Errorf("proc/loadavg field %d: %w", i, err)
		}
		l[i] = v
	}
	return l, nil
}

// includeIface is the network allowlist (see the package doc): everything
// except lo and the container-side virtual interfaces.
func includeIface(name string) bool {
	if name == "lo" {
		return false
	}
	for _, prefix := range []string{"docker", "br-", "veth"} {
		if strings.HasPrefix(name, prefix) {
			return false
		}
	}
	return true
}

// parseNetDev reads per-interface cumulative rx/tx byte counters from
// /proc/net/dev (rx bytes is the first value after the interface colon, tx
// bytes the ninth), allowlist-filtered and sorted by interface name for a
// stable wire order.
func parseNetDev(netdev string) ([]protocol.NetCounters, error) {
	var out []protocol.NetCounters
	for _, line := range strings.Split(netdev, "\n") {
		name, rest, ok := strings.Cut(line, ":")
		if !ok {
			continue // the two header lines have no interface colon
		}
		name = strings.TrimSpace(name)
		if name == "" || strings.Contains(name, " ") {
			continue // "Inter-|   Receive ..." header also carries a '|', not a real iface
		}
		if !includeIface(name) {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) < 9 {
			return nil, fmt.Errorf("proc/net/dev %s: %d fields, want ≥9", name, len(fields))
		}
		rx, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("proc/net/dev %s rx: %w", name, err)
		}
		tx, err := strconv.ParseInt(fields[8], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("proc/net/dev %s tx: %w", name, err)
		}
		out = append(out, protocol.NetCounters{Iface: name, RxBytes: rx, TxBytes: tx})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Iface < out[j].Iface })
	return out, nil
}

// wholeDisk matches the whole-disk block-device names moose boxes carry (BYO
// x86: SATA/USB sd*, virtio vd*, Xen xvd*, legacy IDE hd*, NVMe, eMMC/SD).
// Partition names (sda1, nvme0n1p2, mmcblk0p1) and loop/dm/md/zram/sr views
// deliberately don't match.
var wholeDisk = regexp.MustCompile(`^(sd[a-z]+|hd[a-z]+|vd[a-z]+|xvd[a-z]+|nvme[0-9]+n[0-9]+|mmcblk[0-9]+)$`)

// parseDiskstats reads cumulative read/write byte counters from
// /proc/diskstats for whole-disk devices only: field 3 is the device name,
// fields 6 and 10 are sectors read/written; the kernel's sector unit here is a
// fixed 512 bytes regardless of the device's real sector size. Sorted by
// device name for a stable wire order.
func parseDiskstats(diskstats string) ([]protocol.DiskCounters, error) {
	var out []protocol.DiskCounters
	for _, line := range strings.Split(diskstats, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		dev := fields[2]
		if !wholeDisk.MatchString(dev) {
			continue
		}
		sectorsRead, err := strconv.ParseInt(fields[5], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("proc/diskstats %s sectors read: %w", dev, err)
		}
		sectorsWritten, err := strconv.ParseInt(fields[9], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("proc/diskstats %s sectors written: %w", dev, err)
		}
		out = append(out, protocol.DiskCounters{
			Dev:        dev,
			ReadBytes:  sectorsRead * 512,
			WriteBytes: sectorsWritten * 512,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dev < out[j].Dev })
	return out, nil
}

// parseUptime reads whole seconds since boot from the first /proc/uptime field.
func parseUptime(uptime string) (int64, error) {
	fields := strings.Fields(uptime)
	if len(fields) < 1 {
		return 0, fmt.Errorf("proc/uptime is empty")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		return 0, fmt.Errorf("proc/uptime: %w", err)
	}
	return int64(v), nil
}
