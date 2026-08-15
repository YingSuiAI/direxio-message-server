package agent

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
)

const (
	deliverableTestOwner        = "@owner:example.com"
	deliverableTestRunID        = "11111111-1111-4111-8111-111111111111"
	deliverableTestPlanID       = "22222222-2222-4222-8222-222222222222"
	deliverableTestTaskID       = "33333333-3333-4333-8333-333333333333"
	deliverableTestConfirmID    = "44444444-4444-4444-8444-444444444444"
	deliverableTestConversation = "55555555-5555-4555-8555-555555555555"
	deliverableTestTurnID       = "66666666-6666-4666-8666-666666666666"
	deliverableTestArtifactID   = "77777777-7777-4777-8777-777777777777"
)

type deliverablesRunner struct {
	mu                sync.Mutex
	calls             []string
	artifactExecution string
	err               error
}

func (r *deliverablesRunner) Apply(context.Context, string) error { return nil }

func (r *deliverablesRunner) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	return nil
}

func (r *deliverablesRunner) Invoke(_ context.Context, action string, params map[string]any) (map[string]any, error) {
	r.mu.Lock()
	r.calls = append(r.calls, action)
	r.mu.Unlock()
	if r.err != nil {
		return nil, r.err
	}
	switch action {
	case "agent.execution.v2.runs.list":
		if params["record_kind"] != cloudWorkerRecordKind || params["page_size"] != int64(7) || params["page_token"] != "next-run-page" {
			return nil, errors.New("unexpected runs list request")
		}
		return map[string]any{
			"runs": []any{map[string]any{
				"owner_id": deliverableTestOwner, "account_generation": float64(3),
				"run_id": deliverableTestRunID, "execution_id": deliverableTestRunID,
				"plan_id": deliverableTestPlanID, "plan_revision": float64(2),
				"task_id": deliverableTestTaskID, "confirmation_id": deliverableTestConfirmID,
				"conversation_id": deliverableTestConversation, "turn_id": deliverableTestTurnID,
				"status": "succeeded", "artifact_ids": []any{deliverableTestArtifactID},
				"plan_digest": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				"cleanup":     map[string]any{"verified_destroyed": true},
				"updated_at":  "2026-08-07T09:05:00.123456Z",
			}},
			"next_page_token": "after-run-page",
		}, nil
	case "agent.execution.v2.plans.get":
		if params["record_kind"] != cloudWorkerRecordKind || params["plan_id"] != deliverableTestPlanID || params["revision"] != int64(2) {
			return nil, errors.New("unexpected plan get request")
		}
		return map[string]any{"plan": map[string]any{
			"owner_id": deliverableTestOwner, "account_generation": float64(3),
			"plan_id": deliverableTestPlanID, "revision": float64(2), "artifact_retention_seconds": float64(30 * 24 * 60 * 60),
			"digest":       "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"execution_id": deliverableTestRunID, "task_id": deliverableTestTaskID,
			"confirmation_id": deliverableTestConfirmID, "conversation_id": deliverableTestConversation,
			"turn_id": deliverableTestTurnID, "objective_summary": "Build the requested deliverables",
		}}, nil
	case "agent.execution.v2.artifacts.get":
		if params["record_kind"] != cloudWorkerRecordKind || params["artifact_id"] != deliverableTestArtifactID {
			return nil, errors.New("unexpected artifact get request")
		}
		executionID := r.artifactExecution
		if executionID == "" {
			executionID = deliverableTestRunID
		}
		return map[string]any{"artifact": map[string]any{
			"owner_id": deliverableTestOwner, "account_generation": float64(3),
			"artifact_id": deliverableTestArtifactID, "execution_id": executionID,
			"name": "deliverables.tar.gz", "kind": "workspace_archive", "media_type": "application/gzip",
			"size_bytes": float64(4096), "sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"status": "verified", "created_at": "2026-08-07T09:00:00.123456Z",
		}}, nil
	default:
		return nil, errors.New("unexpected action")
	}
}

func (r *deliverablesRunner) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

func TestDeliverablesListComposesAdamHistoryMetadataAndDownloadContract(t *testing.T) {
	runner := &deliverablesRunner{}
	handler := New(Config{Runner: runner}).Handlers()[deliverablesListAction]
	result, actionErr := handler(context.Background(), map[string]any{
		"record_kind": cloudWorkerRecordKind, "page_size": float64(7), "page_token": "next-run-page",
	})
	if actionErr != nil {
		t.Fatalf("deliverables list failed: %#v", actionErr)
	}
	response, ok := result.(map[string]any)
	if !ok || response["next_page_token"] != "after-run-page" {
		t.Fatalf("deliverables response = %#v", result)
	}
	items, ok := response["deliverables"].([]map[string]any)
	if !ok || len(items) != 1 {
		t.Fatalf("deliverables = %#v", response["deliverables"])
	}
	item := items[0]
	if item["artifact_id"] != deliverableTestArtifactID || item["execution_id"] != deliverableTestRunID || item["conversation_id"] != deliverableTestConversation {
		t.Fatalf("deliverable identity = %#v", item)
	}
	if item["objective_summary"] != "Build the requested deliverables" || item["run_status"] != "succeeded" {
		t.Fatalf("deliverable context = %#v", item)
	}
	if item["retention_expires_at"] != "2026-09-06T09:00:00.123456Z" || item["completed_at"] != "2026-08-07T09:05:00.123456Z" {
		t.Fatalf("deliverable lifecycle = %#v", item)
	}
	download, ok := item["download"].(map[string]any)
	if !ok || download["action"] != deliverableDownloadAction || download["artifact_id"] != deliverableTestArtifactID || download["max_chunk_bytes"] != deliverableDownloadChunk {
		t.Fatalf("download descriptor = %#v", item["download"])
	}
	if runner.callCount() != 3 {
		t.Fatalf("underlying Adam calls = %d, want 3", runner.callCount())
	}
}

func TestDeliverablesListRejectsInvalidRequestBeforeAgent(t *testing.T) {
	runner := &deliverablesRunner{}
	handler := New(Config{Runner: runner}).Handlers()[deliverablesListAction]
	for name, params := range map[string]map[string]any{
		"wrong route":   {"record_kind": "generic"},
		"oversize page": {"record_kind": cloudWorkerRecordKind, "page_size": float64(21)},
		"unknown field": {"record_kind": cloudWorkerRecordKind, "owner_id": deliverableTestOwner},
	} {
		t.Run(name, func(t *testing.T) {
			if _, actionErr := handler(context.Background(), params); actionErr == nil || actionErr.Status != http.StatusBadRequest {
				t.Fatalf("invalid request error = %#v", actionErr)
			}
		})
	}
	if runner.callCount() != 0 {
		t.Fatalf("invalid request reached Agent %d times", runner.callCount())
	}
}

func TestDeliverablesListFailsClosedOnCrossExecutionArtifact(t *testing.T) {
	runner := &deliverablesRunner{artifactExecution: "88888888-8888-4888-8888-888888888888"}
	handler := New(Config{Runner: runner}).Handlers()[deliverablesListAction]
	_, actionErr := handler(context.Background(), map[string]any{
		"record_kind": cloudWorkerRecordKind, "page_size": float64(7), "page_token": "next-run-page",
	})
	if actionErr == nil || actionErr.Status != http.StatusBadGateway || actionErr.Error != "external native agent returned an invalid response" {
		t.Fatalf("cross-execution artifact error = %#v", actionErr)
	}
}
