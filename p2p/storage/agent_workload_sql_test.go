package storage

import (
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
)

func TestInsertWorkloadEventTxNormalizesReadbackParameters(t *testing.T) {
	const (
		owner      = "@workload-event:example.test"
		operation  = "11111111-1111-4111-8111-111111111111"
		workloadID = "22222222-2222-4222-8222-222222222222"
	)
	at := time.Now().UTC()
	insertSQL := regexp.QuoteMeta("INSERT INTO core_workload_events(owner_id,workload_id,operation_id,sequence,public_sequence,kind,status,message,readback_json,at)")

	tests := []struct {
		name     string
		readback []byte
		wantArg  any
	}{
		{name: "absent", readback: nil, wantArg: nil},
		{name: "valid", readback: []byte(`{"state":"ready"}`), wantArg: []byte(`{"state":"ready"}`)},
	}
	for _, tc := range tests {
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
			mock.ExpectQuery(regexp.QuoteMeta("SELECT workload_id::text FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2")).
				WithArgs(owner, operation).
				WillReturnRows(sqlmock.NewRows([]string{"workload_id"}).AddRow(workloadID))
			mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO p2p_agent_deployment_event_cursors(owner_id,workload_id,last_sequence,updated_at)")).
				WithArgs(owner, workloadID, sqlmock.AnyArg()).
				WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(1))
			mock.ExpectExec(insertSQL).
				WithArgs(owner, workloadID, operation, uint64(1), int64(1), "requested", "waiting_user", "waiting", tc.wantArg, sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(0, 1))

			if err = insertWorkloadEventTx(t.Context(), tx, owner, operation, 1, "requested", "waiting_user", "waiting", tc.readback, at); err != nil {
				t.Fatal(err)
			}
			mock.ExpectCommit()
			if err = tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWorkloadEventReadbackArgRejectsInvalidJSON(t *testing.T) {
	for _, readback := range [][]byte{[]byte(`{"state":`), []byte("  \t")} {
		if _, err := workloadEventReadbackArg(readback); err == nil {
			t.Fatalf("invalid readback %q accepted", readback)
		}
	}
}

func TestPostgresInsertWorkloadEventTxStoresAbsentReadbackAsSQLNull(t *testing.T) {
	ctx, store := openAgentJSONBPostgres(t)
	owner := "@workload-event-null:" + uuid.NewString()
	planID, workloadID, operationID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	taskID, confirmationID := uuid.NewString(), uuid.NewString()
	now := time.Now().UTC().Truncate(time.Microsecond)
	digest := strings.Repeat("c", 64)
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_workload_plans(plan_id,owner_id,create_idempotency_key,create_request_hash,revision,digest,summary,plan_json,target_kind,expires_at,created_at) VALUES($1,$2,$3,$4,1,$4,'event null','{}','AWS_EC2_SSM',$5,$6)`, planID, owner, uuid.NewString(), digest, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_workloads(workload_id,owner_id,revision,plan_id,plan_digest,target_kind,state,updated_at) VALUES($1,$2,1,$3,$4,'AWS_EC2_SSM','pending',$5)`, workloadID, owner, planID, digest, now); err != nil {
		t.Fatal(err)
	}
	insertJSONBTestTask(t, ctx, store, owner, taskID, "waiting_user", 1, 0, 1, "", nil, now)
	binding := jsonbTestBinding(owner, workloadID)
	insertJSONBTestConfirmation(t, ctx, store, owner, confirmationID, taskID, "pending", 1, binding, now.Add(time.Hour), now, "")
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_workload_operations(operation_id,owner_id,workload_id,expected_workload_revision,plan_id,operation,plan_revision,plan_digest,target_kind,task_id,confirmation_id,status,revision,created_at,updated_at) VALUES($1,$2,$3,1,$4,'apply',1,$5,'AWS_EC2_SSM',$6,$7,'waiting_user',1,$8,$8)`, operationID, owner, workloadID, planID, digest, taskID, confirmationID, now); err != nil {
		t.Fatal(err)
	}

	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = insertWorkloadEventTx(ctx, tx, owner, operationID, 1, "requested", "waiting_user", "waiting", nil, now); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = insertWorkloadEventTx(ctx, tx, owner, operationID, 2, "readback", "running", "ready", []byte(`{"state":"ready"}`), now); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	var absent bool
	if err = store.DB().QueryRowContext(ctx, `SELECT readback_json IS NULL FROM core_workload_events WHERE owner_id=$1 AND operation_id=$2 AND sequence=1`, owner, operationID).Scan(&absent); err != nil {
		t.Fatal(err)
	}
	if !absent {
		t.Fatal("absent readback was not stored as SQL NULL")
	}
	var state sql.NullString
	if err = store.DB().QueryRowContext(ctx, `SELECT readback_json->>'state' FROM core_workload_events WHERE owner_id=$1 AND operation_id=$2 AND sequence=2`, owner, operationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if !state.Valid || state.String != "ready" {
		t.Fatalf("valid readback = %#v, want ready", state)
	}
}
