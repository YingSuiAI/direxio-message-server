package executionplanning

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentrecipes"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionrunner"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
	"github.com/google/uuid"
)

const serviceLoopInstanceID = "i-0123456789abcdef0"

type serviceLoopSource struct {
	ports map[string]int
}

func (s serviceLoopSource) ResolveSource(_ context.Context, _ string, _ string, in SourceInput) (SourceFacts, error) {
	port, ok := s.ports[in.Location]
	if !ok || in.Kind != "oci_image" || !in.Immutable {
		return SourceFacts{}, ErrSourceInvalid
	}
	return SourceFacts{Analysis: coreexecution.ProjectAnalysis{
		Source:         coreexecution.SourceRef{Kind: in.Kind, Location: in.Location, Immutable: true},
		DetectedStacks: []string{"oci_image"},
		Runtime:        coreexecution.ResourceRequirement{CPU: "1", Memory: "256MiB", Disk: "1GiB", Architecture: "x86_64"},
		Ports:          []int{port},
		Probes:         []string{fmt.Sprintf("http://127.0.0.1:%d/health", port)},
		Exposure:       "target_local",
	}}, nil
}

type serviceLoopCredentials struct {
	owner      string
	credential coreaws.Credentials
}

func (r serviceLoopCredentials) ResolveCredentialRevision(_ context.Context, owner, id string, revision uint64) (coreaws.Credentials, error) {
	if owner != r.owner || id != r.credential.ID || revision != uint64(r.credential.Revision) {
		return coreaws.Credentials{}, coreaws.ErrNotFound
	}
	return r.credential, nil
}

type serviceLoopTransport struct {
	loseRunID string
	dispatch  map[string]int
	reconcile map[string]int
}

func (t *serviceLoopTransport) Dispatch(_ context.Context, in coreaws.FrozenRequest) (coreaws.DispatchResult, error) {
	if t.dispatch == nil {
		t.dispatch = make(map[string]int)
	}
	t.dispatch[in.RunID]++
	return coreaws.DispatchResult{
		Status:        coreaws.DispatchAccepted,
		CommandID:     "cmd-" + in.AttemptID,
		RequestDigest: in.RequestDigest,
		TargetID:      in.TargetID,
		InstanceID:    in.InstanceID,
	}, nil
}

func (t *serviceLoopTransport) Poll(_ context.Context, in coreaws.PollRequest) (coreaws.CommandResult, error) {
	if in.Frozen.RunID == t.loseRunID {
		return coreaws.CommandResult{Status: coreaws.PollUncertain, CommandID: in.CommandID, InstanceID: in.Frozen.InstanceID}, errors.New("simulated SSM response loss")
	}
	digest, err := coreexecution.CanonicalDigest(struct {
		CommandID string
		Fence     coreexecution.Digest
	}{in.CommandID, in.FenceDigest})
	if err != nil {
		return coreaws.CommandResult{}, err
	}
	return coreaws.CommandResult{
		Status:       coreaws.PollSucceeded,
		CommandID:    in.CommandID,
		InstanceID:   in.Frozen.InstanceID,
		OutputDigest: digest,
	}, nil
}

func (t *serviceLoopTransport) ReconcileCommand(_ context.Context, in coreaws.PollRequest) (coreaws.ReconcileResult, error) {
	if t.reconcile == nil {
		t.reconcile = make(map[string]int)
	}
	t.reconcile[in.Frozen.RunID]++
	digest, err := coreexecution.CanonicalDigest(struct {
		CommandID string
		Fence     coreexecution.Digest
	}{in.CommandID, in.FenceDigest})
	return coreaws.ReconcileResult{CommandResult: coreaws.CommandResult{Status: coreaws.PollSucceeded, CommandID: in.CommandID, InstanceID: in.Frozen.InstanceID, OutputDigest: digest}}, err
}

type serviceLoopCase struct {
	label     string
	projectID string
	image     string
	port      int
	analysis  coreexecution.ProjectAnalysis
	plan      coreexecution.ExecutionPlan
	run       storage.ExecutionRunView
	approvals []storage.ExecutionConfirmationRecord
}

func TestGenericContainerServiceFirstLoopPostgres(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	owner := "@execution-service-loop:example.org"

	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	databaseOptions := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	database, err := storage.NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, databaseOptions), &databaseOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	store := storage.NewDatabaseExecutionStore(database.DB(), func() time.Time { return now })
	coordinator := storage.NewDatabaseExecutionCoordinator(database.DB(), func() time.Time { return now })
	artifacts, err := artifactstore.New(t.TempDir(), artifactstore.MaxArtifactSize)
	if err != nil {
		t.Fatal(err)
	}

	credentialID := serviceLoopUUID("credential")
	credential := coreaws.RehydrateCredentials(
		credentialID, "service-loop", "us-east-1", "123456789012",
		"arn:aws:iam::123456789012:role/service-loop",
		[]byte("AKIATESTONLY"), []byte("fixture-secret-must-not-leak"), nil,
		1, 1, now, now,
	)
	credentialRef := coreexecution.CredentialRef{Ref: credentialID, Purpose: "aws", Revision: 1}
	credentialRef.BindingDigest, err = coreaws.CredentialBindingDigest(owner, credentialRef, credential)
	if err != nil {
		t.Fatal(err)
	}
	targetID := serviceLoopUUID("target")
	target, err := coreaws.NormalizeInfrastructureTarget(coreexecution.ExecutionTarget{
		ID: targetID, Provider: "aws", Kind: coreexecution.TargetKindAWSEC2Instance,
		InfrastructureProfileID: coreaws.InfrastructureProfileGeneralLinuxSSMV1,
		AccountID:               "123456789012", Region: "us-east-1", Architecture: "x86_64",
		Capabilities:   []string{"target.aws_ec2_instance", "target.instance." + serviceLoopInstanceID, "transport.aws_ssm"},
		CredentialRefs: []coreexecution.CredentialRef{credentialRef},
		Network:        coreexecution.NetworkPolicy{Mode: coreexecution.NetworkPolicyModeObservedHTTPSEgress},
		Revision:       1,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	target, err = store.CreateTarget(ctx, storage.TargetCreateRequest{
		OwnerID: owner, Target: target, IdempotencyID: serviceLoopUUID("target-create"),
	})
	if err != nil {
		t.Fatal(err)
	}
	securityGroupDigest, err := coreexecution.CanonicalDigest([]string{"sg-0123456789abcdef0"})
	if err != nil {
		t.Fatal(err)
	}
	observation, err := store.CreateTargetObservation(ctx, storage.TargetObservationCreateRequest{
		OwnerID: owner, ObservationID: serviceLoopUUID("target-observation"),
		Observation: coreexecution.TargetObservation{
			TargetID: target.ID, TargetRevision: target.Revision, ObservedAt: now, State: "ready",
			Facts: map[string]string{
				"instance_id": serviceLoopInstanceID, "instance_type": "t3.small",
				"account_id": target.AccountID, "region": target.Region, "partition": "aws",
				"operating_system": "linux", "architecture": target.Architecture,
				"ssm_status": "Online", "platform_name": "Amazon Linux", "platform_version": "2023",
				coreexecution.ObservationFactVCPUCount:           "2",
				coreexecution.ObservationFactMemoryMiB:           "2048",
				coreexecution.ObservationFactRootVolumeGiB:       "20",
				coreexecution.ObservationFactHTTPSEgress:         coreexecution.ObservationFactHTTPSEgressValue,
				coreexecution.ObservationFactSecurityGroupDigest: string(securityGroupDigest),
			},
		},
		IdempotencyID: serviceLoopUUID("target-observation-create"),
	})
	if err != nil || !observation.Observation.Digest.Valid() {
		t.Fatalf("observation=%+v err=%v", observation, err)
	}

	registry, err := agentrecipes.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	cases := []*serviceLoopCase{
		{
			label:     "GeoLibre acceptance fixture",
			projectID: serviceLoopUUID("project-geolibre-fixture"),
			image:     "registry.example/fixtures/geolibre@sha256:" + strings.Repeat("a", 64),
			port:      18080,
		},
		{
			label:     "unrelated echo service",
			projectID: serviceLoopUUID("project-unrelated-echo"),
			image:     "registry.example/services/echo@sha256:" + strings.Repeat("b", 64),
			port:      19090,
		},
	}
	sources := serviceLoopSource{ports: map[string]int{
		cases[0].image: cases[0].port,
		cases[1].image: cases[1].port,
	}}
	planner := New(Config{
		AnalysisStore: store, PlanStore: store, RevisionWriter: store,
		Sources: sources, Targets: NewDatabaseTargetResolver(store),
		Bindings:  NewProductionContainerBindingResolver(store, func() time.Time { return now }),
		Executors: NewArtifactExecutorSealer(artifacts), Recipes: registry,
		Now: func() time.Time { return now },
	})
	if !planner.PlanReady() {
		t.Fatal("production planner did not advertise readiness")
	}

	for _, service := range cases {
		service.analysis, service.plan = serviceLoopAnalyzeAndCompile(t, ctx, planner, owner, target, *service)
	}
	if cases[0].plan.ID == cases[1].plan.ID || cases[0].plan.DeploymentID == cases[1].plan.DeploymentID || cases[0].plan.Digest == cases[1].plan.Digest {
		t.Fatal("distinct projects collapsed to one plan/deployment identity")
	}
	assertServiceLoopPlanTopology(t, cases[0].plan, cases[1].plan)

	// A revision retains the stable deployment identity while changing every
	// immutable plan/stage/executor pin that belongs to the new revision.
	revised, err := planner.Revise(ctx, owner, agentembedded.ExecutionV2PlanReviseRequest{
		PlanID: cases[0].plan.ID, ExpectedRevision: cases[0].plan.Revision,
		Intent: "deploy", RecipeID: "generic-container-service",
		TargetID: target.ID, TargetRevision: target.Revision,
		Purpose: coreexecution.PurposeService, IdempotencyKey: serviceLoopUUID("plan-revise-geolibre-fixture"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if revised.Revision != 2 || revised.DeploymentID != cases[0].plan.DeploymentID || revised.Digest == cases[0].plan.Digest {
		t.Fatalf("revised plan identity=%+v original=%+v", revised, cases[0].plan)
	}
	cases[0].plan = revised

	credentials := serviceLoopCredentials{owner: owner, credential: credential}
	artifactResolver := storage.NewFilesystemArtifactResolver(store, artifacts)
	stepResolver := storage.NewExecutionStepResolver(store, credentials, artifactResolver)
	receiptResolver := storage.NewDatabaseDispatchReceiptResolver(store, credentials)
	outputs := storage.NewExecutionServiceOutputMaterializer(store, artifacts, owner)
	stageStore := storage.NewExecutionStageStoreAdapterWithOutputs(store, outputs)
	transport := &serviceLoopTransport{}
	runner, err := executionrunner.NewRunner(executionrunner.Config{
		Store: stageStore, Resolver: stepResolver, Transport: transport,
		Holder: "service-loop-runner", LeaseTTL: time.Minute, PollInterval: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, service := range cases {
		service.run, service.approvals = serviceLoopDriveRun(t, ctx, owner, service.plan, store, coordinator, runner)
		if got := transport.dispatch[service.run.Run.RunID]; got != len(service.plan.Stages) {
			t.Fatalf("%s dispatches=%d want=%d", service.label, got, len(service.plan.Stages))
		}
		serviceLoopAssertOutputs(t, ctx, owner, *service, database.DB(), store, artifacts)
	}

	// A second plan for the unrelated service proves that an SSM response loss
	// is terminally uncertain and cannot fall back or redispatch.
	lossRequest := agentembedded.ExecutionV2PlanCreateRequest{
		ProjectID: cases[1].projectID, AnalysisID: cases[1].analysis.AnalysisID,
		Intent: "deploy", RecipeID: "generic-container-service",
		TargetID: target.ID, TargetRevision: target.Revision,
		Purpose: coreexecution.PurposeService, IdempotencyKey: serviceLoopUUID("plan-unrelated-loss"),
	}
	lossPlan, err := planner.Compile(ctx, owner, lossRequest)
	if err != nil {
		t.Fatal(err)
	}
	lossMaterialized, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{
		OwnerID: owner, PlanID: lossPlan.ID, PlanRevision: lossPlan.Revision,
		Operation: coreexecution.RunOperationDeploy, IdempotencyKey: serviceLoopUUID("run-unrelated-loss"),
	})
	if err != nil || len(lossMaterialized.Confirmations) != 1 {
		t.Fatalf("loss run=%+v err=%v", lossMaterialized, err)
	}
	if _, err = coordinator.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{
		OwnerID: owner, ConfirmationID: lossMaterialized.Confirmations[0].ID,
		ExpectedRevision: lossMaterialized.Confirmations[0].Revision,
		IdempotencyKey:   serviceLoopUUID("confirm-loss-bootstrap"),
	}); err != nil {
		t.Fatal(err)
	}
	transport.loseRunID = lossMaterialized.Run.RunID
	if err = runner.RunOnce(ctx); !errors.Is(err, executionrunner.ErrRunnerUncertain) {
		t.Fatalf("response loss result=%v", err)
	}
	lost, err := store.GetExecutionRun(ctx, owner, lossMaterialized.Run.RunID)
	if err != nil || lost.Run.Status != coreexecution.RunUncertain || lost.Stages[0].Status != coreexecution.StageUncertain {
		t.Fatalf("uncertain run=%+v err=%v", lost, err)
	}
	if transport.dispatch[lost.Run.RunID] != 1 {
		t.Fatalf("response loss dispatches=%d want=1", transport.dispatch[lost.Run.RunID])
	}
	reconciler := storage.NewExecutionReconciler(store, receiptResolver, transport, nil, func() time.Time { return now })
	if reconciler == nil {
		t.Fatal("reconciler was not constructed")
	}
	reconciler.SetOutputHook(outputs)
	reconcileCommand := storage.ExecutionSSMReconcileCommand{OwnerID: owner, RunID: lost.Run.RunID, StageID: lost.Stages[0].StageID, ExpectedRevision: lost.Run.Revision, IdempotencyKey: serviceLoopUUID("reconcile-loss")}
	resolved, err := reconciler.Reconcile(ctx, reconcileCommand)
	if err != nil || resolved.Status != coreexecution.RunRunning || resolved.Revision <= lost.Run.Revision {
		t.Fatalf("reconciled run=%+v err=%v", resolved, err)
	}
	resolvedView, err := store.GetExecutionRun(ctx, owner, lost.Run.RunID)
	if err != nil || resolvedView.Run.Status != coreexecution.RunRunning || resolvedView.Stages[0].Status != coreexecution.StageSucceeded || resolvedView.Stages[1].Status != coreexecution.StageWaitingUser {
		t.Fatalf("reconciled DAG=%+v err=%v", resolvedView, err)
	}
	var receiptStatus, attemptStatus, intentStatus, leaseStatus string
	if err = database.DB().QueryRowContext(ctx, `SELECT receipt.status,attempt.status,intent.status,lease.status FROM core_execution_receipts receipt JOIN core_execution_step_attempts attempt ON attempt.owner_id=receipt.owner_id AND attempt.attempt_id=receipt.attempt_id JOIN core_execution_dispatch_intents intent ON intent.owner_id=receipt.owner_id AND intent.receipt_id=receipt.receipt_id JOIN core_execution_target_mutation_leases lease ON lease.owner_id=receipt.owner_id AND lease.receipt_id=receipt.receipt_id WHERE receipt.owner_id=$1 AND receipt.run_id=$2`, owner, lost.Run.RunID).Scan(&receiptStatus, &attemptStatus, &intentStatus, &leaseStatus); err != nil {
		t.Fatal(err)
	}
	if receiptStatus != "succeeded" || attemptStatus != "succeeded" || intentStatus != "succeeded" || leaseStatus != "released" {
		t.Fatalf("reconcile evidence receipt=%s attempt=%s intent=%s lease=%s", receiptStatus, attemptStatus, intentStatus, leaseStatus)
	}
	replayed, err := reconciler.Reconcile(ctx, reconcileCommand)
	if err != nil || replayed.RunID != resolved.RunID || replayed.Revision != resolved.Revision || transport.reconcile[lost.Run.RunID] != 1 {
		t.Fatalf("reconcile replay=%+v calls=%d err=%v", replayed, transport.reconcile[lost.Run.RunID], err)
	}
	if err = runner.RunOnce(ctx); !errors.Is(err, coreexecution.ErrNotFound) {
		t.Fatalf("unconfirmed downstream stage was claimable: %v", err)
	}
	if transport.dispatch[lost.Run.RunID] != 1 {
		t.Fatalf("uncertain run redispatched: %d", transport.dispatch[lost.Run.RunID])
	}
	bindings, _, err := store.ListServiceBindings(ctx, owner, cases[1].projectID, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range bindings {
		if binding.DeploymentID == lossPlan.DeploymentID {
			t.Fatal("uncertain run produced a ServiceBinding")
		}
	}
}

func serviceLoopAnalyzeAndCompile(t *testing.T, ctx context.Context, planner *Service, owner string, target coreexecution.ExecutionTarget, service serviceLoopCase) (coreexecution.ProjectAnalysis, coreexecution.ExecutionPlan) {
	t.Helper()
	analysis, err := planner.Analyze(ctx, owner, agentembedded.ExecutionV2AnalyzeRequest{
		ProjectID: service.projectID,
		Source: agentembedded.ExecutionV2SourceInput{
			Kind: "oci_image", Location: service.image, Immutable: true,
		},
		IdempotencyKey: serviceLoopUUID("analysis-" + service.projectID),
	})
	if err != nil {
		t.Fatalf("%s analyze: %v", service.label, err)
	}
	request := agentembedded.ExecutionV2PlanCreateRequest{
		ProjectID: service.projectID, AnalysisID: analysis.AnalysisID,
		Intent: "deploy", RecipeID: "generic-container-service",
		TargetID: target.ID, TargetRevision: target.Revision,
		Purpose: coreexecution.PurposeService, IdempotencyKey: serviceLoopUUID("plan-" + service.projectID),
	}
	plan, err := planner.Compile(ctx, owner, request)
	if err != nil {
		t.Fatalf("%s compile: %v", service.label, err)
	}
	replay, err := planner.Compile(ctx, owner, request)
	if err != nil || replay.ID != plan.ID || replay.DeploymentID != plan.DeploymentID || replay.Digest != plan.Digest {
		t.Fatalf("%s compile replay=%+v plan=%+v err=%v", service.label, replay, plan, err)
	}
	if plan.Purpose != coreexecution.PurposeService || !coreexecution.ValidateUUID(plan.DeploymentID) || len(plan.Recipes) != 1 || plan.Recipes[0].ID != "generic-container-service" || len(plan.Stages) != 3 {
		t.Fatalf("%s plan is not the generic service graph: %+v", service.label, plan)
	}
	return analysis, plan
}

func serviceLoopDriveRun(t *testing.T, ctx context.Context, owner string, plan coreexecution.ExecutionPlan, store *storage.DatabaseExecutionStore, coordinator *storage.DatabaseExecutionCoordinator, runner *executionrunner.Runner) (storage.ExecutionRunView, []storage.ExecutionConfirmationRecord) {
	t.Helper()
	materialized, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{
		OwnerID: owner, PlanID: plan.ID, PlanRevision: plan.Revision,
		Operation: coreexecution.RunOperationDeploy, IdempotencyKey: serviceLoopUUID("run-" + plan.ID),
	})
	if err != nil {
		t.Fatal(err)
	}
	replay, err := coordinator.CreateRun(ctx, coreexecution.CreateRunCommand{
		OwnerID: owner, PlanID: plan.ID, PlanRevision: plan.Revision,
		Operation: coreexecution.RunOperationDeploy, IdempotencyKey: serviceLoopUUID("run-" + plan.ID),
	})
	if err != nil || replay.Run.RunID != materialized.Run.RunID {
		t.Fatalf("run replay=%+v err=%v", replay, err)
	}

	var approvals []storage.ExecutionConfirmationRecord
	for iteration := 0; iteration < 20; iteration++ {
		view, err := store.GetExecutionRun(ctx, owner, materialized.Run.RunID)
		if err != nil {
			t.Fatalf("read run: %v", err)
		}
		if view.Run.Status == coreexecution.RunSucceeded {
			if len(approvals) != 2 {
				t.Fatalf("run %s approvals=%d want=2", view.Run.RunID, len(approvals))
			}
			return view, approvals
		}
		if view.Run.Status == coreexecution.RunFailed || view.Run.Status == coreexecution.RunUncertain || view.Run.Status == coreexecution.RunCanceled || view.Run.Status == coreexecution.RunRejected || view.Run.Status == coreexecution.RunExpired {
			t.Fatalf("run reached unexpected terminal state: %+v", view.Run)
		}
		pending, err := store.ListV2Confirmations(ctx, owner, "", []coreconfirmation.State{coreconfirmation.StatePending}, 100)
		if err != nil {
			t.Fatalf("list confirmations: %v", err)
		}
		confirmed := false
		for _, record := range pending.Items {
			if record.Confirmation.Binding.RunID != view.Run.RunID {
				continue
			}
			serviceLoopAssertConfirmation(t, plan, view, record)
			if _, err = coordinator.ConfirmStage(ctx, coreexecution.ConfirmStageCommand{
				OwnerID: owner, ConfirmationID: record.Confirmation.ID,
				ExpectedRevision: record.Confirmation.Revision,
				IdempotencyKey:   serviceLoopUUID("confirm-" + record.Confirmation.ID),
			}); err != nil {
				t.Fatalf("confirm stage: %v", err)
			}
			approvals = append(approvals, record)
			confirmed = true
			break
		}
		if confirmed {
			continue
		}
		if err = runner.RunOnce(ctx); err != nil {
			t.Fatalf("run %s worker iteration %d: %v", view.Run.RunID, iteration, err)
		}
	}
	t.Fatalf("run %s did not reach a terminal state", materialized.Run.RunID)
	return storage.ExecutionRunView{}, nil
}

func serviceLoopAssertConfirmation(t *testing.T, plan coreexecution.ExecutionPlan, view storage.ExecutionRunView, record storage.ExecutionConfirmationRecord) {
	t.Helper()
	binding, preview := record.Confirmation.Binding, record.Preview
	if binding.PlanID != plan.ID || uint64(binding.PlanRevision) != plan.Revision || string(binding.PlanDigest) != string(plan.Digest) || binding.DeploymentID != plan.DeploymentID || binding.RunID != view.Run.RunID || binding.RunRevision != int64(preview.RunRevision) || binding.StageID != preview.StageID || string(binding.StageDigest) != string(preview.StageDigest) || binding.TargetID != preview.TargetID || string(binding.TargetDigest) != string(preview.TargetDigest) || binding.RiskLevel != string(preview.Risk) || binding.GateType != string(preview.Gate) || binding.Digest == "" {
		t.Fatalf("confirmation binding/preview drift: binding=%+v preview=%+v", binding, preview)
	}
	matched := false
	for _, stage := range plan.Stages {
		if stage.StageKey == preview.StageKey {
			matched = stage.Digest == preview.StageDigest && stage.Risk == preview.Risk && stage.Gate == preview.Gate
			break
		}
	}
	if !matched {
		t.Fatalf("confirmation does not pin a plan stage: %+v", preview)
	}
	raw, err := json.Marshal(preview)
	if err != nil {
		t.Fatal(err)
	}
	lower := strings.ToLower(string(raw))
	for _, projectSpecific := range []string{"geolibre", "echo service", "fixture-secret"} {
		if strings.Contains(lower, projectSpecific) {
			t.Fatalf("generic confirmation exposed project-specific data %q: %s", projectSpecific, raw)
		}
	}
}

func assertServiceLoopPlanTopology(t *testing.T, first, second coreexecution.ExecutionPlan) {
	t.Helper()
	if len(first.Recipes) != 1 || len(second.Recipes) != 1 || first.Recipes[0] != second.Recipes[0] || len(first.Stages) != len(second.Stages) {
		t.Fatalf("generic recipe/topology mismatch: first=%+v second=%+v", first.Recipes, second.Recipes)
	}
	for index := range first.Stages {
		a, b := first.Stages[index], second.Stages[index]
		if a.StageKey != b.StageKey || a.Kind != b.Kind || a.Risk != b.Risk || a.Gate != b.Gate || len(a.Steps) != len(b.Steps) || !a.Digest.Valid() || !b.Digest.Valid() {
			t.Fatalf("stage topology mismatch: first=%+v second=%+v", a, b)
		}
		for stepIndex := range a.Steps {
			if a.Steps[stepIndex].StepKey != b.Steps[stepIndex].StepKey || a.Steps[stepIndex].Kind != b.Steps[stepIndex].Kind || a.Steps[stepIndex].Executor == nil || b.Steps[stepIndex].Executor == nil || !a.Steps[stepIndex].Digest.Valid() || !b.Steps[stepIndex].Digest.Valid() {
				t.Fatalf("step topology mismatch: first=%+v second=%+v", a.Steps[stepIndex], b.Steps[stepIndex])
			}
		}
	}
}

func serviceLoopAssertOutputs(t *testing.T, ctx context.Context, owner string, service serviceLoopCase, db *sql.DB, store *storage.DatabaseExecutionStore, artifacts *artifactstore.Store) {
	t.Helper()
	expectedIntents := 0
	for _, stage := range service.plan.Stages {
		expectedIntents += len(stage.Steps)
	}
	expectedLeaseEpochs := len(service.plan.Stages)
	var intentCount, leaseIDs, leaseEpochs int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*),COUNT(DISTINCT lease_id),COUNT(DISTINCT lease_epoch) FROM core_execution_dispatch_intents WHERE owner_id=$1 AND run_id=$2`, owner, service.run.Run.RunID).Scan(&intentCount, &leaseIDs, &leaseEpochs); err != nil {
		t.Fatal(err)
	}
	if intentCount != expectedIntents || leaseIDs != 1 || leaseEpochs != expectedLeaseEpochs {
		t.Fatalf("%s durable target lease history intents=%d lease_ids=%d epochs=%d want=%d/1/%d", service.label, intentCount, leaseIDs, leaseEpochs, expectedIntents, expectedLeaseEpochs)
	}
	var leaseStatus string
	if err := db.QueryRowContext(ctx, `SELECT status FROM core_execution_target_mutation_leases WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3`, owner, service.plan.Stages[0].TargetID, service.plan.Stages[0].TargetRevision).Scan(&leaseStatus); err != nil {
		t.Fatal(err)
	}
	if leaseStatus != "released" {
		t.Fatalf("%s target mutation lease status=%q want=released", service.label, leaseStatus)
	}
	bindings, _, err := store.ListServiceBindings(ctx, owner, service.projectID, "", 10)
	if err != nil || len(bindings) != 1 {
		t.Fatalf("%s bindings=%+v err=%v", service.label, bindings, err)
	}
	binding := bindings[0]
	if binding.DeploymentID != service.plan.DeploymentID || binding.RunID != service.run.Run.RunID || binding.Protocol != "ssm" || binding.Endpoint != "ssm://"+serviceLoopInstanceID || len(binding.OperationSchemas) != 0 || len(binding.AuthRefs) != 0 || len(binding.ArtifactIDs) != 2 || binding.UsageArtifact.ID == "" || !binding.Digest.Valid() {
		t.Fatalf("%s binding=%+v", service.label, binding)
	}
	var documents strings.Builder
	for _, artifactID := range binding.ArtifactIDs {
		metadata, err := store.GetArtifactMetadata(ctx, owner, artifactID)
		if err != nil || metadata.ProjectID != service.projectID || metadata.PlanID != service.plan.ID || metadata.PlanRevision != service.plan.Revision || metadata.RunID != service.run.Run.RunID || metadata.MediaType != "text/markdown" || !metadata.ContentDigest.Valid() {
			t.Fatalf("%s artifact=%+v err=%v", service.label, metadata, err)
		}
		reader, opened, err := artifacts.Open(ctx, string(metadata.ContentDigest))
		if err != nil {
			t.Fatal(err)
		}
		body, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if readErr != nil || opened.Size != metadata.SizeBytes || int64(len(body)) != metadata.SizeBytes {
			t.Fatalf("%s artifact read size=%d/%d err=%v", service.label, len(body), metadata.SizeBytes, readErr)
		}
		documents.Write(body)
	}
	text := strings.ToLower(documents.String())
	for _, required := range []string{"# service usage", "# service runbook", "ssm://" + serviceLoopInstanceID, "new immutable plan revision"} {
		if !strings.Contains(text, required) {
			t.Fatalf("%s generated documents lack %q: %s", service.label, required, documents.String())
		}
	}
	for _, forbidden := range []string{"geolibre", "echo service", "fixture-secret-must-not-leak", "aws_secret_access_key", "#!/bin/"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("%s generated documents leaked %q: %s", service.label, forbidden, documents.String())
		}
	}
}

func serviceLoopUUID(name string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("dirextalk-execution-service-loop\x00"+name)).String()
}
