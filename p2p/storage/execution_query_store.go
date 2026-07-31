package storage

// Read-only PostgreSQL projections for execution-plan/v2.  These methods are
// intentionally separate from the coordinator: they establish the owner
// fence, validate immutable snapshots and digests, and only then return a
// bounded, secret-free projection to callers.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	"github.com/lib/pq"
)

type ExecutionPlanRevisionRecord struct {
	OwnerID          string
	PlanID           string
	ProjectID        string
	AnalysisID       string
	Revision         uint64
	Status           string
	Digest           coreexecution.Digest
	ExpiresAt        time.Time
	CreatedAt        time.Time
	ChangedStageKeys []string
}

type ExecutionPlanPage struct {
	Items      []ExecutionPlanRevisionRecord
	NextCursor string
}

type ExecutionPlanHistoryPage struct {
	Items      []ExecutionPlanRevisionRecord
	NextCursor string
}

type ExecutionRunView struct {
	Run    coreexecution.ExecutionRun
	Stages []coreexecution.RunStage
}

type ExecutionRunPage struct {
	Items      []ExecutionRunView
	NextCursor string
}

type ExecutionDeploymentRecord struct {
	OwnerID        string
	DeploymentID   string
	ProjectID      string
	RunID          string
	CurrentStageID string
	ReleaseID      string
	State          string
	Revision       uint64
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type ExecutionDeploymentPage struct {
	Items      []ExecutionDeploymentRecord
	NextCursor string
}

type ExecutionConfirmationRecord struct {
	Confirmation coreconfirmation.Confirmation
	Preview      coreexecution.ConfirmationPreview
}

type ExecutionConfirmationPage struct {
	Items      []ExecutionConfirmationRecord
	NextCursor string
}

// ListExecutionPlanRevisions returns immutable plan metadata in ascending
// revision order. The plan body is deliberately not returned here; callers
// use GetPlanRevision after authorization and digest validation when they need
// the executable graph.
func (s *DatabaseExecutionStore) ListExecutionPlanRevisions(ctx context.Context, owner, planID string, afterRevision uint64, limit int) (ExecutionPlanHistoryPage, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !validUUID(planID) || limit < 0 || limit > 200 {
		return ExecutionPlanHistoryPage{}, ErrExecutionStoreInvalid
	}
	if limit == 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT owner_id,plan_id::text,revision,project_id::text,analysis_id::text,status,plan_digest,snapshot_json,expires_at,created_at FROM core_execution_plan_revisions WHERE owner_id=$1 AND plan_id=$2 AND revision>$3 ORDER BY revision ASC LIMIT $4`, owner, planID, afterRevision, limit+1)
	if err != nil {
		return ExecutionPlanHistoryPage{}, err
	}
	defer rows.Close()
	page := ExecutionPlanHistoryPage{Items: make([]ExecutionPlanRevisionRecord, 0, limit)}
	var previous *coreexecution.PlanSnapshot
	if afterRevision > 0 {
		var raw []byte
		if err := s.db.QueryRowContext(ctx, `SELECT snapshot_json FROM core_execution_plan_revisions WHERE owner_id=$1 AND plan_id=$2 AND revision=$3`, owner, planID, afterRevision).Scan(&raw); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ExecutionPlanHistoryPage{}, coreexecution.ErrNotFound
			}
			return ExecutionPlanHistoryPage{}, err
		}
		snap, err := decodeStoredPlan(raw)
		if err != nil {
			return ExecutionPlanHistoryPage{}, ErrExecutionStoreDrift
		}
		previous = &snap
	}
	for rows.Next() {
		rec, snap, err := scanPlanRevision(rows, owner, planID)
		if err != nil {
			return ExecutionPlanHistoryPage{}, err
		}
		rec.ChangedStageKeys = changedStageKeys(previous, &snap)
		copy := snap
		previous = &copy
		page.Items = append(page.Items, rec)
	}
	if err := rows.Err(); err != nil {
		return ExecutionPlanHistoryPage{}, err
	}
	if len(page.Items) > limit {
		page.NextCursor = fmt.Sprint(page.Items[limit-1].Revision)
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func (s *DatabaseExecutionStore) ListExecutionPlans(ctx context.Context, owner, afterPlanID string, limit int) (ExecutionPlanPage, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || (afterPlanID != "" && !validUUID(afterPlanID)) || limit < 0 || limit > 200 {
		return ExecutionPlanPage{}, ErrExecutionStoreInvalid
	}
	if limit == 0 {
		limit = 100
	}
	args := []any{owner}
	q := `SELECT p.owner_id,p.plan_id::text,p.revision,p.project_id::text,r.analysis_id::text,r.status,r.plan_digest,r.snapshot_json,r.expires_at,r.created_at FROM core_execution_plans p JOIN core_execution_plan_revisions r ON r.owner_id=p.owner_id AND r.plan_id=p.plan_id AND r.revision=p.revision WHERE p.owner_id=$1`
	if afterPlanID != "" {
		q += ` AND p.plan_id::text>$2`
		args = append(args, afterPlanID)
	}
	q += fmt.Sprintf(` ORDER BY p.plan_id ASC LIMIT %d`, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return ExecutionPlanPage{}, err
	}
	defer rows.Close()
	page := ExecutionPlanPage{Items: make([]ExecutionPlanRevisionRecord, 0, limit)}
	for rows.Next() {
		rec, _, err := scanPlanRevision(rows, owner, "")
		if err != nil {
			return ExecutionPlanPage{}, err
		}
		page.Items = append(page.Items, rec)
	}
	if err := rows.Err(); err != nil {
		return ExecutionPlanPage{}, err
	}
	if len(page.Items) > limit {
		page.NextCursor = page.Items[limit-1].PlanID
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func scanPlanRevision(rows *sql.Rows, owner, expectedPlan string) (ExecutionPlanRevisionRecord, coreexecution.PlanSnapshot, error) {
	var r ExecutionPlanRevisionRecord
	var raw []byte
	if err := rows.Scan(&r.OwnerID, &r.PlanID, &r.Revision, &r.ProjectID, &r.AnalysisID, &r.Status, &r.Digest, &raw, &r.ExpiresAt, &r.CreatedAt); err != nil {
		return r, coreexecution.PlanSnapshot{}, err
	}
	if r.OwnerID != owner || !validUUID(r.PlanID) || (expectedPlan != "" && r.PlanID != expectedPlan) || r.Revision == 0 || !coreexecution.ValidateDigest(string(r.Digest)) || r.Status == "" {
		return r, coreexecution.PlanSnapshot{}, ErrExecutionStoreDrift
	}
	snap, err := decodeStoredPlan(raw)
	if err != nil || snap.ID != r.PlanID || snap.OwnerID != owner || snap.ProjectID != r.ProjectID || snap.AnalysisID != r.AnalysisID || snap.Revision != r.Revision || snap.Digest != r.Digest || !snap.ExpiresAt.Equal(r.ExpiresAt.UTC()) {
		return r, coreexecution.PlanSnapshot{}, ErrExecutionStoreDrift
	}
	r.ExpiresAt, r.CreatedAt = r.ExpiresAt.UTC(), r.CreatedAt.UTC()
	return r, snap, nil
}

func changedStageKeys(previous, current *coreexecution.PlanSnapshot) []string {
	if current == nil {
		return nil
	}
	if previous == nil {
		out := make([]string, 0, len(current.Stages))
		for _, stage := range current.Stages {
			out = append(out, stage.StageKey)
		}
		return out
	}
	old := make(map[string]coreexecution.ExecutionStage, len(previous.Stages))
	for _, stage := range previous.Stages {
		old[stage.StageKey] = stage
	}
	out := make([]string, 0)
	for _, stage := range current.Stages {
		prior, ok := old[stage.StageKey]
		if !ok || prior.Digest != stage.Digest {
			out = append(out, stage.StageKey)
		}
		delete(old, stage.StageKey)
	}
	for key := range old {
		out = append(out, key)
	}
	// The plan normalizer preserves stage order; deleted keys are appended in a
	// deterministic order so the diff cannot vary across map iteration.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (s *DatabaseExecutionStore) GetExecutionRun(ctx context.Context, owner, runID string) (ExecutionRunView, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !validUUID(runID) {
		return ExecutionRunView{}, ErrExecutionStoreInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ExecutionRunView{}, err
	}
	defer tx.Rollback()
	view, err := readExecutionRunView(ctx, tx, owner, runID)
	if err != nil {
		return ExecutionRunView{}, err
	}
	if err := tx.Commit(); err != nil {
		return ExecutionRunView{}, err
	}
	return view, nil
}

func (s *DatabaseExecutionStore) ListExecutionRuns(ctx context.Context, owner, projectID, deploymentID, afterRunID string, limit int) (ExecutionRunPage, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || (projectID != "" && !validUUID(projectID)) || (deploymentID != "" && !validUUID(deploymentID)) || (afterRunID != "" && !validUUID(afterRunID)) || limit < 0 || limit > 200 {
		return ExecutionRunPage{}, ErrExecutionStoreInvalid
	}
	if limit == 0 {
		limit = 100
	}
	args := []any{owner}
	q := `SELECT run_id::text FROM core_execution_runs WHERE owner_id=$1`
	if projectID != "" {
		q += fmt.Sprintf(` AND project_id=$%d`, len(args)+1)
		args = append(args, projectID)
	}
	if deploymentID != "" {
		q += fmt.Sprintf(` AND deployment_id=$%d`, len(args)+1)
		args = append(args, deploymentID)
	}
	if afterRunID != "" {
		q += fmt.Sprintf(` AND run_id::text>$%d`, len(args)+1)
		args = append(args, afterRunID)
	}
	q += fmt.Sprintf(` ORDER BY run_id ASC LIMIT %d`, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return ExecutionRunPage{}, err
	}
	defer rows.Close()
	page := ExecutionRunPage{Items: make([]ExecutionRunView, 0, limit)}
	ids := make([]string, 0, limit+1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ExecutionRunPage{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return ExecutionRunPage{}, err
	}
	if err := rows.Close(); err != nil {
		return ExecutionRunPage{}, err
	}
	for _, id := range ids {
		view, err := s.GetExecutionRun(ctx, owner, id)
		if err != nil {
			return ExecutionRunPage{}, err
		}
		page.Items = append(page.Items, view)
	}
	if len(page.Items) > limit {
		page.NextCursor = page.Items[limit-1].Run.RunID
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func readExecutionRunView(ctx context.Context, tx *sql.Tx, owner, runID string) (ExecutionRunView, error) {
	var view ExecutionRunView
	var raw []byte
	var rowStatus string
	var rowRevision uint64
	var updated time.Time
	if err := tx.QueryRowContext(ctx, `SELECT status,revision,updated_at,snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, owner, runID).Scan(&rowStatus, &rowRevision, &updated, &raw); errors.Is(err, sql.ErrNoRows) {
		return view, coreexecution.ErrNotFound
	} else if err != nil {
		return view, err
	}
	if strictJSON(raw, &view.Run) != nil {
		return view, fmt.Errorf("%w: run snapshot json", ErrExecutionStoreDrift)
	}
	if view.Run.OwnerID != owner || view.Run.RunID != runID || view.Run.Status != coreexecution.RunStatus(rowStatus) || view.Run.Revision != rowRevision || !view.Run.UpdatedAt.Equal(updated.UTC()) || view.Run.RunDigest == "" {
		return view, fmt.Errorf("%w: run snapshot fields owner=%q/%q id=%q/%q status=%q/%q rev=%d/%d updated=%s/%s digest=%q", ErrExecutionStoreDrift, view.Run.OwnerID, owner, view.Run.RunID, runID, view.Run.Status, rowStatus, view.Run.Revision, rowRevision, view.Run.UpdatedAt, updated.UTC(), view.Run.RunDigest)
	}
	if view.Run.PlanRevision == 0 || !validUUID(view.Run.PlanID) || !validUUID(view.Run.ProjectID) || !coreexecution.ValidateDigest(string(view.Run.PlanDigest)) {
		return view, ErrExecutionStoreDrift
	}
	rows, err := tx.QueryContext(ctx, `SELECT stage_id::text,run_id::text,owner_id,plan_id::text,plan_stage_key,plan_revision,run_revision,stage_revision,plan_stage_digest,COALESCE(target_id::text,''),COALESCE(target_revision,0),COALESCE(target_digest,''),COALESCE(task_id::text,''),COALESCE(confirmation_id::text,''),ordinal,status,started_at,completed_at,created_at,updated_at,snapshot_json FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 ORDER BY ordinal ASC,stage_id ASC`, owner, runID)
	if err != nil {
		return view, err
	}
	defer rows.Close()
	for rows.Next() {
		var stage coreexecution.RunStage
		var status string
		var started, finished sql.NullTime
		var snapshot []byte
		if err := rows.Scan(&stage.StageID, &stage.RunID, &stage.OwnerID, &stage.PlanID, &stage.StageKey, &stage.PlanRevision, &stage.RunRevision, &stage.StageRevision, &stage.StageDigest, &stage.TargetID, &stage.TargetRevision, &stage.TargetDigest, &stage.TaskID, &stage.ConfirmationID, &stage.Ordinal, &status, &started, &finished, &stage.CreatedAt, &stage.UpdatedAt, &snapshot); err != nil {
			return view, err
		}
		stage.Status = coreexecution.StageStatus(status)
		if started.Valid {
			stage.StartedAt = started.Time.UTC()
		}
		if finished.Valid {
			stage.FinishedAt = finished.Time.UTC()
		}
		var stored coreexecution.RunStage
		if strictJSON(snapshot, &stored) != nil {
			return view, fmt.Errorf("%w: stage snapshot json", ErrExecutionStoreDrift)
		}
		if !runStageRowMatches(stored, stage) || stored.OwnerID != owner || stored.RunID != runID || stored.StageID == "" || !validUUID(stored.StageID) || stored.Ordinal == 0 || !validUUID(stored.StageIdempotencyKey) {
			return view, fmt.Errorf("%w: stage snapshot fields stage=%q stored=%#v row=%#v", ErrExecutionStoreDrift, stage.StageID, stored, stage)
		}
		stage.StageIdempotencyKey = stored.StageIdempotencyKey
		view.Stages = append(view.Stages, stage)
	}
	if err := rows.Err(); err != nil {
		return view, err
	}
	if len(view.Stages) == 0 {
		return view, ErrExecutionStoreDrift
	}
	return view, nil
}

func runStageRowMatches(a, b coreexecution.RunStage) bool {
	return a.StageID == b.StageID && a.RunID == b.RunID && a.OwnerID == b.OwnerID && a.PlanID == b.PlanID && a.StageKey == b.StageKey && a.PlanRevision == b.PlanRevision && a.RunRevision == b.RunRevision && a.StageRevision == b.StageRevision && a.StageDigest == b.StageDigest && a.TargetID == b.TargetID && a.TargetRevision == b.TargetRevision && a.TargetDigest == b.TargetDigest && a.TaskID == b.TaskID && a.ConfirmationID == b.ConfirmationID && a.Ordinal == b.Ordinal && a.Status == b.Status && a.StartedAt.Equal(b.StartedAt) && a.FinishedAt.Equal(b.FinishedAt) && a.CreatedAt.Equal(b.CreatedAt.UTC()) && a.UpdatedAt.Equal(b.UpdatedAt.UTC())
}

func (s *DatabaseExecutionStore) GetExecutionDeployment(ctx context.Context, owner, deploymentID string) (ExecutionDeploymentRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !validUUID(deploymentID) {
		return ExecutionDeploymentRecord{}, ErrExecutionStoreInvalid
	}
	return readDeployment(ctx, s.db, owner, deploymentID)
}

func (s *DatabaseExecutionStore) ListExecutionDeployments(ctx context.Context, owner, projectID, afterDeploymentID string, limit int) (ExecutionDeploymentPage, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || (projectID != "" && !validUUID(projectID)) || (afterDeploymentID != "" && !validUUID(afterDeploymentID)) || limit < 0 || limit > 200 {
		return ExecutionDeploymentPage{}, ErrExecutionStoreInvalid
	}
	if limit == 0 {
		limit = 100
	}
	args := []any{owner}
	q := `SELECT deployment_id::text FROM core_execution_deployments WHERE owner_id=$1`
	if projectID != "" {
		q += fmt.Sprintf(` AND project_id=$%d`, len(args)+1)
		args = append(args, projectID)
	}
	if afterDeploymentID != "" {
		q += fmt.Sprintf(` AND deployment_id::text>$%d`, len(args)+1)
		args = append(args, afterDeploymentID)
	}
	q += fmt.Sprintf(` ORDER BY deployment_id ASC LIMIT %d`, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return ExecutionDeploymentPage{}, err
	}
	defer rows.Close()
	page := ExecutionDeploymentPage{Items: make([]ExecutionDeploymentRecord, 0, limit)}
	ids := make([]string, 0, limit+1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ExecutionDeploymentPage{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return ExecutionDeploymentPage{}, err
	}
	if err := rows.Close(); err != nil {
		return ExecutionDeploymentPage{}, err
	}
	for _, id := range ids {
		rec, err := s.GetExecutionDeployment(ctx, owner, id)
		if err != nil {
			return ExecutionDeploymentPage{}, err
		}
		page.Items = append(page.Items, rec)
	}
	if len(page.Items) > limit {
		page.NextCursor = page.Items[limit-1].DeploymentID
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func readDeployment(ctx context.Context, q queryable, owner, deploymentID string) (ExecutionDeploymentRecord, error) {
	var r ExecutionDeploymentRecord
	if err := q.QueryRowContext(ctx, `SELECT owner_id,deployment_id::text,project_id::text,current_run_id::text,COALESCE(current_stage_id::text,''),COALESCE(release_id,''),state,revision,created_at,updated_at FROM core_execution_deployments WHERE owner_id=$1 AND deployment_id=$2`, owner, deploymentID).Scan(&r.OwnerID, &r.DeploymentID, &r.ProjectID, &r.RunID, &r.CurrentStageID, &r.ReleaseID, &r.State, &r.Revision, &r.CreatedAt, &r.UpdatedAt); errors.Is(err, sql.ErrNoRows) {
		return r, coreexecution.ErrNotFound
	} else if err != nil {
		return r, err
	}
	if r.OwnerID != owner || r.DeploymentID != deploymentID || !validUUID(r.ProjectID) || !validUUID(r.RunID) || r.Revision == 0 {
		return r, ErrExecutionStoreDrift
	}
	var runOwner, runProject, purpose, runDeployment string
	if err := q.QueryRowContext(ctx, `SELECT owner_id,project_id::text,purpose,COALESCE(deployment_id::text,'') FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, owner, r.RunID).Scan(&runOwner, &runProject, &purpose, &runDeployment); err != nil {
		return r, ErrExecutionStoreDrift
	}
	if runOwner != owner || runProject != r.ProjectID || purpose != string(coreexecution.PurposeService) || runDeployment != deploymentID {
		return r, ErrExecutionStoreDrift
	}
	r.CreatedAt, r.UpdatedAt = r.CreatedAt.UTC(), r.UpdatedAt.UTC()
	return r, nil
}

// ListV2RunEvents and ListV2DeploymentEvents are strict, owner-scoped event
// reads. Cursors are positive sequence numbers and payloads are re-redacted;
// a row that would expose a secret is treated as tampering and rejected.
func (s *DatabaseExecutionStore) ListV2RunEvents(ctx context.Context, owner, runID string, after uint64, limit int) ([]ExecutionEventRecord, uint64, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !validUUID(runID) || limit < 0 || limit > 200 {
		return nil, 0, ErrExecutionStoreInvalid
	}
	if limit == 0 {
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
		var raw []byte
		if err := rows.Scan(&x.EventID, &x.Sequence, &x.StageID, &x.AttemptID, &x.StepKey, &x.Kind, &x.EventKey, &x.EventDigest, &x.Status, &raw, &x.CreatedAt); err != nil {
			return nil, 0, err
		}
		if x.Sequence == 0 || !validUUID(x.EventID) || !executionEventKind.MatchString(x.Kind) {
			return nil, 0, ErrExecutionStoreDrift
		}
		payload, err := eventPayload(raw)
		if err != nil {
			return nil, 0, fmt.Errorf("%w: event payload", err)
		}
		safe, err := canonicalRedactedJSON(payload)
		if err != nil || !jsonEqual(safe, payload) {
			return nil, 0, fmt.Errorf("%w: event payload redaction", ErrExecutionStoreDrift)
		}
		x.Payload, x.PayloadDigest = payload, coreexecution.Digest(digestBytes(payload))
		x.OwnerID, x.RunID = owner, runID
		if expected, e := executionEventDigest(owner, runID, x.StageID, x.AttemptID, x.StepKey, x.Kind, x.EventKey, x.Status, payload); e != nil || expected != x.EventDigest {
			return nil, 0, fmt.Errorf("%w: event digest expected=%s got=%s stage=%s attempt=%s step=%s kind=%s key=%s status=%s payload=%s", ErrExecutionStoreDrift, expected, x.EventDigest, x.StageID, x.AttemptID, x.StepKey, x.Kind, x.EventKey, x.Status, payload)
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

func (s *DatabaseExecutionStore) ListV2DeploymentEvents(ctx context.Context, owner, deploymentID string, after uint64, limit int) ([]DeploymentEventRecord, uint64, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !validUUID(deploymentID) || limit < 0 || limit > 200 {
		return nil, 0, ErrExecutionStoreInvalid
	}
	if limit == 0 {
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
		if x.Sequence == 0 || !validUUID(x.EventID) {
			return nil, 0, ErrExecutionStoreDrift
		}
		var env struct {
			EventKey string          `json:"event_key"`
			Kind     string          `json:"kind"`
			Status   string          `json:"status"`
			Payload  json.RawMessage `json:"payload"`
		}
		if strictJSON(raw, &env) != nil || !executionEventKind.MatchString(env.Kind) {
			return nil, 0, ErrExecutionStoreDrift
		}
		safe, err := canonicalRedactedJSON(env.Payload)
		if err != nil || !jsonEqual(safe, env.Payload) {
			return nil, 0, ErrExecutionStoreDrift
		}
		x.Payload = env.Payload
		x.OwnerID, x.DeploymentID, x.EventKey, x.Kind, x.Status = owner, deploymentID, env.EventKey, env.Kind, env.Status
		if expected, e := coreexecution.CanonicalDigest(struct {
			OwnerID, DeploymentID, EventKey, Kind, Status string
			Payload                                       json.RawMessage
		}{owner, deploymentID, x.EventKey, x.Kind, x.Status, env.Payload}); e != nil || expected != x.EventDigest {
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

func eventPayload(raw []byte) (json.RawMessage, error) {
	payload, err := canonicalJSONBytes(raw)
	if err != nil {
		return nil, ErrExecutionStoreDrift
	}
	return payload, nil
}

func (s *DatabaseExecutionStore) GetV2Confirmation(ctx context.Context, owner, confirmationID string) (ExecutionConfirmationRecord, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || !validUUID(confirmationID) {
		return ExecutionConfirmationRecord{}, ErrExecutionStoreInvalid
	}
	return readV2Confirmation(ctx, s.db, owner, confirmationID)
}

func (s *DatabaseExecutionStore) ListV2Confirmations(ctx context.Context, owner, afterID string, states []coreconfirmation.State, limit int) (ExecutionConfirmationPage, error) {
	if s == nil || s.db == nil || strings.TrimSpace(owner) == "" || (afterID != "" && !validUUID(afterID)) || limit < 0 || limit > 200 {
		return ExecutionConfirmationPage{}, ErrExecutionStoreInvalid
	}
	if limit == 0 {
		limit = 100
	}
	args := []any{owner}
	q := `SELECT confirmation_id::text FROM agent_confirmations WHERE owner_id=$1 AND operation_domain LIKE 'execution:v2:%'`
	if afterID != "" {
		q += ` AND confirmation_id::text>$2`
		args = append(args, afterID)
	}
	if len(states) > 0 {
		vals := make([]string, len(states))
		for i, state := range states {
			if state == "" {
				return ExecutionConfirmationPage{}, ErrExecutionStoreInvalid
			}
			vals[i] = string(state)
		}
		q += fmt.Sprintf(` AND state=ANY($%d::text[])`, len(args)+1)
		args = append(args, pqStringArray(vals))
	}
	q += fmt.Sprintf(` ORDER BY confirmation_id ASC LIMIT %d`, limit+1)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return ExecutionConfirmationPage{}, err
	}
	defer rows.Close()
	page := ExecutionConfirmationPage{Items: make([]ExecutionConfirmationRecord, 0, limit)}
	ids := make([]string, 0, limit+1)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return ExecutionConfirmationPage{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return ExecutionConfirmationPage{}, err
	}
	if err := rows.Close(); err != nil {
		return ExecutionConfirmationPage{}, err
	}
	for _, id := range ids {
		rec, err := s.GetV2Confirmation(ctx, owner, id)
		if err != nil {
			return ExecutionConfirmationPage{}, err
		}
		page.Items = append(page.Items, rec)
	}
	if len(page.Items) > limit {
		page.NextCursor = page.Items[limit-1].Confirmation.ID
		page.Items = page.Items[:limit]
	}
	return page, nil
}

func readV2Confirmation(ctx context.Context, q queryable, owner, id string) (ExecutionConfirmationRecord, error) {
	var out ExecutionConfirmationRecord
	var c coreconfirmation.Confirmation
	var bindingRaw, previewRaw []byte
	var state string
	var previewDigest sql.NullString
	if err := q.QueryRowContext(ctx, `SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,preview_json,preview_digest,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2`, owner, id).Scan(&c.ID, &c.OwnerID, &c.Binding.OperationDomain, &c.Binding.TargetID, &c.Binding.TargetRevision, &c.Binding.Digest, &bindingRaw, &previewRaw, &previewDigest, &c.TaskID, &state, &c.Revision, &c.CreatedAt, &c.UpdatedAt, &c.ExpiresAt, &c.TerminalReason); errors.Is(err, sql.ErrNoRows) {
		return out, coreexecution.ErrNotFound
	} else if err != nil {
		return out, err
	}
	c.CreatedAt, c.UpdatedAt, c.ExpiresAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC(), c.ExpiresAt.UTC()
	if c.OwnerID != owner || c.ID != id || c.Revision <= 0 || !validUUID(c.TaskID) {
		return out, ErrExecutionStoreDrift
	}
	if !validBindingJSONShape(bindingRaw) {
		return out, ErrExecutionStoreDrift
	}
	if parsed, parseErr := parseExecutionBindingJSON(bindingRaw); parseErr != nil {
		return out, ErrExecutionStoreDrift
	} else {
		c.Binding = parsed
	}
	normalized, err := c.Binding.Normalize()
	if err != nil || normalized.OwnerID != owner || normalized.Digest != c.Binding.Digest || !coreconfirmation.Digest(c.Binding.Digest).Valid() {
		return out, ErrExecutionStoreDrift
	}
	c.Binding = normalized
	c.State = coreconfirmation.State(state)
	c.ConfirmationID = c.ID
	if !strings.HasPrefix(c.Binding.OperationDomain, "execution:v2:") || len(previewRaw) == 0 || !previewDigest.Valid || !coreexecution.ValidateDigest(previewDigest.String) {
		return out, ErrExecutionStoreDrift
	}
	var preview coreexecution.ConfirmationPreview
	if strictJSON(previewRaw, &preview) != nil || preview.SchemaVersion != coreexecution.ConfirmationPreviewSchema || preview.Digest != coreexecution.Digest(previewDigest.String) || !validUUID(preview.RunID) || !validUUID(preview.StageID) || preview.RunRevision == 0 || preview.StageRevision == 0 || preview.ExpiresAt.Location() != time.UTC {
		return out, ErrExecutionStoreDrift
	}
	provided := preview.Digest
	preview.Digest = ""
	expectedPreview, err := coreexecution.CanonicalDigest(struct {
		Preview coreexecution.ConfirmationPreview `json:"preview"`
	}{preview})
	preview.Digest = provided
	if err != nil || expectedPreview != provided || !preview.ExpiresAt.Equal(c.ExpiresAt) {
		return out, ErrExecutionStoreDrift
	}
	if err := verifyConfirmationLinkage(ctx, q, owner, c, preview); err != nil {
		return out, err
	}
	out.Confirmation, out.Preview = c, preview
	return out, nil
}

func validBindingJSONShape(raw []byte) bool {
	_, err := parseExecutionBindingJSON(raw)
	return err == nil
}

func verifyConfirmationLinkage(ctx context.Context, q queryable, owner string, c coreconfirmation.Confirmation, p coreexecution.ConfirmationPreview) error {
	if c.Binding.PlanID != p.PlanID || c.Binding.PlanRevision != int64(p.PlanRevision) || c.Binding.PlanDigest != coreconfirmation.Digest(p.PlanDigest) || c.Binding.DeploymentID != p.DeploymentID || c.Binding.RunID != p.RunID || c.Binding.RunRevision != int64(p.RunRevision) || c.Binding.StageID != p.StageID || c.Binding.StageRevision != int64(p.StageRevision) || c.Binding.StageDigest != coreconfirmation.Digest(p.StageDigest) || c.Binding.TargetID != p.TargetID || c.Binding.TargetRevision != int64(p.TargetRevision) || c.Binding.TargetDigest != coreconfirmation.Digest(p.TargetDigest) || c.Binding.ExecutionDigest != coreconfirmation.Digest(p.ExecutionDigest) || c.Binding.ArtifactSetDigest != coreconfirmation.Digest(p.ArtifactSetDigest) || c.Binding.NetworkDigest != coreconfirmation.Digest(p.NetworkDigest) || c.Binding.SecretGrantDigest != coreconfirmation.Digest(p.SecretGrantDigest) || c.Binding.PolicyDigest != coreconfirmation.Digest(p.PolicyDigest) || c.Binding.CostQuoteDigest != coreconfirmation.Digest(p.CostQuoteDigest) || c.Binding.RollbackDigest != coreconfirmation.Digest(p.RollbackDigest) || c.Binding.PreviewDigest != coreconfirmation.Digest(p.Digest) || c.Binding.RiskLevel != string(p.Risk) || c.Binding.GateType != string(p.Gate) || c.Binding.StageIdempotencyKey != p.StageIdempotencyKey || !c.Binding.MatchesConfirmationExpiry(c.ExpiresAt) {
		return ErrExecutionStoreDrift
	}
	// A confirmation pins the immutable run revision that created its stage.
	// The mutable run head normally advances when a confirmation is accepted,
	// so using core_execution_runs here would make an otherwise valid
	// historical confirmation unreadable (and, worse, make its validation
	// depend on later lifecycle changes).
	var runOwner, runPlan, runProject, runDigest, runDeployment string
	var runRevision, planRevision uint64
	if err := q.QueryRowContext(ctx, `SELECT owner_id,plan_id::text,project_id::text,plan_revision,plan_digest,revision,COALESCE(deployment_id::text,'') FROM core_execution_run_revisions WHERE owner_id=$1 AND run_id=$2 AND revision=$3`, owner, p.RunID, p.RunRevision).Scan(&runOwner, &runPlan, &runProject, &planRevision, &runDigest, &runRevision, &runDeployment); err != nil {
		return ErrExecutionStoreDrift
	}
	// The ledger row is append-only and its insert guard makes its columns and
	// snapshot agree with the run at that exact revision. Validate against the
	// immutable row fields here; decoding the historical snapshot again is not
	// an authorization source and can reject a valid PostgreSQL timestamptz
	// representation solely because JSON uses UTC while the driver preserves a
	// session offset.
	if runOwner != owner || runPlan != p.PlanID || planRevision != p.PlanRevision || runRevision != p.RunRevision || runDigest != string(p.PlanDigest) || runDeployment != p.DeploymentID {
		return ErrExecutionStoreDrift
	}
	var stagePlan, stageKey, stageDigest, targetID, targetDigest string
	var stageRevision, targetRevision uint64
	if err := q.QueryRowContext(ctx, `SELECT plan_id::text,plan_stage_key,stage_revision,plan_stage_digest,COALESCE(target_id::text,''),COALESCE(target_revision,0),COALESCE(target_digest,'') FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND stage_id=$3`, owner, p.RunID, p.StageID).Scan(&stagePlan, &stageKey, &stageRevision, &stageDigest, &targetID, &targetRevision, &targetDigest); err != nil {
		return ErrExecutionStoreDrift
	}
	if stagePlan != p.PlanID || stageKey != p.StageKey || stageRevision != p.StageRevision || stageDigest != string(p.StageDigest) || targetID != p.TargetID || targetRevision != p.TargetRevision || targetDigest != string(p.TargetDigest) {
		return ErrExecutionStoreDrift
	}
	return nil
}

// pqStringArray avoids exposing a mutable pq.Array dependency in the public
// query surface while retaining PostgreSQL's typed ANY(text[]) parameter.
type pqStringArray []string

func (a pqStringArray) Value() (driver.Value, error) { return pq.Array([]string(a)).Value() }
