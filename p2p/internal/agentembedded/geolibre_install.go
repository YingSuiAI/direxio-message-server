package agentembedded

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreworkload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

// geolibreInstallActionPort is deliberately split from the generic workload
// facade.  The only values accepted by this port are an authenticated,
// read-back EC2 provision and a persisted, server-authored workload plan.
func geolibreInstallActionPort(aws *coreaws.Service, workload *coreworkload.Service) ActionPort {
	return ActionPortFunc(func(ctx context.Context, owner, action string, params map[string]any) (any, *actionbase.Error) {
		if aws == nil || workload == nil || strings.TrimSpace(owner) == "" {
			return unavailable(ctx, params)
		}
		switch action {
		case "agent.core.aws.ec2_provisions.geolibre_install.plan":
			return geolibreInstallPlan(ctx, owner, params, aws, workload)
		case "agent.core.aws.ec2_provisions.geolibre_install.request":
			return geolibreInstallRequest(ctx, owner, params, aws, workload)
		default:
			return nil, actionbase.CodedError(404, "agent_action_not_found", "unsupported GeoLibre action")
		}
	})
}

// NewGeoLibreActionPort exposes the fixed release adapter to the message
// server wiring while keeping the implementation and provider types private.
func NewGeoLibreActionPort(aws *coreaws.Service, workload *coreworkload.Service) ActionPort {
	return geolibreInstallActionPort(aws, workload)
}

// ValidateGeoLibrePersistedPlan is the pre-dispatch fence used by the worker
// after confirmation consumption. It rechecks the owner-bound provision and
// immutable release snapshot immediately before any SSM provider call.
func ValidateGeoLibrePersistedPlan(plan coreworkload.Plan, provision coreaws.Provision, credential coreaws.Credentials, owner string, provisionPlans ...coreaws.PlanView) error {
	return validateGeoLibrePersistedPlan(plan, provision, credential, owner, provisionPlans...)
}

// ValidateGeoLibreWorkerPreDispatch is the final pure fence used by both a
// fresh worker claim and uncertain-operation recovery. The recovering bit is
// intentionally accepted to make that invariant explicit: recovery may only
// read/reconcile the same owner-bound immutable plan and can never bypass the
// public-exposure check.
func ValidateGeoLibreWorkerPreDispatch(plan coreworkload.Plan, provision coreaws.Provision, credential coreaws.Credentials, owner string, provisionPlan coreaws.PlanView, recovering bool) error {
	_ = recovering
	return ValidateGeoLibrePersistedPlan(plan, provision, credential, owner, provisionPlan)
}

// GeoLibrePlanProjection returns the immutable, redacted GeoLibre plan DTO
// used by the embedded action response and Native Agent confirmation cards.
func GeoLibrePlanProjection(plan coreworkload.Plan) map[string]any {
	return geoLibrePlanMap(plan)
}

func geolibreInstallPlan(ctx context.Context, owner string, params map[string]any, aws *coreaws.Service, workload *coreworkload.Service) (any, *actionbase.Error) {
	provision, plan, expiresAt, ae := buildGeoLibreInstallPlan(ctx, owner, params, aws, workload)
	if ae != nil {
		return nil, ae
	}
	return map[string]any{
		"plan":               geoLibrePlanMap(plan),
		"provision_id":       provision.ID,
		"provision_revision": provision.Revision,
		"expires_at":         expiresAt.UTC().Format(time.RFC3339Nano),
	}, nil
}

func geolibreInstallRequest(ctx context.Context, owner string, params map[string]any, aws *coreaws.Service, workload *coreworkload.Service) (any, *actionbase.Error) {
	provisionID, ae := requiredUUID(params, "provision_id")
	if ae != nil {
		return nil, ae
	}
	expectedRevision, ae := requiredPositiveInt64(params, "expected_revision")
	if ae != nil {
		return nil, ae
	}
	planID, ae := requiredUUID(params, "plan_id")
	if ae != nil {
		return nil, ae
	}
	planRevision, ae := requiredPositiveInt64(params, "plan_revision")
	if ae != nil {
		return nil, ae
	}
	planDigest, ae := requiredString(params, "plan_digest")
	if ae != nil {
		return nil, ae
	}
	key, ae := requiredUUID(params, "idempotency_key")
	if ae != nil {
		return nil, ae
	}
	expiresAt, ae := requiredTime(params, "expires_at")
	if ae != nil {
		return nil, ae
	}
	if expiresAt.Location() != time.UTC {
		return nil, actionbase.BadRequest("expires_at must use UTC")
	}
	workloadID, ae := optionalUUID(params, "workload_id")
	if ae != nil {
		return nil, ae
	}
	var expectedWorkloadRevision uint64
	if workloadID != "" {
		raw, ok := params["expected_workload_revision"]
		if !ok {
			return nil, actionbase.BadRequest("expected_workload_revision is required with workload_id")
		}
		expectedRevisionValue, e := optionalInt64(map[string]any{"expected_workload_revision": raw}, "expected_workload_revision")
		if e != nil || expectedRevisionValue < 1 {
			return nil, actionbase.BadRequest("expected_workload_revision must be positive")
		}
		workloadRecord, workloadErr := workload.GetWorkload(ctx, workloadID)
		if workloadErr != nil {
			return nil, workloadError(workloadErr)
		}
		expectedWorkloadRevision = uint64(expectedRevisionValue)
		if workloadRecord.Revision != expectedWorkloadRevision {
			return nil, actionbase.CodedError(409, "workload_conflict", "workload revision changed")
		}
	} else if _, ok := params["expected_workload_revision"]; ok {
		return nil, actionbase.BadRequest("workload_id is required with expected_workload_revision")
	}

	provision, err := aws.GetProvisionForOwner(ctx, provisionID, owner)
	if err != nil {
		return nil, awsError(err)
	}
	if provision.Revision != expectedRevision || provision.State != "active" || provision.ActiveChangeID != "" || provision.ReconciliationRequired || provision.Readback.Validate() != nil {
		return nil, actionbase.CodedError(409, "agent_revision_conflict", "GeoLibre install requires an active provision with an exact revision fence")
	}
	plan, err := workload.GetPlan(ctx, planID)
	if err != nil {
		return nil, workloadError(err)
	}
	if planRevision != int64(plan.Revision) || planDigest != plan.Digest {
		return nil, actionbase.CodedError(409, "workload_conflict", "GeoLibre plan revision or digest changed")
	}
	if !expiresAt.Equal(plan.ExpiresAt.UTC()) {
		return nil, actionbase.CodedError(409, "workload_conflict", "GeoLibre plan expiry does not match the persisted plan")
	}
	cred, err := aws.GetCredentialRevision(ctx, provision.CredentialID, provision.CredentialRevision)
	if err != nil {
		return nil, awsError(err)
	}
	provisionPlan, err := aws.GetPlanForOwner(ctx, provision.PlanID, owner)
	if err != nil {
		return nil, actionbase.CodedError(409, "agent_revision_conflict", "owner-bound EC2 provision plan is unavailable")
	}
	if err := validateGeoLibrePersistedPlan(plan, provision, cred, owner, provisionPlan); err != nil {
		return nil, actionbase.CodedError(409, "workload_conflict", "persisted GeoLibre plan failed immutable verification")
	}

	// RequestApply only creates the generic operation, task and confirmation.
	// Provider dispatch is owned by the worker after the confirmation is
	// consumed; this action never invokes an AWS provider.
	result, err := workload.RequestApply(ctx, coreworkload.RequestApplyInput{PlanID: plan.ID, WorkloadID: workloadID, ExpectedWorkloadRevision: expectedWorkloadRevision, IdempotencyKey: key, ExpiresAt: plan.ExpiresAt.UTC()})
	if err != nil {
		return nil, workloadError(err)
	}
	operation, err := workloadOperationProjection(ctx, workload, result.Operation)
	if err != nil {
		return nil, workloadError(err)
	}
	confirmation := confirmationMap(result.Confirmation)
	confirmation["expected_workload_revision"] = result.Operation.ExpectedWorkloadRevision
	return map[string]any{
		"plan":                       geoLibrePlanMap(plan),
		"provision_id":               provision.ID,
		"provision_revision":         provision.Revision,
		"expires_at":                 plan.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"workload_id":                result.Operation.WorkloadID,
		"operation":                  operation,
		"task_id":                    result.Task.ID,
		"task":                       taskMap(result.Task),
		"confirmation_id":            result.Confirmation.ConfirmationID,
		"confirmation":               confirmation,
		"expected_workload_revision": result.Operation.ExpectedWorkloadRevision,
	}, nil
}

// geoLibrePlanMap is the public, redacted projection for the fixed release.
// It deliberately omits command steps, image URI, owner tags and secret
// bindings; those remain in the immutable execution snapshot only.
func geoLibrePlanMap(plan coreworkload.Plan) map[string]any {
	manifest := coreaws.CurrentGeoLibreReleaseManifest()
	identity := plan.Target.Identity
	provisionID := plan.Target.Labels["dirextalk:provision-id"]
	provisionRevision := plan.Target.Labels["dirextalk:provision-revision"]
	credentialID, credentialRevision := "", int64(0)
	if len(plan.SecretGrantRefs) == 1 {
		credentialID, credentialRevision = plan.SecretGrantRefs[0].ReferenceID, plan.SecretGrantRefs[0].Revision
	}
	target := map[string]any{
		"provision_id": provisionID, "provision_revision": provisionRevision,
		"credential_id": credentialID, "credential_revision": credentialRevision,
		"account_id": identity.AccountID, "region": identity.Region, "instance_id": identity.InstanceID,
		"public_endpoint": identity.Endpoint, "service": manifest.SystemdService, "port": manifest.ContainerPort,
		"exposure": "public-unauthenticated-http", "sidecar": "disabled",
	}
	return map[string]any{
		"plan_id": plan.ID, "revision": plan.Revision, "digest": plan.Digest,
		"summary": plan.Summary, "artifact": plan.Artifact, "source": plan.Source,
		"image_digest": plan.ImageDigest, "target_kind": workloadTargetKindWire(plan.TargetKind),
		"expires_at":   plan.ExpiresAt.UTC().Format(time.RFC3339Nano),
		"created_at":   plan.CreatedAt.UTC().Format(time.RFC3339Nano),
		"typed_target": target, "typed_resource_limits": resourceLimitsMap(plan.ResourceLimits),
		"release": map[string]any{
			"version": manifest.Version, "commit": manifest.CommitSHA, "image_digest": manifest.ImageDigest,
			"manifest_digest": manifest.Digest(), "command_digest": coreworkload.GeoLibreStaticV1CommandDigest,
			"service": manifest.SystemdService, "port": manifest.ContainerPort, "health_path": manifest.HealthPath,
		},
	}
}

func buildGeoLibreInstallPlan(ctx context.Context, owner string, params map[string]any, aws *coreaws.Service, workload *coreworkload.Service) (coreaws.Provision, coreworkload.Plan, time.Time, *actionbase.Error) {
	if aws == nil || workload == nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, actionbase.CodedError(412, "agent_embedded_unavailable", ErrUnavailable.Error())
	}
	provisionID, ae := requiredUUID(params, "provision_id")
	if ae != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, ae
	}
	expected, ae := requiredPositiveInt64(params, "expected_revision")
	if ae != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, ae
	}
	expiresAt, ae := requiredTime(params, "expires_at")
	if ae != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, ae
	}
	if expiresAt.Location() != time.UTC {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, actionbase.BadRequest("expires_at must use UTC")
	}
	key, ae := requiredUUID(params, "idempotency_key")
	if ae != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, ae
	}
	provision, err := aws.GetProvisionForOwner(ctx, provisionID, owner)
	if err != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, awsError(err)
	}
	if provision.Revision != expected || provision.State != "active" || provision.ActiveChangeID != "" || provision.ReconciliationRequired || provision.Readback.Validate() != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, actionbase.CodedError(409, "agent_revision_conflict", "GeoLibre install requires an active provision with an exact revision fence")
	}
	cred, err := aws.GetCredentialRevision(ctx, provision.CredentialID, provision.CredentialRevision)
	if err != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, awsError(err)
	}
	provisionPlan, err := aws.GetPlanForOwner(ctx, provision.PlanID, owner)
	if err != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, actionbase.CodedError(409, "agent_revision_conflict", "owner-bound EC2 provision plan is unavailable")
	}
	if err := validateGeoLibreEC2ProvisionPlan(provisionPlan, provision, owner); err != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, actionbase.CodedError(409, "agent_revision_conflict", "GeoLibre requires a public immutable EC2 provision plan")
	}
	target := coreaws.GeoLibreInstallTarget{
		ProvisionID:        provision.ID,
		ProvisionPlanID:    provision.PlanID,
		ProvisionRevision:  provision.Revision,
		CredentialID:       provision.CredentialID,
		CredentialRevision: provision.CredentialRevision,
		AccountID:          cred.AccountID,
		Region:             provision.Region,
		InstanceID:         provision.Readback.InstanceID,
		PublicIP:           provision.Readback.PublicIP,
		SecurityGroupID:    provision.Readback.SecurityGroupID,
		OwnerBindingDigest: provision.OwnerDigest,
	}
	input, err := coreaws.BuildGeoLibreSSMPlan(target, key, expiresAt)
	if err != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, awsError(err)
	}
	plan, err := workload.CreatePlan(ctx, input)
	if err != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, workloadError(err)
	}
	if err := validateGeoLibrePersistedPlan(plan, provision, cred, owner, provisionPlan); err != nil {
		return coreaws.Provision{}, coreworkload.Plan{}, time.Time{}, workloadError(err)
	}
	return provision, plan, expiresAt, nil
}

// validateGeoLibrePersistedPlan is the fail-closed boundary between a
// user-visible plan and provider execution. It intentionally compares every
// immutable release, target, credential and owner binding instead of trusting
// the JSON blob returned by the workload store.
func validateGeoLibrePersistedPlan(plan coreworkload.Plan, provision coreaws.Provision, credential coreaws.Credentials, owner string, provisionPlans ...coreaws.PlanView) error {
	if plan.Validate() != nil || provision.Validate() != nil || credential.Validate() != nil || strings.TrimSpace(owner) == "" {
		return errors.New("invalid GeoLibre plan binding")
	}
	manifest := coreaws.CurrentGeoLibreReleaseManifest()
	if plan.TargetKind != coreworkload.TargetAWSEC2SSM || plan.ImageDigest != manifest.ImageDigest || plan.ImageURI != manifest.ImageURI || plan.Source != manifest.RepositoryURL+"#"+manifest.CommitSHA || plan.Artifact != "geolibre-manifest:"+manifest.Digest() || plan.Summary != coreworkload.GeoLibreStaticV1Summary(provision.ID) || coreworkload.CommandStepsDigest(plan.CommandSteps) != coreworkload.GeoLibreStaticV1CommandDigest || manifest.Digest() != coreworkload.GeoLibreStaticV1ManifestDigest {
		return errors.New("GeoLibre release is not pinned")
	}
	identity := plan.Target.Identity
	if identity.Kind != coreworkload.TargetAWSEC2SSM || identity.AccountID != credential.AccountID || identity.Region != provision.Region || identity.InstanceID != provision.Readback.InstanceID || identity.Endpoint != "http://"+provision.Readback.PublicIP {
		return errors.New("GeoLibre target identity drifted")
	}
	revision, revisionErr := strconv.ParseInt(plan.Target.Labels["dirextalk:provision-revision"], 10, 64)
	if plan.Target.Region != provision.Region || plan.Target.AccountID != credential.AccountID || plan.Target.InstanceID != provision.Readback.InstanceID || plan.Target.EC2SystemdService != manifest.SystemdService || plan.Target.EC2CleanupProfile != coreworkload.EC2CleanupProfileGeoLibreStaticV1 || plan.Target.RequiredInstanceTags["owner"] != provision.OwnerDigest || plan.Target.RequiredInstanceTags["dirextalk:plan-id"] != provision.PlanID || plan.Target.Labels["dirextalk:provision-id"] != provision.ID || revisionErr != nil || revision != provision.Revision || plan.Target.Labels["dirextalk:release"] != manifest.Version || plan.Target.Labels["dirextalk:manifest-digest"] != manifest.Digest() || plan.Target.Labels["dirextalk:command-digest"] != coreworkload.GeoLibreStaticV1CommandDigest || plan.Target.Labels["dirextalk:sidecar"] != "disabled" {
		return errors.New("GeoLibre target settings drifted")
	}
	if coreaws.OwnerBindingDigest(owner) != provision.OwnerDigest || plan.Target.RequiredInstanceTags["managed"] != "true" || plan.Target.RequiredInstanceTags["service"] != coreaws.EC2ServiceProfile {
		return errors.New("GeoLibre owner binding drifted")
	}
	if len(provisionPlans) != 1 {
		return errors.New("GeoLibre provision plan binding is unavailable")
	}
	if err := validateGeoLibreEC2ProvisionPlan(provisionPlans[0], provision, owner); err != nil {
		return err
	}
	refs := plan.SecretGrantRefs
	if len(refs) != 1 || refs[0].Purpose != coreconfirmation.SecretPurposeAWSCredential || refs[0].ReferenceID != provision.CredentialID || refs[0].Revision != provision.CredentialRevision || plan.Target.NetworkGrantDetails == nil || len(plan.Target.NetworkGrantDetails) != 1 || plan.Target.NetworkGrantDetails[0].ReferenceID != provision.Readback.SecurityGroupID || plan.Target.NetworkGrantDetails[0].Kind != "aws_security_group" {
		return errors.New("GeoLibre credential or network binding drifted")
	}
	return nil
}

// validateGeoLibreEC2ProvisionPlan is the cross-domain fence for the
// original typed EC2 plan. GeoLibre is intentionally public HTTP; accepting a
// closed-port provision plan would produce a misleading endpoint and an
// unreachable deployment. The plan is re-read from the AWS service at both
// plan issuance and worker dispatch, so a stale projection cannot pass.
func validateGeoLibreEC2ProvisionPlan(plan coreaws.PlanView, provision coreaws.Provision, owner string) error {
	if plan.ID != provision.PlanID || plan.Revision != provision.PlanRevision ||
		plan.CredentialID != provision.CredentialID || plan.CredentialRevision != provision.CredentialRevision ||
		plan.Region != provision.Region || plan.StackName != provision.StackName ||
		plan.TemplateSHA256 != provision.TemplateSHA256 || plan.PlanDigest != provision.PlanDigest ||
		plan.Operation != coreaws.OperationCreate || plan.Tags["service"] != coreaws.EC2ServiceProfile ||
		plan.Tags["owner"] != provision.OwnerDigest || plan.Tags["dirextalk:owner-binding"] != provision.OwnerDigest ||
		strings.TrimSpace(owner) == "" || coreaws.OwnerBindingDigest(owner) != provision.OwnerDigest {
		return errors.New("GeoLibre EC2 provision plan identity drifted")
	}
	// The typed EC2 builder serializes this immutable parameter as exactly
	// "true". Do not accept coercible aliases: they would weaken the public
	// exposure binding and make the readback contract ambiguous.
	if plan.Parameters["PublicHTTP"] != "true" {
		return errors.New("GeoLibre requires PublicHTTP=true")
	}
	return nil
}
