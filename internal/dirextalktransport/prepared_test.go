package dirextalktransport

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"testing"
)

type preparedTestStore struct{ event *PreparedMatrixEvent }

func (s *preparedTestStore) PrepareMatrixEvent(_ context.Context, event PreparedMatrixEvent) error {
	if s.event != nil {
		if !preparedMatrixEventEqual(s.event, &event) {
			return errors.New("conflict")
		}
		return nil
	}
	copy := event
	copy.Event = clonePreparedMessage(event.Event)
	copy.RootDigest = append([]byte(nil), event.RootDigest...)
	s.event = &copy
	return nil
}
func (s *preparedTestStore) GetMatrixPreparedEvent(_ context.Context, operationID, ownerID string, generation int64, root []byte) (*PreparedMatrixEvent, error) {
	if s.event == nil || s.event.OperationID != operationID || s.event.OwnerID != ownerID || s.event.Generation != generation || string(s.event.RootDigest) != string(root) {
		return nil, sql.ErrNoRows
	}
	copy := *s.event
	copy.Event = clonePreparedMessage(s.event.Event)
	return &copy, nil
}
func (s *preparedTestStore) DeleteMatrixPreparedEvent(context.Context, string, string, int64, []byte) error {
	s.event = nil
	return nil
}
func (s *preparedTestStore) DeleteMatrixPreparedEventByOperation(context.Context, string) error {
	s.event = nil
	return nil
}

type preparedTestPort struct {
	prepared     PreparedMessage
	prepareCount int
	sendCount    int
	present      bool
	sendErr      error
	existsErr    error
	resultEvent  string
}

type statusPreparedTestPort struct {
	preparedTestPort
	status    MatrixEventDisposition
	statusErr error
}

func (p *statusPreparedTestPort) EventStatus(context.Context, PreparedMessage) (MatrixEventDisposition, error) {
	return p.status, p.statusErr
}

func (p *preparedTestPort) PrepareMessage(context.Context, SendMessageRequest) (PreparedMessage, error) {
	p.prepareCount++
	return clonePreparedMessage(p.prepared), nil
}
func (p *preparedTestPort) SendPreparedMessage(_ context.Context, prepared PreparedMessage) (SendMessageResult, error) {
	p.sendCount++
	if p.sendErr != nil {
		return SendMessageResult{}, p.sendErr
	}
	p.present = true
	eventID := prepared.EventID
	if p.resultEvent != "" {
		eventID = p.resultEvent
	}
	return SendMessageResult{EventID: eventID, OriginServerTS: prepared.OriginServerTS}, nil
}
func (p *preparedTestPort) EventExists(context.Context, string, string) (bool, error) {
	return p.present, p.existsErr
}

func testPreparedOperation() CapabilityOperationContext {
	return CapabilityOperationContext{OperationID: "op-1", OwnerID: "owner-1", Generation: 1, RootDigest: make([]byte, 32)}
}

func testPreparedMessage() PreparedMessage {
	content := []byte(`{"body":"hello","msgtype":"m.text"}`)
	digest := sha256.Sum256(content)
	return PreparedMessage{EventID: "$event:example.test", RoomID: "!room:example.test", SenderMXID: "@agent:example.test", EventType: "m.room.message", MessageType: "text", OriginServerTS: 123, RoomVersion: "11", EventJSON: []byte(`{"_event_id":"$event:example.test","_room_version":"11","content":{"body":"hello","msgtype":"m.text"}}`), ContentDigest: digest[:]}
}

func TestExecutePreparedMatrixMutationReconcilesWithoutRebuild(t *testing.T) {
	store := &preparedTestStore{}
	port := &preparedTestPort{prepared: testPreparedMessage()}
	operation := testPreparedOperation()
	first, err := ExecutePreparedMatrixMutation(context.Background(), port, store, operation, "product.messages.v1", "send", SendMessageRequest{})
	if err != nil || first.EventID != port.prepared.EventID {
		t.Fatalf("first prepared send = %#v, err=%v", first, err)
	}
	if port.prepareCount != 1 || port.sendCount != 1 {
		t.Fatalf("first counts prepare=%d send=%d", port.prepareCount, port.sendCount)
	}
	second, err := ExecutePreparedMatrixMutation(context.Background(), port, store, operation, "product.messages.v1", "send", SendMessageRequest{})
	if err != nil || second.EventID != first.EventID {
		t.Fatalf("reconciled send = %#v, err=%v", second, err)
	}
	if port.prepareCount != 1 || port.sendCount != 1 {
		t.Fatalf("reconcile rebuilt or resent: prepare=%d send=%d", port.prepareCount, port.sendCount)
	}
}

func TestExecutePreparedMatrixMutationResendsExactPDUWhenMissing(t *testing.T) {
	store := &preparedTestStore{}
	port := &preparedTestPort{prepared: testPreparedMessage()}
	operation := testPreparedOperation()
	if _, err := ExecutePreparedMatrixMutation(context.Background(), port, store, operation, "product.messages.v1", "send", SendMessageRequest{}); err != nil {
		t.Fatal(err)
	}
	port.present = false
	if _, err := ExecutePreparedMatrixMutation(context.Background(), port, store, operation, "product.messages.v1", "send", SendMessageRequest{}); err != nil {
		t.Fatal(err)
	}
	if port.prepareCount != 1 || port.sendCount != 2 {
		t.Fatalf("expected exact resend prepare=%d send=%d", port.prepareCount, port.sendCount)
	}
}

func TestExecutePreparedMatrixMutationLeavesStageOnSendFailure(t *testing.T) {
	store := &preparedTestStore{}
	port := &preparedTestPort{prepared: testPreparedMessage(), sendErr: errors.New("roomserver unavailable")}
	if _, err := ExecutePreparedMatrixMutation(context.Background(), port, store, testPreparedOperation(), "product.messages.v1", "send", SendMessageRequest{}); !errors.Is(err, ErrMatrixEventUnknown) {
		t.Fatalf("send failure classification = %v, want unknown", err)
	}
	if store.event == nil {
		t.Fatal("prepared event was deleted after uncertain send")
	}
}

func TestExecutePreparedMatrixMutationClassifiesStatusAndReceiptAmbiguity(t *testing.T) {
	operation := testPreparedOperation()
	for name, port := range map[string]*statusPreparedTestPort{
		"status query": {preparedTestPort: preparedTestPort{prepared: testPreparedMessage()}, status: MatrixEventUnknown, statusErr: errors.New("query unavailable")},
		"exists query": {preparedTestPort: preparedTestPort{prepared: testPreparedMessage(), existsErr: errors.New("lookup unavailable")}, status: MatrixEventUnknown},
	} {
		store := &preparedTestStore{event: &PreparedMatrixEvent{OperationID: operation.OperationID, Capability: "product.messages.v1", Operation: "send", OwnerID: operation.OwnerID, Generation: operation.Generation, RootDigest: append([]byte(nil), operation.RootDigest...), Event: testPreparedMessage()}}
		if _, err := ExecutePreparedMatrixMutation(context.Background(), port, store, operation, "product.messages.v1", "send", SendMessageRequest{}); !errors.Is(err, ErrMatrixEventUnknown) {
			t.Fatalf("%s error=%v, want unknown", name, err)
		}
		if store.event == nil {
			t.Fatalf("%s deleted prepared event", name)
		}
	}

	store := &preparedTestStore{}
	port := &preparedTestPort{prepared: testPreparedMessage(), resultEvent: "$different:example.test"}
	if _, err := ExecutePreparedMatrixMutation(context.Background(), port, store, operation, "product.messages.v1", "send", SendMessageRequest{}); !errors.Is(err, ErrMatrixEventUnknown) {
		t.Fatalf("invalid receipt error=%v, want unknown", err)
	}
	if store.event == nil {
		t.Fatal("invalid receipt deleted prepared event")
	}
}

func TestExecutePreparedMatrixMutationDoesNotDuplicateUnknownPresentEvent(t *testing.T) {
	store := &preparedTestStore{}
	port := &statusPreparedTestPort{preparedTestPort: preparedTestPort{prepared: testPreparedMessage()}, status: MatrixEventUnknown}
	operation := testPreparedOperation()
	if _, err := ExecutePreparedMatrixMutation(context.Background(), port, store, operation, "product.messages.v1", "send", SendMessageRequest{}); err != nil {
		t.Fatal(err)
	}
	port.present = true
	if _, err := ExecutePreparedMatrixMutation(context.Background(), port, store, operation, "product.messages.v1", "send", SendMessageRequest{}); !errors.Is(err, ErrMatrixEventUnknown) {
		t.Fatalf("unknown present event err=%v, want ErrMatrixEventUnknown", err)
	}
	if port.sendCount != 1 {
		t.Fatalf("unknown present event was resent: send_count=%d", port.sendCount)
	}
}
