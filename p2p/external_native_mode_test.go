package p2p

import (
	"context"
	"net/http"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentstream"
	"github.com/YingSuiAI/dirextalk-message-server/setup/process"
)

type externalDeprovisionRunner struct{ calls int }

func (r *externalDeprovisionRunner) Apply(context.Context, string) error { return nil }
func (r *externalDeprovisionRunner) Invoke(_ context.Context, action string, params map[string]any) (map[string]any, error) {
	if action != "agent.account.deprovision" {
		return map[string]any{"ok": true}, nil
	}
	r.calls++
	if params["confirm"] != "deprovision_account" || params["idempotency_key"] == "" {
		return nil, context.Canceled
	}
	return map[string]any{"status": "deprovisioned"}, nil
}
func (r *externalDeprovisionRunner) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	return nil
}

type externalNativeRunnerProbe struct {
	invoked string
}

func (p *externalNativeRunnerProbe) Apply(context.Context, string) error { return nil }

func (p *externalNativeRunnerProbe) Invoke(_ context.Context, action string, _ map[string]any) (map[string]any, error) {
	p.invoked = action
	return map[string]any{"ok": true}, nil
}

func (p *externalNativeRunnerProbe) Stream(context.Context, string, map[string]any, func(agentstream.Event) error) error {
	return nil
}

func TestExternalNativeAgentModeDoesNotConstructEmbeddedRuntime(t *testing.T) {
	probe := &externalNativeRunnerProbe{}
	service := NewService(Config{
		ServerName:        "example.test",
		NativeAgentRunner: probe,
	})
	if service == nil || service.agentModule == nil {
		t.Fatal("external mode must retain the public facade module")
	}
	if service.agentModule.HasLocalTurnCoordinator() {
		t.Fatal("external mode must not construct a local native-agent turn store")
	}
	if service.agentModule.HasLocalVoiceCoordinator() {
		t.Fatal("external mode must not construct a local voice coordinator")
	}
	processCtx := process.NewProcessContext()
	processCtx.ShutdownDendrite()
	handler := service.actions["agent.chat"]
	if handler == nil {
		t.Fatal("external mode must preserve the agent.chat action surface")
	}
	if _, actionErr := handler(context.Background(), map[string]any{"message": "hello"}); actionErr != nil {
		t.Fatalf("external agent action failed: %v", actionErr)
	}
	if probe.invoked != "agent.chat" {
		t.Fatalf("external action did not reach gateway runner: %q", probe.invoked)
	}
	coreHandler := service.actions["agent.core.tasks.list"]
	if coreHandler == nil {
		t.Fatal("external mode must preserve the core task action surface")
	}
	if _, actionErr := coreHandler(context.Background(), map[string]any{}); actionErr != nil {
		t.Fatalf("external core task action failed: %v", actionErr)
	}
	if probe.invoked != "agent.core.tasks.list" {
		t.Fatalf("external core task action did not reach gateway runner: %q", probe.invoked)
	}
	turnsHandler := service.actions["agent.chat.turns.list"]
	if turnsHandler == nil {
		t.Fatal("external mode must preserve the durable turn listing action")
	}
	if _, actionErr := turnsHandler(context.Background(), map[string]any{"conversation_id": "conversation-1"}); actionErr != nil {
		t.Fatalf("external turn listing failed: %v", actionErr)
	}
	if probe.invoked != "agent.chat.turns.list" {
		t.Fatalf("external turn listing did not reach Agent Core: %q", probe.invoked)
	}
}

func TestExternalNativeAgentDeletionRequiresAgentDeprovision(t *testing.T) {
	runner := &externalDeprovisionRunner{}
	service := NewService(Config{ServerName: "example.test", AccountGeneration: 3, NativeAgentRunner: runner})
	if err := service.deprovisionExternalAgent(context.Background()); err != nil {
		t.Fatalf("Agent deprovision: %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("deprovision calls=%d", runner.calls)
	}
}

func TestAccountDeletionFailsClosedWithoutExternalAgentDeprovision(t *testing.T) {
	service := NewService(Config{ServerName: "example.test", AccountGeneration: 3})
	if apiErr := service.deprovisionExternalAgent(context.Background()); apiErr == nil || apiErr.Status != http.StatusBadGateway {
		t.Fatalf("missing external Agent deprovision must fail closed, got %#v", apiErr)
	}
}

func TestExternalNativeAgentReadinessRequiresRunner(t *testing.T) {
	service := NewService(Config{ServerName: "example.test"})
	if err := service.agentModule.ReadyError(); err == nil {
		t.Fatal("missing external Agent runner must fail readiness")
	}
}
