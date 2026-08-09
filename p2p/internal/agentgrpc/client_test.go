package agentgrpc

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	agentv1 "github.com/YingSuiAI/dirextalk-agent/api/gen/dirextalk/agent/v1"
	transientmodelsdk "github.com/YingSuiAI/dirextalk-agent/sdk/transientmodel"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const testServiceKey = "svc_message.AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8"

const modelProfileCanary = "model-profile-api-key-canary"
const testCloudConnectionID = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
const testCloudTaskID = "11111111-1111-4111-8111-111111111111"
const testCloudStepID = "22222222-2222-4222-8222-222222222222"
const testCloudPlanID = "33333333-3333-4333-8333-333333333333"
const testCloudQuoteID = "44444444-4444-4444-8444-444444444444"
const testCloudDeploymentID = "55555555-5555-4555-8555-555555555555"
const testCloudWorkerID = "66666666-6666-4666-8666-666666666666"
const testApprovalID = "77777777-7777-4777-8777-777777777777"
const testApprovalKeyID = "cloud-device-0123456789abcdef01234567"

func TestRunnerChatUsesTLS13MountedAuthenticationAndBoundOwner(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})

	result, err := runner.Invoke(context.Background(), "agent.chat", map[string]any{
		"owner_id":        "attacker",
		"conversation_id": "conversation-1",
		"prompt":          "hello",
		"conversation_context": map[string]any{
			"summary":  "legacy summary that must remain on the Message Server side",
			"messages": []any{map[string]any{"role": "user", "text": "legacy message"}},
		},
		"memory_disabled":                true,
		"expected_conversation_revision": 7,
		"cloud_dialogue_mode":            false,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	request := server.service.chatRequest
	authorization := server.service.authorization
	deadlineSet := server.service.deadlineSet
	tlsVersion := server.service.tlsVersion
	server.service.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" || request.GetConversationId() != "conversation-1" ||
		request.GetMessage() != "hello" || !request.GetMemoryDisabled() || request.GetExpectedConversationRevision() != 7 {
		t.Fatalf("unexpected request mapping: %#v", request)
	}
	if request.GetCloudDialogueScope().GetCloudConnectionId() !=
		testCloudConnectionID {
		t.Fatalf(
			"ordinary chat cloud scope = %#v",
			request.GetCloudDialogueScope(),
		)
	}
	if _, err := uuid.Parse(request.GetIdempotencyKey()); err != nil {
		t.Fatalf("generated idempotency key is not a UUID: %q", request.GetIdempotencyKey())
	}
	if strings.Contains(request.String(), "legacy summary") || strings.Contains(request.String(), "legacy message") {
		t.Fatal("legacy conversation context crossed the Agent service boundary")
	}
	if authorization != "DTX-Service-Key "+testServiceKey {
		t.Fatal("mounted service key was not sent as the required authorization metadata")
	}
	if !deadlineSet || tlsVersion != 0x0304 {
		t.Fatalf("deadline=%v tls_version=%#x, want TLS 1.3 with a deadline", deadlineSet, tlsVersion)
	}
	if result["text"] != "world" || result["conversation_id"] != "conversation-1" || result["conversation_revision"] != int64(8) {
		t.Fatalf("unexpected response mapping: %#v", result)
	}
	steps, ok := result["steps"].([]map[string]any)
	if !ok || len(steps) != 1 || steps[0]["kind"] != "tool_call" || steps[0]["tool_name"] != "lookup" {
		t.Fatalf("unexpected step mapping: %#v", result["steps"])
	}
}

func TestRunnerBindsUnambiguousCloudConnectionForCentralJudgement(
	t *testing.T,
) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})
	_, err := runner.Invoke(context.Background(), "agent.chat", map[string]any{
		"idempotency_key": "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
		"conversation_id": "diagnostic-conversation",
		"prompt":          "启动一个云端 Worker 执行诊断任务",
		"memory_disabled": true,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	request := server.service.chatRequest
	server.service.mu.Unlock()
	server.cloud.mu.Lock()
	listRequests := append([]*agentv1.ListCloudConnectionsRequest(nil), server.cloud.listRequests...)
	authorization := server.cloud.authorization
	server.cloud.mu.Unlock()
	if request.GetCloudDialogueScope().GetCloudConnectionId() != testCloudConnectionID {
		t.Fatalf("cloud dialogue scope = %#v", request.GetCloudDialogueScope())
	}
	if len(listRequests) != 1 || listRequests[0].GetOwnerId() != "owner-from-config" ||
		listRequests[0].GetPageSize() != 100 || listRequests[0].GetPageToken() != "" {
		t.Fatalf("cloud connection lookup = %#v", listRequests)
	}
	if authorization != "DTX-Service-Key "+testServiceKey {
		t.Fatal("cloud lookup did not use the mounted service key")
	}

	server.cloud.mu.Lock()
	server.cloud.listRequests = nil
	server.cloud.mu.Unlock()
	_, err = runner.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "测试 Worker 安装 OpenClaw",
	})
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	request = server.service.chatRequest
	server.service.mu.Unlock()
	server.cloud.mu.Lock()
	listCount := len(server.cloud.listRequests)
	server.cloud.mu.Unlock()
	if request.GetCloudDialogueScope().GetCloudConnectionId() !=
		testCloudConnectionID ||
		listCount != 1 {
		t.Fatalf(
			"heavy task scope=%#v lookups=%d",
			request.GetCloudDialogueScope(),
			listCount,
		)
	}
}

func TestRunnerRequiresExplicitSelectionForAmbiguousCloudConnections(
	t *testing.T,
) {
	t.Parallel()
	server := startRuntimeServer(t)
	selectedConnectionID :=
		"cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	server.cloud.mu.Lock()
	server.cloud.connections = append(server.cloud.connections, &agentv1.CloudConnection{
		ConnectionId: selectedConnectionID, OwnerId: "owner-from-config", Status: "active",
	})
	server.cloud.mu.Unlock()
	runner := newTestRunner(t, server, Config{})
	_, err := runner.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "Verify the Worker control-plane diagnostic",
	})
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	request := server.service.chatRequest
	server.service.chatRequest = nil
	server.service.mu.Unlock()
	if request.GetCloudDialogueScope() != nil {
		t.Fatalf(
			"ambiguous connection was selected implicitly: %#v",
			request.GetCloudDialogueScope(),
		)
	}
	_, err = runner.Invoke(
		context.Background(),
		"agent.chat",
		map[string]any{
			"prompt":              "Implement the approved heavy task.",
			"cloud_connection_id": selectedConnectionID,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	request = server.service.chatRequest
	server.service.mu.Unlock()
	if request.GetCloudDialogueScope().GetCloudConnectionId() !=
		selectedConnectionID {
		t.Fatalf(
			"explicit cloud scope = %#v",
			request.GetCloudDialogueScope(),
		)
	}
}

func TestRunnerCloudDialogueAllowsNoConnectionAndRejectsInactiveSelection(
	t *testing.T,
) {
	t.Parallel()
	server := startRuntimeServer(t)
	server.cloud.mu.Lock()
	server.cloud.connections = nil
	server.cloud.mu.Unlock()
	runner := newTestRunner(t, server, Config{})
	if _, err := runner.Invoke(
		context.Background(),
		"agent.chat",
		map[string]any{"prompt": "hello"},
	); err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	request := server.service.chatRequest
	server.service.chatRequest = nil
	server.service.mu.Unlock()
	if request.GetCloudDialogueScope() != nil {
		t.Fatalf(
			"missing connection produced cloud scope: %#v",
			request.GetCloudDialogueScope(),
		)
	}
	_, err := runner.Invoke(
		context.Background(),
		"agent.chat",
		map[string]any{
			"prompt":              "Implement the heavy task.",
			"cloud_connection_id": "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		},
	)
	if err == nil ||
		err.Error() !=
			"selected Agent cloud connection is not active" {
		t.Fatalf("inactive connection error = %v", err)
	}
	server.service.mu.Lock()
	request = server.service.chatRequest
	server.service.mu.Unlock()
	if request != nil {
		t.Fatal("inactive connection request reached RuntimeService")
	}
}

func TestRunnerCloudTaskFacadeBindsOwnerAndRedactsInternalReferences(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})

	result, err := runner.Invoke(context.Background(), actionCloudTasksGet, map[string]any{
		"task_id": testCloudTaskID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result["task_id"] != testCloudTaskID || result["execution_status"] != "awaiting_approval" {
		t.Fatalf("unexpected task result: %#v", result)
	}
	steps, ok := result["steps"].([]map[string]any)
	if !ok || len(steps) != 1 || steps[0]["checkpoint_available"] != true || steps[0]["result_available"] != true {
		t.Fatalf("unexpected task steps: %#v", result["steps"])
	}
	dependencies, ok := steps[0]["depends_on_step_ids"].([]string)
	if !ok || dependencies == nil || len(dependencies) != 0 {
		t.Fatalf("empty task step dependencies must be encoded as []: %#v", steps[0]["depends_on_step_ids"])
	}
	if _, exposed := steps[0]["checkpoint_ref"]; exposed {
		t.Fatal("checkpoint_ref crossed the ProductCore boundary")
	}
	if _, exposed := steps[0]["result_ref"]; exposed {
		t.Fatal("result_ref crossed the ProductCore boundary")
	}

	server.tasks.mu.Lock()
	getRequest := server.tasks.getRequest
	listStepsRequest := server.tasks.listStepsRequest
	server.tasks.mu.Unlock()
	if getRequest.GetTaskId() != testCloudTaskID || listStepsRequest.GetTaskId() != testCloudTaskID {
		t.Fatalf("unexpected task RPC requests: get=%#v steps=%#v", getRequest, listStepsRequest)
	}

	cancelID := "88888888-8888-4888-8888-888888888888"
	canceled, err := runner.Invoke(context.Background(), actionCloudTasksCancel, map[string]any{
		"idempotency_key": cancelID, "task_id": testCloudTaskID, "expected_revision": 3, "reason": "user canceled",
	})
	if err != nil {
		t.Fatal(err)
	}
	if canceled["outcome_status"] != "canceled" {
		t.Fatalf("unexpected canceled task: %#v", canceled)
	}
	server.tasks.mu.Lock()
	cancelRequest := server.tasks.cancelRequest
	server.tasks.mu.Unlock()
	if cancelRequest.GetIdempotencyKey() != cancelID || cancelRequest.GetTaskId() != testCloudTaskID ||
		cancelRequest.GetExpectedRevision() != 3 || cancelRequest.GetReason() != "user canceled" {
		t.Fatalf("unexpected cancel request: %#v", cancelRequest)
	}
}

func TestRunnerCloudTaskOverviewBindsOwnerAndReturnsExactAggregate(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})

	result, err := runner.Invoke(context.Background(), actionCloudTasksOverview, map[string]any{"recent_limit": 3})
	if err != nil {
		t.Fatal(err)
	}
	if result["total_count"] != int64(1) || result["active_count"] != int64(1) ||
		result["awaiting_approval_count"] != int64(1) || result["completed_count"] != int64(0) {
		t.Fatalf("task overview = %#v", result)
	}
	counts, ok := result["status_counts"].([]map[string]any)
	if !ok || len(counts) != 1 || counts[0]["execution_status"] != "awaiting_approval" || counts[0]["count"] != int64(1) {
		t.Fatalf("task overview counts = %#v", result["status_counts"])
	}
	recent, ok := result["recent_tasks"].([]map[string]any)
	if !ok || len(recent) != 1 || recent[0]["task_id"] != testCloudTaskID {
		t.Fatalf("task overview recent = %#v", result["recent_tasks"])
	}
	server.tasks.mu.Lock()
	request := server.tasks.overviewRequest
	server.tasks.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" || request.GetRecentLimit() != 3 {
		t.Fatalf("task overview request = %#v", request)
	}
}

func TestRunnerCloudPlanApprovalUsesOpaqueDeviceSignatureAndBoundOwner(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})

	result, err := runner.Invoke(context.Background(), actionCloudPlansGet, map[string]any{"plan_id": testCloudPlanID})
	if err != nil {
		t.Fatal(err)
	}
	quote, ok := result["quote"].(map[string]any)
	if !ok || quote["currency"] != "USD" || quote["maximum_launch_amount_micros"] != uint64(32000) {
		t.Fatalf("unexpected plan quote: %#v", result["quote"])
	}
	if _, exposed := result["plan_hash"]; exposed {
		t.Fatal("plan_hash crossed the ProductCore boundary")
	}
	resource := result["resource"].(map[string]any)
	for _, field := range []string{"worker_image_id", "worker_image_digest", "vpc_id", "subnet_id", "security_group_id"} {
		if _, exposed := resource[field]; exposed {
			t.Fatalf("%s crossed the ProductCore boundary", field)
		}
	}

	challengeRequestID := "99999999-9999-4999-8999-999999999999"
	challenge, err := runner.Invoke(context.Background(), actionCloudPlanConfirmation, map[string]any{
		"idempotency_key": challengeRequestID, "plan_id": testCloudPlanID,
		"expected_revision": 1, "signer_key_id": testApprovalKeyID,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(challenge["signing_payload_base64url"].(string))
	if err != nil || string(payload) != "opaque-canonical-cbor" {
		t.Fatalf("unexpected signing payload: %#v, %v", challenge, err)
	}
	server.cloud.mu.Lock()
	prepareRequest := server.cloud.challengeRequest
	server.cloud.mu.Unlock()
	if prepareRequest.GetOwnerId() != "owner-from-config" || prepareRequest.GetPlanId() != testCloudPlanID ||
		prepareRequest.GetSignerKeyId() != testApprovalKeyID {
		t.Fatalf("unexpected challenge request: %#v", prepareRequest)
	}

	signature := base64.RawURLEncoding.EncodeToString(make([]byte, 64))
	approved, err := runner.Invoke(context.Background(), actionCloudPlanApprove, map[string]any{
		"idempotency_key":   "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		"plan_id":           testCloudPlanID,
		"expected_revision": 1,
		"approval": map[string]any{
			"approval_id": testApprovalID, "challenge_id": "challenge_" + strings.Repeat("A", 43),
			"signer_key_id": testApprovalKeyID, "expires_at": time.Now().Add(time.Minute).UTC().Format(time.RFC3339Nano),
			"signature_base64url": signature,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if approved["status"] != "approved" || approved["revision"] != int64(2) {
		t.Fatalf("unexpected approved plan: %#v", approved)
	}
	server.cloud.mu.Lock()
	approveRequest := server.cloud.approveRequest
	server.cloud.mu.Unlock()
	if approveRequest.GetOwnerId() != "owner-from-config" ||
		approveRequest.GetApproval().GetSignerKeyId() != testApprovalKeyID ||
		len(approveRequest.GetApproval().GetSignature()) != 64 {
		t.Fatalf("unexpected approve request: %#v", approveRequest)
	}
}

func TestRunnerCloudMutationsFailBeforeRPCOnUnboundInput(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})

	for _, test := range []struct {
		action string
		params map[string]any
	}{
		{actionCloudTasksCancel, map[string]any{
			"idempotency_key": uuid.NewString(), "task_id": testCloudTaskID,
			"expected_revision": 3, "owner_id": "attacker",
		}},
		{actionCloudPlanConfirmation, map[string]any{
			"idempotency_key": uuid.NewString(), "plan_id": testCloudPlanID,
			"expected_revision": 1, "signer_key_id": "device-alias",
		}},
		{actionCloudPlanApprove, map[string]any{
			"idempotency_key": uuid.NewString(), "plan_id": testCloudPlanID, "expected_revision": 1,
			"approval": map[string]any{
				"approval_id": testApprovalID, "challenge_id": "challenge_" + strings.Repeat("A", 43),
				"signer_key_id": testApprovalKeyID, "expires_at": time.Now().UTC().Format(time.RFC3339Nano),
				"signature_base64url": base64.RawURLEncoding.EncodeToString(make([]byte, 63)),
			},
		}},
	} {
		if _, err := runner.Invoke(context.Background(), test.action, test.params); err == nil {
			t.Fatalf("%s accepted invalid parameters", test.action)
		}
	}
	server.tasks.mu.Lock()
	cancelRequest := server.tasks.cancelRequest
	server.tasks.mu.Unlock()
	server.cloud.mu.Lock()
	challengeRequest := server.cloud.challengeRequest
	approveRequest := server.cloud.approveRequest
	server.cloud.mu.Unlock()
	if cancelRequest != nil || challengeRequest != nil || approveRequest != nil {
		t.Fatalf("invalid mutation reached Agent: cancel=%#v challenge=%#v approve=%#v", cancelRequest, challengeRequest, approveRequest)
	}
}

func TestRunnerCloudDeploymentAndWorkerReadsExposeOnlyPublicState(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{UnaryTimeout: time.Second})

	deployment, err := runner.Invoke(context.Background(), actionCloudDeploymentsGet, map[string]any{
		"deployment_id": testCloudDeploymentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	resources := deployment["resources"].(map[string]any)
	if resources["status"] != "active" || resources["existing"] != uint32(1) {
		t.Fatalf("unexpected deployment resources: %#v", resources)
	}
	if _, exposed := resources["provider_id"]; exposed {
		t.Fatal("provider_id crossed the ProductCore boundary")
	}

	worker, err := runner.Invoke(context.Background(), actionCloudWorkersGet, map[string]any{
		"deployment_id": testCloudDeploymentID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if worker["status"] != "leased" || worker["result_available"] != false || worker["evidence_count"] != uint32(2) {
		t.Fatalf("unexpected worker: %#v", worker)
	}
}

func TestRunnerStreamMapsEventsAndPropagatesCancellation(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{StreamTimeout: time.Second})
	var events []nativeagent.Event
	if err := runner.Stream(context.Background(), "agent.chat.stream", map[string]any{"prompt": "stream"}, func(event nativeagent.Event) error {
		events = append(events, event)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[0].Event != "delta" || events[1].Event != "tool" || events[2].Event != "done" {
		t.Fatalf("unexpected stream events: %#v", events)
	}
	if events[0].Data["text"] != "hel" || events[2].Data["text"] != "world" {
		t.Fatalf("unexpected stream data: %#v", events)
	}

	ctx, cancel := context.WithCancel(context.Background())
	server.service.cancelStarted = make(chan struct{})
	server.service.cancelObserved = make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- runner.Stream(ctx, "agent.chat.stream", map[string]any{"prompt": "cancel"}, func(nativeagent.Event) error { return nil })
	}()
	<-server.service.cancelStarted
	cancel()
	if err := <-done; err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancellation error = %v", err)
	}
	select {
	case <-server.service.cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("server did not observe stream cancellation")
	}
}

func TestRunnerRedactsServiceErrorsAndSecrets(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{})
	_, err := runner.Invoke(context.Background(), "agent.chat", map[string]any{
		"prompt": "fail",
	})
	if err == nil {
		t.Fatal("expected service failure")
	}
	for _, forbidden := range []string{"database-password", testServiceKey, "internal stack", modelProfileCanary} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error leaked sensitive detail %q: %v", forbidden, err)
		}
	}
	if err.Error() != "agent service request failed (internal)" {
		t.Fatalf("unexpected sanitized error: %v", err)
	}
}

func TestRunnerFailsClosedForUnrepresentableLegacyParameters(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{})
	for _, params := range []map[string]any{
		{"prompt": "hello", "cloud_dialogue_mode": true},
		{"prompt": "hello", "knowledge_enabled": true},
		{"prompt": "hello", "embedding_profile": map[string]any{"provider": "openai"}},
		{"prompt": "hello", "attachments": []any{map[string]any{"name": "photo.png"}}},
		{"prompt": "hello", "cloud_recipe_id": "recipe-1"},
		{"prompt": "hello", "cloud_recipe_revision": 1},
		{"messages": []any{map[string]any{"role": "user", "content": "hello"}}},
		{"prompt": "hello", "system_prompt": "override"},
		{"prompt": "hello", "enabled_tools": []any{"all"}},
	} {
		_, err := runner.Invoke(context.Background(), "agent.chat", params)
		if err == nil || err.Error() != "agent chat parameters cannot be represented by the remote runtime contract" {
			t.Fatalf("fail-closed error = %v", err)
		}
	}
	server.service.mu.Lock()
	request := server.service.chatRequest
	server.service.mu.Unlock()
	if request != nil {
		t.Fatal("unrepresentable parameters reached the remote Agent service")
	}
}

func TestRunnerEncryptsClientModelCredentialOutsideRuntimeChatRPC(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{})
	requestID := "7d2f90ad-9b9a-4688-baa4-5f96b04b5efe"
	apiKey := modelProfileCanary + "-never-in-runtime-rpc"
	_, err := runner.Invoke(context.Background(), "agent.chat", map[string]any{
		"idempotency_key":  requestID,
		"prompt":           "hello",
		"model_profile_id": "deepseek:deepseek-chat",
		"api_key":          apiKey,
		"model_profile": map[string]any{
			"id": "deepseek:deepseek-chat", "provider": "deepseek", "model": "deepseek-chat",
			"base_url": "https://api.deepseek.com", "api_key": apiKey,
			"context_window": 64, "max_output_tokens": 4096, "temperature": 0.3,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	runtimeRequest := proto.Clone(server.service.chatRequest).(*agentv1.ChatRequest)
	server.service.mu.Unlock()
	server.secrets.mu.Lock()
	createRequest := proto.Clone(server.secrets.createRequest).(*agentv1.CreateSessionRequest)
	uploadRequest := proto.Clone(server.secrets.uploadRequest).(*agentv1.UploadEncryptedRequest)
	server.secrets.mu.Unlock()
	if runtimeRequest.GetTransientModel() == nil || runtimeRequest.GetTransientModel().GetProfile().GetModel() != "deepseek-chat" ||
		runtimeRequest.GetTransientModel().GetCredentialSessionRevision() != 2 || len(runtimeRequest.GetTransientModel().GetCredentialSha256()) != sha256.Size {
		t.Fatalf("transient model request = %#v", runtimeRequest.GetTransientModel())
	}
	for _, encoded := range []string{runtimeRequest.String(), createRequest.String(), uploadRequest.String()} {
		if strings.Contains(encoded, apiKey) {
			t.Fatal("model API key crossed an ordinary RPC or protobuf string surface")
		}
	}
	if bytes.Contains(uploadRequest.GetCiphertext(), []byte(apiKey)) || len(uploadRequest.GetCiphertext()) <= len(apiKey) {
		t.Fatal("secret bootstrap upload did not contain opaque authenticated ciphertext")
	}
	binding, err := transientmodelsdk.ProfileFromProto(runtimeRequest.GetTransientModel().GetProfile())
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(apiKey))
	expectedTarget, err := transientmodelsdk.TargetID("owner-from-config", requestID, binding, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	if createRequest.GetOwnerId() != "owner-from-config" || createRequest.GetPurpose() != transientmodelsdk.CredentialPurpose || createRequest.GetTargetId() != expectedTarget {
		t.Fatalf("bootstrap binding = %#v", createRequest)
	}
}

func TestRunnerRejectsUnsupportedTransientModelProfileFields(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{})
	_, err := runner.Invoke(context.Background(), "agent.chat", map[string]any{
		"idempotency_key":  "ae021176-5af7-4a20-8b54-6d425a38bc20",
		"prompt":           "hello",
		"model_profile_id": "deepseek:deepseek-chat",
		"api_key":          "test-key",
		"model_profile": map[string]any{
			"id": "deepseek:deepseek-chat", "provider": "deepseek", "model": "deepseek-chat",
			"base_url": "https://api.deepseek.com", "api_key": "test-key", "top_k": 40,
		},
	})
	if err == nil || err.Error() != "invalid agent chat parameters: model profile is invalid" {
		t.Fatalf("unsupported profile field error = %v", err)
	}
	server.service.mu.Lock()
	defer server.service.mu.Unlock()
	if server.service.chatRequest != nil {
		t.Fatal("invalid transient profile reached RuntimeService")
	}
}

func TestRunnerRejectsMalformedDeepSeekCredentialBeforeBootstrap(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{})
	apiKey := modelProfileCanary + "`x"
	_, err := runner.Invoke(context.Background(), "agent.chat", map[string]any{
		"idempotency_key":  "89caf6bd-7f44-4a04-a768-fc0292268aa9",
		"prompt":           "hello",
		"model_profile_id": "deepseek:deepseek-chat",
		"api_key":          apiKey,
		"model_profile": map[string]any{
			"id": "deepseek:deepseek-chat", "provider": "deepseek", "model": "deepseek-chat",
			"base_url": "https://api.deepseek.com", "api_key": apiKey,
		},
	})
	var coded interface{ ErrorCode() string }
	if err == nil ||
		err.Error() != "DeepSeek API key contains unsupported characters." ||
		!errors.As(err, &coded) ||
		coded.ErrorCode() != "M_AGENT_MODEL_CREDENTIAL_INVALID" {
		t.Fatalf("malformed credential error = %v", err)
	}
	server.secrets.mu.Lock()
	defer server.secrets.mu.Unlock()
	if server.secrets.createRequest != nil || server.secrets.uploadRequest != nil {
		t.Fatal("malformed credential reached SecretBootstrap")
	}
}

func TestRunnerListsModelsThroughEncryptedTransientCredential(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{})
	apiKey := modelProfileCanary + "-model-list-only"
	result, err := runner.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "deepseek", "base_url": "https://api.deepseek.com", "api_key": apiKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.service.mu.Lock()
	runtimeRequest := proto.Clone(server.service.listModelsRequest).(*agentv1.ListModelsRequest)
	server.service.mu.Unlock()
	server.secrets.mu.Lock()
	createRequest := proto.Clone(server.secrets.createRequest).(*agentv1.CreateSessionRequest)
	uploadRequest := proto.Clone(server.secrets.uploadRequest).(*agentv1.UploadEncryptedRequest)
	server.secrets.mu.Unlock()
	if _, err := uuid.Parse(runtimeRequest.GetRequestId()); err != nil || runtimeRequest.GetOwnerId() != "owner-from-config" {
		t.Fatalf("model list request = %#v", runtimeRequest)
	}
	transient := runtimeRequest.GetTransientModel()
	if transient == nil || transient.GetProfile().GetModel() != "model-discovery" ||
		transient.GetProfile().GetProvider() != agentv1.ModelProvider_MODEL_PROVIDER_DEEPSEEK ||
		transient.GetCredentialSessionRevision() != 2 || len(transient.GetCredentialSha256()) != sha256.Size {
		t.Fatalf("transient discovery binding = %#v", transient)
	}
	for _, encoded := range []string{runtimeRequest.String(), createRequest.String(), uploadRequest.String()} {
		if strings.Contains(encoded, apiKey) {
			t.Fatal("model discovery API key crossed an ordinary protobuf surface")
		}
	}
	if bytes.Contains(uploadRequest.GetCiphertext(), []byte(apiKey)) {
		t.Fatal("model discovery credential was not encrypted")
	}
	models, ok := result["models"].([]map[string]any)
	if !ok || len(models) != 1 || models[0]["id"] != "deepseek-chat" || models[0]["provider"] != "deepseek" || models[0]["context_window"] != int64(65536) {
		t.Fatalf("model list result = %#v", result)
	}
	server.cloud.mu.Lock()
	defer server.cloud.mu.Unlock()
	if len(server.cloud.listRequests) != 0 {
		t.Fatal("model discovery unexpectedly performed a cloud connection lookup")
	}
}

func TestRunnerRejectsUnsupportedModelDiscoveryParametersBeforeBootstrap(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{})
	_, err := runner.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "deepseek", "api_key": "test-key", "secret_ref": "mounted:forbidden",
	})
	if err == nil || err.Error() != "invalid agent model discovery parameters" {
		t.Fatalf("model discovery error = %v", err)
	}
	server.secrets.mu.Lock()
	defer server.secrets.mu.Unlock()
	if server.secrets.createRequest != nil || server.secrets.uploadRequest != nil {
		t.Fatal("invalid model discovery request reached SecretBootstrap")
	}
}

func TestRunnerRejectsMalformedModelDiscoveryCredentialBeforeBootstrap(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{})
	_, err := runner.Invoke(context.Background(), "agent.models.list", map[string]any{
		"provider": "deepseek", "api_key": modelProfileCanary + "`x",
	})
	var coded interface{ ErrorCode() string }
	if err == nil ||
		err.Error() != "DeepSeek API key contains unsupported characters." ||
		!errors.As(err, &coded) ||
		coded.ErrorCode() != "M_AGENT_MODEL_CREDENTIAL_INVALID" {
		t.Fatalf("model discovery error = %v", err)
	}
	server.secrets.mu.Lock()
	defer server.secrets.mu.Unlock()
	if server.secrets.createRequest != nil || server.secrets.uploadRequest != nil {
		t.Fatal("malformed model discovery credential reached SecretBootstrap")
	}
}

func TestRunnerStreamRequiresOneTerminalDoneEvent(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	runner := newTestRunner(t, server, Config{StreamTimeout: time.Second})

	for _, test := range []struct {
		name    string
		message string
	}{
		{name: "EOF before done", message: "stream-eof-before-done"},
		{name: "delta after done", message: "stream-after-done"},
		{name: "duplicate done", message: "stream-duplicate-done"},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []nativeagent.Event
			err := runner.Stream(context.Background(), "agent.chat.stream", map[string]any{"prompt": test.message}, func(event nativeagent.Event) error {
				events = append(events, event)
				return nil
			})
			if err == nil || err.Error() != "agent service returned an invalid stream sequence" {
				t.Fatalf("stream error = %v", err)
			}
			for _, event := range events {
				if event.Event == "done" {
					t.Fatalf("invalid stream emitted terminal success: %#v", events)
				}
			}
		})
	}
}

func TestSanitizeRPCErrorClassifiesConversationRevisionConflict(t *testing.T) {
	err := sanitizeRPCError(
		context.Background(),
		status.Error(codes.Aborted, "expected revision or lease does not match"),
	)
	coded, ok := err.(interface{ ErrorCode() string })
	if !ok || coded.ErrorCode() != "M_AGENT_CONVERSATION_OUT_OF_DATE" {
		t.Fatalf("revision conflict = %T %v", err, err)
	}
	if err.Error() != "Agent conversation changed; please retry" {
		t.Fatalf("public revision conflict message = %q", err.Error())
	}
}

func TestSanitizeRPCErrorClassifiesModelFailures(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		grpcCode    codes.Code
		grpcMessage string
		wantCode    string
	}{
		{name: "credential", grpcCode: codes.PermissionDenied, grpcMessage: "model provider rejected the supplied credential", wantCode: "M_AGENT_MODEL_CREDENTIAL_REJECTED"},
		{name: "request", grpcCode: codes.FailedPrecondition, grpcMessage: "model provider rejected the selected model or request", wantCode: "M_AGENT_MODEL_REQUEST_REJECTED"},
		{name: "rate limited", grpcCode: codes.ResourceExhausted, grpcMessage: "model provider rate limit is temporarily exhausted", wantCode: "M_AGENT_MODEL_RATE_LIMITED"},
		{name: "unavailable", grpcCode: codes.Unavailable, grpcMessage: "model provider is unavailable", wantCode: "M_AGENT_MODEL_UNAVAILABLE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := sanitizeRPCError(context.Background(), status.Error(test.grpcCode, test.grpcMessage))
			coded, ok := err.(interface{ ErrorCode() string })
			if !ok || coded.ErrorCode() != test.wantCode {
				t.Fatalf("model failure = %T %v, want code %s", err, err, test.wantCode)
			}
		})
	}
}

func TestRunnerEnforcesConfiguredMessageLimits(t *testing.T) {
	t.Parallel()
	server := startRuntimeServer(t)
	receiveLimited := newTestRunner(t, server, Config{MaxReceiveBytes: 128})
	if _, err := receiveLimited.Invoke(context.Background(), "agent.chat", map[string]any{"prompt": "large-response"}); err == nil || err.Error() != "agent service request failed (resourceexhausted)" {
		t.Fatalf("receive limit error = %v", err)
	}
	sendLimited := newTestRunner(t, server, Config{MaxSendBytes: 128})
	if _, err := sendLimited.Invoke(context.Background(), "agent.chat", map[string]any{"prompt": strings.Repeat("x", 1024)}); err == nil || err.Error() != "agent service request failed (resourceexhausted)" {
		t.Fatalf("send limit error = %v", err)
	}
}

func TestNewFailsClosedForInvalidSecurityConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "missing target", cfg: Config{CAFile: "ca", ServerName: "agent.test", ServiceKeyFile: "key", OwnerID: "owner"}},
		{name: "missing ca", cfg: Config{Target: "agent:443", ServerName: "agent.test", ServiceKeyFile: "key", OwnerID: "owner"}},
		{name: "missing server name", cfg: Config{Target: "agent:443", CAFile: "ca", ServiceKeyFile: "key", OwnerID: "owner"}},
		{name: "missing key", cfg: Config{Target: "agent:443", CAFile: "ca", ServerName: "agent.test", OwnerID: "owner"}},
		{name: "missing owner", cfg: Config{Target: "agent:443", CAFile: "ca", ServerName: "agent.test", ServiceKeyFile: "key"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(context.Background(), test.cfg); err == nil {
				t.Fatal("expected fail-closed configuration error")
			}
		})
	}
}

type taskTestService struct {
	agentv1.UnimplementedTaskServiceServer
	mu               sync.Mutex
	task             *agentv1.Task
	step             *agentv1.Step
	getRequest       *agentv1.GetTaskRequest
	overviewRequest  *agentv1.GetTaskOverviewRequest
	listStepsRequest *agentv1.ListStepsRequest
	cancelRequest    *agentv1.CancelTaskRequest
	events           []*agentv1.Event
}

func (service *taskTestService) WatchEvents(
	request *agentv1.WatchEventsRequest,
	stream agentv1.TaskService_WatchEventsServer,
) error {
	service.mu.Lock()
	events := append([]*agentv1.Event(nil), service.events...)
	service.mu.Unlock()
	for _, event := range events {
		if event.GetSeq() <= request.GetAfterSeq() {
			continue
		}
		if err := stream.Send(&agentv1.WatchEventsResponse{
			Event: proto.Clone(event).(*agentv1.Event),
		}); err != nil {
			return err
		}
	}
	<-stream.Context().Done()
	return stream.Context().Err()
}

func (service *taskTestService) ListTasks(_ context.Context, request *agentv1.ListTasksRequest) (*agentv1.ListTasksResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" {
		return nil, status.Error(codes.PermissionDenied, "wrong owner")
	}
	return &agentv1.ListTasksResponse{Tasks: []*agentv1.Task{proto.Clone(service.task).(*agentv1.Task)}}, nil
}

func (service *taskTestService) GetTask(_ context.Context, request *agentv1.GetTaskRequest) (*agentv1.GetTaskResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.getRequest = proto.Clone(request).(*agentv1.GetTaskRequest)
	return &agentv1.GetTaskResponse{Task: proto.Clone(service.task).(*agentv1.Task)}, nil
}

func (service *taskTestService) GetTaskOverview(_ context.Context, request *agentv1.GetTaskOverviewRequest) (*agentv1.GetTaskOverviewResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.overviewRequest = proto.Clone(request).(*agentv1.GetTaskOverviewRequest)
	return &agentv1.GetTaskOverviewResponse{
		TotalCount: 1,
		StatusCounts: []*agentv1.TaskStatusCount{{
			ExecutionStatus: service.task.GetExecutionStatus(),
			OutcomeStatus:   service.task.GetOutcomeStatus(), Count: 1,
		}},
		RecentTasks: []*agentv1.Task{proto.Clone(service.task).(*agentv1.Task)},
		AsOf:        timestamppb.New(service.task.GetUpdatedAt().AsTime().Add(time.Second)),
	}, nil
}

func (service *taskTestService) ListSteps(_ context.Context, request *agentv1.ListStepsRequest) (*agentv1.ListStepsResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.listStepsRequest = proto.Clone(request).(*agentv1.ListStepsRequest)
	return &agentv1.ListStepsResponse{Steps: []*agentv1.Step{proto.Clone(service.step).(*agentv1.Step)}}, nil
}

func (service *taskTestService) CancelTask(_ context.Context, request *agentv1.CancelTaskRequest) (*agentv1.CancelTaskResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.cancelRequest = proto.Clone(request).(*agentv1.CancelTaskRequest)
	task := proto.Clone(service.task).(*agentv1.Task)
	task.ExecutionStatus = agentv1.ExecutionStatus_EXECUTION_STATUS_FINISHED
	task.OutcomeStatus = agentv1.OutcomeStatus_OUTCOME_STATUS_CANCELED
	task.Revision++
	task.UpdatedAt = timestamppb.Now()
	return &agentv1.CancelTaskResponse{Task: task}, nil
}

type cloudTestService struct {
	agentv1.UnimplementedCloudControlServiceServer
	mu               sync.Mutex
	connections      []*agentv1.CloudConnection
	plan             *agentv1.CloudPlan
	quote            *agentv1.CloudQuote
	deployment       *agentv1.CloudDeployment
	worker           *agentv1.CloudWorker
	listRequests     []*agentv1.ListCloudConnectionsRequest
	challengeRequest *agentv1.CreateApprovalChallengeRequest
	approveRequest   *agentv1.ApproveCloudPlanRequest
	authorization    string
}

func (service *cloudTestService) ListCloudConnections(ctx context.Context, request *agentv1.ListCloudConnectionsRequest) (*agentv1.ListCloudConnectionsResponse, error) {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	authorization := ""
	if len(values) == 1 {
		authorization = values[0]
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.listRequests = append(service.listRequests, request)
	service.authorization = authorization
	return &agentv1.ListCloudConnectionsResponse{
		Connections: append([]*agentv1.CloudConnection(nil), service.connections...),
	}, nil
}

func (service *cloudTestService) ListCloudPlans(_ context.Context, request *agentv1.ListCloudPlansRequest) (*agentv1.ListCloudPlansResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" {
		return nil, status.Error(codes.PermissionDenied, "wrong owner")
	}
	return &agentv1.ListCloudPlansResponse{Plans: []*agentv1.CloudPlan{proto.Clone(service.plan).(*agentv1.CloudPlan)}}, nil
}

func (service *cloudTestService) GetCloudPlan(_ context.Context, request *agentv1.GetCloudPlanRequest) (*agentv1.GetCloudPlanResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" || request.GetPlanId() != testCloudPlanID {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &agentv1.GetCloudPlanResponse{Plan: proto.Clone(service.plan).(*agentv1.CloudPlan)}, nil
}

func (service *cloudTestService) GetCloudQuote(_ context.Context, request *agentv1.GetCloudQuoteRequest) (*agentv1.GetCloudQuoteResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" || request.GetQuoteId() != testCloudQuoteID {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &agentv1.GetCloudQuoteResponse{Quote: proto.Clone(service.quote).(*agentv1.CloudQuote)}, nil
}

func (service *cloudTestService) CreateApprovalChallenge(_ context.Context, request *agentv1.CreateApprovalChallengeRequest) (*agentv1.CreateApprovalChallengeResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.challengeRequest = proto.Clone(request).(*agentv1.CreateApprovalChallengeRequest)
	return &agentv1.CreateApprovalChallengeResponse{Challenge: &agentv1.ApprovalChallenge{
		ApprovalId: testApprovalID, ChallengeId: "challenge_" + strings.Repeat("A", 43),
		SignerKeyId: request.GetSignerKeyId(), PlanId: request.GetPlanId(),
		PlanRevision: request.GetExpectedRevision(), OwnerId: request.GetOwnerId(),
		ExpiresAt: timestamppb.New(time.Now().Add(time.Minute)), SigningPayloadCbor: []byte("opaque-canonical-cbor"),
		Revision: 1,
	}}, nil
}

func (service *cloudTestService) ApproveCloudPlan(_ context.Context, request *agentv1.ApproveCloudPlanRequest) (*agentv1.ApproveCloudPlanResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.approveRequest = proto.Clone(request).(*agentv1.ApproveCloudPlanRequest)
	plan := proto.Clone(service.plan).(*agentv1.CloudPlan)
	plan.Status = agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_APPROVED
	plan.Revision++
	return &agentv1.ApproveCloudPlanResponse{Plan: plan}, nil
}

func (service *cloudTestService) ListCloudDeployments(_ context.Context, request *agentv1.ListCloudDeploymentsRequest) (*agentv1.ListCloudDeploymentsResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" {
		return nil, status.Error(codes.PermissionDenied, "wrong owner")
	}
	return &agentv1.ListCloudDeploymentsResponse{Deployments: []*agentv1.CloudDeployment{proto.Clone(service.deployment).(*agentv1.CloudDeployment)}}, nil
}

func (service *cloudTestService) GetCloudDeployment(_ context.Context, request *agentv1.GetCloudDeploymentRequest) (*agentv1.GetCloudDeploymentResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" || request.GetDeploymentId() != testCloudDeploymentID {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &agentv1.GetCloudDeploymentResponse{Deployment: proto.Clone(service.deployment).(*agentv1.CloudDeployment)}, nil
}

func (service *cloudTestService) ListCloudWorkers(_ context.Context, request *agentv1.ListCloudWorkersRequest) (*agentv1.ListCloudWorkersResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" {
		return nil, status.Error(codes.PermissionDenied, "wrong owner")
	}
	return &agentv1.ListCloudWorkersResponse{Workers: []*agentv1.CloudWorker{proto.Clone(service.worker).(*agentv1.CloudWorker)}}, nil
}

func (service *cloudTestService) GetCloudWorker(_ context.Context, request *agentv1.GetCloudWorkerRequest) (*agentv1.GetCloudWorkerResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	if request.GetOwnerId() != "owner-from-config" || request.GetDeploymentId() != testCloudDeploymentID {
		return nil, status.Error(codes.NotFound, "not found")
	}
	return &agentv1.GetCloudWorkerResponse{Worker: proto.Clone(service.worker).(*agentv1.CloudWorker)}, nil
}

type runtimeTestService struct {
	agentv1.UnimplementedRuntimeServiceServer
	mu                   sync.Mutex
	chatRequest          *agentv1.ChatRequest
	listModelsRequest    *agentv1.ListModelsRequest
	runtimeConfigRequest *agentv1.GetRuntimeConfigRequest
	putRuntimeRequest    *agentv1.PutRuntimeConfigRequest
	synthesisOwnerID     string
	synthesisSourceID    string
	conversationOwnerID  string
	conversationID       string
	synthesisResponse    *agentv1.SynthesizeTeamCompletionResponse
	getCapabilities      func(*agentv1.RuntimeServiceGetCapabilitiesRequest) (*agentv1.RuntimeServiceGetCapabilitiesResponse, error)
	getRuntimeConfig     func(*agentv1.GetRuntimeConfigRequest) (*agentv1.GetRuntimeConfigResponse, error)
	putRuntimeConfig     func(*agentv1.PutRuntimeConfigRequest) (*agentv1.PutRuntimeConfigResponse, error)
	authorization        string
	deadlineSet          bool
	tlsVersion           uint16
	cancelStarted        chan struct{}
	cancelObserved       chan struct{}
}

func (service *runtimeTestService) Chat(ctx context.Context, request *agentv1.ChatRequest) (*agentv1.ChatResponse, error) {
	if request.GetMessage() == "fail" {
		return nil, status.Error(codes.Internal, "database-password internal stack")
	}
	if request.GetMessage() == "large-response" {
		response := chatResponse()
		response.Message.Content = strings.Repeat("x", 1024)
		return response, nil
	}
	service.capture(ctx, request)
	return chatResponse(), nil
}

func (service *runtimeTestService) SynthesizeTeamCompletion(
	_ context.Context,
	request *agentv1.SynthesizeTeamCompletionRequest,
) (*agentv1.SynthesizeTeamCompletionResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.synthesisOwnerID = request.GetOwnerId()
	service.synthesisSourceID = request.GetSourceEventId()
	if service.synthesisResponse != nil {
		return proto.Clone(service.synthesisResponse).(*agentv1.SynthesizeTeamCompletionResponse), nil
	}
	return &agentv1.SynthesizeTeamCompletionResponse{
		SourceEventId:  request.GetSourceEventId(),
		ConversationId: "agent-chat-11111111-2222-4333-8444-555555555555",
		Message: &agentv1.RuntimeAssistantMessage{
			MessageId: "abcdefab-cdef-4abc-8def-abcdefabcdef",
			Content:   "The Team completed the requested work.",
		},
		ConversationRevision: 31,
	}, nil
}

func (service *runtimeTestService) GetConversationState(
	_ context.Context,
	request *agentv1.GetConversationStateRequest,
) (*agentv1.GetConversationStateResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.conversationOwnerID = request.GetOwnerId()
	service.conversationID = request.GetConversationId()
	return &agentv1.GetConversationStateResponse{
		Found:                true,
		ConversationRevision: 29,
	}, nil
}

func (service *runtimeTestService) ListModels(_ context.Context, request *agentv1.ListModelsRequest) (*agentv1.ListModelsResponse, error) {
	service.mu.Lock()
	service.listModelsRequest = proto.Clone(request).(*agentv1.ListModelsRequest)
	service.mu.Unlock()
	return &agentv1.ListModelsResponse{Models: []*agentv1.ModelDescriptor{{
		Id: "deepseek-chat", Name: "DeepSeek Chat", Provider: "deepseek",
		ContextWindow: 65536, MaxOutputTokens: 8192, ReasoningModes: []string{"standard"},
	}}}, nil
}

func (service *runtimeTestService) StreamChat(request *agentv1.StreamChatRequest, stream grpc.ServerStreamingServer[agentv1.StreamChatResponse]) error {
	if request.GetMessage() == "cancel" {
		close(service.cancelStarted)
		<-stream.Context().Done()
		close(service.cancelObserved)
		return stream.Context().Err()
	}
	responses := []*agentv1.StreamChatResponse{
		{Event: &agentv1.StreamChatResponse_Delta{Delta: &agentv1.ChatDelta{MessageId: "message-1", Content: "hel"}}},
		{Event: &agentv1.StreamChatResponse_Tool{Tool: &agentv1.ToolExecutionSummary{ToolCallId: "call-1", ToolName: "lookup", Finished: true}}},
		{Event: &agentv1.StreamChatResponse_Done{Done: &agentv1.ChatDone{Response: chatResponse()}}},
	}
	switch request.GetMessage() {
	case "stream-eof-before-done":
		responses = responses[:1]
	case "stream-after-done":
		responses = []*agentv1.StreamChatResponse{responses[2], responses[0]}
	case "stream-duplicate-done":
		responses = []*agentv1.StreamChatResponse{responses[2], responses[2]}
	}
	for _, response := range responses {
		if err := stream.Send(response); err != nil {
			return err
		}
	}
	return nil
}

func (service *runtimeTestService) capture(ctx context.Context, request *agentv1.ChatRequest) {
	values := metadata.ValueFromIncomingContext(ctx, "authorization")
	authorization := ""
	if len(values) == 1 {
		authorization = values[0]
	}
	_, deadlineSet := ctx.Deadline()
	tlsVersion := uint16(0)
	if peerInfo, ok := peer.FromContext(ctx); ok {
		if tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo); ok {
			tlsVersion = tlsInfo.State.Version
		}
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	service.chatRequest = request
	service.authorization = authorization
	service.deadlineSet = deadlineSet
	service.tlsVersion = tlsVersion
}

func chatResponse() *agentv1.ChatResponse {
	return &agentv1.ChatResponse{
		ConversationId:       "conversation-1",
		Message:              &agentv1.RuntimeAssistantMessage{MessageId: "message-1", Content: "world"},
		ConversationRevision: 8,
		Steps:                []*agentv1.RuntimeStepSummary{{Kind: agentv1.RuntimeStepKind_RUNTIME_STEP_KIND_TOOL_CALL, ToolCallId: "call-1", ToolName: "lookup"}},
		RelatedTaskIds:       []string{"task-1"}, RelatedPlanIds: []string{"plan-1"},
	}
}

func newTaskTestService() *taskTestService {
	now := time.Date(2026, time.July, 29, 3, 0, 0, 0, time.UTC)
	return &taskTestService{
		task: &agentv1.Task{
			TaskId: testCloudTaskID, OwnerId: "owner-from-config", Goal: "Run the Worker diagnostic",
			ExecutionStatus: agentv1.ExecutionStatus_EXECUTION_STATUS_AWAITING_APPROVAL,
			OutcomeStatus:   agentv1.OutcomeStatus_OUTCOME_STATUS_PENDING,
			RetentionPolicy: agentv1.RetentionPolicy_RETENTION_POLICY_EPHEMERAL_AUTO_DESTROY,
			CurrentStepId:   testCloudStepID, ApprovedPlanId: testCloudPlanID, Revision: 3,
			CreatedAt: timestamppb.New(now), UpdatedAt: timestamppb.New(now.Add(time.Minute)),
		},
		step: &agentv1.Step{
			StepId: testCloudStepID, TaskId: testCloudTaskID, Name: "prepare_resource_candidates",
			ExecutorKind:    agentv1.ExecutorKind_EXECUTOR_KIND_CONTROL_PLANE,
			ExecutionStatus: agentv1.ExecutionStatus_EXECUTION_STATUS_FINISHED,
			OutcomeStatus:   agentv1.OutcomeStatus_OUTCOME_STATUS_SUCCEEDED,
			Attempt:         1, LeaseEpoch: 2, CheckpointRef: "s3://private/checkpoint",
			ResultRef: "postgres://private/result", Revision: 4,
			CreatedAt: timestamppb.New(now), UpdatedAt: timestamppb.New(now.Add(time.Minute)),
		},
	}
}

func newCloudTestService() *cloudTestService {
	now := time.Date(2026, time.July, 29, 3, 0, 0, 0, time.UTC)
	scopeDigest := "sha256:" + strings.Repeat("a", 64)
	quoteDigest := "sha256:" + strings.Repeat("b", 64)
	plan := &agentv1.CloudPlan{
		PlanId: testCloudPlanID, OwnerId: "owner-from-config", ConnectionId: testCloudConnectionID,
		Recipe:  &agentv1.CloudRecipeBinding{RecipeId: "dirextalk-worker-diagnostic-v1", Maturity: "experimental"},
		QuoteId: testCloudQuoteID, QuoteDigest: quoteDigest, QuoteScopeDigest: scopeDigest,
		CandidateProfile: agentv1.CloudCandidateProfile_CLOUD_CANDIDATE_PROFILE_RECOMMENDED,
		QuoteValidUntil:  timestamppb.New(now.Add(15 * time.Minute)),
		Resource: &agentv1.CloudResourceScope{
			CandidateProfile: agentv1.CloudCandidateProfile_CLOUD_CANDIDATE_PROFILE_RECOMMENDED,
			Region:           "ap-northeast-3", InstanceType: "t3.small", InstanceCount: 1,
			Architecture: "amd64", Vcpu: 2, MemoryMib: 2048, DiskGib: 16,
			PurchaseOption: agentv1.CloudPurchaseOption_CLOUD_PURCHASE_OPTION_ON_DEMAND,
			WorkerImageId:  "ami-04965f4bf928dda7b", WorkerImageDigest: "sha256:" + strings.Repeat("c", 64),
		},
		Network: &agentv1.CloudNetworkScope{
			VpcId: "vpc-private", SubnetId: "subnet-private", SecurityGroupId: "sg-private",
			PublicExposure: false, PublicIpv4: false, TlsRequired: true, AuthenticationRequired: true,
		},
		Retention: &agentv1.CloudRetentionScope{
			RetentionClass: agentv1.CloudRetentionClass_CLOUD_RETENTION_CLASS_EPHEMERAL,
			AutoDestroy:    true, GracePeriodSeconds: 60, MaxLifetimeSeconds: 3600,
		},
		Status:   agentv1.CloudPlanStatus_CLOUD_PLAN_STATUS_READY_FOR_CONFIRMATION,
		PlanHash: "sha256:" + strings.Repeat("d", 64), Revision: 1,
	}
	return &cloudTestService{
		connections: []*agentv1.CloudConnection{{
			ConnectionId: testCloudConnectionID, OwnerId: "owner-from-config", Status: "active",
		}},
		plan: plan,
		quote: &agentv1.CloudQuote{
			QuoteId: testCloudQuoteID, QuotedAt: timestamppb.New(now), ValidUntil: timestamppb.New(now.Add(15 * time.Minute)),
			Currency: "USD", Digest: quoteDigest,
			Candidates: []*agentv1.CloudQuoteCandidate{{
				CandidateProfile: agentv1.CloudCandidateProfile_CLOUD_CANDIDATE_PROFILE_RECOMMENDED,
				ScopeDigest:      scopeDigest, HourlyEstimateMicros: 22000, MonthlyEstimateMicros: 16060000,
				MaximumLaunchAmountMicros: 32000,
			}},
			Assumptions: []string{"One hour maximum runtime"}, Exclusions: []string{"Data transfer beyond the estimate"},
		},
		deployment: &agentv1.CloudDeployment{
			DeploymentId: testCloudDeploymentID, OwnerId: "owner-from-config", TaskId: testCloudTaskID,
			StepId: testCloudStepID, WorkerId: testCloudWorkerID, PlanId: testCloudPlanID,
			ConnectionId: testCloudConnectionID, ExecutionStatus: agentv1.ExecutionStatus_EXECUTION_STATUS_RUNNING,
			OutcomeStatus: agentv1.OutcomeStatus_OUTCOME_STATUS_PENDING,
			Resources: &agentv1.CloudResourceSummary{
				Status: agentv1.CloudResourceStatus_CLOUD_RESOURCE_STATUS_ACTIVE, Revision: 3,
				ReadBack: &agentv1.CloudReadBackSummary{
					TotalResources: 1, ObservedResources: 1, ExistingResources: 1,
					LastObservedAt: timestamppb.New(now.Add(2 * time.Minute)),
				},
			},
			Revision: 4, CreatedAt: timestamppb.New(now), UpdatedAt: timestamppb.New(now.Add(2 * time.Minute)),
		},
		worker: &agentv1.CloudWorker{
			DeploymentId: testCloudDeploymentID, OwnerId: "owner-from-config", WorkerId: testCloudWorkerID,
			Status: agentv1.CloudWorkerStatus_CLOUD_WORKER_STATUS_LEASED, Attempt: 1, LeaseEpoch: 2,
			LeaseExpiresAt: timestamppb.New(now.Add(10 * time.Minute)), LastHeartbeatAt: timestamppb.New(now.Add(2 * time.Minute)),
			EvidenceCount: 2, Revision: 3, CreatedAt: timestamppb.New(now), UpdatedAt: timestamppb.New(now.Add(2 * time.Minute)),
		},
	}
}

type secretBootstrapTestService struct {
	agentv1.UnimplementedSecretBootstrapServiceServer
	mu            sync.Mutex
	serverPublic  []byte
	createRequest *agentv1.CreateSessionRequest
	uploadRequest *agentv1.UploadEncryptedRequest
	session       *agentv1.SecretBootstrapSession
}

func newSecretBootstrapTestService(t *testing.T) *secretBootstrapTestService {
	t.Helper()
	privateKey, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return &secretBootstrapTestService{serverPublic: privateKey.PublicKey().Bytes()}
}

func (service *secretBootstrapTestService) CreateSession(_ context.Context, request *agentv1.CreateSessionRequest) (*agentv1.CreateSessionResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.createRequest = proto.Clone(request).(*agentv1.CreateSessionRequest)
	if service.session == nil {
		now := time.Now().UTC().Truncate(time.Microsecond)
		service.session = &agentv1.SecretBootstrapSession{
			SessionId: "27b65194-c9c3-4dc6-bca7-79c9a32ed4dc", OwnerId: request.GetOwnerId(),
			Purpose: request.GetPurpose(), TargetId: request.GetTargetId(), ServerPublicKey: append([]byte(nil), service.serverPublic...),
			CreatedAt: timestamppb.New(now), ExpiresAt: timestamppb.New(now.Add(10 * time.Minute)),
			Status: agentv1.SecretBootstrapSessionStatus_SECRET_BOOTSTRAP_SESSION_STATUS_AWAITING_UPLOAD, Revision: 1,
			AgentInstanceId:       "11111111-2222-4333-8444-555555555555",
			SessionSchemaVersion:  "dirextalk.agent.secret-bootstrap.session/v1",
			EnvelopeSchemaVersion: "dirextalk.agent.secret-bootstrap.envelope/v1",
		}
	}
	session := proto.Clone(service.session).(*agentv1.SecretBootstrapSession)
	uploadToken := []byte(nil)
	if session.GetStatus() == agentv1.SecretBootstrapSessionStatus_SECRET_BOOTSTRAP_SESSION_STATUS_AWAITING_UPLOAD {
		uploadToken = bytes.Repeat([]byte{0x5a}, 32)
	}
	return &agentv1.CreateSessionResponse{
		SessionId: session.GetSessionId(), ServerPublicKey: append([]byte(nil), session.GetServerPublicKey()...),
		UploadToken: uploadToken, ExpiresAt: session.GetExpiresAt(), Session: session,
	}, nil
}

func (service *secretBootstrapTestService) UploadEncrypted(_ context.Context, request *agentv1.UploadEncryptedRequest) (*agentv1.UploadEncryptedResponse, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	service.uploadRequest = proto.Clone(request).(*agentv1.UploadEncryptedRequest)
	if service.session == nil || request.GetSessionId() != service.session.GetSessionId() || request.GetExpectedRevision() != 1 ||
		len(request.GetUploadToken()) != 32 || len(request.GetClientPublicKey()) != 32 || len(request.GetNonce()) != 12 || len(request.GetCiphertext()) < 16 {
		return nil, status.Error(codes.InvalidArgument, "invalid encrypted upload")
	}
	service.session.Status = agentv1.SecretBootstrapSessionStatus_SECRET_BOOTSTRAP_SESSION_STATUS_UPLOADED
	service.session.Revision = 2
	return &agentv1.UploadEncryptedResponse{Revision: 2, Session: proto.Clone(service.session).(*agentv1.SecretBootstrapSession)}, nil
}

type testRuntimeServer struct {
	target  string
	caFile  string
	keyFile string
	service *runtimeTestService
	tasks   *taskTestService
	cloud   *cloudTestService
	team    *teamTestService
	secrets *secretBootstrapTestService
}

func startRuntimeServer(t *testing.T) testRuntimeServer {
	t.Helper()
	certificate, caPEM := testCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	service := &runtimeTestService{}
	tasks := newTaskTestService()
	cloud := newCloudTestService()
	team := newTeamTestService()
	secrets := newSecretBootstrapTestService(t)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS13, MaxVersion: tls.VersionTLS13,
	})))
	agentv1.RegisterRuntimeServiceServer(server, service)
	agentv1.RegisterTaskServiceServer(server, tasks)
	agentv1.RegisterCloudControlServiceServer(server, cloud)
	agentv1.RegisterTeamPlanServiceServer(server, team)
	agentv1.RegisterSecretBootstrapServiceServer(server, secrets)
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { server.Stop(); _ = listener.Close() })
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	keyFile := filepath.Join(dir, "service-key")
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, []byte(testServiceKey+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return testRuntimeServer{
		target: listener.Addr().String(), caFile: caFile, keyFile: keyFile,
		service: service, tasks: tasks, cloud: cloud, team: team, secrets: secrets,
	}
}

func newTestRunner(t *testing.T, server testRuntimeServer, override Config) *Runner {
	t.Helper()
	override.Target = server.target
	override.CAFile = server.caFile
	override.ServerName = "agent.test"
	override.ServiceKeyFile = server.keyFile
	override.OwnerID = "owner-from-config"
	runner, err := New(context.Background(), override)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runner.Close() })
	return runner
}

func testCertificate(t *testing.T) (tls.Certificate, []byte) {
	t.Helper()
	now := time.Now()
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test ca"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTemplate := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "agent.test"}, DNSNames: []string{"agent.test"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, KeyUsage: x509.KeyUsageDigitalSignature}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leafKeyDER, err := x509.MarshalPKCS8PrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := tls.X509KeyPair(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: leafKeyDER}))
	if err != nil {
		t.Fatal(err)
	}
	return certificate, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}
