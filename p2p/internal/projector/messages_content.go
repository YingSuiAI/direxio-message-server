package projector

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	channelsmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/channels"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/types"
)

func (m *Module) projectMessage(ctx context.Context, event *types.HeaderedEvent) error {
	content := map[string]any{}
	if err := json.Unmarshal(event.Content(), &content); err != nil {
		return err
	}
	if m.isAgentRoom(event.RoomID().String()) {
		return nil
	}
	if !m.shouldProjectRoomMessage(ctx, event.RoomID().String(), content) {
		return nil
	}
	body := textValue(content["body"])
	msgType := fallbackText(textValue(content["client_type"]), textValue(content["msgtype"]))
	if msgType == "" {
		msgType = "text"
	}
	kind := textValue(content["p2p_kind"])
	if kind == "" {
		if err := m.projectConversationActivity(ctx, event, body, msgType); err != nil {
			return err
		}
	}
	switch kind {
	case "channel_post":
		return m.projectChannelPost(ctx, event, content, body, msgType)
	case "channel_comment":
		return m.projectChannelComment(ctx, event, content, body, msgType)
	default:
		return nil
	}
}

func (m *Module) isAgentRoom(roomID string) bool {
	roomID = strings.TrimSpace(roomID)
	return roomID != "" && roomID == strings.TrimSpace(m.identity().AgentRoomID)
}

func (m *Module) shouldProjectRoomMessage(ctx context.Context, roomID string, content map[string]any) bool {
	if textValue(content["p2p_kind"]) != "" || textValue(content["channel_id"]) != "" {
		return true
	}
	if m.channelIDForRoom(ctx, roomID) != "" {
		return true
	}
	if m.dependencies.Groups != nil {
		if _, ok, err := m.dependencies.Groups.ByRoom(ctx, roomID); err == nil && ok {
			return true
		}
	}
	if m.dependencies.Contacts != nil {
		if _, ok, err := m.dependencies.Contacts.LookupByRoom(ctx, roomID); err == nil && ok {
			return true
		}
	}
	return false
}

func (m *Module) projectConversationActivity(ctx context.Context, event *types.HeaderedEvent, body, msgType string) error {
	if m.dependencies.Conversations == nil {
		return errors.New("conversation projection port is not configured")
	}
	record, ok, err := m.dependencies.Conversations.GetRecord(ctx, "", event.RoomID().String())
	if err != nil || !ok {
		return err
	}
	record.LastEventID = event.EventID()
	record.LastActivityAt = int64(event.OriginServerTS())
	record.LastMessage = conversationActivityPreview(body, msgType)
	record.UpdatedAt = m.now().UTC().UnixMilli()
	return m.dependencies.Conversations.Save(ctx, record)
}

func conversationActivityPreview(body, msgType string) string {
	body = strings.TrimSpace(body)
	if body != "" {
		return body
	}
	switch strings.ToLower(strings.TrimSpace(msgType)) {
	case "m.image", "image":
		return "图片"
	case "m.video", "video":
		return "视频"
	case "m.audio", "audio":
		return "语音"
	case "m.file", "file":
		return "文件"
	default:
		return ""
	}
}

func (m *Module) projectChannelPost(ctx context.Context, event *types.HeaderedEvent, content map[string]any, body, msgType string) error {
	if m.dependencies.ChannelContent == nil {
		return errors.New("channel content module is not configured")
	}
	return m.dependencies.ChannelContent.ProjectPost(ctx, channelsmodule.ProjectionEvent{
		RoomID:         event.RoomID().String(),
		EventID:        event.EventID(),
		SenderMXID:     string(event.SenderID()),
		OriginServerTS: int64(event.OriginServerTS()),
		Content:        content,
		Body:           body,
		MessageType:    msgType,
	})
}

func (m *Module) projectChannelPostSettings(ctx context.Context, event *types.HeaderedEvent) error {
	if m.dependencies.ChannelContent == nil {
		return errors.New("channel content module is not configured")
	}
	content := map[string]any{}
	if err := json.Unmarshal(event.Content(), &content); err != nil {
		return err
	}
	stateKey := ""
	if event.StateKey() != nil {
		stateKey = *event.StateKey()
	}
	return m.dependencies.ChannelContent.ProjectPostSettings(ctx, channelsmodule.ProjectionEvent{
		RoomID: event.RoomID().String(), EventID: event.EventID(), StateKey: stateKey,
		SenderMXID: string(event.SenderID()), OriginServerTS: int64(event.OriginServerTS()),
		Content: content,
	})
}

func (m *Module) projectChannelComment(ctx context.Context, event *types.HeaderedEvent, content map[string]any, body, msgType string) error {
	if m.dependencies.ChannelContent == nil {
		return errors.New("channel content module is not configured")
	}
	return m.dependencies.ChannelContent.ProjectComment(ctx, channelsmodule.ProjectionEvent{
		RoomID:         event.RoomID().String(),
		EventID:        event.EventID(),
		SenderMXID:     string(event.SenderID()),
		OriginServerTS: int64(event.OriginServerTS()),
		Content:        content,
		Body:           body,
		MessageType:    msgType,
	})
}

func (m *Module) projectReaction(ctx context.Context, event *types.HeaderedEvent) error {
	content := map[string]any{}
	if err := json.Unmarshal(event.Content(), &content); err != nil {
		return err
	}
	if m.dependencies.ChannelContent == nil {
		return errors.New("channel content module is not configured")
	}
	return m.dependencies.ChannelContent.ProjectReaction(ctx, channelsmodule.ProjectionEvent{
		RoomID:         event.RoomID().String(),
		EventID:        event.EventID(),
		SenderMXID:     string(event.SenderID()),
		OriginServerTS: int64(event.OriginServerTS()),
		Content:        content,
	})
}

func (m *Module) removeProjectedEvent(ctx context.Context, eventID string) error {
	if eventID == "" {
		return nil
	}
	if m.dependencies.ChannelContent == nil {
		return errors.New("channel content module is not configured")
	}
	removed, err := m.dependencies.ChannelContent.RemoveProjectedEvent(ctx, eventID)
	if err != nil {
		return err
	}
	if !removed {
		return nil
	}
	return m.appendEvent(ctx, dirextalkdomain.Event{
		Type:      "room.redaction.projected",
		EventID:   eventID,
		DedupeKey: projectedEventDedupeKey("room.redaction.projected", eventID, ""),
		Payload:   map[string]any{"redacted_event_id": eventID},
	})
}

func (m *Module) channelIDForRoom(ctx context.Context, roomID string) string {
	roomID = strings.TrimSpace(roomID)
	if roomID == "" || m.dependencies.Channels == nil {
		return ""
	}
	channel, ok, err := m.dependencies.Channels.ByIDOrRoom(ctx, "", roomID)
	if err == nil && ok {
		return channel.ChannelID
	}
	return ""
}
