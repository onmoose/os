package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onmoose/moose/internal/protocol"
)

// TestWriteAtomic_NoTempLeftovers asserts the standard atomic-write
// invariant: after a successful write, the target file exists and *only*
// the target file exists — no .storage.json.* temp shrapnel from the
// CreateTemp call survives in the directory.
func TestWriteAtomic_NoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "storage.json")

	for i := 0; i < 5; i++ {
		if err := writeAtomic(target, protocol.StorageHealth{
			CheckedAt: "2026-05-25T08:00:00Z",
			Findings:  []protocol.Finding{},
		}); err != nil {
			t.Fatalf("writeAtomic iter %d: %v", i, err)
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "storage.json" {
			continue
		}
		if strings.HasPrefix(e.Name(), ".storage.json.") {
			t.Errorf("leftover temp file: %s", e.Name())
		}
	}
	if len(entries) != 1 {
		t.Errorf("dir should hold exactly 1 file (storage.json), found %d: %v",
			len(entries), entries)
	}
}
