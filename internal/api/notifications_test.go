package api

import (
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/onmoose/os/internal/notify"
)

// --- harness helpers for the bell read surface ---------------------------

// seedNotification writes one notification straight through the store (the
// write seam is tested in internal/notify and internal/store; here we only
// need a row to read back) and returns its id. Each call needs a distinct
// dedupKey — the active-row partial unique index rejects a duplicate.
func (h *harness) seedNotification(dedupKey, audience, userID string) int64 {
	h.t.Helper()
	if err := h.st.RaiseNotification(notify.Notification{
		TS:         time.Now().UnixMilli(),
		Category:   notify.CategoryStorage,
		Severity:   notify.SeverityCritical,
		SourceKind: notify.SourceHealthIssue,
		SourceID:   "data-drive-missing",
		DedupKey:   dedupKey,
		Audience:   audience,
		UserID:     userID,
		Variant:    notify.VariantActionable,
		Summary:    "seeded notification",
	}); err != nil {
		h.t.Fatalf("seed notification %q: %v", dedupKey, err)
	}
	all, err := h.st.ListNotifications()
	if err != nil {
		h.t.Fatalf("list notifications: %v", err)
	}
	for _, n := range all {
		if n.DedupKey == dedupKey {
			return n.ID
		}
	}
	h.t.Fatalf("seeded notification %q not found after raise", dedupKey)
	return 0
}

// bellList fetches the caller's notification inbox via the wire.
func (h *harness) bellList() []NotificationDTO {
	h.t.Helper()
	resp := h.do("GET", "/api/v1/notifications", nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("list notifications: %d", resp.StatusCode)
	}
	return decodeJSON[struct {
		Notifications []NotificationDTO `json:"notifications"`
	}](h.t, resp).Notifications
}

// bellCount fetches the caller's unread badge count via the wire.
func (h *harness) bellCount() int {
	h.t.Helper()
	resp := h.do("GET", "/api/v1/notifications/unread-count", nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("unread-count: %d", resp.StatusCode)
	}
	return decodeJSON[struct {
		Count int `json:"count"`
	}](h.t, resp).Count
}

func hasID(ns []NotificationDTO, id int64) bool {
	for _, n := range ns {
		if n.ID == id {
			return true
		}
	}
	return false
}

// mutedCategories fetches the caller's muted categories via the wire.
func (h *harness) mutedCategories() []string {
	h.t.Helper()
	resp := h.do("GET", "/api/v1/notifications/mutes", nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("list mutes: %d", resp.StatusCode)
	}
	return decodeJSON[struct {
		Muted []string `json:"muted"`
	}](h.t, resp).Muted
}

// --- tests ----------------------------------------------------------------

// The whole bell surface sits behind the auth fence — an unauthenticated
// caller gets 401 on every route, not an empty list.
func TestNotifications_RequireAuth(t *testing.T) {
	h := newHarness(t) // no setup → no session

	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/v1/notifications"},
		{"GET", "/api/v1/notifications/unread-count"},
		{"POST", "/api/v1/notifications/1/read"},
		{"POST", "/api/v1/notifications/read-all"},
		{"POST", "/api/v1/notifications/1/dismiss"},
		{"GET", "/api/v1/notifications/mutes"},
		{"PUT", "/api/v1/notifications/mutes/storage"},
		{"DELETE", "/api/v1/notifications/mutes/storage"},
	} {
		resp := h.do(tc.method, tc.path, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s %s = %d; want 401", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// Audience scoping over the wire: an 'admins' notification reaches every admin
// but no member; a 'user' notification reaches only its owner — not other
// members, and not admins (who see box-wide + their own, never another user's).
func TestNotifications_AudienceScoping(t *testing.T) {
	h := newHarness(t)
	alice := h.setupAdmin("alice", "pass1") // admin session in jar
	h.addMember("u_bob", "bob", "bobpass")

	adminID := h.seedNotification("box:1", notify.AudienceAdmins, "")
	bobID := h.seedNotification("user:bob:1", notify.AudienceUser, "u_bob")
	aliceID := h.seedNotification("user:alice:1", notify.AudienceUser, alice.ID)

	// Alice (admin) sees the box-wide row and her own user row, not bob's.
	got := h.bellList()
	if !hasID(got, adminID) {
		t.Error("admin alice should see the admins-audience notification")
	}
	if !hasID(got, aliceID) {
		t.Error("admin alice should see her own user-audience notification")
	}
	if hasID(got, bobID) {
		t.Error("admin alice must not see bob's user-audience notification")
	}

	// Bob (member) sees only his own user row — not the box-wide admins one.
	h.loginAs("bob", "bobpass")
	got = h.bellList()
	if !hasID(got, bobID) {
		t.Error("member bob should see his own user-audience notification")
	}
	if hasID(got, adminID) {
		t.Error("member bob must not see the admins-audience notification")
	}
	if hasID(got, aliceID) {
		t.Error("member bob must not see alice's user-audience notification")
	}
}

// The 'members' broadcast audience (transparency variant, slice 0028) over the
// wire: every member sees it and can act on it; no admin does — and an admin
// gets 404 (not 403) trying to touch it, the same information-hiding the foreign
// rows get.
func TestNotifications_MembersAudienceScoping(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1") // admin session in jar
	h.addMember("u_bob", "bob", "bobpass")

	memberID := h.seedNotification("members:1", notify.AudienceMembers, "")

	// Alice (admin) does not see the members broadcast.
	if hasID(h.bellList(), memberID) {
		t.Error("admin alice must not see the members-audience notification")
	}
	// And cannot act on it — 404, not 403.
	resp := h.do("POST", fmt.Sprintf("/api/v1/notifications/%d/read", memberID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("admin marking members row = %d; want 404", resp.StatusCode)
	}

	// Bob (member) sees it and can mark it read.
	h.loginAs("bob", "bobpass")
	if !hasID(h.bellList(), memberID) {
		t.Error("member bob should see the members-audience notification")
	}
	resp = h.do("POST", fmt.Sprintf("/api/v1/notifications/%d/read", memberID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("member marking members row = %d; want 204", resp.StatusCode)
	}
	if c := h.bellCount(); c != 0 {
		t.Errorf("member unread after read = %d; want 0", c)
	}
}

// Marking a notification read drops the badge count and flips its `read` flag,
// but the row stays in the list (read ≠ dismissed).
func TestNotifications_MarkReadDropsCount(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	id := h.seedNotification("box:1", notify.AudienceAdmins, "")

	if c := h.bellCount(); c != 1 {
		t.Fatalf("unread count before read = %d; want 1", c)
	}
	// A freshly seeded notification reports read=false over the wire — pins the
	// lower end of the DTO's ReadAt→Read derivation (the count query is separate).
	for _, n := range h.bellList() {
		if n.ID == id && n.Read {
			t.Error("freshly seeded notification should be read=false before mark-read")
		}
	}

	resp := h.do("POST", fmt.Sprintf("/api/v1/notifications/%d/read", id), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mark read = %d; want 204", resp.StatusCode)
	}

	if c := h.bellCount(); c != 0 {
		t.Errorf("unread count after read = %d; want 0", c)
	}
	got := h.bellList()
	if !hasID(got, id) {
		t.Fatal("read notification should still appear in the list")
	}
	for _, n := range got {
		if n.ID == id && !n.Read {
			t.Error("notification should be marked read=true after mark-read")
		}
	}
}

// Dismissing removes the notification from the active inbox (and from the
// badge), without resolving the underlying condition.
func TestNotifications_DismissRemovesFromList(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	id := h.seedNotification("box:1", notify.AudienceAdmins, "")

	resp := h.do("POST", fmt.Sprintf("/api/v1/notifications/%d/dismiss", id), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("dismiss = %d; want 204", resp.StatusCode)
	}

	if hasID(h.bellList(), id) {
		t.Error("dismissed notification must not appear in the active inbox")
	}
	if c := h.bellCount(); c != 0 {
		t.Errorf("unread count after dismiss = %d; want 0", c)
	}
}

// Mark-all-read zeroes the badge across every visible notification in one call.
func TestNotifications_MarkAllReadZeroesCount(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.seedNotification("box:1", notify.AudienceAdmins, "")
	h.seedNotification("box:2", notify.AudienceAdmins, "")

	if c := h.bellCount(); c != 2 {
		t.Fatalf("unread count before mark-all = %d; want 2", c)
	}

	resp := h.do("POST", "/api/v1/notifications/read-all", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mark-all-read = %d; want 204", resp.StatusCode)
	}

	if c := h.bellCount(); c != 0 {
		t.Errorf("unread count after mark-all = %d; want 0", c)
	}
}

// A per-id mutation answers 404 — never 403 — for both a missing id and a row
// the caller can't see, so the inbox leaks no information about which ids exist
// or who else they're addressed to.
func TestNotifications_NotFoundMissingAndForeign(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", "bob", "bobpass")

	bobID := h.seedNotification("user:bob:1", notify.AudienceUser, "u_bob")
	adminID := h.seedNotification("box:1", notify.AudienceAdmins, "")

	// Missing id → 404 (admin caller).
	resp := h.do("POST", "/api/v1/notifications/99999/read", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("read missing id as admin = %d; want 404", resp.StatusCode)
	}

	// Foreign id (bob's user-scoped row) → 404 for the admin, who can't see it.
	resp = h.do("POST", fmt.Sprintf("/api/v1/notifications/%d/read", bobID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("read foreign user row as admin = %d; want 404", resp.StatusCode)
	}

	// And symmetrically: a member can't touch the admins-audience row.
	h.loginAs("bob", "bobpass")
	resp = h.do("POST", fmt.Sprintf("/api/v1/notifications/%d/dismiss", adminID), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("member dismissing admins row = %d; want 404", resp.StatusCode)
	}
}

// Muting a category over the wire drops it from the caller's list and badge;
// GET reflects the mute; DELETE restores it. Round-trips the mute surface
// (NOTIFICATIONS.md # Configuration).
func TestNotifications_MuteHidesCategory(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	id := h.seedNotification("box:1", notify.AudienceAdmins, "") // storage category

	if c := h.bellCount(); c != 1 {
		t.Fatalf("unread before mute = %d; want 1", c)
	}
	if m := h.mutedCategories(); len(m) != 0 {
		t.Fatalf("muted before any mute = %v; want empty", m)
	}

	resp := h.do("PUT", "/api/v1/notifications/mutes/storage", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mute = %d; want 204", resp.StatusCode)
	}

	if m := h.mutedCategories(); len(m) != 1 || m[0] != "storage" {
		t.Errorf("muted after mute = %v; want [storage]", m)
	}
	if hasID(h.bellList(), id) {
		t.Error("muted-category notification must not appear in the list")
	}
	if c := h.bellCount(); c != 0 {
		t.Errorf("unread after mute = %d; want 0", c)
	}

	resp = h.do("DELETE", "/api/v1/notifications/mutes/storage", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("unmute = %d; want 204", resp.StatusCode)
	}
	if !hasID(h.bellList(), id) {
		t.Error("unmuted-category notification should reappear in the list")
	}
	if c := h.bellCount(); c != 1 {
		t.Errorf("unread after unmute = %d; want 1", c)
	}
}

// An unknown category is rejected (422) on both mute and unmute, so a typo never
// persists a mute that silently matches nothing.
func TestNotifications_MuteUnknownCategory(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	for _, method := range []string{"PUT", "DELETE"} {
		resp := h.do(method, "/api/v1/notifications/mutes/bogus", nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("%s unknown category = %d; want 422", method, resp.StatusCode)
		}
	}
}

// A mute is the caller's own preference — it does not leak into another user's
// mute list.
func TestNotifications_MutePerUser(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.addMember("u_bob", "bob", "bobpass")

	resp := h.do("PUT", "/api/v1/notifications/mutes/storage", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("alice mute = %d; want 204", resp.StatusCode)
	}
	if m := h.mutedCategories(); len(m) != 1 || m[0] != "storage" {
		t.Errorf("alice muted = %v; want [storage]", m)
	}

	h.loginAs("bob", "bobpass")
	if m := h.mutedCategories(); len(m) != 0 {
		t.Errorf("bob muted = %v; want empty (alice's mute is hers alone)", m)
	}
}

// A mute hides a category from the aggregate surfaces but does NOT gate acting on
// a specific notification by id — a user can still mark-read/dismiss a row in a
// muted category (e.g. one they saw before muting). Pins the deliberate
// mute-agnostic per-id path (GetNotification ignores mute) against an
// over-zealous "filter everywhere" refactor: a regression would surface here as
// a 404.
func TestNotifications_MutedCategoryStillActionableByID(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	id := h.seedNotification("box:1", notify.AudienceAdmins, "") // storage category

	resp := h.do("PUT", "/api/v1/notifications/mutes/storage", nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("mute = %d; want 204", resp.StatusCode)
	}
	if hasID(h.bellList(), id) {
		t.Fatal("muted-category row should be hidden from the list")
	}

	// Still markable read by id — a 404 would mean the mute wrongly gated it.
	resp = h.do("POST", fmt.Sprintf("/api/v1/notifications/%d/read", id), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("mark-read on muted-category row = %d; want 204 (per-id path is mute-agnostic)", resp.StatusCode)
	}
	// And dismissable by id.
	resp = h.do("POST", fmt.Sprintf("/api/v1/notifications/%d/dismiss", id), nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("dismiss on muted-category row = %d; want 204", resp.StatusCode)
	}
}

// The limit query param is honored at the wire — pagination logic itself is
// covered in internal/store; here we only pin the binding.
func TestNotifications_ListRespectsLimit(t *testing.T) {
	h := newHarness(t)
	h.setupAdmin("alice", "pass1")
	h.seedNotification("box:1", notify.AudienceAdmins, "")
	h.seedNotification("box:2", notify.AudienceAdmins, "")
	h.seedNotification("box:3", notify.AudienceAdmins, "")

	resp := h.do("GET", "/api/v1/notifications?limit=2", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("list with limit = %d; want 200", resp.StatusCode)
	}
	body := decodeJSON[struct {
		Notifications []NotificationDTO `json:"notifications"`
	}](t, resp)
	if len(body.Notifications) != 2 {
		t.Errorf("limit=2 returned %d rows; want 2", len(body.Notifications))
	}
}
