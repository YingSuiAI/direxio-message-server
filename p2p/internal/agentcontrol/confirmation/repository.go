package confirmation

import "context"

// Repository is the durable contract. Implementations must serialize each
// mutation, enforce the live domain/target uniqueness rule, and check exact
// idempotent replays before reading mutable state.
type Repository interface {
	Request(context.Context, RequestCommand) (Confirmation, error)
	Get(context.Context, string) (Confirmation, error)
	List(context.Context, ListQuery) (Page, error)
	Confirm(context.Context, ConfirmCommand) (Confirmation, error)
	Reject(context.Context, RejectCommand) (Confirmation, error)
	Consume(context.Context, ConsumeCommand) (Confirmation, error)
	Expire(context.Context, ExpireCommand) (Confirmation, error)
	ReleaseReservation(context.Context, ReleaseReservationCommand) (Confirmation, error)
}

// TaskTerminalizer is intentionally optional so existing durable stores keep
// the core confirmation repository contract.  The task ledger depends on this
// small hook, not on an AWS or extension-specific lifecycle API.
type TaskTerminalizer interface {
	TerminalizeTask(context.Context, TaskTerminalCommand) error
}
