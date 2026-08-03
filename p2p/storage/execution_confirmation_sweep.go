package storage

// Durable expiry handling for execution/v2 approvals.  This intentionally
// does not use the retired generic/V1 confirmation sweeper: a V2 approval is
// part of a run DAG, so expiry must terminalize its task, stage graph and run
// in one transaction before another worker can dispatch it.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

const maxExecutionConfirmationSweepCandidates = 64

// SweepExpiredV2Confirmations closes up to limit expired pending/confirmed
// V2 confirmations. It is safe to call repeatedly and is intentionally a
// no-op for consumed work, whose provider outcome requires reconciliation.
func (s *DatabaseExecutionStore) SweepExpiredV2Confirmations(ctx context.Context, limit int) (int, error) {
	if s == nil || s.db == nil || limit < 0 || limit > maxExecutionConfirmationSweepCandidates {
		return 0, ErrExecutionStoreInvalid
	}
	if limit == 0 {
		limit = maxExecutionConfirmationSweepCandidates
	}
	closed := 0
	for closed < limit {
		ok, err := s.sweepOneExpiredV2Confirmation(ctx)
		if err != nil {
			return closed, err
		}
		if !ok {
			break
		}
		closed++
	}
	return closed, nil
}

func (s *DatabaseExecutionStore) sweepOneExpiredV2Confirmation(ctx context.Context) (bool, error) {
	at := s.now().UTC().Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var owner, confirmationID, runID string
	err = tx.QueryRowContext(ctx, `SELECT c.owner_id,c.confirmation_id::text,s.run_id::text FROM agent_confirmations c JOIN core_execution_run_stages s ON s.owner_id=c.owner_id AND s.confirmation_id=c.confirmation_id JOIN core_execution_runs r ON r.owner_id=s.owner_id AND r.run_id=s.run_id WHERE c.operation_domain LIKE 'execution:v2:%' AND c.state IN ('pending','confirmed') AND c.expires_at<=$1 AND r.status IN ('pending','waiting_user','queued') ORDER BY c.expires_at,c.confirmation_id FOR UPDATE OF c,s,r SKIP LOCKED LIMIT 1`, at).Scan(&owner, &confirmationID, &runID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var runRaw []byte
	var runRevision uint64
	var runStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status,revision,snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, owner, runID).Scan(&runStatus, &runRevision, &runRaw); err != nil {
		return false, err
	}
	var run coreexecution.ExecutionRun
	if strictJSON(runRaw, &run) != nil || run.OwnerID != owner || run.RunID != runID || run.Revision != runRevision || run.Status != coreexecution.RunStatus(runStatus) {
		return false, ErrExecutionStoreDrift
	}
	// Lock every graph node before changing any. A concurrent claim therefore
	// wins completely (and makes this transaction retry later) or loses
	// completely; expiry never leaves a partially live DAG.
	rows, err := tx.QueryContext(ctx, `SELECT stage_id::text,status,revision,snapshot_json FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, owner, runID)
	if err != nil {
		return false, err
	}
	type stageRow struct {
		id, status string
		revision   uint64
		raw        []byte
	}
	var stages []stageRow
	for rows.Next() {
		var v stageRow
		if err = rows.Scan(&v.id, &v.status, &v.revision, &v.raw); err != nil {
			rows.Close()
			return false, err
		}
		stages = append(stages, v)
	}
	if err = rows.Close(); err != nil {
		return false, err
	}
	for _, v := range stages {
		if v.status != string(coreexecution.StageBlocked) && v.status != string(coreexecution.StageWaitingUser) && v.status != string(coreexecution.StageQueued) {
			continue
		}
		var stage coreexecution.RunStage
		if strictJSON(v.raw, &stage) != nil || stage.StageID != v.id || stage.Status != coreexecution.StageStatus(v.status) {
			return false, ErrExecutionStoreDrift
		}
		stage.Status, stage.FinishedAt, stage.UpdatedAt = coreexecution.StageExpired, at, at
		res, e := tx.ExecContext(ctx, `UPDATE core_execution_run_stages SET status='expired',revision=revision+1,completed_at=$1,snapshot_json=$2,updated_at=$1 WHERE owner_id=$3 AND run_id=$4 AND stage_id=$5 AND revision=$6 AND status=$7`, at, mustJSON(stage), owner, runID, v.id, v.revision, v.status)
		if e != nil {
			return false, e
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return false, coreexecution.ErrConflict
		}
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='canceled',failure_code='confirmation_expired',failure_summary='confirmation_expired',lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$1 WHERE owner_id=$2 AND task_id IN (SELECT task_id FROM core_execution_run_stages WHERE owner_id=$2 AND run_id=$3 AND task_id IS NOT NULL) AND status IN ('waiting_user','queued')`, at, owner, runID); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='expired',terminal_reason='confirmation_expired',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND confirmation_id IN (SELECT confirmation_id FROM core_execution_run_stages WHERE owner_id=$2 AND run_id=$3 AND confirmation_id IS NOT NULL) AND state IN ('pending','confirmed')`, at, owner, runID); err != nil {
		return false, err
	}
	run.Status, run.TerminalReason, run.FinishedAt, run.UpdatedAt, run.Revision = coreexecution.RunExpired, "confirmation_expired", at, at, runRevision+1
	res, err := tx.ExecContext(ctx, `UPDATE core_execution_runs SET status='expired',terminal_reason='confirmation_expired',completed_at=$1,revision=$2,snapshot_json=$3,updated_at=$1 WHERE owner_id=$4 AND run_id=$5 AND revision=$6 AND status IN ('pending','waiting_user','queued')`, at, run.Revision, mustJSON(run), owner, runID, runRevision)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return false, coreexecution.ErrConflict
	}
	if err = insertExecutionRunRevision(ctx, tx, run); err != nil {
		return false, err
	}
	if err = insertExecutionEvent(ctx, tx, owner, runID, "confirmation_expired", "", coreexecution.StageExpired, at); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
