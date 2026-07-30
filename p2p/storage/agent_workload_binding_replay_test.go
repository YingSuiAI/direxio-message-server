package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/google/uuid"
)

// workloadReplayJSONArgument validates the response_json written in the same
// transaction as the operation. It intentionally captures the bytes so the
// next call can exercise the real idempotent replay path.
type workloadReplayJSONArgument struct {
	raw      *[]byte
	expected coreconfirmation.Binding
}

func (a workloadReplayJSONArgument) Match(value driver.Value) bool {
	var raw []byte
	switch v := value.(type) {
	case []byte:
		raw = append([]byte(nil), v...)
	case string:
		raw = []byte(v)
	default:
		return false
	}
	var result workload.RequestResult
	if json.Unmarshal(raw, &result) != nil || !result.Confirmation.Binding.Equal(a.expected) {
		return false
	}
	*a.raw = raw
	return true
}

func TestPostgresWorkloadRequestReplayCarriesCanonicalBinding(t *testing.T) {
	for _, kind := range []workload.OperationKind{workload.OperationApply, workload.OperationDestroy} {
		t.Run(string(kind), func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			repo, err := NewAgentWorkloadStore(NewUnmigratedDatabaseStore(db, sqlutil.NewDummyWriter()), testWorkloadOwnerID)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			plan := workload.Plan{
				ID: testWorkloadPlanID, Revision: 3, Digest: strings.Repeat("a", 64), Summary: "typed workload", Artifact: "artifact", Source: "source",
				TargetKind:      workload.TargetAWSEC2SSM,
				Target:          workload.TargetSettings{Identity: workload.TargetIdentity{Kind: workload.TargetAWSEC2SSM, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-0123456789abcdef0"}, Region: "us-east-1", AccountID: "123456789012", InstanceID: "i-0123456789abcdef0", EC2DocumentVersion: "1", EC2SystemdService: "dirextalk.service", RequiredInstanceTags: map[string]string{"managed": "true"}},
				NetworkGrants:   []string{"security-group:sg-0123456789abcdef0"},
				SecretGrantRefs: []workload.SecretGrantRef{{ReferenceID: "77777777-7777-4777-8777-777777777777", Purpose: coreconfirmation.SecretPurposeAWSCredential, Revision: 1, BindingDigest: coreconfirmation.Digest(strings.Repeat("b", 64))}},
				ExpiresAt:       now.Add(time.Hour), CreatedAt: now,
			}
			planRaw, err := json.Marshal(plan)
			if err != nil {
				t.Fatal(err)
			}
			idempotencyKey := uuid.NewString()
			in := workload.RequestCommand{PlanID: plan.ID, WorkloadID: testWorkloadID, ExpectedWorkloadRevision: 1, Kind: kind, IdempotencyKey: idempotencyKey, ExpiresAt: plan.ExpiresAt}
			expected := workload.BindingForOperation(plan, testWorkloadID, kind)
			expected.OwnerID = testWorkloadOwnerID
			expected, err = expected.Normalize()
			if err != nil {
				t.Fatal(err)
			}

			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_workload_idempotency")).WillReturnError(sql.ErrNoRows)
			mock.ExpectQuery(regexp.QuoteMeta("SELECT plan_json FROM core_workload_plans")).WillReturnRows(sqlmock.NewRows([]string{"plan_json"}).AddRow(planRaw))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT revision,state,plan_id::text,plan_digest,target_kind FROM core_workloads")).WillReturnRows(sqlmock.NewRows([]string{"revision", "state", "plan_id", "plan_digest", "target_kind"}).AddRow(1, "ready", plan.ID, plan.Digest, string(plan.TargetKind)))
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_workloads(")).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_tasks(")).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO agent_confirmations(")).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_workload_operations(")).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_workload_event_counters(")).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT workload_id::text FROM core_workload_operations")).WillReturnRows(sqlmock.NewRows([]string{"workload_id"}).AddRow(testWorkloadID))
			mock.ExpectQuery(regexp.QuoteMeta("INSERT INTO p2p_agent_deployment_event_cursors(")).WillReturnRows(sqlmock.NewRows([]string{"last_sequence"}).AddRow(1))
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_workload_events(")).WillReturnResult(sqlmock.NewResult(0, 1))
			var replayRaw []byte
			mock.ExpectExec(regexp.QuoteMeta("INSERT INTO core_workload_idempotency(")).WithArgs(sqlmock.AnyArg(), string(kind), idempotencyKey, sqlmock.AnyArg(), sqlmock.AnyArg(), workloadReplayJSONArgument{raw: &replayRaw, expected: expected}).WillReturnResult(sqlmock.NewResult(0, 1))
			mock.ExpectCommit()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT operation_id::text,workload_id::text,expected_workload_revision")).WillReturnRows(sqlmock.NewRows([]string{"operation_id", "workload_id", "expected_workload_revision", "plan_id", "operation", "plan_revision", "plan_digest", "target_kind", "task_id", "confirmation_id", "status", "revision", "failure_code", "failure_summary", "created_at", "updated_at", "dispatch_state", "dispatch_attempt", "dispatch_epoch", "dispatch_claim", "dispatch_lease_until", "completion_fingerprint"}).AddRow(testWorkloadOperationID, testWorkloadID, 1, plan.ID, string(kind), plan.Revision, plan.Digest, string(plan.TargetKind), testWorkloadTaskID, testWorkloadConfirmationID, "waiting_user", 1, "", "", now, now, "prepared", 0, 0, nil, nil, ""))
			mock.ExpectQuery(regexp.QuoteMeta("SELECT owner_id,spec_json,status,attempt,lease_epoch,revision,progress_sequence")).WillReturnRows(sqlmock.NewRows([]string{"owner_id", "spec_json", "status", "attempt", "lease_epoch", "revision", "progress_sequence", "available_at", "lease_holder", "lease_expires_at", "result_json", "failure_code", "failure_summary", "execution_started_at", "execution_deadline_at", "retry_of_task_id", "created_at", "updated_at", "deleted_at"}).AddRow(testWorkloadOwnerID, []byte(`{}`), "waiting_user", 1, 0, 1, 0, now, "", nil, nil, "", "", nil, nil, nil, now, now, nil))
			bindingRaw, _ := json.Marshal(expected)
			mock.ExpectQuery(regexp.QuoteMeta("SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision")).WillReturnRows(sqlmock.NewRows([]string{"confirmation_id", "owner_id", "operation_domain", "target_id", "target_revision", "binding_digest", "binding_json", "task_id", "state", "revision", "created_at", "updated_at", "expires_at", "terminal_reason"}).AddRow(testWorkloadConfirmationID, testWorkloadOwnerID, expected.OperationDomain, expected.TargetID, expected.TargetRevision, expected.Digest, bindingRaw, testWorkloadTaskID, "pending", 1, now, now, plan.ExpiresAt, ""))
			mock.ExpectExec(regexp.QuoteMeta("UPDATE core_workload_idempotency SET response_json=")).WillReturnResult(sqlmock.NewResult(0, 1))

			first, err := repo.RequestOperation(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if len(replayRaw) == 0 || !first.Confirmation.Binding.Equal(expected) {
				t.Fatalf("first response binding = %#v, want %#v", first.Confirmation.Binding, expected)
			}
			if first.Operation.WorkloadID != expected.TargetID || first.Operation.PlanRevision != uint64(expected.TargetRevision) || first.Operation.PlanDigest != string(expected.ContentDigest) {
				t.Fatalf("operation binding projection = %#v, want target=%s revision=%d digest=%s", first.Operation, expected.TargetID, expected.TargetRevision, expected.ContentDigest)
			}

			mock.ExpectBegin()
			mock.ExpectQuery(regexp.QuoteMeta("SELECT request_hash,response_json FROM core_workload_idempotency")).WillReturnRows(sqlmock.NewRows([]string{"request_hash", "response_json"}).AddRow(workload.RequestInputDigest(in), replayRaw))
			mock.ExpectCommit()
			replayed, err := repo.RequestOperation(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			if !replayed.Confirmation.Binding.Equal(expected) {
				t.Fatalf("replayed response binding = %#v, want %#v", replayed.Confirmation.Binding, expected)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
