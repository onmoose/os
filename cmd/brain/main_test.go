package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/onmoose/os/internal/api"
	"github.com/onmoose/os/internal/audit"
	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/health"
	"github.com/onmoose/os/internal/lifecycle"
	"github.com/onmoose/os/internal/manifest"
	"github.com/onmoose/os/internal/notify"
	"github.com/onmoose/os/internal/profile"
	"github.com/onmoose/os/internal/protocol"
	"github.com/onmoose/os/internal/store"
)

// fakeEventStore captures audit rows so the per-issue emission can be asserted
// without a real SQLite store. Satisfies audit.EventStore.
type fakeEventStore struct {
	events []store.AuditEvent
}

func (f *fakeEventStore) InsertAuditEvent(e store.AuditEvent) error {
	f.events = append(f.events, e)
	return nil
}

// TestEmitHealthTransitions_OneRecordPerIssue is the direct test of this
// slice's headline behavior: one audit record per transitioned issue, each
// targeting {kind: health_issue, id: <id>} with a system actor — not a bulk
// count record.
func TestEmitHealthTransitions_OneRecordPerIssue(t *testing.T) {
	fs := &fakeEventStore{}
	auditor := audit.New(fs)

	raised := []health.IssueKey{{ID: "data-drive-missing"}, {ID: "canary-mismatch"}}
	cleared := []health.IssueKey{{ID: "mergerfs-assembly-failed"}}

	emitHealthTransitions(context.Background(), auditor, nil, raised, cleared)

	if len(fs.events) != 3 {
		t.Fatalf("want 3 audit records (2 raised + 1 cleared), got %d", len(fs.events))
	}

	// Raised records come first, in the order passed, each targeting its issue.
	for i, k := range raised {
		e := fs.events[i]
		if e.Action != audit.ActionHealthIssueRaised {
			t.Errorf("event %d: action = %q, want %q", i, e.Action, audit.ActionHealthIssueRaised)
		}
		if e.TargetKind != "health_issue" || e.TargetID != k.ID {
			t.Errorf("event %d: target = {%q,%q}, want {health_issue,%q}", i, e.TargetKind, e.TargetID, k.ID)
		}
		if !e.Success {
			t.Errorf("event %d: success = false, want true", i)
		}
		// No identity in ctx → system actor (no false attribution to a user).
		if e.ActorRole != "system" || e.ActorUserID != "" {
			t.Errorf("event %d: actor = {%q,%q}, want {system,<empty>}", i, e.ActorRole, e.ActorUserID)
		}
	}

	// The cleared record uses the clear action and targets the dropped issue.
	c := fs.events[2]
	if c.Action != audit.ActionHealthIssueCleared || c.TargetID != "mergerfs-assembly-failed" {
		t.Errorf("cleared event = {%q,%q}, want {%q,mergerfs-assembly-failed}",
			c.Action, c.TargetID, audit.ActionHealthIssueCleared)
	}
}

// TestEmitHealthTransitions_NoTransitionsNoRecords pins the steady-state case:
// a poll that changes nothing must not write any audit rows.
func TestEmitHealthTransitions_NoTransitionsNoRecords(t *testing.T) {
	fs := &fakeEventStore{}
	emitHealthTransitions(context.Background(), audit.New(fs), nil, nil, nil)
	if len(fs.events) != 0 {
		t.Fatalf("want 0 records for no transitions, got %d", len(fs.events))
	}
}

// TestEmitHealthTransitions_PublishesToBus pins the issue #12 seam: each raised
// and cleared issue publishes the matching SSE event (advisory {id,
// instance_key}) so the dashboard's degraded-mode banner updates live. Audit
// and notify are covered above; here we assert only the event fan-out.
func TestEmitHealthTransitions_PublishesToBus(t *testing.T) {
	bus := events.NewBus()
	ch, unsub := bus.Subscribe()
	defer unsub()

	raised := []health.IssueKey{{ID: "service-down", InstanceKey: "avahi-daemon"}}
	cleared := []health.IssueKey{{ID: "version-mismatch"}}
	emitHealthTransitions(context.Background(), audit.New(&fakeEventStore{}), bus, raised, cleared)

	first := recvEvent(t, ch)
	if first.Kind != events.HealthIssueRaised ||
		first.Data["id"] != "service-down" || first.Data["instance_key"] != "avahi-daemon" {
		t.Errorf("raised event = {%s, %v}, want {%s, service-down/avahi-daemon}",
			first.Kind, first.Data, events.HealthIssueRaised)
	}
	second := recvEvent(t, ch)
	if second.Kind != events.HealthIssueCleared || second.Data["id"] != "version-mismatch" {
		t.Errorf("cleared event = {%s, %v}, want {%s, version-mismatch}",
			second.Kind, second.Data, events.HealthIssueCleared)
	}
}

// recvEvent reads one event from the bus subscription, failing the test if none
// arrives promptly (Publish delivers synchronously into the buffered channel, so
// this never actually blocks on a correct implementation).
func recvEvent(t *testing.T, ch <-chan events.Event) events.Event {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for a published event")
		return events.Event{}
	}
}

// fakeNotifStore captures notification raises/resolves so the cmd/brain
// dispatch can be asserted without SQLite. Satisfies notify.NotificationStore.
type fakeNotifStore struct {
	raised   []notify.Notification
	resolved []string
}

func (f *fakeNotifStore) RaiseNotification(n notify.Notification) error {
	f.raised = append(f.raised, n)
	return nil
}

func (f *fakeNotifStore) ResolveNotification(dedupKey string, _ time.Time) error {
	f.resolved = append(f.resolved, dedupKey)
	return nil
}

// TestEmitHealthNotifications_RaiseLooksUpIssueAndClearResolves is the wiring
// test for this slice: emitHealthNotifications resolves each raised key to its
// live Issue (via Manager.Get) and notifies, and resolves each cleared key by
// dedup_key. Allowlist filtering lives in notify and is tested there; here we
// pin the cmd/brain dispatch — Get-lookup, raise, and resolve.
func TestEmitHealthNotifications_RaiseLooksUpIssueAndClearResolves(t *testing.T) {
	mgr := health.NewManager(nil)
	// Raise two allowlisted issues so Get() returns them at dispatch time.
	mgr.Raise("data-drive-missing", "", "abc-123 absent")
	mgr.Raise("canary-mismatch", "", "checksum drift")

	fns := &fakeNotifStore{}
	notifier := notify.New(fns, nil)

	raised := []health.IssueKey{{ID: "data-drive-missing"}, {ID: "canary-mismatch"}}
	cleared := []health.IssueKey{{ID: "mergerfs-assembly-failed"}}

	emitHealthNotifications(notifier, mgr, raised, cleared)

	// Dispatch pins: each raised key is looked up via Manager.Get and produces an
	// admin notification carrying the live issue's data; the cleared key resolves
	// its problem dedup key. The richer raise/clear behavior (member transparency
	// and "all clear" follow-ups, which add more raised/resolved rows) is asserted
	// in internal/notify — here we only confirm the cmd/brain wiring.
	adminNotif := func(sourceID string) (notify.Notification, bool) {
		for _, n := range fns.raised {
			if n.SourceID == sourceID && n.Audience == notify.AudienceAdmins {
				return n, true
			}
		}
		return notify.Notification{}, false
	}

	ddm, ok := adminNotif("data-drive-missing")
	if !ok {
		t.Fatalf("no admin notification for data-drive-missing; raised = %v", fns.raised)
	}
	// The body is the live issue's Details — proves emitHealthNotifications resolved
	// the key through Manager.Get rather than synthesizing a stub.
	if ddm.Body != "abc-123 absent" {
		t.Errorf("data-drive-missing body = %q, want the live issue's details", ddm.Body)
	}
	if _, ok := adminNotif("canary-mismatch"); !ok {
		t.Errorf("no admin notification for canary-mismatch; raised = %v", fns.raised)
	}
	if !containsString(fns.resolved, "health:mergerfs-assembly-failed") {
		t.Errorf("resolved = %v, want it to include the cleared issue's problem key", fns.resolved)
	}
}

func containsString(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// fakeStatusReader is a host client that reports a chosen agent_version (or an
// error) so the locus-C version check can be driven without a real host-agent.
// Satisfies agentStatusReader.
type fakeStatusReader struct {
	version string
	err     error
}

func (f fakeStatusReader) SystemStatus(context.Context) (protocol.SystemStatus, error) {
	if f.err != nil {
		return protocol.SystemStatus{}, f.err
	}
	return protocol.SystemStatus{AgentVersion: f.version}, nil
}

func versionActive(mgr *health.Manager) bool {
	_, ok := mgr.Get("version-mismatch", "")
	return ok
}

// TestAgentVersionAcceptable pins the compatible-range comparison
// (DECISIONS.md 2026-07-16: version-mismatch moved off exact equality)
// against minimumAgentVersion's semantics directly, independent of the health
// wiring exercised by the checkAgentVersion tests below.
func TestAgentVersionAcceptable(t *testing.T) {
	const min = "0.4.0"
	cases := []struct {
		name  string
		agent string
		want  bool
	}{
		{"older patch is not acceptable", "0.3.9", false},
		{"older minor is not acceptable", "0.3.0", false},
		{"older major is not acceptable", "0.0.1", false},
		{"exact match is acceptable", "0.4.0", true},
		{"newer patch is acceptable", "0.4.1", true},
		{"newer minor is acceptable", "0.5.0", true},
		{"newer major is acceptable", "1.0.0", true},
		// The in-repo fake host-agent self-identifies with a "-fake" build
		// suffix (internal/hostagent.AgentVersion = version.Version, no
		// suffix actually — but a defensive case in general: any
		// prerelease/build suffix on an otherwise-current version must not
		// make it look older, or every `make dev` would spuriously raise
		// version-mismatch against its own paired brain).
		{"prerelease suffix on the exact core is acceptable", "0.4.0-fake", true},
		{"build suffix on the exact core is acceptable", "0.4.0+build123", true},
		{"prerelease suffix on an older core is not acceptable", "0.3.9-fake", false},
		{"garbled version is not acceptable", "not-a-version", false},
		{"empty version is not acceptable", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agentVersionAcceptable(tc.agent, min)
			if got != tc.want {
				t.Errorf("agentVersionAcceptable(%q, %q) = %v, want %v", tc.agent, min, got, tc.want)
			}
		})
	}
}

// TestCheckAgentVersion_OlderThanMinimumRaises is the headline behavior (#37
// Done-when, updated for the compatible-range model): an agent_version older
// than minimumAgentVersion raises version-mismatch and writes exactly one
// raised audit record for it.
func TestCheckAgentVersion_OlderThanMinimumRaises(t *testing.T) {
	mgr := health.NewManager(nil)
	fs := &fakeEventStore{}

	checkAgentVersion(context.Background(), fakeStatusReader{version: "0.1.0"},
		mgr, audit.New(fs), notify.New(&fakeNotifStore{}, nil), nil)

	if !versionActive(mgr) {
		t.Fatal("want version-mismatch active after an agent version older than the minimum")
	}
	if len(fs.events) != 1 ||
		fs.events[0].Action != audit.ActionHealthIssueRaised ||
		fs.events[0].TargetID != "version-mismatch" {
		t.Fatalf("want one raised audit record for version-mismatch, got %+v", fs.events)
	}
}

// TestCheckAgentVersion_NewerThanMinimumNeverRaises pins the compatible-range
// point directly: an agent newer than minimumAgentVersion is normal mid-rollout
// state (UPDATES.md # 1, host-agent updates before brain), not a mismatch.
func TestCheckAgentVersion_NewerThanMinimumNeverRaises(t *testing.T) {
	mgr := health.NewManager(nil)
	fs := &fakeEventStore{}

	checkAgentVersion(context.Background(), fakeStatusReader{version: "9.9.9"},
		mgr, audit.New(fs), notify.New(&fakeNotifStore{}, nil), nil)

	if versionActive(mgr) {
		t.Error("want no version-mismatch for an agent newer than the minimum")
	}
	if len(fs.events) != 0 {
		t.Errorf("want no audit records for a newer-than-minimum agent, got %d", len(fs.events))
	}
}

// TestCheckAgentVersion_AtOrAboveMinimumClears: once raised, a subsequent
// version at or above the minimum clears version-mismatch and writes the clear
// audit record.
func TestCheckAgentVersion_AtOrAboveMinimumClears(t *testing.T) {
	mgr := health.NewManager(nil)
	fs := &fakeEventStore{}
	auditor := audit.New(fs)
	notifier := notify.New(&fakeNotifStore{}, nil)

	checkAgentVersion(context.Background(), fakeStatusReader{version: "0.1.0"}, mgr, auditor, notifier, nil)
	if !versionActive(mgr) {
		t.Fatal("setup: want version-mismatch active after an older agent version")
	}
	checkAgentVersion(context.Background(), fakeStatusReader{version: minimumAgentVersion}, mgr, auditor, notifier, nil)
	if versionActive(mgr) {
		t.Fatal("want version-mismatch cleared once the agent is at the minimum")
	}
	if len(fs.events) != 2 ||
		fs.events[1].Action != audit.ActionHealthIssueCleared ||
		fs.events[1].TargetID != "version-mismatch" {
		t.Fatalf("want a raise then a clear audit record, got %+v", fs.events)
	}
}

// TestCheckAgentVersion_MatchNoIssueIsNoop pins the steady happy path: a
// version at the minimum with no active issue raises nothing and writes no
// audit row.
func TestCheckAgentVersion_MatchNoIssueIsNoop(t *testing.T) {
	mgr := health.NewManager(nil)
	fs := &fakeEventStore{}

	checkAgentVersion(context.Background(), fakeStatusReader{version: minimumAgentVersion},
		mgr, audit.New(fs), notify.New(&fakeNotifStore{}, nil), nil)

	if versionActive(mgr) {
		t.Error("want no version-mismatch for a version at the minimum")
	}
	if len(fs.events) != 0 {
		t.Errorf("want no audit records for a steady matching version, got %d", len(fs.events))
	}
}

// TestCheckAgentVersion_SteadyMismatchRefreshesWithoutReaudit: a persistent
// older-than-minimum agent raises once; a second poll refreshes
// last_checked_at (HEALTH.md last-checked-always-fresh) without re-raising or
// writing a second audit row, and leaves raised_at untouched.
func TestCheckAgentVersion_SteadyMismatchRefreshesWithoutReaudit(t *testing.T) {
	mgr := health.NewManager(nil)
	clock := time.Unix(0, 0).UTC()
	mgr.SetClock(func() time.Time { return clock })
	fs := &fakeEventStore{}
	auditor := audit.New(fs)
	notifier := notify.New(&fakeNotifStore{}, nil)
	reader := fakeStatusReader{version: "0.1.0"}

	checkAgentVersion(context.Background(), reader, mgr, auditor, notifier, nil)
	first, _ := mgr.Get("version-mismatch", "")

	clock = clock.Add(60 * time.Second)
	checkAgentVersion(context.Background(), reader, mgr, auditor, notifier, nil)
	second, ok := mgr.Get("version-mismatch", "")
	if !ok {
		t.Fatal("want version-mismatch still active on the second poll")
	}
	if len(fs.events) != 1 {
		t.Fatalf("want exactly one raised audit across two mismatch polls, got %d", len(fs.events))
	}
	if !second.LastCheckedAt.After(first.LastCheckedAt) {
		t.Errorf("last_checked_at must advance on a no-transition poll: first %v second %v",
			first.LastCheckedAt, second.LastCheckedAt)
	}
	if !second.RaisedAt.Equal(first.RaisedAt) {
		t.Errorf("raised_at must not move on a re-raise: first %v second %v",
			first.RaisedAt, second.RaisedAt)
	}
}

// TestCheckAgentVersion_UnreachableLeavesStateUnchanged: a poll that can't reach
// host-agent must not clear an active version-mismatch (an error is not
// acceptable) and must write no audit record.
func TestCheckAgentVersion_UnreachableLeavesStateUnchanged(t *testing.T) {
	mgr := health.NewManager(nil)
	notifier := notify.New(&fakeNotifStore{}, nil)

	checkAgentVersion(context.Background(), fakeStatusReader{version: "0.1.0"},
		mgr, audit.New(&fakeEventStore{}), notifier, nil)
	if !versionActive(mgr) {
		t.Fatal("setup: want version-mismatch active")
	}

	fs := &fakeEventStore{}
	checkAgentVersion(context.Background(), fakeStatusReader{err: errors.New("dial unix: connection refused")},
		mgr, audit.New(fs), notifier, nil)
	if !versionActive(mgr) {
		t.Error("an unreachable host-agent must not clear version-mismatch")
	}
	if len(fs.events) != 0 {
		t.Errorf("want no audit records on an unreachable poll, got %d", len(fs.events))
	}
}

// A raised key with no live issue produces no notification. emitHealthNotifications
// looks the issue up via Manager.Get and only notifies when it's still active.
// (This is doubly safe: even without the ok guard, Get returns a zero-value
// Issue{ID:""} on a miss, which notify drops since "" isn't allowlisted — so
// this test pins the observable contract, not the guard in isolation. Issue is
// a value type, so there's no nil-deref to defend against.)
func TestEmitHealthNotifications_NoNotificationForInactiveKey(t *testing.T) {
	mgr := health.NewManager(nil) // empty — Get returns ok=false
	fns := &fakeNotifStore{}
	emitHealthNotifications(notify.New(fns, nil), mgr, []health.IssueKey{{ID: "data-drive-missing"}}, nil)
	if len(fns.raised) != 0 {
		t.Fatalf("want 0 raises for a key with no live issue, got %d", len(fns.raised))
	}
}

// fakeIntegrityChecker drives checkBrainDBIntegrity through each branch without
// a real SQLite file. Satisfies integrityChecker.
type fakeIntegrityChecker struct {
	result string
	err    error
}

func (f fakeIntegrityChecker) IntegrityCheck() (string, error) { return f.result, f.err }

func dbCorruptActive(mgr *health.Manager) bool {
	_, ok := mgr.Get("brain-db-corrupt", "")
	return ok
}

// TestCheckBrainDBIntegrity_CorruptRaises is the #36 Done-when: a non-"ok"
// integrity result raises brain-db-corrupt, writes exactly one raised audit
// record targeting the issue, and carries the integrity_check output in details.
func TestCheckBrainDBIntegrity_CorruptRaises(t *testing.T) {
	mgr := health.NewManager(nil)
	fs := &fakeEventStore{}
	fns := &fakeNotifStore{}
	checker := fakeIntegrityChecker{result: "*** in database main ***\nrow 5 missing from index idx_x"}

	checkBrainDBIntegrity(context.Background(), checker, mgr, audit.New(fs), notify.New(fns, nil), nil)

	if !dbCorruptActive(mgr) {
		t.Fatal("want brain-db-corrupt active after a non-ok integrity check")
	}
	if len(fs.events) != 1 {
		t.Fatalf("want 1 audit record (the raise), got %d", len(fs.events))
	}
	e := fs.events[0]
	if e.Action != audit.ActionHealthIssueRaised || e.TargetKind != "health_issue" || e.TargetID != "brain-db-corrupt" {
		t.Errorf("audit = {%q,%q,%q}, want {%q,health_issue,brain-db-corrupt}",
			e.Action, e.TargetKind, e.TargetID, audit.ActionHealthIssueRaised)
	}
	if !e.Success {
		t.Error("raise audit record: Success = false, want true")
	}
	// Details carry the integrity_check report so the diagnostic bundle has it.
	iss, _ := mgr.Get("brain-db-corrupt", "")
	if !strings.Contains(iss.Details, "missing from index") {
		t.Errorf("issue details = %q, want it to include the integrity_check output", iss.Details)
	}
}

// TestCheckBrainDBIntegrity_OkClears: an "ok" result clears a prior raise and
// writes one clear audit record (#36 Done-when: an ok result clears it).
func TestCheckBrainDBIntegrity_OkClears(t *testing.T) {
	mgr := health.NewManager(nil)
	mgr.Raise("brain-db-corrupt", "", "prior corruption")
	fs := &fakeEventStore{}
	fns := &fakeNotifStore{}

	checkBrainDBIntegrity(context.Background(), fakeIntegrityChecker{result: "ok"}, mgr, audit.New(fs), notify.New(fns, nil), nil)

	if dbCorruptActive(mgr) {
		t.Fatal("want brain-db-corrupt cleared after an ok integrity check")
	}
	if len(fs.events) != 1 || fs.events[0].Action != audit.ActionHealthIssueCleared {
		t.Fatalf("want 1 clear audit record, got %+v", fs.events)
	}
}

// TestCheckBrainDBIntegrity_OkNoIssueIsNoop: the steady-healthy path raises
// nothing and audits/notifies nothing.
func TestCheckBrainDBIntegrity_OkNoIssueIsNoop(t *testing.T) {
	mgr := health.NewManager(nil)
	fs := &fakeEventStore{}
	fns := &fakeNotifStore{}

	checkBrainDBIntegrity(context.Background(), fakeIntegrityChecker{result: "ok"}, mgr, audit.New(fs), notify.New(fns, nil), nil)

	if dbCorruptActive(mgr) {
		t.Error("ok on a clean registry must not raise anything")
	}
	if len(fs.events) != 0 {
		t.Errorf("want 0 audit records on the steady-healthy path, got %d", len(fs.events))
	}
	// Pins the no-transition path. (brain-db-corrupt isn't in notify.healthRules
	// yet, so the fan-out is a no-op for this ID regardless — real notification
	// coverage lands with the deferred healthRules entry, not here.)
	if len(fns.raised) != 0 {
		t.Errorf("want 0 notifications on the steady-healthy path, got %d", len(fns.raised))
	}
}

// TestCheckBrainDBIntegrity_SteadyCorruptRefreshesWithoutReaudit: a persistent
// corruption raises once; the next check refreshes last_checked_at without
// re-raising, re-auditing, or moving raised_at (HEALTH.md # Cross-cutting
// detector policy: "last-checked is always fresh").
func TestCheckBrainDBIntegrity_SteadyCorruptRefreshesWithoutReaudit(t *testing.T) {
	mgr := health.NewManager(nil)
	clock := time.Unix(1_700_000_000, 0).UTC()
	mgr.SetClock(func() time.Time { return clock })
	fs := &fakeEventStore{}
	auditor := audit.New(fs)
	notifier := notify.New(&fakeNotifStore{}, nil)
	checker := fakeIntegrityChecker{result: "row 5 missing from index idx_x"}

	checkBrainDBIntegrity(context.Background(), checker, mgr, auditor, notifier, nil)
	first, _ := mgr.Get("brain-db-corrupt", "")

	clock = clock.Add(6 * time.Hour)
	checkBrainDBIntegrity(context.Background(), checker, mgr, auditor, notifier, nil)
	second, _ := mgr.Get("brain-db-corrupt", "")

	if len(fs.events) != 1 {
		t.Fatalf("want exactly 1 audit record across two steady-corrupt checks, got %d", len(fs.events))
	}
	if !second.LastCheckedAt.After(first.LastCheckedAt) {
		t.Errorf("last_checked_at did not advance: first=%s second=%s", first.LastCheckedAt, second.LastCheckedAt)
	}
	if !second.RaisedAt.Equal(first.RaisedAt) {
		t.Errorf("raised_at moved on re-raise: first=%s second=%s", first.RaisedAt, second.RaisedAt)
	}
}

// TestCheckBrainDBIntegrity_QueryErrorLeavesStateUnchanged: a failed query
// can't conclude corrupt or sound, so it must neither clear an active issue nor
// audit — a transient blip neither raises a false banner nor clears a real one.
func TestCheckBrainDBIntegrity_QueryErrorLeavesStateUnchanged(t *testing.T) {
	mgr := health.NewManager(nil)
	mgr.Raise("brain-db-corrupt", "", "prior corruption")
	fs := &fakeEventStore{}
	fns := &fakeNotifStore{}

	checkBrainDBIntegrity(context.Background(), fakeIntegrityChecker{err: errors.New("disk I/O error")}, mgr, audit.New(fs), notify.New(fns, nil), nil)

	if !dbCorruptActive(mgr) {
		t.Error("a query error must not clear an active brain-db-corrupt")
	}
	if len(fs.events) != 0 {
		t.Errorf("a query error must not audit, got %d records", len(fs.events))
	}
}

// --- container-restart-loop detector (issue #35) -------------------------

// fakeRestartReader is a scriptable restartCountReader: `counts` is the map the
// next check() observes; setting `err` makes RestartCounts fail so the
// docker-unreachable path can be exercised. Both are mutated between checks to
// drive a scenario, mirroring how the real Docker counter evolves over polls.
type fakeRestartReader struct {
	counts map[string]int
	err    error
}

func (f *fakeRestartReader) RestartCounts(context.Context) (map[string]int, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(map[string]int, len(f.counts))
	for k, v := range f.counts {
		out[k] = v
	}
	return out, nil
}

// stepClock is a hand-advanced clock so detector tests assert window math
// deterministically without sleeping.
type stepClock struct{ t time.Time }

func (c *stepClock) now() time.Time      { return c.t }
func (c *stepClock) add(d time.Duration) { c.t = c.t.Add(d) }

// newTestDetector wires a restartLoopDetector to in-memory fakes and a
// hand-advanced clock, at the production threshold (3) and window (5m).
func newTestDetector(reader restartCountReader, clk *stepClock) (*restartLoopDetector, *health.Manager, *fakeEventStore, *fakeNotifStore) {
	mgr := health.NewManager(nil)
	es := &fakeEventStore{}
	ns := &fakeNotifStore{}
	d := &restartLoopDetector{
		docker:    reader,
		healthMgr: mgr,
		auditor:   audit.New(es),
		notifier:  notify.New(ns, nil),
		window:    restartLoopWindow,
		threshold: restartLoopThreshold,
		now:       clk.now,
		history:   map[string][]restartSample{},
	}
	return d, mgr, es, ns
}

func auditCount(es *fakeEventStore, action string) int {
	n := 0
	for _, e := range es.events {
		if e.Action == action {
			n++
		}
	}
	return n
}

// loopIssues returns the active container-restart-loop issues, ignoring any
// other category that might be present.
func loopIssues(mgr *health.Manager) []health.Issue {
	var out []health.Issue
	for _, iss := range mgr.List() {
		if iss.ID == "container-restart-loop" {
			out = append(out, iss)
		}
	}
	return out
}

// A within-window restart delta over the threshold raises the issue keyed to
// the owning instance, with one health.issue.raised audit record. RestartCount
// is cumulative, so the first sample only establishes a baseline; the raise
// comes from the climb on the second poll.
func TestRestartLoop_RaisesKeyedToInstance(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := &fakeRestartReader{counts: map[string]int{"immich--abby": 0}}
	d, mgr, es, _ := newTestDetector(r, clk)

	d.check(context.Background()) // baseline at 0 — no raise
	if len(loopIssues(mgr)) != 0 {
		t.Fatalf("first sample must not raise (delta 0), got %v", mgr.List())
	}

	clk.add(time.Minute)
	r.counts["immich--abby"] = 6
	d.check(context.Background()) // delta 6 > 3 → raise

	got := loopIssues(mgr)
	if len(got) != 1 {
		t.Fatalf("want 1 active restart-loop issue, got %d (%v)", len(got), mgr.List())
	}
	if got[0].InstanceKey != "immich--abby" {
		t.Errorf("InstanceKey: want immich--abby, got %q", got[0].InstanceKey)
	}
	if got[0].Details == "" {
		t.Error("Details should describe the restart count")
	}
	if n := auditCount(es, audit.ActionHealthIssueRaised); n != 1 {
		t.Errorf("want 1 raised audit record, got %d", n)
	}
}

// Once the container stops restarting, the old samples age out of the window,
// the delta falls to 0, and the issue clears with a paired audit record.
func TestRestartLoop_ClearsWhenStabilized(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := &fakeRestartReader{counts: map[string]int{"app": 0}}
	d, mgr, es, _ := newTestDetector(r, clk)

	d.check(context.Background())
	clk.add(time.Minute)
	r.counts["app"] = 6
	d.check(context.Background())
	if len(loopIssues(mgr)) != 1 {
		t.Fatalf("expected raise before stabilizing, got %v", mgr.List())
	}

	// Six minutes later with no new restarts: every prior sample is older than
	// the 5-minute window, so the baseline becomes the current count → delta 0.
	clk.add(6 * time.Minute)
	d.check(context.Background())
	if len(loopIssues(mgr)) != 0 {
		t.Fatalf("issue should clear once restarts stop, got %v", mgr.List())
	}
	if n := auditCount(es, audit.ActionHealthIssueCleared); n != 1 {
		t.Errorf("want 1 cleared audit record, got %d", n)
	}
}

// An uninstalled app drops out of RestartCounts entirely; the detector clears
// its issue and forgets its sample history so the map can't grow unbounded.
func TestRestartLoop_ClearsWhenInstanceAbsent(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := &fakeRestartReader{counts: map[string]int{"app": 0}}
	d, mgr, _, _ := newTestDetector(r, clk)

	d.check(context.Background())
	clk.add(time.Minute)
	r.counts["app"] = 6
	d.check(context.Background())
	if len(loopIssues(mgr)) != 1 {
		t.Fatalf("expected raise, got %v", mgr.List())
	}

	clk.add(time.Minute)
	r.counts = map[string]int{} // app uninstalled
	d.check(context.Background())
	if len(loopIssues(mgr)) != 0 {
		t.Fatalf("issue should clear when the instance is gone, got %v", mgr.List())
	}
	if len(d.history) != 0 {
		t.Errorf("history should drop the absent instance, still holds %v", d.history)
	}
}

// A high cumulative RestartCount observed on the very first sample (e.g. after a
// brain restart, with the crashes long in the past) must not raise: the
// detector thresholds the within-window delta, not the raw counter.
func TestRestartLoop_NoFalseRaiseOnHistoricalCount(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := &fakeRestartReader{counts: map[string]int{"app": 100}}
	d, mgr, _, _ := newTestDetector(r, clk)

	d.check(context.Background()) // first sample → baseline 100
	clk.add(time.Minute)
	d.check(context.Background()) // still 100, no new restarts → delta 0

	if len(loopIssues(mgr)) != 0 {
		t.Fatalf("a quiet container with a high historical count must not raise, got %v", mgr.List())
	}
}

// The threshold is strict (delta > N): exactly N restarts in the window does not
// raise; N+1 does.
func TestRestartLoop_ThresholdIsStrict(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := &fakeRestartReader{counts: map[string]int{"app": 0}}
	d, mgr, _, _ := newTestDetector(r, clk)

	d.check(context.Background()) // baseline 0
	clk.add(time.Minute)
	r.counts["app"] = restartLoopThreshold // delta == 3, not > 3
	d.check(context.Background())
	if len(loopIssues(mgr)) != 0 {
		t.Fatalf("delta == threshold must not raise, got %v", mgr.List())
	}

	clk.add(time.Minute)
	r.counts["app"] = restartLoopThreshold + 1 // delta == 4 > 3
	d.check(context.Background())
	if len(loopIssues(mgr)) != 1 {
		t.Fatalf("delta > threshold must raise, got %v", mgr.List())
	}
}

// Per-instance isolation: a looping container raises only its own instance's
// issue, leaving a quiet sibling untouched.
func TestRestartLoop_PerInstanceIsolation(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := &fakeRestartReader{counts: map[string]int{"loud": 0, "quiet": 0}}
	d, mgr, _, _ := newTestDetector(r, clk)

	d.check(context.Background())
	clk.add(time.Minute)
	r.counts["loud"] = 6 // only loud climbs
	d.check(context.Background())

	got := loopIssues(mgr)
	if len(got) != 1 || got[0].InstanceKey != "loud" {
		t.Fatalf("want exactly the loud instance raised, got %v", mgr.List())
	}
}

// Container recreation (app update / reinstall) resets RestartCount to 0. The
// detector must restart the window from the lower value rather than computing a
// negative delta against the stale high baseline — so a recreated container that
// is now quiet clears, and only its post-recreation restarts count toward a new
// raise.
func TestRestartLoop_ContainerRecreationResetsBaseline(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := &fakeRestartReader{counts: map[string]int{"app": 0}}
	d, mgr, _, _ := newTestDetector(r, clk)

	d.check(context.Background())
	clk.add(time.Minute)
	r.counts["app"] = 6
	d.check(context.Background())
	if len(loopIssues(mgr)) != 1 {
		t.Fatalf("expected raise before recreation, got %v", mgr.List())
	}

	// Container recreated: counter drops to 0. Quiet now → clears.
	clk.add(time.Minute)
	r.counts["app"] = 0
	d.check(context.Background())
	if len(loopIssues(mgr)) != 0 {
		t.Fatalf("recreated+quiet container should clear, got %v", mgr.List())
	}

	// A small climb off the fresh baseline stays under threshold — proving the
	// baseline reset to 0, not to the stale 6 (which would read as delta -5..+).
	clk.add(time.Minute)
	r.counts["app"] = 2
	d.check(context.Background())
	if len(loopIssues(mgr)) != 0 {
		t.Fatalf("post-recreation delta of 2 must not raise, got %v", mgr.List())
	}
}

// When Docker is unreachable, check() skips the poll and leaves health state and
// sample history exactly as they were — a transient inspect failure must not
// spuriously clear an active issue.
func TestRestartLoop_DockerErrorLeavesStateUnchanged(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := &fakeRestartReader{counts: map[string]int{"app": 0}}
	d, mgr, es, _ := newTestDetector(r, clk)

	d.check(context.Background())
	clk.add(time.Minute)
	r.counts["app"] = 6
	d.check(context.Background())
	if len(loopIssues(mgr)) != 1 {
		t.Fatalf("expected raise, got %v", mgr.List())
	}
	histBefore := len(d.history["app"])

	clk.add(time.Minute)
	r.err = errors.New("docker unreachable")
	d.check(context.Background())

	if len(loopIssues(mgr)) != 1 {
		t.Errorf("active issue must survive a docker error, got %v", mgr.List())
	}
	if n := auditCount(es, audit.ActionHealthIssueCleared); n != 0 {
		t.Errorf("docker error must not clear (got %d cleared records)", n)
	}
	if len(d.history["app"]) != histBefore {
		t.Errorf("history must be untouched on error: before %d, after %d", histBefore, len(d.history["app"]))
	}
}

// container-restart-loop is not on the notify allowlist (NOTIFICATIONS.md stages
// app-lifecycle notifications behind their detectors), so a raise surfaces in the
// health registry but emits no bell notification. This pins that staging
// decision until the allowlist entry lands as a separate change.
func TestRestartLoop_RaiseEmitsNoBellNotification(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	r := &fakeRestartReader{counts: map[string]int{"app": 0}}
	d, mgr, _, ns := newTestDetector(r, clk)

	d.check(context.Background())
	clk.add(time.Minute)
	r.counts["app"] = 6
	d.check(context.Background())
	if len(loopIssues(mgr)) != 1 {
		t.Fatalf("expected raise, got %v", mgr.List())
	}
	if len(ns.raised) != 0 {
		t.Errorf("container-restart-loop must not emit a notification (not allowlisted), got %v", ns.raised)
	}
}

// --- app-unresponsive probe detector (issue #54) -------------------------

// stubRoundTripper is a scriptable http transport: it answers per the request's
// Host header (the route Caddy would match), so a test can drive healthy /
// unhealthy / connection-failure probes without a real Caddy or app. It records
// the last request so the "routes through Caddy by Host" wiring can be asserted.
type stubRoundTripper struct {
	status map[string]int  // Host -> status to return
	fail   map[string]bool // Host -> return a transport error (timeout / refused)
	last   *http.Request
}

func (s *stubRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	s.last = r
	if s.fail[r.Host] {
		return nil, fmt.Errorf("dial %s: connection refused", r.Host)
	}
	code, ok := s.status[r.Host]
	if !ok {
		code = http.StatusOK
	}
	return &http.Response{
		StatusCode: code,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
	}, nil
}

type fakeInstanceLister struct{ instances []store.Instance }

func (f *fakeInstanceLister) List() ([]store.Instance, error) { return f.instances, nil }

type fakeManifestLoader struct{ m map[string]*manifest.Manifest }

func (f *fakeManifestLoader) InstanceManifest(id string) (*manifest.Manifest, error) {
	man, ok := f.m[id]
	if !ok {
		return nil, fmt.Errorf("no manifest for %s", id)
	}
	return man, nil
}

type fakeContainerReader struct{ containers []lifecycle.ManagedContainer }

func (f *fakeContainerReader) ManagedContainers(context.Context) ([]lifecycle.ManagedContainer, error) {
	return f.containers, nil
}

// newTestProbeDetector wires an appProbeDetector to in-memory fakes, a stub
// transport, and a hand-advanced clock, at the production raise threshold.
func newTestProbeDetector(lister instanceLister, loader instanceManifestLoader, reader managedContainerReader, rt http.RoundTripper, clk *stepClock) (*appProbeDetector, *health.Manager, *fakeEventStore, *fakeNotifStore) {
	mgr := health.NewManager(nil)
	es := &fakeEventStore{}
	ns := &fakeNotifStore{}
	d := &appProbeDetector{
		docker:    reader,
		instances: lister,
		manifests: loader,
		healthMgr: mgr,
		auditor:   audit.New(es),
		notifier:  notify.New(ns, nil),
		client:    &http.Client{Transport: rt},
		baseURL:   "http://caddy.test",
		now:       clk.now,
		bad:       map[string]int{},
	}
	return d, mgr, es, ns
}

func unresponsiveIssues(mgr *health.Manager) []health.Issue {
	var out []health.Issue
	for _, iss := range mgr.List() {
		if iss.ID == "app-unresponsive" {
			out = append(out, iss)
		}
	}
	return out
}

// probeManifest is a minimal manifest carrying just the fields the probe reads.
func probeManifest(mainService, path string, healthy []int, start time.Duration) *manifest.Manifest {
	return &manifest.Manifest{
		MainService: mainService,
		HealthProbe: &manifest.HealthProbe{Path: path, HealthyStatus: healthy, StartPeriod: start},
	}
}

// probeScenario builds a one-instance scenario: instance "app" (slug "app",
// mDNS "app.local"), main_service "web" running and started `age` before the
// clock, declaring a probe on `path` with `healthy`/`start`.
func probeScenario(clk *stepClock, path string, healthy []int, start, age time.Duration) (*fakeInstanceLister, *fakeManifestLoader, *fakeContainerReader) {
	lister := &fakeInstanceLister{instances: []store.Instance{
		{ID: "app", Slug: "app", MDNSName: "app.local", State: "running"},
	}}
	loader := &fakeManifestLoader{m: map[string]*manifest.Manifest{
		"app": probeManifest("web", path, healthy, start),
	}}
	reader := &fakeContainerReader{containers: []lifecycle.ManagedContainer{
		{InstanceID: "app", Service: "web", Running: true, StartedAt: clk.now().Add(-age)},
	}}
	return lister, loader, reader
}

// A failing probe raises app-unresponsive only on the 2nd consecutive failure
// (cross-cutting debounce), keyed to the instance, with one raised audit record.
func TestAppProbe_RaisesAfterTwoBadSamples(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister, loader, reader := probeScenario(clk, "/healthz", nil, manifest.DefaultStartPeriod, 5*time.Minute)
	rt := &stubRoundTripper{status: map[string]int{"app.local": 503}}
	d, mgr, es, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background()) // 1st bad — debounced, no raise
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("first bad sample must not raise, got %v", mgr.List())
	}
	d.check(context.Background()) // 2nd bad — raise
	got := unresponsiveIssues(mgr)
	if len(got) != 1 || got[0].InstanceKey != "app" {
		t.Fatalf("want 1 app-unresponsive keyed to 'app', got %v", mgr.List())
	}
	if n := auditCount(es, audit.ActionHealthIssueRaised); n != 1 {
		t.Errorf("want 1 raised audit record, got %d", n)
	}
}

// One good sample clears an active app-unresponsive, with a paired audit record.
func TestAppProbe_ClearsOnGoodSample(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister, loader, reader := probeScenario(clk, "/healthz", nil, manifest.DefaultStartPeriod, 5*time.Minute)
	rt := &stubRoundTripper{status: map[string]int{"app.local": 503}}
	d, mgr, es, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 1 {
		t.Fatalf("expected raise before recovery, got %v", mgr.List())
	}
	rt.status["app.local"] = 200
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("good sample should clear, got %v", mgr.List())
	}
	if n := auditCount(es, audit.ActionHealthIssueCleared); n != 1 {
		t.Errorf("want 1 cleared audit record, got %d", n)
	}
}

// Default healthy = any status < 500: a 404 on the probe path is "responding"
// and must never raise, however many times it's seen.
func TestAppProbe_DefaultHealthyAllowsClientErrors(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister, loader, reader := probeScenario(clk, "/healthz", nil, manifest.DefaultStartPeriod, 5*time.Minute)
	rt := &stubRoundTripper{status: map[string]int{"app.local": 404}}
	d, mgr, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("404 is responding (default <500); must not raise, got %v", mgr.List())
	}
}

// A narrowed healthy_status set ([200]) treats anything else — including a 204 —
// as unhealthy.
func TestAppProbe_NarrowHealthyStatus(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister, loader, reader := probeScenario(clk, "/healthz", []int{200}, manifest.DefaultStartPeriod, 5*time.Minute)
	rt := &stubRoundTripper{status: map[string]int{"app.local": 204}}
	d, mgr, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 1 {
		t.Fatalf("204 outside [200] must raise, got %v", mgr.List())
	}
}

// Within the start-period grace the probe is skipped entirely — a warming-up
// app doesn't flap the banner. Once the grace passes, failures count.
func TestAppProbe_StartPeriodSuppresses(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	// Started 10s ago, 60s grace → in grace.
	lister, loader, reader := probeScenario(clk, "/healthz", nil, manifest.DefaultStartPeriod, 10*time.Second)
	rt := &stubRoundTripper{status: map[string]int{"app.local": 503}}
	d, mgr, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("must not raise during start-period grace, got %v", mgr.List())
	}
	if rt.last != nil {
		t.Errorf("must not probe during grace; got a request to %v", rt.last.Host)
	}
	// Past the grace now: two failures raise.
	clk.add(time.Minute)
	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 1 {
		t.Fatalf("must raise once past the grace, got %v", mgr.List())
	}
}

// An app that declares no health_probe is never probed and never raises.
func TestAppProbe_NoProbeNeverRaised(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister := &fakeInstanceLister{instances: []store.Instance{{ID: "app", Slug: "app", MDNSName: "app.local"}}}
	loader := &fakeManifestLoader{m: map[string]*manifest.Manifest{
		"app": {MainService: "web"}, // no HealthProbe
	}}
	reader := &fakeContainerReader{containers: []lifecycle.ManagedContainer{
		{InstanceID: "app", Service: "web", Running: true, StartedAt: clk.now().Add(-5 * time.Minute)},
	}}
	rt := &stubRoundTripper{status: map[string]int{"app.local": 503}}
	d, mgr, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("opt-out app must never raise, got %v", mgr.List())
	}
	if rt.last != nil {
		t.Errorf("opt-out app must never be probed; got a request to %v", rt.last.Host)
	}
}

// A crash-looping app surfaces as container-restart-loop, not app-unresponsive:
// the probe defers to a live restart-loop issue and doesn't double-banner.
func TestAppProbe_CrashLooperNotDoubleBannered(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister, loader, reader := probeScenario(clk, "/healthz", nil, manifest.DefaultStartPeriod, 5*time.Minute)
	rt := &stubRoundTripper{status: map[string]int{"app.local": 503}}
	d, mgr, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)
	mgr.Raise("container-restart-loop", "app", "crash-looping") // restart-loop owns it

	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("crash-looper must not also raise app-unresponsive, got %v", mgr.List())
	}
	if rt.last != nil {
		t.Errorf("crash-looper must not be probed; got a request to %v", rt.last.Host)
	}
}

// A connection failure / timeout (no HTTP response at all) is unhealthy — a dead
// upstream behind Caddy reads the same as a 5xx.
func TestAppProbe_ConnectionFailureIsUnhealthy(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister, loader, reader := probeScenario(clk, "/healthz", nil, manifest.DefaultStartPeriod, 5*time.Minute)
	rt := &stubRoundTripper{fail: map[string]bool{"app.local": true}}
	d, mgr, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 1 {
		t.Fatalf("connection failure must raise after two samples, got %v", mgr.List())
	}
}

// A non-running main container is not steady-running → not probed, never raised.
func TestAppProbe_NotRunningSkipped(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister, loader, _ := probeScenario(clk, "/healthz", nil, manifest.DefaultStartPeriod, 5*time.Minute)
	reader := &fakeContainerReader{containers: []lifecycle.ManagedContainer{
		{InstanceID: "app", Service: "web", Running: false, StartedAt: clk.now().Add(-5 * time.Minute)},
	}}
	rt := &stubRoundTripper{status: map[string]int{"app.local": 503}}
	d, mgr, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("a stopped container must not raise app-unresponsive, got %v", mgr.List())
	}
	if rt.last != nil {
		t.Errorf("a stopped container must not be probed; got a request to %v", rt.last.Host)
	}
}

// Once raised, losing eligibility (here: uninstalled — gone from the instance
// list and from Docker) clears the issue.
func TestAppProbe_ClearsWhenUninstalled(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister, loader, reader := probeScenario(clk, "/healthz", nil, manifest.DefaultStartPeriod, 5*time.Minute)
	rt := &stubRoundTripper{status: map[string]int{"app.local": 503}}
	d, mgr, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 1 {
		t.Fatalf("expected raise before uninstall, got %v", mgr.List())
	}
	lister.instances = nil
	reader.containers = nil
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 0 {
		t.Fatalf("uninstalled app must clear app-unresponsive, got %v", mgr.List())
	}
}

// The probe goes to Caddy's listener with Host: <route host> and the manifest
// path — exactly the request a browser makes, never dialing the container.
func TestAppProbe_RoutesThroughCaddyByHost(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister, loader, reader := probeScenario(clk, "/healthz", nil, manifest.DefaultStartPeriod, 5*time.Minute)
	rt := &stubRoundTripper{status: map[string]int{"app.local": 200}}
	d, _, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	if rt.last == nil {
		t.Fatal("expected a probe request")
	}
	if rt.last.Host != "app.local" {
		t.Errorf("probe Host = %q, want app.local (route host)", rt.last.Host)
	}
	if got := rt.last.URL.String(); got != "http://caddy.test/healthz" {
		t.Errorf("probe URL = %q, want http://caddy.test/healthz (Caddy listener + path)", got)
	}
}

func TestProbeHealthy(t *testing.T) {
	cases := []struct {
		status  int
		allowed []int
		want    bool
	}{
		{200, nil, true},
		{404, nil, true}, // default <500 → "responding"
		{499, nil, true},
		{500, nil, false}, // 5xx → not responding
		{502, nil, false}, // Caddy's dead-upstream
		{200, []int{200}, true},
		{204, []int{200}, false},
		{200, []int{200, 204}, true},
		{500, []int{200, 500}, true}, // explicit set can include a 5xx
	}
	for _, c := range cases {
		if got := probeHealthy(c.status, c.allowed); got != c.want {
			t.Errorf("probeHealthy(%d, %v) = %v, want %v", c.status, c.allowed, got, c.want)
		}
	}
}

// When MDNSName is not set the probe Host falls back to slug + ".local".
func TestAppProbe_MDNSFallback(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister := &fakeInstanceLister{instances: []store.Instance{
		{ID: "app", Slug: "myapp", MDNSName: "", State: "running"},
	}}
	loader := &fakeManifestLoader{m: map[string]*manifest.Manifest{
		"app": probeManifest("web", "/healthz", nil, manifest.DefaultStartPeriod),
	}}
	reader := &fakeContainerReader{containers: []lifecycle.ManagedContainer{
		{InstanceID: "app", Service: "web", Running: true, StartedAt: clk.now().Add(-5 * time.Minute)},
	}}
	rt := &stubRoundTripper{status: map[string]int{"myapp.local": 200}}
	d, _, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	if rt.last == nil {
		t.Fatal("expected a probe request")
	}
	if rt.last.Host != "myapp.local" {
		t.Errorf("probe Host = %q, want myapp.local (slug fallback)", rt.last.Host)
	}
}

// On hosted the probe must address the public wildcard route host
// "<slug>.<box-id>.onmoose.network" — not "<slug>.local", which has no Caddy route
// and would 404 into a perpetual app-unresponsive flap. MDNSName is empty on
// hosted (no LAN to multicast on), so the appliance fallback would otherwise win.
func TestAppProbe_HostedWildcardHost(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister := &fakeInstanceLister{instances: []store.Instance{
		{ID: "app", Slug: "myapp", MDNSName: "", State: "running"},
	}}
	loader := &fakeManifestLoader{m: map[string]*manifest.Manifest{
		"app": probeManifest("web", "/healthz", nil, manifest.DefaultStartPeriod),
	}}
	reader := &fakeContainerReader{containers: []lifecycle.ManagedContainer{
		{InstanceID: "app", Service: "web", Running: true, StartedAt: clk.now().Add(-5 * time.Minute)},
	}}
	rt := &stubRoundTripper{status: map[string]int{"myapp.cindy-fox.onmoose.network": 200}}
	d, _, _, _ := newTestProbeDetector(lister, loader, reader, rt, clk)
	d.SetEnvironment(profile.Hosted, "cindy-fox")

	d.check(context.Background())
	if rt.last == nil {
		t.Fatal("expected a probe request")
	}
	if want := "myapp.cindy-fox.onmoose.network"; rt.last.Host != want {
		t.Errorf("probe Host = %q, want %q (hosted wildcard route host)", rt.last.Host, want)
	}
}

// app-unresponsive must not emit a bell notification (not in the notify
// allowlist in v1 — same as container-restart-loop). The health issue is still
// raised; only the bell is suppressed.
func TestAppProbe_RaiseEmitsNoBellNotification(t *testing.T) {
	clk := &stepClock{t: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)}
	lister, loader, reader := probeScenario(clk, "/healthz", nil, manifest.DefaultStartPeriod, 5*time.Minute)
	rt := &stubRoundTripper{status: map[string]int{"app.local": 503}}
	d, mgr, _, ns := newTestProbeDetector(lister, loader, reader, rt, clk)

	d.check(context.Background())
	d.check(context.Background())
	if len(unresponsiveIssues(mgr)) != 1 {
		t.Fatalf("expected raise, got %v", mgr.List())
	}
	if len(ns.raised) != 0 {
		t.Errorf("app-unresponsive must not emit a notification (not allowlisted), got %v", ns.raised)
	}
}

func TestProbeBaseURL(t *testing.T) {
	cases := []struct {
		listen string
		want   string
	}{
		{":80", "http://127.0.0.1:80"},
		{":8080", "http://127.0.0.1:8080"},
		{"0.0.0.0:80", "http://0.0.0.0:80"},
		{"127.0.0.1:3000", "http://127.0.0.1:3000"},
		{"caddy:80", "http://caddy:80"},   // containerised brain → Caddy service name
		{"bad-value", "http://127.0.0.1"}, // SplitHostPort failure → safe fallback (port 80)
	}
	for _, c := range cases {
		if got := probeBaseURL(c.listen); got != c.want {
			t.Errorf("probeBaseURL(%q) = %q, want %q", c.listen, got, c.want)
		}
	}
}

// TestTrustedProxiesConfig covers the env-var wiring behind the brain's client-IP
// trust boundary (#329): the sentinel distinguishes an unset MOOSE_TRUSTED_PROXIES
// (⇒ the private-range default) from one deliberately set to empty (⇒ trust no
// proxy at all, key every per-IP bucket on the peer address). Collapsing the two
// would silently hand back the default to an operator who asked for the opposite.
// The unparseable case is deliberately not covered: it calls fatal(), which exits
// the process.
func TestTrustedProxiesConfig(t *testing.T) {
	t.Run("unset falls back to the private-range default", func(t *testing.T) {
		t.Setenv("MOOSE_TRUSTED_PROXIES", "")
		if err := os.Unsetenv("MOOSE_TRUSTED_PROXIES"); err != nil {
			t.Fatalf("unsetenv: %v", err)
		}
		got := trustedProxies(loadConfig().trustedProxies)
		if want := api.DefaultTrustedProxies(); len(got) != len(want) {
			t.Fatalf("prefixes = %v, want the %d defaults", got, len(want))
		}
	})

	t.Run("explicitly empty trusts nothing", func(t *testing.T) {
		t.Setenv("MOOSE_TRUSTED_PROXIES", "")
		if got := trustedProxies(loadConfig().trustedProxies); len(got) != 0 {
			t.Fatalf("prefixes = %v, want none", got)
		}
	})

	t.Run("explicit spec is parsed", func(t *testing.T) {
		t.Setenv("MOOSE_TRUSTED_PROXIES", "172.18.0.0/16")
		got := trustedProxies(loadConfig().trustedProxies)
		if len(got) != 1 || got[0].String() != "172.18.0.0/16" {
			t.Fatalf("prefixes = %v, want [172.18.0.0/16]", got)
		}
	})
}
