package p2p

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/types"
	"github.com/YingSuiAI/dirextalk-message-server/test"
	"github.com/matrix-org/gomatrixserverlib"
)

func TestProductRoomsUseNativeRoomTypeAndUnifiedProfile(t *testing.T) {
	transport := &recordingTransport{}
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	mustHandle[ownerProfile](t, service, "profile.update", map[string]any{
		"display_name": "Owner",
		"avatar_url":   "mxc://example.com/owner-avatar",
	})

	transport.roomID = "!native-direct:example.com"
	mustHandle[contactRecord](t, service, "contacts.request", map[string]any{
		"mxid":         "@alice:remote.example",
		"display_name": "Alice",
	})
	transport.roomID = "!native-group:example.com"
	group := mustHandle[groupRecord](t, service, "groups.create", map[string]any{
		"name":          "Native Group",
		"topic":         "Group topic",
		"invite_policy": "owner",
	})
	transport.roomID = "!native-channel:example.com"
	createdChannel := mustHandle[channel](t, service, "channels.create", map[string]any{
		"channel_id":       "native-channel",
		"name":             "Native Channel",
		"description":      "Channel description",
		"visibility":       "public",
		"join_policy":      "approval",
		"channel_type":     "post",
		"comments_enabled": false,
	})

	if len(transport.createRooms) != 3 {
		t.Fatalf("expected direct, group, and channel create requests, got %#v", transport.createRooms)
	}
	if transport.createRooms[0].RoomType != DirextalkRoomTypeDirect {
		t.Fatalf("direct room type = %q, want %q", transport.createRooms[0].RoomType, DirextalkRoomTypeDirect)
	}
	if transport.createRooms[1].RoomType != DirextalkRoomTypeGroup {
		t.Fatalf("group room type = %q, want %q", transport.createRooms[1].RoomType, DirextalkRoomTypeGroup)
	}
	if transport.createRooms[2].RoomType != DirextalkRoomTypeChannel {
		t.Fatalf("channel room type = %q, want %q", transport.createRooms[2].RoomType, DirextalkRoomTypeChannel)
	}
	for _, req := range transport.createRooms {
		if req.CreatorAvatarURL != "mxc://example.com/owner-avatar" {
			t.Fatalf("expected creator avatar on %s create request, got %#v", req.RoomType, req)
		}
	}
	assertInitialProfile(t, transport.createRooms[0], DirextalkRoomTypeDirect, map[string]any{
		"name":           "Alice",
		"visibility":     "private",
		"join_policy":    "invite",
		"requester_mxid": "@owner:example.com",
		"target_mxid":    "@alice:remote.example",
		"display_name":   "Owner",
		"avatar_url":     "mxc://example.com/owner-avatar",
		"domain":         "example.com",
	})
	assertInitialProfile(t, transport.createRooms[1], DirextalkRoomTypeGroup, map[string]any{
		"name":          group.Name,
		"topic":         group.Topic,
		"invite_policy": "owner",
	})
	assertInitialProfile(t, transport.createRooms[2], DirextalkRoomTypeChannel, map[string]any{
		"channel_id":       createdChannel.ChannelID,
		"name":             createdChannel.Name,
		"description":      createdChannel.Description,
		"visibility":       "public",
		"join_policy":      "approval",
		"channel_type":     "post",
		"comments_enabled": false,
	})
}

func TestProjectUnifiedRoomProfileForChannelAndGroup(t *testing.T) {
	user := test.NewUser(t)
	channelRoom := test.NewRoom(t, user)
	groupRoom := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})

	channelProfile := channelRoom.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"room_type":        DirextalkRoomTypeChannel,
		"channel_id":       "unified-channel",
		"name":             "Unified Channel",
		"description":      "Profile metadata",
		"visibility":       "public",
		"join_policy":      "approval",
		"channel_type":     "post",
		"comments_enabled": false,
	}, test.WithStateKey(""))
	if err := service.ProjectRoomEvent(context.Background(), channelProfile); err != nil {
		t.Fatal(err)
	}
	channels, err := service.listChannels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || channels[0].ChannelID != "unified-channel" || channels[0].RoomID != channelRoom.ID || channels[0].JoinPolicy != "approval" || channels[0].CommentsEnabled {
		t.Fatalf("expected unified channel projection, got %#v", channels)
	}

	groupProfile := groupRoom.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"room_type":     DirextalkRoomTypeGroup,
		"name":          "Unified Group",
		"topic":         "Group profile",
		"invite_policy": "owner",
	}, test.WithStateKey(""))
	if projectErr := service.ProjectRoomEvent(context.Background(), groupProfile); projectErr != nil {
		t.Fatal(projectErr)
	}
	groups, err := service.listGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].RoomID != groupRoom.ID || groups[0].Name != "Unified Group" || groups[0].InvitePolicy != "owner" {
		t.Fatalf("expected unified group projection, got %#v", groups)
	}
}

func TestProjectJoinRequestStateToMemberProjection(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	owner := test.NewUser(t)
	requester := test.NewUser(t)
	room := test.NewRoom(t, owner)
	pending := trustedStateEvent(t, room.ID, owner.ID, DirextalkJoinRequestEventType, requester.ID, map[string]any{
		"status":  "pending",
		"user_id": requester.ID,
	})
	if err := service.ProjectRoomEvent(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	members := mustHandle[map[string]any](t, service, "channels.members", map[string]any{"room_id": room.ID})["members"].([]memberRecord)
	member := findMember(members, requester.ID)
	if member.Membership != "pending" {
		t.Fatalf("expected pending join request projection, got %#v", members)
	}

	approved := trustedStateEvent(t, room.ID, owner.ID, DirextalkJoinRequestEventType, requester.ID, map[string]any{
		"status":  "approved",
		"user_id": requester.ID,
	})
	if err := service.ProjectRoomEvent(context.Background(), approved); err != nil {
		t.Fatal(err)
	}
	members = mustHandle[map[string]any](t, service, "channels.members", map[string]any{"room_id": room.ID})["members"].([]memberRecord)
	member = findMember(members, requester.ID)
	if member.Membership != "invite" {
		t.Fatalf("expected approved join request to project as invite until Matrix join, got %#v", members)
	}

	rejected := trustedStateEvent(t, room.ID, owner.ID, DirextalkJoinRequestEventType, requester.ID, map[string]any{
		"status":  "rejected",
		"user_id": requester.ID,
	})
	if err := service.ProjectRoomEvent(context.Background(), rejected); err != nil {
		t.Fatal(err)
	}
	member, ok, err := service.lookupMember(context.Background(), room.ID, requester.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("expected rejected join request member record")
	}
	if member.Membership != "reject" {
		t.Fatalf("expected rejected join request projection, got %#v", member)
	}
}

func TestProjectJoinRequestStateFencesDelayedGeneration(t *testing.T) {
	for _, tc := range []struct {
		name              string
		initialMembership string
		status            string
		requestID         string
		membership        string
	}{
		{name: "delayed approved", status: "approved", requestID: "request-a", membership: "pending"},
		{name: "delayed rejected", status: "rejected", requestID: "request-a", membership: "pending"},
		{name: "delayed invite", initialMembership: "invite", status: "approved", requestID: "request-a", membership: "invite"},
		{name: "markerless invite", initialMembership: "invite", status: "rejected", membership: "invite"},
		{name: "current approved", status: "approved", requestID: "request-b", membership: "invite"},
		{name: "current rejected", status: "rejected", requestID: "request-b", membership: "reject"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService(Config{ServerName: "example.com"})
			owner := test.NewUser(t)
			requester := test.NewUser(t)
			room := test.NewRoom(t, owner)
			initialMembership := tc.initialMembership
			if initialMembership == "" {
				initialMembership = "pending"
			}
			if err := service.saveMember(context.Background(), memberRecord{
				RoomID: room.ID, ChannelID: "channel-1", UserID: requester.ID,
				Membership: initialMembership, Role: "member", RequestID: "request-b",
			}); err != nil {
				t.Fatal(err)
			}

			event := trustedStateEvent(t, room.ID, owner.ID, DirextalkJoinRequestEventType, requester.ID, map[string]any{
				"status": tc.status, "user_id": requester.ID, "channel_id": "channel-1", "request_id": tc.requestID,
			})
			if err := service.ProjectRoomEvent(context.Background(), event); err != nil {
				t.Fatal(err)
			}
			member, found, err := service.lookupMember(context.Background(), room.ID, requester.ID)
			if err != nil || !found {
				t.Fatalf("projected member = (%#v, %v, %v)", member, found, err)
			}
			if member.Membership != tc.membership || member.RequestID != "request-b" {
				t.Fatalf("join request projection = %#v, want membership=%q request_id=request-b", member, tc.membership)
			}
		})
	}
}

type joinRequestProjectionInsertRaceStore struct {
	*p2pstorage.MemoryStore
	roomID      string
	userID      string
	replacement memberRecord
	armed       bool
}

func (s *joinRequestProjectionInsertRaceStore) LookupMember(ctx context.Context, roomID, userID string) (memberRecord, bool, error) {
	member, found, err := s.MemoryStore.LookupMember(ctx, roomID, userID)
	if err != nil || !s.armed || roomID != s.roomID || userID != s.userID || found {
		return member, found, err
	}
	s.armed = false
	if err := s.MemoryStore.UpsertMember(ctx, s.replacement); err != nil {
		return memberRecord{}, false, err
	}
	return memberRecord{}, false, nil
}

func TestProjectJoinRequestStateInsertDoesNotOverwriteConcurrentGeneration(t *testing.T) {
	store := &joinRequestProjectionInsertRaceStore{MemoryStore: p2pstorage.NewMemoryStore()}
	service, err := NewServiceWithStore(context.Background(), withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	owner := test.NewUser(t)
	requester := test.NewUser(t)
	room := test.NewRoom(t, owner)
	store.roomID = room.ID
	store.userID = requester.ID
	store.replacement = memberRecord{
		RoomID: room.ID, ChannelID: "channel-1", UserID: requester.ID,
		Membership: "pending", Role: "member", RequestID: "request-b",
	}
	store.armed = true

	event := trustedStateEvent(t, room.ID, owner.ID, DirextalkJoinRequestEventType, requester.ID, map[string]any{
		"status": "approved", "user_id": requester.ID, "channel_id": "channel-1", "request_id": "request-a",
	})
	if err := service.ProjectRoomEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	member, found, err := service.lookupMember(context.Background(), room.ID, requester.ID)
	if err != nil || !found || member.Membership != "pending" || member.RequestID != "request-b" {
		t.Fatalf("concurrent generation was overwritten: member=%#v found=%v err=%v", member, found, err)
	}
}

func TestProjectJoinRequestStateFallsBackToNormalizedUserStateKey(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	owner := test.NewUser(t)
	requester := test.NewUser(t)
	room := test.NewRoom(t, owner)
	event := trustedStateEvent(t, room.ID, owner.ID, DirextalkJoinRequestEventType, productpolicy.UserStateKey(requester.ID), map[string]any{
		"status": "pending",
	})

	if err := service.ProjectRoomEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	member, ok, err := service.lookupMember(context.Background(), room.ID, requester.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || member.UserID != requester.ID || member.Membership != "pending" {
		t.Fatalf("expected join request projection for normalized state key user, got ok=%v member=%#v", ok, member)
	}
}

func trustedStateEvent(t *testing.T, roomID, sender, eventType, stateKey string, content map[string]any) *types.HeaderedEvent {
	t.Helper()
	rawContent, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"type":%q,"state_key":%q,"room_id":%q,"sender":%q,"content":%s}`, eventType, stateKey, roomID, sender, rawContent)
	verImpl, err := gomatrixserverlib.GetRoomVersion(gomatrixserverlib.RoomVersionV10)
	if err != nil {
		t.Fatal(err)
	}
	pdu, err := verImpl.NewEventFromTrustedJSON([]byte(raw), false)
	if err != nil {
		t.Fatal(err)
	}
	return &types.HeaderedEvent{PDU: pdu}
}

func assertInitialProfile(t *testing.T, req CreateRoomRequest, roomType string, expected map[string]any) {
	t.Helper()
	for _, state := range req.InitialState {
		if state.Type != DirextalkRoomProfileEventType {
			continue
		}
		if state.StateKey != "" {
			t.Fatalf("profile state key = %q, want empty", state.StateKey)
		}
		if state.Content["room_type"] != roomType {
			t.Fatalf("profile room_type = %#v, want %q", state.Content["room_type"], roomType)
		}
		for key, want := range expected {
			gotBytes, _ := json.Marshal(state.Content[key])
			wantBytes, _ := json.Marshal(want)
			if string(gotBytes) != string(wantBytes) {
				t.Fatalf("profile %s = %s, want %s in %#v", key, gotBytes, wantBytes, state.Content)
			}
		}
		return
	}
	t.Fatalf("missing %s initial state in %#v", DirextalkRoomProfileEventType, req.InitialState)
}
