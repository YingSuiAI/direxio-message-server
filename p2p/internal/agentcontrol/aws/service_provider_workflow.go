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
	// Historical plans are executable only when their pinned credential
	// revision has a verified caller identity. Check this before any provider
	// read as well as again immediately before each provider mutation below.
	if !credentialReadyForPlan(cred) {
		return Change{}, ErrConflict
	}
	if !conf.Binding.Equal(bindingForPlanOwner(p, cred, conf.OwnerID)) {
		return Change{}, ErrRevisionConflict
	}
	if c.Stage == StageReconciling {
		if c.ChangeSetID == "" && c.Operation != OperationDelete {
			changeSetName := providerChangeSetName(c.ProviderToken)
			cs, de := s.provider.DescribeChangeSet(ctx, cred.handle(), p.Region, p.StackName, changeSetName)
			if de == ErrNotFound && validChangeSetName(c.ProviderToken) {
				// Pre-upgrade changes used the raw UUID as their ChangeSetName.
				// Recover those records read-only when the canonical name is absent;
				// digit-leading UUIDs never become invalid legacy lookups.
				cs, de = s.provider.DescribeChangeSet(ctx, cred.handle(), p.Region, p.StackName, c.ProviderToken)
			}
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
		deleteReference := p.StackName
		if isTypedEC2Plan(p) {
			required, valid := requiredStackOutputs(p)
			if !valid || len(required) == 0 {
				return c, ErrResponseUncertain
			}
			// A destroy is destructive even though the provider call only carries a
			// stack name. Re-read the authoritative stack first and refuse to act on
			// a same-name replacement or any immutable-plan drift. The ARN is then
			// used as the DeleteStack reference so CloudFormation cannot resolve a
			// different stack by name between read and delete.
			expectedStackID := ""
			if provision, provisionErr := s.repo.GetProvisionByChange(ctx, c.ID); provisionErr == nil {
				expectedStackID = provision.Readback.StackID
			}
			if expectedStackID == "" {
				return c, ErrResponseUncertain
			}
			current, readErr := s.provider.DescribeStack(ctx, cred.handle(), p.Region, p.StackName)
			if readErr != nil || !destroyReadbackMatches(current, p, required, expectedStackID) {
				return c, ErrResponseUncertain
			}
			deleteReference = current.Outputs[string(StackOutputStackID)]
		}
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
		if err = s.requireCredentialReady(ctx, p.CredentialID, p.CredentialRevision); err != nil {
			return Change{}, err
		}
		err = s.provider.DeleteStack(ctx, cred.handle(), p.Region, deleteReference, c.ProviderToken)
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
			completed, ce := s.completeExecution(ctx, c, Change{Status: ChangeFailed, ErrorCode: "provider_error", ErrorSummary: "AWS delete failed"}, nil)
			if ce != nil {
				return Change{}, ce
			}
			return completed, err
		}
		return s.reconcileChange(ctx, c, p)
	}
	if c.Stage == StageChangeSetCreating {
		req := ChangeSetRequest{Region: p.Region, StackName: p.StackName, ChangeSetName: providerChangeSetName(c.ProviderToken), ClientToken: c.ProviderToken, Operation: c.Operation, Template: p.Template, Parameters: p.Parameters, Tags: p.Tags, Capabilities: p.Capabilities}
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
		if err = s.requireCredentialReady(ctx, p.CredentialID, p.CredentialRevision); err != nil {
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
			completed, ce := s.completeExecution(ctx, c, Change{Status: ChangeFailed, ErrorCode: "provider_error", ErrorSummary: "AWS change-set creation failed"}, nil)
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
		if err = s.requireCredentialReady(ctx, p.CredentialID, p.CredentialRevision); err != nil {
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
			completed, ce := s.completeExecution(ctx, c, Change{Status: ChangeFailed, ErrorCode: "provider_error", ErrorSummary: "AWS change-set execution failed"}, nil)
			if ce != nil {
				return Change{}, ce
			}
			return completed, err
		}
	}
	return s.reconcileChange(ctx, c, p)
}

func destroyReadbackMatches(stack Stack, plan Plan, required []string, expectedStackID string) bool {
	if stack.Region != plan.Region || stack.StackName != plan.StackName || stack.TemplateSHA256 != plan.TemplateSHA256 || !PlanParametersMatchReadback(plan, stack.Parameters) || canonicalDigest(stack.Tags) != canonicalDigest(plan.Tags) || !stack.Outputs.HasAll(required...) {
		return false
	}
	if stack.Status != "CREATE_COMPLETE" && stack.Status != "UPDATE_COMPLETE" {
		return false
	}
	stackID := stack.Outputs[string(StackOutputStackID)]
	if stackID == "" || (plan.Tags["service"] == EC2ServiceProfile && plan.Tags["dirextalk:template-profile"] == EC2ServiceProfile && plan.Tags["dirextalk:template-version"] == ec2TemplateVersion && plan.Tags["dirextalk:request-digest"] == "") {
		return false
	}
	if expectedStackID != "" && stackID != expectedStackID {
		return false
	}
	if plan.Tags["service"] == EC2ServiceProfile && plan.Tags["dirextalk:template-profile"] == EC2ServiceProfile && plan.Tags["dirextalk:template-version"] == ec2TemplateVersion && plan.Tags["dirextalk:template-digest"] != plan.TemplateSHA256 {
		return false
	}
	return true
}

func isTypedEC2Plan(plan Plan) bool {
	return plan.Tags["service"] == EC2ServiceProfile &&
		plan.Tags["dirextalk:template-profile"] == EC2ServiceProfile &&
		plan.Tags["dirextalk:template-version"] == ec2TemplateVersion
}

func (s *Service) completeExecution(ctx context.Context, previous, terminal Change, readback *ProvisionReadback) (Change, error) {
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
	return s.coordinator.CompleteChange(ctx, CompleteChangeCommand{ChangeID: previous.ID, ConfirmationID: previous.ConfirmationID, TaskID: fence.Task.ID, Attempt: fence.Task.Attempt, LeaseEpoch: fence.Task.LeaseEpoch, ExpectedTaskRevision: fence.Task.Revision, ExpectedChangeRevision: previous.Revision, ExpectedConfirmationRevision: fence.Confirmation.Revision, Status: terminal.Status, ErrorCode: terminal.ErrorCode, ErrorSummary: terminal.ErrorSummary, OperationKey: operationKey(previous.ID, previous.ProviderToken, "complete:"+string(terminal.Status), fence.Task.Attempt, fence.Task.LeaseEpoch), Readback: readback})
}

// reconcileChange derives typed output requirements from the durable plan tag.
// Generic plans have no marker and retain the historical completion behavior;
// typed provisions remain uncertain until readback proves every requested
// allowlisted value.
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
		return s.completeExecution(ctx, c, n, nil)
	}
	if e != nil {
		return c, ErrResponseUncertain
	}
	requiredOutputs, validRequirements := requiredStackOutputs(p)
	if !validRequirements || (isTypedEC2Plan(p) && len(requiredOutputs) == 0) {
		return c, ErrResponseUncertain
	}
	want := map[Operation]string{OperationCreate: "CREATE_COMPLETE", OperationUpdate: "UPDATE_COMPLETE", OperationDelete: "DELETE_COMPLETE"}[c.Operation]
	if c.Operation == OperationDelete && e == ErrNotFound {
		want = ""
	}
	if (c.Operation == OperationDelete && e == ErrNotFound) || (want != "" && stack.Region == p.Region && stack.StackName == p.StackName && stack.Status == want && stack.TemplateSHA256 != "" && stack.TemplateSHA256 == p.TemplateSHA256 && PlanParametersMatchReadback(p, stack.Parameters) && canonicalDigest(stack.Tags) == canonicalDigest(p.Tags) && stack.Outputs.HasAll(requiredOutputs...)) {
		n := c
		n.Status = ChangeSucceeded
		n.Stage = StageSucceeded
		n.Revision++
		n.UpdatedAt = s.now().UTC()
		var readback *ProvisionReadback
		if _, provisionErr := s.repo.GetProvisionByChange(ctx, c.ID); provisionErr == nil && c.Operation == OperationCreate {
			value, readbackErr := ProvisionReadbackFromStack(stack.Outputs, s.now())
			if readbackErr != nil {
				return c, ErrResponseUncertain
			}
			readback = &value
		} else if provisionErr != nil && provisionErr != ErrNotFound {
			return c, ErrResponseUncertain
		}
		return s.completeExecution(ctx, c, n, readback)
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
