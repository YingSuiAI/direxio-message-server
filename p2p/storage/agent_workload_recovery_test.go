package storage

import (
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
)

const (
	testWorkloadOwnerID        = "@owner:example.test"
	testWorkloadOperationID    = "11111111-1111-4111-8111-111111111111"
	testWorkloadID             = "22222222-2222-4222-8222-222222222222"
	testWorkloadPlanID         = "33333333-3333-4333-8333-333333333333"
	testWorkloadTaskID         = "44444444-4444-4444-8444-444444444444"
	testWorkloadConfirmationID = "55555555-5555-4555-8555-555555555555"
)

func TestPostgresWorkloadListsRejectMalformedUUIDPageTokens(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentWorkloadStore(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testWorkloadOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.ListWorkloads(t.Context(), 20, "not-a-uuid"); !errors.Is(err, workload.ErrInvalid) {
		t.Fatalf("ListWorkloads error = %v, want ErrInvalid", err)
	}
	if _, _, err := repo.ListPlans(t.Context(), 20, "not-a-uuid"); !errors.Is(err, workload.ErrInvalid) {
		t.Fatalf("ListPlans error = %v, want ErrInvalid", err)
	}
}

func TestPostgresWorkloadRecoverClaimPromotesConsumedReservation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentWorkloadStore(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testWorkloadOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	taskExpiry := now.Add(time.Hour)
	fence := workload.TaskFence{
		TaskID:     testWorkloadTaskID,
		Attempt:    3,
		LeaseEpoch: 4,
		Revision:   11,
		Holder:     "worker-new",
		ExpiresAt:  taskExpiry,
	}
	oldReservation := []byte(`{"task_id":"44444444-4444-4444-8444-444444444444","attempt":3,"lease_epoch":3,"task_revision":8,"active":true}`)
	newReservation := []byte(`{"task_id":"44444444-4444-4444-8444-444444444444","attempt":3,"lease_epoch":4,"task_revision":12,"active":true}`)

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,dispatch_state,task_id::text,confirmation_id::text,revision,dispatch_epoch,dispatch_claim,dispatch_lease_until FROM core_workload_operations")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "dispatch_state", "task_id", "confirmation_id", "revision", "dispatch_epoch", "dispatch_claim", "dispatch_lease_until"}).
			AddRow("running", "dispatched", testWorkloadTaskID, testWorkloadConfirmationID, 5, 3, "worker-old", now.Add(time.Minute)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,revision,attempt,lease_epoch,lease_holder,lease_expires_at FROM agent_tasks")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "revision", "attempt", "lease_epoch", "lease_holder", "lease_expires_at"}).
			AddRow("running", 12, 3, 4, "worker-new", taskExpiry))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state,revision,reservation_json FROM agent_confirmations")).
		WillReturnRows(sqlmock.NewRows([]string{"state", "revision", "reservation_json"}).
			AddRow("consumed", 7, oldReservation))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workload_operations SET dispatch_state='uncertain'")).
		WithArgs("claim-new", int64(4), sqlmock.AnyArg(), sqlmock.AnyArg(), testWorkloadOwnerID, testWorkloadOperationID, int64(5), int64(3)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET reservation_json=")).
		WithArgs(newReservation, sqlmock.AnyArg(), testWorkloadOwnerID, testWorkloadConfirmationID, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,workload_id::text,plan_id::text,operation,plan_revision,plan_digest,target_kind,task_id::text,confirmation_id::text,status,revision,failure_code,failure_summary,created_at,updated_at,dispatch_state,dispatch_attempt,dispatch_epoch,dispatch_claim,dispatch_lease_until,completion_fingerprint FROM core_workload_operations")).
		WillReturnRows(sqlmock.NewRows([]string{
			"operation_id", "workload_id", "plan_id", "operation", "plan_revision", "plan_digest", "target_kind",
			"task_id", "confirmation_id", "status", "revision", "failure_code", "failure_summary", "created_at", "updated_at",
			"dispatch_state", "dispatch_attempt", "dispatch_epoch", "dispatch_claim", "dispatch_lease_until", "completion_fingerprint",
		}).AddRow(
			testWorkloadOperationID, testWorkloadID, testWorkloadPlanID, "apply", 1, "digest", "AWS_EC2_SSM",
			testWorkloadTaskID, testWorkloadConfirmationID, "running", 6, "", "", now, now,
			"uncertain", 3, 4, "claim-new", taskExpiry, "",
		))

	got, err := repo.RecoverClaimFenced(t.Context(), testWorkloadOperationID, "claim-new", fence)
	if err != nil {
		t.Fatal(err)
	}
	if got.DispatchState != "uncertain" || got.DispatchEpoch != fence.LeaseEpoch || got.DispatchClaim != "claim-new" {
		t.Fatalf("unexpected recovered operation: %+v", got)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresWorkloadRecoverClaimRejectsDifferentLogicalAttempt(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentWorkloadStore(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testWorkloadOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fence := workload.TaskFence{
		TaskID:     testWorkloadTaskID,
		Attempt:    3,
		LeaseEpoch: 4,
		Revision:   11,
		Holder:     "worker-new",
		ExpiresAt:  now.Add(time.Hour),
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,dispatch_state,task_id::text,confirmation_id::text,revision,dispatch_epoch,dispatch_claim,dispatch_lease_until FROM core_workload_operations")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "dispatch_state", "task_id", "confirmation_id", "revision", "dispatch_epoch", "dispatch_claim", "dispatch_lease_until"}).
			AddRow("running", "dispatched", testWorkloadTaskID, testWorkloadConfirmationID, 5, 3, "worker-old", now.Add(time.Minute)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,revision,attempt,lease_epoch,lease_holder,lease_expires_at FROM agent_tasks")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "revision", "attempt", "lease_epoch", "lease_holder", "lease_expires_at"}).
			AddRow("running", 12, 3, 4, "worker-new", now.Add(time.Hour)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state,revision,reservation_json FROM agent_confirmations")).
		WillReturnRows(sqlmock.NewRows([]string{"state", "revision", "reservation_json"}).
			AddRow("consumed", 7, []byte(`{"task_id":"44444444-4444-4444-8444-444444444444","attempt":2,"lease_epoch":3,"task_revision":8,"active":true}`)))
	mock.ExpectRollback()

	_, err = repo.RecoverClaimFenced(t.Context(), testWorkloadOperationID, "claim-new", fence)
	if !errors.Is(err, workload.ErrRevisionConflict) {
		t.Fatalf("expected revision conflict, got %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresWorkloadRenewDispatchLeaseLocksGenericTaskFence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentWorkloadStore(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testWorkloadOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	taskExpiry := now.Add(time.Hour)
	fence := workload.TaskFence{
		TaskID:     testWorkloadTaskID,
		Attempt:    1,
		LeaseEpoch: 2,
		Revision:   4,
		Holder:     "worker",
		ExpiresAt:  now.Add(-time.Minute),
	}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,revision,lease_epoch,attempt,lease_holder,lease_expires_at FROM agent_tasks")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "revision", "lease_epoch", "attempt", "lease_holder", "lease_expires_at"}).
			AddRow("running", 6, 2, 1, "worker", taskExpiry))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workload_operations SET dispatch_lease_until=")).
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), testWorkloadOwnerID, testWorkloadOperationID, testWorkloadTaskID, "dispatch-claim", uint64(2)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,workload_id::text,plan_id::text,operation,plan_revision,plan_digest,target_kind,task_id::text,confirmation_id::text,status,revision,failure_code,failure_summary,created_at,updated_at,dispatch_state,dispatch_attempt,dispatch_epoch,dispatch_claim,dispatch_lease_until,completion_fingerprint FROM core_workload_operations")).
		WillReturnRows(sqlmock.NewRows([]string{
			"operation_id", "workload_id", "plan_id", "operation", "plan_revision", "plan_digest", "target_kind",
			"task_id", "confirmation_id", "status", "revision", "failure_code", "failure_summary", "created_at", "updated_at",
			"dispatch_state", "dispatch_attempt", "dispatch_epoch", "dispatch_claim", "dispatch_lease_until", "completion_fingerprint",
		}).AddRow(
			testWorkloadOperationID, testWorkloadID, testWorkloadPlanID, "apply", 1, "digest", "AWS_EC2_SSM",
			testWorkloadTaskID, testWorkloadConfirmationID, "running", 6, "", "", now, now,
			"dispatched", 1, 2, "dispatch-claim", taskExpiry, "",
		))

	got, err := repo.RenewDispatchLeaseFenced(t.Context(), testWorkloadOperationID, "dispatch-claim", 2, fence)
	if err != nil {
		t.Fatal(err)
	}
	if !got.DispatchLeaseUntil.Equal(taskExpiry) {
		t.Fatalf("dispatch lease = %s, want generic task lease %s", got.DispatchLeaseUntil, taskExpiry)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresWorkloadConsumeAllowsLeaseRenewalRevision(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentWorkloadStore(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testWorkloadOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	fence := workload.TaskFence{
		TaskID:     testWorkloadTaskID,
		Attempt:    1,
		LeaseEpoch: 1,
		Revision:   4,
		Holder:     "worker",
		ExpiresAt:  now.Add(time.Hour),
	}
	wantErr := errors.New("confirmation read failed")

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,revision,attempt,lease_epoch,lease_holder,lease_expires_at FROM agent_tasks")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "revision", "attempt", "lease_epoch", "lease_holder", "lease_expires_at"}).
			AddRow("running", 5, 1, 1, "worker", now.Add(time.Hour)))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workload_operations SET status='running'")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision FROM agent_confirmations")).
		WillReturnError(wantErr)
	mock.ExpectRollback()

	_, _, err = repo.ConsumeFenced(t.Context(), testWorkloadOperationID, testWorkloadConfirmationID, "digest", 2, fence)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected confirmation read error after accepting renewed revision, got %v", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
