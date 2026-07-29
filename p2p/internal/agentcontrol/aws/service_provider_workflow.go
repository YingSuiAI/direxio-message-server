package aws

import (
	"context"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

// ExecuteChange only accepts a consumed confirmation. The confirmation ID is
// reused as the provider idempotency token across retries and crash recovery.
func (s *Service) ExecuteChange(ctx context.Context, confirmationID string) (Change, error) {
	return s.executeChangeStrict(ctx, confirmationID)
}

func (s *Service) executeChangeStrict(ctx context.Context, confirmationID string) (Change, error) {
	if s == nil || s.provider == nil || s.coordinator == nil {
		return Change{}, ErrInvalid
	}
	fence, err := s.coordinator.ExecutionFence(ctx, confirmationID)
	if err != nil {
		return Change{}, err
	}
	c, conf := fence.Change, fence.Confirmation
	if c.Status == ChangeSucceeded || c.Status == ChangeFailed || c.Status == ChangeCanceled {
		if fence.Task.Status == "running" && conf.State == coreconfirmation.StateConsumed && fence.Reservation.Active {
			return s.coordinator.CompleteChange(ctx, CompleteChangeCommand{
				ChangeID:                     c.ID,
				ConfirmationID:               c.ConfirmationID,
				TaskID:                       fence.Task.ID,
				Attempt:                      fence.Task.Attempt,
				LeaseEpoch:                   fence.Task.LeaseEpoch,
				ExpectedTaskRevision:         fence.Task.Revision,
				ExpectedChangeRevision:       c.Revision,
				ExpectedConfirmationRevision: conf.Revision,
				Status:                       c.Status,
				ErrorCode:                    c.ErrorCode,
				ErrorSummary:                 c.ErrorSummary,
				OperationKey:                 operationKey(c.ID, c.ProviderToken, "complete:"+string(c.Status), fence.Task.Attempt, fence.Task.LeaseEpoch),
			})
		}
		return c, nil
	}
	if conf.State != coreconfirmation.StateConsumed || !fence.Reservation.Active {
		return Change{}, ErrUnconfirmed
	}
	p, err := s.repo.GetPlan(ctx, c.PlanID)
	if err != nil {
		return Change{}, err
	}
	cred, err := s.repo.GetCredentialRevision(ctx, p.CredentialID, p.CredentialRevision)
	if err != nil {
		return Change{}, err
	}
	if !conf.Binding.Equal(bindingForPlan(p, cred)) {
		return Change{}, ErrRevisionConflict
	}
	if c.Stage == StageReconciling {
		if c.ChangeSetID == "" && c.Operation != OperationDelete {
			cs, de := s.provider.DescribeChangeSet(ctx, cred.handle(), p.Region, p.StackName, c.ProviderToken)
			if de == nil {
				if cs.Region != p.Region || cs.StackName != p.StackName || cs.ClientToken != c.ProviderToken || cs.RequestDigest != c.ProviderRequestDigest || cs.RequestDigest != providerRequestDigest(p, c.ProviderToken) {
					return Change{}, ErrConflict
				}
				fence, fe := s.coordinator.ExecutionFence(ctx, c.ConfirmationID)
				if fe != nil {
					return Change{}, fe
				}
				c, err = s.coordinator.PersistChangeSetEvidence(ctx, ChangeSetEvidenceCommand{
					ChangeID:                     c.ID,
					ConfirmationID:               c.ConfirmationID,
					TaskID:                       fence.Task.ID,
					ProviderChangeSetID:          cs.ID,
					Attempt:                      fence.Task.Attempt,
					LeaseEpoch:                   fence.Task.LeaseEpoch,
					ExpectedChangeRevision:       c.Revision,
					ExpectedTaskRevision:         fence.Task.Revision,
					ExpectedConfirmationRevision: fence.Confirmation.Revision,
				})
				if err != nil {
					return Change{}, err
				}
				// Evidence persistence advances the change revision. Reload the full
				// fence before the next provider claim so a reclaimed lease never
				// forwards a pre-promotion or pre-evidence revision into a CAS.
				fence, err = s.coordinator.ExecutionFence(ctx, c.ConfirmationID)
				if err != nil {
					return Change{}, err
				}
				if fence.Change.ID != c.ID || fence.Change.ChangeSetID != cs.ID || fence.Change.ProviderToken != c.ProviderToken || fence.Change.ProviderRequestDigest != c.ProviderRequestDigest || fence.Task.Status != "running" || !fence.Reservation.Active {
					return Change{}, ErrRevisionConflict
				}
				c = fence.Change
			} else {
				return s.reconcileChange(ctx, c, p)
			}
		} else {
			return s.reconcileChange(ctx, c, p)
		}
	}
	if c.Stage == StageRequested {
		return Change{}, ErrRevisionConflict
	}
	if c.Operation == OperationDelete && c.Stage == StageChangeSetCreating {
		fence, fe := s.coordinator.ExecutionFence(ctx, c.ConfirmationID)
		if fe != nil {
			return Change{}, fe
		}
		claim := ProviderMutationCommand{ChangeID: c.ID, ConfirmationID: c.ConfirmationID, TaskID: fence.Task.ID, Attempt: fence.Task.Attempt, LeaseEpoch: fence.Task.LeaseEpoch, ExpectedChangeRevision: c.Revision, ExpectedTaskRevision: fence.Task.Revision, ExpectedConfirmationRevision: fence.Confirmation.Revision, Kind: ProviderMutationDelete, OperationKey: operationKey(c.ID, c.ProviderToken, string(ProviderMutationDelete), fence.Task.Attempt, fence.Task.LeaseEpoch)}
		claimedFence, claimErr := s.coordinator.ClaimProviderMutation(ctx, claim)
		if claimErr != nil {
			err = claimErr
			return Change{}, err
		}
		err = s.provider.DeleteStack(ctx, cred.handle(), p.Region, p.StackName, c.ProviderToken)
		claim.ExpectedChangeRevision, claim.ExpectedTaskRevision, claim.ExpectedConfirmationRevision = claimedFence.Change.Revision, claimedFence.Task.Revision, claimedFence.Confirmation.Revision
		committed, ce := s.coordinator.CommitProviderMutation(ctx, ProviderMutationResult{Command: claim, Success: err == nil, ResponseUncertain: err == ErrResponseUncertain, ErrorCode: "provider_error", ErrorSummary: "AWS delete failed"})
		if ce != nil {
			return Change{}, ce
		}
		c = committed
		if err != nil {
			if err == ErrResponseUncertain {
				return c, err
			}
			completed, ce := s.completeExecution(ctx, c, Change{Status: ChangeFailed, ErrorCode: "provider_error", ErrorSummary: "AWS delete failed"})
			if ce != nil {
				return Change{}, ce
			}
			return completed, err
		}
		return s.reconcileChange(ctx, c, p)
	}
	if c.Stage == StageChangeSetCreating {
		req := ChangeSetRequest{Region: p.Region, StackName: p.StackName, ChangeSetName: c.ProviderToken, ClientToken: c.ProviderToken, Operation: c.Operation, Template: p.Template, Parameters: p.Parameters, Tags: p.Tags, Capabilities: p.Capabilities}
		fence, fe := s.coordinator.ExecutionFence(ctx, c.ConfirmationID)
		if fe != nil {
			return Change{}, fe
		}
		claim := ProviderMutationCommand{ChangeID: c.ID, ConfirmationID: c.ConfirmationID, TaskID: fence.Task.ID, Attempt: fence.Task.Attempt, LeaseEpoch: fence.Task.LeaseEpoch, ExpectedChangeRevision: c.Revision, ExpectedTaskRevision: fence.Task.Revision, ExpectedConfirmationRevision: fence.Confirmation.Revision, Kind: ProviderMutationCreate, OperationKey: operationKey(c.ID, c.ProviderToken, string(ProviderMutationCreate), fence.Task.Attempt, fence.Task.LeaseEpoch)}
		claimedFence, claimErr := s.coordinator.ClaimProviderMutation(ctx, claim)
		if claimErr != nil {
			err = claimErr
			return Change{}, err
		}
		cs, e := s.provider.CreateChangeSet(ctx, cred.handle(), req)
		if e == nil && (cs.Region != p.Region || cs.StackName != p.StackName || cs.ClientToken != c.ProviderToken || cs.RequestDigest != providerRequestDigest(p, c.ProviderToken) || c.ProviderRequestDigest != providerRequestDigest(p, c.ProviderToken)) {
			e = ErrConflict
		}
		claim.ExpectedChangeRevision, claim.ExpectedTaskRevision, claim.ExpectedConfirmationRevision = claimedFence.Change.Revision, claimedFence.Task.Revision, claimedFence.Confirmation.Revision
		committed, ce := s.coordinator.CommitProviderMutation(ctx, ProviderMutationResult{Command: claim, Success: e == nil, ResponseUncertain: e == ErrResponseUncertain, ProviderChangeSetID: cs.ID, ErrorCode: "provider_error", ErrorSummary: "AWS change-set creation failed"})
		if ce != nil {
			return Change{}, ce
		}
		c = committed
		if e != nil {
			if e == ErrResponseUncertain {
				return c, e
			}
			completed, ce := s.completeExecution(ctx, c, Change{Status: ChangeFailed, ErrorCode: "provider_error", ErrorSummary: "AWS change-set creation failed"})
			if ce != nil {
				return Change{}, ce
			}
			return completed, e
		}
	}
	if c.Stage == StageChangeSetReady {
		fence, fe := s.coordinator.ExecutionFence(ctx, c.ConfirmationID)
		if fe != nil {
			return Change{}, fe
		}
		claim := ProviderMutationCommand{ChangeID: c.ID, ConfirmationID: c.ConfirmationID, TaskID: fence.Task.ID, Attempt: fence.Task.Attempt, LeaseEpoch: fence.Task.LeaseEpoch, ExpectedChangeRevision: c.Revision, ExpectedTaskRevision: fence.Task.Revision, ExpectedConfirmationRevision: fence.Confirmation.Revision, Kind: ProviderMutationExecute, ProviderChangeSetID: c.ChangeSetID, OperationKey: operationKey(c.ID, c.ProviderToken, string(ProviderMutationExecute), fence.Task.Attempt, fence.Task.LeaseEpoch)}
		claimedFence, claimErr := s.coordinator.ClaimProviderMutation(ctx, claim)
		if claimErr != nil {
			err = claimErr
			return Change{}, err
		}
		err = s.provider.ExecuteChangeSet(ctx, cred.handle(), p.Region, p.StackName, c.ChangeSetID, c.ProviderToken)
		claim.ExpectedChangeRevision, claim.ExpectedTaskRevision, claim.ExpectedConfirmationRevision = claimedFence.Change.Revision, claimedFence.Task.Revision, claimedFence.Confirmation.Revision
		committed, ce := s.coordinator.CommitProviderMutation(ctx, ProviderMutationResult{Command: claim, Success: err == nil, ResponseUncertain: err == ErrResponseUncertain, ErrorCode: "provider_error", ErrorSummary: "AWS change-set execution failed"})
		if ce != nil {
			return Change{}, ce
		}
		c = committed
		if err != nil {
			if err == ErrResponseUncertain {
				return c, err
			}
			completed, ce := s.completeExecution(ctx, c, Change{Status: ChangeFailed, ErrorCode: "provider_error", ErrorSummary: "AWS change-set execution failed"})
			if ce != nil {
				return Change{}, ce
			}
			return completed, err
		}
	}
	return s.reconcileChange(ctx, c, p)
}

func (s *Service) completeExecution(ctx context.Context, previous, terminal Change) (Change, error) {
	if s.coordinator == nil {
		return Change{}, ErrConflict
	}
	fence, err := s.coordinator.ExecutionFence(ctx, previous.ConfirmationID)
	if err != nil {
		return Change{}, err
	}
	if fence.Change.ID != previous.ID || fence.Task.Status != "running" || !fence.Reservation.Active {
		return Change{}, ErrRevisionConflict
	}
	return s.coordinator.CompleteChange(ctx, CompleteChangeCommand{ChangeID: previous.ID, ConfirmationID: previous.ConfirmationID, TaskID: fence.Task.ID, Attempt: fence.Task.Attempt, LeaseEpoch: fence.Task.LeaseEpoch, ExpectedTaskRevision: fence.Task.Revision, ExpectedChangeRevision: previous.Revision, ExpectedConfirmationRevision: fence.Confirmation.Revision, Status: terminal.Status, ErrorCode: terminal.ErrorCode, ErrorSummary: terminal.ErrorSummary, OperationKey: operationKey(previous.ID, previous.ProviderToken, "complete:"+string(terminal.Status), fence.Task.Attempt, fence.Task.LeaseEpoch)})
}

func (s *Service) reconcileChange(ctx context.Context, c Change, p Plan) (Change, error) {
	cred, e := s.repo.GetCredentialRevision(ctx, p.CredentialID, p.CredentialRevision)
	if e != nil {
		return c, e
	}
	if cred.Region != p.Region {
		return c, ErrRevisionConflict
	}
	stack, e := s.provider.DescribeStack(ctx, cred.handle(), p.Region, p.StackName)
	if e == ErrNotFound && c.Operation == OperationDelete {
		n := c
		n.Status = ChangeSucceeded
		n.Stage = StageSucceeded
		n.Revision++
		n.UpdatedAt = s.now().UTC()
		return s.completeExecution(ctx, c, n)
	}
	if e != nil {
		return c, ErrResponseUncertain
	}
	want := map[Operation]string{OperationCreate: "CREATE_COMPLETE", OperationUpdate: "UPDATE_COMPLETE", OperationDelete: "DELETE_COMPLETE"}[c.Operation]
	if c.Operation == OperationDelete && e == ErrNotFound {
		want = ""
	}
	if (c.Operation == OperationDelete && e == ErrNotFound) || (want != "" && stack.Region == p.Region && stack.StackName == p.StackName && stack.Status == want && stack.TemplateSHA256 != "" && stack.TemplateSHA256 == p.TemplateSHA256 && canonicalDigest(stack.Parameters) == canonicalDigest(p.Parameters) && canonicalDigest(stack.Tags) == canonicalDigest(p.Tags)) {
		n := c
		n.Status = ChangeSucceeded
		n.Stage = StageSucceeded
		n.Revision++
		n.UpdatedAt = s.now().UTC()
		return s.completeExecution(ctx, c, n)
	}
	return c, ErrResponseUncertain
}

// PollChange reads only the typed stack status port and advances a running
// change; it never issues a mutation.
func (s *Service) PollChange(ctx context.Context, confirmationID string) (Change, error) {
	if s != nil && s.provider != nil {
		c, err := s.repoChangeByConfirmation(ctx, confirmationID)
		if err != nil {
			return Change{}, err
		}
		if c.Status == ChangeSucceeded || c.Status == ChangeFailed || c.Status == ChangeCanceled {
			return c, nil
		}
		p, err := s.repo.GetPlan(ctx, c.PlanID)
		if err != nil {
			return Change{}, err
		}
		return s.reconcileChange(ctx, c, p)
	}
	return Change{}, ErrInvalid
}
