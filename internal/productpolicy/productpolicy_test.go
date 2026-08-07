package productpolicy

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/roomserver/api"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/types"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

type stateQuerier struct {
	state          map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent
	events         map[string]*types.HeaderedEvent
	pendingInvites map[string]bool
	senderUsers    map[spec.SenderID]*spec.UserID
	senderQueryErr error
}

func (q stateQuerier) QueryCurrentState(ctx context.Context, req *api.QueryCurrentStateRequest, res *api.QueryCurrentStateResponse) error {
	res.StateEvents = map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{}
	for _, tuple := range req.StateTuples {
		if req.AllowWildcards && tuple.StateKey == "*" {
			for stateTuple, event := range q.state {
				if stateTuple.EventType == tuple.EventType {
					res.StateEvents[stateTuple] = event
				}
			}
			continue
		}
		if event := q.state[tuple]; event != nil {
			res.StateEvents[tuple] = event
		}
	}
	return nil
}

func (q stateQuerier) QueryMembershipForUser(ctx context.Context, req *api.QueryMembershipForUserRequest, res *api.QueryMembershipForUserResponse) error {
	event := q.state[gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomMember, StateKey: req.UserID.String()}]
	if event == nil {
		if q.pendingInvites[req.RoomID+"|"+req.UserID.String()] {
			res.RoomExists = true
		}
		return nil
	}
	content := map[string]any{}
	if err := json.Unmarshal(event.Content(), &content); err != nil {
		return err
	}
	res.RoomExists = true
	res.HasBeenInRoom = true
	res.Membership = stringValue(content["membership"])
	res.IsInRoom = res.Membership == spec.Join
	return nil
}

func (q stateQuerier) InvitePending(ctx context.Context, roomID spec.RoomID, senderID spec.SenderID) (bool, error) {
	return q.pendingInvites[roomID.String()+"|"+string(senderID)], nil
}

func (q stateQuerier) QueryEventsByID(ctx context.Context, req *api.QueryEventsByIDRequest, res *api.QueryEventsByIDResponse) error {
	for _, eventID := range req.EventIDs {
		if event := q.events[eventID]; event != nil {
			res.Events = append(res.Events, event)
		}
	}
	return nil
}

func (q stateQuerier) QueryUserIDForSender(_ context.Context, _ spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
	if q.senderQueryErr != nil {
		return nil, q.senderQueryErr
	}
	return q.senderUsers[senderID], nil
}

func TestValidateClientEventRejectsChannelCommentWhenCommentsDisabled(t *testing.T) {
	roomID := "!channel:example.com"
	userID := "@member:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, "@owner:example.com", DirextalkRoomProfileEventType, "", map[string]any{
			"room_type":        DirextalkRoomTypeChannel,
			"comments_enabled": false,
		}),
		{EventType: spec.MRoomMember, StateKey: userID}: stateEvent(t, roomID, userID, spec.MRoomMember, userID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID:     roomID,
		SenderMXID: userID,
		EventType:  "m.room.message",
		Content:    map[string]any{"msgtype": "m.text", "body": "blocked", "p2p_kind": "channel_comment"},
	})

	if err == nil {
		t.Fatalf("expected product policy to reject channel comment when comments are disabled")
	}
}

func TestValidateClientEventRejectsOnlyDisabledChannelPostComments(t *testing.T) {
	roomID := "!channel:example.com"
	userID := "@member:example.com"
	disabledPostID := "post_disabled"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, "@owner:example.com", DirextalkRoomProfileEventType, "", map[string]any{
			"room_type": DirextalkRoomTypeChannel, "comments_enabled": true,
		}),
		{EventType: DirextalkChannelPostSettingsEventType, StateKey: disabledPostID}: stateEvent(t, roomID, "@owner:example.com", DirextalkChannelPostSettingsEventType, disabledPostID, map[string]any{
			"post_id": disabledPostID, "comments_enabled": false,
		}),
		{EventType: spec.MRoomMember, StateKey: userID}: stateEvent(t, roomID, userID, spec.MRoomMember, userID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	blocked := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID: roomID, SenderMXID: userID, EventType: "m.room.message",
		Content: map[string]any{"msgtype": "m.text", "body": "blocked", "p2p_kind": "channel_comment", "post_id": disabledPostID},
	})
	if blocked == nil {
		t.Fatal("expected disabled post comment to be rejected")
	}
	allowed := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID: roomID, SenderMXID: userID, EventType: "m.room.message",
		Content: map[string]any{"msgtype": "m.text", "body": "allowed", "p2p_kind": "channel_comment", "post_id": "post_enabled"},
	})
	if allowed != nil {
		t.Fatalf("another post without a disabling state must remain commentable: %v", allowed)
	}
	reaction := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID: roomID, SenderMXID: userID, EventType: "m.reaction",
		Content: map[string]any{"post_id": disabledPostID, "reaction": "like"},
	})
	if reaction != nil {
		t.Fatalf("post comment setting must not block reactions: %v", reaction)
	}
}

func TestValidateClientEventAllowsPlainChannelMessageWhenCommentsDisabled(t *testing.T) {
	roomID := "!channel:example.com"
	userID := "@member:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, "@owner:example.com", DirextalkRoomProfileEventType, "", map[string]any{
			"room_type":        DirextalkRoomTypeChannel,
			"comments_enabled": false,
		}),
		{EventType: spec.MRoomMember, StateKey: userID}: stateEvent(t, roomID, userID, spec.MRoomMember, userID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID:     roomID,
		SenderMXID: userID,
		EventType:  "m.room.message",
		Content:    map[string]any{"msgtype": "m.text", "body": "allowed"},
	})

	if err != nil {
		t.Fatalf("expected plain channel message to ignore comments_enabled, got %v", err)
	}
}

func TestValidateClientEventRejectsChannelPostFromMember(t *testing.T) {
	roomID := "!channel:example.com"
	memberID := "@member:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, "@owner:example.com", DirextalkRoomProfileEventType, "", map[string]any{
			"room_type":        DirextalkRoomTypeChannel,
			"comments_enabled": true,
		}),
		{EventType: spec.MRoomMember, StateKey: memberID}: stateEvent(t, roomID, memberID, spec.MRoomMember, memberID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID:     roomID,
		SenderMXID: memberID,
		EventType:  "m.room.message",
		Content:    map[string]any{"msgtype": "m.text", "body": "fake post", "p2p_kind": "channel_post"},
	})

	if err == nil {
		t.Fatalf("expected product policy to reject channel_post from ordinary member")
	}
}

func TestValidateClientEventAllowsChannelPostFromOwner(t *testing.T) {
	roomID := "!channel:example.com"
	ownerID := "@owner:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: spec.MRoomCreate, StateKey: ""}: stateEvent(t, roomID, ownerID, spec.MRoomCreate, "", map[string]any{
			"creator": ownerID,
			"type":    DirextalkRoomTypeChannel,
		}),
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, ownerID, DirextalkRoomProfileEventType, "", map[string]any{
			"room_type":        DirextalkRoomTypeChannel,
			"comments_enabled": true,
		}),
		{EventType: spec.MRoomMember, StateKey: ownerID}: stateEvent(t, roomID, ownerID, spec.MRoomMember, ownerID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID:     roomID,
		SenderMXID: ownerID,
		EventType:  "m.room.message",
		Content:    map[string]any{"msgtype": "m.text", "body": "post", "p2p_kind": "channel_post"},
	})

	if err != nil {
		t.Fatalf("expected channel owner to create channel_post, got %v", err)
	}
}

func TestValidateClientEventRejectsSpoofedCreateContentOwner(t *testing.T) {
	roomID := "!channel:example.com"
	memberID := "@member:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: spec.MRoomCreate, StateKey: ""}: stateEvent(t, roomID, "@actual-owner:example.com", spec.MRoomCreate, "", map[string]any{
			"creator": memberID,
			"type":    DirextalkRoomTypeChannel,
		}),
		{EventType: spec.MRoomMember, StateKey: memberID}: stateEvent(t, roomID, memberID, spec.MRoomMember, memberID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID: roomID, SenderMXID: memberID, EventType: "m.room.message",
		Content: map[string]any{"msgtype": "m.text", "body": "spoofed", "p2p_kind": "channel_post"},
	})
	if err == nil {
		t.Fatal("spoofed m.room.create content.creator granted owner policy")
	}
}

func TestValidateClientEventAllowsResolvedPseudoCreateSenderOwner(t *testing.T) {
	roomID := "!channel:example.com"
	ownerID := "@owner:example.com"
	pseudoSender := spec.SenderIDFromPseudoIDKey(ed25519.NewKeyFromSeed(make([]byte, 32)))
	resolvedOwner, err := spec.NewUserID(ownerID, true)
	if err != nil {
		t.Fatal(err)
	}
	querier := stateQuerier{
		state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
			{EventType: spec.MRoomCreate, StateKey: ""}: stateEvent(t, roomID, string(pseudoSender), spec.MRoomCreate, "", map[string]any{
				"creator": "@spoofed:example.com",
				"type":    DirextalkRoomTypeChannel,
			}),
			{EventType: spec.MRoomMember, StateKey: ownerID}: stateEvent(t, roomID, ownerID, spec.MRoomMember, ownerID, map[string]any{
				"membership": spec.Join,
			}),
		},
		senderUsers: map[spec.SenderID]*spec.UserID{pseudoSender: resolvedOwner},
	}

	err = ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID: roomID, SenderMXID: ownerID, EventType: "m.room.message",
		Content: map[string]any{"msgtype": "m.text", "body": "post", "p2p_kind": "channel_post"},
	})
	if err != nil {
		t.Fatalf("resolved pseudo create sender did not receive owner policy: %v", err)
	}
}

func TestValidateClientEventCanonicalizesOwnerRoleToCreateSender(t *testing.T) {
	const (
		roomID  = "!channel:example.com"
		creator = "@creator:example.com"
		member  = "@member:example.com"
	)
	for _, tc := range []struct {
		name         string
		senderMXID   string
		declaredRole string
		wantAllowed  bool
	}{
		{name: "member policy cannot promote non creator", senderMXID: member, declaredRole: "owner"},
		{name: "member policy cannot demote creator", senderMXID: creator, declaredRole: "member", wantAllowed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
				{EventType: spec.MRoomCreate, StateKey: ""}: stateEvent(t, roomID, creator, spec.MRoomCreate, "", map[string]any{
					"type": DirextalkRoomTypeChannel,
				}),
				{EventType: DirextalkMemberPolicyEventType, StateKey: UserStateKey(tc.senderMXID)}: stateEvent(t, roomID, creator, DirextalkMemberPolicyEventType, UserStateKey(tc.senderMXID), map[string]any{
					"role": tc.declaredRole,
				}),
				{EventType: spec.MRoomMember, StateKey: tc.senderMXID}: stateEvent(t, roomID, tc.senderMXID, spec.MRoomMember, tc.senderMXID, map[string]any{
					"membership": spec.Join,
				}),
			}}

			err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
				RoomID: roomID, SenderMXID: tc.senderMXID, EventType: "m.room.message",
				Content: map[string]any{"msgtype": "m.text", "body": "post", "p2p_kind": "channel_post"},
			})
			if tc.wantAllowed && err != nil {
				t.Fatalf("create sender lost owner policy: %v", err)
			}
			if !tc.wantAllowed && err == nil {
				t.Fatal("non-creator member policy role granted owner privileges")
			}
		})
	}
}

func TestValidateClientEventIgnoresRoomProfileMutedForOwner(t *testing.T) {
	roomID := "!group:example.com"
	ownerID := "@owner:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: spec.MRoomCreate, StateKey: ""}: stateEvent(t, roomID, ownerID, spec.MRoomCreate, "", map[string]any{
			"creator": ownerID,
			"type":    DirextalkRoomTypeGroup,
		}),
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, ownerID, DirextalkRoomProfileEventType, "", map[string]any{
			"room_type": DirextalkRoomTypeGroup,
			"muted":     true,
		}),
		{EventType: spec.MRoomMember, StateKey: ownerID}: stateEvent(t, roomID, ownerID, spec.MRoomMember, ownerID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID:     roomID,
		SenderMXID: ownerID,
		EventType:  "m.room.message",
		Content:    map[string]any{"msgtype": "m.text", "body": "allowed"},
	})

	if err != nil {
		t.Fatalf("expected room profile muted to be non-authoritative for sender mute, got %v", err)
	}
}

func TestValidateClientEventRejectsMemberPolicyMuted(t *testing.T) {
	roomID := "!group:example.com"
	userID := "@member:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, "@owner:example.com", DirextalkRoomProfileEventType, "", map[string]any{
			"room_type": DirextalkRoomTypeGroup,
		}),
		{EventType: DirextalkMemberPolicyEventType, StateKey: UserStateKey(userID)}: stateEvent(t, roomID, "@owner:example.com", DirextalkMemberPolicyEventType, UserStateKey(userID), map[string]any{
			"role":    "member",
			"muted":   true,
			"user_id": userID,
		}),
		{EventType: spec.MRoomMember, StateKey: userID}: stateEvent(t, roomID, userID, spec.MRoomMember, userID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID:     roomID,
		SenderMXID: userID,
		EventType:  "m.room.message",
		Content:    map[string]any{"msgtype": "m.text", "body": "blocked"},
	})

	if err == nil {
		t.Fatalf("expected member policy muted to reject sender")
	}
}

func TestValidateClientRedactionRejectsMemberRedactingAnotherSender(t *testing.T) {
	roomID := "!channel:example.com"
	ownerID := "@owner:example.com"
	memberID := "@member:example.com"
	target := timelineEvent(t, roomID, ownerID, "m.room.message", map[string]any{"msgtype": "m.text", "body": "owned"})
	querier := stateQuerier{
		state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
			{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, ownerID, DirextalkRoomProfileEventType, "", map[string]any{
				"room_type":        DirextalkRoomTypeChannel,
				"comments_enabled": true,
			}),
			{EventType: spec.MRoomMember, StateKey: memberID}: stateEvent(t, roomID, memberID, spec.MRoomMember, memberID, map[string]any{
				"membership": spec.Join,
			}),
		},
		events: map[string]*types.HeaderedEvent{target.EventID(): target},
	}

	err := ValidateClientRedaction(context.Background(), querier, ClientRedactionRequest{
		RoomID:        roomID,
		SenderMXID:    memberID,
		TargetEventID: target.EventID(),
	})

	if err == nil {
		t.Fatalf("expected product policy to reject member redacting another sender")
	}
}

func TestValidateClientMembershipRejectsMemberInviteWhenOwnerOnly(t *testing.T) {
	roomID := "!group:example.com"
	ownerID := "@owner:example.com"
	memberID := "@member:example.com"
	inviteeID := "@invitee:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, ownerID, DirextalkRoomProfileEventType, "", map[string]any{
			"room_type":     DirextalkRoomTypeGroup,
			"invite_policy": "owner",
		}),
		{EventType: spec.MRoomMember, StateKey: memberID}: stateEvent(t, roomID, memberID, spec.MRoomMember, memberID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientMembership(context.Background(), querier, ClientMembershipRequest{
		RoomID:     roomID,
		SenderMXID: memberID,
		TargetMXID: inviteeID,
		Membership: spec.Invite,
	})

	if err == nil {
		t.Fatalf("expected product policy to reject member invite when invite_policy=owner")
	}
}

func TestValidateClientMembershipRejectsPendingChannelJoinRequest(t *testing.T) {
	roomID := "!channel:example.com"
	ownerID := "@owner:example.com"
	requesterID := "@requester:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, ownerID, DirextalkRoomProfileEventType, "", map[string]any{
			"room_type":   DirextalkRoomTypeChannel,
			"join_policy": "approval",
		}),
		{EventType: DirextalkJoinRequestEventType, StateKey: UserStateKey(requesterID)}: stateEvent(t, roomID, ownerID, DirextalkJoinRequestEventType, UserStateKey(requesterID), map[string]any{
			"status":  "pending",
			"user_id": requesterID,
		}),
	}}

	err := ValidateClientMembership(context.Background(), querier, ClientMembershipRequest{
		RoomID:     roomID,
		SenderMXID: requesterID,
		TargetMXID: requesterID,
		Membership: spec.Join,
	})

	if err == nil {
		t.Fatalf("expected product policy to reject channel join before approval")
	}
}

func TestValidateClientMembershipAllowsApprovedChannelJoinRequest(t *testing.T) {
	roomID := "!channel:example.com"
	ownerID := "@owner:example.com"
	requesterID := "@requester:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, ownerID, DirextalkRoomProfileEventType, "", map[string]any{
			"room_type":   DirextalkRoomTypeChannel,
			"join_policy": "approval",
		}),
		{EventType: DirextalkJoinRequestEventType, StateKey: UserStateKey(requesterID)}: stateEvent(t, roomID, ownerID, DirextalkJoinRequestEventType, UserStateKey(requesterID), map[string]any{
			"status":  "approved",
			"user_id": requesterID,
		}),
	}}

	err := ValidateClientMembership(context.Background(), querier, ClientMembershipRequest{
		RoomID:     roomID,
		SenderMXID: requesterID,
		TargetMXID: requesterID,
		Membership: spec.Join,
	})

	if err != nil {
		t.Fatalf("expected product policy to allow approved channel join request, got %v", err)
	}
}

func TestValidateClientEventAllowsNonProductRoom(t *testing.T) {
	err := ValidateClientEvent(context.Background(), stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{}}, ClientEventRequest{
		RoomID:     "!plain:example.com",
		SenderMXID: "@user:example.com",
		EventType:  "m.room.message",
		Content:    map[string]any{"msgtype": "m.text", "body": "plain"},
	})
	if err != nil {
		t.Fatalf("expected non-product room to bypass product policy, got %v", err)
	}
}

func TestValidateClientEventIgnoresLegacyProductState(t *testing.T) {
	roomID := "!legacy-channel:example.com"
	userID := "@member:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: "p2p.room.kind", StateKey: ""}: stateEvent(t, roomID, "@owner:example.com", "p2p.room.kind", "", map[string]any{
			"kind":             "channel",
			"comments_enabled": false,
		}),
		{EventType: spec.MRoomMember, StateKey: userID}: stateEvent(t, roomID, userID, spec.MRoomMember, userID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID:     roomID,
		SenderMXID: userID,
		EventType:  "m.room.message",
		Content:    map[string]any{"msgtype": "m.text", "body": "blocked", "p2p_kind": "channel_comment"},
	})

	if err != nil {
		t.Fatalf("expected legacy P2P product state to be ignored by product policy, got %v", err)
	}
}

func TestValidateClientEventRejectsDirectRoomWithoutJoinedPeer(t *testing.T) {
	roomID := "!direct:example.com"
	senderID := "@sender:example.com"
	peerID := "@peer:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: spec.MRoomCreate, StateKey: ""}: stateEvent(t, roomID, senderID, spec.MRoomCreate, "", map[string]any{
			"creator": senderID,
			"type":    DirextalkRoomTypeDirect,
		}),
		{EventType: spec.MRoomMember, StateKey: senderID}: stateEvent(t, roomID, senderID, spec.MRoomMember, senderID, map[string]any{
			"membership": spec.Join,
		}),
		{EventType: spec.MRoomMember, StateKey: peerID}: stateEvent(t, roomID, peerID, spec.MRoomMember, peerID, map[string]any{
			"membership": spec.Invite,
		}),
	}}

	err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID:     roomID,
		SenderMXID: senderID,
		EventType:  "m.room.message",
		Content:    map[string]any{"msgtype": "m.text", "body": "blocked"},
	})

	if err == nil {
		t.Fatalf("expected direct room send to require a joined peer")
	}
}

func TestValidateClientEventAllowsDirectRoomWithJoinedPeer(t *testing.T) {
	roomID := "!direct:example.com"
	senderID := "@sender:example.com"
	peerID := "@peer:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: spec.MRoomCreate, StateKey: ""}: stateEvent(t, roomID, senderID, spec.MRoomCreate, "", map[string]any{
			"creator": senderID,
			"type":    DirextalkRoomTypeDirect,
		}),
		{EventType: spec.MRoomMember, StateKey: senderID}: stateEvent(t, roomID, senderID, spec.MRoomMember, senderID, map[string]any{
			"membership": spec.Join,
		}),
		{EventType: spec.MRoomMember, StateKey: peerID}: stateEvent(t, roomID, peerID, spec.MRoomMember, peerID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientEvent(context.Background(), querier, ClientEventRequest{
		RoomID:     roomID,
		SenderMXID: senderID,
		EventType:  "m.room.message",
		Content:    map[string]any{"msgtype": "m.text", "body": "allowed"},
	})

	if err != nil {
		t.Fatalf("expected direct room send with joined peer to pass, got %v", err)
	}

	blockedReq := ClientEventRequest{
		RoomID: roomID, SenderMXID: senderID, EventType: "m.room.message",
		Content: map[string]any{"msgtype": "m.text", "body": "blocked"},
		BlockChecker: func(_ context.Context, blockedRoomID, blockedSenderID string) (bool, error) {
			return blockedRoomID == roomID && blockedSenderID == senderID, nil
		},
	}
	if err = ValidateClientEvent(context.Background(), querier, blockedReq); err == nil {
		t.Fatalf("expected exact blocked direct sender to be rejected")
	}
	blockedReq.BlockChecker = func(_ context.Context, blockedRoomID, _ string) (bool, error) {
		return blockedRoomID == "!different-direct:example.com", nil
	}
	if err = ValidateClientEvent(context.Background(), querier, blockedReq); err != nil {
		t.Fatalf("room mismatch lookup should allow the direct sender: %v", err)
	}
}

func TestProductRoomTypeScopesDirectRooms(t *testing.T) {
	roomID := "!direct:example.com"
	ordinaryID := "!ordinary:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: spec.MRoomCreate, StateKey: ""}: stateEvent(t, roomID, "@owner:example.com", spec.MRoomCreate, "", map[string]any{
			"creator": "@owner:example.com", "type": DirextalkRoomTypeDirect,
		}),
	}}
	roomType, err := ProductRoomType(context.Background(), querier, roomID)
	if err != nil || roomType != DirextalkRoomTypeDirect {
		t.Fatalf("direct room type = %q, %v", roomType, err)
	}
	roomType, err = ProductRoomType(context.Background(), stateQuerier{}, ordinaryID)
	if err != nil || roomType != "" {
		t.Fatalf("ordinary room type = %q, %v", roomType, err)
	}
}

func TestValidateClientMembershipAllowsDirectPeerReinviteOnly(t *testing.T) {
	roomID := "!direct:example.com"
	requesterID := "@requester:example.com"
	targetID := "@target:example.com"
	thirdID := "@third:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, requesterID, DirextalkRoomProfileEventType, "", map[string]any{
			"room_type":      DirextalkRoomTypeDirect,
			"requester_mxid": requesterID,
			"target_mxid":    targetID,
		}),
		{EventType: spec.MRoomMember, StateKey: requesterID}: stateEvent(t, roomID, requesterID, spec.MRoomMember, requesterID, map[string]any{
			"membership": spec.Leave,
		}),
		{EventType: spec.MRoomMember, StateKey: targetID}: stateEvent(t, roomID, targetID, spec.MRoomMember, targetID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientMembership(context.Background(), querier, ClientMembershipRequest{
		RoomID:     roomID,
		SenderMXID: targetID,
		TargetMXID: requesterID,
		Membership: string(spec.Invite),
	})
	if err != nil {
		t.Fatalf("expected retained direct peer to invite requester back, got %v", err)
	}

	err = ValidateClientMembership(context.Background(), querier, ClientMembershipRequest{
		RoomID:     roomID,
		SenderMXID: targetID,
		TargetMXID: thirdID,
		Membership: string(spec.Invite),
	})
	if err == nil {
		t.Fatalf("expected direct room invite to reject non-peer target")
	}
}

func TestValidateClientMembershipAllowsDirectPriorMemberReinvite(t *testing.T) {
	roomID := "!direct:example.com"
	requesterID := "@requester:example.com"
	targetID := "@target:example.com"
	thirdID := "@third:example.com"
	querier := stateQuerier{state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
		{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, requesterID, DirextalkRoomProfileEventType, "", map[string]any{
			"room_type": DirextalkRoomTypeDirect,
		}),
		{EventType: spec.MRoomMember, StateKey: requesterID}: stateEvent(t, roomID, requesterID, spec.MRoomMember, requesterID, map[string]any{
			"membership": spec.Leave,
		}),
		{EventType: spec.MRoomMember, StateKey: targetID}: stateEvent(t, roomID, targetID, spec.MRoomMember, targetID, map[string]any{
			"membership": spec.Join,
		}),
	}}

	err := ValidateClientMembership(context.Background(), querier, ClientMembershipRequest{
		RoomID:     roomID,
		SenderMXID: targetID,
		TargetMXID: requesterID,
		Membership: string(spec.Invite),
	})
	if err != nil {
		t.Fatalf("expected retained direct peer to re-invite prior room member, got %v", err)
	}

	err = ValidateClientMembership(context.Background(), querier, ClientMembershipRequest{
		RoomID:     roomID,
		SenderMXID: targetID,
		TargetMXID: thirdID,
		Membership: string(spec.Invite),
	})
	if err == nil {
		t.Fatalf("expected direct room invite to reject a user with no prior room membership")
	}
}

func TestValidateClientMembershipAllowsJoinWithPendingInviteTableEntry(t *testing.T) {
	roomID := "!direct:example.com"
	requesterID := "@requester:example.com"
	targetID := "@target:example.com"
	querier := stateQuerier{
		state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
			{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, requesterID, DirextalkRoomProfileEventType, "", map[string]any{
				"room_type":      DirextalkRoomTypeDirect,
				"requester_mxid": requesterID,
				"target_mxid":    targetID,
			}),
			{EventType: spec.MRoomMember, StateKey: targetID}: stateEvent(t, roomID, targetID, spec.MRoomMember, targetID, map[string]any{
				"membership": spec.Join,
			}),
		},
		pendingInvites: map[string]bool{
			roomID + "|" + requesterID: true,
		},
	}

	err := ValidateClientMembership(context.Background(), querier, ClientMembershipRequest{
		RoomID:     roomID,
		SenderMXID: requesterID,
		TargetMXID: requesterID,
		Membership: string(spec.Join),
	})
	if err != nil {
		t.Fatalf("expected pending invite table entry to allow direct room join, got %v", err)
	}
}

func TestValidateClientMembershipAllowsGroupRejoinWithFreshInviteAfterLeaveState(t *testing.T) {
	roomID := "!group:example.com"
	ownerID := "@owner:example.com"
	memberID := "@member:example.com"
	querier := stateQuerier{
		state: map[gomatrixserverlib.StateKeyTuple]*types.HeaderedEvent{
			{EventType: DirextalkRoomProfileEventType, StateKey: ""}: stateEvent(t, roomID, ownerID, DirextalkRoomProfileEventType, "", map[string]any{
				"room_type": DirextalkRoomTypeGroup,
			}),
			{EventType: spec.MRoomMember, StateKey: memberID}: stateEvent(t, roomID, ownerID, spec.MRoomMember, memberID, map[string]any{
				"membership": spec.Leave,
			}),
		},
		pendingInvites: map[string]bool{
			roomID + "|" + memberID: true,
		},
	}

	err := ValidateClientMembership(context.Background(), querier, ClientMembershipRequest{
		RoomID:     roomID,
		SenderMXID: memberID,
		TargetMXID: memberID,
		Membership: string(spec.Join),
	})
	if err != nil {
		t.Fatalf("expected fresh invite to allow group rejoin after leave state, got %v", err)
	}
}

func stateEvent(t *testing.T, roomID, sender, eventType, stateKey string, content map[string]any) *types.HeaderedEvent {
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

func timelineEvent(t *testing.T, roomID, sender, eventType string, content map[string]any) *types.HeaderedEvent {
	t.Helper()
	rawContent, err := json.Marshal(content)
	if err != nil {
		t.Fatal(err)
	}
	raw := fmt.Sprintf(`{"type":%q,"room_id":%q,"sender":%q,"content":%s}`, eventType, roomID, sender, rawContent)
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
