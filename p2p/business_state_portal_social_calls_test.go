package p2p

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestFavoriteAddIsIdempotentByEventAndRoom(t *testing.T) {
	service := NewService(withTestExternalAgent(Config{ServerName: "example.com"}))
	bootstrapService(t, service)

	first := mustHandle[favoriteRecord](t, service, "favorites.add", map[string]any{
		"event_id":     "$event",
		"room_id":      "!room:example.com",
		"content":      "first",
		"message_type": "text",
	})
	second := mustHandle[favoriteRecord](t, service, "favorites.add", map[string]any{
		"event_id":     "$event",
		"room_id":      "!room:example.com",
		"content":      "updated",
		"message_type": "text",
	})
	if second.ID != first.ID {
		t.Fatalf("expected duplicate favorite to reuse id %d, got %d", first.ID, second.ID)
	}
	favorites := mustHandle[map[string]any](t, service, "favorites.list", map[string]any{"message_type": "text"})["favorites"].([]favoriteRecord)
	if len(favorites) != 1 || favorites[0].ID != first.ID || favorites[0].Content != "updated" {
		t.Fatalf("expected one updated favorite, got %#v", favorites)
	}

	otherRoom := mustHandle[favoriteRecord](t, service, "favorites.add", map[string]any{
		"event_id":     "$event",
		"room_id":      "!other:example.com",
		"content":      "other",
		"message_type": "text",
	})
	if otherRoom.ID == first.ID {
		t.Fatalf("expected same event in a different room to get a separate favorite, got %#v", otherRoom)
	}
}

func TestStoredFavoriteAddIsIdempotentByEventAndRoom(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)

	first := mustHandle[favoriteRecord](t, service, "favorites.add", map[string]any{
		"event_id":     "$stored",
		"room_id":      "!room:example.com",
		"content":      "first",
		"message_type": "text",
	})
	second := mustHandle[favoriteRecord](t, service, "favorites.add", map[string]any{
		"event_id":     "$stored",
		"room_id":      "!room:example.com",
		"content":      "updated",
		"message_type": "text",
	})
	if second.ID != first.ID {
		t.Fatalf("expected stored duplicate favorite to reuse id %d, got %d", first.ID, second.ID)
	}

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	favorites := mustHandle[map[string]any](t, reloaded, "favorites.list", map[string]any{"message_type": "text"})["favorites"].([]favoriteRecord)
	if len(favorites) != 1 || favorites[0].ID != first.ID || favorites[0].Content != "updated" {
		t.Fatalf("expected one stored updated favorite after reload, got %#v", favorites)
	}
}

func TestCallActionsPersistChangedEvents(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	created := mustHandle[callRecord](t, service, "calls.create", map[string]any{
		"call_id":    "call_1",
		"room_id":    "!room:example.com",
		"media_type": "video",
	})
	ended := mustHandle[callRecord](t, service, "calls.event", map[string]any{
		"call_id": created.CallID,
		"event":   "ended",
		"reason":  "completed",
	})
	loaded := mustHandle[callRecord](t, service, "calls.get", map[string]any{"call_id": created.CallID})
	if loaded != ended {
		t.Fatalf("calls.get returned %#v, want %#v", loaded, ended)
	}
	p2pEvents := mustListP2PEvents(t, service)
	if len(p2pEvents) != 2 || p2pEvents[1].Type != "call.changed" || p2pEvents[1].RoomID != ended.RoomID {
		t.Fatalf("call actions emitted unexpected product events: %#v", p2pEvents)
	}
	if payloadCall, ok := p2pEvents[1].Payload["call"].(callRecord); !ok || payloadCall != ended {
		t.Fatalf("call.changed payload = %#v, want %#v", p2pEvents[1].Payload, ended)
	}
}

func TestSyncBootstrapIncludesGroupAndChannelInvites(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	group := groupRecord{
		RoomID: "!group:example.com",
		Name:   "产品群",
	}
	if err := service.saveGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	ch := channel{
		ChannelID: "product",
		RoomID:    "!product:example.com",
		Name:      "产品频道",
	}
	if err := service.saveChannel(context.Background(), ch); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     group.RoomID,
		UserID:     "@owner:example.com",
		Domain:     "example.com",
		Membership: "invite",
		Role:       "member",
		JoinedAt:   1770000000000,
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     ch.RoomID,
		ChannelID:  ch.ChannelID,
		UserID:     "@owner:example.com",
		Domain:     "example.com",
		Membership: "invite",
		Role:       "member",
		JoinedAt:   1770000000001,
	}); err != nil {
		t.Fatal(err)
	}

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	pending := bootstrap["pending"].(map[string]any)
	groupInvites := pending["group_invites"].([]map[string]any)
	channelNotices := pending["channel_notices"].([]map[string]any)
	if len(groupInvites) != 1 {
		t.Fatalf("expected one pending group invite, got %#v", pending["group_invites"])
	}
	groupInvite := groupInvites[0]
	if groupInvite["id"] != group.RoomID || groupInvite["title"] != group.Name {
		t.Fatalf("expected pending group invite, got %#v", pending["group_invites"])
	}
	if len(channelNotices) != 1 {
		t.Fatalf("expected one pending channel invite, got %#v", pending["channel_notices"])
	}
	channelNotice := channelNotices[0]
	if channelNotice["id"] != ch.RoomID || channelNotice["title"] != ch.Name {
		t.Fatalf("expected pending channel invite notice, got %#v", pending["channel_notices"])
	}
	if groups := bootstrap["groups"].([]groupRecord); len(groups) != 0 {
		t.Fatalf("expected invited group hidden from bootstrap main groups, got %#v", groups)
	}
	if channels := bootstrap["channels"].([]channel); len(channels) != 0 {
		t.Fatalf("expected invited channel hidden from bootstrap main channels, got %#v", channels)
	}
}

func TestPortalStatusReportsStorageAndProjectorMode(t *testing.T) {
	memoryService := NewService(Config{ServerName: "example.com"})
	memoryStatus := mustHandle[map[string]any](t, memoryService, "portal.status", nil)
	if memoryStatus["store_mode"] != "memory" || memoryStatus["projector_started"] != false || memoryStatus["policy_index_mode"] != "unavailable" || memoryStatus["policy_index_ready"] != false {
		t.Fatalf("expected memory service status to expose storage and projector mode, got %#v", memoryStatus)
	}

	transportService := NewServiceWithTransport(Config{ServerName: "example.com"}, &recordingTransport{})
	transportStatus := mustHandle[map[string]any](t, transportService, "portal.status", nil)
	if transportStatus["policy_index_mode"] != "matrix_state" || transportStatus["policy_index_ready"] != true {
		t.Fatalf("expected transport service status to expose Matrix-state policy index mode, got %#v", transportStatus)
	}

	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	databaseService, err := NewServiceWithStore(ctx, withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	databaseService.SetProjectorStarted(true)
	databaseStatus := mustHandle[map[string]any](t, databaseService, "portal.status", nil)
	if databaseStatus["store_mode"] != "database" || databaseStatus["projector_started"] != true || databaseStatus["policy_index_ready"] != false {
		t.Fatalf("expected database service status to expose storage and projector mode, got %#v", databaseStatus)
	}
}

func TestPortalPasswordSetupAndAgentActions(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	status := mustHandle[map[string]any](t, service, "portal.status", nil)
	assertSingleInitializedFlag(t, status, false)
	requireEightDigitPassword(t, service.password)
	auth := mustHandle[map[string]any](t, service, "portal.auth", map[string]any{"password": service.password})
	authDeviceID, _ := auth["device_id"].(string)
	if !strings.HasPrefix(authDeviceID, "PORTALIM") {
		t.Fatalf("expected auth session without requested device_id to expose generated Matrix device id, got %#v", auth)
	}
	assertSingleInitializedFlag(t, auth, false)
	profile := mustHandle[ownerProfile](t, service, "profile.get", nil)
	if profile.DisplayName != "" {
		t.Fatalf("expected default owner display name to be empty, got %#v", profile)
	}

	defaultPassword := service.password
	bootstrap := bootstrapService(t, service)
	oldAccessToken := bootstrap["access_token"].(string)
	assertSingleInitializedFlag(t, bootstrap, false)

	mustHandle[ownerProfile](t, service, "profile.update", map[string]any{
		"display_name": "Alice",
	})
	profileOnlyAuth := mustHandle[map[string]any](t, service, "portal.auth", map[string]any{"password": defaultPassword})
	assertSingleInitializedFlag(t, profileOnlyAuth, false)
	password := mustHandle[map[string]any](t, service, "portal.password", map[string]any{
		"old_password": defaultPassword,
		"new_password": "new-secret",
	})
	if password["access_token"] == "" {
		t.Fatalf("expected refreshed access token after password change, got %#v", password)
	}
	assertSingleInitializedFlag(t, password, true)
	passwordDeviceID, _ := password["device_id"].(string)
	if !strings.HasPrefix(passwordDeviceID, "PORTALIM") {
		t.Fatalf("expected password session without requested device_id to expose generated Matrix device id, got %#v", password)
	}
	if _, err := service.Handle(context.Background(), "portal.auth", map[string]any{"password": defaultPassword}); err == nil {
		t.Fatalf("expected old password to fail after password change")
	}
	nextAuth := mustHandle[map[string]any](t, service, "portal.auth", map[string]any{"password": "new-secret"})
	assertSingleInitializedFlag(t, nextAuth, true)

	agentPassword := mustHandle[map[string]any](t, service, "agent.password", nil)
	if agentPassword["password"] != "new-secret" {
		t.Fatalf("expected agent password lookup to return current password, got %#v", agentPassword)
	}
	if service.Authenticate(oldAccessToken) {
		t.Fatalf("expected old access token to be rotated after password change")
	}
}

func TestAgentConfigContactsFavoritesAndReports(t *testing.T) {
	service := NewService(withTestExternalAgent(Config{ServerName: "example.com"}))
	bootstrapService(t, service)

	cfg := mustHandle[map[string]any](t, service, "agent.config.update", map[string]any{
		"native_agent_identity": map[string]any{
			"display_name": "Ops Agent",
			"avatar_url":   "mxc://example.com/agent",
		},
		"online_agent_identity": map[string]any{
			"display_name": "Online Agent",
			"avatar_url":   "mxc://example.com/online-agent",
		},
		"enabled":              true,
		"mcp_blocked_room_ids": []any{"!secret:example.com", "!group:example.com"},
	})
	nativeIdentity := cfg["native_agent_identity"].(map[string]any)
	onlineIdentity := cfg["online_agent_identity"].(map[string]any)
	if nativeIdentity["display_name"] != "Ops Agent" || nativeIdentity["avatar_url"] != "mxc://example.com/agent" ||
		onlineIdentity["display_name"] != "Online Agent" || onlineIdentity["avatar_url"] != "mxc://example.com/online-agent" || cfg["enabled"] != true {
		t.Fatalf("expected updated agent config, got %#v", cfg)
	}
	blockedRooms, ok := cfg["mcp_blocked_room_ids"].([]string)
	if !ok || len(blockedRooms) != 2 || blockedRooms[0] != "!secret:example.com" || blockedRooms[1] != "!group:example.com" {
		t.Fatalf("expected normalized blocked room ids, got %#v", cfg["mcp_blocked_room_ids"])
	}
	cfg = mustHandle[map[string]any](t, service, "agent.config.get", nil)
	nativeIdentity = cfg["native_agent_identity"].(map[string]any)
	onlineIdentity = cfg["online_agent_identity"].(map[string]any)
	if nativeIdentity["display_name"] != "Ops Agent" || nativeIdentity["avatar_url"] != "mxc://example.com/agent" ||
		onlineIdentity["display_name"] != "Online Agent" || onlineIdentity["avatar_url"] != "mxc://example.com/online-agent" {
		t.Fatalf("expected persisted agent config, got %#v", cfg)
	}

	contact := mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         "@alice:example.com",
		"display_name": "Alice",
	})
	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)
	if got := contacts["contacts"].([]contactRecord); len(got) != 1 || got[0].RoomID != contact.RoomID {
		t.Fatalf("expected contact list with alice, got %#v", contacts)
	}
	mustHandle[map[string]any](t, service, "contacts.requests.delete", map[string]any{"room_id": contact.RoomID})
	contacts = mustHandle[map[string]any](t, service, "contacts.list", nil)
	if got := contacts["contacts"].([]contactRecord); len(got) != 0 {
		t.Fatalf("expected deleted contact request gone, got %#v", contacts)
	}

	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    "@bob:remote.example",
		DisplayName: "Bob",
		Domain:      "remote.example",
		RoomID:      "!bob:remote.example",
		Status:      "pending_inbound",
	}); err != nil {
		t.Fatalf("failed to seed inbound contact: %s", err)
	}
	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	pending := bootstrap["pending"].(map[string]any)
	friendRequests := pending["friend_requests"].([]map[string]any)
	if len(friendRequests) != 1 || friendRequests[0]["id"] != "!bob:remote.example" || friendRequests[0]["title"] != "Bob" {
		t.Fatalf("expected pending inbound contact in friend requests, got %#v", pending)
	}

	fav1 := mustHandle[favoriteRecord](t, service, "favorites.add", map[string]any{"content": "one"})
	fav2 := mustHandle[favoriteRecord](t, service, "favorites.add", map[string]any{"content": "two"})
	mustHandle[map[string]any](t, service, "favorites.delete_batch", map[string]any{"ids": []any{float64(fav1.ID), float64(fav2.ID)}})
	favorites := mustHandle[map[string]any](t, service, "favorites.list", nil)
	if got := favorites["favorites"].([]favoriteRecord); len(got) != 0 {
		t.Fatalf("expected batch-deleted favorites gone, got %#v", favorites)
	}

	if _, apiErr := service.Handle(context.Background(), "reports.submit", map[string]any{
		"reporter_mxid": "@alice:example.com",
		"reason":        "spam",
	}); apiErr == nil || apiErr.Status != http.StatusBadRequest || apiErr.Error != "room_id or channel_id is required" {
		t.Fatalf("expected reports.submit without room/channel target to be rejected, got %#v", apiErr)
	}

}
