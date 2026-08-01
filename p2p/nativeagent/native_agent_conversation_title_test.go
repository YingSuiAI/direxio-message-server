package nativeagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
	"github.com/cloudwego/eino/schema"
)

func TestFirstSuccessfulPersistentTurnGeneratesConversationTitle(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(jsonValue(payload["messages"]), "Generate one concise title") {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"AWS 服务部署"}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"我会先分析项目。"}}]}`))
	}))
	defer server.Close()

	store := NewInMemoryConversationMemoryStore()
	runtime := New(Config{Memory: store, Conversations: store, PersistentMemoryReady: true})
	if _, err := runtime.Invoke(context.Background(), "agent.chat.conversations.create", map[string]any{
		"conversation_id": "title-conversation", "title": "", "idempotency_key": "create-title",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
		"conversation_id": "title-conversation",
		"prompt":          "请帮我部署一个 AWS 服务",
		"model_profile": map[string]any{
			"provider": "openai_compatible", "model": "mock", "base_url": server.URL, "api_key": "test-key",
		},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := runtime.Invoke(context.Background(), "agent.chat.conversations.get", map[string]any{
		"conversation_id": "title-conversation", "message_limit": 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	conversation := result["conversation"].(map[string]any)
	if conversation["title"] != "AWS 服务部署" || requests != 2 {
		t.Fatalf("conversation=%#v requests=%d", conversation, requests)
	}
}

func TestConversationTitleFallbackIsShortAndRedacted(t *testing.T) {
	title := conversationTitleFallback("OpenRouter token=sk-or-v1-abcdefghijklmnopqrstuvwxyz123456 用于部署服务，后面还有很长的说明", "")
	if title == "" || len([]rune(title)) > nativeAgentConversationTitleRunes || strings.Contains(title, "sk-or-v1-") {
		t.Fatalf("unsafe fallback title %q", title)
	}
}

func TestAutomaticConversationTitleNeverOverwritesUserTitle(t *testing.T) {
	store := NewInMemoryConversationMemoryStore()
	if _, _, err := store.CreateConversation(context.Background(), "owner", "manual-title", "用户标题", "create", agentmemory.KnowledgeDigest("manual-title", "用户标题", nil)); err != nil {
		t.Fatal(err)
	}
	updated, err := store.SetAutomaticConversationTitle(context.Background(), "owner", "manual-title", "模型标题")
	if err != nil || updated {
		t.Fatalf("automatic title updated manual title: updated=%v err=%v", updated, err)
	}
	conversation, _, _, err := store.GetConversation(context.Background(), "owner", "manual-title", 20, "")
	if err != nil || conversation.Title != "用户标题" {
		t.Fatalf("conversation = %#v err=%v", conversation, err)
	}
}

func TestContextCompactionUsesTokenBudget(t *testing.T) {
	messages := []*schema.Message{
		schema.SystemMessage("system"),
		schema.UserMessage(strings.Repeat("old ", 600)),
		schema.AssistantMessage(strings.Repeat("older ", 600), nil),
		schema.UserMessage("latest request"),
	}
	compacted := compactEinoMessagesForContext(messages, 1024, 128)
	if len(compacted) >= len(messages) || compacted[len(compacted)-1].Content != "latest request" {
		t.Fatalf("token compaction = %#v", compacted)
	}
}
