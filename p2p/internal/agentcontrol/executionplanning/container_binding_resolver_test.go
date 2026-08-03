package executionplanning

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/agentrecipes"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

const bindingAnalysisID = "66666666-6666-4666-8666-666666666666"

type containerBindingReaderStub struct {
	analysis    coreexecution.ProjectAnalysis
	observation storage.TargetObservationRecord
	err         error
}

func (s *containerBindingReaderStub) GetAnalysis(context.Context, string, string) (coreexecution.ProjectAnalysis, error) {
	if s.err != nil {
		return coreexecution.ProjectAnalysis{}, s.err
	}
	return s.analysis, nil
}

func (s *containerBindingReaderStub) GetLatestReadyTargetObservation(context.Context, string, string, uint64) (storage.TargetObservationRecord, error) {
	if s.err != nil {
		return storage.TargetObservationRecord{}, s.err
	}
	return s.observation, nil
}

func TestProductionContainerBindingResolverDerivesRestrictedBindings(t *testing.T) {
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	analysis := productionContainerAnalysis(t, now)
	target := productionContainerTarget(t)
	observation := productionContainerObservation(t, target, now)
	reader := &containerBindingReaderStub{analysis: analysis, observation: observation}
	resolver := &ProductionContainerBindingResolver{store: reader, now: func() time.Time { return now }}

	bindings, err := resolver.ResolveBindings(context.Background(), resolverOwner, productionContainerRequest(target), genericContainerRecipe(t), target)
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings.StepBindings) != 4 || bindings.ObservationRef.ObservationDigest != observation.Observation.Digest {
		t.Fatalf("unexpected binding closure: %+v", bindings)
	}
	bootstrap := bindings.StepBindings["ensure-package"]
	if bootstrap.PackageEnsure == nil || bootstrap.PackageEnsure.Manager != "dnf" || bootstrap.PackageEnsure.PlatformProfile != "amazon-linux-2023" || len(bootstrap.NetworkGrants) != 1 || bootstrap.NetworkGrants[0].Host != coreexecution.PublicHTTPSWildcardHost {
		t.Fatalf("bootstrap was not bound to observed AL2023: %+v", bootstrap)
	}
	apply := bindings.StepBindings["apply-container"]
	if apply.ContainerApply == nil || apply.ContainerApply.Image != analysis.Source.Location || apply.ContainerApply.HostAddress != "127.0.0.1" || apply.ContainerApply.RestartPolicy != "unless-stopped" || len(apply.NetworkGrants) != 1 || apply.NetworkGrants[0].Host != coreexecution.PublicHTTPSWildcardHost || apply.NetworkGrants[0].PathPrefix != "" {
		t.Fatalf("container binding did not disclose public HTTPS egress: %+v", apply)
	}
	probe := bindings.StepBindings["probe-http"]
	if probe.HTTPProbe == nil || probe.HTTPProbe.Mode != "target_local" || len(probe.NetworkGrants) != 1 || probe.NetworkGrants[0].Scope != "target_local" {
		t.Fatalf("probe was not exact loopback: %+v", probe)
	}
	if bindings.Placement.Kind != "existing_target" || bindings.Placement.Recommended.CostQuote.ExpiresAt != now.Add(30*time.Minute) {
		t.Fatalf("placement quote was not server-derived: %+v", bindings.Placement)
	}
}

func TestProductionContainerBindingResolverAllowsInitialDeployOnly(t *testing.T) {
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	analysis := productionContainerAnalysis(t, now)
	target := productionContainerTarget(t)
	reader := &containerBindingReaderStub{analysis: analysis, observation: productionContainerObservation(t, target, now)}
	resolver := &ProductionContainerBindingResolver{store: reader, now: func() time.Time { return now }}
	for _, intent := range []string{"upgrade", "repair", "container"} {
		req := productionContainerRequest(target)
		req.Intent = intent
		if _, err := resolver.ResolveBindings(context.Background(), resolverOwner, req, genericContainerRecipe(t), target); !errors.Is(err, ErrUncertain) {
			t.Fatalf("intent %q was accepted: %v", intent, err)
		}
	}
}

func TestProductionContainerBindingResolverFailsClosedWithoutProofs(t *testing.T) {
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*coreexecution.ProjectAnalysis, *coreexecution.ExecutionTarget, *storage.TargetObservationRecord)
	}{
		{name: "analysis blocker", mutate: func(a *coreexecution.ProjectAnalysis, _ *coreexecution.ExecutionTarget, _ *storage.TargetObservationRecord) {
			a.BlockingUncertainties = []string{"registry unverified"}
		}},
		{name: "target envelope", mutate: func(_ *coreexecution.ProjectAnalysis, target *coreexecution.ExecutionTarget, _ *storage.TargetObservationRecord) {
			target.Network.Mode = "none"
		}},
		{name: "instance capability mismatch", mutate: func(_ *coreexecution.ProjectAnalysis, target *coreexecution.ExecutionTarget, _ *storage.TargetObservationRecord) {
			for i, capability := range target.Capabilities {
				if strings.HasPrefix(capability, "target.instance.") {
					target.Capabilities[i] = "target.instance.i-aaaaaaaaaaaaaaaaa"
				}
			}
		}},
		{name: "stale observation", mutate: func(_ *coreexecution.ProjectAnalysis, _ *coreexecution.ExecutionTarget, observation *storage.TargetObservationRecord) {
			observation.Observation.ObservedAt = now.Add(-16 * time.Minute)
		}},
		{name: "missing security group proof", mutate: func(_ *coreexecution.ProjectAnalysis, _ *coreexecution.ExecutionTarget, observation *storage.TargetObservationRecord) {
			delete(observation.Observation.Facts, coreexecution.ObservationFactSecurityGroupDigest)
		}},
		{name: "wrong platform", mutate: func(_ *coreexecution.ProjectAnalysis, _ *coreexecution.ExecutionTarget, observation *storage.TargetObservationRecord) {
			observation.Observation.Facts["platform_name"] = "Ubuntu"
		}},
		{name: "image architecture mismatch", mutate: func(a *coreexecution.ProjectAnalysis, _ *coreexecution.ExecutionTarget, _ *storage.TargetObservationRecord) {
			a.Runtime.Architecture = "arm64"
		}},
		{name: "target architecture mismatch", mutate: func(_ *coreexecution.ProjectAnalysis, target *coreexecution.ExecutionTarget, observation *storage.TargetObservationRecord) {
			target.Architecture = "arm64"
			observation.Observation.Facts["architecture"] = "arm64"
		}},
		{name: "missing capacity proof", mutate: func(_ *coreexecution.ProjectAnalysis, _ *coreexecution.ExecutionTarget, observation *storage.TargetObservationRecord) {
			delete(observation.Observation.Facts, coreexecution.ObservationFactMemoryMiB)
		}},
		{name: "insufficient cpu", mutate: func(_ *coreexecution.ProjectAnalysis, _ *coreexecution.ExecutionTarget, observation *storage.TargetObservationRecord) {
			observation.Observation.Facts[coreexecution.ObservationFactVCPUCount] = "1"
		}},
		{name: "insufficient memory", mutate: func(_ *coreexecution.ProjectAnalysis, _ *coreexecution.ExecutionTarget, observation *storage.TargetObservationRecord) {
			observation.Observation.Facts[coreexecution.ObservationFactMemoryMiB] = "1024"
		}},
		{name: "insufficient disk", mutate: func(_ *coreexecution.ProjectAnalysis, _ *coreexecution.ExecutionTarget, observation *storage.TargetObservationRecord) {
			observation.Observation.Facts[coreexecution.ObservationFactRootVolumeGiB] = "7"
		}},
		{name: "secret requirement", mutate: func(a *coreexecution.ProjectAnalysis, _ *coreexecution.ExecutionTarget, _ *storage.TargetObservationRecord) {
			a.SecretPurposes = []string{"registry"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			analysis := productionContainerAnalysis(t, now)
			target := productionContainerTarget(t)
			observation := productionContainerObservation(t, target, now)
			test.mutate(&analysis, &target, &observation)
			analysis.Digest = ""
			analysis, _ = analysis.Normalize()
			target.Digest = ""
			target, _ = target.Normalize()
			observation.Observation.Digest = ""
			observation.Observation, _ = observation.Observation.Normalize()
			reader := &containerBindingReaderStub{analysis: analysis, observation: observation}
			resolver := &ProductionContainerBindingResolver{store: reader, now: func() time.Time { return now }}
			_, err := resolver.ResolveBindings(context.Background(), resolverOwner, productionContainerRequest(target), genericContainerRecipe(t), target)
			if !errors.Is(err, ErrUncertain) {
				t.Fatalf("missing proof was accepted: %v", err)
			}
		})
	}
}

func genericContainerRecipe(t *testing.T) agentrecipes.RecipeManifest {
	t.Helper()
	registry, err := agentrecipes.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, recipe := range registry.Manifests() {
		if recipe.ID == "generic-container-service" {
			return recipe
		}
	}
	t.Fatal("generic container recipe missing")
	return agentrecipes.RecipeManifest{}
}

func productionContainerRequest(target coreexecution.ExecutionTarget) agentembedded.ExecutionV2PlanCreateRequest {
	return agentembedded.ExecutionV2PlanCreateRequest{
		ProjectID: resolverProjectID, AnalysisID: bindingAnalysisID, Intent: "deploy", RecipeID: "generic-container-service",
		TargetID: target.ID, TargetRevision: target.Revision, Purpose: coreexecution.PurposeService,
		IdempotencyKey: "77777777-7777-4777-8777-777777777777",
	}
}

func productionContainerAnalysis(t *testing.T, now time.Time) coreexecution.ProjectAnalysis {
	t.Helper()
	a := coreexecution.ProjectAnalysis{
		AnalysisID: bindingAnalysisID, ProjectID: resolverProjectID,
		Source:         coreexecution.SourceRef{Kind: "oci_image", Location: "registry.example/service@sha256:" + strings.Repeat("a", 64), Immutable: true},
		DetectedStacks: []string{"oci_image"}, Runtime: coreexecution.ResourceRequirement{CPU: "2", Memory: "2048MiB", Disk: "8GiB", Architecture: "x86_64"}, Ports: []int{8080}, EnvironmentNames: []string{"PATH"}, Probes: []string{"http://127.0.0.1:8080/health"}, Exposure: "target_local",
		Revision: 1, CreatedAt: now, UpdatedAt: now,
	}
	normalized, err := a.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}

func productionContainerTarget(t *testing.T) coreexecution.ExecutionTarget {
	t.Helper()
	target := coreexecution.ExecutionTarget{
		ID: resolverTargetID, Provider: "aws", Kind: coreexecution.TargetKindAWSEC2Instance, InfrastructureProfileID: "general-linux-ssm-v1",
		AccountID: "123456789012", Region: "us-east-1", Architecture: "x86_64", Capabilities: []string{"target.aws_ec2_instance", "target.instance.i-0123456789abcdef0", "transport.aws_ssm"},
		CredentialRefs: []coreexecution.CredentialRef{{Ref: "88888888-8888-4888-8888-888888888888", Purpose: "aws", Revision: 1, BindingDigest: coreexecution.Digest(strings.Repeat("b", 64))}},
		Network:        coreexecution.NetworkPolicy{Mode: coreexecution.NetworkPolicyModeObservedHTTPSEgress}, Revision: 1,
	}
	normalized, err := target.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}

func productionContainerObservation(t *testing.T, target coreexecution.ExecutionTarget, now time.Time) storage.TargetObservationRecord {
	t.Helper()
	observation := coreexecution.TargetObservation{
		TargetID: target.ID, TargetRevision: target.Revision, ObservedAt: now, State: "ready",
		Facts: map[string]string{
			"instance_id": "i-0123456789abcdef0", "instance_type": "t3.small", "account_id": target.AccountID, "region": target.Region,
			"operating_system": "linux", "architecture": "x86_64", "ssm_status": "Online", "platform_name": "Amazon Linux", "platform_version": "2023",
			coreexecution.ObservationFactVCPUCount:           "2",
			coreexecution.ObservationFactMemoryMiB:           "2048",
			coreexecution.ObservationFactRootVolumeGiB:       "20",
			coreexecution.ObservationFactHTTPSEgress:         coreexecution.ObservationFactHTTPSEgressValue,
			coreexecution.ObservationFactSecurityGroupDigest: strings.Repeat("c", 64),
		},
	}
	normalized, err := observation.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return storage.TargetObservationRecord{OwnerID: resolverOwner, ObservationID: "99999999-9999-4999-8999-999999999999", Revision: 1, Status: "observed", Observation: normalized}
}
