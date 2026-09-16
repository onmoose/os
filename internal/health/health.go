// Package health is the brain's typed-issue registry per HEALTH.md.
//
// An Issue is a fact the brain holds: detection creates it, kept until the
// underlying condition clears, surfaced via GET /api/v1/health and (later)
// the SSE event channel. The taxonomy is registered in code — IDs are stable
// strings, severity/category/tier/blocks_* flags are bound to the ID at
// registration, not redeclared per raise.
//
// SQLite persistence: the Manager writes through a HealthStore on every
// raise/clear so issues survive brain restarts. Pass nil for tests that don't
// need persistence.
package health

import (
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/onmoose/moose/internal/protocol"
)

// Severity is one of info | warning | error | critical per HEALTH.md.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// Category is one of storage | state | network | version | capacity per HEALTH.md.
type Category string

const (
	CategoryStorage  Category = "storage"
	CategoryState    Category = "state"
	CategoryNetwork  Category = "network"
	CategoryVersion  Category = "version"
	CategoryCapacity Category = "capacity"
)

// Definition binds an issue ID to its static metadata. Registered at brain
// startup; the registry is the contract.
//
// NoPersist marks internal issues that must never be written to SQLite — used
// for issues that exist precisely because the store itself is broken. Raise
// and Clear skip the store for any definition with NoPersist: true.
//
// ReportCategory ties the issue to a host-agent GET /v1/health/system report
// domain (protocol.HealthCategory: storage | drives | services | resources |
// time | system) — a *separate axis* from Category, which is the issue's display/nature
// taxonomy. ApplyFindings clears an issue only when its ReportCategory matches
// the poll's category and it's absent from that slice. Brain-owned issues
// (locus C, internal) leave it empty so a host-report poll never clears them —
// e.g. store-write-failed is Category state, but with no ReportCategory it's
// untouched by the services poll that reconciles service-down.
//
// Debounce applies the cross-cutting anti-flap default (HEALTH.md # Cross-cutting
// detector policy): raise only on 2 consecutive bad samples (clear still on 1
// good). Locus-A boot reporters and locus-D reactive signals are authoritative
// and leave it false (1-shot). service-down (locus B) sets it; storage findings
// stay 1-shot so storage detection is unchanged.
type Definition struct {
	ID             string
	Category       Category
	Severity       Severity
	Tier           int
	BlocksWrites   bool
	BlocksApps     bool
	BlocksUsers    bool
	Summary        string
	NoPersist      bool
	ReportCategory protocol.HealthCategory
	Debounce       bool
}

// Issue is the JSON shape returned by GET /api/v1/health and the in-memory
// record the manager holds. Mirrors HEALTH.md # Issue shape; the Actions
// list is deferred to a follow-up (the dashboard needs it before this earns
// its keep).
type Issue struct {
	ID            string    `json:"id"`
	InstanceKey   string    `json:"instance_key,omitempty"`
	Category      Category  `json:"category"`
	Severity      Severity  `json:"severity"`
	Tier          int       `json:"tier"`
	BlocksWrites  bool      `json:"blocks_writes"`
	BlocksApps    bool      `json:"blocks_apps"`
	BlocksUsers   bool      `json:"blocks_users"`
	Summary       string    `json:"summary"`
	Details       string    `json:"details,omitempty"`
	RaisedAt      time.Time `json:"raised_at"`
	LastCheckedAt time.Time `json:"last_checked_at"`
}

// IssueKey uniquely identifies one issue instance. Used in BatchUpsertAndDelete
// to avoid exporting the internal issueKey map type.
type IssueKey struct {
	ID          string
	InstanceKey string
}

// HealthStore persists health issues across brain restarts. The interface is
// consumer-side (CLAUDE.md): health does not import store.
type HealthStore interface {
	// UpsertHealthIssue inserts or replaces one issue row (used by Raise).
	UpsertHealthIssue(Issue) error
	// DeleteHealthIssue removes one issue row (used by Clear).
	DeleteHealthIssue(id, instanceKey string) error
	// BatchUpsertAndDelete runs upserts and deletes in a single transaction.
	// Used by ApplyFindings so a crash mid-reconcile can't leave SQLite torn
	// between old and new state.
	BatchUpsertAndDelete(upserts []Issue, deletes []IssueKey) error
	// ListHealthIssues returns all rows; used only at brain startup (LoadFromStore).
	ListHealthIssues() ([]Issue, error)
}

// Manager holds the live issue set. Thread-safe.
type Manager struct {
	mu          sync.Mutex
	definitions map[string]Definition
	active      map[issueKey]Issue
	// pending counts consecutive bad samples for debounced issues not yet
	// raised (HEALTH.md # Cross-cutting detector policy). A key reaches the
	// registry's active set only on its 2nd consecutive bad sample; one good
	// sample (absent from the report) resets the counter.
	pending map[issueKey]int
	now     func() time.Time
	store   HealthStore // nil means no persistence (tests)
}

type issueKey struct {
	id          string
	instanceKey string
}

// NewManager returns a Manager pre-loaded with the v1 storage definitions
// (HEALTH.md # Storage). Pass a non-nil store to persist issues across restarts;
// pass nil for in-memory-only use (tests, dev builds without SQLite).
func NewManager(store HealthStore) *Manager {
	m := &Manager{
		definitions: map[string]Definition{},
		active:      map[issueKey]Issue{},
		pending:     map[issueKey]int{},
		now:         func() time.Time { return time.Now().UTC() },
		store:       store,
	}
	for _, d := range builtinDefinitions() {
		m.definitions[d.ID] = d
	}
	return m
}

// LoadFromStore restores persisted issues into the in-memory registry at boot.
// Call once after NewManager, before the brain starts serving requests. If the
// store is nil or returns no rows, the registry starts empty (same as before
// persistence existed). On store error the brain logs and continues — degraded
// is better than refusing to start. Unknown IDs (issue renamed or removed in
// a brain upgrade/downgrade) are skipped and logged; rows remain in SQLite
// until the next Clear or a future cleanup sweep.
func (m *Manager) LoadFromStore() error {
	if m.store == nil {
		return nil
	}
	issues, err := m.store.ListHealthIssues()
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var unknown []string
	for _, iss := range issues {
		if _, known := m.definitions[iss.ID]; !known {
			unknown = append(unknown, iss.ID)
			continue
		}
		k := issueKey{id: iss.ID, instanceKey: iss.InstanceKey}
		m.active[k] = iss
	}
	if len(unknown) > 0 {
		slog.Warn("health: skipped unknown issue IDs on load; rows left in store until next Clear",
			"ids", unknown)
	}
	return nil
}

// SetClock swaps the time source — tests use this to assert RaisedAt /
// LastCheckedAt without sleeping.
func (m *Manager) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}

// Raise asserts the named issue is currently true. Idempotent: re-raising an
// active issue updates LastCheckedAt and Details, leaves RaisedAt alone, and
// does not emit a duplicate audit event when one lands. instanceKey may be
// empty for box-wide issues.
//
// Returns true if this raise transitioned the issue from cleared to active
// (the caller's signal to emit an audit event / notification). Returns false
// for an unknown ID — callers that *might* pass an unregistered ID (e.g.
// reconciling findings from an external reporter) should detect false and log,
// since this method intentionally does not have enough context to attribute
// the source of the drop.
func (m *Manager) Raise(id, instanceKey, details string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	transitioned := m.raiseLocked(id, instanceKey, details)
	def := m.definitions[id]
	if m.store != nil && !def.NoPersist {
		if iss, ok := m.active[issueKey{id: id, instanceKey: instanceKey}]; ok {
			if err := m.store.UpsertHealthIssue(iss); err != nil {
				slog.Error("health: store upsert failed", "id", id, "err", err)
				m.raiseLocked("store-write-failed", "", err.Error())
			} else {
				m.clearLocked("store-write-failed", "")
			}
		}
	}
	return transitioned
}

// Clear removes the issue if active. Returns true if a transition happened.
func (m *Manager) Clear(id, instanceKey string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	transitioned := m.clearLocked(id, instanceKey)
	def := m.definitions[id]
	if m.store != nil && transitioned && !def.NoPersist {
		if err := m.store.DeleteHealthIssue(id, instanceKey); err != nil {
			slog.Error("health: store delete failed", "id", id, "err", err)
			m.raiseLocked("store-write-failed", "", err.Error())
		} else {
			m.clearLocked("store-write-failed", "")
		}
	}
	return transitioned
}

// Get returns the active issue for (id, instanceKey) and whether it is
// currently raised. Used by the notification emitter to read an issue's
// severity/summary at transition time — ApplyFindings returns only the
// transitioned keys, not the full Issue. Returns ok=false for a cleared or
// never-raised issue.
func (m *Manager) Get(id, instanceKey string) (Issue, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	iss, ok := m.active[issueKey{id: id, instanceKey: instanceKey}]
	return iss, ok
}

// raiseLocked is the lock-held core of Raise, callable from reconcilers that
// already hold m.mu (e.g. ApplyFindings, which must reconcile a whole category
// atomically so List() never sees a torn state where old issues are cleared
// but new ones haven't been raised yet).
func (m *Manager) raiseLocked(id, instanceKey, details string) bool {
	def, ok := m.definitions[id]
	if !ok {
		return false
	}
	now := m.now()
	k := issueKey{id: id, instanceKey: instanceKey}
	if existing, ok := m.active[k]; ok {
		existing.LastCheckedAt = now
		existing.Details = details
		m.active[k] = existing
		return false
	}
	m.active[k] = Issue{
		ID:            def.ID,
		InstanceKey:   instanceKey,
		Category:      def.Category,
		Severity:      def.Severity,
		Tier:          def.Tier,
		BlocksWrites:  def.BlocksWrites,
		BlocksApps:    def.BlocksApps,
		BlocksUsers:   def.BlocksUsers,
		Summary:       def.Summary,
		Details:       details,
		RaisedAt:      now,
		LastCheckedAt: now,
	}
	return true
}

// clearLocked is the lock-held core of Clear. See raiseLocked.
func (m *Manager) clearLocked(id, instanceKey string) bool {
	k := issueKey{id: id, instanceKey: instanceKey}
	if _, ok := m.active[k]; !ok {
		return false
	}
	delete(m.active, k)
	return true
}

// List returns the active issues, sorted (Category, Severity desc, ID,
// InstanceKey) so callers and tests see a stable order.
func (m *Manager) List() []Issue {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Issue, 0, len(m.active))
	for _, iss := range m.active {
		out = append(out, iss)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Category != out[j].Category {
			return out[i].Category < out[j].Category
		}
		if out[i].Severity != out[j].Severity {
			return severityRank(out[i].Severity) > severityRank(out[j].Severity)
		}
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].InstanceKey < out[j].InstanceKey
	})
	return out
}

// ApplyFindings reconciles the live issue set for one report category against a
// fresh slice of findings from host-agent's GET /v1/health/system report. The
// category is a protocol.HealthCategory (storage | drives | services | …) — the
// report axis, not the issue's display Category. Within the category: every
// finding is raised (or refreshed), and every issue whose ReportCategory equals
// it is cleared when absent from the slice. Issues of other report categories —
// and brain-owned issues with no ReportCategory (store-write-failed) — are left
// untouched, so a storage poll never clears a service finding and a services
// poll never clears store-write-failed.
//
// Debounced definitions (service-down) raise only on their 2nd consecutive bad
// sample; one good sample (the finding absent from the report) resets the
// pending counter. last_checked_at is refreshed on every poll for every
// still-present active issue, even when nothing transitions, so a stale
// timestamp itself signals a dead detector (HEALTH.md # Cross-cutting policy).
//
// The clear+raise cycle runs under a single critical section so a concurrent
// List() never sees a transient "all clear". All SQLite writes go through one
// BatchUpsertAndDelete so a crash mid-reconcile can't tear the store. Transitions
// return as the raised / cleared issue keys (sorted by ID, then InstanceKey) so
// the caller can emit one per-issue audit record (target {kind: health_issue,
// id: <id>}) rather than a bulk count. Findings whose ID isn't in the registry
// are logged at warn and dropped — reporter-vs-brain version skew the operator
// needs to see.
func (m *Manager) ApplyFindings(category protocol.HealthCategory, findings []protocol.Finding) (raised, cleared []IssueKey) {
	wantKeys := map[issueKey]string{} // key → details
	for _, f := range findings {
		wantKeys[issueKey{id: f.ID, instanceKey: f.InstanceKey}] = f.Details
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var toUpsert []Issue
	var toDelete []IssueKey
	unknownIDs := []string{}

	for k, details := range wantKeys {
		def, known := m.definitions[k.id]
		if !known {
			unknownIDs = append(unknownIDs, k.id)
			continue
		}
		// Debounce gate: a not-yet-active debounced issue holds (counts) its
		// first bad sample and raises only on the second. Already-active issues
		// skip the gate — they just refresh.
		if _, active := m.active[k]; !active && def.Debounce && m.pending[k]+1 < 2 {
			m.pending[k]++
			continue
		}
		delete(m.pending, k)
		if m.raiseLocked(k.id, k.instanceKey, details) {
			raised = append(raised, IssueKey{ID: k.id, InstanceKey: k.instanceKey})
		}
		// Upsert on every poll tick (not just transitions) to keep last_checked_at
		// current in SQLite. At the 60s cadence with a handful of findings this is
		// negligible; if the poll interval shortens materially, consider moving
		// last_checked_at to a lazy write to avoid per-tick writes for stable issues.
		if m.store != nil {
			if iss, ok := m.active[k]; ok {
				toUpsert = append(toUpsert, iss)
			}
		}
	}

	// Clear-absent: any issue whose ReportCategory is THIS category and which is
	// not in the findings. Scoping by ReportCategory keeps other report domains
	// and brain-owned issues with no ReportCategory (store-write-failed)
	// untouched.
	for k := range m.active {
		if m.definitions[k.id].ReportCategory != category {
			continue
		}
		if _, keep := wantKeys[k]; keep {
			continue
		}
		if m.clearLocked(k.id, k.instanceKey) {
			cleared = append(cleared, IssueKey{ID: k.id, InstanceKey: k.instanceKey})
			if m.store != nil {
				toDelete = append(toDelete, IssueKey{ID: k.id, InstanceKey: k.instanceKey})
			}
		}
	}

	// One good sample (absent from the report) resets a pending, not-yet-raised
	// debounce counter for this category. Run after the raise loop so keys still
	// present this cycle keep their count.
	for k := range m.pending {
		if m.definitions[k.id].ReportCategory != category {
			continue
		}
		if _, present := wantKeys[k]; !present {
			delete(m.pending, k)
		}
	}

	if len(unknownIDs) > 0 {
		slog.Warn("system health: dropped unknown finding ids",
			"category", category, "ids", unknownIDs)
	}

	if m.store != nil && (len(toUpsert) > 0 || len(toDelete) > 0) {
		if err := m.store.BatchUpsertAndDelete(toUpsert, toDelete); err != nil {
			slog.Error("health: batch store write failed",
				"upserts", len(toUpsert), "deletes", len(toDelete), "err", err)
			m.raiseLocked("store-write-failed", "", err.Error())
		} else {
			m.clearLocked("store-write-failed", "")
		}
	}

	// Map iteration order is non-deterministic; sort so the per-issue audit
	// records (and tests) see a stable order, matching List()'s contract.
	sortIssueKeys(raised)
	sortIssueKeys(cleared)
	return raised, cleared
}

func sortIssueKeys(ks []IssueKey) {
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].ID != ks[j].ID {
			return ks[i].ID < ks[j].ID
		}
		return ks[i].InstanceKey < ks[j].InstanceKey
	})
}

func severityRank(s Severity) int {
	switch s {
	case SeverityCritical:
		return 3
	case SeverityError:
		return 2
	case SeverityWarning:
		return 1
	}
	return 0 // info and any unrecognised severity are the floor
}

// builtinDefinitions is the v1 typed-issue taxonomy. Mirrors the tables in
// HEALTH.md # Taxonomy. Storage findings (ReportCategory storage) and
// service-down (ReportCategory services) are reconciled from host-agent's GET
// /v1/health/system report; the other issues are pre-registered so future
// detectors plug in.
func builtinDefinitions() []Definition {
	return []Definition{
		// Storage (HEALTH.md # Storage). ReportCategory storage: they arrive via
		// the host-agent system report (boot reporter + 60s poll) and clear when
		// absent from it.
		{
			ID: "data-drive-missing", Category: CategoryStorage,
			Severity: SeverityError, Tier: 1,
			BlocksWrites: true, BlocksApps: true, BlocksUsers: true,
			Summary: "Your data drive isn't connected.", ReportCategory: protocol.HealthCategoryStorage,
		},
		{
			ID: "data-drive-wrong", Category: CategoryStorage,
			Severity: SeverityCritical, Tier: 2,
			BlocksWrites: true, BlocksApps: true, BlocksUsers: true,
			Summary: "A different drive is attached than the one moose expects.", ReportCategory: protocol.HealthCategoryStorage,
		},
		// data-drive-readonly is pre-registered but not yet emitted by any
		// detector. The findmnt-based device-backing + mount-flags check that
		// will surface it is deferred (see docs/progress/boot-pipeline-units.md
		// # What's next). Definition lives here so the next reporter slice plugs
		// in without a registry edit.
		{
			ID: "data-drive-readonly", Category: CategoryStorage,
			Severity: SeverityCritical, Tier: 1,
			BlocksWrites: true, BlocksApps: true,
			Summary: "The data drive is mounted read-only (likely filesystem error).", ReportCategory: protocol.HealthCategoryStorage,
		},
		{
			ID: "canary-mismatch", Category: CategoryStorage,
			Severity: SeverityCritical, Tier: 2,
			BlocksWrites: true, BlocksApps: true,
			Summary: "Storage assembly succeeded but the canary check failed.", ReportCategory: protocol.HealthCategoryStorage,
		},
		{
			ID: "mergerfs-assembly-failed", Category: CategoryStorage,
			Severity: SeverityError, Tier: 2,
			BlocksWrites: true, BlocksApps: true,
			Summary: "moose could not assemble the storage layout across your drives.", ReportCategory: protocol.HealthCategoryStorage,
		},
		// Synthetic — host-agent's FilesystemHealthSource emits this when the
		// report file is unparseable; the reporter emits it when its inputs are.
		{
			ID: "health-report-malformed", Category: CategoryStorage,
			Severity: SeverityError, Tier: 2,
			BlocksWrites: false, BlocksApps: false, BlocksUsers: false,
			Summary: "moose's storage report is unreadable; storage state is unknown.", ReportCategory: protocol.HealthCategoryStorage,
		},
		// service-down is locus B — host-agent runs `systemctl is-active` over the
		// core-unit allowlist and reports it under the system report's *services*
		// category, while the issue itself is display Category state (HEALTH.md
		// # State). Per-unit instance_key. No block flags: a dead service fails its
		// own ops naturally, and blocking everything because Avahi died would be
		// blunt. Debounces (locus B).
		{
			ID: "service-down", Category: CategoryState,
			Severity: SeverityError, Tier: 2,
			BlocksWrites: false, BlocksApps: false, BlocksUsers: false,
			Summary:        "A core system service isn't running.",
			ReportCategory: protocol.HealthCategoryServices, Debounce: true,
		},
		// Internal — raised when any store write fails persistently. NoPersist
		// because it exists precisely when persistence is broken: writing it to
		// SQLite would fail too. Surfaced on the dashboard so the operator knows
		// health issues won't survive a restart until the store recovers.
		{
			ID: "store-write-failed", Category: CategoryState,
			Severity: SeverityError, Tier: 2,
			BlocksWrites: false, BlocksApps: false, BlocksUsers: false,
			Summary:   "moose can't save health state; issues won't survive a restart.",
			NoPersist: true,
		},
		// Version (HEALTH.md # Version). version-mismatch is a locus-C brain
		// check: host-agent's reported agent_version against this brain's
		// minimumAgentVersion floor (cmd/brain checkAgentVersion) — a compatible
		// RANGE, not exact equality (DECISIONS.md 2026-07-16). host-agent updates
		// ride apt on their own cadence and can lag the brain by up to 24h
		// (UPDATES.md # 2), so a newer agent never raises this; only one older
		// than the floor does. Blocks apps — installing or updating an app
		// against a too-old agent is unsafe — but not writes or users. Error
		// severity, Tier-2 remediation (the lagging agent updates itself; the
		// issue clears once it does).
		{
			ID: "version-mismatch", Category: CategoryVersion,
			Severity: SeverityError, Tier: 2,
			BlocksApps: true,
			Summary:    "moose's system agent needs an update.",
		},
		// State (HEALTH.md # State). brain-db-corrupt is a locus-C brain check:
		// PRAGMA integrity_check at boot + every 6h (cmd/brain checkBrainDBIntegrity).
		// A corrupt brain database can't be trusted for any mutation, so it blocks
		// writes, apps, and users alike ("nearly all ops") — but the dashboard,
		// login, and logs stay up (HEALTH.md # What stays available). Critical,
		// Tier-2 remediation (restore from backup). Persisted (not NoPersist):
		// integrity_check flags corruption that often still permits the row write,
		// and the issue should survive a restart so the banner stays up; the
		// generic store-write-failed fallback covers the case the write also fails.
		{
			ID: "brain-db-corrupt", Category: CategoryState,
			Severity: SeverityCritical, Tier: 2,
			BlocksWrites: true, BlocksApps: true, BlocksUsers: true,
			Summary: "moose's database is damaged; some actions are turned off until it's fixed.",
		},
		// Version/app-runtime (HEALTH.md # Version). container-restart-loop is a
		// locus-D detector: the brain samples each managed container's cumulative
		// Docker RestartCount and raises when it climbs past the threshold within
		// a window (cmd/brain restartLoopDetector). Per-app — keyed by instance_id.
		// The app is already failing, so we surface it rather than block; warning,
		// Tier-2 (view logs / stop the app).
		{
			ID: "container-restart-loop", Category: CategoryVersion,
			Severity: SeverityWarning, Tier: 2,
			BlocksWrites: false, BlocksApps: false, BlocksUsers: false,
			Summary: "An app keeps crashing and restarting.",
		},
		// Version/app-runtime (HEALTH.md # Version). app-unresponsive is a locus-C
		// brain check: for each steady-running instance that declares a
		// health_probe (APP_MANIFEST.md # B), the brain GETs the probe path
		// through the app's Caddy route and raises when it fails (cmd/brain
		// appProbeDetector). Per-app — keyed by instance_id. Opt-in: an app with no
		// health_probe is never probed and never raises this. The app is reachable
		// but not answering coherently, so we surface it rather than block; warning,
		// Tier-2 (view logs / restart the app). The detector applies the
		// cross-cutting 2-bad/1-good debounce itself (it calls Raise/Clear directly,
		// not via ApplyFindings), so the Debounce flag here would be inert — left
		// false to avoid implying ApplyFindings drives it.
		{
			ID: "app-unresponsive", Category: CategoryVersion,
			Severity: SeverityWarning, Tier: 2,
			BlocksWrites: false, BlocksApps: false, BlocksUsers: false,
			Summary: "An app is running but not responding.",
		},
		// Network (HEALTH.md # Network, TIME.md # Drift monitoring). clock-not-synced
		// is locus B — host-agent runs `chronyc tracking` and reports last-sync age
		// + offset under the system report's *time* category, while the issue itself
		// is display Category network. Box-wide (no instance_key). Warning, no block
		// flags — the box stays usable; it gates only Let's Encrypt renewal near
		// expiry (TIME.md). The host-agent reporter owns the 6h/10s thresholds.
		// Debounces (locus B).
		{
			ID: "clock-not-synced", Category: CategoryNetwork,
			Severity: SeverityWarning, Tier: 2,
			BlocksWrites: false, BlocksApps: false, BlocksUsers: false,
			Summary:        "The box's clock isn't being kept accurate. Some features (HTTPS certificates, scheduled backups) may stop working.",
			ReportCategory: protocol.HealthCategoryTime, Debounce: true,
		},
		// Capacity & informational (HEALTH.md # Capacity & informational).
		// ram-pressure is locus B — host-agent samples /proc/pressure/memory (PSI
		// `some avg60`) and reports it under the system report's *resources*
		// category; the issue itself is display Category capacity. Box-wide (no
		// instance_key). Warning, no block flags — it's informational, pointing the
		// user at the per-container monitor rather than gating anything. The
		// host-agent reporter owns the (conservative, tune-at-soak) threshold.
		// Debounces (locus B).
		{
			ID: "ram-pressure", Category: CategoryCapacity,
			Severity: SeverityWarning, Tier: 1,
			BlocksWrites: false, BlocksApps: false, BlocksUsers: false,
			Summary:        "The box is low on memory and is slowing down. Check which app is using the most.",
			ReportCategory: protocol.HealthCategoryResources, Debounce: true,
		},
		// Capacity & informational (HEALTH.md # Capacity & informational).
		// reboot-required is locus B — host-agent stats /var/run/reboot-required
		// (Debian's apt/unattended-upgrades flag) and reports its presence under the
		// system report's *system* category; the issue itself is display Category
		// capacity. Box-wide (no instance_key). Info severity (the first info issue),
		// no block flags — a pending reboot is purely informational (a quiet card per
		// HEALTH.md), pointing the user at "reboot now / schedule". The package list
		// (/var/run/reboot-required.pkgs) rides in Details. Left 1-shot (Debounce
		// false): file presence is deterministic — apt creates the flag, a reboot
		// clears it (tmpfs /run) — so there's nothing to flap, and the info banner
		// surfaces one poll sooner.
		{
			ID: "reboot-required", Category: CategoryCapacity,
			Severity: SeverityInfo, Tier: 2,
			BlocksWrites: false, BlocksApps: false, BlocksUsers: false,
			Summary:        "A system update needs a reboot to finish.",
			ReportCategory: protocol.HealthCategorySystem,
		},
	}
}
