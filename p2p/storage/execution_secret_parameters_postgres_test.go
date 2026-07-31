package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionrunner"
)

type fixedCredentialRevisionResolver struct{ credential coreaws.Credentials }

func (r fixedCredentialRevisionResolver) ResolveCredentialRevision(_ context.Context, _ string, id string, revision uint64) (coreaws.Credentials, error) {
	if r.credential.ID != id || uint64(r.credential.Revision) != revision {
		return coreaws.Credentials{}, coreaws.ErrNotFound
	}
	return r.credential, nil
}

func TestExecutionSecretParameterRuntimeDurableAuthorizationLifecycle(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	database := openExecutionV2Schema(t)
	secretStore := NewDatabaseExecutionSecretStore(database.DB(), executionSecretTestEnveloper(t), func() time.Time { return now })
	meta, err := secretStore.CreateExecutionSecret(ctx, ExecutionSecretCreateRequest{
		OwnerID:       "@secret-parameter:example.test",
		Provider:      "openai",
		Purpose:       coreexecution.AISecretPurposeProviderAPIKey,
		Value:         "sk-runtime-only-not-in-ledger",
		IdempotencyID: "01010101-0101-4101-8101-010101010101",
	})
	if err != nil {
		t.Fatal(err)
	}

	credential := coreaws.RehydrateCredentials(
		"02020202-0202-4202-8202-020202020202", "aws", "us-east-1", "123456789012",
		"arn:aws:iam::123456789012:user/execution", []byte("AKIATEST"), []byte("aws-secret"), nil,
		1, 1, now, now,
	)
	awsRef := coreexecution.CredentialRef{Ref: credential.ID, Purpose: "aws_control", Revision: 1}
	awsRef.BindingDigest, err = coreaws.CredentialBindingDigest(meta.OwnerID, awsRef, credential)
	if err != nil {
		t.Fatal(err)
	}
	target, err := (coreexecution.ExecutionTarget{
		ID: "03030303-0303-4303-8303-030303030303", Provider: "aws", Kind: coreexecution.TargetKindAWSEC2Instance,
		AccountID: credential.AccountID, Region: credential.Region, CredentialRefs: []coreexecution.CredentialRef{awsRef}, Revision: 1,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	secretRef := coreexecution.CredentialRef{Ref: meta.SecretRef, Revision: meta.Revision, Purpose: meta.Purpose, BindingDigest: meta.BindingDigest}
	artifact := coreexecution.ArtifactRef{ID: "04040404-0404-4404-8404-040404040404", Digest: coreexecution.Digest(strings.Repeat("4", 64)), Immutable: true, Size: 1}
	observation := &coreexecution.TargetObservationRef{ObservationID: "05050505-0505-4505-8505-050505050505", TargetID: target.ID, TargetRevision: target.Revision, ObservationDigest: coreexecution.Digest(strings.Repeat("5", 64))}
	post := &coreexecution.Postcondition{Type: "exit_code", Value: "0"}
	consumer := coreexecution.ExecutionStep{
		StepKey: "run", Kind: coreexecution.StepScriptRun, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest,
		ObservationRef: observation, ArtifactRefs: []coreexecution.ArtifactRef{artifact}, SecretRefs: []coreexecution.CredentialRef{secretRef},
		TimeoutSeconds: 30, IdempotencyMarker: "run", OutputPolicy: coreexecution.OutputCapture, Postcondition: post,
		ScriptRun: &coreexecution.ScriptRunStep{Artifact: artifact, Interpreter: "/bin/sh", Argv: []string{"-e"}, CWD: "/", SecretRefs: []coreexecution.CredentialRef{secretRef}, AllowedExitCodes: []int{0}, Root: true, TimeoutSeconds: 30, OutputLimit: 1024, Redaction: coreexecution.RedactionPolicy{Patterns: []string{"secret"}, Replace: "[REDACTED]"}, Postcondition: post, IdempotencyMarker: "run"},
	}
	provision := coreexecution.ExecutionStep{
		StepKey: "provision-secret", Kind: coreexecution.StepSecretProvision, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest,
		SecretRefs: []coreexecution.CredentialRef{secretRef}, TimeoutSeconds: 30, IdempotencyMarker: "provision-secret", OutputPolicy: coreexecution.OutputDiscard,
		SecretProvision: &coreexecution.SecretProvisionStep{Delivery: coreaws.SecretParameterDeliveryTargetSecure},
	}
	quote := coreexecution.CostQuote{Amount: "1", Currency: "USD", ExpiresAt: now.Add(time.Hour)}
	placement := coreexecution.PlacementOption{Region: target.Region, Spec: "t3.small", Disk: "20GiB", Network: "private", CostQuote: quote}
	analysis := coreexecution.ProjectAnalysis{AnalysisID: "06060606-0606-4606-8606-060606060606", ProjectID: "07070707-0707-4707-8707-070707070707", Source: coreexecution.SourceRef{Kind: "git_https", Location: "https://example.test/repo", Commit: strings.Repeat("a", 40), Immutable: true}, Revision: 1, CreatedAt: now.Add(-time.Minute), UpdatedAt: now}
	plan := coreexecution.ExecutionPlan{
		SchemaVersion: coreexecution.SchemaVersion, ID: "08080808-0808-4808-8808-080808080808", Revision: 1, OwnerID: meta.OwnerID, ProjectID: analysis.ProjectID, AnalysisID: analysis.AnalysisID, Purpose: coreexecution.PurposeJob,
		Placement: coreexecution.PlacementRecommendation{Kind: "existing_target", Minimum: placement, Recommended: placement, HighPerformance: placement}, Targets: []coreexecution.ExecutionTarget{target}, Artifacts: []coreexecution.ArtifactRef{artifact},
		AIConfiguration: &coreexecution.AIConfiguration{Mode: coreexecution.AIAuthModeAPIKey, Provider: "openai", SecretRef: secretRef.Ref, SecretRevision: secretRef.Revision, SecretPurpose: secretRef.Purpose, SecretBindingDigest: secretRef.BindingDigest},
		Stages: []coreexecution.ExecutionStage{
			{StageKey: "authorize-secret", Revision: 1, Kind: string(coreexecution.StepSecretProvision), Risk: coreexecution.RiskR2, Gate: coreexecution.GateSecretAccess, Effects: []coreexecution.Gate{coreexecution.GateSecretAccess}, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest, Steps: []coreexecution.ExecutionStep{provision}, TimeoutSeconds: 60},
			{StageKey: "run", Revision: 1, Kind: "run", Risk: coreexecution.RiskR2, Gate: coreexecution.GateRemotePrivilegedExecution, Effects: []coreexecution.Gate{coreexecution.GateRemotePrivilegedExecution, coreexecution.GateSecretAccess}, DependsOn: []string{"authorize-secret"}, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest, Steps: []coreexecution.ExecutionStep{consumer}, TimeoutSeconds: 60},
		}, Status: coreexecution.PlanReady, CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	}
	executionStore := NewDatabaseExecutionStore(database.DB(), func() time.Time { return now })
	createdPlan, err := executionStore.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: meta.OwnerID, Analysis: analysis, Plan: plan, IdempotencyID: "09090909-0909-4909-8909-090909090909"})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewDatabaseExecutionCoordinator(database.DB(), func() time.Time { return now })
	debugRunID := coreexecution.DeterministicRunID(meta.OwnerID, "10101010-1010-4010-8010-101010101010")
	debugRunDigest, _ := coreexecution.CanonicalDigest(struct {
		Plan coreexecution.Digest
		ID   string
	}{createdPlan.Digest, debugRunID})
	debugRun := coreexecution.ExecutionRun{RunID: debugRunID, OwnerID: meta.OwnerID, Operation: coreexecution.RunOperationExecute, TriggerKind: coreexecution.TriggerManual, PlanID: createdPlan.ID, ProjectID: createdPlan.ProjectID, Purpose: createdPlan.Purpose, PlanRevision: createdPlan.Revision, PlanDigest: createdPlan.Digest, RunDigest: debugRunDigest, Status: coreexecution.RunWaitingUser, Revision: 2, CreatedAt: now, UpdatedAt: now}
	if err = debugRun.Validate(); err != nil {
		t.Fatalf("debug run invalid: %v", err)
	}
	if _, err = coreexecution.BuildConfirmationPreview(createdPlan, debugRun, createdPlan.Stages[0]); err != nil {
		t.Fatalf("debug preview invalid: %v", err)
	}
	loadedPlan, err := executionStore.GetPlanRevision(ctx, meta.OwnerID, createdPlan.ID, createdPlan.Revision)
	if err != nil {
		t.Fatalf("load plan: %v", err)
	}
	if _, err = coreexecution.BuildConfirmationPreview(loadedPlan, debugRun, loadedPlan.Stages[0]); err != nil {
		t.Fatalf("loaded preview invalid: %v ai=%+v", err, loadedPlan.AIConfiguration)
	}
	materialized, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: meta.OwnerID, PlanID: createdPlan.ID, PlanRevision: createdPlan.Revision, IdempotencyKey: "10101010-1010-4010-8010-101010101010"})
	if err != nil || len(materialized.Confirmations) != 1 {
		t.Fatalf("materialized=%+v err=%v", materialized, err)
	}
	if _, err = coordinator.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{OwnerID: meta.OwnerID, ConfirmationID: materialized.Confirmations[0].ID, ExpectedRevision: materialized.Confirmations[0].Revision, IdempotencyKey: "11111111-1111-4111-8111-111111111111"}); err != nil {
		t.Fatal(err)
	}
	claim, err := executionStore.ClaimQueuedExecutionStage(ctx, meta.OwnerID, "secret-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	next, err := executionStore.NextExecutableStep(ctx, claim.OwnerID, claim.RunID, claim.StageID)
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewExecutionStepResolver(executionStore, fixedCredentialRevisionResolver{credential: credential}, nil)
	prepared, err := resolver.ResolveStep(ctx,
		executionrunner.StageLease{OwnerID: claim.OwnerID, RunID: claim.RunID, StageID: claim.StageID, TaskID: claim.TaskID, Holder: claim.Holder, Attempt: claim.Attempt, LeaseEpoch: claim.LeaseEpoch, TaskLeaseEpoch: claim.TaskLeaseEpoch, ExpectedTaskRevision: claim.ExpectedTaskRevision, LeaseID: claim.LeaseID, LeaseToken: claim.LeaseToken, ExpiresAt: claim.ExpiresAt},
		executionrunner.NextStep{OwnerID: next.OwnerID, RunID: next.RunID, StageID: next.StageID, StepKey: next.StepKey, StepSet: next.StepSet, StepRevision: next.StepRevision, StepDigest: next.StepDigest},
	)
	if err != nil || prepared.SecretProvision == nil {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	request := *prepared.SecretProvision
	request.Credential = coreaws.RehydrateCredentialMetadata(credential.ID, credential.Name, credential.Region, credential.AccountID, credential.UserARN, credential.VerifiedRevision, credential.Revision, credential.CreatedAt, credential.UpdatedAt)
	if err = executionStore.RecordDispatchIntent(ctx, ExecutionDispatchIntent{Attempt: prepared.Attempt, Receipt: prepared.Receipt, TaskID: claim.TaskID, TaskHolder: claim.Holder, TaskAttempt: claim.Attempt, TaskRevision: claim.ExpectedTaskRevision, TaskLeaseEpoch: claim.TaskLeaseEpoch, TargetID: request.Target.ID, TargetRevision: request.Target.Revision, TargetDigest: request.Target.Digest, LeaseID: claim.LeaseID, LeaseToken: claim.LeaseToken, LeaseEpoch: claim.LeaseEpoch, StepSet: coreexecution.StepSetForward, RequestDigest: request.RequestDigest, FenceDigest: request.FenceDigest, SecretProvision: &request}); err != nil {
		t.Fatal(err)
	}
	// Later DAG stages remain bound to the immutable run revision recorded in
	// their confirmation, while the mutable run head advances as earlier stages
	// are claimed. Secret authorization must compare the request to that bound
	// revision and merely require the current head not to be older.
	advanceSecretTestRunHead(t, ctx, database.DB(), request.OwnerID, request.RunID, now.Add(time.Microsecond))

	runtime := NewDatabaseExecutionSecretParameterRuntime(database.DB(), secretStore)
	authorized, err := runtime.ResolveAuthorizedSecretValues(ctx, authorizedRequestFromProvision(request))
	if err != nil || len(authorized.Values) != 1 || string(authorized.Values[0].Value) != "sk-runtime-only-not-in-ledger" {
		t.Fatalf("authorized=%+v err=%v", authorized, err)
	}
	clear(authorized.Values[0].Value)
	parameterName, err := coreaws.SecretParameterName(request.Target.ID, request.AttemptID, request.SecretRef)
	if err != nil {
		t.Fatal(err)
	}
	intent := coreaws.SecretParameterIntent{OwnerID: request.OwnerID, ParameterName: parameterName, FenceDigest: request.FenceDigest, RequestDigest: request.RequestDigest, Request: request}
	record, created, err := runtime.ReserveSecretParameterIntent(ctx, intent)
	if err != nil || !created || record.Status != "reserved" {
		t.Fatalf("record=%+v created=%v err=%v", record, created, err)
	}
	var receiptStatus, receiptProviderOperation, leaseProviderOperation, leaseReceipt string
	if err = database.DB().QueryRowContext(ctx, `SELECT r.status,r.provider_operation_id,l.provider_operation_id,COALESCE(l.receipt_id::text,'') FROM core_execution_receipts r JOIN core_execution_target_mutation_leases l ON l.owner_id=r.owner_id AND l.run_id=r.run_id AND l.stage_id=$3 WHERE r.owner_id=$1 AND r.receipt_id=$2`, request.OwnerID, prepared.Receipt.ReceiptID, request.StageID).Scan(&receiptStatus, &receiptProviderOperation, &leaseProviderOperation, &leaseReceipt); err != nil || receiptStatus != "running" || receiptProviderOperation != parameterName || leaseProviderOperation != parameterName || leaseReceipt != prepared.Receipt.ReceiptID {
		t.Fatalf("provider handle receipt=%s/%s lease=%s/%s err=%v", receiptStatus, receiptProviderOperation, leaseProviderOperation, leaseReceipt, err)
	}
	if replay, replayed, replayErr := runtime.ReserveSecretParameterIntent(ctx, intent); replayErr != nil || replayed || replay.Intent.RequestDigest != record.Intent.RequestDigest {
		t.Fatalf("replay=%+v created=%v err=%v", replay, replayed, replayErr)
	}
	if record, err = runtime.RecordSecretParameterVersion(ctx, request.OwnerID, request.FenceDigest, 7); err != nil || record.ProviderVersion != 7 || record.Status != "versioned" {
		t.Fatalf("versioned=%+v err=%v", record, err)
	}
	mount, err := coreaws.SecretContainerMountPath(request.SecretRef)
	if err != nil {
		t.Fatal(err)
	}
	lease := coreaws.SecretParameterLease{SchemaVersion: "execution-secret-parameter/v1", OwnerID: request.OwnerID, RunID: request.RunID, ProvisionStageID: request.StageID, ProvisionAttemptID: request.AttemptID, Authorization: authorized.Authorization, TargetID: request.Target.ID, TargetRevision: request.Target.Revision, TargetDigest: request.Target.Digest, SecretRef: request.SecretRef, ParameterName: parameterName, ContainerMountPath: mount, FenceDigest: request.FenceDigest, RequestDigest: request.RequestDigest, ProviderVersion: 7}
	if err = runtime.CompleteSecretParameter(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if err = executionStore.FinalizeDispatchReceipt(ctx, request.OwnerID, prepared.Receipt.ReceiptID, prepared.Attempt.AttemptID, coreexecution.ReceiptSucceeded, mustCanonicalDigest(lease)); err != nil {
		t.Fatalf("finalize provider-typed secret receipt: %v", err)
	}
	if got, err := runtime.ResolveActiveSecretParameterLease(ctx, request.OwnerID, request.FenceDigest); err != nil || got != lease {
		t.Fatalf("lease=%+v err=%v", got, err)
	}
	if _, err = secretStore.RevokeExecutionSecret(ctx, ExecutionSecretRevokeRequest{OwnerID: meta.OwnerID, SecretRef: meta.SecretRef, ExpectedRevision: meta.Revision, IdempotencyID: "12121212-1212-4212-8212-121212121212"}); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("active lease did not block secret revoke: %v", err)
	}
	var persisted string
	if err = database.DB().QueryRowContext(ctx, `SELECT request_json::text||COALESCE(lease_json::text,'') FROM core_execution_secret_parameter_intents WHERE owner_id=$1 AND fence_digest=$2`, request.OwnerID, request.FenceDigest).Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted, "sk-runtime-only-not-in-ledger") || strings.Contains(persisted, "aws-secret") || strings.Contains(persisted, "AKIATEST") {
		t.Fatalf("secret material persisted: %s", persisted)
	}
	if err = runtime.RevokeSecretParameter(ctx, request.OwnerID, request.FenceDigest); err != nil {
		t.Fatal(err)
	}
}

func advanceSecretTestRunHead(t *testing.T, ctx context.Context, db *sql.DB, owner, runID string, at time.Time) {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var raw []byte
	var revision uint64
	if err = tx.QueryRowContext(ctx, `SELECT revision,snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, owner, runID).Scan(&revision, &raw); err != nil {
		t.Fatal(err)
	}
	var run coreexecution.ExecutionRun
	if strictJSON(raw, &run) != nil || run.Revision != revision || run.Status != coreexecution.RunRunning {
		t.Fatal("running secret test run snapshot drifted")
	}
	run.Revision++
	run.UpdatedAt = at.UTC()
	if err = run.Validate(); err != nil {
		t.Fatal(err)
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_execution_runs SET revision=$1,snapshot_json=$2,updated_at=$3 WHERE owner_id=$4 AND run_id=$5 AND revision=$6 AND status='running'`, run.Revision, mustJSON(run), run.UpdatedAt, owner, runID, revision)
	if err != nil {
		t.Fatal(err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		t.Fatalf("advance run head changed=%d err=%v", changed, rowsErr)
	}
	if err = insertExecutionRunRevision(ctx, tx, run); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
