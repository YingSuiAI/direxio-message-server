package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

// terminalizeAWSPreProviderChangeTx closes the exact AWS change attached to a
// confirmation before the provider has been admitted.  The caller already
// owns the task and confirmation locks; every check and projection update is
// kept in that transaction so a forged/stale binding cannot release a
// provision or leave deployment projections half-updated.
func terminalizeAWSPreProviderChangeTx(ctx context.Context, tx *sql.Tx, stored coreconfirmation.Confirmation, taskRow confirmationTaskRow, reason string, at time.Time) error {
	if stored.Binding.OperationDomain != "aws" {
		return nil
	}
	// A running task may already be in the provider hand-off path.  Do not take
	// the change lock after the task lock in that case; the provider path uses
	// the inverse order while it completes its own fence.
	if taskRow.Status != string(coretask.StatusWaitingUser) && taskRow.Status != string(coretask.StatusQueued) {
		return coreconfirmation.ErrConflict
	}
	owner, taskID, confirmationID := stored.OwnerID, stored.TaskID, stored.ID
	var taskSpecRaw []byte
	if err := tx.QueryRowContext(ctx, `SELECT spec_json FROM agent_tasks WHERE owner_id=$1 AND task_id=$2`, owner, taskID).Scan(&taskSpecRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return coreconfirmation.ErrNotFound
		}
		return err
	}
	var spec coretask.TaskSpec
	if json.Unmarshal(taskSpecRaw, &spec) != nil || spec.Kind != coretask.TaskKindAWSChange || spec.Payload.AWSChange == nil || !validAWSUUID(spec.Payload.AWSChange.ChangeID) {
		return coreconfirmation.ErrConflict
	}

	var changeID, planID, credentialID, provisionID string
	var credentialRevision, changeRevision int64
	var operation, status, stage, changeSetID, providerDigest, providerToken string
	if err := tx.QueryRowContext(ctx, `SELECT change_id::text,plan_id::text,credential_id::text,credential_revision,COALESCE(provision_id::text,''),operation,status,stage,change_set_id,provider_request_digest,provider_token,revision FROM core_aws_changes WHERE owner_id=$1 AND task_id=$2 AND confirmation_id=$3 FOR UPDATE`, owner, taskID, confirmationID).Scan(
		&changeID, &planID, &credentialID, &credentialRevision, &provisionID, &operation, &status, &stage, &changeSetID, &providerDigest, &providerToken, &changeRevision,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return coreconfirmation.ErrNotFound
		}
		return err
	}
	if spec.Payload.AWSChange.ChangeID != changeID || !validAWSUUID(planID) || !validAWSUUID(credentialID) ||
		(provisionID != "" && !validAWSUUID(provisionID)) || (operation != string(agentaws.OperationCreate) && operation != string(agentaws.OperationUpdate) && operation != string(agentaws.OperationDelete)) || status != string(agentaws.ChangeWaitingUser) || stage != string(agentaws.StageRequested) || changeSetID != "" || providerToken != confirmationID {
		return coreconfirmation.ErrConflict
	}

	var plan agentaws.Plan
	var params, tags, caps []byte
	if err := tx.QueryRowContext(ctx, `SELECT plan_id::text,credential_id::text,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at FROM core_aws_plans WHERE owner_id=$1 AND plan_id=$2 FOR UPDATE`, owner, planID).Scan(
		&plan.ID, &plan.CredentialID, &plan.CredentialRevision, &plan.Region, &plan.StackName, &plan.Operation, &plan.Template, &plan.TemplateSHA256, &params, &tags, &caps, &plan.Revision, &plan.CreatedAt,
	); err != nil {
		return coreconfirmation.ErrConflict
	}
	if json.Unmarshal(params, &plan.Parameters) != nil || json.Unmarshal(tags, &plan.Tags) != nil || json.Unmarshal(caps, &plan.Capabilities) != nil || plan.Validate() != nil || plan.ID != planID || plan.CredentialID != credentialID || plan.CredentialRevision != credentialRevision || string(plan.Operation) != operation || !preProviderProviderDigestMatches(plan, providerToken, providerDigest) {
		return coreconfirmation.ErrConflict
	}

	// Lock the confirmation before the credential.  This matches the provider
	// consume path (task -> change -> plan -> confirmation -> credential) and
	// avoids a cross-path lock inversion.
	var reservation []byte
	if err := tx.QueryRowContext(ctx, `SELECT reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2 FOR UPDATE`, owner, confirmationID).Scan(&reservation); err != nil {
		return err
	}
	if len(reservation) != 0 {
		return coreconfirmation.ErrConflict
	}
	var credential struct {
		Name, Region, AccountID, UserARN string
		VerifiedRevision, Revision       int64
		CreatedAt, UpdatedAt             time.Time
	}
	if err := tx.QueryRowContext(ctx, `SELECT name,region,account_id,user_arn,verified_revision,revision,created_at,updated_at FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$3 FOR SHARE`, owner, credentialID, credentialRevision).Scan(
		&credential.Name, &credential.Region, &credential.AccountID, &credential.UserARN, &credential.VerifiedRevision, &credential.Revision, &credential.CreatedAt, &credential.UpdatedAt,
	); err != nil {
		return coreconfirmation.ErrConflict
	}
	credentialValue := agentaws.RehydrateCredentialMetadata(credentialID, credential.Name, credential.Region, credential.AccountID, credential.UserARN, credential.VerifiedRevision, credential.Revision, credential.CreatedAt, credential.UpdatedAt)
	if !agentaws.CredentialReadyForPlan(credentialValue) {
		return coreconfirmation.ErrConflict
	}
	expectedBinding := agentaws.BindingForPlan(plan, credentialValue)
	expectedBinding.OwnerID = owner
	if !stored.Binding.Equal(expectedBinding) {
		return coreconfirmation.ErrConflict
	}

	var provisionPlanID, provisionCredentialID, provisionState, activeChangeID string
	var provisionCredentialRevision, provisionRevision int64
	var stackID, instanceID, publicIP, securityGroupID, outputDigest, errorCode, errorSummary string
	var observed *time.Time
	var reconciliationRequired bool
	if provisionID != "" {
		if err := tx.QueryRowContext(ctx, `SELECT plan_id::text,credential_id::text,credential_revision,state,revision,COALESCE(active_change_id::text,''),stack_id,instance_id,public_ip,security_group_id,output_digest,observed_at,reconciliation_required,error_code,error_summary FROM core_aws_ec2_provisions WHERE owner_id=$1 AND provision_id=$2 FOR UPDATE`, owner, provisionID).Scan(
			&provisionPlanID, &provisionCredentialID, &provisionCredentialRevision, &provisionState, &provisionRevision, &activeChangeID, &stackID, &instanceID, &publicIP, &securityGroupID, &outputDigest, &observed, &reconciliationRequired, &errorCode, &errorSummary,
		); err != nil {
			return coreconfirmation.ErrConflict
		}
	} else {
		var linkedProvision bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM core_aws_ec2_provisions WHERE owner_id=$1 AND active_change_id=$2)`, owner, changeID).Scan(&linkedProvision); err != nil {
			return err
		}
		if linkedProvision {
			return coreconfirmation.ErrConflict
		}
	}
	var readback agentaws.ProvisionReadback
	if provisionID != "" {
		if provisionPlanID != planID || provisionCredentialID != credentialID || provisionCredentialRevision != credentialRevision || activeChangeID != changeID || reconciliationRequired || errorCode != "" || errorSummary != "" {
			return coreconfirmation.ErrConflict
		}
		readback = agentaws.ProvisionReadback{StackID: stackID, InstanceID: instanceID, PublicIP: publicIP, SecurityGroupID: securityGroupID, OutputDigest: outputDigest}
		if observed != nil {
			readback.ObservedAt = *observed
		}
		if operation == string(agentaws.OperationCreate) {
			if provisionState != "creating" || readback != (agentaws.ProvisionReadback{}) {
				return coreconfirmation.ErrConflict
			}
		} else if operation == string(agentaws.OperationDelete) {
			if provisionState != "destroying" || readback.Validate() != nil {
				return coreconfirmation.ErrConflict
			}
		} else {
			return coreconfirmation.ErrConflict
		}
	}
	var progressed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM core_aws_events WHERE owner_id=$1 AND change_id=$2 AND kind <> 'change_requested') OR EXISTS(SELECT 1 FROM core_aws_replays WHERE owner_id=$1 AND operation='change-consume' AND (response_json->>'confirmation_id'=$3::text OR response_json->>'ConfirmationID'=$3::text)) OR EXISTS(SELECT 1 FROM core_aws_replays WHERE owner_id=$1 AND operation='provider-mutation' AND (response_json->>'change_id'=$2::text OR response_json->>'ChangeID'=$2::text) AND (response_json->>'confirmation_id'=$3::text OR response_json->>'ConfirmationID'=$3::text))`, owner, changeID, confirmationID).Scan(&progressed); err != nil {
		return err
	}
	if progressed {
		return coreconfirmation.ErrConflict
	}

	now := at.UTC()
	code := "pre_provider_confirmation_terminalized"
	if result, err := tx.ExecContext(ctx, `UPDATE core_aws_changes SET status='canceled',stage='canceled',error_code=$1,error_summary=$2,revision=revision+1,updated_at=$3 WHERE owner_id=$4 AND change_id=$5 AND task_id=$6 AND confirmation_id=$7 AND status='waiting_user' AND stage='requested' AND revision=$8`, code, reason, now, owner, changeID, taskID, confirmationID, changeRevision); err != nil {
		return err
	} else if affected, _ := result.RowsAffected(); affected != 1 {
		return coreconfirmation.ErrRevisionConflict
	}
	if provisionID != "" && operation == string(agentaws.OperationCreate) {
		if result, err := tx.ExecContext(ctx, `UPDATE core_aws_ec2_provisions SET state='planned',active_change_id=NULL,stack_id='',instance_id='',public_ip='',security_group_id='',output_digest='',observed_at=NULL,reconciliation_required=false,error_code='',error_summary='',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND provision_id=$3 AND revision=$4 AND state='creating' AND active_change_id=$5`, now, owner, provisionID, provisionRevision, changeID); err != nil {
			return err
		} else if affected, _ := result.RowsAffected(); affected != 1 {
			return coreconfirmation.ErrRevisionConflict
		}
	} else if provisionID != "" {
		if result, err := tx.ExecContext(ctx, `UPDATE core_aws_ec2_provisions SET state='active',active_change_id=NULL,reconciliation_required=false,error_code='',error_summary='',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND provision_id=$3 AND revision=$4 AND state='destroying' AND active_change_id=$5`, now, owner, provisionID, provisionRevision, changeID); err != nil {
			return err
		} else if affected, _ := result.RowsAffected(); affected != 1 {
			return coreconfirmation.ErrRevisionConflict
		}
	}
	var changeSequence int64
	if err := tx.QueryRowContext(ctx, `INSERT INTO core_aws_event_counters(owner_id,change_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,change_id) DO UPDATE SET next_sequence=core_aws_event_counters.next_sequence+1 RETURNING next_sequence-1`, owner, changeID).Scan(&changeSequence); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,$3,$4,$5,'change_pre_provider_terminalized',$6,$7)`, owner, changeID, changeSequence, uuid.NewString(), taskID, changeRevision+1, now); err != nil {
		return err
	}
	if provisionID != "" {
		var provisionSequence int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO core_aws_ec2_provision_event_counters(owner_id,provision_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,provision_id) DO UPDATE SET next_sequence=core_aws_ec2_provision_event_counters.next_sequence+1 RETURNING next_sequence-1`, owner, provisionID).Scan(&provisionSequence); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO core_aws_ec2_provision_events(owner_id,provision_id,change_id,sequence,event_id,kind,revision,at) VALUES($1,$2,$3,$4,$5,'provision_pre_provider_terminalized',$6,$7)`, owner, provisionID, changeID, provisionSequence, uuid.NewString(), provisionRevision+1, now); err != nil {
			return err
		}
	}
	return nil
}

func lockAWSPreProviderProvisionTx(ctx context.Context, tx *sql.Tx, owner, provisionID string) error {
	if strings.TrimSpace(owner) == "" || !validAWSUUID(provisionID) {
		return coreconfirmation.ErrInvalid
	}
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, canonicalAdvisoryLockIdentity("aws", owner, "pre-provider-provision", provisionID))
	return err
}

func lockAWSPreProviderByConfirmationTx(ctx context.Context, tx *sql.Tx, owner, confirmationID string) error {
	var resolvedOwner, provisionID string
	err := tx.QueryRowContext(ctx, `SELECT owner_id,COALESCE(provision_id::text,'') FROM core_aws_changes WHERE ($1='' OR owner_id=$1) AND confirmation_id=$2`, owner, confirmationID).Scan(&resolvedOwner, &provisionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if provisionID == "" {
		return nil
	}
	return lockAWSPreProviderProvisionTx(ctx, tx, resolvedOwner, provisionID)
}
