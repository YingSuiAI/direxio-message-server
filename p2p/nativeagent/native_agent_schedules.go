package nativeagent

// scheduledToolNames is deliberately independent of interactive selection.
// These are the actual compiled MCP tool names, and exclude message access,
// Matrix writes, runtime/shell, skills, MCP management, and external MCP.
var scheduledToolNames = map[string]struct{}{
	"dirextalk_contacts_list":         {},
	"dirextalk_contacts_search":       {},
	"dirextalk_rooms_search":          {},
	"dirextalk_room_members_list":     {},
	"dirextalk_channel_posts_list":    {},
	"dirextalk_channel_comments_list": {},
}

// EmbeddedAllowedTools is the code-owned read-only schedule tool allowlist.
// Callers cannot expand this set through configuration.
func EmbeddedAllowedTools() []string {
	return []string{"dirextalk_contacts_list", "dirextalk_contacts_search", "dirextalk_rooms_search", "dirextalk_room_members_list", "dirextalk_channel_posts_list", "dirextalk_channel_comments_list"}
}
