package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentturns"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
)

func TestControlToolsExposeOnlyBoundedFirstPartySurface(t *testing.T) {
	tools := ControlTools(ControlInvokerFunc(func(context.Context, string, map[string]any) (any, error) {
		return map[string]any{"ok": true}, nil
	}))
	want := map[string]bool{
		"native_agent_aws_credentials_list":               true,
		"native_agent_aws_credentials_test":               true,
		"native_agent_workload_operations_get":            true,
		"native_agent_workload_operations_events":         true,
		"native_agent_workload_actual_get":                true,
		"native_agent_deployments_list":                   true,
		"native_agent_deployments_get":                    true,
		"native_agent_deployments_events":                 true,
		"native_agent_aws_ec2_provisions_plan":            true,
		"native_agent_aws_ec2_provisions_get":             true,
		"native_agent_aws_ec2_provisions_list":            true,
		"native_agent_aws_ec2_provisions_events":          true,
		"native_agent_aws_ec2_provisions_create_request":  true,
		"native_agent_aws_ec2_provisions_destroy_request": true,
		"native_agent_aws_ec2_geolibre_install_plan":      true,
		"native_agent_aws_ec2_geolibre_install_request":   true,
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
	for _, tool := range tools {
		if tool.Name == "native_agent_aws_ec2_provisions_create_request" || tool.Name == "native_agent_aws_ec2_provisions_destroy_request" || tool.Name == "native_agent_aws_ec2_geolibre_install_request" {
			if !tool.Write {
				t.Fatalf("dangerous tool %q is not marked write", tool.Name)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing control tools: %#v", want)
	}
}

func TestControlToolsExcludeGenericWorkloadMutations(t *testing.T) {
	tools := ControlTools(ControlInvokerFunc(func(context.Context, string, map[string]any) (any, error) { return nil, nil }))
	for _, tool := range tools {
		switch tool.Name {
		case "native_agent_workloads_plan", "native_agent_workloads_apply", "native_agent_workloads_destroy", "native_agent_aws_ec2_provisions_retry":
			t.Fatalf("generic workload mutation leaked into Native Agent tools: %q", tool.Name)
		}
	}
}

func TestControlToolsProjectOnlyTypedFieldsAndRequiresGeoPlanFence(t *testing.T) {
	var gotAction string
	var gotParams map[string]any
	control := ControlInvokerFunc(func(_ context.Context, action string, params map[string]any) (any, error) {
		gotAction, gotParams = action, params
		return map[string]any{"ok": true}, nil
	})
	ctx := nativeagent.WithRequestContextIntent(context.Background(), "owner", "conversation", "turn", "deploy GeoLibre")
	var geoTool nativeagent.Tool
	for _, candidate := range ControlTools(control) {
		if candidate.Name == "native_agent_aws_ec2_geolibre_install_request" {
			geoTool = candidate
		}
	}
	if geoTool.Handler == nil {
		t.Fatal("missing GeoLibre request handler")
	}
	if _, err := geoTool.Handler(ctx, map[string]any{"provision_id": "p", "expected_revision": int64(2), "plan_id": "plan", "plan_revision": int64(1), "plan_digest": "digest", "expires_at": "2030-01-01T00:00:00Z", "workload_id": "workload", "expected_workload_revision": int64(1), "owner_id": "must-not-pass", "command": "must-not-pass"}); err != nil {
		t.Fatalf("handler error: %v", err)
	}
	if gotAction != "agent.core.aws.ec2_provisions.geolibre_install.request" {
		t.Fatalf("action = %q", gotAction)
	}
	if _, ok := gotParams["owner_id"]; ok {
		t.Fatal("model owner_id leaked into control request")
	}
	if _, ok := gotParams["command"]; ok {
		t.Fatal("model command leaked into control request")
	}
	if gotParams["expires_at"] != "2030-01-01T00:00:00Z" || gotParams["idempotency_key"] == nil {
		t.Fatalf("server fields missing: %#v", gotParams)
	}
	if _, err := geoTool.Handler(ctx, map[string]any{"provision_id": "p", "expected_revision": int64(2), "plan_id": "plan", "plan_revision": int64(1), "plan_digest": "digest", "expires_at": "2030-01-01T00:00:00Z", "workload_id": "workload"}); err == nil {
		t.Fatal("workload_id without expected_workload_revision was accepted")
	}
}

func TestControlToolsGeoPlanRequiresPersistedIssuedAt(t *testing.T) {
	var got map[string]any
	control := ControlInvokerFunc(func(_ context.Context, _ string, params map[string]any) (any, error) { got = params; return nil, nil })
	var planTool nativeagent.Tool
	for _, candidate := range ControlTools(control) {
		if candidate.Name == "native_agent_aws_ec2_geolibre_install_plan" {
			planTool = candidate
		}
	}
	ctx := nativeagent.WithRequestContextIntent(context.Background(), "owner", "conversation", "turn", "plan")
	if _, err := planTool.Handler(ctx, map[string]any{"provision_id": "p", "expected_revision": int64(1)}); err == nil {
		t.Fatal("GeoLibre plan succeeded without durable issued_at")
	}
	ctx = agentturns.WithIssuedAt(ctx, time.Now().UTC())
	if _, err := planTool.Handler(ctx, map[string]any{"provision_id": "p", "expected_revision": int64(1)}); err != nil {
		t.Fatalf("GeoLibre plan with issued_at failed: %v", err)
	}
	if got["expires_at"] == nil {
		t.Fatalf("server expiry missing: %#v", got)
	}
	expired := agentturns.WithIssuedAt(ctx, time.Now().UTC().Add(-geoLibrePlanTTL-time.Minute))
	if _, err := planTool.Handler(expired, map[string]any{"provision_id": "p", "expected_revision": int64(1)}); err == nil {
		t.Fatal("GeoLibre plan succeeded with expired durable issued_at")
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

func TestRedactNativeControlResultRemovesExecutionAndOwnerMaterial(t *testing.T) {
	result := redactNativeControlResult(map[string]any{
		"owner_id": "@owner:example", "operation": map[string]any{
			"desired_plan": map[string]any{"command_steps": []any{"rm -rf"}, "image_uri": "private"},
			"confirmation": map[string]any{"binding": map[string]any{"selected_command": []any{"secret"}, "content_digest": "a"}},
		},
		"typed_target": map[string]any{"labels": map[string]string{"owner": "digest"}, "instance_id": "i-1"},
	})
	serialized := fmt.Sprint(result)
	for _, forbidden := range []string{"owner_id", "desired_plan", "command_steps", "image_uri", "selected_command", "labels", "rm -rf", "private", "secret"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("redacted result retained %q: %s", forbidden, serialized)
		}
	}
}
