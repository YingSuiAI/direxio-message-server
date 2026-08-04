package p2p

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/pushrules"
	agentmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

func TestGroupUsesJoinedAndUnifiedChannelsUseSharedHistoryVisibility(t *testing.T) {
	transport := &recordingTransport{}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)

	mustHandle[groupRecord](t, service, "groups.create", map[string]any{"name": "Team"})
	transport.roomID = "!chat:example.com"
	mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":   "chat",
		"name":         "Chat",
		"channel_type": "chat",
	})
	transport.roomID = "!text:example.com"
	mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":   "text",
		"name":         "Text",
		"channel_type": "text",
	})

	if len(transport.createRooms) != 3 {
		t.Fatalf("expected group, chat channel, and text channel rooms to be created, got %#v", transport.createRooms)
	}
	if got, ok := initialHistoryVisibility(transport.createRooms[0]); !ok || got != string(gomatrixserverlib.HistoryVisibilityJoined) {
		t.Fatalf("expected joined history visibility for group create, got %q ok=%v in %#v", got, ok, transport.createRooms[0].InitialState)
	}
	for _, req := range transport.createRooms[1:] {
		if got, ok := initialHistoryVisibility(req); !ok || got != string(gomatrixserverlib.HistoryVisibilityShared) {
			t.Fatalf("expected shared history visibility for unified channel create, got %q ok=%v in %#v", got, ok, req.InitialState)
		}
	}
}

func TestChannelCreateDefaultsToSharedHistoryVisibility(t *testing.T) {
	transport := &recordingTransport{roomID: "!posts:example.com"}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)

	mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "posts",
		"name":       "Posts",
	})

	if len(transport.createRooms) != 1 {
		t.Fatalf("expected channel room to be created, got %#v", transport.createRooms)
	}
	if got, ok := initialHistoryVisibility(transport.createRooms[0]); !ok || got != string(gomatrixserverlib.HistoryVisibilityShared) {
		t.Fatalf("unified channels must use shared history visibility for posts/comments, got %q ok=%v in %#v", got, ok, transport.createRooms[0].InitialState)
	}
}

func TestChannelCreateWithExistingRoomPublishesSharedHistoryVisibility(t *testing.T) {
	transport := &recordingTransport{}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)

	mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":   "posts",
		"room_id":      "!existing:example.com",
		"name":         "Posts",
		"channel_type": "chat",
	})

	if len(transport.createRooms) != 0 {
		t.Fatalf("existing room channel must not create a new room, got %#v", transport.createRooms)
	}
	if len(transport.stateEvents) != 1 {
		t.Fatalf("expected existing channel room to publish shared history visibility, got %#v", transport.stateEvents)
	}
	if got, ok := updateStateHistoryVisibility(transport.stateEvents[0]); !ok || got != string(gomatrixserverlib.HistoryVisibilityShared) {
		t.Fatalf("expected existing channel room to publish shared history visibility, got %q ok=%v in %#v", got, ok, transport.stateEvents[0])
	}
}

func TestServiceUsesTransportForRooms(t *testing.T) {
	transport := &recordingTransport{roomID: "!matrix-room:example.com"}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)

	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{"name": "Team"})
	if group.RoomID != transport.roomID {
		t.Fatalf("expected transport room_id, got %#v", group)
	}
	if len(transport.createRooms) != 1 || transport.createRooms[0].Name != "Team" {
		t.Fatalf("expected group room creation through transport, got %#v", transport.createRooms)
	}
}

func TestEnsureAgentRoomCreatesRealRoomWithOnlineIdentityForLegacyID(t *testing.T) {
	transport := &recordingTransport{roomID: "!agents-real:example.com"}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)

	changed, err := service.ensureAgentRoom(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatalf("expected legacy agent room id to be replaced")
	}
	if service.agentRoomID != "!agents-real:example.com" {
		t.Fatalf("expected real agent room id, got %q", service.agentRoomID)
	}
	if len(transport.createRooms) != 1 {
		t.Fatalf("expected one agent room create, got %#v", transport.createRooms)
	}
	req := transport.createRooms[0]
	if req.CreatorMXID != "@owner:example.com" || req.Name != "Agents" || req.Visibility != "private" || req.RoomType != "" {
		t.Fatalf("unexpected agent room create request: %#v", req)
	}
	if len(req.InviteMXIDs) != 1 || req.InviteMXIDs[0] != "@agent:example.com" {
		t.Fatalf("expected agent invite on room create, got %#v", req.InviteMXIDs)
	}
	if level, ok := initialPowerLevelForUser(req.InitialState, "@agent:example.com"); !ok || level < 50 {
		t.Fatalf("expected created agent room to grant @agent state power, level=%d ok=%v state=%#v", level, ok, req.InitialState)
	}
	if statusState, ok := initialStateOfType(req.InitialState, DirextalkAgentStatusEventType); ok {
		t.Fatalf("agent status state must be sent after join, not as owner-created initial state: %#v", statusState)
	}
	if len(transport.joinRequests) != 1 || transport.joinRequests[0].UserMXID != "@agent:example.com" || transport.joinRequests[0].DisplayName != agentmodule.DefaultOnlineAgentDisplayName {
		t.Fatalf("expected Online Agent to join created room once with its Matrix identity, got %#v", transport.joinRequests)
	}
	if len(transport.stateEvents) != 1 {
		t.Fatalf("expected agent to publish status after joining created room, got %#v", transport.stateEvents)
	}
	if online, ok := agentStatusOnlineUpdate(transport.stateEvents[0], "!agents-real:example.com", "@agent:example.com", "@agent:example.com"); !ok || online {
		t.Fatalf("expected agent room repair to publish offline status until gateway connects, got %#v", transport.stateEvents[0])
	}
}

func TestEnsureAgentRoomMutesOwnerPushRuleByDefault(t *testing.T) {
	transport := &recordingTransport{roomID: "!agents-real:example.com"}
	pushRules := &recordingPushRuleManager{
		ruleSets: pushrules.DefaultAccountRuleSets(ownerLocalpart, "example.com"),
	}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.SetPushRuleManager(pushRules)

	if _, err := service.ensureAgentRoom(context.Background()); err != nil {
		t.Fatal(err)
	}
	if pushRules.putUserID != "@owner:example.com" {
		t.Fatalf("expected owner push rules to be updated, got %q", pushRules.putUserID)
	}
	if pushRules.putRuleSets == nil {
		t.Fatal("expected push rules to be saved")
	}
	for _, rule := range pushRules.putRuleSets.Global.Room {
		if rule.RuleID == "!agents-real:example.com" && rule.Enabled && !rule.Default && len(rule.Actions) == 0 {
			return
		}
	}
	t.Fatalf("expected agent room to default to a room-level dont_notify push rule, got %#v", pushRules.putRuleSets.Global.Room)
}

func TestEnsureAgentRoomJoinsAgentAndOwnerForExistingRealRoom(t *testing.T) {
	transport := &recordingTransport{}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.agentRoomID = "!agents-real:example.com"
	service.agentConfig.DisplayName = "Codex"

	changed, err := service.ensureAgentRoom(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatalf("expected existing real agent room id to remain unchanged")
	}
	if len(transport.createRooms) != 0 {
		t.Fatalf("expected no room create for existing agent room, got %#v", transport.createRooms)
	}
	if len(transport.joinRequests) != 2 {
		t.Fatalf("expected agent and owner to join existing room, got %#v", transport.joinRequests)
	}
	if transport.joinRequests[0].UserMXID != "@agent:example.com" || transport.joinRequests[0].DisplayName != "Codex" {
		t.Fatalf("expected agent to join existing room first, got %#v", transport.joinRequests)
	}
	if transport.joinRequests[1].UserMXID != "@owner:example.com" {
		t.Fatalf("expected owner to join existing room, got %#v", transport.joinRequests)
	}
	if len(transport.stateEvents) != 2 {
		t.Fatalf("expected existing agent room to repair power levels and publish status state, got %#v", transport.stateEvents)
	}
	if transport.stateEvents[0].RoomID != "!agents-real:example.com" ||
		transport.stateEvents[0].SenderMXID != "@owner:example.com" ||
		transport.stateEvents[0].Event.Type != spec.MRoomPowerLevels {
		t.Fatalf("expected owner to repair agent room power levels first, got %#v", transport.stateEvents[0])
	}
	if level, ok := initialPowerLevelForUser([]RoomStateEvent{transport.stateEvents[0].Event}, "@agent:example.com"); !ok || level < 50 {
		t.Fatalf("expected repaired power levels to grant @agent state power, level=%d ok=%v state=%#v", level, ok, transport.stateEvents[0])
	}
	if online, ok := agentStatusOnlineUpdate(transport.stateEvents[1], "!agents-real:example.com", "@agent:example.com", "@agent:example.com"); !ok || online {
		t.Fatalf("expected existing agent room status to remain offline until gateway connects, got %#v", transport.stateEvents[1])
	}
}

func TestEnsureAgentRoomInvitesOwnerFromAgentWhenOwnerJoinRequiresInvite(t *testing.T) {
	transport := &ownerJoinRequiresInviteTransport{}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	service.agentRoomID = "!agents-real:example.com"

	changed, err := service.ensureAgentRoom(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Fatalf("expected existing real agent room id to remain unchanged")
	}
	if len(transport.joinRequests) != 3 {
		t.Fatalf("expected agent join and owner join retry, got %#v", transport.joinRequests)
	}
	if len(transport.inviteRequests) != 1 ||
		transport.inviteRequests[0].InviterMXID != "@agent:example.com" ||
		transport.inviteRequests[0].InviteeMXID != "@owner:example.com" {
		t.Fatalf("expected agent to invite owner back into agents room, got %#v", transport.inviteRequests)
	}
	if len(transport.stateEvents) != 2 {
		t.Fatalf("expected repaired agent room to repair power levels and publish status state, got %#v", transport.stateEvents)
	}
}

func TestSyncBootstrapIncludesAgentRoomID(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	service.agentRoomID = "!agents-real:example.com"
	service.systemRoomID = "!system-real:example.com"
	bootstrapService(t, service)

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	if bootstrap["agent_room_id"] != "!agents-real:example.com" {
		t.Fatalf("expected sync.bootstrap agent_room_id, got %#v", bootstrap)
	}
	if bootstrap["system_room_id"] != "!system-real:example.com" {
		t.Fatalf("expected sync.bootstrap system_room_id, got %#v", bootstrap)
	}
}

func TestReportSubmitCreatesSystemNotificationMessage(t *testing.T) {
	transport := &recordingTransport{roomID: "!system:example.com", eventID: "$report:example.com", ts: 1783433640000}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Product Exchange Group",
	})

	result := mustHandle[reportRecord](t, service, "reports.submit", map[string]any{
		"target_type":           "group",
		"room_id":               group.RoomID,
		"reason":                "Spam / Advertisement",
		"body":                  "Suspicious advertisement",
		"reporter_mxid":         "@zhangsan:remote.example",
		"reporter_display_name": "Zhang San",
		"image_urls":            []any{"mxc://example.com/a", "mxc://example.com/b"},
	})

	if result.EventID != "$report:example.com" || result.SystemRoomID != "!system:example.com" {
		t.Fatalf("expected report response to include system message ids, got %#v", result)
	}
	if service.systemRoomID != "!system:example.com" {
		t.Fatalf("expected service system room id to be persisted in memory, got %q", service.systemRoomID)
	}
	if len(transport.createRooms) != 1 {
		t.Fatalf("expected one system room create after explicit group room id, got %#v", transport.createRooms)
	}
	if transport.createRooms[0].Name != "System Notification" ||
		transport.createRooms[0].Visibility != "private" ||
		transport.createRooms[0].RoomType != DirextalkRoomTypeSystem {
		t.Fatalf("expected private system room creation, got %#v", transport.createRooms[0])
	}
	if len(transport.messages) != 1 {
		t.Fatalf("expected one system report message, got %#v", transport.messages)
	}
	message := transport.messages[0]
	if message.RoomID != "!system:example.com" || message.SenderMXID != "@owner:example.com" {
		t.Fatalf("expected owner to send report notification into system room, got %#v", message)
	}
	if message.Content["msg_type"] != "report" ||
		message.Content["p2p_kind"] != "system_report" ||
		message.Content["target_room_id"] != group.RoomID ||
		message.Content["target_name"] != "Product Exchange Group" ||
		message.Content["reporter_mxid"] != "@zhangsan:remote.example" ||
		message.Content["reporter_display_name"] != "Zhang San" ||
		message.Content["reason"] != "Spam / Advertisement" {
		t.Fatalf("expected report notification content, got %#v", message.Content)
	}
	images, ok := message.Content["image_urls"].([]string)
	if !ok || len(images) != 2 || images[0] != "mxc://example.com/a" {
		t.Fatalf("expected report image urls in notification, got %#v", message.Content["image_urls"])
	}
	list := mustHandle[map[string]any](t, service, "conversations.list", nil)
	conversations := list["conversations"].([]conversationView)
	var systemConversation conversationView
	for _, conversation := range conversations {
		if conversation.Kind == conversationKindSystem {
			systemConversation = conversation
			break
		}
	}
	if systemConversation.MatrixRoomID != "!system:example.com" ||
		systemConversation.Title != systemRoomName ||
		systemConversation.LastEventID != "$report:example.com" ||
		systemConversation.LastActivityAt != 1783433640000 ||
		!systemConversation.Capabilities.Open {
		t.Fatalf("expected open system conversation after report, got %#v", conversations)
	}
}

func TestAgentConfigUpdatePublishesAgentRoomStatusState(t *testing.T) {
	transport := &recordingTransport{}
	service := NewServiceWithTransport(withTestExternalAgent(Config{ServerName: "example.com"}), transport)
	service.agentRoomID = "!agents-real:example.com"
	bootstrapService(t, service)

	result := mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"enabled": false,
	})

	if result["enabled"] != false {
		t.Fatalf("expected disabled agent config response, got %#v", result)
	}
	if len(transport.stateEvents) != 1 {
		t.Fatalf("expected one agent status state update, got %#v", transport.stateEvents)
	}
	if online, ok := agentStatusOnlineUpdate(transport.stateEvents[0], "!agents-real:example.com", "@agent:example.com", "@agent:example.com"); !ok || online {
		t.Fatalf("expected disabled agent status state, got %#v", transport.stateEvents[0])
	}
}

func TestAgentConfigEnableDoesNotPublishOnlineState(t *testing.T) {
	transport := &recordingTransport{}
	service := NewServiceWithTransport(withTestExternalAgent(Config{ServerName: "example.com"}), transport)
	service.agentRoomID = "!agents-real:example.com"
	service.agentConfig.Enabled = false
	bootstrapService(t, service)

	result := mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"enabled": true,
	})

	if result["enabled"] != true {
		t.Fatalf("expected enabled agent config response, got %#v", result)
	}
	if len(transport.stateEvents) != 0 {
		t.Fatalf("enabling config alone must not publish online status, got %#v", transport.stateEvents)
	}
}
