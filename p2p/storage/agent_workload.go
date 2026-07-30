package storage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	workload "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/aws/workload"
	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

// WorkloadRepository is implemented by the durable owner store. The typed
// interface keeps plan, operation, event and replay transitions atomic while
// allowing confirmation/task implementations to be wired by the parent.
type WorkloadRepository interface{ workload.Store }

// PostgresWorkloadStore is the owner-fenced adapter used by the service
// wiring. Its typed implementation can be swapped for the SQL transaction
// implementation without changing provider or task contracts.
type PostgresWorkloadStore struct {
	store   *DatabaseStore
	ownerID string
}

var _ workload.Store = (*PostgresWorkloadStore)(nil)
var _ workload.FencedStore = (*PostgresWorkloadStore)(nil)
var _ workload.AWSCredentialGrantPinningStore = (*PostgresWorkloadStore)(nil)

func (*PostgresWorkloadStore) PinsAWSCredentialGrants() bool { return true }

func NewAgentWorkloadStore(store *DatabaseStore, ownerID string) (*PostgresWorkloadStore, error) {
	if store == nil || strings.TrimSpace(ownerID) == "" {
		return nil, errors.New("storage: invalid workload store owner")
	}
	return &PostgresWorkloadStore{store: store, ownerID: ownerID}, nil
}

func (s *PostgresWorkloadStore) CreatePlan(c context.Context, in workload.PlanInput) (workload.Plan, error) {
	if s == nil || s.store == nil || s.store.db == nil || !workload.ValidUUID(in.IdempotencyKey) {
		return workload.Plan{}, workload.ErrInvalid
	}
	now := time.Now().UTC()
	p := workload.Plan{ID: uuid.NewString(), Revision: 1, Summary: in.Summary, Artifact: in.Artifact, Source: in.Source, CommandSteps: in.CommandSteps, ImageDigest: in.ImageDigest, ImageURI: in.ImageURI, TargetKind: in.TargetKind, Target: in.Target, NetworkGrants: in.NetworkGrants, SecretGrants: in.SecretGrants, SecretGrantRefs: in.SecretGrantRefs, ResourceLimits: in.ResourceLimits, ExpiresAt: in.ExpiresAt.UTC(), CreatedAt: now}
	if p.ExpiresAt.IsZero() {
		return workload.Plan{}, workload.ErrInvalid
	}
	var err error
	p, err = p.Normalize()
	if err != nil {
		return workload.Plan{}, err
	}
	tx, err := s.store.db.BeginTx(c, nil)
	if err != nil {
		return workload.Plan{}, err
	}
	defer tx.Rollback()
	// Pin the durable AWS credential revision while creating the immutable plan.
	// This happens before the plan/request digest is calculated, so the pinned
	// revision is part of the persisted DTO, confirmation binding and replay.
	if err = s.pinAWSCredentialGrantTx(c, tx, &p); err != nil {
		return workload.Plan{}, err
	}
	p.Digest = ""
	p, err = p.Normalize()
	if err != nil {
		return workload.Plan{}, err
	}
	hash := workload.PlanInputDigest(p)
	raw, _ := json.Marshal(p)
	targetRaw, _ := json.Marshal(p.Target.Identity)
	limitsRaw, _ := json.Marshal(p.ResourceLimits)
	refsRaw, _ := json.Marshal(p.SecretGrantRefs)
	var priorHash string
	var priorRaw []byte
	err = tx.QueryRowContext(c, `SELECT request_hash,response_json FROM core_workload_idempotency WHERE owner_id=$1 AND operation='plan_create' AND idempotency_key=$2 FOR UPDATE`, s.ownerID, in.IdempotencyKey).Scan(&priorHash, &priorRaw)
	if err == nil {
		if priorHash != hash {
			return workload.Plan{}, workload.ErrConflict
		}
		_ = json.Unmarshal(priorRaw, &p)
		return p, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return workload.Plan{}, err
	}
	if err = tx.QueryRowContext(c, `SELECT plan_json FROM core_workload_plans WHERE owner_id=$1 AND digest=$2`, s.ownerID, p.Digest).Scan(&priorRaw); err == nil {
		_ = json.Unmarshal(priorRaw, &p)
		return p, tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return workload.Plan{}, err
	}
	if _, err = tx.ExecContext(c, `INSERT INTO core_workload_plans(plan_id,owner_id,create_idempotency_key,create_request_hash,revision,digest,summary,plan_json,target_kind,target_identity_json,resource_limits_json,secret_grant_refs_json,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`, p.ID, s.ownerID, in.IdempotencyKey, hash, p.Revision, p.Digest, p.Summary, raw, p.TargetKind, targetRaw, limitsRaw, refsRaw, p.ExpiresAt, p.CreatedAt); err != nil {
		return workload.Plan{}, mapWorkloadError(err)
	}
	if _, err = tx.ExecContext(c, `INSERT INTO core_workload_idempotency(owner_id,operation,idempotency_key,request_hash,plan_id,response_json) VALUES($1,'plan_create',$2,$3,$4,$5)`, s.ownerID, in.IdempotencyKey, hash, p.ID, raw); err != nil {
		return workload.Plan{}, mapWorkloadError(err)
	}
	if err = tx.Commit(); err != nil {
		return workload.Plan{}, err
	}
	return p, nil
}

// pinAWSCredentialGrantTx proves that the plan references an existing,
// encrypted immutable AWS credential revision, then replaces any caller
// supplied binding digest with the owner-bound AAD digest for that revision.
// The plan never stores a mutable "latest credential" reference.
func (s *PostgresWorkloadStore) pinAWSCredentialGrantTx(ctx context.Context, tx *sql.Tx, p *workload.Plan) error {
	for i := range p.SecretGrantRefs {
		grant := &p.SecretGrantRefs[i]
		if grant.Purpose != coreconfirmation.SecretPurposeAWSCredential {
			continue
		}
		if !workload.ValidUUID(grant.ReferenceID) || grant.Revision < 1 {
			return workload.ErrInvalid
		}
		var region, account string
		var verified int64
		err := tx.QueryRowContext(ctx, `SELECT c.region,c.account_id,c.verified_revision
			FROM core_aws_credentials c
			JOIN p2p_agent_secrets secret ON secret.secret_domain='aws' AND secret.owner_id=c.owner_id AND secret.entity_id=c.credential_id::text AND secret.secret_revision=c.revision AND secret.purpose='credential'
			WHERE c.owner_id=$1 AND c.credential_id=$2 AND c.revision=$3
			FOR KEY SHARE`, s.ownerID, grant.ReferenceID, grant.Revision).Scan(&region, &account, &verified)
		if errors.Is(err, sql.ErrNoRows) {
			return workload.ErrNotFound
		}
		if err != nil {
			return err
		}
		if verified != grant.Revision || region != p.Target.Region || account != p.Target.AccountID || region != p.Target.Identity.Region || account != p.Target.Identity.AccountID {
			return workload.ErrInvalid
		}
		binding := credentialBinding(s.ownerID, grant.ReferenceID, grant.Revision)
		grant.BindingDigest = coreconfirmation.Digest(hex.EncodeToString(binding.BindingDigest[:]))
	}
	return nil
}
func (s *PostgresWorkloadStore) GetPlan(c context.Context, id string) (workload.Plan, error) {
	if s == nil || s.store == nil || s.store.db == nil || !workload.ValidUUID(id) {
		return workload.Plan{}, workload.ErrInvalid
	}
	var raw []byte
	err := s.store.db.QueryRowContext(c, `SELECT plan_json FROM core_workload_plans WHERE owner_id=$1 AND plan_id=$2`, s.ownerID, id).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return workload.Plan{}, workload.ErrNotFound
	}
	if err != nil {
		return workload.Plan{}, err
	}
	var p workload.Plan
	if json.Unmarshal(raw, &p) != nil {
		return workload.Plan{}, workload.ErrInvalid
	}
	return p, nil
}
func (s *PostgresWorkloadStore) GetWorkload(c context.Context, id string) (workload.Workload, error) {
	if s == nil || s.store == nil || s.store.db == nil || !workload.ValidUUID(id) {
		return workload.Workload{}, workload.ErrInvalid
	}
	var w workload.Workload
	var actual []byte
	err := s.store.db.QueryRowContext(c, `SELECT workload_id::text,revision,plan_id::text,plan_digest,target_kind,state,actual_snapshot_json,updated_at FROM core_workloads WHERE owner_id=$1 AND workload_id=$2`, s.ownerID, id).Scan(&w.ID, &w.Revision, &w.PlanID, &w.PlanDigest, &w.TargetKind, &w.State, &actual, &w.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return workload.Workload{}, workload.ErrNotFound
	}
	if err != nil {
		return workload.Workload{}, err
	}
	_ = json.Unmarshal(actual, &w.Actual)
	w.Identity = w.Actual.Identity
	return w, nil
}
func (s *PostgresWorkloadStore) ListWorkloads(c context.Context, n int, k string) ([]workload.Workload, string, error) {
	k = strings.TrimSpace(k)
	if n <= 0 || n > 200 || (k != "" && !workload.ValidUUID(k)) {
		return nil, "", workload.ErrInvalid
	}
	rows, err := s.store.db.QueryContext(c, `SELECT workload_id::text FROM core_workloads WHERE owner_id=$1 AND workload_id>COALESCE(NULLIF($2,'')::uuid,'00000000-0000-0000-0000-000000000000'::uuid) ORDER BY workload_id LIMIT $3`, s.ownerID, k, n+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	out := make([]workload.Workload, 0, len(ids))
	for _, id := range ids {
		v, err := s.GetWorkload(c, id)
		if err != nil {
			return nil, "", err
		}
		out = append(out, v)
	}
	next := ""
	if len(out) > n {
		next = out[n-1].ID
		out = out[:n]
	}
	return out, next, nil
}
func (s *PostgresWorkloadStore) ListPlans(c context.Context, n int, k string) ([]workload.Plan, string, error) {
	k = strings.TrimSpace(k)
	if n <= 0 || n > 200 || (k != "" && !workload.ValidUUID(k)) {
		return nil, "", workload.ErrInvalid
	}
	rows, err := s.store.db.QueryContext(c, `SELECT plan_id::text FROM core_workload_plans WHERE owner_id=$1 AND plan_id>COALESCE(NULLIF($2,'')::uuid,'00000000-0000-0000-0000-000000000000'::uuid) ORDER BY plan_id LIMIT $3`, s.ownerID, k, n+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, "", err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	out := make([]workload.Plan, 0, len(ids))
	for _, id := range ids {
		v, err := s.GetPlan(c, id)
		if err != nil {
			return nil, "", err
		}
		out = append(out, v)
	}
	next := ""
	if len(out) > n {
		next = out[n-1].ID
		out = out[:n]
	}
	return out, next, nil
}
func (s *PostgresWorkloadStore) GetOperation(c context.Context, id string) (workload.Operation, error) {
	if s == nil || s.store == nil || s.store.db == nil || !workload.ValidUUID(id) {
		return workload.Operation{}, workload.ErrInvalid
	}
	return s.loadOperation(c, id)
}
func (s *PostgresWorkloadStore) ListEvents(c context.Context, id string, after uint64) ([]workload.Event, error) {
	if s == nil || s.store == nil || s.store.db == nil || !workload.ValidUUID(id) {
		return nil, workload.ErrInvalid
	}
	rows, err := s.store.db.QueryContext(c, `SELECT sequence,kind,status,message,readback_json,at FROM core_workload_events WHERE owner_id=$1 AND operation_id=$2 AND sequence>$3 ORDER BY sequence`, s.ownerID, id, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []workload.Event{}
	for rows.Next() {
		var e workload.Event
		var raw []byte
		if err := rows.Scan(&e.Sequence, &e.Kind, &e.Status, &e.Message, &raw, &e.At); err != nil {
			return nil, err
		}
		e.OperationID = id
		e.Readback = raw
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *PostgresWorkloadStore) RequestOperation(c context.Context, in workload.RequestCommand) (workload.RequestResult, error) {
	if s == nil || s.store == nil || s.store.db == nil || !workload.ValidUUID(in.PlanID) || !workload.ValidUUID(in.IdempotencyKey) || (in.WorkloadID != "" && !workload.ValidUUID(in.WorkloadID)) || (in.WorkloadID == "") != (in.ExpectedWorkloadRevision == 0) || (in.Kind != workload.OperationApply && in.Kind != workload.OperationDestroy) {
		return workload.RequestResult{}, workload.ErrInvalid
	}
	tx, err := s.store.db.BeginTx(c, nil)
	if err != nil {
		return workload.RequestResult{}, err
	}
	defer tx.Rollback()
	hash := workload.RequestInputDigest(in)
	var prior []byte
	var priorHash string
	err = tx.QueryRowContext(c, `SELECT request_hash,response_json FROM core_workload_idempotency WHERE owner_id=$1 AND operation=$2 AND idempotency_key=$3 FOR UPDATE`, s.ownerID, string(in.Kind), in.IdempotencyKey).Scan(&priorHash, &prior)
	if err == nil {
		if priorHash != hash {
			return workload.RequestResult{}, workload.ErrConflict
		}
		var out workload.RequestResult
		if json.Unmarshal(prior, &out) != nil {
			return workload.RequestResult{}, workload.ErrInvalid
		}
		return out, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return workload.RequestResult{}, err
	}
	var planRaw []byte
	if err = tx.QueryRowContext(c, `SELECT plan_json FROM core_workload_plans WHERE owner_id=$1 AND plan_id=$2`, s.ownerID, in.PlanID).Scan(&planRaw); errors.Is(err, sql.ErrNoRows) {
		return workload.RequestResult{}, workload.ErrNotFound
	} else if err != nil {
		return workload.RequestResult{}, err
	}
	var p workload.Plan
	if json.Unmarshal(planRaw, &p) != nil {
		return workload.RequestResult{}, workload.ErrInvalid
	}
	if p.Validate() != nil {
		return workload.RequestResult{}, workload.ErrInvalid
	}
	wid := in.WorkloadID
	if wid == "" {
		wid = uuid.NewString()
	}
	if in.Kind == workload.OperationDestroy && in.WorkloadID == "" {
		return workload.RequestResult{}, workload.ErrInvalid
	}
	if in.WorkloadID != "" {
		var existing struct {
			revision                              uint64
			state, planID, planDigest, targetKind string
		}
		if err = tx.QueryRowContext(c, `SELECT revision,state,plan_id::text,plan_digest,target_kind FROM core_workloads WHERE owner_id=$1 AND workload_id=$2 FOR UPDATE`, s.ownerID, wid).Scan(&existing.revision, &existing.state, &existing.planID, &existing.planDigest, &existing.targetKind); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return workload.RequestResult{}, workload.ErrNotFound
			}
			return workload.RequestResult{}, err
		}
		if existing.revision != in.ExpectedWorkloadRevision {
			return workload.RequestResult{}, workload.ErrRevisionConflict
		}
		if in.Kind == workload.OperationDestroy {
			if existing.state != "ready" || existing.planID != p.ID || existing.planDigest != p.Digest || existing.targetKind != string(p.TargetKind) {
				return workload.RequestResult{}, workload.ErrConflict
			}
		} else if existing.state != "destroyed" && existing.state != "ready" && existing.state != "failed" {
			return workload.RequestResult{}, workload.ErrConflict
		}
	}
	expectedWorkloadRevision := in.ExpectedWorkloadRevision
	if expectedWorkloadRevision == 0 {
		expectedWorkloadRevision = 1
	}
	if in.ExpiresAt.IsZero() {
		in.ExpiresAt = p.ExpiresAt.UTC()
	}
	now := time.Now().UTC()
	if !p.ExpiresAt.After(now) || !in.ExpiresAt.After(now) || in.ExpiresAt.After(p.ExpiresAt.UTC()) {
		return workload.RequestResult{}, workload.ErrConflict
	}
	opID, taskID, confID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	binding := workload.BindingForOperation(p, wid, in.Kind)
	binding.OwnerID = s.ownerID
	binding, err = binding.Normalize()
	if err != nil {
		return workload.RequestResult{}, workload.ErrInvalid
	}
	payload := coretask.WorkloadTaskPayload{WorkloadID: wid, ExpectedWorkloadRevision: expectedWorkloadRevision, PlanID: p.ID, OperationID: opID, PlanRevision: p.Revision, PlanDigest: p.Digest, TargetKind: string(p.TargetKind), ConfirmationID: confID, ExecutionSnapshot: planRaw}
	spec, _ := (coretask.TaskSpec{Kind: coretask.TaskKindWorkload, Payload: coretask.TaskPayload{Workload: &payload}, Goal: "workload " + string(in.Kind), IdempotencyKey: uuid.NewString(), AvailableAt: now}).Normalize()
	specRaw, _ := json.Marshal(spec)
	bindingRaw, _ := json.Marshal(binding)
	if _, err = tx.ExecContext(c, `INSERT INTO core_workloads(workload_id,owner_id,revision,plan_id,plan_digest,target_kind,state,actual_snapshot_json,updated_at) VALUES($1,$2,1,$3,$4,$5,'pending','{}',$6) ON CONFLICT(owner_id,workload_id) DO NOTHING`, wid, s.ownerID, p.ID, p.Digest, p.TargetKind, now); err != nil {
		return workload.RequestResult{}, mapWorkloadError(err)
	}
	// GeoLibre plans carry a persisted, typed provision identity. Bind that
	// immutable owner/provision pair before any task, confirmation or event is
	// visible; the helper performs the workload_id CAS in this transaction.
	if _, err = linkWorkloadDeploymentTx(c, tx, s.ownerID, wid, p); err != nil {
		return workload.RequestResult{}, mapWorkloadError(err)
	}
	if _, err = tx.ExecContext(c, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,attempt,revision,available_at,created_at,updated_at) VALUES($1,$2,$3,'waiting_user',1,1,$4,$4,$4)`, taskID, s.ownerID, specRaw, now); err != nil {
		return workload.RequestResult{}, mapWorkloadError(err)
	}
	if _, err = tx.ExecContext(c, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id,state,revision,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'pending',1,$9,$10,$10)`, confID, s.ownerID, binding.OperationDomain, wid, p.Revision, binding.Digest, bindingRaw, taskID, in.ExpiresAt, now); err != nil {
		return workload.RequestResult{}, mapWorkloadError(err)
	}
	if _, err = tx.ExecContext(c, `INSERT INTO core_workload_operations(operation_id,owner_id,workload_id,expected_workload_revision,plan_id,operation,plan_revision,plan_digest,target_kind,task_id,confirmation_id,status,revision,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,'waiting_user',1,$12,$12)`, opID, s.ownerID, wid, expectedWorkloadRevision, p.ID, in.Kind, p.Revision, p.Digest, p.TargetKind, taskID, confID, now); err != nil {
		return workload.RequestResult{}, mapWorkloadError(err)
	}
	if _, err = tx.ExecContext(c, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2)`, s.ownerID, opID); err != nil {
		return workload.RequestResult{}, mapWorkloadError(err)
	}
	if err = insertWorkloadEventTx(c, tx, s.ownerID, opID, 1, "requested", "waiting_user", "waiting for owner confirmation", nil, now); err != nil {
		return workload.RequestResult{}, mapWorkloadError(err)
	}
	preO := workload.Operation{ID: opID, WorkloadID: wid, ExpectedWorkloadRevision: expectedWorkloadRevision, PlanID: p.ID, Kind: in.Kind, PlanRevision: p.Revision, PlanDigest: p.Digest, TargetKind: p.TargetKind, TaskID: taskID, ConfirmationID: confID, Status: workload.OperationWaitingUser, Revision: 1, CreatedAt: now, UpdatedAt: now, DispatchState: "prepared"}
	preT := coretask.Task{OwnerID: s.ownerID, ID: taskID, Spec: spec, Status: coretask.StatusWaitingUser, Attempt: 1, Revision: 1, CreatedAt: now, UpdatedAt: now, AvailableAt: now}
	// Persist the exact canonical binding in the in-transaction replay
	// response.  A crash after commit must be replay-safe without relying on
	// the best-effort post-commit refresh below; otherwise a retry could return
	// an empty confirmation binding while the durable confirmation row is
	// correctly pinned.
	preC := coreconfirmation.Confirmation{ConfirmationID: confID, OwnerID: s.ownerID, TaskID: taskID, State: coreconfirmation.StatePending, Revision: 1, CreatedAt: now, UpdatedAt: now, ExpiresAt: in.ExpiresAt, Binding: binding}
	preOut, _ := json.Marshal(workload.RequestResult{Operation: preO, Task: preT, Confirmation: preC})
	if _, err = tx.ExecContext(c, `INSERT INTO core_workload_idempotency(owner_id,operation,idempotency_key,request_hash,operation_id,response_json) VALUES($1,$2,$3,$4,$5,$6)`, s.ownerID, string(in.Kind), in.IdempotencyKey, hash, opID, preOut); err != nil {
		return workload.RequestResult{}, mapWorkloadError(err)
	}
	if err = tx.Commit(); err != nil {
		return workload.RequestResult{}, err
	}
	o, err := s.GetOperation(c, opID)
	if err != nil {
		return workload.RequestResult{}, err
	}
	t, err := NewDatabaseTaskStore(s.store.db).Get(c, s.ownerID, taskID)
	if err != nil {
		return workload.RequestResult{}, err
	}
	cc, err := NewDatabaseConfirmationStore(s.store.db).Get(c, confID)
	if err != nil {
		return workload.RequestResult{}, err
	}
	if cc.OwnerID != "" && cc.OwnerID != s.ownerID {
		return workload.RequestResult{}, workload.ErrNotFound
	}
	out := workload.RequestResult{Operation: o, Task: t, Confirmation: cc}
	raw, _ := json.Marshal(out)
	_, _ = s.store.db.ExecContext(c, `UPDATE core_workload_idempotency SET response_json=$1 WHERE owner_id=$2 AND operation=$3 AND idempotency_key=$4`, raw, s.ownerID, string(in.Kind), in.IdempotencyKey)
	return out, nil
}
func (s *PostgresWorkloadStore) CancelOperation(c context.Context, id, key string, rev uint64) (workload.Operation, error) {
	if !workload.ValidUUID(id) || !workload.ValidUUID(key) {
		return workload.Operation{}, workload.ErrInvalid
	}
	hash := workload.CancelInputDigest(id, rev)
	var prior []byte
	var oldHash string
	err := s.store.db.QueryRowContext(c, `SELECT request_hash,response_json FROM core_workload_idempotency WHERE owner_id=$1 AND operation='cancel' AND idempotency_key=$2`, s.ownerID, key).Scan(&oldHash, &prior)
	if err == nil {
		if oldHash != hash {
			return workload.Operation{}, workload.ErrConflict
		}
		var o workload.Operation
		_ = json.Unmarshal(prior, &o)
		return o, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return workload.Operation{}, err
	}
	res, err := s.store.db.ExecContext(c, `UPDATE core_workload_operations SET status='canceled',revision=revision+1,failure_code='user_canceled',failure_summary='operation canceled',updated_at=clock_timestamp() WHERE owner_id=$1 AND operation_id=$2 AND revision=$3 AND status='waiting_user' AND dispatch_state='prepared'`, s.ownerID, id, rev)
	if err != nil {
		return workload.Operation{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	o, err := s.GetOperation(c, id)
	if err != nil {
		return workload.Operation{}, err
	}
	raw, _ := json.Marshal(o)
	_, _ = s.store.db.ExecContext(c, `INSERT INTO core_workload_idempotency(owner_id,operation,idempotency_key,request_hash,operation_id,response_json) VALUES($1,'cancel',$2,$3,$4,$5)`, s.ownerID, key, hash, id, raw)
	return o, nil
}
func (s *PostgresWorkloadStore) Confirm(c context.Context, id string, rev int64) (coreconfirmation.Confirmation, error) {
	o, err := s.GetOperation(c, id)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	p, err := s.GetPlan(c, o.PlanID)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	b := workload.BindingForOperation(p, o.WorkloadID, o.Kind)
	b.OwnerID = s.ownerID
	b, err = b.Normalize()
	if err != nil {
		return coreconfirmation.Confirmation{}, workload.ErrInvalid
	}
	return NewDatabaseConfirmationStore(s.store.db).Confirm(c, coreconfirmation.ConfirmCommand{OwnerID: s.ownerID, ID: o.ConfirmationID, ConfirmationID: o.ConfirmationID, ExpectedRevision: rev, Binding: b, At: time.Now().UTC()})
}
func (s *PostgresWorkloadStore) Consume(c context.Context, id, cid, digest string, rev uint64) (workload.Operation, coretask.Task, error) {
	if !workload.ValidUUID(id) || !workload.ValidUUID(cid) {
		return workload.Operation{}, coretask.Task{}, workload.ErrInvalid
	}
	claim := uuid.NewString()
	until := time.Now().UTC().Add(time.Hour)
	tx, err := s.store.db.BeginTx(c, nil)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	defer tx.Rollback()
	var planExpiresAt time.Time
	if err = tx.QueryRowContext(c, `SELECT p.expires_at FROM core_workload_operations o JOIN core_workload_plans p ON p.owner_id=o.owner_id AND p.plan_id=o.plan_id WHERE o.owner_id=$1 AND o.operation_id=$2 FOR SHARE`, s.ownerID, id).Scan(&planExpiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workload.Operation{}, coretask.Task{}, workload.ErrNotFound
		}
		return workload.Operation{}, coretask.Task{}, err
	}
	now := time.Now().UTC()
	if !planExpiresAt.After(now) {
		return workload.Operation{}, coretask.Task{}, workload.ErrConflict
	}
	var workloadID string
	var expectedWorkloadRevision, operationRevision int64
	var taskID, storedConfirmationID string
	if err = tx.QueryRowContext(c, `SELECT workload_id::text,expected_workload_revision,task_id::text,confirmation_id::text,revision FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2 FOR UPDATE`, s.ownerID, id).Scan(&workloadID, &expectedWorkloadRevision, &taskID, &storedConfirmationID, &operationRevision); err != nil {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if storedConfirmationID != cid || !workload.ValidUUID(taskID) {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	var currentWorkloadRevision int64
	if err = tx.QueryRowContext(c, `SELECT revision FROM core_workloads WHERE owner_id=$1 AND workload_id=$2 FOR UPDATE`, s.ownerID, workloadID).Scan(&currentWorkloadRevision); err != nil {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if expectedWorkloadRevision < 1 || currentWorkloadRevision != expectedWorkloadRevision {
		if err = terminalizeWorkloadRevisionConflictTx(c, tx, s.ownerID, id, cid, taskID, operationRevision, "workload revision changed before dispatch"); err != nil {
			return workload.Operation{}, coretask.Task{}, err
		}
		if err = tx.Commit(); err != nil {
			return workload.Operation{}, coretask.Task{}, err
		}
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	res, err := tx.ExecContext(c, `UPDATE core_workload_operations SET status='running',dispatch_state='dispatched',dispatch_attempt=dispatch_attempt+1,dispatch_epoch=dispatch_epoch+1,dispatch_claim=$1,dispatch_lease_until=$2,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND operation_id=$4 AND confirmation_id=$5 AND plan_digest=$6 AND revision=$7 AND status='waiting_user'`, claim, until, s.ownerID, id, cid, digest, rev)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	confirmationUpdate, err := tx.ExecContext(c, `UPDATE agent_confirmations SET state='consumed',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND confirmation_id=$3 AND state='confirmed' AND expires_at>$1 AND expires_at<=$4`, now, s.ownerID, cid, planExpiresAt)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	if changed, _ := confirmationUpdate.RowsAffected(); changed != 1 {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if _, err = tx.ExecContext(c, `UPDATE agent_tasks SET status='running',revision=revision+1,lease_holder='workload-handler',lease_epoch=lease_epoch+1,lease_expires_at=$1,updated_at=$1 WHERE owner_id=$2 AND task_id=(SELECT task_id FROM core_workload_operations WHERE operation_id=$3)`, until, s.ownerID, id); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	var seq uint64
	if err = tx.QueryRowContext(c, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,operation_id) DO UPDATE SET next_sequence=core_workload_event_counters.next_sequence+1 RETURNING next_sequence-1`, s.ownerID, id).Scan(&seq); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	if err = insertWorkloadEventTx(c, tx, s.ownerID, id, seq, "consumed", "running", "", nil, until); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	if err = tx.Commit(); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	o, err := s.GetOperation(c, id)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	t, err := NewDatabaseTaskStore(s.store.db).Get(c, s.ownerID, o.TaskID)
	return o, t, err
}
func (s *PostgresWorkloadStore) AppendEvent(c context.Context, id string, e workload.Event) (workload.Event, error) {
	if s == nil || s.store == nil || s.store.db == nil || !workload.ValidUUID(id) {
		return workload.Event{}, workload.ErrInvalid
	}
	e.OperationID = id
	seq, err := s.store.AppendWorkloadEventSQL(c, s.ownerID, id, e)
	if err != nil {
		return workload.Event{}, err
	}
	e.Sequence = seq
	return e, nil
}
func (s *PostgresWorkloadStore) CompleteDispatch(c context.Context, id, tid, claim string, epoch uint64, code string, rb workload.Readback, summary string) (workload.Operation, coretask.Task, error) {
	return s.completeDispatch(c, id, tid, claim, epoch, code, rb, summary, nil)
}

// ReconcileUncertain finalizes an already-fenced provider mutation using only
// readback. It deliberately does not touch agent_tasks or confirmations: both
// were terminalized when the unknown side effect was recorded.
func (s *PostgresWorkloadStore) ReconcileUncertain(c context.Context, id, code string, rb workload.Readback, summary string) (workload.Operation, error) {
	if !workload.ValidUUID(id) {
		return workload.Operation{}, workload.ErrInvalid
	}
	now := time.Now().UTC()
	safeCode, safeSummary := workload.SafeFailure(code, summary)
	rb = workload.SanitizeReadback(rb)
	fingerprint := workload.CompletionFingerprint(safeCode, rb)
	raw, _ := json.Marshal(rb)
	tx, err := s.store.db.BeginTx(c, nil)
	if err != nil {
		return workload.Operation{}, err
	}
	defer tx.Rollback()
	var status, dispatchState, workloadID, planID, operation string
	var currentFingerprint string
	if err = tx.QueryRowContext(c, `SELECT status,dispatch_state,workload_id::text,plan_id::text,operation,completion_fingerprint FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2 FOR UPDATE`, s.ownerID, id).Scan(&status, &dispatchState, &workloadID, &planID, &operation, &currentFingerprint); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return workload.Operation{}, workload.ErrNotFound
		}
		return workload.Operation{}, err
	}
	if status != "uncertain" || dispatchState != "uncertain" {
		if err = tx.Commit(); err != nil {
			return workload.Operation{}, err
		}
		return s.loadOperation(c, id)
	}
	if currentFingerprint != "" && currentFingerprint == fingerprint {
		if err = tx.Commit(); err != nil {
			return workload.Operation{}, err
		}
		return s.loadOperation(c, id)
	}
	if leaseRows, queryErr := tx.QueryContext(c, `SELECT l.provision_id::text,l.token::text,l.epoch,p.plan_id::text FROM core_aws_ec2_provision_mutation_leases l JOIN core_aws_ec2_provisions p ON p.owner_id=l.owner_id AND p.provision_id=l.provision_id WHERE l.owner_id=$1 AND l.operation_id=$2 FOR UPDATE OF l`, s.ownerID, id); queryErr == nil {
		var boundLease struct {
			provisionID, token, planID string
			epoch                      int64
		}
		leaseCount := 0
		for leaseRows.Next() {
			leaseCount++
			if leaseCount > 1 {
				_ = leaseRows.Close()
				return workload.Operation{}, workload.ErrRevisionConflict
			}
			var provisionID, token string
			var epoch int64
			var leasePlanID string
			if err = leaseRows.Scan(&provisionID, &token, &epoch, &leasePlanID); err != nil {
				_ = leaseRows.Close()
				return workload.Operation{}, err
			}
			boundLease.provisionID, boundLease.token, boundLease.epoch, boundLease.planID = provisionID, token, epoch, leasePlanID
		}
		if err = leaseRows.Err(); err != nil {
			_ = leaseRows.Close()
			return workload.Operation{}, err
		}
		if err = leaseRows.Close(); err != nil {
			return workload.Operation{}, err
		}
		if leaseCount == 1 {
			if boundLease.planID != planID {
				return workload.Operation{}, workload.ErrRevisionConflict
			}
			if safeCode != "provider_uncertain" {
				if _, err = tx.ExecContext(c, `UPDATE core_aws_ec2_provision_mutation_leases SET token=NULL,expires_at=NULL,state='active',operation_id=NULL,updated_at=$1 WHERE owner_id=$2 AND provision_id=$3 AND operation_id=$4 AND epoch=$5`, now, s.ownerID, boundLease.provisionID, id, boundLease.epoch); err != nil {
					return workload.Operation{}, err
				}
			}
		}
	} else {
		return workload.Operation{}, queryErr
	}
	newStatus, newDispatch := "uncertain", "uncertain"
	if safeCode == "" {
		newStatus, newDispatch = "succeeded", "terminal"
	} else if safeCode != "provider_uncertain" {
		newStatus, newDispatch = "failed", "terminal"
	}
	if _, err = tx.ExecContext(c, `UPDATE core_workload_operations SET status=$1,dispatch_state=$2,failure_code=$3,failure_summary=$4,completion_fingerprint=$5,completion_result_json=$6,dispatch_lease_until=NULL,revision=revision+1,updated_at=$7 WHERE owner_id=$8 AND operation_id=$9 AND status='uncertain' AND dispatch_state='uncertain'`, newStatus, newDispatch, safeCode, safeSummary, fingerprint, raw, now, s.ownerID, id); err != nil {
		return workload.Operation{}, err
	}
	workloadState := "uncertain"
	if safeCode == "" {
		workloadState = "ready"
		if operation == string(workload.OperationDestroy) {
			workloadState = "destroyed"
		}
	} else if safeCode != "provider_uncertain" {
		workloadState = "failed"
	}
	if _, err = tx.ExecContext(c, `UPDATE core_workloads SET state=$1,actual_snapshot_json=$2,revision=revision+1,updated_at=$3 WHERE owner_id=$4 AND workload_id=$5`, workloadState, raw, now, s.ownerID, workloadID); err != nil {
		return workload.Operation{}, err
	}
	if event, ok := workload.ProviderResultEventFromContext(c); ok {
		var seq uint64
		if err = tx.QueryRowContext(c, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,operation_id) DO UPDATE SET next_sequence=core_workload_event_counters.next_sequence+1 RETURNING next_sequence-1`, s.ownerID, id).Scan(&seq); err != nil {
			return workload.Operation{}, err
		}
		if err = insertWorkloadEventTx(c, tx, s.ownerID, id, seq, event.Kind, string(event.Status), workload.SafeProviderResultEventMessage(event), event.Readback, now); err != nil {
			return workload.Operation{}, err
		}
	}
	var seq uint64
	if err = tx.QueryRowContext(c, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,operation_id) DO UPDATE SET next_sequence=core_workload_event_counters.next_sequence+1 RETURNING next_sequence-1`, s.ownerID, id).Scan(&seq); err != nil {
		return workload.Operation{}, err
	}
	if err = insertWorkloadEventTx(c, tx, s.ownerID, id, seq, "reconciled", newStatus, safeSummary, raw, now); err != nil {
		return workload.Operation{}, err
	}
	if err = tx.Commit(); err != nil {
		return workload.Operation{}, err
	}
	return s.loadOperation(c, id)
}

func (s *PostgresWorkloadStore) completeDispatch(c context.Context, id, tid, claim string, epoch uint64, code string, rb workload.Readback, summary string, fence *workload.TaskFence) (workload.Operation, coretask.Task, error) {
	if !workload.ValidUUID(id) || !workload.ValidUUID(tid) || claim == "" {
		return workload.Operation{}, coretask.Task{}, workload.ErrInvalid
	}
	now := time.Now().UTC()
	status := "succeeded"
	if code != "" {
		status = "failed"
	}
	safeCode, safeSummary := workload.SafeFailure(code, summary)
	if safeCode == "provider_uncertain" {
		status = "uncertain"
	}
	dispatchState := "completed"
	if status == "uncertain" {
		dispatchState = "uncertain"
	}
	rb = workload.SanitizeReadback(rb)
	fingerprint := workload.CompletionFingerprint(safeCode, rb)
	raw, _ := json.Marshal(rb)
	tx, err := s.store.db.BeginTx(c, nil)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	defer tx.Rollback()

	var taskRevision int64
	var taskAttempt int
	var taskEpoch int64
	var taskHolder string
	var taskExpiry *time.Time
	if err = tx.QueryRowContext(c, `SELECT revision,attempt,lease_epoch,lease_holder,lease_expires_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 AND status='running' FOR UPDATE`, s.ownerID, tid).Scan(&taskRevision, &taskAttempt, &taskEpoch, &taskHolder, &taskExpiry); err != nil {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if taskExpiry == nil || !taskExpiry.After(now) || uint64(taskEpoch) != epoch {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if fence != nil {
		if fence.TaskID != tid || fence.Attempt != uint32(taskAttempt) || fence.LeaseEpoch != uint64(taskEpoch) || fence.Holder != taskHolder || uint64(taskRevision) < fence.Revision {
			return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
		}
	}
	if mutationFence, ok := workload.MutationLeaseFenceFromContext(c); ok {
		if mutationFence.OwnerID != s.ownerID || mutationFence.Epoch < 1 {
			return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
		}
		var leaseToken string
		var leaseEpoch int64
		var leaseExpiry time.Time
		if err = tx.QueryRowContext(c, `SELECT token::text,epoch,expires_at FROM core_aws_ec2_provision_mutation_leases WHERE owner_id=$1 AND provision_id=$2 FOR UPDATE`, mutationFence.OwnerID, mutationFence.ProvisionID).Scan(&leaseToken, &leaseEpoch, &leaseExpiry); err != nil {
			return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
		}
		if leaseToken != mutationFence.Token || leaseEpoch != mutationFence.Epoch || !leaseExpiry.After(now) {
			return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
		}
		if mutationFence.OperationID == "" || mutationFence.OperationID != id {
			return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
		}
		if safeCode == "provider_uncertain" {
			if res, updateErr := tx.ExecContext(c, `UPDATE core_aws_ec2_provision_mutation_leases SET state='uncertain',operation_id=$1,expires_at=$2,updated_at=$3 WHERE owner_id=$4 AND provision_id=$5 AND token=$6 AND epoch=$7`, mutationFence.OperationID, now.Add(24*time.Hour), now, mutationFence.OwnerID, mutationFence.ProvisionID, mutationFence.Token, mutationFence.Epoch); updateErr != nil {
				return workload.Operation{}, coretask.Task{}, updateErr
			} else if changed, _ := res.RowsAffected(); changed != 1 {
				return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
			}
		} else {
			if res, updateErr := tx.ExecContext(c, `UPDATE core_aws_ec2_provision_mutation_leases SET token=NULL,expires_at=NULL,state='active',operation_id=NULL,updated_at=$1 WHERE owner_id=$2 AND provision_id=$3 AND token=$4 AND epoch=$5 AND state IN ('active','uncertain')`, now, mutationFence.OwnerID, mutationFence.ProvisionID, mutationFence.Token, mutationFence.Epoch); updateErr != nil {
				return workload.Operation{}, coretask.Task{}, updateErr
			} else if changed, _ := res.RowsAffected(); changed != 1 {
				return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
			}
		}
	}
	if resultEvent, ok := workload.ProviderResultEventFromContext(c); ok {
		var eventSeq uint64
		if err = tx.QueryRowContext(c, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,operation_id) DO UPDATE SET next_sequence=core_workload_event_counters.next_sequence+1 RETURNING next_sequence-1`, s.ownerID, id).Scan(&eventSeq); err != nil {
			return workload.Operation{}, coretask.Task{}, err
		}
		if err = insertWorkloadEventTx(c, tx, s.ownerID, id, eventSeq, resultEvent.Kind, string(resultEvent.Status), workload.SafeProviderResultEventMessage(resultEvent), resultEvent.Readback, now); err != nil {
			return workload.Operation{}, coretask.Task{}, err
		}
	}

	res, err := tx.ExecContext(c, `UPDATE core_workload_operations SET status=$1,revision=revision+1,failure_code=$2,failure_summary=$3,completion_fingerprint=$4,completion_result_json=$5,dispatch_state=$6,updated_at=$7 WHERE owner_id=$8 AND operation_id=$9 AND task_id=$10 AND dispatch_claim=$11 AND dispatch_epoch=$12 AND status='running'`, status, safeCode, safeSummary, fingerprint, raw, dispatchState, now, s.ownerID, id, tid, claim, epoch)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	taskStatus := coretask.StatusSucceeded
	if status == "failed" || status == "uncertain" {
		taskStatus = coretask.StatusFailed
	}
	var taskSeq uint64
	err = tx.QueryRowContext(c, `UPDATE agent_tasks SET status=$1,revision=revision+1,result_json=$2,failure_code=$3,failure_summary=$4,progress_sequence=progress_sequence+1,updated_at=$5,lease_expires_at=NULL,lease_holder='' WHERE owner_id=$6 AND task_id=$7 AND status='running' AND revision=$8 AND attempt=$9 AND lease_epoch=$10 AND lease_holder=$11 AND lease_expires_at>$5 RETURNING progress_sequence`, taskStatus, raw, safeCode, safeSummary, now, s.ownerID, tid, taskRevision, taskAttempt, taskEpoch, taskHolder).Scan(&taskSeq)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	if _, err = tx.ExecContext(c, `INSERT INTO agent_task_events(owner_id,task_id,sequence,event_type,status,payload_json,occurred_at) VALUES($1,$2,$3,'workload_completed',$4,$5,$6)`, s.ownerID, tid, taskSeq, taskStatus, raw, now); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	var seq uint64
	if err = tx.QueryRowContext(c, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,operation_id) DO UPDATE SET next_sequence=core_workload_event_counters.next_sequence+1 RETURNING next_sequence-1`, s.ownerID, id).Scan(&seq); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	if err = insertWorkloadEventTx(c, tx, s.ownerID, id, seq, "completed", status, safeSummary, raw, now); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	if _, err = tx.ExecContext(c, `UPDATE agent_task_runtime_concurrency SET running_count=GREATEST(0,running_count-1),revision=revision+1,updated_at=$1 WHERE singleton=true AND running_count>0`, now); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	if status == "succeeded" {
		state := rb.State
		if state == "" {
			state = "ready"
		}
		if rb.State == "destroyed" {
			state = "destroyed"
		}
		if _, err = tx.ExecContext(c, `UPDATE core_workloads SET state=$1,actual_snapshot_json=$2,revision=revision+1,updated_at=$3 WHERE owner_id=$4 AND workload_id=(SELECT workload_id FROM core_workload_operations WHERE owner_id=$4 AND operation_id=$5)`, state, raw, now, s.ownerID, id); err != nil {
			return workload.Operation{}, coretask.Task{}, err
		}
	} else if status == "uncertain" {
		if _, err = tx.ExecContext(c, `UPDATE core_workloads SET state='uncertain',actual_snapshot_json=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND workload_id=(SELECT workload_id FROM core_workload_operations WHERE owner_id=$3 AND operation_id=$4)`, raw, now, s.ownerID, id); err != nil {
			return workload.Operation{}, coretask.Task{}, err
		}
	}
	var confirmationID, confirmationState string
	var confirmationRevision int64
	var reservationRaw []byte
	if err = tx.QueryRowContext(c, `SELECT confirmation_id::text,state,revision,reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=(SELECT confirmation_id FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2) FOR UPDATE`, s.ownerID, id).Scan(&confirmationID, &confirmationState, &confirmationRevision, &reservationRaw); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	var reservation struct {
		TaskID   string `json:"task_id"`
		Attempt  uint32 `json:"attempt"`
		Epoch    uint64 `json:"lease_epoch"`
		Revision uint64 `json:"task_revision"`
		Active   bool   `json:"active"`
	}
	if confirmationState != "consumed" || json.Unmarshal(reservationRaw, &reservation) != nil || !reservation.Active || reservation.TaskID != tid || reservation.Attempt != uint32(taskAttempt) || reservation.Epoch != uint64(taskEpoch) {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	// Unknown provider outcomes keep the target fenced for reconciliation;
	// proven success/failure releases it atomically with the terminal updates.
	confirmationReservationSQL := `UPDATE agent_confirmations SET reservation_json=NULL,revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND confirmation_id=$3 AND revision=$4 AND state='consumed'`
	if status == "uncertain" {
		confirmationReservationSQL = `UPDATE agent_confirmations SET reservation_json=jsonb_set(reservation_json,'{active}','false'),revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND confirmation_id=$3 AND revision=$4 AND state='consumed'`
	}
	res, err = tx.ExecContext(c, confirmationReservationSQL, now, s.ownerID, confirmationID, confirmationRevision)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	if n, _ = res.RowsAffected(); n != 1 {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if err = tx.Commit(); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	o, err := s.GetOperation(c, id)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	t, err := NewDatabaseTaskStore(s.store.db).Get(c, s.ownerID, tid)
	return o, t, err
}
func (s *PostgresWorkloadStore) RenewDispatchLease(c context.Context, id, claim string, epoch uint64) (workload.Operation, error) {
	if !workload.ValidUUID(id) || claim == "" {
		return workload.Operation{}, workload.ErrInvalid
	}
	until := time.Now().UTC().Add(30 * time.Second)
	res, err := s.store.db.ExecContext(c, `UPDATE core_workload_operations SET dispatch_lease_until=$1,revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND operation_id=$3 AND dispatch_claim=$4 AND dispatch_epoch=$5 AND status='running' AND dispatch_lease_until>$6`, until, s.ownerID, id, claim, epoch, time.Now().UTC())
	if err != nil {
		return workload.Operation{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	return s.GetOperation(c, id)
}
func (s *PostgresWorkloadStore) RecoverClaim(c context.Context, id, claim string) (workload.Operation, error) {
	if !workload.ValidUUID(id) {
		return workload.Operation{}, workload.ErrInvalid
	}
	newClaim := claim
	if newClaim == "" {
		newClaim = uuid.NewString()
	}
	until := time.Now().UTC().Add(30 * time.Second)
	res, err := s.store.db.ExecContext(c, `UPDATE core_workload_operations SET dispatch_state='uncertain',dispatch_claim=$1,dispatch_epoch=dispatch_epoch+1,dispatch_lease_until=$2,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND operation_id=$4 AND status='running' AND dispatch_state IN ('dispatched','uncertain')`, newClaim, until, s.ownerID, id)
	if err != nil {
		return workload.Operation{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return s.GetOperation(c, id)
	}
	return s.GetOperation(c, id)
}

// Fenced variants require the generic worker lease; they never mint a second
// workload-local lease. The durable SQL implementation currently shares the
// same CAS predicates as the legacy methods and rejects an absent fence.
func (s *PostgresWorkloadStore) ConsumeFenced(c context.Context, id, cid, digest string, rev uint64, fence workload.TaskFence) (workload.Operation, coretask.Task, error) {
	now := time.Now().UTC()
	if !fence.Valid(time.Time{}) {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	tx, err := s.store.db.BeginTx(c, nil)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	defer tx.Rollback()
	var taskStatus, taskHolder string
	var taskRevision, taskEpoch int64
	var taskAttempt int
	var taskExpiry *time.Time
	if err = tx.QueryRowContext(c, `SELECT status,revision,attempt,lease_epoch,lease_holder,lease_expires_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, s.ownerID, fence.TaskID).Scan(&taskStatus, &taskRevision, &taskAttempt, &taskEpoch, &taskHolder, &taskExpiry); err != nil {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if taskStatus != "running" || taskRevision < int64(fence.Revision) || taskAttempt != int(fence.Attempt) || taskEpoch != int64(fence.LeaseEpoch) || taskHolder != fence.Holder || taskExpiry == nil || !taskExpiry.After(now) {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	var planExpiresAt time.Time
	if err = tx.QueryRowContext(c, `SELECT p.expires_at FROM core_workload_operations o JOIN core_workload_plans p ON p.owner_id=o.owner_id AND p.plan_id=o.plan_id WHERE o.owner_id=$1 AND o.operation_id=$2 FOR SHARE`, s.ownerID, id).Scan(&planExpiresAt); err != nil {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if !planExpiresAt.After(now) {
		return workload.Operation{}, coretask.Task{}, workload.ErrConflict
	}
	var workloadID string
	var expectedWorkloadRevision, operationRevision int64
	var operationTaskID, storedConfirmationID string
	if err = tx.QueryRowContext(c, `SELECT workload_id::text,expected_workload_revision,task_id::text,confirmation_id::text,revision FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2 FOR UPDATE`, s.ownerID, id).Scan(&workloadID, &expectedWorkloadRevision, &operationTaskID, &storedConfirmationID, &operationRevision); err != nil {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if operationTaskID != fence.TaskID || storedConfirmationID != cid {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	var currentWorkloadRevision int64
	if err = tx.QueryRowContext(c, `SELECT revision FROM core_workloads WHERE owner_id=$1 AND workload_id=$2 FOR UPDATE`, s.ownerID, workloadID).Scan(&currentWorkloadRevision); err != nil {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if expectedWorkloadRevision < 1 || currentWorkloadRevision != expectedWorkloadRevision {
		if err = terminalizeWorkloadRevisionConflictTx(c, tx, s.ownerID, id, cid, operationTaskID, operationRevision, "workload revision changed before dispatch"); err != nil {
			return workload.Operation{}, coretask.Task{}, err
		}
		if err = tx.Commit(); err != nil {
			return workload.Operation{}, coretask.Task{}, err
		}
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	dispatchClaim := uuid.NewString()
	res, err := tx.ExecContext(c, `UPDATE core_workload_operations SET status='running',dispatch_state='dispatched',dispatch_attempt=dispatch_attempt+1,dispatch_epoch=$1,dispatch_claim=$2,dispatch_lease_until=$3,revision=revision+1,updated_at=$4 WHERE owner_id=$5 AND operation_id=$6 AND task_id=$7 AND confirmation_id=$8 AND plan_digest=$9 AND revision=$10 AND status='waiting_user' AND dispatch_state='prepared' AND dispatch_claim IS NULL`, fence.LeaseEpoch, dispatchClaim, taskExpiry.UTC(), now, s.ownerID, id, fence.TaskID, cid, digest, rev)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	var confRev int64
	if err = tx.QueryRowContext(c, `SELECT revision FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2 FOR UPDATE`, s.ownerID, cid).Scan(&confRev); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	res, err = tx.ExecContext(c, `UPDATE agent_confirmations SET state='consumed',revision=revision+1,reservation_json=jsonb_build_object('task_id',$1::uuid,'attempt',$2::integer,'lease_epoch',$3::bigint,'task_revision',$4::bigint,'active',true),updated_at=$5 WHERE owner_id=$6 AND confirmation_id=$7 AND task_id=$1 AND state='confirmed' AND revision=$8 AND expires_at>$5 AND expires_at<=$9`, fence.TaskID, fence.Attempt, fence.LeaseEpoch, taskRevision, now, s.ownerID, cid, confRev, planExpiresAt)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	n, _ = res.RowsAffected()
	if n != 1 {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	if err = tx.Commit(); err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	o, err := s.GetOperation(c, id)
	if err != nil {
		return workload.Operation{}, coretask.Task{}, err
	}
	t, err := NewDatabaseTaskStore(s.store.db).Get(c, s.ownerID, o.TaskID)
	return o, t, err
}

func terminalizeWorkloadRevisionConflictTx(ctx context.Context, tx *sql.Tx, ownerID, operationID, confirmationID, taskID string, operationRevision int64, summary string) error {
	const code = "workload_revision_conflict"
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE core_workload_operations SET status='failed',dispatch_state='terminal',failure_code=$1,failure_summary=$2,revision=revision+1,updated_at=$3 WHERE owner_id=$4 AND operation_id=$5 AND confirmation_id=$6 AND task_id=$7 AND revision=$8 AND status='waiting_user' AND dispatch_state='prepared'`, code, summary, now, ownerID, operationID, confirmationID, taskID, operationRevision)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return workload.ErrRevisionConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='rejected',terminal_reason=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND confirmation_id=$4 AND task_id=$5 AND state='confirmed'`, code, now, ownerID, confirmationID, taskID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return workload.ErrRevisionConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='canceled',failure_code=$1,failure_summary=$2,revision=revision+1,updated_at=$3 WHERE owner_id=$4 AND task_id=$5 AND task_id=(SELECT task_id FROM core_workload_operations WHERE owner_id=$4 AND operation_id=$6) AND status='waiting_user'`, code, summary, now, ownerID, taskID, operationID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return workload.ErrRevisionConflict
	}
	var sequence uint64
	if err = tx.QueryRowContext(ctx, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,operation_id) DO UPDATE SET next_sequence=core_workload_event_counters.next_sequence+1 RETURNING next_sequence-1`, ownerID, operationID).Scan(&sequence); err != nil {
		return err
	}
	return insertWorkloadEventTx(ctx, tx, ownerID, operationID, sequence, "terminal", "failed", summary, nil, now)
}

func (s *PostgresWorkloadStore) CompleteDispatchFenced(c context.Context, id, tid, claim string, epoch uint64, code string, rb workload.Readback, summary string, fence workload.TaskFence) (workload.Operation, coretask.Task, error) {
	if !fence.Valid(time.Time{}) || fence.TaskID != tid || fence.LeaseEpoch != epoch {
		return workload.Operation{}, coretask.Task{}, workload.ErrRevisionConflict
	}
	return s.completeDispatch(c, id, tid, claim, epoch, code, rb, summary, &fence)
}
func (s *PostgresWorkloadStore) RenewDispatchLeaseFenced(c context.Context, id, claim string, epoch uint64, fence workload.TaskFence) (workload.Operation, error) {
	if !workload.ValidUUID(id) || strings.TrimSpace(claim) == "" || !fence.Valid(time.Time{}) || fence.LeaseEpoch != epoch {
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	now := time.Now().UTC()
	tx, err := s.store.db.BeginTx(c, nil)
	if err != nil {
		return workload.Operation{}, err
	}
	defer tx.Rollback()
	var st, holder string
	var rev, ep int64
	var att int
	var expires *time.Time
	if err = tx.QueryRowContext(c, `SELECT status,revision,lease_epoch,attempt,lease_holder,lease_expires_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, s.ownerID, fence.TaskID).Scan(&st, &rev, &ep, &att, &holder, &expires); err != nil || st != "running" || uint64(rev) < fence.Revision || uint64(ep) != fence.LeaseEpoch || uint32(att) != fence.Attempt || holder != fence.Holder || expires == nil || !expires.After(now) {
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	res, err := tx.ExecContext(c, `UPDATE core_workload_operations SET dispatch_lease_until=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND operation_id=$4 AND task_id=$5 AND dispatch_claim=$6 AND dispatch_epoch=$7 AND status='running' AND dispatch_state IN ('dispatched','uncertain')`, expires.UTC(), now, s.ownerID, id, fence.TaskID, claim, epoch)
	if err != nil {
		return workload.Operation{}, err
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return workload.Operation{}, rowsErr
		}
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	if err = tx.Commit(); err != nil {
		return workload.Operation{}, err
	}
	return s.GetOperation(c, id)
}
func (s *PostgresWorkloadStore) RecoverClaimFenced(c context.Context, id, claim string, fence workload.TaskFence) (workload.Operation, error) {
	now := time.Now().UTC()
	if !fence.Valid(time.Time{}) {
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	tx, err := s.store.db.BeginTx(c, nil)
	if err != nil {
		return workload.Operation{}, err
	}
	defer tx.Rollback()
	var opStatus, dispatchState, taskID, confID string
	var opRev, opEpoch int64
	var oldClaim sql.NullString
	var oldLease sql.NullTime
	if err = tx.QueryRowContext(c, `SELECT status,dispatch_state,task_id::text,confirmation_id::text,revision,dispatch_epoch,dispatch_claim,dispatch_lease_until FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2 FOR UPDATE`, s.ownerID, id).Scan(&opStatus, &dispatchState, &taskID, &confID, &opRev, &opEpoch, &oldClaim, &oldLease); err != nil {
		return workload.Operation{}, workload.ErrNotFound
	}
	if claim != "" && oldClaim.Valid && claim == oldClaim.String && dispatchState == "uncertain" && oldLease.Valid && oldLease.Time.After(now) {
		if err = tx.Commit(); err != nil {
			return workload.Operation{}, err
		}
		return s.GetOperation(c, id)
	}
	if opStatus != "running" || (dispatchState != "dispatched" && dispatchState != "uncertain") || taskID != fence.TaskID ||
		opEpoch >= int64(fence.LeaseEpoch) || !oldClaim.Valid || strings.TrimSpace(oldClaim.String) == "" {
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	var taskStatus, holder string
	var taskRev, taskEpoch int64
	var taskAttempt int
	var taskExp *time.Time
	if err = tx.QueryRowContext(c, `SELECT status,revision,attempt,lease_epoch,lease_holder,lease_expires_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, s.ownerID, taskID).Scan(&taskStatus, &taskRev, &taskAttempt, &taskEpoch, &holder, &taskExp); err != nil || taskStatus != "running" || uint64(taskRev) < fence.Revision || uint32(taskAttempt) != fence.Attempt || uint64(taskEpoch) != fence.LeaseEpoch || holder != fence.Holder || taskExp == nil || !taskExp.After(now) {
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	var confState string
	var confRev int64
	var reservation []byte
	if err = tx.QueryRowContext(c, `SELECT state,revision,reservation_json FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2 AND task_id=$3 FOR UPDATE`, s.ownerID, confID, taskID).Scan(&confState, &confRev, &reservation); err != nil || confState != "consumed" {
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	var old struct {
		TaskID       string `json:"task_id"`
		Attempt      uint32 `json:"attempt"`
		LeaseEpoch   uint64 `json:"lease_epoch"`
		TaskRevision uint64 `json:"task_revision"`
		Active       bool   `json:"active"`
	}
	if json.Unmarshal(reservation, &old) != nil || !old.Active || old.TaskID != taskID ||
		old.Attempt != uint32(taskAttempt) || old.LeaseEpoch >= uint64(taskEpoch) ||
		old.TaskRevision >= uint64(taskRev) {
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	newClaim := claim
	if newClaim == "" {
		newClaim = uuid.NewString()
	}
	res, err := tx.ExecContext(c, `UPDATE core_workload_operations SET dispatch_state='uncertain',dispatch_claim=$1,dispatch_epoch=$2,dispatch_lease_until=$3,revision=revision+1,updated_at=$4 WHERE owner_id=$5 AND operation_id=$6 AND revision=$7 AND dispatch_epoch=$8`, newClaim, taskEpoch, taskExp, now, s.ownerID, id, opRev, opEpoch)
	if err != nil {
		return workload.Operation{}, err
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return workload.Operation{}, rowsErr
		}
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	newRes, err := json.Marshal(struct {
		TaskID       string `json:"task_id"`
		Attempt      uint32 `json:"attempt"`
		LeaseEpoch   uint64 `json:"lease_epoch"`
		TaskRevision uint64 `json:"task_revision"`
		Active       bool   `json:"active"`
	}{
		TaskID:       taskID,
		Attempt:      uint32(taskAttempt),
		LeaseEpoch:   uint64(taskEpoch),
		TaskRevision: uint64(taskRev),
		Active:       true,
	})
	if err != nil {
		return workload.Operation{}, err
	}
	res, err = tx.ExecContext(c, `UPDATE agent_confirmations SET reservation_json=$1,revision=revision+1,updated_at=$2 WHERE owner_id=$3 AND confirmation_id=$4 AND revision=$5 AND state='consumed'`, newRes, now, s.ownerID, confID, confRev)
	if err != nil {
		return workload.Operation{}, err
	}
	if n, rowsErr := res.RowsAffected(); rowsErr != nil || n != 1 {
		if rowsErr != nil {
			return workload.Operation{}, rowsErr
		}
		return workload.Operation{}, workload.ErrRevisionConflict
	}
	var eventSequence uint64
	if err = tx.QueryRowContext(c, `INSERT INTO core_workload_event_counters(owner_id,operation_id,next_sequence) VALUES($1,$2,2) ON CONFLICT(owner_id,operation_id) DO UPDATE SET next_sequence=core_workload_event_counters.next_sequence+1 RETURNING next_sequence-1`, s.ownerID, id).Scan(&eventSequence); err != nil {
		return workload.Operation{}, err
	}
	if err = insertWorkloadEventTx(c, tx, s.ownerID, id, eventSequence, "recovery_claim", "running", "read-only recovery claimed dispatch fence", nil, now); err != nil {
		return workload.Operation{}, err
	}
	if err = tx.Commit(); err != nil {
		return workload.Operation{}, err
	}
	return s.GetOperation(c, id)
}

// WorkloadLeaseResolver is the narrow task/confirmation integration seam.
// Implementations must return the already-issued lease; handlers never mint a
// second lease or retry an uncertain provider request blindly.
type WorkloadLeaseResolver interface {
	Claim(context.Context, string, uint64) (workload.TaskFence, error)
	Confirm(context.Context, string, int64) error
}

func (s *PostgresWorkloadStore) QuotePlan(ctx context.Context, planID string, expiresAt time.Time) (workload.Quote, error) {
	if s == nil || s.store == nil || s.store.db == nil || !workload.ValidUUID(planID) {
		return workload.Quote{}, workload.ErrInvalid
	}
	p, err := s.GetPlan(ctx, planID)
	if err != nil {
		return workload.Quote{}, err
	}
	if expiresAt.IsZero() {
		expiresAt = time.Now().UTC().Add(15 * time.Minute)
	}
	if !expiresAt.After(time.Now().UTC()) {
		return workload.Quote{}, workload.ErrInvalid
	}
	q := workload.Quote{ID: uuid.NewString(), PlanID: p.ID, PlanDigest: p.Digest, Summary: p.Summary, ExpiresAt: expiresAt.UTC(), CreatedAt: time.Now().UTC()}
	_, err = s.store.db.ExecContext(ctx, `INSERT INTO core_workload_quotes(quote_id,owner_id,plan_id,plan_digest,summary,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, q.ID, s.ownerID, q.PlanID, q.PlanDigest, q.Summary, q.ExpiresAt, q.CreatedAt)
	if err != nil {
		return workload.Quote{}, mapWorkloadError(err)
	}
	return q, nil
}

func (s *PostgresWorkloadStore) GetQuote(ctx context.Context, quoteID string) (workload.Quote, error) {
	var q workload.Quote
	if s == nil || s.store == nil || s.store.db == nil || !workload.ValidUUID(quoteID) {
		return q, workload.ErrInvalid
	}
	err := s.store.db.QueryRowContext(ctx, `SELECT quote_id::text,plan_id::text,plan_digest,summary,expires_at,created_at FROM core_workload_quotes WHERE owner_id=$1 AND quote_id=$2`, s.ownerID, quoteID).Scan(&q.ID, &q.PlanID, &q.PlanDigest, &q.Summary, &q.ExpiresAt, &q.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return q, workload.ErrNotFound
	}
	return q, err
}

// AgentWorkloadDDL is the exact PostgreSQL schema required by AWS SSM/ECS
// plans and operations. Shared task/confirmation tables are intentionally
// referenced, not recreated here.
const AgentWorkloadDDL = `CREATE TABLE IF NOT EXISTS core_workload_plans (
 plan_id uuid PRIMARY KEY, owner_id text NOT NULL, create_idempotency_key uuid NOT NULL, UNIQUE(owner_id,plan_id),
 create_request_hash text NOT NULL CHECK (create_request_hash ~ '^[a-f0-9]{64}$'), revision bigint NOT NULL DEFAULT 1 CHECK (revision > 0),
 digest text NOT NULL CHECK (digest ~ '^[a-f0-9]{64}$'), summary text NOT NULL CHECK (length(summary) BETWEEN 1 AND 4096),
 plan_json jsonb NOT NULL CHECK (jsonb_typeof(plan_json)='object' AND pg_column_size(plan_json) <= 1048576),
 target_kind text NOT NULL CHECK (target_kind IN ('AWS_EC2_SSM','AWS_ECS')),
 target_identity_json jsonb NOT NULL DEFAULT '{}'::jsonb, resource_limits_json jsonb NOT NULL DEFAULT '{}'::jsonb,
 secret_grant_refs_json jsonb NOT NULL DEFAULT '[]'::jsonb, expires_at timestamptz NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE UNIQUE INDEX IF NOT EXISTS core_workload_plans_digest_idx ON core_workload_plans(owner_id,digest);
CREATE UNIQUE INDEX IF NOT EXISTS core_workload_plans_create_idempotency_idx ON core_workload_plans(owner_id,create_idempotency_key);
CREATE TABLE IF NOT EXISTS core_workload_quotes (quote_id uuid PRIMARY KEY, owner_id text NOT NULL, plan_id uuid NOT NULL,
 plan_digest text NOT NULL CHECK (plan_digest ~ '^[a-f0-9]{64}$'), summary text NOT NULL, expires_at timestamptz NOT NULL,
 created_at timestamptz NOT NULL DEFAULT clock_timestamp(), FOREIGN KEY(owner_id,plan_id) REFERENCES core_workload_plans(owner_id,plan_id) ON DELETE RESTRICT);
CREATE TABLE IF NOT EXISTS core_workloads (workload_id uuid NOT NULL, owner_id text NOT NULL, revision bigint NOT NULL DEFAULT 1,
 plan_id uuid NOT NULL, plan_digest text NOT NULL,
 target_kind text NOT NULL CHECK (target_kind IN ('AWS_EC2_SSM','AWS_ECS')), actual_snapshot_json jsonb NOT NULL DEFAULT '{}'::jsonb,
 state text NOT NULL CHECK (state IN ('pending','ready','failed','destroyed','uncertain')), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 PRIMARY KEY(owner_id,workload_id), FOREIGN KEY(owner_id,plan_id) REFERENCES core_workload_plans(owner_id,plan_id) ON DELETE RESTRICT);
CREATE TABLE IF NOT EXISTS core_workload_operations (operation_id uuid PRIMARY KEY, owner_id text NOT NULL, workload_id uuid NOT NULL, expected_workload_revision bigint NOT NULL DEFAULT 1 CHECK (expected_workload_revision > 0),
 plan_id uuid NOT NULL, operation text NOT NULL CHECK (operation IN ('apply','destroy')),
 plan_revision bigint NOT NULL CHECK (plan_revision > 0), plan_digest text NOT NULL, target_kind text NOT NULL CHECK (target_kind IN ('AWS_EC2_SSM','AWS_ECS')),
 task_id uuid NOT NULL, confirmation_id uuid NOT NULL, status text NOT NULL, revision bigint NOT NULL DEFAULT 1,
 failure_code text NOT NULL DEFAULT '', failure_summary text NOT NULL DEFAULT '', created_at timestamptz NOT NULL DEFAULT clock_timestamp(), updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
 dispatch_state text NOT NULL DEFAULT 'prepared', dispatch_attempt integer NOT NULL DEFAULT 0, dispatch_epoch bigint NOT NULL DEFAULT 0,
 dispatch_claim uuid, dispatch_lease_until timestamptz, dispatch_error text NOT NULL DEFAULT '', completion_fingerprint text NOT NULL DEFAULT '', completion_result_json jsonb,
 FOREIGN KEY(owner_id,plan_id) REFERENCES core_workload_plans(owner_id,plan_id) ON DELETE RESTRICT,
 FOREIGN KEY(owner_id,workload_id) REFERENCES core_workloads(owner_id,workload_id) ON DELETE RESTRICT,
 FOREIGN KEY(owner_id,task_id) REFERENCES agent_tasks(owner_id,task_id) ON DELETE RESTRICT,
 FOREIGN KEY(owner_id,confirmation_id) REFERENCES agent_confirmations(owner_id,confirmation_id) ON DELETE RESTRICT,
 UNIQUE(owner_id,operation_id));
CREATE UNIQUE INDEX IF NOT EXISTS core_workload_operations_live_idx ON core_workload_operations(owner_id,workload_id) WHERE status IN ('waiting_user','running');
CREATE TABLE IF NOT EXISTS core_workload_events (owner_id text NOT NULL, workload_id uuid NOT NULL, operation_id uuid NOT NULL, sequence bigint NOT NULL,
 public_sequence bigint NOT NULL CHECK(public_sequence > 0), kind text NOT NULL, status text NOT NULL, message text NOT NULL DEFAULT '', readback_json jsonb,
 at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(owner_id,operation_id,sequence),
 UNIQUE(owner_id,workload_id,public_sequence),
 FOREIGN KEY(owner_id,operation_id) REFERENCES core_workload_operations(owner_id,operation_id) ON DELETE RESTRICT,
 FOREIGN KEY(owner_id,workload_id) REFERENCES core_workloads(owner_id,workload_id) ON DELETE RESTRICT);
CREATE TABLE IF NOT EXISTS core_workload_event_counters (owner_id text NOT NULL, operation_id uuid NOT NULL, next_sequence bigint NOT NULL CHECK(next_sequence > 0), PRIMARY KEY(owner_id,operation_id), FOREIGN KEY(owner_id,operation_id) REFERENCES core_workload_operations(owner_id,operation_id) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS core_workload_idempotency (owner_id text NOT NULL, operation text NOT NULL, idempotency_key uuid NOT NULL,
 request_hash text NOT NULL, plan_id uuid, operation_id uuid, response_json jsonb NOT NULL, created_at timestamptz NOT NULL DEFAULT clock_timestamp(), PRIMARY KEY(owner_id,operation,idempotency_key),
 FOREIGN KEY(owner_id,plan_id) REFERENCES core_workload_plans(owner_id,plan_id) ON DELETE RESTRICT,
 FOREIGN KEY(owner_id,operation_id) REFERENCES core_workload_operations(owner_id,operation_id) ON DELETE RESTRICT);`

func (s *PostgresWorkloadStore) loadOperation(c context.Context, id string) (workload.Operation, error) {
	var o workload.Operation
	var claim sql.NullString
	var lease sql.NullTime
	err := s.store.db.QueryRowContext(c, `SELECT operation_id::text,workload_id::text,expected_workload_revision,plan_id::text,operation,plan_revision,plan_digest,target_kind,task_id::text,confirmation_id::text,status,revision,failure_code,failure_summary,created_at,updated_at,dispatch_state,dispatch_attempt,dispatch_epoch,dispatch_claim,dispatch_lease_until,completion_fingerprint FROM core_workload_operations WHERE owner_id=$1 AND operation_id=$2`, s.ownerID, id).Scan(&o.ID, &o.WorkloadID, &o.ExpectedWorkloadRevision, &o.PlanID, &o.Kind, &o.PlanRevision, &o.PlanDigest, &o.TargetKind, &o.TaskID, &o.ConfirmationID, &o.Status, &o.Revision, &o.FailureCode, &o.FailureSummary, &o.CreatedAt, &o.UpdatedAt, &o.DispatchState, &o.DispatchAttempt, &o.DispatchEpoch, &claim, &lease, &o.CompletionFingerprint)
	if errors.Is(err, sql.ErrNoRows) {
		return o, workload.ErrNotFound
	}
	if err != nil {
		return o, err
	}
	if claim.Valid {
		o.DispatchClaim = claim.String
	}
	if lease.Valid {
		o.DispatchLeaseUntil = lease.Time
	}
	return o, nil
}
func mapWorkloadError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return workload.ErrNotFound
	}
	if err == nil {
		return nil
	}
	m := strings.ToLower(err.Error())
	if strings.Contains(m, "duplicate") || strings.Contains(m, "unique") {
		return workload.ErrConflict
	}
	return err
}
