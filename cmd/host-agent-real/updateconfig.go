package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"github.com/onmoose/moose/internal/hostagent/updatetarget"
	"github.com/onmoose/moose/internal/profile"
)

// This file resolves the two per-box update settings: which update target this
// box reads, and the window it applies in.
//
// The target URL comes from the provisioning seed first:
//
//	seed  >  environment variable  >  built-in default
//
// The seed is the **only channel a real box has** for a per-box fact. It arrives
// as metadata user-data and is written once, when the VM is created
// (ENVIRONMENT.md # Provisioning & first-boot). "Which control plane does this
// box belong to" is fixed for the life of the box, so the seed is the right home
// for it. A hosted box has no SSH (ENVIRONMENT.md # Access & files), so without
// this a provisioned box could never be pointed at a candidate release. The
// environment variable stays below it as the local hand-edit: an operator
// drop-in, or the CI boot proof pointing the loop at a server inside the guest.
//
// The window has no seed source. It has to be changeable while the box runs, and
// the seed can never be rewritten, so the control plane's answer is its home
// (UPDATES.md # 8.1). What is resolved here is only the fallback under it:
//
//	answer  >  environment variable  >  built-in default
//
// The answer is read by the loop, not here, because it arrives on every poll
// rather than once at startup (internal/hostagent/updatetarget, Loop.windowFor).
//
// **Nothing here is build-tagged, on purpose.** The target URL only matters on
// the hosted build, but `go vet ./...` and `go test ./...` both run untagged, so
// anything behind `//go:build hosted` is neither vetted nor tested by CI. The tag
// stays on the call site (updatetarget_hosted.go); the logic lives here.
const (
	envSeedPath        = "MOOSE_SEED_PATH"
	envUpdateTargetURL = "MOOSE_UPDATE_TARGET_URL"
	envUpdateWindow    = "MOOSE_UPDATE_WINDOW"
)

// Which source won, for the startup log.
const (
	fromSeed    = "seed"
	fromEnv     = "env"
	fromDefault = "default"
)

// seedPath is where this box's provisioning seed lands. The override matches the
// brain's (cmd/brain/main.go), so a test or a boot proof points both sides at one
// file with one variable.
func seedPath() string {
	if v := os.Getenv(envSeedPath); v != "" {
		return v
	}
	return profile.DefaultSeedPath
}

// seedUpdateFacts reads the two per-box update facts out of the provisioning
// seed: the endpoint this box asks, and the identity it says it is when it asks.
//
// **One read for both.** They live in one file, so two reads could see two
// different files and pair a box id with a target that was never meant for it.
// An empty target means the seed named none and the caller should look further
// down the chain. An empty box id means this box has no identity to send, which
// is every appliance box and any hosted box seeded without one.
//
// It reads the file itself instead of calling profile.ReadSeed. That reader is
// the brain's: it hard-errors on a seed with no box_id and no
// assertion-verification key, which is right for the brain and wrong here.
// host-agent wants one optional field, so a box seeded only for updates, and a
// box with no seed at all, must both work. The struct is still profile.Seed, so
// there is one definition of the wire shape.
//
// Three seed states, three answers:
//
//   - **Absent.** The appliance case, and a hosted box provisioned without a
//     seed. Not an error. Fall through, and say so once.
//   - **Present and readable.** Use update_target_url when it is set. An empty
//     field carries no instruction, so it falls through like an absent one.
//   - **Present and malformed.** An error. Bytes we cannot parse might have
//     carried a target and we cannot tell, so reading the fleet endpoint instead
//     could move a box that was meant to be pinned onto stable. Same call, and
//     the same reason, as an unusable URL below.
func seedUpdateFacts() (target, boxID string, err error) {
	path := seedPath()
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		slog.Info("no provisioning seed; taking the update target from the environment or the default", "src", path)
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("read seed %s: %w", path, err)
	}
	var s profile.Seed
	if err := json.Unmarshal(b, &s); err != nil {
		return "", "", fmt.Errorf("parse seed %s: %w", path, err)
	}
	// Trimmed: the seed is text written by a provisioner, and stray whitespace is
	// the most likely way to hand over a URL that then does not parse.
	target = strings.TrimSpace(s.UpdateTargetURL)
	boxID = strings.TrimSpace(s.BoxID)
	if target == "" {
		slog.Info("the provisioning seed names no update target; taking it from the environment or the default", "src", path)
	}
	return target, boxID, nil
}

// updateTarget resolves what this box needs to ask its source: the endpoint, and
// the box id it sends with the question. An empty target means
// updatetarget.DefaultURL, and an empty box id means the box asks anonymously.
//
// The box id comes from the seed whatever the target's source is. Identity and
// endpoint are separate facts: a box whose URL is a local hand-edit is still the
// same box, so it still says who it is.
//
// **A seeded target that is unusable is an error, never a fallback.** A box
// carrying it was deliberately pointed somewhere by whoever provisioned it;
// silently reading the fleet default instead would move a box that is meant to be
// pinned to a candidate straight onto stable, which is the one outcome pinning
// exists to prevent. The caller refuses rather than updates.
//
// Only the seeded value is validated. The environment variable is the operator's
// own hand-edit on a box they can already reach, and it has never been checked;
// tightening it is a separate change, not a side effect of this one.
func updateTarget() (target, from, boxID string, err error) {
	raw, boxID, err := seedUpdateFacts()
	if err != nil {
		return "", "", "", err
	}
	if raw != "" {
		if err := checkTargetURL(raw); err != nil {
			return "", "", "", err
		}
		return raw, fromSeed, boxID, nil
	}
	if v := os.Getenv(envUpdateTargetURL); v != "" {
		return v, fromEnv, boxID, nil
	}
	return "", fromDefault, boxID, nil
}

// checkTargetURL rejects anything the box could not actually fetch from. Its
// messages name the offending value through updatetarget.RedactURL, because
// this error becomes the `detail` of the disabled state on the socket read
// (#443) and a seeded URL may carry credentials.
//
// checkTargetURL rejects anything the box could not actually fetch from. It is
// deliberately shallow (an absolute http or https URL with a host) because the point
// is to catch a provisioning mistake (a hostname with no scheme, a pasted shell
// fragment), not to police where a box may be pointed.
func checkTargetURL(s string) error {
	u, err := url.Parse(s)
	if err != nil {
		// The parser's own error carries the raw URL, password and all, and
		// this message becomes the disabled state's `detail` on the socket read
		// (#443). Name the redacted URL and keep only the reason.
		return fmt.Errorf("seed update_target_url is not a URL (%s): %w",
			updatetarget.RedactURL(s), updatetarget.CauseOf(err))
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("seed update_target_url must be an http or https URL, got %q", updatetarget.RedactURL(s))
	}
	if u.Host == "" {
		return fmt.Errorf("seed update_target_url has no host: %q", updatetarget.RedactURL(s))
	}
	return nil
}

// updateWindow resolves the window an update may start in, unless the source's
// answer names one of its own. It is the box's local setting, and the answer
// sits above it.
//
// Unlike the target URL, an unreadable window falls back with a warning. The two
// are not symmetric: a wrong window can only apply an update at the wrong hour,
// while a wrong target sends the box to the wrong version. Losing a box's
// updates over a mistyped clock reading would be the worse trade.
func updateWindow() (updatetarget.Window, string) {
	raw, from := windowSetting()
	w, err := updatetarget.ParseWindow(raw)
	if err != nil {
		// A box with a mistyped window must not lose its updates, and must not
		// silently use a window nobody chose either: say so and take the default.
		slog.Warn("update window is not readable; using the default", "err", err, "from", from)
		return updatetarget.DefaultWindow, fromDefault
	}
	return w, from
}

// windowSetting returns the raw window setting and where it came from. An empty
// variable counts as absent rather than as "use the default": it carries no
// instruction.
func windowSetting() (raw, from string) {
	if v := os.Getenv(envUpdateWindow); v != "" {
		return v, fromEnv
	}
	return "", fromDefault
}
