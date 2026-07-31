package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

// validateWorkloadTaskFenceTx revalidates the immutable workload handoff before
// cancellation can terminalize the workload projection.  The task row is
// already locked by Cancel; this helper locks the operation in the same order
// used by terminalizeWorkloadOperationTx and then reads the immutable plan.
// Any identity, revision, digest, or target mismatch fails closed.
func validateWorkloadTaskFenceTx(ctx context.Context, tx *sql.Tx, current coretask.Task, stored coreconfirmation.Confirmation) error {
	payload := current.Spec.Payload.Workload
	if current.Spec.Kind != coretask.TaskKindWorkload || payload == nil {
		return coreconfirmation.ErrConflict
	}
	var operationID, workloadID, planID, operation, planDigest, targetKind, taskID, confirmationID, status, dispatchState string
	var expectedWorkloadRevision, planRevision, revision int64
	err := tx.QueryRowContext(ctx, `SELECT operation_id::text,workload_id::text,expected_workload_revision,plan_id::text,operation,plan_revision,plan_digest,target_kind,task_id::text,confirmation_id::text,status,dispatch_state,revision
		FROM core_workload_operations WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, current.OwnerID, current.ID).Scan(
		&operationID, &workloadID, &expectedWorkloadRevision, &planID, &operation, &planRevision, &planDigest, &targetKind, &taskID, &confirmationID, &status, &dispatchState, &revision,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return coreconfirmation.ErrConflict
	}
	if err != nil {
		return err
	}
	if revision < 1 || status != "waiting_user" || dispatchState != "prepared" ||
		operationID != payload.OperationID || confirmationID != payload.ConfirmationID ||
		taskID != current.ID || workloadID != payload.WorkloadID ||
		expectedWorkloadRevision < 1 || uint64(expectedWorkloadRevision) != payload.ExpectedWorkloadRevision ||
		planID != payload.PlanID || planRevision < 1 || uint64(planRevision) != payload.PlanRevision ||
		planDigest != payload.PlanDigest || targetKind != payload.TargetKind ||
		stored.ID != payload.ConfirmationID || stored.OwnerID != current.OwnerID || stored.TaskID != current.ID ||
		stored.Binding.TargetID != payload.WorkloadID || stored.Binding.TargetRevision != planRevision ||
		stored.Binding.OperationDomain != "workload:"+operation {
		return coreconfirmation.ErrConflict
	}
	var workloadRevision int64
	var workloadPlanID, workloadDigest, workloadTargetKind, workloadState string
	err = tx.QueryRowContext(ctx, `SELECT revision,plan_id::text,plan_digest,target_kind,state FROM core_workloads WHERE owner_id=$1 AND workload_id=$2 FOR UPDATE`, current.OwnerID, workloadID).Scan(
		&workloadRevision, &workloadPlanID, &workloadDigest, &workloadTargetKind, &workloadState,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return coreconfirmation.ErrConflict
	}
	if err != nil {
		return err
	}
	if workloadRevision < 1 || uint64(workloadRevision) != payload.ExpectedWorkloadRevision ||
		workloadPlanID != payload.PlanID || workloadDigest != payload.PlanDigest || workloadTargetKind != payload.TargetKind ||
		(operation == "destroy" && workloadState != "ready") {
		return coreconfirmation.ErrConflict
	}
	var bindingDomain, bindingTarget, bindingDigest string
	var bindingTargetRevision int64
	var bindingRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT operation_domain,target_id,target_revision,binding_digest,binding_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2 AND task_id=$3 FOR SHARE`, current.OwnerID, stored.ID, current.ID).Scan(&bindingDomain, &bindingTarget, &bindingTargetRevision, &bindingDigest, &bindingRaw)
	if err != nil {
		return err
	}
	var persistedBinding coreconfirmation.Binding
	if json.Unmarshal(bindingRaw, &persistedBinding) != nil || bindingDomain != stored.Binding.OperationDomain || bindingTarget != stored.Binding.TargetID || bindingTargetRevision != stored.Binding.TargetRevision {
		return coreconfirmation.ErrConflict
	}
	var authoritativePlanID, authoritativeDigest, authoritativeTargetKind string
	var authoritativeRevision int64
	var planRaw, targetIdentityRaw, resourceLimitsRaw, secretGrantRefsRaw []byte
	var authoritativeSummary string
	var authoritativeExpiresAt, authoritativeCreatedAt time.Time
	err = tx.QueryRowContext(ctx, `SELECT plan_id::text,revision,digest,summary,plan_json,target_kind,target_identity_json,resource_limits_json,secret_grant_refs_json,expires_at,created_at FROM core_workload_plans WHERE owner_id=$1 AND plan_id=$2 FOR SHARE`, current.OwnerID, planID).Scan(
		&authoritativePlanID, &authoritativeRevision, &authoritativeDigest, &authoritativeSummary, &planRaw, &authoritativeTargetKind, &targetIdentityRaw, &resourceLimitsRaw, &secretGrantRefsRaw, &authoritativeExpiresAt, &authoritativeCreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return coreconfirmation.ErrConflict
	}
	if err != nil {
		return err
	}
	if authoritativePlanID != payload.PlanID || authoritativeRevision < 1 || uint64(authoritativeRevision) != payload.PlanRevision ||
		authoritativeDigest != payload.PlanDigest || authoritativeTargetKind != payload.TargetKind {
		return coreconfirmation.ErrConflict
	}
	var plan workload.Plan
	if json.Unmarshal(planRaw, &plan) != nil {
		return coreconfirmation.ErrConflict
	}
	declaredDigest := plan.Digest
	plan.Digest = ""
	normalizedPlan, err := plan.Normalize()
	if err != nil || normalizedPlan.ID != authoritativePlanID || normalizedPlan.Revision != uint64(authoritativeRevision) ||
		normalizedPlan.Digest != authoritativeDigest || declaredDigest != authoritativeDigest || normalizedPlan.TargetKind != workload.TargetKind(authoritativeTargetKind) {
		return coreconfirmation.ErrConflict
	}
	canonicalPlan := plan
	canonicalPlan.Digest = normalizedPlan.Digest
	if !reflect.DeepEqual(normalizedPlan, canonicalPlan) {
		return coreconfirmation.ErrConflict
	}
	expectedTargetRaw, _ := json.Marshal(normalizedPlan.Target.Identity)
	expectedLimitsRaw, _ := json.Marshal(normalizedPlan.ResourceLimits)
	expectedRefsRaw, _ := json.Marshal(normalizedPlan.SecretGrantRefs)
	if authoritativeSummary != normalizedPlan.Summary || !authoritativeExpiresAt.UTC().Round(time.Microsecond).Equal(normalizedPlan.ExpiresAt.UTC().Round(time.Microsecond)) ||
		!authoritativeCreatedAt.UTC().Round(time.Microsecond).Equal(normalizedPlan.CreatedAt.UTC().Round(time.Microsecond)) ||
		!canonicalJSONEqual(targetIdentityRaw, expectedTargetRaw) || !canonicalJSONEqual(resourceLimitsRaw, expectedLimitsRaw) ||
		!canonicalJSONEqual(secretGrantRefsRaw, expectedRefsRaw) {
		return coreconfirmation.ErrConflict
	}
	expectedBinding := workload.BindingForOperation(normalizedPlan, payload.WorkloadID, workload.OperationKind(operation))
	expectedBinding.OwnerID = current.OwnerID
	expectedBinding, err = expectedBinding.Normalize()
	if err != nil || stored.Binding.Digest != expectedBinding.Digest || persistedBinding.Digest != expectedBinding.Digest || bindingDigest != string(expectedBinding.Digest) || !stored.Binding.Equal(expectedBinding) || !persistedBinding.Equal(expectedBinding) {
		return coreconfirmation.ErrConflict
	}
	return nil
}

func canonicalJSONEqual(a, b []byte) bool {
	decode := func(raw []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		err := decoder.Decode(&value)
		return value, err
	}
	left, leftErr := decode(a)
	right, rightErr := decode(b)
	return leftErr == nil && rightErr == nil && reflect.DeepEqual(left, right)
}
