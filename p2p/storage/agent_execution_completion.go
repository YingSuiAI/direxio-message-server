package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
)

func completionExecutionKey(receipt agentExecutionCompletionReceipt) string {
	return receipt.OwnerID + "\x00" + strconv.FormatInt(receipt.AccountGeneration, 10) + "\x00" + receipt.ExecutionID
}

// RecordAgentExecutionCompletion stores the minimal receipt and its realtime
// invalidation as one atomic unit. The returned boolean is true only for the
// first exact receipt; exact retries return the original event sequence.
func (s *MemoryStore) RecordAgentExecutionCompletion(
	_ context.Context,
	receipt agentExecutionCompletionReceipt,
	event p2pEvent,
) (bool, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Several focused fixtures construct MemoryStore directly. Initialize every
	// map used by this atomic path lazily so those fixtures retain real store
	// semantics instead of panicking on the first completion.
	if s.completionReceipts == nil {
		s.completionReceipts = make(map[string]agentExecutionCompletionReceipt)
	}
	if s.completionExecutions == nil {
		s.completionExecutions = make(map[string]string)
	}
	if s.completionEventSeq == nil {
		s.completionEventSeq = make(map[string]int64)
	}
	if s.eventSeq == nil {
		s.eventSeq = make(map[int64]struct{})
	}
	if s.eventDedupe == nil {
		s.eventDedupe = make(map[string]int64)
	}

	executionKey := completionExecutionKey(receipt)
	eventReceipt, eventFound := s.completionReceipts[receipt.EventID]
	executionEventID, executionFound := s.completionExecutions[executionKey]
	var executionReceipt agentExecutionCompletionReceipt
	if executionFound {
		var ok bool
		executionReceipt, ok = s.completionReceipts[executionEventID]
		if !ok {
			return false, 0, dirextalkdomain.ErrAgentExecutionCompletionConflict
		}
	}
	if eventFound || executionFound {
		if !eventFound || !executionFound || executionEventID != receipt.EventID || eventReceipt != receipt || executionReceipt != receipt {
			return false, 0, dirextalkdomain.ErrAgentExecutionCompletionConflict
		}
		return false, s.completionEventSeq[receipt.EventID], nil
	}
	if existingSeq, ok := s.eventDedupe[event.DedupeKey]; ok {
		return false, existingSeq, dirextalkdomain.ErrAgentExecutionCompletionConflict
	}
	if event.Seq <= 0 {
		return false, 0, fmt.Errorf("completion event sequence is required")
	}
	for {
		if _, exists := s.eventSeq[event.Seq]; !exists {
			break
		}
		event.Seq++
	}
	event = cloneEvent(event)
	s.events = append(s.events, event)
	s.eventSeq[event.Seq] = struct{}{}
	s.eventDedupe[event.DedupeKey] = event.Seq
	s.completionReceipts[receipt.EventID] = receipt
	s.completionExecutions[executionKey] = receipt.EventID
	s.completionEventSeq[receipt.EventID] = event.Seq
	return true, event.Seq, nil
}

func (s *DatabaseStore) RecordAgentExecutionCompletion(
	ctx context.Context,
	receipt agentExecutionCompletionReceipt,
	event p2pEvent,
) (inserted bool, eventSeq int64, err error) {
	payload := event.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return false, 0, err
	}
	err = s.writer.Do(s.db, nil, func(txn *sql.Tx) error {
		// Completion callbacks are rare. Serialize the receipt identity and
		// event sequence allocation so concurrent Agent retries cannot create
		// two durable invalidations in separate message-server processes.
		if _, err := txn.ExecContext(ctx, `LOCK TABLE p2p_agent_execution_completion_receipts IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		byEvent, eventSequence, eventErr := loadAgentExecutionCompletionByEvent(ctx, txn, receipt.EventID)
		byExecution, executionSequence, executionErr := loadAgentExecutionCompletionByExecution(ctx, txn, receipt.OwnerID, receipt.AccountGeneration, receipt.ExecutionID)
		if eventErr != nil && !errors.Is(eventErr, sql.ErrNoRows) {
			return eventErr
		}
		if executionErr != nil && !errors.Is(executionErr, sql.ErrNoRows) {
			return executionErr
		}
		eventFound := eventErr == nil
		executionFound := executionErr == nil
		if eventFound || executionFound {
			if !eventFound || !executionFound || byEvent != receipt || byExecution != receipt || eventSequence != executionSequence {
				return dirextalkdomain.ErrAgentExecutionCompletionConflict
			}
			inserted, eventSeq = false, eventSequence
			return nil
		}

		if _, err := txn.ExecContext(ctx, `LOCK TABLE p2p_events IN SHARE ROW EXCLUSIVE MODE`); err != nil {
			return err
		}
		if err := txn.QueryRowContext(ctx, `SELECT GREATEST(COALESCE(MAX(seq),0)+1,$1) FROM p2p_events`, event.Seq).Scan(&eventSeq); err != nil {
			return err
		}
		if _, err := txn.ExecContext(ctx, `
			INSERT INTO p2p_events (seq,type,room_id,event_id,dedupe_key,payload_json,created_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
		`, eventSeq, event.Type, event.RoomID, event.EventID, event.DedupeKey, string(payloadJSON), event.CreatedAt); err != nil {
			return err
		}
		if _, err := txn.ExecContext(ctx, `
			INSERT INTO p2p_agent_execution_completion_receipts (
				event_id,execution_id,run_id,conversation_id,turn_id,
				terminal_state,completed_at,payload_digest,owner_id,account_generation,event_seq
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		`, receipt.EventID, receipt.ExecutionID, receipt.RunID, receipt.ConversationID, receipt.TurnID,
			receipt.TerminalState, receipt.CompletedAt, receipt.PayloadDigest,
			receipt.OwnerID, receipt.AccountGeneration, eventSeq); err != nil {
			return err
		}
		inserted = true
		return nil
	})
	if err != nil {
		return false, 0, err
	}
	return inserted, eventSeq, nil
}

func loadAgentExecutionCompletionByEvent(
	ctx context.Context,
	txn *sql.Tx,
	eventID string,
) (agentExecutionCompletionReceipt, int64, error) {
	return loadAgentExecutionCompletion(ctx, txn, `event_id=$1::uuid`, eventID)
}

func loadAgentExecutionCompletionByExecution(
	ctx context.Context,
	txn *sql.Tx,
	ownerID string,
	accountGeneration int64,
	executionID string,
) (agentExecutionCompletionReceipt, int64, error) {
	return loadAgentExecutionCompletion(ctx, txn, `owner_id=$1 AND account_generation=$2 AND execution_id=$3::uuid`, ownerID, accountGeneration, executionID)
}

func loadAgentExecutionCompletion(
	ctx context.Context,
	txn *sql.Tx,
	where string,
	args ...any,
) (agentExecutionCompletionReceipt, int64, error) {
	var receipt agentExecutionCompletionReceipt
	var eventSeq int64
	err := txn.QueryRowContext(ctx, `
		SELECT event_id::text,execution_id::text,run_id::text,conversation_id::text,turn_id::text,
			terminal_state,completed_at,payload_digest,owner_id,account_generation,event_seq
		FROM p2p_agent_execution_completion_receipts
		WHERE `+where+`
		FOR UPDATE
	`, args...).Scan(
		&receipt.EventID, &receipt.ExecutionID, &receipt.RunID, &receipt.ConversationID, &receipt.TurnID,
		&receipt.TerminalState, &receipt.CompletedAt, &receipt.PayloadDigest,
		&receipt.OwnerID, &receipt.AccountGeneration, &eventSeq,
	)
	return receipt, eventSeq, err
}
