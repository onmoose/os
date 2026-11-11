//go:build linux

package usermgr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/onmoose/os/internal/protocol"
)

// TestWritePinFailure pins the marker the brain reads at startup: a failed
// restore writes the path and reason, a later good restore removes it.
func TestWritePinFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, protocol.FolderPinFailedFile)

	writePinFailure(dir, &protocol.FolderPinFailure{Path: "Documents/Notebooks", Reason: "the source is not a real folder"})
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the marker: %v", err)
	}
	var got protocol.FolderPinFailure
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("parse the marker: %v", err)
	}
	if got.Path != "Documents/Notebooks" || got.Reason == "" {
		t.Errorf("marker = %+v", got)
	}

	writePinFailure(dir, nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a good restore did not remove the marker: %v", err)
	}
}

// TestReadPinRecordsKeepsTheInstanceDir: the restorer writes the failure marker
// beside the record, so the reader must return the dir it came from.
func TestReadPinRecordsKeepsTheInstanceDir(t *testing.T) {
	state := t.TempDir()
	idDir := filepath.Join(state, "instances", "i1")
	if err := os.MkdirAll(idDir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(protocol.FolderPinRecord{Pins: []protocol.FolderPin{{
		Pin: "/run/moose/folder-pins/8-1", Username: "alex", Path: "Documents/Notebooks",
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(idDir, "folder-pins.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	recs, err := readPinRecords(state)
	if err != nil {
		t.Fatalf("readPinRecords: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want one record, got %+v", recs)
	}
	if recs[0].dir != idDir {
		t.Errorf("record dir = %q, want %q", recs[0].dir, idDir)
	}
	if len(recs[0].rec.Pins) != 1 || recs[0].rec.Pins[0].Path != "Documents/Notebooks" {
		t.Errorf("record pins = %+v", recs[0].rec.Pins)
	}
}
