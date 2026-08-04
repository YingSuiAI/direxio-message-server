package productcapability

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalktransport"
	"google.golang.org/protobuf/encoding/protojson"
)

func operationTestRecord() *operationRecord {
	return &operationRecord{
		ID: "operation-1", Capability: "product.messages.v1", Operation: "send",
		OwnerID: "owner-1", Generation: 1, RootDigest: make([]byte, 32), Digest: make([]byte, 32),
	}
}

type cancellationPreparedStore struct {
	present bool
	event   *dirextalktransport.PreparedMatrixEvent
}

func (s *cancellationPreparedStore) PrepareMatrixEvent(_ context.Context, event dirextalktransport.PreparedMatrixEvent) error {
	s.present = true
	copy := event
	copy.RootDigest = append([]byte(nil), event.RootDigest...)
	copy.Event.EventJSON = append([]byte(nil), event.Event.EventJSON...)
	copy.Event.ContentDigest = append([]byte(nil), event.Event.ContentDigest...)
	s.event = &copy
	return nil
}
func (s *cancellationPreparedStore) GetMatrixPreparedEvent(context.Context, string, string, int64, []byte) (*dirextalktransport.PreparedMatrixEvent, error) {
	if !s.present {
		return nil, sql.ErrNoRows
	}
	return s.event, nil
}
func (s *cancellationPreparedStore) DeleteMatrixPreparedEvent(context.Context, string, string, int64, []byte) error {
	s.present = false
	return nil
}
func (s *cancellationPreparedStore) DeleteMatrixPreparedEventByOperation(context.Context, string) error {
	s.present = false
	return nil
}
func (s *cancellationPreparedStore) HasMatrixPreparedEvent(context.Context, string, string, int64, []byte) (bool, error) {
	return s.present, nil
}

func TestMemoryOperationStoreReplayFenceAndTerminalCAS(t *testing.T) {
	store := newOperationStore(nil)
	ctx := context.Background()
	record := operationTestRecord()

	created, err := store.start(ctx, record)
	if err != nil || created == nil || !created.Created {
		t.Fatalf("first start created=%#v err=%v", created, err)
	}
	if got := store.eventCount(record.ID); got != 1 {
		t.Fatalf("accepted event count=%d, want 1", got)
	}

	replayed, err := store.start(ctx, operationTestRecord())
	if err != nil || replayed == nil || replayed.Created {
		t.Fatalf("same replay=%#v err=%v", replayed, err)
	}
	for name, mutate := range map[string]func(*operationRecord){
		"capability": func(r *operationRecord) { r.Capability = "product.contacts.v1" },
		"operation":  func(r *operationRecord) { r.Operation = "list" },
		"owner":      func(r *operationRecord) { r.OwnerID = "owner-2" },
		"generation": func(r *operationRecord) { r.Generation = 2 },
		"digest":     func(r *operationRecord) { r.RootDigest[0] = 1 },
	} {
		candidate := operationTestRecord()
		mutate(candidate)
		if _, err := store.start(ctx, candidate); err != errOperationConflict {
			t.Fatalf("%s replay err=%v, want conflict", name, err)
		}
	}

	if err := store.finish(ctx, record.ID, []byte(`{"ok":true}`), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	finished, err := store.get(ctx, record.ID)
	if err != nil || finished.State != capv1.OperationState_OPERATION_STATE_COMPLETED {
		t.Fatalf("finished=%#v err=%v", finished, err)
	}
	if err := store.finish(ctx, record.ID, []byte(`{"changed":true}`), nil); err != nil {
		t.Fatalf("terminal finish must be a no-op: %v", err)
	}
	if err := store.cancel(ctx, record.ID); err != sql.ErrNoRows {
		t.Fatalf("terminal cancel err=%v, want sql.ErrNoRows", err)
	}
}

func TestMemoryOperationStoreCancelCancelsInFlightHandler(t *testing.T) {
	store := newOperationStore(nil)
	ctx := context.Background()
	record := operationTestRecord()
	if _, err := store.start(ctx, record); err != nil {
		t.Fatalf("start: %v", err)
	}
	cancelled := make(chan struct{})
	store.registerCancel(record.ID, func() { close(cancelled) })
	if err := store.cancel(ctx, record.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("cancel did not signal the in-flight handler")
	}
	store.clearCancel(record.ID)
}

func TestMemoryOperationStoreCancelFailsClosedAfterMatrixPrepare(t *testing.T) {
	store := newOperationStore(nil)
	prepared := &cancellationPreparedStore{}
	store.setPreparedMatrixStore(prepared)
	record := operationTestRecord()
	if _, err := store.start(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	exactPDU := []byte(`{"_event_id":"$prepared","content":{"body":"exact"}}`)
	if err := prepared.PrepareMatrixEvent(context.Background(), dirextalktransport.PreparedMatrixEvent{
		OperationID: record.ID, Capability: record.Capability, Operation: record.Operation,
		OwnerID: record.OwnerID, Generation: record.Generation, RootDigest: record.RootDigest,
		Event: dirextalktransport.PreparedMessage{EventID: "$prepared", RoomID: "!room:example.com", SenderMXID: "@agent:example.com", EventType: "m.room.message", RoomVersion: "11", EventJSON: exactPDU, ContentDigest: make([]byte, 32)},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.cancel(context.Background(), record.ID); err != errOperationPrepared {
		t.Fatalf("prepared Matrix cancel err=%v, want errOperationPrepared", err)
	}
	current, err := store.get(context.Background(), record.ID)
	if err != nil || current.State != capv1.OperationState_OPERATION_STATE_PENDING {
		t.Fatalf("prepared cancellation changed state=%v err=%v", current.State, err)
	}
	if prepared.event == nil || string(prepared.event.Event.EventJSON) != string(exactPDU) {
		t.Fatalf("prepared exact PDU was not retained: %#v", prepared.event)
	}
}

func TestMemoryOperationStoreCancelRejectsUncertainMatrixOperation(t *testing.T) {
	store := newOperationStore(nil)
	record := operationTestRecord()
	if _, err := store.start(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := store.markUncertain(context.Background(), record.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := store.cancel(context.Background(), record.ID); err != sql.ErrNoRows {
		t.Fatalf("uncertain cancellation err=%v, want sql.ErrNoRows", err)
	}
}

func TestExecuteOperationKeepsPreparedPDUOnUnknownMatrixOutcome(t *testing.T) {
	store := newOperationStore(nil)
	prepared := &cancellationPreparedStore{}
	store.setPreparedMatrixStore(prepared)
	record := operationTestRecord()
	if _, err := store.start(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	if err := prepared.PrepareMatrixEvent(context.Background(), dirextalktransport.PreparedMatrixEvent{OperationID: record.ID, Capability: record.Capability, Operation: record.Operation, OwnerID: record.OwnerID, Generation: record.Generation, RootDigest: record.RootDigest, Event: dirextalktransport.PreparedMessage{EventID: "$prepared", RoomID: "!room:example.test", SenderMXID: "@agent:example.test", RoomVersion: "11", EventJSON: []byte(`{"event_id":"$prepared"}`), ContentDigest: make([]byte, 32)}}); err != nil {
		t.Fatal(err)
	}
	server := &Server{operations: store}
	provider := &Provider{Handler: func(context.Context, []byte) ([]byte, error) {
		return nil, dirextalktransport.ErrMatrixEventUnknown
	}}
	server.executeOperation(context.Background(), record, provider, record.Operation, []byte(`{}`))
	current, err := store.get(context.Background(), record.ID)
	if err != nil || current.State != capv1.OperationState_OPERATION_STATE_UNCERTAIN {
		t.Fatalf("unknown Matrix outcome state=%v err=%v", current.State, err)
	}
	if !prepared.present || prepared.event == nil {
		t.Fatal("unknown Matrix outcome deleted prepared PDU")
	}
}

func TestMemoryOperationStoreCancelFailsClosedBeforePrepareRace(t *testing.T) {
	store := newOperationStore(nil)
	store.setPreparedMatrixStore(&cancellationPreparedStore{})
	record := operationTestRecord()
	if _, err := store.start(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	// A false point-in-time probe is not proof that a concurrent handler cannot
	// prepare immediately afterward, so the in-memory ledger must reject it.
	if err := store.cancel(context.Background(), record.ID); err != errOperationPrepared {
		t.Fatalf("preparation-race Matrix cancel err=%v, want errOperationPrepared", err)
	}
	current, err := store.get(context.Background(), record.ID)
	if err != nil || current.State != capv1.OperationState_OPERATION_STATE_PENDING {
		t.Fatalf("preparation-race cancellation changed state=%v err=%v", current.State, err)
	}
}

func TestPostgresOperationStartCommitsAcceptedEventAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store := newOperationStore(db)
	record := operationTestRecord()
	now := time.Now().UTC()
	query := regexp.QuoteMeta(`SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,request_digest,state,COALESCE(result_json::text,''),COALESCE(error_code,''),COALESCE(error_message,''),revision,updated_at FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`)
	mock.ExpectBegin()
	mock.ExpectQuery(query).WithArgs(record.ID).WillReturnRows(sqlmock.NewRows([]string{
		"operation_id", "capability_id", "operation", "owner_id", "account_generation", "root_request_digest", "request_digest", "state", "result", "error_code", "error_message", "revision", "updated_at",
	}))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO p2p_capability_operations(operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,request_digest,state,revision,updated_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10) ON CONFLICT(operation_id) DO NOTHING`)).WithArgs(record.ID, record.Capability, record.Operation, record.OwnerID, record.Generation, record.RootDigest, record.Digest, "pending", int64(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT operation_id FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`)).WithArgs(record.ID).WillReturnRows(sqlmock.NewRows([]string{"operation_id"}).AddRow(record.ID))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(MAX(sequence),0)+1 FROM p2p_capability_operation_events WHERE operation_id=$1`)).WithArgs(record.ID).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(1)))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO p2p_capability_operation_events(operation_id,sequence,event_json) VALUES($1,$2,$3::jsonb)`)).WithArgs(record.ID, int64(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	created, err := store.start(context.Background(), record)
	if err != nil || created == nil || !created.Created {
		t.Fatalf("created=%#v err=%v", created, err)
	}
	if got := store.eventCount(record.ID); got != 1 {
		t.Fatalf("cached accepted event count=%d, want 1", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("postgres start expectations: %v (now=%v)", err, now)
	}
}

func TestPostgresOperationStartRollsBackWhenAcceptedEventFails(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store := newOperationStore(db)
	record := operationTestRecord()
	query := regexp.QuoteMeta(`SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,request_digest,state,COALESCE(result_json::text,''),COALESCE(error_code,''),COALESCE(error_message,''),revision,updated_at FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`)
	mock.ExpectBegin()
	mock.ExpectQuery(query).WithArgs(record.ID).WillReturnRows(sqlmock.NewRows([]string{
		"operation_id", "capability_id", "operation", "owner_id", "account_generation", "root_request_digest", "request_digest", "state", "result", "error_code", "error_message", "revision", "updated_at",
	}))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO p2p_capability_operations(operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,request_digest,state,revision,updated_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10) ON CONFLICT(operation_id) DO NOTHING`)).WithArgs(record.ID, record.Capability, record.Operation, record.OwnerID, record.Generation, record.RootDigest, record.Digest, "pending", int64(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT operation_id FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`)).WithArgs(record.ID).WillReturnRows(sqlmock.NewRows([]string{"operation_id"}).AddRow(record.ID))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(MAX(sequence),0)+1 FROM p2p_capability_operation_events WHERE operation_id=$1`)).WithArgs(record.ID).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(1)))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO p2p_capability_operation_events(operation_id,sequence,event_json) VALUES($1,$2,$3::jsonb)`)).WithArgs(record.ID, int64(1), sqlmock.AnyArg()).WillReturnError(sql.ErrConnDone)
	mock.ExpectRollback()

	created, err := store.start(context.Background(), record)
	if err == nil || created != nil {
		t.Fatalf("failed accepted event returned created=%#v err=%v", created, err)
	}
	if got := store.eventCount(record.ID); got != 0 {
		t.Fatalf("rolled-back accepted event cached count=%d, want 0", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("rollback expectations: %v", err)
	}
}

func TestPostgresOperationFinishUsesNonNullErrorColumns(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := newOperationStore(db)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT state FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`)).WithArgs("operation-1").WillReturnRows(sqlmock.NewRows([]string{"state"}).AddRow("pending"))
	query := regexp.QuoteMeta(`UPDATE p2p_capability_operations SET state=$1,result_json=CASE WHEN $2='' THEN NULL ELSE $2::jsonb END,error_code=$3,error_message=$4,revision=revision+1,updated_at=NOW() WHERE operation_id=$5 AND state IN ('pending','running','uncertain')`)
	mock.ExpectExec(query).WithArgs("completed", `{"ok":true}`, "", "", "operation-1").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT operation_id FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`)).WithArgs("operation-1").WillReturnRows(sqlmock.NewRows([]string{"operation_id"}).AddRow("operation-1"))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(MAX(sequence),0)+1 FROM p2p_capability_operation_events WHERE operation_id=$1`)).WithArgs("operation-1").WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(1)))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO p2p_capability_operation_events(operation_id,sequence,event_json) VALUES($1,$2,$3::jsonb)`)).WithArgs("operation-1", int64(1), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	if err := store.finish(context.Background(), "operation-1", []byte(`{"ok":true}`), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("finish expectations: %v", err)
	}
}

func TestPostgresOperationCancelFailsClosedWhenPreparedMatrixEventExists(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store := newOperationStore(db)
	store.setPreparedMatrixStore(dirextalktransport.NewPostgresPreparedMatrixMutationStore(db))
	record := operationTestRecord()
	rowQuery := regexp.QuoteMeta(`SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,state FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`)
	mock.ExpectBegin()
	mock.ExpectQuery(rowQuery).WithArgs(record.ID).WillReturnRows(sqlmock.NewRows([]string{
		"operation_id", "capability_id", "operation", "owner_id", "account_generation", "root_request_digest", "state",
	}).AddRow(record.ID, record.Capability, record.Operation, record.OwnerID, record.Generation, record.RootDigest, "running"))
	presenceQuery := regexp.QuoteMeta(`SELECT EXISTS(SELECT 1 FROM p2p_capability_matrix_prepared_events WHERE operation_id=$1 AND owner_id=$2 AND account_generation=$3 AND root_request_digest=$4)`)
	mock.ExpectQuery(presenceQuery).WithArgs(record.ID, record.OwnerID, record.Generation, record.RootDigest).WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	mock.ExpectRollback()

	if err := store.cancel(context.Background(), record.ID); err != errOperationPrepared {
		t.Fatalf("prepared DB cancel err=%v, want errOperationPrepared", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("prepared DB cancel expectations: %v", err)
	}
}

func TestPostgresOperationCancelRejectsUncertainBeforePreparedProbe(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store := newOperationStore(db)
	store.setPreparedMatrixStore(dirextalktransport.NewPostgresPreparedMatrixMutationStore(db))
	record := operationTestRecord()
	rowQuery := regexp.QuoteMeta(`SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,state FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`)
	mock.ExpectBegin()
	mock.ExpectQuery(rowQuery).WithArgs(record.ID).WillReturnRows(sqlmock.NewRows([]string{
		"operation_id", "capability_id", "operation", "owner_id", "account_generation", "root_request_digest", "state",
	}).AddRow(record.ID, record.Capability, record.Operation, record.OwnerID, record.Generation, record.RootDigest, "uncertain"))
	mock.ExpectCommit()

	if err := store.cancel(context.Background(), record.ID); err != sql.ErrNoRows {
		t.Fatalf("uncertain DB cancel err=%v, want sql.ErrNoRows", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("uncertain DB cancel expectations: %v", err)
	}
}

func TestPostgresOperationEnsureSchemaFencesRestartAndWatchDeliversUncertain(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	store := newOperationStore(db)
	for _, table := range []string{"p2p_capability_operations", "p2p_capability_operation_events", "p2p_capability_matrix_prepared_events"} {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT to_regclass($1) IS NOT NULL`)).WithArgs(table).WillReturnRows(sqlmock.NewRows([]string{"present"}).AddRow(true))
	}
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT operation_id FROM p2p_capability_operations WHERE state IN ('pending','running') FOR UPDATE`)).WillReturnRows(sqlmock.NewRows([]string{"operation_id"}).AddRow("op-pending").AddRow("op-running"))
	for _, operationID := range []string{"op-pending", "op-running"} {
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE p2p_capability_operations SET state='uncertain',error_code='SERVICE_RESTARTED',error_message='operation was in flight when message-server restarted',revision=revision+1,updated_at=NOW() WHERE operation_id=$1 AND state IN ('pending','running')`)).WithArgs(operationID).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT operation_id FROM p2p_capability_operations WHERE operation_id=$1 FOR UPDATE`)).WithArgs(operationID).WillReturnRows(sqlmock.NewRows([]string{"operation_id"}).AddRow(operationID))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(MAX(sequence),0)+1 FROM p2p_capability_operation_events WHERE operation_id=$1`)).WithArgs(operationID).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(2)))
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO p2p_capability_operation_events(operation_id,sequence,event_json) VALUES($1,$2,$3::jsonb)`)).WithArgs(operationID, int64(2), sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectCommit()

	if err := store.ensureSchema(context.Background()); err != nil {
		t.Fatalf("ensureSchema: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("ensureSchema expectations: %v", err)
	}

	// A watcher resuming after the accepted event must receive the durable
	// uncertain progress event instead of waiting forever for a terminal event.
	uncertain := &capv1.WatchOperationEvent{OperationId: "op-pending", Sequence: 2, Event: &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: []byte(`{"state":"uncertain"}`)}}}
	raw, err := protojson.Marshal(uncertain)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT operation_id,capability_id,operation,owner_id,account_generation,root_request_digest,request_digest,state,COALESCE(result_json::text,''),COALESCE(error_code,''),COALESCE(error_message,''),revision,updated_at FROM p2p_capability_operations WHERE operation_id=$1`)).WithArgs("op-pending").WillReturnRows(sqlmock.NewRows([]string{
		"operation_id", "capability_id", "operation", "owner_id", "account_generation", "root_request_digest", "request_digest", "state", "result", "error_code", "error_message", "revision", "updated_at",
	}).AddRow("op-pending", "product.messages.v1", "send", "owner-1", int64(1), make([]byte, 32), make([]byte, 32), "uncertain", "", "SERVICE_RESTARTED", "operation was in flight when message-server restarted", int64(2), time.Now().UTC()))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT sequence,event_json FROM p2p_capability_operation_events WHERE operation_id=$1 AND sequence>$2 ORDER BY sequence`)).WithArgs("op-pending", int64(1)).WillReturnRows(sqlmock.NewRows([]string{"sequence", "event_json"}).AddRow(int64(2), raw))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	watch, err := store.watch(ctx, "op-pending", 1)
	if err != nil {
		t.Fatalf("watch after restart: %v", err)
	}
	select {
	case event := <-watch:
		if event.Sequence != 2 || event.Event == nil || event.Event.GetProgress() == nil {
			t.Fatalf("watch delivered %#v, want uncertain progress sequence 2", event)
		}
	case <-time.After(time.Second):
		t.Fatal("watch remained blocked after accepted event; restart fence was not durable")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("watch expectations: %v", err)
	}
}
