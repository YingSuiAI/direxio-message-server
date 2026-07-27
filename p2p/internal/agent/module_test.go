package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
)

func TestClientCannotForgeServerPinnedImageProfile(t *testing.T) {
	runtime := nativeagent.New(nativeagent.Config{OwnerID: func() string { return "owner" }})
	module := New(Config{Runner: runtimeBackedRunner{runtime: runtime}})
	handler := module.Handlers()["agent.chat"]
	_, actionErr := handler(context.Background(), map[string]any{
		"prompt": "look", "_server_pinned_profile": true,
		"model_profile": map[string]any{"provider": "openai", "model": "gpt", "base_url": "https://example.invalid/v1", "api_key": "client-secret", "model_kind": "conversation", "input_modalities": []any{"text", "image"}},
		"attachments":   []any{map[string]any{"type": "image", "mime_type": "image/png", "data_base64": base64.StdEncoding.EncodeToString([]byte("x"))}},
	})
	if actionErr == nil || actionErr.Status != 400 {
		t.Fatalf("forged server profile status = %#v, want 400", actionErr)
	}
}

type failingConversationStore struct{}

func (failingConversationStore) CreateConversation(context.Context, string, string, string, string, [32]byte) (agentmemory.Conversation, bool, error) {
	return agentmemory.Conversation{}, false, errors.New("injected conversation store failure")
}
func (failingConversationStore) ListAgentConversations(context.Context, string, int, string) ([]agentmemory.Conversation, string, error) {
	return nil, "", errors.New("injected conversation store failure")
}
func (failingConversationStore) GetConversation(context.Context, string, string, int, string) (agentmemory.Conversation, []agentmemory.StoredMessage, string, error) {
	return agentmemory.Conversation{}, nil, "", errors.New("injected conversation store failure")
}
func (failingConversationStore) RenameConversation(context.Context, string, string, string, int64, string, [32]byte) (agentmemory.Conversation, bool, error) {
	return agentmemory.Conversation{}, false, errors.New("injected conversation store failure")
}
func (failingConversationStore) DeleteConversation(context.Context, string, string, int64, string, [32]byte) (agentmemory.Conversation, bool, error) {
	return agentmemory.Conversation{}, false, errors.New("injected conversation store failure")
}

type invalidCursorConversationStore struct{ failingConversationStore }

func (invalidCursorConversationStore) ListAgentConversations(context.Context, string, int, string) ([]agentmemory.Conversation, string, error) {
	return nil, "", agentmemory.ErrInvalidCursor
}
func (invalidCursorConversationStore) GetConversation(context.Context, string, string, int, string) (agentmemory.Conversation, []agentmemory.StoredMessage, string, error) {
	return agentmemory.Conversation{}, nil, "", agentmemory.ErrInvalidCursor
}

type invalidCursorKnowledgeStore struct{}

func (invalidCursorKnowledgeStore) CreateKnowledgeMemory(context.Context, string, string, string, []string, string, [32]byte) (agentmemory.KnowledgeMemory, bool, error) {
	return agentmemory.KnowledgeMemory{}, false, errors.New("unexpected knowledge create")
}
func (invalidCursorKnowledgeStore) SearchKnowledgeMemory(context.Context, string, string, int, string) ([]agentmemory.KnowledgeMemory, string, error) {
	return nil, "", agentmemory.ErrInvalidCursor
}
func (invalidCursorKnowledgeStore) KnowledgeStatus(context.Context, string) (int, error) {
	return 0, errors.New("unexpected knowledge status")
}

type runtimeBackedRunner struct{ runtime *nativeagent.Runtime }

func (r runtimeBackedRunner) Apply(ctx context.Context, action string) error {
	return r.runtime.Apply(ctx, action)
}
func (r runtimeBackedRunner) Invoke(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	return r.runtime.Invoke(ctx, action, params)
}
func (r runtimeBackedRunner) Stream(ctx context.Context, action string, params map[string]any, emit func(nativeagent.Event) error) error {
	return r.runtime.Stream(ctx, action, params, emit)
}

func TestConversationHandlerOnlyMapsValidationErrorsToBadRequest(t *testing.T) {
	invalid := New(Config{}).Handlers()["agent.chat.conversations.list"]
	if _, err := invalid(context.Background(), map[string]any{"page_size": "not-an-integer"}); err == nil || err.Status != 400 {
		t.Fatalf("invalid conversation param status = %#v, want 400", err)
	}
	failedRuntime := nativeagent.New(nativeagent.Config{OwnerID: func() string { return "owner" }, Conversations: failingConversationStore{}})
	failed := New(Config{Runner: runtimeBackedRunner{runtime: failedRuntime}}).Handlers()["agent.chat.conversations.list"]
	if _, err := failed(context.Background(), nil); err == nil || err.Status < 500 {
		t.Fatalf("store failure status = %#v, want 5xx", err)
	}
}

func TestAgentHandlersMapOnlyTypedInvalidCursorsToBadRequest(t *testing.T) {
	runtime := nativeagent.New(nativeagent.Config{
		OwnerID:       func() string { return "owner" },
		Conversations: invalidCursorConversationStore{},
		Knowledge:     invalidCursorKnowledgeStore{},
	})
	handlers := New(Config{Runner: runtimeBackedRunner{runtime: runtime}}).Handlers()
	for _, test := range []struct {
		action string
		params map[string]any
	}{
		{"agent.chat.conversations.list", map[string]any{"page_token": "malformed"}},
		{"agent.chat.conversations.get", map[string]any{"conversation_id": "conversation", "message_cursor": "0"}},
		{"agent.knowledge.search", map[string]any{"page_token": "malformed"}},
	} {
		if _, err := handlers[test.action](context.Background(), test.params); err == nil || err.Status != 400 {
			t.Fatalf("%s cursor status = %#v, want 400", test.action, err)
		}
	}
}

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
