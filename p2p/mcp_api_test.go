package p2p

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	matrixhistory "github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmatrix"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
)

func mustInvokeMCP[T any](t *testing.T, service *Service, action string, params map[string]any) T {
	t.Helper()
	result, apiErr := service.invokeDirextalkMCP(context.Background(), action, params)
	if apiErr != nil {
		t.Fatalf("MCP action %s failed: %d %s", action, apiErr.Status, apiErr.Error)
	}
	typed, ok := result.(T)
	if !ok {
		t.Fatalf("MCP action %s returned %T, want requested type", action, result)
	}
	return typed
}

func mustSaveJoinedMCPRoom(t *testing.T, service *Service, roomID string) {
	t.Helper()
	if err := service.saveConversation(context.Background(), conversationRecord{
		MatrixRoomID: roomID,
		Kind:         conversationKindGroup,
		Lifecycle:    conversationLifecycleActive,
		Title:        roomID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID: roomID, UserID: service.OwnerMXID(), Membership: "join", Role: "owner",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestMCPSearchRoomsReturnsConciseMixedRooms(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})

	mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         "@alice:example.com",
		"display_name": "Alice",
	})
	mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Design Group",
	})
	mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":   "product",
		"room_id":      "!channel:example.com",
		"name":         "Product Channel",
		"channel_type": "post",
	})

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomsSearch, map[string]any{
		"query": "product",
		"type":  "all",
		"limit": float64(10),
	})
	rooms := result["rooms"].([]mcpRoomSummary)
	if len(rooms) != 1 {
		t.Fatalf("expected one product room, got %#v", rooms)
	}
	if rooms[0].Type != "channel" || rooms[0].Name != "Product Channel" || rooms[0].RoomID != "!channel:example.com" {
		t.Fatalf("unexpected room summary: %#v", rooms[0])
	}
}

func TestMCPSearchRoomsEmptyQueryListsByType(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Design Group",
	})

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomsSearch, map[string]any{
		"type": "group",
	})
	rooms := result["rooms"].([]mcpRoomSummary)
	if len(rooms) != 1 || rooms[0].Type != "group" || rooms[0].RoomID != "!group:example.com" {
		t.Fatalf("expected group list from empty query, got %#v", rooms)
	}
}

func TestMCPSearchRoomsOnlyReturnsJoinedRooms(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	for _, room := range []struct {
		roomID     string
		name       string
		membership string
	}{
		{roomID: "!joined:example.com", name: "Joined Group", membership: "join"},
		{roomID: "!joining:example.com", name: "Joining Group", membership: "joining"},
		{roomID: "!left:example.com", name: "Left Group", membership: "left"},
	} {
		mustHandle[groupRecord](t, service, "groups.create", map[string]any{
			"room_id": room.roomID,
			"name":    room.name,
		})
		if err := service.saveMember(context.Background(), memberRecord{
			RoomID: room.roomID, UserID: service.OwnerMXID(), Membership: room.membership, Role: "owner",
		}); err != nil {
			t.Fatal(err)
		}
	}

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomsSearch, map[string]any{
		"type": "group",
	})
	rooms := result["rooms"].([]mcpRoomSummary)
	if len(rooms) != 1 || rooms[0].RoomID != "!joined:example.com" {
		t.Fatalf("expected only joined rooms in MCP discovery, got %#v", rooms)
	}
}

func TestMCPSearchRoomsUsesMatrixMemberCountWhenProductCountIsStale(t *testing.T) {
	transport := &recordingTransport{roomMembers: []memberRecord{
		{RoomID: "!group:example.com", UserID: "@owner:example.com", DisplayName: "Owner Name", Membership: "join", Role: "owner"},
		{RoomID: "!group:example.com", UserID: "@owner:t7.dirextalk.ai", DisplayName: "owner", Membership: "join", Role: "member"},
		{RoomID: "!group:example.com", UserID: "@owner:t8.dirextalk.ai", DisplayName: "owner", Membership: "join", Role: "member"},
	}}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Design Group",
	})

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomsSearch, map[string]any{
		"type":  "group",
		"query": "Design",
	})
	rooms := result["rooms"].([]mcpRoomSummary)
	if len(rooms) != 1 || rooms[0].Subtitle != "3 members" {
		t.Fatalf("expected Matrix-backed member count, got %#v", rooms)
	}
}

func TestMCPBlockedRoomsAreFilteredFromRoomSearch(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!visible:example.com",
		"name":    "Visible Group",
	})
	mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!blocked:example.com",
		"name":    "Blocked Group",
	})
	mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"mcp_blocked_room_ids": []any{"!blocked:example.com"},
	})

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomsSearch, map[string]any{
		"type": "group",
	})
	rooms := result["rooms"].([]mcpRoomSummary)
	if len(rooms) != 1 || rooms[0].RoomID != "!visible:example.com" {
		t.Fatalf("expected blocked room to be filtered from MCP search, got %#v", rooms)
	}
}

func TestMCPContactsListReturnsAcceptedContacts(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    "@alice:example.com",
		DisplayName: "Alice",
		AvatarURL:   "mxc://example.com/alice",
		Domain:      "example.com",
		RoomID:      "!alice:example.com",
		Status:      "accepted",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    "@bob:example.com",
		DisplayName: "Bob",
		Domain:      "example.com",
		RoomID:      "!bob:example.com",
		Status:      "deleted",
	}); err != nil {
		t.Fatal(err)
	}

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionContactsList, map[string]any{})
	contacts := result["contacts"].([]mcpContactSummary)
	if len(contacts) != 1 {
		t.Fatalf("expected only visible contacts, got %#v", contacts)
	}
	if contacts[0].PeerMXID != "@alice:example.com" || contacts[0].DisplayName != "Alice" || contacts[0].AvatarURL != "mxc://example.com/alice" {
		t.Fatalf("unexpected contact summary: %#v", contacts[0])
	}
}

func TestMCPContactsSearchMatchesNamePeerAndDomain(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    "@alice:remote.example",
		DisplayName: "Alice Product",
		Domain:      "remote.example",
		RoomID:      "!alice:remote.example",
		Status:      "accepted",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    "@bob:example.com",
		DisplayName: "Bob",
		Domain:      "example.com",
		RoomID:      "!bob:example.com",
		Status:      "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionContactsSearch, map[string]any{
		"query": "remote",
		"limit": float64(10),
	})
	contacts := result["contacts"].([]mcpContactSummary)
	if len(contacts) != 1 || contacts[0].PeerMXID != "@alice:remote.example" {
		t.Fatalf("expected remote Alice contact, got %#v", contacts)
	}

	result = mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionContactsSearch, map[string]any{
		"query": "product",
	})
	contacts = result["contacts"].([]mcpContactSummary)
	if len(contacts) != 1 || contacts[0].DisplayName != "Alice Product" {
		t.Fatalf("expected display-name match, got %#v", contacts)
	}
}

func TestMCPMessagesSendUsesTransportAndReturnsConciseResult(t *testing.T) {
	transport := &recordingTransport{eventID: "$mcp:event", ts: 1710000000000}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.profile.DisplayName = "Owner"
	mustSaveJoinedMCPRoom(t, service, "!room:example.com")

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionMessagesSend, map[string]any{
		"room_id": "!room:example.com",
		"msg":     "hello",
	})
	if result["ok"] != true || result["room_id"] != "!room:example.com" || result["event_id"] != "$mcp:event" {
		t.Fatalf("unexpected send result: %#v", result)
	}
	if len(transport.messages) != 1 {
		t.Fatalf("expected one Matrix message, got %#v", transport.messages)
	}
	if transport.messages[0].SenderMXID != "@owner:example.com" {
		t.Fatalf("expected ordinary MCP send to proxy owner, got %#v", transport.messages[0])
	}
	content := transport.messages[0].Content
	if content["body"] != "hello" || content["msgtype"] != "m.text" {
		t.Fatalf("expected text Matrix message content, got %#v", content)
	}
	if _, ok := content["p2p_kind"]; ok {
		t.Fatalf("ordinary MCP message must not create channel content: %#v", content)
	}
}

func TestMCPMessagesSendMarksGatewayReplies(t *testing.T) {
	transport := &recordingTransport{
		eventID: "$mcp:event", ts: 1710000000000,
		roomMembers: []memberRecord{{RoomID: "!agents:example.com", UserID: "@agent:example.com", Membership: "join"}},
	}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.agentRoomID = "!agents:example.com"

	mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionMessagesSend, map[string]any{
		"room_id":        "!agents:example.com",
		"msg":            "agent reply",
		"agent_gateway":  true,
		"gateway_source": "codex-cli",
	})
	if len(transport.messages) != 1 {
		t.Fatalf("expected one Matrix message, got %#v", transport.messages)
	}
	if transport.messages[0].SenderMXID != "@agent:example.com" {
		t.Fatalf("expected gateway reply to be sent by agent, got %#v", transport.messages[0])
	}
	content := transport.messages[0].Content
	if content[AgentGatewayContentKey] != true || content[AgentGatewaySourceContentKey] != "codex-cli" {
		t.Fatalf("expected gateway marker content, got %#v", content)
	}
}

func TestMCPMessagesSendRejectsOwnerSendToAgentRoom(t *testing.T) {
	transport := &recordingTransport{eventID: "$mcp:event", ts: 1710000000000}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.agentRoomID = "!agents:example.com"

	_, apiErr := service.invokeDirextalkMCP(context.Background(), dirextalkmcp.ActionMessagesSend, map[string]any{
		"room_id": "!agents:example.com",
		"msg":     "hello agent room",
	})
	if apiErr == nil || apiErr.Status != 400 || !strings.Contains(apiErr.Error, "agent room") {
		t.Fatalf("expected agent room send to fail with bad request, got %#v", apiErr)
	}
	if len(transport.messages) != 0 {
		t.Fatalf("owner MCP send to agent room must not write Matrix messages, got %#v", transport.messages)
	}
}

func TestMCPMessagesRejectBlockedRooms(t *testing.T) {
	transport := &recordingTransport{eventID: "$mcp:event", ts: 1710000000000}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.SetMatrixMessageReader(&fakeMCPMessageReader{messages: []mcpMessageSummary{
		{EventID: "$secret", OriginServerTS: 1710000000000, CreatedAt: "2024-03-09T16:00:00Z", Sender: "Alice", Msg: "secret", SenderMXID: "@alice:example.com"},
	}})
	mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"mcp_blocked_room_ids": []any{"!blocked:example.com"},
	})

	_, apiErr := service.invokeDirextalkMCP(context.Background(), dirextalkmcp.ActionMessagesList, map[string]any{
		"room_id": "!blocked:example.com",
	})
	if apiErr == nil || apiErr.Status != 403 || !strings.Contains(apiErr.Error, "blocked") {
		t.Fatalf("expected blocked room list to fail, got %#v", apiErr)
	}
	_, apiErr = service.invokeDirextalkMCP(context.Background(), dirextalkmcp.ActionMessagesSend, map[string]any{
		"room_id": "!blocked:example.com",
		"msg":     "secret",
	})
	if apiErr == nil || apiErr.Status != 403 || !strings.Contains(apiErr.Error, "blocked") {
		t.Fatalf("expected blocked room send to fail, got %#v", apiErr)
	}
	if len(transport.messages) != 0 {
		t.Fatalf("blocked MCP send must not write Matrix messages, got %#v", transport.messages)
	}
}

type fakeMCPMessageReader struct {
	messages []mcpMessageSummary
	err      error
	calls    int
}

func (r *fakeMCPMessageReader) ListOrdinaryMessages(ctx context.Context, roomID string, page mcpMessagePage) (mcpMessagePageResult, error) {
	r.calls++
	if r.err != nil {
		return mcpMessagePageResult{}, r.err
	}
	out := make([]mcpMessageSummary, 0, len(r.messages))
	for _, msg := range r.messages {
		if mcpPageIncludes(msg.OriginServerTS, msg.EventID, page) {
			out = append(out, msg)
		}
	}
	matrixhistory.SortMessageSummaries(out)
	hasMore := false
	if len(out) > page.Limit {
		hasMore = true
		out = out[:page.Limit]
	}
	return mcpMessagePageResult{Messages: out, HasMore: hasMore}, nil
}

type fakeMatrixProfileResolver struct {
	profiles map[string]matrixUserProfile
}

func (r *fakeMatrixProfileResolver) ResolveMatrixProfile(ctx context.Context, userID string) (matrixUserProfile, error) {
	return r.profiles[strings.TrimSpace(userID)], nil
}

func TestMCPMessagesListUsesReaderAndReturnsConciseMessages(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	service.SetMatrixMessageReader(&fakeMCPMessageReader{messages: []mcpMessageSummary{
		{EventID: "$old", OriginServerTS: 1710000000000, CreatedAt: "2024-03-09T16:00:00Z", Sender: "Alice", Msg: "old", SenderMXID: "@alice:remote.example"},
		{EventID: "$inside", OriginServerTS: 1710000100000, CreatedAt: "2024-03-09T16:01:40Z", Sender: "Alice", Msg: "inside", SenderMXID: "@alice:remote.example", SenderDomain: "remote.example", SenderLocalpart: "alice"},
	}})
	if err := service.saveConversation(context.Background(), conversationRecord{
		MatrixRoomID: "!room:example.com",
		Kind:         conversationKindDirect,
		Lifecycle:    conversationLifecycleActive,
		Title:        "Alice",
	}); err != nil {
		t.Fatal(err)
	}

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionMessagesList, map[string]any{
		"room_id":   "!room:example.com",
		"from_time": "2024-03-09T16:00:50Z",
		"limit":     float64(20),
	})
	if result["room_id"] != "!room:example.com" || result["name"] != "Alice" {
		t.Fatalf("unexpected message list envelope: %#v", result)
	}
	messages := result["messages"].([]mcpMessageSummary)
	if len(messages) != 1 || messages[0].Msg != "inside" {
		t.Fatalf("unexpected message summaries: %#v", messages)
	}
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []struct {
			Sender          string `json:"sender"`
			SenderMXID      string `json:"sender_mxid"`
			SenderDomain    string `json:"sender_domain"`
			SenderLocalpart string `json:"sender_localpart"`
			CreatedAt       string `json:"created_at"`
			TS              int64  `json:"ts,omitempty"`
			Msg             string `json:"msg"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 1 ||
		decoded.Messages[0].Sender != "Alice" ||
		decoded.Messages[0].SenderMXID != "@alice:remote.example" ||
		decoded.Messages[0].SenderDomain != "remote.example" ||
		decoded.Messages[0].SenderLocalpart != "alice" ||
		decoded.Messages[0].CreatedAt != "2024-03-09T16:01:40Z" ||
		decoded.Messages[0].TS != 0 {
		t.Fatalf("expected message JSON to expose sender identity, got %s", string(payload))
	}
}

func TestMCPMessagesPaginationUsesStableSnapshot(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	mustSaveJoinedMCPRoom(t, service, "!channel:example.com")
	reader := &fakeMCPMessageReader{}
	service.SetMatrixMessageReader(reader)
	base := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		id := "msg_" + string(rune('a'+i))
		reader.messages = append(reader.messages, mcpMessageSummary{
			EventID:        "$" + id,
			OriginServerTS: base.Add(time.Duration(i) * time.Minute).UnixMilli(),
			CreatedAt:      base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			Sender:         "Owner",
			SenderMXID:     "@owner:example.com",
			Msg:            id,
		})
	}

	first := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionMessagesList, map[string]any{
		"room_id": "!channel:example.com",
		"to_time": "2026-07-05T10:12:00Z",
		"limit":   float64(5),
	})
	firstMessages := first["messages"].([]mcpMessageSummary)
	if len(firstMessages) != 5 || firstMessages[0].Msg != "msg_l" || firstMessages[4].Msg != "msg_h" {
		t.Fatalf("expected newest five ordinary messages, got %#v", firstMessages)
	}
	cursor, _ := first["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("expected next_cursor in first page: %#v", first)
	}

	for i := 0; i < 10; i++ {
		id := "new_msg_" + string(rune('a'+i))
		reader.messages = append(reader.messages, mcpMessageSummary{
			EventID:        "$" + id,
			OriginServerTS: base.Add(time.Hour + time.Duration(i)*time.Minute).UnixMilli(),
			CreatedAt:      base.Add(time.Hour + time.Duration(i)*time.Minute).Format(time.RFC3339),
			Sender:         "Owner",
			SenderMXID:     "@owner:example.com",
			Msg:            id,
		})
	}

	second := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionMessagesList, map[string]any{
		"room_id": "!channel:example.com",
		"cursor":  cursor,
		"limit":   float64(5),
	})
	secondMessages := second["messages"].([]mcpMessageSummary)
	if len(secondMessages) != 5 || secondMessages[0].Msg != "msg_g" || secondMessages[4].Msg != "msg_c" {
		t.Fatalf("expected cursor to continue original message snapshot, got %#v", secondMessages)
	}
	for _, msg := range secondMessages {
		if strings.HasPrefix(msg.Msg, "new_msg_") {
			t.Fatalf("cursor page must not include newer inserts: %#v", secondMessages)
		}
	}
}

func TestMCPRejectsInvalidTimeParams(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	service.SetMatrixMessageReader(&fakeMCPMessageReader{})

	for _, tc := range []struct {
		name    string
		action  string
		params  map[string]any
		message string
	}{
		{"message legacy timestamp", dirextalkmcp.ActionMessagesList, map[string]any{"room_id": "!room:example.com", "from_ts": float64(1710000000000)}, "from_time"},
		{"post legacy timestamp", dirextalkmcp.ActionChannelPostsList, map[string]any{"room_id": "!channel:example.com", "to_ts": float64(1710000000000)}, "from_time"},
		{"comment legacy timestamp", dirextalkmcp.ActionChannelCommentsList, map[string]any{"post_id": "post_1", "from_ts": float64(1710000000000)}, "from_time"},
		{"non-UTC timestamp", dirextalkmcp.ActionMessagesList, map[string]any{"room_id": "!room:example.com", "from_time": "2024-03-10T00:00:00+08:00"}, "UTC"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, apiErr := service.invokeDirextalkMCP(context.Background(), tc.action, tc.params)
			if apiErr == nil || apiErr.Status != http.StatusBadRequest || !strings.Contains(apiErr.Error, tc.message) {
				t.Fatalf("expected invalid time to mention %q, got %#v", tc.message, apiErr)
			}
		})
	}
}

func TestMCPMessagesListPropagatesMatrixAccessErrors(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	mustSaveJoinedMCPRoom(t, service, "!room:example.com")
	service.SetMatrixMessageReader(&fakeMCPMessageReader{
		err: matrixhistory.StatusError{StatusCode: http.StatusForbidden, Message: "matrix messages failed with status 403"},
	})

	_, apiErr := service.invokeDirextalkMCP(context.Background(), dirextalkmcp.ActionMessagesList, map[string]any{
		"room_id": "!room:example.com",
	})
	if apiErr == nil || apiErr.Status != http.StatusForbidden || !strings.Contains(apiErr.Error, "not allowed") {
		t.Fatalf("expected Matrix access failure to be exposed as 403, got %#v", apiErr)
	}
}

func TestMCPMessagesListRejectsRoomsThatAreNotJoined(t *testing.T) {
	for _, membership := range []string{"joining", "left", "unknown"} {
		t.Run(membership, func(t *testing.T) {
			service := NewService(Config{ServerName: "example.com"})
			reader := &fakeMCPMessageReader{messages: []mcpMessageSummary{{
				EventID: "$secret", OriginServerTS: 1710000000000, Msg: "secret",
			}}}
			service.SetMatrixMessageReader(reader)
			if membership != "unknown" {
				mustHandle[groupRecord](t, service, "groups.create", map[string]any{
					"room_id": "!restricted:example.com",
					"name":    "Restricted Group",
				})
				if err := service.saveMember(context.Background(), memberRecord{
					RoomID: "!restricted:example.com", UserID: service.OwnerMXID(), Membership: membership, Role: "owner",
				}); err != nil {
					t.Fatal(err)
				}
			}

			_, apiErr := service.invokeDirextalkMCP(context.Background(), dirextalkmcp.ActionMessagesList, map[string]any{
				"room_id": "!restricted:example.com",
			})
			if apiErr == nil || apiErr.Status != http.StatusForbidden || !strings.Contains(apiErr.Error, "not joined") {
				t.Fatalf("expected %s room message access to fail, got %#v", membership, apiErr)
			}
			if reader.calls != 0 {
				t.Fatalf("non-joined room must be rejected before Matrix history access, calls=%d", reader.calls)
			}
			_, apiErr = service.invokeDirextalkMCP(context.Background(), dirextalkmcp.ActionMessagesSend, map[string]any{
				"room_id": "!restricted:example.com",
				"msg":     "must not send",
			})
			if apiErr == nil || apiErr.Status != http.StatusForbidden || !strings.Contains(apiErr.Error, "not joined") {
				t.Fatalf("expected %s room message send to fail, got %#v", membership, apiErr)
			}
		})
	}
}

func TestMCPMessagesListEnrichesSenderDisplayNamesFromRoomMembers(t *testing.T) {
	transport := &recordingTransport{roomMembers: []memberRecord{
		{RoomID: "!group:example.com", UserID: "@owner:example.com", DisplayName: "Owner Name", Membership: "join", Role: "owner"},
		{RoomID: "!group:example.com", UserID: "@alice:remote.example", DisplayName: "Alice Remote", Membership: "join", Role: "member"},
	}}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.SetMatrixMessageReader(&fakeMCPMessageReader{messages: []mcpMessageSummary{
		{
			EventID:         "$alice",
			OriginServerTS:  1710000100000,
			CreatedAt:       "2024-03-09T16:01:40Z",
			Sender:          "alice",
			SenderMXID:      "@alice:remote.example",
			SenderDomain:    "remote.example",
			SenderLocalpart: "alice",
			Msg:             "hello from alice",
		},
	}})
	mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Design Group",
	})

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionMessagesList, map[string]any{
		"room_id": "!group:example.com",
	})
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Messages []struct {
			Sender            string `json:"sender"`
			SenderMXID        string `json:"sender_mxid"`
			SenderDisplayName string `json:"sender_display_name"`
			Msg               string `json:"msg"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Messages) != 1 ||
		decoded.Messages[0].Sender != "Alice Remote" ||
		decoded.Messages[0].SenderDisplayName != "Alice Remote" ||
		decoded.Messages[0].SenderMXID != "@alice:remote.example" {
		t.Fatalf("expected message JSON to expose readable sender display name, got %s", string(payload))
	}
}

func TestMCPMessagesListEnrichesFallbackSenderNameFromMatrixProfile(t *testing.T) {
	transport := &recordingTransport{roomMembers: []memberRecord{
		{RoomID: "!group:example.com", UserID: "@owner:example.com", DisplayName: "Owner Name", Membership: "join", Role: "owner"},
		{RoomID: "!group:example.com", UserID: "@owner:t8.dirextalk.ai", DisplayName: "owner", Membership: "join", Role: "member"},
	}}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.SetMatrixProfileResolver(&fakeMatrixProfileResolver{profiles: map[string]matrixUserProfile{
		"@owner:t8.dirextalk.ai": {DisplayName: "liyanan8", AvatarURL: "mxc://t8/avatar"},
	}})
	service.SetMatrixMessageReader(&fakeMCPMessageReader{messages: []mcpMessageSummary{
		{
			EventID:         "$t8",
			OriginServerTS:  1710000100000,
			CreatedAt:       "2024-03-09T16:01:40Z",
			Sender:          "owner",
			SenderMXID:      "@owner:t8.dirextalk.ai",
			SenderDomain:    "t8.dirextalk.ai",
			SenderLocalpart: "owner",
			Msg:             "hello from t8",
		},
	}})
	mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Design Group",
	})

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionMessagesList, map[string]any{
		"room_id": "!group:example.com",
	})
	messages := result["messages"].([]mcpMessageSummary)
	if len(messages) != 1 ||
		messages[0].Sender != "liyanan8" ||
		messages[0].SenderDisplayName != "liyanan8" {
		t.Fatalf("expected profile display name for fallback sender, got %#v", messages)
	}
}

func TestMCPRoomMembersListReturnsMemberIdentities(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Design Group",
	})
	mustHandle[map[string]any](t, service, "groups.join", map[string]any{
		"room_id":      group.RoomID,
		"user_mxid":    "@alice:remote.example",
		"display_name": "Alice Remote",
	})
	for _, member := range []memberRecord{
		{RoomID: group.RoomID, UserID: "@joining:remote.example", Membership: "joining", Role: "member"},
		{RoomID: group.RoomID, UserID: "@pending:remote.example", Membership: "pending", Role: "member"},
	} {
		if err := service.saveMember(context.Background(), member); err != nil {
			t.Fatal(err)
		}
	}

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomMembersList, map[string]any{
		"room_id": group.RoomID,
	})
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		RoomID  string `json:"room_id"`
		Name    string `json:"name"`
		Members []struct {
			UserID      string `json:"user_id"`
			UserMXID    string `json:"user_mxid"`
			Localpart   string `json:"localpart"`
			Domain      string `json:"domain"`
			DisplayName string `json:"display_name"`
			Role        string `json:"role"`
			Membership  string `json:"membership"`
		} `json:"members"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RoomID != group.RoomID || decoded.Name != "Design Group" {
		t.Fatalf("unexpected room member envelope: %s", string(payload))
	}
	if len(decoded.Members) != 2 {
		t.Fatalf("expected owner plus alice, got %s", string(payload))
	}
	var alice = decoded.Members[0]
	for _, member := range decoded.Members {
		if member.UserMXID == "@alice:remote.example" {
			alice = member
			break
		}
	}
	if alice.UserID != "@alice:remote.example" ||
		alice.UserMXID != "@alice:remote.example" ||
		alice.Localpart != "alice" ||
		alice.Domain != "remote.example" ||
		alice.DisplayName != "Alice Remote" ||
		alice.Membership != "join" {
		t.Fatalf("expected member JSON to expose Matrix identity, got %s", string(payload))
	}
}

func TestMCPRoomScopedToolsRejectNonJoinedRooms(t *testing.T) {
	for _, membership := range []string{"joining", "left"} {
		t.Run(membership, func(t *testing.T) {
			transport := &recordingTransport{eventID: "$event", ts: 1710000000000}
			service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
			reader := &fakeMCPMessageReader{}
			service.SetMatrixMessageReader(reader)
			ch := mustHandle[channel](t, service, "channels.create", map[string]any{
				"channel_id":       "restricted",
				"room_id":          "!restricted-channel:example.com",
				"name":             "Restricted Channel",
				"comments_enabled": true,
			})
			post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
				"channel_id": ch.ChannelID,
				"room_id":    ch.RoomID,
				"body":       "existing post",
			})
			if err := service.saveMember(context.Background(), memberRecord{
				RoomID: ch.RoomID, ChannelID: ch.ChannelID, UserID: service.OwnerMXID(), Membership: membership, Role: "owner",
			}); err != nil {
				t.Fatal(err)
			}
			writesBefore := len(transport.messages)
			for _, tc := range []struct {
				action string
				params map[string]any
			}{
				{dirextalkmcp.ActionMessagesList, map[string]any{"room_id": ch.RoomID}},
				{dirextalkmcp.ActionMessagesSend, map[string]any{"room_id": ch.RoomID, "msg": "blocked"}},
				{dirextalkmcp.ActionRoomMembersList, map[string]any{"room_id": ch.RoomID}},
				{dirextalkmcp.ActionChannelPostsList, map[string]any{"room_id": ch.RoomID}},
				{dirextalkmcp.ActionChannelCommentsList, map[string]any{"post_id": post.PostID}},
				{dirextalkmcp.ActionChannelCommentsCreate, map[string]any{"post_id": post.PostID, "msg": "blocked"}},
			} {
				_, apiErr := service.invokeDirextalkMCP(context.Background(), tc.action, tc.params)
				if apiErr == nil || apiErr.Status != http.StatusForbidden || !strings.Contains(apiErr.Error, "not joined") {
					t.Fatalf("expected %s to reject %s room, got %#v", tc.action, membership, apiErr)
				}
			}
			if reader.calls != 0 {
				t.Fatalf("non-joined room must not reach Matrix history, calls=%d", reader.calls)
			}
			if len(transport.messages) != writesBefore {
				t.Fatalf("non-joined room must not accept MCP writes, before=%d after=%d", writesBefore, len(transport.messages))
			}
		})
	}
}

func TestMCPRoomMembersListMergesMatrixRoomStateMembers(t *testing.T) {
	transport := &recordingTransport{roomMembers: []memberRecord{
		{RoomID: "!group:example.com", UserID: "@owner:example.com", DisplayName: "Owner Name", Membership: "join", Role: "owner"},
		{RoomID: "!group:example.com", UserID: "@alice:remote.example", DisplayName: "Alice Remote", Membership: "join", Role: "member"},
		{RoomID: "!group:example.com", UserID: "@bob:remote.example", DisplayName: "Bob Remote", Membership: "left", Role: "member"},
	}}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Design Group",
	})
	for _, member := range []memberRecord{
		{RoomID: group.RoomID, UserID: "@alice:remote.example", Membership: "joining", Role: "member"},
		{RoomID: group.RoomID, UserID: "@bob:remote.example", Membership: "join", Role: "member"},
	} {
		if err := service.saveMember(context.Background(), member); err != nil {
			t.Fatal(err)
		}
	}

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomMembersList, map[string]any{
		"room_id": group.RoomID,
	})
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Members []struct {
			UserMXID    string `json:"user_mxid"`
			DisplayName string `json:"display_name"`
			Membership  string `json:"membership"`
		} `json:"members"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Members) != 2 {
		t.Fatalf("expected only current Matrix join members, got %s", string(payload))
	}
	if decoded.Members[1].UserMXID != "@alice:remote.example" ||
		decoded.Members[1].DisplayName != "Alice Remote" ||
		decoded.Members[1].Membership != "join" {
		t.Fatalf("expected Matrix room state member identity, got %s", string(payload))
	}
}

func TestMCPRoomMembersListGloballySortsMixedGroupAndChannelSources(t *testing.T) {
	for _, tc := range []struct {
		name      string
		roomID    string
		channelID string
		prepare   func(context.Context, *Service) error
	}{
		{
			name:   "group",
			roomID: "!mixed-group:example.com",
			prepare: func(ctx context.Context, service *Service) error {
				return service.saveGroup(ctx, groupRecord{RoomID: "!mixed-group:example.com", Name: "Mixed Group"})
			},
		},
		{
			name:      "channel",
			roomID:    "!mixed-channel:example.com",
			channelID: "mixed_channel",
			prepare: func(ctx context.Context, service *Service) error {
				return service.saveChannel(ctx, channel{ChannelID: "mixed_channel", RoomID: "!mixed-channel:example.com", Name: "Mixed Channel"})
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			matrixTransport := &recordingTransport{roomMembers: []memberRecord{
				{RoomID: tc.roomID, UserID: "@a-matrix:matrix.example", Membership: "join", Role: "member", JoinedAt: 500},
				{RoomID: tc.roomID, UserID: "@fake-owner:matrix.example", Membership: "join", Role: "owner", JoinedAt: 1500},
				{RoomID: tc.roomID, UserID: "@a-tie:matrix.example", Membership: "join", Role: "member", JoinedAt: 2000},
				{RoomID: tc.roomID, UserID: "@missing-time:matrix.example", Membership: "join", Role: "member"},
			}}
			transport := &roomCreatorReadingTransport{
				recordingTransport: matrixTransport,
				creators:           map[string]string{tc.roomID: "@actual-creator:creator.example"},
			}
			service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
			setServiceOwnerForTest(service, "@actual-creator:creator.example", "Actual Creator")
			if err := tc.prepare(ctx, service); err != nil {
				t.Fatal(err)
			}
			if err := service.conversationModule.SetCreator(ctx, tc.roomID, "@actual-creator:creator.example"); err != nil {
				t.Fatal(err)
			}
			for _, member := range []memberRecord{
				{RoomID: tc.roomID, ChannelID: tc.channelID, UserID: "@actual-creator:creator.example", Membership: "join", Role: "member", JoinedAt: 5000},
				{RoomID: tc.roomID, ChannelID: tc.channelID, UserID: "@z-projected:projection.example", Membership: "join", Role: "member", JoinedAt: 1000},
				{RoomID: tc.roomID, ChannelID: tc.channelID, UserID: "@b-tie:projection.example", Membership: "join", Role: "member", JoinedAt: 2000},
			} {
				if err := service.store.UpsertMember(ctx, member); err != nil {
					t.Fatal(err)
				}
			}

			result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomMembersList, map[string]any{"room_id": tc.roomID})
			members := result["members"].([]mcpMemberSummary)
			got := make([]string, len(members))
			for index := range members {
				got[index] = members[index].UserMXID
			}
			want := []string{
				"@actual-creator:creator.example",
				"@a-matrix:matrix.example",
				"@z-projected:projection.example",
				"@fake-owner:matrix.example",
				"@a-tie:matrix.example",
				"@b-tie:projection.example",
				"@missing-time:matrix.example",
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("global mixed-source order = %#v, want %#v", got, want)
			}
			if members[0].Role != "owner" || members[3].Role != "member" {
				t.Fatalf("exact creator roles were not canonicalized after merge: %#v", members)
			}
		})
	}
}

func TestMCPRoomMembersListRejectsUnknownRoomsBeforeMatrixLookup(t *testing.T) {
	transport := &recordingTransport{roomMembers: []memberRecord{
		{RoomID: "!secret:example.com", UserID: "@alice:remote.example", DisplayName: "Alice Remote", Membership: "join", Role: "member"},
	}}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)

	_, apiErr := service.invokeDirextalkMCP(context.Background(), dirextalkmcp.ActionRoomMembersList, map[string]any{
		"room_id": "!secret:example.com",
	})
	if apiErr == nil || apiErr.Status != 404 || !strings.Contains(apiErr.Error, "room not found") {
		t.Fatalf("expected unknown room to be rejected, got %#v", apiErr)
	}
}

func TestMCPRoomMembersListFiltersMergedMatrixMembers(t *testing.T) {
	transport := &recordingTransport{roomMembers: []memberRecord{
		{RoomID: "!group:example.com", UserID: "@owner:example.com", DisplayName: "Owner Name", Membership: "join", Role: "owner"},
		{RoomID: "!group:example.com", UserID: "@alice:remote.example", DisplayName: "Alice Remote", Membership: "join", Role: "member"},
	}}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Design Group",
	})

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomMembersList, map[string]any{
		"room_id": group.RoomID,
		"role":    "member",
	})
	members := result["members"].([]mcpMemberSummary)
	if len(members) != 1 || members[0].UserMXID != "@alice:remote.example" || members[0].Role != "member" {
		t.Fatalf("expected role filter to apply after Matrix member merge, got %#v", members)
	}
}

func TestMCPRoomMembersListEnrichesFallbackNamesFromMatrixProfiles(t *testing.T) {
	transport := &recordingTransport{roomMembers: []memberRecord{
		{RoomID: "!group:example.com", UserID: "@owner:example.com", DisplayName: "Owner Name", Membership: "join", Role: "owner"},
		{RoomID: "!group:example.com", UserID: "@owner:t7.dirextalk.ai", DisplayName: "owner", Membership: "join", Role: "member"},
		{RoomID: "!group:example.com", UserID: "@owner:t8.dirextalk.ai", DisplayName: "owner", Membership: "join", Role: "member"},
	}}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.SetMatrixProfileResolver(&fakeMatrixProfileResolver{profiles: map[string]matrixUserProfile{
		"@owner:t7.dirextalk.ai": {DisplayName: "liyanan7", AvatarURL: "mxc://t7/avatar"},
		"@owner:t8.dirextalk.ai": {DisplayName: "liyanan8", AvatarURL: "mxc://t8/avatar"},
	}})
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Design Group",
	})

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomMembersList, map[string]any{
		"room_id": group.RoomID,
	})
	payload, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Members []struct {
			UserMXID    string `json:"user_mxid"`
			DisplayName string `json:"display_name"`
			AvatarURL   string `json:"avatar_url"`
		} `json:"members"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	avatars := map[string]string{}
	for _, member := range decoded.Members {
		names[member.UserMXID] = member.DisplayName
		avatars[member.UserMXID] = member.AvatarURL
	}
	if names["@owner:t7.dirextalk.ai"] != "liyanan7" ||
		names["@owner:t8.dirextalk.ai"] != "liyanan8" ||
		avatars["@owner:t7.dirextalk.ai"] != "mxc://t7/avatar" {
		t.Fatalf("expected profile-enriched member identities, got %s", string(payload))
	}
}

func TestMCPRoomMembersRejectsBlockedRooms(t *testing.T) {
	transport := &roomCreatorReadingTransport{
		recordingTransport: &recordingTransport{},
		creators:           map[string]string{"!blocked:example.com": "@actual-creator:example.com"},
	}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!blocked:example.com",
		"name":    "Blocked Group",
	})
	mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"mcp_blocked_room_ids": []any{group.RoomID},
	})

	_, apiErr := service.invokeDirextalkMCP(context.Background(), dirextalkmcp.ActionRoomMembersList, map[string]any{
		"room_id": group.RoomID,
	})
	if apiErr == nil || apiErr.Status != 403 || !strings.Contains(apiErr.Error, "blocked") {
		t.Fatalf("expected blocked room member list to fail, got %#v", apiErr)
	}
	if len(transport.reads) != 0 {
		t.Fatalf("blocked room resolved or persisted creator before authorization: %#v", transport.reads)
	}
}

func TestMCPMessagesListUsesAgentRoomNameAndDisplayName(t *testing.T) {
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, &recordingTransport{roomMembers: []memberRecord{
		{RoomID: "!agents:example.com", UserID: "@owner:example.com", Membership: "join"},
	}})
	service.agentRoomID = "!agents:example.com"
	service.agentConfig = normalizeAgentConfig(agentConfig{
		NativeAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Codex Native"},
		OnlineAgentIdentity: dirextalkdomain.AgentIdentityConfig{DisplayName: "Codex Online"},
	})
	service.SetMatrixMessageReader(&fakeMCPMessageReader{messages: []mcpMessageSummary{
		{EventID: "$question", OriginServerTS: 1710000000000, CreatedAt: "2024-03-09T16:00:00Z", Sender: "owner", Msg: "question", SenderMXID: "@owner:example.com"},
		{EventID: "$answer", OriginServerTS: 1710000100000, CreatedAt: "2024-03-09T16:01:40Z", Sender: "agent", Msg: "answer", SenderMXID: "@agent:example.com"},
	}})

	result := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionMessagesList, map[string]any{
		"room_id": "!agents:example.com",
	})
	if result["room_id"] != "!agents:example.com" || result["name"] != agentRoomName {
		t.Fatalf("unexpected agent room envelope: %#v", result)
	}
	messages := result["messages"].([]mcpMessageSummary)
	if len(messages) != 2 || messages[0].Sender != "Codex Online" {
		t.Fatalf("expected agent sender display name, got %#v", messages)
	}
}

func TestMCPChannelPostsAndCommentsReturnConciseJSON(t *testing.T) {
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, &recordingTransport{
		eventID: "$post:event",
		ts:      1710000000000,
	})
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":       "product",
		"room_id":          "!channel:example.com",
		"name":             "Product Channel",
		"channel_type":     "post",
		"comments_enabled": true,
	})
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"body":       "post body",
	})
	commentResult := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionChannelCommentsCreate, map[string]any{
		"post_id": post.PostID,
		"msg":     "comment body",
	})
	if commentResult["ok"] != true || commentResult["post_id"] != post.PostID {
		t.Fatalf("unexpected comment create result: %#v", commentResult)
	}

	posts := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionChannelPostsList, map[string]any{
		"room_id": ch.RoomID,
	})
	if posts["channel_id"] != ch.ChannelID || posts["room_id"] != ch.RoomID {
		t.Fatalf("expected channel identity in post result, got %#v", posts)
	}
	gotPosts := posts["posts"].([]mcpPostSummary)
	if len(gotPosts) != 1 || gotPosts[0].PostID != post.PostID || gotPosts[0].Msg != "post body" {
		t.Fatalf("unexpected post summaries: %#v", gotPosts)
	}

	comments := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionChannelCommentsList, map[string]any{
		"post_id": post.PostID,
	})
	gotComments := comments["comments"].([]mcpCommentSummary)
	if len(gotComments) != 1 || gotComments[0].Msg != "comment body" {
		t.Fatalf("unexpected comment summaries: %#v", gotComments)
	}
}

func TestMCPChannelPostsPaginationUsesStableSnapshotAndReadableCounts(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":       "product",
		"room_id":          "!channel:example.com",
		"name":             "Product Channel",
		"comments_enabled": true,
	})
	base := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		mustInsertChannelPost(t, service, channelPostRecord{
			PostID:         "post_" + string(rune('a'+i)),
			ChannelID:      ch.ChannelID,
			RoomID:         ch.RoomID,
			EventID:        "$post_" + string(rune('a'+i)),
			AuthorMXID:     "@owner:example.com",
			AuthorName:     "Owner",
			Body:           "post body",
			OriginServerTS: base.Add(time.Duration(i) * time.Minute).UnixMilli(),
		})
	}
	mustInsertChannelComment(t, service, channelCommentRecord{
		CommentID:      "comment_1",
		PostID:         "post_l",
		ChannelID:      ch.ChannelID,
		OriginServerTS: base.Add(13 * time.Minute).UnixMilli(),
	})
	mustUpsertReaction(t, service, reactionRecord{
		TargetType: "post",
		TargetID:   "post_l",
		Reaction:   "like",
		UserID:     "@owner:example.com",
		Active:     true,
	})
	mustUpsertReaction(t, service, reactionRecord{
		TargetType: "post",
		TargetID:   "post_l",
		ChannelID:  ch.ChannelID,
		PostID:     "post_l",
		Reaction:   "favorite",
		UserID:     "@owner:example.com",
		Active:     true,
	})

	first := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionChannelPostsList, map[string]any{
		"room_id": ch.RoomID,
		"limit":   float64(5),
	})
	firstPosts := first["posts"].([]mcpPostSummary)
	if len(firstPosts) != 5 || firstPosts[0].PostID != "post_l" || firstPosts[4].PostID != "post_h" {
		t.Fatalf("expected newest five posts, got %#v", firstPosts)
	}
	if firstPosts[0].CreatedAt != "2026-07-05T10:11:00Z" ||
		firstPosts[0].CommentCount != 1 ||
		firstPosts[0].LikeCount != 1 ||
		firstPosts[0].FavoriteCount != 1 ||
		!firstPosts[0].FavoritedByMe {
		t.Fatalf("expected readable post counts and time, got %#v", firstPosts[0])
	}
	cursor, _ := first["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("expected next_cursor in first page: %#v", first)
	}

	for i := 0; i < 10; i++ {
		mustInsertChannelPost(t, service, channelPostRecord{
			PostID:         "new_post_" + string(rune('a'+i)),
			ChannelID:      ch.ChannelID,
			RoomID:         ch.RoomID,
			EventID:        "$new_post_" + string(rune('a'+i)),
			AuthorMXID:     "@owner:example.com",
			AuthorName:     "Owner",
			Body:           "newer post",
			OriginServerTS: time.Now().UTC().Add(time.Hour + time.Duration(i)*time.Minute).UnixMilli(),
		})
	}

	second := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionChannelPostsList, map[string]any{
		"room_id": ch.RoomID,
		"cursor":  cursor,
		"limit":   float64(5),
	})
	secondPosts := second["posts"].([]mcpPostSummary)
	if len(secondPosts) != 5 || secondPosts[0].PostID != "post_g" || secondPosts[4].PostID != "post_c" {
		t.Fatalf("expected cursor to continue original snapshot, got %#v", secondPosts)
	}
	for _, post := range secondPosts {
		if strings.HasPrefix(post.PostID, "new_post_") {
			t.Fatalf("cursor page must not include newer inserts: %#v", secondPosts)
		}
	}
}

func TestMCPChannelCommentsPaginationUsesStableSnapshot(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":       "product",
		"room_id":          "!channel:example.com",
		"name":             "Product Channel",
		"comments_enabled": true,
	})
	mustInsertChannelPost(t, service, channelPostRecord{
		PostID:         "post",
		ChannelID:      ch.ChannelID,
		RoomID:         ch.RoomID,
		EventID:        "$post",
		OriginServerTS: time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC).UnixMilli(),
	})
	base := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 12; i++ {
		mustInsertChannelComment(t, service, channelCommentRecord{
			CommentID:      "comment_" + string(rune('a'+i)),
			PostID:         "post",
			ChannelID:      ch.ChannelID,
			AuthorMXID:     "@owner:example.com",
			AuthorName:     "Owner",
			Body:           "comment body",
			OriginServerTS: base.Add(time.Duration(i) * time.Minute).UnixMilli(),
		})
	}

	first := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionChannelCommentsList, map[string]any{
		"post_id": "post",
		"limit":   float64(5),
	})
	firstComments := first["comments"].([]mcpCommentSummary)
	if len(firstComments) != 5 || firstComments[0].CommentID != "comment_l" || firstComments[4].CommentID != "comment_h" {
		t.Fatalf("expected newest five comments, got %#v", firstComments)
	}
	if firstComments[0].CreatedAt != "2026-07-05T10:11:00Z" {
		t.Fatalf("expected readable comment time, got %#v", firstComments[0])
	}
	cursor, _ := first["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("expected next_cursor in first page: %#v", first)
	}

	for i := 0; i < 10; i++ {
		mustInsertChannelComment(t, service, channelCommentRecord{
			CommentID:      "new_comment_" + string(rune('a'+i)),
			PostID:         "post",
			ChannelID:      ch.ChannelID,
			AuthorMXID:     "@owner:example.com",
			AuthorName:     "Owner",
			Body:           "newer comment",
			OriginServerTS: time.Now().UTC().Add(time.Hour + time.Duration(i)*time.Minute).UnixMilli(),
		})
	}

	second := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionChannelCommentsList, map[string]any{
		"post_id": "post",
		"cursor":  cursor,
		"limit":   float64(5),
	})
	secondComments := second["comments"].([]mcpCommentSummary)
	if len(secondComments) != 5 || secondComments[0].CommentID != "comment_g" || secondComments[4].CommentID != "comment_c" {
		t.Fatalf("expected cursor to continue original snapshot, got %#v", secondComments)
	}
	for _, comment := range secondComments {
		if strings.HasPrefix(comment.CommentID, "new_comment_") {
			t.Fatalf("cursor page must not include newer inserts: %#v", secondComments)
		}
	}
}

func TestMCPChannelContentRejectsBlockedRooms(t *testing.T) {
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, &recordingTransport{
		eventID: "$post:event",
		ts:      1710000000000,
	})
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":       "product",
		"room_id":          "!blocked-channel:example.com",
		"name":             "Product Channel",
		"channel_type":     "post",
		"comments_enabled": true,
	})
	post := mustHandle[channelPostRecord](t, service, "channels.posts.create", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"body":       "post body",
	})
	mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"mcp_blocked_room_ids": []any{ch.RoomID},
	})

	for _, tc := range []struct {
		action string
		params map[string]any
	}{
		{dirextalkmcp.ActionChannelPostsList, map[string]any{"room_id": ch.RoomID}},
		{dirextalkmcp.ActionChannelCommentsList, map[string]any{"post_id": post.PostID}},
		{dirextalkmcp.ActionChannelCommentsCreate, map[string]any{"post_id": post.PostID, "msg": "blocked"}},
	} {
		_, apiErr := service.invokeDirextalkMCP(context.Background(), tc.action, tc.params)
		if apiErr == nil || apiErr.Status != 403 || !strings.Contains(apiErr.Error, "blocked") {
			t.Fatalf("expected %s to reject blocked room, got %#v", tc.action, apiErr)
		}
	}
}
