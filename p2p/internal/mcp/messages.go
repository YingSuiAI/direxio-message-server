package mcp

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	matrixhistory "github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmatrix"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

func (m *Module) messagesSend(ctx context.Context, params map[string]any) (any, *dirextalkmcp.Error) {
	roomID := dirextalkmcp.TrimString(params["room_id"])
	msg := fallback(dirextalkmcp.TrimString(params["msg"]), dirextalkmcp.TrimString(params["text"]))
	if roomID == "" {
		return nil, dirextalkmcp.BadRequest("room_id is required")
	}
	if mcpErr := m.requireRoomAllowed(roomID); mcpErr != nil {
		return nil, mcpErr
	}
	if msg == "" {
		return nil, dirextalkmcp.BadRequest("msg is required")
	}
	gatewayMarked := gatewayMarked(params)
	identity := m.identity()
	senderMXID := identity.OwnerMXID
	if gatewayMarked {
		senderMXID = identity.AgentMXID
	}
	if !gatewayMarked && strings.TrimSpace(identity.AgentRoomID) != "" && roomID == strings.TrimSpace(identity.AgentRoomID) {
		return nil, dirextalkmcp.BadRequest("mcp.messages.send cannot send owner messages to the agent room; use the Matrix agent gateway")
	}
	if mcpErr := m.requireJoinedRoomForUser(ctx, roomID, senderMXID); mcpErr != nil {
		return nil, mcpErr
	}
	if m.matrix == nil {
		return nil, internalError(errors.New("matrix transport is unavailable"))
	}
	content := map[string]any{
		"msgtype": "m.text",
		"body":    msg,
	}
	if gatewayMarked {
		content[agentGatewayContentKey] = true
		if source := dirextalkmcp.TrimString(params["gateway_source"]); source != "" {
			content[agentGatewaySourceContentKey] = source
		} else {
			content[agentGatewaySourceContentKey] = "agent_gateway"
		}
	}
	result, err := m.sendMatrixMutation(ctx, dirextalktransport.SendMessageRequest{
		SenderMXID:  senderMXID,
		RoomID:      roomID,
		MessageType: "text",
		Timestamp:   m.now(),
		Content:     content,
		LogicalID:   roomID,
	}, "product.messages.v1", "send")
	if err != nil {
		return nil, transportWriteError(err)
	}
	return map[string]any{
		"ok":         true,
		"room_id":    roomID,
		"event_id":   result.EventID,
		"created_at": dirextalkmcp.FormatTime(result.OriginServerTS),
	}, nil
}

func gatewayMarked(params map[string]any) bool {
	return actionbase.Bool(params["agent_gateway"]) ||
		actionbase.Bool(params["gateway"]) ||
		actionbase.Bool(params[agentGatewayContentKey]) ||
		dirextalkmcp.TrimString(params["gateway_source"]) != ""
}

func (m *Module) messagesList(ctx context.Context, params map[string]any) (any, *dirextalkmcp.Error) {
	roomID := dirextalkmcp.TrimString(params["room_id"])
	if roomID == "" {
		return nil, dirextalkmcp.BadRequest("room_id is required")
	}
	if mcpErr := m.requireRoomAllowed(roomID); mcpErr != nil {
		return nil, mcpErr
	}
	page, mcpErr := dirextalkmcp.PageFromParams(params, dirextalkmcp.ActionMessagesList, roomID)
	if mcpErr != nil {
		return nil, mcpErr
	}
	if mcpErr := m.requireJoinedRoom(ctx, roomID); mcpErr != nil {
		return nil, mcpErr
	}
	reader := m.messageReader()
	if reader == nil {
		return nil, internalError(errors.New("MCP message reader is unavailable"))
	}
	pageResult, err := reader.ListOrdinaryMessages(ctx, roomID, page)
	if err != nil {
		return nil, matrixMessageReadError(err)
	}
	name, mcpErr := m.messagesRoomName(ctx, roomID)
	if mcpErr != nil {
		return nil, mcpErr
	}
	messages := m.enrichMessageSenders(ctx, roomID, pageResult.Messages)
	result := map[string]any{"room_id": roomID, "name": name, "messages": messages}
	lastTS, lastID := lastMessageKey(messages)
	if mcpErr := dirextalkmcp.AttachPagination(result, dirextalkmcp.ActionMessagesList, roomID, page, pageResult.HasMore, lastTS, lastID); mcpErr != nil {
		return nil, mcpErr
	}
	return result, nil
}

func (m *Module) requireJoinedRoom(ctx context.Context, roomID string) *dirextalkmcp.Error {
	identity := m.identity()
	if roomID == strings.TrimSpace(identity.AgentRoomID) && roomID != "" {
		return m.requireMatrixJoinedUser(ctx, roomID, identity.OwnerMXID)
	}
	record, ok, err := m.conversations.GetRecord(ctx, "", roomID)
	if err != nil {
		return internalError(err)
	}
	if !ok {
		return dirextalkmcp.StatusError(http.StatusForbidden, "room is not joined for MCP access")
	}
	view, err := m.conversations.View(ctx, record)
	if err != nil {
		return internalError(err)
	}
	if !joinedConversation(view) {
		return dirextalkmcp.StatusError(http.StatusForbidden, "room is not joined for MCP access")
	}
	return nil
}

func (m *Module) requireJoinedRoomForUser(ctx context.Context, roomID, userID string) *dirextalkmcp.Error {
	identity := m.identity()
	if strings.TrimSpace(userID) == strings.TrimSpace(identity.OwnerMXID) {
		return m.requireJoinedRoom(ctx, roomID)
	}
	return m.requireMatrixJoinedUser(ctx, roomID, userID)
}

func (m *Module) requireMatrixJoinedUser(ctx context.Context, roomID, userID string) *dirextalkmcp.Error {
	roomID = strings.TrimSpace(roomID)
	userID = strings.TrimSpace(userID)
	if roomID == "" || userID == "" {
		return dirextalkmcp.StatusError(http.StatusForbidden, "room is not joined for MCP access")
	}
	members, err := m.matrixRoomMembers(ctx, roomID)
	if err != nil {
		return internalError(err)
	}
	for _, member := range members {
		if strings.TrimSpace(member.UserID) == userID && dirextalkdomain.MemberMembershipJoined(member.Membership) {
			return nil
		}
	}
	return dirextalkmcp.StatusError(http.StatusForbidden, "room is not joined for MCP access")
}

func matrixMessageReadError(err error) *dirextalkmcp.Error {
	var statusErr matrixhistory.StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusForbidden, http.StatusUnauthorized:
			return dirextalkmcp.StatusError(http.StatusForbidden, "room is not allowed for MCP message access")
		case http.StatusNotFound:
			return dirextalkmcp.StatusError(http.StatusNotFound, "room messages were not found")
		default:
			if statusErr.StatusCode >= 400 && statusErr.StatusCode < 500 {
				return dirextalkmcp.StatusError(statusErr.StatusCode, statusErr.Error())
			}
		}
	}
	return internalError(err)
}

func (m *Module) messagesRoomName(ctx context.Context, roomID string) (string, *dirextalkmcp.Error) {
	identity := m.identity()
	if roomID != "" && roomID == strings.TrimSpace(identity.AgentRoomID) {
		return m.agentRoomName(), nil
	}
	record, ok, err := m.conversations.GetRecord(ctx, "", roomID)
	if err != nil {
		return "", internalError(err)
	}
	if ok {
		view, err := m.conversations.View(ctx, record)
		if err != nil {
			return "", internalError(err)
		}
		return fallback(view.Title, roomID), nil
	}
	return roomID, nil
}

func (m *Module) enrichMessageSenders(ctx context.Context, roomID string, messages []dirextalkmcp.MessageSummary) []dirextalkmcp.MessageSummary {
	if roomID == "" || len(messages) == 0 {
		return messages
	}
	displayNames := m.senderDisplayNames(ctx, roomID)
	profileResolver := m.profileResolver()
	profileCache := map[string]matrixhistory.Profile{}
	for index := range messages {
		mxid := strings.TrimSpace(messages[index].SenderMXID)
		if mxid == "" {
			continue
		}
		displayName := displayNames[mxid]
		if needsProfileDisplayName(mxid, displayName) {
			if profile, ok := resolveMatrixProfile(ctx, profileResolver, profileCache, mxid); ok && strings.TrimSpace(profile.DisplayName) != "" {
				displayName = profile.DisplayName
				displayNames[mxid] = displayName
			}
		}
		if displayName == "" {
			continue
		}
		messages[index].SenderDisplayName = displayName
		messages[index].Sender = displayName
	}
	return messages
}

func (m *Module) senderDisplayNames(ctx context.Context, roomID string) map[string]string {
	names := map[string]string{}
	setName := func(userID, displayName string) {
		userID = strings.TrimSpace(userID)
		displayName = strings.TrimSpace(displayName)
		if userID == "" || displayName == "" {
			return
		}
		current := strings.TrimSpace(names[userID])
		if current == "" || current == displayNameFromMXID(userID) {
			names[userID] = displayName
		}
	}

	identity := m.identity()
	setName(identity.OwnerMXID, identity.OwnerProfile.DisplayName)
	setName(identity.AgentMXID, identity.AgentDisplayName)

	if record, ok, err := m.conversations.GetRecord(ctx, "", roomID); err == nil && ok && record.Kind == "direct" {
		if view, viewErr := m.conversations.View(ctx, record); viewErr == nil {
			setName(view.PeerMXID, view.Title)
			setName(identity.OwnerMXID, identity.OwnerProfile.DisplayName)
		}
	}

	if m.members != nil {
		if members, err := m.members.ListMembers(ctx, roomID, ""); err == nil {
			for _, member := range members {
				setName(member.UserID, member.DisplayName)
			}
		}
	}
	if members, err := m.matrixRoomMembers(ctx, roomID); err == nil {
		for _, member := range members {
			setName(member.UserID, member.DisplayName)
		}
	}
	if profileResolver := m.profileResolver(); profileResolver != nil {
		profileCache := map[string]matrixhistory.Profile{}
		for userID, displayName := range names {
			if !needsProfileDisplayName(userID, displayName) {
				continue
			}
			if profile, ok := resolveMatrixProfile(ctx, profileResolver, profileCache, userID); ok {
				setName(userID, profile.DisplayName)
			}
		}
	}

	if roomID == strings.TrimSpace(identity.AgentRoomID) {
		setName(identity.AgentMXID, identity.AgentDisplayName)
	}
	setName(identity.OwnerMXID, identity.OwnerProfile.DisplayName)
	return names
}

func lastMessageKey(messages []dirextalkmcp.MessageSummary) (int64, string) {
	if len(messages) == 0 {
		return 0, ""
	}
	last := messages[len(messages)-1]
	return last.OriginServerTS, last.EventID
}
