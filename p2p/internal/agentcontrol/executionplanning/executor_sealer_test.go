package executionplanning

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

const (
	sealerProjectID     = "11111111-1111-4111-8111-111111111111"
	sealerPlanID        = "22222222-2222-4222-8222-222222222222"
	sealerTargetID      = "33333333-3333-4333-8333-333333333333"
	sealerObservationID = "44444444-4444-4444-8444-444444444444"
)

func TestArtifactExecutorSealerFreezesTypedStepsDeterministically(t *testing.T) {
	store, err := artifactstore.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sealer := NewArtifactExecutorSealer(store)
	request := ExecutorSealRequest{
		OwnerID: "@owner:example.org", ProjectID: sealerProjectID, PlanID: sealerPlanID, PlanRevision: 1,
		Observation: sealerObservationRef(), Stages: sealerStages(t),
	}
	first, err := sealer.SealExecutors(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := sealer.SealExecutors(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Stages) != 3 || len(first.Artifacts) != 3 {
		t.Fatalf("sealed stages/artifacts = %d/%d", len(first.Stages), len(first.Artifacts))
	}
	if got := first.Stages[0].DependsOn; len(got) != 0 {
		t.Fatalf("removed observation dependency survived: %v", got)
	}
	for i := range first.Artifacts {
		if first.Artifacts[i] != second.Artifacts[i] {
			t.Fatalf("artifact %d was not deterministic", i)
		}
		meta, statErr := store.Stat(context.Background(), string(first.Artifacts[i].Digest))
		if statErr != nil || meta.Size != first.Artifacts[i].Size {
			t.Fatalf("artifact %d missing from CAS: %+v, %v", i, meta, statErr)
		}
	}
	for _, stage := range first.Stages {
		for _, step := range append(append([]coreexecution.ExecutionStep(nil), stage.Steps...), stage.RollbackSteps...) {
			if step.Executor == nil || step.ObservationRef == nil || step.Executor.Artifact.Digest == "" || step.OutputPolicy != coreexecution.OutputDiscard {
				t.Fatalf("typed step was not sealed: %+v", step)
			}
			if step.Kind == coreexecution.StepHTTPProbe && step.Executor.Root {
				t.Fatal("HTTP probe unexpectedly requires root")
			}
		}
	}

	apply := first.Stages[1].Steps[0]
	reader, _, err := store.Open(context.Background(), string(apply.Executor.Artifact.Digest))
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || !strings.Contains(string(data), "registry.example/service@sha256:") || !strings.Contains(string(data), "127.0.0.1:8080:8080") || strings.Contains(string(data), "docker rm -f") || !strings.Contains(string(data), "exit 73") {
		t.Fatalf("container artifact does not bind exact typed inputs: %q, %v", data, err)
	}

	target := sealerTarget(t)
	now := time.Now().UTC()
	for _, stage := range first.Stages {
		for _, step := range stage.Steps {
			if _, err := coreexecution.StepSnapshotFromStep(step, coreexecution.StepSetForward); err != nil {
				t.Fatalf("sealed %s/%s step invalid: %v", stage.StageKey, step.StepKey, err)
			}
		}
		for _, step := range stage.RollbackSteps {
			if _, err := coreexecution.StepSnapshotFromStep(step, coreexecution.StepSetRollback); err != nil {
				t.Fatalf("sealed %s/%s rollback step invalid: %v", stage.StageKey, step.StepKey, err)
			}
		}
	}
	placement := coreexecution.PlacementOption{Region: "us-east-1", Spec: "t3.small", Disk: "existing", Network: "restricted", CostQuote: coreexecution.CostQuote{Amount: "0", Currency: "USD", ExpiresAt: now.Add(time.Hour)}}
	plan := coreexecution.ExecutionPlan{
		SchemaVersion: coreexecution.SchemaVersion, ID: sealerPlanID, Revision: 1, OwnerID: "@owner:example.org",
		ProjectID: sealerProjectID, AnalysisID: "55555555-5555-4555-8555-555555555555", Purpose: coreexecution.PurposeService,
		Placement: coreexecution.PlacementRecommendation{Kind: "existing_target", Minimum: placement, Recommended: placement, HighPerformance: placement},
		Targets:   []coreexecution.ExecutionTarget{target}, Artifacts: first.Artifacts, Stages: first.Stages,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour), Status: coreexecution.PlanReady,
	}
	normalized, err := plan.NormalizeAt(now)
	if err != nil || !normalized.Digest.Valid() {
		t.Fatalf("sealed executor artifacts were not part of a valid plan: %v", err)
	}
	mutated := normalized
	mutated.Digest = ""
	mutated.Artifacts[0].Digest = coreexecution.Digest(strings.Repeat("f", 64))
	if _, err := mutated.NormalizeAt(now); err == nil {
		t.Fatal("plan accepted an executor artifact digest mutation")
	}
}

func TestArtifactExecutorSealerRejectsUnboundedContainerInputs(t *testing.T) {
	store, err := artifactstore.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sealer := NewArtifactExecutorSealer(store)
	base := ExecutorSealRequest{OwnerID: "@owner:example.org", ProjectID: sealerProjectID, PlanID: sealerPlanID, PlanRevision: 1, Observation: sealerObservationRef(), Stages: sealerStages(t)}

	base.Stages[3].Steps[0].HTTPProbe.URL = "http://169.254.169.254/latest/meta-data"
	if _, err := sealer.SealExecutors(context.Background(), base); err == nil {
		t.Fatal("metadata endpoint probe was accepted")
	}
	base.Stages = sealerStages(t)
	base.Stages[2].Steps[0].ContainerApply.RestartPolicy = "always"
	if _, err := sealer.SealExecutors(context.Background(), base); err == nil {
		t.Fatal("unapproved restart policy was accepted")
	}
	base.Stages = sealerStages(t)
	base.Stages[2].Steps[0].NetworkGrants[0].Host = "different.example"
	base.Stages[2].Steps[0].NetworkGrants[0].Digest = ""
	if _, err := sealer.SealExecutors(context.Background(), base); err == nil {
		t.Fatal("false hostname-scoped grant was accepted")
	}
	base.Stages = sealerStages(t)
	base.Stages[1].Steps[0].NetworkGrants = nil
	if _, err := sealer.SealExecutors(context.Background(), base); err == nil {
		t.Fatal("package bootstrap without its public HTTPS grant was accepted")
	}
}

func TestArtifactExecutorSealerPreservesProviderNativeComputeProvisionWithoutHostObservation(t *testing.T) {
	store, err := artifactstore.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	step := coreexecution.ExecutionStep{StepKey: "provision-compute", Kind: coreexecution.StepComputeProvision, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: coreexecution.Digest(strings.Repeat("a", 64)), TimeoutSeconds: 1800, IdempotencyMarker: "provision-compute", ComputeProvision: &coreexecution.ComputeProvisionStep{InfrastructureProfileID: "general-linux-ssm-v1", AMIParameter: coreexecution.AWSAL2023X8664AMIParameter, InstanceType: "t3.small", AvailabilityZone: "us-east-1a", VolumeGiB: 20, Region: "us-east-1", Architecture: "x86_64", ManagementTransport: "aws_ssm", PublicIP: true}}
	stage := coreexecution.ExecutionStage{StageKey: "provision-compute", Revision: 1, Kind: string(coreexecution.StepComputeProvision), Risk: coreexecution.RiskR2, Gate: coreexecution.GateResourcePurchase, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: step.TargetDigest, Steps: []coreexecution.ExecutionStep{step}, TimeoutSeconds: 2100}
	sealed, err := NewArtifactExecutorSealer(store).SealExecutors(context.Background(), ExecutorSealRequest{OwnerID: "@owner:example.org", ProjectID: sealerProjectID, PlanID: sealerPlanID, PlanRevision: 1, Stages: []coreexecution.ExecutionStage{stage}})
	if err != nil || len(sealed.Stages) != 1 || len(sealed.Artifacts) != 0 || sealed.Stages[0].Steps[0].Executor != nil || sealed.Stages[0].Steps[0].ComputeProvision == nil {
		t.Fatalf("sealed=%+v err=%v", sealed, err)
	}
}

func TestArtifactExecutorSealerAcceptsTypedAIControlStepsWithoutArtifacts(t *testing.T) {
	store, err := artifactstore.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	ref := coreexecution.CredentialRef{Ref: "99999999-9999-4999-8999-999999999999", Purpose: coreexecution.AISecretPurposeProviderAPIKey, Revision: 1, BindingDigest: coreexecution.Digest(strings.Repeat("9", 64))}
	targetDigest := sealerTarget(t).Digest
	secret := coreexecution.ExecutionStep{StepKey: "authorize-ai", Kind: coreexecution.StepSecretProvision, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: targetDigest, SecretRefs: []coreexecution.CredentialRef{ref}, TimeoutSeconds: 60, IdempotencyMarker: "authorize-ai", OutputPolicy: coreexecution.OutputDiscard, SecretProvision: &coreexecution.SecretProvisionStep{Delivery: "target_secure_parameter"}}
	auth := coreexecution.ExecutionStep{StepKey: "authorize-ai", Kind: coreexecution.StepExternalAuth, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: targetDigest, TimeoutSeconds: 60, IdempotencyMarker: "authorize-ai", OutputPolicy: coreexecution.OutputDiscard, ExternalAuth: &coreexecution.ExternalAuthStep{Provider: "openrouter", Status: coreexecution.AIExternalAuthPending}}
	for _, step := range []coreexecution.ExecutionStep{secret, auth} {
		stage := coreexecution.ExecutionStage{StageKey: string(step.Kind), Revision: 1, Kind: string(step.Kind), Risk: coreexecution.RiskR2, Gate: coreexecution.GateSecretAccess, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: targetDigest, Steps: []coreexecution.ExecutionStep{step}, TimeoutSeconds: 120}
		if step.Kind == coreexecution.StepExternalAuth {
			stage.Gate = coreexecution.GateExternalAuth
		}
		sealed, err := NewArtifactExecutorSealer(store).SealExecutors(context.Background(), ExecutorSealRequest{OwnerID: "@owner:example.org", ProjectID: sealerProjectID, PlanID: sealerPlanID, PlanRevision: 1, Observation: sealerObservationRef(), Stages: []coreexecution.ExecutionStage{stage}})
		if err != nil || len(sealed.Artifacts) != 0 || len(sealed.Stages) != 1 || sealed.Stages[0].Steps[0].Executor != nil {
			t.Fatalf("control step was artifactized or rejected: kind=%s sealed=%+v err=%v", step.Kind, sealed, err)
		}
	}
	bad := secret
	bad.SecretProvision = &coreexecution.SecretProvisionStep{Delivery: "inline"}
	stage := coreexecution.ExecutionStage{StageKey: "bad", Revision: 1, Kind: string(secret.Kind), Risk: coreexecution.RiskR2, Gate: coreexecution.GateSecretAccess, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: targetDigest, Steps: []coreexecution.ExecutionStep{bad}, TimeoutSeconds: 120}
	if _, err := NewArtifactExecutorSealer(store).SealExecutors(context.Background(), ExecutorSealRequest{OwnerID: "@owner:example.org", ProjectID: sealerProjectID, PlanID: sealerPlanID, PlanRevision: 1, Observation: sealerObservationRef(), Stages: []coreexecution.ExecutionStage{stage}}); err == nil {
		t.Fatal("unapproved secret delivery was accepted")
	}
}

func TestArtifactExecutorSealerUsesTargetFileMountWithoutSecretBytes(t *testing.T) {
	store, err := artifactstore.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	stages := sealerStages(t)
	secretRef := coreexecution.CredentialRef{Ref: "99999999-9999-4999-8999-999999999999", Purpose: coreexecution.AISecretPurposeProviderAPIKey, Revision: 1, BindingDigest: coreexecution.Digest(strings.Repeat("9", 64))}
	stages[2].Steps[0].SecretRefs = []coreexecution.CredentialRef{secretRef}
	cleanup := coreexecution.ExecutionStep{StepKey: "cleanup-container", Kind: coreexecution.StepCleanup, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: sealerTarget(t).Digest, TimeoutSeconds: 120, IdempotencyMarker: "cleanup-container", Cleanup: &coreexecution.CleanupStep{Resource: "dirextalk-service"}}
	stages = append(stages, coreexecution.ExecutionStage{StageKey: "cleanup", Revision: 1, Kind: "cleanup", Risk: coreexecution.RiskR4, Gate: coreexecution.GateServiceDestroy, DependsOn: []string{"apply"}, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: cleanup.TargetDigest, Steps: []coreexecution.ExecutionStep{cleanup}, TimeoutSeconds: 180})

	sealed, err := NewArtifactExecutorSealer(store).SealExecutors(context.Background(), ExecutorSealRequest{OwnerID: "@owner:example.org", ProjectID: sealerProjectID, PlanID: sealerPlanID, PlanRevision: 1, Observation: sealerObservationRef(), Stages: stages})
	if err != nil {
		t.Fatal(err)
	}
	var apply, clean coreexecution.ExecutionStep
	for _, stage := range sealed.Stages {
		for _, step := range stage.Steps {
			switch step.Kind {
			case coreexecution.StepContainerApply:
				apply = step
			case coreexecution.StepCleanup:
				clean = step
			}
		}
	}
	read := func(step coreexecution.ExecutionStep) string {
		t.Helper()
		if step.Executor == nil {
			t.Fatalf("%s executor missing", step.Kind)
		}
		reader, _, openErr := store.Open(context.Background(), string(step.Executor.Artifact.Digest))
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer reader.Close()
		body, readErr := io.ReadAll(reader)
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(body)
	}
	applyBody := read(apply)
	for _, required := range []string{"aws ssm get-parameter --name \"$1\" --with-decryption", "chmod 0400", "dst=/run/secrets/dirextalk,readonly", "/run/dirextalk/secrets/dirextalk-service", "trap 'rm -f -- \"$secret_file\"' EXIT", coreexecution.AISecretPurposeProviderAPIKey} {
		if !strings.Contains(applyBody, required) {
			t.Fatalf("secret executor missing %q: %s", required, applyBody)
		}
	}
	for _, forbidden := range []string{"provider-api-key-value", secretRef.Ref, string(secretRef.BindingDigest), "-e API_KEY="} {
		if strings.Contains(applyBody, forbidden) {
			t.Fatalf("secret executor contains forbidden material %q", forbidden)
		}
	}
	cleanupBody := read(clean)
	if !strings.Contains(cleanupBody, "rm -rf -- '/run/dirextalk/secrets/dirextalk-service'") {
		t.Fatalf("cleanup does not revoke target file mount: %s", cleanupBody)
	}

	rotated := sealerStages(t)
	secretRef.Revision = 2
	secretRef.BindingDigest = coreexecution.Digest(strings.Repeat("8", 64))
	rotated[2].Steps[0].SecretRefs = []coreexecution.CredentialRef{secretRef}
	rotatedSeal, err := NewArtifactExecutorSealer(store).SealExecutors(context.Background(), ExecutorSealRequest{OwnerID: "@owner:example.org", ProjectID: sealerProjectID, PlanID: sealerPlanID, PlanRevision: 1, Observation: sealerObservationRef(), Stages: rotated})
	if err != nil {
		t.Fatal(err)
	}
	if apply.Executor.Artifact.Digest == rotatedSeal.Stages[1].Steps[0].Executor.Artifact.Digest {
		t.Fatal("secret revision rotation did not change the sealed execution artifact")
	}
}

func sealerObservationRef() coreexecution.TargetObservationRef {
	return coreexecution.TargetObservationRef{ObservationID: sealerObservationID, TargetID: sealerTargetID, TargetRevision: 1, ObservationDigest: coreexecution.Digest(strings.Repeat("a", 64))}
}

func sealerStages(t *testing.T) []coreexecution.ExecutionStage {
	t.Helper()
	targetDigest := sealerTarget(t).Digest
	external, err := (coreexecution.NetworkGrant{Scheme: "https", Host: coreexecution.PublicHTTPSWildcardHost, Port: 443, Scope: "external"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	local, err := (coreexecution.NetworkGrant{Scheme: "http", Host: "127.0.0.1", Port: 8080, PathPrefix: "/health", Scope: "target_local"}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	common := func(key string, kind coreexecution.StepKind) coreexecution.ExecutionStep {
		return coreexecution.ExecutionStep{StepKey: key, Kind: kind, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: targetDigest, TimeoutSeconds: 120, IdempotencyMarker: key}
	}
	inspect := common("inspect-target", coreexecution.StepTargetInspect)
	inspect.TargetInspect = &coreexecution.TargetInspectStep{ObservationID: sealerObservationID}
	bootstrap := common("ensure-package", coreexecution.StepPackageEnsure)
	bootstrap.NetworkGrants = []coreexecution.NetworkGrant{external}
	bootstrap.PackageEnsure = &coreexecution.PackageEnsureStep{Name: "docker", Manager: "dnf", PlatformProfile: "amazon-linux-2023"}
	apply := common("apply-container", coreexecution.StepContainerApply)
	apply.NetworkGrants = []coreexecution.NetworkGrant{external}
	apply.ContainerApply = &coreexecution.ContainerApplyStep{Image: "registry.example/service@sha256:" + strings.Repeat("c", 64), Name: "dirextalk-service", HostAddress: "127.0.0.1", HostPort: 8080, ContainerPort: 8080, RestartPolicy: "unless-stopped"}
	probe := common("probe-http", coreexecution.StepHTTPProbe)
	probe.TimeoutSeconds = 30
	probe.NetworkGrants = []coreexecution.NetworkGrant{local}
	probe.HTTPProbe = &coreexecution.HTTPProbeStep{URL: "http://127.0.0.1:8080/health", Mode: "target_local", ExpectedStatus: []int{200}}
	return []coreexecution.ExecutionStage{
		{StageKey: "inspect", Revision: 1, Kind: "target.inspect", Risk: coreexecution.RiskR0, Gate: coreexecution.GateNone, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: targetDigest, Steps: []coreexecution.ExecutionStep{inspect}, TimeoutSeconds: 60},
		{StageKey: "bootstrap", Revision: 1, Kind: "package.ensure", Risk: coreexecution.RiskR2, Gate: coreexecution.GateRemotePrivilegedExecution, DependsOn: []string{"inspect"}, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: targetDigest, Steps: []coreexecution.ExecutionStep{bootstrap}, TimeoutSeconds: 180},
		{StageKey: "apply", Revision: 1, Kind: "container.apply", Risk: coreexecution.RiskR2, Gate: coreexecution.GateRemotePrivilegedExecution, DependsOn: []string{"bootstrap"}, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: targetDigest, Steps: []coreexecution.ExecutionStep{apply}, TimeoutSeconds: 240},
		{StageKey: "probe", Revision: 1, Kind: "http.probe", Risk: coreexecution.RiskR0, Gate: coreexecution.GateNone, DependsOn: []string{"apply"}, TargetID: sealerTargetID, TargetRevision: 1, TargetDigest: targetDigest, Steps: []coreexecution.ExecutionStep{probe}, TimeoutSeconds: 60},
	}
}

func sealerTarget(t *testing.T) coreexecution.ExecutionTarget {
	t.Helper()
	target, err := (coreexecution.ExecutionTarget{
		ID: sealerTargetID, Provider: "aws", Kind: "aws_ec2_instance", InfrastructureProfileID: "general-linux-ssm-v1",
		AccountID: "123456789012", Region: "us-east-1", Architecture: "x86_64",
		Capabilities: []string{"target.aws_ec2_instance", "transport.aws_ssm"},
		Network:      coreexecution.NetworkPolicy{Mode: coreexecution.NetworkPolicyModeObservedHTTPSEgress}, Revision: 1,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return target
}
