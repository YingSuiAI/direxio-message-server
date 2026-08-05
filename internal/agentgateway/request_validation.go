package agentgateway

import (
	"errors"
	"fmt"
	"math"
	"strings"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

// ErrInvalidActionRequest marks ProductCore parameters that cannot be sent to
// the external Agent capability. It is intentionally separate from a
// capability INVALID_ARGUMENT response: these failures are rejected before a
// catalog lookup, grant, or operation is created.
var ErrInvalidActionRequest = errors.New("native agent action request is invalid")

// InvalidActionRequestError carries only a field name and a stable reason. It
// never includes the value of a write-only field (for example api_key).
type InvalidActionRequestError struct {
	Action string
	Field  string
	Reason string
}

func (e *InvalidActionRequestError) Error() string {
	if e == nil {
		return ErrInvalidActionRequest.Error()
	}
	action := strings.TrimSpace(e.Action)
	field := strings.TrimSpace(e.Field)
	reason := strings.TrimSpace(e.Reason)
	if action == "" {
		action = "native agent action"
	}
	if field == "" {
		field = "request"
	}
	if reason == "" {
		reason = "is invalid"
	}
	return fmt.Sprintf("%s %s: %s", action, field, reason)
}

func (e *InvalidActionRequestError) Unwrap() error { return ErrInvalidActionRequest }

func invalidActionRequest(action, field, reason string) error {
	return &InvalidActionRequestError{Action: strings.TrimSpace(action), Field: field, Reason: reason}
}

// ValidateActionRequest enforces the request-side parts of the frozen Native
// Agent ProductCore contract. It is safe to call from both the HTTP/WS module
// and the capability gateway; the validation is side-effect free.
func ValidateActionRequest(action string, params map[string]any) error {
	action = strings.TrimSpace(action)
	switch action {
	case "agent.chat", "agent.chat.stream":
		return validateChatRequest(action, params)
	case "agent.models.list":
		return validateModelCatalogRequest(action, params)
	default:
		return nil
	}
}

func validateChatRequest(action string, params map[string]any) error {
	if params == nil {
		return invalidActionRequest(action, "model_profile_id", "is required")
	}
	// These fields were accepted by older embedded-Agent clients. Keeping them
	// out of the request is important: the selected profile and its secret
	// revision must be resolved server-side and pinned by the caller.
	for _, field := range []string{
		"client_model_profile_id",
		"model_profile",
		"default_profile",
		"default_profile_id",
		"default_model_profile_id",
		"use_default_profile",
		"tool_credentials",
	} {
		if _, present := params[field]; present {
			return invalidActionRequest(action, field, "is not supported")
		}
	}

	profileID, present := params["model_profile_id"]
	if !present {
		return invalidActionRequest(action, "model_profile_id", "is required")
	}
	profileIDText, ok := profileID.(string)
	if !ok || strings.TrimSpace(profileIDText) == "" {
		return invalidActionRequest(action, "model_profile_id", "must be a non-empty string")
	}
	for _, field := range []string{"model_profile_revision", "credential_version"} {
		value, present := params[field]
		if !present {
			return invalidActionRequest(action, field, "is required")
		}
		if !positiveInteger(value) {
			return invalidActionRequest(action, field, "must be a positive integer")
		}
	}
	return nil
}

func validateModelCatalogRequest(action string, params map[string]any) error {
	if params == nil {
		return nil
	}
	if value, present := params["model_kind"]; present {
		kind, ok := value.(string)
		if !ok || !isModelKind(kind) {
			return invalidActionRequest(action, "model_kind", "must be conversation, embedding, or speech")
		}
	}

	profileFields := []string{"model_profile_id", "client_model_profile_id"}
	profilePresent := false
	for _, field := range profileFields {
		if value, present := params[field]; present {
			profilePresent = true
			profileID, ok := value.(string)
			if !ok || strings.TrimSpace(profileID) == "" {
				return invalidActionRequest(action, field, "must be a non-empty string")
			}
		}
	}
	if _, apiKeyPresent := params["api_key"]; apiKeyPresent {
		if profilePresent {
			return invalidActionRequest(action, "api_key", "cannot be combined with a model profile id")
		}
		apiKey, ok := params["api_key"].(string)
		if !ok || strings.TrimSpace(apiKey) == "" {
			return invalidActionRequest(action, "api_key", "must be a non-empty string")
		}
	}
	if _, bothProfiles := params["model_profile_id"]; bothProfiles {
		if _, legacy := params["client_model_profile_id"]; legacy {
			return invalidActionRequest(action, "model_profile_id", "cannot be combined with client_model_profile_id")
		}
	}
	return nil
}

func isModelKind(value string) bool {
	switch value {
	case "conversation", "embedding", "speech":
		return true
	default:
		return false
	}
}

func positiveInteger(value any) bool {
	switch typed := value.(type) {
	case int:
		return typed > 0
	case int8:
		return typed > 0
	case int16:
		return typed > 0
	case int32:
		return typed > 0
	case int64:
		return typed > 0
	case uint:
		return typed > 0
	case uint8:
		return typed > 0
	case uint16:
		return typed > 0
	case uint32:
		return typed > 0
	case uint64:
		return typed > 0
	case float32:
		value := float64(typed)
		return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0) && value <= float64(math.MaxInt64) && math.Trunc(value) == value
	case float64:
		return typed > 0 && !math.IsNaN(typed) && !math.IsInf(typed, 0) && typed <= float64(math.MaxInt64) && math.Trunc(typed) == typed
	default:
		return false
	}
}

// CapabilityError is the safe, structured error returned when Agent rejects
// a capability operation. The upstream message is deliberately discarded;
// Agent providers may include credential-bearing details in that field.
type CapabilityError struct {
	Code capv1.ErrorCode
}

func (e *CapabilityError) Error() string {
	if e == nil {
		return "external native agent operation failed"
	}
	switch e.Code {
	case capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return "external native agent rejected the request"
	case capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return "external native agent denied the request"
	case capv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		return "external native agent resource was not found"
	case capv1.ErrorCode_ERROR_CODE_CONFLICT:
		return "external native agent reported a conflict"
	case capv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED:
		return "external native agent precondition failed"
	case capv1.ErrorCode_ERROR_CODE_NOT_READY:
		return "external native agent is not ready"
	case capv1.ErrorCode_ERROR_CODE_UNAVAILABLE:
		return "external native agent is unavailable"
	case capv1.ErrorCode_ERROR_CODE_UNCERTAIN:
		return "external native agent operation outcome is uncertain"
	case capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED:
		return "external native agent upstream operation failed"
	default:
		return "external native agent operation failed"
	}
}

func capabilityError(code capv1.ErrorCode) error { return &CapabilityError{Code: code} }

// CapabilityHTTPStatus translates the structured Agent error taxonomy to the
// stable ProductCore HTTP semantics. Unknown codes fail closed as 502.
func CapabilityHTTPStatus(code capv1.ErrorCode) int {
	switch code {
	case capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:
		return 400
	case capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:
		return 403
	case capv1.ErrorCode_ERROR_CODE_NOT_FOUND:
		return 404
	case capv1.ErrorCode_ERROR_CODE_CONFLICT, capv1.ErrorCode_ERROR_CODE_UNCERTAIN, capv1.ErrorCode_ERROR_CODE_CYCLE_DETECTED:
		return 409
	case capv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED:
		return 412
	case capv1.ErrorCode_ERROR_CODE_NOT_READY, capv1.ErrorCode_ERROR_CODE_UNAVAILABLE, capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED:
		return 503
	case capv1.ErrorCode_ERROR_CODE_TRUST_FAILED, capv1.ErrorCode_ERROR_CODE_INCOMPATIBLE, capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED:
		return 502
	default:
		return 502
	}
}
