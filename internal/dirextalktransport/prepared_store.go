package dirextalktransport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const MaxPreparedMatrixEventBytes = 64 << 10

// PostgresPreparedMatrixMutationStore is the message-server implementation
// of PreparedMatrixMutationStore.  It intentionally lives next to the
// transport contract so Product Capability and the MCP modules share one
// narrow, dependency-free persistence boundary.
type PostgresPreparedMatrixMutationStore struct {
	db *sql.DB
}

func NewPostgresPreparedMatrixMutationStore(db *sql.DB) *PostgresPreparedMatrixMutationStore {
	if db == nil {
		return nil
	}
	return &PostgresPreparedMatrixMutationStore{db: db}
}

func (s *PostgresPreparedMatrixMutationStore) PrepareMatrixEvent(ctx context.Context, event PreparedMatrixEvent) error {
	if s == nil || s.db == nil {
		return errors.New("prepared Matrix event store is unavailable")
	}
	if err := validatePreparedMatrixEvent(event); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	// Serialize preparation with operation cancellation.  The capability
	// ledger row is created before the handler runs; locking it here lets
	// cancel() prove that no PDU can be inserted after it commits cancelled.
	var operationState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`, event.OperationID).Scan(&operationState); err != nil {
		return err
	}
	if operationState == "completed" || operationState == "failed" || operationState == "cancelled" {
		return fmt.Errorf("operation %s is already terminal", event.OperationID)
	}
	existing, err := scanPreparedMatrixEvent(tx.QueryRowContext(ctx, `SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,logical_id,post_id,state,attempt,revision,receipt_json,event_id,room_id,sender_mxid,event_type,message_type,origin_server_ts,room_version,event_pdu,pdu_sha256,content_digest FROM p2p_capability_matrix_prepared_events WHERE operation_id=$1 FOR UPDATE`, event.OperationID))
	if err == nil {
		if !preparedMatrixEventEqual(existing, &event) {
			return fmt.Errorf("prepared Matrix event conflicts with operation %q", event.OperationID)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if event.State == "" {
		event.State = "prepared"
	}
	if event.Attempt <= 0 {
		event.Attempt = 1
	}
	if event.Revision <= 0 {
		event.Revision = 1
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO p2p_capability_matrix_prepared_events(operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,logical_id,post_id,state,attempt,revision,receipt_json,event_id,room_id,sender_mxid,event_type,message_type,origin_server_ts,room_version,event_pdu,pdu_sha256,content_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`, event.OperationID, event.Capability, event.Operation, event.OwnerID, event.Generation, event.RootDigest, event.LogicalID, event.PostID, event.State, event.Attempt, event.Revision, emptyReceiptJSON(event.ReceiptJSON), event.Event.EventID, event.Event.RoomID, event.Event.SenderMXID, event.Event.EventType, event.Event.MessageType, event.Event.OriginServerTS, event.Event.RoomVersion, event.Event.EventJSON, sha256Digest(event.Event.EventJSON), event.Event.ContentDigest)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresPreparedMatrixMutationStore) HasMatrixPreparedEvent(ctx context.Context, operationID, ownerID string, generation int64, rootDigest []byte) (bool, error) {
	if s == nil || s.db == nil {
		return false, errors.New("prepared Matrix event store is unavailable")
	}
	if strings.TrimSpace(operationID) == "" || strings.TrimSpace(ownerID) == "" || generation <= 0 || len(rootDigest) != 32 {
		return false, errors.New("prepared Matrix event fence is incomplete")
	}
	var present bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM p2p_capability_matrix_prepared_events WHERE operation_id=$1 AND owner_id=$2 AND account_generation=$3 AND root_request_digest=$4)`, operationID, ownerID, generation, rootDigest).Scan(&present); err != nil {
		return false, err
	}
	return present, nil
}

// HasMatrixPreparedEventTx runs the owner-fenced presence probe on a caller's
// transaction. Operation cancellation uses this form while holding the
// capability-operation row lock, so a concurrent PrepareMatrixEvent cannot
// insert a staged PDU between the probe and the cancelled transition (and a
// one-connection pool cannot deadlock waiting for a second connection).
func (s *PostgresPreparedMatrixMutationStore) HasMatrixPreparedEventTx(ctx context.Context, tx *sql.Tx, operationID, ownerID string, generation int64, rootDigest []byte) (bool, error) {
	if s == nil || tx == nil {
		return false, errors.New("prepared Matrix event transaction is unavailable")
	}
	if strings.TrimSpace(operationID) == "" || strings.TrimSpace(ownerID) == "" || generation <= 0 || len(rootDigest) != 32 {
		return false, errors.New("prepared Matrix event fence is incomplete")
	}
	var present bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM p2p_capability_matrix_prepared_events WHERE operation_id=$1 AND owner_id=$2 AND account_generation=$3 AND root_request_digest=$4)`, operationID, ownerID, generation, rootDigest).Scan(&present); err != nil {
		return false, err
	}
	return present, nil
}

func (s *PostgresPreparedMatrixMutationStore) GetMatrixPreparedEvent(ctx context.Context, operationID, ownerID string, generation int64, rootDigest []byte) (*PreparedMatrixEvent, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("prepared Matrix event store is unavailable")
	}
	if strings.TrimSpace(operationID) == "" || strings.TrimSpace(ownerID) == "" || generation <= 0 || len(rootDigest) != 32 {
		return nil, errors.New("prepared Matrix event fence is incomplete")
	}
	event, err := scanPreparedMatrixEvent(s.db.QueryRowContext(ctx, `SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,logical_id,post_id,state,attempt,revision,receipt_json,event_id,room_id,sender_mxid,event_type,message_type,origin_server_ts,room_version,event_pdu,pdu_sha256,content_digest FROM p2p_capability_matrix_prepared_events WHERE operation_id=$1 AND owner_id=$2 AND account_generation=$3 AND root_request_digest=$4`, operationID, ownerID, generation, rootDigest))
	if err != nil {
		return nil, err
	}
	return event, nil
}

func (s *PostgresPreparedMatrixMutationStore) MarkMatrixPreparedEventSent(ctx context.Context, operationID, ownerID string, generation int64, rootDigest []byte, receipt SendMessageResult) error {
	if s == nil || s.db == nil {
		return errors.New("prepared Matrix event store is unavailable")
	}
	if strings.TrimSpace(operationID) == "" || strings.TrimSpace(ownerID) == "" || generation <= 0 || len(rootDigest) != 32 || strings.TrimSpace(receipt.EventID) == "" {
		return errors.New("prepared Matrix event receipt fence is incomplete")
	}
	receiptJSON, err := json.Marshal(receipt)
	if err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, `UPDATE p2p_capability_matrix_prepared_events SET state='sent',receipt_json=$1::jsonb,attempt=attempt+1,revision=revision+1,updated_at=NOW() WHERE operation_id=$2 AND owner_id=$3 AND account_generation=$4 AND root_request_digest=$5 AND event_id=$6`, string(receiptJSON), operationID, ownerID, generation, rootDigest, receipt.EventID)
	if err == nil {
		if rows, rowsErr := result.RowsAffected(); rowsErr != nil {
			err = rowsErr
		} else if rows == 0 {
			err = sql.ErrNoRows
		}
	}
	return err
}

func (s *PostgresPreparedMatrixMutationStore) DeleteMatrixPreparedEvent(ctx context.Context, operationID, ownerID string, generation int64, rootDigest []byte) error {
	if s == nil || s.db == nil {
		return errors.New("prepared Matrix event store is unavailable")
	}
	if strings.TrimSpace(operationID) == "" || strings.TrimSpace(ownerID) == "" || generation <= 0 || len(rootDigest) != 32 {
		return errors.New("prepared Matrix event fence is incomplete")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM p2p_capability_matrix_prepared_events WHERE operation_id=$1 AND owner_id=$2 AND account_generation=$3 AND root_request_digest=$4`, operationID, ownerID, generation, rootDigest)
	return err
}

func (s *PostgresPreparedMatrixMutationStore) DeleteMatrixPreparedEventByOperation(ctx context.Context, operationID string) error {
	if s == nil || s.db == nil {
		return errors.New("prepared Matrix event store is unavailable")
	}
	if strings.TrimSpace(operationID) == "" {
		return errors.New("operation id is required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM p2p_capability_matrix_prepared_events WHERE operation_id=$1`, operationID)
	return err
}

func scanPreparedMatrixEvent(row interface{ Scan(...any) error }) (*PreparedMatrixEvent, error) {
	var event PreparedMatrixEvent
	var rootDigest, receiptJSON, eventJSON, pduDigest, contentDigest []byte
	if err := row.Scan(&event.OperationID, &event.Capability, &event.Operation, &event.OwnerID, &event.Generation, &rootDigest, &event.LogicalID, &event.PostID, &event.State, &event.Attempt, &event.Revision, &receiptJSON, &event.Event.EventID, &event.Event.RoomID, &event.Event.SenderMXID, &event.Event.EventType, &event.Event.MessageType, &event.Event.OriginServerTS, &event.Event.RoomVersion, &eventJSON, &pduDigest, &contentDigest); err != nil {
		return nil, err
	}
	if !bytes.Equal(pduDigest, sha256Digest(eventJSON)) {
		return nil, errors.New("prepared Matrix PDU digest mismatch")
	}
	event.RootDigest = append([]byte(nil), rootDigest...)
	event.ReceiptJSON = append([]byte(nil), receiptJSON...)
	event.Event.EventJSON = append([]byte(nil), eventJSON...)
	event.Event.ContentDigest = append([]byte(nil), contentDigest...)
	if err := validatePreparedMatrixEvent(event); err != nil {
		return nil, err
	}
	return &event, nil
}

func emptyReceiptJSON(value []byte) string {
	if len(value) == 0 {
		return `{}`
	}
	return string(value)
}

func validatePreparedMatrixEvent(event PreparedMatrixEvent) error {
	if strings.TrimSpace(event.OperationID) == "" || strings.TrimSpace(event.Capability) == "" || strings.TrimSpace(event.Operation) == "" || strings.TrimSpace(event.OwnerID) == "" || event.Generation <= 0 || len(event.RootDigest) != 32 {
		return errors.New("prepared Matrix event identity fence is incomplete")
	}
	if strings.TrimSpace(event.Event.EventID) == "" || strings.TrimSpace(event.Event.RoomID) == "" || strings.TrimSpace(event.Event.SenderMXID) == "" || strings.TrimSpace(event.Event.EventType) == "" || strings.TrimSpace(event.Event.RoomVersion) == "" {
		return errors.New("prepared Matrix event metadata is incomplete")
	}
	if len(event.Event.EventJSON) == 0 || len(event.Event.EventJSON) > MaxPreparedMatrixEventBytes || !json.Valid(event.Event.EventJSON) || len(event.Event.ContentDigest) != 32 {
		return errors.New("prepared Matrix event JSON is invalid or exceeds the size limit")
	}
	return nil
}

func preparedMatrixEventEqual(a, b *PreparedMatrixEvent) bool {
	if a == nil || b == nil {
		return false
	}
	return a.OperationID == b.OperationID && a.Capability == b.Capability && a.Operation == b.Operation && a.OwnerID == b.OwnerID && a.Generation == b.Generation && a.LogicalID == b.LogicalID && a.PostID == b.PostID && bytes.Equal(a.RootDigest, b.RootDigest) && a.Event.EventID == b.Event.EventID && a.Event.RoomID == b.Event.RoomID && a.Event.SenderMXID == b.Event.SenderMXID && a.Event.EventType == b.Event.EventType && a.Event.MessageType == b.Event.MessageType && a.Event.OriginServerTS == b.Event.OriginServerTS && a.Event.RoomVersion == b.Event.RoomVersion && bytes.Equal(a.Event.EventJSON, b.Event.EventJSON) && bytes.Equal(a.Event.ContentDigest, b.Event.ContentDigest)
}

func sha256Digest(value []byte) []byte {
	var digest [32]byte
	// Keep the helper local to the transport store so callers cannot mutate a
	// shared hash buffer after validation.
	digest = sha256.Sum256(value)
	return append([]byte(nil), digest[:]...)
}
