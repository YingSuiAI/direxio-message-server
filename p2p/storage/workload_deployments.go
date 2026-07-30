package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcore"
)

// WorkloadDeploymentSource is the dashboard read model over the canonical
// in-process workload tables. It performs no reconciliation and maintains no
// second deployment state machine.
type WorkloadDeploymentSource struct{ store *DatabaseStore }

func NewWorkloadDeploymentSource(store *DatabaseStore) *WorkloadDeploymentSource {
	if store == nil || store.db == nil {
		return nil
	}
	return &WorkloadDeploymentSource{store: store}
}

func (s *WorkloadDeploymentSource) ListDeployments(ctx context.Context, owner string, opts agentcore.DeploymentListOptions) ([]map[string]any, string, error) {
	if s == nil || s.store == nil || strings.TrimSpace(owner) == "" {
		return nil, "", fmt.Errorf("workload deployment source is unavailable")
	}
	limit := agentcorePageSize(opts.PageSize)
	args := []any{strings.TrimSpace(owner)}
	where := []string{"d.owner_id=$1"}
	if token := strings.TrimSpace(opts.PageToken); token != "" {
		cursor, err := decodeDeploymentPageToken(token)
		if err != nil {
			return nil, "", fmt.Errorf("invalid deployment page token")
		}
		args = append(args, cursor.UpdatedAt)
		clause := fmt.Sprintf("(d.updated_at<$%d", len(args))
		cursorID := cursor.DeploymentID
		if cursorID == "" {
			cursorID = cursor.WorkloadID // pre-v106 page tokens
		}
		if cursorID != "" {
			args = append(args, cursorID)
			clause += fmt.Sprintf(" OR (d.updated_at=$%d AND d.deployment_id::text<$%d)", len(args)-1, len(args))
		}
		where = append(where, clause+")")
	}
	if status := strings.TrimSpace(opts.Status); status != "" {
		args = append(args, status)
		where = append(where, fmt.Sprintf(`CASE
			WHEN d.state='destroyed' THEN 'destroyed'
			WHEN d.state IN ('waiting_user','pending') THEN 'pending'
			WHEN d.state='running' THEN 'running'
			WHEN d.state='succeeded' OR d.state='ready' THEN 'succeeded'
			WHEN d.state IN ('failed','uncertain','rejected','expired','canceled') THEN 'failed'
			ELSE d.state END=$%d`, len(args)))
	}
	if kind := strings.TrimSpace(opts.TargetKind); kind != "" {
		args = append(args, kind)
		where = append(where, fmt.Sprintf("d.target_kind=$%d", len(args)))
	}
	query := unifiedDeploymentSelect + ` WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY d.updated_at DESC,d.deployment_id DESC LIMIT ` + strconv.Itoa(limit+1)
	rows, err := s.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	type item struct {
		object       map[string]any
		updatedAt    time.Time
		deploymentID string
	}
	items := make([]item, 0, limit+1)
	for rows.Next() {
		object, updatedAt, deploymentID, scanErr := scanUnifiedDeployment(rows)
		if scanErr != nil {
			return nil, "", scanErr
		}
		items = append(items, item{object: object, updatedAt: updatedAt, deploymentID: deploymentID})
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeDeploymentPageToken(last.updatedAt, last.deploymentID)
	}
	out := make([]map[string]any, 0, len(items))
	for _, value := range items {
		out = append(out, value.object)
	}
	return out, next, nil
}

func (s *WorkloadDeploymentSource) GetDeploymentByID(ctx context.Context, owner, deploymentID string) (map[string]any, bool, error) {
	if s == nil || s.store == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(deploymentID) == "" {
		return nil, false, nil
	}
	row := s.store.db.QueryRowContext(ctx, unifiedDeploymentSelect+` WHERE d.owner_id=$1 AND d.public_deployment_id::text=$2`, strings.TrimSpace(owner), strings.TrimSpace(deploymentID))
	object, _, _, err := scanUnifiedDeployment(row)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	return object, err == nil, err
}

func (s *WorkloadDeploymentSource) GetDeploymentByWorkloadID(ctx context.Context, owner, workloadID string) (map[string]any, bool, error) {
	if s == nil || s.store == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(workloadID) == "" {
		return nil, false, nil
	}
	row := s.store.db.QueryRowContext(ctx, unifiedDeploymentSelect+` WHERE d.owner_id=$1 AND d.workload_id::text=$2`, strings.TrimSpace(owner), strings.TrimSpace(workloadID))
	object, _, _, err := scanUnifiedDeployment(row)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	return object, err == nil, err
}

func (s *WorkloadDeploymentSource) ListDeploymentEventsByID(ctx context.Context, owner, deploymentID string, after int64, limit int) ([]map[string]any, int64, error) {
	return s.listDeploymentEvents(ctx, owner, "d.public_deployment_id::text=$2", deploymentID, after, limit)
}

func (s *WorkloadDeploymentSource) ListDeploymentEventsByWorkloadID(ctx context.Context, owner, workloadID string, after int64, limit int) ([]map[string]any, int64, error) {
	return s.listDeploymentEvents(ctx, owner, "d.workload_id::text=$2", workloadID, after, limit)
}

func (s *WorkloadDeploymentSource) listDeploymentEvents(ctx context.Context, owner, lookup, identifier string, after int64, limit int) ([]map[string]any, int64, error) {
	if s == nil || s.store == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(identifier) == "" || after < 0 {
		return nil, after, fmt.Errorf("invalid workload deployment event query")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 256 {
		limit = 256
	}
	rows, err := s.store.db.QueryContext(ctx, `SELECT e.public_event_id::text,e.sequence,e.source_kind,e.source_id::text,e.source_sequence,e.event_json,e.created_at,d.public_deployment_id::text,COALESCE(d.workload_id::text,''),d.state
		FROM core_deployment_events e JOIN core_deployments d ON d.owner_id=e.owner_id AND d.deployment_id=e.deployment_id
		WHERE d.owner_id=$1 AND `+lookup+` AND e.sequence>$3
		ORDER BY e.sequence LIMIT $4`, strings.TrimSpace(owner), strings.TrimSpace(identifier), after, limit+1)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0, limit+1)
	last := after
	for rows.Next() {
		var eventID, sourceKind, sourceID, deploymentID, linkedWorkload, deploymentState string
		var sequence, sourceSequence int64
		var raw []byte
		var at time.Time
		if err := rows.Scan(&eventID, &sequence, &sourceKind, &sourceID, &sourceSequence, &raw, &at, &deploymentID, &linkedWorkload, &deploymentState); err != nil {
			return nil, after, err
		}
		var event map[string]any
		if json.Unmarshal(raw, &event) != nil {
			event = map[string]any{}
		}
		kind, _ := event["kind"].(string)
		status, _ := event["status"].(string)
		operationID, _ := event["change_id"].(string)
		if operationID == "" && sourceKind == "workload" {
			operationID = sourceID
		}
		if operationID == "" {
			operationID = eventID
		}
		publicStatus := normalizeDeploymentStatusForStorage(status)
		if deploymentState == "destroyed" {
			publicStatus = "destroyed"
		}
		outEvent := map[string]any{
			"event_id": eventID, "deployment_id": deploymentID, "operation_id": operationID,
			"sequence": sequence, "type": workloadDeploymentEventType(kind, status),
			"status": publicStatus, "message": "", "occurred_at": at.UTC().Format(time.RFC3339Nano),
		}
		if actual, ok := event["actual"].(map[string]any); ok && len(actual) != 0 {
			outEvent["actual"] = normalizeWorkloadActual(actual)
		}
		if linkedWorkload != "" {
			outEvent["workload_id"] = linkedWorkload
		}
		out = append(out, outEvent)
		last = sequence
	}
	if err := rows.Err(); err != nil {
		return nil, after, err
	}
	if len(out) > limit {
		out = out[:limit]
		last = int64FromAny(out[len(out)-1]["sequence"])
	}
	return out, last, nil
}

// Legacy aliases retain the pre-v107 storage API for non-agent callers. New
// callers must choose an exact deployment or workload lookup explicitly.
func (s *WorkloadDeploymentSource) GetDeployment(ctx context.Context, owner, workloadID string) (map[string]any, bool, error) {
	return s.GetDeploymentByWorkloadID(ctx, owner, workloadID)
}

func (s *WorkloadDeploymentSource) ListDeploymentEvents(ctx context.Context, owner, workloadID string, after int64, limit int) ([]map[string]any, int64, error) {
	return s.ListDeploymentEventsByWorkloadID(ctx, owner, workloadID, after, limit)
}

// workloadDeploymentEventType translates internal workload lifecycle labels
// into the bounded public deployment event vocabulary. Unknown values are
// intentionally projected to "unknown" by the downstream sanitizer.
func workloadDeploymentEventType(kind, status string) string {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "requested", "queued":
		return "queued"
	case "consumed", "dispatch", "dispatched":
		return "dispatch"
	case "started", "running":
		return "running"
	case "provider_error", "error", "uncertain":
		return "error"
	case "readback", "recovered_readback", "progress":
		return "progress"
	case "completed", "complete":
		return "complete"
	case "succeeded":
		return "succeeded"
	case "failed":
		return "failed"
	case "destroy":
		return "destroy"
	case "destroyed":
		return "destroyed"
	case "canceled", "cancelled":
		return "canceled"
	default:
		// A successful terminal status is still useful when older rows only
		// carried a status and no recognized kind.
		switch normalizeDeploymentStatusForStorage(status) {
		case "succeeded":
			return "succeeded"
		case "failed":
			return "failed"
		}
		return "unknown"
	}
}

const workloadDeploymentSelect = `SELECT
	w.workload_id::text,w.revision,w.plan_id::text,w.plan_digest,w.target_kind,w.state,
	w.actual_snapshot_json,p.target_identity_json,GREATEST(w.updated_at,o.updated_at),
	o.operation_id::text,o.operation,o.plan_revision,o.plan_digest,o.task_id::text,
	o.confirmation_id::text,o.status,o.revision,o.failure_code,o.created_at,o.updated_at,
	o.dispatch_epoch,o.dispatch_lease_until
	FROM core_workloads w
	JOIN core_workload_plans p ON p.owner_id=w.owner_id AND p.plan_id=w.plan_id
	JOIN LATERAL (
		SELECT operation_id,operation,plan_revision,plan_digest,task_id,confirmation_id,status,
		       revision,failure_code,created_at,updated_at,dispatch_epoch,dispatch_lease_until
		FROM core_workload_operations
		WHERE owner_id=w.owner_id AND workload_id=w.workload_id
		ORDER BY updated_at DESC,operation_id DESC LIMIT 1
	) o ON TRUE`

const unifiedDeploymentSelect = `SELECT
	d.public_deployment_id::text,COALESCE(d.provision_id::text,''),COALESCE(d.workload_id::text,''),d.state,d.target_kind,d.revision,d.object_json,d.actual_json,COALESCE(pr.state,''),COALESCE(pr.output_digest,''),d.updated_at,
	COALESCE(w.revision,0),COALESCE(w.plan_id::text,''),COALESCE(w.plan_digest,''),COALESCE(w.state,''),COALESCE(w.actual_snapshot_json,'{}'::jsonb),
	COALESCE(o.operation_id::text,''),COALESCE(o.operation,''),COALESCE(o.plan_revision,0),COALESCE(o.plan_digest,''),COALESCE(o.task_id::text,''),COALESCE(o.confirmation_id::text,''),COALESCE(o.status,''),COALESCE(o.revision,0),COALESCE(o.failure_code,''),COALESCE(o.created_at,d.created_at),COALESCE(o.updated_at,d.updated_at),COALESCE(o.dispatch_epoch,0),o.dispatch_lease_until,d.deployment_id::text
	FROM core_deployments d
	LEFT JOIN core_aws_ec2_provisions pr ON pr.owner_id=d.owner_id AND pr.provision_id=d.provision_id
	LEFT JOIN core_workloads w ON w.owner_id=d.owner_id AND w.workload_id=d.workload_id
	LEFT JOIN LATERAL (
		SELECT operation_id,operation,plan_revision,plan_digest,task_id,confirmation_id,status,revision,failure_code,created_at,updated_at,dispatch_epoch,dispatch_lease_until
		FROM core_workload_operations WHERE owner_id=d.owner_id AND workload_id=d.workload_id
		ORDER BY updated_at DESC,operation_id DESC LIMIT 1
	) o ON TRUE`

type workloadDeploymentScanner interface{ Scan(...any) error }

func scanWorkloadDeployment(row workloadDeploymentScanner) (map[string]any, time.Time, string, error) {
	var workloadID, planID, planDigest, targetKind, workloadState string
	var operationID, operationKind, operationPlanDigest, taskID, confirmationID, operationStatus, failureCode string
	var workloadRevision, planRevision, operationRevision, dispatchEpoch int64
	var actualRaw, targetRaw []byte
	var updatedAt, createdAt, operationUpdatedAt time.Time
	var dispatchUntil sql.NullTime
	if err := row.Scan(
		&workloadID, &workloadRevision, &planID, &planDigest, &targetKind, &workloadState,
		&actualRaw, &targetRaw, &updatedAt, &operationID, &operationKind, &planRevision,
		&operationPlanDigest, &taskID, &confirmationID, &operationStatus, &operationRevision,
		&failureCode, &createdAt, &operationUpdatedAt, &dispatchEpoch, &dispatchUntil,
	); err != nil {
		return nil, time.Time{}, "", err
	}
	actual := map[string]any{}
	_ = json.Unmarshal(actualRaw, &actual)
	actual = normalizeWorkloadActual(actual)
	target := map[string]any{}
	_ = json.Unmarshal(targetRaw, &target)
	target = normalizeWorkloadIdentity(target)
	if len(actual) == 0 {
		actual = nil
	}
	operation := map[string]any{
		"operation_id": operationID, "workload_id": workloadID, "plan_id": planID,
		"kind": operationKind, "plan_revision": planRevision, "plan_digest": operationPlanDigest,
		"target_kind": targetKind, "task_id": taskID, "confirmation_id": confirmationID,
		"status": operationStatus, "revision": operationRevision, "failure_code": failureCode,
		"created_at":     createdAt.UTC().Format(time.RFC3339Nano),
		"updated_at":     operationUpdatedAt.UTC().Format(time.RFC3339Nano),
		"dispatch_epoch": dispatchEpoch,
		"desired_plan":   map[string]any{"target": map[string]any{"identity": target}},
	}
	if dispatchUntil.Valid {
		operation["dispatch_lease_until"] = dispatchUntil.Time.UTC().Format(time.RFC3339Nano)
	}
	object := agentcore.DeploymentObject(agentcore.DeploymentMutation{Operation: operation, Actual: actual})
	object["revision"] = workloadRevision
	object["workload_state"] = workloadState
	object["updated_at"] = updatedAt.UTC().Format(time.RFC3339Nano)
	return agentcore.StripDeploymentInternalFields(object), updatedAt.UTC(), workloadID, nil
}

func scanUnifiedDeployment(row workloadDeploymentScanner) (map[string]any, time.Time, string, error) {
	var deploymentID, provisionID, workloadID, state, targetKind, legacyDeploymentID string
	var revision, workloadRevision, planRevision, operationRevision, dispatchEpoch int64
	var objectRaw, deploymentActualRaw, workloadActualRaw []byte
	var provisionState, provisionOutputDigest string
	var planID, planDigest, workloadState, operationID, operationKind, operationPlanDigest, taskID, confirmationID, operationStatus, failureCode string
	var updatedAt, operationCreatedAt, operationUpdatedAt time.Time
	var dispatchUntil sql.NullTime
	if err := row.Scan(&deploymentID, &provisionID, &workloadID, &state, &targetKind, &revision, &objectRaw, &deploymentActualRaw, &provisionState, &provisionOutputDigest, &updatedAt,
		&workloadRevision, &planID, &planDigest, &workloadState, &workloadActualRaw,
		&operationID, &operationKind, &planRevision, &operationPlanDigest, &taskID, &confirmationID, &operationStatus, &operationRevision, &failureCode, &operationCreatedAt, &operationUpdatedAt, &dispatchEpoch, &dispatchUntil, &legacyDeploymentID); err != nil {
		return nil, time.Time{}, "", err
	}
	object := map[string]any{}
	_ = json.Unmarshal(objectRaw, &object)
	object["deployment_id"], object["provision_id"], object["workload_id"] = deploymentID, provisionID, workloadID
	object["state"], object["target_kind"], object["revision"] = state, targetKind, revision
	actual := map[string]any{}
	if len(workloadActualRaw) > 0 && string(workloadActualRaw) != "{}" {
		_ = json.Unmarshal(workloadActualRaw, &actual)
	}
	if len(actual) == 0 && len(deploymentActualRaw) > 0 {
		_ = json.Unmarshal(deploymentActualRaw, &actual)
	}
	actual = normalizeWorkloadActual(actual)
	if len(actual) > 0 {
		object["actual"] = actual
	}
	if workloadID != "" {
		object["workload_state"] = workloadState
		object["plan_id"], object["plan_digest"] = planID, planDigest
	}
	if operationID != "" {
		op := map[string]any{"operation_id": operationID, "workload_id": workloadID, "plan_id": planID, "kind": operationKind, "plan_revision": planRevision, "plan_digest": operationPlanDigest, "target_kind": targetKind, "task_id": taskID, "confirmation_id": confirmationID, "status": operationStatus, "revision": operationRevision, "failure_code": failureCode, "created_at": operationCreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": operationUpdatedAt.UTC().Format(time.RFC3339Nano), "dispatch_epoch": dispatchEpoch}
		if dispatchUntil.Valid {
			op["dispatch_lease_until"] = dispatchUntil.Time.UTC().Format(time.RFC3339Nano)
		}
		object["current_operation"] = op
		object["latest_operation_id"] = operationID
		object["status"] = normalizeDeploymentStatusForStorage(operationStatus)
		if state == "destroyed" || (operationKind == "destroy" && operationStatus == "succeeded") {
			object["status"] = "destroyed"
		}
	}
	// Endpoint is public only after ready+succeeded and a provider readback
	// digest fence. Pending, uncertain and unverified rows never leak it.
	if (state != "active" && state != "ready") || (provisionID != "" && (provisionState != "active" || strings.TrimSpace(provisionOutputDigest) == "")) || operationStatus != "succeeded" || workloadState != "ready" || strings.TrimSpace(stringValue(actual["readback_digest"])) == "" || strings.TrimSpace(stringValue(actual["applied_plan_digest"])) == "" || stringValue(actual["applied_plan_digest"]) != planDigest {
		if identity, ok := actual["identity"].(map[string]any); ok {
			delete(identity, "endpoint")
		}
	}
	object["updated_at"] = updatedAt.UTC().Format(time.RFC3339Nano)
	return agentcore.StripDeploymentInternalFields(object), updatedAt.UTC(), legacyDeploymentID, nil
}

func normalizeWorkloadActual(actual map[string]any) map[string]any {
	if len(actual) == 0 {
		return nil
	}
	out := make(map[string]any, len(actual))
	for key, value := range actual {
		out[key] = value
	}
	if identity, ok := actual["identity"].(map[string]any); ok {
		out["identity"] = normalizeWorkloadIdentity(identity)
	}
	return out
}

func normalizeWorkloadIdentity(identity map[string]any) map[string]any {
	if len(identity) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(identity)+2)
	for key, value := range identity {
		out[key] = value
	}
	if value, ok := out["account_id"]; ok {
		out["aws_account_id"] = value
	}
	if value, ok := out["region"]; ok {
		out["aws_region"] = value
	}
	return out
}

var _ embeddedDeploymentSourceCompatibility = (*WorkloadDeploymentSource)(nil)

// embeddedDeploymentSourceCompatibility keeps this package independent from
// the service adapter while asserting the exact dashboard read contract.
type embeddedDeploymentSourceCompatibility interface {
	ListDeployments(context.Context, string, agentcore.DeploymentListOptions) ([]map[string]any, string, error)
	GetDeploymentByID(context.Context, string, string) (map[string]any, bool, error)
	GetDeploymentByWorkloadID(context.Context, string, string) (map[string]any, bool, error)
	ListDeploymentEventsByID(context.Context, string, string, int64, int) ([]map[string]any, int64, error)
	ListDeploymentEventsByWorkloadID(context.Context, string, string, int64, int) ([]map[string]any, int64, error)
}
