package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	agentaws "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

// AWSCredentialEnvelope is the persistence-safe hand-off to the credential
// owner. Secret bytes are intentionally opaque to this package; callers must
// resolve a specific owner-scoped revision and never persist plaintext here.
type AWSCredentialEnvelope struct {
	OwnerID   string
	Reference string
	Revision  int64
	Digest    string
	Domain    string
	Purpose   string
	KeyID     string
	Opaque    []byte
}

// AWSCredentialResolver is the narrow integration seam for the credential,
// confirmation and secret subsystems owned by the parent service.
type AWSCredentialResolver interface {
	ResolveCredential(context.Context, string, int64) (AWSCredentialEnvelope, error)
}

// AWSRepository aliases the typed domain repository without widening the
// storage boundary to arbitrary SQL or provider calls.
type AWSRepository interface{ agentaws.Repository }

// PostgresAWSRepository is the single-process adapter seam. The owner and
// revision are fixed at construction; callers cannot cross owners. SQL
// persistence is supplied by the parent migration/wiring layer while the
// typed domain repository remains the only public contract.
type PostgresAWSRepository struct {
	store     *DatabaseStore
	ownerID   string
	enveloper *AgentSecretEnveloper
}

var _ agentaws.Repository = (*PostgresAWSRepository)(nil)
var _ agentaws.ChangeCoordinator = (*PostgresAWSRepository)(nil)

func validAWSUUID(v string) bool {
	id, err := uuid.Parse(strings.TrimSpace(v))
	return err == nil && id != uuid.Nil && id.String() == strings.TrimSpace(v)
}
func stringDigest(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ReplayCredential reads the durable response snapshot before a caller tries
// to reconstruct a mutation from current state (which may already have moved
// on or be tombstoned after a lost response).
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

func credentialReplayTx(ctx context.Context, tx *sql.Tx, owner, operation, key, digest string) (agentaws.CredentialView, bool, error) {
	// A missing replay row has no row lock. Serialize that key explicitly so
	// concurrent lost-response retries cannot both perform the side effect and
	// race only at the final replay insert.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, owner+"\x00"+operation+"\x00"+key); err != nil {
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

func (r *PostgresAWSRepository) RequestChange(ctx context.Context, in agentaws.RequestChangeInput) (agentaws.ChangeRequestResult, error) {
	if !validAWSUUID(in.PlanID) || !validAWSUUID(in.IdempotencyKey) {
		return agentaws.ChangeRequestResult{}, agentaws.ErrInvalid
	}
	p, err := r.GetPlan(ctx, in.PlanID)
	if err != nil {
		return agentaws.ChangeRequestResult{}, err
	}
	now := time.Now().UTC()
	changeID, taskID, confID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	b := in.Binding
	if b.OperationDomain == "" {
		cred, e := r.GetCredentialRevision(ctx, p.CredentialID, p.CredentialRevision)
		if e != nil {
			return agentaws.ChangeRequestResult{}, e
		}
		b = agentaws.BindingForPlan(p, cred)
	}
	b.OwnerID = r.ownerID
	b, err = b.Normalize()
	if err != nil {
		return agentaws.ChangeRequestResult{}, agentaws.ErrInvalid
	}
	spec, _ := (coretask.TaskSpec{Kind: coretask.TaskKindAWSChange, Payload: coretask.TaskPayload{AWSChange: &coretask.AWSChangeTaskPayload{ChangeID: changeID}}, Goal: "AWS change", IdempotencyKey: uuid.NewString(), AvailableAt: now}).Normalize()
	specRaw, _ := json.Marshal(spec)
	bRaw, _ := json.Marshal(b)
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.ChangeRequestResult{}, err
	}
	defer tx.Rollback()
	replayDigest := stringDigest(struct {
		PlanID  string
		Binding coreconfirmation.Binding
	}{in.PlanID, in.Binding})
	var priorHash string
	var priorRaw []byte
	if e := tx.QueryRowContext(ctx, `SELECT request_hash,response_json FROM core_aws_replays WHERE owner_id=$1 AND operation='change-request' AND idempotency_key=$2 FOR UPDATE`, r.ownerID, in.IdempotencyKey).Scan(&priorHash, &priorRaw); e == nil {
		if priorHash != replayDigest {
			return agentaws.ChangeRequestResult{}, agentaws.ErrIdempotencyConflict
		}
		var out agentaws.ChangeRequestResult
		if json.Unmarshal(priorRaw, &out) != nil {
			return agentaws.ChangeRequestResult{}, agentaws.ErrConflict
		}
		return out, tx.Commit()
	} else if !errors.Is(e, sql.ErrNoRows) {
		return agentaws.ChangeRequestResult{}, e
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,revision,available_at,created_at,updated_at) VALUES($1,$2,$3,'waiting_user',1,1,$4,$4,$4)`, taskID, r.ownerID, specRaw, now); err != nil {
		return agentaws.ChangeRequestResult{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,created_at,updated_at) VALUES($1,$2,'aws',$3,$4,$5,$6,$7,'pending',1,$8,$9,$9)`, confID, r.ownerID, p.ID, p.Revision, b.Digest, bRaw, taskID, now.Add(24*time.Hour), now); err != nil {
		return agentaws.ChangeRequestResult{}, err
	}
	v := agentaws.Change{ID: changeID, PlanID: p.ID, CredentialID: p.CredentialID, TaskID: taskID, ConfirmationID: confID, Operation: p.Operation, Status: agentaws.ChangeWaitingUser, Stage: agentaws.StageRequested, Revision: 1, ProviderToken: confID, ProviderRequestDigest: stringDigest(struct {
		Plan  string
		Token string
	}{p.ID, confID}), CreatedAt: now, UpdatedAt: now}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_changes(owner_id,change_id,plan_id,credential_id,credential_revision,task_id,confirmation_id,operation,status,stage,provider_token,provider_request_digest,revision,created_at,updated_at) VALUES($1,$2,$3,$4,(SELECT credential_revision FROM core_aws_plans WHERE owner_id=$1 AND plan_id=$3),$5,$6,$7,$8,$9,$10,$11,1,$12,$12)`, r.ownerID, v.ID, v.PlanID, v.CredentialID, v.TaskID, v.ConfirmationID, v.Operation, v.Status, v.Stage, v.ProviderToken, v.ProviderRequestDigest, now); err != nil {
		return agentaws.ChangeRequestResult{}, err
	}
	out := agentaws.ChangeRequestResult{Change: v, Task: agentaws.Task{ID: taskID, Status: "waiting_user", Revision: 1, PlanID: p.ID, ConfirmationID: confID}, Confirmation: coreconfirmation.Confirmation{ConfirmationID: confID, OwnerID: r.ownerID, Binding: b, TaskID: taskID, State: coreconfirmation.StatePending, Revision: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: now.Add(24 * time.Hour)}}
	outRaw, _ := json.Marshal(out)
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json) VALUES($1,'change-request',$2,$3,$4)`, r.ownerID, in.IdempotencyKey, replayDigest, outRaw); err != nil {
		return agentaws.ChangeRequestResult{}, err
	}
	if err = tx.Commit(); err != nil {
		return agentaws.ChangeRequestResult{}, err
	}
	return out, nil
}

func NewAgentAWSRepository(store *DatabaseStore, ownerID string) (*PostgresAWSRepository, error) {
	if store == nil || strings.TrimSpace(ownerID) == "" {
		return nil, errors.New("storage: invalid AWS repository owner")
	}
	return &PostgresAWSRepository{store: store, ownerID: ownerID}, nil
}

// NewAgentAWSRepositoryWithEnveloper is the production constructor.  The
// enveloper is deliberately injected so credentials can only cross this
// boundary as authenticated ciphertext; no plaintext column or SQL argument
// is ever used.
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

// Memory-backed methods are intentionally kept behind this owner-fenced
// adapter until the parent migration wires the encrypted envelope columns.
// No method accepts or returns a plaintext secret envelope.
func (r *PostgresAWSRepository) CreateCredential(c context.Context, v agentaws.Credentials) (agentaws.Credentials, error) {
	if r == nil || r.store == nil || r.store.db == nil || v.Validate() != nil || r.enveloper == nil {
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
	a, b, st := v.StoredSecretBytes()
	defer clearBytes(a)
	defer clearBytes(b)
	defer clearBytes(st)
	binding := credentialBinding(r.ownerID, v.ID, v.Revision)
	payload, _ := json.Marshal(struct{ Access, Secret, Session string }{string(a), string(b), string(st)})
	env, err := r.enveloper.Seal(binding, payload)
	if err != nil {
		return agentaws.Credentials{}, err
	}
	digest := sha256.Sum256(append(append(append([]byte{}, env.Nonce...), env.Ciphertext...), nil...))
	digestHex := hex.EncodeToString(digest[:])
	tx, err := r.store.db.BeginTx(c, nil)
	if err != nil {
		return agentaws.Credentials{}, err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(c, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) VALUES($1,$2,$3,1,1,'',NULL,NULL,$4,$5,$6,$7,$8,$9,$10,$11)`, r.ownerID, v.ID, v.Revision, digestHex, v.Name, v.Region, v.AccountID, v.UserARN, v.VerifiedRevision, v.CreatedAt, v.UpdatedAt)
	if err != nil {
		return agentaws.Credentials{}, mapAWSError(err)
	}
	if _, err = tx.ExecContext(c, `INSERT INTO core_aws_credential_current(owner_id,credential_id,revision,deleted_at) VALUES($1,$2,$3,NULL)`, r.ownerID, v.ID, v.Revision); err != nil {
		return agentaws.Credentials{}, mapAWSError(err)
	}
	if _, err = tx.ExecContext(c, `INSERT INTO p2p_agent_secrets(secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,1,1,$8,$9,$10,$11)`, "aws", r.ownerID, v.ID, v.Revision, "credential", v.ID, binding.BindingDigest[:], env.KeyID, env.Nonce, env.Ciphertext, v.CreatedAt); err != nil {
		return agentaws.Credentials{}, mapAWSError(err)
	}
	if err = tx.Commit(); err != nil {
		return agentaws.Credentials{}, err
	}
	return v, nil
}

func (r *PostgresAWSRepository) SaveCredentialIdempotent(ctx context.Context, v agentaws.Credentials, key, digest string) (agentaws.CredentialView, error) {
	if r == nil || r.store == nil || r.store.db == nil || r.enveloper == nil || v.Validate() != nil || !validAWSUUID(key) {
		return agentaws.CredentialView{}, agentaws.ErrInvalid
	}
	a, b, st := v.StoredSecretBytes()
	defer clearBytes(a)
	defer clearBytes(b)
	defer clearBytes(st)
	payload, _ := json.Marshal(struct{ Access, Secret, Session string }{string(a), string(b), string(st)})
	binding := credentialBinding(r.ownerID, v.ID, v.Revision)
	env, err := r.enveloper.Seal(binding, payload)
	if err != nil {
		return agentaws.CredentialView{}, err
	}
	sum := sha256.Sum256(append(append([]byte{}, env.Nonce...), env.Ciphertext...))
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.CredentialView{}, err
	}
	defer tx.Rollback()
	if replay, hit, e := credentialReplayTx(ctx, tx, r.ownerID, "credential-save", key, digest); hit {
		if e != nil {
			return agentaws.CredentialView{}, e
		}
		return replay, tx.Commit()
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) VALUES($1,$2,$3,1,1,'',NULL,NULL,$4,$5,$6,$7,$8,$9,$10,$11)`, r.ownerID, v.ID, v.Revision, hex.EncodeToString(sum[:]), v.Name, v.Region, v.AccountID, v.UserARN, v.VerifiedRevision, v.CreatedAt, v.UpdatedAt); err != nil {
		return agentaws.CredentialView{}, mapAWSError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_credential_current(owner_id,credential_id,revision,deleted_at) VALUES($1,$2,$3,NULL)`, r.ownerID, v.ID, v.Revision); err != nil {
		return agentaws.CredentialView{}, mapAWSError(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_secrets(secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext,created_at) VALUES('aws',$1,$2,$3,'credential',$2,$4,1,1,$5,$6,$7,$8)`, r.ownerID, v.ID, v.Revision, binding.BindingDigest[:], env.KeyID, env.Nonce, env.Ciphertext, v.CreatedAt); err != nil {
		return agentaws.CredentialView{}, mapAWSError(err)
	}
	view := v.View()
	if err = recordCredentialReplayTx(ctx, tx, r.ownerID, "credential-save", key, digest, view); err != nil {
		return agentaws.CredentialView{}, err
	}
	if err = tx.Commit(); err != nil {
		return agentaws.CredentialView{}, err
	}
	return view, nil
}
func (r *PostgresAWSRepository) GetCredential(c context.Context, id string) (agentaws.Credentials, error) {
	return r.getCredential(c, id, 0, false)
}
func (r *PostgresAWSRepository) GetCredentialRevision(c context.Context, id string, revision int64) (agentaws.Credentials, error) {
	if revision < 1 {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	return r.getCredential(c, id, revision, true)
}
func (r *PostgresAWSRepository) getCredential(c context.Context, id string, revision int64, exact bool) (agentaws.Credentials, error) {
	if r == nil || r.store == nil || r.store.db == nil || strings.TrimSpace(id) == "" {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	var v agentaws.Credentials
	var key string
	var nonce, ciphertext []byte
	query := `SELECT c.credential_id::text,c.name,c.region,c.account_id,c.user_arn,c.verified_revision,c.revision,c.created_at,c.updated_at,s.key_id,s.nonce,s.ciphertext FROM core_aws_credentials c JOIN p2p_agent_secrets s ON s.owner_id=c.owner_id AND s.entity_id=c.credential_id::text AND s.secret_revision=c.revision AND s.secret_domain='aws' AND s.purpose='credential' WHERE c.owner_id=$1 AND c.credential_id=$2`
	args := []any{r.ownerID, id}
	if exact {
		query += ` AND c.revision=$3`
		args = append(args, revision)
	} else {
		query += ` AND EXISTS(SELECT 1 FROM core_aws_credential_current cur WHERE cur.owner_id=c.owner_id AND cur.credential_id=c.credential_id AND cur.revision=c.revision AND cur.deleted_at IS NULL)`
	}
	err := r.store.db.QueryRowContext(c, query, args...).Scan(&v.ID, &v.Name, &v.Region, &v.AccountID, &v.UserARN, &v.VerifiedRevision, &v.Revision, &v.CreatedAt, &v.UpdatedAt, &key, &nonce, &ciphertext)
	if errors.Is(err, sql.ErrNoRows) {
		return agentaws.Credentials{}, agentaws.ErrNotFound
	}
	if err != nil {
		return agentaws.Credentials{}, err
	}
	if r.enveloper == nil {
		return v, nil
	}
	plain, err := r.enveloper.Open(credentialBinding(r.ownerID, v.ID, v.Revision), AgentSecretEnvelope{KeyID: key, Nonce: nonce, Ciphertext: ciphertext})
	if err != nil {
		return agentaws.Credentials{}, err
	}
	defer clearBytes(plain)
	if len(plain) < 2 {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	// Secret payload is length-delimited to avoid ambiguity between optional
	// session tokens and the required access/secret pair.
	var payload struct{ Access, Secret, Session string }
	if json.Unmarshal(plain, &payload) != nil {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	return agentaws.RehydrateCredentials(v.ID, v.Name, v.Region, v.AccountID, v.UserARN, []byte(payload.Access), []byte(payload.Secret), []byte(payload.Session), v.VerifiedRevision, v.Revision, v.CreatedAt, v.UpdatedAt), nil
}
func (r *PostgresAWSRepository) ListCredentials(c context.Context, n int, k string) (agentaws.CredentialPage, error) {
	k = strings.TrimSpace(k)
	if n < 0 || n > 100 || r == nil || r.store == nil || r.store.db == nil || (k != "" && !validAWSUUID(k)) {
		return agentaws.CredentialPage{}, agentaws.ErrInvalid
	}
	rows, err := r.store.db.QueryContext(c, `SELECT c.credential_id::text,c.name,c.region,c.account_id,c.user_arn,c.revision,c.created_at,c.updated_at,EXISTS(SELECT 1 FROM p2p_agent_secrets s WHERE s.owner_id=c.owner_id AND s.entity_id=c.credential_id::text AND s.secret_domain='aws' AND s.purpose='credential' AND s.secret_revision=c.revision) FROM core_aws_credentials c JOIN core_aws_credential_current cur ON cur.owner_id=c.owner_id AND cur.credential_id=c.credential_id AND cur.revision=c.revision AND cur.deleted_at IS NULL WHERE c.owner_id=$1 AND c.credential_id>COALESCE(NULLIF($2,'')::uuid,'00000000-0000-0000-0000-000000000000'::uuid) ORDER BY c.credential_id LIMIT $3`, r.ownerID, k, n+1)
	if err != nil {
		return agentaws.CredentialPage{}, err
	}
	defer rows.Close()
	out := agentaws.CredentialPage{}
	for rows.Next() {
		var v agentaws.CredentialView
		var configured bool
		if err := rows.Scan(&v.ID, &v.Name, &v.Region, &v.AccountID, &v.UserARN, &v.Revision, &v.CreatedAt, &v.UpdatedAt, &configured); err != nil {
			return out, err
		}
		v.HasAccessKey = configured
		v.HasSecretKey = configured
		out.Items = append(out.Items, v)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if len(out.Items) > n {
		out.NextPageToken = out.Items[n-1].ID
		out.Items = out.Items[:n]
	}
	return out, nil
}
func (r *PostgresAWSRepository) UpdateCredential(c context.Context, v agentaws.Credentials, rev int64) (agentaws.Credentials, error) {
	if r == nil || r.enveloper == nil || v.Validate() != nil || v.Revision != rev+1 {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	// A credential update is an immutable new revision; old envelopes remain
	// addressable for pinned plans and are never overwritten.
	return r.createCredentialRevision(c, v, rev)
}
func (r *PostgresAWSRepository) DeleteCredential(c context.Context, id string, rev int64) error {
	if r == nil || r.store == nil || r.store.db == nil || strings.TrimSpace(id) == "" || rev < 1 {
		return agentaws.ErrInvalid
	}
	tx, err := r.store.db.BeginTx(c, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(c, `UPDATE core_aws_credential_current SET deleted_at=clock_timestamp() WHERE owner_id=$1 AND credential_id=$2 AND revision=$3 AND deleted_at IS NULL`, r.ownerID, id, rev)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.ErrRevisionConflict
	}
	return tx.Commit()
}

func (r *PostgresAWSRepository) DeleteCredentialIdempotent(ctx context.Context, id string, rev int64, key, digest string) error {
	if r == nil || r.store == nil || r.store.db == nil || !validAWSUUID(id) || rev < 1 || !validAWSUUID(key) {
		return agentaws.ErrInvalid
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, hit, e := credentialReplayTx(ctx, tx, r.ownerID, "credential-delete", key, digest); hit {
		if e != nil {
			return e
		}
		return tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `UPDATE core_aws_credential_current SET deleted_at=clock_timestamp() WHERE owner_id=$1 AND credential_id=$2 AND revision=$3 AND deleted_at IS NULL`, r.ownerID, id, rev)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.ErrRevisionConflict
	}
	if err = recordCredentialReplayTx(ctx, tx, r.ownerID, "credential-delete", key, digest, map[string]bool{"deleted": true}); err != nil {
		return err
	}
	return tx.Commit()
}
func (r *PostgresAWSRepository) RecordCredentialIdentity(c context.Context, id string, rev int64, i agentaws.Identity) (agentaws.Credentials, error) {
	if r == nil || r.store == nil || r.store.db == nil {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	res, err := r.store.db.ExecContext(c, `UPDATE core_aws_credentials SET account_id=$1,user_arn=$2,verified_revision=revision,updated_at=clock_timestamp() WHERE owner_id=$3 AND credential_id=$4 AND revision=$5`, i.AccountID, i.UserARN, r.ownerID, id, rev)
	if err != nil {
		return agentaws.Credentials{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.Credentials{}, agentaws.ErrRevisionConflict
	}
	return r.GetCredentialRevision(c, id, rev)
}

func (r *PostgresAWSRepository) createCredentialRevision(c context.Context, v agentaws.Credentials, expected int64) (agentaws.Credentials, error) {
	if r.store == nil || r.store.db == nil {
		return agentaws.Credentials{}, agentaws.ErrInvalid
	}
	a, b, st := v.StoredSecretBytes()
	defer clearBytes(a)
	defer clearBytes(b)
	defer clearBytes(st)
	binding := credentialBinding(r.ownerID, v.ID, v.Revision)
	payload, _ := json.Marshal(struct{ Access, Secret, Session string }{string(a), string(b), string(st)})
	env, err := r.enveloper.Seal(binding, payload)
	if err != nil {
		return agentaws.Credentials{}, err
	}
	sum := sha256.Sum256(append(append([]byte{}, env.Nonce...), env.Ciphertext...))
	digest := hex.EncodeToString(sum[:])
	now := v.UpdatedAt.UTC()
	if now.IsZero() {
		now = time.Now().UTC()
	}
	tx, err := r.store.db.BeginTx(c, nil)
	if err != nil {
		return agentaws.Credentials{}, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(c, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) SELECT owner_id,$2,$3,1,1,'',NULL,NULL,$4,$5,$6,$7,$8,$9,created_at,$10 FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$11`, r.ownerID, v.ID, v.Revision, digest, v.Name, v.Region, v.AccountID, v.UserARN, v.VerifiedRevision, now, expected)
	if err != nil {
		return agentaws.Credentials{}, mapAWSError(err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.Credentials{}, agentaws.ErrRevisionConflict
	}
	res, err = tx.ExecContext(c, `UPDATE core_aws_credential_current SET revision=$3,deleted_at=NULL WHERE owner_id=$1 AND credential_id=$2 AND revision=$4 AND deleted_at IS NULL`, r.ownerID, v.ID, v.Revision, expected)
	if err != nil {
		return agentaws.Credentials{}, err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return agentaws.Credentials{}, agentaws.ErrRevisionConflict
	}
	if _, err = tx.ExecContext(c, `INSERT INTO p2p_agent_secrets(secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext,created_at) VALUES('aws',$1,$2,$3,'credential',$2,$4,1,1,$5,$6,$7,$8)`, r.ownerID, v.ID, v.Revision, binding.BindingDigest[:], env.KeyID, env.Nonce, env.Ciphertext, now); err != nil {
		return agentaws.Credentials{}, mapAWSError(err)
	}
	if err = tx.Commit(); err != nil {
		return agentaws.Credentials{}, err
	}
	return v, nil
}

func (r *PostgresAWSRepository) ReplaceCredentialIdempotent(ctx context.Context, v agentaws.Credentials, expected int64, key, digest string) (agentaws.CredentialView, error) {
	if r == nil || r.store == nil || r.store.db == nil || r.enveloper == nil || v.Validate() != nil || v.Revision != expected+1 || !validAWSUUID(key) {
		return agentaws.CredentialView{}, agentaws.ErrInvalid
	}
	a, b, st := v.StoredSecretBytes()
	defer clearBytes(a)
	defer clearBytes(b)
	defer clearBytes(st)
	payload, _ := json.Marshal(struct{ Access, Secret, Session string }{string(a), string(b), string(st)})
	binding := credentialBinding(r.ownerID, v.ID, v.Revision)
	env, err := r.enveloper.Seal(binding, payload)
	if err != nil {
		return agentaws.CredentialView{}, err
	}
	sum := sha256.Sum256(append(append([]byte{}, env.Nonce...), env.Ciphertext...))
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.CredentialView{}, err
	}
	defer tx.Rollback()
	if replay, hit, e := credentialReplayTx(ctx, tx, r.ownerID, "credential-replace", key, digest); hit {
		if e != nil {
			return agentaws.CredentialView{}, e
		}
		return replay, tx.Commit()
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO core_aws_credentials(owner_id,credential_id,revision,envelope_version,aad_version,key_id,nonce,ciphertext,envelope_digest,name,region,account_id,user_arn,verified_revision,created_at,updated_at) SELECT owner_id,$2,$3,1,1,'',NULL,NULL,$4,$5,$6,$7,$8,$9,created_at,$10 FROM core_aws_credentials WHERE owner_id=$1 AND credential_id=$2 AND revision=$11`, r.ownerID, v.ID, v.Revision, hex.EncodeToString(sum[:]), v.Name, v.Region, v.AccountID, v.UserARN, v.VerifiedRevision, v.UpdatedAt, expected)
	if err != nil {
		return agentaws.CredentialView{}, mapAWSError(err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.CredentialView{}, agentaws.ErrRevisionConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE core_aws_credential_current SET revision=$3,deleted_at=NULL WHERE owner_id=$1 AND credential_id=$2 AND revision=$4 AND deleted_at IS NULL`, r.ownerID, v.ID, v.Revision, expected)
	if err != nil {
		return agentaws.CredentialView{}, err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return agentaws.CredentialView{}, agentaws.ErrRevisionConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_secrets(secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext,created_at) VALUES('aws',$1,$2,$3,'credential',$2,$4,1,1,$5,$6,$7,$8)`, r.ownerID, v.ID, v.Revision, binding.BindingDigest[:], env.KeyID, env.Nonce, env.Ciphertext, v.UpdatedAt); err != nil {
		return agentaws.CredentialView{}, mapAWSError(err)
	}
	view := v.View()
	if err = recordCredentialReplayTx(ctx, tx, r.ownerID, "credential-replace", key, digest, view); err != nil {
		return agentaws.CredentialView{}, err
	}
	if err = tx.Commit(); err != nil {
		return agentaws.CredentialView{}, err
	}
	return view, nil
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
func (r *PostgresAWSRepository) CreatePlan(c context.Context, v agentaws.Plan) (agentaws.Plan, error) {
	if r == nil || r.store == nil || r.store.db == nil {
		return agentaws.Plan{}, agentaws.ErrInvalid
	}
	if err := v.Validate(); err != nil {
		return agentaws.Plan{}, err
	}
	params, _ := json.Marshal(v.Parameters)
	tags, _ := json.Marshal(v.Tags)
	caps, _ := json.Marshal(v.Capabilities)
	var credentialRevision int64
	if err := r.store.db.QueryRowContext(c, `SELECT revision FROM core_aws_credential_current WHERE owner_id=$1 AND credential_id=$2 AND deleted_at IS NULL`, r.ownerID, v.CredentialID).Scan(&credentialRevision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentaws.Plan{}, agentaws.ErrNotFound
		}
		return agentaws.Plan{}, err
	}
	if v.CredentialRevision != credentialRevision {
		return agentaws.Plan{}, agentaws.ErrRevisionConflict
	}
	_, err := r.store.db.ExecContext(c, `INSERT INTO core_aws_plans(owner_id,plan_id,credential_id,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, r.ownerID, v.ID, v.CredentialID, credentialRevision, v.Region, v.StackName, v.Operation, v.Template, v.TemplateSHA256, params, tags, caps, v.Revision, v.CreatedAt)
	return v, mapAWSError(err)
}
func (r *PostgresAWSRepository) GetPlan(c context.Context, id string) (agentaws.Plan, error) {
	var v agentaws.Plan
	var params, tags, caps []byte
	err := r.store.db.QueryRowContext(c, `SELECT plan_id::text,credential_id::text,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at FROM core_aws_plans WHERE owner_id=$1 AND plan_id=$2`, r.ownerID, id).Scan(&v.ID, &v.CredentialID, &v.CredentialRevision, &v.Region, &v.StackName, &v.Operation, &v.Template, &v.TemplateSHA256, &params, &tags, &caps, &v.Revision, &v.CreatedAt)
	if err != nil {
		return v, err
	}
	_ = json.Unmarshal(params, &v.Parameters)
	_ = json.Unmarshal(tags, &v.Tags)
	_ = json.Unmarshal(caps, &v.Capabilities)
	return v, nil
}
func (r *PostgresAWSRepository) ListPlans(c context.Context, n int, k string) (agentaws.PlanPage, error) {
	k = strings.TrimSpace(k)
	if r == nil || r.store == nil || r.store.db == nil || n < 0 || n > 100 || (k != "" && !validAWSUUID(k)) {
		return agentaws.PlanPage{}, agentaws.ErrInvalid
	}
	rows, err := r.store.db.QueryContext(c, `SELECT plan_id::text,credential_id::text,credential_revision,region,stack_name,operation,template,template_sha256,parameters_json,tags_json,capabilities_json,revision,created_at FROM core_aws_plans WHERE owner_id=$1 AND plan_id>COALESCE(NULLIF($2,'')::uuid,'00000000-0000-0000-0000-000000000000'::uuid) ORDER BY plan_id LIMIT $3`, r.ownerID, k, n+1)
	if err != nil {
		return agentaws.PlanPage{}, err
	}
	defer rows.Close()
	page := agentaws.PlanPage{}
	for rows.Next() {
		var v agentaws.Plan
		var params, tags, caps []byte
		if err := rows.Scan(&v.ID, &v.CredentialID, &v.CredentialRevision, &v.Region, &v.StackName, &v.Operation, &v.Template, &v.TemplateSHA256, &params, &tags, &caps, &v.Revision, &v.CreatedAt); err != nil {
			return page, err
		}
		_ = json.Unmarshal(params, &v.Parameters)
		_ = json.Unmarshal(tags, &v.Tags)
		_ = json.Unmarshal(caps, &v.Capabilities)
		page.Items = append(page.Items, v.View())
	}
	if n > 0 && len(page.Items) > n {
		page.Items = page.Items[:n]
		page.NextPageToken = page.Items[n-1].ID
	}
	return page, rows.Err()
}
func (r *PostgresAWSRepository) CreateChange(c context.Context, v agentaws.Change) (agentaws.Change, error) {
	if r == nil || r.store == nil || r.store.db == nil || v.ID == "" || v.PlanID == "" {
		return agentaws.Change{}, agentaws.ErrInvalid
	}
	var credentialRevision int64
	if err := r.store.db.QueryRowContext(c, `SELECT credential_revision FROM core_aws_plans WHERE owner_id=$1 AND plan_id=$2`, r.ownerID, v.PlanID).Scan(&credentialRevision); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentaws.Change{}, agentaws.ErrNotFound
		}
		return agentaws.Change{}, err
	}
	_, err := r.store.db.ExecContext(c, `INSERT INTO core_aws_changes(owner_id,change_id,plan_id,credential_id,credential_revision,task_id,confirmation_id,operation,status,stage,change_set_id,provider_request_digest,provider_token,revision,error_code,error_summary,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`, r.ownerID, v.ID, v.PlanID, v.CredentialID, credentialRevision, v.TaskID, v.ConfirmationID, v.Operation, v.Status, v.Stage, v.ChangeSetID, v.ProviderRequestDigest, v.ProviderToken, v.Revision, v.ErrorCode, v.ErrorSummary, v.CreatedAt, v.UpdatedAt)
	return v, mapAWSError(err)
}
func (r *PostgresAWSRepository) GetChange(c context.Context, id string) (agentaws.Change, error) {
	var v agentaws.Change
	err := r.store.db.QueryRowContext(c, `SELECT change_id::text,plan_id::text,credential_id::text,task_id::text,confirmation_id::text,operation,status,stage,change_set_id,provider_request_digest,provider_token,revision,error_code,error_summary,created_at,updated_at FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2`, r.ownerID, id).Scan(&v.ID, &v.PlanID, &v.CredentialID, &v.TaskID, &v.ConfirmationID, &v.Operation, &v.Status, &v.Stage, &v.ChangeSetID, &v.ProviderRequestDigest, &v.ProviderToken, &v.Revision, &v.ErrorCode, &v.ErrorSummary, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return v, agentaws.ErrNotFound
	}
	return v, err
}
func (r *PostgresAWSRepository) GetChangeByConfirmation(c context.Context, id string) (agentaws.Change, error) {
	var v agentaws.Change
	err := r.store.db.QueryRowContext(c, `SELECT change_id::text,plan_id::text,credential_id::text,task_id::text,confirmation_id::text,operation,status,stage,change_set_id,provider_request_digest,provider_token,revision,error_code,error_summary,created_at,updated_at FROM core_aws_changes WHERE owner_id=$1 AND confirmation_id=$2`, r.ownerID, id).Scan(&v.ID, &v.PlanID, &v.CredentialID, &v.TaskID, &v.ConfirmationID, &v.Operation, &v.Status, &v.Stage, &v.ChangeSetID, &v.ProviderRequestDigest, &v.ProviderToken, &v.Revision, &v.ErrorCode, &v.ErrorSummary, &v.CreatedAt, &v.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return v, agentaws.ErrNotFound
	}
	return v, err
}
func (r *PostgresAWSRepository) ListChanges(c context.Context, n int, p, k string) (agentaws.ChangePage, error) {
	p = strings.TrimSpace(p)
	k = strings.TrimSpace(k)
	if n < 0 || n > 100 || (p != "" && !validAWSUUID(p)) || (k != "" && !validAWSUUID(k)) {
		return agentaws.ChangePage{}, agentaws.ErrInvalid
	}
	rows, err := r.store.db.QueryContext(c, `SELECT change_id::text,plan_id::text,credential_id::text,task_id::text,confirmation_id::text,operation,status,stage,change_set_id,provider_request_digest,provider_token,revision,error_code,error_summary,created_at,updated_at FROM core_aws_changes WHERE owner_id=$1 AND ($2='' OR plan_id=NULLIF($2,'')::uuid) AND change_id>COALESCE(NULLIF($3,'')::uuid,'00000000-0000-0000-0000-000000000000'::uuid) ORDER BY change_id LIMIT $4`, r.ownerID, p, k, func() int {
		if n == 0 {
			return 101
		}
		return n + 1
	}())
	if err != nil {
		return agentaws.ChangePage{}, err
	}
	defer rows.Close()
	out := agentaws.ChangePage{}
	for rows.Next() {
		var v agentaws.Change
		if err := rows.Scan(&v.ID, &v.PlanID, &v.CredentialID, &v.TaskID, &v.ConfirmationID, &v.Operation, &v.Status, &v.Stage, &v.ChangeSetID, &v.ProviderRequestDigest, &v.ProviderToken, &v.Revision, &v.ErrorCode, &v.ErrorSummary, &v.CreatedAt, &v.UpdatedAt); err != nil {
			return out, err
		}
		out.Items = append(out.Items, v)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	if n > 0 && len(out.Items) > n {
		out.NextPageToken = out.Items[n-1].ID
		out.Items = out.Items[:n]
	}
	return out, nil
}
func (r *PostgresAWSRepository) UpdateChange(c context.Context, v agentaws.Change, rev int64) (agentaws.Change, error) {
	if r == nil || r.store == nil || r.store.db == nil || v.Revision != rev+1 {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	res, err := r.store.db.ExecContext(c, `UPDATE core_aws_changes SET status=$1,stage=$2,change_set_id=$3,provider_request_digest=$4,provider_token=$5,revision=$6,error_code=$7,error_summary=$8,updated_at=$9 WHERE owner_id=$10 AND change_id=$11 AND revision=$12 AND NOT(status IN ('succeeded','failed','canceled') AND ($1<>status OR $2<>stage))`, v.Status, v.Stage, v.ChangeSetID, v.ProviderRequestDigest, v.ProviderToken, v.Revision, v.ErrorCode, v.ErrorSummary, v.UpdatedAt, r.ownerID, v.ID, rev)
	if err != nil {
		return agentaws.Change{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	return v, nil
}

// ReplayPlan returns the immutable idempotency snapshot for a plan request.
// A digest mismatch is never treated as a cache hit.
func (r *PostgresAWSRepository) ReplayPlan(c context.Context, operation, key, digest string) (agentaws.PlanView, bool, error) {
	var raw []byte
	var prior string
	err := r.store.db.QueryRowContext(c, `SELECT request_hash,response_json FROM core_aws_replays WHERE owner_id=$1 AND operation=$2 AND idempotency_key=$3`, r.ownerID, operation, key).Scan(&prior, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return agentaws.PlanView{}, false, nil
	}
	if err != nil {
		return agentaws.PlanView{}, false, err
	}
	if prior != digest {
		return agentaws.PlanView{}, true, agentaws.ErrIdempotencyConflict
	}
	var v agentaws.PlanView
	if json.Unmarshal(raw, &v) != nil {
		return agentaws.PlanView{}, true, agentaws.ErrConflict
	}
	return v, true, nil
}

// RecordPlanReplay atomically records a replay response after the caller has
// committed the immutable plan row.
func (r *PostgresAWSRepository) RecordPlanReplay(c context.Context, operation, key, digest string, v agentaws.PlanView) error {
	raw, _ := json.Marshal(v)
	_, err := r.store.db.ExecContext(c, `INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json) VALUES($1,$2,$3,$4,$5) ON CONFLICT(owner_id,operation,idempotency_key) DO UPDATE SET request_hash=EXCLUDED.request_hash,response_json=EXCLUDED.response_json`, r.ownerID, operation, key, digest, raw)
	return err
}

func (r *PostgresAWSRepository) ConsumeChange(ctx context.Context, cmd agentaws.ConsumeChangeCommand) (agentaws.Reservation, error) {
	if !validAWSUUID(cmd.ChangeID) || !validAWSUUID(cmd.ConfirmationID) || !validAWSUUID(cmd.TaskID) || !validAWSUUID(cmd.IdempotencyKey) || cmd.Attempt == 0 || cmd.LeaseEpoch == 0 || cmd.ExpectedChangeRevision < 1 || cmd.ExpectedTaskRevision < 1 || cmd.ExpectedConfirmationRevision < 1 {
		return agentaws.Reservation{}, agentaws.ErrInvalid
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.Reservation{}, err
	}
	defer tx.Rollback()
	digest := stringDigest(cmd)
	var priorHash string
	var priorRaw []byte
	if e := tx.QueryRowContext(ctx, `SELECT request_hash,response_json FROM core_aws_replays WHERE owner_id=$1 AND operation='change-consume' AND idempotency_key=$2 FOR UPDATE`, r.ownerID, cmd.IdempotencyKey).Scan(&priorHash, &priorRaw); e == nil {
		if priorHash != digest {
			return agentaws.Reservation{}, agentaws.ErrIdempotencyConflict
		}
		var replay agentaws.Reservation
		if json.Unmarshal(priorRaw, &replay) != nil {
			return agentaws.Reservation{}, agentaws.ErrConflict
		}
		return replay, tx.Commit()
	} else if !errors.Is(e, sql.ErrNoRows) {
		return agentaws.Reservation{}, e
	}
	var status string
	var taskRev int64
	var attempt int
	var epoch int64
	err = tx.QueryRowContext(ctx, `SELECT status,revision,attempt,lease_epoch FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, r.ownerID, cmd.TaskID).Scan(&status, &taskRev, &attempt, &epoch)
	if err != nil || status != "running" || taskRev != cmd.ExpectedTaskRevision || uint32(attempt) != cmd.Attempt || uint64(epoch) != cmd.LeaseEpoch {
		return agentaws.Reservation{}, agentaws.ErrRevisionConflict
	}
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `UPDATE core_aws_changes SET status='running',stage='change_set_creating',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND change_id=$3 AND task_id=$4 AND confirmation_id=$5 AND revision=$6 AND status='waiting_user'`, now, r.ownerID, cmd.ChangeID, cmd.TaskID, cmd.ConfirmationID, cmd.ExpectedChangeRevision)
	if err != nil {
		return agentaws.Reservation{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.Reservation{}, agentaws.ErrRevisionConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='consumed',revision=revision+1,reservation_json=jsonb_build_object('task_id',$1,'attempt',$2,'lease_epoch',$3,'task_revision',$4,'active',true),updated_at=$5 WHERE owner_id=$6 AND confirmation_id=$7 AND revision=$8 AND state='confirmed' AND expires_at>$5`, cmd.TaskID, cmd.Attempt, cmd.LeaseEpoch, cmd.ExpectedTaskRevision, now, r.ownerID, cmd.ConfirmationID, cmd.ExpectedConfirmationRevision)
	if err != nil {
		return agentaws.Reservation{}, err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return agentaws.Reservation{}, agentaws.ErrRevisionConflict
	}
	var seq int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO core_aws_event_counters(owner_id,change_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,change_id) DO UPDATE SET next_sequence=core_aws_event_counters.next_sequence+1 RETURNING next_sequence-1`, r.ownerID, cmd.ChangeID).Scan(&seq); err != nil {
		return agentaws.Reservation{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,$3,$4,$5,'change_consumed',$6,$7)`, r.ownerID, cmd.ChangeID, seq, uuid.NewString(), cmd.TaskID, cmd.ExpectedChangeRevision+1, now); err != nil {
		return agentaws.Reservation{}, err
	}
	replay := agentaws.Reservation{ConfirmationID: cmd.ConfirmationID, TaskID: cmd.TaskID, Attempt: cmd.Attempt, LeaseEpoch: cmd.LeaseEpoch, TaskRevision: cmd.ExpectedTaskRevision, Active: true}
	replayRaw, _ := json.Marshal(replay)
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json) VALUES($1,'change-consume',$2,$3,$4)`, r.ownerID, cmd.IdempotencyKey, digest, replayRaw); err != nil {
		return agentaws.Reservation{}, err
	}
	if err = tx.Commit(); err != nil {
		return agentaws.Reservation{}, err
	}
	return replay, nil
}
func (r *PostgresAWSRepository) ExecutionFence(ctx context.Context, confirmationID string) (agentaws.ExecutionFence, error) {
	c, err := r.GetChangeByConfirmation(ctx, confirmationID)
	if err != nil {
		return agentaws.ExecutionFence{}, err
	}
	var t agentaws.Task
	err = r.store.db.QueryRowContext(ctx, `SELECT task_id::text,status,revision,attempt,lease_epoch,failure_code,failure_summary FROM agent_tasks WHERE owner_id=$1 AND task_id=$2`, r.ownerID, c.TaskID).Scan(&t.ID, &t.Status, &t.Revision, &t.Attempt, &t.LeaseEpoch, &t.FailureCode, &t.FailureSummary)
	if err != nil {
		return agentaws.ExecutionFence{}, err
	}
	conf, err := NewDatabaseConfirmationStore(r.store.db).Get(ctx, confirmationID)
	if err != nil || conf.OwnerID != r.ownerID {
		return agentaws.ExecutionFence{}, agentaws.ErrNotFound
	}
	var reservationRaw []byte
	if err = r.store.db.QueryRowContext(ctx, `SELECT reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2`, r.ownerID, confirmationID).Scan(&reservationRaw); err != nil {
		return agentaws.ExecutionFence{}, err
	}
	var reserved struct {
		TaskID       string `json:"task_id"`
		Attempt      uint32 `json:"attempt"`
		LeaseEpoch   uint64 `json:"lease_epoch"`
		TaskRevision int64  `json:"task_revision"`
		Active       bool   `json:"active"`
	}
	if len(reservationRaw) > 0 && json.Unmarshal(reservationRaw, &reserved) != nil {
		return agentaws.ExecutionFence{}, agentaws.ErrConflict
	}
	reservation := agentaws.Reservation{
		ConfirmationID: confirmationID,
		TaskID:         reserved.TaskID,
		Attempt:        reserved.Attempt,
		LeaseEpoch:     reserved.LeaseEpoch,
		TaskRevision:   reserved.TaskRevision,
		Active:         reserved.Active,
	}
	return agentaws.ExecutionFence{Change: c, Task: t, Confirmation: conf, Reservation: reservation}, nil
}
func (r *PostgresAWSRepository) CompleteChange(ctx context.Context, cmd agentaws.CompleteChangeCommand) (agentaws.Change, error) {
	stage := agentaws.StageSucceeded
	if cmd.Status == agentaws.ChangeFailed {
		stage = agentaws.StageFailed
	}
	if cmd.Status == agentaws.ChangeCanceled {
		stage = agentaws.StageCanceled
	}
	if cmd.Status != agentaws.ChangeSucceeded && cmd.Status != agentaws.ChangeFailed && cmd.Status != agentaws.ChangeCanceled ||
		!validAWSUUID(cmd.ChangeID) ||
		!validAWSUUID(cmd.ConfirmationID) ||
		!validAWSUUID(cmd.TaskID) ||
		cmd.Attempt == 0 ||
		cmd.LeaseEpoch == 0 ||
		cmd.ExpectedTaskRevision < 1 ||
		cmd.ExpectedChangeRevision < 1 ||
		cmd.ExpectedConfirmationRevision < 1 {
		return agentaws.Change{}, agentaws.ErrInvalid
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.Change{}, err
	}
	defer tx.Rollback()
	var (
		changeTaskID         string
		changeConfirmationID string
		changeStatus         agentaws.ChangeStatus
		changeStage          agentaws.ChangeStage
		changeRevision       int64
	)
	if err = tx.QueryRowContext(ctx, `SELECT task_id::text,confirmation_id::text,status,stage,revision FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2 FOR UPDATE`, r.ownerID, cmd.ChangeID).Scan(
		&changeTaskID,
		&changeConfirmationID,
		&changeStatus,
		&changeStage,
		&changeRevision,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentaws.Change{}, agentaws.ErrNotFound
		}
		return agentaws.Change{}, err
	}
	if changeTaskID != cmd.TaskID ||
		changeConfirmationID != cmd.ConfirmationID ||
		changeRevision != cmd.ExpectedChangeRevision ||
		(changeStatus != agentaws.ChangeRunning && (changeStatus != cmd.Status || changeStage != stage)) {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	var (
		currentTaskStatus   string
		currentTaskRevision int64
		currentTaskAttempt  int
		currentTaskEpoch    int64
		currentTaskExpiry   *time.Time
	)
	now := time.Now().UTC()
	if err = tx.QueryRowContext(ctx, `SELECT status,revision,attempt,lease_epoch,lease_expires_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, r.ownerID, cmd.TaskID).Scan(
		&currentTaskStatus,
		&currentTaskRevision,
		&currentTaskAttempt,
		&currentTaskEpoch,
		&currentTaskExpiry,
	); err != nil {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	if currentTaskStatus != "running" ||
		currentTaskRevision < cmd.ExpectedTaskRevision ||
		currentTaskAttempt != int(cmd.Attempt) ||
		currentTaskEpoch != int64(cmd.LeaseEpoch) ||
		currentTaskExpiry == nil ||
		!currentTaskExpiry.After(now) {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	var (
		confirmationState    coreconfirmation.State
		confirmationRevision int64
		confirmationTaskID   string
		reservationRaw       []byte
	)
	if err = tx.QueryRowContext(ctx, `SELECT state,revision,task_id::text,reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2 FOR UPDATE`, r.ownerID, cmd.ConfirmationID).Scan(
		&confirmationState,
		&confirmationRevision,
		&confirmationTaskID,
		&reservationRaw,
	); err != nil {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	var reservation struct {
		TaskID       string `json:"task_id"`
		Attempt      uint32 `json:"attempt"`
		LeaseEpoch   uint64 `json:"lease_epoch"`
		TaskRevision int64  `json:"task_revision"`
		Active       bool   `json:"active"`
	}
	if confirmationState != coreconfirmation.StateConsumed ||
		confirmationRevision != cmd.ExpectedConfirmationRevision ||
		confirmationTaskID != cmd.TaskID ||
		json.Unmarshal(reservationRaw, &reservation) != nil ||
		!reservation.Active ||
		reservation.TaskID != cmd.TaskID ||
		reservation.Attempt != cmd.Attempt ||
		reservation.LeaseEpoch > cmd.LeaseEpoch ||
		reservation.TaskRevision > currentTaskRevision ||
		(reservation.LeaseEpoch < cmd.LeaseEpoch && reservation.TaskRevision >= currentTaskRevision) {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	res, err := tx.ExecContext(ctx, `UPDATE core_aws_changes SET status=$1,stage=$2,error_code=$3,error_summary=$4,revision=revision+1,updated_at=$5 WHERE owner_id=$6 AND change_id=$7 AND task_id=$8 AND confirmation_id=$9 AND revision=$10 AND (status='running' OR (status=$1 AND stage=$2))`, cmd.Status, stage, cmd.ErrorCode, cmd.ErrorSummary, now, r.ownerID, cmd.ChangeID, cmd.TaskID, cmd.ConfirmationID, changeRevision)
	if err != nil {
		return agentaws.Change{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	taskStatus := "succeeded"
	if cmd.Status == agentaws.ChangeFailed {
		taskStatus = "failed"
	}
	if cmd.Status == agentaws.ChangeCanceled {
		taskStatus = "canceled"
	}
	var seq int64
	if err = tx.QueryRowContext(ctx, `UPDATE agent_tasks SET status=$1,revision=revision+1,failure_code=$2,failure_summary=$3,progress_sequence=progress_sequence+1,updated_at=$4,lease_expires_at=NULL,lease_holder='' WHERE owner_id=$5 AND task_id=$6 AND status='running' AND revision=$7 AND attempt=$8 AND lease_epoch=$9 AND lease_expires_at>$4 RETURNING progress_sequence`, taskStatus, cmd.ErrorCode, cmd.ErrorSummary, now, r.ownerID, cmd.TaskID, currentTaskRevision, cmd.Attempt, cmd.LeaseEpoch).Scan(&seq); err != nil {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) VALUES($1,$2,$3,'aws_change_completed',$4,$5,$6)`, r.ownerID, cmd.TaskID, seq, taskStatus, []byte(`{}`), now); err != nil {
		return agentaws.Change{}, err
	}
	res, err = tx.ExecContext(ctx, `UPDATE agent_confirmations SET reservation_json=jsonb_set(reservation_json,'{active}','false'),revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND confirmation_id=$3 AND revision=$4 AND state='consumed'`, now, r.ownerID, cmd.ConfirmationID, confirmationRevision)
	if err != nil {
		return agentaws.Change{}, err
	}
	if n, _ = res.RowsAffected(); n != 1 {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	if _, err = tx.ExecContext(ctx, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true AND running_count>0`, now); err != nil {
		return agentaws.Change{}, err
	}
	var changeSequence int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO core_aws_event_counters(owner_id,change_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,change_id) DO UPDATE SET next_sequence=core_aws_event_counters.next_sequence+1 RETURNING next_sequence-1`, r.ownerID, cmd.ChangeID).Scan(&changeSequence); err != nil {
		return agentaws.Change{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,$3,$4,$5,'change_completed',$6,$7)`, r.ownerID, cmd.ChangeID, changeSequence, uuid.NewString(), cmd.TaskID, changeRevision+1, now); err != nil {
		return agentaws.Change{}, err
	}
	if err = tx.Commit(); err != nil {
		return agentaws.Change{}, err
	}
	return r.GetChange(ctx, cmd.ChangeID)
}
func (r *PostgresAWSRepository) ReconcileChange(ctx context.Context, cmd agentaws.ReconcileChangeCommand) (agentaws.Change, error) {
	f, err := r.ExecutionFence(ctx, cmd.ConfirmationID)
	if err != nil {
		return agentaws.Change{}, err
	}
	if f.Change.ID != cmd.ChangeID ||
		f.Change.Revision != cmd.ExpectedChangeRevision ||
		f.Task.ID != cmd.TaskID ||
		f.Task.Revision < cmd.ExpectedTaskRevision ||
		f.Task.Attempt != cmd.Attempt ||
		f.Task.LeaseEpoch != cmd.LeaseEpoch ||
		f.Confirmation.Revision != cmd.ExpectedConfirmationRevision ||
		!f.Reservation.Active ||
		f.Reservation.Attempt != cmd.Attempt ||
		f.Reservation.LeaseEpoch > cmd.LeaseEpoch ||
		f.Change.ChangeSetID != cmd.ProviderChangeSetID {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	status := agentaws.ChangeFailed
	if cmd.Success {
		status = agentaws.ChangeSucceeded
	}
	return r.CompleteChange(ctx, agentaws.CompleteChangeCommand{
		ChangeID:                     cmd.ChangeID,
		ConfirmationID:               cmd.ConfirmationID,
		TaskID:                       cmd.TaskID,
		Attempt:                      cmd.Attempt,
		LeaseEpoch:                   cmd.LeaseEpoch,
		ExpectedTaskRevision:         cmd.ExpectedTaskRevision,
		ExpectedChangeRevision:       cmd.ExpectedChangeRevision,
		ExpectedConfirmationRevision: cmd.ExpectedConfirmationRevision,
		Status:                       status,
		ErrorCode:                    cmd.ErrorCode,
		ErrorSummary:                 cmd.ErrorSummary,
	})
}
func (r *PostgresAWSRepository) ClaimProviderMutation(ctx context.Context, cmd agentaws.ProviderMutationCommand) (agentaws.ExecutionFence, error) {
	if err := validateAWSProviderMutationCommand(cmd); err != nil {
		return agentaws.ExecutionFence{}, err
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.ExecutionFence{}, err
	}
	defer tx.Rollback()
	if err = r.lockAWSProviderMutationFence(ctx, tx, cmd, awsProviderFenceClaim); err != nil {
		return agentaws.ExecutionFence{}, err
	}
	dispatchDigest := awsProviderMutationDigest(cmd)
	var priorDigest string
	if err = tx.QueryRowContext(ctx, `SELECT request_hash FROM core_aws_replays WHERE owner_id=$1 AND operation='provider-mutation' AND idempotency_key=$2 FOR UPDATE`, r.ownerID, cmd.OperationKey).Scan(&priorDigest); err == nil {
		if priorDigest != dispatchDigest {
			return agentaws.ExecutionFence{}, agentaws.ErrIdempotencyConflict
		}
		// A persisted dispatch is never replayed. Its successor must perform
		// typed provider readback from the reconciling stage.
		return agentaws.ExecutionFence{}, agentaws.ErrRevisionConflict
	} else if !errors.Is(err, sql.ErrNoRows) {
		return agentaws.ExecutionFence{}, err
	}
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `UPDATE core_aws_changes SET stage='reconciling',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND change_id=$3 AND task_id=$4 AND confirmation_id=$5 AND revision=$6 AND status='running'`, now, r.ownerID, cmd.ChangeID, cmd.TaskID, cmd.ConfirmationID, cmd.ExpectedChangeRevision)
	if err != nil {
		return agentaws.ExecutionFence{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return agentaws.ExecutionFence{}, agentaws.ErrRevisionConflict
	}
	dispatch := awsProviderMutationDispatch{
		ChangeID:              cmd.ChangeID,
		ConfirmationID:        cmd.ConfirmationID,
		TaskID:                cmd.TaskID,
		Kind:                  cmd.Kind,
		ProviderChangeSetID:   cmd.ProviderChangeSetID,
		Status:                "dispatched",
		ClaimedChangeRevision: cmd.ExpectedChangeRevision + 1,
	}
	dispatchRaw, _ := json.Marshal(dispatch)
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_replays(owner_id,operation,idempotency_key,request_hash,response_json,created_at) VALUES($1,'provider-mutation',$2,$3,$4,$5)`, r.ownerID, cmd.OperationKey, dispatchDigest, dispatchRaw, now); err != nil {
		return agentaws.ExecutionFence{}, err
	}
	var sequence int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO core_aws_event_counters(owner_id,change_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,change_id) DO UPDATE SET next_sequence=core_aws_event_counters.next_sequence+1 RETURNING next_sequence-1`, r.ownerID, cmd.ChangeID).Scan(&sequence); err != nil {
		return agentaws.ExecutionFence{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,$3,$4,$5,'provider_mutation_dispatched',$6,$7)`, r.ownerID, cmd.ChangeID, sequence, uuid.NewString(), cmd.TaskID, cmd.ExpectedChangeRevision+1, now); err != nil {
		return agentaws.ExecutionFence{}, err
	}
	if err = tx.Commit(); err != nil {
		return agentaws.ExecutionFence{}, err
	}
	f, err := r.ExecutionFence(ctx, cmd.ConfirmationID)
	if err != nil {
		return agentaws.ExecutionFence{}, err
	}
	if f.Change.ID != cmd.ChangeID ||
		f.Change.Revision != cmd.ExpectedChangeRevision+1 ||
		f.Change.Stage != agentaws.StageReconciling ||
		f.Task.ID != cmd.TaskID ||
		f.Task.Revision < cmd.ExpectedTaskRevision ||
		f.Task.Attempt != cmd.Attempt ||
		f.Task.LeaseEpoch != cmd.LeaseEpoch ||
		f.Confirmation.State != coreconfirmation.StateConsumed ||
		!f.Reservation.Active ||
		f.Reservation.TaskID != cmd.TaskID ||
		f.Reservation.Attempt != cmd.Attempt ||
		f.Reservation.LeaseEpoch != cmd.LeaseEpoch ||
		f.Reservation.TaskRevision > f.Task.Revision {
		return agentaws.ExecutionFence{}, agentaws.ErrRevisionConflict
	}
	return f, nil
}
func (r *PostgresAWSRepository) CommitProviderMutation(ctx context.Context, result agentaws.ProviderMutationResult) (agentaws.Change, error) {
	cmd := result.Command
	if err := validateAWSProviderMutationCommand(cmd); err != nil {
		return agentaws.Change{}, err
	}
	if result.Success && !result.ResponseUncertain && cmd.Kind == agentaws.ProviderMutationCreate && strings.TrimSpace(result.ProviderChangeSetID) == "" {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.Change{}, err
	}
	defer tx.Rollback()
	if err = r.lockAWSProviderMutationFence(ctx, tx, cmd, awsProviderFenceCommit); err != nil {
		return agentaws.Change{}, err
	}
	dispatchDigest := awsProviderMutationDigest(cmd)
	var priorDigest string
	var dispatchRaw []byte
	if err = tx.QueryRowContext(ctx, `SELECT request_hash,response_json FROM core_aws_replays WHERE owner_id=$1 AND operation='provider-mutation' AND idempotency_key=$2 FOR UPDATE`, r.ownerID, cmd.OperationKey).Scan(&priorDigest, &dispatchRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentaws.Change{}, agentaws.ErrRevisionConflict
		}
		return agentaws.Change{}, err
	}
	var dispatch awsProviderMutationDispatch
	if priorDigest != dispatchDigest ||
		json.Unmarshal(dispatchRaw, &dispatch) != nil ||
		dispatch.ChangeID != cmd.ChangeID ||
		dispatch.ConfirmationID != cmd.ConfirmationID ||
		dispatch.TaskID != cmd.TaskID ||
		dispatch.Kind != cmd.Kind ||
		dispatch.ProviderChangeSetID != cmd.ProviderChangeSetID ||
		dispatch.Status != "dispatched" ||
		dispatch.ClaimedChangeRevision != cmd.ExpectedChangeRevision {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	now := time.Now().UTC()
	eventKind := "provider_mutation_committed"
	dispatch.Status = "committed"
	var res sql.Result
	if result.ResponseUncertain {
		eventKind = "provider_mutation_uncertain"
		dispatch.Status = "uncertain"
		res, err = tx.ExecContext(ctx, `UPDATE core_aws_changes SET stage='reconciling',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND change_id=$3 AND task_id=$4 AND confirmation_id=$5 AND revision=$6 AND status='running'`, now, r.ownerID, cmd.ChangeID, cmd.TaskID, cmd.ConfirmationID, cmd.ExpectedChangeRevision)
	} else if !result.Success {
		// The service owns terminalization through CompleteChange. Preserve
		// the running task and consumed reservation here so that task,
		// confirmation, concurrency and terminal event still commit in one
		// transaction after this provider evidence is recorded.
		eventKind = "provider_mutation_failed"
		dispatch.Status = "failed"
		res, err = tx.ExecContext(ctx, `UPDATE core_aws_changes SET status='failed',stage='failed',error_code=$1,error_summary=$2,revision=revision+1,updated_at=$3 WHERE owner_id=$4 AND change_id=$5 AND task_id=$6 AND confirmation_id=$7 AND revision=$8 AND status='running'`, result.ErrorCode, result.ErrorSummary, now, r.ownerID, cmd.ChangeID, cmd.TaskID, cmd.ConfirmationID, cmd.ExpectedChangeRevision)
	} else {
		stage := agentaws.StageExecuting
		if cmd.Kind == agentaws.ProviderMutationCreate {
			stage = agentaws.StageChangeSetReady
		}
		res, err = tx.ExecContext(ctx, `UPDATE core_aws_changes SET stage=$1,change_set_id=COALESCE(NULLIF($2,''),change_set_id),revision=revision+1,updated_at=$3 WHERE owner_id=$4 AND change_id=$5 AND task_id=$6 AND confirmation_id=$7 AND revision=$8 AND status='running'`, stage, result.ProviderChangeSetID, now, r.ownerID, cmd.ChangeID, cmd.TaskID, cmd.ConfirmationID, cmd.ExpectedChangeRevision)
	}
	if err != nil {
		return agentaws.Change{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	dispatchRaw, _ = json.Marshal(dispatch)
	if res, err = tx.ExecContext(ctx, `UPDATE core_aws_replays SET response_json=$1,error_code=$2 WHERE owner_id=$3 AND operation='provider-mutation' AND idempotency_key=$4 AND request_hash=$5`, dispatchRaw, result.ErrorCode, r.ownerID, cmd.OperationKey, dispatchDigest); err != nil {
		return agentaws.Change{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	var sequence int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO core_aws_event_counters(owner_id,change_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,change_id) DO UPDATE SET next_sequence=core_aws_event_counters.next_sequence+1 RETURNING next_sequence-1`, r.ownerID, cmd.ChangeID).Scan(&sequence); err != nil {
		return agentaws.Change{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, r.ownerID, cmd.ChangeID, sequence, uuid.NewString(), cmd.TaskID, eventKind, cmd.ExpectedChangeRevision+1, now); err != nil {
		return agentaws.Change{}, err
	}
	if err = tx.Commit(); err != nil {
		return agentaws.Change{}, err
	}
	return r.GetChange(ctx, cmd.ChangeID)
}

func validateAWSProviderMutationCommand(cmd agentaws.ProviderMutationCommand) error {
	if !validAWSUUID(cmd.ChangeID) ||
		!validAWSUUID(cmd.ConfirmationID) ||
		!validAWSUUID(cmd.TaskID) ||
		!validAWSUUID(cmd.OperationKey) ||
		cmd.Attempt == 0 ||
		cmd.LeaseEpoch == 0 ||
		cmd.ExpectedChangeRevision < 1 ||
		cmd.ExpectedTaskRevision < 1 ||
		cmd.ExpectedConfirmationRevision < 1 {
		return agentaws.ErrInvalid
	}
	switch cmd.Kind {
	case agentaws.ProviderMutationCreate, agentaws.ProviderMutationExecute, agentaws.ProviderMutationDelete:
		return nil
	default:
		return agentaws.ErrInvalid
	}
}

type awsProviderMutationFenceMode uint8

const (
	awsProviderFenceClaim awsProviderMutationFenceMode = iota + 1
	awsProviderFenceCommit
)

type awsProviderMutationDispatch struct {
	ChangeID              string                        `json:"change_id"`
	ConfirmationID        string                        `json:"confirmation_id"`
	TaskID                string                        `json:"task_id"`
	Kind                  agentaws.ProviderMutationKind `json:"kind"`
	ProviderChangeSetID   string                        `json:"provider_change_set_id,omitempty"`
	Status                string                        `json:"status"`
	ClaimedChangeRevision int64                         `json:"claimed_change_revision"`
}

func awsProviderMutationDigest(cmd agentaws.ProviderMutationCommand) string {
	return stringDigest(struct {
		ChangeID, ConfirmationID, TaskID, OperationKey, ProviderChangeSetID string
		Kind                                                                agentaws.ProviderMutationKind
	}{
		ChangeID:            cmd.ChangeID,
		ConfirmationID:      cmd.ConfirmationID,
		TaskID:              cmd.TaskID,
		OperationKey:        cmd.OperationKey,
		ProviderChangeSetID: cmd.ProviderChangeSetID,
		Kind:                cmd.Kind,
	})
}

func (r *PostgresAWSRepository) lockAWSProviderMutationFence(ctx context.Context, tx *sql.Tx, cmd agentaws.ProviderMutationCommand, mode awsProviderMutationFenceMode) error {
	var (
		changeTaskID         string
		changeConfirmationID string
		operation            agentaws.Operation
		status               agentaws.ChangeStatus
		stage                agentaws.ChangeStage
		changeSetID          string
		changeRevision       int64
	)
	err := tx.QueryRowContext(ctx, `SELECT task_id::text,confirmation_id::text,operation,status,stage,change_set_id,revision FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2 FOR UPDATE`, r.ownerID, cmd.ChangeID).Scan(
		&changeTaskID,
		&changeConfirmationID,
		&operation,
		&status,
		&stage,
		&changeSetID,
		&changeRevision,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return agentaws.ErrNotFound
	}
	if err != nil {
		return err
	}
	if changeTaskID != cmd.TaskID ||
		changeConfirmationID != cmd.ConfirmationID ||
		changeRevision != cmd.ExpectedChangeRevision ||
		status != agentaws.ChangeRunning {
		return agentaws.ErrRevisionConflict
	}
	switch cmd.Kind {
	case agentaws.ProviderMutationCreate:
		if operation == agentaws.OperationDelete {
			return agentaws.ErrRevisionConflict
		}
	case agentaws.ProviderMutationExecute:
		if changeSetID == "" || changeSetID != cmd.ProviderChangeSetID {
			return agentaws.ErrRevisionConflict
		}
	case agentaws.ProviderMutationDelete:
		if operation != agentaws.OperationDelete {
			return agentaws.ErrRevisionConflict
		}
	default:
		return agentaws.ErrInvalid
	}
	if mode == awsProviderFenceCommit {
		if stage != agentaws.StageReconciling {
			return agentaws.ErrRevisionConflict
		}
	} else {
		switch cmd.Kind {
		case agentaws.ProviderMutationCreate:
			if stage != agentaws.StageChangeSetCreating {
				return agentaws.ErrRevisionConflict
			}
		case agentaws.ProviderMutationExecute:
			if stage != agentaws.StageChangeSetReady {
				return agentaws.ErrRevisionConflict
			}
		case agentaws.ProviderMutationDelete:
			if stage != agentaws.StageChangeSetCreating {
				return agentaws.ErrRevisionConflict
			}
		}
	}

	var (
		taskStatus   string
		taskRevision int64
		taskAttempt  int
		taskEpoch    int64
		leaseExpiry  *time.Time
	)
	if err = tx.QueryRowContext(ctx, `SELECT status,revision,attempt,lease_epoch,lease_expires_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, r.ownerID, cmd.TaskID).Scan(
		&taskStatus,
		&taskRevision,
		&taskAttempt,
		&taskEpoch,
		&leaseExpiry,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentaws.ErrRevisionConflict
		}
		return err
	}
	if taskStatus != "running" ||
		(mode == awsProviderFenceClaim && taskRevision != cmd.ExpectedTaskRevision) ||
		(mode == awsProviderFenceCommit && taskRevision < cmd.ExpectedTaskRevision) ||
		taskAttempt != int(cmd.Attempt) ||
		taskEpoch != int64(cmd.LeaseEpoch) ||
		leaseExpiry == nil ||
		!leaseExpiry.After(time.Now().UTC()) {
		return agentaws.ErrRevisionConflict
	}

	var (
		confirmationState    coreconfirmation.State
		confirmationRevision int64
		confirmationTaskID   string
		reservationRaw       []byte
	)
	if err = tx.QueryRowContext(ctx, `SELECT state,revision,task_id::text,reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2 FOR UPDATE`, r.ownerID, cmd.ConfirmationID).Scan(
		&confirmationState,
		&confirmationRevision,
		&confirmationTaskID,
		&reservationRaw,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentaws.ErrRevisionConflict
		}
		return err
	}
	var reservation struct {
		TaskID       string `json:"task_id"`
		Attempt      uint32 `json:"attempt"`
		LeaseEpoch   uint64 `json:"lease_epoch"`
		TaskRevision int64  `json:"task_revision"`
		Active       bool   `json:"active"`
	}
	if confirmationState != coreconfirmation.StateConsumed ||
		confirmationRevision != cmd.ExpectedConfirmationRevision ||
		confirmationTaskID != cmd.TaskID ||
		json.Unmarshal(reservationRaw, &reservation) != nil ||
		!reservation.Active ||
		reservation.TaskID != cmd.TaskID ||
		reservation.Attempt != cmd.Attempt ||
		reservation.LeaseEpoch > cmd.LeaseEpoch ||
		reservation.TaskRevision > taskRevision {
		return agentaws.ErrRevisionConflict
	}
	if reservation.LeaseEpoch < cmd.LeaseEpoch {
		// A successor may promote an older consumed reservation only while
		// claiming an original (non-reconciling) stage. The claim transaction
		// persists dispatch evidence before it returns, so absence of that
		// evidence proves no provider mutation was admitted by the predecessor.
		if mode != awsProviderFenceClaim || reservation.TaskRevision >= taskRevision {
			return agentaws.ErrRevisionConflict
		}
		reservation.LeaseEpoch = cmd.LeaseEpoch
		reservation.TaskRevision = taskRevision
		reservationRaw, _ = json.Marshal(reservation)
		res, updateErr := tx.ExecContext(ctx, `UPDATE agent_confirmations SET reservation_json=$1,revision=revision+1,updated_at=clock_timestamp() WHERE owner_id=$2 AND confirmation_id=$3 AND state='consumed' AND revision=$4`, reservationRaw, r.ownerID, cmd.ConfirmationID, confirmationRevision)
		if updateErr != nil {
			return updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return agentaws.ErrRevisionConflict
		}
	} else if reservation.LeaseEpoch != cmd.LeaseEpoch {
		return agentaws.ErrRevisionConflict
	}
	return nil
}
func (r *PostgresAWSRepository) PersistChangeSetEvidence(ctx context.Context, cmd agentaws.ChangeSetEvidenceCommand) (agentaws.Change, error) {
	if !validAWSUUID(cmd.ChangeID) ||
		!validAWSUUID(cmd.ConfirmationID) ||
		!validAWSUUID(cmd.TaskID) ||
		strings.TrimSpace(cmd.ProviderChangeSetID) == "" ||
		cmd.Attempt == 0 ||
		cmd.LeaseEpoch == 0 ||
		cmd.ExpectedChangeRevision < 1 ||
		cmd.ExpectedTaskRevision < 1 ||
		cmd.ExpectedConfirmationRevision < 1 {
		return agentaws.Change{}, agentaws.ErrInvalid
	}
	tx, err := r.store.db.BeginTx(ctx, nil)
	if err != nil {
		return agentaws.Change{}, err
	}
	defer tx.Rollback()
	var (
		changeTaskID         string
		changeConfirmationID string
		changeStatus         agentaws.ChangeStatus
		changeStage          agentaws.ChangeStage
		changeRevision       int64
	)
	if err = tx.QueryRowContext(ctx, `SELECT task_id::text,confirmation_id::text,status,stage,revision FROM core_aws_changes WHERE owner_id=$1 AND change_id=$2 FOR UPDATE`, r.ownerID, cmd.ChangeID).Scan(
		&changeTaskID,
		&changeConfirmationID,
		&changeStatus,
		&changeStage,
		&changeRevision,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return agentaws.Change{}, agentaws.ErrNotFound
		}
		return agentaws.Change{}, err
	}
	if changeTaskID != cmd.TaskID ||
		changeConfirmationID != cmd.ConfirmationID ||
		changeStatus != agentaws.ChangeRunning ||
		changeStage != agentaws.StageReconciling ||
		changeRevision != cmd.ExpectedChangeRevision {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	var (
		taskStatus   string
		taskRevision int64
		taskAttempt  int
		taskEpoch    int64
		taskExpiry   *time.Time
	)
	now := time.Now().UTC()
	if err = tx.QueryRowContext(ctx, `SELECT status,revision,attempt,lease_epoch,lease_expires_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, r.ownerID, cmd.TaskID).Scan(
		&taskStatus,
		&taskRevision,
		&taskAttempt,
		&taskEpoch,
		&taskExpiry,
	); err != nil {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	if taskStatus != "running" ||
		taskRevision < cmd.ExpectedTaskRevision ||
		taskAttempt != int(cmd.Attempt) ||
		taskEpoch != int64(cmd.LeaseEpoch) ||
		taskExpiry == nil ||
		!taskExpiry.After(now) {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	var (
		confirmationState    coreconfirmation.State
		confirmationRevision int64
		confirmationTaskID   string
		reservationRaw       []byte
	)
	if err = tx.QueryRowContext(ctx, `SELECT state,revision,task_id::text,reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2 FOR UPDATE`, r.ownerID, cmd.ConfirmationID).Scan(
		&confirmationState,
		&confirmationRevision,
		&confirmationTaskID,
		&reservationRaw,
	); err != nil {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	var reservation struct {
		TaskID       string `json:"task_id"`
		Attempt      uint32 `json:"attempt"`
		LeaseEpoch   uint64 `json:"lease_epoch"`
		TaskRevision int64  `json:"task_revision"`
		Active       bool   `json:"active"`
	}
	if confirmationState != coreconfirmation.StateConsumed ||
		confirmationRevision != cmd.ExpectedConfirmationRevision ||
		confirmationTaskID != cmd.TaskID ||
		json.Unmarshal(reservationRaw, &reservation) != nil ||
		!reservation.Active ||
		reservation.TaskID != cmd.TaskID ||
		reservation.Attempt != cmd.Attempt ||
		reservation.LeaseEpoch > cmd.LeaseEpoch ||
		reservation.TaskRevision > taskRevision {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	if reservation.LeaseEpoch < cmd.LeaseEpoch {
		if reservation.TaskRevision >= taskRevision {
			return agentaws.Change{}, agentaws.ErrRevisionConflict
		}
		reservation.LeaseEpoch = cmd.LeaseEpoch
		reservation.TaskRevision = taskRevision
		reservationRaw, _ = json.Marshal(reservation)
		res, updateErr := tx.ExecContext(ctx, `UPDATE agent_confirmations SET reservation_json=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND confirmation_id=$4 AND state='consumed' AND revision=$5`, reservationRaw, now, r.ownerID, cmd.ConfirmationID, confirmationRevision)
		if updateErr != nil {
			return agentaws.Change{}, updateErr
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return agentaws.Change{}, agentaws.ErrRevisionConflict
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE core_aws_changes SET change_set_id=$1,stage='change_set_ready',revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND change_id=$4 AND task_id=$5 AND confirmation_id=$6 AND revision=$7 AND status='running' AND stage='reconciling'`, cmd.ProviderChangeSetID, now, r.ownerID, cmd.ChangeID, cmd.TaskID, cmd.ConfirmationID, cmd.ExpectedChangeRevision)
	if err != nil {
		return agentaws.Change{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return agentaws.Change{}, agentaws.ErrRevisionConflict
	}
	var sequence int64
	if err = tx.QueryRowContext(ctx, `INSERT INTO core_aws_event_counters(owner_id,change_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,change_id) DO UPDATE SET next_sequence=core_aws_event_counters.next_sequence+1 RETURNING next_sequence-1`, r.ownerID, cmd.ChangeID).Scan(&sequence); err != nil {
		return agentaws.Change{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_aws_events(owner_id,change_id,sequence,event_id,task_id,kind,revision,at) VALUES($1,$2,$3,$4,$5,'change_set_evidence_reconciled',$6,$7)`, r.ownerID, cmd.ChangeID, sequence, uuid.NewString(), cmd.TaskID, cmd.ExpectedChangeRevision+1, now); err != nil {
		return agentaws.Change{}, err
	}
	if err = tx.Commit(); err != nil {
		return agentaws.Change{}, err
	}
	return r.GetChange(ctx, cmd.ChangeID)
}

// AgentAWSDDL is the exact PostgreSQL schema required by the AWS control
// plane. It is exposed for the owning migration package; this package does
// not mutate shared migration registries.
const AgentAWSDDL = `CREATE TABLE IF NOT EXISTS core_aws_credentials (
 owner_id text NOT NULL, credential_id uuid NOT NULL, revision bigint NOT NULL CHECK (revision > 0),
 envelope_version integer NOT NULL, aad_version integer NOT NULL, key_id text NOT NULL,
 nonce bytea NOT NULL CHECK (octet_length(nonce)=12), ciphertext bytea NOT NULL,
 envelope_digest text NOT NULL CHECK (envelope_digest ~ '^[a-f0-9]{64}$'), name text NOT NULL,
 region text NOT NULL, account_id text NOT NULL DEFAULT '', user_arn text NOT NULL DEFAULT '',
 verified_revision bigint NOT NULL DEFAULT 0, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 PRIMARY KEY(owner_id,credential_id,revision)
);
-- Current projection and tombstone are distinct from immutable revisions.
CREATE TABLE IF NOT EXISTS core_aws_credential_current (
 owner_id text NOT NULL, credential_id uuid NOT NULL, revision bigint NOT NULL CHECK (revision > 0), deleted_at timestamptz NULL,
 PRIMARY KEY(owner_id,credential_id),
 FOREIGN KEY(owner_id,credential_id,revision) REFERENCES core_aws_credentials(owner_id,credential_id,revision) ON DELETE RESTRICT
);
CREATE TABLE IF NOT EXISTS core_aws_plans (
 owner_id text NOT NULL, plan_id uuid NOT NULL, credential_id uuid NOT NULL, credential_revision bigint NOT NULL CHECK (credential_revision > 0),
 region text NOT NULL, stack_name text NOT NULL, operation text NOT NULL, template bytea NOT NULL,
 template_sha256 text NOT NULL, parameters_json jsonb NOT NULL, tags_json jsonb NOT NULL,
 capabilities_json jsonb NOT NULL, revision bigint NOT NULL DEFAULT 1, created_at timestamptz NOT NULL,
 PRIMARY KEY(owner_id,plan_id), FOREIGN KEY(owner_id,credential_id,credential_revision) REFERENCES core_aws_credentials(owner_id,credential_id,revision) ON DELETE RESTRICT
);
CREATE TABLE IF NOT EXISTS core_aws_changes (
 owner_id text NOT NULL, change_id uuid NOT NULL, plan_id uuid NOT NULL, credential_id uuid NOT NULL, credential_revision bigint NOT NULL,
 task_id uuid NOT NULL, confirmation_id uuid NOT NULL, operation text NOT NULL, status text NOT NULL,
 stage text NOT NULL, change_set_id text NOT NULL DEFAULT '', provider_request_digest text NOT NULL DEFAULT '',
 provider_token text NOT NULL DEFAULT '', revision bigint NOT NULL DEFAULT 1, error_code text NOT NULL DEFAULT '',
 error_summary text NOT NULL DEFAULT '', created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
 PRIMARY KEY(owner_id,change_id), FOREIGN KEY(owner_id,plan_id) REFERENCES core_aws_plans(owner_id,plan_id) ON DELETE RESTRICT,
 FOREIGN KEY(owner_id,credential_id,credential_revision) REFERENCES core_aws_credentials(owner_id,credential_id,revision) ON DELETE RESTRICT,
 FOREIGN KEY(owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id) ON DELETE RESTRICT,
 FOREIGN KEY(owner_id,confirmation_id) REFERENCES agent_confirmations(owner_id,confirmation_id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS core_aws_changes_confirmation_idx ON core_aws_changes(owner_id,confirmation_id);
CREATE TABLE IF NOT EXISTS core_aws_replays (owner_id text NOT NULL, operation text NOT NULL, idempotency_key uuid NOT NULL, request_hash text NOT NULL,
 response_json jsonb NOT NULL, error_code text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(owner_id,operation,idempotency_key));
CREATE TABLE IF NOT EXISTS core_aws_events (owner_id text NOT NULL, change_id uuid NOT NULL, sequence bigint NOT NULL, event_id uuid NOT NULL,
 task_id uuid NOT NULL, kind text NOT NULL, revision bigint NOT NULL, at timestamptz NOT NULL,
 PRIMARY KEY(owner_id,change_id,sequence), FOREIGN KEY(owner_id,change_id) REFERENCES core_aws_changes(owner_id,change_id) ON DELETE RESTRICT,
 FOREIGN KEY(owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id) ON DELETE RESTRICT);
CREATE TABLE IF NOT EXISTS core_aws_event_counters (owner_id text NOT NULL, change_id uuid NOT NULL, next_sequence bigint NOT NULL CHECK(next_sequence>0), PRIMARY KEY(owner_id,change_id), FOREIGN KEY(owner_id,change_id) REFERENCES core_aws_changes(owner_id,change_id) ON DELETE CASCADE);
CREATE INDEX IF NOT EXISTS core_aws_changes_target_idx ON core_aws_changes(owner_id,plan_id,status);
ALTER TABLE core_aws_credentials ALTER COLUMN key_id DROP NOT NULL;
ALTER TABLE core_aws_credentials ALTER COLUMN nonce DROP NOT NULL;
ALTER TABLE core_aws_credentials ALTER COLUMN ciphertext DROP NOT NULL;`
