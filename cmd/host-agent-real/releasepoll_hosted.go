//go:build hosted

package main

import "github.com/onmoose/moose/internal/hostagent/relmanifest"

// startReleasePoll does nothing on a hosted box.
//
// The release manifest is the **appliance** trigger: a signed broadcast that
// reaches machines we cannot otherwise address. A hosted box is not one of
// those — the cloud holds its target version per box and the box asks for it
// directly (UPDATES.md # 8.1, RELEASE_MANIFEST.md preamble). Polling a public
// manifest here would give a hosted box a second, competing opinion about which
// version it should run.
//
// Same name and shape as the appliance version so main.go has no build tags in
// it (see releasepoll_appliance.go). The nil poller is what the hosted
// update-target source ignores — it reads the cloud instead
// (updatetarget_hosted.go).
func startReleasePoll(string) (func(), *relmanifest.Poller) { return func() {}, nil }
