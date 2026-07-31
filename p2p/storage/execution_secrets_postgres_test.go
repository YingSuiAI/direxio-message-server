package storage

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
)

func executionSecretTestEnveloper(t *testing.T) *AgentSecretEnveloper {
	t.Helper()
	ring, err := LoadOrCreateAgentSecretKeyring(filepath.Join(t.TempDir(), "agent-secrets.json"))
	if err != nil {
		t.Fatal(err)
	}
	enveloper, err := NewAgentSecretEnveloper(ring)
	if err != nil {
		t.Fatal(err)
	}
	return enveloper
}

func TestExecutionSecretsPostgresEncryptedReplayRevokeAndOwnerFence(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)
	database := openExecutionV2Schema(t)
	store := NewDatabaseExecutionSecretStore(database.DB(), executionSecretTestEnveloper(t), func() time.Time { return now })
	owner := "@execution-secret:example.test"
	request := ExecutionSecretCreateRequest{OwnerID: owner, Provider: "openai", Purpose: coreexecution.AISecretPurposeProviderAPIKey, Value: "sk-only-in-encrypted-envelope", IdempotencyID: "11111111-1111-4111-8111-111111111111"}
	created, err := store.CreateExecutionSecret(ctx, request)
	if err != nil || created.Revision != 1 || created.Status != "active" || !created.BindingDigest.Valid() {
		t.Fatalf("created=%+v err=%v", created, err)
	}
	raw, _ := json.Marshal(created)
	if strings.Contains(string(raw), request.Value) || strings.Contains(string(raw), `"value"`) {
		t.Fatalf("metadata leaked plaintext: %s", raw)
	}
	var ciphertext []byte
	if err := database.DB().QueryRowContext(ctx, `SELECT ciphertext FROM p2p_agent_secrets WHERE secret_domain='execution' AND owner_id=$1 AND entity_id=$2`, owner, created.SecretRef).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ciphertext), request.Value) {
		t.Fatal("ciphertext contains plaintext")
	}
	var usageCount int
	if err := database.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM p2p_agent_secret_key_usage WHERE secret_domain='execution' AND owner_id=$1 AND entity_id=$2 AND secret_revision=1`, owner, created.SecretRef).Scan(&usageCount); err != nil || usageCount != 1 {
		t.Fatalf("secret key usage count=%d err=%v", usageCount, err)
	}
	ref := coreexecution.CredentialRef{Ref: created.SecretRef, Purpose: created.Purpose, Revision: created.Revision, BindingDigest: created.BindingDigest}
	opened, err := store.OpenExecutionSecret(ctx, owner, ref)
	if err != nil || string(opened) != request.Value {
		t.Fatalf("opened=%q err=%v", opened, err)
	}
	clear(opened)
	replay, err := store.CreateExecutionSecret(ctx, request)
	if err != nil || replay != created {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	drift := request
	drift.Value = "different-api-key"
	if _, err := store.CreateExecutionSecret(ctx, drift); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("idempotency value drift=%v", err)
	}
	if _, err := store.GetExecutionSecret(ctx, "@other:example.test", created.SecretRef, 0); !errors.Is(err, coreexecution.ErrNotFound) {
		t.Fatalf("cross-owner read=%v", err)
	}
	wrong := ref
	wrong.BindingDigest = coreexecution.Digest(strings.Repeat("f", 64))
	if err := store.ResolveCredential(ctx, owner, wrong); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("wrong digest resolved=%v", err)
	}

	now = now.Add(time.Minute)
	revoked, err := store.RevokeExecutionSecret(ctx, ExecutionSecretRevokeRequest{OwnerID: owner, SecretRef: created.SecretRef, ExpectedRevision: 1, IdempotencyID: "22222222-2222-4222-8222-222222222222"})
	if err != nil || revoked.Revision != 2 || revoked.Status != "revoked" || revoked.BindingDigest == created.BindingDigest {
		t.Fatalf("revoked=%+v err=%v", revoked, err)
	}
	if err := store.ResolveCredential(ctx, owner, ref); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("revoked secret resolved=%v", err)
	}
	if _, err := store.OpenExecutionSecret(ctx, owner, ref); !errors.Is(err, coreexecution.ErrConflict) {
		t.Fatalf("revoked secret opened=%v", err)
	}
	page, err := store.ListExecutionSecrets(ctx, owner, "", 10)
	if err != nil || len(page.Items) != 1 || page.Items[0].Status != "revoked" || page.Items[0].Revision != 2 {
		t.Fatalf("page=%+v err=%v", page, err)
	}
}
