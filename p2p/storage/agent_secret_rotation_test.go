package storage

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"

	"github.com/YingSuiAI/dirextalk-message-server/internal/sqlutil"
	"github.com/YingSuiAI/dirextalk-message-server/setup/config"
	"github.com/YingSuiAI/dirextalk-message-server/test"
)

func TestAgentSecretEnvelopeBindsDomain(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keyring.json")
	keyring, err := LoadOrCreateAgentSecretKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	enveloper, err := NewAgentSecretEnveloper(keyring)
	if err != nil {
		t.Fatal(err)
	}
	binding := AgentSecretBinding{
		Domain: "aws_credential", OwnerID: "owner", EntityID: "credential",
		Revision: 1, Purpose: "aws_credential", Reference: "credential",
		BindingDigest: sha256.Sum256([]byte("binding")),
	}
	envelope, err := enveloper.Seal(binding, []byte("secret"))
	if err != nil {
		t.Fatal(err)
	}
	binding.Domain = "mcp_credential"
	if _, err := enveloper.Open(binding, envelope); !errorsIsAgentSecret(err, ErrAgentSecretEnvelopeInvalid) {
		t.Fatalf("wrong domain must fail authentication, got %v", err)
	}
}

func TestAgentSecretKeyringRotationResumesBeforeRetirement(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "keyring.json")
	initial, err := LoadOrCreateAgentSecretKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	firstActive := initial.activeKeyID
	file, _, old, err := prepareAgentSecretKeyringRotation(path)
	if err != nil {
		t.Fatal(err)
	}
	if file.ActiveKeyID == firstActive || len(old) != 1 || old[0] != firstActive {
		t.Fatalf("rotation key state active=%q old=%v", file.ActiveKeyID, old)
	}
	resumed, _, resumedOld, err := prepareAgentSecretKeyringRotation(path)
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ActiveKeyID != file.ActiveKeyID || !equalAgentSecretKeyIDs(old, resumedOld) {
		t.Fatal("resume must retain the same active/decrypt-only keys")
	}
	if err := retireAgentSecretKeys(path, file.ActiveKeyID, old); err != nil {
		t.Fatal(err)
	}
	final, err := readAgentSecretKeyringFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Keys) != 1 || final.Keys[0].ID != file.ActiveKeyID || final.Keys[0].DecryptOnly {
		t.Fatalf("retired keyring = %#v", final)
	}
}

func TestPostgresAgentSecretRotationRewrapsGenericAndModelRows(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keyringPath := filepath.Join(dir, "secret-keyring.json")
	keyring, err := LoadOrCreateAgentSecretKeyring(keyringPath)
	if err != nil {
		t.Fatal(err)
	}
	oldKeyID := keyring.activeKeyID
	profiles, err := NewDatabaseModelProfileStoreWithKeyring(ctx, store, keyringPath, "")
	if err != nil {
		t.Fatal(err)
	}
	synced, err := profiles.SyncModelProfiles(ctx, "owner", "rotation-model-sync", "client", []ModelProfileSyncEntry{{
		ClientProfileID: "client", Provider: "openai", Model: "model", APIKey: stringPtr("model-secret"),
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(synced.Profiles) != 1 {
		t.Fatalf("profiles = %#v", synced.Profiles)
	}

	enveloper, err := NewAgentSecretEnveloper(keyring)
	if err != nil {
		t.Fatal(err)
	}
	binding := AgentSecretBinding{
		Domain: "aws_credential", OwnerID: "owner", EntityID: "credential",
		Revision: 1, Purpose: "aws_credential", Reference: "credential",
		BindingDigest: sha256.Sum256([]byte("aws-binding")),
	}
	envelope, err := enveloper.Seal(binding, []byte("aws-secret"))
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

	options := AgentSecretRotationOptions{KeyringFile: keyringPath, LeaseOwner: "rotation-test"}
	if err := RotateAgentSecrets(ctx, store.DB(), options); err != nil {
		t.Fatal(err)
	}
	rotated, err := LoadAgentSecretKeyring(keyringPath)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.activeKeyID == oldKeyID || rotated.keys[oldKeyID] != nil {
		t.Fatal("old key was not safely retired")
	}
	if err := VerifyAgentSecretDatabase(ctx, store.DB(), options); err != nil {
		t.Fatal(err)
	}
	var currentEnvelope, currentAAD, historicalEnvelope, historicalAAD int64
	if err := store.DB().QueryRowContext(ctx, `SELECT api_key_envelope_version,api_key_aad_version FROM p2p_agent_model_profiles WHERE owner_id='owner' AND profile_id=$1`, synced.Profiles[0].ProfileID).Scan(&currentEnvelope, &currentAAD); err != nil || currentEnvelope != 1 || currentAAD != 1 {
		t.Fatalf("rotated current model envelope = %d/%d err=%v", currentEnvelope, currentAAD, err)
	}
	if err := store.DB().QueryRowContext(ctx, `SELECT api_key_envelope_version,api_key_aad_version FROM p2p_agent_model_profile_credentials WHERE owner_id='owner' AND profile_id=$1 AND credential_version=1`, synced.Profiles[0].ProfileID).Scan(&historicalEnvelope, &historicalAAD); err != nil || historicalEnvelope != 1 || historicalAAD != 1 {
		t.Fatalf("rotated historical model envelope = %d/%d err=%v", historicalEnvelope, historicalAAD, err)
	}

	restarted, err := NewDatabaseModelProfileStoreWithKeyring(ctx, store, keyringPath, "")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := restarted.ResolveModelProfile(ctx, "owner", synced.Profiles[0].ProfileID)
	if err != nil || profile.APIKey != "model-secret" {
		t.Fatalf("rotated model credential = %#v, err=%v", profile, err)
	}
	var keyID string
	var nonce, ciphertext []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT key_id,nonce,ciphertext FROM p2p_agent_secrets
		WHERE secret_domain=$1 AND owner_id=$2 AND entity_id=$3 AND secret_revision=$4 AND purpose=$5 AND reference=$6`,
		binding.Domain, binding.OwnerID, binding.EntityID, binding.Revision, binding.Purpose, binding.Reference).
		Scan(&keyID, &nonce, &ciphertext); err != nil {
		t.Fatal(err)
	}
	plaintext, err := NewAgentSecretEnveloperForTest(rotated, binding, AgentSecretEnvelope{KeyID: keyID, Nonce: nonce, Ciphertext: ciphertext})
	if err != nil || string(plaintext) != "aws-secret" {
		t.Fatalf("rotated generic credential err=%v", err)
	}
	clear(plaintext)
}

func NewAgentSecretEnveloperForTest(keyring *AgentSecretKeyring, binding AgentSecretBinding, envelope AgentSecretEnvelope) ([]byte, error) {
	enveloper, err := NewAgentSecretEnveloper(keyring)
	if err != nil {
		return nil, err
	}
	return enveloper.Open(binding, envelope)
}

func errorsIsAgentSecret(err, target error) bool {
	return err == target
}
