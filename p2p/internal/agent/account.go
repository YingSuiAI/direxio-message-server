package agent

import (
	"context"
	"net/http"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
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

func (m *Module) getConfig(context.Context, map[string]any) (any, *actionbase.Error) {
	account, err := m.accountPort()
	if err != nil {
		return nil, err
	}
	return configResponse(account.Config()), nil
}

func (m *Module) updateConfig(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	account, err := m.accountPort()
	if err != nil {
		return nil, err
	}

	disableAgent := false
	config, actionErr := account.UpdateConfig(ctx, func(current dirextalkdomain.AgentConfig) dirextalkdomain.AgentConfig {
		next := ApplyConfigUpdate(current, params)
		if _, ok := params["enabled"]; ok {
			disableAgent = !next.Enabled
		}
		return next
	})
	if actionErr != nil {
		return nil, actionErr
	}
	if disableAgent {
		if actionErr := account.PublishOffline(ctx); actionErr != nil {
			return nil, actionErr
		}
	}
	return configResponse(config), nil
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
			"display_name": config.NativeAgentIdentity.DisplayName,
			"avatar_url":   config.NativeAgentIdentity.AvatarURL,
		},
		"online_agent_identity": map[string]any{
			"display_name": config.OnlineAgentIdentity.DisplayName,
			"avatar_url":   config.OnlineAgentIdentity.AvatarURL,
		},
		"context_window":       config.ContextWindow,
		"enabled":              config.Enabled,
		"model":                config.Model,
		"system_prompt":        config.SystemPrompt,
		"mcp_blocked_room_ids": append([]string(nil), config.MCPBlockedRoomIDs...),
	}
}
