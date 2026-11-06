// Package events is the brain's global SSE event stream (BRAIN_UI_PROTOCOL.md
// Pattern C, stream 2). Typed kinds, monotonic ids, fan-out to subscribers.
// Skeleton scope: in-memory fan-out, no replay buffer yet (Last-Event-ID
// replay is a follow-up).
package events

import (
	"sync"
	"sync/atomic"
)

// Kind is a first-class enum (BRAIN_UI_PROTOCOL.md: "Event kind values are
// enumerated in the schema").
type Kind string

const (
	AppStateChanged Kind = "app.state_changed"
	AppInstalled    Kind = "app.installed"
	AppUninstalled  Kind = "app.uninstalled"
	// AppUpdateChanged says what the box knows about an app's update changed:
	// a check found a new version or cleared one, or an update failed and was
	// recorded (UPDATES.md # 6, #631). Like the others, the payload
	// ({instance_id}) is an advisory refetch trigger.
	AppUpdateChanged Kind = "app.update_changed"
	// Notification lifecycle (NOTIFICATIONS.md # Surfaces). created = a new
	// notification appeared; updated covers read / resolve / dismiss. Payloads
	// are advisory refetch triggers — the client re-reads its audience-scoped
	// list rather than merging the event data.
	NotificationCreated Kind = "notification.created"
	NotificationUpdated Kind = "notification.updated"
	// Health-issue transitions (HEALTH.md # Display, BRAIN_UI_PROTOCOL.md Pattern
	// C). raised = a detector raised an issue; cleared = the condition resolved.
	// Like notifications, the payload is an advisory refetch trigger ({id,
	// instance_key}) — the dashboard re-reads GET /api/v1/health rather than
	// merging the event data. Note the underscore form is the SSE event name,
	// distinct from the dotted audit action (health.issue.raised, LOGGING.md).
	HealthIssueRaised  Kind = "health.issue_raised"
	HealthIssueCleared Kind = "health.issue_cleared"
)

type Event struct {
	ID   uint64         `json:"-"`
	Kind Kind           `json:"-"`
	Data map[string]any `json:"data"`
}

type Bus struct {
	mu     sync.Mutex
	nextID uint64
	subs   map[int]chan Event
	seq    int
}

func NewBus() *Bus { return &Bus{subs: map[int]chan Event{}} }

// Subscribe returns a channel of events and an unsubscribe func.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 32)
	b.mu.Lock()
	id := b.seq
	b.seq++
	b.subs[id] = ch
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
		b.mu.Unlock()
	}
}

func (b *Bus) Publish(kind Kind, data map[string]any) {
	ev := Event{ID: atomic.AddUint64(&b.nextID, 1), Kind: kind, Data: data}
	b.mu.Lock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default: // slow consumer: drop rather than block the publisher
		}
	}
	b.mu.Unlock()
}
