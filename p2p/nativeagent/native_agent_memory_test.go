package nativeagent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cloudwego/eino/schema"
)

func TestConversationIntegerParamsAcceptJSONNumbersAndRejectNonIntegers(t *testing.T) {
	runtime := New(Config{Memory: NewInMemoryConversationMemoryStore()})
	ctx := context.Background()
	if _, err := runtime.Invoke(ctx, "agent.chat.conversations.create", map[string]any{"conversation_id": "json-number", "idempotency_key": "create"}); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	for _, test := range []struct {
		action string
		params map[string]any
	}{
		{"agent.chat.conversations.list", map[string]any{"page_size": json.Number("1")}},
		{"agent.chat.conversations.get", map[string]any{"conversation_id": "json-number", "message_limit": json.Number("1")}},
		{"agent.chat.conversations.rename", map[string]any{"conversation_id": "json-number", "expected_revision": json.Number("1"), "idempotency_key": "rename"}},
	} {
		if _, err := runtime.Invoke(ctx, test.action, test.params); err != nil {
			t.Fatalf("%s rejected exact json.Number: %v", test.action, err)
		}
	}

	for _, value := range []any{json.Number("1.5"), json.Number("9223372036854775808"), math.NaN(), math.Inf(1), true, "1"} {
		if err := strictParams(map[string]any{"page_size": value}, map[string]string{"page_size": "integer"}); err == nil || !IsValidationError(err) {
			t.Fatalf("page_size accepted invalid integer %#v: %v", value, err)
		}
	}
	for _, value := range []any{float64(1), int64(1), uint32(1)} {
		if err := strictParams(map[string]any{"page_size": value}, map[string]string{"page_size": "integer"}); err != nil {
			t.Fatalf("page_size rejected integral WS number %#v: %v", value, err)
		}
	}
}

type failingConversationMemoryStore struct{}

func (failingConversationMemoryStore) LoadConversationMemory(context.Context, string, string) (ConversationMemory, error) {
	return ConversationMemory{}, nil
}
func (failingConversationMemoryStore) AppendConversationMessages(context.Context, string, string, string, []StoredMessage) error {
	return fmt.Errorf("store unavailable")
}
func (failingConversationMemoryStore) SaveConversationSummary(context.Context, string, string, string, int64) error {
	return fmt.Errorf("store unavailable")
}

func TestRememberEinoMessagesReportsPersistenceFailure(t *testing.T) {
	runtime := New(Config{Memory: failingConversationMemoryStore{}})
	run := nativeAgentRunContext{conversationID: "conversation", memory: nativeAgentMemory{ConversationID: "conversation"}, memoryMessages: []*schema.Message{schema.UserMessage("secret prompt")}}
	err := runtime.rememberEinoMessages(context.Background(), map[string]any{}, map[string]any{}, nativeModelProfile{}, run, nil)
	if err == nil || strings.Contains(err.Error(), "secret prompt") {
		t.Fatalf("expected safe memory persistence error, got %v", err)
	}
}

func TestDefaultMemoryStoreDoesNotWriteDataDir(t *testing.T) {
	dir := t.TempDir()
	runtime := New(Config{DataDir: dir})
	run := nativeAgentRunContext{conversationID: "conversation", memory: nativeAgentMemory{ConversationID: "conversation"}, memoryMessages: []*schema.Message{schema.UserMessage("prompt")}}
	if err := runtime.rememberEinoMessages(context.Background(), map[string]any{}, map[string]any{}, nativeModelProfile{}, run, nil); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("default memory store wrote files: %#v", entries)
	}
}

func TestRuntimeInjectsConfiguredOwnerAndFailsClosedWithoutPersistentStore(t *testing.T) {
	runtime := New(Config{OwnerID: func() string { return "@alice:example.com" }, Memory: NewInMemoryConversationMemoryStore()})
	ctx := runtime.withRequestContext(context.Background(), map[string]any{"conversation_id": "c"})
	if got := MemoryStoreOwner(ctx); got != "@alice:example.com" {
		t.Fatalf("owner = %q", got)
	}
	production := New(Config{OwnerID: func() string { return "@alice:example.com" }})
	if err := production.rememberEinoMessages(production.withRequestContext(context.Background(), map[string]any{"conversation_id": "c"}), nil, map[string]any{}, nativeModelProfile{}, nativeAgentRunContext{conversationID: "c", memoryMessages: []*schema.Message{schema.UserMessage("x")}}, nil); err == nil {
		t.Fatal("expected missing persistent store to fail closed")
	}
}

func TestPersistentConversationMemoryRejectsClientHistoryReplay(t *testing.T) {
	store := NewInMemoryConversationMemoryStore()
	runtime := New(Config{Memory: store, Conversations: store, PersistentMemoryReady: true})
	_, err := runtime.prepareEinoRun(
		runtime.withRequestContext(context.Background(), map[string]any{"conversation_id": "server-memory"}),
		nil,
		map[string]any{
			"conversation_id": "server-memory",
			"prompt":          "current instruction",
			"messages":        []any{map[string]any{"role": "user", "content": "client replayed history"}},
		},
		nativeModelProfile{},
	)
	if err == nil || !IsValidationError(err) || !strings.Contains(err.Error(), "send only the current prompt") {
		t.Fatalf("client history replay error = %v", err)
	}
}

func TestContextCompressSummarizesOlderMemoryTurns(t *testing.T) {
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	ctx := context.Background()
	memory := nativeAgentMemory{ConversationID: "compress-test"}
	for i := 0; i < 8; i++ {
		memory.Messages = append(memory.Messages, schema.UserMessage("用户轮次"), schema.AssistantMessage("助手轮次", nil))
	}
	if err := runtime.saveMemory(ctx, memory); err != nil {
		t.Fatalf("save memory: %v", err)
	}

	result, err := runtime.Invoke(ctx, "agent.context.compress", map[string]any{
		"conversation_id": "compress-test",
		"memory_window":   2,
	})
	if err != nil {
		t.Fatalf("compress memory: %v", err)
	}
	if trimString(result["summary"]) == "" {
		t.Fatalf("expected compressed summary, got %#v", result)
	}
	messages, ok := result["messages"].([]*schema.Message)
	if !ok || len(messages) != 2 {
		t.Fatalf("expected only recent Eino messages after compression, got %#v", result["messages"])
	}
}

func TestContextCompressCanUseEinoModelSummary(t *testing.T) {
	var sawCompressionPrompt bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode compression payload: %v", err)
		}
		if strings.Contains(jsonValue(payload["messages"]), "compress Dirextalk Agent conversation memory") {
			sawCompressionPrompt = true
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"模型压缩摘要"}}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	ctx := context.Background()
	memory := nativeAgentMemory{ConversationID: "model-compress"}
	for i := 0; i < 4; i++ {
		memory.Messages = append(memory.Messages, schema.UserMessage("用户偏好中文"), schema.AssistantMessage("已记录偏好", nil))
	}
	if err := runtime.saveMemory(ctx, memory); err != nil {
		t.Fatalf("save memory: %v", err)
	}
	result, err := runtime.Invoke(ctx, "agent.context.compress", map[string]any{
		"conversation_id": "model-compress",
		"memory_window":   2,
		"model_profile": map[string]any{
			"provider": "openai_compatible",
			"model":    "mock-compress",
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	})
	if err != nil {
		t.Fatalf("model compress memory: %v", err)
	}
	if !sawCompressionPrompt || result["summary"] != "模型压缩摘要" || result["compression"] != "eino_model" {
		t.Fatalf("expected Eino model compression, saw=%v result=%#v", sawCompressionPrompt, result)
	}
}
