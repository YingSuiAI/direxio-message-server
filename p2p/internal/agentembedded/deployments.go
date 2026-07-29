package agentembedded

import (
	"context"
	"net/http"
	"strings"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/confirmation"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
)

func (m *Module) deploymentHandler(action string) actionbase.Handler {
	return func(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
		if _, e := m.requireCapability(ctx, p, "deployments.server", m.cfg.Deployments != nil); e != nil {
			return nil, e
		}
		if m == nil || m.cfg.Deployments == nil {
			return unavailable(ctx, p)
		}
		if action == "agent.core.dashboard.get" {
			return m.dashboardGet(ctx, p)
		}
		o := m.owner()
		op := action[strings.LastIndex(action, ".")+1:]
		switch op {
		case "list":
			size, token, e := page(p)
			if e != nil {
				return nil, e
			}
			status, _ := optionalString(p, "status")
			kind, _ := optionalString(p, "target_kind")
			items, next, err := m.cfg.Deployments.ListDeployments(ctx, o, DeploymentListOptions{PageSize: size, PageToken: token, Status: status, TargetKind: kind})
			if err != nil {
				return nil, actionbase.InternalError(err)
			}
			return map[string]any{"deployments": items, "next_page_token": next}, nil
		case "get":
			id, e := requiredString(p, "workload_id")
			if e != nil {
				return nil, e
			}
			item, ok, err := m.cfg.Deployments.GetDeployment(ctx, o, id)
			if err != nil {
				return nil, actionbase.InternalError(err)
			}
			if !ok {
				return nil, actionbase.CodedError(http.StatusNotFound, "deployment_not_found", "deployment was not found")
			}
			deployment := make(map[string]any, len(item))
			for key, value := range item {
				if key != "current_operation" && key != "actual" {
					deployment[key] = value
				}
			}
			result := map[string]any{"deployment": deployment}
			if current := item["current_operation"]; current != nil {
				result["current_operation"] = current
			}
			if actual := item["actual"]; actual != nil {
				result["actual"] = actual
			}
			return result, nil
		case "events":
			id, e := requiredString(p, "workload_id")
			if e != nil {
				return nil, e
			}
			after, e := optionalInt64(p, "after_sequence")
			if e != nil {
				return nil, e
			}
			limit, e := optionalInt64(p, "page_size")
			if e != nil {
				return nil, e
			}
			if limit <= 0 {
				limit = 128
			}
			if limit > 256 {
				return nil, actionbase.BadRequest("limit is too large")
			}
			items, next, err := m.cfg.Deployments.ListDeploymentEvents(ctx, o, id, after, int(limit))
			if err != nil {
				return nil, actionbase.InternalError(err)
			}
			return map[string]any{"events": items, "next_after_sequence": next}, nil
		default:
			return unavailable(ctx, p)
		}
	}
}

func (m *Module) dashboardGet(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
	limit, e := optionalInt64(p, "recent_limit")
	if e != nil {
		return nil, e
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	owner := m.owner()
	recent, _, err := m.cfg.Deployments.ListDeployments(ctx, owner, DeploymentListOptions{PageSize: int(limit)})
	if err != nil {
		return nil, actionbase.InternalError(err)
	}
	summary := map[string]any{
		"deployment_pending": int64(0), "deployment_running": int64(0),
		"deployment_succeeded": int64(0), "deployment_failed": int64(0),
		"deployment_destroyed": int64(0), "task_pending": int64(0),
		"task_running": int64(0), "task_completed": int64(0),
		"task_failed": int64(0), "schedule_active": int64(0),
		"schedule_paused": int64(0), "confirmation_pending": int64(0),
		"server_count": int64(0), "estimated_monthly_usd": float64(0),
		"estimated_accrued_usd": float64(0),
	}
	warnings := []string{}
	deploymentToken := ""
	for page := 0; page < 1000; page++ {
		items, next, listErr := m.cfg.Deployments.ListDeployments(ctx, owner, DeploymentListOptions{PageSize: 200, PageToken: deploymentToken})
		if listErr != nil {
			return nil, actionbase.InternalError(listErr)
		}
		for _, item := range items {
			switch strings.TrimSpace(stringValue(item["status"])) {
			case "pending":
				increment(summary, "deployment_pending")
			case "running":
				increment(summary, "deployment_running")
			case "succeeded":
				increment(summary, "deployment_succeeded")
			case "failed":
				increment(summary, "deployment_failed")
			case "destroyed":
				increment(summary, "deployment_destroyed")
			}
			addNumber(summary, "server_count", item["actual_server_count"])
			addNumber(summary, "estimated_monthly_usd", item["estimated_monthly_usd"])
			addNumber(summary, "estimated_accrued_usd", item["estimated_accrued_usd"])
		}
		if next == "" {
			break
		}
		if next == deploymentToken || page == 999 {
			return nil, actionbase.InternalError(ErrUnavailable)
		}
		deploymentToken = next
	}
	if m.cfg.Tasks != nil {
		cursor := ""
		for page := 0; page < 1000; page++ {
			items, next, listErr := m.cfg.Tasks.ListTasks(ctx, task.TaskListQuery{OwnerID: owner, Cursor: cursor, Limit: 200})
			if listErr != nil {
				warnings = append(warnings, "tasks_unavailable")
				break
			}
			for _, item := range items {
				switch item.Status {
				case task.StatusQueued, task.StatusWaitingUser:
					increment(summary, "task_pending")
				case task.StatusRunning:
					increment(summary, "task_running")
				case task.StatusSucceeded:
					increment(summary, "task_completed")
				case task.StatusFailed, task.StatusCanceled:
					increment(summary, "task_failed")
				}
			}
			if next == "" {
				break
			}
			if next == cursor || page == 999 {
				warnings = append(warnings, "tasks_unavailable")
				break
			}
			cursor = next
		}
	}
	if m.cfg.Schedules != nil {
		cursor := ""
		for page := 0; page < 1000; page++ {
			result, listErr := m.cfg.Schedules.ListSchedules(ctx, owner, 100, cursor)
			if listErr != nil {
				warnings = append(warnings, "schedules_unavailable")
				break
			}
			for _, item := range result.Schedules {
				if item.Status == "disabled" || item.Status == "paused" {
					increment(summary, "schedule_paused")
				} else {
					increment(summary, "schedule_active")
				}
			}
			if result.NextCursor == "" {
				break
			}
			if result.NextCursor == cursor || page == 999 {
				warnings = append(warnings, "schedules_unavailable")
				break
			}
			cursor = result.NextCursor
		}
	}
	if m.cfg.Confirmations != nil {
		state := confirmation.StatePending
		cursor := ""
		for pageNumber := 0; pageNumber < 1000; pageNumber++ {
			page, listErr := m.cfg.Confirmations.List(ctx, confirmation.ListQuery{
				OwnerID: owner, State: &state, PageSize: 100, PageToken: cursor,
			})
			if listErr != nil {
				warnings = append(warnings, "confirmations_unavailable")
				break
			}
			summary["confirmation_pending"] = summary["confirmation_pending"].(int64) + int64(len(page.Confirmations))
			if page.NextPageToken == "" {
				break
			}
			if page.NextPageToken == cursor || pageNumber == 999 {
				warnings = append(warnings, "confirmation_count_truncated")
				break
			}
			cursor = page.NextPageToken
		}
	}
	return map[string]any{
		"summary":     summary,
		"deployments": recent,
		"observed_at": time.Now().UTC().Format(time.RFC3339Nano),
		"partial":     len(warnings) > 0,
		"warnings":    warnings,
	}, nil
}

func increment(summary map[string]any, key string) {
	value, _ := summary[key].(int64)
	summary[key] = value + 1
}

func addNumber(summary map[string]any, key string, raw any) {
	switch current := summary[key].(type) {
	case int64:
		switch value := raw.(type) {
		case int:
			summary[key] = current + int64(value)
		case int64:
			summary[key] = current + value
		case float64:
			summary[key] = current + int64(value)
		}
	case float64:
		switch value := raw.(type) {
		case int:
			summary[key] = current + float64(value)
		case int64:
			summary[key] = current + float64(value)
		case float64:
			summary[key] = current + value
		}
	}
}
