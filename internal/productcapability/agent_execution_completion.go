package productcapability

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
)

const (
	agentExecutionCompletionCapability = "product.agent_execution.v1"
	agentExecutionCompletionOperation  = "record_completion"
	maxAgentExecutionCompletionBytes   = 8 << 10
)

// recordAgentExecutionCompletion is intentionally outside the advertised
// Registry and the generic operation ledger. Only the authenticated Agent
// service can reach this fixed callback, with no owner/model permission grant.
func (s *Server) recordAgentExecutionCompletion(ctx context.Context, req *capv1.StartOperationRequest) *capv1.StartOperationResponse {
	fail := func(code capv1.ErrorCode, message string) *capv1.StartOperationResponse {
		return &capv1.StartOperationResponse{
			OperationId: req.GetOperationId(),
			State:       capv1.OperationState_OPERATION_STATE_FAILED,
			Error:       capabilityError(code, message),
		}
	}
	if req.GetOperation() != agentExecutionCompletionOperation {
		return fail(capv1.ErrorCode_ERROR_CODE_NOT_FOUND, "private completion operation is not available")
	}
	if req.GetPermission() != nil {
		return fail(capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "private completion callback does not accept owner or model permission")
	}
	if err := capv1.ValidateOperationID(req.GetOperationId()); err != nil {
		return fail(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "operation_id must be a canonical UUID")
	}
	call := req.GetCallContext()
	wantRoute := capv1.NodeAgent + capv1.RouteSeparator + capv1.NodeProduct
	if call == nil || call.GetRoute() != wantRoute || call.GetHop() != 2 || call.GetParentCallId() != "" || call.GetRootOperationId() != req.GetOperationId() {
		return fail(capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED, "private completion callback requires a fresh Agent service route")
	}
	if req.GetExpectedRevision() != 0 {
		return fail(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "expected_revision is not supported")
	}
	if s == nil || s.config == nil || strings.TrimSpace(s.config.ServiceOwnerID) == "" || s.config.ServiceOwnerID != strings.TrimSpace(s.config.ServiceOwnerID) || s.config.ExpectedAccountGeneration <= 0 || s.config.RecordAgentExecutionCompletion == nil {
		return fail(capv1.ErrorCode_ERROR_CODE_NOT_READY, "private completion callback is not configured")
	}
	requestJSON := req.GetRequestJson()
	if len(requestJSON) == 0 || len(requestJSON) > maxAgentExecutionCompletionBytes {
		return fail(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "completion request size is invalid")
	}
	canonical, err := capv1.CanonicalizeJSON(requestJSON)
	if err != nil || !bytes.Equal(canonical, requestJSON) {
		return fail(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, "request_json must be RFC 8785 canonical JSON")
	}
	expectedRequestDigest := sha256.Sum256(requestJSON)
	if len(req.GetRequestDigest()) != sha256.Size || subtle.ConstantTimeCompare(req.GetRequestDigest(), expectedRequestDigest[:]) != 1 {
		return fail(capv1.ErrorCode_ERROR_CODE_CONFLICT, "request_digest does not match request_json")
	}
	receipt, err := decodeAgentExecutionCompletion(requestJSON)
	if err != nil {
		return fail(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, err.Error())
	}
	if req.GetOperationId() != receipt.EventID {
		return fail(capv1.ErrorCode_ERROR_CODE_CONFLICT, "operation_id must equal completion event_id")
	}
	receipt.OwnerID = s.config.ServiceOwnerID
	receipt.AccountGeneration = s.config.ExpectedAccountGeneration
	if err := receipt.Validate(); err != nil {
		return fail(capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT, err.Error())
	}
	replayed, err := s.config.RecordAgentExecutionCompletion(ctx, receipt)
	if err != nil {
		if errors.Is(err, dirextalkdomain.ErrAgentExecutionCompletionConflict) {
			return fail(capv1.ErrorCode_ERROR_CODE_CONFLICT, err.Error())
		}
		return fail(capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED, "record completion failed")
	}
	return &capv1.StartOperationResponse{
		OperationId: req.GetOperationId(),
		State:       capv1.OperationState_OPERATION_STATE_COMPLETED,
		Replayed:    replayed,
	}
}

func decodeAgentExecutionCompletion(raw []byte) (dirextalkdomain.AgentExecutionCompletionReceipt, error) {
	var wire struct {
		EventID        string `json:"event_id"`
		ExecutionID    string `json:"execution_id"`
		RunID          string `json:"run_id"`
		ConversationID string `json:"conversation_id"`
		TurnID         string `json:"turn_id"`
		TerminalState  string `json:"terminal_state"`
		CompletedAt    string `json:"completed_at"`
		PayloadDigest  string `json:"payload_digest"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return dirextalkdomain.AgentExecutionCompletionReceipt{}, fmt.Errorf("completion request must contain exactly the eight public fields: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return dirextalkdomain.AgentExecutionCompletionReceipt{}, errors.New("completion request must contain one JSON object")
	}
	return dirextalkdomain.AgentExecutionCompletionReceipt{
		EventID: wire.EventID, ExecutionID: wire.ExecutionID, RunID: wire.RunID,
		ConversationID: wire.ConversationID, TurnID: wire.TurnID,
		TerminalState: wire.TerminalState,
		CompletedAt:   wire.CompletedAt, PayloadDigest: wire.PayloadDigest,
	}, nil
}
