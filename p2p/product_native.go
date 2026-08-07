package p2p

import (
	"context"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkstate"
	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

const (
	DirextalkRoomTypeDirect  = productpolicy.DirextalkRoomTypeDirect
	DirextalkRoomTypeGroup   = productpolicy.DirextalkRoomTypeGroup
	DirextalkRoomTypeChannel = productpolicy.DirextalkRoomTypeChannel
	DirextalkRoomTypeSystem  = "io.dirextalk.room.system"

	DirextalkRoomProfileEventType         = productpolicy.DirextalkRoomProfileEventType
	DirextalkMemberPolicyEventType        = productpolicy.DirextalkMemberPolicyEventType
	DirextalkJoinRequestEventType         = productpolicy.DirextalkJoinRequestEventType
	DirextalkChannelPostSettingsEventType = productpolicy.DirextalkChannelPostSettingsEventType

	DirextalkAgentStatusEventType = "io.dirextalk.agent.status"
	AgentGatewayContentKey        = "io.dirextalk.agent_gateway"
	AgentGatewaySourceContentKey  = "io.dirextalk.gateway_source"
)

type p2pEvent = dirextalkdomain.Event

func roomStateEvent(event dirextalkstate.StateEvent) RoomStateEvent {
	return RoomStateEvent{
		Type:     event.Type,
		StateKey: event.StateKey,
		Content:  event.Content,
	}
}

func dirextalkRoomType(kind string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "direct", "dm", "contact":
		return DirextalkRoomTypeDirect
	case "group":
		return DirextalkRoomTypeGroup
	case "channel":
		return DirextalkRoomTypeChannel
	default:
		return ""
	}
}

func historyVisibilityStateEvent(visibility gomatrixserverlib.HistoryVisibility) RoomStateEvent {
	return RoomStateEvent{
		Type:     spec.MRoomHistoryVisibility,
		StateKey: "",
		Content: map[string]any{
			"history_visibility": string(visibility),
		},
	}
}

func joinedHistoryVisibilityStateEvent() RoomStateEvent {
	return historyVisibilityStateEvent(gomatrixserverlib.HistoryVisibilityJoined)
}

func sharedHistoryVisibilityStateEvent() RoomStateEvent {
	return historyVisibilityStateEvent(gomatrixserverlib.HistoryVisibilityShared)
}

func channelHistoryVisibilityStateEvent(channelType string) (RoomStateEvent, bool) {
	return sharedHistoryVisibilityStateEvent(), true
}

func agentStatusStateEvent(agentMXID string, online bool) RoomStateEvent {
	return RoomStateEvent{
		Type:     DirextalkAgentStatusEventType,
		StateKey: strings.TrimSpace(agentMXID),
		Content: map[string]any{
			"online": online,
		},
	}
}

func agentRoomPowerLevelsStateEvent(ownerMXID, agentMXID string) RoomStateEvent {
	return RoomStateEvent{
		Type:     spec.MRoomPowerLevels,
		StateKey: "",
		Content: map[string]any{
			"users": map[string]any{
				strings.TrimSpace(ownerMXID): 100,
				strings.TrimSpace(agentMXID): 50,
			},
			"users_default":  0,
			"events_default": 0,
			"state_default":  50,
			"ban":            50,
			"kick":           50,
			"redact":         50,
			"invite":         0,
			"events": map[string]any{
				spec.MRoomName:                50,
				spec.MRoomTopic:               50,
				spec.MRoomPowerLevels:         100,
				spec.MRoomHistoryVisibility:   100,
				spec.MRoomCanonicalAlias:      50,
				spec.MRoomAvatar:              50,
				spec.MRoomEncryption:          100,
				"m.room.server_acl":           100,
				DirextalkAgentStatusEventType: 50,
			},
		},
	}
}

func roomProfileForDirect(name, requesterMXID, targetMXID, requesterDisplayName, requesterAvatarURL, remark string, dissolved bool) RoomStateEvent {
	return roomStateEvent(dirextalkstate.DirectRoomProfile(dirextalkstate.DirectRoomProfileInput{
		Name:                 name,
		RequesterMXID:        requesterMXID,
		TargetMXID:           targetMXID,
		RequesterDisplayName: requesterDisplayName,
		RequesterAvatarURL:   requesterAvatarURL,
		Remark:               remark,
		Dissolved:            dissolved,
	}))
}

func accountDeletedDirectProfile(name, ownerMXID, peerMXID, ownerDisplayName, ownerAvatarURL, remark string) RoomStateEvent {
	return roomStateEvent(dirextalkstate.DirectRoomProfile(dirextalkstate.DirectRoomProfileInput{
		Name:                 name,
		RequesterMXID:        ownerMXID,
		TargetMXID:           peerMXID,
		RequesterDisplayName: ownerDisplayName,
		RequesterAvatarURL:   ownerAvatarURL,
		Remark:               remark,
		Dissolved:            true,
		AccountDeleted:       true,
		DeletedMXID:          ownerMXID,
	}))
}

func roomProfileForGroup(group groupRecord, dissolved bool) RoomStateEvent {
	return roomStateEvent(dirextalkstate.GroupRoomProfile(dirextalkstate.GroupProfile{
		RoomID:       group.RoomID,
		Name:         group.Name,
		Topic:        group.Topic,
		AvatarURL:    group.AvatarURL,
		InvitePolicy: group.InvitePolicy,
		Muted:        group.Muted,
	}, dissolved))
}

func roomProfileForChannel(ch channel, dissolved bool) RoomStateEvent {
	return roomStateEvent(dirextalkstate.ChannelRoomProfile(ch, dissolved))
}

func (s *Service) appendP2PEvent(ctx context.Context, event p2pEvent) error {
	return s.eventsModule.Append(ctx, event)
}

func (s *Service) listP2PEvents(ctx context.Context, since int64, limit int) ([]p2pEvent, error) {
	return s.eventsModule.List(ctx, since, limit)
}
