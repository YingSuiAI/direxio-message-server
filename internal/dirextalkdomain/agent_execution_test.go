package dirextalkdomain

import (
	"strings"
	"testing"
)

func goldenAgentExecutionCompletionReceipt() AgentExecutionCompletionReceipt {
	return AgentExecutionCompletionReceipt{
		EventID:           "00000000-0000-4000-8000-000000000001",
		ExecutionID:       "00000000-0000-4000-8000-000000000002",
		RunID:             "00000000-0000-4000-8000-000000000002",
		ConversationID:    "00000000-0000-4000-8000-000000000003",
		TurnID:            "00000000-0000-4000-8000-000000000004",
		TerminalState:     "succeeded",
		CompletedAt:       "2026-08-07T01:02:03.123456789Z",
		PayloadDigest:     "2d0ca6e1e63d3ef71d036a2c28c943376fa8e157e640bc7b701043fe86f7b850",
		OwnerID:           "@owner:example.test",
		AccountGeneration: 7,
	}
}

func TestAgentExecutionCompletionGoldenDigestAndValidation(t *testing.T) {
	receipt := goldenAgentExecutionCompletionReceipt()
	digest, err := CanonicalAgentExecutionCompletionDigest(receipt)
	if err != nil || digest != receipt.PayloadDigest {
		t.Fatalf("completion digest=%q err=%v", digest, err)
	}
	if err := receipt.Validate(); err != nil {
		t.Fatalf("golden receipt rejected: %v", err)
	}
}

func TestAgentExecutionCompletionRejectsNonCanonicalOrDriftedPayload(t *testing.T) {
	base := goldenAgentExecutionCompletionReceipt()
	tests := map[string]func(*AgentExecutionCompletionReceipt){
		"nil UUID": func(r *AgentExecutionCompletionReceipt) { r.EventID = "00000000-0000-0000-0000-000000000000" },
		"uppercase UUID": func(r *AgentExecutionCompletionReceipt) {
			r.EventID = "A0000000-0000-4000-8000-000000000001"
		},
		"run mismatch":            func(r *AgentExecutionCompletionReceipt) { r.RunID = r.TurnID },
		"unsupported terminal":    func(r *AgentExecutionCompletionReceipt) { r.TerminalState = "completed" },
		"non UTC timestamp":       func(r *AgentExecutionCompletionReceipt) { r.CompletedAt = "2026-08-07T09:02:03.123456789+08:00" },
		"non canonical timestamp": func(r *AgentExecutionCompletionReceipt) { r.CompletedAt = "2026-08-07T01:02:03.1234567890Z" },
		"uppercase digest":        func(r *AgentExecutionCompletionReceipt) { r.PayloadDigest = strings.ToUpper(r.PayloadDigest) },
		"payload drift":           func(r *AgentExecutionCompletionReceipt) { r.TerminalState = "failed" },
		"missing owner":           func(r *AgentExecutionCompletionReceipt) { r.OwnerID = "" },
		"zero generation":         func(r *AgentExecutionCompletionReceipt) { r.AccountGeneration = 0 },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			receipt := base
			mutate(&receipt)
			if err := receipt.Validate(); err == nil {
				t.Fatal("invalid receipt was accepted")
			}
		})
	}
}

func TestAgentExecutionCompletionAcceptsOnlyOutboxTerminalStates(t *testing.T) {
	for _, state := range []string{"succeeded", "failed", "canceled"} {
		t.Run("accept "+state, func(t *testing.T) {
			receipt := goldenAgentExecutionCompletionReceipt()
			receipt.TerminalState = state
			digest, err := CanonicalAgentExecutionCompletionDigest(receipt)
			if err != nil {
				t.Fatal(err)
			}
			receipt.PayloadDigest = digest
			if err := receipt.Validate(); err != nil {
				t.Fatalf("outbox terminal state %q rejected: %v", state, err)
			}
		})
	}
	for _, state := range []string{"rejected", "expired"} {
		t.Run("reject "+state, func(t *testing.T) {
			receipt := goldenAgentExecutionCompletionReceipt()
			receipt.TerminalState = state
			digest, err := CanonicalAgentExecutionCompletionDigest(receipt)
			if err != nil {
				t.Fatal(err)
			}
			receipt.PayloadDigest = digest
			if err := receipt.Validate(); err == nil {
				t.Fatalf("non-outbox terminal state %q was accepted", state)
			}
		})
	}
}
