package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/internal/agentgateway"
	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"golang.org/x/sync/errgroup"
)

const (
	deliverablesListAction    = "agent.execution.v2.deliverables.list"
	cloudWorkerRecordKind     = "cloud_worker"
	defaultDeliverablesPage   = int64(20)
	maximumDeliverablesPage   = int64(20)
	deliverableDownloadAction = "agent.execution.v2.artifacts.download"
	deliverableDownloadChunk  = int64(512 << 10)
)

type deliverablesListRequest struct {
	pageSize  int64
	pageToken string
}

// listDeliverables is a Product-facing read projection over the existing Adam
// Execution V2 reads. It does not define another Agent RPC or own Agent state.
func (m *Module) listDeliverables(ctx context.Context, params map[string]any) (any, *actionbase.Error) {
	if m == nil || m.runner == nil {
		return nil, actionbase.StatusError(http.StatusBadGateway, "external native agent gateway is not configured")
	}
	if err := m.readinessError(); err != nil {
		return nil, actionbase.StatusError(http.StatusServiceUnavailable, "external native agent capability catalog is not ready")
	}
	request, err := parseDeliverablesListRequest(params)
	if err != nil {
		return nil, actionbase.BadRequest(err.Error())
	}
	runsResult, err := m.runner.Invoke(ctx, "agent.execution.v2.runs.list", map[string]any{
		"record_kind": cloudWorkerRecordKind,
		"page_size":   request.pageSize,
		"page_token":  request.pageToken,
	})
	if err != nil {
		return nil, externalAgentActionError(err)
	}
	runs, nextPageToken, err := deliverableRunsPage(runsResult)
	if err != nil {
		return nil, externalAgentActionError(err)
	}
	pages := make([][]map[string]any, len(runs))
	group, groupContext := errgroup.WithContext(ctx)
	group.SetLimit(4)
	for index, run := range runs {
		index, run := index, run
		group.Go(func() error {
			items, buildErr := m.deliverablesForRun(groupContext, run)
			if buildErr != nil {
				return buildErr
			}
			pages[index] = items
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, externalAgentActionError(err)
	}
	deliverables := make([]map[string]any, 0)
	for _, page := range pages {
		deliverables = append(deliverables, page...)
	}
	return map[string]any{"deliverables": deliverables, "next_page_token": nextPageToken}, nil
}

func parseDeliverablesListRequest(params map[string]any) (deliverablesListRequest, error) {
	request := deliverablesListRequest{pageSize: defaultDeliverablesPage}
	for field := range params {
		if field != "record_kind" && field != "page_size" && field != "page_token" {
			return request, fmt.Errorf("%s %s: is not allowed", deliverablesListAction, field)
		}
	}
	if kind, ok := params["record_kind"].(string); !ok || kind != cloudWorkerRecordKind {
		return request, fmt.Errorf("%s record_kind: must equal cloud_worker", deliverablesListAction)
	}
	if raw, present := params["page_size"]; present {
		value, ok := deliverableInt64(raw)
		if !ok || value < 1 || value > maximumDeliverablesPage {
			return request, fmt.Errorf("%s page_size: must be an integer from 1 to 20", deliverablesListAction)
		}
		request.pageSize = value
	}
	if raw, present := params["page_token"]; present {
		value, ok := raw.(string)
		if !ok || len(value) > 4096 {
			return request, fmt.Errorf("%s page_token: must be a bounded string", deliverablesListAction)
		}
		request.pageToken = value
	}
	return request, nil
}

func deliverableRunsPage(result map[string]any) ([]map[string]any, string, error) {
	runs, ok := deliverableObjects(result["runs"])
	if !ok {
		return nil, "", invalidDeliverablesResult("runs list is invalid")
	}
	nextPageToken, ok := result["next_page_token"].(string)
	if !ok {
		return nil, "", invalidDeliverablesResult("runs next page token is invalid")
	}
	return runs, nextPageToken, nil
}

func (m *Module) deliverablesForRun(ctx context.Context, run map[string]any) ([]map[string]any, error) {
	if deliverableString(run, "status") != "succeeded" {
		return nil, nil
	}
	cleanup, cleanupOK := deliverableObject(run["cleanup"])
	verifiedDestroyed, verifiedOK := cleanup["verified_destroyed"].(bool)
	if !cleanupOK || !verifiedOK || !verifiedDestroyed {
		return nil, nil
	}
	artifactIDs, ok := deliverableStrings(run["artifact_ids"])
	if !ok {
		return nil, invalidDeliverablesResult("run artifact IDs are invalid")
	}
	if len(artifactIDs) == 0 {
		return nil, nil
	}
	runID, executionID, planID := deliverableString(run, "run_id"), deliverableString(run, "execution_id"), deliverableString(run, "plan_id")
	planRevision, revisionOK := deliverableInt64(run["plan_revision"])
	if runID == "" || executionID != runID || planID == "" || !revisionOK || planRevision < 1 {
		return nil, invalidDeliverablesResult("run identity is invalid")
	}
	completedAt, err := time.Parse(time.RFC3339Nano, deliverableString(run, "updated_at"))
	if err != nil {
		return nil, invalidDeliverablesResult("run completion time is invalid")
	}
	planResult, err := m.runner.Invoke(ctx, "agent.execution.v2.plans.get", map[string]any{
		"record_kind": cloudWorkerRecordKind,
		"plan_id":     planID,
		"revision":    planRevision,
	})
	if err != nil {
		return nil, err
	}
	plan, ok := deliverableObject(planResult["plan"])
	returnedPlanRevision, returnedRevisionOK := deliverableInt64(plan["revision"])
	if !ok || deliverableString(plan, "plan_id") != planID || !returnedRevisionOK || returnedPlanRevision != planRevision ||
		deliverableString(plan, "digest") != deliverableString(run, "plan_digest") ||
		deliverableString(plan, "execution_id") != executionID ||
		deliverableString(plan, "task_id") != deliverableString(run, "task_id") ||
		deliverableString(plan, "confirmation_id") != deliverableString(run, "confirmation_id") ||
		deliverableString(plan, "conversation_id") != deliverableString(run, "conversation_id") ||
		deliverableString(plan, "turn_id") != deliverableString(run, "turn_id") ||
		!deliverableSameAuthority(run, plan) {
		return nil, invalidDeliverablesResult("plan binding is invalid")
	}
	objectiveSummary := deliverableString(plan, "objective_summary")
	if objectiveSummary == "" {
		return nil, invalidDeliverablesResult("plan summary is invalid")
	}
	retentionSeconds, ok := deliverableInt64(plan["artifact_retention_seconds"])
	if !ok || retentionSeconds < 1 || retentionSeconds > int64(math.MaxInt64)/int64(time.Second) {
		return nil, invalidDeliverablesResult("artifact retention is invalid")
	}
	items := make([]map[string]any, 0, len(artifactIDs))
	for _, artifactID := range artifactIDs {
		artifactResult, invokeErr := m.runner.Invoke(ctx, "agent.execution.v2.artifacts.get", map[string]any{
			"record_kind": cloudWorkerRecordKind,
			"artifact_id": artifactID,
		})
		if invokeErr != nil {
			return nil, invokeErr
		}
		artifact, objectOK := deliverableObject(artifactResult["artifact"])
		sizeBytes, sizeOK := deliverableInt64(artifact["size_bytes"])
		if !objectOK || deliverableString(artifact, "artifact_id") != artifactID ||
			deliverableString(artifact, "execution_id") != executionID || !deliverableSameAuthority(run, artifact) ||
			deliverableString(artifact, "status") != "verified" || !sizeOK || sizeBytes < 1 {
			return nil, invalidDeliverablesResult("artifact binding is invalid")
		}
		createdAt, parseErr := time.Parse(time.RFC3339Nano, deliverableString(artifact, "created_at"))
		if parseErr != nil || createdAt.After(completedAt) {
			return nil, invalidDeliverablesResult("artifact time is invalid")
		}
		expiresAt := createdAt.Add(time.Duration(retentionSeconds) * time.Second)
		items = append(items, map[string]any{
			"owner_id": run["owner_id"], "account_generation": run["account_generation"],
			"artifact_id": artifactID, "execution_id": executionID, "run_id": runID, "plan_id": planID,
			"task_id": deliverableString(run, "task_id"), "confirmation_id": deliverableString(run, "confirmation_id"),
			"conversation_id": deliverableString(run, "conversation_id"), "turn_id": deliverableString(run, "turn_id"),
			"objective_summary": objectiveSummary,
			"run_status":        deliverableString(run, "status"), "name": deliverableString(artifact, "name"),
			"kind": deliverableString(artifact, "kind"), "media_type": deliverableString(artifact, "media_type"),
			"size_bytes": sizeBytes, "sha256": deliverableString(artifact, "sha256"),
			"artifact_status": "verified", "created_at": createdAt.UTC().Format(time.RFC3339Nano),
			"completed_at":         completedAt.UTC().Format(time.RFC3339Nano),
			"retention_expires_at": expiresAt.UTC().Format(time.RFC3339Nano),
			"download": map[string]any{
				"action": deliverableDownloadAction, "record_kind": cloudWorkerRecordKind,
				"artifact_id": artifactID, "offset_bytes": int64(0), "max_chunk_bytes": deliverableDownloadChunk,
			},
		})
	}
	return items, nil
}

func invalidDeliverablesResult(reason string) error {
	return fmt.Errorf("%w: %s", agentgateway.ErrInvalidActionResult, reason)
}

func deliverableObject(value any) (map[string]any, bool) {
	result, ok := value.(map[string]any)
	return result, ok && result != nil
}

func deliverableObjects(value any) ([]map[string]any, bool) {
	switch values := value.(type) {
	case []map[string]any:
		return values, true
	case []any:
		result := make([]map[string]any, 0, len(values))
		for _, value := range values {
			object, ok := deliverableObject(value)
			if !ok {
				return nil, false
			}
			result = append(result, object)
		}
		return result, true
	default:
		return nil, false
	}
}

func deliverableStrings(value any) ([]string, bool) {
	switch values := value.(type) {
	case []string:
		return values, true
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return nil, false
			}
			result = append(result, text)
		}
		return result, true
	default:
		return nil, false
	}
}

func deliverableString(object map[string]any, field string) string {
	value, _ := object[field].(string)
	return strings.TrimSpace(value)
}

func deliverableSameAuthority(left, right map[string]any) bool {
	leftGeneration, leftOK := deliverableInt64(left["account_generation"])
	rightGeneration, rightOK := deliverableInt64(right["account_generation"])
	return deliverableString(left, "owner_id") != "" && deliverableString(left, "owner_id") == deliverableString(right, "owner_id") &&
		leftOK && rightOK && leftGeneration == rightGeneration && leftGeneration > 0
}

func deliverableInt64(value any) (int64, bool) {
	switch number := value.(type) {
	case int:
		return int64(number), true
	case int32:
		return int64(number), true
	case int64:
		return number, true
	case uint:
		if uint64(number) > math.MaxInt64 {
			return 0, false
		}
		return int64(number), true
	case uint32:
		return int64(number), true
	case uint64:
		if number > math.MaxInt64 {
			return 0, false
		}
		return int64(number), true
	case float64:
		if math.IsNaN(number) || math.IsInf(number, 0) || math.Trunc(number) != number || number < math.MinInt64 || number > math.MaxInt64 {
			return 0, false
		}
		return int64(number), true
	case json.Number:
		parsed, err := number.Int64()
		return parsed, err == nil
	default:
		return 0, false
	}
}
