package storage

import "context"

func (s *MemoryStore) LoadAgentEventCursor(
	ctx context.Context,
	source string,
) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.agentEventCursors[source], nil
}

func (s *MemoryStore) SaveAgentEventCursor(
	ctx context.Context,
	source string,
	afterSeq int64,
) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if afterSeq > s.agentEventCursors[source] {
		s.agentEventCursors[source] = afterSeq
	}
	return nil
}
