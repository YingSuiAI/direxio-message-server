package agentgrpc

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
)

const (
	actionTeamPlansGet                = "agent.team.plans.get"
	actionTeamApprovalDeviceBootstrap = "agent.team.approval_device.bootstrap"
	actionTeamPlanApprovalPrepare     = "agent.team.plans.approval.prepare"
	actionTeamPlansApprove            = "agent.team.plans.approve"
	actionTeamExecutionsGet           = "agent.team.executions.get"
	teamSignatureSchemaV2             = "dirextalk.agent.team-plan-signature/v2"
	teamArtifactSchemaV1              = "dirextalk.agent.team-artifact/v1"
	maxTeamApprovalSigningPayloadSize = 64 << 10
	maxTeamArtifactsPerRole           = 32
	maxTeamArtifactBytes              = int64(8 << 20)
)

var (
	teamDigestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	teamKeyIDPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$`)
	teamDeviceKeyIDPattern = regexp.MustCompile(
		`^cloud-device-[0-9a-f]{24}$`,
	)
	teamRoleIDPattern       = regexp.MustCompile(`^[a-z][a-z0-9-]{0,63}$`)
	teamActionIDPattern     = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)
	teamArtifactNamePattern = regexp.MustCompile(
		`^[a-z][a-z0-9._-]{0,127}$`,
	)
	teamCurrencyPattern = regexp.MustCompile(`^[A-Z]{3}$`)
)

func isTeamAction(action string) bool {
	switch strings.TrimSpace(action) {
	case actionTeamPlansGet, actionTeamApprovalDeviceBootstrap,
		actionTeamPlanApprovalPrepare,
		actionTeamPlansApprove, actionTeamExecutionsGet:
		return true
	default:
		return false
	}
}

func (runner *Runner) invokeTeamAction(
	ctx context.Context,
	action string,
	params map[string]any,
) (map[string]any, error) {
	if runner.team == nil {
		return nil, errors.New("agent team service is unavailable")
	}
	switch action {
	case actionTeamPlansGet:
		return runner.getTeamPlan(ctx, params)
	case actionTeamApprovalDeviceBootstrap:
		return runner.bootstrapFirstTeamApprovalDevice(ctx, params)
	case actionTeamPlanApprovalPrepare:
		return runner.prepareTeamPlanApproval(ctx, params)
	case actionTeamPlansApprove:
		return runner.approveTeamPlan(ctx, params)
	case actionTeamExecutionsGet:
		return runner.getTeamExecution(ctx, params)
	default:
		return nil, errors.New("agent service action is not supported")
	}
}

func (runner *Runner) bootstrapFirstTeamApprovalDevice(
	ctx context.Context,
	params map[string]any,
) (map[string]any, error) {
	if err := allowTeamActionParams(
		params,
		"idempotency_key",
		"key_id",
		"public_key_base64url",
	); err != nil {
		return nil, err
	}
	idempotencyKey, err := requiredTeamUUIDParam(
		params,
		"idempotency_key",
	)
	if err != nil {
		return nil, err
	}
	keyID, err := requiredTeamTextParam(params, "key_id", 128)
	if err != nil || !teamDeviceKeyIDPattern.MatchString(keyID) {
		return nil, teamParameterError("key_id is invalid")
	}
	publicKeyText, err := requiredTeamTextParam(
		params,
		"public_key_base64url",
		64,
	)
	if err != nil {
		return nil, err
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(publicKeyText)
	if err != nil ||
		len(publicKey) != ed25519.PublicKeySize ||
		base64.RawURLEncoding.EncodeToString(publicKey) != publicKeyText {
		clear(publicKey)
		return nil, teamParameterError(
			"public_key_base64url is invalid",
		)
	}
	defer clear(publicKey)
	digest := sha256.Sum256(publicKey)
	expectedKeyID := "cloud-device-" +
		hex.EncodeToString(digest[:])[:24]
	if keyID != expectedKeyID {
		return nil, teamParameterError(
			"key_id does not match public_key_base64url",
		)
	}

	callContext, cancel := context.WithTimeout(
		ctx,
		runner.chainTimeout,
	)
	defer cancel()
	response, err := runner.team.BootstrapFirstTeamApprovalDeviceV3(
		callContext,
		&agentv1.BootstrapFirstTeamApprovalDeviceV3Request{
			IdempotencyKey: idempotencyKey,
			OwnerId:        runner.ownerID,
			KeyId:          keyID,
			PublicKey:      publicKey,
		},
	)
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil ||
		response.GetKeyId() != keyID ||
		response.GetRevision() != 1 ||
		response.GetExpiresAt() == nil ||
		response.GetExpiresAt().CheckValid() != nil ||
		!time.Now().UTC().Before(response.GetExpiresAt().AsTime()) {
		return nil, errors.New(
			"agent service returned an invalid Team approval-device response",
		)
	}
	expiresAt, err := requiredTimestamp(response.GetExpiresAt())
	if err != nil {
		return nil, errors.New(
			"agent service returned an invalid Team approval-device response",
		)
	}
	return map[string]any{
		"key_id":     response.GetKeyId(),
		"revision":   response.GetRevision(),
		"status":     "active",
		"expires_at": expiresAt,
	}, nil
}

func (runner *Runner) getTeamPlan(
	ctx context.Context,
	params map[string]any,
) (map[string]any, error) {
	if err := allowTeamActionParams(
		params,
		"plan_id",
		"plan_revision",
	); err != nil {
		return nil, err
	}
	planID, err := requiredTeamUUIDParam(params, "plan_id")
	if err != nil {
		return nil, err
	}
	planRevision, err := positiveTeamRevisionParam(
		params,
		"plan_revision",
	)
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.team.GetTeamPlanV3(
		callContext,
		&agentv1.GetTeamPlanV3Request{
			OwnerId:      runner.ownerID,
			PlanId:       planID,
			PlanRevision: planRevision,
		},
	)
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil ||
		response.GetPlan() == nil ||
		response.GetPlan().GetPlanId() != planID ||
		response.GetPlan().GetPlanRevision() != planRevision {
		return nil, errors.New(
			"agent service returned an invalid Team Plan response",
		)
	}
	executionID := response.GetExecutionId()
	if executionID != "" && !canonicalUUID(executionID) {
		return nil, errors.New(
			"agent service returned an invalid Team execution identity",
		)
	}
	plan, err := runner.mapTeamPlan(response.GetPlan())
	if err != nil {
		return nil, err
	}
	plan["execution_id"] = executionID
	return plan, nil
}

func (runner *Runner) prepareTeamPlanApproval(
	ctx context.Context,
	params map[string]any,
) (map[string]any, error) {
	if err := allowTeamActionParams(
		params,
		"idempotency_key",
		"plan_id",
		"plan_revision",
		"expected_plan_record_revision",
	); err != nil {
		return nil, err
	}
	idempotencyKey, err := requiredTeamUUIDParam(
		params,
		"idempotency_key",
	)
	if err != nil {
		return nil, err
	}
	planID, err := requiredTeamUUIDParam(params, "plan_id")
	if err != nil {
		return nil, err
	}
	planRevision, err := positiveTeamRevisionParam(
		params,
		"plan_revision",
	)
	if err != nil {
		return nil, err
	}
	recordRevision, err := positiveTeamRevisionParam(
		params,
		"expected_plan_record_revision",
	)
	if err != nil {
		return nil, err
	}
	signer, err := runner.newTeamSessionApprovalSigner()
	if err != nil {
		return nil, err
	}
	defer signer.clear()
	if err := runner.ensureTeamSessionApprovalSigner(ctx, signer); err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.team.CreateTeamApprovalChallengeV3(
		callContext,
		&agentv1.CreateTeamApprovalChallengeV3Request{
			IdempotencyKey:             idempotencyKey,
			OwnerId:                    runner.ownerID,
			PlanId:                     planID,
			PlanRevision:               planRevision,
			ExpectedPlanRecordRevision: recordRevision,
			SignerKeyId:                signer.keyID,
		},
	)
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	return runner.mapTeamApprovalChallenge(
		response,
		planID,
		planRevision,
		signer.keyID,
	)
}

func (runner *Runner) approveTeamPlan(
	ctx context.Context,
	params map[string]any,
) (map[string]any, error) {
	if err := allowTeamActionParams(
		params,
		"idempotency_key",
		"approval_prepare_idempotency_key",
		"plan_id",
		"plan_revision",
		"expected_plan_record_revision",
		"expected_challenge_record_revision",
		"expected_challenge_id",
		"expected_plan_digest",
		"expected_signer_key_id",
		"expected_launch_authorization_id",
		"expected_launch_authorization_digest",
	); err != nil {
		return nil, err
	}
	idempotencyKey, err := requiredTeamUUIDParam(
		params,
		"idempotency_key",
	)
	if err != nil {
		return nil, err
	}
	prepareIdempotencyKey, err := requiredTeamUUIDParam(
		params,
		"approval_prepare_idempotency_key",
	)
	if err != nil {
		return nil, err
	}
	planID, err := requiredTeamUUIDParam(params, "plan_id")
	if err != nil {
		return nil, err
	}
	planRevision, err := positiveTeamRevisionParam(params, "plan_revision")
	if err != nil {
		return nil, err
	}
	planRecordRevision, err := positiveTeamRevisionParam(
		params,
		"expected_plan_record_revision",
	)
	if err != nil {
		return nil, err
	}
	challengeRecordRevision, err := positiveTeamRevisionParam(
		params,
		"expected_challenge_record_revision",
	)
	if err != nil {
		return nil, err
	}
	expectedChallengeID, err := requiredTeamUUIDParam(
		params,
		"expected_challenge_id",
	)
	if err != nil {
		return nil, err
	}
	expectedPlanDigest, err := requiredTeamDigestParam(
		params,
		"expected_plan_digest",
	)
	if err != nil {
		return nil, err
	}
	expectedSignerKeyID, err := requiredTeamKeyIDParam(
		params,
		"expected_signer_key_id",
	)
	if err != nil {
		return nil, err
	}
	expectedAuthorizationID, err := requiredTeamUUIDParam(
		params,
		"expected_launch_authorization_id",
	)
	if err != nil {
		return nil, err
	}
	expectedAuthorizationDigest, err := requiredTeamDigestParam(
		params,
		"expected_launch_authorization_digest",
	)
	if err != nil {
		return nil, err
	}

	signer, err := runner.newTeamSessionApprovalSigner()
	if err != nil {
		return nil, err
	}
	defer signer.clear()
	if signer.keyID != expectedSignerKeyID {
		return nil, teamParameterError("approval session signer changed")
	}
	if err := runner.ensureTeamSessionApprovalSigner(ctx, signer); err != nil {
		return nil, err
	}

	challengeContext, cancelChallenge := context.WithTimeout(
		ctx,
		runner.chainTimeout,
	)
	challengeResponse, err := runner.team.CreateTeamApprovalChallengeV3(
		challengeContext,
		&agentv1.CreateTeamApprovalChallengeV3Request{
			IdempotencyKey:             prepareIdempotencyKey,
			OwnerId:                    runner.ownerID,
			PlanId:                     planID,
			PlanRevision:               planRevision,
			ExpectedPlanRecordRevision: planRecordRevision,
			SignerKeyId:                signer.keyID,
		},
	)
	if err != nil {
		sanitized := sanitizeRPCError(challengeContext, err)
		cancelChallenge()
		return nil, sanitized
	}
	cancelChallenge()
	if _, err := runner.mapTeamApprovalChallenge(
		challengeResponse,
		planID,
		planRevision,
		signer.keyID,
	); err != nil {
		return nil, err
	}
	challenge := challengeResponse.GetChallenge()
	if challenge.GetChallengeId() != expectedChallengeID ||
		challenge.GetPlanDigest() != expectedPlanDigest ||
		challenge.GetRecordRevision() != challengeRecordRevision ||
		challenge.GetLaunchAuthorizationId() != expectedAuthorizationID ||
		challenge.GetLaunchAuthorizationDigest() !=
			expectedAuthorizationDigest ||
		challenge.GetExpiresAt() == nil ||
		!time.Now().UTC().Before(challenge.GetExpiresAt().AsTime()) {
		return nil, teamParameterError("approval confirmation is stale")
	}
	signature := ed25519.Sign(
		signer.privateKey,
		challenge.GetSigningPayloadCbor(),
	)
	defer clear(signature)
	approval := &agentv1.TeamApprovalSignatureV3{
		SchemaVersion:             teamSignatureSchemaV2,
		ApprovalId:                challenge.GetApprovalId(),
		ChallengeId:               challenge.GetChallengeId(),
		PlanId:                    challenge.GetPlanId(),
		PlanRevision:              challenge.GetPlanRevision(),
		PlanDigest:                challenge.GetPlanDigest(),
		SignerKeyId:               challenge.GetSignerKeyId(),
		Signature:                 signature,
		LaunchAuthorizationId:     challenge.GetLaunchAuthorizationId(),
		LaunchAuthorizationDigest: challenge.GetLaunchAuthorizationDigest(),
	}

	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.team.ApproveTeamPlanV3(
		callContext,
		&agentv1.ApproveTeamPlanV3Request{
			IdempotencyKey:                  idempotencyKey,
			OwnerId:                         runner.ownerID,
			ExpectedPlanRecordRevision:      planRecordRevision,
			ExpectedChallengeRecordRevision: challengeRecordRevision,
			Approval:                        approval,
		},
	)
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil ||
		response.GetPlan() == nil ||
		response.GetPlan().GetPlanId() != approval.GetPlanId() ||
		response.GetPlan().GetPlanRevision() !=
			approval.GetPlanRevision() ||
		response.GetPlan().GetPlanDigest() !=
			approval.GetPlanDigest() ||
		response.GetPlan().GetStatus() !=
			agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_APPROVED ||
		!canonicalUUID(response.GetExecutionId()) {
		return nil, errors.New(
			"agent service returned an invalid Team Plan approval response",
		)
	}
	plan, err := runner.mapTeamPlan(response.GetPlan())
	if err != nil {
		return nil, err
	}
	plan["execution_id"] = response.GetExecutionId()
	return map[string]any{
		"plan":         plan,
		"execution_id": response.GetExecutionId(),
	}, nil
}

func (runner *Runner) getTeamExecution(
	ctx context.Context,
	params map[string]any,
) (map[string]any, error) {
	if err := allowTeamActionParams(params, "execution_id"); err != nil {
		return nil, err
	}
	executionID, err := requiredTeamUUIDParam(
		params,
		"execution_id",
	)
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.team.GetTeamExecutionV3(
		callContext,
		&agentv1.GetTeamExecutionV3Request{
			OwnerId:     runner.ownerID,
			ExecutionId: executionID,
		},
	)
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil ||
		response.GetExecution() == nil ||
		response.GetExecution().GetExecutionId() != executionID {
		return nil, errors.New(
			"agent service returned an invalid Team execution response",
		)
	}
	return runner.mapTeamExecution(response.GetExecution())
}

func (runner *Runner) mapTeamPlan(
	remote *agentv1.TeamPlanV3,
) (map[string]any, error) {
	if remote == nil ||
		remote.GetOwnerId() != runner.ownerID ||
		!canonicalUUID(remote.GetTaskId()) ||
		!canonicalUUID(remote.GetPlanId()) ||
		remote.GetPlanRevision() < 1 ||
		remote.GetRecordRevision() < 1 ||
		!teamDigestPattern.MatchString(remote.GetGoalDigest()) ||
		!teamDigestPattern.MatchString(remote.GetPlanDigest()) ||
		!safePublicText(remote.GetSchemaVersion(), 128, false) ||
		!safePublicText(remote.GetRegion(), 64, false) ||
		!safePublicText(remote.GetRuntimeCatalogRevision(), 128, false) ||
		!safePublicText(remote.GetPolicyRevision(), 128, false) ||
		remote.GetWorkerCount() == 0 ||
		remote.GetMaxConcurrentWorkers() == 0 ||
		remote.GetMaxConcurrentWorkers() > remote.GetWorkerCount() ||
		len(remote.GetAssignments()) != int(remote.GetWorkerCount()) {
		return nil, errors.New(
			"agent service returned an invalid Team Plan response",
		)
	}
	statusValue, ok := teamPlanStatus(remote.GetStatus())
	if !ok {
		return nil, errors.New(
			"agent service returned an invalid Team Plan response",
		)
	}
	provider, connectionID, err := mapTeamProviderScope(
		remote.GetProviderScope(),
	)
	if err != nil {
		return nil, invalidTeamPlanResponse()
	}
	quotedAt, err := requiredTimestamp(remote.GetQuotedAt())
	if err != nil {
		return nil, invalidTeamPlanResponse()
	}
	validUntil, err := requiredTimestamp(remote.GetValidUntil())
	if err != nil {
		return nil, invalidTeamPlanResponse()
	}
	createdAt, err := requiredTimestamp(remote.GetCreatedAt())
	if err != nil {
		return nil, invalidTeamPlanResponse()
	}
	updatedAt, err := requiredTimestamp(remote.GetUpdatedAt())
	if err != nil {
		return nil, invalidTeamPlanResponse()
	}
	if !safePublicText(
		remote.GetProposalRationale(),
		8<<10,
		false,
	) {
		return nil, invalidTeamPlanResponse()
	}
	assignments := make(
		[]map[string]any,
		0,
		len(remote.GetAssignments()),
	)
	for _, assignment := range remote.GetAssignments() {
		mapped, mapErr := mapTeamAssignment(assignment)
		if mapErr != nil {
			return nil, invalidTeamPlanResponse()
		}
		assignments = append(assignments, mapped)
	}
	schedule, err := mapTeamSchedule(remote.GetSchedule())
	if err != nil {
		return nil, invalidTeamPlanResponse()
	}
	cost, err := mapTeamCost(remote.GetCost())
	if err != nil {
		return nil, invalidTeamPlanResponse()
	}
	taskInput, err := mapTeamTaskInput(remote.GetTaskInput())
	if err != nil {
		return nil, invalidTeamPlanResponse()
	}
	return map[string]any{
		"schema_version":           remote.GetSchemaVersion(),
		"task_id":                  remote.GetTaskId(),
		"plan_id":                  remote.GetPlanId(),
		"plan_revision":            remote.GetPlanRevision(),
		"goal_digest":              remote.GetGoalDigest(),
		"provider":                 provider,
		"cloud_connection_id":      connectionID,
		"region":                   remote.GetRegion(),
		"runtime_catalog_revision": remote.GetRuntimeCatalogRevision(),
		"policy_revision":          remote.GetPolicyRevision(),
		"quoted_at":                quotedAt,
		"valid_until":              validUntil,
		"proposal_confidence":      remote.GetProposalConfidence(),
		"proposal_rationale":       remote.GetProposalRationale(),
		"worker_count":             remote.GetWorkerCount(),
		"max_concurrent_workers":   remote.GetMaxConcurrentWorkers(),
		"assignments":              assignments,
		"schedule":                 schedule,
		"cost":                     cost,
		"task_input":               taskInput,
		"plan_digest":              remote.GetPlanDigest(),
		"status":                   statusValue,
		"record_revision":          remote.GetRecordRevision(),
		"created_at":               createdAt,
		"updated_at":               updatedAt,
		"approval_required": remote.GetStatus() ==
			agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_READY_FOR_CONFIRMATION,
	}, nil
}

func mapTeamAssignment(
	remote *agentv1.TeamWorkerAssignmentV3,
) (map[string]any, error) {
	if remote == nil ||
		!teamRoleIDPattern.MatchString(remote.GetRoleId()) ||
		!safePublicText(remote.GetTitle(), 512, false) ||
		!safePublicText(remote.GetObjective(), 8<<10, false) ||
		!safePublicText(remote.GetRuntimeVersion(), 128, false) ||
		!safePublicText(remote.GetModelProvider(), 128, false) ||
		!safePublicText(remote.GetModel(), 256, false) ||
		!safePublicText(remote.GetInstanceType(), 128, false) {
		return nil, errors.New("invalid Team assignment")
	}
	workClass, ok := teamWorkClass(remote.GetWorkClass())
	if !ok {
		return nil, errors.New("invalid Team assignment")
	}
	workspaceMode, ok := teamWorkspaceMode(remote.GetWorkspaceMode())
	if !ok {
		return nil, errors.New("invalid Team assignment")
	}
	runtimeFamily, ok := teamRuntimeFamily(remote.GetRuntimeFamily())
	if !ok {
		return nil, errors.New("invalid Team assignment")
	}
	runtimeAdapter, ok := teamRuntimeAdapter(remote.GetRuntimeAdapter())
	if !ok {
		return nil, errors.New("invalid Team assignment")
	}
	modelInterface, ok := teamModelInterface(remote.GetModelInterface())
	if !ok {
		return nil, errors.New("invalid Team assignment")
	}
	capabilities := make(
		[]string,
		0,
		len(remote.GetRequiredCapabilities()),
	)
	for _, value := range remote.GetRequiredCapabilities() {
		mapped, found := teamCapability(value)
		if !found {
			return nil, errors.New("invalid Team assignment")
		}
		capabilities = append(capabilities, mapped)
	}
	dependencies := append([]string(nil), remote.GetDependsOnRoleIds()...)
	for _, dependency := range dependencies {
		if !teamRoleIDPattern.MatchString(dependency) {
			return nil, errors.New("invalid Team assignment")
		}
	}
	resources, err := mapTeamResources(remote.GetResources())
	if err != nil {
		return nil, err
	}
	duration, err := mapTeamDuration(remote.GetDuration())
	if err != nil {
		return nil, err
	}
	tokens, err := mapTeamTokens(remote.GetTokens())
	if err != nil {
		return nil, err
	}
	marketplace, err := mapTeamMarketplace(remote.GetMarketplace())
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"role_id":               remote.GetRoleId(),
		"title":                 remote.GetTitle(),
		"objective":             remote.GetObjective(),
		"work_class":            workClass,
		"required_capabilities": capabilities,
		"workspace_mode":        workspaceMode,
		"depends_on_role_ids":   dependencies,
		"runtime": map[string]any{
			"family":  runtimeFamily,
			"version": remote.GetRuntimeVersion(),
			"adapter": runtimeAdapter,
		},
		"model": map[string]any{
			"provider":  remote.GetModelProvider(),
			"model":     remote.GetModel(),
			"interface": modelInterface,
		},
		"compute": map[string]any{
			"instance_type":      remote.GetInstanceType(),
			"resources":          resources,
			"duration":           duration,
			"tokens":             tokens,
			"cold_start_seconds": remote.GetColdStartSeconds(),
		},
		"marketplace": marketplace,
	}, nil
}

func mapTeamResources(
	remote *agentv1.TeamResourceEnvelopeV3,
) (map[string]any, error) {
	if remote == nil ||
		remote.GetVcpu() == 0 ||
		remote.GetMemoryMib() == 0 ||
		remote.GetDiskGib() == 0 {
		return nil, errors.New("invalid Team resources")
	}
	architecture, ok := teamArchitecture(remote.GetArchitecture())
	if !ok {
		return nil, errors.New("invalid Team resources")
	}
	return map[string]any{
		"vcpu":         remote.GetVcpu(),
		"memory_mib":   remote.GetMemoryMib(),
		"disk_gib":     remote.GetDiskGib(),
		"architecture": architecture,
	}, nil
}

func mapTeamDuration(
	remote *agentv1.TeamDurationEstimateV3,
) (map[string]any, error) {
	if remote == nil ||
		remote.GetMinimumSeconds() == 0 ||
		remote.GetMinimumSeconds() > remote.GetExpectedSeconds() ||
		remote.GetExpectedSeconds() > remote.GetMaximumSeconds() {
		return nil, errors.New("invalid Team duration")
	}
	return map[string]any{
		"minimum_seconds":  remote.GetMinimumSeconds(),
		"expected_seconds": remote.GetExpectedSeconds(),
		"maximum_seconds":  remote.GetMaximumSeconds(),
	}, nil
}

func mapTeamTokens(
	remote *agentv1.TeamTokenEstimateV3,
) (map[string]any, error) {
	if remote == nil ||
		remote.GetInputMinimum() > remote.GetInputExpected() ||
		remote.GetInputExpected() > remote.GetInputMaximum() ||
		remote.GetOutputMinimum() > remote.GetOutputExpected() ||
		remote.GetOutputExpected() > remote.GetOutputMaximum() {
		return nil, errors.New("invalid Team token estimate")
	}
	return map[string]any{
		"input_minimum":   remote.GetInputMinimum(),
		"input_expected":  remote.GetInputExpected(),
		"input_maximum":   remote.GetInputMaximum(),
		"output_minimum":  remote.GetOutputMinimum(),
		"output_expected": remote.GetOutputExpected(),
		"output_maximum":  remote.GetOutputMaximum(),
	}, nil
}

func mapTeamMarketplace(
	remote *agentv1.TeamWorkerMarketplaceBindingV3,
) (map[string]any, error) {
	if remote == nil ||
		!safePublicText(remote.GetPublisherDisplayName(), 256, false) ||
		!safePublicText(remote.GetPublisherTier(), 64, false) ||
		!safePublicText(remote.GetWorkerTypeId(), 128, false) ||
		!safePublicText(remote.GetReviewRiskClass(), 64, false) ||
		remote.GetGrantedPermissions() == nil {
		return nil, errors.New("invalid Team Marketplace binding")
	}
	var reviewValidUntil any
	if remote.GetReviewValidUntil() == nil {
		if remote.GetPublisherTier() != "dirextalk_official" {
			return nil, errors.New("invalid Team Marketplace binding")
		}
	} else {
		parsed, err := requiredTimestamp(
			remote.GetReviewValidUntil(),
		)
		if err != nil {
			return nil, err
		}
		reviewValidUntil = parsed
	}
	permissions := remote.GetGrantedPermissions()
	networkServices, err := publicTextList(
		permissions.GetNetworkServices(),
		64,
		256,
	)
	if err != nil {
		return nil, err
	}
	toolScopes, err := publicTextList(
		permissions.GetToolScopes(),
		64,
		256,
	)
	if err != nil {
		return nil, err
	}
	if !safePublicText(permissions.GetWorkspace(), 64, false) {
		return nil, errors.New("invalid Team Marketplace binding")
	}
	return map[string]any{
		"worker_type_id":         remote.GetWorkerTypeId(),
		"publisher_display_name": remote.GetPublisherDisplayName(),
		"publisher_tier":         remote.GetPublisherTier(),
		"review_risk_class":      remote.GetReviewRiskClass(),
		"review_valid_until":     reviewValidUntil,
		"permissions": map[string]any{
			"workspace":         permissions.GetWorkspace(),
			"network_services":  networkServices,
			"tool_scopes":       toolScopes,
			"max_temp_disk_mib": permissions.GetMaxTempDiskMib(),
		},
	}, nil
}

func mapTeamSchedule(
	remote *agentv1.TeamScheduleEstimateV3,
) (map[string]any, error) {
	if remote == nil ||
		remote.GetMinimumWallSeconds() == 0 ||
		remote.GetMinimumWallSeconds() >
			remote.GetExpectedWallSeconds() ||
		remote.GetExpectedWallSeconds() >
			remote.GetMaximumWallSeconds() {
		return nil, errors.New("invalid Team schedule")
	}
	return map[string]any{
		"minimum_wall_seconds":  remote.GetMinimumWallSeconds(),
		"expected_wall_seconds": remote.GetExpectedWallSeconds(),
		"maximum_wall_seconds":  remote.GetMaximumWallSeconds(),
	}, nil
}

func mapTeamCost(
	remote *agentv1.TeamCostEstimateV3,
) (map[string]any, error) {
	if remote == nil ||
		!teamCurrencyPattern.MatchString(remote.GetCurrency()) ||
		remote.GetMinimumMicros() > remote.GetExpectedMicros() ||
		remote.GetExpectedMicros() > remote.GetMaximumMicros() ||
		remote.GetMaximumMicros() > remote.GetHardBudgetMicros() {
		return nil, errors.New("invalid Team cost")
	}
	assumptions, err := publicTextList(
		remote.GetAssumptions(),
		32,
		512,
	)
	if err != nil {
		return nil, err
	}
	exclusions, err := publicTextList(
		remote.GetExclusions(),
		32,
		512,
	)
	if err != nil {
		return nil, err
	}
	roles := make([]map[string]any, 0, len(remote.GetRoles()))
	for _, role := range remote.GetRoles() {
		if role == nil ||
			!teamRoleIDPattern.MatchString(role.GetRoleId()) ||
			role.GetTotalMinimumMicros() >
				role.GetTotalExpectedMicros() ||
			role.GetTotalExpectedMicros() >
				role.GetTotalMaximumMicros() {
			return nil, errors.New("invalid Team cost")
		}
		roles = append(roles, map[string]any{
			"role_id":                 role.GetRoleId(),
			"compute_minimum_micros":  role.GetComputeMinimumMicros(),
			"compute_expected_micros": role.GetComputeExpectedMicros(),
			"compute_maximum_micros":  role.GetComputeMaximumMicros(),
			"model_minimum_micros":    role.GetModelMinimumMicros(),
			"model_expected_micros":   role.GetModelExpectedMicros(),
			"model_maximum_micros":    role.GetModelMaximumMicros(),
			"total_minimum_micros":    role.GetTotalMinimumMicros(),
			"total_expected_micros":   role.GetTotalExpectedMicros(),
			"total_maximum_micros":    role.GetTotalMaximumMicros(),
		})
	}
	return map[string]any{
		"currency":           remote.GetCurrency(),
		"minimum_micros":     remote.GetMinimumMicros(),
		"expected_micros":    remote.GetExpectedMicros(),
		"maximum_micros":     remote.GetMaximumMicros(),
		"hard_budget_micros": remote.GetHardBudgetMicros(),
		"roles":              roles,
		"assumptions":        assumptions,
		"exclusions":         exclusions,
	}, nil
}

func mapTeamTaskInput(
	remote *agentv1.TeamTaskInputBindingV3,
) (any, error) {
	if remote == nil {
		return nil, nil
	}
	sourceKind, ok := teamInputSourceKind(remote.GetSourceKind())
	if !ok {
		return nil, errors.New("invalid Team task input")
	}
	result := map[string]any{"source_kind": sourceKind}
	if repository := remote.GetRepository(); repository != nil {
		for _, value := range []string{
			repository.GetProvider(),
			repository.GetHost(),
			repository.GetOwner(),
			repository.GetName(),
			repository.GetBaseCommitSha(),
			repository.GetBaseRef(),
		} {
			if !safePublicText(value, 512, false) {
				return nil, errors.New("invalid Team task input")
			}
		}
		result["repository"] = map[string]any{
			"provider":        repository.GetProvider(),
			"host":            repository.GetHost(),
			"owner":           repository.GetOwner(),
			"name":            repository.GetName(),
			"base_commit_sha": repository.GetBaseCommitSha(),
			"base_ref":        repository.GetBaseRef(),
		}
	}
	if workspace := remote.GetWorkspace(); workspace != nil {
		if workspace.GetWorkspaceSizeBytes() < 0 ||
			!safePublicText(
				workspace.GetWorkspaceMediaType(),
				128,
				false,
			) {
			return nil, errors.New("invalid Team task input")
		}
		result["workspace"] = map[string]any{
			"size_bytes": workspace.GetWorkspaceSizeBytes(),
			"media_type": workspace.GetWorkspaceMediaType(),
		}
	}
	return result, nil
}

func (runner *Runner) mapTeamApprovalChallenge(
	response *agentv1.CreateTeamApprovalChallengeV3Response,
	planID string,
	planRevision int64,
	signerKeyID string,
) (map[string]any, error) {
	if response == nil ||
		response.GetChallenge() == nil ||
		response.GetAuthorization() == nil {
		return nil, invalidTeamChallengeResponse()
	}
	challenge := response.GetChallenge()
	authorization := response.GetAuthorization()
	if challenge.GetOwnerId() != runner.ownerID ||
		challenge.GetPlanId() != planID ||
		challenge.GetPlanRevision() != planRevision ||
		challenge.GetSignerKeyId() != signerKeyID ||
		!canonicalUUID(challenge.GetApprovalId()) ||
		!canonicalUUID(challenge.GetChallengeId()) ||
		!teamDigestPattern.MatchString(challenge.GetPlanDigest()) ||
		challenge.GetChallengeRevision() < 1 ||
		challenge.GetRecordRevision() < 1 ||
		len(challenge.GetSigningPayloadCbor()) == 0 ||
		len(challenge.GetSigningPayloadCbor()) >
			maxTeamApprovalSigningPayloadSize ||
		!canonicalUUID(challenge.GetLaunchAuthorizationId()) ||
		!teamDigestPattern.MatchString(
			challenge.GetLaunchAuthorizationDigest(),
		) ||
		authorization.GetOwnerId() != runner.ownerID ||
		authorization.GetPlanId() != planID ||
		authorization.GetPlanRevision() != planRevision ||
		authorization.GetPlanDigest() != challenge.GetPlanDigest() ||
		authorization.GetApprovalId() != challenge.GetApprovalId() ||
		authorization.GetAuthorizationId() !=
			challenge.GetLaunchAuthorizationId() ||
		authorization.GetWorkerCount() != challenge.GetWorkerCount() ||
		authorization.GetMaxConcurrentBillableWorkers() !=
			challenge.GetMaxConcurrentWorkers() {
		return nil, invalidTeamChallengeResponse()
	}
	provider, _, err := mapTeamProviderScope(
		challenge.GetProviderScope(),
	)
	if err != nil {
		return nil, invalidTeamChallengeResponse()
	}
	authProvider, _, err := mapTeamProviderScope(
		authorization.GetProviderScope(),
	)
	if err != nil || authProvider != provider {
		return nil, invalidTeamChallengeResponse()
	}
	quotedAt, err := requiredTimestamp(challenge.GetQuotedAt())
	if err != nil {
		return nil, invalidTeamChallengeResponse()
	}
	quoteValidUntil, err := requiredTimestamp(
		challenge.GetQuoteValidUntil(),
	)
	if err != nil {
		return nil, invalidTeamChallengeResponse()
	}
	issuedAt, err := requiredTimestamp(challenge.GetIssuedAt())
	if err != nil {
		return nil, invalidTeamChallengeResponse()
	}
	expiresAt, err := requiredTimestamp(challenge.GetExpiresAt())
	if err != nil {
		return nil, invalidTeamChallengeResponse()
	}
	authorizationView, err := mapTeamLaunchAuthorization(authorization)
	if err != nil {
		return nil, invalidTeamChallengeResponse()
	}
	return map[string]any{
		"schema_version":         challenge.GetSchemaVersion(),
		"challenge_revision":     challenge.GetChallengeRevision(),
		"approval_id":            challenge.GetApprovalId(),
		"challenge_id":           challenge.GetChallengeId(),
		"plan_id":                challenge.GetPlanId(),
		"plan_revision":          challenge.GetPlanRevision(),
		"plan_digest":            challenge.GetPlanDigest(),
		"provider":               provider,
		"quoted_at":              quotedAt,
		"quote_valid_until":      quoteValidUntil,
		"worker_count":           challenge.GetWorkerCount(),
		"max_concurrent_workers": challenge.GetMaxConcurrentWorkers(),
		"currency":               challenge.GetCurrency(),
		"minimum_cost_micros":    challenge.GetMinimumCostMicros(),
		"expected_cost_micros":   challenge.GetExpectedCostMicros(),
		"maximum_cost_micros":    challenge.GetMaximumCostMicros(),
		"hard_budget_micros":     challenge.GetHardBudgetMicros(),
		"minimum_wall_seconds":   challenge.GetMinimumWallSeconds(),
		"expected_wall_seconds":  challenge.GetExpectedWallSeconds(),
		"maximum_wall_seconds":   challenge.GetMaximumWallSeconds(),
		"signer_key_id":          challenge.GetSignerKeyId(),
		"issued_at":              issuedAt,
		"expires_at":             expiresAt,
		"signing_payload_base64url": base64.RawURLEncoding.EncodeToString(
			challenge.GetSigningPayloadCbor(),
		),
		"record_revision":             challenge.GetRecordRevision(),
		"launch_authorization_id":     challenge.GetLaunchAuthorizationId(),
		"launch_authorization_digest": challenge.GetLaunchAuthorizationDigest(),
		"authorization":               authorizationView,
	}, nil
}

func mapTeamLaunchAuthorization(
	remote *agentv1.TeamLaunchAuthorizationV3,
) (map[string]any, error) {
	if remote == nil ||
		!canonicalUUID(remote.GetAuthorizationId()) ||
		!canonicalUUID(remote.GetPlanId()) ||
		remote.GetPlanRevision() < 1 ||
		!teamDigestPattern.MatchString(remote.GetPlanDigest()) ||
		!safePublicText(remote.GetRegion(), 64, false) ||
		!teamCurrencyPattern.MatchString(remote.GetCurrency()) ||
		remote.GetRetention() == nil ||
		remote.GetNetwork() == nil ||
		len(remote.GetRoles()) != int(remote.GetWorkerCount()) {
		return nil, errors.New("invalid Team launch authorization")
	}
	launchNotBefore, err := requiredTimestamp(
		remote.GetLaunchNotBefore(),
	)
	if err != nil {
		return nil, err
	}
	launchNotAfter, err := requiredTimestamp(remote.GetLaunchNotAfter())
	if err != nil {
		return nil, err
	}
	retention := remote.GetRetention()
	if !safePublicText(retention.GetRetentionClass(), 64, false) {
		return nil, errors.New("invalid Team launch authorization")
	}
	network := remote.GetNetwork()
	if !safePublicText(network.GetConnectivityMode(), 64, false) {
		return nil, errors.New("invalid Team launch authorization")
	}
	roles := make([]map[string]any, 0, len(remote.GetRoles()))
	for _, role := range remote.GetRoles() {
		if role == nil ||
			!teamRoleIDPattern.MatchString(role.GetRoleId()) ||
			!safePublicText(role.GetInstanceType(), 128, false) {
			return nil, errors.New("invalid Team launch authorization")
		}
		architecture, ok := teamArchitecture(role.GetArchitecture())
		if !ok {
			return nil, errors.New("invalid Team launch authorization")
		}
		roles = append(roles, map[string]any{
			"role_id":                      role.GetRoleId(),
			"instance_type":                role.GetInstanceType(),
			"architecture":                 architecture,
			"vcpu":                         role.GetVcpu(),
			"memory_mib":                   role.GetMemoryMib(),
			"maximum_approved_cost_micros": role.GetMaximumApprovedCostMicros(),
		})
	}
	return map[string]any{
		"authorization_id":                remote.GetAuthorizationId(),
		"region":                          remote.GetRegion(),
		"worker_count":                    remote.GetWorkerCount(),
		"max_concurrent_billable_workers": remote.GetMaxConcurrentBillableWorkers(),
		"currency":                        remote.GetCurrency(),
		"hard_budget_micros":              remote.GetHardBudgetMicros(),
		"requires_fresh_quote":            remote.GetRequiresFreshQuote(),
		"maximum_quote_age_seconds":       remote.GetMaximumQuoteAgeSeconds(),
		"launch_not_before":               launchNotBefore,
		"launch_not_after":                launchNotAfter,
		"network": map[string]any{
			"connectivity_mode": network.GetConnectivityMode(),
			"public_ipv4":       network.GetPublicIpv4(),
			"public_inbound":    network.GetPublicInbound(),
		},
		"retention": map[string]any{
			"class":                    retention.GetRetentionClass(),
			"auto_destroy":             retention.GetAutoDestroy(),
			"maximum_lifetime_seconds": retention.GetMaximumLifetimeSeconds(),
			"destroy_grace_seconds":    retention.GetDestroyGraceSeconds(),
		},
		"roles": roles,
	}, nil
}

func teamApprovalFromParams(
	value any,
) (*agentv1.TeamApprovalSignatureV3, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, teamParameterError("approval is required")
	}
	if err := allowTeamActionParams(
		raw,
		"schema_version",
		"approval_id",
		"challenge_id",
		"plan_id",
		"plan_revision",
		"plan_digest",
		"signer_key_id",
		"signature_base64url",
		"launch_authorization_id",
		"launch_authorization_digest",
	); err != nil {
		return nil, err
	}
	schemaVersion, err := requiredTeamTextParam(
		raw,
		"schema_version",
		128,
	)
	if err != nil || schemaVersion != teamSignatureSchemaV2 {
		return nil, teamParameterError("schema_version is invalid")
	}
	approvalID, err := requiredTeamUUIDParam(raw, "approval_id")
	if err != nil {
		return nil, err
	}
	challengeID, err := requiredTeamUUIDParam(raw, "challenge_id")
	if err != nil {
		return nil, err
	}
	planID, err := requiredTeamUUIDParam(raw, "plan_id")
	if err != nil {
		return nil, err
	}
	planRevision, err := positiveTeamRevisionParam(
		raw,
		"plan_revision",
	)
	if err != nil {
		return nil, err
	}
	planDigest, err := requiredTeamDigestParam(raw, "plan_digest")
	if err != nil {
		return nil, err
	}
	signerKeyID, err := requiredTeamKeyIDParam(
		raw,
		"signer_key_id",
	)
	if err != nil {
		return nil, err
	}
	signatureText, err := requiredTeamTextParam(
		raw,
		"signature_base64url",
		128,
	)
	if err != nil {
		return nil, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureText)
	if err != nil ||
		len(signature) != 64 ||
		base64.RawURLEncoding.EncodeToString(signature) != signatureText {
		clear(signature)
		return nil, teamParameterError("signature_base64url is invalid")
	}
	authorizationID, err := requiredTeamUUIDParam(
		raw,
		"launch_authorization_id",
	)
	if err != nil {
		clear(signature)
		return nil, err
	}
	authorizationDigest, err := requiredTeamDigestParam(
		raw,
		"launch_authorization_digest",
	)
	if err != nil {
		clear(signature)
		return nil, err
	}
	return &agentv1.TeamApprovalSignatureV3{
		SchemaVersion:             schemaVersion,
		ApprovalId:                approvalID,
		ChallengeId:               challengeID,
		PlanId:                    planID,
		PlanRevision:              planRevision,
		PlanDigest:                planDigest,
		SignerKeyId:               signerKeyID,
		Signature:                 signature,
		LaunchAuthorizationId:     authorizationID,
		LaunchAuthorizationDigest: authorizationDigest,
	}, nil
}

func (runner *Runner) mapTeamExecution(
	remote *agentv1.TeamExecutionV3,
) (map[string]any, error) {
	if remote == nil ||
		remote.GetOwnerId() != runner.ownerID ||
		!canonicalUUID(remote.GetExecutionId()) ||
		!canonicalUUID(remote.GetTaskId()) ||
		!canonicalUUID(remote.GetPlanId()) ||
		remote.GetPlanRevision() < 1 ||
		remote.GetRecordRevision() < 1 ||
		!teamDigestPattern.MatchString(remote.GetPlanDigest()) ||
		remote.GetWorkerCount() == 0 ||
		remote.GetMaxConcurrentWorkers() == 0 ||
		remote.GetMaxConcurrentWorkers() > remote.GetWorkerCount() {
		return nil, invalidTeamExecutionResponse()
	}
	statusValue, ok := teamExecutionStatus(remote.GetStatus())
	if !ok {
		return nil, invalidTeamExecutionResponse()
	}
	createdAt, err := requiredTimestamp(remote.GetCreatedAt())
	if err != nil {
		return nil, invalidTeamExecutionResponse()
	}
	updatedAt, err := requiredTimestamp(remote.GetUpdatedAt())
	if err != nil {
		return nil, invalidTeamExecutionResponse()
	}
	var report any
	artifacts := make([]map[string]any, 0)
	if remote.GetReport() != nil {
		if remote.GetStatus() !=
			agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_COMPLETED {
			return nil, invalidTeamExecutionResponse()
		}
		report, err = runner.mapTeamExecutionReport(
			remote.GetReport(),
			remote,
		)
		if err != nil {
			return nil, invalidTeamExecutionResponse()
		}
		artifacts, err = mapTeamExecutionArtifacts(remote)
		if err != nil {
			return nil, invalidTeamExecutionResponse()
		}
	} else if remote.GetStatus() ==
		agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_COMPLETED {
		return nil, invalidTeamExecutionResponse()
	} else if len(remote.GetArtifacts()) != 0 {
		return nil, invalidTeamExecutionResponse()
	}
	taskInput, err := mapTeamTaskInput(remote.GetTaskInput())
	if err != nil {
		return nil, invalidTeamExecutionResponse()
	}
	return map[string]any{
		"schema_version":         remote.GetSchemaVersion(),
		"execution_id":           remote.GetExecutionId(),
		"task_id":                remote.GetTaskId(),
		"plan_id":                remote.GetPlanId(),
		"plan_revision":          remote.GetPlanRevision(),
		"plan_digest":            remote.GetPlanDigest(),
		"status":                 statusValue,
		"worker_count":           remote.GetWorkerCount(),
		"max_concurrent_workers": remote.GetMaxConcurrentWorkers(),
		"record_revision":        remote.GetRecordRevision(),
		"created_at":             createdAt,
		"updated_at":             updatedAt,
		"task_input":             taskInput,
		"cleanup_verified": remote.GetStatus() ==
			agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_COMPLETED,
		"report":    report,
		"artifacts": artifacts,
	}, nil
}

func mapTeamExecutionArtifacts(
	execution *agentv1.TeamExecutionV3,
) ([]map[string]any, error) {
	if execution == nil || execution.GetReport() == nil ||
		execution.GetStatus() !=
			agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_COMPLETED ||
		len(execution.GetArtifacts()) == 0 ||
		len(execution.GetArtifacts()) >
			int(execution.GetWorkerCount())*maxTeamArtifactsPerRole {
		return nil, errors.New("invalid Team execution artifacts")
	}
	executionCreatedAt := execution.GetCreatedAt().AsTime().UTC()
	executionUpdatedAt := execution.GetUpdatedAt().AsTime().UTC()
	seenIDs := make(map[string]struct{}, len(execution.GetArtifacts()))
	seenBindings := make(map[string]struct{}, len(execution.GetArtifacts()))
	artifacts := make(
		[]map[string]any,
		0,
		len(execution.GetArtifacts()),
	)
	for _, artifact := range execution.GetArtifacts() {
		if artifact == nil ||
			artifact.GetSchemaVersion() != teamArtifactSchemaV1 ||
			!canonicalUUID(artifact.GetArtifactId()) ||
			!teamRoleIDPattern.MatchString(artifact.GetRoleId()) ||
			!teamActionIDPattern.MatchString(artifact.GetActionId()) ||
			!teamArtifactNamePattern.MatchString(artifact.GetName()) ||
			!validTeamArtifactKind(
				artifact.GetKind(),
				artifact.GetName(),
			) ||
			!validTeamArtifactMediaType(artifact.GetMediaType()) ||
			artifact.GetSizeBytes() < 1 ||
			artifact.GetSizeBytes() > maxTeamArtifactBytes ||
			!teamDigestPattern.MatchString(artifact.GetSha256()) ||
			artifact.GetVerification() != "passed" {
			return nil, errors.New("invalid Team execution artifact")
		}
		createdAt, err := requiredTimestamp(artifact.GetCreatedAt())
		if err != nil {
			return nil, err
		}
		retentionExpiresAt, err := requiredTimestamp(
			artifact.GetRetentionExpiresAt(),
		)
		if err != nil {
			return nil, err
		}
		artifactCreatedAt := artifact.GetCreatedAt().AsTime().UTC()
		artifactExpiresAt := artifact.GetRetentionExpiresAt().AsTime().UTC()
		if artifactCreatedAt.Before(executionCreatedAt) ||
			artifactCreatedAt.After(executionUpdatedAt) ||
			!artifactExpiresAt.After(artifactCreatedAt) ||
			artifactExpiresAt.After(
				artifactCreatedAt.Add(366*24*time.Hour),
			) ||
			!teamArtifactMatchesReport(artifact, execution.GetReport()) {
			return nil, errors.New("invalid Team execution artifact binding")
		}
		binding := strings.Join([]string{
			artifact.GetRoleId(),
			artifact.GetActionId(),
			artifact.GetName(),
		}, "\x00")
		if _, duplicate := seenIDs[artifact.GetArtifactId()]; duplicate {
			return nil, errors.New("duplicate Team execution artifact")
		}
		if _, duplicate := seenBindings[binding]; duplicate {
			return nil, errors.New("duplicate Team execution artifact binding")
		}
		seenIDs[artifact.GetArtifactId()] = struct{}{}
		seenBindings[binding] = struct{}{}
		artifacts = append(artifacts, map[string]any{
			"schema_version":       artifact.GetSchemaVersion(),
			"artifact_id":          artifact.GetArtifactId(),
			"role_id":              artifact.GetRoleId(),
			"action_id":            artifact.GetActionId(),
			"name":                 artifact.GetName(),
			"kind":                 artifact.GetKind(),
			"media_type":           artifact.GetMediaType(),
			"size_bytes":           artifact.GetSizeBytes(),
			"sha256":               artifact.GetSha256(),
			"verification":         artifact.GetVerification(),
			"created_at":           createdAt,
			"retention_expires_at": retentionExpiresAt,
		})
	}
	return artifacts, nil
}

func teamArtifactMatchesReport(
	artifact *agentv1.TeamExecutionArtifactV3,
	report *agentv1.TeamExecutionReportV3,
) bool {
	for _, role := range report.GetRoles() {
		if role.GetRoleId() != artifact.GetRoleId() {
			continue
		}
		for _, final := range role.GetFinals() {
			if final.GetActionId() != artifact.GetActionId() {
				continue
			}
			return artifact.GetName() != "final.json" ||
				final.GetArtifactSha256() == artifact.GetSha256()
		}
	}
	return false
}

func validTeamArtifactKind(kind, name string) bool {
	switch name {
	case "final.json":
		return kind == "result"
	case "changes.patch":
		return kind == "patch"
	default:
		return kind == "file"
	}
}

func validTeamArtifactMediaType(value string) bool {
	return value == "application/json" ||
		value == "text/plain; charset=utf-8"
}

func (runner *Runner) mapTeamExecutionReport(
	remote *agentv1.TeamExecutionReportV3,
	execution *agentv1.TeamExecutionV3,
) (map[string]any, error) {
	if remote == nil ||
		execution == nil ||
		remote.GetOwnerId() != runner.ownerID ||
		remote.GetExecutionId() != execution.GetExecutionId() ||
		remote.GetTaskId() != execution.GetTaskId() ||
		remote.GetPlanId() != execution.GetPlanId() ||
		remote.GetPlanRevision() != execution.GetPlanRevision() ||
		remote.GetPlanDigest() != execution.GetPlanDigest() ||
		!teamDigestPattern.MatchString(remote.GetReportDigest()) ||
		len(remote.GetRoles()) != int(execution.GetWorkerCount()) {
		return nil, errors.New("invalid Team execution report")
	}
	generatedAt, err := requiredTimestamp(remote.GetGeneratedAt())
	if err != nil {
		return nil, err
	}
	totalUsage, err := mapTeamRuntimeUsage(remote.GetTotalUsage())
	if err != nil {
		return nil, err
	}
	roles := make([]map[string]any, 0, len(remote.GetRoles()))
	for _, role := range remote.GetRoles() {
		if role == nil ||
			!teamRoleIDPattern.MatchString(role.GetRoleId()) ||
			!safePublicText(role.GetTitle(), 512, false) ||
			!safePublicText(role.GetOutcome(), 64, false) ||
			!teamDigestPattern.MatchString(
				role.GetResultEvidenceDigest(),
			) ||
			len(role.GetFinals()) == 0 {
			return nil, errors.New("invalid Team execution report")
		}
		runtimeFamily, ok := teamRuntimeFamily(
			role.GetRuntimeFamily(),
		)
		if !ok {
			return nil, errors.New("invalid Team execution report")
		}
		runtimeAdapter, ok := teamRuntimeAdapter(
			role.GetRuntimeAdapter(),
		)
		if !ok {
			return nil, errors.New("invalid Team execution report")
		}
		finals := make(
			[]map[string]any,
			0,
			len(role.GetFinals()),
		)
		for _, final := range role.GetFinals() {
			mapped, mapErr := mapTeamExecutionFinal(final)
			if mapErr != nil {
				return nil, mapErr
			}
			finals = append(finals, mapped)
		}
		roles = append(roles, map[string]any{
			"role_id":                role.GetRoleId(),
			"title":                  role.GetTitle(),
			"runtime_family":         runtimeFamily,
			"runtime_adapter":        runtimeAdapter,
			"outcome":                role.GetOutcome(),
			"result_evidence_digest": role.GetResultEvidenceDigest(),
			"finals":                 finals,
		})
	}
	return map[string]any{
		"schema_version": remote.GetSchemaVersion(),
		"roles":          roles,
		"total_usage":    totalUsage,
		"report_digest":  remote.GetReportDigest(),
		"generated_at":   generatedAt,
	}, nil
}

func mapTeamExecutionFinal(
	remote *agentv1.TeamExecutionFinalV3,
) (map[string]any, error) {
	if remote == nil ||
		!teamActionIDPattern.MatchString(remote.GetActionId()) ||
		!safePublicText(remote.GetStatus(), 64, false) ||
		!safePublicText(remote.GetSummary(), 8<<10, false) ||
		!teamDigestPattern.MatchString(remote.GetArtifactSha256()) {
		return nil, errors.New("invalid Team execution final")
	}
	runtimeAdapter, ok := teamRuntimeAdapter(
		remote.GetRuntimeAdapter(),
	)
	if !ok {
		return nil, errors.New("invalid Team execution final")
	}
	usage, err := mapTeamRuntimeUsage(remote.GetUsage())
	if err != nil {
		return nil, err
	}
	deliverables, err := publicTextList(
		remote.GetDeliverables(),
		64,
		8<<10,
	)
	if err != nil {
		return nil, err
	}
	tests, err := publicTextList(remote.GetTests(), 64, 8<<10)
	if err != nil {
		return nil, err
	}
	risks, err := publicTextList(remote.GetRisks(), 64, 8<<10)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"action_id":       remote.GetActionId(),
		"runtime_adapter": runtimeAdapter,
		"usage":           usage,
		"status":          remote.GetStatus(),
		"summary":         remote.GetSummary(),
		"deliverables":    deliverables,
		"tests":           tests,
		"risks":           risks,
		"artifact_sha256": remote.GetArtifactSha256(),
	}, nil
}

func mapTeamRuntimeUsage(
	remote *agentv1.TeamRuntimeUsageV3,
) (map[string]any, error) {
	if remote == nil ||
		remote.GetInputTokens() < 0 ||
		remote.GetCachedInputTokens() < 0 ||
		remote.GetOutputTokens() < 0 ||
		remote.GetReasoningOutputTokens() < 0 {
		return nil, errors.New("invalid Team runtime usage")
	}
	return map[string]any{
		"input_tokens":            remote.GetInputTokens(),
		"cached_input_tokens":     remote.GetCachedInputTokens(),
		"output_tokens":           remote.GetOutputTokens(),
		"reasoning_output_tokens": remote.GetReasoningOutputTokens(),
	}, nil
}

func mapTeamProviderScope(
	remote *agentv1.TeamProviderScopeV3,
) (string, string, error) {
	if remote == nil ||
		!canonicalUUID(remote.GetCloudConnectionId()) ||
		remote.GetCloudConnectionRevision() < 1 {
		return "", "", errors.New("invalid Team provider scope")
	}
	switch remote.GetProvider() {
	case agentv1.TeamCloudProviderV3_TEAM_CLOUD_PROVIDER_V3_AWS:
		return "aws", remote.GetCloudConnectionId(), nil
	default:
		return "", "", errors.New("invalid Team provider scope")
	}
}

func teamPlanStatus(
	value agentv1.TeamPlanStatusV3,
) (string, bool) {
	switch value {
	case agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_READY_FOR_CONFIRMATION:
		return "ready_for_confirmation", true
	case agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_APPROVED:
		return "approved", true
	case agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_EXPIRED:
		return "expired", true
	case agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_SUPERSEDED:
		return "superseded", true
	case agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_EXECUTING:
		return "executing", true
	case agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_COMPLETED:
		return "completed", true
	case agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_FAILED:
		return "failed", true
	case agentv1.TeamPlanStatusV3_TEAM_PLAN_STATUS_V3_CANCELED:
		return "canceled", true
	default:
		return "", false
	}
}

func teamExecutionStatus(
	value agentv1.TeamExecutionStatusV3,
) (string, bool) {
	switch value {
	case agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_MATERIALIZED:
		return "materialized", true
	case agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_DISPATCHING:
		return "dispatching", true
	case agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_RUNNING:
		return "running", true
	case agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_VERIFYING:
		return "verifying", true
	case agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_COMPLETED:
		return "completed", true
	case agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_FAILED:
		return "failed", true
	case agentv1.TeamExecutionStatusV3_TEAM_EXECUTION_STATUS_V3_CANCELED:
		return "canceled", true
	default:
		return "", false
	}
}

func teamRuntimeFamily(
	value agentv1.TeamRuntimeFamilyV3,
) (string, bool) {
	switch value {
	case agentv1.TeamRuntimeFamilyV3_TEAM_RUNTIME_FAMILY_V3_CLAUDE_CODE:
		return "claude_code", true
	case agentv1.TeamRuntimeFamilyV3_TEAM_RUNTIME_FAMILY_V3_CODEX:
		return "codex", true
	case agentv1.TeamRuntimeFamilyV3_TEAM_RUNTIME_FAMILY_V3_OPENCLAW:
		return "openclaw", true
	case agentv1.TeamRuntimeFamilyV3_TEAM_RUNTIME_FAMILY_V3_HERMES:
		return "hermes", true
	case agentv1.TeamRuntimeFamilyV3_TEAM_RUNTIME_FAMILY_V3_OPENCODE:
		return "opencode", true
	case agentv1.TeamRuntimeFamilyV3_TEAM_RUNTIME_FAMILY_V3_PI:
		return "pi", true
	default:
		return "", false
	}
}

func teamRuntimeAdapter(
	value agentv1.TeamRuntimeAdapterV3,
) (string, bool) {
	switch value {
	case agentv1.TeamRuntimeAdapterV3_TEAM_RUNTIME_ADAPTER_V3_CLAUDE_CODE_TASK_V1:
		return "claude_code_task_v1", true
	case agentv1.TeamRuntimeAdapterV3_TEAM_RUNTIME_ADAPTER_V3_CODEX_EXEC_TASK_V1:
		return "codex_exec_task_v1", true
	case agentv1.TeamRuntimeAdapterV3_TEAM_RUNTIME_ADAPTER_V3_OPENCLAW_GATEWAY_TASK_V1:
		return "openclaw_gateway_task_v1", true
	case agentv1.TeamRuntimeAdapterV3_TEAM_RUNTIME_ADAPTER_V3_HERMES_API_TASK_V1:
		return "hermes_api_task_v1", true
	case agentv1.TeamRuntimeAdapterV3_TEAM_RUNTIME_ADAPTER_V3_OPENCODE_SERVER_TASK_V1:
		return "opencode_server_task_v1", true
	case agentv1.TeamRuntimeAdapterV3_TEAM_RUNTIME_ADAPTER_V3_PI_JSON_TASK_V1:
		return "pi_json_task_v1", true
	default:
		return "", false
	}
}

func teamWorkClass(value agentv1.TeamWorkClassV3) (string, bool) {
	switch value {
	case agentv1.TeamWorkClassV3_TEAM_WORK_CLASS_V3_SOFTWARE_IMPLEMENTATION:
		return "software_implementation", true
	case agentv1.TeamWorkClassV3_TEAM_WORK_CLASS_V3_SOFTWARE_REVIEW:
		return "software_review", true
	case agentv1.TeamWorkClassV3_TEAM_WORK_CLASS_V3_SOFTWARE_TEST:
		return "software_test", true
	case agentv1.TeamWorkClassV3_TEAM_WORK_CLASS_V3_RESEARCH:
		return "research", true
	case agentv1.TeamWorkClassV3_TEAM_WORK_CLASS_V3_BROWSER_AUTOMATION:
		return "browser_automation", true
	case agentv1.TeamWorkClassV3_TEAM_WORK_CLASS_V3_COMMUNICATION_AUTOMATION:
		return "communication_automation", true
	case agentv1.TeamWorkClassV3_TEAM_WORK_CLASS_V3_GENERAL_TOOL:
		return "general_tool", true
	case agentv1.TeamWorkClassV3_TEAM_WORK_CLASS_V3_LONG_RUNNING_OPERATIONS:
		return "long_running_operations", true
	default:
		return "", false
	}
}

func teamWorkspaceMode(
	value agentv1.TeamWorkspaceModeV3,
) (string, bool) {
	switch value {
	case agentv1.TeamWorkspaceModeV3_TEAM_WORKSPACE_MODE_V3_READ_ONLY:
		return "read_only", true
	case agentv1.TeamWorkspaceModeV3_TEAM_WORKSPACE_MODE_V3_ISOLATED:
		return "isolated", true
	case agentv1.TeamWorkspaceModeV3_TEAM_WORKSPACE_MODE_V3_EXCLUSIVE:
		return "exclusive", true
	default:
		return "", false
	}
}

func teamCapability(
	value agentv1.TeamCapabilityV3,
) (string, bool) {
	switch value {
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_REPOSITORY_READ:
		return "repository_read", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_REPOSITORY_WRITE:
		return "repository_write", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_CODE_REVIEW:
		return "code_review", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_SHELL:
		return "shell", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_GIT:
		return "git", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_TEST:
		return "test", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_WEB_RESEARCH:
		return "web_research", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_BROWSER:
		return "browser", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_MCP_CLIENT:
		return "mcp_client", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_ACP:
		return "acp", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_LONG_MEMORY:
		return "long_memory", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_SUBAGENTS:
		return "subagents", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_MESSAGING:
		return "messaging", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_DOCUMENT:
		return "document", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_DATA_ANALYSIS:
		return "data_analysis", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_LONG_RUNNING:
		return "long_running", true
	case agentv1.TeamCapabilityV3_TEAM_CAPABILITY_V3_STRUCTURED_RESULTS:
		return "structured_results", true
	default:
		return "", false
	}
}

func teamModelInterface(
	value agentv1.TeamModelInterfaceV3,
) (string, bool) {
	switch value {
	case agentv1.TeamModelInterfaceV3_TEAM_MODEL_INTERFACE_V3_ANTHROPIC_API:
		return "anthropic_api", true
	case agentv1.TeamModelInterfaceV3_TEAM_MODEL_INTERFACE_V3_OPENAI_RESPONSES:
		return "openai_responses", true
	case agentv1.TeamModelInterfaceV3_TEAM_MODEL_INTERFACE_V3_OPENAI_COMPATIBLE:
		return "openai_compatible", true
	default:
		return "", false
	}
}

func teamArchitecture(
	value agentv1.TeamArchitectureV3,
) (string, bool) {
	switch value {
	case agentv1.TeamArchitectureV3_TEAM_ARCHITECTURE_V3_AMD64:
		return "amd64", true
	case agentv1.TeamArchitectureV3_TEAM_ARCHITECTURE_V3_ARM64:
		return "arm64", true
	default:
		return "", false
	}
}

func teamInputSourceKind(
	value agentv1.TeamInputSourceKindV3,
) (string, bool) {
	switch value {
	case agentv1.TeamInputSourceKindV3_TEAM_INPUT_SOURCE_KIND_V3_EMPTY:
		return "empty", true
	case agentv1.TeamInputSourceKindV3_TEAM_INPUT_SOURCE_KIND_V3_GITHUB_REPOSITORY:
		return "github_repository", true
	case agentv1.TeamInputSourceKindV3_TEAM_INPUT_SOURCE_KIND_V3_WORKSPACE_ARCHIVE:
		return "workspace_archive", true
	default:
		return "", false
	}
}

func allowTeamActionParams(
	params map[string]any,
	allowed ...string,
) error {
	if params == nil {
		return nil
	}
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range params {
		if _, ok := set[key]; !ok {
			return teamParameterError("unsupported field")
		}
	}
	return nil
}

func requiredTeamUUIDParam(
	params map[string]any,
	key string,
) (string, error) {
	value, ok := params[key].(string)
	value = strings.TrimSpace(value)
	if !ok || !canonicalUUID(value) {
		return "", teamParameterError(
			key + " must be a canonical UUID",
		)
	}
	return value, nil
}

func positiveTeamRevisionParam(
	params map[string]any,
	key string,
) (int64, error) {
	value, err := nonnegativeInt64(params[key])
	if err != nil || value < 1 {
		return 0, teamParameterError(key + " must be positive")
	}
	return value, nil
}

func requiredTeamTextParam(
	params map[string]any,
	key string,
	maxRunes int,
) (string, error) {
	value, ok := params[key].(string)
	value = strings.TrimSpace(value)
	if !ok || !safePublicText(value, maxRunes, false) {
		return "", teamParameterError(key + " is invalid")
	}
	return value, nil
}

func requiredTeamKeyIDParam(
	params map[string]any,
	key string,
) (string, error) {
	value, err := requiredTeamTextParam(params, key, 128)
	if err != nil || !teamKeyIDPattern.MatchString(value) {
		return "", teamParameterError(key + " is invalid")
	}
	return value, nil
}

func requiredTeamDigestParam(
	params map[string]any,
	key string,
) (string, error) {
	value, err := requiredTeamTextParam(params, key, 80)
	if err != nil || !teamDigestPattern.MatchString(value) {
		return "", teamParameterError(key + " is invalid")
	}
	return value, nil
}

func teamParameterError(detail string) error {
	return errors.New("invalid agent team parameters: " + detail)
}

func invalidTeamPlanResponse() error {
	return errors.New(
		"agent service returned an invalid Team Plan response",
	)
}

func invalidTeamChallengeResponse() error {
	return errors.New(
		"agent service returned an invalid Team approval challenge",
	)
}

func invalidTeamExecutionResponse() error {
	return errors.New(
		"agent service returned an invalid Team execution response",
	)
}
