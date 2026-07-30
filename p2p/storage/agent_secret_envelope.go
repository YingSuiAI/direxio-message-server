package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// These errors intentionally contain no path, key, or ciphertext material.
var (
	ErrAgentSecretKeyringUnavailable = errors.New("agent secret keyring unavailable")
	ErrAgentSecretEnvelopeInvalid    = errors.New("agent secret envelope invalid")
)

const agentSecretKeyringVersion = 1

const (
	modelProfileLegacyEnvelopeVersion = 0
	modelProfileEnvelopeVersion       = 1
	modelProfileAADVersion            = 1
)

// AgentSecretBinding identifies one durable secret.  Its digest must be a
// SHA-256 digest of any additional immutable business binding.
type AgentSecretBinding struct {
	Domain             string
	OwnerID, EntityID  string
	Revision           int64
	Purpose, Reference string
	BindingDigest      [sha256.Size]byte
}

// AgentSecretEnvelope is the database row representation. KeyID must be kept
// in a dedicated row column; it is deliberately not inferred from ciphertext.
type AgentSecretEnvelope struct {
	KeyID             string
	Nonce, Ciphertext []byte
}

// ModelProfileCredentialEnvelope is the durable form used when model-profile
// credentials move to the keyring. CredentialVersion is metadata only: it is
// also authenticated so a ciphertext cannot be replayed for another version.
// A blank KeyID identifies an old v1 model-profile ciphertext and is read only.
type ModelProfileCredentialEnvelope struct {
	AgentSecretEnvelope
	CredentialVersion, EnvelopeVersion, AADVersion int64
}

type agentSecretKeyringFile struct {
	Version     int                    `json:"version"`
	ActiveKeyID string                 `json:"active_key_id"`
	Keys        []agentSecretKeyRecord `json:"keys"`
}

type agentSecretKeyRecord struct {
	ID          string `json:"id"`
	Key         string `json:"key"`
	DecryptOnly bool   `json:"decrypt_only,omitempty"`
}

// AgentSecretKeyring holds only decoded key material. Its on-disk JSON format
// is versioned so rotations can retain decrypt-only keys without ambiguity.
type AgentSecretKeyring struct {
	activeKeyID string
	keys        map[string][]byte
}

// LoadOrCreateAgentSecretKeyring creates a v1 keyring only when absent. A
// non-empty legacy deployment must be migrated explicitly; it is never read as
// a plaintext database import.
func LoadOrCreateAgentSecretKeyring(path string) (*AgentSecretKeyring, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	if raw, err := os.ReadFile(path); err == nil {
		if !agentSecretPrivatePath(path) {
			return nil, ErrAgentSecretKeyringUnavailable
		}
		return parseAgentSecretKeyring(raw)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	if err := os.Chmod(filepath.Dir(path), 0700); err != nil || !agentSecretPrivateDir(filepath.Dir(path)) {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	key := make([]byte, 32)
	id := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	if _, err := io.ReadFull(rand.Reader, id); err != nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	file := agentSecretKeyringFile{Version: agentSecretKeyringVersion, ActiveKeyID: hex.EncodeToString(id), Keys: []agentSecretKeyRecord{{ID: hex.EncodeToString(id), Key: base64.RawStdEncoding.EncodeToString(key)}}}
	raw, err := json.Marshal(file)
	if err != nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if errors.Is(err, os.ErrExist) {
		return LoadOrCreateAgentSecretKeyring(path)
	}
	if err != nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	writeErr := func() error {
		if err := f.Chmod(0600); err != nil {
			return err
		}
		if _, err := f.Write(raw); err != nil {
			return err
		}
		return f.Sync()
	}()
	closeErr := f.Close()
	if writeErr != nil || closeErr != nil {
		_ = os.Remove(path)
		return nil, ErrAgentSecretKeyringUnavailable
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	syncErr := dir.Sync()
	closeErr = dir.Close()
	if syncErr != nil || closeErr != nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	return parseAgentSecretKeyring(raw)
}

// LoadAgentSecretKeyring verifies an existing keyring without creating or
// modifying it. It is intended for startup checks and one-shot verification.
func LoadAgentSecretKeyring(path string) (*AgentSecretKeyring, error) {
	path = strings.TrimSpace(path)
	if path == "" || !agentSecretPrivatePath(path) {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	return parseAgentSecretKeyring(raw)
}

func agentSecretPrivatePath(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm() == 0600 && agentSecretPrivateDir(filepath.Dir(path))
}

func agentSecretPrivateDir(path string) bool {
	info, err := os.Lstat(path)
	return err == nil && info.IsDir() && info.Mode().Perm() == 0700
}

func parseAgentSecretKeyring(raw []byte) (*AgentSecretKeyring, error) {
	var file agentSecretKeyringFile
	if json.Unmarshal(raw, &file) != nil || file.Version != agentSecretKeyringVersion || strings.TrimSpace(file.ActiveKeyID) == "" || len(file.Keys) == 0 {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	keys := make(map[string][]byte, len(file.Keys))
	active := false
	for _, entry := range file.Keys {
		id := strings.TrimSpace(entry.ID)
		key, err := base64.RawStdEncoding.DecodeString(entry.Key)
		if err != nil || id == "" || len(key) != 32 || keys[id] != nil {
			return nil, ErrAgentSecretKeyringUnavailable
		}
		if id == file.ActiveKeyID {
			active = !entry.DecryptOnly
		} else if !entry.DecryptOnly {
			return nil, ErrAgentSecretKeyringUnavailable
		}
		keys[id] = append([]byte(nil), key...)
	}
	if !active {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	return &AgentSecretKeyring{activeKeyID: file.ActiveKeyID, keys: keys}, nil
}

// NewAgentSecretEnveloper validates and takes a private copy of the supplied
// keyring, making the result safe for concurrent read-only use.
func NewAgentSecretEnveloper(keyring *AgentSecretKeyring) (*AgentSecretEnveloper, error) {
	if keyring == nil || strings.TrimSpace(keyring.activeKeyID) == "" || len(keyring.keys[keyring.activeKeyID]) != 32 {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	keys := make(map[string][]byte, len(keyring.keys))
	for id, key := range keyring.keys {
		if strings.TrimSpace(id) == "" || len(key) != 32 {
			return nil, ErrAgentSecretKeyringUnavailable
		}
		keys[id] = append([]byte(nil), key...)
	}
	return &AgentSecretEnveloper{activeKeyID: keyring.activeKeyID, keys: keys}, nil
}

type AgentSecretEnveloper struct {
	activeKeyID string
	keys        map[string][]byte
}

func (e *AgentSecretEnveloper) Seal(binding AgentSecretBinding, plaintext []byte) (AgentSecretEnvelope, error) {
	if e == nil || len(e.keys[e.activeKeyID]) != 32 {
		return AgentSecretEnvelope{}, ErrAgentSecretKeyringUnavailable
	}
	aad, err := agentSecretAAD(binding)
	if err != nil {
		return AgentSecretEnvelope{}, err
	}
	block, err := aes.NewCipher(e.keys[e.activeKeyID])
	if err != nil {
		return AgentSecretEnvelope{}, ErrAgentSecretKeyringUnavailable
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || aead.NonceSize() != 12 {
		return AgentSecretEnvelope{}, ErrAgentSecretKeyringUnavailable
	}
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return AgentSecretEnvelope{}, ErrAgentSecretKeyringUnavailable
	}
	return AgentSecretEnvelope{KeyID: e.activeKeyID, Nonce: nonce, Ciphertext: aead.Seal(nil, nonce, plaintext, aad)}, nil
}

func (e *AgentSecretEnveloper) Open(binding AgentSecretBinding, envelope AgentSecretEnvelope) ([]byte, error) {
	if e == nil {
		return nil, ErrAgentSecretKeyringUnavailable
	}
	key := e.keys[strings.TrimSpace(envelope.KeyID)]
	if len(key) != 32 {
		return nil, ErrAgentSecretEnvelopeInvalid
	}
	aad, err := agentSecretAAD(binding)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, ErrAgentSecretEnvelopeInvalid
	}
	aead, err := cipher.NewGCM(block)
	if err != nil || len(envelope.Nonce) != 12 || len(envelope.Ciphertext) < aead.Overhead() {
		return nil, ErrAgentSecretEnvelopeInvalid
	}
	plaintext, err := aead.Open(nil, envelope.Nonce, envelope.Ciphertext, aad)
	if err != nil {
		return nil, ErrAgentSecretEnvelopeInvalid
	}
	return plaintext, nil
}

// SealModelProfileCredential is the single write path for the keyring format.
// The caller persists KeyID in a dedicated row column and preserves the passed
// credential version; it never derives a new version during re-encryption.
func SealModelProfileCredential(e *AgentSecretEnveloper, ownerID, profileID, provider string, profileRevision, credentialVersion int64, plaintext []byte) (ModelProfileCredentialEnvelope, error) {
	if credentialVersion < 1 {
		return ModelProfileCredentialEnvelope{}, ErrAgentSecretEnvelopeInvalid
	}
	binding := modelProfileCredentialBinding(ownerID, profileID, provider, profileRevision, credentialVersion)
	envelope, err := e.Seal(binding, plaintext)
	if err != nil {
		return ModelProfileCredentialEnvelope{}, err
	}
	return ModelProfileCredentialEnvelope{AgentSecretEnvelope: envelope, CredentialVersion: credentialVersion, EnvelopeVersion: modelProfileEnvelopeVersion, AADVersion: modelProfileAADVersion}, nil
}

// OpenModelProfileCredential reads either a new keyring envelope or, only for
// rows without key_id, a caller-supplied legacy decryptor. This is deliberately
// not a write helper for old ciphertext: all new writes use Seal above.
func OpenModelProfileCredential(e *AgentSecretEnveloper, ownerID, profileID, provider string, profileRevision int64, row ModelProfileCredentialEnvelope, legacyOpen func() ([]byte, error)) ([]byte, error) {
	if row.CredentialVersion < 1 {
		return nil, ErrAgentSecretEnvelopeInvalid
	}
	if strings.TrimSpace(row.KeyID) == "" {
		if row.EnvelopeVersion != modelProfileLegacyEnvelopeVersion || row.AADVersion != modelProfileLegacyEnvelopeVersion || legacyOpen == nil {
			return nil, ErrAgentSecretEnvelopeInvalid
		}
		plaintext, err := legacyOpen()
		if err != nil {
			return nil, ErrAgentSecretEnvelopeInvalid
		}
		return plaintext, nil
	}
	if row.EnvelopeVersion != modelProfileEnvelopeVersion || row.AADVersion != modelProfileAADVersion {
		return nil, ErrAgentSecretEnvelopeInvalid
	}
	return e.Open(modelProfileCredentialBinding(ownerID, profileID, provider, profileRevision, row.CredentialVersion), row.AgentSecretEnvelope)
}

func modelProfileCredentialBinding(ownerID, profileID, provider string, profileRevision, credentialVersion int64) AgentSecretBinding {
	digest := sha256.Sum256([]byte(provider + "\x00" + strconv.FormatInt(credentialVersion, 10)))
	return AgentSecretBinding{Domain: "model_profile", OwnerID: ownerID, EntityID: profileID, Revision: profileRevision, Purpose: "model_profile_credential", Reference: provider, BindingDigest: digest}
}

func agentSecretAAD(binding AgentSecretBinding) ([]byte, error) {
	if strings.TrimSpace(binding.Domain) == "" || strings.TrimSpace(binding.OwnerID) == "" || strings.TrimSpace(binding.EntityID) == "" || strings.TrimSpace(binding.Purpose) == "" || strings.TrimSpace(binding.Reference) == "" || binding.Revision < 1 {
		return nil, ErrAgentSecretEnvelopeInvalid
	}
	var revision [8]byte
	binary.BigEndian.PutUint64(revision[:], uint64(binding.Revision))
	parts := [][]byte{[]byte("dirextalk.agent.secret.envelope.v1"), []byte(binding.Domain), []byte(binding.OwnerID), []byte(binding.EntityID), revision[:], []byte(binding.Purpose), []byte(binding.Reference), binding.BindingDigest[:]}
	buf := make([]byte, 0, 128)
	for _, part := range parts {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(part)))
		buf = append(buf, length[:]...)
		buf = append(buf, part...)
	}
	return buf, nil
}
