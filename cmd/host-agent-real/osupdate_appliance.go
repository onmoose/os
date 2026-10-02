//go:build !hosted

package main

import "github.com/onmoose/os/internal/hostagent/osupdate"

// osUpdateApplier is nil on the appliance build: its image is not in the A/B
// layout yet (#564), so there is no other slot to install into. The update
// loop then reports stream A as unsupported.
func osUpdateApplier(osUpdateDeps) *osupdate.Applier { return nil }
