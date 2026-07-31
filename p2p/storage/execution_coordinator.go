package storage

// PostgreSQL execution/v2 aggregate coordinator.  This first slice owns only
// run creation and approval consumption; provider dispatch remains outside the
// transaction and is intentionally not implemented here.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	coreconfirmation "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	coreexecution "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/execution"
	coretask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/google/uuid"
)

type DatabaseExecutionCoordinator struct {
	db                     *sql.DB
	now                    func() time.Time
	executors              ExecutionExecutorAvailability
	executorsAuthoritative bool
}

func NewDatabaseExecutionCoordinator(db *sql.DB, clock func() time.Time) *DatabaseExecutionCoordinator {
	if clock == nil {
		clock = time.Now
	}
	return &DatabaseExecutionCoordinator{db: db, now: clock}
}

// SetExecutorAvailability installs the same closed executor catalog used by
// the stage claimer. It must be called before the coordinator is published.
func (c *DatabaseExecutionCoordinator) SetExecutorAvailability(availability ExecutionExecutorAvailability) {
	if c == nil {
		return
	}
	c.executors = availability
	c.executorsAuthoritative = true
}

// NewPostgresExecutionCoordinator is the explicit PostgreSQL-named alias used
// by integration wiring and keeps the adapter discoverable without exposing a
// second implementation type.
func NewPostgresExecutionCoordinator(db *sql.DB, clock func() time.Time) *DatabaseExecutionCoordinator {
	return NewDatabaseExecutionCoordinator(db, clock)
}

// CreateRun atomically validates an owner-scoped ready plan, materializes the
// immutable run graph, and queues only dependency-ready stages.
func (c *DatabaseExecutionCoordinator) CreateRun(ctx context.Context, in coreexecution.CreateRunCommand) (coreexecution.RunMaterialization, error) {
	if c == nil || c.db == nil || strings.TrimSpace(in.OwnerID) == "" || !coreexecution.ValidateUUID(in.PlanID) || in.PlanRevision == 0 || !validUUID(in.IdempotencyKey) {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	if in.Operation == "" {
		in.Operation = coreexecution.RunOperationExecute
	}
	if in.TriggerKind == "" {
		in.TriggerKind = coreexecution.TriggerManual
	}
	if in.Operation == coreexecution.RunOperationRollback && in.TriggerKind == coreexecution.TriggerManual {
		in.TriggerKind = coreexecution.TriggerRollback
	}
	if !validExecutionOperation(in.Operation) || !validTrigger(in.TriggerKind) || (in.Operation != coreexecution.RunOperationRollback && in.RollbackOfRunID != "") || (in.Operation == coreexecution.RunOperationRollback && !coreexecution.ValidateUUID(in.RollbackOfRunID)) {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	defer tx.Rollback()
	planStore := NewDatabaseExecutionStore(c.db, c.now)
	p, err := planStore.LoadReadyPlanForUpdate(ctx, tx, in.OwnerID, in.PlanID, in.PlanRevision)
	if err != nil {
		return coreexecution.RunMaterialization{}, fmt.Errorf("load ready plan: %w", err)
	}
	// Authorization and plan ownership are established before replay lookup.
	if p.OwnerID != in.OwnerID {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	if !coreexecution.RunOperationAllowedByPlan(p, in.Operation) {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	if c.executorsAuthoritative {
		if err = c.executors.validatePlan(p, in.Operation); err != nil {
			return coreexecution.RunMaterialization{}, err
		}
	}
	if in.Operation == coreexecution.RunOperationRollback {
		var owner, plan string
		var sourceRevision uint64
		if err = tx.QueryRowContext(ctx, `SELECT owner_id,plan_id::text,plan_revision FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, in.OwnerID, in.RollbackOfRunID).Scan(&owner, &plan, &sourceRevision); err != nil || owner != in.OwnerID || plan != p.ID || sourceRevision != p.Revision {
			return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
		}
	}
	key, _ := uuid.Parse(in.IdempotencyKey)
	reqDigest := executionRunRequestDigest(in, p)
	var oldDigest string
	var oldRun sql.NullString
	var oldResponse []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest,run_id::text,response_json FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2 FOR UPDATE`, in.OwnerID, key).Scan(&oldDigest, &oldRun, &oldResponse)
	if err == nil {
		if oldDigest != string(reqDigest) || !oldRun.Valid {
			return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
		}
		m, e := loadExecutionMaterialization(ctx, tx, in.OwnerID, oldRun.String)
		if e != nil {
			return coreexecution.RunMaterialization{}, e
		}
		if e = tx.Commit(); e != nil {
			return coreexecution.RunMaterialization{}, e
		}
		return m, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return coreexecution.RunMaterialization{}, err
	}
	at := c.now().UTC().Truncate(time.Microsecond)
	runID := coreexecution.DeterministicRunID(in.OwnerID, in.IdempotencyKey)
	r := coreexecution.ExecutionRun{RunID: runID, OwnerID: in.OwnerID, Operation: in.Operation, TriggerKind: in.TriggerKind, RollbackOfRunID: in.RollbackOfRunID, DeploymentID: p.DeploymentID, PlanID: p.ID, ProjectID: p.ProjectID, Purpose: p.Purpose, PlanRevision: p.Revision, PlanDigest: p.Digest, Status: coreexecution.RunPending, Revision: 1, CreatedAt: at, UpdatedAt: at}
	r.RunDigest, _ = coreexecution.CanonicalDigest(struct {
		Plan coreexecution.Digest
		ID   string
	}{p.Digest, runID})
	rawRun, _ := json.Marshal(r)
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_runs(owner_id,run_id,project_id,plan_id,plan_revision,rollback_of_run_id,deployment_id,operation,purpose,trigger_kind,plan_digest,revision,status,schema_version,run_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,'')::uuid,NULLIF($7,'')::uuid,$8,$9,$10,$11,1,$12,'execution-run/v2',$13,$14,$15,$15)`, in.OwnerID, runID, p.ProjectID, p.ID, p.Revision, in.RollbackOfRunID, p.DeploymentID, in.Operation, p.Purpose, in.TriggerKind, p.Digest, r.Status, r.RunDigest, rawRun, at); err != nil {
		return coreexecution.RunMaterialization{}, mapCoordErr(err)
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_run_revisions(owner_id,run_id,revision,project_id,plan_id,plan_revision,rollback_of_run_id,deployment_id,operation,purpose,trigger_kind,plan_digest,current_stage,current_stage_id,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,1,$3,$4,$5,NULLIF($6,'')::uuid,NULLIF($7,'')::uuid,$8,$9,$10,$11,$12,NULLIF($13,'')::uuid,$14,$15,NULLIF($16::timestamptz,'0001-01-01 00:00:00+00'::timestamptz),NULLIF($17::timestamptz,'0001-01-01 00:00:00+00'::timestamptz),'execution-run/v2',$18,$19,$20,$20)`, in.OwnerID, runID, p.ProjectID, p.ID, p.Revision, in.RollbackOfRunID, p.DeploymentID, in.Operation, p.Purpose, in.TriggerKind, p.Digest, r.CurrentStage, r.CurrentStageID, r.Status, r.TerminalReason, r.StartedAt, r.FinishedAt, r.RunDigest, rawRun, at); err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	if p.Purpose == coreexecution.PurposeService {
		if !validUUID(p.DeploymentID) {
			return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
		}
		// A deployment is a stable service aggregate. Each retry/upgrade gets a
		// new immutable run while this CAS-updated projection points at the
		// current run; run history remains solely on core_execution_runs.
		if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_deployments(owner_id,deployment_id,project_id,current_run_id,release_id,state,revision,object_json,actual_json,created_at,updated_at) VALUES($1,$2,$3,$4,'','pending',1,'{}','{}',$5,$5) ON CONFLICT(owner_id,deployment_id) DO UPDATE SET current_run_id=EXCLUDED.current_run_id,current_stage_id=NULL,release_id='',state='pending',revision=core_execution_deployments.revision+1,updated_at=EXCLUDED.updated_at WHERE core_execution_deployments.project_id=EXCLUDED.project_id`, in.OwnerID, p.DeploymentID, p.ProjectID, runID, at); err != nil {
			return coreexecution.RunMaterialization{}, mapCoordErr(err)
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_deployment_counters(owner_id,deployment_id,next_sequence) VALUES($1,$2,1) ON CONFLICT(owner_id,deployment_id) DO NOTHING`, in.OwnerID, p.DeploymentID); err != nil {
			return coreexecution.RunMaterialization{}, err
		}
	}
	ordered := append([]coreexecution.ExecutionStage(nil), p.Stages...)
	if in.Operation == coreexecution.RunOperationRollback {
		rows, qerr := tx.QueryContext(ctx, `SELECT plan_stage_key FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND status='succeeded'`, in.OwnerID, in.RollbackOfRunID)
		if qerr != nil {
			return coreexecution.RunMaterialization{}, qerr
		}
		applied := map[string]bool{}
		for rows.Next() {
			var key string
			if qerr = rows.Scan(&key); qerr != nil {
				rows.Close()
				return coreexecution.RunMaterialization{}, qerr
			}
			applied[key] = true
		}
		rows.Close()
		filtered := ordered[:0]
		for _, stage := range ordered {
			if applied[stage.StageKey] {
				filtered = append(filtered, stage)
			}
		}
		ordered = filtered
		ordered = rollbackStageOrder(ordered)
	}
	// Build the complete graph before materializing any stage/task/card. This
	// keeps a malformed forward reference from leaving partial side effects in
	// the transaction, and lets all root approvals pin run revision 2.
	stageIDs := map[string]string{}
	for _, ps := range ordered {
		if _, exists := stageIDs[ps.StageKey]; exists {
			return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
		}
		stageIDs[ps.StageKey] = coreexecution.DeterministicStageID(runID, ps.StageKey)
	}
	dependencies, err := runStageDependencies(ordered, in.Operation)
	if err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	rootStages := make([]coreexecution.ExecutionStage, 0, len(ordered))
	for _, ps := range ordered {
		if len(dependencies[ps.StageKey]) == 0 && !(in.Operation == coreexecution.RunOperationRollback && len(ps.RollbackSteps) == 0) {
			rootStages = append(rootStages, ps)
		}
	}
	if len(rootStages) == 0 {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	// Revision 1 is the pending canonical fact. Promote the aggregate before
	// any materialized child is inserted, so every child, task and approval
	// binds the exact immutable revision 2 snapshot.
	r.Revision = 2
	r.Status = coreexecution.RunQueued
	for _, ps := range rootStages {
		if ps.Risk != coreexecution.RiskR0 && ps.Risk != coreexecution.RiskR1 {
			r.Status = coreexecution.RunWaitingUser
			break
		}
	}
	r.UpdatedAt = at
	rawRun2, marshalErr := json.Marshal(r)
	if marshalErr != nil {
		return coreexecution.RunMaterialization{}, marshalErr
	}
	result, execErr := tx.ExecContext(ctx, `UPDATE core_execution_runs SET status=$1,revision=$2,snapshot_json=$3,updated_at=$4 WHERE owner_id=$5 AND run_id=$6 AND revision=1`, r.Status, r.Revision, rawRun2, at, in.OwnerID, runID)
	if execErr != nil {
		return coreexecution.RunMaterialization{}, execErr
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	if err = insertExecutionRunRevision(ctx, tx, r); err != nil {
		return coreexecution.RunMaterialization{}, err
	}

	stages := make(map[string]coreexecution.RunStage, len(ordered))
	for i, ps := range ordered {
		sid := coreexecution.DeterministicStageID(runID, ps.StageKey)
		rs := coreexecution.RunStage{StageID: sid, RunID: runID, OwnerID: in.OwnerID, PlanID: p.ID, StageKey: ps.StageKey, PlanRevision: p.Revision, RunRevision: r.Revision, StageRevision: ps.Revision, StageDigest: ps.Digest, TargetID: ps.TargetID, TargetRevision: ps.TargetRevision, TargetDigest: ps.TargetDigest, Ordinal: uint64(i + 1), StageIdempotencyKey: coreexecution.DeterministicStageIdempotencyKey(sid, in.Operation), Status: coreexecution.StageBlocked, CreatedAt: at, UpdatedAt: at}
		if in.Operation == coreexecution.RunOperationRollback && len(ps.RollbackSteps) == 0 {
			rs.Status = coreexecution.StageSkipped
			rs.StartedAt = at
			rs.FinishedAt = at
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_run_stages(owner_id,run_id,stage_id,project_id,plan_id,plan_revision,plan_digest,run_revision,plan_stage_key,stage_revision,plan_stage_digest,target_id,target_revision,target_digest,ordinal,revision,status,schema_version,snapshot_json,started_at,completed_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,NULLIF($12,'')::uuid,NULLIF($13,0),NULLIF($14,''),$15,1,$16,'execution-run-stage/v2',$17,NULLIF($18::timestamptz,'0001-01-01 00:00:00+00'::timestamptz),NULLIF($19::timestamptz,'0001-01-01 00:00:00+00'::timestamptz),$20,$20)`, in.OwnerID, runID, sid, p.ProjectID, p.ID, p.Revision, p.Digest, r.Revision, ps.StageKey, ps.Revision, ps.Digest, ps.TargetID, ps.TargetRevision, ps.TargetDigest, rs.Ordinal, rs.Status, mustJSON(rs), rs.StartedAt, rs.FinishedAt, at); err != nil {
			return coreexecution.RunMaterialization{}, mapCoordErr(err)
		}
		stages[ps.StageKey] = rs
	}
	for _, ps := range ordered {
		sid := stageIDs[ps.StageKey]
		for _, dep := range dependencies[ps.StageKey] {
			depID := stageIDs[dep]
			if depID == "" {
				return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
			}
			if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_run_stage_dependencies(owner_id,run_id,stage_id,depends_on_stage_id) VALUES($1,$2,$3,$4)`, in.OwnerID, runID, sid, depID); err != nil {
				return coreexecution.RunMaterialization{}, err
			}
		}
	}
	// Dependencies are now fully persisted. Materialize only graph roots.
	for _, ps := range rootStages {
		if err = materializeExecutionStage(ctx, tx, c.now, p, r, stages[ps.StageKey], at); err != nil {
			return coreexecution.RunMaterialization{}, err
		}
	}
	if err = insertExecutionEvent(ctx, tx, in.OwnerID, runID, "run_created", "", coreexecution.StageBlocked, at); err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	response, err := loadExecutionMaterialization(ctx, tx, in.OwnerID, runID)
	if err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	rawResp, _ := json.Marshal(response)
	if _, err = tx.ExecContext(ctx, `INSERT INTO core_execution_idempotency(owner_id,idempotency_id,run_id,key_digest,request_digest,status,schema_version,response_json,created_at) VALUES($1,$2,$3,$4,$5,'accepted','execution-idempotency/v2',$6,$7)`, in.OwnerID, key, runID, string(digestBytes([]byte(in.IdempotencyKey))), string(reqDigest), rawResp, at); err != nil {
		return coreexecution.RunMaterialization{}, mapCoordErr(err)
	}
	if err = tx.Commit(); err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	return response, nil
}

func materializeExecutionStage(ctx context.Context, tx *sql.Tx, clock func() time.Time, p coreexecution.ExecutionPlan, r coreexecution.ExecutionRun, rs coreexecution.RunStage, at time.Time) error {
	stage := stageByKey(p, rs.StageKey)
	confirm := stage.Risk != coreexecution.RiskR0 && stage.Risk != coreexecution.RiskR1
	status := coretask.StatusQueued
	rs.Status = coreexecution.StageQueued
	payload := &coretask.ExecutionStageTaskPayload{PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: string(p.Digest), DeploymentID: p.DeploymentID, RunID: r.RunID, RunRevision: r.Revision, StageID: rs.StageID, StageRevision: rs.StageRevision, StageDigest: string(rs.StageDigest), TargetID: rs.TargetID, TargetRevision: rs.TargetRevision, TargetDigest: string(rs.TargetDigest)}
	if confirm {
		payload.ConfirmationID = coreexecution.DeterministicConfirmationID(rs.StageID)
		status = coretask.StatusWaitingUser
		rs.Status = coreexecution.StageWaitingUser
		rs.ConfirmationID = payload.ConfirmationID
	}
	tid := coreexecution.DeterministicTaskID(rs.StageID)
	payloadRaw := coretask.TaskSpec{Kind: coretask.TaskKindExecutionStage, Payload: coretask.TaskPayload{ExecutionStage: payload}, Goal: "execute stage " + rs.StageKey, TimeoutSeconds: int64(stage.TimeoutSeconds), IdempotencyKey: coreexecution.DeterministicTaskID(rs.StageID), AvailableAt: at}
	spec, err := payloadRaw.Normalize()
	if err != nil {
		return fmt.Errorf("stage materialization task payload: %w", err)
	}
	rawSpec, _ := json.Marshal(spec)
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_tasks(task_id,owner_id,spec_json,status,available_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$6)`, tid, rs.OwnerID, rawSpec, status, at, at); err != nil {
		return fmt.Errorf("stage materialization task insert: %w", mapCoordErr(err))
	}
	rs.TaskID = tid
	if confirm {
		preview, err := coreexecution.BuildConfirmationPreview(p, r, stage)
		if err != nil {
			return fmt.Errorf("stage materialization confirmation preview: %w", err)
		}
		bindSnap, err := coreexecution.BuildConfirmationBinding(preview, r.Operation)
		if err != nil {
			return fmt.Errorf("stage materialization confirmation binding: %w", err)
		}
		bind, err := confirmationBindingFromSnapshot(bindSnap, targetKindForPlan(p, rs.TargetID))
		if err != nil {
			return fmt.Errorf("stage materialization confirmation binding conversion: %w", coreexecution.ErrConflict)
		}
		rawBind := executionBindingJSON(bind)
		rawPreview, _ := json.Marshal(preview)
		if _, err = tx.ExecContext(ctx, `INSERT INTO agent_confirmations(confirmation_id,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,preview_json,preview_digest,task_id,state,revision,expires_at,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,'pending',1,$11,$12,$12)`, payload.ConfirmationID, rs.OwnerID, bind.OperationDomain, bind.TargetID, bind.TargetRevision, bind.Digest, rawBind, rawPreview, bind.PreviewDigest, tid, p.ExpiresAt, at); err != nil {
			return fmt.Errorf("stage materialization confirmation insert: %w", mapCoordErr(err))
		}
	}
	rs.UpdatedAt = at
	result, err := tx.ExecContext(ctx, `UPDATE core_execution_run_stages SET task_id=$1,confirmation_id=NULLIF($2,'')::uuid,status=$3,revision=revision+1,snapshot_json=$4,updated_at=$5 WHERE owner_id=$6 AND run_id=$7 AND stage_id=$8 AND status='blocked'`, tid, rs.ConfirmationID, rs.Status, mustJSON(rs), at, rs.OwnerID, rs.RunID, rs.StageID)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return fmt.Errorf("stage materialization blocked stage CAS: %w", coreexecution.ErrConflict)
	}
	return insertExecutionEvent(ctx, tx, rs.OwnerID, rs.RunID, "stage_materialized", rs.StageID, rs.Status, at)
}

func insertExecutionRunRevision(ctx context.Context, tx *sql.Tx, r coreexecution.ExecutionRun) error {
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO core_execution_run_revisions(owner_id,run_id,revision,project_id,plan_id,plan_revision,rollback_of_run_id,deployment_id,operation,purpose,trigger_kind,plan_digest,current_stage,current_stage_id,status,terminal_reason,started_at,completed_at,schema_version,run_digest,snapshot_json,created_at,updated_at) VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,'')::uuid,NULLIF($8,'')::uuid,$9,$10,$11,$12,$13,NULLIF($14,'')::uuid,$15,$16,NULLIF($17::timestamptz,'0001-01-01 00:00:00+00'::timestamptz),NULLIF($18::timestamptz,'0001-01-01 00:00:00+00'::timestamptz),'execution-run/v2',$19,$20,$21,$22)`, r.OwnerID, r.RunID, r.Revision, r.ProjectID, r.PlanID, r.PlanRevision, r.RollbackOfRunID, r.DeploymentID, r.Operation, r.Purpose, r.TriggerKind, r.PlanDigest, r.CurrentStage, r.CurrentStageID, r.Status, r.TerminalReason, r.StartedAt, r.FinishedAt, r.RunDigest, raw, r.CreatedAt, r.UpdatedAt)
	return err
}

// runStageDependencies returns the exact materialized graph. Forward runs use
// the plan DAG unchanged; rollback runs reverse every edge among the selected
// stages, which is the only safe inverse ordering for dependent mutations.
func runStageDependencies(stages []coreexecution.ExecutionStage, operation coreexecution.RunOperation) (map[string][]string, error) {
	keys := make(map[string]bool, len(stages))
	deps := make(map[string][]string, len(stages))
	for _, s := range stages {
		if s.StageKey == "" || keys[s.StageKey] {
			return nil, coreexecution.ErrConflict
		}
		keys[s.StageKey] = true
	}
	for _, s := range stages {
		for _, dep := range s.DependsOn {
			if dep == s.StageKey || !keys[dep] {
				return nil, coreexecution.ErrConflict
			}
			if operation == coreexecution.RunOperationRollback {
				deps[dep] = append(deps[dep], s.StageKey)
			} else {
				deps[s.StageKey] = append(deps[s.StageKey], dep)
			}
		}
	}
	for key := range keys {
		sort.Strings(deps[key])
	}
	return deps, nil
}

func (c *DatabaseExecutionCoordinator) ConfirmStage(ctx context.Context, in coreexecution.ConfirmStageCommand) (coreconfirmation.Confirmation, error) {
	if c == nil || c.db == nil || !validUUID(in.ConfirmationID) || !validUUID(in.IdempotencyKey) || in.ExpectedRevision < 1 || strings.TrimSpace(in.OwnerID) == "" {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	defer tx.Rollback()
	conf, err := loadConfirmationForUpdate(ctx, tx, in.OwnerID, in.ConfirmationID)
	if err != nil || conf.OwnerID != in.OwnerID {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	var replayDigest string
	var replayJSON []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM agent_confirmation_replays WHERE owner_id=$1 AND operation='execution.confirm' AND idempotency_key=$2`, in.OwnerID, in.IdempotencyKey).Scan(&replayDigest, &replayJSON)
	if err == nil {
		d := confirmationRequestDigest(in, conf.Binding)
		if replayDigest != string(d) {
			return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
		}
		var out coreconfirmation.Confirmation
		if json.Unmarshal(replayJSON, &out) != nil {
			return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return coreconfirmation.Confirmation{}, err
		}
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return coreconfirmation.Confirmation{}, err
	}
	if conf.State != coreconfirmation.StatePending || conf.Revision != in.ExpectedRevision || !conf.ExpiresAt.After(c.now().UTC()) {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	var runID, stageID, planID string
	var planRevision uint64
	var stageDigest, targetDigest, planDigest string
	var taskID string
	var runRevision uint64
	var runSnapshotRaw []byte
	var stageSnapshotRaw []byte
	var stageStatus string
	err = tx.QueryRowContext(ctx, `SELECT s.run_id::text,s.stage_id::text,s.plan_id::text,s.plan_revision,s.plan_stage_digest,s.target_digest,s.task_id::text,s.status,s.snapshot_json,r.revision,r.plan_digest,r.snapshot_json FROM core_execution_run_stages s JOIN core_execution_runs r ON r.owner_id=s.owner_id AND r.run_id=s.run_id WHERE s.owner_id=$1 AND s.confirmation_id=$2 FOR UPDATE`, in.OwnerID, in.ConfirmationID).Scan(&runID, &stageID, &planID, &planRevision, &stageDigest, &targetDigest, &taskID, &stageStatus, &stageSnapshotRaw, &runRevision, &planDigest, &runSnapshotRaw)
	if err != nil {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	p, err := NewDatabaseExecutionStore(c.db, c.now).GetPlanRevision(ctx, in.OwnerID, planID, planRevision)
	if err != nil {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	stage := stageByID(p, stageID, runID)
	var run coreexecution.ExecutionRun
	if json.Unmarshal(runSnapshotRaw, &run) != nil {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	run.Revision = runRevision
	run.PlanDigest = coreexecution.Digest(planDigest)
	if conf.Binding.RunRevision < 1 {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	// Child stages remain bound to the graph's immutable creation revision
	// while the aggregate run head advances through earlier stages. Rebuild the
	// confirmation against that bound revision, not the mutable current head.
	run.Revision = uint64(conf.Binding.RunRevision)
	preview, err := coreexecution.BuildConfirmationPreview(p, run, stage)
	if err != nil {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	snap, err := coreexecution.BuildConfirmationBinding(preview, run.Operation)
	if err != nil {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	auth, err := confirmationBindingFromSnapshot(snap, targetKindForPlan(p, stage.TargetID))
	if err != nil {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	if stageStatus != string(coreexecution.StageWaitingUser) || !conf.Binding.Equal(auth) || !conf.Binding.MatchesOwner(in.OwnerID) || !conf.Binding.MatchesConfirmationExpiry(conf.ExpiresAt) || taskID == "" {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	var taskStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, in.OwnerID, taskID).Scan(&taskStatus); err != nil || taskStatus != string(coretask.StatusWaitingUser) {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	at := c.now().UTC().Truncate(time.Microsecond)
	newConf := conf
	newConf.State = coreconfirmation.StateConfirmed
	newConf.Revision++
	newConf.UpdatedAt = at
	result, err := tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='confirmed',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND confirmation_id=$3 AND state='pending' AND revision=$4`, at, in.OwnerID, in.ConfirmationID, in.ExpectedRevision)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	result, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='queued',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND task_id=$3 AND status='waiting_user'`, at, in.OwnerID, taskID)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	var stageRecord coreexecution.RunStage
	if strictJSON(stageSnapshotRaw, &stageRecord) != nil || stageRecord.RunID != runID || stageRecord.StageID != stageID || stageRecord.Status != coreexecution.StageWaitingUser || stageRecord.RunRevision != uint64(conf.Binding.RunRevision) {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	stageRecord.Status = coreexecution.StageQueued
	stageRecord.UpdatedAt = at
	result, err = tx.ExecContext(ctx, `UPDATE core_execution_run_stages SET status='queued',revision=revision+1,snapshot_json=$1,updated_at=$2 WHERE owner_id=$3 AND run_id=$4 AND stage_id=$5 AND status='waiting_user'`, mustJSON(stageRecord), at, in.OwnerID, runID, stageID)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	if n, _ := result.RowsAffected(); n != 1 {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	var runRaw []byte
	var runStatus string
	var runRev uint64
	err = tx.QueryRowContext(ctx, `SELECT status,revision,snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, in.OwnerID, runID).Scan(&runStatus, &runRev, &runRaw)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	if runStatus == string(coreexecution.RunWaitingUser) || runStatus == string(coreexecution.RunPending) {
		var rr coreexecution.ExecutionRun
		if strictJSON(runRaw, &rr) != nil || rr.RunID != runID || rr.Revision != runRev {
			return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
		}
		runRev++
		runStatus = string(coreexecution.RunQueued)
		rr.Status = coreexecution.RunQueued
		rr.Revision = runRev
		rr.UpdatedAt = at
		runJSON, marshalErr := json.Marshal(rr)
		if marshalErr != nil {
			return coreconfirmation.Confirmation{}, marshalErr
		}
		result, err = tx.ExecContext(ctx, `UPDATE core_execution_runs SET status='queued',revision=$1,snapshot_json=$2,updated_at=$3 WHERE owner_id=$4 AND run_id=$5 AND revision=$6`, runRev, runJSON, at, in.OwnerID, runID, runRev-1)
		if err != nil {
			return coreconfirmation.Confirmation{}, err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
		}
		if err = insertExecutionRunRevision(ctx, tx, rr); err != nil {
			return coreconfirmation.Confirmation{}, err
		}
	}
	if err = insertExecutionEvent(ctx, tx, in.OwnerID, runID, "confirmation_confirmed", stageID, coreexecution.StageQueued, at); err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	d := confirmationRequestDigest(in, conf.Binding)
	rawOut, _ := json.Marshal(newConf)
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_confirmation_replays(owner_id,operation,idempotency_key,request_digest,response_json,created_at) VALUES($1,'execution.confirm',$2,$3,$4,$5)`, in.OwnerID, in.IdempotencyKey, string(d), rawOut, at); err != nil {
		return coreconfirmation.Confirmation{}, mapCoordErr(err)
	}
	if err = tx.Commit(); err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	return newConf, nil
}

// RejectStage consumes a pending execution approval without ever dispatching
// provider work.  It replays only an exact owner-scoped command and rebuilds
// the immutable binding before changing the aggregate.
func (c *DatabaseExecutionCoordinator) RejectStage(ctx context.Context, in coreexecution.RejectStageCommand) (coreconfirmation.Confirmation, error) {
	if c == nil || c.db == nil || !validUUID(in.ConfirmationID) || !validUUID(in.IdempotencyKey) || in.ExpectedRevision < 1 || strings.TrimSpace(in.OwnerID) == "" {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	defer tx.Rollback()
	conf, err := loadConfirmationForUpdate(ctx, tx, in.OwnerID, in.ConfirmationID)
	if err != nil || conf.OwnerID != in.OwnerID {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	digest := rejectStageRequestDigest(in, conf.Binding)
	var oldDigest string
	var replay []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM agent_confirmation_replays WHERE owner_id=$1 AND operation='execution.reject' AND idempotency_key=$2`, in.OwnerID, in.IdempotencyKey).Scan(&oldDigest, &replay)
	if err == nil {
		var out coreconfirmation.Confirmation
		if oldDigest != string(digest) || json.Unmarshal(replay, &out) != nil {
			return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return coreconfirmation.Confirmation{}, err
		}
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) || conf.State != coreconfirmation.StatePending || conf.Revision != in.ExpectedRevision || !conf.ExpiresAt.After(c.now().UTC()) {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	var runID, stageID, planID, taskID, stageStatus string
	var planRevision, runRevision uint64
	var stageRaw, runRaw []byte
	err = tx.QueryRowContext(ctx, `SELECT s.run_id::text,s.stage_id::text,s.plan_id::text,s.plan_revision,s.task_id::text,s.status,s.snapshot_json,r.revision,r.snapshot_json FROM core_execution_run_stages s JOIN core_execution_runs r ON r.owner_id=s.owner_id AND r.run_id=s.run_id WHERE s.owner_id=$1 AND s.confirmation_id=$2 FOR UPDATE`, in.OwnerID, in.ConfirmationID).Scan(&runID, &stageID, &planID, &planRevision, &taskID, &stageStatus, &stageRaw, &runRevision, &runRaw)
	if err != nil || taskID == "" || stageStatus != string(coreexecution.StageWaitingUser) || conf.Binding.RunRevision < 1 {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	p, err := NewDatabaseExecutionStore(c.db, c.now).GetPlanRevision(ctx, in.OwnerID, planID, planRevision)
	if err != nil {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	var run coreexecution.ExecutionRun
	var stageRecord coreexecution.RunStage
	if strictJSON(runRaw, &run) != nil || strictJSON(stageRaw, &stageRecord) != nil || run.RunID != runID || run.Revision != runRevision || stageRecord.StageID != stageID || stageRecord.Status != coreexecution.StageWaitingUser {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	run.Revision = uint64(conf.Binding.RunRevision)
	preview, e := coreexecution.BuildConfirmationPreview(p, run, stageByID(p, stageID, runID))
	if e != nil {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	snap, e := coreexecution.BuildConfirmationBinding(preview, run.Operation)
	if e != nil {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	auth, e := confirmationBindingFromSnapshot(snap, targetKindForPlan(p, stageRecord.TargetID))
	if e != nil || !conf.Binding.Equal(auth) || !conf.Binding.MatchesOwner(in.OwnerID) || !conf.Binding.MatchesConfirmationExpiry(conf.ExpiresAt) {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	var taskStatus string
	if err = tx.QueryRowContext(ctx, `SELECT status FROM agent_tasks WHERE owner_id=$1 AND task_id=$2 FOR UPDATE`, in.OwnerID, taskID).Scan(&taskStatus); err != nil || taskStatus != string(coretask.StatusWaitingUser) {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	at := c.now().UTC().Truncate(time.Microsecond)
	res, err := tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='rejected',terminal_reason='user_rejected',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND confirmation_id=$3 AND state='pending' AND revision=$4`, at, in.OwnerID, in.ConfirmationID, in.ExpectedRevision)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	res, err = tx.ExecContext(ctx, `UPDATE agent_tasks SET status='canceled',failure_code='user_rejected',failure_summary='user_rejected',revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$1 WHERE owner_id=$2 AND task_id=$3 AND status='waiting_user'`, at, in.OwnerID, taskID)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	stageRecord.Status = coreexecution.StageRejected
	stageRecord.FinishedAt = at
	stageRecord.UpdatedAt = at
	res, err = tx.ExecContext(ctx, `UPDATE core_execution_run_stages SET status='rejected',revision=revision+1,completed_at=$1,snapshot_json=$2,updated_at=$1 WHERE owner_id=$3 AND run_id=$4 AND stage_id=$5 AND status='waiting_user'`, at, mustJSON(stageRecord), in.OwnerID, runID, stageID)
	if err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
	}
	var runnable bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 AND status IN ('waiting_user','queued','running'))`, in.OwnerID, runID).Scan(&runnable); err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	if !runnable {
		run.Status = coreexecution.RunRejected
		run.Revision++
		run.TerminalReason = "user_rejected"
		run.FinishedAt = at
		run.UpdatedAt = at
		res, err = tx.ExecContext(ctx, `UPDATE core_execution_runs SET status='rejected',terminal_reason='user_rejected',completed_at=$1,revision=$2,snapshot_json=$3,updated_at=$1 WHERE owner_id=$4 AND run_id=$5 AND revision=$6 AND status IN ('pending','waiting_user','queued')`, at, run.Revision, mustJSON(run), in.OwnerID, runID, runRevision)
		if err != nil {
			return coreconfirmation.Confirmation{}, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return coreconfirmation.Confirmation{}, coreexecution.ErrConflict
		}
		if err = insertExecutionRunRevision(ctx, tx, run); err != nil {
			return coreconfirmation.Confirmation{}, err
		}
		if err = insertExecutionEvent(ctx, tx, in.OwnerID, runID, "run_rejected", "", coreexecution.StageRejected, at); err != nil {
			return coreconfirmation.Confirmation{}, err
		}
	}
	if err = insertExecutionEvent(ctx, tx, in.OwnerID, runID, "confirmation_rejected", stageID, coreexecution.StageRejected, at); err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	out := conf
	out.State = coreconfirmation.StateRejected
	out.TerminalReason = "user_rejected"
	out.Revision++
	out.UpdatedAt = at
	raw, _ := json.Marshal(out)
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_confirmation_replays(owner_id,operation,idempotency_key,request_digest,response_json,created_at) VALUES($1,'execution.reject',$2,$3,$4,$5)`, in.OwnerID, in.IdempotencyKey, string(digest), raw, at); err != nil {
		return coreconfirmation.Confirmation{}, mapCoordErr(err)
	}
	if err = tx.Commit(); err != nil {
		return coreconfirmation.Confirmation{}, err
	}
	return out, nil
}

// CancelRun is a pre-dispatch-only transition.  Any dispatch intent is
// provider evidence, so cancellation refuses to guess whether work happened.
func (c *DatabaseExecutionCoordinator) CancelRun(ctx context.Context, in coreexecution.CancelRunCommand) (coreexecution.ExecutionRun, error) {
	if c == nil || c.db == nil || strings.TrimSpace(in.OwnerID) == "" || !validUUID(in.RunID) || !validUUID(in.IdempotencyKey) || in.ExpectedRevision < 1 {
		return coreexecution.ExecutionRun{}, coreexecution.ErrConflict
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return coreexecution.ExecutionRun{}, err
	}
	defer tx.Rollback()
	var status string
	var rev uint64
	var raw []byte
	if err = tx.QueryRowContext(ctx, `SELECT status,revision,snapshot_json FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, in.OwnerID, in.RunID).Scan(&status, &rev, &raw); err != nil {
		return coreexecution.ExecutionRun{}, coreexecution.ErrConflict
	}
	d := cancelRunRequestDigest(in)
	var old string
	var replay []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM agent_confirmation_replays WHERE owner_id=$1 AND operation='execution.cancel' AND idempotency_key=$2`, in.OwnerID, in.IdempotencyKey).Scan(&old, &replay)
	if err == nil {
		var out coreexecution.ExecutionRun
		if old != string(d) || json.Unmarshal(replay, &out) != nil {
			return coreexecution.ExecutionRun{}, coreexecution.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return coreexecution.ExecutionRun{}, err
		}
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) || rev != in.ExpectedRevision || (status != string(coreexecution.RunPending) && status != string(coreexecution.RunWaitingUser) && status != string(coreexecution.RunQueued)) {
		return coreexecution.ExecutionRun{}, coreexecution.ErrConflict
	}
	var dispatched bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM core_execution_dispatch_intents WHERE owner_id=$1 AND run_id=$2)`, in.OwnerID, in.RunID).Scan(&dispatched); err != nil || dispatched {
		return coreexecution.ExecutionRun{}, coreexecution.ErrConflict
	}
	var run coreexecution.ExecutionRun
	if strictJSON(raw, &run) != nil || run.RunID != in.RunID || run.Revision != rev {
		return coreexecution.ExecutionRun{}, coreexecution.ErrConflict
	}
	at := c.now().UTC().Truncate(time.Microsecond)
	// Lock and change every mutable child individually; a concurrent claimant
	// therefore causes the whole transaction to fail rather than a partial stop.
	rows, e := tx.QueryContext(ctx, `SELECT stage_id::text,status,revision,snapshot_json FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, in.OwnerID, in.RunID)
	if e != nil {
		return coreexecution.ExecutionRun{}, e
	}
	type cancelableStage struct {
		id, status string
		revision   uint64
		raw        []byte
	}
	var stages []cancelableStage
	for rows.Next() {
		var id, st string
		var sr uint64
		var sraw []byte
		if e = rows.Scan(&id, &st, &sr, &sraw); e != nil {
			rows.Close()
			return coreexecution.ExecutionRun{}, e
		}
		stages = append(stages, cancelableStage{id: id, status: st, revision: sr, raw: sraw})
	}
	if e = rows.Err(); e != nil {
		rows.Close()
		return coreexecution.ExecutionRun{}, e
	}
	if e = rows.Close(); e != nil {
		return coreexecution.ExecutionRun{}, e
	}
	for _, candidate := range stages {
		id, st, sr, sraw := candidate.id, candidate.status, candidate.revision, candidate.raw
		if st == "blocked" || st == "waiting_user" || st == "queued" {
			var s coreexecution.RunStage
			if strictJSON(sraw, &s) != nil || s.StageID != id {
				return coreexecution.ExecutionRun{}, coreexecution.ErrConflict
			}
			s.Status = coreexecution.StageCanceled
			s.FinishedAt = at
			s.UpdatedAt = at
			res, x := tx.ExecContext(ctx, `UPDATE core_execution_run_stages SET status='canceled',revision=revision+1,completed_at=$1,snapshot_json=$2,updated_at=$1 WHERE owner_id=$3 AND run_id=$4 AND stage_id=$5 AND revision=$6 AND status=$7`, at, mustJSON(s), in.OwnerID, in.RunID, id, sr, st)
			if x != nil {
				return coreexecution.ExecutionRun{}, x
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return coreexecution.ExecutionRun{}, coreexecution.ErrConflict
			}
		}
	}
	res, e := tx.ExecContext(ctx, `UPDATE agent_tasks SET status='canceled',failure_code='canceled',failure_summary='run_canceled',lease_holder='',lease_expires_at=NULL,revision=revision+1,progress_sequence=progress_sequence+1,updated_at=$1 WHERE owner_id=$2 AND task_id IN (SELECT task_id FROM core_execution_run_stages WHERE owner_id=$2 AND run_id=$3 AND task_id IS NOT NULL) AND status IN ('waiting_user','queued')`, at, in.OwnerID, in.RunID)
	if e != nil {
		return coreexecution.ExecutionRun{}, e
	}
	_ = res
	_, e = tx.ExecContext(ctx, `UPDATE agent_confirmations SET state='rejected',terminal_reason='run_canceled',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND confirmation_id IN (SELECT confirmation_id FROM core_execution_run_stages WHERE owner_id=$2 AND run_id=$3 AND confirmation_id IS NOT NULL) AND state='pending'`, at, in.OwnerID, in.RunID)
	if e != nil {
		return coreexecution.ExecutionRun{}, e
	}
	// Only an already inactive lease can be released here.  An active lease is
	// evidence of a claim and the run would not have passed the pre-dispatch guard.
	_, e = tx.ExecContext(ctx, `UPDATE core_execution_target_mutation_leases SET status='released',revision=revision+1,updated_at=$1 WHERE owner_id=$2 AND run_id=$3 AND status='active' AND (expires_at IS NULL OR expires_at<=$1)`, at, in.OwnerID, in.RunID)
	if e != nil {
		return coreexecution.ExecutionRun{}, e
	}
	run.Status = coreexecution.RunCanceled
	run.Revision++
	run.TerminalReason = "run_canceled"
	run.FinishedAt = at
	run.UpdatedAt = at
	res, e = tx.ExecContext(ctx, `UPDATE core_execution_runs SET status='canceled',terminal_reason='run_canceled',completed_at=$1,revision=$2,snapshot_json=$3,updated_at=$1 WHERE owner_id=$4 AND run_id=$5 AND revision=$6 AND status IN ('pending','waiting_user','queued')`, at, run.Revision, mustJSON(run), in.OwnerID, in.RunID, rev)
	if e != nil {
		return coreexecution.ExecutionRun{}, e
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return coreexecution.ExecutionRun{}, coreexecution.ErrConflict
	}
	if e = insertExecutionRunRevision(ctx, tx, run); e != nil {
		return coreexecution.ExecutionRun{}, e
	}
	if e = insertExecutionEvent(ctx, tx, in.OwnerID, in.RunID, "run_canceled", "", coreexecution.StageCanceled, at); e != nil {
		return coreexecution.ExecutionRun{}, e
	}
	out, _ := json.Marshal(run)
	if _, e = tx.ExecContext(ctx, `INSERT INTO agent_confirmation_replays(owner_id,operation,idempotency_key,request_digest,response_json,created_at) VALUES($1,'execution.cancel',$2,$3,$4,$5)`, in.OwnerID, in.IdempotencyKey, string(d), out, at); e != nil {
		return coreexecution.ExecutionRun{}, mapCoordErr(e)
	}
	if e = tx.Commit(); e != nil {
		return coreexecution.ExecutionRun{}, e
	}
	return run, nil
}

// RetryRun never reuses a prior graph.  It accepts only terminal, certain
// evidence and delegates materialization to CreateRun with a retry trigger.
func (c *DatabaseExecutionCoordinator) RetryRun(ctx context.Context, in coreexecution.RetryRunCommand) (coreexecution.RunMaterialization, error) {
	if c == nil || c.db == nil || strings.TrimSpace(in.OwnerID) == "" || !validUUID(in.RunID) || !validUUID(in.IdempotencyKey) || in.ExpectedRevision < 1 {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	defer tx.Rollback()
	var status string
	var revision uint64
	var planID string
	var planRevision uint64
	var operation, rollbackOf string
	if err = tx.QueryRowContext(ctx, `SELECT status,revision,plan_id::text,plan_revision,operation,COALESCE(rollback_of_run_id::text,'') FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2 FOR UPDATE`, in.OwnerID, in.RunID).Scan(&status, &revision, &planID, &planRevision, &operation, &rollbackOf); err != nil {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	d := retryRunRequestDigest(in)
	var old string
	var replay []byte
	err = tx.QueryRowContext(ctx, `SELECT request_digest,response_json FROM agent_confirmation_replays WHERE owner_id=$1 AND operation='execution.retry' AND idempotency_key=$2`, in.OwnerID, in.IdempotencyKey).Scan(&old, &replay)
	if err == nil {
		var out coreexecution.RunMaterialization
		if old != string(d) || json.Unmarshal(replay, &out) != nil {
			return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
		}
		if err = tx.Commit(); err != nil {
			return coreexecution.RunMaterialization{}, err
		}
		return out, nil
	}
	if !errors.Is(err, sql.ErrNoRows) || revision != in.ExpectedRevision || (status != "failed" && status != "canceled" && status != "rejected" && status != "expired") {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	var uncertain bool
	if err = tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM core_execution_receipts WHERE owner_id=$1 AND run_id=$2 AND status='uncertain') OR EXISTS(SELECT 1 FROM core_execution_dispatch_intents WHERE owner_id=$1 AND run_id=$2 AND status='uncertain')`, in.OwnerID, in.RunID).Scan(&uncertain); err != nil || uncertain {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	// The retry key must materialize a distinct aggregate.  Looking here also
	// prevents accidentally replaying the source CreateRun command.
	key, _ := uuid.Parse(in.IdempotencyKey)
	var existingRun string
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(run_id::text,'') FROM core_execution_idempotency WHERE owner_id=$1 AND idempotency_id=$2 FOR UPDATE`, in.OwnerID, key).Scan(&existingRun)
	if err == nil || !errors.Is(err, sql.ErrNoRows) {
		return coreexecution.RunMaterialization{}, coreexecution.ErrConflict
	}
	if err = tx.Commit(); err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	// Terminal source rows are immutable.  CreateRun re-locks the immutable
	// ready plan and owns new stage/task/confirmation identities.
	out, err := c.CreateRun(ctx, coreexecution.CreateRunCommand{OwnerID: in.OwnerID, PlanID: planID, PlanRevision: planRevision, Operation: coreexecution.RunOperation(operation), TriggerKind: coreexecution.TriggerRetry, RollbackOfRunID: rollbackOf, IdempotencyKey: in.IdempotencyKey})
	if err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	tx, err = c.db.BeginTx(ctx, nil)
	if err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `INSERT INTO agent_confirmation_replays(owner_id,operation,idempotency_key,request_digest,response_json,created_at) VALUES($1,'execution.retry',$2,$3,$4,$5) ON CONFLICT (owner_id,operation,idempotency_key) DO NOTHING`, in.OwnerID, in.IdempotencyKey, string(d), mustJSON(out), c.now().UTC().Truncate(time.Microsecond)); err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	if err = tx.Commit(); err != nil {
		return coreexecution.RunMaterialization{}, err
	}
	return out, nil
}

func loadExecutionMaterialization(ctx context.Context, tx *sql.Tx, owner, runID string) (coreexecution.RunMaterialization, error) {
	var m coreexecution.RunMaterialization
	var raw []byte
	var currentStatus string
	var currentRevision uint64
	var currentUpdated time.Time
	var r coreexecution.ExecutionRun
	if err := tx.QueryRowContext(ctx, `SELECT snapshot_json,status,revision,updated_at FROM core_execution_runs WHERE owner_id=$1 AND run_id=$2`, owner, runID).Scan(&raw, &currentStatus, &currentRevision, &currentUpdated); err != nil {
		return m, coreexecution.ErrNotFound
	}
	if json.Unmarshal(raw, &r) != nil {
		return m, coreexecution.ErrConflict
	}
	m.Run = r
	m.Run.Status = coreexecution.RunStatus(currentStatus)
	m.Run.Revision = currentRevision
	m.Run.UpdatedAt = currentUpdated.UTC()
	rows, err := tx.QueryContext(ctx, `SELECT stage_id::text,run_id::text,owner_id,plan_id::text,plan_stage_key,plan_revision,run_revision,stage_revision,plan_stage_digest,target_id::text,target_revision,target_digest,COALESCE(task_id::text,''),COALESCE(confirmation_id::text,''),ordinal,status,started_at,completed_at,created_at,updated_at,snapshot_json FROM core_execution_run_stages WHERE owner_id=$1 AND run_id=$2 ORDER BY ordinal`, owner, runID)
	if err != nil {
		return m, err
	}
	for rows.Next() {
		var s coreexecution.RunStage
		var st string
		var start, finish sql.NullTime
		var snapshot []byte
		if err = rows.Scan(&s.StageID, &s.RunID, &s.OwnerID, &s.PlanID, &s.StageKey, &s.PlanRevision, &s.RunRevision, &s.StageRevision, &s.StageDigest, &s.TargetID, &s.TargetRevision, &s.TargetDigest, &s.TaskID, &s.ConfirmationID, &s.Ordinal, &st, &start, &finish, &s.CreatedAt, &s.UpdatedAt, &snapshot); err != nil {
			return m, err
		}
		s.Status = coreexecution.StageStatus(st)
		if start.Valid {
			s.StartedAt = start.Time.UTC()
		}
		if finish.Valid {
			s.FinishedAt = finish.Time.UTC()
		}
		var stored coreexecution.RunStage
		if strictJSON(snapshot, &stored) != nil || stored.StageID != s.StageID || stored.RunID != s.RunID || stored.OwnerID != s.OwnerID || stored.PlanID != s.PlanID || stored.StageKey != s.StageKey || stored.PlanRevision != s.PlanRevision || stored.RunRevision != s.RunRevision || stored.StageRevision != s.StageRevision || stored.StageDigest != s.StageDigest || stored.TargetID != s.TargetID || stored.TargetRevision != s.TargetRevision || stored.TargetDigest != s.TargetDigest || stored.TaskID != s.TaskID || stored.ConfirmationID != s.ConfirmationID || stored.Ordinal != s.Ordinal || stored.Status != s.Status || stored.StageIdempotencyKey == "" || !stored.StartedAt.Equal(s.StartedAt) || !stored.FinishedAt.Equal(s.FinishedAt) || !stored.CreatedAt.Equal(s.CreatedAt.UTC()) || !stored.UpdatedAt.Equal(s.UpdatedAt.UTC()) {
			return m, coreexecution.ErrConflict
		}
		s.StageIdempotencyKey = stored.StageIdempotencyKey
		m.Stages = append(m.Stages, s)
	}
	if err = rows.Err(); err != nil {
		return m, err
	}
	if err = rows.Close(); err != nil {
		return m, err
	}
	for _, s := range m.Stages {
		if s.TaskID != "" {
			var raw []byte
			var status string
			var rev int64
			var created, updated, available time.Time
			if err = tx.QueryRowContext(ctx, `SELECT spec_json,status,revision,created_at,updated_at,available_at FROM agent_tasks WHERE owner_id=$1 AND task_id=$2`, owner, s.TaskID).Scan(&raw, &status, &rev, &created, &updated, &available); err != nil {
				return m, err
			}
			var spec coretask.TaskSpec
			if err = json.Unmarshal(raw, &spec); err != nil {
				return m, err
			}
			m.Tasks = append(m.Tasks, coretask.Task{ID: s.TaskID, OwnerID: owner, Spec: spec, Status: coretask.Status(status), Revision: uint64(rev), CreatedAt: created, UpdatedAt: updated, AvailableAt: available})
		}
		if s.ConfirmationID != "" {
			conf, e := loadConfirmationForUpdate(ctx, tx, owner, s.ConfirmationID)
			if e != nil {
				return m, e
			}
			m.Confirmations = append(m.Confirmations, conf)
		}
	}
	return m, nil
}

func loadConfirmationForUpdate(ctx context.Context, tx *sql.Tx, owner, id string) (coreconfirmation.Confirmation, error) {
	var c coreconfirmation.Confirmation
	var raw []byte
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT confirmation_id::text,owner_id,operation_domain,target_id,target_revision,binding_digest,binding_json,task_id::text,state,revision,created_at,updated_at,expires_at,terminal_reason FROM agent_confirmations WHERE owner_id=$1 AND confirmation_id=$2 FOR UPDATE`, owner, id).Scan(&c.ID, &c.OwnerID, &c.Binding.OperationDomain, &c.Binding.TargetID, &c.Binding.TargetRevision, &c.Binding.Digest, &raw, &c.TaskID, &state, &c.Revision, &c.CreatedAt, &c.UpdatedAt, &c.ExpiresAt, &c.TerminalReason); err != nil {
		return c, err
	}
	if parsed, e := parseExecutionBindingJSON(raw); e != nil {
		return c, coreexecution.ErrConflict
	} else {
		c.Binding = parsed
	}
	c.State = coreconfirmation.State(state)
	c.ExpiresAt = c.ExpiresAt.UTC()
	c.CreatedAt = c.CreatedAt.UTC()
	c.UpdatedAt = c.UpdatedAt.UTC()
	c.ConfirmationID = c.ID
	return c, nil
}

func insertExecutionEvent(ctx context.Context, tx *sql.Tx, owner, runID, kind, stageID string, status coreexecution.StageStatus, at time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO core_execution_event_counters(owner_id,run_id,next_sequence) VALUES($1,$2,1) ON CONFLICT DO NOTHING`, owner, runID); err != nil {
		return err
	}
	var seq uint64
	if err := tx.QueryRowContext(ctx, `UPDATE core_execution_event_counters SET next_sequence=next_sequence+1 WHERE owner_id=$1 AND run_id=$2 RETURNING next_sequence-1`, owner, runID).Scan(&seq); err != nil {
		return err
	}
	eid := uuid.NewString()
	payload := map[string]any{"digest": string(digestBytes([]byte(fmt.Sprintf("%s:%s:%d", kind, stageID, seq))))}
	raw, _ := json.Marshal(payload)
	eventStatus := "recorded"
	switch status {
	case coreexecution.StageRunning:
		eventStatus = "running"
	case coreexecution.StageSucceeded:
		eventStatus = "succeeded"
	case coreexecution.StageFailed:
		eventStatus = "failed"
	case coreexecution.StageUncertain:
		eventStatus = "uncertain"
	case coreexecution.StageCanceled:
		eventStatus = "canceled"
	}
	eventKey := kind + ":" + stageID
	eventDigest, err := executionEventDigest(owner, runID, stageID, "", "", kind, eventKey, eventStatus, raw)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO core_execution_events(owner_id,run_id,event_id,sequence,stage_id,kind,event_key,event_digest,status,event_json,created_at) VALUES($1,$2,$3,$4,NULLIF($5,'')::uuid,$6,$7,$8,$9,$10,$11)`, owner, runID, eid, seq, stageID, kind, eventKey, eventDigest, eventStatus, raw, at)
	return err
}

func confirmationBindingFromSnapshot(s coreexecution.ConfirmationBindingSnapshot, targetKind ...string) (coreconfirmation.Binding, error) {
	kind := "target"
	if len(targetKind) > 0 && targetKind[0] != "" {
		kind = targetKind[0]
	}
	content, _ := coreexecution.CanonicalDigest(s.PlanID)
	parameter, _ := coreexecution.CanonicalDigest(s.StageDigest)
	return (coreconfirmation.Binding{OwnerID: s.OwnerID, OperationDomain: "execution:v2:" + string(s.Gate), TargetID: s.TargetID, TargetRevision: int64(s.TargetRevision), TargetKind: kind, ContentDigest: coreconfirmation.Digest(content), ParameterDigest: coreconfirmation.Digest(parameter), PlanID: s.PlanID, PlanRevision: int64(s.PlanRevision), PlanDigest: coreconfirmation.Digest(s.PlanDigest), DeploymentID: s.DeploymentID, RunID: s.RunID, RunRevision: int64(s.RunRevision), StageID: s.StageID, StageRevision: int64(s.StageRevision), StageDigest: coreconfirmation.Digest(s.StageDigest), TargetDigest: coreconfirmation.Digest(s.TargetDigest), ExecutionDigest: coreconfirmation.Digest(s.ExecutionDigest), ArtifactSetDigest: coreconfirmation.Digest(s.ArtifactSetDigest), NetworkDigest: coreconfirmation.Digest(s.NetworkDigest), SecretGrantDigest: coreconfirmation.Digest(s.SecretGrantDigest), PolicyDigest: coreconfirmation.Digest(s.PolicyDigest), CostQuoteDigest: coreconfirmation.Digest(s.CostQuoteDigest), RollbackDigest: coreconfirmation.Digest(s.RollbackDigest), PreviewDigest: coreconfirmation.Digest(s.PreviewDigest), RiskLevel: string(s.Risk), GateType: string(s.Gate), StageIdempotencyKey: s.StageIdempotencyKey, BindingExpiresAt: s.ExpiresAt}).Normalize()
}

// executionBindingJSON is deliberately separate from generic confirmation
// serialization. V2 rows have one canonical snake_case representation; dual
// Go-name/snake-name JSONB objects make a PostgreSQL round-trip ambiguous.
func executionBindingJSON(b coreconfirmation.Binding) []byte {
	raw, _ := json.Marshal(b)
	var obj map[string]json.RawMessage
	_ = json.Unmarshal(raw, &obj)
	canonical := make(map[string]json.RawMessage, len(obj))
	for _, f := range confirmationBindingFields() {
		if v, ok := obj[confirmationBindingJSONName(f)]; ok {
			canonical[executionBindingWireKey(f.Name)] = v
		}
	}
	return mustJSON(canonical)
}

func confirmationBindingFields() []reflect.StructField {
	t := reflect.TypeOf(coreconfirmation.Binding{})
	out := make([]reflect.StructField, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		out = append(out, t.Field(i))
	}
	return out
}

func confirmationBindingJSONName(f reflect.StructField) string {
	if tag := strings.Split(f.Tag.Get("json"), ",")[0]; tag != "" && tag != "-" {
		return tag
	}
	return f.Name
}

func executionBindingWireKey(name string) string {
	// Binding has a stable closed field set. Spell acronym boundaries exactly
	// rather than relying on a lossy generic CamelCase converter.
	replacer := strings.NewReplacer("ID", "_id", "URL", "_url")
	name = replacer.Replace(name)
	var b strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' {
			if i > 0 && name[i-1] != '_' {
				b.WriteByte('_')
			}
			b.WriteRune(r + ('a' - 'A'))
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func parseExecutionBindingJSON(raw []byte) (coreconfirmation.Binding, error) {
	var wire map[string]json.RawMessage
	if json.Unmarshal(raw, &wire) != nil || len(wire) == 0 {
		return coreconfirmation.Binding{}, ErrExecutionStoreDrift
	}
	byWire := make(map[string]reflect.StructField)
	for _, f := range confirmationBindingFields() {
		byWire[executionBindingWireKey(f.Name)] = f
	}
	decoded := make(map[string]json.RawMessage, len(wire))
	for key, value := range wire {
		f, ok := byWire[key]
		if !ok {
			return coreconfirmation.Binding{}, ErrExecutionStoreDrift
		}
		decoded[confirmationBindingJSONName(f)] = value
	}
	converted, err := json.Marshal(decoded)
	if err != nil {
		return coreconfirmation.Binding{}, ErrExecutionStoreDrift
	}
	var b coreconfirmation.Binding
	if json.Unmarshal(converted, &b) != nil {
		return coreconfirmation.Binding{}, ErrExecutionStoreDrift
	}
	return b, nil
}

func executionRunRequestDigest(in coreexecution.CreateRunCommand, p coreexecution.ExecutionPlan) coreexecution.Digest {
	d, _ := coreexecution.CanonicalDigest(struct {
		Schema, OwnerID, PlanID string
		PlanRevision            uint64
		PlanDigest              coreexecution.Digest
		Operation               coreexecution.RunOperation
		TriggerKind             coreexecution.TriggerKind
		RollbackOfRunID         string
	}{coreexecution.SchemaVersion, in.OwnerID, p.ID, in.PlanRevision, p.Digest, in.Operation, in.TriggerKind, in.RollbackOfRunID})
	return d
}
func confirmationRequestDigest(in coreexecution.ConfirmStageCommand, b coreconfirmation.Binding) coreconfirmation.Digest {
	d, _ := coreexecution.CanonicalDigest(struct {
		Schema, OwnerID, ConfirmationID string
		ExpectedRevision                int64
		Binding                         coreconfirmation.Binding
	}{coreexecution.SchemaVersion, in.OwnerID, in.ConfirmationID, in.ExpectedRevision, b})
	return coreconfirmation.Digest(d)
}
func rejectStageRequestDigest(in coreexecution.RejectStageCommand, b coreconfirmation.Binding) coreconfirmation.Digest {
	d, _ := coreexecution.CanonicalDigest(struct {
		Schema, OwnerID, ConfirmationID string
		ExpectedRevision                int64
		Binding                         coreconfirmation.Binding
	}{coreexecution.SchemaVersion, in.OwnerID, in.ConfirmationID, in.ExpectedRevision, b})
	return coreconfirmation.Digest(d)
}
func cancelRunRequestDigest(in coreexecution.CancelRunCommand) coreexecution.Digest {
	d, _ := coreexecution.CanonicalDigest(struct {
		Schema, OwnerID, RunID string
		ExpectedRevision       uint64
	}{coreexecution.SchemaVersion, in.OwnerID, in.RunID, in.ExpectedRevision})
	return d
}
func retryRunRequestDigest(in coreexecution.RetryRunCommand) coreexecution.Digest {
	d, _ := coreexecution.CanonicalDigest(struct {
		Schema, OwnerID, RunID string
		ExpectedRevision       uint64
	}{coreexecution.SchemaVersion, in.OwnerID, in.RunID, in.ExpectedRevision})
	return d
}
func stageByKey(p coreexecution.ExecutionPlan, key string) coreexecution.ExecutionStage {
	for _, s := range p.Stages {
		if s.StageKey == key {
			return s
		}
	}
	return coreexecution.ExecutionStage{}
}
func stageByID(p coreexecution.ExecutionPlan, id, runID string) coreexecution.ExecutionStage {
	for _, s := range p.Stages {
		if coreexecution.DeterministicStageID(runID, s.StageKey) == id {
			return s
		}
	}
	return coreexecution.ExecutionStage{}
}

func targetKindForPlan(p coreexecution.ExecutionPlan, targetID string) string {
	for _, t := range p.Targets {
		if t.ID == targetID {
			return t.Kind
		}
	}
	return "target"
}

func rollbackStageOrder(stages []coreexecution.ExecutionStage) []coreexecution.ExecutionStage {
	byKey := make(map[string]coreexecution.ExecutionStage, len(stages))
	for _, s := range stages {
		byKey[s.StageKey] = s
	}
	seen := make(map[string]bool, len(stages))
	forward := make([]coreexecution.ExecutionStage, 0, len(stages))
	var visit func(string)
	visit = func(key string) {
		if seen[key] {
			return
		}
		seen[key] = true
		s, ok := byKey[key]
		if !ok {
			return
		}
		for _, dep := range s.DependsOn {
			visit(dep)
		}
		forward = append(forward, s)
	}
	keys := make([]string, 0, len(stages))
	for _, s := range stages {
		keys = append(keys, s.StageKey)
	}
	sort.Strings(keys)
	for _, key := range keys {
		visit(key)
	}
	for i, j := 0, len(forward)-1; i < j; i, j = i+1, j-1 {
		forward[i], forward[j] = forward[j], forward[i]
	}
	return forward
}
func validUUID(v string) bool {
	id, e := uuid.Parse(strings.TrimSpace(v))
	return e == nil && id != uuid.Nil && id.String() == strings.TrimSpace(v)
}
func validExecutionOperation(v coreexecution.RunOperation) bool {
	switch v {
	case coreexecution.RunOperationExecute, coreexecution.RunOperationDeploy, coreexecution.RunOperationUpgrade, coreexecution.RunOperationRepair, coreexecution.RunOperationDestroy, coreexecution.RunOperationRollback:
		return true
	}
	return false
}
func validTrigger(v coreexecution.TriggerKind) bool {
	switch v {
	case coreexecution.TriggerManual, coreexecution.TriggerSchedule, coreexecution.TriggerRetry, coreexecution.TriggerRollback:
		return true
	}
	return false
}
func mapCoordErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, coreexecution.ErrConflict) || strings.Contains(strings.ToLower(err.Error()), "duplicate key") {
		return coreexecution.ErrConflict
	}
	return err
}
