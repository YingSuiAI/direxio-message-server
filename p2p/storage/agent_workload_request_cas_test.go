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
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	"github.com/google/uuid"
)

func TestPostgresRequestOperationWorkloadRevisionCASMissingAndStale(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo, err := NewAgentWorkloadStore(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testWorkloadOwnerID)
	if err != nil {
		t.Fatal(err)
	}
	p := workload.Plan{ID: testWorkloadPlanID, Revision: 1, Digest: strings.Repeat("a", 64), Summary: "test", TargetKind: workload.TargetAWSEC2SSM, Target: workload.TargetSettings{Identity: workload.TargetIdentity{Kind: workload.TargetAWSEC2SSM, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-0123456789abcdef0"}, Region: "us-east-1", AccountID: "123456789012", InstanceID: "i-0123456789abcdef0", EC2DocumentVersion: "1", EC2SystemdService: "dirextalk.service", RequiredInstanceTags: map[string]string{"managed": "true"}}, SecretGrantRefs: []workload.SecretGrantRef{{ReferenceID: "77777777-7777-4777-8777-777777777777", Purpose: "aws_credential", Revision: 1, BindingDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, ExpiresAt: time.Now().UTC().Add(time.Hour)}
	raw, _ := json.Marshal(p)
	cmd := func() workload.RequestCommand {
		return workload.RequestCommand{PlanID: p.ID, WorkloadID: testWorkloadID, ExpectedWorkloadRevision: 2, Kind: workload.OperationApply, IdempotencyKey: uuid.NewString(), ExpiresAt: p.ExpiresAt}
	}
	for _, tc := range []struct {
		name string
		row  *sqlmock.Rows
		want error
	}{
		{"missing", nil, workload.ErrNotFound},
		{"stale", sqlmock.NewRows([]string{"revision", "state", "plan_id", "plan_digest", "target_kind"}).AddRow(1, "ready", p.ID, p.Digest, p.TargetKind), workload.ErrRevisionConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_workload_idempotency")).WillReturnError(sql.ErrNoRows)
			mock.ExpectQuery(regexp.QuoteMeta("SELECT plan_json FROM core_workload_plans")).WillReturnRows(sqlmock.NewRows([]string{"plan_json"}).AddRow(raw))
			q := mock.ExpectQuery(regexp.QuoteMeta("SELECT revision,state,plan_id::text,plan_digest,target_kind FROM core_workloads"))
			if tc.row == nil {
				q.WillReturnError(sql.ErrNoRows)
			} else {
				q.WillReturnRows(tc.row)
			}
			mock.ExpectRollback()
			_, got := repo.RequestOperation(context.Background(), cmd())
			if !errors.Is(got, tc.want) {
				t.Fatalf("error = %v, want %v", got, tc.want)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPostgresConsumeFencedStaleRevisionRejectsWrongConfirmationOrTask(t *testing.T) {
	tests := []struct {
		name         string
		confirmation string
		fenceTask    string
	}{
		{name: "wrong confirmation", confirmation: "66666666-6666-4666-8666-666666666666", fenceTask: testWorkloadTaskID},
		{name: "wrong task", confirmation: testWorkloadConfirmationID, fenceTask: "77777777-7777-4777-8777-777777777777"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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
			fence := workload.TaskFence{TaskID: tc.fenceTask, Attempt: 1, LeaseEpoch: 2, Revision: 4, Holder: "worker", ExpiresAt: now.Add(time.Hour)}
			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT status,revision,attempt,lease_epoch,lease_holder,lease_expires_at FROM agent_tasks")).
				WillReturnRows(sqlmock.NewRows([]string{"status", "revision", "attempt", "lease_epoch", "lease_holder", "lease_expires_at"}).AddRow("running", 5, 1, 2, "worker", now.Add(time.Hour)))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT p.expires_at FROM core_workload_operations o JOIN core_workload_plans p")).
				WillReturnRows(sqlmock.NewRows([]string{"expires_at"}).AddRow(now.Add(time.Hour)))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT workload_id::text,expected_workload_revision,task_id::text,confirmation_id::text,revision FROM core_workload_operations")).
				WillReturnRows(sqlmock.NewRows([]string{"workload_id", "expected_workload_revision", "task_id", "confirmation_id", "revision"}).AddRow(testWorkloadID, 2, testWorkloadTaskID, testWorkloadConfirmationID, 5))
			mock.ExpectRollback()
			_, _, err = repo.ConsumeFenced(t.Context(), testWorkloadOperationID, tc.confirmation, "digest", 5, fence)
			if !errors.Is(err, workload.ErrRevisionConflict) {
				t.Fatalf("error = %v, want revision conflict", err)
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
