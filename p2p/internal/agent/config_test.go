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
	if defaults.NativeAgentIdentity.DisplayName != DefaultNativeAgentDisplayName ||
		defaults.OnlineAgentIdentity.DisplayName != DefaultOnlineAgentDisplayName {
		t.Fatalf("native-only config lost mode defaults: %#v", defaults)
	}

	current := dirextalkdomain.AgentConfig{
		DisplayName:   "Existing Ying",
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
	if next.NativeAgentIdentity.DisplayName != "Updated Agent" || next.OnlineAgentIdentity.DisplayName != DefaultOnlineAgentDisplayName {
		t.Fatalf("runtime identity should update Ying only, got %#v", next)
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
	if state.AgentConfig.NativeAgentIdentity.DisplayName != "Legacy Agent" ||
		state.AgentConfig.OnlineAgentIdentity.DisplayName != "Legacy Agent" {
		t.Fatalf("legacy merge did not seed both identities: %#v", state.AgentConfig)
	}
	if hasNestedKey(ToNativeMap(state.AgentConfig), "api_key") || hasNestedKey(ToNativeMap(state.AgentConfig), "api_key_ref") {
		t.Fatalf("legacy migration persisted secret references: %#v", state.AgentConfig)
	}

	changed, err = MigrateLegacyPluginConfig(context.Background(), store, &state, LegacyPluginID)
	if err != nil || changed {
		t.Fatalf("expected idempotent migration, changed=%v err=%v", changed, err)
	}
}

func TestApplyConfigUpdateKeepsModeIdentitiesIndependent(t *testing.T) {
	current := NormalizeConfig(dirextalkdomain.AgentConfig{
		NativeAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Ying", AvatarURL: "mxc://ying"},
		OnlineAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Your Agent", AvatarURL: "mxc://online"},
		ContextWindow:       30,
		Enabled:             true,
	})

	next := ApplyConfigUpdate(current, map[string]any{
		"native_agent_identity": map[string]any{"display_name": "Ying 2"},
	})
	if next.NativeAgentIdentity.DisplayName != "Ying 2" || next.NativeAgentIdentity.AvatarURL != "mxc://ying" {
		t.Fatalf("native identity partial update failed: %#v", next)
	}
	if next.OnlineAgentIdentity.DisplayName != "Your Agent" || next.OnlineAgentIdentity.AvatarURL != "mxc://online" {
		t.Fatalf("native update affected online identity: %#v", next)
	}

	next = ApplyConfigUpdate(next, map[string]any{
		"online_agent_identity": map[string]any{"avatar_url": "mxc://online-2"},
	})
	if next.OnlineAgentIdentity.DisplayName != "Your Agent" || next.OnlineAgentIdentity.AvatarURL != "mxc://online-2" {
		t.Fatalf("online identity partial update failed: %#v", next)
	}
	if next.NativeAgentIdentity.DisplayName != "Ying 2" || next.NativeAgentIdentity.AvatarURL != "mxc://ying" {
		t.Fatalf("online update affected native identity: %#v", next)
	}
}

func TestApplyConfigUpdateLegacyTopLevelSyncsBothIdentities(t *testing.T) {
	next := ApplyConfigUpdate(dirextalkdomain.AgentConfig{}, map[string]any{
		"display_name": "Legacy Name",
		"avatar_url":   "mxc://legacy",
	})
	if next.NativeAgentIdentity.DisplayName != "Legacy Name" || next.NativeAgentIdentity.AvatarURL != "mxc://legacy" ||
		next.OnlineAgentIdentity.DisplayName != "Legacy Name" || next.OnlineAgentIdentity.AvatarURL != "mxc://legacy" {
		t.Fatalf("legacy top-level update should sync both identities: %#v", next)
	}
}

func TestApplyConfigUpdateNestedIdentityWinsOverTopLevelFallback(t *testing.T) {
	next := ApplyConfigUpdate(dirextalkdomain.AgentConfig{}, map[string]any{
		"display_name": "Legacy Name",
		"native_agent_identity": map[string]any{
			"display_name": "Ying Nested",
		},
	})
	if next.NativeAgentIdentity.DisplayName != "Ying Nested" {
		t.Fatalf("native nested identity should win over top-level: %#v", next)
	}
	if next.OnlineAgentIdentity.DisplayName != "Legacy Name" {
		t.Fatalf("top-level should still seed online fallback: %#v", next)
	}
}

func TestOnlineIdentityUpdateRequestedIgnoresNativeOnlyUpdates(t *testing.T) {
	if OnlineIdentityUpdateRequested(map[string]any{
		"native_agent_identity": map[string]any{"display_name": "Ying 2"},
	}) {
		t.Fatal("native-only identity update must not sync Matrix online identity")
	}
	if !OnlineIdentityUpdateRequested(map[string]any{
		"online_agent_identity": map[string]any{"avatar_url": "mxc://online-2"},
	}) {
		t.Fatal("online identity update should sync Matrix online identity")
	}
	if !OnlineIdentityUpdateRequested(map[string]any{
		"display_name": "Legacy Agent",
	}) {
		t.Fatal("legacy top-level identity update should sync Matrix online identity")
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
