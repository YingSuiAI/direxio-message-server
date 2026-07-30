package storage

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
	"github.com/google/uuid"
)

// TestPostgresWorkloadRequestRepairsExpiredGeoLibreAndRebindsDeployment keeps
// the exact pre-dispatch recovery fence exercised by the SQL implementation:
// a new apply request may reclaim only a pending workload whose terminal
// operation never crossed the provider dispatch boundary. The repair and the
// deployment rebind must be committed with the new waiting-user operation.
func TestPostgresWorkloadRequestRepairsExpiredGeoLibreAndRebindsDeployment(t *testing.T) {
	ctx := context.Background()
	conn, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(closeDB)
	opts := config.DatabaseOptions{ConnectionString: config.DataSource(conn)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, opts), &opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })

	const (
		owner        = "@geolibre-repair:example.test"
		credentialID = "11111111-1111-4111-8111-111111111111"
		awsPlanID    = "22222222-2222-4222-8222-222222222222"
		provisionID  = "33333333-3333-4333-8333-333333333333"
		oldPlanID    = "44444444-4444-4444-8444-444444444444"
		newPlanID    = "55555555-5555-4555-8555-555555555555"
		oldWorkload  = "66666666-6666-4666-8666-666666666666"
		oldOperation = "77777777-7777-4777-8777-777777777777"
		oldTask      = "88888888-8888-4888-8888-888888888888"
		oldConfirm   = "99999999-9999-4999-8999-999999999999"
	)
	now := time.Now().UTC().Truncate(time.Microsecond)
	ownerDigest := agentaws.OwnerBindingDigest(owner)
	credentialBindingValue := credentialBinding(owner, credentialID, 1)
	credentialBindingDigest := stringDigestHex(credentialBindingValue.BindingDigest[:])
	input, err := agentaws.BuildGeoLibreSSMPlan(agentaws.GeoLibreInstallTarget{
		ProvisionID: provisionID, ProvisionPlanID: awsPlanID, ProvisionRevision: 2,
		CredentialID: credentialID, CredentialRevision: 1, AccountID: "123456789012",
		Region: "us-east-1", InstanceID: "i-0123456789abcdef0", PublicIP: "8.8.8.8",
		SecurityGroupID: "sg-0123456789abcdef0", OwnerBindingDigest: ownerDigest,
	}, uuid.NewString(), now.Add(3*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	makePlan := func(id string, expiresAt time.Time) workload.Plan {
		t.Helper()
		refs := append([]workload.SecretGrantRef(nil), input.SecretGrantRefs...)
		refs[0].BindingDigest = coreconfirmation.Digest(credentialBindingDigest)
		plan, normalizeErr := (workload.Plan{
			ID: id, Revision: 1, Summary: input.Summary, Artifact: input.Artifact, Source: input.Source,
			CommandSteps: input.CommandSteps, ImageDigest: input.ImageDigest, ImageURI: input.ImageURI,
			TargetKind: input.TargetKind, Target: input.Target, NetworkGrants: input.NetworkGrants,
			SecretGrantRefs: refs, ResourceLimits: input.ResourceLimits, ExpiresAt: expiresAt, CreatedAt: now,
		}).Normalize()
		if normalizeErr != nil {
			t.Fatal(normalizeErr)
		}
		return plan
	}
	oldPlan := makePlan(oldPlanID, now.Add(2*time.Hour))
	newPlan := makePlan(newPlanID, now.Add(3*time.Hour))
	if oldPlan.Digest == newPlan.Digest {
		t.Fatal("old and new GeoLibre plans unexpectedly share a digest")
	}

	db := store.DB()
	awsPlanDigest := strings.Repeat("b", 64)
	if _, err = db.ExecContext(ctx, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) VALUES($1,$2,1,1,1,'test',decode('000000000000000000000000','hex'),decode('00','hex'),$3,'geo','us-east-1','123456789012','arn:aws:iam::123456789012:user/geo',1,$4,$4)`, owner, credentialID, strings.Repeat("a", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_aws_plans(owner_id,plan_id,credential_id,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at) VALUES($1,$2,$3,1,'us-east-1','geo-stack','create',$4,$5,'{}','{}','[]',1,$6)`, owner, awsPlanID, credentialID, []byte(`{"Resources":{}}`), strings.Repeat("c", 64), now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_aws_ec2_provisions(owner_id,provision_id,plan_id,credential_id,credential_revision,region,stack_name,profile,owner_digest,plan_revision,template_sha256,plan_digest,state,revision,created_at,updated_at) VALUES($1,$2,$3,$4,1,'us-east-1','geo-stack',$5,$6,1,$7,$8,'active',2,$9,$9)`, owner, provisionID, awsPlanID, credentialID, agentaws.EC2ServiceProfile, ownerDigest, strings.Repeat("c", 64), awsPlanDigest, now); err != nil {
		t.Fatal(err)
	}

	insertPlan := func(p workload.Plan, createKey string) {
		t.Helper()
		raw, marshalErr := json.Marshal(p)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		targetRaw, _ := json.Marshal(p.Target.Identity)
		limitsRaw, _ := json.Marshal(p.ResourceLimits)
		refsRaw, _ := json.Marshal(p.SecretGrantRefs)
		if _, execErr := db.ExecContext(ctx, `INSERT INTO core_workload_plans(plan_id,owner_id,create_idempotency_key,create_request_hash,revision,digest,summary,plan_json,target_kind,target_identity_json,resource_limits_json,secret_grant_refs_json,expires_at,created_at) VALUES($1,$2,$3,$4,1,$5,$6,$7,$8,$9,$10,$11,$12,$13)`, p.ID, owner, createKey, strings.Repeat("d", 64), p.Digest, p.Summary, raw, p.TargetKind, targetRaw, limitsRaw, refsRaw, p.ExpiresAt, now); execErr != nil {
			t.Fatal(execErr)
		}
	}
	insertPlan(oldPlan, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	insertPlan(newPlan, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")

	oldSpec, _ := json.Marshal(coretask.TaskSpec{Kind: coretask.TaskKindWorkload, IdempotencyKey: "cccccccc-cccc-4ccc-8ccc-cccccccccccc"})
	if _, err = db.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,revision,available_at,failure_code,failure_summary,created_at,updated_at) VALUES($1,$2,$3,'failed',1,2,$4,'operation_expired','owner confirmation expired',$4,$4)`, oldTask, owner, oldSpec, now); err != nil {
		t.Fatal(err)
	}
	oldBinding := workload.BindingForOperation(oldPlan, oldWorkload, workload.OperationApply)
	oldBinding.OwnerID = owner
	oldBinding, err = oldBinding.Normalize()
	if err != nil {
		t.Fatal(err)
	}
	oldBindingRaw, _ := json.Marshal(oldBinding)
	if _, err = db.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,terminal_reason,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'expired',2,$9,'operation_expired',$10,$10)`, oldConfirm, owner, oldBinding.OperationDomain, oldWorkload, oldPlan.Revision, oldBinding.Digest, oldBindingRaw, oldTask, now.Add(-time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workloads(workload_id,owner_id,revision,plan_id,plan_digest,target_kind,state,actual_snapshot_json,updated_at) VALUES($1,$2,1,$3,$4,$5,'pending','{}',$6)`, oldWorkload, owner, oldPlan.ID, oldPlan.Digest, oldPlan.TargetKind, now); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `INSERT INTO core_workload_operations(operation_id,owner_id,workload_id,expected_workload_revision,plan_id,operation,plan_revision,plan_digest,target_kind,task_id,confirmation_id,status,revision,dispatch_state,dispatch_epoch,created_at,updated_at) VALUES($1,$2,$3,1,$4,'apply',1,$5,$6,$7,$8,'expired',1,'terminal',0,$9,$9)`, oldOperation, owner, oldWorkload, oldPlan.ID, oldPlan.Digest, oldPlan.TargetKind, oldTask, oldConfirm, now); err != nil {
		t.Fatal(err)
	}
	deploymentID, err := legacyDeploymentIDForProvision(owner, provisionID)
	if err != nil {
		t.Fatal(err)
	}
	objectRaw, _ := json.Marshal(map[string]any{"deployment_id": deploymentID, "provision_id": provisionID, "plan_id": awsPlanID, "plan_digest": awsPlanDigest, "target_kind": "AWS_EC2", "status": "expired", "revision": 2, "workload_id": oldWorkload})
	if _, err = db.ExecContext(ctx, `INSERT INTO core_deployments(owner_id,deployment_id,provision_id,workload_id,state,target_kind,revision,object_json,actual_json,created_at,updated_at) VALUES($1,$2,$3,$4,'expired','AWS_EC2',2,$5,'{}',$6,$6)`, owner, deploymentID, provisionID, oldWorkload, objectRaw, now); err != nil {
		t.Fatal(err)
	}

	workloadStore, err := NewAgentWorkloadStore(store, owner)
	if err != nil {
		t.Fatal(err)
	}
	// Hold the exact operation -> workload -> deployment lock order used by
	// confirmation terminalization while the retry begins. A deployment-first
	// retry deadlocks here; the repaired implementation waits on the operation
	// and then proceeds after this transaction commits.
	terminalTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer terminalTx.Rollback()
	if _, err = terminalTx.ExecContext(ctx, `SET LOCAL lock_timeout='2s'`); err != nil {
		t.Fatal(err)
	}
	if _, err = terminalTx.ExecContext(ctx, `SELECT operation_id FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2 FOR UPDATE`, owner, oldOperation); err != nil {
		t.Fatal(err)
	}
	type requestResult struct {
		value workload.RequestResult
		err   error
	}
	requestDone := make(chan requestResult, 1)
	go func() {
		value, requestErr := workloadStore.RequestOperation(ctx, workload.RequestCommand{
			PlanID: newPlan.ID, Kind: workload.OperationApply,
			IdempotencyKey: "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
			ExpiresAt:      newPlan.ExpiresAt,
		})
		requestDone <- requestResult{value: value, err: requestErr}
	}()
	time.Sleep(100 * time.Millisecond)
	if _, err = terminalTx.ExecContext(ctx, `SELECT workload_id FROM core_workloads WHERE owner_id=$1 AND workload_id=$2 FOR UPDATE`, owner, oldWorkload); err != nil {
		t.Fatalf("terminal workload lock = %v", err)
	}
	if _, err = terminalTx.ExecContext(ctx, `SELECT deployment_id FROM core_deployments WHERE owner_id=$1 AND deployment_id=$2 FOR UPDATE`, owner, deploymentID); err != nil {
		t.Fatalf("terminal deployment lock = %v", err)
	}
	if err = terminalTx.Commit(); err != nil {
		t.Fatal(err)
	}
	var requested requestResult
	select {
	case requested = <-requestDone:
	case <-time.After(5 * time.Second):
		t.Fatal("RequestOperation did not finish after terminal locks were released")
	}
	if requested.err != nil {
		t.Fatalf("RequestOperation = %v", requested.err)
	}
	result := requested.value
	if result.Operation.Status != workload.OperationWaitingUser || result.Task.Status != coretask.StatusWaitingUser || result.Confirmation.State != coreconfirmation.StatePending {
		t.Fatalf("new request state = op=%q task=%q confirmation=%q", result.Operation.Status, result.Task.Status, result.Confirmation.State)
	}
	if result.Operation.WorkloadID == oldWorkload || result.Operation.WorkloadID == "" {
		t.Fatalf("new request reused old workload: %q", result.Operation.WorkloadID)
	}
	var oldState string
	var oldRevision int64
	if err = db.QueryRowContext(ctx, `SELECT state,revision FROM core_workloads WHERE owner_id=$1 AND workload_id=$2`, owner, oldWorkload).Scan(&oldState, &oldRevision); err != nil {
		t.Fatal(err)
	}
	if oldState != "failed" || oldRevision != 2 {
		t.Fatalf("old workload repair = %q/%d, want failed/2", oldState, oldRevision)
	}
	var linkedWorkload, deploymentState string
	if err = db.QueryRowContext(ctx, `SELECT workload_id::text,state FROM core_deployments WHERE owner_id=$1 AND deployment_id=$2`, owner, deploymentID).Scan(&linkedWorkload, &deploymentState); err != nil {
		t.Fatal(err)
	}
	if linkedWorkload != result.Operation.WorkloadID || deploymentState != "pending" {
		t.Fatalf("deployment rebind = workload=%q state=%q, want workload=%q state=pending", linkedWorkload, deploymentState, result.Operation.WorkloadID)
	}
	var oldOperationStatus, oldDispatchState string
	if err = db.QueryRowContext(ctx, `SELECT status,dispatch_state FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2`, owner, oldOperation).Scan(&oldOperationStatus, &oldDispatchState); err != nil {
		t.Fatal(err)
	}
	if oldOperationStatus != "expired" || oldDispatchState != "terminal" {
		t.Fatalf("old operation changed = %q/%q", oldOperationStatus, oldDispatchState)
	}
}
