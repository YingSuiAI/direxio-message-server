package productcapability

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	"google.golang.org/protobuf/encoding/protojson"
)

type operationRecord struct {
	ID         string
	Capability string
	Operation  string
	OwnerID    string
	Generation int64
	// RootDigest is the grant-independent business request fence. It is the
	// idempotency key for StartOperation retries; a refreshed root grant must
	// not turn an otherwise identical retry into a conflict.
	RootDigest []byte
	// Digest is the final request digest (which includes the opaque grant
	// digest) retained for audit/debugging. It is immutable after insertion and
	// is deliberately not used as the replay fence.
	Digest    []byte
	State     capv1.OperationState
	Result    []byte
	Err       *capv1.CapabilityError
	Revision  int64
	UpdatedAt time.Time
	Created   bool
}

type operationEvent struct {
	OperationID string
	Sequence    int64
	Event       *capv1.WatchOperationEvent
}

type operationStore struct {
	db       *sql.DB
	prepared dirextalktransport.PreparedMatrixMutationStore
	mu       sync.RWMutex
	records  map[string]*operationRecord
	events   map[string][]operationEvent
	watchers map[string][]chan operationEvent
	cancels  map[string]context.CancelFunc
}

func newOperationStore(db *sql.DB) *operationStore {
	return &operationStore{db: db, records: make(map[string]*operationRecord), events: make(map[string][]operationEvent), watchers: make(map[string][]chan operationEvent), cancels: make(map[string]context.CancelFunc)}
}

func (s *operationStore) setPreparedMatrixStore(store dirextalktransport.PreparedMatrixMutationStore) {
	if s == nil {
		return
	}
	s.prepared = store
}

func (s *operationStore) registerCancel(operationID string, cancel context.CancelFunc) {
	if s == nil || strings.TrimSpace(operationID) == "" || cancel == nil {
		return
	}
	s.mu.Lock()
	s.cancels[operationID] = cancel
	s.mu.Unlock()
}

func (s *operationStore) clearCancel(operationID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	delete(s.cancels, operationID)
	s.mu.Unlock()
}

func (s *operationStore) cancelExecution(operationID string) {
	if s == nil {
		return
	}
	s.mu.RLock()
	cancel := s.cancels[operationID]
	s.mu.RUnlock()
	if cancel != nil {
		cancel()
	}
}

func (s *operationStore) ensureSchema(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	// The capability operation ledger is part of the database migration set,
	// not an ad-hoc startup mutation. Fail closed when a deployment forgot the
	// fresh-schema migration instead of silently creating a partial schema.
	for _, table := range []string{"p2p_capability_operations", "p2p_capability_operation_events", "p2p_capability_matrix_prepared_events"} {
		var present bool
		if err := s.db.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&present); err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("required capability operation table %s is missing; run the fresh database migrations", table)
		}
	}
	// A process restart cannot safely resume an operation whose handler may
	// have committed an external side effect. Fence each in-flight row under a
	// lock and persist the uncertain progress event in the same transaction;
	// otherwise WatchOperation would replay only `accepted` forever.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.QueryContext(ctx, `SELECT operation_id FROM p2p_capability_operations WHERE state IN ('pending','running') FOR UPDATE`)
	if err != nil {
		return err
	}
	operationIDs := make([]string, 0)
	for rows.Next() {
		var operationID string
		if err := rows.Scan(&operationID); err != nil {
			_ = rows.Close()
			return err
		}
		operationIDs = append(operationIDs, operationID)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	uncertainEvents := make(map[string]*capv1.WatchOperationEvent, len(operationIDs))
	for _, operationID := range operationIDs {
		if _, err := tx.ExecContext(ctx, `UPDATE p2p_capability_operations SET state='uncertain',error_code='SERVICE_RESTARTED',error_message='operation was in flight when message-server restarted',revision=revision+1,updated_at=NOW() WHERE operation_id=$1 AND state IN ('pending','running')`, operationID); err != nil {
			return err
		}
		event := uncertainEvent(&operationRecord{ID: operationID, State: capv1.OperationState_OPERATION_STATE_UNCERTAIN, Err: capabilityError(capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED, "operation was in flight when message-server restarted")})
		if err := insertOperationEventTx(ctx, tx, operationID, event); err != nil {
			return err
		}
		uncertainEvents[operationID] = event
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for _, operationID := range operationIDs {
		// The event was inserted in the transaction above; caching after commit
		// only accelerates in-process watchers and never becomes the source of
		// truth for a restarted process.
		s.cacheEvent(operationID, uncertainEvents[operationID])
	}
	return nil
}

func (s *operationStore) start(ctx context.Context, record *operationRecord) (*operationRecord, error) {
	if record == nil || record.ID == "" || (len(record.RootDigest) == 0 && len(record.Digest) == 0) {
		return nil, errors.New("operation id and request digest are required")
	}
	if s.db == nil {
		s.mu.Lock()
		if existing := s.records[record.ID]; existing != nil {
			if !operationMatches(existing, record) {
				s.mu.Unlock()
				return nil, errOperationConflict
			}
			copy := cloneOperation(existing)
			copy.Created = false
			s.mu.Unlock()
			return copy, nil
		}
		record = cloneOperation(record)
		record.Created = true
		record.State = capv1.OperationState_OPERATION_STATE_PENDING
		record.Revision = 1
		record.UpdatedAt = time.Now().UTC()
		s.records[record.ID] = record
		s.mu.Unlock()
		return cloneOperation(record), s.emit(ctx, record.ID, acceptedEvent(record))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Lock an existing row before comparing the replay fence. The lock keeps a
	// concurrent retry from observing a half-written terminal state and makes
	// the idempotent read part of the same transaction as a new insert.
	existing, err := scanOperationRow(tx.QueryRowContext(ctx, `SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,request_digest,state,COALESCE(result_json::text,''),COALESCE(error_code,''),COALESCE(error_message,''),revision,updated_at FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`, record.ID))
	if err == nil {
		existing.Created = false
		if !operationMatches(existing, record) {
			return nil, errOperationConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	record.State = capv1.OperationState_OPERATION_STATE_PENDING
	record.Revision = 1
	record.UpdatedAt = time.Now().UTC()
	execResult, err := tx.ExecContext(ctx, `INSERT INTO p2p_capability_operations(operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,request_digest,state,revision,updated_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10) ON CONFLICT(operation_id) DO NOTHING`, record.ID, record.Capability, record.Operation, record.OwnerID, record.Generation, record.RootDigest, record.Digest, operationStateString(record.State), record.Revision, record.UpdatedAt)
	if err != nil {
		return nil, err
	}
	rowsAffected, rowsErr := execResult.RowsAffected()
	if rowsErr != nil {
		return nil, rowsErr
	}
	if rowsAffected == 0 {
		// The concurrent inserter won after our initial SELECT. Read it while
		// holding the row lock and apply the exact same replay fence.
		existing, err = scanOperationRow(tx.QueryRowContext(ctx, `SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,request_digest,state,COALESCE(result_json::text,''),COALESCE(error_code,''),COALESCE(error_message,''),revision,updated_at FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`, record.ID))
		if err != nil {
			return nil, err
		}
		existing.Created = false
		if !operationMatches(existing, record) {
			return nil, errOperationConflict
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return existing, nil
	}

	// Insert the accepted event in the same transaction as the operation row.
	// A crash cannot leave a pending operation without its first durable event.
	record.Created = true
	accepted := acceptedEvent(record)
	if err := insertOperationEventTx(ctx, tx, record.ID, accepted); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	s.cacheEvent(record.ID, accepted)
	return cloneOperation(record), nil
}

func (s *operationStore) get(ctx context.Context, operationID string) (*operationRecord, error) {
	if s.db == nil {
		s.mu.RLock()
		record := cloneOperation(s.records[operationID])
		s.mu.RUnlock()
		if record == nil {
			return nil, sql.ErrNoRows
		}
		return record, nil
	}
	var record operationRecord
	var state string
	var result, errCode, errMessage []byte
	err := s.db.QueryRowContext(ctx, `SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,request_digest,state,COALESCE(result_json::text,''),COALESCE(error_code,''),COALESCE(error_message,''),revision,updated_at FROM p2p_capability_operations WHERE operation_id=$1`, operationID).Scan(&record.ID, &record.Capability, &record.Operation, &record.OwnerID, &record.Generation, &record.RootDigest, &record.Digest, &state, &result, &errCode, &errMessage, &record.Revision, &record.UpdatedAt)
	if err != nil {
		return nil, err
	}
	record.State, record.Result = operationStateFromString(state), result
	if len(errCode) > 0 {
		record.Err = &capv1.CapabilityError{Code: capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED, Message: string(errMessage)}
	}
	return &record, nil
}

// claimReplay atomically takes ownership of one restart-fenced operation.
// Only an uncertain record can be resumed; a live running handler is left
// alone so concurrent StartOperation retries cannot duplicate a Matrix write.
func (s *operationStore) claimReplay(ctx context.Context, operationID string) (bool, error) {
	if s == nil || strings.TrimSpace(operationID) == "" {
		return false, errors.New("operation id is required")
	}
	if s.db == nil {
		s.mu.Lock()
		defer s.mu.Unlock()
		record := s.records[operationID]
		if record == nil || record.State != capv1.OperationState_OPERATION_STATE_UNCERTAIN {
			return false, nil
		}
		record.State = capv1.OperationState_OPERATION_STATE_RUNNING
		record.Revision++
		record.UpdatedAt = time.Now().UTC()
		return true, nil
	}
	result, err := s.db.ExecContext(ctx, `UPDATE p2p_capability_operations SET state='running',revision=revision+1,updated_at=NOW() WHERE operation_id=$1 AND state='uncertain'`, operationID)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows > 0, err
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanOperationRow(row rowScanner) (*operationRecord, error) {
	var record operationRecord
	var state string
	var result, errCode, errMessage []byte
	if err := row.Scan(&record.ID, &record.Capability, &record.Operation, &record.OwnerID, &record.Generation, &record.RootDigest, &record.Digest, &state, &result, &errCode, &errMessage, &record.Revision, &record.UpdatedAt); err != nil {
		return nil, err
	}
	record.State, record.Result = operationStateFromString(state), result
	if len(errCode) > 0 {
		record.Err = &capv1.CapabilityError{Code: capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED, Message: string(errMessage)}
	}
	return &record, nil
}

func (s *operationStore) finish(ctx context.Context, operationID string, result []byte, capabilityErr *capv1.CapabilityError) error {
	state := capv1.OperationState_OPERATION_STATE_COMPLETED
	if capabilityErr != nil {
		state = capv1.OperationState_OPERATION_STATE_FAILED
	}
	if s.db == nil {
		s.mu.Lock()
		record := s.records[operationID]
		if record == nil {
			s.mu.Unlock()
			return sql.ErrNoRows
		}
		if operationStateTerminal(record.State) {
			s.mu.Unlock()
			return nil
		}
		record.State, record.Result, record.Err = state, append([]byte(nil), result...), capabilityErr
		record.Revision++
		record.UpdatedAt = time.Now().UTC()
		event := terminalEvent(record)
		s.mu.Unlock()
		if s.prepared != nil {
			// In-memory operation ledgers are used by local/test deployments too;
			// terminal transitions must not leave the durable Matrix PDU staged.
			_ = s.prepared.DeleteMatrixPreparedEventByOperation(ctx, operationID)
		}
		return s.emit(ctx, operationID, event)
	}
	errCode, errMessage := "", ""
	if capabilityErr != nil {
		errCode, errMessage = capabilityErr.Code.String(), capabilityErr.Message
	}
	// The state transition and terminal event share one transaction. A crash
	// cannot leave a completed ledger row whose Watch stream has no terminal
	// event (or vice versa).
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var currentState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`, operationID).Scan(&currentState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if operationStateTerminal(operationStateFromString(currentState)) {
		if err := tx.Commit(); err != nil {
			return err
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE p2p_capability_operations SET state=$1,result_json=CASE WHEN $2='' THEN NULL ELSE $2::jsonb END,error_code=$3,error_message=$4,revision=revision+1,updated_at=NOW() WHERE operation_id=$5 AND state IN ('pending','running','uncertain')`, operationStateString(state), string(result), errCode, errMessage, operationID); err != nil {
		return err
	}
	event := terminalEvent(&operationRecord{ID: operationID, State: state, Result: result, Err: capabilityErr})
	if err := insertOperationEventTx(ctx, tx, operationID, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.prepared != nil {
		// Terminal business receipts retain event_id in result_json while the
		// full signed PDU is no longer needed.  A cleanup failure is safe to
		// retry and must not hide the already committed terminal operation.
		_ = s.prepared.DeleteMatrixPreparedEventByOperation(ctx, operationID)
	}
	s.cacheEvent(operationID, event)
	return nil
}

// markUncertain preserves a non-terminal operation when a prepared Matrix PDU
// exists but the roomserver cannot prove whether it was accepted. The staged
// PDU remains available for an explicit reconcile/retry; converting this case
// to failed would discard the only safe duplicate-prevention fence.
func (s *operationStore) markUncertain(ctx context.Context, operationID string, capabilityErr *capv1.CapabilityError) error {
	if s == nil || strings.TrimSpace(operationID) == "" {
		return sql.ErrNoRows
	}
	if capabilityErr == nil {
		capabilityErr = capabilityError(capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED, "Matrix event acceptance is unknown")
	}
	if s.db == nil {
		s.mu.Lock()
		record := s.records[operationID]
		if record == nil {
			s.mu.Unlock()
			return sql.ErrNoRows
		}
		if operationStateTerminal(record.State) {
			s.mu.Unlock()
			return nil
		}
		record.State = capv1.OperationState_OPERATION_STATE_UNCERTAIN
		record.Err = capabilityErr
		record.Revision++
		record.UpdatedAt = time.Now().UTC()
		event := uncertainEvent(record)
		s.mu.Unlock()
		return s.emit(ctx, operationID, event)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var currentState string
	if err := tx.QueryRowContext(ctx, `SELECT state FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`, operationID).Scan(&currentState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return err
	}
	if operationStateTerminal(operationStateFromString(currentState)) {
		return tx.Commit()
	}
	errCode, errMessage := capabilityErr.Code.String(), capabilityErr.Message
	if _, err := tx.ExecContext(ctx, `UPDATE p2p_capability_operations SET state='uncertain',error_code=$1,error_message=$2,revision=revision+1,updated_at=NOW() WHERE operation_id=$3 AND state IN ('pending','running','uncertain')`, errCode, errMessage, operationID); err != nil {
		return err
	}
	event := uncertainEvent(&operationRecord{ID: operationID, State: capv1.OperationState_OPERATION_STATE_UNCERTAIN, Err: capabilityErr})
	if err := insertOperationEventTx(ctx, tx, operationID, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.cacheEvent(operationID, event)
	return nil
}

func (s *operationStore) cancel(ctx context.Context, operationID string) error {
	if s.db == nil {
		s.mu.Lock()
		record := s.records[operationID]
		if record == nil {
			s.mu.Unlock()
			return sql.ErrNoRows
		}
		if operationStateTerminal(record.State) {
			s.mu.Unlock()
			return sql.ErrNoRows
		}
		if record.State == capv1.OperationState_OPERATION_STATE_UNCERTAIN {
			s.mu.Unlock()
			return sql.ErrNoRows
		}
		// The in-memory prepared store cannot share the PostgreSQL operation-row
		// lock with this ledger. Its conservative presence result therefore
		// rejects Matrix cancellation whenever a staged PDU may exist.
		prepared, preparedErr := s.preparedMatrixPresent(ctx, nil, record)
		if preparedErr != nil {
			s.mu.Unlock()
			return preparedErr
		}
		if prepared {
			s.mu.Unlock()
			return errOperationPrepared
		}
		record.State = capv1.OperationState_OPERATION_STATE_CANCELLED
		record.Revision++
		record.UpdatedAt = time.Now().UTC()
		s.mu.Unlock()
		s.cancelExecution(operationID)
		if s.prepared != nil {
			_ = s.prepared.DeleteMatrixPreparedEventByOperation(ctx, operationID)
		}
		return s.emit(ctx, operationID, &capv1.WatchOperationEvent{OperationId: operationID, Sequence: 0, TimestampUnixMs: time.Now().UnixMilli(), Event: &capv1.WatchOperationEvent_Cancelled{Cancelled: &capv1.CancelledEvent{Reason: "cancelled by caller"}}})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var record operationRecord
	var currentState string
	if err := tx.QueryRowContext(ctx, `SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,state FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`, operationID).Scan(&record.ID, &record.Capability, &record.Operation, &record.OwnerID, &record.Generation, &record.RootDigest, &currentState); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return err
	}
	if operationStateTerminal(operationStateFromString(currentState)) || currentState == "uncertain" {
		if err := tx.Commit(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	record.State = operationStateFromString(currentState)
	prepared, preparedErr := s.preparedMatrixPresent(ctx, tx, &record)
	if preparedErr != nil {
		return preparedErr
	}
	if prepared {
		return errOperationPrepared
	}
	if _, err := tx.ExecContext(ctx, `UPDATE p2p_capability_operations SET state='cancelled',revision=revision+1,updated_at=NOW() WHERE operation_id=$1 AND state IN ('pending','running')`, operationID); err != nil {
		return err
	}
	event := &capv1.WatchOperationEvent{OperationId: operationID, TimestampUnixMs: time.Now().UnixMilli(), Event: &capv1.WatchOperationEvent_Cancelled{Cancelled: &capv1.CancelledEvent{Reason: "cancelled by caller"}}}
	if err := insertOperationEventTx(ctx, tx, operationID, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.cancelExecution(operationID)
	if s.prepared != nil {
		_ = s.prepared.DeleteMatrixPreparedEventByOperation(ctx, operationID)
	}
	s.cacheEvent(operationID, event)
	return nil
}

func (s *operationStore) watch(ctx context.Context, operationID string, after int64) (<-chan operationEvent, error) {
	if _, err := s.get(ctx, operationID); err != nil {
		return nil, err
	}
	durable := []operationEvent{}
	if s.db != nil {
		events, err := s.loadEvents(ctx, operationID, after)
		if err != nil {
			return nil, err
		}
		durable = events
	}
	s.mu.Lock()
	initial := append([]operationEvent(nil), durable...)
	seen := make(map[int64]struct{}, len(initial))
	for _, event := range initial {
		seen[event.Sequence] = struct{}{}
	}
	for _, event := range s.events[operationID] {
		if event.Sequence > after {
			if _, exists := seen[event.Sequence]; !exists {
				initial = append(initial, event)
				seen[event.Sequence] = struct{}{}
			}
		}
	}
	out := make(chan operationEvent, len(initial)+16)
	for _, event := range initial {
		out <- event
	}
	s.watchers[operationID] = append(s.watchers[operationID], out)
	s.mu.Unlock()
	go func() {
		<-ctx.Done()
		s.mu.Lock()
		watchers := s.watchers[operationID]
		for index, watcher := range watchers {
			if watcher == out {
				s.watchers[operationID] = append(watchers[:index], watchers[index+1:]...)
				break
			}
		}
		close(out)
		s.mu.Unlock()
	}()
	return out, nil
}

func (s *operationStore) eventCount(operationID string) int64 {
	if s.db != nil {
		var count int64
		if err := s.db.QueryRow(`SELECT COALESCE(MAX(sequence),0) FROM p2p_capability_operation_events WHERE operation_id=$1`, operationID).Scan(&count); err == nil {
			return count
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return int64(len(s.events[operationID]))
}

func (s *operationStore) emit(ctx context.Context, operationID string, event *capv1.WatchOperationEvent) error {
	if event == nil {
		return nil
	}
	if s.db != nil {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		var exists string
		if err := tx.QueryRowContext(ctx, `SELECT operation_id FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`, operationID).Scan(&exists); err != nil {
			return err
		}
		if err := insertOperationEventTx(ctx, tx, operationID, event); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	s.cacheEvent(operationID, event)
	return nil
}

// insertOperationEventTx appends one event while the caller owns the
// operation-row transaction. The row lock serializes sequence allocation with
// concurrent finish/cancel calls and lets StartOperation commit its accepted
// event atomically with the operation insert.
func insertOperationEventTx(ctx context.Context, tx *sql.Tx, operationID string, event *capv1.WatchOperationEvent) error {
	var exists string
	if err := tx.QueryRowContext(ctx, `SELECT operation_id FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`, operationID).Scan(&exists); err != nil {
		return err
	}
	var sequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM p2p_capability_operation_events WHERE operation_id=$1`, operationID).Scan(&sequence); err != nil {
		return err
	}
	event.OperationId, event.Sequence = operationID, sequence
	if event.TimestampUnixMs == 0 {
		event.TimestampUnixMs = time.Now().UnixMilli()
	}
	raw, err := protojson.Marshal(event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO p2p_capability_operation_events(operation_id,sequence,event_json) VALUES($1,$2,$3::jsonb)`, operationID, sequence, string(raw))
	return err
}

func (s *operationStore) cacheEvent(operationID string, event *capv1.WatchOperationEvent) {
	if event == nil {
		return
	}
	s.mu.Lock()
	if event.OperationId == "" {
		event.OperationId = operationID
	}
	if event.Sequence == 0 {
		event.Sequence = int64(len(s.events[operationID]) + 1)
	}
	if event.TimestampUnixMs == 0 {
		event.TimestampUnixMs = time.Now().UnixMilli()
	}
	entry := operationEvent{OperationID: operationID, Sequence: event.Sequence, Event: event}
	s.events[operationID] = append(s.events[operationID], entry)
	watchers := append([]chan operationEvent(nil), s.watchers[operationID]...)
	for _, watcher := range watchers {
		select {
		case watcher <- entry:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *operationStore) loadEvents(ctx context.Context, operationID string, after int64) ([]operationEvent, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sequence,event_json FROM p2p_capability_operation_events WHERE operation_id=$1 AND sequence>$2 ORDER BY sequence`, operationID, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []operationEvent
	for rows.Next() {
		var sequence int64
		var raw []byte
		if err := rows.Scan(&sequence, &raw); err != nil {
			return nil, err
		}
		event := &capv1.WatchOperationEvent{}
		if err := protojson.Unmarshal(raw, event); err != nil {
			return nil, err
		}
		if event.OperationId == "" {
			event.OperationId = operationID
		}
		if event.Sequence == 0 {
			event.Sequence = sequence
		}
		events = append(events, operationEvent{OperationID: operationID, Sequence: sequence, Event: event})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

func acceptedEvent(record *operationRecord) *capv1.WatchOperationEvent {
	return &capv1.WatchOperationEvent{Event: &capv1.WatchOperationEvent_Accepted{Accepted: &capv1.AcceptedEvent{State: capv1.OperationState_OPERATION_STATE_PENDING}}}
}

func uncertainEvent(record *operationRecord) *capv1.WatchOperationEvent {
	payload := map[string]any{"state": "uncertain"}
	if record != nil && record.Err != nil {
		payload["error_code"] = record.Err.Code.String()
		payload["error_message"] = record.Err.Message
	}
	eventJSON, _ := json.Marshal(payload)
	return &capv1.WatchOperationEvent{OperationId: recordID(record), TimestampUnixMs: time.Now().UnixMilli(), Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: eventJSON}}}
}

func recordID(record *operationRecord) string {
	if record == nil {
		return ""
	}
	return record.ID
}

func terminalEvent(record *operationRecord) *capv1.WatchOperationEvent {
	if record != nil && record.Err != nil {
		return &capv1.WatchOperationEvent{Event: &capv1.WatchOperationEvent_Error{Error: &capv1.ErrorEvent{Error: record.Err}}}
	}
	return &capv1.WatchOperationEvent{Event: &capv1.WatchOperationEvent_Result{Result: &capv1.ResultEvent{ResultJson: append([]byte(nil), record.Result...)}}}
}

var errOperationConflict = errors.New("operation id already exists with a different request or owner")
var errOperationPrepared = errors.New("operation has a prepared Matrix mutation")

type preparedMatrixMutationPresenceTxStore interface {
	HasMatrixPreparedEventTx(context.Context, *sql.Tx, string, string, int64, []byte) (bool, error)
}

func (s *operationStore) preparedMatrixPresent(ctx context.Context, tx *sql.Tx, record *operationRecord) (bool, error) {
	if s == nil || s.prepared == nil || record == nil || !isPreparedMatrixMutation(record.Capability, record.Operation) {
		return false, nil
	}
	if s.db == nil {
		// An in-memory operation ledger cannot share the PostgreSQL operation-row
		// lock with a prepared-event writer. A point-in-time presence probe would
		// therefore still race a prepare that commits immediately afterward;
		// reject Matrix cancellation conservatively instead of claiming that the
		// staged-PDU fence is clear.
		return true, nil
	}
	if txStore, ok := s.prepared.(preparedMatrixMutationPresenceTxStore); ok && tx != nil {
		return txStore.HasMatrixPreparedEventTx(ctx, tx, record.ID, record.OwnerID, record.Generation, record.RootDigest)
	}
	presence, ok := s.prepared.(dirextalktransport.PreparedMatrixMutationPresenceStore)
	if !ok {
		// Without an owner-fenced presence probe, fail closed rather than race a
		// handler that may prepare a PDU immediately after this check.
		return true, nil
	}
	return presence.HasMatrixPreparedEvent(ctx, record.ID, record.OwnerID, record.Generation, record.RootDigest)
}

// operationMatches is the replay fence for an operation_id. An idempotent
// retry must carry the exact capability/operation, owner generation and
// canonical request digest that created the original ledger row. Checking
// only the digest would allow an accidental cross-capability replay when two
// descriptors happen to canonicalize the same business input.
func operationMatches(existing, requested *operationRecord) bool {
	if existing == nil || requested == nil {
		return false
	}
	rootDigest := existing.RootDigest
	requestedRootDigest := requested.RootDigest
	// Keep the in-memory store and older package-local fixtures useful when a
	// caller only supplies the historical final digest. Production records
	// always persist RootDigest and therefore take the grant-independent path.
	if len(rootDigest) == 0 {
		rootDigest = existing.Digest
	}
	if len(requestedRootDigest) == 0 {
		requestedRootDigest = requested.Digest
	}
	return existing.Capability == requested.Capability &&
		existing.Operation == requested.Operation &&
		existing.OwnerID == requested.OwnerID &&
		existing.Generation == requested.Generation &&
		equalBytes(rootDigest, requestedRootDigest)
}

func operationStateString(state capv1.OperationState) string {
	return strings.ToLower(strings.TrimPrefix(state.String(), "OPERATION_STATE_"))
}
func operationStateFromString(state string) capv1.OperationState {
	switch strings.ToLower(state) {
	case "pending":
		return capv1.OperationState_OPERATION_STATE_PENDING
	case "running":
		return capv1.OperationState_OPERATION_STATE_RUNNING
	case "completed":
		return capv1.OperationState_OPERATION_STATE_COMPLETED
	case "failed":
		return capv1.OperationState_OPERATION_STATE_FAILED
	case "cancelled":
		return capv1.OperationState_OPERATION_STATE_CANCELLED
	case "uncertain":
		return capv1.OperationState_OPERATION_STATE_UNCERTAIN
	}
	return capv1.OperationState_OPERATION_STATE_UNSPECIFIED
}
func operationStateTerminal(state capv1.OperationState) bool {
	switch state {
	case capv1.OperationState_OPERATION_STATE_COMPLETED,
		capv1.OperationState_OPERATION_STATE_FAILED,
		capv1.OperationState_OPERATION_STATE_CANCELLED:
		return true
	default:
		return false
	}
}
func equalBytes(left, right []byte) bool { return string(left) == string(right) }
func cloneOperation(record *operationRecord) *operationRecord {
	if record == nil {
		return nil
	}
	copy := *record
	copy.RootDigest, copy.Digest, copy.Result = append([]byte(nil), record.RootDigest...), append([]byte(nil), record.Digest...), append([]byte(nil), record.Result...)
	return &copy
}
