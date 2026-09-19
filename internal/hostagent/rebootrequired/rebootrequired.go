// Package rebootrequired is host-agent's locus-B reboot-required detector
// (HEALTH.md # Detector catalog, the reboot-required row). Debian's
// update-notifier-common writes /var/run/reboot-required when a kernel- or
// libc-class package upgrade (via apt / unattended-upgrades) needs a reboot to
// take effect, and lists the responsible packages one-per-line in
// /var/run/reboot-required.pkgs. This reporter stats the flag and, when present,
// emits a single box-wide `reboot-required` finding carrying the package list in
// Details so the UI can say *why* ("kernel, libc6"). The brain reconciles it
// under the report's `system` category.
//
// Why host-agent owns it (DECISIONS.md 2026-05-31): the brain runs containerized
// and can't read the host's /run; the flag is Debian's, set by apt regardless of
// moose's own app-update path — so host-agent reading the file is the only
// coherent locus. It self-clears on reboot (/run is tmpfs): the file vanishes and
// the next poll clears the issue.
//
// No cache (unlike the clock detector): a stat is far too cheap to rate-limit, so
// every Read re-stats. HEALTH.md's 1h cadence is a relaxed floor — checking on the
// brain's 60s poll is strictly better (a pending reboot, and its clear after a
// reboot, surface within one poll rather than up to an hour). The flag can't flap
// within a boot (apt creates it once; only a reboot removes it), so the brain
// registers this 1-shot (no debounce) — see internal/health.
package rebootrequired

import (
	"log/slog"
	"os"
	"strings"

	"github.com/onmoose/os/internal/protocol"
)

// issueRebootRequired is the registered issue ID raised while a reboot is pending.
const issueRebootRequired = "reboot-required"

// Debian's update-notifier-common paths. /var/run is the conventional symlink to
// /run (tmpfs), so the flag self-clears on reboot.
const (
	defaultFlagPath = "/var/run/reboot-required"
	defaultPkgsPath = "/var/run/reboot-required.pkgs"
)

// Reporter implements hostagent.RebootReporter. The paths are fields so tests can
// point them at a temp dir and exercise the real os.Stat / os.ReadFile path.
type Reporter struct {
	flagPath string
	pkgsPath string
}

// New returns a Reporter backed by Debian's /var/run/reboot-required flag.
func New() *Reporter {
	return &Reporter{flagPath: defaultFlagPath, pkgsPath: defaultPkgsPath}
}

// Read returns the reboot-required finding when the flag file exists, or nil when
// it doesn't (no reboot pending). It always returns a usable slice — a pending
// reboot is data, not an error. An unexpected stat error (not "does not exist")
// fails open to nil and is logged: a tooling glitch shouldn't manufacture a
// reboot banner.
func (r *Reporter) Read() []protocol.Finding {
	if _, err := os.Stat(r.flagPath); err != nil {
		if !os.IsNotExist(err) {
			slog.Error("rebootrequired: stat of reboot flag failed", "err", err)
		}
		return nil
	}
	return []protocol.Finding{{
		ID:      issueRebootRequired,
		Details: r.packageReason(),
	}}
}

// packageReason reads the .pkgs list and renders it as a short, plain detail
// ("kernel, libc6") for the finding. The flag alone is enough to raise — a
// missing or unreadable .pkgs just yields an empty detail, never suppresses the
// finding.
func (r *Reporter) packageReason() string {
	b, err := os.ReadFile(r.pkgsPath)
	if err != nil {
		return ""
	}
	return strings.Join(parsePackages(string(b)), ", ")
}

// parsePackages splits the one-package-per-line .pkgs file into a trimmed,
// de-duplicated, order-preserving list. update-notifier appends to the file
// across successive upgrades, so the same package can appear more than once.
func parsePackages(out string) []string {
	var pkgs []string
	seen := map[string]bool{}
	for _, line := range strings.Split(out, "\n") {
		p := strings.TrimSpace(line)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		pkgs = append(pkgs, p)
	}
	return pkgs
}
