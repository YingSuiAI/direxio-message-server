package storage

// Pure execution/v2 catalog persistence.  The catalog deliberately lives
// beside (rather than inside) the plan/run coordinator: projects, analyses,
// targets and observations are immutable, owner-scoped facts which can be
// safely read by planners and executors without exposing mutable run state.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/google/uuid"
)

type ExecutionProject struct {
	OwnerID   string               `json:"owner_id"`
	ProjectID string               `json:"project_id"`
	Revision  uint64               `json:"revision"`
	Status    string               `json:"status"`
	Digest    coreexecution.Digest `json:"digest"`
	CreatedAt time.Time            `json:"created_at"`
	UpdatedAt time.Time            `json:"updated_at"`
}

type ProjectCreateRequest struct {
	OwnerID       string
	ProjectID     string
	IdempotencyID string
}

type AnalysisCreateRequest struct {
	OwnerID       string
	Analysis      coreexecution.ProjectAnalysis
	IdempotencyID string
}

type TargetCreateRequest struct {
	OwnerID          string
	Target           coreexecution.ExecutionTarget
	ExpectedRevision uint64
	IdempotencyID    string
}

type TargetObservationRecord struct {
	OwnerID       string                          `json:"owner_id"`
	ObservationID string                          `json:"observation_id"`
	Revision      uint64                          `json:"revision"`
	Status        string                          `json:"status"`
	Observation   coreexecution.TargetObservation `json:"observation"`
}

type TargetObservationCreateRequest struct {
	OwnerID       string
	ObservationID string
	Observation   coreexecution.TargetObservation
	IdempotencyID string
}

type ExecutionProjectPage struct {
	Items      []ExecutionProject
	NextCursor string
}

type ExecutionTargetPage struct {
	Items      []coreexecution.ExecutionTarget
	NextCursor string
}

type TargetObservationPage struct {
	Items      []TargetObservationRecord
	NextCursor string
}

func (s *DatabaseExecutionStore) CreateProject(ctx context.Context, in ProjectCreateRequest) (ExecutionProject, error) {
	if s == nil || s.db == nil || strings.TrimSpace(in.OwnerID) == "" || !coreexecution.ValidateUUID(in.ProjectID) {
		return ExecutionProject{}, ErrExecutionStoreInvalid
	}
	idem, err := parseCatalogIdempotency(in.IdempotencyID)
	if err != nil {
		return ExecutionProject{}, err
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	project := ExecutionProject{OwnerID: in.OwnerID, ProjectID: in.ProjectID, Revision: 1, Status: "active", CreatedAt: now, UpdatedAt: now}
	snap, err := catalogProjectSnapshot(project)
	if err != nil {
		return ExecutionProject{}, err
	}
	project.Digest = coreexecution.Digest(digestBytes(snap))
	response, err := json.Marshal(project)
	if err != nil {
		return ExecutionProject{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionProject{}, err
	}
	defer tx.Rollback()
	inserted, old, err := catalogIdempotency(ctx, tx, in.OwnerID, idem, snap, response, now)
	if err != nil {
		return ExecutionProject{}, err
	}
	if !inserted {
		var replay ExecutionProject
		if err := strictJSON(old, &replay); err != nil || replay.OwnerID != in.OwnerID || replay.ProjectID != in.ProjectID {
			return ExecutionProject{}, ErrExecutionStoreDrift
		}
		if err := tx.Commit(); err != nil {
			return ExecutionProject{}, err
		}
		return replay, nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_projects(owner_id,project_id,revision,status,schema_version,project_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,1,'active','execution-project/v2',$3,$4,$5,$5) ON CONFLICT (owner_id,project_id) DO NOTHING`, in.OwnerID, in.ProjectID, project.Digest, snap, now); err != nil {
		return ExecutionProject{}, err
	}
	stored, err := loadProjectTx(ctx, s.db, tx, in.OwnerID, in.ProjectID, true)
	if err != nil {
		return ExecutionProject{}, err
	}
	if stored.Digest != project.Digest {
		return ExecutionProject{}, coreexecution.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return ExecutionProject{}, err
	}
	return stored, nil
}

func (s *DatabaseExecutionStore) GetProject(ctx context.Context, owner, projectID string) (ExecutionProject, error) {
	if strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(projectID) {
		return ExecutionProject{}, ErrExecutionStoreInvalid
	}
	return loadProjectTx(ctx, s.db, nil, owner, projectID, false)
}

func (s *DatabaseExecutionStore) ArchiveProject(ctx context.Context, owner, projectID string, expectedRevision uint64) (ExecutionProject, error) {
	if strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(projectID) || expectedRevision == 0 {
		return ExecutionProject{}, ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ExecutionProject{}, err
	}
	defer tx.Rollback()
	current, err := loadProjectTx(ctx, s.db, tx, owner, projectID, true)
	if err != nil {
		return ExecutionProject{}, err
	}
	if current.Revision != expectedRevision || current.Status != "active" {
		return ExecutionProject{}, coreexecution.ErrConflict
	}
	result, err := tx.ExecContext(ctx, `UPDATE core_execution_projects SET revision=revision+1,status='archived',updated_at=$4 WHERE owner_id=$1 AND project_id=$2 AND revision=$3 AND status='active'`, owner, projectID, expectedRevision, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return ExecutionProject{}, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return ExecutionProject{}, coreexecution.ErrConflict
	}
	updated, err := loadProjectTx(ctx, s.db, tx, owner, projectID, true)
	if err != nil {
		return ExecutionProject{}, err
	}
	if updated.Revision != expectedRevision+1 || updated.Status != "archived" {
		return ExecutionProject{}, ErrExecutionStoreDrift
	}
	if err := tx.Commit(); err != nil {
		return ExecutionProject{}, err
	}
	return updated, nil
}

func (s *DatabaseExecutionStore) ListProjects(ctx context.Context, owner, afterProjectID string, limit int) (ExecutionProjectPage, error) {
	if strings.TrimSpace(owner) == "" || limit < 0 || limit > 200 {
		return ExecutionProjectPage{}, ErrExecutionStoreInvalid
	}
	if afterProjectID != "" && !coreexecution.ValidateUUID(afterProjectID) {
		return ExecutionProjectPage{}, ErrExecutionStoreInvalid
	}
	q := `SELECT owner_id,project_id::text,revision,status,project_digest,snapshot_json,created_at,updated_at FROM core_execution_projects WHERE owner_id=$1`
	args := []any{owner}
	if afterProjectID != "" {
		q += ` AND project_id::text>$2`
		args = append(args, afterProjectID)
	}
	q += ` ORDER BY project_id LIMIT $` + fmt.Sprint(len(args)+1)
	if limit == 0 {
		limit = 50
	}
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return ExecutionProjectPage{}, err
	}
	defer rows.Close()
	page := ExecutionProjectPage{}
	for rows.Next() {
		p, err := scanProject(rows)
		if err != nil {
			return ExecutionProjectPage{}, err
		}
		page.Items = append(page.Items, p)
	}
	if err := rows.Err(); err != nil {
		return ExecutionProjectPage{}, err
	}
	if len(page.Items) > limit {
		page.NextCursor = page.Items[limit-1].ProjectID
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func (s *DatabaseExecutionStore) CreateAnalysis(ctx context.Context, in AnalysisCreateRequest) (coreexecution.ProjectAnalysis, error) {
	if s == nil || s.db == nil || strings.TrimSpace(in.OwnerID) == "" || !coreexecution.ValidateUUID(in.Analysis.AnalysisID) || !coreexecution.ValidateUUID(in.Analysis.ProjectID) {
		return coreexecution.ProjectAnalysis{}, ErrExecutionStoreInvalid
	}
	idem, err := parseCatalogIdempotency(in.IdempotencyID)
	if err != nil {
		return coreexecution.ProjectAnalysis{}, err
	}
	requestA := in.Analysis
	requestA.CreatedAt = time.Time{}
	requestA.UpdatedAt = time.Time{}
	requestA.Digest = ""
	requestA.Revision = 1
	requestRaw, err := json.Marshal(requestA)
	if err != nil {
		return coreexecution.ProjectAnalysis{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return coreexecution.ProjectAnalysis{}, err
	}
	defer tx.Rollback()
	// Resolve a replay before inspecting mutable project state or assigning
	// server timestamps. Otherwise the current clock changes the analysis
	// digest and a retry of a successful request is falsely rejected.
	if old, ok, replayErr := catalogReplayResponse(ctx, tx, in.OwnerID, idem); replayErr != nil {
		return coreexecution.ProjectAnalysis{}, replayErr
	} else if ok {
		inserted, storedRaw, idemErr := catalogIdempotency(ctx, tx, in.OwnerID, idem, requestRaw, old, s.now().UTC().Truncate(time.Microsecond))
		if idemErr != nil {
			return coreexecution.ProjectAnalysis{}, idemErr
		}
		var replay coreexecution.ProjectAnalysis
		if inserted || strictJSON(storedRaw, &replay) != nil || replay.AnalysisID != requestA.AnalysisID || replay.ProjectID != requestA.ProjectID {
			return coreexecution.ProjectAnalysis{}, ErrExecutionStoreDrift
		}
		normalized, normalizeErr := replay.Normalize()
		if normalizeErr != nil || normalized.Digest != replay.Digest {
			return coreexecution.ProjectAnalysis{}, ErrExecutionStoreDrift
		}
		if err := tx.Commit(); err != nil {
			return coreexecution.ProjectAnalysis{}, err
		}
		return normalized, nil
	}
	now := s.now().UTC().Truncate(time.Microsecond)
	project, projectErr := ensureActiveProjectTx(ctx, s.db, tx, in.OwnerID, requestA.ProjectID, now)
	if projectErr != nil {
		return coreexecution.ProjectAnalysis{}, projectErr
	}
	a := in.Analysis
	a.CreatedAt, a.UpdatedAt, a.Digest, a.Revision = now, now, "", 1
	a, err = a.Normalize()
	if err != nil {
		return coreexecution.ProjectAnalysis{}, err
	}
	raw, err := json.Marshal(a)
	if err != nil {
		return coreexecution.ProjectAnalysis{}, err
	}
	inserted, old, err := catalogIdempotency(ctx, tx, in.OwnerID, idem, requestRaw, raw, now)
	if err != nil {
		return coreexecution.ProjectAnalysis{}, err
	}
	if !inserted {
		var replay coreexecution.ProjectAnalysis
		if err := strictJSON(old, &replay); err != nil || replay.AnalysisID != requestA.AnalysisID || replay.ProjectID != requestA.ProjectID {
			return coreexecution.ProjectAnalysis{}, ErrExecutionStoreDrift
		}
		normalized, normalizeErr := replay.Normalize()
		if normalizeErr != nil || normalized.Digest != replay.Digest {
			return coreexecution.ProjectAnalysis{}, ErrExecutionStoreDrift
		}
		if err := tx.Commit(); err != nil {
			return coreexecution.ProjectAnalysis{}, err
		}
		return normalized, nil
	}
	if project.Status != "active" {
		return coreexecution.ProjectAnalysis{}, coreexecution.ErrConflict
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_analyses(owner_id,analysis_id,project_id,revision,status,schema_version,analysis_digest,snapshot_json,created_at) VALUES($1,$2,$3,1,'ready','execution-analysis/v2',$4,$5,$6) ON CONFLICT (owner_id,analysis_id) DO NOTHING`, in.OwnerID, a.AnalysisID, a.ProjectID, a.Digest, raw, a.CreatedAt); err != nil {
		return coreexecution.ProjectAnalysis{}, err
	}
	var stored coreexecution.ProjectAnalysis
	if err = tx.QueryRowContext(ctx, `SELECT snapshot_json FROM core_execution_analyses WHERE owner_id=$1 AND analysis_id=$2`, in.OwnerID, a.AnalysisID).Scan(&raw); err != nil || strictJSON(raw, &stored) != nil {
		return coreexecution.ProjectAnalysis{}, ErrExecutionStoreDrift
	}
	if stored.Digest != a.Digest {
		return coreexecution.ProjectAnalysis{}, coreexecution.ErrConflict
	}
	if err := tx.Commit(); err != nil {
		return coreexecution.ProjectAnalysis{}, err
	}
	return stored, nil
}

func (s *DatabaseExecutionStore) GetAnalysis(ctx context.Context, owner, analysisID string) (coreexecution.ProjectAnalysis, error) {
	if strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(analysisID) {
		return coreexecution.ProjectAnalysis{}, ErrExecutionStoreInvalid
	}
	var raw []byte
	var rowOwner, projectID, status, schema, digest string
	var revision uint64
	if err := s.db.QueryRowContext(ctx, `SELECT owner_id,project_id::text,revision,status,schema_version,analysis_digest,snapshot_json FROM core_execution_analyses WHERE owner_id=$1 AND analysis_id=$2`, owner, analysisID).Scan(&rowOwner, &projectID, &revision, &status, &schema, &digest, &raw); errors.Is(err, sql.ErrNoRows) {
		return coreexecution.ProjectAnalysis{}, coreexecution.ErrNotFound
	} else if err != nil {
		return coreexecution.ProjectAnalysis{}, err
	}
	return validateAnalysisRow(raw, rowOwner, owner, projectID, analysisID, revision, status, schema, digest)
}

func (s *DatabaseExecutionStore) CreateTarget(ctx context.Context, in TargetCreateRequest) (coreexecution.ExecutionTarget, error) {
	if s == nil || s.db == nil || strings.TrimSpace(in.OwnerID) == "" || !coreexecution.ValidateUUID(in.Target.ID) {
		return coreexecution.ExecutionTarget{}, ErrExecutionStoreInvalid
	}
	idem, err := parseCatalogIdempotency(in.IdempotencyID)
	if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	defer tx.Rollback()
	// Replay before consulting the latest mutable revision. A retry of the
	// original create must return revision 1 instead of being misclassified as
	// a stale attempt to create revision 2.
	if old, ok, replayErr := catalogReplayResponse(ctx, tx, in.OwnerID, idem); replayErr != nil {
		return coreexecution.ExecutionTarget{}, replayErr
	} else if ok {
		var replay coreexecution.ExecutionTarget
		if strictJSON(old, &replay) != nil || replay.ID != in.Target.ID || replay.Revision == 0 {
			return coreexecution.ExecutionTarget{}, ErrExecutionStoreDrift
		}
		candidate := in.Target
		candidate.Revision = replay.Revision
		candidate.Digest = ""
		candidate, err = candidate.Normalize()
		if err != nil {
			return coreexecution.ExecutionTarget{}, err
		}
		candidateRaw, marshalErr := json.Marshal(candidate)
		if marshalErr != nil {
			return coreexecution.ExecutionTarget{}, marshalErr
		}
		requestRaw, marshalErr := json.Marshal(struct {
			ExpectedRevision uint64                        `json:"expected_revision"`
			Target           coreexecution.ExecutionTarget `json:"target"`
		}{ExpectedRevision: in.ExpectedRevision, Target: candidate})
		if marshalErr != nil {
			return coreexecution.ExecutionTarget{}, marshalErr
		}
		inserted, storedRaw, idemErr := catalogIdempotency(ctx, tx, in.OwnerID, idem, requestRaw, candidateRaw, s.now().UTC().Truncate(time.Microsecond))
		if idemErr != nil {
			return coreexecution.ExecutionTarget{}, idemErr
		}
		if inserted || strictJSON(storedRaw, &replay) != nil || replay.Digest != candidate.Digest || replay.Revision != candidate.Revision {
			return coreexecution.ExecutionTarget{}, ErrExecutionStoreDrift
		}
		if err := tx.Commit(); err != nil {
			return coreexecution.ExecutionTarget{}, err
		}
		return replay, nil
	}
	var current uint64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(target_revision),0) FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2`, in.OwnerID, in.Target.ID).Scan(&current)
	if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	if current == 0 {
		if in.ExpectedRevision != 0 && in.ExpectedRevision != 1 {
			return coreexecution.ExecutionTarget{}, coreexecution.ErrConflict
		}
		in.Target.Revision = 1
	} else {
		if in.ExpectedRevision != current {
			return coreexecution.ExecutionTarget{}, coreexecution.ErrConflict
		}
		in.Target.Revision = current + 1
	}
	in.Target.Digest = ""
	target, err := in.Target.Normalize()
	if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	raw, err := json.Marshal(target)
	if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	requestRaw, err := json.Marshal(struct {
		ExpectedRevision uint64                        `json:"expected_revision"`
		Target           coreexecution.ExecutionTarget `json:"target"`
	}{ExpectedRevision: in.ExpectedRevision, Target: target})
	if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	inserted, old, err := catalogIdempotency(ctx, tx, in.OwnerID, idem, requestRaw, raw, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	if !inserted {
		var replay coreexecution.ExecutionTarget
		if strictJSON(old, &replay) != nil || replay.ID != target.ID || replay.Revision != target.Revision || replay.Digest != target.Digest {
			return coreexecution.ExecutionTarget{}, ErrExecutionStoreDrift
		}
		if err := tx.Commit(); err != nil {
			return coreexecution.ExecutionTarget{}, err
		}
		return replay, nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_targets(owner_id,target_id,target_revision,status,schema_version,provider,target_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,'active','execution-target/v2',$4,$5,$6,$7,$7)`, in.OwnerID, target.ID, target.Revision, target.Provider, target.Digest, raw, s.now().UTC().Truncate(time.Microsecond)); err != nil {
		return coreexecution.ExecutionTarget{}, mapExecutionConflict(err)
	}
	if err := tx.Commit(); err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	return target, nil
}

func (s *DatabaseExecutionStore) GetTarget(ctx context.Context, owner, targetID string, revision uint64) (coreexecution.ExecutionTarget, error) {
	if strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(targetID) {
		return coreexecution.ExecutionTarget{}, ErrExecutionStoreInvalid
	}
	q := `SELECT owner_id,target_id::text,target_revision,status,schema_version,target_digest,snapshot_json FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2`
	args := []any{owner, targetID}
	if revision > 0 {
		q += ` AND target_revision=$3`
		args = append(args, revision)
	} else {
		q += ` ORDER BY target_revision DESC LIMIT 1`
	}
	var rowOwner, id, status, schema, digest string
	var rev uint64
	var raw []byte
	if err := s.db.QueryRowContext(ctx, q, args...).Scan(&rowOwner, &id, &rev, &status, &schema, &digest, &raw); errors.Is(err, sql.ErrNoRows) {
		return coreexecution.ExecutionTarget{}, coreexecution.ErrNotFound
	} else if err != nil {
		return coreexecution.ExecutionTarget{}, err
	}
	return validateTargetRow(raw, rowOwner, owner, id, targetID, rev, status, schema, digest)
}

func (s *DatabaseExecutionStore) ListTargets(ctx context.Context, owner, afterTargetID string, limit int) (ExecutionTargetPage, error) {
	if strings.TrimSpace(owner) == "" || limit < 0 || limit > 200 || afterTargetID != "" && !coreexecution.ValidateUUID(afterTargetID) {
		return ExecutionTargetPage{}, ErrExecutionStoreInvalid
	}
	if limit == 0 {
		limit = 50
	}
	q := `SELECT DISTINCT ON (target_id) owner_id,target_id::text,target_revision,status,schema_version,target_digest,snapshot_json FROM core_execution_targets WHERE owner_id=$1`
	args := []any{owner}
	if afterTargetID != "" {
		q += ` AND target_id::text>$2`
		args = append(args, afterTargetID)
	}
	q += ` ORDER BY target_id,target_revision DESC`
	q = `SELECT owner_id,target_id,target_revision,status,schema_version,target_digest,snapshot_json FROM (` + q + `) q ORDER BY target_id LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return ExecutionTargetPage{}, err
	}
	defer rows.Close()
	page := ExecutionTargetPage{}
	for rows.Next() {
		var ownerID, id, status, schema, digest string
		var rev uint64
		var raw []byte
		if err := rows.Scan(&ownerID, &id, &rev, &status, &schema, &digest, &raw); err != nil {
			return ExecutionTargetPage{}, err
		}
		t, err := validateTargetRow(raw, ownerID, owner, id, id, rev, status, schema, digest)
		if err != nil {
			return ExecutionTargetPage{}, err
		}
		page.Items = append(page.Items, t)
	}
	if err := rows.Err(); err != nil {
		return ExecutionTargetPage{}, err
	}
	if len(page.Items) > limit {
		page.NextCursor = page.Items[limit-1].ID
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func (s *DatabaseExecutionStore) CreateTargetObservation(ctx context.Context, in TargetObservationCreateRequest) (TargetObservationRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(in.OwnerID) == "" || !coreexecution.ValidateUUID(in.ObservationID) {
		return TargetObservationRecord{}, ErrExecutionStoreInvalid
	}
	idem, err := parseCatalogIdempotency(in.IdempotencyID)
	if err != nil {
		return TargetObservationRecord{}, err
	}
	obs := in.Observation
	obs.ObservedAt = obs.ObservedAt.UTC().Truncate(time.Microsecond)
	obs.Digest = ""
	if err := validateCatalogSensitiveData(obs); err != nil {
		return TargetObservationRecord{}, err
	}
	obs, err = obs.Normalize()
	if err != nil {
		return TargetObservationRecord{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return TargetObservationRecord{}, err
	}
	defer tx.Rollback()
	if old, ok, replayErr := catalogReplayResponse(ctx, tx, in.OwnerID, idem); replayErr != nil {
		return TargetObservationRecord{}, replayErr
	} else if ok {
		var replay TargetObservationRecord
		if strictJSON(old, &replay) != nil || replay.OwnerID != in.OwnerID || replay.ObservationID != in.ObservationID || replay.Observation.Digest != obs.Digest || replay.Observation.TargetID != obs.TargetID || replay.Observation.TargetRevision != obs.TargetRevision {
			return TargetObservationRecord{}, coreexecution.ErrConflict
		}
		if err := tx.Commit(); err != nil {
			return TargetObservationRecord{}, err
		}
		return replay, nil
	}
	var current uint64
	var targetStatus string
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(target_revision),0),COALESCE((SELECT status FROM core_execution_targets t2 WHERE t2.owner_id=$1 AND t2.target_id=$2 ORDER BY t2.target_revision DESC LIMIT 1),'') FROM core_execution_targets WHERE owner_id=$1 AND target_id=$2`, in.OwnerID, obs.TargetID).Scan(&current, &targetStatus); err != nil {
		return TargetObservationRecord{}, err
	}
	if current == 0 || current != obs.TargetRevision || targetStatus != "active" {
		return TargetObservationRecord{}, coreexecution.ErrConflict
	}
	raw, err := json.Marshal(obs)
	if err != nil {
		return TargetObservationRecord{}, err
	}
	record := TargetObservationRecord{OwnerID: in.OwnerID, ObservationID: in.ObservationID, Revision: 1, Status: "observed", Observation: obs}
	if obs.Partial && (obs.State == "unavailable" || obs.State == "error") {
		record.Status = "failed"
	}
	response, _ := json.Marshal(record)
	inserted, old, err := catalogIdempotency(ctx, tx, in.OwnerID, idem, raw, response, s.now().UTC().Truncate(time.Microsecond))
	if err != nil {
		return TargetObservationRecord{}, err
	}
	if !inserted {
		var replay TargetObservationRecord
		if strictJSON(old, &replay) != nil || replay.OwnerID != in.OwnerID || replay.ObservationID != in.ObservationID || replay.Observation.Digest != obs.Digest {
			return TargetObservationRecord{}, ErrExecutionStoreDrift
		}
		if err := tx.Commit(); err != nil {
			return TargetObservationRecord{}, err
		}
		return replay, nil
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_target_observations(owner_id,observation_id,target_id,target_revision,revision,status,schema_version,observation_digest,snapshot_json,observed_at) VALUES($1,$2,$3,$4,1,$5,'execution-observation/v2',$6,$7,$8)`, in.OwnerID, in.ObservationID, obs.TargetID, obs.TargetRevision, record.Status, obs.Digest, raw, obs.ObservedAt); err != nil {
		return TargetObservationRecord{}, mapExecutionConflict(err)
	}
	if err := tx.Commit(); err != nil {
		return TargetObservationRecord{}, err
	}
	return record, nil
}

// GetTargetObservationByIdempotency returns the immutable observation response
// recorded for an idempotency key without touching the provider. The key is
// owner-scoped and the response is verified against the canonical digest kept
// in core_execution_idempotency before it is returned.
func (s *DatabaseExecutionStore) GetTargetObservationByIdempotency(ctx context.Context, owner, idempotencyID string) (TargetObservationRecord, bool, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" {
		return TargetObservationRecord{}, false, ErrExecutionStoreInvalid
	}
	id, err := parseCatalogIdempotency(idempotencyID)
	if err != nil {
		return TargetObservationRecord{}, false, err
	}
	var status, schema string
	var responseDigest []byte
	var response []byte
	err = s.db.QueryRowContext(ctx, `SELECT status,schema_version,response_digest,response_json FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2`, owner, id).Scan(&status, &schema, &responseDigest, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return TargetObservationRecord{}, false, nil
	}
	if err != nil {
		return TargetObservationRecord{}, false, err
	}
	if status != "succeeded" || schema != "execution-idempotency/v2" {
		return TargetObservationRecord{}, false, coreexecution.ErrConflict
	}
	canonical, err := canonicalCatalogJSON(response)
	if err != nil || string(responseDigest) != string(digestBytes(canonical)) {
		return TargetObservationRecord{}, false, ErrExecutionStoreDrift
	}
	var record TargetObservationRecord
	if strictJSON(response, &record) != nil || record.OwnerID != owner || !coreexecution.ValidateUUID(record.ObservationID) || record.Revision != 1 || (record.Status != "observed" && record.Status != "failed") {
		return TargetObservationRecord{}, false, ErrExecutionStoreDrift
	}
	validated, err := record.Observation.Normalize()
	if err != nil || validated.Digest != record.Observation.Digest || validateCatalogSensitiveData(record.Observation) != nil {
		return TargetObservationRecord{}, false, ErrExecutionStoreDrift
	}
	record.Observation = validated
	return record, true, nil
}

func (s *DatabaseExecutionStore) GetTargetObservation(ctx context.Context, owner, observationID string) (TargetObservationRecord, error) {
	if strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(observationID) {
		return TargetObservationRecord{}, ErrExecutionStoreInvalid
	}
	var rowOwner, id, status, schema, digest string
	var rev, targetRev uint64
	var targetID string
	var observedAt time.Time
	var raw []byte
	if err := s.db.QueryRowContext(ctx, `SELECT owner_id,observation_id::text,target_id::text,target_revision,revision,status,schema_version,observation_digest,snapshot_json,observed_at FROM core_execution_target_observations WHERE owner_id=$1 AND observation_id=$2`, owner, observationID).Scan(&rowOwner, &id, &targetID, &targetRev, &rev, &status, &schema, &digest, &raw, &observedAt); errors.Is(err, sql.ErrNoRows) {
		return TargetObservationRecord{}, coreexecution.ErrNotFound
	} else if err != nil {
		return TargetObservationRecord{}, err
	}
	return validateObservationRow(raw, rowOwner, owner, id, observationID, targetID, targetRev, rev, status, schema, digest, observedAt)
}

// GetLatestReadyTargetObservation returns the newest immutable ready
// observation for an exact target revision. UUID ordering is not a temporal
// ordering, so planning code must use this method rather than the paginated
// catalog list when binding an execution plan.
func (s *DatabaseExecutionStore) GetLatestReadyTargetObservation(ctx context.Context, owner, targetID string, targetRevision uint64) (TargetObservationRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(targetID) || targetRevision == 0 {
		return TargetObservationRecord{}, ErrExecutionStoreInvalid
	}
	var rowOwner, id, status, schema, digest, rowTargetID string
	var revision, rowTargetRevision uint64
	var observedAt time.Time
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT owner_id,observation_id::text,target_id::text,target_revision,revision,status,schema_version,observation_digest,snapshot_json,observed_at FROM core_execution_target_observations WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3 AND status='observed' ORDER BY observed_at DESC,observation_id DESC LIMIT 1`, owner, targetID, targetRevision).Scan(&rowOwner, &id, &rowTargetID, &rowTargetRevision, &revision, &status, &schema, &digest, &raw, &observedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return TargetObservationRecord{}, coreexecution.ErrNotFound
	}
	if err != nil {
		return TargetObservationRecord{}, err
	}
	return validateObservationRow(raw, rowOwner, owner, id, id, rowTargetID, rowTargetRevision, revision, status, schema, digest, observedAt)
}

func (s *DatabaseExecutionStore) ListTargetObservations(ctx context.Context, owner, targetID string, targetRevision uint64, afterObservationID string, limit int) (TargetObservationPage, error) {
	if strings.TrimSpace(owner) == "" || !coreexecution.ValidateUUID(targetID) || targetRevision == 0 || limit < 0 || limit > 200 || afterObservationID != "" && !coreexecution.ValidateUUID(afterObservationID) {
		return TargetObservationPage{}, ErrExecutionStoreInvalid
	}
	if limit == 0 {
		limit = 50
	}
	q := `SELECT owner_id,observation_id::text,target_id::text,target_revision,revision,status,schema_version,observation_digest,snapshot_json,observed_at FROM core_execution_target_observations WHERE owner_id=$1 AND target_id=$2 AND target_revision=$3`
	args := []any{owner, targetID, targetRevision}
	if afterObservationID != "" {
		q += ` AND observation_id::text>$4`
		args = append(args, afterObservationID)
	}
	q += ` ORDER BY observation_id LIMIT $` + fmt.Sprint(len(args)+1)
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return TargetObservationPage{}, err
	}
	defer rows.Close()
	page := TargetObservationPage{}
	for rows.Next() {
		var rowOwner, id, idTarget, status, schema, digest string
		var targetRev, rev uint64
		var observedAt time.Time
		var raw []byte
		if err := rows.Scan(&rowOwner, &id, &idTarget, &targetRev, &rev, &status, &schema, &digest, &raw, &observedAt); err != nil {
			return TargetObservationPage{}, err
		}
		r, err := validateObservationRow(raw, rowOwner, owner, id, id, idTarget, targetRev, rev, status, schema, digest, observedAt)
		if err != nil {
			return TargetObservationPage{}, err
		}
		page.Items = append(page.Items, r)
	}
	if err := rows.Err(); err != nil {
		return TargetObservationPage{}, err
	}
	if len(page.Items) > limit {
		page.NextCursor = page.Items[limit-1].ObservationID
		page.Items = page.Items[:limit]
	}
	return page, nil
}

type catalogRowScanner interface{ Scan(...any) error }

func scanProject(row catalogRowScanner) (ExecutionProject, error) {
	var p ExecutionProject
	var owner, id, status, digest string
	var raw []byte
	if err := row.Scan(&owner, &id, &p.Revision, &status, &digest, &raw, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return p, err
	}
	return validateProjectRow(raw, owner, owner, id, p.Revision, status, digest, p.CreatedAt, p.UpdatedAt)
}

func loadProjectTx(ctx context.Context, db *sql.DB, tx *sql.Tx, owner, projectID string, lock bool) (ExecutionProject, error) {
	q := `SELECT owner_id,project_id::text,revision,status,project_digest,snapshot_json,created_at,updated_at FROM core_execution_projects WHERE owner_id=$1 AND project_id=$2`
	if lock {
		q += ` FOR UPDATE`
	}
	row := queryRow(ctx, tx, db, q, owner, projectID)
	var p ExecutionProject
	var rowOwner, id, status, digest string
	var raw []byte
	if err := row.Scan(&rowOwner, &id, &p.Revision, &status, &digest, &raw, &p.CreatedAt, &p.UpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return ExecutionProject{}, coreexecution.ErrNotFound
	} else if err != nil {
		return ExecutionProject{}, err
	}
	return validateProjectRow(raw, rowOwner, owner, id, p.Revision, status, digest, p.CreatedAt, p.UpdatedAt)
}

// ensureActiveProjectTx bootstraps the stable project identity as part of the
// first successful analysis transaction. Project creation has no independent
// provider side effect, so this removes the otherwise unreachable
// projects.analyze prerequisite without introducing a second public action.
func ensureActiveProjectTx(ctx context.Context, db *sql.DB, tx *sql.Tx, owner, projectID string, now time.Time) (ExecutionProject, error) {
	project := ExecutionProject{
		OwnerID: owner, ProjectID: projectID, Revision: 1, Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}
	snapshot, err := catalogProjectSnapshot(project)
	if err != nil {
		return ExecutionProject{}, err
	}
	project.Digest = coreexecution.Digest(digestBytes(snapshot))
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_projects(owner_id,project_id,revision,status,schema_version,project_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,1,'active','execution-project/v2',$3,$4,$5,$5) ON CONFLICT (owner_id,project_id) DO NOTHING`, owner, projectID, project.Digest, snapshot, now); err != nil {
		return ExecutionProject{}, err
	}
	stored, err := loadProjectTx(ctx, db, tx, owner, projectID, true)
	if err != nil {
		return ExecutionProject{}, err
	}
	if stored.Status != "active" {
		return ExecutionProject{}, coreexecution.ErrConflict
	}
	return stored, nil
}

func validateProjectRow(raw []byte, rowOwner, owner, id string, revision uint64, status, digest string, created, updated time.Time) (ExecutionProject, error) {
	var snap struct {
		SchemaVersion string `json:"schema_version"`
		ProjectID     string `json:"project_id"`
		OwnerID       string `json:"owner_id"`
	}
	if strictJSON(raw, &snap) != nil || snap.SchemaVersion != "execution-project/v2" || snap.OwnerID != owner || rowOwner != owner || snap.ProjectID != id || !coreexecution.ValidateUUID(id) || revision == 0 || (status != "active" && status != "archived") || !created.Equal(created.UTC()) || !updated.Equal(updated.UTC()) || updated.Before(created) {
		return ExecutionProject{}, ErrExecutionStoreDrift
	}
	canonical, _ := json.Marshal(snap)
	if string(digestBytes(canonical)) != digest {
		return ExecutionProject{}, ErrExecutionStoreDrift
	}
	return ExecutionProject{OwnerID: owner, ProjectID: id, Revision: revision, Status: status, Digest: coreexecution.Digest(digest), CreatedAt: created.UTC(), UpdatedAt: updated.UTC()}, nil
}

func validateAnalysisRow(raw []byte, rowOwner, owner, projectID, analysisID string, revision uint64, status, schema, digest string) (coreexecution.ProjectAnalysis, error) {
	var a coreexecution.ProjectAnalysis
	if strictJSON(raw, &a) != nil || rowOwner != owner || a.ProjectID != projectID || a.AnalysisID != analysisID || !coreexecution.ValidateUUID(projectID) || !coreexecution.ValidateUUID(analysisID) || revision != a.Revision || status != "ready" || schema != "execution-analysis/v2" || digest != string(a.Digest) {
		return coreexecution.ProjectAnalysis{}, ErrExecutionStoreDrift
	}
	n, err := a.Normalize()
	if err != nil || n.Digest != a.Digest {
		return coreexecution.ProjectAnalysis{}, ErrExecutionStoreDrift
	}
	return n, nil
}

func validateTargetRow(raw []byte, rowOwner, owner, id, expectedID string, revision uint64, status, schema, digest string) (coreexecution.ExecutionTarget, error) {
	var t coreexecution.ExecutionTarget
	if strictJSON(raw, &t) != nil || rowOwner != owner || id != expectedID || t.ID != expectedID || t.Revision != revision || status != "active" || schema != "execution-target/v2" || digest != string(t.Digest) {
		return coreexecution.ExecutionTarget{}, ErrExecutionStoreDrift
	}
	n, err := t.Normalize()
	if err != nil || n.Digest != t.Digest {
		return coreexecution.ExecutionTarget{}, ErrExecutionStoreDrift
	}
	return n, nil
}

func validateObservationRow(raw []byte, rowOwner, owner, id, expectedID, targetID string, targetRevision, revision uint64, status, schema, digest string, observedAt time.Time) (TargetObservationRecord, error) {
	var o coreexecution.TargetObservation
	if strictJSON(raw, &o) != nil || rowOwner != owner || id != expectedID || o.TargetID != targetID || o.TargetRevision != targetRevision || o.ObservedAt.UTC().Truncate(time.Microsecond) != observedAt.UTC().Truncate(time.Microsecond) || revision != 1 || schema != "execution-observation/v2" || digest != string(o.Digest) || (status != "observed" && status != "failed") || validateCatalogSensitiveData(o) != nil {
		return TargetObservationRecord{}, ErrExecutionStoreDrift
	}
	n, err := o.Normalize()
	if err != nil || n.Digest != o.Digest {
		return TargetObservationRecord{}, ErrExecutionStoreDrift
	}
	return TargetObservationRecord{OwnerID: owner, ObservationID: expectedID, Revision: revision, Status: status, Observation: n}, nil
}

func catalogProjectSnapshot(p ExecutionProject) ([]byte, error) {
	return json.Marshal(struct {
		SchemaVersion string `json:"schema_version"`
		ProjectID     string `json:"project_id"`
		OwnerID       string `json:"owner_id"`
	}{"execution-project/v2", p.ProjectID, p.OwnerID})
}

func parseCatalogIdempotency(v string) (uuid.UUID, error) {
	id, err := uuid.Parse(strings.TrimSpace(v))
	if err != nil || id == uuid.Nil {
		return uuid.Nil, ErrExecutionStoreInvalid
	}
	return id, nil
}

func catalogIdempotency(ctx context.Context, tx *sql.Tx, owner string, id uuid.UUID, request, response []byte, now time.Time) (bool, []byte, error) {
	canonicalRequest, err := canonicalCatalogJSON(request)
	if err != nil {
		return false, nil, err
	}
	canonicalResponse, err := canonicalCatalogJSON(response)
	if err != nil {
		return false, nil, err
	}
	reqDigest := digestBytes(canonicalRequest)
	respDigest := digestBytes(canonicalResponse)
	res, err := tx.ExecContext(ctx, `INSERT INTO core_execution_idempotency(owner_id,idempotency_id,key_digest,request_digest,response_digest,status,schema_version,response_json,created_at) VALUES($1,$2,$3,$4,$5,'succeeded','execution-idempotency/v2',$6,$7) ON CONFLICT (owner_id,idempotency_id) DO NOTHING`, owner, id, digestBytes([]byte(id.String())), reqDigest, respDigest, response, now)
	if err != nil {
		return false, nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, nil, err
	}
	var oldReq, oldRespDigest []byte
	var status, schema string
	var oldResp []byte
	if err := tx.QueryRowContext(ctx, `SELECT request_digest,response_digest,status,schema_version,response_json FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2 FOR UPDATE`, owner, id).Scan(&oldReq, &oldRespDigest, &status, &schema, &oldResp); err != nil {
		return false, nil, err
	}
	if !equalDigest(oldReq, reqDigest) || status != "succeeded" || schema != "execution-idempotency/v2" {
		return false, nil, coreexecution.ErrConflict
	}
	oldCanonical, err := canonicalCatalogJSON(oldResp)
	if err != nil || string(oldRespDigest) != string(digestBytes(oldCanonical)) {
		return false, nil, ErrExecutionStoreDrift
	}
	return n == 1, oldResp, nil
}

func catalogReplayResponse(ctx context.Context, tx *sql.Tx, owner string, id uuid.UUID) ([]byte, bool, error) {
	var status, schema string
	var responseDigest []byte
	var response []byte
	err := tx.QueryRowContext(ctx, `SELECT status,schema_version,response_digest,response_json FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2 FOR UPDATE`, owner, id).Scan(&status, &schema, &responseDigest, &response)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if status != "succeeded" || schema != "execution-idempotency/v2" {
		return nil, false, coreexecution.ErrConflict
	}
	canonical, canonicalErr := canonicalCatalogJSON(response)
	if canonicalErr != nil || string(responseDigest) != string(digestBytes(canonical)) {
		return nil, false, ErrExecutionStoreDrift
	}
	return response, true, nil
}

func canonicalCatalogJSON(raw []byte) ([]byte, error) {
	var value any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, ErrExecutionStoreDrift
	}
	var extra any
	if dec.Decode(&extra) == nil {
		return nil, ErrExecutionStoreDrift
	}
	return json.Marshal(value)
}

func validateObservationFacts(facts map[string]string) error {
	return validateCatalogSensitiveData(facts)
}
