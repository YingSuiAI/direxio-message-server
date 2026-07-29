package coreworkload

import (
	"context"
	"sort"
	"sync"
	"time"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

type PlanInput struct {
	Summary, Artifact, Source   string
	CommandSteps                []string
	ImageDigest                 string
	ImageURI                    string
	TargetKind                  TargetKind
	Target                      TargetSettings
	NetworkGrants, SecretGrants []string
	SecretGrantRefs             []SecretGrantRef
	ResourceLimits              ResourceLimits
	ExpiresAt                   time.Time
	IdempotencyKey              string
}
type CreatePlanInput = PlanInput
type RequestCommand struct {
	PlanID, WorkloadID, IdempotencyKey string
	Kind                               OperationKind
	ExpiresAt                          time.Time
}

// RequestInputDigest fingerprints every caller-provided request field.
// Generated operation/workload identifiers are intentionally excluded.
func RequestInputDigest(c RequestCommand) string { return canonicalDigest(c) }
func CancelInputDigest(operationID string, expected uint64) string {
	return canonicalDigest(struct {
		OperationID string
		Expected    uint64
	}{operationID, expected})
}

type Store interface {
	CreatePlan(context.Context, PlanInput) (Plan, error)
	GetPlan(context.Context, string) (Plan, error)
	GetWorkload(context.Context, string) (Workload, error)
	ListWorkloads(context.Context, int, string) ([]Workload, string, error)
	ListPlans(context.Context, int, string) ([]Plan, string, error)
	GetOperation(context.Context, string) (Operation, error)
	ListEvents(context.Context, string, uint64) ([]Event, error)
	RequestOperation(context.Context, RequestCommand) (RequestResult, error)
	CancelOperation(context.Context, string, string, uint64) (Operation, error)
	Confirm(context.Context, string, int64) (coreconfirmation.Confirmation, error)
	Consume(context.Context, string, string, string, uint64) (Operation, coretask.Task, error)
	AppendEvent(context.Context, string, Event) (Event, error)
	CompleteDispatch(context.Context, string, string, string, uint64, string, Readback, string) (Operation, coretask.Task, error)
	RenewDispatchLease(context.Context, string, string, uint64) (Operation, error)
	// RecoverClaim fences a previously dispatched operation for read-only
	// reconciliation. It never increments the provider dispatch attempt.
	RecoverClaim(context.Context, string, string) (Operation, error)
}

// FencedStore is the production workload boundary. The legacy Store methods
// remain for existing direct domain tests/adapters; production handlers use
// these methods to bind every mutation to the already-claimed generic task
// lease without creating a second lease.
type FencedStore interface {
	ConsumeFenced(context.Context, string, string, string, uint64, TaskFence) (Operation, coretask.Task, error)
	CompleteDispatchFenced(context.Context, string, string, string, uint64, string, Readback, string, TaskFence) (Operation, coretask.Task, error)
	RenewDispatchLeaseFenced(context.Context, string, string, uint64, TaskFence) (Operation, error)
	RecoverClaimFenced(context.Context, string, string, TaskFence) (Operation, error)
}

func (s *MemoryStore) CancelOperation(_ context.Context, opID, key string, expected uint64) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[opID]
	if !ok {
		return Operation{}, ErrNotFound
	}
	if !ValidUUID(key) {
		return Operation{}, ErrInvalid
	}
	hash := CancelInputDigest(opID, expected)
	if prior, ok := s.replays[key]; ok {
		if s.replayHashes[key] != hash {
			return Operation{}, ErrConflict
		}
		return prior.Operation, nil
	}
	if op.Revision != expected {
		return Operation{}, ErrRevisionConflict
	}
	if op.Status != OperationWaitingUser || op.DispatchState != "prepared" || op.DispatchClaim != "" {
		return Operation{}, ErrConflict
	}
	c := s.confirmations[op.ConfirmationID]
	c, terminalErr := s.terminalizeBeforeDispatchLocked(c, OperationCanceled, coreconfirmation.StateRejected, coretask.StatusCanceled, "user_canceled", "canceled")
	if terminalErr != nil {
		return Operation{}, terminalErr
	}
	op = s.operations[opID]
	t := s.tasks[op.TaskID]
	r := RequestResult{Operation: op, Task: t, Confirmation: c}
	s.replays[key] = r
	s.replayHashes[key] = hash
	return op, nil
}

// MemoryStore is a deterministic store used by unit tests and local wiring.
// RequestOperation and Consume hold one mutex across operation/task/
// confirmation changes, matching the transaction boundary of PostgreSQL.
type MemoryStore struct {
	mu            sync.Mutex
	now           func() time.Time
	plans         map[string]Plan
	operations    map[string]Operation
	tasks         map[string]coretask.Task
	confirmations map[string]coreconfirmation.Confirmation
	events        map[string][]Event
	replays       map[string]RequestResult
	replayHashes  map[string]string
	workloads     map[string]Workload
	planReplays   map[string]Plan
	planHashes    map[string]string
}

func NewMemoryStore(now func() time.Time) *MemoryStore {
	if now == nil {
		now = time.Now
	}
	return &MemoryStore{now: now, plans: map[string]Plan{}, operations: map[string]Operation{}, tasks: map[string]coretask.Task{}, confirmations: map[string]coreconfirmation.Confirmation{}, events: map[string][]Event{}, replays: map[string]RequestResult{}, replayHashes: map[string]string{}, workloads: map[string]Workload{}, planReplays: map[string]Plan{}, planHashes: map[string]string{}}
}

func (s *MemoryStore) CreatePlan(_ context.Context, in PlanInput) (Plan, error) {
	if s == nil || !ValidUUID(in.IdempotencyKey) {
		return Plan{}, ErrInvalid
	}
	p := Plan{ID: uuid.New().String(), Revision: 1, Summary: in.Summary, Artifact: in.Artifact, Source: in.Source, CommandSteps: in.CommandSteps, ImageDigest: in.ImageDigest, ImageURI: in.ImageURI, TargetKind: in.TargetKind, Target: in.Target, NetworkGrants: in.NetworkGrants, SecretGrants: in.SecretGrants, SecretGrantRefs: in.SecretGrantRefs, ResourceLimits: in.ResourceLimits, ExpiresAt: in.ExpiresAt.UTC(), CreatedAt: s.now().UTC()}
	n, e := p.Normalize()
	if e != nil {
		return Plan{}, e
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	hash := planInputDigest(n)
	if prior, ok := s.planReplays[in.IdempotencyKey]; ok {
		if s.planHashes[in.IdempotencyKey] != hash {
			return Plan{}, ErrConflict
		}
		return prior, nil
	}
	for _, v := range s.plans {
		if v.Digest == n.Digest {
			s.planReplays[in.IdempotencyKey], s.planHashes[in.IdempotencyKey] = v, hash
			return v, nil
		}
	}
	s.plans[n.ID] = n
	s.planReplays[in.IdempotencyKey], s.planHashes[in.IdempotencyKey] = n, hash
	return n, nil
}
func (s *MemoryStore) GetPlan(_ context.Context, id string) (Plan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.plans[id]
	if !ok {
		return Plan{}, ErrNotFound
	}
	return p, nil
}
func (s *MemoryStore) GetWorkload(_ context.Context, id string) (Workload, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w, ok := s.workloads[id]
	if !ok {
		return Workload{}, ErrNotFound
	}
	return w, nil
}
func (s *MemoryStore) ListWorkloads(_ context.Context, limit int, cursor string) ([]Workload, string, error) {
	if limit <= 0 || limit > 200 {
		return nil, "", ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Workload, 0, len(s.workloads))
	for _, w := range s.workloads {
		if cursor == "" || w.ID > cursor {
			out = append(out, w)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		return out[:limit], out[limit-1].ID, nil
	}
	return out, "", nil
}
func (s *MemoryStore) ListPlans(_ context.Context, limit int, cursor string) ([]Plan, string, error) {
	if limit <= 0 || limit > 200 {
		return nil, "", ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Plan, 0, len(s.plans))
	for _, p := range s.plans {
		if cursor == "" || p.ID > cursor {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if len(out) > limit {
		return out[:limit], out[limit-1].ID, nil
	}
	return out, "", nil
}
func (s *MemoryStore) GetOperation(_ context.Context, id string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.operations[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	return v, nil
}
func (s *MemoryStore) ListEvents(_ context.Context, id string, after uint64) ([]Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.events[id]
	out := []Event{}
	for _, e := range all {
		if e.Sequence > after {
			out = append(out, e)
		}
	}
	return out, nil
}
func (s *MemoryStore) RequestOperation(_ context.Context, c RequestCommand) (RequestResult, error) {
	if s == nil || !ValidUUID(c.PlanID) || !ValidUUID(c.IdempotencyKey) || (c.WorkloadID != "" && !ValidUUID(c.WorkloadID)) || (c.Kind != OperationApply && c.Kind != OperationDestroy) {
		return RequestResult{}, ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	requestHash := canonicalDigest(c)
	if prior, ok := s.replays[c.IdempotencyKey]; ok {
		if s.replayHashes[c.IdempotencyKey] != requestHash {
			return RequestResult{}, ErrConflict
		}
		return prior, nil
	}
	p, ok := s.plans[c.PlanID]
	if !ok {
		return RequestResult{}, ErrNotFound
	}
	if c.ExpiresAt.IsZero() {
		c.ExpiresAt = s.now().UTC().Add(24 * time.Hour)
	}
	if !c.ExpiresAt.After(s.now().UTC()) || !p.ExpiresAt.After(s.now().UTC()) {
		return RequestResult{}, ErrInvalid
	}
	wid := c.WorkloadID
	if c.Kind == OperationDestroy && !ValidUUID(wid) {
		return RequestResult{}, ErrInvalid
	}
	if wid == "" {
		wid = uuid.New().String()
	}
	if existing, ok := s.workloads[wid]; ok {
		if c.Kind == OperationDestroy {
			if existing.State != "ready" || existing.PlanID != p.ID || existing.PlanDigest != p.Digest || existing.TargetKind != p.TargetKind {
				return RequestResult{}, ErrConflict
			}
		} else if existing.State != "destroyed" && existing.State != "ready" && existing.State != "failed" {
			return RequestResult{}, ErrConflict
		}
	}
	opID, taskID, confID := uuid.New().String(), uuid.New().String(), uuid.New().String()
	now := s.now().UTC()
	binding := bindingForOperation(p, wid, c.Kind)
	conf := coreconfirmation.Confirmation{ConfirmationID: confID, Binding: binding, TaskID: taskID, State: coreconfirmation.StatePending, Revision: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: c.ExpiresAt.UTC()}
	payload := coretask.WorkloadTaskPayload{WorkloadID: wid, PlanID: p.ID, OperationID: opID, PlanRevision: p.Revision, PlanDigest: p.Digest, TargetKind: string(p.TargetKind), ConfirmationID: confID, ExecutionSnapshot: mustJSON(p)}
	spec, _ := coretask.TaskSpec{Kind: coretask.TaskKindWorkload, Payload: coretask.TaskPayload{Workload: &payload}, Goal: "workload " + string(c.Kind), IdempotencyKey: uuid.New().String(), AvailableAt: now}.Normalize()
	task := coretask.Task{ID: taskID, Spec: spec, Status: coretask.StatusWaitingUser, Revision: 1, Attempt: 1, CreatedAt: now, UpdatedAt: now, AvailableAt: now}
	op := Operation{ID: opID, WorkloadID: wid, PlanID: p.ID, Kind: c.Kind, PlanRevision: p.Revision, PlanDigest: p.Digest, TargetKind: p.TargetKind, TaskID: taskID, ConfirmationID: confID, Status: OperationWaitingUser, Revision: 1, CreatedAt: now, UpdatedAt: now, DispatchState: "prepared"}
	s.operations[opID], s.tasks[taskID], s.confirmations[confID] = op, task, conf
	s.events[opID] = []Event{{OperationID: opID, Sequence: 1, Kind: "requested", Status: OperationWaitingUser, Message: "waiting for owner confirmation", At: now}}
	r := RequestResult{Operation: op, Task: task, Confirmation: conf}
	s.replays[c.IdempotencyKey], s.replayHashes[c.IdempotencyKey] = r, requestHash
	return r, nil
}
func (s *MemoryStore) Confirm(_ context.Context, id string, expected int64) (coreconfirmation.Confirmation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.confirmations[id]
	if !ok {
		return coreconfirmation.Confirmation{}, ErrNotFound
	}
	if c.Revision != expected {
		return coreconfirmation.Confirmation{}, ErrRevisionConflict
	}
	if !c.ExpiresAt.After(s.now().UTC()) {
		c.State = coreconfirmation.StateExpired
		c.Revision++
		c.UpdatedAt = s.now().UTC()
		s.confirmations[id] = c
		return c, ErrConflict
	}
	if c.State != coreconfirmation.StatePending {
		return c, nil
	}
	c.State = coreconfirmation.StateConfirmed
	c.Revision++
	c.UpdatedAt = s.now().UTC()
	s.confirmations[id] = c
	return c, nil
}
func (s *MemoryStore) Reject(_ context.Context, id string, expected int64, reason string) (coreconfirmation.Confirmation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.confirmations[id]
	if !ok {
		return coreconfirmation.Confirmation{}, ErrNotFound
	}
	if c.Revision != expected {
		return c, ErrRevisionConflict
	}
	return s.terminalizeBeforeDispatchLocked(c, OperationRejected, coreconfirmation.StateRejected, coretask.StatusCanceled, "user_rejected", reason)
}
func (s *MemoryStore) Expire(_ context.Context, id string, expected int64) (coreconfirmation.Confirmation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.confirmations[id]
	if !ok {
		return coreconfirmation.Confirmation{}, ErrNotFound
	}
	if c.Revision != expected {
		return c, ErrRevisionConflict
	}
	return s.terminalizeBeforeDispatchLocked(c, OperationExpired, coreconfirmation.StateExpired, coretask.StatusFailed, "confirmation_expired", "confirmation_expired")
}

func (s *MemoryStore) terminalizeBeforeDispatchLocked(c coreconfirmation.Confirmation, operationState OperationStatus, confirmationState coreconfirmation.State, taskState coretask.Status, code, summary string) (coreconfirmation.Confirmation, error) {
	var op Operation
	found := false
	for _, candidate := range s.operations {
		if candidate.ConfirmationID == c.ConfirmationID {
			op, found = candidate, true
			break
		}
	}
	if !found {
		return c, ErrNotFound
	}
	t := s.tasks[op.TaskID]
	if (c.State != coreconfirmation.StatePending && c.State != coreconfirmation.StateConfirmed) || op.Status != OperationWaitingUser || op.DispatchState != "prepared" || op.DispatchClaim != "" || t.Status != coretask.StatusWaitingUser {
		return c, ErrConflict
	}
	now := s.now().UTC()
	c.State, c.Revision, c.TerminalCode, c.TerminalReason, c.UpdatedAt = confirmationState, c.Revision+1, code, code, now
	op.Status, op.DispatchState, op.FailureCode, op.FailureSummary, op.Revision, op.UpdatedAt = operationState, "terminal", code, summary, op.Revision+1, now
	t.Status, t.FailureCode, t.FailureSummary, t.Revision, t.UpdatedAt = taskState, code, summary, t.Revision+1, now
	s.confirmations[c.ConfirmationID], s.operations[op.ID], s.tasks[t.ID] = c, op, t
	s.events[op.ID] = append(s.events[op.ID], Event{OperationID: op.ID, Sequence: uint64(len(s.events[op.ID]) + 1), Kind: "terminal", Status: operationState, Message: summary, At: now})
	return c, nil
}
func (s *MemoryStore) Consume(_ context.Context, opID, confirmationID, planDigest string, expected uint64) (Operation, coretask.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[opID]
	if !ok {
		return Operation{}, coretask.Task{}, ErrNotFound
	}
	t := s.tasks[op.TaskID]
	c := s.confirmations[confirmationID]
	if op.ConfirmationID != confirmationID || op.PlanDigest != planDigest || op.Revision != expected || op.Status != OperationWaitingUser || c.State != coreconfirmation.StateConfirmed || !c.ExpiresAt.After(s.now().UTC()) {
		return Operation{}, coretask.Task{}, ErrRevisionConflict
	}
	op.Status = OperationRunning
	op.DispatchState = "dispatched"
	op.DispatchAttempt++
	op.DispatchEpoch++
	op.DispatchClaim = uuid.New().String()
	op.DispatchLeaseUntil = s.now().UTC().Add(time.Hour)
	op.Revision++
	t.Status = coretask.StatusRunning
	t.LeaseEpoch = 1
	t.Revision++
	c.State = coreconfirmation.StateConsumed
	c.Revision++
	now := s.now().UTC()
	op.UpdatedAt = now
	t.UpdatedAt = now
	t.Lease = nil
	t.Lease = &coretask.Lease{TaskID: t.ID, Attempt: t.Attempt, Epoch: t.LeaseEpoch, Holder: "workload-handler", ExpiresAt: now.Add(time.Hour)}
	c.UpdatedAt = now
	s.operations[opID], s.tasks[op.TaskID], s.confirmations[confirmationID] = op, t, c
	s.events[opID] = append(s.events[opID], Event{OperationID: opID, Sequence: uint64(len(s.events[opID]) + 1), Kind: "consumed", Status: OperationRunning, At: now})
	return op, t, nil
}

// ConsumeFenced consumes confirmation exactly once against the generic
// WorkerPool lease. It deliberately leaves the task lease/revision untouched;
// the WorkerPool renewer remains the sole owner of that lease.
func (s *MemoryStore) ConsumeFenced(_ context.Context, opID, confirmationID, planDigest string, expected uint64, fence TaskFence) (Operation, coretask.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if !fence.Valid(time.Time{}) {
		return Operation{}, coretask.Task{}, ErrRevisionConflict
	}
	op, ok := s.operations[opID]
	if !ok {
		return Operation{}, coretask.Task{}, ErrNotFound
	}
	t, ok := s.tasks[op.TaskID]
	if !ok {
		return Operation{}, coretask.Task{}, ErrNotFound
	}
	c, ok := s.confirmations[confirmationID]
	if !ok {
		return Operation{}, coretask.Task{}, ErrNotFound
	}
	if op.ConfirmationID != confirmationID || op.PlanDigest != planDigest || op.Revision != expected || op.Status != OperationWaitingUser || c.State != coreconfirmation.StateConfirmed || !c.ExpiresAt.After(now) || !sameTaskFence(t, fence) {
		return Operation{}, coretask.Task{}, ErrRevisionConflict
	}
	op.Status = OperationRunning
	op.DispatchState = "dispatched"
	op.DispatchAttempt++
	op.DispatchEpoch = fence.LeaseEpoch
	op.DispatchClaim = uuid.NewString()
	op.DispatchLeaseUntil = t.Lease.ExpiresAt
	op.Revision++
	op.UpdatedAt = now
	c.State = coreconfirmation.StateConsumed
	c.Revision++
	c.UpdatedAt = now
	s.operations[opID], s.confirmations[confirmationID] = op, c
	s.events[opID] = append(s.events[opID], Event{OperationID: opID, Sequence: uint64(len(s.events[opID]) + 1), Kind: "consumed", Status: OperationRunning, At: now})
	return op, t, nil
}
func (s *MemoryStore) AppendEvent(_ context.Context, id string, e Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.operations[id]; !ok {
		return Event{}, ErrNotFound
	}
	e.OperationID = id
	e.Sequence = uint64(len(s.events[id]) + 1)
	if e.At.IsZero() {
		e.At = s.now().UTC()
	}
	s.events[id] = append(s.events[id], e)
	return e, nil
}
func (s *MemoryStore) completeLocked(_ context.Context, id, taskID, code string, readback Readback, summary string, claim string, epoch uint64, fence *TaskFence) (Operation, coretask.Task, error) {
	op, ok := s.operations[id]
	if !ok {
		return Operation{}, coretask.Task{}, ErrNotFound
	}
	t, ok := s.tasks[taskID]
	if !ok || op.TaskID != taskID {
		return Operation{}, coretask.Task{}, ErrNotFound
	}
	now := s.now().UTC()
	fingerprint := CompletionFingerprint(code, readback)
	if op.Status != OperationRunning {
		if op.DispatchClaim == claim && op.DispatchEpoch == epoch && claim != "" && op.CompletionFingerprint == fingerprint && (op.Status == OperationSucceeded || op.Status == OperationFailed || op.Status == OperationUncertain) {
			return op, t, nil
		}
		return Operation{}, coretask.Task{}, ErrRevisionConflict
	}
	validTask := t.Status == coretask.StatusRunning && t.Lease != nil && t.Lease.Epoch == epoch && t.Lease.ExpiresAt.After(now)
	if fence == nil {
		validTask = validTask && t.Lease.Holder == "workload-handler"
	} else {
		validTask = validTask && sameTaskFence(t, *fence)
	}
	if claim == "" || op.DispatchClaim != claim || op.DispatchEpoch != epoch || !op.DispatchLeaseUntil.After(now) || !validTask {
		return Operation{}, coretask.Task{}, ErrRevisionConflict
	}
	if code == "" {
		plan, ok := s.plans[op.PlanID]
		if !ok || plan.Digest != op.PlanDigest || plan.Revision != op.PlanRevision || readback.WorkloadID != op.WorkloadID || readback.TargetKind != op.TargetKind || readback.Identity.Validate(op.TargetKind) != nil || !targetIdentityEqual(readback.Identity, plan.Target.Identity, op.TargetKind) || (op.Kind == OperationApply && readback.State != "ready") || (op.Kind == OperationDestroy && readback.State != "destroyed") {
			return Operation{}, coretask.Task{}, ErrRevisionConflict
		}
	}
	if code == "" {
		op.Status = OperationSucceeded
		t.Status = coretask.StatusSucceeded
	} else {
		op.Status = OperationFailed
		if code == "provider_uncertain" {
			op.Status = OperationUncertain
		}
		t.Status = coretask.StatusFailed
		op.FailureCode, op.FailureSummary = SafeFailure(code, summary)
		t.FailureCode, t.FailureSummary = SafeFailure(code, summary)
	}
	if code == "provider_uncertain" {
		op.DispatchState = "uncertain"
	} else {
		op.DispatchState = "terminal"
	}
	// A terminal replay reads the persisted row after its lease has been
	// released. Preserve the claim for exact replay, but return the same zero
	// lease in both the first response and replay.
	op.DispatchLeaseUntil = time.Time{}
	op.Revision++
	op.CompletionFingerprint = fingerprint
	t.Revision++
	op.UpdatedAt = now
	t.UpdatedAt = now
	if code == "" {
		readback = SanitizeReadback(readback)
		readback.Digest = ReadbackDigest(readback)
		t.Result = &coretask.Result{JSON: mustJSON(readback)}
		if w, ok := s.workloads[op.WorkloadID]; ok {
			w.Identity = readback.Identity
			if op.Kind == OperationDestroy {
				w.State = "destroyed"
			} else {
				w.State = "ready"
			}
			w.Revision++
			w.UpdatedAt = now
			w.Actual = ActualSnapshot{WorkloadID: op.WorkloadID, Revision: w.Revision, State: w.State, Identity: readback.Identity, AppliedPlanID: op.PlanID, AppliedPlanDigest: op.PlanDigest, ReadbackDigest: readback.Digest, ProviderVersion: readback.ProviderVersion, ObservedAt: readback.At, UpdatedAt: now}
			s.workloads[op.WorkloadID] = w
		} else if op.Kind == OperationApply {
			actual := ActualSnapshot{WorkloadID: op.WorkloadID, Revision: 1, State: "ready", Identity: readback.Identity, AppliedPlanID: op.PlanID, AppliedPlanDigest: op.PlanDigest, ReadbackDigest: readback.Digest, ProviderVersion: readback.ProviderVersion, ObservedAt: readback.At, UpdatedAt: now}
			s.workloads[op.WorkloadID] = Workload{ID: op.WorkloadID, Revision: 1, PlanID: op.PlanID, PlanDigest: op.PlanDigest, TargetKind: op.TargetKind, Identity: readback.Identity, State: "ready", UpdatedAt: now, Actual: actual}
		}
	} else {
		if w, ok := s.workloads[op.WorkloadID]; ok {
			w.State = "failed"
			if code == "provider_uncertain" {
				w.State = "uncertain"
			}
			w.Revision++
			w.UpdatedAt = now
			s.workloads[op.WorkloadID] = w
		}
	}
	s.operations[id], s.tasks[taskID] = op, t
	_, safeSummary := SafeFailure(code, summary)
	s.events[id] = append(s.events[id], Event{OperationID: id, Sequence: uint64(len(s.events[id]) + 1), Kind: "terminal", Status: op.Status, Message: safeSummary, At: now})
	return op, t, nil
}
func (s *MemoryStore) CompleteDispatch(ctx context.Context, id, taskID, claim string, epoch uint64, code string, readback Readback, summary string) (Operation, coretask.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.operations[id]
	if !ok {
		return Operation{}, coretask.Task{}, ErrNotFound
	}
	return s.completeLocked(ctx, id, taskID, code, readback, summary, claim, epoch, nil)
}

func (s *MemoryStore) CompleteDispatchFenced(ctx context.Context, id, taskID, claim string, epoch uint64, code string, readback Readback, summary string, fence TaskFence) (Operation, coretask.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !fence.Valid(s.now().UTC()) {
		return Operation{}, coretask.Task{}, ErrRevisionConflict
	}
	return s.completeLocked(ctx, id, taskID, code, readback, summary, claim, epoch, &fence)
}
func (s *MemoryStore) RenewDispatchLease(_ context.Context, id, claim string, epoch uint64) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	now := s.now().UTC()
	if op.DispatchClaim != claim || op.DispatchEpoch != epoch || !op.DispatchLeaseUntil.After(now) {
		return Operation{}, ErrRevisionConflict
	}
	t := s.tasks[op.TaskID]
	if t.Status != coretask.StatusRunning || t.Lease == nil || t.Lease.Holder != "workload-handler" || t.Lease.Epoch != epoch || !t.Lease.ExpiresAt.After(now) {
		return Operation{}, ErrRevisionConflict
	}
	op.DispatchLeaseUntil = now.Add(time.Hour)
	op.UpdatedAt = now
	op.Revision++
	t.Lease.ExpiresAt = op.DispatchLeaseUntil
	t.UpdatedAt = now
	t.Revision++
	s.operations[id] = op
	s.tasks[t.ID] = t
	return op, nil
}

func (s *MemoryStore) RenewDispatchLeaseFenced(_ context.Context, id, claim string, epoch uint64, fence TaskFence) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if !fence.Valid(time.Time{}) {
		return Operation{}, ErrRevisionConflict
	}
	op, ok := s.operations[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	t, ok := s.tasks[op.TaskID]
	if !ok || !sameTaskFence(t, fence) || op.DispatchClaim != claim || op.DispatchEpoch != epoch || op.Status != OperationRunning || !op.DispatchLeaseUntil.After(now) {
		return Operation{}, ErrRevisionConflict
	}
	op.DispatchLeaseUntil = t.Lease.ExpiresAt
	op.UpdatedAt = now
	op.Revision++
	s.operations[id] = op
	return op, nil
}

func (s *MemoryStore) RecoverClaim(_ context.Context, id, requestedClaim string) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	op, ok := s.operations[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	now := s.now().UTC()
	if requestedClaim != "" && requestedClaim == op.DispatchClaim && op.DispatchLeaseUntil.After(now) && op.DispatchState != "dispatched" {
		return op, nil
	}
	if op.Status == OperationRunning && op.DispatchState == "dispatched" && op.DispatchClaim != "" && op.DispatchLeaseUntil.After(now) && requestedClaim != op.DispatchClaim {
		return op, ErrRevisionConflict
	}
	if op.Status == OperationRunning && op.DispatchState == "uncertain" && op.DispatchClaim != "" && op.DispatchClaim != requestedClaim && op.DispatchLeaseUntil.After(now) {
		return op, ErrRevisionConflict
	}
	if op.Status != OperationRunning || (op.DispatchState != "dispatched" && op.DispatchState != "uncertain") {
		return op, nil
	}
	op.DispatchState = "uncertain"
	op.DispatchEpoch++
	if requestedClaim == "" || requestedClaim != op.DispatchClaim {
		op.DispatchClaim = uuid.NewString()
	}
	op.DispatchLeaseUntil = now.Add(30 * time.Second)
	op.Revision++
	op.UpdatedAt = now
	s.operations[id] = op
	if t, ok := s.tasks[op.TaskID]; ok && t.Lease != nil {
		t.LeaseEpoch = op.DispatchEpoch
		t.Lease.Epoch = op.DispatchEpoch
		t.Lease.ExpiresAt = op.DispatchLeaseUntil
		t.Revision++
		t.UpdatedAt = now
		s.tasks[t.ID] = t
	}
	events := s.events[id]
	events = append(events, Event{OperationID: id, Sequence: uint64(len(events) + 1), Kind: "recovery_claim", Status: OperationRunning, Message: "read-only recovery claimed dispatch fence", At: op.UpdatedAt})
	s.events[id] = events
	return op, nil
}

func (s *MemoryStore) RecoverClaimFenced(_ context.Context, id, requestedClaim string, fence TaskFence) (Operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	if !fence.Valid(time.Time{}) {
		return Operation{}, ErrRevisionConflict
	}
	op, ok := s.operations[id]
	if !ok {
		return Operation{}, ErrNotFound
	}
	t, ok := s.tasks[op.TaskID]
	if !ok || !sameTaskFence(t, fence) {
		return Operation{}, ErrRevisionConflict
	}
	if op.Status != OperationRunning || (op.DispatchState != "dispatched" && op.DispatchState != "uncertain") {
		return op, nil
	}
	if requestedClaim != "" && requestedClaim == op.DispatchClaim && op.DispatchLeaseUntil.After(now) {
		return op, nil
	}
	// The generic task lease has already been reclaimed to this exact fence.
	// A successor may therefore take the read-only recovery claim even if the
	// old operation dispatch lease has not yet elapsed.
	op.DispatchState = "uncertain"
	op.DispatchEpoch = fence.LeaseEpoch
	op.DispatchClaim = uuid.NewString()
	op.DispatchLeaseUntil = t.Lease.ExpiresAt
	op.Revision++
	op.UpdatedAt = now
	s.operations[id] = op
	s.events[id] = append(s.events[id], Event{OperationID: id, Sequence: uint64(len(s.events[id]) + 1), Kind: "recovery_claim", Status: OperationRunning, Message: "read-only recovery claimed dispatch fence", At: now})
	return op, nil
}
