package controlplane

import (
	"os"
	"testing"

	"github.com/onmoose/os/internal/protocol"
)

func TestLastUpdateMissingIsNil(t *testing.T) {
	got, err := LastUpdate{Dir: t.TempDir()}.Last()
	if err != nil || got != nil {
		t.Fatalf("Last() = %v, %v; want nil, nil on a box that never updated", got, err)
	}
}

func TestLastUpdateRoundTrip(t *testing.T) {
	s := LastUpdate{Dir: t.TempDir()}
	want := protocol.ControlPlaneOutcome{
		ID: "j_1", Outcome: protocol.ControlPlaneOutcomeFailed, Version: "0.18.0", From: "0.17.0",
		At: "2026-10-12T03:04:00Z", StartedAt: "2026-10-12T03:03:00Z", BrainChanged: true,
		FailureMode: "health", Reverted: true,
	}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.Last()
	if err != nil {
		t.Fatal(err)
	}
	if *got != want {
		t.Fatalf("Last() = %+v, want %+v", *got, want)
	}
}

func TestLastUpdateGarbageIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(LastUpdatePath(dir), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := (LastUpdate{Dir: dir}).Last(); err == nil {
		t.Fatal("Last() on a torn file: want an error, got nil")
	}
}
