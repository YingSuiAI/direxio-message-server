package dirextalktransport

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

var (
	// ErrMatrixEventUnknown means Matrix could not prove whether the staged
	// event was accepted. Callers must preserve the prepared row and leave the
	// operation uncertain; retrying with a second PDU would risk a duplicate.
	ErrMatrixEventUnknown  = errors.New("Matrix event acceptance is unknown")
	ErrMatrixEventRejected = errors.New("Matrix event was rejected")
)

// ExecutePreparedMatrixMutation is the common crash-safe write algorithm for
// Product Capability Matrix mutations:
//
//	prepare/sign -> durable stage -> SendEvents -> return receipt
//
// A retry first checks the staged event.  If Matrix already contains the
// event, it returns the receipt without sending again; otherwise it submits
// the exact persisted PDU.  It never rebuilds a PDU after staging.
func ExecutePreparedMatrixMutation(ctx context.Context, port PreparedMessagePort, store PreparedMatrixMutationStore, operation CapabilityOperationContext, capability, operationName string, request SendMessageRequest) (SendMessageResult, error) {
	if port == nil || store == nil {
		return SendMessageResult{}, errors.New("durable Matrix mutation is unavailable")
	}
	if operation.OperationID == "" || operation.OwnerID == "" || operation.Generation <= 0 || len(operation.RootDigest) != 32 {
		return SendMessageResult{}, errors.New("capability operation context is incomplete")
	}
	existing, err := store.GetMatrixPreparedEvent(ctx, operation.OperationID, operation.OwnerID, operation.Generation, operation.RootDigest)
	if err == nil && existing != nil {
		if existing.Capability != capability || existing.Operation != operationName {
			return SendMessageResult{}, fmt.Errorf("prepared Matrix event operation binding mismatch")
		}
		if statusPort, ok := port.(MatrixEventStatusPort); ok {
			disposition, statusErr := statusPort.EventStatus(ctx, clonePreparedMessage(existing.Event))
			if statusErr != nil {
				return SendMessageResult{}, fmt.Errorf("%w: reconcile prepared Matrix event %s: %v", ErrMatrixEventUnknown, existing.Event.EventID, statusErr)
			}
			switch disposition {
			case MatrixEventAccepted:
				result := SendMessageResult{EventID: existing.Event.EventID, OriginServerTS: existing.Event.OriginServerTS}
				if receiptStore, ok := store.(PreparedMatrixMutationReceiptStore); ok {
					_ = receiptStore.MarkMatrixPreparedEventSent(ctx, operation.OperationID, operation.OwnerID, operation.Generation, operation.RootDigest, result)
				}
				return result, nil
			case MatrixEventRejected:
				return SendMessageResult{}, fmt.Errorf("%w: %s", ErrMatrixEventRejected, existing.Event.EventID)
			case MatrixEventUnknown:
				// A present event without an authoritative acceptance bit is not
				// safe to submit twice.  Keep the staged row for an explicit
				// reconcile instead of silently converting it to a duplicate.
				present, existsErr := port.EventExists(ctx, existing.Event.RoomID, existing.Event.EventID)
				if existsErr != nil {
					return SendMessageResult{}, fmt.Errorf("%w: reconcile prepared Matrix event %s: %v", ErrMatrixEventUnknown, existing.Event.EventID, existsErr)
				}
				if present {
					return SendMessageResult{}, fmt.Errorf("%w: %s", ErrMatrixEventUnknown, existing.Event.EventID)
				}
				// The stronger status port proved no event is present, so the
				// exact staged PDU may be submitted once.
			default:
				return SendMessageResult{}, fmt.Errorf("prepared Matrix event %s returned invalid disposition %q", existing.Event.EventID, disposition)
			}
		}
		present, existsErr := port.EventExists(ctx, existing.Event.RoomID, existing.Event.EventID)
		if existsErr != nil {
			return SendMessageResult{}, fmt.Errorf("%w: reconcile prepared Matrix event %s: %v", ErrMatrixEventUnknown, existing.Event.EventID, existsErr)
		}
		if present {
			result := SendMessageResult{EventID: existing.Event.EventID, OriginServerTS: existing.Event.OriginServerTS}
			if receiptStore, ok := store.(PreparedMatrixMutationReceiptStore); ok {
				_ = receiptStore.MarkMatrixPreparedEventSent(ctx, operation.OperationID, operation.OwnerID, operation.Generation, operation.RootDigest, result)
			}
			return result, nil
		}
		result, sendErr := port.SendPreparedMessage(ctx, clonePreparedMessage(existing.Event))
		if sendErr != nil {
			if errors.Is(sendErr, ErrMatrixEventRejected) {
				return SendMessageResult{}, sendErr
			}
			return SendMessageResult{}, fmt.Errorf("%w: resend prepared Matrix event %s: %v", ErrMatrixEventUnknown, existing.Event.EventID, sendErr)
		}
		result, receiptErr := validatePreparedReceipt(existing.Event, result)
		if receiptErr != nil {
			return SendMessageResult{}, fmt.Errorf("%w: invalid Matrix receipt for %s: %v", ErrMatrixEventUnknown, existing.Event.EventID, receiptErr)
		}
		if receiptStore, ok := store.(PreparedMatrixMutationReceiptStore); ok {
			_ = receiptStore.MarkMatrixPreparedEventSent(ctx, operation.OperationID, operation.OwnerID, operation.Generation, operation.RootDigest, result)
		}
		return result, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return SendMessageResult{}, err
	}
	prepared, err := port.PrepareMessage(ctx, request)
	if err != nil {
		return SendMessageResult{}, err
	}
	if err := validatePreparedMessage(prepared); err != nil {
		return SendMessageResult{}, err
	}
	if err := store.PrepareMatrixEvent(ctx, PreparedMatrixEvent{
		OperationID: operation.OperationID,
		Capability:  capability,
		Operation:   operationName,
		OwnerID:     operation.OwnerID,
		Generation:  operation.Generation,
		RootDigest:  append([]byte(nil), operation.RootDigest...),
		LogicalID:   request.LogicalID,
		PostID:      request.PostID,
		State:       "prepared",
		Attempt:     1,
		Revision:    1,
		Event:       clonePreparedMessage(prepared),
	}); err != nil {
		return SendMessageResult{}, err
	}
	result, err := port.SendPreparedMessage(ctx, clonePreparedMessage(prepared))
	if err != nil {
		// Leave the staged event intact. The next idempotent replay will query
		// Matrix and either recover this receipt or resend the identical PDU.
		if errors.Is(err, ErrMatrixEventRejected) {
			return SendMessageResult{}, err
		}
		return SendMessageResult{}, fmt.Errorf("%w: send prepared Matrix event %s: %v", ErrMatrixEventUnknown, prepared.EventID, err)
	}
	result, receiptErr := validatePreparedReceipt(prepared, result)
	if receiptErr != nil {
		return SendMessageResult{}, fmt.Errorf("%w: invalid Matrix receipt for %s: %v", ErrMatrixEventUnknown, prepared.EventID, receiptErr)
	}
	if receiptStore, ok := store.(PreparedMatrixMutationReceiptStore); ok {
		_ = receiptStore.MarkMatrixPreparedEventSent(ctx, operation.OperationID, operation.OwnerID, operation.Generation, operation.RootDigest, result)
	}
	return result, nil
}

func validatePreparedMessage(message PreparedMessage) error {
	if message.EventID == "" || message.RoomID == "" || message.SenderMXID == "" || message.RoomVersion == "" || len(message.EventJSON) == 0 || len(message.EventJSON) > MaxPreparedMatrixEventBytes || len(message.ContentDigest) != 32 {
		return errors.New("prepared Matrix PDU is incomplete")
	}
	return nil
}

func validatePreparedReceipt(prepared PreparedMessage, result SendMessageResult) (SendMessageResult, error) {
	if result.EventID == "" {
		return SendMessageResult{}, errors.New("Matrix transport returned an empty event id")
	}
	if result.EventID != prepared.EventID {
		return SendMessageResult{}, fmt.Errorf("Matrix transport returned event %s for prepared event %s", result.EventID, prepared.EventID)
	}
	if result.OriginServerTS == 0 {
		result.OriginServerTS = prepared.OriginServerTS
	}
	return result, nil
}

func clonePreparedMessage(message PreparedMessage) PreparedMessage {
	message.EventJSON = append([]byte(nil), message.EventJSON...)
	message.ContentDigest = append([]byte(nil), message.ContentDigest...)
	return message
}
