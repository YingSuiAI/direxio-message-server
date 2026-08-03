package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionrunner"
)

func TestExecutionPreDispatchFailureIsAtomicRedactedAndRestartTerminal(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 2, 3, 4, 5, 6, 0, time.UTC)
	database := openExecutionV2Schema(t)
	store := NewDatabaseExecutionStore(database.DB(), func() time.Time { return now })
	analysis, plan := executionStoreFixture(now, 1)
	if _, err := store.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: plan.OwnerID, Analysis: analysis, Plan: plan, IdempotencyID: "71717171-7171-4717-8717-717171717171"}); err != nil {
		t.Fatal(err)
	}
	coordinator := NewDatabaseExecutionCoordinator(database.DB(), func() time.Time { return now })
	materialized, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: plan.OwnerID, PlanID: plan.ID, PlanRevision: 1, IdempotencyKey: "72727272-7272-4727-8727-727272727272"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = coordinator.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{OwnerID: plan.OwnerID, ConfirmationID: materialized.Confirmations[0].ID, ExpectedRevision: materialized.Confirmations[0].Revision, IdempotencyKey: "73737373-7373-4737-8737-737373737373"}); err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimNextExecutionStage(ctx, "runner-a", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	next, err := store.NextExecutableStep(ctx, claim.OwnerID, claim.RunID, claim.StageID)
	if err != nil {
		t.Fatal(err)
	}
	runnerClaim := executionrunner.StageLease{
		OwnerID: claim.OwnerID, RunID: claim.RunID, StageID: claim.StageID, TaskID: claim.TaskID,
		Holder: claim.Holder, Attempt: claim.Attempt, LeaseEpoch: claim.LeaseEpoch,
		TaskLeaseEpoch: claim.TaskLeaseEpoch, ExpectedTaskRevision: claim.ExpectedTaskRevision,
		LeaseID: claim.LeaseID, LeaseToken: claim.LeaseToken, ExpiresAt: claim.ExpiresAt,
	}
	runnerStep := executionrunner.NextStep{
		OwnerID: next.OwnerID, RunID: next.RunID, StageID: next.StageID, StepKey: next.StepKey,
		StepSet: next.StepSet, StepRevision: next.StepRevision, StepDigest: next.StepDigest,
	}
	failure, err := executionrunner.NewPreDispatchFailure(runnerClaim, runnerStep, executionrunner.FailureStepResolution)
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewExecutionStageStoreAdapter(store)
	if err = adapter.FailPreDispatch(ctx, failure); err != nil {
		t.Fatal(err)
	}

	var attemptStatus, attemptOutput, taskStatus, taskCode, taskSummary, taskHolder, leaseStatus, stageStatus, runStatus, terminalReason string
	var attemptCount, receiptCount, intentCount, eventCount, runningCount int
	var taskExpiry sql.NullTime
	if err = database.DB().QueryRowContext(ctx, `SELECT status,output_digest FROM core_execution_step_attempts WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, claim.OwnerID, claim.RunID, claim.StageID).Scan(&attemptStatus, &attemptOutput); err != nil {
		t.Fatal(err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_step_attempts WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, claim.OwnerID, claim.RunID, claim.StageID).Scan(&attemptCount); err != nil {
		t.Fatal(err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT status,failure_code,failure_summary,lease_holder,lease_expires_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2`, claim.OwnerID, claim.TaskID).Scan(&taskStatus, &taskCode, &taskSummary, &taskHolder, &taskExpiry); err != nil {
		t.Fatal(err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT status FROM core_execution_target_mutation_leases WHERE owner_id=$1 AND lease_id=$2`, claim.OwnerID, claim.LeaseID).Scan(&leaseStatus); err != nil {
		t.Fatal(err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT status FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, claim.OwnerID, claim.RunID, claim.StageID).Scan(&stageStatus); err != nil {
		t.Fatal(err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT status,terminal_reason FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, claim.OwnerID, claim.RunID).Scan(&runStatus, &terminalReason); err != nil {
		t.Fatal(err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT running_count FROM agent_task_runtime_concurrency WHERE singleton=true`).Scan(&runningCount); err != nil {
		t.Fatal(err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_receipts WHERE owner_id=$1 AND run_id=$2`, claim.OwnerID, claim.RunID).Scan(&receiptCount); err != nil {
		t.Fatal(err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_dispatch_intents WHERE owner_id=$1 AND run_id=$2`, claim.OwnerID, claim.RunID).Scan(&intentCount); err != nil {
		t.Fatal(err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_events WHERE owner_id=$1 AND run_id=$2 AND kind=$3`, claim.OwnerID, claim.RunID, executionPreDispatchFailureEvent).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if attemptCount != 1 || attemptStatus != "failed" || attemptOutput != string(failure.EvidenceDigest) || receiptCount != 0 || intentCount != 0 || eventCount != 1 {
		t.Fatalf("attempts=%d status=%s output=%s receipts=%d intents=%d events=%d", attemptCount, attemptStatus, attemptOutput, receiptCount, intentCount, eventCount)
	}
	if taskStatus != "failed" || taskCode != executionrunner.FailureStepResolution || taskSummary != preDispatchFailureSummary(taskCode) || taskHolder != "" || taskExpiry.Valid || leaseStatus != "released" || stageStatus != "failed" || runStatus != "failed" || terminalReason != executionrunner.FailureStepResolution || runningCount != 0 {
		t.Fatalf("task=%s/%s/%q holder=%q expiry=%v lease=%s stage=%s run=%s/%s concurrency=%d", taskStatus, taskCode, taskSummary, taskHolder, taskExpiry, leaseStatus, stageStatus, runStatus, terminalReason, runningCount)
	}
	view, err := store.GetExecutionRun(ctx, claim.OwnerID, claim.RunID)
	if err != nil || view.Run.Status != coreexecution.RunFailed || view.Stages[0].Status != coreexecution.StageFailed {
		t.Fatalf("run readback=%#v err=%v", view, err)
	}
	events, _, err := store.ListV2RunEvents(ctx, claim.OwnerID, claim.RunID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.Kind != executionPreDispatchFailureEvent {
			continue
		}
		found = true
		var payload executionPreDispatchFailurePayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		if payload.FailureClass != "resolver" || payload.EvidenceDigest != failure.EvidenceDigest || strings.Contains(string(event.Payload), "Authorization") {
			t.Fatalf("unsafe event payload=%s", event.Payload)
		}
	}
	if !found {
		t.Fatal("pre-dispatch failure event missing")
	}

	// A lost response after this transaction is an exact replay, not a second
	// attempt or event.
	if err = adapter.FailPreDispatch(ctx, failure); err != nil {
		t.Fatalf("exact terminal replay: %v", err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_step_attempts WHERE owner_id=$1 AND run_id=$2`, claim.OwnerID, claim.RunID).Scan(&attemptCount); err != nil || attemptCount != 1 {
		t.Fatalf("replay attempts=%d err=%v", attemptCount, err)
	}
	if err = database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_events WHERE owner_id=$1 AND run_id=$2 AND kind=$3`, claim.OwnerID, claim.RunID, executionPreDispatchFailureEvent).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("replay events=%d err=%v", eventCount, err)
	}

	// A fresh process sees no claimable work and cannot re-run the rejected
	// step. This proves task/target/concurrency release survives restart.
	restarted := NewDatabaseExecutionStore(database.DB(), func() time.Time { return now.Add(time.Minute) })
	if _, err = restarted.ClaimNextExecutionStage(ctx, "runner-b", time.Minute); !errors.Is(err, coreexecution.ErrNotFound) {
		t.Fatalf("restart reclaimed terminal pre-dispatch failure: %v", err)
	}
}
