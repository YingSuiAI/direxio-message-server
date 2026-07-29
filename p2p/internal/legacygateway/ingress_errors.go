package legacygateway

import "errors"

var ErrInvalidIngressConfig = errors.New("legacy gateway ingress configuration is invalid")

type IngressErrorCode string

const (
	IngressErrorUnknown            IngressErrorCode = "unknown"
	IngressErrorInvalidArgument    IngressErrorCode = "invalid_argument"
	IngressErrorUnauthenticated    IngressErrorCode = "unauthenticated"
	IngressErrorPermissionDenied   IngressErrorCode = "permission_denied"
	IngressErrorNotFound           IngressErrorCode = "not_found"
	IngressErrorAlreadyExists      IngressErrorCode = "already_exists"
	IngressErrorConflict           IngressErrorCode = "conflict"
	IngressErrorFailedPrecondition IngressErrorCode = "failed_precondition"
	IngressErrorResourceExhausted  IngressErrorCode = "resource_exhausted"
	IngressErrorCanceled           IngressErrorCode = "canceled"
	IngressErrorDeadlineExceeded   IngressErrorCode = "deadline_exceeded"
	IngressErrorUnavailable        IngressErrorCode = "unavailable"
	IngressErrorInternal           IngressErrorCode = "internal"
)

type IngressError struct {
	code IngressErrorCode
}

func (err *IngressError) Error() string {
	return "legacy gateway ingress failed: " + string(err.code)
}

func IngressErrorCodeOf(err error) IngressErrorCode {
	var ingressError *IngressError
	if errors.As(err, &ingressError) {
		return ingressError.code
	}
	return IngressErrorUnknown
}

// IsPermanentError distinguishes stable request/authentication rejection from
// retryable transport and capacity failures without exposing gRPC details.
func IsPermanentError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrInvalidIngressConfig) {
		return true
	}
	var contractError *ContractError
	if errors.As(err, &contractError) {
		return true
	}
	switch IngressErrorCodeOf(err) {
	case IngressErrorInvalidArgument,
		IngressErrorUnauthenticated,
		IngressErrorPermissionDenied,
		IngressErrorNotFound,
		IngressErrorAlreadyExists,
		IngressErrorConflict,
		IngressErrorFailedPrecondition:
		return true
	default:
		return false
	}
}
