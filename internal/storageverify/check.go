// Package storageverify holds the canary + enrollment-marker check logic for
// the moose-storage-verify reporter (BOOT.md # The storage-ready target,
// STORAGE.md # Storage canary).
//
// Split out from cmd/ so the check is unit-testable against a tempdir-rooted
// filesystem. The cmd binary is a thin shell that writes the findings to
// /run/moose/health/storage.json.
package storageverify

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/onmoose/os/internal/protocol"
)

// Config makes the absolute paths injectable so tests can pass a tempdir as
// Root and the check resolves all paths under it.
type Config struct {
	Root                string // empty in prod; tests pass a tempdir
	MarkerPath          string // /etc/moose/data-drive.enrolled
	DataDriveCanaryPath string // /srv/moose/.canary
	BindMountCanaryPath string // /var/lib/moose/.canary
}

// marker is the on-disk schema of /etc/moose/data-drive.enrolled per
// STORAGE.md # Data drive enrollment marker.
type marker struct {
	UUID       string `json:"uuid"`
	EnrolledAt string `json:"enrolled_at"`
}

// Check returns the findings to ship in protocol.StorageHealth. The result is
// always a non-nil slice (empty = "storage looks healthy"); the caller does
// not need to nil-check.
//
// The check is the v1-slice subset of STORAGE.md # Storage canary:
//   - No marker present → empty findings (Level-0 boot, normal).
//   - Marker present, data-drive canary missing → data-drive-missing.
//   - Marker UUID does not match data-drive canary content → data-drive-wrong.
//   - Data-drive canary matches marker, bind-mount canary missing or differs
//     from data-drive canary → canary-mismatch (bind landed on the wrong
//     filesystem; the silent-orphan bug class STORAGE.md calls out).
//
// Deferred to a follow-up: the device-backing check via findmnt (STORAGE.md
// # Storage canary). Content match without device-backing means a stale
// canary on the OS drive plus a failed data-drive mount reads as healthy.
// Tracked in docs/progress/0019.
func Check(cfg Config) []protocol.Finding {
	findings := []protocol.Finding{}

	markerData, err := os.ReadFile(filepath.Join(cfg.Root, cfg.MarkerPath))
	if errors.Is(err, fs.ErrNotExist) {
		return findings // Level-0 boot, no data drive enrolled
	}
	if err != nil {
		return []protocol.Finding{{
			ID:      "health-report-malformed",
			Details: fmt.Sprintf("reading enrollment marker %s: %v", cfg.MarkerPath, err),
		}}
	}

	var m marker
	if err := json.Unmarshal(markerData, &m); err != nil {
		return []protocol.Finding{{
			ID:      "health-report-malformed",
			Details: fmt.Sprintf("parsing enrollment marker: %v", err),
		}}
	}
	if m.UUID == "" {
		return []protocol.Finding{{
			ID:      "health-report-malformed",
			Details: "enrollment marker has empty uuid",
		}}
	}

	dataCanary, err := readCanary(filepath.Join(cfg.Root, cfg.DataDriveCanaryPath))
	if errors.Is(err, fs.ErrNotExist) {
		findings = append(findings, protocol.Finding{
			ID:      "data-drive-missing",
			Details: fmt.Sprintf("enrolled %s but %s is absent", m.UUID, cfg.DataDriveCanaryPath),
		})
		return findings
	}
	if err != nil {
		findings = append(findings, protocol.Finding{
			ID:      "health-report-malformed",
			Details: fmt.Sprintf("reading data-drive canary: %v", err),
		})
		return findings
	}
	if dataCanary != m.UUID {
		findings = append(findings, protocol.Finding{
			ID:      "data-drive-wrong",
			Details: fmt.Sprintf("enrolled %s, but data-drive canary is %s", m.UUID, dataCanary),
		})
		return findings
	}

	bindCanary, err := readCanary(filepath.Join(cfg.Root, cfg.BindMountCanaryPath))
	if errors.Is(err, fs.ErrNotExist) || (err == nil && bindCanary != dataCanary) {
		findings = append(findings, protocol.Finding{
			ID: "canary-mismatch",
			Details: fmt.Sprintf(
				"data-drive canary %s present but bind-mount view %s is missing or differs (bind likely landed on wrong filesystem)",
				cfg.DataDriveCanaryPath, cfg.BindMountCanaryPath,
			),
		})
		return findings
	}
	if err != nil {
		findings = append(findings, protocol.Finding{
			ID:      "health-report-malformed",
			Details: fmt.Sprintf("reading bind-mount canary: %v", err),
		})
		return findings
	}

	return findings
}

func readCanary(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
