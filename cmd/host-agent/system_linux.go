//go:build linux

package main

import (
	"github.com/onmoose/moose/internal/hostagent"
	"github.com/onmoose/moose/internal/hostagent/procsource"
)

// newSystemSampler returns a real /proc-backed sampler on Linux so make dev
// shows live CPU and RAM rather than synthetic counters.
func newSystemSampler() hostagent.SystemSampler {
	return procsource.New()
}
