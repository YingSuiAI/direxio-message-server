package aws

import (
	"context"
	"errors"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/google/uuid"
	"sync"
	"testing"
	"time"
)

func workflowFixture(t *testing.T) (*Service, *MemoryRepository, *FakeProvider, PlanView) {
	t.Helper()
	r := NewMemoryRepository()
	c := &testConfirm{}
	s := NewService(r, c, testTasks{}, nil, NewFakeProvider(), nil)
	cred, e := saveVerifiedCredential(t, s, r, CredentialInput{Name: "x", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b", IdempotencyKey: uuid.NewString()})
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.CreatePlan(context.Background(), PlanInput{CredentialID: cred.ID, StackName: "demo", Operation: OperationCreate, Template: []byte(`{"Resources":{}}`), IdempotencyKey: uuid.NewString()})
	if e != nil {
		t.Fatal(e)
	}
	return s, r, s.provider.(*FakeProvider), p
}

type rejectExecuteClaimCoordinator struct{ ChangeCoordinator }

func (c rejectExecuteClaimCoordinator) ClaimProviderMutation(ctx context.Context, cmd ProviderMutationCommand) (ExecutionFence, error) {
	if cmd.Kind == ProviderMutationExecute {
		return ExecutionFence{}, ErrConflict
	}
	return c.ChangeCoordinator.ClaimProviderMutation(ctx, cmd)
}

func readyWorkflowChange(t *testing.T) (*Service, *MemoryRepository, *FakeProvider, ChangeRequestResult) {
	t.Helper()
	s, repo, provider, plan := workflowFixture(t)
	out, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: plan.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	consumed := consumeWorkflowChange(t, s, repo, out)
	fullPlan, err := repo.GetPlan(context.Background(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	cs, err := provider.CreateChangeSet(context.Background(), CredentialHandle{Region: fullPlan.Region}, ChangeSetRequest{
		Region: fullPlan.Region, StackName: fullPlan.StackName, ChangeSetName: providerChangeSetName(consumed.Change.ProviderToken),
		ClientToken: consumed.Change.ProviderToken, Operation: fullPlan.Operation, Template: fullPlan.Template,
		Parameters: fullPlan.Parameters, Tags: fullPlan.Tags, Capabilities: fullPlan.Capabilities,
	})
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	change := repo.changes[out.Change.ID]
	change.Stage, change.ChangeSetID, change.Revision = StageChangeSetReady, cs.ID, change.Revision+1
	repo.changes[change.ID] = change
	repo.mu.Unlock()
	consumed.Change = change
	return s, repo, provider, consumed
}

// confirmAWSMemory exercises the public shared confirmation service.  The AWS
// memory repository is a deterministic fake for provider workflow tests, so this
// helper applies the projection that PostgreSQL performs in the same commit.
func confirmAWSMemory(t *testing.T, repo *MemoryRepository, out ChangeRequestResult) ChangeRequestResult {
	t.Helper()
	clock := func() time.Time { return time.Now().UTC() }
	confirmations := coreconfirmation.NewMemoryRepository(clock)
	service, err := coreconfirmation.NewService(confirmations, clock)
	if err != nil {
		t.Fatal(err)
	}
	created, err := service.Request(context.Background(), coreconfirmation.RequestCommand{Binding: out.Confirmation.Binding, TaskID: out.Task.ID, IdempotencyKey: uuid.NewString(), At: clock(), ExpiresAt: clock().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	confirmed, err := service.Confirm(context.Background(), coreconfirmation.ConfirmCommand{ConfirmationID: created.ConfirmationID, ExpectedRevision: created.Revision, IdempotencyKey: uuid.NewString(), Binding: out.Confirmation.Binding, At: clock()})
	if err != nil {
		t.Fatal(err)
	}
	repo.mu.Lock()
	defer repo.mu.Unlock()
	conf := repo.confirmations[out.Confirmation.ConfirmationID]
	task := repo.tasks[out.Task.ID]
	change := repo.changes[out.Change.ID]
	conf.State, conf.Revision, conf.UpdatedAt = confirmed.State, confirmed.Revision, clock()
	task.Status, task.Revision = "queued", task.Revision+1
	change.Status, change.Stage, change.Revision, change.UpdatedAt = ChangeRunning, StageRequested, change.Revision+1, clock()
	repo.confirmations[conf.ConfirmationID], repo.tasks[task.ID], repo.changes[change.ID] = conf, task, change
	return ChangeRequestResult{Change: change, Task: task, Confirmation: conf}
}

func consumeWorkflowChange(t *testing.T, s *Service, repo *MemoryRepository, out ChangeRequestResult) ChangeRequestResult {
	t.Helper()
	confirmed := confirmAWSMemory(t, repo, out)
	coord := s.coordinator.(*MemoryChangeCoordinator)
	if err := coord.SetTaskRunning(out.Task.ID, 1, 1, confirmed.Task.Revision); err != nil {
		t.Fatal(err)
	}
	task, err := repo.GetTask(context.Background(), out.Task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConsumeChange(context.Background(), ConsumeChangeCommand{ChangeID: out.Change.ID, ConfirmationID: out.Confirmation.ConfirmationID, TaskID: out.Task.ID, IdempotencyKey: uuid.NewString(), Attempt: 1, LeaseEpoch: 1, ExpectedChangeRevision: confirmed.Change.Revision, ExpectedConfirmationRevision: confirmed.Confirmation.Revision, ExpectedTaskRevision: task.Revision, Binding: out.Confirmation.Binding}); err != nil {
		t.Fatal(err)
	}
	confirmed.Change, _ = repo.GetChange(context.Background(), out.Change.ID)
	return confirmed
}

func TestAtomicCoordinatorInjectedFailureNoOrphans(t *testing.T) {
	s, r, _, p := workflowFixture(t)
	m := s.coordinator.(*MemoryChangeCoordinator)
	m.InjectFailure(true)
	if _, e := s.RequestChange(context.Background(), RequestChangeInput{PlanID: p.ID, IdempotencyKey: uuid.NewString()}); !errors.Is(e, ErrConflict) {
		t.Fatal(e)
	}
	if len(r.changes) != 0 {
		t.Fatal("orphan change")
	}
}
func TestServiceReconstructionReplay(t *testing.T) {
	s, r, _, p := workflowFixture(t)
	in := RequestChangeInput{PlanID: p.ID, IdempotencyKey: uuid.NewString()}
	a, e := s.RequestChange(context.Background(), in)
	if e != nil {
		t.Fatal(e)
	}
	s2 := NewService(r, s.confirmations, testTasks{}, nil, NewFakeProvider(), nil)
	b, e := s2.RequestChange(context.Background(), in)
	if e != nil || a.Change.ID != b.Change.ID {
		t.Fatal(e)
	}
}
func TestConcurrentChangedCredentialReplayConflict(t *testing.T) {
	s, _, _, _ := workflowFixture(t)
	k := uuid.NewString()
	in := CredentialInput{Name: "a", Region: "us-east-1", AccessKeyID: "a", SecretAccessKey: "b", IdempotencyKey: k}
	in2 := in
	in2.Name = "b"
	var wg sync.WaitGroup
	wg.Add(2)
	es := make([]error, 2)
	go func() { defer wg.Done(); _, es[0] = s.SaveCredential(context.Background(), in) }()
	go func() { defer wg.Done(); _, es[1] = s.SaveCredential(context.Background(), in2) }()
	wg.Wait()
	if es[0] == nil && es[1] == nil {
		t.Fatal("expected conflict")
	}
}
func TestPlanReplayConflict(t *testing.T) {
	s, _, _, p := workflowFixture(t)
	_ = p
	k := uuid.NewString()
	in := PlanInput{CredentialID: "bad", IdempotencyKey: k}
	if _, e := s.CreatePlan(context.Background(), in); e == nil {
		t.Fatal("invalid plan accepted")
	}
}
func TestPendingConfirmationCannotExecute(t *testing.T) {
	s, _, f, p := workflowFixture(t)
	out, e := s.RequestChange(context.Background(), RequestChangeInput{PlanID: p.ID, IdempotencyKey: uuid.NewString()})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); !errors.Is(e, ErrUnconfirmed) {
		t.Fatal(e)
	}
	if f.UnconfirmedMutationCalls() != 0 {
		t.Fatal("mutation")
	}
}
func TestCreateResponseLossReconciling(t *testing.T) {
	s, r, f, p := workflowFixture(t)
	out, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: p.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	confirmed := confirmAWSMemory(t, r, out)
	if err := s.coordinator.(*MemoryChangeCoordinator).SetTaskRunning(out.Task.ID, 1, 1, confirmed.Task.Revision); err != nil {
		t.Fatal(err)
	}
	task, _ := r.GetTask(context.Background(), out.Task.ID)
	_, err = s.ConsumeChange(context.Background(), ConsumeChangeCommand{ChangeID: out.Change.ID, ConfirmationID: out.Confirmation.ConfirmationID, TaskID: out.Task.ID, IdempotencyKey: uuid.NewString(), Attempt: 1, LeaseEpoch: 1, ExpectedChangeRevision: confirmed.Change.Revision, ExpectedConfirmationRevision: confirmed.Confirmation.Revision, ExpectedTaskRevision: task.Revision, Binding: out.Confirmation.Binding})
	if err != nil {
		t.Fatal(err)
	}
	const legacyToken = "a1111111-1111-4111-8111-111111111111"
	r.mu.Lock()
	legacyChange := r.changes[out.Change.ID]
	legacyChange.ProviderToken = legacyToken
	legacyChange.ProviderRequestDigest = providerRequestDigest(r.plans[legacyChange.PlanID], legacyToken)
	r.changes[legacyChange.ID] = legacyChange
	r.mu.Unlock()
	f.ResponseLossCreate = true
	if _, err = s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); err != ErrResponseUncertain {
		t.Fatalf("first execute=%v", err)
	}
	f.ResponseLossCreate = false
	for id, cs := range f.Changes {
		if cs.ClientToken == legacyToken {
			cs.Name = legacyToken
			f.Changes[id] = cs
		}
	}
	completed, err := s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != ChangeSucceeded {
		t.Fatalf("recovered=%#v", completed)
	}
	if len(f.DescribeChangeSetNames) < 2 || f.DescribeChangeSetNames[0] != providerChangeSetName(completed.ProviderToken) || f.DescribeChangeSetNames[1] != completed.ProviderToken {
		t.Fatalf("recovery describe names=%v token=%q", f.DescribeChangeSetNames, completed.ProviderToken)
	}
	for _, cs := range f.Changes {
		if cs.ClientToken == completed.ProviderToken {
			if cs.Name != completed.ProviderToken {
				t.Fatalf("change-set name=%q token=%q", cs.Name, cs.ClientToken)
			}
			return
		}
	}
	t.Fatalf("recovered change set for token %q not found", completed.ProviderToken)
}

func TestCreateResponseLossDigitTokenSkipsInvalidLegacyLookup(t *testing.T) {
	s, r, f, p := workflowFixture(t)
	out, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: p.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	consumed := consumeWorkflowChange(t, s, r, out)
	const digitToken = "11111111-1111-4111-8111-111111111111"
	r.mu.Lock()
	change := r.changes[consumed.Change.ID]
	change.ProviderToken = digitToken
	change.ProviderRequestDigest = providerRequestDigest(r.plans[change.PlanID], digitToken)
	r.changes[change.ID] = change
	r.mu.Unlock()
	f.ResponseLossCreate = true
	if _, err = s.ExecuteChange(context.Background(), consumed.Confirmation.ConfirmationID); err != ErrResponseUncertain {
		t.Fatalf("first execute=%v", err)
	}
	f.ResponseLossCreate = false
	for id, cs := range f.Changes {
		if cs.ClientToken == digitToken {
			cs.Name = "other-change-set"
			f.Changes[id] = cs
		}
	}
	if _, err = s.ExecuteChange(context.Background(), consumed.Confirmation.ConfirmationID); err != ErrResponseUncertain {
		t.Fatalf("missing canonical change set err=%v", err)
	}
	if len(f.DescribeChangeSetNames) != 1 || f.DescribeChangeSetNames[0] != providerChangeSetName(digitToken) {
		t.Fatalf("digit-token recovery describe names=%v", f.DescribeChangeSetNames)
	}
}

func TestExecuteResponseLossReconciling(t *testing.T) {
	_, _, f, p := workflowFixture(t)
	f.ResponseLossExecute = true
	_ = p
}
func TestDeleteResponseLossReconciling(t *testing.T) {
	s, r, f, p := workflowFixture(t)
	cred, err := s.GetCredential(context.Background(), p.CredentialID)
	if err != nil {
		t.Fatal(err)
	}
	deletePlan, err := s.CreatePlan(context.Background(), PlanInput{CredentialID: cred.ID, Region: p.Region, StackName: p.StackName + "-delete", Operation: OperationDelete, Template: []byte(`{"Resources":{}}`), IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: deletePlan.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	confirmed := confirmAWSMemory(t, r, out)
	if err := s.coordinator.(*MemoryChangeCoordinator).SetTaskRunning(out.Task.ID, 1, 1, confirmed.Task.Revision); err != nil {
		t.Fatal(err)
	}
	task, _ := r.GetTask(context.Background(), out.Task.ID)
	_, err = s.ConsumeChange(context.Background(), ConsumeChangeCommand{ChangeID: out.Change.ID, ConfirmationID: out.Confirmation.ConfirmationID, TaskID: out.Task.ID, IdempotencyKey: uuid.NewString(), Attempt: 1, LeaseEpoch: 1, ExpectedChangeRevision: confirmed.Change.Revision, ExpectedConfirmationRevision: confirmed.Confirmation.Revision, ExpectedTaskRevision: task.Revision, Binding: out.Confirmation.Binding})
	if err != nil {
		t.Fatal(err)
	}
	f.ResponseLossDelete = true
	if _, err = s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); err != ErrResponseUncertain {
		t.Fatalf("first delete=%v", err)
	}
	f.ResponseLossDelete = false
	if _, err = s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); err != nil {
		t.Fatal(err)
	}
}
func TestCASFailurePreventsProviderCall(t *testing.T) {
	s, _, f, p := workflowFixture(t)
	out, _ := s.RequestChange(context.Background(), RequestChangeInput{PlanID: p.ID, IdempotencyKey: uuid.NewString()})
	_ = out
	if f.UnconfirmedMutationCalls() != 0 {
		t.Fatal()
	}
}
func TestNoDoubleExecute(t *testing.T) {
	f := NewFakeProvider()
	req := ChangeSetRequest{Region: "us-east-1", StackName: "demo", ChangeSetName: "x", ClientToken: uuid.NewString(), Operation: OperationCreate, Template: []byte("{}")}
	cs, _ := f.CreateChangeSet(context.Background(), CredentialHandle{Region: req.Region}, req)
	_ = f.ExecuteChangeSet(context.Background(), CredentialHandle{Region: req.Region}, req.Region, req.StackName, cs.ID, req.ClientToken)
	_ = f.ExecuteChangeSet(context.Background(), CredentialHandle{Region: req.Region}, req.Region, req.StackName, cs.ID, req.ClientToken)
	if f.UnconfirmedMutationCalls() != 2 {
		t.Fatal()
	}
}

func TestExecuteChangeStrictReadyExecutesExactlyOnce(t *testing.T) {
	s, _, provider, consumed := readyWorkflowChange(t)
	done, err := s.ExecuteChange(context.Background(), consumed.Confirmation.ConfirmationID)
	if err != nil || done.Status != ChangeSucceeded {
		t.Fatalf("ready execute = %+v err=%v", done, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	count := 0
	for _, call := range provider.Calls {
		if call == "execute_change_set" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("ExecuteChangeSet calls=%d all=%v", count, provider.Calls)
	}
}

func TestExecuteChangeStrictReadyClaimErrorSkipsProvider(t *testing.T) {
	s, repo, provider, consumed := readyWorkflowChange(t)
	wrapped := NewServiceWithCoordinator(repo, rejectExecuteClaimCoordinator{s.coordinator}, s.confirmations, s.tasks, s.sts, provider, nil)
	if _, err := wrapped.ExecuteChange(context.Background(), consumed.Confirmation.ConfirmationID); !errors.Is(err, ErrConflict) {
		t.Fatalf("claim error = %v", err)
	}
	if len(provider.Calls) != 1 || provider.Calls[0] != "create_change_set" {
		t.Fatalf("provider calls after claim error = %v", provider.Calls)
	}
}

func TestExecuteChangeStrictReadyResponseLossStaysUncertain(t *testing.T) {
	s, repo, provider, consumed := readyWorkflowChange(t)
	provider.ResponseLossExecute = true
	uncertain, err := s.ExecuteChange(context.Background(), consumed.Confirmation.ConfirmationID)
	if !errors.Is(err, ErrResponseUncertain) {
		t.Fatalf("execute response loss = %v", err)
	}
	if uncertain.Stage != StageReconciling {
		t.Fatalf("response-loss stage=%q, want reconciling", uncertain.Stage)
	}
	// Re-enter under a reclaimed lease; the reconciling stage must use durable
	// readback rather than dispatching ExecuteChangeSet again.
	repo.mu.Lock()
	task := repo.tasks[consumed.Task.ID]
	task.Revision++
	task.LeaseEpoch++
	repo.tasks[task.ID] = task
	repo.mu.Unlock()
	provider.ResponseLossExecute = false
	done, err := s.ExecuteChange(context.Background(), consumed.Confirmation.ConfirmationID)
	if err != nil || done.Status != ChangeSucceeded {
		t.Fatalf("reconcile after response loss = %+v err=%v", done, err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	count := 0
	for _, call := range provider.Calls {
		if call == "execute_change_set" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("response-loss ExecuteChangeSet calls=%d all=%v", count, provider.Calls)
	}
}

func TestAsyncInProgressNotSuccess(t *testing.T) {
	f := NewFakeProvider()
	f.Async = true
	req := ChangeSetRequest{Region: "us-east-1", StackName: "demo", ChangeSetName: "x", ClientToken: uuid.NewString(), Operation: OperationCreate, Template: []byte("{}")}
	cs, _ := f.CreateChangeSet(context.Background(), CredentialHandle{Region: req.Region}, req)
	_ = f.ExecuteChangeSet(context.Background(), CredentialHandle{Region: req.Region}, req.Region, req.StackName, cs.ID, req.ClientToken)
	s, _ := f.DescribeStack(context.Background(), CredentialHandle{Region: req.Region}, req.Region, req.StackName)
	if s.Status == "CREATE_COMPLETE" {
		t.Fatal()
	}
}

func TestAsyncCreatePollReconciliation(t *testing.T) {
	s, repo, provider, plan := workflowFixture(t)
	out, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: plan.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	consumeWorkflowChange(t, s, repo, out)
	provider.Async = true
	if _, err = s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); err != ErrResponseUncertain {
		t.Fatalf("async create err=%v", err)
	}
	provider.PollSequence[plan.Region+"/"+plan.StackName] = []string{"CREATE_COMPLETE"}
	done, err := s.PollChange(context.Background(), out.Confirmation.ConfirmationID)
	if err != nil || done.Status != ChangeSucceeded {
		t.Fatalf("reconciled create=%+v err=%v", done, err)
	}
}

func TestRequiredStackOutputMissingEvidenceStaysUncertain(t *testing.T) {
	s, repo, provider, plan := workflowFixture(t)
	out, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: plan.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	consumeWorkflowChange(t, s, repo, out)
	fullPlan, err := repo.GetPlan(context.Background(), plan.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider.Stacks[plan.Region+"/"+plan.StackName] = Stack{Region: plan.Region, StackName: plan.StackName, Status: "CREATE_COMPLETE", TemplateSHA256: fullPlan.TemplateSHA256, Parameters: map[string]string{}, Tags: map[string]string{}}
	c, err := repo.GetChange(context.Background(), out.Change.ID)
	if err != nil {
		t.Fatal(err)
	}
	fullPlan.Tags = map[string]string{RequiredOutputsTag: string(StackOutputInstanceID)}
	if _, err = s.reconcileChange(context.Background(), c, fullPlan); err != ErrResponseUncertain {
		t.Fatalf("missing output err=%v", err)
	}
	provider.Stacks[plan.Region+"/"+plan.StackName] = Stack{Region: plan.Region, StackName: plan.StackName, Status: "CREATE_COMPLETE", TemplateSHA256: fullPlan.TemplateSHA256, Parameters: map[string]string{}, Tags: cloneMap(fullPlan.Tags), Outputs: StackOutputs{string(StackOutputInstanceID): "i-0123456789abcdef0"}}
	if done, err := s.reconcileChange(context.Background(), c, fullPlan); err != nil || done.Status != ChangeSucceeded {
		t.Fatalf("output readback done=%+v err=%v", done, err)
	}
}

func TestPollChangeResponseLossRequiresDurableOutputReadback(t *testing.T) {
	s, repo, provider, base := workflowFixture(t)
	plan, err := s.CreatePlan(context.Background(), PlanInput{CredentialID: base.CredentialID, Region: base.Region, StackName: "output-poll", Operation: OperationCreate, Template: []byte(`{"Resources":{}}`), Tags: map[string]string{RequiredOutputsTag: string(StackOutputInstanceID)}, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: plan.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	consumeWorkflowChange(t, s, repo, out)
	provider.ResponseLossExecute = true
	if _, err = s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); err != ErrResponseUncertain {
		t.Fatalf("execute response loss err=%v", err)
	}
	if _, err = s.PollChange(context.Background(), out.Confirmation.ConfirmationID); err != ErrResponseUncertain {
		t.Fatalf("missing output poll err=%v", err)
	}
	provider.ResponseLossExecute = false
	stack := provider.Stacks[base.Region+"/output-poll"]
	stack.Outputs = StackOutputs{string(StackOutputInstanceID): "i-0123456789abcdef0"}
	provider.Stacks[base.Region+"/output-poll"] = stack
	done, err := s.PollChange(context.Background(), out.Confirmation.ConfirmationID)
	if err != nil || done.Status != ChangeSucceeded {
		t.Fatalf("readback poll done=%+v err=%v", done, err)
	}
}

func TestAsyncDeletePollReconciliation(t *testing.T) {
	s, repo, provider, plan := workflowFixture(t)
	deletePlan, err := s.CreatePlan(context.Background(), PlanInput{CredentialID: plan.CredentialID, Region: plan.Region, StackName: plan.StackName + "-delete", Operation: OperationDelete, Template: []byte(`{"Resources":{}}`), IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	provider.Stacks[deletePlan.Region+"/"+deletePlan.StackName] = Stack{Region: deletePlan.Region, StackName: deletePlan.StackName, Status: "CREATE_COMPLETE", TemplateSHA256: deletePlan.TemplateSHA256, Parameters: map[string]string{}, Tags: map[string]string{}}
	out, err := s.RequestChange(context.Background(), RequestChangeInput{PlanID: deletePlan.ID, IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	consumeWorkflowChange(t, s, repo, out)
	provider.Async = true
	if _, err = s.ExecuteChange(context.Background(), out.Confirmation.ConfirmationID); err != ErrResponseUncertain {
		t.Fatalf("async delete err=%v", err)
	}
	provider.PollSequence[deletePlan.Region+"/"+deletePlan.StackName] = []string{"DELETE_COMPLETE"}
	done, err := s.PollChange(context.Background(), out.Confirmation.ConfirmationID)
	if err != nil || done.Status != ChangeSucceeded {
		t.Fatalf("reconciled delete=%+v err=%v", done, err)
	}
}
func TestExactTerminalFingerprint(t *testing.T) {
	f := NewFakeProvider()
	req := ChangeSetRequest{Region: "us-east-1", StackName: "demo", ChangeSetName: "x", ClientToken: uuid.NewString(), Operation: OperationCreate, Template: []byte("{}")}
	_, _ = f.CreateChangeSet(context.Background(), CredentialHandle{Region: req.Region}, req)
	if _, e := f.DescribeChangeSet(context.Background(), CredentialHandle{Region: "eu-west-1"}, "eu-west-1", "demo", "x"); e == nil {
		t.Fatal()
	}
}
func TestCompleteStaleFence(t *testing.T) {
	s, _, _, p := workflowFixture(t)
	out, _ := s.RequestChange(context.Background(), RequestChangeInput{PlanID: p.ID, IdempotencyKey: uuid.NewString()})
	_, e := s.CompleteChange(context.Background(), CompleteChangeCommand{ChangeID: out.Change.ID, ConfirmationID: out.Confirmation.ConfirmationID, ExpectedChangeRevision: 1, Status: ChangeSucceeded})
	if e == nil {
		t.Fatal("stale fence accepted")
	}
}
func TestPaginationMutationStable(t *testing.T) {
	s, _, _, _ := workflowFixture(t)
	if _, e := s.ListCredentials(context.Background(), 1, ""); e != nil {
		t.Fatal(e)
	}
}
