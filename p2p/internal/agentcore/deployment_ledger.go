package agentcore

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ErrDeploymentLeaseCAS is returned when a worker no longer owns the
// claimed deployment revision (for example, after lease takeover).
var ErrDeploymentLeaseCAS = errors.New("deployment lease CAS rejected")

// DeploymentLedger is the durable, owner-fenced projection used by the
// Message Server deployment dashboard. Implementations must make mutation
// and operation persistence atomic and must never persist credentials.
type DeploymentLedger interface {
	UpsertDeploymentMutation(context.Context, string, DeploymentMutation) error
	UpsertDeploymentEvent(context.Context, string, map[string]any) error
	ListDeployments(context.Context, string, DeploymentListOptions) ([]map[string]any, string, error)
	GetDeployment(context.Context, string, string) (map[string]any, bool, error)
	ListDeploymentEvents(context.Context, string, string, int64, int) ([]map[string]any, int64, error)
	LastDeploymentOperationSequence(context.Context, string, string, string) (int64, error)
	DeploymentCandidates(context.Context, string, int) ([]DeploymentReconcileCandidate, error)
	ClaimDeploymentBatch(context.Context, string, string, int64, int) ([]DeploymentReconcileCandidate, error)
	ReleaseDeploymentLease(context.Context, string, string, string, int64, bool) error
	// CommitDeploymentReconciliation atomically persists the optional ordered
	// operation events with the owner/revision-fenced projection update. The
	// variadic form keeps existing callers that have no events source-compatible.
	CommitDeploymentReconciliation(context.Context, string, string, string, int64, DeploymentMutation, ...map[string]any) error
}

type DeploymentMutation struct {
	Operation map[string]any
	Actual    map[string]any
	Quote     map[string]any
}

type DeploymentListOptions struct {
	PageSize   int
	PageToken  string
	Status     string
	TargetKind string
}

type DeploymentReconcileCandidate struct {
	WorkloadID  string
	OperationID string
	Status      string
	Revision    int64
	LeaseOwner  string
}

func deploymentObject(m DeploymentMutation, now time.Time) map[string]any {
	op := m.Operation
	if op == nil {
		return nil
	}
	wid, _ := op["workload_id"].(string)
	planID, _ := op["plan_id"].(string)
	planDigest, _ := op["plan_digest"].(string)
	kind, _ := op["kind"].(string)
	rawStatus, _ := op["status"].(string)
	status := normalizeDeploymentStatus(rawStatus, kind)
	if kind == "destroy" && isTerminalStatus(rawStatus) {
		status = "destroyed"
	}
	targetKind, _ := op["target_kind"].(string)
	identity := deploymentIdentity(op, m.Actual)
	obj := map[string]any{
		"workload_id": wid, "plan_id": planID, "plan_digest": planDigest,
		"latest_operation_id": stringValue(op["operation_id"]), "latest_task_id": stringValue(op["task_id"]),
		"latest_confirmation_id": stringValue(op["confirmation_id"]), "target_kind": targetKind,
		"target": identity, "summary": "", "status": status, "raw_status": rawStatus,
		"progress": nil, "error": nil, "desired_server_count": desiredServerCount(identity),
		"actual_server_count": actualServerCount(m.Actual), "estimated_monthly_usd": quoteNumber(m.Quote, "estimated_monthly_usd"),
		"estimated_accrued_usd": nil, "created_at": firstTimestamp(op, "created_at", now),
		"updated_at": firstTimestamp(op, "updated_at", now), "last_synced": now.UTC().Format(time.RFC3339Nano),
		"active_from": nil, "destroyed_at": nil,
	}
	obj["current_operation"] = SanitizedDeploymentOperation(op)
	obj["actual"] = SanitizedDeploymentActual(m.Actual)
	if errCode := SanitizedDeploymentFailureCode(stringValue(op["failure_code"])); stringValue(op["failure_code"]) != "" {
		obj["error"] = map[string]any{"code": errCode, "summary": "Deployment failed"}
	}
	if kind == "apply" && (status == "succeeded" || status == "running") {
		obj["active_from"] = firstTimestamp(op, "updated_at", now)
	}
	if status == "destroyed" {
		obj["destroyed_at"] = firstTimestamp(op, "updated_at", now)
	}
	monthly := 0.0
	switch value := obj["estimated_monthly_usd"].(type) {
	case float64:
		monthly = value
	case float32:
		monthly = float64(value)
	case int:
		monthly = float64(value)
	case int64:
		monthly = float64(value)
	default:
		monthly = -1
	}
	if monthly >= 0 {
		if active, parseErr := time.Parse(time.RFC3339Nano, stringValue(obj["active_from"])); parseErr == nil && !active.IsZero() {
			end := now
			if destroyed := stringValue(obj["destroyed_at"]); destroyed != "" {
				if parsed, err := time.Parse(time.RFC3339Nano, destroyed); err == nil {
					end = parsed
				}
			}
			if end.After(active) {
				obj["estimated_accrued_usd"] = monthly / 730 * end.Sub(active).Hours()
			}
		}
	}
	return obj
}

func SanitizedDeploymentActual(actual map[string]any) map[string]any {
	if actual == nil {
		return nil
	}
	out := map[string]any{}
	for _, key := range []string{"workload_id", "revision", "state", "applied_plan_id", "applied_plan_digest", "readback_digest", "provider_version", "observed_at", "updated_at"} {
		if value, ok := actual[key]; ok {
			out[key] = value
		}
	}
	if identity, ok := actual["identity"].(map[string]any); ok {
		out["identity"] = deploymentIdentity(map[string]any{"desired_plan": map[string]any{"target": map[string]any{"identity": identity}}}, nil)
	}
	return out
}

// SanitizedDeploymentOperation deliberately omits desired plans, templates,
// command steps and secret-grant references from durable dashboard state.
func SanitizedDeploymentOperation(op map[string]any) map[string]any {
	if op == nil {
		return nil
	}
	allowed := []string{"operation_id", "workload_id", "plan_id", "kind", "plan_revision", "plan_digest", "target_kind", "task_id", "confirmation_id", "status", "revision", "failure_code", "created_at", "updated_at", "dispatch_epoch", "dispatch_lease_until"}
	out := map[string]any{}
	for _, key := range allowed {
		if value, ok := op[key]; ok {
			if key == "kind" {
				value = SanitizedDeploymentOperationKind(stringValue(value))
			} else if key == "failure_code" {
				value = SanitizedDeploymentFailureCode(stringValue(value))
			}
			out[key] = value
		}
	}
	return out
}

// DeploymentObject projects a Core mutation into the sanitized dashboard
// object. Storage adapters use this helper so memory and PostgreSQL agree.
func DeploymentObject(m DeploymentMutation) map[string]any { return deploymentObject(m, time.Now()) }

// MergeDeploymentObject carries forward durable fields when a later Core
// mutation is sparse (notably destroy operations). Terminal destroy computes
// accrued cost from the prior active rate before clearing the current rate.
func MergeDeploymentObject(previous, current map[string]any) map[string]any {
	if current == nil || previous == nil {
		return current
	}
	sameOperation := stringValue(previous["latest_operation_id"]) != "" && stringValue(previous["latest_operation_id"]) == stringValue(current["latest_operation_id"])
	for _, key := range []string{"target", "actual", "desired_server_count", "actual_server_count", "estimated_monthly_usd", "active_from"} {
		if current[key] == nil || (key == "target" && lenMap(current[key]) == 0) {
			if previous[key] != nil {
				current[key] = previous[key]
			}
		}
	}
	previousTotal, _ := deploymentNumber(previous["estimated_accrued_usd"])
	base, baseOK := deploymentNumber(previous["_accrued_base_usd"])
	if !sameOperation || !baseOK {
		if !sameOperation {
			base = previousTotal
		}
	}
	current["_accrued_base_usd"] = base
	if currentSegment, ok := deploymentNumber(current["estimated_accrued_usd"]); ok {
		current["estimated_accrued_usd"] = base + currentSegment
	} else if previousValue, ok := deploymentNumber(previous["estimated_accrued_usd"]); ok {
		current["estimated_accrued_usd"] = previousValue
	}
	currentKind := ""
	if operation, ok := current["current_operation"].(map[string]any); ok {
		currentKind = stringValue(operation["kind"])
	}
	if currentKind == "destroy" && stringValue(current["status"]) == "running" {
		current["estimated_accrued_usd"] = previousTotal + deploymentRateSegment(previous, current, previous["estimated_monthly_usd"], time.Now().UTC())
	}
	if stringValue(current["status"]) == "destroyed" {
		if monthly, ok := deploymentNumber(previous["estimated_monthly_usd"]); ok {
			if active, err := time.Parse(time.RFC3339Nano, stringValue(previous["active_from"])); err == nil {
				end := time.Now().UTC()
				if destroyed, parseErr := time.Parse(time.RFC3339Nano, stringValue(current["destroyed_at"])); parseErr == nil && destroyed.After(active) {
					end = destroyed
				}
				if end.After(active) {
					segmentStart := active
					if synced, parseErr := time.Parse(time.RFC3339Nano, stringValue(previous["last_synced"])); parseErr == nil && synced.After(segmentStart) {
						segmentStart = synced
					}
					segment := monthly / 730 * end.Sub(segmentStart).Hours()
					if segment < 0 {
						segment = 0
					}
					current["estimated_accrued_usd"] = previousTotal + segment
				}
			}
		}
		current["estimated_monthly_usd"] = nil
	}
	return current
}

func deploymentRateSegment(previous, current map[string]any, monthlyValue any, end time.Time) float64 {
	monthly, ok := deploymentNumber(monthlyValue)
	if !ok {
		return 0
	}
	start, err := time.Parse(time.RFC3339Nano, stringValue(previous["last_synced"]))
	if err != nil {
		start, err = time.Parse(time.RFC3339Nano, stringValue(previous["active_from"]))
	}
	if err != nil {
		return 0
	}
	if synced, parseErr := time.Parse(time.RFC3339Nano, stringValue(current["last_synced"])); parseErr == nil && synced.Before(end) {
		end = synced
	}
	if !end.After(start) {
		return 0
	}
	return monthly / 730 * end.Sub(start).Hours()
}

// StripDeploymentInternalFields keeps reconciliation bookkeeping out of the
// public deployment DTOs while retaining it in the durable projection.
func StripDeploymentInternalFields(object map[string]any) map[string]any {
	if object == nil {
		return nil
	}
	out := map[string]any{}
	for key, value := range object {
		if key != "_accrued_base_usd" {
			out[key] = value
		}
	}
	return out
}

func lenMap(value any) int {
	m, _ := value.(map[string]any)
	return len(m)
}

func deploymentNumber(value any) (float64, bool) {
	switch n := value.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case int32:
		return float64(n), true
	case uint:
		return float64(n), true
	case uint64:
		return float64(n), true
	case uint32:
		return float64(n), true
	default:
		return 0, false
	}
}

// DeploymentEventID is stable for a given owner/workload/operation Core
// sequence and does not expose upstream event identifiers.
func DeploymentEventID(owner, workload, operation string, sequence int64) string {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(owner+"\x00"+workload+"\x00"+operation+"\x00"+strconv.FormatInt(sequence, 10))).String()
}

// SanitizedDeploymentCode accepts only bounded lowercase identifier tokens.
// Unsafe upstream values map to a stable generic code; they are never
// truncated into a misleading persisted identifier.
func SanitizedDeploymentCode(raw, fallback string) string {
	value := strings.TrimSpace(raw)
	if value == "" || len(value) > 64 {
		return fallback
	}
	for i := 0; i < len(value); i++ {
		ch := value[i]
		if (ch < 'a' || ch > 'z') && (ch < '0' || ch > '9') && ch != '_' && ch != '-' && ch != '.' {
			return fallback
		}
	}
	return value
}

func SanitizedDeploymentFailureCode(raw string) string {
	value := SanitizedDeploymentCode(raw, "")
	if value == "" {
		return "deployment_failed"
	}
	switch value {
	case "provider_failed", "invalid_request", "not_found", "conflict", "timeout", "cancelled", "canceled", "failed", "error", "core_unavailable", "precondition_failed", "deployment_failed":
		return value
	default:
		return "deployment_failed"
	}
}

func SanitizedDeploymentEventKind(raw string) string {
	value := SanitizedDeploymentCode(raw, "")
	switch value {
	case "accepted", "queued", "dispatch", "started", "running", "progress", "complete", "completed", "succeeded", "failed", "error", "terminal", "destroy", "destroyed", "cancelled", "canceled", "late":
		return value
	default:
		return "unknown"
	}
}

func SanitizedDeploymentOperationKind(raw string) string {
	value := SanitizedDeploymentCode(raw, "")
	if value == "apply" || value == "destroy" {
		return value
	}
	return "unknown"
}

func deploymentIdentity(op, actual map[string]any) map[string]any {
	var source map[string]any
	if desired, ok := op["desired_plan"].(map[string]any); ok {
		if target, ok := desired["target"].(map[string]any); ok {
			source, _ = target["identity"].(map[string]any)
		}
	}
	if source == nil && actual != nil {
		source, _ = actual["identity"].(map[string]any)
	}
	allowed := []string{"kind", "aws_account_id", "aws_region", "instance_id", "cluster", "service", "image_digest", "desired_count", "aws_ecs_desired_count"}
	out := map[string]any{}
	for _, key := range allowed {
		if v, ok := source[key]; ok {
			out[key] = v
		}
	}
	return out
}

func stringValue(v any) string { s, _ := v.(string); return strings.TrimSpace(s) }

func firstTimestamp(m map[string]any, key string, fallback time.Time) string {
	if s := stringValue(m[key]); s != "" {
		return s
	}
	return fallback.UTC().Format(time.RFC3339Nano)
}

func quoteNumber(q map[string]any, key string) any {
	if q == nil {
		return nil
	}
	v, ok := q[key]
	if !ok || v == nil {
		return nil
	}
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int, int64, uint64:
		return n
	default:
		return nil
	}
}

func desiredServerCount(identity map[string]any) any {
	for _, key := range []string{"desired_count", "aws_ecs_desired_count"} {
		if n, ok := identity[key]; ok {
			return n
		}
	}
	if stringValue(identity["instance_id"]) != "" {
		return int64(1)
	}
	return nil
}

func actualServerCount(actual map[string]any) any {
	if actual == nil {
		return nil
	}
	identity, _ := actual["identity"].(map[string]any)
	return desiredServerCount(identity)
}

func normalizeDeploymentStatus(raw, kind string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	switch raw {
	case "pending", "queued", "waiting":
		return "pending"
	case "running", "dispatching", "in_progress", "in-progress":
		return "running"
	case "succeeded", "completed", "success", "done":
		return "succeeded"
	case "failed", "error", "cancelled", "canceled":
		return "failed"
	default:
		if kind == "destroy" && isTerminalStatus(raw) {
			return "destroyed"
		}
		return raw
	}
}

func isTerminalStatus(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "succeeded", "completed", "success", "done", "failed", "error", "cancelled", "canceled", "destroyed":
		return true
	default:
		return false
	}
}

func parsePageSize(n int) int {
	if n <= 0 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

func parsePageToken(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t, nil
	}
	if _, err := strconv.ParseInt(raw, 10, 64); err == nil {
		return time.Time{}, errors.New("unsupported page token")
	}
	return time.Time{}, errors.New("invalid page token")
}
