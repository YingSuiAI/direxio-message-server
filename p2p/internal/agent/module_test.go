package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
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

type requestValidationRunner struct {
	invokeCalls int
	streamCalls int
}

type sequenceRunner struct{}

func (sequenceRunner) Apply(context.Context, string) error { return nil }
func (sequenceRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return nil, nil
}
func (sequenceRunner) Stream(_ context.Context, _ string, _ map[string]any, emit func(agentstream.Event) error) error {
	return emit(agentstream.Event{Event: "done", Seq: 31, Data: map[string]any{"done": true}})
}

func TestDurableStreamProjectsRunnerSequence(t *testing.T) {
	module := New(Config{Runner: sequenceRunner{}})
	params := map[string]any{
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
		"turn_id":                "turn-1",
		"conversation_id":        "conversation-1",
	}
	var received agentstream.StreamEvent
	if err := module.DurableStream(context.Background(), "@owner:example.test", "agent.chat.stream", params, func(event agentstream.StreamEvent) error {
		received = event
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if received.Seq != 31 || received.Event != "done" || received.Turn.State != agentstream.StateSucceeded {
		t.Fatalf("durable stream event = %#v, want done seq 31", received)
	}
}

func (r *requestValidationRunner) Apply(context.Context, string) error { return nil }

func (r *requestValidationRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	r.invokeCalls++
	return map[string]any{"ok": true}, nil
}

func (r *requestValidationRunner) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	r.streamCalls++
	return nil
}

func TestChatRequestValidationRunsBeforeInvokeStreamAndDurableRetry(t *testing.T) {
	runner := &requestValidationRunner{}
	module := New(Config{Runner: runner})
	params := map[string]any{
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
		"metadata": []any{map[string]any{
			"authorization": "secret-value",
		}},
	}

	handler := module.Handlers()["agent.chat"]
	if _, apiErr := handler(context.Background(), params); apiErr == nil || apiErr.Status != http.StatusBadRequest {
		t.Fatalf("HTTP ProductAction validation = %#v, want HTTP 400", apiErr)
	}
	if runner.invokeCalls != 0 {
		t.Fatalf("invalid HTTP ProductAction reached runner %d time(s)", runner.invokeCalls)
	}

	if err := module.Stream(context.Background(), "agent.chat.stream", params, func(agentstream.Event) error { return nil }); !errors.Is(err, agentgateway.ErrInvalidActionRequest) {
		t.Fatalf("WS stream validation = %v, want ErrInvalidActionRequest", err)
	}
	if runner.streamCalls != 0 {
		t.Fatalf("invalid WS stream reached runner %d time(s)", runner.streamCalls)
	}

	durableParams := cloneMap(params)
	durableParams["turn_id"] = "turn-1"
	durableParams["conversation_id"] = "conversation-1"
	if err := module.DurableStream(context.Background(), "@owner:example.test", "agent.chat.stream", durableParams, func(agentstream.StreamEvent) error { return nil }); !errors.Is(err, agentgateway.ErrInvalidActionRequest) {
		t.Fatalf("durable retry/replay validation = %v, want ErrInvalidActionRequest", err)
	}
	if runner.streamCalls != 0 {
		t.Fatalf("invalid durable retry/replay reached runner %d time(s)", runner.streamCalls)
	}
}

type configContractRunner struct {
	action string
	params map[string]any
}

func (r *configContractRunner) Apply(context.Context, string) error { return nil }

func (r *configContractRunner) Invoke(_ context.Context, action string, params map[string]any) (map[string]any, error) {
	r.action = action
	r.params = cloneMap(params)
	return map[string]any{
		"display_name": "Ying Remote",
		"avatar_url":   "mxc://ying-remote",
	}, nil
}

func (r *configContractRunner) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	return nil
}

type configContractAccount struct {
	config         dirextalkdomain.AgentConfig
	syncedIdentity dirextalkdomain.AgentIdentityConfig
}

func (a *configContractAccount) Password() string { return "password" }

func (a *configContractAccount) CreateMatrixSession(context.Context, map[string]any) (MatrixSession, *actionbase.Error) {
	return MatrixSession{}, nil
}

func (a *configContractAccount) Config() dirextalkdomain.AgentConfig { return a.config }

func (a *configContractAccount) UpdateConfig(_ context.Context, mutate func(dirextalkdomain.AgentConfig) dirextalkdomain.AgentConfig) (dirextalkdomain.AgentConfig, *actionbase.Error) {
	a.config = mutate(a.config)
	return a.config, nil
}

func (a *configContractAccount) SyncOnlineIdentity(_ context.Context, identity dirextalkdomain.AgentIdentityConfig) *actionbase.Error {
	a.syncedIdentity = identity
	return nil
}

func (a *configContractAccount) PublishOffline(context.Context) *actionbase.Error { return nil }

func TestExternalConfigKeepsNativeAndOnlineIdentityOwnershipSeparate(t *testing.T) {
	runner := &configContractRunner{}
	account := &configContractAccount{config: NormalizeConfig(dirextalkdomain.AgentConfig{})}
	handler := New(Config{Runner: runner, Account: account, OwnerID: func() string { return "@owner:example.com" }}).Handlers()[actionConfigUpdate]

	value, actionErr := handler(context.Background(), map[string]any{
		"native_agent_identity": map[string]any{
			"display_name": "Ying Requested",
			"avatar_url":   "mxc://ying-requested",
		},
	})
	if actionErr != nil {
		t.Fatalf("native identity update failed: %v", actionErr)
	}
	if runner.action != actionConfigUpdate ||
		runner.params["display_name"] != "Ying Requested" ||
		runner.params["avatar_url"] != "mxc://ying-requested" ||
		runner.params["native_agent_identity"] != nil ||
		runner.params["online_agent_identity"] != nil {
		t.Fatalf("native identity update crossed ownership boundary: action=%q params=%#v", runner.action, runner.params)
	}
	if account.syncedIdentity != (dirextalkdomain.AgentIdentityConfig{}) {
		t.Fatalf("native-only update touched Matrix identity: %#v", account.syncedIdentity)
	}
	response := value.(map[string]any)
	if response["online_agent_identity"].(map[string]any)["display_name"] != DefaultOnlineAgentDisplayName {
		t.Fatalf("native response changed Online identity: %#v", response)
	}

	value, actionErr = handler(context.Background(), map[string]any{
		"online_agent_identity": map[string]any{
			"display_name": "Your Online",
			"avatar_url":   "mxc://online",
		},
	})
	if actionErr != nil {
		t.Fatalf("online identity update failed: %v", actionErr)
	}
	if runner.action != actionConfigGet || runner.params["online_agent_identity"] != nil {
		t.Fatalf("online identity reached external Agent: action=%q params=%#v", runner.action, runner.params)
	}
	if account.syncedIdentity.DisplayName != "Your Online" || account.syncedIdentity.AvatarURL != "mxc://online" {
		t.Fatalf("online identity was not synchronized to Matrix: %#v", account.syncedIdentity)
	}
	response = value.(map[string]any)
	if response["native_agent_identity"].(map[string]any)["display_name"] != "Ying Remote" ||
		response["online_agent_identity"].(map[string]any)["display_name"] != "Your Online" {
		t.Fatalf("mode-specific identities were not preserved: %#v", response)
	}
}
