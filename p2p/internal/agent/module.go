// Package agent owns Native Agent runtime actions and their MCP tool bridge.
package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

// Runner is the stable Native Agent runtime boundary exposed by p2p.Config.
type Runner interface {
	Apply(context.Context, string) error
	Invoke(context.Context, string, map[string]any) (map[string]any, error)
	Stream(context.Context, string, map[string]any, func(nativeagent.Event) error) error
}

// Config contains the runtime dependencies owned outside the Agent module.
type Config struct {
	Runner                Runner
	DataDir               string
	Store                 nativeagent.ConfigStore
	MCP                   *dirextalkmcp.Service
	Account               AccountPort
	Turns                 agentturns.Store
	OwnerID               func() string
	Memory                nativeagent.ConversationMemoryStore
	PersistentMemoryReady bool
	ModelProfiles         storage.ModelProfileStore
	ModelProfileResolver  nativeagent.ModelProfileResolver
	VoiceEnabled          bool
	VoiceActive           func(string) bool
	VoiceGeneration       func() uint64
	ScheduleTools         []nativeagent.Tool
}

// Module owns runtime-backed ProductCore actions and streaming invocation.
type Module struct {
	runner        Runner
	account       AccountPort
	turns         *agentturns.Coordinator
	turnErr       error
	ownerID       func() string
	modelProfiles storage.ModelProfileStore
	voice         *voiceCoordinator
}

type pinnedProfileContextKey struct{}
type pinnedProfileContext struct{ revision, credential int64 }

func New(cfg Config) *Module {
	runner := cfg.Runner
	if runner == nil {
		runner = runtimeRunner{runtime: nativeagent.New(nativeagent.Config{
			DataDir:               cfg.DataDir,
			Store:                 cfg.Store,
			Tools:                 append(Tools(cfg.MCP), cfg.ScheduleTools...),
			ModelProfiles:         cfg.ModelProfileResolver,
			OwnerID:               cfg.OwnerID,
			Memory:                cfg.Memory,
			PersistentMemoryReady: cfg.PersistentMemoryReady,
			EmbeddingSession:      embeddingForStore(cfg.ModelProfiles, cfg.OwnerID, nil),
		})}
	}
	turns, turnErr := agentturns.NewCoordinator(context.Background(), cfg.Turns)
	module := &Module{runner: runner, account: cfg.Account, turns: turns, turnErr: turnErr, ownerID: cfg.OwnerID, modelProfiles: cfg.ModelProfiles}
	if cfg.VoiceEnabled {
		module.voice = newVoiceCoordinator(cfg.ModelProfiles, cfg.OwnerID)
		module.voice.active = cfg.VoiceActive
		module.voice.generation = cfg.VoiceGeneration
		module.voice.durable = func(ctx context.Context, owner string, params map[string]any, emit func(agentturns.StreamEvent) error) error {
			revision, credential := actionbase.Int64(params["_voice_revision"]), actionbase.Int64(params["_voice_credential"])
			delete(params, "_voice_revision")
			delete(params, "_voice_credential")
			return module.durablePinnedStream(ctx, owner, "agent.chat.stream", params, revision, credential, emit)
		}
		module.voice.stop = func(ctx context.Context, owner, turnID string) error {
			if module.turns == nil {
				return fmt.Errorf("native agent turn coordinator is not configured")
			}
			_, _, err := module.turns.Stop(ctx, owner, turnID)
			return err
		}
	}
	return module
}

// Handlers returns the complete Agent ProductCore action surface.
func (m *Module) Handlers() map[string]actionbase.Handler {
	handlers := make(map[string]actionbase.Handler, len(runtimeActions)+11)
	for _, action := range runtimeActions {
		handlers[action] = m.invoke(action)
	}
	handlers[actionPassword] = m.accountPassword
	handlers[actionMatrixSessionCreate] = m.createMatrixSession
	handlers[actionConfigGet] = m.getConfig
	handlers[actionConfigUpdate] = m.updateConfig
	handlers["agent.chat.stream"] = streamOnly
	if m.voice != nil {
		handlers["agent.voice.session.stream"] = streamOnly
	}
	handlers["agent.chat.turn.stop"] = m.stopTurn
	handlers["agent.chat.turns.list"] = m.listTurns
	handlers["agent.model_profiles.sync"] = m.modelProfileSync
	handlers["agent.model_profiles.list"] = m.modelProfileList
	handlers["agent.model_profiles.get"] = m.modelProfileGet
	handlers["agent.model_profiles.delete"] = m.modelProfileDelete
	if m.voice != nil {
		handlers["agent.voice.session.create"] = m.createVoiceSession
		handlers["agent.voice.session.start"] = m.startVoiceSession
		handlers["agent.voice.session.transcript"] = m.submitVoiceTranscript
		handlers["agent.voice.session.interrupt"] = m.interruptVoiceSession
		handlers["agent.voice.session.end"] = m.endVoiceSession
	}
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
	if m == nil || m.runner == nil {
		return fmt.Errorf("native agent runtime is not configured")
	}
	if strings.TrimSpace(action) == "agent.voice.session.stream" && m.voice != nil {
		return m.voice.stream(ctx, m.currentOwnerID(), params, emit)
	}
	params = cloneMap(params)
	delete(params, "_server_pinned_profile")
	return m.runner.Stream(ctx, strings.TrimSpace(action), cloneMap(params), emit)
}

func (m *Module) DurableStream(ctx context.Context, ownerID, action string, params map[string]any, emit func(agentturns.StreamEvent) error) error {
	if m == nil || m.runner == nil || m.turns == nil {
		return fmt.Errorf("native agent turn coordinator is not configured")
	}
	params = cloneMap(params)
	delete(params, "_server_pinned_profile")
	turnID := actionbase.String(params["turn_id"])
	conversationID := nativeagent.ConversationID(params)
	profileID := durableServerModelProfileID(params)
	var existing agentturns.Turn
	var existingOK bool
	if turnID != "" {
		var getErr error
		existing, existingOK, getErr = m.turns.Get(context.Background(), ownerID, turnID)
		if getErr != nil {
			return getErr
		}
	}
	contentPresent := durableChatContentPresent(params)
	if existingOK && !contentPresent {
		if suppliedConversation := actionbase.String(params["conversation_id"]); suppliedConversation != "" && suppliedConversation != existing.ConversationID {
			return agentturns.ErrTurnIDReused
		}
		if suppliedAction := strings.TrimSpace(action); suppliedAction != "" && suppliedAction != existing.Action {
			return agentturns.ErrTurnIDReused
		}
		if suppliedProfile := durableServerModelProfileID(params); suppliedProfile != "" && suppliedProfile != existing.ModelProfileID {
			return agentturns.ErrTurnIDReused
		}
		conversationID = existing.ConversationID
		profileID = existing.ModelProfileID
		digest := existing.Digest
		request := agentturns.Request{OwnerID: strings.TrimSpace(ownerID), TurnID: turnID, ConversationID: conversationID, Action: existing.Action, ModelProfileID: profileID, ModelProfileRevision: existing.ModelProfileRevision, CredentialVersion: existing.CredentialVersion, Digest: digest, AfterSeq: actionbase.Int64(params["after_seq"])}
		return m.turns.Stream(ctx, request, func(context.Context, func(agentturns.RuntimeEvent) error) error { return nil }, emit)
	}
	if action == "agent.chat" || action == "agent.chat.stream" {
		if _, err := nativeagent.ValidateNativeAgentChatParams(params); err != nil {
			return err
		}
	}
	var pinnedRevision, pinnedCredential int64
	requestedRevision, requestedCredential := int64(0), int64(0)
	if pin, ok := ctx.Value(pinnedProfileContextKey{}).(pinnedProfileContext); ok {
		requestedRevision, requestedCredential = pin.revision, pin.credential
	}
	if profileID != "" && turnID != "" {
		if existing, ok, getErr := m.turns.Get(context.Background(), ownerID, turnID); getErr == nil && ok && existing.ModelProfileID == profileID {
			pinnedRevision, pinnedCredential = existing.ModelProfileRevision, existing.CredentialVersion
		}
	}
	if profileID != "" && (pinnedRevision <= 0 || pinnedCredential <= 0) {
		if m.modelProfiles == nil {
			return fmt.Errorf("server model profiles are unavailable")
		}
		var profile storage.ModelProfile
		var resolveErr error
		if requestedRevision > 0 {
			profile, resolveErr = m.modelProfiles.ResolveModelProfilePinned(ctx, strings.TrimSpace(ownerID), profileID, requestedRevision, requestedCredential)
		} else {
			profile, resolveErr = m.modelProfiles.ResolveModelProfile(ctx, strings.TrimSpace(ownerID), profileID)
		}
		if resolveErr != nil {
			return resolveErr
		}
		pinnedRevision, pinnedCredential = profile.Revision, profile.CredentialVersion
	}
	if action == "agent.chat" || action == "agent.chat.stream" {
		attachments, _ := nativeagent.ValidateNativeAgentChatParams(params)
		if len(attachments) > 0 {
			// The runtime repeats this fence; doing it here prevents a bad
			// request from reserving a durable turn or calculating its digest.
			profile, resolveErr := m.modelProfiles.ResolveModelProfilePinned(ctx, strings.TrimSpace(ownerID), profileID, pinnedRevision, pinnedCredential)
			if resolveErr != nil {
				return resolveErr
			}
			if profile.ModelKind != storage.ModelKindConversation || !containsImageModality(profile.InputModalities) {
				return fmt.Errorf("image attachments require a conversation model profile with image input")
			}
		}
	}
	digest, err := agentturns.RequestDigest(action, params)
	if err != nil {
		return err
	}
	request := agentturns.Request{
		OwnerID: strings.TrimSpace(ownerID), TurnID: turnID, ConversationID: conversationID,
		Action: strings.TrimSpace(action), ModelProfileID: profileID, ModelProfileRevision: pinnedRevision,
		CredentialVersion: pinnedCredential, Digest: digest, AfterSeq: actionbase.Int64(params["after_seq"]),
	}
	return m.turns.Stream(ctx, request, func(runCtx context.Context, runtimeEmit func(agentturns.RuntimeEvent) error) error {
		runParams := cloneMap(params)
		delete(runParams, "after_seq")
		delete(runParams, "model_profile_revision")
		delete(runParams, "credential_version")
		if request.ModelProfileID != "" {
			if m.modelProfiles == nil {
				return fmt.Errorf("server model profiles are unavailable")
			}
			profile, resolveErr := m.modelProfiles.ResolveModelProfilePinned(runCtx, request.OwnerID, request.ModelProfileID, request.ModelProfileRevision, request.CredentialVersion)
			if resolveErr != nil {
				return resolveErr
			}
			runParams["model_profile"] = durableModelProfileParams(profile)
			runParams["_server_pinned_profile"] = true
			delete(runParams, "model_profile_id")
			delete(runParams, "client_model_profile_id")
		}
		return m.runner.Stream(runCtx, request.Action, runParams, func(event nativeagent.Event) error {
			return runtimeEmit(agentturns.RuntimeEvent{Event: event.Event, Data: event.Data})
		})
	}, emit)
}

func (m *Module) durablePinnedStream(ctx context.Context, owner, action string, params map[string]any, revision, credential int64, emit func(agentturns.StreamEvent) error) error {
	ctx = context.WithValue(ctx, pinnedProfileContextKey{}, pinnedProfileContext{revision: revision, credential: credential})
	return m.DurableStream(ctx, owner, action, params, emit)
}

func durableServerModelProfileID(params map[string]any) string {
	if _, inline := params["model_profile"]; inline {
		return ""
	}
	serverID := actionbase.String(params["model_profile_id"])
	legacyID := actionbase.String(params["client_model_profile_id"])
	if serverID != "" && legacyID == "" {
		return serverID
	}
	if serverID == "" {
		return legacyID
	}
	if serverID == legacyID {
		return serverID
	}
	return ""
}

func durableModelProfileParams(profile storage.ModelProfile) map[string]any {
	params := map[string]any{
		"provider": profile.Provider, "base_url": profile.BaseURL, "model": profile.Model,
		"system_prompt": profile.SystemPrompt, "api_key": profile.APIKey,
		"max_output_tokens": profile.MaxOutputTokens, "context_window": profile.ContextWindow,
		"reasoning_mode": profile.ReasoningEffort, "model_kind": profile.ModelKind,
		"input_modalities": append([]string(nil), profile.InputModalities...),
	}
	if profile.Temperature != nil {
		params["temperature"] = *profile.Temperature
	}
	if profile.TopP != nil {
		params["top_p"] = *profile.TopP
	}
	return params
}

func durableChatContentPresent(params map[string]any) bool {
	for _, key := range []string{"prompt", "message", "messages", "attachments"} {
		value, ok := params[key]
		if !ok || value == nil {
			continue
		}
		if text, ok := value.(string); ok {
			if strings.TrimSpace(text) != "" {
				return true
			}
			continue
		}
		if key == "messages" || key == "attachments" {
			if reflect.ValueOf(value).Kind() == reflect.Slice && reflect.ValueOf(value).Len() > 0 {
				return true
			}
		}
	}
	return false
}

func containsImageModality(modalities []string) bool {
	for _, modality := range modalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			return true
		}
	}
	return false
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
		if m == nil || m.runner == nil {
			return nil, actionbase.StatusError(http.StatusBadGateway, "native agent runtime is not configured")
		}
		params = cloneMap(params)
		delete(params, "_server_pinned_profile")
		result, err := m.runner.Invoke(ctx, strings.TrimSpace(action), params)
		if err != nil {
			if errors.Is(err, agentmemory.ErrInvalidCursor) {
				return nil, actionbase.BadRequest(err.Error())
			}
			if errors.Is(err, agentmemory.ErrIdempotencyConflict) || errors.Is(err, agentmemory.ErrRevisionConflict) {
				return nil, actionbase.StatusError(http.StatusConflict, err.Error())
			}
			if errors.Is(err, agentmemory.ErrNotFound) || errors.Is(err, agentmemory.ErrDeleted) {
				return nil, actionbase.StatusError(http.StatusNotFound, err.Error())
			}
			if nativeagent.IsValidationError(err) {
				return nil, actionbase.BadRequest(err.Error())
			}
			if nativeagent.IsEmbeddedExtensionsForbidden(err) {
				return nil, actionbase.StatusError(http.StatusForbidden, err.Error())
			}
			return nil, actionbase.StatusError(http.StatusBadGateway, err.Error())
		}
		return result, nil
	}
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
	"agent.chat.conversations.create",
	"agent.chat.conversations.list",
	"agent.chat.conversations.get",
	"agent.chat.conversations.rename",
	"agent.chat.conversations.delete",
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
