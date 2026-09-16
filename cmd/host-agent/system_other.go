//go:build !linux

package main

import "github.com/onmoose/moose/internal/hostagent"

// newSystemSampler returns nil on non-Linux platforms; the agent falls back to
// synthetic monotonic counters so the dev loop still produces plausible rates.
func newSystemSampler() hostagent.SystemSampler {
	return nil
}
