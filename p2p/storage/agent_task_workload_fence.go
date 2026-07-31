package storage

import (
	"context"
	"database/sql"
	"errors"

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
	var authoritativePlanID, authoritativeDigest, authoritativeTargetKind string
	var authoritativeRevision int64
	err = tx.QueryRowContext(ctx, `SELECT plan_id::text,revision,digest,target_kind FROM core_workload_plans WHERE owner_id=$1 AND plan_id=$2`, current.OwnerID, planID).Scan(
		&authoritativePlanID, &authoritativeRevision, &authoritativeDigest, &authoritativeTargetKind,
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
	return nil
}
