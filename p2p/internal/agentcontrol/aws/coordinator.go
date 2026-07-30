package aws

import (
	"context"
	"sync"
	"time"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
)

// MemoryChangeCoordinator owns the complete AWS lifecycle in one serialized
// boundary. The repository lock is held while each state transition, event and
// replay snapshot is committed; provider calls happen only after Consume.
type MemoryChangeCoordinator struct {
	mu               sync.Mutex
	repo             *MemoryRepository
	now              func() time.Time
	FailBeforeCommit bool
}

func NewMemoryChangeCoordinator(repo *MemoryRepository, confirmations ConfirmationPort, tasks TaskPort, now func() time.Time) *MemoryChangeCoordinator {
	if now == nil {
		now = time.Now
	}
	return &MemoryChangeCoordinator{repo: repo, now: now}
}
func (m *MemoryChangeCoordinator) InjectFailure(fail bool) {
	if m != nil {
		m.FailBeforeCommit = fail
	}
}

func (m *MemoryChangeCoordinator) RequestChange(ctx context.Context, in RequestChangeInput) (ChangeRequestResult, error) {
	if m == nil || m.repo == nil {
		return ChangeRequestResult{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	digest := canonicalDigest(struct {
		PlanID, ProvisionID string
		Binding             coreconfirmation.Binding
	}{in.PlanID, in.ProvisionID, in.Binding})
	m.repo.mu.Lock()
	defer m.repo.mu.Unlock()
	if v, ok, e := m.repo.replayLocked("change-request", in.IdempotencyKey, digest); ok {
		if e != nil {
			return ChangeRequestResult{}, e
		}
		return *v.change, nil
	}
	if m.FailBeforeCommit {
		return ChangeRequestResult{}, ErrConflict
	}
	p, ok := m.repo.plans[in.PlanID]
	if !ok {
		return ChangeRequestResult{}, ErrNotFound
	}
	if in.ProvisionID != "" {
		provision, found := m.repo.provisions[in.ProvisionID]
		if !found || provision.State == "destroyed" || provision.ActiveChangeID != "" || !ProvisionPlanMatches(provision, p) {
			return ChangeRequestResult{}, ErrRevisionConflict
		}
	}
	cred, ok := m.repo.credentialHistory[p.CredentialID][p.CredentialRevision]
	if !ok {
		return ChangeRequestResult{}, ErrNotFound
	}
	binding := bindingForPlan(p, cred)
	now := m.now().UTC()
	for _, existing := range m.repo.confirmations {
		if existing.Binding.TargetID == p.ID && (existing.State == coreconfirmation.StatePending || existing.State == coreconfirmation.StateConfirmed || existing.State == coreconfirmation.StateConsumed) {
			live := existing.State == coreconfirmation.StateConsumed && m.repo.reservations[existing.ConfirmationID].Active
			if (existing.State == coreconfirmation.StatePending || existing.State == coreconfirmation.StateConfirmed) && existing.ExpiresAt.After(now) {
				live = true
			}
			if live {
				return ChangeRequestResult{}, ErrConflict
			}
		}
	}
	task := Task{ID: newUUID(), Status: "waiting_user", Revision: 1, PlanID: p.ID}
	conf := coreconfirmation.Confirmation{ConfirmationID: newUUID(), Binding: binding, TaskID: task.ID, State: coreconfirmation.StatePending, Revision: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}
	task.ConfirmationID = conf.ConfirmationID
	c := Change{ID: newUUID(), PlanID: p.ID, CredentialID: p.CredentialID, ProvisionID: in.ProvisionID, TaskID: task.ID, ConfirmationID: conf.ConfirmationID, Operation: p.Operation, Status: ChangeWaitingUser, Stage: StageRequested, Revision: 1, ProviderToken: conf.ConfirmationID, ProviderRequestDigest: providerRequestDigest(p, conf.ConfirmationID), CreatedAt: now, UpdatedAt: now}
	if in.ProvisionID != "" {
		provision := m.repo.provisions[in.ProvisionID]
		if p.Operation == OperationCreate {
			provision.State = "creating"
		} else if p.Operation == OperationDelete {
			provision.State = "destroying"
		} else {
			return ChangeRequestResult{}, ErrInvalid
		}
		provision.ActiveChangeID, provision.Revision, provision.UpdatedAt = c.ID, provision.Revision+1, now
		m.repo.provisions[in.ProvisionID] = provision
	}
	out := ChangeRequestResult{Change: c, Task: task, Confirmation: conf}
	m.repo.changes[c.ID], m.repo.tasks[task.ID], m.repo.confirmations[conf.ConfirmationID] = c, task, conf
	m.repo.events = append(m.repo.events, ChangeEvent{ChangeID: c.ID, TaskID: task.ID, Kind: "change_requested", Revision: c.Revision, At: now})
	m.repo.replays[replayKey("change-request", in.IdempotencyKey)] = memoryReplay{digest: digest, change: &out}
	return out, nil
}

func ProvisionPlanMatches(provision Provision, plan Plan) bool {
	if plan.Operation == OperationCreate {
		return provision.State == "planned" && provisionSnapshotMatches(provision, plan)
	}
	return plan.Operation == OperationDelete && provision.State == "active" && provision.CredentialID == plan.CredentialID && provision.CredentialRevision == plan.CredentialRevision && provision.Region == plan.Region && provision.StackName == plan.StackName && provision.TemplateSHA256 == plan.TemplateSHA256 && provision.Profile == plan.Tags["service"] && provision.OwnerDigest == plan.Tags["owner"] && validOwnerDigest(provision.OwnerDigest)
}

// provisionSnapshotMatches binds every immutable provision field to the
// locked create plan. A caller cannot smuggle a different template, owner,
// credential revision or profile by supplying a forged Provision DTO.
func provisionSnapshotMatches(provision Provision, plan Plan) bool {
	return plan.Operation == OperationCreate && provision.PlanID == plan.ID &&
		provision.CredentialID == plan.CredentialID && provision.CredentialRevision == plan.CredentialRevision &&
		provision.Region == plan.Region && provision.StackName == plan.StackName &&
		provision.Profile == plan.Tags["service"] && provision.OwnerDigest == plan.Tags["owner"] &&
		validOwnerDigest(provision.OwnerDigest) && provision.PlanRevision == plan.Revision &&
		provision.TemplateSHA256 == plan.TemplateSHA256 && provision.PlanDigest == planDigest(plan)
}

// ProvisionSnapshotMatches exposes the immutable binding check to storage
// adapters without exposing mutable coordinator state.
func ProvisionSnapshotMatches(provision Provision, plan Plan) bool {
	return provisionSnapshotMatches(provision, plan)
}

func bindingEmpty(b coreconfirmation.Binding) bool {
	return b.OperationDomain == "" && b.TargetID == "" && b.TargetRevision == 0 && b.SourceVersion == "" && b.SourceCommit == "" && b.ContentDigest == "" && b.ParameterDigest == "" && b.NetworkDigest == "" && b.SecretGrantDigest == ""
}

// SetTaskRunning records the scheduler's exact lease fence before Consume.
func (m *MemoryChangeCoordinator) SetTaskRunning(taskID string, attempt uint32, leaseEpoch uint64, expectedRevision int64) error {
	if m == nil || !validUUID(taskID) || attempt == 0 || leaseEpoch == 0 || expectedRevision < 1 {
		return ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repo.mu.Lock()
	defer m.repo.mu.Unlock()
	t, ok := m.repo.tasks[taskID]
	if !ok {
		return ErrNotFound
	}
	if t.Revision != expectedRevision || t.Status != "queued" {
		return ErrRevisionConflict
	}
	t.Status, t.Attempt, t.LeaseEpoch, t.Revision = "running", attempt, leaseEpoch, t.Revision+1
	m.repo.tasks[taskID] = t
	return nil
}

func (m *MemoryChangeCoordinator) ConsumeChange(_ context.Context, cmd ConsumeChangeCommand) (Reservation, error) {
	if m == nil || m.repo == nil || !validUUID(cmd.ChangeID) || !validUUID(cmd.ConfirmationID) || !validUUID(cmd.TaskID) || !validUUID(cmd.IdempotencyKey) || cmd.Attempt == 0 || cmd.LeaseEpoch == 0 || cmd.ExpectedChangeRevision < 1 || cmd.ExpectedConfirmationRevision < 1 || cmd.ExpectedTaskRevision < 1 {
		return Reservation{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repo.mu.Lock()
	defer m.repo.mu.Unlock()
	digest := canonicalDigest(cmd)
	if v, ok, e := m.repo.replayLocked("change-consume", cmd.IdempotencyKey, digest); ok {
		if e != nil {
			return Reservation{}, e
		}
		r := m.repo.reservations[cmd.ConfirmationID]
		if !r.Active || r.TaskID != cmd.TaskID || r.Attempt != cmd.Attempt || r.LeaseEpoch != cmd.LeaseEpoch || r.TaskRevision != cmd.ExpectedTaskRevision {
			return Reservation{}, ErrRevisionConflict
		}
		_ = v
		return r, nil
	}
	c, ok := m.repo.changes[cmd.ChangeID]
	if !ok {
		return Reservation{}, ErrNotFound
	}
	t, tok := m.repo.tasks[cmd.TaskID]
	conf, cok := m.repo.confirmations[cmd.ConfirmationID]
	if tok && cok && c.TaskID == cmd.TaskID && c.ConfirmationID == cmd.ConfirmationID && c.Revision == cmd.ExpectedChangeRevision && t.Revision == cmd.ExpectedTaskRevision && conf.Revision == cmd.ExpectedConfirmationRevision && conf.State == coreconfirmation.StateConsumed {
		r := m.repo.reservations[cmd.ConfirmationID]
		if r.Active && r.TaskID == cmd.TaskID && r.Attempt == cmd.Attempt && r.LeaseEpoch == cmd.LeaseEpoch && r.TaskRevision == cmd.ExpectedTaskRevision {
			return r, nil
		}
		return Reservation{}, ErrRevisionConflict
	}
	if !tok || !cok || c.TaskID != cmd.TaskID || c.ConfirmationID != cmd.ConfirmationID || c.Revision != cmd.ExpectedChangeRevision || conf.Revision != cmd.ExpectedConfirmationRevision || t.Revision != cmd.ExpectedTaskRevision || conf.State != coreconfirmation.StateConfirmed || t.Status != "running" || t.Attempt != cmd.Attempt || t.LeaseEpoch != cmd.LeaseEpoch || (c.Stage != StageRequested && c.Stage != StageChangeSetCreating) {
		if tok && cok && c.TaskID == cmd.TaskID && c.ConfirmationID == cmd.ConfirmationID && t.Status == "running" && t.Attempt == cmd.Attempt && t.LeaseEpoch == cmd.LeaseEpoch && t.Revision == cmd.ExpectedTaskRevision && c.Revision == cmd.ExpectedChangeRevision && conf.Revision == cmd.ExpectedConfirmationRevision && conf.State != coreconfirmation.StateConsumed {
			now := m.now().UTC()
			conf.State, conf.Revision, conf.UpdatedAt = coreconfirmation.StateExpired, conf.Revision+1, now
			t.Status, t.FailureCode, t.FailureSummary, t.Revision = "failed", "confirmation_unconfirmed", "AWS confirmation is not confirmed", t.Revision+1
			c.Status, c.Stage, c.ErrorCode, c.ErrorSummary, c.Revision, c.UpdatedAt = ChangeFailed, StageFailed, "confirmation_unconfirmed", "AWS confirmation is not confirmed", c.Revision+1, now
			m.repo.confirmations[conf.ConfirmationID], m.repo.tasks[t.ID], m.repo.changes[c.ID] = conf, t, c
			m.repo.events = append(m.repo.events, ChangeEvent{ChangeID: c.ID, TaskID: t.ID, Kind: "change_fenced_failed", Revision: c.Revision, At: now})
		}
		return Reservation{}, ErrRevisionConflict
	}
	now := m.now().UTC()
	p, pok := m.repo.plans[c.PlanID]
	cred, cok := m.repo.credentialHistory[p.CredentialID][p.CredentialRevision]
	if !pok || !cok || !conf.ExpiresAt.After(now) || !conf.Binding.Equal(bindingForPlan(p, cred)) || (!bindingEmpty(cmd.Binding) && !conf.Binding.Equal(cmd.Binding)) {
		conf.State, conf.Revision, conf.UpdatedAt = coreconfirmation.StateExpired, conf.Revision+1, now
		t.Status, t.FailureCode, t.FailureSummary, t.Revision = "failed", "confirmation_stale", "AWS confirmation binding is stale or expired", t.Revision+1
		c.Status, c.Stage, c.ErrorCode, c.ErrorSummary, c.Revision, c.UpdatedAt = ChangeFailed, StageFailed, "confirmation_stale", "AWS confirmation binding is stale or expired", c.Revision+1, now
		m.repo.confirmations[conf.ConfirmationID], m.repo.tasks[t.ID], m.repo.changes[c.ID] = conf, t, c
		m.repo.events = append(m.repo.events, ChangeEvent{ChangeID: c.ID, TaskID: t.ID, Kind: "change_fenced_failed", Revision: c.Revision, At: now})
		m.repo.replays[replayKey("change-consume", cmd.IdempotencyKey)] = memoryReplay{digest: digest, err: ErrRevisionConflict}
		return Reservation{}, ErrRevisionConflict
	}
	if m.FailBeforeCommit {
		return Reservation{}, ErrConflict
	}
	conf.State, conf.Revision, conf.UpdatedAt = coreconfirmation.StateConsumed, conf.Revision+1, now
	c.Status, c.Stage, c.Revision, c.UpdatedAt = ChangeRunning, StageChangeSetCreating, c.Revision+1, now
	r := Reservation{ConfirmationID: conf.ConfirmationID, TaskID: t.ID, Attempt: t.Attempt, LeaseEpoch: t.LeaseEpoch, TaskRevision: t.Revision, Active: true}
	m.repo.confirmations[conf.ConfirmationID], m.repo.changes[c.ID], m.repo.reservations[conf.ConfirmationID] = conf, c, r
	m.repo.events = append(m.repo.events, ChangeEvent{ChangeID: c.ID, TaskID: t.ID, Kind: "change_consumed", Revision: c.Revision, At: c.UpdatedAt})
	m.repo.replays[replayKey("change-consume", cmd.IdempotencyKey)] = memoryReplay{digest: digest, change: &ChangeRequestResult{Change: c, Task: t, Confirmation: conf}}
	return r, nil
}

func (m *MemoryChangeCoordinator) CompleteChange(_ context.Context, cmd CompleteChangeCommand) (Change, error) {
	if m == nil || m.repo == nil {
		return Change{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repo.mu.Lock()
	defer m.repo.mu.Unlock()
	digest := canonicalDigest(cmd)
	replayID := cmd.ChangeID + ":" + cmd.ConfirmationID
	if v, ok, e := m.repo.replayLocked("change-complete", replayID, digest); ok {
		if e != nil {
			return Change{}, e
		}
		return v.change.Change, nil
	}
	c, ok := m.repo.changes[cmd.ChangeID]
	if !ok {
		return Change{}, ErrNotFound
	}
	if c.ConfirmationID != cmd.ConfirmationID || c.Revision != cmd.ExpectedChangeRevision || cmd.TaskID == "" || cmd.Attempt == 0 || cmd.LeaseEpoch == 0 || cmd.ExpectedTaskRevision < 1 || cmd.ExpectedConfirmationRevision < 1 {
		return Change{}, ErrRevisionConflict
	}
	t, tok := m.repo.tasks[cmd.TaskID]
	conf, cok := m.repo.confirmations[cmd.ConfirmationID]
	r, rok := m.repo.reservations[cmd.ConfirmationID]
	if !tok || !cok || !rok || c.TaskID != cmd.TaskID || t.Revision < cmd.ExpectedTaskRevision || conf.Revision != cmd.ExpectedConfirmationRevision || conf.State != coreconfirmation.StateConsumed || t.Attempt != cmd.Attempt || t.LeaseEpoch != cmd.LeaseEpoch || !r.Active || r.TaskID != cmd.TaskID || r.Attempt != cmd.Attempt || r.LeaseEpoch > cmd.LeaseEpoch || r.TaskRevision > t.Revision || t.Status != "running" {
		return Change{}, ErrRevisionConflict
	}
	if r.LeaseEpoch < cmd.LeaseEpoch && r.TaskRevision >= t.Revision {
		return Change{}, ErrRevisionConflict
	}
	if cmd.Status != ChangeSucceeded && cmd.Status != ChangeFailed && cmd.Status != ChangeCanceled {
		return Change{}, ErrInvalid
	}
	var provision Provision
	if c.ProvisionID != "" {
		var found bool
		provision, found = m.repo.provisions[c.ProvisionID]
		if !found || provision.ActiveChangeID != c.ID {
			return Change{}, ErrRevisionConflict
		}
		if cmd.Status == ChangeSucceeded && c.Operation == OperationCreate && (cmd.Readback == nil || cmd.Readback.Validate() != nil) {
			return Change{}, ErrRevisionConflict
		}
		if cmd.Readback != nil && (c.Operation != OperationCreate || cmd.Status != ChangeSucceeded) {
			return Change{}, ErrInvalid
		}
	}
	now := m.now().UTC()
	terminalStage := map[ChangeStatus]ChangeStage{ChangeSucceeded: StageSucceeded, ChangeFailed: StageFailed, ChangeCanceled: StageCanceled}[cmd.Status]
	if cmd.Status == ChangeCanceled && c.Stage == StageReconciling {
		terminalStage = StageReconciliationRequired
	}
	c.Status, c.Stage, c.ErrorCode, c.ErrorSummary, c.Revision, c.UpdatedAt = cmd.Status, terminalStage, cmd.ErrorCode, cmd.ErrorSummary, c.Revision+1, now
	t.Status, t.Revision = map[ChangeStatus]string{ChangeSucceeded: "succeeded", ChangeFailed: "failed", ChangeCanceled: "canceled"}[cmd.Status], t.Revision+1
	r.Active = false
	m.repo.changes[c.ID], m.repo.tasks[t.ID], m.repo.reservations[r.ConfirmationID] = c, t, r
	if c.ProvisionID != "" {
		provision.ActiveChangeID, provision.Revision, provision.UpdatedAt = "", provision.Revision+1, now
		if cmd.Status == ChangeSucceeded {
			if c.Operation == OperationCreate {
				provision.State, provision.CreateChangeID, provision.Readback, provision.ReconciliationRequired = "active", c.ID, *cmd.Readback, false
			} else if c.Operation == OperationDelete {
				provision.State, provision.DestroyChangeID, provision.ReconciliationRequired = "destroyed", c.ID, false
			}
		} else if cmd.Status == ChangeFailed {
			provision.State, provision.ReconciliationRequired, provision.ErrorCode, provision.ErrorSummary = "failed", false, cmd.ErrorCode, cmd.ErrorSummary
			if c.Operation == OperationDelete {
				provision.DestroyChangeID = c.ID
			} else if c.Operation == OperationCreate {
				provision.CreateChangeID = c.ID
			}
		} else {
			provision.State = "uncertain"
			provision.ReconciliationRequired = true
			if c.Operation == OperationDelete {
				provision.DestroyChangeID = c.ID
			} else if c.Operation == OperationCreate {
				provision.CreateChangeID = c.ID
			}
		}
		m.repo.provisions[provision.ID] = provision
		m.repo.events = append(m.repo.events, ChangeEvent{ChangeID: c.ID, TaskID: t.ID, Kind: "provision_" + provision.State, Revision: provision.Revision, At: now})
	}
	m.repo.events = append(m.repo.events, ChangeEvent{ChangeID: c.ID, TaskID: t.ID, Kind: "change_completed", Revision: c.Revision, At: now})
	m.repo.replays[replayKey("change-complete", replayID)] = memoryReplay{digest: digest, change: &ChangeRequestResult{Change: c}}
	return c, nil
}

func (m *MemoryChangeCoordinator) ExecutionFence(_ context.Context, confirmationID string) (ExecutionFence, error) {
	if m == nil || m.repo == nil || !validUUID(confirmationID) {
		return ExecutionFence{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repo.mu.RLock()
	defer m.repo.mu.RUnlock()
	conf, ok := m.repo.confirmations[confirmationID]
	if !ok {
		return ExecutionFence{}, ErrNotFound
	}
	c, ok := m.repo.changesByConfirmationLocked(confirmationID)
	if !ok {
		return ExecutionFence{}, ErrNotFound
	}
	t, ok := m.repo.tasks[c.TaskID]
	if !ok {
		return ExecutionFence{}, ErrNotFound
	}
	r := m.repo.reservations[confirmationID]
	return ExecutionFence{Change: c, Task: t, Confirmation: conf, Reservation: r}, nil
}

func (m *MemoryChangeCoordinator) ReconcileChange(ctx context.Context, cmd ReconcileChangeCommand) (Change, error) {
	fence, err := m.ExecutionFence(ctx, cmd.ConfirmationID)
	if err != nil {
		return Change{}, err
	}
	if fence.Change.ID != cmd.ChangeID || fence.Task.ID != cmd.TaskID || fence.Task.Attempt != cmd.Attempt || fence.Task.LeaseEpoch != cmd.LeaseEpoch || fence.Change.Revision != cmd.ExpectedChangeRevision || fence.Task.Revision < cmd.ExpectedTaskRevision || fence.Confirmation.Revision != cmd.ExpectedConfirmationRevision || fence.Reservation.TaskRevision > fence.Task.Revision || fence.Reservation.LeaseEpoch > cmd.LeaseEpoch || !fence.Reservation.Active || fence.Change.ProviderToken != cmd.ProviderToken {
		return Change{}, ErrRevisionConflict
	}
	if fence.Change.ChangeSetID != cmd.ProviderChangeSetID {
		return Change{}, ErrRevisionConflict
	}
	if cmd.Success {
		return m.CompleteChange(ctx, CompleteChangeCommand{ChangeID: cmd.ChangeID, ConfirmationID: cmd.ConfirmationID, TaskID: cmd.TaskID, Attempt: cmd.Attempt, LeaseEpoch: cmd.LeaseEpoch, ExpectedChangeRevision: cmd.ExpectedChangeRevision, ExpectedTaskRevision: cmd.ExpectedTaskRevision, ExpectedConfirmationRevision: cmd.ExpectedConfirmationRevision, Status: ChangeSucceeded})
	}
	return m.CompleteChange(ctx, CompleteChangeCommand{ChangeID: cmd.ChangeID, ConfirmationID: cmd.ConfirmationID, TaskID: cmd.TaskID, Attempt: cmd.Attempt, LeaseEpoch: cmd.LeaseEpoch, ExpectedChangeRevision: cmd.ExpectedChangeRevision, ExpectedTaskRevision: cmd.ExpectedTaskRevision, ExpectedConfirmationRevision: cmd.ExpectedConfirmationRevision, Status: ChangeFailed, ErrorCode: cmd.ErrorCode, ErrorSummary: cmd.ErrorSummary})
}

func (m *MemoryChangeCoordinator) ClaimProviderMutation(ctx context.Context, cmd ProviderMutationCommand) (ExecutionFence, error) {
	f, err := m.ExecutionFence(ctx, cmd.ConfirmationID)
	if err != nil {
		return ExecutionFence{}, err
	}
	if f.Change.ID != cmd.ChangeID || f.Task.ID != cmd.TaskID || f.Task.Attempt != cmd.Attempt || f.Task.LeaseEpoch != cmd.LeaseEpoch || f.Change.Revision != cmd.ExpectedChangeRevision || f.Task.Revision != cmd.ExpectedTaskRevision || f.Confirmation.Revision != cmd.ExpectedConfirmationRevision || !f.Reservation.Active || f.Confirmation.State != coreconfirmation.StateConsumed || f.Task.Status != "running" {
		return ExecutionFence{}, ErrRevisionConflict
	}
	m.repo.mu.RLock()
	p, pok := m.repo.plans[f.Change.PlanID]
	cred, cok := m.repo.credentialHistory[p.CredentialID][p.CredentialRevision]
	bindingOK := pok && cok && f.Confirmation.Binding.Equal(bindingForPlan(p, cred))
	m.repo.mu.RUnlock()
	if !bindingOK {
		return ExecutionFence{}, ErrRevisionConflict
	}
	switch cmd.Kind {
	case ProviderMutationCreate:
		if f.Change.Stage != StageChangeSetCreating {
			return ExecutionFence{}, ErrRevisionConflict
		}
	case ProviderMutationExecute:
		if f.Change.Stage != StageChangeSetReady || f.Change.ChangeSetID != cmd.ProviderChangeSetID {
			return ExecutionFence{}, ErrRevisionConflict
		}
	case ProviderMutationDelete:
		if f.Change.Operation != OperationDelete || f.Change.Stage != StageChangeSetCreating {
			return ExecutionFence{}, ErrRevisionConflict
		}
	default:
		return ExecutionFence{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repo.mu.Lock()
	defer m.repo.mu.Unlock()
	current, ok := m.repo.changes[cmd.ChangeID]
	currentTask, taskOK := m.repo.tasks[cmd.TaskID]
	currentConfirmation, confirmationOK := m.repo.confirmations[cmd.ConfirmationID]
	currentReservation, reservationOK := m.repo.reservations[cmd.ConfirmationID]
	if !ok ||
		!taskOK ||
		!confirmationOK ||
		!reservationOK ||
		current.Revision != cmd.ExpectedChangeRevision ||
		current.Stage != f.Change.Stage ||
		current.Status != ChangeRunning ||
		currentTask.Status != "running" ||
		currentTask.Revision != cmd.ExpectedTaskRevision ||
		currentTask.Attempt != cmd.Attempt ||
		currentTask.LeaseEpoch != cmd.LeaseEpoch ||
		currentConfirmation.State != coreconfirmation.StateConsumed ||
		currentConfirmation.Revision != cmd.ExpectedConfirmationRevision ||
		!currentReservation.Active ||
		currentReservation.TaskID != cmd.TaskID ||
		currentReservation.Attempt != cmd.Attempt ||
		currentReservation.LeaseEpoch > cmd.LeaseEpoch ||
		currentReservation.TaskRevision > currentTask.Revision {
		return ExecutionFence{}, ErrRevisionConflict
	}
	if currentReservation.LeaseEpoch < cmd.LeaseEpoch {
		if currentReservation.TaskRevision >= currentTask.Revision {
			return ExecutionFence{}, ErrRevisionConflict
		}
		currentReservation.LeaseEpoch = cmd.LeaseEpoch
		currentReservation.TaskRevision = currentTask.Revision
		currentConfirmation.Revision++
		m.repo.reservations[cmd.ConfirmationID] = currentReservation
		m.repo.confirmations[cmd.ConfirmationID] = currentConfirmation
	}
	current.Stage = StageReconciling
	current.Revision++
	current.UpdatedAt = m.now().UTC()
	m.repo.changes[current.ID] = current
	m.repo.events = append(m.repo.events, ChangeEvent{ChangeID: current.ID, TaskID: f.Task.ID, Kind: "provider_mutation_dispatched", Revision: current.Revision, At: current.UpdatedAt})
	f.Change = current
	f.Task = currentTask
	f.Confirmation = currentConfirmation
	f.Reservation = currentReservation
	return f, nil
}

func (m *MemoryChangeCoordinator) CommitProviderMutation(_ context.Context, result ProviderMutationResult) (Change, error) {
	cmd := result.Command
	if m == nil || m.repo == nil {
		return Change{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repo.mu.Lock()
	defer m.repo.mu.Unlock()
	c, ok := m.repo.changes[cmd.ChangeID]
	if !ok {
		return Change{}, ErrNotFound
	}
	t, tok := m.repo.tasks[cmd.TaskID]
	conf, cok := m.repo.confirmations[cmd.ConfirmationID]
	r, rok := m.repo.reservations[cmd.ConfirmationID]
	if !tok || !cok || !rok || c.TaskID != cmd.TaskID || c.ConfirmationID != cmd.ConfirmationID || c.Revision != cmd.ExpectedChangeRevision || t.Revision < cmd.ExpectedTaskRevision || conf.Revision != cmd.ExpectedConfirmationRevision || t.Attempt != cmd.Attempt || t.LeaseEpoch != cmd.LeaseEpoch || !r.Active || r.Attempt != cmd.Attempt || r.LeaseEpoch != cmd.LeaseEpoch || r.TaskRevision > t.Revision || conf.State != coreconfirmation.StateConsumed || t.Status != "running" {
		return Change{}, ErrRevisionConflict
	}
	now := m.now().UTC()
	if c.Stage != StageReconciling {
		return Change{}, ErrRevisionConflict
	}
	switch cmd.Kind {
	case ProviderMutationCreate:
		if c.Operation == OperationDelete || result.Success && !result.ResponseUncertain && result.ProviderChangeSetID == "" {
			return Change{}, ErrRevisionConflict
		}
	case ProviderMutationExecute:
		if c.ChangeSetID == "" || c.ChangeSetID != cmd.ProviderChangeSetID {
			return Change{}, ErrRevisionConflict
		}
	case ProviderMutationDelete:
		if c.Operation != OperationDelete {
			return Change{}, ErrRevisionConflict
		}
	default:
		return Change{}, ErrInvalid
	}
	if result.ResponseUncertain {
		c.Stage, c.Revision, c.UpdatedAt = StageReconciling, c.Revision+1, now
	} else if !result.Success {
		c.Status, c.Stage, c.ErrorCode, c.ErrorSummary, c.Revision, c.UpdatedAt = ChangeFailed, StageFailed, result.ErrorCode, result.ErrorSummary, c.Revision+1, now
	} else {
		switch cmd.Kind {
		case ProviderMutationCreate:
			c.ChangeSetID, c.Stage = result.ProviderChangeSetID, StageChangeSetReady
		case ProviderMutationExecute:
			c.Stage = StageExecuting
		case ProviderMutationDelete:
			c.Stage = StageExecuting
		}
		c.Revision++
		c.UpdatedAt = now
	}
	m.repo.changes[c.ID] = c
	m.repo.events = append(m.repo.events, ChangeEvent{ChangeID: c.ID, TaskID: t.ID, Kind: "provider_mutation_committed", Revision: c.Revision, At: now})
	// Provider failures are completed by Service.ExecuteChange via the common
	// CompleteChange boundary. Keep the reservation and running task fence
	// intact until that CAS succeeds.
	return c, nil
}

func (m *MemoryChangeCoordinator) PersistChangeSetEvidence(_ context.Context, cmd ChangeSetEvidenceCommand) (Change, error) {
	if m == nil || m.repo == nil || cmd.ProviderChangeSetID == "" || cmd.Attempt == 0 || cmd.LeaseEpoch == 0 {
		return Change{}, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.repo.mu.Lock()
	defer m.repo.mu.Unlock()
	c, ok := m.repo.changes[cmd.ChangeID]
	if !ok {
		return Change{}, ErrNotFound
	}
	t, tok := m.repo.tasks[cmd.TaskID]
	conf, cok := m.repo.confirmations[cmd.ConfirmationID]
	r, rok := m.repo.reservations[cmd.ConfirmationID]
	if !tok || !cok || !rok || c.TaskID != cmd.TaskID || c.ConfirmationID != cmd.ConfirmationID || c.Revision != cmd.ExpectedChangeRevision || t.Revision < cmd.ExpectedTaskRevision || conf.Revision != cmd.ExpectedConfirmationRevision || !r.Active || r.TaskRevision > t.Revision || t.Status != "running" || t.Attempt != cmd.Attempt || t.LeaseEpoch != cmd.LeaseEpoch || conf.State != coreconfirmation.StateConsumed {
		return Change{}, ErrRevisionConflict
	}
	if r.Attempt != cmd.Attempt || r.LeaseEpoch > cmd.LeaseEpoch || (r.LeaseEpoch < cmd.LeaseEpoch && r.TaskRevision >= t.Revision) {
		return Change{}, ErrRevisionConflict
	}
	if r.LeaseEpoch < cmd.LeaseEpoch {
		r.LeaseEpoch = cmd.LeaseEpoch
		r.TaskRevision = t.Revision
		m.repo.reservations[cmd.ConfirmationID] = r
	}
	c.ChangeSetID, c.Stage, c.Revision, c.UpdatedAt = cmd.ProviderChangeSetID, StageChangeSetReady, c.Revision+1, m.now().UTC()
	m.repo.changes[c.ID] = c
	return c, nil
}
