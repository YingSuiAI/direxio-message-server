package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	ext "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

// PostgresExtensionExecutionFinalizer closes the extension execution fence in
// one transaction.  It is deliberately separate from the lifecycle creator:
// the provider call happens before this method and is never retried here.
type PostgresExtensionExecutionFinalizer struct{ DB *DatabaseStore }

func (f *PostgresExtensionExecutionFinalizer) FinalizeLifecycle(ctx context.Context, r ext.ExecutionFinalizeRequest) error {
	if r.Operation == "" {
		return ext.ErrInvalid
	}
	return f.FinalizeExecution(ctx, r)
}

func (f *PostgresExtensionExecutionFinalizer) FinalizeExecution(ctx context.Context, r ext.ExecutionFinalizeRequest) error {
	if f == nil || f.DB == nil || f.DB.db == nil || r.OwnerID == "" || r.TaskID == "" || r.ConfirmationID == "" || r.InstallationID == "" || r.VersionID == "" || r.RequestDigest == "" || r.LeaseHolder == "" || r.Attempt == 0 || r.LeaseEpoch == 0 || r.TaskRevision == 0 || r.InstallationRevision == 0 {
		return ext.ErrInvalid
	}
	if r.Success && r.Uncertain {
		return ext.ErrInvalid
	}
	if !r.Success && r.ErrorCode == "" {
		return ext.ErrInvalid
	}
	at := time.Now().UTC()
	return f.DB.writer.Do(f.DB.db, nil, func(tx *sql.Tx) error {
		// Lock and validate the immutable installation projection.  Execute
		// does not change its metadata, but this exact revision/version check
		// is the CAS that prevents a stale worker from terminalizing a newer
		// installation.
		var revision int64
		var active, state string
		if err := tx.QueryRowContext(ctx, `SELECT revision,active_version_id,state FROM p2p_agent_extensions WHERE owner_id=$1 AND installation_id=$2 FOR UPDATE`, r.OwnerID, r.InstallationID).Scan(&revision, &active, &state); err != nil {
			if err == sql.ErrNoRows {
				return ext.ErrNotFound
			}
			return err
		}
		if uint64(revision) != r.InstallationRevision {
			return ext.ErrRevisionConflict
		}
		if r.Operation == "" && (active != r.VersionID || state == "removed") {
			return ext.ErrRevisionConflict
		}
		if r.Operation != "" {
			nextState, nextActive := "installed", r.VersionID
			if r.Operation == "uninstall" {
				nextState, nextActive = "removed", ""
			} else if r.Operation != "install" && r.Operation != "update" {
				return ext.ErrInvalid
			}
			result, err := tx.ExecContext(ctx, `UPDATE p2p_agent_extensions SET state=$1,active_version_id=$2,revision=revision+1,updated_at=$3 WHERE owner_id=$4 AND installation_id=$5 AND revision=$6`, nextState, nextActive, at, r.OwnerID, r.InstallationID, revision)
			if err != nil {
				return err
			}
			if n, _ := result.RowsAffected(); n != 1 {
				return ext.ErrRevisionConflict
			}
		} else {
			result, err := tx.ExecContext(ctx, `UPDATE p2p_agent_extensions SET updated_at=$1 WHERE owner_id=$2 AND installation_id=$3 AND revision=$4 AND active_version_id=$5`, at, r.OwnerID, r.InstallationID, revision, r.VersionID)
			if err != nil {
				return err
			}
			if n, _ := result.RowsAffected(); n != 1 {
				return ext.ErrRevisionConflict
			}
		}
		var receiptDigest, receiptStatus string
		var receiptErr error
		if r.Operation != "" {
			receiptErr = sql.ErrNoRows
		} else {
			receiptErr = tx.QueryRowContext(ctx, `SELECT request_digest,status FROM p2p_agent_extension_execution_receipts WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, r.OwnerID, r.TaskID).Scan(&receiptDigest, &receiptStatus)
		}
		if receiptErr == nil {
			if receiptDigest != r.RequestDigest {
				return ext.ErrIdempotencyConflict
			}
			if receiptStatus == "succeeded" || receiptStatus == "uncertain" || receiptStatus == "failed" {
				return ext.ErrAlreadyFinalized
			}
		} else if receiptErr != sql.ErrNoRows {
			return receiptErr
		}

		// The confirmation was consumed in its own fenced transaction before
		// the provider call.  Verify that reservation exactly, then release it
		// only as part of this terminal commit.
		var cstate string
		var crevision int64
		var reservation []byte
		var ctask string
		if err := tx.QueryRowContext(ctx, `SELECT state,revision,task_id::text,reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2 FOR UPDATE`, r.OwnerID, r.ConfirmationID).Scan(&cstate, &crevision, &ctask, &reservation); err != nil {
			if err == sql.ErrNoRows {
				return ext.ErrNotFound
			}
			return err
		}
		var reserved struct {
			TaskID   string `json:"task_id"`
			Attempt  uint32 `json:"attempt"`
			Epoch    uint64 `json:"lease_epoch"`
			Revision int64  `json:"task_revision"`
		}
		if cstate != "consumed" || ctask != r.TaskID || json.Unmarshal(reservation, &reserved) != nil || reserved.TaskID != r.TaskID || reserved.Attempt != r.Attempt {
			return ext.ErrConflict
		}
		if r.ReconcileConsumed {
			if reserved.Epoch >= r.LeaseEpoch || reserved.Revision >= int64(r.TaskRevision) {
				return ext.ErrConflict
			}
		} else if reserved.Epoch != r.LeaseEpoch || reserved.Revision != int64(r.TaskRevision) {
			return ext.ErrConflict
		}

		var taskStatus string
		var taskAttempt int
		var taskEpoch, taskRevision int64
		var holder string
		var leaseExpires *time.Time
		if err := tx.QueryRowContext(ctx, `SELECT status,attempt,lease_epoch,revision,lease_holder,lease_expires_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, r.OwnerID, r.TaskID).Scan(&taskStatus, &taskAttempt, &taskEpoch, &taskRevision, &holder, &leaseExpires); err != nil {
			if err == sql.ErrNoRows {
				return ext.ErrNotFound
			}
			return err
		}
		if taskStatus != "running" || taskAttempt != int(r.Attempt) || uint64(taskEpoch) != r.LeaseEpoch || uint64(taskRevision) < r.TaskRevision || holder != r.LeaseHolder || leaseExpires == nil || !leaseExpires.After(at) {
			return ext.ErrRevisionConflict
		}
		terminalStatus, eventType := "succeeded", "extension_execution_succeeded"
		if !r.Success {
			terminalStatus, eventType = "failed", "extension_execution_failed"
			if r.Uncertain {
				eventType = "extension_execution_uncertain"
			}
		}
		resultJSON := []byte(`{}`)
		if r.Success {
			resultJSON, _ = json.Marshal(map[string]string{"result_digest": r.ResultDigest})
		}
		var sequence int64
		if err := tx.QueryRowContext(ctx, `UPDATE agent_tasks SET status=$1,failure_code=$2,failure_summary=$3,result_json=$4::jsonb,lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$5 WHERE owner_id=$6 AND task_id=$7 AND status='running' AND attempt=$8 AND lease_epoch=$9 AND lease_holder=$10 AND revision=$11 RETURNING progress_sequence`, terminalStatus, r.ErrorCode, r.ErrorSummary, string(resultJSON), at, r.OwnerID, r.TaskID, r.Attempt, r.LeaseEpoch, r.LeaseHolder, taskRevision).Scan(&sequence); err != nil {
			return ext.ErrRevisionConflict
		}
		if _, err := tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true`, at); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) VALUES($1,$2,$3,$4,$5,$6::jsonb,$7)`, r.OwnerID, r.TaskID, sequence, eventType, terminalStatus, string(resultJSON), at); err != nil {
			return err
		}
		if result, err := tx.ExecContext(ctx, `UPDATE agent_confirmations SET reservation_json=NULL,revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND confirmation_id=$3 AND state='consumed' AND revision=$4`, at, r.OwnerID, r.ConfirmationID, crevision); err != nil {
			return err
		} else if n, _ := result.RowsAffected(); n != 1 {
			return ext.ErrRevisionConflict
		}
		uncertain := r.Uncertain
		status := terminalStatus
		if uncertain {
			status = "uncertain"
		}
		if r.Operation != "" {
			return nil
		}
		var receiptWriteErr error
		if receiptErr == sql.ErrNoRows && r.Operation == "" {
			_, receiptWriteErr = tx.ExecContext(ctx, `INSERT INTO p2p_agent_extension_execution_receipts(owner_id,task_id,installation_id,version_id,request_digest,status,result_digest,error_code,uncertain,attempt,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$11)`, r.OwnerID, r.TaskID, r.InstallationID, r.VersionID, r.RequestDigest, status, r.ResultDigest, r.ErrorCode, uncertain, r.Attempt, at)
		} else {
			_, receiptWriteErr = tx.ExecContext(ctx, `UPDATE p2p_agent_extension_execution_receipts SET status=$3,result_digest=$4,error_code=$5,uncertain=$6,attempt=$7,updated_at=$8 WHERE owner_id=$1 AND task_id=$2`, r.OwnerID, r.TaskID, status, r.ResultDigest, r.ErrorCode, uncertain, r.Attempt, at)
		}
		return receiptWriteErr
	})
}

var _ ext.ExecutionFinalizer = (*PostgresExtensionExecutionFinalizer)(nil)
var _ ext.LifecycleFinalizer = (*PostgresExtensionExecutionFinalizer)(nil)
