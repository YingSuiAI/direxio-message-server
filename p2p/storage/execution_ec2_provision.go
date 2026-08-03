package storage

// Durable PostgreSQL intent/readback storage for execution.v2 EC2
// provisioning. The generic dispatch intent must exist first. This adapter
// then persists a deterministic CloudFormation stack key and marks the generic
// receipt/target lease as provider-known in the same transaction, before any
// CreateStack call can occur.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/google/uuid"
)

type executionStoreDiagnosticError struct {
	cause error
	code  string
}

func (e executionStoreDiagnosticError) Error() string    { return "execution store: " + e.code }
func (e executionStoreDiagnosticError) Unwrap() error    { return e.cause }
func (e executionStoreDiagnosticError) SafeCode() string { return e.code }

func ec2ReadbackStoreError(cause error, code string) error {
	if cause == nil {
		cause = ErrExecutionStoreInvalid
	}
	return executionStoreDiagnosticError{cause: cause, code: code}
}

func (s *DatabaseExecutionStore) ReserveEC2ProvisionIntent(ctx context.Context, intent coreaws.EC2ProvisionIntent) (coreaws.EC2ProvisionIntentRecord, bool, error) {
	var zero coreaws.EC2ProvisionIntentRecord
	if s == nil || s.db == nil || coreaws.ValidateEC2ProvisionIntentSnapshot(intent) != nil {
		return zero, false, ErrExecutionStoreInvalid
	}
	quoteDigest, err := coreexecution.CanonicalDigest(intent.Request.Target.ComputeReservation.CostQuote)
	if err != nil || quoteDigest != intent.Request.CostQuoteDigest {
		return zero, false, coreexecution.ErrConflict
	}
	raw, err := json.Marshal(intent.Request)
	if err != nil || validateCatalogSensitiveData(intent.Request) != nil {
		return zero, false, ErrExecutionStoreInvalid
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, false, err
	}
	defer tx.Rollback()
	var runID, stageID, attemptID, receiptID, targetID, targetDigest, planID, planDigest, stageDigest string
	var targetRevision, planRevision, stageRevision uint64
	var receiptStatus, confirmationState, bindingPolicy, bindingCost, bindingPreview string
	err = tx.QueryRowContext(ctx, `SELECT i.run_id::text,i.stage_id::text,i.attempt_id::text,i.receipt_id::text,i.target_id::text,i.target_revision,i.target_digest,i.plan_id::text,i.plan_revision,i.plan_digest,i.stage_revision,i.stage_digest,r.status,c.state,c.binding_json->>'policy_digest',c.binding_json->>'cost_quote_digest',c.binding_json->>'preview_digest' FROM core_execution_dispatch_intents i JOIN core_execution_receipts r ON r.owner_id=i.owner_id AND r.run_id=i.run_id AND r.receipt_id=i.receipt_id AND r.attempt_id=i.attempt_id JOIN core_execution_run_stages s ON s.owner_id=i.owner_id AND s.run_id=i.run_id AND s.stage_id=i.stage_id JOIN agent_confirmations c ON c.owner_id=s.owner_id AND c.confirmation_id=s.confirmation_id WHERE i.owner_id=$1 AND i.fence_digest=$2 FOR UPDATE OF i,r,s,c`, intent.OwnerID, intent.FenceDigest).Scan(
		&runID, &stageID, &attemptID, &receiptID, &targetID, &targetRevision, &targetDigest, &planID, &planRevision, &planDigest, &stageRevision, &stageDigest,
		&receiptStatus, &confirmationState, &bindingPolicy, &bindingCost, &bindingPreview,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return zero, false, coreexecution.ErrConflict
	}
	if err != nil {
		return zero, false, err
	}
	req := intent.Request
	if runID != req.RunID || stageID != req.StageID || attemptID != req.AttemptID || targetID != req.Target.ID || targetRevision != req.Target.Revision || targetDigest != string(req.Target.Digest) ||
		planID != req.PlanID || planRevision != req.PlanRevision || planDigest != string(req.PlanDigest) || stageRevision != req.StageRevision || stageDigest != string(req.StageDigest) ||
		(receiptStatus != string(coreexecution.ReceiptAccepted) && receiptStatus != "running") || confirmationState != "consumed" ||
		bindingPolicy != string(req.PolicyDigest) || bindingCost != string(req.CostQuoteDigest) || !coreexecution.ValidateDigest(bindingPreview) {
		return zero, false, coreexecution.ErrConflict
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO core_execution_ec2_provision_intents(owner_id,fence_digest,request_digest,provider_operation_key,provider_operation_id,run_id,stage_id,attempt_id,receipt_id,target_id,target_revision,target_digest,plan_id,plan_revision,plan_digest,policy_digest,cost_quote_digest,status,revision,schema_version,request_json,created_at,updated_at) VALUES($1,$2,$3,$4,'',$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'accepted',1,'execution-ec2-provision-intent/v2',$17,$18,$18) ON CONFLICT (owner_id,fence_digest) DO NOTHING`, intent.OwnerID, intent.FenceDigest, intent.RequestDigest, intent.ProviderOperationKey, runID, stageID, attemptID, receiptID, targetID, targetRevision, targetDigest, planID, planRevision, planDigest, req.PolicyDigest, req.CostQuoteDigest, raw, now)
	if err != nil {
		return zero, false, mapExecutionConflict(err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return zero, false, err
	}
	if inserted == 1 {
		// The deterministic stack key is durable provider evidence even before
		// AWS returns a stack ARN. A process crash from here onward is therefore
		// readback-only and can never create a second stack.
		if receiptStatus == string(coreexecution.ReceiptAccepted) {
			res, updateErr := tx.ExecContext(ctx, `UPDATE core_execution_receipts SET provider_operation_id=$1,status='running',revision=revision+1 WHERE owner_id=$2 AND receipt_id=$3 AND attempt_id=$4 AND status='accepted' AND provider_operation_id=''`, intent.ProviderOperationKey, intent.OwnerID, receiptID, attemptID)
			if updateErr != nil {
				return zero, false, updateErr
			}
			if n, updateErr := res.RowsAffected(); updateErr != nil || n != 1 {
				return zero, false, coreexecution.ErrConflict
			}
		}
		if _, err = tx.ExecContext(ctx, `UPDATE core_execution_dispatch_intents SET status='accepted',revision=revision+1,updated_at=$3 WHERE owner_id=$1 AND fence_digest=$2 AND status='intent'`, intent.OwnerID, intent.FenceDigest, now); err != nil {
			return zero, false, err
		}
		res, err := tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET provider_operation_id=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND run_id=$4 AND stage_id=$5 AND target_id=$6 AND target_revision=$7 AND status='active' AND provider_operation_id=''`, intent.ProviderOperationKey, now, intent.OwnerID, runID, stageID, targetID, targetRevision)
		if err != nil {
			return zero, false, err
		}
		if n, updateErr := res.RowsAffected(); updateErr != nil || n != 1 {
			return zero, false, coreexecution.ErrConflict
		}
	}
	record, err := loadEC2ProvisionIntentTx(ctx, tx, intent.OwnerID, intent.FenceDigest, true)
	if err != nil || record.Intent.RequestDigest != intent.RequestDigest || record.Intent.ProviderOperationKey != intent.ProviderOperationKey || record.Intent.Request.RequestDigest != req.RequestDigest {
		if err != nil {
			return zero, false, err
		}
		return zero, false, coreexecution.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return zero, false, err
	}
	return record, inserted == 1, nil
}

func (s *DatabaseExecutionStore) GetEC2ProvisionIntent(ctx context.Context, owner string, fence coreexecution.Digest) (coreaws.EC2ProvisionIntentRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !fence.Valid() {
		return coreaws.EC2ProvisionIntentRecord{}, ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return coreaws.EC2ProvisionIntentRecord{}, err
	}
	defer tx.Rollback()
	record, err := loadEC2ProvisionIntentTx(ctx, tx, owner, fence, false)
	if err != nil {
		return coreaws.EC2ProvisionIntentRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return coreaws.EC2ProvisionIntentRecord{}, err
	}
	return record, nil
}

func (s *DatabaseExecutionStore) RecordEC2ProviderOperation(ctx context.Context, owner string, fence coreexecution.Digest, operationID string) (coreaws.EC2ProvisionIntentRecord, error) {
	var zero coreaws.EC2ProvisionIntentRecord
	operationID = strings.TrimSpace(operationID)
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !fence.Valid() || operationID == "" {
		return zero, ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	record, err := loadEC2ProvisionIntentTx(ctx, tx, owner, fence, true)
	if err != nil {
		return zero, err
	}
	if record.ProviderOperationID != "" {
		if record.ProviderOperationID != operationID {
			return zero, coreexecution.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return zero, err
		}
		return record, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE core_execution_ec2_provision_intents SET provider_operation_id=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND fence_digest=$4 AND provider_operation_id='' AND status IN ('accepted','pending','uncertain')`, operationID, s.now().UTC().Truncate(time.Microsecond), record.Intent.OwnerID, fence)
	if err != nil {
		return zero, mapExecutionConflict(err)
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		return zero, coreexecution.ErrConflict
	}
	record, err = loadEC2ProvisionIntentTx(ctx, tx, record.Intent.OwnerID, fence, true)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return record, nil
}

func (s *DatabaseExecutionStore) RecordEC2ProvisionReadback(ctx context.Context, owner string, fence coreexecution.Digest, readback coreaws.CloudFormationProvisionReadback) (coreaws.EC2ProvisionIntentRecord, error) {
	return s.updateEC2ProvisionReadback(ctx, owner, fence, readback, "")
}

func (s *DatabaseExecutionStore) MarkEC2ProvisionFailed(ctx context.Context, owner string, fence coreexecution.Digest, readback coreaws.CloudFormationProvisionReadback) error {
	_, err := s.updateEC2ProvisionReadback(ctx, owner, fence, readback, "failed")
	return err
}

func (s *DatabaseExecutionStore) updateEC2ProvisionReadback(ctx context.Context, owner string, fence coreexecution.Digest, readback coreaws.CloudFormationProvisionReadback, forceStatus string) (coreaws.EC2ProvisionIntentRecord, error) {
	var zero coreaws.EC2ProvisionIntentRecord
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !fence.Valid() || strings.TrimSpace(readback.StackName) == "" || strings.TrimSpace(readback.Status) == "" || (forceStatus != "" && forceStatus != "failed") {
		return zero, ec2ReadbackStoreError(ErrExecutionStoreInvalid, "invalid_input")
	}
	raw, err := json.Marshal(readback)
	if err != nil {
		return zero, ec2ReadbackStoreError(ErrExecutionStoreInvalid, "marshal")
	}
	if validateCatalogSensitiveData(readback) != nil {
		return zero, ec2ReadbackStoreError(ErrExecutionStoreInvalid, "sensitive_metadata")
	}
	digest, err := coreexecution.CanonicalDigest(readback)
	if err != nil {
		return zero, ec2ReadbackStoreError(err, "digest")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, ec2ReadbackStoreError(err, "begin")
	}
	defer tx.Rollback()
	record, err := loadEC2ProvisionIntentTx(ctx, tx, owner, fence, true)
	if err != nil || readback.StackName != record.Intent.ProviderOperationKey || (readback.StackID != "" && record.ProviderOperationID != "" && readback.StackID != record.ProviderOperationID) {
		if err != nil {
			return zero, ec2ReadbackStoreError(err, "load_intent")
		}
		return zero, ec2ReadbackStoreError(coreexecution.ErrConflict, "binding_conflict")
	}
	status := record.Status
	if forceStatus != "" {
		status = forceStatus
	} else if readback.Status == "CREATE_IN_PROGRESS" || readback.Status == "REVIEW_IN_PROGRESS" || readback.PendingReason != "" {
		status = "pending"
	}
	res, err := tx.ExecContext(ctx, `UPDATE core_execution_ec2_provision_intents SET status=$1,readback_digest=$2,readback_json=$3,revision=revision+1,updated_at=$4 WHERE owner_id=$5 AND fence_digest=$6 AND status NOT IN ('succeeded','failed')`, status, digest, raw, s.now().UTC().Truncate(time.Microsecond), record.Intent.OwnerID, fence)
	if err != nil {
		return zero, ec2ReadbackStoreError(err, "update")
	}
	if n, e := res.RowsAffected(); e != nil || n != 1 {
		if e != nil {
			return zero, ec2ReadbackStoreError(e, "update_count")
		}
		return zero, ec2ReadbackStoreError(coreexecution.ErrConflict, "update_cas")
	}
	record, err = loadEC2ProvisionIntentTx(ctx, tx, record.Intent.OwnerID, fence, true)
	if err != nil {
		return zero, ec2ReadbackStoreError(err, "reload_intent")
	}
	if err := tx.Commit(); err != nil {
		return zero, ec2ReadbackStoreError(err, "commit")
	}
	return record, nil
}

func (s *DatabaseExecutionStore) MarkEC2ProvisionUncertain(ctx context.Context, owner string, fence coreexecution.Digest) error {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !fence.Valid() {
		return ErrExecutionStoreInvalid
	}
	res, err := s.db.ExecContext(ctx, `UPDATE core_execution_ec2_provision_intents SET status='uncertain',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND fence_digest=$3 AND status IN ('intent','accepted','pending')`, s.now().UTC().Truncate(time.Microsecond), owner, fence)
	if err != nil {
		return err
	}
	if n, e := res.RowsAffected(); e != nil {
		return e
	} else if n == 0 {
		var status string
		if e = s.db.QueryRowContext(ctx, `SELECT status FROM core_execution_ec2_provision_intents WHERE owner_id=$1 AND fence_digest=$2`, owner, fence).Scan(&status); e != nil || status != "uncertain" {
			return coreexecution.ErrConflict
		}
	}
	return nil
}

func (s *DatabaseExecutionStore) CompleteEC2Provision(ctx context.Context, completion coreaws.EC2ProvisionCompletion) error {
	if s == nil || s.db == nil || coreaws.ValidateEC2ProvisionIntentSnapshot(completion.Intent.Intent) != nil || completion.Intent.Status == "failed" {
		return ErrExecutionStoreInvalid
	}
	target, err := completion.Target.Normalize()
	if err != nil || target.Digest != completion.Target.Digest || target.ID != completion.Intent.Intent.Request.Target.ID || target.Revision != 2 || target.Kind != coreexecution.TargetKindAWSEC2Instance || target.ComputeReservation != nil {
		return ErrExecutionStoreInvalid
	}
	observation := completion.Observation
	observation.ObservedAt = observation.ObservedAt.UTC().Truncate(time.Microsecond)
	observation.Digest = ""
	observation, err = observation.Normalize()
	if err != nil || observation.TargetID != target.ID || observation.TargetRevision != 2 || observation.State != "ready" || observation.Partial || observation.Stale {
		return ErrExecutionStoreInvalid
	}
	targetRaw, _ := json.Marshal(target)
	observationRaw, _ := json.Marshal(observation)
	if validateCatalogSensitiveData(target) != nil || validateCatalogSensitiveData(observation) != nil {
		return ErrExecutionStoreInvalid
	}
	fence := completion.Intent.Intent.FenceDigest
	observationID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("dirextalk.execution.v2.ec2-provision-observation\x00"+string(fence))).String()
	now := s.now().UTC().Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	record, err := loadEC2ProvisionIntentTx(ctx, tx, completion.Intent.Intent.OwnerID, fence, true)
	if err != nil || record.Intent.RequestDigest != completion.Intent.Intent.RequestDigest || record.Intent.Request.Target.Digest != completion.Intent.Intent.Request.Target.Digest || record.Status == "failed" {
		if err != nil {
			return err
		}
		return coreexecution.ErrConflict
	}
	var current uint64
	// Lock the latest exact target revision. PostgreSQL does not permit FOR
	// UPDATE on an aggregate, and every V2 provision completion follows this
	// row lock before attempting the unique revision-2 insert.
	if err = tx.QueryRowContext(ctx, `SELECT target_revision FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2 ORDER BY target_revision DESC LIMIT 1 FOR UPDATE`, record.Intent.OwnerID, target.ID).Scan(&current); err != nil {
		return err
	}
	if current == 1 {
		if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_targets(owner_id,target_id,target_revision,status,schema_version,provider,target_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,2,'active','execution-target/v2',$3,$4,$5,$6,$6)`, record.Intent.OwnerID, target.ID, target.Provider, target.Digest, targetRaw, now); err != nil {
			return mapExecutionConflict(err)
		}
	} else if current != 2 {
		return coreexecution.ErrConflict
	} else {
		var storedDigest string
		if err = tx.QueryRowContext(ctx, `SELECT target_digest FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2 AND target_revision=2`, record.Intent.OwnerID, target.ID).Scan(&storedDigest); err != nil || storedDigest != string(target.Digest) {
			return coreexecution.ErrConflict
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_target_observations(owner_id,observation_id,target_id,target_revision,revision,status,schema_version,observation_digest,snapshot_json,observed_at) VALUES($1,$2,$3,2,1,'observed','execution-observation/v2',$4,$5,$6) ON CONFLICT (owner_id,observation_id) DO NOTHING`, record.Intent.OwnerID, observationID, target.ID, observation.Digest, observationRaw, observation.ObservedAt); err != nil {
		return mapExecutionConflict(err)
	}
	var storedObservationDigest string
	if err = tx.QueryRowContext(ctx, `SELECT observation_digest FROM core_execution_target_observations WHERE owner_id=$1 AND observation_id=$2`, record.Intent.OwnerID, observationID).Scan(&storedObservationDigest); err != nil || storedObservationDigest != string(observation.Digest) {
		return coreexecution.ErrConflict
	}
	if record.Status != "succeeded" {
		res, updateErr := tx.ExecContext(ctx, `UPDATE core_execution_ec2_provision_intents SET status='succeeded',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND fence_digest=$3 AND status IN ('accepted','pending','uncertain') AND readback_digest IS NOT NULL`, now, record.Intent.OwnerID, fence)
		if updateErr != nil {
			return updateErr
		}
		if n, updateErr := res.RowsAffected(); updateErr != nil || n != 1 {
			return coreexecution.ErrConflict
		}
	}
	return tx.Commit()
}

func loadEC2ProvisionIntentTx(ctx context.Context, tx *sql.Tx, owner string, fence coreexecution.Digest, lock bool) (coreaws.EC2ProvisionIntentRecord, error) {
	var out coreaws.EC2ProvisionIntentRecord
	query := `SELECT owner_id,request_digest,provider_operation_key,provider_operation_id,status,revision,request_json FROM core_execution_ec2_provision_intents WHERE fence_digest=$1`
	args := []any{fence}
	if owner != "" {
		query += ` AND owner_id=$2`
		args = append(args, owner)
	}
	if lock {
		query += ` FOR UPDATE`
	}
	var rowOwner, requestDigest, operationKey, operationID, status string
	var revision uint64
	var raw []byte
	err := tx.QueryRowContext(ctx, query, args...).Scan(&rowOwner, &requestDigest, &operationKey, &operationID, &status, &revision, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return out, coreexecution.ErrNotFound
	}
	if err != nil {
		return out, err
	}
	var request coreaws.EC2ProvisionRequest
	if strictJSON(raw, &request) != nil {
		return out, ErrExecutionStoreDrift
	}
	out = coreaws.EC2ProvisionIntentRecord{Intent: coreaws.EC2ProvisionIntent{OwnerID: rowOwner, FenceDigest: fence, RequestDigest: coreexecution.Digest(requestDigest), ProviderOperationKey: operationKey, Request: request}, Status: status, ProviderOperationID: operationID, Revision: revision}
	if coreaws.ValidateEC2ProvisionIntentSnapshot(out.Intent) != nil || request.OwnerID != rowOwner || request.RequestDigest != out.Intent.RequestDigest {
		return coreaws.EC2ProvisionIntentRecord{}, ErrExecutionStoreDrift
	}
	return out, nil
}

var _ coreaws.EC2ProvisionIntentStore = (*DatabaseExecutionStore)(nil)
