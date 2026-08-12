// Package agent owns the narrow account projection shared by the external
// Native Agent gateway and the Matrix-backed Online Agent.
package agent

import (
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

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

// ApplyOnlineConfigUpdate mutates only the Matrix-backed Online Agent
// identity. Native Agent fields are committed by the external gateway and are
// never copied into message-server's portal projection.
func ApplyOnlineConfigUpdate(current dirextalkdomain.AgentConfig, params map[string]any) dirextalkdomain.AgentConfig {
	next := NormalizeConfig(current)
	values := actionbase.Params(params)
	if identity, ok := params[configKeyOnlineAgentIdentity]; ok {
		next.OnlineAgentIdentity = mergeIdentity(next.OnlineAgentIdentity, identity)
	}
	if _, ok := params["mcp_blocked_room_ids"]; ok {
		// MCP room policy is a message-server-owned projection shared by the
		// Native and Online Agent surfaces; the runtime receives the same field
		// through the external config update above.
		next.MCPBlockedRoomIDs = values.Strings("mcp_blocked_room_ids")
	}
	if _, ok := params["enabled"]; ok {
		next.Enabled = values.Bool("enabled")
	}
	return NormalizeConfig(next)
}

// OnlineIdentityUpdateRequested reports whether config update changes the
// Matrix-backed Online Agent identity. Native-only updates stay remote.
func OnlineIdentityUpdateRequested(params map[string]any) bool {
	return hasKey(params, configKeyOnlineAgentIdentity) && identityUpdateRequested(params[configKeyOnlineAgentIdentity])
}

// NormalizeConfig preserves the dual identity projection used by portal
// bootstrap and Matrix session/room repair. Native runtime fields remain
// opaque to this package and are populated by the external gateway response.
func NormalizeConfig(cfg dirextalkdomain.AgentConfig) dirextalkdomain.AgentConfig {
	empty := identityEmpty(cfg.NativeAgentIdentity) &&
		identityEmpty(cfg.OnlineAgentIdentity) &&
		!cfg.Enabled &&
		len(cfg.MCPBlockedRoomIDs) == 0
	if empty {
		cfg.NativeAgentIdentity = dirextalkdomain.AgentIdentityConfig{DisplayName: DefaultNativeAgentDisplayName}
		cfg.OnlineAgentIdentity = dirextalkdomain.AgentIdentityConfig{DisplayName: DefaultOnlineAgentDisplayName}
		cfg.Enabled = true
		return cfg
	}
	cfg.NativeAgentIdentity = normalizeIdentity(cfg.NativeAgentIdentity, dirextalkdomain.AgentIdentityConfig{DisplayName: DefaultNativeAgentDisplayName})
	cfg.OnlineAgentIdentity = normalizeIdentity(cfg.OnlineAgentIdentity, dirextalkdomain.AgentIdentityConfig{
		DisplayName: DefaultOnlineAgentDisplayName,
	})
	cfg.MCPBlockedRoomIDs = actionbase.Strings(cfg.MCPBlockedRoomIDs)
	return cfg
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
	if identity.DisplayName == "" {
		identity.DisplayName = fallbackString(fallback.DisplayName, DefaultNativeAgentDisplayName)
	}
	return identity
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
	return strings.TrimSpace(identity.DisplayName) == "" && strings.TrimSpace(identity.AvatarURL) == ""
}

func hasKey(values map[string]any, key string) bool {
	_, ok := values[key]
	return ok
}

func fallbackString(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return strings.TrimSpace(fallback)
}
