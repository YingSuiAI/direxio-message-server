package storage

// PostgreSQL persistence for immutable execution-plan/v2 graphs.  This file
// intentionally has no ProductCore/action wiring: callers own the surrounding
// authorization and transaction, while this adapter owns snapshot and row
// validation at the database boundary.

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

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/google/uuid"
)

var (
	ErrExecutionStoreInvalid = errors.New("execution store: invalid")
	ErrExecutionStoreDrift   = errors.New("execution store: immutable snapshot drift")
)

// ExecutionPlanCreate is the complete immutable graph written by CreatePlan.
// Digests on plan, stages, steps, targets and analysis are recomputed by the
// server; a caller supplied digest is never authoritative.
type ExecutionPlanCreate struct {
	OwnerID       string
	Analysis      coreexecution.ProjectAnalysis
	Plan          coreexecution.ExecutionPlan
	IdempotencyID string
}

type DatabaseExecutionStore struct {
	db                     *sql.DB
	now                    func() time.Time
	executors              ExecutionExecutorAvailability
	executorsAuthoritative bool
}

func NewDatabaseExecutionStore(db *sql.DB, clock func() time.Time) *DatabaseExecutionStore {
	if clock == nil {
		clock = time.Now
	}
	return &DatabaseExecutionStore{db: db, now: clock}
}

// SetExecutorAvailability installs the immutable runtime executor catalog.
// Production wiring calls this before the worker starts. Stores used only for
// catalog reads may leave it unset; they never claim executable stages.
func (s *DatabaseExecutionStore) SetExecutorAvailability(availability ExecutionExecutorAvailability) {
	if s == nil {
		return
	}
	s.executors = availability
	s.executorsAuthoritative = true
}

// CreatePlan atomically persists project, analysis, targets, the ready plan
// revision, all stage/step snapshots, and immutable referenced records.
func (s *DatabaseExecutionStore) CreatePlan(ctx context.Context, in ExecutionPlanCreate) (coreexecution.ExecutionPlan, error) {
	if s == nil || s.db == nil || strings.TrimSpace(in.OwnerID) == "" || in.OwnerID != in.Plan.OwnerID || !coreexecution.ValidateUUID(in.Plan.ID) || !coreexecution.ValidateUUID(in.Analysis.AnalysisID) || !coreexecution.ValidateUUID(in.Analysis.ProjectID) || in.Analysis.ProjectID != in.Plan.ProjectID || in.IdempotencyID == "" {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreInvalid
	}
	idem, err := uuid.Parse(in.IdempotencyID)
	if err != nil {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreInvalid
	}
	analysis, plan, err := normalizeForStorage(in.Analysis, in.Plan)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	planSnapshot, err := coreexecution.PlanSnapshotFromPlan(plan)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	planRaw, err := coreexecution.EncodePlanSnapshot(planSnapshot)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	analysisRaw, err := json.Marshal(analysis)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	requestDigest := digestBytes(append(append([]byte(nil), planRaw...), analysisRaw...))
	responseRaw, err := json.Marshal(plan)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	now := s.now().UTC()
	if !plan.ExpiresAt.After(now) {
		return coreexecution.ExecutionPlan{}, coreexecution.ErrExpired
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	defer tx.Rollback()
	// response_json is JSONB and PostgreSQL may reorder object keys. Digest the
	// canonical execution snapshot instead of DB-rendered JSON text.
	responseDigest := string(digestBytes(planRaw))
	// Insert first: a unique-key wait serializes concurrent creators. The
	// following FOR UPDATE then deterministically replays or conflicts.
	result, err := tx.ExecContext(ctx, `INSERT INTO core_execution_idempotency(owner_id,idempotency_id,key_digest,request_digest,response_digest,status,schema_version,response_json,created_at) VALUES($1,$2,$3,$4,$5,'succeeded','execution-idempotency/v2',$6,$7) ON CONFLICT (owner_id,idempotency_id) DO NOTHING`, in.OwnerID, idem, string(digestBytes([]byte(in.IdempotencyID))), string(requestDigest), responseDigest, responseRaw, now)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	var oldReq, oldRespDigest, oldResp []byte
	var oldStatus, oldSchema string
	if err = tx.QueryRowContext(ctx, `SELECT request_digest,response_digest,status,schema_version,response_json FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2 FOR UPDATE`, in.OwnerID, idem).Scan(&oldReq, &oldRespDigest, &oldStatus, &oldSchema, &oldResp); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if !equalDigest(oldReq, requestDigest) || oldStatus != "succeeded" || oldSchema != "execution-idempotency/v2" || string(oldRespDigest) != responseDigest {
		return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
	}
	if oldRespDigest == nil || string(oldRespDigest) != responseDigest {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreDrift
	}
	replay, replayErr := decodePlanResponse(oldResp)
	replaySnapshot, snapshotErr := coreexecution.PlanSnapshotFromPlan(replay)
	replayRaw, encodeErr := coreexecution.EncodePlanSnapshot(replaySnapshot)
	if replayErr != nil || snapshotErr != nil || encodeErr != nil || string(digestBytes(replayRaw)) != string(oldRespDigest) || !samePlan(replay, plan) || replay.OwnerID != plan.OwnerID || replay.ID != plan.ID || replay.Revision != plan.Revision || replay.Digest != plan.Digest {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreDrift
	}
	// Existing rows are a replay; do not attempt graph insertion again.
	if inserted == 0 {
		if err := tx.Commit(); err != nil {
			return coreexecution.ExecutionPlan{}, err
		}
		return plan, nil
	}
	projectRaw, _ := json.Marshal(struct {
		SchemaVersion string `json:"schema_version"`
		ProjectID     string `json:"project_id"`
		OwnerID       string `json:"owner_id"`
	}{"execution-project/v2", plan.ProjectID, in.OwnerID})
	projectDigest := digestBytes(projectRaw)
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_projects(owner_id,project_id,revision,status,schema_version,project_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,'active','execution-project/v2',$4,$5,$6,$6) ON CONFLICT (owner_id,project_id) DO NOTHING`, in.OwnerID, plan.ProjectID, plan.Revision, projectDigest, projectRaw, now); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_analyses(owner_id,analysis_id,project_id,revision,status,schema_version,analysis_digest,snapshot_json,created_at) VALUES($1,$2,$3,$4,'ready','execution-analysis/v2',$5,$6,$7) ON CONFLICT (owner_id,analysis_id) DO NOTHING`, in.OwnerID, analysis.AnalysisID, analysis.ProjectID, analysis.Revision, analysis.Digest, analysisRaw, analysis.CreatedAt); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	var existingAnalysisDigest string
	if err = tx.QueryRowContext(ctx, `SELECT analysis_digest FROM core_execution_analyses WHERE owner_id=$1 AND analysis_id=$2`, in.OwnerID, analysis.AnalysisID).Scan(&existingAnalysisDigest); err != nil || existingAnalysisDigest != string(analysis.Digest) {
		return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
	}
	for _, target := range plan.Targets {
		raw, e := json.Marshal(target)
		if e != nil {
			return coreexecution.ExecutionPlan{}, e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO core_execution_targets(owner_id,target_id,target_revision,status,schema_version,provider,target_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,'active','execution-target/v2',$4,$5,$6,$7,$7) ON CONFLICT (owner_id,target_id,target_revision) DO NOTHING`, in.OwnerID, target.ID, target.Revision, target.Provider, target.Digest, raw, now); e != nil {
			return coreexecution.ExecutionPlan{}, e
		}
		var existingTargetDigest string
		if e = tx.QueryRowContext(ctx, `SELECT target_digest FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3`, in.OwnerID, target.ID, target.Revision).Scan(&existingTargetDigest); e != nil || existingTargetDigest != string(target.Digest) {
			return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
		}
	}
	if err = ensurePlanHead(ctx, tx, in.OwnerID, plan, planRaw, now); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	planRevisionID := uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprintf("%s:%d", plan.ID, plan.Revision)))
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_plan_revisions(owner_id,plan_id,plan_revision_id,revision,project_id,analysis_id,status,schema_version,plan_digest,snapshot_json,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,'ready','execution-plan/v2',$7,$8,$9,$10)`, in.OwnerID, plan.ID, planRevisionID, plan.Revision, plan.ProjectID, plan.AnalysisID, plan.Digest, planRaw, plan.ExpiresAt, now); err != nil {
		return coreexecution.ExecutionPlan{}, mapExecutionConflict(err)
	}
	for i, stage := range plan.Stages {
		stageSnapshot, e := coreexecution.StageSnapshotFromStage(stage)
		if e != nil {
			return coreexecution.ExecutionPlan{}, e
		}
		stageRaw, e := coreexecution.EncodeStageSnapshot(stageSnapshot)
		if e != nil {
			return coreexecution.ExecutionPlan{}, e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO core_execution_plan_stages(owner_id,plan_id,plan_revision,stage_key,stage_revision,stage_digest,ordinal,status,schema_version,snapshot_json) VALUES($1,$2,$3,$4,$5,$6,$7,'ready','execution-plan/v2',$8)`, in.OwnerID, plan.ID, plan.Revision, stage.StageKey, stage.Revision, stage.Digest, i+1, stageRaw); e != nil {
			return coreexecution.ExecutionPlan{}, e
		}
		sets := []struct {
			name  coreexecution.StepSet
			steps []coreexecution.ExecutionStep
		}{{coreexecution.StepSetForward, stage.Steps}, {coreexecution.StepSetRollback, stage.RollbackSteps}}
		for _, set := range sets {
			for j, step := range set.steps {
				ss, e := coreexecution.StepSnapshotFromStep(step, set.name)
				if e != nil {
					return coreexecution.ExecutionPlan{}, e
				}
				raw, e := coreexecution.EncodeStepSnapshot(ss)
				if e != nil {
					return coreexecution.ExecutionPlan{}, e
				}
				if _, e = tx.ExecContext(ctx, `INSERT INTO core_execution_plan_steps(owner_id,plan_id,plan_revision,stage_key,step_key,step_set,step_revision,step_digest,ordinal,status,schema_version,snapshot_json) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,'ready','execution-plan/v2',$9)`, in.OwnerID, plan.ID, plan.Revision, stage.StageKey, step.StepKey, set.name, step.Digest, j+1, raw); e != nil {
					return coreexecution.ExecutionPlan{}, e
				}
			}
		}
	}
	for _, ref := range plan.Skills {
		raw, _ := json.Marshal(ref)
		if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_skill_versions(owner_id,id,version,revision,status,schema_version,content_digest,snapshot_json,created_at) VALUES($1,$2,$3,1,'ready','execution-skill/v2',$4,$5,$6) ON CONFLICT (owner_id,id,version) DO NOTHING`, in.OwnerID, ref.ID, ref.Version, ref.Digest, raw, now); err != nil {
			return coreexecution.ExecutionPlan{}, err
		}
		var d string
		if err = tx.QueryRowContext(ctx, `SELECT content_digest FROM core_execution_skill_versions WHERE owner_id=$1 AND id=$2 AND version=$3`, in.OwnerID, ref.ID, ref.Version).Scan(&d); err != nil || d != string(ref.Digest) {
			return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
		}
	}
	for _, ref := range plan.Recipes {
		raw, _ := json.Marshal(ref)
		if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_recipe_versions(owner_id,id,version,revision,status,schema_version,content_digest,snapshot_json,created_at) VALUES($1,$2,$3,1,'ready','execution-recipe/v2',$4,$5,$6) ON CONFLICT (owner_id,id,version) DO NOTHING`, in.OwnerID, ref.ID, ref.Version, ref.Digest, raw, now); err != nil {
			return coreexecution.ExecutionPlan{}, err
		}
		var d string
		if err = tx.QueryRowContext(ctx, `SELECT content_digest FROM core_execution_recipe_versions WHERE owner_id=$1 AND id=$2 AND version=$3`, in.OwnerID, ref.ID, ref.Version).Scan(&d); err != nil || d != string(ref.Digest) {
			return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
		}
	}
	for _, artifact := range plan.Artifacts {
		storageRef := fmt.Sprintf("sha256/%s/%s", string(artifact.Digest)[:2], artifact.Digest)
		if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_artifacts(owner_id,artifact_id,project_id,plan_id,plan_revision,content_digest,storage_backend,storage_ref,size_bytes,media_type,status,schema_version,metadata_json,created_at) VALUES($1,$2,$3,$4,$5,$6,'filesystem',$7,$8,$9,'available','execution-artifact/v2',$10,$11) ON CONFLICT (owner_id,artifact_id) DO NOTHING`, in.OwnerID, artifact.ID, plan.ProjectID, plan.ID, plan.Revision, artifact.Digest, storageRef, artifact.Size, artifact.MediaType, mustJSON(artifact), now); err != nil {
			return coreexecution.ExecutionPlan{}, err
		}
		var d string
		if err = tx.QueryRowContext(ctx, `SELECT content_digest FROM core_execution_artifacts WHERE owner_id=$1 AND artifact_id=$2`, in.OwnerID, artifact.ID).Scan(&d); err != nil || d != string(artifact.Digest) {
			return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
		}
	}
	if err = validateReferencedRows(ctx, tx, in.OwnerID, analysis, plan); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if err = tx.Commit(); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	return plan, nil
}

// RevisePlan appends one immutable plan revision under an exact CAS fence.
// The prior revision is retained and only the head status/revision advances.
func (s *DatabaseExecutionStore) RevisePlan(ctx context.Context, owner string, plan coreexecution.ExecutionPlan, expectedRevision uint64, idempotencyKey string) (coreexecution.ExecutionPlan, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || owner != plan.OwnerID || expectedRevision == 0 || !coreexecution.ValidateUUID(plan.ID) || !coreexecution.ValidateUUID(plan.ProjectID) || !coreexecution.ValidateUUID(plan.AnalysisID) || !coreexecution.ValidateUUID(idempotencyKey) {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreInvalid
	}
	// PostgreSQL timestamptz is microsecond-precise. Seal the response at the
	// same precision as the durable expiry column so restart readback cannot
	// report false snapshot drift.
	now := s.now().UTC().Truncate(time.Microsecond)
	plan.Revision = expectedRevision + 1
	plan.Digest = ""
	plan.Status = coreexecution.PlanReady
	plan.CreatedAt = plan.CreatedAt.UTC().Truncate(time.Microsecond)
	plan.ExpiresAt = plan.ExpiresAt.UTC().Truncate(time.Microsecond)
	normalized, err := plan.NormalizeAt(now)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	plan = normalized
	snap, err := coreexecution.PlanSnapshotFromPlan(plan)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	raw, err := coreexecution.EncodePlanSnapshot(snap)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	requestDigest, err := executionPlanRevisionRequestDigest(owner, plan, expectedRevision)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	responseRaw, err := coreexecution.CanonicalJSON(plan)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	responseDigest, err := coreexecution.CanonicalDigest(plan)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	defer tx.Rollback()
	idem, _ := uuid.Parse(idempotencyKey)
	// Insert first so the unique key serializes concurrent calls. A losing
	// caller then locks and verifies the winner's immutable response instead of
	// attempting the plan mutation again.
	result, err := tx.ExecContext(ctx, `INSERT INTO core_execution_idempotency(owner_id,idempotency_id,key_digest,request_digest,response_digest,status,schema_version,response_json,created_at) VALUES($1,$2,$3,$4,$5,'succeeded','execution-idempotency/v2',$6,$7) ON CONFLICT (owner_id,idempotency_id) DO NOTHING`, owner, idem, string(digestBytes([]byte(idempotencyKey))), string(requestDigest), string(responseDigest), responseRaw, now)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	var oldReq, oldRespDigest, oldResp []byte
	var oldStatus, oldSchema string
	if err = tx.QueryRowContext(ctx, `SELECT request_digest,response_digest,status,schema_version,response_json FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2 FOR UPDATE`, owner, idem).Scan(&oldReq, &oldRespDigest, &oldStatus, &oldSchema, &oldResp); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if !equalDigest(oldReq, []byte(requestDigest)) {
		return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
	}
	replay, replayErr := decodePlanResponse(oldResp)
	replayResponseDigest, replayDigestErr := coreexecution.CanonicalDigest(replay)
	replayRequestDigest, replayRequestErr := executionPlanRevisionRequestDigest(owner, replay, expectedRevision)
	if replayErr != nil || replayDigestErr != nil || replayRequestErr != nil || oldStatus != "succeeded" || oldSchema != "execution-idempotency/v2" || !equalDigest(oldRespDigest, []byte(replayResponseDigest)) || replayRequestDigest != requestDigest || replay.OwnerID != owner || replay.ID != plan.ID || replay.ProjectID != plan.ProjectID || replay.AnalysisID != plan.AnalysisID || replay.Revision != expectedRevision+1 {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreDrift
	}
	if inserted == 0 {
		if err = tx.Commit(); err != nil {
			return coreexecution.ExecutionPlan{}, err
		}
		return replay, nil
	}
	if inserted != 1 || !samePlan(replay, plan) {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreDrift
	}
	current, err := s.getPlan(ctx, tx, owner, plan.ID, expectedRevision, true)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if current.OwnerID != owner || current.ID != plan.ID || current.Revision != expectedRevision || current.ProjectID != plan.ProjectID || current.AnalysisID != plan.AnalysisID {
		return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
	}
	// The head snapshot is an immutable plan-identity envelope; immutable
	// revision snapshots live in core_execution_plan_revisions. Advance only
	// the CAS-protected head revision/status here.
	result, err = tx.ExecContext(ctx, `UPDATE core_execution_plans SET revision=$4,status='ready',updated_at=$5 WHERE owner_id=$1 AND plan_id=$2 AND revision=$3`, owner, plan.ID, expectedRevision, plan.Revision, now)
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	updated, err := result.RowsAffected()
	if err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if updated != 1 {
		return coreexecution.ExecutionPlan{}, coreexecution.ErrConflict
	}
	revID := uuid.NewSHA1(uuid.Nil, []byte(fmt.Sprintf("%s:%d", plan.ID, plan.Revision)))
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_plan_revisions(owner_id,plan_id,plan_revision_id,revision,project_id,analysis_id,status,schema_version,plan_digest,snapshot_json,expires_at,created_at) VALUES($1,$2,$3,$4,$5,$6,'ready','execution-plan/v2',$7,$8,$9,$10)`, owner, plan.ID, revID, plan.Revision, plan.ProjectID, plan.AnalysisID, plan.Digest, raw, plan.ExpiresAt, now); err != nil {
		return coreexecution.ExecutionPlan{}, mapExecutionConflict(err)
	}
	if err = persistRevisedPlanReferences(ctx, tx, owner, plan, now); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	for i, stage := range plan.Stages {
		ss, e := coreexecution.StageSnapshotFromStage(stage)
		if e != nil {
			return coreexecution.ExecutionPlan{}, e
		}
		sr, e := coreexecution.EncodeStageSnapshot(ss)
		if e != nil {
			return coreexecution.ExecutionPlan{}, e
		}
		if _, e = tx.ExecContext(ctx, `INSERT INTO core_execution_plan_stages(owner_id,plan_id,plan_revision,stage_key,stage_revision,stage_digest,ordinal,status,schema_version,snapshot_json) VALUES($1,$2,$3,$4,$5,$6,$7,'ready','execution-plan/v2',$8)`, owner, plan.ID, plan.Revision, stage.StageKey, stage.Revision, stage.Digest, i+1, sr); e != nil {
			return coreexecution.ExecutionPlan{}, e
		}
		sets := []struct {
			name  coreexecution.StepSet
			steps []coreexecution.ExecutionStep
		}{{coreexecution.StepSetForward, stage.Steps}, {coreexecution.StepSetRollback, stage.RollbackSteps}}
		for _, set := range sets {
			for j, step := range set.steps {
				ss, e := coreexecution.StepSnapshotFromStep(step, set.name)
				if e != nil {
					return coreexecution.ExecutionPlan{}, e
				}
				rr, e := coreexecution.EncodeStepSnapshot(ss)
				if e != nil {
					return coreexecution.ExecutionPlan{}, e
				}
				if _, e = tx.ExecContext(ctx, `INSERT INTO core_execution_plan_steps(owner_id,plan_id,plan_revision,stage_key,step_key,step_set,step_revision,step_digest,ordinal,status,schema_version,snapshot_json) VALUES($1,$2,$3,$4,$5,$6,1,$7,$8,'ready','execution-plan/v2',$9)`, owner, plan.ID, plan.Revision, stage.StageKey, step.StepKey, set.name, step.Digest, j+1, rr); e != nil {
					return coreexecution.ExecutionPlan{}, e
				}
			}
		}
	}
	if err = s.validateReferences(ctx, tx, owner, snap); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if err = tx.Commit(); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	return plan, nil
}

// persistRevisedPlanReferences closes the publication gap between writing a
// new immutable plan revision and making its exact target/recipe/artifact
// snapshots resolvable after restart. Executor artifacts are revision-scoped,
// so reusing only the prior revision's rows would make a sealed step
// undispatchable even though RevisePlan returned success.
func persistRevisedPlanReferences(ctx context.Context, tx *sql.Tx, owner string, plan coreexecution.ExecutionPlan, now time.Time) error {
	for _, target := range plan.Targets {
		raw, err := json.Marshal(target)
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_targets(owner_id,target_id,target_revision,status,schema_version,provider,target_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,'active','execution-target/v2',$4,$5,$6,$7,$7) ON CONFLICT (owner_id,target_id,target_revision) DO NOTHING`, owner, target.ID, target.Revision, target.Provider, target.Digest, raw, now); err != nil {
			return err
		}
		var existingOwner, existingProvider, existingDigest string
		var existingRevision uint64
		var existingRaw []byte
		if err = tx.QueryRowContext(ctx, `SELECT owner_id,target_revision,provider,target_digest,snapshot_json FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3`, owner, target.ID, target.Revision).Scan(&existingOwner, &existingRevision, &existingProvider, &existingDigest, &existingRaw); err != nil || existingOwner != owner || existingRevision != target.Revision || existingProvider != target.Provider || existingDigest != string(target.Digest) || !jsonEqual(existingRaw, raw) {
			return coreexecution.ErrConflict
		}
	}
	for _, ref := range plan.Skills {
		raw, _ := json.Marshal(ref)
		if _, err := tx.ExecContext(ctx, `INSERT INTO core_execution_skill_versions(owner_id,id,version,revision,status,schema_version,content_digest,snapshot_json,created_at) VALUES($1,$2,$3,1,'ready','execution-skill/v2',$4,$5,$6) ON CONFLICT (owner_id,id,version) DO NOTHING`, owner, ref.ID, ref.Version, ref.Digest, raw, now); err != nil {
			return err
		}
	}
	for _, ref := range plan.Recipes {
		raw, _ := json.Marshal(ref)
		if _, err := tx.ExecContext(ctx, `INSERT INTO core_execution_recipe_versions(owner_id,id,version,revision,status,schema_version,content_digest,snapshot_json,created_at) VALUES($1,$2,$3,1,'ready','execution-recipe/v2',$4,$5,$6) ON CONFLICT (owner_id,id,version) DO NOTHING`, owner, ref.ID, ref.Version, ref.Digest, raw, now); err != nil {
			return err
		}
	}
	for _, artifact := range plan.Artifacts {
		storageRef := fmt.Sprintf("sha256/%s/%s", string(artifact.Digest)[:2], artifact.Digest)
		metadata := mustJSON(artifact)
		if _, err := tx.ExecContext(ctx, `INSERT INTO core_execution_artifacts(owner_id,artifact_id,project_id,plan_id,plan_revision,content_digest,storage_backend,storage_ref,size_bytes,media_type,status,schema_version,metadata_json,created_at) VALUES($1,$2,$3,$4,$5,$6,'filesystem',$7,$8,$9,'available','execution-artifact/v2',$10,$11) ON CONFLICT (owner_id,artifact_id) DO NOTHING`, owner, artifact.ID, plan.ProjectID, plan.ID, plan.Revision, artifact.Digest, storageRef, artifact.Size, artifact.MediaType, metadata, now); err != nil {
			return err
		}
		got, err := getArtifactTx(ctx, tx, nil, owner, artifact.ID)
		if err != nil || got.OwnerID != owner || got.ProjectID != plan.ProjectID || got.PlanID != plan.ID || got.PlanRevision != plan.Revision || got.RunID != "" || got.AttemptID != "" || got.ContentDigest != artifact.Digest || got.StorageBackend != "filesystem" || got.StorageRef != storageRef || got.SizeBytes != artifact.Size || got.MediaType != artifact.MediaType || got.Revision != 1 || got.Status != "available" || !jsonEqual(got.Metadata, metadata) {
			return coreexecution.ErrConflict
		}
	}
	return nil
}

func (s *DatabaseExecutionStore) GetCurrentPlan(ctx context.Context, owner, planID string) (coreexecution.ExecutionPlan, error) {
	return s.getPlan(ctx, nil, owner, planID, 0, false)
}

func (s *DatabaseExecutionStore) GetPlanRevision(ctx context.Context, owner, planID string, revision uint64) (coreexecution.ExecutionPlan, error) {
	return s.getPlan(ctx, nil, owner, planID, revision, false)
}

// LoadReadyPlanForUpdate is the transaction helper used by CreateRun. The
// caller must commit/rollback tx; no second validation path is needed.
func (s *DatabaseExecutionStore) LoadReadyPlanForUpdate(ctx context.Context, tx *sql.Tx, owner, planID string, revision uint64) (coreexecution.ExecutionPlan, error) {
	if tx == nil {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreInvalid
	}
	return s.getPlan(ctx, tx, owner, planID, revision, true)
}

func (s *DatabaseExecutionStore) getPlan(ctx context.Context, tx *sql.Tx, owner, planID string, revision uint64, lock bool) (coreexecution.ExecutionPlan, error) {
	if strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(planID) {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreInvalid
	}
	q := `SELECT owner_id,plan_id::text,revision,project_id::text,analysis_id::text,status,plan_digest,snapshot_json,expires_at FROM core_execution_plan_revisions WHERE owner_id=$1 AND plan_id=$2`
	args := []any{owner, planID}
	if revision > 0 {
		q += ` AND revision=$3`
		args = append(args, revision)
	} else {
		q += ` ORDER BY revision DESC LIMIT 1`
	}
	if lock {
		q += ` FOR UPDATE`
	}
	row := queryRow(ctx, tx, s.db, q, args...)
	var rowOwner, rowPlan, projectID, analysisID, status, planDigest string
	var rev uint64
	var raw []byte
	var expires time.Time
	if err := row.Scan(&rowOwner, &rowPlan, &rev, &projectID, &analysisID, &status, &planDigest, &raw, &expires); errors.Is(err, sql.ErrNoRows) {
		return coreexecution.ExecutionPlan{}, coreexecution.ErrNotFound
	} else if err != nil {
		return coreexecution.ExecutionPlan{}, err
	} else if status != string(coreexecution.PlanReady) || !expires.After(s.now().UTC()) {
		return coreexecution.ExecutionPlan{}, coreexecution.ErrExpired
	} else if rowOwner != owner || rowPlan != planID || (revision > 0 && rev != revision) {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreDrift
	}
	pSnap, err := decodeStoredPlan(raw)
	if err != nil || pSnap.ID != planID || pSnap.OwnerID != owner || pSnap.ProjectID != projectID || pSnap.AnalysisID != analysisID || pSnap.Revision != rev || string(pSnap.Digest) != planDigest || !pSnap.ExpiresAt.Equal(expires) {
		return coreexecution.ExecutionPlan{}, ErrExecutionStoreDrift
	}
	if err := s.validateChildren(ctx, tx, owner, planID, rev, pSnap); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	if err := s.validateReferences(ctx, tx, owner, pSnap); err != nil {
		return coreexecution.ExecutionPlan{}, err
	}
	p := coreexecution.ExecutionPlan{SchemaVersion: coreexecution.SchemaVersion, ID: pSnap.ID, Revision: pSnap.Revision, OwnerID: pSnap.OwnerID, ProjectID: pSnap.ProjectID, AnalysisID: pSnap.AnalysisID, Purpose: pSnap.Purpose, DeploymentID: pSnap.DeploymentID, AIConfiguration: pSnap.AIConfiguration, Placement: pSnap.Placement, Targets: pSnap.Targets, Artifacts: pSnap.Artifacts, Skills: pSnap.Skills, Recipes: pSnap.Recipes, Stages: pSnap.Stages, Outputs: pSnap.Outputs, CreatedAt: pSnap.CreatedAt, ExpiresAt: pSnap.ExpiresAt, Status: coreexecution.PlanReady, Digest: pSnap.Digest}
	return p, nil
}

func (s *DatabaseExecutionStore) validateReferences(ctx context.Context, tx *sql.Tx, owner string, p coreexecution.PlanSnapshot) error {
	var analysisOwner, analysisProject, analysisStatus, analysisSchema string
	var analysisRevision uint64
	var analysisRaw []byte
	if err := queryRow(ctx, tx, s.db, `SELECT owner_id,project_id::text,revision,status,schema_version,snapshot_json FROM core_execution_analyses WHERE owner_id=$1 AND analysis_id=$2`, owner, p.AnalysisID).Scan(&analysisOwner, &analysisProject, &analysisRevision, &analysisStatus, &analysisSchema, &analysisRaw); err != nil {
		return ErrExecutionStoreDrift
	}
	var storedAnalysis coreexecution.ProjectAnalysis
	if strictJSON(analysisRaw, &storedAnalysis) != nil || analysisOwner != owner || analysisProject != p.ProjectID || analysisRevision == 0 || analysisStatus != "ready" || analysisSchema != "execution-analysis/v2" || storedAnalysis.AnalysisID != p.AnalysisID || storedAnalysis.ProjectID != p.ProjectID {
		return ErrExecutionStoreDrift
	}
	for _, target := range p.Targets {
		var rowOwner, id, provider, status, schema, digest string
		var revision uint64
		var raw []byte
		if err := queryRow(ctx, tx, s.db, `SELECT owner_id,target_id::text,target_revision,provider,status,schema_version,target_digest,snapshot_json FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3`, owner, target.ID, target.Revision).Scan(&rowOwner, &id, &revision, &provider, &status, &schema, &digest, &raw); err != nil {
			return ErrExecutionStoreDrift
		}
		var stored coreexecution.ExecutionTarget
		if strictJSON(raw, &stored) != nil || rowOwner != owner || id != target.ID || revision != target.Revision || provider != target.Provider || status != "active" || schema != "execution-target/v2" || digest != string(target.Digest) || stored.ID != target.ID || stored.Revision != target.Revision || stored.Digest != target.Digest {
			return ErrExecutionStoreDrift
		}
	}
	for _, artifact := range p.Artifacts {
		var rowOwner, id, projectID, planID, digest, status, schema, storageRef string
		var revision, planRevision uint64
		if err := queryRow(ctx, tx, s.db, `SELECT owner_id,artifact_id::text,project_id::text,plan_id::text,plan_revision,revision,content_digest,status,schema_version,storage_ref FROM core_execution_artifacts WHERE owner_id=$1 AND artifact_id=$2`, owner, artifact.ID).Scan(&rowOwner, &id, &projectID, &planID, &planRevision, &revision, &digest, &status, &schema, &storageRef); err != nil {
			return ErrExecutionStoreDrift
		}
		if rowOwner != owner || id != artifact.ID || projectID != p.ProjectID || planID != p.ID || planRevision != p.Revision || revision != 1 || status != "available" || schema != "execution-artifact/v2" || digest != string(artifact.Digest) || !strings.HasSuffix(storageRef, string(artifact.Digest)) {
			return ErrExecutionStoreDrift
		}
	}
	for _, ref := range p.Skills {
		var rowOwner, id, version, status, schema, digest string
		var revision uint64
		var raw []byte
		if err := queryRow(ctx, tx, s.db, `SELECT owner_id,id,version,revision,status,schema_version,content_digest,snapshot_json FROM core_execution_skill_versions WHERE owner_id=$1 AND id=$2 AND version=$3`, owner, ref.ID, ref.Version).Scan(&rowOwner, &id, &version, &revision, &status, &schema, &digest, &raw); err != nil {
			return ErrExecutionStoreDrift
		}
		var stored coreexecution.SkillRef
		if strictJSON(raw, &stored) != nil || rowOwner != owner || id != ref.ID || version != ref.Version || revision != 1 || (status != "active" && status != "ready") || schema != "execution-skill/v2" || digest != string(ref.Digest) || stored != ref {
			return ErrExecutionStoreDrift
		}
	}
	for _, ref := range p.Recipes {
		var rowOwner, id, version, status, schema, digest string
		var revision uint64
		var raw []byte
		if err := queryRow(ctx, tx, s.db, `SELECT owner_id,id,version,revision,status,schema_version,content_digest,snapshot_json FROM core_execution_recipe_versions WHERE owner_id=$1 AND id=$2 AND version=$3`, owner, ref.ID, ref.Version).Scan(&rowOwner, &id, &version, &revision, &status, &schema, &digest, &raw); err != nil {
			return ErrExecutionStoreDrift
		}
		var stored coreexecution.RecipeRef
		if strictJSON(raw, &stored) != nil || rowOwner != owner || id != ref.ID || version != ref.Version || revision != 1 || (status != "active" && status != "ready") || schema != "execution-recipe/v2" || digest != string(ref.Digest) || stored != ref {
			return ErrExecutionStoreDrift
		}
	}
	return nil
}

func validateReferencedRows(ctx context.Context, tx *sql.Tx, owner string, analysis coreexecution.ProjectAnalysis, p coreexecution.ExecutionPlan) error {
	var aOwner, aProject, aSchema, aStatus, aDigest string
	var aRevision uint64
	var aRaw []byte
	if err := tx.QueryRowContext(ctx, `SELECT owner_id,project_id::text,revision,status,schema_version,analysis_digest,snapshot_json FROM core_execution_analyses WHERE owner_id=$1 AND analysis_id=$2`, owner, analysis.AnalysisID).Scan(&aOwner, &aProject, &aRevision, &aStatus, &aSchema, &aDigest, &aRaw); err != nil {
		return fmt.Errorf("%w: analysis query: %v", ErrExecutionStoreDrift, err)
	}
	var storedAnalysis coreexecution.ProjectAnalysis
	if strictJSON(aRaw, &storedAnalysis) != nil || aOwner != owner || aProject != analysis.ProjectID || aRevision != analysis.Revision || aStatus != "ready" || aSchema != "execution-analysis/v2" || aDigest != string(analysis.Digest) || storedAnalysis.Digest != analysis.Digest {
		return fmt.Errorf("%w: analysis fields", ErrExecutionStoreDrift)
	}
	for _, target := range p.Targets {
		var rowOwner, id, provider, status, schema, digest string
		var revision uint64
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT owner_id,target_id::text,target_revision,provider,status,schema_version,target_digest,snapshot_json FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3`, owner, target.ID, target.Revision).Scan(&rowOwner, &id, &revision, &provider, &status, &schema, &digest, &raw); err != nil {
			return fmt.Errorf("%w: target query: %v", ErrExecutionStoreDrift, err)
		}
		var stored coreexecution.ExecutionTarget
		if strictJSON(raw, &stored) != nil || rowOwner != owner || id != target.ID || revision != target.Revision || provider != target.Provider || status != "active" || schema != "execution-target/v2" || digest != string(target.Digest) || stored.Digest != target.Digest {
			return fmt.Errorf("%w: target fields", ErrExecutionStoreDrift)
		}
	}
	for _, ref := range p.Skills {
		var ownerID, id, version, status, schema, digest string
		var revision uint64
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT owner_id,id,version,revision,status,schema_version,content_digest,snapshot_json FROM core_execution_skill_versions WHERE owner_id=$1 AND id=$2 AND version=$3`, owner, ref.ID, ref.Version).Scan(&ownerID, &id, &version, &revision, &status, &schema, &digest, &raw); err != nil {
			return ErrExecutionStoreDrift
		}
		var stored coreexecution.SkillRef
		if strictJSON(raw, &stored) != nil || ownerID != owner || id != ref.ID || version != ref.Version || revision != 1 || (status != "active" && status != "ready") || schema != "execution-skill/v2" || digest != string(ref.Digest) || stored != ref {
			return ErrExecutionStoreDrift
		}
	}
	for _, ref := range p.Recipes {
		var ownerID, id, version, status, schema, digest string
		var revision uint64
		var raw []byte
		if err := tx.QueryRowContext(ctx, `SELECT owner_id,id,version,revision,status,schema_version,content_digest,snapshot_json FROM core_execution_recipe_versions WHERE owner_id=$1 AND id=$2 AND version=$3`, owner, ref.ID, ref.Version).Scan(&ownerID, &id, &version, &revision, &status, &schema, &digest, &raw); err != nil {
			return ErrExecutionStoreDrift
		}
		var stored coreexecution.RecipeRef
		if strictJSON(raw, &stored) != nil || ownerID != owner || id != ref.ID || version != ref.Version || revision != 1 || (status != "active" && status != "ready") || schema != "execution-recipe/v2" || digest != string(ref.Digest) || stored != ref {
			return ErrExecutionStoreDrift
		}
	}
	for _, artifact := range p.Artifacts {
		var ownerID, id, projectID, planID, digest, status, schema string
		var revision, planRevision uint64
		var metadata []byte
		if err := tx.QueryRowContext(ctx, `SELECT owner_id,artifact_id::text,project_id::text,plan_id::text,plan_revision,revision,content_digest,status,schema_version,metadata_json FROM core_execution_artifacts WHERE owner_id=$1 AND artifact_id=$2`, owner, artifact.ID).Scan(&ownerID, &id, &projectID, &planID, &planRevision, &revision, &digest, &status, &schema, &metadata); err != nil {
			return ErrExecutionStoreDrift
		}
		var stored coreexecution.ArtifactRef
		if strictJSON(metadata, &stored) != nil || ownerID != owner || id != artifact.ID || projectID != p.ProjectID || planID != p.ID || planRevision != p.Revision || revision != 1 || digest != string(artifact.Digest) || status != "available" || schema != "execution-artifact/v2" || stored != artifact {
			return ErrExecutionStoreDrift
		}
	}
	return nil
}

func (s *DatabaseExecutionStore) validateChildren(ctx context.Context, tx *sql.Tx, owner, planID string, rev uint64, p coreexecution.PlanSnapshot) error {
	row := queryRow(ctx, tx, s.db, `SELECT COUNT(*) FROM core_execution_plan_stages WHERE owner_id=$1 AND plan_id=$2 AND plan_revision=$3`, owner, planID, rev)
	var count int
	if err := row.Scan(&count); err != nil || count != len(p.Stages) {
		return ErrExecutionStoreDrift
	}
	rows, err := query(ctx, tx, s.db, `SELECT stage_key,stage_revision,stage_digest,ordinal,snapshot_json FROM core_execution_plan_stages WHERE owner_id=$1 AND plan_id=$2 AND plan_revision=$3 ORDER BY ordinal`, owner, planID, rev)
	if err != nil {
		return err
	}
	var stages []coreexecution.StageSnapshot
	for i := 0; rows.Next(); i++ {
		var key, digest string
		var sr, ordinal uint64
		var raw []byte
		if err := rows.Scan(&key, &sr, &digest, &ordinal, &raw); err != nil {
			return err
		}
		if i >= len(p.Stages) || ordinal != uint64(i+1) || p.Stages[i].StageKey != key || p.Stages[i].Revision != sr || string(p.Stages[i].Digest) != digest {
			return ErrExecutionStoreDrift
		}
		ss, err := decodeStoredStage(raw)
		if err != nil || ss.Stage.StageKey != key || ss.Stage.Revision != sr || string(ss.Stage.Digest) != digest {
			return ErrExecutionStoreDrift
		}
		stages = append(stages, ss)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, ss := range stages {
		if err := s.validateSteps(ctx, tx, owner, planID, rev, ss.Stage); err != nil {
			return err
		}
	}
	return nil
}

func (s *DatabaseExecutionStore) validateSteps(ctx context.Context, tx *sql.Tx, owner, planID string, rev uint64, stage coreexecution.ExecutionStage) error {
	rows, err := query(ctx, tx, s.db, `SELECT step_key,step_set,step_revision,step_digest,ordinal,snapshot_json FROM core_execution_plan_steps WHERE owner_id=$1 AND plan_id=$2 AND plan_revision=$3 AND stage_key=$4 ORDER BY step_set,ordinal`, owner, planID, rev, stage.StageKey)
	if err != nil {
		return err
	}
	defer rows.Close()
	sets := map[coreexecution.StepSet][]coreexecution.ExecutionStep{coreexecution.StepSetForward: stage.Steps, coreexecution.StepSetRollback: stage.RollbackSteps}
	seen := map[coreexecution.StepSet]int{}
	for rows.Next() {
		var key, set, digest string
		var sr, ordinal uint64
		var raw []byte
		if err := rows.Scan(&key, &set, &sr, &digest, &ordinal, &raw); err != nil {
			return err
		}
		ss, err := decodeStoredStep(raw)
		if err != nil || string(ss.StepSet) != set || ss.Step.StepKey != key || sr != 1 || string(ss.Step.Digest) != digest {
			return ErrExecutionStoreDrift
		}
		stepSet := coreexecution.StepSet(set)
		expected := sets[stepSet]
		i := seen[stepSet]
		if i >= len(expected) || ordinal != uint64(i+1) || expected[i].StepKey != key || sr != 1 || expected[i].Digest != coreexecution.Digest(digest) {
			return ErrExecutionStoreDrift
		}
		seen[stepSet] = i + 1
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if seen[coreexecution.StepSetForward] != len(stage.Steps) || seen[coreexecution.StepSetRollback] != len(stage.RollbackSteps) {
		return ErrExecutionStoreDrift
	}
	return nil
}

func normalizeForStorage(a coreexecution.ProjectAnalysis, p coreexecution.ExecutionPlan) (coreexecution.ProjectAnalysis, coreexecution.ExecutionPlan, error) {
	// Inputs may be shared by concurrent idempotent callers. Deep-copy before
	// clearing/recomputing digest fields so normalization never mutates caller
	// memory or races with another CreatePlan invocation.
	if raw, err := json.Marshal(a); err == nil {
		var copyA coreexecution.ProjectAnalysis
		if json.Unmarshal(raw, &copyA) == nil {
			a = copyA
		}
	}
	if raw, err := json.Marshal(p); err == nil {
		var copyP coreexecution.ExecutionPlan
		if json.Unmarshal(raw, &copyP) == nil {
			p = copyP
		}
	}
	// PostgreSQL timestamptz has microsecond precision.  Normalize before
	// sealing so the JSON envelope and expiry column remain byte-for-byte
	// consistent after a restart.
	a.CreatedAt = a.CreatedAt.UTC().Truncate(time.Microsecond)
	a.UpdatedAt = a.UpdatedAt.UTC().Truncate(time.Microsecond)
	p.CreatedAt = p.CreatedAt.UTC().Truncate(time.Microsecond)
	p.ExpiresAt = p.ExpiresAt.UTC().Truncate(time.Microsecond)
	a.Digest = ""
	p.Digest = ""
	for i := range p.Targets {
		p.Targets[i].Digest = ""
	}
	for i := range p.Stages {
		clearStageDigests(&p.Stages[i])
	}
	p.Status = coreexecution.PlanReady
	na, err := a.Normalize()
	if err != nil {
		return coreexecution.ProjectAnalysis{}, coreexecution.ExecutionPlan{}, err
	}
	np, err := p.Normalize()
	if err != nil {
		return coreexecution.ProjectAnalysis{}, coreexecution.ExecutionPlan{}, err
	}
	if np.OwnerID == "" || np.ProjectID != na.ProjectID || np.AnalysisID != na.AnalysisID {
		return coreexecution.ProjectAnalysis{}, coreexecution.ExecutionPlan{}, ErrExecutionStoreInvalid
	}
	return na, np, nil
}

func ensurePlanHead(ctx context.Context, tx *sql.Tx, owner string, plan coreexecution.ExecutionPlan, raw []byte, now time.Time) error {
	var project string
	var current uint64
	err := tx.QueryRowContext(ctx, `SELECT project_id::text,revision FROM core_execution_plans WHERE owner_id=$1 AND plan_id=$2 FOR UPDATE`, owner, plan.ID).Scan(&project, &current)
	if errors.Is(err, sql.ErrNoRows) {
		if plan.Revision != 1 {
			return coreexecution.ErrConflict
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO core_execution_plans(owner_id,plan_id,project_id,revision,status,schema_version,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,1,'ready','execution-plan/v2',$4,$5,$5) ON CONFLICT (owner_id,plan_id) DO NOTHING`, owner, plan.ID, plan.ProjectID, raw, now)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n == 1 {
			return nil
		}
		// A concurrent insert may have won after our statement; lock and verify.
		return ensurePlanHead(ctx, tx, owner, plan, raw, now)
	}
	if err != nil {
		return err
	}
	if project != plan.ProjectID || plan.Revision != current+1 {
		return coreexecution.ErrConflict
	}
	res, err := tx.ExecContext(ctx, `UPDATE core_execution_plans SET revision=$4,status='ready',updated_at=$5 WHERE owner_id=$1 AND plan_id=$2 AND revision=$3`, owner, plan.ID, current, plan.Revision, now)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return coreexecution.ErrConflict
	}
	return nil
}

func clearStageDigests(s *coreexecution.ExecutionStage) {
	s.Digest = ""
	for i := range s.Steps {
		s.Steps[i].Digest = ""
	}
	for i := range s.RollbackSteps {
		s.RollbackSteps[i].Digest = ""
	}
}

// executionPlanRevisionRequestDigest identifies the caller's semantic
// revision request. Top-level creation/expiry timestamps are assigned by the
// server while compiling a request and can legitimately differ when the same
// idempotency key is replayed. They, the derived digest, and lifecycle status
// therefore do not participate in the request identity; all executable
// content (including cost-quote expiry, target/secret pins and artifacts)
// remains covered by the canonical digest.
func executionPlanRevisionRequestDigest(owner string, plan coreexecution.ExecutionPlan, expectedRevision uint64) (coreexecution.Digest, error) {
	semantic := plan
	semantic.CreatedAt = time.Time{}
	semantic.ExpiresAt = time.Time{}
	semantic.Status = ""
	semantic.Digest = ""
	return coreexecution.CanonicalDigest(struct {
		Owner            string                      `json:"owner"`
		Plan             coreexecution.ExecutionPlan `json:"plan"`
		ExpectedRevision uint64                      `json:"expected_revision"`
	}{Owner: owner, Plan: semantic, ExpectedRevision: expectedRevision})
}

func decodeStoredPlan(raw []byte) (coreexecution.PlanSnapshot, error) {
	var x coreexecution.PlanSnapshot
	if err := strictJSON(raw, &x); err != nil {
		return x, err
	}
	b, err := coreexecution.EncodePlanSnapshot(x)
	if err != nil {
		return x, err
	}
	return coreexecution.DecodePlanSnapshot(b)
}
func decodeStoredStage(raw []byte) (coreexecution.StageSnapshot, error) {
	var x coreexecution.StageSnapshot
	if err := strictJSON(raw, &x); err != nil {
		return x, err
	}
	b, err := coreexecution.EncodeStageSnapshot(x)
	if err != nil {
		return x, err
	}
	return coreexecution.DecodeStageSnapshot(b)
}
func decodeStoredStep(raw []byte) (coreexecution.StepSnapshot, error) {
	var x coreexecution.StepSnapshot
	if err := strictJSON(raw, &x); err != nil {
		return x, err
	}
	b, err := coreexecution.EncodeStepSnapshot(x)
	if err != nil {
		return x, err
	}
	return coreexecution.DecodeStepSnapshot(b)
}

func decodePlanResponse(raw []byte) (coreexecution.ExecutionPlan, error) {
	var p coreexecution.ExecutionPlan
	if err := strictJSON(raw, &p); err != nil {
		return p, err
	}
	return p.Normalize()
}

func samePlan(a, b coreexecution.ExecutionPlan) bool {
	pa, ea := coreexecution.PlanSnapshotFromPlan(a)
	pb, eb := coreexecution.PlanSnapshotFromPlan(b)
	if ea != nil || eb != nil {
		return false
	}
	ra, ea := coreexecution.EncodePlanSnapshot(pa)
	rb, eb := coreexecution.EncodePlanSnapshot(pb)
	return ea == nil && eb == nil && string(ra) == string(rb)
}
func strictJSON(raw []byte, out any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return ErrExecutionStoreDrift
	}
	var extra any
	if dec.Decode(&extra) == nil {
		return ErrExecutionStoreDrift
	}
	return nil
}
func digestBytes(raw []byte) []byte { d := sha256.Sum256(raw); return []byte(hex.EncodeToString(d[:])) }
func equalDigest(a, b []byte) bool {
	return string(a) == string(b) || fmt.Sprintf("%x", a) == string(b)
}
func mapExecutionConflict(err error) error {
	if err == nil {
		return nil
	}
	if strings.Contains(strings.ToLower(err.Error()), "duplicate key") {
		return coreexecution.ErrConflict
	}
	return err
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

type queryable interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func queryRow(ctx context.Context, tx *sql.Tx, db *sql.DB, q string, args ...any) *sql.Row {
	if tx != nil {
		return tx.QueryRowContext(ctx, q, args...)
	}
	return db.QueryRowContext(ctx, q, args...)
}
func query(ctx context.Context, tx *sql.Tx, db *sql.DB, q string, args ...any) (*sql.Rows, error) {
	if tx != nil {
		return tx.QueryContext(ctx, q, args...)
	}
	return db.QueryContext(ctx, q, args...)
}
