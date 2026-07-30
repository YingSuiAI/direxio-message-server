package agentembedded

import (
	"context"
	"strings"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/google/uuid"
)

const geoTestOwner = "@geo-owner:example.test"

func geoPlanFixture(t *testing.T) (coreworkload.Plan, coreaws.Provision, coreaws.Credentials) {
	t.Helper()
	credentialService := coreaws.NewService(coreaws.NewMemoryRepository(), nil, nil, nil, nil, nil)
	view, err := credentialService.SaveCredential(context.Background(), coreaws.CredentialInput{
		Name: "geo", Region: "ap-east-1", AccessKeyID: "access", SecretAccessKey: "secret",
		IdempotencyKey: uuid.NewString(),
	})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := credentialService.GetCredentialRevision(context.Background(), view.ID, view.Revision)
	if err != nil {
		t.Fatal(err)
	}
	credential.AccountID = "123456789012"
	ownerDigest := coreaws.OwnerBindingDigest(geoTestOwner)
	provision := coreaws.Provision{
		ID: uuid.NewString(), PlanID: uuid.NewString(), CredentialID: credential.ID, Region: credential.Region,
		StackName: "geo-stack", Profile: coreaws.EC2ServiceProfile, OwnerDigest: ownerDigest,
		CredentialRevision: credential.Revision, PlanRevision: 1, TemplateSHA256: strings.Repeat("a", 64),
		PlanDigest: strings.Repeat("b", 64), State: "active", Revision: 1,
		Readback:  coreaws.ProvisionReadback{StackID: "stack", InstanceID: "i-0123456789abcdef0", PublicIP: "192.0.2.10", SecurityGroupID: "sg-0123456789abcdef0", ObservedAt: time.Now().UTC()},
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}
	input, err := coreaws.BuildGeoLibreSSMPlan(coreaws.GeoLibreInstallTarget{
		ProvisionID: provision.ID, ProvisionPlanID: provision.PlanID, ProvisionRevision: provision.Revision,
		CredentialID: credential.ID, CredentialRevision: credential.Revision, AccountID: credential.AccountID,
		Region: credential.Region, InstanceID: provision.Readback.InstanceID, PublicIP: provision.Readback.PublicIP,
		SecurityGroupID: provision.Readback.SecurityGroupID, OwnerBindingDigest: ownerDigest,
	}, uuid.NewString(), time.Now().UTC().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	input.SecretGrantRefs[0].BindingDigest = coreconfirmation.Digest(strings.Repeat("0", 64))
	plan, err := coreworkload.Plan{Revision: 1, Summary: input.Summary, Artifact: input.Artifact, Source: input.Source, CommandSteps: input.CommandSteps, ImageDigest: input.ImageDigest, ImageURI: input.ImageURI, TargetKind: input.TargetKind, Target: input.Target, NetworkGrants: input.NetworkGrants, SecretGrantRefs: input.SecretGrantRefs, ResourceLimits: input.ResourceLimits, ExpiresAt: input.ExpiresAt, ID: uuid.NewString()}.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return plan, provision, credential
}

func TestGeoLibrePersistedPlanFailsClosedOnOwnerRevisionAndTamper(t *testing.T) {
	plan, provision, credential := geoPlanFixture(t)
	ec2Plan := geoEC2PlanView(provision, "true")
	if err := validateGeoLibrePersistedPlan(plan, provision, credential, geoTestOwner, ec2Plan); err != nil {
		t.Fatalf("valid fixture rejected: %v", err)
	}
	if err := validateGeoLibrePersistedPlan(plan, provision, credential, "@other:example.test", ec2Plan); err == nil {
		t.Fatal("cross-owner plan accepted")
	}
	provision.Revision++
	if err := validateGeoLibrePersistedPlan(plan, provision, credential, geoTestOwner, ec2Plan); err == nil {
		t.Fatal("stale provision revision accepted")
	}
	provision.Revision--
	plan.Target.Labels["dirextalk:manifest-digest"] = strings.Repeat("c", 64)
	if err := validateGeoLibrePersistedPlan(plan, provision, credential, geoTestOwner, ec2Plan); err == nil {
		t.Fatal("tampered release accepted")
	}
}

func geoEC2PlanView(provision coreaws.Provision, publicHTTP string) coreaws.PlanView {
	return coreaws.PlanView{
		ID: provision.PlanID, CredentialID: provision.CredentialID, CredentialRevision: provision.CredentialRevision,
		Region: provision.Region, StackName: provision.StackName, TemplateSHA256: provision.TemplateSHA256,
		PlanDigest: provision.PlanDigest, Operation: coreaws.OperationCreate, Revision: provision.PlanRevision,
		Parameters: map[string]string{"PublicHTTP": publicHTTP},
		Tags:       map[string]string{"service": coreaws.EC2ServiceProfile, "owner": provision.OwnerDigest, "dirextalk:owner-binding": provision.OwnerDigest},
	}
}

func TestGeoLibreRequiresPublicImmutableEC2ProvisionPlan(t *testing.T) {
	plan, provision, credential := geoPlanFixture(t)
	closed := geoEC2PlanView(provision, "false")
	if err := validateGeoLibrePersistedPlan(plan, provision, credential, geoTestOwner, closed); err == nil {
		t.Fatal("GeoLibre accepted an EC2 plan with PublicHTTP=false")
	}
	open := geoEC2PlanView(provision, "true")
	if err := validateGeoLibrePersistedPlan(plan, provision, credential, geoTestOwner, open); err != nil {
		t.Fatalf("valid public EC2 plan rejected: %v", err)
	}
	open.Revision++
	if err := validateGeoLibrePersistedPlan(plan, provision, credential, geoTestOwner, open); err == nil {
		t.Fatal("GeoLibre accepted a drifted EC2 provision plan revision")
	}
}

func TestGeoLibreWorkerPreDispatchRechecksPublicPlanForFreshAndRecovery(t *testing.T) {
	plan, provision, credential := geoPlanFixture(t)
	for _, recovering := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "recovery"}[recovering], func(t *testing.T) {
			closed := geoEC2PlanView(provision, "false")
			if err := ValidateGeoLibreWorkerPreDispatch(plan, provision, credential, geoTestOwner, closed, recovering); err == nil {
				t.Fatalf("worker pre-dispatch accepted closed PublicHTTP plan (recovering=%t)", recovering)
			}
			open := geoEC2PlanView(provision, "true")
			if err := ValidateGeoLibreWorkerPreDispatch(plan, provision, credential, geoTestOwner, open, recovering); err != nil {
				t.Fatalf("worker pre-dispatch rejected valid plan (recovering=%t): %v", recovering, err)
			}
		})
	}
}

func TestGeoLibreProjectionRedactsExecutionSecretsAndCommands(t *testing.T) {
	plan, _, _ := geoPlanFixture(t)
	projected := geoLibrePlanMap(plan)
	for _, forbidden := range []string{"command_steps", "image_uri", "typed_secret_grants"} {
		if _, ok := projected[forbidden]; ok {
			t.Fatalf("projection exposes %s", forbidden)
		}
	}
	target, _ := projected["typed_target"].(map[string]any)
	if _, ok := target["owner_digest"]; ok {
		t.Fatal("projection exposes owner digest")
	}
}

func TestGeoLibreRequestCreatesWaitingConfirmationWithoutDispatch(t *testing.T) {
	store := coreworkload.NewMemoryStore(time.Now)
	planInput := coreworkload.PlanInput{Summary: "fixed", TargetKind: coreworkload.TargetAWSEC2SSM, Target: coreworkload.TargetSettings{Identity: coreworkload.TargetIdentity{Kind: coreworkload.TargetAWSEC2SSM, AccountID: "123456789012", Region: "ap-east-1", InstanceID: "i-0123456789abcdef0"}, AccountID: "123456789012", Region: "ap-east-1", InstanceID: "i-0123456789abcdef0", EC2DocumentVersion: "1", EC2SystemdService: "geo.service", RequiredInstanceTags: map[string]string{"managed": "true"}}, ExpiresAt: time.Now().UTC().Add(time.Hour), IdempotencyKey: uuid.NewString(), SecretGrantRefs: []coreworkload.SecretGrantRef{{ReferenceID: uuid.NewString(), Purpose: coreconfirmation.SecretPurposeAWSCredential, Revision: 1, BindingDigest: coreconfirmation.Digest(strings.Repeat("0", 64))}}}
	plan, err := store.CreatePlan(context.Background(), planInput)
	if err != nil {
		t.Fatal(err)
	}
	r, err := store.RequestOperation(context.Background(), coreworkload.RequestCommand{PlanID: plan.ID, Kind: coreworkload.OperationApply, IdempotencyKey: uuid.NewString(), ExpiresAt: plan.ExpiresAt})
	if err != nil {
		t.Fatal(err)
	}
	if r.Operation.Status != coreworkload.OperationWaitingUser || r.Confirmation.State != coreconfirmation.StatePending || r.Task.Status != "waiting_user" {
		t.Fatalf("request dispatched before confirmation: op=%s confirmation=%s task=%s", r.Operation.Status, r.Confirmation.State, r.Task.Status)
	}
}

func TestGeoLibreRequestEnforcesOptionalWorkloadFencePair(t *testing.T) {
	aws := coreaws.NewService(coreaws.NewMemoryRepository(), nil, nil, nil, nil, nil)
	workload, err := coreworkload.NewService(coreworkload.NewMemoryStore(time.Now), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	port := NewGeoLibreActionPort(aws, workload)
	base := map[string]any{
		"provision_id":      uuid.NewString(),
		"expected_revision": int64(1),
		"plan_id":           uuid.NewString(),
		"plan_revision":     int64(1),
		"plan_digest":       strings.Repeat("a", 64),
		"expires_at":        "2099-01-01T00:00:00Z",
		"idempotency_key":   uuid.NewString(),
	}
	for name, params := range map[string]map[string]any{
		"missing revision": func() map[string]any {
			p := cloneParams(base)
			p["workload_id"] = uuid.NewString()
			return p
		}(),
		"missing workload": func() map[string]any {
			p := cloneParams(base)
			p["expected_workload_revision"] = int64(1)
			return p
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			result, apiErr := port.Handle(context.Background(), geoTestOwner, "agent.core.aws.ec2_provisions.geolibre_install.request", params)
			if result != nil || apiErr == nil || apiErr.Status != 400 {
				t.Fatalf("paired workload fence result=%#v error=%#v", result, apiErr)
			}
		})
	}
}

func cloneParams(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}
