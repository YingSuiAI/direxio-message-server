package confirmation

import (
	"context"
	"strings"
	"time"
)

// Service validates the shared domain contract. Request/Consume/Expire are
// intentionally domain APIs; the public gRPC surface exposes only Get/List/
// Confirm/Reject.
type Service struct {
	repository    Repository
	now           func() time.Time
	bindingReader TargetBindingReader
	currentReader CurrentTargetBindingReader
}

type TargetBindingReader interface {
	ReadTargetBinding(context.Context, string) (Binding, error)
}
type CurrentTargetBindingReader interface {
	ReadCurrentTargetBinding(context.Context, string, string) (Binding, error)
}

func NewService(repository Repository, now ...func() time.Time) (*Service, error) {
	if repository == nil {
		return nil, ErrInvalid
	}
	clock := time.Now
	if len(now) > 0 && now[0] != nil {
		clock = now[0]
	}
	service := &Service{repository: repository, now: clock}
	if reader, ok := repository.(TargetBindingReader); ok {
		service.bindingReader = reader
	}
	if reader, ok := repository.(CurrentTargetBindingReader); ok {
		service.currentReader = reader
	}
	return service, nil
}

func (s *Service) Request(ctx context.Context, command RequestCommand) (Confirmation, error) {
	if s == nil || !validateUUID(command.IdempotencyKey) {
		return Confirmation{}, ErrInvalid
	}
	binding, err := command.Binding.normalized()
	if err != nil || !validateUUID(command.TaskID) || command.ExpiresAt.IsZero() || command.ExpiresAt.Location() != time.UTC {
		return Confirmation{}, ErrInvalid
	}
	now := command.At
	if now.IsZero() {
		now = s.now().UTC()
	}
	if now.Location() != time.UTC || !command.ExpiresAt.After(now) {
		return Confirmation{}, ErrInvalid
	}
	command.Binding = binding
	command.TaskID = strings.TrimSpace(command.TaskID)
	command.At = now
	command.RequestDigest = requestDigest(command)
	return s.repository.Request(ctx, command)
}

func (s *Service) Get(ctx context.Context, id string) (Confirmation, error) {
	if s == nil || !validateUUID(id) {
		return Confirmation{}, ErrInvalid
	}
	return s.repository.Get(ctx, strings.TrimSpace(id))
}

func (s *Service) List(ctx context.Context, query ListQuery) (Page, error) {
	if s == nil || query.PageSize < 0 || query.PageSize > 100 {
		return Page{}, ErrInvalid
	}
	return s.repository.List(ctx, query)
}

func (s *Service) Confirm(ctx context.Context, command ConfirmCommand) (Confirmation, error) {
	if s == nil || !validateUUID(command.IdempotencyKey) || !validateUUID(command.ConfirmationID) || command.ExpectedRevision < 1 {
		return Confirmation{}, ErrInvalid
	}
	if s.bindingReader == nil && s.currentReader == nil {
		return Confirmation{}, ErrBindingUnavailable
	}
	if _, sameMemory := s.repository.(*MemoryRepository); !sameMemory && s.currentReader == nil {
		command.ResolveBinding = func(_ context.Context) (Binding, error) {
			return s.bindingReader.ReadTargetBinding(ctx, command.ConfirmationID)
		}
	}
	if command.At.IsZero() {
		command.At = s.now().UTC()
	}
	command.RequestDigest = confirmDigest(command)
	return s.repository.Confirm(ctx, command)
}

func (s *Service) Reject(ctx context.Context, command RejectCommand) (Confirmation, error) {
	if s == nil || !validateUUID(command.IdempotencyKey) || !validateUUID(command.ConfirmationID) || command.ExpectedRevision < 1 {
		return Confirmation{}, ErrInvalid
	}
	command.Reason = strings.TrimSpace(command.Reason)
	command.Note = strings.TrimSpace(command.Note)
	if len(command.Reason) > 256 || len(command.Note) > 256 || strings.ContainsAny(command.Reason+command.Note, "\r\n") {
		return Confirmation{}, ErrInvalid
	}
	if command.At.IsZero() {
		command.At = s.now().UTC()
	}
	command.RequestDigest = rejectDigest(command)
	return s.repository.Reject(ctx, command)
}

func (s *Service) Consume(ctx context.Context, command ConsumeCommand) (Confirmation, error) {
	if s == nil || !validateUUID(command.IdempotencyKey) || !validateUUID(command.ConfirmationID) || !validateUUID(command.TaskID) || command.Attempt == 0 || command.LeaseEpoch == 0 || command.ExpectedRevision < 1 || command.ExpectedTaskRevision < 1 {
		return Confirmation{}, ErrInvalid
	}
	if s.bindingReader == nil && s.currentReader == nil {
		return Confirmation{}, ErrBindingUnavailable
	}
	if _, sameMemory := s.repository.(*MemoryRepository); !sameMemory && s.currentReader == nil {
		command.ResolveBinding = func(_ context.Context) (Binding, error) {
			return s.bindingReader.ReadTargetBinding(ctx, command.ConfirmationID)
		}
	}
	if command.At.IsZero() {
		command.At = s.now().UTC()
	}
	command.RequestDigest = consumeDigest(command)
	return s.repository.Consume(ctx, command)
}

func (s *Service) Expire(ctx context.Context, command ExpireCommand) (Confirmation, error) {
	if s == nil || !validateUUID(command.IdempotencyKey) || !validateUUID(command.ConfirmationID) || command.ExpectedRevision < 1 {
		return Confirmation{}, ErrInvalid
	}
	command.Reason = strings.TrimSpace(command.Reason)
	if command.Reason != ReasonExpired && command.Reason != ReasonStale {
		return Confirmation{}, ErrInvalid
	}
	if command.At.IsZero() {
		command.At = s.now().UTC()
	}
	command.RequestDigest = expireDigest(command)
	return s.repository.Expire(ctx, command)
}

func (s *Service) ReleaseReservation(ctx context.Context, command ReleaseReservationCommand) (Confirmation, error) {
	if s == nil || !validateUUID(command.IdempotencyKey) || !validateUUID(command.ConfirmationID) || !validateUUID(command.TaskID) || command.AcquiredAttempt == 0 || command.AcquiredLeaseEpoch == 0 || command.TerminalAttempt == 0 || command.TerminalLeaseEpoch == 0 || command.ExpectedTaskRevision < 1 {
		return Confirmation{}, ErrInvalid
	}
	command.RequestDigest = releaseDigest(command)
	return s.repository.ReleaseReservation(ctx, command)
}

// TerminalizeTask is a preparation seam for the generic task terminal writer.
// Wiring belongs to the ledger owner: this service deliberately does not race
// it by writing task rows itself.
func (s *Service) TerminalizeTask(ctx context.Context, command TaskTerminalCommand) error {
	if s == nil || !validateUUID(command.TaskID) || (command.Reason != "canceled" && command.Reason != "timed_out") {
		return ErrInvalid
	}
	terminalizer, ok := s.repository.(TaskTerminalizer)
	if !ok {
		return ErrBindingUnavailable
	}
	if command.At.IsZero() {
		command.At = s.now().UTC()
	}
	return terminalizer.TerminalizeTask(ctx, command)
}
