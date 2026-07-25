package p2p

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	agentcoremodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcore"
	coreturns "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcoreturns"
	realtimewsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/realtimews"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type agentCoreStreamAdapter struct {
	core   *agentcoremodule.Client
	ledger *coreturns.Ledger
}

type coreTerminalProjectionError struct {
	event realtimewsmodule.CoreStreamEvent
}

func (e coreTerminalProjectionError) Error() string { return coreturns.ErrTerminal.Error() }
func (e coreTerminalProjectionError) Unwrap() error { return coreturns.ErrTerminal }
func (e coreTerminalProjectionError) CoreTerminalEvent() realtimewsmodule.CoreStreamEvent {
	return e.event
}

func (a *agentCoreStreamAdapter) StartCoreStream(ctx context.Context, owner, clientID string, params map[string]any, after int64, emit func(realtimewsmodule.CoreStreamEvent) error) error {
	conversationID, _ := params["conversation_id"].(string)
	message, messagePresent := params["message"].(string)
	profile, _ := params["client_model_profile_id"].(string)
	providedDigest, _ := params["request_digest"].(string)
	digest, err := parseRequestDigest(providedDigest)
	if err != nil {
		return coreturns.ErrDigestRequired
	}
	req := agentcoremodule.TurnStart{ClientTurnID: clientID, ConversationID: conversationID, Message: message}
	if raw, ok := params["expected_revision"]; ok {
		if v, ok := numberInt64(raw); ok {
			if v < 0 {
				return errors.New("invalid expected revision")
			}
			// Zero is the wire-level sentinel for "no revision check". Keep
			// the protobuf optional field absent so Core can distinguish it.
			if v > 0 {
				req.ExpectedRevision = &v
			}
		} else {
			return errors.New("invalid expected revision")
		}
	}
	// Bind every client parameter (including extension selections) while only
	// the fixed-size digest enters the durable ledger.
	prior, priorErr := a.ledger.Get(ctx, owner, clientID)
	if priorErr != nil && !errors.Is(priorErr, coreturns.ErrNotFound) {
		return priorErr
	}
	if priorErr == nil {
		if !bytes.Equal(digest, prior.RequestDigest) {
			return coreturns.ErrDigestMismatch
		}
		if semanticParamsPresent(params) {
			if !completeInitialSemanticParams(params, message, messagePresent) || !bytes.Equal(digest, coreturns.DigestParams(params)) {
				return coreturns.ErrDigestMismatch
			}
		} else if prior.CoreTurnID == "" {
			return coreturns.ErrReconciliationRequired
		}
		req.ModelProfileID = prior.CoreProfileID
		req.ModelProfileRevision = prior.ModelProfileRevision
	}
	if errors.Is(priorErr, coreturns.ErrNotFound) {
		if !messagePresent || strings.TrimSpace(message) == "" {
			return coreturns.ErrDigestRequired
		}
		if !bytes.Equal(digest, coreturns.DigestParams(params)) {
			return coreturns.ErrDigestMismatch
		}
		resolved, err := a.core.ResolveModelProfile(ctx, profile)
		if err != nil {
			return err
		}
		req.ModelProfileID = resolved.CoreProfileID
		req.ModelProfileRevision = resolved.Revision
	}
	rec, existing, err := a.ledger.Reserve(ctx, coreturns.Record{OwnerID: owner, ClientTurnID: clientID, RequestDigest: digest, ConversationID: conversationID, CoreProfileID: req.ModelProfileID, ModelProfileRevision: req.ModelProfileRevision})
	if err != nil {
		return err
	}
	if existing && rec.ConversationID != "" && conversationID != "" && rec.ConversationID != conversationID {
		return coreturns.ErrConflict
	}
	if existing {
		req.ModelProfileID = rec.CoreProfileID
		req.ModelProfileRevision = rec.ModelProfileRevision
	}
	if rec.ModelProfileRevision == 0 && req.ModelProfileRevision > 0 {
		rec.ModelProfileRevision = req.ModelProfileRevision
	}
	coreTurnID := rec.CoreTurnID
	if coreTurnID == "" {
		turn, err := a.core.StartCoreTurn(ctx, req)
		if err != nil {
			return err
		}
		if err := agentcoremodule.ValidateCoreTurn(turn, turn.CoreTurnID, turn.ConversationID); err != nil {
			return err
		}
		coreTurnID = turn.CoreTurnID
		rec.CoreTurnID = coreTurnID
		rec.ConversationID = turn.ConversationID
		rec.Status = turn.Status
		rec.LastSequence = turn.LastSequence
		rec.CoreRevision = turn.Revision
		if err := a.persistLedger(ctx, owner, clientID, turn.CoreTurnID, turn.ConversationID, turn.Status, turn.LastSequence, turn.Revision, rec.ModelProfileRevision, "", turn.TerminalCode, turn.TerminalSummary); err != nil {
			return err
		}
	} else {
		// After a Message Server restart Core remains authoritative. Reconcile
		// the immutable turn projection before attaching a new watcher.
		turn, err := a.core.GetCoreTurn(ctx, coreTurnID)
		if err != nil {
			return err
		}
		if err := agentcoremodule.ValidateCoreTurn(turn, coreTurnID, rec.ConversationID); err != nil {
			return err
		}
		rec.CoreTurnID, rec.ConversationID, rec.Status, rec.LastSequence = turn.CoreTurnID, turn.ConversationID, turn.Status, turn.LastSequence
		if err := a.persistLedger(ctx, owner, clientID, turn.CoreTurnID, turn.ConversationID, turn.Status, turn.LastSequence, turn.Revision, rec.ModelProfileRevision, "", turn.TerminalCode, turn.TerminalSummary); err != nil {
			return err
		}
	}
	if err := emit(realtimewsmodule.CoreStreamEvent{Kind: "accepted", TurnID: clientID, CoreTurnID: coreTurnID, ConversationID: rec.ConversationID, Summary: rec.Status, LastSeq: rec.LastSequence}); err != nil {
		return err
	}
	events, err := a.core.WatchCoreTurnEvents(ctx, coreTurnID, after)
	if err != nil {
		return err
	}
	terminalObserved := false
	for ev := range events {
		if ev.Err != nil {
			if ev.ErrorCode == "agent_core_stream_ended" && terminalObserved {
				continue
			}
			if err := emit(realtimewsmodule.CoreStreamEvent{Kind: "error", TurnID: clientID, Code: ev.ErrorCode, Summary: ev.ErrorSummary, Retryable: ev.ErrorCode != "agent_core_stream_idle"}); err != nil {
				return err
			}
			continue
		}
		if ev.ReplayGap {
			if err := a.persistLedger(ctx, owner, clientID, coreTurnID, rec.ConversationID, rec.Status, rec.LastSequence, rec.CoreRevision, rec.ModelProfileRevision, "", "", ""); err != nil {
				return err
			}
			if err := emit(realtimewsmodule.CoreStreamEvent{Kind: "error", TurnID: clientID, Code: "agent_core_replay_gap", Retryable: false, FirstSeq: ev.FirstSequence, LastSeq: ev.LastSequence}); err != nil {
				return err
			}
			continue
		}
		rec.LastSequence = ev.Sequence
		if ev.Kind == "done" || ev.Kind == "completed" || ev.Kind == "error" || ev.Kind == "canceled" {
			if ev.Kind == "error" {
				rec.Status = "failed"
			} else if ev.Kind == "done" {
				rec.Status = "completed"
			} else {
				rec.Status = ev.Kind
			}
			terminalObserved = true
		}
		if err := a.persistLedger(ctx, owner, clientID, coreTurnID, rec.ConversationID, rec.Status, rec.LastSequence, rec.CoreRevision, rec.ModelProfileRevision, ev.Kind, ev.ErrorCode, ev.ErrorSummary); err != nil {
			return err
		}
		kind := "event"
		if ev.Kind == "error" {
			kind = "error"
		}
		if err := emit(realtimewsmodule.CoreStreamEvent{Kind: kind, TurnID: clientID, CoreTurnID: coreTurnID, ConversationID: rec.ConversationID, Event: ev.Kind, Code: ev.ErrorCode, Summary: ev.ErrorSummary, Seq: ev.Sequence, Data: map[string]any{"text": ev.Text}}); err != nil {
			return err
		}
	}
	return nil
}

func (a *agentCoreStreamAdapter) persistLedger(ctx context.Context, owner, clientID, coreTurn, conversation, status string, seq, coreRevision, modelProfileRevision int64, eventKind, terminalCode, terminalSummary string) error {
	if err := a.ledger.Update(ctx, owner, clientID, coreTurn, conversation, status, seq, coreRevision, modelProfileRevision, eventKind, terminalCode, terminalSummary); err != nil {
		return fmt.Errorf("%w: %v", coreturns.ErrPersistenceReconciliation, err)
	}
	return nil
}

func semanticParamsPresent(params map[string]any) bool {
	for key := range params {
		if key != "request_digest" && key != "after_seq" && key != "after_sequence" {
			return true
		}
	}
	return false
}

func completeInitialSemanticParams(params map[string]any, message string, messagePresent bool) bool {
	if !messagePresent || strings.TrimSpace(message) == "" {
		return false
	}
	profile, profilePresent := params["client_model_profile_id"].(string)
	_, conversationPresent := params["conversation_id"]
	return profilePresent && strings.TrimSpace(profile) != "" && conversationPresent
}

func (a *agentCoreStreamAdapter) CancelCoreStream(ctx context.Context, owner, clientID string) error {
	rec, err := a.ledger.Get(ctx, owner, clientID)
	if err != nil {
		return err
	}
	if rec.CoreTurnID == "" {
		return coreturns.ErrReconciliationRequired
	}
	turn, err := a.core.CancelCoreTurn(ctx, rec.CoreTurnID, clientID, rec.CoreRevision)
	if err != nil && status.Code(err) == codes.Aborted {
		current, getErr := a.core.GetCoreTurn(ctx, rec.CoreTurnID)
		if getErr != nil {
			return getErr
		}
		if isTerminalStatus(current.Status) {
			turn, err = current, nil
		} else {
			turn, err = a.core.CancelCoreTurn(ctx, rec.CoreTurnID, clientID, current.Revision)
		}
	}
	if err != nil {
		return err
	}
	if err := agentcoremodule.ValidateCoreTurn(turn, rec.CoreTurnID, rec.ConversationID); err != nil {
		return err
	}
	if err := a.persistLedger(ctx, owner, clientID, turn.CoreTurnID, turn.ConversationID, turn.Status, turn.LastSequence, turn.Revision, rec.ModelProfileRevision, "", turn.TerminalCode, turn.TerminalSummary); err != nil {
		return err
	}
	if isTerminalStatus(turn.Status) && turn.Status != "canceled" {
		return coreTerminalProjectionError{event: terminalProjection(turn)}
	}
	if !isTerminalStatus(turn.Status) {
		waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		for !isTerminalStatus(turn.Status) {
			select {
			case <-waitCtx.Done():
				return waitCtx.Err()
			case <-time.After(50 * time.Millisecond):
			}
			turn, err = a.core.GetCoreTurn(waitCtx, rec.CoreTurnID)
			if err != nil {
				return err
			}
			if err := a.persistLedger(waitCtx, owner, clientID, turn.CoreTurnID, turn.ConversationID, turn.Status, turn.LastSequence, turn.Revision, rec.ModelProfileRevision, "", turn.TerminalCode, turn.TerminalSummary); err != nil {
				return err
			}
		}
		if turn.Status != "canceled" {
			return coreTerminalProjectionError{event: terminalProjection(turn)}
		}
	}
	return nil
}

func terminalProjection(turn agentcoremodule.Turn) realtimewsmodule.CoreStreamEvent {
	return realtimewsmodule.CoreStreamEvent{
		Kind:           "event",
		CoreTurnID:     turn.CoreTurnID,
		ConversationID: turn.ConversationID,
		Event:          "done",
		Seq:            turn.LastSequence,
		Summary:        turn.Status,
		Data: map[string]any{
			"status":           turn.Status,
			"terminal_code":    turn.TerminalCode,
			"terminal_summary": turn.TerminalSummary,
		},
	}
}

func isTerminalStatus(status string) bool {
	return status == "completed" || status == "done" || status == "succeeded" || status == "failed" || status == "canceled" || status == "uncertain"
}

func numberInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case float64:
		return int64(n), float64(int64(n)) == n
	case int32:
		return int64(n), true
	}
	return 0, false
}

func parseRequestDigest(value string) ([]byte, error) {
	if len(value) != 64 || strings.ToLower(value) != value {
		return nil, coreturns.ErrDigestRequired
	}
	digest, err := hex.DecodeString(value)
	if err != nil || len(digest) != 32 {
		return nil, coreturns.ErrDigestRequired
	}
	return digest, nil
}
