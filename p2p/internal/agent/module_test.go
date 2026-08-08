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
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

func TestExecutionV2ExternalAllowlistKeepsGenericRunMutationsOnly(t *testing.T) {
	actions := make(map[string]bool, len(externalNativeActions))
	for _, action := range externalNativeActions {
		actions[action] = true
	}
	for _, action := range []string{"agent.execution.v2.runs.create", "agent.execution.v2.runs.retry"} {
		if !actions[action] {
			t.Errorf("generic Execution V2 action %s is missing from external routing", action)
		}
	}
	if !actions["agent.execution.v2.artifacts.download"] {
		t.Error("strict Cloud Worker artifact download action is missing from external routing")
	}
	for _, action := range []string{
		"agent.execution.v2.runs.reconcile",
		"agent.execution.v2.confirmations.get",
		"agent.execution.v2.confirmations.list",
		"agent.execution.v2.confirmations.confirm",
		"agent.execution.v2.confirmations.reject",
	} {
		if actions[action] {
			t.Errorf("superseded Execution V2 action %s remains externally routed", action)
		}
	}
}

func TestImageToolsAreOwnerProxyActions(t *testing.T) {
	actions := make(map[string]bool, len(externalNativeActions))
	for _, action := range externalNativeActions {
		actions[action] = true
	}
	module := New(Config{Runner: &requestValidationRunner{}})
	handlers := module.Handlers()
	for _, action := range []string{
		"agent.image_tools.upload.begin", "agent.image_tools.upload.append", "agent.image_tools.upload.commit",
		"agent.image_tools.extract_text", "agent.image_tools.translate_text",
	} {
		if !actions[action] || handlers[action] == nil {
			t.Errorf("image tool action %s is not routed through the external owner proxy", action)
		}
	}
}

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

func TestExternalAgentActionErrorMapsKnowledgeQuotaExceeded(t *testing.T) {
	err := externalAgentActionError(&agentgateway.CapabilityError{Code: capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED, ClientCode: agentgateway.KnowledgeQuotaExceededCode})
	if err == nil || err.Status != http.StatusRequestEntityTooLarge || err.Code != agentgateway.KnowledgeQuotaExceededCode || err.Error != "knowledge quota exceeded" {
		t.Fatalf("knowledge quota ProductCore error = %#v", err)
	}
}

func TestExternalAgentActionErrorMapsInvalidRequestSentinel(t *testing.T) {
	err := externalAgentActionError(fmt.Errorf("wrapped: %w", agentgateway.ErrInvalidActionRequest))
	if err == nil || err.Status != http.StatusBadRequest {
		t.Fatalf("invalid request status = %#v, want HTTP 400", err)
	}
}

func TestExternalAgentActionErrorDistinguishesTimeoutAndUnavailable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{name: "provider timeout", err: grpcstatus.Error(codes.DeadlineExceeded, "context deadline exceeded"), want: http.StatusGatewayTimeout},
		{name: "provider unavailable", err: grpcstatus.Error(codes.Unavailable, "provider unavailable"), want: http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := externalAgentActionError(test.err)
			if got == nil || got.Status != test.want {
				t.Fatalf("error = %#v, want HTTP %d", got, test.want)
			}
		})
	}
}

func TestExternalAgentActionsFailClosedWhenCatalogLeaseIsNotReady(t *testing.T) {
	runner := &requestValidationRunner{}
	module := New(Config{
		Runner:    runner,
		Readiness: func() error { return errors.New("catalog lease expired") },
	})
	handler := module.Handlers()["agent.core.tasks.list"]
	if _, actionErr := handler(context.Background(), map[string]any{}); actionErr == nil || actionErr.Status != http.StatusServiceUnavailable {
		t.Fatalf("unready catalog action error = %#v, want HTTP 503", actionErr)
	}
}

type requestValidationRunner struct {
	invokeCalls int
	streamCalls int
	lastAction  string
	lastParams  map[string]any
}

type sequenceRunner struct{}

func (sequenceRunner) Apply(context.Context, string) error { return nil }
func (sequenceRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return nil, nil
}
func (sequenceRunner) Stream(_ context.Context, _ string, params map[string]any, emit func(agentstream.Event) error) error {
	return emit(agentstream.Event{Event: "done", Seq: 31, Data: map[string]any{
		"idempotency_key": params["idempotency_key"],
		"conversation_id": params["conversation_id"],
		"turn_id":         "33333333-3333-4333-8333-333333333333",
		"revision":        float64(1),
		"sequence":        int64(31),
	}})
}

func TestDurableStreamProjectsRunnerSequence(t *testing.T) {
	module := New(Config{Runner: sequenceRunner{}})
	params := map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"message":                "hello",
		"model_profile_id":       "profile-id",
		"model_profile_revision": int64(2),
		"credential_version":     int64(3),
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
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

func TestDurableStreamAcceptsTerminalResultIdentityBoundToOperation(t *testing.T) {
	const operationID = "11111111-1111-4111-8111-111111111111"
	const conversationID = "22222222-2222-4222-8222-222222222222"
	module := New(Config{Runner: authoredTurnStreamRunner{events: []agentstream.Event{{
		Event: "done", Seq: 17, Data: map[string]any{
			"idempotency_key": operationID, "conversation_id": conversationID,
			"turn_id": operationID, "revision": float64(2), "sequence": int64(17),
			"text": "done",
		},
	}}}})
	params := map[string]any{
		"idempotency_key": operationID, "conversation_id": conversationID,
		"message": "hello", "model_profile_id": "33333333-3333-4333-8333-333333333333",
		"model_profile_revision": int64(1), "credential_version": int64(1),
	}
	var got agentstream.StreamEvent
	if err := module.DurableStream(context.Background(), "@owner:example.test", "agent.chat.stream", params, func(event agentstream.StreamEvent) error {
		got = event
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got.TurnID != operationID || got.IdempotencyKey != operationID || got.ConversationID != conversationID || got.Revision != 2 || got.Turn.State != agentstream.StateSucceeded {
		t.Fatalf("terminal result projection = %#v", got)
	}
}

func (r *requestValidationRunner) Apply(context.Context, string) error { return nil }

func (r *requestValidationRunner) Invoke(_ context.Context, action string, params map[string]any) (map[string]any, error) {
	r.invokeCalls++
	r.lastAction = action
	r.lastParams = cloneMap(params)
	return map[string]any{"ok": true}, nil
}

func (r *requestValidationRunner) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	r.streamCalls++
	return nil
}

func TestTextToolsActionsAreAllowlistedAndValidatedBeforeDispatch(t *testing.T) {
	runner := &requestValidationRunner{}
	handlers := New(Config{Runner: runner}).Handlers()
	for _, action := range []string{"agent.text_tools.config.get", "agent.text_tools.config.update", "agent.text_tools.execute"} {
		if handlers[action] == nil {
			t.Fatalf("text tools action %s is not externally routed", action)
		}
	}
	if _, actionErr := handlers["agent.text_tools.execute"](context.Background(), map[string]any{
		"tool_id": "summary", "selected_text": "hello", "prompt": "forbidden",
	}); actionErr == nil || actionErr.Status != http.StatusBadRequest {
		t.Fatalf("closed execute request error = %#v", actionErr)
	}
	if runner.invokeCalls != 0 {
		t.Fatal("invalid text tools request reached the external Agent runner")
	}
	if _, actionErr := handlers["agent.text_tools.execute"](context.Background(), map[string]any{
		"tool_id": "summary", "selected_text": "hello",
	}); actionErr != nil {
		t.Fatalf("valid execute rejected: %#v", actionErr)
	}
	if runner.invokeCalls != 1 || runner.lastAction != "agent.text_tools.execute" {
		t.Fatalf("valid execute dispatch = calls %d action %q", runner.invokeCalls, runner.lastAction)
	}
}

func TestTurnStopUsesTypedMutationWithExactConcurrencyFields(t *testing.T) {
	runner := &requestValidationRunner{}
	module := New(Config{Runner: runner})
	params := map[string]any{
		"idempotency_key":   "11111111-1111-4111-8111-111111111111",
		"turn_id":           "22222222-2222-4222-8222-222222222222",
		"expected_revision": int64(3),
	}
	if _, actionErr := module.Handlers()["agent.chat.turn.stop"](context.Background(), params); actionErr != nil {
		t.Fatalf("typed turn stop failed: %v", actionErr)
	}
	if runner.invokeCalls != 1 || runner.lastAction != "agent.chat.turn.stop" || len(runner.lastParams) != 3 {
		t.Fatalf("typed turn stop dispatch = calls %d action %q params %#v", runner.invokeCalls, runner.lastAction, runner.lastParams)
	}
	for field, want := range params {
		if runner.lastParams[field] != want {
			t.Errorf("typed turn stop %s = %#v, want %#v", field, runner.lastParams[field], want)
		}
	}
}

func TestArtifactDownloadUsesStrictExternalQueryFields(t *testing.T) {
	runner := &requestValidationRunner{}
	module := New(Config{Runner: runner})
	handler := module.Handlers()["agent.execution.v2.artifacts.download"]
	if handler == nil {
		t.Fatal("artifact download handler is missing")
	}
	valid := map[string]any{
		"record_kind": "cloud_worker", "artifact_id": "9e728519-ea72-52cc-bb5a-8eb2860722b8",
		"offset_bytes": int64(0), "max_chunk_bytes": int64(512 << 10),
	}
	if _, actionErr := handler(context.Background(), valid); actionErr != nil {
		t.Fatalf("strict artifact download dispatch failed: %v", actionErr)
	}
	if runner.invokeCalls != 1 || runner.lastAction != "agent.execution.v2.artifacts.download" || len(runner.lastParams) != 4 {
		t.Fatalf("artifact download dispatch = calls %d action %q params %#v", runner.invokeCalls, runner.lastAction, runner.lastParams)
	}

	invalid := cloneMap(valid)
	invalid["max_chunk_bytes"] = int64((512 << 10) + 1)
	if _, actionErr := handler(context.Background(), invalid); actionErr == nil || actionErr.Status != http.StatusBadRequest {
		t.Fatalf("oversize artifact download = %#v, want HTTP 400", actionErr)
	}
	if runner.invokeCalls != 1 {
		t.Fatalf("invalid artifact download reached runner; calls=%d", runner.invokeCalls)
	}
}

type authoredTurnStreamRunner struct {
	events []agentstream.Event
}

func (r authoredTurnStreamRunner) Apply(context.Context, string) error { return nil }
func (r authoredTurnStreamRunner) Invoke(context.Context, string, map[string]any) (map[string]any, error) {
	return map[string]any{}, nil
}
func (r authoredTurnStreamRunner) Stream(_ context.Context, _ string, _ map[string]any, emit func(agentstream.Event) error) error {
	for _, event := range r.events {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

func TestDurableStreamUsesAgentAuthoredTurnIdentity(t *testing.T) {
	const startID = "11111111-1111-4111-8111-111111111111"
	const conversationID = "22222222-2222-4222-8222-222222222222"
	const turnID = "33333333-3333-4333-8333-333333333333"
	identity := map[string]any{
		"idempotency_key": startID, "conversation_id": conversationID,
		"turn_id": turnID, "revision": float64(2), "sequence": int64(7),
	}
	module := New(Config{Runner: authoredTurnStreamRunner{events: []agentstream.Event{{Event: "accepted", Seq: 7, Data: identity}}}})
	params := map[string]any{
		"idempotency_key": startID, "conversation_id": conversationID,
		"message": "hello", "model_profile_id": "44444444-4444-4444-8444-444444444444",
		"model_profile_revision": int64(1), "credential_version": int64(1),
	}
	var got agentstream.StreamEvent
	err := module.DurableStream(context.Background(), "@owner:example.test", "agent.chat.stream", params, func(event agentstream.StreamEvent) error {
		got = event
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.TurnID != turnID || got.IdempotencyKey != startID || got.ConversationID != conversationID || got.Revision != 2 || got.Seq != 7 {
		t.Fatalf("durable stream identity = %#v", got)
	}
	if got.Turn.TurnID != turnID || got.Turn.IdempotencyKey != startID || got.Turn.Revision != 2 {
		t.Fatalf("durable turn projection = %#v", got.Turn)
	}
}

func TestDurableStreamRejectsCorrelationIDAsTurnIdentity(t *testing.T) {
	const startID = "11111111-1111-4111-8111-111111111111"
	const conversationID = "22222222-2222-4222-8222-222222222222"
	module := New(Config{Runner: authoredTurnStreamRunner{events: []agentstream.Event{{Event: "accepted", Data: map[string]any{
		"idempotency_key": startID, "conversation_id": conversationID,
		"turn_id": "client-correlation-id", "revision": float64(1),
	}}}}})
	err := module.DurableStream(context.Background(), "@owner:example.test", "agent.chat.stream", map[string]any{
		"idempotency_key": startID, "conversation_id": conversationID,
		"message": "hello", "model_profile_id": "44444444-4444-4444-8444-444444444444",
		"model_profile_revision": int64(1), "credential_version": int64(1),
	}, func(agentstream.StreamEvent) error { return nil })
	if !errors.Is(err, agentgateway.ErrInvalidActionResult) {
		t.Fatalf("forged turn identity error = %v, want ErrInvalidActionResult", err)
	}
}

func TestChatRequestValidationRunsBeforeInvokeStreamAndDurableRetry(t *testing.T) {
	runner := &requestValidationRunner{}
	module := New(Config{Runner: runner})
	params := map[string]any{
		"idempotency_key":        "11111111-1111-4111-8111-111111111111",
		"conversation_id":        "22222222-2222-4222-8222-222222222222",
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

	if err := module.DurableStream(context.Background(), "@owner:example.test", "agent.chat.stream", cloneMap(params), func(agentstream.StreamEvent) error { return nil }); !errors.Is(err, agentgateway.ErrInvalidActionRequest) {
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
