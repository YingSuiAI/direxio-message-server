package storage

import (
	"context"
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

func testExecutionV2ConfirmationBinding(t *testing.T) confirmation.Binding {
	t.Helper()
	digest := confirmation.Digest(strings.Repeat("a", 64))
	binding, err := (confirmation.Binding{
		OwnerID: testConfirmationOwner, OperationDomain: "execution:v2:remote_execution", TargetID: testConfirmationTarget, TargetRevision: 3, TargetKind: "aws_ec2_instance",
		ContentDigest: digest, ExecutionDigest: digest, ParameterDigest: digest, NetworkDigest: digest, SecretGrantDigest: digest,
		PlanID: "44444444-4444-4444-8444-444444444444", PlanRevision: 2, PlanDigest: digest,
		DeploymentID: "55555555-5555-4555-8555-555555555555", RunID: "66666666-6666-4666-8666-666666666666", RunRevision: 3,
		StageID: "77777777-7777-4777-8777-777777777777", StageRevision: 1, StageDigest: digest, TargetDigest: digest,
		ArtifactSetDigest: digest, PolicyDigest: digest, CostQuoteDigest: digest, RollbackDigest: digest, PreviewDigest: digest,
		RiskLevel: "R2", GateType: "remote_execution", StageIdempotencyKey: "88888888-8888-4888-8888-888888888888",
		BindingExpiresAt: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return binding
}

func TestDatabaseExpireAtExecutionV2FailsBeforeTerminalMutation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := testExecutionV2ConfirmationBinding(t)
	at := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 0, 0, 1, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, b, confirmation.StatePending, 1, at.Add(-time.Hour), at.Add(-time.Hour), at.Add(-time.Minute), ""))
	mock.ExpectRollback()
	if err := NewDatabaseConfirmationStore(db).ExpireAt(context.Background(), testConfirmationOwner, testConfirmationID, at); !errors.Is(err, confirmation.ErrConflict) {
		t.Fatalf("ExpireAt() = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseExpireOverdueExecutionV2FailsBeforeTerminalMutation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	b := testExecutionV2ConfirmationBinding(t)
	at := time.Date(2031, 1, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text\nFROM agent_confirmations")).WithArgs(testConfirmationOwner, "", "", at, maxOverdueSweepCandidates).WillReturnRows(sqlmock.NewRows([]string{"confirmation_id"}).AddRow(testConfirmationID))
	mock.ExpectBegin()
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 0, 0, 1, nil)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, b, confirmation.StatePending, 1, at.Add(-time.Hour), at.Add(-time.Hour), at.Add(-time.Minute), ""))
	mock.ExpectRollback()
	if err := NewDatabaseConfirmationStore(db).ExpireOverdue(context.Background(), testConfirmationOwner, at); !errors.Is(err, confirmation.ErrConflict) {
		t.Fatalf("ExpireOverdue() = %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationGenericV2ConfirmAndConsumeFailClosed(t *testing.T) {
	t.Run("confirm", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		binding := testExecutionV2ConfirmationBinding(t)
		at := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
		command := confirmation.ConfirmCommand{OwnerID: testConfirmationOwner, ConfirmationID: testConfirmationID, IdempotencyKey: "99999999-9999-4999-8999-999999999999", ExpectedRevision: 1, Binding: binding, At: at}
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, at.Add(-time.Minute), at.Add(-time.Minute), binding.BindingExpiresAt, ""))
		mock.ExpectRollback()
		if _, err = NewDatabaseConfirmationStore(db).Confirm(t.Context(), command); !errors.Is(err, confirmation.ErrConflict) {
			t.Fatalf("Confirm() error = %v, want ErrConflict", err)
		}
		if err = mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("consume", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		binding := testExecutionV2ConfirmationBinding(t)
		at := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
		command := confirmation.ConsumeCommand{OwnerID: testConfirmationOwner, ConfirmationID: testConfirmationID, IdempotencyKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", TaskID: testConfirmationTaskID, Attempt: 2, LeaseEpoch: 7, ExpectedRevision: 3, ExpectedTaskRevision: 9, Binding: binding, At: at}
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StateConfirmed, 3, at.Add(-time.Minute), at.Add(-time.Minute), binding.BindingExpiresAt, ""))
		mock.ExpectRollback()
		if _, err = NewDatabaseConfirmationStore(db).Consume(t.Context(), command); !errors.Is(err, confirmation.ErrConflict) {
			t.Fatalf("Consume() error = %v, want ErrConflict", err)
		}
		if err = mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("confirm replay hit", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		binding := testExecutionV2ConfirmationBinding(t)
		at := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
		command := confirmation.ConfirmCommand{OwnerID: testConfirmationOwner, ConfirmationID: testConfirmationID, IdempotencyKey: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", ExpectedRevision: 1, Binding: binding, At: at}
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, at.Add(-time.Minute), at.Add(-time.Minute), binding.BindingExpiresAt, ""))
		mock.ExpectRollback()
		if _, err = NewDatabaseConfirmationStore(db).Confirm(t.Context(), command); !errors.Is(err, confirmation.ErrConflict) {
			t.Fatalf("Confirm replay error = %v, want ErrConflict", err)
		}
		if err = mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("consume replay hit", func(t *testing.T) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		binding := testExecutionV2ConfirmationBinding(t)
		at := time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)
		command := confirmation.ConsumeCommand{OwnerID: testConfirmationOwner, ConfirmationID: testConfirmationID, IdempotencyKey: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", TaskID: testConfirmationTaskID, Attempt: 2, LeaseEpoch: 7, ExpectedRevision: 3, ExpectedTaskRevision: 9, Binding: binding, At: at}
		mock.ExpectBegin()
		mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StateConfirmed, 3, at.Add(-time.Minute), at.Add(-time.Minute), binding.BindingExpiresAt, ""))
		mock.ExpectRollback()
		if _, err = NewDatabaseConfirmationStore(db).Consume(t.Context(), command); !errors.Is(err, confirmation.ErrConflict) {
			t.Fatalf("Consume replay error = %v, want ErrConflict", err)
		}
		if err = mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
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

func expectConfirmationPreflight(t *testing.T, mock sqlmock.Sqlmock, binding confirmation.Binding, state confirmation.State, revision int64, createdAt, updatedAt, expiresAt time.Time, reason string) {
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, state, revision, createdAt, updatedAt, expiresAt, reason))
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StatePending, 1, at.Add(-time.Minute), at.Add(-time.Minute), expiresAt, "")
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StateConfirmed, 2, at.Add(-time.Minute), at, expiresAt, "")
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", testConfirmationOwner, "confirm", key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).
		WithArgs(testConfirmationOwner, "confirm", key).
		WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(string(digest), replayJSON))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StateConfirmed, 2, at.Add(-time.Minute), at, expiresAt, ""))
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StatePending, 1, at.Add(-time.Hour), at.Add(-time.Minute), at, "")
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StateExpired, 2, at.Add(-time.Hour), at, at, confirmation.ReasonExpired)
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StateExpired, 2, at.Add(-time.Hour), at, at, confirmation.ReasonExpired)
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StatePending, 1, at.Add(-time.Minute), at.Add(-time.Minute), expiresAt, "")
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StatePending, 1, at.Add(-time.Hour), at.Add(-time.Minute), at, "")
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StateExpired, 2, at.Add(-time.Hour), at, at, confirmation.ReasonExpired)
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StateExpired, 2, at.Add(-time.Hour), at, at, confirmation.ReasonExpired)
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StateConfirmed, 3, at.Add(-time.Hour), at.Add(-time.Minute), at, "")
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StateExpired, 4, at.Add(-time.Hour), at, at, confirmation.ReasonExpired)
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StateExpired, 4, at.Add(-time.Hour), at, at, confirmation.ReasonExpired)
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StateConfirmed, 3, at.Add(-time.Hour), at.Add(-time.Minute), at.Add(time.Hour), "")
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StateConsumed, 4, at.Add(-time.Hour), at, at.Add(time.Hour), "")
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-confirmation", testConfirmationOwner, "consume", key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_confirmation_replays")).
		WithArgs(testConfirmationOwner, "consume", key).
		WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(string(digest), replayJSON))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, testConfirmationID).
		WillReturnRows(testConfirmationRows(t, binding, confirmation.StateConsumed, 4, at.Add(-time.Hour), at, at.Add(time.Hour), ""))
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
	expectConfirmationPreflight(t, mock, binding, confirmation.StatePending, 1, at.Add(-time.Hour), at.Add(-time.Minute), at.Add(-time.Second), "")
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
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_id,task_id::text FROM agent_confirmations")).WithArgs(testConfirmationID, "").WillReturnRows(sqlmock.NewRows([]string{"owner_id", "task_id"}).AddRow(testConfirmationOwner, testConfirmationTaskID))
	expectConfirmationPreflight(t, mock, binding, confirmation.StateConsumed, 4, at.Add(-time.Hour), at.Add(-time.Minute), at.Add(time.Hour), "")
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
	base := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
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
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text FROM agent_confirmations")).WithArgs(testConfirmationOwner, binding.OperationDomain, binding.TargetID, sqlmock.AnyArg(), 3).WillReturnRows(sqlmock.NewRows([]string{"confirmation_id"}))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).
		WithArgs(testConfirmationOwner, binding.OperationDomain, binding.TargetID, sqlmock.AnyArg(), false, sqlmock.AnyArg(), "00000000-0000-0000-0000-000000000000", 3, sqlmock.AnyArg()).
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

func TestDatabaseConfirmationGetLazilyExpiresOverdueCard(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Now().UTC()
	stale := at.Add(-time.Hour)
	getQuery := regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")
	mock.ExpectQuery(getQuery).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, stale, stale, stale, ""))
	mock.ExpectBegin()
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 0, 0, 4, nil)
	mock.ExpectQuery(getQuery).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, stale, stale, stale, ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='expired'")).WithArgs(confirmation.ReasonExpired, sqlmock.AnyArg(), testConfirmationID, testConfirmationOwner, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='failed'")).WithArgs(confirmation.ReasonExpired, sqlmock.AnyArg(), testConfirmationTaskID, testConfirmationOwner, "waiting_user").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).WithArgs(testConfirmationTaskID, confirmation.ReasonExpired, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).WithArgs(confirmation.ReasonExpired, sqlmock.AnyArg(), testConfirmationTaskID, testConfirmationOwner).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(getQuery).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StateExpired, 2, stale, at, stale, confirmation.ReasonExpired))
	out, err := store.GetForOwner(t.Context(), testConfirmationOwner, testConfirmationID)
	if err != nil {
		t.Fatal(err)
	}
	if out.State != confirmation.StateExpired || out.Revision != 2 || out.TerminalReason != confirmation.ReasonExpired {
		t.Fatalf("lazy expiration result = %+v", out)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationListLazilyExpiresOverduePendingFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	at := time.Now().UTC()
	stale := at.Add(-time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text FROM agent_confirmations")).WithArgs(testConfirmationOwner, binding.OperationDomain, binding.TargetID, sqlmock.AnyArg(), 3).WillReturnRows(sqlmock.NewRows([]string{"confirmation_id"}).AddRow(testConfirmationID))
	mock.ExpectBegin()
	expectConfirmationIdentity(mock, testConfirmationOwner)
	expectConfirmationTaskLock(mock, "waiting_user", 0, 0, 4, nil)
	getQuery := regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")
	mock.ExpectQuery(getQuery).WithArgs(testConfirmationOwner, testConfirmationID).WillReturnRows(testConfirmationRows(t, binding, confirmation.StatePending, 1, stale, stale, stale, ""))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state='expired'")).WithArgs(confirmation.ReasonExpired, sqlmock.AnyArg(), testConfirmationID, testConfirmationOwner, int64(1)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='failed'")).WithArgs(confirmation.ReasonExpired, sqlmock.AnyArg(), testConfirmationTaskID, testConfirmationOwner, "waiting_user").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).WithArgs(testConfirmationTaskID, confirmation.ReasonExpired, sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).WithArgs(confirmation.ReasonExpired, sqlmock.AnyArg(), testConfirmationTaskID, testConfirmationOwner).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations")).WithArgs(testConfirmationOwner, binding.OperationDomain, binding.TargetID, sqlmock.AnyArg(), false, sqlmock.AnyArg(), "00000000-0000-0000-0000-000000000000", 3, sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"confirmation_id", "owner_id", "operation_domain", "target_id", "target_revision", "binding_digest", "binding_json", "task_id", "state", "revision", "created_at", "updated_at", "expires_at", "terminal_reason"}))
	page, err := store.List(t.Context(), confirmation.ListQuery{OwnerID: testConfirmationOwner, PageSize: 2, Domain: binding.OperationDomain, TargetID: binding.TargetID, States: []confirmation.State{confirmation.StatePending}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Confirmations) != 0 {
		t.Fatalf("overdue pending card remained in list: %+v", page.Confirmations)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseConfirmationListAppliesCutoffAfterBoundedSweep(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseConfirmationStore(db)
	binding := testConfirmationBinding(t)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text FROM agent_confirmations")).WithArgs(testConfirmationOwner, binding.OperationDomain, binding.TargetID, sqlmock.AnyArg(), 2).WillReturnRows(sqlmock.NewRows([]string{"confirmation_id"}))
	mainQuery := regexp.QuoteMeta("AND NOT (state IN ('pending','confirmed') AND expires_at <= $9)")
	mock.ExpectQuery(mainQuery).WithArgs(testConfirmationOwner, binding.OperationDomain, binding.TargetID, sqlmock.AnyArg(), false, sqlmock.AnyArg(), "00000000-0000-0000-0000-000000000000", 2, sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"confirmation_id", "owner_id", "operation_domain", "target_id", "target_revision", "binding_digest", "binding_json", "task_id", "state", "revision", "created_at", "updated_at", "expires_at", "terminal_reason"}))
	page, err := store.List(t.Context(), confirmation.ListQuery{OwnerID: testConfirmationOwner, PageSize: 1, Domain: binding.OperationDomain, TargetID: binding.TargetID, States: []confirmation.State{confirmation.StatePending}})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Confirmations) != 0 {
		t.Fatalf("cutoff-filtered list = %+v", page.Confirmations)
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
