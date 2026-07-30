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

func TestEmbeddedDirextalkToolUsesExactControlAllowlist(t *testing.T) {
	for _, name := range []string{"native_agent_schedules_list", "native_agent_schedules_get", "native_agent_schedule_runs_list", "native_agent_schedule_runs_get"} {
		if !embeddedDirextalkTool(name) {
			t.Fatalf("read-only schedule tool %q was rejected", name)
		}
	}
	for _, name := range []string{
		"native_agent_aws_credentials_list",
		"native_agent_deployments_events", "native_agent_schedules_list",
		"native_agent_workload_actual_get", "native_agent_aws_ec2_provisions_plan",
		"native_agent_aws_ec2_provisions_get", "native_agent_aws_ec2_provisions_list",
		"native_agent_aws_ec2_provisions_events", "native_agent_aws_ec2_provisions_create_request",
		"native_agent_aws_ec2_provisions_destroy_request", "native_agent_aws_ec2_geolibre_install_plan", "native_agent_aws_ec2_geolibre_install_request",
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
	} {
		if embeddedDirextalkTool(name) {
			t.Fatalf("unallowlisted tool %q was accepted", name)
		}
	}
}

func TestEC2ControlAliasesResolveOnlyTypedNames(t *testing.T) {
	for _, alias := range []string{
		"agent.core.aws.ec2_provisions.plan", "agent.core.aws.ec2_provisions.get",
		"agent.core.aws.ec2_provisions.list", "agent.core.aws.ec2_provisions.events",
		"agent.core.aws.ec2_provisions.create.request", "agent.core.aws.ec2_provisions.destroy.request",
		"agent.core.aws.ec2_provisions.geolibre_install.plan",
		"agent.core.aws.ec2_provisions.geolibre_install.request",
		"agent.core.workloads.actual.get",
	} {
		if got := nativeToolAlias(alias); got == "" || !embeddedDirextalkTool(got) {
			t.Fatalf("typed control alias %q unavailable: %q", alias, got)
		}
	}
	for _, alias := range []string{
		"agent.core.aws.ec2_provisions.retry", "agent.core.aws.ec2_provisions.confirm",
		"agent.core.aws.ec2_provisions.execute", "agent.core.aws.ec2_provisions.raw",
	} {
		if got := nativeToolAlias(alias); got != "" {
			t.Fatalf("forbidden EC2 alias %q resolved to %q", alias, got)
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
	runtime := New(Config{Tools: []Tool{{Name: "native_agent_workload_operations_get", Available: func() bool { return ready }}}})
	if got := runtime.availableTools(); len(got) != 0 {
		t.Fatalf("unready tools = %#v", got)
	}
	ready = true
	if got := runtime.availableTools(); len(got) != 1 || got[0].Name != "native_agent_workload_operations_get" {
		t.Fatalf("ready tools = %#v", got)
	}
}
