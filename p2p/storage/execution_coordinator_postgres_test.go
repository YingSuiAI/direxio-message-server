package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestDatabaseExecutionCoordinatorCreateConfirmReplay(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2030, 1, 1, 0, 5, 0, 0, time.UTC)
	owner := "@coord-owner:example.org"
	targetID := "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	projectID := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	analysisID := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	planID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	target, err := (coreexecution.ExecutionTarget{ID: targetID, Provider: "aws", Kind: "aws_ec2_instance", AccountID: "123456789012", Region: "us-east-1", Revision: 1}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	artifact := coreexecution.ArtifactRef{ID: "dddddddd-dddd-4ddd-8ddd-dddddddddddd", Digest: coreexecution.Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"), Immutable: true, Size: 1}
	step := coreexecution.ExecutionStep{StepKey: "run", Kind: coreexecution.StepScriptRun, TargetID: targetID, TargetRevision: 1, TargetDigest: target.Digest, ObservationRef: &coreexecution.TargetObservationRef{ObservationID: "abababab-abab-4bab-8bab-abababababab", TargetID: targetID, TargetRevision: 1, ObservationDigest: coreexecution.Digest("abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd")}, TimeoutSeconds: 30, IdempotencyMarker: "idem", OutputPolicy: coreexecution.OutputCapture, Postcondition: &coreexecution.Postcondition{Type: "exit_code", Value: "0"}, ScriptRun: &coreexecution.ScriptRunStep{Artifact: artifact, Interpreter: "/bin/sh", Argv: []string{"-e"}, CWD: "/", AllowedExitCodes: []int{0}, Root: true, TimeoutSeconds: 30, OutputLimit: 1024, Redaction: coreexecution.RedactionPolicy{Patterns: []string{"secret"}, Replace: "[REDACTED]"}, Postcondition: &coreexecution.Postcondition{Type: "exit_code", Value: "0"}, IdempotencyMarker: "idem"}}
	quote := coreexecution.CostQuote{Amount: "1", Currency: "USD", ExpiresAt: now.Add(time.Hour)}
	placement := coreexecution.PlacementOption{Region: "us-east-1", Spec: "t3.small", Disk: "20GiB", Network: "private", CostQuote: quote}
	plan := coreexecution.ExecutionPlan{SchemaVersion: coreexecution.SchemaVersion, ID: planID, Revision: 1, OwnerID: owner, ProjectID: projectID, AnalysisID: analysisID, Purpose: coreexecution.PurposeJob, Placement: coreexecution.PlacementRecommendation{Kind: "existing_target", Minimum: placement, Recommended: placement, HighPerformance: placement}, Targets: []coreexecution.ExecutionTarget{target}, Artifacts: []coreexecution.ArtifactRef{artifact}, Stages: []coreexecution.ExecutionStage{{StageKey: "inspect", Revision: 1, Kind: "inspect", Risk: coreexecution.RiskR2, Gate: coreexecution.GateRemotePrivilegedExecution, Effects: []coreexecution.Gate{coreexecution.GateRemotePrivilegedExecution}, TargetID: targetID, TargetRevision: 1, TargetDigest: target.Digest, Steps: []coreexecution.ExecutionStep{step}, TimeoutSeconds: 60}}, Status: coreexecution.PlanReady, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	analysis := coreexecution.ProjectAnalysis{AnalysisID: analysisID, ProjectID: projectID, Source: coreexecution.SourceRef{Kind: "git_https", Location: "https://example.invalid/repo", Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	if _, err = NewDatabaseExecutionStore(store.DB(), func() time.Time { return now }).CreatePlan(ctx, ExecutionPlanCreate{OwnerID: owner, Analysis: analysis, Plan: plan, IdempotencyID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}); err != nil {
		t.Fatal(err)
	}
	c := NewDatabaseExecutionCoordinator(store.DB(), func() time.Time { return now })
	cmd := coreexecution.CreateRunCommand{OwnerID: owner, PlanID: planID, PlanRevision: 1, IdempotencyKey: "11111111-1111-4111-8111-111111111111"}
	first, err := c.CreateRun(ctx, cmd)
	if err != nil {
		t.Fatal(err)
	}
	if first.Run.Status != coreexecution.RunWaitingUser || first.Run.Revision != 2 || len(first.Confirmations) != 1 || first.Stages[0].Status != coreexecution.StageWaitingUser {
		t.Fatalf("materialization=%#v", first)
	}
	var stageRunRevision uint64
	var stageSnapshot []byte
	if err = store.DB().QueryRowContext(ctx, `SELECT run_revision,snapshot_json FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, owner, first.Run.RunID, first.Stages[0].StageID).Scan(&stageRunRevision, &stageSnapshot); err != nil {
		t.Fatal(err)
	}
	var storedStage coreexecution.RunStage
	if err = json.Unmarshal(stageSnapshot, &storedStage); err != nil || stageRunRevision != 2 || storedStage.RunRevision != 2 || storedStage.TaskID != first.Tasks[0].ID || storedStage.ConfirmationID != first.Confirmations[0].ID {
		t.Fatalf("stage pin snapshot=%s stage=%#v err=%v", stageSnapshot, storedStage, err)
	}
	if payload := first.Tasks[0].Spec.Payload.ExecutionStage; payload == nil || payload.RunRevision != 2 || payload.RunID != first.Run.RunID || payload.StageID != first.Stages[0].StageID {
		t.Fatalf("task payload does not pin revision two: %#v", payload)
	}
	if binding := first.Confirmations[0].Binding; binding.RunRevision != 2 || binding.RunID != first.Run.RunID || binding.StageID != first.Stages[0].StageID || string(binding.PlanDigest) != string(first.Run.PlanDigest) {
		t.Fatalf("confirmation binding does not pin revision two: %#v", binding)
	}
	assertExecutionRunHistory(t, ctx, store.DB(), owner, first.Run.RunID, 1, coreexecution.RunPending)
	assertExecutionRunHistory(t, ctx, store.DB(), owner, first.Run.RunID, 2, coreexecution.RunWaitingUser)
	replay, err := c.CreateRun(ctx, cmd)
	if err != nil || replay.Run.RunID != first.Run.RunID {
		t.Fatalf("replay=%#v err=%v", replay, err)
	}
	if _, err = c.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: "@other:example.org", PlanID: planID, PlanRevision: 1, IdempotencyKey: cmd.IdempotencyKey}); err == nil {
		t.Fatal("cross-owner run unexpectedly authorized")
	}

	// Rejection follows the same binding/CAS path but must never queue a task
	// or invoke a provider.  Its replay is exact and the resulting run is
	// terminal only because this fixture has one root stage.
	rejectedRun := first
	rejected, err := c.RejectStage(ctx, coreexecution.RejectStageCommand{OwnerID: owner, ConfirmationID: rejectedRun.Confirmations[0].ID, ExpectedRevision: rejectedRun.Confirmations[0].Revision, IdempotencyKey: "44444444-4444-4444-8444-444444444444"})
	if err != nil || rejected.State != "rejected" {
		t.Fatalf("reject=%#v err=%v", rejected, err)
	}
	if replay, err := c.RejectStage(ctx, coreexecution.RejectStageCommand{OwnerID: owner, ConfirmationID: rejected.ID, ExpectedRevision: rejectedRun.Confirmations[0].Revision, IdempotencyKey: "44444444-4444-4444-8444-444444444444"}); err != nil || replay.Revision != rejected.Revision {
		t.Fatalf("reject replay=%#v err=%v", replay, err)
	}
	if _, err := c.RejectStage(ctx, coreexecution.RejectStageCommand{OwnerID: owner, ConfirmationID: rejected.ID, ExpectedRevision: rejected.Revision, IdempotencyKey: "55555555-5555-4555-8555-555555555555"}); err == nil {
		t.Fatal("rejected confirmation accepted CAS drift")
	}
	assertExecutionRunHistory(t, ctx, store.DB(), owner, rejectedRun.Run.RunID, 3, coreexecution.RunRejected)
	// The card remains a valid immutable historical projection even though its
	// run head moved from the bound revision 2 to terminal revision 3.
	queryStore := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	if got, e := queryStore.GetV2Confirmation(ctx, owner, rejected.ID); e != nil || got.Confirmation.State != "rejected" || got.Preview.RunRevision != rejectedRun.Run.Revision {
		t.Fatalf("historical confirmation get=%#v err=%v", got, e)
	}
	if page, e := queryStore.ListV2Confirmations(ctx, owner, "", nil, 10); e != nil || len(page.Items) == 0 || page.Items[0].Confirmation.ID != rejected.ID {
		t.Fatalf("historical confirmation list=%#v err=%v", page, e)
	}

	// Cancel is pre-dispatch only.  Retry must materialize a different graph
	// with retry trigger and must not reuse its canceled confirmation.
	canceledRun, err := c.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: owner, PlanID: planID, PlanRevision: 1, IdempotencyKey: "66666666-6666-4666-8666-666666666666"})
	if err != nil {
		t.Fatal(err)
	}
	canceled, err := c.CancelRun(ctx, coreexecution.CancelRunCommand{OwnerID: owner, RunID: canceledRun.Run.RunID, ExpectedRevision: canceledRun.Run.Revision, IdempotencyKey: "77777777-7777-4777-8777-777777777777"})
	if err != nil || canceled.Status != coreexecution.RunCanceled || canceled.Revision != 3 {
		t.Fatalf("cancel=%#v err=%v", canceled, err)
	}
	if replay, err := c.CancelRun(ctx, coreexecution.CancelRunCommand{OwnerID: owner, RunID: canceledRun.Run.RunID, ExpectedRevision: canceledRun.Run.Revision, IdempotencyKey: "77777777-7777-4777-8777-777777777777"}); err != nil || replay.Revision != canceled.Revision {
		t.Fatalf("cancel replay=%#v err=%v", replay, err)
	}
	retry, err := c.RetryRun(ctx, coreexecution.RetryRunCommand{OwnerID: owner, RunID: canceledRun.Run.RunID, ExpectedRevision: canceled.Revision, IdempotencyKey: "88888888-8888-4888-8888-888888888888"})
	if err != nil || retry.Run.RunID == canceledRun.Run.RunID || retry.Run.TriggerKind != coreexecution.TriggerRetry || len(retry.Confirmations) != 1 || retry.Confirmations[0].ID == canceledRun.Confirmations[0].ID {
		t.Fatalf("retry=%#v err=%v", retry, err)
	}
	if replay, err := c.RetryRun(ctx, coreexecution.RetryRunCommand{OwnerID: owner, RunID: canceledRun.Run.RunID, ExpectedRevision: canceled.Revision, IdempotencyKey: "88888888-8888-4888-8888-888888888888"}); err != nil || replay.Run.RunID != retry.Run.RunID {
		t.Fatalf("retry replay=%#v err=%v", replay, err)
	}
	if _, err = c.CancelRun(ctx, coreexecution.CancelRunCommand{OwnerID: owner, RunID: retry.Run.RunID, ExpectedRevision: retry.Run.Revision, IdempotencyKey: "99999999-9999-4999-8999-999999999998"}); err != nil {
		t.Fatal(err)
	}
	confirmedRun, err := c.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: owner, PlanID: planID, PlanRevision: 1, IdempotencyKey: "99999999-9999-4999-8999-999999999999"})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := c.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{OwnerID: owner, ConfirmationID: confirmedRun.Confirmations[0].ID, ExpectedRevision: confirmedRun.Confirmations[0].Revision, IdempotencyKey: "12121212-1212-4212-8212-121212121212"})
	if err != nil || confirmed.State != "confirmed" {
		t.Fatalf("confirm=%#v err=%v", confirmed, err)
	}
	claimStore := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	claim, err := claimStore.ClaimQueuedExecutionStage(ctx, owner, "confirmation-worker", time.Minute)
	if err != nil || claim.RunID != confirmedRun.Run.RunID {
		t.Fatalf("claim=%#v err=%v", claim, err)
	}
	if got, e := claimStore.GetV2Confirmation(ctx, owner, confirmed.ID); e != nil || got.Confirmation.State != "consumed" {
		t.Fatalf("consumed=%#v err=%v", got, e)
	}
	if _, err = claimStore.ClaimQueuedExecutionStage(ctx, owner, "restart-worker", time.Minute); err == nil {
		t.Fatal("consumed confirmation replayed")
	}
}

func assertExecutionRunHistory(t *testing.T, ctx context.Context, db interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, owner, runID string, revision uint64, want coreexecution.RunStatus) {
	t.Helper()
	var rowStatus string
	var snapshot []byte
	if err := db.QueryRowContext(ctx, `SELECT status,snapshot_json FROM core_execution_run_revisions WHERE owner_id=$1 AND run_id=$2 AND revision=$3`, owner, runID, revision).Scan(&rowStatus, &snapshot); err != nil {
		t.Fatal(err)
	}
	var stored coreexecution.ExecutionRun
	if err := json.Unmarshal(snapshot, &stored); err != nil || stored.Revision != revision || stored.Status != want || rowStatus != string(want) {
		t.Fatalf("history revision=%d row=%s snapshot=%s run=%#v err=%v", revision, rowStatus, snapshot, stored, err)
	}
}
