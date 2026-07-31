package executionplanning

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentrecipes"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executiontarget"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
)

const handoffOwner = "@provision-handoff:example.org"

type handoffCatalog struct {
	analysis       coreexecution.ProjectAnalysis
	targets        map[uint64]coreexecution.ExecutionTarget
	observation    storage.TargetObservationRecord
	observationKey string
	plans          []coreexecution.ExecutionPlan
}

func (s *handoffCatalog) CreateAnalysis(_ context.Context, in storage.AnalysisCreateRequest) (coreexecution.ProjectAnalysis, error) {
	normalized, err := in.Analysis.Normalize()
	if err == nil {
		s.analysis = normalized
	}
	return normalized, err
}
func (s *handoffCatalog) GetAnalysis(_ context.Context, owner, id string) (coreexecution.ProjectAnalysis, error) {
	if owner != handoffOwner || id != s.analysis.AnalysisID {
		return coreexecution.ProjectAnalysis{}, coreexecution.ErrNotFound
	}
	return s.analysis, nil
}
func (s *handoffCatalog) CreatePlan(_ context.Context, in storage.ExecutionPlanCreate) (coreexecution.ExecutionPlan, error) {
	if in.OwnerID != handoffOwner || in.Analysis.Digest != s.analysis.Digest {
		return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
	}
	plan, err := in.Plan.NormalizeAt(in.Plan.CreatedAt)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	s.plans = append(s.plans, plan)
	return plan, nil
}
func (s *handoffCatalog) GetCurrentPlan(_ context.Context, owner, id string) (coreexecution.ExecutionPlan, error) {
	for i := len(s.plans) - 1; i >= 0; i-- {
		if owner == handoffOwner && s.plans[i].ID == id {
			return s.plans[i], nil
		}
	}
	return coreexecution.ExecutionPlan{}, coreexecution.ErrNotFound
}
func (s *handoffCatalog) RevisePlan(context.Context, string, coreexecution.ExecutionPlan, uint64, string) (coreexecution.ExecutionPlan, error) {
	return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
}
func (s *handoffCatalog) CreateTarget(_ context.Context, in storage.TargetCreateRequest) (coreexecution.ExecutionTarget, error) {
	if in.OwnerID != handoffOwner {
		return coreexecution.ExecutionTarget{}, coreexecution.ErrInvalid
	}
	target, err := in.Target.Normalize()
	if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	if prior, ok := s.targets[target.Revision]; ok {
		if prior.Digest != target.Digest {
			return coreexecution.ExecutionTarget{}, coreexecution.ErrConflict
		}
		return prior, nil
	}
	s.targets[target.Revision] = target
	return target, nil
}
func (s *handoffCatalog) GetTarget(_ context.Context, owner, id string, revision uint64) (coreexecution.ExecutionTarget, error) {
	target, ok := s.targets[revision]
	if owner != handoffOwner || !ok || target.ID != id {
		return coreexecution.ExecutionTarget{}, coreexecution.ErrNotFound
	}
	return target, nil
}
func (s *handoffCatalog) CreateTargetObservation(_ context.Context, in storage.TargetObservationCreateRequest) (storage.TargetObservationRecord, error) {
	if in.OwnerID != handoffOwner {
		return storage.TargetObservationRecord{}, coreexecution.ErrInvalid
	}
	observation, err := in.Observation.Normalize()
	if err != nil {
		return storage.TargetObservationRecord{}, err
	}
	s.observationKey = in.IdempotencyID
	s.observation = storage.TargetObservationRecord{OwnerID: in.OwnerID, ObservationID: in.ObservationID, Revision: 1, Status: "observed", Observation: observation}
	return s.observation, nil
}
func (s *handoffCatalog) GetTargetObservationByIdempotency(_ context.Context, owner, key string) (storage.TargetObservationRecord, bool, error) {
	if owner == handoffOwner && key != "" && key == s.observationKey {
		return s.observation, true, nil
	}
	return storage.TargetObservationRecord{}, false, nil
}
func (s *handoffCatalog) GetLatestReadyTargetObservation(_ context.Context, owner, id string, revision uint64) (storage.TargetObservationRecord, error) {
	if owner != handoffOwner || s.observation.Observation.TargetID != id || s.observation.Observation.TargetRevision != revision {
		return storage.TargetObservationRecord{}, coreexecution.ErrNotFound
	}
	return s.observation, nil
}

type handoffTargetResolver struct{ store *handoffCatalog }

func (r handoffTargetResolver) ResolveTarget(ctx context.Context, owner, id string, revision uint64) (coreexecution.ExecutionTarget, error) {
	return r.store.GetTarget(ctx, owner, id, revision)
}

type handoffCredentialStore struct{ credential coreaws.Credentials }

func (s handoffCredentialStore) GetCredentialRevision(_ context.Context, owner, id string, revision int64) (coreaws.Credentials, error) {
	if owner != handoffOwner || id != s.credential.ID || revision != s.credential.Revision {
		return coreaws.Credentials{}, coreaws.ErrNotFound
	}
	return s.credential, nil
}

type handoffReservationCatalog struct {
	now time.Time
}

func (c handoffReservationCatalog) ResolveReservation(_ context.Context, _ coreaws.Credentials, instanceType string, volumeGiB uint32) (executiontarget.ReservationOffer, error) {
	return executiontarget.ReservationOffer{
		InfrastructureProfileID: coreaws.InfrastructureProfileGeneralLinuxSSMV1,
		AMIParameter:            coreexecution.AWSAL2023X8664AMIParameter, InstanceType: instanceType, AvailabilityZone: "us-east-1a", VolumeGiB: volumeGiB,
		Architecture: "x86_64", ManagementTransport: "aws_ssm", PublicIP: true,
		CostQuote: coreexecution.CostQuote{Amount: "0.0274", Currency: "USD", ExpiresAt: c.now.Add(time.Hour)},
	}, nil
}

type handoffProvisionStore struct {
	catalog *handoffCatalog
	record  coreaws.EC2ProvisionIntentRecord
}

func (s *handoffProvisionStore) ReserveEC2ProvisionIntent(_ context.Context, intent coreaws.EC2ProvisionIntent) (coreaws.EC2ProvisionIntentRecord, bool, error) {
	if s.record.Intent.FenceDigest != "" {
		return s.record, false, nil
	}
	s.record = coreaws.EC2ProvisionIntentRecord{Intent: intent, Status: "accepted", Revision: 1}
	return s.record, true, nil
}
func (s *handoffProvisionStore) GetEC2ProvisionIntent(_ context.Context, owner string, fence coreexecution.Digest) (coreaws.EC2ProvisionIntentRecord, error) {
	if owner != handoffOwner || fence != s.record.Intent.FenceDigest {
		return coreaws.EC2ProvisionIntentRecord{}, coreexecution.ErrNotFound
	}
	return s.record, nil
}
func (s *handoffProvisionStore) RecordEC2ProviderOperation(_ context.Context, owner string, fence coreexecution.Digest, operation string) (coreaws.EC2ProvisionIntentRecord, error) {
	if owner != handoffOwner || fence != s.record.Intent.FenceDigest || operation == "" {
		return coreaws.EC2ProvisionIntentRecord{}, coreexecution.ErrConflict
	}
	s.record.ProviderOperationID, s.record.Revision = operation, s.record.Revision+1
	return s.record, nil
}
func (s *handoffProvisionStore) RecordEC2ProvisionReadback(_ context.Context, owner string, fence coreexecution.Digest, _ coreaws.CloudFormationProvisionReadback) (coreaws.EC2ProvisionIntentRecord, error) {
	if owner != handoffOwner || fence != s.record.Intent.FenceDigest {
		return coreaws.EC2ProvisionIntentRecord{}, coreexecution.ErrConflict
	}
	s.record.Status, s.record.Revision = "pending", s.record.Revision+1
	return s.record, nil
}
func (*handoffProvisionStore) MarkEC2ProvisionUncertain(context.Context, string, coreexecution.Digest) error {
	return nil
}
func (*handoffProvisionStore) MarkEC2ProvisionFailed(context.Context, string, coreexecution.Digest, coreaws.CloudFormationProvisionReadback) error {
	return errors.New("unexpected provision failure")
}
func (s *handoffProvisionStore) CompleteEC2Provision(ctx context.Context, completion coreaws.EC2ProvisionCompletion) error {
	if completion.Target.Revision != 2 || completion.Observation.TargetRevision != 2 {
		return coreexecution.ErrConflict
	}
	if _, err := s.catalog.CreateTarget(ctx, storage.TargetCreateRequest{OwnerID: handoffOwner, Target: completion.Target, ExpectedRevision: 1, IdempotencyID: uuid.NewString()}); err != nil {
		return err
	}
	_, err := s.catalog.CreateTargetObservation(ctx, storage.TargetObservationCreateRequest{
		OwnerID: handoffOwner, ObservationID: uuid.NewString(), Observation: completion.Observation, IdempotencyID: uuid.NewString(),
	})
	return err
}

type handoffProvisionProvider struct {
	readback coreaws.CloudFormationProvisionReadback
}

func (*handoffProvisionProvider) Create(context.Context, coreaws.CloudFormationCreateRequest) (string, error) {
	return "provider-operation", nil
}
func (p *handoffProvisionProvider) Readback(_ context.Context, in coreaws.CloudFormationCreateRequest, _ string) (coreaws.CloudFormationProvisionReadback, error) {
	out := p.readback
	out.StackName = in.StackName
	return out, nil
}

func TestProvisionReservationPlanCompletionHandsOffRevisionTwoToContainerPlan(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2036, 3, 4, 5, 6, 7, 0, time.UTC)
	projectID, analysisID := uuid.NewString(), uuid.NewString()
	credentialID := uuid.NewString()
	credential := coreaws.RehydrateCredentials(credentialID, "handoff", "us-east-1", "123456789012", "arn:aws:iam::123456789012:role/handoff", []byte("AKIAABCDEFGHIJKLMNOP"), []byte("fixture-secret"), nil, 3, 3, now, now)
	analysis, err := (coreexecution.ProjectAnalysis{
		AnalysisID: analysisID, ProjectID: projectID, Revision: 1, CreatedAt: now, UpdatedAt: now,
		Source:         coreexecution.SourceRef{Kind: "oci_image", Location: "registry.example/services/echo@sha256:" + strings.Repeat("a", 64), Immutable: true},
		DetectedStacks: []string{"oci_image"}, Runtime: coreexecution.ResourceRequirement{CPU: "1", Memory: "256MiB", Disk: "1GiB", Architecture: "x86_64"},
		Ports: []int{8080}, Probes: []string{"http://127.0.0.1:8080/health"}, Exposure: "target_local",
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	catalog := &handoffCatalog{analysis: analysis, targets: map[uint64]coreexecution.ExecutionTarget{}}
	targets := executiontarget.New(executiontarget.Config{
		Targets: catalog, Credentials: handoffCredentialStore{credential: credential}, Reservations: handoffReservationCatalog{now: now}, Now: func() time.Time { return now },
	})
	reservation, err := targets.Reserve(ctx, handoffOwner, executiontarget.ReserveRequest{
		CredentialID: credentialID, CredentialRevision: 3, InstanceType: "t3.small", VolumeGiB: 20, IdempotencyKey: uuid.NewString(),
	})
	if err != nil || reservation.Revision != 1 || reservation.ComputeReservation == nil {
		t.Fatalf("reservation=%+v err=%v", reservation, err)
	}
	recipes, err := agentrecipes.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	artifacts, err := artifactstore.New(t.TempDir(), artifactstore.MaxArtifactSize)
	if err != nil {
		t.Fatal(err)
	}
	bindings := &ProductionBindingResolver{
		provision: &ProductionProvisionBindingResolver{store: catalog, now: func() time.Time { return now }},
		container: &ProductionContainerBindingResolver{store: catalog, now: func() time.Time { return now }},
	}
	planner := New(Config{
		AnalysisStore: catalog, PlanStore: catalog, RevisionWriter: catalog, Sources: sourceStub{},
		Targets: handoffTargetResolver{store: catalog}, Bindings: bindings,
		Executors: NewArtifactExecutorSealer(artifacts), Recipes: recipes, Now: func() time.Time { return now },
	})
	port := agentembedded.NewExecutionV2ActionPort(agentembedded.ExecutionV2Config{
		Ready: func() bool { return true }, PlanReady: planner.PlanReady, PlanCompiler: planner,
	})
	provisionResult, actionErr := port.Handle(ctx, handoffOwner, "agent.execution.v2.plans.create", map[string]any{
		"project_id": projectID, "analysis_id": analysisID, "intent": "provision", "recipe_id": "aws-ec2-provision",
		"target_id": reservation.ID, "target_revision": 1, "purpose": "service", "idempotency_key": uuid.NewString(),
	})
	if actionErr != nil || provisionResult == nil || len(catalog.plans) != 1 {
		t.Fatalf("provision plan result=%+v err=%+v", provisionResult, actionErr)
	}
	provisionPlan := catalog.plans[0]
	if len(provisionPlan.Stages) != 1 || len(provisionPlan.Stages[0].Steps) != 1 {
		t.Fatalf("provision topology=%+v", provisionPlan.Stages)
	}
	step := provisionPlan.Stages[0].Steps[0]
	selected := reservation.ComputeReservation
	if step.ComputeProvision == nil || step.ComputeProvision.InstanceType != selected.InstanceType || step.ComputeProvision.AvailabilityZone != selected.AvailabilityZone || step.ComputeProvision.VolumeGiB != selected.VolumeGiB ||
		step.ComputeProvision.Region != reservation.Region || step.ComputeProvision.AMIParameter != selected.AMIParameter || !step.ComputeProvision.PublicIP || step.ComputeProvision.PublicInbound ||
		provisionPlan.Placement.Recommended.CostQuote != selected.CostQuote {
		t.Fatalf("server provision binding=%+v placement=%+v", step, provisionPlan.Placement)
	}
	policyDigest, _ := coreexecution.CanonicalDigest(struct {
		Risk coreexecution.Risk
		Gate coreexecution.Gate
	}{provisionPlan.Stages[0].Risk, provisionPlan.Stages[0].Gate})
	costDigest, _ := coreexecution.CanonicalDigest(provisionPlan.Placement.Recommended.CostQuote)
	provisionStore := &handoffProvisionStore{catalog: catalog}
	provider := &handoffProvisionProvider{readback: coreaws.CloudFormationProvisionReadback{
		Status: "CREATE_COMPLETE", InstanceID: "i-0123456789abcdef0", InstanceType: selected.InstanceType, AvailabilityZone: selected.AvailabilityZone,
		Architecture: "x86_64", OperatingSystem: "linux", SSMStatus: "Online", PlatformName: "Amazon Linux", PlatformVersion: "2023",
		VCPUCount: 2, MemoryMiB: 2048, RootVolumeGiB: int32(selected.VolumeGiB), PublicIP: "203.0.113.10",
		HTTPSEgress:         coreexecution.ObservationFactHTTPSEgressValue,
		SecurityGroupDigest: coreexecution.Digest(strings.Repeat("c", 64)),
	}}
	executor, err := coreaws.NewEC2ProvisionExecutor(provisionStore, provider, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	req := coreaws.EC2ProvisionRequest{
		OwnerID: handoffOwner, PlanID: provisionPlan.ID, PlanRevision: provisionPlan.Revision, PlanDigest: provisionPlan.Digest,
		RunID: uuid.NewString(), RunRevision: 1, RunDigest: coreexecution.Digest(strings.Repeat("d", 64)),
		StageID: uuid.NewString(), StageRevision: provisionPlan.Stages[0].Revision, StageDigest: provisionPlan.Stages[0].Digest,
		AttemptID: uuid.NewString(), StepRevision: 1, Target: reservation, Step: step,
		PolicyDigest: policyDigest, CostQuoteDigest: costDigest, FenceDigest: coreexecution.Digest(strings.Repeat("e", 64)),
	}
	completion, err := executor.Execute(ctx, req, credential)
	if err != nil || completion.Target.Revision != 2 || completion.Observation.State != "ready" {
		t.Fatalf("completion=%+v err=%v", completion, err)
	}
	revisionTwo, err := catalog.GetTarget(ctx, handoffOwner, reservation.ID, 2)
	if err != nil || revisionTwo.Kind != coreexecution.TargetKindAWSEC2Instance || catalog.observation.Observation.Facts[coreexecution.ObservationFactMemoryMiB] != "2048" {
		t.Fatalf("revision2=%+v observation=%+v err=%v", revisionTwo, catalog.observation, err)
	}
	containerResult, actionErr := port.Handle(ctx, handoffOwner, "agent.execution.v2.plans.create", map[string]any{
		"project_id": projectID, "analysis_id": analysisID, "intent": "deploy", "recipe_id": "generic-container-service",
		"target_id": revisionTwo.ID, "target_revision": 2, "purpose": "service", "idempotency_key": uuid.NewString(),
	})
	if actionErr != nil || containerResult == nil || len(catalog.plans) != 2 {
		t.Fatalf("container plan result=%+v err=%+v", containerResult, actionErr)
	}
	containerPlan := catalog.plans[1]
	if containerPlan.Targets[0].Revision != 2 || containerPlan.Targets[0].Digest != revisionTwo.Digest {
		t.Fatalf("container plan did not pin revision2: %+v", containerPlan.Targets)
	}
	boundContainer := false
	for _, stage := range containerPlan.Stages {
		for _, candidate := range stage.Steps {
			if candidate.Kind == coreexecution.StepContainerApply && candidate.ContainerApply != nil && candidate.ContainerApply.Image == analysis.Source.Location {
				boundContainer = true
			}
		}
	}
	if !boundContainer {
		t.Fatalf("container step was not resolved from revision2: %+v", containerPlan.Stages)
	}
}
