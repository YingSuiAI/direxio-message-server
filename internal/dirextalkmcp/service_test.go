package dirextalkmcp

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

type recordingInvoker struct {
	action string
	params map[string]any
}

func (i *recordingInvoker) InvokeCapability(ctx context.Context, action string, params map[string]any) (any, *Error) {
	i.action = action
	i.params = params
	return map[string]any{"action": action}, nil
}

func TestServiceOwnsCapabilityRegistryAndInvokesByAction(t *testing.T) {
	invoker := &recordingInvoker{}
	service := NewService(invoker)

	result, err := service.Invoke(context.Background(), ActionMessagesList, map[string]any{"room_id": "!room:example.com"})
	if err != nil {
		t.Fatalf("expected invoke to pass through unified service, got %v", err)
	}
	if invoker.action != ActionMessagesList || invoker.params["room_id"] != "!room:example.com" {
		t.Fatalf("expected registered action to reach invoker, action=%q params=%#v", invoker.action, invoker.params)
	}
	if result.(map[string]any)["action"] != ActionMessagesList {
		t.Fatalf("unexpected result: %#v", result)
	}

	if _, err := service.Invoke(context.Background(), "mcp.unknown", map[string]any{}); err == nil || err.Status != http.StatusBadRequest {
		t.Fatalf("expected unknown MCP action to be rejected by unified service, got %#v", err)
	}
}

type staticRoomAuthorizer struct {
	blocked map[string]bool
}

func (a staticRoomAuthorizer) MCPRoomBlocked(roomID string) bool {
	return a.blocked[roomID]
}

func TestServiceOwnsRoomAuthorizationError(t *testing.T) {
	service := NewServiceWithConfig(Config{
		Invoker:        &recordingInvoker{},
		RoomAuthorizer: staticRoomAuthorizer{blocked: map[string]bool{"!blocked:example.com": true}},
	})

	if err := service.RequireRoomAllowed("!visible:example.com"); err != nil {
		t.Fatalf("expected visible room to pass, got %v", err)
	}
	if err := service.RequireRoomAllowed("!blocked:example.com"); err == nil || err.Status != http.StatusForbidden || err.Message != "room is blocked for MCP" {
		t.Fatalf("expected blocked room 403 from unified service, got %#v", err)
	}
}

func TestToolsAreGeneratedFromSameRegistryAsActions(t *testing.T) {
	service := NewService(&recordingInvoker{})

	actions := service.Actions()
	tools := service.Tools()
	if len(actions) != len(tools) {
		t.Fatalf("expected each MCP action to have one native tool, actions=%d tools=%d", len(actions), len(tools))
	}
	toolActions := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Name == "" || tool.Description == "" || tool.InputSchema["type"] != "object" {
			t.Fatalf("tool must expose MCP schema metadata, got %#v", tool)
		}
		if tool.Effect != ToolEffectRead && tool.Effect != ToolEffectNonIdempotentWrite {
			t.Fatalf("tool must declare a known effect, got %#v", tool)
		}
		toolActions = append(toolActions, tool.Action)
	}
	if !reflect.DeepEqual(actions, toolActions) {
		t.Fatalf("native tool registry must preserve MCP action order, actions=%#v toolActions=%#v", actions, toolActions)
	}
}

func TestRoomsSearchSchemaUsesValidatorTypeSet(t *testing.T) {
	var roomsSearch Tool
	found := false
	for _, tool := range Tools() {
		if tool.Action == ActionRoomsSearch {
			roomsSearch = tool
			found = true
			break
		}
	}
	if !found {
		t.Fatal("rooms search tool is missing")
	}

	properties, ok := roomsSearch.InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("rooms search properties = %#v", roomsSearch.InputSchema["properties"])
	}
	typeSchema, ok := properties["type"].(map[string]any)
	if !ok {
		t.Fatalf("rooms search type schema = %#v", properties["type"])
	}
	values := RoomsSearchTypeValues()
	if typeSchema["type"] != "string" || !reflect.DeepEqual(typeSchema["enum"], values) {
		t.Fatalf("rooms search type schema = %#v, want enum %#v", typeSchema, values)
	}
	for _, value := range values {
		if !ValidRoomsSearchType(value) {
			t.Fatalf("schema value %q is rejected by validator", value)
		}
	}
	if ValidRoomsSearchType("room") || ValidRoomsSearchType("") {
		t.Fatal("validator accepted a value outside the published enum")
	}
}

func TestToolEffectsProduceSafeStandardAnnotations(t *testing.T) {
	writeActions := map[string]bool{
		ActionMessagesSend:          true,
		ActionChannelCommentsCreate: true,
	}
	for _, tool := range Tools() {
		annotations := tool.Annotations()
		if writeActions[tool.Action] {
			if tool.Effect != ToolEffectNonIdempotentWrite || annotations.ReadOnlyHint || annotations.DestructiveHint || annotations.IdempotentHint || !annotations.OpenWorldHint {
				t.Errorf("write tool %q effect/annotations = %q/%#v", tool.Action, tool.Effect, annotations)
			}
			continue
		}
		if tool.Effect != ToolEffectRead || !annotations.ReadOnlyHint || annotations.DestructiveHint || !annotations.IdempotentHint || !annotations.OpenWorldHint {
			t.Errorf("read tool %q effect/annotations = %q/%#v", tool.Action, tool.Effect, annotations)
		}
	}

	unknown := (Tool{}).Annotations()
	if unknown.ReadOnlyHint || !unknown.DestructiveHint || unknown.IdempotentHint || !unknown.OpenWorldHint {
		t.Fatalf("unknown effect must fail closed, got %#v", unknown)
	}
}
