package storage

import (
	"database/sql"
	"encoding/json"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	ext "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

func TestPostgresExtensionExecutionFinalizerAtomicSuccess(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter())
	reservation, _ := json.Marshal(map[string]any{"task_id": "task", "attempt": 1, "lease_epoch": 1, "task_revision": 3})
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision,active_version_id,state FROM p2p_agent_extensions")).WillReturnRows(sqlmock.NewRows([]string{"revision", "active_version_id", "state"}).AddRow(7, "version", "installed"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE p2p_agent_extensions SET updated_at=")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,status FROM p2p_agent_extension_execution_receipts")).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state,revision,task_id::text,reservation_json FROM agent_confirmations")).WillReturnRows(sqlmock.NewRows([]string{"state", "revision", "task_id", "reservation_json"}).AddRow("consumed", 2, "task", reservation))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,attempt,lease_epoch,revision,lease_holder,lease_expires_at FROM agent_tasks")).WillReturnRows(sqlmock.NewRows([]string{"status", "attempt", "lease_epoch", "revision", "lease_holder", "lease_expires_at"}).AddRow("running", 1, 1, 4, "worker", time.Now().UTC().Add(time.Hour)))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE agent_tasks SET status=")).WillReturnRows(sqlmock.NewRows([]string{"progress_sequence"}).AddRow(5))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_task_runtime_concurrency SET")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET reservation_json=NULL")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO p2p_agent_extension_execution_receipts")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err = (&PostgresExtensionExecutionFinalizer{DB: store}).FinalizeExecution(t.Context(), ext.ExecutionFinalizeRequest{OwnerID: "owner", TaskID: "task", ConfirmationID: "confirmation", InstallationID: "install", VersionID: "version", RequestDigest: "request", LeaseHolder: "worker", Attempt: 1, LeaseEpoch: 1, TaskRevision: 3, InstallationRevision: 7, Success: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresExtensionExecutionFinalizerReconcilesConsumedReservation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter())
	reservation, _ := json.Marshal(map[string]any{"task_id": "task", "attempt": 1, "lease_epoch": 1, "task_revision": 3})
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision,active_version_id,state FROM p2p_agent_extensions")).WillReturnRows(sqlmock.NewRows([]string{"revision", "active_version_id", "state"}).AddRow(7, "version", "installed"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE p2p_agent_extensions SET updated_at=")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,status FROM p2p_agent_extension_execution_receipts")).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state,revision,task_id::text,reservation_json FROM agent_confirmations")).WillReturnRows(sqlmock.NewRows([]string{"state", "revision", "task_id", "reservation_json"}).AddRow("consumed", 2, "task", reservation))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,attempt,lease_epoch,revision,lease_holder,lease_expires_at FROM agent_tasks")).WillReturnRows(sqlmock.NewRows([]string{"status", "attempt", "lease_epoch", "revision", "lease_holder", "lease_expires_at"}).AddRow("running", 1, 2, 6, "successor", time.Now().UTC().Add(time.Hour)))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE agent_tasks SET status=")).WillReturnRows(sqlmock.NewRows([]string{"progress_sequence"}).AddRow(6))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_task_runtime_concurrency SET")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET reservation_json=NULL")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO p2p_agent_extension_execution_receipts")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err = (&PostgresExtensionExecutionFinalizer{DB: store}).FinalizeExecution(t.Context(), ext.ExecutionFinalizeRequest{
		OwnerID:              "owner",
		TaskID:               "task",
		ConfirmationID:       "confirmation",
		InstallationID:       "install",
		VersionID:            "version",
		RequestDigest:        "request",
		LeaseHolder:          "successor",
		Attempt:              1,
		LeaseEpoch:           2,
		TaskRevision:         5,
		InstallationRevision: 7,
		Uncertain:            true,
		ErrorCode:            "extension_execution_uncertain",
		ReconcileConsumed:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresExtensionExecutionFinalizerRecordsDeterministicFailure(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter())
	reservation, _ := json.Marshal(map[string]any{"task_id": "task", "attempt": 1, "lease_epoch": 1, "task_revision": 3})
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta("SELECT revision,active_version_id,state FROM p2p_agent_extensions")).WillReturnRows(sqlmock.NewRows([]string{"revision", "active_version_id", "state"}).AddRow(7, "version", "installed"))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE p2p_agent_extensions SET updated_at=")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT request_digest,status FROM p2p_agent_extension_execution_receipts")).WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT state,revision,task_id::text,reservation_json FROM agent_confirmations")).WillReturnRows(sqlmock.NewRows([]string{"state", "revision", "task_id", "reservation_json"}).AddRow("consumed", 2, "task", reservation))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT status,attempt,lease_epoch,revision,lease_holder,lease_expires_at FROM agent_tasks")).WillReturnRows(sqlmock.NewRows([]string{"status", "attempt", "lease_epoch", "revision", "lease_holder", "lease_expires_at"}).AddRow("running", 1, 1, 4, "worker", time.Now().UTC().Add(time.Hour)))
	mock.ExpectQuery(regexp.QuoteMeta("UPDATE agent_tasks SET status=")).WillReturnRows(sqlmock.NewRows([]string{"progress_sequence"}).AddRow(5))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_task_runtime_concurrency SET")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_task_events")).
		WithArgs("owner", "task", int64(5), "extension_execution_failed", "failed", "{}", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("UPDATE agent_confirmations SET reservation_json=NULL")).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("INSERT INTO p2p_agent_extension_execution_receipts")).
		WithArgs("owner", "task", "install", "version", "request", "failed", "", "extension_tool_schema_changed", false, uint32(1), sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()
	err = (&PostgresExtensionExecutionFinalizer{DB: store}).FinalizeExecution(t.Context(), ext.ExecutionFinalizeRequest{
		OwnerID:              "owner",
		TaskID:               "task",
		ConfirmationID:       "confirmation",
		InstallationID:       "install",
		VersionID:            "version",
		RequestDigest:        "request",
		LeaseHolder:          "worker",
		Attempt:              1,
		LeaseEpoch:           1,
		TaskRevision:         3,
		InstallationRevision: 7,
		ErrorCode:            "extension_tool_schema_changed",
		ErrorSummary:         "remote MCP tool schema changed after confirmation",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
