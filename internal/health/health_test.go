package health

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/onmoose/moose/internal/protocol"
)

func TestRaise_NewIssueReturnsTrue(t *testing.T) {
	m := NewManager(nil)
	if !m.Raise("data-drive-missing", "", "abc-123 absent") {
		t.Fatal("first raise should return true (transition)")
	}
	if len(m.List()) != 1 {
		t.Fatalf("want 1 active issue, got %d", len(m.List()))
	}
}

func TestRaise_IdempotentNoTransition(t *testing.T) {
	m := NewManager(nil)
	m.Raise("data-drive-missing", "", "abc-123 absent")
	if m.Raise("data-drive-missing", "", "abc-123 still absent") {
		t.Fatal("second raise of same issue must return false (no transition)")
	}
}

func TestRaise_PreservesRaisedAtAcrossReRaises(t *testing.T) {
	m := NewManager(nil)
	first := time.Date(2026, 5, 25, 8, 0, 0, 0, time.UTC)
	second := time.Date(2026, 5, 25, 8, 1, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return first })
	m.Raise("data-drive-missing", "", "details A")
	m.SetClock(func() time.Time { return second })
	m.Raise("data-drive-missing", "", "details B")

	got := m.List()[0]
	if !got.RaisedAt.Equal(first) {
		t.Errorf("RaisedAt: want %v (preserved), got %v", first, got.RaisedAt)
	}
	if !got.LastCheckedAt.Equal(second) {
		t.Errorf("LastCheckedAt: want %v (updated), got %v", second, got.LastCheckedAt)
	}
	if got.Details != "details B" {
		t.Errorf("Details: want refreshed, got %q", got.Details)
	}
}

func TestRaise_UnknownIDIsSkipped(t *testing.T) {
	m := NewManager(nil)
	if m.Raise("not-a-real-issue-id", "", "") {
		t.Fatal("unknown ID should return false (no-op), got true")
	}
	if len(m.List()) != 0 {
		t.Fatal("unknown ID should not be added")
	}
}

func TestClear_ReturnsTrueOnTransition(t *testing.T) {
	m := NewManager(nil)
	m.Raise("data-drive-missing", "", "")
	if !m.Clear("data-drive-missing", "") {
		t.Fatal("clear of active issue should return true")
	}
	if m.Clear("data-drive-missing", "") {
		t.Fatal("second clear should return false (no-op)")
	}
}

func TestList_ReturnsStableOrder(t *testing.T) {
	m := NewManager(nil)
	m.Raise("canary-mismatch", "", "")         // critical
	m.Raise("data-drive-missing", "", "")      // error
	m.Raise("health-report-malformed", "", "") // error

	got := m.List()
	if len(got) != 3 {
		t.Fatalf("want 3, got %d", len(got))
	}
	// All storage category; critical first, then errors alphabetically by ID.
	wantOrder := []string{"canary-mismatch", "data-drive-missing", "health-report-malformed"}
	for i, w := range wantOrder {
		if got[i].ID != w {
			t.Errorf("List()[%d]: want %s, got %s", i, w, got[i].ID)
		}
	}
}

func TestList_DefinitionMetadataApplied(t *testing.T) {
	m := NewManager(nil)
	m.Raise("data-drive-missing", "", "")

	got := m.List()[0]
	if got.Severity != SeverityError {
		t.Errorf("Severity: want error, got %s", got.Severity)
	}
	if got.Tier != 1 {
		t.Errorf("Tier: want 1, got %d", got.Tier)
	}
	if !got.BlocksWrites || !got.BlocksApps || !got.BlocksUsers {
		t.Errorf("blocks_*: want all true for data-drive-missing, got %+v", got)
	}
	if got.Summary == "" {
		t.Error("Summary must be populated from definition")
	}
}

// TestList_VersionMismatchDefinition pins the registered metadata for the
// locus-C version-mismatch detector (HEALTH.md # Version): version category,
// error severity, Tier 2, and blocks apps only — not writes or users.
func TestList_VersionMismatchDefinition(t *testing.T) {
	m := NewManager(nil)
	m.Raise("version-mismatch", "", "agent 9.9.9 vs brain 0.0.1")

	got := m.List()[0]
	if got.Category != CategoryVersion {
		t.Errorf("Category: want version, got %s", got.Category)
	}
	if got.Severity != SeverityError {
		t.Errorf("Severity: want error, got %s", got.Severity)
	}
	if got.Tier != 2 {
		t.Errorf("Tier: want 2, got %d", got.Tier)
	}
	if !got.BlocksApps || got.BlocksWrites || got.BlocksUsers {
		t.Errorf("blocks_*: want only apps for version-mismatch, got %+v", got)
	}
	if got.Summary == "" {
		t.Error("Summary must be populated from definition")
	}
}

// TestList_BrainDBCorruptDefinition pins the metadata bound to the
// brain-db-corrupt issue (HEALTH.md # State): a critical, Tier-2, state-category
// issue that blocks every mutation class ("nearly all ops").
func TestList_BrainDBCorruptDefinition(t *testing.T) {
	m := NewManager(nil)
	m.Raise("brain-db-corrupt", "", "")

	got := m.List()[0]
	if got.Category != CategoryState {
		t.Errorf("Category: want state, got %s", got.Category)
	}
	if got.Severity != SeverityCritical {
		t.Errorf("Severity: want critical, got %s", got.Severity)
	}
	if got.Tier != 2 {
		t.Errorf("Tier: want 2, got %d", got.Tier)
	}
	if !got.BlocksWrites || !got.BlocksApps || !got.BlocksUsers {
		t.Errorf("blocks_*: want all true (blocks nearly all ops), got %+v", got)
	}
	if got.Summary == "" {
		t.Error("Summary must be populated from definition")
	}
}

// container-restart-loop (issue #35) is a per-app, advisory detector: warning
// severity, Tier-2 (view logs / stop the app), CategoryVersion, and it blocks
// nothing — the app is already failing, so we surface it rather than gate. The
// instance_key carries the owning instance_id, so the Issue echoes it back.
func TestList_ContainerRestartLoopDefinition(t *testing.T) {
	m := NewManager(nil)
	if !m.Raise("container-restart-loop", "immich--abby", "restarted 6 times") {
		t.Fatal("first raise should transition")
	}

	got := m.List()[0]
	if got.InstanceKey != "immich--abby" {
		t.Errorf("InstanceKey: want immich--abby (per-app keying), got %q", got.InstanceKey)
	}
	if got.Category != CategoryVersion {
		t.Errorf("Category: want version, got %s", got.Category)
	}
	if got.Severity != SeverityWarning {
		t.Errorf("Severity: want warning, got %s", got.Severity)
	}
	if got.Tier != 2 {
		t.Errorf("Tier: want 2, got %d", got.Tier)
	}
	if got.BlocksWrites || got.BlocksApps || got.BlocksUsers {
		t.Errorf("blocks_*: want all false (advisory), got %+v", got)
	}
	if got.Summary == "" {
		t.Error("Summary must be populated from definition")
	}
}

func TestApplyStorageFindings_RaiseAndClear(t *testing.T) {
	m := NewManager(nil)

	// First poll: data drive missing.
	raised, cleared := m.ApplyFindings(protocol.HealthCategoryStorage,
		[]protocol.Finding{{ID: "data-drive-missing", Details: "x"}})
	if len(raised) != 1 || len(cleared) != 0 {
		t.Errorf("first poll: want raised=1 cleared=0, got %d/%d", len(raised), len(cleared))
	}
	if len(m.List()) != 1 {
		t.Fatalf("active: want 1, got %d", len(m.List()))
	}

	// Second poll: drive reattached, no findings.
	raised, cleared = m.ApplyFindings(protocol.HealthCategoryStorage, []protocol.Finding{})
	if len(raised) != 0 || len(cleared) != 1 {
		t.Errorf("clear poll: want raised=0 cleared=1, got %d/%d", len(raised), len(cleared))
	}
	if len(m.List()) != 0 {
		t.Fatalf("active: want 0 after clear, got %d", len(m.List()))
	}
}

// TestApplyFindings_ReturnsAffectedKeys pins the per-issue return contract the
// audit hook depends on: the returned slices carry the exact issue keys that
// transitioned (not a count), sorted by ID for a stable per-issue audit-record
// order.
//
// Three findings (not two) in non-sorted input order make the sort
// load-bearing: with only two keys, Go's map-iteration randomization is weak
// enough that an implementation missing the sort would still pass too often
// to guard anything.
func TestApplyFindings_ReturnsAffectedKeys(t *testing.T) {
	m := NewManager(nil)

	// First poll raises three issues, supplied in non-sorted order. The
	// returned slice must come back sorted by ID.
	raised, cleared := m.ApplyFindings(protocol.HealthCategoryStorage,
		[]protocol.Finding{
			{ID: "mergerfs-assembly-failed"},
			{ID: "data-drive-missing"},
			{ID: "canary-mismatch"},
		})
	if len(cleared) != 0 {
		t.Errorf("first poll: want 0 cleared, got %v", cleared)
	}
	wantRaised := []IssueKey{
		{ID: "canary-mismatch"},
		{ID: "data-drive-missing"},
		{ID: "mergerfs-assembly-failed"},
	}
	if !reflect.DeepEqual(raised, wantRaised) {
		t.Errorf("first poll raised: want %v, got %v", wantRaised, raised)
	}

	// Second poll keeps canary-mismatch, drops the other two: no raises, two
	// cleared keys, also returned sorted by ID.
	raised, cleared = m.ApplyFindings(protocol.HealthCategoryStorage,
		[]protocol.Finding{{ID: "canary-mismatch"}})
	if len(raised) != 0 {
		t.Errorf("second poll: want 0 raised, got %v", raised)
	}
	wantCleared := []IssueKey{
		{ID: "data-drive-missing"},
		{ID: "mergerfs-assembly-failed"},
	}
	if !reflect.DeepEqual(cleared, wantCleared) {
		t.Errorf("second poll cleared: want %v, got %v", wantCleared, cleared)
	}
}

// TestSortIssueKeys_TieBreakOnInstanceKey pins both sort dimensions: primary
// by ID, tie-broken by InstanceKey. service-down now exercises the InstanceKey
// path through ApplyFindings (per-unit findings); this exercises the sort
// directly to keep both dimensions pinned independently of any one caller.
func TestSortIssueKeys_TieBreakOnInstanceKey(t *testing.T) {
	ks := []IssueKey{
		{ID: "data-drive-missing", InstanceKey: "b"},
		{ID: "data-drive-missing", InstanceKey: "a"},
		{ID: "canary-mismatch", InstanceKey: "z"},
	}
	sortIssueKeys(ks)
	want := []IssueKey{
		{ID: "canary-mismatch", InstanceKey: "z"},
		{ID: "data-drive-missing", InstanceKey: "a"},
		{ID: "data-drive-missing", InstanceKey: "b"},
	}
	if !reflect.DeepEqual(ks, want) {
		t.Errorf("sorted keys: want %v, got %v", want, ks)
	}
}

func TestApplyFindings_OnlyTouchesItsReportCategory(t *testing.T) {
	// A storage poll reconciles only issues whose ReportCategory is storage.
	// An issue in another report domain — here a network issue with no
	// ReportCategory set — must survive an empty storage payload. The scoping
	// is by ReportCategory, not display Category.
	m := NewManager(nil)
	// Pretend a network issue is active by writing directly through Raise
	// after registering a definition for it.
	m.mu.Lock()
	m.definitions["mdns-down"] = Definition{
		ID: "mdns-down", Category: CategoryNetwork,
		Severity: SeverityWarning, Tier: 1,
		Summary: "mDNS is not publishing.",
	}
	m.mu.Unlock()
	m.Raise("mdns-down", "", "")

	m.ApplyFindings(protocol.HealthCategoryStorage, []protocol.Finding{})

	if len(m.List()) != 1 || m.List()[0].ID != "mdns-down" {
		t.Errorf("issue in another report domain must survive storage reconciliation, got %v", m.List())
	}
}

// TestApplyFindings_AtomicAcrossClearAndRaise pins down the locking contract: a
// concurrent List() during a reconcile cycle must never observe the moment
// where the old issue has been cleared but the new one hasn't yet been raised.
// The pre-fix implementation snapshotted under the lock and then dropped it
// before reconciling, which exposed this transient.
func TestApplyFindings_AtomicAcrossClearAndRaise(t *testing.T) {
	m := NewManager(nil)
	// Seed initial state: one storage issue active.
	m.ApplyFindings(protocol.HealthCategoryStorage,
		[]protocol.Finding{{ID: "data-drive-missing"}})

	done := make(chan struct{})
	stop := make(chan struct{})
	// Hammer ApplyFindings alternating between two findings so each cycle
	// clears the previous and raises a new one.
	go func() {
		defer close(done)
		flip := false
		for {
			select {
			case <-stop:
				return
			default:
			}
			var id string
			if flip {
				id = "data-drive-missing"
			} else {
				id = "canary-mismatch"
			}
			flip = !flip
			m.ApplyFindings(protocol.HealthCategoryStorage,
				[]protocol.Finding{{ID: id}})
		}
	}()

	// Concurrently List() many times; every observation must show exactly
	// one storage issue (never zero — that would be the torn state).
	for i := 0; i < 10000; i++ {
		got := m.List()
		count := 0
		for _, iss := range got {
			if iss.Category == CategoryStorage {
				count++
			}
		}
		if count != 1 {
			close(stop)
			<-done
			t.Fatalf("torn state observed on iteration %d: want exactly 1 storage issue, got %d (%v)", i, count, got)
		}
	}
	close(stop)
	<-done
}

func TestApplyFindings_UnknownIDsAreDropped(t *testing.T) {
	m := NewManager(nil)
	raised, cleared := m.ApplyFindings(protocol.HealthCategoryStorage,
		[]protocol.Finding{
			{ID: "data-drive-missing"},
			{ID: "not-a-real-issue-id"},
		})
	if len(raised) != 1 || raised[0].ID != "data-drive-missing" {
		t.Errorf("raised: want [data-drive-missing] (the known ID), got %v", raised)
	}
	if len(cleared) != 0 {
		t.Errorf("cleared: want 0, got %d", len(cleared))
	}
	if len(m.List()) != 1 || m.List()[0].ID != "data-drive-missing" {
		t.Fatalf("only the known finding should land in the registry, got %v", m.List())
	}
}

func TestApplyFindings_ReplacesDetailsOnRefresh(t *testing.T) {
	m := NewManager(nil)
	m.ApplyFindings(protocol.HealthCategoryStorage,
		[]protocol.Finding{{ID: "data-drive-missing", Details: "first"}})
	m.ApplyFindings(protocol.HealthCategoryStorage,
		[]protocol.Finding{{ID: "data-drive-missing", Details: "second"}})
	got := m.List()
	if len(got) != 1 || got[0].Details != "second" {
		t.Errorf("Details: want refreshed to 'second', got %v", got)
	}
}

// --- service-down + cross-category reconcile (issue #34) ---

// TestApplyFindings_ServiceDownDebounces pins the locus-B anti-flap default
// (HEALTH.md # Cross-cutting detector policy): a debounced issue raises only on
// its 2nd consecutive bad sample and clears on 1 good sample. A single bad
// sample must leave the registry untouched so a service that blips for one poll
// never banners.
func TestApplyFindings_ServiceDownDebounces(t *testing.T) {
	m := NewManager(nil)
	down := []protocol.Finding{
		{ID: "service-down", InstanceKey: "docker.service", Details: "docker.service is failed"},
	}

	// First bad sample: counted, not raised.
	raised, cleared := m.ApplyFindings(protocol.HealthCategoryServices, down)
	if len(raised) != 0 || len(cleared) != 0 {
		t.Fatalf("first bad sample: want no transition (debounced), got raised=%d cleared=%d", len(raised), len(cleared))
	}
	if len(m.List()) != 0 {
		t.Fatalf("debounced issue must not be active after one sample, got %v", m.List())
	}

	// Second consecutive bad sample: raises.
	raised, cleared = m.ApplyFindings(protocol.HealthCategoryServices, down)
	if len(raised) != 1 || raised[0].ID != "service-down" || raised[0].InstanceKey != "docker.service" {
		t.Fatalf("second bad sample: want service-down/docker.service raised, got %v", raised)
	}
	if len(m.List()) != 1 || m.List()[0].Category != CategoryState {
		t.Fatalf("want one active state-category issue after 2nd sample, got %v", m.List())
	}

	// One good sample: clears immediately (asymmetric — fast to reassure).
	raised, cleared = m.ApplyFindings(protocol.HealthCategoryServices, nil)
	if len(cleared) != 1 || cleared[0].InstanceKey != "docker.service" {
		t.Fatalf("good sample: want service-down cleared, got %v", cleared)
	}
	if len(m.List()) != 0 {
		t.Fatalf("want 0 active after recovery, got %v", m.List())
	}
}

// TestApplyFindings_DebounceResetsOnGoodSample verifies the debounce counter is
// consecutive: a good sample between two bad ones resets it, so the issue needs
// two fresh consecutive bad samples to raise — it does not raise on the next
// bad sample alone.
func TestApplyFindings_DebounceResetsOnGoodSample(t *testing.T) {
	m := NewManager(nil)
	down := []protocol.Finding{{ID: "service-down", InstanceKey: "caddy.service"}}

	m.ApplyFindings(protocol.HealthCategoryServices, down) // bad #1 → pending
	m.ApplyFindings(protocol.HealthCategoryServices, nil)  // good → reset
	if r, _ := m.ApplyFindings(protocol.HealthCategoryServices, down); len(r) != 0 {
		t.Fatalf("bad sample after a reset must not raise (counter restarted), got %v", r)
	}
	if len(m.List()) != 0 {
		t.Fatalf("still debouncing — want 0 active, got %v", m.List())
	}
	r, _ := m.ApplyFindings(protocol.HealthCategoryServices, down) // bad #2 consecutive → raise
	if len(r) != 1 {
		t.Fatalf("second consecutive bad sample must raise, got %v", r)
	}
}

// TestApplyFindings_ClockNotSyncedDebounces is the locus-B reconcile for the
// clock-not-synced detector (issue #39): a finding under the time category
// debounces (raise on the 2nd consecutive bad sample), surfaces as a
// network-category warning, and clears on one good sample (TIME.md # Drift
// monitoring, HEALTH.md # Network). Mirrors the fake reporter producing both a
// bad and a good state.
func TestApplyFindings_ClockNotSyncedDebounces(t *testing.T) {
	m := NewManager(nil)
	bad := []protocol.Finding{{ID: "clock-not-synced", Details: "last synced 7h0m0s ago"}}

	// First bad sample: counted, not raised (debounce).
	if raised, _ := m.ApplyFindings(protocol.HealthCategoryTime, bad); len(raised) != 0 {
		t.Fatalf("first bad sample: want no transition (debounced), got %v", raised)
	}
	if len(m.List()) != 0 {
		t.Fatalf("debounced clock issue must not be active after one sample, got %v", m.List())
	}

	// Second consecutive bad sample: raises a network-category warning.
	raised, _ := m.ApplyFindings(protocol.HealthCategoryTime, bad)
	if len(raised) != 1 || raised[0].ID != "clock-not-synced" || raised[0].InstanceKey != "" {
		t.Fatalf("second bad sample: want box-wide clock-not-synced raised, got %v", raised)
	}
	if l := m.List(); len(l) != 1 || l[0].Category != CategoryNetwork || l[0].Severity != SeverityWarning {
		t.Fatalf("want one active network/warning issue, got %v", l)
	}

	// One good sample (clock synced): clears.
	if _, cleared := m.ApplyFindings(protocol.HealthCategoryTime, nil); len(cleared) != 1 {
		t.Fatalf("good sample: want clock-not-synced cleared, got cleared=%d", len(cleared))
	}
	if len(m.List()) != 0 {
		t.Fatalf("want 0 active after the clock resyncs, got %v", m.List())
	}
}

// TestApplyFindings_RAMPressureDebounces is the locus-B reconcile for the
// ram-pressure detector (issue #38): a finding under the resources category
// debounces (raise on the 2nd consecutive bad sample), surfaces as a
// capacity-category warning, and clears on one good sample (HEALTH.md
// # Capacity & informational). Mirrors the host-agent reporter producing both a
// high-pressure and a below-threshold state.
func TestApplyFindings_RAMPressureDebounces(t *testing.T) {
	m := NewManager(nil)
	bad := []protocol.Finding{{ID: "ram-pressure", Details: "memory stall 34% over the last 60s"}}

	// First bad sample: counted, not raised (debounce).
	if raised, _ := m.ApplyFindings(protocol.HealthCategoryResources, bad); len(raised) != 0 {
		t.Fatalf("first bad sample: want no transition (debounced), got %v", raised)
	}
	if len(m.List()) != 0 {
		t.Fatalf("debounced ram-pressure must not be active after one sample, got %v", m.List())
	}

	// Second consecutive bad sample: raises a capacity-category warning, box-wide.
	raised, _ := m.ApplyFindings(protocol.HealthCategoryResources, bad)
	if len(raised) != 1 || raised[0].ID != "ram-pressure" || raised[0].InstanceKey != "" {
		t.Fatalf("second bad sample: want box-wide ram-pressure raised, got %v", raised)
	}
	if l := m.List(); len(l) != 1 || l[0].Category != CategoryCapacity || l[0].Severity != SeverityWarning {
		t.Fatalf("want one active capacity/warning issue, got %v", l)
	}

	// One good sample (pressure dropped below threshold): clears.
	if _, cleared := m.ApplyFindings(protocol.HealthCategoryResources, nil); len(cleared) != 1 {
		t.Fatalf("good sample: want ram-pressure cleared, got cleared=%d", len(cleared))
	}
	if len(m.List()) != 0 {
		t.Fatalf("want 0 active after pressure subsides, got %v", m.List())
	}
}

// TestApplyFindings_RebootRequiredOneShot is the locus-B reconcile for the
// reboot-required detector (issue #40): a finding under the *system* category
// is 1-shot (raises on the first sample — Debounce false, unlike the noisy
// resources/services/time siblings), surfaces as a box-wide capacity-category
// *info* issue with the pending package list in Details, and clears on one good
// sample (HEALTH.md # Capacity & informational; self-clears on reboot).
func TestApplyFindings_RebootRequiredOneShot(t *testing.T) {
	m := NewManager(nil)
	pending := []protocol.Finding{{ID: "reboot-required", Details: "linux-image-6.8.0, libc6"}}

	// First sample raises immediately — no debounce gate for this detector.
	raised, _ := m.ApplyFindings(protocol.HealthCategorySystem, pending)
	if len(raised) != 1 || raised[0].ID != "reboot-required" || raised[0].InstanceKey != "" {
		t.Fatalf("first sample: want box-wide reboot-required raised 1-shot, got %v", raised)
	}
	l := m.List()
	if len(l) != 1 || l[0].Category != CategoryCapacity || l[0].Severity != SeverityInfo {
		t.Fatalf("want one active capacity/info issue, got %v", l)
	}
	if l[0].Details != "linux-image-6.8.0, libc6" {
		t.Errorf("Details: want the package list carried through, got %q", l[0].Details)
	}

	// The flag is gone (reboot happened / file absent): clears.
	if _, cleared := m.ApplyFindings(protocol.HealthCategorySystem, nil); len(cleared) != 1 {
		t.Fatalf("good sample: want reboot-required cleared, got cleared=%d", len(cleared))
	}
	if len(m.List()) != 0 {
		t.Fatalf("want 0 active after the flag clears, got %v", m.List())
	}
}

// TestList_RebootRequiredDefinition pins reboot-required's static metadata
// (issue #40, HEALTH.md # Capacity & informational): the first info-severity
// issue, Tier-2 (reboot now / schedule), CategoryCapacity, box-wide, and it
// blocks nothing — a pending reboot is purely informational.
func TestList_RebootRequiredDefinition(t *testing.T) {
	m := NewManager(nil)
	if !m.Raise("reboot-required", "", "linux-image-6.8.0") {
		t.Fatal("first raise should transition")
	}

	got := m.List()[0]
	if got.InstanceKey != "" {
		t.Errorf("InstanceKey: want empty (box-wide), got %q", got.InstanceKey)
	}
	if got.Category != CategoryCapacity {
		t.Errorf("Category: want capacity, got %s", got.Category)
	}
	if got.Severity != SeverityInfo {
		t.Errorf("Severity: want info, got %s", got.Severity)
	}
	if got.Tier != 2 {
		t.Errorf("Tier: want 2, got %d", got.Tier)
	}
	if got.BlocksWrites || got.BlocksApps || got.BlocksUsers {
		t.Errorf("blocks_*: want all false (informational), got %+v", got)
	}
	if got.Summary == "" {
		t.Error("Summary must be populated from definition")
	}
}

// TestApplyFindings_StoragePollLeavesServiceDownAlone is the locked
// cross-category isolation property (issue #34): reconciling one report
// category must never clear an active issue belonging to another. A storage
// poll with no findings must leave an active service-down (ReportCategory
// services) untouched.
func TestApplyFindings_StoragePollLeavesServiceDownAlone(t *testing.T) {
	m := NewManager(nil)
	down := []protocol.Finding{{ID: "service-down", InstanceKey: "docker.service"}}
	// Raise service-down (two samples — it debounces).
	m.ApplyFindings(protocol.HealthCategoryServices, down)
	m.ApplyFindings(protocol.HealthCategoryServices, down)
	if len(m.List()) != 1 {
		t.Fatalf("setup: want service-down active, got %v", m.List())
	}

	raised, cleared := m.ApplyFindings(protocol.HealthCategoryStorage, nil)
	if len(raised) != 0 || len(cleared) != 0 {
		t.Fatalf("a storage poll must not transition a service issue, got raised=%v cleared=%v", raised, cleared)
	}
	if len(m.List()) != 1 || m.List()[0].ID != "service-down" {
		t.Fatalf("service-down must survive a storage poll, got %v", m.List())
	}
}

// TestApplyFindings_ServicesPollLeavesStoreWriteFailedAlone protects
// brain-owned issues. store-write-failed has no ReportCategory and shares the
// display Category state with service-down; a services poll that clears
// service-down must leave store-write-failed (locus C, brain-owned) untouched —
// the empty-ReportCategory filter, not the display Category, is what guards it.
func TestApplyFindings_ServicesPollLeavesStoreWriteFailedAlone(t *testing.T) {
	m := NewManager(nil)
	m.mu.Lock()
	m.raiseLocked("store-write-failed", "", "simulated")
	m.mu.Unlock()

	_, cleared := m.ApplyFindings(protocol.HealthCategoryServices, nil)
	if len(cleared) != 0 {
		t.Fatalf("a services poll must not clear a brain-owned issue, got %v", cleared)
	}
	if len(m.List()) != 1 || m.List()[0].ID != "store-write-failed" {
		t.Fatalf("store-write-failed (no ReportCategory) must survive a services poll, got %v", m.List())
	}
}

// TestApplyFindings_RefreshesLastCheckedWithoutTransition pins the
// "last-checked is always fresh" policy (HEALTH.md # Cross-cutting detector
// policy): a steady-state re-poll of an already-active issue updates
// LastCheckedAt (so the dashboard can show "checked 30s ago") while preserving
// RaisedAt and emitting no transition.
func TestApplyFindings_RefreshesLastCheckedWithoutTransition(t *testing.T) {
	m := NewManager(nil)
	t0 := time.Date(2026, 5, 31, 8, 0, 0, 0, time.UTC)
	t1 := t0.Add(60 * time.Second)
	f := []protocol.Finding{{ID: "data-drive-missing"}}

	m.SetClock(func() time.Time { return t0 })
	m.ApplyFindings(protocol.HealthCategoryStorage, f)

	m.SetClock(func() time.Time { return t1 })
	raised, cleared := m.ApplyFindings(protocol.HealthCategoryStorage, f)
	if len(raised) != 0 || len(cleared) != 0 {
		t.Fatalf("steady-state re-poll must not transition, got raised=%d cleared=%d", len(raised), len(cleared))
	}
	got := m.List()[0]
	if !got.LastCheckedAt.Equal(t1) {
		t.Errorf("LastCheckedAt: want refreshed to %v, got %v", t1, got.LastCheckedAt)
	}
	if !got.RaisedAt.Equal(t0) {
		t.Errorf("RaisedAt: want preserved at %v, got %v", t0, got.RaisedAt)
	}
}

// --- HealthStore integration tests ---

// fakeStore is a HealthStore stub for tests that need to inspect calls without
// a real SQLite database.
type fakeStore struct {
	issues  map[string]Issue // key: id+":"+instanceKey
	upserts int
	deletes int
	listErr error
}

func newFakeStore() *fakeStore {
	return &fakeStore{issues: map[string]Issue{}}
}

func (f *fakeStore) key(id, instanceKey string) string { return id + ":" + instanceKey }

func (f *fakeStore) UpsertHealthIssue(h Issue) error {
	f.upserts++
	f.issues[f.key(h.ID, h.InstanceKey)] = h
	return nil
}

func (f *fakeStore) DeleteHealthIssue(id, instanceKey string) error {
	f.deletes++
	delete(f.issues, f.key(id, instanceKey))
	return nil
}

func (f *fakeStore) BatchUpsertAndDelete(upserts []Issue, deletes []IssueKey) error {
	for _, h := range upserts {
		f.upserts++
		f.issues[f.key(h.ID, h.InstanceKey)] = h
	}
	for _, k := range deletes {
		f.deletes++
		delete(f.issues, f.key(k.ID, k.InstanceKey))
	}
	return nil
}

func (f *fakeStore) ListHealthIssues() ([]Issue, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]Issue, 0, len(f.issues))
	for _, iss := range f.issues {
		out = append(out, iss)
	}
	return out, nil
}

func TestManager_RaisePersistsToStore(t *testing.T) {
	fs := newFakeStore()
	m := NewManager(fs)
	m.Raise("data-drive-missing", "", "details")
	if fs.upserts != 1 {
		t.Fatalf("want 1 upsert on first raise, got %d", fs.upserts)
	}
	if _, ok := fs.issues["data-drive-missing:"]; !ok {
		t.Error("upserted issue not in fake store")
	}
}

func TestManager_ReRaisePersistsRefresh(t *testing.T) {
	fs := newFakeStore()
	m := NewManager(fs)
	m.Raise("data-drive-missing", "", "first")
	m.Raise("data-drive-missing", "", "second")
	// Both first raise and re-raise should upsert (to keep last_checked_at current).
	if fs.upserts != 2 {
		t.Fatalf("want 2 upserts (new + refresh), got %d", fs.upserts)
	}
	if fs.issues["data-drive-missing:"].Details != "second" {
		t.Errorf("store should have refreshed Details, got %q", fs.issues["data-drive-missing:"].Details)
	}
}

func TestManager_ClearDeletesFromStore(t *testing.T) {
	fs := newFakeStore()
	m := NewManager(fs)
	m.Raise("data-drive-missing", "", "")
	m.Clear("data-drive-missing", "")
	if fs.deletes != 1 {
		t.Fatalf("want 1 delete on clear, got %d", fs.deletes)
	}
	if _, ok := fs.issues["data-drive-missing:"]; ok {
		t.Error("issue should be gone from fake store after clear")
	}
}

func TestManager_ClearNoTransitionNoDelete(t *testing.T) {
	fs := newFakeStore()
	m := NewManager(fs)
	// Clear of an issue that was never raised: no delete call.
	m.Clear("data-drive-missing", "")
	if fs.deletes != 0 {
		t.Fatalf("want 0 deletes for clear-of-nothing, got %d", fs.deletes)
	}
}

func TestManager_LoadFromStore(t *testing.T) {
	fs := newFakeStore()
	// Pre-populate the fake store as if issues survived from a previous brain run.
	_ = fs.UpsertHealthIssue(Issue{
		ID: "data-drive-missing", InstanceKey: "",
		Category: CategoryStorage, Severity: SeverityError, Tier: 1,
		BlocksWrites: true, BlocksApps: true, BlocksUsers: true,
		Summary: "pre-existing", Details: "from last boot",
		RaisedAt:      time.Now().UTC(),
		LastCheckedAt: time.Now().UTC(),
	})
	fs.upserts = 0 // reset counter after pre-population

	m := NewManager(fs)
	if err := m.LoadFromStore(); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	active := m.List()
	if len(active) != 1 {
		t.Fatalf("want 1 active issue after load, got %d", len(active))
	}
	if active[0].ID != "data-drive-missing" {
		t.Errorf("ID: want data-drive-missing, got %s", active[0].ID)
	}
	if active[0].Details != "from last boot" {
		t.Errorf("Details: want 'from last boot', got %q", active[0].Details)
	}
}

func TestManager_LoadFromStore_NilStoreSafe(t *testing.T) {
	m := NewManager(nil)
	if err := m.LoadFromStore(); err != nil {
		t.Fatalf("LoadFromStore with nil store: %v", err)
	}
	if len(m.List()) != 0 {
		t.Error("nil-store load should leave registry empty")
	}
}

func TestManager_LoadFromStore_StoreError(t *testing.T) {
	fs := newFakeStore()
	fs.listErr = errors.New("disk failure")
	m := NewManager(fs)
	if err := m.LoadFromStore(); err == nil {
		t.Fatal("want error from LoadFromStore when store fails, got nil")
	}
}

func TestManager_NilStore_RaiseAndClearSafe(t *testing.T) {
	m := NewManager(nil)
	// Must not panic when store is nil.
	if !m.Raise("data-drive-missing", "", "") {
		t.Error("Raise should return true on first raise")
	}
	if !m.Clear("data-drive-missing", "") {
		t.Error("Clear should return true on transition")
	}
}

// TestManager_ConcurrentRaiseClear_NoGhostInStore pins down the lock-through-store
// fix: a concurrent Clear must not leave a ghost row that a concurrent Raise
// re-inserts after the delete completes. With the lock held through store calls,
// Raise and Clear are fully serialized — the winner's store state is final.
func TestManager_ConcurrentRaiseClear_NoGhostInStore(t *testing.T) {
	const iterations = 2000
	fs := newFakeStore()
	m := NewManager(fs)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < iterations; i++ {
			m.Raise("data-drive-missing", "", "raising")
			m.Clear("data-drive-missing", "")
		}
	}()
	for i := 0; i < iterations; i++ {
		m.Clear("data-drive-missing", "")
		m.Raise("data-drive-missing", "", "raising")
	}
	<-done

	// After all concurrent Raise/Clear pairs finish, in-memory and store must agree:
	// if the issue is in m.active it must be in the store, and vice versa.
	active := m.List()
	_, inStore := fs.issues["data-drive-missing:"]
	inMemory := len(active) > 0
	if inMemory != inStore {
		t.Fatalf("memory/store mismatch: in_memory=%v in_store=%v (ghost issue)", inMemory, inStore)
	}
}

func TestManager_LoadFromStore_SkipsUnknownIDs(t *testing.T) {
	fs := newFakeStore()
	// Simulate a stale row from a previous brain version with a renamed issue ID.
	_ = fs.UpsertHealthIssue(Issue{
		ID: "old-issue-id-from-v0", InstanceKey: "",
		Category: CategoryStorage, Severity: SeverityWarning, Tier: 1,
		Summary:       "stale",
		RaisedAt:      time.Now().UTC(),
		LastCheckedAt: time.Now().UTC(),
	})
	_ = fs.UpsertHealthIssue(Issue{
		ID: "data-drive-missing", InstanceKey: "",
		Category: CategoryStorage, Severity: SeverityError, Tier: 1,
		BlocksWrites: true, BlocksApps: true, BlocksUsers: true,
		Summary:       "known",
		RaisedAt:      time.Now().UTC(),
		LastCheckedAt: time.Now().UTC(),
	})
	fs.upserts = 0

	m := NewManager(fs)
	if err := m.LoadFromStore(); err != nil {
		t.Fatalf("LoadFromStore: %v", err)
	}
	active := m.List()
	if len(active) != 1 {
		t.Fatalf("want only the known issue loaded, got %d issues: %v", len(active), active)
	}
	if active[0].ID != "data-drive-missing" {
		t.Errorf("wrong issue loaded: %s", active[0].ID)
	}
}

func TestApplyFindings_PersistsToStore(t *testing.T) {
	fs := newFakeStore()
	m := NewManager(fs)
	m.ApplyFindings(protocol.HealthCategoryStorage,
		[]protocol.Finding{{ID: "data-drive-missing", Details: "x"}})
	if _, ok := fs.issues["data-drive-missing:"]; !ok {
		t.Error("ApplyFindings should upsert raised finding to store")
	}

	m.ApplyFindings(protocol.HealthCategoryStorage, []protocol.Finding{})
	if _, ok := fs.issues["data-drive-missing:"]; ok {
		t.Error("ApplyFindings should delete cleared finding from store")
	}
	if fs.deletes != 1 {
		t.Errorf("want 1 delete after clear, got %d", fs.deletes)
	}
}

// fakeErrStore is a HealthStore that always fails writes, for testing
// the store-write-failed signal.
type fakeErrStore struct {
	fakeStore
	writeErr error
}

func (f *fakeErrStore) UpsertHealthIssue(h Issue) error                { return f.writeErr }
func (f *fakeErrStore) DeleteHealthIssue(id, instanceKey string) error { return f.writeErr }
func (f *fakeErrStore) BatchUpsertAndDelete(upserts []Issue, deletes []IssueKey) error {
	return f.writeErr
}

func TestManager_RaiseStoreError_RaisesStoreWriteFailed(t *testing.T) {
	es := &fakeErrStore{writeErr: errors.New("disk full")}
	es.issues = map[string]Issue{}
	m := NewManager(es)
	m.Raise("data-drive-missing", "", "missing")

	active := m.List()
	var found bool
	for _, iss := range active {
		if iss.ID == "store-write-failed" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want store-write-failed in active issues after store error, got %v", active)
	}
}

func TestManager_RaiseStoreRecovers_ClearsStoreWriteFailed(t *testing.T) {
	// First raise with a broken store.
	es := &fakeErrStore{writeErr: errors.New("disk full")}
	es.issues = map[string]Issue{}
	m := NewManager(es)
	m.Raise("data-drive-missing", "", "missing")

	hasStoreFailure := func() bool {
		for _, iss := range m.List() {
			if iss.ID == "store-write-failed" {
				return true
			}
		}
		return false
	}
	if !hasStoreFailure() {
		t.Fatal("want store-write-failed after first broken-store raise")
	}

	// Swap to a working store and raise again — should clear store-write-failed.
	fs := newFakeStore()
	m.store = fs
	m.Raise("canary-mismatch", "", "mismatch")

	if hasStoreFailure() {
		t.Error("store-write-failed should clear once a store write succeeds")
	}
}

func TestManager_StoreWriteFailedIsNoPersist(t *testing.T) {
	// store-write-failed must never be persisted, even when the store is healthy.
	// Simulate by verifying UpsertHealthIssue is not called for it.
	fs := newFakeStore()
	m := NewManager(fs)

	// Manually raise store-write-failed as if the store had failed.
	m.mu.Lock()
	m.raiseLocked("store-write-failed", "", "simulated")
	m.mu.Unlock()

	// Now Clear it; should not call store.Delete.
	m.Clear("store-write-failed", "")
	if fs.deletes != 0 {
		t.Errorf("Clear of NoPersist issue must not call store.Delete, got %d deletes", fs.deletes)
	}
}

func TestCategoryCapacityConstantExists(t *testing.T) {
	// Regression guard: capacity must be in the Category type per HEALTH.md.
	if CategoryCapacity != "capacity" {
		t.Errorf("CategoryCapacity: want %q, got %q", "capacity", CategoryCapacity)
	}
}

func TestGet_ReturnsActiveIssue(t *testing.T) {
	m := NewManager(nil)
	m.Raise("data-drive-missing", "", "abc-123 absent")

	iss, ok := m.Get("data-drive-missing", "")
	if !ok {
		t.Fatal("Get of an active issue should return ok=true")
	}
	if iss.ID != "data-drive-missing" || iss.Severity != SeverityError || iss.Details != "abc-123 absent" {
		t.Errorf("Get returned %+v, want the raised issue with its severity/details", iss)
	}
}

func TestGet_MissingIssueReturnsFalse(t *testing.T) {
	m := NewManager(nil)
	if _, ok := m.Get("data-drive-missing", ""); ok {
		t.Error("Get of a never-raised issue should return ok=false")
	}
	// A per-instance key must not collide with the box-wide one.
	m.Raise("data-drive-missing", "", "box-wide")
	if _, ok := m.Get("data-drive-missing", "inst-abc"); ok {
		t.Error("Get with a different instance_key should return ok=false")
	}
}
