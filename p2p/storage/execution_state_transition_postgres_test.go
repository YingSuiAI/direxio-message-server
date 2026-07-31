package storage

import (
	"context"
	"testing"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func TestExecutionStateTransitionsKeepSnapshotsAndAggregateFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 3, 1, 0, 0, 0, 0, time.UTC)
	store := openExecutionV2Schema(t)
	executionStore := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	analysis, plan := executionStoreFixture(now, 1)
	child := coreexecution.ExecutionStage{
		StageKey:       "verify",
		Revision:       1,
		Kind:           "verify",
		Risk:           coreexecution.RiskR2,
		Gate:           coreexecution.GateRemoteExecution,
		DependsOn:      []string{"deploy"},
		TargetID:       plan.Targets[0].ID,
		TargetRevision: 1,
		Steps: []coreexecution.ExecutionStep{{
			StepKey:           "verify-cleanup",
			Kind:              coreexecution.StepCleanup,
			TargetID:          plan.Targets[0].ID,
			TargetRevision:    1,
			TimeoutSeconds:    1,
			IdempotencyMarker: "verify-cleanup-1",
			Cleanup:           &coreexecution.CleanupStep{Resource: "verification"},
		}},
		TimeoutSeconds: 10,
	}
	plan.Stages = append(plan.Stages, child)
	created, err := executionStore.CreatePlan(ctx, ExecutionPlanCreate{
		OwnerID:       plan.OwnerID,
		Analysis:      analysis,
		Plan:          plan,
		IdempotencyID: "91919191-9191-4191-8191-919191919191",
	})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewDatabaseExecutionCoordinator(store.DB(), func() time.Time { return now })
	materialized, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{
		OwnerID:        plan.OwnerID,
		PlanID:         created.ID,
		PlanRevision:   created.Revision,
		IdempotencyKey: "92929292-9292-4292-8292-929292929292",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(materialized.Confirmations) != 1 {
		t.Fatalf("confirmations=%d", len(materialized.Confirmations))
	}
	confirmation := materialized.Confirmations[0]
	if _, err = coordinator.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{
		OwnerID:          plan.OwnerID,
		ConfirmationID:   confirmation.ID,
		ExpectedRevision: confirmation.Revision,
		IdempotencyKey:   "93939393-9393-4393-8393-939393939393",
	}); err != nil {
		t.Fatal(err)
	}
	claim, err := executionStore.ClaimNextExecutionStage(ctx, "snapshot-test-worker", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	terminalAt := now.Add(time.Minute)
	if err = transitionRunningExecutionStageTx(ctx, tx, plan.OwnerID, materialized.Run.RunID, claim.StageID, coreexecution.StageFailed, terminalAt); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err = skipBlockedExecutionDescendantsTx(ctx, tx, plan.OwnerID, materialized.Run.RunID, terminalAt); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	changed, err := transitionExecutionRunTx(ctx, tx, plan.OwnerID, materialized.Run.RunID, coreexecution.RunSucceeded, "", terminalAt, true)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if !changed {
		_ = tx.Rollback()
		t.Fatal("settled run was not terminalized")
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	view, err := executionStore.GetExecutionRun(ctx, plan.OwnerID, materialized.Run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Run.Status != coreexecution.RunFailed || view.Run.TerminalReason != "stage_failed" || view.Run.FinishedAt.IsZero() {
		t.Fatalf("run=%+v", view.Run)
	}
	statuses := map[string]coreexecution.StageStatus{}
	for _, stage := range view.Stages {
		statuses[stage.StageKey] = stage.Status
	}
	if statuses["deploy"] != coreexecution.StageFailed || statuses["verify"] != coreexecution.StageSkipped {
		t.Fatalf("stages=%v", statuses)
	}
}
