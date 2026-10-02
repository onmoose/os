package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/onmoose/os/internal/hostagent"
	"github.com/onmoose/os/internal/hostagent/brainlaunch"
	"github.com/onmoose/os/internal/hostagent/cpupdate"
	"github.com/onmoose/os/internal/hostagent/osupdate"
	"github.com/onmoose/os/internal/hostagent/updatetarget"
	"github.com/onmoose/os/internal/version"
)

// Stream A's settings (#563). Not build-tagged, so `go vet` and `go test`
// see them; only the call that builds the applier is (osupdate_hosted.go).
const (
	// envOSURLPrefix is the expected start of every bundle URL. The default
	// is the GitHub Releases path of this repo (updatetarget.DefaultOSURLPrefix).
	// The boot lane points it at a server inside the guest.
	envOSURLPrefix = "MOOSE_UPDATE_OS_URL_PREFIX"
	// envOSTrialTimeout bounds a trial boot's wait for the brain, as a Go
	// duration. The default is osupdate.DefaultTrialTimeout.
	envOSTrialTimeout = "MOOSE_OS_TRIAL_TIMEOUT"
)

// osURLPrefix is the bundle URL prefix in force.
func osURLPrefix() string {
	if v := os.Getenv(envOSURLPrefix); v != "" {
		return v
	}
	return updatetarget.DefaultOSURLPrefix
}

// osTrialTimeout is the trial bound in force. An unreadable value warns and
// falls back: a wrong bound only makes a trial shorter or longer.
func osTrialTimeout() time.Duration {
	v := os.Getenv(envOSTrialTimeout)
	if v == "" {
		return osupdate.DefaultTrialTimeout
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		slog.Warn("os update: "+envOSTrialTimeout+" is not a positive duration; using the default", "err", err)
		return osupdate.DefaultTrialTimeout
	}
	// Under the image's 15 min timer, so a slow but healthy slot is decided
	// by host-agent and never reverted by the timer.
	if d > osupdate.MaxTrialTimeout {
		slog.Warn("os update: "+envOSTrialTimeout+" is longer than the image's trial timer allows; using the longest allowed", "err", fmt.Errorf("%s > %s", d, osupdate.MaxTrialTimeout))
		return osupdate.MaxTrialTimeout
	}
	return d
}

// osUpdateDeps is what the applier is built from.
type osUpdateDeps struct {
	agent    *hostagent.Agent
	brainCfg brainlaunch.Config
}

// agentJobs adapts the agent's job lock to the applier's Jobs seam, and maps
// the lock's refusal to osupdate.ErrBusy.
type agentJobs struct{ a *hostagent.Agent }

func (j agentJobs) StartJob(kind string, d time.Duration, fn func(ctx context.Context) error) (string, error) {
	job, err := j.a.StartJob(kind, d, fn)
	if hostagent.IsJobRunning(err) {
		return "", fmt.Errorf("%w: %v", osupdate.ErrBusy, err)
	}
	if err != nil {
		return "", err
	}
	return job.ID, nil
}

// newOSApplier builds the applier. The health check is the brain's /healthz on
// its container address, the same probe the control-plane update uses
// (cpupdate), retried until the trial's deadline: on a fresh boot the brain
// container may not have an address yet.
func newOSApplier(d osUpdateDeps) *osupdate.Applier {
	docker := cpupdate.NewCLIDocker()
	prober := cpupdate.HTTPProber{}
	healthy := func(ctx context.Context) error {
		var last error
		for {
			ip, err := docker.ContainerIP(ctx, d.brainCfg.ContainerName)
			if err == nil {
				if err = prober.WaitServing(ctx, fmt.Sprintf("http://%s:8080/healthz", ip)); err == nil {
					return nil
				}
			}
			last = err
			select {
			case <-ctx.Done():
				return errors.Join(ctx.Err(), last)
			case <-time.After(2 * time.Second):
			}
		}
	}
	return &osupdate.Applier{
		RAUC:         osupdate.CLIRAUC{},
		Jobs:         agentJobs{d.agent},
		Version:      version.Version,
		FloorFile:    filepath.Join(d.brainCfg.StateDir, "minimum-host-agent"),
		Healthy:      healthy,
		TrialTimeout: osTrialTimeout(),
	}
}
