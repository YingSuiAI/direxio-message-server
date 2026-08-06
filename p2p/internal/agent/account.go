package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/google/uuid"
)

const (
	actionPassword            = "agent.password"
	actionMatrixSessionCreate = "agent.matrix_session.create"
	actionConfigGet           = "agent.config.get"
	actionConfigUpdate        = "agent.config.update"
)

// MatrixSession is the public, non-persistent Agent Matrix session response.
// A nil AccessToken preserves the historic JSON null for an unconfigured
// issuer rather than changing the response shape.
type MatrixSession struct {
	AccessToken *string
	DeviceID    string
	UserID      string
	Homeserver  string
}

func (s MatrixSession) Response() map[string]any {
	var accessToken any
	if s.AccessToken != nil {
		accessToken = *s.AccessToken
	}
	return map[string]any{
		"access_token": accessToken,
		"device_id":    s.DeviceID,
		"user_id":      s.UserID,
		"homeserver":   s.Homeserver,
	}
}

// AccountPort is the narrow Service boundary for durable Agent account state
// and Matrix side effects. ProductCore parameter and response logic belongs to
// this module; the root Service remains the owner of locks and infrastructure.
type AccountPort interface {
	Password() string
	CreateMatrixSession(context.Context, map[string]any) (MatrixSession, *actionbase.Error)
	Config() dirextalkdomain.AgentConfig
	UpdateConfig(context.Context, func(dirextalkdomain.AgentConfig) dirextalkdomain.AgentConfig) (dirextalkdomain.AgentConfig, *actionbase.Error)
	PublishOffline(context.Context) *actionbase.Error
}

func (m *Module) accountPassword(context.Context, map[string]any) (any, *actionbase.Error) {
	account, err := m.accountPort()
	if err != nil {
		return nil, err
	}
	return map[string]any{"password": account.Password()}, nil
}

func (m *Module) createMatrixSession(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	account, err := m.accountPort()
	if err != nil {
		return nil, err
	}
	session, actionErr := account.CreateMatrixSession(ctx, params)
	if actionErr != nil {
		return nil, actionErr
	}
	return session.Response(), nil
}

func (m *Module) getConfig(ctx context.Context, _ map[string]any) (any, *actionbase.Error) {
	account, err := m.accountPort()
	if err != nil {
		return nil, err
	}
	if m == nil || m.runner == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "external native agent runtime is not configured")
	}
	remote, invokeErr := m.runner.Invoke(ctx, actionConfigGet, map[string]any{})
	if invokeErr != nil {
		return nil, configGatewayError(invokeErr)
	}
	return configResponseWithNative(account.Config(), remote), nil
}

func (m *Module) updateConfig(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	account, err := m.accountPort()
	if err != nil {
		return nil, err
	}
	return m.updateExternalConfig(ctx, account, params)
}

// updateExternalConfig keeps the public ProductCore config action stable while
// splitting ownership: Native fields are committed by dirextalk-agent, and
// Online identity is committed by message-server only after that remote write
// succeeds. A failed Matrix sync therefore leaves a retryable remote config,
// never a silently divergent local Native projection.
func (m *Module) updateExternalConfig(ctx context.Context, account AccountPort, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.runner == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "external native agent runtime is not configured")
	}
	params = cloneMap(params)
	nativeParams := nativeConfigUpdateParams(params)
	operationID := configOperationID(m.currentOwnerID(), params, nativeParams)
	var remote map[string]any
	if hasNativeConfigUpdate(params) {
		nativeParams["operation_id"] = operationID
		nativeParams["idempotency_key"] = operationID
		var invokeErr error
		remote, invokeErr = m.runner.Invoke(ctx, actionConfigUpdate, nativeParams)
		if invokeErr != nil {
			return nil, configGatewayError(invokeErr)
		}
	} else {
		var invokeErr error
		remote, invokeErr = m.runner.Invoke(ctx, actionConfigGet, map[string]any{})
		if invokeErr != nil {
			return nil, configGatewayError(invokeErr)
		}
	}

	config := account.Config()
	if OnlineIdentityUpdateRequested(params) || hasMCPBlockedRoomUpdate(params) {
		var actionErr *actionbase.Error
		config, actionErr = account.UpdateConfig(ctx, func(current dirextalkdomain.AgentConfig) dirextalkdomain.AgentConfig {
			return ApplyOnlineConfigUpdate(current, params)
		})
		if actionErr != nil {
			return nil, actionErr
		}
		if OnlineIdentityUpdateRequested(params) {
			syncer, ok := account.(interface {
				SyncOnlineIdentity(context.Context, dirextalkdomain.AgentIdentityConfig) *actionbase.Error
			})
			if ok {
				if actionErr := syncer.SyncOnlineIdentity(ctx, OnlineAgentIdentity(config)); actionErr != nil {
					return nil, actionErr
				}
			}
		}
	}
	if enabled, ok := params["enabled"]; ok && !actionbase.Bool(enabled) {
		if actionErr := account.PublishOffline(ctx); actionErr != nil {
			return nil, actionErr
		}
	}
	return configResponseWithNative(config, remote), nil
}

func hasMCPBlockedRoomUpdate(params map[string]any) bool {
	_, ok := params["mcp_blocked_room_ids"]
	return ok
}

func hasNativeConfigUpdate(params map[string]any) bool {
	for _, key := range []string{"display_name", "avatar_url", "native_agent_identity", "context_window", "enabled", "model", "system_prompt", "mcp_blocked_room_ids"} {
		if _, ok := params[key]; ok {
			return true
		}
	}
	return false
}

func nativeConfigUpdateParams(params map[string]any) map[string]any {
	out := make(map[string]any)
	for _, key := range []string{"display_name", "avatar_url", "context_window", "enabled", "model", "system_prompt", "mcp_blocked_room_ids", "expected_revision"} {
		if value, ok := params[key]; ok {
			out[key] = value
		}
	}
	// Agent Core owns only the Native identity and retains the legacy flat
	// config shape. The mode-specific ProductCore field is translated here and
	// never forwarded as a Message Server-specific nested object.
	if identity, ok := params[configKeyNativeAgentIdentity].(map[string]any); ok {
		values := actionbase.Params(identity)
		if displayName := values.String(configKeyDisplayName); displayName != "" {
			out[configKeyDisplayName] = displayName
		}
		if avatarURL := values.String(configKeyAvatarURL); avatarURL != "" {
			out[configKeyAvatarURL] = avatarURL
		}
	}
	return out
}

func configOperationID(owner string, original, native map[string]any) string {
	if value, ok := original["operation_id"].(string); ok {
		if parsed, err := uuid.Parse(strings.TrimSpace(value)); err == nil {
			return parsed.String()
		}
	}
	canonical, err := json.Marshal(map[string]any{"owner_id": strings.TrimSpace(owner), "config": native})
	if err == nil {
		return uuid.NewSHA1(uuid.NameSpaceOID, canonical).String()
	}
	return uuid.New().String()
}

func configGatewayError(err error) *actionbase.Error {
	if errors.Is(err, agentgateway.ErrUnsupportedAction) {
		return actionbase.StatusError(http.StatusNotImplemented, err.Error())
	}
	message := strings.ToLower(strings.TrimSpace(err.Error()))
	switch {
	case strings.Contains(message, "conflict"):
		return actionbase.StatusError(http.StatusConflict, err.Error())
	case strings.Contains(message, "invalid") || strings.Contains(message, "required"):
		return actionbase.BadRequest(err.Error())
	default:
		return actionbase.StatusError(http.StatusBadGateway, err.Error())
	}
}

func (m *Module) accountPort() (AccountPort, *actionbase.Error) {
	if m == nil || m.account == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "agent account service is not configured")
	}
	return m.account, nil
}

func configResponse(config dirextalkdomain.AgentConfig) map[string]any {
	config = NormalizeConfig(config)
	return map[string]any{
		"display_name": config.DisplayName,
		"avatar_url":   config.AvatarURL,
		"native_agent_identity": map[string]any{
			"display_name": NativeAgentIdentity(config).DisplayName,
			"avatar_url":   NativeAgentIdentity(config).AvatarURL,
		},
		"online_agent_identity": map[string]any{
			"display_name": OnlineAgentIdentity(config).DisplayName,
			"avatar_url":   OnlineAgentIdentity(config).AvatarURL,
		},
		"context_window":       config.ContextWindow,
		"enabled":              config.Enabled,
		"model":                config.Model,
		"system_prompt":        config.SystemPrompt,
		"mcp_blocked_room_ids": append([]string(nil), config.MCPBlockedRoomIDs...),
	}
}

func configResponseWithNative(config dirextalkdomain.AgentConfig, remote map[string]any) map[string]any {
	response := configResponse(config)
	if remote == nil {
		return response
	}
	for _, key := range []string{"revision", "display_name", "avatar_url", "context_window", "enabled", "model", "system_prompt", "mcp_blocked_room_ids"} {
		if value, ok := remote[key]; ok {
			response[key] = value
		}
	}
	if identity, ok := remote["native_agent_identity"].(map[string]any); ok {
		response["native_agent_identity"] = identity
	} else {
		identity := response["native_agent_identity"].(map[string]any)
		if displayName := actionbase.String(remote[configKeyDisplayName]); displayName != "" {
			identity[configKeyDisplayName] = displayName
		}
		if avatarURL := actionbase.String(remote[configKeyAvatarURL]); avatarURL != "" {
			identity[configKeyAvatarURL] = avatarURL
		}
	}
	// The top-level aliases are Native Agent fields. Online identity remains
	// sourced from message-server's account record and is never overwritten by
	// an Agent response.
	return response
}
