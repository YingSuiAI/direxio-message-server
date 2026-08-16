package p2p

import (
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/dirextalkmcp"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/serviceapi"
)

func TestActionRegistryCoversPublicAndAgentActions(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})

	for _, action := range serviceapi.PublicActions() {
		if _, ok := service.actions[action]; !ok {
			t.Errorf("public action %q has no registered handler", action)
		}
	}
	for _, action := range serviceapi.AgentActions() {
		if _, ok := service.actions[action]; !ok {
			t.Errorf("agent action %q has no registered handler", action)
		}
	}
}

func TestNativeAgentBusinessActionsAreNotProductActions(t *testing.T) {
	for _, action := range []string{
		"agent.chat", "agent.chat.turn.stop", "agent.core.confirmations.list",
		"agent.workers.list", "agent.model_profiles.test", "agent.memory.status",
	} {
		if _, ok := serviceapi.ActionSpecFor(action); ok {
			t.Errorf("direct Agent data-plane action %q remains in ProductCore", action)
		}
	}
}

func TestFixedMCPBodyActionsAreRemovedFromProductRegistry(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})

	for _, action := range dirextalkmcp.Tools() {
		if _, ok := service.actions[action.Action]; ok {
			t.Fatalf("fixed MCP body action %s must not be registered as a product action", action.Action)
		}
		if _, ok := serviceapi.ActionSpecFor(action.Action); ok {
			t.Fatalf("fixed MCP body action %s must not be listed in product action metadata", action.Action)
		}
		if serviceapi.AgentAction(action.Action) {
			t.Fatalf("fixed MCP body action %s must not authorize agent_token through product actions", action.Action)
		}
	}
}

func TestActionMetadataCoversRegistryAndDerivesClassifications(t *testing.T) {
	service := NewService(Config{ServerName: "example.com"})
	specs := serviceapi.ActionSpecs()
	if len(specs) == 0 {
		t.Fatal("expected action metadata")
	}
	metadataByName := map[string]serviceapi.ActionSpec{}
	for _, spec := range specs {
		if spec.Name == "" {
			t.Fatalf("action metadata has empty name: %#v", spec)
		}
		if _, exists := metadataByName[spec.Name]; exists {
			t.Fatalf("duplicate action metadata for %s", spec.Name)
		}
		switch spec.Auth {
		case serviceapi.ActionAuthPublic:
			if !serviceapi.PublicAction(spec.Name) {
				t.Fatalf("public metadata for %s must drive PublicAction", spec.Name)
			}
		case serviceapi.ActionAuthAgent:
			if !serviceapi.AgentAction(spec.Name) {
				t.Fatalf("agent metadata for %s must drive AgentAction", spec.Name)
			}
		case serviceapi.ActionAuthOwner:
		default:
			t.Fatalf("unexpected auth metadata for %s: %q", spec.Name, spec.Auth)
		}
		switch spec.Transport {
		case serviceapi.ActionTransportHTTPOnly, serviceapi.ActionTransportInternalOnly:
		default:
			t.Fatalf("unexpected transport metadata for %s: %q", spec.Name, spec.Transport)
		}
		metadataByName[spec.Name] = spec
	}
	for action := range service.actions {
		if _, ok := metadataByName[action]; !ok {
			t.Fatalf("registered action %s has no action metadata", action)
		}
	}
	for action := range metadataByName {
		if _, ok := service.actions[action]; !ok {
			t.Fatalf("action metadata %s has no registered handler", action)
		}
	}
}

func TestInternalPublicCallbacksAreHTTPOnly(t *testing.T) {
	for _, action := range []string{"rooms.reactivate", "channels.public.join_result"} {
		spec, ok := serviceapi.ActionSpecFor(action)
		if !ok {
			t.Fatalf("expected action metadata for %s", action)
		}
		if spec.Auth != serviceapi.ActionAuthPublic {
			t.Fatalf("expected %s to remain a public node-to-node callback, got %q", action, spec.Auth)
		}
		if spec.Transport != serviceapi.ActionTransportHTTPOnly {
			t.Fatalf("expected %s to be HTTP-only, got %q", action, spec.Transport)
		}
		if !serviceapi.HTTPAction(action) {
			t.Fatalf("expected %s to remain callable through HTTP body actions", action)
		}
	}
}
