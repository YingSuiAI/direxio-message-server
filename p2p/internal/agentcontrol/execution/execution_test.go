package execution

import (
	"testing"
	"time"
)

const (
	ownerID    = "@owner:example.org"
	planID     = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	targetID   = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	artifactID = "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	projectID  = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	analysisID = "ffffffff-ffff-4fff-8fff-ffffffffffff"
)

var sha = Digest("0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

func observationRef() *TargetObservationRef {
	return &TargetObservationRef{ObservationID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", TargetID: targetID, TargetRevision: 1, ObservationDigest: Digest("fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210")}
}

func grant(host string) NetworkGrant {
	return NetworkGrant{Scheme: "https", Host: host, Port: 443, Scope: "external"}
}

func target() ExecutionTarget {
	n, err := (ExecutionTarget{ID: targetID, Provider: "aws", Kind: "aws_ec2_instance", AccountID: "123456789012", Region: "us-east-1", Revision: 1}).Normalize()
	if err != nil {
		panic(err)
	}
	return n
}
func artifact() ArtifactRef {
	return ArtifactRef{ID: artifactID, Digest: sha, Immutable: true, Size: 1}
}
func placement() PlacementRecommendation {
	q := CostQuote{Amount: "1", Currency: "USD", ExpiresAt: time.Date(2030, 1, 2, 0, 0, 0, 0, time.UTC)}
	o := PlacementOption{Region: "us-east-1", Spec: "t3.small", Disk: "20GiB", Network: "private", CostQuote: q}
	return PlacementRecommendation{Kind: "existing_target", Minimum: o, Recommended: o, HighPerformance: o}
}
func plan() ExecutionPlan {
	created := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	t := target()
	s := ExecutionStep{StepKey: "run", Kind: StepScriptRun, TargetID: targetID, TargetRevision: 1, TargetDigest: t.Digest, ObservationRef: observationRef(), TimeoutSeconds: 30, IdempotencyMarker: "idem", OutputPolicy: OutputCapture, Postcondition: &Postcondition{Type: "exit_code", Value: "0"}, ScriptRun: &ScriptRunStep{Artifact: artifact(), Interpreter: "/bin/sh", Argv: []string{"-e"}, CWD: "/", Root: true, AllowedExitCodes: []int{0}, TimeoutSeconds: 30, OutputLimit: 1024, Redaction: RedactionPolicy{Patterns: []string{"secret"}, Replace: "[REDACTED]"}, Postcondition: &Postcondition{Type: "exit_code", Value: "0"}, IdempotencyMarker: "idem"}}
	return ExecutionPlan{SchemaVersion: SchemaVersion, ID: planID, Revision: 1, OwnerID: ownerID, ProjectID: projectID, AnalysisID: analysisID, Purpose: PurposeJob, Placement: placement(), Targets: []ExecutionTarget{t}, Artifacts: []ArtifactRef{artifact()}, Stages: []ExecutionStage{{StageKey: "deploy", Revision: 1, Kind: "deploy", Risk: RiskR2, Gate: GateRemotePrivilegedExecution, Effects: []Gate{GateRemotePrivilegedExecution}, TargetID: targetID, TargetRevision: 1, TargetDigest: t.Digest, Steps: []ExecutionStep{s}, TimeoutSeconds: 60}}, Status: PlanReady, CreatedAt: created, ExpiresAt: created.Add(time.Hour)}
}

func addSecretProvisionStage(p *ExecutionPlan, ref CredentialRef) {
	t := p.Targets[0]
	p.Stages[0].DependsOn = append(p.Stages[0].DependsOn, "authorize-secret")
	p.Stages = append([]ExecutionStage{{
		StageKey: "authorize-secret", Revision: 1, Kind: string(StepSecretProvision), Risk: RiskR2, Gate: GateSecretAccess,
		Effects: []Gate{GateSecretAccess}, TargetID: t.ID, TargetRevision: t.Revision, TargetDigest: t.Digest,
		Steps: []ExecutionStep{{
			StepKey: "provision-secret", Kind: StepSecretProvision, TargetID: t.ID, TargetRevision: t.Revision, TargetDigest: t.Digest,
			SecretRefs: []CredentialRef{ref}, TimeoutSeconds: 30, IdempotencyMarker: "provision-secret", OutputPolicy: OutputDiscard,
			SecretProvision: &SecretProvisionStep{Delivery: "target_secure_parameter"},
		}}, TimeoutSeconds: 60,
	}}, p.Stages...)
}

func TestCanonicalDeterminismAndArgvOrder(t *testing.T) {
	a := plan()
	b := plan()
	na, e := a.Normalize()
	if e != nil {
		t.Fatal(e)
	}
	nb, e := b.Normalize()
	if e != nil {
		t.Fatal(e)
	}
	if na.Digest != nb.Digest {
		t.Fatal("determinism")
	}
	b.Stages[0].Steps[0].ScriptRun.Argv = []string{"-x"}
	nc, e := b.Normalize()
	if e != nil {
		t.Fatal(e)
	}
	if na.Digest == nc.Digest {
		t.Fatal("argv did not affect digest")
	}
	b = plan()
	b.Stages[0].Steps[0].ObservationRef.ObservationDigest = sha
	nc, e = b.Normalize()
	if e != nil {
		t.Fatal(e)
	}
	if na.Digest == nc.Digest {
		t.Fatal("observation reference did not affect digest")
	}
	b = plan()
	b.Status = PlanExpired
	expired, e := b.Normalize()
	if e != nil {
		t.Fatal(e)
	}
	if expired.Digest != na.Digest {
		t.Fatal("lifecycle status changed immutable digest")
	}
}

func TestValidationBoundaries(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ExecutionPlan)
	}{
		{"wrong risk gate", func(p *ExecutionPlan) { p.Stages[0].Risk = RiskR1; p.Stages[0].Gate = GateRemoteExecution }},
		{"missing artifact closure", func(p *ExecutionPlan) { p.Artifacts = nil }},
		{"invalid env name", func(p *ExecutionPlan) { p.Stages[0].Steps[0].ScriptRun.Env = map[string]string{"bad-name": "x"} }},
		{"secret env name", func(p *ExecutionPlan) {
			p.Stages[0].Steps[0].ScriptRun.Env = map[string]string{"API_TOKEN": "not-allowed"}
		}},
		{"bearer env value", func(p *ExecutionPlan) {
			p.Stages[0].Steps[0].ScriptRun.Env = map[string]string{"MODE": "Bearer credential-value"}
		}},
		{"untruthful non-root SSM script", func(p *ExecutionPlan) { p.Stages[0].Steps[0].ScriptRun.Root = false }},
		{"missing observation reference", func(p *ExecutionPlan) { p.Stages[0].Steps[0].ObservationRef = nil }},
		{"mismatched observation target", func(p *ExecutionPlan) { p.Stages[0].Steps[0].ObservationRef.TargetID = artifactID }},
		{"mismatched observation revision", func(p *ExecutionPlan) { p.Stages[0].Steps[0].ObservationRef.TargetRevision = 2 }},
		{"invalid observation id", func(p *ExecutionPlan) { p.Stages[0].Steps[0].ObservationRef.ObservationID = "not-a-uuid" }},
		{"invalid observation digest", func(p *ExecutionPlan) { p.Stages[0].Steps[0].ObservationRef.ObservationDigest = "not-a-digest" }},
		{"duplicate stage", func(p *ExecutionPlan) { p.Stages = append(p.Stages, p.Stages[0]) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := plan()
			tc.mutate(&p)
			if e := p.Validate(); e == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestDAGAndHistoricalExpiry(t *testing.T) {
	p := plan()
	p.Stages = append(p.Stages, ExecutionStage{StageKey: "second", Revision: 1, Kind: "x", Risk: RiskR2, Gate: GateRemoteExecution, DependsOn: []string{"deploy"}, TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest, Steps: []ExecutionStep{{StepKey: "next", Kind: StepCleanup, TimeoutSeconds: 1, IdempotencyMarker: "x", Cleanup: &CleanupStep{Resource: "resource"}}}, TimeoutSeconds: 1})
	p.Stages[0].DependsOn = []string{"second"}
	if e := p.Validate(); e != ErrCycle {
		t.Fatalf("cycle: %v", e)
	}
	p = plan()
	p.CreatedAt = time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC)
	p.ExpiresAt = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	if e := p.Validate(); e != nil {
		t.Fatalf("historical structural validate: %v", e)
	}
	if e := p.ValidateAt(time.Now().UTC()); e != ErrExpired {
		t.Fatalf("expiry: %v", e)
	}
}

func TestSuppliedDigestMismatchAndStatuses(t *testing.T) {
	p := plan()
	p.Digest = sha
	if e := p.Validate(); e != ErrDigestMismatch {
		t.Fatalf("digest mismatch: %v", e)
	}
	if ValidatePlanStatus(PlanReady) == false || ValidateRunStatus(RunUncertain) == false || ValidateStageStatus(StageRejected) == false {
		t.Fatal("status validators")
	}
}

func TestGenericContainerRunPolicyIsInitialDeployOnly(t *testing.T) {
	p := plan()
	p.Recipes = []RecipeRef{{ID: RecipeGenericContainerService, Version: "1.0.0", Digest: sha}}
	for _, operation := range []RunOperation{RunOperationExecute, RunOperationDeploy} {
		if !RunOperationAllowedByPlan(p, operation) {
			t.Fatalf("initial operation %q rejected", operation)
		}
	}
	for _, operation := range []RunOperation{RunOperationUpgrade, RunOperationRepair, RunOperationDestroy, RunOperationRollback} {
		if RunOperationAllowedByPlan(p, operation) {
			t.Fatalf("unsafe operation %q accepted", operation)
		}
	}
}

func TestEveryStepKindPayloadValidation(t *testing.T) {
	base := plan()
	cases := []struct {
		k       StepKind
		payload func() *ExecutionStep
	}{
		{StepTargetInspect, func() *ExecutionStep {
			return &ExecutionStep{TargetInspect: &TargetInspectStep{ObservationID: targetID}}
		}},
		{StepComputeProvision, func() *ExecutionStep {
			return &ExecutionStep{ComputeProvision: &ComputeProvisionStep{InfrastructureProfileID: "general-linux-ssm-v1", AMIParameter: AWSAL2023X8664AMIParameter, InstanceType: "t3.small", AvailabilityZone: "us-east-1a", VolumeGiB: 20, Region: "us-east-1", Architecture: "x86_64", ManagementTransport: "aws_ssm", PublicIP: true}}
		}},
		{StepComputeDestroy, func() *ExecutionStep {
			return &ExecutionStep{ComputeDestroy: &ComputeDestroyStep{ResourceID: targetID}}
		}},
		{StepSourceFetch, func() *ExecutionStep {
			return &ExecutionStep{SourceFetch: &SourceFetchStep{Source: SourceRef{Kind: "git_https", Location: "https://example.org/r", Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true}, Artifact: artifact()}}
		}},
		{StepArtifactUpload, func() *ExecutionStep {
			return &ExecutionStep{ArtifactUpload: &ArtifactUploadStep{Artifact: artifact(), Destination: "/upload"}}
		}},
		{StepPackageEnsure, func() *ExecutionStep {
			return &ExecutionStep{PackageEnsure: &PackageEnsureStep{Name: "nginx", Version: "1.0", Manager: "dnf", PlatformProfile: "amazon-linux-2023"}}
		}},
		{StepFilePut, func() *ExecutionStep {
			return &ExecutionStep{FilePut: &FilePutStep{Path: "/etc/app.conf", Artifact: artifact()}}
		}},
		{StepContainerApply, func() *ExecutionStep {
			return &ExecutionStep{ContainerApply: &ContainerApplyStep{Image: "repo.example/app@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Name: "app", HostAddress: "127.0.0.1", HostPort: 8080, ContainerPort: 8080, RestartPolicy: "unless-stopped"}}
		}},
		{StepSystemdApply, func() *ExecutionStep { return &ExecutionStep{SystemdApply: &SystemdApplyStep{Unit: "app.service"}} }},
		{StepScriptRun, func() *ExecutionStep { return base.Stages[0].Steps[0].ScriptRunStep() }},
		{StepHTTPProbe, func() *ExecutionStep {
			return &ExecutionStep{HTTPProbe: &HTTPProbeStep{URL: "https://example.org/health", Mode: "external"}}
		}},
		{StepTCPProbe, func() *ExecutionStep {
			return &ExecutionStep{TCPProbe: &TCPProbeStep{Address: "example.org", Port: 443, Mode: "external"}}
		}},
		{StepArtifactCollect, func() *ExecutionStep {
			return &ExecutionStep{ArtifactCollect: &ArtifactCollectStep{Paths: []string{"/tmp/log"}, OutputKey: "logs"}}
		}},
		{StepCleanup, func() *ExecutionStep { return &ExecutionStep{Cleanup: &CleanupStep{Resource: "old-resource"}} }},
	}
	for i, tc := range cases {
		p := plan()
		s := tc.payload()
		s.StepKey = "step-" + string(rune('a'+i))
		s.Kind = tc.k
		s.TimeoutSeconds = 1
		s.IdempotencyMarker = "idempotent"
		switch tc.k {
		case StepSourceFetch:
			s.NetworkGrants = []NetworkGrant{{Scheme: "https", Host: "example.org", Port: 443, PathPrefix: "/r", Scope: "external"}}
		case StepHTTPProbe:
			s.NetworkGrants = []NetworkGrant{{Scheme: "https", Host: "example.org", Port: 443, PathPrefix: "/health", Scope: "external"}}
		case StepTCPProbe:
			s.NetworkGrants = []NetworkGrant{{Scheme: "tcp", Host: "example.org", Port: 443, Scope: "external"}}
		case StepContainerApply:
			s.NetworkGrants = []NetworkGrant{{Scheme: "https", Host: "repo.example", Port: 443, PathPrefix: "/app", Scope: "external"}}
		case StepArtifactCollect:
			p.Outputs = []OutputDeclaration{{Key: "logs", MediaType: "text/plain", MaxSize: 1024}}
		}
		p.Targets[0].Network.Allow = append([]NetworkGrant(nil), s.NetworkGrants...)
		p.Targets[0].Digest = ""
		if s.ScriptRun != nil {
			s.ScriptRun.IdempotencyMarker = s.IdempotencyMarker
			s.ScriptRun.TimeoutSeconds = s.TimeoutSeconds
			s.OutputPolicy = OutputCapture
			s.Postcondition = &Postcondition{Type: "exit_code", Value: "0"}
			s.ScriptRun.Postcondition = s.Postcondition
		}
		s.TargetID = targetID
		s.TargetRevision = 1
		s.TargetDigest = p.Targets[0].Digest
		switch tc.k {
		case StepTargetInspect, StepHTTPProbe, StepTCPProbe:
			p.Stages[0].Risk = RiskR0
			p.Stages[0].Gate = GateNone
		case StepComputeProvision:
			p.Stages[0].Risk = RiskR2
			p.Stages[0].Gate = GateResourcePurchase
		case StepComputeDestroy:
			p.Stages[0].Risk = RiskR4
			p.Stages[0].Gate = GateServiceDestroy
		case StepScriptRun:
			p.Stages[0].Risk = RiskR2
			p.Stages[0].Gate = GateRemotePrivilegedExecution
			p.Stages[0].Effects = []Gate{GateRemotePrivilegedExecution}
		default:
			p.Stages[0].Risk = RiskR2
			p.Stages[0].Gate = GateRemoteExecution
		}
		p.Stages[0].Steps = []ExecutionStep{*s}
		if e := p.Validate(); e != nil {
			t.Errorf("%s: %v", tc.k, e)
		}
	}
}

func (s *ExecutionStep) ScriptRunStep() *ExecutionStep {
	return &ExecutionStep{ScriptRun: s.ScriptRun, ObservationRef: s.ObservationRef}
}

func TestOwnerOptionalUUIDAndSnapshotFields(t *testing.T) {
	p := plan()
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	p.DeploymentID = "11111111-1111-4111-8111-111111111111"
	if e := p.Validate(); e != nil {
		t.Fatal(e)
	}
	b := ConfirmationBindingSnapshot{OwnerID: ownerID, PlanID: planID, PlanRevision: 1, PlanDigest: sha, DeploymentID: p.DeploymentID, RunID: "22222222-2222-4222-8222-222222222222", RunRevision: 1, StageID: "33333333-3333-4333-8333-333333333333", StageRevision: 1, StageIdempotencyKey: "stage-idem", StageDigest: sha, TargetID: targetID, TargetRevision: 1, TargetDigest: sha, ExecutionDigest: sha, ArtifactSetDigest: sha, NetworkDigest: sha, SecretGrantDigest: sha, PolicyDigest: sha, CostQuoteDigest: sha, RollbackDigest: sha, PreviewDigest: sha, Risk: RiskR2, Gate: GateRemoteExecution, ExpiresAt: time.Date(2030, 1, 1, 1, 0, 0, 0, time.UTC)}
	if e := b.Validate(); e != nil {
		t.Fatal(e)
	}
}

func TestObservedHTTPSEgressEnvelopeRequiresHonestWildcardStepGrant(t *testing.T) {
	https, _ := (NetworkGrant{Scheme: "https", Host: PublicHTTPSWildcardHost, Port: 443, Scope: "external"}).Normalize()
	claimedExact, _ := (NetworkGrant{Scheme: "https", Host: "registry.example", Port: 443, PathPrefix: "/repo", Scope: "external"}).Normalize()
	local, _ := (NetworkGrant{Scheme: "http", Host: "127.0.0.1", Port: 8080, PathPrefix: "/health", Scope: "target_local"}).Normalize()
	http, _ := (NetworkGrant{Scheme: "http", Host: "registry.example", Port: 80, PathPrefix: "/repo", Scope: "external"}).Normalize()
	policy := NetworkPolicy{Mode: NetworkPolicyModeObservedHTTPSEgress}
	if err := validateTargetNetworkClosure([]NetworkGrant{https}, policy); err != nil {
		t.Fatalf("public HTTPS wildcard grant rejected: %v", err)
	}
	if err := validateTargetNetworkClosure([]NetworkGrant{claimedExact}, policy); err == nil {
		t.Fatal("hostname/path claim was accepted for a destination-independent security group")
	}
	if err := validateTargetNetworkClosure([]NetworkGrant{local}, policy); err != nil {
		t.Fatalf("target-local grant rejected: %v", err)
	}
	if err := validateTargetNetworkClosure([]NetworkGrant{http}, policy); err == nil {
		t.Fatal("external HTTP escaped the HTTPS-only envelope")
	}
	if err := validateTargetNetworkClosure([]NetworkGrant{https}, NetworkPolicy{Mode: "restricted"}); err == nil {
		t.Fatal("unobserved target accepted public HTTPS")
	}
	policy.Deny = []NetworkGrant{https}
	if err := validateTargetNetworkClosure([]NetworkGrant{https}, policy); err == nil {
		t.Fatal("explicit target denial did not win")
	}
}

func TestPublicHTTPSWildcardGrantIsNarrowlyTyped(t *testing.T) {
	for _, grant := range []NetworkGrant{
		{Scheme: "http", Host: PublicHTTPSWildcardHost, Port: 80, Scope: "external"},
		{Scheme: "https", Host: PublicHTTPSWildcardHost, Port: 443, PathPrefix: "/repo", Scope: "external"},
		{Scheme: "https", Host: PublicHTTPSWildcardHost, Port: 443, Scope: "target_local"},
	} {
		if _, err := grant.Normalize(); err == nil {
			t.Fatalf("invalid wildcard grant accepted: %+v", grant)
		}
	}
}

func TestMissingDependencyRollbackClosureAndDeepClone(t *testing.T) {
	p := plan()
	p.Stages[0].Steps[0].DependsOn = []string{"missing"}
	if e := p.Validate(); e == nil {
		t.Fatal("missing dependency accepted")
	}
	p = plan()
	rb := p.Stages[0].Steps[0]
	rb.StepKey = "rollback"
	rb.Digest = ""
	rb.ScriptRun.Artifact.ID = "44444444-4444-4444-8444-444444444444"
	p.Stages[0].RollbackSteps = []ExecutionStep{rb}
	if e := p.Validate(); e == nil {
		t.Fatal("rollback artifact closure accepted")
	}
	p = plan()
	p.Stages[0].Steps[0].ScriptRun.Env = map[string]string{"MODE": "prod"}
	n, e := p.Normalize()
	if e != nil {
		t.Fatal(e)
	}
	n.Stages[0].Steps[0].ScriptRun.Argv[0] = "changed"
	n.Stages[0].Steps[0].ScriptRun.Env["MODE"] = "changed"
	n.Stages[0].Steps[0].ObservationRef.ObservationID = artifactID
	if p.Stages[0].Steps[0].ScriptRun.Argv[0] == "changed" || p.Stages[0].Steps[0].ScriptRun.Env["MODE"] == "changed" || p.Stages[0].Steps[0].ObservationRef.ObservationID == artifactID {
		t.Fatal("normalize mutated caller")
	}
}

func TestRunRecordStatusInvariants(t *testing.T) {
	if !RunWaitingUser.CanTransition(RunQueued) || RunSucceeded.CanTransition(RunRunning) || RunUncertain.CanTransition(RunSucceeded) || !StageQueued.CanTransition(StageRunning) || StageFailed.CanTransition(StageQueued) || StageUncertain.CanTransition(StageSucceeded) || !PlanDraft.CanTransition(PlanReady) {
		t.Fatal("lifecycle transition bounds")
	}
	now := time.Date(2030, 1, 1, 2, 0, 0, 0, time.UTC)
	r := ExecutionRun{RunID: "22222222-2222-4222-8222-222222222222", OwnerID: ownerID, Operation: RunOperationExecute, ProjectID: projectID, Purpose: PurposeJob, PlanID: planID, PlanRevision: 1, PlanDigest: sha, RunDigest: sha, Revision: 1, Status: RunSucceeded, StartedAt: now.Add(-time.Minute), FinishedAt: now, CreatedAt: now.Add(-2 * time.Minute), UpdatedAt: now}
	if e := r.Validate(); e != nil {
		t.Fatal(e)
	}
	r.FinishedAt = time.Time{}
	if e := r.Validate(); e == nil {
		t.Fatal("terminal run without finish accepted")
	}
	s := RunStage{StageID: "33333333-3333-4333-8333-333333333333", RunID: r.RunID, OwnerID: ownerID, PlanID: planID, StageKey: "deploy", PlanRevision: 1, StageRevision: 1, StageDigest: sha, TargetID: targetID, TargetRevision: 1, TargetDigest: sha, Ordinal: 1, StageIdempotencyKey: "idem", Status: StageRunning, CreatedAt: now.Add(-time.Minute), StartedAt: now.Add(-30 * time.Second), UpdatedAt: now}
	if e := s.Validate(); e != nil {
		t.Fatal(e)
	}
}

func TestLifecycleTimestampStatusMatrix(t *testing.T) {
	for _, tc := range []struct {
		status  RunStatus
		started bool
	}{
		{RunPending, false}, {RunWaitingUser, false}, {RunQueued, false}, {RunRunning, true}, {RunSucceeded, true}, {RunFailed, true}, {RunUncertain, true}, {RunCanceled, false}, {RunRejected, false}, {RunExpired, false},
	} {
		if got := requiresStartedRun(tc.status); got != tc.started {
			t.Fatalf("run %s started=%v", tc.status, got)
		}
	}
	for _, tc := range []struct {
		status  StageStatus
		started bool
	}{{StageSucceeded, true}, {StageFailed, true}, {StageUncertain, true}, {StageSkipped, false}, {StageCanceled, false}, {StageRejected, false}, {StageExpired, false}} {
		if got := stageRequiresStarted(tc.status); got != tc.started {
			t.Fatalf("stage %s started=%v", tc.status, got)
		}
	}
	for _, tc := range []struct {
		status  AttemptStatus
		started bool
	}{{AttemptSucceeded, true}, {AttemptFailed, true}, {AttemptUncertain, true}, {AttemptCanceled, false}} {
		if got := attemptRequiresStarted(tc.status); got != tc.started {
			t.Fatalf("attempt %s started=%v", tc.status, got)
		}
	}
}

func TestLifecycleChronologyValidatesConcreteRecords(t *testing.T) {
	now := time.Date(2030, 1, 1, 2, 0, 0, 0, time.UTC)
	run := func(status RunStatus, started, finished time.Time) ExecutionRun {
		return ExecutionRun{RunID: "22222222-2222-4222-8222-222222222222", OwnerID: ownerID, Operation: RunOperationExecute, ProjectID: projectID, Purpose: PurposeJob, PlanID: planID, PlanRevision: 1, PlanDigest: sha, RunDigest: sha, Revision: 1, Status: status, CreatedAt: now.Add(-time.Minute), StartedAt: started, FinishedAt: finished, UpdatedAt: now}
	}
	stage := func(status StageStatus, started, finished time.Time) RunStage {
		return RunStage{StageID: "33333333-3333-4333-8333-333333333333", RunID: "22222222-2222-4222-8222-222222222222", OwnerID: ownerID, PlanID: planID, StageKey: "deploy", PlanRevision: 1, StageRevision: 1, StageDigest: sha, TargetID: targetID, TargetRevision: 1, TargetDigest: sha, Ordinal: 1, StageIdempotencyKey: "idem", Status: status, CreatedAt: now.Add(-time.Minute), StartedAt: started, FinishedAt: finished, UpdatedAt: now}
	}
	attempt := func(status AttemptStatus, started, finished time.Time) StepAttempt {
		return StepAttempt{AttemptID: "44444444-4444-4444-8444-444444444444", RunID: "22222222-2222-4222-8222-222222222222", StageID: "33333333-3333-4333-8333-333333333333", OwnerID: ownerID, PlanID: planID, PlanRevision: 1, PlanDigest: sha, StageRevision: 1, StageDigest: sha, StepRevision: 1, StepDigest: sha, StepKey: "step", Attempt: 1, Revision: 1, Status: status, Uncertain: status == AttemptUncertain, CreatedAt: now.Add(-time.Minute), StartedAt: started, FinishedAt: finished, UpdatedAt: now}
	}
	started := now.Add(-30 * time.Second)
	for _, tc := range []struct {
		name  string
		valid func() error
		want  bool
	}{
		{"running", run(RunRunning, started, time.Time{}).Validate, true}, {"canceled-prestart", run(RunCanceled, time.Time{}, time.Time{}).Validate, true}, {"rejected-prestart", run(RunRejected, time.Time{}, time.Time{}).Validate, true}, {"expired-prestart", run(RunExpired, time.Time{}, time.Time{}).Validate, true}, {"terminal-missing-finish", run(RunSucceeded, started, time.Time{}).Validate, false}, {"finish-before-start", run(RunSucceeded, started, started.Add(-time.Second)).Validate, false}, {"updated-before-finish", run(RunSucceeded, started, now.Add(time.Second)).Validate, false},
		{"stage-skipped-prestart", stage(StageSkipped, time.Time{}, time.Time{}).Validate, true}, {"stage-canceled-prestart", stage(StageCanceled, time.Time{}, time.Time{}).Validate, true}, {"stage-started-terminal-missing-finish", stage(StageFailed, started, time.Time{}).Validate, false},
		{"attempt-canceled-prestart", attempt(AttemptCanceled, time.Time{}, time.Time{}).Validate, true}, {"attempt-started-terminal-missing-finish", attempt(AttemptFailed, started, time.Time{}).Validate, false}, {"attempt-finish-before-created", attempt(AttemptSucceeded, started, now.Add(-2*time.Minute)).Validate, false},
	} {
		if got := tc.valid() == nil; got != tc.want {
			t.Errorf("%s valid=%v", tc.name, got)
		}
	}
}

func TestAnalysisNormalizeSealsDigest(t *testing.T) {
	a := ProjectAnalysis{AnalysisID: analysisID, ProjectID: projectID, Source: SourceRef{Kind: "git_https", Location: "https://example.org/r", Commit: "0123456789abcdef0123456789abcdef01234567", Immutable: true}, Runtime: ResourceRequirement{Architecture: "x86_64"}, Revision: 1, CreatedAt: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC), UpdatedAt: time.Date(2030, 1, 1, 1, 0, 0, 0, time.UTC)}
	n, e := a.Normalize()
	if e != nil {
		t.Fatal(e)
	}
	if !n.Digest.Valid() {
		t.Fatal("digest not sealed")
	}
	if _, e := n.Normalize(); e != nil {
		t.Fatal(e)
	}
	a.Runtime.Architecture = "amd64"
	if _, e := a.Normalize(); e == nil {
		t.Fatal("non-canonical analysis architecture was accepted")
	}
}

func TestNormalizeDeepClonesGenericAliases(t *testing.T) {
	p := plan()
	step := &p.Stages[0].Steps[0]
	step.ArtifactRefs = []ArtifactRef{artifact()}
	step.NetworkGrants = []NetworkGrant{grant("internal.example.org")}
	step.SecretRefs = []CredentialRef{{Ref: "44444444-4444-4444-8444-444444444444", Purpose: "runtime", Revision: 1, BindingDigest: sha}}
	step.Permissions = []PermissionGrant{{Name: "perm", Revision: 1, BindingDigest: sha}}
	step.ScriptRun.SecretRefs = append([]CredentialRef(nil), step.SecretRefs...)
	step.ScriptRun.NetworkGrants = append([]NetworkGrant(nil), step.NetworkGrants...)
	step.ScriptRun.IdempotencyMarker = step.IdempotencyMarker
	step.ScriptRun.TimeoutSeconds = step.TimeoutSeconds
	p.Stages[0].Gate = GateRemotePrivilegedExecution
	p.Stages[0].Effects = []Gate{GateRemotePrivilegedExecution, GateSecretAccess}
	addSecretProvisionStage(&p, step.SecretRefs[0])
	p.Targets[0].Capabilities = []string{"cap-a"}
	p.Targets[0].Network.Allow = append([]NetworkGrant(nil), step.NetworkGrants...)
	p.Targets[0].Digest = ""
	updatedTarget, err := p.Targets[0].Normalize()
	if err != nil {
		t.Fatal(err)
	}
	p.Targets[0] = updatedTarget
	p.Stages[0].TargetDigest = updatedTarget.Digest
	p.Skills = []SkillRef{{ID: "skill", Version: "1.0.0", Digest: sha}}
	p.Recipes = []RecipeRef{{ID: "recipe", Version: "1.0.0", Digest: sha}}
	n, e := p.Normalize()
	if e != nil {
		t.Fatal(e)
	}
	n.Stages[1].Steps[0].ArtifactRefs[0].ID = artifactID
	n.Stages[1].Steps[0].NetworkGrants[0].Host = "changed.example.org"
	n.Stages[1].Steps[0].SecretRefs[0].Ref = "55555555-5555-4555-8555-555555555555"
	n.Stages[1].Steps[0].Permissions[0].Name = "changed"
	n.Targets[0].Capabilities[0] = "changed"
	n.Skills[0].ID = "changed"
	n.Recipes[0].ID = "changed"
	if p.Stages[1].Steps[0].NetworkGrants[0].Host == "changed.example.org" || p.Targets[0].Capabilities[0] == "changed" || p.Skills[0].ID == "changed" || p.Recipes[0].ID == "changed" {
		t.Fatal("caller alias mutated")
	}
}

func TestExecutionV2EffectsRollbackOutputsAndNetworkClosure(t *testing.T) {
	t.Run("root and secret effects are jointly represented", func(t *testing.T) {
		p := plan()
		step := &p.Stages[0].Steps[0]
		step.SecretRefs = []CredentialRef{{Ref: "44444444-4444-4444-8444-444444444444", Purpose: "runtime", Revision: 1, BindingDigest: sha}}
		step.ScriptRun.SecretRefs = append([]CredentialRef(nil), step.SecretRefs...)
		step.ScriptRun.Root = true
		p.Stages[0].Gate = GateRemotePrivilegedExecution
		p.Stages[0].Effects = []Gate{GateRemotePrivilegedExecution, GateSecretAccess}
		addSecretProvisionStage(&p, step.SecretRefs[0])
		if _, err := p.Normalize(); err != nil {
			t.Fatal(err)
		}
		p.Stages[1].Effects = []Gate{GateRemotePrivilegedExecution}
		if err := p.Validate(); err == nil {
			t.Fatal("secret effect omitted from root stage")
		}
	})
	t.Run("remote secret use requires one distinct authorization stage", func(t *testing.T) {
		p := plan()
		ref := CredentialRef{Ref: "44444444-4444-4444-8444-444444444444", Purpose: "runtime", Revision: 1, BindingDigest: sha}
		p.Stages[0].Steps[0].SecretRefs = []CredentialRef{ref}
		p.Stages[0].Steps[0].ScriptRun.SecretRefs = []CredentialRef{ref}
		p.Stages[0].Effects = []Gate{GateRemotePrivilegedExecution, GateSecretAccess}
		if err := p.Validate(); err == nil {
			t.Fatal("remote secret use without a distinct authorization stage was accepted")
		}
		addSecretProvisionStage(&p, ref)
		if _, err := p.Normalize(); err != nil {
			t.Fatalf("authorized remote secret use rejected: %v", err)
		}
		p.Stages[1].DependsOn = nil
		if err := p.Validate(); err == nil {
			t.Fatal("remote secret use without direct authorization dependency was accepted")
		}
	})
	t.Run("rollback is inert and independently R4 bound", func(t *testing.T) {
		p := plan()
		p.Stages[0].RollbackSteps = []ExecutionStep{{StepKey: "rollback-cleanup", Kind: StepCleanup, TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest, TimeoutSeconds: 1, IdempotencyMarker: "rollback", Cleanup: &CleanupStep{Resource: "service"}}}
		p.Stages[0].RollbackPolicy = &RollbackPolicy{Risk: RiskR4, Gate: GateRollback}
		if _, err := p.Normalize(); err != nil {
			t.Fatal(err)
		}
		p.Stages[0].RollbackPolicy = nil
		if err := p.Validate(); err == nil {
			t.Fatal("rollback declaration without R4 policy accepted")
		}
	})
	t.Run("required output has exactly one bounded producer", func(t *testing.T) {
		p := plan()
		p.Outputs = []OutputDeclaration{{Key: "logs", MediaType: "text/plain", MaxSize: 1024, Required: true}}
		p.Stages[0].Steps = append(p.Stages[0].Steps, ExecutionStep{StepKey: "collect-logs", Kind: StepArtifactCollect, TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest, TimeoutSeconds: 1, IdempotencyMarker: "collect", ArtifactCollect: &ArtifactCollectStep{Paths: []string{"/tmp/log"}, OutputKey: "logs"}})
		if _, err := p.Normalize(); err != nil {
			t.Fatal(err)
		}
		p.Stages[0].Steps = append(p.Stages[0].Steps, p.Stages[0].Steps[1])
		p.Stages[0].Steps[2].StepKey = "collect-logs-again"
		if err := p.Validate(); err == nil {
			t.Fatal("multiple required-output producers accepted")
		}
	})
	t.Run("network grants close over target policy", func(t *testing.T) {
		p := plan()
		grant := grant("registry.example")
		p.Stages[0].Steps[0].NetworkGrants = []NetworkGrant{grant}
		p.Stages[0].Steps[0].ScriptRun.NetworkGrants = []NetworkGrant{grant}
		p.Targets[0].Network.Allow = []NetworkGrant{grant}
		p.Targets[0].Digest = ""
		if _, err := p.Normalize(); err != nil {
			t.Fatal(err)
		}
		p.Targets[0].Network.Allow = nil
		p.Targets[0].Digest = ""
		if err := p.Validate(); err == nil {
			t.Fatal("grant outside target policy accepted")
		}
	})
	t.Run("typed outbound operation rejects unrelated grant", func(t *testing.T) {
		p := plan()
		grant := NetworkGrant{Scheme: "https", Host: "source.example", Port: 443, PathPrefix: "/repository", Scope: "external"}
		p.Stages[0].Steps = []ExecutionStep{{StepKey: "fetch", Kind: StepSourceFetch, TargetID: targetID, TargetRevision: 1, TargetDigest: p.Targets[0].Digest, TimeoutSeconds: 1, IdempotencyMarker: "fetch", NetworkGrants: []NetworkGrant{grant}, SourceFetch: &SourceFetchStep{Source: SourceRef{Kind: "git_https", Location: "https://source.example/other", Commit: "0123456789012345678901234567890123456789", Immutable: true}, Artifact: artifact()}}}
		p.Targets[0].Network.Allow = []NetworkGrant{grant}
		p.Targets[0].Digest = ""
		if err := p.Validate(); err == nil {
			t.Fatal("unrelated concrete source grant accepted")
		}
	})
}
