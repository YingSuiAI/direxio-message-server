package storage

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/google/uuid"
)

const executionSecretDomain = "execution"

var executionSecretProviderRE = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
var executionSecretNamespace = uuid.NewSHA1(uuid.NameSpaceURL, []byte("https://dirextalk.io/namespaces/execution-secret/v2"))

type ExecutionSecretMetadata struct {
	OwnerID       string               `json:"-"`
	SecretRef     string               `json:"secret_ref"`
	Revision      uint64               `json:"revision"`
	Purpose       string               `json:"purpose"`
	Provider      string               `json:"provider"`
	BindingDigest coreexecution.Digest `json:"binding_digest"`
	Status        string               `json:"status"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

type ExecutionSecretCreateRequest struct {
	OwnerID       string
	Provider      string
	Purpose       string
	Value         string
	IdempotencyID string
}

type ExecutionSecretRevokeRequest struct {
	OwnerID          string
	SecretRef        string
	ExpectedRevision uint64
	IdempotencyID    string
}

type ExecutionSecretPage struct {
	Items      []ExecutionSecretMetadata
	NextCursor string
}

type DatabaseExecutionSecretStore struct {
	db        *sql.DB
	enveloper *AgentSecretEnveloper
	now       func() time.Time
}

func NewDatabaseExecutionSecretStore(db *sql.DB, enveloper *AgentSecretEnveloper, clock func() time.Time) *DatabaseExecutionSecretStore {
	if clock == nil {
		clock = time.Now
	}
	return &DatabaseExecutionSecretStore{db: db, enveloper: enveloper, now: clock}
}

func (s *DatabaseExecutionSecretStore) Ready() bool {
	return s != nil && s.db != nil && s.enveloper != nil
}

func (s *DatabaseExecutionSecretStore) CreateExecutionSecret(ctx context.Context, in ExecutionSecretCreateRequest) (ExecutionSecretMetadata, error) {
	owner, provider, purpose := strings.TrimSpace(in.OwnerID), strings.TrimSpace(in.Provider), strings.TrimSpace(in.Purpose)
	idem, err := uuid.Parse(strings.TrimSpace(in.IdempotencyID))
	if !s.Ready() || owner == "" || !executionSecretProviderRE.MatchString(provider) || len(provider) > 64 || purpose != coreexecution.AISecretPurposeProviderAPIKey || in.Value == "" || len(in.Value) > 16<<10 || in.Value != strings.TrimSpace(in.Value) || strings.ContainsAny(in.Value, "\x00\r\n") || err != nil || idem == uuid.Nil {
		return ExecutionSecretMetadata{}, ErrExecutionStoreInvalid
	}
	secretRef := uuid.NewSHA1(executionSecretNamespace, []byte(owner+"\x00"+idem.String())).String()
	now := s.now().UTC().Truncate(time.Microsecond)
	meta, binding := newExecutionSecretMetadata(owner, secretRef, 1, purpose, provider, "active", now, now)
	plaintext := []byte(in.Value)
	defer clear(plaintext)
	envelope, err := s.enveloper.Seal(binding, plaintext)
	if err != nil {
		return ExecutionSecretMetadata{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionSecretMetadata{}, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO core_execution_secrets(owner_id,secret_ref,revision,purpose,provider,binding_digest,status,mutation_kind,idempotency_key,created_at,updated_at) VALUES($1,$2,1,$3,$4,$5,'active','create',$6,$7,$7) ON CONFLICT (owner_id,idempotency_key) DO NOTHING`, owner, secretRef, purpose, provider, meta.BindingDigest, idem, now)
	if err != nil {
		return ExecutionSecretMetadata{}, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return ExecutionSecretMetadata{}, err
	}
	if inserted == 0 {
		replay, kind, err := loadExecutionSecretByIdempotency(ctx, tx, owner, idem.String())
		if err != nil || kind != "create" || replay.SecretRef != secretRef || replay.Revision != 1 || replay.Purpose != purpose || replay.Provider != provider || replay.Status != "active" {
			return ExecutionSecretMetadata{}, coreexecution.ErrConflict
		}
		opened, err := s.openExecutionSecretTx(ctx, tx, replay)
		if err != nil {
			return ExecutionSecretMetadata{}, err
		}
		equal := subtle.ConstantTimeCompare(opened, plaintext) == 1
		clear(opened)
		if !equal {
			return ExecutionSecretMetadata{}, coreexecution.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return ExecutionSecretMetadata{}, err
		}
		return replay, nil
	}
	digestBytes, _ := hex.DecodeString(string(meta.BindingDigest))
	if _, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_secrets(secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext,created_at) VALUES($1,$2,$3,1,$4,$5,$6,1,1,$7,$8,$9,$10)`, executionSecretDomain, owner, secretRef, purpose, provider, digestBytes, envelope.KeyID, envelope.Nonce, envelope.Ciphertext, now); err != nil {
		return ExecutionSecretMetadata{}, err
	}
	if err := upsertAgentSecretUsageTx(ctx, tx, executionSecretDomain, owner, secretRef, 1, purpose, provider, envelope); err != nil {
		return ExecutionSecretMetadata{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExecutionSecretMetadata{}, err
	}
	return meta, nil
}

func (s *DatabaseExecutionSecretStore) GetExecutionSecret(ctx context.Context, owner, secretRef string, revision uint64) (ExecutionSecretMetadata, error) {
	if !s.Ready() || strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(secretRef) {
		return ExecutionSecretMetadata{}, ErrExecutionStoreInvalid
	}
	return loadExecutionSecret(ctx, s.db, nil, strings.TrimSpace(owner), secretRef, revision, false)
}

func (s *DatabaseExecutionSecretStore) ListExecutionSecrets(ctx context.Context, owner, after string, limit int) (ExecutionSecretPage, error) {
	owner = strings.TrimSpace(owner)
	if !s.Ready() || owner == "" || limit < 1 || limit > 200 || (after != "" && !coreexecution.ValidateUUID(after)) {
		return ExecutionSecretPage{}, ErrExecutionStoreInvalid
	}
	rows, err := s.db.QueryContext(ctx, `SELECT owner_id,secret_ref,revision,purpose,provider,binding_digest,status,created_at,updated_at FROM (SELECT DISTINCT ON (secret_ref) owner_id,secret_ref,revision,purpose,provider,binding_digest,status,created_at,updated_at FROM core_execution_secrets WHERE owner_id=$1 AND ($2='' OR secret_ref::text>$2) ORDER BY secret_ref,revision DESC) current_rows ORDER BY secret_ref LIMIT $3`, owner, after, limit+1)
	if err != nil {
		return ExecutionSecretPage{}, err
	}
	defer rows.Close()
	page := ExecutionSecretPage{Items: make([]ExecutionSecretMetadata, 0, limit)}
	for rows.Next() {
		var item ExecutionSecretMetadata
		if err := scanExecutionSecret(rows, &item); err != nil {
			return ExecutionSecretPage{}, err
		}
		if len(page.Items) == limit {
			page.NextCursor = page.Items[len(page.Items)-1].SecretRef
			break
		}
		page.Items = append(page.Items, item)
	}
	return page, rows.Err()
}

func (s *DatabaseExecutionSecretStore) RevokeExecutionSecret(ctx context.Context, in ExecutionSecretRevokeRequest) (ExecutionSecretMetadata, error) {
	owner := strings.TrimSpace(in.OwnerID)
	idem, err := uuid.Parse(strings.TrimSpace(in.IdempotencyID))
	if !s.Ready() || owner == "" || !coreexecution.ValidateUUID(in.SecretRef) || in.ExpectedRevision == 0 || err != nil || idem == uuid.Nil {
		return ExecutionSecretMetadata{}, ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionSecretMetadata{}, err
	}
	defer tx.Rollback()
	if err = lockExecutionSecretParameterTx(ctx, tx, owner, in.SecretRef); err != nil {
		return ExecutionSecretMetadata{}, err
	}
	var activeLeases int
	if err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM core_execution_secret_parameter_intents WHERE owner_id=$1 AND secret_ref=$2 AND status<>'revoked'`, owner, in.SecretRef).Scan(&activeLeases); err != nil {
		return ExecutionSecretMetadata{}, err
	}
	if activeLeases != 0 {
		return ExecutionSecretMetadata{}, coreexecution.ErrConflict
	}
	if replay, kind, replayErr := loadExecutionSecretByIdempotency(ctx, tx, owner, idem.String()); replayErr == nil {
		if kind != "revoke" || replay.SecretRef != in.SecretRef || replay.Revision != in.ExpectedRevision+1 || replay.Status != "revoked" {
			return ExecutionSecretMetadata{}, coreexecution.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return ExecutionSecretMetadata{}, err
		}
		return replay, nil
	} else if !errors.Is(replayErr, coreexecution.ErrNotFound) {
		return ExecutionSecretMetadata{}, replayErr
	}
	current, err := loadExecutionSecret(ctx, s.db, tx, owner, in.SecretRef, 0, true)
	if err != nil {
		return ExecutionSecretMetadata{}, err
	}
	if current.Revision != in.ExpectedRevision || current.Status != "active" {
		return ExecutionSecretMetadata{}, coreexecution.ErrConflict
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	revoked, _ := newExecutionSecretMetadata(owner, current.SecretRef, current.Revision+1, current.Purpose, current.Provider, "revoked", current.CreatedAt, now)
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_secrets(owner_id,secret_ref,revision,purpose,provider,binding_digest,status,mutation_kind,idempotency_key,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,'revoked','revoke',$7,$8,$9)`, owner, current.SecretRef, revoked.Revision, current.Purpose, current.Provider, revoked.BindingDigest, idem, current.CreatedAt, now); err != nil {
		return ExecutionSecretMetadata{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExecutionSecretMetadata{}, err
	}
	return revoked, nil
}

func (s *DatabaseExecutionSecretStore) ResolveCredential(ctx context.Context, owner string, ref coreexecution.CredentialRef) error {
	meta, err := s.GetExecutionSecret(ctx, owner, ref.Ref, 0)
	if err != nil {
		return err
	}
	if meta.Status != "active" || meta.Revision != ref.Revision || meta.Purpose != ref.Purpose || meta.BindingDigest != ref.BindingDigest {
		return coreexecution.ErrConflict
	}
	return nil
}

func (s *DatabaseExecutionSecretStore) OpenExecutionSecret(ctx context.Context, owner string, ref coreexecution.CredentialRef) ([]byte, error) {
	if err := s.ResolveCredential(ctx, owner, ref); err != nil {
		return nil, err
	}
	meta, err := s.GetExecutionSecret(ctx, owner, ref.Ref, ref.Revision)
	if err != nil {
		return nil, err
	}
	return s.openExecutionSecretTx(ctx, nil, meta)
}

func newExecutionSecretMetadata(owner, ref string, revision uint64, purpose, provider, status string, createdAt, updatedAt time.Time) (ExecutionSecretMetadata, AgentSecretBinding) {
	identity := struct {
		Schema, OwnerID, SecretRef, Purpose, Provider, Status string
		Revision                                              uint64
	}{"execution-secret/v2", owner, ref, purpose, provider, status, revision}
	raw, _ := json.Marshal(identity)
	digest := sha256.Sum256(raw)
	meta := ExecutionSecretMetadata{OwnerID: owner, SecretRef: ref, Revision: revision, Purpose: purpose, Provider: provider, BindingDigest: coreexecution.Digest(hex.EncodeToString(digest[:])), Status: status, CreatedAt: createdAt.UTC(), UpdatedAt: updatedAt.UTC()}
	binding := AgentSecretBinding{Domain: executionSecretDomain, OwnerID: owner, EntityID: ref, Revision: int64(revision), Purpose: purpose, Reference: provider, BindingDigest: digest}
	return meta, binding
}

func (s *DatabaseExecutionSecretStore) openExecutionSecretTx(ctx context.Context, tx *sql.Tx, meta ExecutionSecretMetadata) ([]byte, error) {
	query := `SELECT key_id,nonce,ciphertext,binding_digest FROM p2p_agent_secrets WHERE secret_domain=$1 AND owner_id=$2 AND entity_id=$3 AND secret_revision=$4 AND purpose=$5 AND reference=$6`
	var keyID string
	var nonce, ciphertext, storedDigest []byte
	var err error
	if tx != nil {
		err = tx.QueryRowContext(ctx, query, executionSecretDomain, meta.OwnerID, meta.SecretRef, meta.Revision, meta.Purpose, meta.Provider).Scan(&keyID, &nonce, &ciphertext, &storedDigest)
	} else {
		err = s.db.QueryRowContext(ctx, query, executionSecretDomain, meta.OwnerID, meta.SecretRef, meta.Revision, meta.Purpose, meta.Provider).Scan(&keyID, &nonce, &ciphertext, &storedDigest)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, coreexecution.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	expected, binding := newExecutionSecretMetadata(meta.OwnerID, meta.SecretRef, meta.Revision, meta.Purpose, meta.Provider, meta.Status, meta.CreatedAt, meta.UpdatedAt)
	if expected.BindingDigest != meta.BindingDigest || subtle.ConstantTimeCompare(storedDigest, binding.BindingDigest[:]) != 1 {
		return nil, ErrExecutionStoreDrift
	}
	return s.enveloper.Open(binding, AgentSecretEnvelope{KeyID: keyID, Nonce: nonce, Ciphertext: ciphertext})
}

type executionSecretScanner interface{ Scan(...any) error }

func scanExecutionSecret(row executionSecretScanner, out *ExecutionSecretMetadata) error {
	if err := row.Scan(&out.OwnerID, &out.SecretRef, &out.Revision, &out.Purpose, &out.Provider, &out.BindingDigest, &out.Status, &out.CreatedAt, &out.UpdatedAt); err != nil {
		return err
	}
	out.CreatedAt = out.CreatedAt.UTC()
	out.UpdatedAt = out.UpdatedAt.UTC()
	return nil
}

func loadExecutionSecret(ctx context.Context, db *sql.DB, tx *sql.Tx, owner, ref string, revision uint64, lock bool) (ExecutionSecretMetadata, error) {
	query := `SELECT owner_id,secret_ref,revision,purpose,provider,binding_digest,status,created_at,updated_at FROM core_execution_secrets WHERE owner_id=$1 AND secret_ref=$2`
	args := []any{owner, ref}
	if revision > 0 {
		query += ` AND revision=$3`
		args = append(args, revision)
	} else {
		query += ` ORDER BY revision DESC LIMIT 1`
	}
	if lock {
		query += ` FOR UPDATE`
	}
	var out ExecutionSecretMetadata
	var err error
	if tx != nil {
		err = scanExecutionSecret(tx.QueryRowContext(ctx, query, args...), &out)
	} else {
		err = scanExecutionSecret(db.QueryRowContext(ctx, query, args...), &out)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionSecretMetadata{}, coreexecution.ErrNotFound
	}
	return out, err
}

func loadExecutionSecretByIdempotency(ctx context.Context, tx *sql.Tx, owner, idempotency string) (ExecutionSecretMetadata, string, error) {
	row := tx.QueryRowContext(ctx, `SELECT owner_id,secret_ref,revision,purpose,provider,binding_digest,status,created_at,updated_at,mutation_kind FROM core_execution_secrets WHERE owner_id=$1 AND idempotency_key=$2 FOR UPDATE`, owner, idempotency)
	var out ExecutionSecretMetadata
	var kind string
	err := row.Scan(&out.OwnerID, &out.SecretRef, &out.Revision, &out.Purpose, &out.Provider, &out.BindingDigest, &out.Status, &out.CreatedAt, &out.UpdatedAt, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionSecretMetadata{}, "", coreexecution.ErrNotFound
	}
	if err == nil {
		out.CreatedAt = out.CreatedAt.UTC()
		out.UpdatedAt = out.UpdatedAt.UTC()
	}
	return out, kind, err
}
