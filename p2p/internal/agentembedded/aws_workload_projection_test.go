package agentembedded

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

func TestEmbeddedWorkloadProjectionPreservesWireContractAndSecretRevision(t *testing.T) {
	now := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	digest := strings.Repeat("a", 64)
	credentialID := "11111111-1111-4111-8111-111111111111"
	plan := coreworkload.Plan{
		ID:         "22222222-2222-4222-8222-222222222222",
		Revision:   3,
		Digest:     digest,
		Summary:    "ssm",
		Artifact:   "artifact",
		Source:     "test",
		TargetKind: coreworkload.TargetAWSEC2SSM,
		Target: coreworkload.TargetSettings{
			Identity: coreworkload.TargetIdentity{
				Kind: coreworkload.TargetAWSEC2SSM, AccountID: "123456789012",
				Region: "us-east-1", InstanceID: "i-0123456789abcdef0",
			},
			PortDetails:         []coreworkload.Port{{Port: 443}},
			NetworkGrantDetails: []coreworkload.NetworkGrant{{ReferenceID: "network-1", Kind: "https"}},
			Labels:              map[string]string{"env": "test"},
			EC2DocumentVersion:  "1",
			EC2SystemdService:   "dirextalk.service",
			RequiredInstanceTags: map[string]string{
				"DirextalkManaged": "true",
			},
		},
		SecretGrantRefs: []coreworkload.SecretGrantRef{{
			ReferenceID: credentialID, Purpose: coreconfirmation.SecretPurposeAWSCredential,
			Revision: 7, BindingDigest: coreconfirmation.Digest(digest),
		}},
		ResourceLimits: coreworkload.ResourceLimits{TimeoutS: 60},
		ExpiresAt:      now.Add(time.Hour),
		CreatedAt:      now,
	}

	projected := workloadPlanMap(plan)
	if projected["target_kind"] != "aws-ec2-ssm" {
		t.Fatalf("target_kind = %v", projected["target_kind"])
	}
	target := projected["typed_target"].(map[string]any)
	identity := target["identity"].(map[string]any)
	if identity["kind"] != "aws-ec2-ssm" || identity["aws_ec2_document_version"] != "1" {
		t.Fatalf("identity = %#v", identity)
	}
	grants := projected["typed_secret_grants"].([]any)
	grant := grants[0].(map[string]any)
	if grant["secret_revision"] != int64(7) || grant["reference_id"] != credentialID {
		t.Fatalf("secret grant = %#v", grant)
	}

	actual := coreworkload.ActualSnapshot{
		WorkloadID: "33333333-3333-4333-8333-333333333333", Revision: 2,
		State: "ready", Identity: plan.Target.Identity, AppliedPlanID: plan.ID,
		AppliedPlanDigest: digest, ReadbackDigest: digest, ProviderVersion: "ssm-v1",
		ObservedAt: now, UpdatedAt: now,
	}
	operation := coreworkload.Operation{
		ID: "44444444-4444-4444-8444-444444444444", WorkloadID: actual.WorkloadID,
		PlanID: plan.ID, Kind: coreworkload.OperationApply, PlanRevision: plan.Revision,
		PlanDigest: digest, TargetKind: plan.TargetKind,
		TaskID:         "55555555-5555-4555-8555-555555555555",
		ConfirmationID: "66666666-6666-4666-8666-666666666666",
		Status:         coreworkload.OperationWaitingUser, Revision: 1,
		CreatedAt: now, UpdatedAt: now, DispatchLeaseUntil: now.Add(time.Minute),
	}
	operationProjection := workloadOperationMap(operation, plan, &actual)
	if operationProjection["desired_plan"] == nil || operationProjection["actual"] == nil {
		t.Fatalf("operation projection lost readbacks: %#v", operationProjection)
	}

	confirmationProjection := confirmationMap(coreconfirmation.Confirmation{
		ConfirmationID: operation.ConfirmationID, TaskID: operation.TaskID,
		State: coreconfirmation.StatePending, Revision: 1,
		CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour),
		Binding: coreconfirmation.Binding{SecretGrants: []coreconfirmation.SecretGrant{{
			ReferenceID: credentialID, Purpose: coreconfirmation.SecretPurposeAWSCredential,
			Revision: 7, BindingDigest: coreconfirmation.Digest(digest),
		}}},
	})
	binding := confirmationProjection["binding"].(map[string]any)
	confirmationGrant := binding["secret_grants"].([]any)[0].(map[string]any)
	if confirmationGrant["secret_revision"] != int64(7) {
		t.Fatalf("confirmation grant = %#v", confirmationGrant)
	}
}

func TestEmbeddedWorkloadEventProjectsSparseReadback(t *testing.T) {
	now := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	digest := strings.Repeat("b", 64)
	raw, err := json.Marshal(coreworkload.Readback{
		TargetKind: coreworkload.TargetAWSECS,
		WorkloadID: "33333333-3333-4333-8333-333333333333",
		State:      "ready",
		Identity: coreworkload.TargetIdentity{
			Kind: coreworkload.TargetAWSECS, AccountID: "123456789012",
			Region: "us-east-1", Cluster: "cluster", Service: "service",
			TaskDefinitionRevision: "1",
		},
		ProviderVersion: "ecs-v1", Digest: digest, At: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	projected, err := workloadEventMap(coreworkload.Event{
		OperationID: "44444444-4444-4444-8444-444444444444",
		Sequence:    1, Kind: "readback", Status: coreworkload.OperationRunning,
		Readback: raw, At: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	actual := projected["actual"].(map[string]any)
	if actual["readback_digest"] != digest ||
		actual["identity"].(map[string]any)["kind"] != "aws-ecs" {
		t.Fatalf("actual = %#v", actual)
	}
}

func TestEmbeddedAWSPlanProjectionIncludesCredentialRevision(t *testing.T) {
	projected := planViewMap(coreaws.PlanView{ID: "plan", CredentialID: "credential", CredentialRevision: 9})
	if projected["credential_revision"] != int64(9) {
		t.Fatalf("credential_revision = %v", projected["credential_revision"])
	}
}
