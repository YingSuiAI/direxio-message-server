package p2p

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	roomserverAPI "github.com/YingSuiAI/dirextalk-message-server/roomserver/api"
)

type blockingBlockMemoryStore struct {
	*p2pstorage.MemoryStore
	upsertStarted chan struct{}
	releaseUpsert chan struct{}
	resetStarted  chan struct{}
}

type signalingAccountDeprovisioner struct {
	called chan struct{}
}

func (d *signalingAccountDeprovisioner) DeprovisionAccount(context.Context) error {
	close(d.called)
	return nil
}

func (s *blockingBlockMemoryStore) UpsertBlock(ctx context.Context, block blockRecord) error {
	close(s.upsertStarted)
	<-s.releaseUpsert
	return s.MemoryStore.UpsertBlock(ctx, block)
}

func (s *blockingBlockMemoryStore) ResetAccountState() {
	close(s.resetStarted)
	s.MemoryStore.ResetAccountState()
}

func TestAccountDeleteWaitsForInFlightProductWriteBeforeReset(t *testing.T) {
	store := &blockingBlockMemoryStore{
		MemoryStore:   p2pstorage.NewMemoryStore(),
		upsertStarted: make(chan struct{}),
		releaseUpsert: make(chan struct{}),
		resetStarted:  make(chan struct{}),
	}
	service := newService(Config{
		ServerName:        "example.com",
		ReleaseController: &recordingReleaseController{},
	}, store, nil, portalState{}, false)
	service.SetAccountDeactivator(&recordingAccountDeactivator{})
	deprovisioner := &signalingAccountDeprovisioner{called: make(chan struct{})}
	service.SetAccountDeprovisioner(deprovisioner)
	bootstrapService(t, service)

	writeDone := make(chan *apiError, 1)
	go func() {
		_, apiErr := service.Handle(context.Background(), "blocks.add", map[string]any{
			"target_type": "contact",
			"peer_mxid":   "@peer:remote.example",
		})
		writeDone <- apiErr
	}()
	waitForSignal(t, store.upsertStarted, "block Store write to start")

	deleteDone := make(chan *apiError, 1)
	go func() {
		_, apiErr := service.Handle(context.Background(), "portal.account.delete", map[string]any{
			"confirm": accountDeleteConfirmValue,
		})
		deleteDone <- apiErr
	}()
	waitForAccountDeletionStart(t, service)

	resetStageStartedBeforeWrite := ""
	select {
	case <-deprovisioner.called:
		resetStageStartedBeforeWrite = "database reset"
	case <-store.resetStarted:
		resetStageStartedBeforeWrite = "memory reset"
	case <-time.After(100 * time.Millisecond):
	}
	close(store.releaseUpsert)

	if apiErr := waitForAPIResult(t, writeDone, "block write"); apiErr != nil {
		t.Fatalf("blocks.add failed: %#v", apiErr)
	}
	if apiErr := waitForAPIResult(t, deleteDone, "account deletion"); apiErr != nil {
		t.Fatalf("portal.account.delete failed: %#v", apiErr)
	}
	waitForSignal(t, deprovisioner.called, "database account reset")
	waitForSignal(t, store.resetStarted, "account Store reset")

	if resetStageStartedBeforeWrite != "" {
		t.Fatalf("%s started before the in-flight product write", resetStageStartedBeforeWrite)
	}
	blocks, err := store.ListBlocks(context.Background())
	if err != nil {
		t.Fatalf("list blocks: %v", err)
	}
	if len(blocks) != 0 {
		t.Fatalf("in-flight block survived account reset: %#v", blocks)
	}
}

func TestDeprovisionedAccountRejectsQueuedProductMCPAndProjectionWork(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	service.clearAccountStateInMemory()

	if _, apiErr := service.Handle(context.Background(), "blocks.list", nil); apiErr == nil || apiErr.Status != http.StatusUnauthorized {
		t.Fatalf("queued ProductCore action was not rejected after deprovision: %#v", apiErr)
	}
	if _, mcpErr := service.mcpModule.InvokeCapability(
		context.Background(), dirextalkmcp.ActionContactsList, nil,
	); mcpErr == nil || mcpErr.Status != http.StatusUnauthorized {
		t.Fatalf("queued MCP invocation was not rejected after deprovision: %#v", mcpErr)
	}
	if _, found, err := service.portalStore().LoadPortal(context.Background()); err != nil || found {
		t.Fatalf("deprovisioned portal state was unexpectedly repopulated: found=%v err=%v", found, err)
	}

	event := trustedStateEvent(t, "!group:example.com", "@peer:remote.example", DirextalkRoomProfileEventType, "", map[string]any{
		"room_type": DirextalkRoomTypeGroup,
		"name":      "Queued Group",
	})
	if err := service.ProjectOutputEvent(context.Background(), roomserverAPI.OutputEvent{
		Type:         roomserverAPI.OutputTypeNewRoomEvent,
		NewRoomEvent: &roomserverAPI.OutputNewRoomEvent{Event: event},
	}); err != nil {
		t.Fatalf("drop queued projection: %v", err)
	}
	groups, err := service.listGroups(context.Background())
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	if len(groups) != 0 {
		t.Fatalf("queued projection repopulated deprovisioned state: %#v", groups)
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, operation string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
	}
}

func waitForAccountDeletionStart(t *testing.T, service *Service) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		service.mu.Lock()
		started := service.accountDeletionInProgress
		service.mu.Unlock()
		if started {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("timed out waiting for account deletion to start")
}

func waitForAPIResult(t *testing.T, result <-chan *apiError, operation string) *apiError {
	t.Helper()
	select {
	case apiErr := <-result:
		return apiErr
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", operation)
		return nil
	}
}
