package agentcore

import (
	"encoding/json"
	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"math"
	"strings"
	"testing"
	"time"
)

func TestStrictWorkloadDTORejectsUnknownAndPreservesTypedFields(t *testing.T) {
	m := map[string]any{"identity": map[string]any{"kind": "aws-ecs", "aws_region": "us-east-1", "aws_ecs_subnet_ids": []any{"subnet-a"}, "aws_ecs_security_group_ids": []any{"sg-a"}, "aws_ecs_assign_public_ip": true, "aws_ecs_desired_count": 2, "aws_ecs_image_uri": "img"}, "labels": map[string]any{"env": "test"}, "ports": []any{map[string]any{"port": 443}}}
	v, e := targetSettingsParam(m)
	if e != nil || v.GetIdentity().GetAwsRegion() != "us-east-1" || len(v.GetIdentity().GetAwsEcsSubnetIds()) != 1 || !v.GetIdentity().GetAwsEcsAssignPublicIp() {
		t.Fatalf("dto=%v err=%v", v, e)
	}
	m["unknown"] = true
	if _, e = targetSettingsParam(m); e == nil {
		t.Fatal("unknown field accepted")
	}
}

func TestCentralValidatorsGoodAndBad(t *testing.T) {
	now := timestamppb.New(time.Now().UTC())
	later := timestamppb.New(time.Now().UTC().Add(time.Minute))
	u := "00000000-0000-0000-0000-000000000001"
	c := &agentv1.CoreAWSCredential{CredentialId: u, Name: "n", Region: "r", Revision: 2, CreatedAt: now, UpdatedAt: later}
	if validateCredentialResponse(c, u, 1) != nil {
		t.Fatal("credential rejected")
	}
	c.Revision = 1
	if validateCredentialResponse(c, u, 1) == nil {
		t.Fatal("revision accepted")
	}
	p := &agentv1.CoreAWSPlan{PlanId: u, CredentialId: u, Region: "r", StackName: "s", Operation: agentv1.CoreAWSOperation_CORE_AWS_OPERATION_CREATE, Revision: 1, TemplateSha256: strings.Repeat("a", 64), CreatedAt: now}
	if validateAWSPlanResponse(p, u) != nil {
		t.Fatal("plan rejected")
	}
	p.Operation = 0
	if validateAWSPlanResponse(p, u) == nil {
		t.Fatal("enum accepted")
	}
}
func TestWorkloadPlanValidatorRejectsDigestAndKind(t *testing.T) {
	now := timestamppb.New(time.Now().UTC())
	later := timestamppb.New(time.Now().UTC().Add(time.Minute))
	u := "00000000-0000-0000-0000-000000000001"
	id := &agentv1.CoreWorkloadTargetIdentity{Kind: agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER}
	p := &agentv1.CoreWorkloadPlan{PlanId: u, Revision: 1, Digest: strings.Repeat("a", 64), TargetKind: id.GetKind(), TypedTarget: &agentv1.CoreWorkloadTargetSettings{Identity: id}, CreatedAt: now, ExpiresAt: later}
	if validateWorkloadPlan(p, u) != nil {
		t.Fatal("workload plan rejected")
	}
	p.Digest = "bad"
	if validateWorkloadPlan(p, u) == nil {
		t.Fatal("bad digest accepted")
	}
}
func TestStrictDTORejectsWrongNestedTypes(t *testing.T) {
	cases := []map[string]any{{"identity": map[string]any{"kind": "aws-ec2-ssm", "aws_ecs_assign_public_ip": "yes"}}, {"identity": map[string]any{"kind": "aws-ecs", "aws_ecs_subnet_ids": "subnet"}}, {"identity": map[string]any{"kind": "aws-ecs"}, "network_grants": map[string]any{}}, {"identity": map[string]any{"kind": "aws-ecs"}, "labels": map[string]any{"x": 1}}}
	for _, m := range cases {
		if _, e := targetSettingsParam(m); e == nil {
			t.Fatalf("accepted %#v", m)
		}
	}
}
func TestMutationValidatorRejectsCorruptLinks(t *testing.T) {
	u := "00000000-0000-0000-0000-000000000001"
	dig := strings.Repeat("a", 64)
	now := timestamppb.New(time.Now().UTC())
	later := timestamppb.New(time.Now().UTC().Add(time.Minute))
	op := &agentv1.CoreWorkloadOperation{OperationId: u, WorkloadId: u, PlanId: u, Kind: agentv1.CoreWorkloadOperationKind_CORE_WORKLOAD_OPERATION_KIND_APPLY, PlanRevision: 1, PlanDigest: dig, TaskId: u, ConfirmationId: u, Revision: 1, DesiredPlan: &agentv1.CoreWorkloadOperationPlan{PlanId: u, PlanRevision: 1, PlanDigest: dig}, CreatedAt: now, UpdatedAt: later}
	conf := &agentv1.CoreConfirmation{ConfirmationId: u, TaskId: u, State: agentv1.CoreConfirmationState_CORE_CONFIRMATION_STATE_PENDING, Revision: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: timestamppb.New(time.Now().UTC().Add(time.Hour)), Binding: &agentv1.CoreConfirmationBinding{OperationDomain: "workload:apply", TargetId: u, TargetRevision: 1, SourceVersion: "v1", ContentDigest: dig, ParameterDigest: dig, NetworkDigest: dig, SecretGrantDigest: dig}}
	op.DesiredPlan.Target = &agentv1.CoreWorkloadTargetSettings{Identity: &agentv1.CoreWorkloadTargetIdentity{Kind: agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER}}
	op.TargetKind = agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER
	if validateWorkloadMutationResponse(op, conf, u, u, "bad", false) == nil {
		t.Fatal("expected mutation rejection")
	}
}
func TestMutationEmptyRequestWidAndQuoteFinite(t *testing.T) {
	u := "00000000-0000-0000-0000-000000000001"
	dig := strings.Repeat("a", 64)
	now := timestamppb.New(time.Now().UTC())
	later := timestamppb.New(time.Now().UTC().Add(time.Minute))
	op := &agentv1.CoreWorkloadOperation{OperationId: u, WorkloadId: u, PlanId: u, Kind: agentv1.CoreWorkloadOperationKind_CORE_WORKLOAD_OPERATION_KIND_APPLY, PlanRevision: 1, PlanDigest: dig, TargetKind: agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER, TaskId: u, ConfirmationId: u, Revision: 1, DesiredPlan: &agentv1.CoreWorkloadOperationPlan{PlanId: u, PlanRevision: 1, PlanDigest: dig, Target: &agentv1.CoreWorkloadTargetSettings{Identity: &agentv1.CoreWorkloadTargetIdentity{Kind: agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER}}}, CreatedAt: now, UpdatedAt: later}
	conf := &agentv1.CoreConfirmation{ConfirmationId: u, TaskId: u, State: agentv1.CoreConfirmationState_CORE_CONFIRMATION_STATE_PENDING, Revision: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: timestamppb.New(time.Now().UTC().Add(time.Hour)), Binding: &agentv1.CoreConfirmationBinding{OperationDomain: "workload:apply", TargetId: u, TargetRevision: 1, SourceVersion: "v1", ContentDigest: dig, ParameterDigest: dig, NetworkDigest: dig, SecretGrantDigest: dig}}
	if e := validateWorkloadMutationResponse(op, conf, u, u, "", false); e != nil {
		t.Fatalf("empty wid valid fixture rejected: %v", e)
	}
	q := &agentv1.CoreAWSQuote{PlanId: u, PlanDigest: dig, Operation: agentv1.CoreAWSOperation_CORE_AWS_OPERATION_CREATE, Region: "r", StackName: "s"}
	for _, x := range []float64{-1, math.NaN(), math.Inf(1)} {
		q.EstimatedMonthlyUsd = x
		if validateAWSQuote(q, u) == nil {
			t.Fatal("invalid quote accepted")
		}
	}
	q.EstimatedMonthlyUsd = 0
	if validateAWSQuote(q, u) != nil {
		t.Fatal("zero quote rejected")
	}
}

func TestOperationReadbackProjectionIsPublicAndLinked(t *testing.T) {
	u := "00000000-0000-0000-0000-000000000001"
	digest := strings.Repeat("a", 64)
	now := timestamppb.New(time.Now().UTC())
	identity := &agentv1.CoreWorkloadTargetIdentity{Kind: agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_ECS, AwsRegion: "us-east-1", AwsEcsSubnetIds: []string{"subnet-1"}, AwsEcsAssignPublicIp: true}
	target := &agentv1.CoreWorkloadTargetSettings{Identity: identity, Ports: []*agentv1.CoreWorkloadPort{{Port: 443}}, NetworkGrants: []*agentv1.CoreWorkloadNetworkGrant{{ReferenceId: "net-1", Kind: "egress"}}, Labels: map[string]string{"env": "test"}}
	op := &agentv1.CoreWorkloadOperation{OperationId: u, WorkloadId: u, PlanId: u, Kind: agentv1.CoreWorkloadOperationKind_CORE_WORKLOAD_OPERATION_KIND_APPLY, PlanRevision: 2, PlanDigest: digest, TargetKind: agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_ECS, TaskId: u, ConfirmationId: u, Revision: 3, CreatedAt: now, UpdatedAt: now, DesiredPlan: &agentv1.CoreWorkloadOperationPlan{PlanId: u, PlanRevision: 2, PlanDigest: digest, Target: target, ResourceLimits: &agentv1.CoreWorkloadResourceLimits{Cpu: 1, MemoryMb: 64}, SecretGrants: []*agentv1.CoreWorkloadSecretGrantRef{{ReferenceId: "secret-1", Purpose: agentv1.CoreWorkloadSecretPurpose_CORE_WORKLOAD_SECRET_PURPOSE_MCP_CREDENTIAL, BindingDigest: digest}}}, Actual: &agentv1.CoreWorkloadActualSnapshot{WorkloadId: u, Revision: 3, State: "running", Identity: identity, AppliedPlanId: u, AppliedPlanDigest: digest, ReadbackDigest: digest, ProviderVersion: "v1", ObservedAt: now, UpdatedAt: now}}
	projected := operationMap(op)
	if projected["target_kind"] != "aws-ecs" || projected["task_id"] != u || projected["confirmation_id"] != u {
		t.Fatalf("operation linkage projection = %#v", projected)
	}
	desired := projected["desired_plan"].(map[string]any)
	if desired["plan_id"] != u || desired["plan_revision"] != uint64(2) || desired["target"].(map[string]any)["identity"].(map[string]any)["kind"] != "aws-ecs" {
		t.Fatalf("desired plan projection = %#v", desired)
	}
	actual := projected["actual"].(map[string]any)
	if actual["identity"].(map[string]any)["kind"] != "aws-ecs" || actual["applied_plan_id"] != u {
		t.Fatalf("actual projection = %#v", actual)
	}
	raw, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, forbidden := range []string{"CORE_WORKLOAD_TARGET_KIND", "protoimpl", "unknownFields", "DesiredPlan", "Actual"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("protobuf shape leaked: %s", text)
		}
	}
	if !strings.Contains(text, `"target_kind":"aws-ecs"`) || strings.Contains(text, `"kind":3`) {
		t.Fatalf("contract enum string missing: %s", text)
	}
	for _, kind := range []agentv1.CoreWorkloadTargetKind{agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER, agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_EC2_SSM, agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_ECS} {
		if got := workloadTargetKind(kind); got == "" || strings.Contains(got, "_") {
			t.Fatalf("target kind %v projected as %q", kind, got)
		}
	}
}

func TestOperationReadbackAllowsNewerCurrentActualAndEventReadbackIsSparse(t *testing.T) {
	id, oldDigest, newDigest := "00000000-0000-0000-0000-000000000001", strings.Repeat("a", 64), strings.Repeat("b", 64)
	now := timestamppb.New(time.Now().UTC())
	identity := &agentv1.CoreWorkloadTargetIdentity{Kind: agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER}
	currentIdentity := &agentv1.CoreWorkloadTargetIdentity{Kind: agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_ECS}
	current := &agentv1.CoreWorkloadActualSnapshot{WorkloadId: id, Revision: 2, State: "running", Identity: currentIdentity, AppliedPlanId: id, AppliedPlanDigest: newDigest, ReadbackDigest: newDigest, ProviderVersion: "v2", ObservedAt: now, UpdatedAt: now}
	op := &agentv1.CoreWorkloadOperation{OperationId: id, WorkloadId: id, PlanId: id, Kind: agentv1.CoreWorkloadOperationKind_CORE_WORKLOAD_OPERATION_KIND_APPLY, PlanRevision: 1, PlanDigest: oldDigest, TargetKind: identity.GetKind(), TaskId: id, ConfirmationId: id, Status: "succeeded", Revision: 1, CreatedAt: now, UpdatedAt: now, DesiredPlan: &agentv1.CoreWorkloadOperationPlan{PlanId: id, PlanRevision: 1, PlanDigest: oldDigest, Target: &agentv1.CoreWorkloadTargetSettings{Identity: identity}}, Actual: current}
	if err := validateWorkloadOperationReadback(op, id); err != nil {
		t.Fatalf("old core-runner operation with newer AWS current actual rejected: %#v", err)
	}
	sparse := &agentv1.CoreWorkloadActualSnapshot{WorkloadId: id, State: "running", Identity: identity, ReadbackDigest: oldDigest, ProviderVersion: "v1", ObservedAt: now}
	if err := validateSparseEventActual(sparse); err != nil {
		t.Fatalf("valid sparse event readback rejected: %#v", err)
	}
	mapped := sparseEventActualMap(sparse)
	if _, found := mapped["revision"]; found {
		t.Fatalf("sparse event readback fabricated current fields: %#v", mapped)
	}
	sparse.ReadbackDigest = "bad"
	if err := validateSparseEventActual(sparse); err == nil {
		t.Fatal("malformed sparse event readback accepted")
	}
}
func TestAWSChangeValidatorCorruptions(t *testing.T) {
	u := "00000000-0000-0000-0000-000000000001"
	now := timestamppb.New(time.Now().UTC())
	later := timestamppb.New(time.Now().UTC().Add(time.Minute))
	mk := func() *agentv1.CoreAWSChange {
		return &agentv1.CoreAWSChange{ChangeId: u, PlanId: u, CredentialId: u, TaskId: u, ConfirmationId: u, Operation: agentv1.CoreAWSOperation_CORE_AWS_OPERATION_CREATE, Status: "running", Stage: "requested", Revision: 1, CreatedAt: now, UpdatedAt: later}
	}
	if validateAWSChangeResponse(mk(), u, u) != nil {
		t.Fatal("valid rejected")
	}
	cases := []func(*agentv1.CoreAWSChange){func(v *agentv1.CoreAWSChange) { v.PlanId = "bad" }, func(v *agentv1.CoreAWSChange) { v.CredentialId = "bad" }, func(v *agentv1.CoreAWSChange) { v.Status = "bogus" }, func(v *agentv1.CoreAWSChange) { v.Stage = "bogus" }, func(v *agentv1.CoreAWSChange) { v.Operation = 0 }, func(v *agentv1.CoreAWSChange) { v.UpdatedAt = timestamppb.New(time.Now().UTC().Add(-time.Hour)) }}
	for _, mut := range cases {
		v := mk()
		mut(v)
		if validateAWSChangeResponse(v, u, u) == nil {
			t.Fatal("corruption accepted")
		}
	}
}
func TestCredentialReadbackCorruptions(t *testing.T) {
	u := "00000000-0000-0000-0000-000000000001"
	now := timestamppb.New(time.Now().UTC())
	later := timestamppb.New(time.Now().UTC().Add(time.Minute))
	mk := func() *agentv1.CoreAWSCredential {
		return &agentv1.CoreAWSCredential{CredentialId: u, Name: "n", Region: "r", Revision: 2, AccessKeyConfigured: true, SecretAccessKeyConfigured: true, SessionTokenConfigured: true, CreatedAt: now, UpdatedAt: later}
	}
	if validateCredentialReadback(mk(), u, "n", "r", true, true, true, 1) != nil {
		t.Fatal("valid rejected")
	}
	cases := []func(*agentv1.CoreAWSCredential){func(v *agentv1.CoreAWSCredential) { v.Name = "x" }, func(v *agentv1.CoreAWSCredential) { v.Region = "x" }, func(v *agentv1.CoreAWSCredential) { v.Revision = 1 }, func(v *agentv1.CoreAWSCredential) { v.AccessKeyConfigured = false }, func(v *agentv1.CoreAWSCredential) { v.SecretAccessKeyConfigured = false }, func(v *agentv1.CoreAWSCredential) { v.SessionTokenConfigured = false }}
	for _, mut := range cases {
		v := mk()
		mut(v)
		if validateCredentialReadback(v, u, "n", "r", true, true, true, 1) == nil {
			t.Fatal("corruption accepted")
		}
	}
}
func TestCredentialIdentityValidator(t *testing.T) {
	u := "00000000-0000-0000-0000-000000000001"
	ts := timestamppb.New(time.Now().UTC())
	if validateCredentialIdentityResponse(u, "a", "u", "p", 1, ts) != nil {
		t.Fatal("valid rejected")
	}
	if validateCredentialIdentityResponse(u, "", "u", "p", 1, ts) == nil || validateCredentialIdentityResponse(u, "a", "", "p", 1, ts) == nil || validateCredentialIdentityResponse(u, "a", "u", "p", 0, ts) == nil {
		t.Fatal("corruption accepted")
	}
}
func TestCredentialProjectionRedactsSecrets(t *testing.T) {
	s := string(marshalSafe(awsCredentialMap(&agentv1.CoreAWSCredential{CredentialId: "00000000-0000-0000-0000-000000000001", Name: "x", AccessKeyConfigured: true})))
	if strings.Contains(s, "AKIA") || strings.Contains(s, "SECRET") {
		t.Fatal(s)
	}
}

func TestUpstreamValidatorsRejectBadLinkageAndPagination(t *testing.T) {
	if checkRespUUID("bad", "", "x") == nil || checkRespUUID("00000000-0000-0000-0000-000000000001", "00000000-0000-0000-0000-000000000002", "x") == nil {
		t.Fatal("bad linkage accepted")
	}
	if checkNextToken(strings.Repeat("x", 4097)) == nil || checkNextToken("ok") != nil {
		t.Fatal("pagination validation failed")
	}
}
