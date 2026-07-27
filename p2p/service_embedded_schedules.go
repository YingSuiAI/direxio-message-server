package p2p

import (
	"context"
	"strings"
)

// EmbeddedSchedulesReady is fail-closed and intentionally distinct from the
// Agent Core capability. It is true only when durable storage, pinned profile
// resolution and the restricted runner are all wired.
func (s *Service) EmbeddedSchedulesReady() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.scheduleModule != nil && s.scheduleModule.Ready() && s.scheduleRunning
}

// StartEmbeddedScheduler starts the bounded background worker. Startup code
// may call this after its lifecycle context is available; dependency failure
// simply leaves the capability unavailable and does not affect other actions.
func (s *Service) StartEmbeddedScheduler(ctx context.Context, workerID string) bool {
	if s == nil || s.scheduleModule == nil || !s.scheduleModule.Ready() {
		return false
	}
	s.mu.Lock()
	s.scheduleRunning = true
	s.mu.Unlock()
	owner := strings.TrimSpace(s.OwnerMXID())
	go func() {
		defer func() { s.mu.Lock(); s.scheduleRunning = false; s.mu.Unlock() }()
		s.scheduleModule.Worker(owner, workerID).Run(ctx)
	}()
	return true
}
