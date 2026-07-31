package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	"github.com/google/uuid"
)

type AWSCredentialEnvelope struct {
	OwnerID, Reference string
	Revision           int64
	Digest, Domain     string
	Purpose, KeyID     string
	Opaque             []byte
}

type AWSCredentialResolver interface {
	ResolveCredential(context.Context, string, int64) (AWSCredentialEnvelope, error)
}

type AWSRepository interface{ agentaws.Repository }

type PostgresAWSRepository struct {
	store     *DatabaseStore
	ownerID   string
	enveloper *AgentSecretEnveloper
}

var _ agentaws.Repository = (*PostgresAWSRepository)(nil)
var _ agentaws.CredentialReplayRepository = (*PostgresAWSRepository)(nil)

func validAWSUUID(v string) bool { _, err := uuid.Parse(strings.TrimSpace(v)); return err == nil }

func NewAgentAWSRepository(store *DatabaseStore, ownerID string) (*PostgresAWSRepository, error) {
	if store == nil || strings.TrimSpace(ownerID) == "" {
		return nil, errors.New("storage: invalid AWS repository owner")
	}
	return &PostgresAWSRepository{store: store, ownerID: strings.TrimSpace(ownerID)}, nil
}

func NewAgentAWSRepositoryWithEnveloper(store *DatabaseStore, ownerID string, enveloper *AgentSecretEnveloper) (*PostgresAWSRepository, error) {
	r, err := NewAgentAWSRepository(store, ownerID)
	if err != nil {
		return nil, err
	}
	if enveloper == nil {
		return nil, errors.New("storage: missing AWS secret enveloper")
	}
	r.enveloper = enveloper
	return r, nil
}

func (r *PostgresAWSRepository) ReplayCredential(ctx context.Context, operation, key, digest string) (agentaws.CredentialView, bool, error) {
	if r == nil || r.store == nil || r.store.db == nil || !validAWSUUID(key) {
		return agentaws.CredentialView{}, false, agentaws.ErrInvalid
	}
	var hash string
	var raw []byte
	err := r.store.db.QueryRowContext(ctx, `SELECT request_hash,response_json FROM core_aws_replays WHERE owner_id=$1 AND operation=$2 AND idempotency_key=$3`, r.ownerID, operation, key).Scan(&hash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return agentaws.CredentialView{}, false, nil
	}
	if err != nil {
		return agentaws.CredentialView{}, false, err
	}
	if hash != digest {
		return agentaws.CredentialView{}, true, agentaws.ErrIdempotencyConflict
	}
	if operation == "credential-delete" {
		return agentaws.CredentialView{}, true, nil
	}
	var view agentaws.CredentialView
	if json.Unmarshal(raw, &view) != nil {
		return agentaws.CredentialView{}, true, agentaws.ErrConflict
	}
	return view, true, nil
}

func (r *PostgresAWSRepository) ReplayCredentialTest(ctx context.Context, id string, expected int64, key, digest string) (agentaws.CredentialTest, bool, error) {
	if r == nil || r.store == nil || r.store.db == nil || !validAWSUUID(id) || !validAWSUUID(key) || expected < 1 {
		return agentaws.CredentialTest{}, false, agentaws.ErrInvalid
	}
	var hash string
	var raw []byte
	err := r.store.db.QueryRowContext(ctx, `SELECT request_hash,response_json FROM core_aws_replays WHERE owner_id=$1 AND operation='credential-test' AND idempotency_key=$2`, r.ownerID, key).Scan(&hash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return agentaws.CredentialTest{}, false, nil
	}
	if err != nil {
		return agentaws.CredentialTest{}, false, err
	}
	if hash != digest {
		return agentaws.CredentialTest{}, true, agentaws.ErrIdempotencyConflict
	}
	var out agentaws.CredentialTest
	if json.Unmarshal(raw, &out) != nil || out.CredentialID != id {
		return agentaws.CredentialTest{}, true, agentaws.ErrConflict
	}
	return out, true, nil
}

func (r *PostgresAWSRepository) TestCredentialIdempotent(ctx context.Context, id string, expected int64, identity agentaws.Identity, key, digest string) (agentaws.CredentialTest, error) {
	if r == nil || r.store == nil || r.store.db == nil || !validAWSUUID(id) || !validAWSUUID(key) || expected < 1 {
		return agentaws.CredentialTest{}, agentaws.ErrInvalid
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.CredentialTest{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, canonicalAdvisoryLockIdentity("aws", r.ownerID, "credential-test", key)); err != nil {
		return agentaws.CredentialTest{}, err
	}
	var hash string
	var raw []byte
	err = tx.QueryRowContext(ctx, `SELECT request_hash,response_json FROM core_aws_replays WHERE owner_id=$1 AND operation='credential-test' AND idempotency_key=$2 FOR UPDATE`, r.ownerID, key).Scan(&hash, &raw)
	if err == nil {
		if hash != digest {
			return agentaws.CredentialTest{}, agentaws.ErrIdempotencyConflict
		}
		var out agentaws.CredentialTest
		if json.Unmarshal(raw, &out) != nil {
			return agentaws.CredentialTest{}, agentaws.ErrConflict
		}
		return out, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return agentaws.CredentialTest{}, err
	}
	var account, arn string
	var verified, current int64
	if err = tx.QueryRowContext(ctx, `SELECT account_id,user_arn,verified_revision,revision FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$3 FOR UPDATE`, r.ownerID, id, expected).Scan(&account, &arn, &verified, &current); errors.Is(err, sql.ErrNoRows) {
		return agentaws.CredentialTest{}, agentaws.ErrRevisionConflict
	} else if err != nil {
		return agentaws.CredentialTest{}, err
	}
	if current != expected {
		return agentaws.CredentialTest{}, agentaws.ErrRevisionConflict
	}
	if verified == expected && (account != identity.AccountID || arn != identity.UserARN) {
		return agentaws.CredentialTest{}, agentaws.ErrConflict
	}
	now := time.Now().UTC()
	if verified != expected {
		if _, err = tx.ExecContext(ctx, `UPDATE core_aws_credentials SET account_id=$1,user_arn=$2,verified_revision=revision,updated_at=$3 WHERE owner_id=$4 AND credential_id=$5 AND revision=$6`, identity.AccountID, identity.UserARN, now, r.ownerID, id, expected); err != nil {
			return agentaws.CredentialTest{}, err
		}
	}
	out := agentaws.CredentialTest{CredentialID: id, Identity: identity, CredentialRevision: expected, TestedAt: now}
	if err = recordCredentialReplayTx(ctx, tx, r.ownerID, "credential-test", key, digest, out); err != nil {
		return agentaws.CredentialTest{}, err
	}
	if err = tx.Commit(); err != nil {
		return agentaws.CredentialTest{}, err
	}
	return out, nil
}

func credentialReplayTx(ctx context.Context, tx *sql.Tx, owner, operation, key, digest string) (agentaws.CredentialView, bool, error) {
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, canonicalAdvisoryLockIdentity("aws", owner, operation, key)); err != nil {
		return agentaws.CredentialView{}, false, err
	}
	var hash string
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT request_hash,response_json FROM core_aws_replays WHERE owner_id=$1 AND operation=$2 AND idempotency_key=$3 FOR UPDATE`, owner, operation, key).Scan(&hash, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return agentaws.CredentialView{}, false, nil
	}
	if err != nil {
		return agentaws.CredentialView{}, false, err
	}
	if hash != digest {
		return agentaws.CredentialView{}, true, agentaws.ErrIdempotencyConflict
	}
	if operation == "credential-delete" {
		return agentaws.CredentialView{}, true, nil
	}
	var view agentaws.CredentialView
	if json.Unmarshal(raw, &view) != nil {
		return agentaws.CredentialView{}, true, agentaws.ErrConflict
	}
	return view, true, nil
}

func recordCredentialReplayTx(ctx context.Context, tx *sql.Tx, owner, operation, key, digest string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json) VALUES($1,$2,$3,$4,$5)`, owner, operation, key, digest, raw)
	return err
}

func (r *PostgresAWSRepository) CreateCredential(ctx context.Context, v agentaws.Credentials) (agentaws.Credentials, error) {
	if r == nil || r.store == nil || r.store.db == nil || r.enveloper == nil || v.Validate() != nil {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	now := v.CreatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	if v.UpdatedAt.IsZero() {
		v.UpdatedAt = now
	}
	v.CreatedAt = now
	return r.persistCredential(ctx, v, nil)
}

func (r *PostgresAWSRepository) SaveCredentialIdempotent(ctx context.Context, v agentaws.Credentials, key, digest string) (agentaws.CredentialView, error) {
	if !validAWSUUID(key) {
		return agentaws.CredentialView{}, agentaws.ErrInvalid
	}
	if r == nil || r.store == nil || r.store.db == nil || r.enveloper == nil || v.Validate() != nil {
		return agentaws.CredentialView{}, agentaws.ErrInvalid
	}
	return r.persistCredentialReplay(ctx, v, key, digest, "credential-save", false)
}

func (r *PostgresAWSRepository) GetCredential(ctx context.Context, id string) (agentaws.Credentials, error) {
	return r.getCredential(ctx, id, 0, false)
}

func (r *PostgresAWSRepository) GetCredentialRevision(ctx context.Context, id string, revision int64) (agentaws.Credentials, error) {
	if revision < 1 {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	return r.getCredential(ctx, id, revision, true)
}

func (r *PostgresAWSRepository) GetCredentialRevisionMetadata(ctx context.Context, id string, revision int64) (agentaws.Credentials, error) {
	if r == nil || r.store == nil || r.store.db == nil || strings.TrimSpace(id) == "" || revision < 1 {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	var v struct {
		ID, Name, Region, AccountID, UserARN string
		VerifiedRevision, Revision           int64
		CreatedAt, UpdatedAt                 time.Time
	}
	err := r.store.db.QueryRowContext(ctx, `SELECT credential_id::text,name,region,account_id,user_arn,verified_revision,revision,created_at,updated_at FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$3`, r.ownerID, id, revision).Scan(&v.ID, &v.Name, &v.Region, &v.AccountID, &v.UserARN, &v.VerifiedRevision, &v.Revision, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return agentaws.Credentials{}, agentaws.ErrNotFound
	}
	if err != nil {
		return agentaws.Credentials{}, err
	}
	return agentaws.RehydrateCredentialMetadata(v.ID, v.Name, v.Region, v.AccountID, v.UserARN, v.VerifiedRevision, v.Revision, v.CreatedAt, v.UpdatedAt), nil
}

func (r *PostgresAWSRepository) ListCredentialRevisionMetadata(ctx context.Context, refs []agentaws.CredentialRevisionRef) (map[string]agentaws.Credentials, error) {
	if r == nil || r.store == nil || r.store.db == nil {
		return nil, agentaws.ErrInvalid
	}
	if len(refs) == 0 {
		return map[string]agentaws.Credentials{}, nil
	}
	args := []any{r.ownerID}
	parts := make([]string, 0, len(refs))
	expected := make(map[string]struct{}, len(refs))
	for i, ref := range refs {
		if strings.TrimSpace(ref.ID) == "" || ref.Revision < 1 {
			return nil, agentaws.ErrInvalid
		}
		base := 2 + i*2
		parts = append(parts, fmt.Sprintf("(credential_id=$%d AND revision=$%d)", base, base+1))
		args = append(args, ref.ID, ref.Revision)
		expected[ref.ID+":"+fmt.Sprint(ref.Revision)] = struct{}{}
	}
	query := `SELECT credential_id::text,name,region,account_id,user_arn,verified_revision,revision,created_at,updated_at FROM core_aws_credentials WHERE owner_id=$1 AND (` + strings.Join(parts, " OR ") + `)`
	rows, err := r.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]agentaws.Credentials, len(expected))
	for rows.Next() {
		var id, name, region, account, arn string
		var verified, revision int64
		var created, updated time.Time
		if err = rows.Scan(&id, &name, &region, &account, &arn, &verified, &revision, &created, &updated); err != nil {
			return nil, err
		}
		out[id+":"+fmt.Sprint(revision)] = agentaws.RehydrateCredentialMetadata(id, name, region, account, arn, verified, revision, created, updated)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	if len(out) != len(expected) {
		return nil, agentaws.ErrNotFound
	}
	return out, nil
}

func (r *PostgresAWSRepository) ListCredentials(ctx context.Context, n int, token string) (agentaws.CredentialPage, error) {
	if r == nil || r.store == nil || r.store.db == nil || n < 0 || n > 100 || (token != "" && !validAWSUUID(token)) {
		return agentaws.CredentialPage{}, agentaws.ErrInvalid
	}
	if n == 0 {
		n = 25
	}
	rows, err := r.store.db.QueryContext(ctx, `SELECT c.credential_id::text,c.name,c.region,c.account_id,c.user_arn,c.revision,c.verified_revision,c.created_at,c.updated_at FROM core_aws_credentials c JOIN core_aws_credential_current cur ON cur.owner_id=c.owner_id AND cur.credential_id=c.credential_id AND cur.revision=c.revision AND cur.deleted_at IS NULL WHERE c.owner_id=$1 AND c.credential_id>COALESCE(NULLIF($2,'')::uuid,'00000000-0000-0000-0000-000000000000'::uuid) ORDER BY c.credential_id LIMIT $3`, r.ownerID, token, n+1)
	if err != nil {
		return agentaws.CredentialPage{}, err
	}
	defer rows.Close()
	out := agentaws.CredentialPage{}
	for rows.Next() {
		var v agentaws.CredentialView
		if err := rows.Scan(&v.ID, &v.Name, &v.Region, &v.AccountID, &v.UserARN, &v.Revision, &v.VerifiedRevision, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return agentaws.CredentialPage{}, err
		}
		secret, err := r.loadCredentialSecret(ctx, v.ID, v.Revision)
		if err != nil {
			return agentaws.CredentialPage{}, err
		}
		v.AccessKeyConfigured, v.SecretAccessKeyConfigured, v.SessionTokenConfigured, err = credentialEnvelopeConfigured(r.enveloper, r.ownerID, v.ID, v.Revision, secret.key, secret.nonce, secret.ciphertext)
		if err != nil {
			return agentaws.CredentialPage{}, err
		}
		v.HasAccessKey, v.HasSecretKey, v.HasSessionToken = v.AccessKeyConfigured, v.SecretAccessKeyConfigured, v.SessionTokenConfigured
		out.Items = append(out.Items, v)
	}
	if err := rows.Err(); err != nil {
		return agentaws.CredentialPage{}, err
	}
	if len(out.Items) > n {
		out.NextPageToken = out.Items[n-1].ID
		out.Items = out.Items[:n]
	}
	return out, nil
}

func (r *PostgresAWSRepository) UpdateCredential(ctx context.Context, v agentaws.Credentials, expected int64) (agentaws.Credentials, error) {
	if v.Validate() != nil || v.Revision != expected+1 {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	return r.persistCredential(ctx, v, &expected)
}

func (r *PostgresAWSRepository) ReplaceCredentialIdempotent(ctx context.Context, v agentaws.Credentials, expected int64, key, digest string) (agentaws.CredentialView, error) {
	if !validAWSUUID(key) || v.Validate() != nil || v.Revision != expected+1 {
		return agentaws.CredentialView{}, agentaws.ErrInvalid
	}
	return r.persistCredentialReplay(ctx, v, key, digest, "credential-replace", true)
}

func (r *PostgresAWSRepository) DeleteCredential(ctx context.Context, id string, expected int64) error {
	if r == nil || r.store == nil || r.store.db == nil || !validAWSUUID(id) || expected < 1 {
		return agentaws.ErrInvalid
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE core_aws_credential_current SET deleted_at=clock_timestamp() WHERE owner_id=$1 AND credential_id=$2 AND revision=$3 AND deleted_at IS NULL`, r.ownerID, id, expected)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.ErrRevisionConflict
	}
	return tx.Commit()
}

func (r *PostgresAWSRepository) DeleteCredentialIdempotent(ctx context.Context, id string, expected int64, key, digest string) error {
	if !validAWSUUID(key) {
		return agentaws.ErrInvalid
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, hit, err := credentialReplayTx(ctx, tx, r.ownerID, "credential-delete", key, digest); err != nil {
		return err
	} else if hit {
		return tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `UPDATE core_aws_credential_current SET deleted_at=clock_timestamp() WHERE owner_id=$1 AND credential_id=$2 AND revision=$3 AND deleted_at IS NULL`, r.ownerID, id, expected)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.ErrRevisionConflict
	}
	if err := recordCredentialReplayTx(ctx, tx, r.ownerID, "credential-delete", key, digest, map[string]bool{"deleted": true}); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *PostgresAWSRepository) RecordCredentialIdentity(ctx context.Context, id string, revision int64, identity agentaws.Identity) (agentaws.Credentials, error) {
	if r == nil || r.store == nil || r.store.db == nil || !validAWSUUID(id) || revision < 1 || strings.TrimSpace(identity.AccountID) == "" || strings.TrimSpace(identity.UserARN) == "" {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.Credentials{}, err
	}
	defer tx.Rollback()
	var account, arn sql.NullString
	var verified, current int64
	if err := tx.QueryRowContext(ctx, `SELECT account_id,user_arn,verified_revision,revision FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$3 FOR UPDATE`, r.ownerID, id, revision).Scan(&account, &arn, &verified, &current); errors.Is(err, sql.ErrNoRows) {
		return agentaws.Credentials{}, agentaws.ErrRevisionConflict
	} else if err != nil {
		return agentaws.Credentials{}, err
	}
	if current != revision {
		return agentaws.Credentials{}, agentaws.ErrRevisionConflict
	}
	if verified == revision {
		if account.String != identity.AccountID || arn.String != identity.UserARN {
			return agentaws.Credentials{}, agentaws.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return agentaws.Credentials{}, err
		}
		return r.GetCredentialRevision(ctx, id, revision)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE core_aws_credentials SET account_id=$1,user_arn=$2,verified_revision=revision,updated_at=clock_timestamp() WHERE owner_id=$3 AND credential_id=$4 AND revision=$5`, identity.AccountID, identity.UserARN, r.ownerID, id, revision); err != nil {
		return agentaws.Credentials{}, err
	}
	if err := tx.Commit(); err != nil {
		return agentaws.Credentials{}, err
	}
	return r.GetCredentialRevision(ctx, id, revision)
}

func (r *PostgresAWSRepository) persistCredential(ctx context.Context, v agentaws.Credentials, expected *int64) (agentaws.Credentials, error) {
	if r == nil || r.store == nil || r.store.db == nil || r.enveloper == nil {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	env, digest, err := r.sealCredential(v)
	if err != nil {
		return agentaws.Credentials{}, err
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.Credentials{}, err
	}
	defer tx.Rollback()
	var result sql.Result
	if expected == nil {
		result, err = tx.ExecContext(ctx, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) VALUES($1,$2,$3,1,1,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, r.ownerID, v.ID, v.Revision, env.KeyID, env.Nonce, env.Ciphertext, digest, v.Name, v.Region, v.AccountID, v.UserARN, v.VerifiedRevision, v.CreatedAt, v.UpdatedAt)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO core_aws_credential_current(owner_id,credential_id,revision,deleted_at) VALUES($1,$2,$3,NULL)`, r.ownerID, v.ID, v.Revision)
		}
	} else {
		result, err = tx.ExecContext(ctx, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) SELECT owner_id,$2,$3,1,1,$4,$5,$6,$7,$8,$9,$10,$11,$12,created_at,$13 FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$14`, r.ownerID, v.ID, v.Revision, env.KeyID, env.Nonce, env.Ciphertext, digest, v.Name, v.Region, v.AccountID, v.UserARN, v.VerifiedRevision, v.UpdatedAt, *expected)
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE core_aws_credential_current SET revision=$3,deleted_at=NULL WHERE owner_id=$1 AND credential_id=$2 AND revision=$4 AND deleted_at IS NULL`, r.ownerID, v.ID, v.Revision, *expected)
		}
	}
	if err != nil {
		return agentaws.Credentials{}, mapAWSError(err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return agentaws.Credentials{}, agentaws.ErrRevisionConflict
	}
	binding := credentialBinding(r.ownerID, v.ID, v.Revision)
	if _, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_secrets(secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext,created_at) VALUES('aws',$1,$2,$3,'credential',$2,$4,1,1,$5,$6,$7,$8)`, r.ownerID, v.ID, v.Revision, binding.BindingDigest[:], env.KeyID, env.Nonce, env.Ciphertext, v.UpdatedAt); err != nil {
		return agentaws.Credentials{}, mapAWSError(err)
	}
	if err := tx.Commit(); err != nil {
		return agentaws.Credentials{}, err
	}
	return v, nil
}

func (r *PostgresAWSRepository) persistCredentialReplay(ctx context.Context, v agentaws.Credentials, key, digest, operation string, replace bool) (agentaws.CredentialView, error) {
	env, envelopeDigest, err := r.sealCredential(v)
	if err != nil {
		return agentaws.CredentialView{}, err
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.CredentialView{}, err
	}
	defer tx.Rollback()
	if replay, hit, err := credentialReplayTx(ctx, tx, r.ownerID, operation, key, digest); err != nil {
		return agentaws.CredentialView{}, err
	} else if hit {
		return replay, tx.Commit()
	}
	var expected any = nil
	if replace {
		expected = v.Revision - 1
	}
	var result sql.Result
	if replace {
		result, err = tx.ExecContext(ctx, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) SELECT owner_id,$2,$3,1,1,$4,$5,$6,$7,$8,$9,$10,$11,$12,created_at,$13 FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$14`, r.ownerID, v.ID, v.Revision, env.KeyID, env.Nonce, env.Ciphertext, envelopeDigest, v.Name, v.Region, v.AccountID, v.UserARN, v.VerifiedRevision, v.UpdatedAt, expected)
	} else {
		result, err = tx.ExecContext(ctx, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) VALUES($1,$2,$3,1,1,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, r.ownerID, v.ID, v.Revision, env.KeyID, env.Nonce, env.Ciphertext, envelopeDigest, v.Name, v.Region, v.AccountID, v.UserARN, v.VerifiedRevision, v.CreatedAt, v.UpdatedAt)
	}
	if err != nil {
		return agentaws.CredentialView{}, mapAWSError(err)
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return agentaws.CredentialView{}, agentaws.ErrRevisionConflict
	}
	var currentErr error
	if replace {
		_, currentErr = tx.ExecContext(ctx, `UPDATE core_aws_credential_current SET revision=$3,deleted_at=NULL WHERE owner_id=$1 AND credential_id=$2 AND revision=$4 AND deleted_at IS NULL`, r.ownerID, v.ID, v.Revision, v.Revision-1)
	} else {
		_, currentErr = tx.ExecContext(ctx, `INSERT INTO core_aws_credential_current(owner_id,credential_id,revision,deleted_at) VALUES($1,$2,$3,NULL)`, r.ownerID, v.ID, v.Revision)
	}
	if currentErr != nil {
		return agentaws.CredentialView{}, mapAWSError(currentErr)
	}
	binding := credentialBinding(r.ownerID, v.ID, v.Revision)
	if _, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_secrets(secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext,created_at) VALUES('aws',$1,$2,$3,'credential',$2,$4,1,1,$5,$6,$7,$8)`, r.ownerID, v.ID, v.Revision, binding.BindingDigest[:], env.KeyID, env.Nonce, env.Ciphertext, v.UpdatedAt); err != nil {
		return agentaws.CredentialView{}, mapAWSError(err)
	}
	view := v.View()
	if err = recordCredentialReplayTx(ctx, tx, r.ownerID, operation, key, digest, view); err != nil {
		return agentaws.CredentialView{}, err
	}
	if err = tx.Commit(); err != nil {
		return agentaws.CredentialView{}, err
	}
	return view, nil
}

func (r *PostgresAWSRepository) sealCredential(v agentaws.Credentials) (AgentSecretEnvelope, string, error) {
	a, b, st := v.StoredSecretBytes()
	defer clearBytes(a)
	defer clearBytes(b)
	defer clearBytes(st)
	payload, _ := json.Marshal(struct{ Access, Secret, Session string }{string(a), string(b), string(st)})
	env, err := r.enveloper.Seal(credentialBinding(r.ownerID, v.ID, v.Revision), payload)
	if err != nil {
		return AgentSecretEnvelope{}, "", err
	}
	sum := sha256.Sum256(append(append([]byte{}, env.Nonce...), env.Ciphertext...))
	return env, hex.EncodeToString(sum[:]), nil
}

func (r *PostgresAWSRepository) getCredential(ctx context.Context, id string, revision int64, exact bool) (agentaws.Credentials, error) {
	if r == nil || r.store == nil || r.store.db == nil || strings.TrimSpace(id) == "" {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	if r.enveloper == nil {
		return agentaws.Credentials{}, ErrAgentSecretKeyringUnavailable
	}
	var v struct {
		ID, Name, Region, AccountID, UserARN string
		VerifiedRevision, Revision           int64
		CreatedAt, UpdatedAt                 time.Time
	}
	query := `SELECT c.credential_id::text,c.name,c.region,c.account_id,c.user_arn,c.verified_revision,c.revision,c.created_at,c.updated_at FROM core_aws_credentials c WHERE c.owner_id=$1 AND c.credential_id=$2`
	args := []any{r.ownerID, id}
	if exact {
		query += ` AND c.revision=$3`
		args = append(args, revision)
	} else {
		query += ` AND EXISTS(SELECT 1 FROM core_aws_credential_current cur WHERE cur.owner_id=c.owner_id AND cur.credential_id=c.credential_id AND cur.revision=c.revision AND cur.deleted_at IS NULL)`
	}
	if err := r.store.db.QueryRowContext(ctx, query, args...).Scan(&v.ID, &v.Name, &v.Region, &v.AccountID, &v.UserARN, &v.VerifiedRevision, &v.Revision, &v.CreatedAt, &v.UpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return agentaws.Credentials{}, agentaws.ErrNotFound
	} else if err != nil {
		return agentaws.Credentials{}, err
	}
	secret, err := r.loadCredentialSecret(ctx, v.ID, v.Revision)
	if err != nil {
		return agentaws.Credentials{}, err
	}
	plain, err := r.enveloper.Open(credentialBinding(r.ownerID, v.ID, v.Revision), AgentSecretEnvelope{KeyID: secret.key, Nonce: secret.nonce, Ciphertext: secret.ciphertext})
	if err != nil {
		return agentaws.Credentials{}, err
	}
	defer clearBytes(plain)
	payload, err := decodeCredentialSecretPayload(plain)
	if err != nil {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	return agentaws.RehydrateCredentials(v.ID, v.Name, v.Region, v.AccountID, v.UserARN, []byte(payload.Access), []byte(payload.Secret), []byte(payload.Session), v.VerifiedRevision, v.Revision, v.CreatedAt, v.UpdatedAt), nil
}

type credentialSecretRow struct {
	reference                   string
	bindingDigest               []byte
	envelopeVersion, aadVersion int64
	key                         string
	nonce, ciphertext           []byte
}
type credentialSecretPayload struct{ Access, Secret, Session string }

func decodeCredentialSecretPayload(plain []byte) (credentialSecretPayload, error) {
	var p credentialSecretPayload
	dec := json.NewDecoder(bytes.NewReader(plain))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil || strings.TrimSpace(p.Access) == "" || strings.TrimSpace(p.Secret) == "" {
		return credentialSecretPayload{}, ErrAgentSecretEnvelopeInvalid
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		return credentialSecretPayload{}, ErrAgentSecretEnvelopeInvalid
	}
	return p, nil
}

func (r *PostgresAWSRepository) loadCredentialSecret(ctx context.Context, id string, revision int64) (credentialSecretRow, error) {
	rows, err := r.store.db.QueryContext(ctx, `SELECT reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext FROM p2p_agent_secrets WHERE owner_id=$1 AND entity_id=$2 AND secret_domain='aws' AND purpose='credential' AND secret_revision=$3 ORDER BY created_at,key_id`, r.ownerID, id, revision)
	if err != nil {
		return credentialSecretRow{}, err
	}
	defer rows.Close()
	var out credentialSecretRow
	count := 0
	for rows.Next() {
		if err := rows.Scan(&out.reference, &out.bindingDigest, &out.envelopeVersion, &out.aadVersion, &out.key, &out.nonce, &out.ciphertext); err != nil {
			return credentialSecretRow{}, ErrAgentSecretEnvelopeInvalid
		}
		count++
	}
	if count != 1 || validateCredentialSecretRow(r.ownerID, id, revision, out.reference, out.bindingDigest, out.envelopeVersion, out.aadVersion) != nil {
		return credentialSecretRow{}, ErrAgentSecretEnvelopeInvalid
	}
	return out, nil
}

func validateCredentialSecretRow(owner, id string, revision int64, reference string, digest []byte, envelopeVersion, aadVersion int64) error {
	b := credentialBinding(owner, id, revision)
	if reference != id || !bytes.Equal(digest, b.BindingDigest[:]) || envelopeVersion != 1 || aadVersion != 1 {
		return ErrAgentSecretEnvelopeInvalid
	}
	return nil
}

func credentialEnvelopeConfigured(enveloper *AgentSecretEnveloper, owner, id string, revision int64, key string, nonce, ciphertext []byte) (bool, bool, bool, error) {
	if enveloper == nil {
		return false, false, false, ErrAgentSecretKeyringUnavailable
	}
	plain, err := enveloper.Open(credentialBinding(owner, id, revision), AgentSecretEnvelope{KeyID: key, Nonce: nonce, Ciphertext: ciphertext})
	if err != nil {
		return false, false, false, err
	}
	defer clearBytes(plain)
	p, err := decodeCredentialSecretPayload(plain)
	if err != nil {
		return false, false, false, err
	}
	return p.Access != "", p.Secret != "", p.Session != "", nil
}

func clearBytes(v []byte) {
	for i := range v {
		v[i] = 0
	}
}

func credentialBinding(owner, id string, revision int64) AgentSecretBinding {
	sum := sha256.Sum256([]byte("aws-credential-v1\x00" + owner + "\x00" + id + "\x00" + fmt.Sprint(revision)))
	return AgentSecretBinding{Domain: "aws", OwnerID: owner, EntityID: id, Revision: revision, Purpose: "credential", Reference: id, BindingDigest: sum}
}

func mapAWSError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return agentaws.ErrNotFound
	}
	if strings.Contains(strings.ToLower(err.Error()), "duplicate") || strings.Contains(strings.ToLower(err.Error()), "unique") {
		return agentaws.ErrConflict
	}
	return err
}

const AgentAWSDDL = `CREATE TABLE IF NOT EXISTS core_aws_credentials (
 owner_id text NOT NULL, credential_id uuid NOT NULL, revision bigint NOT NULL CHECK (revision > 0),
 envelope_version integer NOT NULL, aad_version integer NOT NULL, key_id text NOT NULL,
 nonce bytea NOT NULL CHECK (octet_length(nonce)=12), ciphertext bytea NOT NULL,
 envelope_digest text NOT NULL CHECK (envelope_digest ~ '^[a-f0-9]{64}$'), name text NOT NULL,
 region text NOT NULL, account_id text NOT NULL DEFAULT '', user_arn text NOT NULL DEFAULT '',
 verified_revision bigint NOT NULL DEFAULT 0, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 PRIMARY KEY(owner_id,credential_id,revision)
);
CREATE TABLE IF NOT EXISTS core_aws_credential_current (
 owner_id text NOT NULL, credential_id uuid NOT NULL, revision bigint NOT NULL CHECK (revision > 0), deleted_at timestamptz NULL,
 PRIMARY KEY(owner_id,credential_id),
 FOREIGN KEY(owner_id,credential_id,revision) REFERENCES core_aws_credentials(owner_id,credential_id,revision) ON DELETE RESTRICT
);
CREATE TABLE IF NOT EXISTS core_aws_replays (owner_id text NOT NULL, operation text NOT NULL, idempotency_key uuid NOT NULL, request_hash text NOT NULL,
 response_json jsonb NOT NULL, error_code text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(owner_id,operation,idempotency_key));
ALTER TABLE core_aws_credentials ALTER COLUMN key_id DROP NOT NULL;
ALTER TABLE core_aws_credentials ALTER COLUMN nonce DROP NOT NULL;
ALTER TABLE core_aws_credentials ALTER COLUMN ciphertext DROP NOT NULL;`
