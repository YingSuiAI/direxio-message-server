package nativeagent

import (
	"context"
	"errors"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentmemory"
)

func knowledgeParams() map[string]any {
	return map[string]any{"title": "t", "content": "body", "tags": []any{}, "idempotency_key": "id"}
}

func TestKnowledgeConfiguredEmbeddingFailureDoesNotFallback(t *testing.T) {
	store := agentmemory.NewInMemoryStore()
	failingRuntime := New(Config{Knowledge: store, OwnerID: func() string { return "owner" }, EmbeddingSession: func(context.Context) (agentmemory.KnowledgeEmbeddingSession, error) {
		return agentmemory.KnowledgeEmbeddingSession{}, errors.New("provider unavailable")
	}})
	if _, err := failingRuntime.Invoke(context.Background(), "agent.knowledge.memory.create", knowledgeParams()); err == nil {
		t.Fatal("expected provider error")
	}
	items, _, err := store.SearchKnowledgeMemory(context.Background(), "owner", "", 10, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("stored text=%v err=%v", items, err)
	}
	runtime := New(Config{Knowledge: store, OwnerID: func() string { return "owner" }, EmbeddingSession: func(context.Context) (agentmemory.KnowledgeEmbeddingSession, error) {
		return agentmemory.KnowledgeEmbeddingSession{ProfileID: "p", Revision: 1, Model: "m", Embed: func(context.Context, string) ([]float32, error) { return []float32{1, 0}, nil }}, nil
	}})
	if _, err := runtime.Invoke(context.Background(), "agent.knowledge.memory.create", knowledgeParams()); err != nil {
		t.Fatal(err)
	}
	items, _, err = store.SearchKnowledgeMemory(context.Background(), "owner", "", 10, "")
	if err != nil || len(items) != 1 {
		t.Fatalf("retry duplicated text=%v err=%v", items, err)
	}
	if _, err := failingRuntime.Invoke(context.Background(), "agent.knowledge.search", map[string]any{"query": "body", "page_size": int64(10), "page_token": ""}); err == nil {
		t.Fatal("expected semantic provider error")
	}
}

func TestKnowledgeNoDefaultEmbeddingUsesTextFallback(t *testing.T) {
	store := agentmemory.NewInMemoryStore()
	runtime := New(Config{Knowledge: store, OwnerID: func() string { return "owner" }, EmbeddingSession: func(context.Context) (agentmemory.KnowledgeEmbeddingSession, error) {
		return agentmemory.KnowledgeEmbeddingSession{}, agentmemory.ErrNoEmbeddingProfile
	}})
	result, err := runtime.Invoke(context.Background(), "agent.knowledge.memory.create", knowledgeParams())
	if err != nil {
		t.Fatal(err)
	}
	if result["embedding_indexed"] != false {
		t.Fatalf("result=%v", result)
	}
	search, err := runtime.Invoke(context.Background(), "agent.knowledge.search", map[string]any{"query": "body", "page_size": int64(10), "page_token": ""})
	if err != nil {
		t.Fatal(err)
	}
	if search["search_mode"] != "text" {
		t.Fatalf("search=%v", search)
	}
}
