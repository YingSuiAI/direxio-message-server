package dirextalkdomain

import (
	"encoding/json"
	"strings"
)

// NormalizeMemberMembership keeps the durable ProductCore membership enum
// aligned with Matrix. "joined" is an operation/result status, not a stored
// m.room.member membership value.
func NormalizeMemberMembership(membership string) string {
	membership = strings.ToLower(strings.TrimSpace(membership))
	if membership == "joined" {
		return "join"
	}
	return membership
}

func MemberMembershipJoined(membership string) bool {
	return strings.EqualFold(strings.TrimSpace(membership), "join")
}

const (
	ChannelPostVisibilityPrivate = "private"
	ChannelPostVisibilityPublic  = "public"
)

// NormalizeChannelPostVisibility keeps legacy or malformed projected posts
// private. Request handlers validate explicit client values before calling
// this fallback-oriented normalization.
func NormalizeChannelPostVisibility(visibility string) string {
	if strings.EqualFold(strings.TrimSpace(visibility), ChannelPostVisibilityPublic) {
		return ChannelPostVisibilityPublic
	}
	return ChannelPostVisibilityPrivate
}

type PortalState struct {
	Initialized    bool
	Password       string
	AccessToken    string
	MatrixDeviceID string
	AgentToken     string
	OwnerMXID      string
	AgentRoomID    string
	SystemRoomID   string
	Profile        OwnerProfile
	AgentConfig    AgentConfig
	ClientBuild    ClientBuild
}

type OwnerProfile struct {
	UserID      string `json:"user_id"`
	DisplayName string `json:"display_name"`
	Domain      string `json:"domain"`
	AvatarURL   string `json:"avatar_url"`
	Gender      string `json:"gender"`
	Birthday    string `json:"birthday"`
	Phone       string `json:"phone"`
	Email       string `json:"email"`
}

type ContactRecord struct {
	PeerMXID            string `json:"peer_mxid"`
	DisplayName         string `json:"display_name"`
	DisplayNameOverride bool   `json:"display_name_override,omitempty"`
	AvatarURL           string `json:"avatar_url"`
	Domain              string `json:"domain"`
	RoomID              string `json:"room_id"`
	Status              string `json:"status"`
	Remark              string `json:"remark,omitempty"`
	RequestID           string `json:"-"`
}

type AgentConfig struct {
	DisplayName         string              `json:"display_name"`
	AvatarURL           string              `json:"avatar_url"`
	NativeAgentIdentity AgentIdentityConfig `json:"native_agent_identity"`
	OnlineAgentIdentity AgentIdentityConfig `json:"online_agent_identity"`
	ContextWindow       int64               `json:"context_window"`
	Enabled             bool                `json:"enabled"`
	Model               string              `json:"model"`
	SystemPrompt        string              `json:"system_prompt"`
	MCPBlockedRoomIDs   []string            `json:"mcp_blocked_room_ids"`
	Native              map[string]any      `json:"-"`
}

// AgentIdentityConfig keeps the product-visible Matrix Online Agent identity
// separate from the Native Agent identity shown by the external runtime.
// Legacy top-level display_name/avatar_url fields remain accepted as aliases.
type AgentIdentityConfig struct {
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url"`
}

type Channel struct {
	ChannelID        string `json:"channel_id"`
	RoomID           string `json:"room_id"`
	Name             string `json:"name"`
	Description      string `json:"description"`
	AvatarURL        string `json:"avatar_url"`
	Visibility       string `json:"visibility"`
	JoinPolicy       string `json:"join_policy"`
	ChannelType      string `json:"channel_type"`
	CommentsEnabled  bool   `json:"comments_enabled"`
	Muted            bool   `json:"muted"`
	MemberCount      int64  `json:"member_count"`
	PendingJoinCount int64  `json:"pending_join_count"`
	IsOwned          bool   `json:"is_owned,omitempty"`
	Role             string `json:"role,omitempty"`
	MemberStatus     string `json:"member_status,omitempty"`
}

type ChannelPostRecord struct {
	PostID         string `json:"post_id"`
	ChannelID      string `json:"channel_id"`
	RoomID         string `json:"room_id"`
	EventID        string `json:"event_id"`
	AuthorMXID     string `json:"author_mxid"`
	AuthorName     string `json:"author_name"`
	Body           string `json:"body"`
	MessageType    string `json:"message_type"`
	MediaJSON      string `json:"media_json"`
	Visibility     string `json:"visibility"`
	OriginServerTS int64  `json:"origin_server_ts"`
	CommentCount   int64  `json:"comment_count"`
}

type ChannelCommentRecord struct {
	CommentID         string `json:"comment_id"`
	PostID            string `json:"post_id"`
	ChannelID         string `json:"channel_id"`
	EventID           string `json:"event_id"`
	AuthorMXID        string `json:"author_mxid"`
	AuthorName        string `json:"author_name"`
	Body              string `json:"body"`
	MessageType       string `json:"message_type"`
	MediaJSON         string `json:"media_json"`
	ReplyToCommentID  string `json:"reply_to_comment_id"`
	ReplyToAuthorMXID string `json:"reply_to_author_mxid"`
	MentionsJSON      string `json:"mentions_json"`
	OriginServerTS    int64  `json:"origin_server_ts"`
}

type GroupRecord struct {
	RoomID       string `json:"room_id"`
	Name         string `json:"name"`
	Topic        string `json:"topic"`
	AvatarURL    string `json:"avatar_url"`
	MemberCount  int64  `json:"member_count"`
	InvitePolicy string `json:"invite_policy"`
	Muted        bool   `json:"muted"`
}

type MemberRecord struct {
	RoomID               string `json:"room_id"`
	ChannelID            string `json:"channel_id"`
	UserID               string `json:"user_id"`
	DisplayName          string `json:"display_name"`
	AvatarURL            string `json:"avatar_url"`
	Domain               string `json:"domain"`
	Membership           string `json:"membership"`
	Role                 string `json:"role"`
	Muted                bool   `json:"muted"`
	JoinedAt             int64  `json:"joined_at"`
	RequesterNodeBaseURL string `json:"-"`
	RequestID            string `json:"-"`
}

type ReadMarker struct {
	RoomID              string `json:"room_id"`
	EventID             string `json:"event_id"`
	OriginServerTS      int64  `json:"origin_server_ts"`
	TopologicalPosition int64  `json:"-"`
	StreamPosition      int64  `json:"-"`
}

type ChannelInviteGrant struct {
	GrantID     string `json:"grant_id"`
	ChannelID   string `json:"channel_id"`
	RoomID      string `json:"room_id"`
	ShareRoomID string `json:"share_room_id"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   int64  `json:"created_at"`
}

type BlockRecord struct {
	TargetType  string `json:"target_type"`
	TargetID    string `json:"target_id"`
	RoomID      string `json:"room_id"`
	ChannelID   string `json:"channel_id,omitempty"`
	PeerMXID    string `json:"peer_mxid"`
	DisplayName string `json:"display_name"`
	AvatarURL   string `json:"avatar_url"`
	CreatedAt   int64  `json:"created_at"`
}

// NormalizeBlockTargetType maps supported contact aliases to the stored type.
func NormalizeBlockTargetType(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "friend", "user", "member", "contact":
		return "contact"
	default:
		return ""
	}
}

// BlockKey returns the canonical in-memory identity for a block record.
func BlockKey(targetType, targetID string) string {
	return NormalizeBlockTargetType(targetType) + "|" + strings.TrimSpace(targetID)
}

// DisplayNameFromMXID derives the legacy localpart fallback for an MXID.
func DisplayNameFromMXID(mxid string) string {
	localpart := strings.TrimPrefix(mxid, "@")
	if index := strings.Index(localpart, ":"); index >= 0 {
		localpart = localpart[:index]
	}
	return FallbackString(localpart, mxid)
}

// DomainFromMXID returns the server-name suffix using the legacy permissive
// parser. Callers that require a valid Matrix ID must validate separately.
func DomainFromMXID(mxid string) string {
	trimmed := strings.TrimPrefix(mxid, "@")
	index := strings.Index(trimmed, ":")
	if index < 0 {
		return ""
	}
	index += len(mxid) - len(trimmed)
	if index+1 >= len(mxid) {
		return ""
	}
	return mxid[index+1:]
}

type CallRecord struct {
	CallID        string `json:"call_id"`
	RoomID        string `json:"room_id"`
	RoomType      string `json:"room_type"`
	MediaType     string `json:"media_type"`
	CreatedByMXID string `json:"created_by_mxid"`
	State         string `json:"state"`
	CreatedAt     string `json:"created_at"`
	AnsweredAt    string `json:"answered_at,omitempty"`
	EndedAt       string `json:"ended_at,omitempty"`
	EndedByMXID   string `json:"ended_by_mxid,omitempty"`
	EndReason     string `json:"end_reason,omitempty"`
	DurationMS    int64  `json:"duration_ms,omitempty"`
}

// TerminalCallState reports whether a normalized call state is final.
func TerminalCallState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "ended", "rejected", "missed", "failed":
		return true
	default:
		return false
	}
}

type FavoriteRecord struct {
	ID             int64  `json:"id"`
	EventID        string `json:"event_id"`
	RoomID         string `json:"room_id"`
	SenderID       string `json:"sender_id"`
	SenderName     string `json:"sender_name"`
	Content        string `json:"content"`
	MessageType    string `json:"message_type"`
	OriginServerTS int64  `json:"origin_server_ts"`
	CreatedAt      string `json:"created_at"`
}

type FollowRecord struct {
	Domain    string `json:"domain"`
	CreatedAt string `json:"created_at"`
}

type ReactionRecord struct {
	EventID    string `json:"event_id"`
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	ChannelID  string `json:"channel_id"`
	PostID     string `json:"post_id"`
	CommentID  string `json:"comment_id"`
	Reaction   string `json:"reaction"`
	UserID     string `json:"user_id"`
	Active     bool   `json:"active"`
	CreatedAt  string `json:"created_at"`
}

type ReportRecord struct {
	ReportID            string   `json:"report_id"`
	TargetType          string   `json:"target_type"`
	TargetRoomID        string   `json:"target_room_id"`
	TargetChannelID     string   `json:"target_channel_id,omitempty"`
	TargetName          string   `json:"target_name"`
	ReporterMXID        string   `json:"reporter_mxid"`
	ReporterDisplayName string   `json:"reporter_display_name"`
	Reason              string   `json:"reason"`
	Body                string   `json:"body"`
	ImageURLs           []string `json:"image_urls"`
	SystemRoomID        string   `json:"system_room_id"`
	EventID             string   `json:"event_id"`
	OriginServerTS      int64    `json:"origin_server_ts"`
	CreatedAt           string   `json:"created_at"`
}

type Event struct {
	Seq       int64          `json:"seq"`
	Type      string         `json:"type"`
	RoomID    string         `json:"room_id,omitempty"`
	EventID   string         `json:"event_id,omitempty"`
	DedupeKey string         `json:"-"`
	Payload   map[string]any `json:"payload,omitempty"`
	CreatedAt string         `json:"created_at"`
}

type EventBounds struct {
	MinSeq int64 `json:"min_seq"`
	MaxSeq int64 `json:"max_seq"`
	Count  int64 `json:"count"`
}

func (c *AgentConfig) UnmarshalJSON(data []byte) error {
	type agentConfigJSON struct {
		DisplayName         string              `json:"display_name"`
		AvatarURL           string              `json:"avatar_url"`
		NativeAgentIdentity AgentIdentityConfig `json:"native_agent_identity"`
		OnlineAgentIdentity AgentIdentityConfig `json:"online_agent_identity"`
		ContextWindow       int64               `json:"context_window"`
		Enabled             bool                `json:"enabled"`
		Model               string              `json:"model"`
		SystemPrompt        string              `json:"system_prompt"`
		MCPBlockedRoomIDs   []string            `json:"mcp_blocked_room_ids"`
	}
	var known agentConfigJSON
	if err := json.Unmarshal(data, &known); err != nil {
		return err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	for _, key := range agentConfigKnownJSONKeys() {
		delete(raw, key)
	}
	c.DisplayName = known.DisplayName
	c.AvatarURL = known.AvatarURL
	c.NativeAgentIdentity = known.NativeAgentIdentity
	c.OnlineAgentIdentity = known.OnlineAgentIdentity
	legacyIdentity := AgentIdentityConfig{DisplayName: known.DisplayName, AvatarURL: known.AvatarURL}
	if agentIdentityConfigEmpty(c.NativeAgentIdentity) {
		c.NativeAgentIdentity = legacyIdentity
	}
	if agentIdentityConfigEmpty(c.OnlineAgentIdentity) {
		c.OnlineAgentIdentity = legacyIdentity
	}
	c.ContextWindow = known.ContextWindow
	c.Enabled = known.Enabled
	c.Model = known.Model
	c.SystemPrompt = known.SystemPrompt
	c.MCPBlockedRoomIDs = known.MCPBlockedRoomIDs
	if len(raw) > 0 {
		c.Native = raw
	} else {
		c.Native = nil
	}
	return nil
}

func (c AgentConfig) MarshalJSON() ([]byte, error) {
	out := make(map[string]any, len(c.Native)+9)
	for key, value := range c.Native {
		if !agentConfigKnownJSONKey(key) {
			out[key] = value
		}
	}
	out["display_name"] = c.DisplayName
	out["avatar_url"] = c.AvatarURL
	out["native_agent_identity"] = c.NativeAgentIdentity
	out["online_agent_identity"] = c.OnlineAgentIdentity
	out["context_window"] = c.ContextWindow
	out["enabled"] = c.Enabled
	out["model"] = c.Model
	out["system_prompt"] = c.SystemPrompt
	out["mcp_blocked_room_ids"] = c.MCPBlockedRoomIDs
	return json.Marshal(out)
}

func agentConfigKnownJSONKeys() []string {
	return []string{
		"display_name",
		"avatar_url",
		"native_agent_identity",
		"online_agent_identity",
		"context_window",
		"enabled",
		"model",
		"system_prompt",
		"mcp_blocked_room_ids",
	}
}

func agentIdentityConfigEmpty(identity AgentIdentityConfig) bool {
	return strings.TrimSpace(identity.DisplayName) == "" && strings.TrimSpace(identity.AvatarURL) == ""
}

func agentConfigKnownJSONKey(key string) bool {
	for _, known := range agentConfigKnownJSONKeys() {
		if key == known {
			return true
		}
	}
	return false
}

func (m MemberRecord) MarshalJSON() ([]byte, error) {
	type memberAlias MemberRecord
	return json.Marshal(struct {
		memberAlias
		UserMXID string `json:"user_mxid"`
		Status   string `json:"status"`
	}{
		memberAlias: memberAlias(m),
		UserMXID:    m.UserID,
		Status:      m.Membership,
	})
}

// MemberHidden reports whether a product member record is omitted from current-member views.
func MemberHidden(membership string) bool {
	switch strings.ToLower(strings.TrimSpace(membership)) {
	case "leave", "left", "remove", "removed", "reject", "rejected", "ban", "banned":
		return true
	default:
		return false
	}
}

// ProductOwnerRole recognizes the only elevated product role.
func ProductOwnerRole(role string) bool {
	return strings.EqualFold(strings.TrimSpace(role), "owner")
}

// NormalizeProductMemberRole maps all non-owner roles to the stable member role.
func NormalizeProductMemberRole(role string) string {
	if ProductOwnerRole(role) {
		return "owner"
	}
	return "member"
}
