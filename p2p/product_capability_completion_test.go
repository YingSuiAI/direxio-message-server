package p2p

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
)

func serviceCompletionReceipt(t *testing.T, service *Service) dirextalkdomain.AgentExecutionCompletionReceipt {
	t.Helper()
	raw, err := os.ReadFile("../internal/agentgateway/testdata/cloud_worker_public_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Completion dirextalkdomain.AgentExecutionCompletionReceipt `json:"completion"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	receipt := fixture.Completion
	receipt.OwnerID = service.OwnerMXID()
	receipt.AccountGeneration = 7
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
	conflict.ResultMessageID = "00000000-0000-4000-8000-000000000006"
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
