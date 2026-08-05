package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
)

const agentTurnColumns = `owner_id, turn_id, conversation_id, action, request_digest, state, error_message, created_at, updated_at`

func (s *DatabaseStore) ReserveAgentTurn(ctx context.Context, candidate agentturns.Candidate) (agentturns.Reservation, error) {
	var reservation agentturns.Reservation
	err := s.writer.Do(s.db, nil, func(txn *sql.Tx) error {
		now := time.Now().UTC()
		row := txn.QueryRowContext(ctx, `
			INSERT INTO p2p_native_agent_turns (
				owner_id, turn_id, conversation_id, action, request_digest, state, created_at, updated_at
			) VALUES ($1, $2, $3, $4, $5, 'accepted', $6, $6)
			ON CONFLICT (owner_id, turn_id) DO NOTHING
			RETURNING `+agentTurnColumns,
			candidate.OwnerID, candidate.TurnID, candidate.ConversationID, candidate.Action, candidate.Digest[:], now)
		turn, err := scanAgentTurn(row)
		if err == nil {
			reservation = agentturns.Reservation{Turn: turn, Created: true}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		turn, err = scanAgentTurn(txn.QueryRowContext(ctx, `
			SELECT `+agentTurnColumns+` FROM p2p_native_agent_turns WHERE owner_id = $1 AND turn_id = $2
		`, candidate.OwnerID, candidate.TurnID))
		if err != nil {
			return err
		}
		reservation = agentturns.Reservation{Turn: turn}
		return nil
	})
	return reservation, err
}

func (s *DatabaseStore) GetAgentTurn(ctx context.Context, ownerID, turnID string) (agentturns.Turn, bool, error) {
	turn, err := scanAgentTurn(s.db.QueryRowContext(ctx, `
		SELECT `+agentTurnColumns+` FROM p2p_native_agent_turns WHERE owner_id = $1 AND turn_id = $2
	`, ownerID, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		return agentturns.Turn{}, false, nil
	}
	return turn, err == nil, err
}

func (s *DatabaseStore) ListAgentTurns(ctx context.Context, ownerID, conversationID string, limit int) ([]agentturns.Turn, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+agentTurnColumns+` FROM p2p_native_agent_turns
		WHERE owner_id = $1 AND ($2 = '' OR conversation_id = $2)
		ORDER BY created_at DESC, turn_id DESC LIMIT $3
	`, ownerID, conversationID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	turns := make([]agentturns.Turn, 0, limit)
	for rows.Next() {
		turn, err := scanAgentTurn(rows)
		if err != nil {
			return nil, err
		}
		turns = append(turns, turn)
	}
	return turns, rows.Err()
}

func (s *DatabaseStore) ListAgentTurnEvents(ctx context.Context, ownerID, turnID string, afterSeq int64) ([]agentturns.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT owner_id, turn_id, conversation_id, seq, kind, event_name, data_json, created_at
		FROM p2p_native_agent_turn_events
		WHERE owner_id = $1 AND turn_id = $2 AND seq > $3 ORDER BY seq
	`, ownerID, turnID, afterSeq)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]agentturns.Event, 0)
	for rows.Next() {
		event, err := scanAgentTurnEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *DatabaseStore) LatestAgentConversationDone(ctx context.Context, ownerID, conversationID string) (agentturns.Event, bool, error) {
	event, err := scanAgentTurnEvent(s.db.QueryRowContext(ctx, `
		SELECT event.owner_id, event.turn_id, event.conversation_id, event.seq,
		       event.kind, event.event_name, event.data_json, event.created_at
		FROM p2p_native_agent_turn_events AS event
		JOIN p2p_native_agent_turns AS turn
		  ON turn.owner_id = event.owner_id AND turn.turn_id = event.turn_id
		WHERE turn.owner_id = $1 AND turn.conversation_id = $2
		  AND turn.state = 'succeeded' AND event.event_name = 'done'
		ORDER BY turn.created_at DESC, turn.turn_id DESC, event.seq DESC
		LIMIT 1
	`, ownerID, conversationID))
	if errors.Is(err, sql.ErrNoRows) {
		return agentturns.Event{}, false, nil
	}
	return event, err == nil, err
}

func (s *DatabaseStore) MarkAgentTurnRunning(ctx context.Context, ownerID, turnID string) (agentturns.Turn, bool, error) {
	turn, err := scanAgentTurn(s.db.QueryRowContext(ctx, `
		UPDATE p2p_native_agent_turns SET state = 'running', updated_at = $3
		WHERE owner_id = $1 AND turn_id = $2 AND state = 'accepted'
		RETURNING `+agentTurnColumns,
		ownerID, turnID, time.Now().UTC()))
	if err == nil {
		return turn, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return agentturns.Turn{}, false, err
	}
	turn, ok, err := s.GetAgentTurn(ctx, ownerID, turnID)
	if err != nil {
		return agentturns.Turn{}, false, err
	}
	if !ok {
		return agentturns.Turn{}, false, agentturns.ErrTurnNotFound
	}
	return turn, false, nil
}

func (s *DatabaseStore) AppendAgentTurnEvent(ctx context.Context, ownerID, turnID, kind, eventName string, data map[string]any) (agentturns.Event, error) {
	var event agentturns.Event
	err := s.writer.Do(s.db, nil, func(txn *sql.Tx) error {
		turn, err := lockAgentTurn(ctx, txn, ownerID, turnID)
		if err != nil {
			return err
		}
		if turn.State.Terminal() {
			return nil
		}
		event, err = insertAgentTurnEvent(ctx, txn, turn, kind, eventName, data)
		return err
	})
	return event, err
}

func (s *DatabaseStore) FinishAgentTurn(ctx context.Context, ownerID, turnID string, state agentturns.State, kind, eventName string, data map[string]any, errorMessage string) (agentturns.Turn, agentturns.Event, bool, error) {
	var turn agentturns.Turn
	var event agentturns.Event
	var changed bool
	err := s.writer.Do(s.db, nil, func(txn *sql.Tx) error {
		current, err := lockAgentTurn(ctx, txn, ownerID, turnID)
		if err != nil {
			return err
		}
		turn = current
		if current.State.Terminal() {
			return nil
		}
		now := time.Now().UTC()
		turn, err = scanAgentTurn(txn.QueryRowContext(ctx, `
			UPDATE p2p_native_agent_turns SET state = $3, error_message = $4, updated_at = $5
			WHERE owner_id = $1 AND turn_id = $2 AND state IN ('accepted', 'running')
			RETURNING `+agentTurnColumns,
			ownerID, turnID, string(state), errorMessage, now))
		if err != nil {
			return err
		}
		event, err = insertAgentTurnEvent(ctx, txn, turn, kind, eventName, data)
		changed = err == nil
		return err
	})
	return turn, event, changed, err
}

func (s *DatabaseStore) StopAgentTurn(ctx context.Context, ownerID, turnID string) (agentturns.Turn, agentturns.Event, bool, error) {
	return s.FinishAgentTurn(ctx, ownerID, turnID, agentturns.StateStopped, "error", "stopped", map[string]any{"error": "turn stopped"}, "turn stopped")
}

func (s *DatabaseStore) InterruptAgentTurns(ctx context.Context) (int64, error) {
	var count int64
	err := s.writer.Do(s.db, nil, func(txn *sql.Tx) error {
		message := "server restarted before terminal result was persisted"
		rows, err := txn.QueryContext(ctx, `
			UPDATE p2p_native_agent_turns SET state = 'interrupted', error_message = $1, updated_at = $2
			WHERE state IN ('accepted', 'running') RETURNING `+agentTurnColumns,
			message, time.Now().UTC())
		if err != nil {
			return err
		}
		turns := make([]agentturns.Turn, 0)
		for rows.Next() {
			turn, scanErr := scanAgentTurn(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			turns = append(turns, turn)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, turn := range turns {
			if _, err := insertAgentTurnEvent(ctx, txn, turn, "error", "interrupted", map[string]any{"error": message}); err != nil {
				return err
			}
		}
		count = int64(len(turns))
		return nil
	})
	return count, err
}

func lockAgentTurn(ctx context.Context, txn *sql.Tx, ownerID, turnID string) (agentturns.Turn, error) {
	turn, err := scanAgentTurn(txn.QueryRowContext(ctx, `
		SELECT `+agentTurnColumns+` FROM p2p_native_agent_turns
		WHERE owner_id = $1 AND turn_id = $2 FOR UPDATE
	`, ownerID, turnID))
	if errors.Is(err, sql.ErrNoRows) {
		return agentturns.Turn{}, agentturns.ErrTurnNotFound
	}
	return turn, err
}

func insertAgentTurnEvent(ctx context.Context, txn *sql.Tx, turn agentturns.Turn, kind, eventName string, data map[string]any) (agentturns.Event, error) {
	data = agentturns.SanitizeData(data)
	encoded, err := json.Marshal(data)
	if err != nil {
		return agentturns.Event{}, fmt.Errorf("encode agent turn event: %w", err)
	}
	return scanAgentTurnEvent(txn.QueryRowContext(ctx, `
		INSERT INTO p2p_native_agent_turn_events (
			owner_id, turn_id, conversation_id, seq, kind, event_name, data_json, created_at
		) VALUES (
			$1, $2, $3,
			(SELECT COALESCE(MAX(seq), 0) + 1 FROM p2p_native_agent_turn_events WHERE owner_id = $1 AND turn_id = $2),
			$4, $5, $6::jsonb, $7
		) RETURNING owner_id, turn_id, conversation_id, seq, kind, event_name, data_json, created_at
	`, turn.OwnerID, turn.TurnID, turn.ConversationID, kind, eventName, string(encoded), time.Now().UTC()))
}

type rowScanner interface{ Scan(...any) error }

func scanAgentTurn(row rowScanner) (agentturns.Turn, error) {
	var turn agentturns.Turn
	var digest []byte
	var state string
	err := row.Scan(&turn.OwnerID, &turn.TurnID, &turn.ConversationID, &turn.Action, &digest, &state, &turn.Error, &turn.CreatedAt, &turn.UpdatedAt)
	if err != nil {
		return agentturns.Turn{}, err
	}
	if len(digest) != len(turn.Digest) {
		return agentturns.Turn{}, fmt.Errorf("native agent turn has invalid request digest")
	}
	copy(turn.Digest[:], digest)
	turn.State = agentturns.State(state)
	return turn, nil
}

func scanAgentTurnEvent(row rowScanner) (agentturns.Event, error) {
	var event agentturns.Event
	var encoded []byte
	if err := row.Scan(&event.OwnerID, &event.TurnID, &event.ConversationID, &event.Seq, &event.Kind, &event.Event, &encoded, &event.CreatedAt); err != nil {
		return agentturns.Event{}, err
	}
	if err := json.Unmarshal(encoded, &event.Data); err != nil {
		return agentturns.Event{}, fmt.Errorf("decode native agent turn event: %w", err)
	}
	if event.Data == nil {
		event.Data = map[string]any{}
	}
	return event, nil
}
