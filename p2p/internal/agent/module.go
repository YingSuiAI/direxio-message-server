// Package agent owns Native Agent runtime actions and their MCP tool bridge.
package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/sirupsen/logrus"
)

// Runner is the stable Native Agent runtime boundary exposed by p2p.Config.
type Runner interface {
	Apply(context.Context, string) error
	Invoke(context.Context, string, map[string]any) (map[string]any, error)
	Stream(context.Context, string, map[string]any, func(nativeagent.Event) error) error
}

type ConversationStateReader interface {
	GetConversationState(context.Context, string) (int64, bool, error)
}

// Config contains the runtime dependencies owned outside the Agent module.
type Config struct {
	Runner Runner
	// ChatRunner optionally moves only Chat/StreamChat to an isolated Agent
	// service while the remaining runtime/config/Skill/Knowledge actions stay
	// on Runner until their public contracts are migrated.
	ChatRunner        Runner
	RuntimeProfiles   RuntimeProfileClient
	SearchProfiles    SearchProfileClient
	ConversationState ConversationStateReader
	DataDir           string
	Store             nativeagent.ConfigStore
	MCP               *dirextalkmcp.Service
	Account           AccountPort
	Turns             agentturns.Store
	OwnerID           func() string
}

// Module owns runtime-backed ProductCore actions and streaming invocation.
type Module struct {
	runner            Runner
	chatRunner        Runner
	runtimeProfiles   RuntimeProfileClient
	searchProfiles    SearchProfileClient
	conversationState ConversationStateReader
	account           AccountPort
	turns             *agentturns.Coordinator
	turnErr           error
	ownerID           func() string
}

func New(cfg Config) *Module {
	runner := cfg.Runner
	if runner == nil {
		runner = runtimeRunner{runtime: nativeagent.New(nativeagent.Config{
			DataDir: cfg.DataDir,
			Store:   cfg.Store,
			Tools:   Tools(cfg.MCP),
		})}
	}
	chatRunner := cfg.ChatRunner
	if chatRunner == nil {
		chatRunner = runner
	}
	turns, turnErr := agentturns.NewCoordinator(context.Background(), cfg.Turns)
	return &Module{
		runner: runner, chatRunner: chatRunner, runtimeProfiles: cfg.RuntimeProfiles, searchProfiles: cfg.SearchProfiles,
		conversationState: cfg.ConversationState, account: cfg.Account,
		turns: turns, turnErr: turnErr, ownerID: cfg.OwnerID,
	}
}

// Handlers returns the complete Agent ProductCore action surface.
func (m *Module) Handlers() map[string]actionbase.Handler {
	handlers := make(map[string]actionbase.Handler, len(runtimeActions)+len(remoteAgentActions)+11)
	for _, action := range runtimeActions {
		handlers[action] = m.invoke(action)
	}
	for _, action := range remoteAgentActions {
		handlers[action] = m.invoke(action)
	}
	handlers[actionPassword] = m.accountPassword
	handlers[actionMatrixSessionCreate] = m.createMatrixSession
	handlers[actionConfigGet] = m.getConfig
	handlers[actionConfigUpdate] = m.updateConfig
	handlers[actionRuntimeProfileGet] = m.getRuntimeProfile
	handlers[actionRuntimeProfileUpdate] = m.updateRuntimeProfile
	handlers[actionSearchProfileGet] = m.getSearchProfile
	handlers[actionSearchProfileUpdate] = m.updateSearchProfile
	handlers["agent.chat.stream"] = streamOnly
	handlers["agent.chat.turn.stop"] = m.stopTurn
	handlers["agent.chat.turns.list"] = m.listTurns
	return handlers
}

func (m *Module) ReadyError() error {
	if m == nil {
		return fmt.Errorf("native agent module is unavailable")
	}
	return m.turnErr
}

// Stream invokes a runtime streaming action after the websocket adapter has
// established its connection-scoped cancellation and frame writer.
func (m *Module) Stream(ctx context.Context, action string, params map[string]any, emit func(nativeagent.Event) error) error {
	if m == nil || m.chatRunner == nil {
		return fmt.Errorf("native agent runtime is not configured")
	}
	return m.chatRunner.Stream(ctx, strings.TrimSpace(action), cloneMap(params), emit)
}

func (m *Module) DurableStream(ctx context.Context, ownerID, action string, params map[string]any, emit func(agentturns.StreamEvent) error) error {
	if m == nil || m.chatRunner == nil || m.turns == nil {
		return fmt.Errorf("native agent turn coordinator is not configured")
	}
	turnID := actionbase.String(params["turn_id"])
	conversationID := nativeagent.ConversationID(params)
	digest, err := agentturns.RequestDigest(action, params)
	if err != nil {
		return err
	}
	request := agentturns.Request{
		OwnerID: strings.TrimSpace(ownerID), TurnID: turnID, ConversationID: conversationID,
		Action: strings.TrimSpace(action), Digest: digest, AfterSeq: actionbase.Int64(params["after_seq"]),
	}
	runParams := cloneMap(params)
	delete(runParams, "turn_id")
	delete(runParams, "after_seq")
	return m.turns.Stream(ctx, request, func(runCtx context.Context, runtimeEmit func(agentturns.RuntimeEvent) error) error {
		authoritativeRevision := int64(0)
		if m.conversationState != nil {
			revision, found, stateErr := m.conversationState.GetConversationState(
				runCtx,
				request.ConversationID,
			)
			if stateErr != nil {
				return fmt.Errorf("read authoritative agent conversation revision: %w", stateErr)
			}
			if found {
				authoritativeRevision = revision
			}
		}
		latestRevision, revisionErr := m.turns.LatestConversationRevision(runCtx, request.OwnerID, request.ConversationID)
		if revisionErr != nil {
			return fmt.Errorf("reconcile agent conversation revision: %w", revisionErr)
		}
		requestedRevision := actionbase.Int64(runParams["expected_conversation_revision"])
		if requestedRevision >= 0 {
			reconciledRevision := maxConversationRevision(
				requestedRevision,
				latestRevision,
				authoritativeRevision,
			)
			if reconciledRevision != requestedRevision {
				runParams["expected_conversation_revision"] = reconciledRevision
			}
		}
		return m.chatRunner.Stream(runCtx, request.Action, runParams, func(event nativeagent.Event) error {
			return runtimeEmit(agentturns.RuntimeEvent{Event: event.Event, Data: event.Data})
		})
	}, emit)
}

func maxConversationRevision(values ...int64) int64 {
	maximum := int64(0)
	for _, value := range values {
		if value > maximum {
			maximum = value
		}
	}
	return maximum
}

func (m *Module) stopTurn(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.turns == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "native agent turn coordinator is not configured")
	}
	turnID := actionbase.String(params["turn_id"])
	if turnID == "" {
		return nil, actionbase.BadRequest("turn_id is required")
	}
	if !agentturns.ValidID(turnID) {
		return nil, actionbase.BadRequest("turn_id is invalid")
	}
	turn, changed, err := m.turns.Stop(ctx, m.currentOwnerID(), turnID)
	if err != nil {
		if errors.Is(err, agentturns.ErrTurnNotFound) {
			return nil, actionbase.CodedError(http.StatusNotFound, "M_TURN_NOT_FOUND", "M_TURN_NOT_FOUND")
		}
		return nil, actionbase.InternalError(err)
	}
	result := turnResponse(turn)
	result["changed"] = changed
	return result, nil
}

func (m *Module) listTurns(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.turns == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "native agent turn coordinator is not configured")
	}
	conversationID := ""
	if actionbase.String(params["conversation_id"]) != "" {
		conversationID = nativeagent.ConversationID(params)
	}
	turns, err := m.turns.List(ctx, m.currentOwnerID(), conversationID, int(actionbase.Int64(params["limit"])))
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	items := make([]map[string]any, 0, len(turns))
	for _, turn := range turns {
		items = append(items, turnResponse(turn))
	}
	return map[string]any{"turns": items}, nil
}

func (m *Module) currentOwnerID() string {
	if m != nil && m.ownerID != nil {
		return strings.TrimSpace(m.ownerID())
	}
	return "owner"
}

func turnResponse(turn agentturns.Turn) map[string]any {
	result := map[string]any{
		"turn_id": turn.TurnID, "conversation_id": turn.ConversationID, "action": turn.Action,
		"state": string(turn.State), "created_at": turn.CreatedAt.UTC().Format(time.RFC3339Nano),
		"updated_at": turn.UpdatedAt.UTC().Format(time.RFC3339Nano),
	}
	if turn.Error != "" {
		result["error"] = turn.Error
	}
	return result
}

func (m *Module) invoke(action string) actionbase.Handler {
	return func(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
		runner := m.runnerForAction(action)
		if runner == nil {
			return nil, actionbase.StatusError(http.StatusBadGateway, "native agent runtime is not configured")
		}
		result, err := runner.Invoke(ctx, strings.TrimSpace(action), cloneMap(params))
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"action": action,
				"error":  err,
			}).Warn("Native Agent action failed")
			var coded interface{ ErrorCode() string }
			if errors.As(err, &coded) {
				code := strings.TrimSpace(coded.ErrorCode())
				if code != "" {
					return nil, actionbase.CodedError(
						runnerErrorStatus(code),
						code,
						err.Error(),
					)
				}
			}
			return nil, actionbase.StatusError(http.StatusBadGateway, err.Error())
		}
		return result, nil
	}
}

func runnerErrorStatus(code string) int {
	if strings.TrimSpace(code) == "M_AGENT_MODEL_CREDENTIAL_INVALID" {
		return http.StatusBadRequest
	}
	return http.StatusBadGateway
}

func (m *Module) runnerForAction(action string) Runner {
	if m == nil {
		return nil
	}
	action = strings.TrimSpace(action)
	if action == "agent.chat" ||
		action == "agent.models.list" ||
		strings.HasPrefix(action, "agent.cloud.") ||
		strings.HasPrefix(action, "agent.team.") {
		return m.chatRunner
	}
	return m.runner
}

func streamOnly(context.Context, map[string]any) (any, *actionbase.Error) {
	return nil, actionbase.BadRequest("action requires websocket")
}

type runtimeRunner struct {
	runtime *nativeagent.Runtime
}

func (r runtimeRunner) Apply(ctx context.Context, action string) error {
	if r.runtime == nil {
		return fmt.Errorf("native agent runtime is not configured")
	}
	return r.runtime.Apply(ctx, strings.TrimSpace(action))
}

func (r runtimeRunner) Invoke(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	if r.runtime == nil {
		return nil, fmt.Errorf("native agent runtime is not configured")
	}
	return r.runtime.Invoke(ctx, strings.TrimSpace(action), cloneMap(params))
}

func (r runtimeRunner) Stream(ctx context.Context, action string, params map[string]any, emit func(nativeagent.Event) error) error {
	if r.runtime == nil {
		return fmt.Errorf("native agent runtime is not configured")
	}
	return r.runtime.Stream(ctx, strings.TrimSpace(action), cloneMap(params), emit)
}

var runtimeActions = []string{
	"agent.config.propose_patch",
	"agent.chat",
	"agent.context.compress",
	"agent.models.list",
	"agent.runtime.inspect",
	"agent.runtime.install",
	"agent.runtime.which",
	"agent.runtime.run",
	"agent.skills.list",
	"agent.skills.install",
	"agent.skills.enable",
	"agent.skills.disable",
	"agent.skills.uninstall",
	"agent.skills.registry.search",
	"agent.mcp.servers.list",
	"agent.mcp.servers.install",
	"agent.mcp.servers.enable",
	"agent.mcp.servers.disable",
	"agent.mcp.servers.uninstall",
	"agent.mcp.registry.search",
	"agent.knowledge.config.get",
	"agent.knowledge.config.update",
	"agent.knowledge.sources.list",
	"agent.knowledge.sources.delete",
	"agent.knowledge.upload.start",
	"agent.knowledge.upload.chunk",
	"agent.knowledge.upload.finish",
	"agent.knowledge.memory.create",
	"agent.knowledge.search",
	"agent.knowledge.status",
	"agent.contacts.list",
	"agent.contacts.search",
	"agent.rooms.search",
	"agent.messages.list",
	"agent.messages.send",
	"agent.room_members.list",
	"agent.channel_posts.list",
	"agent.channel_comments.list",
	"agent.channel_comments.create",
	"agent.summarize",
}

var remoteAgentActions = []string{
	"agent.cloud.tasks.list",
	"agent.cloud.tasks.overview",
	"agent.cloud.tasks.get",
	"agent.cloud.tasks.cancel",
	"agent.cloud.plans.list",
	"agent.cloud.plans.get",
	"agent.cloud.plans.confirmation.prepare",
	"agent.cloud.plans.approve",
	"agent.cloud.deployments.list",
	"agent.cloud.deployments.get",
	"agent.cloud.workers.list",
	"agent.cloud.workers.get",
	"agent.team.plans.get",
	"agent.team.approval_device.bootstrap",
	"agent.team.plans.approval.prepare",
	"agent.team.plans.approve",
	"agent.team.executions.get",
}
