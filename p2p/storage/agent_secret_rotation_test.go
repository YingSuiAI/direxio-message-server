package storage

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestPostgresAgentSecretRotationMigratesRealLegacyModelCurrentAndHistory(t *testing.T) {
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
	keyringPath := filepath.Join(dir, "keyring.json")
	legacyPath := filepath.Join(dir, "legacy.key")
	legacyKey := make([]byte, 32)
	for i := range legacyKey {
		legacyKey[i] = byte(i + 1)
	}
	if err := os.WriteFile(legacyPath, legacyKey, 0600); err != nil {
		t.Fatal(err)
	}
	profiles, err := NewDatabaseModelProfileStoreWithKeyring(ctx, store, keyringPath, legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := profiles.SyncModelProfiles(ctx, "owner", "legacy-model", "", []ModelProfileSyncEntry{{ClientProfileID: "legacy", Provider: "openai", Model: "gpt", APIKey: stringPtr("legacy-secret")}})
	if err != nil || len(result.Profiles) != 1 {
		t.Fatalf("legacy source profile = %#v err=%v", result, err)
	}
	block, err := aes.NewCipher(legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	ciphertext := aead.Seal(nil, nonce, []byte("legacy-secret"), []byte(result.Profiles[0].ProfileID+"\x00openai"))
	for _, table := range []string{"p2p_agent_model_profiles", "p2p_agent_model_profile_credentials"} {
		if _, err := store.DB().ExecContext(ctx, `ALTER TABLE `+table+` DROP CONSTRAINT `+table+`_api_key_envelope_check`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE p2p_agent_model_profiles SET api_key_key_id='',api_key_envelope_version=0,api_key_aad_version=0,api_key_nonce=$1,api_key_ciphertext=$2 WHERE owner_id='owner' AND profile_id=$3`, nonce, ciphertext, result.Profiles[0].ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE p2p_agent_model_profile_credentials SET api_key_key_id='',api_key_envelope_version=0,api_key_aad_version=0,api_key_nonce=$1,api_key_ciphertext=$2 WHERE owner_id='owner' AND profile_id=$3 AND credential_version=1`, nonce, ciphertext, result.Profiles[0].ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM db_migrations WHERE version=$1`, "p2p: model credential envelope versions v107"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate real legacy rows: %v", err)
	}
	options := AgentSecretRotationOptions{KeyringFile: keyringPath, LegacyModelProfileKeyFile: legacyPath, LeaseOwner: "legacy-model-rotation"}
	if err := VerifyAgentSecretDatabase(ctx, store.DB(), options); err != nil {
		t.Fatalf("verify legacy rows with legacy key: %v", err)
	}
	if err := VerifyAgentSecretDatabase(ctx, store.DB(), AgentSecretRotationOptions{KeyringFile: keyringPath, LeaseOwner: "legacy-model-no-key"}); err == nil {
		t.Fatal("verify accepted legacy rows without legacy key")
	}
	if err := RotateAgentSecrets(ctx, store.DB(), options); err != nil {
		t.Fatalf("rotate legacy model rows: %v", err)
	}
	if err := VerifyAgentSecretDatabase(ctx, store.DB(), AgentSecretRotationOptions{KeyringFile: keyringPath, LeaseOwner: "legacy-model-post-rotation"}); err != nil {
		t.Fatalf("verify re-sealed rows without legacy key: %v", err)
	}
	for _, table := range []string{"p2p_agent_model_profiles", "p2p_agent_model_profile_credentials"} {
		var count int
		if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE api_key_key_id='' OR api_key_envelope_version<>1 OR api_key_aad_version<>1`).Scan(&count); err != nil || count != 0 {
			t.Fatalf("post-rotation legacy rows in %s = %d err=%v", table, count, err)
		}
	}
}

func TestPostgresPreV97LegacyRowsBootstrapAcrossAgentMigrations(t *testing.T) {
	ctx := context.Background()
	connStr, closeDB := test.PrepareDBConnectionString(t, test.DBTypePostgres)
	defer closeDB()
	dbOpts := config.DatabaseOptions{ConnectionString: config.DataSource(connStr)}
	store, err := NewDatabaseStoreAtMigration(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts, "p2p: agent deployment public event cursor v96")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	keyringPath := filepath.Join(dir, "keyring.json")
	legacyPath := filepath.Join(dir, "legacy.key")
	legacyKey := make([]byte, 32)
	for i := range legacyKey {
		legacyKey[i] = byte(i + 1)
	}
	if err := os.WriteFile(legacyPath, legacyKey, 0600); err != nil {
		t.Fatal(err)
	}
	profileID := "legacy-profile"
	now := time.Now().UTC()
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO p2p_agent_model_profiles(owner_id,profile_id,client_profile_id,provider,revision,created_at,updated_at) VALUES($1,$2,$3,$4,1,$5,$5)`, "owner", profileID, "legacy", "openai", now); err != nil {
		t.Fatal(err)
	}
	block, err := aes.NewCipher(legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	ciphertext := aead.Seal(nil, nonce, []byte("legacy-secret"), []byte(profileID+"\x00openai"))
	if _, err := store.DB().ExecContext(ctx, `INSERT INTO p2p_agent_model_profile_credentials(owner_id,profile_id,credential_version,provider,api_key_nonce,api_key_ciphertext,created_at) VALUES($1,$2,1,'openai',$3,$4,$5)`, "owner", profileID, nonce, ciphertext, now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE p2p_agent_model_profiles SET api_key_nonce=$1,api_key_ciphertext=$2,credential_version=1 WHERE owner_id='owner' AND profile_id=$3`, nonce, ciphertext, profileID); err != nil {
		t.Fatal(err)
	}
	var before []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT api_key_ciphertext FROM p2p_agent_model_profiles WHERE owner_id='owner' AND profile_id=$1`, profileID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = NewDatabaseStore(ctx, sqlutil.NewConnectionManager(nil, dbOpts), &dbOpts)
	if err != nil {
		t.Fatalf("current startup migration from v96: %v", err)
	}
	keyring, guard, err := BootstrapAgentSecretRuntime(ctx, store.DB(), keyringPath)
	if err != nil {
		t.Fatalf("auto bootstrap after v97-v102 migrations: %v", err)
	}
	if keyring == nil {
		t.Fatal("bootstrap returned no keyring")
	}
	if err := VerifyAgentSecretDatabase(ctx, store.DB(), AgentSecretRotationOptions{KeyringFile: keyringPath, LegacyModelProfileKeyFile: legacyPath}); err != nil {
		t.Fatalf("legacy compatibility verification: %v", err)
	}
	if err := guard.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := NewDatabaseModelProfileStoreWithKeyring(ctx, store, keyringPath, legacyPath); !errors.Is(err, ErrModelProfileUpgradeRequired) {
		t.Fatalf("restart legacy model store error = %v, want explicit upgrade boundary", err)
	}
	if err := VerifyAgentSecretDatabase(ctx, store.DB(), AgentSecretRotationOptions{KeyringFile: keyringPath, LegacyModelProfileKeyFile: legacyPath}); err != nil {
		t.Fatalf("restart legacy verification: %v", err)
	}
	var after []byte
	if err := store.DB().QueryRowContext(ctx, `SELECT api_key_ciphertext FROM p2p_agent_model_profiles WHERE owner_id='owner' AND profile_id=$1`, profileID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("auto bootstrap modified legacy ciphertext")
	}
}

func TestPostgresAgentSecretLegacyModelUpgradeResumesAndIsIdempotent(t *testing.T) {
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
	keyringPath := filepath.Join(dir, "keyring.json")
	legacyPath := filepath.Join(dir, "legacy.key")
	legacyKey := make([]byte, 32)
	for i := range legacyKey {
		legacyKey[i] = byte(i + 1)
	}
	if err := os.WriteFile(legacyPath, legacyKey, 0600); err != nil {
		t.Fatal(err)
	}
	profiles, err := NewDatabaseModelProfileStoreWithKeyring(ctx, store, keyringPath, legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	result, err := profiles.SyncModelProfiles(ctx, "owner", "legacy-upgrade", "", []ModelProfileSyncEntry{{ClientProfileID: "legacy", Provider: "openai", Model: "gpt", APIKey: stringPtr("legacy-secret")}})
	if err != nil || len(result.Profiles) != 1 {
		t.Fatalf("legacy source profile = %#v err=%v", result, err)
	}
	block, err := aes.NewCipher(legacyKey)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	for i := range nonce {
		nonce[i] = byte(i + 1)
	}
	ciphertext := aead.Seal(nil, nonce, []byte("legacy-secret"), []byte(result.Profiles[0].ProfileID+"\x00openai"))
	for _, table := range []string{"p2p_agent_model_profiles", "p2p_agent_model_profile_credentials"} {
		if _, err := store.DB().ExecContext(ctx, `ALTER TABLE `+table+` DROP CONSTRAINT `+table+`_api_key_envelope_check`); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE p2p_agent_model_profiles SET api_key_key_id='',api_key_envelope_version=0,api_key_aad_version=0,api_key_nonce=$1,api_key_ciphertext=$2 WHERE owner_id='owner' AND profile_id=$3`, nonce, ciphertext, result.Profiles[0].ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `UPDATE p2p_agent_model_profile_credentials SET api_key_key_id='',api_key_envelope_version=0,api_key_aad_version=0,api_key_nonce=$1,api_key_ciphertext=$2 WHERE owner_id='owner' AND profile_id=$3 AND credential_version=1`, nonce, ciphertext, result.Profiles[0].ProfileID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM db_migrations WHERE version=$1`, "p2p: model credential envelope versions v107"); err != nil {
		t.Fatal(err)
	}
	if err := store.Migrate(ctx); err != nil {
		t.Fatalf("migrate real legacy rows: %v", err)
	}
	keyring, err := LoadAgentSecretKeyring(keyringPath)
	if err != nil {
		t.Fatal(err)
	}
	activeBefore := keyring.activeKeyID
	options := AgentSecretRotationOptions{KeyringFile: keyringPath, LegacyModelProfileKeyFile: legacyPath, LeaseOwner: "legacy-upgrade-test", BatchSize: 1}
	if err := UpgradeLegacyModelSecrets(ctx, store.DB(), AgentSecretRotationOptions{KeyringFile: keyringPath, LeaseOwner: "missing-upgrade-key"}); err == nil {
		t.Fatal("legacy upgrade accepted missing legacy key")
	}
	wrongLegacyPath := filepath.Join(dir, "wrong-legacy.key")
	wrongLegacy := make([]byte, 32)
	for i := range wrongLegacy {
		wrongLegacy[i] = byte(255 - i)
	}
	if err := os.WriteFile(wrongLegacyPath, wrongLegacy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := UpgradeLegacyModelSecrets(ctx, store.DB(), AgentSecretRotationOptions{KeyringFile: keyringPath, LegacyModelProfileKeyFile: wrongLegacyPath, LeaseOwner: "wrong-upgrade-key"}); err == nil {
		t.Fatal("legacy upgrade accepted wrong legacy key")
	}
	// Commit one bounded current-row batch to model a process crash after a
	// durable commit; the public command must resume the remaining history row.
	enveloper, err := NewAgentSecretEnveloper(keyring)
	if err != nil {
		t.Fatal(err)
	}
	if count, err := upgradeLegacyCurrentModelSecretBatch(ctx, store.DB(), options, enveloper, legacyKey); err != nil || count != 1 {
		t.Fatalf("half-upgrade current batch count=%d err=%v", count, err)
	}
	var legacyHistory int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM p2p_agent_model_profile_credentials WHERE api_key_key_id=''`).Scan(&legacyHistory); err != nil || legacyHistory != 1 {
		t.Fatalf("half-upgrade history rows=%d err=%v", legacyHistory, err)
	}
	interruptedCtx, cancelUpgrade := context.WithCancel(ctx)
	cancelUpgrade()
	if err := UpgradeLegacyModelSecrets(interruptedCtx, store.DB(), options); err == nil {
		t.Fatal("canceled resumed upgrade unexpectedly succeeded")
	}
	if err := UpgradeLegacyModelSecrets(ctx, store.DB(), options); err != nil {
		t.Fatalf("resume legacy upgrade: %v", err)
	}
	if err := UpgradeLegacyModelSecrets(ctx, store.DB(), options); err != nil {
		t.Fatalf("idempotent legacy upgrade: %v", err)
	}
	finalKeyring, err := LoadAgentSecretKeyring(keyringPath)
	if err != nil {
		t.Fatal(err)
	}
	if finalKeyring.activeKeyID != activeBefore {
		t.Fatal("legacy upgrade changed active key")
	}
	if err := VerifyAgentSecretDatabase(ctx, store.DB(), AgentSecretRotationOptions{KeyringFile: keyringPath, LeaseOwner: "post-upgrade-no-legacy"}); err != nil {
		t.Fatalf("verify after upgrade without legacy key: %v", err)
	}
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
