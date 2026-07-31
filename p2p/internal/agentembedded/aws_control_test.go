package agentembedded

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	coreaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	"github.com/google/uuid"
)

func TestAWSActionPortCredentialCRUDIsOwnerScopedAndRedacted(t *testing.T) {
	repo := coreaws.NewMemoryRepository()
	sts := &coreaws.FakeSTSProvider{Identity: coreaws.Identity{AccountID: "123456789012", UserARN: "arn:aws:iam::123456789012:user/test", PrincipalID: "AIDA"}}
	service := coreaws.NewService(repo, nil, nil, sts, nil, nil)
	port, err := NewAWSActionPort(func(owner string) (*coreaws.Service, error) {
		if owner != "owner-1" {
			t.Fatalf("resolver owner = %q", owner)
		}
		return service, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	secret := "super-secret-value"
	result, actionErr := port.Handle(ctx, "owner-1", "agent.core.aws.credentials.create", map[string]any{
		"idempotency_key": uuid.NewString(), "name": "prod", "region": "us-east-1", "access_key_id": "AKIA", "secret_access_key": secret,
	})
	if actionErr != nil {
		t.Fatalf("create error: %#v", actionErr)
	}
	encoded := resultString(result)
	if strings.Contains(encoded, secret) {
		t.Fatalf("secret leaked in create response: %s", encoded)
	}
	view := result.(map[string]any)["credential"].(map[string]any)
	id := view["credential_id"].(string)
	revision := view["revision"].(int64)
	testKey := uuid.NewString()
	tested, actionErr := port.Handle(ctx, "owner-1", "agent.core.aws.credentials.test", map[string]any{"credential_id": id, "idempotency_key": testKey, "expected_revision": revision})
	if actionErr != nil || tested == nil || strings.Contains(resultString(tested), secret) {
		t.Fatalf("test = %#v, %#v", tested, actionErr)
	}
	replayed, actionErr := port.Handle(ctx, "owner-1", "agent.core.aws.credentials.test", map[string]any{"credential_id": id, "idempotency_key": testKey, "expected_revision": revision})
	if actionErr != nil || resultString(replayed) != resultString(tested) {
		t.Fatalf("test replay = %#v, %#v", replayed, actionErr)
	}
	if _, actionErr = port.Handle(ctx, "owner-1", "agent.core.aws.credentials.test", map[string]any{"credential_id": id, "idempotency_key": testKey, "expected_revision": int64(2)}); actionErr == nil || actionErr.Code != "idempotency_conflict" {
		t.Fatalf("test replay drift = %#v", actionErr)
	}
	updated, actionErr := port.Handle(ctx, "owner-1", "agent.core.aws.credentials.update", map[string]any{
		"idempotency_key": uuid.NewString(), "credential_id": id, "expected_revision": revision, "name": "prod-2",
	})
	if actionErr != nil || updated == nil {
		t.Fatalf("update = %#v, %#v", updated, actionErr)
	}
	if _, actionErr = port.Handle(ctx, "owner-1", "agent.core.aws.credentials.list", map[string]any{"unexpected": true}); actionErr == nil || actionErr.Code != "unknown_field" {
		t.Fatalf("unknown field = %#v", actionErr)
	}
	if _, actionErr = port.Handle(ctx, "owner-1", "agent.core.aws.plans.get", map[string]any{}); actionErr == nil || actionErr.Code != "aws_action_not_found" {
		t.Fatalf("retired plan action = %#v", actionErr)
	}
	if _, actionErr = port.Handle(ctx, "owner-1", "agent.core.aws.credentials.delete", map[string]any{
		"idempotency_key": uuid.NewString(), "credential_id": id, "expected_revision": int64(2),
	}); actionErr != nil {
		t.Fatalf("delete error: %#v", actionErr)
	}
}

func resultString(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
