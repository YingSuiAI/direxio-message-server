package p2p

import (
	"context"
	"errors"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
)

type failOnceAgentExecutionCompletionStore struct {
	Store
	delegate agentExecutionCompletionStore
	calls    int
}

func (store *failOnceAgentExecutionCompletionStore) RecordAgentExecutionCompletion(
	ctx context.Context,
	receipt dirextalkdomain.AgentExecutionCompletionReceipt,
	event dirextalkdomain.Event,
) (bool, int64, error) {
	store.calls++
	if store.calls == 1 {
		return false, 0, errors.New("temporary completion store failure")
	}
	return store.delegate.RecordAgentExecutionCompletion(ctx, receipt, event)
}

func serviceCompletionReceipt(t *testing.T, service *Service) dirextalkdomain.AgentExecutionCompletionReceipt {
	t.Helper()
	receipt := dirextalkdomain.AgentExecutionCompletionReceipt{
		EventID: "bb58e65f-f277-5ac2-b958-1cf6c151cbef",
		OwnerID: service.OwnerMXID(), AccountGeneration: 7,
		ExecutionID:    "7e3937bb-c334-5360-90f9-931322e7fd88",
		RunID:          "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		ConversationID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		TurnID:         "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
		TerminalState:  "succeeded",
		CompletedAt:    "2026-08-07T10:11:00.123456Z",
	}
	receipt.PayloadDigest, _ = dirextalkdomain.CanonicalAgentExecutionCompletionDigest(receipt)
	if err := receipt.Validate(); err != nil {
		t.Fatalf("Cloud Worker completion fixture is invalid: %v", err)
	}
	return receipt
}

func TestRecordAgentExecutionCompletionPublishesOneDurableInvalidation(t *testing.T) {
	service := NewService(withTestExternalAgent(Config{ServerName: "example.test", AccountGeneration: 7}))
	receipt := serviceCompletionReceipt(t, service)
	waiter := service.eventsModule.Waiter()
	replayed, err := service.RecordAgentExecutionCompletion(context.Background(), receipt)
	if err != nil || replayed {
		t.Fatalf("first completion replayed=%t err=%v", replayed, err)
	}
	select {
	case <-waiter:
	default:
		t.Fatal("first completion did not wake realtime consumers")
	}
	events, err := service.listP2PEvents(context.Background(), 0, 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("completion events=%#v err=%v", events, err)
	}
	event := events[0]
	if event.Seq <= 0 || event.Type != "agent.execution.v2.completed" || event.EventID != receipt.EventID || event.Payload["execution_id"] != receipt.ExecutionID || event.Payload["payload_digest"] != receipt.PayloadDigest {
		t.Fatalf("completion invalidation=%#v", event)
	}
	if len(event.Payload) != 8 {
		t.Fatalf("completion invalidation payload keys=%#v", event.Payload)
	}
	for _, key := range []string{
		"event_id", "execution_id", "run_id", "conversation_id", "turn_id",
		"terminal_state", "completed_at", "payload_digest",
	} {
		if _, ok := event.Payload[key]; !ok {
			t.Fatalf("completion invalidation is missing %q: %#v", key, event.Payload)
		}
	}
	if after, err := service.listP2PEvents(context.Background(), event.Seq, 10); err != nil || len(after) != 0 {
		t.Fatalf("completion realtime cursor replayed acknowledged event: %#v err=%v", after, err)
	}
	for _, forbidden := range []string{"owner_id", "account_generation", "artifact", "aws", "result", "summary"} {
		if _, exists := event.Payload[forbidden]; exists {
			t.Fatalf("completion invalidation leaked %q: %#v", forbidden, event.Payload)
		}
	}

	waiter = service.eventsModule.Waiter()
	replayed, err = service.RecordAgentExecutionCompletion(context.Background(), receipt)
	if err != nil || !replayed {
		t.Fatalf("exact completion replayed=%t err=%v", replayed, err)
	}
	select {
	case <-waiter:
		t.Fatal("exact completion replay emitted a second invalidation")
	default:
	}
	events, _ = service.listP2PEvents(context.Background(), 0, 10)
	if len(events) != 1 || events[0].Seq != event.Seq {
		t.Fatalf("exact replay events=%#v want original sequence=%d", events, event.Seq)
	}
}

func TestRecordAgentExecutionCompletionFencesIdentityAndConflicts(t *testing.T) {
	service := NewService(withTestExternalAgent(Config{ServerName: "example.test", AccountGeneration: 7}))
	receipt := serviceCompletionReceipt(t, service)
	if _, err := service.RecordAgentExecutionCompletion(context.Background(), receipt); err != nil {
		t.Fatal(err)
	}
	conflict := receipt
	conflict.TerminalState = "failed"
	digest, _ := dirextalkdomain.CanonicalAgentExecutionCompletionDigest(conflict)
	conflict.PayloadDigest = digest
	if _, err := service.RecordAgentExecutionCompletion(context.Background(), conflict); !errors.Is(err, dirextalkdomain.ErrAgentExecutionCompletionConflict) {
		t.Fatalf("conflicting payload error=%v", err)
	}
	foreign := receipt
	foreign.OwnerID = "@attacker:example.test"
	if _, err := service.RecordAgentExecutionCompletion(context.Background(), foreign); !errors.Is(err, dirextalkdomain.ErrAgentExecutionCompletionConflict) {
		t.Fatalf("foreign owner error=%v", err)
	}
	stale := receipt
	stale.AccountGeneration--
	if _, err := service.RecordAgentExecutionCompletion(context.Background(), stale); !errors.Is(err, dirextalkdomain.ErrAgentExecutionCompletionConflict) {
		t.Fatalf("stale generation error=%v", err)
	}
}

func TestRecordAgentExecutionCompletionFailureDoesNotPublishAndExactRetrySucceeds(t *testing.T) {
	service := NewService(withTestExternalAgent(Config{ServerName: "example.test", AccountGeneration: 7}))
	receipt := serviceCompletionReceipt(t, service)
	delegate, ok := service.store.(agentExecutionCompletionStore)
	if !ok {
		t.Fatal("test service completion store is unavailable")
	}
	store := &failOnceAgentExecutionCompletionStore{Store: service.store, delegate: delegate}
	service.store = store

	if replayed, err := service.RecordAgentExecutionCompletion(context.Background(), receipt); err == nil || replayed {
		t.Fatalf("failed completion replayed=%t err=%v", replayed, err)
	}
	if events, err := service.listP2PEvents(context.Background(), 0, 10); err != nil || len(events) != 0 {
		t.Fatalf("failed completion invalidations=%#v err=%v", events, err)
	}

	if replayed, err := service.RecordAgentExecutionCompletion(context.Background(), receipt); err != nil || replayed {
		t.Fatalf("retried completion replayed=%t err=%v", replayed, err)
	}
	events, err := service.listP2PEvents(context.Background(), 0, 10)
	if err != nil || len(events) != 1 || events[0].EventID != receipt.EventID {
		t.Fatalf("retried completion invalidations=%#v err=%v", events, err)
	}
}

func TestRecordAgentExecutionCompletionDoesNotPublishNonOutboxTerminalStates(t *testing.T) {
	for _, state := range []string{"rejected", "expired"} {
		t.Run(state, func(t *testing.T) {
			service := NewService(withTestExternalAgent(Config{ServerName: "example.test", AccountGeneration: 7}))
			receipt := serviceCompletionReceipt(t, service)
			receipt.TerminalState = state
			digest, err := dirextalkdomain.CanonicalAgentExecutionCompletionDigest(receipt)
			if err != nil {
				t.Fatal(err)
			}
			receipt.PayloadDigest = digest
			if _, err := service.RecordAgentExecutionCompletion(context.Background(), receipt); err == nil {
				t.Fatalf("non-outbox terminal state %q was accepted", state)
			}
			events, err := service.listP2PEvents(context.Background(), 0, 10)
			if err != nil || len(events) != 0 {
				t.Fatalf("non-outbox completion invalidations=%#v err=%v", events, err)
			}
		})
	}
}
