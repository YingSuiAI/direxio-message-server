package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/google/uuid"
)

// ExecutionDispatchIntent is the durable fence written before a provider
// call. Once persisted, a lost response is uncertain and must be reconciled;
// the store never silently re-issues the same mutation.
type ExecutionDispatchIntent struct {
	Attempt         coreexecution.StepAttempt
	Receipt         coreexecution.Receipt
	TaskID          string
	TaskHolder      string
	TaskAttempt     uint32
	TaskRevision    uint64
	TaskLeaseEpoch  uint64
	TargetID        string
	TargetRevision  uint64
	TargetDigest    coreexecution.Digest
	LeaseID         string
	LeaseToken      string
	LeaseEpoch      uint64
	LeaseExpiresAt  time.Time
	StepSet         coreexecution.StepSet
	RequestDigest   coreexecution.Digest
	FenceDigest     coreexecution.Digest
	Snapshot        coreaws.FrozenRequestSnapshot
	EC2Provision    *coreaws.EC2ProvisionRequest
	SecretProvision *coreaws.SecretParameterProvisionRequest
}

// ExecutionStageLeaseClaim is the authoritative agent_tasks lease fence used
// to claim a queued execution stage. The expected task revision is checked
// under the same row lock as the stage and target lease.
type ExecutionStageLeaseClaim struct {
	OwnerID              string
	RunID                string
	StageID              string
	TaskID               string
	Holder               string
	Attempt              uint32
	LeaseEpoch           uint64
	TaskLeaseEpoch       uint64
	ExpectedTaskRevision uint64
	LeaseID              string
	LeaseToken           string
	ExpiresAt            time.Time
}

type ExecutionNextStep struct {
	OwnerID      string
	RunID        string
	StageID      string
	StepKey      string
	StepSet      coreexecution.StepSet
	StepRevision uint64
	StepDigest   coreexecution.Digest
}

// NextExecutableStep resumes a running stage after restart. It only returns a
// step with no succeeded attempt, so a receipt already fenced as succeeded is
// never re-issued.
func (s *DatabaseExecutionStore) NextExecutableStep(ctx context.Context, owner, runID, stageID string) (ExecutionNextStep, error) {
	var out ExecutionNextStep
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(runID) || !coreexecution.ValidateUUID(stageID) {
		return out, ErrExecutionStoreInvalid
	}
	err := s.db.QueryRowContext(ctx, `SELECT s.owner_id,s.run_id::text,s.stage_id::text,ps.step_key,ps.step_set,ps.step_revision,ps.step_digest FROM core_execution_run_stages s JOIN core_execution_runs r ON r.owner_id=s.owner_id AND r.run_id=s.run_id JOIN core_execution_plan_steps ps ON ps.owner_id=s.owner_id AND ps.plan_id=s.plan_id AND ps.plan_revision=s.plan_revision AND ps.stage_key=s.plan_stage_key WHERE s.owner_id=$1 AND s.run_id=$2 AND s.stage_id=$3 AND s.status='running' AND ((r.operation='rollback' AND ps.step_set='rollback') OR (r.operation<>'rollback' AND ps.step_set='forward')) AND NOT EXISTS (SELECT 1 FROM core_execution_step_attempts a WHERE a.owner_id=s.owner_id AND a.run_id=s.run_id AND a.stage_id=s.stage_id AND a.step_set=ps.step_set AND a.step_key=ps.step_key AND a.status='succeeded') ORDER BY ps.ordinal LIMIT 1`, owner, runID, stageID).Scan(&out.OwnerID, &out.RunID, &out.StageID, &out.StepKey, &out.StepSet, &out.StepRevision, &out.StepDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return out, coreexecution.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	return out, nil
}

func (s *DatabaseExecutionStore) RenewExecutionStageLease(ctx context.Context, c ExecutionStageLeaseClaim, ttl time.Duration) error {
	if s == nil || s.db == nil || ttl <= 0 || strings.TrimSpace(c.OwnerID) == "" || !coreexecution.ValidateUUID(c.TaskID) || !coreexecution.ValidateUUID(c.LeaseID) || !coreexecution.ValidateUUID(c.LeaseToken) {
		return ErrExecutionStoreInvalid
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	expiry := now.Add(ttl)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET lease_expires_at=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND task_id=$4 AND status='running' AND lease_holder=$5 AND attempt=$6 AND lease_epoch=$7 AND revision=$8 AND lease_expires_at>$2`, expiry, now, c.OwnerID, c.TaskID, c.Holder, c.Attempt, c.TaskLeaseEpoch, c.ExpectedTaskRevision)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET expires_at=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND lease_id=$4 AND token=$5 AND epoch=$6 AND status='active' AND expires_at>$2`, expiry, now, c.OwnerID, c.LeaseID, c.LeaseToken, c.LeaseEpoch)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	return tx.Commit()
}

func successorStageSnapshot(raw []byte, status coreexecution.StageStatus, at time.Time) ([]byte, error) {
	var stage coreexecution.RunStage
	if err := strictJSON(raw, &stage); err != nil {
		return nil, err
	}
	stage.Status = status
	stage.UpdatedAt = at.UTC()
	if status == coreexecution.StageRunning {
		stage.StartedAt = at.UTC()
	}
	return json.Marshal(stage)
}

func promoteExecutionRunForStageTx(
	ctx context.Context,
	tx *sql.Tx,
	owner string,
	runID string,
	stageID string,
	at time.Time,
) error {
	var rowStatus string
	var rowRevision uint64
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT status,revision,snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, owner, runID).Scan(&rowStatus, &rowRevision, &raw); err != nil {
		return err
	}
	if rowStatus != string(coreexecution.RunQueued) && rowStatus != string(coreexecution.RunRunning) {
		return coreexecution.ErrConflict
	}
	var stageKey string
	if err := tx.QueryRowContext(ctx, `SELECT plan_stage_key FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, owner, runID, stageID).Scan(&stageKey); err != nil {
		return err
	}
	var run coreexecution.ExecutionRun
	if strictJSON(raw, &run) != nil ||
		run.OwnerID != owner ||
		run.RunID != runID ||
		run.Status != coreexecution.RunStatus(rowStatus) ||
		run.Revision != rowRevision ||
		strings.TrimSpace(stageKey) == "" {
		return ErrExecutionStoreDrift
	}
	run.Status = coreexecution.RunRunning
	run.CurrentStage = stageKey
	run.CurrentStageID = stageID
	run.Revision++
	if run.StartedAt.IsZero() {
		run.StartedAt = at
	}
	run.UpdatedAt = at
	runRaw, err := json.Marshal(run)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_execution_runs SET status='running',current_stage=$1,current_stage_id=$2,revision=$3,started_at=COALESCE(started_at,$4),snapshot_json=$5,updated_at=$4 WHERE owner_id=$6 AND run_id=$7 AND revision=$8 AND status=$9`, run.CurrentStage, run.CurrentStageID, run.Revision, at, runRaw, owner, runID, rowRevision, rowStatus)
	if err != nil {
		return err
	}
	if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	return insertExecutionRunRevision(ctx, tx, run)
}

// ClaimQueuedExecutionStage is the sole production claim entry point. It
// atomically leases the authoritative execution_stage task, target mutation,
// and run stage; no caller can manufacture a running task fence beforehand.
func (s *DatabaseExecutionStore) ClaimQueuedExecutionStage(ctx context.Context, owner, holder string, ttl time.Duration) (ExecutionStageLeaseClaim, error) {
	var out ExecutionStageLeaseClaim
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(holder) == "" || ttl <= 0 {
		return out, ErrExecutionStoreInvalid
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_runtime_concurrency(singleton,running_count,max_concurrent,revision,updated_at) VALUES(true,0,1,1,$1) ON CONFLICT(singleton) DO NOTHING`, now); err != nil {
		return out, err
	}
	var running, max int
	if err = tx.QueryRowContext(ctx, `SELECT running_count,max_concurrent FROM agent_task_runtime_concurrency WHERE singleton=true FOR UPDATE`).Scan(&running, &max); err != nil {
		return out, err
	}
	if running >= max {
		return out, coreexecution.ErrConflict
	}
	var taskID, runID, stageID, targetID, stageDigest, targetDigest, confirmationID string
	var targetRevision, taskRevision, taskEpoch, taskAttempt, stageRevision uint64
	var stageSnapshot []byte
	executors := s.executors
	filterExecutors := s.executorsAuthoritative
	err = tx.QueryRowContext(ctx, `SELECT t.task_id::text,s.run_id::text,s.stage_id::text,s.target_id::text,s.target_revision,s.target_digest,s.plan_stage_digest,s.revision,t.revision,t.lease_epoch,t.attempt,s.snapshot_json,COALESCE(s.confirmation_id::text,'') FROM agent_tasks t JOIN core_execution_run_stages s ON s.owner_id=t.owner_id AND s.task_id=t.task_id JOIN core_execution_runs r ON r.owner_id=s.owner_id AND r.run_id=s.run_id AND r.status IN ('queued','running') WHERE t.owner_id=$1 AND t.status='queued' AND t.available_at<=$2 AND t.deleted_at IS NULL AND t.spec_json->>'kind'='execution_stage' AND s.status='queued' AND (NOT $3::boolean OR NOT EXISTS (SELECT 1 FROM core_execution_plan_steps ps WHERE ps.owner_id=s.owner_id AND ps.plan_id=s.plan_id AND ps.plan_revision=s.plan_revision AND ps.stage_key=s.plan_stage_key AND ps.step_set=CASE WHEN r.operation='rollback' THEN 'rollback' ELSE 'forward' END AND ((ps.snapshot_json->'step'->>'kind'='compute.provision' AND NOT $4::boolean) OR (ps.snapshot_json->'step'->>'kind'<>'compute.provision' AND NOT $5::boolean)))) ORDER BY t.available_at,t.created_at,t.task_id FOR UPDATE OF t,s SKIP LOCKED LIMIT 1`, owner, now, filterExecutors, executors.ComputeProvision, executors.AWSSSM).Scan(&taskID, &runID, &stageID, &targetID, &targetRevision, &targetDigest, &stageDigest, &stageRevision, &taskRevision, &taskEpoch, &taskAttempt, &stageSnapshot, &confirmationID)
	if errors.Is(err, sql.ErrNoRows) {
		return out, coreexecution.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	// A queued confirmed stage is not dispatchable merely because its task was
	// queued. Revalidate the immutable V2 linkage while the task, stage and
	// confirmation row are locked, then consume the card in this same
	// transaction with the task/stage/target lease fence below.
	var confirmedRevision int64
	if confirmationID != "" {
		locked, lockErr := loadConfirmationForUpdate(ctx, tx, owner, confirmationID)
		if lockErr != nil || locked.State != coreconfirmation.StateConfirmed || !locked.ExpiresAt.After(now) {
			return out, coreexecution.ErrConflict
		}
		record, readErr := readV2Confirmation(ctx, tx, owner, confirmationID)
		if readErr != nil || record.Confirmation.TaskID != taskID || record.Confirmation.State != coreconfirmation.StateConfirmed || !record.Confirmation.ExpiresAt.After(now) || record.Preview.RunID != runID || record.Preview.StageID != stageID {
			return out, coreexecution.ErrConflict
		}
		confirmedRevision = locked.Revision
	}
	taskAttempt++
	taskEpoch++
	taskRevision++
	taskExpiry := now.Add(ttl)
	leaseID, leaseToken := uuid.NewString(), uuid.NewString()
	res, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='running',attempt=$1,lease_epoch=$2,lease_holder=$3,lease_expires_at=$4,execution_started_at=COALESCE(execution_started_at,$5),revision=$6,progress_sequence=progress_sequence+1,updated_at=$5 WHERE owner_id=$7 AND task_id=$8 AND status='queued'`, taskAttempt, taskEpoch, holder, taskExpiry, now, taskRevision, owner, taskID)
	if err != nil {
		return out, err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return out, e
		}
		return out, coreexecution.ErrConflict
	}
	if confirmationID != "" {
		res, err = tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='consumed',reservation_json=jsonb_build_object('task_id',$1::uuid,'attempt',$2::integer,'lease_epoch',$3::bigint,'task_revision',$4::bigint),revision=revision+1,updated_at=$5 WHERE owner_id=$6 AND confirmation_id=$7 AND state='confirmed' AND revision=$8 AND expires_at>$5`, taskID, taskAttempt, taskEpoch, taskRevision, now, owner, confirmationID, confirmedRevision)
		if err != nil {
			return out, err
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			if e != nil {
				return out, e
			}
			return out, coreexecution.ErrConflict
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=running_count+1,revision=revision+1,updated_at=$1 WHERE singleton=true`, now); err != nil {
		return out, err
	}
	var oldEpoch uint64
	var oldStatus, existingLeaseID string
	var oldExpiry sql.NullTime
	err = tx.QueryRowContext(ctx, `SELECT lease_id::text,epoch,status,expires_at FROM core_execution_target_mutation_leases WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3 FOR UPDATE`, owner, targetID, targetRevision).Scan(&existingLeaseID, &oldEpoch, &oldStatus, &oldExpiry)
	targetEpoch := uint64(1)
	if err == nil {
		if oldStatus == "uncertain" || (oldStatus == "active" && oldExpiry.Valid && oldExpiry.Time.After(now)) {
			return out, coreexecution.ErrConflict
		}
		targetEpoch = oldEpoch + 1
		leaseID = existingLeaseID
		if _, err = tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET run_id=$5,stage_id=$6,token=$7,epoch=$8,expires_at=$9,provider_operation_id=CASE WHEN status='released' THEN '' ELSE provider_operation_id END,receipt_id=CASE WHEN status='released' THEN NULL ELSE receipt_id END,status='active',revision=revision+1,updated_at=$10 WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3 AND lease_id=$4`, owner, targetID, targetRevision, leaseID, runID, stageID, leaseToken, targetEpoch, taskExpiry, now); err != nil {
			return out, err
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_target_mutation_leases(owner_id,target_id,target_revision,lease_id,run_id,stage_id,token,epoch,expires_at,revision,status,schema_version,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,1,$8,1,'active','execution-target-lease/v2',$9)`, owner, targetID, targetRevision, leaseID, runID, stageID, leaseToken, taskExpiry, now); err != nil {
			return out, err
		}
	} else {
		return out, err
	}
	stageRevision++
	stageSnapshot, err = successorStageSnapshot(stageSnapshot, coreexecution.StageRunning, now)
	if err != nil {
		return out, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE core_execution_run_stages SET status='running',revision=$1,started_at=COALESCE(started_at,$2),snapshot_json=$3,updated_at=$2 WHERE owner_id=$4 AND run_id=$5 AND stage_id=$6 AND status='queued'`, stageRevision, now, stageSnapshot, owner, runID, stageID)
	if err != nil {
		return out, err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return out, e
		}
		return out, coreexecution.ErrConflict
	}
	if err = promoteExecutionRunForStageTx(ctx, tx, owner, runID, stageID, now); err != nil {
		return out, err
	}
	if err = insertExecutionEvent(ctx, tx, owner, runID, "stage_claimed", stageID, coreexecution.StageRunning, now); err != nil {
		return out, err
	}
	if err = tx.Commit(); err != nil {
		return out, err
	}
	out = ExecutionStageLeaseClaim{OwnerID: owner, RunID: runID, StageID: stageID, TaskID: taskID, Holder: holder, Attempt: uint32(taskAttempt), LeaseEpoch: targetEpoch, TaskLeaseEpoch: taskEpoch, ExpectedTaskRevision: taskRevision, LeaseID: leaseID, LeaseToken: leaseToken, ExpiresAt: taskExpiry}
	_ = stageDigest
	_ = targetDigest
	return out, nil
}

// ClaimNextExecutionStage is the global worker entry point; owner selection
// stays server-side and the returned fence identifies the selected owner.
func (s *DatabaseExecutionStore) ClaimNextExecutionStage(ctx context.Context, holder string, ttl time.Duration) (ExecutionStageLeaseClaim, error) {
	if s == nil || s.db == nil || strings.TrimSpace(holder) == "" || ttl <= 0 {
		return ExecutionStageLeaseClaim{}, ErrExecutionStoreInvalid
	}
	// The worker loop calls this continuously, so sweeping here provides both
	// startup recovery and a bounded periodic V2-only expiry pass before any
	// new dispatch is selected.
	if _, err := s.SweepExpiredV2Confirmations(ctx, 1); err != nil {
		return ExecutionStageLeaseClaim{}, err
	}
	// Recover only a stage for which the durable receipt contains an exact SSM
	// command id, a deterministic CloudFormation stack key, or a durable SSM
	// frozen snapshot. The latter has no provider readback key; the resolver
	// marks it reconcile-only and the runner permanently quarantines it without
	// issuing a second provider mutation.
	if reclaimed, err := s.claimExpiredKnownDispatch(ctx, holder, ttl); err == nil {
		return reclaimed, nil
	} else if !errors.Is(err, coreexecution.ErrNotFound) {
		return ExecutionStageLeaseClaim{}, err
	}
	// An expired running mutation without a durable command receipt cannot be
	// proven absent. Quarantine it before any new work is claimed; explicit
	// reconciliation is the only allowed recovery from this state.
	if err := s.quarantineExpiredDispatchWithoutEvidence(ctx); err != nil && !errors.Is(err, coreexecution.ErrNotFound) {
		return ExecutionStageLeaseClaim{}, err
	}
	var owner string
	err := s.db.QueryRowContext(ctx, `SELECT t.owner_id FROM agent_tasks t JOIN core_execution_run_stages s ON s.owner_id=t.owner_id AND s.task_id=t.task_id JOIN core_execution_runs r ON r.owner_id=s.owner_id AND r.run_id=s.run_id AND r.status IN ('queued','running') WHERE t.status='queued' AND t.available_at<=$1 AND t.deleted_at IS NULL AND t.spec_json->>'kind'='execution_stage' AND s.status='queued' ORDER BY t.available_at,t.created_at,t.task_id LIMIT 1`, s.now().UTC()).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionStageLeaseClaim{}, coreexecution.ErrNotFound
	}
	if err != nil {
		return ExecutionStageLeaseClaim{}, err
	}
	return s.ClaimQueuedExecutionStage(ctx, owner, holder, ttl)
}

func (s *DatabaseExecutionStore) quarantineExpiredDispatchWithoutEvidence(ctx context.Context) error {
	now := s.now().UTC().Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var owner, taskID, runID, stageID, targetID string
	var targetRevision uint64
	err = tx.QueryRowContext(ctx, `SELECT t.owner_id,t.task_id::text,s.run_id::text,s.stage_id::text,s.target_id::text,s.target_revision FROM agent_tasks t JOIN core_execution_run_stages s ON s.owner_id=t.owner_id AND s.task_id=t.task_id JOIN core_execution_target_mutation_leases l ON l.owner_id=s.owner_id AND l.target_id=s.target_id AND l.target_revision=s.target_revision WHERE t.status='running' AND t.lease_expires_at<=$1 AND s.status='running' AND l.status='active' AND l.expires_at<=$1 AND NOT EXISTS (SELECT 1 FROM core_execution_receipts r JOIN core_execution_step_attempts a ON a.owner_id=r.owner_id AND a.attempt_id=r.attempt_id WHERE r.owner_id=s.owner_id AND r.run_id=s.run_id AND a.stage_id=s.stage_id AND r.status IN ('accepted','running') AND (r.command_id<>'' OR r.provider_operation_id<>'')) ORDER BY t.lease_expires_at,t.task_id FOR UPDATE OF t,s,l SKIP LOCKED LIMIT 1`, now).Scan(&owner, &taskID, &runID, &stageID, &targetID, &targetRevision)
	if errors.Is(err, sql.ErrNoRows) {
		return coreexecution.ErrNotFound
	}
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='failed',failure_code='execution_outcome_uncertain',failure_summary='expired execution lease without durable command evidence',lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$1 WHERE owner_id=$2 AND task_id=$3 AND status='running'`, now, owner, taskID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true`, now); err != nil {
		return err
	}
	if err = transitionRunningExecutionStageTx(ctx, tx, owner, runID, stageID, coreexecution.StageUncertain, now); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET status='uncertain',updated_at=$1 WHERE owner_id=$2 AND target_id=$3 AND target_revision=$4 AND status='active'`, now, owner, targetID, targetRevision); err != nil {
		return err
	}
	if _, err = transitionExecutionRunTx(ctx, tx, owner, runID, coreexecution.RunUncertain, "dispatch_evidence_missing", now, false); err != nil {
		return err
	}
	if err = insertExecutionEvent(ctx, tx, owner, runID, "dispatch_evidence_missing_uncertain", stageID, coreexecution.StageUncertain, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DatabaseExecutionStore) claimExpiredKnownDispatch(ctx context.Context, holder string, ttl time.Duration) (ExecutionStageLeaseClaim, error) {
	var out ExecutionStageLeaseClaim
	now := s.now().UTC().Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return out, err
	}
	defer tx.Rollback()
	var owner, taskID, runID, stageID, targetID, leaseID string
	var targetRevision, taskAttempt, taskEpoch, taskRevision, targetEpoch uint64
	err = tx.QueryRowContext(ctx, `SELECT t.owner_id,t.task_id::text,s.run_id::text,s.stage_id::text,s.target_id::text,s.target_revision,t.attempt,t.lease_epoch,t.revision,l.lease_id::text,l.epoch FROM agent_tasks t JOIN core_execution_run_stages s ON s.owner_id=t.owner_id AND s.task_id=t.task_id JOIN core_execution_target_mutation_leases l ON l.owner_id=s.owner_id AND l.target_id=s.target_id AND l.target_revision=s.target_revision JOIN core_execution_receipts r ON r.owner_id=s.owner_id AND r.run_id=s.run_id JOIN core_execution_step_attempts a ON a.owner_id=r.owner_id AND a.attempt_id=r.attempt_id AND a.stage_id=s.stage_id LEFT JOIN core_execution_dispatch_intents i ON i.owner_id=r.owner_id AND i.receipt_id=r.receipt_id AND i.attempt_id=r.attempt_id WHERE t.status='running' AND t.lease_expires_at<=$1 AND s.status='running' AND l.status='active' AND l.expires_at<=$1 AND r.status IN ('accepted','running') AND (r.command_id<>'' OR r.provider_operation_id<>'' OR i.snapshot_json ? 'frozen_request_snapshot') AND a.status='running' ORDER BY t.lease_expires_at,t.task_id FOR UPDATE OF t,s,l,r,a SKIP LOCKED LIMIT 1`, now).Scan(&owner, &taskID, &runID, &stageID, &targetID, &targetRevision, &taskAttempt, &taskEpoch, &taskRevision, &leaseID, &targetEpoch)
	if errors.Is(err, sql.ErrNoRows) {
		return out, coreexecution.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	taskAttempt++
	taskEpoch++
	taskRevision++
	targetEpoch++
	expires := now.Add(ttl)
	token := uuid.NewString()
	res, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET attempt=$1,lease_epoch=$2,lease_holder=$3,lease_expires_at=$4,revision=$5,progress_sequence=progress_sequence+1,updated_at=$6 WHERE owner_id=$7 AND task_id=$8 AND status='running' AND revision=$9 AND lease_expires_at<=$6`, taskAttempt, taskEpoch, holder, expires, taskRevision, now, owner, taskID, taskRevision-1)
	if err != nil {
		return out, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return out, coreexecution.ErrConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET token=$2,epoch=$3,expires_at=$4,revision=revision+1,updated_at=$5 WHERE owner_id=$6 AND target_id=$7 AND target_revision=$8 AND lease_id=$1 AND status='active' AND epoch=$9 AND expires_at<=$5`, leaseID, token, targetEpoch, expires, now, owner, targetID, targetRevision, targetEpoch-1)
	if err != nil {
		return out, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return out, coreexecution.ErrConflict
	}
	if err := insertExecutionEvent(ctx, tx, owner, runID, "stage_reclaimed_poll_only", stageID, coreexecution.StageRunning, now); err != nil {
		return out, err
	}
	if err := tx.Commit(); err != nil {
		return out, err
	}
	return ExecutionStageLeaseClaim{OwnerID: owner, RunID: runID, StageID: stageID, TaskID: taskID, Holder: holder, Attempt: uint32(taskAttempt), LeaseEpoch: targetEpoch, TaskLeaseEpoch: taskEpoch, ExpectedTaskRevision: taskRevision, LeaseID: leaseID, LeaseToken: token, ExpiresAt: expires}, nil
}

// RecordDispatchIntent persists the attempt and accepted receipt in one
// transaction. It is safe to call once per immutable step/attempt key.
func (s *DatabaseExecutionStore) RecordDispatchIntent(ctx context.Context, in ExecutionDispatchIntent) error {
	if s == nil || s.db == nil || strings.TrimSpace(in.Attempt.OwnerID) == "" || !coreexecution.ValidateUUID(in.Attempt.AttemptID) || !coreexecution.ValidateUUID(in.Attempt.RunID) || !coreexecution.ValidateUUID(in.Attempt.StageID) || !coreexecution.ValidateUUID(in.Receipt.ReceiptID) || in.Attempt.StepKey == "" || !in.Attempt.StepDigest.Valid() || (in.StepSet != coreexecution.StepSetForward && in.StepSet != coreexecution.StepSetRollback) || in.Receipt.Status != coreexecution.ReceiptAccepted || !coreexecution.ValidateUUID(in.LeaseID) || !coreexecution.ValidateUUID(in.LeaseToken) || in.LeaseEpoch == 0 {
		return ErrExecutionStoreInvalid
	}
	if !coreexecution.ValidateUUID(in.TaskID) || strings.TrimSpace(in.TaskHolder) == "" || in.TaskAttempt == 0 || in.TaskRevision == 0 || in.TaskLeaseEpoch == 0 {
		return ErrExecutionStoreInvalid
	}
	if !in.RequestDigest.Valid() || !in.FenceDigest.Valid() {
		return ErrExecutionStoreInvalid
	}
	// A durable intent without the exact redacted frozen request cannot be
	// reconciled after restart without risking a second provider mutation.
	// Zero-snapshot compatibility is deliberately not supported in V2.
	snap := in.Snapshot
	if in.SecretProvision != nil {
		req := *in.SecretProvision
		if in.StepSet != coreexecution.StepSetForward || req.OwnerID != in.Attempt.OwnerID || req.PlanID != in.Attempt.PlanID || req.PlanRevision != in.Attempt.PlanRevision || req.PlanDigest != in.Attempt.PlanDigest || req.RunID != in.Attempt.RunID || req.StageID != in.Attempt.StageID || req.AttemptID != in.Attempt.AttemptID || req.RequestDigest != in.RequestDigest || req.FenceDigest != in.FenceDigest || req.Target.ID != in.TargetID || req.Target.Revision != in.TargetRevision || req.Target.Digest != in.TargetDigest || req.StepRevision != in.Attempt.StepRevision || req.StepDigest != in.Attempt.StepDigest || req.SecretRef.Ref == "" || req.Credential.ID == "" {
			return fmt.Errorf("%w: secret provision immutable snapshot fence", ErrExecutionStoreInvalid)
		}
		// The persisted request is metadata-only. A private credential payload
		// is never accepted into the dispatch snapshot, even though Credentials
		// custom JSON marshaling is redacted as a secondary defense.
		if access, secret, token := req.Credential.StoredSecretBytes(); len(access) != 0 || len(secret) != 0 || len(token) != 0 {
			return fmt.Errorf("%w: secret provision plaintext credential", ErrExecutionStoreInvalid)
		}
	} else if in.EC2Provision != nil {
		req := *in.EC2Provision
		intent := coreaws.EC2ProvisionIntent{OwnerID: req.OwnerID, FenceDigest: req.FenceDigest, RequestDigest: req.RequestDigest, ProviderOperationKey: coreaws.EC2ProvisionOperationKey(req.Target.ID), Request: req}
		metadataErr := validateCatalogSensitiveData(req)
		switch {
		case in.StepSet != coreexecution.StepSetForward:
			return fmt.Errorf("%w: ec2 provision forward-step fence", ErrExecutionStoreInvalid)
		case coreaws.ValidateEC2ProvisionIntentSnapshot(intent) != nil:
			return fmt.Errorf("%w: ec2 provision immutable snapshot fence", ErrExecutionStoreInvalid)
		case req.OwnerID != in.Attempt.OwnerID || req.PlanID != in.Attempt.PlanID || req.PlanRevision != in.Attempt.PlanRevision || req.PlanDigest != in.Attempt.PlanDigest || req.RunID != in.Attempt.RunID || req.StageID != in.Attempt.StageID || req.AttemptID != in.Attempt.AttemptID:
			return fmt.Errorf("%w: ec2 provision execution identity fence", ErrExecutionStoreInvalid)
		case req.RequestDigest != in.RequestDigest || req.FenceDigest != in.FenceDigest:
			return fmt.Errorf("%w: ec2 provision request digest fence", ErrExecutionStoreInvalid)
		case req.Target.ID != in.TargetID || req.Target.Revision != in.TargetRevision || req.Target.Digest != in.TargetDigest:
			return fmt.Errorf("%w: ec2 provision target fence", ErrExecutionStoreInvalid)
		case req.Step.StepKey != in.Attempt.StepKey || req.StepRevision != in.Attempt.StepRevision || req.Step.Digest != in.Attempt.StepDigest:
			return fmt.Errorf("%w: ec2 provision step fence", ErrExecutionStoreInvalid)
		case metadataErr != nil:
			return fmt.Errorf("%w: ec2 provision safe metadata fence: %v", ErrExecutionStoreInvalid, metadataErr)
		}
	} else {
		if snap.OwnerID != in.Attempt.OwnerID || snap.PlanID != in.Attempt.PlanID || snap.PlanRevision != in.Attempt.PlanRevision || snap.PlanDigest != in.Attempt.PlanDigest || snap.RunID != in.Attempt.RunID || snap.StageID != in.Attempt.StageID || snap.AttemptID != in.Attempt.AttemptID || snap.RequestDigest != in.RequestDigest || snap.FenceDigest != in.FenceDigest || snap.TargetID != in.TargetID || snap.TargetRevision != in.TargetRevision || snap.TargetDigest != in.TargetDigest || snap.StepKey != in.Attempt.StepKey || snap.StepRevision != in.Attempt.StepRevision || snap.StepDigest != in.Attempt.StepDigest || !coreaws.IsExecutableSSMStep(snap.Script.Step) {
			return fmt.Errorf("%w: ssm snapshot identity or step", ErrExecutionStoreInvalid)
		}
		frozen := coreaws.FrozenRequest{OwnerID: snap.OwnerID, PlanID: snap.PlanID, PlanRevision: snap.PlanRevision, PlanDigest: snap.PlanDigest, RunID: snap.RunID, RunRevision: snap.RunRevision, RunDigest: snap.RunDigest, StageID: snap.StageID, StageRevision: snap.StageRevision, StageDigest: snap.StageDigest, StepKey: snap.StepKey, StepRevision: snap.StepRevision, StepDigest: snap.StepDigest, AttemptID: snap.AttemptID, Attempt: snap.Attempt, Fence: snap.Fence, FenceDigest: snap.FenceDigest, RequestDigest: snap.RequestDigest, Target: snap.Target, TargetID: snap.TargetID, TargetRevision: snap.TargetRevision, TargetDigest: snap.TargetDigest, InstanceID: snap.InstanceID, Credential: coreaws.RehydrateCredentialMetadata(snap.CredentialID, "frozen", snap.CredentialRegion, snap.CredentialAccountID, snap.CredentialUserARN, int64(snap.CredentialRevision), int64(snap.CredentialRevision), time.Time{}, time.Time{}), CredentialID: snap.CredentialID, CredentialRevision: snap.CredentialRevision, Observation: snap.Observation, Script: snap.Script}
		fence, err := coreaws.CanonicalFenceDigest(frozen)
		if err != nil || fence != in.FenceDigest {
			return fmt.Errorf("%w: ssm snapshot fence digest", ErrExecutionStoreInvalid)
		}
		request, err := coreaws.CanonicalRequestDigest(frozen)
		if err != nil || request != in.RequestDigest {
			return fmt.Errorf("%w: ssm snapshot request digest", ErrExecutionStoreInvalid)
		}
		if err := validateCatalogSensitiveData(snap); err != nil {
			// validateCatalogSensitiveData reports only a closed field-name path;
			// including it keeps provider diagnostics actionable without ever
			// reflecting a rejected value or secret material.
			return fmt.Errorf("%w: ssm snapshot safe metadata: %v", ErrExecutionStoreInvalid, err)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := s.now().UTC().Truncate(time.Microsecond)
	var stageStatus, targetID, targetDigest, stagePlanID, stagePlanDigest, planDigestRoot string
	var stageTaskID sql.NullString
	var targetRevision, leaseEpoch, stagePlanRevision, stageRevision uint64
	var leaseStatus string
	if err = tx.QueryRowContext(ctx, `SELECT s.status,s.target_id::text,s.target_revision,s.target_digest,s.task_id::text,s.plan_id::text,s.plan_revision,s.plan_digest,s.plan_stage_digest,s.stage_revision,l.status,l.epoch FROM core_execution_run_stages s JOIN core_execution_target_mutation_leases l ON l.owner_id=s.owner_id AND l.target_id=s.target_id AND l.target_revision=s.target_revision WHERE s.owner_id=$1 AND s.run_id=$2 AND s.stage_id=$3 AND l.lease_id=$4 AND l.token=$5 FOR UPDATE`, in.Attempt.OwnerID, in.Attempt.RunID, in.Attempt.StageID, in.LeaseID, in.LeaseToken).Scan(&stageStatus, &targetID, &targetRevision, &targetDigest, &stageTaskID, &stagePlanID, &stagePlanRevision, &planDigestRoot, &stagePlanDigest, &stageRevision, &leaseStatus, &leaseEpoch); err != nil {
		return coreexecution.ErrConflict
	}
	if stageStatus != string(coreexecution.StageRunning) || leaseStatus != "active" || targetID != in.TargetID || targetRevision != in.TargetRevision || targetDigest != string(in.TargetDigest) || leaseEpoch != in.LeaseEpoch || stagePlanID != in.Attempt.PlanID || stagePlanRevision != in.Attempt.PlanRevision || planDigestRoot != string(in.Attempt.PlanDigest) || stageRevision != in.Attempt.StageRevision || stagePlanDigest != string(in.Attempt.StageDigest) || !stageTaskID.Valid || stageTaskID.String != in.TaskID {
		return coreexecution.ErrConflict
	}
	if stageTaskID.Valid {
		var taskStatus, holder string
		var taskAttempt, taskEpoch, taskRevision uint64
		var taskExpiry sql.NullTime
		if err = tx.QueryRowContext(ctx, `SELECT status,lease_holder,attempt,lease_epoch,revision,lease_expires_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, in.Attempt.OwnerID, stageTaskID.String).Scan(&taskStatus, &holder, &taskAttempt, &taskEpoch, &taskRevision, &taskExpiry); err != nil {
			return coreexecution.ErrConflict
		}
		if taskStatus != "running" || holder != in.TaskHolder || taskAttempt != uint64(in.TaskAttempt) || taskRevision != in.TaskRevision || taskEpoch != in.TaskLeaseEpoch || !taskExpiry.Valid || !taskExpiry.Time.After(now) {
			return coreexecution.ErrConflict
		}
	}
	attemptRaw, _ := json.Marshal(map[string]any{
		"attempt_id": in.Attempt.AttemptID, "run_id": in.Attempt.RunID, "stage_id": in.Attempt.StageID,
		"owner_id": in.Attempt.OwnerID, "plan_id": stagePlanID, "plan_revision": stagePlanRevision,
		"plan_digest": in.Attempt.PlanDigest, "stage_revision": stageRevision, "stage_digest": in.Attempt.StageDigest,
		"step_key": in.Attempt.StepKey, "step_set": in.StepSet, "step_revision": in.Attempt.StepRevision, "step_digest": in.Attempt.StepDigest,
		"attempt": in.Attempt.Attempt, "revision": uint64(1), "status": coreexecution.AttemptRunning,
	})
	res, err := tx.ExecContext(ctx, `INSERT INTO core_execution_step_attempts(owner_id,attempt_id,run_id,stage_id,project_id,plan_id,plan_revision,plan_stage_key,step_key,step_set,step_revision,step_digest,attempt_no,revision,status,schema_version,snapshot_json,started_at) SELECT $1,$2,s.run_id,s.stage_id,s.project_id,s.plan_id,s.plan_revision,s.plan_stage_key,$5,$6,$7,$8,$9,1,'running','execution-step-attempt/v2',$10,$11 FROM core_execution_run_stages s WHERE s.owner_id=$1 AND s.run_id=$3 AND s.stage_id=$4 ON CONFLICT (owner_id,run_id,stage_id,step_key,attempt_no) DO NOTHING`, in.Attempt.OwnerID, in.Attempt.AttemptID, in.Attempt.RunID, in.Attempt.StageID, in.Attempt.StepKey, in.StepSet, in.Attempt.StepRevision, in.Attempt.StepDigest, in.Attempt.Attempt, attemptRaw, now)
	if err != nil {
		return mapExecutionConflict(err)
	}
	if n, e := res.RowsAffected(); e != nil {
		return e
	} else if n == 0 {
		var old string
		if err = tx.QueryRowContext(ctx, `SELECT step_digest FROM core_execution_step_attempts WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 AND step_key=$4 AND attempt_no=$5`, in.Attempt.OwnerID, in.Attempt.RunID, in.Attempt.StageID, in.Attempt.StepKey, in.Attempt.Attempt).Scan(&old); err != nil || old != string(in.Attempt.StepDigest) {
			return coreexecution.ErrConflict
		}
	}
	receiptRaw := receiptSnapshot(in.Receipt, in.RequestDigest, in.FenceDigest)
	idemDigest := string(digestBytes([]byte(in.Receipt.IdempotencyKey)))
	res, err = tx.ExecContext(ctx, `INSERT INTO core_execution_receipts(owner_id,receipt_id,run_id,attempt_id,provider_operation_id,command_id,idempotency_digest,request_digest,fence_digest,response_digest,revision,status,schema_version,snapshot_json,created_at) VALUES($1,$2,$3,$4,'','',$5,$6,$7,NULL,1,'accepted','execution-receipt/v2',$8,$9) ON CONFLICT (owner_id,run_id,idempotency_digest) DO NOTHING`, in.Attempt.OwnerID, in.Receipt.ReceiptID, in.Attempt.RunID, in.Attempt.AttemptID, idemDigest, in.RequestDigest, in.FenceDigest, receiptRaw, now)
	if err != nil {
		return mapExecutionConflict(err)
	}
	if n, e := res.RowsAffected(); e != nil {
		return e
	} else if n == 0 {
		var oldReq, oldFence, oldAttempt string
		if err = tx.QueryRowContext(ctx, `SELECT request_digest,fence_digest,attempt_id::text FROM core_execution_receipts WHERE owner_id=$1 AND run_id=$2 AND idempotency_digest=$3 FOR UPDATE`, in.Attempt.OwnerID, in.Attempt.RunID, idemDigest).Scan(&oldReq, &oldFence, &oldAttempt); err != nil || oldReq != string(in.RequestDigest) || oldFence != string(in.FenceDigest) || oldAttempt != in.Attempt.AttemptID {
			return coreexecution.ErrConflict
		}
	}
	if stageTaskID.Valid {
		intentRaw := map[string]any{
			"owner_id": in.Attempt.OwnerID, "run_id": in.Attempt.RunID, "stage_id": in.Attempt.StageID,
			"attempt_id": in.Attempt.AttemptID, "receipt_id": in.Receipt.ReceiptID, "task_id": in.TaskID,
			"task_holder": in.TaskHolder, "task_attempt": in.TaskAttempt, "task_revision": in.TaskRevision, "task_lease_epoch": in.TaskLeaseEpoch,
			"target_id": in.TargetID, "target_revision": in.TargetRevision, "target_digest": in.TargetDigest,
			"plan_id": in.Attempt.PlanID, "plan_revision": in.Attempt.PlanRevision, "plan_digest": in.Attempt.PlanDigest,
			"stage_revision": in.Attempt.StageRevision, "stage_digest": in.Attempt.StageDigest,
			"step_key": in.Attempt.StepKey, "step_set": in.StepSet, "step_revision": in.Attempt.StepRevision, "step_digest": in.Attempt.StepDigest,
			"attempt_no": in.Attempt.Attempt, "lease_id": in.LeaseID, "lease_token": in.LeaseToken, "lease_epoch": in.LeaseEpoch,
			"request_digest": in.RequestDigest, "fence_digest": in.FenceDigest, "status": "intent",
		}
		if in.SecretProvision != nil {
			intentRaw["secret_parameter_request"] = in.SecretProvision
		} else if in.EC2Provision != nil {
			intentRaw["ec2_provision_request"] = in.EC2Provision
		} else {
			intentRaw["frozen_request_snapshot"] = in.Snapshot
		}
		intentJSON, e := json.Marshal(intentRaw)
		if e != nil {
			return e
		}
		intentResult, e := tx.ExecContext(ctx, `INSERT INTO core_execution_dispatch_intents(owner_id,intent_id,run_id,stage_id,attempt_id,receipt_id,task_id,task_lease_epoch,target_id,target_revision,target_digest,plan_id,plan_revision,plan_digest,stage_revision,stage_digest,step_key,step_set,step_revision,step_digest,attempt_no,lease_id,lease_token,lease_epoch,request_digest,fence_digest,status,revision,schema_version,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,'intent',1,'execution-dispatch-intent/v2',$27,$28,$28) ON CONFLICT (owner_id,fence_digest) DO NOTHING`, in.Attempt.OwnerID, in.Receipt.ReceiptID, in.Attempt.RunID, in.Attempt.StageID, in.Attempt.AttemptID, in.Receipt.ReceiptID, stageTaskID.String, in.TaskLeaseEpoch, in.TargetID, in.TargetRevision, in.TargetDigest, in.Attempt.PlanID, in.Attempt.PlanRevision, in.Attempt.PlanDigest, in.Attempt.StageRevision, in.Attempt.StageDigest, in.Attempt.StepKey, in.StepSet, in.Attempt.StepRevision, in.Attempt.StepDigest, in.Attempt.Attempt, in.LeaseID, in.LeaseToken, in.LeaseEpoch, in.RequestDigest, in.FenceDigest, intentJSON, now)
		if e != nil {
			return mapExecutionConflict(e)
		}
		if n, e := intentResult.RowsAffected(); e != nil || n == 0 {
			if e != nil {
				return e
			}
			var oldRequest, oldAttempt, oldStep string
			if e = tx.QueryRowContext(ctx, `SELECT request_digest,attempt_id::text,step_digest FROM core_execution_dispatch_intents WHERE owner_id=$1 AND fence_digest=$2 FOR UPDATE`, in.Attempt.OwnerID, in.FenceDigest).Scan(&oldRequest, &oldAttempt, &oldStep); e != nil || oldRequest != string(in.RequestDigest) || oldAttempt != in.Attempt.AttemptID || oldStep != string(in.Attempt.StepDigest) {
				return coreexecution.ErrConflict
			}
		}
	}
	return tx.Commit()
}

func receiptSnapshot(r coreexecution.Receipt, request, fence coreexecution.Digest) []byte {
	obj := map[string]any{
		"receipt_id": r.ReceiptID, "run_id": r.RunID, "owner_id": r.OwnerID, "revision": uint64(1),
		"attempt_id": r.AttemptID, "status": coreexecution.ReceiptAccepted, "idempotency_key": r.IdempotencyKey,
		"request_digest": request, "fence_digest": fence,
	}
	result, _ := json.Marshal(obj)
	return result
}

func (s *DatabaseExecutionStore) MarkDispatchUncertain(ctx context.Context, owner, attemptID, receiptID string, evidence ...coreexecution.Digest) error {
	return s.markDispatchUncertain(ctx, owner, attemptID, receiptID, "", "", evidence...)
}

// markDispatchUncertain commits a known provider command id in the same
// transaction as the accepted/running -> uncertain transition.  A command id
// returned by SendCommand must never be lost between two SQL transactions.
func (s *DatabaseExecutionStore) markDispatchUncertain(ctx context.Context, owner, attemptID, receiptID, commandID, providerOperationID string, evidence ...coreexecution.Digest) error {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(attemptID) || !coreexecution.ValidateUUID(receiptID) {
		return ErrExecutionStoreInvalid
	}
	// The migration trigger makes uncertain evidence immutable.  A repeated
	// best-effort call is therefore an idempotent no-op, never a late command-id
	// fill that could conceal split-brain provider evidence.
	var existing string
	if err := s.db.QueryRowContext(ctx, `SELECT status FROM core_execution_receipts WHERE owner_id=$1 AND receipt_id=$2 AND attempt_id=$3`, owner, receiptID, attemptID).Scan(&existing); err == nil && existing == string(coreexecution.ReceiptUncertain) {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	digest := ""
	if len(evidence) > 0 {
		digest = string(evidence[0])
	}
	if digest == "" {
		digest = string(digestBytes([]byte(owner + "\x00" + attemptID + "\x00" + receiptID)))
	}
	var runID, stageID string
	var taskID sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT a.run_id::text,a.stage_id::text,s.task_id::text FROM core_execution_step_attempts a JOIN core_execution_run_stages s ON s.owner_id=a.owner_id AND s.run_id=a.run_id AND s.stage_id=a.stage_id WHERE a.owner_id=$1 AND a.attempt_id=$2 FOR UPDATE`, owner, attemptID).Scan(&runID, &stageID, &taskID); err != nil {
		return coreexecution.ErrConflict
	}
	res, e := tx.ExecContext(ctx, `UPDATE core_execution_step_attempts SET status='uncertain',revision=revision+1,output_digest=$3 WHERE owner_id=$1 AND attempt_id=$2 AND status IN ('running','pending')`, owner, attemptID, digest)
	if e != nil {
		err = e
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	if taskID.Valid {
		if e = terminalizeExecutionTaskTx(ctx, tx, owner, taskID.String, "failed", "execution_outcome_uncertain", "external dispatch outcome uncertain", s.now()); e != nil {
			return e
		}
	}
	res, e = tx.ExecContext(ctx, `UPDATE core_execution_receipts SET status='uncertain',response_digest=$4,command_id=CASE WHEN command_id='' THEN $5 ELSE command_id END,provider_operation_id=CASE WHEN provider_operation_id='' THEN $6 ELSE provider_operation_id END,revision=revision+1 WHERE owner_id=$1 AND receipt_id=$2 AND attempt_id=$3 AND status IN ('accepted','running')`, owner, receiptID, attemptID, digest, strings.TrimSpace(commandID), strings.TrimSpace(providerOperationID))
	if e != nil {
		return e
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	res, e = tx.ExecContext(ctx, `UPDATE core_execution_dispatch_intents SET status='uncertain',revision=revision+1,updated_at=$4 WHERE owner_id=$1 AND attempt_id=$2 AND receipt_id=$3 AND status IN ('intent','accepted')`, owner, attemptID, receiptID, s.now().UTC().Truncate(time.Microsecond))
	if e != nil {
		return e
	}
	if n, e := res.RowsAffected(); e != nil || n > 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	res, e = tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET status='uncertain',receipt_id=$4,provider_operation_id=CASE WHEN provider_operation_id='' THEN $5 ELSE provider_operation_id END,updated_at=$6 WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 AND status='active'`, owner, runID, stageID, receiptID, strings.TrimSpace(providerOperationID), s.now().UTC().Truncate(time.Microsecond))
	if e != nil {
		return e
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	transitionedAt := s.now().UTC().Truncate(time.Microsecond)
	if e = transitionRunningExecutionStageTx(ctx, tx, owner, runID, stageID, coreexecution.StageUncertain, transitionedAt); e != nil {
		return e
	}
	if _, e = transitionExecutionRunTx(ctx, tx, owner, runID, coreexecution.RunUncertain, "provider_outcome_uncertain", transitionedAt, false); e != nil {
		return e
	}
	if e = insertExecutionEvent(ctx, tx, owner, runID, "dispatch_uncertain", stageID, coreexecution.StageUncertain, s.now().UTC().Truncate(time.Microsecond)); e != nil {
		return e
	}
	return tx.Commit()
}

// MarkDispatchUncertainWithCommand preserves a provider command id when the
// response was accepted but receipt persistence or readback became uncertain.
// Reconciliation can therefore poll the exact SSM command without issuing a
// second mutation.
func (s *DatabaseExecutionStore) MarkDispatchUncertainWithCommand(ctx context.Context, owner, attemptID, receiptID, commandID string, evidence ...coreexecution.Digest) error {
	if s == nil || s.db == nil {
		return ErrExecutionStoreInvalid
	}
	return s.markDispatchUncertain(ctx, owner, attemptID, receiptID, commandID, "", evidence...)
}

// MarkDispatchUncertainWithProvider preserves a deterministic provider
// operation key (CloudFormation stack name) for readback-only reconciliation.
func (s *DatabaseExecutionStore) MarkDispatchUncertainWithProvider(ctx context.Context, owner, attemptID, receiptID, providerOperationID string, evidence ...coreexecution.Digest) error {
	if s == nil || s.db == nil || strings.TrimSpace(providerOperationID) == "" {
		return ErrExecutionStoreInvalid
	}
	return s.markDispatchUncertain(ctx, owner, attemptID, receiptID, "", providerOperationID, evidence...)
}

// RecordAccepted binds the provider command/operation identifier to the
// previously persisted dispatch intent. It is owner- and attempt-fenced.
func (s *DatabaseExecutionStore) RecordAccepted(ctx context.Context, owner, receiptID, attemptID, commandID string) error {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(receiptID) || !coreexecution.ValidateUUID(attemptID) || strings.TrimSpace(commandID) == "" {
		return ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM core_execution_receipts WHERE owner_id=$1 AND receipt_id=$2 AND attempt_id=$3 FOR UPDATE`, owner, receiptID, attemptID).Scan(&status); err != nil {
		return coreexecution.ErrConflict
	}
	if status != "accepted" {
		return coreexecution.ErrConflict
	}
	res, err := tx.ExecContext(ctx, `UPDATE core_execution_receipts SET command_id=$1,status='running',revision=revision+1 WHERE owner_id=$2 AND receipt_id=$3 AND attempt_id=$4 AND status='accepted'`, commandID, owner, receiptID, attemptID)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE core_execution_dispatch_intents SET status='accepted',revision=revision+1,updated_at=$4 WHERE owner_id=$1 AND receipt_id=$2 AND attempt_id=$3 AND status='intent'`, owner, receiptID, attemptID, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n > 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	return tx.Commit()
}

// ResolveDispatchReceipt records the typed provider outcome under the exact
// owner/attempt fence. A missing or mismatched fence cannot resolve evidence.
func (s *DatabaseExecutionStore) FinalizeDispatchReceipt(ctx context.Context, owner, receiptID, attemptID string, status coreexecution.ReceiptStatus, responseDigest coreexecution.Digest) error {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(receiptID) || !coreexecution.ValidateUUID(attemptID) || (status != coreexecution.ReceiptSucceeded && status != coreexecution.ReceiptFailed && status != coreexecution.ReceiptUncertain) || !responseDigest.Valid() {
		return ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var oldStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM core_execution_receipts WHERE owner_id=$1 AND receipt_id=$2 AND attempt_id=$3 FOR UPDATE`, owner, receiptID, attemptID).Scan(&oldStatus); err != nil {
		return coreexecution.ErrConflict
	}
	if oldStatus != "accepted" && oldStatus != "running" {
		return coreexecution.ErrConflict
	}
	res, err := tx.ExecContext(ctx, `UPDATE core_execution_receipts SET status=$1,response_digest=$2,revision=revision+1 WHERE owner_id=$3 AND receipt_id=$4 AND attempt_id=$5 AND status IN ('accepted','running')`, status, responseDigest, owner, receiptID, attemptID)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	attemptStatus := "succeeded"
	if status == coreexecution.ReceiptFailed {
		attemptStatus = "failed"
	}
	if status == coreexecution.ReceiptUncertain {
		attemptStatus = "uncertain"
	}
	res, err = tx.ExecContext(ctx, `UPDATE core_execution_step_attempts SET status=$1,revision=revision+1,output_digest=$2 WHERE owner_id=$3 AND attempt_id=$4 AND status IN ('running','pending')`, attemptStatus, responseDigest, owner, attemptID)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	var runID, stageID string
	var taskID sql.NullString
	if err = tx.QueryRowContext(ctx, `SELECT a.run_id::text,a.stage_id::text,s.task_id::text FROM core_execution_step_attempts a JOIN core_execution_run_stages s ON s.owner_id=a.owner_id AND s.run_id=a.run_id AND s.stage_id=a.stage_id WHERE a.owner_id=$1 AND a.attempt_id=$2`, owner, attemptID).Scan(&runID, &stageID, &taskID); err != nil {
		return err
	}
	var stepSet string
	var remaining bool
	if err = tx.QueryRowContext(ctx, `SELECT a.step_set,EXISTS (SELECT 1 FROM core_execution_plan_steps ps JOIN core_execution_run_stages rs ON rs.owner_id=ps.owner_id AND rs.plan_id=ps.plan_id AND rs.plan_revision=ps.plan_revision AND rs.plan_stage_key=ps.stage_key WHERE rs.owner_id=a.owner_id AND rs.run_id=a.run_id AND rs.stage_id=a.stage_id AND ps.step_set=a.step_set AND NOT EXISTS (SELECT 1 FROM core_execution_step_attempts ax WHERE ax.owner_id=a.owner_id AND ax.run_id=a.run_id AND ax.stage_id=a.stage_id AND ax.step_set=a.step_set AND ax.step_key=ps.step_key AND ax.status='succeeded')) FROM core_execution_step_attempts a WHERE a.owner_id=$1 AND a.attempt_id=$2`, owner, attemptID).Scan(&stepSet, &remaining); err != nil {
		return err
	}
	if status == coreexecution.ReceiptSucceeded && remaining {
		res, e := tx.ExecContext(ctx, `UPDATE core_execution_dispatch_intents SET status='succeeded',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND receipt_id=$3 AND attempt_id=$4 AND status IN ('intent','accepted')`, s.now().UTC().Truncate(time.Microsecond), owner, receiptID, attemptID)
		if e != nil {
			return e
		}
		if n, e := res.RowsAffected(); e != nil || n != 1 {
			if e != nil {
				return e
			}
			return coreexecution.ErrConflict
		}
		return tx.Commit()
	}
	_ = stepSet
	stageStatus := string(coreexecution.StageSucceeded)
	if status == coreexecution.ReceiptFailed {
		stageStatus = string(coreexecution.StageFailed)
	}
	if status == coreexecution.ReceiptUncertain {
		stageStatus = string(coreexecution.StageUncertain)
	}
	terminalAt := s.now().UTC().Truncate(time.Microsecond)
	if err = transitionRunningExecutionStageTx(ctx, tx, owner, runID, stageID, coreexecution.StageStatus(stageStatus), terminalAt); err != nil {
		return fmt.Errorf("finalize receipt stage terminal transition: %w", err)
	}
	leaseStatus := "released"
	if status == coreexecution.ReceiptUncertain {
		leaseStatus = "uncertain"
	}
	res, err = tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET status=$1,receipt_id=CASE WHEN $1='uncertain' THEN $2 ELSE receipt_id END,updated_at=$3 WHERE owner_id=$4 AND run_id=$5 AND stage_id=$6 AND status IN ('active','uncertain')`, leaseStatus, receiptID, s.now().UTC().Truncate(time.Microsecond), owner, runID, stageID)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n > 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE core_execution_dispatch_intents SET status=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND receipt_id=$4 AND attempt_id=$5 AND status IN ('intent','accepted')`, string(status), s.now().UTC().Truncate(time.Microsecond), owner, receiptID, attemptID)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil || n > 1 {
		if e != nil {
			return e
		}
		return coreexecution.ErrConflict
	}
	if taskID.Valid {
		taskStatus := "succeeded"
		taskCode, taskSummary := "", ""
		if status == coreexecution.ReceiptFailed {
			taskStatus = "failed"
			taskCode, taskSummary = "execution_failed", "execution provider outcome failed"
		}
		if status == coreexecution.ReceiptUncertain {
			taskStatus = "failed"
			taskCode, taskSummary = "execution_outcome_uncertain", "external dispatch outcome uncertain"
		}
		if err = terminalizeExecutionTaskTx(ctx, tx, owner, taskID.String, taskStatus, taskCode, taskSummary, terminalAt); err != nil {
			return err
		}
		if status != coreexecution.ReceiptUncertain {
			// A consumed confirmation reserves its target only while the task is
			// live. Once this terminal receipt is committed, release that durable
			// reservation in the same transaction so a dependency child can take
			// the target without weakening uncertain-outcome fencing.
			if _, err = tx.ExecContext(ctx, `UPDATE agent_confirmations SET reservation_json=NULL,revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND task_id=$3 AND state='consumed' AND reservation_json IS NOT NULL`, terminalAt, owner, taskID.String); err != nil {
				return fmt.Errorf("finalize receipt confirmation reservation release: %w", err)
			}
		}
	}
	if status == coreexecution.ReceiptFailed {
		if err = skipBlockedExecutionDescendantsTx(ctx, tx, owner, runID, terminalAt); err != nil {
			return err
		}
	}
	// A successful stage may unlock more of the persisted DAG.  Materialize
	// those children in this same transaction: a state-only promotion would
	// leave an executable stage without its exact task/confirmation fence.
	if status == coreexecution.ReceiptSucceeded {
		if err := s.materializeNewlyUnblockedStages(ctx, tx, owner, runID, terminalAt); err != nil {
			return fmt.Errorf("finalize receipt DAG materialization: %w", err)
		}
	}
	if status != coreexecution.ReceiptUncertain {
		runStatus := coreexecution.RunSucceeded
		terminalReason := ""
		if status == coreexecution.ReceiptFailed {
			runStatus = coreexecution.RunFailed
			terminalReason = "stage_failed"
		}
		if _, err = transitionExecutionRunTx(ctx, tx, owner, runID, runStatus, terminalReason, terminalAt, true); err != nil {
			return fmt.Errorf("finalize receipt run terminal transition: %w", err)
		}
	} else {
		if _, err = transitionExecutionRunTx(ctx, tx, owner, runID, coreexecution.RunUncertain, "provider_outcome_uncertain", terminalAt, false); err != nil {
			return fmt.Errorf("finalize receipt run uncertain transition: %w", err)
		}
	}
	return tx.Commit()
}

// materializeNewlyUnblockedStages is deliberately transaction-local.  The
// blocked row lock plus the status predicate in materializeExecutionStage make
// concurrent terminal receipts/restarts idempotent: only the first completion
// can create a child task or approval card.
func (s *DatabaseExecutionStore) materializeNewlyUnblockedStages(ctx context.Context, tx *sql.Tx, owner, runID string, at time.Time) error {
	var rawRun []byte
	if err := tx.QueryRowContext(ctx, `SELECT snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 AND status='running' FOR UPDATE`, owner, runID).Scan(&rawRun); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var run coreexecution.ExecutionRun
	if strictJSON(rawRun, &run) != nil || run.OwnerID != owner || run.RunID != runID || run.Revision == 0 || !run.PlanDigest.Valid() {
		return fmt.Errorf("DAG materialization run snapshot: %w", coreexecution.ErrConflict)
	}
	plan, err := s.GetPlanRevision(ctx, owner, run.PlanID, run.PlanRevision)
	if err != nil || plan.Digest != run.PlanDigest {
		return fmt.Errorf("DAG materialization plan revision: %w", coreexecution.ErrConflict)
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.snapshot_json FROM core_execution_run_stages s WHERE s.owner_id=$1 AND s.run_id=$2 AND s.status='blocked' AND NOT EXISTS (SELECT 1 FROM core_execution_run_stage_dependencies d JOIN core_execution_run_stages parent ON parent.owner_id=d.owner_id AND parent.run_id=d.run_id AND parent.stage_id=d.depends_on_stage_id WHERE d.owner_id=s.owner_id AND d.run_id=s.run_id AND d.stage_id=s.stage_id AND parent.status<>'succeeded') ORDER BY s.ordinal FOR UPDATE`, owner, runID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var unlocked []coreexecution.RunStage
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			return err
		}
		var stage coreexecution.RunStage
		if strictJSON(raw, &stage) != nil || stage.OwnerID != owner || stage.RunID != runID || stage.Status != coreexecution.StageBlocked {
			return fmt.Errorf("DAG materialization blocked stage snapshot: %w", coreexecution.ErrConflict)
		}
		unlocked = append(unlocked, stage)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, stage := range unlocked {
		// Every run-stage is pinned to the immutable run revision created with
		// the graph. The mutable run head advances when bootstrap is claimed;
		// using that head here would produce a child task/card whose revision
		// conflicts with the stage's FK/scope guard. Reconstruct only this
		// materialization input at the stage's durable binding revision.
		boundRun := run
		boundRun.Revision = stage.RunRevision
		if err := materializeExecutionStage(ctx, tx, s.now, plan, boundRun, stage, at); err != nil {
			return fmt.Errorf("DAG materialization child %s: %w", stage.StageID, err)
		}
	}
	return nil
}
