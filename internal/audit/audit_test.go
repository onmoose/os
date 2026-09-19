package audit

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/onmoose/os/internal/auth"
	"github.com/onmoose/os/internal/store"
)

// fakeStore captures InsertAuditEvent calls for assertions.
type fakeStore struct {
	events []store.AuditEvent
	err    error
}

func (f *fakeStore) InsertAuditEvent(e store.AuditEvent) error {
	if f.err != nil {
		return f.err
	}
	f.events = append(f.events, e)
	return nil
}

func newTestRecorder(fs *fakeStore) *Recorder {
	return New(fs)
}

func identity(userID, role string) auth.Identity {
	return auth.Identity{
		User:    store.User{ID: userID, Role: role, CreatedAt: time.Unix(0, 0)},
		Session: store.Session{Token: "tok", UserID: userID},
	}
}

func TestRecordWithAuthenticatedContext(t *testing.T) {
	fs := &fakeStore{}
	rec := newTestRecorder(fs)

	ctx := auth.WithIdentity(context.Background(), identity("u1", "admin"))
	ctx = WithClientIP(ctx, "10.0.0.1")

	rec.Record(ctx, ActionLoginSuccess, Target{}, nil, true)

	if len(fs.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(fs.events))
	}
	e := fs.events[0]
	if e.ActorUserID != "u1" {
		t.Errorf("actor_user_id = %q, want u1", e.ActorUserID)
	}
	if e.ActorRole != "admin" {
		t.Errorf("actor_role = %q, want admin", e.ActorRole)
	}
	if e.Action != ActionLoginSuccess {
		t.Errorf("action = %q, want %s", e.Action, ActionLoginSuccess)
	}
	if e.SourceIP != "10.0.0.1" {
		t.Errorf("source_ip = %q, want 10.0.0.1", e.SourceIP)
	}
	if !e.Success {
		t.Error("success should be true")
	}
}

func TestRecordSystemEvent(t *testing.T) {
	fs := &fakeStore{}
	rec := newTestRecorder(fs)

	// No identity in context — system actor.
	rec.Record(context.Background(), ActionSetupComplete, Target{}, nil, true)

	if len(fs.events) != 1 {
		t.Fatalf("want 1 event, got %d", len(fs.events))
	}
	e := fs.events[0]
	if e.ActorUserID != "" {
		t.Errorf("system event actor_user_id should be empty, got %q", e.ActorUserID)
	}
	if e.ActorRole != "system" {
		t.Errorf("actor_role = %q, want system", e.ActorRole)
	}
}

func TestRecordWithTarget(t *testing.T) {
	fs := &fakeStore{}
	rec := newTestRecorder(fs)

	ctx := auth.WithIdentity(context.Background(), identity("u1", "admin"))
	rec.Record(ctx, ActionAppInstall, Target{Kind: "app", ID: "inst_abc"}, nil, true)

	e := fs.events[0]
	if e.TargetKind != "app" || e.TargetID != "inst_abc" {
		t.Errorf("target = {%q %q}, want {app inst_abc}", e.TargetKind, e.TargetID)
	}
}

func TestRecordWithMetadata(t *testing.T) {
	fs := &fakeStore{}
	rec := newTestRecorder(fs)

	ctx := auth.WithIdentity(context.Background(), identity("u1", "admin"))
	rec.Record(ctx, ActionAppInstall, Target{Kind: "app", ID: "inst1"},
		map[string]any{"slug": "whoami", "manifest_id": "whoami"}, true)

	e := fs.events[0]
	if e.Metadata == "" {
		t.Error("metadata should be populated")
	}
}

func TestRecordInsertFailureDoesNotPanic(t *testing.T) {
	fs := &fakeStore{err: errors.New("db gone")}
	rec := newTestRecorder(fs)

	// Must not panic or propagate the error.
	rec.Record(context.Background(), ActionLoginSuccess, Target{}, nil, true)
}

func TestRecordLoginFailure(t *testing.T) {
	fs := &fakeStore{}
	rec := newTestRecorder(fs)

	ctx := WithClientIP(context.Background(), "192.168.1.50")
	rec.Record(ctx, ActionLoginFailure, Target{}, nil, false)

	e := fs.events[0]
	if e.Success {
		t.Error("success should be false for login.failure")
	}
	if e.ActorRole != "system" {
		t.Errorf("actor_role = %q, want system (no identity on failed login)", e.ActorRole)
	}
}

func TestClientIPContext(t *testing.T) {
	ctx := WithClientIP(context.Background(), "172.16.0.1")
	ip, ok := ClientIPFromContext(ctx)
	if !ok || ip != "172.16.0.1" {
		t.Fatalf("ClientIPFromContext = %q %v, want 172.16.0.1 true", ip, ok)
	}

	_, ok = ClientIPFromContext(context.Background())
	if ok {
		t.Fatal("empty context should return ok=false")
	}
}
