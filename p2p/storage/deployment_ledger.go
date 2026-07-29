package storage

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcore"
)

type deploymentRow struct {
	owner, workload, operation, status, targetKind string
	updated                                        time.Time
	object                                         map[string]any
	revision                                       int64
	leaseOwner                                     string
	leaseUntil                                     time.Time
}
type deploymentEventRow struct {
	owner, workload, operation string
	sequence, publicSequence   int64
	event                      map[string]any
}

type deploymentPageCursor struct {
	UpdatedAt  time.Time `json:"updated_at"`
	WorkloadID string    `json:"workload_id"`
}

func encodeDeploymentPageToken(updated time.Time, workload string) string {
	raw, _ := json.Marshal(deploymentPageCursor{UpdatedAt: updated.UTC(), WorkloadID: workload})
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeDeploymentPageToken(raw string) (deploymentPageCursor, error) {
	if raw == "" {
		return deploymentPageCursor{}, nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(raw)
	if err == nil {
		var cursor deploymentPageCursor
		if json.Unmarshal(decoded, &cursor) == nil && !cursor.UpdatedAt.IsZero() {
			cursor.UpdatedAt = cursor.UpdatedAt.UTC()
			return cursor, nil
		}
	}
	// Accept the pre-opaque timestamp token for clients that held a page open
	// across the rollout.
	legacy, legacyErr := time.Parse(time.RFC3339Nano, raw)
	if legacyErr != nil {
		return deploymentPageCursor{}, fmt.Errorf("invalid deployment page token")
	}
	return deploymentPageCursor{UpdatedAt: legacy.UTC()}, nil
}

func encodeJSON(v any) ([]byte, error) { return json.Marshal(v) }
func decodeJSON(raw []byte) (map[string]any, error) {
	var out map[string]any
	err := json.Unmarshal(raw, &out)
	return out, err
}

func (s *DatabaseStore) UpsertDeploymentMutation(ctx context.Context, owner string, m agentcore.DeploymentMutation) error {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("deployment owner is required")
	}
	obj := agentcoreDeploymentObject(m)
	if obj == nil {
		return fmt.Errorf("deployment operation is required")
	}
	wid, _ := obj["workload_id"].(string)
	opID, _ := obj["latest_operation_id"].(string)
	raw, err := encodeJSON(obj)
	if err != nil {
		return err
	}
	actual, _ := encodeJSON(agentcore.SanitizedDeploymentActual(m.Actual))
	quote, _ := encodeJSON(m.Quote)
	operation, _ := encodeJSON(agentcore.SanitizedDeploymentOperation(m.Operation))
	return s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var previousRaw []byte
		var previousActual, previousQuote []byte
		if scanErr := tx.QueryRowContext(ctx, `SELECT object_json,actual_json,quote_json FROM p2p_agent_deployments WHERE owner_id=$1 AND workload_id=$2`, owner, wid).Scan(&previousRaw, &previousActual, &previousQuote); scanErr == nil {
			if previous, decodeErr := decodeJSON(previousRaw); decodeErr == nil {
				// A terminal operation is immutable. Core may replay an older
				// pending/running/aborting mutation after it has already emitted
				// its terminal result; accepting it would make the deployment a
				// reconciliation candidate again.
				if terminalDeploymentReplay(previous, obj) {
					return nil
				}
				obj = agentcore.MergeDeploymentObject(previous, obj)
				raw, _ = encodeJSON(obj)
			}
			if m.Actual == nil && len(previousActual) > 0 {
				actual = previousActual
			}
			if m.Quote == nil && len(previousQuote) > 0 {
				quote = previousQuote
			}
		} else if scanErr != sql.ErrNoRows {
			return scanErr
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO p2p_agent_deployments(owner_id,workload_id,operation_id,status,target_kind,object_json,operation_json,actual_json,quote_json,created_at,updated_at)
VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8::jsonb,$9::jsonb,NOW(),NOW())
ON CONFLICT(owner_id,workload_id) DO UPDATE SET operation_id=EXCLUDED.operation_id,status=EXCLUDED.status,target_kind=EXCLUDED.target_kind,object_json=EXCLUDED.object_json,operation_json=EXCLUDED.operation_json,actual_json=EXCLUDED.actual_json,quote_json=EXCLUDED.quote_json,revision=p2p_agent_deployments.revision+1,lease_owner='',lease_expires_at=NULL,updated_at=NOW()`, owner, wid, opID, obj["status"], obj["target_kind"], raw, nullJSON(operation), nullJSON(actual), nullJSON(quote))
		return err
	})
}

func nullJSON(raw []byte) string {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	return string(raw)
}

func (s *DatabaseStore) UpsertDeploymentEvent(ctx context.Context, owner string, event map[string]any) error {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("deployment owner is required")
	}
	wid, _ := event["workload_id"].(string)
	opID, _ := event["operation_id"].(string)
	seq := int64FromAny(event["sequence"])
	if wid == "" || opID == "" || seq <= 0 {
		return fmt.Errorf("deployment event linkage is invalid")
	}
	return s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var currentOperation string
		if lookupErr := tx.QueryRowContext(ctx, `SELECT operation_id FROM p2p_agent_deployments WHERE owner_id=$1 AND workload_id=$2 FOR UPDATE`, owner, wid).Scan(&currentOperation); lookupErr == sql.ErrNoRows {
			return fmt.Errorf("deployment event deployment is missing")
		} else if lookupErr != nil {
			return lookupErr
		} else if currentOperation != "" && currentOperation != opID {
			return fmt.Errorf("deployment event operation is stale")
		}
		var existingWorkload string
		if lookupErr := tx.QueryRowContext(ctx, `SELECT workload_id FROM p2p_agent_deployment_events WHERE owner_id=$1 AND operation_id=$2 AND sequence=$3`, owner, opID, seq).Scan(&existingWorkload); lookupErr == nil {
			if existingWorkload != wid {
				return fmt.Errorf("deployment event linkage conflict")
			}
			return nil
		} else if lookupErr != sql.ErrNoRows {
			return lookupErr
		}
		var priorSequence sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(sequence) FROM p2p_agent_deployment_events WHERE owner_id=$1 AND workload_id=$2 AND operation_id=$3`, owner, wid, opID).Scan(&priorSequence); err != nil {
			return err
		}
		statusMayApply := !priorSequence.Valid || seq > priorSequence.Int64
		if _, err := tx.ExecContext(ctx, `INSERT INTO p2p_agent_deployment_event_cursors(owner_id,workload_id,last_sequence,updated_at)
			VALUES($1,$2,0,NOW()) ON CONFLICT(owner_id,workload_id) DO NOTHING`, owner, wid); err != nil {
			return err
		}
		var publicSequence int64
		if err := tx.QueryRowContext(ctx, `UPDATE p2p_agent_deployment_event_cursors
			SET last_sequence=last_sequence+1,updated_at=NOW()
			WHERE owner_id=$1 AND workload_id=$2
			RETURNING last_sequence`, owner, wid).Scan(&publicSequence); err != nil {
			return err
		}
		persistedEvent := sanitizedDeploymentEvent(owner, wid, event, publicSequence)
		raw, err := encodeJSON(persistedEvent)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_deployment_events(owner_id,workload_id,operation_id,sequence,public_sequence,event_id,event_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,NOW())`, owner, wid, opID, seq, publicSequence, persistedEvent["event_id"], raw)
		if err != nil {
			return err
		}
		var objectRaw []byte
		if scanErr := tx.QueryRowContext(ctx, `SELECT object_json FROM p2p_agent_deployments WHERE owner_id=$1 AND workload_id=$2`, owner, wid).Scan(&objectRaw); scanErr == nil {
			obj, decodeErr := decodeJSON(objectRaw)
			if decodeErr != nil {
				return decodeErr
			}
			if status := stringValue(event["status"]); status != "" && statusMayApply && deploymentEventStatusCanApply(stringValue(obj["status"]), status) {
				obj["raw_status"] = status
				normalized := normalizeDeploymentStatusForStorage(status)
				if stringValue(event["kind"]) == "destroy" && normalized == "succeeded" {
					normalized = "destroyed"
				}
				obj["status"] = normalized
			}
			obj["last_synced"] = time.Now().UTC().Format(time.RFC3339Nano)
			updated, marshalErr := encodeJSON(obj)
			if marshalErr != nil {
				return marshalErr
			}
			_, err = tx.ExecContext(ctx, `UPDATE p2p_agent_deployments SET object_json=$1::jsonb, status=$2, updated_at=NOW() WHERE owner_id=$3 AND workload_id=$4`, updated, obj["status"], owner, wid)
		}
		return err
	})
}

func normalizeDeploymentStatusForStorage(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "pending", "queued", "waiting", "waiting_user":
		return "pending"
	case "running", "dispatching", "in_progress", "in-progress":
		return "running"
	case "succeeded", "completed", "success", "done":
		return "succeeded"
	case "failed", "error", "cancelled", "canceled":
		return "failed"
	default:
		return strings.ToLower(strings.TrimSpace(raw))
	}
}

func sanitizedDeploymentEvent(owner, workload string, event map[string]any, publicSequence int64) map[string]any {
	opID := stringValue(event["operation_id"])
	coreSequence := int64FromAny(event["sequence"])
	out := map[string]any{
		"event_id":     agentcore.DeploymentEventID(owner, workload, opID, coreSequence),
		"workload_id":  workload,
		"operation_id": opID,
		"sequence":     publicSequence,
		"type":         agentcore.SanitizedDeploymentEventKind(stringValue(event["kind"])),
		"status":       stringValue(event["status"]),
		"message":      "",
		"occurred_at":  event["at"],
	}
	if actual, ok := event["actual"].(map[string]any); ok && actual != nil {
		out["actual"] = agentcore.SanitizedDeploymentActual(actual)
	}
	return out
}

func normalizeStoredDeploymentEvent(owner, workload, operation string, coreSequence, publicSequence int64, raw map[string]any) map[string]any {
	event := map[string]any{}
	for key, value := range raw {
		event[key] = value
	}
	if publicSequence <= 0 {
		publicSequence = coreSequence
	}
	event["event_id"] = agentcore.DeploymentEventID(owner, workload, operation, coreSequence)
	event["workload_id"] = workload
	event["operation_id"] = operation
	event["sequence"] = publicSequence
	if stringValue(event["type"]) == "" {
		event["type"] = stringValue(event["kind"])
	}
	event["type"] = agentcore.SanitizedDeploymentEventKind(stringValue(event["type"]))
	delete(event, "kind")
	event["message"] = ""
	if event["occurred_at"] == nil {
		event["occurred_at"] = event["at"]
	}
	delete(event, "at")
	if actual, ok := event["actual"].(map[string]any); ok {
		event["actual"] = agentcore.SanitizedDeploymentActual(actual)
	}
	return event
}

func deploymentEventStatusCanApply(current, incoming string) bool {
	current = normalizeDeploymentStatusForStorage(current)
	incoming = normalizeDeploymentStatusForStorage(incoming)
	if current == "succeeded" || current == "failed" || current == "destroyed" {
		return current == incoming
	}
	return true
}

// terminalDeploymentReplay identifies a same-operation mutation that arrived
// after that operation reached a terminal projection. A different operation is
// intentionally not fenced: a destroy operation is a valid successor to a
// successful apply operation for the same workload.
func terminalDeploymentReplay(previous, current map[string]any) bool {
	previousOperation := stringValue(previous["latest_operation_id"])
	return previousOperation != "" && previousOperation == stringValue(current["latest_operation_id"]) && isTerminalDeploymentStatus(stringValue(previous["status"]))
}

func isTerminalDeploymentStatus(status string) bool {
	switch normalizeDeploymentStatusForStorage(status) {
	case "succeeded", "failed", "destroyed":
		return true
	default:
		return false
	}
}

func (s *DatabaseStore) ListDeployments(ctx context.Context, owner string, opts agentcore.DeploymentListOptions) ([]map[string]any, string, error) {
	limit := agentcorePageSize(opts.PageSize)
	args := []any{owner}
	where := []string{"owner_id=$1"}
	if token := strings.TrimSpace(opts.PageToken); token != "" {
		cursor, err := decodeDeploymentPageToken(token)
		if err != nil {
			return nil, "", fmt.Errorf("invalid deployment page token")
		}
		args = append(args, cursor.UpdatedAt)
		where = append(where, fmt.Sprintf("(updated_at<$%d", len(args)))
		if cursor.WorkloadID != "" {
			args = append(args, cursor.WorkloadID)
			where[len(where)-1] += fmt.Sprintf(" OR (updated_at=$%d AND workload_id<$%d)", len(args)-1, len(args))
		}
		where[len(where)-1] += ")"
	}
	if opts.Status != "" {
		args = append(args, opts.Status)
		where = append(where, fmt.Sprintf("status=$%d", len(args)))
	}
	if opts.TargetKind != "" {
		args = append(args, opts.TargetKind)
		where = append(where, fmt.Sprintf("target_kind=$%d", len(args)))
	}
	query := `SELECT object_json,updated_at,workload_id FROM p2p_agent_deployments WHERE ` + strings.Join(where, " AND ") + ` ORDER BY updated_at DESC,workload_id DESC LIMIT ` + strconv.Itoa(limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw []byte
		var updated time.Time
		var workload string
		if err := rows.Scan(&raw, &updated, &workload); err != nil {
			return nil, "", err
		}
		obj, err := decodeJSON(raw)
		if err != nil {
			return nil, "", err
		}
		obj["__deployment_cursor_updated_at"] = updated
		obj["__deployment_cursor_workload_id"] = workload
		out = append(out, agentcore.StripDeploymentInternalFields(obj))
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		if len(out) > 0 {
			last := out[len(out)-1]
			updated, _ := last["__deployment_cursor_updated_at"].(time.Time)
			workload := stringValue(last["__deployment_cursor_workload_id"])
			next = encodeDeploymentPageToken(updated, workload)
		}
	}
	for _, obj := range out {
		delete(obj, "__deployment_cursor_updated_at")
		delete(obj, "__deployment_cursor_workload_id")
	}
	return out, next, nil
}

func (s *DatabaseStore) GetDeployment(ctx context.Context, owner, workload string) (map[string]any, bool, error) {
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT object_json FROM p2p_agent_deployments WHERE owner_id=$1 AND workload_id=$2`, owner, workload).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	obj, err := decodeJSON(raw)
	return agentcore.StripDeploymentInternalFields(obj), err == nil, err
}

func (s *DatabaseStore) ListDeploymentEvents(ctx context.Context, owner, workload string, after int64, limit int) ([]map[string]any, int64, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_json,operation_id,sequence,COALESCE(public_sequence,sequence) FROM p2p_agent_deployment_events WHERE owner_id=$1 AND workload_id=$2 AND COALESCE(public_sequence,sequence)>$3 ORDER BY COALESCE(public_sequence,sequence) LIMIT $4`, owner, workload, after, limit+1)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	out := []map[string]any{}
	last := after
	for rows.Next() {
		var raw []byte
		var operation string
		var coreSequence, publicSequence int64
		if err := rows.Scan(&raw, &operation, &coreSequence, &publicSequence); err != nil {
			return nil, after, err
		}
		obj, err := decodeJSON(raw)
		if err != nil {
			return nil, after, err
		}
		obj = normalizeStoredDeploymentEvent(owner, workload, operation, coreSequence, publicSequence, obj)
		out = append(out, obj)
		last = int64FromAny(obj["sequence"])
	}
	if len(out) > limit {
		out = out[:limit]
		last = int64FromAny(out[len(out)-1]["sequence"])
	}
	return out, last, rows.Err()
}

func (s *DatabaseStore) LastDeploymentOperationSequence(ctx context.Context, owner, workload, operation string) (int64, error) {
	var sequence sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT MAX(sequence) FROM p2p_agent_deployment_events WHERE owner_id=$1 AND workload_id=$2 AND operation_id=$3`, owner, workload, operation).Scan(&sequence)
	if err != nil {
		return 0, err
	}
	if !sequence.Valid {
		return 0, nil
	}
	return sequence.Int64, nil
}

func (s *DatabaseStore) DeploymentCandidates(ctx context.Context, owner string, limit int) ([]agentcore.DeploymentReconcileCandidate, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := s.db.QueryContext(ctx, `SELECT workload_id,operation_id,status FROM p2p_agent_deployments WHERE owner_id=$1 AND status IN ('pending','running') ORDER BY updated_at LIMIT $2`, owner, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []agentcore.DeploymentReconcileCandidate{}
	for rows.Next() {
		var c agentcore.DeploymentReconcileCandidate
		if err := rows.Scan(&c.WorkloadID, &c.OperationID, &c.Status); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *DatabaseStore) ClaimDeploymentBatch(ctx context.Context, owner, worker string, leaseMillis int64, limit int) ([]agentcore.DeploymentReconcileCandidate, error) {
	if limit <= 0 {
		limit = 50
	}
	if leaseMillis <= 0 {
		leaseMillis = 30000
	}
	rows, err := s.db.QueryContext(ctx, `WITH claimed AS (SELECT owner_id,workload_id FROM p2p_agent_deployments WHERE owner_id=$1 AND status IN ('pending','running') AND (lease_owner='' OR lease_expires_at IS NULL OR lease_expires_at<=NOW()) ORDER BY updated_at FOR UPDATE SKIP LOCKED LIMIT $2) UPDATE p2p_agent_deployments d SET lease_owner=$3,lease_expires_at=NOW()+(($4::bigint)*INTERVAL '1 millisecond'),revision=d.revision+1 FROM claimed c WHERE d.owner_id=c.owner_id AND d.workload_id=c.workload_id RETURNING d.workload_id,d.operation_id,d.status,d.revision`, owner, limit, worker, leaseMillis)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []agentcore.DeploymentReconcileCandidate{}
	for rows.Next() {
		var c agentcore.DeploymentReconcileCandidate
		if err := rows.Scan(&c.WorkloadID, &c.OperationID, &c.Status, &c.Revision); err != nil {
			return nil, err
		}
		c.LeaseOwner = worker
		out = append(out, c)
	}
	return out, rows.Err()
}
func (s *DatabaseStore) ReleaseDeploymentLease(ctx context.Context, owner, workload, worker string, revision int64, terminal bool) error {
	result, err := s.db.ExecContext(ctx, `UPDATE p2p_agent_deployments SET lease_owner='',lease_expires_at=NULL,revision=revision+1 WHERE owner_id=$1 AND workload_id=$2 AND lease_owner=$3 AND revision=$4`, owner, workload, worker, revision)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil {
		return err
	} else if affected != 1 {
		return agentcore.ErrDeploymentLeaseCAS
	}
	return nil
}

// CommitDeploymentReconciliation updates the operation/actual projection and
// releases its lease in one owner/revision-guarded transaction. A takeover
// therefore fences stale workers before they can persist a readback.
func (s *DatabaseStore) CommitDeploymentReconciliation(ctx context.Context, owner, workload, worker string, revision int64, m agentcore.DeploymentMutation, events ...map[string]any) error {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return fmt.Errorf("deployment owner is required")
	}
	obj := agentcoreDeploymentObject(m)
	if obj == nil {
		return fmt.Errorf("deployment operation is required")
	}
	wid, _ := obj["workload_id"].(string)
	if wid == "" || wid != workload {
		return fmt.Errorf("deployment workload linkage is invalid")
	}
	opID, _ := obj["latest_operation_id"].(string)
	raw, err := encodeJSON(obj)
	if err != nil {
		return err
	}
	actual, _ := encodeJSON(agentcore.SanitizedDeploymentActual(m.Actual))
	quote, _ := encodeJSON(m.Quote)
	operation, _ := encodeJSON(agentcore.SanitizedDeploymentOperation(m.Operation))
	return s.writer.Do(s.db, nil, func(tx *sql.Tx) error {
		var previousRaw []byte
		var previousActual, previousQuote []byte
		var currentRevision int64
		var currentLeaseOwner string
		if scanErr := tx.QueryRowContext(ctx, `SELECT object_json,actual_json,quote_json,revision,lease_owner FROM p2p_agent_deployments WHERE owner_id=$1 AND workload_id=$2 FOR UPDATE`, owner, wid).Scan(&previousRaw, &previousActual, &previousQuote, &currentRevision, &currentLeaseOwner); scanErr != nil {
			if scanErr == sql.ErrNoRows {
				return agentcore.ErrDeploymentLeaseCAS
			}
			return scanErr
		}
		if currentLeaseOwner != worker || currentRevision != revision {
			return agentcore.ErrDeploymentLeaseCAS
		}
		terminalReplay := false
		if previous, decodeErr := decodeJSON(previousRaw); decodeErr == nil {
			if terminalDeploymentReplay(previous, obj) {
				terminalReplay = true
				obj = previous
				raw = previousRaw
				actual = previousActual
				quote = previousQuote
				operation, _ = encodeJSON(obj["current_operation"])
			} else {
				obj = agentcore.MergeDeploymentObject(previous, obj)
				raw, _ = encodeJSON(obj)
			}
		}
		if m.Actual == nil && len(previousActual) > 0 {
			actual = previousActual
		}
		if m.Quote == nil && len(previousQuote) > 0 {
			quote = previousQuote
		}
		if !terminalReplay {
			for _, event := range events {
				if err := persistDeploymentEventTx(ctx, tx, owner, workload, opID, event); err != nil {
					return err
				}
			}
		}
		result, execErr := tx.ExecContext(ctx, `UPDATE p2p_agent_deployments SET operation_id=$1,status=$2,target_kind=$3,object_json=$4::jsonb,operation_json=$5::jsonb,actual_json=$6::jsonb,quote_json=$7::jsonb,revision=revision+1,lease_owner='',lease_expires_at=NULL,updated_at=NOW() WHERE owner_id=$8 AND workload_id=$9 AND lease_owner=$10 AND revision=$11`, opID, obj["status"], obj["target_kind"], raw, nullJSON(operation), nullJSON(actual), nullJSON(quote), owner, wid, worker, revision)
		if execErr != nil {
			return execErr
		}
		affected, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if affected != 1 {
			return agentcore.ErrDeploymentLeaseCAS
		}
		return nil
	})
}

// persistDeploymentEventTx is used only by the guarded reconciliation commit.
// The lease must already be locked and verified by the caller. Existing
// sequence rows are treated as idempotent replays; a different workload link
// is rejected so sparse readbacks cannot corrupt ownership.
func persistDeploymentEventTx(ctx context.Context, tx *sql.Tx, owner, workload, expectedOperation string, event map[string]any) error {
	opID := stringValue(event["operation_id"])
	seq := int64FromAny(event["sequence"])
	if strings.TrimSpace(workload) == "" || opID == "" || seq <= 0 {
		return fmt.Errorf("deployment event linkage is invalid")
	}
	if expectedOperation != "" && opID != expectedOperation {
		return fmt.Errorf("deployment event operation is stale")
	}
	if linked := stringValue(event["workload_id"]); linked != "" && linked != workload {
		return fmt.Errorf("deployment event linkage conflict")
	}
	var publicSequence int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(COALESCE(public_sequence,sequence)),0)+1 FROM p2p_agent_deployment_events WHERE owner_id=$1 AND workload_id=$2`, owner, workload).Scan(&publicSequence); err != nil {
		return err
	}
	persisted := sanitizedDeploymentEvent(owner, workload, event, publicSequence)
	raw, err := encodeJSON(persisted)
	if err != nil {
		return err
	}
	var existingWorkload string
	lookupErr := tx.QueryRowContext(ctx, `SELECT workload_id FROM p2p_agent_deployment_events WHERE owner_id=$1 AND operation_id=$2 AND sequence=$3`, owner, opID, seq).Scan(&existingWorkload)
	if lookupErr == nil {
		if existingWorkload != workload {
			return fmt.Errorf("deployment event linkage conflict")
		}
		return nil
	}
	if lookupErr != sql.ErrNoRows {
		return lookupErr
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO p2p_agent_deployment_events(owner_id,workload_id,operation_id,sequence,public_sequence,event_id,event_json,created_at) VALUES($1,$2,$3,$4,$5,$6,$7::jsonb,NOW()) ON CONFLICT(owner_id,operation_id,sequence) DO NOTHING`, owner, workload, opID, seq, publicSequence, persisted["event_id"], raw)
	return err
}

func int64FromAny(v any) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	case uint64:
		return int64(n)
	}
	return 0
}
func agentcorePageSize(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}
func stringValue(v any) string { s, _ := v.(string); return s }

// MemoryStore implementation is intentionally test-only and mirrors owner
// fencing and idempotent event linkage used by PostgreSQL.
func (s *MemoryStore) UpsertDeploymentMutation(_ context.Context, owner string, m agentcore.DeploymentMutation) error {
	obj := agentcoreDeploymentObject(m)
	if obj == nil {
		return fmt.Errorf("deployment operation is required")
	}
	wid, _ := obj["workload_id"].(string)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.deployments == nil {
		s.deployments = map[string]deploymentRow{}
	}
	old := s.deployments[owner+"\x00"+wid]
	if terminalDeploymentReplay(old.object, obj) {
		return nil
	}
	obj = agentcore.MergeDeploymentObject(old.object, obj)
	rev := old.revision + 1
	if rev <= 0 {
		rev = 1
	}
	s.deployments[owner+"\x00"+wid] = deploymentRow{owner: owner, workload: wid, operation: stringValue(obj["latest_operation_id"]), status: stringValue(obj["status"]), targetKind: stringValue(obj["target_kind"]), updated: time.Now(), object: obj, revision: rev}
	return nil
}
func (s *MemoryStore) UpsertDeploymentEvent(_ context.Context, owner string, event map[string]any) error {
	wid := stringValue(event["workload_id"])
	op := stringValue(event["operation_id"])
	seq := int64FromAny(event["sequence"])
	if wid == "" || op == "" || seq <= 0 {
		return fmt.Errorf("deployment event linkage is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	deployment, exists := s.deployments[owner+"\x00"+wid]
	if !exists {
		return fmt.Errorf("deployment event deployment is missing")
	}
	if deployment.operation != "" && deployment.operation != op {
		return fmt.Errorf("deployment event operation is stale")
	}
	if s.deploymentEvents == nil {
		s.deploymentEvents = map[string]deploymentEventRow{}
	}
	key := fmt.Sprintf("%s\x00%s\x00%d", owner, op, seq)
	if old, ok := s.deploymentEvents[key]; ok {
		if old.workload != wid {
			return fmt.Errorf("deployment event linkage conflict")
		}
		return nil
	}
	priorSequence := int64(0)
	for _, existing := range s.deploymentEvents {
		if existing.owner == owner && existing.workload == wid && existing.operation == op && existing.sequence > priorSequence {
			priorSequence = existing.sequence
		}
	}
	statusMayApply := seq > priorSequence
	publicSequence := int64(1)
	for _, existing := range s.deploymentEvents {
		if existing.owner == owner && existing.workload == wid && existing.publicSequence >= publicSequence {
			publicSequence = existing.publicSequence + 1
		}
	}
	persistedEvent := sanitizedDeploymentEvent(owner, wid, event, publicSequence)
	s.deploymentEvents[key] = deploymentEventRow{owner: owner, workload: wid, operation: op, sequence: seq, publicSequence: publicSequence, event: persistedEvent}
	if deployment, ok := s.deployments[owner+"\x00"+wid]; ok {
		if status := stringValue(event["status"]); status != "" && statusMayApply && deploymentEventStatusCanApply(stringValue(deployment.object["status"]), status) {
			deployment.object["raw_status"] = status
			normalized := normalizeDeploymentStatusForStorage(status)
			if stringValue(event["kind"]) == "destroy" && normalized == "succeeded" {
				normalized = "destroyed"
			}
			deployment.object["status"] = normalized
			deployment.status = stringValue(deployment.object["status"])
		}
		deployment.object["last_synced"] = time.Now().UTC().Format(time.RFC3339Nano)
		deployment.updated = time.Now()
		s.deployments[owner+"\x00"+wid] = deployment
	}
	return nil
}
func (s *MemoryStore) ListDeployments(_ context.Context, owner string, opts agentcore.DeploymentListOptions) ([]map[string]any, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := []deploymentRow{}
	var cursor deploymentPageCursor
	if token := strings.TrimSpace(opts.PageToken); token != "" {
		var err error
		cursor, err = decodeDeploymentPageToken(token)
		if err != nil {
			return nil, "", fmt.Errorf("invalid deployment page token")
		}
	}
	for _, r := range s.deployments {
		beforeCursor := cursor.UpdatedAt.IsZero() || r.updated.Before(cursor.UpdatedAt) || (r.updated.Equal(cursor.UpdatedAt) && cursor.WorkloadID != "" && r.workload < cursor.WorkloadID)
		if r.owner == owner && beforeCursor && (opts.Status == "" || r.status == opts.Status) && (opts.TargetKind == "" || r.targetKind == opts.TargetKind) {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].updated.Equal(rows[j].updated) {
			return rows[i].workload > rows[j].workload
		}
		return rows[i].updated.After(rows[j].updated)
	})
	limit := agentcorePageSize(opts.PageSize)
	next := ""
	if len(rows) > limit {
		next = encodeDeploymentPageToken(rows[limit-1].updated, rows[limit-1].workload)
		rows = rows[:limit]
	}
	out := []map[string]any{}
	for _, r := range rows {
		out = append(out, agentcore.StripDeploymentInternalFields(r.object))
	}
	return out, next, nil
}
func (s *MemoryStore) GetDeployment(_ context.Context, owner, wid string) (map[string]any, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.deployments[owner+"\x00"+wid]
	return agentcore.StripDeploymentInternalFields(r.object), ok, nil
}
func (s *MemoryStore) ListDeploymentEvents(_ context.Context, owner, wid string, after int64, limit int) ([]map[string]any, int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows := []deploymentEventRow{}
	for _, r := range s.deploymentEvents {
		if r.owner == owner && r.workload == wid && r.publicSequence > after {
			rows = append(rows, r)
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].publicSequence < rows[j].publicSequence })
	if limit <= 0 {
		limit = 50
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	last := after
	out := []map[string]any{}
	for _, r := range rows {
		out = append(out, normalizeStoredDeploymentEvent(owner, wid, r.operation, r.sequence, r.publicSequence, r.event))
		last = r.publicSequence
	}
	return out, last, nil
}

func (s *MemoryStore) LastDeploymentOperationSequence(_ context.Context, owner, workload, operation string) (int64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var last int64
	for _, event := range s.deploymentEvents {
		if event.owner == owner && event.workload == workload && event.operation == operation && event.sequence > last {
			last = event.sequence
		}
	}
	return last, nil
}
func (s *MemoryStore) DeploymentCandidates(_ context.Context, owner string, limit int) ([]agentcore.DeploymentReconcileCandidate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []agentcore.DeploymentReconcileCandidate{}
	for _, r := range s.deployments {
		if r.owner == owner && (r.status == "pending" || r.status == "running") {
			out = append(out, agentcore.DeploymentReconcileCandidate{WorkloadID: r.workload, OperationID: r.operation, Status: r.status})
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}
func (s *MemoryStore) ClaimDeploymentBatch(_ context.Context, owner, worker string, leaseMillis int64, limit int) ([]agentcore.DeploymentReconcileCandidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := []agentcore.DeploymentReconcileCandidate{}
	for key, r := range s.deployments {
		if r.owner != owner || (r.status != "pending" && r.status != "running") || (r.leaseOwner != "" && r.leaseUntil.After(now)) {
			continue
		}
		r.revision++
		r.leaseOwner = worker
		r.leaseUntil = now.Add(time.Duration(leaseMillis) * time.Millisecond)
		s.deployments[key] = r
		out = append(out, agentcore.DeploymentReconcileCandidate{WorkloadID: r.workload, OperationID: r.operation, Status: r.status, Revision: r.revision, LeaseOwner: worker})
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}
func (s *MemoryStore) ReleaseDeploymentLease(_ context.Context, owner, workload, worker string, revision int64, terminal bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + "\x00" + workload
	r, ok := s.deployments[key]
	if !ok || r.leaseOwner != worker || r.revision != revision {
		return agentcore.ErrDeploymentLeaseCAS
	}
	r.revision++
	r.leaseOwner = ""
	r.leaseUntil = time.Time{}
	s.deployments[key] = r
	return nil
}

func (s *MemoryStore) CommitDeploymentReconciliation(_ context.Context, owner, workload, worker string, revision int64, m agentcore.DeploymentMutation, events ...map[string]any) error {
	obj := agentcoreDeploymentObject(m)
	if obj == nil {
		return fmt.Errorf("deployment operation is required")
	}
	wid, _ := obj["workload_id"].(string)
	if wid == "" || wid != workload {
		return fmt.Errorf("deployment workload linkage is invalid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + "\x00" + workload
	r, ok := s.deployments[key]
	if !ok || r.leaseOwner != worker || r.revision != revision {
		return agentcore.ErrDeploymentLeaseCAS
	}
	terminalReplay := terminalDeploymentReplay(r.object, obj)
	if terminalReplay {
		obj = r.object
	} else {
		obj = agentcore.MergeDeploymentObject(r.object, obj)
	}
	if s.deploymentEvents == nil {
		s.deploymentEvents = map[string]deploymentEventRow{}
	}
	pendingEvents := map[string]deploymentEventRow{}
	publicSequence := int64(1)
	for _, existing := range s.deploymentEvents {
		if existing.owner == owner && existing.workload == workload && existing.publicSequence >= publicSequence {
			publicSequence = existing.publicSequence + 1
		}
	}
	for _, event := range events {
		if terminalReplay {
			break
		}
		wid := stringValue(event["workload_id"])
		opID := stringValue(event["operation_id"])
		seq := int64FromAny(event["sequence"])
		if wid != "" && wid != workload || opID == "" || seq <= 0 {
			return fmt.Errorf("deployment event linkage is invalid")
		}
		if opID != stringValue(obj["latest_operation_id"]) {
			return fmt.Errorf("deployment event operation is stale")
		}
		key := fmt.Sprintf("%s\x00%s\x00%d", owner, opID, seq)
		if old, exists := s.deploymentEvents[key]; exists {
			if old.workload != workload {
				return fmt.Errorf("deployment event linkage conflict")
			}
			continue
		}
		if old, exists := pendingEvents[key]; exists {
			if old.workload != workload {
				return fmt.Errorf("deployment event linkage conflict")
			}
			continue
		}
		persisted := sanitizedDeploymentEvent(owner, workload, event, publicSequence)
		pendingEvents[key] = deploymentEventRow{owner: owner, workload: workload, operation: opID, sequence: seq, publicSequence: publicSequence, event: persisted}
		publicSequence++
	}
	for key, event := range pendingEvents {
		s.deploymentEvents[key] = event
	}
	r.operation = stringValue(obj["latest_operation_id"])
	r.status = stringValue(obj["status"])
	r.targetKind = stringValue(obj["target_kind"])
	r.object = obj
	r.updated = time.Now()
	r.revision++
	r.leaseOwner = ""
	r.leaseUntil = time.Time{}
	s.deployments[key] = r
	return nil
}

func agentcoreDeploymentObject(m agentcore.DeploymentMutation) map[string]any {
	return agentcore.DeploymentObject(m)
}
