package agent

import (
	"context"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
)

func TestControlToolsExposeOnlyBoundedFirstPartySurface(t *testing.T) {
	tools := ControlTools(ControlInvokerFunc(func(context.Context, string, map[string]any) (any, error) {
		return map[string]any{"ok": true}, nil
	}))
	want := map[string]bool{
		"native_agent_aws_credentials_list":       true,
		"native_agent_aws_credentials_test":       true,
		"native_agent_workloads_list":             true,
		"native_agent_workloads_get":              true,
		"native_agent_workloads_quote":            true,
		"native_agent_workload_operations_get":    true,
		"native_agent_workload_operations_events": true,
		"native_agent_deployments_list":           true,
		"native_agent_deployments_get":            true,
		"native_agent_deployments_events":         true,
	}
	if len(tools) != len(want) {
		t.Fatalf("control tool count = %d, want %d", len(tools), len(want))
	}
	for _, tool := range tools {
		if !want[tool.Name] {
			t.Fatalf("unexpected control tool %q", tool.Name)
		}
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing control tools: %#v", want)
	}
}

func TestControlToolsExcludeGenericWorkloadMutations(t *testing.T) {
	tools := ControlTools(ControlInvokerFunc(func(context.Context, string, map[string]any) (any, error) { return nil, nil }))
	for _, tool := range tools {
		switch tool.Name {
		case "native_agent_workloads_plan", "native_agent_workloads_apply", "native_agent_workloads_destroy":
			t.Fatalf("generic workload mutation leaked into Native Agent tools: %q", tool.Name)
		}
	}
}

func TestControlIdempotencyOnlyUsesAuthenticatedTurnScope(t *testing.T) {
	ctx := nativeagent.WithRequestContextIntent(context.Background(), "owner", "conversation", "turn", "")
	first := controlIdempotencyKey(ctx, "action-a", map[string]any{"command_steps": []any{"danger"}})
	second := controlIdempotencyKey(ctx, "action-b", map[string]any{"typed_target": map[string]any{"secret": "danger"}})
	if first == "" || first != second {
		t.Fatalf("idempotency key included action/params: %q %q", first, second)
	}
}
