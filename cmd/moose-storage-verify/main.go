// Command moose-storage-verify is the boot-time storage reporter
// (BOOT.md # The storage-ready target). It checks the enrollment marker, the
// canary file on the data drive, and the canary visible through the
// /var/lib/moose bind mount, and writes its findings to
// /run/moose/health/storage.json.
//
// **It is a reporter, not a gate.** Every exit is 0, even when findings are
// raised — `moose-storage-verify.service` carries no OnFailure routing and
// must never trip the systemd-level recovery target. Findings become typed
// health issues the brain raises in degraded mode (HEALTH.md # Storage).
package main

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/onmoose/moose/internal/protocol"
	"github.com/onmoose/moose/internal/storageverify"
)

func main() {
	root := os.Getenv("MOOSE_VERIFY_ROOT") // tests inject a tempdir; prod = ""
	outPath := os.Getenv("MOOSE_VERIFY_OUT")
	if outPath == "" {
		outPath = "/run/moose/health/storage.json"
	}

	findings := storageverify.Check(storageverify.Config{
		Root:                root,
		MarkerPath:          "/etc/moose/data-drive.enrolled",
		DataDriveCanaryPath: "/srv/moose/.canary",
		BindMountCanaryPath: "/var/lib/moose/.canary",
	})

	payload := protocol.StorageHealth{
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
		Findings:  findings,
	}
	if err := writeAtomic(outPath, payload); err != nil {
		slog.Error("storage-verify: write output failed", "out", outPath, "err", err)
		// Still exit 0 — host-agent's FilesystemHealthSource will report
		// the file as missing (treated as "healthy"), which is wrong but
		// silent failure of the reporter is captured in the journal here.
		// The alternative (exit non-zero) would activate OnFailure routing
		// that BOOT.md explicitly forbids for this unit.
	} else {
		slog.Info("storage-verify: report written", "out", outPath, "findings", len(findings))
	}
}

// writeAtomic writes payload as JSON to path via a temp-file + rename so
// host-agent never reads a half-written file. Standard atomic-write pattern:
// a deferred Remove cleans up the temp on any error path (including a
// Rename failure), and the success path nils the temp name so the deferred
// cleanup is a no-op.
func writeAtomic(path string, payload protocol.StorageHealth) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil && !errors.Is(err, fs.ErrExist) {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".storage.json.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(payload); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	tmpName = "" // success — defer becomes no-op
	return nil
}
