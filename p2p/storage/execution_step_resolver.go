package storage

// Authoritative execution.v2 step materialization. This resolver is the only
// production path that turns a claimed stage into a provider request.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionrunner"
	"github.com/google/uuid"
)

var ErrExecutionStepResolve = errors.New("execution step resolver: immutable snapshot unavailable")

type CredentialRevisionResolver interface {
	ResolveCredentialRevision(context.Context, string, string, uint64) (coreaws.Credentials, error)
}

func (r *PostgresAWSRepository) ResolveCredentialRevision(ctx context.Context, owner, id string, revision uint64) (coreaws.Credentials, error) {
	if r == nil || owner == "" || owner != r.ownerID || revision == 0 {
		return coreaws.Credentials{}, coreaws.ErrInvalid
	}
	return r.GetCredentialRevision(ctx, id, int64(revision))
}

// ExecutionStepResolver verifies every immutable pin after the stage claim;
// it never consults a current/latest plan or target.
type ExecutionStepResolver struct {
	Store       *DatabaseExecutionStore
	Credentials CredentialRevisionResolver
	Artifacts   *FilesystemArtifactResolver
}

func NewExecutionStepResolver(store *DatabaseExecutionStore, credentials CredentialRevisionResolver, artifacts *FilesystemArtifactResolver) *ExecutionStepResolver {
	if store == nil || credentials == nil {
		return nil
	}
	return &ExecutionStepResolver{Store: store, Credentials: credentials, Artifacts: artifacts}
}

func (r *ExecutionStepResolver) ResolveStep(ctx context.Context, claim executionrunner.StageLease, next executionrunner.NextStep) (executionrunner.PreparedStep, error) {
	var out executionrunner.PreparedStep
	if r == nil || r.Store == nil || r.Credentials == nil || strings.TrimSpace(claim.OwnerID) == "" || claim.OwnerID != next.OwnerID || claim.RunID != next.RunID || claim.StageID != next.StageID || !coreexecution.ValidateUUID(claim.RunID) || !coreexecution.ValidateUUID(claim.StageID) {
		return out, ErrExecutionStepResolve
	}
	view, err := r.Store.GetExecutionRun(ctx, claim.OwnerID, claim.RunID)
	if err != nil || view.Run.OwnerID != claim.OwnerID || view.Run.RunID != claim.RunID || view.Run.Status != coreexecution.RunRunning || view.Run.Revision == 0 || !view.Run.RunDigest.Valid() || !view.Run.PlanDigest.Valid() {
		return out, ErrExecutionStepResolve
	}
	var stage coreexecution.RunStage
	for _, s := range view.Stages {
		if s.StageID == claim.StageID {
			stage = s
			break
		}
	}
	if stage.StageID == "" || stage.OwnerID != claim.OwnerID || stage.RunID != claim.RunID || stage.Status != coreexecution.StageRunning || stage.PlanRevision != view.Run.PlanRevision || stage.TargetID == "" || stage.TargetRevision == 0 || !stage.TargetDigest.Valid() {
		return out, ErrExecutionStepResolve
	}
	plan, err := r.Store.GetPlanRevision(ctx, claim.OwnerID, view.Run.PlanID, view.Run.PlanRevision)
	if err != nil || plan.OwnerID != claim.OwnerID || plan.ID != view.Run.PlanID || plan.Revision != stage.PlanRevision || plan.Digest != view.Run.PlanDigest || !plan.Digest.Valid() {
		return out, ErrExecutionStepResolve
	}
	var planStage *coreexecution.ExecutionStage
	for i := range plan.Stages {
		if plan.Stages[i].StageKey == stage.StageKey {
			planStage = &plan.Stages[i]
			break
		}
	}
	if planStage == nil || planStage.Revision != stage.StageRevision || planStage.Digest != stage.StageDigest {
		return out, ErrExecutionStepResolve
	}
	var step *coreexecution.ExecutionStep
	if next.StepSet == coreexecution.StepSetForward {
		for i := range planStage.Steps {
			if planStage.Steps[i].StepKey == next.StepKey {
				step = &planStage.Steps[i]
				break
			}
		}
	} else if next.StepSet == coreexecution.StepSetRollback {
		for i := range planStage.RollbackSteps {
			if planStage.RollbackSteps[i].StepKey == next.StepKey {
				step = &planStage.RollbackSteps[i]
				break
			}
		}
	} else {
		return out, ErrExecutionStepResolve
	}
	if step == nil || next.StepRevision == 0 || step.Digest != next.StepDigest || step.TargetID != stage.TargetID || step.TargetRevision != stage.TargetRevision || step.TargetDigest != stage.TargetDigest {
		return out, ErrExecutionStepResolve
	}
	if step.Kind == coreexecution.StepComputeProvision {
		return r.resolveEC2Provision(ctx, claim, next, view.Run, plan, stage, *planStage, *step)
	}
	if step.Kind == coreexecution.StepSecretProvision {
		return r.resolveSecretProvision(ctx, claim, next, view.Run, plan, stage, *planStage, *step)
	}
	if !coreaws.IsExecutableSSMStep(*step) || step.ObservationRef == nil {
		return out, ErrExecutionStepResolve
	}
	// A reclaimed stage is poll-only. Resolve the already persisted immutable
	// snapshot, attempt and receipt instead of allocating a successor attempt
	// (which could turn a restart into a second SendCommand).
	var receiptID, attemptID, commandID string
	var fence coreexecution.Digest
	var frozenRaw []byte
	err = r.Store.db.QueryRowContext(ctx, `SELECT r.receipt_id::text,r.attempt_id::text,r.fence_digest,COALESCE(r.command_id,''),i.snapshot_json->'frozen_request_snapshot' FROM core_execution_receipts r JOIN core_execution_dispatch_intents i ON i.owner_id=r.owner_id AND i.receipt_id=r.receipt_id AND i.attempt_id=r.attempt_id JOIN core_execution_step_attempts a ON a.owner_id=r.owner_id AND a.attempt_id=r.attempt_id WHERE r.owner_id=$1 AND r.run_id=$2 AND a.stage_id=$3 AND a.step_key=$4 AND a.step_set=$5 AND r.status IN ('accepted','running') AND i.snapshot_json ? 'frozen_request_snapshot' ORDER BY r.created_at DESC LIMIT 1`, claim.OwnerID, claim.RunID, claim.StageID, next.StepKey, next.StepSet).Scan(&receiptID, &attemptID, &fence, &commandID, &frozenRaw)
	if err == nil {
		if commandID != "" {
			dispatch, resolveErr := NewDatabaseDispatchReceiptResolver(r.Store, r.Credentials).ResolveDispatchReceipt(ctx, claim.OwnerID, fence)
			if resolveErr != nil || dispatch.Frozen.RunID != claim.RunID || dispatch.Frozen.StageID != claim.StageID || dispatch.Frozen.StepKey != next.StepKey || dispatch.Frozen.StepRevision != next.StepRevision || dispatch.Frozen.StepDigest != next.StepDigest || dispatch.CommandID == "" {
				return out, ErrExecutionStepResolve
			}
			return executionrunner.PreparedStep{Frozen: dispatch.Frozen, Attempt: coreexecution.StepAttempt{AttemptID: attemptID, OwnerID: claim.OwnerID, RunID: claim.RunID, StageID: claim.StageID, PlanID: dispatch.Frozen.PlanID, PlanRevision: dispatch.Frozen.PlanRevision, PlanDigest: dispatch.Frozen.PlanDigest, StageRevision: dispatch.Frozen.StageRevision, StageDigest: dispatch.Frozen.StageDigest, StepKey: dispatch.Frozen.StepKey, StepRevision: dispatch.Frozen.StepRevision, StepDigest: dispatch.Frozen.StepDigest, Attempt: dispatch.Frozen.Attempt, Revision: 1, Status: coreexecution.AttemptRunning}, Receipt: coreexecution.Receipt{ReceiptID: receiptID, OwnerID: claim.OwnerID, RunID: claim.RunID, AttemptID: attemptID, Status: coreexecution.ReceiptAccepted, SSMCommandID: dispatch.CommandID, RequestDigest: dispatch.RequestDigest, FenceDigest: dispatch.FenceDigest}}, nil
		}
		var snapshot coreaws.FrozenRequestSnapshot
		if strictJSON(frozenRaw, &snapshot) != nil {
			return out, ErrExecutionStepResolve
		}
		frozen := frozenRequestFromSnapshot(snapshot)
		computedFence, fenceErr := coreaws.CanonicalFenceDigest(frozen)
		computedRequest, requestErr := coreaws.CanonicalRequestDigest(frozen)
		if frozen.OwnerID != claim.OwnerID || frozen.RunID != claim.RunID || frozen.StageID != claim.StageID || frozen.StepKey != next.StepKey || frozen.StepRevision != next.StepRevision || frozen.StepDigest != next.StepDigest || frozen.AttemptID != attemptID || frozen.FenceDigest != fence || !frozen.RequestDigest.Valid() || fenceErr != nil || computedFence != fence || requestErr != nil || computedRequest != frozen.RequestDigest {
			return out, ErrExecutionStepResolve
		}
		return executionrunner.PreparedStep{Frozen: frozen, Attempt: coreexecution.StepAttempt{AttemptID: attemptID, OwnerID: claim.OwnerID, RunID: claim.RunID, StageID: claim.StageID, PlanID: frozen.PlanID, PlanRevision: frozen.PlanRevision, PlanDigest: frozen.PlanDigest, StageRevision: frozen.StageRevision, StageDigest: frozen.StageDigest, StepKey: frozen.StepKey, StepRevision: frozen.StepRevision, StepDigest: frozen.StepDigest, Attempt: frozen.Attempt, Revision: 1, Status: coreexecution.AttemptRunning}, Receipt: coreexecution.Receipt{ReceiptID: receiptID, OwnerID: claim.OwnerID, RunID: claim.RunID, AttemptID: attemptID, Status: coreexecution.ReceiptAccepted, RequestDigest: frozen.RequestDigest, FenceDigest: frozen.FenceDigest}, ReconcileOnly: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return out, ErrExecutionStepResolve
	}
	obsRef := step.ObservationRef
	obs, err := r.Store.GetTargetObservation(ctx, claim.OwnerID, obsRef.ObservationID)
	if err != nil || !executableObservation(obs, claim.OwnerID, *obsRef, stage) {
		return out, ErrExecutionStepResolve
	}
	var target coreexecution.ExecutionTarget
	for _, t := range plan.Targets {
		if t.ID == stage.TargetID && t.Revision == stage.TargetRevision {
			target = t
			break
		}
	}
	if target.ID == "" || target.Digest != stage.TargetDigest || target.Validate() != nil {
		return out, ErrExecutionStepResolve
	}
	if len(target.CredentialRefs) == 0 {
		return out, ErrExecutionStepResolve
	}
	ref := target.CredentialRefs[0]
	cred, err := r.Credentials.ResolveCredentialRevision(ctx, claim.OwnerID, ref.Ref, ref.Revision)
	if err != nil || cred.ID != ref.Ref || cred.Revision != int64(ref.Revision) || cred.VerifiedRevision != int64(ref.Revision) || cred.AccountID != target.AccountID || cred.Region != target.Region {
		return out, ErrExecutionStepResolve
	}
	bound, err := coreaws.CredentialBindingDigest(claim.OwnerID, ref, cred)
	if err != nil || bound != ref.BindingDigest {
		return out, ErrExecutionStepResolve
	}
	instanceID := obs.Observation.Facts["instance_id"]
	if instanceID == "" || obs.Observation.Facts["account_id"] != target.AccountID || obs.Observation.Facts["region"] != target.Region {
		return out, ErrExecutionStepResolve
	}
	ss := *step
	frozenScript, err := coreaws.FrozenScriptForStep(ss)
	if err != nil {
		return out, ErrExecutionStepResolve
	}
	frozen := coreaws.FrozenRequest{OwnerID: claim.OwnerID, PlanID: plan.ID, PlanRevision: plan.Revision, PlanDigest: plan.Digest, RunID: view.Run.RunID, RunRevision: stage.RunRevision, RunDigest: view.Run.RunDigest, StageID: stage.StageID, StageRevision: stage.StageRevision, StageDigest: stage.StageDigest, StepKey: step.StepKey, StepRevision: next.StepRevision, StepDigest: step.Digest, AttemptID: uuid.NewString(), Attempt: 1, Fence: fmt.Sprintf("%s:%d:%s", claim.LeaseID, claim.LeaseEpoch, step.StepKey), Target: target, TargetID: target.ID, TargetRevision: target.Revision, TargetDigest: target.Digest, InstanceID: instanceID, Credential: cred, CredentialID: cred.ID, CredentialRevision: ref.Revision, Observation: obs.Observation, Script: frozenScript}
	frozen.FenceDigest, err = coreaws.CanonicalFenceDigest(frozen)
	if err != nil {
		return out, ErrExecutionStepResolve
	}
	frozen.RequestDigest, err = coreaws.CanonicalRequestDigest(frozen)
	if err != nil {
		return out, ErrExecutionStepResolve
	}
	attempt := coreexecution.StepAttempt{AttemptID: frozen.AttemptID, RunID: frozen.RunID, StageID: frozen.StageID, PlanID: frozen.PlanID, PlanRevision: frozen.PlanRevision, PlanDigest: frozen.PlanDigest, StageRevision: frozen.StageRevision, StageDigest: frozen.StageDigest, StepRevision: frozen.StepRevision, StepDigest: frozen.StepDigest, StepKey: frozen.StepKey, Attempt: frozen.Attempt, OwnerID: frozen.OwnerID, Revision: 1, Status: coreexecution.AttemptRunning}
	receipt := executionrunner.NewReceipt(claim.OwnerID, claim.RunID, frozen.AttemptID, string(frozen.RequestDigest))
	receipt.RequestDigest = frozen.RequestDigest
	receipt.FenceDigest = frozen.FenceDigest
	out = executionrunner.PreparedStep{Frozen: frozen, Attempt: attempt, Receipt: receipt}
	return out, nil
}

func frozenRequestFromSnapshot(s coreaws.FrozenRequestSnapshot) coreaws.FrozenRequest {
	return coreaws.FrozenRequest{
		OwnerID: s.OwnerID, PlanID: s.PlanID, PlanRevision: s.PlanRevision, PlanDigest: s.PlanDigest,
		RunID: s.RunID, RunRevision: s.RunRevision, RunDigest: s.RunDigest,
		StageID: s.StageID, StageRevision: s.StageRevision, StageDigest: s.StageDigest,
		StepKey: s.StepKey, StepRevision: s.StepRevision, StepDigest: s.StepDigest,
		AttemptID: s.AttemptID, Attempt: s.Attempt, Fence: s.Fence,
		FenceDigest: s.FenceDigest, RequestDigest: s.RequestDigest,
		Target: s.Target, TargetID: s.TargetID, TargetRevision: s.TargetRevision, TargetDigest: s.TargetDigest,
		InstanceID:   s.InstanceID,
		Credential:   coreaws.RehydrateCredentialMetadata(s.CredentialID, "frozen", s.CredentialRegion, s.CredentialAccountID, s.CredentialUserARN, int64(s.CredentialRevision), int64(s.CredentialRevision), time.Time{}, time.Time{}),
		CredentialID: s.CredentialID, CredentialRevision: s.CredentialRevision,
		Observation: s.Observation, Script: s.Script,
	}
}

func (r *ExecutionStepResolver) resolveSecretProvision(
	ctx context.Context,
	claim executionrunner.StageLease,
	next executionrunner.NextStep,
	run coreexecution.ExecutionRun,
	plan coreexecution.ExecutionPlan,
	stage coreexecution.RunStage,
	planStage coreexecution.ExecutionStage,
	step coreexecution.ExecutionStep,
) (executionrunner.PreparedStep, error) {
	var out executionrunner.PreparedStep
	if next.StepSet != coreexecution.StepSetForward || planStage.Risk != coreexecution.RiskR2 || planStage.Gate != coreexecution.GateSecretAccess || step.SecretProvision == nil || step.SecretProvision.Delivery != coreaws.SecretParameterDeliveryTargetSecure || len(step.SecretRefs) != 1 || step.ObservationRef != nil || step.Executor != nil || len(step.ArtifactRefs) != 0 || len(step.NetworkGrants) != 0 {
		return out, ErrExecutionStepResolve
	}
	var target coreexecution.ExecutionTarget
	for _, candidate := range plan.Targets {
		if candidate.ID == stage.TargetID && candidate.Revision == stage.TargetRevision {
			target = candidate
			break
		}
	}
	if target.ID == "" || target.Kind != coreexecution.TargetKindAWSEC2Instance || target.Provider != "aws" || target.Digest != stage.TargetDigest || target.Validate() != nil || len(target.CredentialRefs) != 1 {
		return out, ErrExecutionStepResolve
	}
	awsRef := target.CredentialRefs[0]
	credential, err := r.Credentials.ResolveCredentialRevision(ctx, claim.OwnerID, awsRef.Ref, awsRef.Revision)
	if err != nil || credential.ID != awsRef.Ref || credential.Revision != int64(awsRef.Revision) || credential.VerifiedRevision != int64(awsRef.Revision) || credential.AccountID != target.AccountID || credential.Region != target.Region {
		return out, ErrExecutionStepResolve
	}
	bound, err := coreaws.CredentialBindingDigest(claim.OwnerID, awsRef, credential)
	if err != nil || bound != awsRef.BindingDigest {
		return out, ErrExecutionStepResolve
	}
	secretRef := step.SecretRefs[0]
	if secretRef.Purpose != coreaws.ExecutionSecretPurposeAIProviderAPIKey || secretRef.Ref == "" || secretRef.Revision == 0 || !secretRef.BindingDigest.Valid() {
		return out, ErrExecutionStepResolve
	}
	// A reclaimed stage must materialize the exact persisted request and
	// attempt, then use provider readback only. It must not mint a successor
	// attempt or issue another PutParameter.
	var receiptID, attemptID string
	var requestRaw []byte
	err = r.Store.db.QueryRowContext(ctx, `SELECT r.receipt_id::text,a.attempt_id::text,i.snapshot_json->'secret_parameter_request' FROM core_execution_receipts r JOIN core_execution_dispatch_intents i ON i.owner_id=r.owner_id AND i.receipt_id=r.receipt_id AND i.attempt_id=r.attempt_id JOIN core_execution_step_attempts a ON a.owner_id=r.owner_id AND a.attempt_id=r.attempt_id WHERE r.owner_id=$1 AND r.run_id=$2 AND a.stage_id=$3 AND a.step_key=$4 AND a.step_set='forward' AND r.status IN ('accepted','running') AND i.snapshot_json ? 'secret_parameter_request' ORDER BY r.created_at DESC LIMIT 1`, claim.OwnerID, claim.RunID, claim.StageID, next.StepKey).Scan(&receiptID, &attemptID, &requestRaw)
	if err == nil {
		var req coreaws.SecretParameterProvisionRequest
		if strictJSON(requestRaw, &req) != nil || req.Delivery != step.SecretProvision.Delivery || req.AttemptID != attemptID || req.OwnerID != claim.OwnerID || req.RunID != claim.RunID || req.RunRevision != stage.RunRevision || req.StageID != claim.StageID || req.PlanID != plan.ID || req.PlanRevision != plan.Revision || req.PlanDigest != plan.Digest || req.StageRevision != stage.StageRevision || req.StageDigest != stage.StageDigest || req.StepRevision != next.StepRevision || req.StepDigest != next.StepDigest || req.Target.ID != target.ID || req.Target.Revision != target.Revision || req.Target.Digest != target.Digest || req.Target.Validate() != nil || req.SecretRef != secretRef || req.CredentialID != credential.ID || req.CredentialRevision != uint64(credential.Revision) {
			return out, ErrExecutionStepResolve
		}
		req.Credential = credential
		canonical, digestErr := coreaws.CanonicalSecretParameterRequestDigest(req)
		if digestErr != nil || req.RequestDigest != canonical || !req.FenceDigest.Valid() {
			return out, ErrExecutionStepResolve
		}
		attempt := coreexecution.StepAttempt{AttemptID: attemptID, OwnerID: claim.OwnerID, RunID: claim.RunID, StageID: claim.StageID, PlanID: req.PlanID, PlanRevision: req.PlanRevision, PlanDigest: req.PlanDigest, StageRevision: req.StageRevision, StageDigest: req.StageDigest, StepRevision: req.StepRevision, StepDigest: req.StepDigest, StepKey: next.StepKey, Attempt: 1, Revision: 1, Status: coreexecution.AttemptRunning}
		receipt := coreexecution.Receipt{ReceiptID: receiptID, OwnerID: claim.OwnerID, RunID: claim.RunID, AttemptID: attemptID, Status: coreexecution.ReceiptAccepted, IdempotencyKey: string(req.RequestDigest), RequestDigest: req.RequestDigest, FenceDigest: req.FenceDigest}
		return executionrunner.PreparedStep{SecretProvision: &req, Attempt: attempt, Receipt: receipt, ReconcileOnly: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) || !plan.ExpiresAt.After(r.Store.now().UTC()) {
		return out, ErrExecutionStepResolve
	}
	attemptID = uuid.NewString()
	fence, err := coreexecution.CanonicalDigest(struct {
		OwnerID, PlanID, RunID, StageID, StepKey, AttemptID          string
		PlanRevision, RunRevision, StageRevision, StepRevision       uint64
		PlanDigest, RunDigest, StageDigest, StepDigest, TargetDigest coreexecution.Digest
		LeaseID, LeaseToken                                          string
		LeaseEpoch                                                   uint64
	}{claim.OwnerID, plan.ID, run.RunID, stage.StageID, step.StepKey, attemptID, plan.Revision, stage.RunRevision, stage.StageRevision, next.StepRevision, plan.Digest, run.RunDigest, stage.StageDigest, step.Digest, target.Digest, claim.LeaseID, claim.LeaseToken, claim.LeaseEpoch})
	if err != nil || !fence.Valid() {
		return out, ErrExecutionStepResolve
	}
	req := coreaws.SecretParameterProvisionRequest{OwnerID: claim.OwnerID, PlanID: plan.ID, PlanRevision: plan.Revision, PlanDigest: plan.Digest, RunID: run.RunID, RunRevision: stage.RunRevision, RunDigest: run.RunDigest, StageID: stage.StageID, StageRevision: stage.StageRevision, StageDigest: stage.StageDigest, AttemptID: attemptID, StepRevision: next.StepRevision, StepDigest: step.Digest, Target: target, SecretRef: secretRef, Delivery: step.SecretProvision.Delivery, FenceDigest: fence, Credential: credential, CredentialID: credential.ID, CredentialRevision: awsRef.Revision}
	req.RequestDigest, err = coreaws.CanonicalSecretParameterRequestDigest(req)
	if err != nil || !req.RequestDigest.Valid() {
		return out, ErrExecutionStepResolve
	}
	attempt := coreexecution.StepAttempt{AttemptID: attemptID, OwnerID: claim.OwnerID, RunID: run.RunID, StageID: stage.StageID, PlanID: plan.ID, PlanRevision: plan.Revision, PlanDigest: plan.Digest, StageRevision: stage.StageRevision, StageDigest: stage.StageDigest, StepRevision: next.StepRevision, StepDigest: step.Digest, StepKey: step.StepKey, Attempt: 1, Revision: 1, Status: coreexecution.AttemptRunning}
	receipt := executionrunner.NewReceipt(claim.OwnerID, claim.RunID, attemptID, string(req.RequestDigest))
	receipt.RequestDigest, receipt.FenceDigest = req.RequestDigest, req.FenceDigest
	return executionrunner.PreparedStep{SecretProvision: &req, Attempt: attempt, Receipt: receipt}, nil
}

func (r *ExecutionStepResolver) resolveEC2Provision(
	ctx context.Context,
	claim executionrunner.StageLease,
	next executionrunner.NextStep,
	run coreexecution.ExecutionRun,
	plan coreexecution.ExecutionPlan,
	stage coreexecution.RunStage,
	planStage coreexecution.ExecutionStage,
	step coreexecution.ExecutionStep,
) (executionrunner.PreparedStep, error) {
	var out executionrunner.PreparedStep
	if next.StepSet != coreexecution.StepSetForward || step.ComputeProvision == nil || step.ObservationRef != nil || step.Executor != nil || len(step.NetworkGrants) != 0 || len(step.SecretRefs) != 0 {
		return out, ErrExecutionStepResolve
	}
	var target coreexecution.ExecutionTarget
	for _, candidate := range plan.Targets {
		if candidate.ID == stage.TargetID && candidate.Revision == stage.TargetRevision {
			target = candidate
			break
		}
	}
	if target.ID == "" || target.Kind != coreexecution.TargetKindAWSComputeReservation || target.Revision != 1 || target.Digest != stage.TargetDigest || target.ComputeReservation == nil || target.Validate() != nil || len(target.CredentialRefs) != 1 {
		return out, ErrExecutionStepResolve
	}
	preview, err := coreexecution.BuildConfirmationPreview(plan, run, planStage)
	if err != nil || preview.StageID != stage.StageID || preview.StageRevision != stage.StageRevision || preview.StageDigest != stage.StageDigest || preview.TargetID != target.ID || preview.TargetRevision != target.Revision || preview.TargetDigest != target.Digest || preview.Risk != coreexecution.RiskR2 || preview.Gate != coreexecution.GateResourcePurchase || preview.StepSet != coreexecution.StepSetForward || preview.CostQuoteDigest == "" || preview.PolicyDigest == "" {
		return out, ErrExecutionStepResolve
	}
	quoteDigest, err := coreexecution.CanonicalDigest(target.ComputeReservation.CostQuote)
	if err != nil || quoteDigest != preview.CostQuoteDigest {
		return out, ErrExecutionStepResolve
	}
	ref := target.CredentialRefs[0]
	credential, err := r.Credentials.ResolveCredentialRevision(ctx, claim.OwnerID, ref.Ref, ref.Revision)
	if err != nil || credential.ID != ref.Ref || credential.Revision != int64(ref.Revision) || credential.VerifiedRevision != int64(ref.Revision) || credential.AccountID != target.AccountID || credential.Region != target.Region {
		return out, ErrExecutionStepResolve
	}
	bound, err := coreaws.CredentialBindingDigest(claim.OwnerID, ref, credential)
	if err != nil || bound != ref.BindingDigest {
		return out, ErrExecutionStepResolve
	}
	// A reclaimed compute stage uses the exact persisted redacted request and
	// attempt. The provision executor will find its provider intent and perform
	// readback only; a second CreateStack is structurally unreachable.
	var receiptID, attemptID, providerOperation string
	var fence coreexecution.Digest
	var requestRaw []byte
	err = r.Store.db.QueryRowContext(ctx, `SELECT r.receipt_id::text,r.attempt_id::text,r.fence_digest,r.provider_operation_id,i.snapshot_json->'ec2_provision_request' FROM core_execution_receipts r JOIN core_execution_dispatch_intents i ON i.owner_id=r.owner_id AND i.receipt_id=r.receipt_id AND i.attempt_id=r.attempt_id JOIN core_execution_step_attempts a ON a.owner_id=r.owner_id AND a.attempt_id=r.attempt_id WHERE r.owner_id=$1 AND r.run_id=$2 AND a.stage_id=$3 AND a.step_key=$4 AND a.step_set='forward' AND r.status IN ('accepted','running') AND i.snapshot_json ? 'ec2_provision_request' ORDER BY r.created_at DESC LIMIT 1`, claim.OwnerID, claim.RunID, claim.StageID, next.StepKey).Scan(&receiptID, &attemptID, &fence, &providerOperation, &requestRaw)
	if err == nil {
		var request coreaws.EC2ProvisionRequest
		intent := coreaws.EC2ProvisionIntent{}
		if strictJSON(requestRaw, &request) != nil {
			return out, ErrExecutionStepResolve
		}
		intent = coreaws.EC2ProvisionIntent{OwnerID: request.OwnerID, FenceDigest: request.FenceDigest, RequestDigest: request.RequestDigest, ProviderOperationKey: coreaws.EC2ProvisionOperationKey(request.Target.ID), Request: request}
		if coreaws.ValidateEC2ProvisionIntentSnapshot(intent) != nil || request.OwnerID != claim.OwnerID || request.RunID != claim.RunID || request.StageID != claim.StageID || request.AttemptID != attemptID || request.StepRevision != next.StepRevision || request.Step.StepKey != next.StepKey || request.Step.Digest != next.StepDigest || request.Target.Digest != target.Digest || request.PolicyDigest != preview.PolicyDigest || request.CostQuoteDigest != preview.CostQuoteDigest || fence != request.FenceDigest {
			return out, ErrExecutionStepResolve
		}
		attempt := coreexecution.StepAttempt{AttemptID: attemptID, OwnerID: claim.OwnerID, RunID: claim.RunID, StageID: claim.StageID, PlanID: request.PlanID, PlanRevision: request.PlanRevision, PlanDigest: request.PlanDigest, StageRevision: request.StageRevision, StageDigest: request.StageDigest, StepKey: request.Step.StepKey, StepRevision: request.StepRevision, StepDigest: request.Step.Digest, Attempt: 1, Revision: 1, Status: coreexecution.AttemptRunning}
		receipt := coreexecution.Receipt{ReceiptID: receiptID, OwnerID: claim.OwnerID, RunID: claim.RunID, AttemptID: attemptID, Status: coreexecution.ReceiptAccepted, IdempotencyKey: step.IdempotencyMarker, ProviderOperation: providerOperation, RequestDigest: request.RequestDigest, FenceDigest: request.FenceDigest}
		return executionrunner.PreparedStep{EC2Provision: &request, EC2Credential: credential, Attempt: attempt, Receipt: receipt}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) || !target.ComputeReservation.CostQuote.ExpiresAt.After(r.Store.now().UTC()) || !plan.ExpiresAt.After(r.Store.now().UTC()) {
		return out, ErrExecutionStepResolve
	}
	attemptID = uuid.NewString()
	fence, err = coreexecution.CanonicalDigest(struct {
		OwnerID, PlanID, RunID, StageID, StepKey, AttemptID          string
		PlanRevision, RunRevision, StageRevision, StepRevision       uint64
		PlanDigest, RunDigest, StageDigest, StepDigest, TargetDigest coreexecution.Digest
		LeaseID, LeaseToken                                          string
		LeaseEpoch                                                   uint64
	}{claim.OwnerID, plan.ID, run.RunID, stage.StageID, step.StepKey, attemptID, plan.Revision, stage.RunRevision, stage.StageRevision, next.StepRevision, plan.Digest, run.RunDigest, stage.StageDigest, step.Digest, target.Digest, claim.LeaseID, claim.LeaseToken, claim.LeaseEpoch})
	if err != nil {
		return out, ErrExecutionStepResolve
	}
	request := coreaws.EC2ProvisionRequest{OwnerID: claim.OwnerID, PlanID: plan.ID, PlanRevision: plan.Revision, PlanDigest: plan.Digest, RunID: run.RunID, RunRevision: stage.RunRevision, RunDigest: run.RunDigest, StageID: stage.StageID, StageRevision: stage.StageRevision, StageDigest: stage.StageDigest, AttemptID: attemptID, StepRevision: next.StepRevision, Target: target, Step: step, PolicyDigest: preview.PolicyDigest, CostQuoteDigest: preview.CostQuoteDigest, FenceDigest: fence}
	request.RequestDigest, err = coreaws.CanonicalEC2ProvisionRequestDigest(request)
	intent := coreaws.EC2ProvisionIntent{OwnerID: request.OwnerID, FenceDigest: request.FenceDigest, RequestDigest: request.RequestDigest, ProviderOperationKey: coreaws.EC2ProvisionOperationKey(target.ID), Request: request}
	if err != nil || coreaws.ValidateEC2ProvisionIntentSnapshot(intent) != nil {
		return out, ErrExecutionStepResolve
	}
	attempt := coreexecution.StepAttempt{AttemptID: attemptID, OwnerID: claim.OwnerID, RunID: run.RunID, StageID: stage.StageID, PlanID: plan.ID, PlanRevision: plan.Revision, PlanDigest: plan.Digest, StageRevision: stage.StageRevision, StageDigest: stage.StageDigest, StepKey: step.StepKey, StepRevision: next.StepRevision, StepDigest: step.Digest, Attempt: 1, Revision: 1, Status: coreexecution.AttemptRunning}
	receipt := executionrunner.NewReceipt(claim.OwnerID, claim.RunID, attemptID, string(request.RequestDigest))
	receipt.RequestDigest, receipt.FenceDigest = request.RequestDigest, request.FenceDigest
	return executionrunner.PreparedStep{EC2Provision: &request, EC2Credential: credential, Attempt: attempt, Receipt: receipt}, nil
}

var _ executionrunner.StepResolver = (*ExecutionStepResolver)(nil)

func executableObservation(
	record TargetObservationRecord,
	owner string,
	ref coreexecution.TargetObservationRef,
	stage coreexecution.RunStage,
) bool {
	return record.OwnerID == owner &&
		record.ObservationID == ref.ObservationID &&
		record.Status == "observed" &&
		record.Observation.TargetID == stage.TargetID &&
		record.Observation.TargetRevision == stage.TargetRevision &&
		record.Observation.Digest == ref.ObservationDigest &&
		record.Observation.State == "ready" &&
		!record.Observation.Partial &&
		!record.Observation.Stale
}
