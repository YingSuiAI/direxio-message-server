package agentrecipes

import (
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func TestBuiltinRecipesCompileToExecutionPlanStages(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	ctx := compileContext()
	for _, recipe := range r.Manifests() {
		ctx.StepBindings = bindingsForRecipe(recipe, ctx)
		stages, err := Compile(recipe, ctx)
		if err != nil {
			t.Fatalf("%s compile: %v", recipe.ID, err)
		}
		grants := []execution.NetworkGrant{}
		for _, stage := range stages {
			for _, step := range stage.Steps {
				grants = append(grants, step.NetworkGrants...)
			}
		}
		target := targetForRecipe(recipe, ctx, grants)
		if err := aws.ValidateInfrastructureTarget(target); err != nil {
			t.Fatalf("%s target contract: %v", recipe.ID, err)
		}
		plan := execution.ExecutionPlan{SchemaVersion: execution.SchemaVersion, ID: "22222222-2222-4222-8222-222222222222", Revision: 1, OwnerID: "@owner:example.org", ProjectID: "33333333-3333-4333-8333-333333333333", AnalysisID: "44444444-4444-4444-8444-444444444444", Purpose: execution.PurposeService, Placement: execution.PlacementRecommendation{Kind: "existing_target", Minimum: placement(), Recommended: placement(), HighPerformance: placement()}, Targets: []execution.ExecutionTarget{target}, Artifacts: []execution.ArtifactRef{{ID: "55555555-5555-4555-8555-555555555555", Digest: execution.Digest("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Immutable: true, MediaType: "application/octet-stream"}}, Stages: stages, ExpiresAt: time.Now().UTC().Add(time.Hour), Status: execution.PlanDraft}
		if _, err := plan.Normalize(); err != nil {
			t.Fatalf("%s execution compatibility: %v", recipe.ID, err)
		}
	}
}

func TestAIAuthorizationStagesCompileForAPIKeyAndAuthGate(t *testing.T) {
	registry, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var recipe RecipeManifest
	for _, candidate := range registry.Manifests() {
		if candidate.ID == "generic-container-service" {
			recipe = candidate
		}
	}
	if recipe.ID == "" {
		t.Fatal("generic container recipe missing")
	}
	ref := execution.CredentialRef{Ref: "66666666-6666-4666-8666-666666666666", Purpose: execution.AISecretPurposeProviderAPIKey, Revision: 2, BindingDigest: execution.Digest(strings.Repeat("d", 64))}
	api := &execution.AIConfiguration{Mode: execution.AIAuthModeAPIKey, Provider: "openrouter", SecretRef: ref.Ref, SecretRevision: ref.Revision, SecretPurpose: ref.Purpose, SecretBindingDigest: ref.BindingDigest}
	auth := &execution.AIConfiguration{Mode: execution.AIAuthModeAuthGate, Provider: "openrouter", Status: execution.AIExternalAuthPending}
	for _, tc := range []struct {
		name string
		cfg  *execution.AIConfiguration
	}{
		{name: "api", cfg: api}, {name: "auth", cfg: auth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := compileContext()
			ctx.AIConfiguration = tc.cfg
			ctx.SecretRefs = map[string]execution.CredentialRef{execution.AISecretPurposeProviderAPIKey: ref}
			ctx.StepBindings = bindingsForRecipe(recipe, ctx)
			stages, err := Compile(recipe, ctx)
			if err != nil {
				t.Fatal(err)
			}
			stages, err = AddAIAuthorizationStages(stages, tc.cfg)
			if err != nil {
				t.Fatal(err)
			}
			var authStage, apply *execution.ExecutionStage
			for i := range stages {
				if stages[i].StageKey == "authorize-ai" {
					authStage = &stages[i]
				}
				if stages[i].StageKey == "apply" {
					apply = &stages[i]
				}
			}
			if authStage == nil || apply == nil || !containsString(apply.DependsOn, "authorize-ai") {
				t.Fatalf("authorization stage was not directly linked: %+v", stages)
			}
			grants := []execution.NetworkGrant{}
			for _, stage := range stages {
				for _, step := range stage.Steps {
					grants = append(grants, step.NetworkGrants...)
				}
			}
			target := targetForRecipe(recipe, ctx, grants)
			plan := execution.ExecutionPlan{SchemaVersion: execution.SchemaVersion, ID: "22222222-2222-4222-8222-222222222222", Revision: 1, OwnerID: "@owner:example.org", ProjectID: "33333333-3333-4333-8333-333333333333", AnalysisID: "44444444-4444-4444-8444-444444444444", Purpose: execution.PurposeService, AIConfiguration: tc.cfg, Placement: execution.PlacementRecommendation{Kind: "existing_target", Minimum: placement(), Recommended: placement(), HighPerformance: placement()}, Targets: []execution.ExecutionTarget{target}, Stages: stages, ExpiresAt: time.Now().UTC().Add(time.Hour), Status: execution.PlanDraft}
			if _, err := plan.Normalize(); err != nil {
				t.Fatalf("AI plan did not normalize: %v", err)
			}
			if tc.name == "api" {
				if authStage.Steps[0].Kind != execution.StepSecretProvision || !reflect.DeepEqual(authStage.Steps[0].SecretRefs, []execution.CredentialRef{ref}) || authStage.Steps[0].SecretProvision == nil || authStage.Steps[0].SecretProvision.Delivery != "target_secure_parameter" {
					t.Fatalf("invalid secret stage: %+v", authStage)
				}
				if len(apply.Steps[0].SecretRefs) != 1 || apply.Steps[0].SecretRefs[0] != ref {
					t.Fatalf("container did not pin exact secret: %+v", apply.Steps[0])
				}
			} else if authStage.Steps[0].Kind != execution.StepExternalAuth || len(authStage.Steps[0].SecretRefs) != 0 || authStage.Steps[0].ExternalAuth == nil || authStage.Steps[0].ExternalAuth.Status != execution.AIExternalAuthPending {
				t.Fatalf("invalid external auth stage: %+v", authStage)
			}
		})
	}
}

func TestNoAIAuthorizationStageLeavesRecipeUnchanged(t *testing.T) {
	registry, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var recipe RecipeManifest
	for _, candidate := range registry.Manifests() {
		if candidate.ID == "generic-container-service" {
			recipe = candidate
		}
	}
	ctx := compileContext()
	ctx.StepBindings = bindingsForRecipe(recipe, ctx)
	before, err := Compile(recipe, ctx)
	if err != nil {
		t.Fatal(err)
	}
	after, err := AddAIAuthorizationStages(before, nil)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("no-AI recipe changed: err=%v before=%+v after=%+v", err, before, after)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestScriptRunCompilationRequiresPinnedObservation(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var recipe RecipeManifest
	for _, candidate := range r.Manifests() {
		if candidate.ID == "source-build-systemd" {
			recipe = candidate
		}
	}
	ctx := compileContext()
	ctx.StepBindings = bindingsForRecipe(recipe, ctx)
	ctx.ObservationRef = execution.TargetObservationRef{}
	if _, err := Compile(recipe, ctx); err == nil {
		t.Fatal("script.run compiled without target observation")
	}
	ctx = compileContext()
	ctx.StepBindings = bindingsForRecipe(recipe, ctx)
	ctx.ObservationRef.TargetRevision++
	if _, err := Compile(recipe, ctx); err == nil {
		t.Fatal("script.run compiled with mismatched target observation")
	}
}

func TestCompileRejectsBindingCoverageAndEnvelopeSmuggling(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var recipe RecipeManifest
	for _, candidate := range r.Manifests() {
		if candidate.ID == "generic-container-service" {
			recipe = candidate
		}
	}
	ctx := compileContext()
	ctx.StepBindings = bindingsForRecipe(recipe, ctx)
	delete(ctx.StepBindings, "apply-container")
	if _, err := Compile(recipe, ctx); err == nil {
		t.Fatal("missing binding was accepted")
	}
	ctx = compileContext()
	ctx.StepBindings = bindingsForRecipe(recipe, ctx)
	ctx.StepBindings["unused"] = execution.ExecutionStep{StepKey: "unused", Kind: execution.StepCleanup, Cleanup: &execution.CleanupStep{Resource: "x"}}
	if _, err := Compile(recipe, ctx); err == nil {
		t.Fatal("unused binding was accepted")
	}
	ctx = compileContext()
	ctx.StepBindings = bindingsForRecipe(recipe, ctx)
	ctx.StepBindings["inspect-target"] = execution.ExecutionStep{StepKey: "inspect-target", Kind: execution.StepCleanup, TargetDigest: execution.Digest("dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"), Cleanup: &execution.CleanupStep{Resource: "x"}}
	if _, err := Compile(recipe, ctx); err == nil {
		t.Fatal("wrong-kind/target digest binding was accepted")
	}
}

func TestCompileHasNoProviderOrProjectFallback(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var recipe RecipeManifest
	for _, candidate := range r.Manifests() {
		if candidate.ID == "generic-container-service" {
			recipe = candidate
		}
	}
	ctx := compileContext()
	ctx.StepBindings = bindingsForRecipe(recipe, ctx)
	ctx.StepBindings["apply-container"] = execution.ExecutionStep{StepKey: "apply-container", Kind: execution.StepContainerApply, ContainerApply: &execution.ContainerApplyStep{Image: "registry.example/unrelated@sha256:" + strings.Repeat("c", 64), Name: "unrelated", HostAddress: "127.0.0.1", HostPort: 8080, ContainerPort: 8080, RestartPolicy: "unless-stopped"}, NetworkGrants: grantsFor([]string{PublicHTTPSEgressGrant})}
	stages, err := Compile(recipe, ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range stages {
		for _, step := range stage.Steps {
			if step.Kind == execution.StepContainerApply {
				if step.ContainerApply == nil || step.ContainerApply.Name != "unrelated" || !strings.Contains(step.ContainerApply.Image, "unrelated@sha256:") {
					t.Fatal("typed project value was not preserved")
				}
			}
		}
	}
}

func TestGenericContainerRecipeHasNoDestructiveRollbackMetadata(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var recipe RecipeManifest
	for _, candidate := range r.Manifests() {
		if candidate.ID == "generic-container-service" {
			recipe = candidate
		}
	}
	ctx := compileContext()
	ctx.StepBindings = bindingsForRecipe(recipe, ctx)
	stages, err := Compile(recipe, ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range stages {
		if stage.Risk == execution.RiskR4 || stage.Gate == execution.GateRollback {
			t.Fatal("rollback entered forward stages")
		}
		if len(stage.RollbackSteps) != 0 || stage.RollbackPolicy != nil {
			t.Fatal("generic container recipe advertised destructive rollback")
		}
	}
}

func TestRootSecretEffectsAreDeduplicatedAndSorted(t *testing.T) {
	stage := RecipeStage{Gate: "remote_privileged_execution", Effects: []string{"remote_privileged_execution", "secret_access"}, Steps: []RecipeStep{{Root: true, SecretRefs: []string{"runtime"}}, {Root: true, SecretRefs: []string{"runtime"}}}}
	if gate := deriveStepGate(stage.Steps); gate != "remote_privileged_execution" {
		t.Fatalf("gate = %s", gate)
	}
	if got := deriveStepEffects(stage.Steps); !equalStringSets(got, stage.Effects) {
		t.Fatalf("effects = %v", got)
	}
	got := materializeEffects(stage)
	if len(got) != 2 || got[0] != execution.GateRemotePrivilegedExecution || got[1] != execution.GateSecretAccess {
		t.Fatalf("compiled effects = %v", got)
	}
}

func TestRootSecretRecipeCompilesToNormalizedPlan(t *testing.T) {
	r, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	var recipe RecipeManifest
	for _, candidate := range r.Manifests() {
		if candidate.ID == "source-build-systemd" {
			recipe = candidate
		}
	}
	recipe.SecretPurposes = []string{"runtime"}
	for i := range recipe.Stages {
		if recipe.Stages[i].StageKey == "build" {
			recipe.Stages[i].Gate = "remote_privileged_execution"
			recipe.Stages[i].Effects = []string{"remote_privileged_execution", "secret_access"}
			recipe.Stages[i].Steps[0].Root = true
			recipe.Stages[i].Steps[0].SecretRefs = []string{"runtime"}
		}
	}
	if err := Validate(recipe); err != nil {
		t.Fatal(err)
	}
	ctx := compileContext()
	ctx.SecretRefs = map[string]execution.CredentialRef{"runtime": {Ref: "66666666-6666-4666-8666-666666666666", Purpose: "runtime", Revision: 1, BindingDigest: execution.Digest("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")}}
	ctx.StepBindings = bindingsForRecipe(recipe, ctx)
	stages, err := Compile(recipe, ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range stages {
		if stages[i].StageKey == "build" {
			stages[i].DependsOn = append(stages[i].DependsOn, "authorize-runtime")
		}
	}
	stages = append(stages, execution.ExecutionStage{
		StageKey: "authorize-runtime", Revision: 1, Kind: string(execution.StepSecretProvision), Risk: execution.RiskR2, Gate: execution.GateSecretAccess,
		Effects: []execution.Gate{execution.GateSecretAccess}, TargetID: ctx.TargetID, TargetRevision: ctx.TargetRevision, TargetDigest: ctx.TargetDigest,
		Steps: []execution.ExecutionStep{{StepKey: "provision-runtime", Kind: execution.StepSecretProvision, TargetID: ctx.TargetID, TargetRevision: ctx.TargetRevision, TargetDigest: ctx.TargetDigest, SecretRefs: []execution.CredentialRef{ctx.SecretRefs["runtime"]}, TimeoutSeconds: 20, IdempotencyMarker: "provision-runtime", OutputPolicy: execution.OutputDiscard, SecretProvision: &execution.SecretProvisionStep{Delivery: "target_secure_parameter"}}}, TimeoutSeconds: 30,
	})
	grants := []execution.NetworkGrant{}
	for _, stage := range stages {
		for _, step := range stage.Steps {
			grants = append(grants, step.NetworkGrants...)
		}
	}
	target := targetForRecipe(recipe, ctx, grants)
	if err := aws.ValidateInfrastructureTarget(target); err != nil {
		t.Fatalf("target contract: %v", err)
	}
	plan := execution.ExecutionPlan{SchemaVersion: execution.SchemaVersion, ID: "22222222-2222-4222-8222-222222222222", Revision: 1, OwnerID: "@owner:example.org", ProjectID: "33333333-3333-4333-8333-333333333333", AnalysisID: "44444444-4444-4444-8444-444444444444", Purpose: execution.PurposeService, Placement: execution.PlacementRecommendation{Kind: "existing_target", Minimum: placement(), Recommended: placement(), HighPerformance: placement()}, Targets: []execution.ExecutionTarget{target}, Artifacts: []execution.ArtifactRef{{ID: "55555555-5555-4555-8555-555555555555", Digest: execution.Digest("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), Immutable: true, MediaType: "application/octet-stream"}}, Stages: stages, ExpiresAt: time.Now().UTC().Add(time.Hour), Status: execution.PlanDraft}
	normalized, err := plan.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range normalized.Stages {
		if stage.StageKey == "build" {
			if stage.Gate != execution.GateRemotePrivilegedExecution || len(stage.Effects) != 2 || stage.Effects[0] != execution.GateRemotePrivilegedExecution || stage.Effects[1] != execution.GateSecretAccess || stage.Steps[0].SecretRefs[0].Ref != ctx.SecretRefs["runtime"].Ref {
				t.Fatal("root/secret binding was not sealed")
			}
		}
	}
}
func placement() execution.PlacementOption {
	return execution.PlacementOption{Region: "us-east-1", Spec: "small", Disk: "small", Network: "private", CostQuote: execution.CostQuote{Amount: "0", Currency: "USD", ExpiresAt: time.Now().UTC().Add(time.Hour)}}
}

func compileContext() CompileContext {
	return CompileContext{TargetID: "11111111-1111-4111-8111-111111111111", TargetRevision: 1, TargetDigest: execution.Digest("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), ObservationRef: execution.TargetObservationRef{ObservationID: "99999999-9999-4999-8999-999999999999", TargetID: "11111111-1111-4111-8111-111111111111", TargetRevision: 1, ObservationDigest: execution.Digest("cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc")}}
}

func bindingsForRecipe(recipe RecipeManifest, ctx CompileContext) map[string]execution.ExecutionStep {
	artifact := execution.ArtifactRef{ID: "55555555-5555-4555-8555-555555555555", Digest: execution.Digest("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"), MediaType: "application/octet-stream", Immutable: true}
	out := map[string]execution.ExecutionStep{}
	for _, stage := range recipe.Stages {
		for _, step := range append(append([]RecipeStep(nil), stage.Steps...), stage.RollbackSteps...) {
			b := execution.ExecutionStep{StepKey: step.StepKey, Kind: execution.StepKind(step.Kind), NetworkGrants: grantsFor(step.NetworkGrants)}
			switch step.Kind {
			case "target.inspect":
				b.TargetInspect = &execution.TargetInspectStep{ObservationID: ctx.ObservationRef.ObservationID}
			case "compute.provision":
				b.ComputeProvision = &execution.ComputeProvisionStep{InfrastructureProfileID: "general-linux-ssm-v1", AMIParameter: execution.AWSAL2023X8664AMIParameter, InstanceType: "t3.small", AvailabilityZone: "us-east-1a", VolumeGiB: 20, Region: "us-east-1", Architecture: "x86_64", ManagementTransport: "aws_ssm", PublicIP: true}
			case "compute.destroy":
				b.ComputeDestroy = &execution.ComputeDestroyStep{ResourceID: ctx.TargetID}
			case "source.fetch":
				b.SourceFetch = &execution.SourceFetchStep{Source: execution.SourceRef{Kind: "git_https", Location: "https://source.example/project", Commit: strings.Repeat("a", 40), Immutable: true}, Artifact: artifact}
			case "artifact.upload":
				b.ArtifactUpload = &execution.ArtifactUploadStep{Artifact: artifact, Destination: "/tmp/artifact"}
			case "package.ensure":
				b.PackageEnsure = &execution.PackageEnsureStep{Name: "docker", Manager: "dnf", PlatformProfile: "amazon-linux-2023"}
			case "file.put":
				b.FilePut = &execution.FilePutStep{Path: "/tmp/artifact", Artifact: artifact}
			case "container.apply":
				b.ContainerApply = &execution.ContainerApplyStep{Image: "registry.example/project@sha256:" + strings.Repeat("b", 64), Name: "project", HostAddress: "127.0.0.1", HostPort: 8080, ContainerPort: 8080, RestartPolicy: "unless-stopped"}
			case "systemd.apply":
				b.SystemdApply = &execution.SystemdApplyStep{Unit: "project.service", Artifact: artifact}
			case "script.run":
				b.ScriptRun = &execution.ScriptRunStep{Artifact: artifact, Interpreter: "/bin/bash", Argv: []string{"/tmp/build.sh"}, CWD: "/tmp", Root: step.Root, AllowedExitCodes: []int{0}, TimeoutSeconds: step.TimeoutSeconds, OutputLimit: step.OutputLimit, Redaction: execution.RedactionPolicy{Patterns: []string{"SECRET"}, Replace: "[REDACTED]"}, Postcondition: &execution.Postcondition{Type: "service_active", Value: "project.service"}, IdempotencyMarker: step.IdempotencyMarker, NetworkGrants: b.NetworkGrants}
			case "http.probe":
				b.HTTPProbe = &execution.HTTPProbeStep{Mode: "target_local", URL: "http://127.0.0.1:8080/health", ExpectedStatus: []int{200}}
			case "tcp.probe":
				b.TCPProbe = &execution.TCPProbeStep{Mode: "target_local", Address: "127.0.0.1", Port: 8080}
			case "artifact.collect":
				b.ArtifactCollect = &execution.ArtifactCollectStep{Paths: []string{"/tmp/artifact"}, OutputKey: "artifact-output"}
			case "cleanup":
				b.Cleanup = &execution.CleanupStep{Resource: "project"}
			case "secret.provision":
				b.SecretProvision = &execution.SecretProvisionStep{Delivery: "target_secure_parameter"}
			}
			out[step.StepKey] = b
		}
	}
	return out
}

func grantsFor(declared []string) []execution.NetworkGrant {
	out := make([]execution.NetworkGrant, 0, len(declared))
	for _, raw := range declared {
		if strings.HasPrefix(raw, "target_local:") {
			g, _ := (execution.NetworkGrant{Scheme: "http", Host: "127.0.0.1", Port: 8080, PathPrefix: "/health", Scope: "target_local"}).Normalize()
			out = append(out, g)
			continue
		}
		if raw == PublicHTTPSEgressGrant {
			g, _ := (execution.NetworkGrant{Scheme: "https", Host: execution.PublicHTTPSWildcardHost, Port: 443, Scope: "external"}).Normalize()
			out = append(out, g)
			continue
		}
		if raw == SourceOCIRegistryGrant {
			g, _ := (execution.NetworkGrant{Scheme: "https", Host: "registry.example", Port: 443, PathPrefix: "/project", Scope: "external"}).Normalize()
			out = append(out, g)
			continue
		}
		u, _ := url.Parse(strings.TrimPrefix(raw, "egress:"))
		path := strings.ReplaceAll(u.Path, "${repository}", "project")
		g, _ := (execution.NetworkGrant{Scheme: u.Scheme, Host: u.Hostname(), Port: 443, PathPrefix: path, Scope: "external"}).Normalize()
		out = append(out, g)
	}
	return out
}

func targetForRecipe(recipe RecipeManifest, ctx CompileContext, grants []execution.NetworkGrant) execution.ExecutionTarget {
	unique := make([]execution.NetworkGrant, 0, len(grants))
	seen := map[execution.Digest]bool{}
	for _, grant := range grants {
		normalized, err := grant.Normalize()
		if err == nil && !seen[normalized.Digest] {
			seen[normalized.Digest] = true
			unique = append(unique, normalized)
		}
	}
	profile := aws.InfrastructureProfileGeneralLinuxSSMV1
	capabilities := []string{"target.aws_ec2_instance", "transport.aws_ssm"}
	if contains(recipe.RequiredTargetCapabilities, "runtime.container") {
		profile = aws.InfrastructureProfileContainerHostV1
		capabilities = append(capabilities, "runtime.container")
	}
	return execution.ExecutionTarget{ID: ctx.TargetID, Provider: "aws", Kind: "aws_ec2_instance", InfrastructureProfileID: profile, AccountID: "123456789012", Region: "us-east-1", Architecture: "x86_64", Capabilities: capabilities, Network: execution.NetworkPolicy{Allow: unique}, Revision: ctx.TargetRevision}
}
