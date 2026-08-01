package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/google/uuid"
)

type controlCall struct {
	action string
	params map[string]any
}

type recordingControlInvoker struct {
	ready map[string]bool
	calls []controlCall
}

func (r *recordingControlInvoker) Available(action string) bool { return r.ready[action] }
func (r *recordingControlInvoker) Invoke(_ context.Context, action string, params map[string]any) (any, error) {
	r.calls = append(r.calls, controlCall{action: action, params: params})
	return map[string]any{"ok": true}, nil
}

func controlToolByName(t *testing.T, tools []nativeagent.Tool, name string) nativeagent.Tool {
	t.Helper()
	for _, tool := range tools {
		if tool.Name == name {
			return tool
		}
	}
	t.Fatalf("missing control tool %q", name)
	return nativeagent.Tool{}
}

func TestControlToolsExposeOnlyBoundedFirstPartySurface(t *testing.T) {
	tools := ControlTools(ControlInvokerFunc(func(context.Context, string, map[string]any) (any, error) {
		return map[string]any{"ok": true}, nil
	}))
	want := map[string]bool{
		"native_agent_aws_credentials_list":                 false,
		"native_agent_aws_credentials_test":                 true,
		"native_agent_execution_v2_projects_analyze":        true,
		"native_agent_execution_v2_targets_list":            false,
		"native_agent_execution_v2_targets_get":             false,
		"native_agent_execution_v2_targets_reserve":         true,
		"native_agent_execution_v2_plans_create":            true,
		"native_agent_execution_v2_plans_get":               false,
		"native_agent_execution_v2_runs_create":             true,
		"native_agent_execution_v2_runs_get":                false,
		"native_agent_execution_v2_runs_status":             false,
		"native_agent_execution_v2_runs_events":             false,
		"native_agent_execution_v2_service_bindings_list":   false,
		"native_agent_execution_v2_service_bindings_get":    false,
		"native_agent_execution_v2_service_bindings_invoke": true,
	}
	if len(tools) != len(want) {
		t.Fatalf("control tool count = %d, want %d", len(tools), len(want))
	}
	for _, tool := range tools {
		write, ok := want[tool.Name]
		if !ok {
			t.Fatalf("unexpected control tool %q", tool.Name)
		}
		if tool.Write != write {
			t.Fatalf("tool %q write = %v, want %v", tool.Name, tool.Write, write)
		}
		delete(want, tool.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing control tools: %#v", want)
	}
}

func TestControlToolsExcludeDangerousExecutionActions(t *testing.T) {
	tools := ControlTools(ControlInvokerFunc(func(context.Context, string, map[string]any) (any, error) { return nil, nil }))
	for _, tool := range tools {
		name := strings.ToLower(tool.Name)
		for _, forbidden := range []string{
			"confirm", "reject", "reconcile", "retry", "cancel", "plans_revise",
			"raw_ssm", "ssh_shell", "aws_sdk", "arbitrary_url", "geolibre", "workload", "provision",
		} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("dangerous or retired tool leaked into Native Agent tools: %q", tool.Name)
			}
		}
	}
}

func TestControlToolsUseExactPerActionDynamicReadiness(t *testing.T) {
	invoker := &recordingControlInvoker{ready: map[string]bool{
		"agent.execution.v2.plans.get": true,
		"agent.execution.v2.runs.get":  true,
	}}
	tools := ControlTools(invoker)
	if !controlToolByName(t, tools, "native_agent_execution_v2_plans_get").Available() {
		t.Fatal("ready exact plan action was hidden")
	}
	if !controlToolByName(t, tools, "native_agent_execution_v2_runs_get").Available() ||
		!controlToolByName(t, tools, "native_agent_execution_v2_runs_status").Available() {
		t.Fatal("runs.get readiness did not enable get/status tools")
	}
	if controlToolByName(t, tools, "native_agent_execution_v2_plans_create").Available() {
		t.Fatal("unready plan mutation was advertised")
	}
	if controlToolByName(t, tools, "native_agent_execution_v2_service_bindings_get").Available() {
		t.Fatal("unready binding action was advertised")
	}
}

func TestTargetReadToolsAreStrictReadOnlyControlCalls(t *testing.T) {
	targetID := uuid.NewString()
	invoker := &recordingControlInvoker{ready: map[string]bool{
		"agent.execution.v2.targets.list": true,
		"agent.execution.v2.targets.get":  true,
	}}
	tools := ControlTools(invoker)
	list := controlToolByName(t, tools, "native_agent_execution_v2_targets_list")
	get := controlToolByName(t, tools, "native_agent_execution_v2_targets_get")
	if list.Write || get.Write {
		t.Fatal("target discovery tools must be read-only")
	}
	if _, err := list.Handler(context.Background(), map[string]any{"page_size": 10, "page_token": "cursor"}); err != nil {
		t.Fatalf("list targets: %v", err)
	}
	if _, err := get.Handler(context.Background(), map[string]any{"target_id": targetID, "revision": 2}); err != nil {
		t.Fatalf("get target: %v", err)
	}
	if len(invoker.calls) != 2 || invoker.calls[0].action != "agent.execution.v2.targets.list" || invoker.calls[1].action != "agent.execution.v2.targets.get" {
		t.Fatalf("calls = %#v", invoker.calls)
	}
	if _, ok := invoker.calls[0].params["idempotency_key"]; ok {
		t.Fatal("read-only target list received an idempotency key")
	}
	if _, err := list.Handler(context.Background(), map[string]any{"unknown": true}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown target list field accepted: %v", err)
	}
	if _, err := get.Handler(context.Background(), map[string]any{"target_id": targetID, "revision": 0}); err == nil || !strings.Contains(err.Error(), "positive revision") {
		t.Fatalf("invalid target revision accepted: %v", err)
	}
	if len(invoker.calls) != 2 {
		t.Fatalf("rejected target request reached invoker: %#v", invoker.calls)
	}
}

func TestTargetReservationCreatesOnlyServerIdempotentReservation(t *testing.T) {
	invoker := &recordingControlInvoker{ready: map[string]bool{"agent.execution.v2.targets.reserve": true}}
	tool := controlToolByName(t, ControlTools(invoker), "native_agent_execution_v2_targets_reserve")
	ctx := nativeagent.WithRequestContextIntent(context.Background(), "@owner:example", "conversation", "turn-reserve", "")
	credentialID := uuid.NewString()
	if _, err := tool.Handler(ctx, map[string]any{
		"credential_id": credentialID, "credential_revision": 2, "instance_type": "t3.medium", "volume_gib": 40,
	}); err != nil {
		t.Fatalf("reserve target: %v", err)
	}
	if len(invoker.calls) != 1 || invoker.calls[0].action != "agent.execution.v2.targets.reserve" {
		t.Fatalf("calls = %#v", invoker.calls)
	}
	request := invoker.calls[0].params
	if request["credential_id"] != credentialID || request["instance_type"] != "t3.medium" || request["volume_gib"] != 40 {
		t.Fatalf("reservation request = %#v", request)
	}
	if idempotency, ok := request["idempotency_key"].(string); !ok || uuid.Validate(idempotency) != nil {
		t.Fatalf("reservation idempotency = %#v", request["idempotency_key"])
	}
	for _, invalid := range []map[string]any{
		{"credential_id": credentialID, "credential_revision": 2, "instance_type": "invalid", "volume_gib": 40},
		{"credential_id": credentialID, "credential_revision": 2, "instance_type": "t3.medium", "volume_gib": 4},
	} {
		if _, err := tool.Handler(ctx, invalid); err == nil {
			t.Fatalf("invalid reservation accepted: %#v", invalid)
		}
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("invalid reservation reached invoker: %#v", invoker.calls)
	}
}

func TestProjectAnalysisAllocatesStableProjectIdentityWhenOmitted(t *testing.T) {
	invoker := &recordingControlInvoker{ready: map[string]bool{"agent.execution.v2.projects.analyze": true}}
	tool := controlToolByName(t, ControlTools(invoker), "native_agent_execution_v2_projects_analyze")
	ctx := nativeagent.WithRequestContextIntent(context.Background(), "@owner:example", "conversation", "turn-analyze", "")
	params := map[string]any{"source": map[string]any{
		"kind": "oci_image", "location": "registry.example/app@sha256:" + strings.Repeat("a", 64), "immutable": true,
	}}
	if _, err := tool.Handler(ctx, params); err != nil {
		t.Fatalf("analyze project: %v", err)
	}
	if _, err := tool.Handler(ctx, params); err != nil {
		t.Fatalf("repeat analyze project: %v", err)
	}
	if len(invoker.calls) != 2 {
		t.Fatalf("calls = %#v", invoker.calls)
	}
	first, ok := invoker.calls[0].params["project_id"].(string)
	if !ok || uuid.Validate(first) != nil || invoker.calls[1].params["project_id"] != first {
		t.Fatalf("project identities = %#v / %#v", invoker.calls[0].params["project_id"], invoker.calls[1].params["project_id"])
	}
	if invoker.calls[0].params["idempotency_key"] != invoker.calls[1].params["idempotency_key"] {
		t.Fatal("same turn did not retain idempotent analysis identity")
	}
}

func TestControlMutationInjectsTurnScopedUUIDAndRejectsModelIdempotency(t *testing.T) {
	invoker := &recordingControlInvoker{ready: map[string]bool{"agent.execution.v2.runs.create": true}}
	tool := controlToolByName(t, ControlTools(invoker), "native_agent_execution_v2_runs_create")
	ctx := nativeagent.WithRequestContextIntent(context.Background(), "@owner:example", "conversation", "turn-1", "")
	planID := uuid.NewString()
	if _, err := tool.Handler(ctx, map[string]any{"plan_id": planID, "plan_revision": 7, "operation": "deploy"}); err != nil {
		t.Fatalf("invoke run tool: %v", err)
	}
	if len(invoker.calls) != 1 || invoker.calls[0].action != "agent.execution.v2.runs.create" {
		t.Fatalf("calls = %#v", invoker.calls)
	}
	request := invoker.calls[0].params
	if request["plan_id"] != planID || request["plan_revision"] != 7 || request["operation"] != "deploy" {
		t.Fatalf("projected request = %#v", request)
	}
	idempotency, ok := request["idempotency_key"].(string)
	if !ok || uuid.Validate(idempotency) != nil {
		t.Fatalf("server idempotency key = %#v", request["idempotency_key"])
	}
	if _, err := tool.Handler(ctx, map[string]any{"plan_id": planID, "plan_revision": 7, "idempotency_key": uuid.NewString()}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("model idempotency key was accepted: %v", err)
	}
	for _, operation := range []string{"upgrade", "repair", "destroy", "rollback"} {
		if _, err := tool.Handler(ctx, map[string]any{"plan_id": planID, "plan_revision": 7, "operation": operation}); err == nil || !strings.Contains(err.Error(), "unsupported operation") {
			t.Fatalf("unready %s operation was advertised/accepted: %v", operation, err)
		}
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("rejected request reached invoker: %#v", invoker.calls)
	}
}

func TestControlWritesRequireAuthenticatedTurnContext(t *testing.T) {
	invoker := &recordingControlInvoker{ready: map[string]bool{"agent.execution.v2.plans.create": true}}
	tool := controlToolByName(t, ControlTools(invoker), "native_agent_execution_v2_plans_create")
	params := map[string]any{
		"project_id": uuid.NewString(), "analysis_id": uuid.NewString(), "intent": "container.deploy",
		"recipe_id": "container-service-deploy", "target_id": uuid.NewString(), "target_revision": 2, "purpose": "service",
	}
	if _, err := tool.Handler(context.Background(), params); err == nil || !strings.Contains(err.Error(), "owner, conversation, and turn") {
		t.Fatalf("ownerless write error = %v", err)
	}
	if len(invoker.calls) != 0 {
		t.Fatalf("ownerless write reached invoker: %#v", invoker.calls)
	}
}

func TestPlanToolCarriesReferenceOnlyAIAuthenticationChoice(t *testing.T) {
	invoker := &recordingControlInvoker{ready: map[string]bool{"agent.execution.v2.plans.create": true}}
	tool := controlToolByName(t, ControlTools(invoker), "native_agent_execution_v2_plans_create")
	ctx := nativeagent.WithRequestContextIntent(context.Background(), "@owner:example", "conversation", "turn-ai", "")
	base := map[string]any{
		"project_id": uuid.NewString(), "analysis_id": uuid.NewString(), "intent": "ai.deploy",
		"recipe_id": "source-build-systemd", "target_id": uuid.NewString(), "target_revision": 2, "purpose": "service",
	}
	base["ai_configuration"] = map[string]any{"mode": "auth_gate", "provider": "openai", "status": "pending_external_auth"}
	if _, err := tool.Handler(ctx, base); err != nil {
		t.Fatalf("auth-gate plan: %v", err)
	}
	if got := invoker.calls[0].params["ai_configuration"].(map[string]any)["mode"]; got != "auth_gate" {
		t.Fatalf("ai_configuration = %#v", invoker.calls[0].params["ai_configuration"])
	}
	base["ai_configuration"] = map[string]any{"mode": "api_key", "provider": "openrouter", "api_key": "plaintext"}
	if _, err := tool.Handler(ctx, base); err == nil || !strings.Contains(err.Error(), "forbidden secret field") {
		t.Fatalf("plaintext API key accepted: %v", err)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("secret-bearing plan reached invoker: %#v", invoker.calls)
	}
}

func TestServiceBindingInvokePreservesRevisionAndRejectsDirectSecrets(t *testing.T) {
	invoker := &recordingControlInvoker{ready: map[string]bool{"agent.execution.v2.service_bindings.invoke": true}}
	tool := controlToolByName(t, ControlTools(invoker), "native_agent_execution_v2_service_bindings_invoke")
	ctx := nativeagent.WithRequestContextIntent(context.Background(), "@owner:example", "conversation", "turn-2", "")
	bindingID := uuid.NewString()
	if _, err := tool.Handler(ctx, map[string]any{"binding_id": bindingID, "operation": "search", "expected_revision": 3, "input": map[string]any{"query": "hello"}}); err != nil {
		t.Fatalf("invoke binding tool: %v", err)
	}
	if got := invoker.calls[0].params["expected_revision"]; got != 3 {
		t.Fatalf("expected_revision = %#v", got)
	}
	if _, err := tool.Handler(ctx, map[string]any{"binding_id": bindingID, "operation": "search", "expected_revision": 3, "input": map[string]any{"api_key": "plaintext"}}); err == nil || !strings.Contains(err.Error(), "forbidden secret field") {
		t.Fatalf("direct secret error = %v", err)
	}
	if len(invoker.calls) != 1 {
		t.Fatalf("secret-bearing request reached invoker: %#v", invoker.calls)
	}
}

func TestNativeAgentServiceBindingInvokeRedactsUnexpectedAdapterMarkers(t *testing.T) {
	tool := controlToolByName(t, ControlTools(ControlInvokerFunc(func(context.Context, string, map[string]any) (any, error) {
		return map[string]any{"result": map[string]any{"detail": []any{"Bearer never-return", "Basic bmV2ZXI6cmV0dXJu"}}}, nil
	})), "native_agent_execution_v2_service_bindings_invoke")
	ctx := nativeagent.WithRequestContextIntent(context.Background(), "@owner:example", "conversation", "turn-redaction", "")
	result, err := tool.Handler(ctx, map[string]any{
		"binding_id": uuid.NewString(), "operation": "status", "expected_revision": 1, "input": map[string]any{},
	})
	if err != nil || strings.Contains(fmt.Sprint(result), "never-return") || strings.Contains(fmt.Sprint(result), "bmV2ZXI6cmV0dXJu") {
		t.Fatalf("native invoke result=%#v err=%v", result, err)
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

func TestRedactNativeControlResultRemovesExecutionOwnerAndSecretMaterial(t *testing.T) {
	type typedPlanResult struct {
		OwnerID string `json:"owner_id"`
		URI     string `json:"uri"`
		Digest  string `json:"digest"`
	}
	result := redactNativeControlResult(map[string]any{
		"OwnerID": "@owner:example", "operation": map[string]any{
			"desired_plan": map[string]any{"command_steps": []any{"rm -rf"}, "image_uri": "private"},
			"confirmation": map[string]any{"binding": map[string]any{"selected_command": []any{"secret"}, "content_digest": "a"}},
		},
		"typed_target":  map[string]any{"labels": map[string]string{"owner": "digest"}, "instance_id": "i-1"},
		"result":        map[string]any{"access_token": "plaintext", "endpoint": "https://service.example"},
		"typed_plan":    typedPlanResult{OwnerID: "@owner:example", URI: "file:///private", Digest: strings.Repeat("a", 64)},
		"adapter_error": map[string]any{"detail": []any{"Bearer never-return", "Basic bmV2ZXI6cmV0dXJu"}},
	})
	serialized := fmt.Sprint(result)
	for _, forbidden := range []string{"OwnerID", "owner_id", "desired_plan", "command_steps", "image_uri", "selected_command", "labels", "rm -rf", "private", "secret", "access_token", "plaintext", "file:///", "never-return", "bmV2ZXI6cmV0dXJu"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("redacted result retained %q: %s", forbidden, serialized)
		}
	}
	if !strings.Contains(serialized, "https://service.example") {
		t.Fatalf("redaction removed safe service endpoint: %s", serialized)
	}
}

func TestControlToolSchemasDoNotExposeModelControlledIdempotency(t *testing.T) {
	tools := ControlTools(ControlInvokerFunc(func(context.Context, string, map[string]any) (any, error) { return nil, nil }))
	for _, tool := range tools {
		if !tool.Write {
			continue
		}
		properties, _ := tool.Parameters["properties"].(map[string]any)
		if _, ok := properties["idempotency_key"]; ok {
			t.Fatalf("write tool %q exposes model-controlled idempotency", tool.Name)
		}
		if additional, ok := tool.Parameters["additionalProperties"].(bool); !ok || additional {
			t.Fatalf("write tool %q is not top-level strict: %#v", tool.Name, tool.Parameters)
		}
	}
}
