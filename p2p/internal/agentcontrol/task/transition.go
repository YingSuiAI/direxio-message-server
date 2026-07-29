package task

import (
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

func (t Task) CanTransition(to Status) bool {
	if !validStatus(to) || t.Status == StatusSucceeded || t.Status == StatusFailed || t.Status == StatusCanceled {
		return false
	}
	switch t.Status {
	case StatusQueued:
		return to == StatusRunning || to == StatusCanceled
	case StatusRunning:
		return to == StatusSucceeded || to == StatusFailed || to == StatusWaitingUser || to == StatusCanceled
	case StatusWaitingUser:
		return to == StatusQueued || to == StatusCanceled
	}
	return false
}

func (t *Task) Transition(to Status) error {
	if to == StatusSucceeded || to == StatusFailed || to == StatusCanceled {
		if t != nil && (t.Status == StatusSucceeded || t.Status == StatusFailed || t.Status == StatusCanceled) {
			return ErrTerminal
		}
		return ErrConflict
	}
	if to == StatusRunning {
		return ErrConflict
	}
	if t == nil || t.Validate() != nil {
		return ErrInvalid
	}
	next := *t
	if err := next.transition(to); err != nil {
		return err
	}
	if err := next.Validate(); err != nil {
		return err
	}
	*t = next
	return nil
}

func (t *Task) transition(to Status) error {
	if t == nil || !t.CanTransition(to) {
		if t != nil && (t.Status == StatusSucceeded || t.Status == StatusFailed || t.Status == StatusCanceled) {
			return ErrTerminal
		}
		return ErrConflict
	}
	t.Status = to
	t.Revision++
	if to != StatusRunning {
		t.Lease = nil
	}
	return nil
}

type Fence struct {
	TaskID                       string
	Attempt                      uint32
	LeaseEpoch, ExpectedRevision uint64
}

func (f Fence) validate(task Task, requireLease bool, at time.Time) error {
	if task.ID != f.TaskID || f.ExpectedRevision == 0 || task.Revision != f.ExpectedRevision {
		return ErrRevisionConflict
	}
	if requireLease {
		if task.Status != StatusRunning || f.Attempt == 0 || task.Attempt != f.Attempt || task.LeaseEpoch != f.LeaseEpoch || task.Lease == nil || task.Lease.Epoch != f.LeaseEpoch || at.IsZero() || at.Location() != time.UTC || !at.Before(task.Lease.ExpiresAt) {
			return ErrLeaseConflict
		}
	}
	return nil
}

type ClaimCommand struct {
	OwnerID          string
	TaskID, Holder   string
	ExpectedRevision uint64
	LeaseEpoch       uint64
	LeaseTTL         time.Duration
	At               time.Time
}
type RenewLeaseCommand struct {
	Fence
	Holder   string
	LeaseTTL time.Duration
	At       time.Time
}
type ProgressCommand struct {
	Fence
	Progress         Progress
	ExpectedSequence uint64
}
type WaitUserCommand struct {
	Fence
	Reason string
	At     time.Time
}
type ResumeCommand struct {
	OwnerID          string
	TaskID           string
	ExpectedRevision uint64
}
type CompleteCommand struct {
	Fence
	Result Result
	At     time.Time
}
type CancelCommand struct {
	OwnerID          string
	TaskID           string
	ExpectedRevision uint64
	Mutation         MutationCommand
	Reason           string
	At               time.Time
}
type TimeoutCommand struct {
	Fence
	At time.Time
}
type FailCommand struct {
	Fence
	ErrorCode    string
	ErrorSummary string
	At           time.Time
}
type ReclaimCommand struct {
	OwnerID          string
	TaskID           string
	Holder           string
	ExpectedRevision uint64
	LeaseEpoch       uint64
	LeaseTTL         time.Duration
	At               time.Time
}

func ValidateClaim(task Task, c ClaimCommand) error {
	if task.ID != c.TaskID || task.Status != StatusQueued || c.ExpectedRevision == 0 || task.Revision != c.ExpectedRevision || !validHolder(c.Holder) || c.LeaseTTL <= 0 || c.At.IsZero() || c.At.Location() != time.UTC {
		return ErrConflict
	}
	if !c.LeaseEpochKnown(task, c.LeaseEpoch) {
		return ErrLeaseConflict
	}
	return nil
}

func (c ClaimCommand) LeaseEpochKnown(task Task, epoch uint64) bool {
	return epoch == task.LeaseEpoch+1
}

func ValidateRenewLease(task Task, c RenewLeaseCommand) error {
	if c.LeaseTTL <= 0 || !validHolder(c.Holder) || c.At.IsZero() || c.At.Location() != time.UTC {
		return ErrInvalid
	}
	if err := c.Fence.validate(task, true, c.At); err != nil {
		return err
	}
	if task.Lease.Holder != c.Holder {
		return ErrLeaseConflict
	}
	return nil
}

func ValidateProgress(task Task, c ProgressCommand) error {
	if err := c.Fence.validate(task, true, c.Progress.At); err != nil {
		return err
	}
	p := c.Progress
	if p.TaskID != task.ID || p.Attempt != task.Attempt || p.Sequence == 0 || p.At.IsZero() || p.At.Location() != time.UTC || !validStatus(p.Status) || (p.Status != task.Status && !(task.Status == StatusRunning && p.Status == StatusWaitingUser)) || len([]byte(p.Message)) > MaxProgressMessage || len([]byte(p.Phase)) > 512 || len([]byte(p.ResultSummary)) > MaxSummaryBytes || len([]byte(p.ErrorCode)) > 128 || len([]byte(p.ErrorSummary)) > MaxSummaryBytes || !utf8.ValidString(p.Message) || !utf8.ValidString(p.Phase) || !utf8.ValidString(p.ResultSummary) || !utf8.ValidString(p.ErrorCode) || !utf8.ValidString(p.ErrorSummary) {
		return ErrInvalid
	}
	if p.Percent != nil && (math.IsNaN(*p.Percent) || math.IsInf(*p.Percent, 0) || *p.Percent < 0 || *p.Percent > 100) {
		return ErrInvalid
	}
	return nil
}

// AllocateProgress assigns the next durable sequence. Callers must provide
// the previously committed cursor; the worker cannot choose a persistent seq.
func AllocateProgress(task Task, c ProgressCommand) (Progress, error) {
	if c.ExpectedSequence != task.ProgressSequence || c.Progress.Sequence != 0 {
		return Progress{}, ErrConflict
	}
	p := c.Progress
	p.Sequence = task.ProgressSequence + 1
	check := c
	check.Progress = p
	if err := ValidateProgress(task, check); err != nil {
		return Progress{}, err
	}
	return p, nil
}

func ValidateWaitUser(task Task, c WaitUserCommand) error {
	if err := c.Fence.validate(task, true, c.At); err != nil {
		return err
	}
	if strings.TrimSpace(c.Reason) == "" {
		return ErrInvalid
	}
	return nil
}

func ValidateResume(task Task, c ResumeCommand) error {
	if task.ID != c.TaskID || task.Status != StatusWaitingUser || c.ExpectedRevision == 0 || task.Revision != c.ExpectedRevision {
		return ErrConflict
	}
	return nil
}

func ValidateComplete(task Task, c CompleteCommand) error {
	if err := c.Fence.validate(task, true, c.At); err != nil {
		return err
	}
	return c.Result.Validate()
}

func ValidateCancel(task Task, c CancelCommand) error {
	if err := c.Mutation.Validate(); err != nil {
		return err
	}
	if err := c.Mutation.ValidateExpectedRevision(); err != nil {
		return err
	}
	if task.ID != c.TaskID || task.Revision != c.Mutation.ExpectedRevision || !task.CanTransition(StatusCanceled) {
		return ErrConflict
	}
	if c.At.IsZero() || c.At.Location() != time.UTC {
		return ErrInvalid
	}
	if task.Status == StatusRunning && (task.Lease == nil || task.Lease.Epoch != task.LeaseEpoch) {
		return ErrLeaseConflict
	}
	return nil
}

func ValidateDeleteCommand(task Task, c DeleteTaskCommand) error {
	if !ValidUUID(c.TaskID) || c.TaskID != task.ID || c.At.IsZero() || c.At.Location() != time.UTC {
		return ErrInvalid
	}
	if err := c.Mutation.Validate(); err != nil {
		return err
	}
	if c.Mutation.ExpectedRevision != task.Revision {
		return ErrRevisionConflict
	}
	if task.Status == StatusRunning {
		return ErrConflict
	}
	return nil
}

func ValidateTimeout(task Task, c TimeoutCommand) error {
	if c.At.IsZero() || c.At.Location() != time.UTC {
		return ErrInvalid
	}
	return c.Fence.validate(task, true, c.At)
}
func ValidateFail(task Task, c FailCommand) error {
	if c.At.IsZero() || c.At.Location() != time.UTC || strings.TrimSpace(c.ErrorCode) == "" || len([]byte(c.ErrorCode)) > 128 || len([]byte(c.ErrorSummary)) > MaxSummaryBytes || !utf8.ValidString(c.ErrorCode) || !utf8.ValidString(c.ErrorSummary) {
		return ErrInvalid
	}
	return c.Fence.validate(task, true, c.At)
}

func ValidateReclaim(task Task, c ReclaimCommand) error {
	if task.ID != c.TaskID || task.Status != StatusRunning || task.Attempt != 1 || task.Lease == nil || task.Lease.Epoch != task.LeaseEpoch || c.ExpectedRevision == 0 || task.Revision != c.ExpectedRevision || c.LeaseEpoch != task.LeaseEpoch+1 || !validHolder(c.Holder) || c.LeaseTTL <= 0 || c.At.IsZero() || c.At.Location() != time.UTC || c.At.Before(task.Lease.ExpiresAt) {
		return ErrConflict
	}
	return nil
}

func validHolder(holder string) bool {
	holder = strings.TrimSpace(holder)
	return holder != "" && utf8.ValidString(holder) && len([]byte(holder)) <= MaxLeaseHolderBytes
}

func Claim(task *Task, c ClaimCommand) (Lease, error) {
	if task == nil {
		return Lease{}, ErrInvalid
	}
	if err := ValidateClaim(*task, c); err != nil {
		return Lease{}, err
	}
	task.Status = StatusRunning
	task.Attempt = 1
	task.LeaseEpoch = c.LeaseEpoch
	task.Revision++
	lease := Lease{TaskID: task.ID, Attempt: task.Attempt, Epoch: c.LeaseEpoch, Holder: strings.TrimSpace(c.Holder), ExpiresAt: c.At.UTC().Add(c.LeaseTTL)}
	task.Lease = &lease
	return lease, nil
}

func ApplyProgress(task *Task, c ProgressCommand) error {
	if task == nil {
		return ErrInvalid
	}
	if _, err := AllocateProgress(*task, c); err != nil {
		return err
	}
	task.ProgressSequence++
	task.Revision++
	return nil
}

func WaitForUser(task *Task, c WaitUserCommand) error {
	if task == nil {
		return ErrInvalid
	}
	if err := ValidateWaitUser(*task, c); err != nil {
		return err
	}
	return task.transition(StatusWaitingUser)
}
func Resume(task *Task, c ResumeCommand) error {
	if task == nil {
		return ErrInvalid
	}
	if err := ValidateResume(*task, c); err != nil {
		return err
	}
	return task.transition(StatusQueued)
}
func Complete(task *Task, c CompleteCommand) error {
	if task == nil {
		return ErrInvalid
	}
	if err := ValidateComplete(*task, c); err != nil {
		return err
	}
	next := *task
	next.Result = &c.Result
	if err := next.transition(StatusSucceeded); err != nil {
		return err
	}
	*task = next
	return nil
}
func Cancel(task *Task, c CancelCommand) error {
	if task == nil {
		return ErrInvalid
	}
	if err := ValidateCancel(*task, c); err != nil {
		return err
	}
	next := *task
	if next.Status == StatusRunning {
		next.LeaseEpoch++ // fence the executor before committing the terminal state
	}
	if err := next.transition(StatusCanceled); err != nil {
		return err
	}
	*task = next
	return nil
}
func Timeout(task *Task, c TimeoutCommand) error {
	if task == nil {
		return ErrInvalid
	}
	if err := ValidateTimeout(*task, c); err != nil {
		return err
	}
	next := *task
	next.FailureCode = ErrTimedOut.Error()
	next.FailureSummary = "task timed out"
	if err := next.transition(StatusFailed); err != nil {
		return err
	}
	*task = next
	return nil
}

func Fail(task *Task, c FailCommand) error {
	if task == nil {
		return ErrInvalid
	}
	if err := ValidateFail(*task, c); err != nil {
		return err
	}
	next := *task
	next.FailureCode = c.ErrorCode
	next.FailureSummary = c.ErrorSummary
	if err := next.transition(StatusFailed); err != nil {
		return err
	}
	*task = next
	return nil
}

func Reclaim(task *Task, c ReclaimCommand) (Lease, error) {
	if task == nil {
		return Lease{}, ErrInvalid
	}
	if err := ValidateReclaim(*task, c); err != nil {
		return Lease{}, err
	}
	task.LeaseEpoch = c.LeaseEpoch
	task.Revision++
	lease := Lease{TaskID: task.ID, Attempt: task.Attempt, Epoch: c.LeaseEpoch, Holder: strings.TrimSpace(c.Holder), ExpiresAt: c.At.UTC().Add(c.LeaseTTL)}
	task.Lease = &lease
	return lease, nil
}

func ValidateDelete(task Task, expectedRevision uint64) error {
	if expectedRevision == 0 || task.Revision != expectedRevision {
		return ErrRevisionConflict
	}
	if task.Status == StatusRunning {
		return ErrConflict
	}
	return nil
}

func SoftDelete(task *Task, expectedRevision uint64, at time.Time) error {
	if task == nil || at.IsZero() || at.Location() != time.UTC {
		return ErrInvalid
	}
	if err := ValidateDelete(*task, expectedRevision); err != nil {
		return err
	}
	if task.DeletedAt != nil {
		return nil
	}
	t := at.UTC()
	task.DeletedAt = &t
	task.Revision++
	task.UpdatedAt = t
	return nil
}
