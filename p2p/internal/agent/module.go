// Package agent owns the message-server Native Agent facade. Execution,
// durable turns, schedules, models, memory, extensions, and voice sessions
// are owned by the external dirextalk-agent service.
package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/google/uuid"
)

// Runner is the only Native Agent runtime boundary exposed by p2p.Config.
type Runner interface {
	Apply(context.Context, string) error
	Invoke(context.Context, string, map[string]any) (map[string]any, error)
	Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error
}

type durableChatWatcher interface {
	WatchDurableChat(context.Context, string, string, int64, func(agentstream.Event) error) error
}

// TurnReceipt is the stable identity returned after Agent has durably accepted
// a text turn. Sequence is the cursor of that persisted accepted event.
type TurnReceipt struct {
	TurnID         string            `json:"turn_id"`
	IdempotencyKey string            `json:"idempotency_key"`
	ConversationID string            `json:"conversation_id"`
	State          agentstream.State `json:"state"`
	Revision       int64             `json:"revision"`
	Seq            int64             `json:"seq"`
}

type Config struct {
	Runner  Runner
	Account AccountPort
	OwnerID func() string
	// Readiness is the short, generation-bound catalog lease owned by the
	// message-server. It is checked immediately before every external call.
	Readiness func() error
}

type Module struct {
	runner    Runner
	account   AccountPort
	ownerID   func() string
	readiness func() error
}

func New(cfg Config) *Module {
	return &Module{runner: cfg.Runner, account: cfg.Account, ownerID: cfg.OwnerID, readiness: cfg.Readiness}
}

func (m *Module) ReadyError() error {
	if m == nil {
		return fmt.Errorf("native agent module is unavailable")
	}
	if m.runner == nil {
		return fmt.Errorf("external native agent gateway is not configured")
	}
	return nil
}

func (m *Module) Stream(ctx context.Context, action string, params map[string]any, emit func(agentstream.Event) error) error {
	if m == nil || m.runner == nil {
		return fmt.Errorf("external native agent gateway is not configured")
	}
	if err := m.readinessError(); err != nil {
		return err
	}
	if err := agentgateway.ValidateActionRequest(action, params); err != nil {
		return err
	}
	return m.runner.Stream(ctx, strings.TrimSpace(action), cloneMap(params), emit)
}

// DurableStream forwards a remote durable operation and projects its events
// into the existing ProductCore SSE DTO. No local turn store or runner
// is constructed in message-server.
func (m *Module) DurableStream(ctx context.Context, ownerID, action string, params map[string]any, emit func(agentstream.StreamEvent) error) error {
	if m == nil || m.runner == nil {
		return fmt.Errorf("external native agent gateway is not configured")
	}
	if err := m.readinessError(); err != nil {
		return err
	}
	if strings.TrimSpace(action) != "agent.chat.stream" {
		return fmt.Errorf("%w: %q", agentgateway.ErrUnsupportedAction, action)
	}
	if err := agentgateway.ValidateActionRequest(action, params); err != nil {
		return err
	}
	startID := strings.TrimSpace(actionbase.String(params["idempotency_key"]))
	conversationID := agentstream.ConversationID(params)
	if !canonicalStreamUUID(startID) {
		return fmt.Errorf("idempotency_key is invalid")
	}
	if !canonicalStreamUUID(conversationID) {
		return fmt.Errorf("conversation_id is invalid")
	}
	if emit == nil {
		return fmt.Errorf("native agent stream callback is required")
	}
	params = cloneMap(params)
	params["idempotency_key"] = startID
	params["conversation_id"] = conversationID
	projection := durableProjection{ownerID: strings.TrimSpace(ownerID), action: action, startID: startID, conversationID: conversationID}
	return m.runner.Stream(ctx, action, params, func(event agentstream.Event) error {
		projected, err := projection.project(event)
		if err != nil {
			return err
		}
		return emit(projected)
	})
}

var errTurnAccepted = errors.New("turn accepted")

// StartTurn performs exactly one idempotent admission and returns only after
// the accepted event is persisted. Stopping this observation does not cancel
// the durable Agent operation.
func (m *Module) StartTurn(ctx context.Context, ownerID string, params map[string]any) (TurnReceipt, *actionbase.Error) {
	var receipt TurnReceipt
	err := m.DurableStream(ctx, ownerID, "agent.chat.stream", params, func(event agentstream.StreamEvent) error {
		if event.Kind != agentstream.EventAccepted {
			return nil
		}
		receipt = TurnReceipt{
			TurnID: event.TurnID, IdempotencyKey: event.IdempotencyKey,
			ConversationID: event.ConversationID, State: event.Turn.State,
			Revision: event.Revision, Seq: event.Seq,
		}
		return errTurnAccepted
	})
	if errors.Is(err, errTurnAccepted) {
		if receipt.Seq <= 0 {
			return TurnReceipt{}, actionbase.StatusError(http.StatusBadGateway, "external native agent returned an invalid accepted cursor")
		}
		return receipt, nil
	}
	return TurnReceipt{}, externalAgentActionError(err)
}

// WatchTurn is a read-only durable event attachment. It never calls
// StartOperation and therefore cannot repeat the text mutation.
func (m *Module) WatchTurn(ctx context.Context, ownerID, conversationID, turnID, operationID string, afterSeq int64, emit func(agentstream.StreamEvent) error) error {
	if m == nil || m.runner == nil {
		return fmt.Errorf("external native agent gateway is not configured")
	}
	if err := m.readinessError(); err != nil {
		return err
	}
	watcher, ok := m.runner.(durableChatWatcher)
	if !ok {
		return fmt.Errorf("native agent durable watch is not configured")
	}
	if !canonicalStreamUUID(turnID) || !canonicalStreamUUID(operationID) || !canonicalStreamUUID(conversationID) || afterSeq < 0 || emit == nil {
		return fmt.Errorf("native agent durable watch identity is invalid")
	}
	projection := durableProjection{
		ownerID: strings.TrimSpace(ownerID), action: "agent.chat.stream",
		startID: operationID, conversationID: conversationID, authoredTurnID: turnID,
	}
	return watcher.WatchDurableChat(ctx, operationID, conversationID, afterSeq, func(event agentstream.Event) error {
		projected, err := projection.project(event)
		if err != nil {
			return err
		}
		return emit(projected)
	})
}

// GetTurn reads the Agent-owned authoritative turn ledger and selects the
// exact turn under the path-bound conversation.
func (m *Module) GetTurn(ctx context.Context, conversationID, turnID string) (map[string]any, *actionbase.Error) {
	if m == nil || m.runner == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "external native agent gateway is not configured")
	}
	if err := m.readinessError(); err != nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "external native agent capability catalog is not ready")
	}
	if !canonicalStreamUUID(turnID) || !canonicalStreamUUID(conversationID) {
		return nil, actionbase.BadRequest("turn identity is invalid")
	}
	pageToken := ""
	for {
		params := map[string]any{"conversation_id": conversationID, "limit": int64(1000)}
		if pageToken != "" {
			params["page_token"] = pageToken
		}
		result, err := m.runner.Invoke(ctx, "agent.chat.turns.list", params)
		if err != nil {
			return nil, externalAgentActionError(err)
		}
		turns, _ := result["turns"].([]any)
		for _, raw := range turns {
			turn, ok := raw.(map[string]any)
			if ok && actionbase.String(turn["turn_id"]) == turnID && actionbase.String(turn["conversation_id"]) == conversationID {
				return turn, nil
			}
		}
		next := strings.TrimSpace(actionbase.String(result["next_cursor"]))
		if next == "" || next == pageToken {
			break
		}
		pageToken = next
	}
	return nil, actionbase.StatusError(http.StatusNotFound, "native agent turn was not found")
}

type durableProjection struct {
	ownerID, action, startID, conversationID string
	authoredTurnID                           string
	authoredRevision                         int64
}

func (p *durableProjection) project(event agentstream.Event) (agentstream.StreamEvent, error) {
	eventStartID := strings.TrimSpace(actionbase.String(event.Data["idempotency_key"]))
	eventConversationID := strings.TrimSpace(actionbase.String(event.Data["conversation_id"]))
	eventTurnID := strings.TrimSpace(actionbase.String(event.Data["turn_id"]))
	revision, ok := streamPositiveInt64(event.Data["revision"])
	if eventStartID != p.startID || eventConversationID != p.conversationID || !canonicalStreamUUID(eventTurnID) || !ok {
		return agentstream.StreamEvent{}, fmt.Errorf("%w: native agent stream identity is invalid", agentgateway.ErrInvalidActionResult)
	}
	if p.authoredTurnID == "" {
		p.authoredTurnID, p.authoredRevision = eventTurnID, revision
	} else if eventTurnID != p.authoredTurnID || revision < p.authoredRevision {
		return agentstream.StreamEvent{}, fmt.Errorf("%w: native agent stream identity drifted", agentgateway.ErrInvalidActionResult)
	} else {
		p.authoredRevision = revision
	}
	if event.Seq < 0 {
		return agentstream.StreamEvent{}, fmt.Errorf("%w: native agent stream sequence is invalid", agentgateway.ErrInvalidActionResult)
	}
	state, kind := agentstream.StateRunning, agentstream.EventRuntime
	switch strings.TrimSpace(event.Event) {
	case "accepted":
		state, kind = agentstream.StateAccepted, agentstream.EventAccepted
	case "done":
		state = agentstream.StateSucceeded
	case "error":
		state, kind = agentstream.StateFailed, agentstream.EventError
	case "cancelled":
		state, kind = agentstream.StateStopped, agentstream.EventError
	}
	turn := agentstream.Turn{
		OwnerID: p.ownerID, TurnID: eventTurnID, IdempotencyKey: p.startID,
		ConversationID: p.conversationID, Action: p.action, State: state,
		Revision: revision, UpdatedAt: time.Now().UTC(),
	}
	return agentstream.StreamEvent{
		Kind: kind, Turn: turn, TurnID: eventTurnID, IdempotencyKey: p.startID,
		ConversationID: p.conversationID, Revision: revision, Seq: event.Seq,
		Event: event.Event, Data: event.Data,
	}, nil
}

func canonicalStreamUUID(value string) bool {
	if value == "" || value != strings.TrimSpace(value) {
		return false
	}
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func streamPositiveInt64(value any) (int64, bool) {
	parsed, ok := streamNonnegativeInt64(value)
	return parsed, ok && parsed > 0
}

func streamNonnegativeInt64(value any) (int64, bool) {
	switch item := value.(type) {
	case int:
		return int64(item), item >= 0
	case int64:
		return item, item >= 0
	case float64:
		parsed := int64(item)
		return parsed, item >= 0 && float64(parsed) == item
	default:
		return 0, false
	}
}

func (m *Module) Handlers() map[string]actionbase.Handler {
	if m == nil {
		return nil
	}
	handlers := map[string]actionbase.Handler{
		actionPassword:            m.accountPassword,
		actionMatrixSessionCreate: m.createMatrixSession,
		actionConfigGet:           m.getConfig,
		actionConfigUpdate:        m.updateConfig,
	}
	for _, action := range runtimeActions {
		handlers[action] = m.invoke(action)
	}
	for _, action := range externalNativeActions {
		handlers[action] = m.invoke(action)
	}
	return handlers
}

func (m *Module) readinessError() error {
	if m != nil && m.readiness != nil {
		return m.readiness()
	}
	return nil
}

func (m *Module) invoke(action string) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		if m == nil || m.runner == nil {
			return nil, actionbase.StatusError(http.StatusBadGateway, "external native agent gateway is not configured")
		}
		if err := m.readinessError(); err != nil {
			return nil, actionbase.StatusError(http.StatusServiceUnavailable, "external native agent capability catalog is not ready")
		}
		if err := agentgateway.ValidateActionRequest(action, params); err != nil {
			return nil, externalAgentActionError(err)
		}
		result, err := m.runner.Invoke(ctx, strings.TrimSpace(action), cloneMap(params))
		if err != nil {
			return nil, externalAgentActionError(err)
		}
		return result, nil
	}
}

func externalAgentActionError(err error) *actionbase.Error {
	if err == nil {
		return nil
	}
	if errors.Is(err, agentgateway.ErrUnsupportedAction) {
		return actionbase.StatusError(http.StatusNotImplemented, err.Error())
	}
	if errors.Is(err, agentgateway.ErrInvalidActionRequest) {
		return actionbase.BadRequest(err.Error())
	}
	if errors.Is(err, agentgateway.ErrInvalidActionResult) {
		return actionbase.StatusError(http.StatusBadGateway, "external native agent returned an invalid response")
	}
	if errors.Is(err, agentgateway.ErrCatalogInvalid) {
		return actionbase.StatusError(http.StatusServiceUnavailable, "external native agent capability contract is unavailable")
	}
	var capabilityErr *agentgateway.CapabilityError
	if errors.As(err, &capabilityErr) {
		if capabilityErr.Code == capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED && capabilityErr.ClientCode == agentgateway.KnowledgeQuotaExceededCode {
			return actionbase.CodedError(http.StatusRequestEntityTooLarge, agentgateway.KnowledgeQuotaExceededCode, capabilityErr.Error())
		}
		if capabilityErr.ClientCode != "" {
			return actionbase.CodedError(agentgateway.CapabilityHTTPStatus(capabilityErr.Code), capabilityErr.ClientCode, capabilityErr.Error())
		}
		return actionbase.StatusError(agentgateway.CapabilityHTTPStatus(capabilityErr.Code), capabilityErr.Error())
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(message, "forbidden"), strings.Contains(message, "permission"), strings.Contains(message, "scope"):
		return actionbase.StatusError(http.StatusForbidden, "external native agent denied the request")
	case strings.Contains(message, "not found"), strings.Contains(message, "does not exist"):
		return actionbase.StatusError(http.StatusNotFound, "external native agent resource was not found")
	case strings.Contains(message, "conflict"), strings.Contains(message, "revision"), strings.Contains(message, "idempotency"):
		return actionbase.StatusError(http.StatusConflict, "external native agent reported a conflict")
	case strings.Contains(message, "invalid"), strings.Contains(message, "required"), strings.Contains(message, "malformed"), strings.Contains(message, "server-derived"):
		return actionbase.BadRequest("external native agent rejected the request")
	case strings.Contains(message, "not ready"), strings.Contains(message, "unavailable"), strings.Contains(message, "not configured"):
		return actionbase.StatusError(http.StatusServiceUnavailable, "external native agent is unavailable")
	case strings.Contains(message, "deadline exceeded"), strings.Contains(message, "timeout"):
		return actionbase.StatusError(http.StatusGatewayTimeout, "external native agent request timed out")
	default:
		return actionbase.StatusError(http.StatusBadGateway, "external native agent operation failed")
	}
}

func cloneMap(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func (m *Module) currentOwnerID() string {
	if m != nil && m.ownerID != nil {
		return strings.TrimSpace(m.ownerID())
	}
	return "owner"
}

var runtimeActions = []string{
	"agent.chat", "agent.web_search.config.get", "agent.web_search.config.update", "agent.web_search.test", "agent.text_tools.config.get", "agent.text_tools.config.update", "agent.text_tools.execute",
	"agent.chat.conversations.create", "agent.chat.conversations.list", "agent.chat.conversations.get", "agent.chat.conversations.rename", "agent.chat.conversations.delete", "agent.context.compress", "agent.models.list", "agent.runtime.inspect", "agent.runtime.install", "agent.runtime.which", "agent.runtime.run", "agent.knowledge.config.get", "agent.knowledge.config.update", "agent.knowledge.sources.list", "agent.knowledge.sources.delete", "agent.knowledge.upload.start", "agent.knowledge.upload.chunk", "agent.knowledge.upload.finish", "agent.knowledge.search", "agent.knowledge.status", "agent.contacts.list", "agent.contacts.search", "agent.rooms.search", "agent.messages.list", "agent.messages.send", "agent.room_members.list", "agent.channel_posts.list", "agent.channel_comments.list", "agent.channel_comments.create", "agent.summarize",
}

var externalNativeActions = []string{
	"agent.memory.config.get", "agent.memory.config.update", "agent.memory.status", "agent.memory.facts.update", "agent.memory.facts.delete",
	"agent.static_sites.list", "agent.static_sites.delete",
	"agent.workers.list", "agent.workers.get", "agent.workers.destroy", "agent.workers.bind_domain", "agent.workers.unbind_domain",
	"agent.account.deprovision", "agent.backends.get", "agent.model_profiles.sync", "agent.model_profiles.list", "agent.model_profiles.get", "agent.model_profiles.test", "agent.model_profiles.delete", "agent.core.tasks.get", "agent.core.tasks.list", "agent.core.tasks.cancel", "agent.core.tasks.retry", "agent.core.tasks.events", "agent.core.schedules.create", "agent.core.schedules.get", "agent.core.schedules.list", "agent.core.schedules.update", "agent.core.schedules.pause", "agent.core.schedules.resume", "agent.core.schedules.trigger", "agent.core.schedules.delete", "agent.core.confirmations.get", "agent.core.confirmations.list", "agent.core.confirmations.confirm", "agent.core.confirmations.reject", "agent.core.confirmations.acknowledge_extension_execution_uncertain", "agent.core.mcp.discover", "agent.core.mcp.get", "agent.core.mcp.list", "agent.core.mcp.inspect", "agent.core.mcp.install", "agent.core.mcp.update", "agent.core.mcp.remove", "agent.core.mcp.list_tools", "agent.core.mcp.execute", "agent.core.skills.discover", "agent.core.skills.get", "agent.core.skills.list", "agent.core.skills.inspect", "agent.core.skills.install", "agent.core.skills.update", "agent.core.skills.remove", "agent.core.skills.execute", "agent.core.aws.credentials.create", "agent.core.aws.credentials.update", "agent.core.aws.credentials.delete", "agent.core.aws.credentials.list", "agent.core.aws.credentials.test", "agent.chat.attachment.begin", "agent.chat.attachment.append", "agent.chat.attachment.commit", "agent.image_tools.upload.begin", "agent.image_tools.upload.append", "agent.image_tools.upload.commit", "agent.image_tools.extract_text", "agent.image_tools.translate_text", "agent.chat.turn.stop", "agent.chat.turn.steer", "agent.chat.turns.list", "agent.voice.session.create", "agent.voice.session.start", "agent.voice.session.transcript", "agent.voice.session.interrupt", "agent.voice.session.end",
	"agent.execution.v2.plans.get", "agent.execution.v2.plans.list", "agent.execution.v2.runs.get", "agent.execution.v2.runs.list", "agent.execution.v2.runs.cancel", "agent.execution.v2.runs.events", "agent.execution.v2.artifacts.get", "agent.execution.v2.artifacts.download", "agent.execution.v2.artifacts.delete",
}
