package p2p

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type concurrentContactCreateTransport struct {
	recordingTransport

	mu            sync.Mutex
	createCalls   int
	createEntered chan int
	releaseCreate chan struct{}
}

func newConcurrentContactCreateTransport() *concurrentContactCreateTransport {
	return &concurrentContactCreateTransport{
		createEntered: make(chan int, 2),
		releaseCreate: make(chan struct{}),
	}
}

func (t *concurrentContactCreateTransport) CreateRoom(_ context.Context, _ CreateRoomRequest) (CreateRoomResult, error) {
	t.mu.Lock()
	t.createCalls++
	call := t.createCalls
	t.mu.Unlock()
	t.createEntered <- call
	<-t.releaseCreate
	return CreateRoomResult{RoomID: fmt.Sprintf("!concurrent-%d:example.com", call)}, nil
}

func (t *concurrentContactCreateTransport) calls() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.createCalls
}

func TestConcurrentContactRequestsCreateOneDirectRoom(t *testing.T) {
	transport := newConcurrentContactCreateTransport()
	service := NewServiceWithTransport(Config{ServerName: "example.com"}, transport)
	bootstrapService(t, service)

	type result struct {
		contact contactRecord
		apiErr  *apiError
	}
	const peerMXID = "@alice:remote.example"
	start := make(chan struct{})
	ready := make(chan struct{}, 2)
	results := make(chan result, 2)
	for range 2 {
		go func() {
			ready <- struct{}{}
			<-start
			value, requestErr := service.Handle(context.Background(), "contacts.request", map[string]any{
				"mxid":         peerMXID,
				"display_name": "Alice",
			})
			contact, _ := value.(contactRecord)
			results <- result{contact: contact, apiErr: requestErr}
		}()
	}
	<-ready
	<-ready
	close(start)

	select {
	case <-transport.createEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("first contacts.request did not reach Matrix room creation")
	}

	secondCreateEntered := false
	select {
	case <-transport.createEntered:
		secondCreateEntered = true
	case <-time.After(250 * time.Millisecond):
	}
	close(transport.releaseCreate)

	first := <-results
	second := <-results
	if first.apiErr != nil || second.apiErr != nil {
		t.Fatalf("concurrent contacts.request errors = (%#v, %#v)", first.apiErr, second.apiErr)
	}
	if secondCreateEntered || transport.calls() != 1 {
		t.Fatalf("concurrent contacts.request created %d Matrix rooms, want 1", transport.calls())
	}
	if first.contact.RoomID == "" || second.contact.RoomID != first.contact.RoomID {
		t.Fatalf("concurrent contacts = (%#v, %#v), want the same direct room", first.contact, second.contact)
	}
}

type projectorSnapshotContextKey struct{}

type blockingProjectorContactStore struct {
	Store

	mu               sync.Mutex
	snapshotCaptured bool
	snapshotRead     chan struct{}
	releaseSnapshot  chan struct{}
}

func (s *blockingProjectorContactStore) ListContacts(ctx context.Context) ([]contactStorageRecord, error) {
	contacts, err := s.Store.ListContacts(ctx)
	if ctx.Value(projectorSnapshotContextKey{}) != true {
		return contacts, err
	}
	s.mu.Lock()
	if s.snapshotCaptured {
		s.mu.Unlock()
		return contacts, err
	}
	s.snapshotCaptured = true
	close(s.snapshotRead)
	s.mu.Unlock()
	<-s.releaseSnapshot
	return contacts, err
}

func TestProjectorSnapshotCannotOverwriteCompletedContactReplacement(t *testing.T) {
	baseStore := p2pstorage.NewMemoryStore()
	store := &blockingProjectorContactStore{
		Store:           baseStore,
		snapshotRead:    make(chan struct{}),
		releaseSnapshot: make(chan struct{}),
	}
	service, err := NewServiceWithStore(context.Background(), withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}

	const (
		peerMXID      = "@alice:remote.example"
		oldRoomID     = "!old:example.com"
		replacementID = "!replacement:example.com"
	)
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID: peerMXID, RoomID: oldRoomID, DisplayName: "Old", Status: "accepted",
	}); err != nil {
		t.Fatal(err)
	}

	peerLockHeld := make(chan struct{})
	releasePeerLock := make(chan struct{})
	peerLockDone := make(chan struct{})
	go func() {
		service.contactsModule.SerializePeer(peerMXID, func() {
			close(peerLockHeld)
			<-releasePeerLock
		})
		close(peerLockDone)
	}()
	<-peerLockHeld

	projectorDone := make(chan error, 1)
	projectorCtx := context.WithValue(context.Background(), projectorSnapshotContextKey{}, true)
	memberEvent := trustedStateEvent(t, oldRoomID, peerMXID, "m.room.member", peerMXID, map[string]any{
		"membership":  "join",
		"displayname": "Stale projector snapshot",
	})
	go func() {
		projectorDone <- service.ProjectRoomEvent(projectorCtx, memberEvent)
	}()

	select {
	case <-store.snapshotRead:
	case <-time.After(250 * time.Millisecond):
		// The fixed path is queued on the peer boundary before it can read Store.
	}
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID: peerMXID, RoomID: replacementID, DisplayName: "Replacement", Status: "accepted",
	}); err != nil {
		t.Fatal(err)
	}
	close(store.releaseSnapshot)
	close(releasePeerLock)
	<-peerLockDone
	if err := <-projectorDone; err != nil {
		t.Fatal(err)
	}

	contact, ok, err := service.lookupContactByPeer(context.Background(), peerMXID)
	if err != nil || !ok {
		t.Fatalf("replacement lookup = (%#v, %t, %v)", contact, ok, err)
	}
	if contact.RoomID != replacementID {
		t.Fatalf("projector restored stale contact room %q, want %q", contact.RoomID, replacementID)
	}
}

type ownerDirectJoinProjectionContextKey struct{}

type blockingOwnerDirectJoinContactStore struct {
	*p2pstorage.MemoryStore

	blockOnce       sync.Once
	mutationEntered chan struct{}
	releaseMutation chan struct{}
}

type failOnceOwnerDirectJoinProjectionStore struct {
	*p2pstorage.MemoryStore

	mu       sync.Mutex
	failNext bool
}

func (s *failOnceOwnerDirectJoinProjectionStore) CompareAndSwapContactProjection(
	ctx context.Context,
	contact,
	expected contactStorageRecord,
) (bool, error) {
	s.mu.Lock()
	fail := s.failNext
	s.failNext = false
	s.mu.Unlock()
	if fail {
		return false, errors.New("injected atomic contact conversation projection failure")
	}
	return s.MemoryStore.CompareAndSwapContactProjection(ctx, contact, expected)
}

func (s *blockingOwnerDirectJoinContactStore) blockOwnerDirectJoinProjection(
	ctx context.Context,
	contact contactStorageRecord,
) {
	if ctx.Value(ownerDirectJoinProjectionContextKey{}) != true ||
		!strings.EqualFold(strings.TrimSpace(contact.Status), "accepted") {
		return
	}
	s.blockOnce.Do(func() { close(s.mutationEntered) })
	<-s.releaseMutation
}

func (s *blockingOwnerDirectJoinContactStore) UpsertContact(
	ctx context.Context,
	contact contactStorageRecord,
) error {
	s.blockOwnerDirectJoinProjection(ctx, contact)
	return s.MemoryStore.UpsertContact(ctx, contact)
}

func (s *blockingOwnerDirectJoinContactStore) CompareAndSwapContact(
	ctx context.Context,
	contact,
	expected contactStorageRecord,
) (bool, error) {
	s.blockOwnerDirectJoinProjection(ctx, contact)
	return s.MemoryStore.CompareAndSwapContact(ctx, contact, expected)
}

func (s *blockingOwnerDirectJoinContactStore) CompareAndSwapContactProjection(
	ctx context.Context,
	contact,
	expected contactStorageRecord,
) (bool, error) {
	s.blockOwnerDirectJoinProjection(ctx, contact)
	return s.MemoryStore.CompareAndSwapContactProjection(ctx, contact, expected)
}

func TestOwnerDirectJoinProjectionAtomicFailureReplaysWithoutPartialAccept(t *testing.T) {
	store := &failOnceOwnerDirectJoinProjectionStore{
		MemoryStore: p2pstorage.NewMemoryStore(),
		failNext:    true,
	}
	service, err := NewServiceWithStore(context.Background(), withTestExternalAgent(Config{ServerName: "example.com"}), store)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapService(t, service)

	const (
		roomID   = "!direct-replay:example.com"
		peerMXID = "@replay:remote.example"
	)
	if err := service.saveContact(context.Background(), contactRecord{
		PeerMXID: peerMXID, RoomID: roomID, Status: "joining", RequestID: "$invite-replay",
	}); err != nil {
		t.Fatal(err)
	}
	ownerJoin := trustedStateEvent(
		t, roomID, peerMXID, "m.room.member", service.ownerMXID, map[string]any{"membership": "join"},
	)
	if err := service.ProjectRoomEvent(context.Background(), ownerJoin); err == nil ||
		!strings.Contains(err.Error(), "injected atomic contact conversation projection failure") {
		t.Fatalf("first owner join projection error = %v", err)
	}
	contact, ok, err := service.lookupContactByPeer(context.Background(), peerMXID)
	if err != nil || !ok || !strings.EqualFold(contact.Status, "joining") {
		t.Fatalf("failed atomic projection changed contact: contact=%#v found=%v err=%v", contact, ok, err)
	}
	conversation, found, err := service.conversationModule.GetRecord(context.Background(), "", roomID)
	if err != nil || !found || conversation.Lifecycle == "active" {
		t.Fatalf("failed atomic projection activated conversation: conversation=%#v found=%v err=%v", conversation, found, err)
	}

	if err := service.ProjectRoomEvent(context.Background(), ownerJoin); err != nil {
		t.Fatalf("replayed owner join projection failed: %v", err)
	}
	contact, ok, err = service.lookupContactByPeer(context.Background(), peerMXID)
	if err != nil || !ok || !strings.EqualFold(contact.Status, "accepted") {
		t.Fatalf("replayed owner join did not accept contact: contact=%#v found=%v err=%v", contact, ok, err)
	}
	conversation, found, err = service.conversationModule.GetRecord(context.Background(), "", roomID)
	if err != nil || !found || conversation.Lifecycle != "active" {
		t.Fatalf("replayed owner join did not activate conversation: conversation=%#v found=%v err=%v", conversation, found, err)
	}
}

func TestDirectJoinProjectionsCannotRestoreConcurrentlyDeletedContactAcrossServices(t *testing.T) {
	for _, tc := range []struct {
		name          string
		initialStatus string
		stateKey      func(*Service) string
	}{
		{name: "owner join", initialStatus: "joining", stateKey: func(service *Service) string { return service.ownerMXID }},
		{name: "peer join", initialStatus: "pending_outbound", stateKey: func(_ *Service) string { return "@alice:remote.example" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &blockingOwnerDirectJoinContactStore{
				MemoryStore:     p2pstorage.NewMemoryStore(),
				mutationEntered: make(chan struct{}),
				releaseMutation: make(chan struct{}),
			}
			projectorService, err := NewServiceWithStore(context.Background(), withTestExternalAgent(Config{ServerName: "example.com"}), store)
			if err != nil {
				t.Fatal(err)
			}
			bootstrapService(t, projectorService)
			deleteService, err := NewServiceWithStore(context.Background(), withTestExternalAgent(Config{ServerName: "example.com"}), store)
			if err != nil {
				t.Fatal(err)
			}

			const (
				roomID   = "!direct:example.com"
				peerMXID = "@alice:remote.example"
			)
			if err := projectorService.saveContact(context.Background(), contactRecord{
				PeerMXID: peerMXID, RoomID: roomID, Status: tc.initialStatus, RequestID: "$invite-generation",
			}); err != nil {
				t.Fatal(err)
			}

			projectorDone := make(chan error, 1)
			projectorCtx := context.WithValue(context.Background(), ownerDirectJoinProjectionContextKey{}, true)
			join := trustedStateEvent(t, roomID, peerMXID, "m.room.member", tc.stateKey(projectorService), map[string]any{
				"membership": "join", "displayname": "Alice",
			})
			go func() {
				projectorDone <- projectorService.ProjectRoomEvent(projectorCtx, join)
			}()

			select {
			case <-store.mutationEntered:
			case <-time.After(5 * time.Second):
				t.Fatal("join projection did not reach the contact persistence boundary")
			}
			_, deleteErr := deleteService.Handle(context.Background(), "contacts.delete", map[string]any{
				"room_id": roomID, "peer_mxid": peerMXID,
			})
			close(store.releaseMutation)
			if deleteErr != nil {
				t.Fatalf("concurrent contacts.delete failed: %#v", deleteErr)
			}
			if err := <-projectorDone; err != nil {
				t.Fatal(err)
			}

			contact, ok, err := projectorService.lookupContactByPeer(context.Background(), peerMXID)
			if err != nil || !ok {
				t.Fatalf("contact lookup = (%#v, %t, %v)", contact, ok, err)
			}
			if !strings.EqualFold(strings.TrimSpace(contact.Status), "deleted") {
				t.Fatalf("Matrix join projection restored %q over a later contacts.delete", contact.Status)
			}
			conversation, found, err := projectorService.conversationModule.GetRecord(context.Background(), "", roomID)
			if err != nil {
				t.Fatal(err)
			}
			if found && conversation.Lifecycle == "active" {
				t.Fatalf("Matrix join projection restored an active conversation after delete: %#v", conversation)
			}
		})
	}
}
