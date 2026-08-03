package nativeagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelLoopCallsToolThenFinalizesAndStoresMemory(t *testing.T) {
	var requestCount int
	var sawToolResult bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode model request: %v", err)
		}
		messages, _ := payload["messages"].([]any)
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			if message["role"] == "tool" && strings.Contains(trimString(message["content"]), "Ada") {
				sawToolResult = true
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"dirextalk_contacts_list","arguments":"{\"query\":\"ada\"}"}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"总结：联系人 Ada 已找到。"}}]}`))
	}))
	defer server.Close()

	var toolCalled bool
	runtime := New(Config{
		DataDir: filepath.Join(t.TempDir(), "agent"),
		Store:   &testConfigStore{config: map[string]any{"enabled_tools": []any{"dirextalk_contacts_list"}}},
		Tools: []Tool{{
			Name:        "dirextalk_contacts_list",
			Description: "List contacts.",
			Parameters:  objectSchema(map[string]any{"query": stringSchema()}),
			Handler: func(ctx context.Context, args map[string]any) (any, error) {
				toolCalled = true
				return map[string]any{"contacts": []map[string]any{{"display_name": "Ada", "room_id": "!ada:example.com"}}}, nil
			},
		}},
	})

	result, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
		"conversation_id": "loop-test",
		"prompt":          "找 Ada 并总结",
		"model_profile": map[string]any{
			"provider": "openai_compatible",
			"model":    "mock-model",
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	})
	if err != nil {
		t.Fatalf("agent chat failed: %v", err)
	}
	if !toolCalled || !sawToolResult || requestCount != 2 {
		t.Fatalf("expected tool loop with second model call, toolCalled=%v sawToolResult=%v requestCount=%d", toolCalled, sawToolResult, requestCount)
	}
	if result["text"] != "总结：联系人 Ada 已找到。" {
		t.Fatalf("expected final model answer, got %#v", result)
	}
	if result["framework"] != "eino" {
		t.Fatalf("expected eino framework marker, got %#v", result)
	}
	references, ok := result["references"].([]map[string]any)
	if !ok || len(references) != 1 || references[0]["kind"] != "room" || references[0]["room_id"] != "!ada:example.com" || references[0]["room_type"] != "direct" {
		t.Fatalf("expected contact room reference, got %#v", result["references"])
	}
	steps, ok := result["steps"].([]map[string]any)
	if !ok || len(steps) < 4 {
		t.Fatalf("expected trace steps for context/tool/final display, got %#v", result["steps"])
	}
	if !traceHasStep(steps, "tool_call", "dirextalk_contacts_list") || !traceHasStep(steps, "tool_result", "dirextalk_contacts_list") {
		t.Fatalf("expected tool call and tool result trace steps, got %#v", steps)
	}
	trace, ok := result["trace"].(map[string]any)
	if !ok || trace["framework"] != "eino" || trace["disclaimer"] == "" {
		t.Fatalf("expected observable Eino trace with disclaimer, got %#v", result["trace"])
	}
	memory, err := runtime.loadMemory(context.Background(), "loop-test")
	if err != nil {
		t.Fatalf("load memory: %v", err)
	}
	if len(memory.Messages) != 4 || memory.Messages[0].Role != "user" || memory.Messages[1].Role != "assistant" || memory.Messages[2].Role != "tool" || memory.Messages[3].Role != "assistant" {
		t.Fatalf("expected Eino user/tool-loop/assistant memory messages, got %#v", memory.Messages)
	}
	if len(memory.Messages[1].ToolCalls) != 1 || memory.Messages[2].ToolName != "dirextalk_contacts_list" {
		t.Fatalf("expected tool call and tool result in Eino memory, got %#v", memory.Messages)
	}
}

func TestModelRuntimeCLIActionIsForbidden(t *testing.T) {
	runtime := New(Config{})
	if _, err := runtime.Invoke(context.Background(), "agent.runtime.install", nil); err == nil || err.Error() != embeddedExtensionsForbiddenMessage {
		t.Fatalf("runtime install = %v, want stable forbidden error", err)
	}
}

func TestModelRequestedRuntimeShellIsDeniedWithoutExecution(t *testing.T) {
	var requestCount int
	var denied bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode model request: %v", err)
		}
		messages, _ := payload["messages"].([]any)
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			content := trimString(message["content"])
			if message["role"] == "tool" && strings.Contains(content, "runtime__shell") && strings.Contains(content, "not available") {
				denied = true
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_shell","type":"function","function":{"name":"runtime__shell","arguments":"{\"command\":\"printf shell-eino\"}"}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"嵌入式运行时不提供 shell 命令执行。"}}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent"), Store: &testConfigStore{config: map[string]any{}}})
	result, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "执行 shell 命令",
		"model_profile": map[string]any{
			"provider": "openai_compatible",
			"model":    "mock-model",
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	})
	if err != nil {
		t.Fatalf("agent chat failed: %v", err)
	}
	if requestCount != 2 || !denied || result["text"] != "嵌入式运行时不提供 shell 命令执行。" {
		t.Fatalf("shell must be denied, requestCount=%d denied=%v result=%#v", requestCount, denied, result)
	}
}

func TestRuntimeShellIsAbsentFromEinoRegistry(t *testing.T) {
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	tools, cleanup, err := runtime.enabledEinoTools(context.Background(), map[string]any{"runtime_shell_enabled": true}, map[string]any{})
	if err != nil {
		t.Fatalf("enabled Eino tools: %v", err)
	}
	defer cleanup()
	for _, tool := range tools {
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatalf("tool info: %v", err)
		}
		if info.Name == "runtime__shell" {
			t.Fatalf("runtime__shell must be absent")
		}
	}
}

func TestRuntimeShellEinoToolIsAbsent(t *testing.T) {
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	tools, cleanup, err := runtime.enabledEinoTools(context.Background(), map[string]any{}, nil)
	if err != nil {
		t.Fatalf("enabled Eino tools: %v", err)
	}
	defer cleanup()
	for _, tool := range tools {
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatalf("tool info: %v", err)
		}
		if info.Name != "runtime__shell" {
			continue
		}
		t.Fatal("runtime__shell must be absent from the Eino registry")
	}
	// Reaching here proves the complete Eino registry contains no shell tool.
}

func TestModelLoopHonorsConfiguredMaxToolCalls(t *testing.T) {
	const toolCalls = 8
	var requestCount int
	var observedToolResults int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode model request: %v", err)
		}
		messages, _ := payload["messages"].([]any)
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			if message["role"] == "tool" && strings.Contains(trimString(message["content"]), "compiled-loop-") {
				observedToolResults++
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if requestCount <= toolCalls {
			callID := "call_tool_" + string(rune('0'+requestCount))
			body := `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"` + callID + `","type":"function","function":{"name":"dirextalk_contacts_list","arguments":"{\"query\":\"compiled-loop-` + callID + `\"}"}}]}}]}`
			_, _ = w.Write([]byte(body))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"所有编译内工具步骤已完成。"}}]}`))
	}))
	defer server.Close()

	runtime := New(Config{
		DataDir: filepath.Join(t.TempDir(), "agent"),
		Store: &testConfigStore{config: map[string]any{
			"max_tool_calls": toolCalls,
		}},
		Tools: []Tool{{Name: "dirextalk_contacts_list", Handler: func(_ context.Context, args map[string]any) (any, error) {
			return map[string]any{"result": trimString(args["query"])}, nil
		}}},
	})
	result, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "连续执行多个编译内工具步骤",
		"model_profile": map[string]any{
			"provider": "openai_compatible",
			"model":    "mock-model",
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	})
	if err != nil {
		t.Fatalf("agent chat failed: %v", err)
	}
	if requestCount != toolCalls+1 || observedToolResults < toolCalls || result["text"] != "所有编译内工具步骤已完成。" {
		t.Fatalf("expected configured compiled-tool loop to finish, requestCount=%d observedToolResults=%d result=%#v", requestCount, observedToolResults, result)
	}
}

func TestModelRequestedSkillInstallIsDenied(t *testing.T) {
	var requestCount int
	var sawInstallResult bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode model request: %v", err)
		}
		messages, _ := payload["messages"].([]any)
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			if message["role"] == "tool" && strings.Contains(trimString(message["content"]), "dialogue-skill") {
				sawInstallResult = true
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if requestCount == 1 {
			_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_skill_install","type":"function","function":{"name":"native_agent_skills_install","arguments":"{\"id\":\"dialogue-skill\",\"content\":\"# Skill\\n\\nWhen installed, say DIALOGUE_SKILL_READY.\"}"}}]}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"嵌入式运行时不支持安装 Skill。"}}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent"), Store: &testConfigStore{config: map[string]any{}}})
	result, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "安装一个 dialogue skill",
		"model_profile": map[string]any{
			"provider": "openai_compatible",
			"model":    "mock-model",
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	})
	if err != nil {
		t.Fatalf("agent chat failed: %v", err)
	}
	if requestCount != 2 || !sawInstallResult || result["text"] != "嵌入式运行时不支持安装 Skill。" || strings.Contains(trimString(result["text"]), "已安装") {
		t.Fatalf("model must receive a denied mutable-skill result and reject success claims, requestCount=%d sawInstallResult=%v result=%#v", requestCount, sawInstallResult, result)
	}
	steps, ok := result["steps"].([]map[string]any)
	if !ok || !traceHasStep(steps, "tool_call", "native_agent_skills_install") {
		t.Fatalf("model tool request should remain observable while execution is denied, got %#v", result["steps"])
	}
	list, err := runtime.skillsList(context.Background())
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	skills := list["skills"].([]map[string]any)
	for _, skill := range skills {
		if trimString(skill["id"]) == "dialogue-skill" {
			t.Fatalf("mutable skill must not be installed, got %#v", list)
		}
	}
	config, _, err := runtime.agentConfig(context.Background())
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if prompt := runtime.enabledSkillsPrompt(context.Background(), config); strings.Contains(prompt, "DIALOGUE_SKILL_READY") {
		t.Fatalf("mutable skill must not reach next prompt, got %q", prompt)
	}
}

func TestConfigEnabledToolsStillExposeDialogueManagementTools(t *testing.T) {
	runtime := New(Config{
		DataDir: filepath.Join(t.TempDir(), "agent"),
		Store:   &testConfigStore{config: map[string]any{"enabled_tools": []any{"search_contacts", "search_rooms", "list_messages", "send_message", "summarize_conversation"}}},
	})
	tools, cleanup, err := runtime.enabledEinoTools(context.Background(), map[string]any{
		"enabled_tools": []any{"search_contacts", "search_rooms", "list_messages", "send_message", "summarize_conversation"},
	}, nil)
	if err != nil {
		t.Fatalf("enabled Eino tools: %v", err)
	}
	defer cleanup()
	names := map[string]bool{}
	for _, tool := range tools {
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatalf("tool info: %v", err)
		}
		names[info.Name] = true
	}
	if names["native_agent_skills_install"] || names["native_agent_mcp_servers_install"] {
		t.Fatalf("config-level enabled_tools must not add management tools, got %#v", names)
	}

	requestTools, requestCleanup, err := runtime.enabledEinoTools(context.Background(), map[string]any{
		"enabled_tools": []any{"search_contacts", "search_rooms", "list_messages", "send_message", "summarize_conversation"},
	}, map[string]any{
		"enabled_tools": []any{"search_contacts"},
	})
	if err != nil {
		t.Fatalf("request enabled Eino tools: %v", err)
	}
	defer requestCleanup()
	requestNames := map[string]bool{}
	for _, tool := range requestTools {
		info, err := tool.Info(context.Background())
		if err != nil {
			t.Fatalf("request tool info: %v", err)
		}
		requestNames[info.Name] = true
	}
	if requestNames["native_agent_skills_install"] || requestNames["native_agent_mcp_servers_install"] {
		t.Fatalf("request-level enabled_tools must not add management tools, got %#v", requestNames)
	}
}

func TestOpenAIProviderUsesChatCompletionsEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"openai ok"}}]}`))
	}))
	defer server.Close()
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "hello",
		"model_profile": map[string]any{
			"provider": "openai",
			"model":    "mock-openai",
			"base_url": server.URL + "/v1",
			"api_key":  "test-key",
		},
	})
	if err != nil {
		t.Fatalf("openai provider: %v", err)
	}
	if gotPath != "/v1/chat/completions" || result["text"] != "openai ok" {
		t.Fatalf("expected openai chat completions, path=%q result=%#v", gotPath, result)
	}
}

func TestDeepSeekProviderUsesChatCompletionsEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"deepseek ok"}}]}`))
	}))
	defer server.Close()
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "hello",
		"model_profile": map[string]any{
			"provider": "deepseek",
			"model":    "mock-deepseek",
			"base_url": server.URL + "/v1",
			"api_key":  "test-key",
		},
	})
	if err != nil {
		t.Fatalf("deepseek provider: %v", err)
	}
	if gotPath != "/v1/chat/completions" || result["text"] != "deepseek ok" {
		t.Fatalf("expected deepseek chat completions, path=%q result=%#v", gotPath, result)
	}
}

func TestGeminiProviderUsesNativeV1BetaBaseURL(t *testing.T) {
	for _, test := range []struct {
		baseURL string
		want    string
	}{
		{
			baseURL: "https://generativelanguage.googleapis.com/v1beta",
			want:    "https://generativelanguage.googleapis.com/v1beta",
		},
		{
			baseURL: "https://gateway.example",
			want:    "https://gateway.example/v1beta",
		},
	} {
		if got := geminiV1BetaBaseURL(test.baseURL); got != test.want {
			t.Fatalf("Gemini base URL = %q, want %q", got, test.want)
		}
	}
}

func TestGeminiProviderUsesNativeGenerateContent(t *testing.T) {
	var gotPath, gotAPIKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-goog-api-key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"gemini native ok"}]}}]}`))
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "hello",
		"model_profile": map[string]any{
			"provider": "gemini",
			"model":    "gemini-test",
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	})
	if err != nil {
		t.Fatalf("gemini native provider: %v", err)
	}
	if gotPath != "/v1beta/models/gemini-test:generateContent" ||
		gotAPIKey != "test-key" ||
		result["text"] != "gemini native ok" {
		t.Fatalf("unexpected Gemini native request path=%q api_key=%q result=%#v", gotPath, gotAPIKey, result)
	}
}

func TestStreamEmitsSafeProviderFailureDetail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gateway rejected test-key", http.StatusBadGateway)
	}))
	defer server.Close()

	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	var events []Event
	err := runtime.Stream(context.Background(), "agent.chat.stream", map[string]any{
		"prompt": "hello",
		"model_profile": map[string]any{
			"provider": "openai_compatible",
			"model":    "test-model",
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	if len(events) != 1 || events[0].Event != "error" {
		t.Fatalf("expected one error event, got %#v", events)
	}
	message := trimString(events[0].Data["error"])
	if !strings.Contains(message, "model provider returned 502") ||
		!strings.Contains(message, "[redacted]") ||
		strings.Contains(message, "test-key") {
		t.Fatalf("unexpected safe provider error: %q", message)
	}
}

func TestStreamCompactsMessagesByContextWindow(t *testing.T) {
	var gotMessages []any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode stream payload: %v", err)
		}
		gotMessages, _ = payload["messages"].([]any)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	var events []Event
	err := runtime.Stream(context.Background(), "agent.chat.stream", map[string]any{
		"messages": []any{
			map[string]any{"role": "user", "content": "old"},
			map[string]any{"role": "user", "content": "new"},
		},
		"model_profile": map[string]any{
			"provider":       "openai_compatible",
			"model":          "mock-stream",
			"base_url":       server.URL,
			"api_key":        "test-key",
			"context_window": 1,
		},
	}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	if len(gotMessages) != 2 {
		t.Fatalf("expected compacted stream messages, got %#v", gotMessages)
	}
	system, _ := gotMessages[0].(map[string]any)
	if !strings.Contains(system["content"].(string), "Dirextalk Native Agent") {
		t.Fatalf("expected native system prompt after compaction, got %#v", gotMessages)
	}
	last, _ := gotMessages[1].(map[string]any)
	if last["content"] != "new" {
		t.Fatalf("expected newest message after compaction, got %#v", gotMessages)
	}
	if len(events) != 3 || events[0].Event != "delta" || events[1].Event != "trace" || events[2].Event != "done" {
		t.Fatalf("expected delta, trace, and done events, got %#v", events)
	}
	if events[1].Data["framework"] != "eino" || events[1].Data["steps"] == nil {
		t.Fatalf("expected eino stream trace marker, got %#v", events[1])
	}
	if events[2].Data["framework"] != "eino" || events[2].Data["trace"] == nil {
		t.Fatalf("expected eino stream done marker with trace, got %#v", events[2])
	}
}

func TestStreamDoneIncludesReferencesFromBuiltInToolResults(t *testing.T) {
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		w.Header().Set("Content-Type", "text/event-stream")
		if requestCount == 1 {
			_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call_rooms\",\"type\":\"function\",\"function\":{\"name\":\"dirextalk_rooms_search\",\"arguments\":\"{\\\"query\\\":\\\"Team\\\"}\"}}]}}]}\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"已找到 Team。\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	runtime := New(Config{
		DataDir: filepath.Join(t.TempDir(), "agent"),
		Store:   &testConfigStore{config: map[string]any{"enabled_tools": []any{"dirextalk_rooms_search"}}},
		Tools: []Tool{{
			Name:        "dirextalk_rooms_search",
			Description: "Search rooms.",
			Parameters:  objectSchema(map[string]any{"query": stringSchema()}),
			Handler: func(ctx context.Context, args map[string]any) (any, error) {
				return map[string]any{"rooms": []map[string]any{{"type": "group", "name": "Team", "room_id": "!team:example.com"}}}, nil
			},
		}},
	})
	var events []Event
	err := runtime.Stream(context.Background(), "agent.chat.stream", map[string]any{
		"prompt": "找到 Team 群聊",
		"model_profile": map[string]any{
			"provider": "openai_compatible",
			"model":    "mock-stream",
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	if requestCount != 2 || len(events) == 0 || events[len(events)-1].Event != "done" {
		t.Fatalf("expected tool loop and done event, requests=%d events=%#v", requestCount, events)
	}
	references, ok := events[len(events)-1].Data["references"].([]map[string]any)
	if !ok || len(references) != 1 || references[0]["room_id"] != "!team:example.com" || references[0]["room_type"] != "group" {
		t.Fatalf("expected room reference in done event, got %#v", events[len(events)-1].Data["references"])
	}
}

func TestStreamEmitsOpenAICompatibleReasoningContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"先分析\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"，再回答\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	var events []Event
	err := runtime.Stream(context.Background(), "agent.chat.stream", map[string]any{
		"prompt": "hello",
		"model_profile": map[string]any{
			"provider": "openai_compatible",
			"model":    "mock-stream",
			"base_url": server.URL,
			"api_key":  "test-key",
		},
	}, func(event Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("stream chat: %v", err)
	}
	if len(events) != 4 {
		t.Fatalf("expected reasoning delta, text delta, trace, and done events, got %#v", events)
	}
	if events[0].Event != "delta" || events[0].Data["reasoning_content"] != "先分析" {
		t.Fatalf("expected reasoning delta, got %#v", events[0])
	}
	if events[1].Event != "delta" || events[1].Data["text"] != "，再回答" {
		t.Fatalf("expected text delta, got %#v", events[1])
	}
	if events[3].Event != "done" || events[3].Data["text"] != "，再回答" || events[3].Data["reasoning_content"] != "先分析" {
		t.Fatalf("expected done to include text and reasoning, got %#v", events[3])
	}
}

func TestOpenAICompatibleRequestIncludesReasoningEffort(t *testing.T) {
	var gotReasoningEffort any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode model request: %v", err)
		}
		gotReasoningEffort = payload["reasoning_effort"]
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	_, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "hello",
		"model_profile": map[string]any{
			"provider":       "openai_compatible",
			"model":          "mock-model",
			"base_url":       server.URL,
			"api_key":        "test-key",
			"reasoning_mode": "high",
		},
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if gotReasoningEffort != "high" {
		t.Fatalf("expected reasoning_effort high, got %#v", gotReasoningEffort)
	}
}

func traceHasStep(steps []map[string]any, stepType, name string) bool {
	for _, step := range steps {
		if step["type"] == stepType && step["name"] == name {
			return true
		}
	}
	return false
}

func TestAnthropicProviderUsesMessagesEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if r.Header.Get("x-api-key") != "test-key" || r.Header.Get("anthropic-version") == "" {
			t.Fatalf("missing anthropic headers: %#v", r.Header)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"anthropic ok"}]}`))
	}))
	defer server.Close()
	runtime := New(Config{DataDir: filepath.Join(t.TempDir(), "agent")})
	result, err := runtime.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "hello",
		"model_profile": map[string]any{
			"provider": "anthropic",
			"model":    "mock-claude",
			"base_url": server.URL + "/v1",
			"api_key":  "test-key",
		},
	})
	if err != nil {
		t.Fatalf("anthropic provider: %v", err)
	}
	if gotPath != "/v1/messages" || result["text"] != "anthropic ok" {
		t.Fatalf("expected anthropic messages endpoint, path=%q result=%#v", gotPath, result)
	}
}
