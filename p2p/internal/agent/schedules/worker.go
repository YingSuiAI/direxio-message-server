package schedules

import (
	"context"
	"strings"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p/nativeagent"
	"github.com/YingSuiAI/dirextalk-message-server/p2p/storage"
	"github.com/google/uuid"
)

// Worker is deliberately opt-in: callers start it only after all readiness
// dependencies are present. A failed run is recorded once; no retry queue is
// created here.
type Worker struct {
	Store        storage.ScheduleStore
	Profiles     storage.ModelProfileStore
	Runner       ScheduledRunner
	OwnerID      string
	WorkerID     string
	PollInterval time.Duration
	Lease        time.Duration
}

func (w Worker) Ready() bool {
	return w.Store != nil && w.Profiles != nil && w.Profiles.ModelProfileStoreReady() && w.Runner != nil
}

func (w Worker) Run(ctx context.Context) {
	if !w.Ready() {
		return
	}
	interval := w.PollInterval
	if interval <= 0 {
		interval = time.Second
	}
	lease := w.Lease
	if lease <= 0 {
		lease = 30 * time.Second
	}
	id := strings.TrimSpace(w.WorkerID)
	if id == "" {
		id = "embedded-scheduler"
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		if err := w.tick(ctx, id, lease); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (w Worker) tick(ctx context.Context, id string, lease time.Duration) error {
	now := time.Now().UTC()
	overlaps, err := w.Store.ListOverlappingSchedules(ctx, now, 25)
	if err != nil {
		return err
	}
	for _, s := range overlaps {
		var next *time.Time
		if s.TriggerKind == "cron" {
			var e error
			next, e = nextCron(s.TriggerValue, s.Timezone, now)
			if e != nil {
				return e
			}
		}
		if err := w.Store.CoalesceSchedule(ctx, s.OwnerID, s.ScheduleID, s.LeaseOwner, s.LeaseEpoch, next); err != nil && err != storage.ErrScheduleClaimed {
			return err
		}
	}
	items, err := w.Store.ClaimDueSchedules(ctx, now, id, lease, 25)
	if err != nil {
		return err
	}
	for _, s := range items {
		if err := w.execute(ctx, s, lease); err != nil {
			return err
		}
	}
	return nil
}
func (w Worker) execute(ctx context.Context, s storage.Schedule, lease time.Duration) error {
	scheduledFor := time.Now().UTC()
	if s.NextRunAt != nil {
		scheduledFor = *s.NextRunAt
	}
	r := storage.ScheduleRun{RunID: uuid.NewString(), ScheduleID: s.ScheduleID, OwnerID: s.OwnerID, Status: "running", ScheduledFor: scheduledFor, LeaseEpoch: s.LeaseEpoch}
	_, created, err := w.Store.CreateScheduleRun(ctx, r, s.LeaseOwner, s.Revision, s.LeaseEpoch)
	if err != nil {
		return err
	}
	if !created {
		if _, err = w.Store.RecoverScheduleRun(ctx, s.OwnerID, s.ScheduleID, s.LeaseOwner, s.LeaseEpoch, scheduledFor); err != nil {
			return err
		}
		if s.TriggerKind == "one_time" {
			return w.Store.AdvanceSchedule(ctx, s.OwnerID, s.ScheduleID, s.LeaseOwner, s.Revision, s.LeaseEpoch, nil, "disabled")
		}
		next, err := nextCron(s.TriggerValue, s.Timezone, time.Now().UTC())
		if err != nil {
			return err
		}
		return w.Store.AdvanceSchedule(ctx, s.OwnerID, s.ScheduleID, s.LeaseOwner, s.Revision, s.LeaseEpoch, next, "enabled")
	}
	profile, err := w.Profiles.ResolveModelProfilePinned(ctx, s.OwnerID, s.ModelProfileID, s.ModelProfileRevision, s.CredentialVersion)
	result := ""
	runErr := ""
	if err == nil {
		runCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		type execution struct {
			result string
			err    error
		}
		done := make(chan execution, 1)
		go func() {
			text, e := w.Runner.ExecuteScheduled(runCtx, s.Prompt, profile, nativeagent.EmbeddedAllowedTools())
			done <- execution{text, e}
		}()
		interval := lease / 3
		if interval <= 0 {
			interval = time.Second
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case completed := <-done:
				result, err = completed.result, completed.err
				goto executed
			case <-ticker.C:
				if renewErr := w.Store.RenewScheduleLease(ctx, s.OwnerID, s.ScheduleID, s.LeaseOwner, s.LeaseEpoch, lease); renewErr != nil {
					cancel()
					return renewErr
				}
			case <-ctx.Done():
				cancel()
				return ctx.Err()
			}
		}
	}
executed:
	if err != nil {
		runErr = nativeagent.SanitizeScheduledText(err.Error(), profile.APIKey)
	} else {
		result = nativeagent.SanitizeScheduledText(result, profile.APIKey)
	}
	if err := w.Store.FinishScheduleRun(ctx, s.OwnerID, r.RunID, s.LeaseOwner, s.LeaseEpoch, result, runErr, time.Now().UTC()); err != nil {
		return err
	}
	if s.TriggerKind == "one_time" {
		return w.Store.AdvanceSchedule(ctx, s.OwnerID, s.ScheduleID, s.LeaseOwner, s.Revision, s.LeaseEpoch, nil, "disabled")
	} else if next, e := nextCron(s.TriggerValue, s.Timezone, time.Now().UTC()); e == nil {
		return w.Store.AdvanceSchedule(ctx, s.OwnerID, s.ScheduleID, s.LeaseOwner, s.Revision, s.LeaseEpoch, next, "enabled")
	} else {
		return e
	}
	return nil
}
