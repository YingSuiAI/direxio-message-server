package p2p

import (
	"context"
	"errors"
	"testing"
	"time"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	agentruntime "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/runtime"
	agenttask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

type preProviderCoordinator struct {
	initial, recovery coreaws.ExecutionFence
	onConsume         func()
	recoveryErr       error
	complete          coreaws.Change
	completeErr       error
	completeCommand   coreaws.CompleteChangeCommand
	executionFences   int
	consumeCalls      int
}

type failingAWSRepository struct {
	coreaws.Repository
	getChanges int
	failAfter  int
}

func (r *failingAWSRepository) GetChange(ctx context.Context, id string) (coreaws.Change, error) {
	r.getChanges++
	if r.failAfter > 0 && r.getChanges > r.failAfter {
		return coreaws.Change{}, coreaws.ErrConflict
	}
	return r.Repository.GetChange(ctx, id)
}

func (c *preProviderCoordinator) RequestChange(context.Context, coreaws.RequestChangeInput) (coreaws.ChangeRequestResult, error) {
	return coreaws.ChangeRequestResult{}, coreaws.ErrConflict
}
func (c *preProviderCoordinator) ConsumeChange(context.Context, coreaws.ConsumeChangeCommand) (coreaws.Reservation, error) {
	c.consumeCalls++
	if c.onConsume != nil {
		c.onConsume()
	}
	return coreaws.Reservation{}, coreaws.ErrConflict
}
func (c *preProviderCoordinator) CompleteChange(_ context.Context, command coreaws.CompleteChangeCommand) (coreaws.Change, error) {
	c.completeCommand = command
	if c.completeErr != nil {
		return coreaws.Change{}, c.completeErr
	}
	if c.complete.ID != "" {
		return c.complete, nil
	}
	return coreaws.Change{}, coreaws.ErrConflict
}
func (c *preProviderCoordinator) ExecutionFence(context.Context, string) (coreaws.ExecutionFence, error) {
	c.executionFences++
	if c.executionFences == 1 {
		return c.initial, nil
	}
	if c.recoveryErr != nil {
		return coreaws.ExecutionFence{}, c.recoveryErr
	}
	return c.recovery, nil
}
func (c *preProviderCoordinator) ReconcileChange(context.Context, coreaws.ReconcileChangeCommand) (coreaws.Change, error) {
	return coreaws.Change{}, coreaws.ErrConflict
}
func (c *preProviderCoordinator) ClaimProviderMutation(context.Context, coreaws.ProviderMutationCommand) (coreaws.ExecutionFence, error) {
	return coreaws.ExecutionFence{}, coreaws.ErrConflict
}
func (c *preProviderCoordinator) CommitProviderMutation(context.Context, coreaws.ProviderMutationResult) (coreaws.Change, error) {
	return coreaws.Change{}, coreaws.ErrConflict
}
func (c *preProviderCoordinator) PersistChangeSetEvidence(context.Context, coreaws.ChangeSetEvidenceCommand) (coreaws.Change, error) {
	return coreaws.Change{}, coreaws.ErrConflict
}

func preProviderAWSChangeTask(changeID, taskID string) agenttask.Task {
	now := time.Now().UTC()
	return agenttask.Task{
		OwnerID: "@pre-provider:example.test", ID: taskID, Attempt: 1, LeaseEpoch: 9, Revision: 7,
		Status: agenttask.StatusRunning, CreatedAt: now, UpdatedAt: now, AvailableAt: now,
		Lease: &agenttask.Lease{TaskID: taskID, Attempt: 1, Epoch: 9, Holder: "worker", ExpiresAt: now.Add(time.Minute)},
		Spec:  agenttask.TaskSpec{Kind: agenttask.TaskKindAWSChange, IdempotencyKey: uuid.NewString(), Payload: agenttask.TaskPayload{AWSChange: &agenttask.AWSChangeTaskPayload{ChangeID: changeID}}},
	}
}

func preProviderDispatchFixture(t *testing.T, stage coreaws.ChangeStage, wrap func(coreaws.Repository) coreaws.Repository) (*coreaws.Service, *preProviderCoordinator, *coreaws.FakeProvider, agenttask.Task, coreaws.Change) {
	t.Helper()
	const (
		changeID = "44444444-4444-4444-8444-444444444444"
		taskID   = "55555555-5555-4555-8555-555555555555"
		confID   = "66666666-6666-4666-8666-666666666666"
		planID   = "77777777-7777-4777-8777-777777777777"
		credID   = "88888888-8888-4888-8888-888888888888"
	)
	now := time.Now().UTC()
	base := coreaws.NewMemoryRepository()
	cred := coreaws.RehydrateCredentials(credID, "test", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/test", []byte("access"), []byte("secret"), nil, 1, 1, now, now)
	if _, err := base.CreateCredential(context.Background(), cred); err != nil {
		t.Fatal(err)
	}
	seed := coreaws.NewService(base, nil, nil, nil, coreaws.NewFakeProvider(), nil)
	view, err := seed.CreatePlan(context.Background(), coreaws.PlanInput{ID: planID, CredentialID: credID, Region: "us-east-1", StackName: "dispatch-regression", Operation: coreaws.OperationCreate, Template: []byte(`{"Resources":{}}`), IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := base.GetPlan(context.Background(), view.ID)
	if err != nil {
		t.Fatal(err)
	}
	change := coreaws.Change{ID: changeID, PlanID: planID, CredentialID: credID, TaskID: taskID, ConfirmationID: confID, Operation: coreaws.OperationCreate, Status: coreaws.ChangeRunning, Stage: stage, Revision: 2, ProviderToken: confID, ProviderRequestDigest: coreaws.ProviderRequestDigest(plan, confID), CreatedAt: now, UpdatedAt: now}
	if stage == coreaws.StageChangeSetReady {
		change.ChangeSetID = "change-set-id"
	}
	if _, err = base.CreateChange(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	fence := coreaws.ExecutionFence{Change: change, Task: coreaws.Task{ID: taskID, Status: "running", Revision: 3, Attempt: 1, LeaseEpoch: 1}, Confirmation: coreconfirmation.Confirmation{ConfirmationID: confID, TaskID: taskID, State: coreconfirmation.StateConsumed, Revision: 2, Binding: coreaws.BindingForPlan(plan, cred)}, Reservation: coreaws.Reservation{ConfirmationID: confID, TaskID: taskID, Attempt: 1, LeaseEpoch: 1, TaskRevision: 3, Active: true}}
	coord := &preProviderCoordinator{initial: fence, recovery: fence}
	provider := coreaws.NewFakeProvider()
	repo := coreaws.Repository(base)
	if wrap != nil {
		repo = wrap(repo)
	}
	queued := preProviderAWSChangeTask(changeID, taskID)
	queued.Attempt, queued.LeaseEpoch, queued.Revision = 1, 1, 3
	queued.Lease.Attempt, queued.Lease.Epoch = 1, 1
	return coreaws.NewServiceWithCoordinator(repo, coord, nil, nil, nil, provider, nil), coord, provider, queued, change
}

func TestEmbeddedTaskExecutorAWSPreProviderConsumeRecoveryMapping(t *testing.T) {
	const (
		changeID = "11111111-1111-4111-8111-111111111111"
		taskID   = "22222222-2222-4222-8222-222222222222"
		confID   = "33333333-3333-4333-8333-333333333333"
	)
	baseChange := coreaws.Change{ID: changeID, PlanID: uuid.NewString(), CredentialID: uuid.NewString(), TaskID: taskID, ConfirmationID: confID, Operation: coreaws.OperationCreate, Status: coreaws.ChangeWaitingUser, Stage: coreaws.StageRequested, Revision: 4, ProviderToken: confID, ProviderRequestDigest: "provider-digest"}
	queued := preProviderAWSChangeTask(changeID, taskID)
	nonExpired := time.Now().UTC().Add(time.Hour)

	cases := []struct {
		name         string
		recovery     coreaws.ExecutionFence
		recoveryErr  error
		mutateChange func(*coreaws.Change)
		want         error
	}{
		{
			name: "durable requested rows defer",
			recovery: coreaws.ExecutionFence{
				Change:       baseChange,
				Task:         coreaws.Task{ID: taskID, Status: "running", Revision: 7, Attempt: 1, LeaseEpoch: 9},
				Confirmation: coreconfirmation.Confirmation{ConfirmationID: confID, TaskID: taskID, State: coreconfirmation.StateConfirmed, Revision: 3, ExpiresAt: nonExpired},
				Reservation:  coreaws.Reservation{ConfirmationID: confID, TaskID: taskID},
			},
			want: agentruntime.ErrTaskDeferred,
		},
		{
			name: "expired confirmed confirmation preserves consume error",
			recovery: coreaws.ExecutionFence{
				Change:       baseChange,
				Task:         coreaws.Task{ID: taskID, Status: "running", Revision: 7, Attempt: 1, LeaseEpoch: 9},
				Confirmation: coreconfirmation.Confirmation{ConfirmationID: confID, TaskID: taskID, State: coreconfirmation.StateConfirmed, Revision: 3, ExpiresAt: time.Now().UTC().Add(-time.Second)},
			},
			want: coreaws.ErrConflict,
		},
		{
			name: "consumed active reservation finalizes",
			recovery: coreaws.ExecutionFence{
				Change: func() coreaws.Change {
					c := baseChange
					c.Status, c.Stage = coreaws.ChangeRunning, coreaws.StageChangeSetCreating
					return c
				}(),
				Task:         coreaws.Task{ID: taskID, Status: "running", Revision: 7, Attempt: 1, LeaseEpoch: 9},
				Confirmation: coreconfirmation.Confirmation{ConfirmationID: confID, TaskID: taskID, State: coreconfirmation.StateConsumed, Revision: 4},
				Reservation:  coreaws.Reservation{ConfirmationID: confID, TaskID: taskID, Attempt: 1, LeaseEpoch: 9, TaskRevision: 7, Active: true},
			},
			mutateChange: func(c *coreaws.Change) {
				c.Status, c.Stage, c.Revision = coreaws.ChangeRunning, coreaws.StageChangeSetCreating, c.Revision+1
			},
			want: agentruntime.ErrTaskFinalized,
		},
		{
			name: "terminal change finalizes",
			recovery: coreaws.ExecutionFence{
				Change: func() coreaws.Change {
					c := baseChange
					c.Status, c.Stage = coreaws.ChangeSucceeded, coreaws.StageSucceeded
					return c
				}(),
				Task:         coreaws.Task{ID: taskID, Status: "running", Revision: 7, Attempt: 1, LeaseEpoch: 9},
				Confirmation: coreconfirmation.Confirmation{ConfirmationID: confID, TaskID: taskID, State: coreconfirmation.StateConfirmed, Revision: 3},
				Reservation:  coreaws.Reservation{ConfirmationID: confID, TaskID: taskID},
			},
			mutateChange: func(c *coreaws.Change) {
				c.Status, c.Stage, c.Revision = coreaws.ChangeSucceeded, coreaws.StageSucceeded, c.Revision+1
			},
			want: agentruntime.ErrTaskFinalized,
		},
		{
			name:        "recovery fence read defers",
			recoveryErr: coreaws.ErrConflict,
			want:        agentruntime.ErrTaskDeferred,
		},
		{
			name:     "linkage mismatch preserves consume error",
			recovery: coreaws.ExecutionFence{Change: baseChange, Task: coreaws.Task{ID: uuid.NewString(), Status: "running", Revision: 7, Attempt: 1, LeaseEpoch: 9}, Confirmation: coreconfirmation.Confirmation{ConfirmationID: confID, TaskID: taskID, State: coreconfirmation.StateConfirmed, Revision: 3}},
			want:     coreaws.ErrConflict,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			repo := coreaws.NewMemoryRepository()
			if _, err := repo.CreateChange(context.Background(), baseChange); err != nil {
				t.Fatal(err)
			}
			coord := &preProviderCoordinator{initial: coreaws.ExecutionFence{Change: baseChange, Task: coreaws.Task{ID: taskID, Status: "running", Revision: 7, Attempt: 1, LeaseEpoch: 9}, Confirmation: coreconfirmation.Confirmation{ConfirmationID: confID, TaskID: taskID, State: coreconfirmation.StateConfirmed, Revision: 3, ExpiresAt: nonExpired}}, recovery: tc.recovery, recoveryErr: tc.recoveryErr}
			if tc.mutateChange != nil {
				coord.onConsume = func() {
					c, err := repo.GetChange(context.Background(), changeID)
					if err != nil {
						t.Fatalf("read durable change: %v", err)
					}
					tc.mutateChange(&c)
					if _, err = repo.UpdateChange(context.Background(), c, baseChange.Revision); err != nil {
						t.Fatalf("mutate durable change: %v", err)
					}
				}
			}
			provider := coreaws.NewFakeProvider()
			aws := coreaws.NewServiceWithCoordinator(repo, coord, nil, nil, nil, provider, nil)
			executor := embeddedTaskExecutor{aws: aws}
			_, err := executor.executeAWSChange(context.Background(), queued)
			if !errors.Is(err, tc.want) {
				t.Fatalf("executeAWSChange error = %v, want %v", err, tc.want)
			}
			if coord.consumeCalls != 1 {
				t.Fatalf("ConsumeChange calls = %d, want 1", coord.consumeCalls)
			}
			if len(provider.Calls) != 0 {
				t.Fatalf("provider calls = %v, want none before durable consume commit", provider.Calls)
			}
			if tc.name == "durable requested rows defer" {
				got, readErr := repo.GetChange(context.Background(), changeID)
				if readErr != nil || got.Status != coreaws.ChangeWaitingUser || got.Stage != coreaws.StageRequested || got.Revision != baseChange.Revision {
					t.Fatalf("durable requested change mutated: %+v err=%v", got, readErr)
				}
			}
		})
	}
}

func TestEmbeddedTaskExecutorAWSPreProviderErrorTerminalizesBeforeDispatch(t *testing.T) {
	const (
		changeID = "44444444-4444-4444-8444-444444444444"
		taskID   = "55555555-5555-4555-8555-555555555555"
		confID   = "66666666-6666-4666-8666-666666666666"
		planID   = "77777777-7777-4777-8777-777777777777"
		credID   = "88888888-8888-4888-8888-888888888888"
	)
	now := time.Now().UTC()
	repo := coreaws.NewMemoryRepository()
	cred := coreaws.RehydrateCredentials(credID, "test", "us-east-1", "123456789012", "arn:aws:iam::123456789012:user/test", []byte("access"), []byte("secret"), nil, 1, 1, now, now)
	if _, err := repo.CreateCredential(context.Background(), cred); err != nil {
		t.Fatal(err)
	}
	service := coreaws.NewService(repo, nil, nil, nil, coreaws.NewFakeProvider(), nil)
	planView, err := service.CreatePlan(context.Background(), coreaws.PlanInput{ID: planID, CredentialID: credID, Region: "us-east-1", StackName: "dispatch-regression", Operation: coreaws.OperationCreate, Template: []byte(`{"Resources":{}}`), IdempotencyKey: uuid.NewString()})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := repo.GetPlan(context.Background(), planView.ID)
	if err != nil {
		t.Fatal(err)
	}
	binding := coreaws.BindingForPlan(plan, cred)
	change := coreaws.Change{ID: changeID, PlanID: planID, CredentialID: credID, TaskID: taskID, ConfirmationID: confID, Operation: coreaws.OperationCreate, Status: coreaws.ChangeRunning, Stage: coreaws.StageChangeSetCreating, Revision: 2, ProviderToken: confID, ProviderRequestDigest: coreaws.ProviderRequestDigest(plan, confID), CreatedAt: now, UpdatedAt: now}
	if _, err := repo.CreateChange(context.Background(), change); err != nil {
		t.Fatal(err)
	}
	fence := coreaws.ExecutionFence{Change: change, Task: coreaws.Task{ID: taskID, Status: "running", Revision: 3, Attempt: 1, LeaseEpoch: 1}, Confirmation: coreconfirmation.Confirmation{ConfirmationID: confID, TaskID: taskID, State: coreconfirmation.StateConsumed, Revision: 2, Binding: binding}, Reservation: coreaws.Reservation{ConfirmationID: confID, TaskID: taskID, Attempt: 1, LeaseEpoch: 1, TaskRevision: 3, Active: true}}
	coord := &preProviderCoordinator{initial: fence, recovery: fence}
	provider := coreaws.NewFakeProvider()
	aws := coreaws.NewServiceWithCoordinator(repo, coord, nil, nil, nil, provider, nil)
	queued := agenttask.Task{ID: taskID, Attempt: 1, LeaseEpoch: 1, Revision: 3, Status: agenttask.StatusRunning, Lease: &agenttask.Lease{TaskID: taskID, Attempt: 1, Epoch: 1, Holder: "worker", ExpiresAt: now.Add(time.Minute)}, Spec: agenttask.TaskSpec{Kind: agenttask.TaskKindAWSChange, IdempotencyKey: uuid.NewString(), Payload: agenttask.TaskPayload{AWSChange: &agenttask.AWSChangeTaskPayload{ChangeID: changeID}}}}
	executor := embeddedTaskExecutor{aws: aws}
	coord.complete = func() coreaws.Change {
		failed := change
		failed.Status, failed.Stage = coreaws.ChangeFailed, coreaws.StageFailed
		return failed
	}()
	_, err = executor.executeAWSChange(context.Background(), queued)
	if !errors.Is(err, agentruntime.ErrTaskFinalized) {
		t.Fatalf("pre-provider ExecuteChange error = %v, want finalized after atomic failure", err)
	}
	if coord.completeCommand.Status != coreaws.ChangeFailed || coord.completeCommand.LeaseEpoch != 1 || coord.completeCommand.ExpectedTaskRevision != 3 {
		t.Fatalf("completion command = %+v", coord.completeCommand)
	}
	if len(provider.Calls) != 0 {
		t.Fatalf("provider calls = %v, want none before dispatch evidence", provider.Calls)
	}
}

func TestEmbeddedTaskExecutorAWSLatestReadFailureDefers(t *testing.T) {
	aws, _, provider, queued, _ := preProviderDispatchFixture(t, coreaws.StageChangeSetCreating, func(repo coreaws.Repository) coreaws.Repository {
		return &failingAWSRepository{Repository: repo, failAfter: 1}
	})
	executor := embeddedTaskExecutor{aws: aws}
	_, err := executor.executeAWSChange(context.Background(), queued)
	if !errors.Is(err, agentruntime.ErrTaskDeferred) {
		t.Fatalf("latest change read error = %v, want deferred", err)
	}
	if len(provider.Calls) != 0 {
		t.Fatalf("provider calls = %v, want none", provider.Calls)
	}
}

func TestEmbeddedTaskExecutorAWSProviderBackedStageRemainsCoordinatorOwned(t *testing.T) {
	aws, coordinator, provider, queued, _ := preProviderDispatchFixture(t, coreaws.StageChangeSetReady, nil)
	executor := embeddedTaskExecutor{aws: aws}
	_, err := executor.executeAWSChange(context.Background(), queued)
	if !errors.Is(err, agentruntime.ErrTaskFinalized) {
		t.Fatalf("provider-backed stage error = %v, want finalized", err)
	}
	if coordinator.completeCommand.ChangeID != "" {
		t.Fatalf("provider-backed stage was terminalized as provider-free: %+v", coordinator.completeCommand)
	}
	if len(provider.Calls) != 0 {
		t.Fatalf("provider calls = %v, want none", provider.Calls)
	}
}
