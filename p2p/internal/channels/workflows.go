package channels

import (
	"context"
	"errors"
	"net/http"
	"strings"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

func (m *Module) Create(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	channelID := params.String("channel_id")
	if channelID == "" {
		if m.config.NewChannelID == nil {
			return nil, actionbase.InternalError(errors.New("channel ID generator is not configured"))
		}
		channelID = strings.TrimSpace(m.config.NewChannelID())
	}
	roomID := params.String("room_id")
	existingRoom := roomID != ""
	channel := Channel{
		ChannelID:        channelID,
		RoomID:           roomID,
		Name:             fallback(params.String("name"), channelID),
		Description:      params.String("description"),
		AvatarURL:        params.String("avatar_url"),
		Visibility:       fallback(params.String("visibility"), "public"),
		JoinPolicy:       fallback(params.String("join_policy"), "open"),
		ChannelType:      fallback(params.String("channel_type"), "post"),
		CommentsEnabled:  true,
		MemberCount:      1,
		PendingJoinCount: 0,
		IsOwned:          true,
		Role:             "owner",
		MemberStatus:     "join",
	}
	if _, exists := raw["comments_enabled"]; exists {
		channel.CommentsEnabled = params.Bool("comments_enabled")
	}
	if roomID == "" {
		if m.config.CreateRoom == nil {
			return nil, actionbase.InternalError(errors.New("channel room creator is not configured"))
		}
		var actionErr *actionbase.Error
		roomID, actionErr = m.config.CreateRoom(ctx, channel)
		if actionErr != nil {
			return nil, actionErr
		}
	}
	channel.RoomID = roomID
	if err := m.Save(ctx, channel); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if m.config.SaveOwnerMember == nil {
		return nil, actionbase.InternalError(errors.New("channel owner member writer is not configured"))
	}
	if err := m.config.SaveOwnerMember(ctx, channel.RoomID, channel.ChannelID); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if existingRoom && m.config.PublishHistory != nil {
		if err := m.config.PublishHistory(ctx, channel); err != nil {
			return nil, actionbase.InternalError(err)
		}
	}
	return channel, nil
}

func (m *Module) Update(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	channelID := params.String("channel_id")
	roomID := params.String("room_id")
	if channelID == "" && roomID == "" {
		return nil, actionbase.BadRequest("channel_id or room_id is required")
	}
	channel, ok, actionErr := m.lookup(ctx, channelID, roomID)
	if actionErr != nil || !ok {
		return nil, actionErr
	}
	if m.config.RequireOwner == nil {
		return nil, actionbase.InternalError(errors.New("channel owner policy is not configured"))
	}
	if actionErr := m.config.RequireOwner(ctx, channel.RoomID); actionErr != nil {
		return nil, actionErr
	}
	if name := params.String("name"); name != "" {
		channel.Name = name
	}
	if _, exists := raw["description"]; exists {
		channel.Description = params.String("description")
	}
	if _, exists := raw["avatar_url"]; exists {
		channel.AvatarURL = params.String("avatar_url")
	}
	if visibility := params.String("visibility"); visibility != "" {
		channel.Visibility = visibility
	}
	if joinPolicy := params.String("join_policy"); joinPolicy != "" {
		channel.JoinPolicy = joinPolicy
	}
	if _, exists := raw["comments_enabled"]; exists {
		channel.CommentsEnabled = params.Bool("comments_enabled")
	}
	if _, exists := raw["muted"]; exists {
		channel.Muted = params.Bool("muted")
	}
	if err := m.Save(ctx, channel); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if err := m.publishState(ctx, channel, false); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return channel, nil
}

func (m *Module) handleList(ctx context.Context, _ map[string]any) (any, *actionbase.Error) {
	ownerMXID := ""
	if m.config.OwnerMXID != nil {
		ownerMXID = m.config.OwnerMXID()
	}
	channels, err := m.ListJoined(ctx, ownerMXID)
	if err != nil {
		return map[string]any{"channels": []Channel{}}, nil
	}
	return map[string]any{"channels": channels}, nil
}

func (m *Module) policyHandler(muted bool) actionbase.Handler {
	return func(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
		params := actionbase.Params(raw)
		channelID := params.String("channel_id")
		roomID := params.String("room_id")
		if channelID == "" && roomID == "" {
			return nil, actionbase.BadRequest("channel_id or room_id is required")
		}
		channel, ok, actionErr := m.lookup(ctx, channelID, roomID)
		if actionErr != nil || !ok {
			return nil, actionErr
		}
		channel.Muted = muted
		if err := m.Save(ctx, channel); err != nil {
			return nil, actionbase.InternalError(err)
		}
		if m.config.SetMemberMute != nil {
			if actionErr := m.config.SetMemberMute(ctx, channel.RoomID, channel.ChannelID, muted); actionErr != nil {
				return nil, actionErr
			}
		}
		return map[string]any{
			"status":     "ok",
			"channel_id": channel.ChannelID,
			"room_id":    channel.RoomID,
			"muted":      channel.Muted,
			"channel":    channel,
		}, nil
	}
}

func (m *Module) Dissolve(ctx context.Context, raw map[string]any) (any, *actionbase.Error) {
	params := actionbase.Params(raw)
	roomID := params.String("room_id")
	channelID := params.String("channel_id")
	if roomID == "" && channelID == "" {
		return nil, actionbase.BadRequest("channel_id or room_id is required")
	}
	channel, ok, actionErr := m.lookup(ctx, channelID, roomID)
	if actionErr != nil || !ok {
		return nil, actionErr
	}
	if m.config.RequireOwner == nil {
		return nil, actionbase.InternalError(errors.New("channel owner policy is not configured"))
	}
	if actionErr := m.config.RequireOwner(ctx, channel.RoomID); actionErr != nil {
		return nil, actionErr
	}
	if err := m.publishState(ctx, channel, true); err != nil {
		return nil, actionbase.InternalError(err)
	}
	if err := m.Delete(ctx, channel.ChannelID); err != nil {
		return nil, actionbase.InternalError(err)
	}
	return map[string]any{"status": "ok", "channel": channel}, nil
}

func (m *Module) lookup(ctx context.Context, channelID, roomID string) (Channel, bool, *actionbase.Error) {
	channel, ok, err := m.ByIDOrRoom(ctx, channelID, roomID)
	if err != nil {
		return Channel{}, false, actionbase.InternalError(err)
	}
	if !ok {
		return Channel{}, false, actionbase.StatusError(http.StatusNotFound, "channel not found")
	}
	return channel, true, nil
}

func (m *Module) publishState(ctx context.Context, channel Channel, dissolved bool) error {
	if m.config.PublishState == nil || strings.TrimSpace(channel.RoomID) == "" {
		return nil
	}
	return m.config.PublishState(ctx, channel, dissolved)
}

func fallback(value, fallbackValue string) string {
	if strings.TrimSpace(value) != "" {
		return value
	}
	return fallbackValue
}
