package agentgateway

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"testing"
	"time"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

func TestActionBindingIsExplicit(t *testing.T) {
	if _, ok := actionBindingFor("agent.chat.stream"); !ok {
		t.Fatal("typed chat stream binding is missing")
	}
	if binding, ok := actionBindingFor("agent.chat.turns.list"); !ok || binding.operation != "list_turns" {
		t.Fatalf("turn listing binding = %#v, want list_turns", binding)
	}
	if _, ok := actionBindingFor("agent.chat.conversations.list.stream"); ok {
		t.Fatal("unknown stream suffix must not use heuristic fallback")
	}
	if binding, ok := actionBindingFor("agent.core.mcp.execute"); !ok || binding.capabilityID != "agent.skills.v1" || binding.operation != "execute_mcp" {
		t.Fatalf("typed MCP execute binding = %#v, want agent.skills.v1/execute_mcp", binding)
	}
}

func TestRunnerOperationIDForTurnIsStableAndCanonical(t *testing.T) {
	runner := &Runner{config: RunnerConfig{OwnerID: func() string { return "@owner:example" }}}
	params := map[string]any{"turn_id": "turn-123"}
	first := runner.operationIDFor(params)
	second := runner.operationIDFor(params)
	if first == "" || first != second {
		t.Fatalf("turn operation id is not stable: %q/%q", first, second)
	}
	if _, ok := actionBindingFor("agent.chat"); !ok {
		t.Fatal("chat binding missing")
	}
}

func TestNativeEventsFromResultFlattensCoreChatEnvelope(t *testing.T) {
	result := []byte(`{"events":[{"kind":"started","request_id":"r"},{"kind":"delta","text":"hi"},{"kind":"tool_call","tool_call":{"name":"search"}},{"kind":"done"}],"response":{"done":true}}`)
	events := nativeEventsFromResult(result)
	if len(events) != 4 {
		t.Fatalf("flattened event count = %d, want 4", len(events))
	}
	want := []string{"accepted", "delta", "tool", "done"}
	for i, event := range events {
		if event.Event != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, event.Event, want[i])
		}
	}
	if _, ok := any(events[1].Data["events"]).([]any); ok {
		t.Fatal("flattened delta must not contain nested events")
	}
}

func TestOperationControlPermissionIsFreshAndTargetBound(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{config: RunnerConfig{
		GrantPrivateKey: privateKey,
		GrantCodec:      capv1.GrantCodec{Now: func() time.Time { return time.UnixMilli(1_700_000_100_000) }},
	}}
	base := &capv1.PermissionContext{AuthenticatedOwnerId: "owner-123", AccountGeneration: 7}
	entry := capv1.NewCallContext("018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c12", "018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c12", 1_700_000_150_000)
	entry, err = capv1.AppendCallNode(entry, capv1.NodeMessage)
	if err != nil {
		t.Fatal(err)
	}
	permission, err := runner.operationControlPermission(entry, "018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c12", "watch", base)
	if err != nil {
		t.Fatal(err)
	}
	if string(permission.GetCapabilityGrant()) == string(base.GetCapabilityGrant()) {
		t.Fatal("control grant must not reuse the root permission grant")
	}
	actual, err := capv1.AppendCallNode(entry, capv1.NodeAgent)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := runner.config.GrantCodec.VerifyOperationControlGrant(permission.GetCapabilityGrant(), publicKey, capv1.OperationControlGrantBinding{
		CallContext: actual, OwnerID: base.GetAuthenticatedOwnerId(), AccountGeneration: base.GetAccountGeneration(),
		OperationID: entry.GetRootOperationId(), ControlAction: "watch", ControlScope: "operation:control:watch",
	})
	if err != nil {
		t.Fatalf("control grant did not verify at Agent boundary: %v", err)
	}
	if verified.OperationID != entry.GetRootOperationId() || verified.EntryRoute != capv1.NodeMessage || verified.EntryHop != 1 {
		t.Fatalf("unexpected control grant claims: %#v", verified)
	}
}

func TestPrepareGrantCarriesRootDigestForProductDelegation(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	runner := &Runner{config: RunnerConfig{GrantPrivateKey: privateKey}}
	operationID := "018f2f1e-7b5b-7b21-8f2e-4a6f3b1d9c12"
	entry := capv1.NewCallContext(operationID, operationID, time.Now().Add(time.Minute).UnixMilli())
	entry, err = capv1.AppendCallNode(entry, capv1.NodeMessage)
	if err != nil {
		t.Fatal(err)
	}
	operation := &capv1.OperationDescriptor{OperationId: "invoke_product", RequiredScopes: []string{"agent:skills:execute"}, InputSchemaJson: `{"type":"object"}`}
	descriptor := &capv1.CapabilityDescriptor{CapabilityId: "agent.skills.v1", SemanticVersion: "1.0.0", ProtocolVersion: 1, Operations: []*capv1.OperationDescriptor{operation}}
	catalog := &capv1.DescribeCapabilitiesResponse{CatalogDigest: bytes.Repeat([]byte{1}, sha256.Size), Capabilities: []*capv1.CapabilityDescriptor{descriptor}}
	permission := &capv1.PermissionContext{AuthenticatedOwnerId: "owner-123", AccountGeneration: 7, GrantedScopes: []string{"agent:product:execute"}}
	rootDigest := sha256.Sum256([]byte("root request"))
	if _, _, err := runner.prepareGrantWithCatalog(catalog, entry, operationID, descriptor, operation, permission, rootDigest[:]); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(permission.GetRootRequestDigest(), rootDigest[:]) {
		t.Fatalf("permission root digest = %x, want %x", permission.GetRootRequestDigest(), rootDigest)
	}
}
