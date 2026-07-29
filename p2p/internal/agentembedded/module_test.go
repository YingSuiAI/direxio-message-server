package agentembedded

import (
	"context"
	"testing"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type readyModelProfileStore struct {
	storage.ModelProfileStore
}

func (readyModelProfileStore) ModelProfileStoreReady() bool { return true }

func TestHandlersRegisterEmbeddedAgentSurface(t *testing.T) {
	h := New(Config{}).Handlers()
	for _, name := range []string{
		"agent.backends.get", "agent.core.status.get", "agent.core.model_profiles.list",
		"agent.core.tasks.events", "agent.core.schedules.pause",
		"agent.core.confirmations.reject", "agent.core.mcp.execute",
		"agent.core.skills.execute", "agent.core.aws.plans.get", "agent.core.workloads.apply",
		"agent.core.dashboard.get", "agent.core.deployments.events",
	} {
		if h[name] == nil {
			t.Fatalf("missing handler %q", name)
		}
	}
}

func TestHandlersDoNotClaimLegacyEmbeddedScheduleAliases(t *testing.T) {
	h := New(Config{}).Handlers()
	for _, name := range []string{
		"agent.schedules.create", "agent.schedules.list", "agent.schedules.run_now", "agent.schedule_runs.list",
		"agent.model_profiles.sync", "agent.model_profiles.list", "agent.model_profiles.get", "agent.model_profiles.delete",
	} {
		if _, ok := h[name]; ok {
			t.Fatalf("handler %q overlaps embedded-schedules module", name)
		}
	}
}

func TestUnavailableLocalExecutionIsStableAndSideEffectFree(t *testing.T) {
	called := false
	port := ActionPortFunc(func(context.Context, string, string, map[string]any) (any, *actionbase.Error) {
		called = true
		return map[string]any{"ok": true}, nil
	})
	result, err := New(Config{Skills: port}).Handlers()["agent.core.skills.execute"](context.Background(), map[string]any{"skill_id": "x"})
	if result != nil || err == nil || err.Code != "agent_embedded_unavailable" {
		t.Fatalf("skills execute = %#v, %#v", result, err)
	}
	if err.Status != 412 {
		t.Fatalf("status = %d, want 412", err.Status)
	}
	if called {
		t.Fatal("skill port was called")
	}
}

func TestCoreRunnerWorkloadPlanIsRejectedBeforePersistence(t *testing.T) {
	plan, err := workloadPlanInput(map[string]any{
		"idempotency_key": "00000000-0000-4000-8000-000000000001",
		"summary":         "must not persist",
		"artifact":        "sha256:artifact",
		"source":          "test",
		"target_kind":     "CORE_RUNNER",
	})
	if err == nil || err.Status != 400 || plan.TargetKind != "" {
		t.Fatalf("CORE_RUNNER plan = %#v, %#v", plan, err)
	}
}

func TestMCPSecretInputsUseTheWriteOnlyWireField(t *testing.T) {
	inputs, err := secretInputsParam([]any{map[string]any{
		"reference_id": "00000000-0000-4000-8000-000000000001",
		"purpose":      "mcp_credential",
		"secret_value": "write-only",
	}})
	if err != nil || len(inputs) != 1 || inputs[0].Value != "write-only" {
		t.Fatalf("secret inputs = %#v, %#v", inputs, err)
	}
	if inputs, err = secretInputsParam([]any{map[string]any{
		"reference_id": "00000000-0000-4000-8000-000000000001",
		"purpose":      "mcp_credential",
		"value":        "not-a-public-wire-field",
	}}); err == nil || inputs != nil {
		t.Fatalf("legacy plaintext field accepted = %#v, %#v", inputs, err)
	}
}

func TestBackendsCapabilitiesOnlyExposeReadyEmbeddedPorts(t *testing.T) {
	result, err := New(Config{}).Handlers()["agent.backends.get"](context.Background(), nil)
	if err != nil {
		t.Fatalf("backends error: %#v", err)
	}
	embedded := result.(map[string]any)["embedded"].(map[string]any)
	if got := embedded["capabilities"].([]string); len(got) != 0 {
		t.Fatalf("capabilities = %#v, want empty without ports", got)
	}
}

func TestBackendsPreserveModelRoleMemoryAndVoiceCapabilities(t *testing.T) {
	ready := map[string]bool{
		"model_profiles.server": true,
		"model_roles.server":    true,
		"memory.server":         true,
		"voice.server":          true,
	}
	result, err := New(Config{
		ModelProfiles: readyModelProfileStore{},
		CapabilityReady: func(capability string) bool {
			return ready[capability]
		},
	}).Handlers()["agent.backends.get"](context.Background(), nil)
	if err != nil {
		t.Fatalf("backends error: %#v", err)
	}
	embedded := result.(map[string]any)["embedded"].(map[string]any)
	got := embedded["capabilities"].([]string)
	for _, capability := range []string{
		"model_profiles.server",
		"model_roles.server",
		"memory.server",
		"voice.server",
	} {
		found := false
		for _, value := range got {
			if value == capability {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("capability %q missing from %#v", capability, got)
		}
	}
}

func TestCapabilityGateRejectsPortWhenReadinessTurnsOff(t *testing.T) {
	called := false
	port := ActionPortFunc(func(context.Context, string, string, map[string]any) (any, *actionbase.Error) {
		called = true
		return map[string]any{"ok": true}, nil
	})
	m := New(Config{OwnerID: func() string { return "@owner:example" }, AWS: port, CapabilityReady: func(string) bool { return false }})
	result, err := m.Handlers()["agent.core.aws.plans.get"](context.Background(), map[string]any{"plan_id": "00000000-0000-0000-0000-000000000001"})
	if result != nil || err == nil || err.Code != "agent_embedded_unavailable" || err.Status != 412 {
		t.Fatalf("gated aws action = %#v, %#v", result, err)
	}
	if called {
		t.Fatal("unready AWS port was called")
	}
}
