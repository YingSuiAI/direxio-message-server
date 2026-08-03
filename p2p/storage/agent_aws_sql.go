package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// PersistCredentialEnvelope writes one immutable credential revision. The
// payload is opaque ciphertext; this method has no plaintext credential API.
func (s *DatabaseStore) PersistCredentialEnvelope(ctx context.Context, env AWSCredentialEnvelope, name, region, accountID, userARN string) error {
	if s == nil || s.db == nil || strings.TrimSpace(env.OwnerID) == "" || strings.TrimSpace(env.Reference) == "" || strings.TrimSpace(env.Domain) == "" || strings.TrimSpace(env.Purpose) == "" || strings.TrimSpace(env.KeyID) == "" || env.Revision < 1 || len(env.Opaque) == 0 || len(env.Opaque) < 28 {
		return errors.New("storage: invalid credential envelope")
	}
	id := env.Reference
	if _, err := uuid.Parse(id); err != nil {
		return fmt.Errorf("storage: invalid credential reference")
	}
	digest := env.Digest
	if digest == "" {
		sum := sha256.Sum256(env.Opaque)
		digest = hex.EncodeToString(sum[:])
	}
	digestBytes, _ := hex.DecodeString(digest)
	nonce, ciphertext := env.Opaque[:12], env.Opaque[12:]
	_, err := s.db.ExecContext(ctx, `INSERT INTO p2p_agent_secrets(secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,1,1,$8,$9,$10,clock_timestamp())`, env.Domain, env.OwnerID, id, env.Revision, env.Purpose, env.Reference, digestBytes, env.KeyID, nonce, ciphertext)
	return err
}

// CredentialEnvelopeRow is a safe read model; it never contains plaintext.
type CredentialEnvelopeRow struct {
	OwnerID, CredentialID string
	Revision              int64
	EnvelopeVersion       int
	AADVersion            int
	KeyID                 string
	Nonce, Ciphertext     []byte
	Digest                string
}

func (s *DatabaseStore) LoadCredentialEnvelope(ctx context.Context, ownerID, credentialID string, revision int64) (CredentialEnvelopeRow, error) {
	if s == nil || s.db == nil || strings.TrimSpace(ownerID) == "" || strings.TrimSpace(credentialID) == "" || revision < 1 {
		return CredentialEnvelopeRow{}, errors.New("storage: invalid credential envelope lookup")
	}
	var row CredentialEnvelopeRow
	err := s.db.QueryRowContext(ctx, `SELECT owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$3`, ownerID, credentialID, revision).Scan(&row.OwnerID, &row.CredentialID, &row.Revision, &row.EnvelopeVersion, &row.AADVersion, &row.KeyID, &row.Nonce, &row.Ciphertext, &row.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return CredentialEnvelopeRow{}, errors.New("storage: credential envelope not found")
	}
	return row, err
}
