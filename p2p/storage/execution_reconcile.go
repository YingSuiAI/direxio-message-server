package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/google/uuid"
)

type ExecutionSSMReconcileCommand struct {
	OwnerID          string
	RunID            string
	StageID          string
	ExpectedRevision uint64
	IdempotencyKey   string
}

type ExecutionSSMReconcileTransport interface {
	ReconcileCommand(context.Context, coreaws.PollRequest) (coreaws.ReconcileResult, error)
}

type ExecutionEC2ProvisionReconciler interface {
	Reconcile(context.Context, coreaws.EC2ProvisionRequest, coreaws.Credentials) (coreaws.EC2ProvisionCompletion, error)
}

// ExecutionSecretProvisionReconciler is deliberately reconcile-only. The
// storage uncertain path must never receive an Execute/Put capability.
type ExecutionSecretProvisionReconciler interface {
	Reconcile(context.Context, coreaws.SecretParameterProvisionRequest) (coreaws.SecretParameterLease, error)
}

// ExecutionReconciler resolves only an already persisted uncertain dispatch.
// Its SSM dependency has no Dispatch method, while EC2 and secret dependencies
// expose readback-only methods with no create/put path.
type ExecutionReconciler struct {
	store     *DatabaseExecutionStore
	receipts  *DatabaseDispatchReceiptResolver
	ssm       ExecutionSSMReconcileTransport
	provision ExecutionEC2ProvisionReconciler
	secret    ExecutionSecretProvisionReconciler
	outputs   executionServiceOutputHook
	now       func() time.Time
}

func NewExecutionReconciler(store *DatabaseExecutionStore, receipts *DatabaseDispatchReceiptResolver, ssm ExecutionSSMReconcileTransport, provision ExecutionEC2ProvisionReconciler, clock func() time.Time) *ExecutionReconciler {
	if store == nil || receipts == nil || (ssm == nil && provision == nil) {
		return nil
	}
	if clock == nil {
		clock = time.Now
	}
	return &ExecutionReconciler{store: store, receipts: receipts, ssm: ssm, provision: provision, now: clock}
}

// NewExecutionReconcilerWithSecretProvision constructs a reconciler for a
// deployment which only exposes the typed secret readback boundary.
func NewExecutionReconcilerWithSecretProvision(store *DatabaseExecutionStore, receipts *DatabaseDispatchReceiptResolver, secret ExecutionSecretProvisionReconciler, clock func() time.Time) *ExecutionReconciler {
	if store == nil || receipts == nil || secret == nil {
		return nil
	}
	if clock == nil {
		clock = time.Now
	}
	return &ExecutionReconciler{store: store, receipts: receipts, secret: secret, now: clock}
}

// SetSecretProvisionReconciler installs the readback-only secret boundary on
// an existing reconciler. It is safe to call during immutable runtime setup.
func (r *ExecutionReconciler) SetSecretProvisionReconciler(secret ExecutionSecretProvisionReconciler) {
	if r != nil {
		r.secret = secret
	}
}

// SetOutputHook installs the same deterministic, restart-recoverable service
// output materializer used by the normal runner terminal path.
func (r *ExecutionReconciler) SetOutputHook(outputs executionServiceOutputHook) {
	if r != nil {
		r.outputs = outputs
	}
}

func (r *ExecutionReconciler) Ready() bool {
	return r != nil && r.store != nil && r.store.db != nil && r.receipts != nil && (r.ssm != nil || r.provision != nil || r.secret != nil)
}

type executionSSMReconcileEvidence struct {
	runRevision                                     uint64
	leaseID, token, receiptID, attemptID, commandID string
	providerOperationID                             string
	leaseEpoch                                      uint64
	requestDigest, fenceDigest                      coreexecution.Digest
	frozen                                          coreaws.FrozenRequest
	kind                                            coreexecution.StepKind
	ec2Request                                      *coreaws.EC2ProvisionRequest
	secretRequest                                   *coreaws.SecretParameterProvisionRequest
}

func (r *ExecutionReconciler) Reconcile(ctx context.Context, in ExecutionSSMReconcileCommand) (coreexecution.ExecutionRun, error) {
	var zero coreexecution.ExecutionRun
	if !r.Ready() || strings.TrimSpace(in.OwnerID) == "" || !coreexecution.ValidateUUID(in.RunID) || !coreexecution.ValidateUUID(in.StageID) || in.ExpectedRevision == 0 || !coreexecution.ValidateUUID(in.IdempotencyKey) {
		return zero, coreexecution.ErrConflict
	}
	requestDigest, err := coreexecution.CanonicalDigest(struct {
		Schema                  string
		OwnerID, RunID, StageID string
		ExpectedRevision        uint64
	}{"execution-ssm-reconcile/v2", in.OwnerID, in.RunID, in.StageID, in.ExpectedRevision})
	if err != nil {
		return zero, err
	}
	if replay, found, err := r.loadReplay(ctx, in, requestDigest); err != nil || found {
		return replay, err
	}
	evidence, err := r.loadEvidence(ctx, in)
	if err != nil {
		return zero, err
	}
	var outcome string
	var outcomeDigest coreexecution.Digest
	if evidence.kind == coreexecution.StepSecretProvision {
		if r.secret == nil || evidence.secretRequest == nil {
			return zero, coreexecution.ErrConflict
		}
		lease, reconcileErr := r.secret.Reconcile(ctx, *evidence.secretRequest)
		if reconcileErr != nil || validateExecutionSecretReconcileLease(lease, *evidence.secretRequest) != nil {
			// A missing/mismatched readback is not proof of provider failure;
			// leave the durable execution evidence uncertain for a later retry.
			return zero, coreexecution.ErrConflict
		}
		outcome = string(coreaws.PollSucceeded)
		outcomeDigest, err = executionSecretParameterLeaseDigest(lease)
	} else if evidence.kind == coreexecution.StepComputeProvision {
		if r.provision == nil || evidence.ec2Request == nil || len(evidence.ec2Request.Target.CredentialRefs) != 1 {
			return zero, coreexecution.ErrConflict
		}
		ref := evidence.ec2Request.Target.CredentialRefs[0]
		credential, credentialErr := r.receipts.Credentials.ResolveCredentialRevision(ctx, in.OwnerID, ref.Ref, ref.Revision)
		if credentialErr != nil || credential.ID != ref.Ref || credential.Revision != int64(ref.Revision) || credential.VerifiedRevision != int64(ref.Revision) {
			return zero, coreexecution.ErrConflict
		}
		completion, reconcileErr := r.provision.Reconcile(ctx, *evidence.ec2Request, credential)
		switch {
		case reconcileErr == nil:
			outcome = string(coreaws.PollSucceeded)
			outcomeDigest, err = coreexecution.CanonicalDigest(struct {
				Target      coreexecution.ExecutionTarget
				Observation coreexecution.TargetObservation
			}{completion.Target, completion.Observation})
		case errors.Is(reconcileErr, coreaws.ErrEC2ProvisionFailed):
			outcome = string(coreaws.PollFailed)
			outcomeDigest, err = coreexecution.CanonicalDigest(struct {
				Status  string
				Request coreexecution.Digest
			}{"cloudformation_create_failed", evidence.requestDigest})
		default:
			return zero, coreexecution.ErrConflict
		}
	} else {
		if r.ssm == nil {
			return zero, coreexecution.ErrConflict
		}
		dispatch, resolveErr := r.receipts.ResolveDispatchReceipt(ctx, in.OwnerID, evidence.fenceDigest)
		if resolveErr != nil || dispatch.CommandID != evidence.commandID || dispatch.RequestDigest != evidence.requestDigest || dispatch.Frozen.OwnerID != in.OwnerID || dispatch.Frozen.RunID != in.RunID || dispatch.Frozen.StageID != in.StageID || dispatch.Frozen.AttemptID != evidence.attemptID {
			return zero, coreexecution.ErrConflict
		}
		evidence.frozen = dispatch.Frozen
		result, reconcileErr := r.ssm.ReconcileCommand(ctx, coreaws.PollRequest{
			OwnerID: in.OwnerID, Frozen: evidence.frozen, CommandID: evidence.commandID,
			Known: true, FenceDigest: evidence.fenceDigest,
		})
		if reconcileErr != nil {
			return zero, coreexecution.ErrConflict
		}
		outcome = string(result.Status)
		switch result.Status {
		case coreaws.PollSucceeded, coreaws.PollFailed, coreaws.PollCanceled:
		default:
			// Pending/running/unknown provider evidence remains fenced uncertain.
			return zero, coreexecution.ErrConflict
		}
		outcomeDigest = result.OutputDigest
		if !outcomeDigest.Valid() {
			outcomeDigest, err = coreexecution.CanonicalDigest(struct {
				Status, CommandID, InstanceID, ProviderOperation string
			}{string(result.Status), result.CommandID, result.InstanceID, result.ProviderOperation})
		}
	}
	if err != nil || !outcomeDigest.Valid() {
		return zero, coreexecution.ErrConflict
	}
	run, err := r.commitResolution(ctx, in, requestDigest, evidence, outcome, outcomeDigest)
	if err == nil && outcome == string(coreaws.PollSucceeded) && r.outputs != nil {
		// The provider outcome and DAG transition are already durable. The CAS
		// artifact publisher is intentionally recoverable rather than part of
		// the provider transaction, matching the normal runner terminal path.
		bounded, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = r.outputs.EnsureReceipt(bounded, in.OwnerID, evidence.receiptID, evidence.attemptID)
		cancel()
	}
	return run, err
}

func (r *ExecutionReconciler) loadReplay(ctx context.Context, in ExecutionSSMReconcileCommand, requestDigest coreexecution.Digest) (coreexecution.ExecutionRun, bool, error) {
	var out coreexecution.ExecutionRun
	var storedRequest, storedResponseDigest, status, runID string
	var raw []byte
	err := r.store.db.QueryRowContext(ctx, `SELECT request_digest,response_digest,status,COALESCE(run_id::text,''),response_json FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2`, in.OwnerID, in.IdempotencyKey).Scan(&storedRequest, &storedResponseDigest, &status, &runID, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		var exists bool
		if err = r.store.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2)`, in.OwnerID, in.RunID).Scan(&exists); err != nil {
			return out, false, err
		}
		if !exists {
			return out, false, coreexecution.ErrNotFound
		}
		return out, false, nil
	}
	if err != nil {
		return out, false, err
	}
	if storedRequest != string(requestDigest) || status != "succeeded" || runID != in.RunID || strictJSON(raw, &out) != nil || out.OwnerID != in.OwnerID || out.RunID != in.RunID || out.Revision <= in.ExpectedRevision || out.Validate() != nil {
		return coreexecution.ExecutionRun{}, false, coreexecution.ErrConflict
	}
	digest, err := coreexecution.CanonicalDigest(out)
	if err != nil || string(digest) != storedResponseDigest {
		return coreexecution.ExecutionRun{}, false, coreexecution.ErrConflict
	}
	return out, true, nil
}

func (r *ExecutionReconciler) loadEvidence(ctx context.Context, in ExecutionSSMReconcileCommand) (executionSSMReconcileEvidence, error) {
	var e executionSSMReconcileEvidence
	var runStatus, stageStatus, leaseStatus, receiptStatus, intentStatus string
	var leaseProviderOperation, receiptProviderOperation string
	var requestDigest, fenceDigest, receiptRequest, receiptFence string
	var snapshot []byte
	err := r.store.db.QueryRowContext(ctx, `SELECT run.revision,run.status,stage.status,lease.lease_id::text,lease.token::text,lease.epoch,lease.status,lease.receipt_id::text,COALESCE(lease.provider_operation_id,''),receipt.attempt_id::text,receipt.status,COALESCE(receipt.command_id,''),COALESCE(receipt.provider_operation_id,''),receipt.request_digest,receipt.fence_digest,intent.status,intent.request_digest,intent.fence_digest,intent.snapshot_json FROM core_execution_runs run JOIN core_execution_run_stages stage ON stage.owner_id=run.owner_id AND stage.run_id=run.run_id JOIN core_execution_target_mutation_leases lease ON lease.owner_id=stage.owner_id AND lease.target_id=stage.target_id AND lease.target_revision=stage.target_revision JOIN core_execution_receipts receipt ON receipt.owner_id=lease.owner_id AND receipt.run_id=lease.run_id AND receipt.receipt_id=lease.receipt_id JOIN core_execution_dispatch_intents intent ON intent.owner_id=receipt.owner_id AND intent.run_id=receipt.run_id AND intent.stage_id=stage.stage_id AND intent.receipt_id=receipt.receipt_id AND intent.attempt_id=receipt.attempt_id WHERE run.owner_id=$1 AND run.run_id=$2 AND stage.stage_id=$3`, in.OwnerID, in.RunID, in.StageID).Scan(&e.runRevision, &runStatus, &stageStatus, &e.leaseID, &e.token, &e.leaseEpoch, &leaseStatus, &e.receiptID, &leaseProviderOperation, &e.attemptID, &receiptStatus, &e.commandID, &receiptProviderOperation, &receiptRequest, &receiptFence, &intentStatus, &requestDigest, &fenceDigest, &snapshot)
	if errors.Is(err, sql.ErrNoRows) {
		return e, coreexecution.ErrNotFound
	}
	if err != nil {
		return e, err
	}
	var envelope struct {
		Frozen          *coreaws.FrozenRequestSnapshot           `json:"frozen_request_snapshot"`
		EC2Provision    *coreaws.EC2ProvisionRequest             `json:"ec2_provision_request"`
		SecretProvision *coreaws.SecretParameterProvisionRequest `json:"secret_parameter_request"`
	}
	if e.runRevision != in.ExpectedRevision || runStatus != string(coreexecution.RunUncertain) || stageStatus != string(coreexecution.StageUncertain) || leaseStatus != "uncertain" || receiptStatus != string(coreexecution.ReceiptUncertain) || intentStatus != "uncertain" || leaseProviderOperation != receiptProviderOperation || receiptRequest != requestDigest || receiptFence != fenceDigest || json.Unmarshal(snapshot, &envelope) != nil || (envelope.Frozen != nil && envelope.EC2Provision != nil) || (envelope.Frozen != nil && envelope.SecretProvision != nil) || (envelope.EC2Provision != nil && envelope.SecretProvision != nil) || (envelope.Frozen == nil && envelope.EC2Provision == nil && envelope.SecretProvision == nil) {
		return executionSSMReconcileEvidence{}, coreexecution.ErrConflict
	}
	if envelope.Frozen != nil {
		if e.commandID == "" || envelope.Frozen.OwnerID != in.OwnerID || envelope.Frozen.RunID != in.RunID || envelope.Frozen.StageID != in.StageID || envelope.Frozen.AttemptID != e.attemptID {
			return executionSSMReconcileEvidence{}, coreexecution.ErrConflict
		}
		e.kind = envelope.Frozen.Script.Step.Kind
		if e.kind == coreexecution.StepComputeProvision {
			return executionSSMReconcileEvidence{}, coreexecution.ErrConflict
		}
	} else if envelope.EC2Provision != nil {
		request := envelope.EC2Provision
		intent := coreaws.EC2ProvisionIntent{OwnerID: request.OwnerID, FenceDigest: request.FenceDigest, RequestDigest: request.RequestDigest, ProviderOperationKey: coreaws.EC2ProvisionOperationKey(request.Target.ID), Request: *request}
		if e.commandID != "" || coreaws.ValidateEC2ProvisionIntentSnapshot(intent) != nil || request.OwnerID != in.OwnerID || request.RunID != in.RunID || request.StageID != in.StageID || request.AttemptID != e.attemptID || request.RequestDigest != coreexecution.Digest(requestDigest) || request.FenceDigest != coreexecution.Digest(fenceDigest) || receiptProviderOperation != coreaws.EC2ProvisionOperationKey(request.Target.ID) {
			return executionSSMReconcileEvidence{}, coreexecution.ErrConflict
		}
		e.kind = coreexecution.StepComputeProvision
		e.ec2Request = request
	} else {
		request := envelope.SecretProvision
		if e.commandID != "" || receiptProviderOperation != "" || leaseProviderOperation != "" || request.OwnerID != in.OwnerID || request.RunID != in.RunID || request.StageID != in.StageID || request.AttemptID != e.attemptID || request.RequestDigest != coreexecution.Digest(requestDigest) || request.FenceDigest != coreexecution.Digest(fenceDigest) || !coreexecution.ValidateUUID(request.AttemptID) || request.CredentialID == "" || request.CredentialRevision == 0 {
			return executionSSMReconcileEvidence{}, coreexecution.ErrConflict
		}
		access, secret, token := request.Credential.StoredSecretBytes()
		hasPlaintext := len(access) != 0 || len(secret) != 0 || len(token) != 0
		clearSecretParameterBytes(access, secret, token)
		if hasPlaintext || request.Credential.ID != request.CredentialID || request.Credential.Revision != int64(request.CredentialRevision) {
			return executionSSMReconcileEvidence{}, coreexecution.ErrConflict
		}
		credential, credentialErr := r.receipts.Credentials.ResolveCredentialRevision(ctx, in.OwnerID, request.CredentialID, request.CredentialRevision)
		if credentialErr != nil || credential.ID != request.CredentialID || credential.Revision != int64(request.CredentialRevision) || credential.VerifiedRevision != int64(request.CredentialRevision) || credential.Region != request.Credential.Region || credential.AccountID != request.Credential.AccountID || credential.UserARN != request.Credential.UserARN {
			return executionSSMReconcileEvidence{}, coreexecution.ErrConflict
		}
		request.Credential = credential
		canonical, digestErr := coreaws.CanonicalSecretParameterRequestDigest(*request)
		if digestErr != nil || canonical != request.RequestDigest {
			return executionSSMReconcileEvidence{}, coreexecution.ErrConflict
		}
		if name, nameErr := coreaws.SecretParameterName(request.Target.ID, request.AttemptID, request.SecretRef); nameErr != nil || name == "" {
			return executionSSMReconcileEvidence{}, coreexecution.ErrConflict
		}
		e.kind = coreexecution.StepSecretProvision
		e.secretRequest = request
	}
	e.providerOperationID = receiptProviderOperation
	e.requestDigest, e.fenceDigest = coreexecution.Digest(requestDigest), coreexecution.Digest(fenceDigest)
	if !e.requestDigest.Valid() || !e.fenceDigest.Valid() || !coreexecution.ValidateUUID(e.leaseID) || !coreexecution.ValidateUUID(e.token) || e.leaseEpoch == 0 || !coreexecution.ValidateUUID(e.receiptID) || !coreexecution.ValidateUUID(e.attemptID) {
		return executionSSMReconcileEvidence{}, coreexecution.ErrConflict
	}
	return e, nil
}

// commitResolution is the authoritative uncertain-outcome transition. The
// audit row is inserted before any evidence is advanced so the database scope
// guard can prove the exact uncertain lease/receipt pair. Receipt, attempt,
// intent, stage, run, DAG materialization, lease release, and idempotency are
// then committed in one transaction.
func (r *ExecutionReconciler) commitResolution(ctx context.Context, in ExecutionSSMReconcileCommand, requestDigest coreexecution.Digest, evidence executionSSMReconcileEvidence, outcome string, outcomeDigest coreexecution.Digest) (coreexecution.ExecutionRun, error) {
	var zero coreexecution.ExecutionRun
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()

	var oldOutcome, oldDigest string
	err = tx.QueryRowContext(ctx, `SELECT outcome,outcome_digest FROM core_execution_reconciliation_resolutions WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 AND request_digest=$4`, in.OwnerID, in.RunID, in.StageID, evidence.requestDigest).Scan(&oldOutcome, &oldDigest)
	if err == nil {
		if oldOutcome != outcome || oldDigest != string(outcomeDigest) {
			return zero, coreexecution.ErrConflict
		}
		materialized, loadErr := loadExecutionMaterialization(ctx, tx, in.OwnerID, in.RunID)
		if loadErr != nil || materialized.Run.Revision <= in.ExpectedRevision || materialized.Run.Status == coreexecution.RunUncertain {
			return zero, coreexecution.ErrConflict
		}
		if err = insertExecutionReconcileIdempotencyTx(ctx, tx, in, requestDigest, materialized.Run, r.now); err != nil {
			return zero, err
		}
		if err = tx.Commit(); err != nil {
			return zero, err
		}
		return materialized.Run, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return zero, err
	}

	var runRevision, leaseEpoch uint64
	var runStatus, stageStatus, leaseStatus, receiptStatus, attemptStatus, intentStatus string
	var taskID string
	var leaseID, token, receiptID, attemptID, commandID, leaseProviderOperation, providerOperation, receiptRequest, receiptFence, intentRequest, intentFence string
	err = tx.QueryRowContext(ctx, `SELECT run.revision,run.status,stage.status,COALESCE(stage.task_id::text,''),lease.lease_id::text,lease.token::text,lease.epoch,lease.status,lease.receipt_id::text,COALESCE(lease.provider_operation_id,''),receipt.attempt_id::text,receipt.status,attempt.status,COALESCE(receipt.command_id,''),COALESCE(receipt.provider_operation_id,''),receipt.request_digest,receipt.fence_digest,intent.status,intent.request_digest,intent.fence_digest FROM core_execution_runs run JOIN core_execution_run_stages stage ON stage.owner_id=run.owner_id AND stage.run_id=run.run_id JOIN core_execution_target_mutation_leases lease ON lease.owner_id=stage.owner_id AND lease.target_id=stage.target_id AND lease.target_revision=stage.target_revision JOIN core_execution_receipts receipt ON receipt.owner_id=lease.owner_id AND receipt.run_id=lease.run_id AND receipt.receipt_id=lease.receipt_id JOIN core_execution_step_attempts attempt ON attempt.owner_id=receipt.owner_id AND attempt.attempt_id=receipt.attempt_id JOIN core_execution_dispatch_intents intent ON intent.owner_id=receipt.owner_id AND intent.run_id=receipt.run_id AND intent.stage_id=stage.stage_id AND intent.receipt_id=receipt.receipt_id AND intent.attempt_id=receipt.attempt_id WHERE run.owner_id=$1 AND run.run_id=$2 AND stage.stage_id=$3 FOR UPDATE OF run,stage,lease,receipt,attempt,intent`, in.OwnerID, in.RunID, in.StageID).Scan(&runRevision, &runStatus, &stageStatus, &taskID, &leaseID, &token, &leaseEpoch, &leaseStatus, &receiptID, &leaseProviderOperation, &attemptID, &receiptStatus, &attemptStatus, &commandID, &providerOperation, &receiptRequest, &receiptFence, &intentStatus, &intentRequest, &intentFence)
	if err != nil || runRevision != in.ExpectedRevision || runStatus != string(coreexecution.RunUncertain) || stageStatus != string(coreexecution.StageUncertain) || leaseID != evidence.leaseID || token != evidence.token || leaseEpoch != evidence.leaseEpoch || leaseStatus != "uncertain" || receiptID != evidence.receiptID || leaseProviderOperation != evidence.providerOperationID || providerOperation != evidence.providerOperationID || attemptID != evidence.attemptID || receiptStatus != string(coreexecution.ReceiptUncertain) || attemptStatus != string(coreexecution.AttemptUncertain) || commandID != evidence.commandID || receiptRequest != string(evidence.requestDigest) || receiptFence != string(evidence.fenceDigest) || intentStatus != "uncertain" || intentRequest != receiptRequest || intentFence != receiptFence {
		return zero, fmt.Errorf("lock exact reconciliation evidence (query_err=%v run_revision=%d expected=%d run=%s stage=%s lease=%s/%d receipt=%s attempt=%s intent=%s command_match=%t provider_match=%t request_match=%t fence_match=%t): %w", err, runRevision, in.ExpectedRevision, runStatus, stageStatus, leaseStatus, leaseEpoch, receiptStatus, attemptStatus, intentStatus, commandID == evidence.commandID, leaseProviderOperation == evidence.providerOperationID && providerOperation == evidence.providerOperationID, receiptRequest == string(evidence.requestDigest) && intentRequest == receiptRequest, receiptFence == string(evidence.fenceDigest) && intentFence == receiptFence, coreexecution.ErrConflict)
	}

	receiptTarget, attemptTarget, stageTarget, runTarget, terminalReason, err := executionReconcileStatuses(outcome)
	if err != nil {
		return zero, err
	}
	at := r.now().UTC().Truncate(time.Microsecond)
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_reconciliation_resolutions(owner_id,run_id,stage_id,lease_id,token,epoch,receipt_id,provider_operation_id,request_digest,outcome,outcome_digest,observed_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$12)`, in.OwnerID, in.RunID, in.StageID, evidence.leaseID, evidence.token, evidence.leaseEpoch, evidence.receiptID, evidence.providerOperationID, evidence.requestDigest, outcome, outcomeDigest, at); err != nil {
		return zero, fmt.Errorf("insert reconciliation resolution: %w", mapExecutionConflict(err))
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_execution_receipts SET status=$1,response_digest=$2,revision=revision+1 WHERE owner_id=$3 AND receipt_id=$4 AND attempt_id=$5 AND status='uncertain' AND request_digest=$6 AND fence_digest=$7`, receiptTarget, outcomeDigest, in.OwnerID, evidence.receiptID, evidence.attemptID, evidence.requestDigest, evidence.fenceDigest)
	if err != nil {
		return zero, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return zero, rowsErr
		}
		return zero, fmt.Errorf("advance reconciled receipt: %w", coreexecution.ErrConflict)
	}
	result, err = tx.ExecContext(ctx, `UPDATE core_execution_step_attempts SET status=$1,output_digest=$2,completed_at=COALESCE(completed_at,$3),revision=revision+1 WHERE owner_id=$4 AND attempt_id=$5 AND run_id=$6 AND stage_id=$7 AND status='uncertain'`, attemptTarget, outcomeDigest, at, in.OwnerID, evidence.attemptID, in.RunID, in.StageID)
	if err != nil {
		return zero, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return zero, rowsErr
		}
		return zero, fmt.Errorf("advance reconciled attempt: %w", coreexecution.ErrConflict)
	}
	result, err = tx.ExecContext(ctx, `UPDATE core_execution_dispatch_intents SET status=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND run_id=$4 AND stage_id=$5 AND receipt_id=$6 AND attempt_id=$7 AND status='uncertain' AND request_digest=$8 AND fence_digest=$9`, receiptTarget, at, in.OwnerID, in.RunID, in.StageID, evidence.receiptID, evidence.attemptID, evidence.requestDigest, evidence.fenceDigest)
	if err != nil {
		return zero, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return zero, rowsErr
		}
		return zero, fmt.Errorf("advance reconciled intent: %w", coreexecution.ErrConflict)
	}
	if receiptTarget == string(coreexecution.ReceiptSucceeded) {
		var remaining bool
		if err = tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM core_execution_plan_steps ps JOIN core_execution_run_stages rs ON rs.owner_id=ps.owner_id AND rs.plan_id=ps.plan_id AND rs.plan_revision=ps.plan_revision AND rs.plan_stage_key=ps.stage_key WHERE rs.owner_id=$1 AND rs.run_id=$2 AND rs.stage_id=$3 AND ps.step_set=(SELECT step_set FROM core_execution_step_attempts WHERE owner_id=$1 AND attempt_id=$4) AND NOT EXISTS (SELECT 1 FROM core_execution_step_attempts ax WHERE ax.owner_id=$1 AND ax.run_id=$2 AND ax.stage_id=$3 AND ax.step_set=ps.step_set AND ax.step_key=ps.step_key AND ax.status='succeeded'))`, in.OwnerID, in.RunID, in.StageID, evidence.attemptID).Scan(&remaining); err != nil {
			return zero, err
		}
		if remaining {
			return zero, fmt.Errorf("reconciled stage has unexecuted steps: %w", coreexecution.ErrConflict)
		}
	}
	if err = transitionUncertainExecutionStageTx(ctx, tx, in.OwnerID, in.RunID, in.StageID, stageTarget, at); err != nil {
		return zero, fmt.Errorf("transition reconciled stage: %w", err)
	}
	if taskID != "" {
		taskStatus, taskCode, taskSummary := "succeeded", "", ""
		switch stageTarget {
		case coreexecution.StageFailed:
			taskStatus, taskCode, taskSummary = "failed", "execution_reconciled_failed", "external dispatch reconciled as failed"
		case coreexecution.StageCanceled:
			taskStatus, taskCode, taskSummary = "canceled", "canceled", "external dispatch reconciled as canceled"
		}
		if err = terminalizeExecutionTaskTx(ctx, tx, in.OwnerID, taskID, taskStatus, taskCode, taskSummary, at); err != nil {
			return zero, fmt.Errorf("terminalize reconciled execution task: %w", err)
		}
	}
	result, err = tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET status='released',expires_at=NULL,revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND lease_id=$3 AND token=$4 AND epoch=$5 AND status='uncertain' AND run_id=$6 AND stage_id=$7 AND receipt_id=$8`, at, in.OwnerID, evidence.leaseID, evidence.token, evidence.leaseEpoch, in.RunID, in.StageID, evidence.receiptID)
	if err != nil {
		return zero, err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return zero, rowsErr
		}
		return zero, fmt.Errorf("release reconciled target lease: %w", coreexecution.ErrConflict)
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_confirmations SET reservation_json=NULL,revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND task_id=(SELECT task_id FROM core_execution_run_stages WHERE owner_id=$2 AND run_id=$3 AND stage_id=$4) AND state='consumed' AND reservation_json IS NOT NULL`, at, in.OwnerID, in.RunID, in.StageID); err != nil {
		return zero, err
	}
	if err = reopenUncertainExecutionRunTx(ctx, tx, in.OwnerID, in.RunID, at); err != nil {
		return zero, fmt.Errorf("reopen reconciled run: %w", err)
	}
	switch stageTarget {
	case coreexecution.StageSucceeded:
		if err = r.store.materializeNewlyUnblockedStages(ctx, tx, in.OwnerID, in.RunID, at); err != nil {
			return zero, fmt.Errorf("materialize reconciled DAG: %w", err)
		}
	case coreexecution.StageFailed, coreexecution.StageCanceled:
		if err = skipBlockedExecutionDescendantsTx(ctx, tx, in.OwnerID, in.RunID, at); err != nil {
			return zero, err
		}
	}
	if runTarget == coreexecution.RunCanceled {
		if err = transitionSettledCanceledExecutionRunTx(ctx, tx, in.OwnerID, in.RunID, terminalReason, at); err != nil {
			return zero, err
		}
	} else if _, err = transitionExecutionRunTx(ctx, tx, in.OwnerID, in.RunID, runTarget, terminalReason, at, true); err != nil {
		return zero, fmt.Errorf("settle reconciled run: %w", err)
	}
	if err = insertExecutionEvent(ctx, tx, in.OwnerID, in.RunID, "uncertain_reconciled_"+outcome, in.StageID, stageTarget, at); err != nil {
		return zero, err
	}

	materialized, err := loadExecutionMaterialization(ctx, tx, in.OwnerID, in.RunID)
	if err != nil || materialized.Run.Revision <= in.ExpectedRevision || materialized.Run.Status == coreexecution.RunUncertain {
		return zero, fmt.Errorf("load reconciled materialization: %w", coreexecution.ErrConflict)
	}
	if err = syncExecutionDeploymentAfterReconcileTx(ctx, tx, materialized.Run, at); err != nil {
		return zero, err
	}
	if err = insertExecutionReconcileIdempotencyTx(ctx, tx, in, requestDigest, materialized.Run, r.now); err != nil {
		return zero, err
	}
	if err = tx.Commit(); err != nil {
		return zero, err
	}
	return materialized.Run, nil
}

func executionReconcileStatuses(outcome string) (string, string, coreexecution.StageStatus, coreexecution.RunStatus, string, error) {
	switch outcome {
	case string(coreaws.PollSucceeded):
		return string(coreexecution.ReceiptSucceeded), string(coreexecution.AttemptSucceeded), coreexecution.StageSucceeded, coreexecution.RunSucceeded, "", nil
	case string(coreaws.PollFailed):
		return string(coreexecution.ReceiptFailed), string(coreexecution.AttemptFailed), coreexecution.StageFailed, coreexecution.RunFailed, "stage_failed_after_reconcile", nil
	case string(coreaws.PollCanceled):
		return string(coreexecution.ReceiptCanceled), string(coreexecution.AttemptCanceled), coreexecution.StageCanceled, coreexecution.RunCanceled, "provider_operation_canceled", nil
	default:
		return "", "", "", "", "", coreexecution.ErrConflict
	}
}

func executionSecretParameterLeaseDigest(lease coreaws.SecretParameterLease) (coreexecution.Digest, error) {
	return coreexecution.CanonicalDigest(struct {
		SchemaVersion   string
		OwnerID         string
		RunID           string
		StageID         string
		TargetID        string
		TargetRevision  uint64
		TargetDigest    coreexecution.Digest
		SecretRef       coreexecution.CredentialRef
		ParameterName   string
		ProviderVersion int64
		FenceDigest     coreexecution.Digest
		RequestDigest   coreexecution.Digest
	}{lease.SchemaVersion, lease.OwnerID, lease.RunID, lease.ProvisionStageID, lease.TargetID, lease.TargetRevision, lease.TargetDigest, lease.SecretRef, lease.ParameterName, lease.ProviderVersion, lease.FenceDigest, lease.RequestDigest})
}

func validateExecutionSecretReconcileLease(lease coreaws.SecretParameterLease, req coreaws.SecretParameterProvisionRequest) error {
	expectedName, err := coreaws.SecretParameterName(req.Target.ID, req.AttemptID, req.SecretRef)
	if err != nil || lease.SchemaVersion != "execution-secret-parameter/v1" || lease.OwnerID != req.OwnerID || lease.RunID != req.RunID || lease.ProvisionStageID != req.StageID || lease.ProvisionAttemptID != req.AttemptID || lease.TargetID != req.Target.ID || lease.TargetRevision != req.Target.Revision || lease.TargetDigest != req.Target.Digest || lease.SecretRef != req.SecretRef || lease.ParameterName != expectedName || lease.ContainerMountPath != "/run/secrets/dirextalk/"+req.SecretRef.Purpose || lease.FenceDigest != req.FenceDigest || lease.RequestDigest != req.RequestDigest || lease.ProviderVersion <= 0 {
		return coreexecution.ErrConflict
	}
	return nil
}

func clearSecretParameterBytes(values ...[]byte) {
	for _, value := range values {
		clear(value)
	}
}

func transitionUncertainExecutionStageTx(ctx context.Context, tx *sql.Tx, owner, runID, stageID string, status coreexecution.StageStatus, at time.Time) error {
	if status != coreexecution.StageSucceeded && status != coreexecution.StageFailed && status != coreexecution.StageCanceled {
		return ErrExecutionStoreInvalid
	}
	var revision uint64
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT revision,snapshot_json FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3 AND status='uncertain' FOR UPDATE`, owner, runID, stageID).Scan(&revision, &raw); err != nil {
		return coreexecution.ErrConflict
	}
	var stage coreexecution.RunStage
	if strictJSON(raw, &stage) != nil || stage.OwnerID != owner || stage.RunID != runID || stage.StageID != stageID || stage.Status != coreexecution.StageUncertain || !stage.Status.CanTransition(status) {
		return ErrExecutionStoreDrift
	}
	stage.Status = status
	stage.UpdatedAt = at
	if stage.Validate() != nil {
		return ErrExecutionStoreDrift
	}
	snapshot, err := json.Marshal(stage)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_execution_run_stages SET status=$1,revision=$2,updated_at=$3,snapshot_json=$4 WHERE owner_id=$5 AND run_id=$6 AND stage_id=$7 AND status='uncertain' AND revision=$8`, status, revision+1, at, snapshot, owner, runID, stageID, revision)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	return nil
}

func reopenUncertainExecutionRunTx(ctx context.Context, tx *sql.Tx, owner, runID string, at time.Time) error {
	var revision uint64
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT revision,snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 AND status='uncertain' FOR UPDATE`, owner, runID).Scan(&revision, &raw); err != nil {
		return coreexecution.ErrConflict
	}
	var run coreexecution.ExecutionRun
	if strictJSON(raw, &run) != nil || run.OwnerID != owner || run.RunID != runID || run.Revision != revision || run.Status != coreexecution.RunUncertain || !run.Status.CanTransition(coreexecution.RunRunning) {
		return ErrExecutionStoreDrift
	}
	run.Status = coreexecution.RunRunning
	run.TerminalReason = ""
	run.FinishedAt = time.Time{}
	run.Revision++
	run.UpdatedAt = at
	if run.Validate() != nil {
		return ErrExecutionStoreDrift
	}
	snapshot, err := json.Marshal(run)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_execution_runs SET status='running',terminal_reason='',completed_at=NULL,revision=$1,updated_at=$2,snapshot_json=$3 WHERE owner_id=$4 AND run_id=$5 AND status='uncertain' AND revision=$6`, run.Revision, at, snapshot, owner, runID, revision)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	return insertExecutionRunRevision(ctx, tx, run)
}

func transitionSettledCanceledExecutionRunTx(ctx context.Context, tx *sql.Tx, owner, runID, reason string, at time.Time) error {
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND status IN ('blocked','waiting_user','queued','running','uncertain'))`, owner, runID).Scan(&active); err != nil {
		return err
	}
	if active {
		// A canceled provider operation settles only its exact stage.  Independent
		// roots may still be queued, running, or approval-gated; keep the aggregate
		// running until those stages settle instead of rolling back the durable
		// resolution and leaving the canceled lease fenced forever.
		return nil
	}
	var revision uint64
	var raw []byte
	if err := tx.QueryRowContext(ctx, `SELECT revision,snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 AND status='running' FOR UPDATE`, owner, runID).Scan(&revision, &raw); err != nil {
		return coreexecution.ErrConflict
	}
	var run coreexecution.ExecutionRun
	if strictJSON(raw, &run) != nil || run.OwnerID != owner || run.RunID != runID || run.Revision != revision || run.Status != coreexecution.RunRunning || !run.Status.CanTransition(coreexecution.RunCanceled) {
		return ErrExecutionStoreDrift
	}
	run.Status = coreexecution.RunCanceled
	run.TerminalReason = reason
	run.FinishedAt = at
	run.Revision++
	run.UpdatedAt = at
	if run.Validate() != nil {
		return ErrExecutionStoreDrift
	}
	snapshot, err := json.Marshal(run)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_execution_runs SET status='canceled',terminal_reason=$1,completed_at=$2,revision=$3,updated_at=$2,snapshot_json=$4 WHERE owner_id=$5 AND run_id=$6 AND status='running' AND revision=$7`, reason, at, run.Revision, snapshot, owner, runID, revision)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed != 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	return insertExecutionRunRevision(ctx, tx, run)
}

func syncExecutionDeploymentAfterReconcileTx(ctx context.Context, tx *sql.Tx, run coreexecution.ExecutionRun, at time.Time) error {
	if run.DeploymentID == "" || (run.Status != coreexecution.RunRunning && run.Status != coreexecution.RunFailed && run.Status != coreexecution.RunCanceled) {
		return nil
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_execution_deployments SET state=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND deployment_id=$4 AND current_run_id=$5 AND state IN ('pending','waiting_user','queued','running','uncertain') AND state<>$1`, run.Status, at, run.OwnerID, run.DeploymentID, run.RunID)
	if err != nil {
		return err
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil || changed > 1 {
		if rowsErr != nil {
			return rowsErr
		}
		return coreexecution.ErrConflict
	}
	return nil
}

func insertExecutionReconcileIdempotencyTx(ctx context.Context, tx *sql.Tx, in ExecutionSSMReconcileCommand, requestDigest coreexecution.Digest, run coreexecution.ExecutionRun, clock func() time.Time) error {
	responseRaw, err := json.Marshal(run)
	if err != nil {
		return err
	}
	responseDigest, err := coreexecution.CanonicalDigest(run)
	if err != nil {
		return err
	}
	key, err := uuid.Parse(in.IdempotencyKey)
	if err != nil {
		return coreexecution.ErrConflict
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO core_execution_idempotency(owner_id,idempotency_id,run_id,key_digest,request_digest,response_digest,status,schema_version,response_json,created_at) VALUES($1,$2,$3,$4,$5,$6,'succeeded','execution-idempotency/v2',$7,$8) ON CONFLICT(owner_id,idempotency_id) DO NOTHING`, in.OwnerID, key, in.RunID, string(digestBytes([]byte(in.IdempotencyKey))), requestDigest, responseDigest, responseRaw, clock().UTC().Truncate(time.Microsecond))
	if err != nil {
		return mapExecutionConflict(err)
	}
	if changed, rowsErr := result.RowsAffected(); rowsErr != nil {
		return rowsErr
	} else if changed == 1 {
		return nil
	}
	var oldRequest, oldResponse string
	var oldRaw []byte
	if err = tx.QueryRowContext(ctx, `SELECT request_digest,response_digest,response_json FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2 FOR UPDATE`, in.OwnerID, key).Scan(&oldRequest, &oldResponse, &oldRaw); err != nil || oldRequest != string(requestDigest) || oldResponse != string(responseDigest) {
		return coreexecution.ErrConflict
	}
	var oldRun coreexecution.ExecutionRun
	if strictJSON(oldRaw, &oldRun) != nil || oldRun.OwnerID != run.OwnerID || oldRun.RunID != run.RunID || oldRun.Revision != run.Revision || oldRun.Status != run.Status {
		return coreexecution.ErrConflict
	}
	return nil
}
