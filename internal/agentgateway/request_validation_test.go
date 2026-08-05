package agentgateway

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

func TestValidateChatRequestRequiresImmutableProfilePins(t *testing.T) {
	base := map[string]any{
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
	}
	if err := ValidateActionRequest("agent.chat", base); err != nil {
		t.Fatalf("complete chat request rejected: %v", err)
	}
	for _, field := range []string{"model_profile_id", "model_profile_revision", "credential_version"} {
		params := cloneParams(base)
		delete(params, field)
		if err := ValidateActionRequest("agent.chat.stream", params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("missing %s error = %v, want ErrInvalidActionRequest", field, err)
		}
	}
	for _, field := range []string{"model_profile", "tool_credentials", "default_profile", "client_model_profile_id"} {
		params := cloneParams(base)
		params[field] = map[string]any{"api_key": "secret"}
		err := ValidateActionRequest("agent.chat", params)
		if !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("legacy %s error = %v, want ErrInvalidActionRequest", field, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("legacy %s error leaked a secret: %v", field, err)
		}
	}
	for _, value := range []any{0, -1, 1.5, "2", nil} {
		params := cloneParams(base)
		params["model_profile_revision"] = value
		if err := ValidateActionRequest("agent.chat", params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("revision %#v accepted: %v", value, err)
		}
	}
}

func TestValidateModelCatalogRequestRejectsInvalidKindAndCredentialMix(t *testing.T) {
	for _, kind := range []any{"", "vision", 1, nil} {
		err := ValidateActionRequest("agent.models.list", map[string]any{"model_kind": kind})
		if !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("model_kind %#v accepted: %v", kind, err)
		}
	}
	for _, params := range []map[string]any{
		{"model_profile_id": "profile", "api_key": "secret"},
		{"client_model_profile_id": "profile", "api_key": "secret"},
	} {
		err := ValidateActionRequest("agent.models.list", params)
		if !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("profile/API key mix accepted: %v", err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("profile/API key error leaked a secret: %v", err)
		}
	}
	if err := ValidateActionRequest("agent.models.list", map[string]any{"model_kind": "speech", "api_key": "secret"}); err != nil {
		t.Fatalf("valid speech catalog request rejected: %v", err)
	}
}

func TestCapabilityHTTPStatusUsesStructuredErrorCode(t *testing.T) {
	cases := map[capv1.ErrorCode]int{
		capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT:    http.StatusBadRequest,
		capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED:   http.StatusForbidden,
		capv1.ErrorCode_ERROR_CODE_NOT_FOUND:           http.StatusNotFound,
		capv1.ErrorCode_ERROR_CODE_CONFLICT:            http.StatusConflict,
		capv1.ErrorCode_ERROR_CODE_PRECONDITION_FAILED: http.StatusPreconditionFailed,
		capv1.ErrorCode_ERROR_CODE_NOT_READY:           http.StatusServiceUnavailable,
		capv1.ErrorCode_ERROR_CODE_UNAVAILABLE:         http.StatusServiceUnavailable,
		capv1.ErrorCode_ERROR_CODE_UNCERTAIN:           http.StatusConflict,
		capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED:     http.StatusBadGateway,
	}
	for code, want := range cases {
		if got := CapabilityHTTPStatus(code); got != want {
			t.Errorf("CapabilityHTTPStatus(%s) = %d, want %d", code, got, want)
		}
	}
	secret := "provider-secret-canary"
	if got := (&CapabilityError{Code: capv1.ErrorCode_ERROR_CODE_UPSTREAM_FAILED}).Error(); strings.Contains(got, secret) {
		t.Fatalf("structured capability error leaked %q: %q", secret, got)
	}
}
