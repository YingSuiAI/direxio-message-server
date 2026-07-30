package storage

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
)

const (
	testAWSProvisionID = "77777777-7777-4777-8777-777777777777"
	testAWSPlanID      = "88888888-8888-4888-8888-888888888888"
)

func testAWSCompletionCommand(status agentaws.ChangeStatus) agentaws.CompleteChangeCommand {
	return agentaws.CompleteChangeCommand{
		ChangeID:                     testAWSChangeID,
		ConfirmationID:               testAWSConfirmationID,
		TaskID:                       testAWSTaskID,
		Attempt:                      2,
		LeaseEpoch:                   7,
		ExpectedTaskRevision:         9,
		ExpectedChangeRevision:       4,
		ExpectedConfirmationRevision: 3,
		Status:                       status,
		ErrorCode:                    "canceled_after_dispatch",
		ErrorSummary:                 "canceled_after_dispatch",
		OperationKey:                 "99999999-9999-4999-8999-999999999999",
	}
}

func testAWSReservationJSON(t *testing.T, cmd agentaws.CompleteChangeCommand) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{
		"task_id":       cmd.TaskID,
		"attempt":       cmd.Attempt,
		"lease_epoch":   cmd.LeaseEpoch,
		"task_revision": cmd.ExpectedTaskRevision,
		"active":        true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func expectAWSCompletionFence(t *testing.T, mock sqlmock.Sqlmock, cmd agentaws.CompleteChangeCommand, operation agentaws.Operation, stage agentaws.ChangeStage, provisionID any) {
	t.Helper()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT task_id::text,confirmation_id::text,status,stage,revision FROM core_aws_changes")).
		WillReturnRows(sqlmock.NewRows([]string{"task_id", "confirmation_id", "status", "stage", "revision"}).
			AddRow(cmd.TaskID, cmd.ConfirmationID, string(agentaws.ChangeRunning), string(stage), cmd.ExpectedChangeRevision))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT provision_id::text,operation FROM core_aws_changes")).
		WillReturnRows(sqlmock.NewRows([]string{"provision_id", "operation"}).AddRow(provisionID, string(operation)))
	if provisionID != nil {
		mock.ExpectQuery(regexp.QuoteMeta("SELECT revision,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions")).
			WillReturnRows(sqlmock.NewRows([]string{"revision", "active_change_id"}).AddRow(2, cmd.ChangeID))
	}
	now := time.Now().UTC()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,revision,attempt,lease_epoch,lease_expires_at FROM agent_tasks")).
		WillReturnRows(sqlmock.NewRows([]string{"status", "revision", "attempt", "lease_epoch", "lease_expires_at"}).
			AddRow("running", cmd.ExpectedTaskRevision, cmd.Attempt, cmd.LeaseEpoch, now.Add(time.Hour)))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state,revision,task_id::text,reservation_json FROM agent_confirmations")).
		WillReturnRows(sqlmock.NewRows([]string{"state", "revision", "task_id", "reservation_json"}).
			AddRow("consumed", cmd.ExpectedConfirmationRevision, cmd.TaskID, testAWSReservationJSON(t, cmd)))
}

// A consumed confirmation may be canceled after the provider request has
// already been dispatched. The terminal transition must fence the linked
// provision as uncertain and append its reconciliation event in the same SQL
// transaction as the task/change terminalization.
func TestPostgresAWSCompleteCanceledAfterDispatchPersistsProvisionUncertainAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := testAWSCompletionCommand(agentaws.ChangeCanceled)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).
		WillReturnError(sql.ErrNoRows)
	expectAWSCompletionFence(t, mock, cmd, agentaws.OperationCreate, agentaws.StageReconciling, testAWSProvisionID)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_aws_changes SET status=$1,stage=$2,error_code=$3,error_summary=$4")).
		WithArgs(cmd.Status, agentaws.StageReconciliationRequired, cmd.ErrorCode, cmd.ErrorSummary, sqlmock.AnyArg(), testAWSOwnerID, cmd.ChangeID, cmd.TaskID, cmd.ConfirmationID, cmd.ExpectedChangeRevision).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE agent_tasks SET status=$1,revision=revision+1,failure_code=$2,failure_summary=$3")).
		WillReturnRows(sqlmock.NewRows([]string{"progress_sequence"}).AddRow(12))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at)")).
		WithArgs(testAWSOwnerID, cmd.TaskID, int64(12), "canceled", []byte(`{}`), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET reservation_json=jsonb_set")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_task_runtime_concurrency SET running_count=GREATEST")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_aws_event_counters(owner_id,change_id,next_sequence)")).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(6))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at)")).
		WithArgs(testAWSOwnerID, cmd.ChangeID, int64(6), sqlmock.AnyArg(), cmd.TaskID, cmd.ExpectedChangeRevision+1, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// This update is the durable uncertainty fence: it must be ordered before
	// the provision event counter/event and the terminal response projection.
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_aws_ec2_provisions SET state='uncertain',active_change_id=NULL,reconciliation_required=true,error_code='canceled_after_dispatch'")).
		WithArgs(cmd.ErrorSummary, sqlmock.AnyArg(), testAWSOwnerID, testAWSProvisionID, int64(2)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_aws_ec2_provision_event_counters(owner_id,provision_id,next_sequence)")).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(4))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_ec2_provision_events(owner_id,provision_id,change_id,sequence,event_id,kind,revision,at)")).
		WithArgs(testAWSOwnerID, testAWSProvisionID, cmd.ChangeID, int64(4), sqlmock.AnyArg(), "provision_reconciliation_required", int64(3), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	terminalRows := sqlmock.NewRows([]string{"change_id", "plan_id", "credential_id", "provision_id", "task_id", "confirmation_id", "operation", "status", "stage", "change_set_id", "provider_request_digest", "provider_token", "revision", "error_code", "error_summary", "created_at", "updated_at"}).
		AddRow(cmd.ChangeID, testAWSPlanID, "55555555-5555-4555-8555-555555555555", testAWSProvisionID, cmd.TaskID, cmd.ConfirmationID, "create", "canceled", "reconciliation_required", "", "digest", "token", int64(5), cmd.ErrorCode, cmd.ErrorSummary, time.Now().UTC(), time.Now().UTC())
	mock.ExpectQuery(regexp.QuoteMeta("SELECT change_id::text,plan_id::text,credential_id::text,COALESCE(provision_id::text,''),task_id::text,confirmation_id::text,operation,status,stage,change_set_id,provider_request_digest,provider_token,revision,error_code,error_summary,created_at,updated_at FROM core_aws_changes")).
		WillReturnRows(terminalRows)
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	got, err := repo.CompleteChange(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agentaws.ChangeCanceled || got.Stage != agentaws.StageReconciliationRequired || got.ProvisionID != testAWSProvisionID {
		t.Fatalf("unexpected terminal change: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A response lost after commit must be served from the owner-scoped terminal
// replay. The replay is inserted only after the terminal projection SELECT,
// and the second call must return a deep-equal immutable Change snapshot.
func TestPostgresAWSCompleteLostResponseReplaysFullTerminalSnapshot(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentAWSRepository(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testAWSOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	cmd := testAWSCompletionCommand(agentaws.ChangeSucceeded)
	cmd.ErrorCode, cmd.ErrorSummary = "", ""
	now := time.Now().UTC().Truncate(time.Microsecond)
	terminalRows := sqlmock.NewRows([]string{"change_id", "plan_id", "credential_id", "provision_id", "task_id", "confirmation_id", "operation", "status", "stage", "change_set_id", "provider_request_digest", "provider_token", "revision", "error_code", "error_summary", "created_at", "updated_at"}).
		AddRow(cmd.ChangeID, testAWSPlanID, "55555555-5555-4555-8555-555555555555", "", cmd.TaskID, cmd.ConfirmationID, "create", "succeeded", "succeeded", "changeset-1", "digest", "token", int64(5), "", "", now, now)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).
		WillReturnError(sql.ErrNoRows)
	expectAWSCompletionFence(t, mock, cmd, agentaws.OperationCreate, agentaws.StageReconciling, nil)
	mock.ExpectExec(regexp.QuoteMeta("UPDATE core_aws_changes SET status=$1,stage=$2,error_code=$3,error_summary=$4")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE agent_tasks SET status=$1,revision=revision+1,failure_code=$2,failure_summary=$3")).
		WillReturnRows(sqlmock.NewRows([]string{"progress_sequence"}).AddRow(12))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET reservation_json=NULL")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_task_runtime_concurrency SET running_count=GREATEST")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO core_aws_event_counters(owner_id,change_id,next_sequence)")).
		WillReturnRows(sqlmock.NewRows([]string{"sequence"}).AddRow(6))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at)")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT change_id::text,plan_id::text,credential_id::text,COALESCE(provision_id::text,''),task_id::text,confirmation_id::text,operation,status,stage,change_set_id,provider_request_digest,provider_token,revision,error_code,error_summary,created_at,updated_at FROM core_aws_changes")).
		WillReturnRows(terminalRows)
	// The replay insert is deliberately expected after the terminal projection.
	var terminal agentaws.Change
	terminal = agentaws.Change{ID: cmd.ChangeID, PlanID: testAWSPlanID, CredentialID: "55555555-5555-4555-8555-555555555555", TaskID: cmd.TaskID, ConfirmationID: cmd.ConfirmationID, Operation: agentaws.OperationCreate, Status: agentaws.ChangeSucceeded, Stage: agentaws.StageSucceeded, ChangeSetID: "changeset-1", ProviderRequestDigest: "digest", ProviderToken: "token", Revision: 5, CreatedAt: now, UpdatedAt: now}
	raw, err := json.Marshal(terminal)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json)")).
		WithArgs(testAWSOwnerID, cmd.OperationKey, sqlmock.AnyArg(), raw).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	first, err := repo.CompleteChange(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, terminal) {
		t.Fatalf("first terminal snapshot changed: got=%#v want=%#v", first, terminal)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_aws_replays")).
		WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_json"}).AddRow(stringDigest(cmd), raw))
	mock.ExpectCommit()
	second, err := repo.CompleteChange(t.Context(), cmd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("lost-response replay changed terminal snapshot: first=%#v second=%#v", first, second)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
