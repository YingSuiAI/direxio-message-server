package agentgateway

import (
	"errors"
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

func TestPublicResultAdaptersPreserveLegacyEnvelopes(t *testing.T) {
	tests := []struct {
		name   string
		action string
		input  map[string]any
		check  func(*testing.T, map[string]any)
	}{
		{"conversation create", "agent.chat.conversations.create", map[string]any{"ID": "c1", "Title": "Chat"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["conversation"]; !ok || got["replayed"] != false {
				t.Fatalf("conversation create = %#v", got)
			}
		}},
		{"conversation get", "agent.chat.conversations.get", map[string]any{"ID": "c1", "Messages": []any{map[string]any{"role": "user"}}, "NextCursor": "next"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["conversation"]; !ok || got["next_cursor"] != "next" || len(got["messages"].([]any)) != 1 {
				t.Fatalf("conversation get = %#v", got)
			}
		}},
		{"conversation list", "agent.chat.conversations.list", map[string]any{"Conversations": []any{}, "next_page_token": "p"}, func(t *testing.T, got map[string]any) {
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
		{"model list", "agent.model_profiles.list", map[string]any{"Profiles": []any{}, "NextCursor": "p", "default_tool_client_profile_id": ""}, func(t *testing.T, got map[string]any) {
			if _, ok := got["profiles"]; !ok || got["next_page_token"] != "p" || got["default_tool_client_profile_id"] != "" {
				t.Fatalf("model list = %#v", got)
			}
		}},
		{"provider model catalog", "agent.models.list", map[string]any{
			"Models":    []any{map[string]any{"ID": "openai/gpt-4o", "Name": "GPT-4o", "Provider": "openrouter", "InputModalities": []any{"text", "image"}, "pricing": map[string]any{"prompt": "1"}}},
			"Providers": []any{map[string]any{"Provider": "openrouter", "DefaultBaseURL": "https://openrouter.ai/api/v1", "RequiresAPIKey": true, "DynamicModels": true}},
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
		{"source list", "agent.knowledge.sources.list", map[string]any{"Sources": []any{map[string]any{"ID": "s1", "SizeBytes": float64(4)}}, "NextPageToken": "p"}, func(t *testing.T, got map[string]any) {
			items := got["sources"].([]any)
			if items[0].(map[string]any)["source_id"] != "s1" || got["next_page_token"] != "p" {
				t.Fatalf("source list = %#v", got)
			}
		}},
		{"memory list", "agent.knowledge.memories.list", map[string]any{"Sources": []any{map[string]any{"ID": "m1", "Content": "hello"}}, "NextPageToken": "p"}, func(t *testing.T, got map[string]any) {
			items := got["items"].([]any)
			if items[0].(map[string]any)["memory_id"] != "m1" || items[0].(map[string]any)["content"] != "hello" || got["next_page_token"] != "p" {
				t.Fatalf("memory list = %#v", got)
			}
		}},
		{"knowledge search", "agent.knowledge.search", map[string]any{"Matches": []any{map[string]any{"SourceID": "s1", "Score": float64(.9)}}, "NextPageToken": "p", "EmbeddingProfileID": "embed", "EmbeddingProfileRevision": float64(4), "EmbeddingModel": "text-embedding-3-small", "EmbeddingGeneration": "gen-7", "CollectionConfigDigest": "digest-8"}, func(t *testing.T, got map[string]any) {
			items := got["items"].([]any)
			if items[0].(map[string]any)["source_id"] != "s1" || got["next_cursor"] != "p" || got["embedding_profile_id"] != "embed" || got["embedding_profile_revision"] != float64(4) || got["embedding_model"] != "text-embedding-3-small" || got["embedding_generation"] != "gen-7" || got["collection_config_digest"] != "digest-8" {
				t.Fatalf("knowledge search = %#v", got)
			}
		}},
		{"knowledge config", "agent.knowledge.config.get", map[string]any{"EmbeddingProfileID": "embed", "EmbeddingProfileRevision": float64(4), "EmbeddingModel": "text-embedding-3-small", "EmbeddingGeneration": "drop-me", "Dimension": float64(3), "Collection": "knowledge", "CollectionConfigDigest": "digest-8", "Revision": float64(2), "UpdatedAt": "updated"}, func(t *testing.T, got map[string]any) {
			if got["embedding_profile_id"] != "embed" || got["embedding_profile_revision"] != float64(4) || got["embedding_model"] != "text-embedding-3-small" || got["dimension"] != float64(3) || got["collection"] != "knowledge" || got["collection_config_digest"] != "digest-8" || got["revision"] != float64(2) || got["updated_at"] != "updated" {
				t.Fatalf("knowledge config = %#v", got)
			}
			if _, exposed := got["embedding_generation"]; exposed {
				t.Fatalf("knowledge config exposed search-only generation: %#v", got)
			}
		}},
		{"knowledge upload start exact projection", "agent.knowledge.upload.start", map[string]any{"upload_id": "u1", "source_id": "s1", "status": "open", "size": float64(8), "received_size": float64(2), "max_chunk_bytes": float64(4), "progress": float64(.25), "replayed": true, "revision": float64(2)}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"max_chunk_bytes", "progress", "received_size", "replayed", "size", "source_id", "status", "upload_id"}) {
				t.Fatalf("upload start keys=%v value=%#v", keys, got)
			}
		}},
		{"knowledge status exact projection", "agent.knowledge.status", map[string]any{"Supported": true, "Count": float64(3), "ReadyCount": float64(2), "FailedCount": float64(1), "EmbeddingIndexed": float64(1), "EmbeddingStale": float64(2), "EmbeddingProfileID": "embed", "EmbeddingProfileRevision": float64(4), "EmbeddingModel": "text-embedding-3-small", "QuotaUsedBytes": float64(1024), "QuotaLimitBytes": float64(67108864), "QuotaRemainingBytes": float64(67107840), "MaxSourceBytes": float64(16777216), "extra": "drop"}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"count", "embedding_indexed", "embedding_model", "embedding_profile_id", "embedding_profile_revision", "embedding_stale", "max_source_bytes", "quota_limit_bytes", "quota_remaining_bytes", "quota_used_bytes", "supported"}) {
				t.Fatalf("knowledge status keys=%v value=%#v", keys, got)
			}
			if got["embedding_indexed"] != float64(1) || got["embedding_stale"] != float64(2) || got["quota_used_bytes"] != float64(1024) || got["quota_limit_bytes"] != float64(67108864) || got["quota_remaining_bytes"] != float64(67107840) || got["max_source_bytes"] != float64(16777216) {
				t.Fatalf("knowledge status counters were inferred/changed: %#v", got)
			}
		}},
		{"memory create exact projection", "agent.knowledge.memory.create", map[string]any{"memory_id": "m1", "title": "title", "content": "hello", "tags": []any{"a"}, "revision": float64(2), "created_at": "now", "updated_at": "later", "replayed": true, "embedding_indexed": false, "embedding_profile_id": "embed", "embedding_profile_revision": float64(4), "embedding_model": "model", "extra": "drop"}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"content", "created_at", "embedding_indexed", "embedding_model", "embedding_profile_id", "embedding_profile_revision", "memory_id", "replayed", "tags", "title"}) {
				t.Fatalf("memory create keys=%v value=%#v", keys, got)
			}
		}},
		{"memory get exact projection", "agent.knowledge.memories.get", map[string]any{"memory_id": "m1", "title": "title", "content": "hello", "tags": []any{}, "revision": float64(2), "created_at": "now", "updated_at": "later", "replayed": true, "embedding_indexed": false}, func(t *testing.T, got map[string]any) {
			if keys := sortedMapKeys(got); !reflect.DeepEqual(keys, []string{"content", "created_at", "memory_id", "revision", "tags", "title", "updated_at"}) {
				t.Fatalf("memory get keys=%v value=%#v", keys, got)
			}
		}},
		{"task get", "agent.core.tasks.get", map[string]any{"ID": "t1", "State": "queued"}, func(t *testing.T, got map[string]any) {
			if _, ok := got["task"]; !ok {
				t.Fatalf("task get = %#v", got)
			}
		}},
		{"schedule list", "agent.schedules.list", map[string]any{"schedules": []any{}, "next_page_token": "p"}, func(t *testing.T, got map[string]any) {
			if got["next_cursor"] != "p" {
				t.Fatalf("schedule list = %#v", got)
			}
		}},
		{"extension list", "agent.core.skills.list", map[string]any{"Installations": []any{map[string]any{"ID": "i1", "State": "installed"}}, "NextPageToken": "p"}, func(t *testing.T, got map[string]any) {
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
		{"web search config drops secrets", "agent.web_search.config.get", map[string]any{"Enabled": true, "Provider": "tavily", "APIKeyConfigured": true, "APIKeyHint": "tvly-secret-must-not-leak", "Revision": float64(2), "TestedAt": "tested", "UpdatedAt": "updated", "api_key": "must-not-leak", "secret": "must-not-leak"}, func(t *testing.T, got map[string]any) {
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
		{"web search test exact projection", "agent.web_search.test", map[string]any{"OK": true, "Provider": "tavily", "ResultCount": float64(1), "TestedAt": "tested", "Enabled": true, "APIKeyConfigured": true, "Revision": float64(3), "api_key_hint": "drop", "provider_body": "drop"}, func(t *testing.T, got map[string]any) {
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
		{name: "execution digest is not lowercase sha256", input: map[string]any{"references": []any{withReferenceField(valid, "quote_digest", strings.Repeat("A", 64))}}},
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
		"kind":                  "execution_plan",
		"account_generation":    float64(7),
		"task_id":               "11111111-1111-4111-8111-111111111111",
		"plan_id":               "22222222-2222-4222-8222-222222222222",
		"plan_revision":         float64(3),
		"plan_digest":           strings.Repeat("a", 64),
		"run_id":                "33333333-3333-4333-8333-333333333333",
		"run_revision":          float64(4),
		"run_digest":            strings.Repeat("b", 64),
		"execution_id":          "33333333-3333-4333-8333-333333333333",
		"confirmation_id":       "44444444-4444-4444-8444-444444444444",
		"confirmation_revision": float64(5),
		"binding_digest":        strings.Repeat("c", 64),
		"quote_digest":          strings.Repeat("d", 64),
		"execution_digest":      strings.Repeat("e", 64),
		"status":                "waiting_user",
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
			"ID":                  "model-1",
			"Name":                "Model 1",
			"Provider":            "openrouter",
			"ContextLength":       float64(128000),
			"MaxOutputTokens":     float64(4096),
			"InputModalities":     []any{"text"},
			"OutputModalities":    []any{"text"},
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
		"QuotaUsedBytes": float64(1024), "QuotaLimitBytes": float64(67108864),
		"QuotaRemainingBytes": float64(67107840), "MaxSourceBytes": float64(16777216),
	}
	if _, err := adaptActionResult("agent.knowledge.status", valid); err != nil {
		t.Fatalf("valid knowledge status rejected: %v", err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"missing used":   func(value map[string]any) { delete(value, "QuotaUsedBytes") },
		"fraction limit": func(value map[string]any) { value["QuotaLimitBytes"] = 1.5 },
		"negative max":   func(value map[string]any) { value["MaxSourceBytes"] = -1 },
		"wrong fixed limits": func(value map[string]any) {
			value["QuotaLimitBytes"] = float64(33554432)
			value["QuotaRemainingBytes"] = float64(33553408)
			value["MaxSourceBytes"] = float64(8388608)
		},
		"used exceeds limit":     func(value map[string]any) { value["QuotaUsedBytes"] = float64(67108865) },
		"remaining inconsistent": func(value map[string]any) { value["QuotaRemainingBytes"] = float64(1) },
		"source exceeds limit":   func(value map[string]any) { value["MaxSourceBytes"] = float64(67108865) },
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

func sortedMapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestLegacyInputAliasesAreCanonicalizedBeforeAgentDigest(t *testing.T) {
	input := map[string]any{"confirm": "deprovision_account"}
	applyLegacyInputAliases("agent.account.deprovision", input)
	if input["confirmation"] != "deprovision_account" {
		t.Fatalf("account confirmation alias = %#v", input)
	}
	if _, ok := input["confirm"]; ok {
		t.Fatalf("legacy account confirm must not reach closed Agent schema: %#v", input)
	}
	input = map[string]any{"message_limit": float64(20), "message_cursor": "cursor"}
	applyLegacyInputAliases("agent.chat.conversations.get", input)
	if input["limit"] != float64(20) || input["page_token"] != "cursor" {
		t.Fatalf("conversation aliases = %#v", input)
	}
	input = map[string]any{"name": "daily", "prompt": "summarize", "model_profile_id": "profile", "trigger": map[string]any{"cron": "* * * * *"}}
	applyLegacyInputAliases("agent.schedules.create", input)
	if _, ok := input["spec"]; !ok {
		t.Fatalf("schedule spec alias missing: %#v", input)
	}
	if _, ok := input["trigger"]; ok || input["cron"] != "* * * * *" {
		t.Fatalf("schedule trigger projection = %#v", input)
	}
}
