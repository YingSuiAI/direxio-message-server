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

type modelRoleStore struct {
	storage.ModelProfileStore
	defaults storage.ModelProfileDefaults
	entries  []storage.ModelProfileSyncEntry
}

func (*modelRoleStore) ModelProfileStoreReady() bool { return true }

func (s *modelRoleStore) SyncModelProfilesWithDefaults(
	_ context.Context,
	_, _ string,
	defaults storage.ModelProfileDefaults,
	entries []storage.ModelProfileSyncEntry,
) (storage.ModelProfileSyncResult, error) {
	s.defaults = defaults
	s.entries = entries
	return storage.ModelProfileSyncResult{
		DefaultClientProfileID: defaults.ConversationClientProfileID,
		Defaults:               defaults,
		Profiles: []storage.ModelProfile{{
			ProfileID:            "server-embedding",
			ClientProfileID:      entries[0].ClientProfileID,
			Provider:             entries[0].Provider,
			Model:                entries[0].Model,
			ModelKind:            entries[0].ModelKind,
			Revision:             4,
			CredentialVersion:    2,
			APIKeyConfigured:     true,
			InputModalities:      entries[0].InputModalities,
			ProviderConfig:       entries[0].ProviderConfig,
			ProviderSecretStatus: map[string]bool{},
		}},
	}, nil
}

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
	}, false)
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

func TestCoreModelProfileCompatibilityPreservesRolesDefaultsAndCredentialVersion(t *testing.T) {
	store := &modelRoleStore{}
	handler := New(Config{ModelProfiles: store}).Handlers()["agent.core.model_profiles.sync"]
	result, apiErr := handler(context.Background(), map[string]any{
		"idempotency_key":                     "00000000-0000-4000-8000-000000000001",
		"default_embedding_client_profile_id": "client-embedding",
		"entries": []any{map[string]any{
			"client_profile_id": "client-embedding",
			"provider":          "openai",
			"model":             "text-embedding-3-small",
			"model_kind":        "embedding",
			"api_key":           "write-only",
		}},
	})
	if apiErr != nil {
		t.Fatalf("sync error: %#v", apiErr)
	}
	if store.defaults.EmbeddingClientProfileID != "client-embedding" ||
		len(store.entries) != 1 ||
		store.entries[0].ModelKind != storage.ModelKindEmbedding {
		t.Fatalf("store input = %#v, %#v", store.defaults, store.entries)
	}
	response := result.(map[string]any)
	if response["default_embedding_client_profile_id"] != "client-embedding" {
		t.Fatalf("defaults response = %#v", response)
	}
	profile := response["profiles"].([]any)[0].(map[string]any)
	if profile["model_kind"] != storage.ModelKindEmbedding ||
		profile["credential_version"] != int64(2) ||
		profile["revision"] != int64(4) {
		t.Fatalf("profile response = %#v", profile)
	}
	if _, leaked := profile["api_key"]; leaked {
		t.Fatalf("write-only key leaked: %#v", profile)
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
