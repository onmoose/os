package notify

import (
	"errors"
	"testing"
	"time"

	"github.com/onmoose/moose/internal/events"
	"github.com/onmoose/moose/internal/health"
)

// fakeStore captures Raise/Resolve calls so the derivation logic can be tested
// without SQLite. The coalescing SQL itself is tested in internal/store.
type fakeStore struct {
	raised     []Notification
	resolved   []string // dedup keys
	resolveAt  []time.Time
	raiseErr   error
	resolveErr error
}

func (f *fakeStore) RaiseNotification(n Notification) error {
	if f.raiseErr != nil {
		return f.raiseErr
	}
	f.raised = append(f.raised, n)
	return nil
}

func (f *fakeStore) ResolveNotification(dedupKey string, at time.Time) error {
	if f.resolveErr != nil {
		return f.resolveErr
	}
	f.resolved = append(f.resolved, dedupKey)
	f.resolveAt = append(f.resolveAt, at)
	return nil
}

// fakePublisher captures SSE events so the notifier's bus emission can be
// asserted without a real events.Bus.
type publishedEvent struct {
	kind events.Kind
	data map[string]any
}

type fakePublisher struct {
	events []publishedEvent
}

func (f *fakePublisher) Publish(kind events.Kind, data map[string]any) {
	f.events = append(f.events, publishedEvent{kind, data})
}

func issue(id, instanceKey string) health.Issue {
	return health.Issue{
		ID:          id,
		InstanceKey: instanceKey,
		Category:    health.CategoryStorage,
		Severity:    health.SeverityCritical,
		Summary:     "Your data drive isn't connected.",
		Details:     "abc-123 absent",
	}
}

func newNotifier(s NotificationStore) *Notifier {
	return newNotifierWithPub(s, nil)
}

func newNotifierWithPub(s NotificationStore, pub Publisher) *Notifier {
	n := New(s, pub)
	n.SetClock(func() time.Time { return time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC) })
	return n
}

func TestHealthRaised_AllowlistedProducesNotification(t *testing.T) {
	fs := &fakeStore{}
	newNotifier(fs).HealthRaised(issue("data-drive-missing", ""))

	// data-drive-missing is a member-transparency issue, so it raises two: the
	// admin actionable (raised[0], asserted here) and the member transparency
	// notice (raised[1], asserted in TestHealthRaised_MemberTransparency).
	if len(fs.raised) != 2 {
		t.Fatalf("want 2 raised notifications (admin + member), got %d", len(fs.raised))
	}
	n := fs.raised[0]
	if n.SourceKind != SourceHealthIssue || n.SourceID != "data-drive-missing" {
		t.Errorf("source = {%q,%q}, want {%q,data-drive-missing}", n.SourceKind, n.SourceID, SourceHealthIssue)
	}
	if n.DedupKey != "health:data-drive-missing" {
		t.Errorf("dedup_key = %q, want health:data-drive-missing", n.DedupKey)
	}
	if n.Category != CategoryStorage {
		t.Errorf("category = %q, want storage", n.Category)
	}
	if n.Severity != SeverityCritical {
		t.Errorf("severity = %q, want critical (copied from issue)", n.Severity)
	}
	if n.Audience != AudienceAdmins || n.Variant != VariantActionable {
		t.Errorf("routing = {%q,%q}, want {admins,actionable}", n.Audience, n.Variant)
	}
	if n.ActionLabel != "Open Storage" || n.ActionRoute != "/settings/storage" {
		t.Errorf("action = {%q,%q}, want {Open Storage,/settings/storage}", n.ActionLabel, n.ActionRoute)
	}
	if n.Summary != "Your data drive isn't connected." || n.Body != "abc-123 absent" {
		t.Errorf("summary/body = {%q,%q}", n.Summary, n.Body)
	}
	if n.TS != time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC).UnixMilli() {
		t.Errorf("ts = %d, want clock value", n.TS)
	}
}

func TestHealthRaised_SystemCategoryIssue(t *testing.T) {
	fs := &fakeStore{}
	newNotifier(fs).HealthRaised(issue("canary-mismatch", ""))
	if len(fs.raised) != 1 {
		t.Fatalf("want 1 notification, got %d", len(fs.raised))
	}
	if fs.raised[0].Category != CategorySystem {
		t.Errorf("category = %q, want system", fs.raised[0].Category)
	}
}

// health-report-malformed is a storage-category issue deliberately omitted from
// the notification allowlist (NOTIFICATIONS.md). A naive "all non-network
// storage issues notify" rule would wrongly include it; the explicit allowlist
// must not.
func TestHealthRaised_NotAllowlisted_NoNotification(t *testing.T) {
	for _, id := range []string{"health-report-malformed", "store-write-failed", "made-up-id"} {
		fs := &fakeStore{}
		newNotifier(fs).HealthRaised(issue(id, ""))
		if len(fs.raised) != 0 {
			t.Errorf("%s: want 0 notifications (not allowlisted), got %d", id, len(fs.raised))
		}
	}
}

func TestHealthRaised_InstanceKeyInDedupKey(t *testing.T) {
	fs := &fakeStore{}
	newNotifier(fs).HealthRaised(issue("data-drive-missing", "inst-abc"))
	if len(fs.raised) != 2 {
		t.Fatalf("want 2 notifications (admin + member), got %d", len(fs.raised))
	}
	if got := fs.raised[0].DedupKey; got != "health:data-drive-missing:inst-abc" {
		t.Errorf("admin dedup_key = %q, want health:data-drive-missing:inst-abc", got)
	}
	// The member notice keys off the same base plus :member, so its clear
	// resolves exactly its own raise.
	if got := fs.raised[1].DedupKey; got != "health:data-drive-missing:inst-abc:member" {
		t.Errorf("member dedup_key = %q, want health:data-drive-missing:inst-abc:member", got)
	}
}

func TestHealthCleared_AllowlistedResolves(t *testing.T) {
	fs := &fakeStore{}
	newNotifier(fs).HealthCleared("data-drive-missing", "")
	// A transparency issue resolves both the admin problem and the member notice.
	if !contains(fs.resolved, "health:data-drive-missing") {
		t.Errorf("resolved = %v, want it to include the admin problem key", fs.resolved)
	}
	if !contains(fs.resolved, "health:data-drive-missing:member") {
		t.Errorf("resolved = %v, want it to include the member problem key", fs.resolved)
	}
	if fs.resolveAt[0] != time.Date(2026, 5, 29, 12, 0, 0, 0, time.UTC) {
		t.Errorf("resolve at = %v, want clock value", fs.resolveAt[0])
	}
}

func TestHealthCleared_NotAllowlisted_NoResolve(t *testing.T) {
	fs := &fakeStore{}
	newNotifier(fs).HealthCleared("health-report-malformed", "")
	if len(fs.resolved) != 0 {
		t.Fatalf("want 0 resolve calls for non-allowlisted issue, got %d", len(fs.resolved))
	}
}

// A store error must be swallowed (logged, not propagated): the bell is a
// floor, never a gate (NOTIFICATIONS.md). HealthRaised returns nothing, so we
// just assert it doesn't panic and the fake recorded no successful raise.
func TestHealthRaised_StoreErrorSwallowed(t *testing.T) {
	fs := &fakeStore{raiseErr: errors.New("db down")}
	newNotifier(fs).HealthRaised(issue("data-drive-missing", "")) // must not panic
	if len(fs.raised) != 0 {
		t.Fatalf("raise should have errored before capture, got %d", len(fs.raised))
	}
}

// On a successful raise the notifier publishes notification.created onto the
// bus so the dashboard bell updates without a refresh. The payload is an
// advisory refetch trigger (dedup_key + category/severity for a future toast
// decision), not the notification body.
func TestHealthRaised_PublishesCreated(t *testing.T) {
	fs := &fakeStore{}
	fp := &fakePublisher{}
	newNotifierWithPub(fs, fp).HealthRaised(issue("data-drive-missing", ""))

	if len(fp.events) != 1 {
		t.Fatalf("want 1 published event, got %d", len(fp.events))
	}
	ev := fp.events[0]
	if ev.kind != events.NotificationCreated {
		t.Errorf("kind = %q, want notification.created", ev.kind)
	}
	if ev.data["dedup_key"] != "health:data-drive-missing" {
		t.Errorf("dedup_key payload = %v", ev.data["dedup_key"])
	}
	if ev.data["category"] != "storage" || ev.data["severity"] != "critical" {
		t.Errorf("payload = %v, want category=storage severity=critical", ev.data)
	}
}

// On a clear the notifier publishes notification.updated keyed to the same
// dedup_key (read/resolve/dismiss all ride this one kind).
func TestHealthCleared_PublishesUpdated(t *testing.T) {
	fs := &fakeStore{}
	fp := &fakePublisher{}
	newNotifierWithPub(fs, fp).HealthCleared("data-drive-missing", "")

	if len(fp.events) != 1 || fp.events[0].kind != events.NotificationUpdated {
		t.Fatalf("want 1 notification.updated event, got %v", fp.events)
	}
	if fp.events[0].data["dedup_key"] != "health:data-drive-missing" {
		t.Errorf("dedup_key payload = %v", fp.events[0].data["dedup_key"])
	}
}

// A store error skips the publish — nothing was written, so the bell must not
// be told a notification appeared.
func TestHealthRaised_StoreError_NoPublish(t *testing.T) {
	fs := &fakeStore{raiseErr: errors.New("db down")}
	fp := &fakePublisher{}
	newNotifierWithPub(fs, fp).HealthRaised(issue("data-drive-missing", ""))
	if len(fp.events) != 0 {
		t.Fatalf("want no publish on store error, got %d", len(fp.events))
	}
}

// Symmetric with TestHealthRaised_StoreError_NoPublish: a resolve that errors
// skips the publish — nothing changed in the store, so the bell must not be
// told a notification updated. Guards the early return in HealthCleared.
func TestHealthCleared_StoreError_NoPublish(t *testing.T) {
	fs := &fakeStore{resolveErr: errors.New("db down")}
	fp := &fakePublisher{}
	newNotifierWithPub(fs, fp).HealthCleared("data-drive-missing", "")
	if len(fp.events) != 0 {
		t.Fatalf("want no publish on resolve error, got %d", len(fp.events))
	}
}

// A non-allowlisted issue neither writes nor publishes.
func TestHealthRaised_NotAllowlisted_NoPublish(t *testing.T) {
	fs := &fakeStore{}
	fp := &fakePublisher{}
	newNotifierWithPub(fs, fp).HealthRaised(issue("health-report-malformed", ""))
	if len(fp.events) != 0 {
		t.Fatalf("want no publish for non-allowlisted issue, got %d", len(fp.events))
	}
}

// --- member transparency variant (slice 0028) -----------------------------

// A box-blocking storage issue (data-drive-missing) emits, alongside the admin
// actionable notification, an info-only transparency notice broadcast to members
// (NOTIFICATIONS.md # Member transparency variant): no action link, info
// severity regardless of the issue's own severity, keyed off the base dedup key
// plus :member so it coalesces and clears independently of the admin row.
func TestHealthRaised_MemberTransparency(t *testing.T) {
	fs := &fakeStore{}
	newNotifier(fs).HealthRaised(issue("data-drive-missing", ""))

	m, ok := raisedByKey(fs, "health:data-drive-missing:member")
	if !ok {
		t.Fatalf("no member transparency notice raised; raised keys = %v", raisedKeys(fs))
	}
	if m.Audience != AudienceMembers || m.Variant != VariantTransparency {
		t.Errorf("routing = {%q,%q}, want {members,transparency}", m.Audience, m.Variant)
	}
	if m.Severity != SeverityInfo {
		t.Errorf("severity = %q, want info (transparency is never the issue's own severity)", m.Severity)
	}
	if m.ActionLabel != "" || m.ActionRoute != "" {
		t.Errorf("action = {%q,%q}, want none (remediation is admin-gated)", m.ActionLabel, m.ActionRoute)
	}
	if m.Summary != memberPausedSummary || m.Body != memberPausedBody {
		t.Errorf("copy = {%q,%q}, want the member paused copy", m.Summary, m.Body)
	}
	if m.SourceID != "data-drive-missing" {
		t.Errorf("source_id = %q, want data-drive-missing", m.SourceID)
	}
}

// An allowlisted issue NOT marked memberTransparency (canary-mismatch — a
// System/state issue) notifies admins only, never broadcasting to members, even
// though it also blocks writes. Guards against gating transparency on the
// block-flags instead of the curated allowlist.
func TestHealthRaised_NonTransparency_NoMemberNotice(t *testing.T) {
	fs := &fakeStore{}
	newNotifier(fs).HealthRaised(issue("canary-mismatch", ""))

	if len(fs.raised) != 1 {
		t.Fatalf("want 1 raised (admin only), got %d: %v", len(fs.raised), raisedKeys(fs))
	}
	if fs.raised[0].Audience != AudienceAdmins {
		t.Errorf("audience = %q, want admins", fs.raised[0].Audience)
	}
}

// --- "all clear" on resolve (slice 0028) ----------------------------------

// Clearing a transparency issue resolves both problem rows AND emits an info
// "all clear" to each audience (NOTIFICATIONS.md # Clears): an admin-actionable
// resolved copy and a member transparency resolved copy, each keyed :cleared.
func TestHealthCleared_EmitsAllClear(t *testing.T) {
	fs := &fakeStore{}
	newNotifier(fs).HealthCleared("data-drive-missing", "")

	adminClear, ok := raisedByKey(fs, "health:data-drive-missing:cleared")
	if !ok {
		t.Fatalf("no admin all-clear raised; raised keys = %v", raisedKeys(fs))
	}
	if adminClear.Audience != AudienceAdmins || adminClear.Severity != SeverityInfo {
		t.Errorf("admin all-clear routing = {%q,%q}, want {admins,info}", adminClear.Audience, adminClear.Severity)
	}
	if adminClear.Summary != "Your data drive is reconnected." {
		t.Errorf("admin all-clear summary = %q, want the rule's clearSummary", adminClear.Summary)
	}

	memberClear, ok := raisedByKey(fs, "health:data-drive-missing:member:cleared")
	if !ok {
		t.Fatalf("no member all-clear raised; raised keys = %v", raisedKeys(fs))
	}
	if memberClear.Audience != AudienceMembers || memberClear.Severity != SeverityInfo {
		t.Errorf("member all-clear routing = {%q,%q}, want {members,info}", memberClear.Audience, memberClear.Severity)
	}
	if memberClear.Summary != memberClearSummary {
		t.Errorf("member all-clear summary = %q, want %q", memberClear.Summary, memberClearSummary)
	}
}

// A non-transparency issue's clear emits the admin all-clear only — no member
// row, mirroring the raise.
func TestHealthCleared_NonTransparency_AdminAllClearOnly(t *testing.T) {
	fs := &fakeStore{}
	newNotifier(fs).HealthCleared("canary-mismatch", "")

	if len(fs.raised) != 1 {
		t.Fatalf("want 1 all-clear raised (admin only), got %d: %v", len(fs.raised), raisedKeys(fs))
	}
	if fs.raised[0].DedupKey != "health:canary-mismatch:cleared" || fs.raised[0].Audience != AudienceAdmins {
		t.Errorf("all-clear = {%q,%q}, want {health:canary-mismatch:cleared,admins}", fs.raised[0].DedupKey, fs.raised[0].Audience)
	}
	// Only the admin problem is resolved (no member problem for this issue).
	if !contains(fs.resolved, "health:canary-mismatch") {
		t.Errorf("resolved = %v, want it to include the admin problem key", fs.resolved)
	}
	if contains(fs.resolved, "health:canary-mismatch:member") {
		t.Errorf("resolved = %v, must not touch a member key for a non-transparency issue", fs.resolved)
	}
}

// On raise the notifier resolves the paired all-clear keys, so a flap
// (clear → raise) retracts the now-false "reconnected" notice rather than
// leaving it next to the fresh problem (NOTIFICATIONS.md # Clears).
func TestHealthRaised_ResolvesStaleAllClear(t *testing.T) {
	fs := &fakeStore{}
	newNotifier(fs).HealthRaised(issue("data-drive-missing", ""))

	if !contains(fs.resolved, "health:data-drive-missing:cleared") {
		t.Errorf("resolved = %v, want the admin all-clear key retracted on raise", fs.resolved)
	}
	if !contains(fs.resolved, "health:data-drive-missing:member:cleared") {
		t.Errorf("resolved = %v, want the member all-clear key retracted on raise", fs.resolved)
	}
}

// --- raised-notification lookup helpers ---

func raisedByKey(fs *fakeStore, key string) (Notification, bool) {
	for _, n := range fs.raised {
		if n.DedupKey == key {
			return n, true
		}
	}
	return Notification{}, false
}

func raisedKeys(fs *fakeStore) []string {
	out := make([]string, len(fs.raised))
	for i, n := range fs.raised {
		out[i] = n.DedupKey
	}
	return out
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

// ValidCategory backs the mute API's input check (slice 0029). The full
// taxonomy must be recognized — even categories without a producer yet — and
// junk/case variants rejected, so a typo can't persist a no-op mute.
func TestValidCategory(t *testing.T) {
	if len(Categories) != 6 {
		t.Errorf("Categories has %d entries; want the 6 in NOTIFICATIONS.md # The notification model", len(Categories))
	}
	for _, c := range Categories {
		if !ValidCategory(string(c)) {
			t.Errorf("ValidCategory(%q) = false; want true", c)
		}
	}
	for _, bad := range []string{"", "bogus", "Storage", "STORAGE", "storage "} {
		if ValidCategory(bad) {
			t.Errorf("ValidCategory(%q) = true; want false", bad)
		}
	}
}
