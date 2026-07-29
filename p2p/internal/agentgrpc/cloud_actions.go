package agentgrpc

import (
	"context"
	"encoding/base64"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	actionCloudTasksList              = "agent.cloud.tasks.list"
	actionCloudTasksGet               = "agent.cloud.tasks.get"
	actionCloudTasksCancel            = "agent.cloud.tasks.cancel"
	actionCloudPlansList              = "agent.cloud.plans.list"
	actionCloudPlansGet               = "agent.cloud.plans.get"
	actionCloudPlanConfirmation       = "agent.cloud.plans.confirmation.prepare"
	actionCloudPlanApprove            = "agent.cloud.plans.approve"
	actionCloudDeploymentsList        = "agent.cloud.deployments.list"
	actionCloudDeploymentsGet         = "agent.cloud.deployments.get"
	actionCloudWorkersList            = "agent.cloud.workers.list"
	actionCloudWorkersGet             = "agent.cloud.workers.get"
	maxCloudActionPageSize      int64 = 100
	maxCloudActionTokenBytes          = 2048
)

var (
	approvalDeviceKeyPattern = regexp.MustCompile(`^cloud-device-[0-9a-f]{24}$`)
	challengeIDPattern       = regexp.MustCompile(`^challenge_[A-Za-z0-9_-]{43}$`)
)

func isCloudAction(action string) bool {
	switch strings.TrimSpace(action) {
	case actionCloudTasksList, actionCloudTasksGet, actionCloudTasksCancel,
		actionCloudPlansList, actionCloudPlansGet, actionCloudPlanConfirmation, actionCloudPlanApprove,
		actionCloudDeploymentsList, actionCloudDeploymentsGet, actionCloudWorkersList, actionCloudWorkersGet:
		return true
	default:
		return false
	}
}

func (runner *Runner) invokeCloudAction(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	switch action {
	case actionCloudTasksList:
		return runner.listCloudTasks(ctx, params)
	case actionCloudTasksGet:
		return runner.getCloudTask(ctx, params)
	case actionCloudTasksCancel:
		return runner.cancelCloudTask(ctx, params)
	case actionCloudPlansList:
		return runner.listCloudPlans(ctx, params)
	case actionCloudPlansGet:
		return runner.getCloudPlan(ctx, params)
	case actionCloudPlanConfirmation:
		return runner.prepareCloudPlanConfirmation(ctx, params)
	case actionCloudPlanApprove:
		return runner.approveCloudPlan(ctx, params)
	case actionCloudDeploymentsList:
		return runner.listCloudDeployments(ctx, params)
	case actionCloudDeploymentsGet:
		return runner.getCloudDeployment(ctx, params)
	case actionCloudWorkersList:
		return runner.listCloudWorkers(ctx, params)
	case actionCloudWorkersGet:
		return runner.getCloudWorker(ctx, params)
	default:
		return nil, errors.New("agent service action is not supported")
	}
}

func (runner *Runner) listCloudTasks(ctx context.Context, params map[string]any) (map[string]any, error) {
	if runner.tasks == nil {
		return nil, errors.New("agent task service is unavailable")
	}
	if err := allowActionParams(params, "page_size", "page_token"); err != nil {
		return nil, err
	}
	pageSize, pageToken, err := cloudPage(params)
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.tasks.ListTasks(callContext, &agentv1.ListTasksRequest{
		OwnerId: runner.ownerID, PageSize: pageSize, PageToken: pageToken,
	})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil {
		return nil, errors.New("agent service returned an invalid task response")
	}
	items := make([]map[string]any, 0, len(response.GetTasks()))
	for _, remote := range response.GetTasks() {
		item, mapErr := runner.mapTask(remote)
		if mapErr != nil {
			return nil, mapErr
		}
		items = append(items, item)
	}
	next, err := cloudPageToken(response.GetNextPageToken())
	if err != nil {
		return nil, errors.New("agent service returned an invalid task response")
	}
	return map[string]any{"tasks": items, "next_page_token": next}, nil
}

func (runner *Runner) getCloudTask(ctx context.Context, params map[string]any) (map[string]any, error) {
	if runner.tasks == nil {
		return nil, errors.New("agent task service is unavailable")
	}
	if err := allowActionParams(params, "task_id"); err != nil {
		return nil, err
	}
	taskID, err := requiredUUIDParam(params, "task_id")
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.tasks.GetTask(callContext, &agentv1.GetTaskRequest{TaskId: taskID})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil || response.GetTask() == nil {
		return nil, errors.New("agent service returned an invalid task response")
	}
	item, err := runner.mapTask(response.GetTask())
	if err != nil {
		return nil, err
	}
	stepsResponse, err := runner.tasks.ListSteps(callContext, &agentv1.ListStepsRequest{TaskId: taskID})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if stepsResponse == nil {
		return nil, errors.New("agent service returned an invalid task response")
	}
	steps := make([]map[string]any, 0, len(stepsResponse.GetSteps()))
	for _, remote := range stepsResponse.GetSteps() {
		step, mapErr := mapTaskStep(remote, taskID)
		if mapErr != nil {
			return nil, mapErr
		}
		steps = append(steps, step)
	}
	item["steps"] = steps
	return item, nil
}

func (runner *Runner) cancelCloudTask(ctx context.Context, params map[string]any) (map[string]any, error) {
	if runner.tasks == nil {
		return nil, errors.New("agent task service is unavailable")
	}
	if err := allowActionParams(params, "idempotency_key", "task_id", "expected_revision", "reason"); err != nil {
		return nil, err
	}
	idempotencyKey, err := requiredUUIDParam(params, "idempotency_key")
	if err != nil {
		return nil, err
	}
	taskID, err := requiredUUIDParam(params, "task_id")
	if err != nil {
		return nil, err
	}
	expectedRevision, err := positiveRevisionParam(params, "expected_revision")
	if err != nil {
		return nil, err
	}
	reason, err := boundedTextParam(params, "reason", 512, false)
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.tasks.CancelTask(callContext, &agentv1.CancelTaskRequest{
		IdempotencyKey: idempotencyKey, TaskId: taskID, ExpectedRevision: expectedRevision, Reason: reason,
	})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil || response.GetTask() == nil {
		return nil, errors.New("agent service returned an invalid task response")
	}
	return runner.mapTask(response.GetTask())
}

func (runner *Runner) listCloudPlans(ctx context.Context, params map[string]any) (map[string]any, error) {
	if runner.cloud == nil {
		return nil, errors.New("agent cloud service is unavailable")
	}
	if err := allowActionParams(params, "page_size", "page_token"); err != nil {
		return nil, err
	}
	pageSize, pageToken, err := cloudPage(params)
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.cloud.ListCloudPlans(callContext, &agentv1.ListCloudPlansRequest{
		OwnerId: runner.ownerID, PageSize: pageSize, PageToken: pageToken,
	})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil {
		return nil, errors.New("agent service returned an invalid plan response")
	}
	items := make([]map[string]any, 0, len(response.GetPlans()))
	for _, remote := range response.GetPlans() {
		item, mapErr := runner.mapCloudPlan(remote)
		if mapErr != nil {
			return nil, mapErr
		}
		items = append(items, item)
	}
	next, err := cloudPageToken(response.GetNextPageToken())
	if err != nil {
		return nil, errors.New("agent service returned an invalid plan response")
	}
	return map[string]any{"plans": items, "next_page_token": next}, nil
}

func (runner *Runner) getCloudPlan(ctx context.Context, params map[string]any) (map[string]any, error) {
	if err := allowActionParams(params, "plan_id"); err != nil {
		return nil, err
	}
	planID, err := requiredUUIDParam(params, "plan_id")
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	remote, err := runner.getCloudPlanProto(callContext, planID)
	if err != nil {
		return nil, err
	}
	item, err := runner.mapCloudPlan(remote)
	if err != nil {
		return nil, err
	}
	quoteResponse, err := runner.cloud.GetCloudQuote(callContext, &agentv1.GetCloudQuoteRequest{
		QuoteId: remote.GetQuoteId(), OwnerId: runner.ownerID,
	})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	quote, err := mapPlanQuote(quoteResponse.GetQuote(), remote)
	if err != nil {
		return nil, err
	}
	item["quote"] = quote
	return item, nil
}

func (runner *Runner) prepareCloudPlanConfirmation(ctx context.Context, params map[string]any) (map[string]any, error) {
	if err := allowActionParams(params, "idempotency_key", "plan_id", "expected_revision", "signer_key_id"); err != nil {
		return nil, err
	}
	idempotencyKey, err := requiredUUIDParam(params, "idempotency_key")
	if err != nil {
		return nil, err
	}
	planID, err := requiredUUIDParam(params, "plan_id")
	if err != nil {
		return nil, err
	}
	expectedRevision, err := positiveRevisionParam(params, "expected_revision")
	if err != nil {
		return nil, err
	}
	signerKeyID, err := approvalDeviceParam(params, "signer_key_id")
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	plan, err := runner.getCloudPlanProto(callContext, planID)
	if err != nil {
		return nil, err
	}
	if plan.GetRevision() != expectedRevision || plan.GetStatus() != agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_READY_FOR_CONFIRMATION {
		return nil, errors.New("agent cloud plan revision or state changed")
	}
	response, err := runner.cloud.CreateApprovalChallenge(callContext, &agentv1.CreateApprovalChallengeRequest{
		IdempotencyKey: idempotencyKey, PlanId: planID, ExpectedRevision: expectedRevision,
		SignerKeyId: signerKeyID, OwnerId: runner.ownerID,
	})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	challenge := response.GetChallenge()
	if response == nil || challenge == nil || challenge.GetPlanId() != planID ||
		challenge.GetPlanRevision() != expectedRevision || challenge.GetOwnerId() != runner.ownerID ||
		challenge.GetSignerKeyId() != signerKeyID || challenge.GetRevision() < 1 ||
		!canonicalUUID(challenge.GetApprovalId()) || !challengeIDPattern.MatchString(challenge.GetChallengeId()) ||
		len(challenge.GetSigningPayloadCbor()) == 0 || len(challenge.GetSigningPayloadCbor()) > 64*1024 {
		return nil, errors.New("agent service returned an invalid approval challenge")
	}
	expiresAt, err := requiredTimestamp(challenge.GetExpiresAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid approval challenge")
	}
	return map[string]any{
		"approval_id": challenge.GetApprovalId(), "challenge_id": challenge.GetChallengeId(),
		"signer_key_id": challenge.GetSignerKeyId(), "plan_id": challenge.GetPlanId(),
		"plan_revision": challenge.GetPlanRevision(), "expires_at": expiresAt,
		"signing_payload_base64url": base64.RawURLEncoding.EncodeToString(challenge.GetSigningPayloadCbor()),
		"revision":                  challenge.GetRevision(),
	}, nil
}

func (runner *Runner) approveCloudPlan(ctx context.Context, params map[string]any) (map[string]any, error) {
	if err := allowActionParams(params, "idempotency_key", "plan_id", "expected_revision", "approval"); err != nil {
		return nil, err
	}
	idempotencyKey, err := requiredUUIDParam(params, "idempotency_key")
	if err != nil {
		return nil, err
	}
	planID, err := requiredUUIDParam(params, "plan_id")
	if err != nil {
		return nil, err
	}
	expectedRevision, err := positiveRevisionParam(params, "expected_revision")
	if err != nil {
		return nil, err
	}
	approval, err := approvalFromParams(params["approval"])
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	plan, err := runner.getCloudPlanProto(callContext, planID)
	if err != nil {
		return nil, err
	}
	if plan.GetRevision() != expectedRevision || plan.GetStatus() != agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_READY_FOR_CONFIRMATION {
		return nil, errors.New("agent cloud plan revision or state changed")
	}
	response, err := runner.cloud.ApproveCloudPlan(callContext, &agentv1.ApproveCloudPlanRequest{
		IdempotencyKey: idempotencyKey, PlanId: planID, ExpectedRevision: expectedRevision,
		OwnerId: runner.ownerID, Approval: approval,
	})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil || response.GetPlan() == nil || response.GetPlan().GetPlanId() != planID ||
		response.GetPlan().GetOwnerId() != runner.ownerID ||
		response.GetPlan().GetStatus() != agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_APPROVED {
		return nil, errors.New("agent service returned an invalid plan approval response")
	}
	return runner.mapCloudPlan(response.GetPlan())
}

func (runner *Runner) listCloudDeployments(ctx context.Context, params map[string]any) (map[string]any, error) {
	if runner.cloud == nil {
		return nil, errors.New("agent cloud service is unavailable")
	}
	if err := allowActionParams(params, "page_size", "page_token"); err != nil {
		return nil, err
	}
	pageSize, pageToken, err := cloudPage(params)
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.cloud.ListCloudDeployments(callContext, &agentv1.ListCloudDeploymentsRequest{
		OwnerId: runner.ownerID, PageSize: pageSize, PageToken: pageToken,
	})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	items := make([]map[string]any, 0, len(response.GetDeployments()))
	for _, remote := range response.GetDeployments() {
		item, mapErr := runner.mapCloudDeployment(remote)
		if mapErr != nil {
			return nil, mapErr
		}
		items = append(items, item)
	}
	next, err := cloudPageToken(response.GetNextPageToken())
	if err != nil {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	return map[string]any{"deployments": items, "next_page_token": next}, nil
}

func (runner *Runner) getCloudDeployment(ctx context.Context, params map[string]any) (map[string]any, error) {
	if runner.cloud == nil {
		return nil, errors.New("agent cloud service is unavailable")
	}
	if err := allowActionParams(params, "deployment_id"); err != nil {
		return nil, err
	}
	deploymentID, err := requiredUUIDParam(params, "deployment_id")
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.cloud.GetCloudDeployment(callContext, &agentv1.GetCloudDeploymentRequest{
		OwnerId: runner.ownerID, DeploymentId: deploymentID,
	})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil || response.GetDeployment() == nil || response.GetDeployment().GetDeploymentId() != deploymentID {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	return runner.mapCloudDeployment(response.GetDeployment())
}

func (runner *Runner) listCloudWorkers(ctx context.Context, params map[string]any) (map[string]any, error) {
	if runner.cloud == nil {
		return nil, errors.New("agent cloud service is unavailable")
	}
	if err := allowActionParams(params, "page_size", "page_token"); err != nil {
		return nil, err
	}
	pageSize, pageToken, err := cloudPage(params)
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.cloud.ListCloudWorkers(callContext, &agentv1.ListCloudWorkersRequest{
		OwnerId: runner.ownerID, PageSize: pageSize, PageToken: pageToken,
	})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil {
		return nil, errors.New("agent service returned an invalid worker response")
	}
	items := make([]map[string]any, 0, len(response.GetWorkers()))
	for _, remote := range response.GetWorkers() {
		item, mapErr := runner.mapCloudWorker(remote)
		if mapErr != nil {
			return nil, mapErr
		}
		items = append(items, item)
	}
	next, err := cloudPageToken(response.GetNextPageToken())
	if err != nil {
		return nil, errors.New("agent service returned an invalid worker response")
	}
	return map[string]any{"workers": items, "next_page_token": next}, nil
}

func (runner *Runner) getCloudWorker(ctx context.Context, params map[string]any) (map[string]any, error) {
	if runner.cloud == nil {
		return nil, errors.New("agent cloud service is unavailable")
	}
	if err := allowActionParams(params, "deployment_id"); err != nil {
		return nil, err
	}
	deploymentID, err := requiredUUIDParam(params, "deployment_id")
	if err != nil {
		return nil, err
	}
	callContext, cancel := context.WithTimeout(ctx, runner.chainTimeout)
	defer cancel()
	response, err := runner.cloud.GetCloudWorker(callContext, &agentv1.GetCloudWorkerRequest{
		OwnerId: runner.ownerID, DeploymentId: deploymentID,
	})
	if err != nil {
		return nil, sanitizeRPCError(callContext, err)
	}
	if response == nil || response.GetWorker() == nil || response.GetWorker().GetDeploymentId() != deploymentID {
		return nil, errors.New("agent service returned an invalid worker response")
	}
	return runner.mapCloudWorker(response.GetWorker())
}

func (runner *Runner) getCloudPlanProto(ctx context.Context, planID string) (*agentv1.CloudPlan, error) {
	if runner.cloud == nil {
		return nil, errors.New("agent cloud service is unavailable")
	}
	response, err := runner.cloud.GetCloudPlan(ctx, &agentv1.GetCloudPlanRequest{
		PlanId: planID, OwnerId: runner.ownerID,
	})
	if err != nil {
		return nil, sanitizeRPCError(ctx, err)
	}
	if response == nil || response.GetPlan() == nil || response.GetPlan().GetPlanId() != planID ||
		response.GetPlan().GetOwnerId() != runner.ownerID {
		return nil, errors.New("agent service returned an invalid plan response")
	}
	return response.GetPlan(), nil
}

func (runner *Runner) mapTask(remote *agentv1.Task) (map[string]any, error) {
	if remote == nil || remote.GetOwnerId() != runner.ownerID || !canonicalUUID(remote.GetTaskId()) ||
		remote.GetRevision() < 1 || !safePublicText(remote.GetGoal(), 16*1024, false) {
		return nil, errors.New("agent service returned an invalid task response")
	}
	execution, ok := executionStatus(remote.GetExecutionStatus())
	if !ok {
		return nil, errors.New("agent service returned an invalid task response")
	}
	outcome, ok := outcomeStatus(remote.GetOutcomeStatus())
	if !ok {
		return nil, errors.New("agent service returned an invalid task response")
	}
	retention, ok := retentionPolicy(remote.GetRetentionPolicy())
	if !ok {
		return nil, errors.New("agent service returned an invalid task response")
	}
	createdAt, err := requiredTimestamp(remote.GetCreatedAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid task response")
	}
	updatedAt, err := requiredTimestamp(remote.GetUpdatedAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid task response")
	}
	if !optionalCanonicalUUID(remote.GetCurrentStepId()) || !optionalCanonicalUUID(remote.GetApprovedPlanId()) {
		return nil, errors.New("agent service returned an invalid task response")
	}
	return map[string]any{
		"task_id": remote.GetTaskId(), "goal": remote.GetGoal(), "execution_status": execution,
		"outcome_status": outcome, "retention_policy": retention, "current_step_id": remote.GetCurrentStepId(),
		"approved_plan_id": remote.GetApprovedPlanId(), "revision": remote.GetRevision(),
		"created_at": createdAt, "updated_at": updatedAt,
	}, nil
}

func mapTaskStep(remote *agentv1.Step, taskID string) (map[string]any, error) {
	if remote == nil || remote.GetTaskId() != taskID || !canonicalUUID(remote.GetStepId()) ||
		!safePublicText(remote.GetName(), 512, false) || remote.GetAttempt() < 0 ||
		remote.GetLeaseEpoch() < 0 || remote.GetRevision() < 1 {
		return nil, errors.New("agent service returned an invalid task response")
	}
	dependencies := append([]string(nil), remote.GetDependsOnStepIds()...)
	for _, dependency := range dependencies {
		if !canonicalUUID(dependency) {
			return nil, errors.New("agent service returned an invalid task response")
		}
	}
	executor, ok := executorKind(remote.GetExecutorKind())
	if !ok {
		return nil, errors.New("agent service returned an invalid task response")
	}
	execution, ok := executionStatus(remote.GetExecutionStatus())
	if !ok {
		return nil, errors.New("agent service returned an invalid task response")
	}
	outcome, ok := outcomeStatus(remote.GetOutcomeStatus())
	if !ok {
		return nil, errors.New("agent service returned an invalid task response")
	}
	createdAt, err := requiredTimestamp(remote.GetCreatedAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid task response")
	}
	updatedAt, err := requiredTimestamp(remote.GetUpdatedAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid task response")
	}
	return map[string]any{
		"step_id": remote.GetStepId(), "name": remote.GetName(), "depends_on_step_ids": dependencies,
		"executor_kind": executor, "execution_status": execution, "outcome_status": outcome,
		"attempt": remote.GetAttempt(), "lease_epoch": remote.GetLeaseEpoch(),
		"checkpoint_available": strings.TrimSpace(remote.GetCheckpointRef()) != "",
		"result_available":     strings.TrimSpace(remote.GetResultRef()) != "",
		"revision":             remote.GetRevision(), "created_at": createdAt, "updated_at": updatedAt,
	}, nil
}

func (runner *Runner) mapCloudPlan(remote *agentv1.CloudPlan) (map[string]any, error) {
	if remote == nil || remote.GetOwnerId() != runner.ownerID || !canonicalUUID(remote.GetPlanId()) ||
		!canonicalUUID(remote.GetConnectionId()) || !canonicalUUID(remote.GetQuoteId()) ||
		remote.GetRevision() < 1 || remote.GetRecipe() == nil || remote.GetResource() == nil ||
		remote.GetNetwork() == nil || remote.GetRetention() == nil {
		return nil, errors.New("agent service returned an invalid plan response")
	}
	statusValue, ok := cloudPlanStatus(remote.GetStatus())
	if !ok {
		return nil, errors.New("agent service returned an invalid plan response")
	}
	candidate, ok := cloudCandidateProfile(remote.GetCandidateProfile())
	if !ok {
		return nil, errors.New("agent service returned an invalid plan response")
	}
	resourceCandidate, ok := cloudCandidateProfile(remote.GetResource().GetCandidateProfile())
	if !ok || resourceCandidate != candidate {
		return nil, errors.New("agent service returned an invalid plan response")
	}
	purchase, ok := cloudPurchaseOption(remote.GetResource().GetPurchaseOption())
	if !ok {
		return nil, errors.New("agent service returned an invalid plan response")
	}
	retention, ok := cloudRetentionClass(remote.GetRetention().GetRetentionClass())
	if !ok {
		return nil, errors.New("agent service returned an invalid plan response")
	}
	quoteValidUntil, err := requiredTimestamp(remote.GetQuoteValidUntil())
	if err != nil || !safePublicText(remote.GetRecipe().GetRecipeId(), 160, false) ||
		!safePublicText(remote.GetRecipe().GetMaturity(), 32, false) ||
		!safePublicText(remote.GetResource().GetRegion(), 64, false) ||
		!safePublicText(remote.GetResource().GetInstanceType(), 64, false) ||
		!safePublicText(remote.GetResource().GetArchitecture(), 32, false) ||
		!safePublicText(remote.GetResource().GetGpuType(), 64, true) ||
		remote.GetResource().GetInstanceCount() == 0 {
		return nil, errors.New("agent service returned an invalid plan response")
	}
	return map[string]any{
		"plan_id": remote.GetPlanId(), "connection_id": remote.GetConnectionId(),
		"recipe": map[string]any{
			"recipe_id": remote.GetRecipe().GetRecipeId(), "maturity": remote.GetRecipe().GetMaturity(),
		},
		"quote_id": remote.GetQuoteId(), "candidate_profile": candidate,
		"quote_valid_until": quoteValidUntil, "status": statusValue, "revision": remote.GetRevision(),
		"resource": map[string]any{
			"region": remote.GetResource().GetRegion(), "instance_type": remote.GetResource().GetInstanceType(),
			"instance_count": remote.GetResource().GetInstanceCount(), "architecture": remote.GetResource().GetArchitecture(),
			"vcpu": remote.GetResource().GetVcpu(), "memory_mib": remote.GetResource().GetMemoryMib(),
			"gpu_type": remote.GetResource().GetGpuType(), "gpu_count": remote.GetResource().GetGpuCount(),
			"gpu_memory_mib": remote.GetResource().GetGpuMemoryMib(), "disk_gib": remote.GetResource().GetDiskGib(),
			"purchase_option": purchase,
		},
		"network": map[string]any{
			"public_exposure": remote.GetNetwork().GetPublicExposure(), "public_ipv4": remote.GetNetwork().GetPublicIpv4(),
			"tls_required": remote.GetNetwork().GetTlsRequired(), "authentication_required": remote.GetNetwork().GetAuthenticationRequired(),
		},
		"retention": map[string]any{
			"class": retention, "auto_destroy": remote.GetRetention().GetAutoDestroy(),
			"grace_period_seconds": remote.GetRetention().GetGracePeriodSeconds(),
			"max_lifetime_seconds": remote.GetRetention().GetMaxLifetimeSeconds(),
		},
		"approval_required": remote.GetStatus() == agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_READY_FOR_CONFIRMATION,
	}, nil
}

func mapPlanQuote(remote *agentv1.CloudQuote, plan *agentv1.CloudPlan) (map[string]any, error) {
	if remote == nil || plan == nil || remote.GetQuoteId() != plan.GetQuoteId() ||
		remote.GetDigest() != plan.GetQuoteDigest() || !safePublicText(remote.GetCurrency(), 8, false) {
		return nil, errors.New("agent service returned an invalid quote response")
	}
	quotedAt, err := requiredTimestamp(remote.GetQuotedAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid quote response")
	}
	validUntil, err := requiredTimestamp(remote.GetValidUntil())
	if err != nil {
		return nil, errors.New("agent service returned an invalid quote response")
	}
	var matched *agentv1.CloudQuoteCandidate
	for _, candidate := range remote.GetCandidates() {
		if candidate != nil && candidate.GetCandidateProfile() == plan.GetCandidateProfile() &&
			candidate.GetScopeDigest() == plan.GetQuoteScopeDigest() {
			if matched != nil {
				return nil, errors.New("agent service returned an invalid quote response")
			}
			matched = candidate
		}
	}
	if matched == nil {
		return nil, errors.New("agent service returned an invalid quote response")
	}
	assumptions, err := publicTextList(remote.GetAssumptions(), 32, 512)
	if err != nil {
		return nil, errors.New("agent service returned an invalid quote response")
	}
	exclusions, err := publicTextList(remote.GetExclusions(), 32, 512)
	if err != nil {
		return nil, errors.New("agent service returned an invalid quote response")
	}
	return map[string]any{
		"currency": remote.GetCurrency(), "quoted_at": quotedAt, "valid_until": validUntil,
		"hourly_estimate_micros":       matched.GetHourlyEstimateMicros(),
		"monthly_estimate_micros":      matched.GetMonthlyEstimateMicros(),
		"maximum_launch_amount_micros": matched.GetMaximumLaunchAmountMicros(),
		"assumptions":                  assumptions, "exclusions": exclusions,
	}, nil
}

func (runner *Runner) mapCloudDeployment(remote *agentv1.CloudDeployment) (map[string]any, error) {
	if remote == nil || remote.GetOwnerId() != runner.ownerID || !canonicalUUID(remote.GetDeploymentId()) ||
		!optionalCanonicalUUID(remote.GetTaskId()) || !optionalCanonicalUUID(remote.GetStepId()) ||
		!optionalCanonicalUUID(remote.GetWorkerId()) || !optionalCanonicalUUID(remote.GetPlanId()) ||
		!optionalCanonicalUUID(remote.GetConnectionId()) || remote.GetRevision() < 1 {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	execution, ok := executionStatus(remote.GetExecutionStatus())
	if !ok {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	outcome, ok := outcomeStatus(remote.GetOutcomeStatus())
	if !ok {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	createdAt, err := requiredTimestamp(remote.GetCreatedAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	updatedAt, err := requiredTimestamp(remote.GetUpdatedAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	resources, err := mapCloudResourceSummary(remote.GetResources())
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"deployment_id": remote.GetDeploymentId(), "task_id": remote.GetTaskId(), "step_id": remote.GetStepId(),
		"worker_id": remote.GetWorkerId(), "plan_id": remote.GetPlanId(), "connection_id": remote.GetConnectionId(),
		"execution_status": execution, "outcome_status": outcome, "resources": resources,
		"revision": remote.GetRevision(), "created_at": createdAt, "updated_at": updatedAt,
	}, nil
}

func mapCloudResourceSummary(remote *agentv1.CloudResourceSummary) (map[string]any, error) {
	if remote == nil || remote.GetRevision() < 0 || remote.GetReadBack() == nil {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	statusValue, ok := cloudResourceStatus(remote.GetStatus())
	if !ok {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	readBack := remote.GetReadBack()
	if readBack.GetObservedResources()+readBack.GetUnobservedResources() != readBack.GetTotalResources() ||
		readBack.GetExistingResources()+readBack.GetMissingResources() > readBack.GetObservedResources() {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	lastObservedAt, err := optionalTimestamp(readBack.GetLastObservedAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid deployment response")
	}
	return map[string]any{
		"status": statusValue, "revision": remote.GetRevision(),
		"total": readBack.GetTotalResources(), "observed": readBack.GetObservedResources(),
		"existing": readBack.GetExistingResources(), "missing": readBack.GetMissingResources(),
		"unobserved": readBack.GetUnobservedResources(), "last_observed_at": lastObservedAt,
	}, nil
}

func (runner *Runner) mapCloudWorker(remote *agentv1.CloudWorker) (map[string]any, error) {
	if remote == nil || remote.GetOwnerId() != runner.ownerID || !canonicalUUID(remote.GetDeploymentId()) ||
		!canonicalUUID(remote.GetWorkerId()) || remote.GetAttempt() < 0 || remote.GetLeaseEpoch() < 0 ||
		remote.GetRevision() < 1 {
		return nil, errors.New("agent service returned an invalid worker response")
	}
	statusValue, ok := cloudWorkerStatus(remote.GetStatus())
	if !ok {
		return nil, errors.New("agent service returned an invalid worker response")
	}
	createdAt, err := requiredTimestamp(remote.GetCreatedAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid worker response")
	}
	updatedAt, err := requiredTimestamp(remote.GetUpdatedAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid worker response")
	}
	leaseExpiresAt, err := optionalTimestamp(remote.GetLeaseExpiresAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid worker response")
	}
	lastHeartbeatAt, err := optionalTimestamp(remote.GetLastHeartbeatAt())
	if err != nil {
		return nil, errors.New("agent service returned an invalid worker response")
	}
	return map[string]any{
		"deployment_id": remote.GetDeploymentId(), "worker_id": remote.GetWorkerId(), "status": statusValue,
		"attempt": remote.GetAttempt(), "lease_epoch": remote.GetLeaseEpoch(), "lease_expires_at": leaseExpiresAt,
		"last_heartbeat_at": lastHeartbeatAt, "cancellation_requested": remote.GetCancellationRequested(),
		"checkpoint_available": remote.GetCheckpointAvailable(), "result_available": remote.GetResultAvailable(),
		"evidence_count": remote.GetEvidenceCount(), "revision": remote.GetRevision(),
		"created_at": createdAt, "updated_at": updatedAt,
	}, nil
}

func approvalFromParams(value any) (*agentv1.DeviceApprovalSignature, error) {
	raw, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("invalid agent cloud parameters: approval is required")
	}
	if err := allowActionParams(raw, "approval_id", "challenge_id", "signer_key_id", "expires_at", "signature_base64url"); err != nil {
		return nil, err
	}
	approvalID, err := requiredUUIDParam(raw, "approval_id")
	if err != nil {
		return nil, err
	}
	challengeID, err := boundedTextParam(raw, "challenge_id", 128, true)
	if err != nil || !challengeIDPattern.MatchString(challengeID) {
		return nil, errors.New("invalid agent cloud parameters: challenge_id is invalid")
	}
	signerKeyID, err := approvalDeviceParam(raw, "signer_key_id")
	if err != nil {
		return nil, err
	}
	expiresText, err := boundedTextParam(raw, "expires_at", 64, true)
	if err != nil {
		return nil, err
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, expiresText)
	if err != nil {
		return nil, errors.New("invalid agent cloud parameters: expires_at is invalid")
	}
	signatureText, err := boundedTextParam(raw, "signature_base64url", 128, true)
	if err != nil {
		return nil, err
	}
	signature, err := base64.RawURLEncoding.DecodeString(signatureText)
	if err != nil || len(signature) != 64 {
		clear(signature)
		return nil, errors.New("invalid agent cloud parameters: signature is invalid")
	}
	return &agentv1.DeviceApprovalSignature{
		ApprovalId: approvalID, ChallengeId: challengeID, SignerKeyId: signerKeyID,
		ExpiresAt: timestamppb.New(expiresAt.UTC()), Signature: signature,
	}, nil
}

func cloudPage(params map[string]any) (int32, string, error) {
	size := int64(50)
	if params != nil && params["page_size"] != nil {
		parsed, err := nonnegativeInt64(params["page_size"])
		if err != nil || parsed < 1 || parsed > maxCloudActionPageSize {
			return 0, "", errors.New("invalid agent cloud parameters: page_size must be between 1 and 100")
		}
		size = parsed
	}
	token := stringParam(params, "page_token")
	if params != nil && params["page_token"] != nil {
		if _, ok := params["page_token"].(string); !ok {
			return 0, "", errors.New("invalid agent cloud parameters: page_token is invalid")
		}
	}
	token, err := cloudPageToken(token)
	if err != nil {
		return 0, "", errors.New("invalid agent cloud parameters: page_token is invalid")
	}
	return int32(size), token, nil
}

func cloudPageToken(value string) (string, error) {
	value = strings.TrimSpace(value)
	if len(value) > maxCloudActionTokenBytes || strings.ContainsAny(value, "\x00\r\n") || !utf8.ValidString(value) {
		return "", errors.New("invalid page token")
	}
	return value, nil
}

func allowActionParams(params map[string]any, allowed ...string) error {
	if params == nil {
		return nil
	}
	set := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		set[key] = struct{}{}
	}
	for key := range params {
		if _, ok := set[key]; !ok {
			return errors.New("invalid agent cloud parameters: unsupported field")
		}
	}
	return nil
}

func requiredUUIDParam(params map[string]any, key string) (string, error) {
	value, ok := params[key].(string)
	value = strings.TrimSpace(value)
	if !ok || !canonicalUUID(value) {
		return "", errors.New("invalid agent cloud parameters: " + key + " must be a canonical UUID")
	}
	return value, nil
}

func positiveRevisionParam(params map[string]any, key string) (int64, error) {
	value, err := nonnegativeInt64(params[key])
	if err != nil || value < 1 {
		return 0, errors.New("invalid agent cloud parameters: " + key + " must be positive")
	}
	return value, nil
}

func approvalDeviceParam(params map[string]any, key string) (string, error) {
	value, ok := params[key].(string)
	value = strings.TrimSpace(value)
	if !ok || !approvalDeviceKeyPattern.MatchString(value) {
		return "", errors.New("invalid agent cloud parameters: " + key + " is invalid")
	}
	return value, nil
}

func boundedTextParam(params map[string]any, key string, maxRunes int, required bool) (string, error) {
	value, ok := params[key].(string)
	value = strings.TrimSpace(value)
	if (!ok && params[key] != nil) || !safePublicText(value, maxRunes, !required) || (required && value == "") {
		return "", errors.New("invalid agent cloud parameters: " + key + " is invalid")
	}
	return value, nil
}

func safePublicText(value string, maxRunes int, allowEmpty bool) bool {
	if !utf8.ValidString(value) || (!allowEmpty && strings.TrimSpace(value) == "") ||
		utf8.RuneCountInString(value) > maxRunes || strings.ContainsRune(value, '\x00') {
		return false
	}
	for _, r := range value {
		if r == '\n' || r == '\t' {
			continue
		}
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func publicTextList(values []string, maxItems, maxRunes int) ([]string, error) {
	if len(values) > maxItems {
		return nil, errors.New("too many values")
	}
	result := append([]string(nil), values...)
	for _, value := range result {
		if !safePublicText(value, maxRunes, false) {
			return nil, errors.New("invalid value")
		}
	}
	return result, nil
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func optionalCanonicalUUID(value string) bool {
	return strings.TrimSpace(value) == "" || canonicalUUID(value)
}

func requiredTimestamp(value *timestamppb.Timestamp) (string, error) {
	if value == nil || value.CheckValid() != nil {
		return "", errors.New("invalid timestamp")
	}
	return value.AsTime().UTC().Format(time.RFC3339Nano), nil
}

func optionalTimestamp(value *timestamppb.Timestamp) (any, error) {
	if value == nil {
		return nil, nil
	}
	timestamp, err := requiredTimestamp(value)
	if err != nil {
		return nil, err
	}
	return timestamp, nil
}

func executionStatus(value agentv1.ExecutionStatus) (string, bool) {
	switch value {
	case agentv1.ExecutionStatus_EXECUTION_STATUS_DRAFT:
		return "draft", true
	case agentv1.ExecutionStatus_EXECUTION_STATUS_PLANNING:
		return "planning", true
	case agentv1.ExecutionStatus_EXECUTION_STATUS_AWAITING_APPROVAL:
		return "awaiting_approval", true
	case agentv1.ExecutionStatus_EXECUTION_STATUS_QUEUED:
		return "queued", true
	case agentv1.ExecutionStatus_EXECUTION_STATUS_RUNNING:
		return "running", true
	case agentv1.ExecutionStatus_EXECUTION_STATUS_WAITING_USER:
		return "waiting_user", true
	case agentv1.ExecutionStatus_EXECUTION_STATUS_VERIFYING:
		return "verifying", true
	case agentv1.ExecutionStatus_EXECUTION_STATUS_FINISHED:
		return "finished", true
	default:
		return "", false
	}
}

func outcomeStatus(value agentv1.OutcomeStatus) (string, bool) {
	switch value {
	case agentv1.OutcomeStatus_OUTCOME_STATUS_PENDING:
		return "pending", true
	case agentv1.OutcomeStatus_OUTCOME_STATUS_SUCCEEDED:
		return "succeeded", true
	case agentv1.OutcomeStatus_OUTCOME_STATUS_FAILED:
		return "failed", true
	case agentv1.OutcomeStatus_OUTCOME_STATUS_CANCELED:
		return "canceled", true
	case agentv1.OutcomeStatus_OUTCOME_STATUS_TIMED_OUT:
		return "timed_out", true
	case agentv1.OutcomeStatus_OUTCOME_STATUS_INTERRUPTED:
		return "interrupted", true
	default:
		return "", false
	}
}

func retentionPolicy(value agentv1.RetentionPolicy) (string, bool) {
	switch value {
	case agentv1.RetentionPolicy_RETENTION_POLICY_EPHEMERAL_AUTO_DESTROY:
		return "ephemeral_auto_destroy", true
	case agentv1.RetentionPolicy_RETENTION_POLICY_MANAGED_RETAINED:
		return "managed_retained", true
	default:
		return "", false
	}
}

func executorKind(value agentv1.ExecutorKind) (string, bool) {
	switch value {
	case agentv1.ExecutorKind_EXECUTOR_KIND_CONTROL_PLANE:
		return "control_plane", true
	case agentv1.ExecutorKind_EXECUTOR_KIND_CLOUD_WORKER:
		return "cloud_worker", true
	default:
		return "", false
	}
}

func cloudPlanStatus(value agentv1.CloudPlanStatus) (string, bool) {
	switch value {
	case agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_RESEARCHING:
		return "researching", true
	case agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_QUOTING:
		return "quoting", true
	case agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_READY_FOR_CONFIRMATION:
		return "ready_for_confirmation", true
	case agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_APPROVED:
		return "approved", true
	case agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_EXPIRED:
		return "expired", true
	case agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_SUPERSEDED:
		return "superseded", true
	default:
		return "", false
	}
}

func cloudCandidateProfile(value agentv1.CloudCandidateProfile) (string, bool) {
	switch value {
	case agentv1.CloudCandidateProfile_CLOUD_CANDIDATE_PROFILE_ECONOMY:
		return "economy", true
	case agentv1.CloudCandidateProfile_CLOUD_CANDIDATE_PROFILE_RECOMMENDED:
		return "recommended", true
	case agentv1.CloudCandidateProfile_CLOUD_CANDIDATE_PROFILE_PERFORMANCE:
		return "performance", true
	default:
		return "", false
	}
}

func cloudPurchaseOption(value agentv1.CloudPurchaseOption) (string, bool) {
	switch value {
	case agentv1.CloudPurchaseOption_CLOUD_PURCHASE_OPTION_ON_DEMAND:
		return "on_demand", true
	case agentv1.CloudPurchaseOption_CLOUD_PURCHASE_OPTION_SPOT:
		return "spot", true
	default:
		return "", false
	}
}

func cloudRetentionClass(value agentv1.CloudRetentionClass) (string, bool) {
	switch value {
	case agentv1.CloudRetentionClass_CLOUD_RETENTION_CLASS_EPHEMERAL:
		return "ephemeral", true
	case agentv1.CloudRetentionClass_CLOUD_RETENTION_CLASS_MANAGED:
		return "managed", true
	default:
		return "", false
	}
}

func cloudResourceStatus(value agentv1.CloudResourceStatus) (string, bool) {
	switch value {
	case agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_NONE:
		return "none", true
	case agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_PROVISIONING:
		return "provisioning", true
	case agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_ACTIVE:
		return "active", true
	case agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_DESTROY_SCHEDULED:
		return "destroy_scheduled", true
	case agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_RETAINED_MANAGED:
		return "retained_managed", true
	case agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_DESTROYING:
		return "destroying", true
	case agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_VERIFIED_DESTROYED:
		return "verified_destroyed", true
	case agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_DESTROY_BLOCKED:
		return "destroy_blocked", true
	case agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_ORPHANED:
		return "orphaned", true
	case agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_MIXED:
		return "mixed", true
	default:
		return "", false
	}
}

func cloudWorkerStatus(value agentv1.CloudWorkerStatus) (string, bool) {
	switch value {
	case agentv1.CloudWorkerStatus_CLOUD_WORKER_STATUS_PENDING_ENROLLMENT:
		return "pending_enrollment", true
	case agentv1.CloudWorkerStatus_CLOUD_WORKER_STATUS_READY:
		return "ready", true
	case agentv1.CloudWorkerStatus_CLOUD_WORKER_STATUS_LEASED:
		return "leased", true
	case agentv1.CloudWorkerStatus_CLOUD_WORKER_STATUS_CANCEL_REQUESTED:
		return "cancel_requested", true
	case agentv1.CloudWorkerStatus_CLOUD_WORKER_STATUS_FINISHED:
		return "finished", true
	default:
		return "", false
	}
}
