package api

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"

	"github.com/onmoose/moose/internal/auth"
	"github.com/onmoose/moose/internal/events"
	"github.com/onmoose/moose/internal/notify"
	"github.com/onmoose/moose/internal/store"
)

// maxNotificationLimit caps the bell list page size (mirrors maxAuditLimit).
const maxNotificationLimit = 100

// registerNotifications wires the dashboard bell read surface
// (NOTIFICATIONS.md # Surfaces, # Knock-ons): the audience-scoped inbox plus
// per-recipient read/dismiss. Not admin-gated like /health — every
// authenticated user sees the notifications addressed to them (admins also see
// box-wide ones), mirroring the listAudit member-vs-admin pattern.
func (s *Server) registerNotifications(api huma.API) {
	huma.Register(api, huma.Operation{
		OperationID: "list-notifications", Method: "GET", Path: "/api/v1/notifications",
		Summary: "List the caller's notifications (newest first)",
	}, s.listNotifications)

	huma.Register(api, huma.Operation{
		OperationID: "unread-notification-count", Method: "GET", Path: "/api/v1/notifications/unread-count",
		Summary: "Count the caller's unread notifications (bell badge)",
	}, s.unreadNotificationCount)

	huma.Register(api, huma.Operation{
		OperationID: "mark-notification-read", Method: "POST", Path: "/api/v1/notifications/{id}/read",
		Summary: "Mark one notification read", DefaultStatus: http.StatusNoContent,
	}, s.markNotificationRead)

	huma.Register(api, huma.Operation{
		OperationID: "mark-all-notifications-read", Method: "POST", Path: "/api/v1/notifications/read-all",
		Summary: "Mark all the caller's notifications read", DefaultStatus: http.StatusNoContent,
	}, s.markAllNotificationsRead)

	huma.Register(api, huma.Operation{
		OperationID: "dismiss-notification", Method: "POST", Path: "/api/v1/notifications/{id}/dismiss",
		Summary: "Dismiss one notification (out of the active inbox)", DefaultStatus: http.StatusNoContent,
	}, s.dismissNotification)

	huma.Register(api, huma.Operation{
		OperationID: "list-notification-mutes", Method: "GET", Path: "/api/v1/notifications/mutes",
		Summary: "List the caller's muted notification categories",
	}, s.listNotificationMutes)

	huma.Register(api, huma.Operation{
		OperationID: "mute-notification-category", Method: "PUT", Path: "/api/v1/notifications/mutes/{category}",
		Summary: "Mute a notification category for the caller", DefaultStatus: http.StatusNoContent,
	}, s.muteNotificationCategory)

	huma.Register(api, huma.Operation{
		OperationID: "unmute-notification-category", Method: "DELETE", Path: "/api/v1/notifications/mutes/{category}",
		Summary: "Unmute a notification category for the caller", DefaultStatus: http.StatusNoContent,
	}, s.unmuteNotificationCategory)
}

// NotificationDTO is the wire shape of one notification row, with this caller's
// read state folded into a boolean. Routing fields (audience, variant, user_id)
// and source identifiers stay server-side — the client only needs what it
// renders. dismissed rows are excluded from the list, so there's no dismissed
// flag here.
type NotificationDTO struct {
	ID          int64  `json:"id"`
	TS          int64  `json:"ts"`
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	Summary     string `json:"summary"`
	Body        string `json:"body,omitempty"`
	ActionLabel string `json:"action_label,omitempty"`
	ActionRoute string `json:"action_route,omitempty"`
	Read        bool   `json:"read"`
	ResolvedAt  int64  `json:"resolved_at,omitempty"`
}

func notificationDTO(n notify.Notification) NotificationDTO {
	return NotificationDTO{
		ID: n.ID, TS: n.TS,
		Category: string(n.Category), Severity: string(n.Severity),
		Summary: n.Summary, Body: n.Body,
		ActionLabel: n.ActionLabel, ActionRoute: n.ActionRoute,
		Read:       n.ReadAt != 0,
		ResolvedAt: n.ResolvedAt,
	}
}

func (s *Server) listNotifications(ctx context.Context, in *struct {
	Limit   int   `query:"limit" default:"50"`
	AfterID int64 `query:"after_id"`
}) (*struct {
	Body struct {
		Notifications []NotificationDTO `json:"notifications"`
	}
}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}

	limit := in.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > maxNotificationLimit {
		limit = maxNotificationLimit
	}

	rows, err := s.store.ListNotificationsForRecipient(store.NotificationFilter{
		UserID:  id.User.ID,
		IsAdmin: id.IsAdmin(),
		AfterID: in.AfterID,
		Limit:   limit,
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("notifications query failed", err)
	}

	out := &struct {
		Body struct {
			Notifications []NotificationDTO `json:"notifications"`
		}
	}{}
	out.Body.Notifications = []NotificationDTO{}
	for _, n := range rows {
		out.Body.Notifications = append(out.Body.Notifications, notificationDTO(n))
	}
	return out, nil
}

func (s *Server) unreadNotificationCount(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		Count int `json:"count"`
	}
}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	c, err := s.store.CountUnreadNotifications(id.User.ID, id.IsAdmin())
	if err != nil {
		return nil, huma.Error500InternalServerError("unread count failed", err)
	}
	out := &struct {
		Body struct {
			Count int `json:"count"`
		}
	}{}
	out.Body.Count = c
	return out, nil
}

func (s *Server) markNotificationRead(ctx context.Context, in *struct {
	ID int64 `path:"id"`
}) (*struct{}, error) {
	id, err := s.notificationRecipient(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if err := s.store.MarkNotificationRead(in.ID, id.User.ID, s.auth.Clock()); err != nil {
		return nil, huma.Error500InternalServerError("mark read failed", err)
	}
	s.bus.Publish(events.NotificationUpdated, map[string]any{"id": in.ID})
	return nil, nil
}

func (s *Server) dismissNotification(ctx context.Context, in *struct {
	ID int64 `path:"id"`
}) (*struct{}, error) {
	id, err := s.notificationRecipient(ctx, in.ID)
	if err != nil {
		return nil, err
	}
	if err := s.store.DismissNotification(in.ID, id.User.ID, s.auth.Clock()); err != nil {
		return nil, huma.Error500InternalServerError("dismiss failed", err)
	}
	s.bus.Publish(events.NotificationUpdated, map[string]any{"id": in.ID})
	return nil, nil
}

func (s *Server) markAllNotificationsRead(ctx context.Context, _ *struct{}) (*struct{}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	if err := s.store.MarkAllNotificationsRead(id.User.ID, id.IsAdmin(), s.auth.Clock()); err != nil {
		return nil, huma.Error500InternalServerError("mark all read failed", err)
	}
	s.bus.Publish(events.NotificationUpdated, map[string]any{})
	return nil, nil
}

// listNotificationMutes returns the caller's muted categories (NOTIFICATIONS.md
// # Configuration). Per-user: a mute is the caller's own preference, scoped to
// their session, never another user's.
func (s *Server) listNotificationMutes(ctx context.Context, _ *struct{}) (*struct {
	Body struct {
		Muted []string `json:"muted"`
	}
}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	muted, err := s.store.ListMutedCategories(id.User.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("mutes query failed", err)
	}
	out := &struct {
		Body struct {
			Muted []string `json:"muted"`
		}
	}{}
	out.Body.Muted = []string{}
	out.Body.Muted = append(out.Body.Muted, muted...)
	return out, nil
}

func (s *Server) muteNotificationCategory(ctx context.Context, in *struct {
	Category string `path:"category"`
}) (*struct{}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	if !notify.ValidCategory(in.Category) {
		return nil, huma.Error422UnprocessableEntity("unknown notification category")
	}
	if err := s.store.MuteNotificationCategory(id.User.ID, in.Category); err != nil {
		return nil, huma.Error500InternalServerError("mute failed", err)
	}
	// A mute changes what the caller's bell counts/shows; nudge their other tabs
	// to refetch (mirrors the read-state handlers' SSE publish).
	s.bus.Publish(events.NotificationUpdated, map[string]any{})
	return nil, nil
}

func (s *Server) unmuteNotificationCategory(ctx context.Context, in *struct {
	Category string `path:"category"`
}) (*struct{}, error) {
	id, ok := auth.FromContext(ctx)
	if !ok {
		return nil, huma.Error401Unauthorized("unauthenticated")
	}
	if !notify.ValidCategory(in.Category) {
		return nil, huma.Error422UnprocessableEntity("unknown notification category")
	}
	if err := s.store.UnmuteNotificationCategory(id.User.ID, in.Category); err != nil {
		return nil, huma.Error500InternalServerError("unmute failed", err)
	}
	s.bus.Publish(events.NotificationUpdated, map[string]any{})
	return nil, nil
}

// notificationRecipient resolves the caller and confirms notification id is
// addressed to them, returning 404 otherwise — 404 (not 403) so a member can't
// probe which notification ids exist. The shared guard for the per-id mutating
// handlers (mark-read, dismiss).
func (s *Server) notificationRecipient(ctx context.Context, id int64) (auth.Identity, error) {
	who, ok := auth.FromContext(ctx)
	if !ok {
		return auth.Identity{}, huma.Error401Unauthorized("unauthenticated")
	}
	n, err := s.store.GetNotification(id)
	if errors.Is(err, store.ErrNotFound) {
		return auth.Identity{}, huma.Error404NotFound("no such notification")
	}
	if err != nil {
		return auth.Identity{}, huma.Error500InternalServerError("notification lookup failed", err)
	}
	visible := (n.Audience == notify.AudienceAdmins && who.IsAdmin()) ||
		(n.Audience == notify.AudienceMembers && !who.IsAdmin()) ||
		(n.Audience == notify.AudienceUser && n.UserID == who.User.ID)
	if !visible {
		return auth.Identity{}, huma.Error404NotFound("no such notification")
	}
	return who, nil
}
