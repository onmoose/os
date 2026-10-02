//go:build hosted

package main

import "github.com/onmoose/os/internal/hostagent/osupdate"

// osUpdateApplier builds stream A's applier on the hosted build, the one
// profile whose image is in the A/B layout today (BUILD.md # 1b). The
// appliance gets it with #564.
func osUpdateApplier(r osUpdateDeps) *osupdate.Applier { return newOSApplier(r) }
