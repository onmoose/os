package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/protocol"
)

type fakeOutcomeHost struct{ t protocol.UpdateTarget }

func (f fakeOutcomeHost) SystemUpdateTarget(context.Context) (protocol.UpdateTarget, error) {
	return f.t, nil
}

type fakeSeen map[string]bool

func (f fakeSeen) HasNotification(k string) (bool, error) { return f[k], nil }

type fakeOutcomeNotifier struct{ raised []string }

func (f *fakeOutcomeNotifier) OSUpdateOutcome(id, outcome, version, from string) {
	f.raised = append(f.raised, id+" "+outcome+" "+version+" "+from)
}

func TestCheckOSOutcome(t *testing.T) {
	now := time.Date(2026, 10, 3, 9, 0, 0, 0, time.UTC)
	last := &protocol.OSOutcome{ID: "os-0.15.1-1", Outcome: "reverted", Version: "0.15.1", From: "0.15.0", At: "2026-10-03T03:20:00Z"}
	host := fakeOutcomeHost{protocol.UpdateTarget{OS: &protocol.OSUpdate{State: "held", Last: last}}}

	n := &fakeOutcomeNotifier{}
	checkOSOutcome(context.Background(), host, fakeSeen{}, n, now)
	if len(n.raised) != 1 || n.raised[0] != "os-0.15.1-1 reverted 0.15.1 0.15.0" {
		t.Fatalf("want one notification, got %v", n.raised)
	}

	n = &fakeOutcomeNotifier{}
	checkOSOutcome(context.Background(), host, fakeSeen{"os-update:os-0.15.1-1": true}, n, now)
	if len(n.raised) != 0 {
		t.Fatalf("an outcome already notified was raised again: %v", n.raised)
	}

	n = &fakeOutcomeNotifier{}
	checkOSOutcome(context.Background(), host, fakeSeen{}, n, now.Add(8*24*time.Hour))
	if len(n.raised) != 0 {
		t.Fatalf("an old outcome was raised: %v", n.raised)
	}

	n = &fakeOutcomeNotifier{}
	checkOSOutcome(context.Background(), fakeOutcomeHost{}, fakeSeen{}, n, now)
	if len(n.raised) != 0 {
		t.Fatalf("no OS part must raise nothing: %v", n.raised)
	}
}

func TestWriteFloorFile(t *testing.T) {
	dir := t.TempDir()
	writeFloorFile(dir)
	b, err := os.ReadFile(filepath.Join(dir, floorFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(b)) != minimumAgentVersion {
		t.Fatalf("got %q", b)
	}
}
