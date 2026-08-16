package p2p

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)


// ProductCapabilityDatabase exposes the already-migrated shared PostgreSQL
// handle for durable capability operation records. It is intentionally narrow:
// capability handlers still use ProductCore/Matrix ports, not this database.
func (s *Service) ProductCapabilityDatabase() *sql.DB {
	if s == nil {
		return nil
	}
	if store, ok := s.store.(*p2pstorage.DatabaseStore); ok {
		return store.DB()
	}
	return nil
}

func (s *Service) PreparedMatrixMutationStore() dirextalktransport.PreparedMatrixMutationStore {
	if s == nil {
		return nil
	}
	return s.preparedMatrixStore
}

func (s *Service) DurableMatrixMutationReady() bool {
	if s == nil || s.mcpModule == nil {
		return false
	}
	return s.mcpModule.DurableMatrixMutationReady()
}

// InvokeProductCapability is the single ProductCapability SPI used by the
// external Native Agent gateway. It delegates to the existing ProductCore /
// Matrix MCP modules, so capability additions do not create another HTTP
// action or a second persistence model.
func (s *Service) InvokeProductCapability(ctx context.Context, action string, params map[string]any) (any, error) {
	if s == nil {
		return nil, fmt.Errorf("product capability service is unavailable")
	}
	if params == nil {
		params = map[string]any{}
	} else {
		params = cloneAnyMap(params)
	}
	if action == dirextalkmcp.ActionMessagesSend {
		// Native Agent writes are visibly attributed to the Online Agent Matrix
		// identity. The sender is derived by the MCP module; callers may not
		// supply an arbitrary sender/user field.
		params["agent_gateway"] = true
		if strings.TrimSpace(actionbaseString(params["gateway_source"])) == "" {
			params["gateway_source"] = "native_agent"
		}
	}
	value, mcpErr := s.dirextalkMCPService().Invoke(ctx, action, params)
	if mcpErr != nil {
		return nil, fmt.Errorf("product capability %s failed: %s", action, mcpErr.Message)
	}
	return value, nil
}

type agentExecutionCompletionStore interface {
	RecordAgentExecutionCompletion(context.Context, dirextalkdomain.AgentExecutionCompletionReceipt, dirextalkdomain.Event) (bool, int64, error)
}

// RecordAgentExecutionCompletion persists only the minimal Agent completion
// receipt and the owner realtime invalidation. Result text, artifacts, plan,
// quote, and AWS resource details remain in Agent authority.
func (s *Service) RecordAgentExecutionCompletion(ctx context.Context, receipt dirextalkdomain.AgentExecutionCompletionReceipt) (bool, error) {
	if s == nil || s.eventsModule == nil {
		return false, errors.New("agent execution completion store is unavailable")
	}
	ownerID := s.OwnerMXID()
	if receipt.OwnerID != ownerID || receipt.AccountGeneration != s.accountGeneration {
		return false, dirextalkdomain.ErrAgentExecutionCompletionConflict
	}
	if err := receipt.Validate(); err != nil {
		return false, err
	}
	store, ok := s.store.(agentExecutionCompletionStore)
	if !ok || store == nil {
		return false, errors.New("agent execution completion store is unavailable")
	}
	now := time.Now().UTC()
	event := dirextalkdomain.Event{
		Seq:       now.UnixNano(),
		Type:      "agent.execution.v2.completed",
		EventID:   receipt.EventID,
		DedupeKey: "agent.execution.v2.completed:" + receipt.EventID,
		Payload:   receipt.PublicPayload(),
		CreatedAt: now.Format(time.RFC3339Nano),
	}
	inserted, sequence, err := store.RecordAgentExecutionCompletion(ctx, receipt, event)
	if err != nil {
		return false, err
	}
	if inserted {
		s.eventsModule.NotifyPersisted(sequence)
	}
	return !inserted, nil
}

func actionbaseString(value any) string {
	if text, ok := value.(string); ok {
		return strings.TrimSpace(text)
	}
	return ""
}
