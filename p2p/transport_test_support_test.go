package p2p

import (
	"context"
	"errors"
	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
	"github.com/YingSuiAI/dirextalk-message-server/internal/pushrules"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

func initialHistoryVisibility(req CreateRoomRequest) (string, bool) {
	for _, state := range req.InitialState {
		if state.Type != spec.MRoomHistoryVisibility || state.StateKey != "" {
			continue
		}
		value, _ := state.Content["history_visibility"].(string)
		return value, true
	}
	return "", false
}

func updateStateHistoryVisibility(req SendStateEventRequest) (string, bool) {
	if req.Event.Type != spec.MRoomHistoryVisibility || req.Event.StateKey != "" {
		return "", false
	}
	value, _ := req.Event.Content["history_visibility"].(string)
	return value, true
}

func agentStatusOnlineState(state RoomStateEvent, agentMXID string) (bool, bool) {
	if state.Type != DirextalkAgentStatusEventType || state.StateKey != agentMXID {
		return false, false
	}
	online, ok := state.Content["online"].(bool)
	return online, ok
}

func agentStatusOnlineUpdate(req SendStateEventRequest, roomID, senderMXID, agentMXID string) (bool, bool) {
	if req.RoomID != roomID || req.SenderMXID != senderMXID {
		return false, false
	}
	return agentStatusOnlineState(req.Event, agentMXID)
}

func initialPowerLevelForUser(states []RoomStateEvent, userMXID string) (int, bool) {
	state, ok := initialStateOfType(states, spec.MRoomPowerLevels)
	if !ok {
		return 0, false
	}
	users, ok := state.Content["users"].(map[string]any)
	if !ok {
		return 0, false
	}
	switch level := users[userMXID].(type) {
	case int:
		return level, true
	case int64:
		return int(level), true
	case float64:
		return int(level), true
	default:
		return 0, false
	}
}

func initialStateOfType(states []RoomStateEvent, eventType string) (RoomStateEvent, bool) {
	for _, state := range states {
		if state.Type == eventType {
			return state, true
		}
	}
	return RoomStateEvent{}, false
}

type recordingPushRuleManager struct {
	ruleSets    *pushrules.AccountRuleSets
	putUserID   string
	putRuleSets *pushrules.AccountRuleSets
}

func (m *recordingPushRuleManager) QueryPushRules(ctx context.Context, userID string) (*pushrules.AccountRuleSets, error) {
	if m.ruleSets == nil {
		m.ruleSets = pushrules.DefaultAccountRuleSets(ownerLocalpart, "example.com")
	}
	return m.ruleSets, nil
}

func (m *recordingPushRuleManager) PerformPushRulesPut(ctx context.Context, userID string, ruleSets *pushrules.AccountRuleSets) error {
	m.putUserID = userID
	m.putRuleSets = ruleSets
	return nil
}

type failingSendTransport struct {
	recordingTransport
	err error
}

type failingStateTransport struct {
	recordingTransport
	err error
}

func (t *failingStateTransport) SendStateEvent(ctx context.Context, req SendStateEventRequest) error {
	t.stateEvents = append(t.stateEvents, req)
	return t.err
}

func (t *failingSendTransport) SendMessage(ctx context.Context, req SendMessageRequest) (SendMessageResult, error) {
	t.messages = append(t.messages, req)
	return SendMessageResult{}, t.err
}

type failingInviteTransport struct {
	recordingTransport
	err error
}

func (t *failingInviteTransport) InviteUser(ctx context.Context, req InviteUserRequest) error {
	t.invites = append(t.invites, req.InviterMXID+" -> "+req.InviteeMXID+" in "+req.RoomID)
	t.inviteRequests = append(t.inviteRequests, req)
	return t.err
}

type alreadyJoinedOnceInviteTransport struct {
	recordingTransport
	attempts int
}

func (t *alreadyJoinedOnceInviteTransport) InviteUser(ctx context.Context, req InviteUserRequest) error {
	t.invites = append(t.invites, req.InviterMXID+" -> "+req.InviteeMXID+" in "+req.RoomID)
	t.inviteRequests = append(t.inviteRequests, req)
	t.attempts++
	if t.attempts == 1 {
		return errors.New("user is already joined to room")
	}
	return nil
}

type failingRedactTransport struct {
	recordingTransport
	err error
}

func (t *failingRedactTransport) RedactEvent(ctx context.Context, req RedactEventRequest) (RedactEventResult, error) {
	t.redactions = append(t.redactions, req.EventID)
	return RedactEventResult{}, t.err
}

type failingLeaveTransport struct {
	recordingTransport
	err error
}

func (t *failingLeaveTransport) LeaveRoom(ctx context.Context, req LeaveRoomRequest) error {
	t.leaves = append(t.leaves, req.UserMXID+" from "+req.RoomID)
	return t.err
}

type failOnceJoinTransport struct {
	recordingTransport
	err      error
	attempts int
	failures int
}

func (t *failOnceJoinTransport) JoinRoom(ctx context.Context, req JoinRoomRequest) (JoinRoomResult, error) {
	t.joins = append(t.joins, req.UserMXID+" in "+req.RoomIDOrAlias)
	t.joinRequests = append(t.joinRequests, req)
	t.attempts++
	failures := t.failures
	if failures <= 0 {
		failures = 1
	}
	if t.attempts <= failures {
		return JoinRoomResult{}, t.err
	}
	return JoinRoomResult{RoomID: req.RoomIDOrAlias}, nil
}

type failingAcceptReplacementTransport struct {
	failOnceJoinTransport
	createErr error
}

func (t *failingAcceptReplacementTransport) CreateRoom(_ context.Context, req CreateRoomRequest) (CreateRoomResult, error) {
	t.createRooms = append(t.createRooms, req)
	return CreateRoomResult{}, t.createErr
}

type contactAcceptJoinResultTransport struct {
	recordingTransport
	resultRoomID string
}

func (t *contactAcceptJoinResultTransport) JoinRoom(_ context.Context, req JoinRoomRequest) (JoinRoomResult, error) {
	t.joins = append(t.joins, req.UserMXID+" in "+req.RoomIDOrAlias)
	t.joinRequests = append(t.joinRequests, req)
	return JoinRoomResult{RoomID: t.resultRoomID}, nil
}

type directReactivationJoinTransport struct {
	recordingTransport
}

func (t *directReactivationJoinTransport) JoinRoom(ctx context.Context, req JoinRoomRequest) (JoinRoomResult, error) {
	t.joins = append(t.joins, req.UserMXID+" in "+req.RoomIDOrAlias)
	t.joinRequests = append(t.joinRequests, req)
	if !req.DirectContactReactivation {
		return JoinRoomResult{}, productpolicy.Forbidden("direct room join requires invite")
	}
	return JoinRoomResult{RoomID: req.RoomIDOrAlias}, nil
}

type ownerJoinRequiresInviteTransport struct {
	recordingTransport
	ownerInvited bool
}

func (t *ownerJoinRequiresInviteTransport) JoinRoom(ctx context.Context, req JoinRoomRequest) (JoinRoomResult, error) {
	t.joins = append(t.joins, req.UserMXID+" in "+req.RoomIDOrAlias)
	t.joinRequests = append(t.joinRequests, req)
	if req.UserMXID == "@owner:example.com" && !t.ownerInvited {
		return JoinRoomResult{}, errors.New("owner join requires invite")
	}
	return JoinRoomResult{RoomID: req.RoomIDOrAlias}, nil
}

func (t *ownerJoinRequiresInviteTransport) InviteUser(ctx context.Context, req InviteUserRequest) error {
	if req.InviterMXID == "@agent:example.com" && req.InviteeMXID == "@owner:example.com" {
		t.ownerInvited = true
	}
	return t.recordingTransport.InviteUser(ctx, req)
}

func recordedStatesOfType(states []SendStateEventRequest, eventType string) []SendStateEventRequest {
	filtered := make([]SendStateEventRequest, 0, len(states))
	for _, state := range states {
		if state.Event.Type == eventType {
			filtered = append(filtered, state)
		}
	}
	return filtered
}

func findChannel(channels []channel, channelID string) channel {
	for _, ch := range channels {
		if ch.ChannelID == channelID {
			return ch
		}
	}
	return channel{}
}
