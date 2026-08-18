// Package action owns the shared ProductCore action boundary primitives.
package action

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

const settlementTimeout = 45 * time.Second

type settlementDeadlineContextKey struct{}

const (
	RequestNotFoundCode       = "request_not_found"
	RequestExpiredCode        = "request_expired"
	MatrixJoinUnconfirmedCode = "matrix_join_unconfirmed"
	JoinResultUnconfirmedCode = "join_result_unconfirmed"
	MatrixJoinFailedCode      = "matrix_join_failed"
	OperationIDInvalidCode    = "operation_id_invalid"
	OperationIDConflictCode   = "operation_id_conflict"
	OperationRecoveryCode     = "operation_recovery_failed"
)

// Error is the transport-neutral ProductCore error returned by action handlers.
// Status is consumed by HTTP and realtime adapters and is intentionally omitted
// from the JSON error body.
type Error struct {
	Status        int    `json:"-"`
	Error         string `json:"error"`
	Code          string `json:"code,omitempty"`
	OperationID   string `json:"operation_id,omitempty"`
	CurrentRoomID string `json:"current_room_id,omitempty"`
}

// Handler implements one ProductCore action.
type Handler func(context.Context, map[string]any) (any, *Error)

// SettlementContext keeps a mutation alive for a bounded period after its
// external side effect starts. HTTP/WS request cancellation must not strand a
// committed Matrix write without its durable ProductCore projections.
func SettlementContext(ctx context.Context) (context.Context, context.CancelFunc) {
	base := context.WithoutCancel(ctx)
	deadline, nested := ctx.Value(settlementDeadlineContextKey{}).(time.Time)
	if !nested {
		deadline = time.Now().Add(settlementTimeout)
	} else if currentDeadline, ok := ctx.Deadline(); ok && currentDeadline.Before(deadline) {
		deadline = currentDeadline
	}
	base = context.WithValue(base, settlementDeadlineContextKey{}, deadline)
	return context.WithDeadline(base, deadline)
}

// StatusError creates a ProductCore error with an explicit transport status.
func StatusError(status int, message string) *Error {
	return &Error{Status: status, Error: message}
}

// BadRequest creates a 400 ProductCore error.
func BadRequest(message string) *Error {
	return StatusError(http.StatusBadRequest, message)
}

// CodedError creates a ProductCore error with a stable machine-readable code.
func CodedError(status int, code, message string) *Error {
	return &Error{Status: status, Error: message, Code: code}
}

// InternalError preserves the existing ProductCore internal-error envelope.
func InternalError(err error) *Error {
	message := err.Error()
	switch message {
	case "matrix member lookup is not configured":
		message = "Matrix member lookup is not configured"
	case "matrix transport is not configured":
		message = "Matrix transport is not configured"
	}
	return StatusError(http.StatusInternalServerError, fmt.Sprintf("internal error: %s", message))
}
