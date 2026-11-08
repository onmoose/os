// Package fit says whether an app fits the box's memory and CPU, from the
// resources its manifest recommends (APP_MANIFEST.md # B2, APP_ISOLATION.md #
// Resource limits). It is pure arithmetic over figures the caller gathers: the
// box's memory and cores from the host-agent, and the recommended memory of the
// apps that are running. The brain computes one AppFit and returns it on the
// app's store page and in the install plan, so the page and the install
// refusal never disagree.
package fit

import (
	"cmp"
	"math"
	"slices"
)

// The three states, as the API sends them.
const (
	// Fits: the app's recommendation fits next to what is already running, or
	// it recommends nothing.
	Fits = "fits"
	// Tight: the box has enough memory in total, but not next to the apps that
	// are running. The install needs an explicit "Install anyway".
	Tight = "tight"
	// TooBig: the app recommends more memory than the box has in total, or more
	// CPU cores than the box has. The install is refused.
	TooBig = "too-big"
)

// gib is one binary gigabyte.
const gib = int64(1) << 30

// Box is what the caller knows about the box when it checks a fit.
type Box struct {
	// MemTotalBytes is MemTotal from /proc/meminfo.
	MemTotalBytes int64
	// MemUsedBytes is the memory actually in use: MemTotal minus MemAvailable.
	MemUsedBytes int64
	// Cores is the number of online CPUs. 0 means unknown (a host-agent older
	// than the field), and the CPU check is skipped.
	Cores int
	// Running is every running app instance, with the memory its manifest
	// recommends (0 for none). A stopped app counts for nothing.
	Running []RunningApp
}

// RunningApp is one running app instance and the memory its manifest recommends.
type RunningApp struct {
	InstanceID             string `json:"instance_id"`
	Name                   string `json:"name"`
	RecommendedMemoryBytes int64  `json:"recommended_memory_bytes"`
}

// AppFit is the fit of one app on this box.
type AppFit struct {
	State string `json:"state" enum:"fits,tight,too-big"`
	// RecommendedMemoryBytes and RecommendedCPU are what the app recommends.
	// 0 means it recommends nothing for that resource.
	RecommendedMemoryBytes int64   `json:"recommended_memory_bytes,omitempty"`
	RecommendedCPU         float64 `json:"recommended_cpu,omitempty"`
	// BoxMemoryBytes and BoxCores are the box's totals: MemTotal, and the
	// number of online CPUs (0 when the host-agent does not report it).
	BoxMemoryBytes int64 `json:"box_memory_bytes"`
	BoxCores       int   `json:"box_cores,omitempty"`
	// FreeMemoryBytes is the headroom the tight check used: MemTotal minus the
	// larger of the running apps' recommendations and the memory in use.
	FreeMemoryBytes int64 `json:"free_memory_bytes"`
	// TooBigMemory and TooBigCPU say which resource made the app too big. Both
	// can be true.
	TooBigMemory bool `json:"too_big_memory,omitempty"`
	TooBigCPU    bool `json:"too_big_cpu,omitempty"`
	// Running is the apps that are running and recommend memory, largest first.
	// Sent only when the app is tight, so the page can name what to stop.
	Running []RunningApp `json:"running,omitempty"`
	// OtherRunning counts the running apps that recommend memory but are not
	// in Running because the caller may not see them (another user's
	// personal app). They are still in the headroom. Set by the API, never by
	// Check.
	OtherRunning int `json:"other_running,omitempty"`
}

// Check computes the fit of an app that recommends memBytes of memory and cpu
// cores. The rules, from #599:
//
//   - Too big: memBytes is more than the box's memory, rounded up to a whole
//     GiB, or cpu is more than the box's cores. The rounding is there because a
//     VM sold as "4 GB" reports about 3.8 GiB of MemTotal, as the kernel keeps
//     some, and an app that recommends 4G should warn on it, not be refused.
//   - Tight: memBytes is more than the headroom, which is MemTotal minus the
//     larger of (the sum of the running apps' recommendations) and (the memory
//     in use). The larger of the two keeps one app's understated manifest from
//     hiding a full box. CPU is time-shared and never capped, so it is never
//     tight.
//   - Fits: otherwise, and always when the app recommends nothing.
func Check(box Box, memBytes int64, cpu float64) AppFit {
	var reserved int64
	for _, r := range box.Running {
		reserved += r.RecommendedMemoryBytes
	}
	reserved = max(reserved, box.MemUsedBytes)
	res := AppFit{
		State:                  Fits,
		RecommendedMemoryBytes: memBytes,
		RecommendedCPU:         cpu,
		BoxMemoryBytes:         box.MemTotalBytes,
		BoxCores:               box.Cores,
		FreeMemoryBytes:        max(box.MemTotalBytes-reserved, 0),
	}
	if memBytes > 0 && memBytes > roundUpGiB(box.MemTotalBytes) {
		res.TooBigMemory = true
	}
	if cpu > 0 && box.Cores > 0 && cpu > float64(box.Cores) {
		res.TooBigCPU = true
	}
	switch {
	case res.TooBigMemory || res.TooBigCPU:
		res.State = TooBig
	case memBytes > res.FreeMemoryBytes:
		res.State = Tight
		res.Running = largestFirst(box.Running)
	}
	return res
}

// roundUpGiB rounds a byte count up to the next whole GiB.
func roundUpGiB(n int64) int64 {
	return int64(math.Ceil(float64(n)/float64(gib))) * gib
}

// largestFirst returns the running apps that recommend memory, largest first,
// in a new slice.
func largestFirst(in []RunningApp) []RunningApp {
	var out []RunningApp
	for _, r := range in {
		if r.RecommendedMemoryBytes > 0 {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b RunningApp) int {
		return cmp.Compare(b.RecommendedMemoryBytes, a.RecommendedMemoryBytes)
	})
	return out
}
