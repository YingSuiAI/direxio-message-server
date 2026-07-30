package agentembedded

import (
	"net/http"
	"strings"
	"testing"

	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

func TestDecodeTargetSettingsPreservesTypedAWSProviderFields(t *testing.T) {
	target, apiErr := decodeTargetSettings(map[string]any{
		"identity": map[string]any{
			"kind":                       "aws-ecs",
			"aws_account_id":             "123456789012",
			"aws_region":                 "us-east-1",
			"cluster":                    "arn:aws:ecs:us-east-1:123456789012:cluster/app",
			"service":                    "app",
			"task_definition_revision":   "7",
			"aws_ecs_cluster_arn":        "arn:aws:ecs:us-east-1:123456789012:cluster/app",
			"aws_ecs_service_name":       "app",
			"aws_ecs_task_family":        "app",
			"aws_ecs_platform_version":   "1.4.0",
			"aws_ecs_subnet_ids":         []any{"subnet-a", "subnet-b"},
			"aws_ecs_security_group_ids": []any{"sg-a"},
			"aws_ecs_assign_public_ip":   true,
			"aws_ecs_desired_count":      int64(2),
			"aws_ecs_target_group_arn":   "arn:aws:elasticloadbalancing:us-east-1:123456789012:targetgroup/app/abc",
			"aws_ecs_target_group_port":  int64(8080),
			"aws_ecs_task_role_arn":      "arn:aws:iam::123456789012:role/task",
			"aws_ecs_execution_role_arn": "arn:aws:iam::123456789012:role/execution",
			"aws_ecs_image_uri":          "registry.example/app@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
		"ports":          []any{map[string]any{"port": int64(8080)}},
		"network_grants": []any{map[string]any{"reference_id": "vpc", "kind": "security_group"}},
		"labels":         map[string]any{"env": "test"},
	}, coreworkload.TargetAWSECS)
	if apiErr != nil {
		t.Fatalf("decode target: %#v", apiErr)
	}
	if target.AccountID != "123456789012" || target.Region != "us-east-1" || target.ECSDesiredCount != 2 || !target.ECSAssignPublicIP || len(target.ECSSubnetIDs) != 2 || target.ECSTargetGroupPort != 8080 {
		t.Fatalf("provider target fields were lost: %#v", target)
	}
	if len(target.PortDetails) != 1 || target.PortDetails[0].Port != 8080 || len(target.NetworkGrantDetails) != 1 || target.NetworkGrantDetails[0].Kind != "security_group" {
		t.Fatalf("ports/network grants were lost: %#v", target)
	}
}

func TestWorkloadPlanAllowsUnboundAWSCredentialDigestForStorePinning(t *testing.T) {
	plan, apiErr := workloadPlanInput(map[string]any{
		"idempotency_key": "00000000-0000-4000-8000-000000000001",
		"summary":         "aws workload",
		"artifact":        "artifact",
		"source":          "test",
		"target_kind":     "aws-ec2-ssm",
		"expires_at":      "2099-01-01T00:00:00Z",
		"typed_target": map[string]any{
			"identity": map[string]any{
				"kind":                           "aws-ec2-ssm",
				"aws_account_id":                 "123456789012",
				"aws_region":                     "us-east-1",
				"instance_id":                    "i-0123456789abcdef0",
				"aws_ec2_document_version":       "1",
				"aws_ec2_systemd_service":        "dirextalk-agent.service",
				"aws_ec2_required_instance_tags": map[string]any{"managed": "true"},
			},
		},
		"typed_secret_grants": []any{map[string]any{
			"reference_id":    "00000000-0000-4000-8000-000000000002",
			"purpose":         "aws_credential",
			"secret_revision": int64(3),
		}},
	}, true)
	if apiErr != nil {
		t.Fatalf("AWS credential grant without digest rejected: %#v", apiErr)
	}
	if len(plan.SecretGrantRefs) != 1 || plan.SecretGrantRefs[0].BindingDigest != coreconfirmation.Digest(strings.Repeat("0", 64)) {
		t.Fatalf("temporary digest = %#v", plan.SecretGrantRefs)
	}
}

func TestWorkloadPlanRejectsUnboundAWSCredentialWithoutPinningStore(t *testing.T) {
	base := map[string]any{
		"idempotency_key": "00000000-0000-4000-8000-000000000001",
		"summary":         "aws workload",
		"artifact":        "artifact",
		"source":          "test",
		"target_kind":     "aws-ec2-ssm",
		"expires_at":      "2099-01-01T00:00:00Z",
		"typed_target": map[string]any{
			"identity": map[string]any{
				"aws_account_id": "123456789012",
				"aws_region":     "us-east-1",
				"instance_id":    "i-0123456789abcdef0",
			},
		},
		"typed_secret_grants": []any{map[string]any{
			"reference_id":    "00000000-0000-4000-8000-000000000002",
			"purpose":         "aws_credential",
			"secret_revision": int64(3),
		}},
	}
	_, apiErr := workloadPlanInput(base, false)
	if apiErr == nil || apiErr.Status != http.StatusPreconditionFailed || apiErr.Code != "agent_embedded_unavailable" {
		t.Fatalf("unbound AWS grant without pinning store = %#v", apiErr)
	}
	base["typed_secret_grants"] = []any{map[string]any{
		"reference_id":    "00000000-0000-4000-8000-000000000002",
		"purpose":         "aws_credential",
		"secret_revision": int64(3),
		"binding_digest":  strings.Repeat("a", 64),
	}}
	_, apiErr = workloadPlanInput(base, false)
	if apiErr == nil || apiErr.Status != http.StatusPreconditionFailed || apiErr.Code != "agent_embedded_unavailable" {
		t.Fatalf("caller-bound AWS grant without pinning store = %#v", apiErr)
	}
}
