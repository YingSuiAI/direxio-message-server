package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testAgentSecretBinding() AgentSecretBinding {
	return AgentSecretBinding{Domain: "test", OwnerID: "owner", EntityID: "entity", Revision: 1, Purpose: "credential", Reference: "provider", BindingDigest: sha256.Sum256([]byte("bound"))}
}

func TestAgentSecretKeyringCreatesRestrictedV1File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keyring", "agent-secrets.json")
	keyring, err := LoadOrCreateAgentSecretKeyring(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewAgentSecretEnveloper(keyring); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("keyring mode: %v, %v", info.Mode(), err)
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || info.Mode().Perm() != 0700 {
		t.Fatalf("keyring directory mode: %v, %v", info.Mode(), err)
	}
	if _, err := LoadOrCreateAgentSecretKeyring(path); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAgentSecretKeyringMissingFileDoesNotCreateIt(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "missing.json")
	if _, err := LoadAgentSecretKeyring(path); !errors.Is(err, ErrAgentSecretKeyringUnavailable) {
		t.Fatalf("missing keyring load error=%v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("load unexpectedly created keyring: %v", err)
	}
}

func TestAgentSecretEnvelopeBindsAndFailsClosed(t *testing.T) {
	keyring, err := LoadOrCreateAgentSecretKeyring(filepath.Join(t.TempDir(), "keyring", "keyring.json"))
	if err != nil {
		t.Fatal(err)
	}
	enveloper, err := NewAgentSecretEnveloper(keyring)
	if err != nil {
		t.Fatal(err)
	}
	binding := testAgentSecretBinding()
	envelope, err := enveloper.Seal(binding, []byte("test credential"))
	if err != nil {
		t.Fatal(err)
	}
	if len(envelope.Nonce) != 12 || envelope.KeyID == "" {
		t.Fatalf("envelope metadata: %#v", envelope)
	}
	plaintext, err := enveloper.Open(binding, envelope)
	if err != nil || string(plaintext) != "test credential" {
		t.Fatalf("open: %q, %v", plaintext, err)
	}
	binding.Revision++
	if _, err := enveloper.Open(binding, envelope); err == nil {
		t.Fatal("changed binding decrypted")
	}
	envelope.KeyID = "missing"
	if _, err := enveloper.Open(testAgentSecretBinding(), envelope); err == nil {
		t.Fatal("unknown key decrypted")
	}
}

func TestModelProfileCredentialEnvelopeReadsLegacyButWritesKeyring(t *testing.T) {
	keyring, err := LoadOrCreateAgentSecretKeyring(filepath.Join(t.TempDir(), "keyring", "keyring.json"))
	if err != nil {
		t.Fatal(err)
	}
	enveloper, err := NewAgentSecretEnveloper(keyring)
	if err != nil {
		t.Fatal(err)
	}
	row, err := SealModelProfileCredential(enveloper, "owner", "profile", "provider", 2, 7, []byte("new credential"))
	if err != nil || row.KeyID == "" || row.CredentialVersion != 7 || row.EnvelopeVersion != 1 || row.AADVersion != 1 {
		t.Fatalf("seal row=%#v err=%v", row, err)
	}
	got, err := OpenModelProfileCredential(enveloper, "owner", "profile", "provider", 2, row, nil)
	if err != nil || string(got) != "new credential" {
		t.Fatalf("new open=%q err=%v", got, err)
	}
	legacyCalls := 0
	got, err = OpenModelProfileCredential(enveloper, "owner", "profile", "provider", 2, ModelProfileCredentialEnvelope{CredentialVersion: 7}, func() ([]byte, error) { legacyCalls++; return []byte("legacy credential"), nil })
	if err != nil || string(got) != "legacy credential" || legacyCalls != 1 {
		t.Fatalf("legacy open=%q calls=%d err=%v", got, legacyCalls, err)
	}
	row.EnvelopeVersion++
	if _, err := OpenModelProfileCredential(enveloper, "owner", "profile", "provider", 2, row, nil); err == nil {
		t.Fatal("unsupported envelope version decrypted")
	}
	row.EnvelopeVersion--
	row.Ciphertext[0] ^= 1
	if _, err := OpenModelProfileCredential(enveloper, "owner", "profile", "provider", 2, row, nil); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
}

func TestModelProfileStoreDecryptsLegacyOnlyWhenKeyIDIsBlank(t *testing.T) {
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "keyring")
	if err := os.Mkdir(keyDir, 0700); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(keyDir, "legacy.key")
	legacyKey := make([]byte, 32)
	for i := range legacyKey {
		legacyKey[i] = byte(i + 1)
	}
	if err := os.WriteFile(legacyPath, legacyKey, 0600); err != nil {
		t.Fatal(err)
	}
	legacy, enveloper, err := loadModelProfileSecretMaterial(filepath.Join(keyDir, "active.json"), legacyPath, false)
	if err != nil || len(legacy) != 32 || enveloper == nil {
		t.Fatalf("material legacy=%d enveloper=%v err=%v", len(legacy), enveloper != nil, err)
	}
	store := &encryptedModelProfileStore{key: legacy, enveloper: enveloper}
	block, err := aes.NewCipher(legacy)
	if err != nil {
		t.Fatal(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	legacyCiphertext := aead.Seal(nil, nonce, []byte("old"), []byte("profile\x00provider"))
	got, err := store.decryptCredential("owner", "profile", "provider", 1, 1, "", 0, 0, 0, nonce, legacyCiphertext)
	if err != nil || got != "old" {
		t.Fatalf("legacy decrypt=%q err=%v", got, err)
	}
	keyID, boundRevision, envelopeVersion, aadVersion, nonce, ciphertext, err := store.encryptCredential("owner", "profile", "provider", 2, 2, []byte("new"))
	if err != nil || keyID == "" || boundRevision != 2 || envelopeVersion != 1 || aadVersion != 1 {
		t.Fatalf("new encrypt id=%q revision=%d err=%v", keyID, boundRevision, err)
	}
	got, err = store.decryptCredential("owner", "profile", "provider", 2, 2, keyID, boundRevision, envelopeVersion, aadVersion, nonce, ciphertext)
	if err != nil || got != "new" {
		t.Fatalf("new decrypt=%q err=%v", got, err)
	}
}
