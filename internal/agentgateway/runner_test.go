package agentgateway

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
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
	if binding, ok := actionBindingFor("agent.models.list"); !ok || binding.capabilityID != "agent.info.v1" || binding.operation != "list_models" {
		t.Fatalf("provider model catalog binding = %#v, want agent.info.v1/list_models", binding)
	}
	if binding, ok := actionBindingFor("agent.model_profiles.list"); !ok || binding.capabilityID != "agent.models.v1" || binding.operation != "list_models" {
		t.Fatalf("model profile list binding = %#v, want agent.models.v1/list_models", binding)
	}
	for action, operation := range map[string]string{
		"agent.web_search.config.get":    "get_config",
		"agent.web_search.config.update": "update_config",
		"agent.web_search.test":          "test",
	} {
		binding, ok := actionBindingFor(action)
		if !ok || binding.capabilityID != "agent.web_search.v1" || binding.operation != operation {
			t.Errorf("%s binding = %#v, want agent.web_search.v1/%s", action, binding, operation)
		}
	}
}

func TestEveryLiveActionBindingHasPinnedCatalogSchema(t *testing.T) {
	for action := range actionBindings {
		requirement := NewCatalogRequirement(action)
		if !requirement.RequireSchemaPin || len(requirement.InputSchemaDigest) != sha256.Size || len(requirement.ResultSchemaDigest) != sha256.Size {
			t.Errorf("live action %q is missing exact schema pins: require=%v input=%d result=%d", action, requirement.RequireSchemaPin, len(requirement.InputSchemaDigest), len(requirement.ResultSchemaDigest))
		}
	}
}

func TestCatalogLookupRejectsUnpinnedLiveAction(t *testing.T) {
	const action = "agent.test.unpinned"
	actionBindings[action] = actionBinding{capabilityID: "agent.test.v1", operation: "test"}
	defer delete(actionBindings, action)
	if _, err := catalogRequirementForLookup(action); err == nil {
		t.Fatal("live lookup accepted an action without an exact schema pin")
	}
}

func TestModelCatalogDefaultsKindAtGatewayBoundary(t *testing.T) {
	binding, ok := actionBindingFor("agent.models.list")
	if !ok {
		t.Fatal("agent.models.list binding is missing")
	}
	raw, err := transformCapabilityRequest("agent.models.list", "operation-id", map[string]any{"provider": "openrouter"}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"model_kind":"conversation"`)) {
		t.Fatalf("default model kind was not canonicalized: %s", raw)
	}

	raw, err = transformCapabilityRequest("agent.models.list", "operation-id", map[string]any{"model_kind": "embedding"}, binding)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`"model_kind":"embedding"`)) {
		t.Fatalf("explicit model kind was replaced: %s", raw)
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
	events := nativeEventsFromResult(result, 17)
	if len(events) != 4 {
		t.Fatalf("flattened event count = %d, want 4", len(events))
	}
	want := []string{"accepted", "delta", "tool", "done"}
	for i, event := range events {
		if event.Event != want[i] {
			t.Errorf("event[%d] = %q, want %q", i, event.Event, want[i])
		}
		if event.Seq != 17 {
			t.Errorf("event[%d] seq = %d, want outer result sequence 17", i, event.Seq)
		}
	}
	if _, ok := any(events[1].Data["events"]).([]any); ok {
		t.Fatal("flattened delta must not contain nested events")
	}
}

func TestNativeEventsFromResultProjectsOnlyPositiveIntegerSequences(t *testing.T) {
	events := nativeEventsFromResult([]byte(`{"events":[{"kind":"delta","sequence":7},{"kind":"delta","sequence":2.5},{"kind":"delta","sequence":"8"},{"kind":"delta","sequence":-1},{"kind":"delta","sequence":9223372036854775808},{"kind":"done","sequence":9223372036854775807}]}`), 0)
	if len(events) != 6 {
		t.Fatalf("event count = %d, want 6", len(events))
	}
	want := []int64{7, 0, 0, 0, 0, 9223372036854775807}
	for i, event := range events {
		if event.Seq != want[i] {
			t.Errorf("event[%d] seq = %d, want %d", i, event.Seq, want[i])
		}
	}

	envelopeEvents := nativeEventsFromResult([]byte(`{"sequence":9,"events":[{"kind":"delta"},{"kind":"done"}]}`), 0)
	for i, event := range envelopeEvents {
		if event.Seq != 9 {
			t.Errorf("envelope event[%d] seq = %d, want 9", i, event.Seq)
		}
	}
}

func TestNativeEventFromProtoProjectsSequence(t *testing.T) {
	event, terminal, err := nativeEventFromProto(&capv1.WatchOperationEvent{
		OperationId: "operation-1",
		Sequence:    23,
		Event:       &capv1.WatchOperationEvent_Progress{Progress: &capv1.ProgressEvent{EventJson: []byte(`{"kind":"delta","text":"hi"}`)}},
	})
	if err != nil || terminal || event == nil {
		t.Fatalf("native event = %#v, terminal=%v, err=%v", event, terminal, err)
	}
	if event.Seq != 23 || event.Data["sequence"] != int64(23) {
		t.Fatalf("native event sequence = %#v, want 23", event)
	}

	event, _, _ = nativeEventFromProto(&capv1.WatchOperationEvent{
		OperationId: "operation-1",
		Sequence:    -1,
		Event:       &capv1.WatchOperationEvent_Accepted{Accepted: &capv1.AcceptedEvent{}},
	})
	if event == nil || event.Seq != 0 || event.Data["sequence"] != int64(0) {
		t.Fatalf("non-positive proto sequence was projected: %#v", event)
	}
}

func TestNativeEventFromProtoProjectsKnowledgeQuotaCode(t *testing.T) {
	event, terminal, err := nativeEventFromProto(&capv1.WatchOperationEvent{
		OperationId: "operation-1",
		Sequence:    24,
		Event: &capv1.WatchOperationEvent_Error{Error: &capv1.ErrorEvent{Error: &capv1.CapabilityError{
			Code: capv1.ErrorCode_ERROR_CODE_RESOURCE_EXHAUSTED,
			Details: map[string]string{
				"code": KnowledgeQuotaExceededCode,
			},
		}}},
	})
	var capabilityErr *CapabilityError
	if !terminal || !errors.As(err, &capabilityErr) || capabilityErr.ClientCode != KnowledgeQuotaExceededCode {
		t.Fatalf("knowledge quota event terminal=%v err=%#v", terminal, err)
	}
	if event == nil || event.Data["code"] != KnowledgeQuotaExceededCode || event.Data["error_code"] != KnowledgeQuotaExceededCode || event.Data["error"] != "knowledge quota exceeded" {
		t.Fatalf("knowledge quota event = %#v", event)
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
