// Package notify is the routing + derivation layer for the dashboard
// notification center (NOTIFICATIONS.md). A notification is *derived* from an
// event that already exists elsewhere — this slice wires the first source:
// HEALTH issue raise/clear. The package does not invent a parallel taxonomy;
// it maps a bounded, code-registered allowlist of health issue IDs to a
// persisted, prunable notification.
//
// On a health-issue *transition* the Notifier emits the admin actionable
// notification (coalesced by dedup_key); for box-blocking storage issues it
// also emits an info-only member transparency notice (NOTIFICATIONS.md # Member
// transparency variant). On clear it marks the problem resolved and emits a
// brief info "all clear" to the same audience(s). It publishes
// notification.created / notification.updated onto the SSE bus so the dashboard
// bell updates without a refresh (the read surface — the list/read/dismiss API
// and per-recipient read state — lives in store + api,
// docs/progress/0026-notification-read-surface.md). Per-category mute,
// retention, and off-box transports remain deferred to later slices.
package notify

import (
	"log/slog"
	"time"

	"github.com/onmoose/os/internal/events"
	"github.com/onmoose/os/internal/health"
)

// Category groups notifications in the inbox (NOTIFICATIONS.md # The
// notification model). The full taxonomy is defined here because the per-category
// mute surface (NOTIFICATIONS.md # Configuration) validates against the complete
// set — a user can mute a category before its source exists (e.g. mute `updates`
// chatter). Only storage / system have producers today; updates / security /
// account / app light up as their sources land.
type Category string

const (
	CategoryStorage  Category = "storage"
	CategorySystem   Category = "system"
	CategoryUpdates  Category = "updates"
	CategorySecurity Category = "security"
	CategoryAccount  Category = "account"
	CategoryApp      Category = "app"
)

// Categories is the complete notification taxonomy, in display order — the
// source of truth the mute surface validates against (NOTIFICATIONS.md
// # Configuration).
var Categories = []Category{
	CategoryStorage, CategorySystem, CategoryUpdates,
	CategorySecurity, CategoryAccount, CategoryApp,
}

// ValidCategory reports whether s names a known notification category. The mute
// API uses it to reject unknown categories rather than persist a typo that would
// silently mute nothing.
func ValidCategory(s string) bool {
	for _, c := range Categories {
		if string(c) == s {
			return true
		}
	}
	return false
}

// Severity reuses HEALTH's vocabulary plus info (NOTIFICATIONS.md # Severity).
// For health-derived notifications it is copied verbatim from the issue —
// severity is a property of the source event, never reassigned here.
type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityWarning  Severity = "warning"
	SeverityError    Severity = "error"
	SeverityCritical Severity = "critical"
)

// Source kinds (NOTIFICATIONS.md # The notification model). Only health_issue
// is wired in this slice.
const SourceHealthIssue = "health_issue"

// Audience and variant (NOTIFICATIONS.md # Routing). A box-wide health issue
// produces an actionable notification to admins (who hold the fix); a
// box-blocking storage issue additionally produces an info-only transparency
// notice broadcast to members — the class of all non-admin users — so "why
// can't I save?" answers itself without exposing the admin-gated remediation.
const (
	AudienceAdmins      = "admins"
	AudienceMembers     = "members"
	AudienceUser        = "user"
	VariantActionable   = "actionable"
	VariantTransparency = "transparency"
)

// Dedup-key suffixes. One health issue produces up to four coalescing slots:
// the admin problem (base key), the member transparency notice (base+:member),
// and an "all clear" for each (problem key +:cleared). The suffixes keep all
// four distinct so they never collide on the active-row unique index, and so a
// clear resolves exactly the notification its raise created.
const (
	dedupMemberSuffix = ":member"
	dedupClearSuffix  = ":cleared"
)

// Member transparency copy (NOTIFICATIONS.md # Member transparency variant).
// Box-blocking storage issues pause saving for everyone; members get this
// info-only notice (no fix link — remediation is admin-gated) and a matching
// "all clear" when the issue resolves. The copy is shared across the storage
// drive issues because, from a member's vantage, they are one condition:
// saving is paused. Which drive fault caused it is admin detail.
const (
	memberPausedSummary = "Saving is paused"
	memberPausedBody    = "A storage problem on your moose needs your admin's attention. They've been notified."
	memberClearSummary  = "Saving is back to normal"
	memberClearBody     = "The storage problem is resolved — saving works again."
)

// Notification is the typed record persisted in the brain's SQLite
// `notifications` table (NOTIFICATIONS.md # The notification model). It is
// mutable and prunable — distinct from the append-only audit log.
//
// ReadAt / DismissedAt are *per-recipient* state: they live in the
// notification_reads join, not on the notification row, and are populated only
// by the recipient-scoped read query (ListNotificationsForRecipient). On the
// write path they are zero.
type Notification struct {
	ID          int64
	TS          int64 // unix epoch ms — most-recent occurrence
	Category    Category
	Severity    Severity
	SourceKind  string // SourceHealthIssue
	SourceID    string // the originating issue ID
	DedupKey    string // coalescing key — one active notification per key
	Audience    string // AudienceAdmins | AudienceMembers | AudienceUser
	UserID      string // set only when Audience == AudienceUser
	Variant     string // VariantActionable | VariantTransparency
	Summary     string
	Body        string
	ActionLabel string // "" → no action link
	ActionRoute string
	ResolvedAt  int64 // 0 = active; set when the underlying condition clears
	ReadAt      int64 // 0 = unread for this recipient (per-recipient, from the join)
	DismissedAt int64 // 0 = not dismissed by this recipient (per-recipient, from the join)
}

// NotificationStore is the persistence surface notify needs. Declared here so
// the package doesn't depend on store's full API (consumer-side interface,
// CLAUDE.md). The store implements it; notify does not import store.
type NotificationStore interface {
	// RaiseNotification coalesces by dedup_key: if an active (non-dismissed)
	// notification with the same key exists it is refreshed (ts/severity/body
	// bumped, resolved_at cleared), otherwise a new row is inserted.
	RaiseNotification(Notification) error
	// ResolveNotification marks the active notification for dedupKey resolved.
	// No-op when no active row matches (idempotent).
	ResolveNotification(dedupKey string, at time.Time) error
}

// Publisher fans a notification lifecycle change onto the SSE bus so the
// dashboard bell updates without a refresh (NOTIFICATIONS.md # Surfaces). The
// payload is advisory — a refetch trigger, not a data channel — so the client
// re-reads the audience-scoped list (WEB_UI.md). Consumer-side interface
// (CLAUDE.md): events.Bus implements it. Optional — a nil Publisher disables
// SSE emission (the bell is a floor, not a gate).
type Publisher interface {
	Publish(kind events.Kind, data map[string]any)
}

// healthRule binds one health issue ID to how it notifies. This is the
// curated, code-registered allowlist (NOTIFICATIONS.md # The notification
// list) — bounded on purpose, the same discipline as HEALTH's issue taxonomy.
// An issue ID absent from this map never reaches the bell (e.g.
// health-report-malformed and store-write-failed are deliberately excluded).
type healthRule struct {
	category    Category
	actionLabel string // "" → no action link
	actionRoute string
	// clearSummary is the admin-facing "all clear" copy emitted when the issue
	// resolves (NOTIFICATIONS.md # Clears). HealthCleared has only the issue ID,
	// not the original Issue, so the resolved copy is registered here per rule.
	clearSummary string
	// memberTransparency marks a box-blocking storage issue that additionally
	// emits an info-only transparency notice to members (NOTIFICATIONS.md #
	// Member transparency variant) — and a member "all clear" when it resolves.
	// Set only on the storage drive issues the spec's allowlist marks "+ member
	// transparency"; canary-mismatch / mergerfs-assembly-failed stay admin-only
	// even though they also block writes (they are System/state, not the
	// member-legible "your saving is paused" condition).
	memberTransparency bool
}

// healthRules is the v1 allowlist for HEALTH-derived notifications. It covers
// the registered storage/state issue IDs that NOTIFICATIONS.md lists. Three
// have a live detector today (storageverify emits data-drive-missing,
// data-drive-wrong, canary-mismatch); data-drive-readonly and
// mergerfs-assembly-failed are pre-registered ahead of their detectors,
// mirroring HEALTH's own pre-registration pattern — harmless, and they notify
// the moment their detector lands. The rest of the spec's allowlist (disk-full,
// brain-db-corrupt, SMART warnings, update outcomes, security audit actions,
// app lifecycle) lands as those sources are implemented.
var healthRules = map[string]healthRule{
	// Storage — to Admin, with the member transparency variant (NOTIFICATIONS.md
	// # Storage: "+ member transparency").
	"data-drive-missing":  {category: CategoryStorage, actionLabel: "Open Storage", actionRoute: "/settings/storage", clearSummary: "Your data drive is reconnected.", memberTransparency: true},
	"data-drive-wrong":    {category: CategoryStorage, actionLabel: "Open Storage", actionRoute: "/settings/storage", clearSummary: "The expected data drive is attached again.", memberTransparency: true},
	"data-drive-readonly": {category: CategoryStorage, actionLabel: "Open Storage", actionRoute: "/settings/storage", clearSummary: "The data drive is writable again.", memberTransparency: true},
	// System / state — to Admin only (NOTIFICATIONS.md # System / state).
	"canary-mismatch":          {category: CategorySystem, actionLabel: "Open Storage", actionRoute: "/settings/storage", clearSummary: "The storage canary check is passing again."},
	"mergerfs-assembly-failed": {category: CategorySystem, actionLabel: "Open Storage", actionRoute: "/settings/storage", clearSummary: "Storage assembled successfully across your drives."},
}

// Notifier derives notifications from events and writes them through the store.
// Construct once via New. Like audit.Recorder, it never propagates store
// errors — a failed notification is logged and swallowed, never blocking the
// triggering operation (NOTIFICATIONS.md: the bell is a floor, not a gate).
type Notifier struct {
	store NotificationStore
	pub   Publisher // may be nil — SSE emission is then a no-op
	now   func() time.Time
}

// New returns a Notifier backed by the given store, publishing lifecycle
// changes onto pub (pass nil to disable SSE emission).
func New(s NotificationStore, pub Publisher) *Notifier {
	return &Notifier{store: s, pub: pub, now: func() time.Time { return time.Now().UTC() }}
}

// publish emits an SSE event if a Publisher is wired. Best-effort: a missing
// bus never blocks the notification write.
func (n *Notifier) publish(kind events.Kind, data map[string]any) {
	if n.pub != nil {
		n.pub.Publish(kind, data)
	}
}

// SetClock swaps the time source — tests use this to assert TS without sleeping.
func (n *Notifier) SetClock(now func() time.Time) { n.now = now }

// HealthRaised emits notifications for a health issue that just transitioned to
// active. Issues not on the allowlist are ignored. It always emits the admin
// actionable notification; for a memberTransparency issue it additionally emits
// the info-only member transparency notice. This method is only called on
// genuine raise transitions, so a re-raise here is a real flap (cleared then
// raised again) — which makes any "all clear" from the prior clear stale, so
// each problem's all-clear is resolved. Coalescing (one row per dedup_key) is
// the store's job.
func (n *Notifier) HealthRaised(iss health.Issue) {
	rule, ok := healthRules[iss.ID]
	if !ok {
		return
	}
	base := healthDedupKey(iss.ID, iss.InstanceKey)

	// Admin actionable notification — the fix lives here (admins hold sudo).
	admin := Notification{
		TS:          n.now().UnixMilli(),
		Category:    rule.category,
		Severity:    Severity(iss.Severity),
		SourceKind:  SourceHealthIssue,
		SourceID:    iss.ID,
		DedupKey:    base,
		Audience:    AudienceAdmins,
		Variant:     VariantActionable,
		Summary:     iss.Summary,
		Body:        iss.Details,
		ActionLabel: rule.actionLabel,
		ActionRoute: rule.actionRoute,
	}
	if err := n.store.RaiseNotification(admin); err != nil {
		slog.Error("notify: raise failed", "source_id", iss.ID, "err", err)
		return
	}
	n.resolveStaleClear(base) // retract a now-false "all clear" on a flap (no-op otherwise)

	// Member transparency notice — info-only, non-actionable; remediation stays
	// admin-gated (NOTIFICATIONS.md # Member transparency variant).
	if rule.memberTransparency {
		member := Notification{
			TS:         n.now().UnixMilli(),
			Category:   rule.category,
			Severity:   SeverityInfo,
			SourceKind: SourceHealthIssue,
			SourceID:   iss.ID,
			DedupKey:   base + dedupMemberSuffix,
			Audience:   AudienceMembers,
			Variant:    VariantTransparency,
			Summary:    memberPausedSummary,
			Body:       memberPausedBody,
		}
		if err := n.store.RaiseNotification(member); err != nil {
			slog.Error("notify: member transparency raise failed", "source_id", iss.ID, "err", err)
		}
		n.resolveStaleClear(base + dedupMemberSuffix)
	}

	// One advisory publish is enough — the client refetches its audience-scoped
	// list (members and admins each re-read their own view).
	n.publish(events.NotificationCreated, map[string]any{
		"dedup_key": admin.DedupKey,
		"category":  string(admin.Category),
		"severity":  string(admin.Severity),
	})
}

// HealthCleared marks the cleared health issue's notifications resolved and
// emits a brief info "all clear" to each audience that received the raise
// (NOTIFICATIONS.md # Clears) — the originals stay on the timeline, resolved,
// so the history is honest. Mirrors HealthRaised's allowlist gate so a clear of
// a non-notifying issue is a true no-op. The store calls are idempotent.
func (n *Notifier) HealthCleared(id, instanceKey string) {
	rule, ok := healthRules[id]
	if !ok {
		return
	}
	base := healthDedupKey(id, instanceKey)

	// Resolve the admin problem and emit the admin "all clear".
	if err := n.store.ResolveNotification(base, n.now()); err != nil {
		slog.Error("notify: resolve failed", "source_id", id, "err", err)
		return
	}
	n.raiseAllClear(base, id, rule.category, AudienceAdmins, rule.clearSummary, "")

	// Mirror on the member side for transparency issues.
	if rule.memberTransparency {
		memberKey := base + dedupMemberSuffix
		if err := n.store.ResolveNotification(memberKey, n.now()); err != nil {
			slog.Error("notify: member resolve failed", "source_id", id, "err", err)
		} else {
			n.raiseAllClear(memberKey, id, rule.category, AudienceMembers, memberClearSummary, memberClearBody)
		}
	}

	n.publish(events.NotificationUpdated, map[string]any{"dedup_key": base})
}

// resolveStaleClear resolves the "all clear" notification paired with a problem
// dedup key, best-effort. Called on raise to retract an all-clear made false by
// a flap; a no-op (no matching active row) on a first raise.
func (n *Notifier) resolveStaleClear(problemKey string) {
	if err := n.store.ResolveNotification(problemKey+dedupClearSuffix, n.now()); err != nil {
		slog.Error("notify: stale all-clear resolve failed", "dedup_key", problemKey, "err", err)
	}
}

// raiseAllClear emits the info "all clear" that follows a resolved issue
// (NOTIFICATIONS.md # Clears), keyed to the problem key plus :cleared so it
// coalesces across repeated clears and never collides with the problem row.
// Best-effort — a failed all-clear never blocks anything.
func (n *Notifier) raiseAllClear(problemKey, sourceID string, cat Category, audience, summary, body string) {
	clear := Notification{
		TS:         n.now().UnixMilli(),
		Category:   cat,
		Severity:   SeverityInfo,
		SourceKind: SourceHealthIssue,
		SourceID:   sourceID,
		DedupKey:   problemKey + dedupClearSuffix,
		Audience:   audience,
		Variant:    variantForAudience(audience),
		Summary:    summary,
		Body:       body,
	}
	if err := n.store.RaiseNotification(clear); err != nil {
		slog.Error("notify: all-clear raise failed", "dedup_key", clear.DedupKey, "err", err)
	}
}

// variantForAudience picks the variant for an all-clear from its audience: the
// member stream is transparency, the admin stream actionable. (Variant isn't
// surfaced to the client; it keeps the row consistent with its audience.)
func variantForAudience(audience string) string {
	if audience == AudienceMembers {
		return VariantTransparency
	}
	return VariantActionable
}

// healthDedupKey is the coalescing key for a HEALTH-derived notification:
// "health:<id>" for box-wide issues, "health:<id>:<instanceKey>" for
// per-instance ones. Stable across raise and clear so a clear resolves exactly
// the notification its raise created.
func healthDedupKey(id, instanceKey string) string {
	if instanceKey == "" {
		return "health:" + id
	}
	return "health:" + id + ":" + instanceKey
}
