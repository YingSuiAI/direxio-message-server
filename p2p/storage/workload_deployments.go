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
	where := []string{"w.owner_id=$1"}
	if token := strings.TrimSpace(opts.PageToken); token != "" {
		cursor, err := decodeDeploymentPageToken(token)
		if err != nil {
			return nil, "", fmt.Errorf("invalid deployment page token")
		}
		args = append(args, cursor.UpdatedAt)
		clause := fmt.Sprintf("(GREATEST(w.updated_at,o.updated_at)<$%d", len(args))
		if cursor.WorkloadID != "" {
			args = append(args, cursor.WorkloadID)
			clause += fmt.Sprintf(" OR (GREATEST(w.updated_at,o.updated_at)=$%d AND w.workload_id::text<$%d)", len(args)-1, len(args))
		}
		where = append(where, clause+")")
	}
	if status := strings.TrimSpace(opts.Status); status != "" {
		args = append(args, status)
		where = append(where, fmt.Sprintf(`CASE
			WHEN o.operation='destroy' AND o.status='succeeded' THEN 'destroyed'
			WHEN o.status='waiting_user' THEN 'pending'
			WHEN o.status='running' THEN 'running'
			WHEN o.status='succeeded' THEN 'succeeded'
			WHEN o.status IN ('failed','uncertain','rejected','expired','canceled') THEN 'failed'
			ELSE o.status END=$%d`, len(args)))
	}
	if kind := strings.TrimSpace(opts.TargetKind); kind != "" {
		args = append(args, kind)
		where = append(where, fmt.Sprintf("w.target_kind=$%d", len(args)))
	}
	query := workloadDeploymentSelect + ` WHERE ` + strings.Join(where, " AND ") +
		` ORDER BY GREATEST(w.updated_at,o.updated_at) DESC,w.workload_id DESC LIMIT ` + strconv.Itoa(limit+1)
	rows, err := s.store.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	type item struct {
		object     map[string]any
		updatedAt  time.Time
		workloadID string
	}
	items := make([]item, 0, limit+1)
	for rows.Next() {
		object, updatedAt, workloadID, scanErr := scanWorkloadDeployment(rows)
		if scanErr != nil {
			return nil, "", scanErr
		}
		items = append(items, item{object: object, updatedAt: updatedAt, workloadID: workloadID})
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		last := items[len(items)-1]
		next = encodeDeploymentPageToken(last.updatedAt, last.workloadID)
	}
	out := make([]map[string]any, 0, len(items))
	for _, value := range items {
		out = append(out, value.object)
	}
	return out, next, nil
}

func (s *WorkloadDeploymentSource) GetDeployment(ctx context.Context, owner, workloadID string) (map[string]any, bool, error) {
	if s == nil || s.store == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(workloadID) == "" {
		return nil, false, nil
	}
	row := s.store.db.QueryRowContext(ctx, workloadDeploymentSelect+` WHERE w.owner_id=$1 AND w.workload_id=$2`, strings.TrimSpace(owner), strings.TrimSpace(workloadID))
	object, _, _, err := scanWorkloadDeployment(row)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	return object, err == nil, err
}

func (s *WorkloadDeploymentSource) ListDeploymentEvents(ctx context.Context, owner, workloadID string, after int64, limit int) ([]map[string]any, int64, error) {
	if s == nil || s.store == nil || strings.TrimSpace(owner) == "" || strings.TrimSpace(workloadID) == "" || after < 0 {
		return nil, after, fmt.Errorf("invalid workload deployment event query")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 256 {
		limit = 256
	}
	rows, err := s.store.db.QueryContext(ctx, `SELECT e.operation_id::text,e.sequence,e.public_sequence,e.kind,e.status,e.readback_json,e.at
		FROM core_workload_events e
		WHERE e.owner_id=$1 AND e.workload_id=$2 AND e.public_sequence>$3
		ORDER BY e.public_sequence LIMIT $4`, strings.TrimSpace(owner), strings.TrimSpace(workloadID), after, limit+1)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	out := make([]map[string]any, 0, limit+1)
	last := after
	for rows.Next() {
		var operationID, kind, status string
		var coreSequence, publicSequence int64
		var readback []byte
		var at time.Time
		if err := rows.Scan(&operationID, &coreSequence, &publicSequence, &kind, &status, &readback, &at); err != nil {
			return nil, after, err
		}
		event := map[string]any{"kind": kind, "type": workloadDeploymentEventType(kind, status), "status": normalizeDeploymentStatusForStorage(status), "at": at.UTC().Format(time.RFC3339Nano)}
		var actual map[string]any
		if len(readback) != 0 && string(readback) != "null" && json.Unmarshal(readback, &actual) == nil && len(actual) != 0 {
			event["actual"] = normalizeWorkloadActual(actual)
		}
		out = append(out, normalizeStoredDeploymentEvent(owner, workloadID, operationID, coreSequence, publicSequence, event))
		last = publicSequence
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
	GetDeployment(context.Context, string, string) (map[string]any, bool, error)
	ListDeploymentEvents(context.Context, string, string, int64, int) ([]map[string]any, int64, error)
}
