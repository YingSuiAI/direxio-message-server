package channels

import (
	"context"
	"reflect"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

type testStore struct {
	channels map[string]Channel
	events   *[]string
}

func (s *testStore) UpsertChannel(_ context.Context, channel Channel) error {
	*s.events = append(*s.events, "channel.save")
	s.channels[channel.ChannelID] = channel
	return nil
}

func (s *testStore) DeleteChannel(_ context.Context, channelID string) error {
	*s.events = append(*s.events, "channel.delete")
	delete(s.channels, channelID)
	return nil
}

func (s *testStore) ListChannels(context.Context) ([]Channel, error) {
	return s.records(), nil
}

func (s *testStore) GetChannelByIDOrRoom(_ context.Context, channelID, roomID string) (Channel, bool, error) {
	for _, channel := range s.channels {
		if channelID != "" && channel.ChannelID == channelID || roomID != "" && channel.RoomID == roomID {
			return channel, true, nil
		}
	}
	return Channel{}, false, nil
}

func (s *testStore) ListJoinedChannelsForUser(context.Context, string) ([]Channel, error) {
	return s.records(), nil
}

func (s *testStore) SearchPublicChannels(context.Context, string, int) ([]Channel, error) {
	return s.records(), nil
}

func (s *testStore) ListPublicChannelsForOwner(context.Context, string) ([]Channel, error) {
	return s.records(), nil
}

func (s *testStore) records() []Channel {
	result := make([]Channel, 0, len(s.channels))
	for _, channel := range s.channels {
		result = append(result, channel)
	}
	return result
}

type testConversation struct{ events *[]string }

func (c testConversation) Save(context.Context, dirextalkdomain.ConversationRecord) error {
	*c.events = append(*c.events, "conversation.save")
	return nil
}

func TestChannelHandlersHappyWorkflow(t *testing.T) {
	ctx := context.Background()
	events := []string{}
	store := &testStore{channels: map[string]Channel{}, events: &events}
	module := New(store, testConversation{events: &events}, nil, Config{
		SaveOwnerMember: func(context.Context, string, string) error {
			events = append(events, "owner.save")
			return nil
		},
		PublishHistory: func(context.Context, Channel) error {
			events = append(events, "history.publish")
			return nil
		},
		PublishState: func(_ context.Context, _ Channel, dissolved bool) error {
			if dissolved {
				events = append(events, "state.dissolved")
			} else {
				events = append(events, "state.active")
			}
			return nil
		},
		SetMemberMute: func(_ context.Context, _, _ string, muted bool) *actionbase.Error {
			if muted {
				events = append(events, "members.mute")
			} else {
				events = append(events, "members.unmute")
			}
			return nil
		},
		RequireOwner: func(context.Context, string) *actionbase.Error {
			events = append(events, "owner.require")
			return nil
		},
		OwnerMXID: func() string { return "@owner:example.com" },
	})
	handlers := module.Handlers()

	steps := []struct {
		action string
		params map[string]any
		check  func(*testing.T, any)
	}{
		{actionCreate, map[string]any{"channel_id": "ch_1", "room_id": "!channel:example.com", "comments_enabled": false}, func(t *testing.T, result any) {
			channel := result.(Channel)
			if channel.Name != "ch_1" || channel.Visibility != "public" || channel.JoinPolicy != "open" || channel.ChannelType != "post" || channel.CommentsEnabled || channel.MemberCount != 1 || !channel.IsOwned || channel.Role != "owner" || channel.MemberStatus != "join" {
				t.Fatalf("create result = %#v", channel)
			}
		}},
		{actionUpdate, map[string]any{"channel_id": "ch_1", "name": "News", "description": "Updated", "avatar_url": ""}, func(t *testing.T, result any) {
			channel := result.(Channel)
			if channel.Name != "News" || channel.Description != "Updated" || channel.AvatarURL != "" || channel.JoinPolicy != "open" {
				t.Fatalf("partial update = %#v", channel)
			}
		}},
		{actionList, nil, func(t *testing.T, result any) {
			channels := result.(map[string]any)["channels"].([]Channel)
			if len(channels) != 1 || channels[0].ChannelID != "ch_1" {
				t.Fatalf("list result = %#v", channels)
			}
		}},
		{actionMute, map[string]any{"room_id": "!channel:example.com"}, func(t *testing.T, result any) {
			if result.(map[string]any)["muted"] != true {
				t.Fatalf("mute result = %#v", result)
			}
		}},
		{actionUnmute, map[string]any{"channel_id": "ch_1"}, func(t *testing.T, result any) {
			if result.(map[string]any)["muted"] != false {
				t.Fatalf("unmute result = %#v", result)
			}
		}},
	}
	for _, step := range steps {
		t.Run(step.action, func(t *testing.T) {
			result, actionErr := handlers[step.action](ctx, step.params)
			if actionErr != nil {
				t.Fatalf("handler error = %#v", actionErr)
			}
			step.check(t, result)
		})
	}

	wantCreatePrefix := []string{"channel.save", "conversation.save", "owner.save", "history.publish"}
	if !reflect.DeepEqual(events[:len(wantCreatePrefix)], wantCreatePrefix) {
		t.Fatalf("create order = %v, want prefix %v", events, wantCreatePrefix)
	}
	if got := store.channels["ch_1"]; got.Name != "News" || got.Muted {
		t.Fatalf("final durable channel = %#v", got)
	}
}

func TestDissolvePublishesBeforeDelete(t *testing.T) {
	events := []string{}
	channel := Channel{ChannelID: "ch_1", RoomID: "!channel:example.com", Name: "News"}
	store := &testStore{channels: map[string]Channel{channel.ChannelID: channel}, events: &events}
	module := New(store, testConversation{events: &events}, nil, Config{
		RequireOwner: func(context.Context, string) *actionbase.Error {
			events = append(events, "owner.require")
			return nil
		},
		PublishState: func(context.Context, Channel, bool) error {
			events = append(events, "state.dissolved")
			return nil
		},
	})

	result, actionErr := module.Dissolve(context.Background(), map[string]any{"channel_id": channel.ChannelID})
	if actionErr != nil {
		t.Fatalf("Dissolve() error = %#v", actionErr)
	}
	if result.(map[string]any)["status"] != "ok" {
		t.Fatalf("Dissolve() result = %#v", result)
	}
	if want := []string{"owner.require", "state.dissolved", "channel.delete"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("dissolve order = %v, want %v", events, want)
	}
}

func TestContentRecordConversionsExcludeResponseOnlyFields(t *testing.T) {
	conversation := &dirextalkdomain.ConversationView{}

	post := postFromRecord(postRecord(Post{
		PostID: "post", ChannelID: "channel", Visibility: "public", CommentCount: 2,
		ReactionCount: 3, ReactedByMe: true,
		Operation: map[string]any{"action": "test"}, Conversation: conversation,
	}))
	if post.PostID != "post" || post.ChannelID != "channel" || post.Visibility != "public" || post.CommentCount != 2 || post.ReactionCount != 0 || post.ReactedByMe || post.Operation != nil || post.Conversation != nil {
		t.Fatalf("post round trip = %#v", post)
	}

	comment := commentFromRecord(commentRecord(Comment{
		CommentID: "comment", PostID: "post", MentionsJSON: `["@user:example.com"]`,
		ReactionCount: 3, ReactedByMe: true,
		Operation: map[string]any{"action": "test"}, Conversation: conversation,
	}))
	if comment.CommentID != "comment" || comment.PostID != "post" || comment.MentionsJSON != `["@user:example.com"]` || comment.ReactionCount != 0 || comment.ReactedByMe || comment.Operation != nil || comment.Conversation != nil {
		t.Fatalf("comment round trip = %#v", comment)
	}
}
