package storage

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
)

func (s *MemoryStore) ReserveAgentTurn(_ context.Context, candidate agentturns.Candidate) (agentturns.Reservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := memoryAgentTurnKey(candidate.OwnerID, candidate.TurnID)
	if existing, ok := s.agentTurns[key]; ok {
		return agentturns.Reservation{Turn: existing}, nil
	}
	now := time.Now().UTC()
	turn := agentturns.Turn{
		OwnerID: candidate.OwnerID, TurnID: candidate.TurnID, ConversationID: candidate.ConversationID,
		Action: candidate.Action, Digest: candidate.Digest, State: agentturns.StateAccepted,
		CreatedAt: now, UpdatedAt: now,
	}
	s.agentTurns[key] = turn
	return agentturns.Reservation{Turn: turn, Created: true}, nil
}

func (s *MemoryStore) GetAgentTurn(_ context.Context, ownerID, turnID string) (agentturns.Turn, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	turn, ok := s.agentTurns[memoryAgentTurnKey(ownerID, turnID)]
	return turn, ok, nil
}

func (s *MemoryStore) ListAgentTurns(_ context.Context, ownerID, conversationID string, limit int) ([]agentturns.Turn, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	turns := make([]agentturns.Turn, 0, limit)
	for _, turn := range s.agentTurns {
		if turn.OwnerID != ownerID || conversationID != "" && turn.ConversationID != conversationID {
			continue
		}
		turns = append(turns, turn)
	}
	sort.Slice(turns, func(i, j int) bool {
		if turns[i].CreatedAt.Equal(turns[j].CreatedAt) {
			return turns[i].TurnID < turns[j].TurnID
		}
		return turns[i].CreatedAt.After(turns[j].CreatedAt)
	})
	if len(turns) > limit {
		turns = turns[:limit]
	}
	return turns, nil
}

func (s *MemoryStore) ListAgentTurnEvents(_ context.Context, ownerID, turnID string, afterSeq int64) ([]agentturns.Event, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	stored := s.agentTurnEvents[memoryAgentTurnKey(ownerID, turnID)]
	events := make([]agentturns.Event, 0, len(stored))
	for _, event := range stored {
		if event.Seq > afterSeq {
			event.Data = cloneAnyMap(event.Data)
			events = append(events, event)
		}
	}
	return events, nil
}

func (s *MemoryStore) LatestAgentConversationDone(_ context.Context, ownerID, conversationID string) (agentturns.Event, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var latest agentturns.Event
	found := false
	for key, turn := range s.agentTurns {
		if turn.OwnerID != ownerID || turn.ConversationID != conversationID || turn.State != agentturns.StateSucceeded {
			continue
		}
		for _, event := range s.agentTurnEvents[key] {
			if event.Event != "done" || found && !event.CreatedAt.After(latest.CreatedAt) {
				continue
			}
			latest = event
			found = true
		}
	}
	if found {
		latest.Data = cloneAnyMap(latest.Data)
	}
	return latest, found, nil
}

func (s *MemoryStore) MarkAgentTurnRunning(_ context.Context, ownerID, turnID string) (agentturns.Turn, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := memoryAgentTurnKey(ownerID, turnID)
	turn, ok := s.agentTurns[key]
	if !ok {
		return agentturns.Turn{}, false, agentturns.ErrTurnNotFound
	}
	if turn.State != agentturns.StateAccepted {
		return turn, false, nil
	}
	turn.State = agentturns.StateRunning
	turn.UpdatedAt = time.Now().UTC()
	s.agentTurns[key] = turn
	return turn, true, nil
}

func (s *MemoryStore) AppendAgentTurnEvent(_ context.Context, ownerID, turnID, kind, eventName string, data map[string]any) (agentturns.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := memoryAgentTurnKey(ownerID, turnID)
	turn, ok := s.agentTurns[key]
	if !ok {
		return agentturns.Event{}, agentturns.ErrTurnNotFound
	}
	if turn.State.Terminal() {
		return agentturns.Event{}, nil
	}
	return s.appendAgentTurnEventLocked(key, turn, kind, eventName, data), nil
}

func (s *MemoryStore) FinishAgentTurn(_ context.Context, ownerID, turnID string, state agentturns.State, kind, eventName string, data map[string]any, errorMessage string) (agentturns.Turn, agentturns.Event, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := memoryAgentTurnKey(ownerID, turnID)
	turn, ok := s.agentTurns[key]
	if !ok {
		return agentturns.Turn{}, agentturns.Event{}, false, agentturns.ErrTurnNotFound
	}
	if turn.State.Terminal() {
		return turn, agentturns.Event{}, false, nil
	}
	turn.State = state
	turn.Error = strings.TrimSpace(errorMessage)
	turn.UpdatedAt = time.Now().UTC()
	s.agentTurns[key] = turn
	event := s.appendAgentTurnEventLocked(key, turn, kind, eventName, data)
	return turn, event, true, nil
}

func (s *MemoryStore) StopAgentTurn(ctx context.Context, ownerID, turnID string) (agentturns.Turn, agentturns.Event, bool, error) {
	return s.FinishAgentTurn(ctx, ownerID, turnID, agentturns.StateStopped, "error", "stopped", map[string]any{"error": "turn stopped"}, "turn stopped")
}

func (s *MemoryStore) InterruptAgentTurns(_ context.Context) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int64
	for key, turn := range s.agentTurns {
		if turn.State != agentturns.StateAccepted && turn.State != agentturns.StateRunning {
			continue
		}
		turn.State = agentturns.StateInterrupted
		turn.Error = "server restarted before terminal result was persisted"
		turn.UpdatedAt = time.Now().UTC()
		s.agentTurns[key] = turn
		s.appendAgentTurnEventLocked(key, turn, "error", "interrupted", map[string]any{"error": turn.Error})
		count++
	}
	return count, nil
}

func (s *MemoryStore) appendAgentTurnEventLocked(key string, turn agentturns.Turn, kind, eventName string, data map[string]any) agentturns.Event {
	event := agentturns.Event{
		OwnerID: turn.OwnerID, TurnID: turn.TurnID, ConversationID: turn.ConversationID,
		Seq: int64(len(s.agentTurnEvents[key]) + 1), Kind: kind, Event: eventName,
		Data: cloneAnyMap(agentturns.SanitizeData(data)), CreatedAt: time.Now().UTC(),
	}
	s.agentTurnEvents[key] = append(s.agentTurnEvents[key], event)
	return event
}

func memoryAgentTurnKey(ownerID, turnID string) string {
	return strings.TrimSpace(ownerID) + "\x00" + strings.TrimSpace(turnID)
}
