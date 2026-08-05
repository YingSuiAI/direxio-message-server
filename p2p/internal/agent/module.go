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

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

// Runner is the only Native Agent runtime boundary exposed by p2p.Config.
type Runner interface {
	Apply(context.Context, string) error
	Invoke(context.Context, string, map[string]any) (map[string]any, error)
	Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error
}

type Config struct {
	Runner  Runner
	Account AccountPort
	OwnerID func() string
}

type Module struct {
	runner  Runner
	account AccountPort
	ownerID func() string
}

func New(cfg Config) *Module {
	return &Module{runner: cfg.Runner, account: cfg.Account, ownerID: cfg.OwnerID}
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

// HasLocalTurnCoordinator and HasLocalVoiceCoordinator remain as diagnostic
// compatibility seams. Both are permanently false after the hard split.
func (m *Module) HasLocalTurnCoordinator() bool  { return false }
func (m *Module) HasLocalVoiceCoordinator() bool { return false }

func (m *Module) Stream(ctx context.Context, action string, params map[string]any, emit func(agentstream.Event) error) error {
	if m == nil || m.runner == nil {
		return fmt.Errorf("external native agent gateway is not configured")
	}
	if err := agentgateway.ValidateActionRequest(action, params); err != nil {
		return err
	}
	return m.runner.Stream(ctx, strings.TrimSpace(action), cloneMap(params), emit)
}

// DurableStream forwards a remote durable operation and projects its events
// into the existing ProductCore websocket DTO. No local turn store or runner
// is constructed in message-server.
func (m *Module) DurableStream(ctx context.Context, ownerID, action string, params map[string]any, emit func(agentstream.StreamEvent) error) error {
	if m == nil || m.runner == nil {
		return fmt.Errorf("external native agent gateway is not configured")
	}
	if strings.TrimSpace(action) != "agent.chat.stream" {
		return fmt.Errorf("%w: %q", agentgateway.ErrUnsupportedAction, action)
	}
	if err := agentgateway.ValidateActionRequest(action, params); err != nil {
		return err
	}
	turnID := strings.TrimSpace(actionbase.String(params["turn_id"]))
	conversationID := agentstream.ConversationID(params)
	if !agentstream.ValidID(turnID) {
		return fmt.Errorf("turn_id is invalid")
	}
	if !agentstream.ValidID(conversationID) {
		return fmt.Errorf("conversation_id is invalid")
	}
	if emit == nil {
		return fmt.Errorf("native agent stream callback is required")
	}
	params = cloneMap(params)
	params["turn_id"] = turnID
	params["conversation_id"] = conversationID
	return m.runner.Stream(ctx, action, params, func(event agentstream.Event) error {
		state := agentstream.StateRunning
		kind := agentstream.EventRuntime
		switch strings.TrimSpace(event.Event) {
		case "accepted":
			state = agentstream.StateAccepted
			kind = agentstream.EventAccepted
		case "done":
			state = agentstream.StateSucceeded
		case "error":
			state = agentstream.StateFailed
			kind = agentstream.EventError
		case "cancelled":
			state = agentstream.StateStopped
			kind = agentstream.EventError
		}
		turn := agentstream.Turn{OwnerID: strings.TrimSpace(ownerID), TurnID: turnID, ConversationID: conversationID, Action: action, State: state, UpdatedAt: time.Now().UTC()}
		return emit(agentstream.StreamEvent{Kind: kind, Turn: turn, TurnID: turnID, ConversationID: conversationID, Event: event.Event, Data: event.Data})
	})
}

type externalDurableRunner interface {
	Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error
	Cancel(context.Context, string, map[string]any) (map[string]any, error)
	Get(context.Context, string, map[string]any) (map[string]any, error)
}

func (m *Module) Cancel(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	runner, ok := m.runner.(externalDurableRunner)
	if !ok {
		return nil, fmt.Errorf("external native agent turn control is unavailable")
	}
	return runner.Cancel(ctx, action, cloneMap(params))
}

func (m *Module) CancelExternal(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	return m.Cancel(ctx, action, params)
}

func (m *Module) stopTurn(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	turnID := strings.TrimSpace(actionbase.String(params["turn_id"]))
	if !agentstream.ValidID(turnID) {
		return nil, actionbase.BadRequest("turn_id is invalid")
	}
	result, err := m.Cancel(ctx, "agent.chat.turn.stop", map[string]any{"turn_id": turnID})
	if err != nil {
		return nil, externalAgentActionError(err)
	}
	if result == nil {
		result = map[string]any{}
	}
	result["turn_id"] = turnID
	result["changed"] = true
	return result, nil
}

func (m *Module) listTurns(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.runner == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "external native agent runtime is not configured")
	}
	result, err := m.runner.Invoke(ctx, "agent.chat.turns.list", cloneMap(params))
	if err != nil {
		return nil, externalAgentActionError(err)
	}
	return result, nil
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
		"agent.chat.stream":       streamOnly,
		"agent.chat.turn.stop":    m.stopTurn,
		"agent.chat.turns.list":   m.listTurns,
	}
	for _, action := range runtimeActions {
		handlers[action] = m.invoke(action)
	}
	for _, action := range externalNativeActions {
		if action == "agent.chat.turn.stop" || action == "agent.chat.turns.list" {
			continue
		}
		if strings.HasSuffix(action, ".stream") {
			handlers[action] = streamOnly
		} else {
			handlers[action] = m.invoke(action)
		}
	}
	return handlers
}

func (m *Module) invoke(action string) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		if m == nil || m.runner == nil {
			return nil, actionbase.StatusError(http.StatusBadGateway, "external native agent gateway is not configured")
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
	var capabilityErr *agentgateway.CapabilityError
	if errors.As(err, &capabilityErr) {
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

func streamOnly(context.Context, map[string]any) (any, *actionbase.Error) {
	return nil, actionbase.BadRequest("action requires websocket")
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
	"agent.config.propose_patch", "agent.chat", "agent.web_search.config.get", "agent.web_search.config.update", "agent.web_search.test",
	"agent.chat.conversations.create", "agent.chat.conversations.list", "agent.chat.conversations.get", "agent.chat.conversations.rename", "agent.chat.conversations.delete", "agent.context.compress", "agent.models.list", "agent.runtime.inspect", "agent.runtime.install", "agent.runtime.which", "agent.runtime.run", "agent.skills.list", "agent.skills.install", "agent.skills.enable", "agent.skills.disable", "agent.skills.uninstall", "agent.skills.registry.search", "agent.mcp.servers.list", "agent.mcp.servers.install", "agent.mcp.servers.enable", "agent.mcp.servers.disable", "agent.mcp.servers.uninstall", "agent.mcp.registry.search", "agent.knowledge.config.get", "agent.knowledge.config.update", "agent.knowledge.sources.list", "agent.knowledge.sources.delete", "agent.knowledge.upload.start", "agent.knowledge.upload.chunk", "agent.knowledge.upload.finish", "agent.knowledge.memory.create", "agent.knowledge.memories.list", "agent.knowledge.memories.update", "agent.knowledge.memories.delete", "agent.knowledge.search", "agent.knowledge.status", "agent.contacts.list", "agent.contacts.search", "agent.rooms.search", "agent.messages.list", "agent.messages.send", "agent.room_members.list", "agent.channel_posts.list", "agent.channel_comments.list", "agent.channel_comments.create", "agent.summarize",
}

var externalNativeActions = []string{
	"agent.account.deprovision", "agent.backends.get", "agent.core.status.get", "agent.core.model_profiles.sync", "agent.core.model_profiles.list", "agent.core.model_profiles.get", "agent.core.model_profiles.delete", "agent.model_profiles.sync", "agent.model_profiles.list", "agent.model_profiles.get", "agent.model_profiles.delete", "agent.schedules.create", "agent.schedules.update", "agent.schedules.get", "agent.schedules.list", "agent.schedules.delete", "agent.schedules.enable", "agent.schedules.disable", "agent.schedules.run_now", "agent.schedule_runs.list", "agent.schedule_runs.get", "agent.core.tasks.get", "agent.core.tasks.list", "agent.core.tasks.cancel", "agent.core.tasks.retry", "agent.core.tasks.events", "agent.core.schedules.create", "agent.core.schedules.get", "agent.core.schedules.list", "agent.core.schedules.update", "agent.core.schedules.pause", "agent.core.schedules.resume", "agent.core.schedules.trigger", "agent.core.schedules.delete", "agent.core.confirmations.get", "agent.core.confirmations.list", "agent.core.confirmations.confirm", "agent.core.confirmations.reject", "agent.core.confirmations.acknowledge_extension_execution_uncertain", "agent.core.mcp.discover", "agent.core.mcp.get", "agent.core.mcp.list", "agent.core.mcp.inspect", "agent.core.mcp.install", "agent.core.mcp.update", "agent.core.mcp.remove", "agent.core.mcp.list_tools", "agent.core.mcp.execute", "agent.core.skills.discover", "agent.core.skills.get", "agent.core.skills.list", "agent.core.skills.inspect", "agent.core.skills.install", "agent.core.skills.update", "agent.core.skills.remove", "agent.core.skills.execute", "agent.core.aws.credentials.create", "agent.core.aws.credentials.update", "agent.core.aws.credentials.delete", "agent.core.aws.credentials.list", "agent.core.aws.credentials.test", "agent.chat.turn.stop", "agent.chat.turns.list", "agent.voice.session.create", "agent.voice.session.start", "agent.voice.session.transcript", "agent.voice.session.interrupt", "agent.voice.session.end", "agent.voice.session.stream", "agent.execution.v2.projects.analyze", "agent.execution.v2.analyses.get", "agent.execution.v2.targets.list", "agent.execution.v2.targets.get", "agent.execution.v2.targets.import", "agent.execution.v2.targets.reserve", "agent.execution.v2.targets.observe", "agent.execution.v2.plans.create", "agent.execution.v2.plans.revise", "agent.execution.v2.plans.get", "agent.execution.v2.plans.list", "agent.execution.v2.deployments.list", "agent.execution.v2.deployments.get", "agent.execution.v2.deployments.events", "agent.execution.v2.runs.create", "agent.execution.v2.runs.get", "agent.execution.v2.runs.list", "agent.execution.v2.runs.cancel", "agent.execution.v2.runs.retry", "agent.execution.v2.runs.reconcile", "agent.execution.v2.runs.events", "agent.execution.v2.confirmations.get", "agent.execution.v2.confirmations.list", "agent.execution.v2.confirmations.confirm", "agent.execution.v2.confirmations.reject", "agent.execution.v2.artifacts.get", "agent.execution.v2.service_bindings.list", "agent.execution.v2.service_bindings.get", "agent.execution.v2.service_bindings.invoke", "agent.execution.v2.secrets.create", "agent.execution.v2.secrets.get", "agent.execution.v2.secrets.list", "agent.execution.v2.secrets.revoke",
}
