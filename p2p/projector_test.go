package p2p

import (
	"context"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	roomserverAPI "github.com/YingSuiAI/dirextalk-message-server/roomserver/api"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/types"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestProjectRoomMessageDoesNotCreateP2PMessageRecord(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	event := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype": "m.text",
		"body":    "remote hello",
	})
	service := NewService(Config{ServerName: "test"})
	if err := service.saveGroup(context.Background(), groupRecord{RoomID: room.ID, Name: "Known Product Room"}); err != nil {
		t.Fatal(err)
	}

	if err := service.ProjectRoomEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	p2pEvents := mustListP2PEvents(t, service)
	if len(p2pEvents) != 0 {
		t.Fatalf("ordinary Matrix message must not produce P2P events, got %#v", p2pEvents)
	}
}

func TestProjectRoomMessageIgnoresUnknownNonProductRoom(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	event := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype": "m.text",
		"body":    "regular Matrix room",
	})
	service := NewService(Config{ServerName: "test"})

	if err := service.ProjectRoomEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	p2pEvents := mustListP2PEvents(t, service)
	if len(p2pEvents) != 0 {
		t.Fatalf("non-product Matrix room message must not produce P2P events, got %#v", p2pEvents)
	}
}

func TestProjectAgentRoomMessageDoesNotAppendGatewayEvent(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	event := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype": "m.text",
		"body":    "hello agent",
	})
	service := NewService(Config{ServerName: "test"})
	service.agentRoomID = room.ID

	if err := service.ProjectRoomEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	p2pEvents := mustListP2PEvents(t, service)
	if len(p2pEvents) != 0 {
		t.Fatalf("agent room Matrix messages must not produce P2P events, got %#v", p2pEvents)
	}
}

func TestProjectAgentRoomMessageIgnoresGatewayMarkedReply(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	event := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype":                    "m.text",
		"body":                       "agent reply",
		AgentGatewayContentKey:       true,
		AgentGatewaySourceContentKey: "codex-cli",
	})
	service := NewService(Config{ServerName: "test"})
	service.agentRoomID = room.ID

	if err := service.ProjectRoomEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	p2pEvents := mustListP2PEvents(t, service)
	if len(p2pEvents) != 0 {
		t.Fatalf("gateway-marked replies must not loop through P2P events, got %#v", p2pEvents)
	}
}

func TestProjectRoomMessageUpdatesConversationActivity(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	event := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype": "m.text",
		"body":    "hello from product room",
	})
	service := NewService(Config{ServerName: "test"})
	if err := service.saveContact(context.Background(), contactRecord{
		RoomID:      room.ID,
		PeerMXID:    "@peer:example.com",
		DisplayName: "Peer",
		Status:      "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	if err := service.ProjectRoomEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}

	got, ok, err := service.getConversation(context.Background(), "", room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected projected conversation")
	}
	if got.LastEventID != event.EventID() ||
		got.LastActivityAt != int64(event.OriginServerTS()) ||
		got.LastMessage != "hello from product room" {
		t.Fatalf("conversation activity was not projected: %#v", got)
	}
	view, err := service.conversationView(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	if view.LastMessage != "hello from product room" {
		t.Fatalf("conversation view did not include last message: %#v", view)
	}
}

func TestProjectRoomProfileCreatesConversation(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})
	event := room.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"room_type": DirextalkRoomTypeGroup,
		"name":      "Launch Group",
	}, test.WithStateKey(""))

	if err := service.ProjectRoomEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	got, ok, err := service.getConversation(context.Background(), "", room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || got.Kind != conversationKindGroup || got.Title != "Launch Group" {
		t.Fatalf("expected projected group conversation, got %#v ok=%v", got, ok)
	}
}

func TestProjectRoomProfileNeverClaimsConversationCreator(t *testing.T) {
	for _, tc := range []struct {
		name     string
		roomID   string
		roomType string
		profile  map[string]any
	}{
		{
			name:     "group",
			roomID:   "!profile-group:example.com",
			roomType: DirextalkRoomTypeGroup,
			profile:  map[string]any{"room_type": DirextalkRoomTypeGroup, "name": "Group"},
		},
		{
			name:     "channel",
			roomID:   "!profile-channel:example.com",
			roomType: DirextalkRoomTypeChannel,
			profile:  map[string]any{"room_type": DirextalkRoomTypeChannel, "channel_id": "profile_channel", "name": "Channel"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := NewService(Config{ServerName: "example.com"})
			profile := trustedStateEvent(t, tc.roomID, "@profile-writer:example.com", DirextalkRoomProfileEventType, "", tc.profile)
			if err := service.ProjectRoomEvent(context.Background(), profile); err != nil {
				t.Fatal(err)
			}
			record, ok, err := service.getConversation(context.Background(), "", tc.roomID)
			if err != nil || !ok {
				t.Fatalf("projected conversation = (%#v, %v, %v)", record, ok, err)
			}
			if record.CreatedByMXID != "" {
				t.Fatalf("%s profile sender was treated as creator: %#v", tc.roomType, record)
			}
		})
	}
}

func TestProjectRoomCreatePersistsExactCreatorBeforeProfileUpdates(t *testing.T) {
	for _, tc := range []struct {
		name     string
		roomID   string
		roomType string
		kind     dirextalkdomain.ConversationKind
		profile  map[string]any
	}{
		{
			name:     "group",
			roomID:   "!created-group:example.com",
			roomType: DirextalkRoomTypeGroup,
			kind:     dirextalkdomain.ConversationKindGroup,
			profile:  map[string]any{"room_type": DirextalkRoomTypeGroup, "name": "Group"},
		},
		{
			name:     "channel",
			roomID:   "!created-channel:example.com",
			roomType: DirextalkRoomTypeChannel,
			kind:     dirextalkdomain.ConversationKindChannel,
			profile:  map[string]any{"room_type": DirextalkRoomTypeChannel, "channel_id": "created_channel", "name": "Channel"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			service := NewService(Config{ServerName: "example.com"})
			create := trustedStateEvent(t, tc.roomID, "@actual-creator:remote.example", "m.room.create", "", map[string]any{
				"creator": "@spoofed:example.com",
				"type":    tc.roomType,
			})
			if err := service.ProjectRoomEvent(ctx, create); err != nil {
				t.Fatal(err)
			}
			record, ok, err := service.getConversation(ctx, "", tc.roomID)
			if err != nil || !ok {
				t.Fatalf("create projection = (%#v, %v, %v)", record, ok, err)
			}
			if record.Kind != tc.kind || record.CreatedByMXID != "@actual-creator:remote.example" || record.ProjectionState != dirextalkdomain.ConversationProjectionPending {
				t.Fatalf("unexpected create projection: %#v", record)
			}

			profile := trustedStateEvent(t, tc.roomID, "@later-profile-writer:example.com", DirextalkRoomProfileEventType, "", tc.profile)
			if err := service.ProjectRoomEvent(ctx, profile); err != nil {
				t.Fatal(err)
			}
			record, ok, err = service.getConversation(ctx, "", tc.roomID)
			if err != nil || !ok || record.CreatedByMXID != "@actual-creator:remote.example" || record.ProjectionState != dirextalkdomain.ConversationProjectionReady {
				t.Fatalf("profile update changed authoritative creator: (%#v, %v, %v)", record, ok, err)
			}
		})
	}
}

func TestProjectRoomCreateLeavesUnresolvableCreatorEmpty(t *testing.T) {
	ctx := context.Background()
	service := NewService(Config{ServerName: "example.com"})
	event := trustedStateEvent(t, "!pseudonymous-group:example.com", "$pseudonymous-sender", "m.room.create", "", map[string]any{
		"type":    DirextalkRoomTypeGroup,
		"creator": "@spoofed:example.com",
	})
	if err := service.ProjectRoomEvent(ctx, event); err != nil {
		t.Fatal(err)
	}
	record, ok, err := service.getConversation(ctx, "", "!pseudonymous-group:example.com")
	if err != nil || !ok {
		t.Fatalf("create projection = (%#v, %v, %v)", record, ok, err)
	}
	if record.CreatedByMXID != "" {
		t.Fatalf("unresolvable create sender became creator: %#v", record)
	}
}

func TestProjectRoomProfileRequiresExplicitRoomType(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})
	event := room.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"invite_policy": "member",
		"topic":         "must not imply group",
	}, test.WithStateKey(""))

	if err := service.ProjectRoomEvent(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := service.getConversation(context.Background(), "", room.ID); err != nil || ok {
		t.Fatalf("expected no conversation without explicit room_type, ok=%v err=%v", ok, err)
	}
}

func TestProjectChannelStateAndPostKinds(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})

	state := room.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"room_type":    DirextalkRoomTypeChannel,
		"channel_id":   "ch_remote",
		"channel_type": "post",
		"name":         "Remote Posts",
		"visibility":   "public",
	}, test.WithStateKey(""))
	if err := service.ProjectRoomEvent(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	gotChannels, err := service.listChannels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gotChannels) != 1 || gotChannels[0].ChannelID != "ch_remote" || gotChannels[0].RoomID != room.ID {
		t.Fatalf("expected projected channel state, got %#v", gotChannels)
	}

	post := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype":    "m.text",
		"body":       "projected post",
		"p2p_kind":   "channel_post",
		"channel_id": "ch_remote",
		"post_id":    "post_remote",
		"visibility": "public",
	})
	if projectErr := service.ProjectRoomEvent(context.Background(), post); projectErr != nil {
		t.Fatal(projectErr)
	}
	posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": "ch_remote"})
	gotPosts, ok := posts["posts"].([]channelPostRecord)
	if !ok || len(gotPosts) != 1 || gotPosts[0].PostID != "post_remote" || gotPosts[0].EventID != post.EventID() || gotPosts[0].Visibility != "public" {
		t.Fatalf("expected projected channel post, got %#v", posts)
	}
	publicPosts := mustHandle[map[string]any](t, service, "channels.public.posts.list", map[string]any{
		"channel_id": "ch_remote",
		"room_id":    room.ID,
	})["posts"].([]channelPostRecord)
	if len(publicPosts) != 1 || publicPosts[0].PostID != "post_remote" {
		t.Fatalf("expected projected public post in filtered list, got %#v", publicPosts)
	}
	conversation, ok, err := service.getConversation(context.Background(), "", room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected projected channel conversation")
	}
	if conversation.LastEventID == post.EventID() || conversation.LastMessage == "projected post" {
		t.Fatalf("channel post must not update ordinary chat activity, got %#v", conversation)
	}

	comment := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype":    "m.text",
		"body":       "projected comment",
		"p2p_kind":   "channel_comment",
		"channel_id": "ch_remote",
		"post_id":    "post_remote",
		"comment_id": "comment_remote",
	})
	if projectErr := service.ProjectRoomEvent(context.Background(), comment); projectErr != nil {
		t.Fatal(projectErr)
	}
	comments := mustHandle[map[string]any](t, service, "channels.comments.list", map[string]any{"post_id": "post_remote"})
	gotComments, ok := comments["comments"].([]channelCommentRecord)
	if !ok || len(gotComments) != 1 || gotComments[0].CommentID != "comment_remote" || gotComments[0].EventID != comment.EventID() {
		t.Fatalf("expected projected channel comment, got %#v", comments)
	}
	conversation, ok, err = service.getConversation(context.Background(), "", room.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected projected channel conversation")
	}
	if conversation.LastEventID == comment.EventID() || conversation.LastMessage == "projected comment" {
		t.Fatalf("channel comment must not update ordinary chat activity, got %#v", conversation)
	}

	dissolved := room.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"room_type":    DirextalkRoomTypeChannel,
		"channel_id":   "ch_remote",
		"channel_type": "post",
		"name":         "Remote Posts",
		"dissolved":    true,
	}, test.WithStateKey(""))
	if projectErr := service.ProjectRoomEvent(context.Background(), dissolved); projectErr != nil {
		t.Fatal(projectErr)
	}
	gotChannels, err = service.listChannels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gotChannels) != 0 {
		t.Fatalf("expected dissolved channel state to remove channel, got %#v", gotChannels)
	}
}

func TestProjectGroupStateAndDissolve(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})
	state := room.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"room_type":     DirextalkRoomTypeGroup,
		"name":          "Remote Group",
		"topic":         "Topic",
		"avatar_url":    "mxc://test/group",
		"invite_policy": "owner",
	}, test.WithStateKey(""))
	if err := service.ProjectRoomEvent(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	groups, err := service.listGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 1 || groups[0].RoomID != room.ID || groups[0].Name != "Remote Group" || groups[0].InvitePolicy != "owner" {
		t.Fatalf("expected projected group state, got %#v", groups)
	}

	dissolved := room.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"room_type": DirextalkRoomTypeGroup,
		"name":      "Remote Group",
		"dissolved": true,
	}, test.WithStateKey(""))
	if projectErr := service.ProjectRoomEvent(context.Background(), dissolved); projectErr != nil {
		t.Fatal(projectErr)
	}
	groups, err = service.listGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("expected dissolved group state to remove group, got %#v", groups)
	}
}

func TestProjectSparseChannelProfilePreservesLocalMute(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})
	if err := service.saveChannel(context.Background(), channel{
		ChannelID:       "ch_sparse",
		RoomID:          room.ID,
		Name:            "Existing Channel",
		AvatarURL:       "mxc://test/channel",
		Visibility:      "public",
		JoinPolicy:      "approval",
		ChannelType:     "chat",
		CommentsEnabled: true,
		Muted:           true,
	}); err != nil {
		t.Fatal(err)
	}

	state := room.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"room_type":  DirextalkRoomTypeChannel,
		"channel_id": "ch_sparse",
		"name":       "Renamed Channel",
	}, test.WithStateKey(""))
	if err := service.ProjectRoomEvent(context.Background(), state); err != nil {
		t.Fatal(err)
	}

	channels, err := service.listChannels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(channels) != 1 || !channels[0].Muted || channels[0].AvatarURL != "mxc://test/channel" {
		t.Fatalf("expected sparse channel profile to preserve local fields, got %#v", channels)
	}
}

func TestProjectLegacyProductStateIsIgnored(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})

	legacy := room.CreateAndInsert(t, user, "p2p.room.kind", map[string]any{
		"channel_id": "legacy_channel",
		"name":       "Legacy Channel",
	}, test.WithStateKey(""))
	if err := service.ProjectRoomEvent(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}
	gotChannels, err := service.listChannels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(gotChannels) != 0 {
		t.Fatalf("expected legacy product state to be ignored, got %#v", gotChannels)
	}
}

func TestProjectNativeDirectProfileStateDoesNotCreateGroup(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, owner)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID
	if err := service.saveGroup(context.Background(), groupRecord{
		RoomID:       room.ID,
		Name:         "Stale direct-as-group",
		InvitePolicy: "owner",
	}); err != nil {
		t.Fatal(err)
	}

	profile := room.CreateAndInsert(t, owner, DirextalkRoomProfileEventType, map[string]any{
		"room_type":      DirextalkRoomTypeDirect,
		"name":           "Remote Direct",
		"visibility":     "private",
		"join_policy":    "invite",
		"invite_policy":  "owner",
		"requester_mxid": owner.ID,
		"target_mxid":    remote.ID,
		"display_name":   "Owner",
		"domain":         domainFromMXID(owner.ID),
	}, test.WithStateKey(""))
	if err := service.ProjectRoomEvent(context.Background(), profile); err != nil {
		t.Fatal(err)
	}

	groups, err := service.listGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("direct room profile must not be projected as group, got %#v", groups)
	}
	p2pEvents := mustListP2PEvents(t, service)
	if len(p2pEvents) != 1 || p2pEvents[0].Type != "profile.changed" || p2pEvents[0].Payload["room_type"] != DirextalkRoomTypeDirect {
		t.Fatalf("expected direct profile change event, got %#v", p2pEvents)
	}
}

func TestSaveContactRemovesStaleGroupForSameRoom(t *testing.T) {
	remote := test.NewUser(t)
	room := test.NewRoom(t, remote)
	service := NewService(Config{ServerName: "test"})
	if err := service.saveGroup(context.Background(), groupRecord{
		RoomID:       room.ID,
		Name:         "Stale direct-as-group",
		InvitePolicy: "owner",
	}); err != nil {
		t.Fatal(err)
	}

	if err := service.saveContact(context.Background(), contactRecord{
		RoomID:      room.ID,
		PeerMXID:    remote.ID,
		DisplayName: "Remote Direct",
		Domain:      domainFromMXID(remote.ID),
		Status:      "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	groups, err := service.listGroups(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 0 {
		t.Fatalf("accepted contact room must not remain in groups, got %#v", groups)
	}
}

func TestProjectChannelCommentIgnoresMalformedOptionalMentions(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})
	if err := service.saveChannel(context.Background(), channel{
		ChannelID: "ch_remote",
		RoomID:    room.ID,
		Name:      "Remote Channel",
	}); err != nil {
		t.Fatal(err)
	}
	comment := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype":        "m.text",
		"body":           "projected comment",
		"p2p_kind":       "channel_comment",
		"channel_id":     "ch_remote",
		"post_id":        "post_remote",
		"comment_id":     "comment_remote",
		"mentions_json":  "{not-json",
		"mentions_extra": "{also-ignored",
	})

	if err := service.ProjectRoomEvent(context.Background(), comment); err != nil {
		t.Fatalf("expected malformed optional mentions metadata to be ignored, got %v", err)
	}
	comments := mustHandle[map[string]any](t, service, "channels.comments.list", map[string]any{"post_id": "post_remote"})
	gotComments := comments["comments"].([]channelCommentRecord)
	if len(gotComments) != 1 || gotComments[0].MentionsJSON != "[]" {
		t.Fatalf("expected projected comment with empty mentions fallback, got %#v", comments)
	}
}

func TestProjectReactionAndMembershipEvents(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = user.ID

	post := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype":    "m.text",
		"body":       "projected post",
		"p2p_kind":   "channel_post",
		"channel_id": "ch_remote",
		"post_id":    "post_remote",
	})
	if err := service.ProjectRoomEvent(context.Background(), post); err != nil {
		t.Fatal(err)
	}
	reaction := room.CreateAndInsert(t, user, "m.reaction", map[string]any{
		"m.relates_to": map[string]any{
			"rel_type": "m.annotation",
			"event_id": post.EventID(),
			"key":      "like",
		},
		"channel_id": "ch_remote",
		"post_id":    "post_remote",
	})
	if err := service.ProjectRoomEvent(context.Background(), reaction); err != nil {
		t.Fatal(err)
	}
	reactions := mustHandle[map[string]any](t, service, "channels.my_reactions", nil)
	gotReactions, ok := reactions["reactions"].([]channelReactionHistory)
	if !ok || len(gotReactions) != 1 || gotReactions[0].Reaction.PostID != "post_remote" || gotReactions[0].Reaction.Reaction != "like" {
		t.Fatalf("expected projected reaction, got %#v", reactions)
	}
	if gotReactions[0].Post == nil || gotReactions[0].Post.Body != "projected post" {
		t.Fatalf("expected projected reaction, got %#v", reactions)
	}

	member := room.CreateAndInsert(t, user, "m.room.member", map[string]any{
		"membership":  "join",
		"displayname": "Projected Owner",
	}, test.WithStateKey(user.ID))
	if err := service.ProjectRoomEvent(context.Background(), member); err != nil {
		t.Fatal(err)
	}
	members := mustHandle[map[string]any](t, service, "groups.members", map[string]any{"room_id": room.ID})
	gotMembers, ok := members["members"].([]memberRecord)
	if !ok || len(gotMembers) != 1 || gotMembers[0].UserID != user.ID || gotMembers[0].DisplayName != "Projected Owner" {
		t.Fatalf("expected projected member, got %#v", members)
	}
}

func TestChannelPostFavoritesProjectPerUserAndRefreshForAnotherUser(t *testing.T) {
	userA := test.NewUser(t)
	userB := test.NewUser(t)
	userC := test.NewUser(t)
	room := test.NewRoom(t, userA)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = userC.ID
	room.CreateAndInsert(t, userB, "m.room.member", map[string]any{"membership": "join"}, test.WithStateKey(userB.ID))

	post := room.CreateAndInsert(t, userA, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "favoriteable post", "p2p_kind": "channel_post",
		"channel_id": "ch_remote", "post_id": "post_remote",
	})
	if err := service.ProjectRoomEvent(context.Background(), post); err != nil {
		t.Fatal(err)
	}
	projectFavorite := func(user *test.User, active bool) {
		reaction := room.CreateAndInsert(t, user, "m.reaction", map[string]any{
			"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": post.EventID(), "key": "favorite"},
			"channel_id":   "ch_remote", "post_id": "post_remote", "reaction": "favorite", "active": active,
		})
		if err := service.ProjectRoomEvent(context.Background(), reaction); err != nil {
			t.Fatal(err)
		}
	}
	projectFavorite(userA, true)
	projectFavorite(userA, true) // projector replay must remain idempotent for the same user.
	projectFavorite(userB, true)

	posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": "ch_remote"})["posts"].([]channelPostRecord)
	if len(posts) != 1 || posts[0].FavoriteCount != 2 || posts[0].FavoritedByMe {
		t.Fatalf("third-user refresh should expose two remote favorites without own state, got %#v", posts)
	}

	projectFavorite(userB, false)
	posts = mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": "ch_remote"})["posts"].([]channelPostRecord)
	if posts[0].FavoriteCount != 1 || posts[0].FavoritedByMe {
		t.Fatalf("removed favorite should leave only user A active, got %#v", posts[0])
	}

	service.ownerMXID = userA.ID
	posts = mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": "ch_remote"})["posts"].([]channelPostRecord)
	if posts[0].FavoriteCount != 1 || !posts[0].FavoritedByMe {
		t.Fatalf("favoriting user should retain own selected state, got %#v", posts[0])
	}
}

func TestProjectOwnerJoinRepairsPendingInboundContact(t *testing.T) {
	testProjectOwnerJoinRepairsContactStatus(t, "pending_inbound")
}

func TestProjectOwnerJoinRepairsRejectedContactAfterAcceptCrash(t *testing.T) {
	testProjectOwnerJoinRepairsContactStatus(t, "rejected")
}

func testProjectOwnerJoinRepairsContactStatus(t *testing.T, initialStatus string) {
	t.Helper()
	owner := test.NewUser(t)
	peer := test.NewUser(t)
	room := test.NewRoom(t, owner)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID
	if err := service.saveContact(context.Background(), contactRecord{
		RoomID: room.ID, PeerMXID: peer.ID, DisplayName: "Peer", Status: initialStatus, RequestID: "$invite-a",
	}); err != nil {
		t.Fatal(err)
	}

	join := room.CreateAndInsert(t, owner, "m.room.member", map[string]any{
		"membership": "join", "displayname": "Owner",
	}, test.WithStateKey(owner.ID))
	if err := service.ProjectRoomEvent(context.Background(), join); err != nil {
		t.Fatal(err)
	}
	contact, ok, err := service.lookupContactByRoom(context.Background(), room.ID)
	if err != nil || !ok || contact.Status != "accepted" {
		t.Fatalf("owner join did not repair contact: contact=%#v ok=%v err=%v", contact, ok, err)
	}
	conversation, found, err := service.conversationModule.GetRecord(context.Background(), "", room.ID)
	if err != nil || !found || conversation.Lifecycle != "active" {
		t.Fatalf("owner join did not repair direct conversation: conversation=%#v found=%v err=%v", conversation, found, err)
	}
}

func TestProjectDirectInviteCreatesPendingInboundContact(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, remote)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID

	invite := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership":  "invite",
		"is_direct":   true,
		"displayname": "Owner Invitee Name",
	}, test.WithStateKey(owner.ID))
	setInviteRoomState(t, invite, remote.ID, map[string]any{
		"requester_mxid": remote.ID,
		"target_mxid":    owner.ID,
		"display_name":   "Remote Nick",
		"remark":         "我是 Remote，请通过好友申请",
		"domain":         domainFromMXID(remote.ID),
	})
	if err := service.ProjectRoomEvent(context.Background(), invite); err != nil {
		t.Fatal(err)
	}

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	contacts, ok := bootstrap["contacts"].([]contactRecord)
	if !ok || len(contacts) != 1 {
		t.Fatalf("expected direct invite to appear as pending contact, got %#v", bootstrap["contacts"])
	}
	if contacts[0].PeerMXID != remote.ID || contacts[0].RoomID != room.ID || contacts[0].Status != "pending_inbound" {
		t.Fatalf("expected pending inbound contact for remote inviter, got %#v", contacts[0])
	}
	pending := bootstrap["pending"].(map[string]any)
	friendRequests := pending["friend_requests"].([]map[string]any)
	if len(friendRequests) != 1 || friendRequests[0]["id"] != room.ID || friendRequests[0]["title"] != "Remote Nick" {
		t.Fatalf("expected pending friend request notice for direct invite, got %#v", pending)
	}
	if friendRequests[0]["remark"] != "我是 Remote，请通过好友申请" {
		t.Fatalf("expected pending friend request notice to include remark, got %#v", friendRequests)
	}
	p2pEvents := mustListP2PEvents(t, service)
	if len(p2pEvents) != 1 || p2pEvents[0].Type != "contact.requested" || p2pEvents[0].RoomID != room.ID {
		t.Fatalf("expected contact request event for direct invite, got %#v", p2pEvents)
	}
	if p2pEvents[0].Payload["peer_mxid"] != remote.ID || p2pEvents[0].Payload["status"] != "pending_inbound" || p2pEvents[0].Payload["remark"] != "我是 Remote，请通过好友申请" {
		t.Fatalf("unexpected contact request event payload: %#v", p2pEvents[0].Payload)
	}
}

func TestProjectDirectInviteAcceptsPendingOutboundReplacementContact(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, remote)
	transport := &recordingTransport{}
	service := NewServiceWithTransport(Config{ServerName: "test"}, transport)
	service.ownerMXID = owner.ID
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    remote.ID,
		DisplayName: "Remote Old",
		Domain:      domainFromMXID(remote.ID),
		RoomID:      "!old-direct:test",
		Status:      "pending_outbound",
		Remark:      "old request",
	}); err != nil {
		t.Fatal(err)
	}

	invite := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
		"is_direct":  true,
	}, test.WithStateKey(owner.ID))
	setInviteRoomState(t, invite, remote.ID, map[string]any{
		"requester_mxid": remote.ID,
		"target_mxid":    owner.ID,
		"display_name":   "Remote Accepted",
		"avatar_url":     "mxc://test/remote-accepted",
		"remark":         "replacement request must not surface",
		"domain":         domainFromMXID(remote.ID),
	})
	if err := service.ProjectRoomEvent(context.Background(), invite); err != nil {
		t.Fatal(err)
	}

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	contacts := bootstrap["contacts"].([]contactRecord)
	if len(contacts) != 1 {
		t.Fatalf("expected one accepted contact, got %#v", contacts)
	}
	contact := contacts[0]
	if contact.PeerMXID != remote.ID || contact.RoomID != room.ID || contact.Status != "accepted" {
		t.Fatalf("expected replacement invite to accept pending outbound contact, got %#v", contact)
	}
	if contact.DisplayName != "Remote Accepted" || contact.AvatarURL != "mxc://test/remote-accepted" || contact.Remark != "" {
		t.Fatalf("expected replacement invite to refresh profile and clear request remark, got %#v", contact)
	}
	pending := bootstrap["pending"].(map[string]any)
	friendRequests := pending["friend_requests"].([]map[string]any)
	if len(friendRequests) != 0 {
		t.Fatalf("expected replacement invite not to create a new inbound request, got %#v", friendRequests)
	}
	if len(transport.joins) != 1 || transport.joins[0] != owner.ID+" in "+room.ID {
		t.Fatalf("expected owner to join replacement direct room, got %#v", transport.joins)
	}
}

func TestProjectDirectInviteReplacesAcceptedContactWhenRoomChanges(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, remote)
	transport := &failingInviteTransport{err: productpolicy.Forbidden("sender is not joined to the dirextalk room")}
	service := NewServiceWithTransport(Config{ServerName: "test"}, transport)
	service.ownerMXID = owner.ID
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    remote.ID,
		DisplayName: "Remote Old",
		Domain:      domainFromMXID(remote.ID),
		RoomID:      "!old-direct:test",
		Status:      "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	invite := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
		"is_direct":  true,
	}, test.WithStateKey(owner.ID))
	setInviteRoomState(t, invite, remote.ID, map[string]any{
		"requester_mxid": remote.ID,
		"target_mxid":    owner.ID,
		"display_name":   "Remote Replacement",
		"avatar_url":     "mxc://test/remote-replacement",
		"domain":         domainFromMXID(remote.ID),
	})
	if err := service.ProjectRoomEvent(context.Background(), invite); err != nil {
		t.Fatal(err)
	}

	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if len(contacts) != 1 {
		t.Fatalf("expected one contact after replacement, got %#v", contacts)
	}
	contact := contacts[0]
	if contact.PeerMXID != remote.ID || contact.RoomID != room.ID || contact.Status != "accepted" {
		t.Fatalf("expected changed-room invite to replace accepted contact, got %#v", contact)
	}
	if contact.DisplayName != "Remote Replacement" || contact.AvatarURL != "mxc://test/remote-replacement" {
		t.Fatalf("expected replacement invite to refresh profile, got %#v", contact)
	}
	if len(transport.joins) != 1 || transport.joins[0] != owner.ID+" in "+room.ID {
		t.Fatalf("expected owner to join replacement room, got %#v", transport.joins)
	}
	if len(transport.inviteRequests) != 1 || transport.inviteRequests[0].RoomID != "!old-direct:test" {
		t.Fatalf("expected one failed retained-room invite before replacement, got %#v", transport.inviteRequests)
	}
	conversations := mustHandle[map[string]any](t, service, "conversations.list", nil)["conversations"].([]conversationView)
	for _, conversation := range conversations {
		if conversation.MatrixRoomID == "!old-direct:test" {
			t.Fatalf("expected old direct conversation to be removed after replacement, got %#v", conversations)
		}
	}
	replacementConversation := false
	for _, conversation := range conversations {
		if conversation.MatrixRoomID == room.ID && conversation.RelationshipStatus == "accepted" {
			replacementConversation = true
		}
	}
	if !replacementConversation {
		t.Fatalf("expected replacement direct conversation to be accepted, got %#v", conversations)
	}
}

func TestProjectContactRequestInviteWithoutDirectFlagDoesNotCreateGroupInvite(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, remote)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID

	invite := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
	}, test.WithStateKey(owner.ID))
	setInviteRoomState(t, invite, remote.ID, map[string]any{
		"requester_mxid": remote.ID,
		"target_mxid":    owner.ID,
		"display_name":   "Remote Nick",
		"domain":         domainFromMXID(remote.ID),
	})
	if err := service.ProjectRoomEvent(context.Background(), invite); err != nil {
		t.Fatal(err)
	}

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	pending := bootstrap["pending"].(map[string]any)
	friendRequests := pending["friend_requests"].([]map[string]any)
	groupInvites := pending["group_invites"].([]map[string]any)
	if len(friendRequests) != 1 || friendRequests[0]["id"] != room.ID || friendRequests[0]["title"] != "Remote Nick" {
		t.Fatalf("expected contact request invite to appear only as friend request, got %#v", pending)
	}
	if len(groupInvites) != 0 {
		t.Fatalf("expected contact request invite not to create group invite, got %#v", groupInvites)
	}
}

func TestProjectNativeDirectProfileInviteCreatesPendingInboundContact(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, remote)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID

	invite := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
	}, test.WithStateKey(owner.ID))
	setInviteRoomProfileState(t, invite, remote.ID, map[string]any{
		"room_type":      DirextalkRoomTypeDirect,
		"requester_mxid": remote.ID,
		"target_mxid":    owner.ID,
		"display_name":   "Remote Native Nick",
		"avatar_url":     "mxc://test/native-remote",
		"domain":         domainFromMXID(remote.ID),
	})
	if err := service.ProjectRoomEvent(context.Background(), invite); err != nil {
		t.Fatal(err)
	}

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	pending := bootstrap["pending"].(map[string]any)
	friendRequests := pending["friend_requests"].([]map[string]any)
	if len(friendRequests) != 1 || friendRequests[0]["id"] != room.ID || friendRequests[0]["title"] != "Remote Native Nick" {
		t.Fatalf("expected native direct profile invite to appear as friend request, got %#v", pending)
	}
	contacts := bootstrap["contacts"].([]contactRecord)
	if len(contacts) != 1 || contacts[0].AvatarURL != "mxc://test/native-remote" {
		t.Fatalf("expected native direct profile invite to preserve avatar_url, got %#v", contacts)
	}
}

func TestProjectNativeDirectProfileInviteUsesActualSenderIdentity(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	spoofed := "@b:spoofed.example"
	room := test.NewRoom(t, remote)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID

	invite := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
	}, test.WithStateKey(owner.ID))
	setInviteRoomProfileState(t, invite, spoofed, map[string]any{
		"room_type":      DirextalkRoomTypeDirect,
		"requester_mxid": spoofed,
		"target_mxid":    owner.ID,
		"display_name":   "Spoofed B",
		"avatar_url":     "mxc://test/spoofed",
		"domain":         "spoofed.example",
	})

	if err := service.ProjectRoomEvent(context.Background(), invite); err != nil {
		t.Fatal(err)
	}

	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if len(contacts) != 1 {
		t.Fatalf("expected one pending contact, got %#v", contacts)
	}
	if contacts[0].PeerMXID != remote.ID || contacts[0].Domain != domainFromMXID(remote.ID) {
		t.Fatalf("expected contact identity to come from actual Matrix sender %s, got %#v", remote.ID, contacts[0])
	}
	if contacts[0].PeerMXID == spoofed || contacts[0].Domain == "spoofed.example" {
		t.Fatalf("direct invite must not trust spoofed profile identity, got %#v", contacts[0])
	}
}

func TestProjectDirectInviteUsesSenderProfileFromInviteState(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, remote)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID

	invite := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
		"is_direct":  true,
	}, test.WithStateKey(owner.ID))
	setInviteRoomStates(t, invite, []map[string]any{{
		"type":      "m.room.create",
		"state_key": "",
		"sender":    remote.ID,
		"content": map[string]any{
			"creator": remote.ID,
			"type":    DirextalkRoomTypeDirect,
		},
	}, {
		"type":      "m.room.member",
		"state_key": remote.ID,
		"sender":    remote.ID,
		"content": map[string]any{
			"membership":  "join",
			"displayname": "Remote Profile Nick",
			"avatar_url":  "mxc://test/remote",
		},
	}})
	if err := service.ProjectRoomEvent(context.Background(), invite); err != nil {
		t.Fatal(err)
	}

	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if len(contacts) != 1 || contacts[0].PeerMXID != remote.ID || contacts[0].DisplayName != "Remote Profile Nick" || contacts[0].AvatarURL != "mxc://test/remote" || contacts[0].Domain != domainFromMXID(remote.ID) {
		t.Fatalf("expected direct invite to use sender profile from invite state, got %#v", contacts)
	}
}

func TestProjectDirectInviteReinvitesAcceptedPeerToRetainedRoom(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	spoofed := test.NewUser(t)
	newRoom := test.NewRoom(t, remote)
	transport := &recordingTransport{}
	service := NewServiceWithTransport(Config{ServerName: "test"}, transport)
	service.ownerMXID = owner.ID
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    remote.ID,
		DisplayName: "Remote",
		Domain:      domainFromMXID(remote.ID),
		RoomID:      "!old-direct:test",
		Status:      "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	invite := newRoom.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
		"is_direct":  true,
	}, test.WithStateKey(owner.ID))
	setInviteRoomState(t, invite, spoofed.ID, map[string]any{
		"requester_mxid": spoofed.ID,
		"target_mxid":    owner.ID,
		"display_name":   "Spoofed Request",
		"domain":         domainFromMXID(spoofed.ID),
	})

	if err := service.ProjectRoomEvent(context.Background(), invite); err != nil {
		t.Fatal(err)
	}

	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if len(contacts) != 1 || contacts[0].PeerMXID != remote.ID || contacts[0].RoomID != "!old-direct:test" || contacts[0].Status != "accepted" {
		t.Fatalf("expected accepted retained contact to stay on old room, got %#v", contacts)
	}
	if len(transport.inviteRequests) != 1 {
		t.Fatalf("expected one reactivation invite to retained direct room, got %#v", transport.inviteRequests)
	}
	if inviteReq := transport.inviteRequests[0]; inviteReq.RoomID != "!old-direct:test" || inviteReq.InviterMXID != owner.ID || inviteReq.InviteeMXID != remote.ID {
		t.Fatalf("expected retained-room invite to actual sender, got %#v", inviteReq)
	}
	p2pEvents := mustListP2PEvents(t, service)
	if len(p2pEvents) != 0 {
		t.Fatalf("accepted peer reactivation must not create a new pending request event, got %#v", p2pEvents)
	}
}

func TestProjectDuplicateDirectInvitesKeepFirstPendingInboundContact(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	firstRoom := test.NewRoom(t, remote)
	secondRoom := test.NewRoom(t, remote)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID

	firstInvite := firstRoom.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
		"is_direct":  true,
	}, test.WithStateKey(owner.ID))
	setInviteRoomState(t, firstInvite, remote.ID, map[string]any{
		"requester_mxid": remote.ID,
		"target_mxid":    owner.ID,
		"display_name":   "Remote Nick",
	})
	if err := service.ProjectRoomEvent(context.Background(), firstInvite); err != nil {
		t.Fatal(err)
	}

	secondInvite := secondRoom.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
		"is_direct":  true,
	}, test.WithStateKey(owner.ID))
	setInviteRoomState(t, secondInvite, remote.ID, map[string]any{
		"requester_mxid": remote.ID,
		"target_mxid":    owner.ID,
		"display_name":   "Remote Nick",
	})
	if err := service.ProjectRoomEvent(context.Background(), secondInvite); err != nil {
		t.Fatal(err)
	}

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	contacts := bootstrap["contacts"].([]contactRecord)
	if len(contacts) != 1 || contacts[0].RoomID != firstRoom.ID || contacts[0].PeerMXID != remote.ID || contacts[0].Status != "pending_inbound" {
		t.Fatalf("expected duplicate direct invite to keep first pending inbound contact, got %#v", contacts)
	}
	pending := bootstrap["pending"].(map[string]any)
	friendRequests := pending["friend_requests"].([]map[string]any)
	if len(friendRequests) != 1 || friendRequests[0]["id"] != firstRoom.ID {
		t.Fatalf("expected duplicate direct invite to keep first pending friend request notice, got %#v", pending)
	}
	p2pEvents := mustListP2PEvents(t, service)
	if len(p2pEvents) != 1 || p2pEvents[0].Type != "contact.requested" || p2pEvents[0].RoomID != firstRoom.ID {
		t.Fatalf("expected duplicate direct invite to keep first contact request event only, got %#v", p2pEvents)
	}
}

func TestProjectDirectInviteReopensRejectedContact(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, remote)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    remote.ID,
		DisplayName: "Remote Rejected",
		Domain:      domainFromMXID(remote.ID),
		RoomID:      "!old-direct:test",
		Status:      "rejected",
	}); err != nil {
		t.Fatal(err)
	}

	invite := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
		"is_direct":  true,
	}, test.WithStateKey(owner.ID))
	setInviteRoomState(t, invite, remote.ID, map[string]any{
		"requester_mxid": remote.ID,
		"target_mxid":    owner.ID,
		"display_name":   "Remote Again",
		"domain":         domainFromMXID(remote.ID),
	})
	if err := service.ProjectRoomEvent(context.Background(), invite); err != nil {
		t.Fatal(err)
	}

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	contacts := bootstrap["contacts"].([]contactRecord)
	if len(contacts) != 1 || contacts[0].RoomID != room.ID || contacts[0].Status != "pending_inbound" || contacts[0].DisplayName != "Remote Again" {
		t.Fatalf("expected repeated direct invite to reopen rejected contact as pending inbound, got %#v", contacts)
	}
	pending := bootstrap["pending"].(map[string]any)
	friendRequests := pending["friend_requests"].([]map[string]any)
	if len(friendRequests) != 1 || friendRequests[0]["id"] != room.ID || friendRequests[0]["title"] != "Remote Again" {
		t.Fatalf("expected reopened rejected contact to produce pending friend request, got %#v", pending)
	}
	p2pEvents := mustListP2PEvents(t, service)
	if len(p2pEvents) != 1 || p2pEvents[0].Type != "contact.requested" || p2pEvents[0].RoomID != room.ID {
		t.Fatalf("expected reopened rejected contact to emit contact request event, got %#v", p2pEvents)
	}
	if p2pEvents[0].Payload["display_name"] != "Remote Again" || p2pEvents[0].Payload["peer_mxid"] != remote.ID {
		t.Fatalf("unexpected reopened contact event payload: %#v", p2pEvents[0].Payload)
	}
}

func TestProjectDirectJoinAcceptsPendingOutboundContact(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, owner)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    remote.ID,
		DisplayName: "Remote",
		Domain:      domainFromMXID(remote.ID),
		RoomID:      room.ID,
		Status:      "pending_outbound",
	}); err != nil {
		t.Fatal(err)
	}

	join := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership":  "join",
		"displayname": "Remote",
		"avatar_url":  "mxc://test/remote-joined",
	}, test.WithStateKey(remote.ID))
	if err := service.ProjectRoomEvent(context.Background(), join); err != nil {
		t.Fatal(err)
	}

	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if len(contacts) != 1 || contacts[0].PeerMXID != remote.ID || contacts[0].Status != "accepted" || contacts[0].AvatarURL != "mxc://test/remote-joined" {
		t.Fatalf("expected remote join to accept pending outbound contact, got %#v", contacts)
	}
}

func TestProjectDirectMemberProfileUpdateRefreshesAcceptedContact(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, owner)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:    remote.ID,
		DisplayName: "Remote Old",
		AvatarURL:   "mxc://test/old",
		Domain:      domainFromMXID(remote.ID),
		RoomID:      room.ID,
		Status:      "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	update := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership":  "join",
		"displayname": "Remote New",
		"avatar_url":  "mxc://test/new",
	}, test.WithStateKey(remote.ID))
	if err := service.ProjectRoomEvent(context.Background(), update); err != nil {
		t.Fatal(err)
	}

	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if len(contacts) != 1 || contacts[0].DisplayName != "Remote New" || contacts[0].AvatarURL != "mxc://test/new" || contacts[0].Status != "accepted" {
		t.Fatalf("expected direct member profile update to refresh accepted contact, got %#v", contacts)
	}
	conversations := mustHandle[map[string]any](t, service, "conversations.list", nil)["conversations"].([]conversationView)
	if len(conversations) != 1 || conversations[0].Title != "Remote New" || conversations[0].AvatarURL != "mxc://test/new" {
		t.Fatalf("expected direct conversation to use refreshed contact profile, got %#v", conversations)
	}
}

func TestProjectDirectMemberProfileUpdatePreservesLocalContactRemark(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, owner)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID:            remote.ID,
		DisplayName:         "Local Remark",
		DisplayNameOverride: true,
		AvatarURL:           "mxc://test/old",
		Domain:              domainFromMXID(remote.ID),
		RoomID:              room.ID,
		Status:              "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	update := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership":  "join",
		"displayname": "Remote New",
		"avatar_url":  "mxc://test/new",
	}, test.WithStateKey(remote.ID))
	if err := service.ProjectRoomEvent(context.Background(), update); err != nil {
		t.Fatal(err)
	}

	contacts := mustHandle[map[string]any](t, service, "contacts.list", nil)["contacts"].([]contactRecord)
	if len(contacts) != 1 || contacts[0].DisplayName != "Local Remark" || !contacts[0].DisplayNameOverride || contacts[0].AvatarURL != "mxc://test/new" || contacts[0].Status != "accepted" {
		t.Fatalf("expected direct member profile update to preserve local contact remark, got %#v", contacts)
	}
	conversations := mustHandle[map[string]any](t, service, "conversations.list", nil)["conversations"].([]conversationView)
	if len(conversations) != 1 || conversations[0].Title != "Local Remark" || conversations[0].AvatarURL != "mxc://test/new" {
		t.Fatalf("expected direct conversation to keep local contact remark, got %#v", conversations)
	}
}

func TestProjectOutputNewInviteCreatesPendingInboundContact(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, remote)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID

	invite := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership":  "invite",
		"is_direct":   true,
		"displayname": "Owner Invitee Name",
	}, test.WithStateKey(owner.ID))
	setInviteRoomState(t, invite, remote.ID, map[string]any{
		"requester_mxid": remote.ID,
		"target_mxid":    owner.ID,
		"display_name":   "Remote Nick",
		"domain":         domainFromMXID(remote.ID),
	})
	if err := service.ProjectOutputEvent(context.Background(), roomserverAPI.OutputEvent{
		Type: roomserverAPI.OutputTypeNewInviteEvent,
		NewInviteEvent: &roomserverAPI.OutputNewInviteEvent{
			Event: invite,
		},
	}); err != nil {
		t.Fatal(err)
	}

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	contacts, ok := bootstrap["contacts"].([]contactRecord)
	if !ok || len(contacts) != 1 {
		t.Fatalf("expected new invite output to appear as pending contact, got %#v", bootstrap["contacts"])
	}
	if contacts[0].PeerMXID != remote.ID || contacts[0].RoomID != room.ID || contacts[0].Status != "pending_inbound" || contacts[0].DisplayName != "Remote Nick" {
		t.Fatalf("expected pending inbound contact for remote inviter, got %#v", contacts[0])
	}
}

func TestProjectOutputNewInviteCreatesGroupAndChannelPendingItems(t *testing.T) {
	owner := test.NewUser(t)
	remote := test.NewUser(t)
	groupRoom := test.NewRoom(t, remote)
	channelRoom := test.NewRoom(t, remote)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID

	groupInvite := groupRoom.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
	}, test.WithStateKey(owner.ID))
	setInviteRoomProfileState(t, groupInvite, remote.ID, map[string]any{
		"room_type": DirextalkRoomTypeGroup,
		"room_id":   groupRoom.ID,
		"name":      "远端群聊",
	})
	channelInvite := channelRoom.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "invite",
	}, test.WithStateKey(owner.ID))
	setInviteRoomProfileState(t, channelInvite, remote.ID, map[string]any{
		"room_type":        DirextalkRoomTypeChannel,
		"channel_id":       "remote_channel",
		"room_id":          channelRoom.ID,
		"name":             "远端频道",
		"visibility":       "public",
		"join_policy":      "invite",
		"channel_type":     "chat",
		"comments_enabled": true,
	})
	for _, invite := range []*types.HeaderedEvent{groupInvite, channelInvite} {
		if err := service.ProjectOutputEvent(context.Background(), roomserverAPI.OutputEvent{
			Type: roomserverAPI.OutputTypeNewInviteEvent,
			NewInviteEvent: &roomserverAPI.OutputNewInviteEvent{
				Event: invite,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	bootstrap := mustHandle[map[string]any](t, service, "sync.bootstrap", nil)
	pending := bootstrap["pending"].(map[string]any)
	groupInvites := pending["group_invites"].([]map[string]any)
	channelNotices := pending["channel_notices"].([]map[string]any)
	if len(groupInvites) != 1 || groupInvites[0]["id"] != groupRoom.ID || groupInvites[0]["title"] != "远端群聊" {
		t.Fatalf("expected projected group invite, got %#v", pending["group_invites"])
	}
	if len(channelNotices) != 1 || channelNotices[0]["id"] != channelRoom.ID || channelNotices[0]["title"] != "远端频道" {
		t.Fatalf("expected projected channel invite, got %#v", pending["channel_notices"])
	}
}

func TestProjectTechnicalChannelInvitePreservesPublicJoinWorkflowGeneration(t *testing.T) {
	for _, membership := range []string{"pending", "joining"} {
		t.Run(membership, func(t *testing.T) {
			owner := test.NewUser(t)
			requester := test.NewUser(t)
			room := test.NewRoom(t, owner)
			service := NewService(Config{ServerName: "test"})
			service.ownerMXID = owner.ID
			if err := service.saveChannel(context.Background(), channel{
				ChannelID: "public_channel", RoomID: room.ID, Name: "Public Channel",
				Visibility: "public", JoinPolicy: "open", ChannelType: "chat",
			}); err != nil {
				t.Fatal(err)
			}
			if err := service.saveMember(context.Background(), memberRecord{
				RoomID: room.ID, ChannelID: "public_channel", UserID: requester.ID,
				Membership: membership, Role: "member", RequestID: "request-a",
				RequesterNodeBaseURL: "https://requester.example/_p2p",
			}); err != nil {
				t.Fatal(err)
			}

			invite := room.CreateAndInsert(t, owner, "m.room.member", map[string]any{
				"membership":                          "invite",
				"io.dirextalk.public_join_request_id": "request-a",
			}, test.WithStateKey(requester.ID))
			if err := service.ProjectRoomEvent(context.Background(), invite); err != nil {
				t.Fatal(err)
			}

			projected, ok, err := service.lookupMember(context.Background(), room.ID, requester.ID)
			if err != nil || !ok {
				t.Fatalf("expected public join workflow member: member=%#v ok=%v err=%v", projected, ok, err)
			}
			if projected.Membership != membership || projected.RequestID != "request-a" {
				t.Fatalf("technical Matrix invite replaced public join workflow generation: member=%#v event_id=%s", projected, invite.EventID())
			}
		})
	}
}

func TestProjectStaleTechnicalChannelInviteCannotReplaceNewPublicJoinGeneration(t *testing.T) {
	owner := test.NewUser(t)
	requester := test.NewUser(t)
	room := test.NewRoom(t, owner)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = owner.ID
	if err := service.saveChannel(context.Background(), channel{
		ChannelID: "public_channel", RoomID: room.ID, Name: "Public Channel",
		Visibility: "public", JoinPolicy: "open", ChannelType: "chat",
	}); err != nil {
		t.Fatal(err)
	}
	want := memberRecord{
		RoomID: room.ID, ChannelID: "public_channel", UserID: requester.ID,
		Membership: "joining", Role: "member", RequestID: "request-b",
		RequesterNodeBaseURL: "https://requester.example/_p2p",
	}
	if err := service.saveMember(context.Background(), want); err != nil {
		t.Fatal(err)
	}

	staleInvite := room.CreateAndInsert(t, owner, "m.room.member", map[string]any{
		"membership":                          "invite",
		"io.dirextalk.public_join_request_id": "request-a",
	}, test.WithStateKey(requester.ID))
	if err := service.ProjectRoomEvent(context.Background(), staleInvite); err != nil {
		t.Fatal(err)
	}

	got, ok, err := service.lookupMember(context.Background(), room.ID, requester.ID)
	if err != nil || !ok {
		t.Fatalf("expected current public join generation: member=%#v ok=%v err=%v", got, ok, err)
	}
	if got.Membership != want.Membership || got.RequestID != want.RequestID {
		t.Fatalf("stale technical invite replaced request B: got=%#v want=%#v event_id=%s", got, want, staleInvite.EventID())
	}
}

func TestProjectProductReinviteAdvancesMemberRequestGenerationIdempotently(t *testing.T) {
	tests := []struct {
		name      string
		roomType  string
		channelID string
	}{
		{name: "group", roomType: DirextalkRoomTypeGroup},
		{name: "channel", roomType: DirextalkRoomTypeChannel, channelID: "remote_channel"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			owner := test.NewUser(t)
			remote := test.NewUser(t)
			room := test.NewRoom(t, remote)
			service := NewService(Config{ServerName: "test"})
			service.ownerMXID = owner.ID
			if tt.roomType == DirextalkRoomTypeGroup {
				if err := service.saveGroup(context.Background(), groupRecord{
					RoomID: room.ID, Name: "Remote Group", InvitePolicy: "member",
				}); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := service.saveChannel(context.Background(), channel{
					ChannelID: tt.channelID, RoomID: room.ID, Name: "Remote Channel",
					Visibility: "public", JoinPolicy: "invite", ChannelType: "chat",
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.saveMember(context.Background(), memberRecord{
				RoomID: room.ID, ChannelID: tt.channelID, UserID: owner.ID,
				// A completed public-join workflow must not make a later ordinary
				// Product invite look like the technical invite from that workflow.
				Membership: "rejected", Role: "member", RequestID: "$public-join-a",
				RequesterNodeBaseURL: "https://requester.example/_p2p",
			}); err != nil {
				t.Fatal(err)
			}

			inviteB := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
				"membership": "invite",
			}, test.WithStateKey(owner.ID))
			profile := map[string]any{
				"room_type": tt.roomType,
				"room_id":   room.ID,
				"name":      "Remote " + tt.name,
			}
			if tt.channelID != "" {
				profile["channel_id"] = tt.channelID
				profile["visibility"] = "public"
				profile["join_policy"] = "invite"
				profile["channel_type"] = "chat"
			}
			setInviteRoomProfileState(t, inviteB, remote.ID, profile)
			output := roomserverAPI.OutputEvent{
				Type: roomserverAPI.OutputTypeNewInviteEvent,
				NewInviteEvent: &roomserverAPI.OutputNewInviteEvent{
					Event: inviteB,
				},
			}

			if err := service.ProjectOutputEvent(context.Background(), output); err != nil {
				t.Fatal(err)
			}
			projected, ok, err := service.lookupMember(context.Background(), room.ID, owner.ID)
			if err != nil || !ok || projected.Membership != "invite" ||
				projected.RequestID != inviteB.EventID() || projected.RequestID == "$invite-a" {
				t.Fatalf("invite B did not replace generation A: member=%#v ok=%v err=%v", projected, ok, err)
			}
			firstEvents := mustListP2PEvents(t, service)

			if err := service.ProjectOutputEvent(context.Background(), output); err != nil {
				t.Fatal(err)
			}
			replayed, ok, err := service.lookupMember(context.Background(), room.ID, owner.ID)
			if err != nil || !ok || replayed.Membership != "invite" || replayed.RequestID != inviteB.EventID() {
				t.Fatalf("duplicate invite B changed the generation: member=%#v ok=%v err=%v", replayed, ok, err)
			}
			secondEvents := mustListP2PEvents(t, service)
			if len(secondEvents) != len(firstEvents) {
				t.Fatalf("duplicate invite B appended another outbox event: first=%#v second=%#v", firstEvents, secondEvents)
			}
		})
	}
}

func setInviteRoomState(t *testing.T, event *types.HeaderedEvent, sender string, content map[string]any) {
	t.Helper()
	native := map[string]any{"room_type": DirextalkRoomTypeDirect}
	for key, value := range content {
		native[key] = value
	}
	setInviteRoomProfileState(t, event, sender, native)
}

func setInviteRoomProfileState(t *testing.T, event *types.HeaderedEvent, sender string, content map[string]any) {
	t.Helper()
	setInviteRoomStates(t, event, []map[string]any{{
		"type":      DirextalkRoomProfileEventType,
		"state_key": "",
		"sender":    sender,
		"content":   content,
	}})
}

func setInviteRoomStates(t *testing.T, event *types.HeaderedEvent, states []map[string]any) {
	t.Helper()
	pdu, err := event.SetUnsigned(map[string]any{"invite_room_state": states})
	if err != nil {
		t.Fatal(err)
	}
	event.PDU = pdu
}

func TestDomainFromMXIDKeepsServerPort(t *testing.T) {
	if got := domainFromMXID("@owner:dendrite-b:8448"); got != "dendrite-b:8448" {
		t.Fatalf("expected Matrix server name with port, got %q", got)
	}
}

func TestProjectMemberUsesKnownChannelForRoom(t *testing.T) {
	user := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})

	state := room.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"room_type":    DirextalkRoomTypeChannel,
		"channel_id":   "ch_remote",
		"channel_type": "chat",
		"name":         "Remote Channel",
	}, test.WithStateKey(""))
	if err := service.ProjectRoomEvent(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	member := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership":  "join",
		"displayname": "Remote Member",
		"avatar_url":  "mxc://test/avatar",
	}, test.WithStateKey(remote.ID))
	if err := service.ProjectRoomEvent(context.Background(), member); err != nil {
		t.Fatal(err)
	}

	members := mustHandle[map[string]any](t, service, "channels.members", map[string]any{"channel_id": "ch_remote"})
	gotMembers, ok := members["members"].([]memberRecord)
	if !ok || len(gotMembers) != 1 || gotMembers[0].UserID != remote.ID || gotMembers[0].ChannelID != "ch_remote" || gotMembers[0].AvatarURL != "mxc://test/avatar" {
		t.Fatalf("expected projected channel member, got %#v", members)
	}
}

func TestProjectMemberEventDeduplicatesP2PDelta(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service, err := NewServiceWithStore(ctx, Config{ServerName: "test"}, store)
	if err != nil {
		t.Fatal(err)
	}
	member := room.CreateAndInsert(t, user, "m.room.member", map[string]any{
		"membership":  "join",
		"displayname": "Deduped Member",
	}, test.WithStateKey(user.ID))

	if err := service.ProjectRoomEvent(ctx, member); err != nil {
		t.Fatal(err)
	}
	if err := service.ProjectRoomEvent(ctx, member); err != nil {
		t.Fatal(err)
	}
	events, err := service.listP2PEvents(ctx, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != "room.member.projected" {
		t.Fatalf("expected one deduplicated member delta event, got %#v", events)
	}
}

func TestProjectSparseMemberEventPreservesProfileAndMute(t *testing.T) {
	user := test.NewUser(t)
	remote := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})

	state := room.CreateAndInsert(t, user, DirextalkRoomProfileEventType, map[string]any{
		"room_type":  DirextalkRoomTypeChannel,
		"channel_id": "ch_member_sparse",
		"name":       "Remote Channel",
	}, test.WithStateKey(""))
	if err := service.ProjectRoomEvent(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	joined := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership":  "join",
		"displayname": "Remote Member",
		"avatar_url":  "mxc://test/member",
		"muted":       true,
	}, test.WithStateKey(remote.ID))
	if err := service.ProjectRoomEvent(context.Background(), joined); err != nil {
		t.Fatal(err)
	}
	sparse := room.CreateAndInsert(t, remote, "m.room.member", map[string]any{
		"membership": "join",
	}, test.WithStateKey(remote.ID))
	if err := service.ProjectRoomEvent(context.Background(), sparse); err != nil {
		t.Fatal(err)
	}

	members := mustHandle[map[string]any](t, service, "channels.members", map[string]any{"channel_id": "ch_member_sparse"})
	gotMembers, ok := members["members"].([]memberRecord)
	if !ok || len(gotMembers) != 1 ||
		gotMembers[0].DisplayName != "Remote Member" ||
		gotMembers[0].AvatarURL != "mxc://test/member" ||
		!gotMembers[0].Muted {
		t.Fatalf("expected sparse member event to preserve profile fields, got %#v", members)
	}
}

func TestProjectMemberUsesJoinEventTimeAndPreservesItOnProfileUpdate(t *testing.T) {
	owner := test.NewUser(t)
	memberUser := test.NewUser(t)
	room := test.NewRoom(t, owner)
	service := NewService(Config{ServerName: "test"})
	const inviteTime = int64(1000)
	joinTime := time.UnixMilli(2000).UTC()
	profileTime := time.UnixMilli(3000).UTC()

	if err := service.saveMember(context.Background(), memberRecord{
		RoomID: room.ID, UserID: memberUser.ID, Membership: "invite", Role: "member", JoinedAt: inviteTime,
	}); err != nil {
		t.Fatal(err)
	}
	join := room.CreateAndInsert(t, memberUser, "m.room.member", map[string]any{
		"membership": "join", "displayname": "Joined Member",
	}, test.WithStateKey(memberUser.ID), test.WithTimestamp(joinTime))
	if err := service.ProjectRoomEvent(context.Background(), join); err != nil {
		t.Fatal(err)
	}

	joined, ok, err := service.lookupMember(context.Background(), room.ID, memberUser.ID)
	if err != nil || !ok || joined.JoinedAt != joinTime.UnixMilli() {
		t.Fatalf("join projection timestamp = (%#v, %v, %v), want %d", joined, ok, err, joinTime.UnixMilli())
	}

	profile := room.CreateAndInsert(t, memberUser, "m.room.member", map[string]any{
		"membership": "join", "displayname": "Updated Member",
	}, test.WithStateKey(memberUser.ID), test.WithTimestamp(profileTime))
	if err := service.ProjectRoomEvent(context.Background(), profile); err != nil {
		t.Fatal(err)
	}
	updated, ok, err := service.lookupMember(context.Background(), room.ID, memberUser.ID)
	if err != nil || !ok || updated.JoinedAt != joinTime.UnixMilli() {
		t.Fatalf("profile projection changed join timestamp = (%#v, %v, %v), want %d", updated, ok, err, joinTime.UnixMilli())
	}
}

func TestProjectOutputRedactionRemovesBusinessRecords(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})
	post := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype":    "m.text",
		"body":       "projected post",
		"p2p_kind":   "channel_post",
		"channel_id": "ch_remote",
		"post_id":    "post_remote",
	})
	if err := service.ProjectRoomEvent(context.Background(), post); err != nil {
		t.Fatal(err)
	}
	if err := service.ProjectOutputEvent(context.Background(), roomserverAPI.OutputEvent{
		Type: roomserverAPI.OutputTypeRedactedEvent,
		RedactedEvent: &roomserverAPI.OutputRedactedEvent{
			RedactedEventID: post.EventID(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": "ch_remote"})
	if gotPosts := posts["posts"].([]channelPostRecord); len(gotPosts) != 0 {
		t.Fatalf("expected redacted post hidden, got %#v", gotPosts)
	}
}

func TestProjectOutputRedactionDeactivatesFavoriteReaction(t *testing.T) {
	user := test.NewUser(t)
	room := test.NewRoom(t, user)
	service := NewService(Config{ServerName: "test"})
	service.ownerMXID = user.ID
	post := room.CreateAndInsert(t, user, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "projected post", "p2p_kind": "channel_post",
		"channel_id": "ch_remote", "post_id": "post_remote",
	})
	if err := service.ProjectRoomEvent(context.Background(), post); err != nil {
		t.Fatal(err)
	}
	favorite := room.CreateAndInsert(t, user, "m.reaction", map[string]any{
		"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": post.EventID(), "key": "favorite"},
		"channel_id":   "ch_remote", "post_id": "post_remote", "reaction": "favorite", "active": true,
	})
	if err := service.ProjectRoomEvent(context.Background(), favorite); err != nil {
		t.Fatal(err)
	}
	posts := mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": "ch_remote"})["posts"].([]channelPostRecord)
	if len(posts) != 1 || posts[0].FavoriteCount != 1 || !posts[0].FavoritedByMe {
		t.Fatalf("expected active favorite before redaction, got %#v", posts)
	}
	if err := service.ProjectOutputEvent(context.Background(), roomserverAPI.OutputEvent{
		Type: roomserverAPI.OutputTypeRedactedEvent,
		RedactedEvent: &roomserverAPI.OutputRedactedEvent{
			RedactedEventID: favorite.EventID(),
		},
	}); err != nil {
		t.Fatal(err)
	}
	posts = mustHandle[map[string]any](t, service, "channels.posts.list", map[string]any{"channel_id": "ch_remote"})["posts"].([]channelPostRecord)
	if posts[0].FavoriteCount != 0 || posts[0].FavoritedByMe {
		t.Fatalf("favorite redaction should clear aggregate and requester state, got %#v", posts[0])
	}
}

func TestProjectOutputEventIgnoresNonRoomEvents(t *testing.T) {
	service := NewService(Config{ServerName: "test"})
	if err := service.ProjectOutputEvent(context.Background(), roomserverAPI.OutputEvent{Type: roomserverAPI.OutputTypeOldRoomEvent}); err != nil {
		t.Fatalf("expected ignored output event, got %v", err)
	}
}
