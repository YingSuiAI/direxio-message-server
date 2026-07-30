package p2p

import (
	"context"
	"fmt"
	"time"

	schedulesmodule "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agent/schedules"
	agenttask "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	agentcoreledger "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcore"
	agentembedded "github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentembedded"
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

// embeddedDeploymentSource is the retained durable ledger contract. It
// contains no transport methods; the adapter exists only because the legacy
// ledger page-options type lives beside the projection helpers.
type embeddedDeploymentSource interface {
	ListDeployments(context.Context, string, agentcoreledger.DeploymentListOptions) ([]map[string]any, string, error)
	GetDeploymentByID(context.Context, string, string) (map[string]any, bool, error)
	GetDeploymentByWorkloadID(context.Context, string, string) (map[string]any, bool, error)
	ListDeploymentEventsByID(context.Context, string, string, int64, int) ([]map[string]any, int64, error)
	ListDeploymentEventsByWorkloadID(context.Context, string, string, int64, int) ([]map[string]any, int64, error)
}

type embeddedDeploymentAdapter struct{ source embeddedDeploymentSource }

func (a embeddedDeploymentAdapter) ListDeployments(ctx context.Context, owner string, opts agentembedded.DeploymentListOptions) ([]map[string]any, string, error) {
	return a.source.ListDeployments(ctx, owner, agentcoreledger.DeploymentListOptions{
		PageSize:   opts.PageSize,
		PageToken:  opts.PageToken,
		Status:     opts.Status,
		TargetKind: opts.TargetKind,
	})
}

func (a embeddedDeploymentAdapter) GetDeploymentByID(ctx context.Context, owner, deploymentID string) (map[string]any, bool, error) {
	return a.source.GetDeploymentByID(ctx, owner, deploymentID)
}

func (a embeddedDeploymentAdapter) GetDeploymentByWorkloadID(ctx context.Context, owner, workloadID string) (map[string]any, bool, error) {
	return a.source.GetDeploymentByWorkloadID(ctx, owner, workloadID)
}

func (a embeddedDeploymentAdapter) ListDeploymentEventsByID(ctx context.Context, owner, deploymentID string, after int64, limit int) ([]map[string]any, int64, error) {
	return a.source.ListDeploymentEventsByID(ctx, owner, deploymentID, after, limit)
}

func (a embeddedDeploymentAdapter) ListDeploymentEventsByWorkloadID(ctx context.Context, owner, workloadID string, after int64, limit int) ([]map[string]any, int64, error) {
	return a.source.ListDeploymentEventsByWorkloadID(ctx, owner, workloadID, after, limit)
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
