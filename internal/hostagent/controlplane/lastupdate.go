package controlplane

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/onmoose/os/internal/protocol"
)

// lastUpdateFile holds the outcome of the last control-plane update, beside the
// ledger. The job record lives in host-agent's memory and the brain that
// started the update is replaced by it, so this file is what lets the dashboard
// say "Updated to X" or "the update failed" after the brain comes back (#488).
const lastUpdateFile = "last-update.json"

// LastUpdatePath is the record's path inside the staged control-plane
// directory.
func LastUpdatePath(dir string) string { return filepath.Join(dir, lastUpdateFile) }

// LastUpdate stores the last control-plane update outcome in Dir
// (MOOSE_CONTROL_PLANE_DIR). Scheduled and manual updates write the same
// record, because both run through host-agent's one update job.
type LastUpdate struct {
	Dir string
}

// Save writes the outcome atomically. Only the last one is kept: the screen
// shows the last result, and the journal keeps the history.
func (s LastUpdate) Save(o protocol.ControlPlaneOutcome) error {
	b, err := json.MarshalIndent(o, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal last control-plane update: %w", err)
	}
	return writeFileAtomic(LastUpdatePath(s.Dir), append(b, '\n'), 0o644)
}

// Last reads the outcome. A missing file is nil with no error: every box that
// has never updated is in that state.
func (s LastUpdate) Last() (*protocol.ControlPlaneOutcome, error) {
	b, err := os.ReadFile(LastUpdatePath(s.Dir))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read last control-plane update: %w", err)
	}
	var o protocol.ControlPlaneOutcome
	if err := json.Unmarshal(b, &o); err != nil {
		return nil, fmt.Errorf("parse %s: %w", LastUpdatePath(s.Dir), err)
	}
	return &o, nil
}
