package store

import (
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/onmoose/moose/internal/notify"
)

const (
	// notifyMaxAgeDays mirrors the 90-day session hard-expiry (store.go) — one
	// retention number across the box.
	notifyMaxAgeDays = 90
	// notifyMaxRows is a high-water runaway ceiling, not a hard quota: the cap
	// pass only fires when the table is over it, and trims the oldest excess.
	notifyMaxRows = 1000
)

// PruneNotifications bounds the (mutable, prunable) notifications table
// (NOTIFICATIONS.md # Locked decisions, the retention bullet). Two passes,
// age-primary: (1) delete rows
// older than notifyMaxAgeDays — state-blind, since ts is NOT NULL and
// monotonic this is a total order; (2) if still over notifyMaxRows, drop the
// oldest excess, resolved rows before active ones (a cleared issue is history;
// a live one is not). notification_reads children go via ON DELETE CASCADE
// (store.go; foreign_keys=ON is live per-connection). notification_mutes is
// keyed by category, not notification, so it's untouched. Silent on the bus —
// aged/resolved/dismissed rows are already invisible and the notifier holds no
// in-memory state, so there's nothing to re-render. No transaction: writers are
// serialized (SetMaxOpenConns(1)) and each DELETE is independently correct and
// idempotent; the cap is soft, so a benign ±1 skew from a concurrent raise
// self-corrects on the next run.
func (s *Store) PruneNotifications(now time.Time) error {
	cutoff := now.Add(-notifyMaxAgeDays * 24 * time.Hour).UnixMilli()
	ageRes, err := s.db.Exec(`DELETE FROM notifications WHERE ts < ?`, cutoff)
	if err != nil {
		return err
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM notifications`).Scan(&count); err != nil {
		return err
	}
	var capped int64
	if excess := count - notifyMaxRows; excess > 0 {
		capRes, err := s.db.Exec(
			`DELETE FROM notifications WHERE id IN (
			   SELECT id FROM notifications
			   ORDER BY (resolved_at IS NOT NULL) DESC, ts ASC
			   LIMIT ?)`, excess)
		if err != nil {
			return err
		}
		capped, _ = capRes.RowsAffected()
	}

	aged, _ := ageRes.RowsAffected()
	if aged > 0 || capped > 0 {
		slog.Info("notifications pruned", "aged", aged, "capped", capped)
	}
	return nil
}

// RaiseNotification coalesces by dedup_key (NOTIFICATIONS.md # One notification
// per raise): if an active (non-dismissed) row for the same key exists it is
// refreshed in place — ts/severity/summary/body/action bumped and resolved_at
// cleared — so a flapping issue never produces a second row. Otherwise a new
// row is inserted. Writers are serialized (SetMaxOpenConns(1)), so the
// update-then-insert is race-free; the partial unique index on
// (dedup_key) WHERE dismissed_at IS NULL is the backstop.
func (s *Store) RaiseNotification(n notify.Notification) error {
	res, err := s.db.Exec(
		`UPDATE notifications
		   SET ts=?, severity=?, summary=?, body=?, action_label=?, action_route=?,
		       resolved_at=NULL
		 WHERE dedup_key=? AND dismissed_at IS NULL`,
		n.TS, string(n.Severity), n.Summary, n.Body, n.ActionLabel, n.ActionRoute,
		n.DedupKey)
	if err != nil {
		return err
	}
	if rows, _ := res.RowsAffected(); rows > 0 {
		// A coalesced raise is a fresh occurrence (the notifier only re-raises a
		// key on a genuine issue transition — a problem flapping back, or a
		// repeated clear re-emitting its "all clear"), so clear any per-recipient
		// read/dismiss state — the notification re-surfaces unread on every bell
		// (NOTIFICATIONS.md # One notification per raise: "while unread"). The
		// active row for this key is unique (notifications_active_dedup).
		if _, err := s.db.Exec(
			`DELETE FROM notification_reads
			 WHERE notification_id IN
			   (SELECT id FROM notifications WHERE dedup_key=? AND dismissed_at IS NULL)`,
			n.DedupKey); err != nil {
			return err
		}
		return nil
	}

	var userID any
	if n.UserID != "" {
		userID = n.UserID
	}
	_, err = s.db.Exec(
		`INSERT INTO notifications
		  (ts, category, severity, source_kind, source_id, dedup_key,
		   audience, user_id, variant, summary, body, action_label, action_route)
		 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		n.TS, string(n.Category), string(n.Severity), n.SourceKind, n.SourceID, n.DedupKey,
		n.Audience, userID, n.Variant, n.Summary, n.Body, n.ActionLabel, n.ActionRoute)
	return err
}

// ResolveNotification marks the active notification for dedupKey resolved
// (NOTIFICATIONS.md # Clears) — the original notification stays on the timeline,
// marked resolved rather than deleted. No-op when no active row matches, so a
// clear of an issue that never notified is harmless.
func (s *Store) ResolveNotification(dedupKey string, at time.Time) error {
	_, err := s.db.Exec(
		`UPDATE notifications SET resolved_at=?
		 WHERE dedup_key=? AND dismissed_at IS NULL`,
		at.UnixMilli(), dedupKey)
	return err
}

// ListNotifications returns notifications newest-first. Skeleton scope for the
// write seam: no filtering yet — the audience/read-state-aware list the bell
// API needs lands with that slice. Used by tests today.
func (s *Store) ListNotifications() ([]notify.Notification, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, category, severity, source_kind, source_id, dedup_key,
		        audience, user_id, variant, summary, body, action_label, action_route,
		        resolved_at
		 FROM notifications ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notify.Notification
	for rows.Next() {
		var n notify.Notification
		var category, severity string
		var userID sql.NullString
		var resolvedAt sql.NullInt64
		if err := rows.Scan(
			&n.ID, &n.TS, &category, &severity, &n.SourceKind, &n.SourceID, &n.DedupKey,
			&n.Audience, &userID, &n.Variant, &n.Summary, &n.Body, &n.ActionLabel, &n.ActionRoute,
			&resolvedAt,
		); err != nil {
			return nil, err
		}
		n.Category = notify.Category(category)
		n.Severity = notify.Severity(severity)
		n.UserID = userID.String
		n.ResolvedAt = resolvedAt.Int64
		out = append(out, n)
	}
	return out, rows.Err()
}

// NotificationFilter scopes ListNotificationsForRecipient to one caller.
type NotificationFilter struct {
	UserID  string // the caller
	IsAdmin bool   // admins additionally see box-wide ('admins') notifications
	AfterID int64  // cursor: rows with id < AfterID (0 = from newest)
	Limit   int
	// IncludeDismissed also returns rows this caller dismissed (default off,
	// matching the bell's "active inbox" view).
	IncludeDismissed bool
}

// notificationVisibilityClause is the SQL predicate (over alias n) that scopes
// notifications to a recipient (NOTIFICATIONS.md # Routing): an admin sees
// box-wide ('admins') rows plus their own ('user') rows; a member sees the
// member-broadcast transparency rows ('members') plus their own. The two class
// audiences are disjoint — admins never see 'members' rows (they get the
// actionable copy) and members never see 'admins' rows. Either branch binds
// exactly one parameter — the caller's user id.
func notificationVisibilityClause(isAdmin bool) string {
	if isAdmin {
		return "(n.audience = 'admins' OR (n.audience = 'user' AND n.user_id = ?))"
	}
	return "(n.audience = 'members' OR (n.audience = 'user' AND n.user_id = ?))"
}

// notificationMuteClause drops rows whose category the caller has muted
// (NOTIFICATIONS.md # Configuration). It is a read-time filter on the aggregate
// surfaces (list, unread count, mark-all) — never emit-time suppression, since a
// class notification is one row for many recipients. Binds exactly one parameter
// (the caller's user id); the per-id read/dismiss path (GetNotification) does NOT
// apply it, so a user can still act on a specific row in a muted category.
func notificationMuteClause() string {
	return " AND n.category NOT IN (SELECT category FROM notification_mutes WHERE user_id = ?)"
}

// MuteNotificationCategory mutes category for userID (NOTIFICATIONS.md
// # Configuration). Idempotent — a repeat mute is a no-op. The category is
// validated by the API layer (notify.ValidCategory) before this is called.
func (s *Store) MuteNotificationCategory(userID, category string) error {
	_, err := s.db.Exec(
		`INSERT INTO notification_mutes (user_id, category)
		 VALUES (?,?)
		 ON CONFLICT (user_id, category) DO NOTHING`,
		userID, category)
	return err
}

// UnmuteNotificationCategory removes a mute for userID (NOTIFICATIONS.md
// # Configuration). No-op when the category was not muted, so unmute is
// idempotent and safe to call blindly.
func (s *Store) UnmuteNotificationCategory(userID, category string) error {
	_, err := s.db.Exec(
		`DELETE FROM notification_mutes WHERE user_id = ? AND category = ?`,
		userID, category)
	return err
}

// ListMutedCategories returns the categories userID has muted, sorted — the
// settings surface reads this to render the mute toggles.
func (s *Store) ListMutedCategories(userID string) ([]string, error) {
	rows, err := s.db.Query(
		`SELECT category FROM notification_mutes WHERE user_id = ? ORDER BY category`,
		userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListNotificationsForRecipient returns the caller's notifications newest-first,
// audience-scoped with this caller's per-recipient read/dismiss state joined in
// from notification_reads. Dismissed rows are excluded unless IncludeDismissed.
// This is the audience/read-state-aware list the bell API uses; the unfiltered
// ListNotifications stays for the write-seam tests.
func (s *Store) ListNotificationsForRecipient(f NotificationFilter) ([]notify.Notification, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	// Args in statement order: JOIN user_id, visibility user_id, mute user_id,
	// [AfterID], limit.
	args := []any{f.UserID, f.UserID, f.UserID}
	where := notificationVisibilityClause(f.IsAdmin) + notificationMuteClause()
	if !f.IncludeDismissed {
		where += " AND nr.dismissed_at IS NULL"
	}
	if f.AfterID > 0 {
		where += " AND n.id < ?"
		args = append(args, f.AfterID)
	}
	args = append(args, limit)

	rows, err := s.db.Query(
		`SELECT n.id, n.ts, n.category, n.severity, n.source_kind, n.source_id, n.dedup_key,
		        n.audience, n.user_id, n.variant, n.summary, n.body, n.action_label, n.action_route,
		        n.resolved_at, nr.read_at, nr.dismissed_at
		 FROM notifications n
		 LEFT JOIN notification_reads nr
		   ON nr.notification_id = n.id AND nr.user_id = ?
		 WHERE `+where+`
		 ORDER BY n.id DESC LIMIT ?`,
		args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []notify.Notification
	for rows.Next() {
		n, err := scanNotification(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// scanNotification scans the recipient-list row shape (notification columns plus
// this caller's joined read_at/dismissed_at).
func scanNotification(row scanner) (notify.Notification, error) {
	var n notify.Notification
	var category, severity string
	var userID sql.NullString
	var resolvedAt, readAt, dismissedAt sql.NullInt64
	if err := row.Scan(
		&n.ID, &n.TS, &category, &severity, &n.SourceKind, &n.SourceID, &n.DedupKey,
		&n.Audience, &userID, &n.Variant, &n.Summary, &n.Body, &n.ActionLabel, &n.ActionRoute,
		&resolvedAt, &readAt, &dismissedAt,
	); err != nil {
		return notify.Notification{}, err
	}
	n.Category = notify.Category(category)
	n.Severity = notify.Severity(severity)
	n.UserID = userID.String
	n.ResolvedAt = resolvedAt.Int64
	n.ReadAt = readAt.Int64
	n.DismissedAt = dismissedAt.Int64
	return n, nil
}

// CountUnreadNotifications returns how many notifications visible to the caller
// are unread and not dismissed — the bell badge (NOTIFICATIONS.md # Read /
// unread / dismiss). A resolved-but-unread notification still counts: the user
// hasn't seen it yet.
func (s *Store) CountUnreadNotifications(userID string, isAdmin bool) (int, error) {
	var c int
	err := s.db.QueryRow(
		`SELECT COUNT(*)
		 FROM notifications n
		 LEFT JOIN notification_reads nr
		   ON nr.notification_id = n.id AND nr.user_id = ?
		 WHERE `+notificationVisibilityClause(isAdmin)+notificationMuteClause()+`
		   AND nr.read_at IS NULL AND nr.dismissed_at IS NULL`,
		userID, userID, userID).Scan(&c)
	return c, err
}

// GetNotification returns one notification by id, or ErrNotFound. The bell's
// mutating handlers use it to confirm a notification is visible to the caller
// before recording read/dismiss state.
func (s *Store) GetNotification(id int64) (notify.Notification, error) {
	var n notify.Notification
	var category, severity string
	var userID sql.NullString
	var resolvedAt sql.NullInt64
	err := s.db.QueryRow(
		`SELECT id, ts, category, severity, source_kind, source_id, dedup_key,
		        audience, user_id, variant, summary, body, action_label, action_route, resolved_at
		 FROM notifications WHERE id = ?`, id).Scan(
		&n.ID, &n.TS, &category, &severity, &n.SourceKind, &n.SourceID, &n.DedupKey,
		&n.Audience, &userID, &n.Variant, &n.Summary, &n.Body, &n.ActionLabel, &n.ActionRoute, &resolvedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return notify.Notification{}, ErrNotFound
	}
	if err != nil {
		return notify.Notification{}, err
	}
	n.Category = notify.Category(category)
	n.Severity = notify.Severity(severity)
	n.UserID = userID.String
	n.ResolvedAt = resolvedAt.Int64
	return n, nil
}

// MarkNotificationRead records that userID has read notification id, preserving
// the first-read timestamp on repeat calls (idempotent). Visibility is the
// caller's responsibility (the handler checks it via GetNotification).
func (s *Store) MarkNotificationRead(id int64, userID string, at time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO notification_reads (notification_id, user_id, read_at)
		 VALUES (?,?,?)
		 ON CONFLICT (notification_id, user_id)
		 DO UPDATE SET read_at = COALESCE(notification_reads.read_at, excluded.read_at)`,
		id, userID, at.UnixMilli())
	return err
}

// DismissNotification records that userID has dismissed notification id —
// removing it from their active inbox without resolving the underlying
// condition (NOTIFICATIONS.md # Read / unread / dismiss: dismiss ≠ resolve).
// Per-recipient: one admin dismissing a box-wide notice doesn't dismiss it for
// other admins. Idempotent; preserves the first-dismiss timestamp.
func (s *Store) DismissNotification(id int64, userID string, at time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO notification_reads (notification_id, user_id, dismissed_at)
		 VALUES (?,?,?)
		 ON CONFLICT (notification_id, user_id)
		 DO UPDATE SET dismissed_at = COALESCE(notification_reads.dismissed_at, excluded.dismissed_at)`,
		id, userID, at.UnixMilli())
	return err
}

// MarkAllNotificationsRead marks every notification currently visible to the
// caller (and not already read by them) read in one statement — the bell's
// "mark all read" / read-on-open action. It applies the same mute filter as
// list/count, so a muted category is left untouched: unmuting later reveals its
// notifications in their true unread state rather than as already-read rows the
// user never saw.
func (s *Store) MarkAllNotificationsRead(userID string, isAdmin bool, at time.Time) error {
	_, err := s.db.Exec(
		`INSERT INTO notification_reads (notification_id, user_id, read_at)
		 SELECT n.id, ?, ?
		 FROM notifications n
		 LEFT JOIN notification_reads nr
		   ON nr.notification_id = n.id AND nr.user_id = ?
		 WHERE `+notificationVisibilityClause(isAdmin)+notificationMuteClause()+`
		   AND nr.read_at IS NULL
		 ON CONFLICT (notification_id, user_id)
		 DO UPDATE SET read_at = COALESCE(notification_reads.read_at, excluded.read_at)`,
		userID, at.UnixMilli(), userID, userID, userID)
	return err
}
