package confirmation

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

type replay struct {
	digest Digest
	value  Confirmation
}

// MemoryRepository is a race-safe reference implementation used by domain
// tests and by callers before the PostgreSQL adapter exists.
type MemoryRepository struct {
	mu               sync.Mutex
	now              func() time.Time
	items            map[string]Confirmation
	order            []string
	replays          map[string]replay
	uncertainReplays map[string]struct {
		digest Digest
		value  AcknowledgeExtensionExecutionUncertainResult
	}
	taskFences     map[string]TaskFence
	targetBindings map[string]Binding
	reservations   map[string]Reservation
	taskEvents     map[string]int64
}

func NewMemoryRepository(now ...func() time.Time) *MemoryRepository {
	clock := time.Now
	if len(now) > 0 && now[0] != nil {
		clock = now[0]
	}
	return &MemoryRepository{now: clock, items: make(map[string]Confirmation), replays: make(map[string]replay), uncertainReplays: make(map[string]struct {
		digest Digest
		value  AcknowledgeExtensionExecutionUncertainResult
	}), taskFences: make(map[string]TaskFence), targetBindings: make(map[string]Binding), reservations: make(map[string]Reservation), taskEvents: make(map[string]int64)}
}

func (r *MemoryRepository) AcknowledgeExtensionExecutionUncertain(_ context.Context, command AcknowledgeExtensionExecutionUncertainCommand) (AcknowledgeExtensionExecutionUncertainResult, error) {
	if !validateUUID(command.ConfirmationID) || !validateUUID(command.TaskID) || !validateUUID(command.InstallationID) || !validateUUID(command.IdempotencyKey) || command.ExpectedTaskRevision < 1 || command.ExpectedConfirmationRevision < 1 || command.Resolution != ExtensionUncertainAcknowledgedUnknownNoRetry {
		return AcknowledgeExtensionExecutionUncertainResult{}, ErrInvalid
	}
	digest := AcknowledgeExtensionExecutionUncertainDigest(command)
	r.mu.Lock()
	defer r.mu.Unlock()
	key := "extension_execution_uncertain_ack:" + command.IdempotencyKey
	if previous, ok := r.uncertainReplays[key]; ok {
		if previous.digest != digest {
			return AcknowledgeExtensionExecutionUncertainResult{}, ErrIdempotencyConflict
		}
		return previous.value, nil
	}
	c, ok := r.items[command.ConfirmationID]
	if !ok {
		return AcknowledgeExtensionExecutionUncertainResult{}, ErrNotFound
	}
	if c.TaskID != command.TaskID || c.Binding.OperationDomain != "extension.execute" || c.Binding.TargetID != command.InstallationID || c.Binding.OwnerID == "" || c.State != StateConsumed || c.Revision != command.ExpectedConfirmationRevision {
		return AcknowledgeExtensionExecutionUncertainResult{}, ErrConflict
	}
	fence, ok := r.taskFences[command.TaskID]
	if !ok || fence.State != "failed" || fence.FailureCode != "extension_execution_uncertain" || fence.Revision != command.ExpectedTaskRevision {
		return AcknowledgeExtensionExecutionUncertainResult{}, ErrTaskFenceConflict
	}
	if fence.InstallationID == "" || fence.ConfirmationID == "" {
		return AcknowledgeExtensionExecutionUncertainResult{}, ErrBindingUnavailable
	}
	if fence.InstallationID != command.InstallationID || fence.ConfirmationID != command.ConfirmationID {
		return AcknowledgeExtensionExecutionUncertainResult{}, ErrConflict
	}
	reservation, ok := r.reservations[command.ConfirmationID]
	if !ok || !reservation.Active || reservation.TaskID != command.TaskID || reservation.AcquiredAttempt != fence.Attempt || reservation.AcquiredLeaseEpoch >= fence.LeaseEpoch || reservation.TaskRevision >= command.ExpectedTaskRevision {
		return AcknowledgeExtensionExecutionUncertainResult{}, ErrConflict
	}
	reservation.Active = false
	r.reservations[command.ConfirmationID] = reservation
	c.Revision++
	c.UpdatedAt = r.now().UTC()
	c.TerminalCode = "acknowledged_unknown_no_retry"
	c.TerminalReason = "acknowledged_unknown_no_retry"
	r.items[c.ConfirmationID] = c
	fence.Revision++
	r.taskFences[command.TaskID] = fence
	r.taskEvents[command.TaskID]++
	result := AcknowledgeExtensionExecutionUncertainResult{Confirmation: cloneConfirmation(c), Task: coretask.Task{ID: command.TaskID, Status: coretask.StatusFailed, Revision: uint64(fence.Revision), FailureCode: "extension_execution_uncertain"}, Resolution: command.Resolution, ReservationReleased: true}
	r.uncertainReplays[key] = struct {
		digest Digest
		value  AcknowledgeExtensionExecutionUncertainResult
	}{digest: digest, value: result}
	return result, nil
}

func (r *MemoryRepository) ReadTargetBinding(_ context.Context, confirmationID string) (Binding, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.items[confirmationID]
	if !ok {
		return Binding{}, ErrNotFound
	}
	if binding, ok := r.targetBindings[confirmationID]; ok {
		return binding, nil
	}
	return value.Binding, nil
}

func (r *MemoryRepository) SetTargetBinding(confirmationID string, binding Binding) error {
	binding, err := binding.normalized()
	if err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.items[confirmationID]; !ok {
		return ErrNotFound
	}
	r.targetBindings[confirmationID] = binding
	return nil
}

func (r *MemoryRepository) ReadTaskFence(_ context.Context, taskID string) (TaskFence, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	fence, ok := r.taskFences[taskID]
	if !ok {
		return TaskFence{}, ErrNotFound
	}
	return fence, nil
}

func (r *MemoryRepository) SetTaskFence(fence TaskFence) error {
	if !validateUUID(fence.TaskID) || fence.State == "" || fence.Attempt == 0 || fence.LeaseEpoch == 0 || fence.Revision < 1 {
		return ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.taskFences[fence.TaskID] = fence
	return nil
}

func (r *MemoryRepository) Request(_ context.Context, command RequestCommand) (Confirmation, error) {
	command.OwnerID = strings.TrimSpace(command.OwnerID)
	if command.OwnerID == "" || !validateUUID(command.IdempotencyKey) {
		return Confirmation{}, ErrInvalid
	}
	binding, err := command.Binding.normalized()
	if err != nil || !validateUUID(command.TaskID) {
		return Confirmation{}, ErrInvalid
	}
	if !binding.MatchesOwner(command.OwnerID) {
		return Confirmation{}, ErrInvalid
	}
	command.Binding = binding
	if isExecutionV2Domain(binding.OperationDomain) {
		return Confirmation{}, ErrConflict
	}
	command.RequestDigest = requestDigest(command)
	now := r.at(command.At)
	if command.ExpiresAt.IsZero() || command.ExpiresAt.Location() != time.UTC || !command.ExpiresAt.After(now) {
		return Confirmation{}, ErrInvalid
	}
	if !binding.MatchesConfirmationExpiry(command.ExpiresAt) {
		return Confirmation{}, ErrInvalid
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	replayKey := "request:" + command.OwnerID + ":" + command.IdempotencyKey
	if value, ok, conflict := r.replayLocked(replayKey, command.RequestDigest); ok || conflict != nil {
		return value, conflict
	}
	for id, existing := range r.items {
		reservation := r.reservations[id]
		if existing.OwnerID == command.OwnerID && existing.Binding.OperationDomain == binding.OperationDomain && existing.Binding.TargetID == binding.TargetID && isLiveConfirmation(existing) && (existing.State != StateConsumed || reservation.Active) {
			return Confirmation{}, ErrConflict
		}
	}
	id := uuid.New().String()
	value := Confirmation{ConfirmationID: id, OwnerID: command.OwnerID, Binding: binding, TaskID: strings.TrimSpace(command.TaskID), State: StatePending, Revision: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: command.ExpiresAt}
	r.items[id] = cloneConfirmation(value)
	r.targetBindings[id] = binding
	r.order = append(r.order, id)
	r.replays[replayKey] = replay{digest: command.RequestDigest, value: cloneConfirmation(value)}
	return cloneConfirmation(value), nil
}

func (r *MemoryRepository) Get(_ context.Context, id string) (Confirmation, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.items[id]
	if !ok {
		return Confirmation{}, ErrNotFound
	}
	return cloneConfirmation(value), nil
}

func (r *MemoryRepository) List(_ context.Context, query ListQuery) (Page, error) {
	if query.PageSize < 0 || query.PageSize > 100 {
		return Page{}, ErrInvalid
	}
	owner := strings.TrimSpace(query.OwnerID)
	if owner == "" {
		return Page{}, ErrInvalid
	}
	pageSize := query.PageSize
	if pageSize == 0 {
		pageSize = 50
	}
	filter := ListFilterDigest(query.Domain, query.TargetID, query.States)
	var cursor listCursor
	if query.PageToken != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(query.PageToken)
		if err != nil || json.Unmarshal(decoded, &cursor) != nil || cursor.OwnerID != owner || cursor.Filter != filter {
			return Page{}, ErrInvalid
		}
	}
	allowed := make(map[State]struct{}, len(query.States))
	for _, state := range query.States {
		if !validState(state) {
			return Page{}, ErrInvalid
		}
		allowed[state] = struct{}{}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	values := make([]Confirmation, 0, len(r.order))
	for _, id := range r.order {
		value := r.items[id]
		if value.OwnerID != owner {
			continue
		}
		if cursor.ID != "" && (value.CreatedAt.Before(cursor.CreatedAt) || (value.CreatedAt.Equal(cursor.CreatedAt) && value.ConfirmationID <= cursor.ID)) {
			continue
		}
		if query.Domain != "" && value.Binding.OperationDomain != query.Domain || query.TargetID != "" && value.Binding.TargetID != query.TargetID {
			continue
		}
		if len(allowed) > 0 {
			if _, ok := allowed[value.State]; !ok {
				continue
			}
		}
		values = append(values, cloneConfirmation(value))
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].CreatedAt.Equal(values[j].CreatedAt) {
			return values[i].ConfirmationID < values[j].ConfirmationID
		}
		return values[i].CreatedAt.Before(values[j].CreatedAt)
	})
	end := pageSize
	if end > len(values) {
		end = len(values)
	}
	page := Page{Confirmations: values[:end]}
	if end < len(values) {
		last := values[end-1]
		encoded, _ := json.Marshal(listCursor{OwnerID: owner, CreatedAt: last.CreatedAt, ID: last.ConfirmationID, Filter: filter})
		page.NextPageToken = base64.RawURLEncoding.EncodeToString(encoded)
	}
	return page, nil
}

func normalizedStates(states []State) []State {
	result := append([]State(nil), states...)
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

type listCursor struct {
	OwnerID   string
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
	Filter    Digest    `json:"filter"`
}

func (r *MemoryRepository) Confirm(ctx context.Context, command ConfirmCommand) (Confirmation, error) {
	command.OwnerID = strings.TrimSpace(command.OwnerID)
	if command.OwnerID == "" || !validateUUID(command.IdempotencyKey) || !validateUUID(command.ConfirmationID) || command.ExpectedRevision < 1 {
		return Confirmation{}, ErrInvalid
	}
	command.RequestDigest = confirmDigest(command)
	now := r.at(command.At)
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.items[command.ConfirmationID]
	if !ok {
		return Confirmation{}, ErrNotFound
	}
	if value.OwnerID != command.OwnerID {
		return Confirmation{}, ErrConflict
	}
	if isExecutionV2Domain(value.Binding.OperationDomain) {
		return Confirmation{}, ErrConflict
	}
	key := "confirm:" + command.OwnerID + ":" + command.IdempotencyKey
	if replay, ok, conflict := r.replayLocked(key, command.RequestDigest); ok || conflict != nil {
		return replay, conflict
	}
	if value.Revision != command.ExpectedRevision {
		return Confirmation{}, ErrRevisionConflict
	}
	if value.State != StatePending {
		return Confirmation{}, ErrConflict
	}
	if !value.ExpiresAt.After(now) {
		value.State, value.Revision, value.UpdatedAt = StateExpired, value.Revision+1, now
		value.TerminalCode, value.TerminalReason = ReasonExpired, ReasonExpired
		r.items[value.ConfirmationID] = value
		return Confirmation{}, ErrExpired
	}
	authoritative := value.Binding
	if current, exists := r.targetBindings[command.ConfirmationID]; exists {
		authoritative = current
	}
	var binding Binding
	var err error
	if command.ResolveBinding != nil {
		binding, err = command.ResolveBinding(ctx)
	} else {
		binding, err = command.Binding.normalized()
	}
	if err != nil {
		return Confirmation{}, ErrStale
	}
	if !value.Binding.Equal(authoritative) || !value.Binding.Equal(binding) {
		value.State = StateExpired
		value.Revision++
		value.UpdatedAt = now
		value.TerminalCode, value.TerminalReason = ReasonStale, ReasonStale
		r.items[value.ConfirmationID] = value
		return Confirmation{}, ErrStale
	}
	value.State = StateConfirmed
	value.Revision++
	value.UpdatedAt = now
	r.items[value.ConfirmationID] = value
	r.replays[key] = replay{digest: command.RequestDigest, value: cloneConfirmation(value)}
	return cloneConfirmation(value), nil
}

func (r *MemoryRepository) Reject(_ context.Context, command RejectCommand) (Confirmation, error) {
	command.OwnerID = strings.TrimSpace(command.OwnerID)
	if command.OwnerID == "" || !validateUUID(command.IdempotencyKey) || !validateUUID(command.ConfirmationID) || command.ExpectedRevision < 1 {
		return Confirmation{}, ErrInvalid
	}
	now := r.at(command.At)
	command.RequestDigest = rejectDigest(command)
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.items[command.ConfirmationID]
	if !ok {
		return Confirmation{}, ErrNotFound
	}
	if value.OwnerID != command.OwnerID {
		return Confirmation{}, ErrConflict
	}
	if isExecutionV2Domain(value.Binding.OperationDomain) {
		return Confirmation{}, ErrConflict
	}
	key := "reject:" + command.OwnerID + ":" + command.IdempotencyKey
	if replay, ok, conflict := r.replayLocked(key, command.RequestDigest); ok || conflict != nil {
		return replay, conflict
	}
	if value.Revision != command.ExpectedRevision {
		return Confirmation{}, ErrRevisionConflict
	}
	if value.State == StatePending && !value.ExpiresAt.After(now) {
		value.State = StateExpired
		value.Revision++
		value.UpdatedAt = now
		value.TerminalCode = ReasonExpired
		value.TerminalReason = ReasonExpired
		r.items[value.ConfirmationID] = value
		return Confirmation{}, ErrExpired
	}
	if value.State != StatePending {
		return Confirmation{}, ErrConflict
	}
	value.State = StateRejected
	value.Revision++
	value.UpdatedAt = now
	value.TerminalCode = ReasonUserRejected
	value.TerminalReason = ReasonUserRejected
	value.TerminalNote = strings.TrimSpace(command.Note)
	if value.TerminalNote == "" {
		value.TerminalNote = strings.TrimSpace(command.Reason)
	}
	r.items[value.ConfirmationID] = value
	r.replays[key] = replay{digest: command.RequestDigest, value: cloneConfirmation(value)}
	return cloneConfirmation(value), nil
}

func (r *MemoryRepository) Consume(ctx context.Context, command ConsumeCommand) (Confirmation, error) {
	command.OwnerID = strings.TrimSpace(command.OwnerID)
	if command.OwnerID == "" || !validateUUID(command.IdempotencyKey) || !validateUUID(command.ConfirmationID) || !validateUUID(command.TaskID) || command.Attempt == 0 || command.LeaseEpoch == 0 || command.ExpectedRevision < 1 || command.ExpectedTaskRevision < 1 {
		return Confirmation{}, ErrInvalid
	}
	command.RequestDigest = consumeDigest(command)
	now := r.at(command.At)
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.items[command.ConfirmationID]
	if !ok {
		return Confirmation{}, ErrNotFound
	}
	if value.OwnerID != command.OwnerID {
		return Confirmation{}, ErrConflict
	}
	if isExecutionV2Domain(value.Binding.OperationDomain) {
		return Confirmation{}, ErrConflict
	}
	key := "consume:" + command.OwnerID + ":" + command.IdempotencyKey
	if replay, ok, conflict := r.replayLocked(key, command.RequestDigest); ok || conflict != nil {
		return replay, conflict
	}
	if value.Revision != command.ExpectedRevision || value.TaskID != command.TaskID || value.State != StateConfirmed {
		return Confirmation{}, ErrTaskFenceConflict
	}
	fence, exists := r.taskFences[command.TaskID]
	var err error
	if command.ResolveTaskFence != nil {
		fence, err = command.ResolveTaskFence(ctx, command.TaskID)
		exists = err == nil
	}
	if !exists || fence.State != "running" || fence.Attempt != command.Attempt || fence.LeaseEpoch != command.LeaseEpoch || fence.Revision != command.ExpectedTaskRevision {
		return Confirmation{}, ErrTaskFenceConflict
	}
	var binding Binding
	if command.ResolveBinding != nil {
		binding, err = command.ResolveBinding(ctx)
	} else {
		binding, err = command.Binding.normalized()
	}
	authoritative := value.Binding
	if current, exists := r.targetBindings[command.ConfirmationID]; exists {
		authoritative = current
	}
	if err != nil || !value.Binding.Equal(authoritative) || !value.Binding.Equal(binding) {
		value.State = StateExpired
		value.Revision++
		value.UpdatedAt = now
		value.TerminalCode, value.TerminalReason = ReasonStale, ReasonStale
		r.items[value.ConfirmationID] = value
		return Confirmation{}, ErrStale
	}
	if !value.ExpiresAt.After(now) {
		value.State = StateExpired
		value.Revision++
		value.UpdatedAt = now
		value.TerminalCode = ReasonExpired
		value.TerminalReason = ReasonExpired
		r.items[value.ConfirmationID] = value
		return Confirmation{}, ErrExpired
	}
	value.State = StateConsumed
	value.Revision++
	value.UpdatedAt = now
	r.items[value.ConfirmationID] = value
	r.reservations[value.ConfirmationID] = Reservation{ConfirmationID: value.ConfirmationID, TaskID: command.TaskID, AcquiredAttempt: command.Attempt, AcquiredLeaseEpoch: command.LeaseEpoch, TaskRevision: command.ExpectedTaskRevision, Active: true}
	r.replays[key] = replay{digest: command.RequestDigest, value: cloneConfirmation(value)}
	return cloneConfirmation(value), nil
}

func (r *MemoryRepository) ReleaseReservation(ctx context.Context, command ReleaseReservationCommand) (Confirmation, error) {
	if !validateUUID(command.IdempotencyKey) || !validateUUID(command.ConfirmationID) || !validateUUID(command.TaskID) || command.AcquiredAttempt == 0 || command.AcquiredLeaseEpoch == 0 || command.TerminalAttempt == 0 || command.TerminalLeaseEpoch == 0 || command.ExpectedTaskRevision < 1 {
		return Confirmation{}, ErrInvalid
	}
	command.RequestDigest = releaseDigest(command)
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.items[command.ConfirmationID]
	if !ok {
		return Confirmation{}, ErrNotFound
	}
	if isExecutionV2Domain(value.Binding.OperationDomain) {
		return Confirmation{}, ErrConflict
	}
	key := "release:" + command.IdempotencyKey
	if replay, ok, conflict := r.replayLocked(key, command.RequestDigest); ok || conflict != nil {
		return replay, conflict
	}
	reservation, reserved := r.reservations[command.ConfirmationID]
	if value.State != StateConsumed || !reserved || !reservation.Active || value.TaskID != command.TaskID || reservation.AcquiredAttempt != command.AcquiredAttempt || reservation.AcquiredLeaseEpoch != command.AcquiredLeaseEpoch {
		return Confirmation{}, ErrConflict
	}
	fence := TaskFence{}
	var err error
	if command.ResolveTaskFence != nil {
		fence, err = command.ResolveTaskFence(ctx, command.TaskID)
	} else {
		fence, err = r.taskFences[command.TaskID], nil
	}
	if err != nil || fence.State != "succeeded" && fence.State != "failed" && fence.State != "canceled" || fence.Attempt != command.TerminalAttempt || fence.LeaseEpoch != command.TerminalLeaseEpoch || fence.Revision != command.ExpectedTaskRevision {
		return Confirmation{}, ErrTaskFenceConflict
	}
	reservation.Active = false
	r.reservations[value.ConfirmationID] = reservation
	r.replays[key] = replay{digest: command.RequestDigest, value: cloneConfirmation(value)}
	return cloneConfirmation(value), nil
}

func (r *MemoryRepository) Expire(_ context.Context, command ExpireCommand) (Confirmation, error) {
	if !validateUUID(command.IdempotencyKey) || !validateUUID(command.ConfirmationID) || command.ExpectedRevision < 1 || (command.Reason != ReasonExpired && command.Reason != ReasonStale) {
		return Confirmation{}, ErrInvalid
	}
	now := r.at(command.At)
	command.RequestDigest = expireDigest(command)
	r.mu.Lock()
	defer r.mu.Unlock()
	value, ok := r.items[command.ConfirmationID]
	if !ok {
		return Confirmation{}, ErrNotFound
	}
	if isExecutionV2Domain(value.Binding.OperationDomain) {
		return Confirmation{}, ErrConflict
	}
	key := "expire:" + command.IdempotencyKey
	if replay, ok, conflict := r.replayLocked(key, command.RequestDigest); ok || conflict != nil {
		return replay, conflict
	}
	if value.Revision != command.ExpectedRevision {
		return Confirmation{}, ErrRevisionConflict
	}
	if value.State != StatePending && value.State != StateConfirmed {
		return Confirmation{}, ErrConflict
	}
	value.State = StateExpired
	value.Revision++
	value.UpdatedAt = now
	value.TerminalCode = strings.TrimSpace(command.Reason)
	value.TerminalReason = value.TerminalCode
	r.items[value.ConfirmationID] = value
	r.replays[key] = replay{digest: command.RequestDigest, value: cloneConfirmation(value)}
	return cloneConfirmation(value), nil
}

func (r *MemoryRepository) at(value time.Time) time.Time {
	if value.IsZero() {
		value = r.now().UTC()
	}
	return value.UTC()
}

func (r *MemoryRepository) replayLocked(key string, digest Digest) (Confirmation, bool, error) {
	previous, ok := r.replays[key]
	if !ok {
		return Confirmation{}, false, nil
	}
	if previous.digest != digest {
		return Confirmation{}, false, ErrIdempotencyConflict
	}
	return cloneConfirmation(previous.value), true, nil
}

func isLiveConfirmation(value Confirmation) bool {
	return value.State == StatePending || value.State == StateConfirmed || value.State == StateConsumed
}

func validState(state State) bool {
	return state == StatePending || state == StateConfirmed || state == StateConsumed || state == StateRejected || state == StateExpired
}

// StableOrder returns IDs in creation order, useful to persistence adapters.
func (r *MemoryRepository) StableOrder() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := append([]string(nil), r.order...)
	sort.Strings(result)
	return result
}
