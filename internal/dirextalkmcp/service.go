package dirextalkmcp

import (
	"context"
	"net/http"
	"strings"
)

const (
	ActionRoomsSearch           = "mcp.rooms.search"
	ActionContactsList          = "mcp.contacts.list"
	ActionContactsSearch        = "mcp.contacts.search"
	ActionMessagesSend          = "mcp.messages.send"
	ActionMessagesList          = "mcp.messages.list"
	ActionRoomMembersList       = "mcp.room_members.list"
	ActionChannelPostsList      = "mcp.channel_posts.list"
	ActionChannelCommentsList   = "mcp.channel_comments.list"
	ActionChannelCommentsCreate = "mcp.channel_comments.create"
)

type Invoker interface {
	InvokeCapability(ctx context.Context, action string, params map[string]any) (any, *Error)
}

type RoomAuthorizer interface {
	MCPRoomBlocked(roomID string) bool
}

type Config struct {
	Invoker        Invoker
	RoomAuthorizer RoomAuthorizer
}

// ToolEffect classifies the externally observable side effect of an MCP tool.
// Keep this classification independent from tool names so consumers can make
// retry decisions from the advertised contract rather than naming conventions.
type ToolEffect string

const (
	ToolEffectRead               ToolEffect = "read"
	ToolEffectNonIdempotentWrite ToolEffect = "non_idempotent_write"
)

// ToolAnnotations is the standard MCP tool-annotation contract derived from a
// ToolEffect. All hints are emitted explicitly so clients never need to apply
// the MCP defaults when deciding whether an invocation is safe to retry.
type ToolAnnotations struct {
	ReadOnlyHint    bool `json:"readOnlyHint"`
	DestructiveHint bool `json:"destructiveHint"`
	IdempotentHint  bool `json:"idempotentHint"`
	OpenWorldHint   bool `json:"openWorldHint"`
}

type Tool struct {
	Action      string
	Name        string
	Description string
	InputSchema map[string]any
	Effect      ToolEffect
}

func (t Tool) Annotations() ToolAnnotations {
	switch t.Effect {
	case ToolEffectRead:
		return ToolAnnotations{
			ReadOnlyHint:    true,
			DestructiveHint: false,
			IdempotentHint:  true,
			OpenWorldHint:   true,
		}
	case ToolEffectNonIdempotentWrite:
		return ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: false,
			IdempotentHint:  false,
			OpenWorldHint:   true,
		}
	default:
		// Unknown classifications fail closed as unsafe mutations.
		return ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: true,
			IdempotentHint:  false,
			OpenWorldHint:   true,
		}
	}
}

type Service struct {
	invoker        Invoker
	roomAuthorizer RoomAuthorizer
}

func NewService(invoker Invoker) *Service {
	return NewServiceWithConfig(Config{Invoker: invoker})
}

func NewServiceWithConfig(cfg Config) *Service {
	return &Service{invoker: cfg.Invoker, roomAuthorizer: cfg.RoomAuthorizer}
}

func (s *Service) Invoke(ctx context.Context, action string, params map[string]any) (any, *Error) {
	action = strings.TrimSpace(action)
	if _, ok := toolByAction(action); !ok {
		return nil, StatusError(http.StatusBadRequest, "unknown MCP action")
	}
	if s == nil || s.invoker == nil {
		return nil, StatusError(http.StatusInternalServerError, "Dirextalk MCP capability service is unavailable")
	}
	if params == nil {
		params = map[string]any{}
	}
	return s.invoker.InvokeCapability(ctx, action, params)
}

func (s *Service) RequireRoomAllowed(roomID string) *Error {
	if s != nil && s.roomAuthorizer != nil && s.roomAuthorizer.MCPRoomBlocked(roomID) {
		return StatusError(http.StatusForbidden, "room is blocked for MCP")
	}
	return nil
}

func (s *Service) RoomAllowed(roomID string) bool {
	return s.RequireRoomAllowed(roomID) == nil
}

func (s *Service) Actions() []string {
	actions := make([]string, 0, len(capabilityTools))
	for _, tool := range capabilityTools {
		actions = append(actions, tool.Action)
	}
	return actions
}

func (s *Service) Tools() []Tool {
	return Tools()
}

func Tools() []Tool {
	tools := make([]Tool, len(capabilityTools))
	copy(tools, capabilityTools)
	return tools
}

func NativeToolAction(name string) (string, bool) {
	name = strings.TrimSpace(name)
	for _, tool := range capabilityTools {
		if tool.Name == name {
			return tool.Action, true
		}
	}
	return "", false
}

func toolByAction(action string) (Tool, bool) {
	for _, tool := range capabilityTools {
		if tool.Action == action {
			return tool, true
		}
	}
	return Tool{}, false
}

var capabilityTools = []Tool{
	{
		Action:      ActionContactsList,
		Name:        "dirextalk_contacts_list",
		Description: "List Dirextalk contacts.",
		InputSchema: objectSchema(map[string]any{"query": stringSchema(), "limit": numberSchema()}),
		Effect:      ToolEffectRead,
	},
	{
		Action:      ActionContactsSearch,
		Name:        "dirextalk_contacts_search",
		Description: "Search Dirextalk contacts.",
		InputSchema: objectSchema(map[string]any{"query": stringSchema(), "limit": numberSchema()}),
		Effect:      ToolEffectRead,
	},
	{
		Action:      ActionRoomsSearch,
		Name:        "dirextalk_rooms_search",
		Description: "Search Dirextalk rooms, groups, channels, or contacts.",
		InputSchema: objectSchema(map[string]any{"query": stringSchema(), "type": stringSchema(), "limit": numberSchema()}),
		Effect:      ToolEffectRead,
	},
	{
		Action:      ActionMessagesList,
		Name:        "dirextalk_messages_list",
		Description: "List ordinary messages in an allowed room with optional RFC3339 UTC time range and cursor pagination.",
		InputSchema: objectSchema(map[string]any{"room_id": stringSchema(), "from_time": stringSchema(), "to_time": stringSchema(), "cursor": stringSchema(), "limit": numberSchema()}, "room_id"),
		Effect:      ToolEffectRead,
	},
	{
		Action:      ActionMessagesSend,
		Name:        "dirextalk_messages_send",
		Description: "Send a Matrix message through Dirextalk transport.",
		InputSchema: objectSchema(map[string]any{"room_id": stringSchema(), "msg": stringSchema(), "agent_gateway": boolSchema()}, "room_id", "msg"),
		Effect:      ToolEffectNonIdempotentWrite,
	},
	{
		Action:      ActionRoomMembersList,
		Name:        "dirextalk_room_members_list",
		Description: "List room members.",
		InputSchema: objectSchema(map[string]any{"room_id": stringSchema(), "channel_id": stringSchema(), "status": stringSchema(), "role": stringSchema(), "limit": numberSchema()}),
		Effect:      ToolEffectRead,
	},
	{
		Action:      ActionChannelPostsList,
		Name:        "dirextalk_channel_posts_list",
		Description: "List channel posts with optional RFC3339 UTC time range and cursor pagination.",
		InputSchema: objectSchema(map[string]any{"room_id": stringSchema(), "from_time": stringSchema(), "to_time": stringSchema(), "cursor": stringSchema(), "limit": numberSchema()}, "room_id"),
		Effect:      ToolEffectRead,
	},
	{
		Action:      ActionChannelCommentsList,
		Name:        "dirextalk_channel_comments_list",
		Description: "List channel comments for a post with optional RFC3339 UTC time range and cursor pagination.",
		InputSchema: objectSchema(map[string]any{"post_id": stringSchema(), "from_time": stringSchema(), "to_time": stringSchema(), "cursor": stringSchema(), "limit": numberSchema()}, "post_id"),
		Effect:      ToolEffectRead,
	},
	{
		Action:      ActionChannelCommentsCreate,
		Name:        "dirextalk_channel_comments_create",
		Description: "Create a channel comment through Dirextalk transport.",
		InputSchema: objectSchema(map[string]any{"post_id": stringSchema(), "msg": stringSchema()}, "post_id", "msg"),
		Effect:      ToolEffectNonIdempotentWrite,
	},
}

func objectSchema(properties map[string]any, required ...string) map[string]any {
	schema := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		schema["required"] = append([]string(nil), required...)
	}
	return schema
}

func stringSchema() map[string]any { return map[string]any{"type": "string"} }
func numberSchema() map[string]any { return map[string]any{"type": "number"} }
func boolSchema() map[string]any   { return map[string]any{"type": "boolean"} }
