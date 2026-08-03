package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionrunner"
)

type reconcileCanceledCredentialResolver struct {
	owner      string
	credential coreaws.Credentials
}

func (r reconcileCanceledCredentialResolver) ResolveCredentialRevision(_ context.Context, owner, id string, revision uint64) (coreaws.Credentials, error) {
	if owner != r.owner || id != r.credential.ID || revision != uint64(r.credential.Revision) {
		return coreaws.Credentials{}, coreaws.ErrNotFound
	}
	return r.credential, nil
}

type reconcileCanceledSSM struct {
	dispatch, reconcile int
}

func (f *reconcileCanceledSSM) Dispatch(_ context.Context, in coreaws.FrozenRequest) (coreaws.DispatchResult, error) {
	f.dispatch++
	return coreaws.DispatchResult{
		Status:        coreaws.DispatchAccepted,
		CommandID:     "cmd-" + in.AttemptID,
		RequestDigest: in.RequestDigest,
		TargetID:      in.TargetID,
		InstanceID:    in.InstanceID,
	}, nil
}

func (f *reconcileCanceledSSM) Poll(_ context.Context, in coreaws.PollRequest) (coreaws.CommandResult, error) {
	return coreaws.CommandResult{Status: coreaws.PollUncertain, CommandID: in.CommandID, InstanceID: in.Frozen.InstanceID}, coreaws.ErrTypedUncertain
}

func (f *reconcileCanceledSSM) ReconcileCommand(_ context.Context, in coreaws.PollRequest) (coreaws.ReconcileResult, error) {
	f.reconcile++
	return coreaws.ReconcileResult{CommandResult: coreaws.CommandResult{
		Status:       coreaws.PollCanceled,
		CommandID:    in.CommandID,
		InstanceID:   in.Frozen.InstanceID,
		OutputDigest: coreexecution.Digest(strings.Repeat("c", 64)),
	}}, nil
}

var _ executionrunner.TypedSSMTransport = (*reconcileCanceledSSM)(nil)

func TestExecutionReconcileCanceledMultiRootCommitsResolutionWithoutRedispatch(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 4, 5, 6, 7, 8, 0, time.UTC)
	store := openExecutionV2Schema(t)
	executionStore := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now })
	owner := "@reconcile-canceled-multi-root:example.org"
	projectID := "11111111-1111-4111-8111-111111111101"
	targetID := "22222222-2222-4222-8222-222222222202"
	credentialID := "33333333-3333-4333-8333-333333333303"
	observationID := "44444444-4444-4444-8444-444444444404"
	artifactID := "55555555-5555-4555-8555-555555555505"
	planID := "66666666-6666-4666-8666-666666666606"

	credential := coreaws.RehydrateCredentials(
		credentialID, "reconcile-canceled", "us-east-1", "123456789012",
		"arn:aws:iam::123456789012:role/reconcile-canceled", nil, nil, nil,
		1, 1, now, now,
	)
	credentialRef := coreexecution.CredentialRef{Ref: credentialID, Purpose: "aws", Revision: 1}
	var err error
	credentialRef.BindingDigest, err = coreaws.CredentialBindingDigest(owner, credentialRef, credential)
	if err != nil {
		t.Fatal(err)
	}
	target, err := coreaws.NormalizeInfrastructureTarget(coreexecution.ExecutionTarget{
		ID: targetID, Provider: "aws", Kind: coreexecution.TargetKindAWSEC2Instance,
		InfrastructureProfileID: coreaws.InfrastructureProfileGeneralLinuxSSMV1,
		AccountID:               "123456789012", Region: "us-east-1", Architecture: "x86_64",
		Capabilities:   []string{"target.aws_ec2_instance", "target.instance.i-0123456789abcdef0", "transport.aws_ssm"},
		CredentialRefs: []coreexecution.CredentialRef{credentialRef},
		Network:        coreexecution.NetworkPolicy{Mode: coreexecution.NetworkPolicyModeObservedHTTPSEgress},
		Revision:       1,
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err = executionStore.CreateTarget(ctx, TargetCreateRequest{OwnerID: owner, Target: target, IdempotencyID: "77777777-7777-4777-8777-777777777707"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := executionStore.CreateTargetObservation(ctx, TargetObservationCreateRequest{
		OwnerID: owner, ObservationID: observationID,
		Observation: coreexecution.TargetObservation{
			TargetID: target.ID, TargetRevision: target.Revision, ObservedAt: now, State: "ready",
			Facts: map[string]string{
				"instance_id": "i-0123456789abcdef0", "account_id": target.AccountID, "region": target.Region,
				"partition": "aws", "operating_system": "linux", "architecture": target.Architecture,
				"ssm_status": "Online", "platform_name": "Amazon Linux", "platform_version": "2023",
			},
		},
		IdempotencyID: "88888888-8888-4888-8888-888888888808",
	})
	if err != nil {
		t.Fatal(err)
	}
	artifactDigest := coreexecution.Digest(strings.Repeat("a", 64))
	artifact := coreexecution.ArtifactRef{ID: artifactID, Digest: artifactDigest, MediaType: "application/x-sh", Size: 1, Immutable: true}
	postcondition := &coreexecution.Postcondition{Type: "exit_code", Value: "0"}
	firstStep := coreexecution.ExecutionStep{
		StepKey: "cancel-root-step", Kind: coreexecution.StepHTTPProbe, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest,
		ObservationRef: &coreexecution.TargetObservationRef{ObservationID: observationID, TargetID: target.ID, TargetRevision: target.Revision, ObservationDigest: observation.Observation.Digest},
		NetworkGrants:  []coreexecution.NetworkGrant{{Scheme: "http", Host: "127.0.0.1", Port: 8080, Scope: "target_local", PathPrefix: "/"}},
		TimeoutSeconds: 30, IdempotencyMarker: "cancel-root-marker", OutputPolicy: coreexecution.OutputDiscard, Postcondition: postcondition,
		HTTPProbe: &coreexecution.HTTPProbeStep{URL: "http://127.0.0.1:8080/", Mode: "target_local"},
		Executor:  &coreexecution.ExecutorSpec{Artifact: artifact, Interpreter: "/bin/sh", Argv: []string{"-e"}, CWD: "/", AllowedExitCodes: []int{0}, OutputLimit: 1024, Postcondition: postcondition},
	}
	secondStep := coreexecution.ExecutionStep{
		StepKey: "queued-root-step", Kind: coreexecution.StepScriptRun, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest,
		ObservationRef: &coreexecution.TargetObservationRef{ObservationID: observationID, TargetID: target.ID, TargetRevision: target.Revision, ObservationDigest: observation.Observation.Digest},
		TimeoutSeconds: 30, IdempotencyMarker: "queued-root-marker", OutputPolicy: coreexecution.OutputDiscard, Postcondition: postcondition,
		ScriptRun: &coreexecution.ScriptRunStep{Artifact: artifact, Interpreter: "/bin/sh", Argv: []string{"-e"}, CWD: "/", Root: true, AllowedExitCodes: []int{0}, TimeoutSeconds: 30, OutputLimit: 1024, Postcondition: postcondition, IdempotencyMarker: "queued-root-marker"},
	}
	firstStage := coreexecution.ExecutionStage{StageKey: "cancel-root", Revision: 1, Kind: "execute", Risk: coreexecution.RiskR0, Gate: coreexecution.GateNone, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest, Steps: []coreexecution.ExecutionStep{firstStep}, TimeoutSeconds: 60}
	secondStage := coreexecution.ExecutionStage{StageKey: "queued-root", Revision: 1, Kind: "execute", Risk: coreexecution.RiskR2, Gate: coreexecution.GateRemotePrivilegedExecution, Effects: []coreexecution.Gate{coreexecution.GateRemotePrivilegedExecution}, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest, Steps: []coreexecution.ExecutionStep{secondStep}, TimeoutSeconds: 60}
	analysis := coreexecution.ProjectAnalysis{AnalysisID: "99999999-9999-4999-8999-999999999909", ProjectID: projectID, Source: coreexecution.SourceRef{Kind: "git_https", Location: "https://example.org/reconcile-canceled", Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	quote := coreexecution.CostQuote{Amount: "1", Currency: "USD", ExpiresAt: now.Add(time.Hour)}
	placement := coreexecution.PlacementOption{Region: target.Region, Spec: "t3.small", Disk: "20GiB", Network: "private", CostQuote: quote}
	plan := coreexecution.ExecutionPlan{SchemaVersion: coreexecution.SchemaVersion, ID: planID, Revision: 1, OwnerID: owner, ProjectID: projectID, AnalysisID: analysis.AnalysisID, Purpose: coreexecution.PurposeJob, Placement: coreexecution.PlacementRecommendation{Kind: "existing_target", Minimum: placement, Recommended: placement, HighPerformance: placement}, Targets: []coreexecution.ExecutionTarget{target}, Artifacts: []coreexecution.ArtifactRef{artifact}, Stages: []coreexecution.ExecutionStage{firstStage, secondStage}, Status: coreexecution.PlanReady, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	plan, err = plan.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = executionStore.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: owner, Analysis: analysis, Plan: plan, IdempotencyID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}); err != nil {
		t.Fatal(err)
	}

	coordinator := NewDatabaseExecutionCoordinator(store.DB(), func() time.Time { return now })
	materialized, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: owner, PlanID: plan.ID, PlanRevision: 1, Operation: coreexecution.RunOperationExecute, IdempotencyKey: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"})
	if err != nil || len(materialized.Tasks) != 2 || len(materialized.Confirmations) != 1 {
		t.Fatalf("materialized=%#v err=%v", materialized, err)
	}
	confirmation := materialized.Confirmations[0]
	if _, err = coordinator.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{OwnerID: owner, ConfirmationID: confirmation.ID, ExpectedRevision: confirmation.Revision, IdempotencyKey: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}); err != nil {
		t.Fatal(err)
	}
	// Keep the unrelated root queued but out of this worker's immediate claim;
	// it remains active evidence that prevents aggregate cancellation.
	var queuedTaskID string
	for _, task := range materialized.Tasks {
		if task.Spec.Payload.ExecutionStage != nil && task.Spec.Payload.ExecutionStage.StageID == confirmation.Binding.StageID {
			queuedTaskID = task.ID
		}
	}
	if queuedTaskID == "" {
		t.Fatal("missing unrelated queued root task")
	}
	if _, err = store.DB().ExecContext(ctx, `UPDATE agent_tasks SET available_at=$1 WHERE owner_id=$2 AND task_id=$3`, now.Add(time.Hour), owner, queuedTaskID); err != nil {
		t.Fatal(err)
	}

	credentials := reconcileCanceledCredentialResolver{owner: owner, credential: credential}
	stepResolver := NewExecutionStepResolver(executionStore, credentials, nil)
	receiptResolver := NewDatabaseDispatchReceiptResolver(executionStore, credentials)
	transport := &reconcileCanceledSSM{}
	stageStore := NewExecutionStageStoreAdapter(executionStore)
	runner, err := executionrunner.NewRunner(executionrunner.Config{Store: stageStore, Resolver: stepResolver, Transport: transport, Holder: "reconcile-canceled-runner", LeaseTTL: time.Minute, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err = runner.RunOnce(ctx); !errors.Is(err, executionrunner.ErrRunnerUncertain) {
		t.Fatalf("uncertain run=%v", err)
	}
	lost, err := executionStore.GetExecutionRun(ctx, owner, materialized.Run.RunID)
	if err != nil || lost.Run.Status != coreexecution.RunUncertain || lost.Stages[0].Status != coreexecution.StageUncertain {
		t.Fatalf("lost=%#v err=%v", lost, err)
	}
	if transport.dispatch != 1 {
		t.Fatalf("dispatches=%d want=1", transport.dispatch)
	}

	reconciler := NewExecutionReconciler(executionStore, receiptResolver, transport, nil, func() time.Time { return now })
	if reconciler == nil {
		t.Fatal("reconciler was not constructed")
	}
	command := ExecutionSSMReconcileCommand{OwnerID: owner, RunID: lost.Run.RunID, StageID: lost.Stages[0].StageID, ExpectedRevision: lost.Run.Revision, IdempotencyKey: "dddddddd-dddd-4ddd-8ddd-dddddddddddd"}
	resolved, err := reconciler.Reconcile(ctx, command)
	if err != nil || resolved.Status != coreexecution.RunRunning || resolved.Revision <= lost.Run.Revision {
		t.Fatalf("resolved=%#v err=%v", resolved, err)
	}
	view, err := executionStore.GetExecutionRun(ctx, owner, lost.Run.RunID)
	if err != nil || view.Run.Status != coreexecution.RunRunning || view.Stages[0].Status != coreexecution.StageCanceled || view.Stages[1].Status != coreexecution.StageQueued {
		t.Fatalf("reconciled view=%#v err=%v", view, err)
	}
	var outcome, leaseStatus string
	if err = store.DB().QueryRowContext(ctx, `SELECT outcome FROM core_execution_reconciliation_resolutions WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, owner, lost.Run.RunID, lost.Stages[0].StageID).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT status FROM core_execution_target_mutation_leases WHERE owner_id=$1 AND target_id=$2 AND target_revision=1`, owner, target.ID).Scan(&leaseStatus); err != nil {
		t.Fatal(err)
	}
	if outcome != string(coreaws.PollCanceled) || leaseStatus != "released" {
		t.Fatalf("resolution outcome=%q lease=%q", outcome, leaseStatus)
	}
	var taskStatus string
	var runningCount int
	if err = store.DB().QueryRowContext(ctx, `SELECT t.status FROM agent_tasks t JOIN core_execution_run_stages s ON s.owner_id=t.owner_id AND s.task_id=t.task_id WHERE s.owner_id=$1 AND s.run_id=$2 AND s.stage_id=$3`, owner, lost.Run.RunID, lost.Stages[0].StageID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT running_count FROM agent_task_runtime_concurrency WHERE singleton=true`).Scan(&runningCount); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "canceled" || runningCount != 0 {
		t.Fatalf("reconciled task status=%q running_count=%d, want canceled/0", taskStatus, runningCount)
	}
	replayed, err := reconciler.Reconcile(ctx, command)
	if err != nil || replayed.RunID != resolved.RunID || replayed.Revision != resolved.Revision || transport.reconcile != 1 || transport.dispatch != 1 {
		t.Fatalf("replay=%#v reconcile_calls=%d dispatches=%d err=%v", replayed, transport.reconcile, transport.dispatch, err)
	}

	// A crash after the durable SSM intent commit but before SendCommand can
	// leave an accepted receipt with no provider command id. A lease takeover
	// must rehydrate that exact intent as reconcile-only and permanently mark it
	// uncertain; it must never call the transport again.
	if _, err = store.DB().ExecContext(ctx, `UPDATE agent_tasks SET available_at=$1 WHERE owner_id=$2 AND task_id=$3 AND status='queued'`, now, owner, queuedTaskID); err != nil {
		t.Fatal(err)
	}
	claim2, err := executionStore.ClaimQueuedExecutionStage(ctx, owner, "reconcile-canceled-runner-2", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	next2, err := executionStore.NextExecutableStep(ctx, claim2.OwnerID, claim2.RunID, claim2.StageID)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := stepResolver.ResolveStep(ctx, executionrunner.StageLease{OwnerID: claim2.OwnerID, RunID: claim2.RunID, StageID: claim2.StageID, TaskID: claim2.TaskID, Holder: claim2.Holder, Attempt: claim2.Attempt, LeaseEpoch: claim2.LeaseEpoch, TaskLeaseEpoch: claim2.TaskLeaseEpoch, ExpectedTaskRevision: claim2.ExpectedTaskRevision, LeaseID: claim2.LeaseID, LeaseToken: claim2.LeaseToken, ExpiresAt: claim2.ExpiresAt}, executionrunner.NextStep{OwnerID: next2.OwnerID, RunID: next2.RunID, StageID: next2.StageID, StepKey: next2.StepKey, StepSet: next2.StepSet, StepRevision: next2.StepRevision, StepDigest: next2.StepDigest})
	if err != nil || initial.Frozen.RequestDigest == "" || initial.Receipt.SSMCommandID != "" {
		t.Fatalf("initial=%+v err=%v", initial, err)
	}
	if err = executionStore.RecordDispatchIntent(ctx, ExecutionDispatchIntent{Attempt: initial.Attempt, Receipt: initial.Receipt, TaskID: claim2.TaskID, TaskHolder: claim2.Holder, TaskAttempt: claim2.Attempt, TaskRevision: claim2.ExpectedTaskRevision, TaskLeaseEpoch: claim2.TaskLeaseEpoch, TargetID: initial.Frozen.TargetID, TargetRevision: initial.Frozen.TargetRevision, TargetDigest: initial.Frozen.TargetDigest, LeaseID: claim2.LeaseID, LeaseToken: claim2.LeaseToken, LeaseEpoch: claim2.LeaseEpoch, StepSet: next2.StepSet, RequestDigest: initial.Frozen.RequestDigest, FenceDigest: initial.Frozen.FenceDigest, Snapshot: coreaws.SnapshotFromFrozen(initial.Frozen)}); err != nil {
		t.Fatal(err)
	}
	// The takeover store observes the expired original task/target leases. Its
	// claim path includes the durable frozen SSM intent even though command_id
	// is empty, then hands the same stage to the resolver under a new lease.
	takeoverStore := NewDatabaseExecutionStore(store.DB(), func() time.Time { return now.Add(2 * time.Minute) })
	takeoverClaim, err := takeoverStore.ClaimNextExecutionStage(ctx, "reconcile-canceled-takeover", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	takeoverNext, err := takeoverStore.NextExecutableStep(ctx, takeoverClaim.OwnerID, takeoverClaim.RunID, takeoverClaim.StageID)
	if err != nil {
		t.Fatal(err)
	}
	takeoverPrepared, err := NewExecutionStepResolver(takeoverStore, credentials, nil).ResolveStep(ctx, executionrunner.StageLease{OwnerID: takeoverClaim.OwnerID, RunID: takeoverClaim.RunID, StageID: takeoverClaim.StageID, TaskID: takeoverClaim.TaskID, Holder: takeoverClaim.Holder, Attempt: takeoverClaim.Attempt, LeaseEpoch: takeoverClaim.LeaseEpoch, TaskLeaseEpoch: takeoverClaim.TaskLeaseEpoch, ExpectedTaskRevision: takeoverClaim.ExpectedTaskRevision, LeaseID: takeoverClaim.LeaseID, LeaseToken: takeoverClaim.LeaseToken, ExpiresAt: takeoverClaim.ExpiresAt}, executionrunner.NextStep{OwnerID: takeoverNext.OwnerID, RunID: takeoverNext.RunID, StageID: takeoverNext.StageID, StepKey: takeoverNext.StepKey, StepSet: takeoverNext.StepSet, StepRevision: takeoverNext.StepRevision, StepDigest: takeoverNext.StepDigest})
	if err != nil || !takeoverPrepared.ReconcileOnly || takeoverPrepared.Receipt.SSMCommandID != "" || takeoverPrepared.Attempt.AttemptID != initial.Attempt.AttemptID || takeoverPrepared.Frozen.RequestDigest != initial.Frozen.RequestDigest || takeoverPrepared.Frozen.FenceDigest != initial.Frozen.FenceDigest || takeoverPrepared.Frozen.StepDigest != initial.Frozen.StepDigest || takeoverPrepared.Frozen.TargetDigest != initial.Frozen.TargetDigest {
		t.Fatalf("takeover prepared=%+v initial=%+v err=%v", takeoverPrepared, initial, err)
	}
	takeoverTransport := &reconcileCanceledSSM{}
	takeoverRunner, err := executionrunner.NewRunner(executionrunner.Config{Store: NewExecutionStageStoreAdapter(takeoverStore), Resolver: NewExecutionStepResolver(takeoverStore, credentials, nil), Transport: takeoverTransport, Holder: "reconcile-canceled-takeover", LeaseTTL: time.Minute, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	takeoverLease := executionrunner.StageLease{OwnerID: takeoverClaim.OwnerID, RunID: takeoverClaim.RunID, StageID: takeoverClaim.StageID, TaskID: takeoverClaim.TaskID, Holder: takeoverClaim.Holder, Attempt: takeoverClaim.Attempt, LeaseEpoch: takeoverClaim.LeaseEpoch, TaskLeaseEpoch: takeoverClaim.TaskLeaseEpoch, ExpectedTaskRevision: takeoverClaim.ExpectedTaskRevision, LeaseID: takeoverClaim.LeaseID, LeaseToken: takeoverClaim.LeaseToken, ExpiresAt: takeoverClaim.ExpiresAt}
	if err = takeoverRunner.RunClaimed(ctx, takeoverLease); !errors.Is(err, executionrunner.ErrRunnerUncertain) {
		t.Fatalf("takeover runner error=%v, want permanent uncertainty", err)
	}
	if takeoverTransport.dispatch != 0 || takeoverTransport.reconcile != 0 {
		t.Fatalf("takeover dispatch=%d reconcile=%d, empty-command intent was sent/read back", takeoverTransport.dispatch, takeoverTransport.reconcile)
	}
}

func TestExecutionStepResolverEC2ProvisionUsesStageRunRevisionFence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 4, 6, 7, 8, 9, 0, time.UTC)
	database := openExecutionV2Schema(t)
	executionStore := NewDatabaseExecutionStore(database.DB(), func() time.Time { return now })
	owner := "@ec2-stage-fence:example.org"
	credential := coreaws.RehydrateCredentials(
		"11111111-1111-4111-8111-111111111111", "ec2-stage-fence", "us-east-1", "123456789012",
		"arn:aws:iam::123456789012:role/ec2-stage-fence", []byte("AKIATESTONLY"), []byte("fixture-secret"), nil,
		1, 1, now, now,
	)
	ref := coreexecution.CredentialRef{Ref: credential.ID, Purpose: "aws", Revision: 1}
	var err error
	ref.BindingDigest, err = coreaws.CredentialBindingDigest(owner, ref, credential)
	if err != nil {
		t.Fatal(err)
	}
	reservation := &coreexecution.ComputeReservation{
		InfrastructureProfileID: coreaws.InfrastructureProfileGeneralLinuxSSMV1,
		AMIParameter:            coreexecution.AWSAL2023X8664AMIParameter,
		InstanceType:            "t3.small",
		AvailabilityZone:        "us-east-1a",
		VolumeGiB:               20,
		Architecture:            "x86_64",
		ManagementTransport:     "aws_ssm",
		PublicIP:                true,
		CostQuote:               coreexecution.CostQuote{Amount: "0.02", Currency: "USD", ExpiresAt: now.Add(time.Hour)},
	}
	target, err := (coreexecution.ExecutionTarget{
		ID:                      "22222222-2222-4222-8222-222222222222",
		Provider:                "aws",
		Kind:                    coreexecution.TargetKindAWSComputeReservation,
		InfrastructureProfileID: coreaws.InfrastructureProfileGeneralLinuxSSMV1,
		AccountID:               credential.AccountID,
		Region:                  credential.Region,
		Architecture:            "x86_64",
		Capabilities:            []string{"compute.catalog", "compute.provision", "target.aws_compute_reservation"},
		CredentialRefs:          []coreexecution.CredentialRef{ref},
		Network:                 coreexecution.NetworkPolicy{Mode: "restricted"},
		ComputeReservation:      reservation,
		Revision:                1,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	step := coreexecution.ExecutionStep{
		StepKey:           "provision-compute",
		Kind:              coreexecution.StepComputeProvision,
		TargetID:          target.ID,
		TargetRevision:    target.Revision,
		TargetDigest:      target.Digest,
		TimeoutSeconds:    1800,
		IdempotencyMarker: "provision-compute",
		OutputPolicy:      coreexecution.OutputDiscard,
		ComputeProvision:  &coreexecution.ComputeProvisionStep{InfrastructureProfileID: reservation.InfrastructureProfileID, AMIParameter: reservation.AMIParameter, InstanceType: reservation.InstanceType, AvailabilityZone: reservation.AvailabilityZone, VolumeGiB: reservation.VolumeGiB, Region: target.Region, Architecture: reservation.Architecture, ManagementTransport: reservation.ManagementTransport, PublicIP: reservation.PublicIP, PublicInbound: reservation.PublicInbound},
	}
	stage := coreexecution.ExecutionStage{
		StageKey:       "provision-compute",
		Revision:       1,
		Kind:           "provision",
		Risk:           coreexecution.RiskR2,
		Gate:           coreexecution.GateResourcePurchase,
		Effects:        []coreexecution.Gate{coreexecution.GateResourcePurchase},
		TargetID:       target.ID,
		TargetRevision: target.Revision,
		TargetDigest:   target.Digest,
		Steps:          []coreexecution.ExecutionStep{step},
		TimeoutSeconds: 1800,
	}
	analysis := coreexecution.ProjectAnalysis{AnalysisID: "33333333-3333-4333-8333-333333333333", ProjectID: "44444444-4444-4444-8444-444444444444", Source: coreexecution.SourceRef{Kind: "git_https", Location: "https://example.org/ec2-stage-fence", Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true}, Revision: 1, CreatedAt: now, UpdatedAt: now}
	quote := reservation.CostQuote
	placement := coreexecution.PlacementOption{Region: target.Region, Spec: reservation.InstanceType, Disk: "20GiB", Network: "private", CostQuote: quote}
	plan := coreexecution.ExecutionPlan{SchemaVersion: coreexecution.SchemaVersion, ID: "55555555-5555-4555-8555-555555555555", Revision: 1, OwnerID: owner, ProjectID: analysis.ProjectID, AnalysisID: analysis.AnalysisID, Purpose: coreexecution.PurposeJob, Placement: coreexecution.PlacementRecommendation{Kind: "new_ephemeral_target", Minimum: placement, Recommended: placement, HighPerformance: placement}, Targets: []coreexecution.ExecutionTarget{target}, Stages: []coreexecution.ExecutionStage{stage}, Status: coreexecution.PlanReady, CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
	plan, err = plan.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	createdPlan, err := executionStore.CreatePlan(ctx, ExecutionPlanCreate{OwnerID: owner, Analysis: analysis, Plan: plan, IdempotencyID: "66666666-6666-4666-8666-666666666666"})
	if err != nil {
		t.Fatal(err)
	}
	coordinator := NewDatabaseExecutionCoordinator(database.DB(), func() time.Time { return now })
	materialized, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: owner, PlanID: createdPlan.ID, PlanRevision: createdPlan.Revision, Operation: coreexecution.RunOperationExecute, IdempotencyKey: "77777777-7777-4777-8777-777777777777"})
	if err != nil || len(materialized.Tasks) != 1 || len(materialized.Confirmations) != 1 {
		t.Fatalf("materialized=%#v err=%v", materialized, err)
	}
	payload := materialized.Tasks[0].Spec.Payload.ExecutionStage
	confirmation := materialized.Confirmations[0]
	if payload == nil || payload.RunRevision != uint64(confirmation.Binding.RunRevision) || payload.RunRevision != materialized.Stages[0].RunRevision {
		t.Fatalf("initial pins task=%#v confirmation=%#v stage=%#v", payload, confirmation.Binding, materialized.Stages[0])
	}
	if _, err = coordinator.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{OwnerID: owner, ConfirmationID: confirmation.ID, ExpectedRevision: confirmation.Revision, IdempotencyKey: "88888888-8888-4888-8888-888888888888"}); err != nil {
		t.Fatal(err)
	}
	claim, err := executionStore.ClaimQueuedExecutionStage(ctx, owner, "ec2-stage-fence-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	next, err := executionStore.NextExecutableStep(ctx, claim.OwnerID, claim.RunID, claim.StageID)
	if err != nil {
		t.Fatal(err)
	}
	lease := executionrunner.StageLease{OwnerID: claim.OwnerID, RunID: claim.RunID, StageID: claim.StageID, TaskID: claim.TaskID, Holder: claim.Holder, Attempt: claim.Attempt, LeaseEpoch: claim.LeaseEpoch, TaskLeaseEpoch: claim.TaskLeaseEpoch, ExpectedTaskRevision: claim.ExpectedTaskRevision, LeaseID: claim.LeaseID, LeaseToken: claim.LeaseToken, ExpiresAt: claim.ExpiresAt}
	nextStep := executionrunner.NextStep{OwnerID: next.OwnerID, RunID: next.RunID, StageID: next.StageID, StepKey: next.StepKey, StepSet: next.StepSet, StepRevision: next.StepRevision, StepDigest: next.StepDigest}
	prepared, err := NewExecutionStepResolver(executionStore, reconcileCanceledCredentialResolver{owner: owner, credential: credential}, nil).ResolveStep(ctx, lease, nextStep)
	if err != nil || prepared.EC2Provision == nil {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	request := prepared.EC2Provision
	view, err := executionStore.GetExecutionRun(ctx, owner, claim.RunID)
	if err != nil {
		t.Fatal(err)
	}
	var stageView coreexecution.RunStage
	for _, candidate := range view.Stages {
		if candidate.StageID == claim.StageID {
			stageView = candidate
			break
		}
	}
	if stageView.RunRevision == 0 || request.RunRevision != stageView.RunRevision || request.RunRevision != uint64(confirmation.Binding.RunRevision) || request.RunRevision != payload.RunRevision || view.Run.Revision <= request.RunRevision {
		t.Fatalf("run revision drift: request=%d stage=%d confirmation=%d task=%d head=%d", request.RunRevision, stageView.RunRevision, confirmation.Binding.RunRevision, payload.RunRevision, view.Run.Revision)
	}
	stepDigest := next.StepDigest
	wantFence, err := coreexecution.CanonicalDigest(struct {
		OwnerID, PlanID, RunID, StageID, StepKey, AttemptID          string
		PlanRevision, RunRevision, StageRevision, StepRevision       uint64
		PlanDigest, RunDigest, StageDigest, StepDigest, TargetDigest coreexecution.Digest
		LeaseID, LeaseToken                                          string
		LeaseEpoch                                                   uint64
	}{owner, createdPlan.ID, view.Run.RunID, stageView.StageID, next.StepKey, request.AttemptID, createdPlan.Revision, stageView.RunRevision, stageView.StageRevision, next.StepRevision, createdPlan.Digest, view.Run.RunDigest, stageView.StageDigest, stepDigest, target.Digest, claim.LeaseID, claim.LeaseToken, claim.LeaseEpoch})
	if err != nil || request.FenceDigest != wantFence {
		t.Fatalf("fence digest does not pin stage run revision: got=%s want=%s err=%v", request.FenceDigest, wantFence, err)
	}
}
