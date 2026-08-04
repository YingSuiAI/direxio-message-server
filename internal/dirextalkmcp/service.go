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

type Tool struct {
	Action      string
	Name        string
	Description string
	InputSchema map[string]any
	Write       bool
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
	},
	{
		Action:      ActionContactsSearch,
		Name:        "dirextalk_contacts_search",
		Description: "Search Dirextalk contacts.",
		InputSchema: objectSchema(map[string]any{"query": stringSchema(), "limit": numberSchema()}),
	},
	{
		Action:      ActionRoomsSearch,
		Name:        "dirextalk_rooms_search",
		Description: "Search Dirextalk rooms, groups, channels, or contacts.",
		InputSchema: objectSchema(map[string]any{"query": stringSchema(), "type": stringSchema(), "limit": numberSchema()}),
	},
	{
		Action:      ActionMessagesList,
		Name:        "dirextalk_messages_list",
		Description: "List ordinary messages in an allowed room with optional RFC3339 UTC time range and cursor pagination.",
		InputSchema: objectSchema(map[string]any{"room_id": stringSchema(), "from_time": stringSchema(), "to_time": stringSchema(), "cursor": stringSchema(), "limit": numberSchema()}, "room_id"),
	},
	{
		Action:      ActionMessagesSend,
		Name:        "dirextalk_messages_send",
		Description: "Send a Matrix message through Dirextalk transport.",
		InputSchema: objectSchema(map[string]any{"room_id": stringSchema(), "msg": stringSchema(), "agent_gateway": boolSchema()}, "room_id", "msg"),
		Write:       true,
	},
	{
		Action:      ActionRoomMembersList,
		Name:        "dirextalk_room_members_list",
		Description: "List room members.",
		InputSchema: objectSchema(map[string]any{"room_id": stringSchema(), "channel_id": stringSchema(), "status": stringSchema(), "role": stringSchema(), "limit": numberSchema()}),
	},
	{
		Action:      ActionChannelPostsList,
		Name:        "dirextalk_channel_posts_list",
		Description: "List channel posts with optional RFC3339 UTC time range and cursor pagination.",
		InputSchema: objectSchema(map[string]any{"room_id": stringSchema(), "from_time": stringSchema(), "to_time": stringSchema(), "cursor": stringSchema(), "limit": numberSchema()}, "room_id"),
	},
	{
		Action:      ActionChannelCommentsList,
		Name:        "dirextalk_channel_comments_list",
		Description: "List channel comments for a post with optional RFC3339 UTC time range and cursor pagination.",
		InputSchema: objectSchema(map[string]any{"post_id": stringSchema(), "from_time": stringSchema(), "to_time": stringSchema(), "cursor": stringSchema(), "limit": numberSchema()}, "post_id"),
	},
	{
		Action:      ActionChannelCommentsCreate,
		Name:        "dirextalk_channel_comments_create",
		Description: "Create a channel comment through Dirextalk transport.",
		InputSchema: objectSchema(map[string]any{"post_id": stringSchema(), "msg": stringSchema()}, "post_id", "msg"),
		Write:       true,
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
