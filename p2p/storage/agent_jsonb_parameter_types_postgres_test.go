package storage

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	confirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	task "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

// This calls the production AWS consume path with the same fully-linked rows
// used in the live confirmation flow. In particular, lib/pq must be able to
// infer every reservation_json parameter before the state transition occurs.
func TestPostgresAWSConsumeChangePersistsTypedReservationAtomically(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const (
		owner          = "@jsonb-aws-owner:example.test"
		credentialID   = "11111111-1111-4111-8111-111111111111"
		planID         = "22222222-2222-4222-8222-222222222222"
		changeID       = "33333333-3333-4333-8333-333333333333"
		taskID         = "44444444-4444-4444-8444-444444444444"
		confirmationID = "55555555-5555-4555-8555-555555555555"
		idempotencyKey = "66666666-6666-4666-8666-666666666666"
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	digest := strings.Repeat("a", 64)
	template := []byte(`{"Resources":{}}`)
	_, templateDigest, err := agentaws.NormalizeTemplate(template)
	if err != nil {
		t.Fatal(err)
	}
	plan := agentaws.Plan{ID: planID, CredentialID: credentialID, CredentialRevision: 1, Region: "ap-southeast-1", StackName: "typed-jsonb", Operation: agentaws.OperationCreate, Template: template, TemplateSHA256: templateDigest, Parameters: map[string]string{}, Tags: map[string]string{"service": agentaws.EC2ServiceProfile, "owner": agentaws.OwnerBindingDigest(owner)}, Capabilities: []string{}, Revision: 1, CreatedAt: now}
	params, _ := json.Marshal(plan.Parameters)
	tags, _ := json.Marshal(plan.Tags)
	caps, _ := json.Marshal(plan.Capabilities)
	credential := agentaws.RehydrateCredentialMetadata(credentialID, "test", plan.Region, "123456789012", "arn:aws:iam::123456789012:user/test", 1, 1, now, now)
	binding := agentaws.BindingForPlan(plan, credential)
	binding.OwnerID = owner
	bindingRaw, _ := json.Marshal(binding)
	spec, _ := (task.TaskSpec{Kind: task.TaskKindAWSChange, Payload: task.TaskPayload{AWSChange: &task.AWSChangeTaskPayload{ChangeID: changeID}}, Goal: "AWS change", IdempotencyKey: idempotencyKey, AvailableAt: now}).Normalize()
	specRaw, _ := json.Marshal(spec)
	providerDigest := agentaws.ProviderRequestDigest(plan, confirmationID)
	// Insert the immutable FK chain directly: the test is concerned with the
	// consumption transaction and deliberately does not invoke any provider.
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,lease_epoch,lease_holder,lease_expires_at,revision,available_at,created_at,updated_at) VALUES($1,$2,$3,'running',2,7,'worker',$4,9,$5,$5,$5)`, taskID, owner, specRaw, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,created_at,updated_at) VALUES($1,$2,'aws',$3,$4,$5,$6,$7,'confirmed',3,$8,$9,$9)`, confirmationID, owner, binding.TargetID, binding.TargetRevision, binding.Digest, bindingRaw, taskID, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) VALUES($1,$2,1,1,1,'test',decode('000000000000000000000000','hex'),decode('00','hex'),$3,'test',$4,$5,$6,1,$7,$7)`, owner, credentialID, digest, plan.Region, credential.AccountID, credential.UserARN, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO core_aws_plans(owner_id,plan_id,credential_id,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at) VALUES($1,$2,$3,1,$4,$5,'create',$6,$7,$8,$9,$10,1,$11)`, owner, planID, credentialID, plan.Region, plan.StackName, plan.Template, plan.TemplateSHA256, params, tags, caps, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO core_aws_changes(owner_id,change_id,plan_id,credential_id,credential_revision,task_id,confirmation_id,operation,status,stage,provider_token,provider_request_digest,revision,created_at,updated_at) VALUES($1,$2,$3,$4,1,$5,$6,'create','waiting_user','requested',$7,$8,4,$9,$9)`, owner, changeID, planID, credentialID, taskID, confirmationID, confirmationID, providerDigest, now); err != nil {
		t.Fatal(err)
	}

	repo, err := NewAgentAWSRepository(store, owner)
	if err != nil {
		t.Fatal(err)
	}
	reservation, err := repo.ConsumeChange(ctx, agentaws.ConsumeChangeCommand{
		ChangeID: changeID, ConfirmationID: confirmationID, TaskID: taskID,
		Attempt: 2, LeaseEpoch: 7, ExpectedChangeRevision: 4,
		ExpectedTaskRevision: 9, ExpectedConfirmationRevision: 3,
		IdempotencyKey: idempotencyKey, Binding: binding,
	})
	if err != nil {
		t.Fatalf("consume AWS change: %v", err)
	}
	if reservation.TaskID != taskID || reservation.Attempt != 2 || reservation.LeaseEpoch != 7 || reservation.TaskRevision != 9 || !reservation.Active {
		t.Fatalf("reservation = %#v", reservation)
	}

	var status, stage string
	var revision int64
	if err = store.DB().QueryRowContext(ctx, `SELECT status,stage,revision FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, owner, changeID).Scan(&status, &stage, &revision); err != nil {
		t.Fatal(err)
	}
	if status != "running" || stage != "change_set_creating" || revision != 5 {
		t.Fatalf("change transition = %q/%q/%d", status, stage, revision)
	}
	var reservationRaw []byte
	if err = store.DB().QueryRowContext(ctx, `SELECT reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2 AND state='consumed'`, owner, confirmationID).Scan(&reservationRaw); err != nil {
		t.Fatal(err)
	}
	var stored struct {
		TaskID       string `json:"task_id"`
		Attempt      uint32 `json:"attempt"`
		LeaseEpoch   uint64 `json:"lease_epoch"`
		TaskRevision int64  `json:"task_revision"`
		Active       bool   `json:"active"`
	}
	if err = json.Unmarshal(reservationRaw, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.TaskID != reservation.TaskID || stored.Attempt != reservation.Attempt || stored.LeaseEpoch != reservation.LeaseEpoch || stored.TaskRevision != reservation.TaskRevision || stored.Active != reservation.Active {
		t.Fatalf("stored reservation = %#v, want %#v", stored, reservation)
	}
	var eventCount, replayCount int
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM core_aws_events WHERE owner_id=$1 AND change_id=$2 AND task_id=$3 AND kind='change_consumed'`, owner, changeID, taskID).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("change event count=%d err=%v", eventCount, err)
	}
	if err = store.DB().QueryRowContext(ctx, `SELECT count(*) FROM core_aws_replays WHERE owner_id=$1 AND operation='change-consume' AND idempotency_key=$2`, owner, idempotencyKey).Scan(&replayCount); err != nil || replayCount != 1 {
		t.Fatalf("change replay count=%d err=%v", replayCount, err)
	}
}

func openAgentJSONBPostgres(t *testing.T) (context.Context, *DatabaseStore) {
	t.Helper()
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(closeDB)
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return ctx, store
}

func jsonbTestBinding(owner, target string) confirmation.Binding {
	digest := strings.Repeat("b", 64)
	return confirmation.Binding{
		OwnerID: owner, OperationDomain: "test.operation", TargetID: target, TargetRevision: 1,
		SourceVersion: "v1", ContentDigest: confirmation.Digest(digest), ParameterDigest: confirmation.Digest(digest),
		NetworkDigest: confirmation.Digest(digest), SecretGrantDigest: confirmation.Digest(digest), Digest: digest,
	}
}

func insertJSONBTestTask(t *testing.T, ctx context.Context, store *DatabaseStore, owner, id, status string, attempt int, epoch, revision int64, holder string, lease *time.Time, at time.Time) {
	t.Helper()
	spec, err := json.Marshal(task.TaskSpec{Kind: task.TaskKindAgent, Goal: "JSONB parameter type test", ModelProfileID: "00000000-0000-4000-8000-000000000001", IdempotencyKey: id, AvailableAt: at})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,lease_epoch,lease_holder,lease_expires_at,revision,progress_sequence,available_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,0,$10,$10,$10)`, id, owner, spec, status, attempt, epoch, holder, lease, revision, at); err != nil {
		t.Fatal(err)
	}
}

func insertJSONBTestConfirmation(t *testing.T, ctx context.Context, store *DatabaseStore, owner, id, taskID, state string, revision int64, binding confirmation.Binding, expiresAt, at time.Time, reservation string) {
	t.Helper()
	raw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	var reservationValue any
	if reservation != "" {
		reservationValue = reservation
	}
	if _, err = store.DB().ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,reservation_json,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12::jsonb,$13,$13)`, id, owner, binding.OperationDomain, binding.TargetID, binding.TargetRevision, binding.Digest, raw, taskID, state, revision, expiresAt, reservationValue, at); err != nil {
		t.Fatal(err)
	}
}

func TestPostgresConfirmationMutationsPersistTypedJSONB(t *testing.T) {
	ctx, store := openAgentJSONBPostgres(t)
	const owner = "@jsonb-confirmation:example.test"
	now := time.Now().UTC().Truncate(time.Microsecond)
	confirmations := NewDatabaseConfirmationStore(store.DB())

	t.Run("reject event reason is text", func(t *testing.T) {
		const taskID = "10000000-0000-4000-8000-000000000001"
		const confirmationID = "10000000-0000-4000-8000-000000000002"
		insertJSONBTestTask(t, ctx, store, owner, taskID, "waiting_user", 0, 0, 7, "", nil, now)
		binding := jsonbTestBinding(owner, "reject-target")
		insertJSONBTestConfirmation(t, ctx, store, owner, confirmationID, taskID, "pending", 1, binding, now.Add(time.Hour), now, "")
		out, err := confirmations.Reject(ctx, confirmation.RejectCommand{OwnerID: owner, ConfirmationID: confirmationID, IdempotencyKey: "10000000-0000-4000-8000-000000000003", ExpectedRevision: 1, Reason: "owner declined", At: now})
		if err != nil || out.State != confirmation.StateRejected {
			t.Fatalf("Reject = %#v, %v", out, err)
		}
		var state, kind, reason string
		if err = store.DB().QueryRowContext(ctx, `SELECT c.state,jsonb_typeof(e.payload_json),jsonb_typeof(e.payload_json->'reason') FROM agent_confirmations c JOIN agent_task_events e ON e.owner_id=c.owner_id AND e.task_id=c.task_id WHERE c.confirmation_id=$1 AND e.event_type='confirmation_rejected'`, confirmationID).Scan(&state, &kind, &reason); err != nil || state != "rejected" || kind != "object" || reason != "string" {
			t.Fatalf("rejected JSONB = state=%q event=%q reason=%q err=%v", state, kind, reason, err)
		}
	})

	t.Run("consume reservation uses UUID integer and bigint JSON values", func(t *testing.T) {
		const taskID = "10000000-0000-4000-8000-000000000004"
		const confirmationID = "10000000-0000-4000-8000-000000000005"
		lease := now.Add(time.Hour)
		insertJSONBTestTask(t, ctx, store, owner, taskID, "running", 2, 7, 9, "worker", &lease, now)
		binding := jsonbTestBinding(owner, "consume-target")
		insertJSONBTestConfirmation(t, ctx, store, owner, confirmationID, taskID, "confirmed", 3, binding, now.Add(time.Hour), now, "")
		out, err := confirmations.Consume(ctx, confirmation.ConsumeCommand{OwnerID: owner, ConfirmationID: confirmationID, TaskID: taskID, IdempotencyKey: "10000000-0000-4000-8000-000000000006", Attempt: 2, LeaseEpoch: 7, ExpectedRevision: 3, ExpectedTaskRevision: 9, Binding: binding, At: now})
		if err != nil || out.State != confirmation.StateConsumed {
			t.Fatalf("Consume = %#v, %v", out, err)
		}
		var taskType, attemptType, epochType, revisionType string
		if err = store.DB().QueryRowContext(ctx, `SELECT jsonb_typeof(reservation_json->'task_id'),jsonb_typeof(reservation_json->'attempt'),jsonb_typeof(reservation_json->'lease_epoch'),jsonb_typeof(reservation_json->'task_revision') FROM agent_confirmations WHERE confirmation_id=$1`, confirmationID).Scan(&taskType, &attemptType, &epochType, &revisionType); err != nil || taskType != "string" || attemptType != "number" || epochType != "number" || revisionType != "number" {
			t.Fatalf("reservation JSONB types=%q/%q/%q/%q err=%v", taskType, attemptType, epochType, revisionType, err)
		}
	})

	t.Run("expired consume terminalizes with typed event", func(t *testing.T) {
		const taskID = "10000000-0000-4000-8000-000000000007"
		const confirmationID = "10000000-0000-4000-8000-000000000008"
		lease := now.Add(time.Hour)
		insertJSONBTestTask(t, ctx, store, owner, taskID, "running", 1, 1, 4, "worker", &lease, now)
		binding := jsonbTestBinding(owner, "expired-target")
		insertJSONBTestConfirmation(t, ctx, store, owner, confirmationID, taskID, "confirmed", 2, binding, now.Add(-time.Second), now, "")
		_, err := confirmations.Consume(ctx, confirmation.ConsumeCommand{OwnerID: owner, ConfirmationID: confirmationID, TaskID: taskID, IdempotencyKey: "10000000-0000-4000-8000-000000000009", Attempt: 1, LeaseEpoch: 1, ExpectedRevision: 2, ExpectedTaskRevision: 4, Binding: binding, At: now})
		if !errors.Is(err, confirmation.ErrExpired) {
			t.Fatalf("expired Consume error = %v", err)
		}
		var confirmationState, taskState, reasonType string
		if err = store.DB().QueryRowContext(ctx, `SELECT c.state,t.status,jsonb_typeof(e.payload_json->'reason') FROM agent_confirmations c JOIN agent_tasks t ON t.owner_id=c.owner_id AND t.task_id=c.task_id JOIN agent_task_events e ON e.owner_id=c.owner_id AND e.task_id=c.task_id WHERE c.confirmation_id=$1 AND e.event_type='confirmation_expired'`, confirmationID).Scan(&confirmationState, &taskState, &reasonType); err != nil || confirmationState != "expired" || taskState != "failed" || reasonType != "string" {
			t.Fatalf("expired terminalization = %q/%q/%q err=%v", confirmationState, taskState, reasonType, err)
		}
	})
}

func TestPostgresTaskMutationsPersistTypedJSONB(t *testing.T) {
	ctx, store := openAgentJSONBPostgres(t)
	const owner = "@jsonb-task:example.test"
	now := time.Now().UTC().Truncate(time.Microsecond)
	queue := NewDatabaseTaskStore(store.DB())

	t.Run("terminal transition event code is text", func(t *testing.T) {
		const taskID = "20000000-0000-4000-8000-000000000001"
		lease := now.Add(time.Hour)
		insertJSONBTestTask(t, ctx, store, owner, taskID, "running", 2, 3, 6, "worker", &lease, now)
		out, err := queue.transition(ctx, task.Fence{TaskID: taskID, Attempt: 2, LeaseEpoch: 3, ExpectedRevision: 6}, task.StatusFailed, "typed_failure")
		if err != nil || out.Status != task.StatusFailed {
			t.Fatalf("transition = %#v, %v", out, err)
		}
		var payloadType, codeType string
		if err = store.DB().QueryRowContext(ctx, `SELECT jsonb_typeof(payload_json),jsonb_typeof(payload_json->'code') FROM agent_task_events WHERE task_id=$1 AND event_type='failed'`, taskID).Scan(&payloadType, &codeType); err != nil || payloadType != "object" || codeType != "string" {
			t.Fatalf("transition event JSONB = %q/%q err=%v", payloadType, codeType, err)
		}
	})

	t.Run("wait user event reason is text", func(t *testing.T) {
		const taskID = "20000000-0000-4000-8000-000000000002"
		lease := now.Add(time.Hour)
		insertJSONBTestTask(t, ctx, store, owner, taskID, "running", 1, 1, 5, "worker", &lease, now)
		if err := queue.WaitUser(ctx, task.WaitUserCommand{Fence: task.Fence{TaskID: taskID, Attempt: 1, LeaseEpoch: 1, ExpectedRevision: 5}, Reason: "confirmation required", At: now}); err != nil {
			t.Fatal(err)
		}
		var status, reasonType string
		if err := store.DB().QueryRowContext(ctx, `SELECT t.status,jsonb_typeof(e.payload_json->'reason') FROM agent_tasks t JOIN agent_task_events e ON e.owner_id=t.owner_id AND e.task_id=t.task_id WHERE t.task_id=$1 AND e.event_type='waiting_user'`, taskID).Scan(&status, &reasonType); err != nil || status != "waiting_user" || reasonType != "string" {
			t.Fatalf("WaitUser JSONB = status=%q reason=%q err=%v", status, reasonType, err)
		}
	})

	t.Run("cancel terminalization releases consumed reservation", func(t *testing.T) {
		const taskID = "20000000-0000-4000-8000-000000000003"
		const confirmationID = "20000000-0000-4000-8000-000000000004"
		insertJSONBTestTask(t, ctx, store, owner, taskID, "queued", 0, 0, 4, "", nil, now)
		binding := jsonbTestBinding(owner, "cancel-target")
		insertJSONBTestConfirmation(t, ctx, store, owner, confirmationID, taskID, "consumed", 2, binding, now.Add(time.Hour), now, `{"task_id":"20000000-0000-4000-8000-000000000003","attempt":1,"lease_epoch":1,"task_revision":3,"active":true}`)
		out, err := queue.CancelTask(ctx, task.CancelCommand{OwnerID: owner, TaskID: taskID, ExpectedRevision: 4, Mutation: task.MutationCommand{IdempotencyKey: "20000000-0000-4000-8000-000000000005", ExpectedRevision: 4}, Reason: "owner canceled", At: now})
		if err != nil || out.Status != task.StatusCanceled {
			t.Fatalf("CancelTask = %#v, %v", out, err)
		}
		var reservationType, reasonType, replayType string
		if err = store.DB().QueryRowContext(ctx, `SELECT COALESCE(jsonb_typeof(c.reservation_json),'null'),jsonb_typeof(e.payload_json->'reason'),jsonb_typeof(r.response_json) FROM agent_confirmations c JOIN agent_task_events e ON e.owner_id=c.owner_id AND e.task_id=c.task_id JOIN agent_task_replays r ON r.owner_id=c.owner_id WHERE c.confirmation_id=$1 AND e.event_type='canceled' AND r.operation='cancel'`, confirmationID).Scan(&reservationType, &reasonType, &replayType); err != nil || reservationType != "null" || reasonType != "string" || replayType != "object" {
			t.Fatalf("CancelTask JSONB = reservation=%q reason=%q replay=%q err=%v", reservationType, reasonType, replayType, err)
		}
	})

	t.Run("retry event references predecessor as text", func(t *testing.T) {
		const taskID = "20000000-0000-4000-8000-000000000006"
		insertJSONBTestTask(t, ctx, store, owner, taskID, "canceled", 1, 1, 4, "", nil, now)
		out, err := queue.RetryTask(ctx, task.RetryCommand{TaskID: taskID, Mutation: task.MutationCommand{IdempotencyKey: "20000000-0000-4000-8000-000000000007", ExpectedRevision: 4}, At: now})
		if err != nil || out.RetryOfTaskID != taskID {
			t.Fatalf("RetryTask = %#v, %v", out, err)
		}
		var predecessorType, replayType string
		if err = store.DB().QueryRowContext(ctx, `SELECT jsonb_typeof(e.payload_json->'retry_of_task_id'),jsonb_typeof(r.response_json) FROM agent_task_events e JOIN agent_task_replays r ON r.owner_id=e.owner_id WHERE e.task_id=$1 AND e.event_type='created' AND r.operation='retry'`, out.ID).Scan(&predecessorType, &replayType); err != nil || predecessorType != "string" || replayType != "object" {
			t.Fatalf("RetryTask JSONB = predecessor=%q replay=%q err=%v", predecessorType, replayType, err)
		}
	})
}

func TestPostgresWorkloadConsumeFencedPersistsTypedReservation(t *testing.T) {
	ctx, store := openAgentJSONBPostgres(t)
	const (
		owner          = "@jsonb-workload:example.test"
		taskID         = "30000000-0000-4000-8000-000000000001"
		confirmationID = "30000000-0000-4000-8000-000000000002"
		planID         = "30000000-0000-4000-8000-000000000003"
		workloadID     = "30000000-0000-4000-8000-000000000004"
		operationID    = "30000000-0000-4000-8000-000000000005"
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	lease := now.Add(time.Hour)
	digest := strings.Repeat("c", 64)
	insertJSONBTestTask(t, ctx, store, owner, taskID, "running", 1, 2, 5, "worker", &lease, now)
	binding := jsonbTestBinding(owner, "workload-target")
	insertJSONBTestConfirmation(t, ctx, store, owner, confirmationID, taskID, "confirmed", 3, binding, now.Add(time.Hour), now, "")
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_workload_plans(plan_id,owner_id,create_idempotency_key,create_request_hash,revision,digest,summary,plan_json,target_kind,expires_at,created_at) VALUES($1,$2,'30000000-0000-4000-8000-000000000006',$3,1,$3,'typed reservation','{}','AWS_EC2_SSM',$4,$5)`, planID, owner, digest, now.Add(time.Hour), now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_workloads(workload_id,owner_id,revision,plan_id,plan_digest,target_kind,state,updated_at) VALUES($1,$2,1,$3,$4,'AWS_EC2_SSM','pending',$5)`, workloadID, owner, planID, digest, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO core_workload_operations(operation_id,owner_id,workload_id,expected_workload_revision,plan_id,operation,plan_revision,plan_digest,target_kind,task_id,confirmation_id,status,revision,created_at,updated_at,dispatch_state) VALUES($1,$2,$3,1,$4,'apply',1,$5,'AWS_EC2_SSM',$6,$7,'waiting_user',2,$8,$8,'prepared')`, operationID, owner, workloadID, planID, digest, taskID, confirmationID, now); err != nil {
		t.Fatal(err)
	}
	repo, err := NewAgentWorkloadStore(store, owner)
	if err != nil {
		t.Fatal(err)
	}
	out, gotTask, err := repo.ConsumeFenced(ctx, operationID, confirmationID, digest, 2, workload.TaskFence{TaskID: taskID, Attempt: 1, LeaseEpoch: 2, Revision: 5, Holder: "worker", ExpiresAt: lease})
	if err != nil || out.Status != "running" || gotTask.ID != taskID {
		t.Fatalf("ConsumeFenced = operation=%#v task=%#v err=%v", out, gotTask, err)
	}
	var activeType, taskType, attemptType, epochType, revisionType string
	if err = store.DB().QueryRowContext(ctx, `SELECT jsonb_typeof(reservation_json->'active'),jsonb_typeof(reservation_json->'task_id'),jsonb_typeof(reservation_json->'attempt'),jsonb_typeof(reservation_json->'lease_epoch'),jsonb_typeof(reservation_json->'task_revision') FROM agent_confirmations WHERE confirmation_id=$1`, confirmationID).Scan(&activeType, &taskType, &attemptType, &epochType, &revisionType); err != nil || activeType != "boolean" || taskType != "string" || attemptType != "number" || epochType != "number" || revisionType != "number" {
		t.Fatalf("ConsumeFenced reservation JSONB = %q/%q/%q/%q/%q err=%v", activeType, taskType, attemptType, epochType, revisionType, err)
	}
}
