package agent

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
)

func TestExternalAgentActionErrorClassifiesForgedServerDerivedIdentity(t *testing.T) {
	err := externalAgentActionError(errors.New(`agent operation failed: query error: request field "owner_id" is server-derived`))
	if err == nil || err.Status != http.StatusBadRequest {
		t.Fatalf("forged identity status = %#v, want HTTP 400", err)
	}
}

func TestExternalAgentActionErrorSanitizesInvalidAgentResult(t *testing.T) {
	const upstreamDetail = "provider response contained secret-canary"
	err := externalAgentActionError(fmt.Errorf("catalog adapter failed: %w: %s", agentgateway.ErrInvalidActionResult, upstreamDetail))
	if err == nil || err.Status != http.StatusBadGateway {
		t.Fatalf("invalid Agent result status = %#v, want HTTP 502", err)
	}
	if err.Error != "external native agent returned an invalid response" {
		t.Fatalf("invalid Agent result message = %q", err.Error)
	}
	if strings.Contains(err.Error, upstreamDetail) {
		t.Fatalf("invalid Agent result leaked upstream detail: %q", err.Error)
	}
}

func TestExternalAgentActionErrorUsesStructuredCapabilityCode(t *testing.T) {
	secret := "provider-secret-canary"
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
	for code, status := range cases {
		err := externalAgentActionError(fmt.Errorf("wrapped: %w: %s", &agentgateway.CapabilityError{Code: code}, secret))
		if err == nil || err.Status != status {
			t.Errorf("capability code %s status = %#v, want %d", code, err, status)
		}
		if strings.Contains(err.Error, secret) {
			t.Errorf("capability code %s leaked secret: %q", code, err.Error)
		}
	}
}

func TestExternalAgentActionErrorMapsInvalidRequestSentinel(t *testing.T) {
	err := externalAgentActionError(fmt.Errorf("wrapped: %w", agentgateway.ErrInvalidActionRequest))
	if err == nil || err.Status != http.StatusBadRequest {
		t.Fatalf("invalid request status = %#v, want HTTP 400", err)
	}
}
