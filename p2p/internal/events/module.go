// Package events owns the durable ProductCore event stream, sequence
// allocation, retention, cursor validation, and live waiter notification.
package events

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
)

// Store is the durable event repository required by Module.
type Store interface {
	InsertEvent(context.Context, dirextalkdomain.Event) (bool, error)
	ListEvents(context.Context, int64, int) ([]dirextalkdomain.Event, error)
	EventBounds(context.Context) (dirextalkdomain.EventBounds, error)
	PruneEventsToMaxRows(context.Context, int64) (int64, error)
}

type Config struct {
	RetentionMaxRows      int64
	RetentionPruneOnWrite bool
	Now                   func() time.Time
}

type Module struct {
	mu        sync.Mutex
	store     Store
	config    Config
	nextSeq   int64
	eventWake chan struct{}
}

func New(store Store, cfg Config) *Module {
	return &Module{store: store, config: cfg, eventWake: make(chan struct{})}
}

// Append allocates a monotonic process-local sequence, persists the event,
// applies configured retention, and wakes live consumers only for inserts.
func (m *Module) Append(ctx context.Context, event dirextalkdomain.Event) error {
	if m == nil || m.store == nil {
		return errors.New("event store is unavailable")
	}
	now := m.now()
	if event.CreatedAt == "" {
		event.CreatedAt = now.Format(time.RFC3339Nano)
	}
	event.DedupeKey = strings.TrimSpace(event.DedupeKey)

	m.mu.Lock()
	if event.Seq <= 0 || event.Seq <= m.nextSeq {
		event.Seq = now.UnixNano()
		if event.Seq <= m.nextSeq {
			event.Seq = m.nextSeq + 1
		}
	}
	m.nextSeq = event.Seq
	m.mu.Unlock()

	inserted, err := m.store.InsertEvent(ctx, event)
	if err != nil {
		return err
	}
	if !inserted {
		return nil
	}
	if m.config.RetentionPruneOnWrite && m.config.RetentionMaxRows > 0 {
		if _, err := m.store.PruneEventsToMaxRows(ctx, m.config.RetentionMaxRows); err != nil {
			return err
		}
	}
	m.notify()
	return nil
}

func (m *Module) List(ctx context.Context, since int64, limit int) ([]dirextalkdomain.Event, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("event store is unavailable")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	return m.store.ListEvents(ctx, since, limit)
}

type CursorStatus struct {
	Expired bool
	Since   int64
	Bounds  dirextalkdomain.EventBounds
}

func (m *Module) CursorStatus(ctx context.Context, since int64) (CursorStatus, error) {
	status := CursorStatus{Since: since}
	if since <= 0 {
		return status, nil
	}
	if m == nil || m.store == nil {
		return CursorStatus{}, errors.New("event store is unavailable")
	}
	bounds, err := m.store.EventBounds(ctx)
	if err != nil {
		return CursorStatus{}, err
	}
	status.Bounds = bounds
	status.Expired = bounds.Count > 0 && bounds.MinSeq > 0 && since < bounds.MinSeq
	return status, nil
}

func (m *Module) Waiter() <-chan struct{} {
	if m == nil {
		wait := make(chan struct{})
		return wait
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.eventWake == nil {
		m.eventWake = make(chan struct{})
	}
	return m.eventWake
}

// ResetSequence is used after account deprovision has drained writers and
// cleared the durable event table.
func (m *Module) ResetSequence() {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.nextSeq = 0
	m.mu.Unlock()
}

// NotifyPersisted advances the local sequence fence and wakes realtime
// consumers after another domain transaction atomically inserted a p2p_event.
// It must be called only after that transaction commits successfully.
func (m *Module) NotifyPersisted(sequence int64) {
	if m == nil || sequence <= 0 {
		return
	}
	m.mu.Lock()
	if sequence > m.nextSeq {
		m.nextSeq = sequence
	}
	if m.eventWake == nil {
		m.eventWake = make(chan struct{})
	}
	close(m.eventWake)
	m.eventWake = make(chan struct{})
	m.mu.Unlock()
}

func (m *Module) notify() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.eventWake == nil {
		m.eventWake = make(chan struct{})
	}
	close(m.eventWake)
	m.eventWake = make(chan struct{})
}

func (m *Module) now() time.Time {
	if m != nil && m.config.Now != nil {
		return m.config.Now().UTC()
	}
	return time.Now().UTC()
}
