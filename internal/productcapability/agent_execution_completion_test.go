package productcapability

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
	"testing"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkdomain"
	"github.com/google/uuid"
)

func completionRequest(t *testing.T) ([]byte, dirextalkdomain.AgentExecutionCompletionReceipt) {
	t.Helper()
	fixtureRaw, err := os.ReadFile("../agentgateway/testdata/cloud_worker_public_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Completion map[string]any `json:"completion"`
	}
	if err := json.Unmarshal(fixtureRaw, &fixture); err != nil || fixture.Completion == nil {
		t.Fatalf("decode Cloud Worker completion fixture: %v", err)
	}
	raw, err := json.Marshal(fixture.Completion)
	if err != nil {
		t.Fatal(err)
	}
	var receipt dirextalkdomain.AgentExecutionCompletionReceipt
	if err := json.Unmarshal(raw, &receipt); err != nil {
		t.Fatal(err)
	}
	wantDigest, err := dirextalkdomain.CanonicalAgentExecutionCompletionDigest(receipt)
	if err != nil || wantDigest != receipt.PayloadDigest {
		t.Fatalf("Cloud Worker completion fixture digest=%q want=%q err=%v", receipt.PayloadDigest, wantDigest, err)
	}
	canonical, err := capv1.CanonicalizeJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	return canonical, receipt
}

func completionStartRequest(t *testing.T, raw []byte) *capv1.StartOperationRequest {
	t.Helper()
	operationID := uuid.NewString()
	digest := sha256.Sum256(raw)
	return &capv1.StartOperationRequest{
		CallContext: &capv1.CallContext{
			ChainId: uuid.NewString(), RootOperationId: operationID,
			Hop: 2, Route: capv1.NodeAgent + capv1.RouteSeparator + capv1.NodeProduct,
			DeadlineUnixMs: time.Now().Add(time.Minute).UnixMilli(),
		},
		OperationId: operationID, CapabilityId: agentExecutionCompletionCapability,
		Operation: agentExecutionCompletionOperation, RequestJson: raw, RequestDigest: digest[:],
	}
}

func completionServer(recorder func(context.Context, dirextalkdomain.AgentExecutionCompletionReceipt) (bool, error)) *Server {
	return &Server{
		config: &Config{
			ServiceOwnerID: "@owner:example.test", ExpectedAccountGeneration: 7,
			RecordAgentExecutionCompletion: recorder,
		},
		registry: NewRegistry(), mutationSem: make(chan struct{}, 1),
	}
}

func TestPrivateCompletionStartResponseContractAndReplay(t *testing.T) {
	raw, want := completionRequest(t)
	for _, replayed := range []bool{false, true} {
		t.Run(map[bool]string{false: "first", true: "replay"}[replayed], func(t *testing.T) {
			calls := 0
			server := completionServer(func(_ context.Context, got dirextalkdomain.AgentExecutionCompletionReceipt) (bool, error) {
				calls++
				want.OwnerID, want.AccountGeneration = "@owner:example.test", 7
				if got != want {
					t.Fatalf("receipt=%#v want %#v", got, want)
				}
				return replayed, nil
			})
			request := completionStartRequest(t, raw)
			response, err := server.StartOperation(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || response.GetOperationId() != request.GetOperationId() || response.GetState() != capv1.OperationState_OPERATION_STATE_COMPLETED || response.GetError() != nil || response.GetReplayed() != replayed || len(response.GetControlGrants()) != 0 {
				t.Fatalf("private completion response=%#v calls=%d", response, calls)
			}
		})
	}
}

func TestPrivateCompletionIsHiddenAndRejectsOwnerOrModelGrant(t *testing.T) {
	registry, err := NewRegistryWithInvokerChecked(func(context.Context, string, map[string]any) (any, error) {
		return map[string]any{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := registry.Get(agentExecutionCompletionCapability); ok {
		t.Fatal("private completion capability leaked into the advertised Product catalog")
	}
	raw, _ := completionRequest(t)
	request := completionStartRequest(t, raw)
	request.Permission = &capv1.PermissionContext{AuthenticatedOwnerId: "@owner:example.test", AccountGeneration: 7}
	called := false
	response, err := completionServer(func(context.Context, dirextalkdomain.AgentExecutionCompletionReceipt) (bool, error) {
		called = true
		return false, nil
	}).StartOperation(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if called || response.GetState() != capv1.OperationState_OPERATION_STATE_FAILED || response.GetError().GetCode() != capv1.ErrorCode_ERROR_CODE_PERMISSION_DENIED {
		t.Fatalf("owner/model completion admission=%#v called=%t", response, called)
	}
}

func TestPrivateCompletionRejectsNonCanonicalDigestRouteAndFields(t *testing.T) {
	raw, _ := completionRequest(t)
	tests := map[string]func(*capv1.StartOperationRequest){
		"digest": func(r *capv1.StartOperationRequest) { r.RequestDigest = bytes.Repeat([]byte{1}, sha256.Size) },
		"nested route": func(r *capv1.StartOperationRequest) {
			r.CallContext.Route = capv1.NodeMessage + capv1.RouteSeparator + capv1.NodeAgent + capv1.RouteSeparator + capv1.NodeProduct
			r.CallContext.Hop = 3
		},
		"permission": func(r *capv1.StartOperationRequest) { r.Permission = &capv1.PermissionContext{} },
		"unknown field": func(r *capv1.StartOperationRequest) {
			var value map[string]any
			_ = json.Unmarshal(r.RequestJson, &value)
			value["owner_id"] = "@attacker:example.test"
			encoded, _ := json.Marshal(value)
			r.RequestJson, _ = capv1.CanonicalizeJSON(encoded)
			digest := sha256.Sum256(r.RequestJson)
			r.RequestDigest = digest[:]
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			called := false
			server := completionServer(func(context.Context, dirextalkdomain.AgentExecutionCompletionReceipt) (bool, error) {
				called = true
				return false, nil
			})
			request := completionStartRequest(t, raw)
			mutate(request)
			response, err := server.StartOperation(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if called || response.GetState() != capv1.OperationState_OPERATION_STATE_FAILED || response.GetError() == nil {
				t.Fatalf("invalid private completion response=%#v called=%t", response, called)
			}
		})
	}
}

func TestPrivateCompletionRejectsNonOutboxTerminalStatesFromGoldenShape(t *testing.T) {
	raw, _ := completionRequest(t)
	for _, state := range []string{"rejected", "expired"} {
		t.Run(state, func(t *testing.T) {
			var receipt dirextalkdomain.AgentExecutionCompletionReceipt
			if err := json.Unmarshal(raw, &receipt); err != nil {
				t.Fatal(err)
			}
			receipt.TerminalState = state
			digest, err := dirextalkdomain.CanonicalAgentExecutionCompletionDigest(receipt)
			if err != nil {
				t.Fatal(err)
			}
			receipt.PayloadDigest = digest
			drifted, err := json.Marshal(receipt.PublicPayload())
			if err != nil {
				t.Fatal(err)
			}
			drifted, err = capv1.CanonicalizeJSON(drifted)
			if err != nil {
				t.Fatal(err)
			}
			called := false
			request := completionStartRequest(t, drifted)
			response, err := completionServer(func(context.Context, dirextalkdomain.AgentExecutionCompletionReceipt) (bool, error) {
				called = true
				return false, nil
			}).StartOperation(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			if called || response.GetState() != capv1.OperationState_OPERATION_STATE_FAILED || response.GetError().GetCode() != capv1.ErrorCode_ERROR_CODE_INVALID_ARGUMENT {
				t.Fatalf("non-outbox completion admission=%#v called=%t", response, called)
			}
		})
	}
}
