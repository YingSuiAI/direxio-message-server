// Package runtime contains the in-process bounded task runtime.  It is kept
// deliberately small: scheduling/materialization and task execution are
// supplied through ports, while this package owns claiming, lease fencing and
// shutdown semantics.
package runtime

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

var (
	// ErrExecutionUncertain is terminal by design.  A provider/tool call may
	// have committed a side effect before its response was lost; retrying it
	// would be unsafe, so the worker records a terminal failure instead.
	ErrExecutionUncertain = errors.New("task execution outcome is uncertain")
	ErrWorkerStopped      = errors.New("task worker stopped")
	// ErrTaskFinalized tells the common worker that a domain coordinator
	// committed the task terminal state, confirmation release and domain
	// terminal state in one transaction. The worker must not issue a second
	// generic completion write.
	ErrTaskFinalized = errors.New("task finalized by domain coordinator")
)

// Executor is the only execution dependency of the worker.  It must not
// launch subprocesses or cross a transport/RPC boundary.
type Executor interface {
	Execute(context.Context, task.Task) (task.Result, error)
}

// Store is the canonical task persistence/fencing surface.  ClaimNextDue is
// intentionally kept as the queue selection port; every mutation after claim
// uses task.Store's lease-fenced commands.
type Store interface {
	task.TaskQueueRepository
	RenewLease(context.Context, task.RenewLeaseCommand) (task.Lease, error)
	CompleteTask(context.Context, task.CompleteCommand) (task.Task, error)
	FailTask(context.Context, task.FailCommand) error
	TimeoutTask(context.Context, task.TimeoutCommand) error
}

// CanonicalStore is the complete task.Store contract.  Runtime adapters are
// expected to expose this contract (plus queue selection) even though the
// worker itself only needs the lease-fenced execution subset above.
type CanonicalStore interface {
	task.Store
}

// Reclaimer is optional because queue adapters may reclaim expired rows in
// ClaimNextDue's transaction.  When present, it is called before claiming so
// a crashed worker's lease is fenced to a new attempt.
type Reclaimer interface {
	ReclaimExpired(context.Context, string, time.Time, time.Duration, int) error
}

type Config struct {
	Store         Store
	Executor      Executor
	MaxConcurrent int
	LeaseTTL      time.Duration
	Holder        string
}

type Worker struct {
	store           Store
	executor        Executor
	maxConcurrent   int
	leaseTTL        time.Duration
	holder          string
	stop            chan struct{}
	started         chan struct{}
	stopped         chan struct{}
	startOnce       sync.Once
	stopOnce        sync.Once
	runCancel       context.CancelFunc
	executionCancel context.CancelFunc
	cancelMu        sync.Mutex
	wg              sync.WaitGroup
	active          atomic.Int64
}

func New(c Config) (*Worker, error) {
	if c.Store == nil || c.Executor == nil || c.MaxConcurrent <= 0 || c.LeaseTTL <= 0 {
		return nil, errors.New("invalid task runtime dependencies")
	}
	holder := strings.TrimSpace(c.Holder)
	if holder == "" {
		holder = uuid.NewString()
	}
	return &Worker{store: c.Store, executor: c.Executor, maxConcurrent: c.MaxConcurrent, leaseTTL: c.LeaseTTL, holder: holder, stop: make(chan struct{}), started: make(chan struct{}), stopped: make(chan struct{})}, nil
}

func (w *Worker) Holder() string {
	if w == nil {
		return ""
	}
	return w.holder
}
func (w *Worker) Active() int {
	if w == nil {
		return 0
	}
	return int(w.active.Load())
}

// Run stops claiming as soon as Stop is requested, but leaves already claimed
// executions running during the caller-provided grace period.  Context
// cancellation still interrupts active work and leaves its lease recoverable.
func (w *Worker) Run(ctx context.Context) (runErr error) {
	if w == nil {
		return errors.New("nil task worker")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.startOnce.Do(func() { close(w.started) })
	runCtx, cancel := context.WithCancel(ctx)
	executionCtx, executionCancel := context.WithCancel(ctx)
	w.cancelMu.Lock()
	w.runCancel = cancel
	w.executionCancel = executionCancel
	w.cancelMu.Unlock()
	defer cancel()
	defer func() {
		select {
		case <-w.stop:
			// StopWithContext owns the bounded grace period for active work.
		default:
			executionCancel()
		}
	}()
	defer close(w.stopped)
	for {
		select {
		case <-w.stop:
			return nil
		case <-runCtx.Done():
			select {
			case <-w.stop:
				return nil
			default:
				return runCtx.Err()
			}
		default:
		}
		if int(w.active.Load()) >= w.maxConcurrent {
			if !wait(runCtx, 5*time.Millisecond) {
				select {
				case <-w.stop:
					return nil
				default:
				}
				return runCtx.Err()
			}
			continue
		}
		now := time.Now().UTC()
		if r, ok := w.store.(Reclaimer); ok {
			if err := r.ReclaimExpired(runCtx, w.holder, now, w.leaseTTL, w.maxConcurrent); err != nil && !errors.Is(err, task.ErrNotFound) {
				return err
			}
		}
		// maxConcurrent is the cluster-wide durable slot limit. It must never
		// be reduced to this process's remaining local capacity: ClaimNextDue
		// persists the supplied value while holding the shared slot row, so a
		// local in-flight task must not accidentally shrink the global limit.
		claimed, lease, err := w.store.ClaimNextDue(runCtx, w.holder, now, w.leaseTTL, w.maxConcurrent)
		if err != nil {
			if errors.Is(err, task.ErrNotFound) || errors.Is(err, task.ErrConflict) {
				if !wait(runCtx, 20*time.Millisecond) {
					select {
					case <-w.stop:
						return nil
					default:
					}
					return runCtx.Err()
				}
				continue
			}
			return err
		}
		w.active.Add(1)
		w.wg.Add(1)
		go func() {
			defer w.active.Add(-1)
			defer w.wg.Done()
			// Stop cancels only the claim loop; active work receives the
			// caller context and is allowed to finish during the grace period.
			w.execute(executionCtx, claimed, lease)
		}()
	}
}

func wait(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// StopWithContext implements stop-claim plus a grace period.  It never turns
// an interrupted provider call into a durable failure.
func (w *Worker) StopWithContext(ctx context.Context) error {
	if w == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.stopOnce.Do(func() { close(w.stop) })
	w.cancelMu.Lock()
	if w.runCancel != nil {
		w.runCancel()
	}
	w.cancelMu.Unlock()
	select {
	case <-w.started:
	case <-ctx.Done():
		return ctx.Err()
	default:
		// Stop before Run is a valid no-op and must not wait for a future
		// caller to start the worker.
		return nil
	}
	select {
	case <-w.stopped:
	case <-ctx.Done():
		return ctx.Err()
	}
	done := make(chan struct{})
	go func() { w.wg.Wait(); close(done) }()
	select {
	case <-done:
		w.cancelExecutions()
		return nil
	case <-ctx.Done():
		// The grace period elapsed. Interrupt the in-process call without
		// writing a terminal state; its lease remains recoverable.
		w.cancelExecutions()
		return ctx.Err()
	}
}

func (w *Worker) cancelExecutions() {
	w.cancelMu.Lock()
	cancel := w.executionCancel
	w.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (w *Worker) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return w.StopWithContext(ctx)
}

func (w *Worker) execute(parent context.Context, t task.Task, lease task.Lease) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if t.Spec.TimeoutSeconds > 0 {
		var timeoutCancel context.CancelFunc
		ctx, timeoutCancel = context.WithTimeout(ctx, time.Duration(t.Spec.TimeoutSeconds)*time.Second)
		defer timeoutCancel()
	}
	fence := task.Fence{TaskID: t.ID, Attempt: lease.Attempt, LeaseEpoch: lease.Epoch, ExpectedRevision: t.Revision}
	var fenceMu sync.Mutex
	currentFence := func() task.Fence { fenceMu.Lock(); defer fenceMu.Unlock(); return fence }
	leaseCtx, leaseCancel := context.WithCancel(ctx)
	defer leaseCancel()
	var stale atomic.Bool
	interval := w.leaseTTL / 3
	if interval <= 0 {
		interval = time.Second
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-leaseCtx.Done():
				return
			case at := <-ticker.C:
				before := currentFence()
				next, err := w.store.RenewLease(leaseCtx, task.RenewLeaseCommand{Fence: before, Holder: w.holder, LeaseTTL: w.leaseTTL, At: at.UTC()})
				if err != nil {
					stale.Store(true)
					cancel()
					return
				}
				fenceMu.Lock()
				fence.ExpectedRevision++
				fence.LeaseEpoch = next.Epoch
				fenceMu.Unlock()
			}
		}
	}()
	result, err := w.executor.Execute(ctx, t)
	leaseCancel()
	if stale.Load() {
		return
	}
	if errors.Is(err, ErrTaskFinalized) {
		return
	}
	if err == nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		err = context.DeadlineExceeded
	}
	uncertain := errors.Is(err, ErrExecutionUncertain)
	if !uncertain && (parent.Err() != nil || errors.Is(err, context.Canceled)) {
		return
	}
	writeCtx, writeCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer writeCancel()
	at := time.Now().UTC()
	if err == nil {
		_, _ = w.store.CompleteTask(writeCtx, task.CompleteCommand{Fence: currentFence(), Result: result, At: at})
		return
	}
	if errors.Is(err, context.DeadlineExceeded) {
		_ = w.store.TimeoutTask(writeCtx, task.TimeoutCommand{Fence: currentFence(), At: at})
		return
	}
	code, summary := "task_execution_failed", boundedSummary(err.Error())
	if errors.Is(err, ErrExecutionUncertain) {
		code, summary = "execution_uncertain", "task execution outcome is uncertain; automatic retry is forbidden"
	}
	_ = w.store.FailTask(writeCtx, task.FailCommand{Fence: currentFence(), ErrorCode: code, ErrorSummary: summary, At: at})
}

func boundedSummary(s string) string {
	if len([]byte(s)) <= task.MaxSummaryBytes {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		part := string(r)
		if b.Len()+len([]byte(part)) > task.MaxSummaryBytes {
			break
		}
		b.WriteString(part)
	}
	return b.String()
}

// ScheduleMaterializer is the transactional schedule boundary.  An adapter
// must atomically create occurrence + generic task + legacy schedule-run
// projection, and advance the schedule cursor.  The runtime never executes a
// schedule's prompt itself.
type ScheduleMaterializer interface {
	MaterializeNextDue(context.Context, time.Time, task.CronCalculator) (bool, error)
}

type ScheduleLoop struct {
	store      ScheduleMaterializer
	calculator task.CronCalculator
	interval   time.Duration
	started    chan struct{}
	done       chan struct{}
	startOnce  sync.Once
}

func NewScheduleLoop(store ScheduleMaterializer, calculator task.CronCalculator, interval time.Duration) (*ScheduleLoop, error) {
	if store == nil || calculator == nil || interval <= 0 {
		return nil, errors.New("invalid schedule loop dependencies")
	}
	return &ScheduleLoop{store: store, calculator: calculator, interval: interval, started: make(chan struct{}), done: make(chan struct{})}, nil
}
func (l *ScheduleLoop) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	l.startOnce.Do(func() { close(l.started) })
	defer close(l.done)
	if err := l.drain(ctx, time.Now().UTC()); err != nil {
		return err
	}
	t := time.NewTicker(l.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case at := <-t.C:
			if err := l.drain(ctx, at.UTC()); err != nil {
				return err
			}
		}
	}
}
func (l *ScheduleLoop) Wait(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-l.started:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-l.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (l *ScheduleLoop) drain(ctx context.Context, at time.Time) error {
	for {
		materialized, err := l.store.MaterializeNextDue(ctx, at, l.calculator)
		if err != nil {
			return err
		}
		if !materialized {
			return nil
		}
	}
}

// Ensure the compile-time contract remains explicit for adapters implementing
// the canonical task.Store plus queue selector.
// CronCalculator delegates to the canonical task schedule parser.
type CronCalculator struct{}

func (CronCalculator) Next(after time.Time, expression, timezone string) (time.Time, error) {
	return task.NextCron(after, expression, timezone)
}
