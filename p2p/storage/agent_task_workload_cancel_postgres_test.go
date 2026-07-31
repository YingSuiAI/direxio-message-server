package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestPostgresWorkloadTaskCancelFencesWaitingAndQueued(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	db := store.DB()
	owner := "@workload-cancel-fence:example.test"
	now := time.Now().UTC().Truncate(time.Microsecond)

	for i, status := range []string{"waiting_user", "queued"} {
		digest := strings.Repeat(string(rune('a'+i)), 64)
		base := "7" + string(rune('1'+i))
		operationID := base + "111111-1111-4111-8111-111111111111"
		workloadID := base + "222222-2222-4222-8222-222222222222"
		planID := base + "333333-3333-4333-8333-333333333333"
		taskID := base + "444444-4444-4444-8444-444444444444"
		confirmationID := base + "555555-5555-4555-8555-555555555555"
		// The generated IDs above are intentionally fixed-width UUID strings.
		planJSON := []byte(`{"target_kind":"AWS_EC2_SSM"}`)
		if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_plans(plan_id,owner_id,create_idempotency_key,create_request_hash,revision,digest,summary,plan_json,target_kind,target_identity_json,resource_limits_json,secret_grant_refs_json,expires_at,created_at) VALUES($1,$2,$3,$4,1,$5,'cancel fence',$6,'AWS_EC2_SSM','{}','{}','[]',$7,$8)`, planID, owner, confirmationID, digest, digest, planJSON, now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO core_workloads(workload_id,owner_id,revision,plan_id,plan_digest,target_kind,state,actual_snapshot_json,updated_at) VALUES($1,$2,1,$3,$4,'AWS_EC2_SSM','pending','{}',$5)`, workloadID, owner, planID, digest, now); err != nil {
			t.Fatal(err)
		}
		payload := coretask.WorkloadTaskPayload{WorkloadID: workloadID, ExpectedWorkloadRevision: 1, PlanID: planID, OperationID: operationID, PlanRevision: 1, PlanDigest: digest, TargetKind: "AWS_EC2_SSM", ConfirmationID: confirmationID}
		spec, _ := (coretask.TaskSpec{Kind: coretask.TaskKindWorkload, Goal: "cancel fence", IdempotencyKey: taskID, AvailableAt: now, Payload: coretask.TaskPayload{Workload: &payload}}).Normalize()
		specJSON, _ := json.Marshal(spec)
		if _, err = db.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,revision,available_at,created_at,updated_at) VALUES($1,$2,$3,$4,1,1,$5,$5,$5)`, taskID, owner, specJSON, status, now); err != nil {
			t.Fatal(err)
		}
		bindingJSON := []byte(`{"operation_domain":"workload:apply","target_id":"` + workloadID + `","target_revision":1}`)
		confirmationState := "pending"
		if status == "queued" {
			confirmationState = "confirmed"
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,created_at,updated_at) VALUES($1,$2,'workload:apply',$3,1,$4,$5,$6,$7,1,$8,$9,$9)`, confirmationID, owner, workloadID, digest, bindingJSON, taskID, confirmationState, now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_operations(operation_id,owner_id,workload_id,expected_workload_revision,plan_id,operation,plan_revision,plan_digest,target_kind,task_id,confirmation_id,status,revision,created_at,updated_at) VALUES($1,$2,$3,1,$4,'apply',1,$5,'AWS_EC2_SSM',$6,$7,'waiting_user',1,$8,$8)`, operationID, owner, workloadID, planID, digest, taskID, confirmationID, now); err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2)`, owner, operationID); err != nil {
			t.Fatal(err)
		}
		key := "6" + string(rune('1'+i)) + "666666-6666-4666-8666-666666666666"
		command := coretask.CancelCommand{OwnerID: owner, TaskID: taskID, ExpectedRevision: 1, Mutation: coretask.MutationCommand{IdempotencyKey: key, ExpectedRevision: 1}, Reason: "owner canceled", At: now}
		got, err := NewDatabaseTaskStore(db).Cancel(ctx, command)
		if err != nil {
			t.Fatalf("%s cancel: %v", status, err)
		}
		if got.Status != coretask.StatusCanceled || got.Revision != 2 {
			t.Fatalf("%s canceled task = %+v", status, got)
		}
		var operationStatus, taskStatus, confirmationStatus string
		if err = db.QueryRowContext(ctx, `SELECT status FROM core_workload_operations WHERE operation_id=$1`, operationID).Scan(&operationStatus); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRowContext(ctx, `SELECT status FROM agent_tasks WHERE task_id=$1`, taskID).Scan(&taskStatus); err != nil {
			t.Fatal(err)
		}
		if err = db.QueryRowContext(ctx, `SELECT state FROM agent_confirmations WHERE confirmation_id=$1`, confirmationID).Scan(&confirmationStatus); err != nil {
			t.Fatal(err)
		}
		if operationStatus != "canceled" || taskStatus != "canceled" || confirmationStatus != "expired" {
			t.Fatalf("%s terminal states = operation=%s task=%s confirmation=%s", status, operationStatus, taskStatus, confirmationStatus)
		}
		// Replaying the same key returns the committed immutable response.
		replayed, err := NewDatabaseTaskStore(db).Cancel(ctx, command)
		if err != nil || replayed.ID != taskID || replayed.Revision != 2 {
			t.Fatalf("%s replay = %+v, %v", status, replayed, err)
		}
	}

	// A task payload that points at a different operation is rejected before
	// either the task or workload operation can be terminalized.
	operationID := "73333333-3333-4333-8333-333333333333"
	workloadID := "74444444-4444-4444-8444-444444444444"
	planID := "75555555-5555-4555-8555-555555555555"
	taskID := "76666666-6666-4666-8666-666666666666"
	confirmationID := "77777777-7777-4777-8777-777777777777"
	wrongOperationID := "78888888-8888-4888-8888-888888888888"
	digest := strings.Repeat("c", 64)
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_plans(plan_id,owner_id,create_idempotency_key,create_request_hash,revision,digest,summary,plan_json,target_kind,target_identity_json,resource_limits_json,secret_grant_refs_json,expires_at,created_at) VALUES($1,$2,$3,$4,1,$5,'mismatch',$6,'AWS_EC2_SSM','{}','{}','[]',$7,$8)`, planID, owner, confirmationID, digest, digest, []byte(`{}`), now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workloads(workload_id,owner_id,revision,plan_id,plan_digest,target_kind,state,actual_snapshot_json,updated_at) VALUES($1,$2,1,$3,$4,'AWS_EC2_SSM','pending','{}',$5)`, workloadID, owner, planID, digest, now); err != nil {
		t.Fatal(err)
	}
	payload := coretask.WorkloadTaskPayload{WorkloadID: workloadID, ExpectedWorkloadRevision: 1, PlanID: planID, OperationID: wrongOperationID, PlanRevision: 1, PlanDigest: digest, TargetKind: "AWS_EC2_SSM", ConfirmationID: confirmationID}
	spec, _ := (coretask.TaskSpec{Kind: coretask.TaskKindWorkload, Goal: "mismatch", IdempotencyKey: taskID, AvailableAt: now, Payload: coretask.TaskPayload{Workload: &payload}}).Normalize()
	specJSON, _ := json.Marshal(spec)
	if _, err = db.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,revision,available_at,created_at,updated_at) VALUES($1,$2,$3,'waiting_user',1,1,$4,$4,$4)`, taskID, owner, specJSON, now); err != nil {
		t.Fatal(err)
	}
	bindingJSON := []byte(`{"operation_domain":"workload:apply","target_id":"` + workloadID + `","target_revision":1}`)
	if _, err = db.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,created_at,updated_at) VALUES($1,$2,'workload:apply',$3,1,$4,$5,$6,'pending',1,$7,$8,$8)`, confirmationID, owner, workloadID, digest, bindingJSON, taskID, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_operations(operation_id,owner_id,workload_id,expected_workload_revision,plan_id,operation,plan_revision,plan_digest,target_kind,task_id,confirmation_id,status,revision,created_at,updated_at) VALUES($1,$2,$3,1,$4,'apply',1,$5,'AWS_EC2_SSM',$6,$7,'waiting_user',1,$8,$8)`, operationID, owner, workloadID, planID, digest, taskID, confirmationID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2)`, owner, operationID); err != nil {
		t.Fatal(err)
	}
	_, err = NewDatabaseTaskStore(db).Cancel(ctx, coretask.CancelCommand{OwnerID: owner, TaskID: taskID, ExpectedRevision: 1, Mutation: coretask.MutationCommand{IdempotencyKey: "79999999-9999-4999-8999-999999999999", ExpectedRevision: 1}, Reason: "mismatch", At: now})
	if !errors.Is(err, coretask.ErrConflict) {
		t.Fatalf("mismatched workload fence error = %v, want conflict", err)
	}
	var status string
	if err = db.QueryRowContext(ctx, `SELECT status FROM agent_tasks WHERE task_id=$1`, taskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "waiting_user" {
		t.Fatalf("mismatched task was mutated to %s", status)
	}
}
