package agent

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	agentdatav2 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/agent/data/v2"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/google/uuid"
)

const (
	actionPassword            = "agent.password"
	actionMatrixSessionCreate = "agent.matrix_session.create"
	actionSessionCreate       = "agent.session.create"
	actionConfigGet           = "agent.config.get"
	actionConfigUpdate        = "agent.config.update"
)

const (
	agentSessionAudience = "dirextalk-agent-data"
	agentSessionTTL      = 15 * time.Minute
)

// This is the explicit owner-client capability set advertised by the current
// Agent data plane. The values are generated from the shared OpenAPI contract
// and are intentionally not derived from client input.
var agentSessionScopes = []agentdatav2.AgentDataScope{
	agentdatav2.AgentDataScopeAgentExecutionV2,
	agentdatav2.AgentDataScopeAgentAccountDeprovision,
	agentdatav2.AgentDataScopeAgentAwsCredentialsRead,
	agentdatav2.AgentDataScopeAgentAwsCredentialsWrite,
	agentdatav2.AgentDataScopeAgentChatRead,
	agentdatav2.AgentDataScopeAgentChatWrite,
	agentdatav2.AgentDataScopeAgentConfigRead,
	agentdatav2.AgentDataScopeAgentConfigWrite,
	agentdatav2.AgentDataScopeAgentConfirmationsRead,
	agentdatav2.AgentDataScopeAgentConfirmationsWrite,
	agentdatav2.AgentDataScopeAgentImageToolsExecute,
	agentdatav2.AgentDataScopeAgentImageToolsUpload,
	agentdatav2.AgentDataScopeAgentInfoRead,
	agentdatav2.AgentDataScopeAgentKnowledgeRead,
	agentdatav2.AgentDataScopeAgentKnowledgeWrite,
	agentdatav2.AgentDataScopeAgentMcpExecute,
	agentdatav2.AgentDataScopeAgentMcpRead,
	agentdatav2.AgentDataScopeAgentMcpWrite,
	agentdatav2.AgentDataScopeAgentMemoryRead,
	agentdatav2.AgentDataScopeAgentMemoryWrite,
	agentdatav2.AgentDataScopeAgentModelsRead,
	agentdatav2.AgentDataScopeAgentModelsWrite,
	agentdatav2.AgentDataScopeAgentProductExecute,
	agentdatav2.AgentDataScopeAgentRuntimeRead,
	agentdatav2.AgentDataScopeAgentRuntimeWrite,
	agentdatav2.AgentDataScopeAgentSchedulesRead,
	agentdatav2.AgentDataScopeAgentSchedulesWrite,
	agentdatav2.AgentDataScopeAgentSkillsExecute,
	agentdatav2.AgentDataScopeAgentSkillsRead,
	agentdatav2.AgentDataScopeAgentSkillsWrite,
	agentdatav2.AgentDataScopeAgentStaticSitesRead,
	agentdatav2.AgentDataScopeAgentStaticSitesWrite,
	agentdatav2.AgentDataScopeAgentTasksRead,
	agentdatav2.AgentDataScopeAgentTasksWrite,
	agentdatav2.AgentDataScopeAgentTextToolsExecute,
	agentdatav2.AgentDataScopeAgentTextToolsRead,
	agentdatav2.AgentDataScopeAgentTextToolsWrite,
	agentdatav2.AgentDataScopeAgentVoiceWrite,
	agentdatav2.AgentDataScopeAgentWebSearchRead,
	agentdatav2.AgentDataScopeAgentWebSearchWrite,
	agentdatav2.AgentDataScopeAgentWorkerDestroy,
	agentdatav2.AgentDataScopeAgentWorkerRead,
}

type agentSessionClaims struct {
	Issuer            string   `json:"iss"`
	Audience          string   `json:"aud"`
	Subject           string   `json:"sub"`
	AccountGeneration int64    `json:"account_generation"`
	SessionID         string   `json:"session_id"`
	Nonce             string   `json:"nonce"`
	Scopes            []string `json:"scope"`
	IssuedAt          int64    `json:"iat"`
	ExpiresAt         int64    `json:"exp"`
}

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

type AccountPort interface {
	Password() string
	CreateMatrixSession(context.Context, map[string]any) (MatrixSession, *actionbase.Error)
	Config() dirextalkdomain.AgentConfig
	UpdateConfig(context.Context, func(dirextalkdomain.AgentConfig) dirextalkdomain.AgentConfig) (dirextalkdomain.AgentConfig, *actionbase.Error)
	SyncOnlineIdentity(context.Context, dirextalkdomain.AgentIdentityConfig) *actionbase.Error
	PublishOffline(context.Context) *actionbase.Error
}

func (m *Module) accountPort() (AccountPort, *actionbase.Error) {
	if m == nil || m.account == nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "agent account service is unavailable")
	}
	return m.account, nil
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

func (m *Module) getAccountConfig(_ context.Context, params map[string]any) (any, *actionbase.Error) {
	if len(params) != 0 {
		return nil, actionbase.BadRequest("agent.config.get accepts no parameters")
	}
	account, err := m.accountPort()
	if err != nil {
		return nil, err
	}
	return accountConfigResponse(account.Config()), nil
}

func (m *Module) updateAccountConfig(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	for key := range params {
		switch key {
		case configKeyOnlineAgentIdentity, "enabled", "mcp_blocked_room_ids":
		default:
			return nil, actionbase.BadRequest("agent.config.update contains an unsupported field")
		}
	}
	account, err := m.accountPort()
	if err != nil {
		return nil, err
	}
	config, updateErr := account.UpdateConfig(ctx, func(current dirextalkdomain.AgentConfig) dirextalkdomain.AgentConfig {
		return ApplyOnlineConfigUpdate(current, params)
	})
	if updateErr != nil {
		return nil, updateErr
	}
	if OnlineIdentityUpdateRequested(params) {
		if syncErr := account.SyncOnlineIdentity(ctx, OnlineAgentIdentity(config)); syncErr != nil {
			return nil, syncErr
		}
	}
	if enabled, ok := params["enabled"]; ok && enabled == false {
		if publishErr := account.PublishOffline(ctx); publishErr != nil {
			return nil, publishErr
		}
	}
	return accountConfigResponse(config), nil
}

func accountConfigResponse(config dirextalkdomain.AgentConfig) map[string]any {
	config = NormalizeConfig(config)
	return map[string]any{
		"online_agent_identity": map[string]any{
			"display_name": config.OnlineAgentIdentity.DisplayName,
			"avatar_url":   config.OnlineAgentIdentity.AvatarURL,
		},
		"enabled":              config.Enabled,
		"mcp_blocked_room_ids": append([]string(nil), config.MCPBlockedRoomIDs...),
	}
}

func (m *Module) createAgentSession(_ context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || len(m.ticketPrivateKey) != ed25519.PrivateKeySize || m.accountGeneration <= 0 {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "native agent session issuer is unavailable")
	}
	if len(params) > 1 {
		return nil, actionbase.BadRequest("agent.session.create accepts only session_id")
	}
	var sessionID uuid.UUID
	if raw, ok := params["session_id"]; ok {
		value, ok := raw.(string)
		if !ok || value != strings.TrimSpace(value) {
			return nil, actionbase.BadRequest("session_id must be a canonical UUID")
		}
		parsed, err := uuid.Parse(value)
		if err != nil || parsed == uuid.Nil || parsed.String() != value {
			return nil, actionbase.BadRequest("session_id must be a canonical UUID")
		}
		sessionID = parsed
	} else {
		sessionID = uuid.New()
	}
	ownerID := m.currentOwnerID()
	if ownerID == "" || ownerID == "owner" {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "native agent session owner is unavailable")
	}
	now := m.now().UTC().Truncate(time.Second)
	expiresAt := now.Add(agentSessionTTL)
	scopes := append([]agentdatav2.AgentDataScope(nil), agentSessionScopes...)
	slices.Sort(scopes)
	claimScopes := make([]string, len(scopes))
	for index, scope := range scopes {
		claimScopes[index] = string(scope)
	}
	claims := agentSessionClaims{
		Issuer: "dirextalk-message-server", Audience: agentSessionAudience,
		Subject: ownerID, AccountGeneration: m.accountGeneration,
		SessionID: sessionID.String(), Nonce: uuid.NewString(), Scopes: claimScopes,
		IssuedAt: now.Unix(), ExpiresAt: expiresAt.Unix(),
	}
	ticket, err := signAgentSessionTicket(claims, m.ticketPrivateKey)
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	return agentdatav2.AgentSessionResponse{
		Ticket: ticket, ExpiresAt: expiresAt, ServerTime: now,
		BasePath:  agentdatav2.AgentSessionResponseBasePathAgentv1,
		SessionId: sessionID, Scopes: scopes,
	}, nil
}

func signAgentSessionTicket(claims agentSessionClaims, key ed25519.PrivateKey) (string, error) {
	headerJSON := []byte(`{"alg":"EdDSA","typ":"JWT"}`)
	payloadJSON, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("marshal native agent session claims: %w", err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	signingInput := encode(headerJSON) + "." + encode(payloadJSON)
	signature := ed25519.Sign(key, []byte(signingInput))
	return signingInput + "." + encode(signature), nil
}
