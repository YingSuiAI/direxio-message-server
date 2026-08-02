package agent

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkplugin"
)

type legacyPluginStoreStub struct {
	plugin dirextalkplugin.Instance
	ok     bool
	err    error
}

func (s legacyPluginStoreStub) GetPlugin(context.Context, string) (dirextalkplugin.Instance, bool, error) {
	return s.plugin, s.ok, s.err
}

func TestNativeConfigMappingPreservesSharedAndNativeFields(t *testing.T) {
	defaults := FromNativeMap(dirextalkdomain.AgentConfig{
		Native: map[string]any{"skills": []any{map[string]any{"id": "defaulted"}}},
	}, nil)
	if defaults.DisplayName != "Agent" || defaults.ContextWindow != 30 || !defaults.Enabled {
		t.Fatalf("native-only config lost historic shared defaults: %#v", defaults)
	}

	current := dirextalkdomain.AgentConfig{
		DisplayName:   "Existing Agent",
		ContextWindow: 30,
		Enabled:       true,
		Native:        map[string]any{"skills": []any{map[string]any{"id": "keep"}}},
	}

	next := FromNativeMap(current, map[string]any{
		"display_name": " Updated Agent ",
		"model":        " model-v2 ",
		"api_key":      "must-not-persist",
	})
	if next.DisplayName != "Updated Agent" || next.Model != "model-v2" || next.ContextWindow != 30 || !next.Enabled {
		t.Fatalf("unexpected mapped shared config: %#v", next)
	}
	if _, exposed := next.Native["api_key"]; exposed {
		t.Fatalf("mapped native config exposed api_key: %#v", next.Native)
	}
	if _, ok := next.Native["skills"]; !ok {
		t.Fatalf("mapped native config lost existing fields: %#v", next.Native)
	}
}

func TestSanitizeNativeConfigStripsSecretsWithoutMutatingInput(t *testing.T) {
	profile := map[string]any{
		"id":          "deepseek",
		"model":       "deepseek-chat",
		"api_key":     "profile-secret",
		"api_key_ref": "secret:profile",
	}
	input := map[string]any{
		"api_key":        "root-secret",
		"api_key_ref":    "secret:root",
		"model_profiles": []any{profile},
	}

	sanitized := SanitizeNativeConfigMap(input)
	if _, ok := sanitized["api_key"]; ok {
		t.Fatalf("sanitized config exposed root secret: %#v", sanitized)
	}
	gotProfile := sanitized["model_profiles"].([]any)[0].(map[string]any)
	if _, ok := gotProfile["api_key"]; ok || gotProfile["model"] != "deepseek-chat" {
		t.Fatalf("sanitized profile mismatch: %#v", gotProfile)
	}
	if profile["api_key"] != "profile-secret" || profile["api_key_ref"] != "secret:profile" {
		t.Fatalf("sanitizer mutated caller input: %#v", profile)
	}
}

func TestSanitizeNativeConfigDropsRequestScopedToolCredentials(t *testing.T) {
	input := map[string]any{
		"tool_credentials": map[string]any{
			"web_search": map[string]any{
				"enabled":  true,
				"provider": "tavily",
				"api_key":  "tvly-request-only",
			},
		},
		"model_profiles": []any{map[string]any{"model": "gpt", "api_key": "model-secret"}},
	}
	sanitized := SanitizeNativeConfigMap(input)
	if _, exists := sanitized["tool_credentials"]; exists {
		t.Fatalf("request-scoped tool credentials were retained: %#v", sanitized)
	}
	if hasNestedKey(sanitized, "api_key") {
		t.Fatalf("sanitized config retained an api_key: %#v", sanitized)
	}
	if input["tool_credentials"].(map[string]any)["web_search"].(map[string]any)["api_key"] != "tvly-request-only" {
		t.Fatal("sanitizer mutated caller input")
	}
}

func TestMigrateLegacyPluginConfigFillsMissingFieldsOnce(t *testing.T) {
	state := dirextalkdomain.PortalState{AgentConfig: dirextalkdomain.AgentConfig{
		SystemPrompt: "current prompt",
	}}
	store := legacyPluginStoreStub{
		ok: true,
		plugin: dirextalkplugin.Instance{Config: map[string]any{
			"display_name":  "Legacy Agent",
			"system_prompt": "legacy prompt",
			"skills":        []any{map[string]any{"id": "legacy-skill"}},
			"model_profiles": []any{map[string]any{
				"id": "legacy-profile", "api_key": "must-not-persist", "api_key_ref": "secret:legacy",
			}},
		}},
	}

	changed, err := MigrateLegacyPluginConfig(context.Background(), store, &state, LegacyPluginID)
	if err != nil || !changed {
		t.Fatalf("expected migration change, changed=%v err=%v", changed, err)
	}
	if state.AgentConfig.DisplayName != "Legacy Agent" || state.AgentConfig.SystemPrompt != "current prompt" {
		t.Fatalf("legacy merge overwrote current config: %#v", state.AgentConfig)
	}
	if hasNestedKey(ToNativeMap(state.AgentConfig), "api_key") || hasNestedKey(ToNativeMap(state.AgentConfig), "api_key_ref") {
		t.Fatalf("legacy migration persisted secret references: %#v", state.AgentConfig)
	}

	changed, err = MigrateLegacyPluginConfig(context.Background(), store, &state, LegacyPluginID)
	if err != nil || changed {
		t.Fatalf("expected idempotent migration, changed=%v err=%v", changed, err)
	}
}

func hasNestedKey(value any, wanted string) bool {
	switch typed := value.(type) {
	case map[string]any:
		if _, ok := typed[wanted]; ok {
			return true
		}
		for _, child := range typed {
			if hasNestedKey(child, wanted) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if hasNestedKey(child, wanted) {
				return true
			}
		}
	}
	return false
}
