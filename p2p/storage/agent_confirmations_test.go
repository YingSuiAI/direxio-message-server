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

func expectConfirmationReplayMiss(mock sqlmock.Sqlmock, owner, operation, key string) {
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(owner + "\x00" + operation + "\x00" + key).
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
		WithArgs(testConfirmationOwner + "\x00confirm\x00" + key).
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
	mock.ExpectRollback()

	_, err = store.Consume(t.Context(), command)
	if !errors.Is(err, confirmation.ErrExpired) {
		t.Fatalf("expected expired rejection, got %v", err)
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
		WithArgs(testConfirmationOwner + "\x00consume\x00" + key).
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
