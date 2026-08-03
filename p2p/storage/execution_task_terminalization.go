package storage

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

// terminalizeExecutionTaskTx closes the task fence which originated an
// execution stage. The task row is locked before deciding whether the
// concurrency slot must be released, so uncertain/reconciled retries cannot
// decrement the global count twice.
func terminalizeExecutionTaskTx(ctx context.Context, tx *sql.Tx, owner, taskID, status, failureCode, failureSummary string, at time.Time) error {
	if tx == nil || strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(taskID) {
		return ErrExecutionStoreInvalid
	}
	if status != "succeeded" && status != "failed" && status != "canceled" {
		return ErrExecutionStoreInvalid
	}
	at = at.UTC().Truncate(time.Microsecond)
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, owner, taskID).Scan(&current); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return coreexecution.ErrConflict
		}
		return err
	}
	if current == status {
		return nil
	}
	if current != "running" && current != "queued" && current != "waiting_user" && current != "failed" {
		return coreexecution.ErrConflict
	}
	if current == "failed" && status == "failed" {
		return nil
	}
	if status == "succeeded" {
		failureCode, failureSummary = "", ""
	} else {
		if strings.TrimSpace(failureCode) == "" {
			failureCode = status
		}
		if strings.TrimSpace(failureSummary) == "" {
			failureSummary = failureCode
		}
	}
	result, err := tx.ExecContext(ctx, `UPDATE agent_tasks SET status=$1,failure_code=$2,failure_summary=$3,lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$4 WHERE owner_id=$5 AND task_id=$6 AND status=$7`, status, failureCode, failureSummary, at, owner, taskID, current)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	if current != "running" {
		return nil
	}
	result, err = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=running_count-1,revision=revision+1,updated_at=$1 WHERE singleton=true AND running_count>0`, at)
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
