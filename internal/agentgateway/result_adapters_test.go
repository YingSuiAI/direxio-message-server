package agentgateway

import (
	"errors"
	"maps"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func canonicalChatResponseForTest(content string, references, taskIDs, planIDs []any) map[string]any {
	message := map[string]any{"content": content}
	response := map[string]any{"message": message, "done": true}
	for field, value := range map[string][]any{
		"references": references, "related_task_ids": taskIDs, "related_plan_ids": planIDs,
	} {
		if value != nil {
			response[field] = value
			message[field] = value
		}
	}
	return response
}

func TestPublicResultAdaptersProjectCanonicalCapabilityResults(t *testing.T) {
	tests := []struct {
		name   string
		action string
		input  map[string]any
		check  func(*testing.T, map[string]any)
	}{
		{"conversation create", "agent.chat.conversations.create", map[string]any{"conversation_id": "c1", "title": "Chat"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["conversation"]; !ok || got["replayed"] != false {
				t.Fatalf("conversation create = %#v", got)
			}
		}},
		{"conversation get", "agent.chat.conversations.get", map[string]any{"conversation": map[string]any{"conversation_id": "c1"}, "messages": []any{map[string]any{"role": "user", "references": []any{}}}, "next_page_token": "next"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["conversation"]; !ok || got["next_cursor"] != "next" || len(got["messages"].([]any)) != 1 {
				t.Fatalf("conversation get = %#v", got)
			}
		}},
		{"conversation list", "agent.chat.conversations.list", map[string]any{"conversations": []any{}, "next_page_token": "p"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["conversations"]; !ok || got["next_cursor"] != "p" {
				t.Fatalf("conversation list = %#v", got)
			}
		}},
		{"conversation delete", "agent.chat.conversations.delete", map[string]any{"deleted": true}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"conversation", "replayed"}) {
				t.Fatalf("conversation delete envelope keys=%v value=%#v", keys, got)
			}
		}},
		{"model get", "agent.model_profiles.get", map[string]any{"id": "p1", "provider": "openai_compatible"}, func(t *testing.T, got map[string]any) {
			profile, ok := got["profile"].(map[string]any)
			if !ok || profile["profile_id"] != "p1" {
				t.Fatalf("model get = %#v", got)
			}
			if _, leaked := profile["id"]; leaked {
				t.Fatalf("core id leaked into model profile: %#v", profile)
			}
		}},
		{"model list", "agent.model_profiles.list", map[string]any{"profiles": []any{}, "next_page_token": "p", "default_tool_client_profile_id": ""}, func(t *testing.T, got map[string]any) {
			if _, ok := got["profiles"]; !ok || got["next_page_token"] != "p" || got["default_tool_client_profile_id"] != "" {
				t.Fatalf("model list = %#v", got)
			}
		}},
		{"provider model catalog", "agent.models.list", map[string]any{
			"models":    []any{map[string]any{"id": "openai/gpt-4o", "name": "GPT-4o", "provider": "openrouter", "input_modalities": []any{"text", "image"}, "pricing": map[string]any{"prompt": "1"}}},
			"providers": []any{map[string]any{"provider": "openrouter", "default_base_url": "https://openrouter.ai/api/v1", "requires_api_key": true, "dynamic_models": true}},
		}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"models", "providers"}) {
				t.Fatalf("model catalog keys=%v value=%#v", keys, got)
			}
			models := got["models"].([]any)
			model := models[0].(map[string]any)
			if model["id"] != "openai/gpt-4o" || model["name"] != "GPT-4o" || !reflect.DeepEqual(model["input_modalities"], []any{"text", "image"}) {
				t.Fatalf("model catalog model = %#v", model)
			}
			if _, leaked := model["pricing"]; leaked {
				t.Fatalf("model catalog retained non-schema field: %#v", model)
			}
			if _, leakedAlias := model["ID"]; leakedAlias {
				t.Fatalf("model catalog retained non-canonical alias: %#v", model)
			}
			providers := got["providers"].([]any)
			provider := providers[0].(map[string]any)
			if provider["provider"] != "openrouter" || provider["default_base_url"] != "https://openrouter.ai/api/v1" || provider["requires_api_key"] != true || provider["dynamic_models"] != true {
				t.Fatalf("model catalog provider = %#v", provider)
			}
		}},
		{"source list", "agent.knowledge.sources.list", map[string]any{"sources": []any{map[string]any{"source_id": "s1", "size_bytes": float64(4)}}, "next_page_token": "p"}, func(t *testing.T, got map[string]any) {
			items := got["sources"].([]any)
			if items[0].(map[string]any)["source_id"] != "s1" || got["next_page_token"] != "p" {
				t.Fatalf("source list = %#v", got)
			}
		}},
		{"knowledge search", "agent.knowledge.search", map[string]any{"items": []any{map[string]any{"source_id": "s1", "score": float64(.9)}}, "next_page_token": "p", "embedding_profile_id": "embed", "embedding_profile_revision": float64(4), "embedding_model": "text-embedding-3-small", "embedding_generation": "gen-7", "collection_config_digest": "digest-8"}, func(t *testing.T, got map[string]any) {
			items := got["items"].([]any)
			if items[0].(map[string]any)["source_id"] != "s1" || got["next_cursor"] != "p" || got["embedding_profile_id"] != "embed" || got["embedding_profile_revision"] != float64(4) || got["embedding_model"] != "text-embedding-3-small" || got["embedding_generation"] != "gen-7" || got["collection_config_digest"] != "digest-8" {
				t.Fatalf("knowledge search = %#v", got)
			}
		}},
		{"knowledge config", "agent.knowledge.config.get", map[string]any{"embedding_profile_id": "embed", "embedding_profile_revision": float64(4), "embedding_model": "text-embedding-3-small", "embedding_generation": "drop-me", "dimension": float64(3), "collection": "knowledge", "collection_config_digest": "digest-8", "revision": float64(2), "updated_at": "updated"}, func(t *testing.T, got map[string]any) {
			if got["embedding_profile_id"] != "embed" || got["embedding_profile_revision"] != float64(4) || got["embedding_model"] != "text-embedding-3-small" || got["dimension"] != float64(3) || got["collection"] != "knowledge" || got["collection_config_digest"] != "digest-8" || got["revision"] != float64(2) || got["updated_at"] != "updated" {
				t.Fatalf("knowledge config = %#v", got)
			}
			if _, exposed := got["embedding_generation"]; exposed {
				t.Fatalf("knowledge config exposed search-only generation: %#v", got)
			}
		}},
		{"knowledge upload start exact projection", "agent.knowledge.upload.start", map[string]any{"upload_id": "u1", "source_id": "s1", "status": "open", "declared_size": float64(8), "received_size": float64(2), "max_chunk_bytes": float64(4), "progress": float64(.25), "replayed": true, "revision": float64(2)}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"max_chunk_bytes", "progress", "received_size", "replayed", "size", "source_id", "status", "upload_id"}) {
				t.Fatalf("upload start keys=%v value=%#v", keys, got)
			}
		}},
		{"knowledge status exact projection", "agent.knowledge.status", map[string]any{"supported": true, "count": float64(6), "ready_count": float64(2), "uploading_count": float64(1), "indexing_count": float64(1), "failed_count": float64(1), "cleanup_pending_count": float64(1), "checked_at": "2026-08-08T12:00:00Z", "embedding_indexed": float64(1), "embedding_stale": float64(2), "embedding_profile_id": "embed", "embedding_profile_revision": float64(4), "embedding_model": "text-embedding-3-small", "quota_used_bytes": float64(1024), "quota_limit_bytes": float64(67108864), "quota_remaining_bytes": float64(67107840), "max_source_bytes": float64(16777216), "extra": "drop"}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"checked_at", "cleanup_pending_count", "count", "embedding_indexed", "embedding_model", "embedding_profile_id", "embedding_profile_revision", "embedding_stale", "failed_count", "indexing_count", "max_source_bytes", "quota_limit_bytes", "quota_remaining_bytes", "quota_used_bytes", "ready_count", "supported", "uploading_count"}) {
				t.Fatalf("knowledge status keys=%v value=%#v", keys, got)
			}
			if got["ready_count"] != float64(2) || got["uploading_count"] != float64(1) || got["indexing_count"] != float64(1) || got["failed_count"] != float64(1) || got["cleanup_pending_count"] != float64(1) || got["checked_at"] != "2026-08-08T12:00:00Z" || got["embedding_indexed"] != float64(1) || got["embedding_stale"] != float64(2) || got["quota_used_bytes"] != float64(1024) || got["quota_limit_bytes"] != float64(67108864) || got["quota_remaining_bytes"] != float64(67107840) || got["max_source_bytes"] != float64(16777216) {
				t.Fatalf("knowledge status counters were inferred/changed: %#v", got)
			}
		}},
		{"task get", "agent.core.tasks.get", map[string]any{"id": "11111111-1111-4111-8111-111111111111", "status": "queued"}, func(t *testing.T, got map[string]any) {
			task, ok := got["task"].(map[string]any)
			if !ok || task["task_id"] != "11111111-1111-4111-8111-111111111111" {
				t.Fatalf("task get = %#v", got)
			}
			if _, present := task["id"]; present {
				t.Fatalf("task get retained Agent identity: %#v", got)
			}
		}},
		{"schedule list", "agent.core.schedules.list", map[string]any{"schedules": []any{}, "next_page_token": "p"}, func(t *testing.T, got map[string]any) {
			if got["next_page_token"] != "p" {
				t.Fatalf("schedule list = %#v", got)
			}
		}},
		{"extension list", "agent.core.skills.list", map[string]any{"installations": []any{map[string]any{"id": "i1", "state": "installed"}}, "next_page_token": "p"}, func(t *testing.T, got map[string]any) {
			items := got["installations"].([]any)
			if items[0].(map[string]any)["id"] != "i1" || got["next_page_token"] != "p" {
				t.Fatalf("extension list = %#v", got)
			}
		}},
		{"chat", "agent.chat", canonicalChatResponseForTest("hello", nil, nil, nil), func(t *testing.T, got map[string]any) {
			if got["text"] != "hello" {
				t.Fatalf("chat = %#v", got)
			}
		}},
		{"web search config drops secrets", "agent.web_search.config.get", map[string]any{"enabled": true, "provider": "tavily", "api_key_configured": true, "api_key_hint": "tvly-secret-must-not-leak", "revision": float64(2), "tested_at": "tested", "updated_at": "updated", "api_key": "must-not-leak", "secret": "must-not-leak"}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"api_key_configured", "api_key_hint", "enabled", "provider", "revision", "tested_at", "updated_at"}) {
				t.Fatalf("web search config keys=%v value=%#v", keys, got)
			}
			if got["provider"] != "tavily" || got["api_key_hint"] != "configured" {
				t.Fatalf("web search config = %#v", got)
			}
		}},
		{"web search config omits unconfigured hint", "agent.web_search.config.get", map[string]any{"enabled": false, "provider": "tavily", "api_key_configured": false, "api_key_hint": "tvly-secret-must-not-leak", "revision": float64(0)}, func(t *testing.T, got map[string]any) {
			if _, exposed := got["api_key_hint"]; exposed {
				t.Fatalf("unconfigured web search exposed a hint: %#v", got)
			}
		}},
		{"web search test exact projection", "agent.web_search.test", map[string]any{"ok": true, "provider": "tavily", "result_count": float64(1), "tested_at": "tested", "enabled": true, "api_key_configured": true, "revision": float64(3), "api_key_hint": "drop", "provider_body": "drop"}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"api_key_configured", "enabled", "ok", "provider", "result_count", "revision", "tested_at"}) {
				t.Fatalf("web search test keys=%v value=%#v", keys, got)
			}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := adaptActionResult(test.action, test.input)
			if err != nil {
				t.Fatal(err)
			}
			test.check(t, got)
		})
	}
}

func TestConversationGetNormalizesCanonicalEmptyMessages(t *testing.T) {
	result, err := adaptActionResult("agent.chat.conversations.get", map[string]any{
		"conversation": map[string]any{"conversation_id": "953224b6-2357-47ae-af7b-9666ecef2da4"},
		"messages":     nil, "next_page_token": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	messages, ok := result["messages"].([]any)
	if !ok || messages == nil || len(messages) != 0 {
		t.Fatalf("empty conversation messages=%#v", result["messages"])
	}
	invalid := []map[string]any{
		{"conversation": map[string]any{}, "next_page_token": ""},
		{"conversation": map[string]any{}, "messages": map[string]any{}, "next_page_token": ""},
	}
	for _, output := range invalid {
		if _, err := adaptActionResult("agent.chat.conversations.get", output); !errors.Is(err, ErrInvalidActionResult) {
			t.Fatalf("invalid empty conversation response accepted: %#v err=%v", output, err)
		}
	}
}

func TestTaskActionsValidateAgentShapeAndProjectProductEnvelope(t *testing.T) {
	const taskID = "11111111-1111-4111-8111-111111111111"
	for _, action := range []string{
		"agent.core.tasks.get",
		"agent.core.tasks.cancel",
		"agent.core.tasks.retry",
	} {
		t.Run(action, func(t *testing.T) {
			got, err := adaptActionResult(action, map[string]any{
				"id": taskID, "status": "succeeded", "revision": float64(3),
			})
			if err != nil {
				t.Fatalf("valid unwrapped Agent task rejected: %v", err)
			}
			task, ok := got["task"].(map[string]any)
			if !ok || task["task_id"] != taskID || task["status"] != "succeeded" || task["revision"] != float64(3) {
				t.Fatalf("public task envelope = %#v", got)
			}
			if _, present := task["id"]; present {
				t.Fatalf("Agent task id escaped public projection: %#v", got)
			}
		})
	}
}

func TestCurrentAgentResultShapesRejectAlternateEnvelopes(t *testing.T) {
	validSource := map[string]any{"source_id": "source", "kind": "text", "status": "ready"}
	tests := []struct {
		action string
		value  map[string]any
	}{
		{"agent.model_profiles.get", map[string]any{"profile": map[string]any{"id": "profile"}}},
		{"agent.model_profiles.delete", map[string]any{"profile": map[string]any{"id": "profile"}}},
		{"agent.knowledge.sources.delete", validSource},
		{"agent.knowledge.upload.finish", map[string]any{"source": validSource}},
		{"agent.core.tasks.get", map[string]any{"task": map[string]any{"id": "task"}}},
		{"agent.core.schedules.get", map[string]any{"schedule_id": "schedule"}},
		{"agent.core.schedules.trigger", map[string]any{"schedule": map[string]any{}, "occurrence_id": "occurrence", "task_id": "task"}},
		{"agent.core.skills.get", map[string]any{"installation": map[string]any{"id": "installation"}}},
		{"agent.core.skills.install", map[string]any{"id": "installation"}},
		{"agent.core.aws.credentials.create", map[string]any{"credential_id": "credential"}},
		{"agent.core.skills.inspect", map[string]any{"candidate": map[string]any{}}},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			if _, err := adaptActionResult(test.action, test.value); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("alternate Agent result error = %v, want ErrInvalidActionResult", err)
			}
		})
	}
}

func TestKnowledgeStatusResultPreservesRequiredSnakeCaseFields(t *testing.T) {
	input := map[string]any{
		"ready_count":           float64(2),
		"uploading_count":       float64(1),
		"indexing_count":        float64(3),
		"failed_count":          float64(4),
		"cleanup_pending_count": float64(5),
		"checked_at":            "2026-08-08T12:00:00Z",
	}
	got := knowledgeStatusResult(input)
	if !reflect.DeepEqual(got, input) {
		t.Fatalf("knowledge status required fields = %#v, want %#v", got, input)
	}
}

func TestChatResultPromotesServerAuthoredExecutionLinkage(t *testing.T) {
	taskID := "11111111-1111-4111-8111-111111111111"
	planID := "22222222-2222-4222-8222-222222222222"
	reference := validExecutionReferenceForTest()
	metadata := map[string]any{
		"references":       []any{reference},
		"related_task_ids": []any{taskID},
		"related_plan_ids": []any{planID},
	}
	input := canonicalChatResponseForTest("done", metadata["references"].([]any), metadata["related_task_ids"].([]any), metadata["related_plan_ids"].([]any))
	got, err := adaptActionResultForRequestWithAuthority(
		"agent.chat", nil, input,
		actionResultAuthority{ownerID: "@owner:example.test", accountGeneration: 7},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got["text"] != "done" || !reflect.DeepEqual(got["references"], metadata["references"]) || !reflect.DeepEqual(got["related_task_ids"], metadata["related_task_ids"]) || !reflect.DeepEqual(got["related_plan_ids"], metadata["related_plan_ids"]) {
		t.Fatalf("chat projection = %#v", got)
	}

	got, err = adaptActionResult("agent.chat", canonicalChatResponseForTest("queued", nil, []any{taskID}, []any{planID}))
	if err != nil {
		t.Fatal(err)
	}
	if _, synthesized := got["references"]; synthesized {
		t.Fatalf("related ids synthesized an authority-bearing reference: %#v", got)
	}
}

func TestReasoningContentUsesExistingChatShapes(t *testing.T) {
	response := canonicalChatResponseForTest("answer", nil, nil, nil)
	response["message"].(map[string]any)["reasoning_content"] = "complete reasoning"
	got, err := adaptActionResult("agent.chat", response)
	if err != nil || got["reasoning_content"] != "complete reasoning" {
		t.Fatalf("chat reasoning projection = %#v, err %v", got, err)
	}

	invalidResponse := canonicalChatResponseForTest("answer", nil, nil, nil)
	invalidResponse["message"].(map[string]any)["reasoning_content"] = []any{"not", "a string"}
	if _, err := adaptActionResult("agent.chat", invalidResponse); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("non-string chat reasoning accepted: %v", err)
	}

	history := map[string]any{
		"conversation": map[string]any{"conversation_id": durableTestConversationID},
		"messages": []any{map[string]any{
			"role": "assistant", "content": "answer", "reasoning_content": "complete reasoning", "references": []any{},
		}},
	}
	projectedHistory, err := adaptActionResult("agent.chat.conversations.get", history)
	if err != nil {
		t.Fatal(err)
	}
	messages := projectedHistory["messages"].([]any)
	if messages[0].(map[string]any)["reasoning_content"] != "complete reasoning" {
		t.Fatalf("history reasoning projection = %#v", projectedHistory)
	}
	for name, message := range map[string]map[string]any{
		"wrong type": {"role": "assistant", "reasoning_content": true, "references": []any{}},
		"user role":  {"role": "user", "reasoning_content": "private", "references": []any{}},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := map[string]any{"conversation": map[string]any{}, "messages": []any{message}}
			if _, err := adaptActionResult("agent.chat.conversations.get", candidate); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid history reasoning accepted: %v", err)
			}
		})
	}

	delta := map[string]any{
		"kind": "delta", "idempotency_key": durableTestStartID,
		"conversation_id": durableTestConversationID, "turn_id": durableTestTurnID,
		"revision": float64(2), "reasoning_content": "partial reasoning",
	}
	if err := validateChatStreamEvent(delta, actionResultAuthority{}); err != nil {
		t.Fatalf("reasoning-only delta rejected: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"wrong type": func(event map[string]any) { event["reasoning_content"] = 1 },
		"wrong kind": func(event map[string]any) { event["kind"] = "started" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := maps.Clone(delta)
			mutate(candidate)
			if err := validateChatStreamEvent(candidate, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid stream reasoning accepted: %v", err)
			}
		})
	}
}

func TestChatResultRejectsAlternateDuplicateAndConflictingLocations(t *testing.T) {
	taskID := "11111111-1111-4111-8111-111111111111"
	canonical := canonicalChatResponseForTest("done", nil, []any{taskID}, nil)
	conflict := canonicalChatResponseForTest("done", nil, []any{taskID}, nil)
	conflict["message"].(map[string]any)["related_task_ids"] = []any{"22222222-2222-4222-8222-222222222222"}
	for name, input := range map[string]map[string]any{
		"top-level text":       {"text": "done", "message": map[string]any{"content": "done"}},
		"Pascal message":       {"Message": map[string]any{"content": "done"}},
		"response wrapper":     {"response": canonical},
		"message-only linkage": {"message": map[string]any{"content": "done", "related_task_ids": []any{taskID}}},
		"conflicting copies":   conflict,
		"unknown root field":   {"message": map[string]any{"content": "done"}, "done": true, "metadata": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := adaptActionResult("agent.chat", input); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("non-canonical chat shape accepted: %v", err)
			}
		})
	}
	streamDuplicate := map[string]any{
		"kind": "done", "text": "done", "response": canonicalChatResponseForTest("done", nil, nil, nil),
	}
	if err := promoteChatResultFields(streamDuplicate, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("duplicate stream/result text accepted: %v", err)
	}
	for name, event := range map[string]map[string]any{
		"done without response": {"kind": "done"},
		"delta with response":   {"kind": "delta", "response": canonicalChatResponseForTest("done", nil, nil, nil)},
		"unknown kind":          {"kind": "complete"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := validateChatStreamEvent(event, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("non-canonical stream event accepted: %v", err)
			}
		})
	}
}

func TestValidateChatStreamWaitingConfirmationRequiresExactAuthority(t *testing.T) {
	event := map[string]any{
		"kind":            "waiting_confirmation",
		"idempotency_key": durableTestStartID,
		"conversation_id": durableTestConversationID,
		"turn_id":         durableTestTurnID,
		"revision":        float64(3),
		"sequence":        int64(4),
		"confirmation_id": "11111111-1111-4111-8111-111111111111",
		"execution_id":    "33333333-3333-4333-8333-333333333333",
		"status":          "waiting_confirmation",
	}
	if err := validateChatStreamEvent(event, actionResultAuthority{}); err != nil {
		t.Fatalf("waiting confirmation event rejected: %v", err)
	}

	for _, field := range []string{"confirmation_id", "execution_id", "status"} {
		t.Run("missing_"+field, func(t *testing.T) {
			invalid := maps.Clone(event)
			delete(invalid, field)
			if err := validateChatStreamEvent(invalid, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("missing %s accepted: %v", field, err)
			}
		})
	}
	t.Run("superseded attempt authority", func(t *testing.T) {
		invalid := maps.Clone(event)
		invalid["attempt_id"] = "22222222-2222-4222-8222-222222222222"
		if err := validateChatStreamEvent(invalid, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
			t.Fatalf("superseded attempt_id accepted: %v", err)
		}
	})
	for field, value := range map[string]any{
		"text": "not allowed", "tool_call": map[string]any{}, "tool_result": map[string]any{},
		"response": map[string]any{}, "error_code": "not_allowed", "error_summary": "not allowed",
		"created_at": "2026-08-15T03:13:30Z",
	} {
		t.Run("mixed_"+field, func(t *testing.T) {
			invalid := maps.Clone(event)
			invalid[field] = value
			if err := validateChatStreamEvent(invalid, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("mixed waiting field %s accepted: %v", field, err)
			}
		})
	}

	for _, kind := range []string{"accepted", "started", "delta", "tool_call", "tool_result", "worker_status", "done", "error"} {
		t.Run("authority_on_"+kind, func(t *testing.T) {
			invalid := maps.Clone(event)
			invalid["kind"] = kind
			if kind == "done" {
				invalid["response"] = durableChatResponseForTest("done", nil, nil, nil)
			}
			if err := validateChatStreamEvent(invalid, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("confirmation authority on %s accepted: %v", kind, err)
			}
		})
	}
}

func TestValidateChatStreamWorkerStatusRequiresExactAuthority(t *testing.T) {
	event := map[string]any{
		"kind":            "worker_status",
		"idempotency_key": durableTestStartID,
		"conversation_id": durableTestConversationID,
		"turn_id":         durableTestTurnID,
		"revision":        float64(4),
		"sequence":        int64(5),
		"execution_id":    "33333333-3333-4333-8333-333333333333",
		"status":          "queued",
		"created_at":      "2026-08-15T03:13:30.123Z",
	}
	for _, status := range []string{"queued", "provisioning", "running", "succeeded", "failed", "canceled", "rejected", "expired"} {
		candidate := maps.Clone(event)
		candidate["status"] = status
		if err := validateChatStreamEvent(candidate, actionResultAuthority{}); err != nil {
			t.Fatalf("worker status %s rejected: %v", status, err)
		}
	}

	for name, mutate := range map[string]func(map[string]any){
		"missing execution": func(value map[string]any) { delete(value, "execution_id") },
		"invalid execution": func(value map[string]any) { value["execution_id"] = "not-a-uuid" },
		"missing status":    func(value map[string]any) { delete(value, "status") },
		"unknown status":    func(value map[string]any) { value["status"] = "stopping" },
		"missing timestamp": func(value map[string]any) { delete(value, "created_at") },
		"invalid timestamp": func(value map[string]any) { value["created_at"] = "yesterday" },
		"confirmation": func(value map[string]any) {
			value["confirmation_id"] = "11111111-1111-4111-8111-111111111111"
		},
		"mixed text": func(value map[string]any) { value["text"] = "not allowed" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := maps.Clone(event)
			mutate(candidate)
			if err := validateChatStreamEvent(candidate, actionResultAuthority{}); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid worker status accepted: %v", err)
			}
		})
	}
}

func TestChatResultKeepsGenericExecutionAndServiceBindingReferencesInformational(t *testing.T) {
	digest := strings.Repeat("a", 64)
	references := []any{
		map[string]any{"kind": "execution_plan", "plan_id": "11111111-1111-4111-8111-111111111111", "plan_revision": float64(4), "plan_digest": digest},
		map[string]any{
			"kind": "execution_run", "run_id": "22222222-2222-4222-8222-222222222222", "run_revision": float64(2), "run_digest": digest,
			"plan_id": "11111111-1111-4111-8111-111111111111", "plan_revision": float64(4), "plan_digest": digest,
			"deployment_id": "77777777-7777-4777-8777-777777777777", "status": "waiting_user",
		},
		map[string]any{
			"kind": "execution_confirmation", "confirmation_id": "55555555-5555-4555-8555-555555555555",
			"plan_id": "11111111-1111-4111-8111-111111111111", "plan_revision": float64(4), "plan_digest": digest,
			"run_id": "22222222-2222-4222-8222-222222222222", "run_revision": float64(2), "run_digest": digest,
			"stage_id": "33333333-3333-4333-8333-333333333333", "stage_revision": float64(1), "stage_digest": digest,
			"target_id": "44444444-4444-4444-8444-444444444444", "target_revision": float64(3), "target_digest": digest,
		},
		map[string]any{
			"kind": "service_binding", "binding_id": "66666666-6666-4666-8666-666666666666", "binding_revision": float64(2), "binding_digest": digest,
			"deployment_id": "77777777-7777-4777-8777-777777777777", "project_id": "88888888-8888-4888-8888-888888888888",
			"run_id": "22222222-2222-4222-8222-222222222222", "target_id": "44444444-4444-4444-8444-444444444444",
			"target_revision": float64(3), "target_digest": digest,
		},
	}
	got, err := adaptActionResult("agent.chat", canonicalChatResponseForTest("done", references, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["references"], references) {
		t.Fatalf("generic references changed: %#v", got)
	}
}

func TestChatResultRejectsCloudWorkerReferenceFromForeignGeneration(t *testing.T) {
	reference := validExecutionReferenceForTest()
	reference["account_generation"] = float64(8)
	_, err := adaptActionResultForRequestWithAuthority(
		"agent.chat", nil, canonicalChatResponseForTest("done", []any{reference}, nil, nil),
		actionResultAuthority{ownerID: "@owner:example.test", accountGeneration: 7},
	)
	if !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("foreign generation err=%v", err)
	}
}

func TestChatAndHistoryPreserveExecutionArtifactReference(t *testing.T) {
	reference := validExecutionArtifactReferenceForTest()
	authority := actionResultAuthority{ownerID: "@owner:example.test", accountGeneration: 7}
	chat, err := adaptActionResultForRequestWithAuthority(
		"agent.chat", nil, canonicalChatResponseForTest("done", []any{reference}, nil, nil), authority,
	)
	if err != nil || !reflect.DeepEqual(chat["references"], []any{reference}) {
		t.Fatalf("artifact chat projection=%#v err=%v", chat, err)
	}
	historyResult := map[string]any{
		"conversation": map[string]any{"conversation_id": "22222222-2222-4222-8222-222222222222"},
		"messages": []any{map[string]any{
			"message_id": "33333333-3333-4333-8333-333333333333", "role": "assistant", "content": "done",
			"created_at": "2026-08-15T00:00:00Z", "message_seq": float64(2), "status": "done", "references": []any{reference},
		}},
		"next_page_token": "",
	}
	history, err := adaptActionResultForRequestWithAuthority("agent.chat.conversations.get", nil, historyResult, authority)
	if err != nil {
		t.Fatal(err)
	}
	messages := history["messages"].([]any)
	if !reflect.DeepEqual(messages[0].(map[string]any)["references"], []any{reference}) {
		t.Fatalf("artifact history projection=%#v", history)
	}
}

func TestExecutionArtifactReferenceRejectsShapeAndAuthorityDrift(t *testing.T) {
	authority := actionResultAuthority{ownerID: "@owner:example.test", accountGeneration: 7}
	valid := validExecutionArtifactReferenceForTest()
	for name, mutate := range map[string]func(map[string]any){
		"foreign generation": func(value map[string]any) { value["account_generation"] = float64(8) },
		"cloud worker kind":  func(value map[string]any) { value["record_kind"] = "cloud_worker" },
		"missing size":       func(value map[string]any) { delete(value, "size_bytes") },
		"unsafe name":        func(value map[string]any) { value["name"] = "../secret" },
		"bad media type":     func(value map[string]any) { value["media_type"] = "text/html\r\nsecret" },
		"oversize":           func(value map[string]any) { value["size_bytes"] = float64((64 << 20) + 1) },
		"bad digest":         func(value map[string]any) { value["sha256"] = "ABC" },
		"cross-kind field":   func(value map[string]any) { value["task_id"] = "44444444-4444-4444-8444-444444444444" },
	} {
		t.Run(name, func(t *testing.T) {
			reference := withReferenceField(valid, "kind", valid["kind"])
			mutate(reference)
			_, err := adaptActionResultForRequestWithAuthority(
				"agent.chat", nil, canonicalChatResponseForTest("done", []any{reference}, nil, nil), authority,
			)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid artifact reference accepted: %v", err)
			}
		})
	}
}

func TestChatResultRejectsMalformedReferencesAndRelatedIDs(t *testing.T) {
	valid := validExecutionReferenceForTest()
	tests := []struct {
		name  string
		input map[string]any
	}{
		{name: "related task is not canonical", input: map[string]any{"related_task_ids": []any{"TASK-1"}}},
		{name: "unknown reference kind", input: map[string]any{"references": []any{map[string]any{"kind": "service_binding"}}}},
		{name: "room lacks identity", input: map[string]any{"references": []any{map[string]any{"kind": "room"}}}},
		{name: "channel post lacks post", input: map[string]any{"references": []any{map[string]any{"kind": "channel_post", "room_id": "!r:example", "channel_id": "!r:example"}}}},
		{name: "execution is incomplete", input: map[string]any{"references": []any{map[string]any{"kind": "execution_plan", "task_id": "11111111-1111-4111-8111-111111111111"}}}},
		{name: "execution has unknown field", input: map[string]any{"references": []any{withReferenceField(valid, "action", "confirm")}}},
		{name: "retired Cloud Worker digest", input: map[string]any{"references": []any{withReferenceField(valid, "quote_digest", strings.Repeat("a", 64))}}},
		{name: "duplicate reference", input: map[string]any{"references": []any{valid, valid}}},
		{name: "semantic duplicate with explicit empty field", input: map[string]any{"references": []any{map[string]any{"kind": "room", "room_id": "!room:example"}, map[string]any{"kind": "room", "room_id": "!room:example", "title": ""}}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := canonicalChatResponseForTest("done", nil, nil, nil)
			for field, value := range test.input {
				input[field] = value
				if field == "references" || field == "related_task_ids" || field == "related_plan_ids" {
					input["message"].(map[string]any)[field] = value
				}
			}
			_, err := adaptActionResult("agent.chat", input)
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("error = %v, want ErrInvalidActionResult", err)
			}
		})
	}
}

func validExecutionReferenceForTest() map[string]any {
	return map[string]any{
		"kind":               "execution_plan",
		"account_generation": float64(7),
		"task_id":            "11111111-1111-4111-8111-111111111111",
		"plan_id":            "22222222-2222-4222-8222-222222222222",
		"plan_revision":      float64(3),
		"status":             "waiting_user",
	}
}

func validExecutionArtifactReferenceForTest() map[string]any {
	return map[string]any{
		"kind": "execution_artifact", "account_generation": float64(7), "record_kind": "local_sandbox",
		"artifact_id": "55555555-5555-4555-8555-555555555555", "execution_id": "66666666-6666-4666-8666-666666666666",
		"name": "report/index.html", "media_type": "text/html", "size_bytes": float64(0), "sha256": strings.Repeat("a", 64),
	}
}

func withReferenceField(source map[string]any, key string, value any) map[string]any {
	clone := make(map[string]any, len(source)+1)
	for name, item := range source {
		clone[name] = item
	}
	clone[key] = value
	return clone
}

func TestModelCatalogResultAdapterDropsUnknownModelFieldsAtGatewayBoundary(t *testing.T) {
	got := modelCatalogResult(map[string]any{
		"models": []any{map[string]any{
			"id":                  "model-1",
			"name":                "Model 1",
			"provider":            "openrouter",
			"context_length":      float64(128000),
			"max_output_tokens":   float64(4096),
			"input_modalities":    []any{"text"},
			"output_modalities":   []any{"text"},
			"API_KEY":             "canary-key",
			"Authorization":       "Bearer canary-key",
			"nested":              map[string]any{"api_key": "canary-key"},
			"unknown_alias_field": "drop-me",
		}},
		"providers": []any{},
	})
	models := got["models"].([]any)
	if len(models) != 1 {
		t.Fatalf("model catalog models = %#v", got)
	}
	model := models[0].(map[string]any)
	wantKeys := []string{"context_length", "id", "input_modalities", "max_output_tokens", "name", "output_modalities", "provider"}
	if keys := sortedMapKeys(model); !reflect.DeepEqual(keys, wantKeys) {
		t.Fatalf("gateway model projection keys=%v value=%#v", keys, model)
	}
	if model["id"] != "model-1" || model["name"] != "Model 1" || model["provider"] != "openrouter" {
		t.Fatalf("gateway model canonical fields = %#v", model)
	}
}

func TestTurnsListResultPublishesOnlyCanonicalMetadata(t *testing.T) {
	input := map[string]any{
		"turns": []any{map[string]any{
			"turn_id":          "11111111-1111-4111-8111-111111111111",
			"idempotency_key":  "33333333-3333-4333-8333-333333333333",
			"conversation_id":  "22222222-2222-4222-8222-222222222222",
			"state":            "completed",
			"revision":         float64(3),
			"last_sequence":    float64(7),
			"terminal_code":    "",
			"terminal_summary": "",
			"created_at":       "2026-08-06T01:02:03Z",
			"updated_at":       "2026-08-06T01:02:04.123Z",
		}},
		"next_page_token": "next",
	}
	got, err := adaptActionResult("agent.chat.turns.list", input)
	if err != nil {
		t.Fatal(err)
	}
	if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"next_cursor", "turns"}) {
		t.Fatalf("turn list keys=%v value=%#v", keys, got)
	}
	turn := got["turns"].([]any)[0].(map[string]any)
	if keys := sortedMapKeys(turn); !reflect.DeepEqual(keys, []string{"conversation_id", "created_at", "idempotency_key", "last_sequence", "revision", "state", "terminal_code", "terminal_summary", "turn_id", "updated_at"}) {
		t.Fatalf("turn keys=%v value=%#v", keys, turn)
	}
}

func TestTurnsListResultRejectsAliasesLeaksAndMalformedMetadata(t *testing.T) {
	valid := map[string]any{
		"turn_id":          "11111111-1111-4111-8111-111111111111",
		"idempotency_key":  "33333333-3333-4333-8333-333333333333",
		"conversation_id":  "22222222-2222-4222-8222-222222222222",
		"state":            "running",
		"revision":         float64(2),
		"last_sequence":    float64(0),
		"terminal_code":    "",
		"terminal_summary": "",
		"created_at":       "2026-08-06T01:02:03Z",
		"updated_at":       "2026-08-06T01:02:03Z",
	}
	for name, mutate := range map[string]func(map[string]any){
		"Go alias":      func(turn map[string]any) { delete(turn, "turn_id"); turn["ID"] = valid["turn_id"] },
		"prompt leak":   func(turn map[string]any) { turn["prompt"] = "must not cross" },
		"status alias":  func(turn map[string]any) { delete(turn, "state"); turn["status"] = "running" },
		"bad UUID":      func(turn map[string]any) { turn["turn_id"] = "turn-1" },
		"bad start key": func(turn map[string]any) { turn["idempotency_key"] = "request-1" },
		"bad state":     func(turn map[string]any) { turn["state"] = "unknown" },
		"zero revision": func(turn map[string]any) { turn["revision"] = float64(0) },
		"negative seq":  func(turn map[string]any) { turn["last_sequence"] = float64(-1) },
		"bad timestamp": func(turn map[string]any) { turn["updated_at"] = "today" },
		"time reversal": func(turn map[string]any) { turn["updated_at"] = "2026-08-05T01:02:03Z" },
	} {
		t.Run(name, func(t *testing.T) {
			turn := cloneParams(valid)
			mutate(turn)
			_, err := adaptActionResult("agent.chat.turns.list", map[string]any{"turns": []any{turn}, "next_page_token": ""})
			if !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("error=%v, want ErrInvalidActionResult", err)
			}
		})
	}
}

func TestTurnStopResultPublishesExactAuthoritativeMetadata(t *testing.T) {
	const turnID = "11111111-1111-4111-8111-111111111111"
	result := map[string]any{
		"turn_id":          turnID,
		"idempotency_key":  "22222222-2222-4222-8222-222222222222",
		"conversation_id":  "33333333-3333-4333-8333-333333333333",
		"state":            "canceled",
		"revision":         float64(3),
		"last_sequence":    float64(5),
		"terminal_code":    "canceled",
		"terminal_summary": "canceled by owner",
		"created_at":       "2026-08-06T01:02:03Z",
		"updated_at":       "2026-08-06T01:02:04Z",
	}
	got, err := adaptActionResultForRequest("agent.chat.turn.stop", map[string]any{
		"idempotency_key":   "44444444-4444-4444-8444-444444444444",
		"turn_id":           turnID,
		"expected_revision": float64(2),
	}, result)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, result) {
		t.Fatalf("stop result = %#v, want exact metadata %#v", got, result)
	}

	for name, mutate := range map[string]func(map[string]any){
		"wrong turn": func(value map[string]any) { value["turn_id"] = "55555555-5555-4555-8555-555555555555" },
		"leak":       func(value map[string]any) { value["prompt"] = "must not cross" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneParams(result)
			mutate(candidate)
			if _, err := adaptActionResultForRequest("agent.chat.turn.stop", map[string]any{"turn_id": turnID}, candidate); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("error=%v, want ErrInvalidActionResult", err)
			}
		})
	}
}

func TestTurnSteerResultPublishesAuthoritativeTurnAndMutationReceipt(t *testing.T) {
	const turnID = "11111111-1111-4111-8111-111111111111"
	const startID = "22222222-2222-4222-8222-222222222222"
	const steerID = "44444444-4444-4444-8444-444444444444"
	result := map[string]any{
		"turn_id": turnID, "idempotency_key": startID,
		"steer_idempotency_key": steerID,
		"conversation_id":       "33333333-3333-4333-8333-333333333333",
		"state":                 "accepted", "revision": float64(3), "last_sequence": float64(5),
		"terminal_code": "", "terminal_summary": "",
		"created_at": "2026-08-06T01:02:03Z", "updated_at": "2026-08-06T01:02:04Z",
	}
	request := map[string]any{
		"idempotency_key": steerID, "turn_id": turnID,
		"expected_revision": float64(2), "instruction": "guide now",
	}
	got, err := adaptActionResultForRequest("agent.chat.turn.steer", request, result)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, result) {
		t.Fatalf("steer result = %#v, want %#v", got, result)
	}
	waiting := cloneParams(result)
	waiting["state"] = "waiting_confirmation"
	if _, err := adaptActionResultForRequest("agent.chat.turn.steer", request, waiting); err != nil {
		t.Fatalf("waiting-confirmation steer result rejected: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"wrong receipt": func(value map[string]any) { value["steer_idempotency_key"] = "55555555-5555-4555-8555-555555555555" },
		"terminal":      func(value map[string]any) { value["state"] = "completed" },
		"leak":          func(value map[string]any) { value["instruction"] = "must not cross" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneParams(result)
			mutate(candidate)
			if _, err := adaptActionResultForRequest("agent.chat.turn.steer", request, candidate); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("error=%v, want ErrInvalidActionResult", err)
			}
		})
	}
}

func TestModelCatalogResultAdapterRejectsEntriesThatViolatePublishedSchema(t *testing.T) {
	_, err := adaptActionResult("agent.models.list", map[string]any{
		"models": []any{
			map[string]any{"id": "valid", "provider": "openrouter", "name": "Valid", "context_length": float64(128000), "input_modalities": []any{"text"}},
			map[string]any{"id": "missing-provider"},
			map[string]any{"id": float64(7), "provider": "openrouter"},
			map[string]any{"id": "bad-provider", "provider": true},
			map[string]any{"id": "bad-name", "provider": "openrouter", "name": float64(1)},
			map[string]any{"id": "fractional", "provider": "openrouter", "context_length": 1.5},
			map[string]any{"id": "bad-modalities", "provider": "openrouter", "input_modalities": []any{"text", float64(1)}},
			"not-an-object",
		},
		"providers": []any{
			map[string]any{"provider": "openrouter", "default_base_url": "https://openrouter.ai/api/v1", "requires_api_key": true, "dynamic_models": true},
			map[string]any{"provider": "missing-dynamic", "requires_api_key": true},
			map[string]any{"provider": float64(1), "requires_api_key": true, "dynamic_models": true},
			map[string]any{"provider": "bad-bool", "requires_api_key": "true", "dynamic_models": true},
			map[string]any{"provider": "bad-url", "default_base_url": true, "requires_api_key": true, "dynamic_models": true},
		},
	})
	if !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("schema-invalid catalog error = %v, want ErrInvalidActionResult", err)
	}
}

func TestWebSearchResultAdapterRejectsMissingOrWrongTypedFields(t *testing.T) {
	validConfig := map[string]any{
		"enabled": true, "provider": "tavily", "api_key_configured": true,
		"revision": float64(2), "tested_at": "2026-08-05T00:00:00Z",
	}
	if _, err := adaptActionResult("agent.web_search.config.get", validConfig); err != nil {
		t.Fatalf("valid web-search config rejected: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing enabled": func(value map[string]any) { delete(value, "enabled") },
		"wrong provider":  func(value map[string]any) { value["provider"] = "other" },
		"wrong revision":  func(value map[string]any) { value["revision"] = "2" },
		"wrong configured": func(value map[string]any) {
			value["api_key_configured"] = "true"
		},
	} {
		t.Run(name, func(t *testing.T) {
			value := cloneParams(validConfig)
			mutate(value)
			if _, err := adaptActionResult("agent.web_search.config.update", value); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid web-search config error = %v, want ErrInvalidActionResult", err)
			}
		})
	}
	validTest := map[string]any{
		"ok": true, "provider": "tavily", "result_count": float64(1),
		"tested_at": "2026-08-05T00:00:00Z", "enabled": true,
		"api_key_configured": true, "revision": float64(3),
	}
	if _, err := adaptActionResult("agent.web_search.test", validTest); err != nil {
		t.Fatalf("valid web-search test rejected: %v", err)
	}
	delete(validTest, "tested_at")
	if _, err := adaptActionResult("agent.web_search.test", validTest); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("web-search test missing tested_at error = %v, want ErrInvalidActionResult", err)
	}
}

func TestKnowledgeStatusResultAdapterRequiresCanonicalQuotaCounters(t *testing.T) {
	valid := map[string]any{
		"quota_used_bytes": float64(1024), "quota_limit_bytes": float64(67108864),
		"quota_remaining_bytes": float64(67107840), "max_source_bytes": float64(16777216),
	}
	if _, err := adaptActionResult("agent.knowledge.status", valid); err != nil {
		t.Fatalf("valid knowledge status rejected: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing used":   func(value map[string]any) { delete(value, "quota_used_bytes") },
		"fraction limit": func(value map[string]any) { value["quota_limit_bytes"] = 1.5 },
		"negative max":   func(value map[string]any) { value["max_source_bytes"] = -1 },
		"wrong fixed limits": func(value map[string]any) {
			value["quota_limit_bytes"] = float64(33554432)
			value["quota_remaining_bytes"] = float64(33553408)
			value["max_source_bytes"] = float64(8388608)
		},
		"used exceeds limit":     func(value map[string]any) { value["quota_used_bytes"] = float64(67108865) },
		"remaining inconsistent": func(value map[string]any) { value["quota_remaining_bytes"] = float64(1) },
		"source exceeds limit":   func(value map[string]any) { value["max_source_bytes"] = float64(67108865) },
	} {
		t.Run(name, func(t *testing.T) {
			value := cloneParams(valid)
			mutate(value)
			if _, err := adaptActionResult("agent.knowledge.status", value); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid knowledge status error = %v, want ErrInvalidActionResult", err)
			}
		})
	}
}

func TestCoreMCPNodeReceiptUsesOnlyPublishedProtoContract(t *testing.T) {
	valid := validManagedNodeMCPInstallationResult(true)
	projected, err := adaptActionResult("agent.core.mcp.get", valid)
	if err != nil {
		t.Fatalf("valid managed Node MCP installation rejected: %v", err)
	}
	installation := projected["installation"].(map[string]any)
	version := installation["versions"].([]any)[0].(map[string]any)
	receipt := version["node_artifact"].(map[string]any)
	if len(receipt) != 8 || receipt["node_version"] != managedNodeVersion || receipt["npm_version"] != managedNPMVersion {
		t.Fatalf("public Node receipt = %#v", receipt)
	}
	for _, internal := range []string{"input_digest", "artifact_digest", "entry_path", "entry_sha256", "lock_sha256"} {
		if _, leaked := receipt[internal]; leaked {
			t.Fatalf("internal Node receipt field %q escaped projection: %#v", internal, receipt)
		}
	}
	for _, internal := range []string{"artifact_path", "artifact_digest", "artifact_cleanup_token", "published_at"} {
		if _, leaked := version[internal]; leaked {
			t.Fatalf("internal extension version field %q escaped projection: %#v", internal, version)
		}
	}

	unpublished := validManagedNodeMCPInstallationResult(false)
	if _, err := adaptActionResult("agent.core.mcp.get", unpublished); err != nil {
		t.Fatalf("unpublished proposed Node version without receipt rejected: %v", err)
	}

	invalid := map[string]func(map[string]any){
		"internal version artifact digest": func(value map[string]any) {
			value["versions"].([]any)[0].(map[string]any)["artifact_digest"] = strings.Repeat("b", 64)
		},
		"internal publication timestamp": func(value map[string]any) {
			value["versions"].([]any)[0].(map[string]any)["published_at"] = "2026-08-11T00:01:00Z"
		},
		"internal digest field": func(value map[string]any) {
			nodeReceiptFromInstallation(value)["artifact_digest"] = strings.Repeat("b", 64)
		},
		"scripts not proven disabled": func(value map[string]any) {
			nodeReceiptFromInstallation(value)["lifecycle_scripts_disabled"] = false
		},
		"legacy scripts absent field": func(value map[string]any) {
			receipt := nodeReceiptFromInstallation(value)
			delete(receipt, "lifecycle_scripts_disabled")
			receipt["lifecycle_scripts_absent"] = true
		},
		"package version mismatch": func(value map[string]any) {
			nodeReceiptFromInstallation(value)["package_version"] = "1.2.4"
		},
		"wrong managed Node version": func(value map[string]any) {
			nodeReceiptFromInstallation(value)["node_version"] = "v22.0.0"
		},
		"single artifact exceeds limit": func(value map[string]any) {
			nodeReceiptFromInstallation(value)["artifact_bytes"] = float64((64 << 20) + 1)
		},
		"single artifact file count exceeds limit": func(value map[string]any) {
			nodeReceiptFromInstallation(value)["file_count"] = float64(8193)
		},
		"receipt on static MCP": func(value map[string]any) { value["transport"] = "stdio_static" },
		"active receipt missing": func(value map[string]any) {
			delete(value["versions"].([]any)[0].(map[string]any), "node_artifact")
		},
	}
	for name, mutate := range invalid {
		t.Run(name, func(t *testing.T) {
			value := validManagedNodeMCPInstallationResult(true)
			mutate(value)
			if _, err := adaptActionResult("agent.core.mcp.get", value); !errors.Is(err, ErrInvalidActionResult) {
				t.Fatalf("invalid Node receipt error = %v, want ErrInvalidActionResult", err)
			}
		})
	}
}

func validManagedNodeMCPInstallationResult(published bool) map[string]any {
	digest := strings.Repeat("a", 64)
	versionID := "33333333-3333-4333-8333-333333333333"
	version := map[string]any{
		"version_id":     versionID,
		"pin":            map[string]any{"registry_version": "1.2.3", "registry_sha256": digest},
		"content_digest": digest, "manifest_digest": digest, "execution_digest": digest,
		"network_schema_digest": digest, "secret_schema_digest": digest,
		"execution":  map[string]any{"stdio": map[string]any{"relative_path": "node_modules/@modelcontextprotocol/server-filesystem/dist/index.js", "digest": digest, "argv": []any{}, "runtime": "node"}},
		"created_at": "2026-08-11T00:00:00Z",
	}
	installation := map[string]any{
		"id": "22222222-2222-4222-8222-222222222222", "kind": "mcp", "source": "npm",
		"candidate_id": "@modelcontextprotocol/server-filesystem", "name": "Filesystem MCP", "transport": "stdio_node",
		"revision": float64(2), "state": "installing", "enabled": false, "proposed_version_id": versionID,
		"versions": []any{version}, "created_at": "2026-08-11T00:00:00Z", "updated_at": "2026-08-11T00:00:00Z",
	}
	if published {
		installation["state"] = "installed"
		installation["enabled"] = true
		installation["active_version_id"] = versionID
		delete(installation, "proposed_version_id")
		version["node_artifact"] = map[string]any{
			"package_name": "@modelcontextprotocol/server-filesystem", "package_version": "1.2.3",
			"artifact_bytes": float64(4096), "file_count": float64(12),
			"node_version": managedNodeVersion, "npm_version": managedNPMVersion,
			"lifecycle_scripts_disabled": true, "native_addons_absent": true,
		}
	}
	return installation
}

func nodeReceiptFromInstallation(value map[string]any) map[string]any {
	return value["versions"].([]any)[0].(map[string]any)["node_artifact"].(map[string]any)
}

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestProductCoreInputIsTranslatedBeforeAgentDigest(t *testing.T) {
	input := map[string]any{"confirm": "deprovision_account"}
	translateProductCoreInput("agent.account.deprovision", input)
	if input["confirmation"] != "deprovision_account" {
		t.Fatalf("account confirmation alias = %#v", input)
	}
	if _, ok := input["confirm"]; ok {
		t.Fatalf("ProductCore account confirm must not reach the Agent schema: %#v", input)
	}
	input = map[string]any{"message_limit": float64(20), "message_cursor": "cursor"}
	translateProductCoreInput("agent.chat.conversations.get", input)
	if input["limit"] != float64(20) || input["page_token"] != "cursor" {
		t.Fatalf("translated conversation input = %#v", input)
	}
	input = map[string]any{"page_size": float64(25)}
	translateProductCoreInput("agent.knowledge.search", input)
	if input["limit"] != float64(25) || input["page_size"] != nil {
		t.Fatalf("translated knowledge search input = %#v", input)
	}
}
