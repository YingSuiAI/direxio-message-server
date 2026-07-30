package aws

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDestroyReadbackRejectsSameNameReplacementOrImmutableDrift(t *testing.T) {
	built, err := BuildEC2ProvisionPlan(validEC2ProvisionRequest())
	if err != nil {
		t.Fatal(err)
	}
	plan := built.CorePlan()
	required, ok := requiredStackOutputs(plan)
	if !ok {
		t.Fatal("typed plan did not declare required outputs")
	}
	stackID := "arn:aws:cloudformation:us-east-1:123456789012:stack/geolibre-prod/01234567-89ab-cdef-0123-456789abcdef"
	stack := Stack{
		Region: plan.Region, StackName: plan.StackName, Status: "CREATE_COMPLETE", TemplateSHA256: plan.TemplateSHA256,
		Parameters: cloneMap(plan.Parameters), Tags: cloneMap(plan.Tags), Outputs: StackOutputs{
			string(StackOutputStackID): stackID, string(StackOutputInstanceID): "i-0123456789abcdef0",
			string(StackOutputPublicIP): "192.0.2.10", string(StackOutputSecurityGroup): "sg-0123456789abcdef0",
		},
	}
	stack.Parameters["LatestAmiId"] = "ami-0123456789abcdef0"
	if !destroyReadbackMatches(stack, plan, required, stackID) {
		t.Fatal("matching immutable stack readback rejected")
	}
	for name, mutate := range map[string]func(*Stack){
		"replacement stack": func(s *Stack) {
			s.Outputs[string(StackOutputStackID)] = "arn:aws:cloudformation:us-east-1:123456789012:stack/geolibre-prod/11234567-89ab-cdef-0123-456789abcdef"
		},
		"owner tag drift": func(s *Stack) { s.Tags["owner"] = "sha256:" + strings.Repeat("f", 64) },
		"parameter drift": func(s *Stack) { s.Parameters["DisplayName"] = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := stack
			candidate.Outputs = stack.Outputs.Clone()
			candidate.Parameters = cloneMap(stack.Parameters)
			candidate.Tags = cloneMap(stack.Tags)
			mutate(&candidate)
			if destroyReadbackMatches(candidate, plan, required, stackID) {
				t.Fatal("drifted stack readback accepted")
			}
		})
	}
}

func typedDestroyServiceFixture(t *testing.T, mutatePlan func(*Plan), mutateStack func(*Stack)) (*Service, *MemoryRepository, *FakeProvider, ChangeRequestResult) {
	t.Helper()
	repo := NewMemoryRepository()
	s := NewService(repo, &testConfirm{}, testTasks{}, nil, NewFakeProvider(), nil)
	credentialView, err := s.SaveCredential(context.Background(), CredentialInput{Name: "typed", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b", IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatalf("credential: %v", err)
	}
	if _, err = repo.RecordCredentialIdentity(context.Background(), credentialView.ID, credentialView.Revision, Identity{AccountID: "123456789012", UserARN: "arn:aws:iam::123456789012:user/test"}); err != nil {
		t.Fatalf("credential identity: %v", err)
	}
	credential, err := repo.GetCredential(context.Background(), credentialView.ID)
	if err != nil {
		t.Fatalf("credential reload: %v", err)
	}
	provider := s.provider.(*FakeProvider)
	req := validEC2ProvisionRequest()
	req.CredentialID, req.CredentialRevision = credential.ID, credential.Revision
	built, err := BuildEC2ProvisionPlan(req)
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	createPlan := built.CorePlan()
	provisionID := uuid.NewString()
	stackID := "arn:aws:cloudformation:us-east-1:123456789012:stack/geolibre-prod/01234567-89ab-cdef-0123-456789abcdef"
	readback, err := ProvisionReadbackFromStack(StackOutputs{
		string(StackOutputStackID): stackID, string(StackOutputInstanceID): "i-0123456789abcdef0", string(StackOutputPublicIP): "192.0.2.10", string(StackOutputSecurityGroup): "sg-0123456789abcdef0",
	}, time.Now().UTC())
	if err != nil {
		t.Fatalf("readback: %v", err)
	}
	now := time.Now().UTC()
	provision := Provision{ID: provisionID, PlanID: createPlan.ID, CredentialID: createPlan.CredentialID, Region: createPlan.Region, StackName: createPlan.StackName, Profile: createPlan.Tags["service"], OwnerDigest: createPlan.Tags["owner"], CredentialRevision: createPlan.CredentialRevision, PlanRevision: createPlan.Revision, TemplateSHA256: createPlan.TemplateSHA256, PlanDigest: PlanDigest(createPlan), State: "active", Revision: 1, Readback: readback, CreatedAt: now, UpdatedAt: now}
	deletePlan := createPlan
	deletePlan.ID = uuid.NewString()
	deletePlan.Operation = OperationDelete
	deletePlan.CreatedAt = now
	if mutatePlan != nil {
		mutatePlan(&deletePlan)
	}
	repo.mu.Lock()
	repo.plans[createPlan.ID] = createPlan
	repo.plans[deletePlan.ID] = deletePlan
	repo.provisions[provision.ID] = provision
	repo.mu.Unlock()
	stack := Stack{Region: deletePlan.Region, StackName: deletePlan.StackName, Status: "CREATE_COMPLETE", TemplateSHA256: deletePlan.TemplateSHA256, Parameters: cloneMap(deletePlan.Parameters), Tags: cloneMap(deletePlan.Tags), Outputs: readbackOutputs(readback)}
	stack.Parameters["LatestAmiId"] = "ami-0123456789abcdef0"
	if mutateStack != nil {
		mutateStack(&stack)
	}
	provider.Stacks[stack.Region+"/"+stack.StackName] = stack
	out, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: deletePlan.ID, ProvisionID: provision.ID, ExpectedProvisionRevision: 1, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	return s, repo, provider, consumeWorkflowChange(t, s, repo, out)
}

func readbackOutputs(readback ProvisionReadback) StackOutputs {
	return StackOutputs{string(StackOutputStackID): readback.StackID, string(StackOutputInstanceID): readback.InstanceID, string(StackOutputPublicIP): readback.PublicIP, string(StackOutputSecurityGroup): readback.SecurityGroupID}
}

func TestExecuteChangeStrictTypedDestroyUsesMatchingAuthoritativeARN(t *testing.T) {
	s, _, provider, out := typedDestroyServiceFixture(t, nil, nil)
	if _, err := s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); err != nil {
		t.Fatalf("typed destroy err=%v", err)
	}
	if len(provider.Calls) == 0 || provider.Calls[len(provider.Calls)-1] != "describe_stack" {
		t.Fatalf("destroy did not reconcile after authoritative delete: %#v", provider.Calls)
	}
	foundDelete := false
	for _, call := range provider.Calls {
		if call == "delete_stack" {
			foundDelete = true
		}
	}
	if !foundDelete {
		t.Fatalf("authoritative delete was not issued: %#v", provider.Calls)
	}
}

func TestExecuteChangeStrictTypedDestroyRejectsReplacementWithoutDelete(t *testing.T) {
	s, _, provider, out := typedDestroyServiceFixture(t, nil, func(stack *Stack) {
		stack.Outputs[string(StackOutputStackID)] = "arn:aws:cloudformation:us-east-1:123456789012:stack/geolibre-prod/11234567-89ab-cdef-0123-456789abcdef"
	})
	if _, err := s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); !errors.Is(err, ErrResponseUncertain) {
		t.Fatalf("replacement destroy err=%v", err)
	}
	for _, call := range provider.Calls {
		if call == "delete_stack" {
			t.Fatalf("replacement stack triggered DeleteStack: %#v", provider.Calls)
		}
	}
}

func TestExecuteChangeStrictTypedDestroyRejectsMalformedRequiredOutputs(t *testing.T) {
	for name, mutate := range map[string]func(*Plan){
		"missing":   func(plan *Plan) { delete(plan.Tags, RequiredOutputsTag) },
		"invalid":   func(plan *Plan) { plan.Tags[RequiredOutputsTag] = "UnknownOutput" },
		"duplicate": func(plan *Plan) { plan.Tags[RequiredOutputsTag] = "InstanceId,InstanceId" },
	} {
		t.Run(name, func(t *testing.T) {
			s, _, provider, out := typedDestroyServiceFixture(t, mutate, nil)
			if _, err := s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); !errors.Is(err, ErrResponseUncertain) {
				t.Fatalf("malformed required outputs destroy err=%v", err)
			}
			for _, call := range provider.Calls {
				if call == "delete_stack" {
					t.Fatalf("malformed required outputs triggered DeleteStack: %#v", provider.Calls)
				}
			}
		})
	}
}

func TestExecuteChangeStrictRejectsUnverifiedConsumedCredentialBeforeProvider(t *testing.T) {
	s, repo, provider, plan := workflowFixture(t)
	requested, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: plan.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	consumed := consumeWorkflowChange(t, s, repo, requested)
	repo.mu.Lock()
	grant := consumed.Confirmation.Binding.SecretGrants[0]
	cred := repo.credentialHistory[grant.ReferenceID][grant.Revision]
	cred.VerifiedRevision = 0
	repo.credentialHistory[cred.ID][cred.Revision] = cred
	repo.mu.Unlock()
	if _, err := s.ExecuteChange(context.Background(), consumed.Confirmation.ConfirmationID); !errors.Is(err, ErrConflict) {
		t.Fatalf("unverified consumed credential error = %v, want conflict", err)
	}
	if len(provider.Calls) != 0 {
		t.Fatalf("provider mutation occurred for unverified credential: %#v", provider.Calls)
	}
}

func TestExecuteChangeStrictRejectsUnverifiedCredentialBeforeProviderReads(t *testing.T) {
	tests := []struct {
		name  string
		build func(*testing.T) (*Service, *MemoryRepository, *FakeProvider, string)
	}{
		{
			name: "reconciling describe-change-set",
			build: func(t *testing.T) (*Service, *MemoryRepository, *FakeProvider, string) {
				s, repo, provider, plan := workflowFixture(t)
				requested, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: plan.ID, IdempotencyKey: uuid.NewString()})
				if err != nil {
					t.Fatal(err)
				}
				consumed := consumeWorkflowChange(t, s, repo, requested)
				repo.mu.Lock()
				change := repo.changes[consumed.Change.ID]
				change.Stage = StageReconciling
				repo.changes[change.ID] = change
				p := repo.plans[change.PlanID]
				cred := repo.credentialHistory[p.CredentialID][p.CredentialRevision]
				cred.VerifiedRevision = 0
				repo.credentialHistory[p.CredentialID][p.CredentialRevision] = cred
				repo.mu.Unlock()
				return s, repo, provider, consumed.Confirmation.ConfirmationID
			},
		},
		{
			name: "typed destroy describe-stack",
			build: func(t *testing.T) (*Service, *MemoryRepository, *FakeProvider, string) {
				s, repo, provider, consumed := typedDestroyServiceFixture(t, nil, nil)
				repo.mu.Lock()
				p := repo.plans[consumed.Change.PlanID]
				cred := repo.credentialHistory[p.CredentialID][p.CredentialRevision]
				cred.VerifiedRevision = 0
				repo.credentialHistory[p.CredentialID][p.CredentialRevision] = cred
				repo.mu.Unlock()
				return s, repo, provider, consumed.Confirmation.ConfirmationID
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, _, provider, confirmationID := tc.build(t)
			if _, err := s.ExecuteChange(context.Background(), confirmationID); !errors.Is(err, ErrConflict) {
				t.Fatalf("unverified credential error = %v, want conflict", err)
			}
			if len(provider.Calls) != 0 {
				t.Fatalf("provider read/mutation occurred before identity gate: %#v", provider.Calls)
			}
		})
	}
}
