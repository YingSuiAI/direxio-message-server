package storage

// Atomic failure handling for an execution.v2 step that was rejected before
// any provider dispatch intent was persisted. This path must never be used to
// resolve an accepted or ambiguous provider operation; those remain uncertain
// and poll/reconcile-only.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionrunner"
	"github.com/google/uuid"
)

const executionPreDispatchFailureEvent = "step_pre_dispatch_failed"

type ExecutionPreDispatchFailure struct {
	Claim          ExecutionStageLeaseClaim
	Step           ExecutionNextStep
	Code           string
	EvidenceDigest coreexecution.Digest
}

type executionPreDispatchFailurePayload struct {
	FailureClass   string               `json:"failure_class"`
	EvidenceDigest coreexecution.Digest `json:"evidence_digest"`
}

// FailExecutionStageBeforeDispatch records one failed attempt without a
// receipt/intent, terminalizes the claimed stage/task/run, and releases the
// exact target and task-concurrency fences in one transaction.
func (s *DatabaseExecutionStore) FailExecutionStageBeforeDispatch(ctx context.Context, in ExecutionPreDispatchFailure) error {
	if s == nil || s.db == nil {
		return fmt.Errorf("%w: pre-dispatch store", ErrExecutionStoreInvalid)
	}
	runnerFailure, err := executionrunner.NewPreDispatchFailure(
		executionrunner.StageLease{
			OwnerID: in.Claim.OwnerID, RunID: in.Claim.RunID, StageID: in.Claim.StageID,
			TaskID: in.Claim.TaskID, Holder: in.Claim.Holder, Attempt: in.Claim.Attempt,
			LeaseEpoch: in.Claim.LeaseEpoch, TaskLeaseEpoch: in.Claim.TaskLeaseEpoch,
			ExpectedTaskRevision: in.Claim.ExpectedTaskRevision, LeaseID: in.Claim.LeaseID,
			LeaseToken: in.Claim.LeaseToken, ExpiresAt: in.Claim.ExpiresAt,
		},
		executionrunner.NextStep{
			OwnerID: in.Step.OwnerID, RunID: in.Step.RunID, StageID: in.Step.StageID,
			StepKey: in.Step.StepKey, StepSet: in.Step.StepSet,
			StepRevision: in.Step.StepRevision, StepDigest: in.Step.StepDigest,
		},
		in.Code,
	)
	if err != nil || !in.EvidenceDigest.Valid() || in.EvidenceDigest != runnerFailure.EvidenceDigest {
		return fmt.Errorf("%w: pre-dispatch fence", ErrExecutionStoreInvalid)
	}

	attemptID := preDispatchAttemptID(in)
	eventKey := preDispatchEventKey(in)
	payload := executionPreDispatchFailurePayload{FailureClass: preDispatchFailureClass(in.Code), EvidenceDigest: in.EvidenceDigest}
	payloadRaw, err := canonicalRedactedJSON(payload)
	if err != nil {
		return fmt.Errorf("%w: pre-dispatch event payload", ErrExecutionStoreInvalid)
	}
	wantEventDigest, err := executionEventDigest(in.Claim.OwnerID, in.Claim.RunID, in.Claim.StageID, attemptID, in.Step.StepKey, executionPreDispatchFailureEvent, eventKey, "failed", payloadRaw)
	if err != nil {
		return fmt.Errorf("%w: pre-dispatch event digest", ErrExecutionStoreInvalid)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// A retried response after commit is an exact, read-only replay. Any change
	// to the task/lease/step/code fence produces a different evidence digest and
	// is rejected instead of rewriting terminal evidence.
	var existingAttempt, existingStep, existingKind, existingStatus, existingDigest string
	var existingPayload []byte
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(attempt_id::text,''),COALESCE(step_key,''),kind,status,event_digest,event_json FROM core_execution_events WHERE owner_id=$1 AND run_id=$2 AND event_key=$3 FOR UPDATE`, in.Claim.OwnerID, in.Claim.RunID, eventKey).Scan(&existingAttempt, &existingStep, &existingKind, &existingStatus, &existingDigest, &existingPayload)
	if err == nil {
		canonicalExisting, canonicalErr := canonicalJSONBytes(existingPayload)
		if canonicalErr != nil || existingAttempt != attemptID || existingStep != in.Step.StepKey || existingKind != executionPreDispatchFailureEvent || existingStatus != "failed" || existingDigest != string(wantEventDigest) || !jsonEqual(canonicalExisting, payloadRaw) {
			return ErrExecutionStoreDrift
		}
		if err := verifyPreDispatchFailureReplay(ctx, tx, in, attemptID); err != nil {
			return err
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	now := s.now().UTC().Truncate(time.Microsecond)
	var projectID, planID, planDigest, stageKey, stageDigest, targetID, targetDigest string
	var planRevision, stageRevision, targetRevision uint64
	var stageStatus, taskStatus, taskHolder, leaseID, leaseToken, leaseStatus string
	var taskAttempt, taskLeaseEpoch, taskRevision, targetLeaseEpoch uint64
	var stageRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT s.project_id::text,s.plan_id::text,s.plan_revision,s.plan_digest,s.plan_stage_key,s.stage_revision,s.plan_stage_digest,s.target_id::text,s.target_revision,s.target_digest,s.status,s.snapshot_json,t.status,t.lease_holder,t.attempt,t.lease_epoch,t.revision,l.lease_id::text,l.token::text,l.epoch,l.status FROM core_execution_run_stages s JOIN agent_tasks t ON t.owner_id=s.owner_id AND t.task_id=s.task_id JOIN core_execution_target_mutation_leases l ON l.owner_id=s.owner_id AND l.target_id=s.target_id AND l.target_revision=s.target_revision WHERE s.owner_id=$1 AND s.run_id=$2 AND s.stage_id=$3 AND s.task_id=$4 FOR UPDATE OF s,t,l`, in.Claim.OwnerID, in.Claim.RunID, in.Claim.StageID, in.Claim.TaskID).Scan(
		&projectID, &planID, &planRevision, &planDigest, &stageKey, &stageRevision, &stageDigest,
		&targetID, &targetRevision, &targetDigest, &stageStatus, &stageRaw,
		&taskStatus, &taskHolder, &taskAttempt, &taskLeaseEpoch, &taskRevision,
		&leaseID, &leaseToken, &targetLeaseEpoch, &leaseStatus,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return coreexecution.ErrConflict
	}
	if err != nil {
		return err
	}
	if stageStatus != string(coreexecution.StageRunning) || taskStatus != "running" || taskHolder != in.Claim.Holder || taskAttempt != uint64(in.Claim.Attempt) || taskLeaseEpoch != in.Claim.TaskLeaseEpoch || taskRevision != in.Claim.ExpectedTaskRevision || leaseID != in.Claim.LeaseID || leaseToken != in.Claim.LeaseToken || targetLeaseEpoch != in.Claim.LeaseEpoch || leaseStatus != "active" {
		return coreexecution.ErrConflict
	}
	if targetID == "" || targetRevision == 0 || !coreexecution.ValidateDigest(targetDigest) || !coreexecution.ValidateDigest(planDigest) || !coreexecution.ValidateDigest(stageDigest) {
		return ErrExecutionStoreDrift
	}

	var exactStep bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM core_execution_plan_steps WHERE owner_id=$1 AND plan_id=$2 AND plan_revision=$3 AND stage_key=$4 AND step_set=$5 AND step_key=$6 AND step_revision=$7 AND step_digest=$8 AND status='ready')`, in.Claim.OwnerID, planID, planRevision, stageKey, in.Step.StepSet, in.Step.StepKey, in.Step.StepRevision, in.Step.StepDigest).Scan(&exactStep); err != nil {
		return err
	}
	if !exactStep {
		return coreexecution.ErrConflict
	}

	// A dispatch intent or an existing attempt for this exact task attempt is a
	// hard boundary. Even if its command identifier is absent, it may not be
	// converted into a deterministic pre-dispatch failure.
	var providerEvidence, existingAttemptRow bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM core_execution_dispatch_intents WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 AND step_set=$4 AND step_key=$5 AND attempt_no=$6),EXISTS (SELECT 1 FROM core_execution_step_attempts WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 AND step_set=$4 AND step_key=$5 AND attempt_no=$6)`, in.Claim.OwnerID, in.Claim.RunID, in.Claim.StageID, in.Step.StepSet, in.Step.StepKey, in.Claim.Attempt).Scan(&providerEvidence, &existingAttemptRow); err != nil {
		return err
	}
	if providerEvidence || existingAttemptRow {
		return coreexecution.ErrConflict
	}

	var stage coreexecution.RunStage
	if strictJSON(stageRaw, &stage) != nil || stage.OwnerID != in.Claim.OwnerID || stage.RunID != in.Claim.RunID || stage.StageID != in.Claim.StageID || stage.PlanID != planID || stage.PlanRevision != planRevision || stage.StageKey != stageKey || stage.StageRevision != stageRevision || stage.StageDigest != coreexecution.Digest(stageDigest) || stage.TargetID != targetID || stage.TargetRevision != targetRevision || stage.TargetDigest != coreexecution.Digest(targetDigest) || stage.TaskID != in.Claim.TaskID || stage.Status != coreexecution.StageRunning || stage.StartedAt.IsZero() {
		return ErrExecutionStoreDrift
	}
	stage.Status = coreexecution.StageFailed
	stage.FinishedAt = now
	stage.UpdatedAt = now
	if stage.Validate() != nil {
		return ErrExecutionStoreDrift
	}
	failedStageRaw, err := json.Marshal(stage)
	if err != nil {
		return err
	}

	attempt := coreexecution.StepAttempt{
		AttemptID: attemptID, RunID: in.Claim.RunID, StageID: in.Claim.StageID,
		PlanID: planID, PlanRevision: planRevision, PlanDigest: coreexecution.Digest(planDigest),
		StageRevision: stageRevision, StageDigest: coreexecution.Digest(stageDigest),
		StepRevision: in.Step.StepRevision, StepDigest: in.Step.StepDigest, StepKey: in.Step.StepKey,
		Attempt: uint64(in.Claim.Attempt), OwnerID: in.Claim.OwnerID, Revision: 1,
		Status: coreexecution.AttemptFailed, StartedAt: stage.StartedAt, FinishedAt: now,
		CreatedAt: stage.StartedAt, UpdatedAt: now,
	}
	if attempt.Validate() != nil {
		return ErrExecutionStoreDrift
	}
	attemptRaw, err := json.Marshal(attempt)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO core_execution_step_attempts(owner_id,attempt_id,run_id,stage_id,project_id,plan_id,plan_revision,plan_stage_key,step_key,step_set,step_revision,step_digest,attempt_no,revision,status,schema_version,input_digest,output_digest,snapshot_json,started_at,completed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,1,'failed','execution-step-attempt/v2',$12,$14,$15,$16,$17)`, in.Claim.OwnerID, attemptID, in.Claim.RunID, in.Claim.StageID, projectID, planID, planRevision, stageKey, in.Step.StepKey, in.Step.StepSet, in.Step.StepRevision, in.Step.StepDigest, in.Claim.Attempt, in.EvidenceDigest, attemptRaw, stage.StartedAt, now)
	if err != nil {
		return mapExecutionConflict(err)
	}
	if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}

	summary := preDispatchFailureSummary(in.Code)
	result, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='failed',failure_code=$1,failure_summary=$2,lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$3 WHERE owner_id=$4 AND task_id=$5 AND status='running' AND lease_holder=$6 AND attempt=$7 AND lease_epoch=$8 AND revision=$9`, in.Code, summary, now, in.Claim.OwnerID, in.Claim.TaskID, in.Claim.Holder, in.Claim.Attempt, in.Claim.TaskLeaseEpoch, in.Claim.ExpectedTaskRevision)
	if err != nil {
		return err
	}
	if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=running_count-1,revision=revision+1,updated_at=$1 WHERE singleton=true AND running_count>0`, now)
	if err != nil {
		return err
	}
	if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET status='released',expires_at=NULL,revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND target_id=$3 AND target_revision=$4 AND lease_id=$5 AND token=$6 AND epoch=$7 AND run_id=$8 AND stage_id=$9 AND status='active'`, now, in.Claim.OwnerID, targetID, targetRevision, in.Claim.LeaseID, in.Claim.LeaseToken, in.Claim.LeaseEpoch, in.Claim.RunID, in.Claim.StageID)
	if err != nil {
		return err
	}
	if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE core_execution_run_stages SET status='failed',revision=revision+1,completed_at=$1,updated_at=$1,snapshot_json=$2 WHERE owner_id=$3 AND run_id=$4 AND stage_id=$5 AND status='running'`, now, failedStageRaw, in.Claim.OwnerID, in.Claim.RunID, in.Claim.StageID)
	if err != nil {
		return err
	}
	if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}

	var runRaw []byte
	var runStatus string
	var runRevision uint64
	if err = tx.QueryRowContext(ctx, `SELECT status,revision,snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, in.Claim.OwnerID, in.Claim.RunID).Scan(&runStatus, &runRevision, &runRaw); err != nil {
		return err
	}
	var run coreexecution.ExecutionRun
	if strictJSON(runRaw, &run) != nil || run.OwnerID != in.Claim.OwnerID || run.RunID != in.Claim.RunID || run.Status != coreexecution.RunRunning || runStatus != string(coreexecution.RunRunning) || run.Revision != runRevision || run.CurrentStageID != in.Claim.StageID {
		return ErrExecutionStoreDrift
	}
	run.Status = coreexecution.RunFailed
	run.TerminalReason = in.Code
	run.FinishedAt = now
	run.UpdatedAt = now
	run.Revision++
	if run.Validate() != nil {
		return ErrExecutionStoreDrift
	}
	failedRunRaw, err := json.Marshal(run)
	if err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `UPDATE core_execution_runs SET status='failed',terminal_reason=$1,revision=$2,completed_at=$3,updated_at=$3,snapshot_json=$4 WHERE owner_id=$5 AND run_id=$6 AND status='running' AND revision=$7`, in.Code, run.Revision, now, failedRunRaw, in.Claim.OwnerID, in.Claim.RunID, runRevision)
	if err != nil {
		return err
	}
	if n, rowsErr := result.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	if err = insertExecutionRunRevision(ctx, tx, run); err != nil {
		return err
	}
	if err = insertExecutionPreDispatchFailureEvent(ctx, tx, in, attemptID, eventKey, wantEventDigest, payloadRaw, now); err != nil {
		return err
	}
	return tx.Commit()
}

func verifyPreDispatchFailureReplay(ctx context.Context, tx *sql.Tx, in ExecutionPreDispatchFailure, attemptID string) error {
	var attemptStatus, outputDigest, stageStatus, taskStatus, taskCode, taskSummary, taskHolder, leaseStatus, runStatus, terminalReason string
	var taskExpiry sql.NullTime
	err := tx.QueryRowContext(ctx, `SELECT a.status,COALESCE(a.output_digest,''),s.status,t.status,t.failure_code,t.failure_summary,t.lease_holder,t.lease_expires_at,l.status,r.status,r.terminal_reason FROM core_execution_step_attempts a JOIN core_execution_run_stages s ON s.owner_id=a.owner_id AND s.run_id=a.run_id AND s.stage_id=a.stage_id JOIN agent_tasks t ON t.owner_id=s.owner_id AND t.task_id=s.task_id JOIN core_execution_target_mutation_leases l ON l.owner_id=s.owner_id AND l.run_id=s.run_id AND l.stage_id=s.stage_id JOIN core_execution_runs r ON r.owner_id=s.owner_id AND r.run_id=s.run_id WHERE a.owner_id=$1 AND a.run_id=$2 AND a.attempt_id=$3 AND a.stage_id=$4 AND a.step_set=$5 AND a.step_key=$6 AND a.attempt_no=$7`, in.Claim.OwnerID, in.Claim.RunID, attemptID, in.Claim.StageID, in.Step.StepSet, in.Step.StepKey, in.Claim.Attempt).Scan(&attemptStatus, &outputDigest, &stageStatus, &taskStatus, &taskCode, &taskSummary, &taskHolder, &taskExpiry, &leaseStatus, &runStatus, &terminalReason)
	if err != nil {
		return ErrExecutionStoreDrift
	}
	if attemptStatus != "failed" || outputDigest != string(in.EvidenceDigest) || stageStatus != "failed" || taskStatus != "failed" || taskCode != in.Code || taskSummary != preDispatchFailureSummary(in.Code) || taskHolder != "" || taskExpiry.Valid || leaseStatus != "released" || runStatus != "failed" || terminalReason != in.Code {
		return ErrExecutionStoreDrift
	}
	return nil
}

func insertExecutionPreDispatchFailureEvent(ctx context.Context, tx *sql.Tx, in ExecutionPreDispatchFailure, attemptID, eventKey string, eventDigest coreexecution.Digest, payload []byte, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO core_execution_event_counters(owner_id,run_id,next_sequence) VALUES($1,$2,1) ON CONFLICT DO NOTHING`, in.Claim.OwnerID, in.Claim.RunID); err != nil {
		return err
	}
	var sequence uint64
	if err := tx.QueryRowContext(ctx, `UPDATE core_execution_event_counters SET next_sequence=next_sequence+1 WHERE owner_id=$1 AND run_id=$2 RETURNING next_sequence-1`, in.Claim.OwnerID, in.Claim.RunID).Scan(&sequence); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO core_execution_events(owner_id,run_id,event_id,sequence,stage_id,attempt_id,step_key,kind,event_key,event_digest,status,event_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'failed',$11,$12)`, in.Claim.OwnerID, in.Claim.RunID, uuid.NewString(), sequence, in.Claim.StageID, attemptID, in.Step.StepKey, executionPreDispatchFailureEvent, eventKey, eventDigest, payload, at)
	return err
}

func preDispatchAttemptID(in ExecutionPreDispatchFailure) string {
	name := strings.Join([]string{"execution-pre-dispatch-attempt/v2", in.Claim.OwnerID, in.Claim.RunID, in.Claim.StageID, string(in.Step.StepSet), in.Step.StepKey, strconv.FormatUint(uint64(in.Claim.Attempt), 10)}, "\x00")
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(name)).String()
}

func preDispatchEventKey(in ExecutionPreDispatchFailure) string {
	return fmt.Sprintf("%s:%s:%s:%s:%d", executionPreDispatchFailureEvent, in.Claim.StageID, in.Step.StepSet, in.Step.StepKey, in.Claim.Attempt)
}

func preDispatchFailureSummary(code string) string {
	switch code {
	case executionrunner.FailureStepResolution:
		return "execution step could not be resolved before provider dispatch"
	case executionrunner.FailurePreparedStep:
		return "execution step failed immutable validation before provider dispatch"
	case executionrunner.FailureExecutor:
		return "required execution provider was unavailable before dispatch"
	default:
		return "execution step failed before provider dispatch"
	}
}

func preDispatchFailureClass(code string) string {
	switch code {
	case executionrunner.FailureStepResolution:
		return "resolver"
	case executionrunner.FailurePreparedStep:
		return "validation"
	case executionrunner.FailureExecutor:
		return "executor"
	default:
		return "rejected"
	}
}
