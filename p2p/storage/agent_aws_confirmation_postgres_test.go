package storage

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

func preparePreProviderConfirmation(t *testing.T, store *DatabaseStore, fx preProviderPostgresFixture, operation agentaws.Operation) agentaws.ProvisionReadback {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET status='waiting_user',failure_code='',failure_summary='',revision=1,progress_sequence=0 WHERE owner_id=$1 AND task_id=$2`, fx.owner, fx.taskID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='pending',revision=1,reservation_json=NULL,expires_at=$1,terminal_reason='' WHERE owner_id=$2 AND confirmation_id=$3`, now.Add(-time.Minute), fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	deletePlanID := ""
	if operation == agentaws.OperationDelete {
		deletePlanID = uuid.NewSHA1(uuid.Nil, []byte("ec2-destroy:"+fx.provisionID)).String()
		if _, err := store.DB().ExecContext(fx.ctx, `INSERT INTO core_aws_plans(owner_id,plan_id,credential_id,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at) SELECT owner_id,$1,credential_id,credential_revision,region,stack_name,'delete',template,template_sha256,parameters_json,tags_json,capabilities_json,1,$2 FROM core_aws_plans WHERE owner_id=$3 AND plan_id=$4`, deletePlanID, now, fx.owner, fx.planID); err != nil {
			t.Fatal(err)
		}
	} else if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_plans SET operation=$1 WHERE owner_id=$2 AND plan_id=$3`, operation, fx.owner, fx.planID); err != nil {
		t.Fatal(err)
	}
	state := "creating"
	if operation == agentaws.OperationDelete {
		state = "destroying"
	}
	planID := fx.planID
	providerDigest := fx.change.ProviderRequestDigest
	if operation == agentaws.OperationDelete {
		planID = deletePlanID
		deletePlan := fx.plan
		deletePlan.ID = deletePlanID
		deletePlan.Operation = agentaws.OperationDelete
		deletePlan.Revision = 1
		deletePlan.CreatedAt = now
		providerDigest = agentaws.ProviderRequestDigest(deletePlan, fx.confirmationID)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET plan_id=$1,operation=$2,status='waiting_user',stage='requested',change_set_id='',provider_request_digest=$3,revision=1,error_code='',error_summary='' WHERE owner_id=$4 AND change_id=$5`, planID, operation, providerDigest, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_ec2_provisions SET state=$1,revision=2,active_change_id=$2,stack_id='',instance_id='',public_ip='',security_group_id='',output_digest='',observed_at=NULL,reconciliation_required=false,error_code='',error_summary='' WHERE owner_id=$3 AND provision_id=$4`, state, fx.changeID, fx.owner, fx.provisionID); err != nil {
		t.Fatal(err)
	}
	if operation != agentaws.OperationDelete {
		return agentaws.ProvisionReadback{}
	}
	readback, err := agentaws.ProvisionReadbackFromStack(agentaws.StackOutputs{"StackId": "stack", "InstanceId": "i-1", "SecurityGroupId": "sg-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE core_aws_ec2_provisions SET stack_id=$1,instance_id=$2,security_group_id=$3,output_digest=$4,observed_at=$5 WHERE owner_id=$6 AND provision_id=$7`, readback.StackID, readback.InstanceID, readback.SecurityGroupID, readback.OutputDigest, readback.ObservedAt, fx.owner, fx.provisionID); err != nil {
		t.Fatal(err)
	}
	return readback
}

func TestPostgresAWSConfirmationExpiryReleasesCreateAndDelete(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation agentaws.Operation
		wantState string
	}{
		{name: "create", operation: agentaws.OperationCreate, wantState: "planned"},
		{name: "delete", operation: agentaws.OperationDelete, wantState: "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _, fx := newPreProviderPostgresFixture(t)
			preparePreProviderConfirmation(t, store, fx, tc.operation)
			deploymentID, err := legacyDeploymentIDForProvision(fx.owner, fx.provisionID)
			if err != nil {
				t.Fatal(err)
			}
			publicDeploymentID, err := publicDeploymentIDForProvision(fx.owner, fx.provisionID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.DB().ExecContext(fx.ctx, `INSERT INTO core_deployments(owner_id,deployment_id,public_deployment_id,provision_id,state,target_kind,revision,object_json,created_at,updated_at) VALUES($1,$2,$3,$4,$5,'AWS_EC2',2,'{}',NOW(),NOW())`, fx.owner, deploymentID, publicDeploymentID, fx.provisionID, fx.provision.State); err != nil {
				t.Fatal(err)
			}
			confirmations := NewDatabaseConfirmationStore(store.DB())
			if err := confirmations.ExpireAt(fx.ctx, fx.owner, fx.confirmationID, time.Now().UTC()); err != nil {
				t.Fatalf("ExpireAt = %v", err)
			}
			var state, active, changeStatus, changeStage string
			var provisionRevision, changeRevision int64
			if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,''),revision FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active, &provisionRevision); err != nil {
				t.Fatal(err)
			}
			if state != tc.wantState || active != "" || provisionRevision != 3 {
				t.Fatalf("provision = %q/%q revision=%d", state, active, provisionRevision)
			}
			if err := store.DB().QueryRowContext(fx.ctx, `SELECT status,stage,revision FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID).Scan(&changeStatus, &changeStage, &changeRevision); err != nil {
				t.Fatal(err)
			}
			if changeStatus != "canceled" || changeStage != "canceled" || changeRevision != 2 {
				t.Fatalf("change = %q/%q revision=%d", changeStatus, changeStage, changeRevision)
			}
			var changeEvent, provisionEvent string
			if err := store.DB().QueryRowContext(fx.ctx, `SELECT kind FROM core_aws_events WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID).Scan(&changeEvent); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRowContext(fx.ctx, `SELECT kind FROM core_aws_ec2_provision_events WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&provisionEvent); err != nil {
				t.Fatal(err)
			}
			if changeEvent != "change_pre_provider_terminalized" || provisionEvent != "provision_pre_provider_terminalized" {
				t.Fatalf("terminal events = %q/%q", changeEvent, provisionEvent)
			}
			var deploymentState string
			var deploymentEvents int
			if err := store.DB().QueryRowContext(fx.ctx, `SELECT state FROM core_deployments WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&deploymentState); err != nil {
				t.Fatal(err)
			}
			if err := store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM core_deployment_events WHERE owner_id=$1 AND deployment_id=$2`, fx.owner, deploymentID).Scan(&deploymentEvents); err != nil {
				t.Fatal(err)
			}
			if deploymentState != tc.wantState || deploymentEvents != 1 {
				t.Fatalf("deployment projection = %q events=%d", deploymentState, deploymentEvents)
			}
		})
	}
}

func TestPostgresAWSConfirmationExpiryRefusesDispatchedAndRollsBack(t *testing.T) {
	store, _, fx := newPreProviderPostgresFixture(t)
	preparePreProviderConfirmation(t, store, fx, agentaws.OperationCreate)
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET stage='reconciling',status='running' WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(fx.ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,1,'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa',$3,'provider_mutation_dispatched',2,NOW())`, fx.owner, fx.changeID, fx.taskID); err != nil {
		t.Fatal(err)
	}
	confirmations := NewDatabaseConfirmationStore(store.DB())
	if err := confirmations.ExpireAt(fx.ctx, fx.owner, fx.confirmationID, time.Now().UTC()); err == nil {
		t.Fatal("ExpireAt unexpectedly terminalized dispatched change")
	} else if !errors.Is(err, coreconfirmation.ErrConflict) {
		t.Fatalf("ExpireAt dispatched error = %v", err)
	}
	var state, active string
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
		t.Fatal(err)
	}
	if state != "creating" || active != fx.changeID {
		t.Fatalf("dispatched provision mutated: %q/%q", state, active)
	}
	var confirmationState string
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT state FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2`, fx.owner, fx.confirmationID).Scan(&confirmationState); err != nil {
		t.Fatal(err)
	}
	if confirmationState != "pending" {
		t.Fatalf("confirmation mutation was not rolled back: %q", confirmationState)
	}
}

func TestPostgresAWSConfirmationDeletePreservesReadback(t *testing.T) {
	store, _, fx := newPreProviderPostgresFixture(t)
	readback := preparePreProviderConfirmation(t, store, fx, agentaws.OperationDelete)
	confirmations := NewDatabaseConfirmationStore(store.DB())
	if err := confirmations.ExpireAt(fx.ctx, fx.owner, fx.confirmationID, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	var gotDigest string
	var observed time.Time
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT output_digest,observed_at FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&gotDigest, &observed); err != nil {
		t.Fatal(err)
	}
	if gotDigest != readback.OutputDigest || observed.IsZero() {
		t.Fatalf("delete readback lost: %q/%v", gotDigest, observed)
	}
}

func TestPostgresAWSConfirmationTerminalizesServiceDerivedDestroy(t *testing.T) {
	for _, tc := range []struct {
		name   string
		reject bool
	}{
		{name: "expiry"},
		{name: "reject", reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, repo, fx := newPreProviderPostgresFixture(t)
			now := time.Now().UTC().Truncate(time.Microsecond)
			readback, err := agentaws.ProvisionReadbackFromStack(agentaws.StackOutputs{"StackId": "stack", "InstanceId": "i-1", "SecurityGroupId": "sg-1"}, now)
			if err != nil {
				t.Fatal(err)
			}
			// Retire the fixture's synthetic create confirmation without changing
			// the original create plan. The service call below must create the
			// distinct derived delete plan through its production path.
			if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET status='canceled',revision=8 WHERE owner_id=$1 AND task_id=$2`, fx.owner, fx.taskID); err != nil {
				t.Fatal(err)
			}
			if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='expired',revision=4,reservation_json=NULL,expires_at=$1 WHERE owner_id=$2 AND confirmation_id=$3`, now.Add(-time.Minute), fx.owner, fx.confirmationID); err != nil {
				t.Fatal(err)
			}
			if _, err = store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET status='canceled',stage='canceled',revision=2 WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID); err != nil {
				t.Fatal(err)
			}
			if _, err = store.DB().ExecContext(fx.ctx, `UPDATE core_aws_ec2_provisions SET state='active',revision=1,active_change_id=NULL,stack_id=$1,instance_id=$2,security_group_id=$3,output_digest=$4,observed_at=$5,reconciliation_required=false,error_code='',error_summary='' WHERE owner_id=$6 AND provision_id=$7`, readback.StackID, readback.InstanceID, readback.SecurityGroupID, readback.OutputDigest, readback.ObservedAt, fx.owner, fx.provisionID); err != nil {
				t.Fatal(err)
			}
			service := agentaws.NewServiceWithCoordinator(repo, repo, nil, nil, nil, nil, nil)
			requested, err := service.RequestEC2Destroy(fx.ctx, fx.provisionID, 1, "99999999-9999-4999-8999-999999999991", fx.owner)
			if err != nil {
				t.Fatalf("RequestEC2Destroy = %v", err)
			}
			if requested.Change.PlanID == fx.planID || requested.Provision.PlanID != fx.planID || requested.Change.Operation != agentaws.OperationDelete {
				t.Fatalf("derived destroy linkage = change plan %q provision plan %q operation %q", requested.Change.PlanID, requested.Provision.PlanID, requested.Change.Operation)
			}
			var originalOperation string
			if err = store.DB().QueryRowContext(fx.ctx, `SELECT operation FROM core_aws_plans WHERE owner_id=$1 AND plan_id=$2`, fx.owner, fx.planID).Scan(&originalOperation); err != nil {
				t.Fatal(err)
			}
			if originalOperation != string(agentaws.OperationCreate) {
				t.Fatalf("RequestEC2Destroy mutated original plan operation to %q", originalOperation)
			}
			confirmations := NewDatabaseConfirmationStore(store.DB())
			if tc.reject {
				if _, err = confirmations.Reject(fx.ctx, coreconfirmation.RejectCommand{OwnerID: fx.owner, ConfirmationID: requested.Confirmation.ConfirmationID, IdempotencyKey: "99999999-9999-4999-8999-999999999992", ExpectedRevision: 1, Reason: "declined", At: now}); err != nil {
					t.Fatalf("Reject = %v", err)
				}
			} else if err = confirmations.ExpireAt(fx.ctx, fx.owner, requested.Confirmation.ConfirmationID, now.Add(48*time.Hour)); err != nil {
				t.Fatalf("ExpireAt = %v", err)
			}
			var state, active, status, stage string
			if err = store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
				t.Fatal(err)
			}
			if state != "active" || active != "" {
				t.Fatalf("terminalized destroy provision = %q/%q", state, active)
			}
			if err = store.DB().QueryRowContext(fx.ctx, `SELECT status,stage FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, requested.Change.ID).Scan(&status, &stage); err != nil {
				t.Fatal(err)
			}
			if status != "canceled" || stage != "canceled" {
				t.Fatalf("terminalized destroy change = %q/%q", status, stage)
			}
		})
	}
}

func newServiceDerivedDestroyPostgresFixture(t *testing.T) (*DatabaseStore, *PostgresAWSRepository, preProviderPostgresFixture, agentaws.ChangeRequestResult, time.Time) {
	t.Helper()
	store, repo, fx := newPreProviderPostgresFixture(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	readback, err := agentaws.ProvisionReadbackFromStack(agentaws.StackOutputs{"StackId": "stack", "InstanceId": "i-1", "SecurityGroupId": "sg-1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET status='canceled',revision=8 WHERE owner_id=$1 AND task_id=$2`, fx.owner, fx.taskID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET state='expired',revision=4,reservation_json=NULL,expires_at=$1 WHERE owner_id=$2 AND confirmation_id=$3`, now.Add(-time.Minute), fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET status='canceled',stage='canceled',revision=2 WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	if _, err = store.DB().ExecContext(fx.ctx, `UPDATE core_aws_ec2_provisions SET state='active',revision=1,active_change_id=NULL,stack_id=$1,instance_id=$2,security_group_id=$3,output_digest=$4,observed_at=$5,reconciliation_required=false,error_code='',error_summary='' WHERE owner_id=$6 AND provision_id=$7`, readback.StackID, readback.InstanceID, readback.SecurityGroupID, readback.OutputDigest, readback.ObservedAt, fx.owner, fx.provisionID); err != nil {
		t.Fatal(err)
	}
	service := agentaws.NewServiceWithCoordinator(repo, repo, nil, nil, nil, nil, nil)
	requested, err := service.RequestEC2Destroy(fx.ctx, fx.provisionID, 1, "99999999-9999-4999-8999-999999999990", fx.owner)
	if err != nil {
		t.Fatalf("RequestEC2Destroy = %v", err)
	}
	return store, repo, fx, requested, now
}

func TestPostgresAWSConfirmationDerivedDestroyCorruptionRollsBack(t *testing.T) {
	for _, tc := range []struct {
		name      string
		corrupt   string
		provision bool
		reject    bool
	}{
		{name: "reject original operation", corrupt: `UPDATE core_aws_plans SET operation='delete' WHERE owner_id=$1 AND plan_id=$2`, reject: true},
		{name: "expiry original operation", corrupt: `UPDATE core_aws_plans SET operation='delete' WHERE owner_id=$1 AND plan_id=$2`},
		{name: "reject region snapshot", corrupt: `UPDATE core_aws_ec2_provisions SET region='eu-west-1' WHERE owner_id=$1 AND provision_id=$2`, provision: true, reject: true},
		{name: "reject stack snapshot", corrupt: `UPDATE core_aws_ec2_provisions SET stack_name='other-stack' WHERE owner_id=$1 AND provision_id=$2`, provision: true, reject: true},
		{name: "reject profile snapshot", corrupt: `UPDATE core_aws_ec2_provisions SET profile='other-profile' WHERE owner_id=$1 AND provision_id=$2`, provision: true, reject: true},
		{name: "reject owner snapshot", corrupt: `UPDATE core_aws_ec2_provisions SET owner_digest=$1 WHERE owner_id=$2 AND provision_id=$3`, provision: true, reject: true},
		{name: "reject plan revision snapshot", corrupt: `UPDATE core_aws_ec2_provisions SET plan_revision=2 WHERE owner_id=$1 AND provision_id=$2`, provision: true, reject: true},
		{name: "reject template snapshot", corrupt: `UPDATE core_aws_ec2_provisions SET template_sha256=$1 WHERE owner_id=$2 AND provision_id=$3`, provision: true, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _, fx, requested, now := newServiceDerivedDestroyPostgresFixture(t)
			corruptID := fx.planID
			if tc.provision {
				corruptID = fx.provisionID
			}
			args := []any{fx.owner, corruptID}
			if tc.name == "reject owner snapshot" {
				args = []any{"sha256:" + strings.Repeat("b", 64), fx.owner, corruptID}
			} else if tc.name == "reject template snapshot" {
				args = []any{strings.Repeat("b", 64), fx.owner, corruptID}
			}
			if _, err := store.DB().ExecContext(fx.ctx, tc.corrupt, args...); err != nil {
				t.Fatal(err)
			}
			confirmations := NewDatabaseConfirmationStore(store.DB())
			var err error
			if tc.reject {
				_, err = confirmations.Reject(fx.ctx, coreconfirmation.RejectCommand{OwnerID: fx.owner, ConfirmationID: requested.Confirmation.ConfirmationID, IdempotencyKey: "99999999-9999-4999-8999-999999999989", ExpectedRevision: 1, Reason: "declined", At: now})
			} else {
				err = confirmations.ExpireAt(fx.ctx, fx.owner, requested.Confirmation.ConfirmationID, now.Add(48*time.Hour))
			}
			if !errors.Is(err, coreconfirmation.ErrConflict) {
				t.Fatalf("terminalization error = %v, want conflict", err)
			}
			var state, active, status, stage, confirmationState string
			if err = store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
				t.Fatal(err)
			}
			if state != "destroying" || active != requested.Change.ID {
				t.Fatalf("corrupt provision mutated = %q/%q", state, active)
			}
			if err = store.DB().QueryRowContext(fx.ctx, `SELECT status,stage FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, requested.Change.ID).Scan(&status, &stage); err != nil {
				t.Fatal(err)
			}
			if status != "waiting_user" || stage != "requested" {
				t.Fatalf("corrupt change mutated = %q/%q", status, stage)
			}
			if err = store.DB().QueryRowContext(fx.ctx, `SELECT state FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2`, fx.owner, requested.Confirmation.ConfirmationID).Scan(&confirmationState); err != nil {
				t.Fatal(err)
			}
			if confirmationState != "pending" {
				t.Fatalf("corrupt confirmation mutated = %q", confirmationState)
			}
		})
	}
}

func TestPostgresAWSConfirmationRejectVsLinkedRequestChangeIsBounded(t *testing.T) {
	store, repo, fx, requested, now := newServiceDerivedDestroyPostgresFixture(t)
	confirmations := NewDatabaseConfirmationStore(store.DB())
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err := confirmations.Reject(fx.ctx, coreconfirmation.RejectCommand{OwnerID: fx.owner, ConfirmationID: requested.Confirmation.ConfirmationID, IdempotencyKey: "99999999-9999-4999-8999-999999999988", ExpectedRevision: 1, Reason: "declined", At: now})
		results <- err
	}()
	go func() {
		defer wg.Done()
		_, err := repo.RequestChange(fx.ctx, agentaws.RequestChangeInput{PlanID: fx.planID, ProvisionID: fx.provisionID, ExpectedProvisionRevision: requested.Provision.Revision, IdempotencyKey: "99999999-9999-4999-8999-999999999987"})
		results <- err
	}()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("linked RequestChange and Reject deadlocked")
	}
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, agentaws.ErrRevisionConflict) && !errors.Is(err, coreconfirmation.ErrConflict) {
			t.Fatalf("concurrent linked request error = %v", err)
		}
	}
	var state, active string
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
		t.Fatal(err)
	}
	if state != "active" || active != "" {
		t.Fatalf("linked request final provision = %q/%q", state, active)
	}
}

func TestPostgresAWSConfirmationOriginalPlanLockPrecedesProvisionDeterministically(t *testing.T) {
	store, _, fx, requested, now := newServiceDerivedDestroyPostgresFixture(t)
	blocker, err := store.DB().BeginTx(fx.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Rollback()
	var lockedPlan string
	if err = blocker.QueryRowContext(fx.ctx, `SELECT plan_id::text FROM core_aws_plans WHERE owner_id=$1 AND plan_id=$2 FOR UPDATE`, fx.owner, fx.planID).Scan(&lockedPlan); err != nil {
		t.Fatal(err)
	}
	if lockedPlan != fx.planID {
		t.Fatalf("blocked wrong plan = %q", lockedPlan)
	}

	rejectResult := make(chan error, 1)
	go func() {
		_, rejectErr := NewDatabaseConfirmationStore(store.DB()).Reject(fx.ctx, coreconfirmation.RejectCommand{
			OwnerID: fx.owner, ConfirmationID: requested.Confirmation.ConfirmationID,
			IdempotencyKey: "99999999-9999-4999-8999-999999999986", ExpectedRevision: 1,
			Reason: "declined", At: now,
		})
		rejectResult <- rejectErr
	}()

	deadline := time.Now().Add(3 * time.Second)
	for {
		var waiting int
		err = store.DB().QueryRowContext(fx.ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type='Lock' AND query LIKE '%FROM core_aws_plans WHERE owner_id=$1 AND plan_id=$2 FOR UPDATE%'`).Scan(&waiting)
		if err != nil {
			t.Fatal(err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Reject did not block on the original plan lock")
		}
		time.Sleep(10 * time.Millisecond)
	}

	var provisionID string
	if err = store.DB().QueryRowContext(fx.ctx, `SELECT provision_id::text FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2 FOR UPDATE NOWAIT`, fx.owner, fx.provisionID).Scan(&provisionID); err != nil {
		t.Fatalf("provision lock was not available while original plan was blocked: %v", err)
	}
	if provisionID != fx.provisionID {
		t.Fatalf("locked wrong provision = %q", provisionID)
	}
	if err = blocker.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-rejectResult:
		if err != nil {
			t.Fatalf("Reject after original plan release = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reject did not complete after original plan release")
	}
}

func TestPostgresAWSConfirmationRejectReleasesCreate(t *testing.T) {
	store, _, fx := newPreProviderPostgresFixture(t)
	preparePreProviderConfirmation(t, store, fx, agentaws.OperationCreate)
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET expires_at=$1 WHERE owner_id=$2 AND confirmation_id=$3`, time.Now().UTC().Add(time.Hour), fx.owner, fx.confirmationID); err != nil {
		t.Fatal(err)
	}
	confirmations := NewDatabaseConfirmationStore(store.DB())
	if _, err := confirmations.Reject(fx.ctx, coreconfirmation.RejectCommand{OwnerID: fx.owner, ConfirmationID: fx.confirmationID, IdempotencyKey: "88888888-8888-4888-8888-888888888888", ExpectedRevision: 1, Reason: "declined", At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	var state, active, confirmationState string
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
		t.Fatal(err)
	}
	if state != "planned" || active != "" {
		t.Fatalf("rejected provision = %q/%q", state, active)
	}
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT state FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2`, fx.owner, fx.confirmationID).Scan(&confirmationState); err != nil {
		t.Fatal(err)
	}
	if confirmationState != "rejected" {
		t.Fatalf("confirmation state = %q", confirmationState)
	}
}

func TestPostgresAWSConfirmationReplayProgressUsesExactLinkage(t *testing.T) {
	t.Run("unrelated same-owner replay does not block", func(t *testing.T) {
		store, _, fx := newPreProviderPostgresFixture(t)
		preparePreProviderConfirmation(t, store, fx, agentaws.OperationCreate)
		if _, err := store.DB().ExecContext(fx.ctx, `INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json) VALUES($1,'provider-mutation',$2,$3,$4::jsonb)`, fx.owner, "99999999-9999-4999-8999-999999999999", "unrelated", `{"change_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","confirmation_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","status":"dispatched"}`); err != nil {
			t.Fatal(err)
		}
		if err := NewDatabaseConfirmationStore(store.DB()).ExpireAt(fx.ctx, fx.owner, fx.confirmationID, time.Now().UTC()); err != nil {
			t.Fatalf("unrelated replay blocked expiry: %v", err)
		}
	})
	t.Run("exact linked replay blocks and rolls back", func(t *testing.T) {
		store, _, fx := newPreProviderPostgresFixture(t)
		preparePreProviderConfirmation(t, store, fx, agentaws.OperationCreate)
		if _, err := store.DB().ExecContext(fx.ctx, `INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json) VALUES($1,'provider-mutation',$2,$3,$4::jsonb)`, fx.owner, "99999999-9999-4999-8999-999999999998", "linked", `{"change_id":"44444444-4444-4444-8444-444444444444","confirmation_id":"66666666-6666-4666-8666-666666666666","status":"dispatched"}`); err != nil {
			t.Fatal(err)
		}
		if err := NewDatabaseConfirmationStore(store.DB()).ExpireAt(fx.ctx, fx.owner, fx.confirmationID, time.Now().UTC()); err == nil {
			t.Fatal("linked replay did not block expiry")
		}
		var state, active string
		if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
			t.Fatal(err)
		}
		if state != "creating" || active != fx.changeID {
			t.Fatalf("linked replay failure was not atomic: %q/%q", state, active)
		}
	})
}

func TestPostgresAWSConfirmationQueuedPreProviderExpiryAndReject(t *testing.T) {
	t.Run("confirmed then queued expiry", func(t *testing.T) {
		store, _, fx := newPreProviderPostgresFixture(t)
		preparePreProviderConfirmation(t, store, fx, agentaws.OperationCreate)
		confirmations := NewDatabaseConfirmationStore(store.DB())
		if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET expires_at=$1 WHERE owner_id=$2 AND confirmation_id=$3`, time.Now().UTC().Add(time.Hour), fx.owner, fx.confirmationID); err != nil {
			t.Fatal(err)
		}
		if _, err := confirmations.Confirm(fx.ctx, coreconfirmation.ConfirmCommand{OwnerID: fx.owner, ConfirmationID: fx.confirmationID, IdempotencyKey: "99999999-9999-4999-8999-999999999995", ExpectedRevision: 1, Binding: fx.confirmation.Binding, At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET expires_at=$1 WHERE owner_id=$2 AND confirmation_id=$3`, time.Now().UTC().Add(-time.Minute), fx.owner, fx.confirmationID); err != nil {
			t.Fatal(err)
		}
		if err := confirmations.ExpireAt(fx.ctx, fx.owner, fx.confirmationID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		var state, active string
		if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
			t.Fatal(err)
		}
		if state != "planned" || active != "" {
			t.Fatalf("confirmed queued expiry provision = %q/%q", state, active)
		}
	})
	t.Run("queued expiry", func(t *testing.T) {
		store, _, fx := newPreProviderPostgresFixture(t)
		preparePreProviderConfirmation(t, store, fx, agentaws.OperationCreate)
		if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_tasks SET status='queued' WHERE owner_id=$1 AND task_id=$2`, fx.owner, fx.taskID); err != nil {
			t.Fatal(err)
		}
		if err := NewDatabaseConfirmationStore(store.DB()).ExpireAt(fx.ctx, fx.owner, fx.confirmationID, time.Now().UTC()); err != nil {
			t.Fatal(err)
		}
		var state, active string
		if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
			t.Fatal(err)
		}
		if state != "planned" || active != "" {
			t.Fatalf("queued expiry provision = %q/%q", state, active)
		}
	})
	t.Run("queued reject", func(t *testing.T) {
		store, _, fx := newPreProviderPostgresFixture(t)
		preparePreProviderConfirmation(t, store, fx, agentaws.OperationCreate)
		if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET expires_at=$1 WHERE owner_id=$2 AND confirmation_id=$3`, time.Now().UTC().Add(time.Hour), fx.owner, fx.confirmationID); err != nil {
			t.Fatal(err)
		}
		confirmations := NewDatabaseConfirmationStore(store.DB())
		if _, err := confirmations.Confirm(fx.ctx, coreconfirmation.ConfirmCommand{OwnerID: fx.owner, ConfirmationID: fx.confirmationID, IdempotencyKey: "99999999-9999-4999-8999-999999999994", ExpectedRevision: 1, Binding: fx.confirmation.Binding, At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		if _, err := confirmations.Reject(fx.ctx, coreconfirmation.RejectCommand{OwnerID: fx.owner, ConfirmationID: fx.confirmationID, IdempotencyKey: "99999999-9999-4999-8999-999999999997", ExpectedRevision: 2, Reason: "declined", At: time.Now().UTC()}); err != nil {
			t.Fatal(err)
		}
		var state, active string
		if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
			t.Fatal(err)
		}
		if state != "planned" || active != "" {
			t.Fatalf("queued reject provision = %q/%q", state, active)
		}
	})
}

func TestPostgresAWSStandaloneCloudFormationChangeTerminalizesWithoutProvision(t *testing.T) {
	for _, tc := range []struct {
		name      string
		operation agentaws.Operation
		reject    bool
	}{
		{name: "create expiry", operation: agentaws.OperationCreate},
		{name: "update reject", operation: agentaws.OperationUpdate, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, _, fx := newPreProviderPostgresFixture(t)
			preparePreProviderConfirmation(t, store, fx, tc.operation)
			if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET provision_id=NULL WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID); err != nil {
				t.Fatal(err)
			}
			if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_ec2_provisions SET state='planned',active_change_id=NULL WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID); err != nil {
				t.Fatal(err)
			}
			confirmations := NewDatabaseConfirmationStore(store.DB())
			if tc.reject {
				if _, err := store.DB().ExecContext(fx.ctx, `UPDATE agent_confirmations SET expires_at=$1 WHERE owner_id=$2 AND confirmation_id=$3`, time.Now().UTC().Add(time.Hour), fx.owner, fx.confirmationID); err != nil {
					t.Fatal(err)
				}
				if _, err := confirmations.Reject(fx.ctx, coreconfirmation.RejectCommand{OwnerID: fx.owner, ConfirmationID: fx.confirmationID, IdempotencyKey: "99999999-9999-4999-8999-999999999996", ExpectedRevision: 1, Reason: "declined", At: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
			} else if err := confirmations.ExpireAt(fx.ctx, fx.owner, fx.confirmationID, time.Now().UTC()); err != nil {
				t.Fatal(err)
			}
			var status, stage string
			if err := store.DB().QueryRowContext(fx.ctx, `SELECT status,stage FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID).Scan(&status, &stage); err != nil {
				t.Fatal(err)
			}
			if status != "canceled" || stage != "canceled" {
				t.Fatalf("standalone change = %q/%q", status, stage)
			}
			var provisionState, activeChange string
			if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&provisionState, &activeChange); err != nil {
				t.Fatal(err)
			}
			if provisionState != "planned" || activeChange != "" {
				t.Fatalf("standalone provision changed = %q/%q", provisionState, activeChange)
			}
		})
	}
}

func TestPostgresAWSPreProviderExpiryConcurrentBounded(t *testing.T) {
	store, repo, fx := newPreProviderPostgresFixture(t)
	preparePreProviderConfirmation(t, store, fx, agentaws.OperationCreate)
	confirmations := NewDatabaseConfirmationStore(store.DB())
	results := make(chan error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		results <- confirmations.ExpireAt(fx.ctx, fx.owner, fx.confirmationID, time.Now().UTC())
	}()
	go func() {
		defer wg.Done()
		_, err := repo.RetryProvision(fx.ctx, fx.provisionID, 2, "99999999-9999-4999-8999-999999999993")
		results <- err
	}()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent expiry did not complete within bound")
	}
	close(results)
	for err := range results {
		if err != nil && !errors.Is(err, coreconfirmation.ErrConflict) && !errors.Is(err, coreconfirmation.ErrRevisionConflict) && !errors.Is(err, agentaws.ErrRevisionConflict) {
			t.Fatalf("concurrent expiry error = %v", err)
		}
	}
	var state, active string
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
		t.Fatal(err)
	}
	if state != "planned" || active != "" {
		t.Fatalf("concurrent expiry provision = %q/%q", state, active)
	}
}

func TestPostgresAWSStandaloneCloudFormationCorruptProvisionLinkRollsBack(t *testing.T) {
	store, _, fx := newPreProviderPostgresFixture(t)
	preparePreProviderConfirmation(t, store, fx, agentaws.OperationCreate)
	if _, err := store.DB().ExecContext(fx.ctx, `UPDATE core_aws_changes SET provision_id=NULL WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID); err != nil {
		t.Fatal(err)
	}
	if err := NewDatabaseConfirmationStore(store.DB()).ExpireAt(fx.ctx, fx.owner, fx.confirmationID, time.Now().UTC()); err == nil {
		t.Fatal("corrupt unlinked provision link was accepted")
	}
	var status, stage, state, active string
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT status,stage FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, fx.owner, fx.changeID).Scan(&status, &stage); err != nil {
		t.Fatal(err)
	}
	if err := store.DB().QueryRowContext(fx.ctx, `SELECT state,COALESCE(active_change_id::text,'') FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2`, fx.owner, fx.provisionID).Scan(&state, &active); err != nil {
		t.Fatal(err)
	}
	if status != "waiting_user" || stage != "requested" || state != "creating" || active != fx.changeID {
		t.Fatalf("corrupt standalone mutation not rolled back: %q/%q/%q/%q", status, stage, state, active)
	}
}

func TestAWSConfirmationTerminalizationSQLMockRefusesRunningChangeAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	taskID := "55555555-5555-4555-8555-555555555555"
	changeID := "44444444-4444-4444-8444-444444444444"
	taskRaw, _ := json.Marshal(coretask.TaskSpec{Kind: coretask.TaskKindAWSChange, Payload: coretask.TaskPayload{AWSChange: &coretask.AWSChangeTaskPayload{ChangeID: changeID}}})
	owner := "@sqlmock:example.test"
	confirmationID := "66666666-6666-4666-8666-666666666666"
	mock.ExpectBegin()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(regexp.QuoteMeta("SELECT spec_json FROM agent_tasks")).WithArgs(owner, taskID).WillReturnRows(sqlmock.NewRows([]string{"spec_json"}).AddRow(taskRaw))
	mock.ExpectQuery(regexp.QuoteMeta("SELECT change_id::text,plan_id::text,credential_id::text,credential_revision,COALESCE(provision_id::text,''),operation,status,stage,change_set_id,provider_request_digest,provider_token,revision FROM core_aws_changes")).WithArgs(owner, taskID, confirmationID).WillReturnRows(sqlmock.NewRows([]string{"change_id", "plan_id", "credential_id", "credential_revision", "provision_id", "operation", "status", "stage", "change_set_id", "provider_request_digest", "provider_token", "revision"}).AddRow(changeID, "11111111-1111-4111-8111-111111111111", "22222222-2222-4222-8222-222222222222", 1, "33333333-3333-4333-8333-333333333333", "create", "running", "reconciling", "", "digest", confirmationID, 2))
	mock.ExpectRollback()
	err = terminalizeAWSPreProviderChangeTx(context.Background(), tx, coreconfirmation.Confirmation{ConfirmationID: confirmationID, ID: confirmationID, OwnerID: owner, TaskID: taskID, Binding: coreconfirmation.Binding{OperationDomain: "aws"}}, confirmationTaskRow{OwnerID: owner, Status: string(coretask.StatusWaitingUser)}, coreconfirmation.ReasonExpired, time.Now().UTC())
	if !errors.Is(err, coreconfirmation.ErrConflict) {
		t.Fatalf("terminalization error = %v", err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
