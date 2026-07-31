package storage

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/artifactstore"
	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/executionrunner"
)

// FilesystemArtifactResolver enforces the owner/database metadata fence
// before opening the content-addressed object. URI is never consulted.
type FilesystemArtifactResolver struct {
	Store     *DatabaseExecutionStore
	Artifacts *artifactstore.Store
	MaxRead   int64
}

func NewFilesystemArtifactResolver(store *DatabaseExecutionStore, artifacts *artifactstore.Store) *FilesystemArtifactResolver {
	if store == nil || artifacts == nil {
		return nil
	}
	return &FilesystemArtifactResolver{Store: store, Artifacts: artifacts, MaxRead: artifactstore.MaxArtifactSize}
}

func (r *FilesystemArtifactResolver) ResolveArtifact(ctx context.Context, owner string, ref coreexecution.ArtifactRef) ([]byte, error) {
	if r == nil || r.Store == nil || r.Artifacts == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(ref.URI) != "" || ref.Validate() != nil {
		return nil, coreaws.ErrTypedArtifact
	}
	meta, err := r.Store.GetArtifactMetadata(ctx, owner, ref.ID)
	if err != nil || meta.Status != "available" || meta.StorageBackend != "filesystem" || meta.ContentDigest != ref.Digest || meta.SizeBytes != ref.Size || (ref.MediaType != "" && meta.MediaType != ref.MediaType) || meta.StorageRef != "sha256/"+string(ref.Digest)[:2]+"/"+string(ref.Digest) {
		return nil, coreaws.ErrTypedArtifact
	}
	f, opened, err := r.Artifacts.Open(ctx, string(ref.Digest))
	if err != nil {
		return nil, coreaws.ErrTypedArtifact
	}
	defer f.Close()
	if opened.Digest != string(ref.Digest) || opened.Size != ref.Size || opened.StorageRef != meta.StorageRef {
		return nil, coreaws.ErrTypedArtifact
	}
	limit := r.MaxRead
	if limit <= 0 || limit > artifactstore.MaxArtifactSize {
		limit = artifactstore.MaxArtifactSize
	}
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil || int64(len(b)) != ref.Size {
		return nil, coreaws.ErrTypedArtifact
	}
	return b, nil
}

var _ coreaws.ImmutableArtifactResolver = (*FilesystemArtifactResolver)(nil)

// ExecutionStageStoreAdapter is the deliberately small bridge between the
// SQL dispatch store and the execution-stage runner. Keeping conversion here
// lets the runner remain testable while the persistence schema evolves.
type executionServiceOutputHook interface {
	EnsureReceipt(context.Context, string, string, string) error
	RecoverPending(context.Context) error
}

type ExecutionStageStoreAdapter struct {
	Store   *DatabaseExecutionStore
	outputs executionServiceOutputHook
}

func NewExecutionStageStoreAdapter(store *DatabaseExecutionStore) *ExecutionStageStoreAdapter {
	if store == nil {
		return nil
	}
	return &ExecutionStageStoreAdapter{Store: store}
}

func NewExecutionStageStoreAdapterWithOutputs(store *DatabaseExecutionStore, outputs *ExecutionServiceOutputMaterializer) *ExecutionStageStoreAdapter {
	adapter := NewExecutionStageStoreAdapter(store)
	if adapter == nil || outputs == nil {
		return nil
	}
	adapter.outputs = outputs
	return adapter
}

func (a *ExecutionStageStoreAdapter) ClaimNextExecutionStage(ctx context.Context, holder string, ttl time.Duration) (executionrunner.StageLease, error) {
	// Filesystem content may have been published immediately before a process
	// crash. Recover that bounded gap before dispatching another mutation.
	if a.outputs != nil {
		if err := a.outputs.RecoverPending(ctx); err != nil {
			return executionrunner.StageLease{}, err
		}
	}
	c, err := a.Store.ClaimNextExecutionStage(ctx, holder, ttl)
	return executionrunner.StageLease{OwnerID: c.OwnerID, RunID: c.RunID, StageID: c.StageID, TaskID: c.TaskID, Holder: c.Holder, Attempt: c.Attempt, LeaseEpoch: c.LeaseEpoch, TaskLeaseEpoch: c.TaskLeaseEpoch, ExpectedTaskRevision: c.ExpectedTaskRevision, LeaseID: c.LeaseID, LeaseToken: c.LeaseToken, ExpiresAt: c.ExpiresAt}, err
}
func (a *ExecutionStageStoreAdapter) NextExecutableStep(ctx context.Context, owner, runID, stageID string) (executionrunner.NextStep, error) {
	n, err := a.Store.NextExecutableStep(ctx, owner, runID, stageID)
	return executionrunner.NextStep{OwnerID: n.OwnerID, RunID: n.RunID, StageID: n.StageID, StepKey: n.StepKey, StepSet: n.StepSet, StepRevision: n.StepRevision, StepDigest: n.StepDigest}, err
}
func (a *ExecutionStageStoreAdapter) RenewExecutionStageLease(ctx context.Context, c executionrunner.StageLease, ttl time.Duration) (executionrunner.StageLease, error) {
	err := a.Store.RenewExecutionStageLease(ctx, ExecutionStageLeaseClaim{OwnerID: c.OwnerID, RunID: c.RunID, StageID: c.StageID, TaskID: c.TaskID, Holder: c.Holder, Attempt: c.Attempt, LeaseEpoch: c.LeaseEpoch, TaskLeaseEpoch: c.TaskLeaseEpoch, ExpectedTaskRevision: c.ExpectedTaskRevision, LeaseID: c.LeaseID, LeaseToken: c.LeaseToken, ExpiresAt: c.ExpiresAt}, ttl)
	if err != nil {
		return c, err
	}
	c.ExpectedTaskRevision++
	c.ExpiresAt = time.Now().UTC().Add(ttl)
	return c, nil
}
func (a *ExecutionStageStoreAdapter) FailPreDispatch(ctx context.Context, failure executionrunner.PreDispatchFailure) error {
	err := a.Store.FailExecutionStageBeforeDispatch(ctx, ExecutionPreDispatchFailure{
		Claim: ExecutionStageLeaseClaim{
			OwnerID: failure.Claim.OwnerID, RunID: failure.Claim.RunID, StageID: failure.Claim.StageID,
			TaskID: failure.Claim.TaskID, Holder: failure.Claim.Holder, Attempt: failure.Claim.Attempt,
			LeaseEpoch: failure.Claim.LeaseEpoch, TaskLeaseEpoch: failure.Claim.TaskLeaseEpoch,
			ExpectedTaskRevision: failure.Claim.ExpectedTaskRevision, LeaseID: failure.Claim.LeaseID,
			LeaseToken: failure.Claim.LeaseToken, ExpiresAt: failure.Claim.ExpiresAt,
		},
		Step: ExecutionNextStep{
			OwnerID: failure.Step.OwnerID, RunID: failure.Step.RunID, StageID: failure.Step.StageID,
			StepKey: failure.Step.StepKey, StepSet: failure.Step.StepSet,
			StepRevision: failure.Step.StepRevision, StepDigest: failure.Step.StepDigest,
		},
		Code: failure.Code, EvidenceDigest: failure.EvidenceDigest,
	})
	if err != nil {
		return fmt.Errorf("execution stage store: fail before dispatch: %w", err)
	}
	return nil
}
func (a *ExecutionStageStoreAdapter) RecordDispatchIntent(ctx context.Context, in executionrunner.DispatchIntent) error {
	err := a.Store.RecordDispatchIntent(ctx, ExecutionDispatchIntent{Attempt: in.Attempt, Receipt: in.Receipt, TaskID: in.TaskID, TaskHolder: in.TaskHolder, TaskAttempt: in.TaskAttempt, TaskRevision: in.TaskRevision, TaskLeaseEpoch: in.TaskLeaseEpoch, TargetID: in.TargetID, TargetRevision: in.TargetRevision, TargetDigest: in.TargetDigest, LeaseID: in.LeaseID, LeaseToken: in.LeaseToken, LeaseEpoch: in.LeaseEpoch, StepSet: in.StepSet, RequestDigest: in.RequestDigest, FenceDigest: in.FenceDigest, Snapshot: in.Snapshot, EC2Provision: in.EC2Provision, SecretProvision: in.SecretProvision})
	if err != nil {
		return fmt.Errorf("execution stage store: record dispatch intent: %w", err)
	}
	return nil
}
func (a *ExecutionStageStoreAdapter) MarkDispatchUncertain(ctx context.Context, owner, attemptID, receiptID, commandID string, evidence ...coreexecution.Digest) error {
	if err := a.Store.MarkDispatchUncertainWithCommand(ctx, owner, attemptID, receiptID, commandID, evidence...); err != nil {
		return fmt.Errorf("execution stage store: mark dispatch uncertain: %w", err)
	}
	return nil
}
func (a *ExecutionStageStoreAdapter) MarkProviderDispatchUncertain(ctx context.Context, owner, attemptID, receiptID, providerOperationID string, evidence ...coreexecution.Digest) error {
	return a.Store.MarkDispatchUncertainWithProvider(ctx, owner, attemptID, receiptID, providerOperationID, evidence...)
}
func (a *ExecutionStageStoreAdapter) RecordAccepted(ctx context.Context, owner, receiptID, attemptID, commandID string) error {
	if err := a.Store.RecordAccepted(ctx, owner, receiptID, attemptID, commandID); err != nil {
		return fmt.Errorf("execution stage store: record accepted: %w", err)
	}
	return nil
}
func (a *ExecutionStageStoreAdapter) FinalizeDispatchReceipt(ctx context.Context, owner, receiptID, attemptID string, status coreexecution.ReceiptStatus, responseDigest coreexecution.Digest) error {
	if err := a.Store.FinalizeDispatchReceipt(ctx, owner, receiptID, attemptID, status, responseDigest); err != nil {
		return fmt.Errorf("execution stage store: finalize dispatch receipt: %w", err)
	}
	if status == coreexecution.ReceiptSucceeded && a.outputs != nil {
		// Provider terminal evidence is already committed. Output failure must
		// never turn that fact into provider uncertainty; RecoverPending will
		// retry the deterministic CAS+database materialization on the next loop
		// or process restart.
		bounded, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.outputs.EnsureReceipt(bounded, owner, receiptID, attemptID)
	}
	return nil
}

var _ executionrunner.StageStore = (*ExecutionStageStoreAdapter)(nil)
