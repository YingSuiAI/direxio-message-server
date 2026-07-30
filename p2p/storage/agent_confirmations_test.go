package storage

import (
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

const (
	testConfirmationOwner  = "@owner:example.test"
	testConfirmationID     = "11111111-1111-4111-8111-111111111111"
	testConfirmationTaskID = "22222222-2222-4222-8222-222222222222"
	testConfirmationTarget = "33333333-3333-4333-8333-333333333333"
)

func testConfirmationBinding(t *testing.T) confirmation.Binding {
	t.Helper()
	digest := confirmation.Digest(strings.Repeat("a", 64))
	binding, err := (confirmation.Binding{
		Digest:            string(digest),
		OwnerID:           testConfirmationOwner,
		OperationDomain:   "extension.execute",
		TargetID:          testConfirmationTarget,
		TargetRevision:    3,
		TargetKind:        "mcp",
		SourceVersion:     "1.0.0",
		ContentDigest:     digest,
		ParameterDigest:   digest,
		NetworkDigest:     digest,
		SecretGrantDigest: digest,
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func testWorkloadBinding(t *testing.T, kind workload.OperationKind, targetKind workload.TargetKind) confirmation.Binding {
	t.Helper()
	p := workload.Plan{
		ID:         "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
		Revision:   3,
		Digest:     strings.Repeat("b", 64),
		TargetKind: targetKind,
		Target: workload.TargetSettings{Identity: workload.TargetIdentity{
			Kind:       targetKind,
			AccountID:  "123456789012",
			Region:     "ap-east-1",
			InstanceID: "i-0123456789abcdef0",
		}},
	}
	binding := workload.BindingForOperation(p, testConfirmationTarget, kind)
	binding.OwnerID = testConfirmationOwner
	normalized, err := binding.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}

func testConfirmationRows(t *testing.T, binding confirmation.Binding, state confirmation.State, revision int64, createdAt, updatedAt, expiresAt time.Time, reason string) *sqlmock.Rows {
	t.Helper()
	raw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	return sqlmock.NewRows([]string{
		"confirmation_id", "owner_id", "operation_domain", "target_id", "target_revision",
		"binding_digest", "binding_json", "task_id", "state", "revision", "created_at",
		"updated_at", "expires_at", "terminal_reason",
	}).AddRow(
		testConfirmationID, testConfirmationOwner, binding.OperationDomain, binding.TargetID,
		binding.TargetRevision, binding.Digest, raw, testConfirmationTaskID, string(state),
		revision, createdAt, updatedAt, expiresAt, reason,
	)
}

func terminalConfirmationReplayJSON(t *testing.T, value confirmation.Confirmation) []byte {
	t.Helper()
	raw, err := json.Marshal(struct {
		Confirmation confirmation.Confirmation `json:"confirmation"`
		Error        string                    `json:"error"`
	}{Confirmation: value, Error: "expired"})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func expectConfirmationReplayMiss(mock sqlmock.Sqlmock, owner, operation, key string) {
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", owner, operation, key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).
		WithArgs(owner, operation, key).
		WillReturnError(sql.ErrNoRows)
}

func expectConfirmationIdentity(mock sqlmock.Sqlmock, owner string) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_id,task_id::text FROM agent_confirmations")).
		WithArgs(testConfirmationID, owner).
		WillReturnRows(sqlmock.NewRows([]string{"owner_id", "task_id"}).AddRow(testConfirmationOwner, testConfirmationTaskID))
}

func expectConfirmationTaskLock(mock sqlmock.Sqlmock, status string, attempt int, epoch, revision int64, lease any) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_id,status,attempt,lease_epoch,revision,lease_expires_at FROM agent_tasks")).
		WithArgs(testConfirmationTaskID, testConfirmationOwner).
		WillReturnRows(sqlmock.NewRows([]string{"owner_id", "status", "attempt", "lease_epoch", "revision", "lease_expires_at"}).
			AddRow(testConfirmationOwner, status, attempt, epoch, revision, lease))
}

func TestDatabaseConfirmationConfirmPersistsAndReplaysResponse(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Date(2026, 7, 29, 8, 0, 0, 0, time.UTC)
	expiresAt := at.Add(time.Hour)
	key := "44444444-4444-4444-8444-444444444444"
	command := confirmation.ConfirmCommand{
		OwnerID:          testConfirmationOwner,
		ConfirmationID:   testConfirmationID,
		IdempotencyKey:   key,
		RequestDigest:    confirmation.Digest(strings.Repeat("f", 64)),
		ExpectedRevision: 1,
		Binding:          binding,
		At:               at,
	}
	digest := confirmation.RequestDigestForConfirm(command)

	mock.ExpectBegin()
	expectConfirmationReplayMiss(mock, testConfirmationOwner, "confirm", key)
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 0, 0, 7, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, at.Add(-time.Minute), at.Add(-time.Minute), expiresAt, ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='confirmed'")).
		WithArgs(at, testConfirmationID, testConfirmationOwner, int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='queued'")).
		WithArgs(at, testConfirmationTaskID, testConfirmationOwner).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).
		WithArgs(at, testConfirmationTaskID, testConfirmationOwner).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StateConfirmed, 2, at.Add(-time.Minute), at, expiresAt, ""))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmation_replays")).
		WithArgs(testConfirmationOwner, "confirm", key, string(digest), sqlmock.AnyArg(), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	first, err := store.Confirm(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != confirmation.StateConfirmed || first.Revision != 2 {
		t.Fatalf("unexpected confirmed value: %+v", first)
	}
	replayJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", testConfirmationOwner, "confirm", key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).
		WithArgs(testConfirmationOwner, "confirm", key).
		WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(string(digest), replayJSON))
	mock.ExpectRollback()

	second, err := store.Confirm(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.State != first.State || second.Revision != first.Revision {
		t.Fatalf("replay changed response: first=%+v second=%+v", first, second)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationConfirmLateExpiryTerminalizesBeforeReturning(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Date(2026, 7, 29, 8, 15, 0, 0, time.UTC)
	key := "41414141-4141-4141-8414-414141414141"
	command := confirmation.ConfirmCommand{OwnerID: testConfirmationOwner, ConfirmationID: testConfirmationID, IdempotencyKey: key, ExpectedRevision: 1, Binding: binding, At: at}

	mock.ExpectBegin()
	expectConfirmationReplayMiss(mock, testConfirmationOwner, "confirm", key)
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 1, 1, 7, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, at.Add(-time.Hour), at.Add(-time.Minute), at, ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='expired'")).WithArgs(confirmation.ReasonExpired, at, testConfirmationID, testConfirmationOwner, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='failed'")).WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner, "waiting_user").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).WithArgs(testConfirmationTaskID, confirmation.ReasonExpired, at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StateExpired, 2, at.Add(-time.Hour), at, at, confirmation.ReasonExpired))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmation_replays")).WithArgs(testConfirmationOwner, "confirm", key, string(confirmation.RequestDigestForConfirm(command)), sqlmock.AnyArg(), at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	expired, err := store.Confirm(t.Context(), command)
	if !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("expected expired confirmation, got %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", testConfirmationOwner, "confirm", key)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).WithArgs(testConfirmationOwner, "confirm", key).WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(string(confirmation.RequestDigestForConfirm(command)), terminalConfirmationReplayJSON(t, expired)))
	mock.ExpectRollback()
	second, err := store.Confirm(t.Context(), command)
	if !errors.Is(err, confirmation.ErrExpired) || second.State != confirmation.StateExpired {
		t.Fatalf("expired replay = %#v, %v", second, err)
	}
	conflictCommand := command
	conflictCommand.ExpectedRevision = 2
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", testConfirmationOwner, "confirm", key)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).WithArgs(testConfirmationOwner, "confirm", key).WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(string(confirmation.RequestDigestForConfirm(command)), terminalConfirmationReplayJSON(t, expired)))
	mock.ExpectRollback()
	if _, err = store.Confirm(t.Context(), conflictCommand); !errors.Is(err, confirmation.ErrIdempotencyConflict) {
		t.Fatalf("different digest error = %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationRejectAtomicallyCancelsTaskAndPersistsReplay(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Date(2026, 7, 29, 8, 30, 0, 0, time.UTC)
	expiresAt := at.Add(time.Hour)
	key := "45454545-4545-4545-8545-454545454545"
	command := confirmation.RejectCommand{
		OwnerID:          testConfirmationOwner,
		ConfirmationID:   testConfirmationID,
		IdempotencyKey:   key,
		RequestDigest:    confirmation.Digest(strings.Repeat("f", 64)),
		ExpectedRevision: 1,
		Reason:           "not approved",
		Note:             "owner declined",
		At:               at,
	}
	digest := confirmation.RequestDigestForReject(command)

	mock.ExpectBegin()
	expectConfirmationReplayMiss(mock, testConfirmationOwner, "reject", key)
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 0, 0, 7, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, at.Add(-time.Minute), at.Add(-time.Minute), expiresAt, ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='rejected'")).
		WithArgs(command.Reason, at, testConfirmationID, testConfirmationOwner, int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='canceled'")).
		WithArgs(command.Reason, at, testConfirmationTaskID, testConfirmationOwner).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).
		WithArgs(testConfirmationTaskID, confirmation.ReasonUserRejected, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).
		WithArgs(testConfirmationTaskID, command.Reason, at, testConfirmationOwner).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StateRejected, 2, at.Add(-time.Minute), at, expiresAt, command.Reason))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmation_replays")).
		WithArgs(testConfirmationOwner, "reject", key, string(digest), sqlmock.AnyArg(), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	out, err := store.Reject(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != confirmation.StateRejected || out.Revision != 2 || out.TerminalReason != command.Reason {
		t.Fatalf("unexpected rejected value: %+v", out)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationRejectLateExpiryTerminalizesBeforeReturning(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Date(2026, 7, 29, 8, 20, 0, 0, time.UTC)
	key := "42424242-4242-4242-8424-424242424242"
	command := confirmation.RejectCommand{OwnerID: testConfirmationOwner, ConfirmationID: testConfirmationID, IdempotencyKey: key, ExpectedRevision: 1, Reason: "too late", At: at}

	mock.ExpectBegin()
	expectConfirmationReplayMiss(mock, testConfirmationOwner, "reject", key)
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 1, 1, 7, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, at.Add(-time.Hour), at.Add(-time.Minute), at, ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='expired'")).WithArgs(confirmation.ReasonExpired, at, testConfirmationID, testConfirmationOwner, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='failed'")).WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner, "waiting_user").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).WithArgs(testConfirmationTaskID, confirmation.ReasonExpired, at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StateExpired, 2, at.Add(-time.Hour), at, at, confirmation.ReasonExpired))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmation_replays")).WithArgs(testConfirmationOwner, "reject", key, string(confirmation.RequestDigestForReject(command)), sqlmock.AnyArg(), at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	expired, err := store.Reject(t.Context(), command)
	if !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("expected expired confirmation, got %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", testConfirmationOwner, "reject", key)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).WithArgs(testConfirmationOwner, "reject", key).WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(string(confirmation.RequestDigestForReject(command)), terminalConfirmationReplayJSON(t, expired)))
	mock.ExpectRollback()
	second, err := store.Reject(t.Context(), command)
	if !errors.Is(err, confirmation.ErrExpired) || second.State != confirmation.StateExpired {
		t.Fatalf("expired replay = %#v, %v", second, err)
	}
	conflictCommand := command
	conflictCommand.Reason = "different"
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", testConfirmationOwner, "reject", key)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).WithArgs(testConfirmationOwner, "reject", key).WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(string(confirmation.RequestDigestForReject(command)), terminalConfirmationReplayJSON(t, expired)))
	mock.ExpectRollback()
	if _, err = store.Reject(t.Context(), conflictCommand); !errors.Is(err, confirmation.ErrIdempotencyConflict) {
		t.Fatalf("different digest error = %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationRejectTerminalizesLinkedWorkloadOperation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testWorkloadBinding(t, workload.OperationApply, workload.TargetAWSEC2SSM)
	at := time.Date(2026, 7, 29, 8, 45, 0, 0, time.UTC)
	expiresAt := at.Add(time.Hour)
	key := "46464646-4646-4646-8464-464646464646"
	operationID := "77777777-7777-4777-8777-777777777777"
	workloadID := testConfirmationTarget
	command := confirmation.RejectCommand{
		OwnerID:          testConfirmationOwner,
		ConfirmationID:   testConfirmationID,
		IdempotencyKey:   key,
		ExpectedRevision: 1,
		Reason:           "not approved",
		At:               at,
	}
	digest := confirmation.RequestDigestForReject(command)

	mock.ExpectBegin()
	expectConfirmationReplayMiss(mock, testConfirmationOwner, "reject", key)
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 1, 1, 7, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, at.Add(-time.Minute), at.Add(-time.Minute), expiresAt, ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='rejected'")).WithArgs(command.Reason, at, testConfirmationID, testConfirmationOwner, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='canceled'")).WithArgs(command.Reason, at, testConfirmationTaskID, testConfirmationOwner).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).WithArgs(testConfirmationTaskID, confirmation.ReasonUserRejected, at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,workload_id::text,confirmation_id::text,plan_revision,operation,status,dispatch_state,revision,expected_workload_revision")).WithArgs(testConfirmationOwner, testConfirmationTaskID).WillReturnRows(sqlmock.NewRows([]string{"operation_id", "workload_id", "confirmation_id", "plan_revision", "operation", "status", "dispatch_state", "revision", "expected_workload_revision"}).AddRow(operationID, workloadID, testConfirmationID, 3, "apply", "waiting_user", "prepared", 1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workload_operations SET status=$1,dispatch_state='terminal'")).WithArgs("rejected", "user_rejected", command.Reason, at, testConfirmationOwner, operationID, testConfirmationTaskID, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workloads SET state='failed',revision=revision+1,updated_at=$1")).WithArgs(at, testConfirmationOwner, workloadID, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence)")).WithArgs(testConfirmationOwner, operationID).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(2)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT workload_id::text FROM core_workload_operations")).WithArgs(testConfirmationOwner, operationID).WillReturnRows(sqlmock.NewRows([]string{"workload_id"}).AddRow(workloadID))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO p2p_agent_deployment_event_cursors(owner_id,workload_id,last_sequence,updated_at)")).WithArgs(testConfirmationOwner, workloadID, at).WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(int64(4)))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_workload_events(owner_id,workload_id,operation_id,sequence,public_sequence,kind,status,message,readback_json,at)")).WithArgs(testConfirmationOwner, workloadID, operationID, uint64(2), int64(4), "terminal", "rejected", command.Reason, sqlmock.AnyArg(), at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).WithArgs(testConfirmationTaskID, command.Reason, at, testConfirmationOwner).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StateRejected, 2, at.Add(-time.Minute), at, expiresAt, command.Reason))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmation_replays")).WithArgs(testConfirmationOwner, "reject", key, string(digest), sqlmock.AnyArg(), at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	out, err := store.Reject(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != confirmation.StateRejected || out.Revision != 2 {
		t.Fatalf("unexpected rejected value: %+v", out)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationConsumeRejectsExpiredApprovalBeforeReservation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Date(2026, 7, 29, 9, 0, 0, 0, time.UTC)
	key := "55555555-5555-4555-8555-555555555555"
	command := confirmation.ConsumeCommand{
		OwnerID:              testConfirmationOwner,
		ConfirmationID:       testConfirmationID,
		IdempotencyKey:       key,
		TaskID:               testConfirmationTaskID,
		Attempt:              2,
		LeaseEpoch:           7,
		ExpectedRevision:     3,
		ExpectedTaskRevision: 9,
		Binding:              binding,
		At:                   at,
	}

	mock.ExpectBegin()
	expectConfirmationReplayMiss(mock, testConfirmationOwner, "consume", key)
	expectConfirmationTaskLock(mock, "running", 2, 7, 9, at.Add(time.Hour))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StateConfirmed, 3, at.Add(-time.Hour), at.Add(-time.Minute), at, ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='expired'")).WithArgs(confirmation.ReasonExpired, at, testConfirmationID, testConfirmationOwner, int64(3)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='failed'")).WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner, "running").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_task_runtime_concurrency SET")).WithArgs(at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).WithArgs(testConfirmationTaskID, confirmation.ReasonExpired, at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StateExpired, 4, at.Add(-time.Hour), at, at, confirmation.ReasonExpired))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmation_replays")).WithArgs(testConfirmationOwner, "consume", key, string(confirmation.RequestDigestForConsume(command)), sqlmock.AnyArg(), at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	expired, err := store.Consume(t.Context(), command)
	if !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("expected expired rejection, got %v", err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", testConfirmationOwner, "consume", key)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).WithArgs(testConfirmationOwner, "consume", key).WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(string(confirmation.RequestDigestForConsume(command)), terminalConfirmationReplayJSON(t, expired)))
	mock.ExpectRollback()
	second, err := store.Consume(t.Context(), command)
	if !errors.Is(err, confirmation.ErrExpired) || second.State != confirmation.StateExpired {
		t.Fatalf("expired replay = %#v, %v", second, err)
	}
	conflictCommand := command
	conflictCommand.ExpectedTaskRevision = 10
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", testConfirmationOwner, "consume", key)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).WithArgs(testConfirmationOwner, "consume", key).WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(string(confirmation.RequestDigestForConsume(command)), terminalConfirmationReplayJSON(t, expired)))
	mock.ExpectRollback()
	if _, err = store.Consume(t.Context(), conflictCommand); !errors.Is(err, confirmation.ErrIdempotencyConflict) {
		t.Fatalf("different digest error = %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationConsumePersistsReplayWithReservation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Date(2026, 7, 29, 9, 30, 0, 0, time.UTC)
	key := "56565656-5656-4656-8656-565656565656"
	command := confirmation.ConsumeCommand{
		OwnerID:              testConfirmationOwner,
		ConfirmationID:       testConfirmationID,
		IdempotencyKey:       key,
		RequestDigest:        confirmation.Digest(strings.Repeat("f", 64)),
		TaskID:               testConfirmationTaskID,
		Attempt:              2,
		LeaseEpoch:           7,
		ExpectedRevision:     3,
		ExpectedTaskRevision: 9,
		Binding:              binding,
		At:                   at,
	}
	digest := confirmation.RequestDigestForConsume(command)

	mock.ExpectBegin()
	expectConfirmationReplayMiss(mock, testConfirmationOwner, "consume", key)
	expectConfirmationTaskLock(mock, "running", 2, 7, 9, at.Add(time.Hour))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StateConfirmed, 3, at.Add(-time.Hour), at.Add(-time.Minute), at.Add(time.Hour), ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='consumed'")).
		WithArgs(testConfirmationTaskID, uint32(2), uint64(7), int64(9), at, testConfirmationID, testConfirmationOwner, int64(3)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StateConsumed, 4, at.Add(-time.Hour), at, at.Add(time.Hour), ""))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmation_replays")).
		WithArgs(testConfirmationOwner, "consume", key, string(digest), sqlmock.AnyArg(), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	first, err := store.Consume(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	replayJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", testConfirmationOwner, "consume", key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).
		WithArgs(testConfirmationOwner, "consume", key).
		WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(string(digest), replayJSON))
	mock.ExpectRollback()
	second, err := store.Consume(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.State != confirmation.StateConsumed || second.Revision != first.Revision {
		t.Fatalf("consume replay changed response: first=%+v second=%+v", first, second)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationExpireAtomicallyFailsTaskAndClearsReservations(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Date(2026, 7, 29, 10, 0, 0, 0, time.UTC)
	key := "66666666-6666-4666-8666-666666666666"
	command := confirmation.ExpireCommand{
		ConfirmationID:   testConfirmationID,
		IdempotencyKey:   key,
		RequestDigest:    confirmation.Digest(strings.Repeat("f", 64)),
		ExpectedRevision: 1,
		Reason:           confirmation.ReasonExpired,
		At:               at,
	}
	digest := confirmation.RequestDigestForExpire(command)

	mock.ExpectBegin()
	expectConfirmationIdentity(mock, "")
	expectConfirmationReplayMiss(mock, testConfirmationOwner, "expire", key)
	expectConfirmationTaskLock(mock, "running", 2, 7, 9, at.Add(time.Hour))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, at.Add(-time.Hour), at.Add(-time.Minute), at.Add(-time.Second), ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='expired'")).
		WithArgs(confirmation.ReasonExpired, at, testConfirmationID, testConfirmationOwner, int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='failed'")).
		WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner, "running").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_task_runtime_concurrency SET")).
		WithArgs(at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).
		WithArgs(testConfirmationTaskID, confirmation.ReasonExpired, at).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).
		WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StateExpired, 2, at.Add(-time.Hour), at, at.Add(-time.Second), confirmation.ReasonExpired))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmation_replays")).
		WithArgs(testConfirmationOwner, "expire", key, string(digest), sqlmock.AnyArg(), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	out, err := store.Expire(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != confirmation.StateExpired || out.Revision != 2 || out.TerminalReason != confirmation.ReasonExpired {
		t.Fatalf("unexpected expired value: %+v", out)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationExpireAtUsesSameAtomicTaskTransition(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Date(2026, 7, 29, 11, 0, 0, 0, time.UTC)

	mock.ExpectBegin()
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 0, 0, 4, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, at.Add(-time.Hour), at.Add(-time.Minute), at, ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='expired'")).
		WithArgs(confirmation.ReasonExpired, at, testConfirmationID, testConfirmationOwner, int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='failed'")).
		WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner, "waiting_user").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).
		WithArgs(testConfirmationTaskID, confirmation.ReasonExpired, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).
		WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err = store.ExpireAt(t.Context(), testConfirmationOwner, testConfirmationID, at); err != nil {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationExpireAtTerminalizesLinkedWorkloadOperation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testWorkloadBinding(t, workload.OperationDestroy, workload.TargetAWSECS)
	at := time.Date(2026, 7, 29, 11, 30, 0, 0, time.UTC)
	operationID := "99999999-9999-4999-8999-999999999999"
	workloadID := testConfirmationTarget

	mock.ExpectBegin()
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 1, 1, 4, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, at.Add(-time.Hour), at.Add(-time.Minute), at, ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='expired'")).WithArgs(confirmation.ReasonExpired, at, testConfirmationID, testConfirmationOwner, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='failed'")).WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner, "waiting_user").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).WithArgs(testConfirmationTaskID, confirmation.ReasonExpired, at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,workload_id::text,confirmation_id::text,plan_revision,operation,status,dispatch_state,revision,expected_workload_revision")).WithArgs(testConfirmationOwner, testConfirmationTaskID).WillReturnRows(sqlmock.NewRows([]string{"operation_id", "workload_id", "confirmation_id", "plan_revision", "operation", "status", "dispatch_state", "revision", "expected_workload_revision"}).AddRow(operationID, workloadID, testConfirmationID, 3, "destroy", "waiting_user", "prepared", 1, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workload_operations SET status=$1,dispatch_state='terminal'")).WithArgs("expired", confirmation.ReasonExpired, confirmation.ReasonExpired, at, testConfirmationOwner, operationID, testConfirmationTaskID, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence)")).WithArgs(testConfirmationOwner, operationID).WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(2)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT workload_id::text FROM core_workload_operations")).WithArgs(testConfirmationOwner, operationID).WillReturnRows(sqlmock.NewRows([]string{"workload_id"}).AddRow(workloadID))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO p2p_agent_deployment_event_cursors(owner_id,workload_id,last_sequence,updated_at)")).WithArgs(testConfirmationOwner, workloadID, at).WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(int64(5)))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_workload_events(owner_id,workload_id,operation_id,sequence,public_sequence,kind,status,message,readback_json,at)")).WithArgs(testConfirmationOwner, workloadID, operationID, uint64(2), int64(5), "terminal", "expired", confirmation.ReasonExpired, sqlmock.AnyArg(), at).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).WithArgs(confirmation.ReasonExpired, at, testConfirmationTaskID, testConfirmationOwner).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err = store.ExpireAt(t.Context(), testConfirmationOwner, testConfirmationID, at); err != nil {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalizeWorkloadOperationFailsClosedOnIdentityAndCardinality(t *testing.T) {
	binding := testWorkloadBinding(t, workload.OperationApply, workload.TargetAWSEC2SSM)
	stored := confirmation.Confirmation{ID: testConfirmationID, ConfirmationID: testConfirmationID, OwnerID: testConfirmationOwner, TaskID: testConfirmationTaskID, Binding: binding}
	operationID := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	workloadID := binding.TargetID
	columns := []string{"operation_id", "workload_id", "confirmation_id", "plan_revision", "operation", "status", "dispatch_state", "revision", "expected_workload_revision"}
	cases := []struct {
		name string
		rows *sqlmock.Rows
		want error
	}{
		{name: "missing", rows: sqlmock.NewRows(columns), want: confirmation.ErrNotFound},
		{name: "wrong confirmation", rows: sqlmock.NewRows(columns).AddRow(operationID, workloadID, testConfirmationTarget, 3, "apply", "waiting_user", "prepared", 1, 1), want: confirmation.ErrConflict},
		{name: "wrong target", rows: sqlmock.NewRows(columns).AddRow(operationID, "99999999-9999-4999-8999-999999999999", testConfirmationID, 3, "apply", "waiting_user", "prepared", 1, 1), want: confirmation.ErrConflict},
		{name: "wrong revision", rows: sqlmock.NewRows(columns).AddRow(operationID, workloadID, testConfirmationID, 4, "apply", "waiting_user", "prepared", 1, 1), want: confirmation.ErrConflict},
		{name: "wrong kind", rows: sqlmock.NewRows(columns).AddRow(operationID, workloadID, testConfirmationID, 3, "destroy", "waiting_user", "prepared", 1, 1), want: confirmation.ErrConflict},
		{name: "ambiguous", rows: sqlmock.NewRows(columns).AddRow(operationID, workloadID, testConfirmationID, 3, "apply", "waiting_user", "prepared", 1, 1).AddRow("cccccccc-cccc-4ccc-8ccc-cccccccccccc", workloadID, testConfirmationID, 3, "apply", "waiting_user", "prepared", 1, 1), want: confirmation.ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			mock.ExpectBegin()
			tx, err := db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,workload_id::text,confirmation_id::text,plan_revision,operation,status,dispatch_state,revision")).WithArgs(testConfirmationOwner, testConfirmationTaskID).WillReturnRows(tc.rows)
			mock.ExpectRollback()
			got := terminalizeWorkloadOperationTx(t.Context(), tx, stored, "expired", confirmation.ReasonExpired, confirmation.ReasonExpired, time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC))
			if !errors.Is(got, tc.want) {
				t.Fatalf("error = %v, want %v", got, tc.want)
			}
			if err = tx.Rollback(); err != nil {
				t.Fatal(err)
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTerminalizeWorkloadOperationPreservesExistingReadyWorkload(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	binding := testWorkloadBinding(t, workload.OperationApply, workload.TargetAWSEC2SSM)
	stored := confirmation.Confirmation{
		ID:             testConfirmationID,
		ConfirmationID: testConfirmationID,
		OwnerID:        testConfirmationOwner,
		TaskID:         testConfirmationTaskID,
		Binding:        binding,
	}
	const operationID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	const expectedWorkloadRevision = int64(5)
	at := time.Date(2026, 7, 29, 12, 5, 0, 0, time.UTC)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,workload_id::text,confirmation_id::text,plan_revision,operation,status,dispatch_state,revision,expected_workload_revision")).
		WithArgs(testConfirmationOwner, testConfirmationTaskID).
		WillReturnRows(sqlmock.NewRows([]string{
			"operation_id", "workload_id", "confirmation_id", "plan_revision",
			"operation", "status", "dispatch_state", "revision", "expected_workload_revision",
		}).AddRow(operationID, binding.TargetID, testConfirmationID, 3, "apply", "waiting_user", "prepared", 1, expectedWorkloadRevision))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workload_operations SET status=$1,dispatch_state='terminal'")).
		WithArgs("rejected", "user_rejected", "not approved", at, testConfirmationOwner, operationID, testConfirmationTaskID, int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workloads SET state='failed',revision=revision+1,updated_at=$1")).
		WithArgs(at, testConfirmationOwner, binding.TargetID, expectedWorkloadRevision).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state,revision FROM core_workloads")).
		WithArgs(testConfirmationOwner, binding.TargetID).
		WillReturnRows(sqlmock.NewRows([]string{"state", "revision"}).AddRow("ready", expectedWorkloadRevision))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence)")).
		WithArgs(testConfirmationOwner, operationID).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(int64(2)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT workload_id::text FROM core_workload_operations")).
		WithArgs(testConfirmationOwner, operationID).
		WillReturnRows(sqlmock.NewRows([]string{"workload_id"}).AddRow(binding.TargetID))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO p2p_agent_deployment_event_cursors(owner_id,workload_id,last_sequence,updated_at)")).
		WithArgs(testConfirmationOwner, binding.TargetID, at).
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(int64(4)))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_workload_events(owner_id,workload_id,operation_id,sequence,public_sequence,kind,status,message,readback_json,at)")).
		WithArgs(testConfirmationOwner, binding.TargetID, operationID, uint64(2), int64(4), "terminal", "rejected", "not approved", sqlmock.AnyArg(), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectRollback()

	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = terminalizeWorkloadOperationTx(t.Context(), tx, stored, "rejected", "user_rejected", "not approved", at); err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationReleaseReservationPersistsReplayAfterTerminalFence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	key := "77777777-7777-4777-8777-777777777777"
	command := confirmation.ReleaseReservationCommand{
		ConfirmationID:       testConfirmationID,
		TaskID:               testConfirmationTaskID,
		AcquiredAttempt:      2,
		AcquiredLeaseEpoch:   7,
		TerminalAttempt:      2,
		TerminalLeaseEpoch:   7,
		ExpectedTaskRevision: 10,
		IdempotencyKey:       key,
		RequestDigest:        confirmation.Digest(strings.Repeat("f", 64)),
	}
	digest := confirmation.RequestDigestForRelease(command)
	reservation, err := json.Marshal(map[string]any{
		"task_id":       testConfirmationTaskID,
		"attempt":       2,
		"lease_epoch":   7,
		"task_revision": 9,
	})
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	expectConfirmationIdentity(mock, "")
	expectConfirmationReplayMiss(mock, testConfirmationOwner, "release", key)
	expectConfirmationTaskLock(mock, "succeeded", 2, 7, 10, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StateConsumed, 4, at.Add(-time.Hour), at.Add(-time.Minute), at.Add(time.Hour), ""))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT reservation_json FROM agent_confirmations")).
		WithArgs(testConfirmationID, testConfirmationOwner).
		WillReturnRows(sqlmock.NewRows([]string{"reservation_json"}).AddRow(reservation))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET reservation_json=NULL")).
		WithArgs(sqlmock.AnyArg(), testConfirmationID, testConfirmationOwner, int64(4)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StateConsumed, 5, at.Add(-time.Hour), at, at.Add(time.Hour), ""))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmation_replays")).
		WithArgs(testConfirmationOwner, "release", key, string(digest), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	out, err := store.ReleaseReservation(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != confirmation.StateConsumed || out.Revision != 5 {
		t.Fatalf("unexpected released confirmation: %+v", out)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationListBindsCursorToOwnerAndFilters(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	base := time.Date(2026, 7, 29, 13, 0, 0, 0, time.UTC)
	rows := sqlmock.NewRows([]string{
		"confirmation_id", "owner_id", "operation_domain", "target_id", "target_revision",
		"binding_digest", "binding_json", "task_id", "state", "revision", "created_at",
		"updated_at", "expires_at", "terminal_reason",
	})
	raw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	for i, id := range []string{
		"11111111-1111-4111-8111-111111111111",
		"11111111-1111-4111-8111-111111111112",
		"11111111-1111-4111-8111-111111111113",
	} {
		createdAt := base.Add(time.Duration(i) * time.Minute)
		rows.AddRow(id, testConfirmationOwner, binding.OperationDomain, binding.TargetID, binding.TargetRevision, binding.Digest, raw, testConfirmationTaskID, string(confirmation.StatePending), 1, createdAt, createdAt, base.Add(time.Hour), "")
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, binding.OperationDomain, binding.TargetID, sqlmock.AnyArg(), false, sqlmock.AnyArg(), "00000000-0000-0000-0000-000000000000", 3).
		WillReturnRows(rows)

	page, err := store.List(t.Context(), confirmation.ListQuery{
		OwnerID:  testConfirmationOwner,
		PageSize: 2,
		Domain:   binding.OperationDomain,
		TargetID: binding.TargetID,
		States:   []confirmation.State{confirmation.StatePending},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Confirmations) != 2 || page.NextPageToken == "" {
		t.Fatalf("unexpected first page: %+v", page)
	}
	_, err = store.List(t.Context(), confirmation.ListQuery{
		OwnerID:   testConfirmationOwner,
		PageSize:  2,
		PageToken: page.NextPageToken,
		Domain:    binding.OperationDomain,
		TargetID:  "different-target",
		States:    []confirmation.State{confirmation.StatePending},
	})
	if !errors.Is(err, confirmation.ErrInvalid) {
		t.Fatalf("expected filter-bound token rejection, got %v", err)
	}
	_, err = store.List(t.Context(), confirmation.ListQuery{
		OwnerID:   "@other:example.test",
		PageSize:  2,
		PageToken: page.NextPageToken,
		Domain:    binding.OperationDomain,
		TargetID:  binding.TargetID,
		States:    []confirmation.State{confirmation.StatePending},
	})
	if !errors.Is(err, confirmation.ErrInvalid) {
		t.Fatalf("expected owner-bound token rejection, got %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTerminalizeConfirmationsClearsConsumedReservations(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	at := time.Date(2026, 7, 29, 14, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).
		WithArgs(testConfirmationTaskID, "failed", at).
		WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()
	tx, err := db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = terminalizeConfirmationsTx(t.Context(), tx, testConfirmationTaskID, "failed", at); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
