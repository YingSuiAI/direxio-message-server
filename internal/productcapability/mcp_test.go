package productcapability

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"testing"

	capv1 "github.com/YingSuiAI/dirextalk-capability-api/gen/go/dirextalk/capability/v1"
)

func TestRegistryWithInvokerPublishesMCPToolsAsFixedCapabilities(t *testing.T) {
	var gotAction string
	registry, err := NewRegistryWithInvokerChecked(func(_ context.Context, action string, _ map[string]any) (any, error) {
		gotAction = action
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	provider, ok := registry.Get("product.spi.messages_send.v1")
	if !ok || provider == nil {
		t.Fatal("MCP messages.send capability was not registered")
	}
	if _, ok := registry.Operation("product.spi.messages_send.v1", "invoke"); !ok {
		t.Fatal("MCP SPI operation was not registered")
	}
	operation, ok := registry.Operation("product.spi.messages_send.v1", "invoke")
	if !ok || len(operation.GetInputSchemaDigest()) != sha256.Size || len(operation.GetResultSchemaDigest()) != sha256.Size {
		t.Fatalf("MCP SPI operation schema digests are incomplete: %#v", operation)
	}
	for _, descriptor := range registry.List() {
		for _, candidate := range descriptor.GetOperations() {
			inputDigest := sha256.Sum256([]byte(candidate.GetInputSchemaJson()))
			resultDigest := sha256.Sum256([]byte(candidate.GetResultSchemaJson()))
			if len(candidate.GetInputSchemaDigest()) != sha256.Size || !bytes.Equal(candidate.GetInputSchemaDigest(), inputDigest[:]) {
				t.Fatalf("%s/%s input schema digest is missing or stale", descriptor.GetCapabilityId(), candidate.GetOperationId())
			}
			if strings.TrimSpace(candidate.GetResultSchemaJson()) == "" || len(candidate.GetResultSchemaDigest()) != sha256.Size || !bytes.Equal(candidate.GetResultSchemaDigest(), resultDigest[:]) {
				t.Fatalf("%s/%s result schema digest is missing or stale", descriptor.GetCapabilityId(), candidate.GetOperationId())
			}
		}
	}
	result, err := provider.Handler(context.Background(), []byte(`{"operation":"invoke","input":{"room_id":"!room:example.test","msg":"hello"}}`))
	if err != nil {
		t.Fatalf("fixed MCP SPI handler failed: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(result, &decoded); err != nil || decoded["ok"] != true {
		t.Fatalf("unexpected MCP SPI result: %s", result)
	}
	if gotAction != "mcp.messages.send" {
		t.Fatalf("SPI handler tunneled the wrong action: %q", gotAction)
	}
	if _, err := provider.Handler(context.Background(), []byte(`{"operation":"invoke","input":{"owner_id":"attacker"}}`)); err == nil {
		t.Fatal("SPI handler accepted a forged owner identity")
	}
}

func TestRoomsCapabilityPublishesOnlySupportedRoomTypes(t *testing.T) {
	registry, err := NewRegistryWithInvokerChecked(func(context.Context, string, map[string]any) (any, error) {
		return map[string]any{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, operationID := range []string{"list", "search"} {
		operation, ok := registry.Operation("product.rooms.v1", operationID)
		if !ok {
			t.Fatalf("product.rooms.v1/%s is missing", operationID)
		}
		var schema struct {
			Properties map[string]struct {
				Enum []string `json:"enum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal([]byte(operation.GetInputSchemaJson()), &schema); err != nil {
			t.Fatal(err)
		}
		if got := strings.Join(schema.Properties["type"].Enum, ","); got != "all,contact,group,channel" {
			t.Fatalf("product.rooms.v1/%s type enum=%q", operationID, got)
		}
	}
}

func TestMemberGetAllowsTargetUserButRejectsOwnerSpoofing(t *testing.T) {
	var got map[string]any
	registry, err := NewRegistryWithInvokerChecked(func(_ context.Context, _ string, params map[string]any) (any, error) {
		got = params
		return map[string]any{"ok": true}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := registry.Get("product.members.v1")
	if !ok {
		t.Fatal("members capability missing")
	}
	if _, err := provider.Handler(context.Background(), []byte(`{"operation":"get","input":{"room_id":"!room:example.test","user_id":"@member:example.test"}}`)); err != nil {
		t.Fatalf("legitimate target user was rejected: %v", err)
	}
	if got["user_id"] != "@member:example.test" {
		t.Fatalf("target user was not forwarded: %#v", got)
	}
	if _, err := provider.Handler(context.Background(), []byte(`{"operation":"get","input":{"room_id":"!room:example.test","user_id":"@member:example.test","authenticated_owner_id":"@attacker:example.test"}}`)); err == nil {
		t.Fatal("members capability accepted forged authenticated owner")
	}
}

func TestGenericMatrixSPIUsesPreparedMutationFence(t *testing.T) {
	for _, test := range []struct {
		capability string
		operation  string
		want       bool
	}{
		{"product.messages.v1", "send", true},
		{"product.channel_comments.v1", "create", true},
		{"product.spi.messages_send.v1", "invoke", true},
		{"product.spi.channel_comments_create.v1", "invoke", true},
		{"product.spi.contacts_list.v1", "invoke", false},
	} {
		if got := isPreparedMatrixMutation(test.capability, test.operation); got != test.want {
			t.Fatalf("isPreparedMatrixMutation(%q,%q)=%v want %v", test.capability, test.operation, got, test.want)
		}
	}
}

func TestCapabilityDescriptorValidationFailsClosed(t *testing.T) {
	descriptor := &capv1.CapabilityDescriptor{
		CapabilityId: "product.invalid.v1", SemanticVersion: "1.0.0", ProtocolVersion: 1,
		Operations: []*capv1.OperationDescriptor{{
			OperationId: "read", InputSchemaJson: `{"type": "object"}`,
			RequiredScopes: []string{"product:invalid:read"},
			Audience:       []capv1.Audience{capv1.Audience_AUDIENCE_NATIVE_AGENT},
		}},
	}
	if err := validateCapabilityDescriptor(descriptor); err == nil {
		t.Fatal("non-canonical descriptor schema was accepted")
	}
}

func TestCapabilityDescriptorValidationRequiresResultSchemaDigest(t *testing.T) {
	input := `{"type":"object"}`
	inputDigest := sha256.Sum256([]byte(input))
	descriptor := &capv1.CapabilityDescriptor{
		CapabilityId: "product.invalid-result.v1", SemanticVersion: "1.0.0", ProtocolVersion: 1,
		Operations: []*capv1.OperationDescriptor{{
			OperationId: "read", InputSchemaJson: input, InputSchemaDigest: inputDigest[:],
			RequiredScopes: []string{"product:invalid:read"}, Audience: []capv1.Audience{capv1.Audience_AUDIENCE_NATIVE_AGENT},
		}},
	}
	if err := validateCapabilityDescriptor(descriptor); err == nil {
		t.Fatal("descriptor without result schema/digest was accepted")
	}
}
