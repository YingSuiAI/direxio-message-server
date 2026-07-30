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
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,operation_id) DO UPDATE SET next_sequence=core_workload_event_counters.next_sequence+1 RETURNING next_sequence-1")).
		WithArgs(testWorkloadOwnerID, testWorkloadOperationID).
		WillReturnRows(sqlmock.NewRows([]string{"next_sequence"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT workload_id::text FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2")).
		WithArgs(testWorkloadOwnerID, testWorkloadOperationID).
		WillReturnRows(sqlmock.NewRows([]string{"workload_id"}).AddRow(testWorkloadID))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO p2p_agent_deployment_event_cursors(owner_id,workload_id,last_sequence,updated_at)")).
		WithArgs(testWorkloadOwnerID, testWorkloadID, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_workload_events(owner_id,workload_id,operation_id,sequence,public_sequence,kind,status,message,readback_json,at)")).
		WithArgs(testWorkloadOwnerID, testWorkloadID, testWorkloadOperationID, uint64(2), int64(2), "recovery_claim", "running", "read-only recovery claimed dispatch fence", sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,workload_id::text,expected_workload_revision,plan_id::text,operation,plan_revision,plan_digest,target_kind,task_id::text,confirmation_id::text,status,revision,failure_code,failure_summary,created_at,updated_at,dispatch_state,dispatch_attempt,dispatch_epoch,dispatch_claim,dispatch_lease_until,completion_fingerprint FROM core_workload_operations")).
		WillReturnRows(sqlmock.NewRows([]string{
			"operation_id", "workload_id", "expected_workload_revision", "plan_id", "operation", "plan_revision", "plan_digest", "target_kind",
			"task_id", "confirmation_id", "status", "revision", "failure_code", "failure_summary", "created_at", "updated_at",
			"dispatch_state", "dispatch_attempt", "dispatch_epoch", "dispatch_claim", "dispatch_lease_until", "completion_fingerprint",
		}).AddRow(
			testWorkloadOperationID, testWorkloadID, 1, testWorkloadPlanID, "apply", 1, "digest", "AWS_EC2_SSM",
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

func TestPostgresWorkloadReconcileUncertainReleasesBoundMutationLease(t *testing.T) {
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
	provisionID := "77777777-7777-4777-8777-777777777777"
	planID := testWorkloadPlanID
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,dispatch_state,workload_id::text,plan_id::text,operation,completion_fingerprint FROM core_workload_operations")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "dispatch_state", "workload_id", "plan_id", "operation", "completion_fingerprint"}).AddRow("uncertain", "uncertain", testWorkloadID, planID, "apply", ""))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT l.provision_id::text,l.token::text,l.epoch,p.plan_id::text FROM core_aws_ec2_provision_mutation_leases")).
		WillReturnRows(sqlmock.NewRows([]string{"provision_id", "token", "epoch", "plan_id"}).AddRow(provisionID, "88888888-8888-4888-8888-888888888888", int64(4), planID))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_aws_ec2_provision_mutation_leases SET token=NULL,expires_at=NULL,state='active',operation_id=NULL")).
		WithArgs(sqlmock.AnyArg(), testWorkloadOwnerID, provisionID, testWorkloadOperationID, int64(4)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workload_operations SET status=$1,dispatch_state=$2")).
		WithArgs("succeeded", "terminal", "", "", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), testWorkloadOwnerID, testWorkloadOperationID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workloads SET state=$1,actual_snapshot_json=$2")).
		WithArgs("ready", sqlmock.AnyArg(), sqlmock.AnyArg(), testWorkloadOwnerID, testWorkloadID).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence)")).
		WillReturnRows(sqlmock.NewRows([]string{"next_sequence"}).AddRow(2))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT workload_id::text FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2")).
		WillReturnRows(sqlmock.NewRows([]string{"workload_id"}).AddRow(testWorkloadID))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO p2p_agent_deployment_event_cursors(owner_id,workload_id,last_sequence,updated_at)")).
		WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(2))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_workload_events(owner_id,workload_id,operation_id,sequence,public_sequence,kind,status,message,readback_json,at)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,workload_id::text,expected_workload_revision,plan_id::text,operation,plan_revision,plan_digest,target_kind,task_id::text,confirmation_id::text,status,revision,failure_code,failure_summary,created_at,updated_at,dispatch_state,dispatch_attempt,dispatch_epoch,dispatch_claim,dispatch_lease_until,completion_fingerprint FROM core_workload_operations")).
		WillReturnRows(sqlmock.NewRows([]string{"operation_id", "workload_id", "expected_workload_revision", "plan_id", "operation", "plan_revision", "plan_digest", "target_kind", "task_id", "confirmation_id", "status", "revision", "failure_code", "failure_summary", "created_at", "updated_at", "dispatch_state", "dispatch_attempt", "dispatch_epoch", "dispatch_claim", "dispatch_lease_until", "completion_fingerprint"}).AddRow(testWorkloadOperationID, testWorkloadID, 1, planID, "apply", 1, "digest", "AWS_EC2_SSM", testWorkloadTaskID, testWorkloadConfirmationID, "succeeded", 4, "", "", now, now, "terminal", 1, 4, nil, nil, ""))
	if _, err = repo.ReconcileUncertain(t.Context(), testWorkloadOperationID, "", workload.Readback{WorkloadID: testWorkloadID, TargetKind: workload.TargetAWSEC2SSM, State: "ready"}, ""); err != nil {
		t.Fatal(err)
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
	mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,workload_id::text,expected_workload_revision,plan_id::text,operation,plan_revision,plan_digest,target_kind,task_id::text,confirmation_id::text,status,revision,failure_code,failure_summary,created_at,updated_at,dispatch_state,dispatch_attempt,dispatch_epoch,dispatch_claim,dispatch_lease_until,completion_fingerprint FROM core_workload_operations")).
		WillReturnRows(sqlmock.NewRows([]string{
			"operation_id", "workload_id", "expected_workload_revision", "plan_id", "operation", "plan_revision", "plan_digest", "target_kind",
			"task_id", "confirmation_id", "status", "revision", "failure_code", "failure_summary", "created_at", "updated_at",
			"dispatch_state", "dispatch_attempt", "dispatch_epoch", "dispatch_claim", "dispatch_lease_until", "completion_fingerprint",
		}).AddRow(
			testWorkloadOperationID, testWorkloadID, 1, testWorkloadPlanID, "apply", 1, "digest", "AWS_EC2_SSM",
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
	mock.ExpectQuery(regexp.QuoteMeta("SELECT p.expires_at FROM core_workload_operations o JOIN core_workload_plans p")).
		WillReturnRows(sqlmock.NewRows([]string{"expires_at"}).AddRow(now.Add(time.Hour)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT workload_id::text,expected_workload_revision,task_id::text,revision FROM core_workload_operations")).
		WillReturnRows(sqlmock.NewRows([]string{"workload_id", "expected_workload_revision", "task_id", "revision"}).AddRow(testWorkloadID, 1, testWorkloadTaskID, 2))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision FROM core_workloads")).
		WillReturnRows(sqlmock.NewRows([]string{"revision"}).AddRow(1))
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
