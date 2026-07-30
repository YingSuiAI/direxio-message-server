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

const (
	DefaultNativeAgentDisplayName = "Agent"
	DefaultOnlineAgentDisplayName = "Your Agent"
)

const (
	configKeyDisplayName         = "display_name"
	configKeyAvatarURL           = "avatar_url"
	configKeyNativeAgentIdentity = "native_agent_identity"
	configKeyOnlineAgentIdentity = "online_agent_identity"
)

// LegacyPluginStore is the only plugin persistence capability needed to import
// pre-native Agent configuration during service startup.
type LegacyPluginStore interface {
	GetPlugin(context.Context, string) (dirextalkplugin.Instance, bool, error)
}

// ToNativeMap combines shared Agent fields and runtime-owned fields while
// excluding credentials that must never enter portal state.
func ToNativeMap(cfg dirextalkdomain.AgentConfig) map[string]any {
	cfg = NormalizeConfig(cfg)
	nativeIdentity := NativeAgentIdentity(cfg)
	out := cloneMap(cfg.Native)
	out[configKeyDisplayName] = nativeIdentity.DisplayName
	out[configKeyAvatarURL] = nativeIdentity.AvatarURL
	out[configKeyNativeAgentIdentity] = identityMap(nativeIdentity)
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
	if _, ok := merged[configKeyDisplayName]; ok {
		next.NativeAgentIdentity.DisplayName = actionbase.String(merged[configKeyDisplayName])
	}
	if _, ok := merged[configKeyAvatarURL]; ok {
		next.NativeAgentIdentity.AvatarURL = actionbase.String(merged[configKeyAvatarURL])
	}
	if _, ok := config[configKeyNativeAgentIdentity]; ok {
		next.NativeAgentIdentity = mergeIdentity(next.NativeAgentIdentity, config[configKeyNativeAgentIdentity])
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

// ApplyConfigUpdate merges ProductCore agent.config.update params into the
// durable Agent configuration while preserving legacy top-level compatibility.
func ApplyConfigUpdate(current dirextalkdomain.AgentConfig, params map[string]any) dirextalkdomain.AgentConfig {
	next := current
	values := actionbase.Params(params)
	hasNativeIdentity := hasKey(params, configKeyNativeAgentIdentity)
	hasOnlineIdentity := hasKey(params, configKeyOnlineAgentIdentity)

	if displayName := values.String(configKeyDisplayName); displayName != "" {
		next.DisplayName = displayName
		if !hasNativeIdentity {
			next.NativeAgentIdentity.DisplayName = displayName
		}
		if !hasOnlineIdentity {
			next.OnlineAgentIdentity.DisplayName = displayName
		}
	}
	if avatarURL := values.String(configKeyAvatarURL); avatarURL != "" {
		next.AvatarURL = avatarURL
		if !hasNativeIdentity {
			next.NativeAgentIdentity.AvatarURL = avatarURL
		}
		if !hasOnlineIdentity {
			next.OnlineAgentIdentity.AvatarURL = avatarURL
		}
	}
	if hasNativeIdentity {
		next.NativeAgentIdentity = mergeIdentity(next.NativeAgentIdentity, params[configKeyNativeAgentIdentity])
	}
	if hasOnlineIdentity {
		next.OnlineAgentIdentity = mergeIdentity(next.OnlineAgentIdentity, params[configKeyOnlineAgentIdentity])
	}
	if contextWindow := values.Int64("context_window"); contextWindow > 0 {
		next.ContextWindow = contextWindow
	}
	if _, ok := params["enabled"]; ok {
		next.Enabled = values.Bool("enabled")
	}
	if model := values.String("model"); model != "" {
		next.Model = model
	}
	if systemPrompt := values.String("system_prompt"); systemPrompt != "" {
		next.SystemPrompt = systemPrompt
	}
	if _, ok := params["mcp_blocked_room_ids"]; ok {
		next.MCPBlockedRoomIDs = values.Strings("mcp_blocked_room_ids")
	}
	return NormalizeConfig(next)
}

// OnlineIdentityUpdateRequested reports whether agent.config.update changes the
// Matrix-backed Online Agent identity. Native-only identity updates deliberately
// return false so Ying changes never trigger Matrix profile/member writes.
func OnlineIdentityUpdateRequested(params map[string]any) bool {
	hasOnlineIdentity := hasKey(params, configKeyOnlineAgentIdentity)
	if hasOnlineIdentity {
		return identityUpdateRequested(params[configKeyOnlineAgentIdentity])
	}
	return topLevelIdentityUpdateRequested(params)
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
	legacyDisplayName := actionbase.String(legacy[configKeyDisplayName])
	legacyAvatarURL := actionbase.String(legacy[configKeyAvatarURL])
	if actionbase.String(next.DisplayName) == "" && legacyDisplayName != "" {
		next.DisplayName = legacyDisplayName
	}
	if actionbase.String(next.AvatarURL) == "" {
		if _, ok := legacy[configKeyAvatarURL]; ok {
			next.AvatarURL = legacyAvatarURL
		}
	}
	if legacyDisplayName != "" {
		if strings.TrimSpace(next.NativeAgentIdentity.DisplayName) == "" {
			next.NativeAgentIdentity.DisplayName = legacyDisplayName
		}
		if strings.TrimSpace(next.OnlineAgentIdentity.DisplayName) == "" {
			next.OnlineAgentIdentity.DisplayName = legacyDisplayName
		}
	}
	if legacyAvatarURL != "" {
		if strings.TrimSpace(next.NativeAgentIdentity.AvatarURL) == "" {
			next.NativeAgentIdentity.AvatarURL = legacyAvatarURL
		}
		if strings.TrimSpace(next.OnlineAgentIdentity.AvatarURL) == "" {
			next.OnlineAgentIdentity.AvatarURL = legacyAvatarURL
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
		identityEmpty(cfg.NativeAgentIdentity) &&
		identityEmpty(cfg.OnlineAgentIdentity) &&
		cfg.ContextWindow == 0 &&
		!cfg.Enabled &&
		strings.TrimSpace(cfg.Model) == "" &&
		strings.TrimSpace(cfg.SystemPrompt) == "" &&
		len(cfg.MCPBlockedRoomIDs) == 0
	if empty {
		cfg.DisplayName = DefaultNativeAgentDisplayName
		cfg.NativeAgentIdentity = dirextalkdomain.AgentIdentityConfig{DisplayName: DefaultNativeAgentDisplayName}
		cfg.OnlineAgentIdentity = dirextalkdomain.AgentIdentityConfig{DisplayName: DefaultOnlineAgentDisplayName}
		cfg.ContextWindow = 30
		cfg.Enabled = true
		return cfg
	}
	if strings.TrimSpace(cfg.DisplayName) == "" {
		cfg.DisplayName = DefaultNativeAgentDisplayName
	} else {
		cfg.DisplayName = strings.TrimSpace(cfg.DisplayName)
	}
	cfg.AvatarURL = strings.TrimSpace(cfg.AvatarURL)
	legacyIdentity := dirextalkdomain.AgentIdentityConfig{
		DisplayName: cfg.DisplayName,
		AvatarURL:   cfg.AvatarURL,
	}
	cfg.NativeAgentIdentity = normalizeIdentity(cfg.NativeAgentIdentity, legacyIdentity)
	cfg.OnlineAgentIdentity = normalizeIdentity(cfg.OnlineAgentIdentity, dirextalkdomain.AgentIdentityConfig{
		DisplayName: DefaultOnlineAgentDisplayName,
	})
	cfg.DisplayName = cfg.NativeAgentIdentity.DisplayName
	cfg.AvatarURL = cfg.NativeAgentIdentity.AvatarURL
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
	sanitized := cloneMap(config)
	delete(sanitized, "api_key")
	delete(sanitized, "api_key_ref")
	if profiles, ok := sanitized["model_profiles"].([]any); ok {
		sanitized["model_profiles"] = sanitizeModelProfiles(profiles)
	}
	return sanitized
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
		identityEmpty(cfg.NativeAgentIdentity) &&
		identityEmpty(cfg.OnlineAgentIdentity) &&
		cfg.ContextWindow == 0 &&
		!cfg.Enabled &&
		actionbase.String(cfg.Model) == "" &&
		actionbase.String(cfg.SystemPrompt) == "" &&
		len(cfg.MCPBlockedRoomIDs) == 0 &&
		len(cfg.Native) == 0
}

func sharedConfigKey(key string) bool {
	switch key {
	case configKeyDisplayName, configKeyAvatarURL, configKeyNativeAgentIdentity, configKeyOnlineAgentIdentity, "context_window", "enabled", "model", "system_prompt", "mcp_blocked_room_ids":
		return true
	default:
		return false
	}
}

func NativeAgentIdentity(cfg dirextalkdomain.AgentConfig) dirextalkdomain.AgentIdentityConfig {
	return NormalizeConfig(cfg).NativeAgentIdentity
}

func OnlineAgentIdentity(cfg dirextalkdomain.AgentConfig) dirextalkdomain.AgentIdentityConfig {
	return NormalizeConfig(cfg).OnlineAgentIdentity
}

func mergeIdentity(current dirextalkdomain.AgentIdentityConfig, value any) dirextalkdomain.AgentIdentityConfig {
	raw, ok := value.(map[string]any)
	if !ok {
		return current
	}
	values := actionbase.Params(raw)
	next := current
	if displayName := values.String(configKeyDisplayName); displayName != "" {
		next.DisplayName = displayName
	}
	if avatarURL := values.String(configKeyAvatarURL); avatarURL != "" {
		next.AvatarURL = avatarURL
	}
	return next
}

func normalizeIdentity(identity, fallback dirextalkdomain.AgentIdentityConfig) dirextalkdomain.AgentIdentityConfig {
	identity.DisplayName = strings.TrimSpace(identity.DisplayName)
	identity.AvatarURL = strings.TrimSpace(identity.AvatarURL)
	fallback.DisplayName = strings.TrimSpace(fallback.DisplayName)
	fallback.AvatarURL = strings.TrimSpace(fallback.AvatarURL)
	if identity.DisplayName == "" && identity.AvatarURL == "" {
		identity = fallback
	}
	if strings.TrimSpace(identity.DisplayName) == "" {
		identity.DisplayName = fallbackString(fallback.DisplayName, DefaultNativeAgentDisplayName)
	}
	return identity
}

func topLevelIdentityUpdateRequested(params map[string]any) bool {
	values := actionbase.Params(params)
	return values.String(configKeyDisplayName) != "" || values.String(configKeyAvatarURL) != ""
}

func identityUpdateRequested(value any) bool {
	raw, ok := value.(map[string]any)
	if !ok {
		return false
	}
	values := actionbase.Params(raw)
	return values.String(configKeyDisplayName) != "" || values.String(configKeyAvatarURL) != ""
}

func identityEmpty(identity dirextalkdomain.AgentIdentityConfig) bool {
	return strings.TrimSpace(identity.DisplayName) == "" &&
		strings.TrimSpace(identity.AvatarURL) == ""
}

func identityMap(identity dirextalkdomain.AgentIdentityConfig) map[string]any {
	return map[string]any{
		configKeyDisplayName: strings.TrimSpace(identity.DisplayName),
		configKeyAvatarURL:   strings.TrimSpace(identity.AvatarURL),
	}
}

func hasKey(values map[string]any, key string) bool {
	_, ok := values[key]
	return ok
}

func fallbackString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
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
