package dirextalkstate

import (
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
)

func TestDirectRoomProfileIncludesAccountDeletedFields(t *testing.T) {
	event := DirectRoomProfile(DirectRoomProfileInput{
		Name:                 " Alice ",
		RequesterMXID:        " @owner:example.com ",
		TargetMXID:           "@alice:remote.example",
		RequesterDisplayName: " Owner ",
		RequesterAvatarURL:   " mxc://example.com/owner ",
		Remark:               " friend ",
		Dissolved:            true,
		AccountDeleted:       true,
		DeletedMXID:          "@owner:example.com",
	})

	if event.Type != RoomProfileEventType || event.StateKey != "" {
		t.Fatalf("expected room profile state event, got %#v", event)
	}
	if event.Content["room_type"] != RoomTypeDirect ||
		event.Content["name"] != "Alice" ||
		event.Content["requester_mxid"] != "@owner:example.com" ||
		event.Content["target_mxid"] != "@alice:remote.example" ||
		event.Content["domain"] != "example.com" ||
		event.Content["remark"] != "friend" ||
		event.Content["dissolved"] != true ||
		event.Content["account_deleted"] != true ||
		event.Content["deleted_mxid"] != "@owner:example.com" {
		t.Fatalf("unexpected direct profile content: %#v", event.Content)
	}
}

func TestGroupAndChannelRoomProfilesApplyDefaults(t *testing.T) {
	group := GroupRoomProfile(GroupProfile{
		RoomID:       "!group:example.com",
		Name:         "Group",
		InvitePolicy: " owner ",
		Muted:        true,
	}, true)
	if group.Content["room_type"] != RoomTypeGroup ||
		group.Content["invite_policy"] != "owner" ||
		group.Content["dissolved"] != true {
		t.Fatalf("unexpected group profile content: %#v", group.Content)
	}
	defaultGroup := GroupRoomProfile(GroupProfile{RoomID: "!group:example.com"}, false)
	if defaultGroup.Content["invite_policy"] != "member" {
		t.Fatalf("expected default group invite policy, got %#v", defaultGroup.Content)
	}

	channel := ChannelRoomProfile(dirextalkdomain.Channel{
		ChannelID:       "ch",
		RoomID:          "!channel:example.com",
		Name:            "Channel",
		Visibility:      " public ",
		JoinPolicy:      " open ",
		ChannelType:     " chat ",
		CommentsEnabled: true,
	}, false)
	if channel.Content["room_type"] != RoomTypeChannel ||
		channel.Content["visibility"] != "public" ||
		channel.Content["join_policy"] != "open" ||
		channel.Content["channel_type"] != "chat" ||
		channel.Content["comments_enabled"] != true {
		t.Fatalf("unexpected channel profile content: %#v", channel.Content)
	}
	defaultChannel := ChannelRoomProfile(dirextalkdomain.Channel{RoomID: "!channel:example.com"}, false)
	if defaultChannel.Content["visibility"] != "private" ||
		defaultChannel.Content["join_policy"] != "invite" ||
		defaultChannel.Content["channel_type"] != "post" {
		t.Fatalf("expected default channel profile content, got %#v", defaultChannel.Content)
	}
}

func TestChannelPostSettingsUsesPostStateKeyAndFullSettings(t *testing.T) {
	at := time.Date(2026, 8, 7, 9, 30, 0, 123, time.UTC)
	event := ChannelPostSettings(ChannelPostSettingsInput{
		PostID: " post_1 ", ChannelID: " ch_1 ", RoomID: " !channel:example.com ",
		PostEventID: " $post ", Visibility: " public ", CommentsEnabled: false, UpdatedAt: at,
	})
	if event.Type != ChannelPostSettingsEventType || event.StateKey != "post_1" ||
		event.Content["post_id"] != "post_1" || event.Content["channel_id"] != "ch_1" ||
		event.Content["room_id"] != "!channel:example.com" || event.Content["post_event_id"] != "$post" ||
		event.Content["visibility"] != "public" || event.Content["comments_enabled"] != false ||
		event.Content["updated_at"] != at.Format(time.RFC3339Nano) {
		t.Fatalf("unexpected channel post settings state: %#v", event)
	}
}

func TestMemberPolicyAndJoinRequestState(t *testing.T) {
	memberPolicy := MemberPolicyState(dirextalkdomain.MemberRecord{
		RoomID: "!room:example.com",
		UserID: "@alice:example.com",
		Role:   " owner ",
		Muted:  true,
	})
	if memberPolicy.Type != MemberPolicyEventType ||
		memberPolicy.StateKey != productpolicy.UserStateKey("@alice:example.com") ||
		memberPolicy.Content["role"] != "owner" ||
		memberPolicy.Content["muted"] != true {
		t.Fatalf("unexpected member policy state: %#v", memberPolicy)
	}

	at := time.Date(2026, 7, 8, 12, 0, 0, 123, time.UTC)
	joinRequest := JoinRequestState("!room:example.com", "@alice:example.com", " Approved ", " ok ", " request-1 ", at)
	if joinRequest.Type != JoinRequestEventType ||
		joinRequest.StateKey != productpolicy.UserStateKey("@alice:example.com") ||
		joinRequest.Content["status"] != "approved" ||
		joinRequest.Content["room_id"] != "!room:example.com" ||
		joinRequest.Content["user_id"] != "@alice:example.com" ||
		joinRequest.Content["reason"] != "ok" ||
		joinRequest.Content["request_id"] != "request-1" ||
		joinRequest.Content["created_at"] != at.Format(time.RFC3339Nano) ||
		joinRequest.Content["updated_at"] != at.Format(time.RFC3339Nano) {
		t.Fatalf("unexpected join request state: %#v", joinRequest)
	}
}
