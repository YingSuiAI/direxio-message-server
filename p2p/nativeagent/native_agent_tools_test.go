package nativeagent

import (
	"context"
	"strings"
	"testing"
)

func TestEnabledToolsExposeManagementAndRuntimeToolsWithoutConfirmation(t *testing.T) {
	runtime := &Runtime{}
	for _, tool := range runtime.enabledTools(context.Background(), nil, nil) {
		if strings.HasPrefix(tool.Name, "native_agent_") || strings.HasPrefix(tool.Name, "runtime__") || strings.HasPrefix(tool.Name, "mcp__") {
			t.Fatalf("mutable extension tool leaked: %q", tool.Name)
		}
	}
	if tools := runtime.enabledRuntimeEinoTools(nil, nil); len(tools) != 0 {
		t.Fatalf("runtime Eino tools leaked: %#v", tools)
	}
}

func toolEnabled(tools []Tool, name string) bool {
	for _, tool := range tools {
		if tool.Name == name {
			return true
		}
	}
	return false
}

func TestEnabledToolsAddsExecutionV2AfterUpgrade(t *testing.T) {
	runtime := New(Config{Tools: []Tool{
		{Name: "dirextalk_contacts_list", Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }},
		{Name: "native_agent_execution_v2_projects_analyze", Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }},
		{Name: "native_agent_execution_v2_runs_create", Handler: func(context.Context, map[string]any) (any, error) { return nil, nil }},
	}})
	tools := runtime.enabledTools(context.Background(), map[string]any{"enabled_tools": []any{"dirextalk_contacts_list"}}, nil)
	for _, name := range []string{"dirextalk_contacts_list", "native_agent_execution_v2_projects_analyze", "native_agent_execution_v2_runs_create"} {
		if !toolEnabled(tools, name) {
			t.Fatalf("upgraded config omitted compiled V2 tool %q: %#v", name, tools)
		}
	}
}

func TestEmbeddedDirextalkToolUsesExactControlAllowlist(t *testing.T) {
	for _, name := range []string{"native_agent_schedules_list", "native_agent_schedules_get", "native_agent_schedule_runs_list", "native_agent_schedule_runs_get"} {
		if !embeddedDirextalkTool(name) {
			t.Fatalf("read-only schedule tool %q was rejected", name)
		}
	}
	for _, name := range []string{
		"native_agent_aws_credentials_list", "native_agent_aws_credentials_test",
		"native_agent_execution_v2_projects_analyze",
		"native_agent_execution_v2_targets_list", "native_agent_execution_v2_targets_get", "native_agent_execution_v2_targets_reserve",
		"native_agent_execution_v2_plans_create", "native_agent_execution_v2_plans_get",
		"native_agent_execution_v2_runs_create", "native_agent_execution_v2_runs_get",
		"native_agent_execution_v2_runs_status", "native_agent_execution_v2_runs_events",
		"native_agent_execution_v2_service_bindings_list", "native_agent_execution_v2_service_bindings_get",
		"native_agent_execution_v2_service_bindings_invoke",
	} {
		if !embeddedDirextalkTool(name) {
			t.Fatalf("allowlisted tool %q was rejected", name)
		}
	}
	for _, name := range []string{
		"native_agent_workloads_list", "native_agent_workloads_get", "native_agent_workloads_quote",
		"native_agent_workloads_plan", "native_agent_workloads_apply", "native_agent_workloads_destroy",
		"native_agent_workloads_confirm", "native_agent_workloads_create", "native_agent_workloads_delete", "native_agent_workload_operations_confirm",
		"native_agent_schedules_confirm",
		"native_agent_deployments_destroy", "native_agent_aws_credentials_delete",
		"native_agent_aws_ec2_provisions_retry", "native_agent_aws_ec2_provisions_confirm",
		"native_agent_execution_v2_confirmations_get", "native_agent_execution_v2_confirmations_confirm", "native_agent_execution_v2_confirmations_reject",
		"native_agent_execution_v2_plans_revise", "native_agent_execution_v2_runs_cancel", "native_agent_execution_v2_runs_retry", "native_agent_execution_v2_runs_reconcile",
		"native_agent_execution_v2_ssm_send_command", "native_agent_execution_v2_ssh_shell", "native_agent_execution_v2_aws_sdk", "native_agent_execution_v2_url_invoke",
	} {
		if embeddedDirextalkTool(name) {
			t.Fatalf("unallowlisted tool %q was accepted", name)
		}
	}
}

func TestRetiredProvisionAliasesAreUnavailable(t *testing.T) {
	for _, alias := range []string{"agent.core.aws.ec2_provisions.plan", "agent.core.aws.ec2_provisions.create.request", "agent.core.workloads.actual.get"} {
		if got := nativeToolAlias(alias); got != "" {
			t.Fatalf("retired alias %q resolved to %q", alias, got)
		}
	}
}

func TestExecutionV2AliasesRequireExactPublicActionNames(t *testing.T) {
	want := map[string]string{
		"agent.execution.v2.projects.analyze":        "native_agent_execution_v2_projects_analyze",
		"agent.execution.v2.targets.list":            "native_agent_execution_v2_targets_list",
		"agent.execution.v2.targets.get":             "native_agent_execution_v2_targets_get",
		"agent.execution.v2.targets.reserve":         "native_agent_execution_v2_targets_reserve",
		"agent.execution.v2.plans.create":            "native_agent_execution_v2_plans_create",
		"agent.execution.v2.plans.get":               "native_agent_execution_v2_plans_get",
		"agent.execution.v2.runs.create":             "native_agent_execution_v2_runs_create",
		"agent.execution.v2.runs.get":                "native_agent_execution_v2_runs_get",
		"agent.execution.v2.runs.events":             "native_agent_execution_v2_runs_events",
		"agent.execution.v2.service_bindings.list":   "native_agent_execution_v2_service_bindings_list",
		"agent.execution.v2.service_bindings.get":    "native_agent_execution_v2_service_bindings_get",
		"agent.execution.v2.service_bindings.invoke": "native_agent_execution_v2_service_bindings_invoke",
	}
	for action, tool := range want {
		if got := nativeToolAlias(action); got != tool {
			t.Fatalf("alias %q = %q, want %q", action, got, tool)
		}
	}
	for _, action := range []string{
		"execution.v2.runs.get", "agent.execution.runs.get", "agent.execution.v2.runs.status",
		"agent.execution.v2.confirmations.get", "agent.execution.v2.confirmations.confirm",
		"agent.execution.v2.plans.revise", "agent.execution.v2.runs.cancel", "agent.execution.v2.runs.retry", "agent.execution.v2.runs.reconcile",
	} {
		if got := nativeToolAlias(action); got != "" {
			t.Fatalf("unsafe or non-public alias %q resolved to %q", action, got)
		}
	}
}

func TestScheduleMutationAliasesAreUnavailableToNativeAgent(t *testing.T) {
	for _, alias := range []string{
		"agent_schedules_create", "agent_schedules_update", "agent_schedules_enable",
		"agent_schedules_disable", "agent_schedules_delete", "agent_schedules_run_now", "agent_schedules_confirm",
	} {
		if got := nativeToolAlias(alias); got != "" {
			t.Fatalf("schedule mutation alias %q resolved to %q", alias, got)
		}
	}
	for _, alias := range []string{"agent_schedules_list", "agent_schedules_get", "agent_schedule_runs_list", "agent_schedule_runs_get"} {
		if got := nativeToolAlias(alias); got == "" || !embeddedDirextalkTool(got) {
			t.Fatalf("read-only schedule alias %q unavailable: %q", alias, got)
		}
	}
}

func TestAvailableToolsFiltersDynamicReadiness(t *testing.T) {
	ready := false
	runtime := New(Config{Tools: []Tool{{Name: "native_agent_aws_credentials_list", Available: func() bool { return ready }}}})
	if got := runtime.availableTools(); len(got) != 0 {
		t.Fatalf("unready tools = %#v", got)
	}
	ready = true
	if got := runtime.availableTools(); len(got) != 1 || got[0].Name != "native_agent_aws_credentials_list" {
		t.Fatalf("ready tools = %#v", got)
	}
}
