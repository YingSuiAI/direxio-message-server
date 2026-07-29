package task

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

type OccurrenceStore interface {
	FindOccurrence(context.Context, string, string) (Occurrence, error)
	CreateOccurrence(context.Context, Schedule, TriggerNowCommand, Occurrence) (Occurrence, error)
}

type MutationRecord struct {
	Operation      string
	IdempotencyKey string
	Digest         string
	Response       json.RawMessage
	CreatedAt      time.Time
}

type MutationCommand struct {
	IdempotencyKey   string
	RequestDigest    string
	ExpectedRevision uint64
}

func (m MutationCommand) Validate() error {
	if !ValidUUID(m.IdempotencyKey) || len(m.RequestDigest) != 64 || strings.ToLower(m.RequestDigest) != m.RequestDigest {
		return ErrInvalid
	}
	if _, err := hex.DecodeString(m.RequestDigest); err != nil {
		return ErrInvalid
	}
	return nil
}

func (m MutationCommand) ValidateExpectedRevision() error {
	if m.ExpectedRevision == 0 {
		return ErrInvalid
	}
	return nil
}

func (c CreateTaskCommand) Validate() error {
	if strings.TrimSpace(c.OwnerID) == "" {
		return ErrInvalid
	}
	if err := c.Mutation.Validate(); err != nil {
		return err
	}
	if c.Mutation.ExpectedRevision != 0 {
		return ErrInvalid
	}
	if c.Spec.IdempotencyKey != c.Mutation.IdempotencyKey {
		return ErrConflict
	}
	return c.Spec.Validate()
}

type CreateTaskCommand struct {
	OwnerID  string
	Spec     TaskSpec
	Mutation MutationCommand
}
type DeleteTaskCommand struct {
	TaskID   string
	Mutation MutationCommand
	At       time.Time
}
type DeletedTaskResponse struct {
	TaskID    string    `json:"task_id"`
	DeletedAt time.Time `json:"deleted_at"`
	Revision  uint64    `json:"revision"`
	Tombstone bool      `json:"tombstone"`
}

func (r DeletedTaskResponse) Validate() error {
	if !ValidUUID(r.TaskID) || r.DeletedAt.IsZero() || r.DeletedAt.Location() != time.UTC || r.Revision == 0 || !r.Tombstone {
		return ErrInvalid
	}
	return nil
}

// ReplayMutation must run before mutable revision, terminal, or deleted checks.
func ReplayMutation(record MutationRecord, operation string, command MutationCommand) ([]byte, bool, error) {
	if err := command.Validate(); err != nil {
		return nil, false, err
	}
	if record.Operation != operation || record.IdempotencyKey != command.IdempotencyKey {
		return nil, false, ErrNotFound
	}
	if record.Digest != command.RequestDigest {
		return nil, true, ErrConflict
	}
	return append([]byte(nil), record.Response...), true, nil
}

func (m MutationRecord) Validate() error {
	if m.Operation == "" || !ValidUUID(m.IdempotencyKey) || len(m.Digest) != 64 || func() bool { _, err := hex.DecodeString(m.Digest); return err != nil }() || len(m.Response) > MaxResultBytes || !json.Valid(m.Response) || m.CreatedAt.IsZero() || m.CreatedAt.Location() != time.UTC {
		return ErrInvalid
	}
	return nil
}

type MutationRepository interface {
	LookupMutation(context.Context, string, string) (MutationRecord, error)
	// CommitMutation atomically stores the response snapshot with the mutation;
	// an existing same-key record is replayed verbatim, while a digest mismatch conflicts.
	CommitMutation(context.Context, MutationRecord) (MutationRecord, error)
}

// TriggerNowWithStore performs the durable lookup-before-create idempotency
// protocol. A found occurrence is returned verbatim, regardless of new input.
func TriggerNowWithStore(ctx context.Context, store OccurrenceStore, schedule Schedule, command TriggerNowCommand) (Occurrence, error) {
	if store == nil {
		return Occurrence{}, ErrInvalid
	}
	if !ValidUUID(command.ScheduleID) || !ValidUUID(command.IdempotencyKey) {
		return Occurrence{}, ErrInvalid
	}
	if existing, err := store.FindOccurrence(ctx, command.ScheduleID, command.IdempotencyKey); err == nil {
		return existing, nil
	} else if !errors.Is(err, ErrNotFound) {
		return Occurrence{}, err
	}
	if schedule.Deleted || schedule.Validate() != nil {
		return Occurrence{}, ErrInvalid
	}
	candidate, err := TriggerNow(schedule, command)
	if err != nil {
		return Occurrence{}, err
	}
	return store.CreateOccurrence(ctx, schedule, command, candidate)
}

func FIFO(tasks []Task) []Task {
	out := append([]Task(nil), tasks...)
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].AvailableAt.Equal(out[j].AvailableAt) {
			return out[i].AvailableAt.Before(out[j].AvailableAt)
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

type ClaimPolicy struct{ MaxConcurrent int }

func (p ClaimPolicy) CanClaim(running int) bool {
	return p.MaxConcurrent > 0 && running >= 0 && running < p.MaxConcurrent
}
func ClaimNext(tasks []Task, running int, policy ClaimPolicy, now time.Time) (Task, bool) {
	if !policy.CanClaim(running) {
		return Task{}, false
	}
	if now.IsZero() || now.Location() != time.UTC {
		return Task{}, false
	}
	for _, task := range FIFO(tasks) {
		if task.Status == StatusQueued && !task.AvailableAt.After(now) {
			return task, true
		}
	}
	return Task{}, false
}

type RetryRequest struct {
	TaskID, IdempotencyKey string
	RequestDigest          string
	ExpectedRevision       uint64
	At                     time.Time
}
type RetryCommand struct {
	TaskID   string
	Mutation MutationCommand
	At       time.Time
}

func (c RetryCommand) Validate() error {
	if !ValidUUID(c.TaskID) || c.At.IsZero() || c.At.Location() != time.UTC {
		return ErrInvalid
	}
	if err := c.Mutation.Validate(); err != nil {
		return err
	}
	return c.Mutation.ValidateExpectedRevision()
}

func (r RetryRequest) Mutation() MutationCommand {
	return MutationCommand{IdempotencyKey: r.IdempotencyKey, RequestDigest: r.RequestDigest, ExpectedRevision: r.ExpectedRevision}
}

func RetryTask(original Task, request RetryRequest) (Task, error) {
	if !ValidUUID(original.ID) || !ValidUUID(request.TaskID) || request.TaskID != original.ID || !ValidUUID(request.IdempotencyKey) || request.At.IsZero() {
		return Task{}, ErrInvalid
	}
	if original.Status != StatusSucceeded && original.Status != StatusFailed && original.Status != StatusCanceled {
		return Task{}, ErrConflict
	}
	if original.Spec.Kind != "" && original.Spec.Kind != TaskKindAgent {
		return Task{}, ErrConflict
	}
	if original.FailureCode == "tool_uncertain" || original.FailureCode == "model_uncertain" {
		return Task{}, ErrConflict
	}
	if err := request.Mutation().Validate(); err != nil {
		return Task{}, err
	}
	if err := request.Mutation().ValidateExpectedRevision(); err != nil {
		return Task{}, err
	}
	if request.ExpectedRevision != original.Revision {
		return Task{}, ErrRevisionConflict
	}
	copySpec := original.Spec
	copySpec.IdempotencyKey = request.IdempotencyKey
	copySpec.AvailableAt = request.At.UTC()
	copySpec, err := copySpec.Normalize()
	if err != nil {
		return Task{}, err
	}
	id := deterministicUUID(original.OwnerID + ":task:" + original.ID + ":retry:" + request.IdempotencyKey)
	availableAt := copySpec.AvailableAt
	return Task{ID: id, Spec: copySpec, Status: StatusQueued, Revision: 1, CreatedAt: request.At.UTC(), UpdatedAt: request.At.UTC(), AvailableAt: availableAt, RetryOfTaskID: original.ID}, nil
}

// Store is the persistence boundary used by the future PostgreSQL adapter.
// Implementations must perform fencing and mutation plus event append atomically.
type Store interface {
	MutationRepository
	CreateTask(context.Context, CreateTaskCommand) (Task, error)
	GetTask(context.Context, string) (Task, error)
	ListTasks(context.Context, TaskListQuery) ([]Task, string, error)
	DeleteTask(context.Context, DeleteTaskCommand) (DeletedTaskResponse, error)
	ClaimTask(context.Context, ClaimCommand) (Task, Lease, error)
	ReclaimTask(context.Context, ReclaimCommand) (Task, Lease, error)
	RenewLease(context.Context, RenewLeaseCommand) (Lease, error)
	// AppendProgress must allocate previous_cursor+1 and atomically CAS the task
	// revision, advance its progress cursor, and insert the progress event.
	AppendProgress(context.Context, ProgressCommand) (Task, Progress, error)
	ListProgress(context.Context, string, uint64, int) ([]Progress, string, error)
	WaitTask(context.Context, WaitUserCommand) error
	ResumeTask(context.Context, ResumeCommand) error
	CompleteTask(context.Context, CompleteCommand) (Task, error)
	CancelTask(context.Context, CancelCommand) (Task, error)
	TimeoutTask(context.Context, TimeoutCommand) error
	FailTask(context.Context, FailCommand) error
	RetryTask(context.Context, RetryCommand) (Task, error)
	WatchProgress(context.Context, string, uint64) (<-chan Progress, error)
}

type TaskQueueRepository interface {
	ClaimNextDue(context.Context, string, time.Time, time.Duration, int) (Task, Lease, error)
}

type TaskListQuery struct {
	OwnerID        string
	Cursor         string
	Limit          int
	Status         *Status
	IncludeDeleted bool
}

func FilterVisibleTasks(tasks []Task, includeDeleted bool) []Task {
	if includeDeleted {
		return append([]Task(nil), tasks...)
	}
	visible := make([]Task, 0, len(tasks))
	for _, task := range tasks {
		if task.DeletedAt == nil {
			visible = append(visible, task)
		}
	}
	return visible
}

type ScheduleStore interface {
	OccurrenceStore
	CreateSchedule(context.Context, CreateScheduleCommand) (Schedule, error)
	GetSchedule(context.Context, string) (Schedule, error)
	ListSchedules(context.Context, string, int) ([]Schedule, string, error)
	UpdateSchedule(context.Context, UpdateScheduleCommand) (Schedule, error)
	PauseSchedule(context.Context, ScheduleMutationCommand) (Schedule, error)
	ResumeSchedule(context.Context, ScheduleMutationCommand) (Schedule, error)
	TriggerNow(context.Context, TriggerScheduleCommand) (Schedule, Occurrence, Task, error)
	DeleteSchedule(context.Context, ScheduleMutationCommand) (Schedule, error)
}

type ProgressWatcher interface {
	ListProgress(context.Context, string, uint64, int) ([]Progress, string, error)
	WatchProgress(context.Context, string, uint64) (<-chan Progress, error)
}

// ProgressStreamEvent preserves a database/watch failure instead of encoding
// it as a clean channel close. Callers can reconnect from the last delivered
// sequence without losing the causal error.
type ProgressStreamEvent struct {
	Progress *Progress
	Err      error
}

type ProgressErrorWatcher interface {
	WatchProgressWithErrors(context.Context, string, uint64) (<-chan ProgressStreamEvent, error)
}

// Service is the application boundary over Store; implementations enforce
// request normalization and delegate atomic state changes to the repository.
type Service interface {
	CreateTask(context.Context, CreateTaskCommand) (Task, error)
	GetTask(context.Context, string) (Task, error)
	ListTasks(context.Context, TaskListQuery) ([]Task, string, error)
	CancelTask(context.Context, CancelCommand) (Task, error)
	RetryTask(context.Context, RetryCommand) (Task, error)
	DeleteTask(context.Context, DeleteTaskCommand) (DeletedTaskResponse, error)
	ListProgress(context.Context, string, uint64, int) ([]Progress, string, error)
	WatchProgress(context.Context, string, uint64) (<-chan Progress, error)
}
