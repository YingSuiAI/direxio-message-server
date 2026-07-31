package storage

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
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
	now := time.Now().UTC()

	for i, status := range []string{"waiting_user", "queued"} {
		base := "7" + string(rune('1'+i))
		operationID := base + "111111-1111-4111-8111-111111111111"
		workloadID := base + "222222-2222-4222-8222-222222222222"
		planID := base + "333333-3333-4333-8333-333333333333"
		taskID := base + "444444-4444-4444-8444-444444444444"
		confirmationID := base + "555555-5555-4555-8555-555555555555"
		// The generated IDs above are intentionally fixed-width UUID strings.
		plan, err := (workload.Plan{ID: planID, Revision: 1, Summary: "cancel fence " + status, TargetKind: workload.TargetAWSEC2SSM, SecretGrantRefs: []workload.SecretGrantRef{{ReferenceID: "88888888-8888-4888-8888-888888888888", Revision: 1, Purpose: "aws_credential", BindingDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}}, Target: workload.TargetSettings{Identity: workload.TargetIdentity{Kind: workload.TargetAWSEC2SSM, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-0123456789abcdef0"}, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-0123456789abcdef0", EC2DocumentVersion: "1", EC2SystemdService: "dirextalk.service", RequiredInstanceTags: map[string]string{"managed": "true"}}, ExpiresAt: now.Add(time.Hour), CreatedAt: now}).Normalize()
		if err != nil {
			t.Fatal(err)
		}
		digest := plan.Digest
		planJSON, _ := json.Marshal(plan)
		targetRaw, _ := json.Marshal(plan.Target.Identity)
		limitsRaw, _ := json.Marshal(plan.ResourceLimits)
		refsRaw, _ := json.Marshal(plan.SecretGrantRefs)
		if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_plans(plan_id,owner_id,create_idempotency_key,create_request_hash,revision,digest,summary,plan_json,target_kind,target_identity_json,resource_limits_json,secret_grant_refs_json,expires_at,created_at) VALUES($1,$2,$3,$4,1,$5,$6,$7,'AWS_EC2_SSM',$8,$9,$10,$11,$12)`, planID, owner, confirmationID, digest, digest, plan.Summary, planJSON, targetRaw, limitsRaw, refsRaw, now.Add(time.Hour), now); err != nil {
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
		binding := workload.BindingForOperation(plan, workloadID, workload.OperationApply)
		binding.OwnerID = owner
		binding, err = binding.Normalize()
		if err != nil {
			t.Fatal(err)
		}
		bindingJSON, _ := json.Marshal(binding)
		confirmationState := "pending"
		if status == "queued" {
			confirmationState = "confirmed"
		}
		if _, err = db.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,created_at,updated_at) VALUES($1,$2,'workload:apply',$3,1,$4,$5,$6,$7,1,$8,$9,$9)`, confirmationID, owner, workloadID, "", bindingJSON, taskID, confirmationState, now.Add(time.Hour), now); err != nil {
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
	plan, err := (workload.Plan{ID: planID, Revision: 1, Summary: "mismatch", TargetKind: workload.TargetAWSEC2SSM, SecretGrantRefs: []workload.SecretGrantRef{{ReferenceID: "88888888-8888-4888-8888-888888888889", Revision: 1, Purpose: "aws_credential", BindingDigest: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}}, Target: workload.TargetSettings{Identity: workload.TargetIdentity{Kind: workload.TargetAWSEC2SSM, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-0123456789abcdef0"}, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-0123456789abcdef0", EC2DocumentVersion: "1", EC2SystemdService: "dirextalk.service", RequiredInstanceTags: map[string]string{"managed": "true"}}, ExpiresAt: now.Add(time.Hour), CreatedAt: now}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	digest := plan.Digest
	planJSON, _ := json.Marshal(plan)
	targetRaw, _ := json.Marshal(plan.Target.Identity)
	limitsRaw, _ := json.Marshal(plan.ResourceLimits)
	refsRaw, _ := json.Marshal(plan.SecretGrantRefs)
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_plans(plan_id,owner_id,create_idempotency_key,create_request_hash,revision,digest,summary,plan_json,target_kind,target_identity_json,resource_limits_json,secret_grant_refs_json,expires_at,created_at) VALUES($1,$2,$3,$4,1,$5,$6,$7,'AWS_EC2_SSM',$8,$9,$10,$11,$12)`, planID, owner, confirmationID, digest, digest, plan.Summary, planJSON, targetRaw, limitsRaw, refsRaw, now.Add(time.Hour), now); err != nil {
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
	binding := workload.BindingForOperation(plan, workloadID, workload.OperationApply)
	binding.OwnerID = owner
	binding, err = binding.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	bindingJSON, _ := json.Marshal(binding)
	if _, err = db.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,created_at,updated_at) VALUES($1,$2,'workload:apply',$3,1,$4,$5,$6,'pending',1,$7,$8,$8)`, confirmationID, owner, workloadID, "", bindingJSON, taskID, now.Add(time.Hour), now); err != nil {
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
	// With the task identity repaired, a tampered confirmation binding still
	// fails closed before any terminal mutation.
	payload.OperationID = operationID
	spec.Payload.Workload = &payload
	specJSON, _ = json.Marshal(spec)
	if _, err = db.ExecContext(ctx, `UPDATE agent_tasks SET spec_json=$1 WHERE task_id=$2`, specJSON, taskID); err != nil {
		t.Fatal(err)
	}
	tamperedBinding := binding
	tamperedBinding.TargetID = "78888888-8888-4888-8888-888888888888"
	tamperedBindingJSON, _ := json.Marshal(tamperedBinding)
	if _, err = db.ExecContext(ctx, `UPDATE agent_confirmations SET binding_json=$1 WHERE confirmation_id=$2`, tamperedBindingJSON, confirmationID); err != nil {
		t.Fatal(err)
	}
	_, err = NewDatabaseTaskStore(db).Cancel(ctx, coretask.CancelCommand{OwnerID: owner, TaskID: taskID, ExpectedRevision: 1, Mutation: coretask.MutationCommand{IdempotencyKey: "78899999-9999-4999-8999-999999999999", ExpectedRevision: 1}, Reason: "binding tamper", At: now})
	if !errors.Is(err, coretask.ErrConflict) {
		t.Fatalf("tampered workload binding error = %v, want conflict", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT status FROM agent_tasks WHERE task_id=$1`, taskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "waiting_user" {
		t.Fatalf("tampered binding mutated task to %s", status)
	}
	// Restore the binding, then tamper the immutable plan DTO while retaining
	// its old digest column. The canonical Plan fence must reject this too.
	correctBindingJSON, _ := json.Marshal(binding)
	if _, err = db.ExecContext(ctx, `UPDATE agent_confirmations SET binding_json=$1 WHERE confirmation_id=$2`, correctBindingJSON, confirmationID); err != nil {
		t.Fatal(err)
	}
	tamperedPlan := plan
	tamperedPlan.Summary = "tampered plan"
	tamperedPlanJSON, _ := json.Marshal(tamperedPlan)
	if _, err = db.ExecContext(ctx, `UPDATE core_workload_plans SET plan_json=$1 WHERE plan_id=$2`, tamperedPlanJSON, planID); err != nil {
		t.Fatal(err)
	}
	_, err = NewDatabaseTaskStore(db).Cancel(ctx, coretask.CancelCommand{OwnerID: owner, TaskID: taskID, ExpectedRevision: 1, Mutation: coretask.MutationCommand{IdempotencyKey: "78999999-9999-4999-8999-999999999999", ExpectedRevision: 1}, Reason: "plan tamper", At: now})
	if !errors.Is(err, coretask.ErrConflict) {
		t.Fatalf("tampered workload plan error = %v, want conflict", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT status FROM agent_tasks WHERE task_id=$1`, taskID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if status != "waiting_user" {
		t.Fatalf("tampered plan mutated task to %s", status)
	}
	canonicalPlanJSON, _ := json.Marshal(plan)
	if _, err = db.ExecContext(ctx, `UPDATE core_workload_plans SET plan_json=$1,resource_limits_json=$2 WHERE plan_id=$3`, canonicalPlanJSON, []byte(`{"cpu":9007199254740993}`), planID); err != nil {
		t.Fatal(err)
	}
	_, err = NewDatabaseTaskStore(db).Cancel(ctx, coretask.CancelCommand{OwnerID: owner, TaskID: taskID, ExpectedRevision: 1, Mutation: coretask.MutationCommand{IdempotencyKey: "79099999-9999-4999-8999-999999999999", ExpectedRevision: 1}, Reason: "numeric tamper", At: now})
	if !errors.Is(err, coretask.ErrConflict) {
		t.Fatalf("numeric plan column tamper error = %v, want conflict", err)
	}
}

func TestPostgresWorkloadDestroyCancelRejectsStaleWorkloadRevision(t *testing.T) {
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
	owner := "@workload-destroy-stale:example.test"
	now := time.Now().UTC()
	operationID := "81111111-1111-4111-8111-111111111111"
	workloadID := "82222222-2222-4222-8222-222222222222"
	planID := "83333333-3333-4333-8333-333333333333"
	taskID := "84444444-4444-4444-8444-444444444444"
	confirmationID := "85555555-5555-4555-8555-555555555555"
	plan, err := (workload.Plan{ID: planID, Revision: 1, Summary: "destroy stale", TargetKind: workload.TargetAWSEC2SSM, SecretGrantRefs: []workload.SecretGrantRef{{ReferenceID: "86666666-6666-4666-8666-666666666666", Revision: 1, Purpose: "aws_credential", BindingDigest: "dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"}}, Target: workload.TargetSettings{Identity: workload.TargetIdentity{Kind: workload.TargetAWSEC2SSM, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-0123456789abcdef0"}, AccountID: "123456789012", Region: "us-east-1", InstanceID: "i-0123456789abcdef0", EC2DocumentVersion: "1", EC2SystemdService: "dirextalk.service", RequiredInstanceTags: map[string]string{"managed": "true"}}, ExpiresAt: now.Add(time.Hour), CreatedAt: now}).Normalize()
	if err != nil {
		t.Fatal(err)
	}
	planJSON, _ := json.Marshal(plan)
	targetRaw, _ := json.Marshal(plan.Target.Identity)
	limitsRaw, _ := json.Marshal(plan.ResourceLimits)
	refsRaw, _ := json.Marshal(plan.SecretGrantRefs)
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_plans(plan_id,owner_id,create_idempotency_key,create_request_hash,revision,digest,summary,plan_json,target_kind,target_identity_json,resource_limits_json,secret_grant_refs_json,expires_at,created_at) VALUES($1,$2,$3,$4,1,$5,$6,$7,'AWS_EC2_SSM',$8,$9,$10,$11,$12)`, planID, owner, confirmationID, plan.Digest, plan.Digest, plan.Summary, planJSON, targetRaw, limitsRaw, refsRaw, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workloads(workload_id,owner_id,revision,plan_id,plan_digest,target_kind,state,actual_snapshot_json,updated_at) VALUES($1,$2,2,$3,$4,'AWS_EC2_SSM','ready','{}',$5)`, workloadID, owner, planID, plan.Digest, now); err != nil {
		t.Fatal(err)
	}
	payload := coretask.WorkloadTaskPayload{WorkloadID: workloadID, ExpectedWorkloadRevision: 1, PlanID: planID, OperationID: operationID, PlanRevision: 1, PlanDigest: plan.Digest, TargetKind: "AWS_EC2_SSM", ConfirmationID: confirmationID}
	spec, _ := (coretask.TaskSpec{Kind: coretask.TaskKindWorkload, Goal: "destroy stale", IdempotencyKey: taskID, AvailableAt: now, Payload: coretask.TaskPayload{Workload: &payload}}).Normalize()
	specJSON, _ := json.Marshal(spec)
	if _, err = db.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,revision,available_at,created_at,updated_at) VALUES($1,$2,$3,'waiting_user',1,1,$4,$4,$4)`, taskID, owner, specJSON, now); err != nil {
		t.Fatal(err)
	}
	binding := workload.BindingForOperation(plan, workloadID, workload.OperationDestroy)
	binding.OwnerID = owner
	binding, err = binding.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	bindingJSON, _ := json.Marshal(binding)
	if _, err = db.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,created_at,updated_at) VALUES($1,$2,'workload:destroy',$3,1,$4,$5,$6,'pending',1,$7,$8,$8)`, confirmationID, owner, workloadID, "", bindingJSON, taskID, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_operations(operation_id,owner_id,workload_id,expected_workload_revision,plan_id,operation,plan_revision,plan_digest,target_kind,task_id,confirmation_id,status,revision,created_at,updated_at) VALUES($1,$2,$3,1,$4,'destroy',1,$5,'AWS_EC2_SSM',$6,$7,'waiting_user',1,$8,$8)`, operationID, owner, workloadID, planID, plan.Digest, taskID, confirmationID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2)`, owner, operationID); err != nil {
		t.Fatal(err)
	}
	_, err = NewDatabaseTaskStore(db).Cancel(ctx, coretask.CancelCommand{OwnerID: owner, TaskID: taskID, ExpectedRevision: 1, Mutation: coretask.MutationCommand{IdempotencyKey: "87777777-7777-4777-8777-777777777777", ExpectedRevision: 1}, Reason: "stale destroy", At: now})
	if !errors.Is(err, coretask.ErrConflict) {
		t.Fatalf("stale destroy cancellation error = %v, want conflict", err)
	}
	var taskStatus, operationStatus string
	var revision int64
	if err = db.QueryRowContext(ctx, `SELECT status FROM agent_tasks WHERE task_id=$1`, taskID).Scan(&taskStatus); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT status FROM core_workload_operations WHERE operation_id=$1`, operationID).Scan(&operationStatus); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, `SELECT revision FROM core_workloads WHERE workload_id=$1`, workloadID).Scan(&revision); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "waiting_user" || operationStatus != "waiting_user" || revision != 2 {
		t.Fatalf("stale destroy mutated state: task=%s operation=%s workload_revision=%d", taskStatus, operationStatus, revision)
	}
}
