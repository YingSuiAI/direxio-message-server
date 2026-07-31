package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func newAgentSecretGuardTestStore(t *testing.T) *DatabaseStore {
	t.Helper()
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	t.Cleanup(closeDB)
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestAgentSecretLiveRuntimeGuardBlocksRotation(t *testing.T) {
	ctx := context.Background()
	store := newAgentSecretGuardTestStore(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keyring.json")
	initial, err := LoadOrCreateAgentSecretKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := AcquireAgentSecretRuntimeGuard(ctx, store.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := RotateAgentSecrets(ctx, store.DB(), AgentSecretRotationOptions{KeyringFile: path, LeaseOwner: "guard-test"}); !errors.Is(err, ErrAgentSecretRotation) {
		t.Fatalf("rotation while service is live = %v, want fenced failure", err)
	}
	after, err := LoadAgentSecretKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.activeKeyID != initial.activeKeyID {
		t.Fatal("blocked rotation changed the active key")
	}
}

func TestAgentSecretLiveRuntimeGuardBlocksLegacyUpgrade(t *testing.T) {
	ctx := context.Background()
	store := newAgentSecretGuardTestStore(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keyring.json")
	initial, err := LoadOrCreateAgentSecretKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := AcquireAgentSecretRuntimeGuard(ctx, store.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer guard.Close()
	if err := UpgradeLegacyModelSecrets(ctx, store.DB(), AgentSecretRotationOptions{KeyringFile: path, LeaseOwner: "guard-upgrade-test"}); !errors.Is(err, ErrAgentSecretRotation) {
		t.Fatalf("legacy upgrade while service is live = %v, want fenced failure", err)
	}
	after, err := LoadAgentSecretKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.activeKeyID != initial.activeKeyID {
		t.Fatal("blocked legacy upgrade changed the active key")
	}
}

func TestInitializeAgentSecretKeyringRefusesLostKeyWithCiphertext(t *testing.T) {
	ctx := context.Background()
	store := newAgentSecretGuardTestStore(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keyring.json")
	keyring, err := InitializeAgentSecretKeyringForDatabase(ctx, store.DB(), path)
	if err != nil {
		t.Fatal(err)
	}
	enveloper, err := NewAgentSecretEnveloper(keyring)
	if err != nil {
		t.Fatal(err)
	}
	binding := AgentSecretBinding{
		Domain: "test", OwnerID: "owner", EntityID: "entity", Revision: 1,
		Purpose: "purpose", Reference: "reference", BindingDigest: sha256.Sum256([]byte("lost-key")),
	}
	envelope, err := enveloper.Seal(binding, []byte("cannot-recover-with-new-key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO p2p_agent_secrets(
		secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,
		envelope_version,aad_version,key_id,nonce,ciphertext
	) VALUES($1,$2,$3,$4,$5,$6,$7,1,1,$8,$9,$10)`,
		binding.Domain, binding.OwnerID, binding.EntityID, binding.Revision, binding.Purpose, binding.Reference,
		binding.BindingDigest[:], envelope.KeyID, envelope.Nonce, envelope.Ciphertext); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := InitializeAgentSecretKeyringForDatabase(ctx, store.DB(), path); !errors.Is(err, ErrAgentSecretKeyringUnavailable) {
		t.Fatalf("lost keyring initialization = %v, want fail closed", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("lost keyring must not be replaced, stat err=%v", err)
	}
}

func TestBootstrapAgentSecretRuntimeFencesRotationDuringHandoff(t *testing.T) {
	ctx := context.Background()
	store := newAgentSecretGuardTestStore(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "bootstrap-keyring.json")
	keyring, guard, err := BootstrapAgentSecretRuntime(ctx, store.DB(), path)
	if err != nil {
		t.Fatal(err)
	}
	if keyring == nil || guard == nil {
		t.Fatal("bootstrap returned incomplete runtime state")
	}
	defer guard.Close()
	if err := RotateAgentSecrets(ctx, store.DB(), AgentSecretRotationOptions{KeyringFile: path, LeaseOwner: "bootstrap-fence"}); !errors.Is(err, ErrAgentSecretRotation) {
		t.Fatalf("rotation during bootstrap handoff = %v, want fenced failure", err)
	}
}
