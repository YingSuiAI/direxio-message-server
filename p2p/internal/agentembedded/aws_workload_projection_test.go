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
		NetworkGrants: []string{"security-group:sg-0123456789abcdef0"},
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

	// Apply and destroy must expose the exact immutable binding used by the
	// confirmation created for the same plan/operation. This projection is
	// deliberately digest/reference-only; no credential material is allowed.
	for _, kind := range []coreworkload.OperationKind{coreworkload.OperationApply, coreworkload.OperationDestroy} {
		operation.Kind = kind
		projected := workloadOperationMap(operation, plan, &actual)
		binding := coreworkload.BindingForOperation(plan, operation.WorkloadID, kind)
		for key, want := range map[string]any{
			"target_id":           binding.TargetID,
			"target_revision":     binding.TargetRevision,
			"content_digest":      string(binding.ContentDigest),
			"parameter_digest":    string(binding.ParameterDigest),
			"network_digest":      string(binding.NetworkDigest),
			"secret_grant_digest": string(binding.SecretGrantDigest),
			"network_grants":      binding.NetworkGrants,
		} {
			if got := projected[key]; !equalProjectionValue(got, want) {
				t.Fatalf("%s binding %s = %#v, want %#v", kind, key, got, want)
			}
		}
		encoded, err := json.Marshal(projected)
		if err != nil {
			t.Fatal(err)
		}
		serialized := string(encoded)
		for _, forbidden := range []string{"secret_access_key", "access_key_id", "session_token", "super-secret-value"} {
			if strings.Contains(serialized, forbidden) {
				t.Fatalf("%s projection leaked %q: %s", kind, forbidden, serialized)
			}
		}
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

func TestEmbeddedConfirmationProjectionEmitsCanonicalArrayFields(t *testing.T) {
	for name, binding := range map[string]coreconfirmation.Binding{
		"empty": {},
		"non_empty": {
			NetworkGrants:   []string{"security-group:sg-0123456789abcdef0", "network:subnet-0123456789abcdef0"},
			SelectedCommand: []string{"echo one", "echo two"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			projected := confirmationMap(coreconfirmation.Confirmation{ConfirmationID: "11111111-1111-4111-8111-111111111111", Binding: binding})
			encoded, err := json.Marshal(projected)
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				Binding struct {
					NetworkGrants   []string `json:"network_grants"`
					SelectedCommand []string `json:"selected_command"`
				} `json:"binding"`
			}
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.Binding.NetworkGrants == nil || wire.Binding.SelectedCommand == nil {
				t.Fatalf("array fields must not be null: %s", encoded)
			}
			expectedGrants := append([]string{}, binding.NetworkGrants...)
			expectedCommand := append([]string{}, binding.SelectedCommand...)
			if !equalProjectionValue(wire.Binding.NetworkGrants, expectedGrants) || !equalProjectionValue(wire.Binding.SelectedCommand, expectedCommand) {
				t.Fatalf("array fields changed order/content: got grants=%#v command=%#v, want grants=%#v command=%#v", wire.Binding.NetworkGrants, wire.Binding.SelectedCommand, expectedGrants, expectedCommand)
			}
		})
	}
}

func equalProjectionValue(got, want any) bool {
	gotJSON, gotErr := json.Marshal(got)
	wantJSON, wantErr := json.Marshal(want)
	return gotErr == nil && wantErr == nil && string(gotJSON) == string(wantJSON)
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

func TestEmbeddedAWSPlanAndChangeProjectionPreserveBindingDigests(t *testing.T) {
	binding := coreconfirmation.Binding{
		TargetID:          "aws-target:" + strings.Repeat("a", 64),
		ContentDigest:     coreconfirmation.Digest(strings.Repeat("b", 64)),
		ParameterDigest:   coreconfirmation.Digest(strings.Repeat("c", 64)),
		NetworkDigest:     coreconfirmation.Digest(strings.Repeat("d", 64)),
		SecretGrantDigest: coreconfirmation.Digest(strings.Repeat("e", 64)),
	}
	view := coreaws.PlanView{ID: "33333333-3333-4333-8333-333333333333", CredentialID: "11111111-1111-4111-8111-111111111111", PlanDigest: strings.Repeat("f", 64), ContentDigest: string(binding.ContentDigest), ParameterDigest: string(binding.ParameterDigest), NetworkDigest: string(binding.NetworkDigest), SecretGrantDigest: string(binding.SecretGrantDigest), TargetID: binding.TargetID}
	plan := planViewMap(view)
	for key, want := range map[string]string{"target_id": binding.TargetID, "content_digest": string(binding.ContentDigest), "parameter_digest": string(binding.ParameterDigest), "network_digest": string(binding.NetworkDigest), "secret_grant_digest": string(binding.SecretGrantDigest)} {
		if plan[key] != want {
			t.Fatalf("plan.%s = %#v, want %q", key, plan[key], want)
		}
	}
	action := changeRequestMap(coreaws.ChangeRequestResult{
		Change:       coreaws.Change{ID: "22222222-2222-4222-8222-222222222222", PlanID: view.ID, ProvisionID: "44444444-4444-4444-8444-444444444444", TaskID: "55555555-5555-4555-8555-555555555555", ConfirmationID: "66666666-6666-4666-8666-666666666666", Operation: coreaws.OperationCreate, Status: coreaws.ChangeWaitingUser, Revision: 1},
		Task:         coreaws.Task{ID: "55555555-5555-4555-8555-555555555555", PlanID: view.ID, ConfirmationID: "66666666-6666-4666-8666-666666666666", Status: "waiting_user", Revision: 1},
		Confirmation: coreconfirmation.Confirmation{ConfirmationID: "66666666-6666-4666-8666-666666666666", Binding: binding},
		Provision:    coreaws.Provision{ID: "44444444-4444-4444-8444-444444444444", PlanID: view.ID, PlanDigest: view.PlanDigest},
	})
	projected := action["provision"].(map[string]any)
	for key, want := range map[string]string{"target_id": binding.TargetID, "content_digest": string(binding.ContentDigest), "parameter_digest": string(binding.ParameterDigest), "network_digest": string(binding.NetworkDigest), "secret_grant_digest": string(binding.SecretGrantDigest)} {
		if projected[key] != want {
			t.Fatalf("change provision.%s = %#v, want %q", key, projected[key], want)
		}
	}
}

func TestEmbeddedEC2ChangeRequestProjectsCanonicalConfirmationTarget(t *testing.T) {
	targetID := "aws-target:" + strings.Repeat("a", 64)
	result := changeRequestMap(coreaws.ChangeRequestResult{
		Change:       coreaws.Change{ID: "22222222-2222-4222-8222-222222222222", PlanID: "33333333-3333-4333-8333-333333333333", ProvisionID: "11111111-1111-4111-8111-111111111111", TaskID: "44444444-4444-4444-8444-444444444444", ConfirmationID: "55555555-5555-4555-8555-555555555555", Operation: coreaws.OperationCreate, Status: coreaws.ChangeWaitingUser, Revision: 1},
		Task:         coreaws.Task{ID: "44444444-4444-4444-8444-444444444444", PlanID: "33333333-3333-4333-8333-333333333333", ConfirmationID: "55555555-5555-4555-8555-555555555555", Status: "waiting_user", Revision: 1},
		Confirmation: coreconfirmation.Confirmation{ConfirmationID: "55555555-5555-4555-8555-555555555555", Binding: coreconfirmation.Binding{TargetID: targetID}},
		Provision:    coreaws.Provision{ID: "11111111-1111-4111-8111-111111111111", PlanID: "33333333-3333-4333-8333-333333333333"},
	})
	projected := result["provision"].(map[string]any)
	if projected["target_id"] != targetID {
		t.Fatalf("target_id = %#v, want %q", projected["target_id"], targetID)
	}
}

func TestEmbeddedGeoLibrePlanProjectionUsesFlatTypedTarget(t *testing.T) {
	digest := strings.Repeat("a", 64)
	plan := coreworkload.Plan{
		ID: "33333333-3333-4333-8333-333333333333", Revision: 2, Digest: digest,
		TargetKind: coreworkload.TargetAWSEC2SSM,
		Target: coreworkload.TargetSettings{
			Identity: coreworkload.TargetIdentity{Kind: coreworkload.TargetAWSEC2SSM, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-0123456789abcdef0"},
			Labels:   map[string]string{"dirextalk:provision-id": "11111111-1111-4111-8111-111111111111", "dirextalk:provision-revision": "7", "owner_id": "must-not-project"},
		},
		SecretGrantRefs: []coreworkload.SecretGrantRef{{ReferenceID: "77777777-7777-4777-8777-777777777777", Purpose: coreconfirmation.SecretPurposeAWSCredential, Revision: 4, BindingDigest: coreconfirmation.Digest(digest)}},
		ExpiresAt:       time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	projected := geoLibrePlanMap(plan)
	target, ok := projected["typed_target"].(map[string]any)
	if !ok {
		t.Fatalf("typed_target = %#v", projected["typed_target"])
	}
	for key, want := range map[string]any{
		"provision_id": "11111111-1111-4111-8111-111111111111", "provision_revision": "7",
		"credential_id": "77777777-7777-4777-8777-777777777777", "credential_revision": int64(4),
		"account_id": "123456789012", "region": "us-east-1", "instance_id": "i-0123456789abcdef0",
	} {
		if target[key] != want {
			t.Fatalf("typed_target.%s = %#v, want %#v", key, target[key], want)
		}
	}
	if _, ok := target["labels"]; ok {
		t.Fatal("GeoLibre typed_target leaked raw labels")
	}
}
