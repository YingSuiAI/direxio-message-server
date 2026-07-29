package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"

	ext "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
)

// AgentExtensionSecretResolver opens only an encrypted envelope whose owner,
// installation/version, reference, purpose and binding digest all match the
// immutable row. Plaintext never enters a DTO or persistence record.
type AgentExtensionSecretResolver struct {
	Store          *DatabaseStore
	Enveloper      *AgentSecretEnveloper
	InstallationID string
}

func (r *AgentExtensionSecretResolver) Resolve(ctx context.Context, owner, versionID, reference, purpose, bindingDigest string) ([]byte, error) {
	if r == nil || r.Store == nil || r.Enveloper == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(versionID) == "" || strings.TrimSpace(reference) == "" || strings.TrimSpace(purpose) == "" || len(bindingDigest) != 64 || r.InstallationID == "" {
		return nil, ext.ErrInvalid
	}
	var key string
	var revision int64
	var nonce, ciphertext []byte
	entityID := extensionSecretEntityID(r.InstallationID, versionID)
	err := r.Store.db.QueryRowContext(ctx, `SELECT key_id,nonce,ciphertext,secret_revision FROM p2p_agent_secrets WHERE secret_domain='extension' AND owner_id=$1 AND entity_id=$2 AND purpose=$3 AND reference=$4 AND binding_digest=$5 ORDER BY secret_revision DESC LIMIT 1`, owner, entityID, purpose, reference, mustDigestBytes(bindingDigest)).Scan(&key, &nonce, &ciphertext, &revision)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ext.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	decoded, _ := hex.DecodeString(bindingDigest)
	var digest [32]byte
	copy(digest[:], decoded)
	plain, err := r.Enveloper.Open(AgentSecretBinding{Domain: "extension", OwnerID: owner, EntityID: entityID, Revision: revision, Purpose: purpose, Reference: reference, BindingDigest: digest}, AgentSecretEnvelope{KeyID: key, Nonce: nonce, Ciphertext: ciphertext})
	if err != nil {
		return nil, ext.ErrConflict
	}
	return plain, nil
}

func mustDigestBytes(value string) []byte {
	b, _ := hex.DecodeString(value)
	if len(b) != sha256.Size {
		return nil
	}
	return b
}

func (r *AgentExtensionSecretResolver) Stage(ctx context.Context, owner, installationID string, revision int64, versionID string, inputs []ext.SecretInput) error {
	if r == nil || r.Store == nil || r.Enveloper == nil || owner == "" || installationID == "" || versionID == "" || revision < 1 {
		return ext.ErrInvalid
	}
	return r.Store.writer.Do(r.Store.db, nil, func(tx *sql.Tx) error {
		for _, in := range inputs {
			if in.ReferenceID == "" || in.Purpose != "mcp_credential" || in.Value == "" {
				return ext.ErrInvalid
			}
			digest := sha256.Sum256([]byte(in.Value))
			entityID := extensionSecretEntityID(installationID, versionID)
			binding := AgentSecretBinding{Domain: "extension", OwnerID: owner, EntityID: entityID, Revision: revision, Purpose: in.Purpose, Reference: in.ReferenceID, BindingDigest: digest}
			plaintext := []byte(in.Value)
			sealed, e := r.Enveloper.Seal(binding, plaintext)
			clear(plaintext)
			if e != nil {
				return e
			}
			if _, e = tx.ExecContext(ctx, `INSERT INTO p2p_agent_secrets(secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext,created_at) VALUES('extension',$1,$2,$3,$4,$5,$6,1,1,$7,$8,$9,clock_timestamp()) ON CONFLICT DO NOTHING`, owner, entityID, revision, in.Purpose, in.ReferenceID, digest[:], sealed.KeyID, sealed.Nonce, sealed.Ciphertext); e != nil {
				return e
			}
		}
		return nil
	})
}

func extensionSecretEntityID(installationID, versionID string) string {
	// Both IDs are opaque UUID-like tokens; use a text-safe delimiter because
	// PostgreSQL TEXT rejects NUL bytes.  The pair is still unambiguous for the
	// validated identifier grammar used by the extension boundary.
	return installationID + "::" + versionID
}
