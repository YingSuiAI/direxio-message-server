package schedules

import (
	"context"
	"encoding/json"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
)

// OccurrenceRequest is the immutable handoff from the legacy schedule API to
// the unified task runtime.  Materialize must persist the occurrence, the
// generic task and the legacy ScheduleRun projection in one transaction.
type OccurrenceRequest struct {
	Schedule     storage.Schedule
	OccurrenceID string
	TaskID       string
	RunID        string
	TriggerKey   string
	ScheduledFor time.Time
}

type OccurrenceMaterializer interface {
	MaterializeOccurrence(context.Context, OccurrenceRequest) (storage.ScheduleRun, error)
}

func occurrenceRequest(s storage.Schedule, scheduledFor time.Time, runID, triggerKey string) (OccurrenceRequest, error) {
	scheduledFor = scheduledFor.UTC()
	if s.ScheduleID == "" || scheduledFor.IsZero() {
		return OccurrenceRequest{}, task.ErrInvalid
	}
	occurrenceID, taskID := ScheduledOccurrenceIDs(s.OwnerID, s.ScheduleID, scheduledFor)
	if runID == "" {
		runID = occurrenceID
	}
	return OccurrenceRequest{Schedule: s, OccurrenceID: occurrenceID, TaskID: taskID, RunID: runID, TriggerKey: triggerKey, ScheduledFor: scheduledFor}, nil
}

// MaterializeTaskSpec is exported for adapters so they cannot accidentally
// execute the legacy prompt or resolve a mutable profile at run time.
func MaterializeTaskSpec(s storage.Schedule, req OccurrenceRequest) (task.TaskSpec, error) {
	if req.Schedule.ScheduleID != "" && req.Schedule.ScheduleID != s.ScheduleID {
		return task.TaskSpec{}, task.ErrConflict
	}
	tpl := task.TaskTemplate{Kind: task.TaskKindAgent, Goal: s.Prompt, ModelProfileID: s.ModelProfileID}
	if len(s.TaskTemplate) > 0 {
		if err := json.Unmarshal(s.TaskTemplate, &tpl); err != nil {
			return task.TaskSpec{}, task.ErrInvalid
		}
	}
	return tpl.Materialize(taskIDempotency(req), req.ScheduledFor.UTC())
}

func taskIDempotency(req OccurrenceRequest) string {
	return uuid.NewSHA1(uuid.Nil, []byte(req.OccurrenceID+"\x00idempotency")).String()
}
