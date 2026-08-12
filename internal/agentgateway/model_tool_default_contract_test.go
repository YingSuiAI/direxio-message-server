package agentgateway

import (
	"errors"
	"testing"
)

func TestModelSyncToolDefaultRequiresConversationEntry(t *testing.T) {
	base := map[string]any{
		"idempotency_key": "11111111-1111-4111-8111-111111111111",
		"entries": []any{map[string]any{
			"client_profile_id": "tool", "model_kind": "conversation",
		}},
	}
	for _, action := range []string{"agent.model_profiles.sync"} {
		valid := cloneParams(base)
		valid["default_tool_client_profile_id"] = "tool"
		if err := ValidateActionRequest(action, valid); err != nil {
			t.Fatalf("%s valid tool default rejected: %v", action, err)
		}
		empty := cloneParams(base)
		empty["default_tool_client_profile_id"] = ""
		if err := ValidateActionRequest(action, empty); err != nil {
			t.Fatalf("%s empty tool default rejected: %v", action, err)
		}
		invalid := cloneParams(base)
		invalid["default_tool_client_profile_id"] = "tool"
		invalid["entries"] = []any{map[string]any{"client_profile_id": "tool", "model_kind": "embedding"}}
		if err := ValidateActionRequest(action, invalid); !errors.Is(err, ErrInvalidActionRequest) {
			t.Fatalf("%s non-conversation tool default error = %v", action, err)
		}
	}
}

func TestModelProfileResultsProjectAndValidateToolDefault(t *testing.T) {
	valid := map[string]any{
		"profiles":        []any{map[string]any{"client_profile_id": "tool", "model_kind": "conversation"}},
		"next_page_token": "", "default_tool_client_profile_id": "tool",
	}
	for _, action := range []string{"agent.model_profiles.list"} {
		result, err := adaptActionResult(action, valid)
		if err != nil {
			t.Fatalf("%s valid result rejected: %v", action, err)
		}
		if result["default_tool_client_profile_id"] != "tool" {
			t.Fatalf("%s tool default projection = %#v", action, result)
		}
	}
	invalid := cloneParams(valid)
	invalid["profiles"] = []any{map[string]any{"client_profile_id": "tool", "model_kind": "speech"}}
	if _, err := adaptActionResult("agent.model_profiles.list", invalid); !errors.Is(err, ErrInvalidActionResult) {
		t.Fatalf("non-conversation result error = %v", err)
	}
	for _, fallback := range []string{"default_tool_profile_id", "DefaultToolProfileID", "DefaultToolClientProfileID"} {
		aliasOnly := cloneParams(valid)
		delete(aliasOnly, "default_tool_client_profile_id")
		aliasOnly[fallback] = "tool"
		if _, err := adaptActionResult("agent.model_profiles.list", aliasOnly); !errors.Is(err, ErrInvalidActionResult) {
			t.Fatalf("fallback %s was accepted: %v", fallback, err)
		}
		canonicalAndAlias := cloneParams(valid)
		canonicalAndAlias[fallback] = "tool"
		if _, err := adaptActionResult("agent.model_profiles.list", canonicalAndAlias); !errors.Is(err, ErrInvalidActionResult) {
			t.Fatalf("fallback %s alongside canonical field was accepted: %v", fallback, err)
		}
	}
}

func TestModelProfileCatalogPinsMatchAgentToolDefaultSchemas(t *testing.T) {
	const (
		syncInput  = `{"type":"object","additionalProperties":false,"properties":{"idempotency_key":{"type":"string"},"default_conversation_client_profile_id":{"type":"string"},"default_tool_client_profile_id":{"type":"string"},"default_embedding_client_profile_id":{"type":"string"},"default_speech_client_profile_id":{"type":"string"},"entries":{"type":"array"}},"required":["idempotency_key","entries"]}`
		syncResult = `{"additionalProperties":false,"properties":{"default_conversation_client_profile_id":{"type":"string"},"default_embedding_client_profile_id":{"type":"string"},"default_speech_client_profile_id":{"type":"string"},"default_tool_client_profile_id":{"type":"string"},"profiles":{"type":"array"}},"required":["profiles","default_conversation_client_profile_id","default_tool_client_profile_id","default_embedding_client_profile_id","default_speech_client_profile_id"],"type":"object"}`
		listInput  = `{"additionalProperties":false,"properties":{"page_size":{"maximum":100,"minimum":1,"type":"integer"},"page_token":{"maxLength":4096,"type":"string"}},"type":"object"}`
		listResult = `{"additionalProperties":false,"properties":{"default_conversation_client_profile_id":{"type":"string"},"default_embedding_client_profile_id":{"type":"string"},"default_speech_client_profile_id":{"type":"string"},"default_tool_client_profile_id":{"type":"string"},"next_page_token":{"type":"string"},"profiles":{"type":"array"}},"required":["profiles","next_page_token","default_conversation_client_profile_id","default_tool_client_profile_id","default_embedding_client_profile_id","default_speech_client_profile_id"],"type":"object"}`
	)
	for _, test := range []struct{ action, operation, input, result string }{
		{"agent.model_profiles.sync", "sync_models", syncInput, syncResult},
		{"agent.model_profiles.list", "list_models", listInput, listResult},
	} {
		descriptor := catalogTestDescriptor("agent.models.v1", test.operation, test.input, test.result)
		if err := ValidateCatalog(catalogTestWithDigest(t, descriptor), []CatalogRequirement{NewCatalogRequirement(test.action)}); err != nil {
			t.Fatalf("%s current Agent schema rejected: %v", test.action, err)
		}
	}
}
