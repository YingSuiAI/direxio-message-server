package p2p

import (
	"context"
	"reflect"
	"testing"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	agentembeddedmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type executionV2ReadinessPort struct{}

func (executionV2ReadinessPort) Handle(context.Context, string, string, map[string]any) (any, *actionbase.Error) {
	return map[string]any{}, nil
}

type executionV2HTTPInvokePort struct{}

func (executionV2HTTPInvokePort) Invoke(context.Context, string, agentembeddedmodule.ExecutionV2InvokeRequest) (map[string]any, error) {
	return map[string]any{"status": "accepted"}, nil
}

func TestNativeAgentControlRequirementsKeepDangerousToolsCapabilityFenced(t *testing.T) {
	tests := []struct {
		action string
		all    []string
		any    []string
	}{}
	for _, tt := range tests {
		t.Run(tt.action, func(t *testing.T) {
			got, ok := nativeAgentControlRequirements(tt.action)
			if !ok || !reflect.DeepEqual(got.all, tt.all) || !reflect.DeepEqual(got.any, tt.any) {
				t.Fatalf("requirements = %#v, %t; want all=%v any=%v", got, ok, tt.all, tt.any)
			}
		})
	}
	for _, action := range []string{
		"agent.core.confirmations.confirm",
		"agent.core.skills.execute",
	} {
		if _, ok := nativeAgentControlRequirements(action); ok {
			t.Fatalf("unsafe or unsupported Native control action %q became available", action)
		}
	}
}

func TestNativeAgentControlRequirementsExposeOnlySafeExecutionV2Tools(t *testing.T) {
	tests := map[string][]string{
		"agent.execution.v2.projects.analyze":        {"execution.v2.plan"},
		"agent.execution.v2.targets.list":            {"execution.v2"},
		"agent.execution.v2.targets.get":             {"execution.v2"},
		"agent.execution.v2.plans.create":            {"execution.v2.plan"},
		"agent.execution.v2.plans.get":               {"execution.v2.plan"},
		"agent.execution.v2.runs.create":             {"execution.v2.run"},
		"agent.execution.v2.runs.get":                {"execution.v2.run"},
		"agent.execution.v2.runs.events":             {"execution.v2.run"},
		"agent.execution.v2.service_bindings.list":   {"execution.v2.bindings"},
		"agent.execution.v2.service_bindings.get":    {"execution.v2.bindings"},
		"agent.execution.v2.service_bindings.invoke": {"execution.v2.bindings", "execution.v2.transport.http_api"},
	}
	for action, capabilities := range tests {
		requirement, ok := nativeAgentControlRequirements(action)
		if !ok || !reflect.DeepEqual(requirement.all, capabilities) || len(requirement.any) != 0 {
			t.Fatalf("%q requirements = %#v, %t", action, requirement, ok)
		}
	}
	for _, action := range []string{
		"agent.execution.v2.confirmations.confirm",
		"agent.execution.v2.confirmations.reject",
		"agent.execution.v2.runs.cancel",
		"agent.execution.v2.runs.retry",
		"agent.execution.v2.runs.reconcile",
		"agent.execution.v2.plans.revise",
		"agent.execution.v2.targets.import",
		"agent.execution.v2.targets.observe",
	} {
		if requirement, ok := nativeAgentControlRequirements(action); ok {
			t.Fatalf("dangerous action %q unexpectedly supported with requirement %#v", action, requirement)
		}
	}
}

func TestNativeAgentBindingInvokeRequiresReadyHTTPAPITransport(t *testing.T) {
	service := &Service{
		agentEmbedded:            agentembeddedmodule.New(agentembeddedmodule.Config{ExecutionV2: executionV2ReadinessPort{}}),
		executionV2BindingsReady: func() bool { return true },
		executionV2InvokeReady:   func() bool { return false },
	}
	if !service.nativeAgentControlActionReady("agent.execution.v2.service_bindings.get") {
		t.Fatal("ready binding read action was hidden")
	}
	if service.nativeAgentControlActionReady("agent.execution.v2.service_bindings.invoke") {
		t.Fatal("binding read readiness published the deferred invoke tool")
	}
	service.executionV2InvokeReady = func() bool { return true }
	if !service.nativeAgentControlActionReady("agent.execution.v2.service_bindings.invoke") {
		t.Fatal("fully ready HTTP API invoke action stayed hidden")
	}
}

func TestExecutionV2ProductionCapabilityHooksRequireConcreteDependencies(t *testing.T) {
	bindings := agentembeddedmodule.ExecutionV2Config{BindingsReady: func() bool { return true }}
	if executionV2BindingReadsReady(bindings) {
		t.Fatal("binding reads became ready without a durable execution store")
	}
	bindings.Store = new(p2pstorage.DatabaseExecutionStore)
	if !executionV2BindingReadsReady(bindings) {
		t.Fatal("binding reads stayed closed with store and materializer readiness")
	}

	invoke := agentembeddedmodule.ExecutionV2Config{InvokeReady: func() bool { return true }}
	if executionV2HTTPAPIInvokeReady(invoke) {
		t.Fatal("HTTP API capability became ready without an invoke port")
	}
	invoke.Invoke = executionV2HTTPInvokePort{}
	if !executionV2HTTPAPIInvokeReady(invoke) {
		t.Fatal("configured HTTP API invoke port stayed closed")
	}
}
