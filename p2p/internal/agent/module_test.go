package agent

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

type recordingMCPInvoker struct {
	action string
}

func (i *recordingMCPInvoker) InvokeCapability(_ context.Context, action string, _ map[string]any) (any, *dirextalkmcp.Error) {
	i.action = action
	return map[string]any{"action": action}, nil
}

func TestRuntimeActionsUseConfiguredMCPService(t *testing.T) {
	invoker := &recordingMCPInvoker{}
	module := New(Config{MCP: dirextalkmcp.NewService(invoker)})
	handlers := module.Handlers()

	for _, test := range []struct {
		action string
		want   string
	}{
		{"agent.contacts.list", dirextalkmcp.ActionContactsList},
		{"agent.contacts.search", dirextalkmcp.ActionContactsSearch},
		{"agent.rooms.search", dirextalkmcp.ActionRoomsSearch},
		{"agent.messages.list", dirextalkmcp.ActionMessagesList},
		{"agent.messages.send", dirextalkmcp.ActionMessagesSend},
		{"agent.room_members.list", dirextalkmcp.ActionRoomMembersList},
		{"agent.channel_posts.list", dirextalkmcp.ActionChannelPostsList},
		{"agent.channel_comments.list", dirextalkmcp.ActionChannelCommentsList},
		{"agent.channel_comments.create", dirextalkmcp.ActionChannelCommentsCreate},
	} {
		t.Run(test.action, func(t *testing.T) {
			invoker.action = ""
			value, actionErr := handlers[test.action](context.Background(), map[string]any{})
			if actionErr != nil {
				t.Fatalf("invoke %s: %v", test.action, actionErr)
			}
			result := value.(map[string]any)
			if invoker.action != test.want || result["action"] != test.want {
				t.Fatalf("mapped to %q with result %#v, want %q", invoker.action, result, test.want)
			}
		})
	}
}

type recordingAccountPort struct {
	password      string
	session       MatrixSession
	sessionParams map[string]any
	config        dirextalkdomain.AgentConfig
	published     bool
	syncedOnline  []dirextalkdomain.AgentIdentityConfig
}

func (p *recordingAccountPort) Password() string { return p.password }

func (p *recordingAccountPort) CreateMatrixSession(_ context.Context, params map[string]any) (MatrixSession, *actionbase.Error) {
	p.sessionParams = cloneMap(params)
	return p.session, nil
}

func (p *recordingAccountPort) Config() dirextalkdomain.AgentConfig { return p.config }

func (p *recordingAccountPort) UpdateConfig(_ context.Context, mutate func(dirextalkdomain.AgentConfig) dirextalkdomain.AgentConfig) (dirextalkdomain.AgentConfig, *actionbase.Error) {
	p.config = mutate(p.config)
	return p.config, nil
}

func (p *recordingAccountPort) SyncOnlineIdentity(_ context.Context, identity dirextalkdomain.AgentIdentityConfig) *actionbase.Error {
	p.syncedOnline = append(p.syncedOnline, identity)
	return nil
}

func (p *recordingAccountPort) PublishOffline(context.Context) *actionbase.Error {
	p.published = true
	return nil
}

func TestAccountHandlersPreserveSessionAndConfigContracts(t *testing.T) {
	accessToken := "agent-access-token"
	account := &recordingAccountPort{
		password: "portal-password",
		session: MatrixSession{
			AccessToken: &accessToken,
			DeviceID:    "AGENT_DEVICE",
			UserID:      "@agent:example.com",
			Homeserver:  "https://example.com",
		},
		config: dirextalkdomain.AgentConfig{
			DisplayName:   "Agent",
			ContextWindow: 30,
			Enabled:       true,
			Native:        map[string]any{"api_key": "must-not-return"},
		},
	}
	module := New(Config{Account: account})
	handlers := module.Handlers()

	password, actionErr := handlers["agent.password"](context.Background(), nil)
	if actionErr != nil || password.(map[string]any)["password"] != "portal-password" {
		t.Fatalf("agent.password = %#v, %v", password, actionErr)
	}

	session, actionErr := handlers["agent.matrix_session.create"](context.Background(), map[string]any{"device_id": "AGENT_DEVICE"})
	if actionErr != nil {
		t.Fatalf("agent.matrix_session.create: %v", actionErr)
	}
	if got := session.(map[string]any); got["access_token"] != "agent-access-token" || got["device_id"] != "AGENT_DEVICE" || got["user_id"] != "@agent:example.com" || got["homeserver"] != "https://example.com" {
		t.Fatalf("unexpected agent Matrix session: %#v", got)
	}
	if account.sessionParams["device_id"] != "AGENT_DEVICE" {
		t.Fatalf("expected full session params to reach account port, got %#v", account.sessionParams)
	}
	account.session.AccessToken = nil
	session, actionErr = handlers["agent.matrix_session.create"](context.Background(), nil)
	if actionErr != nil {
		t.Fatalf("agent.matrix_session.create without issuer: %v", actionErr)
	}
	if accessToken, exists := session.(map[string]any)["access_token"]; !exists || accessToken != nil {
		t.Fatalf("unconfigured issuer must preserve null access_token, got %#v", session)
	}

	updated, actionErr := handlers["agent.config.update"](context.Background(), map[string]any{
		"display_name":         " Ops Agent ",
		"avatar_url":           "",
		"context_window":       float64(64),
		"enabled":              false,
		"model":                " local-model ",
		"system_prompt":        " concise ",
		"mcp_blocked_room_ids": []any{"!secret:example.com", " !secret:example.com ", ""},
	})
	if actionErr != nil {
		t.Fatalf("agent.config.update: %v", actionErr)
	}
	config := updated.(map[string]any)
	if config["display_name"] != "Ops Agent" || config["enabled"] != false || config["model"] != "local-model" || config["system_prompt"] != "concise" {
		t.Fatalf("unexpected public config: %#v", config)
	}
	nativeIdentity := config["native_agent_identity"].(map[string]any)
	onlineIdentity := config["online_agent_identity"].(map[string]any)
	if nativeIdentity["display_name"] != "Ops Agent" ||
		onlineIdentity["display_name"] != "Ops Agent" {
		t.Fatalf("legacy top-level update should sync mode identities: %#v", config)
	}
	if _, found := config["api_key"]; found || !account.published {
		t.Fatalf("config must stay sanitized and disabling must publish offline: %#v published=%v", config, account.published)
	}
	if len(account.syncedOnline) != 1 || account.syncedOnline[0].DisplayName != "Ops Agent" {
		t.Fatalf("legacy top-level identity update should sync online identity, got %#v", account.syncedOnline)
	}
	blocked := config["mcp_blocked_room_ids"].([]string)
	if len(blocked) != 1 || blocked[0] != "!secret:example.com" {
		t.Fatalf("blocked rooms = %#v", blocked)
	}

	updated, actionErr = handlers["agent.config.update"](context.Background(), map[string]any{
		"native_agent_identity": map[string]any{
			"display_name": "Ying Prime",
		},
		"online_agent_identity": map[string]any{
			"avatar_url": "mxc://example.com/online",
		},
	})
	if actionErr != nil {
		t.Fatalf("agent.config.update identities: %v", actionErr)
	}
	config = updated.(map[string]any)
	nativeIdentity = config["native_agent_identity"].(map[string]any)
	onlineIdentity = config["online_agent_identity"].(map[string]any)
	if nativeIdentity["display_name"] != "Ying Prime" ||
		nativeIdentity["avatar_url"] != "" ||
		onlineIdentity["display_name"] != "Ops Agent" ||
		onlineIdentity["avatar_url"] != "mxc://example.com/online" {
		t.Fatalf("nested identity updates should merge independently: %#v", config)
	}
	if len(account.syncedOnline) != 2 || account.syncedOnline[1].AvatarURL != "mxc://example.com/online" {
		t.Fatalf("online identity update should sync Matrix identity once, got %#v", account.syncedOnline)
	}
}
