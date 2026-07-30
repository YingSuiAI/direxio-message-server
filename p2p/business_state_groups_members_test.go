package p2p

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestGroupsAndChannelsDoNotInferCreatorFromNodeOwner(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{"room_id": "!group:example.com", "name": "Group"})
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{"channel_id": "ch", "room_id": "!channel:example.com", "name": "Channel"})
	for _, target := range []struct {
		roomID  string
		profile map[string]any
	}{
		{roomID: group.RoomID, profile: map[string]any{"room_type": DirextalkRoomTypeGroup, "name": "Updated Group"}},
		{roomID: ch.RoomID, profile: map[string]any{"room_type": DirextalkRoomTypeChannel, "channel_id": ch.ChannelID, "name": "Updated Channel"}},
	} {
		conversation, ok, err := service.conversationModule.GetRecord(context.Background(), "", target.roomID)
		if err != nil || !ok || conversation.CreatedByMXID != "" {
			t.Fatalf("bound existing room must not infer creator for %s: (%#v, %v, %v)", target.roomID, conversation, ok, err)
		}
		profile := trustedStateEvent(t, target.roomID, "@profile-writer:remote.example", DirextalkRoomProfileEventType, "", target.profile)
		if err := service.ProjectRoomEvent(context.Background(), profile); err != nil {
			t.Fatal(err)
		}
		conversation, ok, err = service.conversationModule.GetRecord(context.Background(), "", target.roomID)
		if err != nil || !ok || conversation.CreatedByMXID != "" {
			t.Fatalf("later profile sender must not become creator for %s: (%#v, %v, %v)", target.roomID, conversation, ok, err)
		}
	}

	groupMembers := mustHandle[map[string]any](t, service, "groups.members", map[string]any{"room_id": group.RoomID})
	if got, ok := groupMembers["members"].([]memberRecord); !ok || len(got) != 1 || got[0].UserID != "@owner:example.com" {
		t.Fatalf("expected owner group member, got %#v", groupMembers)
	}
	channelMembers := mustHandle[map[string]any](t, service, "channels.members", map[string]any{"channel_id": ch.ChannelID, "room_id": ch.RoomID})
	if got, ok := channelMembers["members"].([]memberRecord); !ok || len(got) != 1 || got[0].UserID != "@owner:example.com" {
		t.Fatalf("expected owner channel member, got %#v", channelMembers)
	}
}

func TestGroupAndChannelMemberListsUseExactConversationCreator(t *testing.T) {
	ctx := context.Background()
	service := NewService(Config{ServerName: "reader.example.com"})
	tests := []struct {
		name      string
		action    string
		roomID    string
		channelID string
		kind      dirextalkdomain.ConversationKind
		params    map[string]any
	}{
		{
			name:   "group",
			action: "groups.members",
			roomID: "!group:creator.example.com",
			kind:   dirextalkdomain.ConversationKindGroup,
			params: map[string]any{"room_id": "!group:creator.example.com"},
		},
		{
			name:      "channel",
			action:    "channels.members",
			roomID:    "!channel:creator.example.com",
			channelID: "creator_channel",
			kind:      dirextalkdomain.ConversationKindChannel,
			params:    map[string]any{"channel_id": "creator_channel"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := service.conversationModule.Save(ctx, dirextalkdomain.ConversationRecord{
				MatrixRoomID:  tt.roomID,
				Kind:          tt.kind,
				CreatedByMXID: "@owner:creator.example.com",
			}); err != nil {
				t.Fatal(err)
			}
			for _, member := range []memberRecord{
				{RoomID: tt.roomID, ChannelID: tt.channelID, UserID: "@owner:member.example.com", Membership: "join", Role: "owner", JoinedAt: 10},
				{RoomID: tt.roomID, ChannelID: tt.channelID, UserID: "@alice:example.com", Membership: "join", Role: "member", JoinedAt: 20},
				{RoomID: tt.roomID, ChannelID: tt.channelID, UserID: "@owner:creator.example.com", Membership: "join", Role: "member", JoinedAt: 30},
			} {
				if err := service.store.UpsertMember(ctx, member); err != nil {
					t.Fatal(err)
				}
			}

			result := mustHandle[map[string]any](t, service, tt.action, tt.params)
			members := result["members"].([]memberRecord)
			if len(members) != 3 {
				t.Fatalf("member count = %d, want 3: %#v", len(members), members)
			}
			if got, want := []string{members[0].UserID, members[1].UserID, members[2].UserID}, []string{"@owner:creator.example.com", "@owner:member.example.com", "@alice:example.com"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("member order = %#v, want %#v", got, want)
			}
			if members[0].Role != "owner" || members[1].Role != "member" {
				t.Fatalf("creator roles were not canonicalized by exact MXID: %#v", members)
			}
		})
	}
}

func TestGroupAndChannelMemberReadsRepairLegacyCreatorForHTTPAndMCP(t *testing.T) {
	ctx := context.Background()
	const (
		staleCreator  = "@profile-writer:example.com"
		actualCreator = "@actual-creator:example.com"
	)
	for _, tc := range []struct {
		name      string
		action    string
		roomID    string
		channelID string
		kind      dirextalkdomain.ConversationKind
		params    map[string]any
	}{
		{
			name:   "group",
			action: "groups.members",
			roomID: "!legacy-group:example.com",
			kind:   dirextalkdomain.ConversationKindGroup,
			params: map[string]any{"room_id": "!legacy-group:example.com"},
		},
		{
			name:      "channel",
			action:    "channels.members",
			roomID:    "!legacy-channel:example.com",
			channelID: "legacy_channel",
			kind:      dirextalkdomain.ConversationKindChannel,
			params:    map[string]any{"room_id": "!legacy-channel:example.com", "channel_id": "legacy_channel"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &roomCreatorReadingTransport{
				recordingTransport: &recordingTransport{},
				creators:           map[string]string{tc.roomID: actualCreator},
			}
			service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
			setServiceOwnerForTest(service, actualCreator, "Actual Creator")
			if err := service.conversationModule.Save(ctx, dirextalkdomain.ConversationRecord{
				MatrixRoomID: tc.roomID, Kind: tc.kind, CreatedByMXID: staleCreator,
			}); err != nil {
				t.Fatal(err)
			}
			for _, member := range []memberRecord{
				{RoomID: tc.roomID, ChannelID: tc.channelID, UserID: staleCreator, Membership: "join", Role: "owner", JoinedAt: 10},
				{RoomID: tc.roomID, ChannelID: tc.channelID, UserID: "@alice:example.com", Membership: "join", Role: "member", JoinedAt: 20},
				{RoomID: tc.roomID, ChannelID: tc.channelID, UserID: actualCreator, Membership: "join", Role: "member", JoinedAt: 30},
			} {
				if err := service.store.UpsertMember(ctx, member); err != nil {
					t.Fatal(err)
				}
			}

			httpResult := mustHandle[map[string]any](t, service, tc.action, tc.params)
			httpMembers := httpResult["members"].([]memberRecord)
			assertRepairedMemberOwner(t, httpMembers[0].UserID, httpMembers[0].Role, httpMembers[1].UserID, httpMembers[1].Role, actualCreator, staleCreator)
			assertStoredConversationCreator(t, service, tc.roomID, actualCreator)

			if err := service.conversationModule.SetCreator(ctx, tc.roomID, staleCreator); err != nil {
				t.Fatal(err)
			}
			mcpResult := mustInvokeMCP[map[string]any](t, service, dirextalkmcp.ActionRoomMembersList, map[string]any{"room_id": tc.roomID})
			mcpMembers := mcpResult["members"].([]mcpMemberSummary)
			assertRepairedMemberOwner(t, mcpMembers[0].UserMXID, mcpMembers[0].Role, mcpMembers[1].UserMXID, mcpMembers[1].Role, actualCreator, staleCreator)
			assertStoredConversationCreator(t, service, tc.roomID, actualCreator)
			if got := len(transport.reads); got != 2 {
				t.Fatalf("creator reads = %d, want HTTP and MCP reads", got)
			}
		})
	}
}

func TestRoomOwnerReaderEmptyClearsStaleCreatorAndErrorPreservesIt(t *testing.T) {
	ctx := context.Background()
	const roomID = "!legacy-group:example.com"
	for _, tc := range []struct {
		name        string
		readerError error
		wantError   bool
		wantCreator string
	}{
		{name: "unresolved creator clears stale projection"},
		{name: "roomserver error preserves stale projection", readerError: errors.New("roomserver unavailable"), wantError: true, wantCreator: "@profile-writer:example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &roomCreatorReadingTransport{recordingTransport: &recordingTransport{}, err: tc.readerError}
			service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
			if err := service.conversationModule.Save(ctx, dirextalkdomain.ConversationRecord{
				MatrixRoomID: roomID, Kind: dirextalkdomain.ConversationKindGroup, CreatedByMXID: "@profile-writer:example.com",
			}); err != nil {
				t.Fatal(err)
			}
			for _, member := range []memberRecord{
				{RoomID: roomID, UserID: "@first:example.com", Membership: "join", Role: "member", JoinedAt: 10},
				{RoomID: roomID, UserID: "@profile-writer:example.com", Membership: "join", Role: "owner", JoinedAt: 20},
			} {
				if err := service.store.UpsertMember(ctx, member); err != nil {
					t.Fatal(err)
				}
			}

			result, actionErr := service.Handle(ctx, "groups.members", map[string]any{"room_id": roomID})
			if tc.wantError {
				if actionErr == nil || result != nil {
					t.Fatalf("expected owner read error, result=%#v err=%#v", result, actionErr)
				}
			} else {
				if actionErr != nil {
					t.Fatal(actionErr)
				}
				members := result.(map[string]any)["members"].([]memberRecord)
				if members[0].UserID != "@first:example.com" || members[0].Role != "member" {
					t.Fatalf("unresolved creator did not fall back to join order: %#v", members)
				}
			}
			assertStoredConversationCreator(t, service, roomID, tc.wantCreator)
		})
	}
}

func TestResolveRoomOwnerIgnoresNonGroupChannelConversation(t *testing.T) {
	ctx := context.Background()
	const roomID = "!direct:example.com"
	transport := &roomCreatorReadingTransport{
		recordingTransport: &recordingTransport{},
		creators:           map[string]string{roomID: "@actual-creator:example.com"},
	}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	if err := service.conversationModule.Save(ctx, dirextalkdomain.ConversationRecord{
		MatrixRoomID: roomID, Kind: dirextalkdomain.ConversationKindDirect, CreatedByMXID: "@legacy:example.com",
	}); err != nil {
		t.Fatal(err)
	}

	ownerMXID, err := service.resolveRoomOwner(ctx, roomID)
	if err != nil || ownerMXID != "" || len(transport.reads) != 0 {
		t.Fatalf("direct owner resolution = (%q, %v), reads=%#v", ownerMXID, err, transport.reads)
	}
	assertStoredConversationCreator(t, service, roomID, "@legacy:example.com")
}

func TestResolveRoomOwnerClearsStaleCreatorWithoutReader(t *testing.T) {
	ctx := context.Background()
	const roomID = "!group:example.com"
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, &recordingTransport{})
	if err := service.conversationModule.Save(ctx, dirextalkdomain.ConversationRecord{
		MatrixRoomID: roomID, Kind: dirextalkdomain.ConversationKindGroup, CreatedByMXID: "@legacy:example.com",
	}); err != nil {
		t.Fatal(err)
	}

	ownerMXID, err := service.resolveRoomOwner(ctx, roomID)
	if err != nil || ownerMXID != "" {
		t.Fatalf("owner resolution without reader = (%q, %v)", ownerMXID, err)
	}
	assertStoredConversationCreator(t, service, roomID, "")
}

type roomCreatorReadingTransport struct {
	*recordingTransport
	creators map[string]string
	err      error
	reads    []string
}

func (t *roomCreatorReadingTransport) ReadRoomCreator(_ context.Context, roomID string) (string, error) {
	t.reads = append(t.reads, roomID)
	if t.err != nil {
		return "", t.err
	}
	return t.creators[roomID], nil
}

func assertRepairedMemberOwner(t *testing.T, firstID, firstRole, secondID, secondRole, wantOwner, staleOwner string) {
	t.Helper()
	if firstID != wantOwner || firstRole != "owner" || secondID != staleOwner || secondRole != "member" {
		t.Fatalf("member owner repair = (%q, %q), (%q, %q), want actual owner then stale member", firstID, firstRole, secondID, secondRole)
	}
}

func assertStoredConversationCreator(t *testing.T, service *Service, roomID, want string) {
	t.Helper()
	record, ok, err := service.conversationModule.GetRecord(context.Background(), "", roomID)
	if err != nil || !ok || record.CreatedByMXID != want {
		t.Fatalf("stored creator = (%#v, %v, %v), want %q", record, ok, err, want)
	}
}

func TestChannelOwnerRoleSurvivesMatrixMemberProjection(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":       "ch",
		"room_id":          "!channel:example.com",
		"name":             "Channel",
		"channel_type":     "post",
		"comments_enabled": true,
	})
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:      ch.RoomID,
		ChannelID:   ch.ChannelID,
		UserID:      "@owner:example.com",
		DisplayName: "Owner",
		Domain:      "example.com",
		Membership:  "join",
		Role:        "member",
	}); err != nil {
		t.Fatal(err)
	}

	channels := mustHandle[map[string]any](t, service, "channels.list", nil)["channels"].([]channel)
	if len(channels) != 1 || !channels[0].IsOwned || channels[0].Role != "owner" {
		t.Fatalf("expected local channel owner role to survive Matrix projection, got %#v", channels)
	}
	got := mustHandle[conversationView](t, service, "conversations.get", map[string]any{"room_id": ch.RoomID})
	if got.Role != "owner" || !got.Capabilities.PostCreate {
		t.Fatalf("expected channel conversation to preserve owner post capability, got %#v", got)
	}
}

func TestStoredChannelOwnerRoleRepairAfterReload(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":       "stored_ch",
		"room_id":          "!stored-channel:example.com",
		"name":             "Stored Channel",
		"channel_type":     "post",
		"comments_enabled": true,
	})
	if upsertErr := store.UpsertMember(ctx, memberRecord{
		RoomID:      ch.RoomID,
		ChannelID:   ch.ChannelID,
		UserID:      "@owner:example.com",
		DisplayName: "Owner",
		Domain:      "example.com",
		Membership:  "join",
		Role:        "member",
	}); upsertErr != nil {
		t.Fatal(upsertErr)
	}

	reloaded, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, store)
	if err != nil {
		t.Fatal(err)
	}
	member, ok, err := store.LookupMember(ctx, ch.RoomID, "@owner:example.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || member.Role != "owner" {
		t.Fatalf("expected stored local channel owner role to be repaired, got ok=%v member=%#v", ok, member)
	}
	got := mustHandle[conversationView](t, reloaded, "conversations.get", map[string]any{"room_id": ch.RoomID})
	if got.Role != "owner" || !got.Capabilities.PostCreate {
		t.Fatalf("expected repaired channel conversation to preserve owner post capability, got %#v", got)
	}
}

func TestGroupAndChannelMemberLifecycle(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{"room_id": "!group:example.com", "name": "Group"})
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{"channel_id": "ch", "room_id": "!channel:example.com", "name": "Channel"})

	mustHandle[map[string]any](t, service, "groups.invite", map[string]any{
		"room_id":    group.RoomID,
		"user_id":    "@alice:example.com",
		"peer_mxids": []any{"@bob:example.com"},
	})
	groupMembers := mustHandle[map[string]any](t, service, "groups.members", map[string]any{"room_id": group.RoomID})
	groupList := groupMembers["members"].([]memberRecord)
	if len(groupList) != 3 {
		t.Fatalf("expected owner plus two invited group members, got %#v", groupMembers)
	}
	mustHandle[map[string]any](t, service, "groups.member.mute", map[string]any{"room_id": group.RoomID, "user_id": "@alice:example.com"})
	muted := mustHandle[map[string]any](t, service, "groups.members", map[string]any{"room_id": group.RoomID})["members"].([]memberRecord)
	if !findMember(muted, "@alice:example.com").Muted {
		t.Fatalf("expected alice muted, got %#v", muted)
	}
	mustHandle[map[string]any](t, service, "groups.member.remove", map[string]any{"room_id": group.RoomID, "user_id": "@alice:example.com"})
	afterRemove := mustHandle[map[string]any](t, service, "groups.members", map[string]any{"room_id": group.RoomID})["members"].([]memberRecord)
	if findMember(afterRemove, "@alice:example.com").UserID != "" {
		t.Fatalf("expected alice removed from joined member list, got %#v", afterRemove)
	}
	if _, apiErr := service.Handle(context.Background(), "groups.member.remove", map[string]any{"room_id": group.RoomID, "user_id": "@owner:example.com"}); apiErr == nil || apiErr.Status != 409 {
		t.Fatalf("expected group owner remove to return 409, got %#v", apiErr)
	}
	if _, apiErr := service.Handle(context.Background(), "groups.leave", map[string]any{"room_id": group.RoomID}); apiErr == nil || apiErr.Status != 409 {
		t.Fatalf("expected group owner leave to return 409, got %#v", apiErr)
	}
	afterLeave := mustHandle[map[string]any](t, service, "groups.members", map[string]any{"room_id": group.RoomID})["members"].([]memberRecord)
	if findMember(afterLeave, "@owner:example.com").UserID == "" {
		t.Fatalf("expected owner to remain after rejected leave, got %#v", afterLeave)
	}

	mustHandle[map[string]any](t, service, "channels.invite", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"user_ids":   []any{"@carol:example.com"},
	})
	channelMembers := mustHandle[map[string]any](t, service, "channels.members", map[string]any{"channel_id": ch.ChannelID})["members"].([]memberRecord)
	if findMember(channelMembers, "@carol:example.com").UserID == "" {
		t.Fatalf("expected invited channel member, got %#v", channelMembers)
	}
	mustHandle[map[string]any](t, service, "channels.member.mute", map[string]any{"channel_id": ch.ChannelID, "room_id": ch.RoomID, "user_id": "@carol:example.com"})
	channelMembers = mustHandle[map[string]any](t, service, "channels.members", map[string]any{"channel_id": ch.ChannelID})["members"].([]memberRecord)
	if !findMember(channelMembers, "@carol:example.com").Muted {
		t.Fatalf("expected carol muted, got %#v", channelMembers)
	}
	mustHandle[map[string]any](t, service, "channels.member.remove", map[string]any{"channel_id": ch.ChannelID, "room_id": ch.RoomID, "user_id": "@carol:example.com"})
	channelMembers = mustHandle[map[string]any](t, service, "channels.members", map[string]any{"channel_id": ch.ChannelID})["members"].([]memberRecord)
	if findMember(channelMembers, "@carol:example.com").UserID != "" {
		t.Fatalf("expected carol removed from joined channel members, got %#v", channelMembers)
	}
	if _, apiErr := service.Handle(context.Background(), "channels.member.remove", map[string]any{"channel_id": ch.ChannelID, "room_id": ch.RoomID, "user_id": "@owner:example.com"}); apiErr == nil || apiErr.Status != 409 {
		t.Fatalf("expected channel owner remove to return 409, got %#v", apiErr)
	}
	if _, apiErr := service.Handle(context.Background(), "channels.leave", map[string]any{"channel_id": ch.ChannelID, "room_id": ch.RoomID}); apiErr == nil || apiErr.Status != 409 {
		t.Fatalf("expected channel owner leave to return 409, got %#v", apiErr)
	}
	channelMembers = mustHandle[map[string]any](t, service, "channels.members", map[string]any{"channel_id": ch.ChannelID})["members"].([]memberRecord)
	if findMember(channelMembers, "@owner:example.com").UserID == "" {
		t.Fatalf("expected channel owner to remain after rejected leave, got %#v", channelMembers)
	}

	groupDissolve := mustHandle[map[string]any](t, service, "groups.dissolve", map[string]any{"room_id": group.RoomID})
	if groupDissolve["status"] != "ok" {
		t.Fatalf("expected group dissolve ok, got %#v", groupDissolve)
	}
	groupsAfterDissolve := mustHandle[map[string]any](t, service, "groups.list", nil)["groups"].([]groupRecord)
	if len(groupsAfterDissolve) != 0 {
		t.Fatalf("expected dissolved group removed from list, got %#v", groupsAfterDissolve)
	}

	channelDissolve := mustHandle[map[string]any](t, service, "channels.dissolve", map[string]any{"channel_id": ch.ChannelID, "room_id": ch.RoomID})
	if channelDissolve["status"] != "ok" {
		t.Fatalf("expected channel dissolve ok, got %#v", channelDissolve)
	}
	channelsAfterDissolve := mustHandle[map[string]any](t, service, "channels.list", nil)["channels"].([]channel)
	if len(channelsAfterDissolve) != 0 {
		t.Fatalf("expected dissolved channel removed from list, got %#v", channelsAfterDissolve)
	}
}

func TestGroupAndChannelWideMuteAndInvitePolicyActions(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{"room_id": "!group:example.com", "name": "Group"})
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{"channel_id": "ch", "room_id": "!channel:example.com", "name": "Channel"})

	mustHandle[map[string]any](t, service, "groups.invite", map[string]any{
		"room_id": group.RoomID,
		"user_id": "@alice:example.com",
	})
	mustHandle[map[string]any](t, service, "channels.invite", map[string]any{
		"channel_id": ch.ChannelID,
		"room_id":    ch.RoomID,
		"user_id":    "@carol:example.com",
	})

	groupMute := mustHandle[map[string]any](t, service, "groups.mute", map[string]any{"room_id": group.RoomID})
	if groupMute["muted"] != true {
		t.Fatalf("expected group mute response, got %#v", groupMute)
	}
	groupMembers := mustHandle[map[string]any](t, service, "groups.members", map[string]any{"room_id": group.RoomID})["members"].([]memberRecord)
	if findMember(groupMembers, "@owner:example.com").Muted || !findMember(groupMembers, "@alice:example.com").Muted {
		t.Fatalf("expected only ordinary group member muted, got %#v", groupMembers)
	}
	updatedPolicy := mustHandle[groupRecord](t, service, "groups.invite_policy.update", map[string]any{"room_id": group.RoomID, "invite_policy": "owner"})
	if !updatedPolicy.Muted || updatedPolicy.InvitePolicy != "owner" {
		t.Fatalf("expected muted group with updated invite policy, got %#v", updatedPolicy)
	}
	mustHandle[map[string]any](t, service, "groups.unmute", map[string]any{"room_id": group.RoomID})
	groupMembers = mustHandle[map[string]any](t, service, "groups.members", map[string]any{"room_id": group.RoomID})["members"].([]memberRecord)
	if findMember(groupMembers, "@alice:example.com").Muted {
		t.Fatalf("expected group unmute to clear ordinary member mute, got %#v", groupMembers)
	}

	channelMute := mustHandle[map[string]any](t, service, "channels.mute", map[string]any{"channel_id": ch.ChannelID, "room_id": ch.RoomID})
	if channelMute["muted"] != true {
		t.Fatalf("expected channel mute response, got %#v", channelMute)
	}
	channels := mustHandle[map[string]any](t, service, "channels.list", nil)["channels"].([]channel)
	if !channels[0].Muted {
		t.Fatalf("expected channel list to expose muted state, got %#v", channels)
	}
	channelMembers := mustHandle[map[string]any](t, service, "channels.members", map[string]any{"channel_id": ch.ChannelID})["members"].([]memberRecord)
	if findMember(channelMembers, "@owner:example.com").Muted || !findMember(channelMembers, "@carol:example.com").Muted {
		t.Fatalf("expected only ordinary channel member muted, got %#v", channelMembers)
	}
	mustHandle[map[string]any](t, service, "channels.unmute", map[string]any{"channel_id": ch.ChannelID, "room_id": ch.RoomID})
	channelMembers = mustHandle[map[string]any](t, service, "channels.members", map[string]any{"channel_id": ch.ChannelID})["members"].([]memberRecord)
	if findMember(channelMembers, "@carol:example.com").Muted {
		t.Fatalf("expected channel unmute to clear ordinary member mute, got %#v", channelMembers)
	}
}

func TestProductUpdatesPreserveExistingFieldsAndPublicGetDoesNotCreate(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id":       "!group:example.com",
		"name":          "Original Group",
		"topic":         "original topic",
		"avatar_url":    "mxc://old-group-avatar",
		"invite_policy": "member",
	})
	updatedGroup := mustHandle[groupRecord](t, service, "groups.update", map[string]any{
		"room_id":    group.RoomID,
		"avatar_url": "mxc://new-group-avatar",
	})
	if updatedGroup.Name != "Original Group" || updatedGroup.Topic != "original topic" || updatedGroup.InvitePolicy != "member" || updatedGroup.AvatarURL != "mxc://new-group-avatar" {
		t.Fatalf("expected partial group update to preserve existing fields, got %#v", updatedGroup)
	}

	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":       "news",
		"room_id":          "!news:example.com",
		"name":             "News",
		"description":      "original description",
		"visibility":       "private",
		"join_policy":      "approval",
		"channel_type":     "post",
		"comments_enabled": true,
	})
	updatedChannel := mustHandle[channel](t, service, "channels.update", map[string]any{
		"channel_id":  ch.ChannelID,
		"description": "new description",
	})
	if updatedChannel.Name != "News" || updatedChannel.Visibility != "private" || updatedChannel.JoinPolicy != "approval" || updatedChannel.ChannelType != "post" || updatedChannel.Description != "new description" {
		t.Fatalf("expected partial channel update to preserve existing fields, got %#v", updatedChannel)
	}

	if _, apiErr := service.Handle(context.Background(), "channels.public.get", map[string]any{"room_id": ch.RoomID}); apiErr == nil || apiErr.Status != 404 {
		t.Fatalf("expected private channel public get to return 404, got %#v", apiErr)
	}
	updatedChannel = mustHandle[channel](t, service, "channels.update", map[string]any{
		"channel_id": ch.ChannelID,
		"visibility": "public",
	})
	detail := mustHandle[channel](t, service, "channels.public.get", map[string]any{"room_id": updatedChannel.RoomID})
	if detail.ChannelID != ch.ChannelID || detail.Description != "new description" {
		t.Fatalf("expected public get to return public existing channel, got %#v", detail)
	}
	if _, apiErr := service.Handle(context.Background(), "channels.public.get", map[string]any{"room_id": "!missing:example.com"}); apiErr == nil || apiErr.Status != 404 {
		t.Fatalf("expected missing public channel to return 404, got %#v", apiErr)
	}
	channels := mustHandle[map[string]any](t, service, "channels.list", nil)["channels"].([]channel)
	if len(channels) != 1 {
		t.Fatalf("expected public get missing not to create a channel, got %#v", channels)
	}
	if !channels[0].IsOwned || channels[0].Role != "owner" || channels[0].MemberStatus != "join" {
		t.Fatalf("expected channel list to expose owner membership fields, got %#v", channels[0])
	}
	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	bootstrapChannels := bootstrap["channels"].([]channel)
	if len(bootstrapChannels) != 1 || !bootstrapChannels[0].IsOwned || bootstrapChannels[0].Role != "owner" || bootstrapChannels[0].MemberStatus != "join" {
		t.Fatalf("expected sync bootstrap to expose owner membership fields, got %#v", bootstrapChannels)
	}
}

func TestOnlyProductOwnersCanUpdateGroupAndChannelDetails(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id":    "!owner-group:example.com",
		"name":       "Original Group",
		"avatar_url": "mxc://example.com/group-old",
	})
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":  "owner-channel",
		"room_id":     "!owner-channel:example.com",
		"name":        "Original Channel",
		"description": "Original Description",
		"avatar_url":  "mxc://example.com/channel-old",
	})

	updatedGroup := mustHandle[groupRecord](t, service, "groups.update", map[string]any{
		"room_id":    group.RoomID,
		"name":       "Updated Group",
		"avatar_url": "mxc://example.com/group-new",
	})
	if updatedGroup.Name != "Updated Group" || updatedGroup.AvatarURL != "mxc://example.com/group-new" {
		t.Fatalf("owner group update = %#v", updatedGroup)
	}
	updatedChannel := mustHandle[channel](t, service, "channels.update", map[string]any{
		"channel_id":  ch.ChannelID,
		"name":        "Updated Channel",
		"description": "Updated Description",
		"avatar_url":  "mxc://example.com/channel-new",
	})
	if updatedChannel.Name != "Updated Channel" ||
		updatedChannel.Description != "Updated Description" ||
		updatedChannel.AvatarURL != "mxc://example.com/channel-new" {
		t.Fatalf("owner channel update = %#v", updatedChannel)
	}

	service.mu.Lock()
	service.ownerMXID = "@member:example.com"
	service.mu.Unlock()
	if _, apiErr := service.Handle(context.Background(), "groups.update", map[string]any{
		"room_id": group.RoomID,
		"name":    "Blocked Group",
	}); apiErr == nil || apiErr.Status != http.StatusForbidden {
		t.Fatalf("non-owner group update error = %#v, want 403", apiErr)
	}
	if _, apiErr := service.Handle(context.Background(), "channels.update", map[string]any{
		"channel_id":  ch.ChannelID,
		"description": "Blocked Description",
	}); apiErr == nil || apiErr.Status != http.StatusForbidden {
		t.Fatalf("non-owner channel update error = %#v, want 403", apiErr)
	}

	storedGroup, ok, err := service.groupByRoom(context.Background(), group.RoomID)
	if err != nil || !ok || storedGroup.Name != "Updated Group" {
		t.Fatalf("blocked group update changed durable state: group=%#v ok=%v err=%v", storedGroup, ok, err)
	}
	storedChannel, ok, err := service.channelByIDOrRoom(context.Background(), ch.ChannelID, "")
	if err != nil || !ok || storedChannel.Description != "Updated Description" {
		t.Fatalf("blocked channel update changed durable state: channel=%#v ok=%v err=%v", storedChannel, ok, err)
	}
}

func TestGroupAndChannelListsOnlyExposeJoinedOwnerMembership(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	joinedGroup := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!joined-group:example.com",
		"name":    "Joined group",
	})
	invitedGroup := groupRecord{RoomID: "!invited-group:example.com", Name: "Invited group"}
	if err := service.saveGroup(context.Background(), invitedGroup); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     invitedGroup.RoomID,
		UserID:     "@owner:example.com",
		Domain:     "example.com",
		Membership: "invite",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}
	joinedChannel := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "joined-channel",
		"room_id":    "!joined-channel:example.com",
		"name":       "Joined channel",
	})
	invitedChannel := channel{ChannelID: "invited-channel", RoomID: "!invited-channel:example.com", Name: "Invited channel"}
	if err := service.saveChannel(context.Background(), invitedChannel); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     invitedChannel.RoomID,
		ChannelID:  invitedChannel.ChannelID,
		UserID:     "@owner:example.com",
		Domain:     "example.com",
		Membership: "invite",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}

	groups := mustHandle[map[string]any](t, service, "groups.list", nil)["groups"].([]groupRecord)
	if len(groups) != 1 || groups[0].RoomID != joinedGroup.RoomID {
		t.Fatalf("expected only joined group in groups.list, got %#v", groups)
	}
	channels := mustHandle[map[string]any](t, service, "channels.list", nil)["channels"].([]channel)
	if len(channels) != 1 || channels[0].ChannelID != joinedChannel.ChannelID {
		t.Fatalf("expected only joined channel in channels.list, got %#v", channels)
	}
}

func TestGroupCardJoinRequiresRecordedInvite(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"name": "产品群",
	})

	_, apiErr := service.Handle(context.Background(), "groups.join", map[string]any{
		"room_id":         group.RoomID,
		"user_id":         "@alice:remote.example",
		"group_name":      group.Name,
		"invite_event_id": "$invite",
		"direct_room_id":  "!dm:remote.example",
	})
	if apiErr == nil || apiErr.Status != http.StatusGone || apiErr.Code != "request_expired" {
		t.Fatalf("expected stable expired card join error, got %#v", apiErr)
	}
}

func TestGroupInviteRejectUsesCurrentLocalUserAndHidesInvitation(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	group := groupRecord{
		RoomID:       "!remote-group:remote.example",
		Name:         "Remote Group",
		InvitePolicy: "member",
	}
	if err := service.saveGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:      group.RoomID,
		UserID:      "@owner:example.com",
		DisplayName: "Owner",
		Domain:      "example.com",
		Membership:  "invite",
		Role:        "member",
	}); err != nil {
		t.Fatal(err)
	}

	rejected := mustHandle[map[string]any](t, service, "groups.invite.reject", map[string]any{
		"room_id": group.RoomID,
	})

	if rejected["status"] != "rejected" {
		t.Fatalf("expected rejected status, got %#v", rejected)
	}
	member := rejected["member"].(memberRecord)
	if member.UserID != "@owner:example.com" || member.Membership != "reject" {
		t.Fatalf("expected current local user invite to become rejected, got %#v", member)
	}
	members := mustHandle[map[string]any](t, service, "groups.members", map[string]any{"room_id": group.RoomID})["members"].([]memberRecord)
	if findMember(members, "@owner:example.com").UserID != "" {
		t.Fatalf("expected rejected invite hidden from visible group members, got %#v", members)
	}
	groups := mustHandle[map[string]any](t, service, "groups.list", nil)["groups"].([]groupRecord)
	if len(groups) != 0 {
		t.Fatalf("expected rejected invite hidden from joined groups, got %#v", groups)
	}
}

func TestStoredGroupOwnerCannotLeaveOrRemoveAfterReload(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!stored-owner:example.com",
		"name":    "Stored Owner",
	})

	reloaded, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, apiErr := reloaded.Handle(ctx, "groups.leave", map[string]any{"room_id": group.RoomID}); apiErr == nil || apiErr.Status != 409 {
		t.Fatalf("expected stored owner leave to be rejected after reload, got %#v", apiErr)
	}
	if _, apiErr := reloaded.Handle(ctx, "groups.member.remove", map[string]any{
		"room_id": group.RoomID,
		"user_id": "@owner:example.com",
	}); apiErr == nil || apiErr.Status != 409 {
		t.Fatalf("expected stored owner removal to be rejected after reload, got %#v", apiErr)
	}
	members := mustHandle[map[string]any](t, reloaded, "groups.members", map[string]any{"room_id": group.RoomID})["members"].([]memberRecord)
	owner := findMember(members, "@owner:example.com")
	if owner.UserID == "" || owner.Membership != "join" || owner.Role != "owner" {
		t.Fatalf("expected stored owner to remain joined owner after rejected mutations, got %#v", members)
	}
}

func TestStoredMemberRolesAndMutesSurviveReload(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	service, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!stored-members:example.com",
		"name":    "Stored Members",
	})
	ch := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "stored-channel-members",
		"room_id":    "!stored-channel-members:example.com",
		"name":       "Stored Channel Members",
	})
	if err := service.saveMember(ctx, memberRecord{
		RoomID:      group.RoomID,
		UserID:      "@alice:remote.example",
		DisplayName: "Alice",
		Domain:      "remote.example",
		Membership:  "join",
		Role:        "member",
	}); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(ctx, memberRecord{
		RoomID:      ch.RoomID,
		ChannelID:   ch.ChannelID,
		UserID:      "@bob:remote.example",
		DisplayName: "Bob",
		Domain:      "remote.example",
		Membership:  "join",
		Role:        "member",
	}); err != nil {
		t.Fatal(err)
	}
	mustHandle[map[string]any](t, service, "groups.mute", map[string]any{"room_id": group.RoomID})
	mustHandle[map[string]any](t, service, "channels.mute", map[string]any{"channel_id": ch.ChannelID, "room_id": ch.RoomID})

	reloadedStore, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloadedStore.Close()
	reloaded, err := NewServiceWithStore(ctx, Config{ServerName: "example.com"}, reloadedStore)
	if err != nil {
		t.Fatal(err)
	}
	groupMembers := mustHandle[map[string]any](t, reloaded, "groups.members", map[string]any{"room_id": group.RoomID})["members"].([]memberRecord)
	if owner := findMember(groupMembers, "@owner:example.com"); owner.UserID == "" || owner.Role != "owner" || owner.Muted {
		t.Fatalf("expected group owner role and unmuted state to survive reload, got %#v", groupMembers)
	}
	if alice := findMember(groupMembers, "@alice:remote.example"); alice.UserID == "" || alice.Role != "member" || !alice.Muted {
		t.Fatalf("expected group member role and muted state to survive reload, got %#v", groupMembers)
	}
	channelMembers := mustHandle[map[string]any](t, reloaded, "channels.members", map[string]any{"channel_id": ch.ChannelID})["members"].([]memberRecord)
	if owner := findMember(channelMembers, "@owner:example.com"); owner.UserID == "" || owner.Role != "owner" || owner.Muted {
		t.Fatalf("expected channel owner role and unmuted state to survive reload, got %#v", channelMembers)
	}
	if bob := findMember(channelMembers, "@bob:remote.example"); bob.UserID == "" || bob.Role != "member" || !bob.Muted {
		t.Fatalf("expected channel member role and muted state to survive reload, got %#v", channelMembers)
	}
}

func TestKickedGroupMemberRequiresFreshInvite(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!moderated-group:example.com",
		"name":    "Moderated Group",
	})

	mustHandle[map[string]any](t, service, "groups.invite", map[string]any{
		"room_id":   group.RoomID,
		"user_mxid": "@kicked:remote.example",
	})
	mustHandle[map[string]any](t, service, "groups.member.remove", map[string]any{
		"room_id":   group.RoomID,
		"user_mxid": "@kicked:remote.example",
	})
	if _, apiErr := service.Handle(context.Background(), "groups.join", map[string]any{
		"room_id":   group.RoomID,
		"user_mxid": "@kicked:remote.example",
	}); apiErr == nil || apiErr.Status != 403 {
		t.Fatalf("expected kicked group member direct rejoin to be rejected, got %#v", apiErr)
	}
	if result, apiErr := service.Handle(context.Background(), "groups.join", map[string]any{
		"room_id":         group.RoomID,
		"user_mxid":       "@kicked:remote.example",
		"group_name":      group.Name,
		"invite_event_id": "$old-card-event",
		"direct_room_id":  "!direct:remote.example",
	}); result != nil || apiErr == nil || apiErr.Status != http.StatusGone || apiErr.Code != actionbase.RequestExpiredCode {
		t.Fatalf("old group card reauthorized removed member: result=%#v err=%#v", result, apiErr)
	}
	mustHandle[map[string]any](t, service, "groups.invite", map[string]any{
		"room_id": group.RoomID, "user_mxid": "@kicked:remote.example",
	})
	rejoined := mustHandle[map[string]any](t, service, "groups.join", map[string]any{
		"room_id": group.RoomID, "user_mxid": "@kicked:remote.example",
		"group_name": group.Name, "invite_event_id": "$direct-card-event",
		"direct_room_id": "!direct:remote.example",
	})
	rejoinedMember := rejoined["member"].(memberRecord)
	if rejoinedMember.UserID != "@kicked:remote.example" || rejoinedMember.Membership != "join" {
		t.Fatalf("expected fresh invite to let kicked group member rejoin, got %#v", rejoined)
	}

}

func TestGroupJoinReplacesInviteTimeWithConfirmedJoinTime(t *testing.T) {
	ctx := context.Background()
	service := NewService(Config{ServerName: "example.com"})
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!join-time:example.com", "name": "Join Time",
	})
	invited := memberRecord{
		RoomID: group.RoomID, UserID: "@member:example.com",
		Membership: "invite", Role: "member", JoinedAt: 1,
	}
	if err := service.store.UpsertMember(ctx, invited); err != nil {
		t.Fatal(err)
	}

	result := mustHandle[map[string]any](t, service, "groups.join", map[string]any{
		"room_id": group.RoomID, "user_mxid": invited.UserID,
	})
	joined := result["member"].(memberRecord)
	if joined.Membership != "join" || joined.JoinedAt <= invited.JoinedAt {
		t.Fatalf("confirmed group join retained invite timestamp: invited=%#v joined=%#v", invited, joined)
	}
}

func TestGroupMemberLeaveActionCanRejoin(t *testing.T) {
	service := NewService(Config{ServerName: "remote.example"})
	bootstrapService(t, service)
	group := groupRecord{
		RoomID:       "!moderated-group:example.com",
		Name:         "Moderated Group",
		MemberCount:  1,
		InvitePolicy: "member",
	}
	if err := service.saveGroup(context.Background(), group); err != nil {
		t.Fatal(err)
	}
	if err := service.saveMember(context.Background(), memberRecord{
		RoomID:     group.RoomID,
		UserID:     "@owner:remote.example",
		Domain:     "remote.example",
		Membership: "join",
		Role:       "member",
	}); err != nil {
		t.Fatal(err)
	}

	left := mustHandle[map[string]any](t, service, "groups.leave", map[string]any{
		"room_id": group.RoomID,
	})
	member := left["member"].(memberRecord)
	if member.UserID != "@owner:remote.example" || member.Membership != "leave" || member.Role != "member" {
		t.Fatalf("expected current member to leave through real action, got %#v", left)
	}
	joined := mustHandle[map[string]any](t, service, "groups.join", map[string]any{
		"room_id": group.RoomID,
	})
	member = joined["member"].(memberRecord)
	if joined["status"] != "ok" || member.UserID != "@owner:remote.example" || member.Membership != "join" {
		t.Fatalf("expected action-left group member to be able to rejoin, got %#v", joined)
	}
}

func TestGroupInviteAcceptsInviteArrayAlias(t *testing.T) {
	service := NewService(Config{ServerName: "im1.dirextalk.ai"})
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{"room_id": "!group:im1.dirextalk.ai", "name": "Group"})

	res, apiErr := service.Handle(context.Background(), "groups.invite", map[string]any{
		"room_id": group.RoomID,
		"invite":  []any{"@owner:dm1.dirextalk.ai"},
	})
	if apiErr != nil {
		t.Fatalf("expected invite array alias to be accepted, got %#v", apiErr)
	}
	invite := res.(map[string]any)
	members := invite["members"].([]memberRecord)
	if len(members) != 1 || members[0].UserID != "@owner:dm1.dirextalk.ai" || members[0].Membership != "invite" {
		t.Fatalf("expected invite array alias to create invited member, got %#v", invite)
	}
}

func TestMemberProfilesAndCountsPropagateAcrossGroupAndChannelJoins(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"room_id": "!group:example.com",
		"name":    "Group",
	})
	mustHandle[map[string]any](t, service, "groups.join", map[string]any{
		"room_id":    group.RoomID,
		"user_mxid":  "@alice:example.com",
		"avatar_url": "mxc://example.com/alice",
	})
	mustHandle[map[string]any](t, service, "groups.join", map[string]any{
		"room_id":    group.RoomID,
		"user_mxid":  "@bob:example.com",
		"avatar_url": "mxc://example.com/bob",
	})

	list := mustHandle[map[string]any](t, service, "groups.members", map[string]any{"room_id": group.RoomID})
	members := list["members"].([]memberRecord)
	if len(members) != 3 {
		t.Fatalf("expected owner plus two members, got %#v", members)
	}
	alice := findMember(members, "@alice:example.com")
	bob := findMember(members, "@bob:example.com")
	if alice.AvatarURL != "mxc://example.com/alice" || bob.AvatarURL != "mxc://example.com/bob" {
		t.Fatalf("expected member avatars to be preserved, got %#v", members)
	}
	if alice.JoinedAt == 0 || bob.JoinedAt == 0 {
		t.Fatalf("expected joined_at on every member, got %#v", members)
	}
	groups := mustHandle[map[string]any](t, service, "groups.list", nil)["groups"].([]groupRecord)
	if len(groups) != 1 || groups[0].RoomID != group.RoomID || groups[0].MemberCount != 3 {
		t.Fatalf("expected group member_count to match joined members, got %#v", groups)
	}

	createdChannel := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id": "ch_members",
		"room_id":    "!channel:example.com",
		"name":       "Members",
	})
	mustHandle[map[string]any](t, service, "channels.join", map[string]any{
		"channel_id":   createdChannel.ChannelID,
		"user_mxid":    "@carol:example.com",
		"display_name": "Carol",
		"avatar_url":   "mxc://example.com/carol",
	})
	channelMembersList := mustHandle[map[string]any](t, service, "channels.members", map[string]any{"channel_id": createdChannel.ChannelID})
	channelMembers := channelMembersList["members"].([]memberRecord)
	if len(channelMembers) != 2 {
		t.Fatalf("expected channel owner and joined member, got %#v", channelMembers)
	}
	carol := findMember(channelMembers, "@carol:example.com")
	if carol.DisplayName != "Carol" || carol.AvatarURL != "mxc://example.com/carol" || carol.Membership != "join" {
		t.Fatalf("expected channel member profile and status, got %#v", carol)
	}
}

func TestGroupJoinCreatesLocalGroupRecord(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	joined := mustHandle[map[string]any](t, service, "groups.join", map[string]any{
		"room_id":    "!remote:remote.example",
		"group_name": "Remote Group",
	})
	member := joined["member"].(memberRecord)
	if member.RoomID != "!remote:remote.example" || member.Membership != "join" {
		t.Fatalf("expected local joined member for remote group, got %#v", joined)
	}

	list := mustHandle[map[string]any](t, service, "groups.list", nil)
	groups := list["groups"].([]groupRecord)
	if len(groups) != 1 {
		t.Fatalf("expected joined remote group in groups.list, got %#v", list)
	}
	if groups[0].RoomID != "!remote:remote.example" || groups[0].Name != "Remote Group" {
		t.Fatalf("expected joined remote group summary, got %#v", groups[0])
	}
}

func TestGroupCardJoinConsumesRecordedInvite(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	bootstrapService(t, service)
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"name": "产品群",
	})
	mustHandle[map[string]any](t, service, "groups.invite", map[string]any{
		"room_id": group.RoomID,
		"user_id": "@alice:remote.example",
	})

	result := mustHandle[map[string]any](t, service, "groups.join", map[string]any{
		"room_id":         group.RoomID,
		"user_id":         "@alice:remote.example",
		"group_name":      group.Name,
		"invite_event_id": "$invite",
		"direct_room_id":  "!dm:remote.example",
	})

	member := result["member"].(memberRecord)
	if member.UserID != "@alice:remote.example" || member.Membership != "join" {
		t.Fatalf("expected invited user to join, got %#v", member)
	}
	stored, ok, err := service.lookupMember(context.Background(), group.RoomID, "@alice:remote.example")
	if err != nil || !ok || stored.Membership != "join" {
		t.Fatalf("expected stored joined member, got member=%#v ok=%v err=%v", stored, ok, err)
	}
	if result["room_id"] != group.RoomID {
		t.Fatalf("expected join response to include room_id %s, got %#v", group.RoomID, result)
	}
}
