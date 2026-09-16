// Package healthsource reads storage findings written by
// moose-storage-verify (BOOT.md # The storage-ready target) from
// /run/moose/health/storage.json and returns them to the host-agent HTTP
// layer as a protocol.StorageHealth payload.
//
// Contract (see hostagent.HealthSource): Read always returns a usable
// StorageHealth. Missing file = empty findings ("storage looks healthy", or
// "no report yet" — the brain treats them the same). Malformed JSON = a
// single "health-report-malformed" finding so the brain still raises
// something rather than silently passing. Any error returned alongside the
// payload is for the handler's structured log; the payload is always the
// authoritative thing to ship to the brain.
package healthsource

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/onmoose/moose/internal/protocol"
)

// DefaultPath is the production location of the storage findings file, set
// by moose-storage-verify.service. Cross-ref: BOOT.md # The storage-ready
// target.
const DefaultPath = "/run/moose/health/storage.json"

// FilesystemHealthSource reads protocol.StorageHealth from a JSON file.
type FilesystemHealthSource struct {
	path string
}

// New returns a source that reads from path. Pass DefaultPath in prod;
// tests pass a tempdir path.
func New(path string) *FilesystemHealthSource {
	return &FilesystemHealthSource{path: path}
}

// Read returns the current StorageHealth payload. It never returns a
// payload-less error: every code path produces a payload the handler can
// ship verbatim to the brain.
func (s *FilesystemHealthSource) Read() (protocol.StorageHealth, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		// No report file yet — boot may still be in progress, or the
		// reporter is disabled in this environment. Empty findings tells
		// the brain "storage looks healthy"; if that's wrong, a periodic
		// re-check from a different signal will catch it.
		return protocol.StorageHealth{
			CheckedAt: time.Now().UTC().Format(time.RFC3339),
			Findings:  []protocol.Finding{},
		}, nil
	}
	if err != nil {
		// Unreadable file (permissions, IO error). Same posture as
		// malformed: raise health-report-malformed so the user sees
		// *something*, return the underlying error for the log.
		return malformed(fmt.Sprintf("reading %s: %v", s.path, err)), err
	}

	var sh protocol.StorageHealth
	if err := json.Unmarshal(data, &sh); err != nil {
		return malformed(fmt.Sprintf("parsing %s: %v", s.path, err)), err
	}
	if sh.Findings == nil {
		sh.Findings = []protocol.Finding{}
	}
	if sh.CheckedAt == "" {
		sh.CheckedAt = time.Now().UTC().Format(time.RFC3339)
	}
	return sh, nil
}

func malformed(details string) protocol.StorageHealth {
	return protocol.StorageHealth{
		CheckedAt: time.Now().UTC().Format(time.RFC3339),
		Findings: []protocol.Finding{
			{ID: "health-report-malformed", Details: details},
		},
	}
}
