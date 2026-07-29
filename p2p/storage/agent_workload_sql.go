package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
)

// AppendWorkloadEventSQL allocates a sequence under a row lock. It never
// derives the cursor with MAX()+1, so concurrent writers cannot collide.
func (s *DatabaseStore) AppendWorkloadEventSQL(ctx context.Context, ownerID, operationID string, event workload.Event) (uint64, error) {
	if s == nil || s.db == nil || strings.TrimSpace(ownerID) == "" || strings.TrimSpace(operationID) == "" {
		return 0, errors.New("storage: invalid workload event")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var sequence uint64
	if err = tx.QueryRowContext(ctx, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,operation_id) DO UPDATE SET next_sequence=core_workload_event_counters.next_sequence+1 RETURNING next_sequence-1`, ownerID, operationID).Scan(&sequence); err != nil {
		return 0, err
	}
	readback, _ := json.Marshal(event.Readback)
	if err = insertWorkloadEventTx(ctx, tx, ownerID, operationID, sequence, event.Kind, string(event.Status), event.Message, readback, time.Now().UTC()); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return sequence, nil
}

func insertWorkloadEventTx(ctx context.Context, tx *sql.Tx, ownerID, operationID string, sequence uint64, kind, status, message string, readback []byte, at time.Time) error {
	var workloadID string
	if err := tx.QueryRowContext(ctx, `SELECT workload_id::text FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2`, ownerID, operationID).Scan(&workloadID); err != nil {
		return err
	}
	var publicSequence int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO p2p_agent_deployment_event_cursors(owner_id,workload_id,last_sequence,updated_at)
		VALUES($1,$2,1,$3)
		ON CONFLICT(owner_id,workload_id) DO UPDATE
		SET last_sequence=p2p_agent_deployment_event_cursors.last_sequence+1,updated_at=EXCLUDED.updated_at
		RETURNING last_sequence`, ownerID, workloadID, at.UTC()).Scan(&publicSequence); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO core_workload_events(owner_id,workload_id,operation_id,sequence,public_sequence,kind,status,message,readback_json,at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`, ownerID, workloadID, operationID, sequence, publicSequence, kind, status, message, readback, at.UTC())
	return err
}
