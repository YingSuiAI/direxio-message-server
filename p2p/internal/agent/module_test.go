package agent

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
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

func TestDedicatedChatRunnerDoesNotTakeOverOtherRuntimeActions(t *testing.T) {
	local := &recordingRunner{result: map[string]any{"source": "local"}}
	remote := &recordingRunner{result: map[string]any{"source": "remote"}}
	module := New(Config{Runner: local, ChatRunner: remote})

	chat, actionErr := module.Handlers()["agent.chat"](context.Background(), map[string]any{"prompt": "hello"})
	if actionErr != nil || chat.(map[string]any)["source"] != "remote" {
		t.Fatalf("agent.chat = %#v, %v", chat, actionErr)
	}
	models, actionErr := module.Handlers()["agent.models.list"](context.Background(), nil)
	if actionErr != nil || models.(map[string]any)["source"] != "local" {
		t.Fatalf("agent.models.list = %#v, %v", models, actionErr)
	}
	if len(remote.invokes) != 1 || remote.invokes[0] != "agent.chat" || len(local.invokes) != 1 || local.invokes[0] != "agent.models.list" {
		t.Fatalf("runner routing local=%v remote=%v", local.invokes, remote.invokes)
	}

	if err := module.Stream(context.Background(), "agent.chat.stream", map[string]any{"prompt": "hello"}, func(nativeagent.Event) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if len(remote.streams) != 1 || remote.streams[0] != "agent.chat.stream" || len(local.streams) != 0 {
		t.Fatalf("stream routing local=%v remote=%v", local.streams, remote.streams)
	}
}

type recordingRunner struct {
	result  map[string]any
	invokes []string
	streams []string
}

func (*recordingRunner) Apply(context.Context, string) error { return nil }

func (runner *recordingRunner) Invoke(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	runner.invokes = append(runner.invokes, action)
	return runner.result, nil
}

func (runner *recordingRunner) Stream(_ context.Context, action string, _ map[string]any, emit func(nativeagent.Event) error) error {
	runner.streams = append(runner.streams, action)
	return emit(nativeagent.Event{Event: "done", Data: runner.result})
}

type recordingAccountPort struct {
	password      string
	session       MatrixSession
	sessionParams map[string]any
	config        dirextalkdomain.AgentConfig
	published     bool
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
	if _, found := config["api_key"]; found || !account.published {
		t.Fatalf("config must stay sanitized and disabling must publish offline: %#v published=%v", config, account.published)
	}
	blocked := config["mcp_blocked_room_ids"].([]string)
	if len(blocked) != 1 || blocked[0] != "!secret:example.com" {
		t.Fatalf("blocked rooms = %#v", blocked)
	}
}
