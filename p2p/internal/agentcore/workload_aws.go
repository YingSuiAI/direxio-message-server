package agentcore

import (
	"context"
	"github.com/google/uuid"
	"math"
	"regexp"
	"strings"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	agentv1 "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcorev1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func upstreamInvalid(detail string) *actionbase.Error {
	return actionbase.CodedError(502, "agent_core_upstream_failed", "agent core returned an invalid "+detail+" response")
}
func validRespUUID(id string) bool { _, err := uuid.Parse(id); return err == nil }
func checkRespUUID(id, want, family string) *actionbase.Error {
	if !validRespUUID(id) || (want != "" && id != want) {
		return upstreamInvalid(family)
	}
	return nil
}
func checkNextToken(token string) *actionbase.Error {
	if len(token) > 4096 || strings.ContainsAny(token, "\r\n") {
		return upstreamInvalid("pagination")
	}
	return nil
}
func validDigest(s string) bool { return regexp.MustCompile(`^[a-fA-F0-9]{64}$`).MatchString(s) }
func validAWSOp(v agentv1.CoreAWSOperation) bool {
	return v == agentv1.CoreAWSOperation_CORE_AWS_OPERATION_CREATE || v == agentv1.CoreAWSOperation_CORE_AWS_OPERATION_UPDATE || v == agentv1.CoreAWSOperation_CORE_AWS_OPERATION_DELETE
}
func validTarget(v agentv1.CoreWorkloadTargetKind) bool {
	return v == agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER || v == agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_EC2_SSM || v == agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_ECS
}
func validChangeStatus(s string) bool {
	switch s {
	case "waiting_user", "running", "succeeded", "failed", "canceled":
		return true
	}
	return false
}
func validChangeStage(s string) bool {
	switch s {
	case "requested", "change_set_creating", "change_set_ready", "executing", "reconciling", "reconciliation_required", "succeeded", "failed", "canceled":
		return true
	}
	return false
}
func validateAWSQuote(v *agentv1.CoreAWSQuote, want string) *actionbase.Error {
	if v == nil || v.GetPlanId() != want || !validDigest(v.GetPlanDigest()) || !validAWSOp(v.GetOperation()) || v.GetRegion() == "" || v.GetStackName() == "" || v.GetResourceCount() < 0 || v.GetParameterCount() < 0 || v.GetTagCount() < 0 || math.IsNaN(v.GetEstimatedMonthlyUsd()) || math.IsInf(v.GetEstimatedMonthlyUsd(), 0) || v.GetEstimatedMonthlyUsd() < 0 {
		return upstreamInvalid("AWS quote")
	}
	return nil
}
func validWorkloadOp(v agentv1.CoreWorkloadOperationKind) bool {
	return v == agentv1.CoreWorkloadOperationKind_CORE_WORKLOAD_OPERATION_KIND_APPLY || v == agentv1.CoreWorkloadOperationKind_CORE_WORKLOAD_OPERATION_KIND_DESTROY
}
func validWorkloadOperationStatus(status string) bool {
	switch status {
	case "waiting_user", "running", "succeeded", "failed", "uncertain", "rejected", "expired", "canceled":
		return true
	}
	return false
}
func validTimePair(created, updated *timestamppb.Timestamp) bool {
	return created != nil && updated != nil && created.IsValid() && updated.IsValid() && !updated.AsTime().Before(created.AsTime())
}
func validateCredentialResponse(v *agentv1.CoreAWSCredential, want string, min ...int64) *actionbase.Error {
	minRev := int64(0)
	if len(min) > 0 {
		minRev = min[0]
	}
	if v == nil || checkRespUUID(v.GetCredentialId(), want, "credential") != nil || v.GetRevision() <= minRev || v.GetName() == "" || v.GetRegion() == "" || !validTimePair(v.GetCreatedAt(), v.GetUpdatedAt()) {
		return upstreamInvalid("credential")
	}
	return nil
}
func validateCredentialReadback(v *agentv1.CoreAWSCredential, want, name, region string, ak, sk, st bool, min int64) *actionbase.Error {
	if e := validateCredentialResponse(v, want, min); e != nil {
		return e
	}
	if (name != "" && v.GetName() != name) || (region != "" && v.GetRegion() != region) || (ak && !v.GetAccessKeyConfigured()) || (sk && !v.GetSecretAccessKeyConfigured()) || (st && !v.GetSessionTokenConfigured()) {
		return upstreamInvalid("credential")
	}
	return nil
}
func validateCredentialIdentityResponse(id, account, user, principal string, rev int64, ts *timestamppb.Timestamp) *actionbase.Error {
	if !validRespUUID(id) || account == "" || user == "" || principal == "" || rev < 1 || ts == nil || !ts.IsValid() {
		return upstreamInvalid("credential identity")
	}
	return nil
}
func validateAWSChangeResponse(v *agentv1.CoreAWSChange, want string, filter ...string) *actionbase.Error {
	planFilter := ""
	if len(filter) > 0 {
		planFilter = filter[0]
	}
	if v == nil || checkRespUUID(v.GetChangeId(), want, "change") != nil || !validRespUUID(v.GetPlanId()) || (planFilter != "" && v.GetPlanId() != planFilter) || !validRespUUID(v.GetCredentialId()) || !validRespUUID(v.GetTaskId()) || !validRespUUID(v.GetConfirmationId()) || !validAWSOp(v.GetOperation()) || v.GetRevision() < 1 || !validChangeStatus(v.GetStatus()) || !validChangeStage(v.GetStage()) || !validTimePair(v.GetCreatedAt(), v.GetUpdatedAt()) {
		return upstreamInvalid("change")
	}
	return nil
}
func validateAWSPlanResponse(v *agentv1.CoreAWSPlan, want string) *actionbase.Error {
	if v == nil || checkRespUUID(v.GetPlanId(), want, "plan") != nil || !validRespUUID(v.GetCredentialId()) || !validAWSOp(v.GetOperation()) || v.GetRevision() < 1 || v.GetRegion() == "" || v.GetStackName() == "" || !validDigest(v.GetTemplateSha256()) || v.GetCreatedAt() == nil || !v.GetCreatedAt().IsValid() {
		return upstreamInvalid("plan")
	}
	return nil
}
func validateWorkloadPlan(v *agentv1.CoreWorkloadPlan, want string) *actionbase.Error {
	if v == nil || checkRespUUID(v.GetPlanId(), want, "workload plan") != nil || v.GetRevision() < 1 || !validDigest(v.GetDigest()) || !validTarget(v.GetTargetKind()) || v.GetTypedTarget() == nil || v.GetTypedTarget().GetIdentity() == nil || v.GetTypedTarget().GetIdentity().GetKind() != v.GetTargetKind() || !validTimePair(v.GetCreatedAt(), v.GetExpiresAt()) {
		return upstreamInvalid("workload plan")
	}
	return nil
}
func validateWorkloadMutation(v *agentv1.CoreWorkloadOperation, plan string, destroy bool) *actionbase.Error {
	if v == nil || !validRespUUID(v.GetOperationId()) || !validRespUUID(v.GetWorkloadId()) || v.GetPlanId() != plan || !validDigest(v.GetPlanDigest()) || !validWorkloadOp(v.GetKind()) || v.GetPlanRevision() < 1 || !validTarget(v.GetTargetKind()) || v.GetRevision() < 1 || !validRespUUID(v.GetTaskId()) || !validRespUUID(v.GetConfirmationId()) || !validTimePair(v.GetCreatedAt(), v.GetUpdatedAt()) || v.GetDesiredPlan() == nil || v.GetDesiredPlan().GetPlanId() != plan || int64(v.GetDesiredPlan().GetPlanRevision()) != v.GetPlanRevision() || v.GetDesiredPlan().GetPlanDigest() != v.GetPlanDigest() || v.GetDesiredPlan().GetTarget() == nil || v.GetDesiredPlan().GetTarget().GetIdentity() == nil || v.GetDesiredPlan().GetTarget().GetIdentity().GetKind() != v.GetTargetKind() {
		return upstreamInvalid("workload operation")
	}
	if destroy && v.GetKind() != agentv1.CoreWorkloadOperationKind_CORE_WORKLOAD_OPERATION_KIND_DESTROY {
		return upstreamInvalid("workload operation")
	}
	if !destroy && v.GetKind() != agentv1.CoreWorkloadOperationKind_CORE_WORKLOAD_OPERATION_KIND_APPLY {
		return upstreamInvalid("workload operation")
	}
	return nil
}
func validateWorkloadOperationReadback(v *agentv1.CoreWorkloadOperation, want string) *actionbase.Error {
	if v == nil || checkRespUUID(v.GetOperationId(), want, "workload operation") != nil || !validRespUUID(v.GetWorkloadId()) || !validRespUUID(v.GetPlanId()) || !validWorkloadOp(v.GetKind()) || v.GetPlanRevision() < 1 || !validDigest(v.GetPlanDigest()) || !validTarget(v.GetTargetKind()) || !validRespUUID(v.GetTaskId()) || !validRespUUID(v.GetConfirmationId()) || !validWorkloadOperationStatus(v.GetStatus()) || v.GetRevision() < 1 || !validTimePair(v.GetCreatedAt(), v.GetUpdatedAt()) {
		return upstreamInvalid("workload operation")
	}
	desired := v.GetDesiredPlan()
	if desired == nil || desired.GetPlanId() != v.GetPlanId() || int64(desired.GetPlanRevision()) != v.GetPlanRevision() || desired.GetPlanDigest() != v.GetPlanDigest() || desired.GetTarget() == nil || desired.GetTarget().GetIdentity() == nil || desired.GetTarget().GetIdentity().GetKind() != v.GetTargetKind() {
		return upstreamInvalid("workload operation")
	}
	if actual := v.GetActual(); actual != nil {
		if e := validateWorkloadActualReadback(actual, actual.GetWorkloadId()); e != nil || actual.GetWorkloadId() != v.GetWorkloadId() {
			return upstreamInvalid("workload operation")
		}
	}
	return nil
}
func validateWorkloadActualReadback(v *agentv1.CoreWorkloadActualSnapshot, want string) *actionbase.Error {
	if v == nil || checkRespUUID(v.GetWorkloadId(), want, "workload") != nil || v.GetRevision() < 1 || strings.TrimSpace(v.GetState()) == "" || v.GetIdentity() == nil || !validTarget(v.GetIdentity().GetKind()) || !validRespUUID(v.GetAppliedPlanId()) || !validDigest(v.GetAppliedPlanDigest()) || !validDigest(v.GetReadbackDigest()) || strings.TrimSpace(v.GetProviderVersion()) == "" || !validTimePair(v.GetObservedAt(), v.GetUpdatedAt()) {
		return upstreamInvalid("workload")
	}
	return nil
}
func validateWorkloadMutationResponse(op *agentv1.CoreWorkloadOperation, conf *agentv1.CoreConfirmation, topTask, plan, wid string, destroy bool) *actionbase.Error {
	if e := validateWorkloadMutation(op, plan, destroy); e != nil {
		return e
	}
	if topTask == "" || topTask != op.GetTaskId() || conf == nil || conf.GetConfirmationId() != op.GetConfirmationId() || conf.GetTaskId() != op.GetTaskId() {
		return upstreamInvalid("workload confirmation")
	}
	if wid != "" && (!validRespUUID(wid) || op.GetWorkloadId() != wid) {
		return upstreamInvalid("workload workload")
	}
	b := conf.GetBinding()
	domain := "workload:apply"
	if destroy {
		domain = "workload:destroy"
	}
	if conf.GetState() != agentv1.CoreConfirmationState_CORE_CONFIRMATION_STATE_PENDING || conf.GetRevision() < 1 || !validTimePair(conf.GetCreatedAt(), conf.GetUpdatedAt()) || conf.GetExpiresAt() == nil || !conf.GetExpiresAt().IsValid() || !conf.GetExpiresAt().AsTime().After(conf.GetUpdatedAt().AsTime()) || b == nil || b.GetOperationDomain() != domain || b.GetTargetId() != op.GetWorkloadId() || b.GetTargetRevision() != op.GetPlanRevision() || b.GetContentDigest() != op.GetPlanDigest() || !validDigest(b.GetContentDigest()) || !validDigest(b.GetParameterDigest()) || !validDigest(b.GetNetworkDigest()) || !validDigest(b.GetSecretGrantDigest()) || b.GetSourceVersion() == "" {
		return upstreamInvalid("workload confirmation")
	}
	return nil
}

func (c *Client) capabilityAny(ctx context.Context, names ...string) *actionbase.Error {
	_ = c.Probe(ctx)
	s := c.Snapshot()
	if s.Status != StatusReady {
		return actionbase.CodedError(412, "agent_core_capability_unavailable", "agent core capability is unavailable")
	}
	for _, have := range s.Capabilities {
		for _, want := range names {
			if have == want {
				return nil
			}
		}
	}
	return actionbase.CodedError(412, "agent_core_capability_unavailable", "agent core capability is unavailable")
}
func (c *Client) workloadGate(ctx context.Context) *actionbase.Error {
	return c.capabilityAny(ctx, "workload.core_runner", "workload.aws_ssm", "workload.aws_ecs")
}
func (c *Client) awsGate(ctx context.Context) *actionbase.Error {
	return c.capability(ctx, "aws.control")
}

func tsParam(params map[string]any, key string) (*timestamppb.Timestamp, *actionbase.Error) {
	s, e := requiredString(params, key)
	if e != nil {
		return nil, e
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return nil, actionbase.BadRequest(key + " must be RFC3339 timestamp")
	}
	return timestamppb.New(t.UTC()), nil
}
func strictStringMap(v any, k string) (map[string]string, *actionbase.Error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, actionbase.BadRequest(k + " must be object")
	}
	out := map[string]string{}
	for key, val := range m {
		s, ok := val.(string)
		if !ok {
			return nil, actionbase.BadRequest(k + " values must be strings")
		}
		out[key] = s
	}
	return out, nil
}
func strictKeys(m map[string]any, allowed ...string) *actionbase.Error {
	set := map[string]bool{}
	for _, k := range allowed {
		set[k] = true
	}
	for k := range m {
		if !set[k] {
			return actionbase.BadRequest("unknown field: " + k)
		}
	}
	return nil
}
func int64Param(m map[string]any, k string) (int64, *actionbase.Error) {
	v, e := optionalInt64(m, k)
	if e != nil {
		return 0, e
	}
	return v, nil
}
func boolParam(m map[string]any, k string) (bool, *actionbase.Error) {
	v, ok := m[k]
	if !ok {
		return false, nil
	}
	b, ok := v.(bool)
	if !ok {
		return false, actionbase.BadRequest(k + " must be boolean")
	}
	return b, nil
}
func targetSettingsParam(v any) (*agentv1.CoreWorkloadTargetSettings, *actionbase.Error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, actionbase.BadRequest("typed_target must be an object")
	}
	if e := strictKeys(m, "identity", "ports", "network_grants", "labels"); e != nil {
		return nil, e
	}
	idm, ok := m["identity"].(map[string]any)
	if !ok {
		return nil, actionbase.BadRequest("typed_target.identity is required")
	}
	if e := strictKeys(idm, "kind", "core_runner_service", "image_digest", "aws_account_id", "aws_region", "instance_id", "cluster", "service", "task_definition_revision", "desired_count", "endpoint", "core_runner_id", "aws_ec2_document_version", "aws_ec2_systemd_service", "aws_ec2_required_instance_tags", "aws_ecs_cluster_arn", "aws_ecs_service_name", "aws_ecs_task_family", "aws_ecs_platform_version", "aws_ecs_subnet_ids", "aws_ecs_security_group_ids", "aws_ecs_assign_public_ip", "aws_ecs_target_group_arn", "aws_ecs_target_group_port", "aws_ecs_task_role_arn", "aws_ecs_execution_role_arn", "aws_ecs_desired_count", "aws_ecs_image_uri"); e != nil {
		return nil, e
	}
	id := &agentv1.CoreWorkloadTargetIdentity{}
	get := func(k string) (string, *actionbase.Error) {
		if raw, exists := idm[k]; exists {
			s, ok := raw.(string)
			if !ok {
				return "", actionbase.BadRequest(k + " must be string")
			}
			return s, nil
		}
		return "", nil
	}
	kindRaw, e := get("kind")
	if e != nil {
		return nil, e
	}
	kind := strings.ToLower(kindRaw)
	switch kind {
	case "core-runner", "core_runner":
		id.Kind = agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER
	case "aws-ec2-ssm", "aws_ec2_ssm":
		id.Kind = agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_EC2_SSM
	case "aws-ecs", "aws_ecs":
		id.Kind = agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_ECS
	default:
		return nil, actionbase.BadRequest("target_kind is invalid")
	}
	id.CoreRunnerService, e = get("core_runner_service")
	if e != nil {
		return nil, e
	}
	assign := func(dst *string, k string) *actionbase.Error {
		s, er := get(k)
		if er != nil {
			return er
		}
		*dst = s
		return nil
	}
	for dst, k := range map[*string]string{&id.ImageDigest: "image_digest", &id.AwsAccountId: "aws_account_id", &id.AwsRegion: "aws_region", &id.InstanceId: "instance_id", &id.Cluster: "cluster", &id.Service: "service", &id.TaskDefinitionRevision: "task_definition_revision", &id.Endpoint: "endpoint", &id.CoreRunnerId: "core_runner_id", &id.AwsEc2DocumentVersion: "aws_ec2_document_version", &id.AwsEc2SystemdService: "aws_ec2_systemd_service", &id.AwsEcsClusterArn: "aws_ecs_cluster_arn", &id.AwsEcsServiceName: "aws_ecs_service_name", &id.AwsEcsTaskFamily: "aws_ecs_task_family", &id.AwsEcsPlatformVersion: "aws_ecs_platform_version", &id.AwsEcsTargetGroupArn: "aws_ecs_target_group_arn", &id.AwsEcsTaskRoleArn: "aws_ecs_task_role_arn", &id.AwsEcsExecutionRoleArn: "aws_ecs_execution_role_arn", &id.AwsEcsImageUri: "aws_ecs_image_uri"} {
		if er := assign(dst, k); er != nil {
			return nil, er
		}
	}
	id.DesiredCount, e = int64Param(idm, "desired_count")
	if e != nil {
		return nil, e
	}
	id.AwsEcsDesiredCount, e = int64Param(idm, "aws_ecs_desired_count")
	if e != nil {
		return nil, e
	}
	id.AwsEcsAssignPublicIp, e = boolParam(idm, "aws_ecs_assign_public_ip")
	if e != nil {
		return nil, e
	}
	id.AwsEcsTargetGroupPort, e = uint32Param(idm, "aws_ecs_target_group_port")
	if e != nil {
		return nil, e
	}
	if tags, ok := idm["aws_ec2_required_instance_tags"]; ok {
		id.AwsEc2RequiredInstanceTags, e = strictStringMap(tags, "aws_ec2_required_instance_tags")
		if e != nil {
			return nil, e
		}
	}
	if v, ok := idm["aws_ecs_subnet_ids"]; ok {
		id.AwsEcsSubnetIds, e = stringSliceParam(v, "aws_ecs_subnet_ids")
		if e != nil {
			return nil, e
		}
	}
	if v, ok := idm["aws_ecs_security_group_ids"]; ok {
		id.AwsEcsSecurityGroupIds, e = stringSliceParam(v, "aws_ecs_security_group_ids")
		if e != nil {
			return nil, e
		}
	}
	out := &agentv1.CoreWorkloadTargetSettings{Identity: id, Labels: map[string]string{}}
	if labels, ok := m["labels"]; ok {
		if _, ok := labels.(map[string]any); !ok {
			return nil, actionbase.BadRequest("labels must be object")
		}
		out.Labels, e = strictStringMap(labels, "labels")
		if e != nil {
			return nil, e
		}
	}
	if v, ok := m["network_grants"]; ok {
		a, ok := v.([]any)
		if !ok {
			return nil, actionbase.BadRequest("network_grants must be array")
		}
		for _, x := range a {
			mm, ok := x.(map[string]any)
			if !ok {
				return nil, actionbase.BadRequest("network_grants items must be objects")
			}
			if e := strictKeys(mm, "reference_id", "kind"); e != nil {
				return nil, e
			}
			ref, e := requiredString(mm, "reference_id")
			if e != nil {
				return nil, e
			}
			kind, e := requiredString(mm, "kind")
			if e != nil {
				return nil, e
			}
			out.NetworkGrants = append(out.NetworkGrants, &agentv1.CoreWorkloadNetworkGrant{ReferenceId: ref, Kind: kind})
		}
	}
	if v, ok := m["ports"]; ok {
		arr, ok := v.([]any)
		if !ok {
			return nil, actionbase.BadRequest("ports must be array")
		}
		for _, x := range arr {
			mm, ok := x.(map[string]any)
			if !ok {
				return nil, actionbase.BadRequest("ports items must be objects")
			}
			if e := strictKeys(mm, "port"); e != nil {
				return nil, e
			}
			p, pe := optionalInt64(mm, "port")
			if pe != nil {
				return nil, pe
			}
			if p < 1 || p > 65535 {
				return nil, actionbase.BadRequest("port is invalid")
			}
			out.Ports = append(out.Ports, &agentv1.CoreWorkloadPort{Port: uint32(p)})
		}
	}
	return out, nil
}
func uint32Param(m map[string]any, k string) (uint32, *actionbase.Error) {
	v, e := optionalInt64(m, k)
	if e != nil {
		return 0, e
	}
	if v < 0 || v > 4294967295 {
		return 0, actionbase.BadRequest(k + " is out of range")
	}
	return uint32(v), nil
}
func stringSliceParam(v any, k string) ([]string, *actionbase.Error) {
	a, ok := v.([]any)
	if !ok {
		return nil, actionbase.BadRequest(k + " must be array")
	}
	out := make([]string, 0, len(a))
	for _, x := range a {
		s, ok := x.(string)
		if !ok || strings.TrimSpace(s) == "" {
			return nil, actionbase.BadRequest(k + " must contain strings")
		}
		out = append(out, s)
	}
	return out, nil
}
func limitsParam(v any) (*agentv1.CoreWorkloadResourceLimits, *actionbase.Error) {
	if v == nil {
		return nil, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, actionbase.BadRequest("typed_resource_limits must be object")
	}
	if e := strictKeys(m, "cpu", "memory_mb", "processes", "disk_mb", "timeout_seconds", "output_mb"); e != nil {
		return nil, e
	}
	o := &agentv1.CoreWorkloadResourceLimits{}
	var e *actionbase.Error
	o.Cpu, e = int64Param(m, "cpu")
	if e != nil {
		return nil, e
	}
	o.MemoryMb, e = int64Param(m, "memory_mb")
	if e != nil {
		return nil, e
	}
	o.Processes, e = int64Param(m, "processes")
	if e != nil {
		return nil, e
	}
	o.DiskMb, e = int64Param(m, "disk_mb")
	if e != nil {
		return nil, e
	}
	o.TimeoutSeconds, e = int64Param(m, "timeout_seconds")
	if e != nil {
		return nil, e
	}
	o.OutputMb, e = int64Param(m, "output_mb")
	if e != nil {
		return nil, e
	}
	return o, nil
}
func secretRefsParam(v any) ([]*agentv1.CoreWorkloadSecretGrantRef, *actionbase.Error) {
	if v == nil {
		return nil, nil
	}
	a, ok := v.([]any)
	if !ok {
		return nil, actionbase.BadRequest("typed_secret_grants must be array")
	}
	out := make([]*agentv1.CoreWorkloadSecretGrantRef, 0, len(a))
	for _, x := range a {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, actionbase.BadRequest("typed_secret_grants items must be objects")
		}
		if e := strictKeys(m, "reference_id", "purpose", "binding_digest"); e != nil {
			return nil, e
		}
		ref, e := requiredString(m, "reference_id")
		if e != nil {
			return nil, e
		}
		purp, e := requiredString(m, "purpose")
		if e != nil {
			return nil, e
		}
		var p agentv1.CoreWorkloadSecretPurpose
		switch strings.ToLower(purp) {
		case "model-api-key", "model_api_key":
			p = agentv1.CoreWorkloadSecretPurpose_CORE_WORKLOAD_SECRET_PURPOSE_MODEL_API_KEY
		case "mcp-credential", "mcp_credential":
			p = agentv1.CoreWorkloadSecretPurpose_CORE_WORKLOAD_SECRET_PURPOSE_MCP_CREDENTIAL
		case "skill-secret", "skill_secret":
			p = agentv1.CoreWorkloadSecretPurpose_CORE_WORKLOAD_SECRET_PURPOSE_SKILL_SECRET
		case "aws-credential", "aws_credential":
			p = agentv1.CoreWorkloadSecretPurpose_CORE_WORKLOAD_SECRET_PURPOSE_AWS_CREDENTIAL
		case "other-extension-secret", "other_extension_secret":
			p = agentv1.CoreWorkloadSecretPurpose_CORE_WORKLOAD_SECRET_PURPOSE_OTHER_EXTENSION_SECRET
		default:
			return nil, actionbase.BadRequest("purpose is invalid")
		}
		dig, e := requiredString(m, "binding_digest")
		if e != nil {
			return nil, e
		}
		out = append(out, &agentv1.CoreWorkloadSecretGrantRef{ReferenceId: ref, Purpose: p, BindingDigest: dig})
	}
	return out, nil
}
func workloadPlanMap(p *agentv1.CoreWorkloadPlan) map[string]any {
	if p == nil {
		return nil
	}
	out := map[string]any{"plan_id": p.GetPlanId(), "revision": p.GetRevision(), "digest": p.GetDigest(), "summary": safeText(p.GetSummary()), "artifact": safeText(p.GetArtifact()), "source": safeText(p.GetSource()), "command_steps": append([]string(nil), p.GetCommandSteps()...), "image_digest": p.GetImageDigest(), "image_uri": p.GetImageUri(), "target_kind": workloadTargetKind(p.GetTargetKind()), "expires_at": timestampMap(p.GetExpiresAt()), "created_at": timestampMap(p.GetCreatedAt()), "typed_target": targetSettingsMap(p.GetTypedTarget()), "typed_resource_limits": resourceLimitsMap(p.GetTypedResourceLimits())}
	refs := []any{}
	for _, r := range p.GetTypedSecretGrants() {
		if r != nil {
			refs = append(refs, map[string]any{"reference_id": r.GetReferenceId(), "purpose": enumName(r.GetPurpose().String()), "binding_digest": r.GetBindingDigest()})
		}
	}
	out["typed_secret_grants"] = refs
	return out
}
func operationMap(o *agentv1.CoreWorkloadOperation) map[string]any {
	if o == nil {
		return nil
	}
	return map[string]any{"operation_id": o.GetOperationId(), "workload_id": o.GetWorkloadId(), "plan_id": o.GetPlanId(), "kind": enumName(o.GetKind().String()), "plan_revision": o.GetPlanRevision(), "plan_digest": o.GetPlanDigest(), "target_kind": workloadTargetKind(o.GetTargetKind()), "task_id": o.GetTaskId(), "confirmation_id": o.GetConfirmationId(), "status": safeText(o.GetStatus()), "revision": o.GetRevision(), "failure_code": safeText(o.GetFailureCode()), "failure_summary": safeText(o.GetFailureSummary()), "created_at": timestampMap(o.GetCreatedAt()), "updated_at": timestampMap(o.GetUpdatedAt()), "desired_plan": operationPlanMap(o.GetDesiredPlan()), "actual": actualMap(o.GetActual()), "dispatch_epoch": o.GetDispatchEpoch(), "dispatch_lease_until": timestampMap(o.GetDispatchLeaseUntil())}
}
func actualMap(a *agentv1.CoreWorkloadActualSnapshot) map[string]any {
	if a == nil {
		return nil
	}
	return map[string]any{"workload_id": a.GetWorkloadId(), "revision": a.GetRevision(), "state": safeText(a.GetState()), "identity": targetIdentityMap(a.GetIdentity()), "applied_plan_id": a.GetAppliedPlanId(), "applied_plan_digest": a.GetAppliedPlanDigest(), "readback_digest": a.GetReadbackDigest(), "provider_version": safeText(a.GetProviderVersion()), "observed_at": timestampMap(a.GetObservedAt()), "updated_at": timestampMap(a.GetUpdatedAt())}
}
func sparseEventActualMap(a *agentv1.CoreWorkloadActualSnapshot) map[string]any {
	if a == nil {
		return nil
	}
	return map[string]any{"workload_id": a.GetWorkloadId(), "state": safeText(a.GetState()), "identity": targetIdentityMap(a.GetIdentity()), "readback_digest": a.GetReadbackDigest(), "provider_version": safeText(a.GetProviderVersion()), "observed_at": timestampMap(a.GetObservedAt())}
}
func validateSparseEventActual(v *agentv1.CoreWorkloadActualSnapshot) *actionbase.Error {
	if v == nil || !validRespUUID(v.GetWorkloadId()) || strings.TrimSpace(v.GetState()) == "" || v.GetIdentity() == nil || !validTarget(v.GetIdentity().GetKind()) || !validDigest(v.GetReadbackDigest()) || strings.TrimSpace(v.GetProviderVersion()) == "" || v.GetObservedAt() == nil || !v.GetObservedAt().IsValid() {
		return upstreamInvalid("workload event readback")
	}
	return nil
}

func workloadTargetKind(kind agentv1.CoreWorkloadTargetKind) string {
	switch kind {
	case agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER:
		return "core-runner"
	case agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_EC2_SSM:
		return "aws-ec2-ssm"
	case agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_ECS:
		return "aws-ecs"
	default:
		return ""
	}
}

func targetIdentityMap(identity *agentv1.CoreWorkloadTargetIdentity) map[string]any {
	if identity == nil {
		return nil
	}
	return map[string]any{"kind": workloadTargetKind(identity.GetKind()), "core_runner_service": identity.GetCoreRunnerService(), "image_digest": identity.GetImageDigest(), "aws_account_id": identity.GetAwsAccountId(), "aws_region": identity.GetAwsRegion(), "instance_id": identity.GetInstanceId(), "cluster": identity.GetCluster(), "service": identity.GetService(), "task_definition_revision": identity.GetTaskDefinitionRevision(), "desired_count": identity.GetDesiredCount(), "endpoint": identity.GetEndpoint(), "core_runner_id": identity.GetCoreRunnerId(), "aws_ec2_document_version": identity.GetAwsEc2DocumentVersion(), "aws_ec2_systemd_service": identity.GetAwsEc2SystemdService(), "aws_ec2_required_instance_tags": copyStringMap(identity.GetAwsEc2RequiredInstanceTags()), "aws_ecs_cluster_arn": identity.GetAwsEcsClusterArn(), "aws_ecs_service_name": identity.GetAwsEcsServiceName(), "aws_ecs_task_family": identity.GetAwsEcsTaskFamily(), "aws_ecs_platform_version": identity.GetAwsEcsPlatformVersion(), "aws_ecs_subnet_ids": append([]string(nil), identity.GetAwsEcsSubnetIds()...), "aws_ecs_security_group_ids": append([]string(nil), identity.GetAwsEcsSecurityGroupIds()...), "aws_ecs_assign_public_ip": identity.GetAwsEcsAssignPublicIp(), "aws_ecs_target_group_arn": identity.GetAwsEcsTargetGroupArn(), "aws_ecs_target_group_port": identity.GetAwsEcsTargetGroupPort(), "aws_ecs_task_role_arn": identity.GetAwsEcsTaskRoleArn(), "aws_ecs_execution_role_arn": identity.GetAwsEcsExecutionRoleArn(), "aws_ecs_desired_count": identity.GetAwsEcsDesiredCount(), "aws_ecs_image_uri": identity.GetAwsEcsImageUri()}
}

func targetSettingsMap(target *agentv1.CoreWorkloadTargetSettings) map[string]any {
	if target == nil {
		return nil
	}
	ports, grants := make([]any, 0, len(target.GetPorts())), make([]any, 0, len(target.GetNetworkGrants()))
	for _, port := range target.GetPorts() {
		if port != nil {
			ports = append(ports, map[string]any{"port": port.GetPort()})
		}
	}
	for _, grant := range target.GetNetworkGrants() {
		if grant != nil {
			grants = append(grants, map[string]any{"reference_id": grant.GetReferenceId(), "kind": safeText(grant.GetKind())})
		}
	}
	return map[string]any{"identity": targetIdentityMap(target.GetIdentity()), "ports": ports, "network_grants": grants, "labels": copyStringMap(target.GetLabels())}
}

func resourceLimitsMap(limits *agentv1.CoreWorkloadResourceLimits) map[string]any {
	if limits == nil {
		return nil
	}
	return map[string]any{"cpu": limits.GetCpu(), "memory_mb": limits.GetMemoryMb(), "processes": limits.GetProcesses(), "disk_mb": limits.GetDiskMb(), "timeout_seconds": limits.GetTimeoutSeconds(), "output_mb": limits.GetOutputMb()}
}

func secretGrantRefsMap(refs []*agentv1.CoreWorkloadSecretGrantRef) []any {
	out := make([]any, 0, len(refs))
	for _, ref := range refs {
		if ref != nil {
			out = append(out, map[string]any{"reference_id": ref.GetReferenceId(), "purpose": enumName(ref.GetPurpose().String()), "binding_digest": ref.GetBindingDigest()})
		}
	}
	return out
}

func operationPlanMap(plan *agentv1.CoreWorkloadOperationPlan) map[string]any {
	if plan == nil {
		return nil
	}
	return map[string]any{"plan_id": plan.GetPlanId(), "plan_revision": plan.GetPlanRevision(), "plan_digest": plan.GetPlanDigest(), "target": targetSettingsMap(plan.GetTarget()), "resource_limits": resourceLimitsMap(plan.GetResourceLimits()), "secret_grants": secretGrantRefsMap(plan.GetSecretGrants())}
}

func copyStringMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	out := make(map[string]string, len(source))
	for key, value := range source {
		out[key] = value
	}
	return out
}
func awsCredentialMap(v *agentv1.CoreAWSCredential) map[string]any {
	if v == nil {
		return nil
	}
	return map[string]any{"credential_id": v.GetCredentialId(), "name": safeText(v.GetName()), "region": v.GetRegion(), "account_id": v.GetAccountId(), "user_arn": v.GetUserArn(), "access_key_configured": v.GetAccessKeyConfigured(), "secret_access_key_configured": v.GetSecretAccessKeyConfigured(), "session_token_configured": v.GetSessionTokenConfigured(), "revision": v.GetRevision(), "created_at": timestampMap(v.GetCreatedAt()), "updated_at": timestampMap(v.GetUpdatedAt())}
}
func awsPlanMap(v *agentv1.CoreAWSPlan) map[string]any {
	if v == nil {
		return nil
	}
	return map[string]any{"plan_id": v.GetPlanId(), "credential_id": v.GetCredentialId(), "region": v.GetRegion(), "stack_name": v.GetStackName(), "operation": enumName(v.GetOperation().String()), "template_sha256": v.GetTemplateSha256(), "parameters": v.GetParameters(), "tags": v.GetTags(), "capabilities": v.GetCapabilities(), "revision": v.GetRevision(), "created_at": timestampMap(v.GetCreatedAt())}
}
func awsQuoteMap(v *agentv1.CoreAWSQuote) map[string]any {
	if v == nil {
		return nil
	}
	return map[string]any{"plan_id": v.GetPlanId(), "operation": enumName(v.GetOperation().String()), "region": v.GetRegion(), "stack_name": v.GetStackName(), "resource_count": v.GetResourceCount(), "parameter_count": v.GetParameterCount(), "tag_count": v.GetTagCount(), "estimated_monthly_usd": v.GetEstimatedMonthlyUsd(), "summary": safeText(v.GetSummary()), "plan_digest": v.GetPlanDigest()}
}
func awsChangeMap(v *agentv1.CoreAWSChange) map[string]any {
	if v == nil {
		return nil
	}
	return map[string]any{"change_id": v.GetChangeId(), "plan_id": v.GetPlanId(), "credential_id": v.GetCredentialId(), "task_id": v.GetTaskId(), "confirmation_id": v.GetConfirmationId(), "operation": enumName(v.GetOperation().String()), "status": safeText(v.GetStatus()), "stage": safeText(v.GetStage()), "change_set_id": v.GetChangeSetId(), "provider_request_digest": v.GetProviderRequestDigest(), "revision": v.GetRevision(), "error_code": safeText(v.GetErrorCode()), "error_summary": safeText(v.GetErrorSummary()), "created_at": timestampMap(v.GetCreatedAt()), "updated_at": timestampMap(v.GetUpdatedAt())}
}

func (c *Client) workloadHandlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{"agent.core.workloads.plan": c.workloadPlan, "agent.core.workloads.get": c.workloadGet, "agent.core.workloads.list": c.workloadList, "agent.core.workloads.quote": c.workloadQuote, "agent.core.workloads.apply": c.workloadApply, "agent.core.workloads.destroy": c.workloadDestroy, "agent.core.workloads.operations.get": c.workloadOperationGet, "agent.core.workloads.operations.events": c.workloadOperationEvents, "agent.core.workloads.actual.get": c.workloadActualGet}
}
func (c *Client) workloadPlan(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "idempotency_key", "summary", "artifact", "source", "command_steps", "image_digest", "image_uri", "target_kind", "expires_at", "typed_target", "typed_resource_limits", "typed_secret_grants"); e != nil {
		return nil, e
	}
	if e := c.workloadGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "idempotency_key")
	if e != nil {
		return nil, e
	}
	exp, e := tsParam(p, "expires_at")
	if e != nil {
		return nil, e
	}
	target, e := targetSettingsParam(p["typed_target"])
	if e != nil {
		return nil, e
	}
	summary, e := requiredString(p, "summary")
	if e != nil {
		return nil, e
	}
	artifact, e := requiredString(p, "artifact")
	if e != nil {
		return nil, e
	}
	source, e := requiredString(p, "source")
	if e != nil {
		return nil, e
	}
	imageDigest, e := optionalString(p, "image_digest")
	if e != nil {
		return nil, e
	}
	imageURI, e := optionalString(p, "image_uri")
	if e != nil {
		return nil, e
	}
	steps := []string{}
	if raw, present := p["command_steps"]; present {
		v, ok := raw.([]any)
		if !ok {
			return nil, actionbase.BadRequest("command_steps must be array")
		}
		for _, x := range v {
			s, ok := x.(string)
			if !ok || strings.TrimSpace(s) == "" {
				return nil, actionbase.BadRequest("command_steps must contain nonempty strings")
			}
			steps = append(steps, s)
		}
	}
	kindName, e := requiredString(p, "target_kind")
	if e != nil {
		return nil, e
	}
	kindName = strings.ToLower(kindName)
	expected := ""
	switch kindName {
	case "core-runner", "core_runner":
		expected = "core_workload_target_kind_core_runner"
	case "aws-ec2-ssm", "aws_ec2_ssm":
		expected = "core_workload_target_kind_aws_ec2_ssm"
	case "aws-ecs", "aws_ecs":
		expected = "core_workload_target_kind_aws_ecs"
	default:
		return nil, actionbase.BadRequest("target_kind is invalid")
	}
	if strings.ToLower(target.GetIdentity().GetKind().String()) != expected {
		return nil, actionbase.BadRequest("target_kind does not match typed_target.identity.kind")
	}
	limits, e := limitsParam(p["typed_resource_limits"])
	if e != nil {
		return nil, e
	}
	refs, e := secretRefsParam(p["typed_secret_grants"])
	if e != nil {
		return nil, e
	}
	tk := agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_UNSPECIFIED
	switch strings.ToLower(target.GetIdentity().GetKind().String()) {
	case "core_workload_target_kind_core_runner":
		tk = agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_CORE_RUNNER
	case "core_workload_target_kind_aws_ec2_ssm":
		tk = agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_EC2_SSM
	case "core_workload_target_kind_aws_ecs":
		tk = agentv1.CoreWorkloadTargetKind_CORE_WORKLOAD_TARGET_KIND_AWS_ECS
	}
	var r *agentv1.WorkloadServicePlanResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewWorkloadServiceClient(conn).Plan(cc, &agentv1.WorkloadServicePlanRequest{IdempotencyKey: id, Summary: summary, Artifact: artifact, Source: source, ImageDigest: imageDigest, ImageUri: imageURI, CommandSteps: steps, TargetKind: tk, ExpiresAt: exp, TypedTarget: target, TypedResourceLimits: limits, TypedSecretGrants: refs})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "workload")
	}
	if e := ensureResponse(r, "workload plan"); e != nil {
		return nil, e
	}
	if e := validateWorkloadPlan(r.GetPlan(), ""); e != nil {
		return nil, e
	}
	return map[string]any{"plan": workloadPlanMap(r.GetPlan())}, nil
}

func (c *Client) workloadGet(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "plan_id"); e != nil {
		return nil, e
	}
	if e := c.workloadGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "plan_id")
	if e != nil {
		return nil, e
	}
	var r *agentv1.WorkloadServiceGetResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewWorkloadServiceClient(conn).Get(cc, &agentv1.WorkloadServiceGetRequest{PlanId: id})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "workload")
	}
	if e := ensureResponse(r, "workload get"); e != nil {
		return nil, e
	}
	if e := validateWorkloadPlan(r.GetPlan(), id); e != nil {
		return nil, e
	}
	return map[string]any{"plan": workloadPlanMap(r.GetPlan())}, nil
}
func (c *Client) workloadList(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "page_size", "page_token"); e != nil {
		return nil, e
	}
	if e := c.workloadGate(ctx); e != nil {
		return nil, e
	}
	sz, t, e := parsePage(p)
	if e != nil {
		return nil, e
	}
	var r *agentv1.WorkloadServiceListResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewWorkloadServiceClient(conn).List(cc, &agentv1.WorkloadServiceListRequest{PageSize: sz, PageToken: t})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "workload")
	}
	if e := ensureResponse(r, "workload list"); e != nil {
		return nil, e
	}
	if e := checkNextToken(r.GetNextPageToken()); e != nil {
		return nil, e
	}
	out := []any{}
	for _, v := range r.GetPlans() {
		if e := validateWorkloadPlan(v, ""); e != nil {
			return nil, e
		}
		if v == nil || !validRespUUID(v.GetPlanId()) || v.GetDigest() == "" {
			return nil, upstreamInvalid("workload list")
		}
		out = append(out, workloadPlanMap(v))
	}
	return map[string]any{"plans": out, "next_page_token": r.GetNextPageToken()}, nil
}
func (c *Client) workloadQuote(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "plan_id"); e != nil {
		return nil, e
	}
	if e := c.workloadGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "plan_id")
	if e != nil {
		return nil, e
	}
	var r *agentv1.WorkloadServiceQuoteResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewWorkloadServiceClient(conn).Quote(cc, &agentv1.WorkloadServiceQuoteRequest{PlanId: id})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "workload")
	}
	if e := ensureResponse(r, "workload quote"); e != nil {
		return nil, e
	}
	if r.GetQuote() == nil || r.GetQuote().GetPlanId() != id || !validDigest(r.GetQuote().GetPlanDigest()) {
		return nil, upstreamInvalid("workload quote")
	}
	return map[string]any{"quote": map[string]any{"plan_id": r.GetQuote().GetPlanId(), "plan_digest": r.GetQuote().GetPlanDigest(), "summary": safeText(r.GetQuote().GetSummary())}}, nil
}
func (c *Client) workloadMutation(ctx context.Context, p map[string]any, destroy bool) (any, *actionbase.Error) {
	if e := strictKeys(p, "idempotency_key", "plan_id", "workload_id"); e != nil {
		return nil, e
	}
	if e := c.workloadGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "idempotency_key")
	if e != nil {
		return nil, e
	}
	plan, e := requiredUUID(p, "plan_id")
	if e != nil {
		return nil, e
	}
	wid, e := optionalString(p, "workload_id")
	if e != nil {
		return nil, e
	}
	if wid != "" && !validRespUUID(wid) {
		return nil, actionbase.BadRequest("workload_id must be a UUID")
	}
	var op *agentv1.CoreWorkloadOperation
	var conf *agentv1.CoreConfirmation
	var task string
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		if destroy {
			r, er := agentv1.NewWorkloadServiceClient(conn).RequestDestroy(cc, &agentv1.WorkloadServiceRequestDestroyRequest{IdempotencyKey: id, PlanId: plan, WorkloadId: wid})
			if er != nil {
				return er
			}
			op, conf, task = r.GetOperation(), r.GetConfirmation(), r.GetTaskId()
		} else {
			r, er := agentv1.NewWorkloadServiceClient(conn).RequestApply(cc, &agentv1.WorkloadServiceRequestApplyRequest{IdempotencyKey: id, PlanId: plan, WorkloadId: wid})
			if er != nil {
				return er
			}
			op, conf, task = r.GetOperation(), r.GetConfirmation(), r.GetTaskId()
		}
		return nil
	})
	if err != nil {
		return nil, c.controlActionError(err, "workload")
	}
	if e := validateWorkloadMutationResponse(op, conf, task, plan, wid, destroy); e != nil {
		return nil, e
	}
	return map[string]any{"operation": operationMap(op), "confirmation": confirmationMap(conf), "task_id": task}, nil
}
func (c *Client) workloadApply(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	return c.workloadMutation(ctx, p, false)
}
func (c *Client) workloadDestroy(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	return c.workloadMutation(ctx, p, true)
}
func (c *Client) workloadOperationGet(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "operation_id"); e != nil {
		return nil, e
	}
	if e := c.workloadGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "operation_id")
	if e != nil {
		return nil, e
	}
	var response *agentv1.WorkloadServiceGetOperationResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var callErr error
		response, callErr = agentv1.NewWorkloadServiceClient(conn).GetOperation(cc, &agentv1.WorkloadServiceGetOperationRequest{OperationId: id})
		return callErr
	})
	if err != nil {
		return nil, c.controlActionError(err, "workload operation")
	}
	if e := ensureResponse(response, "workload operation"); e != nil {
		return nil, e
	}
	if e := validateWorkloadOperationReadback(response.GetOperation(), id); e != nil {
		return nil, e
	}
	return map[string]any{"operation": operationMap(response.GetOperation())}, nil
}
func (c *Client) workloadOperationEvents(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "operation_id", "after_sequence"); e != nil {
		return nil, e
	}
	if e := c.workloadGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "operation_id")
	if e != nil {
		return nil, e
	}
	after, e := optionalInt64(p, "after_sequence")
	if e != nil {
		return nil, e
	}
	if after < 0 {
		return nil, actionbase.BadRequest("after_sequence must be nonnegative")
	}
	var response *agentv1.WorkloadServiceListEventsResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var callErr error
		response, callErr = agentv1.NewWorkloadServiceClient(conn).ListEvents(cc, &agentv1.WorkloadServiceListEventsRequest{OperationId: id, AfterSequence: uint64(after)})
		return callErr
	})
	if err != nil {
		return nil, c.controlActionError(err, "workload operation events")
	}
	if e := ensureResponse(response, "workload operation events"); e != nil {
		return nil, e
	}
	previous := after
	events := make([]any, 0, len(response.GetEvents()))
	for _, event := range response.GetEvents() {
		if event == nil || event.GetOperationId() != id || event.GetSequence() <= previous || strings.TrimSpace(event.GetKind()) == "" || !validWorkloadOperationStatus(event.GetStatus()) || event.GetAt() == nil || !event.GetAt().IsValid() {
			return nil, upstreamInvalid("workload operation events")
		}
		if actual := event.GetActual(); actual != nil {
			if e := validateSparseEventActual(actual); e != nil {
				return nil, upstreamInvalid("workload operation events")
			}
		}
		events = append(events, map[string]any{"operation_id": event.GetOperationId(), "sequence": event.GetSequence(), "kind": safeText(event.GetKind()), "status": safeText(event.GetStatus()), "message": safeText(event.GetMessage()), "actual": sparseEventActualMap(event.GetActual()), "at": timestampMap(event.GetAt())})
		previous = event.GetSequence()
	}
	return map[string]any{"events": events}, nil
}
func (c *Client) workloadActualGet(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "workload_id"); e != nil {
		return nil, e
	}
	if e := c.workloadGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "workload_id")
	if e != nil {
		return nil, e
	}
	var response *agentv1.WorkloadServiceGetWorkloadResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var callErr error
		response, callErr = agentv1.NewWorkloadServiceClient(conn).GetWorkload(cc, &agentv1.WorkloadServiceGetWorkloadRequest{WorkloadId: id})
		return callErr
	})
	if err != nil {
		return nil, c.controlActionError(err, "workload")
	}
	if e := ensureResponse(response, "workload"); e != nil {
		return nil, e
	}
	if e := validateWorkloadActualReadback(response.GetWorkload(), id); e != nil {
		return nil, e
	}
	return map[string]any{"workload": actualMap(response.GetWorkload())}, nil
}

func (c *Client) awsHandlers() map[string]actionbase.Handler {
	return map[string]actionbase.Handler{"agent.core.aws.credentials.create": c.awsCredentialCreate, "agent.core.aws.credentials.update": c.awsCredentialUpdate, "agent.core.aws.credentials.delete": c.awsCredentialDelete, "agent.core.aws.credentials.list": c.awsCredentialList, "agent.core.aws.credentials.test": c.awsCredentialTest, "agent.core.aws.plans.get": c.awsPlanGet, "agent.core.aws.plans.list": c.awsPlanList, "agent.core.aws.plans.quote": c.awsPlanQuote, "agent.core.aws.changes.get": c.awsChangeGet, "agent.core.aws.changes.list": c.awsChangeList, "agent.core.aws.changes.status": c.awsChangeStatus}
}
func (c *Client) awsCredentialCreate(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "idempotency_key", "name", "region", "access_key_id", "secret_access_key", "session_token"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "idempotency_key")
	if e != nil {
		return nil, e
	}
	name, e := requiredString(p, "name")
	if e != nil {
		return nil, e
	}
	region, e := requiredString(p, "region")
	if e != nil {
		return nil, e
	}
	ak, e := requiredString(p, "access_key_id")
	if e != nil {
		return nil, e
	}
	sk, e := requiredString(p, "secret_access_key")
	if e != nil {
		return nil, e
	}
	st, e := optionalString(p, "session_token")
	if e != nil {
		return nil, e
	}
	var r *agentv1.CoreCloudControlServiceCreateCredentialResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewCoreCloudControlServiceClient(conn).CreateCredential(cc, &agentv1.CoreCloudControlServiceCreateCredentialRequest{IdempotencyKey: id, Name: name, Region: region, AccessKeyId: ak, SecretAccessKey: sk, SessionToken: st})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS credential")
	}
	if e := ensureResponse(r, "AWS credential"); e != nil {
		return nil, e
	}
	if e := validateCredentialReadback(r.GetCredential(), "", name, region, ak != "", sk != "", st != "", 0); e != nil {
		return nil, e
	}
	return map[string]any{"credential": awsCredentialMap(r.GetCredential())}, nil
}
func (c *Client) awsCredentialUpdate(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "idempotency_key", "credential_id", "expected_revision", "name", "region", "access_key_id", "secret_access_key", "session_token"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	ik, e := requiredUUID(p, "idempotency_key")
	if e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "credential_id")
	if e != nil {
		return nil, e
	}
	rev, e := optionalInt64(p, "expected_revision")
	if e != nil || rev < 1 {
		return nil, actionbase.BadRequest("expected_revision is required")
	}
	name, e := optionalString(p, "name")
	if e != nil {
		return nil, e
	}
	region, e := optionalString(p, "region")
	if e != nil {
		return nil, e
	}
	ak, e := optionalString(p, "access_key_id")
	if e != nil {
		return nil, e
	}
	sk, e := optionalString(p, "secret_access_key")
	if e != nil {
		return nil, e
	}
	st, e := optionalString(p, "session_token")
	if e != nil {
		return nil, e
	}
	var r *agentv1.CoreCloudControlServiceUpdateCredentialResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewCoreCloudControlServiceClient(conn).UpdateCredential(cc, &agentv1.CoreCloudControlServiceUpdateCredentialRequest{IdempotencyKey: ik, CredentialId: id, ExpectedRevision: rev, Name: name, Region: region, AccessKeyId: ak, SecretAccessKey: sk, SessionToken: st})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS credential")
	}
	if e := ensureResponse(r, "AWS credential"); e != nil {
		return nil, e
	}
	if e := validateCredentialReadback(r.GetCredential(), id, name, region, ak != "", sk != "", st != "", rev); e != nil {
		return nil, e
	}
	return map[string]any{"credential": awsCredentialMap(r.GetCredential())}, nil
}
func (c *Client) awsCredentialDelete(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "idempotency_key", "credential_id", "expected_revision"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	ik, e := requiredUUID(p, "idempotency_key")
	if e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "credential_id")
	if e != nil {
		return nil, e
	}
	rev, e := optionalInt64(p, "expected_revision")
	if e != nil || rev < 1 {
		return nil, actionbase.BadRequest("expected_revision is required")
	}
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		_, er := agentv1.NewCoreCloudControlServiceClient(conn).DeleteCredential(cc, &agentv1.CoreCloudControlServiceDeleteCredentialRequest{IdempotencyKey: ik, CredentialId: id, ExpectedRevision: rev})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS credential")
	}
	return map[string]any{"deleted": true, "credential_id": id}, nil
}
func (c *Client) awsCredentialList(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "page_size", "page_token"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	sz, t, e := parsePage(p)
	if e != nil {
		return nil, e
	}
	var r *agentv1.CoreCloudControlServiceListCredentialsResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewCoreCloudControlServiceClient(conn).ListCredentials(cc, &agentv1.CoreCloudControlServiceListCredentialsRequest{PageSize: sz, PageToken: t})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS credential")
	}
	if e := ensureResponse(r, "AWS credential list"); e != nil {
		return nil, e
	}
	if e := checkNextToken(r.GetNextPageToken()); e != nil {
		return nil, e
	}
	for _, v := range r.GetCredentials() {
		if e := validateCredentialResponse(v, ""); e != nil {
			return nil, e
		}
	}
	out := []any{}
	for _, v := range r.GetCredentials() {
		out = append(out, awsCredentialMap(v))
	}
	return map[string]any{"credentials": out, "next_page_token": r.GetNextPageToken()}, nil
}
func (c *Client) awsCredentialTest(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "credential_id"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "credential_id")
	if e != nil {
		return nil, e
	}
	var r *agentv1.CoreCloudControlServiceTestCredentialIdentityResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewCoreCloudControlServiceClient(conn).TestCredentialIdentity(cc, &agentv1.CoreCloudControlServiceTestCredentialIdentityRequest{CredentialId: id})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS credential")
	}
	if e := ensureResponse(r, "AWS credential test"); e != nil {
		return nil, e
	}
	if e := checkRespUUID(r.GetCredentialId(), id, "AWS credential test"); e != nil {
		return nil, e
	}
	if e := validateCredentialIdentityResponse(r.GetCredentialId(), r.GetAccountId(), r.GetUserArn(), r.GetPrincipalId(), r.GetCredentialRevision(), r.GetTestedAt()); e != nil {
		return nil, e
	}
	return map[string]any{"credential_id": r.GetCredentialId(), "account_id": r.GetAccountId(), "user_arn": r.GetUserArn(), "principal_id": r.GetPrincipalId(), "credential_revision": r.GetCredentialRevision(), "tested_at": timestampMap(r.GetTestedAt())}, nil
}
func (c *Client) awsPlanGet(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "plan_id"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "plan_id")
	if e != nil {
		return nil, e
	}
	var r *agentv1.CoreCloudControlServiceGetPlanResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewCoreCloudControlServiceClient(conn).GetPlan(cc, &agentv1.CoreCloudControlServiceGetPlanRequest{PlanId: id})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS plan")
	}
	if e := ensureResponse(r, "AWS plan"); e != nil {
		return nil, e
	}
	if e := validateAWSPlanResponse(r.GetPlan(), id); e != nil {
		return nil, e
	}
	return map[string]any{"plan": awsPlanMap(r.GetPlan())}, nil
}
func (c *Client) awsPlanList(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "page_size", "page_token"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	sz, t, e := parsePage(p)
	if e != nil {
		return nil, e
	}
	var r *agentv1.CoreCloudControlServiceListPlansResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewCoreCloudControlServiceClient(conn).ListPlans(cc, &agentv1.CoreCloudControlServiceListPlansRequest{PageSize: sz, PageToken: t})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS plan")
	}
	if e := ensureResponse(r, "AWS plan list"); e != nil {
		return nil, e
	}
	if e := checkNextToken(r.GetNextPageToken()); e != nil {
		return nil, e
	}
	out := []any{}
	for _, v := range r.GetPlans() {
		if e := validateAWSPlanResponse(v, ""); e != nil {
			return nil, e
		}
		out = append(out, awsPlanMap(v))
	}
	return map[string]any{"plans": out, "next_page_token": r.GetNextPageToken()}, nil
}
func (c *Client) awsPlanQuote(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "plan_id"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "plan_id")
	if e != nil {
		return nil, e
	}
	var r *agentv1.CoreCloudControlServiceQuoteResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewCoreCloudControlServiceClient(conn).Quote(cc, &agentv1.CoreCloudControlServiceQuoteRequest{PlanId: id})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS plan")
	}
	if e := ensureResponse(r, "AWS quote"); e != nil {
		return nil, e
	}
	if e := validateAWSQuote(r.GetQuote(), id); e != nil {
		return nil, e
	}
	return map[string]any{"quote": awsQuoteMap(r.GetQuote())}, nil
}
func (c *Client) awsChangeGet(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "change_id"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "change_id")
	if e != nil {
		return nil, e
	}
	var r *agentv1.CoreCloudControlServiceGetChangeResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewCoreCloudControlServiceClient(conn).GetChange(cc, &agentv1.CoreCloudControlServiceGetChangeRequest{ChangeId: id})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS change")
	}
	if e := ensureResponse(r, "AWS change"); e != nil {
		return nil, e
	}
	if e := validateAWSChangeResponse(r.GetChange(), id); e != nil {
		return nil, e
	}
	return map[string]any{"change": awsChangeMap(r.GetChange())}, nil
}
func (c *Client) awsChangeList(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "page_size", "page_token", "plan_id"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	sz, t, e := parsePage(p)
	if e != nil {
		return nil, e
	}
	plan, e := optionalString(p, "plan_id")
	if e != nil {
		return nil, e
	}
	if plan != "" && !validRespUUID(plan) {
		return nil, actionbase.BadRequest("plan_id must be a UUID")
	}
	var r *agentv1.CoreCloudControlServiceListChangesResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewCoreCloudControlServiceClient(conn).ListChanges(cc, &agentv1.CoreCloudControlServiceListChangesRequest{PageSize: sz, PageToken: t, PlanId: plan})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS change")
	}
	if e := ensureResponse(r, "AWS change list"); e != nil {
		return nil, e
	}
	if e := checkNextToken(r.GetNextPageToken()); e != nil {
		return nil, e
	}
	for _, v := range r.GetChanges() {
		if e := validateAWSChangeResponse(v, "", plan); e != nil {
			return nil, e
		}
	}
	out := []any{}
	for _, v := range r.GetChanges() {
		out = append(out, awsChangeMap(v))
	}
	return map[string]any{"changes": out, "next_page_token": r.GetNextPageToken()}, nil
}
func (c *Client) awsChangeStatus(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	if e := strictKeys(p, "change_id"); e != nil {
		return nil, e
	}
	if e := c.awsGate(ctx); e != nil {
		return nil, e
	}
	id, e := requiredUUID(p, "change_id")
	if e != nil {
		return nil, e
	}
	var r *agentv1.CoreCloudControlServiceGetChangeStatusResponse
	err := c.controlPlaneUnary(ctx, func(cc context.Context, conn *grpc.ClientConn) error {
		var er error
		r, er = agentv1.NewCoreCloudControlServiceClient(conn).GetChangeStatus(cc, &agentv1.CoreCloudControlServiceGetChangeStatusRequest{ChangeId: id})
		return er
	})
	if err != nil {
		return nil, c.controlActionError(err, "AWS change")
	}
	if e := ensureResponse(r, "AWS change status"); e != nil {
		return nil, e
	}
	if e := validateAWSChangeResponse(r.GetChange(), id); e != nil || r.GetStatus() != r.GetChange().GetStatus() || r.GetStage() != r.GetChange().GetStage() {
		return nil, upstreamInvalid("AWS change status")
	}
	return map[string]any{"change": awsChangeMap(r.GetChange()), "status": safeText(r.GetStatus()), "stage": safeText(r.GetStage())}, nil
}
