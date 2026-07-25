package realtimews

import (
	"context"
	"errors"
	"net/http"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	coreturns "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcoreturns"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (m *Module) startAgentCoreStream(ctx context.Context, client *connection, frame map[string]any) {
	id := actionbase.String(frame["turn_id"])
	if id == "" {
		id = actionbase.String(frame["id"])
	}
	if id == "" {
		client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_invalid_argument", false, 0, 0))
		return
	}
	if client.record.Role != "owner" {
		client.send(coreStreamError(id, http.StatusForbidden, "M_FORBIDDEN", false, 0, 0))
		return
	}
	if m.core == nil {
		client.send(coreStreamError(id, http.StatusBadGateway, "agent_core_unavailable", true, 0, 0))
		return
	}
	rawDigest, digestPresent := frame["request_digest"]
	if !digestPresent || rawDigest == nil {
		client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_digest_required", false, 0, 0))
		return
	}
	requestDigest, digestTypeOK := rawDigest.(string)
	if !digestTypeOK || requestDigest != strings.TrimSpace(requestDigest) {
		client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_invalid_argument", false, 0, 0))
		return
	}
	if requestDigest == "" {
		client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_digest_required", false, 0, 0))
		return
	}
	if !isCanonicalRequestDigest(requestDigest) {
		client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_invalid_argument", false, 0, 0))
		return
	}
	params := map[string]any{}
	if raw, ok := frame["params"].(map[string]any); ok {
		params = cloneMap(raw)
	} else if frame["params"] != nil {
		client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_invalid_argument", false, 0, 0))
		return
	}
	// Keep the transport digest available to the adapter without making it part
	// of the immutable Core request digest.
	params["request_digest"] = requestDigest
	if extensionsNonEmpty(params["extensions"]) {
		client.send(coreStreamError(id, http.StatusBadGateway, "agent_core_incompatible", false, 0, 0))
		return
	}
	after := int64(0)
	_, hasAfterSeq := frame["after_seq"]
	_, hasAfterSequence := frame["after_sequence"]
	if raw, ok := frame["after_seq"]; ok {
		parsed, valid := strictJSONInt64(raw)
		if !valid {
			client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_invalid_argument", false, 0, 0))
			return
		}
		after = parsed
	}
	if raw, ok := frame["after_sequence"]; ok {
		canonicalAfter, valid := strictJSONInt64(raw)
		if !valid {
			client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_invalid_argument", false, 0, 0))
			return
		}
		if hasAfterSeq && hasAfterSequence && canonicalAfter != after {
			client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_invalid_argument", false, 0, 0))
			return
		}
		after = canonicalAfter
	}
	if after < 0 {
		client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_invalid_argument", false, 0, 0))
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	if !client.startStream(id, cancel) {
		cancel()
		client.send(coreStreamError(id, http.StatusConflict, "agent_core_conflict", false, 0, 0))
		return
	}
	go func() {
		defer client.finishStream(id)
		err := m.core.StartCoreStream(streamCtx, client.record.UserID, id, params, after, func(ev CoreStreamEvent) error { return client.sendBlocking(streamCtx, coreEventFrame(id, ev)) })
		if err != nil && streamCtx.Err() == nil {
			status, code, retryable := coreStreamErrorCode(err)
			if errors.Is(err, coreturns.ErrConflict) {
				status, code, retryable = http.StatusConflict, "agent_core_conflict", false
			}
			if errors.Is(err, coreturns.ErrDigestRequired) {
				status, code, retryable = http.StatusBadRequest, "agent_core_digest_required", false
			}
			if errors.Is(err, coreturns.ErrDigestMismatch) {
				status, code, retryable = http.StatusConflict, "agent_core_digest_mismatch", false
			}
			if errors.Is(err, coreturns.ErrReconciliationRequired) {
				status, code, retryable = http.StatusConflict, "agent_core_reconciliation_required", false
			}
			if errors.Is(err, coreturns.ErrPersistenceReconciliation) {
				status, code, retryable = http.StatusServiceUnavailable, "agent_core_persistence_reconciliation", true
			}
			_ = client.sendBlocking(ctx, coreStreamError(id, status, code, retryable, 0, 0))
		}
	}()
}

func isCanonicalRequestDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func strictJSONInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case float64:
		parsed := int64(number)
		return parsed, float64(parsed) == number
	case int:
		return int64(number), true
	case int64:
		return number, true
	default:
		return 0, false
	}
}

func extensionsNonEmpty(value any) bool {
	switch values := value.(type) {
	case []any:
		return len(values) > 0
	case []map[string]any:
		return len(values) > 0
	default:
		return value != nil
	}
}

func (m *Module) cancelAgentCoreStream(ctx context.Context, client *connection, frame map[string]any) {
	id := actionbase.String(frame["turn_id"])
	if id == "" {
		id = actionbase.String(frame["id"])
	}
	if id == "" {
		client.send(coreStreamError(id, http.StatusBadRequest, "agent_core_invalid_argument", false, 0, 0))
		return
	}
	if client.record.Role != "owner" {
		client.send(coreStreamError(id, http.StatusForbidden, "M_FORBIDDEN", false, 0, 0))
		return
	}
	if m.core == nil {
		client.send(coreStreamError(id, http.StatusBadGateway, "agent_core_unavailable", true, 0, 0))
		return
	}
	if err := m.core.CancelCoreStream(ctx, client.record.UserID, id); err != nil {
		if terminal, ok := err.(CoreTerminalProjectionError); ok {
			ev := terminal.CoreTerminalEvent()
			ev.TurnID = id
			client.send(coreEventFrame(id, ev))
			return
		}
		if errors.Is(err, coreturns.ErrTerminal) {
			client.send(coreStreamError(id, http.StatusConflict, "agent_core_terminal", false, 0, 0))
			return
		}
		status, code, retryable := coreStreamErrorCode(err)
		if errors.Is(err, coreturns.ErrNotFound) {
			status, code, retryable = http.StatusNotFound, "agent_core_not_found", false
		}
		if errors.Is(err, coreturns.ErrReconciliationRequired) {
			status, code, retryable = http.StatusConflict, "agent_core_reconciliation_required", false
		}
		if errors.Is(err, coreturns.ErrPersistenceReconciliation) {
			status, code, retryable = http.StatusServiceUnavailable, "agent_core_persistence_reconciliation", true
		}
		client.send(coreStreamError(id, status, code, retryable, 0, 0))
		return
	}
	client.cancelStream(id)
	client.send(map[string]any{"type": "server.agent_core_stream.cancelled", "turn_id": id})
}

func coreEventFrame(id string, ev CoreStreamEvent) map[string]any {
	base := map[string]any{"turn_id": id}
	switch ev.Kind {
	case "accepted":
		base["type"] = "server.agent_core_stream.accepted"
		base["core_turn_id"] = ev.CoreTurnID
		base["conversation_id"] = ev.ConversationID
		base["status"] = ev.Summary
		base["latest_seq"] = ev.LastSeq
	case "error":
		base["type"] = "server.agent_core_stream.error"
		base["code"] = ev.Code
		base["retryable"] = ev.Retryable
		if ev.FirstSeq > 0 {
			base["first_seq"] = ev.FirstSeq
		}
		if ev.LastSeq > 0 {
			base["last_seq"] = ev.LastSeq
		}
	case "cancelled":
		base["type"] = "server.agent_core_stream.cancelled"
	default:
		base["type"] = "server.agent_core_stream.event"
		base["seq"] = ev.Seq
		base["event"] = strings.TrimSpace(ev.Event)
		if base["event"] == "" {
			base["event"] = "message"
		}
		base["data"] = ev.Data
	}
	return base
}
func coreStreamError(id string, status int, code string, retryable bool, first, last int64) map[string]any {
	return map[string]any{"type": "server.agent_core_stream.error", "turn_id": id, "code": code, "retryable": retryable, "status": status, "first_seq": first, "last_seq": last}
}

func coreStreamErrorCode(err error) (int, string, bool) {
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusServiceUnavailable, "agent_core_unavailable", true
	}
	switch status.Code(err) {
	case codes.DeadlineExceeded, codes.Unavailable:
		return http.StatusServiceUnavailable, "agent_core_unavailable", true
	case codes.Unauthenticated, codes.PermissionDenied:
		return http.StatusBadGateway, "agent_core_trust_failed", false
	case codes.InvalidArgument:
		return http.StatusBadRequest, "agent_core_invalid_argument", false
	case codes.Aborted:
		return http.StatusConflict, "agent_core_conflict", false
	case codes.NotFound:
		return http.StatusNotFound, "agent_core_not_found", false
	case codes.Unimplemented:
		return http.StatusBadGateway, "agent_core_incompatible", false
	}
	return http.StatusBadGateway, "agent_core_upstream_failed", true
}
