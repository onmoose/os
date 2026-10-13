package lifecycle

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/onmoose/os/internal/fit"
	"github.com/onmoose/os/internal/manifest"
)

// FitBox gathers what fit.Check needs about this box (#599): MemTotal, the
// memory in use and the core count from the host-agent, and the recommended
// memory of every running instance from the manifest the installer saved for
// it. A stopped instance counts for nothing.
//
// ok is false when the host-agent cannot report memory. The caller then has no
// fit to show and no install to refuse: the check is advice about this box, so
// a box that cannot measure itself is not a reason to block an install. A
// running instance whose manifest cannot be read is logged and counted as
// recommending nothing.
func (m *Manager) FitBox(ctx context.Context) (fit.Box, bool) {
	res, err := m.host.SystemResources(ctx)
	if err != nil {
		slog.Warn("app fit: host resources unavailable", "err", err)
		return fit.Box{}, false
	}
	if res.Mem.TotalBytes <= 0 {
		return fit.Box{}, false
	}
	box := fit.Box{
		MemTotalBytes: res.Mem.TotalBytes,
		MemUsedBytes:  res.Mem.UsedBytes,
		Cores:         res.CPU.Cores,
	}
	insts, err := m.store.List()
	if err != nil {
		slog.Warn("app fit: list instances", "err", err)
		return box, true
	}
	for _, inst := range insts {
		if inst.State != "running" {
			continue
		}
		man, err := m.loadInstanceManifest(inst.ID)
		if err != nil {
			slog.Warn("app fit: read instance manifest", "instance_id", inst.ID, "err", err)
			continue
		}
		mem, err := man.Resources.Recommended.MemoryBytes()
		if err != nil {
			slog.Warn("app fit: unparseable recommended memory", "instance_id", inst.ID, "err", err)
			continue
		}
		box.Running = append(box.Running, fit.RunningApp{InstanceID: inst.ID, Name: inst.Name, RecommendedMemoryBytes: mem})
	}
	return box, true
}

// CheckFit is fit.Check for one app's recommended resources. A memory figure
// that does not parse counts as no recommendation, and is logged: the catalog's
// copy of the block is read without manifest.Parse, so it is the one place a
// malformed value can reach.
func CheckFit(box fit.Box, manifestID string, rec manifest.Recommended) fit.AppFit {
	mem, err := rec.MemoryBytes()
	if err != nil {
		slog.Warn("app fit: unparseable recommended memory", "manifest_id", manifestID, "err", err)
		mem = 0
	}
	return fit.Check(box, mem, rec.CPU)
}

// TooBigError is the install refusal for an app that recommends more memory
// than the box has, or more CPU cores (#599, APP_ISOLATION.md # Resource
// limits). Like ErrNoGPU it is raised before the instance row or any Docker
// work exists, so there is nothing to roll back, and its text is the failed
// install job's message.
type TooBigError struct {
	Fit fit.AppFit
}

func (e *TooBigError) Error() string {
	msg := "This box can't run this app as it is."
	if e.Fit.TooBigMemory {
		msg += fmt.Sprintf(" It recommends %s of memory, and this box has %s.", gbText(e.Fit.RecommendedMemoryBytes), gbText(e.Fit.BoxMemoryBytes))
	}
	if e.Fit.TooBigCPU {
		msg += fmt.Sprintf(" It recommends %s CPU cores, and this box has %d.", coresText(e.Fit.RecommendedCPU), e.Fit.BoxCores)
	}
	return msg
}

// checkInstallFit is the install-time backstop for the store page's fit: a
// too-big app is refused. A tight app is not: the dialog has already asked for
// an explicit "Install anyway".
func (m *Manager) checkInstallFit(ctx context.Context, man *manifest.Manifest) error {
	box, ok := m.FitBox(ctx)
	if !ok {
		return nil
	}
	if res := CheckFit(box, man.ID, man.Resources.Recommended); res.State == fit.TooBig {
		return &TooBigError{Fit: res}
	}
	return nil
}

// gbText writes a byte count in binary GB with one decimal, the unit the store
// page uses ("3.8 GB").
func gbText(n int64) string {
	return fmt.Sprintf("%.1f GB", float64(n)/float64(int64(1)<<30))
}

// coresText writes a core count without a trailing ".0".
func coresText(c float64) string {
	return fmt.Sprintf("%g", c)
}
