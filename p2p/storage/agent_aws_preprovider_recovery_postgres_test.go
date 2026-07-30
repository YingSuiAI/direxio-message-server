package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	agenttask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

type preProviderPostgresFixture struct {
	ctx                                                                        context.Context
	owner, planID, credentialID, provisionID, changeID, taskID, confirmationID string
	plan                                                                       agentaws.Plan
	provision                                                                  agentaws.Provision
	change                                                                     agentaws.Change
	task                                                                       agenttask.Task
	confirmation                                                               coreconfirmation.Confirmation
}

func newPreProviderPostgresFixture(t *testing.T) (*DatabaseStore, *PostgresAWSRepository, preProviderPostgresFixture) {
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

	fx := preProviderPostgresFixture{
		ctx: ctx, owner: "@pre-provider:example.test", planID: "11111111-1111-4111-8111-111111111111", credentialID: "22222222-2222-4222-8222-222222222222",
		provisionID: "33333333-3333-4333-8333-333333333333", changeID: "44444444-4444-4444-8444-444444444444", taskID: "55555555-5555-4555-8555-555555555555", confirmationID: "66666666-6666-4666-8666-666666666666",
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	template, templateDigest, err := agentaws.NormalizeTemplate([]byte(`{"Resources":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	fx.plan = agentaws.Plan{ID: fx.planID, CredentialID: fx.credentialID, CredentialRevision: 1, Region: "us-east-1", StackName: "pre-provider", Operation: agentaws.OperationCreate, Template: template, TemplateSHA256: templateDigest, Parameters: map[string]string{}, Tags: map[string]string{"service": agentaws.EC2ServiceProfile, "owner": agentaws.OwnerBindingDigest(fx.owner)}, Capabilities: []string{}, Revision: 1, CreatedAt: now}
	fx.provision = agentaws.Provision{ID: fx.provisionID, PlanID: fx.planID, CredentialID: fx.credentialID, CredentialRevision: 1, Region: fx.plan.Region, StackName: fx.plan.StackName, Profile: agentaws.EC2ServiceProfile, OwnerDigest: fx.plan.Tags["owner"], PlanRevision: 1, TemplateSHA256: templateDigest, PlanDigest: agentaws.PlanDigest(fx.plan), State: "creating", Revision: 2, ActiveChangeID: fx.changeID, CreatedAt: now, UpdatedAt: now}
	cred := agentaws.RehydrateCredentials(fx.credentialID, "pre-provider", fx.plan.Region, "123456789012", "arn:aws:iam::123456789012:user/test", []byte("access"), []byte("secret"), nil, 1, 1, now, now)
	binding := agentaws.BindingForPlan(fx.plan, cred)
	binding.OwnerID = fx.owner
	// RetryProvision validates the plan linkage itself; preserve the persisted
	// owner and grant facts while making the target linkage explicit.
	binding.Digest = strings.Repeat("b", 64)
	bindingRaw, err := json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	taskSpec, _ := json.Marshal(agenttask.TaskSpec{Kind: agenttask.TaskKindAWSChange, IdempotencyKey: "77777777-7777-4777-8777-777777777777", Payload: agenttask.TaskPayload{AWSChange: &agenttask.AWSChangeTaskPayload{ChangeID: fx.changeID}}})
	fx.task = agenttask.Task{ID: fx.taskID, OwnerID: fx.owner, Status: agenttask.StatusFailed, Attempt: 1, LeaseEpoch: 4, Revision: 7, FailureCode: "task_execution_failed", FailureSummary: "consume failed"}
	fx.confirmation = coreconfirmation.Confirmation{ConfirmationID: fx.confirmationID, OwnerID: fx.owner, TaskID: fx.taskID, State: coreconfirmation.StateExpired, Revision: 3, TerminalReason: "task_execution_failed", Binding: binding}
	// This orphan mirrors the currently persisted RequestChange shape from the
	// legacy V8 path; recovery accepts it only for the strict pre-provider fence.
	providerDigest := stringDigest(struct{ Plan, Token string }{fx.planID, fx.confirmationID})
	fx.change = agentaws.Change{ID: fx.changeID, PlanID: fx.planID, CredentialID: fx.credentialID, ProvisionID: fx.provisionID, TaskID: fx.taskID, ConfirmationID: fx.confirmationID, Operation: agentaws.OperationCreate, Status: agentaws.ChangeWaitingUser, Stage: agentaws.StageRequested, Revision: 1, ProviderToken: fx.confirmationID, ProviderRequestDigest: providerDigest, CreatedAt: now, UpdatedAt: now}
	db := store.DB()
	if _, err = db.ExecContext(ctx, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) VALUES($1,$2,1,1,1,'test',decode('000000000000000000000000','hex'),decode('00','hex'),$3,'pre-provider',$4,'123456789012','arn:aws:iam::123456789012:user/test',1,$5,$5)`, fx.owner, fx.credentialID, strings.Repeat("a", 64), fx.plan.Region, now); err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(fx.plan.Parameters)
	tags, _ := json.Marshal(fx.plan.Tags)
	caps, _ := json.Marshal(fx.plan.Capabilities)
	if _, err = db.ExecContext(ctx, `INSERT INTO core_aws_plans(owner_id,plan_id,credential_id,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at) VALUES($1,$2,$3,1,$4,$5,'create',$6,$7,$8,$9,$10,1,$11)`, fx.owner, fx.planID, fx.credentialID, fx.plan.Region, fx.plan.StackName, fx.plan.Template, fx.plan.TemplateSHA256, params, tags, caps, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,lease_epoch,revision,available_at,failure_code,failure_summary,created_at,updated_at) VALUES($1,$2,$3,'failed',1,4,7,$4,'task_execution_failed','consume failed',$4,$4)`, fx.taskID, fx.owner, taskSpec, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,terminal_reason,created_at,updated_at) VALUES($1,$2,'aws',$3,1,$4,$5,$6,'expired',3,$7,'task_execution_failed',$8,$8)`, fx.confirmationID, fx.owner, fx.planID, binding.Digest, bindingRaw, fx.taskID, now.Add(-time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_aws_ec2_provisions(owner_id,provision_id,plan_id,credential_id,credential_revision,region,stack_name,profile,owner_digest,plan_revision,template_sha256,plan_digest,state,revision,active_change_id,created_at,updated_at) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8,1,$9,$10,'creating',2,$11,$12,$12)`, fx.owner, fx.provisionID, fx.planID, fx.credentialID, fx.plan.Region, fx.plan.StackName, fx.provision.Profile, fx.provision.OwnerDigest, fx.provision.TemplateSHA256, fx.provision.PlanDigest, fx.changeID, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_aws_changes(owner_id,change_id,plan_id,credential_id,credential_revision,provision_id,task_id,confirmation_id,operation,status,stage,provider_token,provider_request_digest,revision,created_at,updated_at) VALUES($1,$2,$3,$4,1,$5,$6,$7,'create','waiting_user','requested',$8,$9,1,$10,$10)`, fx.owner, fx.changeID, fx.planID, fx.credentialID, fx.provisionID, fx.taskID, fx.confirmationID, fx.confirmationID, providerDigest, now); err != nil {
		t.Fatal(err)
	}
	repo, err := NewAgentAWSRepository(store, fx.owner)
	if err != nil {
		t.Fatal(err)
	}
	return store, repo, fx
}

func TestPostgresRetryProvisionRearmsExactPreProviderOrphanAndReplays(t *testing.T) {
	store, repo, fx := newPreProviderPostgresFixture(t)
	key := "77777777-7777-4777-8777-777777777777"
	first, err := repo.RetryProvision(fx.ctx, fx.provisionID, 2, key)
	if err != nil {
		t.Fatalf("RetryProvision = %v", err)
	}
	if first.State != "planned" || first.ActiveChangeID != "" || first.Revision != 3 {
		t.Fatalf("rearmed provision = %+v", first)
	}
	var status, stage, code, summary string
	var revision int64
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT status,stage,error_code,error_summary,revision FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID).Scan(&status, &stage, &code, &summary, &revision); err != nil {
		t.Fatal(err)
	}
	if status != "canceled" || stage != "canceled" || code != "pre_provider_rearmed" || summary != code || revision != 2 {
		t.Fatalf("old change after rearm = %q/%q/%q/%q/%d", status, stage, code, summary, revision)
	}
	var taskStatus, failureCode, confirmationState, terminalReason string
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT t.status,t.failure_code,c.state,c.terminal_reason FROM agent_tasks t JOIN agent_confirmations c ON c.owner_id=t.owner_id AND c.task_id=t.task_id WHERE t.owner_id=$1 AND t.task_id=$2`, fx.owner, fx.taskID).Scan(&taskStatus, &failureCode, &confirmationState, &terminalReason); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "failed" || failureCode != "task_execution_failed" || confirmationState != "expired" || terminalReason != "task_execution_failed" {
		t.Fatalf("old task/confirmation mutated = %q/%q/%q/%q", taskStatus, failureCode, confirmationState, terminalReason)
	}
	var eventKind string
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT kind FROM core_aws_ec2_provision_events WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&eventKind); err != nil {
		t.Fatal(err)
	}
	if eventKind != "provision_preprovider_rearmed" {
		t.Fatalf("provision event kind = %q", eventKind)
	}
	var replayCount int
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_aws_replays WHERE owner_id=$1 AND operation='provision-retry' AND idempotency_key=$2`, fx.owner, key).Scan(&replayCount); err != nil || replayCount != 1 {
		t.Fatalf("retry replay count=%d err=%v", replayCount, err)
	}
	second, err := repo.RetryProvision(fx.ctx, fx.provisionID, 2, key)
	if err != nil || second.ID != first.ID || second.State != first.State || second.Revision != first.Revision || second.ActiveChangeID != first.ActiveChangeID {
		t.Fatalf("retry replay = %+v/%+v err=%v", first, second, err)
	}
	var eventCount int
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_aws_ec2_provision_events WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&eventCount); err != nil || eventCount != 1 {
		t.Fatalf("event count after replay=%d err=%v", eventCount, err)
	}
	service := agentaws.NewServiceWithCoordinator(repo, repo, nil, nil, nil, nil, nil)
	fresh, err := service.RequestEC2Create(fx.ctx, fx.provisionID, 3, "88888888-8888-4888-8888-888888888888", fx.owner)
	if err != nil {
		t.Fatalf("fresh RequestEC2Create = %v", err)
	}
	if fresh.Change.ID == fx.changeID || fresh.Task.ID == fx.taskID || fresh.Confirmation.ConfirmationID == fx.confirmationID || fresh.Change.Status != agentaws.ChangeWaitingUser || fresh.Change.Stage != agentaws.StageRequested {
		t.Fatalf("fresh request did not allocate new durable IDs: %+v", fresh)
	}
	var freshDigest, freshToken string
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT provider_request_digest,provider_token FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, fresh.Change.ID).Scan(&freshDigest, &freshToken); err != nil {
		t.Fatal(err)
	}
	if freshDigest != agentaws.ProviderRequestDigest(fx.plan, freshToken) {
		t.Fatalf("fresh provider digest = %q, want canonical digest", freshDigest)
	}
}

func TestPostgresAWSPreProviderFailureCompletesAllFencesAtomically(t *testing.T) {
	store, repo, fx := newPreProviderPostgresFixture(t)
	now := time.Now().UTC()
	initialReservation, err := json.Marshal(map[string]any{"confirmation_id": fx.confirmationID, "task_id": fx.taskID, "attempt": 1, "lease_epoch": 4, "task_revision": 7, "active": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET status='running',lease_holder='worker',lease_expires_at=$1,failure_code='',failure_summary='' WHERE owner_id=$2 AND task_id=$3`, now.Add(time.Hour), fx.owner, fx.taskID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='consumed',revision=4,reservation_json=$1::jsonb,terminal_reason='' WHERE owner_id=$2 AND confirmation_id=$3`, initialReservation, fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET status='running',stage='change_set_creating',revision=2,provider_request_digest=$1 WHERE owner_id=$2 AND change_id=$3`, agentaws.ProviderRequestDigest(fx.plan, fx.confirmationID), fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `INSERT INTO agent_task_runtime_concurrency(singleton,running_count,max_concurrent,revision,updated_at) VALUES(true,1,4,1,$1) ON CONFLICT(singleton) DO UPDATE SET running_count=1,max_concurrent=4,updated_at=$1`, now); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `INSERT INTO core_aws_event_counters(owner_id,change_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,change_id) DO UPDATE SET next_sequence=2`, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,1,$3,$4,'change_consumed',2,$5)`, fx.owner, fx.changeID, "99999999-9999-4999-8999-999999999999", fx.taskID, now); err != nil {
		t.Fatal(err)
	}
	completion := agentaws.CompleteChangeCommand{
		ChangeID: fx.changeID, ConfirmationID: fx.confirmationID, TaskID: fx.taskID,
		Attempt: 1, LeaseEpoch: 4, ExpectedTaskRevision: 7, ExpectedChangeRevision: 2,
		ExpectedConfirmationRevision: 4, Status: agentaws.ChangeFailed,
		ErrorCode: "provider_error", ErrorSummary: "AWS provider dispatch failed before mutation",
		OperationKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaab",
	}
	completed, err := repo.CompleteChange(fx.ctx, completion)
	if err != nil || completed.Status != agentaws.ChangeFailed || completed.Stage != agentaws.StageFailed {
		t.Fatalf("CompleteChange err=%v status=%q stage=%q", err, completed.Status, completed.Stage)
	}
	var taskStatus, leaseHolder, changeStatus, changeStage, confirmationState string
	var leaseExpires *time.Time
	var reservation []byte
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT t.status,t.lease_holder,t.lease_expires_at,ch.status,ch.stage,c.state,c.reservation_json FROM agent_tasks t JOIN core_aws_changes ch ON ch.owner_id=t.owner_id AND ch.task_id=t.task_id JOIN agent_confirmations c ON c.owner_id=t.owner_id AND c.task_id=t.task_id WHERE t.owner_id=$1 AND t.task_id=$2`, fx.owner, fx.taskID).Scan(&taskStatus, &leaseHolder, &leaseExpires, &changeStatus, &changeStage, &confirmationState, &reservation); err != nil {
		t.Fatal(err)
	}
	if taskStatus != "failed" || leaseHolder != "" || leaseExpires != nil || changeStatus != "failed" || changeStage != "failed" || confirmationState != "consumed" || reservation != nil {
		t.Fatalf("terminal fence state = task %q/%q change %q/%q confirmation %q reservation=%s", taskStatus, leaseHolder, changeStatus, changeStage, confirmationState, reservation)
	}
	var provisionState, activeChange, createChange string
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,''),COALESCE(create_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&provisionState, &activeChange, &createChange); err != nil {
		t.Fatal(err)
	}
	if provisionState != "failed" || activeChange != "" || createChange != fx.changeID {
		t.Fatalf("provision state = %q active=%q create=%q", provisionState, activeChange, createChange)
	}
	var running int
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT running_count FROM agent_task_runtime_concurrency WHERE singleton=true`).Scan(&running); err != nil || running != 0 {
		t.Fatalf("runtime running_count=%d err=%v", running, err)
	}
	var taskEvents, changeEvents, provisionEvents, replays, providerReplays, providerEvents int
	_ = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM agent_task_events WHERE owner_id=$1 AND task_id=$2 AND event_type='aws_change_completed'`, fx.owner, fx.taskID).Scan(&taskEvents)
	_ = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_aws_events WHERE owner_id=$1 AND change_id=$2 AND kind='change_completed'`, fx.owner, fx.changeID).Scan(&changeEvents)
	_ = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_aws_ec2_provision_events WHERE owner_id=$1 AND provision_id=$2 AND kind='provision_failed'`, fx.owner, fx.provisionID).Scan(&provisionEvents)
	_ = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_aws_replays WHERE owner_id=$1 AND operation='change-complete' AND idempotency_key=$2`, fx.owner, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaab").Scan(&replays)
	_ = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_aws_replays WHERE owner_id=$1 AND operation='provider-mutation'`, fx.owner).Scan(&providerReplays)
	_ = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_aws_events WHERE owner_id=$1 AND change_id=$2 AND kind='provider_mutation_dispatched'`, fx.owner, fx.changeID).Scan(&providerEvents)
	if taskEvents != 1 || changeEvents != 1 || provisionEvents != 1 || replays != 1 || providerReplays != 0 || providerEvents != 0 {
		t.Fatalf("completion evidence task=%d change=%d provision=%d replay=%d provider_replays=%d provider_events=%d", taskEvents, changeEvents, provisionEvents, replays, providerReplays, providerEvents)
	}
	if replayed, replayErr := repo.CompleteChange(fx.ctx, completion); replayErr != nil || replayed.ID != completed.ID {
		t.Fatalf("completion replay = %+v err=%v", replayed, replayErr)
	}
	var replayEvents int
	_ = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_aws_events WHERE owner_id=$1 AND change_id=$2 AND kind='change_completed'`, fx.owner, fx.changeID).Scan(&replayEvents)
	if replayEvents != 1 {
		t.Fatalf("change completion event count after replay=%d", replayEvents)
	}
	rearmed, err := repo.RetryProvision(fx.ctx, fx.provisionID, 3, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil || rearmed.State != "planned" || rearmed.ActiveChangeID != "" || rearmed.Revision != 4 {
		t.Fatalf("RetryProvision after atomic completion = %+v err=%v", rearmed, err)
	}
	service := agentaws.NewServiceWithCoordinator(repo, repo, nil, nil, nil, nil, nil)
	fresh, err := service.RequestEC2Create(fx.ctx, fx.provisionID, rearmed.Revision, "cccccccc-cccc-4ccc-8ccc-cccccccccccd", fx.owner)
	if err != nil || fresh.Confirmation.ConfirmationID == fx.confirmationID || fresh.Change.ConfirmationID == fx.confirmationID {
		t.Fatalf("fresh confirmation after terminal release = %+v err=%v", fresh, err)
	}
}

func TestPostgresV109ReleasesOnlyTerminalConsumedReservations(t *testing.T) {
	store, _, fx := newPreProviderPostgresFixture(t)
	reservation := []byte(`{"task_id":"` + fx.taskID + `","attempt":1,"lease_epoch":4,"task_revision":7,"active":false}`)
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='consumed',revision=4,reservation_json=$1::jsonb,terminal_reason='' WHERE owner_id=$2 AND confirmation_id=$3`, reservation, fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET status='failed',stage='failed' WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	var eligible int
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM agent_confirmations c JOIN core_aws_changes ch ON c.owner_id=ch.owner_id AND c.confirmation_id=ch.confirmation_id AND c.task_id=ch.task_id JOIN agent_tasks t ON t.owner_id=ch.owner_id AND t.task_id=ch.task_id WHERE c.owner_id=$1 AND c.confirmation_id=$2 AND c.state='consumed' AND c.reservation_json ? 'active' AND (c.reservation_json->>'active')::boolean=false AND ch.status IN ('succeeded','failed','canceled') AND ch.stage IN ('succeeded','failed','canceled') AND t.status IN ('succeeded','failed','canceled')`, fx.owner, fx.confirmationID).Scan(&eligible); err != nil || eligible != 1 {
		t.Fatalf("v109 fixture eligible=%d err=%v", eligible, err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `DELETE FROM db_migrations WHERE version=$1`, "p2p: release terminal confirmation reservations v109"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(fx.ctx); err != nil {
		t.Fatal(err)
	}
	var released []byte
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2`, fx.owner, fx.confirmationID).Scan(&released); err != nil {
		t.Fatal(err)
	}
	if released != nil {
		t.Fatalf("v109 left terminal reservation envelope: %s", released)
	}
}

func TestPostgresAWSUncertainCompletionRetainsReservationFence(t *testing.T) {
	store, repo, fx := newPreProviderPostgresFixture(t)
	now := time.Now().UTC()
	reservation, err := json.Marshal(map[string]any{"confirmation_id": fx.confirmationID, "task_id": fx.taskID, "attempt": 1, "lease_epoch": 4, "task_revision": 7, "active": true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET status='running',lease_holder='worker',lease_expires_at=$1,failure_code='',failure_summary='' WHERE owner_id=$2 AND task_id=$3`, now.Add(time.Hour), fx.owner, fx.taskID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='consumed',revision=4,reservation_json=$1::jsonb,terminal_reason='' WHERE owner_id=$2 AND confirmation_id=$3`, reservation, fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET status='running',stage='reconciling',revision=2 WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `INSERT INTO agent_task_runtime_concurrency(singleton,running_count,max_concurrent,revision,updated_at) VALUES(true,1,4,1,$1) ON CONFLICT(singleton) DO UPDATE SET running_count=1,max_concurrent=4,updated_at=$1`, now); err != nil {
		t.Fatal(err)
	}
	completion := agentaws.CompleteChangeCommand{ChangeID: fx.changeID, ConfirmationID: fx.confirmationID, TaskID: fx.taskID, Attempt: 1, LeaseEpoch: 4, ExpectedTaskRevision: 7, ExpectedChangeRevision: 2, ExpectedConfirmationRevision: 4, Status: agentaws.ChangeCanceled, ErrorCode: "canceled_after_dispatch", ErrorSummary: "provider response lost", OperationKey: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"}
	if completed, completeErr := repo.CompleteChange(fx.ctx, completion); completeErr != nil || completed.Stage != agentaws.StageReconciliationRequired {
		t.Fatalf("uncertain completion = %+v err=%v", completed, completeErr)
	}
	var retained []byte
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2`, fx.owner, fx.confirmationID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if len(retained) == 0 || !strings.Contains(string(retained), `"active": false`) {
		t.Fatalf("uncertain completion lost reservation fence: %s", retained)
	}
}

func TestPostgresV109PreservesAWSReconciliationReservation(t *testing.T) {
	store, _, fx := newPreProviderPostgresFixture(t)
	reservation := []byte(`{"task_id":"` + fx.taskID + `","attempt":1,"lease_epoch":4,"task_revision":7,"active":false}`)
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='consumed',revision=4,reservation_json=$1::jsonb,terminal_reason='' WHERE owner_id=$2 AND confirmation_id=$3`, reservation, fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET status='canceled',stage='reconciliation_required' WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `DELETE FROM db_migrations WHERE version=$1`, "p2p: release terminal confirmation reservations v109"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(fx.ctx); err != nil {
		t.Fatal(err)
	}
	var retained []byte
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2`, fx.owner, fx.confirmationID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if len(retained) == 0 {
		t.Fatal("v109 incorrectly released reconciliation reservation")
	}
}

func TestPostgresRetryProvisionPreProviderNegativeGatesAreAtomic(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(context.Context, *sql.DB, preProviderPostgresFixture) error
	}{
		{name: "readback", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			_, err := db.ExecContext(ctx, `UPDATE core_aws_ec2_provisions SET stack_id='stack' WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID)
			return err
		}},
		{name: "reconcile error", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			_, err := db.ExecContext(ctx, `UPDATE core_aws_ec2_provisions SET reconciliation_required=true,error_code='provider_error' WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID)
			return err
		}},
		{name: "consumed event", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			_, err := db.ExecContext(ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,1,$3,$4,'change_consumed',5,$5)`, fx.owner, fx.changeID, "99999999-9999-4999-8999-999999999999", fx.taskID, time.Now().UTC())
			return err
		}},
		{name: "change consume replay", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			reservation, _ := json.Marshal(agentaws.Reservation{ConfirmationID: fx.confirmationID, TaskID: fx.taskID, Attempt: 1, LeaseEpoch: 4, TaskRevision: 7, Active: true})
			_, err := db.ExecContext(ctx, `INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json) VALUES($1,'change-consume',$2,'digest',$3)`, fx.owner, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", reservation)
			return err
		}},
		{name: "reservation", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			_, err := db.ExecContext(ctx, `UPDATE agent_confirmations SET reservation_json='{"active":true}'::jsonb WHERE owner_id=$1 AND confirmation_id=$2`, fx.owner, fx.confirmationID)
			return err
		}},
		{name: "wrong provision", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			_, err := db.ExecContext(ctx, `UPDATE core_aws_changes SET provision_id=NULL WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID)
			return err
		}},
		{name: "wrong credential revision", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			if _, err := db.ExecContext(ctx, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) SELECT owner_id,credential_id,2,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=1`, fx.owner, fx.credentialID); err != nil {
				return err
			}
			_, err := db.ExecContext(ctx, `UPDATE core_aws_changes SET credential_revision=2 WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID)
			return err
		}},
		{name: "wrong provider token", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			_, err := db.ExecContext(ctx, `UPDATE core_aws_changes SET provider_token=$1 WHERE owner_id=$2 AND change_id=$3`, "99999999-9999-4999-8999-999999999999", fx.owner, fx.changeID)
			return err
		}},
		{name: "wrong provider digest", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			_, err := db.ExecContext(ctx, `UPDATE core_aws_changes SET provider_request_digest='wrong-digest' WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID)
			return err
		}},
		{name: "binding tamper", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			_, err := db.ExecContext(ctx, `UPDATE agent_confirmations SET binding_json=jsonb_set(binding_json,'{TargetID}','"tampered"') WHERE owner_id=$1 AND confirmation_id=$2`, fx.owner, fx.confirmationID)
			return err
		}},
		{name: "task spec kind payload linkage", mutate: func(ctx context.Context, db *sql.DB, fx preProviderPostgresFixture) error {
			_, err := db.ExecContext(ctx, `UPDATE agent_tasks SET spec_json='{"kind":"agent","idempotency_key":"77777777-7777-4777-8777-777777777777"}'::jsonb WHERE owner_id=$1 AND task_id=$2`, fx.owner, fx.taskID)
			return err
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, repo, fx := newPreProviderPostgresFixture(t)
			if err := tc.mutate(fx.ctx, store.DB(), fx); err != nil {
				t.Fatal(err)
			}
			before, err := repo.GetProvision(fx.ctx, fx.provisionID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = repo.RetryProvision(fx.ctx, fx.provisionID, 2, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaab"); !errors.Is(err, agentaws.ErrRevisionConflict) {
				t.Fatalf("gate error = %v, want ErrRevisionConflict", err)
			}
			after, err := repo.GetProvision(fx.ctx, fx.provisionID)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("partial provision mutation: before=%+v after=%+v err=%v", before, after, err)
			}
			var status string
			var revision int64
			if err = store.DB().QueryRowContext(fx.ctx, `SELECT status,revision FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID).Scan(&status, &revision); err != nil {
				t.Fatal(err)
			}
			if status != "waiting_user" || revision != 1 {
				t.Fatalf("partial change mutation: %q rev=%d", status, revision)
			}
		})
	}
}

func TestPostgresRetryProvisionAllowsOnlyChangeRequestedHistory(t *testing.T) {
	store, repo, fx := newPreProviderPostgresFixture(t)
	if _, err := store.DB().ExecContext(fx.ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,1,$3,$4,'change_requested',1,$5)`, fx.owner, fx.changeID, "99999999-9999-4999-8999-999999999999", fx.taskID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	got, err := repo.RetryProvision(fx.ctx, fx.provisionID, 2, "aaaaaaaa-aaaa-4aaa-8999-aaaaaaaaaaac")
	if err != nil || got.State != "planned" || got.Revision != 3 {
		t.Fatalf("change_requested history retry = %+v err=%v", got, err)
	}
}

func TestPostgresConsumeChangeRearmsLegacyDigestBeforeProvider(t *testing.T) {
	store, repo, fx := newPreProviderPostgresFixture(t)
	now := time.Now().UTC()
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET status='running',available_at=$1 WHERE owner_id=$2 AND task_id=$3`, now, fx.owner, fx.taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='confirmed',expires_at=$1 WHERE owner_id=$2 AND confirmation_id=$3`, now.Add(time.Hour), fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	reservation, err := repo.ConsumeChange(fx.ctx, agentaws.ConsumeChangeCommand{ChangeID: fx.changeID, ConfirmationID: fx.confirmationID, TaskID: fx.taskID, IdempotencyKey: "99999999-9999-4999-8999-999999999999", Attempt: 1, LeaseEpoch: 4, ExpectedChangeRevision: 1, ExpectedConfirmationRevision: 3, ExpectedTaskRevision: 7, Binding: fx.confirmation.Binding})
	if err != nil || !reservation.Active {
		t.Fatalf("legacy ConsumeChange = %+v err=%v", reservation, err)
	}
	var status, stage, digest, token string
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT status,stage,provider_request_digest,provider_token FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID).Scan(&status, &stage, &digest, &token); err != nil {
		t.Fatal(err)
	}
	if status != "running" || stage != "change_set_creating" || digest != agentaws.ProviderRequestDigest(fx.plan, token) {
		t.Fatalf("legacy consume transition = %q/%q/%q", status, stage, digest)
	}
	var state string
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT state FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2`, fx.owner, fx.confirmationID).Scan(&state); err != nil || state != "consumed" {
		t.Fatalf("confirmation state=%q err=%v", state, err)
	}
}

func TestPostgresClaimProviderMutationNormalizesLegacyConsumedDigest(t *testing.T) {
	store, repo, fx := newPreProviderPostgresFixture(t)
	now := time.Now().UTC()
	reservation, _ := json.Marshal(struct {
		TaskID       string `json:"task_id"`
		Attempt      uint32 `json:"attempt"`
		LeaseEpoch   uint64 `json:"lease_epoch"`
		TaskRevision int64  `json:"task_revision"`
		Active       bool   `json:"active"`
	}{TaskID: fx.taskID, Attempt: 1, LeaseEpoch: 4, TaskRevision: 7, Active: true})
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET status='running',lease_holder='worker',lease_expires_at=$1,available_at=$2 WHERE owner_id=$3 AND task_id=$4`, now.Add(time.Hour), now, fx.owner, fx.taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='consumed',revision=4,reservation_json=$1::jsonb,expires_at=$2 WHERE owner_id=$3 AND confirmation_id=$4`, reservation, now.Add(time.Hour), fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET status='running',stage='change_set_creating',revision=2 WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,1,$3,$4,'change_consumed',2,$5)`, fx.owner, fx.changeID, "99999999-9999-4999-8999-999999999999", fx.taskID, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `INSERT INTO core_aws_event_counters(owner_id,change_id,next_sequence) VALUES($1,$2,2)`, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	consumeReplay, _ := json.Marshal(agentaws.Reservation{ConfirmationID: fx.confirmationID, TaskID: fx.taskID, Attempt: 1, LeaseEpoch: 4, TaskRevision: 7, Active: true})
	if _, err := store.DB().ExecContext(fx.ctx, `INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json) VALUES($1,'change-consume',$2,'consume-digest',$3)`, fx.owner, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", consumeReplay); err != nil {
		t.Fatal(err)
	}
	cmd := agentaws.ProviderMutationCommand{ChangeID: fx.changeID, ConfirmationID: fx.confirmationID, TaskID: fx.taskID, Attempt: 1, LeaseEpoch: 4, ExpectedChangeRevision: 2, ExpectedTaskRevision: 7, ExpectedConfirmationRevision: 4, Kind: agentaws.ProviderMutationCreate, OperationKey: "aaaaaaaa-aaaa-4aaa-8999-aaaaaaaaaaad"}
	fence, err := repo.ClaimProviderMutation(fx.ctx, cmd)
	if err != nil {
		t.Fatalf("legacy ClaimProviderMutation = %+v err=%v", fence, err)
	}
	if fence.Change.Stage != agentaws.StageReconciling || fence.Change.ProviderRequestDigest != agentaws.ProviderRequestDigest(fx.plan, fx.confirmationID) {
		t.Fatalf("claimed fence digest/stage = %q/%q", fence.Change.ProviderRequestDigest, fence.Change.Stage)
	}
	var digest, eventKind string
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT provider_request_digest FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID).Scan(&digest); err != nil {
		t.Fatal(err)
	}
	if digest != agentaws.ProviderRequestDigest(fx.plan, fx.confirmationID) {
		t.Fatalf("stored canonical digest = %q", digest)
	}
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT kind FROM core_aws_events WHERE owner_id=$1 AND change_id=$2 ORDER BY sequence DESC LIMIT 1`, fx.owner, fx.changeID).Scan(&eventKind); err != nil || eventKind != "provider_mutation_dispatched" {
		t.Fatalf("dispatch event = %q err=%v", eventKind, err)
	}
}

func TestPostgresClaimProviderMutationAllowsExecuteAfterCommittedCreate(t *testing.T) {
	store, repo, fx := newPreProviderPostgresFixture(t)
	now := time.Now().UTC()
	reservation, err := json.Marshal(map[string]any{
		"confirmation_id": fx.confirmationID,
		"task_id":         fx.taskID,
		"attempt":         1,
		"lease_epoch":     4,
		"task_revision":   7,
		"active":          true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET status='running',lease_holder='worker',lease_expires_at=$1,available_at=$2 WHERE owner_id=$3 AND task_id=$4`, now.Add(time.Hour), now, fx.owner, fx.taskID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='consumed',revision=4,reservation_json=$1::jsonb,expires_at=$2 WHERE owner_id=$3 AND confirmation_id=$4`, reservation, now.Add(time.Hour), fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	providerDigest := agentaws.ProviderRequestDigest(fx.plan, fx.confirmationID)
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET status='running',stage='change_set_creating',revision=2,provider_request_digest=$1 WHERE owner_id=$2 AND change_id=$3`, providerDigest, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	create := agentaws.ProviderMutationCommand{
		ChangeID: fx.changeID, ConfirmationID: fx.confirmationID, TaskID: fx.taskID,
		Attempt: 1, LeaseEpoch: 4, ExpectedChangeRevision: 2, ExpectedTaskRevision: 7,
		ExpectedConfirmationRevision: 4, Kind: agentaws.ProviderMutationCreate,
		OperationKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaac",
	}
	claimedCreate, err := repo.ClaimProviderMutation(fx.ctx, create)
	if err != nil || claimedCreate.Change.Stage != agentaws.StageReconciling {
		t.Fatalf("create claim = %+v err=%v", claimedCreate, err)
	}
	create.ExpectedChangeRevision = claimedCreate.Change.Revision
	create.ExpectedTaskRevision = claimedCreate.Task.Revision
	create.ExpectedConfirmationRevision = claimedCreate.Confirmation.Revision
	committedCreate, err := repo.CommitProviderMutation(fx.ctx, agentaws.ProviderMutationResult{
		Command: create, Success: true, ProviderChangeSetID: "change-set-ready",
	})
	if err != nil || committedCreate.Stage != agentaws.StageChangeSetReady {
		t.Fatalf("create commit = %+v err=%v", committedCreate, err)
	}
	// Reclaim the same task attempt under a newer lease epoch. The consumed
	// reservation is promoted atomically by the execute claim before dispatch.
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET revision=8,lease_epoch=5,lease_expires_at=$1 WHERE owner_id=$2 AND task_id=$3`, now.Add(time.Hour), fx.owner, fx.taskID); err != nil {
		t.Fatal(err)
	}
	execute := agentaws.ProviderMutationCommand{
		ChangeID: fx.changeID, ConfirmationID: fx.confirmationID, TaskID: fx.taskID,
		Attempt: 1, LeaseEpoch: 5, ExpectedChangeRevision: committedCreate.Revision,
		ExpectedTaskRevision: 8, ExpectedConfirmationRevision: claimedCreate.Confirmation.Revision,
		Kind: agentaws.ProviderMutationExecute, ProviderChangeSetID: "change-set-ready",
		OperationKey: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaad",
	}
	claimedExecute, err := repo.ClaimProviderMutation(fx.ctx, execute)
	if err != nil || claimedExecute.Change.Stage != agentaws.StageReconciling {
		t.Fatalf("execute claim after create = %+v err=%v", claimedExecute, err)
	}
	execute.ExpectedChangeRevision = claimedExecute.Change.Revision
	execute.ExpectedTaskRevision = claimedExecute.Task.Revision
	execute.ExpectedConfirmationRevision = claimedExecute.Confirmation.Revision
	uncertain, err := repo.CommitProviderMutation(fx.ctx, agentaws.ProviderMutationResult{
		Command: execute, ResponseUncertain: true, ErrorCode: "provider_error", ErrorSummary: "AWS response lost",
	})
	if err != nil || uncertain.Stage != agentaws.StageReconciling {
		t.Fatalf("uncertain execute commit = %+v err=%v", uncertain, err)
	}
	// A later lease must reconcile the durable uncertain stage; it may not
	// dispatch ExecuteChangeSet again.
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET revision=9,lease_epoch=6,lease_expires_at=$1 WHERE owner_id=$2 AND task_id=$3`, now.Add(time.Hour), fx.owner, fx.taskID); err != nil {
		t.Fatal(err)
	}
	reclaimedExecute := execute
	reclaimedExecute.ExpectedChangeRevision = uncertain.Revision
	reclaimedExecute.ExpectedTaskRevision = 9
	reclaimedExecute.ExpectedConfirmationRevision = execute.ExpectedConfirmationRevision
	reclaimedExecute.LeaseEpoch = 6
	if _, err = repo.ClaimProviderMutation(fx.ctx, reclaimedExecute); !errors.Is(err, agentaws.ErrRevisionConflict) {
		t.Fatalf("uncertain lease reclaim execute claim err=%v", err)
	}
	var dispatched int
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_aws_events WHERE owner_id=$1 AND change_id=$2 AND kind='provider_mutation_dispatched'`, fx.owner, fx.changeID).Scan(&dispatched); err != nil {
		t.Fatal(err)
	}
	if dispatched != 2 {
		t.Fatalf("dispatch event count after create+execute = %d", dispatched)
	}
	if _, err = repo.ClaimProviderMutation(fx.ctx, execute); !errors.Is(err, agentaws.ErrRevisionConflict) {
		t.Fatalf("duplicate execute claim err=%v", err)
	}
	var afterDuplicate int
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_aws_events WHERE owner_id=$1 AND change_id=$2 AND kind='provider_mutation_dispatched'`, fx.owner, fx.changeID).Scan(&afterDuplicate); err != nil || afterDuplicate != dispatched {
		t.Fatalf("duplicate execute dispatch count=%d err=%v", afterDuplicate, err)
	}
}

func TestPostgresConsumeChangeRejectsMismatchedCommandBindingAtomically(t *testing.T) {
	store, repo, fx := newPreProviderPostgresFixture(t)
	now := time.Now().UTC()
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET status='running',available_at=$1 WHERE owner_id=$2 AND task_id=$3`, now, fx.owner, fx.taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='confirmed',expires_at=$1 WHERE owner_id=$2 AND confirmation_id=$3`, now.Add(time.Hour), fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET provider_request_digest=$1 WHERE owner_id=$2 AND change_id=$3`, agentaws.ProviderRequestDigest(fx.plan, fx.confirmationID), fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	binding := fx.confirmation.Binding
	binding.TargetID = "tampered-command-binding"
	_, err := repo.ConsumeChange(fx.ctx, agentaws.ConsumeChangeCommand{ChangeID: fx.changeID, ConfirmationID: fx.confirmationID, TaskID: fx.taskID, IdempotencyKey: "99999999-9999-4999-8999-999999999998", Attempt: 1, LeaseEpoch: 4, ExpectedChangeRevision: 1, ExpectedConfirmationRevision: 3, ExpectedTaskRevision: 7, Binding: binding})
	if !errors.Is(err, agentaws.ErrRevisionConflict) && !errors.Is(err, agentaws.ErrInvalid) {
		t.Fatalf("mismatched command binding error = %v", err)
	}
	var status, state string
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT status FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT state FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2`, fx.owner, fx.confirmationID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if status != "waiting_user" || state != "confirmed" {
		t.Fatalf("mismatched binding partially mutated rows: status=%q state=%q", status, state)
	}
}
