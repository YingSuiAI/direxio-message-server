package schedules

import (
	"encoding/json"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
	"time"
)

// CoreScheduleProjection is the generic scheduler view. It deliberately
// projects, rather than replaces, the existing agent.schedules DTO.
type CoreScheduleProjection struct {
	ID               string          `json:"id"`
	OwnerID          string          `json:"owner_id"`
	TaskTemplate     json.RawMessage `json:"task_template"`
	Trigger          json.RawMessage `json:"trigger"`
	Status           string          `json:"status"`
	Revision         int64           `json:"revision"`
	NextRunAt        *time.Time      `json:"next_run_at,omitempty"`
	LastScheduledFor *time.Time      `json:"last_scheduled_for,omitempty"`
	Deleted          bool            `json:"deleted"`
}

func ProjectCoreSchedule(s storage.Schedule) CoreScheduleProjection {
	status := s.CoreState
	if status == "" {
		status = s.Status
	}
	if s.Status == "enabled" {
		status = "active"
	}
	if s.Status == "disabled" {
		status = "paused"
	}
	trigger := append(json.RawMessage(nil), s.TriggerJSON...)
	if len(trigger) == 0 {
		kind := s.TriggerKind
		if kind == "one_time" {
			kind = "run_at"
		}
		if kind == "run_at" {
			trigger, _ = json.Marshal(map[string]any{"kind": kind, "run_at": s.TriggerValue})
		} else if kind == "cron" {
			trigger, _ = json.Marshal(map[string]any{"kind": kind, "expression": s.TriggerValue, "timezone": s.Timezone})
		}
	}
	return CoreScheduleProjection{ID: s.ScheduleID, OwnerID: s.OwnerID, TaskTemplate: append(json.RawMessage(nil), s.TaskTemplate...), Trigger: trigger, Status: status, Revision: s.Revision, NextRunAt: s.NextRunAt, LastScheduledFor: s.LatestRunAt, Deleted: s.Status == "deleted"}
}

// ScheduledOccurrenceIDs are stable across retries and workers, but remain
// owner-local: different owners are allowed to use the same schedule UUID.
func ScheduledOccurrenceIDs(ownerID, scheduleID string, scheduledFor time.Time) (occurrenceID, taskID string) {
	occurrenceID = uuid.NewSHA1(uuid.Nil, []byte(ownerID+"\x00"+scheduleID+"\x00scheduled\x00"+scheduledFor.UTC().Format(time.RFC3339Nano))).String()
	taskID = uuid.NewSHA1(uuid.Nil, []byte(occurrenceID+"\x00task")).String()
	return
}
