package p2p

import (
	"context"
	"fmt"
	"time"

	schedulesmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent/schedules"
	agenttask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	p2pstorage "github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

// embeddedScheduleMaterializer is the compatibility projection for
// agent.schedules.run_now. The durable store owns the transaction that
// creates the occurrence, legacy run and generic task.
type embeddedScheduleMaterializer struct{ store *p2pstorage.DatabaseStore }

func (a embeddedScheduleMaterializer) MaterializeOccurrence(ctx context.Context, request schedulesmodule.OccurrenceRequest) (p2pstorage.ScheduleRun, error) {
	if a.store == nil {
		return p2pstorage.ScheduleRun{}, agenttask.ErrInvalid
	}
	var occurrenceID, taskID string
	var err error
	if request.TriggerKey != "" {
		_, occurrenceID, taskID, err = a.store.TriggerSchedule(ctx, request.Schedule.OwnerID, request.Schedule.ScheduleID, request.TriggerKey)
	} else {
		occurrenceID, taskID, err = a.store.MaterializeScheduleTask(ctx, request.Schedule.OwnerID, request.Schedule.ScheduleID, request.ScheduledFor)
	}
	if err != nil {
		return p2pstorage.ScheduleRun{}, err
	}
	if occurrenceID != request.OccurrenceID || taskID != request.TaskID {
		return p2pstorage.ScheduleRun{}, fmt.Errorf("schedule occurrence identity mismatch")
	}
	run, ok, err := a.store.GetScheduleRun(ctx, request.Schedule.OwnerID, request.Schedule.ScheduleID, request.RunID)
	if err != nil {
		return p2pstorage.ScheduleRun{}, err
	}
	if !ok {
		return p2pstorage.ScheduleRun{}, p2pstorage.ErrScheduleNotFound
	}
	return run, nil
}

type embeddedTaskRetryAdapter struct{ store *p2pstorage.DatabaseTaskStore }

func (a embeddedTaskRetryAdapter) RetryTask(ctx context.Context, owner, taskID, idempotencyKey string, expectedRevision uint64) (agenttask.Task, error) {
	if a.store == nil {
		return agenttask.Task{}, agenttask.ErrInvalid
	}
	current, err := a.store.GetTask(ctx, taskID)
	if err != nil {
		return agenttask.Task{}, err
	}
	if owner != "" && current.OwnerID != owner {
		return agenttask.Task{}, agenttask.ErrNotFound
	}
	return a.store.RetryTask(ctx, agenttask.RetryCommand{
		TaskID: taskID,
		Mutation: agenttask.MutationCommand{
			IdempotencyKey:   idempotencyKey,
			RequestDigest:    agenttask.Digest(map[string]any{"task_id": taskID, "idempotency_key": idempotencyKey, "expected_revision": expectedRevision}),
			ExpectedRevision: expectedRevision,
		},
		At: time.Now().UTC(),
	})
}
