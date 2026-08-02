package agentturns_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func TestCoordinatorDurableLifecycleAndReplay(t *testing.T) {
	store := p2pstorage.NewMemoryStore()
	coordinator, err := agentturns.NewCoordinator(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	release := make(chan struct{})
	var executions atomic.Int32
	var issuedAt time.Time
	run := func(ctx context.Context, emit func(agentturns.RuntimeEvent) error) error {
		executions.Add(1)
		var ok bool
		issuedAt, ok = agentturns.IssuedAt(ctx)
		if !ok {
			t.Error("runner did not receive persisted issued_at")
		}
		close(started)
		<-release
		if err := emit(agentturns.RuntimeEvent{Event: "delta", Data: map[string]any{"text": "hello"}}); err != nil {
			return err
		}
		return emit(agentturns.RuntimeEvent{Event: "done", Data: map[string]any{"text": "hello"}})
	}

	accepted := make(chan agentturns.StreamEvent, 1)
	streamDone := make(chan error, 1)
	ctx, detach := context.WithCancel(context.Background())
	go func() {
		streamDone <- coordinator.Stream(ctx, request("owner-a", "turn-1", "conversation-a", "hello"), run, func(event agentturns.StreamEvent) error {
			if event.Kind == agentturns.EventAccepted {
				accepted <- event
			}
			return nil
		})
	}()

	select {
	case event := <-accepted:
		if event.Turn.State != agentturns.StateAccepted {
			t.Fatalf("accepted state = %q", event.Turn.State)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted was not emitted")
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("execution did not start after acceptance")
	}
	detach()
	if err := <-streamDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("detached stream error = %v", err)
	}
	close(release)
	waitTurnState(t, store, "owner-a", "turn-1", agentturns.StateSucceeded)
	persisted, ok, err := store.GetAgentTurn(context.Background(), "owner-a", "turn-1")
	if err != nil || !ok {
		t.Fatalf("persisted turn = (%#v, %v, %v)", persisted, ok, err)
	}
	if issuedAt.IsZero() || !issuedAt.Equal(persisted.CreatedAt) {
		t.Fatalf("runner issued_at = %s, persisted CreatedAt = %s", issuedAt, persisted.CreatedAt)
	}

	replayed := collectTerminalStream(t, coordinator, request("owner-a", "turn-1", "conversation-a", "hello"), run, 0)
	if executions.Load() != 1 {
		t.Fatalf("executions = %d, want 1", executions.Load())
	}
	assertEventSequence(t, replayed, []string{"accepted", "delta", "done"})
	for _, event := range replayed {
		if event.Kind == agentturns.EventAccepted && !event.Turn.CreatedAt.Equal(persisted.CreatedAt) {
			t.Fatalf("replayed event CreatedAt = %s, want %s", event.Turn.CreatedAt, persisted.CreatedAt)
		}
		if event.Kind == agentturns.EventRuntime && (event.TurnID != "turn-1" || event.ConversationID != "conversation-a" || event.Seq <= 0) {
			t.Fatalf("replayed event metadata = %#v", event)
		}
	}

	mismatch := request("owner-a", "turn-1", "conversation-a", "different")
	if err := coordinator.Stream(context.Background(), mismatch, run, func(agentturns.StreamEvent) error { return nil }); !errors.Is(err, agentturns.ErrTurnIDReused) {
		t.Fatalf("mismatch error = %v, want ErrTurnIDReused", err)
	}
	otherOwner := collectTerminalStream(t, coordinator, request("owner-b", "turn-1", "conversation-a", "other"), func(_ context.Context, emit func(agentturns.RuntimeEvent) error) error {
		return emit(agentturns.RuntimeEvent{Event: "done", Data: map[string]any{"text": "other"}})
	}, 0)
	assertEventSequence(t, otherOwner, []string{"accepted", "done"})
}

func TestCoordinatorStopIsSoleDurableCancellation(t *testing.T) {
	store := p2pstorage.NewMemoryStore()
	coordinator, err := agentturns.NewCoordinator(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	run := func(ctx context.Context, _ func(agentturns.RuntimeEvent) error) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	ctx, detach := context.WithCancel(context.Background())
	go func() {
		_ = coordinator.Stream(ctx, request("owner", "turn-stop", "conversation", "hold"), run, func(agentturns.StreamEvent) error { return nil })
	}()
	<-started
	detach()
	time.Sleep(20 * time.Millisecond)
	turn, changed, err := coordinator.Stop(context.Background(), "owner", "turn-stop")
	if err != nil || !changed || turn.State != agentturns.StateStopped {
		t.Fatalf("stop = (%#v, %v, %v)", turn, changed, err)
	}
	turn, changed, err = coordinator.Stop(context.Background(), "owner", "turn-stop")
	if err != nil || changed || turn.State != agentturns.StateStopped {
		t.Fatalf("idempotent stop = (%#v, %v, %v)", turn, changed, err)
	}
	replayed := collectTerminalStream(t, coordinator, request("owner", "turn-stop", "conversation", "hold"), run, 0)
	assertEventSequence(t, replayed, []string{"accepted", "stopped"})
}

func TestCoordinatorSerializesConversationAcceptanceOrder(t *testing.T) {
	store := p2pstorage.NewMemoryStore()
	coordinator, err := agentturns.NewCoordinator(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	firstStarted := make(chan struct{})
	firstRelease := make(chan struct{})
	secondStarted := make(chan struct{})
	var orderMu sync.Mutex
	var order []string
	first := func(_ context.Context, emit func(agentturns.RuntimeEvent) error) error {
		orderMu.Lock()
		order = append(order, "first")
		orderMu.Unlock()
		close(firstStarted)
		<-firstRelease
		return emit(agentturns.RuntimeEvent{Event: "done"})
	}
	second := func(_ context.Context, emit func(agentturns.RuntimeEvent) error) error {
		orderMu.Lock()
		order = append(order, "second")
		orderMu.Unlock()
		close(secondStarted)
		return emit(agentturns.RuntimeEvent{Event: "done"})
	}
	go func() {
		_ = coordinator.Stream(context.Background(), request("owner", "first", "same", "1"), first, func(agentturns.StreamEvent) error { return nil })
	}()
	<-firstStarted
	go func() {
		_ = coordinator.Stream(context.Background(), request("owner", "second", "same", "2"), second, func(agentturns.StreamEvent) error { return nil })
	}()
	select {
	case <-secondStarted:
		t.Fatal("second turn started before first completed")
	case <-time.After(40 * time.Millisecond):
	}
	close(firstRelease)
	select {
	case <-secondStarted:
	case <-time.After(time.Second):
		t.Fatal("second turn did not start")
	}
	orderMu.Lock()
	defer orderMu.Unlock()
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("execution order = %#v", order)
	}
}

func TestCoordinatorRestartInterruptsUnsafeTurns(t *testing.T) {
	store := p2pstorage.NewMemoryStore()
	digest, err := agentturns.RequestDigest("agent.chat.stream", map[string]any{"prompt": "hold"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ReserveAgentTurn(context.Background(), agentturns.Candidate{
		OwnerID: "owner", TurnID: "restart", ConversationID: "conversation", Action: "agent.chat.stream", Digest: digest,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := agentturns.NewCoordinator(context.Background(), store); err != nil {
		t.Fatal(err)
	}
	turn, ok, err := store.GetAgentTurn(context.Background(), "owner", "restart")
	if err != nil || !ok || turn.State != agentturns.StateInterrupted {
		t.Fatalf("restart turn = (%#v, %v, %v)", turn, ok, err)
	}
	events, err := store.ListAgentTurnEvents(context.Background(), "owner", "restart", 0)
	if err != nil || len(events) != 1 || events[0].Event != "interrupted" {
		t.Fatalf("restart events = (%#v, %v)", events, err)
	}
}

func TestDurableTurnDigestsAndEventsDropRequestScopedToolCredentials(t *testing.T) {
	withCredential, err := agentturns.RequestDigest("agent.chat.stream", map[string]any{
		"turn_id": "turn", "conversation_id": "conversation", "prompt": "search",
		"tool_credentials": map[string]any{
			"web_search": map[string]any{"enabled": true, "provider": "tavily", "api_key": "tvly-secret"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	withoutCredential, err := agentturns.RequestDigest("agent.chat.stream", map[string]any{
		"turn_id": "turn", "conversation_id": "conversation", "prompt": "search",
	})
	if err != nil {
		t.Fatal(err)
	}
	if withCredential != withoutCredential {
		t.Fatal("request-scoped tool credentials changed the durable turn digest")
	}
	clean := agentturns.SanitizeData(map[string]any{
		"tool_credentials": map[string]any{
			"web_search": map[string]any{"api_key": "tvly-secret"},
		},
		"result": "safe",
	})
	if _, exists := clean["tool_credentials"]; exists {
		t.Fatalf("durable event retained request-scoped credentials: %#v", clean)
	}
	if clean["result"] != "safe" {
		t.Fatalf("durable event sanitizer removed unrelated result: %#v", clean)
	}
}

func request(owner, turn, conversation, prompt string) agentturns.Request {
	params := map[string]any{"turn_id": turn, "conversation_id": conversation, "prompt": prompt, "model_profile": map[string]any{"api_key": "not-persisted"}}
	digest, err := agentturns.RequestDigest("agent.chat.stream", params)
	if err != nil {
		panic(err)
	}
	return agentturns.Request{OwnerID: owner, TurnID: turn, ConversationID: conversation, Action: "agent.chat.stream", Digest: digest}
}

func collectTerminalStream(t *testing.T, coordinator *agentturns.Coordinator, req agentturns.Request, run agentturns.Runner, after int64) []agentturns.StreamEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var events []agentturns.StreamEvent
	err := coordinator.Stream(ctx, req.WithAfterSeq(after), run, func(event agentturns.StreamEvent) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	return events
}

func assertEventSequence(t *testing.T, events []agentturns.StreamEvent, want []string) {
	t.Helper()
	got := make([]string, 0, len(events))
	for _, event := range events {
		if event.Kind == agentturns.EventAccepted {
			got = append(got, "accepted")
		} else {
			got = append(got, event.Event)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("event sequence = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event sequence = %#v, want %#v", got, want)
		}
	}
}

func waitTurnState(t *testing.T, store *p2pstorage.MemoryStore, owner, turnID string, state agentturns.State) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		turn, ok, err := store.GetAgentTurn(context.Background(), owner, turnID)
		if err == nil && ok && turn.State == state {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("turn %s did not reach %s", turnID, state)
}
