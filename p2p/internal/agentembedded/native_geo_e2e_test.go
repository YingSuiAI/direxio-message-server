package agentembedded

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	coreagent "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/cloudwego/eino/schema"
	"github.com/google/uuid"
)

func TestGeoLibreProjectionRedactionAndNativeReferenceE2E(t *testing.T) {
	plan, provision, _ := geoPlanFixture(t)
	workloadID := uuid.NewString()
	taskID, confirmationID, operationID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	digest := strings.Repeat("d", 64)
	aggregate := coreworkload.SecretGrantAggregateDigestForTypedRefs(plan.SecretGrantRefs)
	projectedPlan := GeoLibrePlanProjection(plan)
	result := map[string]any{
		"provision_id": provision.ID, "provision_revision": provision.Revision, "expected_workload_revision": 1,
		"plan": projectedPlan,
		"operation": map[string]any{
			"operation_id": operationID, "workload_id": workloadID, "plan_id": plan.ID, "task_id": taskID, "confirmation_id": confirmationID,
			"kind": "apply", "revision": 1, "expected_workload_revision": 1, "plan_revision": plan.Revision, "plan_digest": plan.Digest, "target_kind": "aws-ec2-ssm", "summary": "Install GeoLibre",
			"secret_grant_refs": []any{map[string]any{"reference_id": plan.SecretGrantRefs[0].ReferenceID, "purpose": "aws_credential", "secret_revision": plan.SecretGrantRefs[0].Revision, "binding_digest": string(plan.SecretGrantRefs[0].BindingDigest)}},
		},
		"confirmation": map[string]any{
			"confirmation_id": confirmationID, "task_id": taskID, "state": "pending", "revision": 1, "expires_at": plan.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
			"binding": map[string]any{
				"operation_domain": "workload:apply", "target_id": workloadID, "target_revision": plan.Revision,
				"content_digest": plan.Digest, "parameter_digest": digest, "network_digest": digest, "secret_grant_digest": aggregate,
				"secret_grants": []any{map[string]any{"reference_id": plan.SecretGrantRefs[0].ReferenceID, "purpose": "aws_credential", "secret_revision": plan.SecretGrantRefs[0].Revision, "binding_digest": string(plan.SecretGrantRefs[0].BindingDigest)}},
			},
		},
		"owner_id": "owner-must-not-persist", "selected_command": []string{"rm -rf /"}, "image_uri": "private-image", "desired_plan": map[string]any{"labels": map[string]string{"secret": "bytes"}},
	}
	redacted := coreagent.RedactNativeControlResult(result)
	encoded, err := json.Marshal(map[string]any{"result": redacted})
	if err != nil {
		t.Fatal(err)
	}
	serialized := fmt.Sprint(redacted)
	for _, forbidden := range []string{"owner-must-not-persist", "rm -rf /", "private-image", "bytes", "labels", "selected_command"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("redaction retained %q: %s", forbidden, serialized)
		}
	}
	references := nativeagent.ReferencesFromToolMessages([]*schema.Message{schema.ToolMessage(string(encoded), "call_geo", schema.WithToolName("native_agent_aws_ec2_geolibre_install_request"))})
	if len(references) != 1 {
		t.Fatalf("references = %#v", references)
	}
	card := references[0]
	for key, want := range map[string]any{"provision_id": provision.ID, "provision_revision": int64(provision.Revision), "expected_workload_revision": int64(1), "credential_id": plan.SecretGrantRefs[0].ReferenceID, "credential_revision": plan.SecretGrantRefs[0].Revision, "target_id": workloadID, "plan_digest": plan.Digest} {
		if card[key] != want {
			t.Fatalf("card.%s = %#v, want %#v; full=%#v", key, card[key], want, card)
		}
	}
	encodedEnvelope, err := json.Marshal(map[string]any{"result": redacted})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(map[string]any){
		"top-level-zero": func(envelope map[string]any) {
			envelope["result"].(map[string]any)["expected_workload_revision"] = 0
		},
		"operation-zero": func(envelope map[string]any) {
			envelope["result"].(map[string]any)["operation"].(map[string]any)["expected_workload_revision"] = 0
		},
		"operation-drift": func(envelope map[string]any) {
			envelope["result"].(map[string]any)["operation"].(map[string]any)["expected_workload_revision"] = 2
		},
	} {
		var candidate map[string]any
		if err := json.Unmarshal(encodedEnvelope, &candidate); err != nil {
			t.Fatal(err)
		}
		mutate(candidate)
		encodedCandidate, err := json.Marshal(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if got := nativeagent.ReferencesFromToolMessages([]*schema.Message{schema.ToolMessage(string(encodedCandidate), "call_geo", schema.WithToolName("native_agent_aws_ec2_geolibre_install_request"))}); len(got) != 0 {
			t.Fatalf("%s accepted stale workload fence: %#v", name, got)
		}
	}
}

func TestEmbeddedEC2ChangeProjectionFeedsNativeReference(t *testing.T) {
	now := time.Now().UTC()
	credentialID := uuid.NewString()
	provisionID, planID := uuid.NewString(), uuid.NewString()
	taskID, confirmationID, changeID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	targetID := "aws-target:" + strings.Repeat("a", 64)
	content, parameter, network, aggregate, grantDigest := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64)
	binding := coreconfirmation.Binding{
		OperationDomain: "aws", TargetID: targetID, TargetRevision: 1,
		ContentDigest: coreconfirmation.Digest(content), ParameterDigest: coreconfirmation.Digest(parameter),
		NetworkDigest: coreconfirmation.Digest(network), SecretGrantDigest: coreconfirmation.Digest(aggregate),
		SecretGrants: []coreconfirmation.SecretGrant{{ReferenceID: credentialID, Purpose: coreconfirmation.SecretPurposeAWSCredential, Revision: 4, BindingDigest: coreconfirmation.Digest(grantDigest)}},
	}
	projected := changeRequestMap(coreaws.ChangeRequestResult{
		Change:       coreaws.Change{ID: changeID, PlanID: planID, ProvisionID: provisionID, TaskID: taskID, ConfirmationID: confirmationID, Operation: coreaws.OperationCreate, Status: coreaws.ChangeWaitingUser, Revision: 2},
		Task:         coreaws.Task{ID: taskID, PlanID: planID, ConfirmationID: confirmationID, Status: "waiting_user", Revision: 1},
		Confirmation: coreconfirmation.Confirmation{ConfirmationID: confirmationID, TaskID: taskID, State: coreconfirmation.StatePending, Revision: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Binding: binding},
		Provision:    coreaws.Provision{ID: provisionID, PlanID: planID, CredentialID: credentialID, CredentialRevision: 4, PlanRevision: 1, TemplateSHA256: content, PlanDigest: content, Revision: 2, CreatedAt: now, UpdatedAt: now},
	})
	encoded, err := json.Marshal(map[string]any{"result": projected})
	if err != nil {
		t.Fatal(err)
	}
	references := nativeagent.ReferencesFromToolMessages([]*schema.Message{schema.ToolMessage(string(encoded), "call_ec2", schema.WithToolName("native_agent_aws_ec2_provisions_create_request"))})
	if len(references) != 1 || references[0]["credential_id"] != credentialID {
		t.Fatalf("EC2 producer/reference projection = %#v", references)
	}
}

func TestEmbeddedEC2DestroyChangeProjectionFeedsNativeReference(t *testing.T) {
	now := time.Now().UTC()
	credentialID := uuid.NewString()
	provisionID, originalPlanID := uuid.NewString(), uuid.NewString()
	deletePlanID := uuid.NewSHA1(uuid.Nil, []byte("ec2-destroy:"+provisionID)).String()
	taskID, confirmationID, changeID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	targetID := "aws-target:" + strings.Repeat("a", 64)
	content, parameter, network, aggregate, grantDigest := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64)
	binding := coreconfirmation.Binding{
		OperationDomain: "aws", TargetID: targetID, TargetRevision: 1,
		ContentDigest: coreconfirmation.Digest(content), ParameterDigest: coreconfirmation.Digest(parameter),
		NetworkDigest: coreconfirmation.Digest(network), SecretGrantDigest: coreconfirmation.Digest(aggregate),
		SecretGrants: []coreconfirmation.SecretGrant{{ReferenceID: credentialID, Purpose: coreconfirmation.SecretPurposeAWSCredential, Revision: 4, BindingDigest: coreconfirmation.Digest(grantDigest)}},
	}
	projected := changeRequestMap(coreaws.ChangeRequestResult{
		Change:       coreaws.Change{ID: changeID, PlanID: deletePlanID, ProvisionID: provisionID, TaskID: taskID, ConfirmationID: confirmationID, Operation: coreaws.OperationDelete, Status: coreaws.ChangeWaitingUser, Revision: 2},
		Task:         coreaws.Task{ID: taskID, PlanID: deletePlanID, ConfirmationID: confirmationID, Status: "waiting_user", Revision: 1},
		Confirmation: coreconfirmation.Confirmation{ConfirmationID: confirmationID, TaskID: taskID, State: coreconfirmation.StatePending, Revision: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(time.Hour), Binding: binding},
		Provision:    coreaws.Provision{ID: provisionID, PlanID: originalPlanID, CredentialID: credentialID, CredentialRevision: 4, PlanRevision: 1, TemplateSHA256: content, PlanDigest: content, Revision: 2, CreatedAt: now, UpdatedAt: now},
	})
	encoded, err := json.Marshal(map[string]any{"result": projected})
	if err != nil {
		t.Fatal(err)
	}
	references := nativeagent.ReferencesFromToolMessages([]*schema.Message{schema.ToolMessage(string(encoded), "call_ec2_destroy", schema.WithToolName("native_agent_aws_ec2_provisions_destroy_request"))})
	if len(references) != 1 || references[0]["action"] != "destroy" || references[0]["plan_id"] != deletePlanID || references[0]["provision_id"] != provisionID {
		t.Fatalf("EC2 destroy producer/reference projection = %#v", references)
	}
}
