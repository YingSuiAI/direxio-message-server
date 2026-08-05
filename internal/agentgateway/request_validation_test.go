package agentgateway

import (
	"encoding/json"
	"errors"
	"math"
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
	jsonNumbers := cloneParams(base)
	jsonNumbers["model_profile_revision"] = json.Number("2")
	jsonNumbers["credential_version"] = json.Number("3")
	if err := ValidateActionRequest("agent.chat", jsonNumbers); err != nil {
		t.Fatalf("JSON-number chat profile pins rejected: %v", err)
	}
	for _, field := range []string{"model_profile_id", "model_profile_revision", "credential_version"} {
		params := cloneParams(base)
		delete(params, field)
		if err := ValidateActionRequest("agent.chat.stream", params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("missing %s error = %v, want ErrInvalidActionRequest", field, err)
		}
	}
	for _, field := range []string{"model_profile", "tool_credentials", "default_profile", "client_model_profile_id", "messages", "history", "chat_history", "conversation_history"} {
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

func TestValidateChatRequestRejectsNestedSensitiveKeysButNotPromptValues(t *testing.T) {
	base := map[string]any{
		"message":                "A prompt may mention api_key, authorization, and password.",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
	}
	for _, test := range []struct {
		name string
		key  string
	}{
		{name: "tool credentials", key: "tool_credentials"},
		{name: "credentials plural", key: "credentials"},
		{name: "aws credentials", key: "aws_credentials"},
		{name: "aws credentials compact", key: "awsCredentials"},
		{name: "secrets plural", key: "secrets"},
		{name: "tokens plural", key: "tokens"},
		{name: "model profile", key: " model_profile "},
		{name: "model profile camel variant", key: "modelProfileId"},
		{name: "client profile", key: "CLIENT_MODEL_PROFILE_ID"},
		{name: "client profile camel variant", key: "clientModelProfileId"},
		{name: "credential version camel variant", key: "credentialVersion"},
		{name: "api key", key: " API_KEY "},
		{name: "api key compact", key: "ApiKey"},
		{name: "api keys plural", key: "api_keys"},
		{name: "authorization", key: "authorization"},
		{name: "request headers camel", key: "requestHeaders"},
		{name: "request headers compact", key: "requestheaders"},
		{name: "access token", key: "access_token"},
		{name: "bearer", key: "bearer"},
		{name: "bearer token camel", key: "bearerToken"},
		{name: "db pass camel", key: "dbPass"},
		{name: "user pass camel", key: "userPass"},
		{name: "pass exact", key: "pass"},
		{name: "db passes plural", key: "dbPasses"},
		{name: "http basic camel", key: "httpBasic"},
		{name: "http basic auth camel", key: "httpBasicAuth"},
		{name: "basic exact", key: "basic"},
		{name: "http basics plural", key: "httpBasics"},
		{name: "headers", key: "headers"},
		{name: "cookie", key: "cookie"},
		{name: "password", key: "password"},
		{name: "passwords plural", key: "passwords"},
		{name: "passwds plural", key: "passwds"},
		{name: "passphrases plural", key: "passphrases"},
		{name: "authorizations plural", key: "authorizations"},
		{name: "secret", key: "secret"},
		{name: "private key", key: "private_key"},
		{name: "messages", key: "messages"},
		{name: "history", key: "history"},
		{name: "chat history camel", key: "chatHistory"},
		{name: "conversation history", key: "conversation_history"},
	} {
		t.Run(test.name, func(t *testing.T) {
			params := cloneParams(base)
			params["metadata"] = []any{
				map[string]any{"label": "ordinary value", "nested": map[string]string{test.key: "secret-value"}},
			}
			err := ValidateActionRequest("agent.chat.stream", params)
			if !errors.Is(err, ErrInvalidActionRequest) {
				t.Fatalf("nested key %q accepted: %v", test.key, err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("nested key %q error leaked its value: %v", test.key, err)
			}
		})
	}

	for _, value := range []any{
		"api_key",
		[]string{"authorization", "password"},
		map[string]any{"message": "api_key"},
	} {
		params := cloneParams(base)
		params["metadata"] = value
		if err := ValidateActionRequest("agent.chat", params); err != nil {
			t.Errorf("ordinary value %#v was rejected: %v", value, err)
		}
	}
}

func TestPositiveIntegerUsesExactInt64UpperBound(t *testing.T) {
	max := uint64(math.MaxInt64)
	for _, value := range []any{
		int64(math.MaxInt64),
		max,
		json.Number("2"),
		json.Number("2.0"),
		json.Number("9223372036854775807"),
		json.Number("9223372036854775807.0"),
	} {
		if !positiveInteger(value) {
			t.Errorf("valid positive integer %#v was rejected", value)
		}
	}

	for _, value := range []any{
		uint64(math.MaxInt64) + 1,
		uint(max) + 1,
		float64(math.MaxInt64),
		float32(math.MaxInt64),
		json.Number("9223372036854775808"),
		json.Number("9223372036854775808.0"),
		json.Number("1e100"),
	} {
		if positiveInteger(value) {
			t.Errorf("overflowing positive integer %#v was accepted", value)
		}
	}
}

func TestTurnControlRequestsRequireCanonicalClosedShapes(t *testing.T) {
	turnID := "11111111-1111-4111-8111-111111111111"
	conversationID := "22222222-2222-4222-8222-222222222222"
	if err := ValidateActionRequest("agent.chat.turn.stop", map[string]any{"turn_id": turnID}); err != nil {
		t.Fatalf("canonical stop rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.chat.turns.list", map[string]any{"conversation_id": conversationID, "page_token": "", "limit": json.Number("20")}); err != nil {
		t.Fatalf("canonical list rejected: %v", err)
	}
	if err := ValidateActionRequest("agent.chat.turns.list", map[string]any{"conversation_id": conversationID, "limit": json.Number("1000")}); err != nil {
		t.Fatalf("maximum canonical list limit rejected: %v", err)
	}
	for _, test := range []struct {
		action string
		params map[string]any
	}{
		{"agent.chat.turn.stop", map[string]any{"turn_id": "turn-1"}},
		{"agent.chat.turn.stop", map[string]any{"turn_id": "00000000-0000-0000-0000-000000000000"}},
		{"agent.chat.turn.stop", map[string]any{"turn_id": turnID, "conversation_id": conversationID}},
		{"agent.chat.turns.list", map[string]any{"conversation_id": "conversation-1"}},
		{"agent.chat.turns.list", map[string]any{"conversation_id": conversationID, "next_cursor": "legacy"}},
		{"agent.chat.turns.list", map[string]any{"conversation_id": conversationID, "limit": 0}},
		{"agent.chat.turns.list", map[string]any{"conversation_id": conversationID, "limit": json.Number("1001")}},
	} {
		if err := ValidateActionRequest(test.action, test.params); !errors.Is(err, ErrInvalidActionRequest) {
			t.Errorf("%s %#v error=%v, want ErrInvalidActionRequest", test.action, test.params, err)
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
