package productpolicy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/YingSuiAI/dirextalk-message-server/internal/eventutil"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/api"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/types"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

const (
	DirextalkRoomTypeDirect  = "io.dirextalk.room.direct"
	DirextalkRoomTypeGroup   = "io.dirextalk.room.group"
	DirextalkRoomTypeChannel = "io.dirextalk.room.channel"

	DirextalkRoomProfileEventType         = "io.dirextalk.room.profile"
	DirextalkMemberPolicyEventType        = "io.dirextalk.member.policy"
	DirextalkJoinRequestEventType         = "io.dirextalk.join_request"
	DirextalkChannelPostSettingsEventType = "io.dirextalk.channel.post.settings"
	PublicJoinRequestIDContentKey         = "io.dirextalk.public_join_request_id"
)

const (
	userStateKeyPrefix = "user:"
)

func UserStateKey(mxid string) string {
	return userStateKeyPrefix + strings.TrimSpace(mxid)
}

func UserIDFromStateKey(stateKey string) string {
	stateKey = strings.TrimSpace(stateKey)
	if strings.HasPrefix(stateKey, userStateKeyPrefix) {
		return strings.TrimPrefix(stateKey, userStateKeyPrefix)
	}
	return stateKey
}

type CurrentStateQuerier interface {
	QueryCurrentState(ctx context.Context, req *api.QueryCurrentStateRequest, res *api.QueryCurrentStateResponse) error
	QueryMembershipForUser(ctx context.Context, req *api.QueryMembershipForUserRequest, res *api.QueryMembershipForUserResponse) error
}

type userIDForSenderQuerier interface {
	QueryUserIDForSender(ctx context.Context, roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error)
}

type RedactionQuerier interface {
	CurrentStateQuerier
	QueryEventsByID(ctx context.Context, req *api.QueryEventsByIDRequest, res *api.QueryEventsByIDResponse) error
}

type invitePendingQuerier interface {
	InvitePending(ctx context.Context, roomID spec.RoomID, senderID spec.SenderID) (bool, error)
}

type ClientEventRequest struct {
	RoomID       string
	SenderMXID   string
	EventType    string
	StateKey     *string
	Content      map[string]any
	BlockChecker func(context.Context, string, string) (bool, error)
}

// ValidateBlockedDirectMessage checks only the product direct-room and exact
// contact block pair, leaving ordinary Matrix rooms and other product rooms
// untouched.
func ValidateBlockedDirectMessage(ctx context.Context, querier CurrentStateQuerier, req ClientEventRequest) (bool, error) {
	if querier == nil || req.BlockChecker == nil {
		return false, nil
	}
	room, err := resolveRoom(ctx, querier, req.RoomID, req.SenderMXID)
	if err != nil {
		return false, err
	}
	if !room.Product || room.RoomType != DirextalkRoomTypeDirect {
		return false, nil
	}
	return req.BlockChecker(ctx, strings.TrimSpace(req.RoomID), strings.TrimSpace(req.SenderMXID))
}

// IsDirextalkDirectRoom scopes federation message filtering without requiring
// a sender MXID (which may be unavailable for pseudo-ID events).
func IsDirextalkDirectRoom(ctx context.Context, querier CurrentStateQuerier, roomID string) (bool, error) {
	roomType, err := ProductRoomType(ctx, querier, roomID)
	return roomType == DirextalkRoomTypeDirect, err
}

func ProductRoomType(ctx context.Context, querier CurrentStateQuerier, roomID string) (string, error) {
	if querier == nil {
		return "", nil
	}
	var res api.QueryCurrentStateResponse
	if err := querier.QueryCurrentState(ctx, &api.QueryCurrentStateRequest{
		RoomID: roomID,
		StateTuples: []gomatrixserverlib.StateKeyTuple{
			{EventType: spec.MRoomCreate, StateKey: ""},
			{EventType: DirextalkRoomProfileEventType, StateKey: ""},
		},
	}, &res); err != nil {
		if roomDoesNotExistError(err) {
			return "", nil
		}
		return "", err
	}
	createContent, err := eventContent(res.StateEvents[gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomCreate, StateKey: ""}])
	if err != nil {
		return "", err
	}
	if isDirextalkRoomType(stringValue(createContent["type"])) {
		return stringValue(createContent["type"]), nil
	}
	profileContent, err := eventContent(res.StateEvents[gomatrixserverlib.StateKeyTuple{EventType: DirextalkRoomProfileEventType, StateKey: ""}])
	if err != nil {
		return "", err
	}
	roomType := stringValue(profileContent["room_type"])
	if isDirextalkRoomType(roomType) {
		return roomType, nil
	}
	return "", nil
}

type ClientRedactionRequest struct {
	RoomID        string
	SenderMXID    string
	TargetEventID string
}

type ClientMembershipRequest struct {
	RoomID     string
	SenderMXID string
	TargetMXID string
	Membership string
}

type PolicyError struct {
	Code    int
	Message string
}

func (e *PolicyError) Error() string {
	return e.Message
}

func Forbidden(message string) *PolicyError {
	return &PolicyError{Code: 403, Message: message}
}

func ValidateClientEvent(ctx context.Context, querier CurrentStateQuerier, req ClientEventRequest) error {
	if querier == nil {
		return nil
	}
	eventType := strings.TrimSpace(req.EventType)
	if !isProductPolicyEvent(eventType) {
		return nil
	}
	room, err := resolveRoom(ctx, querier, req.RoomID, req.SenderMXID)
	if err != nil {
		return err
	}
	if !room.Product {
		return nil
	}
	if room.Dissolved {
		return Forbidden("dirextalk room is dissolved")
	}
	if !room.SenderJoined {
		return Forbidden("sender is not joined to the dirextalk room")
	}
	if room.SenderMuted {
		return Forbidden("sender is muted in the dirextalk room")
	}
	if room.RoomType == DirextalkRoomTypeDirect && !room.DirectPeerJoined {
		return Forbidden("direct room peer is not joined to the dirextalk room")
	}
	if room.RoomType == DirextalkRoomTypeDirect && eventType == "m.room.message" && req.BlockChecker != nil {
		blocked, blockErr := ValidateBlockedDirectMessage(ctx, querier, req)
		if blockErr != nil {
			return blockErr
		}
		if blocked {
			return Forbidden("sender is blocked in this dirextalk direct room")
		}
	}
	if room.RoomType == DirextalkRoomTypeChannel && !room.SenderPrivileged() {
		switch {
		case isChannelPostEvent(req.Content):
			return Forbidden("sender cannot create channel posts in this dirextalk room")
		case isChannelCommentEvent(eventType, req.Content):
			if !room.CommentsEnabled {
				return Forbidden("channel comments are disabled")
			}
			if !strings.EqualFold(channelEventKind(req.Content), "channel_comment") {
				break
			}
			enabled, settingsErr := channelPostCommentsEnabled(ctx, querier, req.RoomID, stringValue(req.Content["post_id"]))
			if settingsErr != nil {
				return settingsErr
			}
			if !enabled {
				return Forbidden("comments are disabled for this channel post")
			}
		}
	}
	return nil
}

func ValidateClientRedaction(ctx context.Context, querier RedactionQuerier, req ClientRedactionRequest) error {
	if querier == nil {
		return nil
	}
	room, err := resolveRoom(ctx, querier, req.RoomID, req.SenderMXID)
	if err != nil {
		return err
	}
	if !room.Product {
		return nil
	}
	if room.Dissolved {
		return Forbidden("dirextalk room is dissolved")
	}
	if !room.SenderJoined {
		return Forbidden("sender is not joined to the dirextalk room")
	}
	if room.SenderMuted {
		return Forbidden("sender is muted in the dirextalk room")
	}
	target, err := queryTargetEvent(ctx, querier, req.RoomID, req.TargetEventID)
	if err != nil || target == nil {
		return err
	}
	if string(target.SenderID()) == req.SenderMXID || room.SenderPrivileged() {
		return nil
	}
	return Forbidden("sender cannot redact another sender in dirextalk room")
}

func ValidateClientMembership(ctx context.Context, querier CurrentStateQuerier, req ClientMembershipRequest) error {
	if querier == nil {
		return nil
	}
	room, err := resolveRoom(ctx, querier, req.RoomID, req.SenderMXID)
	if err != nil {
		return err
	}
	if !room.Product {
		return nil
	}
	membership := strings.ToLower(strings.TrimSpace(req.Membership))
	if membership == string(spec.Join) && strings.TrimSpace(req.SenderMXID) == strings.TrimSpace(req.TargetMXID) {
		if room.Dissolved {
			return Forbidden("dirextalk room is dissolved")
		}
		return validateJoin(room)
	}
	if membership == string(spec.Leave) && strings.TrimSpace(req.SenderMXID) == strings.TrimSpace(req.TargetMXID) {
		return nil
	}
	if room.Dissolved {
		return Forbidden("dirextalk room is dissolved")
	}
	if !room.SenderJoined {
		return Forbidden("sender is not joined to the dirextalk room")
	}
	switch membership {
	case string(spec.Invite):
		if room.DirectPeerInvite(req.SenderMXID, req.TargetMXID) {
			return nil
		}
		if room.RoomType == DirextalkRoomTypeDirect {
			wasMember, err := priorRoomMember(ctx, querier, req.RoomID, req.TargetMXID)
			if err != nil {
				return err
			}
			if wasMember {
				return nil
			}
		}
		if room.RoomType == DirextalkRoomTypeGroup && strings.EqualFold(room.InvitePolicy, "member") {
			return nil
		}
		if room.SenderPrivileged() {
			return nil
		}
		return Forbidden("sender cannot invite members to this dirextalk room")
	case string(spec.Ban):
		if room.SenderPrivileged() {
			return nil
		}
		return Forbidden("sender cannot ban members in this dirextalk room")
	case string(spec.Leave):
		if room.SenderPrivileged() {
			return nil
		}
		return Forbidden("sender cannot remove another member from this dirextalk room")
	default:
		return nil
	}
}

func validateJoin(room roomPolicy) error {
	if room.SenderJoined {
		return nil
	}
	if strings.EqualFold(room.SenderMembership, string(spec.Invite)) {
		return nil
	}
	switch room.RoomType {
	case DirextalkRoomTypeChannel:
		if strings.EqualFold(room.JoinPolicy, "open") {
			return nil
		}
		if strings.EqualFold(room.JoinRequestStatus, "approved") {
			return nil
		}
		return Forbidden("channel join requires approved join request")
	case DirextalkRoomTypeGroup:
		return Forbidden("group join requires invite")
	case DirextalkRoomTypeDirect:
		return Forbidden("direct room join requires invite")
	default:
		return Forbidden("sender is not joined to the dirextalk room")
	}
}

func isProductPolicyEvent(eventType string) bool {
	switch eventType {
	case "m.room.message", "m.reaction":
		return true
	default:
		return false
	}
}

func isChannelPostEvent(content map[string]any) bool {
	return strings.EqualFold(channelEventKind(content), "channel_post")
}

func isChannelCommentEvent(eventType string, content map[string]any) bool {
	if eventType == "m.reaction" {
		return true
	}
	return strings.EqualFold(channelEventKind(content), "channel_comment")
}

func channelEventKind(content map[string]any) string {
	return stringValue(content["p2p_kind"])
}

func channelPostCommentsEnabled(ctx context.Context, querier CurrentStateQuerier, roomID, postID string) (bool, error) {
	postID = strings.TrimSpace(postID)
	if postID == "" {
		return false, nil
	}
	tuple := gomatrixserverlib.StateKeyTuple{EventType: DirextalkChannelPostSettingsEventType, StateKey: postID}
	var res api.QueryCurrentStateResponse
	if err := querier.QueryCurrentState(ctx, &api.QueryCurrentStateRequest{
		RoomID: roomID, StateTuples: []gomatrixserverlib.StateKeyTuple{tuple},
	}, &res); err != nil {
		return false, err
	}
	event := res.StateEvents[tuple]
	if event == nil {
		return true, nil
	}
	content, err := eventContent(event)
	if err != nil {
		return false, err
	}
	if statePostID := strings.TrimSpace(stringValue(content["post_id"])); statePostID != "" && statePostID != postID {
		return false, nil
	}
	enabled, ok := content["comments_enabled"].(bool)
	if !ok {
		return false, nil
	}
	return enabled, nil
}

func queryTargetEvent(ctx context.Context, querier RedactionQuerier, roomID, eventID string) (*types.HeaderedEvent, error) {
	var res api.QueryEventsByIDResponse
	if err := querier.QueryEventsByID(ctx, &api.QueryEventsByIDRequest{
		RoomID:   roomID,
		EventIDs: []string{eventID},
	}, &res); err != nil {
		return nil, err
	}
	if len(res.Events) == 0 {
		return nil, nil
	}
	return res.Events[0], nil
}

type roomPolicy struct {
	Product           bool
	RoomType          string
	Dissolved         bool
	CommentsEnabled   bool
	InvitePolicy      string
	JoinPolicy        string
	DirectRequester   string
	DirectTarget      string
	SenderRole        string
	SenderMuted       bool
	SenderMembership  string
	SenderJoined      bool
	DirectPeerJoined  bool
	JoinRequestStatus string
}

func (p roomPolicy) SenderPrivileged() bool {
	return strings.EqualFold(strings.TrimSpace(p.SenderRole), "owner")
}

func (p roomPolicy) DirectPeerInvite(senderMXID, targetMXID string) bool {
	if p.RoomType != DirextalkRoomTypeDirect {
		return false
	}
	requester := strings.TrimSpace(p.DirectRequester)
	target := strings.TrimSpace(p.DirectTarget)
	senderMXID = strings.TrimSpace(senderMXID)
	targetMXID = strings.TrimSpace(targetMXID)
	return requester != "" && target != "" &&
		((senderMXID == requester && targetMXID == target) ||
			(senderMXID == target && targetMXID == requester))
}

//nolint:gocyclo // Resolves all supported Matrix and Dirextalk room policy state in one snapshot query.
func resolveRoom(ctx context.Context, querier CurrentStateQuerier, roomID, senderMXID string) (roomPolicy, error) {
	var res api.QueryCurrentStateResponse
	if err := querier.QueryCurrentState(ctx, &api.QueryCurrentStateRequest{
		RoomID: roomID,
		StateTuples: []gomatrixserverlib.StateKeyTuple{
			{EventType: spec.MRoomCreate, StateKey: ""},
			{EventType: DirextalkRoomProfileEventType, StateKey: ""},
			{EventType: DirextalkMemberPolicyEventType, StateKey: senderMXID},
			{EventType: DirextalkMemberPolicyEventType, StateKey: UserStateKey(senderMXID)},
			{EventType: DirextalkJoinRequestEventType, StateKey: senderMXID},
			{EventType: DirextalkJoinRequestEventType, StateKey: UserStateKey(senderMXID)},
			{EventType: spec.MRoomMember, StateKey: senderMXID},
		},
	}, &res); err != nil {
		if roomDoesNotExistError(err) {
			return roomPolicy{}, nil
		}
		return roomPolicy{}, err
	}

	policy := roomPolicy{
		CommentsEnabled: true,
		InvitePolicy:    "member",
		JoinPolicy:      "invite",
		SenderRole:      "member",
	}
	createEvent := res.StateEvents[gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomCreate, StateKey: ""}]
	createContent, err := eventContent(createEvent)
	if err != nil {
		return roomPolicy{}, err
	}
	if roomType := stringValue(createContent["type"]); isDirextalkRoomType(roomType) {
		policy.Product = true
		policy.RoomType = roomType
	}

	if event := res.StateEvents[gomatrixserverlib.StateKeyTuple{EventType: DirextalkRoomProfileEventType, StateKey: ""}]; event != nil {
		content, contentErr := eventContent(event)
		if contentErr != nil {
			return roomPolicy{Product: true}, contentErr
		}
		if roomType := stringValue(content["room_type"]); isDirextalkRoomType(roomType) {
			policy.Product = true
			policy.RoomType = roomType
		}
		if policy.Product {
			applyRoomPolicyContent(&policy, content, true)
		}
	}

	if !policy.Product {
		return policy, nil
	}
	creatorMXID, err := authoritativeCreateSenderMXID(ctx, querier, roomID, createEvent)
	if err != nil {
		return roomPolicy{}, err
	}
	if creatorMXID == senderMXID {
		policy.SenderRole = "owner"
	}

	memberPolicyEvent := res.StateEvents[gomatrixserverlib.StateKeyTuple{EventType: DirextalkMemberPolicyEventType, StateKey: UserStateKey(senderMXID)}]
	if memberPolicyEvent == nil {
		memberPolicyEvent = res.StateEvents[gomatrixserverlib.StateKeyTuple{EventType: DirextalkMemberPolicyEventType, StateKey: senderMXID}]
	}
	memberPolicy, err := eventContent(memberPolicyEvent)
	if err != nil {
		return roomPolicy{Product: true}, err
	}
	if boolValue(memberPolicy["muted"]) {
		policy.SenderMuted = true
	}
	joinRequestEvent := res.StateEvents[gomatrixserverlib.StateKeyTuple{EventType: DirextalkJoinRequestEventType, StateKey: UserStateKey(senderMXID)}]
	if joinRequestEvent == nil {
		joinRequestEvent = res.StateEvents[gomatrixserverlib.StateKeyTuple{EventType: DirextalkJoinRequestEventType, StateKey: senderMXID}]
	}
	joinRequest, err := eventContent(joinRequestEvent)
	if err != nil {
		return roomPolicy{Product: true}, err
	}
	policy.JoinRequestStatus = strings.ToLower(stringValue(joinRequest["status"]))
	policy.SenderMembership, policy.SenderJoined, err = senderMembership(ctx, querier, roomID, senderMXID, res.StateEvents[gomatrixserverlib.StateKeyTuple{EventType: spec.MRoomMember, StateKey: senderMXID}])
	if err != nil {
		return roomPolicy{Product: true}, err
	}
	if policy.RoomType == DirextalkRoomTypeDirect {
		policy.DirectPeerJoined, err = directPeerJoined(ctx, querier, roomID, senderMXID)
		if err != nil {
			return roomPolicy{Product: true}, err
		}
	}
	return policy, nil
}

func authoritativeCreateSenderMXID(ctx context.Context, querier CurrentStateQuerier, roomID string, event *types.HeaderedEvent) (string, error) {
	if event == nil {
		return "", nil
	}
	senderID := event.SenderID()
	if userID, err := spec.NewUserID(strings.TrimSpace(string(senderID)), true); err == nil && userID != nil {
		return userID.String(), nil
	}
	resolver, ok := querier.(userIDForSenderQuerier)
	if !ok || strings.TrimSpace(string(senderID)) == "" {
		return "", nil
	}
	validRoomID, err := spec.NewRoomID(strings.TrimSpace(roomID))
	if err != nil {
		return "", err
	}
	userID, err := resolver.QueryUserIDForSender(ctx, *validRoomID, senderID)
	if err != nil || userID == nil {
		return "", err
	}
	return userID.String(), nil
}

func directPeerJoined(ctx context.Context, querier CurrentStateQuerier, roomID, senderMXID string) (bool, error) {
	var res api.QueryCurrentStateResponse
	if err := querier.QueryCurrentState(ctx, &api.QueryCurrentStateRequest{
		RoomID:         roomID,
		AllowWildcards: true,
		StateTuples: []gomatrixserverlib.StateKeyTuple{
			{EventType: spec.MRoomMember, StateKey: "*"},
		},
	}, &res); err != nil {
		return false, err
	}
	for tuple, event := range res.StateEvents {
		if event == nil || tuple.EventType != spec.MRoomMember {
			continue
		}
		userID := tuple.StateKey
		if userID == "" && event.StateKey() != nil {
			userID = *event.StateKey()
		}
		if userID == "" || userID == senderMXID {
			continue
		}
		content, err := eventContent(event)
		if err != nil {
			return false, err
		}
		if stringValue(content["membership"]) == string(spec.Join) {
			return true, nil
		}
	}
	return false, nil
}

func roomDoesNotExistError(err error) bool {
	if err == nil {
		return false
	}
	var noRoom eventutil.ErrRoomNoExists
	if errors.As(err, &noRoom) {
		return true
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "room ") && (strings.Contains(message, "doesn't exist") || strings.Contains(message, "does not exist"))
}

func applyRoomPolicyContent(policy *roomPolicy, content map[string]any, requireChannelComments bool) {
	if _, ok := content["dissolved"]; ok {
		policy.Dissolved = boolValue(content["dissolved"])
	}
	if policy.RoomType == DirextalkRoomTypeChannel {
		if _, ok := content["comments_enabled"]; ok || requireChannelComments {
			policy.CommentsEnabled = boolValue(content["comments_enabled"])
		}
	}
	if invitePolicy := stringValue(content["invite_policy"]); invitePolicy != "" {
		policy.InvitePolicy = invitePolicy
	}
	if joinPolicy := stringValue(content["join_policy"]); joinPolicy != "" {
		policy.JoinPolicy = joinPolicy
	}
	if policy.RoomType == DirextalkRoomTypeDirect {
		if requester := stringValue(content["requester_mxid"]); requester != "" {
			policy.DirectRequester = requester
		}
		if target := stringValue(content["target_mxid"]); target != "" {
			policy.DirectTarget = target
		}
	}
}

func senderMembership(ctx context.Context, querier CurrentStateQuerier, roomID, senderMXID string, event *types.HeaderedEvent) (string, bool, error) {
	content, err := eventContent(event)
	if err != nil {
		return "", false, err
	}
	stateMembership := stringValue(content["membership"])
	if stateMembership == spec.Join {
		return stateMembership, true, nil
	}
	userID, err := spec.NewUserID(senderMXID, true)
	if err != nil {
		return "", false, err
	}
	var res api.QueryMembershipForUserResponse
	if err = querier.QueryMembershipForUser(ctx, &api.QueryMembershipForUserRequest{
		RoomID: roomID,
		UserID: *userID,
	}, &res); err != nil {
		return "", false, err
	}
	if res.Membership == string(spec.Join) {
		return res.Membership, true, nil
	}
	if res.Membership == "" || stateMembership != "" {
		invites, ok := querier.(invitePendingQuerier)
		if !ok {
			return membershipValue(res.Membership, stateMembership), res.IsInRoom || res.Membership == string(spec.Join), nil
		}
		validRoomID, roomErr := spec.NewRoomID(roomID)
		if roomErr != nil {
			return "", false, roomErr
		}
		pending, pendingErr := invites.InvitePending(ctx, *validRoomID, spec.SenderID(senderMXID))
		if pendingErr != nil {
			return "", false, pendingErr
		}
		if pending {
			return string(spec.Invite), false, nil
		}
	}
	return membershipValue(res.Membership, stateMembership), res.IsInRoom || res.Membership == string(spec.Join), nil
}

func membershipValue(queryMembership, stateMembership string) string {
	if queryMembership != "" {
		return queryMembership
	}
	return stateMembership
}

func priorRoomMember(ctx context.Context, querier CurrentStateQuerier, roomID, userMXID string) (bool, error) {
	userID, err := spec.NewUserID(userMXID, true)
	if err != nil {
		return false, err
	}
	var res api.QueryMembershipForUserResponse
	if err = querier.QueryMembershipForUser(ctx, &api.QueryMembershipForUserRequest{
		RoomID: roomID,
		UserID: *userID,
	}, &res); err != nil {
		return false, err
	}
	return res.RoomExists && res.HasBeenInRoom, nil
}

func eventContent(event *types.HeaderedEvent) (map[string]any, error) {
	if event == nil {
		return map[string]any{}, nil
	}
	content := map[string]any{}
	if err := json.Unmarshal(event.Content(), &content); err != nil {
		return nil, err
	}
	return content, nil
}

func isDirextalkRoomType(value string) bool {
	switch strings.TrimSpace(value) {
	case DirextalkRoomTypeDirect, DirextalkRoomTypeGroup, DirextalkRoomTypeChannel:
		return true
	default:
		return false
	}
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func boolValue(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case string:
		parsed := strings.ToLower(strings.TrimSpace(v))
		return parsed == "true" || parsed == "1" || parsed == "yes"
	case float64:
		return v != 0
	case int:
		return v != 0
	default:
		return false
	}
}
