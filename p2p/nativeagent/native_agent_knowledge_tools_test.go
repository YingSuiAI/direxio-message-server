package nativeagent

import (
	"context"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
)

func knowledgeToolRuntime(ready bool) (*Runtime, *agentmemory.InMemoryStore) {
	store := agentmemory.NewInMemoryStore()
	return New(Config{
		Knowledge:             store,
		OwnerID:               func() string { return "owner-a" },
		PersistentMemoryReady: ready,
	}), store
}

func findKnowledgeTool(t *testing.T, runtime *Runtime, name string) Tool {
	t.Helper()
	for _, tool := range runtime.enabledTools(context.Background(), nil, nil) {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("tool %q is not registered", name)
	return Tool{}
}

func TestKnowledgeEinoToolsRegistrationSelectionAndSchema(t *testing.T) {
	runtime, _ := knowledgeToolRuntime(true)
	tools := runtime.enabledTools(context.Background(), map[string]any{"enabled_tools": []any{"dirextalk_contacts_list"}}, nil)
	seen := map[string]Tool{}
	for _, tool := range tools {
		seen[tool.Name] = tool
	}
	for _, name := range []string{"native_agent_memory_remember", "native_agent_memory_search"} {
		tool, ok := seen[name]
		if !ok {
			t.Fatalf("durable memory tool %q missing from legacy selection", name)
		}
		if _, ok := tool.Parameters["owner"]; ok {
			t.Fatalf("%s must not expose owner argument", name)
		}
		if _, ok := tool.Parameters["idempotency_key"]; ok {
			t.Fatalf("%s must not expose raw idempotency argument", name)
		}
	}
	if got := nativeToolAlias("recall"); got != "native_agent_memory_search" {
		t.Fatalf("recall alias = %q", got)
	}
	if got := nativeToolAlias("remember"); got != "native_agent_memory_remember" {
		t.Fatalf("remember alias = %q", got)
	}
}

func TestKnowledgeEinoToolsPersistIdempotentlyAndIsolateOwners(t *testing.T) {
	runtime, store := knowledgeToolRuntime(true)
	remember := findKnowledgeTool(t, runtime, "native_agent_memory_remember")
	ctx := WithRequestContext(context.Background(), "owner-a", "conversation-1", "请记住：我喜欢红茶")
	args := map[string]any{"title": "偏好", "content": "我喜欢红茶", "tags": []any{"preference"}}
	first, err := remember.Handler(ctx, args)
	if err != nil {
		t.Fatalf("remember: %v", err)
	}
	second, err := remember.Handler(ctx, args)
	if err != nil {
		t.Fatalf("idempotent replay: %v", err)
	}
	firstMap, secondMap := first.(map[string]any), second.(map[string]any)
	if firstMap["memory_id"] != secondMap["memory_id"] || secondMap["replayed"] != true {
		t.Fatalf("expected deterministic replay, first=%#v second=%#v", first, second)
	}
	count, err := store.KnowledgeStatus(ctx, "owner-a")
	if err != nil || count != 1 {
		t.Fatalf("owner-a memory count = %d, err=%v", count, err)
	}
	search := findKnowledgeTool(t, runtime, "native_agent_memory_search")
	otherOwner := WithRequestContext(context.Background(), "owner-b", "conversation-1", "你还记得吗")
	result, err := search.Handler(otherOwner, map[string]any{"query": "红茶"})
	if err != nil {
		t.Fatalf("owner-scoped search: %v", err)
	}
	if got := len(result.(map[string]any)["items"].([]map[string]any)); got != 0 {
		t.Fatalf("owner-b saw owner-a memory: %#v", result)
	}
}

func TestKnowledgeEinoRememberRequiresExplicitIntent(t *testing.T) {
	runtime, store := knowledgeToolRuntime(true)
	remember := findKnowledgeTool(t, runtime, "native_agent_memory_remember")
	args := map[string]any{"content": "ordinary conversation"}
	for _, text := range []string{
		"普通聊天，不要保存", "你还记得我喜欢什么？", "不要记住这个", "别记住这个",
		"do not remember this", "don't remember this", "I remember my name is Adam",
	} {
		ctx := WithRequestContext(context.Background(), "owner-a", "conversation-1", text)
		if _, err := remember.Handler(ctx, args); err == nil {
			t.Fatalf("remember unexpectedly accepted %q", text)
		}
	}
	if count, _ := store.KnowledgeStatus(context.Background(), "owner-a"); count != 0 {
		t.Fatalf("rejected requests created %d memories", count)
	}
	for _, text := range []string{"please remember this", "remember my name is Adam", "请存到记忆", "请存入记忆"} {
		ctx := WithRequestContext(context.Background(), "owner-a", "conversation-1", text)
		if _, err := remember.Handler(ctx, args); err != nil {
			t.Fatalf("explicit remember %q rejected: %v", text, err)
		}
	}
}

func TestKnowledgeEinoToolsOmitWhenUnavailable(t *testing.T) {
	runtime, _ := knowledgeToolRuntime(false)
	for _, tool := range runtime.enabledTools(context.Background(), nil, nil) {
		if strings.HasPrefix(tool.Name, "native_agent_memory_") {
			t.Fatalf("unready store exposed %s", tool.Name)
		}
	}
}

func TestNativeAgentMemoryPromptPolicy(t *testing.T) {
	runtime := New(Config{})
	prompt := runtime.agentSystemPrompt(context.Background(), nil, nil, "")
	for _, phrase := range []string{"native_agent_memory_remember", "native_agent_memory_search", "Never silently store", "conversation memory is separate"} {
		if !strings.Contains(prompt, phrase) {
			t.Fatalf("prompt missing %q: %s", phrase, prompt)
		}
	}
}
