package aws

import (
	"context"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

func (s *Service) GetChange(ctx context.Context, id string) (Change, error) {
	if s == nil || s.repo == nil || !validUUID(id) {
		return Change{}, ErrInvalid
	}
	return s.repo.GetChange(ctx, id)
}
func (s *Service) ListChanges(ctx context.Context, size int, planID, token string) (ChangePage, error) {
	if s == nil || s.repo == nil {
		return ChangePage{}, ErrInvalid
	}
	return s.repo.ListChanges(ctx, size, planID, token)
}

type RequestChangeInput struct {
	PlanID, ProvisionID, IdempotencyKey string
	ExpectedProvisionRevision           int64
	Binding                             coreconfirmation.Binding
}
type ChangeRequestResult struct {
	Change       Change
	Task         Task
	Confirmation coreconfirmation.Confirmation
	// Provision is the exact post-request snapshot committed with the change.
	// It is replayed verbatim for the same idempotency key.
	Provision Provision
}

func (s *Service) RequestChange(ctx context.Context, in RequestChangeInput) (ChangeRequestResult, error) {
	if s == nil || s.coordinator == nil || !validUUID(in.PlanID) || !validUUID(in.IdempotencyKey) || (in.ProvisionID != "" && !validUUID(in.ProvisionID)) {
		return ChangeRequestResult{}, ErrInvalid
	}
	if in.ProvisionID != "" && in.ExpectedProvisionRevision == 0 {
		if p, err := s.repo.GetProvision(ctx, in.ProvisionID); err == nil {
			in.ExpectedProvisionRevision = p.Revision
		}
	}
	if in.ProvisionID != "" && in.ExpectedProvisionRevision < 1 {
		return ChangeRequestResult{}, ErrInvalid
	}
	return s.coordinator.RequestChange(ctx, in)
}

// RetryProvision is the explicit terminal retry transition for deterministic
// plans. Callers must supply the observed provision revision and a UUID
// idempotency key; the durable row remains the same so prior changes/events
// stay auditable.
func (s *Service) RetryProvision(ctx context.Context, provisionID string, expectedRevision int64, idempotencyKey string) (Provision, error) {
	if s == nil || s.repo == nil || !validUUID(provisionID) || !validUUID(idempotencyKey) || expectedRevision < 1 {
		return Provision{}, ErrInvalid
	}
	return s.repo.RetryProvision(ctx, provisionID, expectedRevision, idempotencyKey)
}

func (s *Service) ConsumeChange(ctx context.Context, cmd ConsumeChangeCommand) (Reservation, error) {
	if s == nil || s.coordinator == nil || !validUUID(cmd.ChangeID) || !validUUID(cmd.ConfirmationID) || !validUUID(cmd.TaskID) || !validUUID(cmd.IdempotencyKey) || cmd.Attempt == 0 || cmd.LeaseEpoch == 0 {
		return Reservation{}, ErrInvalid
	}
	return s.coordinator.ConsumeChange(ctx, cmd)
}

// ExecutionFence returns the current durable change/task/confirmation fence
// for the in-process worker. It exposes no credential material and exists so
// the generic task lease can be consumed without a transport adapter.
func (s *Service) ExecutionFence(ctx context.Context, confirmationID string) (ExecutionFence, error) {
	if s == nil || s.coordinator == nil || !validUUID(confirmationID) {
		return ExecutionFence{}, ErrInvalid
	}
	return s.coordinator.ExecutionFence(ctx, confirmationID)
}

func (s *Service) CompleteChange(ctx context.Context, cmd CompleteChangeCommand) (Change, error) {
	if s == nil || s.coordinator == nil || !validUUID(cmd.ChangeID) || !validUUID(cmd.ConfirmationID) || cmd.ExpectedChangeRevision < 1 || cmd.Status != ChangeSucceeded && cmd.Status != ChangeFailed && cmd.Status != ChangeCanceled {
		return Change{}, ErrInvalid
	}
	if cmd.OperationKey == "" {
		fence, err := s.coordinator.ExecutionFence(ctx, cmd.ConfirmationID)
		if err != nil {
			return Change{}, err
		}
		cmd.OperationKey = operationKey(cmd.ChangeID, fence.Change.ProviderToken, "complete:"+string(cmd.Status), cmd.Attempt, cmd.LeaseEpoch)
	}
	return s.coordinator.CompleteChange(ctx, cmd)
}
func (s *Service) repoChangeByConfirmation(ctx context.Context, id string) (Change, error) {
	return s.repo.GetChangeByConfirmation(ctx, id)
}
