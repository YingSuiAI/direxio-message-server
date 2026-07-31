package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

// transitionRunningExecutionStageTx keeps the queryable row and its strict
// JSON snapshot in lockstep. The row revision is a persistence CAS counter;
// RunStage.StageRevision remains the immutable plan-stage revision.
func transitionRunningExecutionStageTx(
	ctx context.Context,
	tx *sql.Tx,
	owner string,
	runID string,
	stageID string,
	status coreexecution.StageStatus,
	at time.Time,
) error {
	if tx == nil ||
		(status != coreexecution.StageSucceeded &&
			status != coreexecution.StageFailed &&
			status != coreexecution.StageUncertain) {
		return ErrExecutionStoreInvalid
	}
	at = at.UTC().Truncate(time.Microsecond)
	var rowStatus string
	var rowRevision uint64
	var raw []byte
	if err := tx.QueryRowContext(
		ctx,
		`SELECT status,revision,snapshot_json FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 FOR UPDATE`,
		owner,
		runID,
		stageID,
	).Scan(&rowStatus, &rowRevision, &raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return coreexecution.ErrNotFound
		}
		return err
	}
	var stage coreexecution.RunStage
	if rowStatus != string(coreexecution.StageRunning) ||
		strictJSON(raw, &stage) != nil ||
		stage.OwnerID != owner ||
		stage.RunID != runID ||
		stage.StageID != stageID ||
		stage.Status != coreexecution.StageRunning {
		return ErrExecutionStoreDrift
	}
	stage.Status = status
	stage.FinishedAt = at
	stage.UpdatedAt = at
	if err := stage.Validate(); err != nil {
		return ErrExecutionStoreDrift
	}
	snapshot, err := json.Marshal(stage)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE core_execution_run_stages SET status=$1,revision=$2,completed_at=$3,updated_at=$3,snapshot_json=$4 WHERE owner_id=$5 AND run_id=$6 AND stage_id=$7 AND status='running' AND revision=$8`,
		status,
		rowRevision+1,
		at,
		snapshot,
		owner,
		runID,
		stageID,
		rowRevision,
	)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	return nil
}

// transitionExecutionRunTx updates the authoritative run snapshot and writes
// its immutable revision record in the same transaction. When requireSettled
// is true, an active stage means the run remains running and no update occurs.
func transitionExecutionRunTx(
	ctx context.Context,
	tx *sql.Tx,
	owner string,
	runID string,
	status coreexecution.RunStatus,
	terminalReason string,
	at time.Time,
	requireSettled bool,
) (bool, error) {
	if tx == nil ||
		(status != coreexecution.RunSucceeded &&
			status != coreexecution.RunFailed &&
			status != coreexecution.RunUncertain) {
		return false, ErrExecutionStoreInvalid
	}
	at = at.UTC().Truncate(time.Microsecond)
	var rowStatus string
	var rowRevision uint64
	var raw []byte
	if err := tx.QueryRowContext(
		ctx,
		`SELECT status,revision,snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`,
		owner,
		runID,
	).Scan(&rowStatus, &rowRevision, &raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, coreexecution.ErrNotFound
		}
		return false, err
	}
	if rowStatus != string(coreexecution.RunRunning) && rowStatus != string(coreexecution.RunQueued) {
		return false, coreexecution.ErrConflict
	}
	if requireSettled {
		var active, hasFailed, hasUncertain bool
		if err := tx.QueryRowContext(
			ctx,
			`SELECT
				EXISTS (SELECT 1 FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND status IN ('running','queued','blocked','waiting_user')),
				EXISTS (SELECT 1 FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND status='failed'),
				EXISTS (SELECT 1 FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND status='uncertain')`,
			owner,
			runID,
		).Scan(&active, &hasFailed, &hasUncertain); err != nil {
			return false, err
		}
		if active {
			return false, nil
		}
		switch {
		case hasUncertain:
			status = coreexecution.RunUncertain
			terminalReason = "stage_outcome_uncertain"
		case hasFailed:
			status = coreexecution.RunFailed
			terminalReason = "stage_failed"
		default:
			status = coreexecution.RunSucceeded
			terminalReason = ""
		}
	}
	var run coreexecution.ExecutionRun
	if strictJSON(raw, &run) != nil ||
		run.OwnerID != owner ||
		run.RunID != runID ||
		run.Status != coreexecution.RunStatus(rowStatus) ||
		run.Revision != rowRevision {
		return false, ErrExecutionStoreDrift
	}
	run.Status = status
	run.TerminalReason = terminalReason
	run.Revision++
	run.UpdatedAt = at
	if !run.StartedAt.IsZero() {
		run.FinishedAt = at
	}
	if err := run.Validate(); err != nil {
		return false, ErrExecutionStoreDrift
	}
	snapshot, err := json.Marshal(run)
	if err != nil {
		return false, err
	}
	var completedAt any
	if !run.FinishedAt.IsZero() {
		completedAt = run.FinishedAt
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE core_execution_runs SET status=$1,terminal_reason=$2,revision=$3,completed_at=$4,updated_at=$5,snapshot_json=$6 WHERE owner_id=$7 AND run_id=$8 AND status=$9 AND revision=$10`,
		run.Status,
		run.TerminalReason,
		run.Revision,
		completedAt,
		run.UpdatedAt,
		snapshot,
		owner,
		runID,
		rowStatus,
		rowRevision,
	)
	if err != nil {
		return false, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return false, rowsErr
		}
		return false, coreexecution.ErrConflict
	}
	if err := insertExecutionRunRevision(ctx, tx, run); err != nil {
		return false, err
	}
	return true, nil
}

// skipBlockedExecutionDescendantsTx closes dependency branches which can no
// longer become executable because at least one parent did not succeed.
// Blocked stages have never started, so their strict snapshot keeps both
// StartedAt and FinishedAt empty while UpdatedAt advances.
func skipBlockedExecutionDescendantsTx(
	ctx context.Context,
	tx *sql.Tx,
	owner string,
	runID string,
	at time.Time,
) error {
	at = at.UTC().Truncate(time.Microsecond)
	for {
		rows, err := tx.QueryContext(
			ctx,
			`SELECT child.stage_id::text,child.revision,child.snapshot_json
			 FROM core_execution_run_stages child
			 WHERE child.owner_id=$1 AND child.run_id=$2 AND child.status='blocked'
			   AND EXISTS (
			     SELECT 1
			     FROM core_execution_run_stage_dependencies dependency
			     JOIN core_execution_run_stages parent
			       ON parent.owner_id=dependency.owner_id
			      AND parent.run_id=dependency.run_id
			      AND parent.stage_id=dependency.depends_on_stage_id
			     WHERE dependency.owner_id=child.owner_id
			       AND dependency.run_id=child.run_id
			       AND dependency.stage_id=child.stage_id
			       AND parent.status IN ('failed','uncertain','skipped','canceled','rejected','expired')
			   )
			 ORDER BY child.ordinal
			 FOR UPDATE OF child`,
			owner,
			runID,
		)
		if err != nil {
			return err
		}
		type blockedStage struct {
			id       string
			revision uint64
			raw      []byte
		}
		var blocked []blockedStage
		for rows.Next() {
			var item blockedStage
			if err := rows.Scan(&item.id, &item.revision, &item.raw); err != nil {
				rows.Close()
				return err
			}
			blocked = append(blocked, item)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return err
		}
		rows.Close()
		if len(blocked) == 0 {
			return nil
		}
		for _, item := range blocked {
			var stage coreexecution.RunStage
			if strictJSON(item.raw, &stage) != nil ||
				stage.OwnerID != owner ||
				stage.RunID != runID ||
				stage.StageID != item.id ||
				stage.Status != coreexecution.StageBlocked {
				return ErrExecutionStoreDrift
			}
			stage.Status = coreexecution.StageSkipped
			stage.UpdatedAt = at
			if err := stage.Validate(); err != nil {
				return ErrExecutionStoreDrift
			}
			snapshot, err := json.Marshal(stage)
			if err != nil {
				return err
			}
			result, err := tx.ExecContext(
				ctx,
				`UPDATE core_execution_run_stages SET status='skipped',revision=$1,completed_at=NULL,updated_at=$2,snapshot_json=$3 WHERE owner_id=$4 AND run_id=$5 AND stage_id=$6 AND status='blocked' AND revision=$7`,
				item.revision+1,
				at,
				snapshot,
				owner,
				runID,
				item.id,
				item.revision,
			)
			if err != nil {
				return err
			}
			if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
				if rowsErr != nil {
					return rowsErr
				}
				return coreexecution.ErrConflict
			}
		}
	}
}
