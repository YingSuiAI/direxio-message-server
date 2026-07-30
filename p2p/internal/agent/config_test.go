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
		DisplayName: "Existing Agent",
		NativeAgentIdentity: dirextalkdomain.AgentIdentityConfig{
			DisplayName: "Existing Ying",
			AvatarURL:   "mxc://existing-ying",
		},
		OnlineAgentIdentity: dirextalkdomain.AgentIdentityConfig{
			DisplayName: "Existing Online",
			AvatarURL:   "mxc://existing-online",
		},
		ContextWindow: 30,
		Enabled:       true,
		Native:        map[string]any{"skills": []any{map[string]any{"id": "keep"}}},
	}

	next := FromNativeMap(current, map[string]any{
		"display_name": " Updated Agent ",
		"avatar_url":   " mxc://updated-ying ",
		"model":        " model-v2 ",
		"api_key":      "must-not-persist",
	})
	if next.DisplayName != "Updated Agent" || next.AvatarURL != "mxc://updated-ying" ||
		next.NativeAgentIdentity.DisplayName != "Updated Agent" ||
		next.NativeAgentIdentity.AvatarURL != "mxc://updated-ying" ||
		next.OnlineAgentIdentity.DisplayName != "Existing Online" ||
		next.OnlineAgentIdentity.AvatarURL != "mxc://existing-online" ||
		next.Model != "model-v2" || next.ContextWindow != 30 || !next.Enabled {
		t.Fatalf("unexpected mapped shared config: %#v", next)
	}
	if _, exposed := next.Native["api_key"]; exposed {
		t.Fatalf("mapped native config exposed api_key: %#v", next.Native)
	}
	if _, ok := next.Native["skills"]; !ok {
		t.Fatalf("mapped native config lost existing fields: %#v", next.Native)
	}
	nativeMap := ToNativeMap(next)
	if nativeMap["display_name"] != "Updated Agent" || nativeMap["avatar_url"] != "mxc://updated-ying" {
		t.Fatalf("native runtime config should expose Ying identity, got %#v", nativeMap)
	}
	if _, ok := nativeMap["online_agent_identity"]; ok {
		t.Fatalf("native runtime config must not expose Online Agent identity: %#v", nativeMap)
	}
	if _, ok := next.Native["native_agent_identity"]; ok {
		t.Fatalf("mode identities must not be stored as runtime Native fields: %#v", next.Native)
	}
}

func TestApplyConfigUpdateMaintainsModeIdentities(t *testing.T) {
	current := NormalizeConfig(dirextalkdomain.AgentConfig{
		DisplayName: "Legacy",
		AvatarURL:   "mxc://legacy",
		NativeAgentIdentity: dirextalkdomain.AgentIdentityConfig{
			DisplayName: "Legacy",
			AvatarURL:   "mxc://legacy",
		},
		OnlineAgentIdentity: dirextalkdomain.AgentIdentityConfig{
			DisplayName: "Legacy",
			AvatarURL:   "mxc://legacy",
		},
	})

	nativeOnly := ApplyConfigUpdate(current, map[string]any{
		"native_agent_identity": map[string]any{
			"display_name": "Ying Prime",
		},
	})
	if nativeOnly.DisplayName != "Ying Prime" ||
		nativeOnly.NativeAgentIdentity.DisplayName != "Ying Prime" ||
		nativeOnly.NativeAgentIdentity.AvatarURL != "mxc://legacy" ||
		nativeOnly.OnlineAgentIdentity.DisplayName != "Legacy" ||
		nativeOnly.OnlineAgentIdentity.AvatarURL != "mxc://legacy" {
		t.Fatalf("native identity update leaked or dropped fields: %#v", nativeOnly)
	}

	onlineAvatarOnly := ApplyConfigUpdate(nativeOnly, map[string]any{
		"online_agent_identity": map[string]any{
			"avatar_url": " mxc://online ",
		},
	})
	if onlineAvatarOnly.OnlineAgentIdentity.DisplayName != "Legacy" ||
		onlineAvatarOnly.OnlineAgentIdentity.AvatarURL != "mxc://online" ||
		onlineAvatarOnly.NativeAgentIdentity.DisplayName != "Ying Prime" ||
		onlineAvatarOnly.NativeAgentIdentity.AvatarURL != "mxc://legacy" {
		t.Fatalf("online avatar update should preserve names and Ying identity: %#v", onlineAvatarOnly)
	}

	legacyTopLevel := ApplyConfigUpdate(onlineAvatarOnly, map[string]any{
		"display_name": "Shared Agent",
		"avatar_url":   "mxc://shared",
	})
	if legacyTopLevel.NativeAgentIdentity.DisplayName != "Shared Agent" ||
		legacyTopLevel.NativeAgentIdentity.AvatarURL != "mxc://shared" ||
		legacyTopLevel.OnlineAgentIdentity.DisplayName != "Shared Agent" ||
		legacyTopLevel.OnlineAgentIdentity.AvatarURL != "mxc://shared" {
		t.Fatalf("legacy top-level update should sync both identities: %#v", legacyTopLevel)
	}

	nestedWins := ApplyConfigUpdate(legacyTopLevel, map[string]any{
		"display_name": "Legacy Request",
		"native_agent_identity": map[string]any{
			"display_name": "Nested Ying",
		},
	})
	if nestedWins.NativeAgentIdentity.DisplayName != "Nested Ying" ||
		nestedWins.OnlineAgentIdentity.DisplayName != "Legacy Request" {
		t.Fatalf("nested identity should win while top-level fills the other mode: %#v", nestedWins)
	}

	emptyAvatarIgnored := ApplyConfigUpdate(nestedWins, map[string]any{
		"avatar_url": "",
		"online_agent_identity": map[string]any{
			"avatar_url": "",
		},
	})
	if emptyAvatarIgnored.NativeAgentIdentity.AvatarURL != "mxc://shared" ||
		emptyAvatarIgnored.OnlineAgentIdentity.AvatarURL != "mxc://shared" {
		t.Fatalf("empty avatar strings must not clear existing identities: %#v", emptyAvatarIgnored)
	}
}

func TestOnlineIdentityUpdateRequested(t *testing.T) {
	if OnlineIdentityUpdateRequested(map[string]any{
		"native_agent_identity": map[string]any{"display_name": "Ying"},
	}) {
		t.Fatal("native-only identity update must not request Online Agent Matrix sync")
	}
	if !OnlineIdentityUpdateRequested(map[string]any{"display_name": "Legacy"}) {
		t.Fatal("legacy top-level identity update should request Online Agent Matrix sync")
	}
	if !OnlineIdentityUpdateRequested(map[string]any{
		"online_agent_identity": map[string]any{"avatar_url": "mxc://example.com/online"},
	}) {
		t.Fatal("online identity avatar update should request Matrix sync")
	}
	if OnlineIdentityUpdateRequested(map[string]any{
		"online_agent_identity": map[string]any{"avatar_url": ""},
	}) {
		t.Fatal("empty online identity fields must not request Matrix sync")
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
		t.Fatalf("legacy top-level identity should initialize both mode identities: %#v", state.AgentConfig)
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
