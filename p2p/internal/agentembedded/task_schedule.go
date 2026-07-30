package agentembedded

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	actionbase "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/action"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

func (m *Module) taskHandler(action string) actionbase.Handler {
	return func(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
		if _, e := m.requireCapability(ctx, p, "task", m.cfg.Tasks != nil); e != nil {
			return nil, e
		}
		if m == nil || m.cfg.Tasks == nil {
			return unavailable(ctx, p)
		}
		o := m.owner()
		switch action[strings.LastIndex(action, ".")+1:] {
		case "get":
			id, e := requiredString(p, "task_id")
			if e != nil {
				return nil, e
			}
			v, err := m.cfg.Tasks.GetTask(ctx, id)
			if err != nil {
				return nil, taskError(err)
			}
			if strings.TrimSpace(v.OwnerID) != o {
				return nil, taskError(task.ErrNotFound)
			}
			return map[string]any{"task": taskMap(v)}, nil
		case "list":
			size, _, e := page(p)
			if e != nil {
				return nil, e
			}
			var status *task.Status
			if raw, ok := p["status"]; ok && raw != nil {
				s, ok := raw.(string)
				if !ok {
					return nil, actionbase.BadRequest("status must be a string")
				}
				v := task.Status(strings.TrimSpace(s))
				status = &v
			}
			items, next, err := m.cfg.Tasks.ListTasks(ctx, task.TaskListQuery{OwnerID: o, Status: status, Limit: size})
			if err != nil {
				return nil, taskError(err)
			}
			out := make([]any, 0, len(items))
			for _, v := range items {
				out = append(out, taskMap(v))
			}
			return map[string]any{"tasks": out, "next_page_token": next}, nil
		case "events":
			id, e := requiredString(p, "task_id")
			if e != nil {
				return nil, e
			}
			v, err := m.cfg.Tasks.GetTask(ctx, id)
			if err != nil || strings.TrimSpace(v.OwnerID) != o {
				if err == nil {
					err = task.ErrNotFound
				}
				return nil, taskError(err)
			}
			after, e := optionalInt64(p, "after_sequence")
			if e != nil {
				return nil, e
			}
			limit, e := optionalInt64(p, "limit")
			if e != nil {
				return nil, e
			}
			if limit <= 0 {
				limit = 128
			}
			if limit > 256 {
				return nil, actionbase.BadRequest("limit is too large")
			}
			items, _, err := m.cfg.Tasks.ListProgress(ctx, id, uint64(after), int(limit))
			if err != nil {
				return nil, taskError(err)
			}
			out := make([]any, 0, len(items))
			for _, v := range items {
				out = append(out, eventMap(v))
			}
			return map[string]any{"events": out}, nil
		case "retry":
			id, e := requiredString(p, "task_id")
			if e != nil {
				return nil, e
			}
			key, e := requiredString(p, "idempotency_key")
			if e != nil {
				return nil, e
			}
			rev, e := optionalInt64(p, "expected_revision")
			if e != nil {
				return nil, e
			}
			v, err := m.cfg.Tasks.GetTask(ctx, id)
			if err != nil || strings.TrimSpace(v.OwnerID) != o {
				if err == nil {
					err = task.ErrNotFound
				}
				return nil, taskError(err)
			}
			v, err = m.cfg.Tasks.RetryTask(ctx, task.RetryCommand{TaskID: id, Mutation: task.MutationCommand{IdempotencyKey: key, RequestDigest: task.Digest(p), ExpectedRevision: uint64(rev)}, At: time.Now().UTC()})
			if err != nil {
				return nil, taskError(err)
			}
			return map[string]any{"task": taskMap(v)}, nil
		case "cancel":
			id, e := requiredString(p, "task_id")
			if e != nil {
				return nil, e
			}
			key, e := requiredString(p, "idempotency_key")
			if e != nil {
				return nil, e
			}
			rev, e := optionalInt64(p, "expected_revision")
			if e != nil {
				return nil, e
			}
			reason, e := optionalString(p, "reason")
			if e != nil {
				return nil, e
			}
			v, err := m.cfg.Tasks.CancelTask(ctx, task.CancelCommand{TaskID: id, OwnerID: o, ExpectedRevision: uint64(rev), Reason: reason, At: time.Now().UTC(), Mutation: task.MutationCommand{IdempotencyKey: key, RequestDigest: task.Digest(p), ExpectedRevision: uint64(rev)}})
			if err != nil {
				return nil, taskError(err)
			}
			return map[string]any{"task": taskMap(v)}, nil
		default:
			return unavailable(ctx, p)
		}
	}
}

func taskMap(v task.Task) map[string]any {
	// The action contract declares both reference fields as arrays. Start from
	// non-nil empty slices so a task created without references serializes as
	// [] rather than null, which strict clients reject before dispatch.
	attachmentRefs := append([]string{}, v.Spec.AttachmentRefs...)
	knowledgeRefs := append([]string{}, v.Spec.KnowledgeRefs...)
	out := map[string]any{"task_id": v.ID, "goal": v.Spec.Goal, "conversation_id": v.Spec.ConversationID, "model_profile_id": v.Spec.ModelProfileID, "attachment_refs": attachmentRefs, "knowledge_refs": knowledgeRefs, "timeout_seconds": v.Spec.TimeoutSeconds, "status": string(v.Status), "attempt": v.Attempt, "lease_epoch": v.LeaseEpoch, "available_at": v.AvailableAt.UTC().Format(time.RFC3339Nano), "retry_of_task_id": v.RetryOfTaskID, "failure_code": v.FailureCode, "failure_summary": v.FailureSummary, "revision": v.Revision, "kind": string(v.Spec.Kind), "created_at": v.CreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": v.UpdatedAt.UTC().Format(time.RFC3339Nano)}
	if v.Spec.Payload.Workload != nil {
		out["expected_workload_revision"] = v.Spec.Payload.Workload.ExpectedWorkloadRevision
	}
	return out
}
func eventMap(v task.Progress) map[string]any {
	return map[string]any{"task_id": v.TaskID, "sequence": v.Sequence, "event_id": v.EventID, "attempt": v.Attempt, "status": string(v.Status), "phase": v.Phase, "progress_message": v.Message, "occurred_at": v.At.UTC().Format(time.RFC3339Nano), "error_code": v.ErrorCode, "error_summary": v.ErrorSummary}
}
func taskError(err error) *actionbase.Error {
	if errors.Is(err, task.ErrNotFound) {
		return actionbase.CodedError(http.StatusNotFound, "task_not_found", "task was not found")
	}
	if errors.Is(err, task.ErrRevisionConflict) || errors.Is(err, task.ErrConflict) {
		return actionbase.CodedError(http.StatusConflict, "task_conflict", "task revision conflict")
	}
	return actionbase.InternalError(err)
}

func (m *Module) scheduleHandler(action string) actionbase.Handler {
	return func(ctx context.Context, p map[string]any) (any, *actionbase.Error) {
		if _, e := m.requireCapability(ctx, p, "schedules.server", m.cfg.Schedules != nil && m.cfg.ScheduleTrigger != nil); e != nil {
			return nil, e
		}
		if m == nil || m.cfg.Schedules == nil {
			return unavailable(ctx, p)
		}
		o := m.owner()
		op := action[strings.LastIndex(action, ".")+1:]
		switch op {
		case "create":
			name, e := requiredString(p, "name")
			if e != nil {
				return nil, e
			}
			key, e := requiredString(p, "idempotency_key")
			if e != nil {
				return nil, e
			}
			v, e := scheduleFromParams(o, p, "")
			if e != nil {
				return nil, e
			}
			v.Name = name
			created, err := m.cfg.Schedules.CreateSchedule(ctx, v, key)
			if err != nil {
				return nil, scheduleError(err)
			}
			return map[string]any{"schedule": scheduleMap(created)}, nil
		case "get":
			id, e := requiredString(p, "schedule_id")
			if e != nil {
				return nil, e
			}
			v, ok, err := m.cfg.Schedules.GetSchedule(ctx, o, id)
			if err != nil {
				return nil, scheduleError(err)
			}
			if !ok {
				return nil, actionbase.CodedError(http.StatusNotFound, "schedule_not_found", "schedule was not found")
			}
			return map[string]any{"schedule": scheduleMap(v)}, nil
		case "list":
			size, token, e := page(p)
			if e != nil {
				return nil, e
			}
			out, err := m.cfg.Schedules.ListSchedules(ctx, o, size, token)
			if err != nil {
				return nil, scheduleError(err)
			}
			items := make([]any, 0, len(out.Schedules))
			for _, v := range out.Schedules {
				items = append(items, scheduleMap(v))
			}
			return map[string]any{"schedules": items, "next_page_token": out.NextCursor}, nil
		case "delete":
			id, e := requiredString(p, "schedule_id")
			if e != nil {
				return nil, e
			}
			key, e := requiredString(p, "idempotency_key")
			if e != nil {
				return nil, e
			}
			if err := m.cfg.Schedules.DeleteSchedule(ctx, o, id, key); err != nil {
				return nil, scheduleError(err)
			}
			return map[string]any{"deleted": true, "schedule_id": id}, nil
		case "update":
			id, e := requiredString(p, "schedule_id")
			if e != nil {
				return nil, e
			}
			key, e := requiredString(p, "idempotency_key")
			if e != nil {
				return nil, e
			}
			old, ok, err := m.cfg.Schedules.GetSchedule(ctx, o, id)
			if err != nil {
				return nil, scheduleError(err)
			}
			if !ok {
				return nil, actionbase.CodedError(http.StatusNotFound, "schedule_not_found", "schedule was not found")
			}
			v, e := scheduleFromParams(o, p, id)
			if e != nil {
				return nil, e
			}
			v.Revision = old.Revision
			if name, present := p["name"]; present {
				s, ok := name.(string)
				if !ok {
					return nil, actionbase.BadRequest("name must be a string")
				}
				v.Name = s
			} else {
				v.Name = old.Name
			}
			if _, present := p["task_template"]; !present {
				v.Prompt, v.TaskTemplate, v.ModelProfileID = old.Prompt, old.TaskTemplate, old.ModelProfileID
			}
			v.CoreState = old.CoreState
			if _, present := p["trigger"]; !present {
				v.TriggerKind, v.TriggerValue, v.Timezone, v.TriggerJSON = old.TriggerKind, old.TriggerValue, old.Timezone, old.TriggerJSON
			}
			updated, err := m.cfg.Schedules.UpdateSchedule(ctx, v, key)
			if err != nil {
				return nil, scheduleError(err)
			}
			return map[string]any{"schedule": scheduleMap(updated)}, nil
		case "trigger":
			id, e := requiredString(p, "schedule_id")
			if e != nil {
				return nil, e
			}
			key, e := requiredString(p, "idempotency_key")
			if e != nil {
				return nil, e
			}
			if m.cfg.ScheduleTrigger == nil {
				return unavailable(ctx, p)
			}
			v, occurrenceID, taskID, err := m.cfg.ScheduleTrigger.TriggerSchedule(ctx, o, id, key)
			if err != nil {
				return nil, scheduleError(err)
			}
			return map[string]any{"schedule": scheduleMap(v), "occurrence_id": occurrenceID, "task_id": taskID}, nil
		case "pause", "resume", "enable", "disable":
			id, e := requiredString(p, "schedule_id")
			if e != nil {
				return nil, e
			}
			key, e := requiredString(p, "idempotency_key")
			if e != nil {
				return nil, e
			}
			status := "enabled"
			if op == "pause" || op == "disable" {
				status = "disabled"
			}
			v, err := m.cfg.Schedules.SetScheduleStatus(ctx, o, id, key, status == "enabled")
			if err != nil {
				return nil, scheduleError(err)
			}
			return map[string]any{"schedule": scheduleMap(v)}, nil
		default:
			return unavailable(ctx, p)
		}
	}
}
func scheduleMap(v storage.Schedule) map[string]any {
	state := v.CoreState
	if state == "" {
		state = v.Status
	}
	if state == "enabled" {
		state = "active"
	}
	if state == "disabled" {
		state = "paused"
	}
	trigger := append(json.RawMessage(nil), v.TriggerJSON...)
	if len(trigger) == 0 {
		kind := v.TriggerKind
		if kind == "one_time" {
			kind = "run_at"
		}
		if kind == "run_at" {
			trigger, _ = json.Marshal(map[string]any{"kind": kind, "run_at": v.TriggerValue})
		} else if kind == "cron" {
			trigger, _ = json.Marshal(map[string]any{"kind": kind, "expression": v.TriggerValue, "timezone": v.Timezone})
		}
	}
	template := append(json.RawMessage(nil), v.TaskTemplate...)
	if len(template) == 0 {
		template, _ = json.Marshal(map[string]any{"goal": v.Prompt, "model_profile_id": v.ModelProfileID})
	}
	return map[string]any{"schedule_id": v.ScheduleID, "name": v.Name, "state": state, "status": v.Status, "next_run_at": timeString(v.NextRunAt), "last_scheduled_for": timeString(v.LatestRunAt), "revision": v.Revision, "created_at": v.CreatedAt.UTC().Format(time.RFC3339Nano), "updated_at": v.UpdatedAt.UTC().Format(time.RFC3339Nano), "task_template": json.RawMessage(template), "trigger": json.RawMessage(trigger)}
}

func scheduleFromParams(owner string, p map[string]any, id string) (storage.Schedule, *actionbase.Error) {
	v := storage.Schedule{OwnerID: owner, ScheduleID: id, Status: "enabled", CoreState: "active"}
	if raw, ok := p["task_template"]; ok && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return v, actionbase.BadRequest("task_template must be an object")
		}
		goal, e := requiredString(m, "goal")
		if e != nil {
			return v, e
		}
		v.Prompt = goal
		v.TaskTemplate, _ = json.Marshal(m)
		v.ModelProfileID, e = optionalString(m, "model_profile_id")
		if e != nil {
			return v, e
		}
	}
	if raw, ok := p["trigger"]; ok && raw != nil {
		m, ok := raw.(map[string]any)
		if !ok {
			return v, actionbase.BadRequest("trigger must be an object")
		}
		kind, e := requiredString(m, "kind")
		if e != nil {
			return v, e
		}
		kind = strings.ReplaceAll(strings.ToLower(kind), "-", "_")
		v.TriggerKind = kind
		if kind == "run_at" {
			v.TriggerValue, e = requiredString(m, "run_at")
			if e != nil {
				return v, e
			}
		} else if kind == "cron" {
			v.TriggerValue, e = requiredString(m, "expression")
			if e != nil {
				return v, e
			}
			v.Timezone, e = requiredString(m, "timezone")
			if e != nil {
				return v, e
			}
		} else {
			return v, actionbase.BadRequest("trigger kind is invalid")
		}
		v.TriggerJSON, _ = json.Marshal(m)
	}
	return v, nil
}
func timeString(v *time.Time) string {
	if v == nil {
		return ""
	}
	return v.UTC().Format(time.RFC3339Nano)
}
func scheduleError(err error) *actionbase.Error {
	if errors.Is(err, storage.ErrScheduleNotFound) {
		return actionbase.CodedError(http.StatusNotFound, "schedule_not_found", "schedule was not found")
	}
	if errors.Is(err, storage.ErrScheduleConflict) || errors.Is(err, storage.ErrScheduleIdempotency) {
		return actionbase.CodedError(http.StatusConflict, "schedule_conflict", "schedule conflict")
	}
	return actionbase.InternalError(err)
}
