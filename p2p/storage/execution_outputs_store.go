package storage

// Durable output persistence for execution/v2.  This adapter deliberately
// contains no ProductCore or transport wiring: it owns only the owner-scoped
// artifact, event, and service-binding records which are consumed by those
// layers.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/google/uuid"
)

var executionEventKind = regexp.MustCompile(`^[a-z][a-z0-9_.-]*$`)

type ExecutionArtifactCreate struct {
	OwnerID       string
	ArtifactID    string
	ProjectID     string
	PlanID        string
	PlanRevision  uint64
	RunID         string
	AttemptID     string
	ContentDigest coreexecution.Digest
	StorageRef    string
	SizeBytes     int64
	MediaType     string
	Metadata      any
}

type ExecutionArtifactRecord struct {
	OwnerID        string
	ArtifactID     string
	ProjectID      string
	PlanID         string
	PlanRevision   uint64
	RunID          string
	AttemptID      string
	ContentDigest  coreexecution.Digest
	StorageBackend string
	StorageRef     string
	SizeBytes      int64
	MediaType      string
	Revision       uint64
	Status         string
	Metadata       json.RawMessage
	CreatedAt      time.Time
}

type ExecutionEventCreate struct {
	OwnerID   string
	RunID     string
	StageID   string
	AttemptID string
	StepKey   string
	Kind      string
	EventKey  string
	Status    string
	Payload   any
}

type ExecutionEventRecord struct {
	OwnerID       string
	RunID         string
	EventID       string
	Sequence      uint64
	StageID       string
	AttemptID     string
	StepKey       string
	Kind          string
	EventKey      string
	EventDigest   coreexecution.Digest
	Status        string
	PayloadDigest coreexecution.Digest
	Payload       json.RawMessage
	CreatedAt     time.Time
}

type DeploymentEventCreate struct {
	OwnerID      string
	DeploymentID string
	EventID      string
	EventKey     string
	Kind         string
	Status       string
	Payload      any
}

type DeploymentEventRecord struct {
	OwnerID      string
	DeploymentID string
	EventID      string
	Sequence     uint64
	EventKey     string
	Kind         string
	EventDigest  coreexecution.Digest
	Status       string
	Payload      json.RawMessage
	CreatedAt    time.Time
}

type ServiceBindingCreate struct {
	OwnerID          string
	Binding          coreexecution.ServiceBinding
	ExpectedRevision uint64
	IdempotencyID    string
}

func (s *DatabaseExecutionStore) CreateArtifactMetadata(ctx context.Context, in ExecutionArtifactCreate) (ExecutionArtifactRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(in.OwnerID) == "" || !validUUID(in.ArtifactID) || !validUUID(in.ProjectID) || !validUUID(in.PlanID) || in.PlanRevision == 0 || !coreexecution.ValidateDigest(string(in.ContentDigest)) || in.SizeBytes < 0 || !validStorageRef(in.StorageRef, in.ContentDigest) {
		return ExecutionArtifactRecord{}, ErrExecutionStoreInvalid
	}
	if in.RunID != "" && !validUUID(in.RunID) || in.AttemptID != "" && !validUUID(in.AttemptID) {
		return ExecutionArtifactRecord{}, ErrExecutionStoreInvalid
	}
	if in.AttemptID != "" && in.RunID == "" {
		return ExecutionArtifactRecord{}, ErrExecutionStoreInvalid
	}
	media := strings.TrimSpace(in.MediaType)
	if media == "" {
		media = "application/octet-stream"
	}
	metadata, err := canonicalRedactedJSON(in.Metadata)
	if err != nil {
		return ExecutionArtifactRecord{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionArtifactRecord{}, err
	}
	defer tx.Rollback()
	if err := validateArtifactFrozenScope(ctx, tx, in); err != nil {
		return ExecutionArtifactRecord{}, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO core_execution_artifacts(owner_id,artifact_id,project_id,plan_id,plan_revision,run_id,attempt_id,content_digest,storage_backend,storage_ref,size_bytes,media_type,revision,status,schema_version,metadata_json,created_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,NULLIF($7,'')::uuid,$8,'filesystem',$9,$10,$11,1,'available','execution-artifact/v2',$12,$13) ON CONFLICT (owner_id,artifact_id) DO NOTHING`, in.OwnerID, in.ArtifactID, in.ProjectID, in.PlanID, in.PlanRevision, in.RunID, in.AttemptID, in.ContentDigest, in.StorageRef, in.SizeBytes, media, metadata, now)
	if err != nil {
		return ExecutionArtifactRecord{}, mapExecutionConflict(err)
	}
	got, err := getArtifactTx(ctx, tx, s.db, in.OwnerID, in.ArtifactID)
	if err != nil {
		return ExecutionArtifactRecord{}, err
	}
	if got.ProjectID != in.ProjectID || got.PlanID != in.PlanID || got.PlanRevision != in.PlanRevision || got.RunID != in.RunID || got.AttemptID != in.AttemptID || got.ContentDigest != in.ContentDigest || got.StorageRef != in.StorageRef || got.SizeBytes != in.SizeBytes || got.MediaType != media || !jsonEqual(got.Metadata, metadata) {
		return ExecutionArtifactRecord{}, coreexecution.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionArtifactRecord{}, err
	}
	return got, nil
}

func (s *DatabaseExecutionStore) GetArtifactMetadata(ctx context.Context, owner, artifactID string) (ExecutionArtifactRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !validUUID(artifactID) {
		return ExecutionArtifactRecord{}, ErrExecutionStoreInvalid
	}
	return getArtifactTx(ctx, nil, s.db, owner, artifactID)
}

func (s *DatabaseExecutionStore) AppendExecutionEvent(ctx context.Context, in ExecutionEventCreate) (ExecutionEventRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(in.OwnerID) == "" || !validUUID(in.RunID) || (in.StageID != "" && !validUUID(in.StageID)) || (in.AttemptID != "" && !validUUID(in.AttemptID)) || !executionEventKind.MatchString(in.Kind) {
		return ExecutionEventRecord{}, ErrExecutionStoreInvalid
	}
	status := in.Status
	if status == "" {
		status = "recorded"
	}
	payload, err := canonicalRedactedJSON(in.Payload)
	if err != nil {
		return ExecutionEventRecord{}, err
	}
	eventDigest, err := executionEventDigest(in.OwnerID, in.RunID, in.StageID, in.AttemptID, in.StepKey, in.Kind, in.EventKey, status, payload)
	if err != nil {
		return ExecutionEventRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionEventRecord{}, err
	}
	defer tx.Rollback()
	// Serialize appenders for one run before checking an EventKey.  Without
	// locking the run row first, concurrent retries can both observe no row,
	// allocate sequences, and turn a deterministic replay into a unique-key
	// error.
	var runExists int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, in.OwnerID, in.RunID).Scan(&runExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ExecutionEventRecord{}, coreexecution.ErrNotFound
		}
		return ExecutionEventRecord{}, err
	}
	if in.EventKey != "" {
		var existingID string
		err = tx.QueryRowContext(ctx, `SELECT event_id::text FROM core_execution_events WHERE owner_id=$1 AND run_id=$2 AND event_key=$3`, in.OwnerID, in.RunID, in.EventKey).Scan(&existingID)
		if err == nil {
			got, e := getExecutionEventTx(ctx, tx, in.OwnerID, in.RunID, existingID)
			if e != nil {
				return ExecutionEventRecord{}, e
			}
			if got.EventDigest != eventDigest || !jsonEqual(got.Payload, payload) {
				return ExecutionEventRecord{}, coreexecution.ErrConflict
			}
			if e = tx.Commit(); e != nil {
				return ExecutionEventRecord{}, e
			}
			return got, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return ExecutionEventRecord{}, err
		}
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_event_counters(owner_id,run_id,next_sequence) VALUES($1,$2,1) ON CONFLICT DO NOTHING`, in.OwnerID, in.RunID); err != nil {
		return ExecutionEventRecord{}, err
	}
	var seq uint64
	if err = tx.QueryRowContext(ctx, `UPDATE core_execution_event_counters SET next_sequence=next_sequence+1 WHERE owner_id=$1 AND run_id=$2 RETURNING next_sequence-1`, in.OwnerID, in.RunID).Scan(&seq); err != nil {
		return ExecutionEventRecord{}, err
	}
	eid := uuid.NewString()
	_, err = tx.ExecContext(ctx, `INSERT INTO core_execution_events(owner_id,run_id,event_id,sequence,stage_id,attempt_id,step_key,kind,event_key,event_digest,status,event_json,created_at) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,NULLIF($6,'')::uuid,NULLIF($7,''),$8,$9,$10,$11,$12,$13)`, in.OwnerID, in.RunID, eid, seq, in.StageID, in.AttemptID, in.StepKey, in.Kind, in.EventKey, eventDigest, status, payload, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return ExecutionEventRecord{}, mapExecutionConflict(err)
	}
	got, err := getExecutionEventTx(ctx, tx, in.OwnerID, in.RunID, eid)
	if err != nil {
		return ExecutionEventRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return ExecutionEventRecord{}, err
	}
	return got, nil
}

func (s *DatabaseExecutionStore) ListExecutionEvents(ctx context.Context, owner, runID string, after uint64, limit int) ([]ExecutionEventRecord, uint64, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !validUUID(runID) {
		return nil, 0, ErrExecutionStoreInvalid
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id::text,sequence,COALESCE(stage_id::text,''),COALESCE(attempt_id::text,''),COALESCE(step_key,''),kind,event_key,event_digest,status,event_json,created_at FROM core_execution_events WHERE owner_id=$1 AND run_id=$2 AND sequence>$3 ORDER BY sequence ASC,event_id ASC LIMIT $4`, owner, runID, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]ExecutionEventRecord, 0, limit)
	for rows.Next() {
		var x ExecutionEventRecord
		if err := rows.Scan(&x.EventID, &x.Sequence, &x.StageID, &x.AttemptID, &x.StepKey, &x.Kind, &x.EventKey, &x.EventDigest, &x.Status, &x.Payload, &x.CreatedAt); err != nil {
			return nil, 0, err
		}
		x.Payload, _ = canonicalJSONBytes(x.Payload)
		x.OwnerID, x.RunID = owner, runID
		x.PayloadDigest = coreexecution.Digest(digestBytes(x.Payload))
		if expected, e := executionEventDigest(owner, runID, x.StageID, x.AttemptID, x.StepKey, x.Kind, x.EventKey, x.Status, x.Payload); e != nil || expected != x.EventDigest {
			return nil, 0, ErrExecutionStoreDrift
		}
		items = append(items, x)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next uint64
	if len(items) > limit {
		next = items[limit-1].Sequence
		items = items[:limit]
	}
	return items, next, nil
}

func (s *DatabaseExecutionStore) AppendDeploymentEvent(ctx context.Context, in DeploymentEventCreate) (DeploymentEventRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(in.OwnerID) == "" || !validUUID(in.DeploymentID) || !executionEventKind.MatchString(in.Kind) {
		return DeploymentEventRecord{}, ErrExecutionStoreInvalid
	}
	status := in.Status
	if status == "" {
		status = "recorded"
	}
	payload, err := canonicalRedactedJSON(in.Payload)
	if err != nil {
		return DeploymentEventRecord{}, err
	}
	digest, err := coreexecution.CanonicalDigest(struct {
		OwnerID, DeploymentID, EventKey, Kind, Status string
		Payload                                       json.RawMessage
	}{in.OwnerID, in.DeploymentID, in.EventKey, in.Kind, status, payload})
	if err != nil {
		return DeploymentEventRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeploymentEventRecord{}, err
	}
	defer tx.Rollback()
	var deploymentExists int
	if err = tx.QueryRowContext(ctx, `SELECT 1 FROM core_execution_deployments WHERE owner_id=$1 AND deployment_id=$2 FOR UPDATE`, in.OwnerID, in.DeploymentID).Scan(&deploymentExists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return DeploymentEventRecord{}, coreexecution.ErrNotFound
		}
		return DeploymentEventRecord{}, err
	}
	if in.EventKey != "" {
		var id string
		if e := tx.QueryRowContext(ctx, `SELECT event_id::text FROM core_execution_deployment_events WHERE owner_id=$1 AND deployment_id=$2 AND event_json->>'event_key'=$3`, in.OwnerID, in.DeploymentID, in.EventKey).Scan(&id); e == nil {
			got, ge := getDeploymentEventTx(ctx, tx, in.OwnerID, in.DeploymentID, id)
			if ge != nil {
				return DeploymentEventRecord{}, ge
			}
			if got.EventDigest != digest || !jsonEqual(got.Payload, payload) {
				return DeploymentEventRecord{}, coreexecution.ErrConflict
			}
			if ge = tx.Commit(); ge != nil {
				return DeploymentEventRecord{}, ge
			}
			return got, nil
		}
	}
	var seq uint64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(sequence),0)+1 FROM core_execution_deployment_events WHERE owner_id=$1 AND deployment_id=$2`, in.OwnerID, in.DeploymentID).Scan(&seq); err != nil {
		return DeploymentEventRecord{}, err
	}
	eid := uuid.NewString()
	eventJSON, _ := json.Marshal(map[string]any{"event_key": in.EventKey, "kind": in.Kind, "status": status, "payload": json.RawMessage(payload)})
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_deployment_events(owner_id,deployment_id,event_id,sequence,event_digest,event_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)`, in.OwnerID, in.DeploymentID, eid, seq, digest, eventJSON, s.now().UTC().Truncate(time.Microsecond)); err != nil {
		return DeploymentEventRecord{}, mapExecutionConflict(err)
	}
	got, err := getDeploymentEventTx(ctx, tx, in.OwnerID, in.DeploymentID, eid)
	if err != nil {
		return DeploymentEventRecord{}, err
	}
	if err = tx.Commit(); err != nil {
		return DeploymentEventRecord{}, err
	}
	return got, nil
}

func (s *DatabaseExecutionStore) ListDeploymentEvents(ctx context.Context, owner, deploymentID string, after uint64, limit int) ([]DeploymentEventRecord, uint64, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !validUUID(deploymentID) {
		return nil, 0, ErrExecutionStoreInvalid
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id::text,sequence,event_digest,event_json,created_at FROM core_execution_deployment_events WHERE owner_id=$1 AND deployment_id=$2 AND sequence>$3 ORDER BY sequence ASC,event_id ASC LIMIT $4`, owner, deploymentID, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]DeploymentEventRecord, 0, limit)
	for rows.Next() {
		var x DeploymentEventRecord
		var raw []byte
		if err := rows.Scan(&x.EventID, &x.Sequence, &x.EventDigest, &raw, &x.CreatedAt); err != nil {
			return nil, 0, err
		}
		var env struct {
			EventKey string          `json:"event_key"`
			Kind     string          `json:"kind"`
			Status   string          `json:"status"`
			Payload  json.RawMessage `json:"payload"`
		}
		if err := strictJSON(raw, &env); err != nil {
			return nil, 0, ErrExecutionStoreDrift
		}
		x.Payload, _ = canonicalJSONBytes(env.Payload)
		x.OwnerID, x.DeploymentID, x.EventKey, x.Kind, x.Status = owner, deploymentID, env.EventKey, env.Kind, env.Status
		if expected, e := coreexecution.CanonicalDigest(struct {
			OwnerID, DeploymentID, EventKey, Kind, Status string
			Payload                                       json.RawMessage
		}{owner, deploymentID, x.EventKey, x.Kind, x.Status, x.Payload}); e != nil || expected != x.EventDigest {
			return nil, 0, ErrExecutionStoreDrift
		}
		items = append(items, x)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next uint64
	if len(items) > limit {
		next = items[limit-1].Sequence
		items = items[:limit]
	}
	return items, next, nil
}

func (s *DatabaseExecutionStore) CreateServiceBinding(ctx context.Context, in ServiceBindingCreate) (coreexecution.ServiceBinding, error) {
	if s == nil || s.db == nil || strings.TrimSpace(in.OwnerID) == "" || in.OwnerID != in.Binding.OwnerID || !validUUID(in.Binding.BindingID) || !validUUID(in.Binding.DeploymentID) || !validUUID(in.Binding.ProjectID) || !validUUID(in.Binding.RunID) || in.IdempotencyID == "" || !validUUID(in.IdempotencyID) {
		return coreexecution.ServiceBinding{}, ErrExecutionStoreInvalid
	}
	if !validUUID(in.Binding.TargetID) || in.Binding.TargetRevision == 0 || !in.Binding.TargetDigest.Valid() {
		return coreexecution.ServiceBinding{}, ErrExecutionStoreInvalid
	}
	if strings.TrimSpace(in.Binding.Protocol) == "" || strings.TrimSpace(in.Binding.Endpoint) == "" {
		return coreexecution.ServiceBinding{}, ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return coreexecution.ServiceBinding{}, err
	}
	defer tx.Rollback()
	// A committed idempotency row is authoritative for replay.  Read it before
	// consulting the mutable binding head so a later revision cannot turn a
	// harmless retry into a new revision.
	key, _ := uuid.Parse(in.IdempotencyID)
	var replayReq, replayResp string
	var replayRaw []byte
	if e := tx.QueryRowContext(ctx, `SELECT request_digest,response_digest,response_json FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2 FOR UPDATE`, in.OwnerID, key).Scan(&replayReq, &replayResp, &replayRaw); e == nil {
		var replay coreexecution.ServiceBinding
		canonicalReplay, ce := canonicalBindingBytes(replayRaw, &replay)
		if ce != nil || validateStoredBinding(replay) != nil || replay.BindingID != in.Binding.BindingID || replay.OwnerID != in.OwnerID || replayResp != string(digestBytes(canonicalReplay)) {
			return coreexecution.ServiceBinding{}, ErrExecutionStoreDrift
		}
		candidate, e := prepareBinding(in.Binding, replay.Revision)
		if e != nil {
			return coreexecution.ServiceBinding{}, e
		}
		req, e := coreexecution.CanonicalDigest(struct {
			OwnerID          string
			Binding          coreexecution.ServiceBinding
			ExpectedRevision uint64
		}{in.OwnerID, candidate, in.ExpectedRevision})
		if e != nil || string(req) != replayReq {
			return coreexecution.ServiceBinding{}, coreexecution.ErrConflict
		}
		if e = tx.Commit(); e != nil {
			return coreexecution.ServiceBinding{}, e
		}
		return replay, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return coreexecution.ServiceBinding{}, e
	}
	var existing coreexecution.ServiceBinding
	var existingRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT snapshot_json FROM core_execution_service_bindings WHERE owner_id=$1 AND binding_id=$2 FOR UPDATE`, in.OwnerID, in.Binding.BindingID).Scan(&existingRaw)
	hasExisting := err == nil
	if hasExisting && strictJSON(existingRaw, &existing) != nil {
		return coreexecution.ServiceBinding{}, ErrExecutionStoreDrift
	}
	if !hasExisting && !errors.Is(err, sql.ErrNoRows) {
		return coreexecution.ServiceBinding{}, err
	}
	if hasExisting && (in.ExpectedRevision == 0 || existing.Revision != in.ExpectedRevision) {
		return coreexecution.ServiceBinding{}, coreexecution.ErrConflict
	}
	if !hasExisting && in.ExpectedRevision != 0 {
		return coreexecution.ServiceBinding{}, coreexecution.ErrConflict
	}
	revision := uint64(1)
	if hasExisting {
		revision = existing.Revision + 1
	}
	b, err := prepareBinding(in.Binding, revision)
	if err != nil {
		return coreexecution.ServiceBinding{}, err
	}
	requestDigest, err := coreexecution.CanonicalDigest(struct {
		OwnerID          string
		Binding          coreexecution.ServiceBinding
		ExpectedRevision uint64
	}{in.OwnerID, b, in.ExpectedRevision})
	if err != nil {
		return coreexecution.ServiceBinding{}, err
	}
	raw, _ := json.Marshal(b)
	now := s.now().UTC().Truncate(time.Microsecond)
	responseDigest := string(digestBytes(raw))
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_idempotency(owner_id,idempotency_id,run_id,key_digest,request_digest,response_digest,status,schema_version,response_json,created_at) VALUES($1,$2,$3,$4,$5,$6,'succeeded','execution-idempotency/v2',$7,$8) ON CONFLICT (owner_id,idempotency_id) DO NOTHING`, in.OwnerID, key, b.RunID, string(digestBytes([]byte(in.IdempotencyID))), requestDigest, responseDigest, raw, now); err != nil {
		return coreexecution.ServiceBinding{}, err
	}
	var oldReq, oldResponseDigest string
	var oldRaw []byte
	if err = tx.QueryRowContext(ctx, `SELECT request_digest,response_digest,response_json FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2 FOR UPDATE`, in.OwnerID, key).Scan(&oldReq, &oldResponseDigest, &oldRaw); err != nil {
		return coreexecution.ServiceBinding{}, err
	}
	var oldBinding coreexecution.ServiceBinding
	canonicalOld, ce := canonicalBindingBytes(oldRaw, &oldBinding)
	if ce != nil || oldReq != string(requestDigest) || oldResponseDigest != string(digestBytes(canonicalOld)) {
		return coreexecution.ServiceBinding{}, coreexecution.ErrConflict
	}
	if err = validateServiceBindingScope(ctx, tx, b); err != nil {
		return coreexecution.ServiceBinding{}, fmt.Errorf("execution service binding scope: %w", err)
	}
	if err = validateBindingArtifacts(ctx, tx, b); err != nil {
		return coreexecution.ServiceBinding{}, fmt.Errorf("execution service binding artifacts: %w", err)
	}
	if hasExisting && !sameServiceBindingIdentity(existing, b) {
		return coreexecution.ServiceBinding{}, coreexecution.ErrConflict
	}
	if hasExisting {
		result, e := tx.ExecContext(ctx, `UPDATE core_execution_service_bindings SET release_id=$4,protocol=$5,endpoint=$6,binding_digest=$7,operation_schema_digest=$8,usage_artifact_id=NULLIF($9,'')::uuid,health_artifact_id=NULLIF($10,'')::uuid,revision=$11,snapshot_json=$12,updated_at=$13 WHERE owner_id=$1 AND binding_id=$2 AND revision=$3`, b.OwnerID, b.BindingID, in.ExpectedRevision, b.ReleaseID, b.Protocol, b.Endpoint, b.Digest, operationSchemaDigest(b.OperationSchemas), nullArtifactID(b.UsageArtifact), nullArtifactID(b.HealthArtifact), b.Revision, raw, now)
		if e != nil {
			return coreexecution.ServiceBinding{}, mapExecutionConflict(e)
		}
		n, e := result.RowsAffected()
		if e != nil || n != 1 {
			return coreexecution.ServiceBinding{}, coreexecution.ErrConflict
		}
	} else if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_service_bindings(owner_id,binding_id,deployment_id,release_id,project_id,run_id,target_id,target_revision,protocol,endpoint,binding_digest,operation_schema_digest,usage_artifact_id,health_artifact_id,revision,status,schema_version,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid,NULLIF($8,0),$9,$10,$11,$12,NULLIF($13,'')::uuid,NULLIF($14,'')::uuid,1,'active','execution-service-binding/v2',$15,$16,$16)`, b.OwnerID, b.BindingID, b.DeploymentID, b.ReleaseID, b.ProjectID, b.RunID, b.TargetID, b.TargetRevision, b.Protocol, b.Endpoint, b.Digest, operationSchemaDigest(b.OperationSchemas), nullArtifactID(b.UsageArtifact), nullArtifactID(b.HealthArtifact), raw, now); err != nil {
		return coreexecution.ServiceBinding{}, fmt.Errorf("execution service binding insert: %w", mapExecutionConflict(err))
	}
	if err = tx.Commit(); err != nil {
		return coreexecution.ServiceBinding{}, err
	}
	return b, nil
}

func (s *DatabaseExecutionStore) GetServiceBinding(ctx context.Context, owner, bindingID string) (coreexecution.ServiceBinding, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !validUUID(bindingID) {
		return coreexecution.ServiceBinding{}, ErrExecutionStoreInvalid
	}
	var raw []byte
	var deployment, release, project, run, protocol, endpoint, bindingDigest, operationDigest string
	var target sql.NullString
	var targetRevision sql.NullInt64
	var revision uint64
	if err := s.db.QueryRowContext(ctx, `SELECT deployment_id::text,release_id,project_id::text,run_id::text,COALESCE(target_id::text,''),target_revision,protocol,endpoint,binding_digest,operation_schema_digest,revision,snapshot_json FROM core_execution_service_bindings WHERE owner_id=$1 AND binding_id=$2`, owner, bindingID).Scan(&deployment, &release, &project, &run, &target, &targetRevision, &protocol, &endpoint, &bindingDigest, &operationDigest, &revision, &raw); errors.Is(err, sql.ErrNoRows) {
		return coreexecution.ServiceBinding{}, coreexecution.ErrNotFound
	} else if err != nil {
		return coreexecution.ServiceBinding{}, err
	}
	var b coreexecution.ServiceBinding
	if strictJSON(raw, &b) != nil || validateStoredBinding(b) != nil || b.OwnerID != owner || b.BindingID != bindingID || b.DeploymentID != deployment || b.ReleaseID != release || b.ProjectID != project || b.RunID != run || b.Protocol != protocol || b.Endpoint != endpoint || string(b.Digest) != bindingDigest || b.Revision != revision || operationSchemaDigest(b.OperationSchemas) != coreexecution.Digest(operationDigest) || b.TargetID != target.String || (target.Valid && b.TargetRevision != uint64(targetRevision.Int64)) {
		return coreexecution.ServiceBinding{}, ErrExecutionStoreDrift
	}
	var targetDigest string
	if err := s.db.QueryRowContext(ctx, `SELECT target_digest FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3`, owner, b.TargetID, b.TargetRevision).Scan(&targetDigest); err != nil || targetDigest != string(b.TargetDigest) {
		return coreexecution.ServiceBinding{}, ErrExecutionStoreDrift
	}
	return b, nil
}

func (s *DatabaseExecutionStore) ListServiceBindings(ctx context.Context, owner, projectID string, after string, limit int) ([]coreexecution.ServiceBinding, string, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || (projectID != "" && !validUUID(projectID)) {
		return nil, "", ErrExecutionStoreInvalid
	}
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	args := []any{owner}
	q := `SELECT binding_id::text,snapshot_json FROM core_execution_service_bindings WHERE owner_id=$1`
	if projectID != "" {
		q += ` AND project_id=$2`
		args = append(args, projectID)
	}
	if after != "" {
		if !validUUID(after) {
			return nil, "", ErrExecutionStoreInvalid
		}
		q += fmt.Sprintf(` AND binding_id>$%d`, len(args)+1)
		args = append(args, after)
	}
	q += fmt.Sprintf(` ORDER BY binding_id ASC LIMIT %d`, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	items := make([]coreexecution.ServiceBinding, 0, limit)
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, "", err
		}
		var b coreexecution.ServiceBinding
		if strictJSON(raw, &b) != nil || validateStoredBinding(b) != nil || b.OwnerID != owner || b.BindingID != id {
			return nil, "", ErrExecutionStoreDrift
		}
		items = append(items, b)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		next = items[limit-1].BindingID
		items = items[:limit]
	}
	return items, next, nil
}

func getArtifactTx(ctx context.Context, tx *sql.Tx, db *sql.DB, owner, id string) (ExecutionArtifactRecord, error) {
	var x ExecutionArtifactRecord
	var run, attempt sql.NullString
	if err := queryRow(ctx, tx, db, `SELECT owner_id,artifact_id::text,project_id::text,plan_id::text,plan_revision,COALESCE(run_id::text,''),COALESCE(attempt_id::text,''),content_digest,storage_backend,storage_ref,size_bytes,media_type,revision,status,metadata_json,created_at FROM core_execution_artifacts WHERE owner_id=$1 AND artifact_id=$2`, owner, id).Scan(&x.OwnerID, &x.ArtifactID, &x.ProjectID, &x.PlanID, &x.PlanRevision, &run, &attempt, &x.ContentDigest, &x.StorageBackend, &x.StorageRef, &x.SizeBytes, &x.MediaType, &x.Revision, &x.Status, &x.Metadata, &x.CreatedAt); errors.Is(err, sql.ErrNoRows) {
		return x, coreexecution.ErrNotFound
	} else if err != nil {
		return x, err
	}
	x.RunID, x.AttemptID = run.String, attempt.String
	if x.OwnerID != owner || !validStorageRef(x.StorageRef, x.ContentDigest) || x.StorageBackend != "filesystem" || x.Status != "available" || strictJSON(x.Metadata, new(any)) != nil {
		return x, ErrExecutionStoreDrift
	}
	if x.RunID != "" {
		var projectID, planID string
		var planRevision uint64
		if err := queryRow(ctx, tx, db, `SELECT project_id::text,plan_id::text,plan_revision FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, owner, x.RunID).Scan(&projectID, &planID, &planRevision); err != nil || projectID != x.ProjectID || planID != x.PlanID || planRevision != x.PlanRevision {
			return x, ErrExecutionStoreDrift
		}
		if x.AttemptID != "" {
			var attemptRun, attemptProject, attemptPlan string
			var attemptRevision uint64
			if err := queryRow(ctx, tx, db, `SELECT run_id::text,project_id::text,plan_id::text,plan_revision FROM core_execution_step_attempts WHERE owner_id=$1 AND attempt_id=$2`, owner, x.AttemptID).Scan(&attemptRun, &attemptProject, &attemptPlan, &attemptRevision); err != nil || attemptRun != x.RunID || attemptProject != x.ProjectID || attemptPlan != x.PlanID || attemptRevision != x.PlanRevision {
				return x, ErrExecutionStoreDrift
			}
		}
	}
	return x, nil
}

func getExecutionEventTx(ctx context.Context, tx *sql.Tx, owner, runID, eventID string) (ExecutionEventRecord, error) {
	var x ExecutionEventRecord
	if err := queryRow(ctx, tx, nil, `SELECT event_id::text,sequence,COALESCE(stage_id::text,''),COALESCE(attempt_id::text,''),COALESCE(step_key,''),kind,event_key,event_digest,status,event_json,created_at FROM core_execution_events WHERE owner_id=$1 AND run_id=$2 AND event_id=$3`, owner, runID, eventID).Scan(&x.EventID, &x.Sequence, &x.StageID, &x.AttemptID, &x.StepKey, &x.Kind, &x.EventKey, &x.EventDigest, &x.Status, &x.Payload, &x.CreatedAt); errors.Is(err, sql.ErrNoRows) {
		return x, coreexecution.ErrNotFound
	} else if err != nil {
		return x, err
	}
	x.Payload, _ = canonicalJSONBytes(x.Payload)
	x.OwnerID, x.RunID = owner, runID
	x.PayloadDigest = coreexecution.Digest(digestBytes(x.Payload))
	if expected, err := executionEventDigest(owner, runID, x.StageID, x.AttemptID, x.StepKey, x.Kind, x.EventKey, x.Status, x.Payload); err != nil || expected != x.EventDigest {
		return x, ErrExecutionStoreDrift
	}
	return x, nil
}

func getDeploymentEventTx(ctx context.Context, tx *sql.Tx, owner, deploymentID, eventID string) (DeploymentEventRecord, error) {
	var x DeploymentEventRecord
	var raw []byte
	if err := queryRow(ctx, tx, nil, `SELECT event_id::text,sequence,event_digest,event_json,created_at FROM core_execution_deployment_events WHERE owner_id=$1 AND deployment_id=$2 AND event_id=$3`, owner, deploymentID, eventID).Scan(&x.EventID, &x.Sequence, &x.EventDigest, &raw, &x.CreatedAt); errors.Is(err, sql.ErrNoRows) {
		return x, coreexecution.ErrNotFound
	} else if err != nil {
		return x, err
	}
	var env struct {
		EventKey string          `json:"event_key"`
		Kind     string          `json:"kind"`
		Status   string          `json:"status"`
		Payload  json.RawMessage `json:"payload"`
	}
	if strictJSON(raw, &env) != nil {
		return x, ErrExecutionStoreDrift
	}
	x.Payload, _ = canonicalJSONBytes(env.Payload)
	x.OwnerID, x.DeploymentID, x.EventKey, x.Kind, x.Status = owner, deploymentID, env.EventKey, env.Kind, env.Status
	if expected, err := coreexecution.CanonicalDigest(struct {
		OwnerID, DeploymentID, EventKey, Kind, Status string
		Payload                                       json.RawMessage
	}{owner, deploymentID, x.EventKey, x.Kind, x.Status, x.Payload}); err != nil || expected != x.EventDigest {
		return x, ErrExecutionStoreDrift
	}
	return x, nil
}

func validateServiceBindingScope(ctx context.Context, tx *sql.Tx, b coreexecution.ServiceBinding) error {
	var purpose, targetRun, targetProject string
	if err := tx.QueryRowContext(ctx, `SELECT purpose,project_id::text,COALESCE(deployment_id::text,'') FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, b.OwnerID, b.RunID).Scan(&purpose, &targetProject, &targetRun); errors.Is(err, sql.ErrNoRows) {
		return coreexecution.ErrNotFound
	} else if err != nil {
		return err
	}
	if purpose != string(coreexecution.PurposeService) || targetProject != b.ProjectID || targetRun != b.DeploymentID {
		return fmt.Errorf("%w: run purpose/project/deployment mismatch (%s,%s,%s)", coreexecution.ErrConflict, purpose, targetProject, targetRun)
	}
	var targetDigest string
	var targetRaw []byte
	if err := tx.QueryRowContext(ctx, `SELECT target_digest,snapshot_json FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3`, b.OwnerID, b.TargetID, b.TargetRevision).Scan(&targetDigest, &targetRaw); errors.Is(err, sql.ErrNoRows) {
		return coreexecution.ErrConflict
	} else if err != nil {
		return err
	}
	if targetDigest != string(b.TargetDigest) {
		return fmt.Errorf("%w: target digest mismatch", coreexecution.ErrConflict)
	}
	var target struct {
		CredentialRefs []coreexecution.CredentialRef `json:"credential_refs,omitempty"`
	}
	if json.Unmarshal(targetRaw, &target) != nil {
		return fmt.Errorf("%w: target snapshot invalid", coreexecution.ErrConflict)
	}
	for _, ref := range b.AuthRefs {
		found := false
		for _, targetRef := range target.CredentialRefs {
			if targetRef.Ref == ref.Ref {
				if targetRef.Revision != ref.Revision || targetRef.BindingDigest != ref.BindingDigest {
					return coreexecution.ErrConflict
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("%w: binding auth ref absent", coreexecution.ErrConflict)
		}
	}
	return nil
}

func prepareBinding(in coreexecution.ServiceBinding, revision uint64) (coreexecution.ServiceBinding, error) {
	b := in
	b.Revision = revision
	b.Digest = ""
	for i := range b.OperationSchemas {
		b.OperationSchemas[i].Digest = ""
		d, err := coreexecution.CanonicalDigest(struct{ Name, Version string }{b.OperationSchemas[i].Name, b.OperationSchemas[i].Version})
		if err != nil {
			return coreexecution.ServiceBinding{}, err
		}
		b.OperationSchemas[i].Digest = d
	}
	return b.Normalize()
}

func validateStoredBinding(b coreexecution.ServiceBinding) error {
	prepared, err := prepareBinding(b, b.Revision)
	if err != nil || prepared.Digest != b.Digest || len(prepared.OperationSchemas) != len(b.OperationSchemas) {
		return ErrExecutionStoreDrift
	}
	return nil
}

func sameServiceBindingIdentity(a, b coreexecution.ServiceBinding) bool {
	return a.OwnerID == b.OwnerID && a.BindingID == b.BindingID && a.DeploymentID == b.DeploymentID && a.ProjectID == b.ProjectID && a.RunID == b.RunID && a.TargetID == b.TargetID && a.TargetRevision == b.TargetRevision && a.TargetDigest == b.TargetDigest
}

func validateArtifactFrozenScope(ctx context.Context, tx *sql.Tx, in ExecutionArtifactCreate) error {
	if in.RunID == "" {
		return nil
	}
	var projectID, planID string
	var planRevision uint64
	if err := tx.QueryRowContext(ctx, `SELECT project_id::text,plan_id::text,plan_revision FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, in.OwnerID, in.RunID).Scan(&projectID, &planID, &planRevision); errors.Is(err, sql.ErrNoRows) {
		return coreexecution.ErrNotFound
	} else if err != nil {
		return err
	}
	if projectID != in.ProjectID || planID != in.PlanID || planRevision != in.PlanRevision {
		return coreexecution.ErrConflict
	}
	if in.AttemptID != "" {
		var attemptRun, attemptProject, attemptPlan string
		var attemptRevision uint64
		if err := tx.QueryRowContext(ctx, `SELECT run_id::text,project_id::text,plan_id::text,plan_revision FROM core_execution_step_attempts WHERE owner_id=$1 AND attempt_id=$2`, in.OwnerID, in.AttemptID).Scan(&attemptRun, &attemptProject, &attemptPlan, &attemptRevision); errors.Is(err, sql.ErrNoRows) {
			return coreexecution.ErrNotFound
		} else if err != nil {
			return err
		}
		if attemptRun != in.RunID || attemptProject != in.ProjectID || attemptPlan != in.PlanID || attemptRevision != in.PlanRevision {
			return coreexecution.ErrConflict
		}
	}
	return nil
}

func validateBindingArtifacts(ctx context.Context, tx *sql.Tx, b coreexecution.ServiceBinding) error {
	ids := append([]string(nil), b.ArtifactIDs...)
	if b.UsageArtifact.ID != "" {
		ids = append(ids, b.UsageArtifact.ID)
	}
	if b.HealthArtifact.ID != "" {
		ids = append(ids, b.HealthArtifact.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if !validUUID(id) {
			return ErrExecutionStoreInvalid
		}
		var owner, project, plan, digest, run, attempt string
		var rev uint64
		if err := tx.QueryRowContext(ctx, `SELECT owner_id,project_id::text,plan_id::text,plan_revision,COALESCE(run_id::text,''),COALESCE(attempt_id::text,''),content_digest FROM core_execution_artifacts WHERE owner_id=$1 AND artifact_id=$2`, b.OwnerID, id).Scan(&owner, &project, &plan, &rev, &run, &attempt, &digest); err != nil {
			return coreexecution.ErrConflict
		}
		var runPlan, runProject string
		var runRevision uint64
		if err := tx.QueryRowContext(ctx, `SELECT project_id::text,plan_id::text,plan_revision FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, b.OwnerID, b.RunID).Scan(&runProject, &runPlan, &runRevision); err != nil {
			return coreexecution.ErrConflict
		}
		if owner != b.OwnerID || project != b.ProjectID || plan != runPlan || rev != runRevision || plan == "" || rev == 0 || (run != "" && run != b.RunID) {
			return coreexecution.ErrConflict
		}
		if attempt != "" {
			var attemptRun, attemptPlan string
			var attemptRevision uint64
			if err := tx.QueryRowContext(ctx, `SELECT run_id::text,plan_id::text,plan_revision FROM core_execution_step_attempts WHERE owner_id=$1 AND attempt_id=$2`, b.OwnerID, attempt).Scan(&attemptRun, &attemptPlan, &attemptRevision); err != nil || attemptRun != b.RunID || attemptPlan != runPlan || attemptRevision != runRevision {
				return coreexecution.ErrConflict
			}
		}
	}
	for _, ref := range []coreexecution.ArtifactRef{b.UsageArtifact, b.HealthArtifact} {
		if ref.ID == "" {
			continue
		}
		var digest string
		if err := tx.QueryRowContext(ctx, `SELECT content_digest FROM core_execution_artifacts WHERE owner_id=$1 AND artifact_id=$2`, b.OwnerID, ref.ID).Scan(&digest); err != nil || digest != string(ref.Digest) {
			return coreexecution.ErrConflict
		}
	}
	return nil
}

func executionEventDigest(owner, run, stage, attempt, step, kind, key, status string, payload []byte) (coreexecution.Digest, error) {
	return coreexecution.CanonicalDigest(struct {
		OwnerID, RunID, StageID, AttemptID, StepKey, Kind, EventKey, Status string
		PayloadDigest                                                       coreexecution.Digest
	}{owner, run, stage, attempt, step, kind, key, status, coreexecution.Digest(digestBytes(payload))})
}

func operationSchemaDigest(schemas []coreexecution.OperationSchema) coreexecution.Digest {
	d, _ := coreexecution.CanonicalDigest(schemas)
	return d
}
func nullArtifactID(a coreexecution.ArtifactRef) string { return a.ID }
func validStorageRef(ref string, d coreexecution.Digest) bool {
	return strings.HasPrefix(ref, "sha256/") && strings.HasSuffix(ref, string(d)) && len(ref) == len("sha256/")+2+1+64 && coreexecution.ValidateDigest(string(d)) && ref[7:9] == string(d)[:2]
}

func canonicalRedactedJSON(v any) ([]byte, error) {
	// Outputs are durable evidence, so sensitive material is rejected rather
	// than best-effort scrubbed.  The shared validator covers nested metadata,
	// token headers, cookies, and provider credential patterns consistently
	// across catalog and output stores.
	if err := validateCatalogSensitiveData(v); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var decoded any
	if err = json.Unmarshal(raw, &decoded); err != nil {
		return nil, err
	}
	r := redactExecutionValue(decoded, "")
	return json.Marshal(r)
}
func canonicalJSONBytes(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
func canonicalBindingBytes(raw []byte, b *coreexecution.ServiceBinding) ([]byte, error) {
	if err := strictJSON(raw, b); err != nil {
		return nil, err
	}
	return json.Marshal(*b)
}
func redactExecutionValue(v any, key string) any {
	if sensitiveExecutionKey(key) {
		return "[REDACTED]"
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = redactExecutionValue(val, k)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = redactExecutionValue(val, "")
		}
		return out
	default:
		return v
	}
}
func sensitiveExecutionKey(k string) bool {
	k = strings.ToLower(strings.ReplaceAll(k, "-", "_"))
	return strings.Contains(k, "password") || strings.Contains(k, "secret_value") || strings.Contains(k, "private_key") || strings.Contains(k, "api_key") || k == "token" || k == "access_key" || k == "credential"
}
func jsonEqual(a, b []byte) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}
