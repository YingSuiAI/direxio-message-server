package agentembedded

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

type executionSecretPortStub struct {
	created storage.ExecutionSecretCreateRequest
	item    storage.ExecutionSecretMetadata
}

func (s *executionSecretPortStub) CreateExecutionSecret(_ context.Context, in storage.ExecutionSecretCreateRequest) (storage.ExecutionSecretMetadata, error) {
	s.created = in
	return s.item, nil
}
func (s *executionSecretPortStub) GetExecutionSecret(context.Context, string, string, uint64) (storage.ExecutionSecretMetadata, error) {
	return s.item, nil
}
func (s *executionSecretPortStub) ListExecutionSecrets(context.Context, string, string, int) (storage.ExecutionSecretPage, error) {
	return storage.ExecutionSecretPage{Items: []storage.ExecutionSecretMetadata{s.item}}, nil
}
func (s *executionSecretPortStub) RevokeExecutionSecret(context.Context, storage.ExecutionSecretRevokeRequest) (storage.ExecutionSecretMetadata, error) {
	item := s.item
	item.Revision++
	item.Status = "revoked"
	return item, nil
}

func TestExecutionV2SecretActionsAreDedicatedAndNeverProjectPlaintext(t *testing.T) {
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	stub := &executionSecretPortStub{item: storage.ExecutionSecretMetadata{SecretRef: "11111111-1111-4111-8111-111111111111", Revision: 1, Purpose: coreexecution.AISecretPurposeProviderAPIKey, Provider: "openai", BindingDigest: coreexecution.Digest(strings.Repeat("a", 64)), Status: "active", CreatedAt: now, UpdatedAt: now}}
	port := NewExecutionV2ActionPort(ExecutionV2Config{Ready: func() bool { return false }, Secrets: stub, SecretsReady: func() bool { return true }})
	result, actionErr := port.Handle(context.Background(), "@owner:example.test", "agent.execution.v2.secrets.create", map[string]any{"provider": "openai", "purpose": coreexecution.AISecretPurposeProviderAPIKey, "value": "sk-test-only-value", "idempotency_key": "22222222-2222-4222-8222-222222222222"})
	if actionErr != nil {
		t.Fatal(actionErr)
	}
	if stub.created.Value != "sk-test-only-value" || stub.created.OwnerID != "@owner:example.test" {
		t.Fatalf("dedicated mutation did not receive owner-scoped value: %#v", stub.created)
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "sk-test-only-value") || strings.Contains(string(raw), `"value"`) {
		t.Fatalf("secret response leaked value: %s", raw)
	}
	secret := result.(map[string]any)["secret"].(map[string]any)
	if secret["secret_ref"] != stub.item.SecretRef || secret["binding_digest"] != string(stub.item.BindingDigest) {
		t.Fatalf("safe metadata missing: %#v", secret)
	}
}

func TestExecutionV2AIConfigurationRejectsMixedModesAndUnknownFields(t *testing.T) {
	validAPI := map[string]any{"mode": "api_key", "provider": "openai", "secret_ref": "11111111-1111-4111-8111-111111111111", "secret_revision": 1, "secret_purpose": coreexecution.AISecretPurposeProviderAPIKey, "secret_binding_digest": strings.Repeat("a", 64)}
	if got, err := parseExecutionV2AIConfiguration(validAPI); err != nil || got.Mode != coreexecution.AIAuthModeAPIKey {
		t.Fatalf("valid api config=%#v err=%v", got, err)
	}
	validAuth := map[string]any{"mode": "auth_gate", "provider": "anthropic", "status": coreexecution.AIExternalAuthPending}
	if got, err := parseExecutionV2AIConfiguration(validAuth); err != nil || got.Mode != coreexecution.AIAuthModeAuthGate {
		t.Fatalf("valid auth config=%#v err=%v", got, err)
	}
	for name, value := range map[string]map[string]any{
		"api status":         {"mode": "api_key", "provider": "openai", "status": coreexecution.AIExternalAuthPending},
		"auth secret":        {"mode": "auth_gate", "provider": "anthropic", "status": coreexecution.AIExternalAuthPending, "secret_ref": "11111111-1111-4111-8111-111111111111"},
		"unknown field":      {"mode": "auth_gate", "provider": "anthropic", "status": coreexecution.AIExternalAuthPending, "token": "x"},
		"completed external": {"mode": "auth_gate", "provider": "anthropic", "status": "completed"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseExecutionV2AIConfiguration(value); err == nil {
				t.Fatal("invalid mixed AI configuration accepted")
			}
		})
	}
}
