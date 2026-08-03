package execution

// This file is deliberately an in-memory Phase 2 harness.  It models the
// aggregate transaction boundaries which the PostgreSQL adapter will own, but
// is not wired into the embedded worker, actions, or capability list.

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

var (
	ErrConflict  = errors.New("execution coordinator: conflict")
	ErrNotFound  = errors.New("execution coordinator: not found")
	ErrUncertain = errors.New("execution coordinator: uncertain")
)

type MemoryCoordinatorStore struct {
	mu             sync.Mutex
	plans          map[string]ExecutionPlan
	runs           map[string]ExecutionRun
	stages         map[string]RunStage
	tasks          map[string]coretask.Task
	confirmations  map[string]coreconfirmation.Confirmation
	reservations   map[string]coreconfirmation.Reservation
	attempts       map[string][]StepAttempt
	receipts       map[string]Receipt
	events         map[string][]Event
	leases         map[string]memoryTargetLease
	replays        map[string]memoryReplay
	confirmReplays map[string]memoryConfirmationReplay
	resolutions    map[string][]memoryReconciliationResolution
}

// memoryReconciliationResolution is append-only outcome evidence for one
// exact uncertain lease. The queryable run/stage aggregate is promoted only
// after this record is appended.
type memoryReconciliationResolution struct {
	RunID, StageID, LeaseToken string
	LeaseEpoch                 uint64
	Outcome                    StageStatus
	OutcomeDigest              Digest
	Succeeded                  bool
	At                         time.Time
}

type memoryReplay struct {
	digest Digest
	runID  string
}
type memoryConfirmationReplay struct {
	digest, confirmationID string
	value                  coreconfirmation.Confirmation
}
type memoryTargetLease struct {
	token          string
	epoch          uint64
	runID, stageID string
	uncertain      bool
	expiresAt      time.Time
}

func NewMemoryCoordinatorStore(plans ...ExecutionPlan) (*MemoryCoordinatorStore, error) {
	s := &MemoryCoordinatorStore{plans: map[string]ExecutionPlan{}, runs: map[string]ExecutionRun{}, stages: map[string]RunStage{}, tasks: map[string]coretask.Task{}, confirmations: map[string]coreconfirmation.Confirmation{}, reservations: map[string]coreconfirmation.Reservation{}, attempts: map[string][]StepAttempt{}, receipts: map[string]Receipt{}, events: map[string][]Event{}, leases: map[string]memoryTargetLease{}, replays: map[string]memoryReplay{}, confirmReplays: map[string]memoryConfirmationReplay{}, resolutions: map[string][]memoryReconciliationResolution{}}
	for _, p := range plans {
		n, err := p.Normalize()
		if err != nil {
			return nil, err
		}
		s.plans[n.ID] = clonePlan(n)
	}
	return s, nil
}

type FakeMode string

const (
	FakeSuccess                FakeMode = "success"
	FakeFailBeforeDispatch     FakeMode = "fail_before_dispatch"
	FakeUncertainAfterDispatch FakeMode = "uncertain_after_dispatch"
	FakeBlocking               FakeMode = "blocking"
	FakeReconcileSuccess       FakeMode = "reconcile_success"
	FakeReconcileFailed        FakeMode = "reconcile_failed"
	FakeErrorAfterDispatch     FakeMode = "error_after_dispatch"
	FakeReconcileCanceled      FakeMode = "reconcile_canceled"
	FakeReconcilePending       FakeMode = "reconcile_pending"
	FakeReconcileUnknown       FakeMode = "reconcile_unknown"
)

type FakeExecutor struct {
	Mode, UncertainOnDispatch FakeMode
	Started                   chan struct{}
	Continue                  <-chan struct{}
	Dispatches                *int
}
type fakeResult struct {
	providerOperation, commandID string
	failed, uncertain            bool
	reconciled                   StageStatus
}

func (f FakeExecutor) execute(ctx context.Context, stage RunStage) (fakeResult, error) {
	dispatch := 0
	if f.Dispatches != nil {
		*f.Dispatches++
		dispatch = *f.Dispatches
	}
	if f.Mode == FakeBlocking {
		if f.Started != nil {
			select {
			case f.Started <- struct{}{}:
			default:
			}
		}
		if f.Continue != nil {
			select {
			case <-f.Continue:
			case <-ctx.Done():
				return fakeResult{}, ctx.Err()
			}
		}
	}
	r := fakeResult{providerOperation: deterministicID("provider:" + stage.StageID), commandID: deterministicID("command:" + stage.StageID)}
	switch f.Mode {
	case FakeFailBeforeDispatch:
		return fakeResult{failed: true}, nil
	case FakeErrorAfterDispatch:
		return fakeResult{}, errors.New("provider result lost after dispatch")
	case FakeUncertainAfterDispatch:
		r.uncertain = true
	}
	if f.UncertainOnDispatch != "" && dispatch == 2 {
		r.uncertain = true
	}
	return r, nil
}
func (f FakeExecutor) reconcile(_ context.Context, _ RunStage) fakeResult {
	switch f.Mode {
	case FakeReconcileSuccess:
		return fakeResult{reconciled: StageSucceeded}
	case FakeReconcileFailed:
		return fakeResult{reconciled: StageFailed}
	case FakeReconcileCanceled:
		return fakeResult{reconciled: StageCanceled}
	case FakeReconcilePending, FakeReconcileUnknown:
		fallthrough
	default:
		// A pending, running, or unknown typed readback is not a terminal
		// provider fact. The caller must leave the original uncertain evidence
		// fenced for a later readback-only reconciliation.
		return fakeResult{}
	}
}

type MemoryCoordinator struct {
	store    *MemoryCoordinatorStore
	executor FakeExecutor
	now      func() time.Time
	leaseTTL time.Duration
}

func NewMemoryCoordinator(store *MemoryCoordinatorStore, executor FakeExecutor, now ...func() time.Time) (*MemoryCoordinator, error) {
	if store == nil {
		return nil, ErrConflict
	}
	clock := time.Now
	if len(now) > 0 && now[0] != nil {
		clock = now[0]
	}
	return &MemoryCoordinator{store: store, executor: executor, now: clock, leaseTTL: time.Minute}, nil
}

type CreateRunCommand struct {
	OwnerID, PlanID, IdempotencyKey string
	PlanRevision                    uint64
	Operation                       RunOperation
	TriggerKind                     TriggerKind
	RollbackOfRunID                 string
}
type RunMaterialization struct {
	Run           ExecutionRun
	Stages        []RunStage
	Tasks         []coretask.Task
	Confirmations []coreconfirmation.Confirmation
}

func (c *MemoryCoordinator) CreateRun(_ context.Context, in CreateRunCommand) (RunMaterialization, error) {
	if c == nil || !ValidateUUID(in.PlanID) || in.PlanRevision == 0 || !coretask.ValidUUID(in.IdempotencyKey) {
		return RunMaterialization{}, ErrConflict
	}
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	p, ok := c.store.plans[in.PlanID]
	if !ok || p.OwnerID != in.OwnerID || p.Status != PlanReady || !p.ExpiresAt.After(c.now().UTC()) {
		return RunMaterialization{}, ErrConflict
	}
	if in.PlanRevision != p.Revision || (in.TriggerKind != "" && !validTriggerKind(in.TriggerKind)) {
		return RunMaterialization{}, ErrConflict
	}
	if in.Operation == "" {
		in.Operation = RunOperationExecute
	}
	if !validRunOperation(in.Operation) || !RunOperationAllowedByPlan(p, in.Operation) {
		return RunMaterialization{}, ErrConflict
	}
	if in.Operation != RunOperationRollback && in.RollbackOfRunID != "" {
		return RunMaterialization{}, ErrConflict
	}
	if in.TriggerKind == "" {
		in.TriggerKind = TriggerManual
	}
	if in.Operation == RunOperationRollback && in.TriggerKind == TriggerManual {
		in.TriggerKind = TriggerRollback
	}
	if in.Operation == RunOperationRollback {
		if !ValidateUUID(in.RollbackOfRunID) {
			return RunMaterialization{}, ErrConflict
		}
		source, found := c.store.runs[in.RollbackOfRunID]
		if !found || source.OwnerID != in.OwnerID || source.PlanID != p.ID || source.PlanRevision != p.Revision {
			return RunMaterialization{}, ErrConflict
		}
	}
	// Authorization is deliberately before replay.  A foreign owner must not
	// learn that an idempotency key exists, and the replay digest covers every
	// command field that changes the durable aggregate.
	replayKey := in.OwnerID + "/" + in.IdempotencyKey
	replayDigest := createRunReplayDigestForPlan(in, p)
	if old, ok := c.store.replays[replayKey]; ok {
		if old.digest != replayDigest {
			return RunMaterialization{}, ErrConflict
		}
		return c.materializationLocked(old.runID), nil
	}
	rid := DeterministicRunID(in.OwnerID, in.IdempotencyKey)
	at := c.now().UTC()
	r := ExecutionRun{RunID: rid, OwnerID: p.OwnerID, Operation: in.Operation, TriggerKind: in.TriggerKind, RollbackOfRunID: in.RollbackOfRunID, DeploymentID: p.DeploymentID, PlanID: p.ID, ProjectID: p.ProjectID, Purpose: p.Purpose, PlanRevision: p.Revision, PlanDigest: p.Digest, Status: RunPending, Revision: 1, CreatedAt: at, UpdatedAt: at}
	r.RunDigest = mustDigest(struct {
		Plan Digest
		ID   string
	}{p.Digest, rid})
	c.store.runs[rid] = r
	stagePlans := p.Stages
	if in.Operation == RunOperationRollback {
		source := c.store.runs[in.RollbackOfRunID]
		applied := map[string]bool{}
		for _, ss := range c.store.stages {
			if ss.RunID == source.RunID && ss.Status == StageSucceeded {
				applied[ss.StageKey] = true
			}
		}
		stagePlans = append([]ExecutionStage(nil), p.Stages...)
		stagePlans = sortStagesReverseDependency(stagePlans, applied)
	}
	for i, ps := range stagePlans {
		sid := DeterministicStageID(rid, ps.StageKey)
		rs := RunStage{StageID: sid, RunID: rid, OwnerID: p.OwnerID, PlanID: p.ID, StageKey: ps.StageKey, PlanRevision: p.Revision, RunRevision: r.Revision, StageRevision: ps.Revision, StageDigest: ps.Digest, TargetID: ps.TargetID, TargetRevision: ps.TargetRevision, TargetDigest: ps.TargetDigest, Ordinal: uint64(i + 1), StageIdempotencyKey: deterministicID("execution:v2:stage-idem:" + rid + ":" + ps.StageKey), Status: StageBlocked, CreatedAt: at, UpdatedAt: at}
		rs.StageIdempotencyKey = DeterministicStageIdempotencyKey(sid, in.Operation)
		if in.Operation == RunOperationRollback && len(ps.RollbackSteps) == 0 {
			rs.Status = StageSkipped
			rs.StartedAt = at
			rs.FinishedAt = at
		}
		c.store.stages[stageMapKey(rid, sid)] = rs
	}
	c.promoteLocked(rid, at)
	c.store.replays[replayKey] = memoryReplay{replayDigest, rid}
	return c.materializationLocked(rid), nil
}

type ConfirmStageCommand struct {
	OwnerID, ConfirmationID, IdempotencyKey string
	ExpectedRevision                        int64
}

// RejectStageCommand, CancelRunCommand, and RetryRunCommand are deliberately
// narrow aggregate commands.  Their UUID idempotency keys are server-digested
// by durable coordinators; callers never supply a digest.
type RejectStageCommand struct {
	OwnerID, ConfirmationID, IdempotencyKey string
	ExpectedRevision                        int64
}

type CancelRunCommand struct {
	OwnerID, RunID, IdempotencyKey string
	ExpectedRevision               uint64
}

type RetryRunCommand struct {
	OwnerID, RunID, IdempotencyKey string
	ExpectedRevision               uint64
}

func (c *MemoryCoordinator) ConfirmStage(_ context.Context, in ConfirmStageCommand) (coreconfirmation.Confirmation, error) {
	if c == nil || !coretask.ValidUUID(in.ConfirmationID) || !coretask.ValidUUID(in.IdempotencyKey) || in.ExpectedRevision < 1 {
		return coreconfirmation.Confirmation{}, ErrConflict
	}
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	at := c.now().UTC()
	conf, ok := c.store.confirmations[in.ConfirmationID]
	// Owner authorization precedes replay lookup.  Replay identities are
	// owner-scoped and server-digested rather than trusting a caller digest.
	if !ok || conf.OwnerID != in.OwnerID {
		return coreconfirmation.Confirmation{}, ErrConflict
	}
	replayKey := in.OwnerID + "/" + in.IdempotencyKey
	replayDigest := confirmationReplayDigest(in, conf.Binding)
	if replay, ok := c.store.confirmReplays[replayKey]; ok {
		if replay.digest != string(replayDigest) || replay.confirmationID != in.ConfirmationID {
			return coreconfirmation.Confirmation{}, ErrConflict
		}
		return cloneConfirmation(replay.value), nil
	}
	if conf.State != coreconfirmation.StatePending || conf.Revision != in.ExpectedRevision {
		return coreconfirmation.Confirmation{}, ErrConflict
	}
	rs, plan, err := c.authoritativeLocked(conf)
	if err != nil || !conf.ExpiresAt.After(at) {
		return coreconfirmation.Confirmation{}, ErrConflict
	}
	if conf.Binding.Digest == "" || !conf.Binding.Equal(c.authoritativeBindingLocked(conf, plan, rs)) {
		return coreconfirmation.Confirmation{}, ErrConflict
	}
	t := c.store.tasks[rs.TaskID]
	if t.Status != coretask.StatusWaitingUser {
		return coreconfirmation.Confirmation{}, ErrConflict
	}
	conf.State = coreconfirmation.StateConfirmed
	conf.Revision++
	conf.UpdatedAt = at
	c.store.confirmations[conf.ID] = conf
	t.Status = coretask.StatusQueued
	t.Revision++
	t.UpdatedAt = at
	c.store.tasks[t.ID] = t
	rs.Status = StageQueued
	rs.UpdatedAt = at
	c.store.stages[stageMapKey(rs.RunID, rs.StageID)] = rs
	c.updateRunLocked(rs.RunID, RunQueued, rs.StageID, at)
	c.eventLocked(rs.RunID, rs.StageID, "confirmation_confirmed", StageQueued, at)
	c.store.confirmReplays[replayKey] = memoryConfirmationReplay{digest: string(replayDigest), confirmationID: in.ConfirmationID, value: conf}
	return cloneConfirmation(conf), nil
}

// ExecuteClaimedStage claims one already queued stage and owns every later
// transition. No generic task store is involved.
func (c *MemoryCoordinator) ExecuteClaimedStage(ctx context.Context, owner, taskID, holder string, at time.Time) error {
	if c == nil || owner == "" || !coretask.ValidUUID(taskID) || holder == "" {
		return ErrConflict
	}
	c.store.mu.Lock()
	now := c.at(at)
	t, ok := c.store.tasks[taskID]
	if !ok || t.OwnerID != owner || t.Status != coretask.StatusQueued {
		c.store.mu.Unlock()
		return ErrConflict
	}
	rs, ok := c.stageForTaskLocked(taskID)
	if !ok || rs.Status != StageQueued {
		c.store.mu.Unlock()
		return ErrConflict
	}
	p := c.store.plans[t.Spec.Payload.ExecutionStage.PlanID]
	// Validate every immutable fence, including the confirmation's current
	// authoritative binding, before acquiring a target lease or changing any
	// task/stage/run state.  This is the aggregate's claim-and-consume point.
	if err := c.authoritativeStageLocked(t, rs, p, now); err != nil {
		c.store.mu.Unlock()
		return err
	}
	if err := c.acquireLeaseLocked(rs, now); err != nil {
		c.store.mu.Unlock()
		return err
	}
	t.Status = coretask.StatusRunning
	t.Attempt++
	// The target lease owns the monotonic epoch across runs; task-local
	// counters must fence against that exact epoch after a prior run releases
	// the target.
	t.LeaseEpoch = c.store.leases[rs.TargetID].epoch
	t.Lease = &coretask.Lease{TaskID: t.ID, Attempt: t.Attempt, Epoch: t.LeaseEpoch, Holder: holder, ExpiresAt: now.Add(c.leaseTTL)}
	t.Revision++
	t.UpdatedAt = now
	c.store.tasks[taskID] = t
	rs.Status = StageRunning
	if rs.StartedAt.IsZero() {
		rs.StartedAt = now
		rs.UpdatedAt = now
		c.store.stages[stageMapKey(rs.RunID, rs.StageID)] = rs
	}
	c.updateRunLocked(rs.RunID, RunRunning, rs.StageID, now)
	if rs.ConfirmationID != "" {
		conf := c.store.confirmations[rs.ConfirmationID]
		conf.State = coreconfirmation.StateConsumed
		conf.Revision++
		conf.UpdatedAt = now
		c.store.confirmations[conf.ID] = conf
		c.store.reservations[conf.ID] = coreconfirmation.Reservation{ConfirmationID: conf.ID, TaskID: t.ID, AcquiredAttempt: t.Attempt, AcquiredLeaseEpoch: t.LeaseEpoch, TaskRevision: int64(t.Revision), Active: true}
	}
	c.eventLocked(rs.RunID, rs.StageID, "stage_started", StageRunning, now)
	// Capture immutable execution selection while holding the aggregate lock;
	// no map read is permitted after the provider call begins.
	run := cloneRun(c.store.runs[rs.RunID])
	steps := cloneSteps(c.stepsForRunLocked(run, p, rs))
	c.store.mu.Unlock()
	for _, step := range steps {
		// Persist the dispatch intent/fence before the external call.  Therefore
		// a cancellation, transport error, or process loss after this point is
		// conservatively uncertain and cannot enable a later step.
		c.store.mu.Lock()
		now = c.at(time.Time{})
		if err := c.assertLeaseLocked(rs, taskID, holder); err != nil {
			c.store.mu.Unlock()
			return err
		}
		a := StepAttempt{AttemptID: uuid.NewString(), RunID: rs.RunID, StageID: rs.StageID, PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, StageRevision: rs.StageRevision, StageDigest: rs.StageDigest, StepRevision: 1, StepDigest: step.Digest, StepKey: step.StepKey, Attempt: uint64(t.Attempt), OwnerID: owner, Revision: 1, Status: AttemptRunning, CreatedAt: now, StartedAt: now, UpdatedAt: now}
		rec := Receipt{ReceiptID: uuid.NewString(), RunID: rs.RunID, OwnerID: owner, Revision: 1, AttemptID: a.AttemptID, Status: ReceiptAccepted, IdempotencyKey: step.IdempotencyMarker, ProviderOperation: deterministicID("dispatch-intent:" + a.AttemptID), At: now}
		a.ReceiptID = rec.ReceiptID
		c.store.attempts[rs.StageID] = append(c.store.attempts[rs.StageID], a)
		c.store.receipts[rec.ReceiptID] = rec
		c.fenceDispatchIntentLocked(rs)
		c.eventLocked(rs.RunID, rs.StageID, "dispatch_intent", StageRunning, now)
		c.store.mu.Unlock()
		result, err := c.executor.execute(ctx, rs)
		c.store.mu.Lock()
		now = c.at(time.Time{})
		if err != nil {
			if c.assertLeaseLocked(rs, taskID, holder) == nil {
				c.finalizeLocked(rs, t, AttemptUncertain, StageUncertain, RunUncertain, "dispatch_result_unknown", now, true)
			}
			c.store.mu.Unlock()
			return ErrUncertain
		}
		if err := c.assertLeaseLocked(rs, taskID, holder); err != nil {
			c.store.mu.Unlock()
			return err
		}
		if result.failed {
			// A failed result is the executor's explicit promise that it did not
			// dispatch.  Remove the intent rather than representing a side effect.
			c.removeLatestIntentLocked(rs.StageID)
			c.finalizeLocked(rs, t, AttemptFailed, StageFailed, RunFailed, "failed_before_dispatch", now, false)
			c.store.mu.Unlock()
			return nil
		}
		c.updateLatestReceiptLocked(rs.StageID, result)
		if result.uncertain {
			c.finalizeLocked(rs, t, AttemptUncertain, StageUncertain, RunUncertain, "provider_uncertain", now, true)
			c.store.mu.Unlock()
			return ErrUncertain
		}
		c.markLatestAttemptLocked(rs.StageID, AttemptSucceeded, now)
		c.store.mu.Unlock()
	}
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	now = c.at(time.Time{})
	if err := c.assertLeaseLocked(rs, taskID, holder); err != nil {
		return err
	}
	c.finalizeLocked(rs, t, AttemptSucceeded, StageSucceeded, RunRunning, "", now, false)
	c.promoteLocked(rs.RunID, now)
	return nil
}

func (c *MemoryCoordinator) ReconcileStage(_ context.Context, owner, runID, stageID string, at time.Time) (RunStage, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	key := stageMapKey(runID, stageID)
	rs, ok := c.store.stages[key]
	if !ok || rs.OwnerID != owner {
		return RunStage{}, ErrConflict
	}
	for _, resolution := range c.store.resolutions[runID] {
		if resolution.StageID == stageID {
			// Reconciliation is idempotent after the exact immutable evidence has
			// been resolved. It never performs a second provider readback or
			// inserts a second resolution successor.
			return cloneStage(rs), nil
		}
	}
	if rs.Status != StageUncertain {
		return RunStage{}, ErrConflict
	}
	l := c.store.leases[rs.TargetID]
	t := c.store.tasks[rs.TaskID]
	if !l.uncertain || l.runID != runID || l.stageID != stageID || t.OwnerID != owner {
		return RunStage{}, ErrConflict
	}
	// Keep the exact uncertain lease fence through reconciliation.  A different
	// run cannot resolve this evidence merely because wall-clock time advanced.
	token, epoch := l.token, l.epoch
	r := c.executor.reconcile(context.Background(), rs)
	current := c.store.leases[rs.TargetID]
	if current.token != token || current.epoch != epoch || current.runID != runID || current.stageID != stageID || !current.uncertain {
		return RunStage{}, ErrConflict
	}
	status := r.reconciled
	if status != StageSucceeded && status != StageFailed && status != StageCanceled {
		// Pending/running/unknown readback is not a typed terminal fact. Keep
		// the original uncertain attempt, receipt, stage, run, and lease intact.
		return RunStage{}, ErrConflict
	}
	resolvedAt := c.at(at)
	outcomeDigest := reconciliationOutcomeDigest(rs, status)
	if err := c.recordResolutionLocked(rs, token, epoch, status, outcomeDigest, resolvedAt); err != nil {
		return RunStage{}, err
	}
	var attemptStatus AttemptStatus
	var runStatus RunStatus
	reason := ""
	switch status {
	case StageSucceeded:
		attemptStatus, runStatus = AttemptSucceeded, RunSucceeded
	case StageFailed:
		attemptStatus, runStatus, reason = AttemptFailed, RunFailed, "stage_failed"
	case StageCanceled:
		attemptStatus, runStatus, reason = AttemptCanceled, RunCanceled, "provider_operation_canceled"
	}
	c.resolveUncertainLocked(rs, t, attemptStatus, status, runStatus, reason, outcomeDigest, resolvedAt)
	return cloneStage(c.store.stages[key]), nil
}

func (c *MemoryCoordinator) GetRun(_ context.Context, owner, id string) (ExecutionRun, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	r, ok := c.store.runs[id]
	if !ok || r.OwnerID != owner {
		return ExecutionRun{}, ErrNotFound
	}
	return cloneRun(r), nil
}
func (c *MemoryCoordinator) GetStage(_ context.Context, owner, runID, stageID string) (RunStage, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	r, ok := c.store.stages[stageMapKey(runID, stageID)]
	if !ok || r.OwnerID != owner {
		return RunStage{}, ErrNotFound
	}
	return cloneStage(r), nil
}
func (c *MemoryCoordinator) GetConfirmation(_ context.Context, owner, id string) (coreconfirmation.Confirmation, error) {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	v, ok := c.store.confirmations[id]
	if !ok || v.OwnerID != owner {
		return coreconfirmation.Confirmation{}, ErrNotFound
	}
	return cloneConfirmation(v), nil
}
func (c *MemoryCoordinator) Attempts(_ context.Context, owner, stageID string) []StepAttempt {
	c.store.mu.Lock()
	defer c.store.mu.Unlock()
	out := append([]StepAttempt(nil), c.store.attempts[stageID]...)
	for i := range out {
		if out[i].OwnerID != owner {
			return nil
		}
	}
	return out
}

func (c *MemoryCoordinator) promoteLocked(runID string, at time.Time) {
	r := c.store.runs[runID]
	all := true
	waiting := false
	queued := false
	for _, key := range c.stageKeysLocked(runID) {
		rs := c.store.stages[key]
		if rs.Status == StageBlocked && c.dependenciesDoneLocked(runID, rs) {
			p := c.store.plans[rs.PlanID]
			st := p.stage(rs.StageKey)
			confirm := st.Risk != RiskR0 && st.Risk != RiskR1
			if c.store.runs[runID].Operation == RunOperationRollback {
				confirm = st.RollbackPolicy != nil && st.RollbackPolicy.Risk == RiskR4 && st.RollbackPolicy.Gate == GateRollback
			}
			if !confirm {
				c.materializeTaskLocked(rs, at, false)
				rs = c.store.stages[key]
				queued = true
			} else {
				c.materializeTaskLocked(rs, at, true)
				rs = c.store.stages[key]
				waiting = true
			}
		}
		switch rs.Status {
		case StageSucceeded, StageSkipped:
		default:
			all = false
		}
		if rs.Status == StageWaitingUser {
			waiting = true
		}
		if rs.Status == StageQueued {
			queued = true
		}
	}
	if all {
		c.updateRunLocked(runID, RunSucceeded, "", at)
	} else if r.Status == RunRunning {
		// Once execution has started, later queued or approval-gated stages do
		// not move the aggregate back to a pre-start state.  Their own status
		// carries the waiting/queued detail while StartedAt remains valid.
		c.store.runs[runID] = r
	} else if waiting {
		c.updateRunLocked(runID, RunWaitingUser, "", at)
	} else if queued {
		c.updateRunLocked(runID, RunQueued, "", at)
	} else {
		c.store.runs[runID] = r
	}
}
func (c *MemoryCoordinator) materializeTaskLocked(rs RunStage, at time.Time, confirm bool) {
	p := c.store.plans[rs.PlanID]
	run := c.store.runs[rs.RunID]
	// A child stage remains bound to the immutable run revision captured when
	// the graph was created. Never bind a newly materialized child to the
	// mutable aggregate head after reconciliation/restart.
	if rs.RunRevision != 0 {
		run.Revision = rs.RunRevision
	}
	tid := DeterministicTaskID(rs.StageID)
	payload := &coretask.ExecutionStageTaskPayload{PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: string(p.Digest), DeploymentID: p.DeploymentID, RunID: rs.RunID, RunRevision: run.Revision, StageID: rs.StageID, StageRevision: rs.StageRevision, StageDigest: string(rs.StageDigest), TargetID: rs.TargetID, TargetRevision: rs.TargetRevision, TargetDigest: string(rs.TargetDigest)}
	if confirm {
		payload.ConfirmationID = DeterministicConfirmationID(rs.StageID)
	}
	spec := coretask.TaskSpec{Kind: coretask.TaskKindExecutionStage, Payload: coretask.TaskPayload{ExecutionStage: payload}, Goal: "execute stage " + rs.StageKey, TimeoutSeconds: int64(p.stage(rs.StageKey).TimeoutSeconds), IdempotencyKey: DeterministicTaskID(rs.StageID), AvailableAt: at}
	n, err := spec.Normalize()
	if err != nil {
		panic(err)
	}
	t := coretask.Task{ID: tid, OwnerID: rs.OwnerID, Spec: n, Status: coretask.StatusQueued, Revision: 1, CreatedAt: at, UpdatedAt: at, AvailableAt: at}
	rs.TaskID = tid
	rs.Status = StageQueued
	if confirm {
		t.Status = coretask.StatusWaitingUser
		rs.Status = StageWaitingUser
		rs.ConfirmationID = payload.ConfirmationID
		b := c.bindingLocked(p, run, rs, at)
		conf := coreconfirmation.Confirmation{ID: payload.ConfirmationID, ConfirmationID: payload.ConfirmationID, OwnerID: rs.OwnerID, Binding: b, TaskID: tid, State: coreconfirmation.StatePending, Revision: 1, CreatedAt: at, UpdatedAt: at, ExpiresAt: p.ExpiresAt}
		c.store.confirmations[conf.ID] = conf
	}
	c.store.tasks[tid] = t
	c.store.stages[stageMapKey(rs.RunID, rs.StageID)] = rs
	c.eventLocked(rs.RunID, rs.StageID, "stage_materialized", rs.Status, at)
}
func (c *MemoryCoordinator) bindingLocked(p ExecutionPlan, r ExecutionRun, s RunStage, at time.Time) coreconfirmation.Binding {
	st := p.stage(s.StageKey)
	risk, gate, execution := st.Risk, st.Gate, st.Steps
	rollback := any(st.RollbackSteps)
	if r.Operation == RunOperationRollback {
		// A rollback card authorizes the independently declared R4/GateRollback
		// operation, never the forward stage's risk/gate or step list.
		risk, gate, execution = st.RollbackPolicy.Risk, st.RollbackPolicy.Gate, st.RollbackSteps
		rollback = struct {
			Steps  []ExecutionStep
			Policy *RollbackPolicy
		}{st.RollbackSteps, st.RollbackPolicy}
	}
	d := func(x any) coreconfirmation.Digest { return coreconfirmation.Digest(mustDigest(x)) }
	// Existing immutable pins are copied as-is. In particular, never hash a
	// digest string again (plan/stage/target/step pins are already SHA-256).
	exact := func(x Digest) coreconfirmation.Digest { return coreconfirmation.Digest(x) }
	b := coreconfirmation.Binding{OwnerID: p.OwnerID, OperationDomain: "execution:v2:" + string(gate), TargetID: s.TargetID, TargetRevision: int64(s.TargetRevision), TargetKind: c.targetKind(p, s.TargetID), ContentDigest: d(p.AnalysisID), ParameterDigest: d(st), NetworkDigest: d(execution), SecretGrantDigest: d(execution), PlanID: p.ID, PlanRevision: int64(p.Revision), PlanDigest: exact(p.Digest), DeploymentID: p.DeploymentID, RunID: r.RunID, RunRevision: int64(r.Revision), StageID: s.StageID, StageRevision: int64(s.StageRevision), StageDigest: exact(s.StageDigest), TargetDigest: exact(s.TargetDigest), ExecutionDigest: d(execution), ArtifactSetDigest: d(p.Artifacts), PolicyDigest: d(struct {
		Risk Risk
		Gate Gate
	}{risk, gate}), CostQuoteDigest: d(p.Placement), RollbackDigest: d(rollback), PreviewDigest: d(struct{ Title, Kind string }{st.Title, st.Kind}), RiskLevel: string(risk), GateType: string(gate), StageIdempotencyKey: s.StageIdempotencyKey, BindingExpiresAt: p.ExpiresAt.UTC()}
	n, err := b.Normalize()
	if err != nil {
		panic(err)
	}
	return n
}

// Run revision advances for operational state transitions (for example,
// waiting_user -> queued).  That transition is itself caused by accepting the
// same card, so revalidation preserves the immutable revision carried by the
// card while still re-reading the run's plan pin and stage/target snapshots.
func (c *MemoryCoordinator) authoritativeBindingLocked(conf coreconfirmation.Confirmation, p ExecutionPlan, s RunStage) coreconfirmation.Binding {
	r := c.store.runs[s.RunID]
	r.Revision = uint64(conf.Binding.RunRevision)
	return c.bindingLocked(p, r, s, time.Time{})
}
func (c *MemoryCoordinator) authoritativeLocked(conf coreconfirmation.Confirmation) (RunStage, ExecutionPlan, error) {
	for _, rs := range c.store.stages {
		if rs.ConfirmationID == conf.ID {
			p, ok := c.store.plans[rs.PlanID]
			if !ok {
				return RunStage{}, ExecutionPlan{}, ErrNotFound
			}
			found := false
			for _, target := range p.Targets {
				if target.ID == rs.TargetID && target.Revision == rs.TargetRevision && target.Digest == rs.TargetDigest {
					found = true
					break
				}
			}
			if !found || p.Revision != rs.PlanRevision || p.Digest == "" {
				return RunStage{}, ExecutionPlan{}, ErrConflict
			}
			return rs, p, nil
		}
	}
	return RunStage{}, ExecutionPlan{}, ErrNotFound
}
func (c *MemoryCoordinator) acquireLeaseLocked(s RunStage, at time.Time) error {
	l, ok := c.store.leases[s.TargetID]
	if ok && (l.uncertain || l.expiresAt.After(at)) {
		return ErrConflict
	}
	l = memoryTargetLease{token: uuid.NewString(), epoch: l.epoch + 1, runID: s.RunID, stageID: s.StageID, expiresAt: at.Add(c.leaseTTL)}
	c.store.leases[s.TargetID] = l
	return nil
}
func (c *MemoryCoordinator) assertLeaseLocked(s RunStage, taskID, holder string) error {
	l := c.store.leases[s.TargetID]
	t := c.store.tasks[taskID]
	if l.runID != s.RunID || l.stageID != s.StageID || t.Lease == nil || t.Lease.Holder != holder || l.epoch != t.Lease.Epoch || (!l.uncertain && !l.expiresAt.After(c.at(time.Time{}))) {
		return ErrConflict
	}
	return nil
}
func (c *MemoryCoordinator) fenceDispatchIntentLocked(s RunStage) {
	l := c.store.leases[s.TargetID]
	l.uncertain, l.expiresAt = true, time.Time{}
	c.store.leases[s.TargetID] = l
}
func (c *MemoryCoordinator) finalizeLocked(s RunStage, t coretask.Task, as AttemptStatus, ss StageStatus, rs RunStatus, reason string, at time.Time, uncertain bool) {
	for i := range c.store.attempts[s.StageID] {
		a := &c.store.attempts[s.StageID][i]
		if uncertain && i != len(c.store.attempts[s.StageID])-1 {
			continue
		}
		if !uncertain && a.Status != AttemptRunning {
			continue
		}
		a.Status = as
		a.Uncertain = uncertain
		a.FinishedAt = at
		a.Revision++
		a.UpdatedAt = at
		if rec, ok := c.store.receipts[a.ReceiptID]; ok {
			if uncertain {
				rec.Status = ReceiptUncertain
			} else if as == AttemptSucceeded {
				rec.Status = ReceiptSucceeded
			} else {
				rec.Status = ReceiptFailed
			}
			rec.Revision++
			c.store.receipts[rec.ReceiptID] = rec
		}
	}
	s.Status = ss
	s.FinishedAt = at
	s.UpdatedAt = at
	c.store.stages[stageMapKey(s.RunID, s.StageID)] = s
	t.Status = coretask.StatusSucceeded
	t.Result = &coretask.Result{Summary: "execution stage completed"}
	if ss != StageSucceeded {
		t.Status = coretask.StatusFailed
		t.FailureCode = reason
		t.FailureSummary = reason
		t.Result = nil
	}
	t.Lease = nil
	t.Revision++
	t.UpdatedAt = at
	c.store.tasks[t.ID] = t
	if s.ConfirmationID != "" && !uncertain {
		conf := c.store.confirmations[s.ConfirmationID]
		delete(c.store.reservations, conf.ID)
		conf.UpdatedAt = at
		conf.Revision++
		c.store.confirmations[conf.ID] = conf
	}
	l := c.store.leases[s.TargetID]
	if uncertain {
		l.uncertain = true
		// Uncertain dispatch is a durable fence, not a TTL-based retry.  Only
		// reconciliation of this exact lease may release it.
		l.expiresAt = time.Time{}
	} else {
		l.uncertain = false
		l.expiresAt = time.Time{}
		l.runID = ""
		l.stageID = ""
	}
	c.store.leases[s.TargetID] = l
	c.updateRunLocked(s.RunID, rs, s.StageID, at)
	c.eventLocked(s.RunID, s.StageID, "stage_terminal", ss, at)
}

// resolveUncertainLocked advances only the exact uncertain evidence that was
// read back. The resolution row is append-only, while the queryable aggregate
// is promoted in place under the same store lock.
func (c *MemoryCoordinator) resolveUncertainLocked(s RunStage, t coretask.Task, as AttemptStatus, ss StageStatus, rs RunStatus, reason string, outcomeDigest Digest, at time.Time) {
	attempts := c.store.attempts[s.StageID]
	for i := len(attempts) - 1; i >= 0; i-- {
		a := &attempts[i]
		if a.Status != AttemptUncertain {
			continue
		}
		a.Status, a.Uncertain, a.Revision, a.UpdatedAt = as, false, a.Revision+1, at
		if rec, ok := c.store.receipts[a.ReceiptID]; ok {
			rec.Status, rec.ResponseDigest, rec.Revision = receiptStatusForStage(ss), outcomeDigest, rec.Revision+1
			c.store.receipts[rec.ReceiptID] = rec
		}
		break
	}
	c.store.attempts[s.StageID] = attempts

	s.Status, s.UpdatedAt = ss, at
	c.store.stages[stageMapKey(s.RunID, s.StageID)] = s
	t.Lease = nil
	t.Revision++
	t.UpdatedAt = at
	t.FailureCode, t.FailureSummary, t.Result = "", "", nil
	switch ss {
	case StageSucceeded:
		t.Status = coretask.StatusSucceeded
		t.Result = &coretask.Result{Summary: "execution stage reconciled"}
	case StageFailed:
		t.Status, t.FailureCode, t.FailureSummary = coretask.StatusFailed, reason, reason
	case StageCanceled:
		t.Status, t.FailureCode, t.FailureSummary = coretask.StatusCanceled, reason, reason
	}
	c.store.tasks[t.ID] = t
	if s.ConfirmationID != "" {
		conf := c.store.confirmations[s.ConfirmationID]
		delete(c.store.reservations, conf.ID)
		conf.Revision++
		conf.UpdatedAt = at
		c.store.confirmations[conf.ID] = conf
	}
	l := c.store.leases[s.TargetID]
	l.uncertain, l.expiresAt, l.runID, l.stageID = false, time.Time{}, "", ""
	c.store.leases[s.TargetID] = l

	// Production reconciliation reopens the uncertain run before promoting its
	// DAG. A successful stage may therefore leave the run running while a child
	// is queued/waiting; a failed/canceled stage skips all blocked descendants
	// and settles the aggregate when no other stage is active.
	run := c.store.runs[s.RunID]
	run.Status, run.TerminalReason, run.FinishedAt = RunRunning, "", time.Time{}
	run.Revision++
	run.UpdatedAt = at
	c.store.runs[s.RunID] = run
	if ss == StageSucceeded {
		c.promoteLocked(s.RunID, at)
	} else {
		c.skipBlockedDescendantsLocked(s.RunID, at)
		c.settleReconciledRunLocked(s.RunID, rs, reason, s.StageID, at)
	}
	c.eventLocked(s.RunID, s.StageID, "uncertain_reconciled_"+string(ss), ss, at)
}

func receiptStatusForStage(status StageStatus) ReceiptStatus {
	switch status {
	case StageSucceeded:
		return ReceiptSucceeded
	case StageFailed:
		return ReceiptFailed
	case StageCanceled:
		return ReceiptCanceled
	default:
		return ReceiptUncertain
	}
}

func reconciliationOutcomeDigest(s RunStage, status StageStatus) Digest {
	return mustDigest(struct {
		StageID string
		Status  StageStatus
	}{s.StageID, status})
}

func (c *MemoryCoordinator) settleReconciledRunLocked(runID string, preferred RunStatus, reason, currentStage string, at time.Time) {
	active, hasUncertain := false, false
	for _, key := range c.stageKeysLocked(runID) {
		s := c.store.stages[key]
		switch s.Status {
		case StageBlocked, StageWaitingUser, StageQueued, StageRunning:
			active = true
		case StageUncertain:
			hasUncertain = true
		}
	}
	if active {
		return
	}
	target, terminalReason := preferred, reason
	if hasUncertain {
		target, terminalReason = RunUncertain, "stage_outcome_uncertain"
	}
	c.updateRunLocked(runID, target, currentStage, at)
	r := c.store.runs[runID]
	r.TerminalReason = terminalReason
	c.store.runs[runID] = r
}
func (c *MemoryCoordinator) payloadMatchesLocked(t coretask.Task, s RunStage, p ExecutionPlan) error {
	q := t.Spec.Payload.ExecutionStage
	if q == nil || q.PlanID != p.ID || q.PlanRevision != p.Revision || q.PlanDigest != string(p.Digest) || q.RunID != s.RunID || q.RunRevision == 0 || q.RunRevision != s.RunRevision || q.StageID != s.StageID || q.StageRevision != s.StageRevision || q.StageDigest != string(s.StageDigest) || q.TargetID != s.TargetID || q.TargetRevision != s.TargetRevision || q.TargetDigest != string(s.TargetDigest) {
		return ErrConflict
	}
	if s.ConfirmationID != "" && uint64(c.store.confirmations[s.ConfirmationID].Binding.RunRevision) != q.RunRevision {
		return ErrConflict
	}
	return nil
}
func createRunReplayDigestForPlan(in CreateRunCommand, p ExecutionPlan) Digest {
	// The idempotency key is intentionally absent: the replay map scopes by
	// owner/key, while this digest detects a changed command body.
	return mustDigest(struct {
		Schema          string
		OwnerID, PlanID string
		PlanRevision    uint64
		PlanDigest      Digest
		Operation       RunOperation
		TriggerKind     TriggerKind
		RollbackOfRunID string
	}{SchemaVersion, in.OwnerID, p.ID, in.PlanRevision, p.Digest, in.Operation, in.TriggerKind, in.RollbackOfRunID})
}
func confirmationReplayDigest(in ConfirmStageCommand, binding coreconfirmation.Binding) Digest {
	return mustDigest(struct {
		Schema                  string
		OwnerID, ConfirmationID string
		ExpectedRevision        int64
		AuthoritativeBinding    coreconfirmation.Binding
	}{SchemaVersion, in.OwnerID, in.ConfirmationID, in.ExpectedRevision, binding})
}

func (c *MemoryCoordinator) updateLatestReceiptLocked(stageID string, result fakeResult) {
	attempts := c.store.attempts[stageID]
	if len(attempts) == 0 {
		return
	}
	if rec, ok := c.store.receipts[attempts[len(attempts)-1].ReceiptID]; ok {
		rec.ProviderOperation, rec.SSMCommandID, rec.Revision = result.providerOperation, result.commandID, rec.Revision+1
		c.store.receipts[rec.ReceiptID] = rec
	}
}
func (c *MemoryCoordinator) removeLatestIntentLocked(stageID string) {
	attempts := c.store.attempts[stageID]
	if len(attempts) == 0 {
		return
	}
	last := attempts[len(attempts)-1]
	delete(c.store.receipts, last.ReceiptID)
	c.store.attempts[stageID] = attempts[:len(attempts)-1]
}
func (c *MemoryCoordinator) recordResolutionLocked(s RunStage, token string, epoch uint64, outcome StageStatus, outcomeDigest Digest, at time.Time) error {
	for _, old := range c.store.resolutions[s.RunID] {
		if old.StageID == s.StageID && old.LeaseToken == token && old.LeaseEpoch == epoch {
			if old.Outcome != outcome || old.OutcomeDigest != outcomeDigest {
				return ErrConflict
			}
			return nil
		}
	}
	c.store.resolutions[s.RunID] = append(c.store.resolutions[s.RunID], memoryReconciliationResolution{
		RunID: s.RunID, StageID: s.StageID, LeaseToken: token, LeaseEpoch: epoch,
		Outcome: outcome, OutcomeDigest: outcomeDigest, Succeeded: outcome == StageSucceeded, At: at,
	})
	return nil
}

// authoritativeStageLocked is intentionally side-effect free: it is called
// before the lease is claimed and before a confirmed card is consumed.
func (c *MemoryCoordinator) authoritativeStageLocked(t coretask.Task, s RunStage, p ExecutionPlan, at time.Time) error {
	if p.ID == "" || p.OwnerID != s.OwnerID || p.Revision != s.PlanRevision || p.Digest == "" || p.Status != PlanReady || !p.ExpiresAt.After(at) {
		return ErrConflict
	}
	if err := c.payloadMatchesLocked(t, s, p); err != nil {
		return err
	}
	found := false
	for _, target := range p.Targets {
		if target.ID == s.TargetID && target.Revision == s.TargetRevision && target.Digest == s.TargetDigest {
			found = true
			break
		}
	}
	if !found {
		return ErrConflict
	}
	if s.ConfirmationID != "" {
		conf, ok := c.store.confirmations[s.ConfirmationID]
		if !ok || conf.OwnerID != s.OwnerID || conf.TaskID != t.ID || conf.State != coreconfirmation.StateConfirmed || !conf.ExpiresAt.After(at) || !conf.Binding.Equal(c.authoritativeBindingLocked(conf, p, s)) {
			return ErrConflict
		}
	}
	if c.store.runs[s.RunID].Operation == RunOperationRollback {
		stage := p.stage(s.StageKey)
		if len(stage.RollbackSteps) == 0 || stage.RollbackPolicy == nil || stage.RollbackPolicy.Risk != RiskR4 || stage.RollbackPolicy.Gate != GateRollback {
			return ErrConflict
		}
	}
	return nil
}

func (c *MemoryCoordinator) stepsForRunLocked(r ExecutionRun, p ExecutionPlan, s RunStage) []ExecutionStep {
	stage := p.stage(s.StageKey)
	selection, _ := SelectStageExecution(r.Operation, stage)
	return selection.Steps
}

func sortStagesReverseDependency(stages []ExecutionStage, applied map[string]bool) []ExecutionStage {
	by := make(map[string]ExecutionStage, len(stages))
	for _, stage := range stages {
		if applied[stage.StageKey] {
			by[stage.StageKey] = stage
		}
	}
	depth := make(map[string]int)
	var visit func(string) int
	visit = func(key string) int {
		if d, ok := depth[key]; ok {
			return d
		}
		max := 0
		for _, dep := range by[key].DependsOn {
			if _, ok := by[dep]; ok {
				if d := visit(dep) + 1; d > max {
					max = d
				}
			}
		}
		depth[key] = max
		return max
	}
	out := make([]ExecutionStage, 0, len(by))
	for key, stage := range by {
		visit(key)
		out = append(out, stage)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if depth[out[i].StageKey] != depth[out[j].StageKey] {
			return depth[out[i].StageKey] > depth[out[j].StageKey]
		}
		return out[i].StageKey > out[j].StageKey
	})
	return out
}

func (c *MemoryCoordinator) markLatestAttemptLocked(stageID string, status AttemptStatus, at time.Time) {
	attempts := c.store.attempts[stageID]
	if len(attempts) == 0 {
		return
	}
	a := &attempts[len(attempts)-1]
	a.Status, a.FinishedAt, a.Revision, a.UpdatedAt = status, at, a.Revision+1, at
	if rec, ok := c.store.receipts[a.ReceiptID]; ok {
		rec.Status, rec.Revision = ReceiptSucceeded, rec.Revision+1
		c.store.receipts[rec.ReceiptID] = rec
	}
	c.store.attempts[stageID] = attempts
}

// skipBlockedDescendantsLocked mirrors the durable DAG transition: once a
// stage has a terminal non-success outcome, every blocked descendant is
// skipped transitively and is never materialized as an executable task.
func (c *MemoryCoordinator) skipBlockedDescendantsLocked(runID string, at time.Time) {
	for {
		changed := false
		for _, key := range c.stageKeysLocked(runID) {
			s := c.store.stages[key]
			if s.Status != StageBlocked {
				continue
			}
			p := c.store.plans[s.PlanID]
			blocked := false
			for _, dependency := range p.stage(s.StageKey).DependsOn {
				for _, parent := range c.store.stages {
					if parent.RunID != runID || parent.StageKey != dependency {
						continue
					}
					switch parent.Status {
					case StageFailed, StageUncertain, StageSkipped, StageCanceled, StageRejected, StageExpired:
						blocked = true
					}
				}
			}
			if !blocked {
				continue
			}
			s.Status, s.UpdatedAt = StageSkipped, at
			c.store.stages[key] = s
			c.eventLocked(runID, s.StageID, "stage_skipped", StageSkipped, at)
			changed = true
		}
		if !changed {
			return
		}
	}
}

func (c *MemoryCoordinator) dependenciesDoneLocked(runID string, s RunStage) bool {
	p := c.store.plans[s.PlanID]
	if c.store.runs[runID].Operation == RunOperationRollback {
		// Rollback uses the inverse DAG: a stage may roll back only after every
		// materialized stage that depended on it has finished its rollback.
		for _, candidate := range p.Stages {
			dependsOnStage := false
			for _, dependency := range candidate.DependsOn {
				if dependency == s.StageKey {
					dependsOnStage = true
					break
				}
			}
			if !dependsOnStage {
				continue
			}
			for _, other := range c.store.stages {
				if other.RunID == runID && other.StageKey == candidate.StageKey &&
					other.Status != StageSucceeded && other.Status != StageSkipped {
					return false
				}
			}
		}
		return true
	}
	for _, d := range p.stage(s.StageKey).DependsOn {
		for _, other := range c.store.stages {
			if other.RunID == runID && other.StageKey == d && other.Status != StageSucceeded && other.Status != StageSkipped {
				return false
			}
		}
	}
	return true
}
func (c *MemoryCoordinator) updateRunLocked(id string, status RunStatus, current string, at time.Time) {
	r := c.store.runs[id]
	r.Status = status
	if current != "" {
		r.CurrentStageID = current
	}
	if (status == RunRunning || isTerminalRun(status)) && r.StartedAt.IsZero() {
		r.StartedAt = at
	}
	r.Revision++
	r.UpdatedAt = at
	if status == RunSucceeded || status == RunFailed || status == RunUncertain || status == RunCanceled {
		r.FinishedAt = at
	}
	c.store.runs[id] = r
}
func (c *MemoryCoordinator) eventLocked(runID, stageID, typ string, status StageStatus, at time.Time) {
	e := Event{EventID: uuid.NewString(), RunID: runID, OwnerID: c.store.runs[runID].OwnerID, Revision: 1, StageID: stageID, Status: status, Type: typ, Sequence: uint64(len(c.store.events[runID]) + 1), At: at}
	c.store.events[runID] = append(c.store.events[runID], e)
}
func (c *MemoryCoordinator) materializationLocked(runID string) RunMaterialization {
	r := RunMaterialization{Run: cloneRun(c.store.runs[runID])}
	for _, k := range c.stageKeysLocked(runID) {
		s := c.store.stages[k]
		r.Stages = append(r.Stages, cloneStage(s))
		if s.TaskID != "" {
			r.Tasks = append(r.Tasks, cloneTask(c.store.tasks[s.TaskID]))
		}
		if s.ConfirmationID != "" {
			r.Confirmations = append(r.Confirmations, cloneConfirmation(c.store.confirmations[s.ConfirmationID]))
		}
	}
	return r
}
func (c *MemoryCoordinator) stageKeysLocked(runID string) []string {
	out := []string{}
	for k, s := range c.store.stages {
		if s.RunID == runID {
			out = append(out, k)
		}
	}
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if c.store.stages[out[j]].Ordinal < c.store.stages[out[i]].Ordinal {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
func (c *MemoryCoordinator) stageForTaskLocked(taskID string) (RunStage, bool) {
	for _, s := range c.store.stages {
		if s.TaskID == taskID {
			return s, true
		}
	}
	return RunStage{}, false
}
func (c *MemoryCoordinator) targetKind(p ExecutionPlan, id string) string {
	for _, t := range p.Targets {
		if t.ID == id {
			return t.Kind
		}
	}
	return "unknown"
}
func (c *MemoryCoordinator) at(v time.Time) time.Time {
	if v.IsZero() {
		return c.now().UTC()
	}
	return v.UTC()
}
func stageMapKey(run, stage string) string { return run + "/" + stage }
func deterministicID(v string) string {
	const prefix = "execution:v2:stage:"
	if strings.HasPrefix(v, prefix) {
		parts := strings.SplitN(strings.TrimPrefix(v, prefix), ":", 2)
		if len(parts) == 2 {
			return DeterministicStageID(parts[0], parts[1])
		}
	}
	return deterministicUUID("legacy", v)
}
func mustDigest(v any) Digest {
	d, e := CanonicalDigest(v)
	if e != nil {
		panic(fmt.Sprintf("digest: %v", e))
	}
	return d
}
func (p ExecutionPlan) stage(key string) ExecutionStage {
	for _, s := range p.Stages {
		if s.StageKey == key {
			return s
		}
	}
	panic("missing stage")
}
func cloneRun(v ExecutionRun) ExecutionRun                                            { return v }
func cloneStage(v RunStage) RunStage                                                  { return v }
func cloneTask(v coretask.Task) coretask.Task                                         { v.Spec, _ = v.Spec.Normalize(); return v }
func cloneConfirmation(v coreconfirmation.Confirmation) coreconfirmation.Confirmation { return v }
