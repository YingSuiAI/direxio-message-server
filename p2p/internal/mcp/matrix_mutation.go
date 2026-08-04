package mcp

import (
	"context"
	"errors"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
)

func (m *Module) sendMatrixMutation(ctx context.Context, request dirextalktransport.SendMessageRequest, capability, operation string) (dirextalktransport.SendMessageResult, error) {
	if m == nil || m.matrix == nil {
		return dirextalktransport.SendMessageResult{}, errors.New("Matrix transport is unavailable")
	}
	operationContext, hasOperation := dirextalktransport.CapabilityOperationContextFrom(ctx)
	if !hasOperation {
		// HTTP/MCP callers that are not Product Capability operations retain the
		// existing synchronous product path. External Agent write descriptors
		// are withheld unless DurableMatrixMutationReady is true.
		return m.matrix.SendMessage(ctx, request)
	}
	preparedPort, ok := m.matrix.(dirextalktransport.PreparedMessagePort)
	if !ok || m.preparedStore == nil {
		return dirextalktransport.SendMessageResult{}, errors.New("durable Matrix mutation is unavailable")
	}
	return dirextalktransport.ExecutePreparedMatrixMutation(ctx, preparedPort, m.preparedStore, operationContext, capability, operation, request)
}
