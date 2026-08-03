package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	confirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	ext "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/extensions"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

// AtomicExtensionStore is the only store that may advertise extension
// readiness. Its lifecycle commit uses one PostgreSQL transaction for the
// installation/version, canonical secret envelopes, task, confirmation and
// replay record.
type AtomicExtensionStore struct {
	DB        *DatabaseStore
	Enveloper *AgentSecretEnveloper
}

func NewAtomicExtensionStore(db *DatabaseStore, enveloper *AgentSecretEnveloper) (*AtomicExtensionStore, error) {
	if db == nil || enveloper == nil {
		return nil, ext.ErrUnavailable
	}
	return &AtomicExtensionStore{DB: db, Enveloper: enveloper}, nil
}
func (s *AtomicExtensionStore) SupportsAtomicLifecycle() bool {
	return s != nil && s.DB != nil && s.Enveloper != nil
}
func (s *AtomicExtensionStore) Get(c context.Context, o, id string) (ext.Installation, error) {
	return s.DB.Get(c, o, id)
}
func (s *AtomicExtensionStore) List(c context.Context, o string, n int, t string) ([]ext.Installation, string, error) {
	return s.DB.List(c, o, n, t)
}
func (s *AtomicExtensionStore) Put(c context.Context, i ext.Installation) error {
	return s.DB.Put(c, i)
}
func (s *AtomicExtensionStore) PutCAS(c context.Context, i ext.Installation, r int64) error {
	return s.DB.PutCAS(c, i, r)
}
func (s *AtomicExtensionStore) Replay(c context.Context, o, k, d string) (ext.LifecycleResult, bool, error) {
	return s.DB.Replay(c, o, k, d)
}
func (s *AtomicExtensionStore) SaveReplay(c context.Context, o, k, d string, r ext.LifecycleResult) error {
	return s.DB.SaveReplay(c, o, k, d, r)
}

func (s *AtomicExtensionStore) CommitLifecycle(ctx context.Context, r ext.AtomicLifecycleRequest) (ext.LifecycleResult, error) {
	if !s.SupportsAtomicLifecycle() || r.Installation.Validate() != nil || r.OwnerID == "" || r.IdempotencyKey == "" || r.MutationDigest == "" {
		return ext.LifecycleResult{}, ext.ErrInvalid
	}
	returnResult := ext.LifecycleResult{}
	err := s.DB.writer.Do(s.DB.db, nil, func(tx *sql.Tx) error {
		var oldDigest string
		var oldRaw []byte
		e := tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM p2p_agent_extension_replays WHERE owner_id=$1 AND idempotency_key=$2 FOR UPDATE`, r.OwnerID, r.IdempotencyKey).Scan(&oldDigest, &oldRaw)
		if e == nil {
			if oldDigest != r.MutationDigest {
				return ext.ErrIdempotencyConflict
			}
			return json.Unmarshal(oldRaw, &returnResult)
		}
		if e != sql.ErrNoRows {
			return e
		}
		if r.ExpectedRevision > 0 {
			var current int64
			if e = tx.QueryRowContext(ctx, `SELECT revision FROM p2p_agent_extensions WHERE owner_id=$1 AND installation_id=$2 FOR UPDATE`, r.OwnerID, r.Installation.ID).Scan(&current); e == sql.ErrNoRows || current != r.ExpectedRevision {
				return ext.ErrRevisionConflict
			}
			if e != nil {
				return e
			}
		}
		v := r.Installation.Versions[len(r.Installation.Versions)-1]
		cid := uuid.New()
		candidate, _ := json.Marshal(r.Installation.Candidate)
		if _, e = tx.ExecContext(ctx, `INSERT INTO p2p_agent_extensions(owner_id,installation_id,candidate_json,state,revision,active_version_id,created_at,updated_at) VALUES($1,$2,$3::jsonb,$4,$5,$6,$7,$8) ON CONFLICT(owner_id,installation_id) DO UPDATE SET candidate_json=EXCLUDED.candidate_json,state=EXCLUDED.state,revision=EXCLUDED.revision,active_version_id=EXCLUDED.active_version_id,updated_at=EXCLUDED.updated_at`, r.OwnerID, r.Installation.ID, string(candidate), r.Installation.State, r.Installation.Revision, r.Installation.ActiveVersionID, r.Installation.CreatedAt, r.Installation.UpdatedAt); e != nil {
			return e
		}
		if e = insertVersion(ctx, tx, r.OwnerID, r.Installation.ID, v); e != nil {
			return e
		}
		for _, in := range r.SecretInputs {
			if e = insertSecret(ctx, tx, s.Enveloper, r.OwnerID, r.Installation.ID, v.VersionID, r.Installation.Revision, in); e != nil {
				return e
			}
		}
		taskID := uuid.NewSHA1(uuid.Nil, []byte(r.OwnerID+"\x00task\x00"+r.IdempotencyKey))
		if parsed, pe := uuid.Parse(r.Task.TaskID); pe == nil {
			taskID = parsed
		}
		now := time.Now().UTC()
		payload := task.ExtensionTaskPayload{Operation: task.ExtensionOperation(r.Operation), InstallationID: r.Installation.ID, ExpectedRevision: uint64(r.Installation.Revision), Version: v.VersionID, Digest: v.ContentDigest, ConfirmationID: cid.String()}
		if r.Task.Payload != nil {
			_ = json.Unmarshal(r.Task.Payload, &payload)
		}
		spec := task.TaskSpec{Kind: task.TaskKindExtension, Goal: r.Task.Goal, Payload: task.TaskPayload{Extension: &payload}, IdempotencyKey: r.IdempotencyKey, AvailableAt: now}
		rawSpec, _ := json.Marshal(spec)
		if _, e = tx.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,revision,available_at,created_at,updated_at) VALUES($1,$2,$3::jsonb,'waiting_user',1,1,$4,$4,$4)`, taskID, r.OwnerID, string(rawSpec), now); e != nil {
			return e
		}
		b := toCoreBinding(r.Confirmation.Binding)
		braw, _ := json.Marshal(b)
		if _, e = tx.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9,$10,$10)`, cid, r.OwnerID, b.OperationDomain, b.TargetID, b.TargetRevision, b.Digest, braw, taskID, r.Confirmation.ExpiresAt, now); e != nil {
			return e
		}
		result := ext.LifecycleResult{Installation: r.Installation, TaskID: taskID.String(), ConfirmationID: cid.String()}
		encoded, _ := json.Marshal(result)
		if _, e = tx.ExecContext(ctx, `INSERT INTO p2p_agent_extension_replays(owner_id,idempotency_key,request_digest,response_json,created_at) VALUES($1,$2,$3,$4::jsonb,$5)`, r.OwnerID, r.IdempotencyKey, r.MutationDigest, string(encoded), now); e != nil {
			return e
		}
		returnResult = result
		return nil
	})
	if err != nil {
		return ext.LifecycleResult{}, err
	}
	return returnResult, nil
}
func (s *AtomicExtensionStore) CommitUninstall(ctx context.Context, r ext.AtomicLifecycleRequest) (ext.LifecycleResult, error) {
	return s.CommitLifecycle(ctx, r)
}
func (s *AtomicExtensionStore) CommitExecute(ctx context.Context, r ext.ExecuteAtomicRequest) (ext.ExecuteResult, error) {
	payload := task.ExtensionTaskPayload{Operation: task.ExtensionOperationExecuteTool, InstallationID: r.Installation.ID, ExpectedRevision: uint64(r.Installation.Revision), Version: r.Version.VersionID, Digest: r.Version.ContentDigest, ToolName: r.Tool.Name, CanonicalInputJSON: append([]byte(nil), r.Input...)}
	raw, _ := json.Marshal(payload)
	r.Task.Payload = raw
	lr, e := s.CommitLifecycle(ctx, ext.AtomicLifecycleRequest{OwnerID: r.OwnerID, IdempotencyKey: r.IdempotencyKey, Operation: string(task.ExtensionOperationExecuteTool), MutationDigest: task.Digest(struct {
		Owner, Key, Install, Version, Tool string
		Input                              json.RawMessage
	}{r.OwnerID, r.IdempotencyKey, r.Installation.ID, r.Version.VersionID, r.Tool.Name, r.Input}), ExpectedRevision: r.Installation.Revision, Installation: r.Installation, Task: r.Task, Confirmation: r.Confirmation})
	if e != nil {
		return ext.ExecuteResult{}, e
	}
	return ext.ExecuteResult{TaskID: lr.TaskID, ConfirmationID: lr.ConfirmationID, InstallationID: r.Installation.ID, VersionID: r.Version.VersionID}, nil
}
func (s *AtomicExtensionStore) FinalizeExecution(ctx context.Context, r ext.ExecutionFinalizeRequest) error {
	if !s.SupportsAtomicLifecycle() {
		return ext.ErrUnavailable
	}
	return (&PostgresExtensionExecutionFinalizer{DB: s.DB}).FinalizeExecution(ctx, r)
}
func (s *AtomicExtensionStore) FinalizeLifecycle(ctx context.Context, r ext.ExecutionFinalizeRequest) error {
	if !s.SupportsAtomicLifecycle() {
		return ext.ErrUnavailable
	}
	return (&PostgresExtensionExecutionFinalizer{DB: s.DB}).FinalizeLifecycle(ctx, r)
}
func insertVersion(ctx context.Context, tx *sql.Tx, owner, id string, v ext.Version) error {
	pin, _ := json.Marshal(v.Pin)
	execution, _ := json.Marshal(v.Execution)
	network, _ := json.Marshal(v.NetworkGrants)
	secrets, _ := json.Marshal(v.SecretGrants)
	tools, _ := json.Marshal(v.Tools)
	_, e := tx.ExecContext(ctx, `INSERT INTO p2p_agent_extension_versions(owner_id,installation_id,version_id,pin_json,content_digest,manifest_digest,execution_digest,network_digest,secret_digest,execution_json,network_grants_json,secret_grants_json,tools_json,created_at) VALUES($1,$2,$3,$4::jsonb,$5,$6,$7,$8,$9,$10::jsonb,$11::jsonb,$12::jsonb,$13::jsonb,$14) ON CONFLICT DO NOTHING`, owner, id, v.VersionID, string(pin), v.ContentDigest, v.ManifestDigest, v.ExecutionDigest, v.NetworkDigest, v.SecretDigest, string(execution), string(network), string(secrets), string(tools), v.CreatedAt)
	return e
}
func insertSecret(ctx context.Context, tx *sql.Tx, e *AgentSecretEnveloper, owner, installation, version string, revision int64, in ext.SecretInput) error {
	if in.Value == "" || in.Purpose != "mcp_credential" {
		return ext.ErrInvalid
	}
	digest := sha256.Sum256([]byte(in.Value))
	entityID := extensionSecretEntityID(installation, version)
	binding := AgentSecretBinding{Domain: "extension", OwnerID: owner, EntityID: entityID, Revision: revision, Purpose: in.Purpose, Reference: in.ReferenceID, BindingDigest: digest}
	plain := []byte(in.Value)
	sealed, err := e.Seal(binding, plain)
	clear(plain)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_secrets(secret_domain,owner_id,entity_id,secret_revision,purpose,reference,binding_digest,envelope_version,aad_version,key_id,nonce,ciphertext,created_at) VALUES('extension',$1,$2,$3,$4,$5,$6,1,1,$7,$8,$9,clock_timestamp())`, owner, entityID, revision, in.Purpose, in.ReferenceID, digest[:], sealed.KeyID, sealed.Nonce, sealed.Ciphertext)
	return err
}
func toCoreBinding(b ext.ConfirmationBinding) confirmation.Binding {
	g := make([]confirmation.SecretGrant, 0, len(b.SecretGrants))
	for _, x := range b.SecretGrants {
		g = append(g, confirmation.SecretGrant{ReferenceID: x.ReferenceID, Purpose: confirmation.SecretPurpose(x.Purpose), BindingDigest: confirmation.Digest(x.BindingDigest)})
	}
	operation := strings.TrimSpace(b.Operation)
	for strings.HasPrefix(operation, "extension.") {
		operation = strings.TrimPrefix(operation, "extension.")
	}
	core := confirmation.Binding{OwnerID: b.OwnerID, OperationDomain: "extension." + operation, TargetID: b.TargetID, TargetRevision: b.TargetRevision, TargetKind: "mcp", ExtensionVersionID: b.VersionID, SourceVersion: b.SourceVersion, SourceCommit: b.SourceCommit, ContentDigest: confirmation.Digest(b.ContentDigest), ManifestDigest: confirmation.Digest(b.ManifestDigest), ExecutionDigest: confirmation.Digest(b.ExecutionDigest), PermissionDigest: confirmation.Digest(b.ToolSchemaDigest), ParameterDigest: confirmation.Digest(b.ParameterDigest), NetworkDigest: confirmation.Digest(b.NetworkDigest), SecretGrantDigest: confirmation.Digest(b.SecretDigest), SelectedTool: b.ToolName, NetworkGrants: b.NetworkGrants, SecretGrants: g}
	if normalized, err := core.Normalize(); err == nil {
		core = normalized
	}
	core.Digest = task.Digest(core)
	return core
}

var _ ext.AtomicLifecycleStore = (*AtomicExtensionStore)(nil)
var _ ext.ExecutionFinalizer = (*AtomicExtensionStore)(nil)
var _ ext.LifecycleFinalizer = (*AtomicExtensionStore)(nil)
