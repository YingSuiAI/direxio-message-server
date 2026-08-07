package dirextalkstate

import (
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
)

const (
	RoomTypeDirect  = productpolicy.DirextalkRoomTypeDirect
	RoomTypeGroup   = productpolicy.DirextalkRoomTypeGroup
	RoomTypeChannel = productpolicy.DirextalkRoomTypeChannel

	RoomProfileEventType         = productpolicy.DirextalkRoomProfileEventType
	MemberPolicyEventType        = productpolicy.DirextalkMemberPolicyEventType
	JoinRequestEventType         = productpolicy.DirextalkJoinRequestEventType
	ChannelPostSettingsEventType = productpolicy.DirextalkChannelPostSettingsEventType
)

type StateEvent struct {
	Type     string
	StateKey string
	Content  map[string]any
}

type DirectRoomProfileInput struct {
	Name                 string
	RequesterMXID        string
	TargetMXID           string
	RequesterDisplayName string
	RequesterAvatarURL   string
	Remark               string
	Dissolved            bool
	AccountDeleted       bool
	DeletedMXID          string
}

type GroupProfile struct {
	RoomID       string
	Name         string
	Topic        string
	AvatarURL    string
	InvitePolicy string
	Muted        bool
}

type ChannelPostSettingsInput struct {
	PostID          string
	ChannelID       string
	RoomID          string
	PostEventID     string
	Visibility      string
	CommentsEnabled bool
	UpdatedAt       time.Time
}

func DirectRoomProfile(input DirectRoomProfileInput) StateEvent {
	requesterMXID := strings.TrimSpace(input.RequesterMXID)
	content := map[string]any{
		"room_type":      RoomTypeDirect,
		"name":           strings.TrimSpace(input.Name),
		"visibility":     "private",
		"join_policy":    "invite",
		"invite_policy":  "owner",
		"requester_mxid": requesterMXID,
		"target_mxid":    strings.TrimSpace(input.TargetMXID),
		"display_name":   strings.TrimSpace(input.RequesterDisplayName),
		"avatar_url":     strings.TrimSpace(input.RequesterAvatarURL),
		"domain":         domainFromMXID(requesterMXID),
		"remark":         strings.TrimSpace(input.Remark),
		"dissolved":      input.Dissolved,
	}
	if input.AccountDeleted {
		content["account_deleted"] = true
		content["deleted_mxid"] = strings.TrimSpace(input.DeletedMXID)
	}
	return StateEvent{Type: RoomProfileEventType, Content: content}
}

func GroupRoomProfile(group GroupProfile, dissolved bool) StateEvent {
	return StateEvent{
		Type: RoomProfileEventType,
		Content: map[string]any{
			"room_type":     RoomTypeGroup,
			"room_id":       group.RoomID,
			"name":          group.Name,
			"topic":         group.Topic,
			"avatar_url":    group.AvatarURL,
			"invite_policy": fallbackString(group.InvitePolicy, "member"),
			"muted":         group.Muted,
			"dissolved":     dissolved,
		},
	}
}

func ChannelRoomProfile(ch dirextalkdomain.Channel, dissolved bool) StateEvent {
	return StateEvent{
		Type: RoomProfileEventType,
		Content: map[string]any{
			"room_type":        RoomTypeChannel,
			"channel_id":       ch.ChannelID,
			"room_id":          ch.RoomID,
			"name":             ch.Name,
			"description":      ch.Description,
			"avatar_url":       ch.AvatarURL,
			"visibility":       fallbackString(ch.Visibility, "private"),
			"join_policy":      fallbackString(ch.JoinPolicy, "invite"),
			"channel_type":     fallbackString(ch.ChannelType, "post"),
			"comments_enabled": ch.CommentsEnabled,
			"muted":            ch.Muted,
			"dissolved":        dissolved,
		},
	}
}

func ChannelPostSettings(input ChannelPostSettingsInput) StateEvent {
	updatedAt := input.UpdatedAt.UTC()
	return StateEvent{
		Type:     ChannelPostSettingsEventType,
		StateKey: strings.TrimSpace(input.PostID),
		Content: map[string]any{
			"post_id":          strings.TrimSpace(input.PostID),
			"channel_id":       strings.TrimSpace(input.ChannelID),
			"room_id":          strings.TrimSpace(input.RoomID),
			"post_event_id":    strings.TrimSpace(input.PostEventID),
			"visibility":       fallbackString(input.Visibility, "private"),
			"comments_enabled": input.CommentsEnabled,
			"updated_at":       updatedAt.Format(time.RFC3339Nano),
		},
	}
}

func MemberPolicyState(member dirextalkdomain.MemberRecord) StateEvent {
	return StateEvent{
		Type:     MemberPolicyEventType,
		StateKey: productpolicy.UserStateKey(member.UserID),
		Content: map[string]any{
			"role":    fallbackString(member.Role, "member"),
			"muted":   member.Muted,
			"user_id": member.UserID,
			"room_id": member.RoomID,
		},
	}
}

func JoinRequestState(roomID, userID, status, reason, requestID string, at time.Time) StateEvent {
	timestamp := at.UTC().Format(time.RFC3339Nano)
	content := map[string]any{
		"status":     strings.ToLower(strings.TrimSpace(status)),
		"room_id":    roomID,
		"user_id":    userID,
		"created_at": timestamp,
		"updated_at": timestamp,
	}
	if strings.TrimSpace(reason) != "" {
		content["reason"] = strings.TrimSpace(reason)
	}
	if strings.TrimSpace(requestID) != "" {
		content["request_id"] = strings.TrimSpace(requestID)
	}
	return StateEvent{
		Type:     JoinRequestEventType,
		StateKey: productpolicy.UserStateKey(userID),
		Content:  content,
	}
}

func fallbackString(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value != "" {
		return value
	}
	return fallback
}

func domainFromMXID(mxid string) string {
	mxid = strings.TrimSpace(mxid)
	if !strings.HasPrefix(mxid, "@") {
		return ""
	}
	parts := strings.SplitN(mxid[1:], ":", 2)
	if len(parts) != 2 {
		return ""
	}
	return parts[1]
}
