package runtime

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

type fakeTaskStore struct {
	mu         sync.Mutex
	claimed    bool
	claims     int
	claimLimit int
	claimMax   []int
	fail       chan task.FailCommand
	complete   chan task.CompleteCommand
	timeout    chan task.TimeoutCommand
	task       task.Task
}

func (s *fakeTaskStore) Claimed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.claimed
}

func (s *fakeTaskStore) ClaimMaxima() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int(nil), s.claimMax...)
}

func (s *fakeTaskStore) ClaimNextDue(_ context.Context, _ string, _ time.Time, _ time.Duration, max int) (task.Task, task.Lease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimMax = append(s.claimMax, max)
	limit := s.claimLimit
	if limit == 0 {
		limit = 1
	}
	if s.claims >= limit {
		return task.Task{}, task.Lease{}, task.ErrNotFound
	}
	s.claimed = true
	s.claims++
	v := s.task
	if v.ID == "" {
		v = task.Task{ID: "task-" + string(rune('0'+s.claims)), Revision: 1}
	}
	return v, task.Lease{TaskID: v.ID, Attempt: 1, Epoch: 1, Holder: "worker", ExpiresAt: time.Now().UTC().Add(time.Minute)}, nil
}
func (s *fakeTaskStore) RenewLease(context.Context, task.RenewLeaseCommand) (task.Lease, error) {
	return task.Lease{Epoch: 1}, nil
}
func (s *fakeTaskStore) CompleteTask(_ context.Context, c task.CompleteCommand) (task.Task, error) {
	s.complete <- c
	return task.Task{}, nil
}
func (s *fakeTaskStore) FailTask(_ context.Context, c task.FailCommand) error {
	s.fail <- c
	return nil
}
func (s *fakeTaskStore) TimeoutTask(_ context.Context, c task.TimeoutCommand) error {
	if s.timeout != nil {
		s.timeout <- c
	}
	return nil
}

type fakeExecutor struct{ err error }

func (e fakeExecutor) Execute(context.Context, task.Task) (task.Result, error) {
	return task.Result{}, e.err
}

type blockingExecutor struct {
	started  chan struct{}
	canceled chan struct{}
}

type multiBlockingExecutor struct {
	started chan struct{}
	once    sync.Once
}

func (e *multiBlockingExecutor) Execute(ctx context.Context, _ task.Task) (task.Result, error) {
	e.once.Do(func() { close(e.started) })
	<-ctx.Done()
	return task.Result{}, ctx.Err()
}

func TestWorkerClaimAlwaysUsesConfiguredGlobalLimit(t *testing.T) {
	store := &fakeTaskStore{claimLimit: 3, fail: make(chan task.FailCommand, 3), complete: make(chan task.CompleteCommand, 3)}
	executor := &multiBlockingExecutor{started: make(chan struct{})}
	w, err := New(Config{Store: store, Executor: executor, MaxConcurrent: 2, LeaseTTL: time.Second, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()
	deadline := time.Now().Add(time.Second)
	for len(store.ClaimMaxima()) < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := store.ClaimMaxima(); len(got) != 2 || got[0] != 2 || got[1] != 2 {
		t.Fatalf("claim limits=%v, want two configured global limits", got)
	}
	// Two local executions exhaust the worker without attempting a third
	// claim. This is a local admission check only; it does not alter the
	// durable global slot limit supplied to either successful claim.
	time.Sleep(25 * time.Millisecond)
	if got := store.ClaimMaxima(); len(got) != 2 {
		t.Fatalf("claims while locally saturated=%v", got)
	}
	grace, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := w.StopWithContext(grace); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop error=%v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("run error=%v", err)
	}
}

func (e blockingExecutor) Execute(ctx context.Context, _ task.Task) (task.Result, error) {
	close(e.started)
	<-ctx.Done()
	close(e.canceled)
	return task.Result{}, ctx.Err()
}

func TestWorkerFailsClosedOnUncertainExecution(t *testing.T) {
	store := &fakeTaskStore{fail: make(chan task.FailCommand, 1), complete: make(chan task.CompleteCommand, 1)}
	w, err := New(Config{Store: store, Executor: fakeExecutor{err: ErrExecutionUncertain}, MaxConcurrent: 1, LeaseTTL: time.Second, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case failed := <-store.fail:
		if failed.ErrorCode != "execution_uncertain" {
			t.Fatalf("code=%q", failed.ErrorCode)
		}
		cancel()
	case <-time.After(time.Second):
		t.Fatal("worker did not fail task")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v", err)
	}
}

func TestWorkerLeavesDeferredTaskNonterminal(t *testing.T) {
	store := &fakeTaskStore{fail: make(chan task.FailCommand, 1), complete: make(chan task.CompleteCommand, 1)}
	w, err := New(Config{Store: store, Executor: fakeExecutor{err: ErrTaskDeferred}, MaxConcurrent: 1, LeaseTTL: time.Second, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for !store.Claimed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	// The executor has returned, but the durable task remains leased/running
	// for normal expiry-and-reclaim; no generic terminal write is permitted.
	time.Sleep(25 * time.Millisecond)
	select {
	case got := <-store.fail:
		t.Fatalf("deferred task failed: %+v", got)
	case got := <-store.complete:
		t.Fatalf("deferred task completed: %+v", got)
	default:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v", err)
	}
}

func TestWorkerStopStopsClaimsAndJoins(t *testing.T) {
	store := &fakeTaskStore{fail: make(chan task.FailCommand, 1), complete: make(chan task.CompleteCommand, 1)}
	w, err := New(Config{Store: store, Executor: fakeExecutor{}, MaxConcurrent: 1, LeaseTTL: time.Second, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	deadline := time.Now().Add(time.Second)
	for !store.Claimed() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if err := w.StopWithContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatalf("run error=%v", err)
	}
}

func TestWorkerTaskTimeoutUsesDurableTimeoutTransition(t *testing.T) {
	store := &fakeTaskStore{
		fail:     make(chan task.FailCommand, 1),
		complete: make(chan task.CompleteCommand, 1),
		timeout:  make(chan task.TimeoutCommand, 1),
		task:     task.Task{ID: "task", Revision: 1, Spec: task.TaskSpec{TimeoutSeconds: 1}},
	}
	executor := blockingExecutor{started: make(chan struct{}), canceled: make(chan struct{})}
	w, err := New(Config{Store: store, Executor: executor, MaxConcurrent: 1, LeaseTTL: 3 * time.Second, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- w.Run(ctx) }()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	select {
	case <-store.timeout:
		cancel()
	case <-time.After(2 * time.Second):
		t.Fatal("task timeout was not persisted")
	}
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("run error=%v", err)
	}
}

func TestWorkerGraceTimeoutCancelsActiveWithoutTerminalFailure(t *testing.T) {
	store := &fakeTaskStore{fail: make(chan task.FailCommand, 1), complete: make(chan task.CompleteCommand, 1)}
	executor := blockingExecutor{started: make(chan struct{}), canceled: make(chan struct{})}
	w, err := New(Config{Store: store, Executor: executor, MaxConcurrent: 1, LeaseTTL: time.Second, Holder: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()
	select {
	case <-executor.started:
	case <-time.After(time.Second):
		t.Fatal("executor did not start")
	}
	grace, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := w.StopWithContext(grace); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop error=%v", err)
	}
	select {
	case <-executor.canceled:
	case <-time.After(time.Second):
		t.Fatal("active execution was not canceled after grace")
	}
	select {
	case failed := <-store.fail:
		t.Fatalf("canceled execution must remain lease-recoverable, got failure %#v", failed)
	default:
	}
	if err := <-done; err != nil {
		t.Fatalf("run error=%v", err)
	}
}
