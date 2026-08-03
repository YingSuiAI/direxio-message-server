package storage

import (
	"context"
	"reflect"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/operations"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestMemoryStoreUpsertsLooksUpAndResetsOperations(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	var _ operations.Store = store

	if record, ok, err := store.LookupOperation(ctx, "missing"); err != nil || ok || record.OperationID != "" {
		t.Fatalf("missing operation lookup = (%#v, %v, %v)", record, ok, err)
	}

	record := operationFixture()
	if err := store.UpsertOperation(ctx, record); err != nil {
		t.Fatal(err)
	}
	record.Status = "joined"
	record.Phase = "complete"
	record.CurrentRoomID = "!current:example.com"
	record.ResultJSON = `{"status":"joined"}`
	record.ErrorCode = ""
	record.UpdatedAt++
	if err := store.UpsertOperation(ctx, record); err != nil {
		t.Fatal(err)
	}

	got, ok, err := store.LookupOperation(ctx, record.OperationID)
	if err != nil || !ok || !reflect.DeepEqual(got, record) {
		t.Fatalf("operation lookup = (%#v, %v, %v), want %#v", got, ok, err, record)
	}

	store.ResetAccountState()
	if _, ok, err := store.LookupOperation(ctx, record.OperationID); err != nil || ok {
		t.Fatalf("operation survived account reset: ok=%v err=%v", ok, err)
	}
	verifyOperationClaimCAS(t, store)
	verifyOperationIdentityConflictDoesNotLease(t, store)
}

func TestDatabaseStorePersistsOperationsAndCreatesSchema(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()

	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var _ operations.Store = store

	expectedColumns := []string{
		"operation_id", "action", "status", "phase", "room_id", "current_room_id", "user_id",
		"peer_mxid", "request_id", "base_request_id", "result_json", "error_code", "revision", "lease_owner", "lease_until",
		"created_at", "updated_at",
	}
	for _, column := range expectedColumns {
		var count int
		if err := store.DB().QueryRowContext(ctx, `
			SELECT COUNT(*) FROM information_schema.columns
			WHERE table_schema = 'public' AND table_name = 'p2p_operations' AND column_name = $1
		`, column).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("expected p2p_operations.%s to exist", column)
		}
	}
	var indexName string
	if err := store.DB().QueryRowContext(ctx, `
		SELECT indexname FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'p2p_operations_status_updated_idx'
	`).Scan(&indexName); err != nil {
		t.Fatalf("expected operation recovery index: %v", err)
	}
	if err := store.DB().QueryRowContext(ctx, `
		SELECT indexname FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = 'p2p_operations_lease_idx'
	`).Scan(&indexName); err != nil {
		t.Fatalf("expected operation lease index: %v", err)
	}

	if record, ok, err := store.LookupOperation(ctx, "missing"); err != nil || ok || record.OperationID != "" {
		t.Fatalf("missing operation lookup = (%#v, %v, %v)", record, ok, err)
	}
	record := operationFixture()
	if err := store.UpsertOperation(ctx, record); err != nil {
		t.Fatal(err)
	}
	record.Status = "join_failed"
	record.Phase = "matrix_join"
	record.CurrentRoomID = "!current:example.com"
	record.ErrorCode = "matrix_join_failed"
	record.UpdatedAt++
	if err := store.UpsertOperation(ctx, record); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer reloaded.Close()
	got, ok, err := reloaded.LookupOperation(ctx, record.OperationID)
	if err != nil || !ok || !reflect.DeepEqual(got, record) {
		t.Fatalf("reloaded operation = (%#v, %v, %v), want %#v", got, ok, err, record)
	}
	verifyOperationClaimCAS(t, reloaded)
	verifyOperationIdentityConflictDoesNotLease(t, reloaded)
}

func verifyOperationClaimCAS(t *testing.T, store operations.Store) {
	t.Helper()
	ctx := context.Background()
	record := operationFixture()
	record.OperationID += "-claim"
	record.Revision = 0
	record.LeaseOwner = ""
	record.LeaseUntil = 0

	claimed, ok, err := store.ClaimOperation(ctx, record, "worker-a", 2_000)
	if err != nil || !ok || claimed.Revision != 1 || claimed.LeaseOwner != "worker-a" || claimed.LeaseUntil == 0 {
		t.Fatalf("initial claim = (%#v, %v, %v)", claimed, ok, err)
	}
	blocked, ok, err := store.ClaimOperation(ctx, record, "worker-b", 2_000)
	if err != nil || ok || blocked.LeaseOwner != "worker-a" || blocked.Revision != claimed.Revision {
		t.Fatalf("live lease was not exclusive = (%#v, %v, %v)", blocked, ok, err)
	}
	claimed.Status = "completed"
	updated, ok, err := store.CompareAndSwapOperation(ctx, claimed, claimed.Revision, "worker-a", 5_000)
	if err != nil || !ok || updated.Revision != claimed.Revision+1 || updated.Status != "completed" || updated.LeaseUntil <= claimed.LeaseUntil {
		t.Fatalf("owner CAS = (%#v, %v, %v)", updated, ok, err)
	}
	stale, ok, err := store.CompareAndSwapOperation(ctx, claimed, claimed.Revision, "worker-a", 5_000)
	if err != nil || ok || stale.OperationID != "" {
		t.Fatalf("stale CAS unexpectedly succeeded = (%#v, %v, %v)", stale, ok, err)
	}
	updated.LeaseOwner = ""
	updated.LeaseUntil = 0
	released, ok, err := store.CompareAndSwapOperation(ctx, updated, updated.Revision, "worker-a", 0)
	if err != nil || !ok || released.LeaseOwner != "" || released.LeaseUntil != 0 {
		t.Fatalf("lease release = (%#v, %v, %v)", released, ok, err)
	}
	reclaimed, ok, err := store.ClaimOperation(ctx, record, "worker-b", 2_000)
	if err != nil || !ok || reclaimed.LeaseOwner != "worker-b" || reclaimed.Revision != released.Revision+1 {
		t.Fatalf("released claim was not reusable = (%#v, %v, %v)", reclaimed, ok, err)
	}
}

func verifyOperationIdentityConflictDoesNotLease(t *testing.T, store operations.Store) {
	t.Helper()
	ctx := context.Background()
	record := operationFixture()
	record.OperationID += "-identity-conflict"
	record.Revision = 0
	record.LeaseOwner = ""
	record.LeaseUntil = 0
	if err := store.UpsertOperation(ctx, record); err != nil {
		t.Fatal(err)
	}

	conflict := record
	conflict.RoomID = "!different:example.com"
	claimed, ok, err := store.ClaimOperation(ctx, conflict, "conflicting-worker", 2_000)
	if err != nil || ok || claimed.OperationID != record.OperationID {
		t.Fatalf("identity-conflicting claim = (%#v, %v, %v)", claimed, ok, err)
	}
	current, found, err := store.LookupOperation(ctx, record.OperationID)
	if err != nil || !found {
		t.Fatalf("lookup after identity conflict = (%#v, %v, %v)", current, found, err)
	}
	if current.Revision != record.Revision || current.LeaseOwner != "" || current.LeaseUntil != 0 {
		t.Fatalf("identity conflict mutated or leased operation: got=%#v want_revision=%d", current, record.Revision)
	}
	valid, ok, err := store.ClaimOperation(ctx, record, "valid-worker", 2_000)
	if err != nil || !ok || valid.LeaseOwner != "valid-worker" || valid.Revision != record.Revision+1 {
		t.Fatalf("identity conflict left an unusable lease: (%#v, %v, %v)", valid, ok, err)
	}
}

func operationFixture() operations.Record {
	return operations.Record{
		OperationID:   "op-123",
		Action:        "channels.join_request.approve",
		Status:        "joining",
		Phase:         "invite_sent",
		RoomID:        "!requested:example.com",
		CurrentRoomID: "",
		UserID:        "@alice:remote.example",
		PeerMXID:      "@owner:example.com",
		RequestID:     "request-456",
		BaseRequestID: "request-123",
		ResultJSON:    "",
		ErrorCode:     "join_pending",
		Revision:      3,
		LeaseOwner:    "worker",
		LeaseUntil:    1710000060000,
		CreatedAt:     1710000000000,
		UpdatedAt:     1710000000100,
	}
}
