package groups

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
)

type testStore struct {
	groups map[string]dirextalkdomain.GroupRecord
	events *[]string
}

func (s *testStore) UpsertGroup(_ context.Context, group dirextalkdomain.GroupRecord) error {
	*s.events = append(*s.events, "group.save")
	s.groups[group.RoomID] = group
	return nil
}

func (s *testStore) DeleteGroup(_ context.Context, roomID string) error {
	*s.events = append(*s.events, "group.delete")
	delete(s.groups, roomID)
	return nil
}

func (s *testStore) ListGroups(context.Context) ([]dirextalkdomain.GroupRecord, error) {
	return s.records(), nil
}

func (s *testStore) GetGroupByRoom(_ context.Context, roomID string) (dirextalkdomain.GroupRecord, bool, error) {
	group, ok := s.groups[roomID]
	return group, ok, nil
}

func (s *testStore) ListJoinedGroupsForUser(context.Context, string) ([]dirextalkdomain.GroupRecord, error) {
	return s.records(), nil
}

func (s *testStore) records() []dirextalkdomain.GroupRecord {
	result := make([]dirextalkdomain.GroupRecord, 0, len(s.groups))
	for _, group := range s.groups {
		result = append(result, group)
	}
	return result
}

type testConversation struct{ events *[]string }

func (c testConversation) Save(context.Context, dirextalkdomain.ConversationRecord) error {
	*c.events = append(*c.events, "conversation.save")
	return nil
}

func (c testConversation) DeleteKindByRoom(context.Context, string, dirextalkdomain.ConversationKind) error {
	*c.events = append(*c.events, "conversation.delete")
	return nil
}

func (c testConversation) Operation(_ context.Context, action, status, roomID string) (map[string]any, *dirextalkdomain.ConversationView, error) {
	*c.events = append(*c.events, "conversation.operation")
	return map[string]any{"action": action, "status": status, "room_id": roomID}, &dirextalkdomain.ConversationView{MatrixRoomID: roomID}, nil
}

func TestGroupHandlersHappyWorkflow(t *testing.T) {
	ctx := context.Background()
	events := []string{}
	store := &testStore{groups: map[string]dirextalkdomain.GroupRecord{}, events: &events}
	module := New(store, testConversation{events: &events}, Config{
		SaveOwnerMember: func(context.Context, string) error {
			events = append(events, "owner.save")
			return nil
		},
		PublishState: func(_ context.Context, _ View, dissolved bool) error {
			if dissolved {
				events = append(events, "state.dissolved")
			} else {
				events = append(events, "state.active")
			}
			return nil
		},
		SetMemberMute: func(_ context.Context, _ string, muted bool) *actionbase.Error {
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
		check  func(t *testing.T, result any)
	}{
		{actionCreate, map[string]any{"room_id": "!group:example.com", "group_name": "Team"}, func(t *testing.T, result any) {
			group := result.(View)
			if group.Name != "Team" || group.InvitePolicy != "member" || group.Operation["action"] != actionCreate || group.Conversation == nil {
				t.Fatalf("create result = %#v", group)
			}
		}},
		{actionUpdate, map[string]any{"room_id": "!group:example.com", "topic": "Updated", "avatar_url": ""}, func(t *testing.T, result any) {
			group := result.(View)
			if group.Name != "Team" || group.Topic != "Updated" || group.AvatarURL != "" {
				t.Fatalf("partial update = %#v", group)
			}
		}},
		{actionList, nil, func(t *testing.T, result any) {
			groups := result.(map[string]any)["groups"].([]View)
			if len(groups) != 1 || groups[0].RoomID != "!group:example.com" {
				t.Fatalf("list result = %#v", groups)
			}
		}},
		{actionInvitePolicyUpdate, map[string]any{"room_id": "!group:example.com", "invite_policy": "owner"}, func(t *testing.T, result any) {
			if result.(View).InvitePolicy != "owner" {
				t.Fatalf("policy result = %#v", result)
			}
		}},
		{actionMute, map[string]any{"room_id": "!group:example.com"}, func(t *testing.T, result any) {
			if result.(map[string]any)["muted"] != true {
				t.Fatalf("mute result = %#v", result)
			}
		}},
		{actionUnmute, map[string]any{"room_id": "!group:example.com"}, func(t *testing.T, result any) {
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

	wantCreatePrefix := []string{"group.save", "conversation.save", "owner.save", "state.active", "conversation.operation"}
	if !reflect.DeepEqual(events[:len(wantCreatePrefix)], wantCreatePrefix) {
		t.Fatalf("create order = %v, want prefix %v", events, wantCreatePrefix)
	}
	if got := store.groups["!group:example.com"]; got.InvitePolicy != "owner" || got.Muted {
		t.Fatalf("final durable group = %#v", got)
	}
}

func TestDissolvePublishesBeforeDeleteAndStopsOnPublishFailure(t *testing.T) {
	for _, publishFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "publish failure"}[publishFails], func(t *testing.T) {
			events := []string{}
			roomID := "!group:example.com"
			store := &testStore{
				groups: map[string]dirextalkdomain.GroupRecord{roomID: {RoomID: roomID, Name: "Team"}},
				events: &events,
			}
			module := New(store, testConversation{events: &events}, Config{
				RequireOwner: func(context.Context, string) *actionbase.Error {
					events = append(events, "owner.require")
					return nil
				},
				PublishState: func(context.Context, View, bool) error {
					events = append(events, "state.dissolved")
					if publishFails {
						return errors.New("publish failed")
					}
					return nil
				},
			})

			_, actionErr := module.Dissolve(context.Background(), map[string]any{"room_id": roomID})
			if publishFails {
				if actionErr == nil {
					t.Fatal("Dissolve() succeeded after publish failure")
				}
				if _, exists := store.groups[roomID]; !exists {
					t.Fatal("group deleted after publish failure")
				}
				if want := []string{"owner.require", "state.dissolved"}; !reflect.DeepEqual(events, want) {
					t.Fatalf("failure order = %v, want %v", events, want)
				}
				return
			}
			if actionErr != nil {
				t.Fatalf("Dissolve() error = %#v", actionErr)
			}
			if want := []string{"owner.require", "state.dissolved", "group.delete", "conversation.delete"}; !reflect.DeepEqual(events, want) {
				t.Fatalf("success order = %v, want %v", events, want)
			}
		})
	}
}
