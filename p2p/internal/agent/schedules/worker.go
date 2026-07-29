package schedules

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/runtime"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/internal/agentcontrol/task"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
)

// Worker only materializes due schedule occurrences.  Generic task execution
// is owned by agentcontrol/runtime; this package never invokes NativeAgent.
type Worker struct {
	Store        storage.ScheduleStore
	Materializer OccurrenceMaterializer
	OwnerID      string
	WorkerID     string
	PollInterval time.Duration
	Lease        time.Duration
}

func (w Worker) Ready() bool { return w.Store != nil && w.Materializer != nil }

func (w Worker) Run(ctx context.Context) {
	if !w.Ready() || ctx == nil {
		return
	}
	interval := w.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	for {
		if err := w.tick(ctx, time.Now().UTC()); err != nil {
			return
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

func (w Worker) tick(ctx context.Context, now time.Time) error {
	holder := strings.TrimSpace(w.WorkerID)
	if holder == "" {
		holder = "embedded-scheduler"
	}
	lease := w.Lease
	if lease <= 0 {
		lease = 30 * time.Second
	}
	items, err := w.Store.ClaimDueSchedules(ctx, now.UTC(), holder, lease, 25)
	if err != nil {
		return err
	}
	for _, schedule := range items {
		if schedule.NextRunAt == nil || schedule.Status != "running" {
			continue
		}
		scheduledFor := schedule.NextRunAt.UTC()
		occurrenceID, _ := ScheduledOccurrenceIDs(schedule.OwnerID, schedule.ScheduleID, scheduledFor)
		req := OccurrenceRequest{Schedule: schedule, OccurrenceID: occurrenceID, RunID: occurrenceID, TaskID: taskID(schedule.OwnerID, schedule.ScheduleID, scheduledFor), ScheduledFor: scheduledFor}
		if _, err := w.Materializer.MaterializeOccurrence(ctx, req); err != nil && !errors.Is(err, storage.ErrScheduleConflict) && !errors.Is(err, storage.ErrScheduleClaimed) {
			return err
		}
	}
	return nil
}

func taskID(ownerID, scheduleID string, at time.Time) string {
	_, id := ScheduledOccurrenceIDs(strings.TrimSpace(ownerID), strings.TrimSpace(scheduleID), at.UTC())
	return id
}

// RuntimeScheduleLoop adapts a transactional materializer to the generic
// runtime scheduler. It is useful to callers that do not need legacy polling.
type RuntimeScheduleLoop interface {
	Run(context.Context) error
	Wait(context.Context) error
}

func NewRuntimeScheduleLoop(materializer runtime.ScheduleMaterializer, interval time.Duration) (RuntimeScheduleLoop, error) {
	return runtime.NewScheduleLoop(materializer, runtime.CronCalculator{}, interval)
}

var _ task.CronCalculator = runtime.CronCalculator{}
