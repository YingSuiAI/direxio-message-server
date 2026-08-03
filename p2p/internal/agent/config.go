// Package agent owns Native Agent configuration mapping and legacy import.
package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkplugin"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

const LegacyPluginID = "io.dirextalk.agent"

// LegacyPluginStore is the only plugin persistence capability needed to import
// pre-native Agent configuration during service startup.
type LegacyPluginStore interface {
	GetPlugin(context.Context, string) (dirextalkplugin.Instance, bool, error)
}

// ToNativeMap combines shared Agent fields and runtime-owned fields while
// excluding credentials that must never enter portal state.
func ToNativeMap(cfg dirextalkdomain.AgentConfig) map[string]any {
	out := cloneMap(cfg.Native)
	out["display_name"] = cfg.DisplayName
	out["avatar_url"] = cfg.AvatarURL
	out["context_window"] = cfg.ContextWindow
	out["enabled"] = cfg.Enabled
	out["model"] = cfg.Model
	out["system_prompt"] = cfg.SystemPrompt
	out["mcp_blocked_room_ids"] = append([]string(nil), cfg.MCPBlockedRoomIDs...)
	return SanitizeNativeConfigMap(out)
}

// FromNativeMap applies runtime configuration over the current durable Agent
// configuration and separates shared fields from runtime-owned fields.
func FromNativeMap(current dirextalkdomain.AgentConfig, config map[string]any) dirextalkdomain.AgentConfig {
	merged := ToNativeMap(current)
	for key, value := range config {
		merged[key] = value
	}
	merged = SanitizeNativeConfigMap(merged)

	next := current
	if _, ok := merged["display_name"]; ok {
		next.DisplayName = actionbase.String(merged["display_name"])
	}
	if _, ok := merged["avatar_url"]; ok {
		next.AvatarURL = actionbase.String(merged["avatar_url"])
	}
	if value := actionbase.Int64(merged["context_window"]); value > 0 {
		next.ContextWindow = value
	}
	if _, ok := merged["enabled"]; ok {
		next.Enabled = actionbase.Bool(merged["enabled"])
	}
	if _, ok := merged["model"]; ok {
		next.Model = actionbase.String(merged["model"])
	}
	if _, ok := merged["system_prompt"]; ok {
		next.SystemPrompt = actionbase.String(merged["system_prompt"])
	}
	if _, ok := merged["mcp_blocked_room_ids"]; ok {
		next.MCPBlockedRoomIDs = actionbase.Strings(merged["mcp_blocked_room_ids"])
	}

	native := make(map[string]any)
	for key, value := range merged {
		if sharedConfigKey(key) {
			continue
		}
		native[key] = value
	}
	if len(native) > 0 {
		next.Native = native
	} else {
		next.Native = nil
	}
	return NormalizeConfig(next)
}

// MigrateLegacyPluginConfig imports the retired Agent plugin configuration
// into durable portal state without overwriting already configured values.
func MigrateLegacyPluginConfig(ctx context.Context, store LegacyPluginStore, state *dirextalkdomain.PortalState, pluginID string) (bool, error) {
	if store == nil || state == nil {
		return false, nil
	}
	plugin, ok, err := store.GetPlugin(ctx, pluginID)
	if err != nil || !ok {
		return false, err
	}
	legacy := SanitizeNativeConfigMap(plugin.Config)
	if len(legacy) == 0 {
		return false, nil
	}
	before := ToNativeMap(state.AgentConfig)
	state.AgentConfig = MergeLegacyConfig(state.AgentConfig, legacy)
	after := ToNativeMap(state.AgentConfig)
	return jsonValue(before) != jsonValue(after), nil
}

// MergeLegacyConfig fills gaps from the retired plugin representation.
func MergeLegacyConfig(current dirextalkdomain.AgentConfig, legacy map[string]any) dirextalkdomain.AgentConfig {
	next := current
	if actionbase.String(next.DisplayName) == "" && actionbase.String(legacy["display_name"]) != "" {
		next.DisplayName = actionbase.String(legacy["display_name"])
	}
	if actionbase.String(next.AvatarURL) == "" {
		if _, ok := legacy["avatar_url"]; ok {
			next.AvatarURL = actionbase.String(legacy["avatar_url"])
		}
	}
	if next.ContextWindow <= 0 {
		if value := actionbase.Int64(legacy["context_window"]); value > 0 {
			next.ContextWindow = value
		}
	}
	if configEmpty(current) {
		if _, ok := legacy["enabled"]; ok {
			next.Enabled = actionbase.Bool(legacy["enabled"])
		}
	}
	if actionbase.String(next.Model) == "" && actionbase.String(legacy["model"]) != "" {
		next.Model = actionbase.String(legacy["model"])
	}
	if actionbase.String(next.SystemPrompt) == "" && actionbase.String(legacy["system_prompt"]) != "" {
		next.SystemPrompt = actionbase.String(legacy["system_prompt"])
	}
	if len(next.MCPBlockedRoomIDs) == 0 {
		next.MCPBlockedRoomIDs = actionbase.Strings(legacy["mcp_blocked_room_ids"])
	}

	native := cloneMap(next.Native)
	for key, value := range legacy {
		if sharedConfigKey(key) {
			continue
		}
		if _, exists := native[key]; !exists {
			native[key] = value
		}
	}
	if len(native) > 0 {
		next.Native = native
	}
	return NormalizeConfig(next)
}

// NormalizeConfig preserves the historic shared defaults and normalization.
func NormalizeConfig(cfg dirextalkdomain.AgentConfig) dirextalkdomain.AgentConfig {
	empty := strings.TrimSpace(cfg.DisplayName) == "" &&
		strings.TrimSpace(cfg.AvatarURL) == "" &&
		cfg.ContextWindow == 0 &&
		!cfg.Enabled &&
		strings.TrimSpace(cfg.Model) == "" &&
		strings.TrimSpace(cfg.SystemPrompt) == "" &&
		len(cfg.MCPBlockedRoomIDs) == 0
	if empty {
		cfg.DisplayName = "Agent"
		cfg.ContextWindow = 30
		cfg.Enabled = true
		return cfg
	}
	if strings.TrimSpace(cfg.DisplayName) == "" {
		cfg.DisplayName = "Agent"
	} else {
		cfg.DisplayName = strings.TrimSpace(cfg.DisplayName)
	}
	cfg.AvatarURL = strings.TrimSpace(cfg.AvatarURL)
	if cfg.ContextWindow <= 0 {
		cfg.ContextWindow = 30
	}
	cfg.Model = strings.TrimSpace(cfg.Model)
	cfg.SystemPrompt = strings.TrimSpace(cfg.SystemPrompt)
	cfg.MCPBlockedRoomIDs = actionbase.Strings(cfg.MCPBlockedRoomIDs)
	return cfg
}

// SanitizeNativeConfigMap clones the mutable levels it edits and removes
// runtime credentials and references from durable/public configuration.
func SanitizeNativeConfigMap(config map[string]any) map[string]any {
	sanitized, ok := sanitizeNativeConfigValue(config).(map[string]any)
	if !ok {
		return map[string]any{}
	}
	return sanitized
}

func sanitizeNativeConfigValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		sanitized := make(map[string]any, len(typed))
		for key, child := range typed {
			if sensitiveNativeConfigKey(key) {
				continue
			}
			sanitized[key] = sanitizeNativeConfigValue(child)
		}
		return sanitized
	case []any:
		sanitized := make([]any, len(typed))
		for index, child := range typed {
			sanitized[index] = sanitizeNativeConfigValue(child)
		}
		return sanitized
	case []map[string]any:
		sanitized := make([]map[string]any, len(typed))
		for index, child := range typed {
			sanitized[index], _ = sanitizeNativeConfigValue(child).(map[string]any)
		}
		return sanitized
	default:
		return value
	}
}

func sensitiveNativeConfigKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "api_key", "api_key_ref", "tool_credentials", "web_search_credentials", "credential_ref", "credentials_ref":
		return true
	default:
		return false
	}
}

func sanitizeModelProfiles(profiles []any) []any {
	sanitized := make([]any, 0, len(profiles))
	for _, rawProfile := range profiles {
		profile, ok := rawProfile.(map[string]any)
		if !ok {
			sanitized = append(sanitized, rawProfile)
			continue
		}
		cloned := cloneMap(profile)
		delete(cloned, "api_key")
		delete(cloned, "api_key_ref")
		sanitized = append(sanitized, cloned)
	}
	return sanitized
}

func configEmpty(cfg dirextalkdomain.AgentConfig) bool {
	return actionbase.String(cfg.DisplayName) == "" &&
		actionbase.String(cfg.AvatarURL) == "" &&
		cfg.ContextWindow == 0 &&
		!cfg.Enabled &&
		actionbase.String(cfg.Model) == "" &&
		actionbase.String(cfg.SystemPrompt) == "" &&
		len(cfg.MCPBlockedRoomIDs) == 0 &&
		len(cfg.Native) == 0
}

func sharedConfigKey(key string) bool {
	switch key {
	case "display_name", "avatar_url", "context_window", "enabled", "model", "system_prompt", "mcp_blocked_room_ids":
		return true
	default:
		return false
	}
}

func cloneMap(values map[string]any) map[string]any {
	if values == nil {
		return map[string]any{}
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

func jsonValue(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(data)
}
