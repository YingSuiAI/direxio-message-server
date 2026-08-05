package agentgrpc

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestSanitizeModelListRPCErrorPreservesSafeProviderFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		err         error
		wantMessage string
		wantCode    string
	}{
		{
			name:        "credential rejected",
			err:         status.Error(codes.PermissionDenied, "model provider rejected the supplied credential"),
			wantMessage: "model provider rejected the API key",
			wantCode:    "M_AGENT_MODEL_CREDENTIAL_REJECTED",
		},
		{
			name:        "model list unsupported",
			err:         status.Error(codes.FailedPrecondition, "model provider does not expose a compatible model list"),
			wantMessage: "model provider does not support model discovery",
			wantCode:    "M_AGENT_MODEL_LIST_UNSUPPORTED",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			err := sanitizeModelListRPCError(context.Background(), test.err)
			if err == nil || err.Error() != test.wantMessage {
				t.Fatalf("error = %v, want %q", err, test.wantMessage)
			}
			var coded codedRunnerError
			if !errors.As(err, &coded) || coded.ErrorCode() != test.wantCode {
				t.Fatalf("coded error = %#v, want %q", err, test.wantCode)
			}
		})
	}
}

func TestSanitizeModelListRPCErrorDoesNotRelabelAuthorizationFailures(t *testing.T) {
	t.Parallel()

	err := sanitizeModelListRPCError(
		context.Background(),
		status.Error(codes.PermissionDenied, "authenticated client lacks the required scope"),
	)
	if err == nil || err.Error() != "agent service request failed (permissiondenied)" {
		t.Fatalf("error = %v", err)
	}
	var coded codedRunnerError
	if errors.As(err, &coded) {
		t.Fatalf("authorization failure was mislabeled as provider failure: %#v", err)
	}
}
