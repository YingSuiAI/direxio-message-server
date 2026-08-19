package mcp

import (
	"context"
	"sort"
	"strconv"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
)

func (m *Module) roomsSearch(ctx context.Context, params map[string]any) (any, *dirextalkmcp.Error) {
	query := strings.ToLower(dirextalkmcp.TrimString(params["query"]))
	kind := strings.ToLower(dirextalkmcp.TrimString(params["type"]))
	if kind == "" {
		kind = dirextalkmcp.RoomsSearchTypeAll
	}
	if !dirextalkmcp.ValidRoomsSearchType(kind) {
		return nil, dirextalkmcp.BadRequest("type must be contact, group, channel, or all")
	}
	records, err := m.conversations.ListRecords(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	rooms := make([]dirextalkmcp.RoomSummary, 0, len(records))
	for _, record := range records {
		view, err := m.conversations.View(ctx, record)
		if err != nil {
			return nil, internalError(err)
		}
		if !joinedConversation(view) {
			continue
		}
		summary := roomSummaryFromConversation(view)
		summary = m.enrichRoomSummaryWithMatrixMemberCount(ctx, summary)
		if summary.RoomID == "" {
			continue
		}
		if !m.Service().RoomAllowed(summary.RoomID) {
			continue
		}
		if kind != dirextalkmcp.RoomsSearchTypeAll && summary.Type != kind {
			continue
		}
		if query != "" && !roomMatches(summary, query) {
			continue
		}
		rooms = append(rooms, summary)
	}
	sort.SliceStable(rooms, func(i, j int) bool {
		if rooms[i].LastActivityTS == rooms[j].LastActivityTS {
			return rooms[i].Name < rooms[j].Name
		}
		return rooms[i].LastActivityTS > rooms[j].LastActivityTS
	})
	limit := dirextalkmcp.Limit(params)
	if len(rooms) > limit {
		rooms = rooms[:limit]
	}
	return map[string]any{"rooms": rooms}, nil
}

func joinedConversation(view dirextalkdomain.ConversationView) bool {
	if view.Lifecycle != dirextalkdomain.ConversationLifecycleActive {
		return false
	}
	return dirextalkdomain.MemberMembershipJoined(view.Membership)
}

func (m *Module) contactsList(ctx context.Context, params map[string]any) (any, *dirextalkmcp.Error) {
	return m.contactsSearch(ctx, params)
}

func (m *Module) contactsSearch(ctx context.Context, params map[string]any) (any, *dirextalkmcp.Error) {
	query := strings.ToLower(dirextalkmcp.TrimString(params["query"]))
	contacts, err := m.contacts.ListVisible(ctx)
	if err != nil {
		return nil, internalError(err)
	}
	summaries := make([]dirextalkmcp.ContactSummary, 0, len(contacts))
	for _, contact := range contacts {
		if !strings.EqualFold(strings.TrimSpace(contact.Status), "accepted") || strings.EqualFold(strings.TrimSpace(contact.Status), "deleted") {
			continue
		}
		summary := contactSummary(contact)
		if summary.PeerMXID == "" || summary.RoomID == "" {
			continue
		}
		record, ok, err := m.conversations.GetRecord(ctx, "", summary.RoomID)
		if err != nil {
			return nil, internalError(err)
		}
		if !ok {
			continue
		}
		view, err := m.conversations.View(ctx, record)
		if err != nil {
			return nil, internalError(err)
		}
		if !joinedConversation(view) {
			continue
		}
		if query != "" && !contactMatches(summary, query) {
			continue
		}
		summaries = append(summaries, summary)
	}
	sort.SliceStable(summaries, func(i, j int) bool {
		left, right := strings.ToLower(summaries[i].DisplayName), strings.ToLower(summaries[j].DisplayName)
		if left == right {
			return summaries[i].PeerMXID < summaries[j].PeerMXID
		}
		return left < right
	})
	limit := dirextalkmcp.Limit(params)
	if len(summaries) > limit {
		summaries = summaries[:limit]
	}
	return map[string]any{"contacts": summaries}, nil
}

func contactSummary(contact dirextalkdomain.ContactRecord) dirextalkmcp.ContactSummary {
	displayName := fallback(contact.Remark, contact.DisplayName)
	if displayName == "" {
		displayName = contact.PeerMXID
	}
	return dirextalkmcp.ContactSummary{
		PeerMXID:    contact.PeerMXID,
		DisplayName: displayName,
		AvatarURL:   contact.AvatarURL,
		Domain:      contact.Domain,
		RoomID:      contact.RoomID,
		Status:      contact.Status,
		Remark:      contact.Remark,
	}
}

func contactMatches(contact dirextalkmcp.ContactSummary, query string) bool {
	query = strings.ToLower(strings.TrimSpace(query))
	if query == "" {
		return true
	}
	values := []string{
		contact.PeerMXID,
		contact.DisplayName,
		contact.AvatarURL,
		contact.Domain,
		contact.RoomID,
		contact.Remark,
	}
	for _, value := range values {
		if strings.Contains(strings.ToLower(strings.TrimSpace(value)), query) {
			return true
		}
	}
	return false
}

func (m *Module) enrichRoomSummaryWithMatrixMemberCount(ctx context.Context, summary dirextalkmcp.RoomSummary) dirextalkmcp.RoomSummary {
	if summary.RoomID == "" || (summary.Type != "group" && summary.Type != "channel") {
		return summary
	}
	members, err := m.matrixRoomMembers(ctx, summary.RoomID)
	if err != nil || len(members) == 0 {
		return summary
	}
	var count int64
	for _, member := range members {
		if dirextalkdomain.MemberHidden(member.Membership) {
			continue
		}
		count++
	}
	if count > 0 {
		summary.Subtitle = formatMemberCount(count)
	}
	return summary
}

func roomSummaryFromConversation(view dirextalkdomain.ConversationView) dirextalkmcp.RoomSummary {
	roomType := string(view.Kind)
	if roomType == "direct" {
		roomType = "contact"
	}
	subtitle := view.PeerMXID
	if roomType == "group" || roomType == "channel" {
		subtitle = formatMemberCount(view.MemberCount)
	}
	return dirextalkmcp.RoomSummary{
		Type:           roomType,
		Name:           fallback(view.Title, view.MatrixRoomID),
		RoomID:         view.MatrixRoomID,
		Subtitle:       subtitle,
		LastMsg:        view.LastMessage,
		LastMessageAt:  dirextalkmcp.FormatTime(view.LastActivityAt),
		LastActivityTS: view.LastActivityAt,
	}
}

func roomMatches(room dirextalkmcp.RoomSummary, query string) bool {
	return strings.Contains(strings.ToLower(room.Name), query) ||
		strings.Contains(strings.ToLower(room.Subtitle), query) ||
		strings.Contains(strings.ToLower(room.RoomID), query)
}

func formatMemberCount(count int64) string {
	if count <= 0 {
		return ""
	}
	return strconv.FormatInt(count, 10) + " members"
}
