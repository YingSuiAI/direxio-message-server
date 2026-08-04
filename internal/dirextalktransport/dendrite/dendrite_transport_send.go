package dendrite

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/productpolicy"
	roomserverAPI "github.com/YingSuiAI/dirextalk-message-server/roomserver/api"
	"github.com/YingSuiAI/dirextalk-message-server/roomserver/types"
	"github.com/matrix-org/gomatrixserverlib"
	"github.com/matrix-org/gomatrixserverlib/spec"
)

func (t *DendriteTransport) PrepareMessage(ctx context.Context, req SendMessageRequest) (PreparedMessage, error) {
	event, _, err := t.buildMessageEvent(ctx, req)
	if err != nil {
		return PreparedMessage{}, err
	}
	eventJSON, err := event.ToHeaderedJSON()
	if err != nil {
		return PreparedMessage{}, err
	}
	return PreparedMessage{
		EventID:        event.EventID(),
		RoomID:         event.RoomID().String(),
		SenderMXID:     req.SenderMXID,
		EventType:      event.Type(),
		MessageType:    req.MessageType,
		OriginServerTS: int64(event.OriginServerTS()),
		RoomVersion:    string(event.Version()),
		EventJSON:      append([]byte(nil), eventJSON...),
		ContentDigest:  contentDigest(event.Content()),
	}, nil
}

func contentDigest(content []byte) []byte {
	digest := sha256.Sum256(content)
	return append([]byte(nil), digest[:]...)
}

func (t *DendriteTransport) SendMessage(ctx context.Context, req SendMessageRequest) (SendMessageResult, error) {
	prepared, err := t.PrepareMessage(ctx, req)
	if err != nil {
		return SendMessageResult{}, err
	}
	return t.SendPreparedMessage(ctx, prepared)
}

func (t *DendriteTransport) SendPreparedMessage(ctx context.Context, prepared PreparedMessage) (SendMessageResult, error) {
	if prepared.EventID == "" || prepared.RoomID == "" || prepared.SenderMXID == "" || len(prepared.EventJSON) == 0 {
		return SendMessageResult{}, fmt.Errorf("prepared Matrix message is incomplete")
	}
	event, err := gomatrixserverlib.NewEventFromHeaderedJSON(prepared.EventJSON, false)
	if err != nil {
		return SendMessageResult{}, fmt.Errorf("decode prepared Matrix event: %w", err)
	}
	if event.EventID() != prepared.EventID || event.RoomID().String() != prepared.RoomID {
		return SendMessageResult{}, fmt.Errorf("prepared Matrix event identity mismatch")
	}
	if len(prepared.ContentDigest) != sha256.Size || !equalDigest(prepared.ContentDigest, contentDigest(event.Content())) {
		return SendMessageResult{}, fmt.Errorf("prepared Matrix event content digest mismatch")
	}
	fullUserID, err := spec.NewUserID(prepared.SenderMXID, true)
	if err != nil {
		return SendMessageResult{}, err
	}
	validRoomID, err := spec.NewRoomID(prepared.RoomID)
	if err != nil {
		return SendMessageResult{}, err
	}
	resolvedSender, err := t.rsAPI.QueryUserIDForSender(ctx, *validRoomID, event.SenderID())
	if err != nil {
		return SendMessageResult{}, err
	}
	if resolvedSender == nil || resolvedSender.String() != fullUserID.String() {
		return SendMessageResult{}, fmt.Errorf("prepared Matrix event sender mismatch")
	}
	if err = roomserverAPI.SendEvents(
		ctx,
		t.rsAPI,
		roomserverAPI.KindNew,
		[]*types.HeaderedEvent{{PDU: event}},
		fullUserID.Domain(),
		fullUserID.Domain(),
		fullUserID.Domain(),
		nil,
		false,
	); err != nil {
		return SendMessageResult{}, err
	}
	return SendMessageResult{EventID: prepared.EventID, OriginServerTS: prepared.OriginServerTS}, nil
}

func equalDigest(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func (t *DendriteTransport) EventExists(ctx context.Context, roomID, eventID string) (bool, error) {
	query, ok := t.rsAPI.(roomserverAPI.QueryEventsAPI)
	if !ok {
		return false, fmt.Errorf("roomserver event lookup is unavailable")
	}
	var response roomserverAPI.QueryEventsByIDResponse
	if err := query.QueryEventsByID(ctx, &roomserverAPI.QueryEventsByIDRequest{RoomID: roomID, EventIDs: []string{eventID}}, &response); err != nil {
		return false, err
	}
	for _, event := range response.Events {
		if event != nil && event.EventID() == eventID && event.RoomID().String() == roomID {
			return true, nil
		}
	}
	return false, nil
}

// EventStatus deliberately returns Unknown for a matching event. QueryEventsByID
// proves that the roomserver retained the PDU, but this API does not expose the
// rejected/soft-failed bit; treating presence as accepted could submit a second
// copy after a crash between SendEvents and the durable receipt update.
func (t *DendriteTransport) EventStatus(ctx context.Context, prepared PreparedMessage) (MatrixEventDisposition, error) {
	query, ok := t.rsAPI.(roomserverAPI.QueryEventsAPI)
	if !ok {
		return MatrixEventUnknown, fmt.Errorf("roomserver event lookup is unavailable")
	}
	var response roomserverAPI.QueryEventsByIDResponse
	if err := query.QueryEventsByID(ctx, &roomserverAPI.QueryEventsByIDRequest{RoomID: prepared.RoomID, EventIDs: []string{prepared.EventID}}, &response); err != nil {
		return MatrixEventUnknown, err
	}
	found := false
	for _, event := range response.Events {
		if event == nil || event.EventID() != prepared.EventID {
			continue
		}
		found = true
		if event.RoomID().String() != prepared.RoomID || (prepared.EventType != "" && event.Type() != prepared.EventType) || len(prepared.ContentDigest) != sha256.Size || !equalDigest(prepared.ContentDigest, contentDigest(event.Content())) {
			return MatrixEventRejected, nil
		}
		validRoomID, err := spec.NewRoomID(prepared.RoomID)
		if err != nil {
			return MatrixEventRejected, nil
		}
		fullUserID, err := spec.NewUserID(prepared.SenderMXID, true)
		if err != nil {
			return MatrixEventRejected, nil
		}
		resolvedSender, err := t.rsAPI.QueryUserIDForSender(ctx, *validRoomID, event.SenderID())
		if err != nil {
			return MatrixEventUnknown, err
		}
		if resolvedSender == nil || resolvedSender.String() != fullUserID.String() {
			return MatrixEventRejected, nil
		}
	}
	if !found {
		// The event may have been rejected before persistence, or the query may
		// race the roomserver input worker. EventExists below distinguishes a
		// confidently absent event (safe exact resend) from a present unknown.
		return MatrixEventUnknown, nil
	}
	return MatrixEventUnknown, nil
}

func (t *DendriteTransport) buildMessageEvent(ctx context.Context, req SendMessageRequest) (*types.HeaderedEvent, *spec.UserID, error) {
	if req.Timestamp.IsZero() {
		req.Timestamp = time.Now()
	}
	fullUserID, err := spec.NewUserID(req.SenderMXID, true)
	if err != nil {
		return nil, nil, err
	}
	validRoomID, err := spec.NewRoomID(req.RoomID)
	if err != nil {
		return nil, nil, err
	}
	senderID, err := t.rsAPI.QuerySenderIDForUser(ctx, *validRoomID, *fullUserID)
	if err != nil {
		return nil, nil, err
	}
	if senderID == nil {
		return nil, nil, fmt.Errorf("sender %s is not joined to room %s", req.SenderMXID, req.RoomID)
	}
	eventType := strings.TrimSpace(req.EventType)
	if eventType == "" {
		eventType = "m.room.message"
	}
	content := req.Content
	if content == nil {
		content = map[string]any{}
	}
	if eventType == "m.room.message" {
		if _, ok := content["msgtype"]; !ok {
			content["msgtype"] = matrixMessageType(req.MessageType, false)
		}
	}
	var blockChecker func(context.Context, string, string) (bool, error)
	if t.blockedDirectMessageChecker != nil {
		blockChecker = t.checkBlockedDirectMessage
	}
	if err = productpolicy.ValidateClientEvent(ctx, t.productPolicyQuerier(), productpolicy.ClientEventRequest{
		RoomID:       req.RoomID,
		SenderMXID:   req.SenderMXID,
		EventType:    eventType,
		Content:      content,
		BlockChecker: blockChecker,
	}); err != nil {
		return nil, nil, err
	}
	identity, err := t.rsAPI.SigningIdentityFor(ctx, *validRoomID, *fullUserID)
	if err != nil {
		return nil, nil, err
	}
	proto := gomatrixserverlib.ProtoEvent{
		SenderID: string(*senderID),
		RoomID:   req.RoomID,
		Type:     eventType,
	}
	if err = proto.SetContent(content); err != nil {
		return nil, nil, err
	}
	event, queryRes, err := t.queryAndBuildEvent(ctx, &proto, &identity, req.Timestamp, req.RoomID)
	if err != nil {
		return nil, nil, err
	}
	stateEvents := make([]gomatrixserverlib.PDU, len(queryRes.StateEvents))
	for i := range queryRes.StateEvents {
		stateEvents[i] = queryRes.StateEvents[i].PDU
	}
	provider, err := gomatrixserverlib.NewAuthEvents(gomatrixserverlib.ToPDUs(stateEvents))
	if err != nil {
		return nil, nil, err
	}
	if err = gomatrixserverlib.Allowed(event.PDU, provider, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
		return t.rsAPI.QueryUserIDForSender(ctx, *validRoomID, senderID)
	}); err != nil {
		return nil, nil, err
	}
	return event, fullUserID, nil
}

func (t *DendriteTransport) checkBlockedDirectMessage(ctx context.Context, roomID, senderMXID string) (bool, error) {
	members, err := t.ListRoomMembers(ctx, roomID)
	if err != nil {
		return false, err
	}
	for _, member := range members {
		if member.Membership == string(spec.Join) && member.UserID != senderMXID {
			return t.blockedDirectMessageChecker(ctx, roomID, member.UserID)
		}
	}
	return false, nil
}

func (t *DendriteTransport) SendStateEvent(ctx context.Context, req SendStateEventRequest) error {
	if req.Timestamp.IsZero() {
		req.Timestamp = time.Now()
	}
	sender, err := spec.NewUserID(req.SenderMXID, true)
	if err != nil {
		return err
	}
	if sender.Domain() != t.serverName {
		return fmt.Errorf("state sender %s is not local to %s", req.SenderMXID, t.serverName)
	}
	validRoomID, err := spec.NewRoomID(req.RoomID)
	if err != nil {
		return err
	}
	senderID, err := t.rsAPI.QuerySenderIDForUser(ctx, *validRoomID, *sender)
	if err != nil {
		return err
	}
	if senderID == nil {
		return fmt.Errorf("state sender %s is not joined to room %s", req.SenderMXID, req.RoomID)
	}
	identity, err := t.rsAPI.SigningIdentityFor(ctx, *validRoomID, *sender)
	if err != nil {
		return err
	}
	stateKey := req.Event.StateKey
	proto := gomatrixserverlib.ProtoEvent{
		SenderID: string(*senderID),
		RoomID:   req.RoomID,
		Type:     req.Event.Type,
		StateKey: &stateKey,
	}
	if err = proto.SetContent(req.Event.Content); err != nil {
		return err
	}
	event, queryRes, err := t.queryAndBuildEvent(ctx, &proto, &identity, req.Timestamp, req.RoomID)
	if err != nil {
		return err
	}
	stateEvents := make([]gomatrixserverlib.PDU, len(queryRes.StateEvents))
	for i := range queryRes.StateEvents {
		stateEvents[i] = queryRes.StateEvents[i].PDU
	}
	provider, err := gomatrixserverlib.NewAuthEvents(gomatrixserverlib.ToPDUs(stateEvents))
	if err != nil {
		return err
	}
	if !isDirextalkPolicyStateEvent(req.Event.Type) {
		if err = gomatrixserverlib.Allowed(event.PDU, provider, func(roomID spec.RoomID, senderID spec.SenderID) (*spec.UserID, error) {
			return t.rsAPI.QueryUserIDForSender(ctx, *validRoomID, senderID)
		}); err != nil {
			return err
		}
	}
	return roomserverAPI.SendEvents(
		ctx,
		t.rsAPI,
		roomserverAPI.KindNew,
		[]*types.HeaderedEvent{{PDU: event.PDU}},
		sender.Domain(),
		sender.Domain(),
		sender.Domain(),
		nil,
		false,
	)
}

func isDirextalkPolicyStateEvent(eventType string) bool {
	switch eventType {
	case DirextalkJoinRequestEventType, DirextalkMemberPolicyEventType:
		return true
	default:
		return false
	}
}
