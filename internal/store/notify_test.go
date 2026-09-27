package store

import (
	"fmt"
	"testing"
	"time"

	"github.com/onmoose/os/internal/notify"
)

func newNotification(dedupKey string) notify.Notification {
	return notify.Notification{
		TS:          time.Now().UTC().UnixMilli(),
		Category:    notify.CategoryStorage,
		Severity:    notify.SeverityCritical,
		SourceKind:  notify.SourceHealthIssue,
		SourceID:    "data-drive-missing",
		DedupKey:    dedupKey,
		Audience:    notify.AudienceAdmins,
		Variant:     notify.VariantActionable,
		Summary:     "Your data drive isn't connected.",
		Body:        "abc-123 absent",
		ActionLabel: "Open Storage",
		ActionRoute: "/settings/storage",
	}
}

func TestRaiseNotification_CreatesRow(t *testing.T) {
	s := open(t)
	if err := s.RaiseNotification(newNotification("health:data-drive-missing")); err != nil {
		t.Fatalf("raise: %v", err)
	}
	got, err := s.ListNotifications()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 notification, got %d", len(got))
	}
	n := got[0]
	if n.SourceID != "data-drive-missing" || n.DedupKey != "health:data-drive-missing" {
		t.Errorf("source/dedup = {%q,%q}", n.SourceID, n.DedupKey)
	}
	if n.Category != notify.CategoryStorage || n.Severity != notify.SeverityCritical {
		t.Errorf("category/severity = {%q,%q}", n.Category, n.Severity)
	}
	if n.Audience != notify.AudienceAdmins || n.Variant != notify.VariantActionable {
		t.Errorf("routing = {%q,%q}", n.Audience, n.Variant)
	}
	if n.ActionLabel != "Open Storage" || n.ActionRoute != "/settings/storage" {
		t.Errorf("action = {%q,%q}", n.ActionLabel, n.ActionRoute)
	}
	if n.ResolvedAt != 0 {
		t.Errorf("resolved_at = %d, want 0 (active)", n.ResolvedAt)
	}
}

// Re-raising the same dedup_key coalesces: one row, refreshed in place — not a
// second row (NOTIFICATIONS.md # One notification per raise).
func TestRaiseNotification_CoalescesByDedupKey(t *testing.T) {
	s := open(t)
	first := newNotification("health:data-drive-missing")
	first.TS = 1000
	first.Body = "first"
	if err := s.RaiseNotification(first); err != nil {
		t.Fatalf("first raise: %v", err)
	}
	// Vary every column the coalescing UPDATE refreshes — not just ts/body — so
	// a regression that dropped severity/summary/action from the UPDATE clause
	// is caught (with identical values it would be invisible).
	second := newNotification("health:data-drive-missing")
	second.TS = 2000
	second.Body = "second"
	second.Severity = notify.SeverityError
	second.Summary = "refreshed summary"
	second.ActionLabel = "Retry"
	second.ActionRoute = "/settings/storage/retry"
	if err := s.RaiseNotification(second); err != nil {
		t.Fatalf("second raise: %v", err)
	}

	got, err := s.ListNotifications()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 coalesced row, got %d", len(got))
	}
	n := got[0]
	if n.TS != 2000 || n.Body != "second" {
		t.Errorf("ts/body not refreshed: ts=%d body=%q, want 2000/second", n.TS, n.Body)
	}
	if n.Severity != notify.SeverityError || n.Summary != "refreshed summary" {
		t.Errorf("severity/summary not refreshed: {%q,%q}, want {error,refreshed summary}", n.Severity, n.Summary)
	}
	if n.ActionLabel != "Retry" || n.ActionRoute != "/settings/storage/retry" {
		t.Errorf("action not refreshed: {%q,%q}, want {Retry,/settings/storage/retry}", n.ActionLabel, n.ActionRoute)
	}
}

func TestRaiseNotification_DistinctKeysDistinctRows(t *testing.T) {
	s := open(t)
	if err := s.RaiseNotification(newNotification("health:data-drive-missing")); err != nil {
		t.Fatalf("raise 1: %v", err)
	}
	if err := s.RaiseNotification(newNotification("health:canary-mismatch")); err != nil {
		t.Fatalf("raise 2: %v", err)
	}
	got, err := s.ListNotifications()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 rows for 2 dedup keys, got %d", len(got))
	}
}

func TestResolveNotification_SetsResolvedAt(t *testing.T) {
	s := open(t)
	if err := s.RaiseNotification(newNotification("health:data-drive-missing")); err != nil {
		t.Fatalf("raise: %v", err)
	}
	at := time.Date(2026, 5, 29, 15, 0, 0, 0, time.UTC)
	if err := s.ResolveNotification("health:data-drive-missing", at); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	got, err := s.ListNotifications()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row (resolved, not deleted), got %d", len(got))
	}
	if got[0].ResolvedAt != at.UnixMilli() {
		t.Errorf("resolved_at = %d, want %d", got[0].ResolvedAt, at.UnixMilli())
	}
}

func TestResolveNotification_MissingKeyNoOp(t *testing.T) {
	s := open(t)
	if err := s.ResolveNotification("health:does-not-exist", time.Now()); err != nil {
		t.Fatalf("resolve of missing key should be a no-op, got: %v", err)
	}
	got, err := s.ListNotifications()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("want 0 rows, got %d", len(got))
	}
}

// A re-raise after a resolve un-resolves the existing row (flap) — still one
// row, resolved_at cleared, ts bumped. This is the cross-restart / drive-flap
// path the dedup_key defends.
func TestRaiseNotification_ReRaiseAfterResolveUnresolves(t *testing.T) {
	s := open(t)
	n := newNotification("health:data-drive-missing")
	n.TS = 1000
	if err := s.RaiseNotification(n); err != nil {
		t.Fatalf("raise: %v", err)
	}
	if err := s.ResolveNotification("health:data-drive-missing", time.UnixMilli(1500)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	reraise := newNotification("health:data-drive-missing")
	reraise.TS = 2000
	if err := s.RaiseNotification(reraise); err != nil {
		t.Fatalf("re-raise: %v", err)
	}
	got, err := s.ListNotifications()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 row after flap, got %d", len(got))
	}
	if got[0].ResolvedAt != 0 {
		t.Errorf("resolved_at = %d, want 0 (un-resolved by re-raise)", got[0].ResolvedAt)
	}
	if got[0].TS != 2000 {
		t.Errorf("ts = %d, want 2000 (bumped by re-raise)", got[0].TS)
	}
}

// --- read surface (slice 0026) -------------------------------------------

func seedUser(t *testing.T, s *Store, id, role string) {
	t.Helper()
	if err := s.CreateUser(User{ID: id, Username: id, DisplayName: id, Role: role, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("create user %s: %v", id, err)
	}
}

// userNotification builds a personal (audience=user) notification owned by
// userID — the shape a future app/account source produces.
func userNotification(dedupKey, userID string) notify.Notification {
	n := newNotification(dedupKey)
	n.Audience = notify.AudienceUser
	n.UserID = userID
	return n
}

// raiseAndID raises n and returns the active row's id (the read surface needs
// it to mark-read/dismiss a specific notification).
func raiseAndID(t *testing.T, s *Store, n notify.Notification) int64 {
	t.Helper()
	if err := s.RaiseNotification(n); err != nil {
		t.Fatalf("raise %s: %v", n.DedupKey, err)
	}
	var id int64
	if err := s.db.QueryRow(
		`SELECT id FROM notifications WHERE dedup_key=? AND dismissed_at IS NULL`,
		n.DedupKey).Scan(&id); err != nil {
		t.Fatalf("lookup %s: %v", n.DedupKey, err)
	}
	return id
}

func TestListNotificationsForRecipient_AudienceScoping(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	seedUser(t, s, "u_mem", RoleMember)
	seedUser(t, s, "u_other", RoleMember)

	// A box-wide (admins) notification, a personal one for u_mem, and a personal
	// one owned by the admin.
	if err := s.RaiseNotification(newNotification("health:data-drive-missing")); err != nil {
		t.Fatalf("raise admins: %v", err)
	}
	if err := s.RaiseNotification(userNotification("app:foo:u_mem", "u_mem")); err != nil {
		t.Fatalf("raise user: %v", err)
	}
	if err := s.RaiseNotification(userNotification("app:bar:u_admin", "u_admin")); err != nil {
		t.Fatalf("raise admin-own: %v", err)
	}

	// Admin sees the box-wide row + their own personal row, NOT u_mem's.
	admin, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin", IsAdmin: true})
	if err != nil {
		t.Fatalf("list admin: %v", err)
	}
	if !dedupSet(admin, "health:data-drive-missing", "app:bar:u_admin") {
		t.Errorf("admin sees %v, want {health:data-drive-missing, app:bar:u_admin}", dedups(admin))
	}

	// Member sees only their own personal row — never the box-wide one.
	mem, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_mem", IsAdmin: false})
	if err != nil {
		t.Fatalf("list member: %v", err)
	}
	if !dedupSet(mem, "app:foo:u_mem") {
		t.Errorf("member sees %v, want {app:foo:u_mem}", dedups(mem))
	}

	// An unrelated member sees nothing.
	other, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_other", IsAdmin: false})
	if err != nil {
		t.Fatalf("list other: %v", err)
	}
	if len(other) != 0 {
		t.Errorf("unrelated member sees %v, want none", dedups(other))
	}
}

// The 'members' broadcast audience (the transparency variant, slice 0028) is
// visible to every member and to no admin — the mirror of the 'admins'
// audience. Admins receive the actionable copy instead, so they must not also
// see the member transparency row.
func TestListNotificationsForRecipient_MembersAudience(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	seedUser(t, s, "u_mem", RoleMember)

	box := newNotification("health:data-drive-missing")
	if err := s.RaiseNotification(box); err != nil {
		t.Fatalf("raise admins: %v", err)
	}
	members := newNotification("health:data-drive-missing:member")
	members.Audience = notify.AudienceMembers
	members.Variant = notify.VariantTransparency
	if err := s.RaiseNotification(members); err != nil {
		t.Fatalf("raise members: %v", err)
	}

	// Member sees the broadcast members row, never the admins row.
	mem, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_mem", IsAdmin: false})
	if err != nil {
		t.Fatalf("list member: %v", err)
	}
	if !dedupSet(mem, "health:data-drive-missing:member") {
		t.Errorf("member sees %v, want {health:data-drive-missing:member}", dedups(mem))
	}

	// Admin sees the admins row, never the members transparency row.
	admin, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin", IsAdmin: true})
	if err != nil {
		t.Fatalf("list admin: %v", err)
	}
	if !dedupSet(admin, "health:data-drive-missing") {
		t.Errorf("admin sees %v, want {health:data-drive-missing} only (not the member row)", dedups(admin))
	}

	// The member's badge counts the broadcast row.
	if c, _ := s.CountUnreadNotifications("u_mem", false); c != 1 {
		t.Errorf("member unread = %d, want 1 (the members broadcast)", c)
	}
}

func TestListNotificationsForRecipient_ExcludesDismissed(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	id := raiseAndID(t, s, newNotification("health:data-drive-missing"))

	if err := s.DismissNotification(id, "u_admin", time.UnixMilli(1000)); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	def, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin", IsAdmin: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(def) != 0 {
		t.Errorf("default list shows dismissed row: %v", dedups(def))
	}

	inc, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin", IsAdmin: true, IncludeDismissed: true})
	if err != nil {
		t.Fatalf("list include-dismissed: %v", err)
	}
	if len(inc) != 1 {
		t.Errorf("include-dismissed list = %d rows, want 1", len(inc))
	}
}

func TestListNotificationsForRecipient_Cursor(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	for _, k := range []string{"health:a", "health:b", "health:c"} {
		if err := s.RaiseNotification(newNotification(k)); err != nil {
			t.Fatalf("raise %s: %v", k, err)
		}
	}

	page1, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin", IsAdmin: true, Limit: 2})
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 = %d rows, want 2", len(page1))
	}
	// Newest-first: ids descending.
	if page1[0].ID <= page1[1].ID {
		t.Errorf("page1 not newest-first: %d then %d", page1[0].ID, page1[1].ID)
	}

	page2, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin", IsAdmin: true, Limit: 2, AfterID: page1[1].ID})
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 = %d rows, want 1 (the remaining oldest)", len(page2))
	}
	if page2[0].ID >= page1[1].ID {
		t.Errorf("cursor leaked: page2 id %d not < %d", page2[0].ID, page1[1].ID)
	}
}

func TestCountUnreadNotifications(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	id1 := raiseAndID(t, s, newNotification("health:a"))
	_ = raiseAndID(t, s, newNotification("health:b"))

	if c, _ := s.CountUnreadNotifications("u_admin", true); c != 2 {
		t.Fatalf("initial unread = %d, want 2", c)
	}

	if err := s.MarkNotificationRead(id1, "u_admin", time.UnixMilli(1000)); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if c, _ := s.CountUnreadNotifications("u_admin", true); c != 1 {
		t.Errorf("after one read, unread = %d, want 1", c)
	}

	// A member with no visible notifications has a zero badge (no admins access).
	if c, _ := s.CountUnreadNotifications("u_mem_absent", false); c != 0 {
		t.Errorf("member with no rows: unread = %d, want 0", c)
	}
}

func TestMarkNotificationRead_PreservesFirstRead(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	id := raiseAndID(t, s, newNotification("health:a"))

	if err := s.MarkNotificationRead(id, "u_admin", time.UnixMilli(1000)); err != nil {
		t.Fatalf("first read: %v", err)
	}
	if err := s.MarkNotificationRead(id, "u_admin", time.UnixMilli(5000)); err != nil {
		t.Fatalf("second read: %v", err)
	}

	got, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin", IsAdmin: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(got) != 1 || got[0].ReadAt != 1000 {
		t.Fatalf("read_at = %v, want first-read 1000 preserved", got)
	}
}

// Dismiss is per-recipient: one admin dismissing a box-wide notification does
// not remove it from another admin's inbox (NOTIFICATIONS.md # Read/unread).
func TestDismissNotification_PerRecipient(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin1", RoleAdmin)
	seedUser(t, s, "u_admin2", RoleAdmin)
	id := raiseAndID(t, s, newNotification("health:data-drive-missing"))

	if err := s.DismissNotification(id, "u_admin1", time.UnixMilli(1000)); err != nil {
		t.Fatalf("dismiss: %v", err)
	}

	one, _ := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin1", IsAdmin: true})
	if len(one) != 0 {
		t.Errorf("admin1 still sees dismissed row: %v", dedups(one))
	}
	two, _ := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin2", IsAdmin: true})
	if len(two) != 1 {
		t.Errorf("admin2 lost the row to admin1's dismiss: %v", dedups(two))
	}
	if c, _ := s.CountUnreadNotifications("u_admin2", true); c != 1 {
		t.Errorf("admin2 badge = %d, want 1 (unaffected)", c)
	}
}

func TestMarkAllNotificationsRead(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	for _, k := range []string{"health:a", "health:b", "health:c"} {
		if err := s.RaiseNotification(newNotification(k)); err != nil {
			t.Fatalf("raise %s: %v", k, err)
		}
	}

	if err := s.MarkAllNotificationsRead("u_admin", true, time.UnixMilli(2000)); err != nil {
		t.Fatalf("mark all: %v", err)
	}
	if c, _ := s.CountUnreadNotifications("u_admin", true); c != 0 {
		t.Errorf("unread after mark-all = %d, want 0", c)
	}
	// Read, not dismissed: the rows stay in the inbox, just no longer unread.
	got, _ := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin", IsAdmin: true})
	if len(got) != 3 {
		t.Fatalf("list after mark-all = %d rows, want 3 (read ≠ removed)", len(got))
	}
	for _, n := range got {
		if n.ReadAt == 0 {
			t.Errorf("%s still unread after mark-all", n.DedupKey)
		}
	}
}

// Mark-all-read goes through the same visibility clause as list/count, so a
// member's mark-all must reach their broadcast 'members' rows (and their own
// 'user' rows) while never touching 'admins' rows. Symmetry guard for the
// member branch of the clause across all three read queries (slice 0028).
func TestMarkAllNotificationsRead_MemberAudience(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	seedUser(t, s, "u_mem", RoleMember)

	members := newNotification("health:data-drive-missing:member")
	members.Audience = notify.AudienceMembers
	if err := s.RaiseNotification(members); err != nil {
		t.Fatalf("raise members: %v", err)
	}
	own := userNotification("app:foo:u_mem", "u_mem")
	if err := s.RaiseNotification(own); err != nil {
		t.Fatalf("raise user: %v", err)
	}
	// An admins row the member must not be able to read away.
	if err := s.RaiseNotification(newNotification("health:canary-mismatch")); err != nil {
		t.Fatalf("raise admins: %v", err)
	}

	if c, _ := s.CountUnreadNotifications("u_mem", false); c != 2 {
		t.Fatalf("member unread before mark-all = %d, want 2", c)
	}
	if err := s.MarkAllNotificationsRead("u_mem", false, time.UnixMilli(2000)); err != nil {
		t.Fatalf("mark all: %v", err)
	}
	if c, _ := s.CountUnreadNotifications("u_mem", false); c != 0 {
		t.Errorf("member unread after mark-all = %d, want 0 (members + own rows read)", c)
	}
	// The admin still sees the admins row as unread — the member's mark-all did
	// not reach it.
	if c, _ := s.CountUnreadNotifications("u_admin", true); c != 1 {
		t.Errorf("admin unread = %d, want 1 (untouched by member's mark-all)", c)
	}
}

// A re-raise (coalesce) is a fresh occurrence and must re-surface: the prior
// per-recipient read state is cleared so the badge lights up again.
func TestRaiseNotification_ReRaiseClearsReadState(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	id := raiseAndID(t, s, newNotification("health:data-drive-missing"))

	if err := s.MarkNotificationRead(id, "u_admin", time.UnixMilli(1000)); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if c, _ := s.CountUnreadNotifications("u_admin", true); c != 0 {
		t.Fatalf("after read, unread = %d, want 0", c)
	}

	// Issue clears, then flaps back: re-raise coalesces onto the same row.
	if err := s.ResolveNotification("health:data-drive-missing", time.UnixMilli(1500)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := s.RaiseNotification(newNotification("health:data-drive-missing")); err != nil {
		t.Fatalf("re-raise: %v", err)
	}

	if c, _ := s.CountUnreadNotifications("u_admin", true); c != 1 {
		t.Errorf("after re-raise, unread = %d, want 1 (re-surfaced)", c)
	}
}

func TestGetNotification_NotFound(t *testing.T) {
	s := open(t)
	if _, err := s.GetNotification(999); err != ErrNotFound {
		t.Fatalf("GetNotification(missing) err = %v, want ErrNotFound", err)
	}
}

// --- per-category mute (slice 0029) --------------------------------------

// A muted category drops out of the caller's list and unread count; other
// categories are untouched (NOTIFICATIONS.md # Configuration).
func TestNotificationMute_HidesCategoryFromListAndCount(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	if err := s.RaiseNotification(newNotification("health:data-drive-missing")); err != nil { // storage
		t.Fatalf("raise storage: %v", err)
	}
	sys := newNotification("health:canary-mismatch")
	sys.Category = notify.CategorySystem
	if err := s.RaiseNotification(sys); err != nil {
		t.Fatalf("raise system: %v", err)
	}

	if err := s.MuteNotificationCategory("u_admin", string(notify.CategoryStorage)); err != nil {
		t.Fatalf("mute: %v", err)
	}

	got, err := s.ListNotificationsForRecipient(NotificationFilter{UserID: "u_admin", IsAdmin: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !dedupSet(got, "health:canary-mismatch") {
		t.Errorf("after muting storage, list = %v, want {health:canary-mismatch}", dedups(got))
	}
	if c, _ := s.CountUnreadNotifications("u_admin", true); c != 1 {
		t.Errorf("unread after mute = %d, want 1 (system only)", c)
	}
}

// Unmuting restores a category — its notifications reappear in the list/count.
func TestNotificationMute_Unmute(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	if err := s.RaiseNotification(newNotification("health:data-drive-missing")); err != nil {
		t.Fatalf("raise: %v", err)
	}
	cat := string(notify.CategoryStorage)
	if err := s.MuteNotificationCategory("u_admin", cat); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if c, _ := s.CountUnreadNotifications("u_admin", true); c != 0 {
		t.Fatalf("muted unread = %d, want 0", c)
	}
	if err := s.UnmuteNotificationCategory("u_admin", cat); err != nil {
		t.Fatalf("unmute: %v", err)
	}
	if c, _ := s.CountUnreadNotifications("u_admin", true); c != 1 {
		t.Errorf("unmuted unread = %d, want 1 (restored)", c)
	}
}

// A mute is one user's preference: it never affects another user's view.
func TestNotificationMute_PerUser(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin1", RoleAdmin)
	seedUser(t, s, "u_admin2", RoleAdmin)
	if err := s.RaiseNotification(newNotification("health:data-drive-missing")); err != nil {
		t.Fatalf("raise: %v", err)
	}
	if err := s.MuteNotificationCategory("u_admin1", string(notify.CategoryStorage)); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if c, _ := s.CountUnreadNotifications("u_admin1", true); c != 0 {
		t.Errorf("admin1 (muted) unread = %d, want 0", c)
	}
	if c, _ := s.CountUnreadNotifications("u_admin2", true); c != 1 {
		t.Errorf("admin2 (not muted) unread = %d, want 1", c)
	}
}

// Mute and unmute are idempotent; ListMutedCategories returns the set sorted.
func TestNotificationMute_IdempotentAndListed(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	// Mute out of order, with a repeat, to exercise idempotency + sorted output.
	for _, c := range []string{"updates", "storage", "updates", "system"} {
		if err := s.MuteNotificationCategory("u_admin", c); err != nil {
			t.Fatalf("mute %s: %v", c, err)
		}
	}
	muted, err := s.ListMutedCategories("u_admin")
	if err != nil {
		t.Fatalf("list muted: %v", err)
	}
	want := []string{"storage", "system", "updates"}
	if len(muted) != len(want) {
		t.Fatalf("muted = %v, want %v", muted, want)
	}
	for i := range want {
		if muted[i] != want[i] {
			t.Fatalf("muted = %v, want sorted %v", muted, want)
		}
	}
	// Unmute is a no-op when absent and safe to repeat.
	if err := s.UnmuteNotificationCategory("u_admin", "security"); err != nil {
		t.Errorf("unmute absent category: %v", err)
	}
	if err := s.UnmuteNotificationCategory("u_admin", "storage"); err != nil {
		t.Errorf("unmute: %v", err)
	}
	if err := s.UnmuteNotificationCategory("u_admin", "storage"); err != nil {
		t.Errorf("double unmute: %v", err)
	}
	muted, _ = s.ListMutedCategories("u_admin")
	if len(muted) != 2 {
		t.Errorf("after unmute storage, muted = %v, want {system, updates}", muted)
	}
}

// Mark-all-read applies the mute filter: a muted category is left untouched, so
// unmuting later reveals its notifications still unread (the user never saw
// them). Pins the consistency of the three aggregate read queries.
func TestMarkAllNotificationsRead_SkipsMuted(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	if err := s.RaiseNotification(newNotification("health:data-drive-missing")); err != nil { // storage
		t.Fatalf("raise storage: %v", err)
	}
	sys := newNotification("health:canary-mismatch")
	sys.Category = notify.CategorySystem
	if err := s.RaiseNotification(sys); err != nil {
		t.Fatalf("raise system: %v", err)
	}
	if err := s.MuteNotificationCategory("u_admin", string(notify.CategoryStorage)); err != nil {
		t.Fatalf("mute: %v", err)
	}

	if err := s.MarkAllNotificationsRead("u_admin", true, time.UnixMilli(2000)); err != nil {
		t.Fatalf("mark all: %v", err)
	}
	// Unmute storage — its notification must still be unread (mark-all skipped it).
	if err := s.UnmuteNotificationCategory("u_admin", string(notify.CategoryStorage)); err != nil {
		t.Fatalf("unmute: %v", err)
	}
	if c, _ := s.CountUnreadNotifications("u_admin", true); c != 1 {
		t.Errorf("after unmute, unread = %d, want 1 (storage row never marked read)", c)
	}
}

// The mute filter is audience-independent: a member muting a category drops the
// members-broadcast rows of that category from their list/count too.
func TestNotificationMute_MembersAudience(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_mem", RoleMember)
	members := newNotification("health:data-drive-missing:member") // storage category
	members.Audience = notify.AudienceMembers
	members.Variant = notify.VariantTransparency
	if err := s.RaiseNotification(members); err != nil {
		t.Fatalf("raise members: %v", err)
	}
	if c, _ := s.CountUnreadNotifications("u_mem", false); c != 1 {
		t.Fatalf("member unread before mute = %d, want 1", c)
	}
	if err := s.MuteNotificationCategory("u_mem", string(notify.CategoryStorage)); err != nil {
		t.Fatalf("mute: %v", err)
	}
	if c, _ := s.CountUnreadNotifications("u_mem", false); c != 0 {
		t.Errorf("member unread after muting storage = %d, want 0", c)
	}
}

// --- retention / pruning (PruneNotifications) ----------------------------

// Rows older than the age cap are deleted; rows inside the window are kept. The
// age pass is state-blind — it deletes by ts alone.
func TestPruneNotifications_AgeCutoff(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	old := newNotification("health:old")
	old.TS = now.Add(-91 * 24 * time.Hour).UnixMilli()
	if err := s.RaiseNotification(old); err != nil {
		t.Fatalf("raise old: %v", err)
	}
	recent := newNotification("health:recent")
	recent.TS = now.Add(-89 * 24 * time.Hour).UnixMilli()
	if err := s.RaiseNotification(recent); err != nil {
		t.Fatalf("raise recent: %v", err)
	}
	// Exactly on the cutoff (now − 90d): kept, because the age predicate is a
	// strict `ts < cutoff`. Pins `<` vs `<=` — a `<=` mutant deletes this row.
	// Same expression the implementation uses, so the ms values are identical.
	boundary := newNotification("health:boundary")
	boundary.TS = now.Add(-notifyMaxAgeDays * 24 * time.Hour).UnixMilli()
	if err := s.RaiseNotification(boundary); err != nil {
		t.Fatalf("raise boundary: %v", err)
	}

	if err := s.PruneNotifications(now); err != nil {
		t.Fatalf("prune: %v", err)
	}

	got, err := s.ListNotifications()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !dedupSet(got, "health:recent", "health:boundary") {
		t.Errorf("after prune list = %v, want {health:recent, health:boundary} (91d-old pruned; 89d-old and exactly-90d kept)", dedups(got))
	}
}

// Pruning a notification cascades to its per-recipient notification_reads rows
// (ON DELETE CASCADE + foreign_keys=ON). Fails loud if the pragma or the FK
// ever regresses, leaving orphaned read rows.
func TestPruneNotifications_CascadesToReads(t *testing.T) {
	s := open(t)
	seedUser(t, s, "u_admin", RoleAdmin)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	old := newNotification("health:old")
	old.TS = now.Add(-91 * 24 * time.Hour).UnixMilli()
	id := raiseAndID(t, s, old)
	if err := s.MarkNotificationRead(id, "u_admin", time.UnixMilli(old.TS)); err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if err := s.DismissNotification(id, "u_admin", time.UnixMilli(old.TS)); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if n := countReads(t, s, id); n != 1 {
		t.Fatalf("setup: want 1 notification_reads row, got %d", n)
	}

	if err := s.PruneNotifications(now); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if n := countReads(t, s, id); n != 0 {
		t.Errorf("notification_reads not cascade-deleted: %d row(s) remain after prune", n)
	}
}

// When over the row cap, the oldest excess is trimmed resolved-rows-first: an
// equally-old resolved row is dropped before an active one (a cleared issue is
// history; a live one is not).
func TestPruneNotifications_CountCapDropsResolvedBeforeActive(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	// The active row is deliberately OLDER than the resolved row. Under a plain
	// ts-ASC trim — the degenerate mutant where the resolved-first key is
	// dropped — the oldest row (the active one) would be deleted. The
	// `(resolved_at IS NOT NULL) DESC` key must override ts so the *resolved*
	// row goes first despite being newer. Asserting the older active row
	// SURVIVES while the newer resolved row is DROPPED makes resolved-first the
	// only ordering that can produce this outcome — defeating both a ts-only
	// mutant and any rowid tie-break (the two rows have distinct ts, so no tie).
	active := newNotification("cap:active")
	active.TS = now.Add(-3 * 24 * time.Hour).UnixMilli()
	if err := s.RaiseNotification(active); err != nil {
		t.Fatalf("raise active: %v", err)
	}
	resolved := newNotification("cap:resolved")
	resolved.TS = now.Add(-1 * 24 * time.Hour).UnixMilli()
	if err := s.RaiseNotification(resolved); err != nil {
		t.Fatalf("raise resolved: %v", err)
	}
	if err := s.ResolveNotification("cap:resolved", now.Add(-1*24*time.Hour)); err != nil {
		t.Fatalf("resolve: %v", err)
	}

	// Active fillers sit between the two (older than resolved, newer than active)
	// so with excess == 1 they're never the trim victim under any ordering — keeping
	// the test focused on the active-vs-resolved choice. All inside the age window,
	// so only the count pass acts.
	bulkSeedActive(t, s, notifyMaxRows-1, now.Add(-2*24*time.Hour).UnixMilli(), "cap:filler")
	if total := countNotifications(t, s); total != notifyMaxRows+1 {
		t.Fatalf("setup: seeded %d rows, want %d", total, notifyMaxRows+1)
	}

	if err := s.PruneNotifications(now); err != nil {
		t.Fatalf("prune: %v", err)
	}

	if total := countNotifications(t, s); total != notifyMaxRows {
		t.Errorf("after count-cap prune total = %d, want %d", total, notifyMaxRows)
	}
	if exists(t, s, "cap:resolved") {
		t.Errorf("newer resolved row survived the cap trim — resolved rows must be dropped before active ones")
	}
	if !exists(t, s, "cap:active") {
		t.Errorf("older active row was dropped — the resolved-first clause should have dropped the newer resolved row instead")
	}
}

// Pruning a table inside both caps is a no-op, and is idempotent: a second run
// changes nothing.
func TestPruneNotifications_NoOpWithinCaps(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	// Empty table: harmless.
	if err := s.PruneNotifications(now); err != nil {
		t.Fatalf("prune empty: %v", err)
	}

	n := newNotification("health:recent")
	n.TS = now.Add(-1 * 24 * time.Hour).UnixMilli()
	if err := s.RaiseNotification(n); err != nil {
		t.Fatalf("raise: %v", err)
	}
	for i := 0; i < 2; i++ {
		if err := s.PruneNotifications(now); err != nil {
			t.Fatalf("prune %d: %v", i, err)
		}
		got, err := s.ListNotifications()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("within-caps prune %d changed state: %d rows, want 1", i, len(got))
		}
	}
}

// Pruning fully removes a row — it doesn't merely hide it — so its dedup_key is
// freed: a later re-raise of the same key inserts cleanly without colliding on
// the notifications_active_dedup partial unique index.
func TestPruneNotifications_FreesDedupForReRaise(t *testing.T) {
	s := open(t)
	now := time.Date(2026, 5, 31, 12, 0, 0, 0, time.UTC)

	old := newNotification("health:flap")
	old.TS = now.Add(-91 * 24 * time.Hour).UnixMilli()
	if err := s.RaiseNotification(old); err != nil {
		t.Fatalf("raise: %v", err)
	}
	if err := s.PruneNotifications(now); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if got, _ := s.ListNotifications(); len(got) != 0 {
		t.Fatalf("aged row not pruned: %v", dedups(got))
	}

	reraise := newNotification("health:flap")
	reraise.TS = now.UnixMilli()
	if err := s.RaiseNotification(reraise); err != nil {
		t.Fatalf("re-raise of a pruned key should insert cleanly, got: %v", err)
	}
	got, _ := s.ListNotifications()
	if !dedupSet(got, "health:flap") {
		t.Errorf("re-raised row missing: %v", dedups(got))
	}
}

// --- small assertions helpers ---

// bulkSeedActive inserts n active (unresolved) notifications at ts via a single
// transaction — a fast seed for the count-cap test, which must exceed
// notifyMaxRows. Reuses the canonical column values from newNotification.
func bulkSeedActive(t *testing.T, s *Store, n int, ts int64, keyPrefix string) {
	t.Helper()
	tmpl := newNotification("")
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	stmt, err := tx.Prepare(`INSERT INTO notifications
		(ts, category, severity, source_kind, source_id, dedup_key, audience, variant, summary)
		VALUES (?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(ts, string(tmpl.Category), string(tmpl.Severity),
			tmpl.SourceKind, tmpl.SourceID, fmt.Sprintf("%s:%d", keyPrefix, i),
			tmpl.Audience, tmpl.Variant, tmpl.Summary); err != nil {
			t.Fatalf("insert filler %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func countNotifications(t *testing.T, s *Store) int {
	t.Helper()
	var c int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notifications`).Scan(&c); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return c
}

func countReads(t *testing.T, s *Store, id int64) int {
	t.Helper()
	var c int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notification_reads WHERE notification_id=?`, id).Scan(&c); err != nil {
		t.Fatalf("count reads: %v", err)
	}
	return c
}

func exists(t *testing.T, s *Store, dedupKey string) bool {
	t.Helper()
	var c int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notifications WHERE dedup_key=?`, dedupKey).Scan(&c); err != nil {
		t.Fatalf("exists %s: %v", dedupKey, err)
	}
	return c > 0
}

func dedups(ns []notify.Notification) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = n.DedupKey
	}
	return out
}

func dedupSet(ns []notify.Notification, want ...string) bool {
	if len(ns) != len(want) {
		return false
	}
	have := map[string]bool{}
	for _, n := range ns {
		have[n.DedupKey] = true
	}
	for _, w := range want {
		if !have[w] {
			return false
		}
	}
	return true
}
