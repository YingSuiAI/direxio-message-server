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
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

const (
	testTaskOwnerA      = "@owner-a:example.test"
	testTaskOwnerB      = "@owner-b:example.test"
	testTaskID          = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	testTaskProfileID   = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	testTaskOriginalKey = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

func testDatabaseTaskSpec(t *testing.T, idempotencyKey string, availableAt time.Time) task.TaskSpec {
	t.Helper()
	spec, err := (task.TaskSpec{
		Kind:           task.TaskKindAgent,
		Goal:           "durable task replay",
		ModelProfileID: testTaskProfileID,
		IdempotencyKey: idempotencyKey,
		AvailableAt:    availableAt.UTC(),
	}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	return spec
}

type testDatabaseTaskRow struct {
	Owner          string
	ID             string
	Spec           task.TaskSpec
	Status         task.Status
	Attempt        int
	LeaseEpoch     int64
	Revision       int64
	Progress       int64
	AvailableAt    time.Time
	Holder         string
	LeaseExpiresAt any
	FailureCode    string
	FailureSummary string
	RetryOf        any
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

func testDatabaseTaskRows(t *testing.T, value testDatabaseTaskRow) *sqlmock.Rows {
	t.Helper()
	raw, err := json.Marshal(value.Spec)
	if err != nil {
		t.Fatal(err)
	}
	return sqlmock.NewRows([]string{
		"owner_id", "spec_json", "status", "attempt", "lease_epoch", "revision",
		"progress_sequence", "available_at", "lease_holder", "lease_expires_at",
		"result_json", "failure_code", "failure_summary", "execution_started_at",
		"execution_deadline_at", "retry_of_task_id", "created_at", "updated_at", "deleted_at",
	}).AddRow(
		value.Owner, raw, string(value.Status), value.Attempt, value.LeaseEpoch,
		value.Revision, value.Progress, value.AvailableAt, value.Holder,
		value.LeaseExpiresAt, nil, value.FailureCode, value.FailureSummary, nil,
		nil, value.RetryOf, value.CreatedAt, value.UpdatedAt, nil,
	)
}

func expectTaskReplayMiss(mock sqlmock.Sqlmock, owner, operation, key string) {
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-task", owner, operation, key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_task_replays")).
		WithArgs(owner, operation, key).
		WillReturnError(sql.ErrNoRows)
}

func TestDatabaseTaskCreateSerializesAndReplaysFullSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseTaskStore(db)
	availableAt := time.Date(2026, 7, 29, 14, 0, 0, 0, time.UTC)
	createdAt := availableAt.Add(-time.Minute)
	key := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	digest := strings.Repeat("a", 64)
	spec := testDatabaseTaskSpec(t, key, availableAt)
	command := task.CreateTaskCommand{
		OwnerID: testTaskOwnerA,
		Spec:    spec,
		Mutation: task.MutationCommand{
			IdempotencyKey: key,
			RequestDigest:  digest,
		},
	}
	id := deterministicDatabaseTaskID(testTaskOwnerA, key)
	expected := task.Task{
		OwnerID: testTaskOwnerA, ID: id, Spec: spec, Status: task.StatusQueued,
		Revision: 1, AvailableAt: availableAt, CreatedAt: createdAt, UpdatedAt: createdAt,
	}
	responseJSON, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	specJSON, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	expectTaskReplayMiss(mock, testTaskOwnerA, "create", key)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,available_at,created_at,updated_at)")).
		WithArgs(id, testTaskOwnerA, specJSON, availableAt, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(databaseTaskSelect+" WHERE task_id=$1 AND owner_id=$2")).
		WithArgs(id, testTaskOwnerA).
		WillReturnRows(testDatabaseTaskRows(t, testDatabaseTaskRow{
			Owner: testTaskOwnerA, ID: id, Spec: spec, Status: task.StatusQueued,
			Revision: 1, AvailableAt: availableAt, CreatedAt: createdAt, UpdatedAt: createdAt,
		}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_replays(owner_id,operation,idempotency_key,request_digest,response_json,created_at)")).
		WithArgs(testTaskOwnerA, "create", key, digest, responseJSON, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, err := store.CreateTask(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotJSON) != string(responseJSON) {
		t.Fatalf("create response is not the scanned snapshot:\n got %s\nwant %s", gotJSON, responseJSON)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// A same-key caller serializes behind the transaction-level advisory lock
	// and receives the original immutable response without reading task state.
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-task", testTaskOwnerA, "create", key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_task_replays")).
		WithArgs(testTaskOwnerA, "create", key).
		WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(digest, responseJSON))
	mock.ExpectRollback()
	replayed, err := store.CreateTask(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	replayedJSON, err := json.Marshal(replayed)
	if err != nil {
		t.Fatal(err)
	}
	if string(replayedJSON) != string(responseJSON) {
		t.Fatalf("lost-response replay changed snapshot:\n got %s\nwant %s", replayedJSON, responseJSON)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// Digest conflicts are decided from replay state before mutable task state.
	conflict := command
	conflict.Mutation.RequestDigest = strings.Repeat("b", 64)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-task", testTaskOwnerA, "create", key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_task_replays")).
		WithArgs(testTaskOwnerA, "create", key).
		WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(digest, responseJSON))
	mock.ExpectRollback()
	if _, err = store.CreateTask(t.Context(), conflict); !errors.Is(err, task.ErrConflict) {
		t.Fatalf("expected create digest conflict before task state read, got %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseTaskCancelPersistsAtomicReplayBeforeMutableChecks(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseTaskStore(db)
	at := time.Date(2026, 7, 29, 15, 0, 0, 0, time.UTC)
	availableAt := at.Add(-time.Hour)
	key := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	spec := testDatabaseTaskSpec(t, testTaskOriginalKey, availableAt)
	command := task.CancelCommand{
		OwnerID:          testTaskOwnerA,
		TaskID:           testTaskID,
		ExpectedRevision: 4,
		Mutation: task.MutationCommand{
			IdempotencyKey:   key,
			RequestDigest:    strings.Repeat("f", 64),
			ExpectedRevision: 4,
		},
		Reason: "owner canceled",
		At:     at,
	}
	digest := taskCancelRequestDigest(command)

	mock.ExpectBegin()
	expectTaskReplayMiss(mock, testTaskOwnerA, "cancel", key)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_id,COALESCE(provision_id::text,'') FROM core_aws_changes WHERE owner_id=$1 AND task_id=$2")).
		WithArgs(testTaskOwnerA, testTaskID).
		WillReturnRows(sqlmock.NewRows([]string{"owner_id", "provision_id"}))
	mock.ExpectQuery(regexp.QuoteMeta(databaseTaskSelect+` WHERE task_id=$1 AND owner_id=$2 FOR UPDATE`)).
		WithArgs(testTaskID, testTaskOwnerA).
		WillReturnRows(testDatabaseTaskRows(t, testDatabaseTaskRow{
			Owner: testTaskOwnerA, ID: testTaskID, Spec: spec, Status: task.StatusRunning,
			Attempt: 1, LeaseEpoch: 7, Revision: 4, Progress: 2, AvailableAt: availableAt,
			Holder: "worker-a", LeaseExpiresAt: at.Add(time.Hour), CreatedAt: availableAt,
			UpdatedAt: at.Add(-time.Minute),
		}))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_tasks SET status='canceled', failure_code='canceled'")).
		WithArgs(command.Reason, at, testTaskID, testTaskOwnerA, uint64(4), string(task.StatusRunning)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_task_runtime_concurrency SET")).
		WithArgs(at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET state=CASE WHEN state IN ('pending','confirmed') THEN 'expired' ELSE state END")).
		WithArgs(testTaskID, "canceled", at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).
		WithArgs(testTaskID, command.Reason, at, testTaskOwnerA).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(databaseTaskSelect+` WHERE task_id=$1 AND owner_id=$2`)).
		WithArgs(testTaskID, testTaskOwnerA).
		WillReturnRows(testDatabaseTaskRows(t, testDatabaseTaskRow{
			Owner: testTaskOwnerA, ID: testTaskID, Spec: spec, Status: task.StatusCanceled,
			Attempt: 1, LeaseEpoch: 8, Revision: 5, Progress: 3, AvailableAt: availableAt,
			FailureCode: "canceled", FailureSummary: command.Reason, CreatedAt: availableAt,
			UpdatedAt: at,
		}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_replays")).
		WithArgs(testTaskOwnerA, "cancel", key, digest, sqlmock.AnyArg(), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	first, err := store.CancelTask(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != task.StatusCanceled || first.Revision != 5 || first.LeaseEpoch != 8 {
		t.Fatalf("unexpected canceled task: %+v", first)
	}
	replayJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}

	retry := command
	retry.At = at.Add(time.Minute)
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-task", testTaskOwnerA, "cancel", key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_task_replays")).
		WithArgs(testTaskOwnerA, "cancel", key).
		WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(digest, replayJSON))
	mock.ExpectRollback()
	second, err := store.CancelTask(t.Context(), retry)
	if err != nil {
		t.Fatal(err)
	}
	if second.UpdatedAt != first.UpdatedAt || second.Revision != first.Revision {
		t.Fatalf("lost-response replay changed task: first=%+v second=%+v", first, second)
	}

	conflict := retry
	conflict.Reason = "different reason"
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-task", testTaskOwnerA, "cancel", key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_task_replays")).
		WithArgs(testTaskOwnerA, "cancel", key).
		WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(digest, replayJSON))
	mock.ExpectRollback()
	_, err = store.CancelTask(t.Context(), conflict)
	if !errors.Is(err, task.ErrConflict) {
		t.Fatalf("expected same-key digest conflict, got %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestDatabaseTaskRetryPersistsSuccessorEventAndReplayAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewDatabaseTaskStore(db)
	at := time.Date(2026, 7, 29, 16, 0, 0, 0, time.UTC)
	availableAt := at.Add(-time.Hour)
	key := "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	originalSpec := testDatabaseTaskSpec(t, testTaskOriginalKey, availableAt)
	command := task.RetryCommand{
		TaskID: testTaskID,
		Mutation: task.MutationCommand{
			IdempotencyKey:   key,
			RequestDigest:    strings.Repeat("f", 64),
			ExpectedRevision: 4,
		},
		At: at,
	}
	digest := taskRetryRequestDigest(command)
	original := task.Task{
		OwnerID: testTaskOwnerA, ID: testTaskID, Spec: originalSpec, Status: task.StatusCanceled,
		Attempt: 1, LeaseEpoch: 8, Revision: 4, ProgressSequence: 3,
		AvailableAt: availableAt, CreatedAt: availableAt, UpdatedAt: at.Add(-time.Minute),
		FailureCode: "canceled", FailureSummary: "owner canceled",
	}
	next, err := task.RetryTask(original, task.RetryRequest{
		TaskID: testTaskID, IdempotencyKey: key, RequestDigest: digest,
		ExpectedRevision: 4, At: at,
	})
	if err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_id FROM agent_tasks WHERE task_id=$1")).
		WithArgs(testTaskID).
		WillReturnRows(sqlmock.NewRows([]string{"owner_id"}).AddRow(testTaskOwnerA))
	expectTaskReplayMiss(mock, testTaskOwnerA, "retry", key)
	mock.ExpectQuery(regexp.QuoteMeta(databaseTaskSelect+` WHERE task_id=$1 AND owner_id=$2 FOR UPDATE`)).
		WithArgs(testTaskID, testTaskOwnerA).
		WillReturnRows(testDatabaseTaskRows(t, testDatabaseTaskRow{
			Owner: testTaskOwnerA, ID: testTaskID, Spec: originalSpec, Status: task.StatusCanceled,
			Attempt: 1, LeaseEpoch: 8, Revision: 4, Progress: 3, AvailableAt: availableAt,
			FailureCode: "canceled", FailureSummary: "owner canceled", CreatedAt: availableAt,
			UpdatedAt: at.Add(-time.Minute),
		}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_tasks")).
		WithArgs(next.ID, testTaskOwnerA, sqlmock.AnyArg(), next.AvailableAt, testTaskID, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).
		WithArgs(testTaskOwnerA, next.ID, testTaskID, at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(databaseTaskSelect+` WHERE task_id=$1 AND owner_id=$2`)).
		WithArgs(next.ID, testTaskOwnerA).
		WillReturnRows(testDatabaseTaskRows(t, testDatabaseTaskRow{
			Owner: testTaskOwnerA, ID: next.ID, Spec: next.Spec, Status: task.StatusQueued,
			Attempt: 0, LeaseEpoch: 0, Revision: 1, Progress: 1, AvailableAt: at,
			RetryOf: testTaskID, CreatedAt: at, UpdatedAt: at,
		}))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_replays")).
		WithArgs(testTaskOwnerA, "retry", key, digest, sqlmock.AnyArg(), at).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	first, err := store.RetryTask(t.Context(), command)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != next.ID || first.OwnerID != testTaskOwnerA || first.ProgressSequence != 1 || first.RetryOfTaskID != testTaskID {
		t.Fatalf("unexpected retry successor: %+v", first)
	}
	replayJSON, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}

	retry := command
	retry.At = at.Add(time.Minute)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_id FROM agent_tasks WHERE task_id=$1")).
		WithArgs(testTaskID).
		WillReturnRows(sqlmock.NewRows([]string{"owner_id"}).AddRow(testTaskOwnerA))
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-task", testTaskOwnerA, "retry", key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_task_replays")).
		WithArgs(testTaskOwnerA, "retry", key).
		WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(digest, replayJSON))
	mock.ExpectRollback()
	second, err := store.RetryTask(t.Context(), retry)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID != first.ID || second.CreatedAt != first.CreatedAt {
		t.Fatalf("retry replay changed successor: first=%+v second=%+v", first, second)
	}

	conflict := retry
	conflict.Mutation.ExpectedRevision = 5
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_id FROM agent_tasks WHERE task_id=$1")).
		WithArgs(testTaskID).
		WillReturnRows(sqlmock.NewRows([]string{"owner_id"}).AddRow(testTaskOwnerA))
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1,0))")).
		WithArgs(canonicalAdvisoryLockIdentity("agent-task", testTaskOwnerA, "retry", key)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,response_json FROM agent_task_replays")).
		WithArgs(testTaskOwnerA, "retry", key).
		WillReturnRows(sqlmock.NewRows([]string{"request_digest", "response_json"}).AddRow(digest, replayJSON))
	mock.ExpectRollback()
	_, err = store.RetryTask(t.Context(), conflict)
	if !errors.Is(err, task.ErrConflict) {
		t.Fatalf("expected retry digest conflict before mutable task read, got %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetryTaskDeterministicIDIncludesOwner(t *testing.T) {
	at := time.Date(2026, 7, 29, 17, 0, 0, 0, time.UTC)
	key := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	spec := testDatabaseTaskSpec(t, testTaskOriginalKey, at.Add(-time.Hour))
	request := task.RetryRequest{
		TaskID: testTaskID, IdempotencyKey: key, RequestDigest: strings.Repeat("a", 64),
		ExpectedRevision: 4, At: at,
	}
	base := task.Task{
		ID: testTaskID, Spec: spec, Status: task.StatusCanceled, Revision: 4,
		AvailableAt: at.Add(-time.Hour), CreatedAt: at.Add(-time.Hour), UpdatedAt: at,
	}
	left := base
	left.OwnerID = testTaskOwnerA
	right := base
	right.OwnerID = testTaskOwnerB
	first, err := task.RetryTask(left, request)
	if err != nil {
		t.Fatal(err)
	}
	again, err := task.RetryTask(left, request)
	if err != nil {
		t.Fatal(err)
	}
	otherOwner, err := task.RetryTask(right, request)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != again.ID {
		t.Fatalf("same owner/input was not deterministic: %s != %s", first.ID, again.ID)
	}
	if first.ID == otherOwner.ID {
		t.Fatalf("retry IDs collided across owners: %s", first.ID)
	}
}

func TestCreateTaskDeterministicIDIncludesOwner(t *testing.T) {
	key := "ffffffff-ffff-4fff-8fff-ffffffffffff"
	first := deterministicDatabaseTaskID(testTaskOwnerA, key)
	again := deterministicDatabaseTaskID(testTaskOwnerA, key)
	otherOwner := deterministicDatabaseTaskID(testTaskOwnerB, key)
	if first != again {
		t.Fatalf("same owner/idempotency key was not deterministic: %s != %s", first, again)
	}
	if first == otherOwner {
		t.Fatalf("create IDs collided for two owners sharing idempotency key %s: %s", key, first)
	}
}
